package main

// The `spoond history import` reader (records A, spoond-15a5): journalctl
// short-iso-precise lines in, one lease_history row per paired
// create+end, idempotent on a re-run.

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v4/store"
)

// historyTestDB opens a temp store for the import tests.
func historyTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "spoond.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// sampleJournal is the fixture: real-shaped `journalctl -o
// short-iso-precise` lines around a lease journal — a create/release
// pair with a peak and a quoted CI reason, a create/lost pair whose
// quoted reason carries spaces and an equals sign, a suspend line, a
// systemd notice, and a release whose create is outside the window.
const sampleJournal = `2026-10-09T16:30:59.123456+00:00 spoond-backend: starting lease sweep
2026-10-09T16:31:02.123456+00:00 lease journal: op=create lease_id=l-1 owner=u-abc sandbox=sb-1 image=py-base memory_mib=2048 reason=new
2026-10-09T16:31:03.000000+00:00 lease journal: op=suspend lease_id=l-1 owner=u-abc sandbox=sb-1 image=py-base memory_mib=2048 reason=take_back
2026-10-09T18:02:11.500000+00:00 lease journal: op=release lease_id=l-1 owner=u-abc sandbox=sb-1 image=py-base memory_mib=2048 peak_mib=412 reason="ci job 3609 ✓ 11m02s"
2026-10-09T18:05:00.000001+00:00 lease journal: op=create lease_id=l-2 owner=u-def sandbox=sb-2 image=elixir-base memory_mib=4096 reason=snapshot ci@3
2026-10-10T02:14:09.000000+00:00 lease journal: op=lost lease_id=l-2 owner=u-def sandbox=sb-2 image=elixir-base memory_mib=4096 reason="recovery failed after 3 attempts: watch=exited"
2026-10-10T02:14:10.000000+00:00 systemd[1]: spoond-backend.service: Deactivated successfully.
2026-10-10T09:00:00.000000+00:00 lease journal: op=release lease_id=l-3 owner=u-abc sandbox=sb-3 image=py-base memory_mib=2048 reason=ttl
`

// TestHistoryImportPairsAndPersists: paired lines write one row each
// with the journal's fields; unpaired and non-lease lines are skipped.
func TestHistoryImportPairsAndPersists(t *testing.T) {
	db := historyTestDB(t)
	ctx := context.Background()

	imported, skipped, err := importHistory(ctx, db, strings.NewReader(sampleJournal))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if imported != 2 {
		t.Fatalf("imported = %d, want 2 (l-1 released, l-2 lost)", imported)
	}
	if skipped != 4 {
		t.Fatalf("skipped = %d, want 4 (the notice, the suspend, the create-less release, the systemd line)", skipped)
	}

	h1, err := db.GetLeaseHistory(ctx, "l-1")
	if err != nil {
		t.Fatalf("get l-1: %v", err)
	}
	wantCreated := time.Date(2026, 10, 9, 16, 31, 2, 123456000, time.UTC)
	wantEnded := time.Date(2026, 10, 9, 18, 2, 11, 500000000, time.UTC)
	if !h1.CreatedAt.Equal(wantCreated) || !h1.EndedAt.Equal(wantEnded) {
		t.Fatalf("l-1 span = %v..%v, want %v..%v", h1.CreatedAt, h1.EndedAt, wantCreated, wantEnded)
	}
	if h1.Outcome != "released" || h1.Owner != "u-abc" || h1.Image != "py-base" || h1.MemoryMB != 2048 || h1.PeakMiB != 412 {
		t.Fatalf("l-1 row = %+v, want the journal's fields", h1)
	}
	// The quoted reason unquotes to its text: the CI reason with its
	// check mark and spaces.
	if h1.Reason != "ci job 3609 ✓ 11m02s" {
		t.Fatalf("l-1 reason = %q, want the quoted CI reason", h1.Reason)
	}
	// The plan's stays-0 columns: the journal predates them.
	if h1.StartMS != 0 || h1.StartKind != "" || h1.MaxNapSecs != 0 {
		t.Fatalf("l-1 stamps = %d/%q/%d, want 0/\"\"/0", h1.StartMS, h1.StartKind, h1.MaxNapSecs)
	}

	h2, err := db.GetLeaseHistory(ctx, "l-2")
	if err != nil {
		t.Fatalf("get l-2: %v", err)
	}
	if h2.Outcome != "lost" {
		t.Fatalf("l-2 outcome = %q, want lost", h2.Outcome)
	}
	if h2.Reason != "recovery failed after 3 attempts: watch=exited" {
		t.Fatalf("l-2 reason = %q, want the quoted loss reason (with its equals sign)", h2.Reason)
	}
}

