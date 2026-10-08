package store

import (
	"context"
	"testing"
	"time"
)

// seedOwnerLease inserts a running lease for owner.
func seedOwnerLease(t *testing.T, db *DB, id, owner string) {
	t.Helper()
	if err := db.UpsertLease(context.Background(), LeaseRow{
		ID: id, Owner: owner, Image: "py-base", State: "running", Class: "guaranteed",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), LastActive: time.Now(),
	}); err != nil {
		t.Fatalf("seed lease %s: %v", id, err)
	}
}

// TestKeptBuildsOfOwnerAndDelete: the per-owner kept-build listing sees
// only that owner's pins and the owner delete drops them all, naming
// what it unpinned.
func TestKeptBuildsOfOwnerAndDelete(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedOwnerLease(t, db, "l-alice", "alice")
	seedOwnerLease(t, db, "l-bob", "bob")
	for _, k := range []struct{ lease, build string }{
		{"l-alice", "b-a1"}, {"l-alice", "b-a2"}, {"l-bob", "b-b1"},
	} {
		if err := db.KeepBuild(ctx, k.lease, k.build, time.Now()); err != nil {
			t.Fatalf("keep %s: %v", k.build, err)
		}
	}

	list, err := db.ListKeptBuildsOfOwner(ctx, "alice")
	if err != nil {
		t.Fatalf("list alice: %v", err)
	}
	if len(list) != 2 || list[0] != "b-a1" || list[1] != "b-a2" {
		t.Fatalf("alice kept = %v, want [b-a1 b-a2]", list)
	}

	removed, err := db.DeleteKeptBuildsOfOwner(ctx, "alice")
	if err != nil {
		t.Fatalf("delete alice: %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed = %v, want two", removed)
	}
	if got, _ := db.ListKeptBuildsOfOwner(ctx, "alice"); len(got) != 0 {
		t.Fatalf("alice pins survived: %v", got)
	}
	if got, _ := db.ListKeptBuildsOfOwner(ctx, "bob"); len(got) != 1 {
		t.Fatalf("bob pins crossed the owner delete: %v", got)
	}
}

// TestNamedSnapshotsOfOwnerDelete: the owner delete removes every
// version and the name settings row, returning the dropped versions,
// and leaves another owner's snapshots alone.
func TestNamedSnapshotsOfOwnerDelete(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if _, err := db.InsertNamedSnapshot(ctx, namedRow("alice", "warm", "b-a1"), 3); err != nil {
		t.Fatalf("insert alice warm: %v", err)
	}
	if _, err := db.InsertNamedSnapshot(ctx, namedRow("alice", "warm", "b-a2"), 3); err != nil {
		t.Fatalf("insert alice warm 2: %v", err)
	}
	if _, err := db.InsertNamedSnapshot(ctx, namedRow("alice", "cold", "b-a3"), 3); err != nil {
		t.Fatalf("insert alice cold: %v", err)
	}
	if _, err := db.InsertNamedSnapshot(ctx, namedRow("bob", "warm", "b-b1"), 3); err != nil {
		t.Fatalf("insert bob: %v", err)
	}

	rows, err := db.DeleteNamedSnapshotsOfOwner(ctx, "alice")
	if err != nil {
		t.Fatalf("delete alice: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("dropped = %d, want 3", len(rows))
	}
	if got, _ := db.ListNamedSnapshots(ctx, "alice", ""); len(got) != 0 {
		t.Fatalf("alice snapshots survived: %v", got)
	}
	if _, err := db.NamedSnapshotKeep(ctx, "alice", "warm"); err == nil {
		t.Fatal("alice settings row survived the owner delete")
	}
	if got, _ := db.ListNamedSnapshots(ctx, "bob", ""); len(got) != 1 {
		t.Fatalf("bob snapshots crossed the owner delete: %v", got)
	}
}

// TestRunningJobsOfOwner: the per-owner running-job listing sees one
// owner's running jobs only.
func TestRunningJobsOfOwner(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	seedOwnerLease(t, db, "l-alice", "alice")
	seedOwnerLease(t, db, "l-bob", "bob")
	base := time.Now()
	for _, j := range []JobRow{
		{JobID: "j-a1", LeaseID: "l-alice", Owner: "alice", Cmd: "a", State: "running", StartedAt: base},
		{JobID: "j-a2", LeaseID: "l-alice", Owner: "alice", Cmd: "b", State: "exited", StartedAt: base.Add(time.Second)},
		{JobID: "j-b1", LeaseID: "l-bob", Owner: "bob", Cmd: "c", State: "running", StartedAt: base},
	} {
		if err := db.InsertJob(ctx, j); err != nil {
			t.Fatalf("insert %s: %v", j.JobID, err)
		}
	}
	rows, err := db.ListRunningJobsOfOwner(ctx, "alice")
	if err != nil {
		t.Fatalf("list alice: %v", err)
	}
	if len(rows) != 1 || rows[0].JobID != "j-a1" {
		t.Fatalf("alice running = %+v, want just j-a1", rows)
	}
}
