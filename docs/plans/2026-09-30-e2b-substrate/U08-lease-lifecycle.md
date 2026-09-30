# U08 — Lease lifecycle on E2B

## Purpose

Rewire `api.Service` and `api.Server` from the forkd client onto
`substrate.Substrate` and SQLite. Every lease API route keeps its contract
(D5). A few routes are added. This unit also deploys the **staging**
backend on vm2.

## Preconditions

U05, U06 and U07 are done, and all 7 images have `current_build_id` in
`/var/lib/spoond/staging.db`.

## Facts relied on

- `Service` methods, their current forkd calls and mutation points: A1 §3.6,
  §3.8–§3.16, and U05's Facts table.
- Handlers and request/response shapes: A1 §5.2–§5.10, and U02's Facts list.
- Handlers that call forkd directly: `handleExec`, `handlePrompt`,
  `handleStat` (`Exec`), `handleClone` (`Branch`), `handleMetrics`
  (`Metrics`). `ImageRegistry` calls `SnapshotExists` and `ListSnapshots`.
- `buildShellArgs` wraps commands as `/bin/bash -c` (A1 §5.10).
- Exec timeout: default 30 s, capped by `MAX_EXEC_TIMEOUT_SECS` (default
  300). A forkd 404 maps to HTTP 410 (A1 §5.9).
- Stream protocol (A1 §5.6):
  - client first frame `{"args","cwd","env","pty"(default true)}`;
  - server frames:
    - `{"stream":"started","pid":N,"pty":bool}`
    - `{"out":"..."}`
    - `{"exit_code":N}`
    - `{"error":"..."}`
  - client frames `{"in":"..."}` and `{"action":"stop"}`;
  - gorilla/websocket, text frames, `CheckOrigin` always true.
- E2B semantics are in U06's Facts. The memory of a sandbox equals its
  build's memory, so a per-lease memory override is impossible on a
  snapshot restore.

## Schema migration `store/migrations/0002_image_env.sql` (exact)

```sql
ALTER TABLE images ADD COLUMN env TEXT NOT NULL DEFAULT '{}';
```

- Add `Env map[string]string` to `store.ImageRow`, stored as JSON.
- Update `spoond images build` (U07 step 4) to set `Env` from the manifest
  entry's `env`.
- Re-run `spoond images build --all` on staging after this migration.

## Service changes (`api/service.go`)

1. **Replace** the field `forkd ForkdClient` with `sub substrate.Substrate`.
   Delete the `ForkdClient` interface and every forkd call. `db *store.DB`
   is now **required**; the helpers from U05 stop checking for nil.
2. **Constructor:** replace `NewService`/`NewServiceWithIdle` with

   ```go
   type ServiceConfig struct {
   	PoolSize          int
   	DefaultTTL, MaxTTL, IdleTimeout time.Duration
   	HostGuestAddr     string // HOST_GUEST_SERVICE_ADDR
   	HostGuestPort     int    // HOST_GUEST_SERVICE_PORT
   	CheckpointEvery   time.Duration // U10
   }
   func NewService(sub substrate.Substrate, db *store.DB, tokens map[string]string, cfg ServiceConfig) *Service
   ```
