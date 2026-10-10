# spoond-c0x4 — FS3a disk take-back core: done and owed

Round 2–5 scope: FS3a ONLY, the core as new code, not wired into any
request path. `api/disk_room.go` (accounting, selector, take-back) and
`api/disk_takeback_test.go` carry it; migration 0023 backfills
`paused_at` for the one clock's rows.

## Done

- **Room formula.** `computeDiskRoom` (pure, no I/O):
  `Usable = Free - Reserved - Floor - Pending + Freeing`. Reserved is
  the whole `disk_mb` allowance of running leases; Floor is
  `DISK_RESERVE_PCT` of the volume (env, default 5, clamped 0–50);
  Pending is bytes promised to in-flight writes; Freeing is bytes of
  deletions statfs has not reported yet. Each term is
  mutation-tested.
- **In-flight tracker.** `diskInflight.reserve` returns an idempotent
  release func for an in-flight write; `noteFreeing` records a deletion
  with the statfs free at the note; `settle` drops an entry once free
  has grown by its bytes or after 30 s (ZFS late-free guard: statfs lag
  causes no second deletion — tested).
- **Selector.** `diskVictims` picks the owner with the highest disk
  ratio (usage/slice, FS1 shares) whose ratio after giving up the lease
  stays above the requester's after-request ratio (aligned with
  memVictims on FS2's branch — tested, including the not-strictly-above
  edge), then that owner's oldest unpinned paused lease; pinned and
  running leases never appear, named/kept bytes are not candidates.
  Asked for the shortfall, not the whole need.
- **Take-back.** `diskTakeBack`: garbage first (`reapOrphans`,
  ORPHAN_REAP and GC_DELETE guards kept — an off reap frees nothing,
  tested), re-check, then one victim at a time under a take-back mutex:
  look up the LIVE lease, `releaseBecauseIf(..., "disk_reclaim")` with
  the predicate re-checking paused/unpinned/unreleased/not-busy at
  commitment, emit `lease.critical_release {owner, ratio, free_pct}`
  only after the commit, credit Freeing, re-check. A stale pick (resume
  or pin won the race) is skipped silently and the loop re-picks;
  nothing left returns `*boxFullError` (unwraps to `errBoxFull`).
  Exactly one `disk.cleanup` event per call with bytes per category
  (garbage, leases). Garbage that already covers the need releases no
  lease.
- **Owner views.** `diskOwners`/`diskOwnersAtView` build per-owner
  disk usage/slice from the fair-share snapshot plus paused candidates
  sized by the pause build's recorded `size_bytes`; one lease-catalog
  read per pass.
- **Migration 0023** backfills `paused_at` for suspended rows (the one
  clock), mirroring the approved memory-mb backfill shape.
- **Tests.** The eight FS3a behaviours fail under mutation: each room
  term; garbage before any lease; inside-slice owner untouched; ratio
  ordering; pinned and running leases never taken; named/kept never
  touched; statfs lag causes no second deletion; nothing reclaimable
  answers box-full. Event assertions wait for the always-emitted
  `disk.cleanup` before checking `critical_release`'s absence (proof:
  moving the emit ahead of the release makes the stale-pick test fail).

## FS3b still owes (after FS2 spoond-pxsn lands)

- Wire `diskTakeBack` into the request paths: create, resume, pause,
  keep, named save, build — every write that must fit in Usable,
  reservation and all; one take-back pass per tick/request while the
  write waits (`capacity_wait`), re-checking after each pass.
- Map `errBoxFull` to the caller: `503 capacity_wait` while take-back
  can still serve, `429 box_full` (JSON `code: box_full`, usage-vs-slice
  message) when nothing unpinned can be taken — plus the
  `capacity.box_full` event and the dashboard notification, cleared
  under 100 % of the box minus one default lease.
- Remove the 15 % `PREEMPT_DISK_FLOOR_PCT` and the held rule 5
  critical-disk release this replaces (FS2 owns `api/preempt.go`,
  `api/admission.go`, `api/admitqueue.go` right now).
- Passive alert only: one statfs per minute; free under the 5 % floor
  (something outside spoond wrote) emits `capacity.disk_low` and raises
  one dashboard notification. It deletes nothing.
- `GET /api/shares` and the dashboard show Reserved, Usable and paused
  bytes per owner.
- `docs/api.md`: every status code change the wiring introduces,
  listed.
- sb read-only verification numbers: volume size, free, Reserved for
  the running leases, Usable.
