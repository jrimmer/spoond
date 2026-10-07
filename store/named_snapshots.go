package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Named snapshots (2.7, #83): a checkpoint build with a name and a
// version, owned by an identity and outliving the lease it was saved
// from. Every access function is owner-scoped by the caller; these
// helpers never cross owners. A version row is inserted only after its
// checkpoint build is ready, in one transaction that computes
// max(version)+1, so "latest" moves atomically and a failed or in-flight
// save is never visible.

// NamedSnapshotRow is one row of the named_snapshots table.
type NamedSnapshotRow struct {
	Owner, Name         string
	Version             int64
	BuildID             string
	IdempotencyKey      string
	SourceLeaseID       string
	Image               string
	ImageBuildID        string
	MemoryMB            int
	SizeBytes           int64
	EnvdVersion         string
	FirecrackerVersion  string
	OrchestratorVersion string
	CreatedAt           time.Time
}

const namedSnapshotColumns = `owner, name, version, build_id, idempotency_key,
	source_lease_id, image, image_build_id, memory_mb, size_bytes,
	envd_version, firecracker_version, orchestrator_version, created_at`

// InsertNamedSnapshot inserts a version row for (owner, name), assigning
// version = max(version)+1 for that name inside one transaction. keep is
// the requested retention for the name: it is stored in
// named_snapshot_names only when that name has no settings row yet (the
// first save sets it; later saves ignore the field). The returned row
// carries the assigned version.
func (db *DB) InsertNamedSnapshot(ctx context.Context, r NamedSnapshotRow, keep int) (NamedSnapshotRow, error) {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return NamedSnapshotRow{}, fmt.Errorf("store: insert named snapshot %s/%s: %w", r.Owner, r.Name, err)
	}
	if err := insertNamedSnapshotTx(ctx, tx, &r, keep); err != nil {
		tx.Rollback()
		return NamedSnapshotRow{}, err
	}
	if err := tx.Commit(); err != nil {
		return NamedSnapshotRow{}, fmt.Errorf("store: insert named snapshot %s/%s: %w", r.Owner, r.Name, err)
	}
	return r, nil
}

