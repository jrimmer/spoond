# DONE spoond-5x1l: critical-disk FIFO release and the proactive cleanup tier

## What changed

### Critical-disk FIFO for every suspended lease (#145 D5)

Held rule 5 (`api/held.go`) is now the one critical-disk cleanup for
**every suspended lease**, not only held leases a rule suspended:

- Below `CRITICAL_DISK_FREE_PCT` (default 5) the **oldest suspended
  lease** is released, one per sweep tick, until free space is above
  `CRITICAL_DISK_RECOVER_PCT` (default 10).
- The FIFO key is `suspended_at` (the lease.suspended bead's column);
  a hand or drain suspension carries no automatic reason, so the pause
  action time (`last_action_at`) stands in for it.
- Only a lease in state `suspended` is a candidate: a running lease,
  and a lease with an in-flight operation (`busy`), is never released.
- Before each release one `critical_release` warning event names the
  lease, its owner and the free percentage; then the lease goes through
  `releaseBecause` with reason `disk_critical`, so `released` carries it.
- The GC runs first (at most every 5 minutes) and the `GC_DELETE=1`
  guard stays: with the dry-run GC nothing is released.
- The old `held_action` record (`critical/release`) and the
  `spoond_held_actions_total{critical,release}` counter still count each
  release.

### Proactive cleanup tier before the FIFO (owner refinement 2026-10-08)

Added `api/diskcleanup.go`, run at the start of every sweep tick
(`runHeldRules`) so it always precedes the FIFO. Below
`DISK_CLEAN_START_PCT` (default 20) free it reclaims spoond's own
garbage in this order until `DISK_CLEAN_STOP_PCT` (default 25):

1. orphan build/snapshot directories with no store row (the existing
   orphan reap, `ORPHAN_REAP`/`GC_DELETE` guards unchanged);
2. leftovers of released/lost leases (unreferenced pause/checkpoint
   builds the catalog still holds);
3. expired kept checkpoints past `KEPT_CHECKPOINT_TTL_SECS` (the pin is
   dropped so this same tick's GC may reclaim the build);
4. unreferenced template builds.

One `disk.cleanup` event per tick carries the bytes freed per category.
None of these is live owner data, and the tier never touches a running
or suspended lease. The FIFO below 5 % remains the last resort.

New env vars: `DISK_CLEAN_START_PCT`, `DISK_CLEAN_STOP_PCT`,
`KEPT_CHECKPOINT_TTL_SECS` (default 604800 = 7 d).

### No new refusals

Nothing about this change refuses a create, resume or keep. The
owner contract (no host-structural refusal; structural shortage is a
wait) is unchanged: the critical cleanup acts, it never gates admission.

## DONE note: on-disk bytes per category on main's test fixtures

Measured with the repo's own test fixtures (`newTestService` +
`seedImage`, so py-base at 2 GiB; `seedSizedBuild` records 8 MiB per
build; `mkOrphanDir` writes a 4096-byte memfile). One category is
seeded per row so the split is visible; the full pass totals 32 MiB.

| Category | Fixture | Bytes freed |
|---|---|---|
| Orphans (no store row) | one `mkOrphanDir` dir, aged past `ORPHAN_MIN_AGE_SECS` | 4 KiB (the fixture file; on production a real build dir is GiBs) |
| Released/lost leftovers | one `pause` build row, 8 MiB, no live lease | 8 MiB |
| Expired kept checkpoints | one `checkpoint` build pinned by a live lease, `kept_at` 2 h old | 8 MiB |
| Unreferenced template builds | one `template` build row, 8 MiB, no image points at it | 8 MiB |

Which categories had **no reclaim path before** this change:

- **Expired kept checkpoints** — kept builds were permanent GC roots
  while their lease lived; nothing expired a pin. The proactive tier is
  the first path that unpins an old keep so the GC can take it.
- **Leftovers of released/lost leases and unreferenced template
  builds** did already flow through `gcCandidates`, but only on the
  hourly catalog GC (or the 5-minute GC the critical rule runs); they
  had no *proactive* path at moderate pressure. The tier now reclaims
  them as soon as the disk crosses 20 % free.
- **Orphan directories** had the `reapOrphans` path, shared unchanged.

## Tests

- `api/diskcleanup_test.go`:
  - `TestCriticalReleaseEndState` — the Honey contract: after a critical
    release the lease answers `404 lease not found` on GET, resume and
    DELETE, and the stream carries `critical_release` immediately before
    `released` with reason `disk_critical`.
  - `TestDiskCleanupBeforeCriticalFIFO` — at 18 % free the tier reclaims
    an unreferenced build and emits `disk.cleanup` while a live
    suspended lease survives; at 3 % the FIFO releases it.
  - `TestCriticalNeverReleasesRunningLease` — no running lease is
    released, and the counter stays 0 with no suspended candidate.
  - `TestDiskCleanupExpiresKeptCheckpoint` — an old pin is dropped and
    its build becomes a GC candidate in the same tick.
  - `TestDiskCleanupEventNamesCategories` — one `disk.cleanup` event
    names all four categories.
- `api/held_test.go` — `TestCriticalReleasesAnySuspendedOldestFirst`
  replaces the old rule-suspended-only test: a hand-suspended lease is
  now a FIFO candidate, oldest first, one per tick, running untouched.
  `TestCriticalReleasesOldestSuspendedFirstToRecovery` now drives the
  order through `suspended_at`.
- `api/notify_test.go` — the notifier forwards `critical_release` as
  critical under `held.critical.*`.
- `api/events_test.go` — the wire names `disk.cleanup` and
  `critical_release`.

## Gates run

- `go build ./...` — pass
- `go vet ./...` — pass
- `gofmt -l .` — empty
- `go test -p 2 -count=1 ./...` — pass
- `go test -race -count=1 ./api/ ./store/` — pass
- Tests as a non-root user: all use `t.TempDir()` and the fake
  substrate; nothing needs root or `/work`.

## Docs

- `CHANGELOG.md` `[Unreleased] ### Added`.
- `docs/api.md`: the `Critical disk cleanup` section (no status-code
  change — the change adds no new status code, listed explicitly), the
  `released` reason `disk_critical`, and the `disk.cleanup` /
  `critical_release` event rows.
- `docs/operations.md`: the proactive tier, the reworked rule 5 table
  row, the kept-checkpoint TTL, the metrics note.
- `docs/setup.md`: the new env vars.
- `docs/operations.md` + dashboard: `disk.cleanup` and
  `critical_release` event colours.

## Origin/main

`git fetch origin` shows no commits on `origin/main` past the branch
point (base 67f939a); `git diff HEAD...origin/main` is empty. The
change is against the current main.
