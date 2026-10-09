package store

import (
	"context"
	"fmt"
)

// Fair-share usage accounting (#145 FS1). These read the recorded
// snapshot sizes (builds.size_bytes, named_snapshots.size_bytes) in one
// query for the whole box, so the per-request owner-usage view never
// walks the filesystem.

// PausedBytesByOwner sums the recorded size_bytes of each owner's
// pause snapshots (builds of kind pause), per owner. Every pause build
// stays on disk until the GC reclaims it, so a lease's whole pause chain
// counts, not only the build it currently resumes from. A deleted build's
// files are gone and is skipped.
func (db *DB) PausedBytesByOwner(ctx context.Context) (map[string]int64, error) {
	return db.bytesByOwner(ctx, `
		SELECT owner, COALESCE(SUM(size_bytes), 0)
		FROM builds
		WHERE kind = 'pause' AND state <> 'deleted'
		GROUP BY owner`)
}

// KeptBytesByOwner sums the recorded size_bytes of the builds each
// owner's leases pinned with keep (lease_kept_builds), deleted builds
// excluded: a pin on thin air holds no disk.
func (db *DB) KeptBytesByOwner(ctx context.Context) (map[string]int64, error) {
	return db.bytesByOwner(ctx, `
		SELECT l.owner, COALESCE(SUM(b.size_bytes), 0)
		FROM lease_kept_builds k
		JOIN leases l ON l.id = k.lease_id
		JOIN builds b ON b.build_id = k.build_id
		WHERE b.state <> 'deleted'
		GROUP BY l.owner`)
}

// bytesByOwner runs a (owner, bytes) query and returns it as a map. A
// result error wraps err with what failed.
func (db *DB) bytesByOwner(ctx context.Context, q string) (map[string]int64, error) {
	rows, err := db.r.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: bytes by owner: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var owner string
		var bytes int64
		if err := rows.Scan(&owner, &bytes); err != nil {
			return nil, fmt.Errorf("store: bytes by owner: %w", err)
		}
		out[owner] = bytes
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: bytes by owner: %w", err)
	}
	return out, nil
}
