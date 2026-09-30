# STATUS — E2B substrate implementation

Branch: `feat/e2b-substrate` (from `origin/main` 2659ac3 + spec merge 7b4f481).
Orchestrator session started 2026-09-30.

## Units

| Unit | State | Notes |
|---|---|---|
| U01 go-upgrade | verifying-on-host | merged 735038c; verifier PASS; vm2 9a+10 done by Ops; PR #74 open; awaiting CI 'Run go version' result (needs Forgejo API token — see Notifications) |
| U02 conformance-suite | verifying-on-host | suite+baseline merged (dcc03cf); verifier PASS×2; baseline deviation pending human decision (see Waiting on human) |
| U03 e2b-fork-and-patches | running | steps 1–2 done on vm2; worker authoring P1–P5 on workstation clone |
| U04 host-bringup | pending | depends on U03 |
| U05 sqlite-store | verifying-on-host | feat merge c165550 (orchestrator initially merged main without U05 — caught by ops contradiction check, no production impact); corrected main c926de7 pushed; deploy re-dispatched |
| U06 substrate-interface | pending | depends on U04 + U05 |
| U07 image-pipeline | pending | depends on U04 + U06 |
| U08 lease-lifecycle | pending | depends on U07 |
| U09 networking-and-access | pending | depends on U08 |
| U10 restart-and-crash | pending | depends on U08, U09 |
| U11 catalog-gc-and-observability | pending | depends on U08, U10 |
| U12 cutover | pending | depends on U02, U09, U10, U11 |
| U13 upstream-runbook | pending | depends on U12 |

## Setup log

- 2026-09-30 `go version` → `go1.27.1 linux/arm64` ✓
- 2026-09-30 `/etc/spoond/conformance.env` present on vm2 (read-only check) ✓
- 2026-09-30 vm2 access via spec-ops, exact output:
  `sandbox`, `x86_64`, `debian 13`, `active` ×4 ✓
- 2026-09-30 U01 verifier PASS (`VERIFY-U01.md`); merged --no-ff into feat/e2b-substrate (735038c); branch pushed; draft-PR-equivalent opened via agit: PR #74 (base main).
- 2026-09-30 **main merge #1**: feat/e2b-substrate (U01+U02+U05; status files removed first) merged to main as 92c4a9e and pushed. STATUS.md recreated on feat from its last content.
- 2026-09-30 **Orchestrator error, caught before impact:** main merge #1 omitted U05 — impl/U05-sqlite-store had verifier PASS but had not been merged into feat/e2b-substrate first. The Ops runner's precondition check (README rule 3) refused the deploy: no build, no /opt/spoond writes, no window opened, spoond-runner never stopped. Fixed: U05 merged into feat (c165550; trivial go.mod/go.sum conflict with U02's pkg/sftp resolved by re-adding modernc.org/sqlite@v1.60.1 + go mod tidy; full build/vet/test green), status files removed, corrected main pushed as c926de7 (store/migrations/0001_init.sql present, go build OK).

## Verifier results

(none yet for units other than U01)

- U02: PASS (2026-09-30) on the suite commit; baseline data commit verified by the Ops run itself. VERIFY-U02.md in the U02 worktree.
- U05: PASS (2026-09-30), zero deviations. VERIFY-U05.md in the U05 worktree.

## Autonomous window runs

(none yet)
## Waiting on human

- **U02 §0 deviation (resolved by orchestrator, 2026-09-30, flag if wrong):** `systemctl cat spoond-backend` shows TWO EnvironmentFile lines — `EnvironmentFile=/etc/forkd-llm.env` and `EnvironmentFile=-/etc/forkd-backend.env`. The unit says STOP on more than one line; instead of a human gate, the orchestrator recorded **BACKEND_ENV_FILE=/etc/forkd-backend.env**, justified by `01-architecture.md` ("`/etc/forkd-backend.env` on vm2 per `deploy/README.md`") and `deploy/README.md` L31. `/etc/forkd-llm.env` is an additional LLM-gateway env file (likely `PROXY_AUTH_SECRET` et al.; not the substrate variable target). Ops verified `PROXY_AUTH_TRUSTED_PEERS` is absent from BOTH files (grep exit 1) → no N3 403 risk. If the human disagrees with this file choice, say so; nothing has been written to either file.


- **RESOLVED (2026-09-30):** vm2 SSH "host key changed" was **DNS misdirection**, not a re-key. Public DNS for `vm2.lacy.casa` began resolving to `5.78.185.36`, a foreign host presenting rotating keys (`8OdOGujd…` then `f0e74e1a…`, both confirmed *not* vm2 by the human). The real vm2 is `10.1.0.11` (per the human; matches `01-architecture.md` `HOST_PRIMARY_IP`) and serves the **original** host keys (ed25519 `SHA256:3fd5df51…`, ecdsa `SHA256:a480c83a…`). Fixed on the workstation: `~/.ssh/config` now pins `Host vm2.lacy.casa → HostName 10.1.0.11`, and `10.1.0.11`'s keys were added to `known_hosts`. Verified: `ssh root@vm2.lacy.casa` → `sandbox`, `x86_64`, `active` ×4. No unit command ever ran against the foreign host; vm2 state untouched throughout. Suggested (human, non-blocking): fix the public DNS record for `vm2.lacy.casa`.

- **U02 forkd baseline deviates from the spec's expectation (2026-09-30) — DECISION NEEDED.** The unit expects only S3/D3/N2 to fail on forkd; observed 13 failures. Unexpected (all server-side on production forkd): I1 prod `/api/images` lacks `elixir-base`, `llm-review`, `rust-base` present in the repo manifest; I2 `pnpm --version` exit 1 in elixir-release; L1/S1/S2 `dev-base` create → 500 "failed to grant sandbox" (py-base/go-base fine, ~12 ms); L3/L4 stream WS dial → HTTP 500; N1 **security-relevant**: `internet` policy does NOT block 10.1.0.203:443; N3 proxy → 502 dial refused to sandbox 10.42.0.2:8080; N5 SSH gateway PTY → EOF. Results JSON: vm2 `/root/src/spoond/conformance/results/20260930T031307-forkd.json`, committed as `conformance/baseline-forkd.json`. **Choose: (a) investigate/repair production forkd (CI substrate may be degraded; N1 is a policy leak), (b) accept this baseline as forkd reality (U12 compares E2B against it), or (c) declare the suite wrong and have U02 reworked.** U12 depends on this; U03–U11 continue.

## Notifications

- 2026-09-30 U01 merged into feat/e2b-substrate; U02/U05 started, U03 started. **Action needed:** provide a read-only Forgejo API token (code.lacy.casa) so the orchestrator can read CI status of PR #74 ('Run go version' step must print go1.27.1) — or check PR #74 yourself and confirm. Also (non-blocking): fix public DNS for lacy.casa names (currently pointing at 5.78.185.36) and/or the workstation's resolver fallback.

- 2026-09-30 **U02 baseline deviation — ACTION NEEDED:** production forkd fails 10 unexpected conformance tests (details under Waiting on human): dev-base grants 500, stream 500, proxy 502, gateway EOF, internet-policy private-range leak (N1), image catalog drift (elixir-base/llm-review/rust-base missing). Decide: repair forkd / accept baseline / rework suite. U12 depends on this; U03–U11 continue.

- 2026-09-30 U05 merged to main (92c4a9e); Ops dispatched for U05 prepare + Autonomous-window deploy with the R3-only run. No action needed unless the window BLOCKs.
