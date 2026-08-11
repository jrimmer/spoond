# Security model & hardening notes

Status of the adversarial review findings (issue #37) after the fix pass.

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
