# DONE spoond-g077: a failed List must not become NotFound/410

Incident 2026-10-08 22:29 PDT (cytale run 3441, job 3901): `substrate/e2b/envd.go`
`listed()` returned false whenever the orchestrator `List` call failed, so a
stream drop during an orchestrator stall was reported as `NotFound`, which the
API answered as `410 lease_lost`. `410` is FINAL for clients (Honey treats it
as gone with no confirming GET; the runner fails the job permanently). The
backend logged `list sandboxes failed: DeadlineExceeded` that night.

## Changes

1. **New sentinel `substrate.ErrUnavailable`** (`substrate/errors.go`): the
   sandbox's state is unknown (the confirming `List` failed), so the caller
   answers a retryable error and never marks the lease lost.

2. **`listed()` became `sandboxListed(ctx, sandboxID) error`**
   (`substrate/e2b/envd.go`). It returns:
   - `nil` when `List` succeeds and names the sandbox (the operation failure
     was transient);
   - an error wrapping `substrate.ErrNotFound` when `List` succeeds and the
     sandbox is absent (the one case that is a confirmed loss);
   - an error wrapping `substrate.ErrUnavailable` when every `List` attempt
     fails.
   `List` is retried `listProbeAttempts = 3` times with
   `listProbeBackoff = 250ms`, each attempt under `listProbeTimeout = 5s` (a
   fresh bound from the caller's context, so a hung orchestrator cannot hold
   the caller). Callers: `Start`, `Exec` (both the initial `startProcess`
   failure and a stream `EventError`), `WriteFile`.

3. **Which code: `substrate_unavailable`, not `capacity_wait`.** The existing
   capacity code is `substrate.ErrCapacity` (`503` "capacity: …"); reusing it
   would tell clients the node refused for room, which is false and would push
   them into the admission path. `substrate_unavailable` says what happened:
   the sandbox's state could not be confirmed. The docs say so explicitly.

4. **API mapping.** New `Server.writeSubstrateUnavailable` (503,
   `Retry-After: 5`, `{"code":"substrate_unavailable"}`) and
   `Server.writeSandboxOpError`, which dispatches `ErrUnavailable` to the 503
   and `ErrNotFound` to the existing `writeSandboxGone` (409 busy / 410 lost).
   `substrateUnknownRetryAfterSecs = 5` and `substrateUnknownStatusCode = 503`.

