# Usage guide

Practical recipes for working with spoond sandboxes: SSH, exec, agents,
proxying, and policies.

## Quick start

```bash
# One-liner: create + run + delete
curl -s -X POST https://sandbox.example.com/api/sandboxes \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"image":"dev-base","ttl":600}'
# → {"id":"…","address":"10.42.0.2:8888",…}

ssh <id>@sandbox.example.com "uname -a"          # run a command
ssh <id>@sandbox.example.com                      # interactive shell
ssh ctl@sandbox.example.com "rm <id>"             # release
```

## Three ways to run a command

1. **SSH into the sandbox** (needs the gateway key in the image):
   ```bash
   ssh <id>@sandbox.example.com -p 2222 "ls -la"
   ```
2. **Control plane** (no key-in-image needed, your ctl key only):
   ```bash
   ssh ctl@sandbox.example.com -p 2222 "ls --json"
   ```
3. **API exec** (best for LLM tools / automation):
   ```bash
   curl -s -X POST https://sandbox.example.com/api/sandboxes/<id>/exec \
     -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
     -d '{"cmd":"ls -la","timeout":30}'
   # → {"stdout":"…","stderr":"","exit":0}
   ```

## Interactive sessions

```bash
# Fresh sandbox (auto-creates persistent dev-base lease)
ssh new@sandbox.example.com -p 2222

# A specific image
ssh new-go@sandbox.example.com -p 2222

# Re-attach to an existing lease
ssh <id>@sandbox.example.com -p 2222

# Detach (tmux) then reconnect later
#   Ctrl-b d
ssh <id>@sandbox.example.com -p 2222
```

The MOTD prints the reconnect hint with the exact port; the footer shows
the lease id. Friendly names work after `ctl tag`:

```bash
ssh ctl@sandbox.example.com "tag <id> mybox"
ssh mybox@sandbox.example.com -p 2222
```

## Persistent vs ephemeral

- **Ephemeral** (`persistent:false`, default): TTL-based; auto-deleted
  when `expires_at` passes. Cheap, disposable.
- **Persistent** (`persistent:true`): survives TTL sweeps; `keepalive`
  extends it; `suspend`/`resume` snapshot/restore (workspace-backed);
  `IDLE_TIMEOUT_SECS` can auto-suspend idle ones.

```bash
curl -s -X POST …/api/sandboxes -H "Authorization: Bearer $TOKEN" \
  -d '{"image":"dev-base","persistent":true,"ttl":3600}'
ssh ctl@sandbox.example.com "keepalive <id>"
ssh ctl@sandbox.example.com "suspend <id>"   # snapshot + stop
ssh ctl@sandbox.example.com "resume <id>"    # back to work
```

## Clones (snapshots)

```bash
ssh ctl@sandbox.example.com "cp <id> my-snapshot"   # branch + spawn
# then use the new lease id as a clean starting point
```

## Web server inside a sandbox

With `PROXY_ADDR` + a wildcard Caddy front:

- `https://<id>.sandbox.example.com/` → port 8888 inside the sandbox
- `https://<id>-8080.sandbox.example.com/` → port 8080

```bash
# inside the sandbox
python3 -m http.server 8080
# outside
curl -s https://<id>-8080.sandbox.example.com/
```

## LLM gateway

Per-lease OpenAI-compatible endpoint (no bearer token — the lease id is
the capability):

```bash
curl -s https://sandbox.example.com/llm/<id>/openai/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-oss-20b-fireworks","messages":[{"role":"user","content":"hi"}]}'
```

Config: `LLM_UPSTREAM_URL`/`LLM_UPSTREAM_KEY` (server-side upstream) or
the sandbox's own Shelley agent on `127.0.0.1:9000`.

## In-sandbox coding agent (Shelley)

```bash
ssh ctl@sandbox.example.com "shelly <id>"        # start the agent
ssh ctl@sandbox.example.com "prompt <id> write a fibonacci function"
```

## MCP / ACP agent endpoints

### `forkd-dev-mcp` (MCP stdio server, JSON-RPC 2.0 over stdio)

Tools: `shell`, `read_file`, `write_file`, `edit_file`, `list_files`,
`status`. Point Goose/Claude Code-style MCP clients at it:

```bash
FORKD_BACKEND_URL=https://sandbox.example.com FORKD_TOKEN=<consumer-token> \
  ./spoond mcp
```

### `forkd-acp` (Agent Client Protocol server)

Sessions map 1:1 to leases; the agent loop runs through the LLM gateway
with in-sandbox tools. One `spoond acp` process serves the whole
conversation (sessions are process-scoped).

```bash
FORKD_BACKEND_URL=https://sandbox.example.com FORKD_TOKEN=<consumer-token> \
  FORKD_LLM_MODEL=gpt-oss-20b-fireworks ./spoond acp
```

## Network policies

| Policy | Egress |
|---|---|
| `none` | none (loopback only) |
| `lan` | RFC1918 + link-local (default) |
| `internet` | full egress via host NAT |
| `restricted` | allowlisted IPs/CIDRs/domains only |

```bash
curl -s -X POST …/api/sandboxes -H "Authorization: Bearer $TOKEN" \
  -d '{"image":"dev-base","network_policy":"restricted","egress_allowlist":["10.1.0.47","github.com"]}'
```

## LLM-driven usage pattern

The whole surface is API-first: an LLM skill can create a lease, exec
commands, read output, and release it — no shell needed:

1. `POST /api/sandboxes` → id
2. `POST /api/sandboxes/{id}/exec` → stdout/stderr/exit (loop as needed)
3. `GET /api/sandboxes/{id}/stat` → resource awareness
4. `DELETE /api/sandboxes/{id}` → always release

`forkd-curl` (in `scripts/`) wraps this with a friendly CLI.
