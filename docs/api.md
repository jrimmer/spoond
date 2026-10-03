# API Reference

Base URL: `https://<backend>:8890` (HTTPS when `TLS_CERT`/`TLS_KEY` are
set, plain HTTP otherwise). The resource is the **lease**; every route
lives under `/api/leases/…`. `/api/sandboxes/…` is a permanent alias for
the same routes — identical behavior, auth and responses — kept for
every client written before 2.0; new code should use `/api/leases/…`.
All endpoints except `/healthz`,
`GET /hive/guide` (the hive's enlistment guide, below), the
`/api/admin/*` routes (which carry their own `ADMIN_TOKEN`) and the
`/llm/` prefix (where the lease id in the path is the capability)
require a bearer token (`/metrics` also accepts the scrape-only
`METRICS_TOKEN`):

```
Authorization: Bearer <token>
```

Tokens are either `token=consumer` pairs from `CONSUMER_TOKENS` or
per-user tokens from the identity store. The authenticated consumer id
becomes the **owner** of every lease they create. A lease id acts as a
capability: only the owner (or anyone holding the token that owns it, or
a grantee via a share) can act on it.

Errors are JSON: `{"error":"human-readable message"}` with an appropriate
HTTP status. A lease runs on an E2B microVM (the substrate "sandbox");
what that implies for a given field is noted below, and the platform
itself is described in [substrate.md](substrate.md).

---

## Leases

### `POST /api/leases` — create a lease

Request:

| Field | Type | Default | Notes |
|---|---|---|---|
| `image` | string | *(required)* | image name; must have a current build in the catalog (`GET /api/images`) |
| `ttl` | int | `DEFAULT_TTL_SECS` | seconds; capped at `MAX_TTL_SECS` and at the user's `max_ttl` |
| `persistent` | bool | `false` | not TTL-swept; supports keepalive, suspend/resume, checkpoint |
| `memory_mib` | int | `0` | **must be `0` or exactly the image's `memory_mb`** — memory is fixed per image (a snapshot restores with its build's RAM). Any other value is `400`. |
| `network` | string | *(ignored)* | accepted for compatibility |
| `init_cmd` | string | *(ignored)* | accepted for compatibility |
| `network_policy` | string | `restricted` | `none` \| `lan` \| `internet` \| `restricted` |
| `egress_allowlist` | []string | *(empty)* | IPs/CIDRs/domains for `restricted`; also lease references (see below) |
| `expose_ports` | []int | *(none)* | guest TCP ports published for peer leases. Max 8; port 49983 (envd) is refused; duplicates and out-of-range ports are refused |

Response `201 Created`:

```json
{
  "id": "8f3a…32hex…",
  "owner": "u-…",
  "address": "10.11.0.7",
  "image": "dev-base",
  "ttl": 300,
  "persistent": false,
  "expires_at": "2026-10-01T03:00:00Z",
  "exposed": {"9042": "10.11.0.7:9042"}
}
```

