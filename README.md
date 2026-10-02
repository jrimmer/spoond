# spoond

Fast, isolated, ephemeral compute for people and agents: a lease API in
front of **Firecracker microVMs run by E2B's orchestrator**. Consumers
request a sandbox, run work in it, and release it. Every sandbox starts
as a memory-snapshot restore, so starts are warm, and fork, pause/resume
and checkpoint are native. On top: an SSH gateway, an HTTP proxy, an LLM
gateway, native MCP/ACP agent endpoints, a Forgejo Actions runner and a
read-only dashboard.

```
spoond (this repo, one Go binary)          e2b-orchestrator (our fork of
┌──────────────────────────────────┐       e2b-dev/runtime, patch queue)
│ lease API            :8890       │ gRPC  ┌──────────────────────────────┐
│ HTTP proxy, LLM gateway :8891    │ ────▶ │ Firecracker microVM lifecycle│
│ SSH gateway + ctl    :2222       │ :5008 │ memory snapshots (UFFD), NBD │
│ dashboard (read-only) :8893      │       │ rootfs, netns per sandbox,   │
│ MCP / ACP agent servers          │ envd  │ egress firewall, templates   │
│ Forgejo Actions runner           │ ────▶ │ envd in every guest (:49983) │
│ SQLite state (leases, catalog)   │ :5007 └──────────────────────────────┘
└──────────────────────────────────┘
images/*.dockerfile → docker → local registry → E2B templates (spoond images)
```

Production moved from forkd to E2B on 2026-10-01. The design, decisions
and per-unit specs are in
[docs/plans/2026-09-30-e2b-substrate/](docs/plans/2026-09-30-e2b-substrate/00-README.md);
the forkd rollback path stays until U12 step 20 (2026-10-31).

## What it gives you

- **Lease API**: `POST /api/sandboxes` (image, TTL, persistent,
  network policy, exposed ports), then exec, stream (WebSocket PTY),
  keepalive, suspend/resume, checkpoint, fork, clone, tag, comment,
  delete. Auth via bearer tokens (`CONSUMER_TOKENS=token=owner,...`) or
  per-user identity tokens. Leases and the image catalog persist in
  SQLite, so a backend restart loses nothing.
