# Operations

Runbook for operating a spoond deployment on the E2B substrate: health,
`spoond doctor`, backups, snapshot GC, the drain protocol, crash
recovery, restarting the orchestrator or the backend, and the dashboard.
What the substrate is and why it behaves this way is
[substrate.md](substrate.md).

> Deploying the 2.7 generic-defaults change on the production host: the
> exact settings vm2 must carry before the swap are listed in
> [`deploy/PRODUCTION-ENV-2.7.md`](../deploy/PRODUCTION-ENV-2.7.md).

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
    url: https://spoond.example.com:8890/readyz
    interval: 30s
    conditions:
      - "[STATUS] == 200"
      - "[RESPONSE_TIME] < 2000"        # milliseconds; /readyz bounds every check to 2 s
      - "[CERTIFICATE_EXPIRATION] > 72h"

  - name: dashboard-readyz
    url: https://dash.example.com:8893/readyz
    interval: 30s
    conditions:
      - "[STATUS] == 200"
      - "[RESPONSE_TIME] < 2000"
      - "[CERTIFICATE_EXPIRATION] > 72h" # needs DASH_TLS_CERT set, else drop this condition

  - name: ssh-gateway
    url: tcp://sandbox.example.com:2222       # a TCP connect proves the listener answers
    interval: 30s
    conditions:
      - "[CONNECTED] == true"
      - "[RESPONSE_TIME] < 2000"
