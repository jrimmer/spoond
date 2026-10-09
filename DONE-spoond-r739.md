# DONE spoond-r739: runner create with wait and capacity retry

Branch `work/spoond-r739`. Incident 2026-10-08 21:46 PDT (cytale runs
3438/3439, jobs 3896-3898 and 3904): `runner/lease_adapter.go` Create sent
no `"wait"`, so the backend admission queue (#129) was skipped, and a
`503` on create failed the job at once (no sandbox, no task logs).

## Round 1

1. **Create sends `"wait"`.** `HTTPLeaseClient.Create` posts
   `wait: RUNNER_ADMIT_WAIT_SECS` (default 900, `0` = no wait; clamped by
   `MAX_ADMIT_WAIT_SECS`). It also raises its own HTTP timeout to cover
   the wait.
2. **A capacity 503 is retried, never a job failure.** `createOnce`
   reports a `503` as capacity with `Retry-After` (or a 30 s default);
   `Create` logs the wait and retries until ctx ends. Other refusals
   (400/401/403/404/429) are returned at once.
3. **The job bounds the wait.** `Executor.JobTimeout`
   (`RUNNER_JOB_TIMEOUT`, duration or bare seconds) and the job's own
   `timeout-minutes` combine as the tighter bound; a create cut by that
   context reports `ResultCancelled`, not failure.
4. **A plain `503 capacity: …` refusal carries `Retry-After: 30`** like
   the burst-reserve and preempt refusals.

## Round 2

5. **H1 keepalive during create.** `Executor.keepaliveWhileWaiting` sends
   `Sink.Keepalive` every `CreateKeepaliveInterval` (default 60 s) and a
   `waiting for capacity on spoond (N s)` log row at the start of the
   wait and every `CreateLogInterval` (default 3 m). A create admitted at
   once sends neither. The rows are numbered before the first step's.
6. **M1 lenient `timeout-minutes`.** `WorkflowJob.TimeoutMinutes` is a
   `TimeoutMinutes` (`float64`) with a custom `UnmarshalYAML`: an int or
   float is honoured, an expression or string is logged once and ignored.
   On main a plain `int` made `timeout-minutes: ${{ matrix.t }}` fail the
   whole workflow parse.
7. **L1 changelog.** `timeout-minutes` is now ENFORCED (a job past it is
   cut and reported cancelled, matching GitHub); known users named (hrmny
   `e2e-live.yml` 75, `ci.yml` 45/45/20).
8. **L2 orphan lease on a gone client.** After `waitForAdmission`
   returns a granted lease, `POST /api/sandboxes` releases it with reason
   `client_gone` and writes nothing when `r.Context().Err() != nil`. The
   race happens when `finishTicket` loses to the client's context: the
   lease would otherwise have no runner owner and no spoond-job label and
   live until its TTL.
9. **L3 prompt drain for waiting jobs.** `Executor.OnCreateWait` reports
   a create wait, wired through `WorkerImpl.SetCreateWaitNotifier` and the
   pool. `RunnerPool.Stop` cancels a still-waiting job at once; jobs
   running steps keep `RUNNER_STOP_GRACE`.
10. **L4 default job bound.** `DefaultJobTimeout = 6h` is the
    `RUNNER_JOB_TIMEOUT` default, so a node answering 503 forever cannot
    pin a worker; `0` disables it.

## Tests, and the mutation each kills

- `TestHTTPLeaseClientCreateSendsAdmitWait` — remove the `wait` field
  from the create payload.
- `TestHTTPLeaseClientCreateNoWaitWhenDisabled` — send `wait` even when
  admission waiting is off.
- `TestHTTPLeaseClientCreateRetriesCapacity503` — treat a 503 as final,
  or retry without the `Retry-After` delay.
- `TestHTTPLeaseClientCreateCapacity503DefaultRetryAfter` — drop the
  default hint for a header-less 503.
- `TestHTTPLeaseClientCreateCapacity503BoundedByContext` — ignore ctx in
  the retry loop.
- `TestHTTPLeaseClientCreateNon503NotRetried` — retry a permanent
  refusal.
- `TestAdmitCapacityRefusalCarriesRetryAfter` (api) — drop the
  `Retry-After` on a plain capacity refusal.
- `TestExecutorJobTimeoutBoundsCreateWait` — remove the host job bound.
- `TestExecutorJobTimeoutMinutesBoundsCreateWait` — remove the job's own
  bound.
- `TestEffectiveJobTimeoutPicksTighter` — pick the looser bound, or let a
  negative `timeout-minutes` bound.
- `TestExecutorReportsCancelledOnContextDeath` — report a create cut by
  the context as failure.
- `TestExecutorCreateWaitSendsKeepalives` — remove the wait-time
  keepalive/log row.
- `TestExecutorCreateAdmittedAtOnceSendsNoWaitRows` — log/ping on an
  immediate create.
- `TestParseWorkflowTimeoutMinutesLenient` — go back to a plain `int`
  (expression fails the parse), or treat a string as the bound.
- `TestExecutorTimeoutMinutesExpressionDoesNotBound` — fail the run on an
  expression value.
- `TestAdmitGrantedLeaseReleasedWhenClientGone` — write the 201 (and skip
  the release) for a granted lease whose client is gone.
- `TestStopCancelsJobWaitingInCreate` — insert the grace before
  cancelling a create-waiting job.
- `TestDefaultJobTimeout` (cmd) — change the 6h default or ignore an
  explicit `RUNNER_JOB_TIMEOUT=0`.
- `TestMainSIGTERMGraceThenCancel` (cmd) — the wiring end to end: the
  create carries the default wait, the running step gets the grace.

Gates run on this branch: `go build ./...`, `go vet ./...`, `gofmt -l .`
empty, `go test -p 2 -count=1 ./...`, `go test -race -count=1 ./runner/
./api/`.

## Round 3

11. **T1 create-wait test gap.**
    `TestStopCancelsJobWaitingInCreateKeepsGraceForRunningJob` drives the
    real `Executor` through the pool and asserts a job past Create (a
    step running) keeps `RUNNER_STOP_GRACE` while another still waiting
    is cancelled at once. Kills the mutation that never clears
    `createWait` after `Create`.
12. **T2 env read.** `jobTimeoutFromEnv` is the helper `Main` uses;
    `TestDefaultJobTimeout` now calls it (unset -> 6h, `0` -> no bound,
    `90m`, `5400`). Kills `envDurOr("RUNNER_JOB_TIMEOUT", 0)` in Main,
    which the constant-only test survived.
13. **C1 overflow.** `TimeoutMinutes.UnmarshalYAML` ignores and logs a
    non-finite, `<= 0`, or `> 1e6` value, so `.inf`/`1e300` cannot
    overflow the minutes-to-duration conversion and remove every bound.
    `TestParseWorkflowTimeoutMinutesLenient` covers `.inf`, `-.inf`,
    `1e300`, `1000001`, `0`, `-5`.
14. **C2 race.** `cancelWaitingJobs` re-checks `createWait` and cancels
    under `w.mu`; the executor clears `createWait` under the same lock
    after Create and before steps. The T1 test fails if the flag is not
    cleared.
15. **S1 semantics.** `timeout-minutes` bounds execution from sandbox
    creation, not the admission wait; `RUNNER_JOB_TIMEOUT` stays the
    whole-job bound and caps a larger `timeout-minutes`. Documented in
    `docs/operations.md` and CHANGELOG. Tests:
    `TestExecutorTimeoutMinutesBoundsExecutionNotWait`,
    `TestExecutorTimeoutMinutesBoundsExecution`,
    `TestExecutorJobTimeoutCapsExecutionTimeout`, `TestExecutionTimeout`.

Round 3 test names and the mutation each kills:

- `TestStopCancelsJobWaitingInCreateKeepsGraceForRunningJob` — never
  clear `createWait` after `Create` (also covers the C2 re-check).
- `TestDefaultJobTimeout` — `envDurOr("RUNNER_JOB_TIMEOUT", 0)` in Main.
- `TestParseWorkflowTimeoutMinutesLenient` — drop the C1 overflow guard.
- `TestExecutorTimeoutMinutesBoundsExecutionNotWait` — count the
  admission wait against `timeout-minutes`.
- `TestExecutorTimeoutMinutesBoundsExecution` — never bound execution by
  `timeout-minutes`.
- `TestExecutorJobTimeoutCapsExecutionTimeout` — let `timeout-minutes`
  beat the host cap.
- `TestExecutionTimeout` — mis-map unset/non-positive minutes.
