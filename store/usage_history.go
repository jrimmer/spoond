package store

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Usage history (#140 U4). One row per owner per sample tick, written by
// the backend's sampler from the fair-share snapshot and pruned once
// older than the retention window. Rows are the caller's own view:
// GET /api/usage/history reads them oldest first.

// UsageSampleRow is one usage_samples row.
type UsageSampleRow struct {
	Owner         string
	At            time.Time
	MemoryUsedMiB int
	DiskUsedBytes int64
	Running       int
	Paused        int
}

// usageSampleColumns is the read projection; rowid is selected only for
// the prune, never scanned into the row.
const usageSampleColumns = `owner, at, memory_used_mib, disk_used_bytes, running, paused`

// InsertUsageSample records one owner's sample at one tick.
func (db *DB) InsertUsageSample(ctx context.Context, s UsageSampleRow) error {
	_, err := db.w.ExecContext(ctx, `
INSERT INTO usage_samples (`+usageSampleColumns+`)
VALUES (?, ?, ?, ?, ?, ?)`,
		s.Owner, formatTime(s.At), s.MemoryUsedMiB, s.DiskUsedBytes, s.Running, s.Paused)
	if err != nil {
		return fmt.Errorf("store: insert usage sample %s: %w", s.Owner, err)
	}
	return nil
}

// ListUsageHistory returns one owner's samples at or after since, oldest
// first. A zero since returns every sample.
func (db *DB) ListUsageHistory(ctx context.Context, owner string, since time.Time) ([]UsageSampleRow, error) {
	rows, err := db.r.QueryContext(ctx, `
SELECT `+usageSampleColumns+` FROM usage_samples WHERE owner = ?`, owner)
	if err != nil {
		return nil, fmt.Errorf("store: list usage history %s: %w", owner, err)
	}
	defer rows.Close()
	out, err := scanUsageSamples(rows.Scan, rows.Next, rows.Err)
	if err != nil {
		return nil, fmt.Errorf("store: list usage history %s: %w", owner, err)
	}
	// Order in Go: the stored RFC 3339 fraction width varies, so a string
	// sort would mis-order values with and without fractional seconds.
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	if !since.IsZero() {
		kept := out[:0]
		for _, s := range out {
			if !s.At.Before(since) {
				kept = append(kept, s)
			}
		}
		out = kept
	}
	return out, nil
}

// PruneUsageSamples deletes every sample older than cutoff, returning
// how many rows went. The comparison is in Go for the same RFC 3339
// fraction-width reason as ListUsageHistory, and deletion is by rowid so
// two samples that share an owner and time cannot delete each other.
func (db *DB) PruneUsageSamples(ctx context.Context, cutoff time.Time) (int64, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT rowid, at FROM usage_samples`)
	if err != nil {
		return 0, fmt.Errorf("store: prune usage samples: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		var at string
		if err := rows.Scan(&id, &at); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: prune usage samples: %w", err)
		}
		if parseTime(at).Before(cutoff) {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("store: prune usage samples: %w", err)
	}
	rows.Close()
	var n int64
	for _, id := range ids {
		res, err := db.w.ExecContext(ctx, `DELETE FROM usage_samples WHERE rowid = ?`, id)
		if err != nil {
			return n, fmt.Errorf("store: prune usage samples: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return n, fmt.Errorf("store: prune usage samples: %w", err)
		}
		n += affected
	}
	return n, nil
}

// scanUsageSamples reads usage_samples rows with next/scan, so the prune
// and the read share one scan body.
func scanUsageSamples(scan func(...any) error, next func() bool, errFn func() error) ([]UsageSampleRow, error) {
	var out []UsageSampleRow
	for next() {
		var s UsageSampleRow
		var at string
		if err := scan(&s.Owner, &at, &s.MemoryUsedMiB, &s.DiskUsedBytes, &s.Running, &s.Paused); err != nil {
			return nil, err
		}
		s.At = parseTime(at)
		out = append(out, s)
	}
	if err := errFn(); err != nil {
		return nil, err
	}
	return out, nil
}