```

`[CERTIFICATE_EXPIRATION] > 72h` pages while there is still time to
renew — ahead of the 30-day expiry the dashboard's own certificate
counter shows. If the gateway fronts a TLS listener of its own, give it the
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
| `lease API: listener` + `/healthz` | the backend listener is up and healthy. The `/healthz` probe over TLS trusts the backend's own cert chain loaded from `TLS_CERT` and picks the hostname from the cert's DNS SAN (a wildcard bind will not validate) — a separate `lease API: TLS trust` check FAILs when the cert cannot be read, and verification is never skipped. With several pairs it probes with the first. `tls: <file>` checks each pair: loadable, and WARN within 14 days of expiry |
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

After the catalog candidates each pass also reaps **orphan build
directories**: directories under `E2B_TEMPLATE_STORAGE_PATH` that the
catalog never sees, either because spoond marked the build `deleted` at
creation while the orchestrator finished writing its memory file seconds
later (so the directory reappeared after the delete), or because the
directory was never recorded at all (an image build's intermediate
layers, or a build whose catalog insert failed). Only direct child
directories whose name parses as a UUID are considered, and symlinks are
never followed.

A directory is **needed**, and never touched, when it is:

- a catalog build that is not `deleted`;
- named by any lease (`resume_build_id`, `last_checkpoint_build_id`), any
  kept build (`lease_kept_builds`), any sandbox row or any image's
  current build;
- reachable from a needed build through its `memfile.header` /
  `rootfs.ext4.header`, transitively (a header records the build ids it
  maps pages from as 16 raw UUID bytes);
- modified within the last `ORPHAN_MIN_AGE_SECS` (default `3600`), so a
  directory in use or still being written is spared.

Everything else is an orphan. `ORPHAN_REAP` selects what happens to one:

- `dryrun` (the default) logs
  `gc: would reap orphan <id> (<size>)` and changes nothing;
- `quarantine` moves the directory to
  `<storage path>/../quarantine/<id>`, dropping a `.spoond-quarantine`
  marker that dates the move; every later pass moves a quarantined
  directory back if a catalog build, lease, kept build, sandbox or image
  needs it again, and deletes it only once it has sat there for
  `ORPHAN_QUARANTINE_SECS` (default `86400`) — so a misclassification is
  recoverable for a day, across backend restarts;
- `off` disables the reap entirely.

`GC_DELETE` does not control the orphan reap. When a quarantined
orphan is finally deleted, it is removed with `os.RemoveAll`, and the
space counts into `spoond_gc_orphans_reaped_total`,
`spoond_gc_orphan_bytes_reaped_total` and the pass's `gc` event. The
reap is refused (logged and skipped, never failing the pass) when the
catalog read fails or names no needed builds at all.

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
`40`) percent of the snapshot disk, the dashboard's Notifications panel
says so and the notifier raises `disk.kept` (warn) — the held-lease
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
   the drain continues. Drain pauses write snapshots through their own
   process-wide limiter, `DRAIN_SNAPSHOT_CONCURRENCY` (default `2`),
   separate from the default limiter `SNAPSHOT_WRITE_CONCURRENCY`
   (default `1`) that paces every other snapshot write — see
   [Snapshot write pacing](#snapshot-write-pacing).
3. The orchestrator stops; on start, `ExecStartPost=/opt/spoond/spoond
   drain --start` waits for the node (up to 120 s), calls
   `POST /api/admin/undrain`, which clears draining and resumes exactly
   the drained leases. Resumes run `UNDRAIN_CONCURRENCY` (default `2`)
   at a time, so restoring a batch of large memory snapshots does not
   stack the node's I/O and memory. A resume that fails with a
   retryable envd/start error ("syncing took too long", a context
   deadline, envd init) is retried `UNDRAIN_RESUME_RETRIES` (default
   `2`) times with a short backoff before the lease becomes `lost`; the
   response's `failed` entry and the log line name how many attempts
   were made. A lease that fails permanently (its build is gone, or the
   node keeps refusing the resume) still becomes `lost`; one over its
   owner's memory cap, without burst room, or unable to preempt stays
   `drained` for a later undrain.
4. If systemd's `SERVICE_RESULT` is not `success` (the orchestrator
   crashed or was killed), the drain is skipped — there is nothing to
   pause — and the backend's crash reconcile handles recovery.

The unit's `TimeoutStopSec` must cover the drain: the pause phase takes
roughly `leases × per-pause time / DRAIN_SNAPSHOT_CONCURRENCY`, plus the
up-to-180 s quiesce wait. The shipped unit's `TimeoutStopSec=330` is
sized for the default width on this node; raising the lease count, the
per-pause time (larger guests) or the default width's ratio needs the
unit's timeout raised to match.

## Snapshot write pacing

A memory snapshot (`Pause` or `Checkpoint`) saturates the host's disk
while it writes, and the orchestrator's NBD server must still answer
every guest's rootfs requests inside the kernel ceiling. On 2026-10-06
on vm2 a burst of snapshot writes stacked up, the NBD server missed
that deadline, and every guest on the stalled devices lost its root
disk (permanent EIO). spoond therefore runs every snapshot write
through one process-wide limiter:

- `SNAPSHOT_WRITE_CONCURRENCY` (default `1`) is its width. Every pause
  and checkpoint — a hand suspend, the idle sweep, the held-lease
  idle/pressure rules, preemption, restart's pause leg, a drain pause
  (which has its own width), and every checkpoint (on demand, periodic,
  clone, fork, keep) — waits its turn. `0` means unlimited, the
  pre-fix behaviour.
- `DRAIN_SNAPSHOT_CONCURRENCY` (default `2`) is the drain's own width,
  used only for the pauses `POST /api/admin/drain` issues, so a planned
  restart can pause a batch of leases inside the unit's stop window.

Waiting is bounded by the caller's context: an API caller that waits
longer than the limiter only sees added latency, never a different
answer. A write that waited 5 s or more logs one line naming the wait
and the backlog (`snapshot write waited 41s behind 1 other`). The
gauges `spoond_snapshot_writes_in_flight` and
`spoond_snapshot_write_wait_seconds` show the pressure. The idle sweep
and the held-lease rules suspend at most one lease per tick while the
limiter is busy, skipping the rest to retry on the next tick rather
than queueing a batch.

## Bounded substrate calls

Every orchestrator gRPC call runs under its own timeout, so a hung
orchestrator cannot wedge a background loop, a lease's busy flag or the
snapshot limiter (spoond-j3a). The defaults are create/resume, pause and
checkpoint `5m`; delete `2m`; `NodeInfo` `15s`; list/update/drain-override
and template builds `30s` — set `E2B_CREATE_TIMEOUT`, `E2B_PAUSE_TIMEOUT`,
`E2B_CHECKPOINT_TIMEOUT`, `E2B_DELETE_TIMEOUT`, `E2B_NODEINFO_TIMEOUT`
and `E2B_CONTROL_TIMEOUT` with a Go duration (`90s`, `5m`) or seconds to
change them. Exec keeps its own request timeout. The client also sends
HTTP/2 keepalive pings every 5 minutes (the gRPC server's default
minimum) with a 20 s ack timeout. On the service
side each sweep stage (TTL release, held rules, pool refill, job prune)
is bounded by `SWEEP_TIMEOUT` (default `15m`): a stage that overruns is
logged and abandoned, the lease's `busy` flag clears through its deferred
release, and the next tick runs.

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

Recovery is retried, not given up on at the first error: a `recoverFromCheckpoint`
failure keeps the lease live with no sandbox and the next reconcile pass tries
it again. Recovery retries anything that is not permanent — deliberately
the inverse of `resumeRetryable` — so a busy node's envd start, a
deadline or any other retryable error counts against
`RECOVERY_RETRY_ATTEMPTS` (default 3) and is bounded by
`RECOVERY_RETRY_WINDOW` (default 30m) since the first failure. The only
recovery failure that waits for room instead of counting is a substrate
`ErrCapacity` (the node is full or draining); admission refusals (over
quota, under the burst reserve, no preemption room) cannot reach a live
lease's recovery, which skips admission, so they are not in this path. A
missing checkpoint build or image is permanent and loses the lease at
once. Every transient failure emits a `recovery_retry` event naming the
attempt and the cause, and `GET /api/leases/{id}` exposes the pending
retry as `recovery: {attempt, of, since}`. When the budget is spent the
lease is marked `lost` with a reason naming the attempts and the error,
and a `lost` event is emitted.

Preemption's resume queue (`resumePreempted`, every 15 s) is bounded
differently: a preempted lease whose resume keeps failing with a
non-admission error gets `PREEMPT_RESUME_RETRIES` (default 3) attempts
before it is marked `lost` with the reason and a `lost` event. An
admission/capacity refusal is not a failure — the preemption parked the
lease to free the very room it now waits for — so it neither counts nor
starts/extends the window and the lease waits for room indefinitely,
resuming when room appears. So a permanently failing resume cannot
create a new orchestrator sandbox every 15 s for ever, while a lease
merely waiting for capacity is never lost.

The recovery budget is keyed by the sandbox that failed and dropped
whenever the lease gets a new sandbox (restart, restore, resume), on
recovery success, loss and release, so a stale budget can never make the
next reconcile roll a healthy lease back to an old checkpoint. A
recovery or preemption loss that races a release leaves the released
lease alone: no resurrection, no late `lost` event.

To the API and the gateway, `recovered` behaves exactly like `running`
(`state` keeps showing it until the lease is suspended or restarted),
while `lost` answers `410` with `code: lease_lost`, the stored reason
and the `DELETE` that frees the quota, on exec, stream, proxy and SSH
(see [api.md](api.md#lost-leases)). `POST
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
kept bytes alone start drawing attention: the dashboard's Notifications
panel and the notifier's `disk.kept` both read it.

