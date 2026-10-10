# DONE spoond-bs5j: fair shares FS1 — equal floating slice per owner and usage accounting

Branch `work/spoond-bs5j`, targeting 3.0. This unit computes and reports
each owner's equal floating slice (1/N of the box) and their usage; it
does **not** change any admission, preemption or take-back behaviour.

## Changes

1. **One place computes the shares (`api/fair_shares.go`).** `fairShares`
   builds a `fairShareSnapshot`: for N = the number of owners that exist
   (each identity user, plus the legacy consumer token as one owner),
   every owner gets `slice_pct = 100/N`, `memory.slice_mib = pool/N` and
   `disk.slice_bytes = usable/N`. Usage is
   `memory.used_mib` = the owner's running leases' `memory_mb`, and
   `disk` split into `paused_bytes` + `kept_bytes` + `named_bytes` with
   `used_bytes` their de-duplicated sum. `ratio` is the larger of the
   memory and disk usage/slice ratios; a zero slice contributes no ratio
   (zero-capacity guard: no division by zero, every slice and ratio 0).
   The disk basis is the volume's **usable** bytes: the statfs
   free-to-unprivileged bytes (`Bavail`) plus the bytes spoond already
   accounts for (kept + named + paused), i.e. what spoond can hand out.

2. **Cached and invalidated on change.** `fairSharesCache` holds the last
   snapshot behind a mutex with an epoch, so a computation that races an
   invalidation is not stored. `invalidateFairShares` is called from
   `saveLeaseLocked` (only when a lease's accounted signature changes:
   owner, running state or memory charge; an activity-only save leaves
   the cache warm) and `deleteLeaseLocked`, from `UpdateKeptMetrics` and
   `UpdateNamedSnapshotMetrics` (kept/named disk changes), from
   `unkeepBuilds`, and from `POST /api/users` and
   `DELETE /api/users/{id}` (owner add/delete). A 5 s TTL is a backstop
   against a missed invalidation. Disk bytes come from the store's
   recorded `size_bytes` (`DiskUsageByOwner`, one grouped query for the
   whole box) — no filesystem walk per request.

