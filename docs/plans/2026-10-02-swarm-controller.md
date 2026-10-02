# Hive: the swarm controller (draft for decision)

Status: **accepted 2026-10-02** (C1-C11; the owner asked for it to be
built, including enlistment and the guide). Build order at the end.

## Why

Since 2026-10-01, coding work on spoond runs as a *swarm*: bees
(`bee-N`) in agent-worker leases take tasks over Agent Mail, implement
them, have them independently verified, and push branches. Today the
orchestrator (an interactive session on a laptop) does everything between
those steps: it spawns bees, answers `[READY]` with a task, notices
dead ones, releases their claims, and stops idle ones. Two consequences:

- **Nothing moves while the orchestrator is not running.** Overnight on
  2026-10-01/02 three tasks sat dead for hours.
- **Spawning lives in laptop scripts** (agent-hub `swarm-spawn`,
  `swarm-assign`, `swarm-stop`) holding credentials in `~/.config/swarm`.

The hive moves the *mechanical* half of orchestration into spoond, on
the host, so it runs whether or not anyone is at a keyboard. Planning, review,
merging and production changes stay with the orchestrator and the owner.

## What it does, and what it does not

| The hive (spoond, on the host) | The orchestrator / owner |
|---|---|
| Holds each project's task graph and its claims | Writes and polishes tasks; sets dependencies |
| Spawns bees to match the ready work, within quotas | Decides worker classes and quotas |
| Answers `[READY]` with the next eligible task | Reviews `[DONE]` branches; merges or sends back |
| Releases the claims of bees that die or stall; retries | Decides what to do after repeated failure |
| Stops idle bees (`[BYE]` or idle timeout) | Production changes (`vm2-window`), diagnosis |
| Reports state to the dashboard and Agent Mail | Talks to the owner |

## Proposed decisions

**C1. Where it runs: a spoond subcommand, `spoond hive`, as the unit
`spoond-hive` on the host.** The hive is where a project's bees live:
their task graph, dispatch and scaling. It is a consumer of the lease API like
`spoond runner`, using its own agent identity (`swarm`). The lease API
contract does not change.

**C2. The task graph lives on the host, owned by the hive.** One `br`
(beads) workspace per project under `/var/lib/spoond/hive/<project>/`.
The hive is the only process that changes claims and states; the
orchestrator adds and edits tasks through the hive (`spoond hive
task add|edit|dep`, or an import of a beads JSONL file), never by editing
the files. All machines see one graph, and claims stay atomic because they
happen in one place. A read-only JSONL export is committed to the
project's agent-hub folder after each change, for history.

**C3. Dispatch replaces the orchestrator's `[READY]` handling.** The
controller registers as the dispatcher identity in Agent Mail
(`dispatch-1`), answers `[READY]` with the best eligible task (beads'
ready set, filtered by the worker class's allowed labels, by priority),
and handles `[DONE]`/`[BLOCKED]`/`[BYE]` bookkeeping. `[DONE]` marks the
task `review:pending` and copies the report to the orchestrator; the
orchestrator closes it after review. Bees are unchanged (they
already only talk to whoever sends their tasks).

**C4. Scaling rule.** For each project:
`wanted = min(eligible ready tasks, max_workers) - idle bees`.
Spawn while `wanted > 0`; never more than `max_workers` alive. Idle
bees leave on their own after their idle timeout (default 30 min).
A task that becomes ready while none are idle gets a fresh bee
within one hive tick (30 s). Persistent leases, stopped by the
controller after `[BYE]`.

**C5. Health and retries.**
- Bees keep their own leases awake: the worker posts the lease
  heartbeat (`POST $SPOOND_GATEWAY_URL/lease/<id>/active`, guest-service
  port, the lease id as capability) at least every 10 minutes, working
  or idle, so the backend's idle sweep (`IDLE_TIMEOUT_SECS=1800`) leaves
  them alone. Before this, three bees were suspended mid-task
  (2026-10-02). The hive does not poke leases; it only resumes a bee
  lease it finds suspended (a bee that stopped heartbeating is a dead
  bee, handled below).
- An bee whose lease is gone, or whose progress signal reports
  "no log activity" for 30 min, or that sends nothing for 60 min, is
  stopped; its claim is released with a comment; the task is retried.
- A task gets 3 attempts. After that it is marked `blocked` and the
  orchestrator and owner are mailed.
- Model-service outages (the bee reports `[BLOCKED] retryable:
  infrastructure`) do not count as attempts; the hive pauses
  spawning for that project until `llm.lacy.casa` answers again.

**C6. Projects and worker classes.** A project is described by its
`.spoond/hive.yaml` (C10) and registered by enlisting it; the hive keeps
the enlisted copy under `/var/lib/spoond/hive/<project>/`. A worker class
names the worker image (`<base>-worker`, C10), the implement and verify
models (Bifrost names), the network allowlist (derived, C10), and which
task labels it may take (today: `needs:vm2-ssh` and `serial:prod` are
never taken by bees).

**C7. Credentials stay on the host.** The deploy keys, the Agent Mail token and
the `swarm` lease token live in `/etc/spoond/hive/secrets/` (0600, root),
and are injected into bees at start as today. Nothing on a laptop.

**C8. Visibility.** The dashboard gains a hive panel (bees, their
task and latest progress line, ready/blocked counts). `spoond hive status`
prints the same. Every dispatch decision is also in Agent Mail.

**C9. Budget guard.** A per-project cap on bee-hours per day
(default 24); when reached, the hive stops spawning and mails the
owner. Model spend is watched in Bifrost by the owner.

**C10. Enlistment: one project file, one command, everything else
derived.** A project describes itself in its own repository, in
`.spoond/hive.yaml`:

```yaml
project: hrmny
repo: ssh://git@git.lacy.casa/lacy.casa/hrmny.git
base_image: elixir-release      # any image in the spoond catalog
gates:                          # what "done" means; bee and verifier run them
  - mix format --check-formatted
  - mix test
needs: [leases, registry]       # extra network; repo host and model service are implied
max_workers: 3
models: {implement: Z.ai/glm-5.3, verify: Z.ai/glm-5.3}
```

From it the hive derives:
- **The worker image.** A worker is a base image plus one fixed layer
  (Pi, the bee start script, amail). Enlisting builds `<base>-worker`
  with spoond's image builder and rebuilds it whenever the base image's
  current build changes. No per-project Dockerfile.
- **The network allowlist.** Repo host, model service and Agent Mail
  always; each `needs:` entry maps to a fixed set (`leases`: the lease
  API; `registry`: the registry host). No raw addresses in the file.
- **Credentials.** The hive mints the project's lease token (scoped to
  its own leases), registers its Agent Mail identities, and generates
  its deploy key pair. The private half never leaves the host (C7).

