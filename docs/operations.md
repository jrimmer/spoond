# Operations

Runbook for operating a spoond deployment on the E2B substrate: health,
drain, crash recovery, backups, the catalog GC and restart procedures.

The substrate is E2B's orchestrator (our fork of `e2b-dev/runtime`),
running as `e2b-orchestrator.service` and driving Firecracker microVMs.
spoond is the control plane: `spoond-backend` (lease API), the SSH
gateway, the runner, and the SQLite store under `SPOOND_DB_PATH`.

## Component health

| Check | Command |
|---|---|
| Orchestrator | `systemctl is-active e2b-orchestrator` and `curl -s http://127.0.0.1:5008/health` |
| Backend | `systemctl is-active spoond-backend` |
| Gateway | `systemctl is-active spoond-sshd-gateway` |
| Runner | `systemctl is-active spoond-runner` |
| Lease API | `curl -s https://127.0.0.1:8890/healthz` |
| Everything | `spoond doctor` (exit 0 = all pass; `--json` for machines) |
| Metrics | `curl -s -H "Authorization: Bearer $METRICS_TOKEN" https://127.0.0.1:8890/metrics` |
| Identity store | `test -f /var/lib/spoond/users.json && stat -c '%a' /var/lib/spoond/users.json` (expect `600`) |

`spoond doctor` exercises every external surface the backend depends on:
the orchestrator (`/health`, `NodeInfo`), the image registry, the lease
API listener, the gateway port, the token seed, the SQLite catalog with
its baked images, the pinned Firecracker/kernel/envd artifacts, the build
storage, TLS material, the LLM upstream and disk. It reads the same
environment the backend uses, so its output is a true reflection of the
deployed config.

## Lease states

Every lease carries a `state`: `running`, `suspended`, `recovered` or
`lost`, reported by `GET /api/sandboxes/{id}` and the `ls` ctl verb.

- **`suspended`** — paused into a build; `resume` restores it (with its
  memory) from `resume_build_id`.
- **`recovered`** — the sandbox died in an orchestrator crash and was
  resumed from its last checkpoint (`recovered_from` is the checkpoint
  time). Running state goes back only as far as that checkpoint.
- **`lost`** — no checkpoint existed to recover from, or recovery failed.
  The lease answers `410` with `sandbox lost in a substrate crash; delete
  this lease`; delete it and start again.

Watch `spoond_leases{state="lost"}`: it must be `0` unless an orchestrator
crash happened — if it moves without one, that is a bug, not wear.

## The drain protocol (planned orchestrator restarts)

A **planned** restart is lossless: drain first, so every running sandbox
is snapshotted to a build and stopped, then nothing is lost.

```bash
# 1. Stop the runner so no job starts mid-drain.
systemctl stop spoond-runner

# 2. Drain: pause every live lease (4 at a time), delete the warm pool
#    and wait until the node is quiesced.
curl -s -X POST -H "Authorization: Bearer $ADMIN_TOKEN" \
  https://127.0.0.1:8890/api/admin/drain
#   {"paused":N,"failed":[],"pool_deleted":M,"quiesced":true}
#   "failed" must be empty; a 503 means the orchestrator was
#   unreachable and nothing changed.

# 3. Restart the orchestrator (or the whole host).
systemctl restart e2b-orchestrator

# 4. Undrain: resume every drained lease from its pause build.
curl -s -X POST -H "Authorization: Bearer $ADMIN_TOKEN" \
  https://127.0.0.1:8890/api/admin/undrain
#   {"resumed":N,"failed":[]}

systemctl start spoond-runner
```

A lease in `failed` did not pause or resume; the drain/undrain continues
past it and reports the lease id with the error. Resolve those
individually (`stat`, then `resume` or delete).

## Crash recovery (unplanned)

An orchestrator crash or a host power loss kills every running sandbox.
This is the accepted trade-off (D4): running state returns to each
sandbox's last checkpoint, not to the instant of the crash.

Reconciliation is automatic. It runs once at backend start, every 30 s in
the background, and immediately when the orchestrator's `NodeInfo` goes
from failing to succeeding:

- a lease with a checkpoint is resumed from that build, with the **same**
  sandbox id, and marked `recovered`;
- a lease without one is marked `lost`;
- substrate sandboxes no lease or pool entry claims are deleted;
- pool entries whose sandbox is gone are dropped.

To run it on demand (e.g. right after bringing the orchestrator back):

```bash
curl -s -X POST -H "Authorization: Bearer $ADMIN_TOKEN" \
  https://127.0.0.1:8890/api/admin/reconcile
#   {"recovered":N,"lost":M}
```

