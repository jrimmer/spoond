# DONE spoond-k0uz (FS5): pins replace holds, one clock

Layer-3 review of `4b0dc78` (branch `work/spoond-k0uz`). The base work
was correct but incomplete: it missed a create-path wiring bug, two
idle-suspend guards, a migration backfill and a resume gate, and it
carried several doc/consistency defects. This round closes H1, H2, H3,
M5, M6, L7, L8, L9 and L10, applies the 2026-10-09 owner decision, and
turns every reviewer probe into a regression test. The branch is rebased
onto `origin/main` (`204623b`, release 2.9.2).

## H1 — `"pinned": true` on create was ignored

`api/server.go` decoded `Pinned` but the `leaseRequest` literal never
copied it, so an HTTP create could not pin. The field is now carried
into the grant.

- **`TestCreatePinnedTrueHTTP`** — mutation: drop `pinned: req.Pinned`
  from the `leaseRequest`, which leaves `body["pinned"] == false` on
  create. The existing pin tests call `grantLease` directly, so only an
  HTTP-level test catches this.

## H2 — spoond paused pinned leases via idle_suspend

`suspendIdleLeases` (collection and re-check) and `effectiveIdleSuspend`
had no `l.Pinned` guard, so a pinned lease with its own `idle_suspend`
or under `IDLE_SUSPEND_DEFAULT_SECS` was paused. Both guards are added;
an owner's explicit `POST /pause` still works.

- **`TestPinnedNotIdleSuspended`** — mutation: remove the `l.Pinned`
  guard in `suspendIdleLeases`, which suspends the pinned lease despite
  its `idle_suspend`.
- **`TestPinnedNotPausedByHostDefaultIdle`** — mutation: remove the
  `l.Pinned` short-circuit in `effectiveIdleSuspend`, which pauses the
  pinned lease under the host default (the unpinned twin proves the
  sweep still fires).

## H3 — migration 0022 left already-suspended leases with no clock

No `paused_at` backfill, so a lease suspended at upgrade was never
released. Migration 0022 now stamps `paused_at = now` for every
`suspended = 1` row: a fresh 30 d from the upgrade, no lease sooner.

- **`TestMigration22PinsHoldsAndBackfillsPausedAt`** — a v21 fixture
  with held, lapsed-hold, plain, unheld-suspended, held-suspended and
  lost leases. Mutation: drop the backfill, which leaves every
  suspended row's `paused_at` zero. The test also pins the pin
  conversion and the cleared hold columns.

## M5 — resume refused an unpinned non-persistent lease

`resume` (and `resumeAny`) refused a non-persistent lease with
`400 not_persistent`, contradicting resume-on-use. The gate is removed:
any suspended lease the caller owns can be explicitly resumed.

- The `TestHolderLabelNeverPins` tail now pauses and resumes a
  non-persistent lease, asserting success. Mutation: restore the
  `!l.Persistent && !l.Pinned` gate, which returns `errNotPersistent`.
- `CHANGELOG.md` records the `400` removal; `docs/api.md` drops the
  `400` from both resume routes.

## M6 — rollback story

Migration 0022 now clears `hold_expires_at`, `hold_set_at` and
`hold_ttl` after converting a live hold to a pin, so a 2.9 binary
rolled back onto this database reads every row as unheld instead of
re-treating an unexpired hold as live (re-protecting or re-pausing
leases the admin route had just unpinned).

- Pinned by the migration fixture's hold-column assertion. Mutation:
  skip the clearing UPDATE, which leaves an unexpired `hold_expires_at`
  for 2.9 to re-read.
- Documented in `docs/operations.md` (Rollback) and `CHANGELOG.md`.

## L7 — dashboard notice text

The pinned-idle notice is now `N pinned leases idle over 7 d (owner:
count, ...)` with a per-owner breakdown; `Snapshot.PinnedIdleByOwner`
carries the counts (display names where known).

- **`TestNoticeTriggers`** and the new assertion in
  **`TestFromDBPinnedIdleCountsOutsideTheWindow`** — mutation: drop the
  per-owner breakdown, which renders the generic `owner: count`.

## L8 — documentation defects

- Removed the false "a holder label still counts as the lease's
  activity" sentence (`docs/api.md`).
- Fixed the resume `400`/`404` statement on both resume routes and the
  per-path status text; a suspended lease the caller owns resumes
  whatever suspended it.
- `README.md` no longer names the removed lapsed-hold markers; the
  dashboard marks a pinned lease's holder ◆ only.
- `api/idle_suspend.go` header comments rewritten for FS5 (the removed
  IDLE_TIMEOUT sweep and held rules, the pin guard).
- `api/journal.go` "callersupplied" → "caller-supplied".

## L9 — unpinByHolderPrefix order

The in-memory leases are updated and saved first, then the store helper
clears any pinned row with no in-memory twin. Mutation: DB first and
memory second, which leaves the running process pinned while the store
is unpinned.

- Still covered by `TestAdminUnpinByHolderPrefix`; the memory-save path
  is now the primary.

## L10 — lease-less event id

`box_full` and `admin_unpin` now carry the placeholder lease id `-`
instead of an empty id, so the SSE JSON's `lease_id` is always
well-formed and a per-lease filter (`?lease_id=`, `EventFilter{LeaseID}`)
never matches (a real id is never `-`). The dashboard draws `spoond` as
their subject and the `-` in the id column.

- **`TestEventLinesLeaseLessPlaceholder`** — mutation: revert the
  placeholder to an empty id (or let it leak into the subject).
- Documented in `docs/api.md`.

## Owner decision 2026-10-09 — a paused pin is on the clock

`releasePausedLeases` and `notifyPausedExpiring` no longer exempt a
pinned lease: every paused lease is released 30 d after its pause date,
pinned or not. A pin protects only a running VM.

- **`TestPausedPinnedReleasedAt30`** — mutation: restore the `l.Pinned`
  skip in `releasePausedLeases`, which never releases the paused pin.
- **`TestDrainedResumeFailedPausedAtOneClock`** — a drained lease left
  suspended with `resume_failed` keeps its drain pause date and is
  released 30 d after it. Mutation: stamp a fresh `paused_at` (or none)
  in `markResumeFailed`, which resets or loses the clock.

## Gates

- `go build ./...` — clean
- `go vet ./...` — clean
- `gofmt -l .` — empty
- `go test -p 2 -count=1 ./...` — all pass
- `go test -race -count=1 ./api/ ./store/` — all pass
- `go vet -tags conformance ./conformance/` and a conformance compile —
  clean
- The added tests were re-run as the non-root `user` account
  (`HOME=/home/user`, build cache under `/tmp`), with no `/work`,
  `/run/honey` or `/opt/honey` reads: all pass.
- `git fetch origin`: `origin/main` is at `204623b` (release 2.9.2,
  plus spoond-58e6, spoond-hfko and spoond-638d); the branch is merged
  onto it and `git log HEAD..origin/main` is empty.
