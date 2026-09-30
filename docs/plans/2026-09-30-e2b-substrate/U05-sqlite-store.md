# U05 — SQLite store (leases, shares, pool, catalog)

## Purpose

Persist spoond's lease state in an embedded SQLite database, so a backend
restart no longer loses leases. This lands **while still on forkd**. It is
independent of the substrate and immediately fixes the backend-restart data
loss (conformance test R3).

## Preconditions

U01 is done (Go 1.27.1; `modernc.org/sqlite` v1.60.1 needs Go ≥ 1.26).

## Facts relied on (A1 §3.2, §3.3, §3.6, §3.16, §8)

- `api.Store` holds, in memory only:
  - `leases map[string]*Lease`
  - `pool map[string][]string`
  - `shares map[string]map[string]*Share`
  - `pending map[string]int`

  Nothing is persisted.
- The `Lease` fields are listed in A1 §3.2. `Share` has `LeaseID`,
  `Grantee`, `Mode`, `ExpiresAt` and `CreatedAt`.
- `Service.Shutdown` releases **all** leases (deleting workspaces) and kills
  pooled sandboxes.
- `ReconcileOrphans` kills every controller sandbox not in `leases` or
  `pool`.
- **Functions that mutate leases, shares or the pool** (the Mutates column
  lists what changes):

  | Function | Mutates |
  |---|---|
  | `applyNetpol` | `NetPolicy`, `ExposedIP` |
  | `NewServiceWithIdle` | `pool` seeding |
  | `sweepExpired` | `Suspended`, release |
  | `touch` | `LastActive` |
  | `keepAlive` | `ExpiresAt`, `LastActive` |
  | `release` | delete |
  | `grant` | insert; `Workspace`, `ForkdID`, `Address`; pool pops |
  | `fillEndpoint` | `Address` |
  | `suspend` | `Suspended` |
  | `resume` | `ForkdID`, `LastActive`, `Suspended` |
  | `restart` | `ForkdID`, `Suspended`, `LastActive` |
  | `setName` | `Name` |
  | `setComment` | `Comment` |
  | `GrantShare` | shares insert |
  | `RevokeShare` | shares delete |
  | `grantFromSnapshot` | insert; `Workspace`, `ForkdID`, `Address` |
  | `warmPool` | pool append |
- `cmd/spoond-backend/main.go` constructs the Service, calls
  `ReconcileOrphans`, starts loops, and calls `Shutdown` on SIGTERM (A1 §8).

## Deliverables

1. `go.mod`: add `modernc.org/sqlite v1.60.1`.
2. New package `store/` (`github.com/jrimmer/spoond/store`):
   - `store/db.go`
   - `store/migrations/0001_init.sql`
   - `store/leases.go`
   - `store/shares.go`
   - `store/pool.go`
   - `store/store_test.go`
3. Changes in `api/service.go` and `cmd/spoond-backend/main.go` (below).
4. A tests addition in `api/`: `api/persist_test.go`.

## `store/migrations/0001_init.sql` (exact)

All timestamps are TEXT in RFC 3339 with nanoseconds, UTC
(`time.RFC3339Nano`). The empty string means unset.

