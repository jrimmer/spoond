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
// named snapshot prefers its build row's re-measured size and falls back
// to its own recorded size only when the build row is gone (its build is
// a GC root).
//
// The three sets overlap — a saved snapshot's checkpoint stays pinned by
// the lease it was saved from — so a build is attributed to its owner
// once in Used even when it is both kept and named. The per-kind fields
// report each kind's raw bytes.
//
// A named snapshot's size_bytes is the size at save time; the build row
// is re-measured by the hourly disk accounting pass and is authoritative.
// The named arm prefers the build row and falls back to the named row
// only when the build row is gone (COALESCE), so an owner's Named (and
// the Used built from it) can differ from the size_bytes the
// named-snapshot listing reports.
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

// AccountedSnapshotBytes returns the box-wide recorded snapshot bytes
// with every build counted once, even when several owners share it. A
// build can be attributed to more than one owner — a lease may pin a
// build another owner owns, and a named snapshot is owned independently
// of the owner of its build — so summing the per-owner Used values would
// count a shared build once per owner. This derives the accounted part
// of the disk slice basis from DISTINCT build_id instead (FS1 follow-up
// b).
//
// Every kind contributes its build exactly once: a pause build, a kept
// pin and a named snapshot all collapse to one row per build_id, and the
// largest recorded size wins (the build row's re-measured size is the
// authoritative one; a named snapshot whose build row is gone still
// counts with its own recorded size).
func (db *DB) AccountedSnapshotBytes(ctx context.Context) (int64, error) {
	var total int64
	err := db.r.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(size_bytes), 0) FROM (
			SELECT build_id, MAX(size_bytes) AS size_bytes
			FROM (
				SELECT build_id, size_bytes
				FROM builds
				WHERE kind = 'pause' AND state <> 'deleted'
				UNION ALL
				SELECT b.build_id, b.size_bytes
				FROM lease_kept_builds k
				JOIN builds b ON b.build_id = k.build_id
				WHERE b.state <> 'deleted'
				UNION ALL
				SELECT n.build_id, COALESCE(b.size_bytes, n.size_bytes)
				FROM named_snapshots n
				LEFT JOIN builds b ON b.build_id = n.build_id
			)
			GROUP BY build_id
		)`).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("store: accounted snapshot bytes: %w", err)
	}
	return total, nil
}
