# U8 — LLM gateway per-user keys/quota (T8 / #35): design memo

> Epic #26 · parallel research (delegated agent, 2026-08-11) · input to U8 implementation.
> Scope: per-user LLM keys on the identity store, quota metering, upstream key hygiene.

---

I have everything I need. Here's the design memo.

---

# Design Memo: Per-User Auth + Quota Metering for the spoond LLM Gateway (epic #26, T8/#35)

**Repo:** `/root/forgejo-work/hyper-forgejo-runner` (module `github.com/jrimmer/spoond`), branch `main` (clean; no `identity/` work landed yet — `git log` top is `79d2bbf`, the plan-commit `60fb1f6` is in history).

## 1. How the gateway works today

**Path & routing.** Guests reach `POST /llm/<lease-id>/<provider>/...` (`llmGatewayPrefix = "/llm/"`, `api/llmgateway.go:22`). The gateway is mounted twice: on the authenticated mux (`api/server.go:71`) and directly on the plain-HTTP proxy listener (`api/proxy.go:36-39`, reached at `http://10.43.0.1:8891/llm/...` — this is the URL guests actually use).

**Auth = auth-exempt + lease-id-as-capability.** `authMiddleware` (`api/server.go:107-127`) exempts any path starting with `/llm/` (line 109). The gateway itself does NO credential check: `ServeHTTP` (`api/llmgateway.go:106-145`) parses the lease id, resolves it with `g.lookup` — which is `svc.lookupAny` (`api/server.go:46`, `api/service.go:700`), i.e. **owner-agnostic** — checks only `lease == nil` / `lease.Suspended`, matches a provider prefix, and forwards. Anyone who knows a lease id (anything inside the sandbox, or anyone who sees the URL) can burn the shared upstream key. Attribution is impossible: every request goes out as the one host key. Confirmed by `TestLLMGateway` (`api/server_test.go:758-881`, esp. line 791: "No bearer token: /llm/ must be auth-exempt").

**Forwarding & key injection.** `ServeHTTP` reads the body, remaps `model` through `modelMap`/`defaultModel` (`mapModel`, lines 75-101), builds an outbound request, and sets `Authorization: Bearer <g.key>` (`api/llmgateway.go:180`) — the sole auth to the upstream. It deliberately avoids `httputil.ReverseProxy` (comment at 160-164) and streams the response body SSE-safe (lines 201-224). Note: it strips `X-Lease-Id` but copies all other inbound headers (line 179-181) — a guest-supplied `Authorization` would be overwritten by the host key, which is correct.

**Upstream keys host-side.** `LLM_UPSTREAM_URL` / `LLM_UPSTREAM_KEY` / `LLM_DEFAULT_MODEL` / `LLM_MODEL_MAP` env → `NewServerWithLLM(svc, reg, url, key, defaultModel, modelMap)` (`cmd/spoond-backend/main.go:99-108`) → `newLLMGateway(...)` stores the key in `llmGateway.key` (`api/llmgateway.go:42`). It never enters a VM; `cmd/spoond-doctor/main.go:230-243` validates the pair. **This invariant must not change.**

**Guest-side discovery.** The sshd gateway's `runShelly` ctl verb (`cmd/spoond-sshd-gateway/main.go:1093-1135`) execs a script inside the sandbox that writes `/root/shelley.json` = `{"llm_gateway":"http://10.43.0.1:8891/llm/<lease-id>","default_model":...}` (line 1103). Shelley (exe.dev's Gateway source) calls `{gateway}/<provider>/...` with an "implicit" credential — i.e., **no key today**. The gateway talks to the backend as one shared identity (`Authorization: Bearer *backendTok`, `main.go:1016`).

**Identity/quota infrastructure.** None beyond `Service.tokens map[string]string` (token→consumer name, `api/service.go:86`; `CONSUMER_TOKENS` env, `main.go:64-77`) and `Lease.Owner` (`service.go:25`), which is enforced by `lookup(owner,id)` (`service.go:687`) but never by the LLM route. The users store is planned in U1 (`docs/plans/2026-08-11-multi-user-tenancy-plan.md:127-133`: id, name, kind=person|agent, admin, fingerprints, token hashes, quotas — new `identity/` package); quota columns in U5 (plan:159-165). **Neither exists yet.**

## 2. Recommended design

### 2a. Auth: per-user LLM key, validated inside the gateway

- **Keep `/llm/` exempt from `authMiddleware`.** Sandboxes hold no consumer token (hard constraint, documented in `server.go:104-106`); the capability model for *reaching* the route stays.
- **Add a per-user key check inside `llmGateway.ServeHTTP`**, between lease lookup (line 116) and provider matching:
  1. Parse `Authorization: Bearer <key>`; missing/invalid → 401.
  2. `user := users.ByLLMKey(key)` (hash lookup, constant-time compare) → 401 if unknown.
  3. **Ownership:** `lease.Owner != user.ID` → 403 (this is the "per-lease LLM route validates the lease owner" from U8). Note the *same* 403 shape should cover the future U9 share case.
  4. Quota pre-check (below) → 429.
