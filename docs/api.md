# API Reference

Base URL: `https://<backend>:8890` (HTTPS when `TLS_CERT`/`TLS_KEY` are
set, plain HTTP otherwise). All endpoints except `/healthz` and the
`/llm/` and `/lease/` prefixes require a bearer token:

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
| `image` | string | *(required)* | image name, must have a current build in the catalog (`GET /api/images`) |
| `ttl` | int | `DEFAULT_TTL_SECS` | seconds; capped at `MAX_TTL_SECS` |
| `persistent` | bool | `false` | survives TTL sweeps, supports suspend/resume and periodic checkpoint |
| `memory_mib` | int | *(image default)* | **fixed per image**: `0` or exactly the image's memory, else `400` |
| `network_policy` | string | `restricted` | `none` \| `lan` \| `internet` \| `restricted` |
| `egress_allowlist` | []string | *(host services only)* | additional IPs/CIDRs/domains for `network_policy=restricted` |
| `expose_ports` | []int | *(none)* | guest TCP ports to publish on the lease's host address for peer sandboxes (#70). Max 8 |

Response `201 Created`:

```json
{
  "id": "8f3a…32hex…",
  "owner": "consumer",
  "address": "10.11.0.5",
  "image": "dev-base",
  "ttl": 300,
  "persistent": false,
  "expires_at": "2026-10-01T03:00:00Z",
  "exposed": {"9042": "10.11.0.5:9042"}
}
```

`address` is the sandbox's host-side address (`HostIP`); `exposed` maps each
published port to `<host-ip>:<port>` — reachable from the host and from
sandboxes whose policy reaches it (`lan`, `internet`, or an allowlisted
`restricted`), never from the LAN. The same map appears in `GET
/api/sandboxes`. A create is a **memory-snapshot restore** of the image's
current build, so it is warm.

### `GET /api/sandboxes` — list leases

Response `200 OK`: `{"sandboxes":[ {…lease…}, … ]}` where each lease has
`id`, `owner`, `image`, `address`, `expires_at` (RFC3339), `persistent`,
`suspended`, `state`, `build_id`, `resume_build_id`, `name`, `comment`,
`net_policy`, `egress_allowlist`, `exposed`.

### `GET /api/sandboxes/{id}` — one lease

The list row plus the lifecycle fields:

