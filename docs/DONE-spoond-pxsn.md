# spoond-pxsn — what is done and what FS2b still owes

Bead: FS2 fair shares — memory take-back from the biggest borrower
replaces classes, preemption and rule 4. Branch `work/spoond-pxsn`.

## Done (FS2a, this branch)

- `api/take_back.go`: the pure selector `memVictims` and its view
  builders.
  - Owners are ranked by ratio (used/slice); an owner inside their
    slice is never touched, and a give-up that would leave the owner at
    or below the requester's post-request ratio is never made (ratio,
    not absolute GiB, decides; the honey 12/28 vs pool 8/20 example
    takes from the pool).
  - Within an owner: least recently used unpinned lease first (a
    running background job counts as in use and ranks last); pinned
    leases are never candidates, not even in the fall-through when the
    LRU lease is too big to give up.
  - Stops as soon as the need fits; returns nil when nothing can be
    taken (the box_full case, served fully in FS2b).
  - The stop condition is the SHORTFALL (request minus free MiB); the
    requester's after-request ratio is computed for the whole request.
- `takeBackPause`: the guarded pause. Eligibility (running, unreleased,
  unpinned, not suspended, not busy, no running job, not the
  requester's own) is re-checked in the SAME critical section that
  marks the lease busy, so a pin, job start, release or resume landing
  in the gap changes the outcome. A pin that lands DURING the pause is
  caught by the re-check after the pause body and un-does the pause at
  once (the lease is resumed inside the busy window and nothing counts
  as freed) — a pin never ends suspended for take-back. One pause at a
  time; callers stop as soon as the request fits.
- Auto-resume stays suppressed for take-back suspends (the take-back
  stamp; resume happens on the holder's next work call), so a
  taken-back lease can never be auto-resumed into a take-back of the
  lease that displaced it.
- `takeBackOwners` builds the views from `s.fairShares(ctx)` owners
  (Memory.UsedMiB / Memory.SliceMiB) plus, under the store lock, each
  owner's live unreleased running leases (MemoryMB, Pinned,
  busy || running job, LastActive); owners are indexed by map so a
  late append cannot orphan earlier leases.
- `preemptForGuaranteed` now picks victims with `memVictims` +
  `takeBackPause` (the old priority/newest/guarantee order is gone
  from the take-back path). Function names and callers are unchanged
  for now.
- `lease.suspended` with reason `take_back` carries the victim owner's
  ratio (`take_back_ratio`) and the requester (`take_back_for`) on the
  event itself (bus, SSE and lease stamps); the separate `take_back`
  event stays.
- Tests: the pure-selector table (inside-slice never, ratio not
  absolute GiB, LRU within owner, busy last, pinned never, requester
  furthest over gets nil, stop once the need fits, fall-through to a
  smaller lease), the view builder, the pause happy path, the illegal
  victims, the guard race (pin/job before and during the pause) under
  -race, and the shortfall admission test.

## What FS2b still owes (explicitly out of scope this round)

- Replace the `preemptionCandidates()` gate (burst-only candidates,
  priority/newest/guarantee ordering) in `preemptForGuaranteed`: today
  it only feeds the disk-floor and box_full estimates; the selector
  already ignores it for the victim choice. Delete the function with
  the classes.
- Remove the guaranteed/burst admission classes, BURST_RESERVE (env
  `BURST_RESERVE_MIB` and `ServiceConfig.BurstReserveMiB`), the class
  promotion path and `admitClass`; every lease admits against the one
  pool.
- Remove the pressure sweep and its order/steps, and the env vars:
  `GUARANTEED`-related, `BURST_RESERVE_MIB`, `PRESSURE_HELD_IDLE_SECS`
  and friends — list each as Removed in the CHANGELOG.
- The box_full path: 429 + JSON code `box_full` + alert + dashboard
  notification on every path that cannot be served by take-back
  (create, clone/fork/restore, resume-on-use, POST /resume, and a
  queued create ended by the admission queue), with the table test
  over all paths; today only the pinned-candidates case answers
  box_full.
- Wait-never-refuse for capacity (503 + Retry-After + `capacity_wait`)
  and the removal of the 429 lease/memory quota refusals; the only
  capacity-like refusal left is 409 `kept_budget`.
- Disk take-back (FS3, work-246 owns disk code; this branch does not
  touch `api/gc*.go` or any disk code).
- docs/api.md: every API change, each status code change, and a note
  that a taken-back lease reports `"preempted": false` (the field
  stays false for take-back; FS2b documents it with the class
  removal).
- Rename/remove `preemptForGuaranteed`, `admitGuaranteed`,
  `errPreemptCannot` and the preemption metrics once the classes are
  gone (the 503 "cannot preempt (snapshot disk low)" mapping goes with
  them).