`address` is the lease's host-side address (no port). `exposed` maps
each published port to `<address>:<port>` — reachable from peers whose
egress policy permits it (see [Network policy](#network-policy)), never
from the LAN. The same map appears in `GET /api/leases`.

Errors: `400` bad policy/ports/memory, `404` unknown image, `429` quota,
`503` capacity (not enough free hugepage memory for the image, or the
node is not healthy).

### `GET /api/leases` — list leases

Response `200 OK`: `{"sandboxes":[ {…lease…}, … ]}` where each row has
`id`, `owner`, `image`, `address`, `expires` (unix seconds),
`persistent`, `suspended`, `state`, `build_id`, `resume_build_id`,
`name`, `comment`, `net_policy`, `egress_allowlist`, `exposed`.

### `GET /api/leases/{id}` — lease detail
The same object as a list row plus `state`, `recovered_from` (RFC 3339 or
`""`) and `last_checkpoint_at`. Requires the owner or an `http` share.

`state` is `running`, `suspended`, `recovered` or `lost`. `recovered`
behaves exactly like `running` — it marks a lease the crash reconcile
resumed from a checkpoint, and keeps showing `recovered` until the lease
is suspended or restarted. `lost` means the sandbox died with no
checkpoint; those leases answer `410` and should be deleted.

`build_id` is the E2B build the running sandbox was created from (`""`
while suspended, where `resume_build_id` is the one to resume from).

### `GET /api/names/{name}` — resolve by name

`{"id": "<lease-id>", "name": …, "image": …}` for a friendly name set
with `tag`. Owner-scoped. Used by the SSH gateway (`ssh <name>@…`) and by
scripts.

### `DELETE /api/leases/{id}` — delete

Releases the lease and its sandbox. `204 No Content`. Builds are left for
the GC (and stay listed by `GET /api/snapshots` until reclaimed).

### `POST /api/leases/{id}/exec` — run a command

| Field | Type | Default | Notes |
|---|---|---|---|
| `cmd` | string | *(required)* | shell command (run via `bash -c`) |
| `cwd` | string | *(none)* | working directory |
| `env` | object | *(none)* | extra environment variables |
| `timeout` | int | `30` | seconds; capped at `MAX_EXEC_TIMEOUT_SECS` (default 300) |

Response `200 OK`:

```json
{"stdout": "…", "stderr": "…", "exit": 0}
```

`409` if the lease is suspended (resume it first); `410` if it is
`lost`, or if the sandbox no longer exists on the substrate; `429` when
the per-owner concurrent exec/stream cap is reached. (There is no
lease-busy `409` here: the concurrency guard on exec is the per-owner
cap, which yields `429`.)

### `GET /api/leases/{id}/stat` — guest metrics

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

### `GET /api/leases/{id}/endpoint` — resolve lease endpoint

Kept for compatibility. The gateway no longer uses it (SSH sessions are
relayed over `/stream`). The `forkd_id` key keeps its pre-2.0 name
(stored/protocol data; renaming it would break clients):

```json
{"id":"…","forkd_id":"<sandbox id>","image":"…","netns":"","guest_addr":"10.11.0.7"}
```

### `GET /api/leases/{id}/stream` — interactive process (WebSocket)

Upgrade to WebSocket; the first client message starts a process:

```json
{"args":["/bin/bash","-l"],"cwd":"/work","env":{…},"pty":true,
 "binary":true,"cols":120,"rows":40}
```

| Field | Default | Notes |
|---|---|---|
| `args` | *(required)* | argv |
| `cwd`, `env` | *(none)* | working directory and extra env |
| `pty` | `true` | allocate a PTY |
| `binary` | `false` | binary framing (below) |
| `cols`, `rows` | `80`×`24` | initial PTY size |

**Text mode (the default).** Server events are one text JSON frame each,
with no trailing newline:

- `{"stream":"started","pid":1234,"pty":true}`
- `{"out":"…"}` — stdout, stderr and PTY output all use this frame
- `{"exit_code":N}`, then the socket closes
- `{"error":"…"}`, then the socket closes

Client text frames are control JSON:

| Frame | Effect |
|---|---|
| `{"in":"…"}` | write to the process (PTY input, or stdin without a PTY) — **text mode only**; in binary mode send the bytes as binary frames |
| `{"resize":{"cols":C,"rows":R}}` | resize the PTY |
| `{"action":"stop"}` | SIGTERM |
| `{"action":"kill"}` | SIGKILL |
| `{"action":"eof"}` | close stdin (non-PTY only) |

**Binary mode** (`"binary":true`) is what the SSH gateway uses. Process
output arrives as WebSocket **binary** frames whose first byte selects
the channel — `1` stdout, `2` stderr, `3` PTY — with the payload after
it; client binary frames are raw input written to the process; text
frames carry the same control JSON as above. `started`, `exit_code` and
`error` stay text JSON frames in both modes.

Closing the WebSocket stops the relay but does **not** kill the process;
send `stop` or `kill` for that.

### `POST /api/leases/{id}/keepalive` — extend a persistent lease

Request `{"ttl": <seconds>}` (0 = `MAX_TTL_SECS`; capped). Response:
`{"id":"…","persistent":true,"expires_at":"…"}`. `400` if the lease is
not persistent.

### `POST /api/leases/{id}/suspend` — snapshot + stop

Persistent leases only (`400` otherwise). The sandbox is paused into a
new build and stops; the lease stays, becomes `state: "suspended"`, and
records `resume_build_id`. Response
`{"id":"…","status":"suspended","message":"lease suspended; state snapshot kept (resume to restore)"}`.

### `POST /api/leases/{id}/resume` — start from the snapshot

Restores a suspended lease from `resume_build_id` **with the same sandbox
id**, so its address and identity are unchanged. Response
`{"id":"…","status":"running","address":"…"}`. `400` if not persistent,
`409` if the lease is busy (another lifecycle operation is in flight).
Resume is idempotent: there is no already-running check, so resuming a
lease that is already running restores from the snapshot again.

### `POST /api/leases/{id}/restart` — reboot

Persistent and running: suspend then resume (same lease, same build
chain, lossless through the pause build). Persistent and suspended:
resume. Non-persistent: delete the sandbox and create a fresh one from
the image's current build, keeping the lease id (its disk is lost —
there is no snapshot to restore). Response
`{"id":"…","status":"running","message":"lease restarted"}`.
`404` unknown, `409` when busy; a substrate
failure on the non-persistent path (which does create a sandbox)
surfaces as `500`, not `503` — unlike create, fork and clone, restart
does not map capacity errors to `503`.

### `POST /api/leases/{id}/checkpoint` — snapshot a running lease

Owner only, live leases only (`409` otherwise, including while another
operation is in flight). Writes a checkpoint build the lease can be
recovered from, and the lease keeps running from it. This is also what
the background checkpoint loop does for active persistent leases. Response:

```json
{"id":"…","build_id":"<uuid>","at":"2026-10-01T12:00:00Z"}
```

### `POST /api/leases/{id}/clone` — branch to a new lease

Checkpoints the running sandbox and grants a fresh **persistent** lease
from the checkpoint build, copying the source's network policy and
exposed ports. The optional body `{"tag":"…"}` is accepted and ignored —
the checkpoint build id is the snapshot's identity (use `tag` for a
friendly *name*). Response `201 Created`:

```json
{"id":"…","image":"…","source":"<source-id>","branch_tag":"<build id>","persistent":true,"expires_at":"…"}
```

### `POST /api/leases/{id}/fork` — N copies of a running lease

Owner only, as clone (shares are not honoured). Checkpoints the source
once and creates `count` leases from that build, each its own lease
owned by the caller, with the source's policy copied. Quota is reserved
for all of them up front (all or nothing); if any create fails, every
lease created in the call is deleted.

Request `{"count":1..20,"persistent":false,"ttl":300}` (`ttl` 0 = the
default TTL, capped at the maximum and the user's `max_ttl`).

Response `201 Created`: `{"source":"<id>","build_id":"<uuid>","ids":["…","…"]}`.
Errors: `400` bad count, `404` unknown, `409` suspended or busy, `429`
quota, `503` capacity.

### `POST /api/leases/{id}/network` — change egress policy live

Owner only. Updates the lease's policy and allowlist, re-applies the
egress config to the running sandbox, and refreshes every peer's
allowances — no restart, no new lease.

Request `{"network_policy":"none|lan|internet|restricted","egress_allowlist":[…]}`.
Response `200` `{"id","network_policy","egress_allowlist"}`. `400` on an
invalid policy, `404` unknown, `409` suspended.

### `POST /api/leases/{id}/tag` — friendly name

Request `{"name": "<unique-per-owner-name>"}`. Response
`{"id":"…","name":"…","ok":true}`. Names enable `ssh <name>@…` and
`GET /api/names/{name}`.

### `POST /api/leases/{id}/comment` — annotate

Request `{"comment": "…"}`. Response `{"id":"…","comment":"…","ok":true}`.

### `POST /api/leases/{id}/prompt` — message the in-sandbox Shelley agent

Request `{"message":"…","model":"gpt-oss-20b-fireworks"}` (model
optional). Polls the Shelley conversation API inside the sandbox and
returns the agent's reply. Requires the agent to be running (see the
`shelly` ctl verb). Response `200 OK` with
`{"id":"…","message":"…","reply":"…"}` (the lease id, the message sent, and
the agent's reply).

---

## Network policy

`network_policy` decides what may leave a lease; the substrate enforces
it, from the config carried on create and updated live by `/network`.
The default is **`restricted`** — a guest reaches only the host services
spoond grants it (the proxy/LLM gateway port and DNS) plus its
allowlist, and the peers that allowlist names through their published
ports. Under `lan` and `internet` there is no such gate: every other
live lease that publishes ports is reachable on those ports, whatever
the owner.

| Policy | Egress |
|---|---|
| `none` | nothing at all |
| `lan` | the LAN ranges (RFC 1918 minus the lease networks), host services, DNS, **every** exposing peer's published ports |
| `internet` | everything public, **plus** the LAN ranges, host services, DNS, **every** exposing peer's published ports |
| `restricted` *(default)* | host services, DNS, the allowlist, and the published ports of the peers the allowlist names |

`egress_allowlist` entries are IPs, CIDRs or domains. Entries that name
another lease — its id, its friendly name, or those prefixed `lease:` —
are peer references and permit that lease's published ports, not a
domain.

`lan` and `internet` guests may additionally reach the lease API itself
on the host service address (so a CI job can lease a database from inside
its sandbox); `restricted` and `none` may not.

Reserved guest ports: `49983` (envd, the guest agent) can never be
published or proxied.

---

## Images, health, snapshots

### `GET /api/images`

`{"images":["dev-base","go-base","py-base","elixir-base","elixir-release","llm-review","scylla"]}` —
the catalog entries with a current build. With `?detail=1`:

```json
{"images":[{"name":"py-base","build_id":"…","template_id":"…","digest":"…@sha256:…",
            "vcpu":2,"memory_mb":1024,"disk_mb":4096,"updated_at":"…"}]}
```

Images are built with `spoond images build` (see [ci-jobs.md](ci-jobs.md)).

### `GET /api/snapshots` — list your snapshot builds

The caller's builds (`kind` `pause` or `checkpoint`), excluding deleted
ones:

```json
{"snapshots":[{"build_id":"…","kind":"checkpoint","image":"dev-base",
               "parent_build_id":"…","size_bytes":123456,
               "created_at":"…","in_use":false}]}
```

### `DELETE /api/snapshots/{build_id}` — delete a snapshot build

Checks in order: `404` unknown or already deleted; `403` for template
builds (they belong to the image catalog); `404` for another owner's;
`409 {"error":"snapshot in use"}` while the GC's kept set references it;
otherwise the build's files are removed and the row marked deleted →
`204`.

### `GET /healthz`

No auth. `200 {"status":"ok","orchestrator":"<status>"}` when the
orchestrator answers, `503 {"status":"degraded","orchestrator":"unreachable"}`
when it does not — for Gatus/load balancers.

### `GET /metrics`

Prometheus output for the backend, plus the orchestrator's series when
`OTEL_PROM_URL` is set. Requires an admin user (identity-store mode) or
the scrape-only `METRICS_TOKEN`; `403` otherwise. The series are listed
in [operations.md](operations.md).

---

## Admin API

Routes under `/api/admin/*` are authenticated with
`Authorization: Bearer <ADMIN_TOKEN>` — a dedicated token, not a user or
consumer token. With no `ADMIN_TOKEN` configured they answer `404`; a
wrong or missing token answers `401`.

| Route | Effect |
|---|---|
| `POST /api/admin/drain` | Set the node draining and pause every live lease into a pause build (marking it drained), delete the warm pool, then wait up to 180 s until the node reports no running sandboxes and no outstanding work. Response `{"paused":N,"failed":[{"id","error"}],"pool_deleted":M,"quiesced":bool}`. `503 {"error":"orchestrator unreachable: …"}` (nothing changed) when the node cannot be reached. |
| `POST /api/admin/undrain` | Wait up to 120 s for the node, clear draining, resume exactly the drained leases (a lease that fails to resume becomes `lost`). Response `{"resumed":N,"failed":[…]}`. |
| `POST /api/admin/reconcile` | Run the crash reconciliation now. Response `{"recovered":N,"lost":M}`. |

These are what `spoond drain --stop|--start` calls from the orchestrator
unit's `ExecStop`/`ExecStartPost`; see [operations.md](operations.md)
before using them by hand.

---

## LLM gateway (per-lease)

`POST /llm/{lease-id}/openai/chat/completions` — OpenAI-compatible chat
completion against the configured upstream. The lease id in the path is
the capability; leases hold no consumer token. The route only exists
when `LLM_UPSTREAM_URL` is set: with no upstream the gateway is never
built, so `/llm/…` answers `404` (`spoond doctor` reports the same as a
WARN). There is no in-sandbox fallback.

Per-user key auth: when the lease owner has an LLM key configured,
requests must present it as `Authorization: Bearer <user-key>`.
Missing/wrong/foreign keys → `401`. Owners without a key keep the open
behavior — **unless** `LLM_OPEN_LEGACY` is unset (the default): with an
identity store present, keyless identity users are then denied outright
(`401`) and only legacy consumer-owned leases stay open. Set
`LLM_OPEN_LEGACY=1` to restore the pre-2.0 open behavior. The user key is
replaced by the server-side upstream key before forwarding, so it never
reaches the provider.

## Hive

The hive enlists a project's agent workers on this instance
(docs/plans/2026-10-02-swarm-controller.md, C10/C11). A project
describes itself in `.spoond/hive.yaml`; everything else (worker image,
network allowlist, credentials) is derived.

| Route | Auth | What |
|---|---|---|
| `GET /hive/guide` | none (LAN) | The enlistment guide for *this* instance, rendered live: real addresses, the image catalog, the hive.yaml schema, the derived-allowlist `needs:` keys, the routes, the checks and the next step. Markdown by default; `Accept: application/json` for the same content as JSON. This guide **is** the documentation — point an agent at it rather than at this file. |
| `POST /hive/check` | bearer token | Run every enlistment check against the body (a hive.yaml; `Content-Type: application/yaml` or `text/plain`). The report (text by default, JSON with `Accept: application/json`) is `200` whether or not checks fail — the first failure's remedy is the `Next:` line; `400` only for an unreadable body. The trial-lease check starts and deletes a real lease owned by the caller. |

Not automated yet (until enlistment lands): worker image build, deploy
key push, gates and budget report `skip` with the by-hand remedy.
The CLI `spoond hive check <file>` runs the same checks from anywhere;
steps that need the host report `skip` there and point at
`POST /hive/check`.

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

## HTTP proxy

The proxy listener (`PROXY_ADDR`, e.g. `0.0.0.0:8891` behind a wildcard
TLS front) routes hostnames to guest ports:

- `<lease-id>.sandbox.example` → guest port 3000
- `<lease-id>-<port>.sandbox.example` → guest `<port>`
- `<label>.<user>.sandbox.example` → forward-auth mode only, scoped to
  that user

In the default capability model only the unguessable 32-hex lease id
hostname resolves (friendly names are `404`, since they are guessable);
with `PROXY_AUTH_MODE=forward-auth` every request must carry the
`X-Proxy-Auth` shared secret and a `Remote-User` identity, and lookups
are owner-scoped. Guest port 49983 (envd) is refused with `403`.

Guest apps receive `Host: 127.0.0.1:5007` (the orchestrator's sandbox
proxy), so the public hostname is preserved in `X-Forwarded-Host` and the
scheme in `X-Forwarded-Proto`. Frameworks that validate the Host header —
dev servers with a host allowlist, for instance — must read
`X-Forwarded-Host`.

## Users & identity

The user store (`USERS_FILE`) makes people and agents first-class
identities. User endpoints are admin-gated except `/me`, `/by-name`,
`/by-key` and `/identity-status`; **`GET /api/users` is admin-only** (the
full directory is not enumerable by any token holder).

### `POST /api/users` — create a user

Bootstrap: when the store is empty, the first create is open (or gated by
`X-Bootstrap-Token: <BOOTSTRAP_TOKEN>` when configured) and the first
user becomes admin. After that, admin only.

```json
{
  "name": "jason",
  "kind": "person",
  "fingerprints": ["SHA256:AbC…", "SHA256:DeF…"],
  "token": "optional-bearer-token"
}
```

- `kind`: `person` | `agent` — agents are non-interactive identities
  (the MCP/ACP endpoints authenticate as their agent user).
- `fingerprints`: SSH public-key fingerprints (use `ssh-keygen -lf
  pubkey.pub`) — the gateway's `PublicKeyCallback` resolves these.
- `token`: optional per-user bearer token (like a `CONSUMER_TOKENS`
  entry, but bound to the identity).

Response `201 Created`: `{"user": {id, name, kind, admin, fingerprints,
max_leases, max_ttl, created_at}}` — the token hash and LLM key hash are
never exposed.

### `GET /api/users` — list users (admin only)

`403` for non-admin callers.

### `GET /api/users/me` — current user

Self-service: `{"user": {id, name, kind, admin, max_leases, max_ttl}}`.

### `GET /api/users/by-name/{name}` — minimal lookup

`{"user": {id, name}}` — the shape for share-granting and gateway name
resolution.

### `GET /api/users/by-key?fingerprint=…` — resolve SSH key

`{"user": {id, name}}` — the gateway calls this in its
`PublicKeyCallback`; minimal shape.

### `DELETE /api/users/{id}` — remove a user (admin only)

Removes the identity and its keys. **This is what actually revokes SSH
access** — the gateway treats the identity store as authoritative when
present, so removing the user invalidates all their keys immediately.

### `POST /api/users/{id}/quota` — set lease quota (admin only)

Request `{"max_leases": N, "max_ttl": S}` — concurrent-lease cap and
per-user TTL ceiling. `0` = unlimited/unset. Over-cap creates and forks
return `429`.

### `POST /api/users/{id}/llm-key` — set/rotate/revoke a user's LLM gateway key

Admin only. Request: `{"llm_key": "<slk-…>"}`; an empty `llm_key`
revokes (the owner's leases revert to the open gateway). The key is
stored as a salted hash and never returned in responses. `404` unknown
user; `403` non-admin.

### `GET /api/identity-status`

`{"identity_store": true|false}` — tells the SSH gateway whether the
backend has an identity store, so the gateway knows whether key
resolution must be authoritative. Requires a bearer token (the gateway
uses its service token).

## Shares

A lease owner can grant another user access to a lease for a limited
time — sharing a workspace with a collaborator or an agent without
copying the lease id/capability.

### `POST /api/leases/{id}/share` — grant (owner only)

Request: `{"grantee": "<user-id>", "mode": "ssh"|"http", "ttl": 3600}`.
`grantee` must be an existing user id (the gateway's `share add` verb
also accepts a user name and resolves it; this route does not); `ttl` in
seconds (0 = no expiry); `mode` (default `http`) selects which
operations the grantee may perform: `ssh` → `/endpoint`, `/prompt` and
SSH attach through the gateway; `http` → `/exec`, `/stream`, `/stat`,
`GET /api/leases/{id}`. The HTTP proxy does not consult shares: in
the capability model the lease id is the only credential, and under
forward-auth lookups are owner-scoped. `/stream` is an `http`
operation — an `ssh`-share grantee who calls it directly gets `404`
(the gateway's service token is the exception: the SSH gateway relays
session channels through `/stream`, so requests carrying it may attach
over an `ssh` share too). Response `201
Created` `{"shared":true,"lease_id":…,"grantee":…,"mode":…}`.
`400` unknown grantee; a non-owner gets `404 "lease not found"` —
the lookup is owner-scoped, so a lease you do not own looks the same as
a lease that does not exist.

There is no per-lease share listing. `GET /api/shares` lists every share
granted on the caller's leases — `{"shares": [{lease_id, grantee, mode,
created_at, expires_at?}]}` — which is what `share ls` prints.

### `DELETE /api/leases/{id}/share/{grantee}` — revoke (owner only)

`204 No Content` — the grantee loses access immediately. `404` if not shared.

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
  (trusted impersonation via `X-Spoond-User-Id`, gated on the gateway's
  service token).
- Persistent leases survive TTL sweeps; `keepalive` extends them;
  `IDLE_TIMEOUT_SECS` can auto-suspend idle persistent leases.
- All lease state, shares, the pool and the image catalog persist in
  SQLite: a backend restart loses none of it.
- The lease id doubles as a capability (e.g. `GET /api/…/endpoint` and
  the proxy hostname are how a caller addresses someone's lease).