3. **`Lease` fields.**
   - Rename `ForkdID` → `SandboxID`.
   - Remove `Workspace`.
   - Add `HostIP string`, `BuildID string` (the build the running sandbox
     was created from) and `TemplateID string`.
   - `Address` becomes `HostIP`, with no port.
   - Update `leaseToRow`/`rowToLease`. The `sandboxes` table records
     `build_id`; `leases` keeps the U05 columns.
   - **Persistence of the new fields** (no new lease columns):
     - `HostIP` is stored in the `address` column;
     - `TemplateID` is not stored: `LoadState` sets it from
       `images.template_id` for the lease's `image`;
     - `BuildID` is stored in the `sandboxes` row. `LoadState` sets it from
       the row whose `lease_id` equals the lease id. A suspended lease has
       no such row, and its `BuildID` is `""` (`ResumeBuildID` applies).
   - `Suspended` stays, derived from `State=="suspended"`.
   - **Delete the `Address` field.** Every response that emitted
     `"address": l.Address` now emits `"address": l.HostIP`.
   - **`live()` helper** (new, in `api/service.go`):
     ```go
     // live reports whether the lease has a running sandbox. "recovered" is
     // introduced in U10 and behaves exactly like "running".
     func (l *Lease) live() bool { return l.State == "running" || l.State == "recovered" }
     ```
     Every "running lease" test in U08, U09, U10 and U11 uses `l.live()`,
     never `l.State == "running"`.
   - **Sandbox rows** use the U07 store API: `UpsertSandbox` after every
     successful `sub.Create` (fields from the returned `substrate.Sandbox`,
     `LeaseID = l.ID`, or `""` for pool sandboxes), `DeleteSandbox` wherever
     this unit says "delete the `sandboxes` row", `GetSandboxByLease` in
     `LoadState`, `GetSandbox` for pool entries.
4. **Admission** (new, `api/admission.go`):
   `func (s *Service) admit(ctx, memoryMB int) error`:
   - Call `sub.NodeInfo`.
   - Compute `free = (HugepagesTotal - HugepagesUsed - HugepagesReserved) * HugepageSizeBytes`.
   - If `free < memoryMB*1024*1024` or `Status != "healthy"`, return
     `substrate.ErrCapacity`.
   - The handlers map `ErrCapacity` to **HTTP 503**
     `{"error":"capacity: <reason>"}`.
5. **Create helper** (new):
   `func (s *Service) createSandbox(ctx, img store.ImageRow, b store.BuildRow, resume bool, sandboxID string, l *Lease) (substrate.Sandbox, error)`:
   1. `admit(b.MemoryMB)`.
   2. If `sandboxID == ""`, set `sandboxID = e2b.NewSandboxID()`.
   3. Build `CreateRequest`:
      - `TemplateID=b.TemplateID`, `BuildID=b.BuildID`;
      - versions from `b`;
      - `VCPU/MemoryMB/DiskMB` from `b`;
      - `Resume=resume`;
      - `EnvVars` = `img.Env` merged with
        `{"SPOOND_LEASE_ID": l.ID, "SPOOND_GATEWAY_URL": "http://"+HostGuestAddr+":"+HostGuestPort}`;
      - `Metadata={"lease_id": l.ID, "owner": l.Owner}`;
      - `EndAt=l.ExpiresAt`;
      - `Egress = s.egressFor(l)` (below).
   4. Call `sub.Create`.
   5. On success, `UpsertSandbox` the row (upsert, because resume reuses the
      sandbox id).
   6. Record `spoond_create_duration_seconds` only from U11 on.
