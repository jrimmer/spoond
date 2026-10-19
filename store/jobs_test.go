package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestJobRoundTripAndTieBreak covers the lease_jobs store surface (2.6,
// #135): insert/get/list, running counts, exit updates, the newest-exit
// view with exact started_at ties, pruning and lease-deletion cascade.
func TestJobRoundTripAndTieBreak(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"l-1", "l-2"} {
		if err := db.UpsertLease(ctx, LeaseRow{
			ID: id, Owner: "alice", Image: "py-base",
			CreatedAt: base, ExpiresAt: base.Add(time.Hour), LastActive: base,
			State: "running", Class: "guaranteed",
		}); err != nil {
			t.Fatalf("upsert lease %s: %v", id, err)
		}
	}
	insert := func(id, lease, state string, started time.Time) {
		t.Helper()
		if err := db.InsertJob(ctx, JobRow{
			JobID: id, LeaseID: lease, Owner: "alice", Cmd: "echo " + id,
			State: state, StartedAt: started, Generation: 3,
			MaxRuntimeSecs: 86400,
		}); err != nil {
			t.Fatalf("insert job %s: %v", id, err)
		}
	}
	insert("j-old", "l-1", "running", base)
	insert("j-new", "l-1", "running", base.Add(time.Minute))
	insert("j-other", "l-2", "running", base.Add(2*time.Minute))

	rows, err := db.ListJobs(ctx, "l-1")
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(rows) != 2 || rows[0].JobID != "j-new" || rows[1].JobID != "j-old" {
		t.Fatalf("list jobs = %+v, want newest first", rows)
	}
	if rows[0].MaxRuntimeSecs != 86400 || rows[0].Reason != "" {
		t.Fatalf("round-tripped job = max_runtime_secs %d reason %q", rows[0].MaxRuntimeSecs, rows[0].Reason)
	}
	if n, err := db.CountRunningJobs(ctx, "l-1"); err != nil || n != 2 {
		t.Fatalf("count l-1 = %d (%v), want 2", n, err)
	}
	byLease, err := db.CountRunningJobsByLease(ctx)
	if err != nil || byLease["l-1"] != 2 || byLease["l-2"] != 1 {
		t.Fatalf("counts by lease = %v (%v)", byLease, err)
	}

	// Finish both l-1 jobs. The two share an exact started_at so the
	// tie-break by job_id decides: "j-tie-b" is the newest.
	if _, err := db.UpdateJobExit(ctx, "j-old", 0, base.Add(5*time.Minute), ""); err != nil {
		t.Fatalf("exit j-old: %v", err)
	}
	insert("j-tie-a", "l-2", "exited", base.Add(3*time.Minute))
	insert("j-tie-b", "l-2", "exited", base.Add(3*time.Minute))
	latest, err := db.LatestJobExit(ctx)
	if err != nil {
		t.Fatalf("latest exits: %v", err)
	}
	if got := latest["l-1"].JobID; got != "j-old" {
		t.Fatalf("latest l-1 = %q, want j-old", got)
	}
	if got := latest["l-2"].JobID; got != "j-tie-b" {
		t.Fatalf("latest l-2 = %q, want j-tie-b (job_id tie-break)", got)
	}
	if row, ok, err := db.LatestJobExitOfLease(ctx, "l-2"); err != nil || !ok || row.JobID != "j-tie-b" {
		t.Fatalf("LatestJobExitOfLease l-2 = %+v %v %v", row, ok, err)
	}

	// A first outcome wins: an exited job cannot be re-marked.
	if changed, err := db.UpdateJobExit(ctx, "j-old", 9, base, ""); err != nil || changed {
		t.Fatalf("second exit = %v (%v), want no change", changed, err)
	}
	if changed, err := db.MarkJobLost(ctx, "j-old", base.Add(6*time.Minute)); err != nil || changed {
		t.Fatalf("lost after exit = %v (%v), want no change", changed, err)
	}
	if changed, err := db.MarkJobTimedOut(ctx, "j-old", 124, base.Add(6*time.Minute), ""); err != nil || changed {
		t.Fatalf("timed out after exit = %v (%v), want no change", changed, err)
	}

	// MarkJobTimedOut is the max-runtime path: it stamps the reason and
	// the exit code, and is idempotent like the other outcomes.
	insert("j-timeout", "l-1", "running", base.Add(8*time.Minute))
	if changed, err := db.MarkJobTimedOut(ctx, "j-timeout", 124, base.Add(9*time.Minute), "bye"); err != nil || !changed {
		t.Fatalf("mark timed out = %v (%v), want change", changed, err)
	}
	timedOut, err := db.GetJob(ctx, "j-timeout")
	if err != nil {
		t.Fatalf("get timed-out job: %v", err)
	}
	if timedOut.State != "exited" || timedOut.Reason != "timed_out" || timedOut.ExitCode == nil || *timedOut.ExitCode != 124 {
		t.Fatalf("timed-out job = %+v", timedOut)
	}
	if changed, err := db.MarkJobTimedOut(ctx, "j-timeout", 124, base, "again"); err != nil || changed {
		t.Fatalf("second timed out = %v (%v), want no change", changed, err)
	}

	// Pruning removes only exited rows past their window; running and
	// lost stay.
	insert("j-run", "l-1", "running", base.Add(7*time.Minute))
	insert("j-lost", "l-1", "lost", base.Add(7*time.Minute))
	if _, err := db.PruneJobs(ctx, base.Add(6*time.Minute), 0); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := db.GetJob(ctx, "j-old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old exited job survived prune: %v", err)
	}
	for _, id := range []string{"j-run", "j-lost"} {
		if _, err := db.GetJob(ctx, id); err != nil {
			t.Fatalf("%s pruned: %v", id, err)
		}
	}

	// Deleting the lease cascades its jobs away.
	if err := db.DeleteLease(ctx, "l-1"); err != nil {
		t.Fatalf("delete lease: %v", err)
	}
	if _, err := db.GetJob(ctx, "j-run"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("job survived lease deletion: %v", err)
	}
}

