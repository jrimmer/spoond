# Usage guide

Practical recipes for working with spoond sandboxes: SSH, exec, agents,
proxying, and policies. What a sandbox *is* (a Firecracker microVM
restored from an image snapshot, on the E2B substrate) is
[substrate.md](substrate.md); the endpoint reference is [api.md](api.md).

## Quick start

```bash
# One-liner: create + run + delete
curl -s -X POST https://sandbox.example.com/api/sandboxes \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"image":"dev-base","ttl":600}'
# → {"id":"…","address":"10.11.0.7","expires_at":"…",…}

ssh <id>@sandbox.example.com "uname -a"          # run a command
ssh <id>@sandbox.example.com                      # interactive shell
ssh ctl@sandbox.example.com "rm <id>"             # release
```

Or skip curl entirely: `ssh new@sandbox.example.com` creates a persistent
dev sandbox and drops you into tmux.

## Three ways to run a command

1. **SSH into the sandbox**:
   ```bash
   ssh <id>@sandbox.example.com -p 2222 "ls -la"
   ```
2. **Control plane** (your ctl key, no key-in-image needed):
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

Interactive/agent clients can also drive a PTY over the API:
`GET /api/sandboxes/{id}/stream` (WebSocket) starts a process with
`{"args":["/bin/bash","-l"],"pty":true}` and relays input/output/resizes
— that is exactly how the SSH gateway itself attaches. See
[api.md](api.md#get-apisandboxesidstream--interactive-process-websocket).

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

`new` accepts the same short names as `ctl new` (`dev`, `go`, `py`,
`python`, `elixir`, `llm`, `base`) or a full image tag, but only images
with sshd qualify for interactive SSH — the gateway's `--ssh-images`
list (default `dev-base`; CI images like `go-base` have no sshd and are
rejected with a hint). The MOTD prints the reconnect hint with the exact
port; the footer shows the lease id. Friendly names work after `ctl tag`:

```bash
ssh ctl@sandbox.example.com "tag <id> mybox"
ssh mybox@sandbox.example.com -p 2222
```

## Persistent vs ephemeral

- **Ephemeral** (`persistent:false`, default): TTL-based; auto-deleted
  when `expires_at` passes. Cheap, disposable.
- **Persistent** (`persistent:true`): survives TTL sweeps; `keepalive`
  extends it; `suspend`/`resume` snapshot/restore (a suspend is an E2B
  *pause* — snapshot to a new build, then stop; a resume restores that
  build with the **same sandbox id**, so your tmux session comes back);
  `IDLE_TIMEOUT_SECS` can auto-suspend idle ones.

```bash
curl -s -X POST …/api/sandboxes -H "Authorization: Bearer $TOKEN" \
  -d '{"image":"dev-base","persistent":true,"ttl":3600}'
ssh ctl@sandbox.example.com "keepalive <id>"
ssh ctl@sandbox.example.com "suspend <id>"   # snapshot + stop
ssh ctl@sandbox.example.com "resume <id>"    # back to work
```

Take a checkpoint of a *running* persistent sandbox to bound what a host
restart can cost (`POST /api/sandboxes/{id}/checkpoint`); the platform
also checkpoints active persistent leases hourly by default. A sandbox
lost with no checkpoint answers `410` — delete it and start again.

## Clones and forks (snapshot branching)

`clone`/`cp` checkpoints the running sandbox and starts a fresh
persistent lease from that build, copying its network policy and exposed
ports. `fork` does the same for N copies at once:

```bash
ssh ctl@sandbox.example.com "cp <id> my-snapshot"   # branch + spawn
curl -s -X POST …/api/sandboxes/<id>/fork \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"count":3}'
# → {"source":"…","build_id":"…","ids":["…","…","…"]}
```

The checkpoint build id is the snapshot's identity; `GET /api/snapshots`
lists yours and `DELETE /api/snapshots/{build_id}` reclaims one. Memory
comes with it — a fork resumes with the source's RAM, which is what makes
it useful for "try a risky thing, keep the original".

## Web server inside a sandbox

With `PROXY_ADDR` + a wildcard Caddy front:

- `https://<id>.sandbox.example.com/` → port 3000 inside the sandbox
- `https://<id>-8080.sandbox.example.com/` → port 8080

```bash
# inside the sandbox
python3 -m http.server 8080
# outside
curl -s https://<id>-8080.sandbox.example.com/
```

Note for guest apps: requests arrive with `Host: 127.0.0.1:5007` (the
substrate's sandbox proxy); the public hostname is in
`X-Forwarded-Host`. Frameworks with a host allowlist must read that
header.

## Exposing a service to peer sandboxes

A sandbox can publish up to 8 TCP ports for *peer sandboxes* (not the
LAN) — a database for a CI job, for instance:

```bash
# the database lease
curl -s -X POST …/api/sandboxes -H "Authorization: Bearer $TOKEN" \
  -d '{"image":"scylla","persistent":true,"expose_ports":[9042]}'
# → "exposed":{"9042":"10.11.0.7:9042"}

# the job's lease must allow that peer by id, name or lease:<id>
curl -s -X POST …/api/sandboxes -H "Authorization: Bearer $TOKEN" \
  -d '{"image":"go-base","egress_allowlist":["<db-lease-id>"]}'
```

Guest port 49983 (the in-guest agent) is reserved and can never be
published.

## LLM gateway

Per-lease OpenAI-compatible endpoint (the lease id in the path is the
capability):

```bash
curl -s https://sandbox.example.com/llm/<id>/openai/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-oss-20b-fireworks","messages":[{"role":"user","content":"hi"}]}'
```

### Per-user keys

When a lease owner has a per-user LLM key configured, `/llm/` requests
on their leases must present it in the standard OpenAI-compatible
`Authorization` header:

```bash
curl -s https://sandbox.example.com/llm/<id>/openai/chat/completions \
  -H "Authorization: Bearer <user-llm-key>" \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-oss-20b-fireworks","messages":[{"role":"user","content":"hi"}]}'
```

The user key only authorizes the caller (missing/wrong/foreign keys get
`401`); it is replaced by the server-side upstream key before the
request is forwarded, so it never reaches the provider. Owners **without**
a key keep the legacy open behavior (backward compatible), including
deployments with no identity store at all — unless `LLM_OPEN_LEGACY` is
unset, which is the default: keyless identity users are then denied
(set `LLM_OPEN_LEGACY=1` to keep them open).

An admin sets/rotates/revokes a key (stored hashed, never returned by
the API):

```bash
curl -s -X POST https://sandbox.example.com/api/users/<user-id>/llm-key \
  -H "Authorization: Bearer <admin-token>" -H 'Content-Type: application/json' \
  -d '{"llm_key":"slk-…"}'        # set/rotate
curl -s -X POST https://sandbox.example.com/api/users/<user-id>/llm-key \
  -H "Authorization: Bearer <admin-token>" -H 'Content-Type: application/json' \
  -d '{"llm_key":""}'             # revoke (owner reverts to open)
```

Optional per-user concurrency cap (in-flight `/llm/` requests per user;
`429` when exceeded): set `LLM_MAX_CONCURRENT_PER_USER` (default 0 =
unlimited).

Config: `LLM_UPSTREAM_URL`/`LLM_UPSTREAM_KEY` (server-side upstream).
With no upstream configured the route is not mounted at all (`404`).

## In-sandbox coding agent (Shelley)

```bash
ssh ctl@sandbox.example.com "shelly <id>"        # start the agent
ssh ctl@sandbox.example.com "prompt <id> write a fibonacci function"
```

## MCP / ACP agent endpoints

### `spoond mcp` (MCP stdio server, JSON-RPC 2.0 over stdio)

Tools: `shell`, `read_file`, `write_file`, `edit_file`, `list_files`,
`status`. Point Goose/Claude Code-style MCP clients at it:

```bash
FORKD_BACKEND_URL=https://sandbox.example.com FORKD_AGENT_TOKEN=<agent-token> \
  ./spoond mcp
```

Create the agent user first with `POST /api/users` and
`kind=agent` — the agent needs a `token` in the request body, not an
SSH key (`ssh ctl@… "ssh-key add …"` cannot create one: it always
posts a `person` with no token) — then set `FORKD_AGENT_TOKEN` to that
token; the legacy `FORKD_TOKEN` fallback logs a deprecation warning.

### `spoond acp` (Agent Client Protocol server)

Sessions map 1:1 to leases; the agent loop runs through the LLM gateway
with in-sandbox tools. One `spoond acp` process serves the whole
conversation (sessions are process-scoped).

```bash
FORKD_BACKEND_URL=https://sandbox.example.com FORKD_AGENT_TOKEN=<agent-token> \
  FORKD_LLM_MODEL=gpt-oss-20b-fireworks ./spoond acp
```

(The `FORKD_*` names above are the live, supported variable names.)

## Network policies

| Policy | Egress | Default |
|---|---|---|
| `none` | nothing | |
| `lan` | the LAN ranges, host services, DNS, every exposing peer's published ports | |
| `internet` | everything public plus the LAN, host services, DNS, every exposing peer's published ports | |
| `restricted` | host services, DNS, the allowlist, the published ports of the peers the allowlist names | ✅ default |

The default is **`restricted`**: a guest can reach the host services
spoond grants it (the proxy/LLM gateway port and DNS) plus its
allowlist, and peer sandboxes only through the published ports the
allowlist names. Opt in to wider egress with `network_policy=lan` or
`internet` — which also admits **every** exposing peer's published
ports, on any owner, with no allowlist gate.

```bash
curl -s -X POST …/api/sandboxes -H "Authorization: Bearer $TOKEN" \
  -d '{"image":"dev-base","network_policy":"restricted","egress_allowlist":["10.0.0.47","github.com"]}'
```

Allowlist entries may be IPs, CIDRs or domains — or a **lease
reference** (another lease's id, its friendly name, or `lease:<id>`),
which permits that lease's published ports. Policy can be changed live
with `POST /api/sandboxes/{id}/network` — no restart, no new lease.

## Multi-user tenancy

With `USERS_FILE` set, people and agents are first-class identities.
Operational flow for an admin:

```bash
# 1. Bootstrap the first (admin) user with your SSH public key
ssh ctl@sandbox.example.com "ssh-key add ssh-ed25519 AAAA… you@laptop you"
# (first user is admin; with BOOTSTRAP_TOKEN set, do this via direct
#  API call — see docs/setup.md "First-user bootstrap")

# 2. Add teammates (admin; each becomes a `person` identity)
ssh ctl@sandbox.example.com "ssh-key add ssh-ed25519 AAAA… alice@mbp alice"

#    Agents are created over the API instead (kind=agent — what the
#    MCP/ACP endpoints authenticate as):
curl -s -X POST https://sandbox.example.com/api/users \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"ci","kind":"agent","token":"<its-token>"}'

# 3. List users, set quotas (admin)
ssh ctl@sandbox.example.com "ssh-key ls"
curl -s -X POST https://sandbox.example.com/api/users/<alice-id>/quota \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"max_leases": 4, "max_ttl": 7200}'

# 4. Everyone uses their own key; leases are ownership-scoped
ssh alice@sandbox.example.com            # fresh sandbox, owned by alice
ssh ctl@sandbox.example.com "ls --json"  # alice sees only her own
```

**Sharing a sandbox** — hand a collaborator or an agent limited access
without copying the lease capability:

```bash
ssh ctl@sandbox.example.com "share add <id> alice http 3600"   # 1h exec/stream
ssh ctl@sandbox.example.com "share add <id> ci ssh"            # interactive, no expiry
ssh ctl@sandbox.example.com "share ls"                      # every share on your leases
ssh ctl@sandbox.example.com "share rm <id> alice"              # revoke immediately
```

**Per-user LLM keys** — an admin gives a user their own gateway key
(`POST /api/users/<id>/llm-key`); that user's `/llm/` requests must then
present it. Guests' leases stay isolated: exec/stream/stat are
owner-scoped everywhere, and a shared lease is the only way in.

**Per-user proxy hostnames** — with forward-auth enabled
(`PROXY_AUTH_MODE=forward-auth`), each user gets
`<label>.<user>.sandbox.example` and lookups are scoped to the
authenticated owner; in the default capability model, only the
unguessable 32-hex lease id hostname resolves.

## LLM-driven usage pattern

The whole surface is API-first: an LLM skill can create a lease, exec
commands, read output, and release it — no shell needed:

1. `POST /api/sandboxes` → id
2. `POST /api/sandboxes/{id}/exec` → stdout/stderr/exit (loop as needed)
3. `GET /api/sandboxes/{id}/stat` → resource awareness
4. `DELETE /api/sandboxes/{id}` → always release

`scripts/forkd-curl` (its name is historical; it wraps this API, not the
old controller) injects the bearer token and pins the API hostname to
loopback so TLS validates — `FORKD_API`, `FORKD_TOKEN` or
`FORKD_TOKEN_FILE` configure it.