6. **Egress mapping** `func (s *Service) egressForLocked(l *Lease) substrate.Egress`
   (caller holds `s.store.mu`), and
   `func (s *Service) egressFor(l *Lease) substrate.Egress`, which takes
   `s.store.mu`, calls `egressForLocked`, and releases it. `peerAllowances`
   (U09) reads other leases, so it is always called with `s.store.mu` held
   (from `egressForLocked`). Call `egressFor` wherever the lock is not held,
   and `egressForLocked` where it is (item 18).
   `hostSvc` is the allowance
   `{CIDR: HostGuestAddr+"/32", TCPPorts: [HostGuestPort]}`, and `dns` is
   `{CIDR: "10.1.0.1/32", TCPPorts: [53]}`.

   | Policy | Egress |
   |---|---|
   | `none` | `DeniedCIDRs=["0.0.0.0/0"]`, nothing else |
   | `internet` | `Private=[hostSvc, dns] + s.peerAllowances(l)` |
   | `lan` | `DeniedCIDRs=["0.0.0.0/0"]`, `Private=[lanRanges..., hostSvc, dns] + s.peerAllowances(l)`, where lanRanges are the CIDRs below with empty `TCPPorts` |
   | `restricted` | `DeniedCIDRs=["0.0.0.0/0"]`, `AllowedDomains` = the domain entries of `NetAllow`, `AllowedCIDRs` = the IP/CIDR entries of `NetAllow` (public ones), `Private` = `[hostSvc, dns]` + the private IP/CIDR entries of `NetAllow` (empty ports) + `s.peerAllowances(l)` |

   **`lanRanges` (exact).** These are RFC 1918 minus the sandbox networks
   `10.11.0.0/16` and `10.12.0.0/16`:
   - `10.0.0.0/13`
   - `10.8.0.0/15`
   - `10.10.0.0/16`
   - `10.13.0.0/16`
   - `10.14.0.0/15`
   - `10.16.0.0/12`
   - `10.32.0.0/11`
   - `10.64.0.0/10`
   - `10.128.0.0/9`
   - `172.16.0.0/12`
   - `192.168.0.0/16`

   `peerAllowances` is implemented in U09. In U08 it returns `nil`.

   **Classifying a `NetAllow` entry:**
   - it parses as an IP → a `/32` CIDR;
   - it parses as a CIDR → a CIDR;
   - otherwise it is a domain.

   A CIDR is private if it lies within 10/8, 172.16/12, 192.168/16,
   127/8 or 169.254/16. The patch's host-address guard still protects the
   host (U03 P4).

   **Default policy** when empty: `restricted`, as today (A1 §5.4). Remove
   the `hostBridgeAllow` constant **and** the line
   `req.NetAllow = append(req.NetAllow, hostBridgeAllow...)` in
   `handleCreate`; `hostSvc` replaces them.
7. **`grant`** (rewritten):
   1. Load `img := db.GetImage(image)`; if missing or
      `CurrentBuildID==""`, return an "unknown image" error (→ 404).
   2. Load `b := db.GetBuild(img.CurrentBuildID)`.
   3. Reserve quota (unchanged).
   4. **Pool:** if `PoolSize>0`, pop the oldest pool entry for the image.
      The pool serves persistent and non-persistent grants alike.
      Discard it (`sub.Delete` and `removePoolLocked`) if its sandboxes row
      has a `build_id` other than `img.CurrentBuildID`, or if `sub.Health`
      fails; then try the next one.
      - A pooled sandbox was created with a placeholder lease, so call
        `sub.UpdateEgress(id, egressFor(l))` and
        `sub.UpdateEndAt(id, l.ExpiresAt)` on it, then `UpsertSandbox` its
        row with `LeaseID = l.ID`.
      - Its envd default `SPOOND_LEASE_ID` is `pool` (env vars cannot be
        updated after create). So for a lease served from the pool, set
        `l.pooled = true` (new unexported field, not persisted), and
        `handleExec`, `handlePrompt`, `handleStat` and `handleStream` add
        `SPOOND_LEASE_ID=<lease id>` to each request's env when
        `l.pooled` is true.
   5. With no usable pool entry, call `createSandbox(img, b, false, "", l)`.
   6. Run the integrity probe (unchanged logic, through `exec`) when
      enabled. On probe failure, `sub.Delete` and return an error.
   7. Persist the lease with `State="running"`, `SandboxID`, `HostIP`,
      `BuildID=b.BuildID`, `TemplateID`.
   8. **On any error after the sandbox exists, `sub.Delete` it.** This fixes
      the leak in A1 §17 item 7.
8. **`warmPool`:**
   - Images are those with a non-empty `current_build_id` (from
     `db.ListImages`).
   - Pool sandboxes use a placeholder lease `{ID:"pool", Owner:"pool",
     ExpiresAt: now+24h, NetPolicy:"none"}`.
   - Their `sandboxes.lease_id` is `""`.
   - `refillPool` is unchanged otherwise.
