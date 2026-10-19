# Changelog

Notable changes to spoond, newest first. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). In the 2.0
section, every entry names where it comes from: a commit (`<short-hash>`),
a unit of the E2B substrate spec (`U01`–`U13`, under
`docs/plans/2026-09-30-e2b-substrate/`), or a fixed decision (`D1`–`D17`,
in that spec's `00-README.md`). The earlier-releases section is
summarised from README "Status".

## [Unreleased]

## [3.10.1] - 2026-10-18

### Changed

- **Dashboard: hugepages, snapshot disk and root disk read "used of
  total"**, like memory ("35.2 of 104 GiB"), instead of "GiB free".
  Numbers of 100 and over drop their decimal so the text fits beside
  the bar.

### Fixed

- **Leases lost under hugepage exhaustion (spoond-90ia).** On
  2026-10-17 two running leases faulted in memory faster than the 15 s
  memory-floor check and died of hugepage exhaustion 4 s before the
  next check would have paused one, after give-back had handed an 8 GiB
  lease back and forth four times in 80 minutes. The floor loop now
  checks every second while free memory sits under twice
  `MEMORY_FLOOR_MIB`. Give-back stays quiet for 15 minutes after a floor
  take-back (2 after an admission take-back, as before), counts memory
  running leases were granted but have not touched yet as spoken for,
  and holds a lease taken back again after a give-back for 15 minutes,
  doubling with each re-take up to 4 hours (a lease that ran an hour
  starts afresh).

No API, schema or configuration change.

## [3.10.0] - 2026-10-17

### Added

- **Give-back: spoond resumes the leases its own take-back paused
  (spoond-aeo6).** A pause spoond makes to protect itself (the
  fair-share admission take-back or the `MEMORY_FLOOR_MIB` loop, both
  `suspend_reason` `take_back`) is now spoond's to undo: a floor-loop
  tick that finds the box at or above the floor resumes one such lease,
  the longest-paused first and strictly in order, once the lease's
  memory plus the floor plus `GIVE_BACK_MARGIN_MIB` (default 1024) is
  free and the overcommit cap has room, no create is waiting for
  admission, the node is not draining, and no take-back has run for two
  minutes. The resume takes a holder's path and never takes another
  lease back for its room; a failed resume leaves the lease paused and
  is retried after 10 minutes. Pauses a caller made (hand suspend,
  `idle_suspend`, the drain) still resume only on the caller's next
  call. `GIVE_BACK=off` disables it. New: `lease.resumed` detail
  `give_back: ...`, the `give back:` log line and
  `spoond_memory_give_backs_total`. Before this, a taken-back lease
  with nobody to call it (a pool worker, a CI job) stayed paused until
  someone touched it.

### Fixed

- **spoond-runner waits out a `box_full` create (spoond-49od).** Every
  CI job shares the runner's identity, so a burst of jobs can make it
  the box's biggest borrower, and fair shares answers 429 `box_full` at
  once. The runner treated that as a failed job; it now retries it like
  a full node's 503 (Retry-After, bounded by the job's timeout). Other
  429s (quota codes) still fail at once.

## [3.9.1] - 2026-10-14


### Fixed

