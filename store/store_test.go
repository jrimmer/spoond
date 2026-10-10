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
		Class:         "guaranteed",
		SuspendReason: "idle", SuspendPolicyStep: "pressure/hugepages",
		SuspendBuildID: "b-3", SuspendedAt: base.Add(4 * time.Minute),
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
	// Resuming clears the suspension facts too.
	updated.SuspendReason = ""
	updated.SuspendPolicyStep = ""
	updated.SuspendBuildID = ""
	updated.SuspendedAt = time.Time{}
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
		`ALTER TABLE leases DROP COLUMN suspend_reason`,
		`ALTER TABLE leases DROP COLUMN suspend_policy_step`,
		`ALTER TABLE leases DROP COLUMN suspend_build_id`,
		`ALTER TABLE leases DROP COLUMN suspended_at`,
		`ALTER TABLE leases DROP COLUMN paused_expiry_notified`,
		`ALTER TABLE leases DROP COLUMN pinned_idle_since`,
		`ALTER TABLE leases DROP COLUMN paused_at`,
		`ALTER TABLE leases DROP COLUMN pinned`,
		`ALTER TABLE leases DROP COLUMN disk_mb`,
		`DELETE FROM schema_migrations WHERE version IN (7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23)`,
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
		`ALTER TABLE leases DROP COLUMN suspend_reason`,
		`ALTER TABLE leases DROP COLUMN suspend_policy_step`,
		`ALTER TABLE leases DROP COLUMN suspend_build_id`,
		`ALTER TABLE leases DROP COLUMN suspended_at`,
		`ALTER TABLE leases DROP COLUMN paused_expiry_notified`,
		`ALTER TABLE leases DROP COLUMN pinned_idle_since`,
		`ALTER TABLE leases DROP COLUMN paused_at`,
		`ALTER TABLE leases DROP COLUMN pinned`,
		`ALTER TABLE leases DROP COLUMN disk_mb`,
		`DELETE FROM schema_migrations WHERE version IN (9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23)`,
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

