# Setup

This guide covers building and running the spoond services: the backend
(lease API), the SSH gateway and the optional Forgejo Actions runner. The
sandbox substrate is **E2B's orchestrator** — see
[install.md](install.md) for bringing up the host and
[substrate.md](substrate.md) for how spoond uses E2B.

## Architecture

```
                 ┌────────────────────── spoond ──────────────────────┐
  SSH :2222 ───▶ │ spoond-sshd-gateway                                 │
  HTTP :8891 ──▶ │   (proxy front, LLM gateway; relays to envd)       │
                 │                                                    │
  HTTPS :8890 ─▶ │ spoond-backend ──▶ e2b-orchestrator (gRPC :5008)   │
                 │   lease API, warm pool, TTL/idle sweeps, SQLite    │
                 │                                                    │
  Forgejo ─────▶ │ spoond-runner (optional Forgejo Actions worker)    │
                 └────────────────────────────────────────────────────┘
```

## Prerequisites

- A host brought up by `deploy/e2b/host-setup.sh` plus the remaining
  steps of
  [U04](plans/2026-09-30-e2b-substrate/U04-host-bringup.md): the
  `e2b-orchestrator.service` unit, its env file, the token seed, the
  `flags.json` override and the OpenTelemetry collector
- Go 1.27.1 to build
- Optional: a Caddy/nginx reverse proxy for TLS + hostname wildcards

## Build

One binary, all services; exclude any module with Go build tags:

```bash
go build -o spoond ./cmd/spoond                       # all modules
go build -tags 'nobackend,nomcp,norunner' -o spoond ./cmd/spoond  # subset
```

Subcommands: `backend`, `gateway`, `acp`, `mcp`, `runner`, `ctl`,
`images`, `drain`, `dash`, `doctor`. Exclusion tags: `nobackend`,
`nogateway`, `noacp`, `nomcp`, `norunner`, `noctl`, `noimages`.

### Agent endpoints (`mcp` / `acp`)

