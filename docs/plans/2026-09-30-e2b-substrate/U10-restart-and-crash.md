# U10 — Restarts and crashes: drain, recovery, periodic checkpoints

## Purpose

- **Planned restarts** of the orchestrator are lossless (R15). Every running
  sandbox is paused to disk and resumed afterwards.
- **Crashes** recover every lease that has a checkpoint, and surface the
  rest as `lost` (R16, D4).
- **Persistent leases** are checkpointed periodically, to bound crash loss.

## Preconditions

U08 is done, including item 18 (checkpoint side effects). U09 is done.

## Facts relied on

- **The orchestrator keeps its data plane in-process.**
  - A crash or restart kills every running sandbox.
  - On startup it reclaims every Firecracker process under `/fc-versions`
    (patch P1).
  - On SIGTERM it drains, bounded by `SANDBOX_DRAIN_TIMEOUT=60s` (patch P2).
  - A3 P1 and P2.
- **Pause** writes a build and stops the sandbox. **Resume** is
  `Create(snapshot=true, same sandbox_id)` (U06).
- **Checkpoint** writes a build. The source then keeps running from that
  build on a fresh network slot (U08 item 18).
- **`InfoService`:** `ServiceStatusOverride(Draining)` makes the node draining.
  - The orchestrator does not itself refuse `Create` while draining (A3 A6).
    spoond's `admit` refuses when the status is not `healthy` (U08).
  - `ServiceInfo` reports the running count and `outstanding_work` (U06
    `NodeInfo`).
- **The lease store** has `state` in
  `running|suspended|recovered|lost`, plus `last_checkpoint_build_id`,
  `last_checkpoint_at` and `recovered_from` (U05).

## Schema migration `store/migrations/0003_drain.sql` (exact)

```sql
ALTER TABLE leases ADD COLUMN drained INTEGER NOT NULL DEFAULT 0;
```

Add `Drained bool` to `Lease` and `store.LeaseRow`, and the `drained`
column to `UpsertLease` and `ListLeases`.

## Busy leases (new, `api/service.go`)

Add `busy bool` (unexported, not persisted) to `Lease`. `suspend`,
`resume`, `resumeLease` (below), `restart`, every `sub.Checkpoint` caller
(clone, fork, periodic and manual checkpoint) and the drain's pause set
`l.busy = true` under `s.store.mu` before their first `sub` call, and set
it back to `false` under `s.store.mu` when they return (use `defer`). A
second operation on a busy lease returns `409`
`{"error":"sandbox is busy; retry"}`.

## Admin endpoints (new, in `api/admin.go`)

- **Authentication:** `Authorization: Bearer <ADMIN_TOKEN>`, where
  `ADMIN_TOKEN` is a new backend env var, compared in constant time with
  `subtle.ConstantTimeCompare`. `ADMIN_TOKEN` is not a user or consumer
  token, so `authMiddleware` must let these routes through: add
  `strings.HasPrefix(r.URL.Path, "/api/admin/")` to its bypass condition
  (next to `/healthz`), and do the check in `api/admin.go`. When
  `ADMIN_TOKEN` is empty, the routes return 404. A wrong or missing token
  returns 401.
- **`POST /api/admin/drain`**:
  1. `sub.SetDraining(true)`. If it fails, respond `503`
     `{"error":"orchestrator unreachable: <err>"}` and change nothing
     (`s.draining` stays false). Otherwise set `s.draining=true`; the
     backend's pool refill, idle sweep, GC and crash reconcile skip while it
     is true.
  2. For every lease with `l.live()`, **persistent or not**:
     1. `buildID, refs, err := sub.Pause(l.SandboxID, l.TemplateID)`;
     2. insert a `pause` build row (as `suspend` does);
     3. set `State="suspended"`, `Suspended=true`, `ResumeBuildID=buildID`
        and `Drained=true`;
     4. save.

     Run up to 4 pauses concurrently. Record per-lease errors and continue.
  3. For every pool sandbox, `sub.Delete` and `removePoolLocked`.
  4. Poll `sub.NodeInfo` every 1 s until `RunningSandboxes==0` and
     `OutstandingWork==0`, up to 180 s.
  5. Respond `200`
     `{"paused":N,"failed":[{"id","error"}],"pool_deleted":M,"quiesced":bool}`.
