# DONE spoond-bs5j: fair shares FS1 — equal floating slice per owner and usage accounting

Branch `work/spoond-bs5j`, targeting 3.0. This unit computes and reports
each owner's equal floating slice (1/N of the box) and their usage; it
does **not** change any admission, preemption or take-back behaviour.

## Changes

1. **One place computes the shares (`api/shares.go`).** `fairShares`
   builds a `fairShareSnapshot`: for N = the number of owners that exist
   (each identity user, plus the legacy consumer token as one owner),
   every owner gets `slice_pct = 100/N`, `memory.slice_mib = pool/N` and
   `disk.slice_bytes = usable/N`. Usage is
   `memory.used_mib` = the owner's running leases' `memory_mb`, and
   `disk` split into `paused_bytes` + `kept_bytes` + `named_bytes` with
   `used_bytes` their sum. `ratio` is the larger of the memory and disk
   usage/slice ratios; a zero slice contributes no ratio (zero-capacity
   guard: no division by zero, every slice and ratio 0).

2. **Cached and invalidated on change.** `fairSharesCache` holds the last
   snapshot behind a mutex with an epoch, so a computation that races an
   invalidation is not stored. `invalidateFairShares` is called from
   `saveLeaseLocked` and `deleteLeaseLocked` (every lease state change
   that writes or removes a row, including create/suspend/resume/
   release), from `UpdateKeptMetrics` and `UpdateNamedSnapshotMetrics`
   (kept/named disk changes), from `unkeepBuilds`, and from
   `POST /api/users` and `DELETE /api/users/{id}` (owner add/delete). A
   5 s TTL is a backstop against a missed invalidation. Disk bytes come
   from the store's recorded `size_bytes` (`PausedBytesByOwner`,
   `KeptBytesByOwner`, `NamedSnapshotBytesByOwner`), three grouped
   queries for the whole box — no filesystem walk per request.

3. **API.**
   - `GET /api/users/{id}` (admin): `{"user": …, "share": …}`; `403` for
     a non-admin, `404` for an unknown id.
   - `GET /api/users/me` now also carries `"share"`.
   - `GET /api/usage` (self-scoped, any token): `{"share": …}`.
   - `GET /api/shares` (admin) is now the fair-share view: `{"owners":
     [...]}` sorted by `ratio` descending, ties by owner id.
   - **Status-code/shape change:** `GET /api/shares` used to list the
     caller's lease grants. That listing moved to
     `GET /api/shares/grants` (same body and statuses). The SSH
     gateway's `share ls` calls the new path.

4. **Store helpers.** `store/usage.go` adds `PausedBytesByOwner`
   (recorded `size_bytes` of each owner's `kind='pause'` builds, deleted
   excluded), `KeptBytesByOwner` (the owner's `lease_kept_builds` pins
   joined to their leases, deleted excluded) and the per-owner named
   bytes (`NamedSnapshotBytesByOwner`, added next to the existing
   single-owner `NamedSnapshotBytesOfOwner`). All exclude deleted builds,
   whose files are gone.

No per-owner settings or weights were added; the existing class/quota
fields are untouched (removing them is later work).

## Tests

- `api/shares_test.go`
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
  - `TestFairSharesRatioOrdering` — `GET /api/shares` lists the heavier
    owner first, ratios non-increasing.
  - `TestFairSharesCacheInvalidated` — the cached snapshot is returned
    until invalidated, then recomputed.
  - `TestFairSharesOwnerDeleteViaAPI` — `DELETE /api/users/{id}` drops N
    by one.
  - `TestUserUsageEndpointAdminOnly`, `TestUserMeCarriesShare`,
    `TestSharesListAdminOnly`, `TestSharesListJSONShape`,
    `TestFairSharesDeletedBuildNotCounted`.
- `store/usage_test.go` — `TestUsageBytesByOwner` (pause/kept/named sums
  per owner, deleted builds excluded) and `TestUsageBytesEmptyStore`.

## DONE note: computed slices for sb's current owners

The task asked for a fixture of today's owner counts "if available".
None is present in the repository — there is no checked-in snapshot of
sb's users, leases or snapshot sizes — and the harness rules forbid
touching the production host, so no real per-owner numbers are computed
here. The table test `TestFairSharesNChanges` covers the arithmetic for
the plausible small N (1–4 owners). `GET /api/shares` on the deployed
backend prints the real numbers for sb's current owners.

## Gates

- `go build ./...` — clean
- `go vet ./...` — clean
- `gofmt -l .` — empty
- `go test -p 2 -count=1 ./...` — all pass
- `go test -race -count=1 ./api/ ./store/` — all pass
- The added tests also pass as a non-root user (`user`, `HOME` and
  `GOCACHE` under `/home/user`), using `t.TempDir()` everywhere; none
  needs root or reads `/work`, `/run/honey` or `/opt/honey`.
- `git fetch origin`: no commits were added to `origin/main` since the
  branch base (`0a1c495`), so there is no new step type, provider,
  restart, cancel or retry path to reconcile. The branch also builds and
  passes against the untracked working state the harness will rebase.
