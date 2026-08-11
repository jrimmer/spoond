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

## 1. forkd-backend (lease API)

### Environment

| Variable | Default | Purpose |
|---|---|---|
| `FORKD_URL` | `http://127.0.0.1:8889` | forkd-controller base URL |
| `FORKD_TOKEN` | *(empty)* | controller auth token, if the controller requires one |
| `CONSUMER_TOKENS` | *(required)* | comma-separated `token=consumer` pairs, e.g. `abc=forgejo,def=pi` — consumers authenticate with bearer tokens |
| `POOL_SIZE` | `0` | warm-pool size **per image**; pre-forked sandboxes served in milliseconds. `0` disables |
| `KNOWN_IMAGES` | *(all)* | comma-separated allowlist of image tags that may be granted (e.g. `dev-base,go-base`) |
| `PROXY_ADDR` | *(empty)* | `0.0.0.0:8891` to serve the HTTP proxy/LLM gateway listener (Caddy wildcard fronts it) |
| `TLS_CERT` / `TLS_KEY` | *(empty)* | serve HTTPS on :8890 when both set |
| `DEFAULT_TTL_SECS` | `300` | default lease TTL for non-persistent sandboxes |
| `MAX_TTL_SECS` | `3600` | maximum TTL a consumer may request |
| `IDLE_TIMEOUT_SECS` | `0` | auto-suspend persistent leases idle for this long (`0` disables) |
| `NETPOL_DNS` | *(system)* | DNS server IPs used by network policies |
| `LLM_UPSTREAM_URL` | *(empty)* | OpenAI-compatible LLM API base for the per-lease LLM gateway |
| `LLM_UPSTREAM_KEY` | *(empty)* | server-side key for that upstream (never sent into sandboxes) |
| `LLM_DEFAULT_MODEL` | *(empty)* | default model id for LLM gateway requests |
| `LLM_MODEL_MAP` | *(empty)* | optional `pattern=model` comma-separated map |

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

### Key model

- `--client-keys` lists the **user** keys allowed to connect (as `ctl`,
  `new-*`, or `<lease-id>` usernames). Each user key must also be
  present in the sandbox images' `authorized_keys` if you want the
  gateway to connect into sandboxes on your behalf.
- `--gateway-key` is the gateway's own identity; its public half is
  baked into the sandbox image (`dev-base`).

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
