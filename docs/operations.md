# Operations

Runbook for operating a spoond deployment on the E2B substrate: health,
`spoond doctor`, backups, snapshot GC, the drain protocol, crash
recovery, restarting the orchestrator or the backend, and the dashboard.
What the substrate is and why it behaves this way is
[substrate.md](substrate.md).

## Component health

| Check | Command |
|---|---|
| Orchestrator | `curl -fsS http://127.0.0.1:5008/health` (body has `"status":"healthy"`) |
| Host firewall | `nft list table inet e2b_guard >/dev/null && echo ok` |
| Backend | `systemctl is-active spoond-backend` |
| Backend + orchestrator together | `curl -s --resolve "$HOST:8890:127.0.0.1" https://$HOST:8890/healthz` (`$HOST` = the name in the TLS certificate; `127.0.0.1` itself does not validate) → `200 {"status":"ok","orchestrator":"healthy"}`, `503 {"status":"degraded","orchestrator":"unreachable"}` when the orchestrator is down |
| Gateway | `systemctl is-active spoond-sshd-gateway` |
| Runner | `systemctl is-active spoond-runner` |
| Registry | `curl -fsS http://127.0.0.1:5000/v2/` |
| OTel collector | `curl -s http://127.0.0.1:19464/metrics >/dev/null && echo ok` (the Prometheus exporter spoond's `/metrics` appends) |
| Metrics | `curl -s --resolve "$HOST:8890:127.0.0.1" -H "Authorization: Bearer $METRICS_TOKEN" https://$HOST:8890/metrics` |
| Dashboard | `curl -fsS -u "$DASH_USER:$DASH_PASS" http://127.0.0.1:8893/` (plain HTTP by default; with `DASH_TLS_CERT`/`DASH_TLS_KEY` set, `https://$HOST:8893/` with `--resolve` as above) |
| Identity store | `test -f /var/lib/spoond/users.json && stat -c '%a' /var/lib/spoond/users.json` (expect `600`) |

`spoond dash` (below) is the watching surface; `spoond doctor` is the
triage one.

## `spoond doctor`

`spoond doctor` exercises every external surface the backend depends on
and prints a `PASS` / `FAIL` / `WARN` table, exiting 1 when anything is
failing. It reads the same environment the backend uses, so it reflects
the deployed configuration rather than a shell's:

```bash
set -a; . /etc/spoond/backend.env; set +a
/opt/spoond/spoond doctor            # or --json; --manifest <path> overrides the manifest
```

| Check | PASS / FAIL / WARN |
|---|---|
| `config: CONSUMER_TOKENS` | FAIL when unset (the backend refuses to start) |
| `config: BIND_ADDR` | WARN when unset (defaults to `127.0.0.1:8890`) |
| `orchestrator: /health` | `GET http://$E2B_GRPC_ADDR/health` must answer `"status":"healthy"` |
| `orchestrator: node info` | status, running sandboxes and free hugepages; WARN when the status is not `healthy` or free hugepage memory is below 4 GiB |
| `registry: /v2/` | the local registry answers `200` |
| `token seed: file` | `/etc/spoond/e2b-token-seed` exists, mode 0600, at least 32 bytes |
| `store: database` | the SQLite file opens read-only and its migration version is ≥ 4 |
| `catalog: baked images` | every manifest image with `baked: true` has a `current_build_id` in `ready` state (WARN when the manifest has none) |
| `artifacts: sha256` | Firecracker, the guest kernel and busybox match their pins, **and** prints the distinct Firecracker/kernel versions non-deleted builds still use — never delete a `/fc-versions/<v>` or `/fc-kernels/<v>` directory while it appears there |
| `storage: free space` | WARN below 20 GiB free at `E2B_TEMPLATE_STORAGE_PATH` |
| `lease API: listener` + `/healthz` | the backend listener is up and healthy. The `/healthz` probe over TLS trusts the backend's own cert chain loaded from `TLS_CERT` and picks the hostname from the cert's DNS SAN (a wildcard bind will not validate) — a separate `lease API: TLS trust` check FAILs when the cert cannot be read, and verification is never skipped |
| `ssh gateway: listener` | the gateway port (`GATEWAY_ADDR`, default `127.0.0.1:2222`) answers |
| `llm gateway: upstream` / `key` / `/models` | upstream configured, key present, key accepted |
| `tls: cert/key` | WARN when unconfigured (plain HTTP), FAIL when the pair does not load |
| `disk: root` | WARN above 75% full, FAIL above 90% |
| `drain: shutdown unit` | FAIL unless `spoond-drain.service` is enabled, active and ordered after `spoond-backend` and `e2b-orchestrator`, and `/etc/e2b/drain.env` names a backend and a readable token file; without it a reboot loses every running lease (see Rebooting the host) |

## Backups

State that matters is SQLite plus three small files. The backend backs
the database up itself: a `VACUUM INTO` copy daily at 03:00 local time
(and once at start when the newest copy is older than 24 h) into
`SPOOND_BACKUP_DIR` (default `/var/lib/spoond/backups`), named
`<db-basename>-<YYYYMMDD-HHMMSS>.db`, keeping the newest 7. Restore by
copying a backup over `SPOOND_DB_PATH` while the backend is stopped.

Back up by hand, on whatever schedule you keep:

```bash
tar czf /backup/spoond-$(date +%F).tar.gz \
  /var/lib/spoond/spoond.db /var/lib/spoond/users.json \
  /var/lib/spoond/users.json.salt /etc/spoond /etc/e2b /etc/spoond-gateway
```

- `users.json` + its `.salt` sidecar are the identity store; the salt is
  what makes the stored token and LLM-key hashes verifiable, so losing it
  invalidates every token.
- `/etc/spoond/e2b-token-seed` mints every envd/traffic token; losing it
  invalidates them all.
- `/etc/e2b/orchestrator.env`, `flags.json` and `drain.env` are the
  orchestrator's configuration.
- The build store under `E2B_TEMPLATE_STORAGE_PATH` is reproducible
  (`spoond images build --all`) and is normally left to the GC.

## Snapshot catalog GC

Every pause, checkpoint and template build is a directory under
`E2B_TEMPLATE_STORAGE_PATH/<build_id>/`. Builds form chains
(`parent_build_id`) and their headers reference other builds' blocks
(`build_refs`, taken from E2B's scheduling metadata), so a build must
never be deleted while a descendant or a referencing build is kept.

The GC runs 10 minutes after the backend starts and then hourly. It
computes the kept set — every image's current build, every lease's resume
and last-checkpoint builds, every sandbox's build (the warm pool
included), every in-flight build, and the transitive closure of their
parents and references — and considers only `ready`/`failed` builds
outside it that have been idle for an hour.

- **Dry-run is the default** (`GC_DELETE` unset or `0`): candidates are
  logged as `gc: would delete <build_id> kind=<k> image=<i>`. Nothing is
  removed.
- `GC_DELETE=1` makes the GC actually delete candidates, marking them
  `deleted` and counting `spoond_gc_deleted_total{kind}`. Only enable it
  after reading a week of dry-run logs.
- Users manage their own snapshots through the API:
  `GET /api/snapshots` lists the caller's builds with `in_use` flags, and
  `DELETE /api/snapshots/{build_id}` removes one (`409` while anything
  references it, `403` for template builds). See [api.md](api.md).

Disk accounting runs with the GC: each non-deleted build's `size_bytes`
is refreshed (allocated blocks, not apparent size) and exposed as
`spoond_snapshot_bytes{kind}`, with `spoond_storage_free_bytes` for the
store's free space.

## Restarting the orchestrator (planned)

`systemctl restart e2b-orchestrator` is safe: the unit's drain hooks make
it lossless. Do not stop the backend first.

1. systemd runs `ExecStop=/opt/spoond/spoond drain --stop` **before**
   sending SIGTERM.
2. That calls `POST /api/admin/drain`, which sets the node draining (the
   backend refuses new grants while it is), pauses every live lease into
   a pause build and marks it `drained`, deletes the warm pool, and waits
   (up to 180 s) until the node reports no running sandboxes and no
   outstanding work. Per-lease failures are recorded in the response and
   the drain continues.
3. The orchestrator stops; on start, `ExecStartPost=/opt/spoond/spoond
   drain --start` waits for the node (up to 120 s), calls
   `POST /api/admin/undrain`, which clears draining and resumes exactly
   the drained leases. A lease that fails to resume becomes `lost`.
4. If systemd's `SERVICE_RESULT` is not `success` (the orchestrator
   crashed or was killed), the drain is skipped — there is nothing to
   pause — and the backend's crash reconcile handles recovery.

