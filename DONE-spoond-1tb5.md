# DONE spoond-1tb5: resume on the holder's next work call (#145 D2)

Branch `feat/resume-on-use`, targeting 2.9. Every automatically suspended
lease and a lease its holder suspended by hand resumes on its holder's
next work call; the background preempt auto-resume is gone.

## Changes

1. **One resume-on-use rule for every kind of suspend.** `ensureRunning`
   (`api/idle_suspend.go`) no longer tests for the `idle_suspend`
   `LastAction`: any suspended lease is resumed through the normal
   `resumeLease` path before the call is served. It is wired into every
   work path: exec and exec stream, files (read/write/stat/list/mkdir/
   remove), the proxy and stepd/guest dial, background jobs and job
   signal, the LLM gateway, the network-policy change and the prompt. GET,
   status, events and SSE never call it; `POST /resume` is unchanged. A
   lease whose hold lapsed (rule 3) resumes with its hold still lapsed —
   resume never renews a hold.

2. **One no-room refusal shape, `capacity_wait`.** `writeResumeRefusal`
   now answers every structural shortage (the preemption disk floor
   `errPreemptCannot`, the burst reserve `errBurstReserve`, a substrate
   `ErrCapacity`, and a draining node `errDraining`) with `503`, a
   `Retry-After: 30` header and JSON
   `{"error": ..., "code": "capacity_wait"}`. The owner's own memory
   quota stays `429`; `kept_budget` stays `409`. `writeResumeRefusal` is
   shared by the API server and the LLM gateway (which has no `*Server`),
   so the paths cannot drift. `writeResumeRefusal`'s `errLeaseBusy` case
   now carries `code: lease_busy` as JSON (a second `writeErrorCode`
   helper and a `writeErrorCodeAfter` helper were added). The table test
   `TestResumeOnUseNoRoomEveryPath` exercises exec, exec stream, files,
   proxy and jobs; `TestResumeOnUseLLMNoRoom` covers the gateway.

3. **Removed the background preempt auto-resume.** `runPreemptResumeLoop`,
   `resumePreempted` and `losePreempted` are gone from `api/preempt.go`,
   along with the `PREEMPT_RESUME_RETRIES` handling: the config field, the
   `DefaultPreemptResumeRetries` constant, `preemptResumeLimit`, the
   `preemptRetries`/`preemptCapLogAt` maps, `preemptWaitForCapacity`,
   `resetRetryWindow`, `shouldLogPreemptCap` and `clearPreemptCapLog`
   (`api/retry.go`), the `envIntOr("PREEMPT_RESUME_RETRIES", ...)` read in
   `cmd/spoond-backend/main.go`, and the docs. `promoteAllBurst` keeps its
   own 15 s ticker (`runPromoteLoop`, `api/admission.go`). Preemption's
   victim choice is unchanged. No resume failure ever marks a lease lost;
   the error goes to the caller. `spoond-dxq`'s crash-recovery retries are
   untouched.

## Where `409 lease_suspended` still remains

The code stays only for paths that genuinely cannot resume a suspended
lease because they need an already-running guest and are not "work":

- `GET /api/leases/{id}/stat` — the stat probe runs an exec in the guest.
- `POST /api/leases/{id}/fork` — forking checkpoints the source guest.
- `POST /api/leases/{id}/crash-test` — there is nothing running to crash
  (this one answers a plain `409` message, no `code`).
- the guest heartbeat `POST /lease/{id}/active` — it only moves
  `LastActive`, it is not work, and it keeps `409 lease_suspended` with
  `reason`.

No work path answers `409 lease_suspended` any more. An internal race
where a suspend lands between a work path's lookup and its
`setNetwork`/`resumeLease` call can still surface `errSuspended`
internally; the network handler now maps that to `409 lease_busy`
(retryable), and `ensureRunning`'s own `resumeLease` returns
`errLeaseBusy` while the pause holds the lease.

## Tests

- `TestResumeOnUsePerSuspendReason` — one case per reason (`idle`,
  `idle_suspend`, `hold_lapsed`, `pressure`, `preempt`, `hand`): exec on
  the suspended lease resumes it and returns `200`.
- `TestResumeOnUseNoRoomEveryPath` — the one table test over exec, exec
  stream, files, proxy and jobs: `503`, `Retry-After: 30`, JSON
  `{"error": ..., "code": "capacity_wait"}`, lease still suspended.
- `TestResumeOnUseLLMNoRoom` — the same shape on the LLM gateway.
- `TestResumeOnUseGetNeverResumes` — GET detail/list/events leave the
  lease suspended.
- `TestResumeOnUseNoRoomCapacityWait` and `TestResumeOnUseBusyCode` — a
  refused preempted resume stays suspended and is never lost; a busy
  lease answers `409 lease_busy`.