// insertNamedSnapshotTx does the work of InsertNamedSnapshot on an open
// transaction.
func insertNamedSnapshotTx(ctx context.Context, tx *sql.Tx, r *NamedSnapshotRow, keep int) error {
	var maxVersion, highWater sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(version) FROM named_snapshots WHERE owner = ? AND name = ?`,
		r.Owner, r.Name).Scan(&maxVersion); err != nil {
		return fmt.Errorf("store: named snapshot max version %s/%s: %w", r.Owner, r.Name, err)
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT last_version FROM named_snapshot_names WHERE owner = ? AND name = ?`,
		r.Owner, r.Name).Scan(&highWater); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: named snapshot high-water %s/%s: %w", r.Owner, r.Name, err)
	}
	// The high-water mark is the larger of the surviving max version and
	// the last version ever assigned, so deleting the latest version and
	// saving again never reuses its number (S4).
	r.Version = max(maxVersion.Int64, highWater.Int64) + 1
	if _, err := tx.ExecContext(ctx, `INSERT INTO named_snapshots (`+namedSnapshotColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Owner, r.Name, r.Version, r.BuildID, r.IdempotencyKey,
		r.SourceLeaseID, r.Image, r.ImageBuildID, r.MemoryMB, r.SizeBytes,
		r.EnvdVersion, r.FirecrackerVersion, r.OrchestratorVersion, formatTime(r.CreatedAt)); err != nil {
		return fmt.Errorf("store: insert named snapshot %s/%s@%d: %w", r.Owner, r.Name, r.Version, err)
	}
	// The name's retention is a first-save setting: keep is stored only
	// when the name has no settings row yet, while last_version always
	// advances (S4).
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO named_snapshot_names (owner, name, keep, last_version, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(owner, name) DO UPDATE SET last_version = excluded.last_version`,
		r.Owner, r.Name, keep, r.Version, formatTime(r.CreatedAt)); err != nil {
		return fmt.Errorf("store: insert named snapshot name %s/%s: %w", r.Owner, r.Name, err)
	}
	return nil
}

// GetNamedSnapshot returns one version of one name, or ErrNotFound.
func (db *DB) GetNamedSnapshot(ctx context.Context, owner, name string, version int64) (NamedSnapshotRow, error) {
	row := db.r.QueryRowContext(ctx,
		`SELECT `+namedSnapshotColumns+` FROM named_snapshots WHERE owner = ? AND name = ? AND version = ?`,
		owner, name, version)
	return scanNamedSnapshot(row.Scan)
}

// GetNamedSnapshotByBuild returns the version row a lease's
// snapshot_build_id points at, owner-scoped. It resolves the name and
// version for a lease's "snapshot" view (A3) and for the retention
// re-run after a lease is released (S5), or ErrNotFound when no row
// carries the build (a forced delete).
func (db *DB) GetNamedSnapshotByBuild(ctx context.Context, owner, buildID string) (NamedSnapshotRow, error) {
	if buildID == "" {
		return NamedSnapshotRow{}, ErrNotFound
	}
	row := db.r.QueryRowContext(ctx,
		`SELECT `+namedSnapshotColumns+` FROM named_snapshots WHERE owner = ? AND build_id = ?`,
		owner, buildID)
	return scanNamedSnapshot(row.Scan)
}

// GetNamedSnapshotLatest returns the newest version of one name, or
// ErrNotFound.
func (db *DB) GetNamedSnapshotLatest(ctx context.Context, owner, name string) (NamedSnapshotRow, error) {
	row := db.r.QueryRowContext(ctx,
		`SELECT `+namedSnapshotColumns+` FROM named_snapshots
		 WHERE owner = ? AND name = ? ORDER BY version DESC LIMIT 1`, owner, name)
	return scanNamedSnapshot(row.Scan)
}

// GetNamedSnapshotByKey returns the version saved under an idempotency
// key for one name, or ErrNotFound. The key is scoped by (owner, name),
// never by lease: a replay from any lease answers the version the first
// save produced.
func (db *DB) GetNamedSnapshotByKey(ctx context.Context, owner, name, key string) (NamedSnapshotRow, error) {
	if key == "" {
		return NamedSnapshotRow{}, ErrNotFound
	}
	row := db.r.QueryRowContext(ctx,
		`SELECT `+namedSnapshotColumns+` FROM named_snapshots
		 WHERE owner = ? AND name = ? AND idempotency_key = ?`, owner, name, key)
	return scanNamedSnapshot(row.Scan)
}

// ListNamedSnapshots returns every version of every name for owner whose
// name starts with prefix ("" = all), ordered by name ascending and
// version descending (newest first).
func (db *DB) ListNamedSnapshots(ctx context.Context, owner, prefix string) ([]NamedSnapshotRow, error) {
	q := `SELECT ` + namedSnapshotColumns + ` FROM named_snapshots WHERE owner = ?`
	args := []any{owner}
	if prefix != "" {
		q += ` AND name LIKE ? ESCAPE '\'`
		args = append(args, likePrefix(prefix))
	}
	q += ` ORDER BY name, version DESC`
	return db.queryNamedSnapshots(ctx, q, args...)
}

// ListNamedSnapshotsExact returns every version of one exact name
// (owner-scoped), newest first. It is the exact-name counterpart of
// ListNamedSnapshots' prefix match: the whole-name delete must not be
// tricked by a longer name sharing a prefix.
func (db *DB) ListNamedSnapshotsExact(ctx context.Context, owner, name string) ([]NamedSnapshotRow, error) {
	q := `SELECT ` + namedSnapshotColumns + ` FROM named_snapshots
		WHERE owner = ? AND name = ? ORDER BY version DESC`
	return db.queryNamedSnapshots(ctx, q, owner, name)
}

// queryNamedSnapshots runs a named_snapshots query and scans the rows.
func (db *DB) queryNamedSnapshots(ctx context.Context, q string, args ...any) ([]NamedSnapshotRow, error) {
	rows, err := db.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list named snapshots: %w", err)
	}
	defer rows.Close()
	var out []NamedSnapshotRow
	for rows.Next() {
		r, err := scanNamedSnapshot(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: list named snapshots: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list named snapshots: %w", err)
	}
	return out, nil
}

// likePrefix escapes LIKE wildcards in a literal name prefix and appends
// the trailing wildcard.
func likePrefix(prefix string) string {
	out := make([]rune, 0, len(prefix)+1)
	for _, c := range prefix {
		switch c {
		case '%', '_', '\\':
			out = append(out, '\\')
		}
		out = append(out, c)
	}
	return string(out) + "%"
}

