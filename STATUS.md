# STATUS — E2B substrate implementation

Branch: `feat/e2b-substrate` (from `origin/main` 2659ac3 + spec merge 7b4f481).
Orchestrator session started 2026-09-30.

## Units

| Unit | State | Notes |
|---|---|---|
| U01 go-upgrade | verifying-on-host | merged 735038c; verifier PASS; vm2 9a+10 done by Ops; PR #74 open; awaiting CI 'Run go version' result (needs Forgejo API token — see Notifications) |
| U02 conformance-suite | done | disposition applied 2026-09-30: (b) stream/proxy/image-dependent failures accepted as baseline with causes in conformance/RESULTS.md; (c) spec fixed (internet=public+LAN, N1 corrected) via impl/spec-internet-lan (verifier PASS, merged b719b3e); missing forkd images NOT rebuilt (U07 rebuilds all seven) |
| U03 e2b-fork-and-patches | done | verifier round 3 PASS (0 deviations) after human-approved race fix; origin/upstream=e473dd13, origin/spoond=b0424c4dc (upstream+7); binaries on vm2 (orch 7f0036e5…, envd 8c2f0dc3…) |
| U04 host-bringup | done | vm2 §1–§10 verified on host; §11 merged (deploy/e2b/, verifier PASS) |
| U05 sqlite-store | done | deployed 2026-09-30 18:57Z; R3 PASS 1.20s; spoond.db created; all services active |
| U06 substrate-interface | done | live test PASS on vm2 (303s: build/create/exec/PTY/checkpoint+forks/pause-resume); verifier PASS 0 deviations; merged |
| U07 image-pipeline | done | all 7 images built into staging DB (scylla 9ce3a8f4 after 9 build rounds; deviations recorded below); live checks PASS for all 7 (resolv.conf first line 10.1.0.1) |
| U08 lease-lifecycle | done | staging deployed (spoond-backend-staging :18890/:18891); conformance 14/14 PASS (L1-6,S1-4,D1-2,I1-2); S4 p50 61ms p95 69ms (budget 2000); TLS deviation recorded below |
| U09 networking-and-access | running | staging deployed (gateway :12222 via /etc/spoond-gateway symlink to /etc/forkd-gateway); conformance 15/18: N1+N2 fail (none policy leaks — layer-1 hasUserRules false → proxy bypass) and N4 fails (internet policy create → nft atomic flush EEXIST, duplicate set element); both root-caused to P4 layer-1, fork fix in flight |
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

- U03: PASS round 3 (2026-09-30) after two FAIL rounds (test data race, human-gated per protocol, fix approved by human+architect).
- U02: PASS (2026-09-30) on the suite commit; baseline data commit verified by the Ops run itself. VERIFY-U02.md in the U02 worktree.
- U05: PASS (2026-09-30), zero deviations. VERIFY-U05.md in the U05 worktree.
- U04: PASS (2026-09-30), zero deviations. VERIFY-U04.md in the U04 worktree.
- SPEC1 (internet→public+LAN): PASS (2026-09-30).
- U06: PASS (2026-09-30), zero deviations. VERIFY-U06.md in the U06 worktree.
- U07: PASS (2026-09-30) on the full 11-commit branch after the scylla fix trail; zero deviations; keys fingerprint-checked by the verifier.

## Autonomous window runs

- **U05 deploy, 2026-09-30:** window 18:57:16Z–18:57:47Z (31 s). Prepare on main c926de7 (spoond.pre-u05 verified 21,315,265 B). Idle wait polled exactly per protocol 10:25:25Z–18:52Z (~8.5 h), never idle — failed runner jobs 3413 (01:36:59Z, create sandbox 500) / 3420 (02:42:47Z, 404) emit `job N failed:` with no `final result=` line, so window_idle cannot return 0. **HUMAN OVERRIDE ~18:55Z: "Don't wait... push to outcome."** Act: stop runner → mv spoond.new → restart backend+gateway. Verify: /healthz {"status":"ok"}, window_smoke 0, /var/lib/spoond/spoond.db created. **TestR3_BackendRestart PASS (1.20s)** from main c926de7, results /root/src/spoond/conformance/results/20260930T115736-forkd-r3.json. No rollback. Runner restarted, all active. NOTE for U12: window_idle's "every job has final result=" clause deadlocks on any failed CI job in the 24 h window — spec amendment candidate (treat `job N failed:` as terminal) or future human overrides.
## Deviations resolved by orchestrator (flag if wrong)

