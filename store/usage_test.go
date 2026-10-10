package store

import (
	"context"
	"testing"
	"time"
)

// TestUsageDiskByOwner lays down the rows the fair-share disk-usage
// helper reads: one lease with a resume (pause) build, one kept build and
// a named snapshot version. A build that is both kept and named counts
// once in Used (R5-5); the per-kind fields still report each kind.
func TestUsageDiskByOwner(t *testing.T) {
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
	// alice's named snapshot points at the very build kept-r1: it is a
	// kept checkpoint and a named snapshot at once, so it counts once in
	// Used (300, not 500).
	if _, err := db.InsertNamedSnapshot(ctx, NamedSnapshotRow{
		Owner: "alice", Name: "warm", BuildID: "kept-r1", SourceLeaseID: "p1",
		Image: "img", ImageBuildID: "img-1", MemoryMB: 1, SizeBytes: 200,
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

	usage, err := db.DiskUsageByOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := usage["alice"].Paused; got != 100 {
		t.Fatalf("alice paused = %d, want 100", got)
	}
	if got := usage["alice"].Kept; got != 200 {
		t.Fatalf("alice kept = %d, want 200", got)
	}
	if got := usage["alice"].Named; got != 200 {
		t.Fatalf("alice named = %d, want 200", got)
	}
	if got := usage["alice"].Used; got != 300 {
		t.Fatalf("alice used = %d, want 300 (kept-r1 counted once)", got)
	}
	if got := usage["bob"].Paused; got != 0 {
		t.Fatalf("bob paused = %d, want 0", got)
	}
	if got := usage["bob"].Kept; got != 50 {
		t.Fatalf("bob kept = %d, want 50", got)
	}
	if got := usage["bob"].Named; got != 40 {
		t.Fatalf("bob named = %d, want 40", got)
	}
	if got := usage["bob"].Used; got != 90 {
		t.Fatalf("bob used = %d, want 90", got)
	}
}

// TestUsageDiskByOwnerDedupsNamedBuild: a named snapshot whose build is
// also pinned by a lease counts once, even when the named row's
// recorded size differs from the build's (the build row is what
// spoond measured).
func TestUsageDiskByOwnerDedupsNamedBuild(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	now := time.Now()
	if err := db.UpsertLease(ctx, LeaseRow{ID: "l1", Owner: "alice", Image: "img",
		State: "running", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		LastActive: now, Class: "guaranteed"}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertBuild(ctx, BuildRow{BuildID: "shared", Kind: "checkpoint",
		Owner: "alice", State: "ready", SizeBytes: 500, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.KeepBuild(ctx, "l1", "shared", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertNamedSnapshot(ctx, NamedSnapshotRow{
		Owner: "alice", Name: "warm", BuildID: "shared", SourceLeaseID: "l1",
		Image: "img", ImageBuildID: "img-1", MemoryMB: 1, SizeBytes: 500,
		CreatedAt: now,
	}, 1); err != nil {
		t.Fatal(err)
	}
	usage, err := db.DiskUsageByOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := usage["alice"].Used; got != 500 {
		t.Fatalf("used = %d, want 500 (one build)", got)
	}
	if got := usage["alice"].Kept; got != 500 {
		t.Fatalf("kept = %d, want 500", got)
	}
	if got := usage["alice"].Named; got != 500 {
		t.Fatalf("named = %d, want 500", got)
	}
}

// TestUsageDiskEmptyStore: no rows means an empty map, not an error.
func TestUsageDiskEmptyStore(t *testing.T) {
	db, _ := openTestDB(t)
	got, err := db.DiskUsageByOwner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("usage = %v, want empty", got)
	}
}

// TestUsageAccountedSnapshotBytesCountsSharedBuildOnce: a build pinned by
// two owners' leases and also named once is one build on disk, so the
// box-level accounted bytes count it once while each owner's Used still
// reports it (FS1 follow-up b).
func TestUsageAccountedSnapshotBytesCountsSharedBuildOnce(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	now := time.Now()
	for _, id := range []string{"l-alice", "l-bob"} {
		owner := "alice"
		if id == "l-bob" {
			owner = "bob"
		}
		if err := db.UpsertLease(ctx, LeaseRow{ID: id, Owner: owner, Image: "img",
			State: "running", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
			LastActive: now, Class: "guaranteed"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.InsertBuild(ctx, BuildRow{BuildID: "shared", Kind: "checkpoint",
		Owner: "alice", State: "ready", SizeBytes: 700, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertBuild(ctx, BuildRow{BuildID: "solo", Kind: "checkpoint",
		Owner: "bob", State: "ready", SizeBytes: 300, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	// Two owners pin the same build, and alice also names it. The named
	// row records a stale size (500); the build row's 700 wins.
	for _, pin := range []struct{ lease, build string }{{"l-alice", "shared"}, {"l-bob", "shared"}, {"l-bob", "solo"}} {
		if err := db.KeepBuild(ctx, pin.lease, pin.build, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.InsertNamedSnapshot(ctx, NamedSnapshotRow{
		Owner: "alice", Name: "warm", BuildID: "shared", SourceLeaseID: "l-alice",
		Image: "img", ImageBuildID: "img-1", MemoryMB: 1, SizeBytes: 500,
		CreatedAt: now,
	}, 1); err != nil {
		t.Fatal(err)
	}

	usage, err := db.DiskUsageByOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The shared build is attributed to both owners, so summing Used
	// would double-count it: 700 (alice) + 1000 (bob).
	if got := usage["alice"].Kept; got != 700 {
		t.Fatalf("alice kept = %d, want 700", got)
	}
	if got := usage["bob"].Kept; got != 1000 {
		t.Fatalf("bob kept = %d, want 1000", got)
	}
	if got := usage["alice"].Named; got != 700 {
		t.Fatalf("alice named = %d, want 700 (build row wins over named row)", got)
	}

	accounted, err := db.AccountedSnapshotBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Distinct builds: shared (700) + solo (300) = 1000, not the
	// 1700 the per-owner sums would give.
	if accounted != 1000 {
		t.Fatalf("accounted = %d, want 1000 (one shared build once)", accounted)
	}
}

// TestUsageAccountedSnapshotBytesFallsBackToNamedSize: a named snapshot
// whose build row is gone still counts its own recorded size.
func TestUsageAccountedSnapshotBytesFallsBackToNamedSize(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	now := time.Now()
	if _, err := db.InsertNamedSnapshot(ctx, NamedSnapshotRow{
		Owner: "alice", Name: "warm", BuildID: "gone", SourceLeaseID: "l1",
		Image: "img", ImageBuildID: "img-1", MemoryMB: 1, SizeBytes: 42,
		CreatedAt: now,
	}, 1); err != nil {
		t.Fatal(err)
	}
	accounted, err := db.AccountedSnapshotBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if accounted != 42 {
		t.Fatalf("accounted = %d, want 42 (named size fallback)", accounted)
	}
}

// TestUsageAccountedSnapshotBytesEmpty: an empty store sums to 0.
func TestUsageAccountedSnapshotBytesEmpty(t *testing.T) {
	db, _ := openTestDB(t)
	got, err := db.AccountedSnapshotBytes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("accounted = %d, want 0", got)
	}
}
