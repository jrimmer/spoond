package store

// Lease history (records A, spoond-15a5): one row per ended lease, and
// the live lease's nap/start stamps the history row reads at release.

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestLeaseHistoryRoundTrip: a row inserts, reads back field for field,
// a second insert of the same lease id is ignored (INSERT OR IGNORE — a
// lost lease's later release must not overwrite the loss), and an
// unknown id answers ErrNotFound.
func TestLeaseHistoryRoundTrip(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()

	at := time.Now().UTC().Truncate(time.Second)
	inserted, err := db.InsertLeaseHistory(ctx, LeaseHistoryRow{
		LeaseID: "l-1", Owner: "u-abc", Image: "py-base", MemoryMB: 2048,
		CreatedAt: at, EndedAt: at.Add(time.Hour),
		Outcome: "released", Reason: `ci job 3609 ✓ 11m02s`, PeakMiB: 412,
		StartMS: 1234, StartKind: "warm", MaxNapSecs: 90,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if !inserted {
		t.Fatal("first insert reported a skip, want an insert")
	}
	got, err := db.GetLeaseHistory(ctx, "l-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	want := LeaseHistoryRow{
		LeaseID: "l-1", Owner: "u-abc", Image: "py-base", MemoryMB: 2048,
		CreatedAt: at, EndedAt: at.Add(time.Hour),
		Outcome: "released", Reason: `ci job 3609 ✓ 11m02s`, PeakMiB: 412,
		StartMS: 1234, StartKind: "warm", MaxNapSecs: 90,
	}
	if got != want {
		t.Fatalf("round trip =\n%+v\nwant\n%+v", got, want)
	}

	// INSERT OR IGNORE: the same lease id again changes nothing.
	inserted, err = db.InsertLeaseHistory(ctx, LeaseHistoryRow{
		LeaseID: "l-1", Owner: "u-abc", Image: "py-base", MemoryMB: 1,
		CreatedAt: at, EndedAt: at.Add(2 * time.Hour),
		Outcome: "lost", Reason: "overwritten", PeakMiB: 7,
	})
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if inserted {
		t.Fatal("second insert reported an insert, want a skip")
	}
	got, err = db.GetLeaseHistory(ctx, "l-1")
	if err != nil {
		t.Fatalf("get after ignore: %v", err)
	}
	if got.Outcome != "released" || got.Reason != `ci job 3609 ✓ 11m02s` || got.EndedAt.Equal(at.Add(2*time.Hour)) {
		t.Fatalf("second insert overwrote the first row: %+v", got)
	}

	if _, err := db.GetLeaseHistory(ctx, "l-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing id err = %v, want ErrNotFound", err)
	}

	// ListLeaseHistory returns every row, oldest end first.
	all, err := db.ListLeaseHistory(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 || all[0].LeaseID != "l-1" || all[0].Outcome != "released" {
		t.Fatalf("list = %+v, want the one released row for l-1", all)
	}
}

// TestLeaseStartMSStamp: SetLeaseStartMS writes the latency on the live
// lease row and reads back through GetLease; a missing row is not an
// error.
func TestLeaseStartMSStamp(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	if err := db.UpsertLease(ctx, LeaseRow{
		ID: "l-start", Owner: "u-abc", Image: "py-base",
		CreatedAt: at, ExpiresAt: at.Add(time.Hour), LastActive: at,
		State: "running",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := db.SetLeaseStartMS(ctx, "l-start", 1234); err != nil {
		t.Fatalf("set start_ms: %v", err)
	}
	row, err := db.GetLease(ctx, "l-start")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.StartMS != 1234 {
		t.Fatalf("start_ms = %d, want 1234", row.StartMS)
	}
	// A stamp for a lease whose row is already gone is a no-op, not an
	// error: the release deleted it first.
	if err := db.SetLeaseStartMS(ctx, "l-gone", 5); err != nil {
		t.Fatalf("set start_ms on missing row: %v", err)
	}
}

// TestLeaseNapStamps: SuspendNap stamps nap_since; ResumeNap raises
// max_nap_secs to the sleep's length when that is the longer so far and
// clears nap_since; a resume with no sleep in flight changes nothing; a
// lease released while suspended keeps nap_since (nothing cleared it).
func TestLeaseNapStamps(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	if err := db.UpsertLease(ctx, LeaseRow{
		ID: "l-nap", Owner: "u-abc", Image: "py-base",
		CreatedAt: at, ExpiresAt: at.Add(time.Hour), LastActive: at,
		State: "running",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := db.SuspendNap(ctx, "l-nap", at); err != nil {
		t.Fatalf("suspend nap: %v", err)
	}
	row, err := db.GetLease(ctx, "l-nap")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !row.NapSince.Equal(at) || row.MaxNapSecs != 0 {
		t.Fatalf("after suspend = nap_since %v max_nap_secs %d, want %v and 0", row.NapSince, row.MaxNapSecs, at)
	}
	if err := db.ResumeNap(ctx, "l-nap", at.Add(90*time.Second)); err != nil {
		t.Fatalf("resume nap: %v", err)
	}
	row, err = db.GetLease(ctx, "l-nap")
	if err != nil {
		t.Fatalf("get after resume: %v", err)
	}
	if !row.NapSince.IsZero() || row.MaxNapSecs != 90 {
		t.Fatalf("after resume = nap_since %v max_nap_secs %d, want zero and 90", row.NapSince, row.MaxNapSecs)
	}
	// A second resume with no sleep in flight changes nothing: the stamp
	// stays cleared and the record stands.
	if err := db.ResumeNap(ctx, "l-nap", at.Add(time.Hour)); err != nil {
		t.Fatalf("resume with no sleep in flight: %v", err)
	}
	again, err := db.GetLease(ctx, "l-nap")
	if err != nil {
		t.Fatalf("get after empty resume: %v", err)
	}
	if !again.NapSince.IsZero() || again.MaxNapSecs != 90 {
		t.Fatalf("after empty resume = nap_since %v max_nap_secs %d, want zero and 90", again.NapSince, again.MaxNapSecs)
	}
	// UpsertLease writes all three columns back (the api service saves
	// the whole row through it).
	row.NapSince = at.Add(time.Minute)
	row.MaxNapSecs = 120
	row.StartMS = 42
	if err := db.UpsertLease(ctx, row); err != nil {
		t.Fatalf("upsert with nap stamps: %v", err)
	}
	again, err = db.GetLease(ctx, "l-nap")
	if err != nil {
		t.Fatalf("get after upsert: %v", err)
	}
	if !again.NapSince.Equal(at.Add(time.Minute)) || again.MaxNapSecs != 120 || again.StartMS != 42 {
		t.Fatalf("after upsert = nap_since %v max_nap_secs %d start_ms %d", again.NapSince, again.MaxNapSecs, again.StartMS)
	}
}

// TestMigration29LeaseHistoryOnV28Database builds a database at version
// 28 (one existing lease row) and opens it: migration 29 must apply,
// creating the lease_history table and defaulting the new lease columns,
// so rows from before it existed read as never suspended and unknown
// start latency.
func TestMigration29LeaseHistoryOnV28Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v28.db")
	{
		db, err := Open(path) // applies every migration
		if err != nil {
			t.Fatalf("open fresh: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	// Rewind to version 28: drop what migration 29 added and its row, so
	// the next Open applies 0029 for real.
	db28, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db28.Close()
	for _, stmt := range []string{
		`ALTER TABLE leases DROP COLUMN start_ms`,
		`ALTER TABLE leases DROP COLUMN nap_since`,
		`ALTER TABLE leases DROP COLUMN max_nap_secs`,
		`DROP TABLE IF EXISTS lease_history`,
		`DELETE FROM schema_migrations WHERE version = 29`,
	} {
		if _, err := db28.Exec(stmt); err != nil {
			t.Fatalf("rewind (%s): %v", stmt, err)
		}
	}
	if _, err := db28.Exec(
		`INSERT INTO leases (id, owner, image, created_at, expires_at, last_active, state)
		 VALUES ('lease-v28', 'alice', 'py-base', '2026-01-01T00:00:00Z', '2026-01-01T01:00:00Z', '2026-01-01T00:30:00Z', 'running')`); err != nil {
		t.Fatalf("seed v28 lease: %v", err)
	}
	db28.Close()

	db, err := Open(path) // migration 29 applies here
	if err != nil {
		t.Fatalf("open v28 database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	row, err := db.GetLease(context.Background(), "lease-v28")
	if err != nil {
		t.Fatalf("get lease: %v", err)
	}
	if row.StartMS != 0 || !row.NapSince.IsZero() || row.MaxNapSecs != 0 {
		t.Fatalf("lease after migration = start_ms %d nap_since %v max_nap_secs %d, want all zero",
			row.StartMS, row.NapSince, row.MaxNapSecs)
	}
	// The history table is empty but answers.
	if _, err := db.GetLeaseHistory(context.Background(), "lease-v28"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("history for v28 lease err = %v, want ErrNotFound", err)
	}
}
