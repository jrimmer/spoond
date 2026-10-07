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

- **Lease API**: `POST /api/leases` (image, TTL, persistent, network
  policy, exposed ports, holder, secrets; `/api/sandboxes` is kept as an
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
    `keep: true` to pin it (at most `MAX_KEPT_PER_LEASE`, default 4, and
    an optional per-owner byte budget), and `POST /restore` puts the lease
    back to a kept checkpoint in place. `restart?mode=cold` gives the lease a fresh
    guest from its image, keeping its id.
  - **Holders**: `holder` and `holder_url` say what holds a lease (a CI
    job, an orchestrator's run, someone's scratch work). A held lease
    outlives its TTL until its hold lapses, and automatic limits (idle
    suspend, release after a week suspended, pressure and critical-disk
    rules) keep held leases bounded; nothing running is ever released
    automatically.
- **Multi-user tenancy**: people and agents are first-class identities,
  with per-user SSH keys, per-user tokens, quotas
  (`max_leases`/`max_ttl`, memory: `guaranteed_mib`/`max_mib`), admin roles, lease sharing with expiry,
  per-user LLM gateway keys, and per-user proxy hostnames
  (`<label>.<user>.sandbox.example`). See
  [docs/security.md](docs/security.md) and the [Users & identity API
  section](docs/api.md#users--identity).
- **Capacity classes**: a lease is **guaranteed** while its owner's
  running memory stays within their `guaranteed_mib`, and **burst** past
  it or when asked (`"burst": true`, with a `priority`). Burst leases
  are admitted only while `BURST_RESERVE_MIB` of hugepages stays free,
  and when guaranteed work needs room spoond suspends them (lowest
  priority, then newest) and resumes them by itself once they fit again
  (`preempted`/`resumed` events; the memory continues). A user without
  a `guaranteed_mib` keeps every lease guaranteed. A create may
  **wait** for room or for one of its owner's own leases to go
  (`"wait": <seconds>`, up to `MAX_ADMIT_WAIT_SECS`) instead of failing
  on capacity, the memory cap or the lease-count cap, served in
  fair-share order; a persistent lease
  may set `idle_suspend` to give its memory back when idle, and the
  next exec, files call or dial resumes it.
- **Images**: one Dockerfile per capability in `images/`, built into E2B
  templates by `spoond images build <name>` (or `--all`). Every guest
  resolves names through the LAN resolver only and carries a container
  marker, so container tools such as kaniko work inside sandboxes.
- **SSH gateway**: `ssh new@sandbox.example` auto-creates a persistent
  sandbox; `ssh <lease-id>@sandbox.example` re-attaches; friendly names
  after `tag`. Sessions land in a tmux session.
- **Control plane over SSH**: `ssh ctl@sandbox.example "ls"` (pretty
  table by default; `--json` for raw). Verbs: `new`, `ls`, `stat`,
  `rm`, `keepalive`, `suspend`, `resume`, `restart`, `cp` (clone),
  `tag`, `comment`, `exec`, `share`, `ssh-key` (admin), `whoami`,
  `shelly`, `prompt`.
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
  workers.
- **Dashboard**: `spoond dash` and `spoond top`, below.
- **Observability**: `/metrics` (Prometheus) covers the backend, the
  orchestrator (via an OpenTelemetry collector) and leases per state and
  per image. It needs an admin token or the scrape-only `METRICS_TOKEN`.
  `GET /readyz` answers uptime monitors such as Gatus (orchestrator,
  database, disk and hugepage checks; an example config is in
  [docs/operations.md](docs/operations.md#uptime-monitoring-gatus)).
- **Notifications**: with `NOTIFY_WEBHOOKS` set, the backend pushes what
  needs a person (a lost lease, a held-lease rule acting, a unit down,
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
history), live leases (id, image, owner, run state, policy, age, time
left, holder — on the page the holder is a link; a hold marks the
holder ◆, or ◉ once lapsed), the image catalog beside the systemd
units, a refusals-and-failures row with the mean create and resume
times, and the newest lease events (from the lease event stream,
through a read-only `EVENTS_TOKEN`), with a status line at the bottom.
Its layout puts capacity and host side by side at 104 columns and
stacks them below that. The palette gives each state a colour role on
the black background — cyan for titles and lease ids, blue for run
state, green ok, amber warn, red bad, violet owner, and amber banners —
never alone: every coloured state keeps its glyph or word. Above the
panels sits one
attention banner, shown only when something needs a person: a unit not
active, a lost lease, free hugepages or snapshot disk past the danger
level, preempted burst leases, or a held lease that a held-lease rule suspended and that is
still suspended. It refreshes every 2 seconds over
one server-sent-event stream shared by all viewers; the page is the
current grid in a `<pre>` with [WebTUI](https://webtui.ironclad.sh) for
the chrome, and Datastar patches the rows that changed.

`spoond top` draws the same grid with ANSI styles in the terminal, at
the terminal's width (COLUMNS, else 104), redrawn every 2 seconds until
interrupted — the same collector, the same banner, no browser. Both it
and `spoond dash` are excluded from the binary by the `nodash` build
tag.

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
| [API reference](docs/api.md) | every endpoint: auth, request/response, errors |
| [ctl reference](docs/ctl.md) | control-plane verbs, output contract, examples |
| [Usage guide](docs/usage.md) | SSH, exec, persistent/suspend, clones, proxy, LLM, agents, policies, multi-user |
| [Security](docs/security.md) | threat model, hardening notes, adversarial-review fixes |
| [Conformance suite](conformance/README.md) | the lease-API contract tests, run against production |
| [E2B upgrade runbook](docs/runbooks/e2b-upgrade.md) | moving the fork to a newer upstream |
| [Install](docs/install.md), [Setup](docs/setup.md) | bringing up an E2B host and spoond, first users and services |
| [Operations](docs/operations.md) | day-2: doctor, drain and reboots, the network watchdog, GC, backups, the dashboard |
| [Changelog](CHANGELOG.md) | what changed in each release |

## Status

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
hugepage reserve, and preemption of burst leases by suspend with an
automatic resume queue, so CI and other guaranteed work get room on a
busy host without losing anyone's state. spoond is BSD-3-Clause
licensed from this release.

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
package. Leases can name their holder, and held leases are bounded by
automatic limits.

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
`noctl`, `noimages`, `nodoctor`, `nodrain`, `nodash` (excludes `dash`
and `top` together), `nonotify`.

```bash
./spoond backend    # lease API, HTTP proxy, LLM gateway
./spoond gateway    # SSH gateway + ctl plane
./spoond mcp        # MCP endpoint
./spoond runner     # Forgejo Actions runner
./spoond ctl        # control-plane CLI
./spoond images     # build images into E2B templates; list the catalog
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

The repo targets a homelab by default (addresses like `10.0.0.11`,
hostnames like `sandbox.example.com` appear as *defaults only*); every
knob is overridable. The fixed addresses, ports and paths of the E2B
deployment are listed in
[01-architecture.md](docs/plans/2026-09-30-e2b-substrate/01-architecture.md).
Notable settings: `HOST_GUEST_SERVICE_ADDR` (where guests reach host
services), `HOST_API_PORT` (the lease API port `internet`/`lan` guests
may reach), `METRICS_TOKEN`, `LLM_UPSTREAM_URL`, `SPOOND_DB_PATH`,
`NOTIFY_WEBHOOKS`, the capacity settings (`BURST_RESERVE_MIB`,
`PREEMPT_DISK_FLOOR_PCT`, `MAX_ADMIT_WAIT_SECS`,
`IDLE_SUSPEND_DEFAULT_SECS`), background jobs
(`MAX_RUNNING_JOBS_PER_LEASE`, `JOB_RETENTION_SECS`), `CRASH_TEST`, the
orphan-build reaper (`ORPHAN_REAP`, default `dryrun`), and
the held-lease
limits (`HOLD_TTL_SECS` and the rest, in
[docs/operations.md](docs/operations.md)).

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