- **Warm-pool sandboxes count toward the overcommit cap, and the
  low-memory floor sheds them first (spoond-41tk).** A pool VM holds
  its hugepages around the clock, but the cap (`MEMORY_OVERCOMMIT`)
  saw only leases: a full box with `POOL_SIZE` set could crash a VM the
  moment it touched its own memory again. The pool's memory is now part
  of the capped and committed total (`spoond_memory_committed_mib`
  included), stocked per sandbox and re-seeded across a restart; a
  pooled sandbox handed to a lease is counted once, as the lease. A
  warm-pool fill stops (logged, retried by the next refill) rather than
  stock a VM that would put the box over the cap or leave less than the
  floor headroom free — the headroom is kept even with the floor loop
  off, since a pool fill parks a VM for good — and never takes a lease
  back. Below the `MEMORY_FLOOR_MIB` floor the tick now drops one
  newest pooled sandbox first (the newest member of any image, its
  guest deleted on the substrate and its rows cleared exactly like the
  drain's pool trim) and returns: only an empty pool falls through to
  pausing a lease, so under a standing breach the box sheds its idle
  pool one VM per tick before anybody loses work.
- **A grant served from the warm pool is admitted as the memory it
  adds (also spoond-41tk).** Pool VMs count toward the cap now, and
  grantLease still admitted before its pool pop, so a pooled grant was
  charged twice — take-back paused a lease for it, and on a box at its
  cap the grant was refused outright (`lease memory would be 2048 MiB
  over the overcommit cap`), leaving the pool useless exactly when the
  box is fullest. A pooled grant now runs admission only when nothing
  was pooled: a pool hit takes over a VM already counted and is
  admitted with nothing to debit and no take-back; an empty pool
  (or one whose members all failed validation) falls through to the
  ordinary admission for the cold create, take-back and cap included.
  This also removes a double credit the pooled pop could fire (the
  debit refunded at the pop and again through the undo), which read
  the cache up to 2 GiB freer than the box was for up to 15 s.
- **The biggest-borrower `box_full` refusal honours a stocked pool
  (spoond-18xe).** The pooled grant's admission (above) runs only when
  nothing was pooled, so it skipped the biggest-borrower refusal and an
  owner over its fair slice could take a stocked pool VM, the request
  its cold create would have been refused. The refusal is now judged
  before the pool pop on the same take-back view the ordinary admission
  builds: the biggest borrower (over its slice after the request, with
  no other owner further over) is refused `429 box_full` with
  `own_leases` and nothing taken back, debited or popped — the pool
  entry stays stocked — while an owner inside its slice is served from
  the pool as before. No effect at `POOL_SIZE=0` (production): the
  pooled path is the only one that changed.

- **The pooled `box_full` gate refuses only where a cold create would
  be refused too (spoond-fi5v).** The gate added above (spoond-18xe)
  ran before the pool pop, so it fired on the cached node view alone
  and refused an owner whose cold create the box could still have
  served — the request plus the floor headroom still fit in the
  cached free hugepages. The gate now refuses only where a cold
  create would also be refused (cached free hugepages below the
  request plus the floor headroom), so a stocked pool is never
  barricaded by memory the box still has.

- **A pool VM popped for a grant counts as in flight until the grant
  settles (spoond-fi5v).** The pop removed the VM's MiB from the
  pool's committed figure the moment it left the pool, but the lease
  row only counted it at commit, so between the two the overcommit
  cap saw that memory nowhere: a cold create racing the grant could be
  admitted into memory the popped VM was already holding. The popped
  VM's MiB is now raised into the in-flight admission figure
  (`inflightAdmitMiB`) in the same `nodeInfoMu` section the admissions
  use, right after the pop, and subtracted exactly once by the grant's
  undo — a discarded pop counts nothing, a committed one is counted by
  its lease from then on, so the memory is never invisible and never
  double-counted.

- **Warm-pool fills decide and hold under one lock (spoond-fi5v).** A
  fill's overcommit check, free-page check and the take of its own MiB
  ran as separate steps, so a cold admission racing the fill could
  slip between them into memory the fill was about to take. The fill
  now does all three in one `nodeInfoMu` section — from the moment it
  decides to stock, the VM's MiB counts as a promise a racing
  admission sees in the cap — and the hold is released whole when the
  fill stocks or fails.

- **A guest-side OOM is never treated as a wait (spoond-fi5v).** The
  raw `cannot allocate memory` text match spoond-6ayi added beside
  `errors.Is(err, substrate.ErrCapacity)` also caught the same kernel
  text raised from inside a guest — a permanent failure — so a create
  that OOMed its guest was drained, undrained or retried as if the
  host had merely run out of room. `subCreate` already wraps the
  host's ENOMEM as `substrate.ErrCapacity`, so the raw text matches
  are gone from the admission gate (`admitqueue.go`), the undrain rule
  (`admin.go`) and the recovery retry (`retry.go`); the 6ayi `503
  capacity_wait` answer itself is unchanged, and a guest-side OOM now
  fails permanently.

- **A pooled grant that falls through to a cold create runs the
  saturation gate (spoond-fi5v).** A pool hit boots nothing, so the
  saturation check was skipped for the pooled shape — but a pool that
  empties (or fails validation) falls through to a cold create on the
  same request, and that cold create never saw the gate. The
  fall-through now runs `checkSaturation` like every cold create.

- **Host ENOMEM on a create or resume answers `503 capacity_wait`, not
  `500` (spoond-6ayi).** On 2026-10-14 a create whose snapshot load
  failed on host ENOMEM (`failed to start FC: error loading snapshot
  ... mmap memfd: cannot allocate memory`, `failed uffdio copy: cannot
  allocate memory`) answered `500 "failed to grant lease"`, which
  clients read as a permanent failure and do not retry. The
  orchestrator's gRPC error carries no code for it, only text, so
  spoond now recognises the kernel's `cannot allocate memory` message
  (case-insensitive) on every lease-creating substrate call and answers
  it the capacity refusal it is: `503` with `code: capacity_wait` and
  `Retry-After: 30` on every path (create, resume, restart, clone,
  fork, restore, recovery), waitable out with `wait` like any other
  capacity wait, and counted in `spoond_capacity_rejections_total`.
  The failed create's quota reservation and admission debit are undone
  exactly as on any other failed create.

## [3.9.0] - 2026-10-14

### Added

- **Low-memory floor (`MEMORY_FLOOR_MIB`, spoond-l3lq).** Every 15 s
  the backend reads the node's free hugepages fresh and, below the
  floor (default 2048 MiB; 0 disables the check), pauses exactly one
  lease — the least recently used unpinned lease of the owner furthest
  over their fair slice (a single-lease owner over their slice is a
  candidate, an all-pinned top owner is passed over for the next one,
  and busy leases rank last); pinned leases and owners inside their
  slice are never touched, and the lease resumes on its holder's next
  work call. Admission counts the floor as headroom in every
  free-vs-request comparison, so a paused lease resumes only with the
  floor free on top of its own memory. The overcommit cap
  (`MEMORY_OVERCOMMIT`) keeps admission from lending memory out; the
  floor is the running counterpart that sheds load when the free
  pages sit low without a request needing room. The tick stands down
  while draining and on bad node readings; a lease still marked
  `Drained` (an undrain pending on it) is never picked, but the tick
  itself runs on, so a lease stuck `Drained` cannot switch the floor
  off for good. A pause survives a shutdown in flight (the pause's
  snapshot write is never cut in half). A floor nothing can be taken
  back for raises the new `memory_floor` event at most once per 10
  minutes. New metrics `spoond_memory_committed_mib` (set every tick,
  even with the floor off) and `spoond_memory_pressure_takebacks_total`.
  Busy leases (a running job or operation) are passed over, so an
  all-busy top borrower never stalls the loop; a lease returning to
  what the box already held (a take-back that lost to a pin, an
  undrain after a restart) is admitted without the floor headroom.


- **`box_full` lists the requester's own leases (`own_leases`).** The
  memory `box_full` refusal for a requester that is itself the biggest
  borrower (over its fair slice after the request, with no other owner
  further over) now carries the requester's **own running leases, least
  recently used first** under `own_leases`, so the caller can see what
  holds its memory and pause one itself. Each row carries `id`, `name`
  (omitted when untagged), `memory_mb`, `last_active`, `pinned` and
  `busy`. No policy change: nothing new is paused — the pinned/disk
  `box_full` (only pinned leases in the way) answers without the key,
  as before, and the listing is the refusal's only effect. The same
  body shape on every re-admission path (create, suspend, restart,
  clone, fork, checkpoint, resume, restore). A create answered after an
  admission wait stamps `waited_ms` inside the same body.

### Changed

- **Fair shares split the box among active owners only (owner decision
  2026-10-14).** N in the `1/N` slice is no longer the number of
  identities that exist: it counts owners holding a running or paused
  (suspended) lease, plus anyone with an admission in flight — a
  legacy consumer token included, so a requester counts for its own
  first lease. A
  registered identity that never took a lease splits nothing, and an
  owner's slice shrinks to `1/(N+1)` on their first lease. `GET
  /api/fair-shares` and the self-scoped share payloads now carry an
  `active` boolean per owner: every owner is still listed, an inactive
  one shows the slice it would get on starting (`total/(N+1)`) and its
  `ratio` reads `0`, so an idle owner with a stale snapshot is never
  ranked the heaviest borrower. A lost lease never counts, however it
  was suspended before the loss.

## [3.8.1] - 2026-10-14

3.8.1 stops admission from lending out memory a running VM may fault
back in. No breaking changes, no schema change.

### Fixed

- **Admission no longer lends out memory a running VM may fault back
  in (`MEMORY_OVERCOMMIT`, spoond-ybzf).** With free-page reporting a
  VM returns memory its guest freed, and admission counted that memory
  as free, so a full host could crash a VM when it touched its own
  memory again (`uffdio copy: cannot allocate memory`), losing the
  lease. Admission now also caps the memory promised to live leases at
  `MEMORY_OVERCOMMIT` x the hugepage pool (default 1.25; 1.0 means
  never more than the pool). Over the cap, take-back makes room from
  the biggest borrower as usual, and otherwise the request waits
  (`503 capacity_wait`).

## [3.8.0] - 2026-10-14

3.8.0 keeps a durable history of every lease and shows it: a new
`lease_history` table (schema 29) with a journal backfill, a records
panel of all-time stats, and an images panel that sorts by use, folds
past eight rows into a link to a full `/images` page, and shows sizes as
`mem/vcpu`. The dashboard no longer raises a notification for low free
hugepages. `elixir-base` moves to Elixir 1.19.6 / OTP 28.5.0.7 with
PostgreSQL 17. No breaking changes.

### Added

- **The dashboard's `/images` page (spoond-p5s9).** `GET /images` serves
  the dashboard's page shell drawing only the images panel — full frame
  width, every image row uncapped (same sort, no fold), with the total
  row — live-updating over the same SSE stream (`GET
  /stream?view=images`; any other view, or none, is the dashboard). The
  images panel caps its rows at eight: a longer catalog folds into one
  `+N more` row whose live and uses cells sum the hidden images, before
  the total row over every image. The panel sorts by use — live leases
  first, then most recent use (the newest `lease_history` end, a live
  lease counting as now), then lifetime uses, then name — and its title
  reads `images · N · by use`. On the page the `+N more` text is an
  anchor to `/images` and the `/images` page's title links back; in the
  terminal the fold row reads `+N more · spoond images list` (cut to
  fit) and the `/images` footer carries `· images`.
- **Lease history (spoond-15a5).** Each ended lease leaves one row in a
  new `lease_history` table (migration 29): owner, image, size, start and
  end, outcome (released or lost), reason, measured peak, create-to-grant
  latency and its longest nap. `spoond history import` backfills it from
  the backend journal. The records panel reads it.
- **The dashboard's records panel (spoond-y5by).** `spoond dash` gains a
  panel of twelve all-time records — busiest moment, one image at once,
  longest and shortest run, top owner, lifetime lease/hours/GiB-hours
  totals, busiest Pacific day, biggest single-owner fleet, most memory,
  no-losses streak, longest nap and fastest start — computed over the
  backend's `lease_history` table plus the live leases, owner ids mapped
  to names from the identity store. `DASH_RECORDS_EXCLUDE` (a comma
  list of owner names, default `conformance,test-consumer`) keeps test
  leases out of every record; the images panel's `uses` column is
  untouched. The computation is cached for a minute (the 2 s refresh
  reuses it), and a catalog without the table yet shows "no history
  yet". Needs the backend's lease-history change to have data; the two
  latency records read `— (counting from 3.8)` until 3.8 backends start
  writing `start_ms` and `max_nap_secs`.

### Changed

- **Dashboard: no "hugepages past the danger level" notification.** Low free
  hugepages is not something to act on: spoond takes memory back itself, and a
  request it cannot serve raises its own box_full alert.
- **Dashboard images panel: one `mem/vcpu` column** in the leases
  panel's size form (`8G/2V`), in place of separate `vcpu` and `mem`
  columns, and a total row summing live leases and lifetime uses.
- **`elixir-base` moves to Elixir 1.19.6 / Erlang OTP 28.5.0.7 and runs
  PostgreSQL 17.** Built from the exact hexpm tag, so repos pinning those
  versions match. A Postgres server starts at guest boot on
  127.0.0.1:5432 (user `postgres`, password `postgres`, exported as
  `POSTGRES_HOST`/`POSTGRES_PORT`/`POSTGRES_USER`/`POSTGRES_PASSWORD`),
  for Ecto suites that create and migrate their own databases. Python 3
  (stdlib) is included for build-time generators. The default size is
  now 4 GiB and 8 GiB disk; ask for more per lease with `memory_mb`.

## [3.7.2] - 2026-10-13

3.7.2 stops image builds from filling the host's root disk: after a
successful push, `spoond images build` removes the local tagged copy it
just pushed (the registry keeps the canonical one). No breaking changes
and no schema change.

### Fixed

- **`spoond images build` removes its local tagged image after a
  successful push (spoond-07fn).** The build's push left
  `<registry>/<name>:latest` (the build's only local ref) in dockerd,
  where the dangling-only `docker image prune -f` after it never
  reached it — on 2026-10-13 three template-build rounds took the
  host's root disk from 47 % to 62 %, 7 GB of it tagged local images.
  After the digest is read and before the prunes, the build now runs
  `docker image rm` on exactly the ref it just pushed (best effort, a
  failed rm warns like the prunes and never fails the build). The
  registry holds the canonical copy and template builds pull from it,
  so the local copy is not needed after a successful push. Deliberately
  no blanket `docker image prune -a`, which would take out images other
  containers use (the registry's own, the buildkit container's).

## [3.7.1] - 2026-10-12

3.7.1 keeps the host's root disk in view and in check. `/readyz` now
also fails when the root filesystem passes 90 % used, so an uptime
monitor hears about it; image builds prune both docker builders' cache,
by age and with a size cap (`IMAGE_BUILD_CACHE_KEEP`, default 10 GB).
Lease comments refuse control characters. No breaking changes and no
schema change.

### Added

- **`/readyz` watches the host's root filesystem too (replaces the
  soak timer's root-disk watch; owner-approved 2026-10-12).** The
  lease API's readiness report gains a second disk check, `root disk`,
  beside the snapshot disk's: the host's root filesystem (`/`)
  against the same 90 % used danger level, the same 2 s bound on its
  own goroutine, named in the 503 body's `checks` list like every
  other check. When the snapshot store and `/` sit on one filesystem
  (same st_dev) the root check drops itself, so a filesystem is never
  counted twice. This takes over the forkd-era
  `spoond-soak.timer`'s one still-owned catch — the root disk at 94 %
  on 2026-10-11 when docker's image-build cache filled it — so the
  external uptime monitor pages on a filling root disk within a poll
  instead of up to a day later; the timer keeps the soak run's doctor
  and lost-lease counts. The path is overridable only for tests,
  through `E2B_ROOT_FS_PATH`.

### Changed

- **Lease comments refuse control characters (3.7 review nit).** The
  create body's `comment` field and `POST /api/leases/{id}/comment`
  now answer a comment carrying a control character — anything below
  `0x20`, a newline included, and `0x7F` — with `400 comment must not
  contain control characters`, so a terminal escape cannot ride a
  comment into the dashboard's lease line or a notifier's message.
  Both surfaces now read one shared validator (api/comment.go)
  instead of each carrying its own copy of the 512 cap, so they
  cannot drift apart; plain and non-ASCII printable text (`é ✓`)
  still passes, and an empty comment still clears through the comment
  route. No behaviour change for valid input.

- **The deploy units point at the real env-file paths
  (spoond-uzs prerequisite, review finding B8).**
  `deploy/spoond-runner.service` reads
  `EnvironmentFile=/etc/spoond/runner.env` (was
  `/etc/spoond-runner.env`), and `deploy/spoond-backend.service` reads
  `EnvironmentFile=-/etc/spoond/llm.env` then
  `EnvironmentFile=-/etc/spoond/backend.env` — a later file wins, so
  `backend.env` stays the operator's file and may override the optional
  `llm.env` (the per-lease LLM gateway's `LLM_UPSTREAM_URL`/`KEY`
  pair) — with `Environment=BIND_ADDR=127.0.0.1:8890` kept as the
  fallback default. Installing a canonical unit over a host's forkd-era
  one no longer depends on a drop-in naming the old paths; the gateway
  unit's `EnvironmentFile=/etc/spoond-gateway.env` already matched
  docs/install.md and docs/operations.md and is unchanged. Docs that
  name these paths (install.md, operations.md, deploy/README.md,
  PRODUCTION-ENV-2.7.md, images/README.md, the runner env.example's
  header) now agree with the units.

### Fixed

- **`spoond images build` prunes the build cache on both builders, by
  age and by a size cap (spoond-vdoq).** The 2026-10-11 prune ran
  `docker builder prune -f --filter until=72h`, which clears only
  dockerd's default builder and only cache older than 72 h — on
  2026-10-12, after 11 image builds in one afternoon, the host's root
  disk still reached 80 %: 14 GB of cache younger than three days
  survived the age filter, and the `docker-container` buildx builder
  the host actually builds with (the current one in `docker buildx
  ls`) kept 8 GB of its own that nothing pruned. After every build the
  pipeline now runs all four prunes best effort (a failed prune never
  fails the build): the default builder's `docker builder prune -f
  --filter until=72h` plus a size cap `docker builder prune -f
  --keep-storage IMAGE_BUILD_CACHE_KEEP`, and the current buildx
  builder's `docker buildx prune -f --filter until=72h` plus
  `docker buildx prune -f --keep-storage IMAGE_BUILD_CACHE_KEEP` (no
  `--builder` flag), with the same two rules. The new
  `IMAGE_BUILD_CACHE_KEEP` setting (TypeString, owner `images`;
  default `10GB`) holds docker's size spelling (`10GB`, `1024MiB`);
  a host without the buildx plugin skips its two prunes silently.

## [3.7.0] - 2026-10-12

3.7 makes spoond describe itself and report more of what it sees.
`GET /api/capabilities` (and `spoond capabilities`) returns every route,
image, size, network policy and other way in. A released lease's event
names its measured peak memory; a create may carry a `comment` that its
`created` event shows, which the CI runner uses for its job label; idle
sweeps report a summary event. The `/api/sandboxes` alias and keyless LLM
access, both removed in 4.0, now answer with `Deprecation` headers and
are counted, and spoond's own clients use `/api/leases`. `spoond doctor`
warns while the orchestrator is still starting. No breaking changes and
no schema change.

### Added

- **Deprecation signals for the surfaces 4.0 removes (#139
  groundwork).** Nothing changes behavior; the two removal candidates
  now report their use so the removal is decided on data. Every request
  still sent to the `/api/sandboxes` alias of the lease API answers with
  `Deprecation: true` and a `Link: </api/leases…>;
  rel="successor-version"` header (RFC 8594), bumps the new
  `spoond_deprecated_requests_total{kind="sandboxes_alias"}` counter and
  logs at most once per caller per hour (the user/consumer id and the
  path, never a token) until the caller moves to `/api/leases`. Every
  LLM-gateway request served only because `LLM_OPEN_LEGACY=1` kept a
  keyless owner open bumps
  `spoond_deprecated_requests_total{kind="llm_keyless"}`, with the same
  once-per-owner-per-hour log line. Both counters and headers are
  described in docs/api.md and docs/operations.md.
- **`GET /api/capabilities` self-description (#52).** One endpoint
  where any valid user token (the same grant as `GET /api/images`)
  reads what this spoond offers: the binary `version`, the `api` block
  (base, primary/alias lease paths, the auth model), `endpoints` (one
  row — method, path, one-line summary, admin flag — per route the
  backend registers; a test scans `api/server.go`'s registrations so a
  new route without a table row, or a stale row, fails the build),
  `images` (the catalog's current builds with the manifest's
  capability/description and default shape, the same rows
  `GET /api/images?detail=1` reads), `sizes` (the `GET /api/sizes`
  data), `network_policies` (the four egress classes with one-line
  meanings) and `surfaces` (only what this host configures: the SSH
  gateway address `GATEWAY_ADDR`, the `/llm/{lease-id}/` prefix when
  the LLM gateway is built, the stdio MCP server `spoond-dev-mcp` with
  the tool names from its own registry, and the CI runner's
  `IMAGE_MAP` labels). Never secrets, never other users' data. New
  `spoond capabilities` subcommand prints the summary or `--json` the
  raw document; endpoint documented in docs/api.md (Self-description).
- **Released leases carry their measured peak memory (#130 step 1).**
  When `OTEL_PROM_URL` is set the backend has sampled every
  running lease's guest memory since 3.5; a release now reports what
  the VM actually held: the `released` event's detail appends
  `· peak N MiB` after the release reason (the runner's
  `ci job 3609 ✓ 11m02s · peak 412 MiB`), whole MiB rounded up, and the
  release lease journal line gains `peak_mib=N` between `memory_mib`
  and `reason`. A release with no measurement (no `OTEL_PROM_URL`, or a
  lease that never got a sample) adds nothing on either line.
- **`spoond doctor` warns when the orchestrator is still activating.**
  A new `orchestrator: unit state` check reads the `e2b-orchestrator`
  unit's `ActiveState`/`SubState`: PASS when active/running, WARN when
  `activating` (start-post's `drain --start` still running — restarting
  or stopping the unit then skips `ExecStop`, so running leases are
  killed, not paused; see Restarting the orchestrator), FAIL when the
  unit is `failed`/`inactive`. Prompted by the 2026-10-12 03:08 PT
  restart-during-activating loss.
- **The dashboard's last two events (#132).** A CI job lease's `created`
  line is re-labelled as the job's start: the runner sends the job's
  comment (`forgejo job <id> <job URL>`, #119) in the create body, the
  backend lands it on the lease and cuts it into the `created` event's
  detail (at most 120 characters), and the events panel draws the line
  with the type word `runner` in the green ok role, the short lease id
  in the id column and the subject `ci job N started` — no new backend
  event. An idle-sweep pass that suspended at least one lease emits one
  lease-less `sweep` event next to the pass's per-lease
  `idle_suspended` events, detail `N suspended`; a pass that suspended
  nothing emits nothing. The panel draws it `idle sweep · N suspended`
  with the type word dim, its subject `spoond`. `POST /api/leases`
  (and its `/api/sandboxes` alias) gains the create-body `comment`
  field with the comment route's own 512-character cap. No schema
  change.

### Changed

- **spoond's own clients request `/api/leases`, not the alias (#139
  follow-up).** The runner's lease HTTP client, the SSH gateway's backend
  calls (create through delete, exec, stream) and the integration
  wsclient moved every request from the deprecated `/api/sandboxes`
  spelling onto the primary `/api/leases` one (identical behavior via
  the rewrite). The MCP servers already went through the runner client,
  and `spoondctl` is pure SSH, so the alias-usage counter
  (`spoond_deprecated_requests_total{kind="sandboxes_alias"}`) now
  measures outside callers only.

## [3.6.1] - 2026-10-12

3.6.1 fixes file writes into sticky directories such as `/tmp` on images
whose kernel setting `fs.protected_regular` is on (systemd's default, so
Arch-based images): `PUT /api/leases/{id}/files/…` there answered `502`
and left an empty file. No breaking changes and no schema change.

### Fixed

- **File writes land via a private upload directory and a rename.** The
  body is uploaded under a fresh name in a `0700` directory beside the
  target, then one step applies the mode and renames it into place. A
  secret is still never readable by others while it lands, readers never
  see a partial file, and an existing file is replaced rather than
  truncated. A directory in the way now answers `409` as documented. The
  pre-checkpoint secrets scrub also removes an upload directory a write
  cut off mid-upload left behind.

### Added (tests)

- **Conformance: the capabilities document and the create-time comment
  (spoond-35u2).** `conformance/capabilities_test.go`, group A, always
  on: A20 reads `GET /api/capabilities` with the test token and pins
  the document's shape — non-empty `endpoints` carrying `GET
  /api/images` and `POST /api/sandboxes`, every `GET /api/images` name
  in `images` (vacuously true when the catalog is empty), non-empty
  `sizes.presets`, the four egress classes in `network_policies` — and
  on one raw request with no Authorization header that the route
  answers `401` without a token. A21 sends the create body's `comment`
  (#132): the lease's GET carries it, a create without one still
  reports none, a comment over 512 characters is a `400` that starts
  nothing, and the created event's detail ends with ` · conformance
  comment 1` — read off the all-lease event stream the test opens
  before the create, matched on the lease's own id, with that sub-check
  alone skipped and the reason recorded when the stream does not open
  for the test token. No developer-machine mirror: the capability
  window must prove A20 on the production API and A21 on the
  production event stream.
- **Conformance: the lease API from guests and guest DNS (spoond-ndh).**
  `conformance/guest_dns_test.go`: N10 checks that an `internet` and a
  `lan` lease reach `GET /healthz` on the API's base URL and round-trip
  a create+delete with the test token (passed as an exec-time secret
  file, never env or argv), while a `restricted` lease cannot reach it.
  N12 checks every policy's `/etc/resolv.conf` names no public resolver
  and, for every policy but `none`, that the API's host name resolves
  to the LAN address. Both skip unless `CONFORMANCE_LAN_DNS` (the
  expected LAN address) is set. The mixed restricted allowlist stays
  N9's.

## [3.6.0] - 2026-10-11

3.6 keeps secrets out of the environment: every secret setting may name
a 0600 file holding its value instead (`<NAME>_FILE=/path`), and each
spoond process reads it once at startup, refusing to start on a missing,
unreadable or empty file or on both spellings at once. `spoond config
show` and `spoond doctor` understand the file form. A lease's
`size_hint` now keeps its vCPUs and comes with `memory_hint_mb`. No
breaking changes and no schema change.

### Added

- **Secrets from files (#137 C2).** Every secret setting (`ADMIN_TOKEN`,
  `METRICS_TOKEN`, `EVENTS_TOKEN`, `CONSUMER_TOKENS`, `GATEWAY_TOKEN`,
  `BOOTSTRAP_TOKEN`, `LLM_UPSTREAM_KEY`, `PROXY_AUTH_SECRET`,
  `DASH_PASSWORD_HASH`, `DASH_EVENTS_TOKEN`, `RUNNER_TOKEN`,
  `LEASE_TOKEN`, `FORGEJO_ADMIN_TOKEN`, `SPOOND_GATEWAY_TOKEN`) may be
  given as `<NAME>_FILE=/path` (a 0600 file whose content, trailing
  newlines trimmed, is the value): the processes read it once at
  startup. Setting both forms, or naming an unreadable or empty file,
  refuses startup; a file readable by group or others logs a warning
  and `spoond doctor` flags it. `spoond config show` keeps the file
  spelling as its own row (the path shown, the content never) beside
  the secret's row (`(hidden, from file)`, source the env file), and
  `GET /api/admin/config` reports such a secret set, so the two
  surfaces still agree. The procedure is docs/operations.md, "Moving a
  secret to a file".

### Fixed

- **`size_hint` never names a preset with fewer vCPUs than the lease runs** (a 2-vCPU lease was told `small`, 1 vCPU), and a lease also reports `memory_hint_mb`: the memory that holds its measured peak with the same 25 % margin, for a create that keeps its vCPUs.

## [3.5.0] - 2026-10-11

3.5 measures what each lease's VM really uses: every running lease
reports its current and peak memory, and a size hint names the smallest
preset that would hold its peak with headroom, so an owner can pick a
smaller size. `GET /api/usage` adds the owner's measured memory. The
backend now checks every setting's type at startup and refuses to start
on a bad value, naming each one. The dashboard's columns read `state`
and `policy`. No breaking changes and no schema change.

### Added

- **The backend validates its typed settings at startup (#137 C1).**
  Every setting it reads that is set must parse as the type the code
  reads it as — integer, boolean, duration (Go duration or seconds),
  percentage 0–100, URL with a scheme, `host:port`, or a
  comma-separated list — and one bad value refuses startup with a
  single message naming every problem
  (`config: MAX_TTL_SECS="2h": not an integer; …`), with
  secret-named settings' values masked to `(hidden)`. Unset settings
  keep their defaults; unknown keys stay `spoond doctor`'s warning.
  The settings package (`settings.Specs`, `settings.Validate`) holds
  the per-setting type and owner binary behind the same
  `settings.Known` list `spoond doctor` checks env files against.
### Changed

- **The dashboard's `access` column is headed `policy`**: it shows the network policy today, and more lease policy is coming.
- **The dashboard's state column is headed `state`**, not `ST`, so a reader new to spoond can scan it; the column widens by three cells.

### Added

- Measured lease memory (#130): when `OTEL_PROM_URL` is set, the backend
  samples the orchestrator's per-sandbox ram gauges every 30 s and each
  live lease's row (GET /api/leases, GET /api/leases/{id}) gains
  `memory_used_mb` (latest) and `memory_peak_mb` (max since the current
  VM started), plus `size_hint` — the smallest size preset whose memory
  fits 1.25 × the measured peak, once at least 10 samples are in and it
  would actually save memory. `GET /api/usage`'s `leases` object gains
  `memory_used_mib`, the summed latest usage of the caller's running
  leases. In memory only (nothing persisted, nothing without
  `OTEL_PROM_URL`).

## [3.4.1] - 2026-10-11

### Fixed

- **`spoond config show` reads every env file of the unit.** A unit with two `EnvironmentFile=` lines prints `EnvironmentFiles=` twice; only the last was read, so settings from the first showed as drift.
- **`spoond config show` lists only what the unit sets and what drifts** from the running backend; `--all` lists every known setting (most belong to other binaries).

## [3.4.0] - 2026-10-11

3.4 adds `spoond config show`: every setting of a unit with its value
(secrets masked), the file it came from, and whether the running
backend has the same value — so a changed env file waiting for a
restart, or a setting no file sets, shows at a glance. The backend
answers its own view at `GET /api/admin/config` (admin). `spoond images
build` now prunes old docker build cache. No breaking changes and no
schema change.

### Added

- `GET /api/admin/config` (#137): read-only echo of the backend's
  environment — every `settings.Known` name with whether it is set and
  its value, masked to `(hidden)` for any name carrying `TOKEN`, `KEY`,
  `SECRET`, `PASSWORD`, `HASH` or `SEED`, for `NOTIFY_WEBHOOKS`, and for
  any value holding a URL with credentials or a query string, plus the process's
  `started_at` (RFC3339) — so an operator can see what the running
  backend was started with. Admin auth like the other `/api/admin`
  routes.
- `spoond config show` (#137): resolves the backend unit's effective
  environment (`systemctl show <unit> -p EnvironmentFiles -p
  Environment`; `Environment=` first, then each env file, later wins),
  masks secrets with the same rule, and prints NAME / VALUE / SOURCE /
  RUNNING — the running-backend column comes from the new admin config
  endpoint, fetched with the merged env's own token and bind address —
  classifying each setting `same`, `differs — restart to apply`, `not
  set in running backend`, `?` (unreachable) or `n/a`; unknown keys
  carry a `(unknown setting)` note. `--json` for the raw document;
  read-only.

### Fixed

- **`spoond images build` prunes docker build cache older than three days** after each build, so repeated image builds no longer fill the host's root filesystem.

## [3.3.0] - 2026-10-11

3.3 brings authorization into one simple model (#138): users and admin
users, plus two read-only service tokens and the host's break-glass
`ADMIN_TOKEN`. An admin user's own token now reaches `/api/admin/*`; a
user can mint several named tokens, each revocable and optionally
expiring; root on the host can mint a short-lived token for any user
during a maintenance window. The dashboard's leases table shows each
lease's state and pin as two glyphs. No breaking changes and no schema
change.

### Changed

- **The dashboard's leases table reads its state in two cells** instead
  of the glyph-plus-word: the run-state glyph (`▶` running,
  `‖` suspended, `⭘` recovered, `■` lost) then `◆` on a pinned lease,
  with the pin mark removed from the holder column, the column headed
  `ST`, and the suspend reason carried by the glyph's colour — taken
  back warn, idle-suspended dim, any other suspend plain. A dim legend
  under the table — `▶ running ‖ suspended ⭘ recovered ■ lost ◆ pinned`
  — now spells the glyphs out on every table with rows, and the freed
  column width goes to the owner then the holder.

### Added

- **The break-glass `ADMIN_TOKEN` mints a user token (#138 A3).**
  `POST /api/users/{id}/tokens` now accepts the env `ADMIN_TOKEN` (the
  host root's token) as well as the user's or an admin's, so root on
  the host can mint a short-lived token for **any** user during a
  maintenance window (`ttl_secs` is required, so the token always
  expires; the response answers the secret once). The mint is logged with the user, the
  token's name and id — never the secret. The break-glass stays on
  that one route: GET/DELETE on the tokens and every other user route
  still refuse it (`401`), so root cannot list, revoke or read a
  user's tokens; wrong token is `401` as ever. See
  [operations.md](docs/operations.md#maintenance-windows-and-the-break-glass-token-mint)
  and the who-can-do-what table in
  [security.md](docs/security.md#who-can-do-what).
- **Tokens are capped and admin routes are throttled (#138 review).** A user holds at most 50 tokens (`409 too_many_tokens`); a token's `last_used_at` refreshes at most every 5 minutes, so logins rarely take the store's write lock; a revoke the users file could not save is rolled back and answers `500`; wrong tokens on `/api/admin/*` hit the per-address failed-auth throttle (`429`).
- **Named, revocable, expiring API tokens per user (#138 A2).** A user
  can hold several named bearer tokens beside its original one:
  `POST /api/users/{id}/tokens {"name","ttl_secs"?}` mints one and
  answers the secret once (`201 {id,name,token,created_at,
  expires_at}`), `GET /api/users/{id}/tokens` lists id, name, created,
  expires and last used — never secrets — and
  `DELETE /api/users/{id}/tokens/{token_id}` revokes (`204`). The user
  itself and an admin manage a user's tokens; another user gets `404`,
  a legacy consumer token `403`, like the other user routes. A token
  with a `ttl_secs` expiry stops authenticating the moment it passes.
  Every mint and revoke is logged with the token's name and id, never
  its secret.
- Names are unique per user and `default` is reserved while the
  original token is still set (`409` otherwise).
- **Admin users reach `/api/admin/*` (#138 A1).** The admin routes
  (`drain`, `undrain`, `reconcile`, `unpin-by-holder`) accept an
  identity-store admin user's bearer token as well as `ADMIN_TOKEN`; a
  non-admin user's token stays `401` and `ADMIN_TOKEN` works unchanged.

## [3.2.0] - 2026-10-11

3.2 makes a saturated host make new work wait, lets an owner share a
secret-free named snapshot with another user, and makes leases that
are about to expire say so. A create, clone or fork waits (`503
capacity_wait`) while the host's I/O or CPU pressure is over its target;
running leases are never held. A named snapshot saved from a lease that
never had a secret can be granted to another user, who starts leases
from it and may restore back to it. `spoond doctor` checks the env
files for unknown and repeated keys, and the dashboard shows each
lease's size. No breaking changes; the schema moves from 27 to 28.
Every lease that exists before the upgrade counts as having had a
secret, so a snapshot meant for sharing is saved from a new lease.

### Added

- **`spoond doctor` checks the env files (#137).** Every `*.env` in `/etc/spoond` is read by key name: a key no spoond binary reads (a typo or a removed setting) or a key set twice is a warning. A test keeps the list of known settings equal to what the code reads.
- **`spoond create` / `spoondctl create` take `--size <preset>` and `--snapshot-owner <user>`**, for a lease at a chosen size (#151) or from another user's granted snapshot (#141).
- **Named-snapshot grants (#141).** An owner shares one of its named snapshots with another user: `POST /api/named-snapshot-grants {"name","grantee"}`, `GET` (given and received), `DELETE ?name=&grantee=`. The grantee starts from it with the new create field `snapshot_owner` (user name or id); without a grant the answer is `404 not_found`, the same as an unknown name. The grantee is charged the lease's memory; the snapshot's disk stays the owner's, and releasing the grantee's lease re-runs the owner's retention. Revoking stops new starts and leaves running leases alone. A lease's `snapshot` object names the version's `owner`.
- **Secret-free snapshots (#141).** A lease that ever held a secret (create-time, exec-time or job `secrets`, or one inherited through clone, fork or a start from a tainted version) is marked for good; a named snapshot saved from it reports `secret_free: false` and can never be granted (`409 not_secret_free`). Exec `env` values are not secrets. Versions saved before this release read `secret_free: false` until saved again.
- **Restore to the start snapshot (#141).** `POST /api/leases/{id}/restore` accepts the build the lease started from (its `snapshot.build_id`), also when another user owns it. The fresh guest gets this lease's `lease-id` and `started-from` markers and an empty `/run/secrets` before its own create-time secrets return.
- **The dashboard's leases table shows each lease's size** after the image: memory then vCPUs (`4G/2V`, `1.5G/1V`, `512M/1V`; `-` when unknown).
- **New work waits while the host is saturated (#136 stage 2).** A create, clone or fork answers `503 capacity_wait` (or waits in the admission queue) while the host's I/O full or CPU some pressure (PSI, 10 s average) is at or over `SATURATION_IO_FULL_PCT` (default 20) or `SATURATION_CPU_SOME_PCT` (default 75). Running leases, resumes and restores are not held. Metrics `spoond_host_pressure_pct` and `spoond_saturation_refusals_total`.
- **A create that will expire says so.** A non-persistent create answers with a `Warning:` header naming its TTL when the TTL was defaulted or a pin or holder is set (neither keeps a lease past its TTL since 3.0).
- **`GET /api/usage` "recent" lists leases spoond released on its own clock**: a TTL expiry, the paused-release clock or a lost lease's grace, as `released` events.

### Fixed

- **The gateway's `ssh-key add` takes a real public key (#72).** The whole authorized-keys line (`<type> <base64> [comment]`) followed by the user name, or the name first; before, the command was split on whitespace and every real key was refused.

## [3.1.0] - 2026-10-11

3.1 lets a create choose its lease's size and shows owners what they
use. A create takes a size preset (`GET /api/sizes`) or `vcpu` and
`memory_mb`; sizes with a warm build start from a memory snapshot as
before, and any other size boots in about a second. Every lease reports
its size and how it started, and clone, fork, restore and recovery now
charge the size the VM actually runs. `GET /api/usage` adds lease
counts, waiting creates and recent refusals, and
`GET /api/usage/history` keeps 7 days. No breaking changes; the schema
moves from 23 to 27 with additive migrations.

### Added

- **`GET /api/usage` and `GET /api/users/me` report the caller's lease counts.** A `leases` object beside `share` gives the owner's running, paused, pinned and lost lease counts plus how many background jobs those leases run (#140 U1).
- **`GET /api/usage` lists the caller's waiting creates.** `waiting` gives each queued create's place in the whole admission queue, how long it has waited and the reason, the same entries as `GET /api/leases/queue` (#140 U2).
- **`GET /api/usage` lists the caller's recent refusals and reclaims.** `recent` gives the caller's newest 20 `box_full`, `take_back`, `queued`, `suspended`, `critical_release` and `paused_expiring` events, newest first. It is kept in memory, so it starts empty after a backend restart (#140 U3).
- **`box_full` events name the refused requester as their owner**, so the requester now receives them on its own event stream (`GET /api/leases/events`); before, they carried no owner and only admins saw them.
- **Choose a lease's size per create (#151).** A create takes `size` (a preset from `GET /api/sizes`) or `vcpu` and `memory_mb`; the image gives the default. A size with a warm build starts warm as before; any other size cold-boots in about a second, and the lease reports `start: "warm"` or `"cold"`. A lease created at a chosen size keeps it through restarts; one created at the default size takes the image's current default when restarted, as before. The lease's memory is its charge for admission and fair shares; vCPU is never charged. A cold-booted guest runs its image's start command (spoond-guest-init and its init hooks) and passes the image's ready check before the lease gets it, as a template build does. A bad size is `400 invalid_size`. `memory_mib` stays as the old name of `memory_mb`; it no longer has to equal the image's memory. Named snapshots record the size of the lease they were saved from.
- **A lease reports its own size and keeps it (#151).** Lease rows carry `vcpu`, `memory_mb` and `disk_mb`. Clone, fork, restore and recovery now run and charge the size the lease actually has; before, they charged the image's current `memory_mb`, so after an image was rebuilt at another size a restored lease could be charged one size while its VM ran another. Migrations 25 and 27 add the lease's `vcpu` and whether its size was chosen.
- **Size presets and per-lease limits (#151).** `SIZE_PRESETS` names VM sizes (default `small=1:2048,medium=2:4096,large=4:8192,xlarge=8:16384`, as `name=vcpu:memory_mb`); `MAX_LEASE_VCPU` (default the host's CPU count) and `MAX_LEASE_MEMORY_MB` (default 32768) cap one lease's VM. A malformed value stops the backend at startup. `GET /api/sizes` lists the presets and the limits to any authenticated caller.
- **Warm sizes per image (#151).** A manifest entry's `warm_sizes: [medium, large]` makes `spoond images build` build one more memory snapshot per listed preset from the same image, so a VM of that size can start warm. `min_memory_mb` sets the smallest memory a lease of the image may ask for (default 512, or the image's own memory when smaller); a warm preset below it, or an unknown preset name, fails the build before it starts. Both are inherited through `from:`. `GET /api/images?detail=1` rows gain `min_memory_mb` and `warm_sizes`. The GC keeps each image's warm builds like its current build. Migration 26 adds the `image_sizes` table and `images.min_memory_mb`.
- **Usage history (#140 U4).** spoond records each owner's usage every 15 minutes and keeps 7 days; `GET /api/usage/history?hours=N` (default 24, cap 168) returns the caller's own samples, oldest first. Migration 24 only adds the `usage_samples` table (schema 23 → 24), so a 3.0.0 binary can still open the database.

### Fixed

- **Event durations between one second and a minute read to two decimals (#147).** A checkpoint that took 17.2689333 s now reads "17.27 s"; one that rounds up to a minute reads "1m0s", never "60 s".
- **The dashboard's host panel GC row fits its panel (#148).** The kept builds now come right after the GC mode, before the lifetime deleted count; a part that does not fit is dropped whole, and the row ends in … as a last resort, instead of running past the panel's edge.
- **The dashboard's events panel no longer cuts a detail silently (#149).** A row too long for the panel ends in …, and the subject column is capped at 20 characters instead of 24, so more of the detail fits.

## [3.0.0] - 2026-10-10

3.0 replaces guaranteed and burst classes, preemption, holds and
per-owner caps with fair shares: every owner gets an equal, floating 1/N
slice of memory and snapshot disk. When a request does not fit, spoond
takes room back from the biggest borrower: for memory it pauses that
owner's least recently used unpinned lease, and for disk it deletes the
owner's oldest unpinned paused lease. A pin keeps a lease from being
paused or deleted, and one clock releases every paused lease 30 days
after its pause date. Every capacity shortage answers one shape,
`503 capacity_wait` with `Retry-After`, and a request take-back cannot
serve answers `429 box_full`. Upgrading from 2.9? See
[docs/upgrading-3.0.md](docs/upgrading-3.0.md).

### Breaking

- **The Go module path is now `github.com/jrimmer/spoond/v3`.** Go
  requires the major version in the module path from v2 on, so 3.0
  moves the path from `/v2` to `/v3`. Code that imports
  `github.com/jrimmer/spoond/v2` stays on 2.x until it changes the
  import.

### Added

- **Named saves answer disk refusals like every other path.** A save
  whose checkpoint does not fit in usable snapshot disk answers
  `429 box_full` or `503 capacity_wait` with `Retry-After`, not
  `500 save failed`.

- **FS2b: the biggest borrower gets box_full at once.** When take-back
  can free nothing because the requester is itself the biggest borrower
  (over its fair memory slice after the request, and no other owner
  further over), create, resume, restart, clone and fork answer
  `429 box_full` immediately, with the `box_full` event and alert,
  instead of waiting on `503 capacity_wait`. A shortfall no owner causes
  still waits.

- **FS2a: memory take-back picks the biggest borrower by ratio.**
  When a guaranteed admission does not fit, the fair-shares selector
  (`api/take_back.go`) names the leases to pause: the owner furthest
  over their slice (usage/slice, never an owner inside their slice, and
  a give-up that would drop them below the requester's post-request
  ratio is never made), and within that owner the least recently used
  unpinned lease (busy leases last, pinned leases never). Pauses run
  one at a time through `takeBackPause`, which re-checks the victim's
  eligibility in the same critical section that marks it busy and
  un-does the pause if the owner pins the lease mid-pause. The
  `lease.suspended` event with reason `take_back` now carries
  `take_back_ratio` (the victim owner's ratio) and `take_back_for` (the
  requesting owner); the `take_back` event is unchanged.

- **Fair shares: an equal floating slice per owner and usage
  accounting (#145 FS1).** Every owner now has a computed 1/N slice of
  the box — the hugepage memory pool and the snapshot volume's usable
  bytes, N being the number of owners that exist (each identity user,
  plus the legacy consumer token as one owner). Usage is measured from
  recorded state: memory is the owner's running leases, disk is their
  pause snapshots + kept checkpoints + named snapshots (recorded
  `size_bytes`, never a filesystem walk per request). The view is
  computed in one place (`api/fair_shares.go`), cached, and invalidated
  on lease state changes and owner add/delete.

  - **API.** `GET /api/fair-shares` (admin) lists every owner sorted by
    ratio (usage/slice) descending; `GET /api/users/{id}` (admin) and
    `GET /api/users/me`/`GET /api/usage` (self) return the owner's
    `slice_pct`, `memory {slice_mib, used_mib}`, `disk {slice_bytes,
    used_bytes, paused_bytes, kept_bytes, named_bytes}` and `ratio`.
  - **`GET /api/shares` is unchanged.** It still lists the caller's
    lease grants; the fair-share view is served at `GET
    /api/fair-shares` instead, so no existing caller breaks. The SSH
    gateway's `share ls` is untouched.
  - This unit computes and reports the shares only: no admission,
    preemption or take-back behaviour changes yet.
  - Capacity is read from the node-info cache the admission path keeps
    warm; the fair-share routes make no substrate RPC. On a cold cache,
    or when a store read fails, the slices are `0`, the result is marked
    unknown and **not cached**, and the next request recomputes it (a
    failed read is never served as a real zero). Invalidations run
    after the store write, so a concurrent compute cannot cache the
    pre-write state.
  - **Unknown capacity is explicit (`capacity_known`).** The
    `/api/fair-shares`, `/api/usage` and `users/me` share payloads carry
    a `capacity_known` boolean: `false` means the node-info cache was
    cold or a store read failed, so the slices are not computed yet
    (rather than a wrong zero). The endpoint still answers `200`.
  - **Disk slice basis is the volume's usable bytes.** The slice is
    1/N of the `statfs` free-to-unprivileged bytes (`Bavail`, not the
    raw filesystem total) plus the snapshot bytes spoond already
    accounts for (kept + named + paused) — the space spoond can hand
    out. A build that is both a kept checkpoint and a named snapshot
    counts once.
  - **The cache does not churn on activity.** A lease save invalidates
    the fair-share view only when an accounted quantity changes (a
    lease is created/released/suspended/resumed, or its memory charge
    changes); an activity-only save (`LastActive`, expiry, holder)
    leaves it warm.
  - **A shared build counts once in the disk basis.** The accounted
    part of the disk slice basis is summed over distinct builds, so a
    build pinned by two owners (or named apart from its build's owner)
    does not count twice. `named_bytes` uses the build's recorded
    `size_bytes` (re-measured by the hourly pass) and falls back to the
    named row's size only when the build row is gone, so it can differ
    from the named-snapshot listing's `size_bytes`.
  - **A settled build size invalidates the cache unconditionally.**
    `settleBuildSize` drops the fair-share cache even when the metrics
    gauges are not wired.
- **FS3a: disk room accounting and disk take-back core (not yet wired
  into requests).** `computeDiskRoom` is the pure room formula for the
  snapshot volume: `Usable = Free - Reserved - Floor - Pending +
  Freeing`, where Reserved is the whole `disk_mb` allowance of the
  running leases, Floor the `DISK_RESERVE_PCT` (env, default 5, clamped
  0–50) share of the volume, Pending the bytes of in-flight snapshot
  writes and Freeing the bytes of deletions statfs has not reported
  yet (the ZFS late-free guard, entries dropped once free grows by
  their bytes or after 30 s). `diskTakeBack` is the request-driven
  take-back: spoond's own garbage first (`reapOrphans`, the ORPHAN_REAP
  and GC_DELETE guards unchanged), then the biggest disk borrower's
  (usage/slice) oldest unpinned paused lease, one at a time — running
  and pinned leases never, named snapshots and kept checkpoints never,
  a candidate only while its owner's ratio after giving it up stays
  above the requester's after-request ratio. The live lease is
  re-checked at commitment, a stale pick announces nothing and the
  loop re-picks; a call that frees nothing returns a box-full error.
  One `disk.cleanup` event per call carries the bytes freed per
  category. Not wired into any request path yet.
- **FS3b: create, resume, pause and every checkpoint must fit in the
  snapshot disk.** `ensureDiskRoom` guards `grantLease` (the image's
  `disk_mb`), `resumeLease` (the lease's `disk_mb`), `pauseLeaseBody`
  (the memory size beyond the lease's own `disk_mb`, which the pause
  gives back) and `checkpointLease` (the memory size as the snapshot
  estimate) before any write, reserving the bytes until the request
  finishes. An admin drain is never refused for disk.
  When usable disk is short, disk take-back runs first — garbage, then
  the biggest borrower's oldest paused lease — and a request that still
  does not fit answers `429 box_full` (nothing reclaimable) or `503
  capacity_wait` (a structural shortage), on create, resume, suspend,
  checkpoint/keep and named save alike.
- **`GET /api/fair-shares` reports the snapshot disk's room.** When the
  disk can be read the response carries a `disk` object
  (`total_bytes`, `free_bytes`, `reserved_bytes`, `floor_bytes`,
  `pending_bytes`, `freeing_bytes`, `usable_bytes`) — the same numbers
  the take-back policy decides with; the object is omitted when the
  statfs read fails, so an unknown disk is never a zero one.
- **`capacity.disk_low` alert.** The notifier's periodic checks now
  alert (critical) when the snapshot disk's free bytes fall below the
  reserve floor (`DISK_RESERVE_PCT`, default 5, of the disk), resolving
  when free comes back. It is a passive alert: it reads statfs and
  emits, and deletes nothing.
- **The dashboard shows the snapshot disk's fair-share room.** The
  backend exports `spoond_snapshot_disk_reserved_bytes` and
  `spoond_snapshot_disk_usable_bytes`, and the host panel draws one
  line, `disk reserved X · usable Y` (humanized).

### Status code changes

- **No per-owner `429` any more (FS4).** `429 quota_exceeded` and the
  plain over-quota `429` are gone from every path: create, resume,
  every resume-on-use work call, restart, clone, fork and restore. Room
  is refused only by `429 box_full` (take-back can free nothing) or
  waited on as `503 capacity_wait`. `docs/api.md` has a "Refusals"
  section and a per-path "Changes from 2.9" table.
- **`409 kept_budget` is measured against the owner's fair disk slice
  (FS4)** on a checkpoint keep and a named save, instead of the
  per-owner `max_kept_bytes`. While the box capacity is unknown there
  is no budget.
- **`429 box_full`** with JSON `code: box_full`: a request that needed
  room and could not get it because every take-back candidate is pinned
  (FS5). This is a refusal, not a wait: nothing is paused or released.
  `PUT /api/leases/{id}/pin` and `DELETE
  /api/leases/{id}/pin` answer the lease's pin state, and the
  `pinned`, `paused_at` and `pinned_idle_since` fields appear on the
  lease API. `hold_expires_at` and `hold_state` are gone. The
  `box_full` and `admin_unpin` events carry lease id `-` (a placeholder
  for a lease-less event), documented in `docs/api.md`. **Every
  re-admission path answers it too:** `POST /resume`, every resume-on-use
  work call (exec, stream, files, proxy, jobs, the LLM gateway, the SSH
  gateway), restart, clone, fork and restore map a box_full class
  re-admission to the same `429 box_full` body a create gets, not a 500
  a client would read as a permanent lease failure; and an undrain in
  that state defers (the lease stays `drained`) instead of reporting
  `resume_failed` or losing the lease (spoond-k0uz R3-2).
- **Every structural capacity refusal answers `503 capacity_wait`**
  with `Retry-After: 30`: create, clone, fork, restore, checkpoint and
  the files routes (and a queued create that times out on a capacity
  cause) carry the JSON `code: capacity_wait` and body prefix
  `capacity: …`, so no path refuses for a structural shortage with a
  bare 503. `429 box_full` is unchanged.
- **Resume no longer refuses an unpinned non-persistent lease.**
  `POST /api/leases/{id}/resume` and the SSH gateway's resume accept
  any suspended lease the caller owns, as resume-on-use already did; the
  `400` for a non-persistent lease is gone. A suspended non-persistent
  lease the drain left suspended resumes normally.
- **`PUT /holder` with a different holder replaces it** instead of
  answering `409`: a holder is a plain label now, so there is nothing
  to conflict with (v3.0).

### Changed

- **The dashboard drops the burst and preempted marks for taken-back
  leases.** The 3.0 dashboard no longer shows the removed burst class or
  preemption: the capacity panel's `taken back N` count and the leases
  table's "taken back" state name the leases suspended by memory
  take-back (`suspend_reason = 'take_back'`).

- **Queued creates are served by fair-share ratio (FS4).** The owner
  with the lowest ratio (usage over their equal slice) goes first, an
  owner the fair-share view does not know after every known one, then
  FIFO. `guaranteed_mib` headroom no longer orders the queue, and the
  `queued` event's refusal is always `capacity`.
- **`POST /api/users/{id}/quota` sets `max_ttl` only (FS4).** It still
  accepts `max_leases`, `guaranteed_mib`, `max_mib` and
  `max_kept_bytes` (no `400`, any value) and ignores them; a request
  that sets any of them answers with `Deprecation: true` and
  `Warning: 299 - "… ignored since 3.0 (fair shares)"`. The
  `guaranteed_mib <= max_mib` check is gone.

- **Pins replace holds; one clock for every paused lease (FS5, owner
  decision 2026-10-08).** spoond no longer does anything pre-emptively:
  there is no auto idle pause, no pressure sweep and no different
  clocks. A lease gains a **`pinned`** flag (`"pinned": true` on create,
  or `PUT /api/leases/{id}/pin`; unpin with `DELETE`). A pinned lease
  is never paused or deleted by spoond before its own expiry — the TTL
  still applies (pin and expiry collaborate), and a pinned persistent
  lease stays until the owner releases it. There is no limit on how
  many leases an owner pins. **Take-back (memory and disk) touches only
  unpinned leases**; if nothing unpinned can be taken, the request
  answers `429 box_full` and raises an alert. **One clock:** every
  paused lease is released `PAUSED_RELEASE_DAYS` (default `30`) days
  after its pause date, whatever paused it, preceded by a
  `lease.paused_expiring` event 24 h before; resuming clears the date.
  A lease's own TTL and its own `idle_suspend` opt-in are unchanged.

  - **`holder` and `holder_url` are plain labels now.** They have no
    lifecycle effect: setting a holder never pins, never keeps a lease
    past its TTL and never protects it from take-back. `hold_ttl` is
    accepted and ignored for one release, with a `Deprecation` header;
    `hold_expires_at`, `hold_state` and hold renewal are removed.
  - **`"pinned": true` on create pins the new lease.** The field was
    decoded but dropped by the request path; it is now carried into the
    grant (spoond-k0uz H1). A lease pinned on create is never paused by
    the idle logic or take-back, and is still released at its own TTL.
  - **A pinned lease is never idle-suspended.** The `idle_suspend`
    sweep skips any pinned lease, whether its own `idle_suspend` or the
    host `IDLE_SUSPEND_DEFAULT_SECS` would fire (spoond-k0uz H2). An
    owner's explicit `POST /pause` still pauses a pinned lease.
  - **A paused pinned lease is on the one clock (owner decision
    2026-10-09).** Every paused lease is released 30 d after its pause
    date, pinned or not: a pin protects only a running VM. This covers
    an owner's own `POST /pause` on a pinned lease and a pinned lease a
    failed drain resume left suspended; a running pinned lease is never
    touched. A drained lease left suspended with `resume_failed` gets
    `paused_at` at its drain pause time and so the clock.
  - **Pinned-idle visibility (no automatic action).** A pinned lease
    whose last API activity (`LastActive`; no heartbeat, no guest
    activity) is older than `PINNED_IDLE_NOTICE_DAYS` (default `7`) is
    flagged: GET returns `pinned_idle_since`, a `lease.pinned_idle`
    event fires once per crossing, and the dashboard shows one
    aggregate notification, `N pinned leases idle over 7 d (owner:
    count, ...)`, broken down per owner. Nothing is paused, unpinned or
    released because of it.
  - **Migration 0022** turns every lease with an unexpired hold into a
    pin, adds `pinned`, `paused_at`, `pinned_idle_since` and
    `paused_expiry_notified`, backfills `paused_at` to the migration
    time for every lease already `suspended` (a fresh 30 d from the
    upgrade; no lease is released sooner), extends the `expires_at` of
    every pinned non-persistent lease whose hold had outlived its TTL
    to that hold's expiry (the hold was what kept it alive in 2.9, so
    the first 3.0 sweep must not delete it at once — spoond-k0uz R3-1),
    and clears the old hold columns (`hold_expires_at`, `hold_set_at`,
    `hold_ttl`). An admin can unpin leases by holder label with
    `POST /api/admin/unpin-by-holder?holder_prefix=<prefix>`.
    Clearing the hold columns is the **rollback story**: a 2.9 binary
    rolled back onto this database reads every row as unheld instead of
    treating an unexpired hold as live again (spoond-k0uz M6, see
    `docs/operations.md`).
  - **Before upgrading:** clients that kept leases alive with a holder
    must create them with `"pinned": true` (plus `"persistent": true`
    if they must outlive `MaxTTL`) — `PUT /holder` no longer renews anything, and a
    non-persistent lease cannot be extended (spoond-k0uz R3-1).
  - **The one clock only ever releases a suspended lease
    (spoond-k0uz R3-3).** A running lease with a stale `paused_at` (the
    mark of a rollback to 2.9, which resumes without clearing the
    column) is not deleted 30 d after the old pause: the clock's sweep
    and its pre-release re-check both require the lease to still be
    suspended, unreleased and not busy, and `LoadState` clears the
    stale date (and stamps a fresh one on a suspended row with none) at
    load. This also closes the race where a resume landing between the
    sweep's collection pass and the release deleted a lease that had
    just come back.

### Removed

- **Per-owner caps (FS4).** `max_leases`, `max_mib`, `guaranteed_mib`
  and `max_kept_bytes` no longer limit anything: every owner gets an
  equal fair slice of memory and snapshot disk instead. The user views
  (`GET /api/users`, `/api/users/me`, `/api/users/{id}`, the quota
  response) and the lease detail no longer return them; the lease
  detail keeps `charged_mib`. A create no longer waits on a lease-count
  or memory cap. The users file drops the four fields on its next save
  (an older file still loads). The metric
  `spoond_quota_exceeded_total` and the dashboard's `quota` refusal
  counter are removed.

- **Every automatic hold/idle rule and every clock but one (FS5).**
  Removed: held rules 1 (idle 4 h), 3 (hold lapse), 4 (pressure) and 5
  (critical disk); rule 2 (7 d) and the k5m 30 d stale design; the
  `IDLE_TIMEOUT_SECS` auto-suspend of unheld persistent leases; and the
  environment variables `HELD_IDLE_TIMEOUT_SECS`,
  `HELD_SUSPENDED_RELEASE_SECS`, `HOLD_TTL_SECS`, `HOLD_TTL_MAX_SECS`,
  `PRESSURE_HELD_IDLE_SECS` and `IDLE_TIMEOUT_SECS`. A set variable is
  logged as removed and ignored at startup. `spoond_held_actions_total`
  is replaced by `spoond_box_full_total`. Only the lease's own
  `idle_suspend` opt-in suspends a lease for idleness now.

- **Lease classes (FS2b-1).** Every lease now admits against one
  hugepage pool; memory take-back (FS2a) replaces classes and preemption.
  Removed:
  - the create fields `burst` and `priority` (now ignored if sent) and
    the lease field `class`;
  - promotion of burst leases and the `promoted` event;
  - the burst reserve and the environment variable `BURST_RESERVE_MIB`,
    with the `503 no burst capacity` refusal;
  - preemption of burst leases, the `preempted` event, and the
    environment variable `PREEMPT_DISK_FLOOR_PCT` with the
    `503 capacity: cannot preempt (snapshot disk low)` refusal.
  - the metrics `spoond_preemptions_total` and `spoond_preempted_leases`.
  The lease field `preempted` stays and is always `false`. The suspend
  reason `preempt` is now `take_back`. (`guaranteed_mib` and `max_mib`
  went later, with the per-owner caps.)

### Fixed

- **Take-back counts memory still in flight.** An owner's admitted
  creates that have not started yet count toward its usage, so it cannot
  read a falsely low ratio and take back from an owner who is no further
  over its slice.

- **Disk take-back works.** It now finds a paused lease's pause build
  (it was looked up by the wrong key, so no paused lease was ever
  reclaimed), deletes that build at once instead of leaving it for the
  hourly GC, and counts only bytes actually deleted. The room check and
  its reservation are one step, so concurrent writes cannot share the
  same free bytes. Restart, clone, fork, restore and recovery now check
  usable snapshot disk for the lease's `disk_mb` like a create. A box
  whose capacity is unknown, or a pass that already freed bytes, answers
  `503 capacity_wait` instead of `box_full`.

- **A refused fork batch leaves no take-back victim paused.** A fork
  admits its whole batch's memory once, before any child is created, so
  a refusal cannot leave an earlier child's take-back behind: the
  children share one admission and a rollback credits it.

- **A disk-full failure from the orchestrator is a 503
  `capacity_wait`, not a 500.** Create, pause, resume and checkpoint
  errors whose message reports `no space left on device`, `ENOSPC` or
  `disk quota exceeded` are mapped to `substrate.ErrCapacity` even when
  the orchestrator sends them as `Internal`/`Unknown`.

- **A memory admission that fails afterwards gives its cache debit
  back.** `admitMemory` returns an undo that credits the cached
  `NodeInfo` when the create or resume it admitted never took hold, so
  for the rest of the cache TTL `fits()` no longer reads less free
  memory than there is and take-back no longer pauses a lease for
  memory that is free.

## [2.9.3] - 2026-10-09

### Added

- **Dashboard login toggle (`DASH_AUTH`).** The web dashboard's basic
  auth can be turned off with `DASH_AUTH=off` for a dashboard on a
  private network; `DASH_USER` and `DASH_PASSWORD_HASH` are then not
  required, and the dashboard logs that it serves without a login.
  The default stays on, and any value other than an off-word (`off`,
  `0`, `false`, `no`) keeps the login.

## [2.9.2] - 2026-10-09

### Changed

- **The dashboard's i/o stall meter is scaled to line up with the other
  host meters, and its value reads plainly.** Its levels
  (`DASH_IO_FULL_WARN_PCT` / `DASH_IO_FULL_BAD_PCT`, defaults 5 and 15)
  are far below 100, so on a 0-100 % bar its warning tick sat at the
  far left and progress toward it was hard to see. The bar is now drawn
  piecewise: 0..warn fills 0..80 % of the bar, warn..bad fills
  80..100 %, and bad or more fills it, putting the tick on cell 12 —
  the same column as every other host meter's. Colours still follow the
  real value against warn/bad and the right-hand text keeps the true
  60 s average. The value text also drops the PSI term "full" for the
  plain "stalled": `0.4% stalled` on the meter and `disk i/o stalled
  N% of the last 60 s` in the notification. The `DASH_IO_FULL_*`
  variable names are unchanged.

## [2.9.1] - 2026-10-09

### Fixed

- **A planned orchestrator restart no longer loses leases to a
  not-yet-ready orchestrator.** The `ExecStartPost` drain resume ran
  before the restarted orchestrator accepted sandbox creates, so a
  resume failed with `Unavailable`/`connection reset`; the short attempt
  budget was spent and the lease was marked `lost` — the worst outcome
  on a planned restart. `POST /api/admin/undrain` now waits for the
  orchestrator to answer both `NodeInfo` and a `List`
  (`UNDRAIN_READY_TIMEOUT`, default `180s`) before the first resume, and
  the e2b client maps gRPC `Unavailable` (a connection reset, an
  unexpected EOF, the server not serving yet) to the retryable
  `substrate.ErrUnavailable`. An indeterminate transport error is
  retried with backoff for `UNDRAIN_RESUME_WINDOW` (default `5 min`) and
  never counts toward losing the lease. A resume that exhausts its
  retries is left suspended with reason `resume_failed` and its
  `drained` flag — its snapshot intact — so the holder's next work call
  retries it through resume-on-use and the self-heal loop keeps trying;
  only a permanent error (the image or build is gone) still marks the
  lease `lost` (spoond-638d).
- **Drain-resume outcomes are tidy on a planned restart.** A drained
  lease another path already lost is no longer stamped `resume_failed`
  and left `drained` for the heal loop to retry for 24 h: its
  `drained` flag is cleared quietly, no `drain_deferred` is emitted for
  a released lease, and a lease a concurrent resume brought back
  running is left alone. A gRPC `Internal` envd start error whose text
  happens to say "connection refused" now uses the bounded retry
  budget instead of the long transport window. A resume refused because
  the substrate is unavailable answers the retryable `503
  substrate_unavailable` with `Retry-After: 5` (not a generic `500`),
  and a lease left suspended with reason `resume_failed` now emits a
  `suspended` event and a lease-journal line (spoond-638d round 2).
- **A failing drain resume no longer looks like a fresh suspension on
  every retry.** The self-heal loop calls the resume-failed stamp every
  interval for a lease whose resume keeps failing, but only the call
  that changes the reason to `resume_failed` emits the `suspended` event
  and the lease-journal line. Later retries keep the lease suspended
  without emitting, so a consumer never reads one failing lease as a new
  suspension each interval (spoond-hfko).

### Changed

- **Conformance F2, S1 and N7b follow resume on use.** They expected the
  pre-2.9 `409` on a suspended or paused lease. They now expect the
  lease to come back on the holder's work call and check that it does
  (spoond-clcw). The admission wake test installs its pass hook before
  the create, so it no longer races (spoond-qjsy).

## [2.9.0] - 2026-10-09

A suspended lease now comes back on its holder's next work call,
whatever suspended it, and every resume refusal has one shape: `503
capacity_wait`, `429 quota_exceeded` or `409 lease_busy`, with the
lease left suspended and never marked lost. An orchestrator stall
answers a retryable `503 substrate_unavailable` instead of reporting a
sandbox gone. The CI runner waits for room on a full node instead of
failing the job, and enforces a job's `timeout-minutes`. Deleting a
user releases their leases and drops their named snapshots and kept
builds. Background jobs get a runtime cap, exec and stream bodies are
bounded, every automatic suspend names its reason, and a lease journal
records every lease's life in the backend's journal. Store migrations
0020 and 0021 are additive.

### Status code changes

A summary for clients; the details are in the entries below and in
[docs/api.md](docs/api.md).

- **Resume on use.** Exec, exec stream, files, guest dial, the proxy,
  jobs, the LLM gateway, `POST …/network` and `POST …/prompt` resume a
  suspended lease instead of answering `409 lease_suspended`. A resume
  they cannot do answers `503` `capacity_wait` with `Retry-After` (no
  room on the host, or a drain in progress), `429` `quota_exceeded`
  with `Retry-After: 30` (the owner's own memory quota), `409`
  `lease_busy` (a pause or another resume is in flight; retry) or `410`
  `lease_lost`. `POST /resume` and the SSH gateway's resume answer the
  same shapes. The stat probe, fork and the crash test still answer
  `409 lease_suspended`.
- **`503 substrate_unavailable`** with `Retry-After: 5` when the
  orchestrator cannot confirm a sandbox's state, on exec, stat,
  background exec, `PUT` file and directory remove, and job signal.
  These answered `410` (exec, stat, background exec), `404` (files) or
  `409` (job signal) before. A confirmed absence is unchanged.
- **`DELETE /api/users/{id}`** answers `200` with a `removed` body
  (was `204`), and `500` with `"incomplete": true` and the failed
  `step` when the cleanup could not finish, or `500 read state: …` when
  the store cannot be read after a restart.
- **A create's plain capacity `503`** (`capacity: …`) now carries
  `Retry-After: 30`, like the burst-reserve and preemption refusals.
- **An oversized exec body** answers `413`; an oversized exec-stream
  first frame closes the socket with code `1009`.

### Changed

- **A suspended lease resumes on its holder's next work call; the
  background preempt auto-resume is gone (#145 D2, spoond-1tb5).** Every
  automatically suspended lease — pressure, preemption, the plain idle
  sweep, the lease's own `idle_suspend`, held rule 1 and a lapsed hold
  (rule 3) — and one its holder suspended by hand resumes on its
  holder's next work call: exec, exec stream, files (read/write/stat/
  list), the proxy and guest dial, jobs, the LLM gateway, a network
  change and a prompt. This is now **one rule for every kind of
  suspend**, not just `idle_suspend`. GET, status, events and SSE never
  resume. `POST /resume` is the explicit resume: it and the SSH
  gateway's resume take the same path and answer the same refusal
  shapes (below), including `503 capacity_wait` during a drain or a
  pending drain clear and `429 quota_exceeded` with `Retry-After: 30`
  for the owner's quota. A lease suspended by a
  non-work path that cannot resume (the `stat` probe, forking a running
  source, the crash test) still answers `409 lease_suspended` with its
  `reason` — `lease_suspended` no longer appears on any work path.

  - **One refusal shape.** A resume-on-use that finds no room on the
    host answers the one shape every path shares: `503` with a
    `Retry-After` header and JSON `{"error": ..., "code":
    "capacity_wait"}` (was `503` with a plain `error`, or `409
    lease_suspended` on paths that did not resume). A host-structural
    shortage (disk, hugepages, the burst reserve, the snapshot store,
    orchestrator capacity) is a wait, never a refusal: spoond acts and
    the caller retries. The owner's own memory quota on a resume is
    `429` with `Retry-After: 30` and JSON `code: quota_exceeded` (the
    quota frees when the owner releases its own leases); a
    `kept_budget` refusal stays `409`. While a pause or another caller's
    resume is in flight a work call answers `409` with `code:
    lease_busy` (retryable). This also unifies `POST /resume`, where the
    preemption-disk-floor and burst-reserve bodies now carry
    `capacity_wait`.
  - **No resume during a planned-restart drain.** While spoond is
    draining the node (or still owes a drain clear), a resume-on-use,
    `POST /resume` and the SSH gateway's resume all refuse with the
    shared `503` `capacity_wait` + `Retry-After` shape. The lease stays
    suspended and `Drained`, so no resume races into a node that is
    stopping and the drain's "no running sandboxes" wait is not broken.
    The undrain clears the drain first and resumes the drained leases as
    before.
  - **No resume failure marks a lease `lost`.** The preempt-resume
    budget (`PREEMPT_RESUME_RETRIES`) and its lose-after-N-failures path
    are removed, along with the `runPreemptResumeLoop`,
    `resumePreempted` and `losePreempted` code. The error goes to the
    caller and the lease stays suspended with its snapshot intact.
    `promoteAllBurst` keeps its own 15 s ticker. A preempted lease's
    victim choice is unchanged; only its background auto-resume and its
    loss are gone. `PREEMPT_RESUME_RETRIES` and its docs are removed
    (the environment variable is ignored). `spoond-dxq`'s
    crash-recovery retries are unchanged.
  - **A preempted held lease is subject to held rules 2 and 5 like any
    other rule-suspended lease.** The old exemption ("waits for the
    resume queue") is gone with that queue: a preempted held lease
    untouched for `HeldSuspendedRelease` is released by the stale rule,
    and one under critical disk pressure is rule 5's victim.
  - A lease whose hold lapsed (rule 3) resumes with its hold still
    lapsed: resuming does not renew a hold. See
    [docs/api.md](docs/api.md) for the full per-path status table.

### Added

- **A durable lease journal (spoond-puqp).** Every lease create,
  release, lost and suspend (including preemption) now writes one
  logfmt line to the backend's journal, so a lease-to-sandbox mapping
  and the reason a lease went can be reconstructed from `journalctl`
  alone — even after the dashboard's 50-event ring has rolled over and
  the lease row is gone (the 2026-10-08 incident). The line names the
  operation, the lease id, the owner id (never the token), the sandbox
  id, the image, the admission class and a reason; an empty value is
  `""` and a value with a space, quote, equals sign or control
  character is quoted, so a field splitter cannot be confused. The
  release reason is canonical: the owner-named API deletion
  (`deleted via API by <owner>`), `ttl`, `lost grace expired`,
  `user deleted`, `disk`, `idle` or `hold_lapsed`, with a caller's own
  reason (a CI job's `ci job <id> …`) kept verbatim; a create says
  `new`, `snapshot <name>@<version>`, `clone of <id>` or
  `fork of <id>`; a suspend says its automatic reason (`idle`,
  `idle_suspend`, `hold_lapsed`, `pressure`, `preempt`), `drain` or
  `hand`; and a lost lease carries the same reason as its `lost` event.
  The line carries no secret material. See
  [operations.md](docs/operations.md#the-lease-journal).

- **Every automatic suspend names its reason, policy step and build
  (spoond-9gm2).** A `suspended` event now carries structured fields
  beside its human detail — `reason`
  (`idle`|`idle_suspend`|`hold_lapsed`|`pressure`|`preempt`),
  `policy_step` (the pressure order's step name, empty until that order
  names steps) and `build_id` (the pause build) — and the lease object
  gains `suspend_reason`, `suspend_policy_step`, `suspend_build_id` and
  `suspended_at` (additive, omitted while unset). Every automatic
  suspend names it: the plain `IDLE_TIMEOUT_SECS` sweep (`idle`), a
  lease's own `idle_suspend` (`idle_suspend`), a lapsed hold
  (`hold_lapsed`), held rule 1 shortened under pressure (`pressure`) and
  preemption (`preempt`). A hand or drain suspend carries none of the
  four — it has no automatic reason. A `409`
  `lease_suspended` body adds `"reason"` when the suspension was
  automatic. All four fields are cleared on resume, restore, cold restart
  and crash recovery; `lease.held_action` is unchanged. Migration
  **0021** adds the four `leases` columns. See
  [docs/api.md](docs/api.md).

- **Pause-chain size is measured before any compaction (spoond-p9j).** A
  pause build's parent is the build it resumed from and the GC keeps
  every ancestor of a live lease's resume build, so a persistent lease
  that suspends repeatedly accumulates one memory snapshot per pause
  until a cold restart (or the lease's release) breaks the chain.
  `GET /api/leases/{id}` now reports `chain_depth` (builds in the
  lease's chain) and `chain_bytes` (the lease's parent chain's summed
  recorded `size_bytes`, including shared ancestors such as the template
  root, and ignoring `build_refs`), and every pause observes the same
  two numbers as the unlabeled `spoond_pause_chain_depth` and
  `spoond_pause_chain_bytes` histograms once its build has settled —
  bounded cardinality, so the per-lease figures stay on the lease API.
  No compaction happens yet; a follow-up decides on automatic compaction
  after measuring.

- **A cap on background job runtime (spoond-wb5).** A background exec job
  may no longer run forever: `JOB_MAX_RUNTIME` (default 24 h, a Go
  duration or seconds) is the host cap, the exec body's new optional
  `max_runtime_secs` can ask for a shorter one — never a longer one, and
  a negative value is `400`. Past its effective cap the job's process
  group is killed first (the same path as
  `POST …/jobs/{job}/signal`) and only then is the record marked exited
  with reason `timed_out` and exit code `124`; a failed kill leaves the
  record running so the next reconcile retries, rather than orphaning a
  live process the counts no longer see. A `job_exited` event whose
  detail starts `timed out: exit 124` tells the owner the cap, not the
  command, ended it (`reason` is also on the job record, and
  `spoond_jobs_exited_total` gains the `timed_out` result). The cap is
  wall-clock from the job's start: while the lease is suspended — or
  busy with an in-flight pause, resume, restart or restore — reconcile
  leaves the job running (the guest cannot be signalled), and the first
  reconcile after a resume kills a job whose cap was spent in the
  meantime — so a `sleep infinity` can no longer pin a lease's memory
  and hugepages forever. A running record written before the cap existed
  (`max_runtime_secs` 0) is capped by the current host value. An
  oversized `max_runtime_secs` is clamped to the host cap rather than
  overflowing the seconds conversion into "uncapped".
  Migration **0020** adds `lease_jobs.max_runtime_secs` and
  `lease_jobs.reason`. A job on a suspended lease also no longer keeps
  that lease active: `reconcileJobs` stopped calling `markActive` on a
  suspended lease, so the held-lease rules' untouched test still sees
  the suspension. A negative `JOB_MAX_RUNTIME` disables the cap entirely;
  a negative value under one second is rejected at startup rather than
  truncating to the default.

- **Deleting a user now releases their leases and drops their named
  snapshots and kept builds.** `DELETE /api/users/{id}` used to remove
  only the identity, leaving the user's leases running, their named
  snapshots and kept builds pinning disk and their jobs with nobody to
  charge (an owner with no user has no quota). It now releases every
  lease (reason `user_deleted`), cancels every running job, drops every
  named snapshot and unpins every kept build, logs each step and emits
  one `user_deleted` event naming the counts. A create of that user
  still waiting in the admission queue, or one that raced the delete, is
  refused (`403 owner deleted`) instead of granted, since an owner with
  no identity row has no quota. The response changed from
  `204` to `200` with `{"removed": {user, leases, jobs, snapshots,
  kept_builds}}`, so a caller can see exactly what was cleaned up
  (spoond-q4j).

### Fixed

- **Guest log lines are dropped, not flooded, when no collector is
  configured (spoond-99c8, e2b-runtime P10).** `LOGS_COLLECTOR_ADDRESS`
  is unset on spoond hosts, but envd in every guest still posts log
  lines to the orchestrator's `/logs` because its MMDS address is always
  set. With an empty primary URL the fork's handler forwarded each line
  to nothing and logged one ERROR per failure — roughly 29k lines/hour,
  enough for journald to suppress real orchestrator errors (it hid the
  2026-10-08 CI incident). `Logs` now resolves the route first and, when
  the primary URL is empty and there are no shadow URLs, answers `200`
  and records a `primary`/`dropped`/`no_collector` metric instead of
  forwarding, mirroring how the sandbox logger disables itself on an
  empty collector address. Bundled with 2.9 (orchestrator swap first).

- **A runner job no longer fails when the node is full: the create
  waits for admission and retries capacity refusals (spoond-r739).** A
  `create` sent no `"wait"`, so the backend's admission queue (#129)
  was skipped and a `503` on a full node failed the job at once — no
  sandbox, no task logs, `dbfs actions_log` missing. Every runner
  create now sends `"wait": RUNNER_ADMIT_WAIT_SECS` (default `900`,
  `0` = no wait) and a `503` is never a job failure: the runner logs
  the wait and retries per the response's `Retry-After` (or 30 s)
  until the create is admitted or `RUNNER_JOB_TIMEOUT` ends the job.
  While a create waits it sends `Sink.Keepalive` every minute and a
  `waiting for capacity on spoond (N s)` log row, so Forgejo does not
  reap the silent task. `RUNNER_JOB_TIMEOUT` (duration or seconds,
  default `6h`) bounds the whole job, the wait included, so a wait
  cannot pin a worker forever. The lease client's HTTP timeout grows to
  cover the admission wait. A plain
  `503 capacity: …` refusal now also carries a `Retry-After: 30` like
  the burst-reserve and preempt ones, so a client that sent no `wait`
  still knows when to come back. A job granted a lease in the moment
  its client disappears is released (`client_gone`) instead of leaking
  until its TTL, and a job still waiting for a sandbox when the runner
  drains is cancelled at once instead of spending `RUNNER_STOP_GRACE`.
  See [docs/operations.md](docs/operations.md).

- **The CI runner now enforces a job's `timeout-minutes` (spoond-r739).**
  It follows GitHub Actions: the timeout counts the job's execution
  time from sandbox creation, and the admission wait is excluded. A job
  that runs past it is cut and reported cancelled. `RUNNER_JOB_TIMEOUT`
  (default `6h`) bounds the whole job, the wait included, and caps a
  larger `timeout-minutes`. The field parses leniently: a number
  (integer or fractional) is honoured, an expression or string (e.g.
  `timeout-minutes: ${{ matrix.t }}`) is ignored with one log line
  instead of failing the whole workflow, and a value that is not
  finite, is zero or less, or is above `1e6` is ignored so it cannot
  overflow the duration and remove every bound. Known users: the
  hrmny `e2e-live.yml` live job (75) and `ci.yml` (45/45/20).

- **A user delete that cannot finish now says so (spoond-y0jj).** When a
  cleanup store step failed — listing a user's running jobs or kept
  builds, unpinning their kept builds, or dropping their named snapshots
  — `DELETE /api/users/{id}` logged it, still answered `200` with empty
  lists and left the rows behind as GC roots, so an admin had no reason
  to retry. It now answers `500` with the partial `removed` body,
  `"incomplete": true` and the failed `step` (`list_jobs`,
  `list_kept_builds`, `unpin_kept_builds` or `drop_named_snapshots`),
  while the other steps still run so the body lists what was removed; a
  retry completes the cleanup. A retry after a backend restart also
  survives a busy store: the in-memory deleted-owner mark is gone, so
  the delete reads the store to tell a never-existed id (`404`) from a
  half-cleaned one, and answers `500 read state: …` when that read
  fails instead of a `404` that hides the leftover rows. A create for
  an owner already marked
  deleted is now refused before its admission ticket parks, instead of
  waiting out a deadline the admission pass would only refuse at the
  end. `docs/api.md` also notes that the repeat-delete `200` holds only
  until a backend restart: the deleted-owner mark is in memory, so a
  repeat delete of a fully cleaned user answers `404` after a restart.

- **A queued create no longer stalls for a full tick when a wake-up
  arrives while an admission pass is already running (spoond-vbdj).** A
  release (or pause, preemption, quota change) credited capacity to a
  full node while the wake-up goroutine was between its "no room"
  judgement and its end; the old `wakeScheduled` compare-and-swap then
  dropped the release's wake-up, so the waiting create sat until the
  periodic retry (the `admitQueueTick` constant, 5 s in production,
  or an hour in some tests). Every wake-up now sets a `wakePending`
  flag, the pass loops until it has absorbed every wake that arrived
  during it, and it
  releases `wakeScheduled` with a compare-and-swap ordered so a wake
  landing in the gap either feeds the running pass or starts a new one.
  At most one wake-up goroutine still runs and callers stay
  non-blocking. A deterministic regression test holds a pass inside
  `tryAdmitQueued`, frees room and wakes, then releases the pass and
  asserts the ticket is admitted with no tick.

- **User-delete cleanup follow-ups (spoond-q4j).** A create of a deleted
  user that was parked on a quota cap (`max_leases` or memory) answered
  the cap's `429` when the delete refused it, because the queued-create
  refusal wrapped the original error and the quota checks were tested
  first; it now answers `403 owner deleted`. A ticket parked after the
  delete's queue cancel already ran is refused at its next admission
  pass instead of waiting out its deadline. Deleting a real user twice
  now answers `200` with empty removed lists, matching the documented
  idempotence (the second call had answered `404` once the identity was
  gone); an id that was never a user still answers `404`. Restoring a
  suspended lease whose owner was deleted answers `403 owner deleted`
  instead of `500`. The owner-deleted grant refusal also drops the
  lease's remembered egress config, like every other failed create.

- **The host-address guard now follows the host's current addresses
  (spoond-6s9, e2b-runtime P9).** The fork's `tcpfirewall` computed its
  host-address set once at package init and from IPv4 addresses only, so
  a slot veth address or a host IPv6 address that appeared later was not
  guarded: an unscoped allowance such as `10.0.0.0/8` could reach
  host-local veth IPs and host services listening on `0.0.0.0`/`::`, on
  both the CIDR and the domain path. The unspecified addresses `0.0.0.0/8`
  and `::` are now always refused, and every IPv6 form that embeds an IPv4
  address — IPv4-mapped, IPv4-compatible `::/96`, NAT64 `64:ff9b::/96` and
  6to4 `2002::/16` — is normalized to that address before the host check,
  so a guest `Host`/SNI of `0.0.0.0` or an allowed domain resolving to
  `0.0.0.0`/`::` can no longer reach host loopback listeners. The guest's
  own allowed/denied CIDRs still see the original destination, so
  normalization cannot widen them. The set is
  recomputed from the live interfaces at most every 3 s, covers IPv4 and
  IPv6 (and a resolved-IPv6 zone id is stripped), and keeps loopback and
  link-local refused. The snapshot is published through an atomic pointer
  and refreshed single-flight with a non-blocking lock, so a decision never
  waits on the interface syscall (a reader that loses the refresh race
  serves the expired snapshot). A failed interface read keeps the last good
  set for one extra
  TTL and then fails closed, logging and counting the failure once per
  TTL; the domain path's resolved-IP check consults the same guard.
  The fix is in the orchestrator fork and needs its matching build.

- **The host-address guard follow-ups: the private allowance sees the
  original IP, and the first host enumeration is blocking
  (spoond-kh29, e2b-runtime P9a).** Two low-severity findings from the P9
  review. First, a port-scoped private allowance naming a host's IPv4
  address (`10.0.0.11/32` on port 8080) admitted a 6to4/NAT64 form of that
  address (`2002:a00:b::1:8080`), whose dial goes to the IPv6 address, even
  under `denied_cidrs ::/0`; the allowance is now matched against the
  original destination while the host-address check keeps seeing the
  normalized one. Go's `IPNet.Contains` still matches an IPv4-mapped
  `::ffff:` address against an IPv4 allowance. Second, the host-address set
  was published as an always-denied-only snapshot and refreshed lazily, so
  a decision that lost the first `TryLock` was checked against that partial
  set until the first enumeration published; the constructor now performs
  one blocking enumeration, so a successful read publishes the real set
  before any decision and a failed read publishes a fail-closed set that
  the TTL refresh retries. Also in the orchestrator fork.

- **The exec and stream request bodies are bounded (spoond-mrbr).**
  `POST /api/leases/{id}/exec` decoded its JSON body with no size bound,
  so one authenticated caller could send a multi-GB `cmd` or `secrets`
  object and the server held it all in memory; the stream WebSocket's
  first frame had the same gap. The exec body is now wrapped in
  `http.MaxBytesReader` and the stream sets a read limit, both under
  `MAX_EXEC_BODY_BYTES` (default 8 MiB, which leaves room for a large
  `cmd` and `env` next to the 64 KiB secrets cap). A bigger exec body
  answers `413` with a JSON error; a bigger stream first frame closes
  the socket with code `1009`. The body is read before `ensureRunning`,
  so an oversize request no longer resumes an idle-suspended lease.
  This bounds host memory only: the guest's own argv limit is about
  128 KiB per string, so a larger `cmd` still fails with `Argument list
  too long` from the guest shell.

- **Background-job max-runtime follow-ups (spoond-wb5).** A kill that
  succeeds but whose store write that marks the job `timed_out` fails no
  longer leaves the record running: the in-memory timed-out intent is
  kept, so the next reconcile records the outcome without signalling a
  process that is already gone. `JOB_MAX_RUNTIME` is held as whole
  seconds, so a positive fractional duration rounds up (`500ms` is 1 s,
  not a 0 that reads as the default, and `1500ms` is 2 s, not a
  truncated 1 s) and a bare integer too large to represent is clamped
  instead of overflowing the seconds conversion. A synchronous exec with
  a negative `max_runtime_secs` now answers `400` (it was accepted and
  ignored), while a non-negative value on a synchronous exec is still
  ignored. A job whose `pid` file never appears is retried on a backoff,
  so it no longer costs the pid wait on every reconcile pass.

- **Conformance N1 probes a configurable LAN target, and I3 no longer
  depends on a stale apt index.** N1 hardcoded `10.0.0.203:443` as the
  private destination for the `internet` and `lan` policies; that box was
  gone, so the case failed even though the substrate was correct. The
  target is now `CONFORMANCE_LAN_TARGET` (a private `host:port`, no
  default): set it in `/etc/spoond/conformance.env`; N1 skips with
  a clear message when it is unset, like the N9 `CONFORMANCE_MIXED_*`
  knobs. I3 installs `docker.io` into `dev-base` after `apt-get update`,
  so it no longer 404s on a package version the Ubuntu archive dropped.

- **A stale template build's failure is now announced.** The GC's
  stale-building sweep emits a lease-less `gc` event per row it fails
  (in dry-run too), naming the build and the timeout, because a template
  build has no owner and never appears in `/api/snapshots`; the sweep
  still logs it and moves the row on to an ordinary candidate. It relies
  on the `spoond_builds_in_flight` gauge (2.8.0) and the sweep guard
  reading the same catalog count. `SPOOND_BUILD_TIMEOUT` now wires
  `ServiceConfig.BuildTimeout` from the environment instead of being
  hardcoded, so the backend's stale-row sweep can be tuned to match
  `spoond images build`; both processes read the variable from their own
  environment and must each be given the same value (see
  `docs/setup.md` and `deploy/PRODUCTION-ENV-2.7.md`).

- **A release that lands around an idle or held suspension no longer
  counts or announces it.** `recordIdleSuspend` and the held rules
  re-check the released flag under the store lock before bumping
  `spoond_idle_suspends_total` / `spoond_held_actions_total`, saving
  `last_action` or emitting `idle_suspended` / `held_action`, and the
  idle event is emitted inside that critical section, so a release in
  the window cannot leave a suspension event on the stream for a lease
  that is gone. A release that raced the pause itself is no longer
  logged as an idle-suspend or held-rule error, and the checkpoint
  cleanup log keeps the lease and the unreferenced build. A checkpoint
  whose `List` returns nothing — a cancelled request, a failing node —
  now deletes the source sandbox id directly instead of trusting the
  list, so a released lease's resumed guest is stopped rather than left
  running (spoond-15i).

- **Per-lease memory maps no longer leak, and the response to a leaked
  auth IP is bounded.** Releasing a lease now drops its applied-egress
  memo, its deferred exec-time secret removals and the queued auth
  failures of an IP an attacker never retries; `DeleteBuildsPermanently`
  only removes a build's refs while the build row is still `deleted`;
  and `refreshPeers` reaps any memo whose lease is no longer live and
  unreleased, so a refresh or network-policy update that races a release
  cannot re-add one (spoond-966, follow-ups). The same release also
  bounds long-deleted `builds` rows and lost job records. No API change;
  no store migration.

- **A refresh mid-create no longer drops a lease's egress memo.**
  `createSandbox` records the applied-egress memo before the lease
  enters the store, so a `refreshPeers` landing in that gap pruned it as
  a memo with no live lease, and the next pass re-applied egress once
  needlessly (idempotent, never a wrong skip). The lease ids of creates
  in progress are now tracked from before the memo is written until the
  lease is registered or the create has cleaned up, and the prune skips
  them (spoond-ob18). `DeleteBuildsPermanently` removes a build row and
  its `build_refs` in one transaction guarded on the row still being
  `deleted`; the deferred secret-removal and applied-egress guards now
  test lease presence rather than liveness, so a suspended lease's
  bookkeeping is not discarded; and the orphan reap's dry run logs the
  quarantine restore it would do (spoond-ob18 NITs).

- **`ORPHAN_REAP` modes now mean exactly what the docs say.** `off` is a
  hard stop: instead of still purging expired quarantined directories,
  it leaves the snapshot disk entirely alone. `dryrun` logs what it
  would reap — including what it would purge from quarantine — and
  changes nothing. Only `quarantine` moves, restores and deletes. An
  operator who set `off` to keep the GC's hands off the disk now gets
  that, and an expired quarantine waits for a pass in a mode allowed to
  purge it (spoond-ob18).

- **A clone or fork of a deleted user can no longer commit an
  ownerless lease.** A clone (or a fork child) reserves quota and then
  spends seconds checkpointing; a `DELETE /api/users/{id}` racing that
  window released the source and finished, after which the clone or
  fork committed an ownerless, uncapped lease. Both now re-check the
  owner-delete mark under the store lock at the commit: a deleted
  owner's clone is refused and its fresh sandbox stopped, and a fork
  rolls back every child (each sandbox stopped exactly once). A named
  snapshot save that reached its row insert after the owner's snapshots
  were dropped is refused the same way, so no row outlives the user as
  a GC root. A user delete of an id that is neither a known identity
  nor still owns any state now answers `404` (it used to silently
  succeed); an id that is a legacy token-map owner answers `409` and is
  left untouched (`RemoveUser` is idempotent, so retrying a partial
  cleanup still works). Clone, fork, restart and resume now map the
  refusal onto `403 owner deleted` instead of a `500`, and the refused
  grant drops its staged create-time secrets.

- **An orchestrator stall no longer reports a sandbox as gone
  (spoond-g077).** A failed substrate operation (an envd stream drop, an
  exec error, a file-path failure) was mapped to `substrate.ErrNotFound`
  whenever the orchestrator `List` call failed, so a stream drop during
  an orchestrator stall became `404`/`410 lease_lost`. `410` is final for
  clients: Honey treats it as gone with no confirming GET, and the runner
  fails the job permanently. The e2b client now distinguishes *List
  failed* (the sandbox's state is unknown) from *listed and absent*. On
  unknown it retries `List` a bounded number of times with a short
  backoff and then returns the new `substrate.ErrUnavailable`; the API
  answers **`503` with `Retry-After: 5` and code `substrate_unavailable`**
  (a retryable answer) and **never marks the lease lost**. The rootfs
  liveness probe never counts an `ErrUnavailable` probe toward its
  3-strike recovery, so an orchestrator stall cannot delete a live
  sandbox. A confirmed absence is unchanged: `410 lease no longer
  exists` on exec/stat/background exec and `404` on files. See
  [docs/api.md](docs/api.md).

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
