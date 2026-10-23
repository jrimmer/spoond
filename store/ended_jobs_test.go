package store

import (
	"context"
	"testing"
	"time"
)

// TestArchiveJobsOfLease: the release's copy of a lease's jobs outlives
// the lease row. A running job becomes lost with the reason and the
// tail given; an exited one keeps its exit and tail; a second archive
// keeps the first copy; the prune honours each row's window.
func TestArchiveJobsOfLease(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if err := db.UpsertLease(ctx, LeaseRow{
		ID: "l-1", Owner: "alice", Image: "py-base",
		CreatedAt: base, ExpiresAt: base.Add(time.Hour), LastActive: base,
		State: "running", Class: "guaranteed",
	}); err != nil {
		t.Fatal(err)
	}
	for _, j := range []JobRow{
		{JobID: "j-run", LeaseID: "l-1", Owner: "alice", Cmd: "worker", State: "running", StartedAt: base},
		{JobID: "j-done", LeaseID: "l-1", Owner: "alice", Cmd: "make", State: "running", StartedAt: base.Add(time.Minute), RetentionSecs: 60},
	} {
		if err := db.InsertJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.UpdateJobExit(ctx, "j-done", 2, base.Add(2*time.Minute), "make: error"); err != nil {
		t.Fatal(err)
	}
	end := base.Add(3 * time.Minute)
	n, err := db.ArchiveJobsOfLease(ctx, "l-1", "alice", end, JobReasonLeaseReleased, map[string]string{"j-run": "boom", "j-done": "ignored"})
	if err != nil || n != 2 {
		t.Fatalf("archive: %d, %v", n, err)
	}
	if err := db.DeleteLease(ctx, "l-1"); err != nil {
		t.Fatal(err)
	}
	if n, err := db.ArchiveJobsOfLease(ctx, "l-1", "alice", end, JobReasonLeaseReleased, nil); err != nil || n != 0 {
		t.Fatalf("archive after the lease went: %d, %v", n, err)
	}
	rows, owner, err := db.ListEndedJobs(ctx, "l-1")
	if err != nil || owner != "alice" || len(rows) != 2 {
		t.Fatalf("list: %v owner %q err %v", rows, owner, err)
	}
	done, run := rows[0], rows[1]
	if done.JobID != "j-done" || done.State != "exited" || done.ExitCode == nil || *done.ExitCode != 2 || done.StderrTail != "make: error" {
		t.Fatalf("exited job archived as %+v", done)
	}
	if run.State != "lost" || run.Reason != JobReasonLeaseReleased || !run.EndedAt.Equal(end) || run.StderrTail != "boom" || run.ExitCode != nil {
		t.Fatalf("running job archived as %+v", run)
	}

	// j-done asked for 60 s, j-run takes the host's hour.
	if n, err := db.PruneEndedJobs(ctx, end.Add(30*time.Minute), time.Hour); err != nil || n != 1 {
		t.Fatalf("prune at +30m: %d, %v", n, err)
	}
	if n, err := db.PruneEndedJobs(ctx, end.Add(2*time.Hour), time.Hour); err != nil || n != 1 {
		t.Fatalf("prune at +2h: %d, %v", n, err)
	}
	if rows, owner, err := db.ListEndedJobs(ctx, "l-1"); err != nil || len(rows) != 0 || owner != "" {
		t.Fatalf("after prune: %v %q %v", rows, owner, err)
	}
}