- **`POST /api/admin/undrain`**:
  1. Wait up to 120 s for `sub.NodeInfo` to succeed.
  2. `sub.SetDraining(false)`, and set `s.draining=false`.
  3. For every lease with `Drained==true`: `resumeLease(ctx, l)`, with up
     to 4 concurrently. `resumeLease` is a new internal method holding the
     body of U08's `resume` (steps 1–4) with **no** owner check and **no**
     persistence check; U08's `resume` becomes the owner and persistence
     checks followed by a call to `resumeLease`. Drained non-persistent
     leases resume this way. On success, set `Drained=false`. On failure, set
     `State="lost"` and `Drained=false`, and record the error.
  4. Respond `200` `{"resumed":N,"failed":[{"id","error"}]}`.
- **`POST /api/admin/reconcile`**: runs `reconcileCrash` (below) now, and
  returns its summary.

## Drain CLI and systemd wiring

New subcommand `spoond drain` (package `cmd/spoond-drain`, `Main(args []string) int`,
registered in `cmd/spoond/drain.go` with build tag `//go:build !nodrain`;
add `"drain"` to `usage()` and `nodrain` to its build-tags line):

- `spoond drain --stop`: first read env `SERVICE_RESULT` (systemd sets it
  for `ExecStop`). If it is set and not `success`, print
  `drain: skipped (SERVICE_RESULT=<value>)` and exit 0: systemd runs
  `ExecStop` even after the orchestrator crashed or was killed, and a drain
  then must not run. Otherwise POST `<url>/api/admin/drain` with a timeout of
  240 s. Print the JSON. **Exit 0 even on HTTP errors or connection
  failure**, logging them: a failed drain must never block the stop.
- `spoond drain --start`: POST `<url>/api/admin/undrain` with a timeout of
  300 s. Same exit policy.
- Configuration from env:
  - `SPOOND_DRAIN_URL`, e.g. `https://127.0.0.1:18890` on staging;
  - `SPOOND_ADMIN_TOKEN_FILE`;
  - both may be comma-separated lists of the same length (U13 drains
    production and staging together). `--stop` drains each URL/token pair in
    list order; `--start` undrains each pair in list order. A failure on one
    pair is logged and the next pair still runs. Lists of different lengths:
    log the error and exit 0 without calling anything;
  - `SPOOND_DRAIN_INSECURE=1` skips TLS verification, because the
    certificate is for `vm2.lacy.casa` and the call goes to 127.0.0.1.

vm2 files:
- `/etc/e2b/drain.env` (0600):
  ```ini
  SPOOND_DRAIN_URL=https://127.0.0.1:18890
  SPOOND_ADMIN_TOKEN_FILE=/etc/spoond/admin-token-staging
  SPOOND_DRAIN_INSECURE=1
  ```
  U12 switches it to production (`:8890`, `/etc/spoond/admin-token`).
- `/etc/spoond/admin-token-staging`: `openssl rand -hex 32`, mode 0600. Add
  `ADMIN_TOKEN=<same value>` to `/etc/spoond-staging/backend.env`, then
  `systemctl restart spoond-backend-staging`.
- **The `e2b-orchestrator.service` changes (exact):** add to `[Service]`
  ```ini
  EnvironmentFile=/etc/e2b/drain.env
  ExecStop=/opt/spoond-staging/spoond drain --stop
  ExecStartPost=/opt/spoond-staging/spoond drain --start
  ```
  - `ExecStop` runs before systemd sends SIGTERM.
  - `ExecStartPost` runs after start. The orchestrator must be healthy
    first; `undrain` waits for `NodeInfo`.
  - The unit already has `TimeoutStartSec=420` and `TimeoutStopSec=330`
    (U04). Check with `systemctl show e2b-orchestrator -p TimeoutStartUSec
    -p TimeoutStopUSec`; if either is lower, set them to those values. With
    systemd's default 90 s, a slow undrain would fail the start and
    `Restart=always` would loop drain and restart.
  - U12 changes the binary path to `/opt/spoond/spoond`.

## Crash recovery (`api/recovery.go`)

`func (s *Service) reconcileCrash(ctx) (summary)`:
1. `sub.List`, into a set of live sandbox ids. If `List` fails, return
   `{"recovered":0,"lost":0}` and change nothing.
