# Swarm controller (draft for decision)

Status: **draft, 2026-10-02.** Decisions marked **Proposed** need the
owner's yes before an implementation spec is written.

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

The controller moves the *mechanical* half of orchestration into spoond, on
vm2, so it runs whether or not anyone is at a keyboard. Planning, review,
merging and production changes stay with the orchestrator and the owner.

## What it does, and what it does not

| The controller (spoond, on vm2) | The orchestrator / owner |
|---|---|
| Holds each project's task graph and its claims | Writes and polishes tasks; sets dependencies |
| Spawns bees to match the ready work, within quotas | Decides worker classes and quotas |
| Answers `[READY]` with the next eligible task | Reviews `[DONE]` branches; merges or sends back |
| Releases the claims of bees that die or stall; retries | Decides what to do after repeated failure |
| Stops idle bees (`[BYE]` or idle timeout) | Production changes (`vm2-window`), diagnosis |
| Reports state to the dashboard and Agent Mail | Talks to the owner |

## Proposed decisions

**C1. Where it runs: a spoond subcommand, `spoond swarm`, as the unit
`spoond-swarm` on vm2.** It is a consumer of the lease API like
`spoond runner`, using its own agent identity (`swarm`). The lease API
contract does not change.

**C2. The task graph lives on vm2, owned by the controller.** One `br`
(beads) workspace per project under `/var/lib/spoond/swarm/<project>/`.
The controller is the only process that changes claims and states; the
orchestrator adds and edits tasks through the controller (`spoond swarm
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
within one controller tick (30 s). Persistent leases, stopped by the
controller after `[BYE]`.

**C5. Health and retries.**
- An bee whose lease is gone, or whose progress signal reports
  "no log activity" for 30 min, or that sends nothing for 60 min, is
  stopped; its claim is released with a comment; the task is retried.
- A task gets 3 attempts. After that it is marked `blocked` and the
  orchestrator and owner are mailed.
- Model-service outages (the bee reports `[BLOCKED] retryable:
  infrastructure`) do not count as attempts; the controller pauses
  spawning for that project until `llm.lacy.casa` answers again.

**C6. Projects and worker classes.** A project is configured once
(`/etc/spoond/swarm/<project>.yaml`): repository URL, deploy-key name,
default worker class, `max_workers`. A worker class names the image
(`agent-worker`), the implement and verify models (Bifrost names), the
network allowlist, and which task labels it may take (today:
`needs:vm2-ssh` and `serial:prod` are never taken by bees).

**C7. Credentials stay on vm2.** The deploy keys, the Agent Mail token and
the `swarm` lease token live in `/etc/spoond/swarm/secrets/` (0600, root),
and are injected into bees at start as today. Nothing on a laptop.

**C8. Visibility.** The dashboard gains a swarm panel (bees, their
task and latest progress line, ready/blocked counts). `spoond swarm status`
prints the same. Every dispatch decision is also in Agent Mail.

**C9. Budget guard.** A per-project cap on bee-hours per day
(default 24); when reached, the controller stops spawning and mails the
owner. Model spend is watched in Bifrost by the owner.

## Open questions

1. **Agent Mail and Bifrost stay on vm1.** A vm1 stall stops every
   bee (2026-10-02). Moving them is an infra decision; the
   controller only needs to survive it (C5 already does).
2. **Merging.** Should a `[DONE]` that the verifier PASSed and that only
   touches docs merge automatically? Proposed: no, every merge stays a
   human or orchestrator decision for now.
3. **hrmny** joins as a second project once this works for spoond.

## Not in scope

The jobs API (`POST /api/jobs`, generic image + params + secrets) discussed
on 2026-10-01 is the general form of what the controller does for one image.
The controller is built first, on the existing lease API; the jobs API can
later absorb its spawning half.

## Reference behaviour

agent-hub `bin/swarm-spawn`, `swarm-assign`, `swarm-stop`, `swarm`, and
spoond `images/agent-worker-start.sh` (once committed) are the working
prototype. Incidents that shaped C4 and C5 are in agent-hub
`windows/` and the 2026-10-01/02 task comments.
