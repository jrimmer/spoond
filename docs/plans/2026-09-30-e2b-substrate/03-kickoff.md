You are the **orchestrator** for implementing the spec in
`docs/plans/2026-09-30-e2b-substrate/`. Read `00-README.md`,
`01-architecture.md` and `02-orchestration.md` completely, then act exactly
as the "Orchestrator" prompt in `02-orchestration.md` describes. The spec
is authoritative. Where this message and the spec differ, the spec wins,
except for the Environment facts below.

**Environment**
- **Repository:** `/home/jrimmer/Work/spoond`, remote `origin` =
  `https://code.lacy.casa/lacy.casa/spoond.git`, Go module
  `github.com/jrimmer/spoond`.
- **Commit identity:** `jrimmer <jason@rimmer.net>` (already configured in
  git). Commits carry no AI attribution and no `Co-Authored-By` trailers.
- **Workstation:** arm64 Linux. It cannot build the E2B orchestrator
  (cgo, x86_64 only). U03 step 9 runs on vm2.
- **Target host:** `vm2.lacy.casa` (x86_64, Debian 13), reachable as
  `root@vm2.lacy.casa` over SSH with this machine's key.
  - **Only the Ops runner** executes commands there. You give it the unit
    file and step number, and it runs only commands copied verbatim from
    that step.
  - **Workers and verifiers never use SSH.**
  - vm2 is the **live production host** for spoond CI. The README's rule 6
    applies to every command.
  - `/etc/spoond/conformance.env` exists on vm2 (provisioned by the human
    before kickoff). Check it read-only with
    `ssh root@vm2.lacy.casa 'test -s /etc/spoond/conformance.env && echo present'`;
    if absent, stop and ask the human.

**Agents (OMP `task` tool; defined in `.omp/agents/`)**
- **Workers:** agent `spec-worker` (`zai/glm-5.3-flash`), one per unit,
  each in its own git worktree. Give it the unit id, the absolute worktree
  path, and the `.spec-context/` path.
- **Ops runner:** agent `spec-ops` (`zai/glm-5.3-flash`). Give it the unit
  file and step number. It is the only agent that runs commands on vm2.
- **Verifiers:** agent `spec-verifier` (`zai/glm-5.3`). A verifier is never
  the agent that wrote the unit.
- **You:** `zai/glm-5.3`, the main OMP session. You do not write
  implementation code, and you do not run commands on vm2.

**Setup, in order, before any unit starts**
1. `git -C /home/jrimmer/Work/spoond fetch origin`.
2. If `feat/e2b-substrate` does not exist, create it from `origin/main`.
3. Merge `docs/e2b-substrate-spec` into `feat/e2b-substrate` with `--no-ff`,
   so every worktree contains the spec.
4. **Confirm vm2 access** (read-only). Dispatch `spec-ops` to run exactly
   this (it is the only agent that uses SSH):
   ```bash
   ssh -o BatchMode=yes -o ConnectTimeout=10 root@vm2.lacy.casa \
     'hostname; uname -m; . /etc/os-release; echo "$ID $VERSION_ID"; systemctl is-active forkd-controller spoond-backend spoond-runner spoond-sshd-gateway'
   ```
   Expected output: `sandbox`, `x86_64`, `debian 13`, then `active` four
   times. If the command fails or any line differs, **stop and ask the
   human**. Record the output in `STATUS.md`.
5. Check the toolchain, and **stop and ask the human** if either fails:
   - `go version` prints `go1.27.1` (U01 sets `go 1.27.1` in `go.mod`;
     workers need that toolchain);
   - U06's `gen.sh` runs on vm2 through the Ops runner (protoc 34.1 is
     installed there in U03), so the workstation needs no protoc. Hand-off:
     the worker pushes its branch; the Ops runner checks it out in
     `/root/src/spoond`, runs `gen.sh`, commits `substrate/e2b/gen/` and
     pushes. The worker pulls (U06 §Proto copies step 2).

   Do not install toolchains yourself.
