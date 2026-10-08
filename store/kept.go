package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Kept builds (2.3, #121): the checkpoints a lease pinned with
// {"keep":true}. Kept builds are GC roots while the lease lives and can
// be restored in place; releasing the lease (any path) removes its
// rows, so the next GC pass may reclaim the builds. Rows also cascade
// on the lease row's delete.

// KeepBuild pins buildID to leaseID (INSERT OR IGNORE: keeping the same
// build twice keeps kept_at at the first keep).
func (db *DB) KeepBuild(ctx context.Context, leaseID, buildID string, keptAt time.Time) error {
	_, err := db.w.ExecContext(ctx,
		`INSERT OR IGNORE INTO lease_kept_builds (lease_id, build_id, kept_at) VALUES (?, ?, ?)`,
		leaseID, buildID, formatTime(keptAt))
	if err != nil {
		return fmt.Errorf("store: keep build %s for lease %s: %w", buildID, leaseID, err)
	}
	return nil
}

// UnkeepBuild drops every kept-builds row naming the build, across
// all leases. The snapshot delete path uses it: the owner deleting the
// snapshot unpins it wherever it is pinned. Today only the lease that
// checkpointed a build can pin it (each checkpoint mints a fresh build
// id and only handleCheckpoint pins), so this matches at most the
// caller's own pins; a single-lease delete is reserved for a future
// cross-lease keep.
func (db *DB) UnkeepBuildAny(ctx context.Context, buildID string) error {
	_, err := db.w.ExecContext(ctx,
		`DELETE FROM lease_kept_builds WHERE build_id = ?`, buildID)
	if err != nil {
		return fmt.Errorf("store: unkeep build %s: %w", buildID, err)
	}
	return nil
}

// DeleteKeptBuilds drops every kept-builds row of one lease: the release
// path, so the next GC pass may reclaim the builds.
func (db *DB) DeleteKeptBuilds(ctx context.Context, leaseID string) error {
	_, err := db.w.ExecContext(ctx,
		`DELETE FROM lease_kept_builds WHERE lease_id = ?`, leaseID)
	if err != nil {
		return fmt.Errorf("store: delete kept builds of lease %s: %w", leaseID, err)
	}
	return nil
}

// ListKeptBuildsOfOwner returns the build ids pinned by the leases of
// one owner, ordered by build id. Deleting a user reads it before the
// pins go, so the response can name what was unpinned.
func (db *DB) ListKeptBuildsOfOwner(ctx context.Context, owner string) ([]string, error) {
	rows, err := db.r.QueryContext(ctx, `
		SELECT k.build_id FROM lease_kept_builds k
		JOIN leases l ON l.id = k.lease_id
		WHERE l.owner = ?
		ORDER BY k.build_id`, owner)
	if err != nil {
		return nil, fmt.Errorf("store: list kept builds of owner %s: %w", owner, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: list kept builds of owner %s: %w", owner, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list kept builds of owner %s: %w", owner, err)
	}
	return out, nil
}

// DeleteKeptBuildsOfOwner drops every kept-builds row pinned by one
// owner's leases, returning the build ids it unpinned. Deleting a user
// uses it after the leases are released, so a pin whose lease row lagged
// (or that a release path missed) cannot outlive the user and pin the
// build against GC.
func (db *DB) DeleteKeptBuildsOfOwner(ctx context.Context, owner string) ([]string, error) {
	ids, err := db.ListKeptBuildsOfOwner(ctx, owner)
	if err != nil {
		return nil, err
	}
	if _, err := db.w.ExecContext(ctx,
		`DELETE FROM lease_kept_builds WHERE lease_id IN (SELECT id FROM leases WHERE owner = ?)`, owner); err != nil {
		return nil, fmt.Errorf("store: delete kept builds of owner %s: %w", owner, err)
	}
	return ids, nil
}

// ListKeptBuilds returns every (lease_id, build_id) row as
// lease id -> kept build ids.
func (db *DB) ListKeptBuilds(ctx context.Context) (map[string][]string, error) {
	rows, err := db.r.QueryContext(ctx,
		`SELECT lease_id, build_id FROM lease_kept_builds ORDER BY kept_at`)
	if err != nil {
		return nil, fmt.Errorf("store: list kept builds: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var leaseID, buildID string
		if err := rows.Scan(&leaseID, &buildID); err != nil {
			return nil, fmt.Errorf("store: list kept builds: %w", err)
		}
		out[leaseID] = append(out[leaseID], buildID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list kept builds: %w", err)
	}
	return out, nil
}

// KeptBuildRow is one kept checkpoint as the lease detail shows it
// (#126): the build's id, its recorded disk size and when it was kept.
type KeptBuildRow struct {
	BuildID   string
	SizeBytes int64
	KeptAt    time.Time
}

// ListKeptBuildRows returns the lease's kept builds with sizes and
// kept_at, oldest keep first. A kept build whose row is gone (deleted
// under the pin) is skipped: it holds no disk and restores nothing.
func (db *DB) ListKeptBuildRows(ctx context.Context, leaseID string) ([]KeptBuildRow, error) {
	rows, err := db.r.QueryContext(ctx, `
		SELECT k.build_id, COALESCE(b.size_bytes, 0), k.kept_at
		FROM lease_kept_builds k
		LEFT JOIN builds b ON b.build_id = k.build_id
		WHERE k.lease_id = ?
		ORDER BY k.kept_at`, leaseID)
	if err != nil {
		return nil, fmt.Errorf("store: list kept build rows of lease %s: %w", leaseID, err)
	}
	defer rows.Close()
	var out []KeptBuildRow
	for rows.Next() {
		var r KeptBuildRow
		var keptAt string
		if err := rows.Scan(&r.BuildID, &r.SizeBytes, &keptAt); err != nil {
			return nil, fmt.Errorf("store: list kept build rows of lease %s: %w", leaseID, err)
		}
		r.KeptAt = parseTime(keptAt)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list kept build rows of lease %s: %w", leaseID, err)
	}
	return out, nil
}

// CountKeptBuilds returns how many builds the lease has pinned. The
// per-lease cap (#126) reads it before taking a kept checkpoint.
func (db *DB) CountKeptBuilds(ctx context.Context, leaseID string) (int, error) {
	var n int
	if err := db.r.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM lease_kept_builds WHERE lease_id = ?`, leaseID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count kept builds of lease %s: %w", leaseID, err)
	}
	return n, nil
}

// LeaseKeepsBuild reports whether the lease keeps the build.
func (db *DB) LeaseKeepsBuild(ctx context.Context, leaseID, buildID string) (bool, error) {
	var one int
	err := db.r.QueryRowContext(ctx,
		`SELECT 1 FROM lease_kept_builds WHERE lease_id = ? AND build_id = ?`,
		leaseID, buildID).Scan(&one)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, fmt.Errorf("store: kept check %s/%s: %w", leaseID, buildID, err)
}
