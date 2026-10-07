package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// seedNamedLease inserts a lease row so the live-lease retention rule has
// something to read.
func seedNamedLease(t *testing.T, db *DB, id, buildID, state string) {
	t.Helper()
	if err := db.UpsertLease(context.Background(), LeaseRow{
		ID: id, Owner: "alice", Image: "py-base", State: state, Class: "guaranteed",
		SnapshotBuildID: buildID,
		CreatedAt:       time.Now(), ExpiresAt: time.Now().Add(time.Hour), LastActive: time.Now(),
	}); err != nil {
		t.Fatalf("seed lease: %v", err)
	}
}

// row builds a NamedSnapshotRow with only the fields these tests read.
func namedRow(owner, name, build string) NamedSnapshotRow {
	return NamedSnapshotRow{
		Owner: owner, Name: name, BuildID: build, SourceLeaseID: "lease",
		Image: "py-base", ImageBuildID: "img-build", MemoryMB: 1024, SizeBytes: 10,
		CreatedAt: time.Now(),
	}
}

// TestNamedSnapshotVersions: versions increment per (owner, name) and the
// first-save keep is stored; later keeps are ignored.
func TestNamedSnapshotVersions(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()

	a1, err := db.InsertNamedSnapshot(ctx, namedRow("alice", "warm", "b1"), 2)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	a2, _ := db.InsertNamedSnapshot(ctx, namedRow("alice", "warm", "b2"), 1)
	if a1.Version != 1 || a2.Version != 2 {
		t.Fatalf("versions = %d, %d, want 1, 2", a1.Version, a2.Version)
	}
	// A different owner starts at 1.
	b1, _ := db.InsertNamedSnapshot(ctx, namedRow("bob", "warm", "b3"), 3)
	if b1.Version != 1 {
		t.Fatalf("bob version = %d, want 1", b1.Version)
	}
	// A different name starts at 1.
	c1, _ := db.InsertNamedSnapshot(ctx, namedRow("alice", "cold", "b4"), 3)
	if c1.Version != 1 {
		t.Fatalf("cold version = %d, want 1", c1.Version)
	}
	// The first save's keep is stored; the second's is ignored.
	keep, err := db.NamedSnapshotKeep(ctx, "alice", "warm")
	if err != nil || keep != 2 {
		t.Fatalf("keep = %d (%v), want 2 (first save's keep)", keep, err)
	}
	latest, _ := db.GetNamedSnapshotLatest(ctx, "alice", "warm")
	if latest.BuildID != "b2" {
		t.Fatalf("latest = %s, want b2", latest.BuildID)
	}
}

// TestNamedSnapshotKeyScoping: a key is scoped by (owner, name), never by
// lease: the same key under a different name is a different save.
func TestNamedSnapshotKeyScoping(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	r := namedRow("alice", "warm", "b1")
	r.IdempotencyKey = "k1"
	if _, err := db.InsertNamedSnapshot(ctx, r, 0); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := db.GetNamedSnapshotByKey(ctx, "alice", "warm", "k1")
	if err != nil || got.BuildID != "b1" {
		t.Fatalf("by key = %+v (%v)", got, err)
	}
	if _, err := db.GetNamedSnapshotByKey(ctx, "alice", "cold", "k1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("same key other name = %v, want not found", err)
	}
	if _, err := db.GetNamedSnapshotByKey(ctx, "bob", "warm", "k1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("same key other owner = %v, want not found", err)
	}
	// The unique index rejects a second row with the same key under the
	// same (owner, name).
	r2 := namedRow("alice", "warm", "b2")
	r2.IdempotencyKey = "k1"
	if _, err := db.InsertNamedSnapshot(ctx, r2, 0); err == nil {
		t.Fatal("duplicate (owner, name, key) insert succeeded; want unique-index error")
	}
}

