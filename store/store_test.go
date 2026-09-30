package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func openTestDB(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

// TestOpenTwiceIdempotent verifies that opening an existing database
// re-runs the migrations without error.
func TestOpenTwiceIdempotent(t *testing.T) {
	_, path := openTestDB(t)
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	if err := db2.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestLeaseRoundTrip upserts, lists and reads back every LeaseRow
// field, including slices, booleans and zero (unset) times.
func TestLeaseRoundTrip(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()

	base := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	full := LeaseRow{
		ID: "lease-1", Owner: "alice", Image: "py-base",
		SandboxID: "sb-1", Address: "10.42.0.2:8888",
		CreatedAt: base, ExpiresAt: base.Add(time.Hour), LastActive: base.Add(time.Minute),
		Persistent: true, Suspended: true,
		Workspace: "ws-1", Name: "full", NetPolicy: "restricted",
		NetAllow:    []string{"10.0.0.1", "example.com"},
		ExposePorts: []int{8080, 9090},
		ExposedIP:   "10.42.0.9", Comment: "hello", State: "suspended",
		ResumeBuildID: "b-2", LastCheckpointBuildID: "b-1",
		LastCheckpointAt: base.Add(2 * time.Minute), RecoveredFrom: base,
	}
	// Zero times, nil slices and empty strings everywhere they can be.
	minimal := LeaseRow{
		ID: "lease-2", Owner: "bob", Image: "go-base",
		CreatedAt: base, ExpiresAt: base.Add(time.Hour), LastActive: base,
		State: "running",
	}

	for _, row := range []LeaseRow{full, minimal} {
		if err := db.UpsertLease(ctx, row); err != nil {
			t.Fatalf("upsert %s: %v", row.ID, err)
		}
	}

	// An upsert on an existing id updates every column.
	updated := full
	updated.Address = "10.42.0.3:8888"
	updated.Suspended = false
	updated.State = "running"
	updated.Name = "renamed"
	updated.Comment = ""
	updated.NetAllow = nil
	updated.ExposePorts = nil
	updated.ResumeBuildID = ""
	updated.LastCheckpointBuildID = ""
	updated.LastCheckpointAt = time.Time{}
	updated.RecoveredFrom = time.Time{}
	if err := db.UpsertLease(ctx, updated); err != nil {
		t.Fatalf("upsert update: %v", err)
	}

	rows, err := db.ListLeases(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string]LeaseRow{}
	for _, r := range rows {
		got[r.ID] = r
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 leases, got %d", len(got))
	}
	if !reflect.DeepEqual(got["lease-1"], updated) {
		t.Errorf("updated row mismatch:\n got: %+v\nwant: %+v", got["lease-1"], updated)
	}
	if !reflect.DeepEqual(got["lease-2"], minimal) {
		t.Errorf("minimal row mismatch:\n got: %+v\nwant: %+v", got["lease-2"], minimal)
	}
}

// TestDeleteLeaseCascadesShares verifies that deleting a lease removes
// its shares.
func TestDeleteLeaseCascadesShares(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

	if err := db.UpsertLease(ctx, LeaseRow{
		ID: "lease-1", Owner: "alice", Image: "py-base",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastActive: now,
		State: "running",
	}); err != nil {
		t.Fatalf("upsert lease: %v", err)
	}
	if err := db.UpsertShare(ctx, ShareRow{
		LeaseID: "lease-1", Grantee: "bob", Mode: "ssh", CreatedAt: now,
	}); err != nil {
		t.Fatalf("upsert share: %v", err)
	}
	shares, err := db.ListShares(ctx)
	if err != nil {
		t.Fatalf("list shares: %v", err)
	}
	if len(shares) != 1 {
		t.Fatalf("expected 1 share before delete, got %d", len(shares))
	}

	if err := db.DeleteLease(ctx, "lease-1"); err != nil {
		t.Fatalf("delete lease: %v", err)
	}
	shares, err = db.ListShares(ctx)
	if err != nil {
		t.Fatalf("list shares: %v", err)
	}
	if len(shares) != 0 {
		t.Fatalf("expected shares cascaded away, got %d", len(shares))
	}
	leases, err := db.ListLeases(ctx)
	if err != nil {
		t.Fatalf("list leases: %v", err)
	}
	if len(leases) != 0 {
		t.Fatalf("expected lease deleted, got %d", len(leases))
	}
}

// TestUniqueOwnerName verifies the partial unique index: two leases of
// one owner cannot share a non-empty name, but empty names never
// collide.
func TestUniqueOwnerName(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	mk := func(id, name string) LeaseRow {
		return LeaseRow{
			ID: id, Owner: "owner-1", Image: "py-base", Name: name,
			CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastActive: now,
			State: "running",
		}
	}

	if err := db.UpsertLease(ctx, mk("a", "dupe")); err != nil {
		t.Fatalf("upsert a: %v", err)
	}
	if err := db.UpsertLease(ctx, mk("b", "dupe")); err == nil {
		t.Fatal("expected duplicate (owner,name) to be rejected")
	}
	// The same name under a different owner is fine.
	other := mk("c", "dupe")
	other.Owner = "owner-2"
	if err := db.UpsertLease(ctx, other); err != nil {
		t.Fatalf("upsert c: %v", err)
	}
	// Empty names never collide.
	if err := db.UpsertLease(ctx, mk("d", "")); err != nil {
		t.Fatalf("upsert d: %v", err)
	}
	if err := db.UpsertLease(ctx, mk("e", "")); err != nil {
		t.Fatalf("upsert e: %v", err)
	}
}

// TestPoolOrdering verifies pool lists come back ordered by created_at.
func TestPoolOrdering(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()

	if err := db.AddPool(ctx, "sb-1", "py-base"); err != nil {
		t.Fatalf("add sb-1: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := db.AddPool(ctx, "sb-2", "py-base"); err != nil {
		t.Fatalf("add sb-2: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := db.AddPool(ctx, "sb-3", "py-base"); err != nil {
		t.Fatalf("add sb-3: %v", err)
	}
	if err := db.RemovePool(ctx, "sb-2"); err != nil {
		t.Fatalf("remove sb-2: %v", err)
	}

	pool, err := db.ListPool(ctx)
	if err != nil {
		t.Fatalf("list pool: %v", err)
	}
	want := []string{"sb-1", "sb-3"}
	if got := pool["py-base"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("pool order: got %v, want %v", got, want)
	}
}

// TestUpdateLastActive verifies the batched LastActive update.
func TestUpdateLastActive(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC)
	for _, id := range []string{"a", "b"} {
		if err := db.UpsertLease(ctx, LeaseRow{
			ID: id, Owner: "o", Image: "py-base",
			CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastActive: now,
			State: "running",
		}); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}

	t1 := now.Add(time.Minute)
	t2 := now.Add(2 * time.Minute)
	if err := db.UpdateLastActive(ctx, map[string]time.Time{"a": t1, "b": t2}); err != nil {
		t.Fatalf("update last active: %v", err)
	}
	rows, err := db.ListLeases(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string]time.Time{}
	for _, r := range rows {
		got[r.ID] = r.LastActive
	}
	if !got["a"].Equal(t1) || !got["b"].Equal(t2) {
		t.Fatalf("last_active not updated: a=%v b=%v", got["a"], got["b"])
	}
}
