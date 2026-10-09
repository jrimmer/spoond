# DONE spoond-638d round 2: drain-resume outcome and refusal gaps

Layer-3 review of `bcf7970` (work/spoond-638d). The 2.9.0 fix works;
this round closes F1, F2, N1, N2 and N3 without changing the happy path.
The branch is `work/spoond-638d`, rebased by the harness onto
`origin/main` (`3e59d21`, the spoond-qjsy admitqueue test fix). The new
round-2 work is in `bcf7970..` on top of that base.

## F1 — a lost lease no longer gets resume_failed (medium)

`markResumeFailed` used to check only `l.released`. A lease another path
had already marked lost made `resumeLease` return `*leaseLostError`; the
catch-all branch then stamped `suspend_reason=resume_failed`, kept
`Drained` set, and the heal loop retried the lease for
`DRAIN_RESUME_MAX_AGE` (24 h), emitting `drain_deferred` then
`drain_gave_up` for a lease whose sandbox is gone.

`markResumeFailed` now runs the shared `undrainLossAllowed` guard under
the store lock and returns whether it stamped. `drainResumeOutcome`
clears `Drained` quietly and reports non-deferred when the lease is
already lost (matching Main's `alreadyLost` branch); a released, running
or busy lease (a concurrent resume won) is skipped without a reason.

- `TestDrainResumeOutcomeAlreadyLostClearsDrainedQuietly` — stamping
  `resume_failed` on a lost, drained lease and keeping `Drained`.
- `TestDrainResumeOutcomeConcurrentResumeWinNotStamped` — stamping a
  running lease that a concurrent resume brought back.

## F2 — gRPC status text no longer masquerades as a transport error (low-medium)

`undrainNotReady`'s text fallback matched `"connection refused"` and
friends inside an orchestrator `Internal` error such as `failed to init
envd: ... connection refused`, so an envd start failure was retried for
the whole `UNDRAIN_RESUME_WINDOW` (5 min of multi-GiB snapshot resumes)
instead of the bounded `UNDRAIN_RESUME_RETRIES` budget. The text markers
now apply only to errors without a `"rpc error: code = "` prefix; a gRPC
error is classified by its code (the `Unavailable` code is still
not-ready, `substrate.ErrUnavailable` still maps directly).

- `TestUndrainNotReadyIgnoresTransportTextInGRPCStatus` — matching the
  text before the status-prefix check.
- `TestDrainResumeOutcomeInternalEnvdUsesRetryBudget` — running the
  window instead of the two attempts for an `Internal` envd failure.

## N1 — released lease is checked before the deferral branch

`drainResumeOutcome` now checks `errLeaseReleased` before
`undrainDeferred(err) || ctx.Err() != nil`, so the heal loop never emits
`drain_deferred` for a lease that was released while its resume ran.

- `TestDrainResumeOutcomeReleasedBeforeDeferral` — moving the released
  check below the deferral check, which returns `deferred=true`.

## N2 — substrate-unavailable resume is 503, not 500

`writeResumeRefusal` maps `substrate.ErrUnavailable` to the same
retryable `503 substrate_unavailable` + `Retry-After: 5` the other
substrate-unknown paths give, instead of falling through to the generic
`500 "resume failed"`.

- `TestWriteResumeRefusalSubstrateUnavailable` — dropping the
  `ErrUnavailable` case.

## N3 — resume_failed emits its suspended event and journal line

`markResumeFailed` now emits a `suspended` event with reason
`resume_failed` (and the resume build as `build_id`) and writes the puqp
`op=suspend reason=resume_failed` line, matching the comment in
`api/events.go` and the resume-on-use contract. `api/lostlease_test.go`
no longer cites the non-existent `TestUndrainRepeatedEnvFailureLeavesSuspended`;
it cites `TestUndrainResumeRetriesPermanent`.

- `TestMarkResumeFailedEmitsSuspendedEventAndJournal` — dropping the
  emit and journal calls.

## Gates

- `go build ./...` — clean
- `go vet ./...` — clean
- `gofmt -l .` — empty
- `go test -p 2 -count=1 ./...` — all pass
- `go test -race -count=1 ./api/ ./store/` — all pass
- The new and existing drain/undrain tests also pass as a non-root user
  (`spoondtest`, `HOME`/`GOCACHE` under `/home`, a copy of the tree under
  `/tmp`), with `t.TempDir()` everywhere; none reads `/work`, `/run/honey`
  or `/opt/honey` and none needs root.
- `git fetch origin`: `origin/main` gained `3e59d21` (the spoond-qjsy
  admitqueue wake-test fix, `api/admitqueue.go` +
  `api/admitqueue_test.go` + `api/service.go` comment). It touches the
  admission queue, not the drain/undrain, resume, restart or cancel
  paths; the branch rebases cleanly and the tests above cover the
  combined tree. No new step type, provider or retry path to reconcile.