- Existing tests updated where they asserted the old behaviour:
  `TestPreemptionResumeOnUse` (exec resumes a preempted lease),
  `TestIdleSuspendExecAutoResumes`, `TestIdleSuspendStaleMarkerDoesNotResume`,
  `TestFilesSuspendedResumes`, `TestGuestDialSuspendedResumes`,
  `TestJobStartOnSuspendedResumes`,
  `TestProxyRefusesEnvdPortAndResumesSuspended`, the `TestLLMGateway`
  suspended-lease case, `TestNetworkRoute/suspended` and the rewritten
  `api/lease_suspended_test.go`.

## Gates

- `go build ./...` — clean
- `go vet ./...` — clean
- `gofmt -l .` — empty
- `go test -p 2 -count=1 ./...` — all pass
- `go test -race -count=1 ./api/ ./store/` — all pass
- The added and updated tests also pass as a non-root user
  (`spoondtest`, `HOME`/`GOCACHE` under `/home`), with `t.TempDir()`
  everywhere; no test needs root and none reads `/work`, `/run/honey` or
  `/opt/honey`.
- `git fetch origin`: no commits were added to `origin/main` since the
  branch base, so there is no new step type, provider, restart, cancel or
  retry path to reconcile.

## Round 2 (layer-3 review of 2d8f376)

### R1 — drain hole

`ensureRunning` (`api/idle_suspend.go`) now calls the new
`Service.resumeForUse` (`api/service.go`); `resume` and `resumeAny`
route through it too. `resumeForUse` takes `drainGate`'s read side with
`TryRLock` (a work call never queues behind a finishing drain), refuses
with `errDraining` while `draining` or `drainClearPending` is set, and
holds the read side across `resumeLease`. `writeResumeRefusal` maps
`errDraining` onto the shared `503` + `Retry-After: 30` +
`code: capacity_wait` shape (the previously dead `resumeNoRoom` branch).
The undrain still clears the drain first and resumes the drained leases
through `resumeLease` directly.

- Test: `TestResumeOnUseDrainRefuses` (`api/resume_on_use_test.go`).
- Mutation that fails it: disable the `draining.Load() ||
  drainClearPending.Load()` check in `resumeForUse` (exec on a Drained
  lease during drain then returns 200 and clears `Drained`).

### R2 — LLM gateway auth order

`api/llmgateway.go` now runs the per-user LLM key / `requireKey` check
before the `lease.Suspended` resume, so a 401 leaves the lease
suspended and spends no hugepages.

- Test: `TestResumeOnUseLLM401LeavesSuspended`
  (`api/resume_on_use_test.go`).
- Mutation that fails it: move the resume block back above the
  authentication block (an empty key resumes the lease before 401).

### R3 — preempted held leases stranded

`suspendedByRule` (`api/held.go`) no longer exempts a preempted lease;
`pauseActionPreempt` (`preempt/suspend`) is one of the accepted
`LastAction`s, so rules 2 and 5 cover it like any rule suspension. The
old exemption's comment is gone.

- Tests: `TestPreemptedHeldLeaseReleasedByStaleRule`,
  `TestPreemptedHeldLeaseReleasedByCriticalRule` (`api/held_test.go`);
  `TestPreemptedHeldLeaseStaleReleased` (`api/preempt_test.go`) and
  `TestIdleSuspendedStaleRelease` (`api/idle_suspend_test.go`) updated
  to the new behaviour.
- Mutation that fails them: re-add the `!l.PreemptedAt.IsZero()` early
  return in `suspendedByRule`.

### R4 — concurrency

`TestResumeOnUseConcurrentExactlyOneResume`
(`api/resume_on_use_test.go`) runs four real parallel execs on one
suspended lease with the first resume's `Create` held open; it asserts
exactly one resume `Create`, every caller answered 200 or `409
lease_busy`, and no 500.

- Mutation that fails it: `if false && l.busy` in `resumeLease`
  (4 Creates instead of 1).

### R5 — job signal order

`handleJobSignal` (`api/jobs_http.go`) resolves the job record and
checks `State == "running"` before `ensureRunning`, so signalling a
finished job does not resume the lease.

- Test: `TestJobSignalFinishedDoesNotResume` (`api/jobs_test.go`).

### R6 — docs

`docs/api.md`'s per-path table now splits `GET /api/leases/{id}` (lease
detail: 200 for a suspended lease) from
`GET /api/leases/{id}/stat` (the stat probe: 409 lease_suspended), and
the heartbeat row records 204 for a busy (not suspended) lease. The
`CHANGELOG.md` mismatched backtick at the non-work-path sentence is
fixed; the preempted-rule change and the drain refusal are documented.

## Round 2 gates

- `go build ./...` — clean
- `go vet ./...` — clean
- `gofmt -l .` — empty
- `go test -p 2 -count=1 ./...` — all pass
- `go test -race -count=1 ./api/ ./store/` — all pass
- The added and updated tests pass as a non-root user
  (`spoondtest`), with `t.TempDir()` everywhere.
- `git fetch origin`: `origin/main` is unchanged since the branch base
  (`git log HEAD..origin/main` is empty), so there is no new step type,
  provider, restart, cancel or retry path to reconcile.
