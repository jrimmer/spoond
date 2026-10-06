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

	// Pruning removes only exited rows older than the cutoff; running and
	// lost stay.
	insert("j-run", "l-1", "running", base.Add(7*time.Minute))
	insert("j-lost", "l-1", "lost", base.Add(7*time.Minute))
	if _, err := db.PruneJobs(ctx, base.Add(6*time.Minute)); err != nil {
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

	expired, err := db.ListExpiredJobs(ctx, base)
	if err != nil {
		t.Fatalf("list expired: %v", err)
	}
	if len(expired) != 1 || expired[0].JobID != "j-earlier" {
		t.Fatalf("expired = %+v, want just j-earlier", expired)
	}
	if _, err := db.PruneJobs(ctx, base); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := db.GetJob(ctx, "j-earlier"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("j-earlier survived prune: %v", err)
	}
	if _, err := db.GetJob(ctx, "j-later"); err != nil {
		t.Fatalf("j-later wrongly pruned: %v", err)
	}
}
