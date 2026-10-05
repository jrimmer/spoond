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
triage one; Gatus (above) is the machine that watches on its own.

## Uptime monitoring (Gatus)

For an external watcher, `/readyz` is the endpoint to poll: unlike
`/healthz` (liveness — the process answers) it fails when a dependency
is wrong, so a monitor pages before a human notices. Both listeners
serve it without auth and bound each check to 2 s; the lease API caches
its answer for 5 s, the dashboard answers live.

- Lease API (`:8890`): `200 {"status":"ok"}` only when the
  orchestrator reports the node healthy, the catalog answers a trivial
  query, and the snapshot disk (danger ≥ 90 % used) and hugepage pool
  (danger ≥ 92 % used) are below the dashboard's danger levels;
  otherwise `503 {"status":"fail","checks":[{name, ok, detail}…]}`
  names what failed (see [api.md](api.md)).
- Web dashboard (`:8893`): `200` only when the metrics scrape, the
  catalog and the identity store all answer.

An example [Gatus](https://github.com/TwiN/gatus) configuration —
hostnames and alerting are deployment choices; the checks and
conditions are the point. The two HTTPS endpoints get all three:
status, response time (staying inside the 2 s bound the endpoint
itself enforces per check) and TLS certificate expiry; the SSH gateway
speaks SSH, not TLS, so its check is a TCP connect with the connected
and response-time conditions:

```yaml
endpoints:
  - name: lease-api-readyz
    url: https://vm2.lacy.casa:8890/readyz
    interval: 30s
    conditions:
      - "[STATUS] == 200"
      - "[RESPONSE_TIME] < 2000"        # milliseconds; /readyz bounds every check to 2 s
      - "[CERTIFICATE_EXPIRATION] > 72h"

  - name: dashboard-readyz
    url: https://vm2.lacy.casa:8893/readyz
    interval: 30s
    conditions:
      - "[STATUS] == 200"
      - "[RESPONSE_TIME] < 2000"
      - "[CERTIFICATE_EXPIRATION] > 72h" # needs DASH_TLS_CERT set, else drop this condition

  - name: ssh-gateway
    url: tcp://vm2.lacy.casa:2222       # a TCP connect proves the listener answers
    interval: 30s
    conditions:
      - "[CONNECTED] == true"
      - "[RESPONSE_TIME] < 2000"
```

`[CERTIFICATE_EXPIRATION] > 72h` pages while there is still time to
renew — ahead of the 30-day expiry banner the dashboard draws for its
own cert. If the gateway fronts a TLS listener of its own, give it the
same certificate condition on that endpoint.

The dashboard listens on every interface by default (`DASH_ADDR`,
default `0.0.0.0:8893`); the backend follows `BIND_ADDR` — loopback
when unset, a public bind in the full deployment. Either way, point
Gatus at the public name behind the host's TLS termination so the
certificate condition watches what visitors actually see.

## Notifications to webhooks

Gatus pulls; webhooks push. With `NOTIFY_WEBHOOKS` set, the backend
sends the events that need a person to your own push channels — ntfy,
Slack or Discord, or anything that takes JSON — instead of waiting for
someone to look at a dashboard. Unset (the default) keeps the notifier
off entirely.

The value is a JSON list; every entry is one receiver:

```json
[
  {"url": "https://ntfy.example/spoond-alerts",
   "format": "ntfy",
   "min_severity": "warn",
   "headers": {"Authorization": "Bearer tk_ntfy.example_xxx"}},
  {"url": "https://discord.com/api/webhooks/123/abc",
   "format": "slack",
   "events": ["lease.*", "unit.*", "disk.*"]},
  {"url": "https://hooks.example.internal/spoond",
   "format": "json"}
]
```

| Field | Meaning |
|---|---|
| `url` | required, http(s). May carry secrets (tokens in path, query or userinfo) — see Secrets below |
| `format` | `ntfy` (the topic URL, `https://ntfy.example/<topic>`: the topic is its last path segment, and the message is published as JSON to the server root with severity as the numeric priority plus a tag), `slack` (`{"text": …}` — Slack and Discord incoming webhooks both take it), or `json` (the event object itself) |
| `min_severity` | floor: `info` (default), `warn` or `critical`. A resolved message carries the condition's own severity, so a warn webhook that heard about a problem also hears that it cleared |
| `events` | optional key filter; exact keys or `*` globs (`disk.*`); empty means everything |
| `headers` | optional extra request headers (auth tokens); never logged |

What arrives, with its key and severity:

| Key | Severity | When |
|---|---|---|
| `lease.lost.<lease-id>` | critical | a lease was lost (orchestrator crash, failed recovery) |
| `held.<rule>.<lease-id>` | warn, critical on `release` | a held-lease rule acted on a lease |
| `unit.inactive.<unit>` | critical | a watched systemd unit is not active. `NOTIFY_UNITS` lists them, comma-separated; the default is `e2b-orchestrator.service,spoond-sshd-gateway.service`, and `none` watches nothing (a host without systemd, where every probe would fail) |
| `disk.warn` / `disk.danger` | warn / critical | the snapshot disk past 80 % / 90 % used |
| `disk.kept` | warn | kept checkpoints (#126) past `KEPT_DISK_WARN_PCT` (default 40) percent of the snapshot disk. The critical-disk rule never deletes a kept build, so only a person can unpin — that is what this alert asks for |
| `hugepages.warn` / `hugepages.danger` | warn / critical | the hugepage pool past 80 % / 92 % used |
| `tls.cert.30d` / `.7d` / `.1d` | warn / warn / critical | the TLS certificate within 30, 7 or 1 day of expiry |
| `gc.failed` | warn | the last snapshot catalog GC pass failed |
| `backup.stale` | warn | the newest database backup older than its age limit — `BACKUP_MAX_AGE_SECS`, default 93600 (26 h: the 03:00 daily run plus one missed day) |

Delivery rules:

- Asynchronous: events are queued and delivered in the background; a
  slow or dead webhook never blocks the lease API.
- A failing delivery retries with exponential backoff (2 s, 4 s, 8 s
  …) for up to an hour, then the message is dropped and counted.
- The same key is delivered at most once per hour while a condition
  keeps firing.
- When a warn/critical condition clears, one `resolved` message goes
  out for its key — and only then: a healthy deployment sends nothing.
- Each webhook is rate-limited to 30 deliveries per hour; beyond that,
  messages wait for the window to free.
- Every outcome is counted in
  `spoond_notifications_total{webhook,severity,result}` (see Metrics);
  `webhook` is the receiver's index in `NOTIFY_WEBHOOKS`, `result` is
  `sent`, `retry`, `dropped`, `deduped` or `rate_limited`.

Secrets: webhook URLs and headers may carry tokens and are never
logged. Logs, metrics, `spoond doctor` output and the state file name
a webhook by its index and a redacted `scheme://host` only, delivery
errors are stripped of anything URL-bearing, and a redirecting
endpoint is treated as a failure rather than followed (following one
would replay the configured headers elsewhere).

Two tools close the loop:

- `spoond notify test` posts one info message to every configured
  webhook (in parallel) and prints a per-webhook PASS/FAIL line —
  redacted. Exit 0 when all answered, 1 when any failed, 2 on a
  configuration error. Use it after changing `NOTIFY_WEBHOOKS`.
- `spoond doctor` (below) checks each webhook's reachability with the
  same kind of POST and reports the deliveries the backend dropped in
  the last 24 h, read from the notifier's state file
  (`NOTIFY_STATE_FILE`, default `notify-state.json` next to the
  database). A few retries that ended in `sent` are business as usual;
  a stream of `dropped` means the receiver is down and nobody is being
  paged.

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
| `notify: webhooks` | WARN when `NOTIFY_WEBHOOKS` is unset (notifications are optional), FAIL on a malformed list — one `notify: webhook N reachability` check per webhook, POSTing a test message; FAIL when the receiver does not answer 2xx |
| `notify: dropped deliveries` | WARN when the backend gave up on webhook deliveries in the last 24 h (read from the notifier's state file, `NOTIFY_STATE_FILE`) |
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
store's free space. A freshly written build's `size_bytes` is measured
at write time and then confirmed hourly, so `GET /api/snapshots` shows
a new snapshot's size immediately.

### Kept checkpoints and their caps

A lease can **keep** a checkpoint (`{"keep":true}` on the checkpoint
route): the build is pinned as a GC root and restore point while the
lease lives. Two caps keep pins from filling the disk (#126):

- `MAX_KEPT_PER_LEASE` (default `4`, `0` = no cap) bounds the keeps per
  lease. A keep on a lease at the cap answers `409` and takes no
  checkpoint; the owner unpins one build
  (`DELETE /api/snapshots/{build_id}`) to free a slot.
- A user's `max_kept_bytes` (identity-store field, set via
  `POST /api/users/{id}/quota`) bounds the bytes their kept builds hold.
  An over-budget keep is written and then left unpinned (409 with the
  build id), so it ages out like any unreferenced snapshot.

`GET /api/leases/{id}` lists a lease's kept builds (id, size, kept_at);
`spoond_kept_builds` and `spoond_kept_builds_bytes` report the totals
over live leases. When kept bytes pass `KEPT_DISK_WARN_PCT` (default
`40`) percent of the snapshot disk, the dashboard's attention strip says
so and the notifier raises `disk.kept` (warn) — the held-lease
critical-disk rule never deletes a kept build, so unpinning stays with
the owner.

Interaction with the held-lease critical rule (rule 5 in [Held-lease
limits](#held-lease-limits)): a release frees no disk by itself — the
space returns only when the GC reclaims the released lease's builds,
which takes the GC age (1 h) and `GC_DELETE=1`. Under the dry-run
default the critical rule therefore releases nothing (it logs, at most
once an hour, that the dry-run GC stops it); a full snapshot disk on a
node with held leases is a reason to turn the GC out of dry-run.

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

Persistent leases are checkpointed in the background by
`CHECKPOINT_INTERVAL_MINS` — since 2.3 that value is the **default
per-lease interval** for leases without their own, and its default is
`0` = never (before 2.3 it was 60 and applied to every persistent
lease). A lease's own `checkpoint_interval` (`POST /api/leases`, `PUT
/api/leases/{id}/checkpoint-policy`) overrides the default: `0` =
never, 60..604800 = seconds. The loop ticks every minute and
checkpoints a live lease whose effective interval has elapsed since its
last checkpoint and that has been active since — that is what bounds
the loss. Being held no longer puts a lease on the pass (before 2.3 it
did). Users can force one with `POST /api/leases/{id}/checkpoint`, and
every checkpoint's guest pause is observed in
`spoond_checkpoint_pause_seconds` with a log line naming the lease, the
pause and the image's `memory_mb`.

Keeps are capped (`MAX_KEPT_PER_LEASE`, default `4`, `0` = no cap): a
keep on a lease at the cap answers `409` and takes nothing — see
[Kept checkpoints and their caps](#kept-checkpoints-and-their-caps) in
the GC section. `KEPT_DISK_WARN_PCT` (default `40`, `0` = off) is when
kept bytes alone start drawing attention: the dashboard's strip and the
notifier's `disk.kept` both read it.

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

## The runner

`spoond-runner` (the `runner` subcommand) runs Forgejo Actions jobs in
sandboxes leased from the backend. Each job lease is labelled with its
job (#119): right after the create, the runner sets the lease's comment
to `forgejo job <id> <job URL>`. The lease is deliberately **not held**:
a hold would keep it out of the TTL sweep, and since 2.3 a hold no
longer brings periodic checkpointing with it anyway (a lease is
checkpointed only when its own `checkpoint_interval` says so — set one
on create if a job's work must survive a crash). A job lease lives for
`LEASE_TTL` like any plain lease.

**Orphan sweep at start.** When the runner starts it lists its token's
leases and deletes every one whose comment starts with `forgejo job ` —
at start the process runs nothing, so all of them are orphans of a
crashed or restarted predecessor (without the sweep they keep their
sandboxes until `LEASE_TTL` runs out). Each deletion is logged with the
lease id and label. The token's other leases are never touched, and a failed
sweep (backend unreachable) only delays it: the pool still starts, and
the next restart sweeps again.

**Graceful stop.** On SIGTERM or SIGINT (systemd's stop signal is
SIGTERM) the runner stops fetching new jobs, waits up to
`RUNNER_STOP_GRACE` (duration, default `10m`) for running jobs to
finish, then cancels the rest — each job is reported to Forgejo as
**cancelled**, its lease released by the executor's deferred delete —
and exits 0. `systemd` kills the unit when
its own stop timeout is reached, so the unit's `TimeoutStopSec` must be
**above** `RUNNER_STOP_GRACE` (the shipped `deploy/spoond-runner.service`
sets `TimeoutStopSec=660` for the 600 s default):

```ini
# /etc/systemd/system/spoond-runner.service
[Service]
Environment=RUNNER_STOP_GRACE=10m
TimeoutStopSec=660   # must exceed RUNNER_STOP_GRACE
```

If `RUNNER_STOP_GRACE` is raised, raise `TimeoutStopSec` with it; a
SIGKILLed runner leaves its job leases behind until the next runner
start's orphan sweep (or their TTL) releases them.

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
`hold_expires_at` and `hold_state`). A hold also lapses on its own: it
lasts `HOLD_TTL_SECS` from when it was set or renewed (renewal is `PUT
/api/leases/{id}/holder` with the same holder), at most
`HOLD_TTL_MAX_SECS` for an explicit `hold_ttl`. A lapsed hold
**suspends** a running lease and never releases one: the lease stays
held with no expiry (`hold_state` `lapsed`), so the TTL sweep never
touches it however long ago its own TTL passed; rule 2 releases it once
it has stayed suspended and untouched for its limit, and renewing
restores a normal hold.

**Nothing running is ever released automatically.** Rules 2 and 5
release only leases that a rule suspended (idle, pressure or a lapse)
and that saw no activity since; a held lease suspended by hand or by
the drain is never released by them.

| # | Rule | Variable | Default | Meaning |
|---|---|---|---|---|
| 1 | Idle suspend | `HELD_IDLE_TIMEOUT_SECS` | `14400` (4 h) | a held lease with no activity — what the idle sweep already counts: exec, stream, proxy, keepalive, guest heartbeat — for this long is **suspended** (memory and hugepages freed; nothing deleted; it resumes on next use, the SSH gateway does that on attach) |
| 2 | Stale release | `HELD_SUSPENDED_RELEASE_SECS` | `604800` (7 d) | a held lease suspended by rule 1, 3 or 4 and untouched since for this long is **released** (deleted); the GC reclaims its builds |
| 3 | Hold lapse | `HOLD_TTL_SECS`, `HOLD_TTL_MAX_SECS` | `604800` (7 d), `2592000` (30 d) | an unrenewed hold lapses: a running lease is **suspended** (never released), stays held with no expiry, and rule 2 takes it from there |
| 4 | Pressure | `PRESSURE_DISK_FREE_PCT`, `PRESSURE_HELD_IDLE_SECS` | `15`, `1800` (30 min) | when snapshot-disk free space is under the percentage, or free hugepages are short (admission would refuse a 1 GiB lease — no seeded image is smaller), rule 1 uses the shorter threshold |
| 5 | Critical disk | `CRITICAL_DISK_FREE_PCT`, `CRITICAL_DISK_RECOVER_PCT` | `5`, `10` | when snapshot-disk free space is under the critical percentage and `GC_DELETE=1`, held leases a rule suspended (1, 3 or 4), untouched since, are **released** oldest suspension first, at most one per sweep tick, until free space is above the recovery percentage; the GC runs first, at most every 5 minutes. A running lease is never released. With the dry-run GC the rule releases nothing, since nothing would be freed |
| 6 | Scheduling | — | — | the rules run in the existing sweep loop and skip while the node is draining |

Set `HELD_IDLE_TIMEOUT_SECS`, `HELD_SUSPENDED_RELEASE_SECS`,
`PRESSURE_HELD_IDLE_SECS`, `PRESSURE_DISK_FREE_PCT` or
`CRITICAL_DISK_FREE_PCT` to `0` to disable that rule. `HOLD_TTL_SECS`
and `HOLD_TTL_MAX_SECS` cannot be disabled: `0` means their default, so
every hold lapses eventually. Watch the rules with `journalctl -u spoond-backend | grep 'held
lease'` and `spoond_held_actions_total` — a rising `critical{release}`
means the disk needs attention the leases are paying for.

## Users & identity

- **Revoking access** = `DELETE /api/users/{id}` (or `ssh-key rm
  <user-id>`); the gateway treats the identity store as authoritative, so
  removal is immediate — no key-dir cleanup needed.
- **Quotas** are per-user (`max_leases`/`max_ttl` via `POST
  /api/users/{id}/quota`); over-cap creates and forks return `429`. A
  user with `max_leases: 0` is unlimited.
- **Memory quotas** (#128) are per-user too (`guaranteed_mib`/
  `max_mib`, same endpoint). A lease costs its image's `memory_mb`;
  the sum over a user's **running** leases may not pass `max_mib` — a
  suspended lease holds no hugepages and is not charged, so suspending
  frees the budget and resuming re-checks it (an over-budget resume
  answers `429` and the lease stays suspended). The same re-check runs
  before any operation that turns a suspended lease back into a running
  one: warm and cold restart, restore to a kept checkpoint, the crash
  reconcile and undrain. Resuming adds no lease, so only `max_mib`
  applies on those paths — a user at their `max_leases` cap can still
  resume their own suspended (or drained) lease, and an over-budget
  undrain leaves the lease drained (reported in `failed`) instead of
  losing it. Create, clone and fork (each child's `memory_mb`, reserved
  up front, all or nothing) are checked under the same lock as
  `max_leases`, so races cannot blow past either cap. `guaranteed_mib`
  is the user's floor of host memory; admission does not count it
  against them. Watch a user's charge as `used_mib` on
  `GET /api/users/me` (and `charged_mib` on the lease detail).
  - **Migration:** none, by design. A user with `max_leases > 0` and no
    `max_mib` keeps working unchanged — no count is converted into a
    memory number. To cap a user's memory, set it explicitly, sizing it
    from their current charge: `GET /api/users/me` (or `GET
    /api/users`) for `used_mib`, then `POST /api/users/{id}/quota` with
    `"max_mib": <MiB>` (and `"guaranteed_mib"` at most that). Setting a
    quota deletes nothing, but it bites on the next grant, resume,
    restart, restore, recovery or undrain — size `max_mib` before a
    drain; an undrain whose resume fails the check leaves the lease
    drained (reported in `failed`) instead of losing it.
- **Token/key hashes** are HMAC-SHA256 with a per-store salt (sidecar
  `<users-file>.salt`); back the salt up alongside the store or existing
  hashes become unverifiable on restore.
- **Forward-auth proxy** (`PROXY_AUTH_MODE=forward-auth`): the
  `PROXY_AUTH_SECRET` is shared with the IdP/Caddy; ensure Caddy strips
  inbound `X-Proxy-Auth`/`Remote-User` headers so guests can't spoof
  them, and keep `PROXY_AUTH_TRUSTED_PEERS` to the proxy's own CIDR.
- See [security.md](security.md) for the boundaries and [api.md](api.md)
  for the endpoints.

## Dashboard (`spoond dash`) and `spoond top`

`spoond dash` is a read-only, live view of spoond's present operation,
for watching rather than triage. It draws the whole frame as one
character grid at a fixed width — capacity (running/limit meter, leases
by state, queued, granted, swept, and one row per image with live
leases) beside the host meters (CPU, memory, hugepages, snapshot and
root disk) at a wide frame, stacked below it at a narrow one — then a
full-width throughput panel (running leases, requests per second,
creates per minute and egress connections, each with its current value
and a sparkline over the history, titled with the window the history
covers), live leases (id, image, owner, the run state — ▶ running,
‖ suspended, ■ lost, ⭘ recovered —, policy, age, time left and the
holder, ◆ when a hold is active and ◉ once it has lapsed; on the page
the holder is a link), the image catalog (shape, live leases, lifetime
uses, baked-at) beside the systemd units, a refusals-and-failures row
(auth, quota, throttled, capacity, build fails, lost leases — non-zero
counts highlighted — with the mean create and resume latencies), and
the events panel (the backend's lease event stream) — plus
an attention strip above the panels (one ▲ row per trigger, only when
something needs a person): a unit not active, a lost lease, free
hugepages or snapshot disk past the danger level, kept checkpoints past
`KEPT_DISK_WARN_PCT` of the snapshot disk (#126), the TLS certificate
inside 30 days of expiring, or an automatic held-lease action in the
last 24 h. The host panel's GC row also shows the kept total —
`kept N (X GiB)` when any build is pinned. A status line under the panels carries the headline numbers
and the clock. The browser page is the grid in a `<pre>` (Datastar
patching changed rows); `spoond top` draws the same grid with ANSI
styles in the terminal, at the terminal's width (COLUMNS, else 104),
redrawn every 2 seconds until interrupted. It runs as its own service on **:8893** behind
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
history are kept so a new page starts with trends. With
`DASH_EVENTS_TOKEN` set, the collector also holds one subscription to
the backend's lease event stream (`/api/leases/events`, resuming by
`Last-Event-ID` and backing off when the backend refuses it) and keeps
the last 50 events; the events panel shows the newest 5 — `HH:MM:SS`,
the type, the lease id and the holder (else the comment, else the
owner), lost and held-lease actions highlighted, releases dim. Without
the token the panel says `events need DASH_EVENTS_TOKEN` instead.
Configuration lives in `cmd/spoond-dash/dash.go`; the notable
variables:

| Variable | Default | Purpose |
|---|---|---|
| `DASH_ADDR` | `0.0.0.0:8893` | listen address |
| `DASH_USER`, `DASH_PASSWORD_HASH` | *(required)* | basic auth (`spoond dash hash PASS` makes the hash) |
| `DASH_TLS_CERT`, `DASH_TLS_KEY` | *(unset)* | serve HTTPS (set both or neither) |
| `METRICS_URL` | `https://127.0.0.1:8890/metrics` | spoond's `/metrics` |
| `METRICS_SERVER_NAME` | `spoond.example.com` | TLS server name for that URL |
| `METRICS_TOKEN` | *(required)* | the backend's scrape-only token |
| `DASH_EVENTS_TOKEN` | *(unset)* | the backend's events-only `EVENTS_TOKEN`; the lease events panel's source (unset: the panel says so) |
| `SPOOND_DB_PATH` | `/var/lib/spoond/spoond.db` | catalog database (opened read-only) |
| `USERS_FILE` | `/var/lib/spoond/users.json` | identity store (names only) |
| `E2B_TEMPLATE_STORAGE_PATH` | `/forkdcache/e2b/storage/templates` | disk to report |
| `DASH_SERVICES` | `spoond-backend,spoond-runner,spoond-sshd-gateway,e2b-orchestrator,e2b-guard,otelcol` | systemd units to show |
| `DASH_INTERVAL` | `2s` | refresh interval (minimum 1 s) |
| `DASH_HISTORY` | `150` | sparkline points kept (10–200) |
| `DASH_WIDTH` | `104` | frame width in cells (72–104) |
| `DASH_HOST` | *(the hostname)* | header label |

The dashboard can only read: it has no write path to the backend, the
database or the orchestrator, and the tokens it holds are refused
everywhere except their own routes — `METRICS_TOKEN` on `/metrics`,
`DASH_EVENTS_TOKEN` on `GET /api/leases/events`.

### Setting up the events panel

The backend's `EVENTS_TOKEN` and the dashboard's `DASH_EVENTS_TOKEN`
are two names for one secret: generate it once, put it in
`/etc/spoond/backend.env` as `EVENTS_TOKEN` and in the dashboard's
environment (the `dash.env` file the spoond-dash unit reads) as
`DASH_EVENTS_TOKEN`, then restart both units. On the backend it is
admitted on `GET /api/leases/events` alone — every owner's events, and
nothing else; on the dashboard it turns the events panel on. Generate
it like the other tokens:

```bash
EVENTS_TOKEN=$(openssl rand -hex 32)
printf 'EVENTS_TOKEN=%s\n' "$EVENTS_TOKEN" >> /etc/spoond/backend.env
printf 'DASH_EVENTS_TOKEN=%s\n' "$EVENTS_TOKEN" >> /etc/spoond/dash.env
chmod 600 /etc/spoond/backend.env /etc/spoond/dash.env
systemctl restart spoond-backend spoond-dash
```

Rolling it out to a running deployment needs both sides at once (the
panel reads the same secret the backend checks), and a new deployment
sets it alongside `METRICS_TOKEN` when writing the env files — see
[install.md](install.md).

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
| `spoond_checkpoint_pause_seconds` | how long each checkpoint froze its guest (2.3, #122) |
| `spoond_snapshot_bytes{kind}` | build disk per kind |
| `spoond_storage_free_bytes` | free bytes at the build store |
| `spoond_gc_deleted_total{kind}` | builds deleted by the GC |
| `spoond_kept_builds` | kept checkpoints of live leases (pins; #126) |
| `spoond_kept_builds_bytes` | disk bytes held by kept checkpoints of live leases (recorded `size_bytes`; #126) |
| `spoond_held_actions_total{rule,action}` | automatic actions on held leases: `rule` is `idle`, `stale`, `expiry`, `pressure` or `critical`; `action` is `suspend_idle`, `suspend_lapsed`, `release` or `expire` |
| `spoond_guest_dials_active` | open guest port dials (WebSocket→guest TCP bridges) |
| `spoond_guest_dials_total{result}` | guest port dial attempts: `ok`, `refused` (the per-owner 16-dial cap) or `error` (the guest dial failed) |
| `spoond_capacity_rejections_total` | admission refusals |
| `spoond_store_errors_total{op}` | SQLite write failures |
| `spoond_notifications_total{webhook,severity,result}` | webhook notification delivery outcomes; `webhook` is the receiver's index in `NOTIFY_WEBHOOKS` (never its URL — the URL may carry secrets), `severity` is the message's grade, `result` is `sent`, `retry`, `dropped`, `deduped` or `rate_limited` |
| `spoond_backend_start_time_seconds` | Unix time the backend process started; the dashboard header shows its uptime from it |

`spoond_leases{state="lost"}` above zero means an orchestrator crash
happened — it is the number the soak watch uses. Deploying
`deploy/e2b/spoond-soak.{service,timer}` (with `soak-check.sh` from
`deploy/e2b/` in `/usr/local/lib/spoond/`) automates that watch: once a
day it sources the backend env, runs `spoond doctor`, reads the lease
states and the runner's job results of the last 24 h read-only from
SQLite, and appends one JSON line to `/var/lib/spoond/soak.log`. The
unit fails (so `systemctl --failed` shows it) when doctor reports a
FAIL or any lease is `lost`.
