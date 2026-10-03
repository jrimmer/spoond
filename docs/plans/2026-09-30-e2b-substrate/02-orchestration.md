# 02 — Orchestrating unattended, parallel implementation

How to run this spec with coding agents: an **orchestrator**, one **worker**
per unit, an independent **verifier** per unit, and an optional **ops
runner** for host commands.

"Unattended" means *between human gates*. Every step marked OPERATOR in a
unit file needs a human. Agents run until they reach one of those gates, or a
STOP condition, and then park that lane.

## Roles and models

| Role | Model | Writes code? | Touches host? |
|---|---|---|---|
| Orchestrator | the strongest available | no (plans, merges to `feat/e2b-substrate` and `main`, dispatches the Ops runner, tracks status) | no |
| Worker (one per unit) | GLM-5.3-Flash or similar | yes, its unit only | no |
| Verifier (one per unit) | a different agent from the worker; preferably a stronger model | no | no |
| Ops runner | any | no | yes: only commands copied verbatim from a unit, and the Autonomous window protocol (`00-README.md`) for production steps |

Workers and verifiers never touch host (README rule 9); every host command
goes through the Ops runner.

## Schedule (from the Unit Index dependencies)

```
Lane A (spoond repo):  U01 ─┬─ U02 (conformance suite)
                            └─ U05 (SQLite store) ── orchestrator merges to main; Ops runner deploys (Autonomous window)
Lane B (fork + host):   U01 step 10 ── U03 ── U04
Join:                  U06 (needs U04 + U05 merged to main) → U07 → U08
Fan-out, then merge:   U09 ┐
                       U10 ├─ parallel worktrees; merge strictly U09 → U10 → U11,
                       U11 ┘  each later branch rebased and re-tested before merge
Autonomous windows:    U12 (steps 1–17; 18–19 are a worker; 20 after 30 days) → U13
```

- Start U01. U03 starts as soon as U01 step 10 (Go 1.27.1 on the host) is done.
  Then U02, U05 and U04 run in parallel.
- After U05 passes verification, the orchestrator merges
  `feat/e2b-substrate` into `main` (README rule 10) and has the Ops runner
  run U05's production deploy under the Autonomous window protocol (U05
  §Merge and production deploy). The orchestrator then rebases
  `feat/e2b-substrate` on `origin/main` before starting U06.
- U09, U10 and U11 all edit `api/service.go` and `api/server.go`. Run them
  in parallel only if the orchestrator enforces the merge order and a
  rebase plus full re-test before each merge. If a rebase needs non-trivial
  conflict resolution, the worker must write `BLOCKED-Uxx.md` rather than
  resolve behaviour conflicts on its own.
- U12 and U13 run like every other unit: code and doc steps by a worker,
  host steps by the Ops runner, production steps under the Autonomous window
  protocol. U12 step 20 runs 30 days after cutover if no rollback happened;
  the orchestrator records its due date in `STATUS.md`, and whoever is
  running then (the orchestrator via the Ops runner, or a human) executes
  it.

## Branches and worktrees

- The integration branch is `feat/e2b-substrate` in the spoond repo.
- Each unit gets `impl/<Uxx>-<slug>` from `feat/e2b-substrate`, in its own
  `git worktree`, e.g. `impl/U05-sqlite-store`.
- The orchestrator merges a unit branch into `feat/e2b-substrate` (with
  `--no-ff`) **only after its verifier returns PASS**.
- U03's work lives in the separate `example.com/e2b-runtime` repo (branches
  `upstream` and `spoond`), not in spoond.
- Only the orchestrator merges to `main` (README rule 10): once after U05,
  and once in U12 step 1, each only after the unit's worker tests, verifier
  PASS and required conformance result all pass.

## Status files (in the spoond repo root on `feat/e2b-substrate`; never merged to `main`)

| File | Written by | Content |
|---|---|---|
| `STATUS.md` | orchestrator | the table of units (state: pending, running, verifying, verifying-on-host, soaking, blocked, done), open blockers, OPERATOR steps waiting, the last verifier result per unit, every Autonomous window run (step, start, end, result, rollback if any), and the due date of U12 step 20 |
| `BLOCKED-<Uxx>.md` | worker | what blocked it, the file and line, expected vs found, and exactly what is needed to unblock |
| `DONE-<Uxx>.md` | worker | each "Done when" item, how it was verified, and test output |
| `VERIFY-<Uxx>.md` | verifier | PASS, or FAIL with numbered deviations |