9. **`suspend`** (any persistent lease; workspaces no longer exist):
   1. `buildID, refs, err := sub.Pause(l.SandboxID, l.TemplateID)` (`refs` is
      stored from U11 on).
   2. `InsertBuild{kind: pause, parent: l.BuildID, source_sandbox_id: l.SandboxID, template_id, image, state: ready, versions and sizes copied from the parent build row}`.
   3. Delete the `sandboxes` row.
   4. Set `State="suspended"`, `Suspended=true`, `ResumeBuildID=buildID`.
   5. Save.

   A non-persistent lease returns HTTP 400 (as today for non-workspace
   leases).
10. **`resume`:**
    1. Load `b := GetBuild(l.ResumeBuildID)` and the image row.
    2. Call `createSandbox(img, b, true, l.SandboxID, l)`. **Same sandbox
       id.**
    3. Set `HostIP` (new), `BuildID = l.ResumeBuildID`, `State="running"`,
       `Suspended=false` and `LastActive=now`.
    4. Save.
11. **`restart`:**
    - Persistent and running: `suspend`, then `resume`.
    - Persistent and suspended: `resume`.
    - Non-persistent:
      1. `sub.Delete`;
      2. `createSandbox` from the image's **current** build with a new
         sandbox id;
      3. update the lease;
      4. keep the lease id.

    This decides A1 §17 item 4.
12. **`release`:** `sub.Delete(l.SandboxID)` (nil if gone), delete the
    `sandboxes` row, then the lease row, and remove shares (cascade). Builds
    are left for U11's GC.
13. **`clone`** (new Service method, replacing `grantFromSnapshot`):
    1. `buildID, refs, err := sub.Checkpoint(src.SandboxID)` (`refs` is
      stored from U11 on).
    2. `InsertBuild{kind: checkpoint, parent: src.BuildID, source_sandbox_id: src.SandboxID, ...copied fields}`.
    3. Create a new **persistent** lease for the caller, with
       `NetPolicy/NetAllow/ExposePorts` **copied from the source**,
       `Image = src.Image` (never a tag or build id; `TemplateID` is looked
       up by image name), `ExpiresAt = now + maxTTL`, `State = "running"`.
       The request's optional `tag` is accepted and ignored. This decides
       A1 §17 item 5.
    4. `createSandbox(img, checkpointBuild, false, "", newLease)`.
    5. Return the lease and `buildID`.
14. **`fork`** (new): `fork(ctx, owner, srcID string, count int, persistent bool, ttl time.Duration) ([]*Lease, string, error)`:
    1. `count` must be 1..20, otherwise 400.
    2. Reserve quota for `count` leases up front, all or nothing (429 when
       short).
    3. Call `Checkpoint` once and insert the checkpoint build row.
    4. Create `count` leases and sandboxes from the checkpoint build. Each
       lease: owner = caller (quota charged to the caller),
       `Image = src.Image`, `NetPolicy/NetAllow/ExposePorts` copied from the
       source, `Persistent = persistent`, `ExpiresAt = now + ttl` where
       `ttl` = the request's `ttl` seconds, or `defaultTTL` when 0, capped at
       `maxTTL` and the user's `MaxTTL`; `State = "running"`.
    5. If any create fails, `sub.Delete` every sandbox created in this call,
       release the reservations, and return the error.
    6. Return the leases and the checkpoint build id.
15. **`sweepExpired`:** unchanged logic, except that idle-suspend now
    applies to every persistent lease (not only workspace leases), and
    release uses the new `release`.
16. **`ReconcileOrphans`:**
    - `sub.List`. If it fails, log `reconcile: list sandboxes failed: <err>`
      and return without changing anything (never mark leases lost).
    - Delete every sandbox whose id is in neither a lease's `SandboxID` nor
      the pool.
    - For every lease with `l.live()` whose sandbox is missing, set
      `State="lost"` (U10 upgrades this to recovery).
    - Delete pool entries whose sandbox is missing.