| Field | Meaning |
|---|---|
| `state` | `running` \| `suspended` \| `recovered` \| `lost` |
| `build_id` | the build the sandbox currently runs (its image's template build, a pause build, a checkpoint build, or a fork source) |
| `resume_build_id` | set while suspended: the pause build `resume` restores from |
| `recovered_from` | set on a lease recovered after a substrate crash: the checkpoint time it resumed from |
| `last_checkpoint_at` | the lease's last checkpoint time |

A `lost` lease answers `410` on exec/stream with `sandbox lost in a
substrate crash; delete this lease`; delete it and start again.

### `GET /api/names/{name}` — resolve by name

Returns `{"id": "<lease-id>"}` for a friendly name set with `tag`. Used by
the SSH gateway (`ssh <name>@…`) and by scripts/agent tools.

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

Returns the substrate sandbox id and the host-side address guests use.
`netns` is always empty on the E2B substrate (kept for response
compatibility; the gateway no longer reads it):

```json
{"id":"…","forkd_id":"i…","image":"…","netns":"","guest_addr":"10.11.0.5"}
```

`guest_addr` is the sandbox's host-side address (`HostIP`), the same value
as `address` in `POST /api/sandboxes`. `forkd_id` is the substrate sandbox
id; the name predates the E2B substrate and is kept for response
compatibility (D5).

### `GET /api/sandboxes/{id}/stream` — interactive PTY (WebSocket)

Upgrade to WebSocket; the first client message is the exec request:
`{"args":[…],"cwd":…,"env":{…},"pty":true,"binary":bool}`.

In **text mode** (the default) every server event comes back as one text
JSON frame — `started`, incremental `stdout`/`stderr`/`pty` chunks,
`exit_code`, `error` — and client text frames carry either
`{"in":"…"}` (written to process stdin) or a control message.

In **binary mode** (`"binary":true`, for programs that emit or consume
arbitrary bytes) process output arrives as WebSocket binary frames whose
first byte selects the channel — `1` stdout, `2` stderr, `3` pty — and
client binary frames are raw stdin. `started`, `exit_code` and `error`
remain text JSON frames in both modes.

Control messages (text frames):

| Message | Effect |
|---|---|
| `{"in":"…"}` | write to process stdin (text mode only) |
| `{"action":"stop"}` | SIGTERM, then keep relaying until the process exits |
| `{"action":"kill"}` | SIGKILL, then keep relaying until the process exits |
| `{"action":"eof"}` | close the process's stdin |
| `{"resize":{"cols":C,"rows":R}}` | resize the PTY |

Closing the WebSocket alone does **not** kill the process — the stream
closes, the process keeps running.

### `POST /api/sandboxes/{id}/keepalive` — extend persistent lease

Request `{"ttl": <seconds>}` (0 = `MAX_TTL_SECS`; capped). Response:
`{"id":"…","persistent":true,"expires_at":"…"}`. `400` if the lease is
not persistent.

### `POST /api/sandboxes/{id}/suspend` — snapshot + stop

Persistent leases only. The substrate snapshots the running sandbox
(memory included) into a new build and stops it; the lease stays, becomes
`suspended`, and can be resumed from `resume_build_id`. `400` if not
persistent; `409` if busy.

### `POST /api/sandboxes/{id}/resume` — start from snapshot

Restores a suspended lease from its pause build, with its memory intact.
`400` if not persistent; `409` if already running.

### `POST /api/sandboxes/{id}/checkpoint` — snapshot a running lease

Snapshots a running sandbox into a new build **without stopping it**: the
lease keeps running, and its `last_checkpoint_at` advances. Persistent
leases are checkpointed automatically every `CHECKPOINT_INTERVAL_MINS`;
this does it on demand. Owner only, live leases only (`409` otherwise,
including busy). Response `200 OK`:

```json
{"id":"…","build_id":"<uuid>","at":"2026-10-01T12:00:00Z"}
```

A checkpoint is what a lease is recovered from after a substrate crash
(see [operations.md](operations.md#crash-recovery-unplanned)).

### `POST /api/sandboxes/{id}/fork` — copy a running sandbox N times

Checkpoints the running sandbox into a new build, then creates `count`
sandboxes from it. Request: `{"count": N, "persistent": bool, "ttl": S}`;
`count` is 1..20 (`400` otherwise). Response `201 Created`:

```json
{"source":"<id>","build_id":"<uuid>","ids":["…","…"]}
```

Forked leases are independent from the source from the moment of the
snapshot.

### `POST /api/sandboxes/{id}/restart` — reboot

Restarts a running sandbox's guest OS.

### `POST /api/sandboxes/{id}/tag` — friendly name

Request `{"name": "<unique-per-owner-name>"}`. Response
`{"id":"…","name":"…","ok":true}`. Names enable `ssh <name>@…` and
`GET /api/names/{name}`.

### `POST /api/sandboxes/{id}/comment` — annotate

Request `{"comment": "…"}`. Response `{"id":"…","comment":"…","ok":true}`.

### `POST /api/sandboxes/{id}/clone` — copy a running sandbox

Checkpoints the running sandbox and creates a new persistent lease from
that build (`MAX_TTL_SECS`). Response `201 Created`:
`{"id":"…","image":"…","source":"<source-id>","build_id":"<uuid>","persistent":true,"expires_at":"…"}`.

### `POST /api/sandboxes/{id}/network` — change egress policy live

Request: `{"network_policy":"none|lan|internet|restricted",
"egress_allowlist":[…]}`. The lease is updated and saved, the egress
config is re-applied to its sandbox, and every peer's allowances are
refreshed. Owner only. Response `200 OK`:
`{"id":"…","network_policy":"…","egress_allowlist":[…]}`.

### `GET /api/snapshots` — list your snapshot builds

Every non-deleted build owned by the caller — pauses, checkpoints and
fork sources (image template builds have no owner and are not listed).
Response `200 OK`:

```json
{"snapshots":[{
  "build_id":"<uuid>","kind":"pause|checkpoint",
  "image":"dev-base","parent_build_id":"<uuid>|null",
  "size_bytes":1234567890,"created_at":"2026-10-01T12:00:00Z",
  "in_use":false}]}
```

`in_use` is true while anything keeps the build (an image's current
build, a resumed lease, a non-deleted child or a `build_refs` entry).

### `DELETE /api/snapshots/{build_id}` — delete a snapshot build

`204 No Content` on success. `404` unknown, deleted, or another owner's
build; `403` for an image template build; `409` while `in_use`.
Deletion frees the build's disk immediately (the catalog GC's rules are
the same).

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

Serves the backend's own Prometheus metrics, and — when `OTEL_PROM_URL`
is set — appends the orchestrator's collector output after a marker line.
Admin-only (identity store present) or the scrape-only `METRICS_TOKEN`;
`403` for non-admins and legacy consumer tokens when a store is present.

---

## Admin (drain, undrain, reconcile)

Operations endpoints for the substrate lifecycle, authenticated with a
separate `Authorization: Bearer <ADMIN_TOKEN>` (not a user or consumer
token). With no `ADMIN_TOKEN` configured the routes answer `404`; a wrong
or missing token answers `401`.

### `POST /api/admin/drain` — pause everything for an orchestrator restart

Pauses every live lease into a pause build (4 at a time), deletes the warm
pool, then waits until the orchestrator reports no running sandboxes and
no outstanding work. Response `200 OK`:

```json
{"paused":N,"failed":[],"pool_deleted":M,"quiesced":true}
```

`503` when the orchestrator is unreachable — nothing changes then. See
[operations.md](operations.md#the-drain-protocol-planned-orchestrator-restarts).

### `POST /api/admin/undrain` — resume after the restart

Resumes every drained lease from its pause build:
`{"resumed":N,"failed":[]}`.

### `POST /api/admin/reconcile` — run crash recovery now

Runs the crash reconciliation that otherwise happens at backend start,
every 30 s and on orchestrator recovery, and returns its summary:
`{"recovered":N,"lost":M}`.

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

## Guest-service endpoints (port `HOST_GUEST_SERVICE_PORT`)

Besides the per-lease LLM gateway above (`/llm/{lease-id}/…`), the
guest-service listener serves routes a sandbox itself calls. Every
network policy — `restricted` included — permits this port.

### `POST /lease/{lease-id}/active` — lease heartbeat

Records activity on a lease so the idle sweep
(`IDLE_TIMEOUT_SECS`) does not auto-suspend it while an agent is
working inside the sandbox. An agent never calls the lease API and holds
no owner token; this route is how it stays visible to the sweeper.

**Capability model:** the lease id in the path is the authorization —
the same model as `/llm/{lease-id}/` and the SSH gateway. No bearer
token; a sandbox that knows its own lease id may heartbeat it, and no
other lease is reachable or revealed through the response.

The heartbeat only sets `LastActive` (the state the idle sweep reads)
and persists it. It does **not** extend `ExpiresAt`, change
persistence, resume a suspended lease, or do anything else.

Responses: `204` on success (no body); `404` for an unknown or released
lease; `409` for a suspended lease; `405` for any other method. Writes
are limited to one per lease per 60 s — later calls inside that window
still return `204`, so a fast loop cannot hammer the store.

A guest uses the values its sandbox was created with
(`SPOOND_GATEWAY_URL`, `SPOOND_LEASE_ID`):

```
curl -fsS -X POST "$SPOOND_GATEWAY_URL/lease/$SPOOND_LEASE_ID/active"
```

Metric: `spoond_lease_heartbeats_total` counts accepted heartbeats
(every `204`, including rate-limited no-op calls).

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