## Network watchdog

`spoond-netwatch.service` (`deploy/e2b/spoond-netwatch.sh`, installed as
`/usr/local/sbin/spoond-netwatch`) probes the gateway and the LAN
resolver every 15 s. When neither answers for 2 minutes it logs the
physical port's state and bounces the port; a minute later it runs
`ifreload -a`; after 10 minutes, if the port received nothing in that
time and it has not rebooted the host in the last 12 hours, it reboots
cleanly (so `spoond-drain` drains leases first). A port that still
receives means the fault is upstream, and it does not reboot. Its log
lines start with `netwatch:` (`journalctl -u spoond-netwatch`). It was
added after the 10 GbE port's receive path died on 2026-10-02 with the
link still up.

By hand, from the console: record `ip -s link show enp1s0f0; ethtool -S
enp1s0f0 | grep -v ': 0$'`, then `ip link set enp1s0f0 down; ip link set
enp1s0f0 up`, then `ifreload -a`; reboot with `reboot` (never the reset
button) only if those fail.

## Rebooting the host (planned)

A host shutdown stops every unit, and the orchestrator's own drain hook
cannot work then: `spoond-backend` is ordered after the orchestrator, so
systemd stops the backend first and the hook's drain call is refused
(this lost every lease in a reboot on 2026-10-02). `spoond-drain.service`
(`deploy/e2b/spoond-drain.service`) covers it. It is ordered after both
units, so at shutdown it stops first and its `ExecStop=spoond drain
--stop` drains while both are up; at boot it starts last, waits for
`/healthz` to report the orchestrator healthy, and its `ExecStart=spoond
drain --start` resumes the drained leases. Drain and undrain are
idempotent, so the orchestrator's own hooks, which also run, do nothing
the second time. Install it with the orchestrator unit and enable it:

```bash
install -m 644 deploy/e2b/spoond-drain.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now spoond-drain
```

Do not stop `spoond-backend` and `e2b-orchestrator` by hand together
without stopping `spoond-drain` first (`systemctl stop spoond-drain`
drains; `systemctl start spoond-drain` resumes).

`spoond drain` always exits 0, even when a call fails: a failed drain
must never block the stop. Watch a restart with:

```bash
journalctl -u e2b-orchestrator -f        # drain/undrain output, then "startup resource reclaim completed"
journalctl -u spoond-backend -f          # recovery: lease <id> recovered (checkpoint <at>)
```

The drain hook can serve more than one backend
(`SPOOND_DRAIN_URL`/`SPOOND_ADMIN_TOKEN_FILE` accept comma-separated
lists of equal length, drained in order) — that is how a staging backend
shares the orchestrator during an upgrade.

## Crash recovery

An orchestrator crash kills every running sandbox (the accepted trade-off
D4; there is no supervisor per VM). spoond reconciles:

- once at backend start, after loading state;
- every 30 s in the background, while not draining;
- immediately when the orchestrator's node info goes from failing to
  succeeding.

For each lease whose sandbox no longer exists: **with** a checkpoint it is
resumed from that build with the same sandbox id and becomes `recovered`
(logging `recovery: lease <id> recovered (checkpoint <at>)`); **without**
one it becomes `lost`. Nothing is marked lost when the sandbox list itself
cannot be read — a transient failure never destroys lease state. Orphan
sandboxes no lease or pool entry claims are deleted, and peer egress
allowances are refreshed.

To the API and the gateway, `recovered` behaves exactly like `running`
(`state` keeps showing it until the lease is suspended or restarted),
while `lost` answers `410 {"error":"lease lost in a substrate crash;
delete this lease"}` on exec, stream, proxy and SSH. `POST
/api/admin/reconcile` runs the reconciliation on demand and returns
`{"recovered":N,"lost":M}` (admin token).

Persistent leases are checkpointed in the background every
`CHECKPOINT_INTERVAL_MINS` (default 60, `0` disables), only when they
have been active since the last checkpoint — that is what bounds the
loss. Users can force one with `POST /api/leases/{id}/checkpoint`.

## Restarting the backend

A backend restart loses nothing: leases, shares, the pool and the catalog
are in SQLite and are loaded on start. Use it freely.

