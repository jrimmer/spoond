# Changelog

Notable changes to spoond, newest first. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). In the 2.0
section, every entry names where it comes from: a commit (`<short-hash>`),
a unit of the E2B substrate spec (`U01`–`U13`, under
`docs/plans/2026-09-30-e2b-substrate/`), or a fixed decision (`D1`–`D17`,
in that spec's `00-README.md`). The earlier-releases section is
summarised from README "Status".

## [Unreleased]

### Added

- **Memory quotas (#128, part 1).** A user's quota gains
  `guaranteed_mib` and `max_mib` (set via `POST /api/users/{id}/quota`,
  admin only; `0` = unset, `guaranteed_mib` must not exceed `max_mib`).
  A lease costs its image's `memory_mb`, and the sum over a user's
  **running** leases may not pass `max_mib` — a suspended lease holds
  no hugepages and is not charged, so suspending frees the budget and
  resuming re-checks it. Every path that turns a suspended lease back
  into a running one re-passes the check — resume, warm and cold
  restart, restore, crash recovery, undrain — and those paths check
  memory only: a user at their `max_leases` cap can still resume their
  own suspended (or drained) lease. Create, clone and fork (each
  child's `memory_mb`, reserved up front, all or nothing) and resume
  answer `429` naming the memory limit when over; the reservation is
  atomic with the lease count under the store lock, so racing creates
  cannot blow past either cap. `GET /api/users/me` reports the charge as
  `used_mib` beside `guaranteed_mib`/`max_mib`; `GET /api/leases/{id}`
  shows the owner's `charged_mib`/`guaranteed_mib`/`max_mib`.
  **Migration:** none — a user with `max_leases > 0` and no `max_mib`
  keeps working unchanged; set `max_mib` explicitly per user (see
  docs/operations.md, "Memory quotas").
- **Guaranteed and burst leases (#128, part 2).** Every lease is
  admitted as `guaranteed` or `burst`, decided at admission and
  persisted with it (**migration 0013** adds `class` and `priority` to
  `leases`; existing rows become `guaranteed`, which keeps their
  behaviour). `guaranteed` is today's admission: the owner's running
  charge with this lease stays within their `guaranteed_mib` — and a
  user with no `guaranteed_mib` keeps every lease guaranteed. `burst`
  covers work above the guarantee (the first lease past it bursts) or
  forced with `"burst": true` on create — preemptible even within
  another user's guarantee, and admissible only while the node's free
  hugepages stay above `BURST_RESERVE_MIB` (default `8192`, `0`
  disables; see docs/operations.md) after its own, so guaranteed work
  and crash recovery always have room to land. A burst lease refused on
  the reserve answers `503` `no burst capacity` with `Retry-After: 30`
  on every admission path — create, resume, restart, restore, fork,
  clone — and stays as it was; an undrain defers it (drained, retried
  later) instead of losing it. Create takes `"priority"` (int,
  default `0`, between `-128` and `127`): preemption order within a
  class, lower preempted first,
  advisory until part 3. `class` and `priority` ride every lease row
  and detail; the dashboard marks burst rows (`▶ running·b`) and adds
  `burst N` to the capacity panel.
- **Preemption by suspend (#128, part 3).** A guaranteed admission
  (create, fork, clone, resume, warm or cold restart, restore, crash
  recovery, undrain) that cannot get its hugepages now reclaims them by
  suspending burst leases through the normal pause path — memory
  continues on resume, so the generation does not change. Preemption
  takes the lowest `priority`, then the newest lease, then the owner
  furthest over its `guaranteed_mib`, stops as soon as enough memory is
  free, and is serialised so two guaranteed creates cannot each preempt
  for themselves. Every preempted lease is persisted with `preempted`
  and emits a `preempted` event (`for a guaranteed lease of <owner>`).
  A **disk floor** guards the pause: preemption stops while the snapshot
  disk would fall under `PREEMPT_DISK_FLOOR_PCT` (default `15`), and a
  guaranteed admission that cannot preempt answers `503` `capacity:
  cannot preempt (snapshot disk low)` with `Retry-After: 30`. A
  background **resume queue** runs every 15 s and resumes preempted
  leases, oldest preemption first, when they fit again, clearing
  `preempted` and emitting `resumed` with detail `after preemption`;
  the same path serves a client's explicit resume. New metrics
  `spoond_preemptions_total` and `spoond_preempted_leases`, a dashboard
  state mark (`‖ suspended·p`) and an attention-strip row name
  them.

## [2.3.3] - 2026-10-05

Dashboard fixes. No schema change.

### Fixed

- **The dashboard's sparklines vanished after the first frame.** The
  page's stream sent the throughput history with its first frame only,
  so every later frame redrew the graphs empty; it now sends it with
  each frame.

### Changed

- **The events panel shows each event's detail** after its subject
  (`granted from image py-base`, `paused into build 1ede0933`, …; build
  ids cut to 8 characters), and a `released` event now says why:
  `deleted through the API`, `TTL expired` or `released by a held-lease
  rule`. The page hides its scrollbar (it still scrolls).

## [2.3.2] - 2026-10-05

Upgrading: no schema change.

### Fixed

- **A fresh build's recorded size was partial.** The orchestrator
  finishes writing a build's memory file after Checkpoint/Pause return,
  so the size measured at write time (2.3.1, #125) was often 0 or a
  fraction of the build (64,000 bytes for a 140 MB py-base checkpoint).
  ZFS also reports a just-written file's blocks only after its next
  transaction-group commit (a 50 MB file measured 1 block for over 2 s
  on vm2). The backend now re-measures a fresh build every 2 s once its
  memory file exists, records each new size, and stops once the size has
  held for 15 s (for up to 10 minutes); the hourly pass
  still corrects anything left. Until it settles, a kept-bytes budget
  check (#126) can count a build low.

## [2.3.1] - 2026-10-05

Limits for kept checkpoints, sizes recorded when builds are written, and
dashboard fixes found in the first hours of 2.3.

Upgrading: no schema change. `MAX_KEPT_PER_LEASE` (default 4) caps kept
checkpoints per lease; `max_kept_bytes` on a user's quota is optional.

### Added

- **Kept checkpoints are visible (#126).** `GET /api/leases/{id}`
  lists `kept_builds` — `build_id`, `size_bytes` and `kept_at` each,
  oldest keep first — and two gauges report the totals over live
  leases: `spoond_kept_builds` (pins) and `spoond_kept_builds_bytes`
  (their recorded sizes). The dashboard's host panel GC row appends
  `· kept N (X GiB)` when N > 0.
- **A disk warning for kept checkpoints (#126).** When kept bytes pass
  `KEPT_DISK_WARN_PCT` (default 40) percent of the snapshot disk, the
  dashboard's attention strip shows "kept checkpoints use X% of the
  snapshot disk" and the notifier emits `disk.kept` (warn) from its
  periodic checks, deduped and resolved like every condition key. The
  held-lease critical-disk rule still never deletes a kept build —
  unpinning is the owner's call.
- **A per-owner kept-bytes budget (#126).** `POST
  /api/users/{id}/quota` takes `max_kept_bytes` (user-record field,
  `0` = none) beside `max_leases`/`max_ttl`. A keep whose fresh build
  would push the owner's kept bytes (the recorded `size_bytes` over
  their kept builds) past the budget answers `409` naming the budget and
  the new `build_id`: the checkpoint was written but not pinned, so it
  ages out like any unreferenced snapshot and the caller can retry
  without `keep`.
- **Kept checkpoints are capped per lease (#126).** `MAX_KEPT_PER_LEASE`
  (default 4, `0` = no cap) bounds how many builds one lease may pin
  with `{"keep":true}`. A keep on a lease already at the cap answers
  `409` naming the limit and `DELETE /api/snapshots/{build_id}`; nothing
  is evicted and no checkpoint is taken. Unpinning a build frees a slot.

### Fixed

- **A snapshot's `size_bytes` was 0 until the next hourly accounting
  pass (#125).** A fresh checkpoint or pause build's disk size is now
  measured when the build is written and stored with its row, so
  `GET /api/snapshots` and the dashboard show a size immediately
  instead of up to an hour later. The measurement covers every path
  that writes a fresh build: checkpoints (manual, periodic, clone and
  fork), suspend and the admin drain, and template builds from
  `spoond images build`. A failed measurement logs and
  stores 0; the hourly pass still re-measures every build and corrects
  the row.
- **The dashboard's events panel showed owner ids.** An event for a lease
  with no holder or comment named its owner as an identity id
  (`u-42f5…`); it now shows the user's name, as the leases table does
  (#127).
- **The services panel folded units it had room for.** Beside a taller
  images panel it still stopped at six units and said "+1 more" over
  empty rows; it now fills the rows the box has and folds only what does
  not fit.
- **The dashboard's uptime was the host's.** The header's "up 58h" right
  after a deploy was vm2's uptime; it now shows how long the spoond
  backend has run, from a new `spoond_backend_start_time_seconds` gauge.

## [2.3.0] - 2026-10-05

2.3 puts checkpoints on the lease's terms and finishes the dashboard.
Periodic checkpoints are off unless a lease asks for them; a checkpoint
can be kept and the lease restored to it in place; a cold restart gives a
lease a fresh guest without changing its id. Guest images stop leaving
root-run binaries writable by other users, and the dashboard is laid out
to its mockup, with a panel of lease events.

Upgrading: store migrations 10 (a column with a default) and 11 (a new
table) run on start; a 2.2 binary still runs on the migrated database.
`CHECKPOINT_INTERVAL_MINS` now defaults to `0` (never) and is only the
default for leases that do not set `checkpoint_interval`. Rebuild images
(`spoond images build --all`) to pick up the guest permission fix. The
dashboard's events panel needs `EVENTS_TOKEN` in the backend's env and
the same value as `DASH_EVENTS_TOKEN` in the dashboard's.

### Added

- **Restore a lease in place to a kept checkpoint (#121).**
  `POST /api/leases/{id}/checkpoint` accepts `{"keep":true}`: the
  checkpoint build is pinned — it joins the GC's kept set while the
  lease lives (migration 0011's `lease_kept_builds`) and shows up as a
  restore point. `POST /api/leases/{id}/restore`
  `{"build_id":"<uuid>"}` then rolls the lease's guest back to that
  snapshot in place: the lease keeps its id, holder, name, network
  policy, exposed ports and `checkpoint_interval`; the generation bumps
  and `/run/spoond/generation` is rewritten; create-time secrets are
  re-written; the new `restored` event carries the build id; a
  suspended (or drained) lease comes back running. Owner or admin,
  others `404`; the build must be the lease's own newest checkpoint or
  a kept build of that lease (anything else is `404`); a lost lease is
  `410` like every other route; `409` while busy. Releasing the lease
  (any path) drops its kept rows, so the next GC pass may reclaim the
  builds; a lost lease's kept rows go when its grace period lapses;
  `DELETE /api/snapshots/{build_id}` also unpins a kept build.
  Conformance S6 checks it.
- **Cold restart: `POST /api/leases/{id}/restart?mode=cold` (#120).**
  A warm restart (the default) keeps a persistent lease's guest by
  pausing and resuming it, which cannot unstick a hung process. The new
  `mode=cold` (also accepted as the body `{"mode":"cold"}`) gives any
  lease — persistent or not, running or suspended — a fresh guest from
  the image's current build while keeping the lease id, holder, name,
  network policy and exposed ports: create-time secrets are re-written,
  the generation bumps and `/run/spoond/generation` is rewritten, and
  the "restarted" event carries detail "cold". A persistent lease stays
  persistent but its pause builds stop being its resume point
  (`resume_build_id` is cleared; the next suspend sets it as usual); a
  suspended lease restarted cold comes back running. Any other mode is
  `400`. `spoondctl restart <id> --cold` and the ctl verb
  `restart <id> --cold` drive it.
- **Per-lease checkpoint intervals (#122).** `POST /api/leases` accepts
  `checkpoint_interval` (seconds; `0` = never; omitted = the host
  default) and `PUT /api/leases/{id}/checkpoint-policy` changes it
  later (owner or admin, others `404`; emits a `checkpoint_policy`
  event). Every lease row and detail reports the effective
  `checkpoint_interval` in seconds, host default resolved. Clone and
  fork copy the source's interval. The host default is
  `CHECKPOINT_INTERVAL_MINS` and applies to leases storing -1; the new
  `spoond_checkpoint_pause_seconds` histogram (buckets 1..600)
  measures how long each checkpoint pauses its guest, next to a log
  line naming the lease, the pause and the image's `memory_mb`.

### Changed

- **Periodic checkpointing now defaults to never and is per-lease
  (#122).** `CHECKPOINT_INTERVAL_MINS` defaults to `0` (was `60`) and
  is only the default for leases without their own interval. The
  background pass ticks every minute and checkpoints a lease whose
  effective interval has elapsed and that has been active since its
  last checkpoint — previously every active persistent lease and every
  held lease was checkpointed hourly. A held lease is checkpointed
  only if it has an interval: set one if the holder's work must survive
  an orchestrator crash. Leases created before 2.3 get the host default. An
  orchestrator crash loses the work a lease has done since its last
  checkpoint; a lease that has never been checkpointed is lost
  entirely, while planned restarts and drains pause into a build
  first and lose nothing.

- **The dashboard's panels are laid out to the mockup (#118, parts
  1–3).** One fixed-width character grid (104 by default, `DASH_WIDTH`
  72–104) draws, under the header and legend: an attention strip when
  something needs a person; the capacity and host panels side by side
  (stacked below 104 cells); a full-width throughput panel — running
  leases, requests per second, creates per minute and egress
  connections, each with its current value and a 23-cell sparkline over
  the history, titled `throughput · last N min` for the window the
  history actually covers (with `DASH_HISTORY` 150 and the default
  2 s scrape that is 5 min); the leases table — id, image, owner, the
  run state (`▶ running`, `‖ suspended`, `■ lost`, `⭘ recovered`),
  policy, age, time left (a held lease counts down its hold, a
  persistent lease without a hold shows `∞`) and the holder, `◆` before
  a held lease's holder and `◉` once the hold has lapsed (on the page
  the holder is still a link); the image catalog (61 cells: shape, live
  leases, lifetime uses, baked-at) beside the systemd units (41 cells:
  `✓ active`, `○` a state on its way, `✗` one that needs a person),
  stacked below 104 cells; a refusals-and-failures row — auth, quota,
  throttled, capacity, build fails and lost leases, non-zero counts
  highlighted, with the mean create and resume latencies (– for none) —
  and the events panel. `spoond top` draws the same frame with ANSI
  styles, and a test draws every width from 72 to 104 and checks every
  row fits with every panel border in its column.
- **Dashboard follow-ups (#118).** The events panel is fed by the
  backend's lease event stream instead of the backend's journal: the
  backend gained an events-only `EVENTS_TOKEN` (constant-time compare,
  empty disables) that may `GET /api/leases/events` — every owner's
  events, and that route only — and is refused on every other route,
  and the dashboard holds one SSE subscription with
  it (`DASH_EVENTS_TOKEN`, resuming by `Last-Event-ID`, backing off on
  errors) and keeps the last 50 events. The panel shows the newest 5 —
  `HH:MM:SS`, the type padded to 10, the lease id to 10, then the
  holder, else the comment, else the owner — styled by type (`lost` and
  `held_action` warn, `released` dim); without `DASH_EVENTS_TOKEN` it
  says so, dim. The journalctl reader (`DASH_ACTIVITY_UNIT`) is gone.
  A lease with no holder and no name (a CI job) shows its comment in
  the holder column, dim, cut with `…`. The refusals row at narrow
  widths now drops whole counters from the right instead of clipping
  one mid-item, and the services panel folds units past its row cap
  into a final `+N more` row.

### Fixed

### Security

- **Guest system binaries were writable by any user (#124).** E2B's
  template build runs `chmod -R 777 /usr/local` and `/code`, and writes
  envd with mode 0777, so an unprivileged user inside a guest could
  replace binaries that root runs (spoond-guest-init, e2b-provision-runner,
  envd). spoond-guest-init now removes group and other write access under
  /usr/local, sets envd to 0755 and makes /code sticky (1777) before the
  template is snapshotted. Images must be rebuilt
  (`spoond images build --all`) to pick it up. Conformance L7 checks it.

## [2.2.1] - 2026-10-04

Fixes found running 2.2.0 under real load.

Upgrading: the runner now drains on SIGTERM for up to `RUNNER_STOP_GRACE`
(default 10 min), so the `spoond-runner` unit needs `TimeoutStopSec`
above it (`deploy/spoond-runner.service` sets 660). No schema change.

### Fixed

- **Restarting a persistent lease bumped its generation.** `POST
  /api/leases/{id}/restart` on a persistent lease is a snapshot
  round-trip: the guest resumes from the pause build it just wrote, so
  its memory continues. It still bumped the generation, telling clients
  that work had been undone when it hadn't. Only a non-persistent restart
  (a fresh sandbox) and crash recovery bump it now.
- **A lease holder with a link broke its dashboard row.** The page
  swapped the holder for an `<a>` but took the rest of the row along with
  it, so the row's tail (padding and right border) appeared twice and the
  row wrapped. The anchor now wraps only the holder text. A holder longer
  than its column is cut with `…`.
- **A checkpointing lease answered 410 "lease no longer exists".** The
  periodic checkpoint pauses the sandbox while it writes the snapshot
  (about two minutes for a 4 GiB guest), and the orchestrator reports
  the sandbox missing meanwhile. Exec, stat and guest dial answered `410`
  and the file routes `404 file not found`, which tells a client its
  lease is gone for good. While the lease is busy they now answer `409`
  with `Retry-After: 5`.
- **Restart is no longer called a reboot.** On a persistent lease it
  pauses and resumes the guest with its memory and processes intact, so
  it cannot unstick a hung guest. The API, ctl and spoondctl docs now say
  so. A cold restart that keeps the lease id is planned (#120).
- **Stopping the runner leaked its job leases (#119).** systemd's
  SIGTERM killed `spoond-runner` mid-job, so its deferred deletes never
  ran: the job leases kept their sandboxes until their TTL, and Forgejo
  never heard the jobs' fate. Now, on SIGTERM/SIGINT, the runner stops
  fetching jobs and lets running jobs finish for up to `RUNNER_STOP_GRACE`
  (default `10m`; the unit's `TimeoutStopSec` must stay above it,
  `deploy/spoond-runner.service` sets 660). It then cancels the rest: each
  is reported to Forgejo as cancelled and its lease released. Then it
  exits 0. Every job lease's comment names its job
  (`forgejo job <id> <job URL>`). At start, the runner deletes its
  token's leases with such a comment, since they are a dead
  predecessor's orphans. Job leases are deliberately not held: held
  leases are checkpointed periodically, which would pause CI sandboxes.

## [2.2.0] - 2026-10-04

2.2 gives the lease API what a client needs to drive work inside a lease
without exec gymnastics: files in and out, raw TCP to a guest port,
secrets as files, a live event stream, and a generation counter that
tells a client its guest was restored. For operators it adds a readiness
endpoint for uptime monitors and push notifications to webhooks.

Upgrading: store migration 9 adds the `generation` column with a default
of 1. It runs on start, and a 2.1 binary still runs on the migrated
database.

### Added

- **Lease generations (2.2, #112).** Every lease carries a `generation`
  (store column via migration 9, defaulted to 1), returned by the lease
  API as `generation` and written into the guest at
  `/run/spoond/generation` (0644, parent `/run/spoond` 0755, via the
  substrate file operations; the file is also written at create and on
  every resume). It starts at 1 and is bumped — and persisted — on
  exactly the paths that put the lease into a state its processes did
  not continue from: crash recovery (`recoverFromCheckpoint`) and
  restart (both the persistent pause + resume and the non-persistent
  fresh sandbox path). A planned pause/resume and the
  admin drain/undrain continue the memory and do not bump it. The guest
  write is best effort: a failure is logged and nothing else changes.
- **Lease secrets as files (#80).** Lease create and exec accept an
  optional `secrets` object (`{name: value}`; names
  `[A-Za-z0-9_.-]{1,64}`, at most 32 secrets and 64 KiB of values per
  request, `400` otherwise). Before anything runs the backend mounts a
  0700 tmpfs at `/run/secrets` inside the guest (owned by the exec user)
  and writes every secret as `/run/secrets/<name>`, mode 0600, through
  the substrate's file API — never environment variables, never argv.
  Create-time secrets stay for the lease's life and are re-written after
  a resume, restart or crash recovery; the tmpfs is guest memory, so
  snapshots (suspend, checkpoint, fork, clone) carry the files.
  Exec-time secrets are written before the command and
  removed after it, restoring any create-time value a name shadowed.
  Values are kept in the backend's memory only — never in the store,
  logs, error strings or metrics, never returned by any endpoint — so a
  backend restart loses them and callers re-send on their next exec.
- **Webhook notifications for events that need a person (#117, spoond
  2.2).** With `NOTIFY_WEBHOOKS` set, the backend pushes the events a
  person should know about to ntfy, Slack or Discord (`slack` format
  works for both), or any JSON receiver — instead of waiting for
  someone to watch a dashboard. The sources are the lease event bus
  (a lease `lost` is critical; a held-lease rule action is warn,
  critical when it released) and a once-a-minute pass over the
  standing conditions: a watched systemd unit not active (critical,
  keyed per unit; `NOTIFY_UNITS`, default the E2B orchestrator and the
  SSH gateway),
  the snapshot disk past the dashboard's warn/danger levels (80 %/
  90 %), the hugepage pool past its own (80 %/92 %), the TLS
  certificate within 30 or 7 days (warn) or 1 day (critical), a
  failed snapshot GC pass (warn), and the newest database backup
  older than `BACKUP_MAX_AGE_SECS` (warn; default 93600 s = 26 h —
  the 03:00 daily run plus one missed day). Every condition has a
  stable key: repeats dedupe to one message per hour, and when a
  condition clears one `resolved` message goes out — only for keys an
  alert actually opened, so a healthy system stays silent. Delivery
  is asynchronous per webhook, retries with exponential backoff for
  up to an hour before a message is dropped and counted, and each
  webhook is limited to 30 deliveries per hour. Receivers are matched
  by `min_severity` and an optional key glob list. Webhook URLs and
  headers may carry secrets and are never logged: everything names a
  webhook by its index and a redacted `scheme://host`. Every outcome
  is counted in
  `spoond_notifications_total{webhook,severity,result}`. New package
  `notify`; ntfy messages are published as JSON to the server root,
  with the topic taken from the configured topic URL. `spoond notify test` posts one test message to every
  receiver and reports per-webhook results; `spoond doctor` probes
  each receiver's reachability and reports dropped deliveries of the
  last 24 h (mirrored to `NOTIFY_STATE_FILE`). Documented in
  [docs/operations.md](docs/operations.md#notifications-to-webhooks).
- **Substrate file operations (#114).** `substrate.Substrate` gains
  `WriteFile`, `ReadFile`, `Stat`, `MakeDir` and `Remove` with
  `substrate.FileInfo` and a `substrate.ErrTooLarge` sentinel: file content
  and metadata against a lease's filesystem, substrate-wide, with the HTTP
  routes to follow. The E2B implementation pushes content through envd's
  HTTP `/files` on 49983 (multipart POST creates parents) and metadata
  through the envd filesystem Connect client; envd sets no mode, so the
  file is created empty with its mode by an exec `install` before the
  upload (so a 0600 file is never readable by others) and the mode is
  re-applied with a chmod afterwards. The fake substrate implements the same
  semantics on an in-memory per-sandbox filesystem (modes kept, parents
  created, `ErrNotFound`), cleared when the sandbox is deleted.
- **Lease file API (#114).** `/api/leases/{id}/files/{path…}` puts the
  substrate file methods on the wire: GET downloads a file
  (`application/octet-stream`, or the metadata document with `?stat=1`),
  PUT writes the request body (mode from `?mode=0644` octal, default
  0644, parents created), `POST ?op=mkdir` creates directories (mode
  default 0755), DELETE removes (`?recursive=1` for non-empty
  directories). Owner or admin only — a share grant does not carry file
  access — with the usual 404 for anyone else and 409 while the lease is
  suspended. Paths are guest-absolute and cleaned; `..` cannot leave the
  guest root. Transfers are capped at 256 MiB (413 beyond), and every
  call counts as activity for the idle sweeper. Conformance group F2
  writes, stats, reads and removes a file in a py-base lease through the
  API and confirms the bytes from inside the guest.

- **Readiness endpoint for uptime monitors (#81).** `GET /readyz` on the
  lease API listener (no auth, like `/healthz`) answers
  `200 {"status":"ok"}` only when the orchestrator's node info reports
  healthy, the catalog answers a trivial query, and the snapshot disk and
  hugepage pool are below the dashboard's danger levels (90 % / 92 %);
  otherwise `503 {"status":"fail","checks":[…]}` names each failing
  check with a reason. Every check is bounded to 2 s on its own and the whole
  answer is cached for 5 s, so an external poller costs nothing. The
  web dashboard serves `GET /readyz` the same way: 200 only when its
  sources (the metrics scrape, the catalog, the identity store) answer.
  `docs/operations.md` gains an example Gatus configuration watching
  both `/readyz` endpoints and the SSH gateway port, with conditions on
  status, response time and certificate expiry.

- **Lease event stream over SSE (#115, spoond 2.2).** Every lease
  lifecycle change now emits one event on an in-process bus:
  `created` (grant, fork, clone), `released`, `suspended`, `resumed`,
  `checkpointed`, `recovered` (from a checkpoint after a crash),
  `lost`, `restarted`, `holder_set`, `holder_cleared` and
  `held_action` (the held-lease rule and action that fired). Each
  event carries a per-process monotonic `seq` and a random per-start
  `epoch`, so a client can tell a backend restart from an ordinary
  resume. The bus keeps the last 10000 events and serves them over
  Server-Sent Events at `GET /api/leases/events` (the caller's leases;
  admins see all) and `GET /api/leases/{id}/events` (one lease): ids
  are `<epoch>-<seq>`, `Last-Event-ID` resumes exactly where the
  caller left off when the event is still buffered, an unresumable
  position (unknown epoch, event out of the ring, malformed id) is
  announced with a `gap` event before the live flow, and a
  `: keepalive` comment every 15 s keeps idle streams open. Access is
  re-checked per event, so a stream never leaks another owner's
  leases. In-process consumers (the webhook notifier is the intended
  first) subscribe with `Service.Subscribe(filter)`; a slow subscriber
  has events dropped — never blocking the lifecycle — and is told so
  with a `gap` event. Documented in
  [docs/api.md](docs/api.md#lease-events-server-sent-events).

- **Guest port dial (#113).** `GET
  /api/leases/{id}/ports/{port}/dial` upgrades to a WebSocket that
  carries raw bytes both ways to TCP port `port` (1–65535) inside the
  lease, through the substrate's host-to-guest dial (the same HostIP
  DNAT path the SSH gateway and the HTTP proxy use). Binary frames;
  either side closing closes both; a dial that carries no bytes in
  either direction for 10 minutes closes.
  The dial is host-to-guest, not guest egress, so it works under every
  network policy (`restricted` and `none` included). Owner or admin —
  anyone else gets the lease routes' usual `404` — with `409` for a
  suspended lease, `403` for envd's port 49983, `429` past 16
  concurrent dials per owner, `502` when the lease has no running
  sandbox or the guest port is closed, and `400` for a bad port. Counted in
  `spoond_guest_dials_active` and `spoond_guest_dials_total{result}`.
  The conformance suite dials a TCP echo server in a py-base lease
  through the new route (`TestN7_GuestDialEcho`), proving host-to-guest
  TCP on E2B.

## [2.1.2] - 2026-10-04

### Fixed

- **Resuming a running lease rolled its memory back.** `POST
  /api/leases/{id}/resume` had no already-running check: on a running
  lease it restored the pause build again, so the guest lost everything
  since that snapshot. Resuming a running lease is now a no-op that
  answers `200` with the lease as it is.
- **Dashboard rows were double-spaced.** The page put a newline between
  its grid rows, which are block elements, so every row was followed by
  a blank line and the panel borders broke into pieces. The rows are now
  joined with nothing between them.

## [2.1.1] - 2026-10-04

### Fixed

- **Dashboard rows ran together.** The live stream's first frame (and
  any frame where the row count changed) replaced the page's
  `<pre id="grid">` with its bare rows, dropping the element and its
  class, so the grid lost its line breaks and wrapped at the window
  edge. The stream now patches the rows inside the `<pre>`.

## [2.1.0] - 2026-10-04

2.1 makes spoond a plain microVM utility again and gives it a terminal-style
dashboard. Agent workflow (the hive, the worker layer, the bee loop) moved
to the separate Honey project; leases can name what holds them, and held
leases are bounded by limits that act on their own.

### Added

- **Package `grid` (#110).** A public character-grid renderer
  (`github.com/jrimmer/spoond/v2/grid`): cells with a glyph, a style and an
  element id; boxes with titles on the frame; text bars with a warning tick;
  block-character sparklines; ANSI, HTML-span and plain-text writers; and a
  glyph check against the shipped JetBrains Mono, so every drawn character
  comes from the same font and columns never misalign. `Sanitize` makes
  user-supplied text safe to draw.
- **Limits on held leases that act automatically (#111 follow-up).** A
  held lease can no longer keep memory or disk forever, and nobody has
  to watch a dashboard for it: the limits run in the sweep loop, skip
  while draining, and every action is logged (one line naming the
  lease, holder, rule and numbers), counted in
  `spoond_held_actions_total{rule,action}` and recorded on the lease
  (`last_action`, `last_action_at`, returned by the lease API together
  with `hold_expires_at`). Rule 1 (`HELD_IDLE_TIMEOUT_SECS`, default
  14400 = 4 h) suspends a held lease idle that long (memory and
  hugepages freed, nothing deleted, resumes on next use — the SSH
  gateway resumes on attach, the owner with `POST /api/leases/{id}/resume`,
  which now accepts held leases as well as persistent ones). Rule 2 (`HELD_SUSPENDED_RELEASE_SECS`, default 604800 = 7 d)
  releases a lease a rule suspended that stayed untouched that long.
  Rule 3 lapses a hold on its own: `HOLD_TTL_SECS` (default
  604800 = 7 d) from set or renewal — renewal is the holder PUT with
  the same holder, another holder is `409` — capped at
  `HOLD_TTL_MAX_SECS` (default 2592000 = 30 d) for the new explicit
  `hold_ttl` on create, fork and the holder PUT; a lapsed hold
  suspends a running lease and never releases one (`hold_state`
  `lapsed`; rule 2 takes it from there unless it is renewed). Rule 4 (`PRESSURE_DISK_FREE_PCT` 15, `PRESSURE_HELD_IDLE_SECS`
  1800 = 30 min) shortens rule 1 when the snapshot disk runs low or
  free hugepages would not admit the smallest seeded image. Rule 5
  (`CRITICAL_DISK_FREE_PCT` 5, `CRITICAL_DISK_RECOVER_PCT` 10) releases
  leases a rule suspended, oldest suspension first — at most one per
  sweep tick, since a release frees disk only via a later GC pass —
  until free space recovers, and only with `GC_DELETE=1`. Nothing
  running is ever released automatically, and neither is a lease
  suspended by hand or by the drain. `0` disables a rule (hold expiry
  excepted). Store
  migration 8 adds the hold-expiry and last-action columns.

- **Lease holders (#111).** A lease can say what holds it — a CI job,
  an orchestrator's flight, a person's scratch work — with an optional
  link. `holder` and `holder_url` are accepted on lease create and on
  fork, returned on every lease read and in lists, and can be set or
  cleared later with `PUT /api/leases/{id}/holder` (owner or admin;
  `holder` at most 128 printable characters, `holder_url` empty or an
  absolute http(s) URL of at most 512 characters, otherwise `400`). A
  lease with a non-empty holder is **held**: the TTL sweeper does not
  release it, the idle sweep does not suspend it, and it is
  checkpointed periodically like a persistent lease, so the holder's
  work survives both expiry and a crash. Clearing the holder restores
  normal sweeping. Store migration 7 adds the two columns (existing
  leases default to unheld).

### Changed

- **The dashboard is drawn on a character grid (#110 part 2).** `spoond
  dash` renders one fixed-width grid (104 by default, `DASH_WIDTH`
  72–104): framed, titled panels for capacity, host meters, throughput,
  leases, images, units, refusals and events, plus one attention banner
  shown only when something needs a person (a unit not active, a lost
  lease, hugepages or snapshot disk past the danger level, the TLS
  certificate inside 30 days, an automatic held-lease action in the
  last 24 h). The page is that grid in a `<pre>` with vendored WebTUI
  for the chrome and Datastar patching changed rows over the SSE
  stream; holder text links out, a lapsed hold is marked. The Starbase
  components, the pixel-art themes, the theme switch, the starfield,
  odometers, gauges, sparkline components and their vendored files are
  removed; one palette as CSS variables remains. New: `spoond top`, the
  same grid with ANSI styles in the terminal at the terminal's width
  (COLUMNS, else 104), redrawn every 2 s — same collector, same banner,
  no browser (excluded with `dash` by the `nodash` tag).

### Removed

- **The hive, the worker layer and the bee loop moved to Honey.**
  spoond is a microVM utility and takes no position on how agents
  organise work; agent workflow now lives in the separate Honey project
  (`lacy.casa/honey`), which uses spoond through the lease API. Gone from
  spoond: the `hive` package, `spoond hive`, `cmd/spoond-hive`, the
  `GET /hive/guide` and `POST /hive/check` routes (a 2.0 preview), the
  `<base>-worker` image layer and `worker-start.sh`, and the bee, swarm
  and hive terms in the docs. Images already built with the layer stay
  in the catalog until their owner deletes them.
- **`spoond acp`.** It was spoond's own agent loop behind an ACP
  endpoint. Agents use spoond through `spoond mcp`; `SPOOND_LLM_MODEL`
  (and `FORKD_LLM_MODEL`) are no longer read. The `noacp` and `nohive`
  build tags are gone.
- **The cfos adapter** (`cfos/`, `cmd/cfos-adapter`,
  `deploy/cfos-adapter.service`), written for forkd and unused since
  the E2B cutover.

## [2.0.0] - 2026-10-03

2.0 replaces forkd with a patch-queue fork of E2B's node runtime as the
sandbox substrate (production cut over 2026-10-01), moves spoond's state
to SQLite, and adds a template-based image pipeline, native
fork/checkpoint and pause/resume, a drain protocol for orchestrator
restarts, and a read-only dashboard. The lease API is the compatibility
contract and changes only additively (D5); everything beneath it changed.
The design, decisions and per-unit specs are in
`docs/plans/2026-09-30-e2b-substrate/`.

### Changed

- **Breaking: the substrate is now E2B's orchestrator, not forkd.** spoond
  is the control plane; the data plane is a fork of `github.com/e2b-dev/runtime`
  (orchestrator + template manager, plus envd inside every guest), driven
  over gRPC through the new `substrate/` package (D1, D2, D15; `39f9da7`
  substrate interface and E2B client, `156d273` lease lifecycle rewired onto
  it, `efb1b56` merge of U06–U11). Every sandbox is a memory-snapshot
  restore, so starts are warm: create p50 60 ms / p95 73 ms against the
  2000 ms budget (`RESULTS.md`, U12 precondition run). An operator must
  bring up the E2B host — `deploy/e2b/host-setup.sh`, the
  `e2b-orchestrator` unit, the `e2b-guard` firewall and the OpenTelemetry
  collector (U04; `dafe71c`, `d04faf7`) — build every image into templates
  with `spoond images build --all` (U07), and point the backend at the
  orchestrator with `E2B_GRPC_ADDR`, `E2B_PROXY_URL`,
  `E2B_TOKEN_SEED_FILE`, `E2B_TEAM_ID`, `E2B_TEMPLATE_STORAGE_PATH`,
  `IMAGE_REGISTRY` and `HOST_GUEST_SERVICE_ADDR` (U12 step 4;
  `01-architecture.md`). There is no forkd adapter and no in-place
  migration of forkd leases or snapshots (D15; U12, non-goal).
- **Breaking: forkd is gone.** forkd's client, its bake scripts
  (`deploy/bake-*.sh`, `deploy/rebuild-dev-base.sh`), `deploy/rootfs-init/`,
  the forkd rollout and spawn-watchdog scripts, the `spoond-watchdog`
  units, and the backend's `FORKD_URL`, `FORKD_TOKEN`,
  `FORKD_HTTP_TIMEOUT_SECS`, `NETPOL_DNS` and `KNOWN_IMAGES` settings are
  deleted in 2.0 (D1; U12 step 18). An operator must move the backend's
  environment to `/etc/spoond/backend.env` and change the unit's `After=`
  from `forkd-controller.service` to `e2b-orchestrator.service`
  (U12 steps 4 and 18). The forkd-era installer and the rollback artifacts
  stay on the host only until U12 step 20 — 30 days after the cutover
  (README "Install").
- **Breaking: lease state lives in SQLite.** Leases, shares, the warm pool,
  the image catalog and the build/snapshot catalog are persisted in an
  embedded SQLite database at `SPOOND_DB_PATH` (D10, U05; `2b721d1`,
  `60f007c`), so a backend restart no longer loses leases — conformance
  R3 (backend restart) passed as a U05 deploy check (U05 "Done when"). An
  operator must provision `SPOOND_DB_PATH` and a backup directory
  (`SPOOND_BACKUP_DIR`); in-memory state is no longer authoritative.
- **Breaking: Go 1.27.1 is required to build and deploy.** `go.mod` and
  every dependency were moved forward with no downgrades (D11, U01;
  `bb21513`); `modernc.org/sqlite` v1.60.1 needs Go ≥ 1.26. An operator
  must upgrade the toolchain on the build host and on the E2B node (U01
  step 10).
- **Breaking: the Go module path is `github.com/jrimmer/spoond/v2`.** Go
  resolves a v2 tag only for a module path ending in `/v2`, so code that
  imports spoond packages must change its imports; behavior is unchanged
  (`6caea85`, merged in `c46a611`).
- **Breaking: memory is fixed per image.** A snapshot restores with its
  build's RAM, so `memory_mib` on create must be 0 or exactly the image's
  memory; anything else returns 400 (D16; `156d273`). An operator must drop
  per-lease memory overrides from jobs and automation.
- **Breaking: 2 MiB hugepages are required on the host.** E2B builds every
  template with hugepages, spoond sets `huge_pages=true` on every create
  and refuses with 503 `capacity: …` when free hugepages are short (D12;
  `156d273` admission, `c68b14c` capacity metrics, U08). An operator must
  reserve hugepages at host bring-up (`deploy/e2b/host-setup.sh`, U04) and
  size them to the working set — 48 GiB at the cutover (U12 step 13).
- **Breaking: the SSH gateway relays sessions instead of dialing a guest
  sshd.** Session channels are relayed onto envd processes through the
  backend's `/api/sandboxes/{id}/stream` (D14, U09; `63d1f12`); spoond now
  has no `setns`, no netns and no iptables code (U09; `55cd9e8` deletes
  the netns dialing). The gateway's `SHELLY_BINARY_URL` and
  `LLM_GATEWAY_URL` defaults moved from `10.43.0.1` to `10.0.0.11`
  (`63d1f12`); an operator must remove overrides of both so the new
  defaults apply (U12 step 9).
- **Breaking: admin routes need `ADMIN_TOKEN`.** Drain, undrain and
  reconcile are authenticated by a new constant-time bearer token; the
  routes 404 while it is unset and 401 on a wrong token (U10; `cbf12fa`).
  An operator must generate and set `ADMIN_TOKEN` for the drain hooks to
  work (U12 step 4).
- The HTTP proxy dials sandboxes through the orchestrator's sandbox proxy
  instead of a netns dial (U09; `55cd9e8`), and exposed ports are published
  as per-sandbox peer allowances on E2B's egress firewall, with live policy
  changes on `POST /api/sandboxes/{id}/network` (U09; `55cbfe8`).
- The shelley-binary and LLM-gateway URLs default to the host-service
  address `10.0.0.11:8891` — the value of `HOST_GUEST_SERVICE_ADDR` in
  `01-architecture.md` — replacing forkd's `10.43.0.1` (U09; `63d1f12`).

- **`FORKD_`-prefixed configuration variables are renamed to `SPOOND_`;
  the old names are deprecated but still work.** `SPOOND_BACKEND_URL`, `SPOOND_AGENT_TOKEN`, `SPOOND_IMAGE`,
  `SPOOND_LLM_MODEL`, `SPOOND_CTL_HOST`, `SPOOND_CTL_PORT`,
  `SPOOND_CTL_KEY`, `SPOOND_GATEWAY_HOST` and the guest's `SPOOND_NO_TMUX`
  are the 2.0 names; each old `FORKD_` name is still read as a fallback
  that logs a one-line deprecation warning, once per variable per process
  (`SPOOND_` wins when both are set). The pairs are tabulated in
  docs/setup.md ("Renamed in 2.0"). The gateway's `forkd-*` SSH
  permission keys and the endpoint response's `forkd_id` field keep their
  names: they are stored/protocol data, not configuration.

### Added

- **`/api/leases` as the primary lease API path**: every route under
  `/api/sandboxes` is also served under `/api/leases` with identical
  behavior, auth and responses — one path rewrite at the top of the
  handler chain, so metrics and logs keep their `/api/sandboxes` labels
  and nothing double counts. `/api/leases/…` is the documented path and
  `/api/sandboxes/…` stays as a permanent alias (D5 — additive only).
  Error messages and docs now say "lease" for the resource; JSON keys,
  Go identifiers and metric names are unchanged.
- **Fork**: `POST /api/sandboxes/{id}/fork` checkpoints a running sandbox
  once and creates N sandboxes from that build (D3; U08; `156d273`).
- **Checkpoint**: `POST /api/sandboxes/{id}/checkpoint` snapshots a running
  sandbox, which keeps running; persistent leases are also checkpointed
  periodically every `CHECKPOINT_INTERVAL_MINS` (default 60) to bound crash
  loss (U10; `96676dc`).
- **Suspend/resume with memory**: pause writes a build and stops the
  sandbox; resume restores it with its memory from that build (D3; U08;
  `156d273`). Lease objects now carry `state` (`running|suspended|
  recovered|lost`), `recovered_from` and `last_checkpoint_at` (`60f007c`,
  `156d273`), plus `build_id` and `resume_build_id` (`220d7a2`).
- **Drain protocol**: `POST /api/admin/drain` pauses every running lease
  (persistent or not) and quiesces the node, `POST /api/admin/undrain`
  resumes them, and the new `spoond drain` subcommand is wired into the
  `e2b-orchestrator` unit's `ExecStop`/`ExecStartPost` so planned
  orchestrator restarts are lossless (D4; U10; `cbf12fa`, `f348719`).
- **Crash recovery**: on backend start, every 30 s, and whenever the node
  comes back, leases with a checkpoint are resumed from it and the rest are
  marked `lost` (410 `sandbox lost in a substrate crash; delete this
  lease`); `POST /api/admin/reconcile` reports the summary (D4; U10;
  `14284a7`).
- **Image pipeline**: one Dockerfile per capability in `images/`, built by
  docker, pushed to the local registry and turned into E2B templates by
  `spoond images build <name>|--all`, with the catalog (images, builds,
  owners, per-image env) in SQLite (D6, U07; `f9f8e44`, `b6e6093`,
  `50180f2`, `f884782`). The forkd bake scripts and `rootfs-init` are
  removed with forkd (see Removed).
- **Snapshot catalog and GC**: `GET /api/snapshots` and `DELETE
  /api/snapshots/{build_id}` list and delete the caller's builds, and a
  GC keeps the closure of the root set — every image's current build,
  every lease row's resume and checkpoint builds, every sandbox's build,
  every in-flight build, then parents (`parent_build_id`) and header
  references (`build_refs`) — and reclaims unreferenced builds older than
  an hour. It runs 10 minutes after start and then hourly, never during a
  drain; dry-run by default, `GC_DELETE=1` enables deletion (U11;
  `220d7a2`). A build is also never a candidate while any non-deleted
  build names it as a parent or references it in `build_refs`, whatever
  the lease and sandbox rows say (`d7d5686`). A lease lost in a crash
  keeps its snapshots for a grace period after `lost_at` — 7 days when
  persistent, 1 day otherwise (`GC_LOST_GRACE_PERSISTENT`,
  `GC_LOST_GRACE`); leases lost before `lost_at` existed are stamped by
  the first GC pass that sees them, and `spoond doctor` warns about lost
  leases (`30fc197`, `733952a`; merged in `c83d4cb`).
- **Database backups**: `VACUUM INTO` copies into `SPOOND_BACKUP_DIR` daily
  at 03:00 (and once at start when the newest is over 24 h old), keeping
  the last 7 (U11; `f884782`).
- **Observability**: orchestrator metrics are collected by an OpenTelemetry
  collector and appended to spoond's `/metrics` when `OTEL_PROM_URL` is set
  (U11; `c68b14c`, `d04faf7`); `GET /healthz` reports the node's status and
  is 503 when the orchestrator is unreachable (`c68b14c`); new gauges and
  counters cover node state and substrate operations — `spoond_leases`,
  `spoond_leases_by_image`, `spoond_node_running_sandboxes`,
  `spoond_node_hugepages_free_bytes`, `spoond_node_outstanding_work`,
  `spoond_create_duration_seconds`, `spoond_capacity_rejections_total`,
  `spoond_checkpoint_duration_seconds`, `spoond_snapshot_bytes`,
  `spoond_storage_free_bytes`, `spoond_gc_deleted_total`,
  `spoond_lease_heartbeats_total` (`c68b14c`, `5fa2dcb`, `96676dc` for the
  checkpoint histogram, `220d7a2` for the snapshot, storage and GC
  series, `01edabf` for heartbeats).
- **`METRICS_TOKEN`**: a scrape-only bearer token that reads `/metrics` and
  nothing else, so Prometheus and the dashboard no longer need admin
  rights; adds `spoond_leases_by_image` (`5fa2dcb`).
- **`spoond dash`**: a read-only, live dashboard of leases, sandboxes, host
  vitals (memory outside the hugepage pool, and hugepages as committed
  lease capacity), request rate, creates per minute with the last hour's
  mean create and resume times, egress connections, units and the image
  catalog with each image's lifetime uses, over one shared SSE stream, with 5-minute sparklines
  and themes; basic auth, HTTPS via `DASH_TLS_CERT`/`DASH_TLS_KEY`, default
  port 8893 (`b5d7e5e`, `8e2f08b`; numbers and gauges `87c47e4`,
  `79ea483`, `2b23b1a`, `69eec55`, `b5f6089`, `4222706`). Its data access is read-only
  (`/metrics`, the SQLite catalog, identity names, `/proc`, systemd).
- **`spoond doctor` E2B checks**: the forkd checks were replaced by the
  orchestrator's `/health` and node info (status, running sandboxes, free
  hugepages), the local registry, the token seed (0600, ≥ 32 bytes), the
  SQLite database and catalog, the pinned E2B artifacts by SHA-256, build
  storage headroom, the backend, the gateway port, the LLM gateway and TLS
  (U11; `fe6981d`), plus the Firecracker and kernel versions still in use
  by non-deleted builds (U13; `d66ff13`).
- **Lease heartbeat**: `POST /lease/{lease-id}/active` on the
  guest-service port marks a lease active for the idle sweep, so an agent
  working inside a persistent lease is not suspended; the lease id is the
  capability, writes are limited to one per lease per minute, and the
  route is not served on the API port (`01edabf`, `97cb20d`; merged in
  `8fba7df`).
- **Image uses**: each image's lifetime lease grants are counted
  (migration 0005, `image_uses`) and shown in the dashboard's catalog
  (`fd0828a`; merged in `da0544f`).
- **Layered images**: `images/manifest.yaml` entries can name a catalog
  image in `from:` (built on its current digest, inheriting its shape and
  env) and pass `build_args:`; `images/worker.dockerfile` uses this to turn
  any base image into a `<base>-worker` agent-worker image
  (`go-base-worker`) (`5cd238f`; merged in `6a4404a`).
- **`spoond hive check`** (preview): validates a project's
  `.spoond/hive.yaml` and runs the enlistment checks that can run off the
  host, ending with the next step to take
  (`docs/plans/2026-10-02-swarm-controller.md`; `aa7755c`, merged in
  `7d97786`).
- **Hive guide and check API** (preview): `GET /hive/guide` (no token)
  serves this instance's enlistment guide, rendered live from the same
  tables the server and the checks run on (addresses, image catalog,
  `hive.yaml` schema, `needs:` keys, routes, checks); `POST /hive/check`
  runs the enlistment checks against a submitted `hive.yaml`, including a
  trial lease that probes each `needs:` target from inside the base image
  (`85c07f1`, merged in `f689b62`; `docs/api.md` "Hive").
- **`spoond-netwatch`**: recovers the host network when the port stops
  receiving while the link stays up — it bounces the port, then reloads
  the network, and only as a last resort (nothing received for 10 minutes,
  at most once per 12 hours) reboots cleanly so leases are drained
  (`2125bd2`, merged in `d22266c`).
- **Lease API additions**: `GET /api/sandboxes/{id}`, the fork, checkpoint
  and network routes above, and a binary stream mode for `/stream` with
  `resize`, `kill` and `eof` controls (D5 — additive only; `156d273`,
  `96676dc`, `55cbfe8`, `4c74444`).
- **Conformance suite**: a substrate conformance suite in `conformance/`
  (build tag `conformance`), run from a host checkout through the lease
  API, with E2B-aware reachability probes (under E2B a deny closes at the
  data phase, and port 443 needs a real TLS handshake, because SNI routing
  decides on the ClientHello); the full suite — 27 tests, all groups —
  passed on staging with every budget met as the U12 precondition
  (`RESULTS.md`; U12 step 14 runs the same suite against production)
  (U02; `63e3939`, `3a27ed3`, `107e060`, `4ebea12`, `d2e2d5c`).
- **Operations**: the E2B upgrade runbook (`docs/runbooks/e2b-upgrade.md`,
  rebase of the fork at most monthly, gated by the conformance suite; U13;
  `e1bc392`), daily soak checks for the cutover (`deploy/e2b/soak-check.sh`
  and timer; `002cbca`), and `deploy/e2b/` host bring-up artifacts
  (`dafe71c`).

### Removed

- **forkd**, as part of 2.0: the `forkd/` client package and every Go use
  of it, `FORKD_URL`, `FORKD_TOKEN` (backend), `FORKD_HTTP_TIMEOUT_SECS`
  and `NamespaceControllerMetrics`, the bake scripts, `rootfs-init`, the
  forkd-patched rollout and spawn-watchdog scripts and the watchdog units
  (D1; U12 step 18). Deployed names that merely contain `FORKD` (gateway
  and CI variables, gateway permission keys) are intentionally not renamed
  (U12 step 18). The forkd-era installer `deploy/install-spoond.sh`,
  `scripts/forkd-curl` and the forkd conformance baseline went with it
  (`1e3f224`; merged in `63f5832`).
- The netns, `setns` and iptables code paths in the backend (`55cd9e8`),
  and the guest `sshd` requirement — sessions are relayed onto envd
  processes instead (D14; `63d1f12`).

### Fixed

Found in production after the 2026-10-01 cutover and deployed the same day:

- **A host reboot drains and resumes leases.** At shutdown systemd
  stopped the backend before the orchestrator, so the orchestrator's drain
  was refused and a reboot lost every lease; at boot the undrain ran before
  the backend existed. The new `spoond-drain.service` is ordered after both
  units, so it drains first at shutdown and undrains last at boot
  (`b211048`, merged in `1e0d122`); `spoond doctor` fails when the unit is
  missing, inactive, misordered or cannot read its token (`4131bd6`,
  merged in `d1d04d1`). An operator must install and enable the unit with
  the backend (`docs/install.md`).
- **`lan`/`internet` guests can reach the lease API again.** The fork's
  host-address guard only admits a host destination when an allowance names
  both IP and port, so the any-port LAN ranges did not count and CI jobs
  that lease databases from inside their sandbox broke. The API port is now
  named explicitly (`HOST_API_PORT`, defaulting to `BIND_ADDR`'s port);
  `restricted` and `none` are unchanged (`9643e53`; deployed in `4f63ddf`).
- **Internet egress allowances survive peer refreshes.** The fork's Update
  path collapsed an egress with no CIDRs, domains, rules or proxy to nil
  and dropped `allowed_private`, so the first lease that published ports
  silently removed the lease-API allowance for `internet` leases. A deny of
  TEST-NET-1 (never routed) kept the policy non-empty as a stopgap
  (`dfb6f65`; deployed in `3121626`) until the fork counted
  `allowed_private` itself (e2b-runtime `eb70296db`); the stopgap was then
  removed (`0d8a603`; deployed in `dbc25ec`).
- **Guests resolve through the LAN resolver only.** The old
  `10.0.0.1` + `8.8.8.8` pair fell back to public DNS, which answers
  `*.example.com` with the public edge, and the router gave stale LAN answers;
  `resolv.conf` now names Technitium (`10.0.0.2`) alone, and the egress DNS
  allowance follows it (`f2b0289`; deployed in `26d9cab`).
- **The guest rootfs is kaniko-safe again.** `/var/lib/dpkg/statoverride`
  is emptied (E2B's chrony `_chrony` lines and the images' own `messagebus`
  override made apt abort when kaniko unpacked a base image over the live
  root and replaced `/etc/group`), and `/.dockerenv` is created so
  container-aware tools detect a container (`f2b0289`, `e76d66a`; deployed
  in `26d9cab`).

Found on 2026-10-02:

- **A migration with a duplicate version is refused at start.** Two
  files shared version 5; a database already at 5 would have skipped the
  second for good. It was renumbered (0006, `lost_at`) and the loader now
  rejects duplicates (`3a0b5ab`, `d44d0e3`).
- **The dashboard's create time is real.** It averaged a single 2-second
  scrape and read 0; it now averages the last hour and shows "–" when
  nothing was created (`b5f6089`).
- **`spoond_lease_grant_duration_seconds` measures the grant again.**
  The observation was dropped in the 2.0 rewiring, so the histogram sat
  at count 0 and the dashboard stopped plotting it. It is now observed
  around the whole grant — request received to lease returned, covering
  pool hits, cold creates and the integrity probe; failed grants stay
  out of the latency.

Earlier in the 2.0 effort:

- **Clone, fork and drains are no longer stuck for 120 s.** The substrate
  client waited for `OutstandingWork == 0` after every pause/checkpoint,
  which never terminates; the wait is now bounded (back to the pre-call
  value, or 10 s), taking clone from 120.8 s to 0.596 s (`bc2a78c`, merged
  in `9d51b89`; measured in `cc49dff`, `RESULTS.md`).
- **Image build correctness**: scylla builds without gpg in the build
  context (the signing key and apt list are vendored in `images/`, the key
  fingerprint verified at commit time, since gpg cannot run there) and
  without running host-level sysctls (procps preinstalled, a `dpkg-divert`
  stub around the scylla install), and the pin tracks the offered 2026.2.7
  (`26b8947`, `32bd651`, `b99bb69`, `bda1249`, `81b77f6`, `362d0aa`).
- **Leases survive a backend restart** (conformance R3), previously lost
  with the in-memory store (U05; `60f007c`).

### Security

- **Guest DNS can no longer leak to public resolvers**: sandboxes resolve
  only through the LAN resolver, so credentialed names never fall back to
  the public edge (`f2b0289`).
- **The orchestrator is not reachable off-host**: `deploy/e2b/e2b-guard.nft`
  accepts loopback, lets sandbox source ranges reach only the egress proxy
  and hyperloop, drops everything else from them, and drops all off-host
  traffic to the orchestrator's ports — so the gRPC and sandbox-proxy
  ports are loopback-only (U04; `dafe71c`).
- **Per-sandbox private allowances with TCP port scoping**, plus the
  host-address guard, come from the fork's patch P4, extending E2B's own
  two egress layers (per-netns nftables + userspace TCP proxy) (D13; U03,
  U09).
- **The lease heartbeat is a guest-service route only**: it is not
  mounted, auth-exempt, on the API listener (`97cb20d`).
- **`ADMIN_TOKEN`** gates the drain/undrain/reconcile routes with a
  constant-time compare and 404s while unset (U10; `cbf12fa`);
  **`METRICS_TOKEN`** is scrape-only — the lease API refuses it, and an
  unset token grants nothing (`5fa2dcb`).
- **The dashboard is read-only by construction**: it reads `/metrics` with
  the scrape-only token, opens the SQLite catalog read-only, and only reads
  identity names, `/proc` and systemd (`b5d7e5e`).
- **`spoond doctor` verifies the substrate's secrets and artifacts**: the
  envd/traffic HMAC seed's mode and size, and the SHA-256 pins of the
  published Firecracker, kernel and busybox binaries (U11; `fe6981d`).
- Guest memory never leaves the node: snapshots and builds live on the
  host's storage only; no cloud object storage, Redis, ClickHouse or log
  shipping runs (D2; `01-architecture.md`).

## [1.2.0] and earlier

Earlier releases predate this changelog; summarised from README "Status".

- **v1.2 — infrastructure & observability.** Prometheus metrics for the
  backend, gateway and runner; MCP HTTP/SSE transport; SSH gateway image
  resolution and configurable SSH images; LLM-based PR review in CI; runner
  reliability and exec fixes.
- **v1.1 — multi-user tenancy.** People and agents became first-class
  identities (per-user SSH keys, tokens, quotas, admin roles, lease
  sharing, per-user LLM gateway keys and proxy hostnames), with security
  hardening across the board; see `docs/security.md`.
- **v1.0 — single-operator.** The original lease API, warm pool, gateway,
  proxy and Forgejo Actions runner on a forkd homelab.