Delete these files from the branch before each merge to `main` (after U05
and in U12). After the U05 merge, the orchestrator recreates `STATUS.md` on
`feat/e2b-substrate` from its last content.

## Guardrails

1. **Split host access.** Workers and verifiers get the repositories only.
   Commands on the host are executed by the Ops runner, copied verbatim from the
   unit.
2. **STOP means park.** On a false "Fact", a missing precondition, an
   OPERATOR step or a host command, the worker writes `BLOCKED-<Uxx>.md` and
   exits. The orchestrator pauses the unit's dependents and continues
   independent lanes.
3. **Independent verification.** The verifier never trusts `DONE-<Uxx>.md`.
   It re-runs the tests and checks the diff.
4. **Small context per worker.** A worker receives only:
   - `00-README.md`;
   - `01-architecture.md`;
   - its unit file;
   - the appendix sections that unit cites, extracted by the orchestrator,
     not the whole appendix files.
5. **No scope growth.** A change outside the files a unit lists is a
   verifier FAIL, unless the unit explicitly allows it (e.g. U08's test
   port).

## Prompts (use verbatim; fill in `<…>`)

### Kickoff (the human's first message to the orchestrating agent)

**Assumed setup.** Adjust the "Environment" block of the prompt if yours
differs.
- **Harness:** OMP (oh-my-pi, `omp`), with its `task` sub-agents. The
  project agents live in `.omp/agents/` in the spoond repo:
  - **Orchestrator:** the main OMP session, `zai/glm-5.3` (thinking `high`).
  - **Workers:** agent `spec-worker`, `zai/glm-5.3-flash` (thinking `high`).
  - **Verifiers:** agent `spec-verifier`, `zai/glm-5.3`. A different model
    from the workers, so verification is independent.
  - **Ops runner:** agent `spec-ops`, `zai/glm-5.3-flash` (thinking `low`).
    The only agent that uses SSH.

  None of the three agents may spawn further agents (`spawns: []`). The
  orchestrator creates the git worktrees itself with `git worktree add`, and
  gives each worker its worktree's absolute path.
- **SSH:** always the local key `~/.ssh/id_ed25519`, never an SSH agent.
  This is enforced in `~/.ssh/config`.
- **Where it runs:** the orchestrator and workers run on the operator's
  workstation, an **arm64** Linux machine with the spoond checkout at
  `/home/jrimmer/Work/spoond`.
- **host:** the orchestrating agent has SSH access as `root@spoond.example.com`.
  host steps, including the E2B fork's x86_64 cgo build, run through the
  **Ops runner**. Production-affecting steps run under the Autonomous
  window protocol (`00-README.md`), authorized in advance by the human.
- **Branches:** the spec is committed on branch `docs/e2b-substrate-spec`.

