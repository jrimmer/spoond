# Changelog

Notable changes to spoond, newest first. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). In the 2.0
section, every entry names where it comes from: a commit (`<short-hash>`),
a unit of the E2B substrate spec (`U01`–`U13`, under
`docs/plans/2026-09-30-e2b-substrate/`), or a fixed decision (`D1`–`D17`,
in that spec's `00-README.md`). The earlier-releases section is
summarised from README "Status".

## [Unreleased]

### Fixed

- **Conformance N1 probes a configurable LAN target, and I3 no longer
  depends on a stale apt index.** N1 hardcoded `10.0.0.203:443` as the
  private destination for the `internet` and `lan` policies; that box was
  gone, so the case failed even though the substrate was correct. The
  target is now `CONFORMANCE_LAN_TARGET` (a private `host:port`, no
  default): sb sets it in `/etc/spoond/conformance.env` and N1 skips with
  a clear message when it is unset, like the N9 `CONFORMANCE_MIXED_*`
  knobs. I3 installs `docker.io` into `dev-base` after `apt-get update`,
  so it no longer 404s on a package version the Ubuntu archive dropped.

- **`spoond_builds_in_flight` reports the catalog's in-flight template
  builds, and a stale-build failure is now announced.** The gauge was
  only written during a `/metrics` scrape, so the dashboard's "builds
  busy" cell could read 0 while `spoond images build` was baking; the
  metrics tick now refreshes it from the catalog's template builds still
  `building`, the same count the orphan sweep skips on (spoond-63a G3).
  The GC's stale-building sweep emits a lease-less `gc` event per row it
  fails (in dry-run too), because a template build has no owner and never
  appears in `/api/snapshots`. `SPOOND_BUILD_TIMEOUT` now wires
  `ServiceConfig.BuildTimeout` from the environment and is shared with
  the image pipeline, so the two sides cannot drift.

## [2.8.0] - 2026-10-08

spoond cleans up after itself under every race it has met so far. A
lost lease's guest is now always stopped, and a periodic two-pass sweep
deletes sandboxes no lease or pool entry claims (never while a create
or a template build is in flight). A checkpoint, pause, resume,
restart or restore that finishes after its lease was released no
longer brings the lease back, and the admin drain and undrain heal
themselves. Guests get both LAN resolvers with retries
(`SPOOND_GUEST_DNS_ADDR` takes a list), the web proxy routes lease
hostnames before its own `/assets/`, `/lease/` and `/llm/` paths (#144),
a suspended-lease `409` carries `code: lease_suspended`, and a template
build left `building` by a crash no longer pins the snapshot disk. An
operation that races a `DELETE` of the same lease now answers `404`
instead of succeeding. No store migration; the grid package is
unchanged since 2.7.0.

### Added

- **A suspended-lease refusal now carries `code: lease_suspended`.**
  Every "lease is suspended; resume it first" answer is the same `409`
  as before, but its JSON body now includes
  `"code":"lease_suspended"` (additive). A client can tell it apart
  from the other `409` — a busy lease, which carries
  `code: lease_busy` — without matching the message text. The
  plain-text `http.Error` sites (guest heartbeat, LLM gateway, HTTP
  proxy) answer the same JSON shape as the rest. See the error-code
  table in [docs/api.md](docs/api.md).

- **Guests get both LAN resolvers, with retries.** `SPOOND_GUEST_DNS_ADDR`
  now takes a comma-separated list (`10.1.0.2,10.1.0.3`); a single value
  keeps working. The backend grants each address a port-53 egress
  allowance and validates each as an IP at startup, and sends no public
  DNS fallback once any resolver is configured. `spoond-guest-init`
  writes one `nameserver` line per address plus
  `options timeout:2 attempts:3 rotate`, so a guest survives one slow
  or dead resolver — the 2026-10-07 incident where a Honey worker's
  first lookup at boot failed after a disk-saturation spike. The value
  is baked at image build; `deploy/PRODUCTION-ENV-2.7.md` records the
  sb and agent-hub build-worker change and the image rebuild.

### Fixed

- **A lost lease's guest is stopped.** Every path that marks a lease
  `lost` — crash recovery (including a recovery budget that runs out or
  a preempted resume that fails), the admin undrain and the rootfs probe
  — now deletes the lease's sandbox through the substrate, retrying a
  few times with a log line and dropping the sandbox row. Before this, a
  create or resume that failed after its VM had started could leave a
  guest running while the lease answered `410` and looked stopped: the
  crash reconcile's recover-from-checkpoint failure never deleted it,
  undrain cleaned up between attempts but not after the last, and the
  rootfs probe's delete was best effort. A delete that still fails after
  the retries is left to the periodic orphan sandbox sweep, which now
  treats a sandbox whose lease is lost or released as an orphan (and
  never touches a live or busy one); the startup `ReconcileOrphans` runs
  the same rule after its crash reconcile. A lease an owner operation is
  bringing back is never lost: the preempt-resume and undrain losses
  require the lease to be still suspended, not busy and unreleased, so a
  resume in flight saves the guest. A create that finishes after its
  lease was released stops the fresh guest and skips every save, so a
  release cannot be undone by a late recovery, resume, restart or
  restore. The sweep also deletes any substrate sandbox no lease and no
  pool entry claims, but only once it has been seen unclaimed on two
  consecutive passes and never while a create holds that sandbox in
  flight; the startup pass still deletes a foreign leftover at once. A
  cold restart re-checks the released flag after taking the store lock,
  so a release landing between its early check and the lock cannot
  resurrect the lease row or leave its fresh guest; a plain
  (non-persistent) restart does the same. The sweep also skips entirely while
  any template build in the catalog is still `building` (or the catalog
  cannot be read), because a build sandbox has no spoond row; the
  `spoond_builds_in_flight` gauge now reports that count (it was never
  set before). The sweep is safe because the pinned e2b orchestrator
  leaves build sandboxes out of `Server.List`, so a future orchestrator
  must keep them out (or spoond must track build sandbox ids).
  `docs/api.md` states it under [Lost leases]: a lost lease's guest is
  stopped, and `DELETE` frees the quota.

- **A stale `building` template build no longer stays a GC root forever.**
  `spoond images build` wrote the build row in state `building` before it
  asked the orchestrator to build the template, and on failure it wrote
  the `failed` state through the build's own (possibly already cancelled)
  context. A SIGKILL or reboot between the two writes left the row
  `building` with nothing to move it on; every `building` row is a kept
  root, together with its whole ancestor chain, so the catalog — and the
  snapshot disk — grew without bound. The failure update now runs on a
  context detached from the build's deadline, so a timed-out build is
  still recorded as failed; and each GC pass fails any build left
  `building` for longer than twice the shared build timeout (one hour),
  logging the id, kind and age (`gc: marked stale building build failed
  ...`), so the row becomes an ordinary candidate once it has been idle
  an hour (spoond-4yl).

- **Drain self-heal follow-ups: the half-sandbox cleanup only runs after
  a real Create, a wedged heal retry no longer holds off a drain, and an
  admin undrain gives the new deferral a fresh budget.** A resume refused
  before the orchestrator (admission, node status) no longer issues a
  Delete for the still-paused sandbox or logs a bogus cleanup line; the
  delete runs only when a Create actually reached the orchestrator. The
  heal releases its `drainGate` read side for each retry backoff, so an
  admin drain waiting on the write side gets in between attempts instead
  of after the whole retry budget (a wedged Create can run the full
  `E2B_CREATE_TIMEOUT` each time), and re-checks draining before the next
  attempt. An admin undrain that defers a lease drops any earlier
  drain-heal entry, so a give-up from a previous restart does not silently
  skip the new deferral or spend its budget early. `DRAIN_RESUME_MAX_AGE`
  also accepts a plain integer of seconds (matching `E2B_*_TIMEOUT`) as
  setup.md already claimed, rather than reading it as the default.

- **A checkpoint, pause, resume or restore that finishes after its lease
  was released no longer writes the lease row back.** A release running
  while one of those operations was in flight removed the lease from
  memory and deleted its row, but the operation's late save wrote a
  `state=running` row back with no in-memory lease — a phantom lease
  that reappeared on the next backend start and held its owner's quota
  until the lost-lease grace lapsed (spoond-775). `saveLeaseLocked` now
  refuses a released lease, and each async path drops its late sandbox,
  build and lease writes and stops the sandbox it created; the
  checkpoint/pause build is left unreferenced for the GC. A backend
  start reloads every stored lease, so a phantom row such a race left
  behind is not dropped by the reconcile sweep — it is loaded as an
  ordinary live lease whose sandbox is gone and goes through the
  lost/grace path (spoond-d76).

- **A release racing a pause, a checkpoint or a drain is now handled
  under the same lock that marks the lease suspended.** The pause's
  released re-check ran outside the lock that set `Suspended`, so a
  release in that window returned a successful pause, emitted a
  `suspended` event after `released`, credited the lease's memory twice
  and let preemption/idle bookkeeping count a released lease. The
  re-check now runs inside the lock, and preemption's own re-check
  skips a released lease (spoond-d76). A checkpoint whose resume-fresh
  started a sandbox after the release's delete now stops that sandbox
  (detached context, bounded retries) and drops its row, so a resumed
  guest cannot run on holding hugepages. The GC stamps a stored-only
  lost row with an `UPDATE` rather than an upsert, so a concurrent
  release is not undone by re-inserting the row, and the admin drain
  skips (rather than lists as failed) a lease released mid-drain.

- **The web proxy decides by Host first, so guest-service routes no
  longer shadow lease hostnames.** `/assets/`, `/lease/` and `/llm/`
  were matched before lease-hostname routing whatever the Host, so a
  request to `https://<lease>-<port>.<domain>/assets/x.js` answered the
  host's 404 (or a host file) instead of proxying to the guest: Vite/SPA
  bundles under `/assets/` loaded blank, and a guest app's own `/lease/`
  or `/llm/` routes were shadowed. A request whose Host parses as a
  lease hostname now goes straight to the forward-auth gate and the
  guest proxy with its path untouched; only non-lease hosts (guests
  calling `http://<HOST_GUEST_SERVICE_ADDR>:8891/...`) reach the
  internal handlers. The assets containment check is unchanged. #144

- **The admin drain and undrain heal themselves: a detached context, a
  bounded drain, drained leases retried, and draining visible.** A
  client that gave up (the `spoond drain --start` hook at 300 s) used to
  cancel the undrain's remaining resumes, which then went lost, and a
  cancelled pause left a lease running into the stop; drain and undrain
  now run on a context detached from the request with their own bound,
  and a context, admission or capacity error keeps the lease `drained`
  for a retry instead of losing it. A lease the drain paused had no
  automatic exit: a drain self-heal loop now resumes any `drained` lease
  with the same bounded retries as undrain, each on its own doubling
  backoff (15 s to 10 min, so a permanently deferred lease is not
  resumed every pass) and giving up after `DRAIN_RESUME_MAX_AGE`
  (default 24 h) with the lease left suspended — its snapshot intact,
  not lost — and a `drain_gave_up` event, so a missed undrain (a backend
  restart between drain and undrain, or an `ExecStartPost` that exited
  0) no longer strands the lease. A deferred attempt logs a line and
  emits a `drain_deferred` event on the first deferral or a cause
  change; the per-lease backoff state is dropped as soon as the lease
  is no longer drained (owner resume, restore or release), so a later
  planned restart's deferral is not skipped or given up on early. A
  drain that outlives `DRAIN_MAX_SECS` (default 900) on a
  healthy node now undrains itself, logs it and emits a `drain_healed`
  event instead of refusing every create with 503 forever; a lease the
  drain could not pause is logged and emits a `drain_failed` event, and
  a failed node-drain clear keeps spoond draining so the self-heal loop
  retries it. A backend that starts while the node reports `draining`
  adopts that drain, so it does not undrain a node another process left
  mid-planned-stop. A release that races a resume no longer resurrects
  a released lease, and the sandbox the resume created is deleted. An
  owner's own resume finally clears `drained`, so resume-on-next-call
  keeps working and a later undrain cannot resume a lease the owner is
  running. Draining is reported in `/healthz` (`"draining":true`) and
  `/readyz`, and the notifier warns on a `node.draining` key only once a
  healthy node's drain passes half the self-heal limit or the node is
  unhealthy, so a planned restart under a minute stays silent.

## [2.7.1] - 2026-10-07

spoond looks after more of itself and says less on the dashboard about
things a viewer cannot act on. A lost lease now answers `410`
with `code: lease_lost` and its reason, and is released once its grace period
lapses, so its owner's quota comes back; every orchestrator call has a
deadline. The dashboard gains a Notifications panel for spoond system
messages only (dismissable per viewer), i/o stall and disk-busy meters,
a left-aligned header with uptime and clock, a footer with the version,
release date and a GitHub link, and a leases table whose access column
says isolated for no network and whose columns fit their content. Store migration 0019 is additive; the grid package is
unchanged since 2.7.0.

### Added

- **The dashboard's host panel shows disk I/O pressure and the snapshot
  disk's throughput as meters under the CPU meter.** Two new meter rows
  directly under `cpu`: `i/o stall`, whose value is the PSI `full` 60 s
  average drawn on the same 0-100 scale as the other meters, its value
  text `0.4% full`, and
  `<dev> busy`, whose value is the snapshot device's busy share with
  `<N> MB/s w` in its value text (`nvme0n1 busy ... 18% · 12 MB/s w`).
  The stall meter reads `/proc/pressure/io` and warns at
  `DASH_IO_FULL_WARN_PCT` (default 5), turning bad at
  `DASH_IO_FULL_BAD_PCT` (default 15) of the full 60 s average, where
  the Notifications panel also raises
  `disk i/o stalled: full pressure N% over 60 s` (id `io-pressure`,
  cleared when the pressure drops); PSI rather than `iowait`, which
  falls when CPUs are busy even if the disk is saturated. The busy
  meter reads `/proc/diskstats` (a delta between collections) and warns
  at 80, turning bad at 90. A missing PSI hides the stall meter only;
  a device the collector could not resolve hides the busy meter only.
  The device is auto-detected from the storage path's mount or set with
  `DASH_DISK_DEVICE`. The thresholds are a first cut, to be tuned from
  #136's measurements.
- **The dashboard's notifications panel for spoond system messages.**
  The old attention strip drew per-lease rows (a lost lease, the
  preempted burst count, a lapsed hold) that the dashboard viewer cannot
  act on; those leases stay visible in the leases table with their state
  (lost, preempted, suspended). In their place, a bordered full-width
  **Notifications** panel below the header draws spoond's own system
  messages — a systemd unit not active, hugepages or snapshot disk past
  the danger level, kept checkpoints past `KEPT_DISK_WARN_PCT` — each
  with a stable id from its trigger and a severity. The panel is not
  drawn at all when there are no undismissed messages. Each row carries
  a `×` dismiss control; the dismissal is per viewer in `localStorage`
  (try/catch-wrapped, works without it) keyed by the message id, stays
  hidden while the trigger stays active and returns if the trigger
  clears and fires again. No server state; the dashboard stays
  read-only.
- **The dashboard's leases table sizes its columns to the content and
  names its policy and holder columns.** The network-policy column's
  header reads **access** (narrow fallback `acc` / `net`), and a lease
  whose API `network_policy` is `none` shows `isolated`; the stored
  value stays `none`. The fixed-width columns (state, image, owner,
  access, left) are as wide as the widest value actually shown, never
  below their header, so a table of short states leaves no blank run;
  the width freed that way widens the owner first (up to its own longest
  value) and then the last column. The holder column's header is plain
  `holder`, and the `◆ held · ◉ lapsed` legend moves to one dim line
  directly under the table's last row, drawn only when a row carries a
  hold. Docs and goldens cover both frame widths.
- **Lost leases tell their initiator why and what to do.** A lease whose
  sandbox a substrate crash (or a failed recovery) lost now records the
  reason (`leases.lost_reason`, migration 0019) and returns it: the
  `lost` lease event's `detail` carries it, `GET` shows `lost_reason`
  beside `state: lost`, and any call on a lost lease answers `410` with
  `code: lease_lost` and a message naming the substrate, the reason and
  that `DELETE` frees the quota. This replaces the old bare `410` that named
  no cause.

### Changed

- **A lost lease says why.** A call on a lost lease still answers
  `410 Gone`, and the body now carries `code: lease_lost` and a message
  naming the substrate, the reason and the `DELETE` that frees the
  quota. The reason is stored (`leases.lost_reason`, migration 0019) and
  shown as `lost_reason` on the lease object (additive, `omitempty`).
  `409` keeps meaning "busy, retry".
- **The dashboard's header title sits at the left margin and its
  footer links the project's GitHub repository.** The header's left edge
  reads `SPOOND · <host>` at the same inset as the panels' frames (the
  version is not there); the right side keeps spoond's own uptime and
  the frame's clock as `up <dur>, <time>`, dropping the uptime before
  the clock and never overlapping the title. The footer is one dim,
  centred line — `Spoond v2.7.1 (2026-10-07) · GitHub` — with the
  dashboard binary's version (`debug.ReadBuildInfo`, shortened like the
  header used to), its release date (the build's `vcs.time` as
  `YYYY-MM-DD`, omitted for a dev build), and the GitHub mark linking to
  `DASH_PROJECT_URL` (default the module's home); the URL text is no
  longer shown. On a narrow frame the date drops first, keeping the
  version and the mark. In the browser the mark is the standard GitHub
  octocat inline SVG (16px, `currentColor`), so it follows the dim
  footer colour and the light/dark theme; in the terminal it is the dim
  word `GitHub`.

### Fixed

- **Recovery and preempt-resume retry transient failures and give up on
  permanent ones.** A lease whose crash recovery failed was marked `lost`
  on the first error, including a busy node's envd timeout, a deadline or
  a capacity refusal that a retry would clear, and the rootfs-probe
  recovery took the same path. Recovery now classifies the failure and
  retries anything that is not permanent — the deliberate inverse of
  `resumeRetryable` — leaving the lease live with no sandbox for the next
  reconcile pass (there is no new state: a recovering lease keeps its
  `running`/`recovered` state). A counted failure is bounded by
  `RECOVERY_RETRY_ATTEMPTS` (default 3) and `RECOVERY_RETRY_WINDOW`
  (default 30m) since the first failure; a substrate capacity refusal is
  a wait for room, so it does not count an attempt (admission refusals
  cannot reach a live lease's recovery, which skips admission) but is
  still bounded by the window; a missing checkpoint build or image is
  permanent and loses the lease at once. Every transient failure emits a
  `recovery_retry` event naming the attempt and the cause, and `GET`
  exposes the pending retry as `recovery: {attempt, of, since}`. The
  lease is marked `lost` with a reason naming the attempts and the error
  when the budget is spent. The recovery budget is keyed by the sandbox
  that failed and dropped whenever the lease gets a new one (restart,
  restore, resume), on recovery success, loss and release, so a stale
  budget can never make reconcile roll a healthy lease back to an old
  checkpoint. Separately, the preemption resume queue retried a
  permanently failing resume every 15 s for ever, each a real
  orchestrator `Create`; it now counts non-admission failures and, after
  `PREEMPT_RESUME_RETRIES` (default 3), marks the lease `lost` with the
  reason and emits a `lost` event. A preempted lease parked for room is
  different: its admission/capacity refusal neither counts nor starts the
  window, and a wait also resets the window origin of any budget a
  counted failure already started, so a long wait for room between two
  counted failures cannot age an intact lease out; it waits for room
  indefinitely and resumes when room appears. A recovery or preempt loss
  that races a release no longer resurrects the released lease or emits a
  late `lost` event (`spoond-dxq`).
- **A lost lease is released automatically once its grace period
  lapses, freeing its owner's quota.** A lease in state `lost` was never
  released unless its owner deleted it: it kept holding the owner's
  concurrent-lease slot forever, so
  an owner who had moved on could not create a replacement. The GC pass
  now releases a lost lease past its grace period — the same 7-day
  persistent / 1-day otherwise window its snapshots already kept
  (`GC_LOST_GRACE_PERSISTENT` / `GC_LOST_GRACE`) — through the normal
  release path, so quota, the admission queue wake-up, snapshot
  retention, job cleanup and events all happen; the release carries the
  reason `lost_expired` on the lease's `released` event. It is idempotent
  and logs the lease id, owner and age. `DELETE /api/leases/{id}` still
  frees quota immediately for an owner who wants it sooner.
- **Every orchestrator gRPC call is bounded, with gRPC keepalive.** The
  E2B client had no per-call deadline and no HTTP/2 keepalive, and the
  background sweeper called `Create`/`Pause`/`NodeInfo` on the
  service-lifetime context. One hung RPC wedged the sweep goroutine —
  TTL release, held rules, pool refill and `pruneJobs` all ran
  serially behind it — held the lease's busy flag (so every operation on
  it answered `409`) and the snapshot limiter. Each RPC now runs under
  its own timeout: create/resume, pause and checkpoint default to 5 min,
  delete to 2 min, `NodeInfo`/exec-control to 10–30 s, and everything
  else (`List`, `Update`, `SetDraining`, template builds) to 30 s. Exec
  keeps its own per-request timeout. The defaults are configurable via
  `E2B_CREATE_TIMEOUT`, `E2B_PAUSE_TIMEOUT`,
  `E2B_CHECKPOINT_TIMEOUT`, `E2B_DELETE_TIMEOUT`,
  `E2B_NODEINFO_TIMEOUT` and `E2B_CONTROL_TIMEOUT` (a Go duration or
  seconds). The client sends HTTP/2 keepalive pings (5 min, 20 s ack
  timeout) so a dead connection is retired. On the service side,
  every background sweep stage runs under `SWEEP_TIMEOUT` (default
  15 min), so a substrate that ignores its context still frees the loop
  and the lease's busy flag. A test with a fake that blocks forever
  pins that the per-call bounds release the caller, the sweep moves on,
  the busy flag clears and the limiter is not stuck (spoond-j3a).

## [2.7.0] - 2026-10-07

Named snapshots (#83): save a lease as a named, versioned snapshot,
start new leases from it, and manage the catalog with `spoondctl
snapshot` and `spoondctl create --snapshot`. A restricted allowlist
that lists domains keeps its LAN IPs reachable, an undrain retries
transient resume failures, snapshot writes are paced, and a lease whose
root disk dies is detected and recovered. Built-in defaults are now
generic: deployment-specific hosts and addresses come from the
environment, and operators must set them (see
[`deploy/PRODUCTION-ENV-2.7.md`](deploy/PRODUCTION-ENV-2.7.md)). Store
migrations 0017 and 0018 are additive; the grid package is unchanged
since 2.6.7.

### Added

- **Named snapshots (2.7, #83): save, list, show, delete.** A lease can
  be saved as a checkpoint build with a name and a version that outlives
  it: `POST /api/leases/{id}/snapshots` checkpoints the lease, scrubs the
  `/run/secrets` files it staged, inserts the version, applies retention
  and emits a `snapshot_saved` event. `GET /api/named-snapshots`
  (`?prefix=`) lists the caller's names with `in_use` and `stale`;
  `GET`/`DELETE /api/named-snapshots/{name}[@v]` show and delete one
  (`?force=1` overrides the live-lease `409`); `PUT` sets a name's
  retention. Saves are idempotent by an `(owner, name, idempotency_key)`
  key, with an in-memory in-flight/failed state and a
  `?idempotency_key=` lookup; errors carry machine-readable codes
  (`save_in_progress`, `secrets_in_use`, `lease_busy`, `kept_budget`,
  `snapshot_limit`, `snapshot_in_use`, `not_found`). Limits: `MAX_NAMED_SNAPSHOTS` names per owner and the
  owner's `max_kept_bytes`; `SNAPSHOT_KEEP_VERSIONS` versions per name
  (never dropping one a live lease started from). The pre-checkpoint
  scrub clears the whole `/run/secrets` directory through the guest, so
  a secret staged before a backend restart is removed too and a
  directory that is not empty aborts the save with `scrub_failed`; a
  per-lease secrets gate serialises a save against exec and job secret
  staging. A version number is never reused after a delete — the name's
  high-water mark survives even deleting the whole name — a replay
  reaches a committed key from any lease (even a released one), and the
  `/run/spoond/last-save` marker and create-time re-stage survive a
  client disconnect. A save after a backend restart drops the source
  lease's create-time secrets (the guest files go, and the backend no
  longer knows the values) and logs it. Named builds are GC
  roots, and two gauges (`spoond_named_snapshots`,
  `spoond_named_snapshot_bytes`) report the catalog. Every path that
  (re)creates a guest now writes `/run/spoond/lease-id` (`0644`) beside
  `/run/spoond/generation`, and a save writes a `/run/spoond/last-save`
  marker on the source; all are written atomically. The
  `leases.snapshot_build_id` column (migration 0017) carries the version
  a lease started from, indexed by migration 0018. See
  [docs/api.md](docs/api.md). `spoondctl` and the conformance case
  follow.
- **Named snapshots (2.7, #83): spoondctl and conformance.** `spoondctl
  snapshot save <lease> <name> [--key K] [--keep N]`, `snapshot ls
  [prefix]`, `snapshot show <name[@v]>` and `snapshot rm <name[@v]>
  [--force]` manage the catalog, and `spoondctl create [image]
  [--snapshot <name[@v]>]` starts a lease from one; the `ctl` SSH
  control plane carries the same verbs (`create`, `snapshot`). The
  `S7_NamedSnapshots` conformance case exercises the save, a replay, a
  start-from (marker, the new lease id, `/run/secrets` and the guest
  clock within 1 s of the host), retention with `keep: 1`, the in-use
  delete `409` and the `204` delete. See [docs/ctl.md](docs/ctl.md).
- **Named snapshots (2.7, #83): start a lease from a snapshot.** A lease
  create accepts `"snapshot": "name"` or `"name@v"`: it starts from the
  version's build rather than the image's current build, with `image`
  optional (when given it must match the version's image, else `400
  image_mismatch`). The version is resolved owner-scoped (`404
  not_found`), never served from the warm pool, and its `memory_mb` is
  the quota and admission charge. `grantLease` takes a build override, so
  quota, class, `wait`, pool bypass, the integrity probe, create-time
  secrets, `writeGeneration`, the lease-id file, `countImageUse` and the
  `created` event all follow the normal path; the create response and
  `GET /api/leases/{id}` carry
  `"snapshot":{"name","version","build_id"}`, and the `created` event
  says `started from snapshot <name>@<v> in <dur>`. A build that cannot
  start on this host (missing files, or a saved
  envd/firecracker/orchestrator that differs from the host's) answers
  `409 cannot_start` with
  `snapshot <name>@<v> cannot start on this host (<cause>); save it
  again` and no retry loop. A start holds its build in an in-memory
  refcount from resolve to the lease row, so delete-in-use, retention
  and the GC treat it as live while the create is in flight. Retention
  and delete now see real live leases through
  `leases.snapshot_build_id`: a version spared because a
  live lease ran from it is dropped once that lease is released. The
  copy-side `/run/spoond/lease-id`, `/run/spoond/generation` and
  `/run/spoond/started-from` markers are written atomically before the
  create answers and before any exec the API runs in it; a failed
  copy-side write deletes the sandbox and fails the create rather than
  handing out a copy that cannot tell source from copy. The copy
  starts from a scrubbed `/run/secrets`, so it holds only its own
  create-time secrets. See
  [docs/api.md](docs/api.md).
- **Rootfs liveness probe (spoond-5ca).** The kernel NBD connections
  backing a guest's root disk can die (a host disk stall past the kernel
  ceiling); the guest then answers `Input/output error` on every
  uncached read while spoond keeps its lease `running` forever. Every
  `ROOTFS_PROBE_SECS` (default `120`, `0` disables) each running lease
  now runs a cheap exec that reads one block of its root block device
  with `O_DIRECT` at a random offset. Three consecutive failures (an
  I/O error, or the exec failing at the transport) treat the sandbox as
  crashed: spoond emits a `lost` event with detail `root disk unreadable
  (I/O errors)`, deletes the dead sandbox and runs the crash-recovery
  path (from the last checkpoint, or `lost`). Busy leases are skipped,
  a drain and a probe-triggered recovery are mutually exclusive, a
  recent successful exec (exit `0`) skips the probe, and a pass in
  which every probe fails at the transport is treated as the
  orchestrator being unreachable (logged once, no action). New metrics
  `spoond_rootfs_probe_failures_total` and `spoond_rootfs_dead_total`.

### Changed

- **Built-in defaults are generic; deployment-specific hosts and LAN
  addresses are settings.** No code carries a maintainer hostname or
  address any more. Operators must set the new variables (the generic
  defaults keep localhost/example.com only):
  - `SPOOND_GUEST_DNS_ADDR` (backend, image build, live test) — the
    guest DNS resolver; granted on port 53 and baked into the guest
    image by `images/guest/spoond-guest-init`. Empty = no resolver
    allowance and the image keeps its own `resolv.conf`.
  - `SPOOND_PROXY_HOST_SUFFIX` (backend) — the wildcard hostname suffix
    the HTTP proxy routes; default `.sandbox.example.com`.
  - `HOST_GUEST_SERVICE_ADDR` / `HOST_GUEST_SERVICE_PORT` now also
    default the gateway's `SHELLY_BINARY_URL` and `LLM_GATEWAY_URL`
    (default `http://127.0.0.1:8891/...` when unset).
  - `SPOOND_GATEWAY_HOST`, `SPOOND_CTL_HOST` default to
    `sandbox.example.com`; the gateway `sessionEnv` host address uses
    `HOST_GUEST_SERVICE_ADDR` (default `127.0.0.1`).
  - `SPOOND_SSH_CONNECTION_ADDR` (gateway) — the server address
    reported in `SSH_CONNECTION`; defaults to
    `HOST_GUEST_SERVICE_ADDR`. Single-host deploys need not set it.
  - `deploy/e2b/orchestrator.env` ships `NODE_ID=node1` instead of the
    maintainer's host. `NODE_ID` is required and is the orchestrator's
    `ServiceInfo.ClientId` and telemetry host id.
  - `METRICS_SERVER_NAME` (dashboard) has no hostname default: unset
    means no explicit TLS server name.
  - `FORGEJO_URL`, `REPO_BASE_URL` (runner) have no hostname default;
    `FORGEJO_URL` is now required and `REPO_BASE_URL` must be set for
    `actions/checkout` (a missing value fails the checkout step
    clearly).
  - `NETWATCH_TARGETS` (netwatch) and the integration tests' LAN
    targets are required/configurable instead of baked-in addresses.
  The exact values a deployment needs to keep its pre-2.7 behaviour are
  listed in
  [`deploy/PRODUCTION-ENV-2.7.md`](deploy/PRODUCTION-ENV-2.7.md).
- **Snapshot writes are paced, one at a time.** Every call that makes
  the substrate write a memory snapshot — `Pause` (hand suspend, idle
  sweep, held-lease idle/pressure rules, preemption, drain, restart's
  pause leg) and `Checkpoint` (on demand, periodic, clone/fork, keep)
  — now goes through one process-wide limiter. The 2026-10-06 incident:
  a burst of pauses saturated the host disk, the orchestrator's
  NBD server could not answer guests' rootfs requests inside the
  kernel ceiling, and every guest on the stalled devices lost its root
  disk (permanent EIO). `SNAPSHOT_WRITE_CONCURRENCY` (default `1`;
  `0` = unlimited, the old behaviour) is the width. The admin drain
  pauses through its own `DRAIN_SNAPSHOT_CONCURRENCY` (default `2`), so
  a planned orchestrator restart can finish a batch inside the unit's
  `TimeoutStopSec`. Waiting is bounded by the caller's context (an API
  caller only sees added latency), a write that waits 5 s or more logs
  one line (`snapshot write waited 41s behind 1 other`), and the new
  `spoond_snapshot_writes_in_flight` gauge and
  `spoond_snapshot_write_wait_seconds` histogram expose the pressure.
  The idle sweep and the held-lease rules suspend at most one lease per
  tick while the limiter is busy and retry the rest next tick.

### Fixed

- **A restricted allowlist that mixes an IP and a domain keeps the IP
  reachable.** The guest's `allowed_cidrs` gained the DNS fallback as a
  bare `8.8.8.8` whenever the allowlist named any domain; the
  orchestrator's layer-2 egress decision parses `allowed_cidrs` with
  `net.ParseCIDR`, so that one entry made every connection that reached
  the loop — including an allow-listed private IP with no matching SNI —
  fail with a TLS EOF (`curl` 000; `--resolve` did not help). The
  fallback is now `8.8.8.8/32`. On the orchestrator side (fork patch
  `fix/private-allowance-domain-path`), a domain that resolves to an
  allow-listed private address is accepted on the SNI path, so an
  allow-listed LAN name works together with an allow-listed LAN IP;
  everything not explicitly allowed is still refused. The fallback is a
  **public** resolver, so with a private guest resolver configured
  (`SPOOND_GUEST_DNS_ADDR`) it is dropped entirely rather
  than added on every port; a deployment without one keeps it. The
  substrate no longer appends into the caller's `allowed_cidrs` slice,
  and a bare IPv6 address in an allowlist gets `/128` instead of `/32`.
- **An undrain no longer loses leases to a transient envd start/sync
  timeout.** After an orchestrator restart, undrain resumed
  five of eight drained leases and marked the other three `lost` with
  `syncing took too long`: it resumed all of them at once (an I/O and
  memory spike restoring 8 × 4 GiB snapshots) and made a single resume
  attempt before giving up. `POST /api/admin/undrain` now resumes at most
  `UNDRAIN_CONCURRENCY` leases at a time (default `2`), and a resume that
  fails with a retryable envd/start error ("syncing took too long", a
  context deadline, envd init) is retried `UNDRAIN_RESUME_RETRIES`
  (default `2`) times with a short backoff before the lease is marked
  `lost`; a permanent failure (a missing image or build) is not retried.
  The response's `failed` entries and the per-lease log lines report how
  many attempts were made (spoond-urm).

## [2.6.7] - 2026-10-07

Several TLS certificates per listener with hot reload, so the host can
serve the names its ACME client issues one certificate each (Caddy) and
pick up renewals without a restart; exec env out of the guest command
line; a quieter dashboard. No schema change; the `grid` package is
unchanged since 2.4.0.

### Added

- **Several TLS certificates per listener, reloaded on change.**
  `TLS_CERT`/`TLS_KEY` (lease API) and `DASH_TLS_CERT`/`DASH_TLS_KEY`
  (dashboard) accept comma-separated lists of equal length, paired by
  position; the client's SNI picks the certificate (exact or one-label
  wildcard) and the first pair is the default. The files are re-read
  every minute, so a renewed certificate is served without a restart; a
  pair caught half-written (new certificate, old key) keeps serving the
  previous one until both files match. `spoond doctor` checks every pair
  and warns within 14 days of expiry. This lets a host serve names that
  an ACME client issues as separate certificates (Caddy issues one per
  name).
### Security

- **Exec env no longer appears in the guest command line.** Per-request
  env values (tokens, deploy keys) used to be inlined as `export
  'K'='V';` into the `bash -c` argv, where any process in the guest
  could read them from `/proc/<pid>/cmdline`. They now travel in
  `ExecRequest.Env` and are set in the process environment by envd; the
  argv carries only the command and any `cd <cwd> &&` prefix. The CI
  runner's checkout likewise passes its `GITHUB_TOKEN` header in the
  exec env instead of prefixing `git clone` with it.

### Fixed

- **A create refused while the node drains says when to retry.** The
  `503 draining` answer (during a planned orchestrator restart) now
  carries `Retry-After: 30`, like the burst-reserve and preemption 503s.

### Changed

- **Dashboard attention strip: only what needs a person.** A held
  lease the idle or pressure rule suspended no longer raises a row: it
  resumes on its next use, and the leases table already shows it
  suspended. A held lease whose hold lapsed keeps its row (`hold lapsed
  3h00m ago - renew it`), now with its owner. Lost leases are named with
  their owner (`lost lease e151d2653d (honey) - … its owner should delete
  it`), up to three, then one counting row.
- **Dashboard capacity panel: the lease counts on one line,** with the
  burst share inside the running count: `8 leases · 8 running (3
  burst) · 0 suspended · 0 lost` (`susp` when the full word does not
  fit). The burst count no longer drops onto a line of its own.
- **Dashboard events panel: the detail gets the room.** The type and
  subject columns are as wide as the events shown need (the subject at
  most 24 characters, cut with `…`), instead of fixed 14 and 32.
- **Dashboard header: a title and a gutter.** The legend line and the
  `═` rule under the title are gone; a blank row separates the title
  from the panels. Every state is spelled out where it is shown, and
  the holder column's header explains its two marks (`◆ held · ◉
  lapsed`).
- **Dashboard header: no orchestrator version.** The title line reads
  `SPOOND · host · version · up …`; the substrate's version
  (`e2b 0.16.1`) is gone from it.

## [2.6.6] - 2026-10-06

The GC finds orphan build directories (dry run by default, quarantine
before delete); image builds stop leaving dangling images. No schema
change; the `grid` package is unchanged since 2.4.0.

### Fixed

- **The snapshot disk leak from orphan build directories.** The catalog
  GC only walked builds rows, so it never saw two kinds of directory
  that piled up under `E2B_TEMPLATE_STORAGE_PATH`: a build spoond marked
  `deleted` at creation (an abandoned pause or checkpoint whose memory
  file the orchestrator finished writing seconds later, so the directory
  reappeared after the delete), and a directory spoond never recorded
  (an image build's intermediate layers, or a build whose catalog insert
  failed) — about 91 GiB in 84 directories on vm2. Each GC pass now
  reaps the direct child build directories the catalog does not need,
  keeping any directory a catalog build, lease, kept build, sandbox or
  image still names, any directory reachable from those through a
  `memfile.header` / `rootfs.ext4.header`, and any changed within
  `ORPHAN_MIN_AGE_SECS` (default 1 h). `ORPHAN_REAP` chooses the policy:
  `dryrun` (default) logs `gc: would reap orphan <id> (<size>)` and
  changes nothing, `quarantine` moves the directory to
  `<storage path>/../quarantine/<id>` with a marker and restores or
  finally deletes it (after `ORPHAN_QUARANTINE_SECS`, default 24 h), and
  `off` disables the reap. `GC_DELETE` no longer governs this path. The
  reap counts `spoond_gc_orphans_reaped_total` and
  `spoond_gc_orphan_bytes_reaped_total` and rides the pass's `gc` event.
- **`spoond images build` no longer leaves a dangling image per build.**
  Re-tagging `:latest` left the previous build's image behind in the
  local Docker store (about 1.5–1.9 GB per worker image rebuild); after
  a successful push the build now prunes dangling images (only those;
  the registry keeps the canonical copy). Best effort: a failed prune
  never fails the build.

## [2.6.5] - 2026-10-06

A user's guarantee no longer drifts to all-burst; restore names its
checkpoint's time. No schema change; the `grid` package is unchanged
since 2.4.0.

### Added

- **The restore response names when its checkpoint was taken:**
  `build_created_at` (RFC 3339) beside `build_id`.

### Fixed

- **A user's guarantee no longer drifts to all-burst.** A lease's class
  was decided once, at admission, against the owner's *total* running
  memory, burst leases included. Once an owner had burst leases every
  new lease came in burst, and as the older guaranteed leases went
  nothing moved the survivors back; a busy owner ended up with every
  lease burst and preemptible. A lease is now guaranteed while the
  owner's guaranteed leases plus it fit `guaranteed_mib` (burst leases
  do not count), and running burst leases are promoted, oldest first,
  when the guarantee has room again (on a release or pause of a
  guaranteed lease, and every 15 s), with a `promoted` event. A lease
  created with `"burst": true` stays burst.

## [2.6.4] - 2026-10-06

Dashboard only. No backend change, no schema change; the `grid` package
is unchanged since 2.4.0.

### Fixed

- **Leases table cells never run together.** Every cell keeps a space
  before the next and ends in `…` when cut, so a long owner no longer
  runs into the state (`test-consu▶ running`). The age column fits
  `10h37m`. Lease ids are cut as prefixes, and in a narrow window the
  policy shows `inet`/`rstr` under a `net` header instead of a word cut
  mid-way.

## [2.6.3] - 2026-10-06

Dashboard only: readable lease states. No backend change, no schema
change; the `grid` package is unchanged since 2.4.0.

### Changed

- **Dashboard: the lease state is always spelled out.** The state cell
  no longer carries cryptic suffixes (`running·b`, `suspended·p`,
  `suspended·i`). The glyph carries the run state (▶ running, ‖
  suspended, ■ lost, ⭘ recovered) and the word spells it out with its
  qualifier: `running, burst`, `suspended, burst`, `preempted`,
  `idle-suspended`. In a narrow window the word steps down through
  fixed shorter forms (`run, burst` → `running` → `run`), dropping the
  qualifier before the state and never cutting a word mid-way.

## [2.6.2] - 2026-10-06

Background jobs polish. No schema change; the `grid` package is
unchanged since 2.4.0.

### Fixed

- Background job starts use a per-lease lock, so starts on different
  leases run concurrently instead of serialising on one global lock.
- Signalling a job whose lease is suspended answers 409 "lease is
  suspended; resume it first" instead of reaching the substrate.
- Event-detail cuts and the stderr tail cut land on rune boundaries, and
  invalid UTF-8 from the guest is replaced with U+FFFD.
- Job retention compares parsed ended_at times instead of RFC3339
  strings, so values with and without fractional seconds order correctly.

## [2.6.1] - 2026-10-06

More detail in the events panel (#132 part 2). No schema change; the
`grid` package is unchanged since 2.4.0.

### Added

- **Events panel details: release reasons, grant and checkpoint
  durations, and GC events (#132 part 2).** The `created` event gains
  how long the grant took (measured from admission start, so a queued
  create reports its admission time, not its wait): `granted from image
  py-base in 61 ms`. The `checkpointed` event gains the duration and the
  short build id: `540 ms · build 9e1f2ab3…`. `DELETE /api/leases/{id}` (and
  the `/api/sandboxes` alias) accepts an optional `reason` (query
  `?reason=` or JSON body `{"reason"}`, at most 120 printable
  characters, sanitised like comments) that its `released` event
  carries, else the event keeps `deleted through the API`. The CI runner
  sends the job's outcome and duration as that reason (`ci job 3609 ✓
  11m02s`, `ci job 3604 ✗ 4m10s`, `ci job N cancelled` on drain), and a
  catalog GC pass that deleted builds emits one lease-less `gc` event
  (`N builds deleted · X GiB freed`) which reaches the all-leases stream
  but never a per-lease one. The dashboard colours `gc` events ok and a
  failed CI release warn.

## [2.6.0] - 2026-10-06

Background exec jobs (#135), an owner-scoped crash test for recovery
suites, and the dashboard's colour roles (#132 part 1). Store migration
16 adds the `lease_jobs` table. The `grid` package is unchanged since
2.4.0.

### Added

- **Crash test.** `POST /api/leases/{id}/crash-test` (and the
  `/api/sandboxes/{id}/crash-test` alias) runs one lease through the
  crash-recovery path on demand: it deletes the lease's sandbox as a
  crash would (without releasing the lease), drops the sandbox row and
  runs the same per-lease recovery the startup reconcile runs — from
  the lease's last checkpoint (`generation` +1, state `recovered`) or
  `lost` with no checkpoint. The response is
  `{"id","result":"recovered"|"lost","generation","state"}`. A
  `crash_test` lease event (detail `crashed by its owner` or `crashed
  by an admin`) precedes the recovery event. It is off unless the host
  sets `CRASH_TEST=1` (the route answers `404` otherwise), so an
  ordinary user can drive a crash suite without an admin token. The
  lease's owner may crash their own lease and an admin any lease;
  anyone else gets `404`. `409` busy or suspended, `410` already lost,
  `404` unknown or released. The recovery runs to the end even if the
  client hangs up. Built for on-demand crash testing (Honey's M3
  suite); the reconcile code is factored so the startup pass and this
  endpoint share one function. No effect on other leases, the warm pool
  or any release path. The conformance case X1 runs with
  `CONFORMANCE_CRASH_TEST=1`.

- **Background exec jobs (2.6, #135).** `POST
  /api/leases/{id}/exec` (and the `/api/sandboxes` alias) accepts
  `"background": true`: the command runs in the caller's lease and the
  request answers `202 {"job_id","started_at"}` as soon as it has
  started (`timeout` is ignored). The guest records the outcome itself
  under `/var/lib/spoond/jobs/<job_id>/` (`stdout`, `stderr`, `pid` and
  an atomically written `rc`), detached from the envd stream, so a
  backend restart does not kill the job and the files — not the stream
  — are the source of truth. The `env` object rides the substrate's
  start request, never the job directory, the record, the logs or an
  event. Jobs are tracked in a new `lease_jobs`
  table (**migration 0016**), deleted with their lease and pruned after
  `JOB_RETENTION_SECS` (default 7 days), when the guest's job directory
  is removed too; at most
  `MAX_RUNNING_JOBS_PER_LEASE` (default 16) run per lease (`429` past
  it). Read them with `GET …/jobs` (newest first) and
  `GET …/jobs/{job}` (record plus the last 64 KiB of output, or
  `?wait=<seconds>` to long-poll to the exit), follow output with
  `GET …/jobs/{job}/output?stream=&offset=&limit=`, and signal a job
  with `POST …/jobs/{job}/signal`. `job_started`, `job_exited` and
  `job_lost` ride the lease event stream, the lease object gains a
  `jobs` summary, and a lease with a running job counts as active for
  every idle rule. New metrics `spoond_jobs_running` and
  `spoond_jobs_exited_total{result}`; the dashboard events panel shows
  `job_exited` lines.

### Changed

- **`spoond dash` colour roles.** The dashboard's palette now follows
  the mockup on the black background: cyan titles and lease ids, blue
  run state, green ok, amber warn, red bad, violet owners, and each
  event type coloured by its kind — the lifecycle in cyan, a lease put
  aside in amber, one lost or timed out in red, anything else dim.
  Meters draw an amber tick at their warning level, and `spoond top`
  maps the same roles to ANSI colours. Colour never carries a state
  alone: every coloured state keeps its glyph or word.

## [2.5.2] - 2026-10-06

### Fixed

- **A guaranteed lease no longer preempts to protect the burst reserve.**
  The reserve (`BURST_RESERVE_MIB`) keeps room free for guaranteed work:
  burst leases may not dip into it, but a guaranteed lease may. Since
  2.4 a guaranteed admission instead treated the reserve as its own
  floor and suspended burst leases whenever free memory was under
  reserve + its size, so a short CI job could pause a burst worker on a
  host with plenty of room. It now preempts only when its own memory is
  not free.
- **Dashboard attention strip: only what needs a person.** The
  held-lease row showed any held lease with a `last_action` in the last
  24 h, so since 2.5 (when every pause started recording its action) an
  ordinary preemption, drain or hand suspend stayed on the strip for a
  day, even after the lease ran again. It now shows a held lease only
  while a held-lease rule (idle, pressure or a lapsed hold) has it
  suspended, and clears when the lease runs again; preempted leases keep
  their own row while preempted.

### Removed

- **TLS certificate expiry monitoring.** The dashboard's certificate row
  and `cert` status item and the backend's `tls.cert.30d`/`.7d`/`.1d`
  notifications are gone: watching certificates is the host's IT
  tooling's job (the Gatus example in docs/operations.md does it), not
  spoond's. spoond still serves HTTPS with `TLS_CERT`/`DASH_TLS_CERT`,
  and `spoond doctor` still checks that the configured certificate can
  be loaded and trusted.

The `grid` package is unchanged since 2.4.0. No schema change.

## [2.5.1] - 2026-10-06

### Changed

- **`wait` covers the lease-count cap.** A create refused because its
  owner is at `max_leases` (`429 lease quota exceeded`) now waits like
  the other capacity refusals when it sends `wait`, and is admitted
  once one of the owner's own leases is released; on timeout it gets
  the same `429` plus `waited_ms`. The `queued` event names it `lease
  cap`. The cap's own decisions are unchanged, and a create without
  `wait` answers at once as before. The `grid` package is unchanged
  since 2.4.0.

## [2.5.0] - 2026-10-06

Waiting for admission and per-lease idle reclamation (#129). Store
migration 15 adds a column with a default; both features are opt-in, so
nothing changes until a create sends `wait` or a lease sets
`idle_suspend`. The `grid` package is unchanged since 2.4.0.

### Added

- **Queued admission (#129 part 1).** A create that cannot be admitted
  right now can wait for room instead of failing. `POST
  /api/leases` (and its `/api/sandboxes` alias) gains a `wait` field
  (seconds, default `0` = today's behaviour exactly), capped at the new
  `MAX_ADMIT_WAIT_SECS` (default `900`; `0` disables waiting). Only
  capacity, burst-reserve, cannot-preempt and the `max_mib` refusal
  wait; the lease-count cap, bad requests, auth and unknown images still
  answer at once. The in-memory queue is served in fair-share order
  (the owner furthest under its `guaranteed_mib` first, then FIFO; the
  `queued` event carries the position, and `GET /api/leases/queue`
  lists waiting creates with position, age and reason), with
  each wake-up admitting every queued create that fits, so a smaller one
  may pass a larger one. Admitted creates carry `waited_ms`; a timeout
  answers the original refusal with `waited_ms`; a client that goes away
  drops its ticket; a drain answers every queued create `503 draining`.
  The lease id is allocated when the create is queued, and the queue
  emits `queued`/`timed_out` events. New metrics
  `spoond_leases_queued_oldest_seconds`, `spoond_admit_wait_seconds` and
  `spoond_admit_timeouts_total`, and the dashboard shows the oldest
  ticket's age. No schema migration.
- **Per-lease idle reclamation (#129, part 2).** A persistent lease can
  be suspended after a period without activity, freeing its hugepages
  and memory while keeping everything for the next use. Each lease
  carries an `idle_suspend` threshold in seconds (`0` = never;
  `60`–`604800` = suspend after that long without activity; omitted =
  the host default `IDLE_SUSPEND_DEFAULT_SECS`, itself `0` = never), set
  on `POST /api/leases` or with `PUT /api/leases/{id}/idle-policy`
  (owner or admin) — **migration 0015** adds `idle_suspend` with a
  default of `-1` (host default), so existing leases are unchanged. A
  non-zero value needs a persistent lease (`400` otherwise). The idle
  sweep suspends through the normal pause path (the generation does not
  change), shares preemption's snapshot-disk floor
  (`PREEMPT_DISK_FLOOR_PCT`), records `last_action`
  `idle_suspend/suspend_idle`, emits an `idle_suspended` event and
  counts in `spoond_idle_suspends_total`; a lease with a non-zero
  effective `idle_suspend` is reclaimed on it alone, so the plain
  `IDLE_TIMEOUT_SECS` sweep and held rule 1 skip it, and rules 2 and 5
  may later release it if it stays idle-suspended and untouched. The
  **next call resumes it**: exec, stream, the files API and guest port
  dial on an idle-suspended lease resume it first (admission, class and
  quota apply) and then serve the call, while any other suspension
  keeps the `409`. Clone and fork copy the source's value, and the
  dashboard marks idle-suspended rows (`‖ suspended·i`).

### Fixed

- **Free hugepages never wrap.** A node reading that briefly reports
  more hugepages used and reserved than it has (sandboxes starting or
  stopping) made the unsigned free figure wrap to an enormous value, so
  admission, the held pressure rule, the free-hugepages gauge and
  `spoond doctor` saw a nearly empty node. Free memory now saturates at
  0 (`NodeInfo.FreeHugepageBytes`).
- **Room freed by a release or pause is seen at once.** Since 2.4,
  admissions take their memory from a cached node reading (refreshed
  every 15 s); only a preemption gave memory back to it. A release or
  any other pause now credits the reading too, so a burst or guaranteed
  admission right after a release is no longer refused on the stale
  figure until the next refresh.

## [2.4.0] - 2026-10-06

Capacity: memory quotas, guaranteed and burst leases, and preemption of
burst leases by suspend (#128). Store migrations 12–14 add columns with
defaults: a user without a `guaranteed_mib` keeps every lease
guaranteed, so admission behaves as in 2.3 until quotas are set.

### Changed

- **License: BSD 3-Clause.** spoond is licensed under the BSD 3-Clause
  License from this release; 2.3.3 and earlier stay available under
  Apache-2.0. E2B's API definitions (`substrate/e2b/proto/`) keep their
  Apache-2.0 license, and NOTICE lists every third-party file and its
  license. Datastar's MIT license text now ships beside the vendored
  build.

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
  class, lower preempted first. `class` and `priority` ride every lease row
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
  (**migration 0014** adds `preempted_at`) and emits a `preempted` event (`for a guaranteed lease of <owner>`).
  A **disk floor** guards the pause: preemption stops while the snapshot
  disk would fall under `PREEMPT_DISK_FLOOR_PCT` (default `15`), and a
  guaranteed admission that cannot preempt answers `503` `capacity:
  cannot preempt (snapshot disk low)` with `Retry-After: 30`. A
  background **resume queue** runs every 15 s and resumes preempted
  leases, oldest preemption first, when they fit again, clearing
  `preempted` and emitting `resumed` with detail `after preemption`;
  the same path serves a client's explicit resume. Any other path that
  runs the lease again (restore, cold restart, recovery) or loses it
  ends the preemption too, and the held-lease rules never release a
  preempted lease. New metrics
  `spoond_preemptions_total` and `spoond_preempted_leases`, a dashboard
  state mark (`‖ suspended·p`) and an attention-strip row name
  them.

### Fixed

- **Dashboard: no scrollbar beside the grid.** The grid had its own
  vertical scrollbar whenever its content was a fraction of a pixel
  taller than its box (2.3.3 hid only the page's). It no longer scrolls
  vertically, and a narrow window scrolls it sideways without a bar.

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
