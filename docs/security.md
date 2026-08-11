# Security model & hardening notes

Status of the adversarial review findings (issue #37) after the fix pass
and the independent clean-room rescan (deleg_3a249d37, two fresh reviewers).

## Rescan fixes (second pass, commit after 35540b5)

- **F1 clone quota** — `grantFromSnapshot` now calls `reserveQuota` +
  deferred release (clone can't bypass `max_leases`); clone surfaces 429
  like create.
- **F2 proxy header leak** — the reverse-proxy `Rewrite` strips
  `X-Proxy-Auth`, `Remote-User`, `X-Spoond-User-Id`, `X-Bootstrap-Token`
  before the guest app sees them; a tenant can no longer harvest the
  forward-auth secret from inside their own sandbox.
- **F3 guest isolation** — default egress policy is now `restricted`, not
  `lan`: a guest can only reach the host bridge IP (10.43.0.1: LLM
  gateway, shelly assets, proxy) + its allowlist, NOT peer sandboxes.
  Every sandbox carries an unauthenticated root exec agent on :8888 and
  a shelley agent on :9000, so guest→guest must be blocked by default;
  operators who need full LAN egress opt in with `network_policy=lan`.
  (The agent itself still binds 0.0.0.0:8888 because forkd-controller
  dials it from the host — full agent-auth is a controller-side change.)
- **F4 proxy capability names** — in capability mode (no auth) only the
  unguessable 32-hex lease id resolves; guessable friendly names are 404
  unless forward-auth is on (where lookups are owner-scoped). Legacy
  single-user deployments (no identity store) keep friendly names.
- **F5 by-key oracle** — `GET /api/users/by-key` now returns only id +
  name (was full UserView: admin flag, fingerprints, quotas).
- **F6 memory cap** — `memory_mib` is clamped server-side to
  `maxLeaseMemoryMiB` (16 GiB); negatives → 0.
- **F7 bootstrap token** — the SSH gateway no longer forwards
  `X-Bootstrap-Token` (replaying it on the most exposed surface
  recreated the fresh-store admin race); bootstrap is an operator action
  via direct API call. `--bootstrap-token` flag accepted but ignored.
- **F8 attach scoping** — SSH attach/`restartSSHD` now use the
  user-scoped context (`gwCtx`), so `/endpoint` + exec run as the SSH
  user, not the gateway service identity (was: broken attach in store
  mode + gateway-owned leases attachable by any user).
- **F9 activity cap** — per-owner `busyMax` (8) on exec/stream → 429
  beyond it (quota covers lease count, not in-flight activity).
- **F10 LLM body cap** — gateway request body limited to 1 MiB; both
  HTTP listeners get `ReadHeaderTimeout` + `MaxHeaderBytes`.
- **F11 prompt JSON injection** — `model` is now `json.dumps`-escaped
  like `message` (was raw interpolation into the agent JSON).
- **F12 nil-identity panic** — impersonation block guards `identities !=
  nil` (gateway token without a store no longer crashes).
- **F13 assets containment** — `/assets/` does an explicit
  `filepath.Join` containment check instead of relying on the stdlib's
  incidental dot-dot rejection.
- **Unit hardening** — gateway token moved out of `ExecStart` into
  `/etc/spoond-gateway.env` (0600, `SPOOND_GATEWAY_TOKEN`); install
  script writes it.

Known/accepted residuals (documented, not code-changed):
- Legacy consumer-owned leases keep the capability-model LLM path
  (operator-controlled tokens); `requireKey` covers identity-store users.
- `InsecureSkipVerify` on the gateway→backend loopback TLS (self-signed
  cert; local-only). Prefer a pinned CA or Unix socket in locked-down
  deployments.
- The in-guest agent channel is unauthenticated by contract with
  forkd-controller; default restricted policy is the spoond-side
  mitigation.
- `PROXY_AUTH_MODE` still defaults to off (capability model) — flipping
  it requires the staged Caddy forward-auth deploy (U7).

## Multi-user boundaries (epic #26)

- **Identity store is authoritative** for SSH when configured: the gateway
  probes `GET /api/identity-status` at startup; with a store present, only
  keys registered on a user authenticate (local `--client-keys` allowlist
  is a legacy single-user fallback and is ignored). `ssh-key rm` therefore
  really revokes.
- **`GET /api/users` is admin-only.** Non-admins get `GET /api/users/me`
  and `GET /api/users/by-name/{name}` (id+name only). Sharing grants by
  username resolve through the minimal endpoint.
- **Quotas are reservation-based** (`reserveQuota` under the store lock +
  `releaseQuotaReservation` on completion): concurrent creates cannot
  exceed `max_leases` (TOCTOU closed).
- **Bootstrap**: set `BOOTSTRAP_TOKEN` on the backend and gateway units so
  the first-user creation requires `X-Bootstrap-Token`; without it, any
  token holder could claim admin on a fresh store.
- **LLM gateway**: when the identity store is present, leases owned by an
  identity user are DENIED on `/llm/` unless that user has an LLM key
  (`POST /api/users/{id}/llm-key`), unless `LLM_OPEN_LEGACY=1`. Legacy
  consumer-owned leases keep the capability model.
- **Forward-auth proxy** (`PROXY_AUTH_MODE=forward-auth`): requires
  `X-Proxy-Auth` shared secret (constant-time) + `Remote-User`; set
  `PROXY_AUTH_TRUSTED_PEERS` (e.g. `10.1.0.203/32`) so only Caddy can
  present an identity. Caddyfile: `deploy/caddy-sandbox-forwardauth.conf`.

## Token/key storage

- Token and LLM-key hashes are **HMAC-SHA256 with a per-store random
  salt** (sidecar `<users-file>.salt`, 0600) since the fix pass. A
  pre-existing store without a sidecar stays in legacy plain-SHA256 mode
  so existing tokens keep verifying; to migrate, re-seed users or rotate
  tokens (documented trade-off, security review #37 M1).
- `users.json` is written 0600 via temp+rename; keep its parent directory
  `0700` (e.g. `/var/lib/spoond`).
- Shares are in-memory: a backend restart drops all grants along with the
  leases themselves (leases are also in-memory). Acceptable, but know it
  before relying on long-lived shares.

## CI runner

- The checkout token (`secrets.GITHUB_TOKEN`) is passed to git via
  `GIT_CONFIG_COUNT/KEY/VALUE` env with a validated charset
  (`[A-Za-z0-9_.-]`) and single-quoting — never interpolated into the
  command string, never visible in argv/ps. Tokens with shell
  metacharacters fail the job rather than inject (security review #37 C3).

## Rate limiting

- Token auth failures are throttled per client IP (5 failures / 30s →
  429) and reset on success. Tokens are high-entropy; this bounds
  by-key probing and junk traffic (security review #37 L5).

## Known operational notes

- `/metrics` is admin-only when the identity store is present; make sure
  the metrics scraper uses an admin token.
- Rotate the SSH gateway's `--backend-token` if it ever appears outside
  the unit file; it is admin-equivalent (it can impersonate any user).
