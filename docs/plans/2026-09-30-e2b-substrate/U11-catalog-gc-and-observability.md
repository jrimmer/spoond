# U11 — Snapshot catalog GC, disk accounting and observability

## Purpose

- Reclaim unreferenced E2B builds safely.
- Account for disk.
- Expose metrics, health and doctor checks for the new substrate.
- Back up the SQLite database.

## Preconditions

U08 and U10 are done.

## Facts relied on

- **Build storage** (A3 C2):
  - each build is the directory
    `<TEMPLATE_STORAGE>/<build_id>/` (on vm2
    `/forkdcache/e2b/storage/templates/<build_id>/`);
  - it contains `metadata.json`, `rootfs.ext4`, its `.header`, and for memory
    snapshots `memfile`, its `.header` and `snapfile`.
- **Diff builds** reference their ancestors' blocks through the headers
  (A3 C3). So a build must never be deleted while any descendant is kept.
- **`TemplateBuildDelete{buildID, templateID}`** runs `rm -rf` of that
  directory **with no reference check** (A3 C4).
- **The spoond catalog** has `builds.parent_build_id` for every pause and
  checkpoint build, recorded in U08 and U10. Template builds have no parent
  in the catalog, but a build's headers can reference blocks of other builds,
  including cached layers the catalog never recorded. E2B reports every
  referenced build id in `SchedulingMetadata.rootfs_build_ids` and
  `memfile_build_ids` (A3 C1), which U06 returns as `substrate.BuildRefs`.
- **Orchestrator metrics** are OTLP only, sent to
  `OTEL_COLLECTOR_GRPC_ENDPOINT=127.0.0.1:14317` (A3 §7, 01-architecture).
  Port 4317 is taken on vm2.
- **Existing spoond metrics:** A1 §12, `metrics/metrics.go`.

## Schema migration `store/migrations/0004_build_owner.sql` (exact)

```sql
ALTER TABLE builds ADD COLUMN owner TEXT NOT NULL DEFAULT '';

CREATE TABLE build_refs (
  build_id     TEXT NOT NULL,
  ref_build_id TEXT NOT NULL,
  PRIMARY KEY (build_id, ref_build_id)
);
CREATE INDEX build_refs_ref ON build_refs(ref_build_id);
```

- Set `owner` to the lease's owner when inserting `pause` and `checkpoint`
  builds (U08 `suspend`, `clone`, `fork`; U10 drain and checkpoints).
- Template builds have `owner=''`.
- Add `Owner` to `store.BuildRow`, and to `InsertBuild`, `GetBuild`,
  `ListBuilds` and `ChildBuilds`.
- New store API in `store/catalog.go`:
  ```go
  // AddBuildRefs inserts (buildID, ref) for every ref in refs, ignoring duplicates
  // (INSERT OR IGNORE) and ignoring ref == buildID.
  func (db *DB) AddBuildRefs(ctx context.Context, buildID string, refs []string) error
  func (db *DB) ListBuildRefs(ctx context.Context) (map[string][]string, error) // build_id -> ref_build_ids
  ```
- **Populate it** with `AddBuildRefs(buildID, append(refs.RootfsBuildIDs, refs.MemfileBuildIDs...))`:
  - in `spoond images build` on success, from `BuildResult.Refs` (U07);
  - after every `sub.Pause` (U08 `suspend`, U10 drain) and every
    `sub.Checkpoint` (U08 `clone`, `fork`, U10 periodic and manual
    checkpoints), from the returned `BuildRefs`.
- Builds recorded before this migration have no refs. That affects only the
  staging catalog, where GC is dry-run; the production catalog is built in
  U12 with this code.

## GC (`api/gc.go`)

1. **Root set** R:
   - every `images.current_build_id`;
   - for every lease row (any state): `BuildID` (from its `sandboxes` row,
     if any), `ResumeBuildID` and `LastCheckpointBuildID`;
   - for every `sandboxes` row, including pool sandboxes: `build_id`;
   - every build with `state='building'`.
2. **Kept set** K = the closure of R: repeatedly add, for every build in
   K, its `parent_build_id` (when non-empty) and every `ref_build_id` from
   `build_refs` for that build, until nothing new is added. A ref id that is
   not in `builds` is still kept (it is never a candidate, because only
   `builds` rows are candidates).
3. **Candidates:** builds with `state IN ('ready','failed')`, not in K, with
   `updated_at` older than 1 hour.
4. For each candidate:
   - with `GC_DELETE=1` (backend env): call
     `sub.DeleteBuild(template_id, build_id)`, then set `state='deleted'`,
     and increment `spoond_gc_deleted_total{kind}`;
   - otherwise (the default `GC_DELETE` unset or `0`), only log
     `gc: would delete <build_id> kind=<k> image=<i>`.
