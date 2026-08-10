---
title: CFOS/Sandstorm on forkd - Plan
type: feat
date: 2026-08-08
artifact_contract: ce-unified-plan/v1
artifact_readiness: implementation-ready
product_contract_source: ce-plan-bootstrap
execution: code
origin: docs/plans/2026-08-08-001-feat-forkd-ephemeral-backend-plan.md
---

# CFOS/Sandstorm on forkd - Plan

**Target repo:** `lacy.casa/forkd-service` (at `forgejo-work/hyper-forgejo-runner/`)

## Goal Capsule

Move Cloudflare OS (CFOS, "Gadgets Workshop", running at `os.lacy.casa` on CT144) off Cloudflare Workers execution and onto forkd microVMs. Today CFOS runs gadget/agent code (`executeCode`) on a "restricted and heavily-sandboxed variant of Cloudflare Workers." This plan replaces that execution backend with forkd microVMs obtained from the forkd lease API, so gadget code runs in fast, isolated, ephemeral microVMs on our own hardware instead of Cloudflare's.

**Authority hierarchy:** the plan is the authority for scope and sequencing. The user (Jason) owns product decisions; the implementer owns execution details the plan leaves open.

**Stop conditions:** stop when CFOS's chat agent brain runs in/on forkd microVMs (obtained via the lease API) with forkd-service offering the CFOS driver the way it offers Shelly — CFOS configured (not patched) to use it, and the CFOS chat works end-to-end. Do not build the full exe.dev-style interactive dev environment in this plan — that is a separate follow-up.

**Tail ownership:** the implementer owns the code, tests, and deployment of the forkd-side CFOS driver. The user owns the decision to decommission the Cloudflare Workers execution path and any CFOS-side changes.

> **2026-08-10 RESEARCH UPDATE (drives this revision):** after code-level research into the CFOS codebase (workshop-backend `overseer.ts`, `agent.ts`, `ai-models.ts`, `server.ts`, wrangler config at `os.lacy.casa` = 10.1.0.55:8787):
> - CFOS is NOT a plain consumer of public Cloudflare Workers APIs. It runs on workerd with its own runtime extensions (`worker_loaders` is a non-standard wrangler key; capnweb RPC; `cloudflare:workers` module).
> - Its "interface to CF Workers" is really three seams: (1) the chat agent loop (`runAgent` in `agent.ts`), abstracted behind `AgentHooks` + `ModelHandle`; (2) code execution (`executeCode` tool → `executeCodeMode` → `LOADER.load(workerDef)`), a private dynamic-worker loader, synchronous, stateless per call; (3) persistent/async work: Gadgets (Durable Objects with SQLite, `ctx.restore()`, tails, alarms) and Gatekeepers (separate Workers holding credentials, reached via RPC stubs).
> - **The model routing is the driver socket.** `getModel()` in `ai-models.ts` has three modes including *direct provider access with the config's own `apiUrl`/`apiToken`* — CFOS already supports pointing a chat model at an arbitrary OpenAI-compatible endpoint with zero code change. The frontend's backend URL is also plain config (`VITE_BACKEND_HOST`).
> - **Conclusion: no CFOS patches are needed.** Earlier plan revisions (KTD1/KTD2, HTTP bridge, `executeCodeMode` forkd branch, `/api/forkd-bridge/*` endpoints) were implemented and then **reverted** (2026-08-10) because they put forkd-awareness inside CFOS — architecturally backwards. The preferred direction (user-confirmed, "B as a sort of CFOS driver") is: forkd-service *offers* the CFOS driver the way it offers Shelly — agent brain in a forkd microVM, LLM gateway + attach hosted by forkd-service, OpenAI-compatible endpoint, keys off-VM, thin images — and CFOS config-only points at it.


## Product Contract

### Summary

CFOS is a self-hosted platform for "vibe coded" personal applications and AI agents that run inside a strong sandbox. It runs on CT144 (10.1.0.144) as a pnpm service (`cfos.service`), with the upstream repo at `github.com/cloudflare/cloudflare-os`. Gadgets and agent `executeCode` calls currently execute on a sandboxed variant of Cloudflare Workers. The forkd lease API (built in the prior plan) provides fast, isolated, ephemeral microVMs on vm2. This plan wires CFOS's code execution to forkd, keeping the CFOS frontend, chat, and gadget model intact while swapping the execution substrate.

### Problem Frame