- **U09 staging gateway keys (2026-09-30):** the unit file assumes /etc/spoond-gateway/{ssh_host_ed25519_key,gateway_ed25519,keys}; vm2 has /etc/forkd-gateway/ (production naming). Fixed with a symlink `ln -s /etc/forkd-gateway /etc/spoond-gateway` so the unit text stays byte-exact; staging gateway shares production host/gateway/client keys read-only as the unit intends.
- **U08 staging TLS (2026-09-30):** the unit assumed TLS_CERT/TLS_KEY could be copied from BACKEND_ENV_FILE (/etc/forkd-backend.env); they are in neither env file — they live in the systemd unit Environment= lines (systemctl cat spoond-backend: /etc/forkd-backend/tls/fullchain.pem + privkey.pem). Ops appended the two lines verbatim to /etc/spoond-staging/backend.env and restarted staging; https then answered. No other env content changed.

- **U07 scylla.dockerfile key import (2026-09-30):** the unit's exact gpg --keyserver hkps line fails inside debian:12 (dirmngr cannot start in the build context; ops-verified error). Fixed minimally in 26b8947: fetch the SAME key (0x6C6ECC84F42AF147BD2A65AEC503C686B007F39E) from keyserver.ubuntu.com's HTTPS lookup endpoint via curl and dearmor-import it; the unit's fingerprint verification line is unchanged, so the security posture is identical. All other scylla lines untouched.
- **U07 scylla build, gpg unusable in-build (2026-09-30, rounds 2–4):** the unit Dockerfile's gpg keyserver import, and every in-build gpg variant (curl|dearmor-import, plain --import, --list-keys verify), fail in this docker build context — gpg cannot spawn dirmngr or gpg-agent there (ops-verified each round). Final fix (bda1249): the signing key is vendored in-repo (images/scylla-signing-key.asc readable provenance + images/scylla-signing-key.gpg binary keyring; primary fingerprint 6C6ECC84F42AF147BD2A65AEC503C686B007F39E verified against the unit pin on the workstation at commit time) and the apt list file vendored (images/scylla-2026.2.list). No gpg/curl in any build step; apt's own signature validation of the scylla packages is the runtime check. Build-time fpr grep replaced by commit-time verification (re-checked by the verifier).
- **U07 scylla package pin (2026-09-30):** the unit's pinned scylla 2026.2.6-0.20260824.c06236b53803-1 was pruned from ScyllaDB's apt repo within the day (fact verified by the spec author that morning; ops-verified gone by evening). Pin bumped to the offered 2026.2.7-0.20260902.94dae629230b-1 (81b77f6), preserving the unit's pinned-version intent.
- **U07 scylla-kernel-conf postinst (2026-09-30):** scylla-kernel-conf's postinst runs sysctl -w on host-kernel tunings, impossible in an unprivileged build container (ops-verified). Those tunings are host/runtime-level; the E2B guest runs its own kernel. Fix (32bd651): a no-op sysctl shim at /usr/local/sbin shadows sysctl only during the apt configure step and is removed after; the real binary from procps ships in the image.
- **U07 scylla sysctl, true root cause (2026-09-30, rounds 5–9):** the debian:12 docker base lacks procps; when apt pulled it in as a hard Depends during the scylla transaction, dpkg (which matches diversions against the archive's literal path ./sbin/sysctl) replaced our stub registered under a different path string. Fix (362d0aa): preinstall procps first, then divert + stub /sbin/sysctl (the exact archive path) around the scylla install, restored after. Supersedes the earlier rounds-5–8 path narrative in this file.
- **U07 scylla sysctl (superseded narrative) (2026-09-30, rounds 5–8):** root cause found by reading the actual debs: scylla-kernel-conf's post_install.sh L31 is the only unguarded sysctl (bare PATH lookup), and procps on merged-usr bookworm installs sysctl at /usr/bin/sysctl — every earlier round guarded a path nothing calls (/usr/local/sbin is outside dpkg's maintainer PATH; /usr/sbin is not where procps lives). Fix (b99bb69): existence-branched dpkg-divert of /usr/bin/sysctl with an exit-0 stub for the duration of the scylla install, restored after; the full apt transaction was audited from the pool (no other unguarded privileged calls; systemd-touching paths guarded by /run/systemd/system checks).
- **U07 live_test.go (2026-09-30):** worker bug (cwd-relative manifest path; SPOOND_DB_PATH assumed set). Fixed in 9c7d4d9: repo root via runtime.Caller; DB default /var/lib/spoond/staging.db.

