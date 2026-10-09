package store

import (
	"context"
	"testing"
	"time"
)

// seedUsageBuilds lays down the rows the fair-share usage helpers read:
// one lease with a resume (pause) build, one kept build and a named
// snapshot version.
func TestUsageBytesByOwner(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	now := time.Now()

	// Two leases for alice: a suspended one resuming from pause-p1 and a
	// running one; one lease for bob.
	for _, l := range []LeaseRow{
		{ID: "p1", Owner: "alice", Image: "img", State: "suspended",
			CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastActive: now,
			ResumeBuildID: "pause-p1", Class: "guaranteed"},
		{ID: "r1", Owner: "alice", Image: "img", State: "running",
			CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastActive: now, Class: "guaranteed"},
		{ID: "b1", Owner: "bob", Image: "img", State: "running",
			CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastActive: now, Class: "guaranteed"},
	} {
		if err := db.UpsertLease(ctx, l); err != nil {
			t.Fatalf("upsert lease %s: %v", l.ID, err)
		}
	}
	for _, b := range []BuildRow{
		{BuildID: "pause-p1", Kind: "pause", Owner: "alice", State: "ready", SizeBytes: 100, CreatedAt: now, UpdatedAt: now},
		{BuildID: "kept-r1", Kind: "checkpoint", Owner: "alice", State: "ready", SizeBytes: 200, CreatedAt: now, UpdatedAt: now},
		{BuildID: "deleted", Kind: "pause", Owner: "alice", State: "deleted", SizeBytes: 999, CreatedAt: now, UpdatedAt: now},
		{BuildID: "kept-b1", Kind: "checkpoint", Owner: "bob", State: "ready", SizeBytes: 50, CreatedAt: now, UpdatedAt: now},
	} {
		if err := db.InsertBuild(ctx, b); err != nil {
			t.Fatalf("insert build %s: %v", b.BuildID, err)
		}
	}
	if err := db.KeepBuild(ctx, "r1", "kept-r1", now); err != nil {
		t.Fatal(err)
	}
	if err := db.KeepBuild(ctx, "b1", "kept-b1", now); err != nil {
		t.Fatal(err)
	}
	// A pin on the deleted build must not count its bytes.
	if err := db.KeepBuild(ctx, "r1", "deleted", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertNamedSnapshot(ctx, NamedSnapshotRow{
		Owner: "alice", Name: "warm", BuildID: "named-a", SourceLeaseID: "p1",
		Image: "img", ImageBuildID: "img-1", MemoryMB: 1, SizeBytes: 300,
		CreatedAt: now,
	}, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertNamedSnapshot(ctx, NamedSnapshotRow{
		Owner: "bob", Name: "hot", BuildID: "named-b", SourceLeaseID: "b1",
		Image: "img", ImageBuildID: "img-1", MemoryMB: 1, SizeBytes: 40,
		CreatedAt: now,
	}, 1); err != nil {
		t.Fatal(err)
	}

	paused, err := db.PausedBytesByOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if paused["alice"] != 100 || paused["bob"] != 0 {
		t.Fatalf("paused = %v, want alice=100 bob=0", paused)
	}

	kept, err := db.KeptBytesByOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if kept["alice"] != 200 || kept["bob"] != 50 {
		t.Fatalf("kept = %v, want alice=200 bob=50", kept)
	}

	named, err := db.NamedSnapshotBytesByOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if named["alice"] != 300 || named["bob"] != 40 {
		t.Fatalf("named = %v, want alice=300 bob=40", named)
	}
}

// TestUsageBytesEmptyStore: no rows means empty maps, not an error.
func TestUsageBytesEmptyStore(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	for name, fn := range map[string]func(context.Context) (map[string]int64, error){
		"paused": db.PausedBytesByOwner,
		"kept":   db.KeptBytesByOwner,
		"named":  db.NamedSnapshotBytesByOwner,
	} {
		got, err := fn(ctx)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != 0 {
			t.Fatalf("%s = %v, want empty", name, got)
		}
	}
}
