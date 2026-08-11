# spoond

Fast, isolated, ephemeral compute for people and agents: a lease API in
front of **forkd microVMs** on a warm pool. Consumers request a sandbox,
run work in it, and release it — each job gets its own isolated KVM
environment in milliseconds, with an SSH gateway, HTTP proxy, LLM
gateway, and native MCP/ACP agent endpoints on top.

```
spoond (this repo)                 forkd controller (separate repo)
┌──────────────────────────┐      ┌───────────────────────────────┐
│ lease API  :8890         │ ───▶ │ Firecracker microVM lifecycle  │
│ SSH gateway :2222        │      │ netns slots, snapshots,        │
│ HTTP proxy  :8891        │      │ workspaces (suspend/resume)    │
│ ctl plane (SSH-as-API)   │      │ (deeplethe/forkd, Apache-2.0)  │
│ MCP / ACP agent servers  │      └───────────────────────────────┘
│ LLM gateway  /llm/<id>   │
└──────────────────────────┘
```

## What it gives you

- **Lease API** — `POST /api/sandboxes` (image, TTL, persistent,
  network policy) → exec, stream (WebSocket PTY), keepalive, suspend/
  resume, clone, tag, comment, delete. Auth via bearer tokens
  (`CONSUMER_TOKENS=token=owner,...`) or per-user identity tokens.
- **Multi-user tenancy (v1.1)** — people AND agents are first-class
  identities: per-user SSH keys, per-user tokens, quotas
  (`max_leases`/`max_ttl`), admin roles, lease sharing with expiry,
  per-user LLM gateway keys, and per-user proxy hostnames
  (`<label>.<user>.sandbox.example`). See
  [docs/security.md](docs/security.md) and the [Users & identity API
  section](docs/api.md#users--identity).
- **SSH gateway** — `ssh new@sandbox.example` auto-creates a persistent
  sandbox; `ssh <lease-id>@sandbox.example` re-attaches; friendly names
  after `tag`. Sessions land in a tmux session.
- **Control plane over SSH** — `ssh ctl@sandbox.example "ls"` (pretty
  table by default; `--json` for raw). Verbs: `new`, `ls`, `stat`,
  `rm`, `keepalive`, `suspend`, `resume`, `restart`, `cp` (clone),
  `tag`, `comment`, `exec`, `share`, `ssh-key` (admin), `whoami`,
  `shelly`, `prompt`.
- **HTTP proxy** — `<lease-id>.sandbox.example` and `<id>-<port>`
  public URLs for sandbox web servers (Caddy fronts TLS).
- **LLM gateway** — per-lease OpenAI-compatible endpoint
  (`/llm/<lease-id>/openai/chat/completions`); upstream keys stay on
  the host, never inside sandboxes.
- **Native agent endpoints** — `spoond mcp` (MCP stdio server:
  shell/read_file/write_file/edit_file/list_files/status tools) and
  `spoond acp` (Agent Client Protocol: session = lease, agent loop
  through the LLM gateway). Point Goose/Claude/Codex-style clients at
  them to get sandboxed tool execution.
- **Per-sandbox network policy** — `none` | `lan` | `internet` |
  `restricted` (with egress allowlist), enforced with iptables in the
  child netns.
- **Forgejo Actions runner** — `spoond runner` adaptively leases
  sandboxes as CI workers.

## Install

Two paths — see [docs/install.md](docs/install.md):

```bash
git clone https://github.com/jrimmer/spoond && cd spoond
./deploy/install-spoond.sh --with-forkd   # forkd + spoond (full stack)
./deploy/install-spoond.sh                # spoond only (existing forkd-controller)
/opt/spoond/spoond doctor                 # verify all dependencies
```

## Docs

| Doc | Contents |
|---|---|
| [Install](docs/install.md) | forkd + spoond (full stack) or spoond-only; install script + verification |
| [Setup](docs/setup.md) | prerequisites, build, env/flag reference, systemd, TLS |
| [API reference](docs/api.md) | every endpoint: auth, request/response, errors |
| [ctl reference](docs/ctl.md) | control-plane verbs, output contract, examples |
| [Usage guide](docs/usage.md) | SSH, exec, persistent/suspend, clones, proxy, LLM, agents, policies, multi-user | 
| [Operations](docs/operations.md) | pool, watchdog, failure runbook, backups, identity ops |
| [Security](docs/security.md) | threat model, hardening notes, adversarial-review fixes |
| [deploy/](deploy/README.md) | systemd units for vm2-style deployments |

## Status

**v1.1 — multi-user tenancy.** People and agents are first-class
identities (epic #26): a user store with per-user SSH keys and bearer
tokens, ownership-scoped leases, quotas, admin roles, lease sharing with
expiry, per-user LLM gateway keys, per-user proxy hostnames, and a
forward-auth proxy mode for integrating an IdP (Caddy/Authelia-style).
Security hardening across the board — see
[docs/security.md](docs/security.md) for the adversarial-review findings
and fixes that shipped with 1.1 (admin-gated directory, closed LLM
gateway, atomic quotas, salted hashes, constant-time compares, guest
network isolation, rate limiting, and more).

Prior: **v1.0 — single-operator** (one operator, one SSH key allowlist,
one set of consumer tokens).

## Build

One binary, all services; exclude any module with build tags:

```bash
go build -o spoond ./cmd/spoond                       # all modules
go build -tags 'nobackend,nomcp,norunner' -o spoond ./cmd/spoond  # subset
```

Supported exclusion tags: `nobackend`, `nogateway`, `noacp`, `nomcp`,
`norunner`, `noctl`, `nodoctor`.

```bash
./spoond backend    # lease API
./spoond gateway    # SSH gateway + ctl plane
./spoond acp        # ACP endpoint
./spoond mcp        # MCP endpoint
./spoond runner     # Forgejo Actions runner
./spoond ctl        # control-plane CLI
./spoond doctor     # dependency/connectivity checks (forkd, LLM, listeners, pool)
```

## Run

See [docs/setup.md](docs/setup.md) for the full guide; the short form:

```bash
export CONSUMER_TOKENS='abc=forgejo'
export POOL_SIZE=3
./spoond backend

./spoond gateway --backend https://127.0.0.1:8890 \
  --backend-token abc --client-keys /etc/spoond-gateway/keys
```

## Configuration knobs

The repo targets a homelab by default (addresses like `10.43.0.1`,
hostnames like `sandbox.lacy.casa` appear as *defaults only*); every
knob is overridable so the software runs anywhere the forkd controller
does. Notable overrides: `FORKD_GATEWAY_HOST`, `SHELLY_BINARY_URL`,
`LLM_GATEWAY_URL`, `BE_API` (integration tests).

## Tests

```bash
go test ./...            # unit tests (fast, no infra)
```

Integration (`tests/integration/`) exercises the full stack — lease
API, SSH gateway, ctl plane, MCP/ACP, HTTP proxy, network policy —
against a **live forkd homelab** (warm pool + controller + images). It
cannot run on CI without that infrastructure; the GitHub Actions
workflow gates it behind a manual trigger with a note.

```bash
SSHHOST=root@<vm> BE_API=https://127.0.0.1:8890 bash tests/integration/run.sh
```

## License

Apache-2.0 — see [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE).
Contributions welcome: see [`CONTRIBUTING.md`](CONTRIBUTING.md).