```bash
systemctl restart spoond-backend
curl -fsS --resolve "$HOST:8890:127.0.0.1" https://$HOST:8890/healthz
```

On start the backend loads state, deletes substrate sandboxes nothing
claims, reconciles the crash state, then warms the pool. There is a brief
window while it comes up; the gateway retries backend calls through it.

The SSH gateway is deliberately only `Wants=`-coupled to the backend, so
a backend restart does not stop the gateway.

## Capacity and the warm pool

Admission is hugepage-based: a create is refused with `503 capacity: …`
when the node's free hugepage memory (total − used − reserved, × page
size) is below the image's `memory_mb`, or when the node is not
`healthy`. Refusals are counted in `spoond_capacity_rejections_total`.
If creates start failing this way, check `grep HugePages_
/proc/meminfo` and `spoond_node_hugepages_free_bytes` first.

`POOL_SIZE` pre-creates that many sandboxes per image with a current
build so grants are served without a cold restore. Production runs
`POOL_SIZE=0` — with snapshot restores, a cold grant is tens of
milliseconds, and pool sandboxes hold hugepage memory around the clock.
If you do raise it, remember the pool's memory is `POOL_SIZE × images ×
memory_mb` of hugepages held permanently, and that the drain deletes the
pool (it refills afterwards).

A pooled sandbox is created under a placeholder lease with
`SPOOND_LEASE_ID=pool`; when it is handed to a real lease, the backend
re-applies that lease's egress policy and TTL and adds the lease id to
each request's env.

## Common failures

| Symptom | Cause | Fix |
|---|---|---|
| `503 capacity: … bytes of hugepage memory free` | not enough free hugepages for the image, or the node is draining/unhealthy | free sandboxes, lower `POOL_SIZE`, or raise `vm.nr_hugepages` (then re-check with doctor) |
| `410 lease lost in a substrate crash` | the lease had no checkpoint when the orchestrator died | delete the lease; nothing to resume |
| `409 lease is suspended; resume it first` | the lease is paused | `resume` it (the SSH gateway does this automatically on attach) |
| `409 lease is busy; retry` | a suspend/resume/restart/checkpoint is already in flight on that lease | retry once it finishes |
| `exec failed` / `agent unreachable` | envd in the guest is not answering (sandbox died under us, node overloaded) | `spoond doctor`; if the sandbox is really gone the next reconcile marks the lease |
| `unknown image tag: …` on create | the image has no current build in the catalog | `spoond images build <name>` (see [ci-jobs.md](ci-jobs.md)) |
| `spoond images build` fails at the template step | the base image is RHEL-family (E2B rejects it) or the registry is down | use a Debian/Ubuntu/Fedora/Arch/Alpine/NixOS base; `curl 127.0.0.1:5000/v2/` |
| `Text file busy` on deploy | overwrote a running binary | build to `spoond.next` then `mv` it into place (the cutover procedure's pattern) |
| Orchestrator restart-looping after an upgrade | the drain timed out and `TimeoutStopSec`/`TimeoutStartSec` are below 420/330 | set both on the unit and restart |
| `connection refused` on :2222 | gateway down/restarting | `systemctl restart spoond-sshd-gateway` |
| Doctor `artifacts: sha256` FAIL | a pinned binary was replaced or removed | restore the pinned Firecracker/kernel/busybox; a mismatch means snapshots may not restore |

## Diagnostics first

Before any recovery, capture state:

```bash
journalctl -u e2b-orchestrator --since "30 min ago" --no-pager > /tmp/orch.log
journalctl -u spoond-backend     --since "30 min ago" --no-pager > /tmp/be.log
curl -s http://127.0.0.1:5008/health > /tmp/health.json
ps -eo pid,etimes,comm,args | grep '[f]irecracker' > /tmp/fc.txt
sqlite3 "file:/var/lib/spoond/spoond.db?mode=ro" \
  "select state,count(*) from leases group by state; select name,current_build_id from images;"
