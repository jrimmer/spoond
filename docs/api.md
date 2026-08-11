# API Reference

Base URL: `https://<backend>:8890` (HTTPS when `TLS_CERT`/`TLS_KEY` are
set, plain HTTP otherwise). All endpoints except `/healthz` and the
`/llm/` prefix require a bearer token:

```
Authorization: Bearer <token>
```

Tokens are the `token=consumer` pairs configured in `CONSUMER_TOKENS`.
The authenticated consumer id becomes the **owner** of every lease they
create. A lease id acts as a capability: only the owner (or anyone
holding the token that owns it) can act on it.

Errors are JSON: `{"error":"human-readable message"}` with an
appropriate HTTP status.

---

## Sandboxes

### `POST /api/sandboxes` — create a lease

Request:

| Field | Type | Default | Notes |
|---|---|---|---|
| `image` | string | *(required)* | image tag, must be in the registry and `KNOWN_IMAGES` allowlist |
| `ttl` | int | `DEFAULT_TTL_SECS` | seconds; capped at `MAX_TTL_SECS` |
| `persistent` | bool | `false` | workspace-backed: not TTL-swept, supports suspend/resume |
| `memory_mib` | int | *(image default)* | guest memory request |
| `network` | string | *(image default)* | legacy network mode |
| `network_policy` | string | `restricted` | `none` \| `lan` \| `internet` \| `restricted` |
| `egress_allowlist` | []string | *(host bridge only)* | additional IPs/CIDRs/domains for `network_policy=restricted` |
| `init_cmd` | string | *(none)* | command run at sandbox start |

Response `201 Created`:

```json
{
  "id": "8f3a…32hex…",
  "address": "10.42.0.2:8888",
  "image": "dev-base",
  "ttl": 300,
  "persistent": false,
  "expires_at": "2026-08-11T03:00:00Z"
}
```

### `GET /api/sandboxes` — list leases

Response `200 OK`: `{"sandboxes":[ {…lease…}, … ]}` where each lease has
`id`, `image`, `address`, `expires_at` (RFC3339), `persistent`,
`suspended`, `name`, `comment`, `network_policy`, `egress_allowlist`.

### `GET /api/sandboxes/{id}` *(via `GET /api/names/{name}`)* — resolve by name

`GET /api/names/{name}` returns `{"id": "<lease-id>"}` for a friendly
name set with `tag`. Used by the SSH gateway (`ssh <name>@…`) and by
scripts/LLM tools.

### `DELETE /api/sandboxes/{id}` — delete

Releases the sandbox. `200 OK` on success.

### `POST /api/sandboxes/{id}/exec` — run a command

Request:

| Field | Type | Default | Notes |
|---|---|---|---|
| `cmd` | string | *(required)* | shell command (executed via `bash -lc`) |
| `cwd` | string | *(none)* | working directory |
| `env` | object | *(none)* | extra environment variables |
| `timeout` | int | `30` | seconds; capped at 300 |

Response `200 OK`:

```json
{"stdout": "…", "stderr": "…", "exit": 0}
```

`409 Conflict` if the lease is suspended (`resume` it first).

### `GET /api/sandboxes/{id}/stat` — guest metrics

One-shot, stateless probe (loadavg/meminfo/netdev/df via exec, 5s
timeout). Response `200 OK`:

```json
{
  "cpu":  {"load1": 0.08},
  "mem":  {"used_mib": 320, "total_mib": 1024},
  "disk": {"used_mib": 512, "total_mib": 4096},
  "net":  {"rx_bytes": 12345, "tx_bytes": 6789}
}
```

### `GET /api/sandboxes/{id}/endpoint` — resolve network endpoint

Returns the underlying forkd sandbox endpoint (for gateway/SSH use):

```json
{"id":"…","forkd_id":"…","image":"…","netns":"forkd-…","guest_addr":"10.42.0.2:8888"}
```

### `GET /api/sandboxes/{id}/stream` — interactive PTY (WebSocket)

Upgrade to WebSocket; first client message `{"args":[…],"cwd":…,
"env":{…},"pty":true}` starts a PTY; output streams as text frames;
client text frames are written to process stdin; `{"action":"stop"}`
terminates.

