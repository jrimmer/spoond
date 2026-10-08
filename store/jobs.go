package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// JobRow is one row of the lease_jobs table (2.6, #135): a background
// exec job started in a lease. The command (cmd) is stored as given and
// never re-rendered; env and secret values are deliberately absent.
type JobRow struct {
	JobID    string
	LeaseID  string
	Owner    string
	Cmd      string
	Cwd      string
	State    string // running | exited | lost
	ExitCode *int   // nil while running
	// StartedAt and EndedAt are zero when unset; EndedAt is zero while
	// the job is running.
	StartedAt  time.Time
	EndedAt    time.Time
	StderrTail string
	// Generation is the lease's continuity generation when the job
	// started: a reconcile that finds the lease on a newer generation
	// marks the job lost.
	Generation int64
	// MaxRuntimeSecs is the effective max runtime recorded at start,
	// in seconds, 0 meaning "no cap". A job is killed and marked
	// exited with Reason timed_out once it has run this long.
	MaxRuntimeSecs int64
	// Reason records why a record left 'running' outside the guest's own
	// rc file: "" for a normal exit, "timed_out" when the max runtime
	// was spent.
	Reason string
}

const jobColumns = `job_id, lease_id, owner, cmd, cwd, state, exit_code,
	started_at, ended_at, stderr_tail, generation, max_runtime_secs, reason`

// JobReasonTimedOut is the lease_jobs.reason recorded when the max
// runtime, not the command, ended a job. It is also the label the API
// and the job_exited event use (spoond-wb5).
const JobReasonTimedOut = "timed_out"

// InsertJob records a newly started background job. state must be
// "running" and exit_code nil.
func (db *DB) InsertJob(ctx context.Context, j JobRow) error {
	_, err := db.w.ExecContext(ctx, `
INSERT INTO lease_jobs (`+jobColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.JobID, j.LeaseID, j.Owner, j.Cmd, j.Cwd, j.State, j.ExitCode,
		formatTime(j.StartedAt), formatTime(j.EndedAt), j.StderrTail, j.Generation,
		j.MaxRuntimeSecs, j.Reason)
	if err != nil {
		return fmt.Errorf("store: insert job %s: %w", j.JobID, err)
	}
	return nil
}

// UpdateJobExit marks a job exited: its exit code, end time and stderr
// tail. changed is false when the row was already exited or lost (the
// first outcome wins).
func (db *DB) UpdateJobExit(ctx context.Context, jobID string, exitCode int, endedAt time.Time, stderrTail string) (bool, error) {
	res, err := db.w.ExecContext(ctx, `
UPDATE lease_jobs SET state='exited', exit_code=?, ended_at=?, stderr_tail=?
WHERE job_id=? AND state='running'`,
		exitCode, formatTime(endedAt), stderrTail, jobID)
	if err != nil {
		return false, fmt.Errorf("store: update job exit %s: %w", jobID, err)
	}
	return rowsChanged(res), nil
}

// MarkJobLost marks a running job lost. changed is false when it was
// already exited or lost.
func (db *DB) MarkJobLost(ctx context.Context, jobID string, endedAt time.Time) (bool, error) {
	res, err := db.w.ExecContext(ctx, `
UPDATE lease_jobs SET state='lost', ended_at=?
WHERE job_id=? AND state='running'`,
		formatTime(endedAt), jobID)
	if err != nil {
		return false, fmt.Errorf("store: mark job lost %s: %w", jobID, err)
	}
	return rowsChanged(res), nil
}

// MarkJobTimedOut marks a running job exited because its max runtime was
// spent: the exit code and stderr tail the kill produced, and Reason
// 'timed_out' so the API and the events can say the cap, not the command,
// ended it. changed is false when the row was already exited or lost (a
// job that ended on its own just before the cap won the race).
func (db *DB) MarkJobTimedOut(ctx context.Context, jobID string, exitCode int, endedAt time.Time, stderrTail string) (bool, error) {
	res, err := db.w.ExecContext(ctx, `
UPDATE lease_jobs SET state='exited', exit_code=?, ended_at=?, stderr_tail=?, reason=?
WHERE job_id=? AND state='running'`,
		exitCode, formatTime(endedAt), stderrTail, JobReasonTimedOut, jobID)
	if err != nil {
		return false, fmt.Errorf("store: mark job timed out %s: %w", jobID, err)
	}
	return rowsChanged(res), nil
}

// rowsChanged reports whether an ExecContext changed at least one row.
func rowsChanged(res sql.Result) bool {
	n, err := res.RowsAffected()
	return err == nil && n > 0
}