```sql
CREATE TABLE schema_migrations (
  version    INTEGER PRIMARY KEY,
  applied_at TEXT NOT NULL
);

CREATE TABLE leases (
  id                        TEXT PRIMARY KEY,
  owner                     TEXT NOT NULL,
  image                     TEXT NOT NULL,
  sandbox_id                TEXT NOT NULL DEFAULT '',
  address                   TEXT NOT NULL DEFAULT '',
  created_at                TEXT NOT NULL,
  expires_at                TEXT NOT NULL,
  persistent                INTEGER NOT NULL DEFAULT 0,
  last_active               TEXT NOT NULL,
  workspace                 TEXT NOT NULL DEFAULT '',
  suspended                 INTEGER NOT NULL DEFAULT 0,
  name                      TEXT NOT NULL DEFAULT '',
  net_policy                TEXT NOT NULL DEFAULT '',
  net_allow                 TEXT NOT NULL DEFAULT '[]',
  expose_ports              TEXT NOT NULL DEFAULT '[]',
  exposed_ip                TEXT NOT NULL DEFAULT '',
  comment                   TEXT NOT NULL DEFAULT '',
  state                     TEXT NOT NULL DEFAULT 'running'
                              CHECK (state IN ('running','suspended','recovered','lost')),
  resume_build_id           TEXT NOT NULL DEFAULT '',
  last_checkpoint_build_id  TEXT NOT NULL DEFAULT '',
  last_checkpoint_at        TEXT NOT NULL DEFAULT '',
  recovered_from            TEXT NOT NULL DEFAULT ''
);
CREATE INDEX leases_owner ON leases(owner);
CREATE UNIQUE INDEX leases_owner_name ON leases(owner, name) WHERE name <> '';

CREATE TABLE shares (
  lease_id   TEXT NOT NULL REFERENCES leases(id) ON DELETE CASCADE,
  grantee    TEXT NOT NULL,
  mode       TEXT NOT NULL CHECK (mode IN ('ssh','http')),
  expires_at TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  PRIMARY KEY (lease_id, grantee)
);

CREATE TABLE pool (
  sandbox_id TEXT PRIMARY KEY,
  image      TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX pool_image ON pool(image);

CREATE TABLE images (
  name             TEXT PRIMARY KEY,
  template_id      TEXT NOT NULL,
  current_build_id TEXT NOT NULL DEFAULT '',
  digest           TEXT NOT NULL DEFAULT '',
  vcpu             INTEGER NOT NULL,
  memory_mb        INTEGER NOT NULL,
  disk_mb          INTEGER NOT NULL,
  start_cmd        TEXT NOT NULL DEFAULT '',
  ready_cmd        TEXT NOT NULL DEFAULT '',
  updated_at       TEXT NOT NULL
);

CREATE TABLE builds (
  build_id            TEXT PRIMARY KEY,
  kind                TEXT NOT NULL CHECK (kind IN ('template','pause','checkpoint')),
  template_id         TEXT NOT NULL,
  image               TEXT NOT NULL,
  parent_build_id     TEXT NOT NULL DEFAULT '',
  source_sandbox_id   TEXT NOT NULL DEFAULT '',
  state               TEXT NOT NULL CHECK (state IN ('building','ready','failed','deleted')),
  kernel_version      TEXT NOT NULL DEFAULT '',
  firecracker_version TEXT NOT NULL DEFAULT '',
  envd_version        TEXT NOT NULL DEFAULT '',
  vcpu                INTEGER NOT NULL DEFAULT 0,
  memory_mb           INTEGER NOT NULL DEFAULT 0,
  disk_mb             INTEGER NOT NULL DEFAULT 0,
  size_bytes          INTEGER NOT NULL DEFAULT 0,
  error               TEXT NOT NULL DEFAULT '',
  created_at          TEXT NOT NULL,
  updated_at          TEXT NOT NULL
);
CREATE INDEX builds_parent ON builds(parent_build_id);
CREATE INDEX builds_image ON builds(image);

CREATE TABLE sandboxes (
  sandbox_id   TEXT PRIMARY KEY,
  lease_id     TEXT NOT NULL DEFAULT '',
  build_id     TEXT NOT NULL,
  execution_id TEXT NOT NULL,
  host_ip      TEXT NOT NULL DEFAULT '',
  vcpu         INTEGER NOT NULL,
  memory_mb    INTEGER NOT NULL,
  started_at   TEXT NOT NULL,
  end_at       TEXT NOT NULL
);
CREATE INDEX sandboxes_lease ON sandboxes(lease_id);
```

The `images`, `builds` and `sandboxes` tables are created now and first
used in U06–U08. `leases.sandbox_id` holds forkd's sandbox id until U12
(it replaces the Go field `ForkdID`).

## `store/db.go` (exact behaviour)