5. **Every caller of `listed()`/`NotFound` audited.** Where each lands on an
   unknown (`List` failed) versus a confirmed absence:

   | Path | Where | Confirmed absent | Unknown |
   |---|---|---|---|
   | exec | `api/server.go` `handleExec` | 410 `lease no longer exists` (no code) | 503 `substrate_unavailable` |
   | stat probe | `api/server.go` `handleStat` | 410 `lease no longer exists` (no code) | 503 `substrate_unavailable` |
   | exec stream | `api/server.go` `handleStream` | WS `error` frame | WS `error` frame with `substrate: unavailable` (upgrade already committed) |
   | background exec | `api/jobs_http.go` `handleBackgroundExec` | 410 `lease no longer exists` (no code) | 503 `substrate_unavailable` |
   | files (stat/download/upload/mkdir/remove) | `api/files.go` `mapFileError` | 404 `file not found` | 503 `substrate_unavailable` |
   | guest dial | `api/guestdial.go` `handleGuestDial` | 410 `lease no longer exists` (no code) | 503 `substrate_unavailable` |
   | job output | `api/jobs_http.go` `handleJobGet`/output | empty output | 503 `substrate_unavailable` |
   | job signal | `api/jobs_http.go` `handleJobSignal` | 409 `job is not running` | 503 `substrate_unavailable` |
   | proxy | `api/proxy.go` | (no substrate-list check; dials directly) | unchanged 502 |
   | recovery | `api/recovery.go` `reconcileCrash` | marks lost only when `List` succeeds and the sandbox is absent | takes no action on a failed `List` (already correct) |
   | lost-lease guard | `api/recovery.go` `ensureLive`/`lostErr` | reads store state only; no substrate `List` | unaffected |
   | rootfs probe | `api/rootfs_probe.go` `probeRootfsOnce`/`probeRootfsLeases` | a transport failure counts toward recovery | `substrate.ErrUnavailable` is its own `rootfsProbeUnavailable` outcome: logged and metered but **never counted** toward the 3-strike threshold, so a mixed pass cannot delete a live sandbox or mark the lease lost (round-2 F1) |
   | no-room refusal | `api/idle_suspend.go` `writeResumeRefusal` | n/a | `substrate.ErrUnavailable` falls to the generic 500 (only reachable if a resume's create reports it) |

   The proxy is unaffected: it does not consult `listed()`, it dials the
   orchestrator proxy directly and its failures stay `502`.

6. **Docs.** `docs/api.md` gains a `substrate_unavailable` row in the code
   table, a **Substrate unavailable** section (the incident rationale, the
   `substrate_unavailable`-not-`capacity_wait` choice, and the per-path status
   table for Honey) and a sentence in the exec section. `CHANGELOG.md`
   `[Unreleased]` gets the fixed entry.

## Tests

- `substrate/e2b/listed_test.go` (new): absent → `ErrNotFound`; present →
  nil; list error → `ErrUnavailable` after exactly the bounded retry and never
  `ErrNotFound`; a list that fails once then succeeds → nil; a hung list →
  `ErrUnavailable` within the per-attempt bound.
- `substrate/e2b/files_requests_test.go`: `TestWriteFileUploadFails` now pins
  `ErrUnavailable` and that it is not `ErrNotFound`.
- `api/substrate_unavailable_test.go` (new):
  `TestExecSubstrateUnavailableIsRetryable` (503 + Retry-After +
  `substrate_unavailable`, lease not lost) and
  `TestExecSandboxConfirmedGoneIsStillGone` (410 unchanged).
- `api/files_test.go`: `TestFilesSubstrateUnavailable` (503 +
  `substrate_unavailable` from a file stat, lease not lost).
- `api/rootfs_probe_test.go`: `TestRootfsProbeUnavailableDoesNotCount` (round-2
  F1).

### Test-to-mutation map (round 2)

Every mutation the round-2 fixes guard against has a test that fails when the
fix is reverted:

| Test | Mutation it kills |
|---|---|
| `TestRootfsProbeUnavailableDoesNotCount` (`api/rootfs_probe_test.go`) | Reverting F1: treating `ErrUnavailable` as transport/EIO and counting it. In the mixed pass the unknown lease reaches the threshold, so the test sees it recovered (or deleted) and fails. |
| `TestSandboxListedListErrorIsUnavailable` (`substrate/e2b/listed_test.go`) | Changing `listProbeAttempts` (the test now asserts a literal `3`, so a retry-count change fails it). |
| `TestExecSubstrateUnavailableIsRetryable` | Mapping `ErrUnavailable` to 410 (or not keeping the lease). |
| `TestExecSandboxConfirmedGoneIsStillGone` | Mapping a confirmed not-found away from 410. |
| `TestFilesSubstrateUnavailable` | Dropping the `ErrUnavailable` branch in `mapFileError`. |
| `TestWriteFileUploadFails` | Reporting a failed `List` as `ErrNotFound` again. |

### Round-2 review fixes

- **F1 (medium):** `probeRootfsOnce` now classifies `substrate.ErrUnavailable`
  as the new `rootfsProbeUnavailable` outcome. `probeRootfsLeases` logs and
  meters it but never counts it toward `rootfsProbeFailuresThreshold`, and an
  all-unknown pass is treated like an all-transport one. Previously a mixed
  pass (a fast-failing orchestrator mid-pass) could reach the 3-strike
  threshold on an unknown and recover, deleting a live sandbox (or losing the
  lease) on a non-answer.
- **F2 (docs):** the Honey status table now lists exactly the paths that can
  produce `substrate_unavailable` on e2b (only `Start`, `Exec` and `WriteFile`
  call the sandbox-list check): `POST exec`, `GET stat`, background exec,
  file `PUT`/dir remove, job signal, exec stream. The guest-dial, job
  get/output and file `GET`/`stat`/`mkdir` defensive branches are kept in code
  but are not documented as live.
- **F3 (docs):** confirmed-absence wording corrected: `writeSandboxGone`
  answers `{"error":"lease no longer exists"}` with **no** code; only an
  already-lost lease carries `code: lease_lost`.
- **F4:** the stat, files, job-signal and background-exec 503 paths now log
  one line with the lease id, op and "substrate unavailable", like
  `handleExec`.
- **T1:** `TestSandboxListedListErrorIsUnavailable` asserts a literal `3`
  instead of comparing to `listProbeAttempts`, so a retry-count change fails
  it.

## Gates

- `go build ./...` — clean
- `go vet ./...` — clean
- `gofmt -l .` — clean
- `go test -p 2 ./...` — all pass
- `go test -race ./api/ ./substrate/...` — all pass