// GetJob returns one job by id (ErrNotFound when unknown).
func (db *DB) GetJob(ctx context.Context, jobID string) (JobRow, error) {
	row, err := scanJob(db.r.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM lease_jobs WHERE job_id = ?`, jobID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return JobRow{}, ErrNotFound
	}
	if err != nil {
		return JobRow{}, fmt.Errorf("store: get job %s: %w", jobID, err)
	}
	return row, nil
}

// ListJobs returns a lease's jobs, newest first.
func (db *DB) ListJobs(ctx context.Context, leaseID string) ([]JobRow, error) {
	rows, err := db.r.QueryContext(ctx,
		`SELECT `+jobColumns+` FROM lease_jobs WHERE lease_id = ? ORDER BY started_at DESC, job_id DESC`, leaseID)
	if err != nil {
		return nil, fmt.Errorf("store: list jobs of %s: %w", leaseID, err)
	}
	defer rows.Close()
	return scanJobs(rows)
}

// ListRunningJobsOfOwner returns one owner's running jobs, newest
// first. Deleting a user lists them before cancelling, so the response
// can name what was cancelled.
func (db *DB) ListRunningJobsOfOwner(ctx context.Context, owner string) ([]JobRow, error) {
	rows, err := db.r.QueryContext(ctx,
		`SELECT `+jobColumns+` FROM lease_jobs WHERE owner = ? AND state='running' ORDER BY started_at DESC, job_id DESC`, owner)
	if err != nil {
		return nil, fmt.Errorf("store: list running jobs of %s: %w", owner, err)
	}
	defer rows.Close()
	return scanJobs(rows)
}

// ListRunningJobs returns every running job, oldest first (the
// reconcile pass's work list).
func (db *DB) ListRunningJobs(ctx context.Context) ([]JobRow, error) {
	rows, err := db.r.QueryContext(ctx,
		`SELECT `+jobColumns+` FROM lease_jobs WHERE state='running' ORDER BY started_at`)
	if err != nil {
		return nil, fmt.Errorf("store: list running jobs: %w", err)
	}
	defer rows.Close()
	return scanJobs(rows)
}

// CountRunningJobs returns how many background jobs a lease has running
// (the per-lease cap check).
func (db *DB) CountRunningJobs(ctx context.Context, leaseID string) (int, error) {
	var n int
	if err := db.r.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM lease_jobs WHERE lease_id = ? AND state='running'`, leaseID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count running jobs of %s: %w", leaseID, err)
	}
	return n, nil
}

// CountRunningJobsByLease returns running-job counts per lease, for the
// lease view's jobs.running field.
func (db *DB) CountRunningJobsByLease(ctx context.Context) (map[string]int, error) {
	rows, err := db.r.QueryContext(ctx,
		`SELECT lease_id, COUNT(*) FROM lease_jobs WHERE state='running' GROUP BY lease_id`)
	if err != nil {
		return nil, fmt.Errorf("store: count running jobs: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("store: count running jobs: %w", err)
		}
		out[id] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: count running jobs: %w", err)
	}
	return out, nil
}

// LatestJobExit returns the most recent exited job of each lease, for
// the lease view's jobs.last_exit field. The row is the newest by
// (started_at, job_id), so exact start-time ties resolve deterministically.
func (db *DB) LatestJobExit(ctx context.Context) (map[string]JobRow, error) {
	rows, err := db.r.QueryContext(ctx, `
SELECT `+jobColumns+` FROM lease_jobs j
WHERE state='exited' AND NOT EXISTS (
  SELECT 1 FROM lease_jobs x
  WHERE x.lease_id = j.lease_id AND x.state='exited'
    AND (x.started_at > j.started_at
         OR (x.started_at = j.started_at AND x.job_id > j.job_id))
)`)
	if err != nil {
		return nil, fmt.Errorf("store: latest job exits: %w", err)
	}
	defer rows.Close()
	all, err := scanJobs(rows)
	if err != nil {
		return nil, fmt.Errorf("store: latest job exits: %w", err)
	}
	out := map[string]JobRow{}
	for _, r := range all {
		out[r.LeaseID] = r
	}
	return out, nil
}