### Crash test

`CRASH_TEST=1` (or `true`) in the backend's environment enables `POST
/api/leases/{id}/crash-test` ([api.md](api.md)), which crashes one lease
and runs it through the recovery above on demand: from its checkpoint
or `lost`, with a `crash_test` event first. It is off by default, and
the route then answers `404` like an unknown route; set it only on hosts
that run crash suites. It affects only the caller's own leases (an admin
may crash any lease), and nothing else: no other lease, no pool, no
release. Each run logs `crash-test: lease <id> crashed by <caller id>`.

### Rootfs liveness probe

The kernel NBD connections backing a guest's root disk can die when the
host disk stalls past the kernel ceiling (incident 2026-10-06): the
guest then answers I/O errors on every uncached read, execs return HTTP
`500` `exec failed`, and the lease otherwise stays `running` forever.
`ROOTFS_PROBE_SECS` (default `120`, `0` disables) makes the backend
catch that: every interval it runs one cheap exec per running lease that
reads a single 4096-byte block of the guest's root block device at a
pseudo-random offset with `O_DIRECT` (`iflag=direct`), so the page cache
cannot answer the read. The offset is drawn from `/dev/urandom` rather
than `$RANDOM`, which dash does not provide. The script takes the device
from `findmnt -no SOURCE /`, falling back to `/dev/vda` when that is not
a block device.

A probe is a failure when it answers an `Input/output error` or when
the exec itself fails at the transport. A probe that hits the 10 s
timeout (the substrate kills the hung exec and reports it the way the
e2b backend does: exit `124` with a `timed out` line) is a slow disk,
not a dead one: it is logged and counted in
`spoond_rootfs_probe_failures_total`, but it never counts toward
recovery. Since e2b-runtime P7 the kernel lets a stalled NBD request
wait up to 360 s instead of failing it, and recovering a lease whose
disk is only slow would discard its work since the last checkpoint; a
disk that really dies past that ceiling answers `EIO`. A non-zero exit for any
other reason is not a failure (the guest answered); the backend logs it
so a probe that silently degraded to a no-op — a base image without
`dd`, or a root device that rejects `O_DIRECT` — is visible. **Three
consecutive
failures** treat the sandbox as crashed:
the backend logs the lease, emits a `lost` event with detail `root disk
unreadable (I/O errors)`, deletes the dead sandbox through the substrate
and runs the same per-lease recovery as the crash reconcile — from the
last checkpoint (generation +1, state `recovered`) or `lost` when there
is none. A success in between resets the count. A lease with a
successful exec (exit `0`) in the last `ROOTFS_PROBE_SECS` is skipped
(it has
already proven it is alive), as are busy leases (checkpoint, restart,
suspend in flight). The admin drain and a probe-triggered recovery are
mutually exclusive: the drain's `SetDraining` waits for a recovery in
flight, and a recovery that reaches the drain waits for it and then
stands down, so no sandbox is deleted or recovered during the drain.

If **every** lease's probe fails at the transport in one pass, the
orchestrator is unreachable, not the guests: the pass logs once and
changes nothing. (On a host with a single running lease, an
all-transport pass is indistinguishable from that case, so a lone guest
whose agent died is left to the crash reconcile rather than marked
`lost` by the probe.) A probe that answers an I/O error or a timeout
proves
the orchestrator is reachable, so a mixed pass still recovers the
affected leases. The counters are `spoond_rootfs_probe_failures_total`
(failures, by probe) and `spoond_rootfs_dead_total` (leases declared
dead). `ROOTFS_PROBE_SECS=0` disables the probe entirely.

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

**Burst leases keep a reserve** (#128 part 2). Every lease is admitted
`guaranteed` or `burst` (see Users & identity below): guaranteed when
the owner's running charge with the lease stays within their
`guaranteed_mib`, burst above it or when the request forces it. A burst
lease is admitted only while the node's free hugepages (the same
`spoond_node_hugepages_free_bytes` source, cached at most 15 s) stay
above the reserve after its own — so guaranteed work and crash recovery
always have room to land even when burst tenants fill the node. The
reserve is `BURST_RESERVE_MIB`, default `8192` (8 GiB); `0` disables
it. A burst lease refused on the reserve answers `503` `no burst
capacity` with `Retry-After: 30` and the lease stays as it was; the
reserve frees as guaranteed work suspends. Size it from your crash
recovery headroom: everything you want a recovered lease to be able to
resume into, minus what guaranteed tenants are entitled to.

**Preemption reclaims burst capacity by suspending** (#128 part 3). When
a guaranteed admission (create, fork, clone, resume, warm or cold
restart, restore, crash recovery, undrain) cannot get its hugepages,
spoond suspends burst leases through the usual pause path — memory
continues on resume, so the generation does not change — in order of
lowest `priority`, then newest, then the owner furthest over their
`guaranteed_mib`. It stops as soon as enough memory is free and admits
the guaranteed lease, and it serialises preemption, so two guaranteed
creates cannot each preempt for themselves. Each preempted lease is
marked `preempted` and emits a `preempted` event naming the guaranteed
lease's owner; `spoond_preemptions_total` counts them and
`spoond_preempted_leases` is the current gauge.

The pause writes a snapshot, so preemption stops before the snapshot
disk does: `PREEMPT_DISK_FLOOR_PCT` (default `15`) is the free
percentage a pause must leave, estimated from the burst lease's
`memory_mb`. The floor cannot be turned off: `0` or a negative value
means the default. If preemption can free enough memory only by pausing
leases the floor blocks, the guaranteed admission answers `503`
`capacity: cannot preempt (snapshot disk low)` with `Retry-After: 30`
and suspends no one. If the node cannot host the lease even after
pausing every candidate, preemption suspends no one and the admission
falls through to the ordinary capacity check.

The **resume queue** runs every 15 s: it resumes preempted leases,
oldest preemption first, whenever they fit again — host hugepages above
the reserve and the owner within `max_mib` — through the normal
admission path, as a burst lease again if the owner is still above the
guarantee. On resume `preempted` is cleared and a `resumed` event is
emitted with detail `after preemption`. A preemption is therefore
temporary: clients (Honey included) should treat a `preempted` lease as
waiting rather than gone, and simply wait for its `resumed` event or
poll the lease — deleting and recreating it throws away the paused
work. A client's own `resume` of a preempted lease takes the same path
and answers `503` while capacity is still short.

**Queued admission** (#129 part 1): a create can wait for room instead
of failing, by sending `"wait": N` (seconds) on `POST /api/leases` (see
[api.md](api.md#queued-admission)). `MAX_ADMIT_WAIT_SECS` caps the wait
(default `900`; `0` disables waiting — the request field is accepted and
ignored). The queue lives in the backend process and is lost on restart.
Waiting creates are served in fair-share order: the owner furthest under
their `guaranteed_mib` first (an owner with no `guaranteed_mib`, or
already at it, ranks after every owner with headroom), then FIFO; each
wake-up admits every queued create that fits, so a smaller one may pass
a larger one. The queue is retried whenever capacity may have freed (a
lease released, suspended, preempted-and-resumed, or a quota changed)
and on a 5 s tick. The metrics: `spoond_leases_queued` (current queue
depth), `spoond_leases_queued_oldest_seconds` (the oldest ticket's age),
`spoond_admit_wait_seconds` (histogram of the wait of admitted creates)
and `spoond_admit_timeouts_total` (waits that ended without a lease —
timeout, client gone or drain). The dashboard's capacity panel shows
this as `queued N (oldest Ms)`.

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
| `503 capacity: cannot preempt (snapshot disk low)` | a guaranteed lease needed hugepages, but pausing a burst lease would take the snapshot disk under `PREEMPT_DISK_FLOOR_PCT` | free snapshot disk (run the catalog GC, delete old snapshots) or lower `PREEMPT_DISK_FLOOR_PCT`; retry after `Retry-After` |
| `503 draining` on a create with `"wait"` | the admin drain started while the create was queued; the drain answers every queued create at once | retry after `undrain` |
| lease shows `preempted` / `‖ preempted` on the dashboard | a guaranteed admission suspended a burst lease to reclaim memory; the resume queue will restore it | wait for the lease's `resumed` event (`after preemption`) or poll it; do not delete and recreate |
| `410 lease_lost` (`code: lease_lost`) | the lease's sandbox died with no checkpoint (or its recovery failed); the message names the reason | `DELETE` the lease to free its quota; nothing to resume |
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
| 1 | Idle suspend | `HELD_IDLE_TIMEOUT_SECS` | `14400` (4 h) | a held lease with no activity — what the idle sweep already counts: exec, stream, proxy, keepalive, guest heartbeat, files, guest dial — for this long is **suspended** (memory and hugepages freed; nothing deleted; it resumes on next use, the SSH gateway does that on attach). A lease whose own effective `idle_suspend` is `> 0` is reclaimed by the idle sweep on that value instead and is skipped by rule 1 (and by rule 4's shortening); see [Idle reclamation](#idle-reclamation) |
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

## Background exec jobs

`POST /api/leases/{id}/exec` with `"background": true` (2.6, #135)
starts a tracked job in the lease instead of holding the request open.
The guest records its outcome under `/var/lib/spoond/jobs/<job_id>/`
inside the lease: `stdout`, `stderr`, `pid` and — written atomically
when the command ends — `rc`. Those files are the source of truth, so a
backend restart or a broken envd stream does not lose an outcome: while
the backend holds the stream it notices the exit at once, and otherwise
a reconcile pass (every 10 s while any job runs) reads `rc` through the
files path. The files are kept until the exited record is pruned (after
`JOB_RETENTION_SECS`), when the sweeper removes the job directory too;
the job record outlives the files only within that window.

A running job keeps its lease out of every idle rule — the plain
`IDLE_TIMEOUT_SECS` sweep, held rule 1 and idle suspension — so nothing
suspends a lease mid-job. A lease that does suspend normally with a job
running leaves the job record `running` (the memory continues; reconcile
after resume). When the guest's memory does **not** continue — a cold
restart, a restore or crash recovery, i.e. a generation bump — every
running job is marked `lost`.

The per-exec `secrets` stay staged under `/run/secrets` for the job's
whole life; the guest wrapper removes them at exit and the backend also
removes them on reconcile. Neither `env` nor secret values are ever
stored in the job record, logged, emitted in an event or written to the
guest's job directory — `env` rides the substrate's start request — and
`cmd` is stored as given.

| Variable | Default | Meaning |
|---|---|---|
| `MAX_RUNNING_JOBS_PER_LEASE` | `16` | running background jobs per lease; past it a start answers `429` |
| `JOB_RETENTION_SECS` | `604800` (7 d) | exited job records older than this are pruned by the sweeper (running and lost records are kept) |

The job record lives in the `lease_jobs` table (migration 0016) and is
deleted with its lease. Metrics: `spoond_jobs_running` (gauge) and
`spoond_jobs_exited_total{result}` (`ok`/`error`/`lost`). Events:
`job_started`, `job_exited`, `job_lost`; the dashboard events panel
shows `job_exited` lines, non-zero exits in the warning colour. The
endpoints, the events and the lease's `jobs` summary are in
[api.md](api.md).

## Idle reclamation

Persistent leases can be suspended after a period without activity,
freeing their hugepages and disk-backed memory while keeping everything
for the next use (the guest's memory continues on resume, so the
generation does not change). Two mechanisms share the job:

- **the plain sweep** (`IDLE_TIMEOUT_SECS`) and **held rule 1**
  (`HELD_IDLE_TIMEOUT_SECS`, shortened under pressure by rule 4) apply to
  leases whose effective `idle_suspend` is `0` — today's behaviour;
- **the idle sweep** (`#129` part 2) applies to a lease whose effective
  `idle_suspend` is `> 0`: the lease's own value, or the host default
  `IDLE_SUSPEND_DEFAULT_SECS` when it has none. `POST /api/leases` and
  `PUT /api/leases/{id}/idle-policy` set it (`0` = never, `60`–`604800`
  seconds). A non-zero value needs a persistent lease.