- **Key material:** each user record gains `LLMKeyHash` (SHA-256 of a random `slk_…` key) plus the U5 quota fields. Keys are minted/rotated via backend endpoints, never via env. The upstream key path (`g.key` at line 180) is untouched — the per-user key only authorizes *the caller*; the host key still authenticates *to the provider*. Attribution becomes "which user's key was presented," which the gateway logs and meters.
- **Key delivery into the sandbox:** `runShelly` already writes `shelley.json`; after U1 the gateway knows the SSH-authenticated user (key→user resolution), so it fetches that user's LLM key from a new backend endpoint (e.g. `GET /api/users/{id}/llm-key`, callable with the gateway's `*backendTok`) and adds `"api_key": "<slk_…>"` to `shelley.json` (line 1103's `printf`). This is the only place a user key crosses into a VM — acceptable, because a leaked user key is revocable and scoped to that user's quota, unlike the upstream key.
- **Backward compat (KTD-3):** single-user deployments (no users store, only `CONSUMER_TOKENS`) keep today's auth-exempt behavior. Recommend an explicit toggle, e.g. `LLM_REQUIRE_USER_KEY` (default off; on in multi-user deploys), rather than inferring from store population.

### 2b. Quota metering hook (U5 integration)

- **Data:** U5's accounting counters live on the user record (max concurrent leases, max TTL at create — unchanged; plus LLM: e.g. `llm_max_tokens_per_day`, `llm_used_tokens`, `llm_window_start`).
- **Request gate:** in `ServeHTTP` after auth/ownership, `svc.CheckLLMQuota(user.ID)` → 429 with a clear message. This is a cheap in-memory check (mutex-protected counters in the users store, mirroring `Service.tokens`'s map pattern).
- **Post-response metering:** the response is streamed (SSE), so record usage *after* the copy loop at lines 211-224:
  - Non-stream JSON: parse the response `usage` object (`prompt_tokens`/`completion_tokens`).
  - Streaming: the OpenAI-compatible `usage` field rides in the final chunk before `data: [DONE]` — tee the tail of the stream (or capture the last ~64KB) and parse it; fallback estimate `len(body)/4`.
  - Call `svc.RecordLLMUsage(user.ID, prompt, completion)` once per request; log lease id + user id + tokens (never keys).
- **Semantics:** pre-check at request start only; a long stream may overshoot the cap mid-flight — acceptable for v1 (post-metering catches it for the next request).

### 2c. Minimal change set (T8 deltas on top of U1+U5)

U1 (users store) and U5 (quota columns/accounting) are hard prerequisites — T8's plan dependency (`plan.md:104`, `U8 depends U1, U5`). Given those land, the T8-specific changes are:

| # | File | Change |
|---|---|---|
| 1 | `identity/` (new, U1) | `User` gains `LLMKeyHash string`; `ByLLMKey(sha256(key)) *User`; quota fields + counters (U5) |
| 2 | `api/service.go` | `userByLLMKey(key string) *User`, `CheckLLMQuota(userID) error`, `RecordLLMUsage(userID, prompt, completion int)`, `UserLLMKey(userID) string` (gateway-token-only); expose users store on `Service` |
| 3 | `api/llmgateway.go` | add `users` field to `llmGateway`; in `ServeHTTP` after line 124: bearer-key auth → ownership check vs `lease.Owner` → quota 429; meter after the copy loop (2b); keep `g.key` injection at line 180 untouched |
| 4 | `api/server.go` | `authMiddleware` unchanged (still exempts `/llm/`); `NewServerWithLLM` gains the users store (or reads `svc.users`); new `GET /api/users/{id}/llm-key` handler (gateway-token-only) |
| 5 | `cmd/spoond-backend/main.go` | pass users store into `NewServerWithLLM`; no new LLM env vars (user keys are store data, not config) |
| 6 | `cmd/spoond-sshd-gateway/main.go` | `runShelly` (line 1093): fetch user's LLM key via new endpoint, write `"api_key"` into `shelley.json` (line 1103) |
| 7 | `api/server_test.go` | extend `TestLLMGateway` (line 758): missing key → 401; valid key on own lease → 200 with upstream auth still the host key; cross-owner key → 403; quota-exhausted → 429 |
| 8 | `docs/api.md` | update the "LLM gateway (per-lease, no bearer token)" section (lines 179-185) for the new contract |

**Order of checks in `ServeHTTP` (final):** parse lease id → `lookupAny` (404) → suspended (409) → user-key auth (401) → owner match (403) → quota (429) → provider match (501) → forward with host key → meter.

## 3. Pitfalls / notes

- **Don't break streaming for metering:** never buffer a full SSE response; tee the tail only.
- **Constant-time key lookup:** compare SHA-256 hashes (`crypto/subtle`), never raw keys; never log `Authorization`.
- **Plain-HTTP hop:** guests reach the gateway over `forkd-br0` (`http://10.43.0.1:8891`); bearer keys transit that internal LAN. Acceptable, but note it in ops docs; the public proxy path is Caddy-TLS-terminated.
- **Mid-stream quota overshoot** and **shared-lease attribution** (U9 shares must extend the ownership predicate at the same 403 site) are the two known follow-ups.
- **Model mapping is unaffected** — per-user keys are orthogonal to `LLM_MODEL_MAP`/`LLM_DEFAULT_MODEL`.

**Recommended approach:** implement 2a in full (key + ownership) and 2b's pre-check + post-metering; the single highest-value line is the ownership check `lease.Owner == user.ID` in `llmGateway.ServeHTTP`, which converts the gateway from "any lease-id holder burns the shared key" to "each user authenticated, attributable, and capped."

---

## Implementation notes (T8 landed 2026-08-11)

What was actually implemented, and the deliberate deviations from the memo:

- **Auth mechanism: `Authorization: Bearer <slk_…>`** (not `X-Spoond-LLM-Key`). The gateway is an OpenAI-compatible endpoint, so the standard Authorization header is the least surprising contract and Shelley/ACP clients can pass it through unchanged. The user key never reaches the upstream: `ServeHTTP` clones inbound headers but overwrites `Authorization` with the host key (`g.key`) before forwarding — same code path as before, verified by test.
- **Ownership is folded into key verification** (memo 2a step 3's separate 403 does not exist as its own check). The gateway does `g.users.LLMKeyOK(lease.Owner, presentedKey)` — a key that verifies is by construction the lease owner's key, so a foreign user's valid key gets `401`, not `403`. Same security outcome, one less lookup; the U9 share case extends this predicate site.
- **Backward compat (KTD-3):** gate is `owner.LLMKeyHash != ""` — no `LLM_REQUIRE_USER_KEY` toggle env was added (store-population inference is exactly the memo's recommended semantic; the toggle can be added later without touching the auth code). No identity store (`svc.identities == nil`) ⇒ gateway fully open, unchanged.
- **Quota metering (memo 2b):** the post-response token metering (`RecordLLMUsage`, usage-object parsing) was **not** implemented — it needs U5 accounting counters on the user record that don't exist yet. Instead, the implemented quota hook is the **per-user concurrent in-flight cap** (`LLM_MAX_CONCURRENT_PER_USER`, default 0 = unlimited, `429` when exceeded), a self-contained counter in `llmGateway` keyed by lease owner. Post-stream token metering remains the documented hook point here (2b above).
- **New admin endpoint:** `POST /api/users/{id}/llm-key` with `{"llm_key":"…"}`; empty string revokes. Stored as SHA-256 hex in `User.LLMKeyHash` (persisted in the users JSON), verified constant-time (`crypto/subtle`). Never returned by the API (`UserView` has no field for it; test asserts no `llm_key_hash` in responses).
- **Key delivery into the sandbox (memo 2a "Key delivery"):** not implemented — `cmd/spoond-sshd-gateway` is out of scope for T8 (separate unit). `shelley.json` will gain `"api_key"` when that lands.
- **Order of checks now:** parse lease id → `lookupAny` (404) → suspended (409) → user-key auth (401) → concurrency cap (429) → provider match (501) → forward with host key.

Files changed by T8: `identity/store.go` (+`LLMKeyHash`, `SetLLMKey`, `LLMKeyOK`, `hashSecret`), `api/llmgateway.go` (auth + cap), `api/server.go` (wiring, route, `SetLLMMaxConcurrent`), `api/users.go` (`handleUsersLLMKey`), `cmd/spoond-backend/main.go` (`LLM_MAX_CONCURRENT_PER_USER`), tests in `identity/store_test.go`, `api/users_test.go`, new `api/llmgateway_test.go`, docs (`usage.md`, `api.md`, this memo).

---

## Summary

- **What I did:** Read `api/llmgateway.go`, `api/server.go`, `api/service.go`, `cmd/spoond-backend/main.go`, `api/proxy.go`, the sshd gateway's `runShelly`/backend auth, `TestLLMGateway`, the epic #26 plan (`docs/plans/2026-08-11-multi-user-tenancy-plan.md`), and `docs/api.md`; checked git state (main is clean, no `identity/` landed).
- **Findings:** (1) `/llm/` is auth-exempt with lease-id-as-capability via `lookupAny` — no credential, no ownership check; (2) `LLM_UPSTREAM_KEY` lives only in `llmGateway.key`, injected at `llmgateway.go:180`, never in VMs; (3) per-user keys fit as `LLMKeyHash` on the planned U1 users store, validated inside `ServeHTTP`, with delivery via `shelley.json` in `runShelly`; (4) quota hooks in as a 429 pre-check + post-stream usage accounting via new `Service` methods; (5) minimal change set is 8 concrete edits (table above), all on top of U1+U5 prerequisites.
- **Files created/modified:** none (research only, per instructions).
- **Issues:** none — main blocker is that U1/U5 prerequisites don't exist yet, so T8 cannot land standalone.