17. **Remove** `fillEndpoint`, `resolveEndpoint`, the `Endpoint` type,
    `applyNetpol`, `SetNetpol`, and the `netpol` and `netpolDNS` fields.
    Everything that called them gets this **interim** behaviour until U09:
    - `handleEndpoint` returns `200`
      `{"id":l.ID,"forkd_id":l.SandboxID,"image":l.Image,"netns":"","guest_addr":l.HostIP}`.
    - `handleProxy` (`api/proxy.go`) keeps its lookup and auth logic, but its
      target becomes `net.JoinHostPort(lease.HostIP, strconv.Itoa(port))`
      and its `Transport` is a plain `&http.Transport{IdleConnTimeout: 30 * time.Second}`
      (no `dialInNetns`). A lease without `HostIP` returns `502`
      `sandbox not running`.
    - `CanExposePorts()` returns `false`, so `handleCreate` keeps returning
      `501` for `expose_ports` until U09.
    - `api/netns_linux.go`, `api/netns_other.go` and `api/netpolicy.go` stay
      (U09 deletes them); `NetnsPolicyApplier` stays unused.
    - `cmd/spoond-backend/main.go` drops the `NETPOL_DNS` block and its
      `SetNetpol` call.
    - Tests that exercised `applyNetpol` through `fakeNetpol`/`fakeExposer`
      (`api/netpolicy_test.go`, `api/expose_test.go`) keep only their
      assertions on `policyCommands`/`exposeCommands`/`ValidateExposePorts`
      until U09.
18. **After every `sub.Checkpoint(src)` call** (clone, fork, and U10's
    periodic and manual checkpoints): the source keeps running from the new
    build (A2 §3.5 and §3.6, the "resume-fresh" path). Its host IP may
    change, so it is always re-read. Under `s.store.mu`:
    - set `src.BuildID = <checkpoint build id>`, `src.LastCheckpointBuildID`
      to the same, and `src.LastCheckpointAt = now`;
    - re-read the source with `sub.List`, and set `src.HostIP` to that
      entry's `HostIP`;
    - `UpsertSandbox` the source's row with the new `build_id` and
      `host_ip`;
    - `saveLeaseLocked(src)`.

    Then (after unlocking) call `refreshPeers` (U09; a no-op in U08).

## Server changes (`api/server.go` and friends)

1. **`handleCreate`:**
   - `memory_mib`: `0` is fine; if it equals the image's `memory_mb`,
     accept; otherwise return **400**
     `{"error":"memory is fixed per image on this backend: <image> has <N> MiB"}`.
   - `network` and `init_cmd` stay ignored (as today).
   - Map errors: `ErrCapacity` → 503.
   - The response is unchanged. `address` = `HostIP`.
2. **`handleExec`, `handlePrompt`, `handleStat`:**
   - Keep the existing log lines `exec: <id>: ...` and `create: grant ...`
     (with `lease.SandboxID` in place of `lease.ForkdID`); the Autonomous
     window protocol's `window_idle` reads them. `handleStream` logs
     `stream: <lease id>: start` when a process starts.
   - Call
     `s.svc.sub.Exec(ctx, lease.SandboxID, substrate.ExecRequest{Args: buildShellArgs(...), Timeout: time.Duration(timeout)*time.Second})`.
   - `substrate.ErrNotFound` → **410**
     `{"error":"sandbox no longer exists"}`.
   - The response shape is unchanged: `stdout`, `stderr`, `exit`.
