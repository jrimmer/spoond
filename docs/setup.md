# Setup

This guide covers installing spoond from source, wiring it to a forkd
controller, and running the three services. It assumes you already have
a working **forkd controller** (the microVM runtime this project leases
sandboxes from) — see [deeplethe/forkd](https://github.com/deeplethe/forkd).

## Architecture

```
                 ┌────────────────────── spoond ──────────────────────┐
  SSH :2222 ───▶ │ forkd-sshd-gateway                                 │
  HTTP :8891 ──▶ │   (ctl plane, proxy front, LLM gateway)            │
                 │                                                    │
  HTTPS :8890 ─▶ │ forkd-backend ──▶ forkd-controller (forkd repo)    │
                 │   lease API, warm pool, TTL/idle sweeps            │
                 │                                                    │
  Forgejo ─────▶ │ forkd-runner (optional Forgejo Actions worker)     │
                 └────────────────────────────────────────────────────┘
```

## Prerequisites

- Linux host (tested on Debian-based) with KVM and a forkd controller
  running on `127.0.0.1:8889` (plain HTTP)
- Go 1.25+ to build
- Optional: a Caddy/nginx reverse proxy for TLS + hostname wildcards

## Build

One binary, all services; exclude any module with Go build tags:

```bash
go build -o spoond ./cmd/spoond                       # all modules
go build -tags 'nobackend,nomcp,norunner' -o spoond ./cmd/spoond  # subset
```

Subcommands: `backend`, `gateway`, `acp`, `mcp`, `runner`, `ctl`.
Exclusion tags: `nobackend`, `nogateway`, `noacp`, `nomcp`, `norunner`,
`noctl`.

### Agent endpoints (`mcp` / `acp`)

Both endpoints authenticate to the backend as a **per-agent user** (epic
#26 U4): leases they create are owned by that agent's identity.

| Variable | Default | Purpose |
|---|---|---|
| `FORKD_AGENT_TOKEN` | *(empty)* | per-agent bearer token for this endpoint, provisioned from the users store; wins over `FORKD_TOKEN` |
| `FORKD_TOKEN` | *(empty)* | legacy fallback (deprecated): used with a warning when `FORKD_AGENT_TOKEN` is unset |

Create an agent user first (`ssh-key add <pubkey> <name>` or
`POST /api/users` with `kind=agent`), then set `FORKD_AGENT_TOKEN` to
that user's token. If neither variable is set, the endpoint fails fast
with provisioning instructions.

## 1. forkd-backend (lease API)

### Environment

| Variable | Default | Purpose |
|---|---|---|
| `FORKD_URL` | `http://127.0.0.1:8889` | forkd-controller base URL |
| `FORKD_TOKEN` | *(empty)* | controller auth token, if the controller requires one |
| `CONSUMER_TOKENS` | *(required)* | comma-separated `token=consumer` pairs, e.g. `abc=forgejo,def=pi` — consumers authenticate with bearer tokens |
| `USERS_FILE` | *(empty)* | identity store path (JSON, chmod 600). Set for multi-user tenancy (v1.1): per-user keys, tokens, quotas, sharing |
| `BOOTSTRAP_TOKEN` | *(empty)* | gates the first-user bootstrap when the store is empty (security review #37 H3/M4); unset = legacy open first-create |
| `GATEWAY_TOKEN` | *(empty)* | SSH gateway's service token; lets the gateway call the backend as the authenticated SSH user (trusted impersonation, epic #26 U6) |
| `POOL_SIZE` | `0` | warm-pool size **per image**; pre-forked sandboxes served in milliseconds. `0` disables |
| `KNOWN_IMAGES` | *(all)* | comma-separated allowlist of image tags that may be granted (e.g. `dev-base,go-base`) |
| `PROXY_ADDR` | *(empty)* | `0.0.0.0:8891` to serve the HTTP proxy/LLM gateway listener (Caddy wildcard fronts it) |
| `PROXY_AUTH_MODE` | `off` | `off` = capability model (lease id is the credential); `forward-auth` = require `X-Proxy-Auth` secret + `Remote-User` identity (epic #26 U7) |
| `PROXY_AUTH_SECRET` | *(empty)* | shared secret for `forward-auth` mode (set by Caddy/IdP; never forwarded to guests) |
| `PROXY_AUTH_TRUSTED_PEERS` | *(empty)* | comma-separated CIDRs allowed to set `Remote-User` (security review #37 M3) |
| `TLS_CERT` / `TLS_KEY` | *(empty)* | serve HTTPS on :8890 when both set |
| `DEFAULT_TTL_SECS` | `300` | default lease TTL for non-persistent sandboxes |
| `MAX_TTL_SECS` | `3600` | maximum TTL a consumer may request |
| `IDLE_TIMEOUT_SECS` | `0` | auto-suspend persistent leases idle for this long (`0` disables) |
| `NETPOL_DNS` | *(system)* | DNS server IPs used by network policies |
| `LLM_UPSTREAM_URL` | *(empty)* | OpenAI-compatible LLM API base for the per-lease LLM gateway |
| `LLM_UPSTREAM_KEY` | *(empty)* | server-side key for that upstream (never sent into sandboxes) |
| `LLM_DEFAULT_MODEL` | *(empty)* | default model id for LLM gateway requests |
| `LLM_MODEL_MAP` | *(empty)* | optional `pattern=model` comma-separated map |
| `LLM_MAX_CONCURRENT_PER_USER` | `0` | in-flight `/llm/` requests per user before `429` (`0` = unlimited) |
| `LLM_OPEN_LEGACY` | `1` | `1` = keyless owners keep open `/llm/`; `0` = deny keyless identity users (security review #37 C2) |

### Run

```bash
export CONSUMER_TOKENS='abc=forgejo,def=pi'
export POOL_SIZE=3
export TLS_CERT=/etc/spoond/tls/fullchain.pem TLS_KEY=/etc/spoond/tls/privkey.pem
./spoond backend
```

### systemd unit

See `deploy/spoond-backend.service`; the unit sources `/etc/forkd-backend.env`
(`chmod 600`) and runs with `User=forkd`.

## 2. forkd-sshd-gateway (SSH + ctl plane)

### Flags

| Flag | Default | Purpose |
|---|---|---|
| `--listen` | `:2222` | SSH listen address |
| `--host-key` | `/etc/forkd-gateway/ssh_host_ed25519_key` | SSH host key (generated if missing) |
| `--backend` | `https://127.0.0.1:8890` | forkd-backend base URL |
| `--backend-token` | *(required)* | forkd-backend consumer token |
| `--client-keys` | *(empty)* | comma-separated paths to authorized client public keys, **or a directory scanned for `*.pub` files** |
| `--gateway-key` | `/etc/forkd-gateway/gateway_ed25519` | gateway identity for nested connections into sandboxes |
| `--gateway-host` | `sandbox.lacy.casa` (env `FORKD_GATEWAY_HOST`) | public hostname advertised in MOTDs |
| `--shelly-binary-url` | env `SHELLY_BINARY_URL` | URL the sandbox fetches the shelley agent binary from |
| `--llm-gateway-url` | env `LLM_GATEWAY_URL` | base URL of the per-lease LLM gateway |
| `--shelly-model` | `gpt-oss-20b-fireworks` | default model id for the shelley agent |
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

## 3. forkd-runner (Forgejo Actions, optional)

Environment-driven; registers as a Forgejo Actions runner and leases
sandboxes adaptively as CI workers. See `cmd/forkd-runner/main.go` for
the env reference (`FORGEJO_*`, `FORKD_URL`, `CONSUMER_TOKEN`).

## 4. Reverse proxy (TLS)

The gateway and proxy speak HTTP on loopback; put a TLS-terminating
reverse proxy in front with a wildcard cert so sandbox URLs work:

- `sandbox.example.com` → gateway :2222 (SSH, keep port 2222 or use a
  non-22 port per the MOTD hints)
- `*.sandbox.example.com` → proxy :8891 (each lease gets
  `<lease-id>.sandbox.example.com` and `<id>-<port>.sandbox.example.com`)

See `deploy/caddy/` for the reference Caddyfile.

## Verify

```bash
# Backend health (no auth)
curl -s https://127.0.0.1:8890/healthz

# Create a lease
curl -s -X POST https://127.0.0.1:8890/api/sandboxes \
  -H "Authorization: Bearer abc" -H 'Content-Type: application/json' \
  -d '{"image":"dev-base","ttl":300}'

# SSH into it
ssh <lease-id>@sandbox.example.com

# Control plane
ssh ctl@sandbox.example.com "ls"
```