Both endpoints authenticate to the backend as a **per-agent user** (epic
#26 U4): leases they create are owned by that agent's identity.

| Variable | Default | Purpose |
|---|---|---|
| `FORKD_AGENT_TOKEN` | *(required)* | per-agent bearer token for this endpoint, provisioned from the users store |
| `FORKD_BACKEND_URL` | `https://127.0.0.1:8890` | lease API base URL |
| `FORKD_IMAGE` | `dev-base` | image the endpoint leases |
| `FORKD_LLM_MODEL` | `gpt-oss-20b-fireworks` | default model id for the agent loop (`acp` only) |

Create an agent user first (`ssh-key add <pubkey> <name>` or
`POST /api/users` with `kind=agent`), then set `FORKD_AGENT_TOKEN` to
that user's token. If it is not set, the endpoint fails fast with
provisioning instructions.

## 1. spoond-backend (lease API)

### Environment

| Variable | Default | Purpose |
|---|---|---|
| `CONSUMER_TOKENS` | *(required)* | comma-separated `token=consumer` pairs, e.g. `abc=forgejo,def=pi` — consumers authenticate with bearer tokens |
| `E2B_GRPC_ADDR` | `127.0.0.1:5008` | orchestrator gRPC address |
| `E2B_PROXY_URL` | `http://127.0.0.1:5007` | orchestrator sandbox proxy (envd) |
| `E2B_TOKEN_SEED_FILE` | `/etc/spoond/e2b-token-seed` | envd/traffic HMAC seed file (64 hex chars, mode 0600) |
| `E2B_TEAM_ID` | *(fixed UUID)* | team UUID sent on every substrate request |
| `E2B_TEMPLATE_STORAGE_PATH` | `/forkdcache/e2b/storage/templates` | build storage root, for disk accounting |
| `SPOOND_DB_PATH` | `/var/lib/spoond/spoond.db` | SQLite catalog/lease database |
| `BIND_ADDR` | `127.0.0.1:8890` | lease API listen address |
| `PROXY_ADDR` | *(empty)* | public proxy/LLM gateway listener (`0.0.0.0:8891`; Caddy wildcard fronts it) |
| `HOST_GUEST_SERVICE_ADDR` | *(required)* | address guests use to reach host services (the host's primary IP) |
| `HOST_GUEST_SERVICE_PORT` | `8891` | host port guests use for the proxy/LLM gateway |
| `HOST_API_PORT` | `BIND_ADDR`'s port | lease API port `lan`/`internet` guests may reach |
| `POOL_SIZE` | `0` | warm-pool size **per image**; pre-started sandboxes served in milliseconds. `0` disables |
| `SANDBOX_PROBE` | `1` | check each sandbox runs a healthy toolchain before pooling or leasing it; `0` disables (see `docs/ci-jobs.md`) |
| `SANDBOX_PROBE_TIMEOUT_SECS` | `20` | exec timeout for each integrity probe |
| `DEFAULT_TTL_SECS` | `300` | default lease TTL for non-persistent sandboxes |
| `MAX_TTL_SECS` | `3600` | maximum TTL a consumer may request |
| `IDLE_TIMEOUT_SECS` | `0` | auto-suspend persistent leases idle for this long (`0` disables) |
| `CHECKPOINT_INTERVAL_MINS` | `60` | periodic checkpoint of persistent leases (U10) |
| `TLS_CERT` / `TLS_KEY` | *(empty)* | serve HTTPS on :8890 when both set |
| `USERS_FILE` | *(empty)* | identity store path (JSON, chmod 600). Set for multi-user tenancy (v1.1): per-user keys, tokens, quotas, sharing |
| `BOOTSTRAP_TOKEN` | *(empty)* | gates the first-user bootstrap when the store is empty (security review #37 H3/M4); unset = legacy open first-create |
| `GATEWAY_TOKEN` | *(empty)* | SSH gateway's service token; lets the gateway call the backend as the authenticated SSH user (trusted impersonation, epic #26 U6) |
| `ADMIN_TOKEN` | *(empty)* | bearer token for `/api/admin/*` (empty disables the routes) |
| `METRICS_TOKEN` | *(empty)* | scrape-only bearer for `/metrics` and nothing else (Prometheus, `spoond dash`) |
| `GC_DELETE` | *(empty)* | allows the catalog GC to delete builds; unset = log only (U11) |
| `SPOOND_BACKUP_DIR` | `/var/lib/spoond/backups` | daily `VACUUM INTO` backup target (U11) |
| `PROXY_AUTH_MODE` | `off` | `off` = capability model (lease id is the credential); `forward-auth` = require `X-Proxy-Auth` secret + `Remote-User` identity (epic #26 U7) |
| `PROXY_AUTH_SECRET` | *(empty)* | shared secret for `forward-auth` mode (set by Caddy/IdP; never forwarded to guests) |
| `PROXY_AUTH_TRUSTED_PEERS` | *(empty)* | comma-separated CIDRs allowed to set `Remote-User` (security review #37 M3) |
| `LLM_UPSTREAM_URL` | *(empty)* | OpenAI-compatible LLM API base for the per-lease LLM gateway |
| `LLM_UPSTREAM_KEY` | *(empty)* | server-side key for that upstream (never sent into sandboxes) |
| `LLM_DEFAULT_MODEL` | *(empty)* | default model id for LLM gateway requests |
| `LLM_MODEL_MAP` | *(empty)* | optional `pattern=model` comma-separated map |
| `LLM_MAX_CONCURRENT_PER_USER` | `0` | in-flight `/llm/` requests per user before `429` (`0` = unlimited) |
| `LLM_OPEN_LEGACY` | *(empty)* | `1` = keyless owners keep open `/llm/`; unset = deny keyless identity users (security review #37 C2) |

### Run

```bash
export CONSUMER_TOKENS='abc=forgejo,def=pi'
export HOST_GUEST_SERVICE_ADDR=10.1.0.11
./spoond backend
```

### systemd unit

See `deploy/spoond-backend.service`; the unit sources
`/etc/spoond/backend.env` (`chmod 600`) and runs as root (the drain
hooks and template storage require it).

## 2. spoond-sshd-gateway (SSH + ctl plane)

### Flags

| Flag | Default | Purpose |
|---|---|---|
| `--listen` | `:2222` | SSH listen address |
| `--host-key` | `/etc/spoond-gateway/ssh_host_ed25519_key` | SSH host key (generated if missing) |
| `--backend` | `https://127.0.0.1:8890` | spoond-backend base URL |
| `--backend-token` | *(required)* | spoond-backend consumer token (or `$SPOOND_GATEWAY_TOKEN`) |
| `--client-keys` | *(empty)* | comma-separated paths to authorized client public keys, **or a directory scanned for `*.pub` files** |
| `--gateway-key` | `/etc/spoond-gateway/gateway_ed25519` | gateway identity for nested connections into sandboxes |
| `--gateway-host` | `sandbox.lacy.casa` (env `FORKD_GATEWAY_HOST`) | public hostname advertised in MOTDs |
| `--shelly-binary-url` | env `SHELLY_BINARY_URL` | URL the sandbox fetches the shelley agent binary from |
| `--llm-gateway-url` | env `LLM_GATEWAY_URL` | base URL of the per-lease LLM gateway |
| `--shelly-model` | `gpt-oss-20b-fireworks` | default model id for the shelley agent |
| `--ssh-images` | `dev-base` (env `GATEWAY_SSH_IMAGES`) | comma-separated image tags that support interactive SSH (have sshd) |
| `--bootstrap-token` | *(ignored)* | **deprecated** (security review #37 rescan F7): accepted for unit compatibility; bootstrap via direct backend call only |

### Key model

- **Identity store (v1.1, recommended):** when the backend has
  `USERS_FILE` set, the store is the **single source of truth** for SSH
  keys (security review #37 H1). The gateway probes
  `GET /api/identity-status` at startup; in store mode every connecting
  key must resolve to a user (the local `--client-keys` allowlist is
  ignored), and deleting the user (`DELETE /api/users/{id}`) revokes
  access immediately. Create users via the `ssh-key` ctl verb or
  `POST /api/users` — see [api.md](api.md#users--identity-epic-26).
- **Legacy mode (no identity store):** `--client-keys` lists the keys
  allowed to connect (as `ctl`, `new-*`, or `<lease-id>` usernames).
  Each user key must also be present in the sandbox images'
  `authorized_keys` if you want the gateway to connect into sandboxes on
  your behalf.
- `--gateway-key` is the gateway's own identity; its public half is
  baked into the sandbox image (`dev-base`).

### First-user bootstrap (v1.1)

On a fresh `USERS_FILE`, the first `POST /api/users` (via
`ssh-key add` or curl) creates the **admin** user. If
`BOOTSTRAP_TOKEN` is set, that create must present
`X-Bootstrap-Token: <token>` — bootstrap is an operator action done
directly against the backend (the gateway deliberately does not forward
the token; security review #37 rescan F7):

```bash
curl -s -X POST https://127.0.0.1:8890/api/users \
  -H "Authorization: Bearer <consumer-token>" \
  -H "X-Bootstrap-Token: <BOOTSTRAP_TOKEN>" \
  -H 'Content-Type: application/json' \
  -d '{"name":"jason","kind":"person","fingerprints":["SHA256:…"]}'
```

### Run

```bash
./spoond gateway --backend https://127.0.0.1:8890 \
  --backend-token abc \
  --client-keys /etc/spoond-gateway/keys   # dir scan: add user = drop .pub + restart
```

## 3. spoond-runner (Forgejo Actions, optional)

Environment-driven; registers as a Forgejo Actions runner and leases
sandboxes adaptively as CI workers. See `cmd/spoond-runner/main.go` for
the env reference (`FORGEJO_URL`, `RUNNER_TOKEN`, `RUNNER_NAME`,
`RUNNER_LABELS`, `LEASE_URL`, `LEASE_TOKEN`, `IMAGE_MAP`,
`DEFAULT_IMAGE`, `RUNNER_FLOOR`/`RUNNER_MAX`/`RUNNER_SCALE_STEP`,
`SCALE_UP_DELAY`/`SCALE_DOWN_DELAY`, `EXEC_TIMEOUT_SECS`,
`JOB_RECORD_DIR`, `RUNNER_STATE_FILE`).

## 4. Reverse proxy (TLS)

The gateway and proxy speak HTTP on loopback; put a TLS-terminating
reverse proxy in front with a wildcard cert so sandbox URLs work:

- `sandbox.example.com` → gateway :2222 (SSH, keep port 2222 or use a
  non-22 port per the MOTD hints)
- `*.sandbox.example.com` → proxy :8891 (each lease gets
  `<lease-id>.sandbox.example.com` and `<id>-<port>.sandbox.example.com`)

See `deploy/caddy-sandbox-forwardauth.conf` for the forward-auth block
(epic #26 T7/#34).

## Verify

```bash
# Backend health (no auth)
curl -s https://127.0.0.1:8890/healthz

# Full install check
spoond doctor

# Create a lease
curl -s -X POST https://127.0.0.1:8890/api/sandboxes \
  -H "Authorization: Bearer abc" -H 'Content-Type: application/json' \
  -d '{"image":"dev-base","ttl":300}'

# SSH into it
ssh <lease-id>@sandbox.example.com

# Control plane
ssh ctl@sandbox.example.com "ls"
```
