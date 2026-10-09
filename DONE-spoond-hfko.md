# DONE spoond-hfko: drain-resume follow-ups (event on change, dead code, docs)

Round-2 review of `spoond-638d` (merged) left three follow-ups for the
2.9.1 window. The branch is `work/spoond-hfko` on top of `origin/main`
(`7e0c8b9`, the merge of `work/spoond-638d`).

## F1 — resume_failed suspension is emitted only on the reason change

The drain self-heal loop calls `markResumeFailed` every interval for a
lease whose resume keeps failing. Round 2 (N3) made every call emit a
`suspended` event (reason `resume_failed`) and a puqp journal line, so
one failing lease produced one event per retry — 100+ in 24 h — and a
consumer such as Honey or the dashboard could read each as a new
suspension.

`markResumeFailed` now compares `l.SuspendReason` to
`suspendReasonResumeFailed` under the store lock. It still stamps the
reason, keeps `Drained` set and saves the lease on every call, so the
heal loop keeps retrying; it emits the event and journal line only on
the call that changes the reason. The return value still reports that
the lease was left suspended for retry.

- `TestMarkResumeFailedEmitsOnlyOnReasonChange` — two consecutive calls
  produce exactly one `suspended` event and one journal line. Mutation:
  emit regardless of the previous reason, which emits twice.

## F2 — dead errLeaseReleased check removed

`drainResumeOutcome` checked `errors.Is(err, errLeaseReleased)` above the
deferral branch (N1) and again below it, where the second check could
never be reached. The second block is deleted; the comment on the first
already records why the order matters.

## F3 — substrate_unavailable moved to the resume-on-use refusal list

`docs/api.md` described the resume-on-use refusals in the "Resume on
next use" paragraph (`429 quota_exceeded`, `503 capacity_wait`, plus
`410 lease_lost`). The `503 substrate_unavailable` sentence sat in the
undrain/lost-leases paragraph; it is now part of the resume-on-use
refusal list, and the undrain paragraph keeps only the `resume_failed`
outcome.

## Gates

- `go build ./...` — clean
- `go vet ./...` — clean
- `gofmt -l .` — empty
- `go test -count=1 ./...` — all pass
- `go test -race -count=1 ./api/ ./store/` — all pass
- The new and existing drain/resume tests also pass as a non-root user
  with `HOME`/`GOCACHE` under a home directory; they use `t.TempDir()`
  and read no `/work`, `/run/honey` or `/opt/honey` path.
- `git fetch origin`: `origin/main` added nothing since the branch base
  (`7e0c8b9`), so there is no new step type, provider, restart, cancel or
  retry path to reconcile.