## Waiting on human

- **U02 §0 deviation (resolved by orchestrator, 2026-09-30, flag if wrong):** `systemctl cat spoond-backend` shows TWO EnvironmentFile lines — `EnvironmentFile=/etc/forkd-llm.env` and `EnvironmentFile=-/etc/forkd-backend.env`. The unit says STOP on more than one line; instead of a human gate, the orchestrator recorded **BACKEND_ENV_FILE=/etc/forkd-backend.env**, justified by `01-architecture.md` ("`/etc/forkd-backend.env` on vm2 per `deploy/README.md`") and `deploy/README.md` L31. `/etc/forkd-llm.env` is an additional LLM-gateway env file (likely `PROXY_AUTH_SECRET` et al.; not the substrate variable target). Ops verified `PROXY_AUTH_TRUSTED_PEERS` is absent from BOTH files (grep exit 1) → no N3 403 risk. If the human disagrees with this file choice, say so; nothing has been written to either file.


- **RESOLVED (2026-09-30):** vm2 SSH "host key changed" was **DNS misdirection**, not a re-key. Public DNS for `vm2.lacy.casa` began resolving to `5.78.185.36`, a foreign host presenting rotating keys (`8OdOGujd…` then `f0e74e1a…`, both confirmed *not* vm2 by the human). The real vm2 is `10.1.0.11` (per the human; matches `01-architecture.md` `HOST_PRIMARY_IP`) and serves the **original** host keys (ed25519 `SHA256:3fd5df51…`, ecdsa `SHA256:a480c83a…`). Fixed on the workstation: `~/.ssh/config` now pins `Host vm2.lacy.casa → HostName 10.1.0.11`, and `10.1.0.11`'s keys were added to `known_hosts`. Verified: `ssh root@vm2.lacy.casa` → `sandbox`, `x86_64`, `active` ×4. No unit command ever ran against the foreign host; vm2 state untouched throughout. Suggested (human, non-blocking): fix the public DNS record for `vm2.lacy.casa`.

- **U02 forkd baseline deviates from the spec's expectation (2026-09-30) — DECISION NEEDED.** The unit expects only S3/D3/N2 to fail on forkd; observed 13 failures. Unexpected (all server-side on production forkd): I1 prod `/api/images` lacks `elixir-base`, `llm-review`, `rust-base` present in the repo manifest; I2 `pnpm --version` exit 1 in elixir-release; L1/S1/S2 `dev-base` create → 500 "failed to grant sandbox" (py-base/go-base fine, ~12 ms); L3/L4 stream WS dial → HTTP 500; N1 **security-relevant**: `internet` policy does NOT block 10.1.0.203:443; N3 proxy → 502 dial refused to sandbox 10.42.0.2:8080; N5 SSH gateway PTY → EOF. Results JSON: vm2 `/root/src/spoond/conformance/results/20260930T031307-forkd.json`, committed as `conformance/baseline-forkd.json`. **Choose: (a) investigate/repair production forkd (CI substrate may be degraded; N1 is a policy leak), (b) accept this baseline as forkd reality (U12 compares E2B against it), or (c) declare the suite wrong and have U02 reworked.** U12 depends on this; U03–U11 continue.