// TestMigration23DiskMBBackfill: the running-disk reservation charge
// (FS3a) backfills disk_mb from the image row — and a lease whose image
// row is gone stays 0 (unknown), like a new lease of a vanished image.
func TestMigration23DiskMBBackfill(t *testing.T) {
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

	// Rewind to version 22 so migration 0023 (the disk_mb backfill this
	// test pins) applies for real.
	db22, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	for _, stmt := range []string{
		`ALTER TABLE leases DROP COLUMN disk_mb`,
		`DELETE FROM schema_migrations WHERE version = 23`,
	} {
		if _, err := db22.Exec(stmt); err != nil {
			t.Fatalf("rewind (%s): %v", stmt, err)
		}
	}
	db22.Close()

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
		got[r.ID] = r.DiskMB
	}
	if got["lease-with-image"] != 5120 {
		t.Fatalf("disk_mb not backfilled from the image row: %d", got["lease-with-image"])
	}
	if got["lease-without-image"] != 0 {
		t.Fatalf("disk_mb of a lease with no image row should stay 0, got %d", got["lease-without-image"])
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
		`ALTER TABLE leases DROP COLUMN suspend_reason`,
		`ALTER TABLE leases DROP COLUMN suspend_policy_step`,
		`ALTER TABLE leases DROP COLUMN suspend_build_id`,
		`ALTER TABLE leases DROP COLUMN suspended_at`,
		`ALTER TABLE leases DROP COLUMN disk_mb`,
		`DELETE FROM schema_migrations WHERE version IN (13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23)`,
		`ALTER TABLE leases DROP COLUMN memory_mb`,
		`ALTER TABLE leases DROP COLUMN paused_expiry_notified`,
		`ALTER TABLE leases DROP COLUMN pinned_idle_since`,
		`ALTER TABLE leases DROP COLUMN paused_at`,
		`ALTER TABLE leases DROP COLUMN pinned`,
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
		`ALTER TABLE leases DROP COLUMN suspend_reason`,
		`ALTER TABLE leases DROP COLUMN suspend_policy_step`,
		`ALTER TABLE leases DROP COLUMN suspend_build_id`,
		`ALTER TABLE leases DROP COLUMN suspended_at`,
		`ALTER TABLE leases DROP COLUMN paused_expiry_notified`,
		`ALTER TABLE leases DROP COLUMN pinned_idle_since`,
		`ALTER TABLE leases DROP COLUMN paused_at`,
		`ALTER TABLE leases DROP COLUMN pinned`,
		`ALTER TABLE leases DROP COLUMN disk_mb`,
		`DELETE FROM schema_migrations WHERE version IN (15, 16, 17, 18, 19, 20, 21, 22, 23)`,
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
		`ALTER TABLE leases DROP COLUMN suspend_reason`,
		`ALTER TABLE leases DROP COLUMN suspend_policy_step`,
		`ALTER TABLE leases DROP COLUMN suspend_build_id`,
		`ALTER TABLE leases DROP COLUMN suspended_at`,
		`ALTER TABLE lease_jobs DROP COLUMN max_runtime_secs`,
		`ALTER TABLE lease_jobs DROP COLUMN reason`,
		`ALTER TABLE leases DROP COLUMN paused_expiry_notified`,
		`ALTER TABLE leases DROP COLUMN pinned_idle_since`,
		`ALTER TABLE leases DROP COLUMN paused_at`,
		`ALTER TABLE leases DROP COLUMN pinned`,
		`ALTER TABLE leases DROP COLUMN disk_mb`,
		`DELETE FROM schema_migrations WHERE version IN (17, 18, 19, 20, 21, 22, 23)`,
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
	// Rewind to version 18: drop what migrations 19 and 20 added and
	// their rows, so the next Open applies 0019 (and 0020) for real.
	db18, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	for _, stmt := range []string{
		`ALTER TABLE leases DROP COLUMN lost_reason`,
		`ALTER TABLE leases DROP COLUMN suspend_reason`,
		`ALTER TABLE leases DROP COLUMN suspend_policy_step`,
		`ALTER TABLE leases DROP COLUMN suspend_build_id`,
		`ALTER TABLE leases DROP COLUMN suspended_at`,
		`ALTER TABLE lease_jobs DROP COLUMN max_runtime_secs`,
		`ALTER TABLE lease_jobs DROP COLUMN reason`,
		`ALTER TABLE leases DROP COLUMN paused_expiry_notified`,
		`ALTER TABLE leases DROP COLUMN pinned_idle_since`,
		`ALTER TABLE leases DROP COLUMN paused_at`,
		`ALTER TABLE leases DROP COLUMN pinned`,
		`ALTER TABLE leases DROP COLUMN disk_mb`,
		`DELETE FROM schema_migrations WHERE version IN (19, 20, 21, 22, 23)`,
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

// TestMigration20JobMaxRuntimeOnV19Database builds a database at version
// 19 (one existing lease and one running job row) and opens it: migration
// 20 must apply, adding lease_jobs.max_runtime_secs and lease_jobs.reason
// defaulted to 0 and ” — an unchanged, uncapped record for jobs written
// before the column existed (spoond-wb5).
func TestMigration20JobMaxRuntimeOnV19Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v19.db")
	{
		db, err := Open(path) // applies every migration
		if err != nil {
			t.Fatalf("open fresh: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	// Rewind to version 19: drop what migration 20 added and its row, so
	// the next Open applies 0020 for real.
	db19, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	for _, stmt := range []string{
		`ALTER TABLE lease_jobs DROP COLUMN max_runtime_secs`,
		`ALTER TABLE lease_jobs DROP COLUMN reason`,
		`ALTER TABLE leases DROP COLUMN suspend_reason`,
		`ALTER TABLE leases DROP COLUMN suspend_policy_step`,
		`ALTER TABLE leases DROP COLUMN suspend_build_id`,
		`ALTER TABLE leases DROP COLUMN suspended_at`,
		`ALTER TABLE leases DROP COLUMN paused_expiry_notified`,
		`ALTER TABLE leases DROP COLUMN pinned_idle_since`,
		`ALTER TABLE leases DROP COLUMN paused_at`,
		`ALTER TABLE leases DROP COLUMN pinned`,
		`ALTER TABLE leases DROP COLUMN disk_mb`,
		`DELETE FROM schema_migrations WHERE version IN (20, 21, 22, 23)`,
	} {
		if _, err := db19.Exec(stmt); err != nil {
			t.Fatalf("rewind (%s): %v", stmt, err)
		}
	}
	if _, err := db19.Exec(
		`INSERT INTO leases (id, owner, image, created_at, expires_at, last_active, state)
		 VALUES ('lease-v19', 'alice', 'py-base', '2026-01-01T00:00:00Z', '2026-01-01T01:00:00Z', '2026-01-01T00:30:00Z', 'running')`); err != nil {
		t.Fatalf("seed v19 lease: %v", err)
	}
	if _, err := db19.Exec(
		`INSERT INTO lease_jobs (job_id, lease_id, owner, cmd, cwd, state, started_at, ended_at, generation)
		 VALUES ('job-v19', 'lease-v19', 'alice', 'sleep 1', '', 'running', '2026-01-01T00:30:00Z', '', 1)`); err != nil {
		t.Fatalf("seed v19 job: %v", err)
	}
	db19.Close()

	db, err := Open(path) // migration 20 applies here
	if err != nil {
		t.Fatalf("open v19 database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	row, err := db.GetJob(context.Background(), "job-v19")
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if row.MaxRuntimeSecs != 0 || row.Reason != "" {
		t.Fatalf("job after migration = max_runtime_secs %d reason %q, want 0 and empty",
			row.MaxRuntimeSecs, row.Reason)
	}
	// The columns are writable through the timeout path.
	if _, err := db.MarkJobTimedOut(context.Background(), "job-v19", 124, time.Now(), "bye"); err != nil {
		t.Fatalf("mark timed out: %v", err)
	}
	again, err := db.GetJob(context.Background(), "job-v19")
	if err != nil {
		t.Fatalf("get job after timeout: %v", err)
	}
	if again.Reason != "timed_out" || again.ExitCode == nil || *again.ExitCode != 124 {
		t.Fatalf("timed-out job = %+v", again)
	}
}

// TestMigration21SuspendFactsOnV20Database builds a database at version
// 20 (one existing lease row) and opens it: migration 21 must apply,
// adding the structured suspension facts defaulted empty, so a lease
// suspended before the columns existed reads as a suspension with no
// automatic reason (#145 D6).
func TestMigration21SuspendFactsOnV20Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v20.db")
	{
		db, err := Open(path) // applies every migration
		if err != nil {
			t.Fatalf("open fresh: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	// Rewind to version 20: drop what migration 21 added and its row, so
	// the next Open applies 0021 for real.
	db20, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db20.Close()
	for _, stmt := range []string{
		`ALTER TABLE leases DROP COLUMN suspend_reason`,
		`ALTER TABLE leases DROP COLUMN suspend_policy_step`,
		`ALTER TABLE leases DROP COLUMN suspend_build_id`,
		`ALTER TABLE leases DROP COLUMN suspended_at`,
		`ALTER TABLE leases DROP COLUMN paused_expiry_notified`,
		`ALTER TABLE leases DROP COLUMN pinned_idle_since`,
		`ALTER TABLE leases DROP COLUMN paused_at`,
		`ALTER TABLE leases DROP COLUMN pinned`,
		`ALTER TABLE leases DROP COLUMN disk_mb`,
		`DELETE FROM schema_migrations WHERE version IN (21, 22, 23)`,
	} {
		if _, err := db20.Exec(stmt); err != nil {
			t.Fatalf("rewind (%s): %v", stmt, err)
		}
	}
	if _, err := db20.Exec(
		`INSERT INTO leases (id, owner, image, created_at, expires_at, last_active, state)
		 VALUES ('lease-v20', 'alice', 'py-base', '2026-01-01T00:00:00Z', '2026-01-01T01:00:00Z', '2026-01-01T00:30:00Z', 'suspended')`); err != nil {
		t.Fatalf("seed v20 lease: %v", err)
	}
	db20.Close()

	db, err := Open(path) // migration 21 applies here
	if err != nil {
		t.Fatalf("open v20 database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	row, err := db.GetLease(context.Background(), "lease-v20")
	if err != nil {
		t.Fatalf("get lease: %v", err)
	}
	if row.SuspendReason != "" || row.SuspendPolicyStep != "" || row.SuspendBuildID != "" || !row.SuspendedAt.IsZero() {
		t.Fatalf("suspend facts after migration = %q/%q/%q/%v, want empty",
			row.SuspendReason, row.SuspendPolicyStep, row.SuspendBuildID, row.SuspendedAt)
	}
	// The columns are writable through the upsert.
	row.SuspendReason = "idle"
	row.SuspendPolicyStep = "pressure/disk"
	row.SuspendBuildID = "b-1"
	row.SuspendedAt = time.Now().UTC().Truncate(time.Second)
	if err := db.UpsertLease(context.Background(), row); err != nil {
		t.Fatalf("upsert with suspension facts: %v", err)
	}
	again, err := db.GetLease(context.Background(), "lease-v20")
	if err != nil {
		t.Fatalf("get lease after upsert: %v", err)
	}
	if again.SuspendReason != "idle" || again.SuspendPolicyStep != "pressure/disk" || again.SuspendBuildID != "b-1" || !again.SuspendedAt.Equal(row.SuspendedAt) {
		t.Fatalf("suspend facts after upsert = %+v", again)
	}
}

// TestMigration22PinsHoldsAndBackfillsPausedAt builds a database at
// version 21 with a live-hold lease, a lapsed-hold lease, an
// unheld-suspended lease, a held-suspended lease and a lost lease, and
// opens it. Migration 22 must apply: every unexpired hold becomes a pin,
// every row already suspended (held or not) gets paused_at backfilled to
// the migration time so the one clock starts a fresh 30 d from the
// upgrade, and the hold columns are cleared for rollback (FS5,
// spoond-k0uz H3/M6).
func TestMigration22PinsHoldsAndBackfillsPausedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v21.db")
	{
		db, err := Open(path) // applies every migration
		if err != nil {
			t.Fatalf("open fresh: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	// Rewind to version 21: drop what migration 22 added and its row, so
	// the next Open applies 0022 for real. The hold columns stay (0008
	// created them), so the UPDATE's columns exist.
	db21, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	for _, stmt := range []string{
		`ALTER TABLE leases DROP COLUMN paused_expiry_notified`,
		`ALTER TABLE leases DROP COLUMN pinned_idle_since`,
		`ALTER TABLE leases DROP COLUMN paused_at`,
		`ALTER TABLE leases DROP COLUMN pinned`,
		`ALTER TABLE leases DROP COLUMN disk_mb`,
		`DELETE FROM schema_migrations WHERE version IN (22, 23)`,
	} {
		if _, err := db21.Exec(stmt); err != nil {
			t.Fatalf("rewind (%s): %v", stmt, err)
		}
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	later := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	// A whole-second expires_at and a hold 293 ms into the same second:
	// in string order the expires_at sorts after the hold, in true time
	// order the hold is later.
	wholeSecondTTL := time.Now().Add(30 * time.Minute).UTC().Format("2006-01-02T15:04:05Z")
	wholeSecondHold := time.Now().Add(30*time.Minute + 293*time.Millisecond).UTC().Format("2006-01-02T15:04:05.293Z")
	for _, seed := range []struct {
		id, state, holder, holdExpires string
		suspended                      int
		persistent                     int
		expiresAt                      string
	}{
		{"lease-hold", "running", "ci-job", future, 0, 1, past},
		{"lease-lapsed", "running", "ci-job", past, 0, 1, past},
		{"lease-plain", "running", "", "", 0, 1, past},
		// Already suspended with no hold: the backfill gives it the one
		// clock (it would otherwise never be released).
		{"lease-suspended", "suspended", "", "", 1, 1, past},
		// Suspended and held: pinned by the hold conversion and given the
		// same clock.
		{"lease-held-suspended", "suspended", "ci-job", future, 1, 1, past},
		// A lost lease is suspended too; the backfill applies, so it is
		// never released sooner than 30 d after the upgrade.
		{"lease-lost", "lost", "", "", 1, 1, past},
		// spoond-k0uz R3-1: a non-persistent lease with a live hold. In
		// 2.9 the hold kept it past its TTL (held() is Holder != ""), so
		// its expires_at may already be past when the upgrade pins it; the
		// migration must extend the TTL to the hold's expiry or the first
		// 3.0 sweep deletes the VM at once.
		{"lease-hold-ttl-past", "running", "pool:honey/work-1", future, 0, 0, past},
		// The same shape whose TTL is not later than its hold: nothing to
		// extend (max() keeps the later value). The TTL is strictly later
		// than the hold so the max() is really exercised (R4-4).
		{"lease-hold-ttl-future", "running", "pool:honey/work-2", future, 0, 0, later},
		// The hold lands in the same second as a whole-second expires_at,
		// a fraction of a second later. Plain string order puts the
		// whole-second stamp after the fractional one, so the extension
		// would be skipped and the TTL would stay 293 ms short; the
		// migration compares by instant instead.
		{"lease-hold-ttl-fraction", "running", "pool:honey/work-3", wholeSecondHold, 0, 0, wholeSecondTTL},
	} {
		if _, err := db21.Exec(
			`INSERT INTO leases (id, owner, image, created_at, expires_at, last_active, state, suspended, persistent, holder, hold_expires_at, hold_set_at, hold_ttl)
			 VALUES (?, 'alice', 'py-base', '2026-01-01T00:00:00Z', ?, '2026-01-01T00:30:00Z', ?, ?, ?, ?, ?, ?, 3600)`,
			seed.id, seed.expiresAt, seed.state, seed.suspended, seed.persistent, seed.holder, seed.holdExpires, seed.holdExpires); err != nil {
			t.Fatalf("seed %s: %v", seed.id, err)
		}
	}
	db21.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("open v21 database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	rows, err := db.ListLeases(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string]LeaseRow{}
	for _, r := range rows {
		got[r.ID] = r
	}
	// A live hold becomes a pin; a lapsed or absent hold does not.
	if !got["lease-hold"].Pinned {
		t.Fatal("a lease with an unexpired hold was not pinned by migration 22")
	}
	if got["lease-lapsed"].Pinned {
		t.Fatal("a lease with a lapsed hold was pinned by migration 22")
	}
	if got["lease-plain"].Pinned {
		t.Fatal("a plain lease was pinned by migration 22")
	}
	if !got["lease-held-suspended"].Pinned {
		t.Fatal("a suspended lease with an unexpired hold was not pinned by migration 22")
	}
	// Every row already suspended gets the paused_at backfill, pinned or
	// not, lost included.
	for _, id := range []string{"lease-suspended", "lease-held-suspended", "lease-lost"} {
		if got[id].PausedAt.IsZero() {
			t.Fatalf("%s (suspended) did not get paused_at backfilled", id)
		}
		if age := time.Since(got[id].PausedAt); age < 0 || age > time.Minute {
			t.Fatalf("%s paused_at = %v, want the migration time", id, got[id].PausedAt)
		}
	}
	// A row that was not suspended gets no clock.
	if !got["lease-hold"].PausedAt.IsZero() || !got["lease-plain"].PausedAt.IsZero() {
		t.Fatal("a running lease was given a pause date by migration 22")
	}
	// spoond-k0uz R3-1: the pinned non-persistent row whose TTL the hold
	// had already outlived gets expires_at = its hold_expires_at, so the
	// first 3.0 sweep does not delete a lease the hold was keeping alive.
	if want, _ := time.Parse(time.RFC3339Nano, future); !got["lease-hold-ttl-past"].ExpiresAt.Equal(want) {
		t.Fatalf("a pinned non-persistent lease with a past TTL kept expires_at %v, want the hold expiry %v", got["lease-hold-ttl-past"].ExpiresAt, want)
	}
	// max(): a TTL already past the hold expiry is left alone; the seed's
	// TTL is strictly later than the hold, so the max() is really tested.
	if want, _ := time.Parse(time.RFC3339Nano, later); !got["lease-hold-ttl-future"].ExpiresAt.Equal(want) {
		t.Fatalf("a pinned non-persistent lease with a later TTL had its expires_at changed to %v, want %v", got["lease-hold-ttl-future"].ExpiresAt, want)
	}
	// Same-second, sub-second-later hold: the extension still happens —
	// the compare is by instant, not by string (the whole-second
	// "…:00Z" string sorts after "…:00.293Z").
	if want, _ := time.Parse(time.RFC3339Nano, wholeSecondHold); !got["lease-hold-ttl-fraction"].ExpiresAt.Equal(want) {
		t.Fatalf("a whole-second TTL 293 ms before its hold stayed %v, want the hold expiry %v", got["lease-hold-ttl-fraction"].ExpiresAt, want)
	}
	// Persistent rows are never TTL-swept, so the conversion leaves their
	// expires_at alone (the seed's past value, not the hold's future one).
	if want, _ := time.Parse(time.RFC3339Nano, past); !got["lease-hold"].ExpiresAt.Equal(want) {
		t.Fatalf("a persistent pinned lease's expires_at was rewritten to %v, want %v", got["lease-hold"].ExpiresAt, want)
	}
	// Rollback: the hold columns are cleared after conversion, so a 2.9
	// binary re-reads every row as unheld (spoond-k0uz M6).
	for _, id := range []string{"lease-hold", "lease-held-suspended", "lease-lapsed"} {
		var holdExpires, holdSetAt string
		var holdTTL int
		if err := db.r.QueryRowContext(ctx,
			`SELECT hold_expires_at, hold_set_at, hold_ttl FROM leases WHERE id = ?`, id).
			Scan(&holdExpires, &holdSetAt, &holdTTL); err != nil {
			t.Fatalf("read hold columns for %s: %v", id, err)
		}
		if holdExpires != "" || holdSetAt != "" || holdTTL != 0 {
			t.Fatalf("migration 22 left hold columns on %s: expires=%q set=%q ttl=%d", id, holdExpires, holdSetAt, holdTTL)
		}
	}
}

// TestUnpinLeasesByHolderPrefix: the migration-window helper clears only
// the matching pinned leases and refuses an empty prefix.
func TestUnpinLeasesByHolderPrefix(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	for _, r := range []LeaseRow{
		{ID: "worker", Owner: "o", Image: "i", State: "running", Class: "guaranteed", Holder: "pool:honey/work-1", Pinned: true},
		{ID: "other", Owner: "o", Image: "i", State: "running", Class: "guaranteed", Holder: "ci-job", Pinned: true},
		{ID: "wild", Owner: "o", Image: "i", State: "running", Class: "guaranteed", Holder: "poolX", Pinned: true},
	} {
		if err := db.UpsertLease(ctx, r); err != nil {
			t.Fatalf("upsert %s: %v", r.ID, err)
		}
	}
	if _, err := db.UnpinLeasesByHolderPrefix(ctx, ""); err == nil {
		t.Fatal("an empty prefix was accepted")
	}
	n, err := db.UnpinLeasesByHolderPrefix(ctx, "pool:")
	if err != nil {
		t.Fatalf("unpin: %v", err)
	}
	if n != 1 {
		t.Fatalf("unpinned %d leases, want 1", n)
	}
	rows, err := db.ListLeases(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.ID] = r.Pinned
	}
	if got["worker"] || !got["other"] || !got["wild"] {
		t.Fatalf("unpin by prefix hit the wrong rows: %v", got)
	}
}