CFOS's execution currently depends on Cloudflare Workers — a managed, external sandbox. The homelab has forkd microVMs (fast fork, warm pool, TTL-enforced leases) that provide the same isolation locally. Running CFOS gadget code on forkd keeps execution on-prem, removes the Cloudflare Workers dependency, and aligns with the homelab's "microVMs over Docker/cloud" preference. The forkd lease API is the shared contract; CFOS becomes another consumer alongside the Forgejo runner and command adapter.

### Requirements

**R1. forkd execution adapter for CFOS.** A component that takes a CFOS `executeCode` request (code + bindings) and runs it in a forkd microVM obtained from the lease API, returning stdout/stderr/exit to the CFOS chat.

**R2. Preserve CFOS gadget model.** The CFOS frontend, chat, gadget identity, and bindings model remain unchanged. Only the execution substrate swaps from Workers to forkd.

**R3. Bindings/context mapping.** CFOS's `env` bindings (gatekeepers, gadgets, external resources) must be made available to code running in the forkd microVM, or explicitly mapped to a forkd equivalent.

**R4. Lease lifecycle.** Each `executeCode` run obtains a sandbox lease (image + TTL), executes, and releases. No orphaned microVMs.

**R5. Auth.** The forkd lease API is token-authenticated; the adapter must present a valid consumer token.

**R6. Image selection.** The adapter selects a forkd image tag appropriate for the gadget/agent code (e.g. a JS/TS-capable image for Workers-style code, or a language base).

### Scope Boundaries

**In scope:** the forkd execution adapter for CFOS, the lease lifecycle, image selection, and wiring it into CFOS's `executeCode` path.

**Deferred to Follow-Up Work:**
- The exe.dev-style interactive dev environment (ssh/mosh into a persistent forkd microVM) — separate plan
- Full CFOS Workers feature parity (Workers logs, R2, Durable Objects semantics) — only what `executeCode` needs is in scope
- Migrating CFOS's own hosting off CT144

**Outside this product's identity:** modifying forkd itself (upstream, we consume it); the Cloudflare Workers platform.

## Planning Contract

### Key Technical Decisions

