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