Two things stay with the owner by design: **authorizing the deploy key**
on the repository (write access to a repo is the owner's decision) and
**the budget** (C9). Merging stays with the orchestrator or the owner.

`spoond hive init` drafts a hive.yaml from a checkout: base image from
the toolchain files, gates from the CI workflows. An agent reviews
rather than writes it.

**C11. The guide: the running instance teaches enlistment.** There is
no separate how-to to keep in step. The lease API serves:

| Route | Auth | What it returns |
|---|---|---|
| `GET /hive/guide` | none (LAN) | How to enlist a project on *this* instance: its real addresses, images, models and routes, the hive.yaml schema, and the first step. No secrets. |
| `POST /hive/check` | consumer token | Runs every enlistment check against a submitted hive.yaml, for real, on a trial lease. |
| `POST /hive/projects` | consumer token | Enlists (the same checks must pass first). |
| `GET /hive/projects/<p>/guide` | project owner | Where this project stands and the next step. |
| `GET /hive/projects/<p>/doctor` | project owner | The checks again, for an enlisted project: key still authorized, image builds, gates pass on main, budget left. |

Rules:
- **Generated, never hand-written.** The guide is rendered from the
  same configuration and route table the server runs, and a test fails
  when a hive route or a hive.yaml field is missing from it.
- **Every answer ends with `Next:`**, computed from the project's state.
  An agent never needs the whole process, only the last line.
- **One check engine, three front doors.** `POST /hive/check`,
  `spoond hive enlist --check`, and the guide's `Next:` all run the same
  checks. Each result is a pass or fail plus the exact remedy, as text
  (`Accept: text/plain`, the default) or JSON.
- The repository docs say one thing: point your agent at
  `https://<host>:8890/hive/guide`.

The checks, in order: hive.yaml parses and validates; base image exists;
the worker image builds; the deploy key can clone and push a scratch
branch (then deletes it); a trial lease with the derived allowlist
reaches every `needs:` target; the gates pass on the default branch;
a budget is set.

## Open questions

1. **Agent Mail and Bifrost stay on vm1.** A vm1 stall stops every
   bee (2026-10-02). Moving them is an infra decision; the
   controller only needs to survive it (C5 already does).
2. **Merging.** Should a `[DONE]` that the verifier PASSed and that only
   touches docs merge automatically? Proposed: no, every merge stays a
   human or orchestrator decision for now.
3. **hrmny** is the first project to enlist through C10/C11 after spoond
   itself (enlisted the same way, replacing agent-hub's scripts).

## Not in scope

The jobs API (`POST /api/jobs`, generic image + params + secrets) discussed
on 2026-10-01 is the general form of what the hive does for one image.
The hive is built first, on the existing lease API; the jobs API can
later absorb its spawning half.

## Reference behaviour

agent-hub `bin/swarm-spawn`, `swarm-assign`, `swarm-stop`, `swarm`, and
spoond `images/agent-worker-start.sh` (once committed) are the working
prototype. Incidents that shaped C4 and C5 are in agent-hub
`windows/` and the 2026-10-01/02 task comments.

## Build order

Each step is usable on its own, and each is a task in the spoond graph.

1. **Worker layer.** Turn `images/agent-worker*` into a layer applied to
   any base image (`<base>-worker`); `agent-worker` becomes
   `go-base-worker`. Commit the image files to the spoond repo.
2. **hive.yaml and the check engine** (`hive` package): schema,
   validation, derivation (allowlist, worker image), and the checks, with
   text and JSON reports. `spoond hive enlist --check FILE`.
3. **Guide and check API** (C11): `GET /hive/guide`, `POST /hive/check`,
   the guide-completeness test.
4. **Deferred: build only if v3 is delayed** (spoond #86). The v3 workflow
   engine (#78) covers it generally. **Hive core** (C1-C5, C7): the `spoond-hive` unit, enlisted projects,
   per-project task graph, dispatch, scaling, health, retries; spawns
   bees with minted credentials. Retires agent-hub `swarm-*` scripts and
   `swarm-keepalive`.
5. **Enlist and per-project routes**: `POST /hive/projects`, project
   guide and doctor, `spoond hive init`.
6. **Visibility and budget** (C8, C9): dashboard panel, `spoond hive
   status`, bee-hour cap.
7. **Enlist spoond, then hrmny.**