A lease with a non-zero `idle_suspend` is reclaimed on that value alone;
the plain sweep and rule 1 skip it. Activity is what the sweeps already
count — exec, stream, proxy, keepalive, guest heartbeat, the files API
and guest port dial — and the threshold is measured from `LastActive`.
The sweep suspends through the normal pause path and shares preemption's
snapshot-disk floor (`PREEMPT_DISK_FLOOR_PCT`): a pause that would take
the disk under the floor is skipped for that pass and retried on the
next one. An idle suspension marks the lease `last_action
idle_suspend/suspend_idle`, emits an `idle_suspended` event and counts
in `spoond_idle_suspends_total`. Because it is a rule suspension, rules
2 and 5 may later release the lease if it stays idle-suspended and
untouched — a preempted lease stays excluded, and nothing running is
ever released.

The **next call resumes it**: exec, stream, files and guest port dial on
an `idle_suspend`-suspended lease resume it first through the normal
resume path (admission, class and quota apply) and then serve the call;
a refused resume answers what resume would (`429` over quota, `503` with
`Retry-After` for capacity or the burst reserve) and the lease stays
suspended. Any other suspension keeps answering `409 lease is suspended;
resume it first`; an explicit `resume` (and the SSH gateway's resume on
attach) works as always. `IDLE_TIMEOUT_SECS` remains the legacy host-wide
knob — new deployments should set `IDLE_SUSPEND_DEFAULT_SECS` and the
per-lease value instead. Watch idle suspensions with `journalctl -u
spoond-backend | grep idle_suspend` and `spoond_idle_suspends_total`.

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
- **Lease classes** (#128 part 2): every lease is `guaranteed` or
  `burst`, decided at admission and stored with it (dashboard: `▶
  running, burst` and `burst N` in the capacity panel). A lease past the
  owner's `guaranteed_mib` bursts, as does one created with
  `"burst": true`; a user with no `guaranteed_mib` keeps every lease
  guaranteed. A burst lease is preemptible even within another user's
  guarantee and is held to the node's burst reserve — `BURST_RESERVE_MIB`,
  default `8192` (see Capacity and the warm pool above): refused with
  `503` `no burst capacity` (`Retry-After: 30`) while free hugepages
  would dip under it. Every re-admission re-decides the class — a
  guarantee-burst lease can fall back to guaranteed when the charge has
  room, a request-burst one stays burst — and an undrain whose burst
  lease cannot fit the reserve leaves it drained for a retry, like the
  over-quota case above.
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
leases) beside the host meters (CPU, the two disk I/O meters directly
under it — `i/o stall` from PSI and `<dev> busy` from the snapshot
device — then memory, hugepages, snapshot and root disk) at a
wide frame, stacked below it at a narrow one — then a
full-width throughput panel (running leases, requests per second,
creates per minute and egress connections, each with its current value
and a sparkline over the history, titled with the window the history
covers), live leases (id, image, owner, the run state — ▶ running,
‖ suspended, ■ lost, ⭘ recovered —, the access policy (`isolated` when
the API's `network_policy` is `none`, else `lan`/`restricted`/
`internet`), age, time left and the holder, ◆ when a hold is active and
◉ once it has lapsed; on the page the holder is a link; the table's
fixed columns are sized to the values actually shown, so short states
leave no blank run and the freed width goes to the owner then the
holder, and the ◆ held · ◉ lapsed legend sits on one dim line under the
table when a row carries a hold), the image catalog (shape, live leases, lifetime
uses, baked-at) beside the systemd units, a refusals-and-failures row
(auth, quota, throttled, capacity, build fails, lost leases — non-zero
counts highlighted — with the mean create and resume latencies), and
the events panel (the backend's lease event stream) — plus
a **Notifications** panel below the header (only when there is a
message): spoond's own system messages, one row each — a unit not
active, free hugepages or snapshot disk past the danger level, kept
checkpoints past `KEPT_DISK_WARN_PCT` of the snapshot disk (#126), or
the snapshot disk's I/O full pressure past `DASH_IO_FULL_BAD_PCT`
(`disk i/o stalled: full pressure N% over 60 s`, cleared when the
pressure drops). Each
message has a stable id from its trigger, a severity (warn/bad) and a
`×` the viewer can dismiss for their own browser (`localStorage`, no
server state; a dismissed message stays hidden while its trigger stays
active and returns if the trigger clears and fires again). Per-lease
trouble is not a dashboard message: the viewer cannot act on a lease,
so spoond tells the lease's initiator itself (the lease event stream
and the API's `410 lease_lost` with `lost_reason`). TLS certificate expiry is
left to the host's own monitoring (see the Gatus example above). The host panel's GC row also shows the kept total —
`kept N (X GiB)` when any build is pinned. The header draws
`SPOOND · <host>` at the left margin and right-aligns spoond's uptime
and the frame's clock as `up <dur>, <time>` (the uptime drops before the
clock on a narrow frame). The footer is one dim, centred line —
`Spoond v2.7.1 (2026-10-07) · GitHub` — with the dashboard binary's
version (`debug.ReadBuildInfo`, shortened like the header used to), its
release date (`vcs.time` as `YYYY-MM-DD`, omitted for a dev build) and
the GitHub mark linking to `DASH_PROJECT_URL`; the URL text is no
longer shown. On a narrow frame the date drops first, keeping the
version and the mark. The mark is the standard GitHub octocat inline
SVG (16px, `currentColor`) in the browser and the dim word `GitHub` in
the terminal. The
browser page
is the grid in a `<pre>` (Datastar
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
history are kept so a new page starts with trends. The host I/O readout
comes from `/proc/pressure/io` (PSI: `some` and `full` over 60 s, not
`iowait`, which drops when CPUs are busy even if the disk is saturated)
and `/proc/diskstats` (the snapshot device's write MB/s and busy share,
a delta between collections). The i/o stall meter's value is the full
60 s average, shown as `0.4% full` (the 60 s `some` average stays in
the metrics, not on the meter); the busy meter's value text is
`<busy>% · <N> MB/s w`, its label the device name (`nvme0n1 busy`)
when that fits the meter label column, else `disk busy`. A kernel
without PSI (no `/proc/pressure`) simply hides the stall meter;
a device the collector could not resolve hides the busy meter only.
`DASH_DISK_DEVICE` names the block device to watch, defaulting to the
one the storage path's mount sits on. With
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
| `DASH_TLS_CERT`, `DASH_TLS_KEY` | *(unset)* | serve HTTPS (set both or neither); comma-separated lists serve several certificates by SNI and reload on change, as `TLS_CERT`/`TLS_KEY` do |
| `METRICS_URL` | `https://127.0.0.1:8890/metrics` | spoond's `/metrics` |
| `METRICS_SERVER_NAME` | *(METRICS_URL host)* | TLS server name for that URL |
| `METRICS_TOKEN` | *(required)* | the backend's scrape-only token |
| `DASH_EVENTS_TOKEN` | *(unset)* | the backend's events-only `EVENTS_TOKEN`; the lease events panel's source (unset: the panel says so) |
| `SPOOND_DB_PATH` | `/var/lib/spoond/spoond.db` | catalog database (opened read-only) |
| `USERS_FILE` | `/var/lib/spoond/users.json` | identity store (names only) |
| `E2B_TEMPLATE_STORAGE_PATH` | `/forkdcache/e2b/storage/templates` | disk to report |
| `DASH_DISK_DEVICE` | *(auto from the storage mount)* | block device for the write-throughput and busy readout |
| `DASH_IO_FULL_WARN_PCT` | `5` | PSI full avg60 at which the i/o stall meter turns warn |
| `DASH_IO_FULL_BAD_PCT` | `15` | PSI full avg60 at which it turns bad and the notification fires |
| `DASH_SERVICES` | `spoond-backend,spoond-runner,spoond-sshd-gateway,e2b-orchestrator,e2b-guard,otelcol,spoond-netwatch` | systemd units to show |
| `DASH_INTERVAL` | `2s` | refresh interval (minimum 1 s) |
| `DASH_HISTORY` | `150` | sparkline points kept (10–200) |
| `DASH_WIDTH` | `104` | frame width in cells (72–104) |
| `DASH_HOST` | *(the hostname)* | header label |
| `DASH_PROJECT_URL` | `github.com/jrimmer/spoond` | the footer's GitHub mark links here |

The I/O thresholds are a first cut, to be tuned from #136's
measurements. `iowait` is deliberately not used: it is CPU idle time
with I/O outstanding, so it falls when the CPUs are busy even while the
disk is saturated — PSI's `full` line measures the stall itself.

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
| `spoond_jobs_running` | background exec jobs currently running (2.6, #135) |
| `spoond_jobs_exited_total{result}` | background exec jobs that ended: `ok` (exit 0), `error` (non-zero exit) or `lost` (the guest did not continue, #135) |
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
