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
   - `GET /api/fair-shares` (admin) is the fair-share view: `{"owners":
     [...]}` sorted by `ratio` descending, ties by owner id.
   - **`GET /api/shares` is unchanged.** It still lists the caller's
     lease grants, exactly as on `origin/main` (`handleShareList`,
     `{"shares": …}`); the fair-share admin view lives at `GET
     /api/fair-shares` instead, so no existing caller (the SSH
     gateway's `share ls`, clients) breaks.

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
- `api/shares_test.go` is the `origin/main` lease-grant suite, untouched:
  `TestShareGrantEnablesExec` still calls `GET /api/shares` and passes
  unchanged, so the grant listing behaves exactly as before.
- `store/usage_test.go` — `TestUsageBytesByOwner` (pause/kept/named sums
  per owner, deleted builds excluded) and `TestUsageBytesEmptyStore`.

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
