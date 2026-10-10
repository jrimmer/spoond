package store

import (
	"context"
	"fmt"
)

// Fair-share usage accounting (#145 FS1). These read the recorded
// snapshot sizes (builds.size_bytes, named_snapshots.size_bytes) in one
// query for the whole box, so the per-request owner-usage view never
// walks the filesystem.

// OwnerUsage is one owner's recorded snapshot disk bytes by kind.
// Used is the union of the three kinds: a build that is both a kept
// checkpoint and a named snapshot is counted once (R5-5), while the
// per-kind fields still report the bytes that kind holds.
type OwnerUsage struct {
	Paused int64
	Kept   int64
	Named  int64
	Used   int64
}

// DiskUsageByOwner returns every owner's recorded snapshot bytes for the
// whole box in one query: pause snapshots, kept checkpoints and named
// snapshots, from each build's recorded size_bytes. A deleted build's
// files are gone and count for nothing in the kept and pause sets; a
// named snapshot keeps its recorded size (its build is a GC root).
//
// The three sets overlap — a saved snapshot's checkpoint stays pinned by
// the lease it was saved from — so a build is attributed to its owner
// once in Used even when it is both kept and named. The per-kind fields
// report each kind's raw bytes.
func (db *DB) DiskUsageByOwner(ctx context.Context) (map[string]OwnerUsage, error) {
	rows, err := db.r.QueryContext(ctx, `
		SELECT owner, build_id, MAX(size_bytes), MAX(is_pause), MAX(is_kept), MAX(is_named)
		FROM (
			SELECT owner, build_id, size_bytes, 1 AS is_pause, 0 AS is_kept, 0 AS is_named
			FROM builds
			WHERE kind = 'pause' AND state <> 'deleted'
			UNION ALL
			SELECT l.owner, b.build_id, b.size_bytes, 0, 1, 0
			FROM lease_kept_builds k
			JOIN leases l ON l.id = k.lease_id
			JOIN builds b ON b.build_id = k.build_id
			WHERE b.state <> 'deleted'
			UNION ALL
			SELECT n.owner, n.build_id, COALESCE(b.size_bytes, n.size_bytes), 0, 0, 1
			FROM named_snapshots n
			LEFT JOIN builds b ON b.build_id = n.build_id
		)
		GROUP BY owner, build_id`)
	if err != nil {
		return nil, fmt.Errorf("store: disk usage by owner: %w", err)
	}
	defer rows.Close()
	out := map[string]OwnerUsage{}
	for rows.Next() {
		var owner, buildID string
		var size, isPause, isKept, isNamed int64
		if err := rows.Scan(&owner, &buildID, &size, &isPause, &isKept, &isNamed); err != nil {
			return nil, fmt.Errorf("store: disk usage by owner: %w", err)
		}
		u := out[owner]
		if isPause != 0 {
			u.Paused += size
		}
		if isKept != 0 {
			u.Kept += size
		}
		if isNamed != 0 {
			u.Named += size
		}
		u.Used += size
		out[owner] = u
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: disk usage by owner: %w", err)
	}
	return out, nil
}
