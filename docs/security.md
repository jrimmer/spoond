# Security model & hardening notes

The platform boundary is unchanged in spirit — one tenant per microVM, a
lease API that authenticates callers, unguessable ids as capabilities —
but the substrate underneath it is new, so this page covers both: the
E2B-specific posture first, then the tenant boundaries, then the state of
the adversarial review findings (issue #37) after the fix pass and the
independent clean-room rescan.

## The substrate's posture

**Firecracker runs as root, without the jailer.** E2B's orchestrator is a
single root process that supervises every microVM on the host; it does
not use Firecracker's jailer, and it needs `CAP_SYS_ADMIN`-class
privileges for its own machinery (NBD devices, network namespaces, userfaultfd
for memory snapshots). Compensating controls:

- **Every sandbox is a Firecracker microVM.** The isolation boundary
  between tenants is the VM boundary, not a container or a namespace; the
  guest kernel is E2B's pinned build and the guest runs only what the
  image's Dockerfile puts there.
- **Every orchestrator listener except pprof binds `0.0.0.0`**, and its
  gRPC API is **unauthenticated**. That is why the host runs an nftables
  table (`inet e2b_guard`, loaded by `e2b-guard.service` from
  `deploy/e2b/e2b-guard.nft`) that:
  - accepts traffic from sandbox source addresses only to the ports they
    need (`5010`, `5016`–`5018`);
  - drops everything else from sandbox sources;
  - drops all off-host traffic to the orchestrator ports
    `5007, 5008, 5010–5012, 5016–5018`.

  spoond reaches the orchestrator over loopback only (`E2B_GRPC_ADDR`
  `127.0.0.1:5008`, `E2B_PROXY_URL` `http://127.0.0.1:5007`). **Never**
  relax that: nothing on the network may be able to call `Create` or
  `Delete`. The guard table is its own file and its own oneshot unit
  precisely so it never touches `nftables.service` or the host's other
  rules.
- **Do not run the rest of E2B's stack.** No E2B API, dashboard, client
  proxy, Postgres, ClickHouse, Redis, Nomad or cloud storage — the
  attack surface is one Go process plus Firecracker.

**The host-address guard (patch P4).** The orchestrator's TCP egress
firewall refuses any connection whose destination is an address of the
host itself — including loopback — unless a sandbox's egress policy
carries an allowance naming *both* the IP and the TCP port. spoond's
policies use exactly that mechanism to grant what guests need: the proxy
/ LLM gateway port (`HOST_GUEST_SERVICE_PORT`) and DNS, plus the lease API
port for `lan`/`internet` guests. So a guest with `lan` or `internet`
egress can reach the LAN, and it *still* cannot reach the orchestrator's
gRPC port, the registry, or any other host service spoond did not name.
Layer 1 (per-netns nftables inside the orchestrator) enforces the
private-range floor; layer 2 (the userspace TCP proxy) enforces domains,
CIDRs and the port-scoped private allowances.

**Tokens.** Two HMAC-derived-from-one-seed tokens plus one fixed id
(`/etc/spoond/e2b-token-seed`, 0600, at least 32 bytes — the backend
refuses to start without it):

- the **envd token** per sandbox (`hex(HMAC-SHA256(seed, id))`),
  required by envd's gRPC for process start, PTY, files and health;
- the **envd traffic token** per sandbox
  (`hex(HMAC-SHA256(seed, "sandbox-traffic-"+id))`), required by the
  orchestrator's proxy for every guest port except envd's own — this is
  how spoond's HTTP proxy reaches a guest app, and why a peer sandbox
  cannot;
- the **team id** (`E2B_TEAM_ID`), a fixed UUID sent on every gRPC
  request — not a secret, but pinned so the orchestrator's per-team
  accounting stays consistent;
- the seed itself never leaves the host. Losing it invalidates every
  derived token, so it is part of the backup set.

Guest port **49983 is envd**, the guest agent (process start, PTY,
files, health). It is never published via `expose_ports`, never routed by
spoond's HTTP proxy (`403`), and reachable only with the sandbox's
traffic token through the orchestrator. Its powers are exactly the
powers exec and stream already give the lease owner.

## Rescan fixes (second pass, commit after 35540b5)

- **F1 clone quota** — clone reserves and releases quota like create; it
  surfaces `429` and cannot bypass `max_leases`.
- **F2 proxy header leak** — the reverse proxy strips `X-Proxy-Auth`,
  `Remote-User`, `X-Spoond-User-Id`, `X-Bootstrap-Token` and every
  `E2b-*` routing header before the guest app sees them, then sets its
  own; a tenant cannot harvest the forward-auth secret or forge a
  sandbox-routing header from inside their own sandbox.
- **F3 guest isolation** — default egress policy is `restricted`, not
  `lan`: a guest reaches only the host services spoond grants (the proxy
  / LLM gateway port, DNS) plus its allowlist, not peer sandboxes.
  Operators who need full LAN egress opt in with `network_policy=lan`.
- **F4 proxy capability names** — in capability mode (no auth) only the
  unguessable 32-hex lease id hostname resolves; guessable friendly
  names are `404` unless forward-auth is on (where lookups are
  owner-scoped). Legacy single-user deployments keep friendly names.
- **F5 by-key oracle** — `GET /api/users/by-key` returns only id + name
  (was the full user view: admin flag, fingerprints, quotas).
- **F6 memory cap** — superseded by the substrate rule: `memory_mib` on
  create must be `0` or exactly the image's fixed `memory_mb`, otherwise
  `400`. There is nothing to clamp.
- **F7 bootstrap token** — the SSH gateway does not forward
  `X-Bootstrap-Token` (replaying it on the most exposed surface recreated
  the fresh-store admin race); bootstrap is an operator action via a
  direct API call. The `--bootstrap-token` flag is accepted and ignored.
- **F8 attach scoping** — SSH attach and session relay run under the
  user-scoped context (`gwCtx`), so exec and stream happen as the SSH
  user, not the gateway service identity.
- **F9 activity cap** — per-owner `busyMax` (8) on exec/stream → `429`
  beyond it (quota covers lease count, not in-flight activity).
- **F10 LLM body cap** — gateway request body limited to 1 MiB; both
  HTTP listeners get `ReadHeaderTimeout` + `MaxHeaderBytes`.
- **F11 prompt JSON injection** — `model` is JSON-escaped like `message`
  (was raw interpolation into the agent JSON).
- **F12 nil-identity panic** — impersonation guards `identities != nil`.
- **F13 assets containment** — `/assets/` does an explicit
  `filepath.Join` containment check instead of relying on the stdlib's
  incidental dot-dot rejection.
- **Unit hardening** — the gateway token moved out of `ExecStart` into
  `/etc/spoond-gateway.env` (0600, `SPOOND_GATEWAY_TOKEN`).

Known/accepted residuals:

- Legacy consumer-owned leases keep the capability-model LLM path
  (operator-controlled tokens); `requireKey` covers identity-store users.
- `InsecureSkipVerify` on the gateway→backend loopback TLS (self-signed
  cert; local-only). Prefer a pinned CA or Unix socket in locked-down
  deployments.
- `PROXY_AUTH_MODE` still defaults to off (capability model) — flipping
  it requires the staged Caddy forward-auth deploy.
- Firecracker without the jailer (above) is a deliberate trade: the VM
  boundary plus the host firewall and the host-address guard are the
  compensating controls, and the orchestrator is not exposed off-host.

## Multi-user boundaries

- **Identity store is authoritative** for SSH when configured: the
  gateway probes `GET /api/identity-status` at startup; with a store
  present, only keys registered on a user authenticate (the local
  `--client-keys` allowlist is a legacy single-user fallback and is
  ignored). `ssh-key rm` therefore really revokes.
- **`GET /api/users` is admin-only.** Non-admins get `GET /api/users/me`
  and `GET /api/users/by-name/{name}` (id+name only). Sharing grants by
  username resolve through the minimal endpoint.
- **Quotas are reservation-based** (`reserveQuota` under the store lock +
  release on completion): concurrent creates cannot exceed `max_leases`
  (TOCTOU closed). `fork` reserves for every child before creating any.
- **Bootstrap**: set `BOOTSTRAP_TOKEN` on the backend so the first-user
  creation requires `X-Bootstrap-Token`; without it, any token holder
  could claim admin on a fresh store.
- **LLM gateway**: when the identity store is present, leases owned by an
  identity user are denied on `/llm/` unless that user has an LLM key
  (`POST /api/users/{id}/llm-key`) — that is the default. Setting
  `LLM_OPEN_LEGACY` to any non-empty value restores the open behavior
  for keyless identity users. Legacy consumer-owned leases keep the
  capability model either way.
- **Forward-auth proxy** (`PROXY_AUTH_MODE=forward-auth`): requires
  `X-Proxy-Auth` shared secret (constant-time) + `Remote-User`; set
  `PROXY_AUTH_TRUSTED_PEERS` (e.g. `10.1.0.203/32`) so only Caddy can
  present an identity. Caddyfile: `deploy/caddy-sandbox-forwardauth.conf`.
- **Shares** are scoped by mode (`ssh` = attach/prompt/stream, `http` =
  exec/stream/stat/proxy), expire, and are revocable on the spot.

## Token/key storage

- Token and LLM-key hashes are **HMAC-SHA256 with a per-store random
  salt** (sidecar `<users-file>.salt`, 0600) since the fix pass. A
  pre-existing store without a sidecar stays in legacy plain-SHA256 mode
  so existing tokens keep verifying; to migrate, re-seed users or rotate
  tokens.
- `users.json` is written 0600 via temp+rename; keep its parent directory
  `0700` (e.g. `/var/lib/spoond`).
- Shares, leases, the pool and the image catalog persist in SQLite
  (`SPOOND_DB_PATH`) — back that file up with its salt and the token
  seed; see [operations.md](operations.md).
- The envd/traffic seed (`/etc/spoond/e2b-token-seed`) is 0600 and never
  leaves the host.

## CI runner

- The checkout token (`secrets.GITHUB_TOKEN`) is passed to git via
  `GIT_CONFIG_COUNT/KEY/VALUE` env with a validated charset
  (`[A-Za-z0-9_.-]`) and single-quoting — never interpolated into the
  command string, never visible in argv/ps. Tokens with shell
  metacharacters fail the job rather than inject.
- CI jobs run in their own microVM with the image's fixed memory and the
  policy the runner requested (`restricted` unless the job opts out), so
  a job cannot reach the host or other tenants by default.

## Rate limiting

- Token auth failures are throttled per client IP (5 failures / 30s →
  429) and reset on success. Tokens are high-entropy; this bounds
  by-key probing and junk traffic.

## Known operational notes

- `/metrics` requires an admin user or the scrape-only `METRICS_TOKEN`;
  make sure the scraper and the dashboard use one of those, and nothing
  else uses the metrics token (the backend refuses it on every other
  route).
- Rotate the SSH gateway's `SPOOND_GATEWAY_TOKEN` if it ever appears
  outside its 0600 env file; it is admin-equivalent (it can impersonate
  any user). Same for `ADMIN_TOKEN`, which drives the drain API.
- Keep the orchestrator's ports firewalled (above) and the build store's
  permissions as installed — it holds every tenant's snapshots.
- See [operations.md](operations.md#users--identity) for the identity
  revocation and salt-rotation procedures, and [substrate.md](substrate.md)
  for what the orchestrator does and does not run.