3. **`handleStream`** (rewritten onto `sub.Start`). The pre-upgrade checks
   are exactly: `acquireBusy(owner)` (429), `lookupWithShare(owner, id,
   ShareHTTP)` (404), `touch(id)`, and `Suspended` → `409`. The old
   `resolveEndpoint` call is gone. After the upgrade:
   1. Read the first frame exactly as today.
   2. Call `sub.Start(lease.SandboxID, StartRequest{Args, Env, Cwd, PTY: pty, Cols: 80, Rows: 24, Stdin: true})`.
      - On error, send `{"error":"agent unreachable: <err>"}` and close.
   3. **Server → client**, one text frame per event (the relay keeps
      running until `EventExit` or `EventError`):
      - `EventStarted` → `{"stream":"started","pid":P,"pty":bool}`
      - `EventStdout`, `EventStderr` and `EventPTY` → `{"out":"<data as UTF-8 string>"}`.
        stderr is now forwarded; the forkd agent dropped it without a PTY.
      - `EventExit` → `{"exit_code":N}`, then close.
      - `EventError` → `{"error":"<err>"}`, then close.
      - Frames have **no** trailing newline. (The forkd relay included the
        agent's `\n`; JSON parsers ignore it either way.)
   4. **Client → server:**
      - `{"in":"<text>"}` → `proc.Write([]byte(text))`;
      - `{"action":"stop"}` → `proc.Signal(false)` (SIGTERM), then stop
        reading client frames, but keep relaying server events until
        `EventExit` or `EventError`, then close;
      - **new, additive:** `{"resize":{"cols":C,"rows":R}}` →
        `proc.Resize(C,R)`;
      - `{"action":"eof"}` → `proc.CloseStdin()` (non-PTY);
      - ignore anything else.
   5. When the WebSocket closes first, call `proc.Close()`. Do **not** kill
      the process. That matches forkd, where the agent saw EOF.
4. **`handleClone`:**
   - Calls `svc.clone`.
   - Response `201`:
     `{"id","image","source","branch_tag":<checkpoint build id>,"persistent":true,"expires_at"}`.
5. **New `POST /api/sandboxes/{id}/fork`:**
   - Body `{"count":int,"persistent":bool(optional, default false),"ttl":int(optional)}`.
   - Owner only, as for clone (`s.svc.lookup(owner, id)`; clone does not
     accept shares).
   - Response `201` `{"source":id,"build_id":"...","ids":[...]}`.
   - Errors: 400 bad count, 404 unknown, 409 suspended, 429 quota,
     503 capacity.
6. **New `GET /api/sandboxes/{id}`:** returns the same object as one element
   of `GET /api/sandboxes`, plus `"state"`, `"recovered_from"` (RFC 3339 or
   `""`) and `"last_checkpoint_at"`. Owner or an `http` share is required.
7. **`GET /api/images`:**
   - Returns `{"images":[names]}` from `db.ListImages` with a non-empty
     `current_build_id`, sorted.
   - With `?detail=1`, it returns
     `{"images":[{"name","build_id","template_id","digest","vcpu","memory_mb","disk_mb","updated_at"}]}`.
   - Replace `ImageRegistry` with this DB-backed implementation.
8. **`handleMetrics`:** remove the forkd passthrough. It serves spoond's own
   metrics only (U11 adds the OTel passthrough).
9. **`handleEndpoint`:** unchanged until U09, which rewrites it.

## `cmd/spoond-backend/main.go`

- `cfg, err := e2b.FromEnv()`, then `sub, err := e2b.New(cfg)`. Exit with a
  message on either error.
- Build `NewService(sub, db, tokens, ServiceConfig{...})` from env:
  - `POOL_SIZE`, `DEFAULT_TTL_SECS`, `MAX_TTL_SECS`, `IDLE_TIMEOUT_SECS`
    (existing);
  - `HOST_GUEST_SERVICE_ADDR` (required);
  - `HOST_GUEST_SERVICE_PORT` (default `8891`);
  - `CHECKPOINT_INTERVAL_MINS` (U10).
- Remove `FORKD_URL`, `FORKD_TOKEN`, `FORKD_HTTP_TIMEOUT_SECS`,
  `KNOWN_IMAGES` and `NETPOL_DNS` handling. Images now come from the DB.

## Tests

- **Port every test in `api/*_test.go`** from the fake `ForkdClient` to
  `substrate/fake`:
  - Tests of workspace-specific behaviour become the equivalent
    pause/resume tests.
  - Tests asserting forkd call sequences assert the fake's `Calls` log
    instead: `Create`, `Pause`, `Checkpoint`, `Delete`.
  - Delete no test without replacing its behavioural assertion.
- **New unit tests:**
  - `egressFor`, for each policy, with exact expected slices;
  - `admit`, with free hugepages below and above the need;
  - `grant` deletes the sandbox when the probe fails;
  - `fork` with count 3 when the 2nd create fails: all created sandboxes
    are deleted and quota is released;
  - `clone` copies the source policy;
  - `handleCreate` `memory_mib` handling;
  - the stream relay: with fake events, assert the exact frames, including
    the `resize` and `eof` client frames.

## Staging deployment on vm2 (exact)

1. Build: `export PATH=/usr/local/go/bin:$PATH && cd /root/src/spoond && git fetch && git checkout feat/e2b-substrate && git pull --ff-only && go build -o /opt/spoond-staging/spoond ./cmd/spoond`.
2. Write `/etc/spoond-staging/backend.env` (mode 0600):
   ```ini
   BIND_ADDR=0.0.0.0:18890
   PROXY_ADDR=0.0.0.0:18891
   SPOOND_DB_PATH=/var/lib/spoond/staging.db
   USERS_FILE=/var/lib/spoond/staging-users.json
   POOL_SIZE=0
   E2B_GRPC_ADDR=127.0.0.1:5008
   E2B_PROXY_URL=http://127.0.0.1:5007
   E2B_TOKEN_SEED_FILE=/etc/spoond/e2b-token-seed
   E2B_TEAM_ID=5b0f4e3a-8c1d-4f2e-9a6b-7d3c2e1f0a95
   IMAGE_REGISTRY=localhost:5000
   HOST_GUEST_SERVICE_ADDR=10.1.0.11
   HOST_GUEST_SERVICE_PORT=18891
   CHECKPOINT_INTERVAL_MINS=60
   ```
   - Also add, each `<hexN>` a new value from `openssl rand -hex 32`:
     ```ini
     CONSUMER_TOKENS=<hex1>=staging-admin,<hex2>=staging-gateway
     GATEWAY_TOKEN=<hex2>
     SPOOND_BACKUP_DIR=/var/lib/spoond/backups-staging
     BOOTSTRAP_TOKEN=<hex3>
     PROXY_AUTH_SECRET=<hex4>
     ```
     The backend exits without `CONSUMER_TOKENS`, and the bootstrap call in
     step 5 authenticates with `<hex1>`. `<hex2>` is the staging gateway's
     token (U09).
   - Copy `TLS_CERT`, `TLS_KEY`, `ASSETS_DIR`, `PROXY_AUTH_MODE`,
     `PROXY_AUTH_TRUSTED_PEERS` and every `LLM_*` line **verbatim** from
     `/etc/forkd-backend.env` (the path recorded in U02 step 0), when
     present there.
3. `/etc/systemd/system/spoond-backend-staging.service` (exact):
   ```ini
   [Unit]
   Description=spoond lease API backend (staging, E2B substrate)
   After=network-online.target e2b-orchestrator.service
   Wants=network-online.target

   [Service]
   Type=simple
   ExecStart=/opt/spoond-staging/spoond backend
   Restart=on-failure
   RestartSec=3
   EnvironmentFile=/etc/spoond-staging/backend.env
   User=root
   Group=root

   [Install]
   WantedBy=multi-user.target
   ```
   Before step 2, run `install -d -m 700 /etc/spoond-staging`.
4. `systemctl daemon-reload && systemctl enable --now spoond-backend-staging`.
5. **Staging conformance user (Ops runner).** Bootstrap the first staging
   identity user, which becomes admin, and write the staging conformance
   env file:
   ```bash
   set -a; . /etc/spoond-staging/backend.env; set +a
   BEARER=${CONSUMER_TOKENS%%=*}            # <hex1>
   API=https://vm2.lacy.casa:18890
   ssh-keygen -t ed25519 -N '' -C conformance-staging -f /etc/spoond-staging/conformance_ed25519
   FP=$(ssh-keygen -lf /etc/spoond-staging/conformance_ed25519.pub | awk '{print $2}')   # SHA256:...
   TOK=$(openssl rand -hex 32)
   USER_ID=$(curl -fsS -H "Authorization: Bearer $BEARER" -H "X-Bootstrap-Token: $BOOTSTRAP_TOKEN" \
     -d "{\"name\":\"conformance\",\"kind\":\"agent\",\"fingerprints\":[\"$FP\"],\"token\":\"$TOK\"}" \
     "$API/api/users" | python3 -c 'import json,sys; u=json.load(sys.stdin)["user"]; assert u["admin"]; print(u["id"])')
   curl -fsS -H "Authorization: Bearer $TOK" -d '{"max_leases":20,"max_ttl":0}' "$API/api/users/$USER_ID/quota"
   umask 077
   cat > /etc/spoond-staging/conformance.env <<EOF
   CONFORMANCE_API=$API
   CONFORMANCE_TOKEN=$TOK
   CONFORMANCE_USER=conformance
   CONFORMANCE_USER_ID=$USER_ID
   CONFORMANCE_SSH=local
   CONFORMANCE_SSH_KEY=/etc/spoond-staging/conformance_ed25519
   CONFORMANCE_SSH_GATEWAY=127.0.0.1:12222
   CONFORMANCE_PROXY_URL=http://127.0.0.1:18891
   CONFORMANCE_PROXY_SECRET=$PROXY_AUTH_SECRET
   CONFORMANCE_PROXY_SUFFIX=.sandbox.lacy.casa
   CONFORMANCE_BACKEND_UNIT=spoond-backend-staging
   EOF
   chmod 600 /etc/spoond-staging/conformance.env
   ```
   The `python3` assertion fails (and the step STOPs) if the user is not
   admin, i.e. if the staging identity store was not empty. Every staging
   conformance run loads this file with
   `set -a; . /etc/spoond-staging/conformance.env; set +a`.

Production `spoond-backend` is **not** touched.

## Commits

Each commit builds and passes `go build ./... && go vet ./... && go test ./...`.

1. `feat(store): per-image env column`
2. `feat(api): lease lifecycle on the Substrate interface` — one commit
   containing the Service rewrite, the server changes (including the fork
   route, the lease detail route and the DB-backed image list), the interim
   endpoint/proxy behaviour from item 17, `main.go`, and every ported and new
   test.

## Done when

- `go test ./...` passes.
- These conformance tests pass against the **staging** backend, run on
  vm2 from `/root/src/spoond` after
  `set -a; . /etc/spoond-staging/conformance.env; set +a` with
  `CONFORMANCE_SUBSTRATE=e2b` and `CONFORMANCE_GUEST_SERVICE=10.1.0.11:18891`
  (select them with `-run '^Test(L[1-6]|S[1-4]|D[12]|I[12])_'`):
  - L1–L6 (group N comes in U09);
  - S1–S4;
  - D1–D2;
  - I1–I2.
- Budgets: record S4's `create_p50_ms` and `create_p95_ms` with
  `POOL_SIZE=0` in `docs/plans/2026-09-30-e2b-substrate/RESULTS.md` (create
  the file, and append a dated section). If `create_p95_ms > 2000`, STOP for
  an OPERATOR decision. Do not change `POOL_SIZE` yourself (7 images × 2 pool
  sandboxes would exceed the 24 GiB of hugepages).

## Do not

- Do not touch the production backend, gateway or runner.
- Do not change the SSH gateway (U09).
- Do not delete `forkd/` (U12).
