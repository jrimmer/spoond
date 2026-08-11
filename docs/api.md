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
| `network_policy` | string | `lan` | `none` \| `lan` \| `internet` \| `restricted` |
| `egress_allowlist` | []string | *(none)* | required when `network_policy=restricted`; IPs/CIDRs/domains |
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

Proxies forkd-controller's Prometheus metrics (unauthenticated;
loopback listener only).

---

## LLM gateway (per-lease, no bearer token)

`POST /llm/{lease-id}/openai/chat/completions` — OpenAI-compatible chat
completion against the lease's sandbox-hosted LLM gateway. The lease id
in the path is the capability; sandboxes hold no consumer token. When no
`LLM_UPSTREAM_URL` is configured the gateway forwards to the sandbox's
own `127.0.0.1:9000` Shelley agent instead.

---

## Auth & ownership model

- Every request is authenticated by consumer token → consumer id.
- Leases belong to the consumer that created them; operations check
  `owner == lease.Owner`.
- Persistent leases survive TTL sweeps; `keepalive` extends them;
  `IDLE_TIMEOUT_SECS` can auto-suspend idle persistent leases.
- The lease id doubles as a capability (e.g. `GET /api/…/endpoint` is
  how the gateway resolves `ssh <id>@…`).