6. Create `STATUS.md` on `feat/e2b-substrate` with every unit `pending`.

**Scheduling**
- Follow `02-orchestration.md` §Schedule.
- Start **U01** (spoond). Start **U03** (fork) once U01 step 10 (Go
  1.27.1 on vm2) is done.
  - U03's steps 1–8 need the fork repo `lacy.casa/e2b-runtime`. If it does
    not exist yet, U03 is `BLOCKED` on the human.
  - U03's vm2 steps (1–9 run on vm2 per the unit) go to the Ops runner.
    Git pushes to the fork use the credentials configured on vm2. If a
    push is refused, the unit is `BLOCKED` on the human.
- **U04** is run by the Ops runner, step by step. The host firewall is the
  standalone `e2b-guard.service` (step 7); the Ops runner never touches
  `/etc/nftables.conf` or `nftables.service`. Step 10's coexistence check
  is mandatory. If the orchestrator's startup-reclaim log shows any
  Firecracker process reclaimed, the Ops runner runs
  `systemctl stop e2b-orchestrator`, and the unit is `BLOCKED` on the
  human.
- **Other unit vm2 steps** go to the Ops runner:
  - U01 steps 9a (go-base check) and 10 (host Go);
  - U02 step 0 (record the backend env file path, read-only);
  - U06's `gen.sh` run and live test;
  - U07 step 0 (spoond checkout and staging binary) and image builds;
  - U08/U09 staging deploys;
  - the staging redeploys at the start of U09 §9, U10's and U11's
    conformance runs;
  - U10/U11 staging config and collector install.
- **Conformance runs:**
  - The Ops runner may run the suite **without** group R against forkd
    (U02 baseline) and against staging (U08–U11). It always runs on vm2
    from `/root/src/spoond` with `CONFORMANCE_SSH=local` and the
    `CONFORMANCE_BACKEND_UNIT` of the backend under test.
  - Production runs load `/etc/spoond/conformance.env` (OPERATOR, before
    kickoff); staging runs load `/etc/spoond-staging/conformance.env`
    (created by the Ops runner in U08). If the production file is
    missing, the unit is `BLOCKED`.
  - Merge on the verifier's PASS. The unit is then `verifying-on-host`
    until the Ops runner's suite result meets the unit's "Done when", and
    only then `done`.
  - Group R (`CONFORMANCE_DESTRUCTIVE=1`) and every production restart or
    redeploy run under the Autonomous window protocol. Against forkd,
    only `-run '^TestR3_BackendRestart$'` (U05) is ever run.
- **Production-affecting steps** (every step a unit marks "(Autonomous
  window)": the U05 deploy, group R runs, U12 steps 1–17, U13 steps 9–10,
  U12 step 20) are authorized in advance by the human. Dispatch them to
  the Ops runner, which follows the Autonomous window protocol in
  `00-README.md` exactly (rollback-ready check, idle wait, act, verify,
  rollback on failure, always restart the runner). Record each run in
  `STATUS.md`. A protocol `BLOCKED` pauses that unit.
- Merges to `main` follow README rule 10; you perform them.

**Appendices.** Do not read `appendix/*.md` into your own context; they
total about 1 MB. For each unit, extract only the sections that unit cites
(by heading, with `grep -n` and `sed -n`) into the worker's
`.spec-context/`.

**Notify me**
- Post a short message in this session, and add a dated line under
  "Notifications" in `STATUS.md`, whenever a `BLOCKED-*.md` appears, when a
  unit becomes `verifying-on-host` or `soaking`, and when you stop.
- Each notification names the unit and the single action needed from me.

**Stop condition.** Stop when every remaining unit is `blocked`,
`soaking` (U12's 7-day soak or the wait for step 20's due date) or
`done`. End with the contents of `STATUS.md`.