```

Failed CI jobs are recorded as JSON under `/var/lib/spoond/jobs/`
(`JOB_RECORD_DIR`), naming the failing step, its exit code and its output
tail — the first place to look for a red build, since Forgejo exposes no
readable log API. See [ci-jobs.md](ci-jobs.md).

## Held-lease limits

A held lease (`holder` set, see [api.md](api.md)) is exempt from the
plain TTL and idle sweeps — that is the point — but it must never be
able to keep memory or disk forever, and nobody watches the dashboard.
So the limits act on their own: they run in the existing sweep loop
(every `sweepInterval`, 5 s) and skip while the node is draining. Every
automatic action is logged as one line naming the lease, the holder,
the rule and the numbers that triggered it, counted in
`spoond_held_actions_total{rule,action}`, and recorded on the lease
(`last_action`, `last_action_at`, returned by the lease API with
`hold_expires_at`). A hold also expires on its own: it lasts
`HOLD_TTL_SECS` from when it was set or renewed (renewal is `PUT
/api/leases/{id}/holder` with the same holder), at most
`HOLD_TTL_MAX_SECS` for an explicit `hold_ttl`; past expiry the holder
is cleared and the lease follows the normal TTL and idle rules from
then on (an already-expired TTL releases it at the next sweep).

| # | Rule | Variable | Default | Meaning |
|---|---|---|---|---|
| 1 | Idle suspend | `HELD_IDLE_TIMEOUT_SECS` | `14400` (4 h) | a held lease with no activity — what the idle sweep already counts: exec, stream, proxy, keepalive, guest heartbeat — for this long is **suspended** (memory and hugepages freed; nothing deleted; it resumes on next use, the SSH gateway does that on attach) |
| 2 | Stale release | `HELD_SUSPENDED_RELEASE_SECS` | `604800` (7 d) | a held lease suspended by rule 1 or 4 and untouched for this long is **released** (deleted); the GC reclaims its builds |
| 3 | Hold expiry | `HOLD_TTL_SECS`, `HOLD_TTL_MAX_SECS` | `604800` (7 d), `2592000` (30 d) | the hold ends on its own; holder and `holder_url` are cleared and normal sweeping resumes |
| 4 | Pressure | `PRESSURE_DISK_FREE_PCT`, `PRESSURE_HELD_IDLE_SECS` | `15`, `1800` (30 min) | when snapshot-disk free space is under the percentage, or free hugepages are short (admission would refuse a default-size lease), rule 1 uses the shorter threshold |
| 5 | Critical disk | `CRITICAL_DISK_FREE_PCT`, `CRITICAL_DISK_RECOVER_PCT` | `5`, `10` | when snapshot-disk free space is under the critical percentage, held leases already suspended by rule 1 or 4 are **released** oldest suspension first — after the GC has run — until free space is above the recovery percentage; a running lease is never released |
| 6 | Scheduling | — | — | the rules run in the existing sweep loop and skip while the node is draining |

Set any of the numeric variables to `0` to disable that rule (a rule
off means held leases are again bounded only by their holder clearing
them). Watch the rules with `journalctl -u spoond-backend | grep 'held
lease'` and `spoond_held_actions_total` — a rising `critical{release}`
means the disk needs attention the leases are paying for.

## Users & identity

- **Revoking access** = `DELETE /api/users/{id}` (or `ssh-key rm
  <user-id>`); the gateway treats the identity store as authoritative, so
  removal is immediate — no key-dir cleanup needed.
- **Quotas** are per-user (`max_leases`/`max_ttl` via `POST
  /api/users/{id}/quota`); over-cap creates and forks return `429`. A
  user with `max_leases: 0` is unlimited.
- **Token/key hashes** are HMAC-SHA256 with a per-store salt (sidecar
  `<users-file>.salt`); back the salt up alongside the store or existing
  hashes become unverifiable on restore.
- **Forward-auth proxy** (`PROXY_AUTH_MODE=forward-auth`): the
  `PROXY_AUTH_SECRET` is shared with the IdP/Caddy; ensure Caddy strips
  inbound `X-Proxy-Auth`/`Remote-User` headers so guests can't spoof
  them, and keep `PROXY_AUTH_TRUSTED_PEERS` to the proxy's own CIDR.
- See [security.md](security.md) for the boundaries and [api.md](api.md)
  for the endpoints.

## Dashboard (`spoond dash`)

`spoond dash` is a read-only, live view of spoond's present operation,
for watching rather than triage: lease and sandbox counts, host CPU,
memory, hugepages and snapshot disk, five-minute sparklines (sandboxes,
API requests, lease grant time, egress connections), the systemd units,
live leases and the image catalog. It runs as its own service on **:8893** behind
**basic auth** — set `DASH_USER` and
`DASH_PASSWORD_HASH` (a bcrypt hash; `spoond dash hash <password>` prints
one) — and serves HTTPS when `DASH_TLS_CERT`/`DASH_TLS_KEY` are set.
Since basic auth sends the password, use TLS unless it is bound to
localhost.

