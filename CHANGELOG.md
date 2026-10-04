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

- **Readiness endpoint for uptime monitors (#81).** `GET /readyz` on the
  lease API listener (no auth, like `/healthz`) answers
  `200 {"status":"ok"}` only when the orchestrator's node info reports
  healthy, the catalog answers a trivial query, and the snapshot disk and
  hugepage pool are below the dashboard's danger levels (90 % / 92 %);
  otherwise `503 {"status":"fail","checks":[…]}` names each failing
  check with a reason. Every check is bounded to 2 s and the whole
  answer is cached for 5 s, so an external poller costs nothing. The
  web dashboard serves `GET /readyz` the same way: 200 only when its
  sources (the metrics scrape, the catalog, the identity store) answer.
  `docs/operations.md` gains an example Gatus configuration watching
  both `/readyz` endpoints and the SSH gateway port, with conditions on
  status, response time and certificate expiry.

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
