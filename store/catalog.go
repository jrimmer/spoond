package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ImageRow is one row of the images table: the stable identity of a
// spoond image and the template build it currently points at.
type ImageRow struct {
	Name, TemplateID, CurrentBuildID, Digest string
	VCPU, MemoryMB, DiskMB                   int
	StartCmd, ReadyCmd                       string
	Env                                      map[string]string // JSON in env
	UpdatedAt                                time.Time
}

// GetImage returns the image row for name, or ErrNotFound.
func (db *DB) GetImage(ctx context.Context, name string) (ImageRow, error) {
	row := db.r.QueryRowContext(ctx, `SELECT
		name, template_id, current_build_id, digest,
		vcpu, memory_mb, disk_mb, start_cmd, ready_cmd, env, updated_at
		FROM images WHERE name = ?`, name)
	return scanImage(row.Scan)
}

// ListImages returns every image row, ordered by name.
func (db *DB) ListImages(ctx context.Context) ([]ImageRow, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT
		name, template_id, current_build_id, digest,
		vcpu, memory_mb, disk_mb, start_cmd, ready_cmd, env, updated_at
		FROM images ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list images: %w", err)
	}
	defer rows.Close()
	var out []ImageRow
	for rows.Next() {
		r, err := scanImage(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: list images: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list images: %w", err)
	}
	return out, nil
}

