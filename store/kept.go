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

// UnkeepBuild drops one kept-builds row. No-op when the lease does not
// keep the build.
func (db *DB) UnkeepBuild(ctx context.Context, leaseID, buildID string) error {
	_, err := db.w.ExecContext(ctx,
		`DELETE FROM lease_kept_builds WHERE lease_id = ? AND build_id = ?`,
		leaseID, buildID)
	if err != nil {
		return fmt.Errorf("store: unkeep build %s for lease %s: %w", buildID, leaseID, err)
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
