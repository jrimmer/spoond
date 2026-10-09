# DONE spoond-clcw: conformance F2, S1 and N7b on the 2.9 resume-on-use contract

2.9.0 production conformance had three failures, all asserting the
pre-2.9 `409 lease_suspended` on a suspended lease. Since `spoond-1tb5`
(#145 D2) a work call resumes a suspended lease and is served; only the
paths that cannot resume one still answer the structured 409. The three
tests now assert the 2.9 contract.

## Changes

- **F2 (`TestF2_LeaseFilesSuspendedRefused`)** — a file `GET` on a
  suspended lease is work: it resumes the lease and serves the bytes.
  The test now asserts `200`, the body matches the pre-suspend write,
  and the lease is running afterwards; the guest `cat` still confirms the
  file survived. The 409 assertion moved to the paths that cannot resume:
  the stat probe and `POST /fork`.
- **S1 (`TestS1_SuspendResumeKeepsProcesses`)** — an `exec` on a suspended
  lease resumes it and returns `200`; the test then asserts the lease is
  running, the counter did not advance through the suspended window and
  both the counter and the tmux session survived the resume. The stat
  probe and the guest heartbeat keep their 409 assertions; a five-second
  suspended window is preserved so the "did not advance" bound stays
  meaningful.
- **N7b (`TestN7b_GuestDialRefusals`)** — a dial resumes the lease and is
  then served. A fresh `py-base` lease has nothing listening on port 80,
  so the resumed dial fails with `502` from the guest dial itself. The
  test asserts `502` and then that the lease is running and an exec
  answers, which pins that the 502 came from the guest dial rather than a
  resume refusal (a refused resume is `409`/`429`/`503`/`410`, never
  `502`). The bad-port `400` and unknown-lease `404` guards are unchanged.
- **Helpers** — `client.heartbeat` posts `POST /lease/{id}/active` on the
  guest-service listener (no bearer token; the lease id is the
  capability), presenting `X-Proxy-Auth`/`Remote-User` for the
  forward-auth case, and `requireLeaseSuspended` asserts the exact 409
  `lease_suspended` code and message for the paths that still refuse.

## Gates

- `go build ./...` — clean
- `go vet ./...` — clean
- `gofmt -l .` — empty
- `go vet -tags conformance ./conformance/` — clean
- `go test -tags conformance -c ./conformance/` — compiles
- `go test -count=1 ./...` — all pass
- `go test -race -count=1 ./api/ ./store/` — all pass
- The conformance package also builds and vets as a non-root user
  (`/home/user`, build cache under `$HOME`, no `/work`, `/run/honey` or
  `/opt/honey` reads). The tests themselves exercise a live host and are
  not run here.
- `git fetch origin`: `origin/main` added nothing since the branch base,
  so there is no new step type, provider, restart, cancel or retry path
  to reconcile.