// TestNamedSnapshotPruneLiveLease: prune drops versions beyond keep
// unless a live lease runs from the build; a lost lease does not protect.
func TestNamedSnapshotPruneLiveLease(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	for _, b := range []string{"b1", "b2", "b3"} {
		if _, err := db.InsertNamedSnapshot(ctx, namedRow("alice", "warm", b), 3); err != nil {
			t.Fatalf("insert %s: %v", b, err)
		}
	}
	// b1 is in use by a running lease: it survives keep=2.
	seedNamedLease(t, db, "l1", "b1", "running")
	deleted, err := db.PruneNamedSnapshots(ctx, "alice", "warm", 2)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(deleted) != 0 {
		t.Fatalf("deleted = %v, want none (b1 is live)", deleted)
	}
	if _, err := db.GetNamedSnapshot(ctx, "alice", "warm", 1); err != nil {
		t.Fatalf("live v1 dropped: %v", err)
	}
	// Release the lease (lost): the next prune drops b1.
	seedNamedLease(t, db, "l1", "b1", "lost")
	deleted, _ = db.PruneNamedSnapshots(ctx, "alice", "warm", 2)
	if len(deleted) != 1 || deleted[0] != "b1" {
		t.Fatalf("deleted = %v, want [b1]", deleted)
	}
}

// TestNamedSnapshotDeleteNameSettings: deleting a name's last version
// drops its settings row.
func TestNamedSnapshotDeleteNameSettings(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if _, err := db.InsertNamedSnapshot(ctx, namedRow("alice", "warm", "b1"), 5); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := db.DeleteNamedSnapshot(ctx, "alice", "warm", 1); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.NamedSnapshotKeep(ctx, "alice", "warm"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("settings row survived the last version: %v", err)
	}
}

// TestNamedSnapshotListPrefix: the prefix filters by name, escaping LIKE
// wildcards.
func TestNamedSnapshotListPrefix(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	for _, n := range []string{"spoond/warm", "spoond/cold", "other", "under_score"} {
		if _, err := db.InsertNamedSnapshot(ctx, namedRow("alice", n, "b-"+n), 0); err != nil {
			t.Fatalf("insert %s: %v", n, err)
		}
	}
	rows, err := db.ListNamedSnapshots(ctx, "alice", "spoond/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("prefix rows = %d, want 2", len(rows))
	}
	// A literal underscore must not match any single character.
	rows, _ = db.ListNamedSnapshots(ctx, "alice", "under_")
	if len(rows) != 1 {
		t.Fatalf("underscore prefix rows = %d, want 1", len(rows))
	}
}

// TestNamedSnapshotVersionNeverReused: deleting the latest version and
// saving again gives latest+1, not the deleted number, via the
// named_snapshot_names last_version high-water mark (S4).
func TestNamedSnapshotVersionNeverReused(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	for _, b := range []string{"b1", "b2"} {
		if _, err := db.InsertNamedSnapshot(ctx, namedRow("alice", "warm", b), 3); err != nil {
			t.Fatalf("insert %s: %v", b, err)
		}
	}
	// Delete the latest version, then save again: the next version is 3.
	if err := db.DeleteNamedSnapshot(ctx, "alice", "warm", 2); err != nil {
		t.Fatalf("delete v2: %v", err)
	}
	r3, err := db.InsertNamedSnapshot(ctx, namedRow("alice", "warm", "b3"), 0)
	if err != nil {
		t.Fatalf("insert after delete: %v", err)
	}
	if r3.Version != 3 {
		t.Fatalf("version after deleting v2 = %d, want 3", r3.Version)
	}
}

// TestMarkUnnamedCheckpointsFailed: a building checkpoint with no named
// row is marked failed; a named one is left alone.
func TestMarkUnnamedCheckpointsFailed(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	now := time.Now()
	for _, b := range []string{"stranded", "named"} {
		if err := db.InsertBuild(ctx, BuildRow{
			BuildID: b, Kind: "checkpoint", TemplateID: "t", Image: "py-base",
			State: "building", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("insert build %s: %v", b, err)
		}
	}
	if _, err := db.InsertNamedSnapshot(ctx, namedRow("alice", "warm", "named"), 0); err != nil {
		t.Fatalf("insert named: %v", err)
	}
	n, err := db.MarkUnnamedCheckpointsFailed(ctx)
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if n != 1 {
		t.Fatalf("marked %d, want 1", n)
	}
	got, _ := db.GetBuild(ctx, "stranded")
	if got.State != "failed" {
		t.Fatalf("stranded state = %q, want failed", got.State)
	}
	named, _ := db.GetBuild(ctx, "named")
	if named.State != "building" {
		t.Fatalf("named build state = %q, want building (it has a version row)", named.State)
	}
}
