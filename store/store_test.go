package store

import (
	"context"
	"database/sql"
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
		LostAt: base.Add(3 * time.Minute), LostReason: "no checkpoint", Drained: true,
		Holder: "ci-job-42", HolderUrl: "https://ci.example.com/jobs/42",
		Class: "guaranteed",
	}
	// Zero times, nil slices and empty strings everywhere they can be.
	minimal := LeaseRow{
		ID: "lease-2", Owner: "bob", Image: "go-base",
		CreatedAt: base, ExpiresAt: base.Add(time.Hour), LastActive: base,
		State: "running", Class: "guaranteed",
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
	updated.Drained = false
	updated.LostAt = time.Time{} // leaving the lost state clears it
	updated.LostReason = ""      // and so does the reason
	updated.Holder = ""          // holder cleared: normal sweeping
	updated.HolderUrl = ""
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
		State: "running", Class: "guaranteed",
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
			State: "running", Class: "guaranteed",
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
			State: "running", Class: "guaranteed",
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

// Every embedded migration has its own version: a duplicate would be
// skipped for good on a database already at that version.
func TestMigrationVersionsUnique(t *testing.T) {
	if _, err := loadMigrations(); err != nil {
		t.Fatal(err)
	}
}

// TestMigration7HolderOnV6Database builds a database by hand at version
// 6 (the pre-holder schema, with one existing lease row) and opens it:
// migrations 7 and 8 must apply, stamping holder and holder_url plus
// the hold-expiry and last-action columns on the leases table, all
// defaulted empty — the unheld default that keeps normal sweeping.
func TestMigration7HolderOnV6Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v6.db")
	{
		db, err := Open(path) // applies 0001..0008
		if err != nil {
			t.Fatalf("open fresh: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	// Rewind the file to version 6: drop the columns migrations 7, 8
	// and 9 added and remove their schema_migrations rows, so the next
	// Open applies 0007, 0008 and 0009 for real.
	db6, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db6.Close()
	for _, stmt := range []string{
		`ALTER TABLE leases DROP COLUMN holder`,
		`ALTER TABLE leases DROP COLUMN holder_url`,
		`ALTER TABLE leases DROP COLUMN hold_set_at`,
		`ALTER TABLE leases DROP COLUMN hold_expires_at`,
		`ALTER TABLE leases DROP COLUMN hold_ttl`,
		`ALTER TABLE leases DROP COLUMN last_action`,
		`ALTER TABLE leases DROP COLUMN last_action_at`,
		`ALTER TABLE leases DROP COLUMN generation`,
		`ALTER TABLE leases DROP COLUMN checkpoint_interval`,
		`ALTER TABLE leases DROP COLUMN idle_suspend`,
		`ALTER TABLE leases DROP COLUMN memory_mb`,
		`ALTER TABLE leases DROP COLUMN class`,
		`ALTER TABLE leases DROP COLUMN priority`,
		`ALTER TABLE leases DROP COLUMN preempted_at`,
		`DROP TABLE lease_kept_builds`,
		`DROP TABLE IF EXISTS lease_jobs`,
		`DROP TABLE IF EXISTS named_snapshots`,
		`DROP TABLE IF EXISTS named_snapshot_names`,
		`DROP INDEX IF EXISTS leases_snapshot_build_id`,
		`ALTER TABLE leases DROP COLUMN snapshot_build_id`,
		`ALTER TABLE leases DROP COLUMN lost_reason`,
		`DELETE FROM schema_migrations WHERE version IN (7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19)`,
	} {
		if _, err := db6.Exec(stmt); err != nil {
			t.Fatalf("rewind (%s): %v", stmt, err)
		}
	}
	if _, err := db6.Exec(
		`INSERT INTO leases (id, owner, image, created_at, expires_at, last_active, state)
		 VALUES ('lease-v6', 'alice', 'py-base', '2026-01-01T00:00:00Z', '2026-01-01T01:00:00Z', '2026-01-01T00:30:00Z', 'running')`); err != nil {
		t.Fatalf("seed v6 lease: %v", err)
	}
	db6.Close()

	db, err := Open(path) // migrations 7 and 8 apply here
	if err != nil {
		t.Fatalf("open v6 database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	rows, err := db.ListLeases(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "lease-v6" {
		t.Fatalf("leases after migration: %v", rows)
	}
	if rows[0].Holder != "" || rows[0].HolderUrl != "" {
		t.Fatalf("holder not defaulted empty: %q / %q", rows[0].Holder, rows[0].HolderUrl)
	}
}

// TestMigration9GenerationOnV8Database builds a database by hand at
// version 8 (the pre-generation schema, with one existing lease row)
// and opens it: migration 9 must apply, stamping the generation column
// on the leases table defaulted to 1 — the value every lease is on
// until a restart or crash recovery moves it (2.2, #112).
func TestMigration9GenerationOnV8Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v8.db")
	{
		db, err := Open(path) // applies 0001..0009
		if err != nil {
			t.Fatalf("open fresh: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	// Rewind the file to version 8: drop the column migration 9 added
	// and remove its schema_migrations row, so the next Open applies
	// 0009 for real.
	db8, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	for _, stmt := range []string{
		`ALTER TABLE leases DROP COLUMN generation`,
		`ALTER TABLE leases DROP COLUMN checkpoint_interval`,
		`ALTER TABLE leases DROP COLUMN idle_suspend`,
		`ALTER TABLE leases DROP COLUMN memory_mb`,
		`ALTER TABLE leases DROP COLUMN class`,
		`ALTER TABLE leases DROP COLUMN priority`,
		`ALTER TABLE leases DROP COLUMN preempted_at`,
		`DROP TABLE lease_kept_builds`,
		`DROP TABLE IF EXISTS lease_jobs`,
		`DROP TABLE IF EXISTS named_snapshots`,
		`DROP TABLE IF EXISTS named_snapshot_names`,
		`DROP INDEX IF EXISTS leases_snapshot_build_id`,
		`ALTER TABLE leases DROP COLUMN snapshot_build_id`,
		`ALTER TABLE leases DROP COLUMN lost_reason`,
		`DELETE FROM schema_migrations WHERE version IN (9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19)`,
	} {
		if _, err := db8.Exec(stmt); err != nil {
			t.Fatalf("rewind (%s): %v", stmt, err)
		}
	}
	if _, err := db8.Exec(
		`INSERT INTO leases (id, owner, image, created_at, expires_at, last_active, state)
		 VALUES ('lease-v8', 'alice', 'py-base', '2026-01-01T00:00:00Z', '2026-01-01T01:00:00Z', '2026-01-01T00:30:00Z', 'running')`); err != nil {
		t.Fatalf("seed v8 lease: %v", err)
	}
	db8.Close()

	db, err := Open(path) // migration 9 applies here
	if err != nil {
		t.Fatalf("open v8 database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	rows, err := db.ListLeases(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "lease-v8" {
		t.Fatalf("leases after migration: %v", rows)
	}
	if rows[0].Generation != 1 {
		t.Fatalf("generation not defaulted to 1: %d", rows[0].Generation)
	}
}

// TestMigration12MemoryMBBackfill: the per-lease memory charge (#128)
// backfills from the image row — and a lease whose image row is gone
// stays 0 (uncharged), like a new lease of a vanished image.
func TestMigration12MemoryMBBackfill(t *testing.T) {
	db, path := openTestDB(t)
	ctx := context.Background()
	if err := db.UpsertImage(ctx, ImageRow{
		Name: "py-base", TemplateID: "t-1", MemoryMB: 2048, DiskMB: 5120,
		UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed image: %v", err)
	}
	for _, id := range []string{"lease-with-image", "lease-without-image"} {
		if err := db.UpsertLease(ctx, LeaseRow{
			ID: id, Owner: "alice", Image: "py-base",
			CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
			LastActive: time.Now(), State: "running", Class: "guaranteed",
		}); err != nil {
			t.Fatalf("seed lease %s: %v", id, err)
		}
	}
	if _, err := db.w.ExecContext(ctx, `UPDATE leases SET image = 'gone' WHERE id = 'lease-without-image'`); err != nil {
		t.Fatalf("detach image: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Rewind to version 12 so migration 13 applies for real, and to 11
	// so migration 12 (the memory_mb backfill this test pins) applies
	// after it.
	db11, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	for _, stmt := range []string{
		`ALTER TABLE leases DROP COLUMN class`,
		`ALTER TABLE leases DROP COLUMN priority`,
		`ALTER TABLE leases DROP COLUMN preempted_at`,
		`DROP TABLE IF EXISTS lease_jobs`,
		`DROP TABLE IF EXISTS named_snapshots`,
		`DROP TABLE IF EXISTS named_snapshot_names`,
		`ALTER TABLE leases DROP COLUMN idle_suspend`,
		`DROP INDEX IF EXISTS leases_snapshot_build_id`,
		`ALTER TABLE leases DROP COLUMN snapshot_build_id`,
		`ALTER TABLE leases DROP COLUMN lost_reason`,
		`DELETE FROM schema_migrations WHERE version IN (13, 14, 15, 16, 17, 18, 19)`,
		`ALTER TABLE leases DROP COLUMN memory_mb`,
		`DELETE FROM schema_migrations WHERE version = 12`,
	} {
		if _, err := db11.Exec(stmt); err != nil {
			t.Fatalf("rewind (%s): %v", stmt, err)
		}
	}
	db11.Close()

	db, err = Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	rows, err := db.ListLeases(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string]int{}
	for _, r := range rows {
		got[r.ID] = r.MemoryMB
	}
	if got["lease-with-image"] != 2048 {
		t.Fatalf("memory_mb not backfilled from the image row: %d", got["lease-with-image"])
	}
	if got["lease-without-image"] != 0 {
		t.Fatalf("memory_mb of a lease with no image row should stay 0, got %d", got["lease-without-image"])
	}
}

// TestMigration15IdleSuspendOnV14Database builds a database by hand at
// version 14 (one existing lease row) and opens it: migration 15 must
// apply, stamping idle_suspend defaulted to -1 (the host default) so
// existing leases keep today's behaviour (2.5, #129 part 2).
func TestMigration15IdleSuspendOnV14Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v14.db")
	{
		db, err := Open(path) // applies every migration
		if err != nil {
			t.Fatalf("open fresh: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	// Rewind the file to version 14: undo migration 16 (lease_jobs) and
	// drop the column migration 15 added, and remove their
	// schema_migrations rows, so the next Open applies 0015 (and 0016)
	// for real — migrate only applies versions above the highest row.
	db14, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS lease_jobs`,
		`DROP TABLE IF EXISTS named_snapshots`,
		`DROP TABLE IF EXISTS named_snapshot_names`,
		`ALTER TABLE leases DROP COLUMN idle_suspend`,
		`DROP INDEX IF EXISTS leases_snapshot_build_id`,
		`ALTER TABLE leases DROP COLUMN snapshot_build_id`,
		`ALTER TABLE leases DROP COLUMN lost_reason`,
		`DELETE FROM schema_migrations WHERE version IN (15, 16, 17, 18, 19)`,
	} {
		if _, err := db14.Exec(stmt); err != nil {
			t.Fatalf("rewind (%s): %v", stmt, err)
		}
	}
	if _, err := db14.Exec(
		`INSERT INTO leases (id, owner, image, created_at, expires_at, last_active, state)
		 VALUES ('lease-v14', 'alice', 'py-base', '2026-01-01T00:00:00Z', '2026-01-01T01:00:00Z', '2026-01-01T00:30:00Z', 'running')`); err != nil {
		t.Fatalf("seed v14 lease: %v", err)
	}
	db14.Close()

	db, err := Open(path) // migration 15 applies here
	if err != nil {
		t.Fatalf("open v14 database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	row, err := db.GetLease(context.Background(), "lease-v14")
	if err != nil {
		t.Fatalf("get lease: %v", err)
	}
	if row.IdleSuspend != -1 {
		t.Fatalf("idle_suspend after migration = %d, want -1 (the host default)", row.IdleSuspend)
	}
}

// TestMigration17NamedSnapshotsOnV16Database builds a database by hand at
// version 16 (one existing lease row) and opens it: migration 17 must
// apply, adding the named_snapshots and named_snapshot_names tables and
// the leases.snapshot_build_id column defaulted empty (2.7, #83).
func TestMigration17NamedSnapshotsOnV16Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v16.db")
	{
		db, err := Open(path) // applies every migration
		if err != nil {
			t.Fatalf("open fresh: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	// Rewind to version 16: drop what migration 17 added and its row, so
	// the next Open applies 0017 for real.
	db16, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS named_snapshots`,
		`DROP TABLE IF EXISTS named_snapshot_names`,
		`DROP INDEX IF EXISTS leases_snapshot_build_id`,
		`ALTER TABLE leases DROP COLUMN snapshot_build_id`,
		`ALTER TABLE leases DROP COLUMN lost_reason`,
		`DELETE FROM schema_migrations WHERE version IN (17, 18, 19)`,
	} {
		if _, err := db16.Exec(stmt); err != nil {
			t.Fatalf("rewind (%s): %v", stmt, err)
		}
	}
	if _, err := db16.Exec(
		`INSERT INTO leases (id, owner, image, created_at, expires_at, last_active, state)
		 VALUES ('lease-v16', 'alice', 'py-base', '2026-01-01T00:00:00Z', '2026-01-01T01:00:00Z', '2026-01-01T00:30:00Z', 'running')`); err != nil {
		t.Fatalf("seed v16 lease: %v", err)
	}
	db16.Close()

	db, err := Open(path) // migration 17 applies here
	if err != nil {
		t.Fatalf("open v16 database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	row, err := db.GetLease(context.Background(), "lease-v16")
	if err != nil {
		t.Fatalf("get lease: %v", err)
	}
	if row.SnapshotBuildID != "" {
		t.Fatalf("snapshot_build_id after migration = %q, want empty", row.SnapshotBuildID)
	}
	// The named tables exist and are writable.
	if _, err := db.InsertNamedSnapshot(context.Background(), NamedSnapshotRow{
		Owner: "alice", Name: "warm", BuildID: "b1", CreatedAt: time.Now(),
	}, 3); err != nil {
		t.Fatalf("insert named snapshot after migration: %v", err)
	}
}

// TestMigration19LostReasonOnV18Database builds a database at version 18
// (one existing lease row) and opens it: migration 19 must apply, adding
// the leases.lost_reason column defaulted empty, so a lease lost before
// the column existed answers without naming a cause.
func TestMigration19LostReasonOnV18Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v18.db")
	{
		db, err := Open(path) // applies every migration
		if err != nil {
			t.Fatalf("open fresh: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	// Rewind to version 18: drop what migration 19 added and its row, so
	// the next Open applies 0019 for real.
	db18, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	for _, stmt := range []string{
		`ALTER TABLE leases DROP COLUMN lost_reason`,
		`DELETE FROM schema_migrations WHERE version = 19`,
	} {
		if _, err := db18.Exec(stmt); err != nil {
			t.Fatalf("rewind (%s): %v", stmt, err)
		}
	}
	if _, err := db18.Exec(
		`INSERT INTO leases (id, owner, image, created_at, expires_at, last_active, state)
		 VALUES ('lease-v18', 'alice', 'py-base', '2026-01-01T00:00:00Z', '2026-01-01T01:00:00Z', '2026-01-01T00:30:00Z', 'lost')`); err != nil {
		t.Fatalf("seed v18 lease: %v", err)
	}
	db18.Close()

	db, err := Open(path) // migration 19 applies here
	if err != nil {
		t.Fatalf("open v18 database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	row, err := db.GetLease(context.Background(), "lease-v18")
	if err != nil {
		t.Fatalf("get lease: %v", err)
	}
	if row.LostReason != "" {
		t.Fatalf("lost_reason after migration = %q, want empty", row.LostReason)
	}
	// The column is writable through the upsert.
	row.LostReason = "no checkpoint to recover from"
	if err := db.UpsertLease(context.Background(), row); err != nil {
		t.Fatalf("upsert with a reason: %v", err)
	}
	again, err := db.GetLease(context.Background(), "lease-v18")
	if err != nil {
		t.Fatalf("get lease after upsert: %v", err)
	}
	if again.LostReason != "no checkpoint to recover from" {
		t.Fatalf("lost_reason after upsert = %q", again.LostReason)
	}
}