- **Multi-user tenancy**: people and agents are first-class identities,
  with per-user SSH keys, per-user tokens, quotas
  (`max_leases`/`max_ttl`), admin roles, lease sharing with expiry,
  per-user LLM gateway keys, and per-user proxy hostnames
  (`<label>.<user>.sandbox.example`). See
  [docs/security.md](docs/security.md) and the [Users & identity API
  section](docs/api.md#users--identity).
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
- **Native agent endpoints**: `spoond mcp` (MCP stdio server:
  shell/read_file/write_file/edit_file/list_files/status tools) and
  `spoond acp` (Agent Client Protocol: session = lease, agent loop
  through the LLM gateway).
- **Per-sandbox network policy**: `none` | `lan` | `internet` |
  `restricted` (with an egress allowlist), enforced by the orchestrator's
  egress firewall. `internet` and `lan` guests may call the lease API
  (CI jobs lease databases from inside their sandbox); `restricted` and
  `none` may not. Known limit: a domain in a `restricted` allowlist
  currently breaks HTTPS to allow-listed LAN IPs, so list IPs only.
- **Forgejo Actions runner**: `spoond runner` leases sandboxes as CI
  workers.
- **Dashboard**: `spoond dash`, below.
- **Observability**: `/metrics` (Prometheus) covers the backend, the
  orchestrator (via an OpenTelemetry collector) and leases per state and
  per image. It needs an admin token or the scrape-only `METRICS_TOKEN`.

## Dashboard

`spoond dash` is a read-only, live view of spoond's present operation,
for watching rather than triage: lease and sandbox counts, host CPU,
memory, hugepages and snapshot disk, five-minute sparklines (sandboxes,
API requests, lease grant time, egress connections), the systemd units,
live leases and the image catalog. It refreshes every 2 seconds over one
server-sent-event stream shared by all viewers, and switches between
pixel-art themes (Deep Space, Terminal, Nebula, Daylight; built with
[Starbase](https://starbase.zweiundeins.gmbh) components, vendored).

On vm2 it runs as the `spoond-dash` unit on **:8893** (HTTPS, basic
auth). Its data access is read-only: `/metrics` through the scrape-only
`METRICS_TOKEN` (which the lease API refuses), the SQLite catalog opened
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
| [Install](docs/install.md), [Setup](docs/setup.md), [Operations](docs/operations.md) | **forkd-era**; being rewritten for E2B with the forkd removal |

## Status

**v2.0: E2B substrate (2026-10-01).** Production runs on a patch-queue
fork of E2B's orchestrator instead of forkd: warm memory-snapshot starts,
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

Exclusion tags: `nobackend`, `nogateway`, `noacp`, `nomcp`, `norunner`,
`noctl`, `noimages`, `nodoctor`, `nodrain`, `nodash`, `nohive`.

```bash
./spoond backend    # lease API, HTTP proxy, LLM gateway
./spoond gateway    # SSH gateway + ctl plane
./spoond acp        # ACP endpoint
./spoond mcp        # MCP endpoint
./spoond runner     # Forgejo Actions runner
./spoond ctl        # control-plane CLI
./spoond images     # build images into E2B templates; list the catalog
./spoond drain      # pause sandboxes before an orchestrator restart, resume after
./spoond doctor     # health checks (below)
./spoond dash       # read-only dashboard
./spoond hive       # enlistment checks for a project's hive.yaml
```

`spoond doctor` checks the configuration, the orchestrator, the local
registry, the token seed, the SQLite database, the image catalog, the
pinned E2B artifacts (SHA-256, plus the Firecracker and kernel versions
builds still use), storage headroom, the backend, the SSH gateway port,
the LLM gateway and TLS. It exits 1 if any check fails.

## The hive

A project that wants bees (agent workers in leases) describes itself in
`.spoond/hive.yaml` ([the hive plan](docs/plans/2026-10-02-swarm-controller.md),
C10) — the
project name, its repo, a base image from the catalog, the gates that
define "done", extra network needs (`leases`, `registry`), a worker cap
and the implement/verify models — and everything else (the worker image,
the network allowlist, the credentials) is derived from it.

```bash
spoond hive check .spoond/hive.yaml
```

runs the enlistment checks (C11) and ends with one `Next:` line: the
first failing check's remedy, or the enlistment route when nothing
failed. The exit code is 0 when nothing failed. It reads `SPOOND_API`
(default `https://vm2.lacy.casa:8890`) and `SPOOND_TOKEN`, and it looks
up the image catalog for real; the steps that need the host (building
the worker image, cloning with the deploy key, the trial lease, the
gates, the budget) report `skipped` there; `POST /hive/check` on the
instance will run them for real (build order step 3).

## Configuration knobs

The repo targets a homelab by default (addresses like `10.1.0.11`,
hostnames like `sandbox.lacy.casa` appear as *defaults only*); every
knob is overridable. The fixed addresses, ports and paths of the E2B
deployment are listed in
[01-architecture.md](docs/plans/2026-09-30-e2b-substrate/01-architecture.md).
Notable settings: `HOST_GUEST_SERVICE_ADDR` (where guests reach host
services), `HOST_API_PORT` (the lease API port `internet`/`lan` guests
may reach), `METRICS_TOKEN`, `LLM_UPSTREAM_URL`, `SPOOND_DB_PATH`.

## Tests

```bash
go test ./...            # unit tests (fast, no infra)
```

The conformance suite (`conformance/`, build tag `conformance`) is the
executable definition of the lease-API contract. It runs on the host
against a live spoond, as root; see [conformance/README.md](conformance/README.md).

## License

Apache-2.0; see [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE).
Contributions welcome: see [`CONTRIBUTING.md`](CONTRIBUTING.md).
