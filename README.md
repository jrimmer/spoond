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
  equal fair share of memory and snapshot disk per owner (no per-owner
  caps since 3.0), admin roles, lease sharing with expiry,
  per-user LLM gateway keys, and per-user proxy hostnames
  (`<label>.<user>.sandbox.example`). See
  [docs/security.md](docs/security.md) and the [Users & identity API
  section](docs/api.md#users--identity).
- **Fair shares**: every owner gets an equal, floating 1/N slice of
  memory and snapshot disk; an owner may borrow past it while the box
  has room. When a request does not fit, spoond takes room back from
  the biggest borrower (highest usage over slice): memory take-back
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
line — `Spoond v3.8.0 (2026-10-14) · GitHub` — with the
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

**v3.8: lease history and records.** Every ended lease leaves a row in
a new `lease_history` table (with its size, outcome, peak memory,
start latency and longest nap); `spoond history import` backfills it
from the backend journal. The dashboard gains a records panel over it,
and the images panel sorts by use, caps at eight rows with a `+N more`
link to a full `/images` page, and shows each image's size as
`mem/vcpu`. Low free hugepages is no longer a dashboard notification.
Schema 29. No breaking changes.

**v3.7.2: image builds clean up after themselves.** The local tagged
image is removed after each successful push. No breaking changes.

**v3.7.1: root disk in view.** `/readyz` also watches the root
filesystem; image builds prune both docker builders' cache with a size
cap; lease comments refuse control characters. No breaking changes.

**v3.7: self-description and better signals.** `GET /api/capabilities`
and `spoond capabilities` list everything this spoond offers. Released
leases report their measured peak memory; creates may carry a `comment`;
the dashboard shows CI job starts and idle sweeps. The `/api/sandboxes`
alias and keyless LLM access carry `Deprecation` headers ahead of 4.0.
No breaking changes.

**v3.6.1: file writes on Arch images.** `PUT /api/leases/{id}/files/…`
into a sticky directory such as `/tmp` works on images with
`fs.protected_regular` on: the body lands in a private upload directory
and is renamed into place. Adds the `rust-qt-base` image (Rust + Qt 6 on
Arch) and guest-networking conformance (N10, N12). No breaking changes.

**v3.6: secrets from files.** Every secret setting may be given as
`<NAME>_FILE=/path` (a 0600 file holding the value) instead of a value
in the env file; spoond refuses to start on a missing, unreadable or
empty file, or with both spellings set. `spoond config show` shows the
path, never the content, and `spoond doctor` checks the file. A lease's
`size_hint` keeps its vCPUs and adds `memory_hint_mb`. No breaking
changes.

**v3.5: measured memory.** Every running lease reports its current and
peak memory use (`memory_used_mb`, `memory_peak_mb`) and a `size_hint`
when a smaller preset would hold its peak with headroom; `GET
/api/usage` adds the owner's measured memory. The backend checks every
setting's type at startup and refuses a bad value, naming it. No
breaking changes.

**v3.4: config show.** `spoond config show` lists every setting of a
unit with its value (secrets masked), the file it came from and
whether the running backend matches, so an env change waiting for a
restart shows at a glance (`GET /api/admin/config` is the backend's own
view). `spoond doctor` still flags unknown and repeated keys. No
breaking changes.

**v3.3: one authorization model.** Users and admin users, two
read-only service tokens (`METRICS_TOKEN`, `EVENTS_TOKEN`) and the
host's break-glass `ADMIN_TOKEN` — see the who-can-do-what table in
[docs/security.md](docs/security.md#who-can-do-what). An admin user's
own token reaches `/api/admin/*`; a user keeps several named tokens,
each revocable and optionally expiring (`/api/users/{id}/tokens`); root
on the host mints a short-lived token for any user with `ADMIN_TOKEN`.
The dashboard shows a lease's state and pin as two glyphs. No breaking
changes.

**v3.2: saturation and sharing.** New creates, clones and forks wait
while the host's I/O or CPU pressure is high (`SATURATION_IO_FULL_PCT`,
`SATURATION_CPU_SOME_PCT`); running leases are never held. An owner can
grant a named snapshot to another user, but only a version saved from
a lease that never had a secret; a lease started from a snapshot can
restore back to it. A create that will expire carries a `Warning`
header, `spoond doctor` checks the env files, and the dashboard shows
each lease's size. No breaking changes.

**v3.1: sizes and usage.** A lease's size is chosen per create (a
preset, or vCPU and memory), no longer fixed by its image: warm sizes
start from a memory snapshot and any other size cold-boots. Every lease
reports its size and how it started, and clone, fork, restore and
recovery now charge the size the VM actually runs. Owners see their
lease counts, waiting creates, recent refusals and a 7-day usage
history in `GET /api/usage`. No breaking changes.

**v3.0: fair shares.** Guaranteed and burst classes, preemption, holds
and per-owner caps are gone. Every owner gets an equal slice of memory
and snapshot disk, and spoond takes room back from the biggest borrower
by pausing its least recently used unpinned lease (memory) or releasing
its oldest unpinned paused lease (disk). Pins protect a lease, one clock
releases every paused lease 30 days after its pause date, and a
taken-back lease resumes on its next work call. Every capacity refusal
is `503 capacity_wait` or `429 box_full`; `429 quota_exceeded` is gone.
The Go module path is `github.com/jrimmer/spoond/v3`. Clients moving
from 2.9 read [docs/upgrading-3.0.md](docs/upgrading-3.0.md).