// TestHistoryImportIdempotent: a second run of the same journal imports
// nothing more and skips nothing extra — INSERT OR IGNORE finds its own
// rows.
func TestHistoryImportIdempotent(t *testing.T) {
	db := historyTestDB(t)
	ctx := context.Background()

	if _, _, err := importHistory(ctx, db, strings.NewReader(sampleJournal)); err != nil {
		t.Fatalf("first import: %v", err)
	}
	imported, skipped, err := importHistory(ctx, db, strings.NewReader(sampleJournal))
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if imported != 0 {
		t.Fatalf("second run imported %d, want 0", imported)
	}
	if skipped != 4 {
		t.Fatalf("second run skipped = %d, want the same 4", skipped)
	}
	rows, err := db.ListLeaseHistory(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows after two runs = %d, want 2", len(rows))
	}
}

// TestHistoryImportLostBeforeReleaseKeepsLost: a lost lease whose grace
// later logged a release line keeps the loss — the earlier end wins and
// the release's peak (the loss line carries none) rides along.
func TestHistoryImportLostBeforeReleaseKeepsLost(t *testing.T) {
	db := historyTestDB(t)
	ctx := context.Background()
	const journal = `2026-10-09T10:00:00.000000+00:00 lease journal: op=create lease_id=l-9 owner=u-abc sandbox=sb-9 image=py-base memory_mib=1024 reason=new
2026-10-09T11:00:00.000000+00:00 lease journal: op=lost lease_id=l-9 owner=u-abc sandbox=sb-9 image=py-base memory_mib=1024 reason="no checkpoint to recover from"
2026-10-09T13:00:00.000000+00:00 lease journal: op=release lease_id=l-9 owner=u-abc sandbox=sb-9 image=py-base memory_mib=1024 peak_mib=512 reason=lost_expired
`
	if _, _, err := importHistory(ctx, db, strings.NewReader(journal)); err != nil {
		t.Fatalf("import: %v", err)
	}
	h, err := db.GetLeaseHistory(ctx, "l-9")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if h.Outcome != "lost" {
		t.Fatalf("outcome = %q, want lost (the earlier end wins)", h.Outcome)
	}
	if h.Reason != "no checkpoint to recover from" {
		t.Fatalf("reason = %q, want the loss's", h.Reason)
	}
	if h.PeakMiB != 512 {
		t.Fatalf("peak_mib = %d, want 512 (the later release line's)", h.PeakMiB)
	}
	if !h.EndedAt.Equal(time.Date(2026, 10, 9, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("ended_at = %v, want the loss's 11:00", h.EndedAt)
	}
}

// TestHistoryImportMalformedLines: an unparseable stamp, a non-key=value
// body and a truncated line are skipped, not fatal.
func TestHistoryImportMalformedLines(t *testing.T) {
	db := historyTestDB(t)
	ctx := context.Background()
	const journal = "not a journal line at all\n" +
		"2026-10-09T10:00:00.000000+00:00 lease journal: op=release lease_id=l-x owner=u image=py-base memory_mib=1 reason=ttl\n" +
		"Oct 09 10:00:00 host lease journal: op=create lease_id=l-y owner=u image=py-base memory_mib=1 reason=new\n"
	imported, skipped, err := importHistory(ctx, db, strings.NewReader(journal))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if imported != 0 {
		t.Fatalf("imported = %d, want 0 (the only paired-looking line has no create)", imported)
	}
	if skipped != 3 {
		t.Fatalf("skipped = %d, want 3", skipped)
	}
}

// TestHistoryImportLargeReasonLine: a reason near the scanner line
// limit survives (the buffer is grown), a >1 MiB line is an error the
// command reports.
func TestHistoryImportLargeReasonLine(t *testing.T) {
	db := historyTestDB(t)
	ctx := context.Background()
	reason := strings.Repeat("x", 900*1024)
	var buf bytes.Buffer
	buf.WriteString("2026-10-09T10:00:00.000000+00:00 lease journal: op=create lease_id=l-big owner=u image=py-base memory_mib=1 reason=new\n")
	// The 900 KiB reason stays under the 1 MiB line cap.
	buf.WriteString("2026-10-09T11:00:00.000000+00:00 lease journal: op=release lease_id=l-big owner=u image=py-base memory_mib=1 reason=\"" + reason + "\"\n")
	imported, _, err := importHistory(ctx, db, &buf)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if imported != 1 {
		t.Fatalf("imported = %d, want 1", imported)
	}
	h, err := db.GetLeaseHistory(ctx, "l-big")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if h.Reason != reason {
		t.Fatal("reason did not survive the round trip")
	}
}

// TestParseJournalStamp: the short-iso-precise stamp and a plain RFC
// 3339 offset both parse; junk does not.
func TestParseJournalStamp(t *testing.T) {
	cases := []struct {
		in    string
		want  time.Time
		valid bool
	}{
		{"2026-10-09T16:31:02.123456+00:00", time.Date(2026, 10, 9, 16, 31, 2, 123456000, time.UTC), true},
		{"2026-10-09T16:31:02.123456Z", time.Date(2026, 10, 9, 16, 31, 2, 123456000, time.UTC), true},
		{"2026-10-09T18:31:02+02:00", time.Date(2026, 10, 9, 16, 31, 2, 0, time.UTC), true},
		{"2026-10-09T16:31:02", time.Date(2026, 10, 9, 16, 31, 2, 0, time.UTC), true},
		{"Oct 09 16:31:02", time.Time{}, false},
		{"1696871462", time.Time{}, false},
	}
	for _, c := range cases {
		got, ok := parseJournalStamp(c.in)
		if ok != c.valid {
			t.Errorf("parseJournalStamp(%q) ok = %v, want %v", c.in, ok, c.valid)
			continue
		}
		if c.valid && !got.Equal(c.want) {
			t.Errorf("parseJournalStamp(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestHistoryCommandRegistered: the subcommand is in the command map
// (the blocker this file exists for) and its dispatch answers help.
func TestHistoryCommandRegistered(t *testing.T) {
	c, ok := commands["history"]
	if !ok {
		t.Fatal("no `history` command registered")
	}
	if c.run == nil {
		t.Fatal("the history command has no run")
	}
}

// TestImportHistoryEmptyInput: no input, no rows, no error.
func TestImportHistoryEmptyInput(t *testing.T) {
	db := historyTestDB(t)
	imported, skipped, err := importHistory(context.Background(), db, strings.NewReader(""))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if imported != 0 || skipped != 0 {
		t.Fatalf("imported/skipped = %d/%d, want 0/0", imported, skipped)
	}
	if _, err := db.ListLeaseHistory(context.Background()); err != nil || len(mustRows(t, db)) != 0 {
		t.Fatal("empty input left rows behind")
	}
}

func mustRows(t *testing.T, db *store.DB) []store.LeaseHistoryRow {
	t.Helper()
	rows, err := db.ListLeaseHistory(context.Background())
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("list: %v", err)
	}
	return rows
}

// TestHistoryImportRealFormatAndFallbacks: a real-shaped line (syslog
// identifier and the log package's stamp before the marker, no
// memory_mib on the end line) imports with the create line's memory; a
// %q-escaped reason unquotes; a line with op= and lease_id= but no
// "lease journal: " marker is not a lease line.
func TestHistoryImportRealFormatAndFallbacks(t *testing.T) {
	db := historyTestDB(t)
	ctx := context.Background()
	in := `2026-10-09T04:12:09.993851-07:00 sandbox spoond[153401]: 2026/10/09 04:12:09 lease journal: op=create lease_id=r-1 owner=u-a sandbox=s image=py-base memory_mib=2048 class=guaranteed reason=new
2026-10-09T04:12:10.064145-07:00 sandbox spoond[153401]: 2026/10/09 04:12:10 lease journal: op=release lease_id=r-1 owner=u-a sandbox=s image=py-base class=guaranteed reason="line one\nline two"
2026-10-09T04:12:11.000000-07:00 sandbox other[1]: op=create lease_id=x-1 owner=u-a image=py-base
2026-10-09T04:12:12.000000-07:00 sandbox other[1]: op=release lease_id=x-1 owner=u-a image=py-base
`
	imported, _, err := importHistory(ctx, db, strings.NewReader(in))
	if err != nil || imported != 1 {
		t.Fatalf("import = %d, %v; want 1 (r-1 only)", imported, err)
	}
	h, err := db.GetLeaseHistory(ctx, "r-1")
	if err != nil {
		t.Fatalf("get r-1: %v", err)
	}
	if h.MemoryMB != 2048 || h.Reason != "line one\nline two" || h.Outcome != "released" {
		t.Fatalf("r-1 = memory %d reason %q outcome %q", h.MemoryMB, h.Reason, h.Outcome)
	}
}