2. For every lease with `l.live()`, `!l.busy`, whose `SandboxID` is **not**
   live:
   - **If `LastCheckpointBuildID != ""`:**
     1. load the build and the image;
     2. `createSandbox(img, b, true, l.SandboxID, l)`, a resume from the
        checkpoint build **with the same sandbox id** (its `UpsertSandbox`
        replaces the stale row);
     3. on success, set `State="recovered"`, `RecoveredFrom=LastCheckpointAt`,
        `BuildID=LastCheckpointBuildID` and the new `HostIP`;
     4. on failure, set `State="lost"`.
   - **Otherwise:** set `State="lost"` and `DeleteSandbox(l.SandboxID)`.

   Save each lease, and log each at Warn:
   `recovery: lease <id> <state> (checkpoint <at>)`.
3. Delete pool entries whose sandbox is not live.
4. Call `refreshPeers`.
5. Return `{"recovered":N,"lost":M}`.

**When it runs:**
- once at backend start, after `LoadState` (this replaces U08's
  `ReconcileOrphans` marking; keep the orphan deletion);
- every 30 s in the background, only when `!s.draining`;
- immediately when the background loop sees `NodeInfo` go from failing to
  succeeding (the orchestrator came back).

**Lease API behaviour for the new states** (additive):
- The lease list and detail include `"state"`.
- A `lost` lease answers exec, stream, proxy and SSH with **410**
  `{"error":"sandbox lost in a substrate crash; delete this lease"}`.
- `recovered` behaves exactly like `running`. `state` keeps showing
  `recovered` until the lease is suspended or restarted, which reset it to
  the normal states.

## Periodic and manual checkpoints (`api/checkpoint.go`)

- **Background:** every `CHECKPOINT_INTERVAL_MINS` (skip entirely when 0),
  for each lease with `Persistent`, `l.live()`, `!l.busy`, and
  `LastActive.After(LastCheckpointAt)`:
  1. `sub.Checkpoint`;
  2. insert a `checkpoint` build row with parent = the previous
     `l.BuildID`;
  3. apply U08 item 18.

  One lease at a time, 2 s apart. Measure each Checkpoint call's duration
  into the histogram `spoond_checkpoint_duration_seconds`.
- **New route `POST /api/sandboxes/{id}/checkpoint`:** owner only, leases
  with `l.live()` only (409 otherwise, including busy). It does the same as above for one lease and
  responds `200` `{"id","build_id","at"}`.

## Tests

- **Unit tests** (fake substrate):
  - drain pauses every running lease and marks it `Drained`; undrain resumes
    exactly those;
  - drain continues past one failed pause;
  - `reconcileCrash`: a lease with a checkpoint → `recovered` with the same
    sandbox id in the fake's `Create` call; without a checkpoint → `lost`;
  - periodic checkpoint skips idle leases;
  - a `lost` lease returns 410 on exec.
- **Redeploy staging first** (Ops runner), after this unit's commits are
  merged into `feat/e2b-substrate`:
  ```bash
  export PATH=/usr/local/go/bin:$PATH
  cd /root/src/spoond && git fetch && git checkout feat/e2b-substrate && git pull --ff-only
  go build -o /opt/spoond-staging/spoond ./cmd/spoond
  systemctl restart spoond-backend-staging spoond-sshd-gateway-staging
  ```
- **Conformance** against staging, in a window, with the U08/U09 staging
  settings (including `CONFORMANCE_BACKEND_UNIT=spoond-backend-staging`):
  - `CONFORMANCE_DESTRUCTIVE=1`: R1 (planned restart), R2 (crash) and R3
    (backend restart) pass;
  - `restart_total_ms` ≤ 120000.

## Commits

1. `feat(api): admin drain and undrain; drained lease flag`
2. `feat(api): crash recovery from checkpoints; lost and recovered lease states`
3. `feat(api): periodic and manual checkpoints`
4. `feat(cmd): spoond drain, wired into the orchestrator unit`

## Done when

The unit tests and conformance R1–R3 pass against staging.

## Do not

- Do not attempt to keep sandboxes running across an orchestrator restart
  (D4).
- Do not checkpoint non-persistent leases periodically.