// TestJobRetentionFractionBoundary pins that the retention comparison
// orders ended_at by parsed time, not by the stored RFC3339 string. A
// value with a fractional second that is later than the cutoff sorts
// before it as a string ("12:00:00.5Z" < "12:00:00Z") and a string
// comparison would wrongly prune it; the parsed comparison keeps it.
func TestJobRetentionFractionBoundary(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if err := db.UpsertLease(ctx, LeaseRow{
		ID: "l-1", Owner: "alice", Image: "py-base",
		CreatedAt: base, ExpiresAt: base.Add(time.Hour), LastActive: base,
		State: "running", Class: "guaranteed",
	}); err != nil {
		t.Fatalf("upsert lease: %v", err)
	}
	insert := func(id string, ended time.Time) {
		t.Helper()
		if err := db.InsertJob(ctx, JobRow{
			JobID: id, LeaseID: "l-1", Owner: "alice", Cmd: "echo " + id,
			State: "exited", StartedAt: ended.Add(-time.Minute), EndedAt: ended,
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	// One ends 500 ms before the cutoff, one 500 ms after. A string
	// comparison mis-orders the later one ("12:00:00.5" sorts before
	// "12:00:00Z") and would prune it; the parsed comparison keeps it.
	insert("j-earlier", base.Add(-500*time.Millisecond))
	insert("j-later", base.Add(500*time.Millisecond))

	// now = end + 1 s and a 1 s host window put the boundary exactly at
	// the cutoff the old string comparison mis-ordered around.
	exp, err := db.ListExpiredJobs(ctx, base.Add(time.Second), time.Second)
	if err != nil {
		t.Fatalf("list expired: %v", err)
	}
	if len(exp) != 1 || exp[0].JobID != "j-earlier" {
		t.Fatalf("expired = %+v, want just j-earlier", exp)
	}
	if _, err := db.PruneJobs(ctx, base.Add(time.Second), time.Second); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := db.GetJob(ctx, "j-earlier"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("j-earlier survived prune: %v", err)
	}
	if _, err := db.GetJob(ctx, "j-later"); err != nil {
		t.Fatalf("j-later wrongly pruned: %v", err)
	}
}

// TestPruneLostJobs covers the L3 lost-row sweep: only lost rows whose
// ended_at is before the cutoff go; exited and running rows stay.
func TestPruneLostJobs(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if err := db.UpsertLease(ctx, LeaseRow{
		ID: "l-1", Owner: "alice", Image: "py-base",
		CreatedAt: base, ExpiresAt: base.Add(time.Hour), LastActive: base,
		State: "running", Class: "guaranteed",
	}); err != nil {
		t.Fatalf("upsert lease: %v", err)
	}
	insert := func(id, state string, ended time.Time) {
		t.Helper()
		if err := db.InsertJob(ctx, JobRow{
			JobID: id, LeaseID: "l-1", Owner: "alice", Cmd: "echo " + id,
			State: state, StartedAt: ended.Add(-time.Minute), EndedAt: ended,
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	insert("j-lost-old", "lost", base)
	insert("j-lost-recent", "lost", base.Add(48*time.Hour))
	insert("j-exited-old", "exited", base)
	insert("j-run", "running", time.Time{})
	// A lost row with no ended_at (lost before markJobLost stamped it)
	// ages from its started_at.
	if err := db.InsertJob(ctx, JobRow{
		JobID: "j-lost-noend", LeaseID: "l-1", Owner: "alice", Cmd: "echo x",
		State: "lost", StartedAt: base,
	}); err != nil {
		t.Fatalf("insert j-lost-noend: %v", err)
	}

	n, err := db.PruneLostJobs(ctx, base.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 2 {
		t.Fatalf("pruned = %d, want 2", n)
	}
	for _, id := range []string{"j-lost-old", "j-lost-noend"} {
		if _, err := db.GetJob(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s survived: %v", id, err)
		}
	}
	for _, id := range []string{"j-lost-recent", "j-exited-old", "j-run"} {
		if _, err := db.GetJob(ctx, id); err != nil {
			t.Fatalf("%s wrongly pruned: %v", id, err)
		}
	}
}

// TestJobPerJobRetention (spoond-hwto): an exited row recorded with a
// retention_secs shorter than the host window is pruned by its own
// window; a row still inside its own window survives a pass that prunes
// host-window rows; a 0 row (a record written before the column
// existed) falls back to the host window. PruneJobs reports the rows it
// deleted, and DeleteJob removes exactly one row.
func TestJobPerJobRetention(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if err := db.UpsertLease(ctx, LeaseRow{
		ID: "l-1", Owner: "alice", Image: "py-base",
		CreatedAt: base, ExpiresAt: base.Add(time.Hour), LastActive: base,
		State: "running", Class: "guaranteed",
	}); err != nil {
		t.Fatalf("upsert lease: %v", err)
	}
	insert := func(id string, retention int64, ended time.Time) {
		t.Helper()
		if err := db.InsertJob(ctx, JobRow{
			JobID: id, LeaseID: "l-1", Owner: "alice", Cmd: "echo " + id,
			State: "exited", StartedAt: ended.Add(-time.Minute), EndedAt: ended,
			RetentionSecs: retention,
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	// j-short ended an hour ago with a 60 s retention; j-host the same
	// age with none (0: the host window applies); j-fresh with a 60 s
	// retention ended a minute ago — its own window has not run out.
	ended := base.Add(-time.Hour)
	insert("j-short", 60, ended)
	insert("j-host", 0, ended)
	insert("j-fresh", 60, base.Add(-time.Minute))

	expired, err := db.ListExpiredJobs(ctx, base, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("list expired: %v", err)
	}
	if len(expired) != 1 || expired[0].JobID != "j-short" {
		t.Fatalf("expired = %+v, want just j-short", expired)
	}
	n, err := db.PruneJobs(ctx, base, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned = %d, want 1", n)
	}
	if _, err := db.GetJob(ctx, "j-short"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("j-short survived its own 60 s window: %v", err)
	}
	for _, id := range []string{"j-host", "j-fresh"} {
		if _, err := db.GetJob(ctx, id); err != nil {
			t.Fatalf("%s wrongly pruned: %v", id, err)
		}
	}

	// A later pass with the service clock moved the host window's 7 days
	// ahead prunes j-host; j-fresh's own window has long run out by then.
	if _, err := db.PruneJobs(ctx, base.Add(8*24*time.Hour), 7*24*time.Hour); err != nil {
		t.Fatalf("second prune: %v", err)
	}
	if _, err := db.GetJob(ctx, "j-host"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("j-host survived the host window: %v", err)
	}
	if _, err := db.GetJob(ctx, "j-fresh"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("j-fresh survived its own window: %v", err)
	}

	// DeleteJob removes exactly one row, and reports unknown as no
	// change.
	insert("j-del", 0, base)
	if changed, err := db.DeleteJob(ctx, "j-del"); err != nil || !changed {
		t.Fatalf("DeleteJob = %v (%v), want change", changed, err)
	}
	if _, err := db.GetJob(ctx, "j-del"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("j-del survived DeleteJob: %v", err)
	}
	if changed, err := db.DeleteJob(ctx, "j-del"); err != nil || changed {
		t.Fatalf("DeleteJob of a gone job = %v (%v), want no change", changed, err)
	}
}
