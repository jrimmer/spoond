# DONE spoond-urm: undrain resume retries

## Finding: how undrain resumed leases before this change

Code path: `POST /api/admin/undrain` -> `handleAdminUndrain` (`api/admin.go`)
-> `Service.undrain` (`api/admin.go`).

Before this change:

- **Concurrency.** `undrain` collected every lease with `Drained == true`
  and resumed all of them at once, up to `drainConcurrency = 4` at a time
  (the same constant the drain pauses use). So 8 drained leases would run
  4 resumes concurrently, each restoring a full 4 GiB memory snapshot:
  the host I/O and hugepage/memory spike the incident describes.
- **Timeouts.** `undrain` itself waits up to 120 s for `NodeInfo` to
  answer before it does anything. A single resume then went through
  `resumeLease` -> `resumeLeaseBody` -> `createSandbox` -> the substrate
  `Create`. The bound on envd init/sync after resume lives in the E2B
  orchestrator/fork (envd init timeout / `ENVD_TIMEOUT`,
  `envd-init-request-timeout-milliseconds`), not in spoond; spoond's
  `e2b` substrate additionally polls envd health for 30 s after `Create`
  returns. spoond held no configurable resume timeout of its own.
- **When a lease becomes lost.** A `resumeLease` error was handled once:
  admission refusals (`errQuotaExceeded`, `errBurstReserve`,
  `errPreemptCannot`) kept the lease `drained` for a later undrain, and
  every other error immediately set the lease `lost` and emitted a
  `lost` event. There was **no retry**.

The incident's `syncing took too long` is the orchestrator's envd init
deadline; it is a transient, load-dependent failure (six such messages in
the orchestrator journal over the window). Resuming 4 × 4 GiB snapshots
at once is what made the deadline trip. spoond side fixes: bound the
resume width and retry the transient failure. Raising the fork's envd
init timeout is a fork patch (P9) and out of scope for this
spoond-only task.

## Changes

1. Bounded undrain concurrency: `UNDRAIN_CONCURRENCY` (default 2,
   `0` = unlimited). The drain pauses keep their own `drainConcurrency`
   width.
2. Retry a retryable resume failure: `UNDRAIN_RESUME_RETRIES` (default 2)
   extra attempts with a 500 ms backoff before the lease is marked lost.
   Retryable = `context.DeadlineExceeded`, or an error mentioning
   "syncing took too long", "failed to init envd", "failed to init new
   envd", "envd not healthy", "context deadline exceeded"/"deadline
   exceeded". Non-retryable = `store.ErrNotFound`/`substrate.ErrNotFound`
   /`errNotFound` (missing image/build). Each retry is logged with the
   lease id and attempt.
3. Admission refusals keep their old "stays drained for a later undrain"
   behaviour and are not retried.
4. `drainFailure` gained an `attempts` field (omitted when not tracked);
   the undrain response's `failed` list and the per-lease logs state how
   many attempts were made. A retried success logs
   `undrain: resume <id> succeeded on attempt N`.
5. Tests (`api/undrain_test.go`) on the fake substrate:
   - `TestUndrainResumeRetriesTransient`: 3 leases, one resume fails once
     with `syncing took too long` then succeeds -> 3 resumed, 0 lost, no
     `lost` stamp.
   - `TestUndrainResumeRetriesPermanent`: a resume that keeps failing ->
     3 attempts (1 + 2 retries), lease lost, `failed[0].attempts == 3`.
   - `TestUndrainResumeNonRetryable`: a missing build -> 1 attempt, lost.
   - `TestUndrainConcurrencyBound`: 4 leases, resume held open -> at most
     2 in flight, the next starts only after one finishes.
6. Docs: `docs/setup.md` env table (`UNDRAIN_CONCURRENCY`,
   `UNDRAIN_RESUME_RETRIES`), `docs/operations.md` drain/undrain section,
   `docs/api.md` admin route row, `README.md` notable settings, and
   `CHANGELOG.md` `[Unreleased] ### Fixed`.

## Gates

- `go build ./...` — pass
- `go vet ./...` — pass
- `gofmt -l .` — empty
- `GOMAXPROCS=2 go test -p 1 ./...` — pass (see below)

## Notes / not done

- The fork's envd init timeout (P9) is not touched: this task is spoond
  side only.
- No vm2 measurement was taken: orch-1 scoped this task to spoond only.