3. **API.**
   - `GET /api/users/{id}` (admin): `{"user": …, "share": …}`; `403` for
     a non-admin, `404` for an unknown id.
   - `GET /api/users/me` now also carries `"share"`.
   - `GET /api/usage` (self-scoped, any token): `{"share": …}`.
   - `GET /api/fair-shares` (admin) is the fair-share view:
     `{"capacity_known": …,
     "owners": [...]}` sorted by `ratio` descending, ties by owner id.
   - Every share payload carries `capacity_known`: `false` means the box
     totals were not readable (a cold node-info cache) and the slices are
     not computed yet, so no caller reads a wrong zero for a real slice.
   - **`GET /api/shares` is unchanged.** It still lists the caller's
     lease grants, exactly as on `origin/main` (`handleShareList`,
     `{"shares": …}`); the fair-share admin view lives at `GET
     /api/fair-shares` instead, so no existing caller (the SSH
     gateway's `share ls`, clients) breaks.

4. **Store helpers.** `store/usage.go` adds `DiskUsageByOwner`: one
   query over pause builds, kept pins and named snapshots that
   de-duplicates a build counted by more than one kind (a checkpoint
   that is both kept and named counts once) and returns per-kind bytes
   plus a de-duplicated `Used`. Deleted builds count for nothing in the
   pause/kept sets.

No per-owner settings or weights were added; the existing class/quota
fields are untouched (removing them is later work).

No per-owner settings or weights were added; the existing class/quota
fields are untouched (removing them is later work).

## Tests

- `api/fair_shares_test.go`
  - `TestFairSharesEqualSlices` — every owner gets exactly 1/3 for three
    owners (100/3 %, 341 MiB, 1 GiB/3).
  - `TestFairSharesLegacyTokenIsOneOwner` — an identity user plus a
    legacy consumer token is N=2, and the token owner has no name.
  - `TestFairSharesNChanges` — the table test: 1 → 2 → 3 owners →
    legacy owner added → owner deleted, asserting the recomputed
    memory and disk slices each step.
  - `TestFairSharesZeroCapacityGuard` — a node with no hugepage size and
    a zero-capacity disk reports 0 slices/ratio; no owners also reports
    N=0 with an empty list.
  - `TestFairSharesUsageByKind` — one running lease, a pause build, a
    kept checkpoint and a named snapshot land in the right buckets and
    the ratio is the max of the two resource ratios.
  - `TestFairSharesRatioOrdering` — `GET /api/fair-shares` lists the
    heavier owner first, ratios non-increasing.
  - `TestFairSharesCacheInvalidated` — the cached snapshot is returned
    until invalidated, then recomputed.
  - `TestFairSharesOwnerDeleteViaAPI` — `DELETE /api/users/{id}` drops N
    by one.
  - `TestUserUsageEndpointAdminOnly`, `TestUserMeCarriesShare`,
    `TestSharesListAdminOnly`, `TestSharesListJSONShape`,
    `TestFairSharesDeletedBuildNotCounted`.
- `api/shares_test.go` is the `origin/main` lease-grant suite (plus the
  round-4 R1 pin): `TestShareGrantEnablesExec` still calls
  `GET /api/shares` and passes unchanged, so the grant listing behaves
  exactly as before.
- `store/usage_test.go` — `TestUsageDiskByOwner` (pause/kept/named sums
  per owner, deleted builds excluded, a kept+named build counted once),
  `TestUsageDiskByOwnerDedupsNamedBuild` and `TestUsageDiskEmptyStore`.

## DONE note: computed slices for sb's current owners

The task asked for a fixture of today's owner counts "if available".
None is present in the repository — there is no checked-in snapshot of
sb's users, leases or snapshot sizes — and the harness rules forbid
touching the production host, so no real per-owner numbers are computed
here. The table test `TestFairSharesNChanges` covers the arithmetic for
the plausible small N (1–4 owners). `GET /api/fair-shares` on the
deployed backend prints the real numbers for sb's current owners.

## Gates

- `go build ./...` — clean
- `go vet ./...` — clean
- `gofmt -l .` — empty
- `go test -p 2 -count=1 ./...` — all pass
- `go test -race -count=1 ./api/ ./store/` — all pass
- The added tests also pass as a non-root user (`user`, `HOME` and
  `GOCACHE` under `/home/user`), using `t.TempDir()` everywhere; none
  needs root or reads `/work`, `/run/honey` or `/opt/honey`.
- `git fetch origin`: `origin/main` gained the v2.9.0 tag, the
  spoond-r739 runner merge and the fork P10 guest-log merge since the
  branch base (`0a1c495`). spoond-r739 is in `runner/`, the create
  path's capacity `503` (Retry-After) and `admitqueue_test.go`; P10 is
  the orchestrator fork's `/logs` handler and its changelog. Neither
  adds a step type, provider, restart, cancel or retry path that touches
  the shares work. The harness rebase onto `origin/main` leaves the
  branch building, vetting, gofmt-clean and passing
  `go test -p 2 -count=1 ./...` and `go test -race -count=1 ./api/
  ./store/`. The one rebase conflict is `CHANGELOG.md` at the v2.9.0
  release boundary (main moved the old `[Unreleased]` content into the
  released section); the fair-shares entry is kept under `[Unreleased]`
  here so the resolution stays a 3.0 change.

## Round 3

Round 2 had moved the lease-grant listing to `GET /api/shares/grants`
and given `/api/shares` to the fair-share view, breaking existing
callers (`share ls`, clients). That is undone:

- `api/shares.go` and `api/shares_test.go` are restored byte-for-byte to
  `origin/main`; `GET /api/shares` lists lease grants with the same body
  and statuses, and its grant-listing test (`TestShareGrantEnablesExec`)
  passes unchanged.
- The fair-share implementation and its tests moved to
  `api/fair_shares.go` / `api/fair_shares_test.go` (the `lease_shares.go`
  copies are gone). The admin fair-share list is now
  `GET /api/fair-shares`; it is admin-only and sorted by ratio.
- `cmd/spoond-sshd-gateway/main.go` is restored to `origin/main`, so
  `share ls` calls `/api/shares` again.
- `docs/api.md` and `CHANGELOG.md` say `GET /api/fair-shares` is the
  admin fair-share view and that `GET /api/shares` still lists grants;
  no doc calls `/api/shares` the fair-share list.

### Round-3 gates and origin/main

All gates were rerun after the change: `go build ./...`,
`go vet ./...`, `gofmt -l .` (empty), `go test -p 2 -count=1 ./...`
and `go test -race -count=1 ./api/ ./store/`. The grant-listing and
fair-share tests also pass as a non-root user (`user`, `HOME`,
`GOCACHE` and `GOTMPDIR` under `/home/user`) with `t.TempDir()`
everywhere; none needs root or reads `/work`, `/run/honey` or
`/opt/honey`.

`git fetch origin` since round 2 shows `origin/main` only gained the
spoond-clcw conformance merge (`conformance/`, `DONE-spoond-clcw.md`)
and its `CHANGELOG.md` entry. `git diff HEAD...origin/main` touches no
`api/`, `store/` or `store/migrations/` file, so there is no new step
type, provider, restart, cancel or retry path to reconcile; the branch
has no migrations of its own.

## Round 4

Layer-3 review of 06ca078: the code, gates and policy routing were
right, but the tests did not catch the real regressions below. Same
branch.

### R1 — pin `/api/shares`

- `api/shares_test.go` gains `TestShareListJSONShapePinned`: it grants
  a share, decodes `GET /api/shares` with `DisallowUnknownFields` into
  `{"shares":[{lease_id, grantee, mode, created_at, expires_at?}]}` and
  asserts lease id, grantee and mode. Renaming `"shares"` or adding a
  top-level key fails the decode; dropping the array fails the length
  check; losing `created_at` fails the non-empty check.
- `cmd/spoond-sshd-gateway/snapshot_handler_test.go` gains
  `TestCtlShareListParsesSharesKey` (the backend's `shares` key renders
  the table) and `TestCtlShareListRejectsRenamedKey` (a backend that
  names it `grants` must not render a table).
- Existing `TestShareGrantEnablesExec` in `api/shares_test.go` still
  calls `GET /api/shares` and passes unchanged.

### R2 — invalidation tests (fixed clock)

`freezeFairShareClock` pins `svc.now`, so the 5 s TTL never expires and
only an explicit invalidation replaces the snapshot; `freshAfter`
asserts the next read is a different snapshot. Tests (each kills the
mutation that removes its invalidation call):

| Test | Mutation it kills |
| --- | --- |
| `TestFairSharesInvalidateOnLeaseRelease` | remove the `invalidateFairShares` in `saveLeaseLocked` |
| `TestFairSharesInvalidateOnLeaseDelete` | remove it in `deleteLeaseLocked` |
| `TestFairSharesInvalidateOnUserCreateAndDelete` | remove it in `handleUsersCreate` or `handleUsersDelete` |
| `TestFairSharesInvalidateOnKeep` | remove it in `UpdateKeptMetrics` |
| `TestFairSharesInvalidateOnNamedSaveAndDelete` | remove it in `UpdateNamedSnapshotMetrics` |

Each was checked by removing the call and watching the test fail.

### R3 — invalidate after the write

`api/service.go` `saveLeaseLocked`/`deleteLeaseLocked` and
`api/named_snapshots.go` `unkeepBuilds` now invalidate only after the
store call succeeds. Invalidating first let a concurrent compute read
the pre-write state and cache it for the TTL.

### R4 — don't cache failed reads

`fairShareSnapshot` carries `ok`. `computeFairShares` sets it false
when the hugepage cache is cold, statfs fails or any of the three
grouped store queries fails; `fairShares` returns that snapshot without
storing it, so the next caller retries. The box-wide compute runs on
`context.WithoutCancel(ctx)` under a 5 s timeout, so a client
disconnect or hung read is not turned into cached zeros.

`TestFairSharesFailedReadNotCached` covers a cold NodeInfo, a statfs
error and a closed store (each asserts `!ok` and no cache entry);
`TestFairSharesCancelledRequestContextDetached` asserts a pre-cancelled
request context still yields an ok, cached snapshot.

### R5 — no RPC on this path

`totalHugepageMiB` reads only `nodeInfoCache`/`nodeInfoAt` under
`nodeInfoMu`; a cold cache returns `(0, false)` and never calls
`s.sub.NodeInfo`. `TestFairSharesFailedReadNotCached/cold_node_info`
also asserts the fair-share path made no `NodeInfo` RPC.

### R5 — unknown vs zero, disk basis, cache churn, shapes, dedup

R5-1: every share payload (and `GET /api/fair-shares` at the top level)
carries `capacity_known`. It is `false` when the node-info cache is cold
or a store read failed; the slices are then `0` (not computed) and the
snapshot is not cached. `TestFairSharesCapacityKnownStates` reads the
cold state through `/api/fair-shares`, `/api/usage` and `/api/users/me`
(all `capacity_known: false`, a 1/N `slice_pct`, a zero `slice_bytes`)
and then the warm state (`true`, a non-zero slice).

R5-2: `usableSnapshotBytes` returns the statfs free-to-unprivileged
bytes plus the accounted snapshot bytes. `TestFairSharesUsableDiskBasis`
seeds a 200 MiB named snapshot and asserts the slice grows from 1 GiB to
1 GiB + 200 MiB.

R5-3: `saveLeaseLocked` compares each lease's accounted signature
(owner, running state, memory charge) and invalidates only on a change;
`TestFairSharesActivitySaveKeepsCacheWarm` calls `markActive` (activity
only) and asserts the snapshot is the same object, then a create and
asserts a fresh one.

R5-4: `TestSharesListJSONShape` decodes `GET /api/fair-shares` with
`json.Decoder.DisallowUnknownFields` into a struct listing every field,
including `capacity_known`, and asserts non-zero `slice_bytes`,
`used_bytes`, `named_bytes`, `ratio` and a non-empty `owners`. Renaming
any field or adding an unlisted one fails the decode.

R5-5: `DiskUsageByOwner` groups by `(owner, build_id)` so a build that
is both kept and named counts once. `TestUsageDiskByOwner` and
`TestUsageDiskByOwnerDedupsNamedBuild` assert the de-duplicated `Used`
while the per-kind fields still report each kind.

### Gates and origin/main

All gates rerun on the branch: `go build ./...`, `go vet ./...`,
`gofmt -l .` (empty), `go test -p 2 -count=1 ./...` and
`go test -race -count=3 -timeout 50m ./api/ ./store/` (the three
`./api/` race passes take ~13 min, over Go's default 10 min). The added
tests pass as a non-root user (`user`, `HOME`/`GOCACHE` under
`/home/user`) with `t.TempDir()` everywhere. `git fetch origin` shows
`origin/main` gained the 2.9.1/2.9.2 dash releases and the
`work/spoond-hfko` merge: the dashboard i/o stall meter (`cmd/spoond-dash`)
and the resume_failed event-on-change guard (`api/admin.go`). Neither
touches the fair-share files, no new step type, provider, restart, cancel
or retry path; the branch adds no migration. The rebase onto the latest
`origin/main` leaves the branch building, vetting, gofmt-clean and
passing the gates.