5. **Schedule:** once an hour, and once 10 minutes after the backend
   starts. Never while `s.draining`.

## Snapshot API

- **`DELETE /api/snapshots/{build_id}`**, checks in exactly this order:
  1. `404` if the build is unknown or `state='deleted'`.
  2. `403` if `kind='template'` (template builds have `owner=''`, so this
     check must come before the owner check).
  3. `404` if `owner` ≠ caller.
  4. `409` `{"error":"snapshot in use"}` if the build is in K. The same
     computation as GC; ignore the 1-hour age rule here.
  5. Otherwise `sub.DeleteBuild`, set `state='deleted'`, and return `204`.
- **`GET /api/snapshots`:** the caller's builds (`owner` = caller,
  `state` ≠ `deleted`):
  `{"snapshots":[{"build_id","kind","image","parent_build_id","size_bytes","created_at","in_use":bool}]}`.
- **Lease list and detail rows** gain `"resume_build_id"` and
  `"build_id"`.

## Disk accounting

- Once an hour (with GC), for each non-deleted build, set `size_bytes` to
  the sum of regular file sizes (allocated blocks × 512, via `syscall.Stat_t.Blocks`)
  under `<E2B_TEMPLATE_STORAGE_PATH>/<build_id>/`.
- New backend env `E2B_TEMPLATE_STORAGE_PATH`, with vm2 value and default
  `/forkdcache/e2b/storage/templates`.
- Gauge `spoond_snapshot_bytes{kind}` = the sum per kind.
- Gauge `spoond_storage_free_bytes` = `statfs` free bytes of that path.

## Metrics (add to `metrics/metrics.go`; exact names)

| Name | Type | Labels | Meaning |
|---|---|---|---|
| `spoond_leases` | gauge | `state` | leases per state (running, suspended, recovered, lost) |
| `spoond_node_running_sandboxes` | gauge | none | from `NodeInfo` |
| `spoond_node_hugepages_free_bytes` | gauge | none | (total − used − reserved) × size |
| `spoond_node_outstanding_work` | gauge | none | from `NodeInfo` |
| `spoond_store_errors_total` | counter | `op` | from U05 |
| `spoond_checkpoint_duration_seconds` | histogram | none | from U10 |
| `spoond_create_duration_seconds` | histogram | `resume` (true/false) | around `sub.Create` |
| `spoond_snapshot_bytes` | gauge | `kind` | disk accounting |
| `spoond_storage_free_bytes` | gauge | none | disk accounting |
| `spoond_gc_deleted_total` | counter | `kind` | GC |
| `spoond_capacity_rejections_total` | counter | none | `admit` refusals |

- Update `NodeInfo`-derived gauges every 15 s.
- Keep `spoond_leases_active` (existing) working. U02 L6 checks it.

## `/metrics` composition

`handleMetrics` writes spoond's registry output, then, if `OTEL_PROM_URL` is
set:
- GET it with a 2 s timeout;
- append the body verbatim after a line `# --- orchestrator (otel) ---`;
- on error, append `# orchestrator metrics unavailable: <err>`.

## `/healthz`

- `200` `{"status":"ok","orchestrator":"<NodeInfo.Status>"}` when `NodeInfo`
  succeeds.
- `503` `{"status":"degraded","orchestrator":"unreachable"}` when it fails.

## OpenTelemetry Collector on vm2 (exact)

Write the config **before** installing the package: the package starts the
service on install, and its default config would bind the busy port 4317.

```bash
install -d /etc/otelcol-contrib
```

Write `/etc/otelcol-contrib/config.yaml` (exact):

```yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 127.0.0.1:14317
exporters:
  prometheus:
    endpoint: 127.0.0.1:19464
  nop: {}
service:
  pipelines:
    metrics:
      receivers: [otlp]
      exporters: [prometheus]
    traces:
      receivers: [otlp]
      exporters: [nop]
    logs:
      receivers: [otlp]
      exporters: [nop]
  telemetry:
    metrics:
      level: none
```

(`telemetry.metrics.level: none` stops the collector binding its own
metrics port 8888.) Then:

```bash
cd /tmp
curl -fsSLO https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/v0.162.0/otelcol-contrib_0.162.0_linux_amd64.deb
echo "0b2b37eeb83db8e19a5f9dfc60894d058c739384a226ac085e674561befaad44  otelcol-contrib_0.162.0_linux_amd64.deb" | sha256sum -c -
dpkg --force-confold -i otelcol-contrib_0.162.0_linux_amd64.deb
systemctl enable otelcol-contrib
systemctl restart otelcol-contrib
curl -fsS 127.0.0.1:19464/metrics | head
```
Add `OTEL_PROM_URL=http://127.0.0.1:19464/metrics` to the staging backend
env. Commit the config file to `deploy/e2b/otelcol-config.yaml`.

## Database backups