// LatestJobExitOfLease returns a lease's most recent exited job. ok is
// false when the lease has none.
func (db *DB) LatestJobExitOfLease(ctx context.Context, leaseID string) (JobRow, bool, error) {
	row, err := scanJob(db.r.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM lease_jobs WHERE lease_id = ? AND state='exited'
		 ORDER BY started_at DESC, job_id DESC LIMIT 1`, leaseID).Scan)
	if errors.Is(err, ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return JobRow{}, false, nil
	}
	if err != nil {
		return JobRow{}, false, fmt.Errorf("store: latest job exit of %s: %w", leaseID, err)
	}
	return row, true, nil
}

// PruneJobs deletes exited jobs older than cutoff, returning how many
// rows went. Running and lost jobs are kept.
//
// ended_at is stored as RFC3339 with a variable-width fraction, so a
// string comparison (ended_at < ?) mis-orders values with and without
// fractional seconds: "12:00:00" sorts after "12:00:00.5" even though
// the latter is earlier. Select the exited rows and compare the parsed
// times in Go instead; retention is days, so reading the candidate set
// is cheap.
func (db *DB) PruneJobs(ctx context.Context, cutoff time.Time) (int64, error) {
	expired, err := db.ListExpiredJobs(ctx, cutoff)
	if err != nil {
		return 0, err
	}
	if len(expired) == 0 {
		return 0, nil
	}
	ids := make([]any, len(expired))
	for i, r := range expired {
		ids[i] = r.JobID
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	res, err := db.w.ExecContext(ctx,
		`DELETE FROM lease_jobs WHERE state='exited' AND job_id IN (`+placeholders+`)`, ids...)
	if err != nil {
		return 0, fmt.Errorf("store: prune jobs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune jobs: %w", err)
	}
	return n, nil
}

// ListExpiredJobs returns the exited jobs whose parsed ended_at is
// before cutoff, for the guest-side cleanup that follows a prune. The
// comparison is in Go because the stored RFC3339 fraction width varies
// (see PruneJobs).
func (db *DB) ListExpiredJobs(ctx context.Context, cutoff time.Time) ([]JobRow, error) {
	rows, err := db.r.QueryContext(ctx,
		`SELECT `+jobColumns+` FROM lease_jobs WHERE state='exited' AND ended_at IS NOT NULL AND ended_at != ''`)
	if err != nil {
		return nil, fmt.Errorf("store: list expired jobs: %w", err)
	}
	defer rows.Close()
	all, err := scanJobs(rows)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, r := range all {
		if r.EndedAt.Before(cutoff) {
			out = append(out, r)
		}
	}
	return out, nil
}

// DeleteJobsOfLease removes a lease's job rows. The foreign key already
// cascades on lease deletion; this is for the explicit paths.
func (db *DB) DeleteJobsOfLease(ctx context.Context, leaseID string) error {
	if _, err := db.w.ExecContext(ctx, `DELETE FROM lease_jobs WHERE lease_id = ?`, leaseID); err != nil {
		return fmt.Errorf("store: delete jobs of %s: %w", leaseID, err)
	}
	return nil
}

// PruneLostJobs deletes lost jobs older than cutoff, returning how many
// rows went. Lost rows have no owner-facing cleanup left (their guest
// files went with the lost sandbox) and are invisible to the API's
// exited-job views, so unlike exited records nothing else ever removes
// them: without this they accumulate for ever (spoond-966 L3). A lost
// row with no ended_at (lost before markJobLost stamped it) ages from
// its started_at instead, so it cannot leak either. The comparison is in
// Go for the same RFC3339 fraction reason as PruneJobs.
func (db *DB) PruneLostJobs(ctx context.Context, cutoff time.Time) (int64, error) {
	rows, err := db.r.QueryContext(ctx,
		`SELECT `+jobColumns+` FROM lease_jobs WHERE state='lost'`)
	if err != nil {
		return 0, fmt.Errorf("store: list lost jobs: %w", err)
	}
	defer rows.Close()
	var ids []any
	for rows.Next() {
		r, err := scanJob(rows.Scan)
		if err != nil {
			return 0, err
		}
		agedAt := r.EndedAt
		if agedAt.IsZero() {
			agedAt = r.StartedAt
		}
		if agedAt.Before(cutoff) {
			ids = append(ids, r.JobID)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: list lost jobs: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	res, err := db.w.ExecContext(ctx,
		`DELETE FROM lease_jobs WHERE state='lost' AND job_id IN (`+placeholders+`)`, ids...)
	if err != nil {
		return 0, fmt.Errorf("store: prune lost jobs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune lost jobs: %w", err)
	}
	return n, nil
}

func scanJobs(rows *sql.Rows) ([]JobRow, error) {
	var out []JobRow
	for rows.Next() {
		r, err := scanJob(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func scanJob(scan func(dest ...any) error) (JobRow, error) {
	var r JobRow
	var exitCode sql.NullInt64
	var startedAt, endedAt string
	err := scan(&r.JobID, &r.LeaseID, &r.Owner, &r.Cmd, &r.Cwd, &r.State,
		&exitCode, &startedAt, &endedAt, &r.StderrTail, &r.Generation,
		&r.MaxRuntimeSecs, &r.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return JobRow{}, ErrNotFound
	}
	if err != nil {
		return JobRow{}, err
	}
	if exitCode.Valid {
		v := int(exitCode.Int64)
		r.ExitCode = &v
	}
	r.StartedAt = parseTime(startedAt)
	r.EndedAt = parseTime(endedAt)
	return r, nil
}
