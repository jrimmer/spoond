package store

import (
	"context"
	"testing"
	"time"
)

// TestUsageSampleRoundTrip inserts samples for two owners and reads one
// owner's history back oldest first with every field.
func TestUsageSampleRoundTrip(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 4, 3, 2, 1, 500000000, time.UTC)
	// Insert alice's samples out of order so the read must sort them.
	for _, s := range []UsageSampleRow{
		{Owner: "alice", At: base.Add(30 * time.Minute), MemoryUsedMiB: 300, DiskUsedBytes: 4000, Running: 2, Paused: 0},
		{Owner: "alice", At: base, MemoryUsedMiB: 100, DiskUsedBytes: 2000, Running: 1, Paused: 3},
		{Owner: "alice", At: base.Add(15 * time.Minute), MemoryUsedMiB: 200, DiskUsedBytes: 3000, Running: 2, Paused: 1},
		{Owner: "bob", At: base, MemoryUsedMiB: 900, DiskUsedBytes: 9000, Running: 9, Paused: 9},
	} {
		if err := db.InsertUsageSample(ctx, s); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	got, err := db.ListUsageHistory(ctx, "alice", time.Time{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d alice samples, want 3", len(got))
	}
	if !got[0].At.Equal(base) || !got[1].At.Equal(base.Add(15*time.Minute)) || !got[2].At.Equal(base.Add(30*time.Minute)) {
		t.Fatalf("samples not oldest first: %v", []time.Time{got[0].At, got[1].At, got[2].At})
	}
	if got[0].MemoryUsedMiB != 100 || got[0].DiskUsedBytes != 2000 || got[0].Running != 1 || got[0].Paused != 3 {
		t.Fatalf("first sample wrong: %+v", got[0])
	}
	if got[2].MemoryUsedMiB != 300 || got[2].DiskUsedBytes != 4000 || got[2].Running != 2 || got[2].Paused != 0 {
		t.Fatalf("last sample wrong: %+v", got[2])
	}

	// The since filter drops samples strictly before it, and the other
	// owner's samples never appear.
	got, err = db.ListUsageHistory(ctx, "alice", base.Add(15*time.Minute))
	if err != nil {
		t.Fatalf("list since: %v", err)
	}
	if len(got) != 2 || !got[0].At.Equal(base.Add(15*time.Minute)) {
		t.Fatalf("since filter wrong: %+v", got)
	}
	bob, err := db.ListUsageHistory(ctx, "bob", time.Time{})
	if err != nil {
		t.Fatalf("list bob: %v", err)
	}
	if len(bob) != 1 || bob[0].Owner != "bob" {
		t.Fatalf("bob history wrong: %+v", bob)
	}
}

// TestUsageSamplePrune deletes only the samples older than the cutoff,
// leaving the newer ones for both owners.
func TestUsageSamplePrune(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 4, 3, 2, 1, 0, time.UTC)
	for _, s := range []UsageSampleRow{
		{Owner: "alice", At: base.Add(-8 * 24 * time.Hour)},
		{Owner: "alice", At: base.Add(-24 * time.Hour)},
		{Owner: "alice", At: base},
		{Owner: "bob", At: base.Add(-7 * 24 * time.Hour).Add(-time.Minute)},
		{Owner: "bob", At: base},
	} {
		if err := db.InsertUsageSample(ctx, s); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	cutoff := base.Add(-7 * 24 * time.Hour)
	n, err := db.PruneUsageSamples(ctx, cutoff)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 2 {
		t.Fatalf("pruned %d, want 2", n)
	}
	alice, err := db.ListUsageHistory(ctx, "alice", time.Time{})
	if err != nil {
		t.Fatalf("list alice: %v", err)
	}
	if len(alice) != 2 {
		t.Fatalf("alice has %d samples after prune, want 2", len(alice))
	}
	bob, err := db.ListUsageHistory(ctx, "bob", time.Time{})
	if err != nil {
		t.Fatalf("list bob: %v", err)
	}
	if len(bob) != 1 || !bob[0].At.Equal(base) {
		t.Fatalf("bob history after prune wrong: %+v", bob)
	}
	// A second prune with the same cutoff is idempotent.
	if n, err := db.PruneUsageSamples(ctx, cutoff); err != nil || n != 0 {
		t.Fatalf("second prune = %d, %v; want 0, nil", n, err)
	}
}

// TestUsageSampleEmptyStore: an owner with no samples reads an empty
// history, not an error, and pruning an empty table is a no-op.
func TestUsageSampleEmptyStore(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	got, err := db.ListUsageHistory(ctx, "nobody", time.Time{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("history = %+v, want empty", got)
	}
	if n, err := db.PruneUsageSamples(ctx, time.Now()); err != nil || n != 0 {
		t.Fatalf("prune = %d, %v; want 0, nil", n, err)
	}
}
