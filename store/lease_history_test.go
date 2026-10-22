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

// TestLeaseHistoryStampsUpsertRoundTrip: the api service saves the whole
// lease row through UpsertLease, so start_ms, nap_since and max_nap_secs
// must write and read back through it.
func TestLeaseHistoryStampsUpsertRoundTrip(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	row := LeaseRow{
		ID: "l-nap", Owner: "u-abc", Image: "py-base",
		CreatedAt: at, ExpiresAt: at.Add(time.Hour), LastActive: at,
		State: "suspended", StartMS: 42, NapSince: at.Add(time.Minute), MaxNapSecs: 120,
	}
	if err := db.UpsertLease(ctx, row); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := db.GetLease(ctx, "l-nap")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.NapSince.Equal(row.NapSince) || got.MaxNapSecs != 120 || got.StartMS != 42 {
		t.Fatalf("read back nap_since %v max_nap_secs %d start_ms %d", got.NapSince, got.MaxNapSecs, got.StartMS)
	}
	// Cleared on resume: a zero nap_since writes back as unset.
	got.NapSince = time.Time{}
	if err := db.UpsertLease(ctx, got); err != nil {
		t.Fatalf("upsert cleared: %v", err)
	}
	again, err := db.GetLease(ctx, "l-nap")
	if err != nil {
		t.Fatalf("get cleared: %v", err)
	}
	if !again.NapSince.IsZero() || again.MaxNapSecs != 120 {
		t.Fatalf("after clear nap_since %v max_nap_secs %d", again.NapSince, again.MaxNapSecs)
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
		`ALTER TABLE leases DROP COLUMN self_token`,
		`ALTER TABLE leases DROP COLUMN self_token_hash`,
		`DROP TABLE IF EXISTS lease_history`,
		// Rewinding under migration 0030 (spoond-hwto) must also drop the
		// column it adds and its version row: with 30 still recorded the
		// reopened database would skip 0029 for good (29 <= 30) and the
		// dropped start_ms would never come back. Migration 0031 (the
		// output cap) is the same reason: 31 recorded would leave 29's
		// start_ms dropped for good.
		`ALTER TABLE lease_jobs DROP COLUMN retention_secs`,
		`ALTER TABLE lease_jobs DROP COLUMN output_max_bytes`,
		`ALTER TABLE lease_jobs DROP COLUMN stdout_dropped_bytes`,
		`ALTER TABLE lease_jobs DROP COLUMN stdout_dropped_newlines`,
		`ALTER TABLE lease_jobs DROP COLUMN stderr_dropped_bytes`,
		`ALTER TABLE lease_jobs DROP COLUMN stderr_dropped_newlines`,
		`ALTER TABLE lease_jobs DROP COLUMN stdout_dropped_ends_with_newline`,
		`ALTER TABLE lease_jobs DROP COLUMN stderr_dropped_ends_with_newline`,
		`ALTER TABLE images DROP COLUMN capability`,
		`ALTER TABLE images DROP COLUMN description`,
		`DELETE FROM schema_migrations WHERE version IN (29, 30, 31, 32, 33, 34)`,
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

// TestInsertLeaseHistoryBadOutcomeErrors: only a repeated lease id is
// skipped; a row that breaks the table's CHECK (an outcome spelled
// "release") is an error, not a silent skip.
func TestInsertLeaseHistoryBadOutcomeErrors(t *testing.T) {
	db, _ := openTestDB(t)
	now := time.Now().UTC()
	inserted, err := db.InsertLeaseHistory(context.Background(), LeaseHistoryRow{
		LeaseID: "bad-1", Owner: "u-x", Image: "py-base",
		CreatedAt: now.Add(-time.Minute), EndedAt: now, Outcome: "release",
	})
	if err == nil || inserted {
		t.Fatalf("insert with outcome %q = (%v, %v), want an error", "release", inserted, err)
	}
}
