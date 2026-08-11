# U7 — HTTP layer: proxy auth + per-user domains (T7 / #34): design memo

> Epic #26 · parallel research (delegated agent, 2026-08-11) · input to U7 implementation.
> Scope: Authelia forward-auth on the proxy, per-user domains/CNAMEs, owner-scoping.

---

---

# Design memo: per-user auth for the spoond HTTP proxy (epic #26 T7 / #34)

## 1. How the proxy listener works today

**Topology.** `cmd/spoond-backend/main.go` runs two listeners off one process:
- `BIND_ADDR` (default `127.0.0.1:8890`) → `srv.Handler()` (API mux + bearer-token `authMiddleware`, `api/server.go:99-127`).
- `PROXY_ADDR` (e.g. `0.0.0.0:8891`) → `srv.ProxyHandler()` (`main.go:131-139`). Plain HTTP; Caddy on CT131 (10.1.0.203) terminates TLS for `*.sandbox.lacy.casa` and forwards here (`api/proxy.go:17`).

**Routing.** `ProxyHandler()` (`api/proxy.go:31-49`) is a bare `http.HandlerFunc` — **no auth of any kind**. Dispatch order:
1. `/llm/…` prefix → `s.llm.ServeHTTP` (LLM gateway, below).
2. `/assets/…` → static file serve (`SetAssetsDir`, `proxy.go:52`).
3. Everything else → `handleProxy` (`proxy.go:54-105`): `parseProxyHost(r.Host)` extracts `<label>-<port>.sandbox.lacy.casa` → lease lookup → `svc.resolveEndpoint` → reverse proxy that `dialInNetns` into the guest netns (`proxy.go:88-96`), preserving the original Host so guest apps see the public hostname.

**Hostname grammar (`parseProxyHost`, `proxy.go:114-145`).** Single label under `.sandbox.lacy.casa`:
- `<32-hex-lease-id>.sandbox.lacy.casa` → `svc.lookupAny(label)` (`service.go:700`) — **owner-blind by design**;
- `<friendly-name>.sandbox.lacy.casa` → `svc.lookupByName(label)` (`service.go:628`) — **also owner-blind** (first match wins; names unique per owner);
- `-<port>` suffix selects the guest port; `defaultProxyPort = 3000` (`proxy.go:24`).

**Auth today: none.** "The lease id in the hostname is the capability (same model as SSH)" (`proxy.go:30`). Anyone who knows a lease id/name can reach the guest's web server. This is the gap R8 closes.

**Backward-compat wiring note:** the API listener's `authMiddleware` (`server.go:107-127`) is a separate path; `ProxyHandler` never passes through it, and T7 should keep it that way.

## 2. How the LLM gateway shares the listener

