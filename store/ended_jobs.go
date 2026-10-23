package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Ended jobs (migration 0035): a lease's job records copied out of
// lease_jobs when the lease ends, since those rows cascade away with
// the lease row. The rows keep the JobRow shape (jobColumns) plus the
// lease's owner, who may read them back; the sweeper prunes them on the
// job retention window.

// ArchiveJobsOfLease copies every lease_jobs row of leaseID into
// ended_jobs, recording leaseOwner as the lease's owner. A job still
// running there becomes lost with reason, ended at endedAt, and keeps
// the stderr tail tails names for it (the guest's last words, read
// before the sandbox went); every other row is copied as it stands. A
// job already archived keeps its first copy. Returns how many rows were
// copied.
func (db *DB) ArchiveJobsOfLease(ctx context.Context, leaseID, leaseOwner string, endedAt time.Time, reason string, tails map[string]string) (int64, error) {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: archive jobs of %s: %w", leaseID, err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
INSERT INTO ended_jobs (lease_owner, `+jobColumns+`)
SELECT ?, job_id, lease_id, owner, cmd, cwd, state, exit_code,
	started_at, COALESCE(ended_at, ''), stderr_tail, generation, max_runtime_secs, reason,
	retention_secs, output_max_bytes, stdout_dropped_bytes, stdout_dropped_newlines,
	stdout_dropped_ends_with_newline, stderr_dropped_bytes, stderr_dropped_newlines,
	stderr_dropped_ends_with_newline
FROM lease_jobs WHERE lease_id = ?
ON CONFLICT(job_id) DO NOTHING`, leaseOwner, leaseID)
	if err != nil {
		return 0, fmt.Errorf("store: archive jobs of %s: %w", leaseID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: archive jobs of %s: %w", leaseID, err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE ended_jobs SET state='lost', ended_at=?, reason=?
WHERE lease_id = ? AND state='running'`, formatTime(endedAt), reason, leaseID); err != nil {
		return 0, fmt.Errorf("store: archive jobs of %s: %w", leaseID, err)
	}
	for jobID, tail := range tails {
		if _, err := tx.ExecContext(ctx, `
UPDATE ended_jobs SET stderr_tail=? WHERE job_id = ? AND lease_id = ? AND stderr_tail = ''`,
			tail, jobID, leaseID); err != nil {
			return 0, fmt.Errorf("store: archive jobs of %s: %w", leaseID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: archive jobs of %s: %w", leaseID, err)
	}
	return n, nil
}

// ListEndedJobs returns an ended lease's archived jobs, newest first,
// and the lease's owner ("" when nothing is archived for leaseID).
func (db *DB) ListEndedJobs(ctx context.Context, leaseID string) ([]JobRow, string, error) {
	rows, err := db.r.QueryContext(ctx,
		`SELECT lease_owner, `+jobColumns+` FROM ended_jobs WHERE lease_id = ? ORDER BY started_at DESC, job_id`, leaseID)
	if err != nil {
		return nil, "", fmt.Errorf("store: list ended jobs of %s: %w", leaseID, err)
	}
	defer rows.Close()
	var out []JobRow
	var leaseOwner string
	for rows.Next() {
		var owner string
		r, err := scanJob(func(dest ...any) error {
			return rows.Scan(append([]any{&owner}, dest...)...)
		})
		if err != nil {
			return nil, "", fmt.Errorf("store: list ended jobs of %s: %w", leaseID, err)
		}
		leaseOwner = owner
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("store: list ended jobs of %s: %w", leaseID, err)
	}
	return out, leaseOwner, nil
}

// PruneEndedJobs deletes the archived jobs whose retention has run out
// against now: the row's own retention_secs when it asked for one, else
// hostRetention, counted from the job's end (its start for a row that
// never recorded one). The comparison is in Go for the same RFC3339
// fraction reason as PruneJobs.
func (db *DB) PruneEndedJobs(ctx context.Context, now time.Time, hostRetention time.Duration) (int64, error) {
	rows, err := db.r.QueryContext(ctx, `SELECT `+jobColumns+` FROM ended_jobs`)
	if err != nil {
		return 0, fmt.Errorf("store: list ended jobs: %w", err)
	}
	all, err := scanJobs(rows)
	rows.Close()
	if err != nil {
		return 0, fmt.Errorf("store: list ended jobs: %w", err)
	}
	var ids []any
	for _, r := range all {
		window := hostRetention
		if r.RetentionSecs > 0 && (window <= 0 || time.Duration(r.RetentionSecs)*time.Second < window) {
			window = time.Duration(r.RetentionSecs) * time.Second
		}
		agedAt := r.EndedAt
		if agedAt.IsZero() {
			agedAt = r.StartedAt
		}
		if window <= 0 || agedAt.Add(window).Before(now) {
			ids = append(ids, r.JobID)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	res, err := db.w.ExecContext(ctx, `DELETE FROM ended_jobs WHERE job_id IN (`+placeholders+`)`, ids...)
	if err != nil {
		return 0, fmt.Errorf("store: prune ended jobs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune ended jobs: %w", err)
	}
	return n, nil
}