> You are the **orchestrator** for implementing the spec in
> `docs/plans/2026-09-30-e2b-substrate/`. Read `00-README.md`,
> `01-architecture.md` and `02-orchestration.md` completely, then act exactly
> as the "Orchestrator" prompt in `02-orchestration.md` describes. The spec
> is authoritative. Where this message and the spec differ, the spec wins,
> except for the Environment facts below.
>
> **Environment**
> - **Repository:** `/home/jrimmer/Work/spoond`, remote `origin` =
>   `https://git.example.com/example/spoond.git`, Go module
>   `github.com/jrimmer/spoond`.
> - **Commit identity:** `jrimmer <jason@rimmer.net>` (already configured in
>   git). Commits carry no AI attribution and no `Co-Authored-By` trailers.
> - **Workstation:** arm64 Linux. It cannot build the E2B orchestrator
>   (cgo, x86_64 only). U03 step 9 runs on the host.
> - **Target host:** `spoond.example.com` (x86_64, Debian 13), reachable as
>   `root@spoond.example.com` over SSH with this machine's key.
>   - **Only the Ops runner** executes commands there. You give it the unit
>     file and step number, and it runs only commands copied verbatim from
>     that step.
>   - **Workers and verifiers never use SSH.**
>   - host is the **live production host** for spoond CI. The README's rule 6
>     applies to every command.
>   - `/etc/spoond/conformance.env` exists on the host (provisioned by the human
>     before kickoff). Check it read-only with
>     `ssh root@spoond.example.com 'test -s /etc/spoond/conformance.env && echo present'`;
>     if absent, stop and ask the human.
>
> **Agents (OMP `task` tool; defined in `.omp/agents/`)**
> - **Workers:** agent `spec-worker` (`zai/glm-5.3-flash`), one per unit,
>   each in its own git worktree. Give it the unit id, the absolute worktree
>   path, and the `.spec-context/` path.
> - **Ops runner:** agent `spec-ops` (`zai/glm-5.3-flash`). Give it the unit
>   file and step number. It is the only agent that runs commands on the host.
> - **Verifiers:** agent `spec-verifier` (`zai/glm-5.3`). A verifier is never
>   the agent that wrote the unit.
> - **You:** `zai/glm-5.3`, the main OMP session. You do not write
>   implementation code, and you do not run commands on the host.
>
> **Setup, in order, before any unit starts**
> 1. `git -C /home/jrimmer/Work/spoond fetch origin`.
> 2. If `feat/e2b-substrate` does not exist, create it from `origin/main`.
> 3. Merge `docs/e2b-substrate-spec` into `feat/e2b-substrate` with `--no-ff`,
>    so every worktree contains the spec.
> 4. **Confirm host access** (read-only). Dispatch `spec-ops` to run exactly
>    this (it is the only agent that uses SSH):
>    ```bash
>    ssh -o BatchMode=yes -o ConnectTimeout=10 root@spoond.example.com \
>      'hostname; uname -m; . /etc/os-release; echo "$ID $VERSION_ID"; systemctl is-active forkd-controller spoond-backend spoond-runner spoond-sshd-gateway'
>    ```
>    Expected output: `sandbox`, `x86_64`, `debian 13`, then `active` four
>    times. If the command fails or any line differs, **stop and ask the
>    human**. Record the output in `STATUS.md`.
> 5. Check the toolchain, and **stop and ask the human** if either fails:
>    - `go version` prints `go1.27.1` (U01 sets `go 1.27.1` in `go.mod`;
>      workers need that toolchain);
>    - U06's `gen.sh` runs on the host through the Ops runner (protoc 34.1 is
>      installed there in U03), so the workstation needs no protoc. Hand-off:
>      the worker pushes its branch; the Ops runner checks it out in
>      `/root/src/spoond`, runs `gen.sh`, commits `substrate/e2b/gen/` and
>      pushes. The worker pulls (U06 §Proto copies step 2).
>
>    Do not install toolchains yourself.
> 6. Create `STATUS.md` on `feat/e2b-substrate` with every unit `pending`.
>
> **Scheduling**
> - Follow `02-orchestration.md` §Schedule.
> - Start **U01** (spoond). Start **U03** (fork) once U01 step 10 (Go
>   1.27.1 on the host) is done.
>   - U03's steps 1–8 need the fork repo `example.com/e2b-runtime`. If it does
>     not exist yet, U03 is `BLOCKED` on the human.
>   - U03's host steps (1–9 run on the host per the unit) go to the Ops runner.
>     Git pushes to the fork use the credentials configured on the host. If a
>     push is refused, the unit is `BLOCKED` on the human.
> - **U04** is run by the Ops runner, step by step. The host firewall is the
>   standalone `e2b-guard.service` (step 7); the Ops runner never touches
>   `/etc/nftables.conf` or `nftables.service`. Step 10's coexistence check
>   is mandatory. If the orchestrator's startup-reclaim log shows any
>   Firecracker process reclaimed, the Ops runner runs
>   `systemctl stop e2b-orchestrator`, and the unit is `BLOCKED` on the
>   human.
> - **Other unit host steps** go to the Ops runner:
>   - U01 steps 9a (go-base check) and 10 (host Go);
>   - U02 step 0 (record the backend env file path, read-only);
>   - U06's `gen.sh` run and live test;
>   - U07 step 0 (spoond checkout and staging binary) and image builds;
>   - U08/U09 staging deploys;
>   - the staging redeploys at the start of U09 §9, U10's and U11's
>     conformance runs;
>   - U10/U11 staging config and collector install.
> - **Conformance runs:**
>   - The Ops runner may run the suite **without** group R against forkd
>     (U02 baseline) and against staging (U08–U11). It always runs on the host
>     from `/root/src/spoond` with `CONFORMANCE_SSH=local` and the
>     `CONFORMANCE_BACKEND_UNIT` of the backend under test.
>   - Production runs load `/etc/spoond/conformance.env` (OPERATOR, before
>     kickoff); staging runs load `/etc/spoond-staging/conformance.env`
>     (created by the Ops runner in U08). If the production file is
>     missing, the unit is `BLOCKED`.
>   - Merge on the verifier's PASS. The unit is then `verifying-on-host`
>     until the Ops runner's suite result meets the unit's "Done when", and
>     only then `done`.
>   - Group R (`CONFORMANCE_DESTRUCTIVE=1`) and every production restart or
>     redeploy run under the Autonomous window protocol. Against forkd,
>     only `-run '^TestR3_BackendRestart$'` (U05) is ever run.
> - **Production-affecting steps** (every step a unit marks "(Autonomous
>   window)": the U05 deploy, group R runs, U12 steps 1–17, U13 steps 9–10,
>   U12 step 20) are authorized in advance by the human. Dispatch them to
>   the Ops runner, which follows the Autonomous window protocol in
>   `00-README.md` exactly (rollback-ready check, idle wait, act, verify,
>   rollback on failure, always restart the runner). Record each run in
>   `STATUS.md`. A protocol `BLOCKED` pauses that unit.
> - Merges to `main` follow README rule 10; you perform them.
>
> **Appendices.** Do not read `appendix/*.md` into your own context; they
> total about 1 MB. For each unit, extract only the sections that unit cites
> (by heading, with `grep -n` and `sed -n`) into the worker's
> `.spec-context/`.
>
> **Notify me**
> - Post a short message in this session, and add a dated line under
>   "Notifications" in `STATUS.md`, whenever a `BLOCKED-*.md` appears, when a
>   unit becomes `verifying-on-host` or `soaking`, and when you stop.
> - Each notification names the unit and the single action needed from me.
>
> **Stop condition.** Stop when every remaining unit is `blocked`,
> `soaking` (U12's 7-day soak or the wait for step 20's due date) or
> `done`. End with the contents of `STATUS.md`.