- `store.Backup(ctx, dir string, keep int) error`:
  - runs `VACUUM INTO '<dir>/<prefix>-<YYYYMMDD-HHMMSS>.db'` on the writer
    (`<prefix>` below);
  - then deletes all but the newest `keep` files matching `<prefix>-*.db`.
- The backend runs it daily at 03:00 local time, and once at start if no
  backup is newer than 24 h.
- Config: `SPOOND_BACKUP_DIR`, default `/var/lib/spoond/backups`; keep 7.
  Staging sets `SPOOND_BACKUP_DIR=/var/lib/spoond/backups-staging` (U08
  staging env; add the line now if it is missing, and restart staging).
- The file name prefix is the DB file's basename without extension, so
  production writes `spoond-<YYYYMMDD-HHMMSS>.db` and staging writes
  `staging-<YYYYMMDD-HHMMSS>.db`; pruning matches only `<prefix>-*.db`.

## `spoond doctor` (`cmd/spoond-doctor`)

Delete the forkd checks (`checkForkd`, `checkPool`, and the `FORKD_URL`
line of `checkConfig`). Keep every other existing check. Add the following,
using the existing status words `PASS`, `FAIL` plus a new `WARN`, each with
a reason. Doctor reads the backend environment: `SPOOND_DB_PATH`, the
`E2B_*` variables through `e2b.FromEnv()`, `E2B_TEMPLATE_STORAGE_PATH`, and
a new flag `--manifest` (default `/root/src/spoond/images/manifest.yaml`).
Run it as
`set -a; . /etc/spoond-staging/backend.env; set +a; /opt/spoond-staging/spoond doctor`
(staging) or with `/etc/spoond/backend.env` and `/opt/spoond/spoond`
(production after U12).
1. `GET 127.0.0.1:5008/health` returns `healthy`.
2. `NodeInfo`: status, running sandboxes, and free hugepages ≥ 4 GiB (WARN
   below).
3. The registry `GET http://127.0.0.1:5000/v2/` returns 200.
4. `/etc/spoond/e2b-token-seed` exists, is mode 0600, and is ≥ 32 bytes.
5. The DB opens, and the migration version is ≥ 4.
6. Every manifest image with `baked: true` has a `current_build_id` in
   `ready` state.
7. The artifact SHA-256 values match:
   - `/fc-versions/v1.14-0.2.0/amd64/firecracker`:
     `ef22aec7cbffcf6cc44a8436a4db79f9e6fe5c52218c81af321dd20f10ad6e5d`;
   - `/fc-kernels/vmlinux-6.1.177_5008931/amd64/vmlinux.bin`:
     `9191ced12d24e6e381753a7ab12ec850877524d453eae75c20aeb176f3b5ad05`;
   - `/fc-busybox/1.36.1/amd64/busybox`:
     `d7cce939adb09a41a22a5f846d22ba8d576b38dbb2b46a5c77a3a3e27ec52520`.
8. Free space at `E2B_TEMPLATE_STORAGE_PATH` is ≥ 20 GiB (WARN below).

Remove the `forkd` import from `cmd/spoond-doctor`.

## Tests

- **GC unit tests:**
  - a chain template → checkpoint c1 → checkpoint c2, with a lease on c2:
    nothing is deletable;
  - with the lease deleted and c2 no longer a root: c2 and c1 are
    candidates, and the template is not (current);
  - a `building` build is never deleted;
  - `GC_DELETE` unset only logs (the fake records no `DeleteBuild` call).
- **Snapshot API tests:** 403 for a template, 409 in use, 404 for another
  owner, and 204 then 404.
- **`/healthz`** with the fake `NodeInfo` failing: 503.
- **Conformance** D3 and L6 pass against staging, after a staging redeploy
  (Ops runner, once this unit's commits are merged):
  ```bash
  export PATH=/usr/local/go/bin:$PATH
  cd /root/src/spoond && git fetch && git checkout feat/e2b-substrate && git pull --ff-only
  go build -o /opt/spoond-staging/spoond ./cmd/spoond
  systemctl restart spoond-backend-staging spoond-sshd-gateway-staging
  ```
- **GC refs test:** a build referenced only through `build_refs` by a kept
  build is not a candidate.

## Commits

1. `feat(store): build owner column and database backups`
2. `feat(api): snapshot catalog GC (dry-run by default) and snapshot API`
3. `feat(metrics): substrate metrics, OTel passthrough and health`
4. `feat(doctor): substrate checks replace forkd checks`
5. `feat(deploy): OpenTelemetry collector config for vm2`

## Done when

- The tests pass.
- Staging `/metrics` shows the new metrics and the orchestrator section.
- `spoond doctor` against staging is all `ok`.
- GC in dry-run logs sensible candidates.

## Do not

Do not enable `GC_DELETE=1` in this unit. U12 enables it after reviewing a
week of dry-run logs.
