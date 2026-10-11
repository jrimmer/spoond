# DONE spoond-5mr1: #138 A3 — ADMIN_TOKEN mints user tokens; who-can-do-what docs

A3 of the unified-auth design
([docs/plans/2026-10-11-unified-auth.md](docs/plans/2026-10-11-unified-auth.md),
needs A2, which main already has): `POST /api/users/{id}/tokens` also
accepts `ADMIN_TOKEN` — the root-only break-glass — so root on the host can
mint a short-lived token for any user during a maintenance window. It stays
on that one route: GET/DELETE on the tokens keep their A2 rules
(self/admin-user), and every other user route refuses `ADMIN_TOKEN` as the
unknown token it is. Every ADMIN_TOKEN mint is logged with the user, the
token's name and id, never the secret.

## Changes

- **Route (`api/users_tokens.go`, `api/server.go`)** — `Handler()` evaluates
  `IsAdminBearerToken` once per request and, when the bearer equals the
  configured `ADMIN_TOKEN` **and** the path/method is exactly the mint route
  (`isAdminTokenMintPath`, POST), marks the request context
  (`ctxAdminMint`). `authMiddleware` lets a marked request through
  untouched (no owner, no identity user); without the exemption
  `ADMIN_TOKEN` is not a consumer token and keeps its ordinary `401`.
  The mint handler's `adminMintExemption` re-checks marker + method +
  path so the middleware's grant and the handler's recognition agree by
  construction and a stale marker cannot authenticate anything else.
  The route keeps its shape: an ADMIN_TOKEN mint answers `404` for an
  unknown target user, keeps the same body validation (`400`s) and the
  same `409` duplicate-name conflict; identity callers (self or admin)
  mint exactly as in A2; a legacy consumer token still gets `403`.
  The break-glass log line reads `token minted with ADMIN_TOKEN: user
  <name> (<id>) token "<name>" id <t-id>, expires_at …` — no secret.
- **Not a consumer token** — `ADMIN_TOKEN` still resolves to nothing: no
  owner for leases, no identity user, no gateway impersonation, no
  `/metrics`, no event stream; and a plain user's own token does not
  gain the break-glass power (the exemption rides the token, not the
  route).
- **Tests (`api/users_tokens_admin_test.go`)**: ADMIN_TOKEN mints for a
  plain user and a second plain user (the minted token authenticates as
  its user on `/api/users/me`; the log carries name/id and never the
  `sdt-` secret); ADMIN_TOKEN still cannot use the other user routes
  (a walk over GET/DELETE of the tokens, `/api/users`,
  create/quota/llm-key/delete/usage-view/me, the lease surface,
  `/metrics`, the `by-name` spelling — all `401`, each probe from a
  distinct fake client IP so the refused-auth limiter cannot turn them
  into `429`s); wrong or missing token `401` on the mint route; the
  `/api/admin/*` surface is unchanged (correct + wrong token paths
  still pass the A1 contract); with no `ADMIN_TOKEN` configured the
  path is inert (legacy consumer `403`, unknown `401`).
- **Docs**: docs/api.md — the mint route's **Who** paragraph plus a
  break-glass subsection with the curl example (`ttl_secs`, secret
  answered once, refused everywhere else); docs/operations.md —
  "Maintenance windows and the break-glass token mint" before the
  planned orchestrator restart (find the id, mint with `ttl_secs`,
  revoke note, the one-route rule); docs/security.md — the "Who can do
  what" table from the design (user token, admin user token,
  `ADMIN_TOKEN`, `METRICS_TOKEN`/`EVENTS_TOKEN`) and what the one-route
  rule bounds for a leaked `ADMIN_TOKEN`; CHANGELOG Unreleased entry.

## Gates

- `go build ./...` — clean; `gofmt -l api/` — empty; `go vet ./api` — clean
- `GOMAXPROCS=2 go test ./api -count=1 -p=2` — ok (~56 s)
- `go test ./... -count=1` — all pass
- The named gates and the token tests also pass as the non-root `user`
  (`HOME`/`GOCACHE` under `/tmp`, no writes outside the checkout):
  `su user -c 'GOMAXPROCS=2 go test ./api -count=1 -p=2'` — ok. No store
  paths or CWD assumptions were added, so nothing needed a root skip.
- `git fetch origin`: `origin/main` added nothing since the branch base
  (`HEAD..origin/main` empty); no new step types, providers, restart,
  cancel or retry paths to reconcile. No store/migrations file was
  touched.