- `llmGatewayPrefix = "/llm/"` (`api/llmgateway.go:22`); mounted on the API mux and served from `ProxyHandler` (`proxy.go:36-39`) and the API listener.
- `ServeHTTP` (`llmgateway.go:106`) parses `/llm/<lease-id>/<provider>/...`, looks up the lease via `svc.lookupAny` (capability model), rewrites `model` through the map, injects the server-side upstream key, streams SSE.
- Guests reach it at `http://10.43.0.1:8891/llm/<lease-id>/...` (raw bridge IP, **no Caddy, no TLS** — deliberately, to dodge the backend's self-signed cert). Same for `/assets/` (`proxy.go:43-46`).
- Consequence for T7: `/llm/` and `/assets/` are guest-initiated paths that bypass Caddy. **They must stay exempt from proxy auth** (they already are, by dispatch order). T8 (per-user LLM) is a separate unit and changes only the app-layer check.

## 3. Authelia forward-auth contract (what T7 must consume)

Homelab facts (from `homelab-auth-architecture` skill): Authelia v4.39.20 on the Pangolin VPS, `https://auth.lacy.casa`, behind Traefik; session cookies 24h/1h/30d; `two_factor` policy on all domains; usernames are bare (`jason`, `trina`). Caddy on CT131 (10.1.0.203) is the internal front door; forward-auth was explicitly **skipped in Phase 12** (2026-07-27), so this is the first Caddy forward-auth deployment.

**Verify endpoint (v4.38+, use on 4.39.20):** `GET https://auth.lacy.casa/api/authz/forward-auth` (the old `/api/verify` is deprecated/removed).

**Request (header_auth strategy):** the proxy must forward the client's session `Cookie` plus:
- `X-Forwarded-Host` (original host, e.g. `mybox-8080.sandbox.lacy.casa`)
- `X-Forwarded-Uri` (original path+query)
- `X-Forwarded-Method`
- `X-Forwarded-Proto` (optional, default http)

**Responses:**
- `200` + headers `Remote-User` (bare username), `Remote-Name`, `Remote-Email`, `Remote-Groups` (comma-separated) — the proxy copies these onto the upstream request. (Some builds also refresh the session cookie.)
- `401` unauthenticated (redirect cookie/body to the portal; proxy passes it to the browser).
- `403` authenticated but denied by access_control rules.
- For browser flows the proxy does **not** strip the client's cookies; Authelia session cookie domain must cover `*.sandbox.lacy.casa` (already the case if `session.cookies[].domain: lacy.casa` — verify on the VPS).

**Caddy native integration (no caddy-security plugin build needed)** — Caddy ≥ 2.7 `handle_response` + `copy_headers`:

```
*.sandbox.lacy.casa {
    @noverify path /llm/* /assets/*
    handle @noverify { reverse_proxy 10.1.0.11:8891 }          # guest capability paths stay open
    handle {
        reverse_proxy https://auth.lacy.casa {                  # Authelia verify subrequest
            header_up Host auth.lacy.casa
            header_up X-Forwarded-Host {host}
            header_up X-Forwarded-Uri {uri}
            header_up X-Forwarded-Method {method}
            header_up X-Forwarded-Proto {scheme}
            handle_response {                                   # fires on 200
                copy_headers Remote-User Remote-Name Remote-Email Remote-Groups
                reverse_proxy 10.1.0.11:8891 {
                    header_up X-Proxy-Auth {$SPOOND_PROXY_SECRET}
                }
            }
        }
    }
}
```
401/403 pass through to the browser; on 200 the original request is re-proxied to spoond with `Remote-*` + the shared secret. Strip inbound `Remote-*`/`X-Proxy-Auth` from client-supplied headers in the same site block (defense in depth).

**Trust model:** spoond's listener is reachable by guests (raw IP) and by Caddy. Therefore `Remote-User` alone is spoofable — **spoond must require the `X-Proxy-Auth` shared secret on every hostname-routed request** and ignore any client-sent `Remote-User` (use only the resolved owner). `/llm/` + `/assets/` stay secret-free (unchanged capability posture).

## 4. Target topology

```
browser ── https://<label>.<user>.sandbox.lacy.casa ──► Caddy .203
        ── TLS + Authelia verify subrequest (auth.lacy.casa) ──► 200 + Remote-User
        ──► spoond :8891 (X-Proxy-Auth secret + Remote-User)
        ──► handleProxy: resolve user → owner-scope lookup → dialInNetns → guest app

guest VM ── http://10.43.0.1:8891/llm/<id>/... , /assets/...  (exempt, unchanged)
```

## 5. Minimal change set (file/function level)

**A. `api/proxy.go` (core)**
1. `Server` gains `proxyAuthMode` (`""`/`"off"`/`"forward-auth"`) + `proxyAuthSecret` (set via new `SetProxyAuth(mode, secret string)` in `api/server.go`; default `off` preserves KTD-3 single-user behavior).
2. `ProxyHandler()` (`proxy.go:31`): after the `/llm/`+`/assets/` exemptions, add the auth gate before `handleProxy`:
   - `off` → passthrough (today's behavior).
   - `forward-auth` → require `X-Proxy-Auth` == secret (constant-time compare; missing/wrong → 403); require non-empty `Remote-User` (missing → 401); resolve username → owner: `svc.identities.UserByName(remoteUser)` → `u.ID` (unknown user → 403); when `identities == nil` (legacy single-user) owner = raw `Remote-User` string; stash owner in context (`ctxProxyOwnerKey`), **never read the inbound `Remote-User` header again**.
3. `handleProxy()` (`proxy.go:54`): owner-scope all lookups (section 7). Change `lookupAny`/`lookupByName` calls to the scoped variants.
4. New `parseProxyHost2(host) (label, user string, port int, ok bool)` — keeps `parseProxyHost` as a wrapper for existing tests. Grammar in section 6.
5. New `handleUserRoot(owner, user string, port int)` — user-root label fallback: minimal lease portal (`svc.list(owner)` → JSON/HTML links); default-lease semantics deferred.

**B. `api/service.go`**
1. New `lookupUserScoped(caller, user, label string) *Lease` — resolves a hex id or friendly name against leases where `Owner == user`; requires `caller == user` (admin/shared leases arrive via T6 later).
2. New `lookupByNameOwner(owner, name string) *Lease` — `lookupByName` (`service.go:628`) with an `Owner` filter.
3. Leave `lookupByName`/`lookupAny` intact for the SSH gateway (`handleByName`, `server.go:913`) and the LLM gateway; T2/U3 scopes the gateway, T8 the LLM path. The proxy must stop using the owner-blind variants.

**C. `api/server.go`**
- `Server` struct (`server.go:23-29`) + `SetProxyAuth`; `ctxProxyOwnerKey`/`proxyOwnerFrom` beside `ctxOwnerKey` (`server.go:129-138`). No `authMiddleware` changes — the API listener is untouched.

**D. `cmd/spoond-backend/main.go`**
- New envs: `PROXY_AUTH_MODE` (default `off`), `PROXY_AUTH_SECRET` (required when mode is `forward-auth`); call `srv.SetProxyAuth(...)`; wire `svc.SetIdentities(...)` from a users-file env (T1 wiring, currently in the working tree: `api/users.go`, `identity/store.go`, `SetIdentities`/`ResolveOwner` in `service.go:120-140`).

**E. `deploy/` + docs**
- Caddyfile (CT131 `/etc/caddy/Caddyfile`): forward-auth block above + header stripping.
- Authelia (`/root/config/authelia/configuration.yml` on VPS): access_control rule for `*.sandbox.lacy.casa` (`two_factor`); confirm session cookie domain covers subdomains; follow the skill's backup→restart→verify SOP.
- Technitium: per-user wildcard CNAME `*.<user>.sandbox.lacy.casa → sandbox.lacy.casa` (one record per user; script at user creation; see skill `references/technitium-dns-api.md`).
- `docs/usage.md` proxy section (L86-98): new grammar + auth note.

## 6. Per-user domain grammar + DNS

`parseProxyHost2` accepts (case-insensitive, optional `:port`, optional `-<port>` suffix on the label, as today):
1. `<label>.sandbox.lacy.casa` — legacy single-label: hex id | friendly name | **user root**.
2. `<label>.<user>.sandbox.lacy.casa` — user-scoped: `<user>` must be a known user (else 404); label = hex id or friendly name of a lease **owned by that user**.

- **DNS reality check:** `*.sandbox.lacy.casa` already covers form 1 (one label). Form 2 is two labels — the existing wildcard does **not** cover it. Per-user wildcard records (`*.<user>.sandbox.lacy.casa`) are required; create them at user-creation time. Caddy's host wildcard may also need a `host *.*.sandbox.lacy.casa` matcher (verify with `caddy adapt` on .203).
- **Disambiguation rule for form 1:** lease lookup first (id, then owner-scoped name); if no lease and `identities.UserByName(label)` exists → user root (`handleUserRoot`). Edge: a lease named identically to its owner resolves as a lease — acceptable, document it.
- External (Cloudflare) exposure of per-user domains is **out of scope for T7** (internal split-horizon only; external stays via `vm2.lacy.casa:8890` API + Pangolin).

## 7. Owner-scoping semantics

Authenticated owner = user **ID** (`u-<hex>`, matching `Lease.Owner` once T2 lands) when the identity store is present; legacy consumer name otherwise. In `handleProxy`:
- user-scoped hostname → `lookupUserScoped(caller, user, label)`; caller ≠ user or lease missing → **403** (404 only when the hostname doesn't parse at all — keeps existence probing hard).
- legacy hostname, hex id → `lookupAny` then `lease.Owner != owner → 403` (this is the actual security fix: ids stop being a capability for the proxy).
- legacy hostname, friendly name → `lookupByNameOwner(owner, label)`; someone else's name → **403** (previously names leaked cross-owner via `lookupByName`).
- `/llm/` + `/assets/` unchanged (capability model; guest-only paths).
- Idle sweeper `touch` (`proxy.go:70`) only after the ownership check passes.

## 8. Tests (`api/proxy_test.go` + new handler tests)

- `parseProxyHost2` table: user-scoped id/name, `-port` variants, two-label, bad port, apex, >63-char, dot-in-user rejected.
- httptest handler tests (fake `Service`/`ForkdClient`, mirroring `proxy_test.go` + `server_test.go` style): no `Remote-User` → 401; wrong/missing secret → 403; owner → 200 + proxied; non-owner legacy id → 403; non-owner name → 403; user-scoped host of another user → 403; unknown Authelia user → 403; `/llm/` + `/assets/` without auth still work; `off` mode → legacy behavior.
- Integration (vm2, per plan U7): browser-level through Caddy; curl with `Remote-User` + secret headers; two-identity fixture from the plan's verification contract.

## 9. Risks / open questions

1. **Caddy `handle_response` + WebSocket upgrades** — verify an in-sandbox WS app through the authed host; fallback: exempt WS paths at Caddy or use the caddy-security plugin (requires xcaddy rebuild of Caddy on CT131).
2. **Per-request verify latency** (LAN → VPS subrequest) — fine at homelab scale; a short Caddy-side auth cache is a later optimization.
3. **Authelia username charset vs `isValidLabel`** (no dots) — usernames containing dots can't have a user-scoped hostname; constrain at T1 user creation.
4. **Ordering:** T7 depends on T1/T2 (identity store + `Lease.Owner` serialization — in progress, uncommitted in the working tree). The `off` default lets T7 land independently and flip when Caddy/Authelia are ready.
5. **Shared-secret hygiene:** `X-Proxy-Auth` is only as good as the secret; Caddy must strip inbound `Remote-*`/`X-Proxy-Auth` so clients can't smuggle headers past the verify.

**Recommended approach (TL;DR):** keep the single `:8891` listener; gate hostname-routed requests on `X-Proxy-Auth` secret + `Remote-User` (Caddy does the Authelia verify natively via `handle_response`/`copy_headers`, no plugin build); resolve username→user through the T1 `identity.Store` (`UserByName`), falling back to legacy consumer names; owner-scope every lease lookup in `handleProxy`; add per-user wildcard CNAMEs `*.<user>.sandbox.lacy.casa` in Technitium and a `parseProxyHost2` that understands two-label hosts; land behind `PROXY_AUTH_MODE=off` for backward compatibility.

---

**Summary of work done:**
- **Read:** `api/proxy.go`, `api/server.go`, `api/service.go`, `api/llmgateway.go`, `api/proxy_test.go`, `cmd/spoond-backend/main.go`, `identity/store.go`, `api/users.go`, `docs/usage.md`, `docs/api.md`, `deploy/README.md`, and the epic plan `docs/plans/2026-08-11-multi-user-tenancy-plan.md`; grepped callers of `lookupByName`/`lookupAny`/`Owner`; checked git state and the uncommitted T1/T2 diff.
- **Loaded skill:** `homelab-auth-architecture` (grounds the Authelia v4.39.20 deployment, session/access-control config, and the fact that Caddy forward-auth was previously skipped — this is the first such deployment).
- **Key findings:** proxy listener is plain HTTP with zero auth (lease id in hostname = capability); LLM gateway + `/assets/` share the listener as guest capability paths (must stay exempt); T1 identity store is already in the working tree (`identity.Store.UserByName` is the exact hook the proxy needs); `Lease.Owner` will hold user IDs.
- **Files created/modified:** none (research only, per instructions). Web search was unavailable (no Firecrawl key), so the Authelia header contract is from the skill + known v4.39 behavior — flagged `verify on live Authelia` in the memo.
- **Issues:** none blocking; noted Caddy WS/handle_response and DNS-wildcard-depth as verification items.