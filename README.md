# spoond

Fast, isolated, ephemeral compute for people and agents: a lease API in
front of **Firecracker microVMs run by E2B's orchestrator**. Consumers
request a sandbox, run work in it, and release it. Every sandbox starts
as a memory-snapshot restore, so starts are warm, and fork, pause/resume
and checkpoint are native. On top: an SSH gateway, an HTTP proxy, an LLM
gateway, an MCP server for agents, a Forgejo Actions runner and a
read-only dashboard.

```
spoond (this repo, one Go binary)          e2b-orchestrator (our fork of
┌──────────────────────────────────┐       e2b-dev/runtime, patch queue)
│ lease API            :8890       │ gRPC  ┌──────────────────────────────┐
│ HTTP proxy, LLM gateway :8891    │ ────▶ │ Firecracker microVM lifecycle│
│ SSH gateway + ctl    :2222       │ :5008 │ memory snapshots (UFFD), NBD │
│ dashboard (read-only) :8893      │       │ rootfs, netns per sandbox,   │
│ MCP server for agents            │ envd  │ egress firewall, templates   │
│ Forgejo Actions runner           │ ────▶ │ envd in every guest (:49983) │
│ SQLite state (leases, catalog)   │ :5007 └──────────────────────────────┘
└──────────────────────────────────┘
images/*.dockerfile → docker → local registry → E2B templates (spoond images)
```

spoond 2.0 runs on E2B's orchestrator; forkd, the 1.x substrate, is
removed. spoond is a generic microVM utility: it takes no position on
how agents work, and agent workflow lives in the separate Honey project,
which drives spoond through the lease API. The design, decisions and
per-unit specs are in
[docs/plans/2026-09-30-e2b-substrate/](docs/plans/2026-09-30-e2b-substrate/00-README.md),
and every change is in [CHANGELOG.md](CHANGELOG.md).

## What it gives you

