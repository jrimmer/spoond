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


- **RESOLVED (2026-09-30):** vm2 SSH "host key changed" was **DNS misdirection**, not a re-key. Public DNS for `vm2.lacy.casa` began resolving to `5.78.185.36`, a foreign host presenting rotating keys (`8OdOGujd…` then `f0e74e1a…`, both confirmed *not* vm2 by the human). The real vm2 is `10.1.0.11` (per the human; matches `01-architecture.md` `HOST_PRIMARY_IP`) and serves the **original** host keys (ed25519 `SHA256:3fd5df51…`, ecdsa `SHA256:a480c83a…`). Fixed on the workstation: `~/.ssh/config` now pins `Host vm2.lacy.casa → HostName 10.1.0.11`, and `10.1.0.11`'s keys were added to `known_hosts`. Verified: `ssh root@vm2.lacy.casa` → `sandbox`, `x86_64`, `active` ×4. No unit command ever ran against the foreign host; vm2 state untouched throughout. Suggested (human, non-blocking): fix the public DNS record for `vm2.lacy.casa`.

## Notifications

- 2026-09-30 Resumed: vm2 blocker was DNS misdirection to a foreign host; address pinned to 10.1.0.11 in ssh config. U01 steps 9a/10 re-dispatched.