```go
package store

// DB is spoond's embedded SQLite database. One writer connection, a small
// reader pool.
type DB struct {
	w *sql.DB // SetMaxOpenConns(1): all writes serialize here
	r *sql.DB // SetMaxOpenConns(8): reads
}

// Open creates the parent directory (0700) if missing, opens the file with
// the DSN below on two handles, and applies pending migrations in order.
func Open(path string) (*DB, error)
func (db *DB) Close() error
// ErrNotFound is returned by single-row getters.
var ErrNotFound = errors.New("store: not found")
```

- **Driver:** `import _ "modernc.org/sqlite"` with driver name `"sqlite"`.
- **DSN:** `file:` + path + `?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)`.
- **Migrations:**
  - Embedded with `//go:embed migrations/*.sql`. The file name prefix is
    the version (`0001` → 1).
  - Each file runs in one transaction on the writer, and inserts its
    `schema_migrations` row.
  - Before creating `schema_migrations`, check whether it exists
    (`SELECT name FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).
    Apply only versions greater than `MAX(version)`.
  - **Exception for 0001:** it contains the `CREATE TABLE schema_migrations`
    statement itself, so apply it only when the table does not exist.
- **Queries:** all writes use `db.w`, and all reads use `db.r`.

## `store/leases.go`, `shares.go`, `pool.go` (exact API)

```go
type LeaseRow struct {
	ID, Owner, Image, SandboxID, Address string
	CreatedAt, ExpiresAt, LastActive    time.Time
	Persistent, Suspended                bool
	Workspace, Name, NetPolicy           string
	NetAllow                             []string // JSON in net_allow
	ExposePorts                          []int    // JSON in expose_ports
	ExposedIP, Comment, State            string   // State: running|suspended|recovered|lost
	ResumeBuildID, LastCheckpointBuildID string
	LastCheckpointAt, RecoveredFrom      time.Time // zero = unset ('')
}
func (db *DB) UpsertLease(ctx context.Context, l LeaseRow) error      // INSERT ... ON CONFLICT(id) DO UPDATE SET every column
func (db *DB) DeleteLease(ctx context.Context, id string) error       // shares cascade
func (db *DB) ListLeases(ctx context.Context) ([]LeaseRow, error)
func (db *DB) UpdateLastActive(ctx context.Context, ids map[string]time.Time) error // one transaction

type ShareRow struct {
	LeaseID, Grantee, Mode string
	ExpiresAt, CreatedAt   time.Time // ExpiresAt zero = never
}
func (db *DB) UpsertShare(ctx context.Context, s ShareRow) error
func (db *DB) DeleteShare(ctx context.Context, leaseID, grantee string) error
func (db *DB) ListShares(ctx context.Context) ([]ShareRow, error)

func (db *DB) AddPool(ctx context.Context, sandboxID, image string) error
func (db *DB) RemovePool(ctx context.Context, sandboxID string) error
func (db *DB) ListPool(ctx context.Context) (map[string][]string, error) // image -> ids, ordered by created_at
```

**Time encoding:** write `t.UTC().Format(time.RFC3339Nano)`, and `""` for the
zero time. Parse `""` back as `time.Time{}`.

## Service integration (`api/service.go`)

1. **Fields.** Add `db *store.DB` to `Service`, a setter
   `func (s *Service) SetDB(db *store.DB)`, a dirty set
   `lastActiveDirty map[string]time.Time` (under `s.store.mu`), and
   `stopLoops context.CancelFunc`. `Start(ctx)` begins with
   `ctx, s.stopLoops = context.WithCancel(ctx)` and passes that `ctx` to its
   goroutine.
2. **Mapping.** Add `func leaseToRow(l *Lease) store.LeaseRow` and
   `func rowToLease(r store.LeaseRow) *Lease`.
   - Map `ForkdID` ↔ `SandboxID`.
   - `leaseToRow` writes `State: l.State` when `l.State != ""`; when
     `l.State == ""` it writes `suspended` if `l.Suspended`, otherwise
     `running`. `rowToLease` copies `State` and sets
     `Suspended = (State == "suspended")`. `suspend` sets
     `l.State = "suspended"`; `resume` and `restart` set
     `l.State = "running"`; `grant` and `grantFromSnapshot` set
     `State: "running"` on the new lease.
   - Keep new fields `State`, `ResumeBuildID`, `LastCheckpointBuildID`,
     `LastCheckpointAt` and `RecoveredFrom` on `Lease`. Add them to the
     `Lease` struct now with those names and types; later units set them.
3. **Persistence helpers**, all called **with `s.store.mu` held**, and all
   no-ops when `s.db == nil` so the existing tests keep working:
   - `saveLeaseLocked(l *Lease)`: `UpsertLease` with a 5 s timeout context.
   - `deleteLeaseLocked(id string)`
   - `saveShareLocked(sh *Share)`
   - `deleteShareLocked(leaseID, grantee string)`
   - `addPoolLocked(id, image string)`
   - `removePoolLocked(id string)`

   On error: log `store: <op> <id>: <err>` and increment the new counter
   `spoond_store_errors_total`, a `CounterVec` with the single label `op`,
   added to `BackendMetrics` in `metrics/metrics.go`. The `op` values are
   exactly `upsert_lease`, `delete_lease`, `upsert_share`, `delete_share`,
   `add_pool`, `remove_pool` and `update_last_active`. **Do not fail the
   request.** Availability wins over durability for a single write.
4. **Call sites.** In each function in the Facts table, after the mutation
   and before unlocking, call the matching helper:
   - lease field change or insert → `saveLeaseLocked`;
   - lease deletion → `deleteLeaseLocked`;
   - share add or replace → `saveShareLocked`;
   - share removal → `deleteShareLocked`;
   - pool append → `addPoolLocked`;
   - pool pop or removal → `removePoolLocked`.

   **`release`:** call `deleteLeaseLocked(l.ID)` at both places that run
   `delete(s.store.leases, l.ID)` (the "already gone" path and the success
   path), never on the path that re-opens the lease after a failed kill.

   **Exception, `touch`:** only record `lastActiveDirty[id]=now`.
   `sweepExpired` flushes the dirty set with `UpdateLastActive` at the start
   of each tick, then clears it.
5. **Load.** Add `func (s *Service) LoadState(ctx context.Context) error`.
   It reads all leases, shares and the pool from the DB into `s.store`, and
   seeds known images into the pool map as today. It must run before
   `Start` and before `ReconcileOrphans`.
6. **Shutdown.** Change `Shutdown(ctx)` to **only** stop the background
   loops (call `s.stopLoops()` when it is non-nil) and flush
   `lastActiveDirty`. It must
   **not** release leases, delete workspaces or kill pooled sandboxes.
   Update its doc comment accordingly.
7. **ReconcileOrphans.** After LoadState:
   - Kill only controller sandboxes whose id is in neither `leases` (by
     `SandboxID`/`ForkdID`) nor `pool`. That is the current logic, applied
     to loaded state.
   - Additionally, for every non-suspended lease whose sandbox id is **not**
     in the controller's list, set `State="lost"` and `saveLeaseLocked`.
     Do not delete it.
   - Pool entries whose sandbox is missing are removed (`removePoolLocked`).

## `cmd/spoond-backend/main.go`

- Read `SPOOND_DB_PATH` (default `/var/lib/spoond/spoond.db`).
- `store.Open` it. On error, **exit** with a message.
- Exact startup order: `store.Open` → `svc.SetDB(db)` →
  `svc.LoadState(ctx)` (exit on error) → `svc.ReconcileOrphans(ctx)` →
  `svc.Start(ctx)`. This moves `Start` after `ReconcileOrphans` (today it is
  before).
- On SIGTERM: `svc.Shutdown(ctx)`, then `db.Close()`.

## Tests

- `store/store_test.go`, using a temp dir:
  - `Open` twice on the same path is idempotent (no migration error).
  - Upsert, list and roundtrip every `LeaseRow` field, including slices,
    booleans and zero times.
  - `DeleteLease` cascades shares.
  - The unique `(owner,name)` index rejects a duplicate non-empty name.
  - The pool is ordered by `created_at`.
- Replace `TestShutdownKillsLeasesAndPool` (`api/server_test.go`) with
  `TestShutdownKeepsLeasesAndPool`: same setup, then assert
  `len(ff.killed) == 0` after `Shutdown`, and that `svc.LiveLeases()` still
  contains the lease.
- `api/persist_test.go`, using the existing fake `ForkdClient` from
  `api/server_test.go`:
  - Build a Service with `SetDB(tempDB)`, grant a lease, set a name, add a
    share.
  - Build a **new** Service on the same DB and call `LoadState`.
  - The lease, its name and the share exist.
  - `Shutdown` on the first Service did not call `Kill` or `DeleteWorkspace`
    on the fake: assert `len(ff.killed) == 0` (`fakeForkd.killed` records
    both).

## Merge and production deploy (orchestrator merge; Autonomous window deploy)

1. **Merge (orchestrator, README rule 10).** Once both commits pass the
   worker's tests and the verifier returns PASS, the orchestrator merges
   `feat/e2b-substrate` (at this point U01, U02 and U05) into `main`, with
   the status files removed first (`02-orchestration.md`).
2. **Prepare (Ops runner; no production impact):**
   ```bash
   export PATH=/usr/local/go/bin:$PATH
   test -d /root/src/spoond || git clone https://code.lacy.casa/lacy.casa/spoond.git /root/src/spoond
   cd /root/src/spoond && git fetch && git checkout main && git pull --ff-only
   install -d -m 700 /var/lib/spoond
   go build -o /opt/spoond/spoond.new ./cmd/spoond
   cp /opt/spoond/spoond /opt/spoond/spoond.pre-u05
   ```
3. **Deploy (Autonomous window, `00-README.md`).** This restart is the only
   time leases are lost, because the old binary's `Shutdown` still releases
   them; the human has accepted that.
   - **Rollback artifacts:** `/opt/spoond/spoond.pre-u05` (verify:
     `test -s` and `/opt/spoond/spoond.pre-u05 help` exits 0 or 2). The unit
     files and env files are not changed. There is no previous DB.
   - **Act:**
     ```bash
     mv /opt/spoond/spoond.new /opt/spoond/spoond
     systemctl restart spoond-backend spoond-sshd-gateway
     sleep 5
     ```
   - **Verify:** `curl -fsS https://vm2.lacy.casa:8890/healthz` prints
     `{"status":"ok"}`; `window_smoke`; `test -s /var/lib/spoond/spoond.db`;
     then, in the same window, the R3-only conformance run from "Done when"
     passes.
   - **Rollback commands:**
     ```bash
     cp /opt/spoond/spoond.pre-u05 /opt/spoond/spoond
     systemctl restart spoond-backend spoond-sshd-gateway
     ```
     then `window_smoke`, then `BLOCKED`.
4. From U06 on, work continues on `feat/e2b-substrate`, rebased on `main`
   (`git rebase origin/main`).

## Commits

1. `feat(store): embedded SQLite store with lease, share, pool and catalog schema`
   (the `store/` package and tests).
2. `feat(api): persist leases, shares and pool; keep leases across backend restarts`
   (the Service integration, main.go, and api tests).

## Done when

- `go test ./...` passes.
- In the deploy's Autonomous window (after the restart, with
  `spoond-runner` still stopped), only R3 passes against forkd, run from
  `/root/src/spoond` on `main`:
  `set -a; . /etc/spoond/conformance.env; set +a; CONFORMANCE_DESTRUCTIVE=1 CONFORMANCE_SUBSTRATE=forkd CONFORMANCE_GUEST_SERVICE=10.43.0.1:8891 go test -tags conformance -count=1 -run '^TestR3_BackendRestart$' -v ./conformance/ -args -results "$PWD/conformance/results/$(date +%Y%m%dT%H%M%S)-forkd-r3.json"`.
  Never run all of group R against forkd.

## Do not

- Do not change the lease API.
- Do not move the identity store (`users.json`) into SQLite.
- Do not make DB errors fail requests.
