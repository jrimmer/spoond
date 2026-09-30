# STATUS — E2B substrate implementation

Branch: `feat/e2b-substrate` (from `origin/main` 2659ac3 + spec merge 7b4f481).
Orchestrator session started 2026-09-30.

## Units

| Unit | State | Notes |
|---|---|---|
| U01 go-upgrade | blocked | local commit bb21513 on impl/U01-go-upgrade; vm2 steps 9a+10 blocked on host-key change (see Waiting on human) |
| U02 conformance-suite | pending | blocked behind U01 |
| U03 e2b-fork-and-patches | pending | starts after U01 step 10 |
| U04 host-bringup | pending | depends on U03 |
| U05 sqlite-store | pending | depends on U01 |
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
- 2026-09-30 U01 worker finished local steps (commit bb21513 on `impl/U01-go-upgrade`), parked on vm2 steps 9a/10 per protocol.

## Verifier results

(none yet)

## Autonomous window runs

(none yet)

## Waiting on human


- **vm2 SSH host key changed mid-session (2026-09-30).** The setup access check and the conformance.env check both passed against the stored key earlier in this session. Minutes later, every ssh to `root@vm2.lacy.casa` fails host-key verification. Offered ED25519 fingerprint now: `SHA256:8OdOGujdZElO17X+/NO5QXOsEOOwTVB2iN/am4oMgis`. Stored fingerprints (known_hosts lines 8–10): ed25519 `SHA256:3fd5df51…`, rsa `SHA256:942925d6…`, ecdsa `SHA256:a480c83a…`. Either vm2 was rebuilt/re-keyed, or this is a MITM. No unit commands ran on vm2 after the change; no vm2 state was touched.
  **Needed from human:** confirm the host key change is legitimate (and if so update known_hosts, or tell the orchestrator to accept `SHA256:8OdOGujdZElO17X+/NO5QXOsEOOwTVB2iN/am4oMgis`), or investigate. All vm2 steps (U01 9a+10, then U03/U04 and every later ops step) are paused until then.

## Notifications

- 2026-09-30 BLOCKED (all vm2 work): vm2 SSH host key changed mid-session. Action needed: confirm the new key `SHA256:8OdOGujd…` is legitimate (vm2 rebuild/re-key) or investigate as a security event; then tell the orchestrator to resume.
