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

// CountBuildingTemplateBuilds returns how many image/template builds were
// set building at or after notBefore. The catalog is shared with the
// separate `spoond images build` process, so the backend can see its
// bakes here; notBefore bounds the count to builds that could still be
// running (the build timeout), so a killed build left `building` for ever
// does not wedge the caller. Used by the periodic orphan sweep, which
// must not risk deleting a build sandbox it cannot see in Server.List
// (N1).
func (db *DB) CountBuildingTemplateBuilds(ctx context.Context, notBefore time.Time) (int, error) {
	row := db.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM builds
		WHERE state = 'building' AND kind = 'template' AND created_at >= ?`,
		formatTime(notBefore))
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count building template builds: %w", err)
	}
	return n, nil
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