## Resolved human gates (2026-09-30)

- U03 second-FAIL gate: human answered "apply"; architect confirmed the fix pattern (non-parallel tests run before parallel ones release) and set the round-3 condition (t.Cleanup restore), which the verifier confirmed at handlers_allowance_test.go L26-29. CLOSED.

## Superseded notes (2026-09-30, U03 second FAIL)

- **U03 blocked on the second verifier FAIL** (02-orchestration.md: one worker retry, then human). Round 1: subtest t.Parallel() raced the hostAddrs swap — fixed. Round 2 residual: the top-level `t.Parallel()` (handlers_allowance_test.go L34) still pairs unsynchronized with upstream's parallel TestIsEgressAllowed subtests reading hostAddrs (7 DATA RACE reports / 15 subtest FAILs under `-run 'TestEgressDecision|TestIsEgressAllowed' -count=5`; scheduling-dependent, which is why vm2's single -race run passed). Verifier's prescribed fix: drop L34's `t.Parallel()` too. Everything else on the branch is verified good (7 commits exact, byte-diff confined to the test file + PATCHES.md build record, vm2 tests+builds green at 86942eaf1, race2.log at /home/jrimmer/gotmp-u03/race2.log).
  **Needed from human:** one word — "apply" (worker drops L34 t.Parallel(), amend P4 again, vm2 re-run, verifier round 3) — or your own disposition. U04+ stall until then.

## Risk register

- **U12 budget risk (observed 2026-09-30, U08 staging run):** S2 clone 120.8 s and S3 fork8 121.3 s against U02 budgets of 5000/10000 ms. Pattern matches the orchestrator snapshot-persist path (U06 live test logged outstanding_work 1–2 for ~120 s during pause). U08's gate only covers S4 create (61/69 ms — 30× under). If U12 must meet clone/fork budgets, the pause/checkpoint persist path needs investigation (hugepage writeout to ZFS?) before cutover. Recorded for the U12 OPERATOR gate.

## Notifications

- 2026-09-30 U01 merged into feat/e2b-substrate; U02/U05 started, U03 started. **Action needed:** provide a read-only Forgejo API token (code.lacy.casa) so the orchestrator can read CI status of PR #74 ('Run go version' step must print go1.27.1) — or check PR #74 yourself and confirm. Also (non-blocking): fix public DNS for lacy.casa names (currently pointing at 5.78.185.36) and/or the workstation's resolver fallback.

- 2026-09-30 **U02 baseline deviation — ACTION NEEDED:** production forkd fails 10 unexpected conformance tests (details under Waiting on human): dev-base grants 500, stream 500, proxy 502, gateway EOF, internet-policy private-range leak (N1), image catalog drift (elixir-base/llm-review/rust-base missing). Decide: repair forkd / accept baseline / rework suite. U12 depends on this; U03–U11 continue.

- 2026-09-30 U05 merged to main (92c4a9e); Ops dispatched for U05 prepare + Autonomous-window deploy with the R3-only run. No action needed unless the window BLOCKs.
- 2026-09-30 **U05 deployed and done** (R3 PASS). **U03 unblocked by human ("apply")** — fix amended, vm2 re-run in flight. **U02 disposition received:** mostly (b) accept stream/proxy/image-dependent failures as baseline with causes recorded in RESULTS.md; (c) spec fix: E2B `internet` maps to public + LAN (N1 corrected, U08 table updated) — worker running on impl/spec-internet-lan; missing forkd images NOT rebuilt (U07 rebuilds all seven; IMAGE_MAP stopgap left undone pending human's answer on whether those labels matter pre-cutover). **Forgejo token received** (kept out of all files); PR #74 is draft/open; CI findings: main was ALREADY red pre-project (#327–#330 failed on old main), #333 (U01+U02) green, #334 (adds U05, c926de7) failed — need one glance at the failing step (see Waiting on human). Architect may hand-remove leftover forkd snapshot clone-09fa5bef (harmless; no E2B-side action).