// DeleteNamedSnapshot removes one version row. It returns ErrNotFound
// when the row does not exist. The name's settings row (and its
// last_version high-water mark) is kept even when this was the last
// version, so a later save never reuses the deleted number (R3).
func (db *DB) DeleteNamedSnapshot(ctx context.Context, owner, name string, version int64) error {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: delete named snapshot %s/%s@%d: %w", owner, name, version, err)
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM named_snapshots WHERE owner = ? AND name = ? AND version = ?`, owner, name, version)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("store: delete named snapshot %s/%s@%d: %w", owner, name, version, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("store: delete named snapshot %s/%s@%d: %w", owner, name, version, err)
	}
	if n == 0 {
		tx.Rollback()
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: delete named snapshot %s/%s@%d: %w", owner, name, version, err)
	}
	return nil
}

// DeleteNamedSnapshotName removes every version of one name. It returns
// how many versions were deleted. The settings row is kept so the
// last_version high-water mark survives a later save (R3).
func (db *DB) DeleteNamedSnapshotName(ctx context.Context, owner, name string) (int64, error) {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: delete named snapshot name %s/%s: %w", owner, name, err)
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM named_snapshots WHERE owner = ? AND name = ?`, owner, name)
	if err != nil {
		tx.Rollback()
		return 0, fmt.Errorf("store: delete named snapshot name %s/%s: %w", owner, name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		tx.Rollback()
		return 0, fmt.Errorf("store: delete named snapshot name %s/%s: %w", owner, name, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: delete named snapshot name %s/%s: %w", owner, name, err)
	}
	return n, nil
}

// CountNamedSnapshotNames returns how many distinct names owner has. The
// per-owner names cap (MAX_NAMED_SNAPSHOTS) reads it before a first
// save.
func (db *DB) CountNamedSnapshotNames(ctx context.Context, owner string) (int, error) {
	var n int
	if err := db.r.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT name) FROM named_snapshots WHERE owner = ?`, owner).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count named snapshot names of %s: %w", owner, err)
	}
	return n, nil
}

// NamedSnapshotMetrics returns the owner-independent totals for the
// gauges: every version row and the summed size_bytes of its build.
func (db *DB) NamedSnapshotMetrics(ctx context.Context) (versions int, bytes int64, err error) {
	err = db.r.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(size_bytes), 0) FROM named_snapshots`).Scan(&versions, &bytes)
	if err != nil {
		return 0, 0, fmt.Errorf("store: named snapshot metrics: %w", err)
	}
	return versions, bytes, nil
}

// NamedSnapshotBytesOfOwner sums size_bytes over one owner's named
// snapshots: the owner's max_kept_bytes budget counts them alongside
// kept checkpoints.
func (db *DB) NamedSnapshotBytesOfOwner(ctx context.Context, owner string) (int64, error) {
	var bytes int64
	if err := db.r.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(size_bytes), 0) FROM named_snapshots WHERE owner = ?`, owner).Scan(&bytes); err != nil {
		return 0, fmt.Errorf("store: named snapshot bytes of %s: %w", owner, err)
	}
	return bytes, nil
}

// NamedSnapshotKeep returns the retention setting for (owner, name), or
// ErrNotFound when the name has no settings row (and therefore no
// version).
func (db *DB) NamedSnapshotKeep(ctx context.Context, owner, name string) (int, error) {
	var keep int
	err := db.r.QueryRowContext(ctx,
		`SELECT keep FROM named_snapshot_names WHERE owner = ? AND name = ?`, owner, name).Scan(&keep)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: named snapshot keep %s/%s: %w", owner, name, err)
	}
	return keep, nil
}

// SetNamedSnapshotKeep sets a name's retention (PUT
// /api/named-snapshots/{name}). It does not create a settings row for a
// name with no versions.
func (db *DB) SetNamedSnapshotKeep(ctx context.Context, owner, name string, keep int) error {
	_, err := db.w.ExecContext(ctx,
		`UPDATE named_snapshot_names SET keep = ? WHERE owner = ? AND name = ?`, keep, owner, name)
	if err != nil {
		return fmt.Errorf("store: set named snapshot keep %s/%s: %w", owner, name, err)
	}
	return nil
}

// PruneNamedSnapshots drops the versions of (owner, name) that fall
// beyond keep, newest kept, oldest pruned, unless a live lease started
// from the version's build (leases.snapshot_build_id, state != 'lost').
// It returns the build ids of the rows it deleted, so the caller can log
// or account. keep <= 0 prunes nothing.
func (db *DB) PruneNamedSnapshots(ctx context.Context, owner, name string, keep int) ([]string, error) {
	if keep <= 0 {
		return nil, nil
	}
	rows, err := db.r.QueryContext(ctx, `
		SELECT n.version, n.build_id
		FROM named_snapshots n
		WHERE n.owner = ? AND n.name = ?
		  AND n.version NOT IN (
			SELECT version FROM named_snapshots
			WHERE owner = ? AND name = ? ORDER BY version DESC LIMIT ?)
		ORDER BY n.version`, owner, name, owner, name, keep)
	if err != nil {
		return nil, fmt.Errorf("store: prune named snapshots %s/%s: %w", owner, name, err)
	}
	type cand struct {
		version int64
		build   string
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.version, &c.build); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: prune named snapshots %s/%s: %w", owner, name, err)
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: prune named snapshots %s/%s: %w", owner, name, err)
	}
	rows.Close()
	if len(cands) == 0 {
		return nil, nil
	}
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: prune named snapshots %s/%s: %w", owner, name, err)
	}
	var deleted []string
	for _, c := range cands {
		var live int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM leases WHERE snapshot_build_id = ? AND state <> 'lost'`, c.build).Scan(&live); err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("store: prune named snapshots %s/%s: %w", owner, name, err)
		}
		if live > 0 {
			continue
		}
		res, err := tx.ExecContext(ctx,
			`DELETE FROM named_snapshots WHERE owner = ? AND name = ? AND version = ?`, owner, name, c.version)
		if err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("store: prune named snapshots %s/%s: %w", owner, name, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			deleted = append(deleted, c.build)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: prune named snapshots %s/%s: %w", owner, name, err)
	}
	return deleted, nil
}