### `POST /api/sandboxes/{id}/keepalive` — extend persistent lease

Request `{"ttl": <seconds>}` (0 = `MAX_TTL_SECS`; capped). Response:
`{"id":"…","persistent":true,"expires_at":"…"}`. `400` if the lease is
not persistent.

### `POST /api/sandboxes/{id}/suspend` — snapshot + stop

Workspace-backed persistent leases only. The controller snapshots the
sandbox and stops it; the lease remains and can be resumed.

### `POST /api/sandboxes/{id}/resume` — start from snapshot

Re-hydrates a suspended workspace-backed lease. `400` if not
workspace-backed; `409` if already running.

### `POST /api/sandboxes/{id}/restart` — reboot

Restarts a running sandbox's guest OS.

### `POST /api/sandboxes/{id}/tag` — friendly name

Request `{"name": "<unique-per-owner-name>"}`. Response
`{"id":"…","name":"…","ok":true}`. Names enable `ssh <name>@…` and
`GET /api/names/{name}`.

### `POST /api/sandboxes/{id}/comment` — annotate

Request `{"comment": "…"}`. Response `{"id":"…","comment":"…","ok":true}`.

### `POST /api/sandboxes/{id}/clone` — branch to new snapshot

Request (optional) `{"tag":"my-snapshot"}` — default tag
`clone-<id8>-<unix>`. Branches the running sandbox, spawns a lease from
the branch (persistent, `MAX_TTL_SECS`). Response `201 Created`:
`{"id":"…","image":"…","source":"<source-id>","branch_tag":"…","persistent":true,"expires_at":"…"}`.

### `POST /api/sandboxes/{id}/prompt` — message the in-sandbox Shelley agent

Request `{"message":"…","model":"gpt-oss-20b-fireworks"}` (model
optional). Polls the Shelley conversation API inside the sandbox and
returns the agent's reply. Requires the agent to be running (see
`shelly` ctl verb). Response `200 OK` with `{"reply":"…"}`.

---

## Images & health

### `GET /api/images`

Response: `{"images":["dev-base","go-base","py-base","elixir","llm",…]}`

### `GET /healthz`

No auth. Returns `200 OK` (plain `ok`) when the backend is alive —
for Gatus/load balancers.

### `GET /metrics`

Proxies forkd-controller's Prometheus metrics. Admin-only (identity
store present) — requires the caller's token to resolve to an admin
user; `403` for non-admins and legacy consumer tokens when a store is
present.

---

## LLM gateway (per-lease)

`POST /llm/{lease-id}/openai/chat/completions` — OpenAI-compatible chat
completion against the lease's sandbox-hosted LLM gateway. The lease id
in the path is the capability; sandboxes hold no consumer token. When no
`LLM_UPSTREAM_URL` is configured the gateway forwards to the sandbox's
own `127.0.0.1:9000` Shelley agent instead.