**v2.9: resume on use.** A suspended lease comes back on its holder's
next work call, whatever suspended it, and every refusal has one shape
(`503 capacity_wait`, `429 quota_exceeded`, `409 lease_busy`). An
orchestrator stall answers a retryable `503 substrate_unavailable`
instead of reporting a sandbox gone. CI jobs wait for room on a full
node and honour `timeout-minutes`. Deleting a user releases their
leases and drops their snapshots and kept builds; background jobs have
a runtime cap; exec bodies are bounded; and the lease journal records
every lease's life in the backend's journal. 2.9.1 makes a planned
orchestrator restart wait for the orchestrator before resuming leases,
so a restart no longer loses them. 2.9.2 scales the dashboard's i/o
stall meter so its warning tick lines up with the other host meters.
2.9.3 adds `DASH_AUTH`, which can turn the dashboard's login off for a
dashboard on a private network (it stays on by default).

**v2.8: no loose ends.** A lost lease's guest is always stopped, and
spoond sweeps away sandboxes nothing claims. A late checkpoint, pause or
restore can no longer bring a released lease back, and drain and
undrain recover on their own. Guests resolve through both LAN resolvers,
lease web hostnames reach the guest's own `/assets/` (#144), and a
suspended lease's `409` says `lease_suspended`.

**v2.7.1: a utility that tidies up after itself.** A lost lease says
why (`410` with `code: lease_lost`) and is released after its grace period, every
orchestrator call has a deadline, and the dashboard shows spoond system
messages in a dismissable Notifications panel, i/o stall and disk-busy
meters, and the version and a GitHub link in its footer.

**v2.7: named snapshots.** A lease can be saved as a named, versioned
snapshot, and a new lease can start from it (`"snapshot": "name@v"`,
or `spoondctl create --snapshot`); `spoondctl snapshot save|ls|show|rm`
manages them. A restricted allowlist that lists domains keeps its LAN
IPs reachable. Snapshot writes are paced, a lease whose root disk dies
is recovered, and an undrain retries a resume that fails for a moment.
Built-in defaults are generic, so a deployment sets its own hosts and
addresses (see [PRODUCTION-ENV-2.7.md](deploy/PRODUCTION-ENV-2.7.md)).

**v2.6.7: certificates by name, renewed in place.** The lease API and the
dashboard serve several TLS certificates, chosen by the name the client
asks for, and re-read them when they change, so renewals need no
restart. Exec env no longer shows in the guest's command line.

**v2.6: background jobs and crash testing.** A long command runs as a
tracked background job in its lease, with its exit, output and a
`job_exited` event spoond keeps even across backend restarts. A host
running crash suites can let lease owners send a lease through crash
recovery on demand (`CRASH_TEST=1`). The dashboard takes the mockup's
colour roles on its black background.

**v2.5: waiting and idle reclamation.** A create can wait for room
(`"wait"`, fair-share order, with its queue position on the event
stream and at `GET /api/leases/queue`) instead of failing on a full
host, and a persistent lease can set its own `idle_suspend` so idle
memory goes back to the host and the next call resumes it.

**v2.4: capacity on shared hosts.** Memory quotas per user
(`guaranteed_mib`, `max_mib`), guaranteed and burst leases with a
hugepage reserve, and preemption of burst leases by suspend, so CI and
other guaranteed work get room on a busy host without losing anyone's
state. A preempted lease comes back on its holder's next work call
(#145 D2); spoond is BSD-3-Clause licensed from this release.

**v2.3: checkpoints on the lease's terms.** Periodic checkpoints are off
by default and set per lease (`checkpoint_interval`); a checkpoint can be
kept and a lease restored to it in place; `restart?mode=cold` gives a
lease a fresh guest without changing its id. Guest images no longer
leave root-run binaries writable by other users. The dashboard is laid
out to its mockup, with a lease events panel.

**v2.2: driving work inside a lease.** Lease files, guest port dial,
secrets as files, the lease event stream and generations on the lease
API; `/readyz` for uptime monitors and webhook notifications for
operators.

**v2.1: a plain microVM utility.** Agent workflow (the hive, the worker
layer, `spoond acp`, the cfos adapter) moved to the separate Honey
project. The dashboard is drawn on a character grid, the same in the
browser and in the terminal (`spoond top`), on the public `grid`
package. Leases can name their holder.

**v2.0: E2B substrate.** spoond runs on a patch-queue fork of E2B's
orchestrator instead of forkd: warm memory-snapshot starts,
native fork, pause/resume and checkpoint, SQLite state, a template-based
image pipeline, a drain protocol for orchestrator restarts, the read-only
dashboard and a scrape-only metrics token. The lease API contract is
unchanged (additions only).

**v1.2: infrastructure & observability.** Prometheus metrics for the
backend, gateway and runner; MCP HTTP/SSE transport; SSH gateway image
resolution; LLM-based PR review; runner reliability and exec fixes.

**v1.1: multi-user tenancy.** People and agents are first-class
identities (epic #26), with security hardening across the board; see
[docs/security.md](docs/security.md).

Prior: **v1.0: single-operator**.

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
