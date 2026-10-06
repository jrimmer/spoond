# Setup

This guide covers building spoond from source, wiring it to the E2B
substrate, and running the services. It assumes the host is already
installed per [install.md](install.md) — orchestrator up, registry up,
image catalog built.

## Architecture

```
                 ┌────────────────────── spoond ──────────────────────┐
  SSH :2222 ───▶ │ spoond-sshd-gateway        (dev sandboxes + ctl)   │
  HTTP :8891 ──▶ │   (proxy front, LLM gateway)                       │
                 │                                                    │
  HTTPS :8890 ─▶ │ spoond-backend ──gRPC──▶ e2b-orchestrator :5008    │
                 │   lease API, warm pool, TTL/idle sweeps            │
                 │                                                    │
  Forgejo ─────▶ │ spoond-runner (optional Forgejo Actions worker)    │
                 └────────────────────────────────────────────────────┘
```

The backend runs the control plane (leases, quotas, sharing, the catalog,
SQLite state) and talks to the E2B node orchestrator on loopback to
create, list, pause and checkpoint sandboxes. What the substrate is and
how that split works: [substrate.md](substrate.md).

> **Naming note.** Since 2.0 the service environment variables carry the
> `SPOOND_` prefix (`SPOOND_BACKEND_URL`, `SPOOND_AGENT_TOKEN`, …). The
> pre-2.0 `FORKD_` names still work everywhere but log a one-line
> deprecation warning; see the "Renamed in 2.0" table below.

## Renamed in 2.0

The pre-2.0 `FORKD_`-prefixed configuration variables were renamed to
`SPOOND_`. Each pair works as follows: the `SPOOND_` name is the
primary name; the old `FORKD_` name is still read as a fallback and
logs a one-line deprecation warning (once per variable per process).
When both are set, `SPOOND_` wins.

| 2.0 name | Pre-2.0 name (deprecated) | Used by |
|---|---|---|
| `SPOOND_BACKEND_URL` | `FORKD_BACKEND_URL` | `spoond mcp` — lease API base URL |
| `SPOOND_AGENT_TOKEN` | `FORKD_AGENT_TOKEN` | `spoond mcp` — per-agent bearer token |
| `SPOOND_IMAGE` | `FORKD_IMAGE` | `spoond mcp` — default image for new leases |
| `SPOOND_CTL_HOST` | `FORKD_CTL_HOST` | `spoondctl` — gateway host |
| `SPOOND_CTL_PORT` | `FORKD_CTL_PORT` | `spoondctl` — gateway SSH port |
| `SPOOND_CTL_KEY` | `FORKD_CTL_KEY` | `spoondctl` — SSH private key |
| `SPOOND_GATEWAY_HOST` | `FORKD_GATEWAY_HOST` | `spoond gateway` `--gateway-host` default |
| `SPOOND_NO_TMUX` | `FORKD_NO_TMUX` | guest image: skip the tmux auto-attach on SSH login |

Not renamed: the gateway's `forkd-*` SSH permission keys and the
`forkd_id` field on `GET /api/leases/{id}/endpoint` are stored/protocol
data, not configuration — renaming them would break clients.

One special case: `SPOOND_NO_TMUX` is read by the guest image's login
shell (`/etc/profile.d/spoond-tmux.sh`), not by a long-lived spoond
process. There the deprecation warning is emitted once per user (a
canary file in the home directory dedupes across logins) rather than
once per process.

## Prerequisites

- An installed E2B host: orchestrator healthy on `127.0.0.1:5008`,
  sandbox proxy on `127.0.0.1:5007`, registry on `127.0.0.1:5000`, and
  at least one image built into the catalog (see [install.md](install.md)).
- Go 1.27.1+ to build.
- Optional: a Caddy/nginx reverse proxy for TLS + hostname wildcards.

## Build

One binary, all services; exclude any module with Go build tags:

```bash
go build -o spoond ./cmd/spoond                       # all modules
go build -tags 'nobackend,nomcp,norunner' -o spoond ./cmd/spoond  # subset
```

Subcommands: `backend`, `gateway`, `mcp`, `runner`, `ctl`,
`images`, `drain`, `doctor`, `dash`, `notify`.
Exclusion tags (one per gated file — `cmd/spoond/main.go` prints this
list at `spoond help`): `nobackend`, `nogateway`, `nomcp`,
`norunner`, `noctl`, `noimages`, `nodoctor`, `nodrain`, `nodash`,
`nonotify`.

