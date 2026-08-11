# U4 — Agents as users (T3 / #30): design memo

> Epic #26 · parallel research (delegated agent, 2026-08-11) · input to U4 implementation.
> Scope: per-agent identity for MCP/ACP endpoints (KTD-1: separate keypair per agent).

---

---

# Design Memo: Per-Agent Identity for MCP/ACP (spoond U4 / T3-#30)

**Repo:** `/root/forgejo-work/hyper-forgejo-runner` (module `github.com/jrimmer/spoond`). Research only — no files modified.

## 1. How `FORKD_TOKEN` / `FORKD_BACKEND_URL` flow into lease creation today

**Client side (both servers are symmetric):**

- `cmd/spoond-dev-mcp/main.go` `Main()` (lines 40–48): reads `FORKD_BACKEND_URL` (default `https://127.0.0.1:8890`) and `FORKD_TOKEN` (required — `log.Fatal` if empty). Line 48: `sandbox := runner.NewHTTPLeaseClient(backendURL, token)` → line 50: `mcp.New(mcp.Config{Sandbox: sandbox, ...})`.
- `cmd/spoond-acp/main.go` `Main()` (lines 43–57): identical env reads; line 51 `runner.NewHTTPLeaseClient(backendURL, token)` → line 57 `acp.NewAgent(sandbox, llm, image, 1800, 12)` → line 59 `acp.New(acp.Config{Agent: agent, ...})`.
- `runner/lease_adapter.go` `HTTPLeaseClient` (lines 14–27) holds `BaseURL` + `Token`. Every call attaches `Authorization: Bearer <Token>`:
  - `Create()` (lines 30–53): `POST {BaseURL}/api/sandboxes` body `{image, ttl}`; expects 201, returns `id`.
  - `Exec()` (lines 56–77): `POST /api/sandboxes/{id}/exec` body `{cmd, cwd, env, timeout}`; expects 200.
  - `Delete()` (lines 80–95): `DELETE /api/sandboxes/{id}`; expects 204.

**Backend side:**

- `api/server.go` `authMiddleware` (lines 107–127): strips `Bearer `, looks up `owner, ok := s.svc.tokens[token]` (line 119), injects the **consumer name string** into the request context via `ctxOwnerKey{}` (line 124). `/healthz` and the `/llm/` prefix are exempt (line 109).
- `handleCreate` (lines 141–209): `s.svc.grant(ctx, ownerFrom(ctx), req.Image, req.MemoryMiB, ttl, req.Persistent, req.NetPolicy, req.NetAllow)` (line 195).
- `api/service.go` `Service.tokens` (line 87) is a flat `map[string]string` (token → consumer name). Populated in `cmd/spoond-backend/main.go` lines 63–77 from `CONSUMER_TOKENS` env (`token=consumer` comma-separated; `log.Fatal` if empty).
- **Lease ownership is set in exactly one place:** `api/service.go` `grant()` line 354: `Owner: owner` (the string from `ownerFrom(ctx)`). `handleCreate`'s TTL cap at lines 185–191; `grantFromSnapshot` (clone path, `api/server.go` line 885) is the only other owner-passing grant.

## 2. API calls each server makes

| Server | Calls (all via `runner.SandboxProvider` unless noted) |
|---|---|
| **MCP** (`mcp/server.go`) | Per tool call (`withSandbox`, lines 285–296): `Create` → `Exec` → `Delete` (deferred). Stateless v1: `POST /api/sandboxes`, `POST /api/sandboxes/{id}/exec`, `DELETE /api/sandboxes/{id}` — all bearer-token. |
| **ACP** (`acp/server.go` + `acp/agent.go`) | `session/new` → `SandboxAgent.NewSession` (`acp/agent.go` lines 249–251) → `Create` (one lease per session, held for session lifetime; `Close`/`Release` → `Delete`). Tools (`shell`/`read_file`/`write_file`, lines 124–194) → `Exec`. Plus **LLM calls**: `LLMClient.ChatCompletion` (`acp/agent.go` lines 81–103) → `POST {BaseURL}/llm/{leaseID}/openai/chat/completions` with **no auth header** — the lease id in the path is the capability, and the gateway is auth-exempt by design (`api/server.go` line 109, `llmGatewayPrefix` mounted in `NewServerWithLLM` lines 67–71). |

