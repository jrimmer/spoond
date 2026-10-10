# DONE spoond-xi9r: FS1 follow-ups

Three follow-ups to FS1 (`spoond-bs5j`, merged into `origin/main` as
`e558aa1`). No admission, preemption or take-back behaviour changes.

## (a) named_snapshot disk bytes come from the build first

`store/usage.go` `DiskUsageByOwner` reads a named snapshot's bytes as
`COALESCE(b.size_bytes, n.size_bytes)` (a `LEFT JOIN builds` on
`build_id`): the build row's `size_bytes` is re-measured by the hourly
disk accounting pass and is authoritative, and the named row's size —
the size at save time — is only used when the build row is gone. This is
the round-3 form that a later round dropped; it is restored and now
documented.

The consequence, documented in `docs/api.md` under **Fair shares**: the
fair-share `named_bytes` for a snapshot can differ from the `size_bytes`
the named-snapshot listing reports for the same version, because the
listing uses the named row's size at save time while the fair view uses
the disk's measurement.

Test: `TestFairSharesSharedBuildCountedOnceInBasis` (api) asserts an
owner's `named_bytes` is the build row's 200 MiB, not the stale named
row's 99 MiB. `TestUsageDiskByOwnerDedupsNamedBuild` (store) already
covers the same-build dedup.

## (b) box-level DISTINCT build basis

`api/fair_shares.go` `computeFairShares` summed each owner's `Used` for
the accounted part of the disk slice basis. A build can be attributed to
more than one owner — a lease may pin a build another owner owns, and a
named snapshot is owned independently of its build's owner — so a shared
build was counted once per owner, inflating the basis (and every slice).

`store/usage.go` gains `AccountedSnapshotBytes`: a box-level query that
collapses the pause, kept-pin and named arms to one row per `build_id`
(`MAX(size_bytes)`; the named arm's `COALESCE` prefers the build row) and
sums the distinct builds. `computeFairShares` uses it instead of summing
`usageByOwner`'s `Used`; a failed read is the same unknown capacity as a
failed `DiskUsageByOwner` (not cached).

Tests:
- `TestUsageAccountedSnapshotBytesCountsSharedBuildOnce` (store): one
  build pinned by alice and bob and named by alice — per-owner `Used`
  sums to 1700 but the box-level accounted bytes are 1000.
- `TestFairSharesSharedBuildCountedOnceInBasis` (api): the two-owner
  slice is `(1 GiB + 200 MiB)/2`, not `(1 GiB + 400 MiB)/2`.

## (c) unconditional invalidation on size settle

`api/gc.go` `settleBuildSize`'s `record` closure now calls
`s.invalidateFairShares()` directly after the store write. Before this,
the invalidation was reached only through `UpdateKeptMetrics`; the base
`UpdateKeptMetrics` already invalidated unconditionally before its
metrics nil check, so this was not an observed bug. The change makes the
invalidation explicit in `record` rather than incidental to the gauge
update, and a test pins it independent of the metrics wiring.

Test: `TestFairSharesSizeSettleInvalidatesWithoutMetrics` (api), with
`s.metrics == nil` and a frozen clock, asserts the snapshot is replaced
once a settled size is recorded.

## Gates

`go build ./...`, `go vet ./api/ ./store/`, `gofmt -l .` (empty),
`go test ./api ./store`. The added tests use `t.TempDir()` and run as a
non-root user. No migration is added.

`git fetch origin`: `origin/main` is `e558aa1`, the base of this branch;
no commits landed on `origin/main` since. Nothing here touches a step
type, provider, restart, cancel or retry path.
