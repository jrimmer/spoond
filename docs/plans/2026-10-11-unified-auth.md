# Unified authorization (#138) — 3.3 design

Status: settled by orch-1, 2026-10-11 PT (owner: "settle it yourself
though don't overcomplicate — spoond is intended to be straightforward
to use and manage"). Each point is one rule the owner can change.

## The model

Two roles and two narrow service tokens, nothing more:

| Credential | Who | Can do |
|---|---|---|
| user token | any user | its own leases, snapshots, usage, its own tokens |
| admin user token | a user with `admin: true` | everything a user can, every user's resources, `/api/users`, `/api/admin/*`, `/metrics`, the full event stream |
| `ADMIN_TOKEN` (env, root-only file) | the host's root | `/api/admin/*` and minting a token for any user — the break-glass for maintenance windows |
| `METRICS_TOKEN`, `EVENTS_TOKEN` | scrapers, the dashboard | `GET /metrics`, `GET /api/leases/events`, read-only, unchanged |

No scope lists. The 2.x tokens keep working unchanged (`CONSUMER_TOKENS`,
a user's existing token, `ADMIN_TOKEN`).

## Changes

1. **A1 — admin users reach `/api/admin/*`.** Those routes accept an
   admin user's token as well as `ADMIN_TOKEN`. (Today only
   `ADMIN_TOKEN` works there, while `/api/users` needs an admin user:
   the 2026-10-07 quota change failed for want of the other one.)
2. **A2 — several named tokens per user, revocable, optionally
   expiring.** `POST /api/users/{id}/tokens {"name","ttl_secs"?}`
   returns the secret once; `GET /api/users/{id}/tokens` lists name,
   id, created, expires and last used (never secrets); `DELETE
   /api/users/{id}/tokens/{token_id}` revokes. The caller may manage its
   own tokens; an admin may manage anyone's. The user's existing single
   token is listed as the token named `default` and revocable like the
   rest. An expired token authenticates nobody.
3. **A3 — the host mints with `ADMIN_TOKEN`.** `POST
   /api/users/{id}/tokens` also accepts `ADMIN_TOKEN`, so root on the
   host can mint a short-lived token for any user during a window
   (`ttl_secs` recommended). Every mint and revoke is logged with the
   token's name and id, never its secret.
4. **Docs**: one "Who can do what" table in docs/api.md and
   docs/security.md; CHANGELOG.

Out of scope: per-route scopes, OAuth/OIDC, moving secrets out of env.