Per-user key auth (epic #26 T8): when the lease owner has an LLM key
configured, requests must present it as `Authorization: Bearer
<user-key>`. Missing/wrong/foreign keys → `401`. Owners without a key
keep the open behavior — **unless the deployment sets
`LLM_OPEN_LEGACY=0`** (security review #37 C2): with an identity store
present, keyless identity users are then denied outright (`401`) and
only legacy consumer-owned leases stay open. The user key is replaced by
the server-side upstream key before forwarding, so it never reaches the
provider.

## Users & identity (epic #26)

The user store (`USERS_FILE`) makes people and agents first-class
identities. User endpoints are admin-gated except `/me`, `/by-name`,
`/by-key`, and `/identity-status`; **`GET /api/users` is admin-only**
(security review #37 C1 — the full directory is not enumerable by any
token holder).

### `POST /api/users` — create a user

Bootstrap: when the store is empty, the first create is open (or gated
by `X-Bootstrap-Token: <BOOTSTRAP_TOKEN>` when configured — security
review #37 H3/M4) and the first user becomes admin. After that, admin
only.

Request:

```json
{
  "name": "jason",
  "kind": "person",
  "fingerprints": ["SHA256:AbC…", "SHA256:DeF…"],
  "token": "optional-bearer-token"
}
```

- `kind`: `person` | `agent` — agents are non-interactive identities
  (MCP/ACP endpoints authenticate as their agent user; epic #26 U4).
- `fingerprints`: SSH public-key fingerprints (use `ssh-keygen -lf
  pubkey.pub`) — the gateway's `PublicKeyCallback` resolves these.
- `token`: optional per-user bearer token (like `CONSUMER_TOKENS`
  entries, but bound to the identity).

Response `201 Created`: `{"user": {id, name, kind, admin, …}}` (token
hash, LLM key hash, and fingerprints are never exposed).

### `GET /api/users` — list users (admin only)

`403` for non-admin callers (security review #37 C1).

### `GET /api/users/me` — current user

Self-service: `{"user": {id, name, kind, admin, max_leases, max_ttl}}`.

### `GET /api/users/by-name/{name}` — minimal lookup

`{"user": {id, name}}` — the minimal shape for share-granting and
gateway name resolution (no admin flags or fingerprints).

### `GET /api/users/by-key?fingerprint=…` — resolve SSH key

`{"user": {id, name}}` — the gateway calls this in its
`PublicKeyCallback`; minimal shape (security review #37 rescan F5).

### `DELETE /api/users/{id}` — remove a user (admin only)

Removes the identity and its keys. **This is what actually revokes SSH
access** — the gateway treats the identity store as authoritative when
present, so removing the user invalidates all their keys immediately
(security review #37 H1).

### `POST /api/users/{id}/quota` — set lease quota (admin only)

Request `{"max_leases": N, "max_ttl": S}` — concurrent-lease cap and
per-user TTL ceiling (epic #26 U5). `0` = unlimited/unset. Over-cap
creates return `429`.

### `POST /api/users/{id}/llm-key` — set/rotate/revoke a user's LLM gateway key

Admin only. Request: `{"llm_key": "<slk-…>"}`; an empty `llm_key`
revokes (the owner's leases revert to the open gateway). The key is
stored as a salted SHA-256 hash and never returned in responses.
Response `200 OK` with the user object (no `llm_key_hash` field). `404`
unknown user; `403` non-admin.

### `GET /api/identity-status`

`{"identity_store": true|false}` — tells the SSH gateway whether the
backend has an identity store, so the gateway knows whether key
resolution must be authoritative (security review #37 H1). Unauthenticated.

## Shares (epic #26 U9)

A lease owner can grant another user access to a lease for a limited
time — sharing a workspace with a collaborator or an agent without
copying the lease id/capability.

### `POST /api/sandboxes/{id}/share` — grant (owner only)

Request: `{"grantee": "<user-id>", "mode": "ssh"|"http", "ttl": 3600}`.
`grantee` must be an existing user id (or resolvable name); `ttl` in
seconds (0 = no expiry); `mode` selects which operations the grantee
may perform: `ssh` → `/endpoint`, `/prompt` (interactive/agent access);
`http` → `/exec`, `/stream`, `/stat`, proxy. Response `201 Created` with
the share record. `400` unknown grantee, `403` non-owner.

### `GET /api/sandboxes/{id}/share` — list (owner only)

`{"shares": [{grantee, mode, expires_at, …}]}`.

### `DELETE /api/sandboxes/{id}/share/{grantee}` — revoke (owner only)

`200 OK` — the grantee loses access immediately. `404` if not shared.

Grantee access is enforced owner-scoped (`lookupWithShare`): a shared
lease behaves like the grantee's own for the granted operations until
expiry or revocation.

---

## Auth & ownership model

- Every request is authenticated by consumer token → consumer id, or by
  per-user token → identity user.
- Leases belong to the consumer/user that created them; operations check
  `owner == lease.Owner` (shares are the explicit exception).
- The SSH gateway authenticates users by SSH key (identity store
  authoritative when present) and calls the backend as that user
  (trusted impersonation via `X-Spoond-User-Id`).
- Persistent leases survive TTL sweeps; `keepalive` extends them;
  `IDLE_TIMEOUT_SECS` can auto-suspend idle persistent leases.
- The lease id doubles as a capability (e.g. `GET /api/…/endpoint` is
  how the gateway resolves `ssh <id>@…`).