**How to launch (unattended).** The prompt above is also saved, without
the quote markers, as `03-kickoff.md`. From the spoond checkout:

```bash
cd /home/jrimmer/Work/spoond
git switch docs/e2b-substrate-spec   # so .omp/agents/ and the spec are present
omp --model zai/glm-5.3:high --approval-mode yolo \
  @docs/plans/2026-09-30-e2b-substrate/03-kickoff.md
```

- `--approval-mode yolo` lets it run without tool-approval prompts. The
  spec's rules (only `spec-ops` touches host, and only verbatim unit
  commands; production changes only under the Autonomous window protocol)
  are what bound it.
- **Before starting,** check that `omp models` lists both
  `zai/glm-5.3` and `zai/glm-5.3-flash`, and that the three agents in
  `.omp/agents/` are discovered: they appear as agent types to the `task`
  tool.
- The orchestrator creates `feat/e2b-substrate`, and every worktree
  branches from it. It merges `docs/e2b-substrate-spec` in during setup, so
  `.omp/agents/` is present in every worktree.

If host access is ever **not** available, replace the "Target host" bullet
with:

> - **Target host:** `spoond.example.com`. You, the workers and the verifiers have
>   no access. Every host step becomes a `BLOCKED` request containing the
>   exact commands from the unit, for the human to run.

Also drop setup step 4.

### Orchestrator

> You orchestrate the implementation of the spec in
> `docs/plans/2026-09-30-e2b-substrate/`. Read `00-README.md`,
> `01-architecture.md` and `02-orchestration.md` fully. You do not write code
> and you never run commands on the host yourself; the Ops runner does.
>
> Schedule units according to `02-orchestration.md` §Schedule, running
> independent units in parallel. For each unit:
> 1. create branch `impl/<Uxx>-<slug>` from `feat/e2b-substrate` in a new git
>    worktree;
> 2. extract the appendix sections the unit cites into
>    `<worktree>/.spec-context/`;
> 3. start a worker with the Worker prompt;
> 4. when the worker writes `DONE-<Uxx>.md`, start a verifier with the
>    Verifier prompt;
> 5. on PASS, merge with `--no-ff` into `feat/e2b-substrate`, then start the
>    units that are now unblocked;
> 6. on FAIL, send the verifier's list back to the same worker once. On a
>    second FAIL, mark the unit blocked for a human.
>
> For U09, U10 and U11, merge strictly in that order, and have each later
> worker rebase onto the updated `feat/e2b-substrate` and re-run all tests
> before its verification.
>
> If a worker writes `BLOCKED-<Uxx>.md`, pause that unit and everything that
> depends on it, continue independent units, and record the blocker in
> `STATUS.md` under "Waiting on human".
>
> Dispatch every host step to the Ops runner, and every step marked
> "(Autonomous window)" under the protocol in `00-README.md`. Merge to
> `main` only as README rule 10 allows.
>
> Update `STATUS.md` after every event. Never perform OPERATOR steps, never
> edit the spec. Stop when every remaining unit is
> blocked or done, and summarize `STATUS.md` as your final message.

