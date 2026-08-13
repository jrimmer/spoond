# Using spoond from an AI coding agent

spoond is a self-hosted microVM sandbox platform (Firecracker via forkd).
It is designed to be driven by agents as much as by people. This document
is the full surface map and "how do I X?" reference for agent authors.

[AGENTS.md](../AGENTS.md) is the concise auto-loaded version for agents
working *inside this repository*; this file is the detailed reference for
any agent (or person) that wants to *use* a spoond deployment.

## The surface at a glance

| Surface | Reach it via | What it gives you |
|---|---|---|
| **Lease API** | HTTPS `:8890`, bearer token | create/list/exec/suspend/resume/restart/clone/share/tag/comment/prompt/stat/stream sandboxes; images |
| **SSH gateway + control plane** | `:2222` | `ssh ctl@<host> "verb"` for the control plane; `ssh <id\|name>@<host>` to attach to a sandbox |
| **HTTP proxy** | `:8891` | wildcard hostname → sandbox HTTP (per-user proxy hostnames) |
| **LLM gateway** | `/llm/<lease-id>/<provider>/…` on `:8890`/`:8891` | per-lease LLM access; the model key never enters the VM |
| **MCP server** | stdio, or HTTP `:9090` | 6 tools: `shell`, `read_file`, `write_file`, `edit_file`, `list_files`, `status` |
| **ACP server** | stdio (JSON-RPC 2.0) | session-scoped agent protocol: `session/new`, `session/prompt`, `session/cancel` |
| **CI runner** | Forgejo Actions | jobs land on forkd sandboxes by `runs-on` label |
| **forkd CLI** | host shell | low-level microVM tooling (`from-image`, `snapshot`, `exec --child`, `ping --child`) |
| **Guest agent** | in-VM `:8888` | `forkd-agent.py` (PID 1): JSON-over-TCP `ping`/`exec`/`eval`/`stream` |

## How do I X?

| I want to… | Do this |
|---|---|
| Spawn a sandbox | `ssh ctl@<host> -p 2222 "new [dev\|go\|py\|elixir\|llm]"`, or `POST /api/sandboxes {"image":"<tag>"}` |
| Run a command in a sandbox | `POST /api/sandboxes/{id}/exec {"cmd":"…"}` (or `forkd exec --child <netns> -- <cmd>` on the host) |
| Get an interactive shell | `ssh <id\|name>@<host> -p 2222` (dev-base image only) |
| Suspend / resume a persistent sandbox | `ssh ctl@<host> -p 2222 "suspend <id>"` / `"resume <id>"` |
| Clone / branch a running sandbox | `ssh ctl@<host> -p 2222 "cp <id>"`, or `POST /api/sandboxes/{id}/clone` |
| Give an agent MCP access | run `spoond-dev-mcp` (see below) |
| Let a sandbox call an LLM | `POST /llm/<lease-id>/<provider>/chat/completions` |
| Reach a sandbox over HTTP | the per-user proxy hostname on `:8891` |
| Run CI | push to the Forgejo instance the runner watches (labels: `go`, `golang`, `elixir`, `forkd`, `dev`, `llm-review`) |
| Linux build/test (Go/Rust) | spawn a `go`/`rust` sandbox and exec, or `ssh <user>@<host>` for the host toolchain |

## Connecting an agent host

### MCP — per-agent sandbox bridge

`spoond-dev-mcp` gives an agent a sandbox on demand: every tool call
creates (or reuses) a sandbox, runs in it, and tears it down. stdio is
the default; an HTTP/SSE transport exists for remote hosts.

```bash
# stdio (default): spawn as a subprocess from the agent host
FORKD_BACKEND_URL=https://127.0.0.1:8890 \
FORKD_AGENT_TOKEN=<agent-token> \
FORKD_IMAGE=dev-base \
  spoond-dev-mcp

# HTTP / streamable transport (remote agent host)
MCP_TRANSPORT=http MCP_LISTEN=:9090 MCP_AUTH_TOKEN=<secret> \
FORKD_AGENT_TOKEN=<agent-token> \
  spoond-dev-mcp
#   POST /mcp   streamable HTTP    GET /sse   SSE    GET /healthz
```

`FORKD_AGENT_TOKEN` is a per-agent credential: create an agent identity
and set its token yourself (the server stores it, it does not mint one):

```bash
curl -s -X POST https://<host>:8890/api/users \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"my-agent","kind":"agent","token":"<choose-a-strong-token>"}'
```

Tools: `shell`, `read_file`, `write_file`, `edit_file`, `list_files`,
`status`.

### SSH — control plane and interactive attach

Register the agent's public key in the same create call as its token, so
it can drive the control plane (`ssh ctl@<host> "verb"`) and attach to
sandboxes (`ssh <id|name>@<host>`):

```bash
# one create call: token (MCP/API) + key fingerprint (ctl/attach)
FP=$(ssh-keygen -lf <agent>.pub | awk '{print $2}')
curl -s -X POST https://<host>:8890/api/users \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d "{\"name\":\"my-agent\",\"kind\":\"agent\",\"token\":\"<token>\",\"fingerprints\":[\"$FP\"]}"
```

ctl verbs: `new`, `ls`, `stat`, `rm`, `keepalive`, `suspend`, `resume`,
`cp`/`clone`, `shelly`/`agent`, `restart`, `tag`, `comment`,
`prompt`, `ssh-key`, `share`, `whoami`, `help`. There is **no** `exec`
verb — run commands via the API or by attaching.

### Lease API

Bearer-token auth (`Authorization: Bearer <token>`). Key routes:
`POST /api/sandboxes`, `GET /api/sandboxes`,
`POST /api/sandboxes/{id}/exec`, `DELETE /api/sandboxes/{id}`,
`/keepalive`, `/suspend`, `/resume`, `/restart`, `/tag`, `/comment`,
`/prompt`, `/stat`, `/stream` (WebSocket PTY), `/clone`, `/share`,
`/api/images`. Full reference: [api.md](api.md).

### ACP — session-scoped agent protocol

JSON-RPC 2.0 over stdio (`spoond acp`). Unlike MCP (stateless per call),
ACP sessions own a lease: `session/new` (cwd + mcpServers) → `session/prompt`
(run a turn with streaming updates) → `session/cancel`. Full reference in
[usage.md](usage.md).

## Configuration reference

- Env/flag reference: [setup.md](setup.md)
- HTTP API: [api.md](api.md)
- Control plane: [ctl.md](ctl.md)
- Deploying + first agent identity: [install.md](install.md)