// NamedSnapshotBuilds returns every named snapshot's build id as a set:
// the GC's roots (keptBuilds, orphanRoots).
func (db *DB) NamedSnapshotBuilds(ctx context.Context) (map[string]bool, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT build_id FROM named_snapshots`)
	if err != nil {
		return nil, fmt.Errorf("store: named snapshot builds: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: named snapshot builds: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: named snapshot builds: %w", err)
	}
	return out, nil
}

// LiveLeaseCountsByBuild counts non-lost leases started from each of
// buildIDs in one query: the list route's in_use values without one
// query per version (N5). A build absent from the result has zero live
// leases.
func (db *DB) LiveLeaseCountsByBuild(ctx context.Context, buildIDs []string) (map[string]int, error) {
	out := make(map[string]int, len(buildIDs))
	if len(buildIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(buildIDs))
	args := make([]any, len(buildIDs))
	for i, id := range buildIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := db.r.QueryContext(ctx,
		`SELECT snapshot_build_id, COUNT(*) FROM leases
		 WHERE snapshot_build_id IN (`+strings.Join(placeholders, ", ")+`)
		   AND state <> 'lost'
		 GROUP BY snapshot_build_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: live leases by build: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("store: live leases by build: %w", err)
		}
		out[id] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: live leases by build: %w", err)
	}
	return out, nil
}

// LiveLeasesUsingBuild counts non-lost leases started from buildID
// (leases.snapshot_build_id). The delete route reads it for its 409 and
// the live-lease retention rule reads it per candidate.
func (db *DB) LiveLeasesUsingBuild(ctx context.Context, buildID string) (int, error) {
	var n int
	if err := db.r.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM leases WHERE snapshot_build_id = ? AND state <> 'lost'`, buildID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: live leases using build %s: %w", buildID, err)
	}
	return n, nil
}

// MarkUnnamedCheckpointsFailed marks every checkpoint build still
// `building` that has no named snapshot row as `failed` (A7): a backend
// that stopped mid-save wrote no version, so a build that finishes later
// in the orchestrator is an ordinary GC candidate or orphan. It returns
// how many rows it marked.
func (db *DB) MarkUnnamedCheckpointsFailed(ctx context.Context) (int64, error) {
	res, err := db.w.ExecContext(ctx, `
		UPDATE builds SET state = 'failed', error = ?, updated_at = ?
		WHERE state = 'building' AND kind = 'checkpoint'
		  AND build_id NOT IN (SELECT build_id FROM named_snapshots)`,
		"backend stopped during save", formatTime(time.Now()))
	if err != nil {
		return 0, fmt.Errorf("store: mark unnamed checkpoints failed: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: mark unnamed checkpoints failed: %w", err)
	}
	return n, nil
}

func scanNamedSnapshot(scan func(dest ...any) error) (NamedSnapshotRow, error) {
	var r NamedSnapshotRow
	var createdAt string
	err := scan(&r.Owner, &r.Name, &r.Version, &r.BuildID, &r.IdempotencyKey,
		&r.SourceLeaseID, &r.Image, &r.ImageBuildID, &r.MemoryMB, &r.SizeBytes,
		&r.EnvdVersion, &r.FirecrackerVersion, &r.OrchestratorVersion, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return NamedSnapshotRow{}, ErrNotFound
	}
	if err != nil {
		return NamedSnapshotRow{}, err
	}
	r.CreatedAt = parseTime(createdAt)
	return r, nil
}