**KTD1. Driver, not adapter — forkd-service offers the CFOS brain the way it offers Shelly.** A new forkd-service component (e.g. `cmd/cfos-driver`) runs an agent brain in a forkd microVM (image attach + LLM gateway, keys off-VM), and exposes an OpenAI-compatible endpoint. CFOS points its existing model config at that endpoint — **no CFOS code changes** (the "config-only" requirement confirmed by `getModel()`'s direct-provider mode with `apiUrl`/`apiToken`). The driver's job is to look like a model to CFOS and like forkd to the sandboxes.

**KTD2. The driver speaks the CFOS agent protocol over the bridge.** CFOS chat messages arrive at the driver as OpenAI-compatible chat requests; the driver's agent (in-sandbox) uses tools (executeCode-in-forkd, read/write files, gatekeeper calls). The gatekeeper/gadget bindings bridge (KTD6) is the part of the driver that translates RPC stubs to HTTP calls **outbound from the sandbox back to CFOS** — reverse direction of the earlier (reverted) bridge. CFOS stays upstream-clean.

**KTD3. Reuse the command adapter pattern.** The existing `commandadapter` (synchronous `POST /v1/run` → stdout/stderr/exit) is the natural fit for in-driver `executeCode`. Extend or wrap it rather than inventing a new execution path.

**KTD4. Image per gadget language.** Gadget code is JS/TS (Workers-style). Bake a `js-base` (or reuse a node-capable image) for gadget execution; language bases (`go-base`, `elixir-base`) serve agent code that needs them.

**KTD5. `executeCode` is stateless — no persistent-sandbox mode needed.** Research (Cloudflare OS blog, Aug 5 2026, and the `executeCodeMode` implementation) confirms each `executeCode` call loads a fresh Dynamic Worker (V8 isolate), runs it once, returns a log string, and tears down. No state persists between calls. CFOS's persistent state (SQLite, conversation history, app state) lives in its own Durable Objects / storage, NOT in the execution sandbox. So the forkd batch lease model (create → exec → release) maps cleanly; the persistent-sandbox/suspend-resume work belongs to the separate exe.dev plan.

**KTD6. The gatekeeper bindings bridge is the core technical risk.** CFOS generated code receives resources as typed bindings (`const issues = await env.PROJECT.listIssues(...)`) that are RPC stubs to Gatekeepers (which hold credentials, enforce policy, mediate side effects). The forkd sandbox must (a) run JS (Node.js) and (b) reach the gatekeeper bindings via an HTTP/RPC bridge back to CFOS. This is the hard part of the driver — not "run code in a sandbox" but "run JS in a sandbox with access to CFOS's gatekeeper RPC."

**KTD7. Shelly is the template for the driver.** Shelly = agent in a forkd microVM, forkd-service hosts LLM gateway + attach, keys never enter the image, OpenAI-compatible endpoint exposed. The CFOS driver is the same shape with CFOS-specific tools; reuse the Shelly attach/gateway machinery rather than building a parallel mechanism.

### High-Level Technical Design

```mermaid
flowchart LR
    subgraph CFOS [CT144 - cfos.service (unpatched)]
        FE[Frontend]
        CHAT[Chat / Overseer runAgent]
        GK[Gatekeepers / Gadget DOs]
    end
    subgraph Driver [forkd-service cfos-driver]
        LLM[OpenAI-compat endpoint]
        AGT[Agent brain in sandbox]
        CA[Command Adapter /v1/run]
        BR[gatekeeper bridge → CFOS]
    end
    subgraph Backend [vm2 - forkd-backend]
        API[Lease API :8890]
        POOL[Warm Pool]
    end
    subgraph Host [vm2]
        CTRL[forkd-controller]
        SNAP[(snapshot tags)]
    end
    CHAT -- "model config (apiUrl)" --> LLM
    LLM --> AGT
    AGT --> CA
    AGT -- "RPC stubs over HTTP" --> BR
    BR --> GK
    CA --> API
    API --> POOL --> CTRL --> SNAP
```

**Chat flow (B1 + driver):**
1. CFOS chat turn → `getModel()` direct-provider route → OpenAI-compatible `apiUrl` = forkd cfos-driver endpoint (CFOS config only)
2. Driver receives chat messages; its in-sandbox agent runs the CFOS agent protocol (system prompt, tools)
3. Agent's tools execute in forkd microVMs (command adapter / lease API), stateless per call
4. Gatekeeper calls from the sandbox go back over the driver's bridge to CFOS's existing gatekeepers
5. Model stream returns to CFOS chat; CFOS storage/UX unchanged

### Assumptions

- forkd-backend lease API is live on vm2 at `https://vm2.lacy.casa:8890` (built in prior plan).
- CFOS runs on CT144 and can reach the adapter over the homelab network (internal traffic, not via Pangolin).
- Gadget code is JS/TS; a `js-base` image is baked for it.
- The adapter runs as a systemd service (plain Go binary, per homelab preference).

## Implementation Units

> **STATUS: ON HOLD (2026-08-10).** The user asked to pause implementation pending a decision on how much this matters vs. other forkd-service work. The units below are the revised plan (driver shape) — **do not implement until the user lifts the hold.** What was already built (U1/U2 adapter, U3 js-base bake) remains in the repo/deployed state and is reusable; the A-design CFOS patches were reverted.

### U1. CFOS driver — OpenAI-compatible model endpoint (replaces old "adapter")

**Goal:** forkd-service exposes an OpenAI-compatible chat endpoint (`cmd/cfos-driver`) that CFOS can point a model at via existing config (`AiModelConfig.apiUrl`/`apiToken`, direct-provider mode). The driver hosts an agent brain in a forkd microVM (Shelly-style attach + LLM gateway, keys off-VM) and speaks the CFOS agent protocol to its tools.

**Requirements:** R1, R2, R3

**Dependencies:** U3 (js-base image), Shelly attach/gateway machinery (reuse, per KTD7)

**Files:**
- `cmd/cfos-driver/main.go` (new)
- `cfos/driver.go`, `cfos/driver_test.go` (new)

**Approach:** OpenAI-compatible `/v1/chat/completions` (streaming) in front of an agent loop that runs in a forkd microVM. Chat messages + tool definitions come from CFOS; the agent executes tools (executeCode-in-forkd, file ops, gatekeeper calls via the KTD6 bridge). Each turn uses a lease (create → run → release); conversation state stays in CFOS (per KTD5).

**Patterns to follow:** Shelly integration (attach + LLM gateway), `commandadapter/server.go` (auth, response shape), pi-ai streaming shape (SSE, `text/event-stream`).

**Test scenarios:**
- Happy path: OpenAI-compatible request → driver → agent in sandbox → streamed reply.
- Tool path: agent calls `executeCode` → command adapter → stdout back in stream.
- Auth: missing/invalid token returns 401.
- Integration: CFOS chat pointed at driver (config only) completes a turn.

**Verification:** `go test ./cfos/...` passes; a live CFOS chat turn routed to the driver completes end-to-end.

### U2. Image selection for gadget/agent code

**Goal:** Select the correct forkd image tag for CFOS agent/gadget code based on language/capability.

**Requirements:** R6

**Dependencies:** U1

**Files:**
- `cfos/image.go` (new)
- `cfos/image_test.go` (new)

**Approach:** Simple label→image map, defaulting to `js-base` for Workers-style gadget code; language bases for agent code. Reject unknown capabilities with a clear error. (Unit-tested in the first pass — already committed as `cfos/image.go`.)

**Test scenarios:**
- Happy path: JS/TS → `js-base`; Go → `go-base`; Python → `py-base`.
- Edge case: unknown capability returns a clear "no image" error.
- Integration: a JS gadget executes in a `js-base` sandbox.

**Verification:** unit tests pass; a JS gadget executes end-to-end in a `js-base` forkd sandbox.

### U3. Bake `js-base` image

**Goal:** Bake a forkd `js-base` snapshot (Node.js + git) for Workers-style gadget code.

**Requirements:** R6

**Dependencies:** none

**Files:**
- `deploy/bake-js-base.sh` (exists from first pass — verify)

**Approach:** `forkd from-image node:22 --tag js-base --extra 'git ca-certificates curl'` (the `--extra` takes ONE quoted string). Already done + verified in the first pass: node v22.23.2 + git 2.39.5, `JS_BASE_OK`, registered in KNOWN_IMAGES.

**Test scenarios:**
- Happy path: `node --version` and `git --version` resolve in a spawned `js-base` sandbox.
- Edge case: rootfs size sufficient (use `--size-mib` if needed).

**Verification:** `forkd images` lists `js-base`; a spawned sandbox runs `node --version` and `git --version`.

### U4. Wire CFOS to the driver (config only) + gatekeeper bridge

**Goal:** Point CFOS's chat model at the forkd driver — **configuration only, no CFOS code changes.** Prove the gatekeeper binding bridge (KTD6) end-to-end: sandboxed code calls a gatekeeper over the driver's outbound bridge and gets a result.

**Requirements:** R1, R2, R3

**Dependencies:** U1, U2, U3

**Files:**
- `deploy/cfos-driver.service` (new, systemd unit)
- CFOS config: model entry with `apiUrl` = driver endpoint (no source changes)
- `cfos/bridge.go`, `cfos/bridge_test.go` (new) — outbound bridge: sandbox → CFOS gatekeepers

**Approach:** Add a model to CFOS config pointing at the driver's OpenAI-compatible endpoint. The driver's sandboxed agent calls gatekeepers via the outbound bridge (HTTP from sandbox → CFOS's existing gatekeeper RPC surface). Verify a CFOS chat turn that uses a gatekeeper binding completes.

**Patterns to follow:** `deploy/forkd-runner.service` (systemd unit pattern).

**Test scenarios:**
- Happy path: CFOS chat turn with a gatekeeper binding completes via driver + bridge.
- Error path: driver down → CFOS surfaces a clear error; no orphaned sandboxes.
- Integration: a real gatekeeper call (e.g. GitHub issues) returns data to the sandbox.

**Verification:** a live CFOS chat turn routed to the driver completes end-to-end; gatekeeper data returns; no orphaned microVMs.

## Deferred to Follow-Up Work

- exe.dev-style interactive dev environment (ssh/mosh into a persistent forkd microVM) — separate plan
- Full CFOS Workers feature parity (Workers logs, R2, Durable Objects semantics)
- Migrating CFOS hosting off CT144
- CFOS Gadget execution on forkd (persistent DOs) — beyond the chat-agent driver scope of this plan

## Open Questions

- **RESOLVED (2026-08-10): Does the integration require CFOS patches?** No. CFOS's `getModel()` direct-provider mode accepts arbitrary OpenAI-compatible `apiUrl`/`apiToken`; the frontend backend URL is config (`VITE_BACKEND_HOST`). The driver shape is config-only for CFOS. (This supersedes the earlier "HTTP bridge into executeCodeMode" design, which was reverted.)
- **RESOLVED: Does `executeCode` need Durable Objects semantics?** No — each call is stateless (fresh Dynamic Worker, run once, tear down). Persistent state lives in CFOS's own Durable Objects/storage, not the sandbox. (KTD5)
- **Which CFOS bindings must be available in the sandbox for v1?** The gatekeeper bindings (env.PROJECT, etc.) are the core requirement. The minimal v1 should support at least one gatekeeper binding end-to-end to prove the bridge (KTD6).
- **PENDING (user decision):** whether the CFOS driver is worth building now vs. other forkd-service work. Implementation is on hold until then.