### Agent endpoint (`mcp`)

`spoond mcp` authenticates to the backend as a **per-agent user**:
leases it creates are owned by that agent's identity.

| Variable | Default | Purpose |
|---|---|---|
| `SPOOND_AGENT_TOKEN` | *(empty)* | per-agent bearer token for this endpoint, provisioned from the users store; **required** |
| `SPOOND_BACKEND_URL` | `https://127.0.0.1:8890` | lease API base URL |
| `SPOOND_IMAGE` | `dev-base` | default image for the leases it creates |

Create an agent user first (`POST /api/users` with `kind=agent` — it
needs a `token`, not an SSH key), then set `SPOOND_AGENT_TOKEN` to
that user's token. If it is not set, the endpoint fails fast
with provisioning instructions. The pre-2.0 `FORKD_*` names still work
(see "Renamed in 2.0" above).

## 1. spoond-backend (lease API)

### Environment

| Variable | Default | Purpose |
|---|---|---|
| `E2B_GRPC_ADDR` | `127.0.0.1:5008` | orchestrator gRPC address |
| `E2B_PROXY_URL` | `http://127.0.0.1:5007` | orchestrator sandbox proxy base URL |
| `E2B_TEAM_ID` | `5b0f4e3a-8c1d-4f2e-9a6b-7d3c2e1f0a95` | fixed team UUID sent with every gRPC request |
| `E2B_TOKEN_SEED_FILE` | `/etc/spoond/e2b-token-seed` | envd/traffic HMAC seed file (0600, ≥ 32 bytes; the backend exits without it) |
| `E2B_TEMPLATE_STORAGE_PATH` | `/forkdcache/e2b/storage/templates` | build store — where GC and disk accounting look |
| `IMAGE_REGISTRY` | `localhost:5000` | registry `spoond images build` pushes to |
| `CONSUMER_TOKENS` | *(required)* | comma-separated `token=consumer` pairs, e.g. `abc=forgejo,def=pi` — consumers authenticate with bearer tokens |
| `USERS_FILE` | *(empty)* | identity store path (JSON, chmod 600). Set for multi-user tenancy: per-user keys, tokens, quotas, sharing |
| `BOOTSTRAP_TOKEN` | *(empty)* | gates the first-user bootstrap when the store is empty; unset = legacy open first-create |
| `GATEWAY_TOKEN` | *(empty)* | SSH gateway's service token; lets the gateway call the backend as the authenticated SSH user (trusted impersonation) |
| `ADMIN_TOKEN` | *(empty)* | bearer token for `/api/admin/*` (drain, undrain, reconcile); unset = those routes answer `404` |
| `METRICS_TOKEN` | *(empty)* | scrape-only token for `/metrics`; refused on every other route |
| `EVENTS_TOKEN` | *(empty)* | events-only token for the lease event streams (every owner's events); refused on every other route — spoond dash's `DASH_EVENTS_TOKEN` |
| `BIND_ADDR` | `127.0.0.1:8890` | lease API listen address (`0.0.0.0:8890` behind a proxy) |
| `POOL_SIZE` | `0` | warm-pool size **per image with a current build**; pre-created sandboxes served without a cold restore. `0` disables |
| `SANDBOX_PROBE` | `1` | check each sandbox runs a healthy toolchain before pooling or leasing it; `0` disables (see [ci-jobs.md](ci-jobs.md)) |
| `SANDBOX_PROBE_TIMEOUT_SECS` | `20` | exec timeout for each integrity probe |
| `CHECKPOINT_INTERVAL_MINS` | `0` | default per-lease background checkpoint interval in minutes for leases without their own `checkpoint_interval` (`0` = never; a lease's own interval overrides) |
| `MAX_KEPT_PER_LEASE` | `4` | kept-checkpoint cap per lease (`0` = no cap): a keep on a lease at the cap answers `409` and takes nothing (#126) |
| `MAX_RUNNING_JOBS_PER_LEASE` | `16` | running background exec jobs per lease; past it a start answers `429` (#135) |
| `JOB_RETENTION_SECS` | `604800` | exited background-job records older than this (seconds) are pruned by the sweeper; running and lost records are kept (#135) |
| `KEPT_DISK_WARN_PCT` | `40` | kept-checkpoint share of the snapshot disk past which the notifier's `disk.kept` check warns and the dashboard's strip shows a row (`0` = off; #126) |
| `GC_DELETE` | `0` | `1` = the snapshot catalog GC actually deletes; default dry-run only logs candidates (see [operations.md](operations.md)) |
| `ORPHAN_REAP` | `dryrun` | what the GC does with orphan build directories: `off`, `dryrun` (log only), or `quarantine` (move aside, delete after `ORPHAN_QUARANTINE_SECS`; see [operations.md](operations.md)) |
| `ORPHAN_MIN_AGE_SECS` | `3600` | don't reap a build directory modified more recently than this (it may still be written; see [operations.md](operations.md)) |
| `ORPHAN_QUARANTINE_SECS` | `86400` | how long a `quarantine`-mode orphan waits before it may be deleted (see [operations.md](operations.md)) |
| `PROXY_ADDR` | *(empty)* | `0.0.0.0:8891` to serve the HTTP proxy/LLM gateway listener (Caddy wildcard fronts it) |
| `PROXY_AUTH_MODE` | `off` | `off` = capability model (lease id is the credential); `forward-auth` = require `X-Proxy-Auth` secret + `Remote-User` identity |
| `PROXY_AUTH_SECRET` | *(empty)* | shared secret for `forward-auth` mode (set by Caddy/IdP; never forwarded to guests) |
| `PROXY_AUTH_TRUSTED_PEERS` | *(empty)* | comma-separated CIDRs allowed to set `Remote-User` |
| `HOST_GUEST_SERVICE_ADDR` | *(empty)* | host address guests use to reach host services (the proxy/LLM gateway) |
| `HOST_GUEST_SERVICE_PORT` | `8891` | host TCP port granted to guests with the above |
| `SPOOND_GUEST_DNS_ADDR` | *(empty)* | guest DNS resolver: granted to every lease's egress policy on port 53 and baked into the guest image (empty = no resolver allowance) |
| `SPOOND_PROXY_HOST_SUFFIX` | `.sandbox.example.com` | wildcard hostname suffix the HTTP proxy routes (`<id>.<suffix>`, `<id>-<port>.<suffix>`) |
| `SPOOND_SSH_CONNECTION_ADDR` | `HOST_GUEST_SERVICE_ADDR` | gateway: server address reported in `SSH_CONNECTION` (single-host deploys need not set it) |
| `HOST_API_PORT` | `BIND_ADDR`'s port | lease API port `lan`/`internet` guests may reach on `HOST_GUEST_SERVICE_ADDR` (`0` = none) |
| `TLS_CERT` / `TLS_KEY` | *(empty)* | serve HTTPS on :8890 when both set |
| `DEFAULT_TTL_SECS` | `300` | default lease TTL for non-persistent sandboxes |
| `MAX_TTL_SECS` | `3600` | maximum TTL a consumer may request |
| `IDLE_TIMEOUT_SECS` | `0` | legacy plain-sweep auto-suspend: suspend a persistent lease idle for this long when its effective `idle_suspend` is `0` (`0` disables; new deployments should use `IDLE_SUSPEND_DEFAULT_SECS` and per-lease `idle_suspend`) |
| `IDLE_SUSPEND_DEFAULT_SECS` | `0` | default per-lease idle reclamation threshold for leases without their own `idle_suspend` (`0` = never; a lease's own `idle_suspend` overrides; #129 part 2) |
| `HELD_IDLE_TIMEOUT_SECS` | `14400` | suspend a held lease idle this long (held-lease rule 1; `0` disables) |
| `HELD_SUSPENDED_RELEASE_SECS` | `604800` | release a held lease a rule suspended once it stays untouched this long (rule 2; `0` disables) |
| `HOLD_TTL_SECS` | `604800` | how long a hold lasts from when it was set or renewed (rule 3; `0` means the default) |
| `HOLD_TTL_MAX_SECS` | `2592000` | cap for an explicit `hold_ttl` (`0` means the default) |
| `PRESSURE_DISK_FREE_PCT` | `15` | snapshot-disk free percentage under which rule 1 uses the shorter threshold (rule 4; `0` disables the disk trigger) |
| `PRESSURE_HELD_IDLE_SECS` | `1800` | rule 1's threshold under pressure (`0` disables rule 4) |
| `CRITICAL_DISK_FREE_PCT` | `5` | snapshot-disk free percentage under which rule 5 releases rule-suspended held leases (needs `GC_DELETE=1`; `0` disables) |
| `CRITICAL_DISK_RECOVER_PCT` | `10` | rule 5 stops releasing above this free percentage |
| `MAX_EXEC_TIMEOUT_SECS` | `300` | ceiling on one exec/stream call's `timeout` (raise it for compile-heavy CI steps) |
| `ASSETS_DIR` | *(empty)* | serve static assets (the shelley binary) to guests at `/assets/<file>` on the proxy listener |
| `LLM_UPSTREAM_URL` | *(empty)* | OpenAI-compatible LLM API base for the per-lease LLM gateway |
| `LLM_UPSTREAM_KEY` | *(empty)* | server-side key for that upstream (never sent into sandboxes) |
| `LLM_DEFAULT_MODEL` | *(empty)* | default model id for LLM gateway requests |
| `LLM_MODEL_MAP` | *(empty)* | optional `pattern=model` comma-separated map |
| `LLM_MAX_CONCURRENT_PER_USER` | `0` | in-flight `/llm/` requests per user before `429` (`0` = unlimited) |
| `LLM_OPEN_LEGACY` | *(empty)* | set to **any non-empty value** to keep keyless identity owners open on `/llm/` (the code tests for non-empty, so `1` is the conventional value); unset (the default) denies them |
| `OTEL_PROM_URL` | *(empty)* | fetch the orchestrator's Prometheus output here and append it to `/metrics` |
| `SPOOND_DB_PATH` | `/var/lib/spoond/spoond.db` | SQLite state: leases, shares, pool, image catalog |
| `SPOOND_BACKUP_DIR` | `/var/lib/spoond/backups` | daily `VACUUM INTO` backups, keep 7 |

### Run

```bash
export CONSUMER_TOKENS='abc=forgejo,def=pi'
export TLS_CERT=/etc/spoond/tls/fullchain.pem TLS_KEY=/etc/spoond/tls/privkey.pem
./spoond backend
```

### systemd unit

See `deploy/spoond-backend.service`. The shipped unit reads
`EnvironmentFile=-/etc/spoond-backend.env`; the install procedure
repoints it at `/etc/spoond/backend.env` (`chmod 600`) — that is the
file the backend sources and the operator snippets in
[install.md](install.md) and [operations.md](operations.md) read.

## 2. spoond-sshd-gateway (SSH + ctl plane)

### Flags

| Flag | Default | Purpose |
|---|---|---|
| `--listen` | `:2222` | SSH listen address |
| `--host-key` | `/etc/spoond-gateway/ssh_host_ed25519_key` | SSH host key (generated if missing) |
| `--backend` | `https://127.0.0.1:8890` | spoond-backend base URL |
| `--backend-token` | *(required)*; env `SPOOND_GATEWAY_TOKEN` | spoond-backend service token (`GATEWAY_TOKEN`) — admin-equivalent, so it is read from the env file, never `ExecStart` |
| `--client-keys` | *(empty)* | comma-separated paths to authorized client public keys, **or a directory scanned for `*.pub` files** (legacy mode only) |
| `--gateway-key` | `/etc/spoond-gateway/gateway_ed25519` | gateway identity key (kept for unit compatibility; the gateway no longer connects into sandboxes with it) |
| `--gateway-host` | `sandbox.example.com` (env `SPOOND_GATEWAY_HOST`) | public hostname advertised in MOTDs |
| `--shelly-binary-url` | env `SHELLY_BINARY_URL` (`http://HOST_GUEST_SERVICE_ADDR:HOST_GUEST_SERVICE_PORT/assets/shelley`) | URL the sandbox fetches the shelley agent binary from |
| `--llm-gateway-url` | env `LLM_GATEWAY_URL` (`http://HOST_GUEST_SERVICE_ADDR:HOST_GUEST_SERVICE_PORT/llm/`) | base URL of the per-lease LLM gateway the shelley agent is pointed at |
| `--shelly-model` | `gpt-oss-20b-fireworks` | default model id written into shelley.json |
| `--ssh-images` | env `GATEWAY_SSH_IMAGES` = `dev-base` | images that may serve interactive `ssh <id>@` sessions |
| `--metrics-listen` | env `GATEWAY_METRICS_LISTEN` *(empty = off)* | address for the gateway's own `/metrics` |
| `--image-aliases` | env `GATEWAY_IMAGE_ALIASES` *(empty)* | extra `short=full` image aliases for `ssh new-<short>@` |
| `--bootstrap-token` | *(ignored)* | **deprecated**: accepted for unit compatibility; bootstrap via direct backend call only |

### Key model

- **Identity store (recommended):** when the backend has `USERS_FILE`
  set, the store is the **single source of truth** for SSH keys. The
  gateway probes `GET /api/identity-status` at startup; in store mode
  every connecting key must resolve to a user (the local `--client-keys`
  allowlist is ignored), and deleting the user (`DELETE /api/users/{id}`)
  revokes access immediately. Create users via the `ssh-key` ctl verb or
  `POST /api/users` — see [api.md](api.md#users--identity).
- **Legacy mode (no identity store):** `--client-keys` lists the keys
  allowed to connect (as `ctl`, `new-*`, or `<lease-id>` usernames).
  Each user key must also be present in the sandbox images'
  `authorized_keys` if you want the gateway to connect into sandboxes on
  your behalf.
- `--gateway-key` is the gateway's own identity; its public half is in
  the image's `authorized_keys` via the image Dockerfile.

SSH sessions are relayed through the backend's `/stream` endpoint
(WebSocket) into the sandbox's PTY, so the gateway needs no network path
into any sandbox — see [substrate.md](substrate.md).

### First-user bootstrap

On a fresh `USERS_FILE`, the first `POST /api/users` (via `ssh-key add`
or curl) creates the **admin** user. If `BOOTSTRAP_TOKEN` is set, that
create must present `X-Bootstrap-Token: <token>` — bootstrap is an
operator action done directly against the backend (the gateway
deliberately does not forward the token):

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
  --backend-token abc
```

## 3. spoond-runner (Forgejo Actions, optional)

Environment-driven; registers as a Forgejo Actions runner and leases
sandboxes as CI workers. Key variables (see `cmd/spoond-runner/main.go`
for the full reference):

| Variable | Purpose |
|---|---|
| `FORGEJO_URL`, `RUNNER_TOKEN` | Forgejo instance and registration token (required) |
| `LEASE_URL` / `LEASE_TOKEN` | backend base URL and the bearer token the runner authenticates with |
| `IMAGE_MAP` / `DEFAULT_IMAGE` | `runs-on` label → image mapping ([ci-jobs.md](ci-jobs.md)) |
| `LEASE_TTL` | sandbox lease TTL seconds (default 600) |
| `EXEC_TIMEOUT_SECS` | per-step exec timeout override (0 = the backend's `MAX_EXEC_TIMEOUT_SECS`, default 300) |
| `LEASE_NETPOL` | egress policy the runner's leases get (`none` \| `lan` \| `internet` \| `restricted`; default `internet`) |
| `LEASE_NET_ALLOW` | comma-separated allowlist entries added on top of the policy |
| `RUNNER_FLOOR` / `RUNNER_MAX` / `RUNNER_SCALE_STEP` / `SCALE_UP_DELAY` / `SCALE_DOWN_DELAY` | registered-runner pool: floor, cap, step and scale delays |
| `RUNNER_STATE_FILE` | persists runner UUIDs across restarts (default `/var/lib/spoond/runner-state.json`) |
| `REPO_BASE_URL` | git host base URL for `actions/checkout` clones (no default; checkout fails without it) |
| `JOB_RECORD_DIR` | failed-job JSON records (default `/var/lib/spoond/jobs`) |

## 4. Reverse proxy (TLS)

The gateway and proxy speak HTTP on loopback; put a TLS-terminating
reverse proxy in front with a wildcard cert so sandbox URLs work:

- `sandbox.example.com` → gateway :2222 (SSH, keep port 2222 or use a
  non-22 port per the MOTD hints)
- `*.sandbox.example.com` → proxy :8891 (each lease gets
  `<lease-id>.sandbox.example.com` and `<id>-<port>.sandbox.example.com`)

See `deploy/caddy-sandbox-forwardauth.conf` for the staged Caddy snippet
that also fronts the proxy with a forward-auth (Authelia) block — the
reference for the `PROXY_AUTH_MODE=forward-auth` setup in
[security.md](security.md).

## Verify

```bash
# Backend health (no auth)
curl -s https://127.0.0.1:8890/healthz

# Create a lease
curl -s -X POST https://127.0.0.1:8890/api/leases \
  -H "Authorization: Bearer abc" -H 'Content-Type: application/json' \
  -d '{"image":"dev-base","ttl":300}'

# SSH into it
ssh <lease-id>@sandbox.example.com

# Control plane
ssh ctl@sandbox.example.com "ls"
```

`spoond doctor` (see [install.md](install.md#7-verify) and
[operations.md](operations.md)) checks the whole surface end to end.