One collector loop builds a snapshot every `DASH_INTERVAL` (default 2 s)
from spoond's `/metrics` using the scrape-only `METRICS_TOKEN`, the
SQLite catalog opened read-only, user names from the identity store,
`/proc` and systemd. Every viewer shares that loop through a single
server-sent-event stream, and `DASH_HISTORY` (default 150) points of
history are kept so a new page starts with trends. Configuration lives in
`cmd/spoond-dash/dash.go`; the notable variables:

| Variable | Default | Purpose |
|---|---|---|
| `DASH_ADDR` | `0.0.0.0:8893` | listen address |
| `DASH_USER`, `DASH_PASSWORD_HASH` | *(required)* | basic auth (`spoond dash hash PASS` makes the hash) |
| `DASH_TLS_CERT`, `DASH_TLS_KEY` | *(unset)* | serve HTTPS (set both or neither) |
| `METRICS_URL` | `https://127.0.0.1:8890/metrics` | spoond's `/metrics` |
| `METRICS_SERVER_NAME` | `spoond.example.com` | TLS server name for that URL |
| `METRICS_TOKEN` | *(required)* | the backend's scrape-only token |
| `SPOOND_DB_PATH` | `/var/lib/spoond/spoond.db` | catalog database (opened read-only) |
| `USERS_FILE` | `/var/lib/spoond/users.json` | identity store (names only) |
| `E2B_TEMPLATE_STORAGE_PATH` | `/forkdcache/e2b/storage/templates` | disk to report |
| `DASH_SERVICES` | `spoond-backend,spoond-runner,spoond-sshd-gateway,e2b-orchestrator,e2b-guard,otelcol` | systemd units to show |
| `DASH_INTERVAL` | `2s` | refresh interval (minimum 1 s) |
| `DASH_HISTORY` | `150` | sparkline points kept (10–200) |

The dashboard can only read: it has no write path to the backend, the
database or the orchestrator, and the metrics token it holds is refused
everywhere except `/metrics`.

## Metrics

`GET /metrics` (admin user, or the scrape-only `METRICS_TOKEN`) serves
spoond's registry and, when `OTEL_PROM_URL` is set, appends the
orchestrator's Prometheus output after a `# --- orchestrator (otel) ---`
marker. The substrate-specific series:

| Series | Meaning |
|---|---|
| `spoond_leases{state}` | leases per state (`running`, `suspended`, `recovered`, `lost`) |
| `spoond_leases_by_image{image}` | live leases per image |
| `spoond_node_running_sandboxes` | orchestrator running count |
| `spoond_node_hugepages_free_bytes` | (total − used − reserved) × page size |
| `spoond_node_outstanding_work` | orchestrator outstanding work (in-flight snapshot uploads) |
| `spoond_create_duration_seconds{resume}` | `Create` latency, resume vs fresh |
| `spoond_checkpoint_duration_seconds` | checkpoint latency |
| `spoond_snapshot_bytes{kind}` | build disk per kind |
| `spoond_storage_free_bytes` | free bytes at the build store |
| `spoond_gc_deleted_total{kind}` | builds deleted by the GC |
| `spoond_held_actions_total{rule,action}` | automatic actions on held leases: `rule` is `idle`, `stale`, `expiry`, `pressure` or `critical`; `action` is `suspend`, `release` or `expire` |
| `spoond_capacity_rejections_total` | admission refusals |
| `spoond_store_errors_total{op}` | SQLite write failures |

`spoond_leases{state="lost"}` above zero means an orchestrator crash
happened — it is the number the soak watch uses. Deploying
`deploy/e2b/spoond-soak.{service,timer}` (with `soak-check.sh` from
`deploy/e2b/` in `/usr/local/lib/spoond/`) automates that watch: once a
day it sources the backend env, runs `spoond doctor`, reads the lease
states and the runner's job results of the last 24 h read-only from
SQLite, and appends one JSON line to `/var/lib/spoond/soak.log`. The
unit fails (so `systemctl --failed` shows it) when doctor reports a
FAIL or any lease is `lost`.