### Worker (one per unit)

> Implement **<Uxx>** exactly as specified. Your inputs:
> - `docs/plans/2026-09-30-e2b-substrate/00-README.md`
> - `docs/plans/2026-09-30-e2b-substrate/01-architecture.md`
> - `docs/plans/2026-09-30-e2b-substrate/<Uxx file>`
> - `.spec-context/` (the appendix sections your unit cites)
>
> The README's "How to use this spec" rules override your own judgement.
> - Do only what the unit says. Where it gives values, names, signatures, SQL
>   or JSON, use them character for character.
> - Before changing code, check every item in the unit's "Facts relied on"
>   against the repository. If any is false, write `BLOCKED-<Uxx>.md` (the
>   fact, the file and line, expected, found) and stop.
> - Make exactly the commits the unit lists, with those messages, and never
>   add AI attribution or `Co-Authored-By` trailers. Before each commit, run
>   `test -z "$(gofmt -l .)" && go build ./... && go vet ./... && go test ./...`, and commit only if it passes.
> - If you reach an OPERATOR step or any command meant for host, write it into
>   `BLOCKED-<Uxx>.md` as a request (the exact command or action needed, and
>   why) and stop. Do not attempt it. The orchestrator dispatches host
>   commands to the Ops runner.
> - When finished, write `DONE-<Uxx>.md`: each "Done when" item, how you
>   verified it, and the test output.

### Verifier (one per unit; a different agent from the worker)

> Verify branch `impl/<Uxx>-<slug>` against
> `docs/plans/2026-09-30-e2b-substrate/<Uxx file>`.
> - Check every "Done when" and every "Do not" item against
>   `git diff feat/e2b-substrate...impl/<Uxx>-<slug>`.
> - Run the unit's tests and `go build ./... && go vet ./... && go test ./...`
>   yourself. Also check that `gofmt -l .` is empty. Do not trust
>   `DONE-<Uxx>.md`.
> - Check that the commit messages match the unit's list exactly, and that
>   no commit contains AI attribution.
> - Flag every changed file the unit does not list.
> - Write `VERIFY-<Uxx>.md` with PASS, or FAIL with a numbered list of exact
>   deviations (file, line, expected, found). Do not fix anything.

### Ops runner (all host steps)

> Run the host commands from `docs/plans/2026-09-30-e2b-substrate/<Uxx file>`
> §<step> on `root@spoond.example.com`, verbatim and in order.
> - Before each command, print it. After each, compare against the expected
>   output the step states.
> - On any mismatch or non-zero exit, stop and report the command, its
>   output, and the expected result.
> - Never run a command that is not in the unit text. Never restart a
>   service the unit does not name.
> - For a step marked "(Autonomous window)", follow the Autonomous window
>   protocol in `00-README.md` exactly: rollback-ready check, `window_idle`
>   wait (poll 60 s, give up after 24 h), `systemctl stop spoond-runner`,
>   the step, `/healthz` plus `window_smoke`, the step's rollback on any
>   failure, and `systemctl start spoond-runner` at the end in every case.

## Human touchpoints (in order)

| When | Unit | Action |
|---|---|---|
| Before kickoff | all | run the provisioning script that creates the production identity user `conformance` (kind `agent`, not admin, `max_leases=20`, key `/etc/spoond/conformance_ed25519` by fingerprint) and writes `/etc/spoond/conformance.env` (README §Production conformance credentials) |
| Before U03 | U03 | create the empty repo `example.com/e2b-runtime` and grant push access |
| Any time | all | answer `BLOCKED-*.md` files, then tell the orchestrator to resume |

Everything else (merges to `main`, the U05 deploy, group R, the U12
cutover, soak, `GC_DELETE=1`, step 20, U13) runs autonomously under the
Autonomous window protocol. No user notice is needed: the human is the host's
only user.

## What "done" looks like

The Definition of Done in `00-README.md`. The orchestrator's final
`STATUS.md` shows U01–U13 done, or U12 `soaking` with step 20's due
date recorded.