// UpsertImage inserts the image row, or updates every column of the
// existing row on name conflict. The caller owns the row's identity:
// the template id of an existing image is passed back unchanged.
func (db *DB) UpsertImage(ctx context.Context, r ImageRow) error {
	env, err := json.Marshal(r.Env)
	if err != nil {
		return fmt.Errorf("store: image %s: env: %w", r.Name, err)
	}
	_, err = db.w.ExecContext(ctx, `INSERT INTO images
		(name, template_id, current_build_id, digest,
		 vcpu, memory_mb, disk_mb, start_cmd, ready_cmd, env, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
		  template_id = excluded.template_id,
		  current_build_id = excluded.current_build_id,
		  digest = excluded.digest,
		  vcpu = excluded.vcpu,
		  memory_mb = excluded.memory_mb,
		  disk_mb = excluded.disk_mb,
		  start_cmd = excluded.start_cmd,
		  ready_cmd = excluded.ready_cmd,
		  env = excluded.env,
		  updated_at = excluded.updated_at`,
		r.Name, r.TemplateID, r.CurrentBuildID, r.Digest,
		r.VCPU, r.MemoryMB, r.DiskMB, r.StartCmd, r.ReadyCmd, string(env), formatTime(r.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: upsert image %s: %w", r.Name, err)
	}
	return nil
}

func scanImage(scan func(dest ...any) error) (ImageRow, error) {
	var r ImageRow
	var updatedAt, env string
	err := scan(&r.Name, &r.TemplateID, &r.CurrentBuildID, &r.Digest,
		&r.VCPU, &r.MemoryMB, &r.DiskMB, &r.StartCmd, &r.ReadyCmd, &env, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ImageRow{}, ErrNotFound
	}
	if err != nil {
		return ImageRow{}, err
	}
	r.UpdatedAt = parseTime(updatedAt)
	if err := json.Unmarshal([]byte(env), &r.Env); err != nil {
		return ImageRow{}, fmt.Errorf("store: image %s: env: %w", r.Name, err)
	}
	return r, nil
}

// BuildRow is one row of the builds table: one immutable E2B artifact
// set (kind template, pause or checkpoint) and its lifecycle state.
type BuildRow struct {
	BuildID, Kind, TemplateID, Image, ParentBuildID, SourceSandboxID, Owner, State string
	KernelVersion, FirecrackerVersion, EnvdVersion                                 string
	VCPU, MemoryMB, DiskMB                                                         int
	SizeBytes                                                                      int64
	Error                                                                          string
	CreatedAt, UpdatedAt                                                           time.Time
}

// GetBuild returns the build row for id, or ErrNotFound.
func (db *DB) GetBuild(ctx context.Context, id string) (BuildRow, error) {
	row := db.r.QueryRowContext(ctx, `SELECT
		build_id, kind, template_id, image, parent_build_id, source_sandbox_id, owner, state,
		kernel_version, firecracker_version, envd_version,
		vcpu, memory_mb, disk_mb, size_bytes, error, created_at, updated_at
		FROM builds WHERE build_id = ?`, id)
	return scanBuild(row.Scan)
}

// InsertBuild records a new build in state building (or whatever state
// the caller set). The build id must not exist yet.
func (db *DB) InsertBuild(ctx context.Context, r BuildRow) error {
	_, err := db.w.ExecContext(ctx, `INSERT INTO builds
		(build_id, kind, template_id, image, parent_build_id, source_sandbox_id, owner, state,
		 kernel_version, firecracker_version, envd_version,
		 vcpu, memory_mb, disk_mb, size_bytes, error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.BuildID, r.Kind, r.TemplateID, r.Image, r.ParentBuildID, r.SourceSandboxID, r.Owner, r.State,
		r.KernelVersion, r.FirecrackerVersion, r.EnvdVersion,
		r.VCPU, r.MemoryMB, r.DiskMB, r.SizeBytes, r.Error,
		formatTime(r.CreatedAt), formatTime(r.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: insert build %s: %w", r.BuildID, err)
	}
	return nil
}

// UpdateBuildState moves a build to state with an error message ("" when
// none). r non-nil: its version and size fields are copied too, so a
// finished build records what it actually produced.
func (db *DB) UpdateBuildState(ctx context.Context, id, state, errMsg string, r *BuildRow) error {
	var q string
	var args []any
	if r != nil {
		q = `UPDATE builds SET state = ?, error = ?, updated_at = ?,
			kernel_version = ?, firecracker_version = ?, envd_version = ?,
			size_bytes = ?, disk_mb = ?
			WHERE build_id = ?`
		args = []any{state, errMsg, formatTime(time.Now()),
			r.KernelVersion, r.FirecrackerVersion, r.EnvdVersion,
			r.SizeBytes, r.DiskMB, id}
	} else {
		q = `UPDATE builds SET state = ?, error = ?, updated_at = ? WHERE build_id = ?`
		args = []any{state, errMsg, formatTime(time.Now()), id}
	}
	_, err := db.w.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("store: update build %s: %w", id, err)
	}
	return nil
}

// ListBuilds returns every build row, ordered by created_at.
func (db *DB) ListBuilds(ctx context.Context) ([]BuildRow, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT
		build_id, kind, template_id, image, parent_build_id, source_sandbox_id, owner, state,
		kernel_version, firecracker_version, envd_version,
		vcpu, memory_mb, disk_mb, size_bytes, error, created_at, updated_at
		FROM builds ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("store: list builds: %w", err)
	}
	defer rows.Close()
	var out []BuildRow
	for rows.Next() {
		r, err := scanBuild(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: list builds: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list builds: %w", err)
	}
	return out, nil
}

// MaxBuildChainDepth bounds the ancestor walk in BuildChain. A correct
// chain is one build per pause (plus the template root), so no real
// lease comes close; the cap only stops a corrupt catalog cycle (or a
// catalog a bug made a chain into) from walking for ever.
const MaxBuildChainDepth = 10000

// BuildChain walks head's non-deleted parent chain and returns how many
// builds it holds and their summed recorded size_bytes. The walk follows
// parent_build_id until a missing build, a deleted build, an empty
// parent, a cycle or the depth cap, exactly the closure the GC keeps a
// live resume build under: a deleted build's files are gone, so it and
// its ancestors are no longer part of the live chain. It reads one row
// per ancestor through the recursive CTE (indexed by the primary key and
// builds_parent) instead of scanning the whole builds table. A missing
// or empty head is an empty chain, not an error.
func (db *DB) BuildChain(ctx context.Context, head string) (depth int, bytes int64, err error) {
	if head == "" {
		return 0, 0, nil
	}
	rows, err := db.r.QueryContext(ctx, `WITH RECURSIVE chain(build_id, parent_build_id, size_bytes, state, depth, path) AS (
		SELECT build_id, parent_build_id, size_bytes, state, 1, ',' || build_id || ','
			FROM builds WHERE build_id = ?
		UNION ALL
		SELECT b.build_id, b.parent_build_id, b.size_bytes, b.state, chain.depth + 1,
				chain.path || b.build_id || ','
			FROM builds b JOIN chain ON b.build_id = chain.parent_build_id
			WHERE chain.state <> 'deleted' AND chain.depth < ?
				AND instr(chain.path, ',' || b.build_id || ',') = 0
	)
	SELECT COUNT(*), COALESCE(SUM(size_bytes), 0) FROM chain WHERE state <> 'deleted'`,
		head, MaxBuildChainDepth)
	if err != nil {
		return 0, 0, fmt.Errorf("store: build chain %s: %w", head, err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, 0, fmt.Errorf("store: build chain %s: %w", head, err)
		}
		return 0, 0, nil
	}
	var n int64
	if err := rows.Scan(&n, &bytes); err != nil {
		return 0, 0, fmt.Errorf("store: build chain %s: %w", head, err)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("store: build chain %s: %w", head, err)
	}
	return int(n), bytes, nil
}

// CountBuildingTemplateBuilds returns how many template builds are in
// state building. The catalog is shared with the separate `spoond images
// build` process, so this is how the backend sees that process's bakes:
// it inserts the row `building` before it asks the orchestrator for the
// build and moves it on when the build ends. A row a killed build left
// `building` is failed by the GC after twice the build timeout
// (spoond-4yl), so it cannot hold the count up for ever. The count backs
// the periodic orphan sweep's skip guard (a build sandbox has no spoond
// row and would look unclaimed, spoond-63a G3) and the
// spoond_builds_in_flight gauge.
func (db *DB) CountBuildingTemplateBuilds(ctx context.Context) (int, error) {
	row := db.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM builds
		WHERE state = 'building' AND kind = 'template'`)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count building template builds: %w", err)
	}
	return n, nil
}

// MarkStaleBuildingFailed marks every build still `building` whose
// updated_at is older than cutoff as `failed` (spoond-4yl). A build is
// written in state building before the orchestrator is asked to build
// it; a SIGKILL or reboot between those two writes leaves the row
// building forever, and every building row is a GC root (with its whole
// ancestor chain), so it pins the catalog indefinitely. The GC fails
// such rows; a template build has no owner and never appears in
// /api/snapshots, so the caller emits a lease-less `gc` event for each
// one. It returns the rows it changed so the caller can log and announce
// each one.
func (db *DB) MarkStaleBuildingFailed(ctx context.Context, cutoff time.Time, reason string) ([]BuildRow, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT
		build_id, kind, template_id, image, parent_build_id, source_sandbox_id, owner, state,
		kernel_version, firecracker_version, envd_version,
		vcpu, memory_mb, disk_mb, size_bytes, error, created_at, updated_at
		FROM builds WHERE state = 'building'`)
	if err != nil {
		return nil, fmt.Errorf("store: list building builds: %w", err)
	}
	var stale []BuildRow
	for rows.Next() {
		r, err := scanBuild(rows.Scan)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: list building builds: %w", err)
		}
		// Compare in Go: updated_at is RFC 3339 with trailing-zero
		// trimming, so a plain string comparison misorders the
		// sub-second forms ("...00Z" sorts after "...00.5Z").
		if r.UpdatedAt.Before(cutoff) {
			stale = append(stale, r)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: list building builds: %w", err)
	}
	rows.Close()
	var marked []BuildRow
	for _, b := range stale {
		res, err := db.w.ExecContext(ctx,
			`UPDATE builds SET state = 'failed', error = ?, updated_at = ? WHERE build_id = ? AND state = 'building'`,
			reason, formatTime(time.Now()), b.BuildID)
		if err != nil {
			return marked, fmt.Errorf("store: mark stale building build %s failed: %w", b.BuildID, err)
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			continue // a racing writer moved it on already
		}
		b.State = "failed"
		b.Error = reason
		marked = append(marked, b)
	}
	return marked, nil
}

// ChildBuilds returns the builds with parent_build_id = parentID
// (pause/checkpoint builds of one sandbox lineage), ordered by
// created_at.
func (db *DB) ChildBuilds(ctx context.Context, parentID string) ([]BuildRow, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT
		build_id, kind, template_id, image, parent_build_id, source_sandbox_id, owner, state,
		kernel_version, firecracker_version, envd_version,
		vcpu, memory_mb, disk_mb, size_bytes, error, created_at, updated_at
		FROM builds WHERE parent_build_id = ? ORDER BY created_at`, parentID)
	if err != nil {
		return nil, fmt.Errorf("store: list child builds %s: %w", parentID, err)
	}
	defer rows.Close()
	var out []BuildRow
	for rows.Next() {
		r, err := scanBuild(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: list child builds %s: %w", parentID, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list child builds %s: %w", parentID, err)
	}
	return out, nil
}

func scanBuild(scan func(dest ...any) error) (BuildRow, error) {
	var r BuildRow
	var createdAt, updatedAt string
	err := scan(&r.BuildID, &r.Kind, &r.TemplateID, &r.Image, &r.ParentBuildID,
		&r.SourceSandboxID, &r.Owner, &r.State,
		&r.KernelVersion, &r.FirecrackerVersion, &r.EnvdVersion,
		&r.VCPU, &r.MemoryMB, &r.DiskMB, &r.SizeBytes, &r.Error,
		&createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return BuildRow{}, ErrNotFound
	}
	if err != nil {
		return BuildRow{}, err
	}
	r.CreatedAt = parseTime(createdAt)
	r.UpdatedAt = parseTime(updatedAt)
	return r, nil
}

// AddBuildRefs inserts (buildID, ref) for every ref in refs, ignoring duplicates
// (INSERT OR IGNORE) and ignoring ref == buildID.
func (db *DB) AddBuildRefs(ctx context.Context, buildID string, refs []string) error {
	for _, ref := range refs {
		if ref == buildID {
			continue
		}
		if _, err := db.w.ExecContext(ctx,
			`INSERT OR IGNORE INTO build_refs (build_id, ref_build_id) VALUES (?, ?)`,
			buildID, ref); err != nil {
			return fmt.Errorf("store: add build refs %s: %w", buildID, err)
		}
	}
	return nil
}

// ListBuildRefs returns every build_refs row as build_id -> ref_build_ids.
func (db *DB) ListBuildRefs(ctx context.Context) (map[string][]string, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT build_id, ref_build_id FROM build_refs`)
	if err != nil {
		return nil, fmt.Errorf("store: list build refs: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var buildID, ref string
		if err := rows.Scan(&buildID, &ref); err != nil {
			return nil, fmt.Errorf("store: list build refs: %w", err)
		}
		out[buildID] = append(out[buildID], ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list build refs: %w", err)
	}
	return out, nil
}

// DeleteBuildsPermanently removes already-deleted build rows whose
// updated_at is older than cutoff, together with each row's build_refs
// rows. A build in state deleted has had its files removed and protects
// nothing; its catalog row and refs are pure leak, scanned by every GC
// pass and orphan-root walk for ever (spoond-966 L3). The comparison is
// in Go because updated_at is RFC 3339 with a variable-width fraction
// (see PruneJobs). It returns the ids it removed, so the caller can log
// them.
func (db *DB) DeleteBuildsPermanently(ctx context.Context, cutoff time.Time) ([]string, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT build_id, updated_at FROM builds WHERE state = 'deleted'`)
	if err != nil {
		return nil, fmt.Errorf("store: list deleted builds: %w", err)
	}
	var stale []string
	for rows.Next() {
		var id, updatedAt string
		if err := rows.Scan(&id, &updatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: list deleted builds: %w", err)
		}
		// Compare in Go: updated_at is RFC 3339 with a variable-width
		// fraction, so a plain string comparison misorders sub-second
		// forms (see PruneJobs).
		if parseTime(updatedAt).Before(cutoff) {
			stale = append(stale, id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: list deleted builds: %w", err)
	}
	rows.Close()

	var removed []string
	for _, id := range stale {
		ok, err := db.deleteBuildPermanently(ctx, id)
		if err != nil {
			return removed, err
		}
		if ok {
			removed = append(removed, id)
		}
	}
	return removed, nil
}

// deleteBuildPermanently removes one already-deleted build row and its
// build_refs rows in a single transaction, guarded on the row still
// being state deleted. It reports whether the row was removed. The
// shared transaction means a crash cannot leave a deleted row without
// its refs or vice versa, and the state guard means a build that was
// un-deleted between the scan and the delete keeps both (spoond-ob18).
func (db *DB) deleteBuildPermanently(ctx context.Context, id string) (bool, error) {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store: begin delete build %s: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM build_refs WHERE build_id = ? AND EXISTS (SELECT 1 FROM builds WHERE build_id = ? AND state = 'deleted')`, id, id); err != nil {
		tx.Rollback()
		return false, fmt.Errorf("store: delete build refs %s: %w", id, err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM builds WHERE build_id = ? AND state = 'deleted'`, id)
	if err != nil {
		tx.Rollback()
		return false, fmt.Errorf("store: delete build %s: %w", id, err)
	}
	n, rowsErr := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: commit delete build %s: %w", id, err)
	}
	return rowsErr == nil && n > 0, nil
}

// UpdateBuildSize records a build's measured disk size (U11 disk
// accounting). It leaves updated_at alone: the GC's one-hour age rule
// keys on it.
func (db *DB) UpdateBuildSize(ctx context.Context, id string, sizeBytes int64) error {
	_, err := db.w.ExecContext(ctx, `UPDATE builds SET size_bytes = ? WHERE build_id = ?`, sizeBytes, id)
	if err != nil {
		return fmt.Errorf("store: update build size %s: %w", id, err)
	}
	return nil
}

// CountImageUse adds one lease grant to image's lifetime count.
func (db *DB) CountImageUse(ctx context.Context, image string) error {
	_, err := db.w.ExecContext(ctx, `INSERT INTO image_uses (image, uses) VALUES (?, 1)
ON CONFLICT(image) DO UPDATE SET uses = uses + 1`, image)
	if err != nil {
		return fmt.Errorf("store: count use of %s: %w", image, err)
	}
	return nil
}

// ImageUses returns every image's lifetime lease-grant count.
func (db *DB) ImageUses(ctx context.Context) (map[string]int, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT image, uses FROM image_uses`)
	if err != nil {
		return nil, fmt.Errorf("store: image uses: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var image string
		var n int
		if err := rows.Scan(&image, &n); err != nil {
			return nil, fmt.Errorf("store: image uses: %w", err)
		}
		out[image] = n
	}
	return out, rows.Err()
}

// SandboxRow is one row of the sandboxes table: a running (or recently
// running) E2B microVM. LeaseID "" marks a pool sandbox.
type SandboxRow struct {
	SandboxID, LeaseID, BuildID, ExecutionID, HostIP string // LeaseID "" = pool sandbox
	VCPU, MemoryMB                                   int
	StartedAt, EndAt                                 time.Time
}

// UpsertSandbox is INSERT ... ON CONFLICT(sandbox_id) DO UPDATE SET every column:
// resume and crash recovery reuse a sandbox id.
func (db *DB) UpsertSandbox(ctx context.Context, r SandboxRow) error {
	_, err := db.w.ExecContext(ctx, `INSERT INTO sandboxes
		(sandbox_id, lease_id, build_id, execution_id, host_ip, vcpu, memory_mb, started_at, end_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(sandbox_id) DO UPDATE SET
		  lease_id = excluded.lease_id,
		  build_id = excluded.build_id,
		  execution_id = excluded.execution_id,
		  host_ip = excluded.host_ip,
		  vcpu = excluded.vcpu,
		  memory_mb = excluded.memory_mb,
		  started_at = excluded.started_at,
		  end_at = excluded.end_at`,
		r.SandboxID, r.LeaseID, r.BuildID, r.ExecutionID, r.HostIP,
		r.VCPU, r.MemoryMB, formatTime(r.StartedAt), formatTime(r.EndAt))
	if err != nil {
		return fmt.Errorf("store: upsert sandbox %s: %w", r.SandboxID, err)
	}
	return nil
}

// DeleteSandbox drops a sandbox entry. Idempotent: deleting an absent
// id is not an error.
func (db *DB) DeleteSandbox(ctx context.Context, sandboxID string) error {
	_, err := db.w.ExecContext(ctx, `DELETE FROM sandboxes WHERE sandbox_id = ?`, sandboxID)
	if err != nil {
		return fmt.Errorf("store: delete sandbox %s: %w", sandboxID, err)
	}
	return nil
}

// GetSandbox returns the sandbox row for sandboxID, or ErrNotFound.
func (db *DB) GetSandbox(ctx context.Context, sandboxID string) (SandboxRow, error) {
	row := db.r.QueryRowContext(ctx, `SELECT
		sandbox_id, lease_id, build_id, execution_id, host_ip, vcpu, memory_mb, started_at, end_at
		FROM sandboxes WHERE sandbox_id = ?`, sandboxID)
	return scanSandbox(row.Scan)
}

// GetSandboxByLease returns the sandbox row leased to leaseID, or
// ErrNotFound.
func (db *DB) GetSandboxByLease(ctx context.Context, leaseID string) (SandboxRow, error) {
	row := db.r.QueryRowContext(ctx, `SELECT
		sandbox_id, lease_id, build_id, execution_id, host_ip, vcpu, memory_mb, started_at, end_at
		FROM sandboxes WHERE lease_id = ?`, leaseID)
	return scanSandbox(row.Scan)
}

// ListSandboxes returns every sandbox row, ordered by started_at.
func (db *DB) ListSandboxes(ctx context.Context) ([]SandboxRow, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT
		sandbox_id, lease_id, build_id, execution_id, host_ip, vcpu, memory_mb, started_at, end_at
		FROM sandboxes ORDER BY started_at`)
	if err != nil {
		return nil, fmt.Errorf("store: list sandboxes: %w", err)
	}
	defer rows.Close()
	var out []SandboxRow
	for rows.Next() {
		r, err := scanSandbox(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: list sandboxes: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list sandboxes: %w", err)
	}
	return out, nil
}

func scanSandbox(scan func(dest ...any) error) (SandboxRow, error) {
	var r SandboxRow
	var startedAt, endAt string
	err := scan(&r.SandboxID, &r.LeaseID, &r.BuildID, &r.ExecutionID, &r.HostIP,
		&r.VCPU, &r.MemoryMB, &startedAt, &endAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SandboxRow{}, ErrNotFound
	}
	if err != nil {
		return SandboxRow{}, err
	}
	r.StartedAt = parseTime(startedAt)
	r.EndAt = parseTime(endAt)
	return r, nil
}