No server makes any identity/account API call today — there is no such endpoint yet (no `identity/` package exists; U1 is unimplemented).

## 3. Where per-agent identity plugs in (minimal change analysis)

**Key structural fact:** both servers are already hexagonal. `mcp.Server` and `acp.SandboxAgent` depend only on the `runner.SandboxProvider` port (`runner/ports.go` lines 65–69: `Create`/`Exec`/`Delete`). **Neither `mcp/server.go`, `acp/server.go`, nor `acp/agent.go` touches tokens at all** — the credential lives entirely in the `HTTPLeaseClient` constructed in the two `cmd/*/main.go` files. Lease ownership is derived server-side from whatever the bearer token resolves to. So the client half of U4 is a **main.go env-contract change + adapter credential change**; the server half is the U1 users store.

### Recommended approach

**Phase 1 — per-agent bearer token (the minimal T3 core, ~2 small diffs + U1 dependency):**

1. **Backend (U1 prerequisite, T1/#28):** replace the flat `Service.tokens map[string]string` (`api/service.go` line 87) with a users store: rows `{id, name, kind: person|agent, admin, token_hash, key_fingerprints}`. `authMiddleware` (`api/server.go` line 119) resolves token → **user id** instead of consumer-name string. Legacy `CONSUMER_TOKENS` entries keep working as implicitly-created `kind=person` users (KTD-3 backward compat). `grant()` line 354 then stamps `Owner` = the agent's user id with zero changes.
2. **Client env contract** (`cmd/spoond-dev-mcp/main.go` lines 42–48, `cmd/spoond-acp/main.go` lines 43–48): add `FORKD_AGENT_TOKEN` (per-agent, provisioned from the users store, e.g. `spoondctl ssh-key add`/token issuance per U1's `ssh-key` verb). Precedence: `FORKD_AGENT_TOKEN` wins; `FORKD_TOKEN` remains as legacy fallback (log a deprecation warning). Everything downstream is untouched: `NewHTTPLeaseClient` already sends the bearer token on create/exec/delete, and the backend resolves it to the agent.
3. **`runner/lease_adapter.go`:** no change for Phase 1. The `SandboxProvider` interface (`runner/ports.go`) needs no change either — identity is per-credential, not per-call.

**Phase 2 — per-agent keypair (Buzz-style, KTD-1 full):**

- Each agent gets its own Ed25519 keypair: private half held by the agent deployment (Buzz `agent_command_env`), public half registered on the agent's users row (reuse U1's `ssh-key add <pubkey> <user>` verb; the plan's `GET /api/users/by-key` (KTD-4) is the resolution endpoint).
- Add a signing-capable adapter in `runner/lease_adapter.go`: `NewHTTPLeaseClientWithSigner(baseURL, signer)` implementing the same `SandboxProvider`, attaching a per-request signature (e.g. `Authorization: Signature ...` or an `X-Spoond-Sig` + timestamp header, or exchange key→short-lived token via a new `POST /api/auth/token`). Backend verifies against the stored public key in the users store, resolving to the agent user id. This keeps `mcp`/`acp` packages credential-agnostic — the cmd just picks the adapter based on which env vars are set.
- **ACP LLM calls** (`acp/agent.go` `LLMClient.ChatCompletion`, lines 81–103) currently send no auth. Under per-agent identity this stays correct *only while* the LLM gateway remains capability-based (`/llm/{leaseID}/...`). U8 (T8/#35) adds owner validation in `llmgateway.go` (its `lookup func(leaseID) *Lease` is the hook); if per-user metering later demands authenticated LLM calls, `LLMClient` must carry the same agent credential — flag for U8, not U4.

**Recommended: do Phase 1 for U4/T3 (unblocks "two agents → disjoint leases" immediately), land Phase 2's keypair adapter in the same unit if U1's `ssh-key` verb ships in the same release** — the keypair path is what "Buzz-style" means per KTD-1, and both are small because the architecture already isolates credentials to one adapter constructor + one env read.

## 4. Concrete change list

| File | Change |
|---|---|
| `cmd/spoond-dev-mcp/main.go` (lines 42–48) | Read `FORKD_AGENT_TOKEN` (fallback `FORKD_TOKEN`); pass to `NewHTTPLeaseClient`; update doc comment (lines 8–21). |
| `cmd/spoond-acp/main.go` (lines 43–57) | Same env change; doc comment (lines 11–21) shows the old `FORKD_TOKEN=<consumer token>` contract. |
| `runner/lease_adapter.go` | (P2) add signer-aware constructor; keep `NewHTTPLeaseClient` for legacy. |
| `api/server.go` `authMiddleware` (line 119) | Resolve token via users store → user id (U1); inject `kind` too if admin checks need it. |
| `api/service.go` `tokens` (line 87), `grant` (line 354) | `tokens` map → users store (U1); `grant` unchanged (already stamps `Owner`). |
| `api/server.go` `NewServerWithLLM`/`handleCreate` (lines 195, 885) | No change — owner flows from context. |
| `mcp/server.go`, `acp/server.go`, `acp/agent.go` | **No change required** (port-based; credentials live in the adapter). Optionally thread an agent display name into `initialize`'s `serverInfo` for auditability. |
| `tests/integration/test_mcp.sh` (line 34), `test_acp.sh` | Add a second per-agent token fixture; assert disjoint leases / per-owner listing (U4 verification). |
| `docs/usage.md`, `docs/setup.md` | Update env contract (both currently document shared `FORKD_TOKEN`). |

**Lease-ownership answer (Q4):** `api/service.go` `grant()` line 354 (`Lease.Owner = owner`), fed by `ownerFrom(ctx)` (`api/server.go` lines 135–138), which today is the CONSUMER_TOKENS consumer name. The only code change needed for "sessions create leases owned by that agent" is making the token→owner resolution identity-aware (U1); the ownership stamping already works.

## Notes / adjacent findings

- `handleByName` (`api/server.go` lines 913–925) is **not owner-scoped** (`lookupByName` without owner filter) — a pre-existing gap U2 should close, relevant because agent names will be guessable.
- The SSH gateway (`cmd/spoond-sshd-gateway/main.go`) authenticates keys via a flat `loadAuthorizedKeys` set + `PublicKeyCallback` (lines 95–118) — no user mapping yet; this is where U1's key→user resolution lands and where the agent keypair model can reuse the same users store.
- KTD-3 (backward compat) is the reason to keep `FORKD_TOKEN`/`CONSUMER_TOKENS` as fallbacks rather than deleting them; the SSH gateway's own `--backend-token` (line 60) stays a service credential, not an agent credential.

---

## Summary

- **What I did:** Read all six target files plus `runner/lease_adapter.go`, `runner/ports.go`, `api/service.go`, `cmd/spoond-backend/main.go`, the tenancy plan, the SSH gateway auth path, `llmgateway.go`, and the MCP integration test. No files modified.
- **Key findings:**
  - Token flow: `cmd/*/main.go` reads `FORKD_TOKEN` → `runner.NewHTTPLeaseClient` → `Authorization: Bearer` on `POST /api/sandboxes`, `POST .../exec`, `DELETE ...` → `authMiddleware` maps token→consumer-name string via `Service.tokens` → `grant()` line 354 stamps `Lease.Owner`.
  - ACP additionally calls the auth-exempt `/llm/{leaseID}/openai/chat/completions` gateway with no credential (capability = lease id in path).
  - Neither `mcp/server.go`, `acp/server.go`, nor `acp/agent.go` touches credentials — they're port-based (`runner.SandboxProvider`), so per-agent identity is an adapter + main.go env change on the client side and a users-store resolution change on the backend (U1).
  - **Recommended:** Phase 1 per-agent bearer tokens (`FORKD_AGENT_TOKEN` with `FORKD_TOKEN` legacy fallback) resolving to `kind=agent` user ids; Phase 2 per-agent Ed25519 keypairs (Buzz-style/KTD-1) via a signer-aware `HTTPLeaseClient` variant and backend key verification, reusing U1's `ssh-key` verb and `GET /api/users/by-key`.
- **Files created/modified:** none (research only).
- **Issues:** None blocking; noted `handleByName` is not owner-scoped (adjacent U2 gap) and LLM-gateway auth is deferred to U8.