- **Lease API**: `POST /api/leases` (image, size, TTL, persistent,
  network policy, exposed ports, holder, secrets; `/api/sandboxes` is kept as an
  alias), then exec, stream (WebSocket PTY), keepalive, suspend/resume,
  restart (warm or cold), checkpoint, restore, fork, clone, tag, comment,
  delete. Auth via bearer
  tokens (`CONSUMER_TOKENS=token=owner,...`) or per-user identity tokens.
  Leases and the image catalog persist in SQLite, so a backend restart
  loses nothing. On every lease:
  - **Files**: `/api/leases/{id}/files/{path}` puts, gets, stats, makes
    and removes files in the guest, up to 256 MiB per file.
  - **Background jobs**: `POST /api/leases/{id}/exec` with
    `"background": true` starts a tracked job (202 + `job_id`);
    `GET …/jobs/{job}?wait=<s>` long-polls to its exit, and
    `…/jobs/{job}/output` follows stdout/stderr by byte range, while
    `job_started`/`job_exited`/`job_lost` ride the event stream.
  - **Crash test**: on a host with `CRASH_TEST=1`, `POST
    /api/leases/{id}/crash-test` sends the caller's own lease through
    crash recovery (back from its last checkpoint with a new generation,
    or lost without one), so a client can test how it survives a crash.
  - **Guest port dial**: `GET /api/leases/{id}/ports/{port}/dial` opens
    raw TCP to any port in the guest over a WebSocket (a database's own
    protocol, a debugger, a REPL), under every network policy.
  - **Secrets as files**: a `secrets` object on create or exec becomes
    0600 files under `/run/secrets` on a guest tmpfs, never environment
    variables, argv, the store or logs.
  - **Event stream**: `GET /api/leases/events` (or one lease's
    `/events`) streams lifecycle events over SSE, with resumable
    positions.
  - **Generations**: a `generation` counter, also in
    `/run/spoond/generation` in the guest, bumped when the guest's memory
    did not continue (crash recovery, a cold or non-persistent restart, a
    restore), so a client can tell its processes were restored.
  - **Checkpoints on request**: no periodic checkpoints unless the lease
    asks (`checkpoint_interval`); `POST /checkpoint` any time, with
    `keep: true` to pin it (at most `MAX_KEPT_PER_LEASE`, default 4,
    within the owner's fair disk slice), and `POST /restore` puts the lease
    back to a kept checkpoint in place. `restart?mode=cold` gives the lease a fresh
    guest from its image, keeping its id.
  - **Named snapshots**: `POST /api/leases/{id}/snapshots` saves a live
    lease as a named, versioned checkpoint that outlives it (`name` or
    `name@v`); a later lease starts from it with `"snapshot":
    "spoond/warm@3"` (the version's memory, a new lease id and generation
    `1`). Saves are idempotent by an `idempotency_key`, the `/run/secrets`
    files are scrubbed before the checkpoint, and a name keeps its last
    `SNAPSHOT_KEEP_VERSIONS` versions. `GET`/`DELETE
    /api/named-snapshots/{name}[@v]` list, show and delete them. An
    owner may grant a name to another user
    (`/api/named-snapshot-grants`), who starts from it with
    `"snapshot_owner"`; only versions saved from a lease that never had
    a secret can be shared, and a lease started from a snapshot can
    restore back to it. On the
    source, spoond writes `/run/spoond/last-save`; on a lease started
    from a snapshot it writes `/run/spoond/lease-id`,
    `/run/spoond/generation` and `/run/spoond/started-from`, so a
    restored process can tell which side it is on — see
    [api.md](docs/api.md#identity-in-a-restored-guest).
  - **Holders and pins**: `holder` and `holder_url` are plain labels
    saying what holds a lease (a CI job, an orchestrator's run,
    someone's scratch work). A **pinned** lease (`pinned` on create, or
    `PUT/DELETE /api/leases/{id}/pin`) is never paused or deleted by
    spoond before its own expiry; take-back touches only unpinned
    leases, and every paused lease is released 30 days after its pause
    date by one clock. See
    [operations.md](docs/operations.md#pins-and-the-paused-release-clock).
- **Multi-user tenancy**: people and agents are first-class identities,
  with per-user SSH keys, per-user tokens, a per-user `max_ttl`, an
  equal fair share of memory and snapshot disk among active owners (no
  per-owner caps since 3.0), admin roles, lease sharing with expiry,
  per-user LLM gateway keys, and per-user proxy hostnames
  (`<label>.<user>.sandbox.example`). See
  [docs/security.md](docs/security.md) and the [Users & identity API
  section](docs/api.md#users--identity).
- **Fair shares**: every owner holding a running or paused lease gets
  an equal, floating 1/N slice of memory and snapshot disk (N counts
  active owners only — an identity that never took a lease does not
  split the box, and a lost lease never counts); an owner may borrow
  past it while the box has room. When a request does not fit, spoond
  takes room back from the biggest borrower (highest usage over
  slice): memory take-back
  pauses that owner's least recently used unpinned lease, and disk
  take-back releases their oldest unpinned paused lease. A shortage
  answers `503 capacity_wait` with `Retry-After`; a request take-back
  cannot serve (every candidate pinned, or the caller is itself the
  biggest borrower) answers `429 box_full`. `GET /api/usage` shows an
  owner's slice and ratio, lease and job counts, its creates waiting for
  room with their place in the queue, and its recent refusals and
  take-backs; `GET /api/usage/history` keeps 7 days of 15-minute
  samples. `GET /api/fair-shares` (admin) shows every owner's. A create may **wait** for room (`"wait": <seconds>`, up to
  `MAX_ADMIT_WAIT_SECS`), served lowest ratio first; a persistent lease
  may set `idle_suspend` to give its memory back when idle. See
  [upgrading-3.0.md](docs/upgrading-3.0.md).
- **Per-lease sizes**: a create picks its VM's size by preset
  (`"size": "large"`, listed by `GET /api/sizes`) or by `vcpu` and
  `memory_mb`; the image gives the default and a minimum. Sizes with a
  warm build (the image default and the presets an image lists in
  `warm_sizes`) start in milliseconds from a memory snapshot; any other
  size boots in about a second (`"start": "cold"`). Memory is the lease's
  charge for fair shares; vCPU is never charged. A lease keeps its size
  through pause, resume, clone, fork, restore and recovery, and a chosen
  size survives restarts too.
- **Resume on use**: a suspended lease, whatever suspended it
  (`idle_suspend`, take-back or its owner), resumes
  on its holder's next work call: exec, exec stream, files, guest dial,
  the proxy, jobs, the LLM gateway, a network change or a prompt. A
  resume that finds no room answers `503` `capacity_wait` with
  `Retry-After` and the lease stays suspended; no failed resume marks a
  lease lost. Automatic suspends name their reason on the lease and its
  `suspended` event.
- **Images**: one Dockerfile per capability in `images/`, built into E2B
  templates by `spoond images build <name>` (or `--all`). Every guest
  resolves names through the LAN resolver only and carries a container
  marker, so container tools such as kaniko work inside sandboxes.
- **SSH gateway**: `ssh new@sandbox.example` auto-creates a persistent
  sandbox; `ssh <lease-id>@sandbox.example` re-attaches; friendly names
  after `tag`. Sessions land in a tmux session.
- **Control plane over SSH**: `ssh ctl@sandbox.example "ls"` (pretty
  table by default; `--json` for raw). Verbs: `new`, `create`, `ls`,
  `stat`, `rm`, `keepalive`, `suspend`, `resume`, `restart`, `cp`
  (clone), `snapshot save|ls|show|rm`, `tag`, `comment`, `exec`,
  `share`, `ssh-key` (admin), `whoami`, `shelly`, `prompt`.
- **HTTP proxy**: `<lease-id>.sandbox.example` and `<id>-<port>`
  public URLs for sandbox web servers (Caddy fronts TLS).
- **LLM gateway**: per-lease OpenAI-compatible endpoint
  (`/llm/<lease-id>/openai/chat/completions`); upstream keys stay on
  the host, never inside sandboxes.
- **Agent access**: `spoond mcp`, an MCP stdio server with
  shell/read_file/write_file/edit_file/list_files/status tools, so any
  MCP-capable agent can work inside a lease.
- **Per-sandbox network policy**: `none` | `lan` | `internet` |
  `restricted` (with an egress allowlist), enforced by the orchestrator's
  egress firewall. `internet` and `lan` guests may call the lease API
  (CI jobs lease databases from inside their sandbox); `restricted` and
  `none` may not. Known limit: a domain in a `restricted` allowlist
  currently breaks HTTPS to allow-listed LAN IPs, so list IPs only.
- **Forgejo Actions runner**: `spoond runner` leases sandboxes as CI
  workers. On a full node a job waits for admission and retries instead
  of failing, and a job's `timeout-minutes` is enforced as in GitHub
  Actions (execution from sandbox creation), capped by
  `RUNNER_JOB_TIMEOUT` (default 6h, the whole job).
- **Dashboard**: `spoond dash` and `spoond top`, below.
- **Observability**: `/metrics` (Prometheus) covers the backend, the
  orchestrator (via an OpenTelemetry collector) and leases per state and
  per image. It needs an admin token or the scrape-only `METRICS_TOKEN`.
  Every lease create, release, loss and suspend also writes one line to
  the backend's journal (the lease journal), so `journalctl` alone can
  say what happened to a lease and why.
  `GET /readyz` answers uptime monitors such as Gatus (orchestrator,
  database, disk and hugepage checks; an example config is in
  [docs/operations.md](docs/operations.md#uptime-monitoring-gatus)).
- **Notifications**: with `NOTIFY_WEBHOOKS` set, the backend pushes what
  needs a person (a lost lease, a box_full refusal, a paused lease
  nearing its release, a pinned-idle notice, a unit down,
  disk or hugepages past their levels, a failed GC, a stale backup) to ntfy, Slack/Discord or any JSON
  receiver, with hourly dedupe, resolved messages, retries and a rate
  limit. `spoond notify test` checks the setup; see
  [docs/operations.md](docs/operations.md#notifications-to-webhooks).

## Error codes

Every error on the files, exec and jobs routes carries a stable,
machine-readable `code` beside the free-form `error` message — the same
shape on the lease create/exec/stream routes' refusals and on the exec
stream's WebSocket error frames too. Code is the
contract: programs branch on it. The message is diagnostic only: it can
change between releases. Codes follow the errno names where a POSIX
class fits (the e2b substrate's file layer is a filesystem) and
snake_case verbs where it does not. HTTP statuses are unchanged by
this and remain the first-class signal.

| Code | Status | Meaning | Caller effect |
|---|---|---|---|
| `ENOENT` | 404 | no such file or path (files; also `GET …/stat` of a file) | fix the path |
| `ENOTDIR` | 409 | a file sits in a path's way | fix the path |
| `EISDIR` | 409 | the target is a directory (read or write over one) | use the directory ops |
| `EEXIST` | 409 | the wrong type exists at the path (mkdir over a file) | remove or pick a new name |
| `ENOTEMPTY` | 409 | non-recursive remove of a non-empty directory | retry with `?recursive=1` |
| `EACCES` | 403 | the substrate refused a file operation on permissions | check the file's mode/owner in the guest (e2b's `GET` content read has no refusal reason and answers `EIO`) |
| `EFBIG` | 413 | file or request body over the size cap | send less |
| `EINVAL` | 400/404 | invalid path, `op`, `mode`, signal, stream or job-output parameter | fix the request |
| `EIO` | 400/502 | a request body's bytes could not be read (400), or a substrate read/write failure that is none of the above (502) | retry; investigate if persistent |

| Code | Status | Meaning | Caller effect |
|---|---|---|---|
| `bad_request` | 400 | request body failed validation (exec/jobs/secrets) | fix the request |
| `invalid_size` | 400 | create body's size fields are unusable | fix `memory_mb`/`disk_mb`/`vcpu` |
| `not_found` | 404 | the lease is not the caller's (or unknown) | check the lease id and token |
| `owner_deleted` | 403 | a resume-on-use refusal on the files, exec, stream and jobs routes: the owner's identity was removed while the request was in flight | stop; the owner is gone |
| `lease_suspended` | 409 | a non-work path hit a suspended lease | resume the lease first |
| `lease_busy` | 409 | a checkpoint or other lifecycle operation is in flight | retry after `Retry-After` |
| `lease_lost` | 410 | the sandbox is gone with nothing in flight | create a new lease (data is lost) |
| `lease_not_live` | 409 | the lease has no live sandbox for the operation | start or resume the lease |
| `cannot_start` | 409 | the lease failed to reach a running sandbox | inspect the lease's `failed_state` |
| `save_in_progress` | 409 | a named-snapshot save already holds the lease | retry after `Retry-After` |
| `not_secret_free` | 409 | a snapshot grant of a lease that had secrets | share only secret-free leases |
| `secrets_in_use` | 409 | a background job with staged secrets blocks the save | retry after the job exits |
| `snapshot_in_use` | 409 | a live lease is started from the named snapshot | stop (or delete) that lease first |
| `snapshot_limit` | 409 | the owner's `MAX_NAMED_SNAPSHOTS` cap is reached | delete a named snapshot first |
| `scrub_failed` | 500 | the save could not scrub `/run/secrets` | inspect the guest; retry the save |
| `kept_budget` | 409 | too many kept checkpoints for the lease | unpin or delete a kept one |
| `image_mismatch` | 409 | snapshot start with a mismatching `image` | omit `image` or use the snapshot's |
| `path_removed` | 409 | a create idempotency replay raced its DELETE | create a fresh lease id |
| `box_full` | 429 | the host's box is full and take-back freed nothing | wait; poll the lease list |
| `capacity_wait` | 503 | no room for the operation right now | retry after `Retry-After` |
| `substrate_unavailable` | 503 | the substrate could not confirm the sandbox state | retry after `Retry-After` |
| `too_busy` | 429 | the caller's per-owner exec/stream/file-transfer/dial cap is reached (identity overrides raised via the quota route) | retry once one of the caller's own calls ends (no `Retry-After`) |
| `too_many_jobs` | 429 | the lease is at its running background-job cap | wait for a job to exit |
| `spawn_error` | 500 | the stream's command or a background job could never start (agent unreachable) | retry; inspect the sandbox |
| `EIO` | 500 | a sync exec, or a stream after its start, failed in the transport (whether the command ran is unknown) | check for its effects before re-running |
| `job_not_found` | 404 | unknown job id, or the job belongs to another lease | check `job_id` |
| `job_not_running` | 409 | signal of a finished job, or its pid never appeared | read the record's `state` |
| `job_running` | 409 | delete of a job that is still running | signal it, wait for it to exit, then delete |
| `timeout` | 200 | a sync exec was ended by the substrate's own time limit (beside `exit: 124`, from the result's timeout flag, never from the output) | re-run with a longer `timeout` |
| `internal` | 500 | an unexpected backend or store failure | retry once; report if persistent |

The full per-route tables (including the statuses' history) are in
[docs/api.md](docs/api.md); `GET /api/capabilities` self-describes the
deployment.

## Dashboard

`spoond dash` is a read-only, live view of spoond's present operation,
for watching rather than triage. It draws one character grid at a fixed
width (DASH_WIDTH, default 104): framed panels for capacity (a
running/limit meter, leases by state, queued, granted, swept, running
leases per image), host meters (CPU, memory, hugepages, snapshot and
root disk, with the warn/danger levels), a full-width throughput panel
(running leases, requests per second, creates per minute, egress
connections — each with its current value and a sparkline over the
history), live leases (id, image, size as memory/vCPUs, owner, the
two-cell state glyphs (▶ ‖ ■ ⭘, with ◆ on a pinned lease; a suspended
lease's glyph is coloured by its suspend reason), policy, age, time
left, holder — on the page the holder is a link), the image catalog
(default size as mem/vcpu, sorted by use, eight rows then `+N more`
linking to a full `/images` page, and a total row) beside the systemd
units, a **records** panel of all-time stats from the lease history
(busiest moment, longest and shortest run, top owner, lifetime totals
and more; test owners left out via `DASH_RECORDS_EXCLUDE`), a refusals-and-failures row with the mean create and resume
times, and the newest lease events (from the lease event stream,
through a read-only `EVENTS_TOKEN`). The header draws `SPOOND ·
<host>` at the left margin and right-aligns spoond's uptime and the
frame's clock as `up <dur>, <time>`; the footer is one dim, centred
line — `Spoond v3.9.0 (2026-10-14) · GitHub` — with the
dashboard binary's version and its release date, and the GitHub mark
linking to the project URL (`DASH_PROJECT_URL`), dropping the date on a
narrow frame. `DASH_SERVICES` includes `spoond-netwatch` by default.
Its layout puts capacity and host side by side at 104 columns and
stacks them below that. The palette gives each state a colour role on
the black background — cyan for titles and lease ids, blue for run
state, green ok, amber warn, red bad, violet owner — never alone: every
coloured state keeps its glyph or word. Above the panels sits a
**Notifications** panel, drawn only when spoond has a system message to
show: a systemd unit not active, snapshot disk past the danger level,
or kept checkpoints past `KEPT_DISK_WARN_PCT` of the
snapshot disk. Each message carries a stable id from its trigger and a
severity, and each row has a `×` the viewer can click to dismiss it for
their own browser (`localStorage`, no server state); the dismissal stays
while the trigger stays active and returns if the trigger clears and
fires again. Per-lease trouble is not a dashboard message — the
dashboard viewer cannot act on a lease, so spoond tells the lease's
initiator itself (the lease event stream and the API's `409
lease_lost`/`lost_reason`). It refreshes every 2 seconds over
one server-sent-event stream shared by all viewers; the page is the
current grid in a `<pre>` with [WebTUI](https://webtui.ironclad.sh) for
the chrome, and Datastar patches the rows that changed.

`spoond top` draws the same grid with ANSI styles in the terminal, at
the terminal's width (COLUMNS, else 104), redrawn every 2 seconds until
interrupted — the same collector, the same notifications panel without
the dismiss control, no browser. Both it and `spoond dash` are excluded
from the binary by the `nodash` build tag.

On host it runs as the `spoond-dash` unit on **:8893** (HTTPS, basic
auth). Its data access is read-only: `/metrics` through the scrape-only
`METRICS_TOKEN` (which the lease API refuses), the lease event stream
through the events-only `EVENTS_TOKEN`, the SQLite catalog opened
read-only, user names from the identity store, `/proc` and systemd.

Setting it up is manual today; generating the credentials as part of
installation is planned:

```bash
spoond dash hash 'the-password'   # bcrypt hash for DASH_PASSWORD_HASH
# /etc/spoond/backend.env: METRICS_TOKEN=<random>, then restart the backend
# /etc/spoond/dash.env:    DASH_USER, DASH_PASSWORD_HASH, METRICS_TOKEN,
#                          DASH_TLS_CERT, DASH_TLS_KEY; every setting is
#                          listed in cmd/spoond-dash/dash.go
```

## Install

The E2B host is brought up by `deploy/e2b/host-setup.sh` (packages,
sysctls and hugepages, storage, pinned Firecracker/kernel/envd artifacts)
plus the remaining steps of
[U04](docs/plans/2026-09-30-e2b-substrate/U04-host-bringup.md): the
orchestrator unit, the host firewall (`e2b-guard`) and the OpenTelemetry
collector. Images are then built with `spoond images build --all`, and
`spoond doctor` verifies the result. A single installer for the E2B
stack does not exist yet.

## Docs

| Doc | Contents |
|---|---|
| [E2B substrate spec](docs/plans/2026-09-30-e2b-substrate/00-README.md) | the current platform: architecture, decisions, per-unit specs, ops appendices |
| [API reference](docs/api.md) | every endpoint: auth, request/response, errors; `GET /api/capabilities` self-describes the deployment |
| [Upgrading to 3.0](docs/upgrading-3.0.md) | what API clients change moving from 2.9 to 3.0 |
| [ctl reference](docs/ctl.md) | control-plane verbs, output contract, examples |
| [Usage guide](docs/usage.md) | SSH, exec, persistent/suspend, clones, proxy, LLM, agents, policies, multi-user |
| [Security](docs/security.md) | threat model, hardening notes, adversarial-review fixes |
| [Conformance suite](conformance/README.md) | the lease-API contract tests, run against production |
| [E2B upgrade runbook](docs/runbooks/e2b-upgrade.md) | moving the fork to a newer upstream |
| [Install](docs/install.md), [Setup](docs/setup.md) | bringing up an E2B host and spoond, first users and services |
| [Operations](docs/operations.md) | day-2: doctor, drain and reboots, the network watchdog, GC, backups, the dashboard |
| [Changelog](CHANGELOG.md) | what changed in each release |

## Status

**v3.10.1** (2026-10-18) is the current release. spoond 3.x shares a host
fairly: every active owner gets an equal, floating slice of memory and
snapshot disk, and when a request does not fit, spoond takes room back
from the biggest borrower. Since 3.9 it also pauses a lease on its own
before the host runs out of memory, and since 3.10 it resumes the
leases it paused once there is room again; 3.10.1 reacts to memory
pressure within a second and keeps those resumes from flapping. Clients
moving from 2.9 read [docs/upgrading-3.0.md](docs/upgrading-3.0.md).

Every release and what changed in it is in [CHANGELOG.md](CHANGELOG.md)
and on the [releases page](https://github.com/jrimmer/spoond/releases).

## Build

One binary, all services; exclude any module with build tags:

```bash
go build -o spoond ./cmd/spoond                                   # all modules
go build -tags 'nobackend,nomcp,norunner' -o spoond ./cmd/spoond  # subset
```

Exclusion tags: `nobackend`, `nogateway`, `nomcp`, `norunner`,
`noctl`, `noimages`, `nocapabilities`, `nodoctor`, `nodrain`, `nodash` (excludes `dash`
and `top` together), `nonotify`.

```bash
./spoond backend    # lease API, HTTP proxy, LLM gateway
./spoond gateway    # SSH gateway + ctl plane
./spoond mcp        # MCP endpoint
./spoond runner     # Forgejo Actions runner
./spoond ctl        # control-plane CLI
./spoond images     # build images into E2B templates; list the catalog
./spoond capabilities # what this spoond offers: images, sizes, policies, routes, surfaces
./spoond drain      # pause sandboxes before an orchestrator restart, resume after
./spoond doctor     # health checks (below)
./spoond dash       # read-only dashboard (browser)
./spoond top        # the dashboard grid in the terminal
./spoond notify test  # send a test message to every NOTIFY_WEBHOOKS receiver
```

`spoond doctor` checks the configuration, the orchestrator, the local
registry, the token seed, the SQLite database, the image catalog, the
pinned E2B artifacts (SHA-256, plus the Firecracker and kernel versions
builds still use), storage headroom, the backend, the SSH gateway port,
the LLM gateway, TLS, the drain unit, and the webhook receivers (their
reachability and any deliveries dropped in the last 24 h). It exits 1 if
any check fails.

## Configuration knobs

Built-in defaults are generic (`localhost`, `sandbox.example.com`);
a deployment sets its own hosts and addresses, such as
`SPOOND_GUEST_DNS_ADDR` (comma-separated for several resolvers),
`SPOOND_PROXY_HOST_SUFFIX` and `NODE_ID` (the
full list is in
[PRODUCTION-ENV-2.7.md](deploy/PRODUCTION-ENV-2.7.md)). Every knob is
overridable. The fixed addresses, ports and paths of the E2B
deployment are listed in
[01-architecture.md](docs/plans/2026-09-30-e2b-substrate/01-architecture.md).
Notable settings: `HOST_GUEST_SERVICE_ADDR` (where guests reach host
services), `HOST_API_PORT` (the lease API port `internet`/`lan` guests
may reach), `METRICS_TOKEN`, `LLM_UPSTREAM_URL`, `SPOOND_DB_PATH`,
`NOTIFY_WEBHOOKS`, the capacity settings (`DISK_RESERVE_PCT`, the
share of snapshot disk kept free, default 5; `MAX_ADMIT_WAIT_SECS`,
`IDLE_SUSPEND_DEFAULT_SECS`, `SNAPSHOT_WRITE_CONCURRENCY`,
`DRAIN_SNAPSHOT_CONCURRENCY`, `UNDRAIN_CONCURRENCY`,
`UNDRAIN_RESUME_RETRIES`, `UNDRAIN_READY_TIMEOUT`,
`UNDRAIN_RESUME_WINDOW`, `RECOVERY_RETRY_ATTEMPTS`,
`RECOVERY_RETRY_WINDOW`, `ROOTFS_PROBE_SECS`,
`SWEEP_TIMEOUT`), the
per-call E2B RPC bounds (`E2B_CREATE_TIMEOUT` and friends), named
snapshots (`MAX_NAMED_SNAPSHOTS`, `SNAPSHOT_KEEP_VERSIONS`), background jobs
(`MAX_RUNNING_JOBS_PER_LEASE`, `JOB_RETENTION_SECS`, `JOB_MAX_RUNTIME`),
`MAX_EXEC_BODY_BYTES`, the runner's `RUNNER_ADMIT_WAIT_SECS` and
`RUNNER_JOB_TIMEOUT`, `CRASH_TEST`, the
orphan-build reaper (`ORPHAN_REAP`, default `dryrun`), and
the saturation gate (`SATURATION_IO_FULL_PCT`, default 20;
`SATURATION_CPU_SOME_PCT`, default 75; new work waits while host
pressure is over them), the lease sizes (`SIZE_PRESETS`, default
`small=1:2048,medium=2:4096,large=4:8192,xlarge=8:16384`;
`MAX_LEASE_VCPU`, default the host's cores; `MAX_LEASE_MEMORY_MB`,
default 32768), the one paused-release clock (`PAUSED_RELEASE_DAYS`, default 30) and
the pinned-idle notice (`PINNED_IDLE_NOTICE_DAYS`, default 7) in
[docs/operations.md](docs/operations.md#pins-and-the-paused-release-clock).

## Tests

```bash
go test ./...            # unit tests (fast, no infra)
```

The conformance suite (`conformance/`, build tag `conformance`) is the
executable definition of the lease-API contract. It runs on the host
against a live spoond, as root; see [conformance/README.md](conformance/README.md).

## License

BSD-3-Clause from 2.4.0 (2.3.3 and earlier: Apache-2.0); see
[`LICENSE`](LICENSE) and [`NOTICE`](NOTICE), which lists the
third-party files that keep their own licenses.
Contributions welcome: see [`CONTRIBUTING.md`](CONTRIBUTING.md).