Persistent leases are checkpointed every `CHECKPOINT_INTERVAL_MINS`
(default 60; `0` disables the loop), so that is the worst-case window of
lost work. Lower it if a workload's re-run cost is higher than the
checkpoint cost.

## Restarting the orchestrator

See §The drain protocol above for a **planned** restart; never restart
`e2b-orchestrator` without draining first unless it is already down.

## Restarting the backend

`spoond-backend` may be restarted freely: leases, shares, the pool and the
image catalog live in SQLite and are reloaded on start, then
reconciliation aligns the substrate with the stored state.

```bash
systemctl restart spoond-backend
journalctl -u spoond-backend --since "-2 min"    # look for recovery: lines
curl -s https://127.0.0.1:8890/healthz
```

The runner keeps running; its next lease request either rides through the
restart or retries. The SSH gateway does the same (it has no `Requires=`
on the backend for exactly this reason).

## Backups

`SPOOND_BACKUP_DIR` (default `/var/lib/spoond/backups`) receives a
`VACUUM INTO` snapshot daily at 03:00. The backup is a consistent SQLite
file: restore by copying it to a stopped backend and starting it.

Back up the whole of `/var/lib/spoond/` and `/etc/spoond/` from the host.
The identity store (`users.json` and its `.salt` sidecar) holds users, key
fingerprints, quotas and token hashes — losing the salt makes every
existing token unverifiable, so keep it with the store. The template
storage under `E2B_TEMPLATE_STORAGE_PATH` holds the build artifacts; back
it up too, or rebuild the images with `spoond images build --all`.

## Catalog GC

Builds accumulate: every image build, pause, resume and checkpoint creates
one. GC runs hourly and is **dry-run by default**, logging each candidate
as `gc: would delete <build_id> kind=<k> image=<i>`.

A build is a candidate only when it is unreferenced: not any image's
`current_build_id`, not the `parent_build_id` of a live build, not a
`ref_build_id` in `build_refs` (E2B's scheduling metadata makes builds
share blocks, so those must be kept), and its state is not `deleted`.

Check the last 7 days of candidates before enabling deletion:

```bash
journalctl -u spoond-backend --since -7d | grep 'gc: would delete'
```

Then set `GC_DELETE=1` in `/etc/spoond/backend.env` and restart the
backend. Keep a copy of the env file first: rollback is restoring it and
restarting. `spoond_gc_deleted_total` counts deletions by kind, and
`spoond_snapshot_bytes` / `spoond_storage_free_bytes` track the disk.

## Common failures

| Symptom | Cause | Fix |
|---|---|---|
| `Text file busy` on deploy | overwrote a running binary | deploy to `.new` then `mv` |
| create fails, hugepages mentioned | admission refused: no free hugepages | free sandboxes or raises `vm.nr_hugepages`; see `spoond_node_hugepages_free_bytes` |
| create `503`, drain state stuck | orchestrator unreachable | `systemctl status e2b-orchestrator`; undrain only after it is back |
| lease `410 lost` | orchestrator crashed before a checkpoint | delete the lease; lower `CHECKPOINT_INTERVAL_MINS` if this recurs |
| `sandbox is suspended; resume it first` | op needs a live sandbox | `resume <id>` first |
| exec `proxy.golang.org` blocked | `lan` policy has no internet | use `network_policy: internet` or an allowlist |
| `spoond doctor` fails the artifact check | pinned Firecracker/kernel/envd changed | follow the [upgrade runbook](runbooks/e2b-upgrade.md) |

## Users & identity

- **Revoking access** = `DELETE /api/users/{id}` (or `ssh-key rm
  <user-id>`); with the identity store present the gateway treats it as
  authoritative, so removal is immediate.
- **Quotas** are per-user (`max_leases`/`max_ttl` via
  `POST /api/users/{id}/quota`); over-cap creates return `429`. A user
  with `max_leases: 0` is unlimited.
- **Salt rotation / token hashes**: token and LLM-key hashes are
  HMAC-SHA256 with a per-store salt (sidecar `<users-file>.salt`); back
  up the salt alongside the store or existing hashes become unverifiable
  on restore.
- **Forward-auth proxy** (`PROXY_AUTH_MODE=forward-auth`): the
  `PROXY_AUTH_SECRET` is shared with the IdP/Caddy; ensure Caddy strips
  inbound `X-Proxy-Auth`/`Remote-User` headers so guests can't spoof
  them, and keep `PROXY_AUTH_TRUSTED_PEERS` to the proxy's own CIDR.
