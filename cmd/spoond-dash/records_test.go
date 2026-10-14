package spoonddash

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v3/store"
)

// recordsFixtureDB creates a catalog with the lease_history table and
// the leases columns the records panel reads, as the plan's Part A
// migration will. Part A is a separate change, so the fixture applies
// its SQL by hand; the panel must treat an older backend's DB (no
// lease_history) as "no history yet".
func recordsFixtureDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	for _, q := range []string{
		// From the plan's 0029_lease_history.sql.
		`CREATE TABLE lease_history (
			lease_id     TEXT PRIMARY KEY,
			owner        TEXT NOT NULL,
			image        TEXT NOT NULL,
			memory_mb    INTEGER NOT NULL DEFAULT 0,
			created_at   TEXT NOT NULL,
			ended_at     TEXT NOT NULL,
			outcome      TEXT NOT NULL CHECK (outcome IN ('released','lost')),
			reason       TEXT NOT NULL DEFAULT '',
			peak_mib     INTEGER NOT NULL DEFAULT 0,
			start_ms     INTEGER NOT NULL DEFAULT 0,
			start_kind   TEXT NOT NULL DEFAULT '',
			max_nap_secs INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX lease_history_ended ON lease_history(ended_at)`,
		// start_ms and max_nap_secs are new with the plan's migration;
		// suspended_at already arrived with 0021_lease_suspend.
		`ALTER TABLE leases ADD COLUMN start_ms INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE leases ADD COLUMN max_nap_secs INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return raw
}

// recFixture is a records test's fixture: a catalog DB with the history
// schema, an identity store and a collector pointing at them. Clock
// fixed at 2026-10-14 12:00 UTC so ages and days are exact.
type recFixture struct {
	c   *collector
	raw *sql.DB
	now time.Time
}

// newRecFixture builds the fixture and registers its cleanup.
func newRecFixture(t *testing.T) *recFixture {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{
		DBPath:      filepath.Join(dir, "spoond.db"),
		UsersFile:   filepath.Join(dir, "users.json"),
		StoragePath: dir,
	}
	f := &recFixture{raw: recordsFixtureDB(t, cfg.DBPath), now: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	if err := os.WriteFile(cfg.UsersFile, []byte(`{"users":[{"id":"u-honey","name":"honey"},{"id":"u-conf","name":"conformance"},{"id":"u-agent","name":"agent"},{"id":"u-beta","name":"beta"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f.c = newCollector(cfg)
	return f
}

// addHist inserts one lease_history row, RFC 3339 UTC like the store
// writes.
func (f *recFixture) addHist(t *testing.T, leaseID, owner, image string, memMiB int, created, ended time.Time, outcome string, peak, startMs, napSecs int64) {
	t.Helper()
	_, err := f.raw.Exec(`INSERT INTO lease_history (lease_id, owner, image, memory_mb, created_at, ended_at, outcome, peak_mib, start_ms, max_nap_secs)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		leaseID, owner, image, memMiB, created.Format(time.RFC3339Nano), ended.Format(time.RFC3339Nano),
		outcome, peak, startMs, napSecs)
	if err != nil {
		t.Fatal(err)
	}
}

// addLive inserts one live leases row (the columns the panel's read
// needs, at their NOT NULL defaults otherwise).
func (f *recFixture) addLive(t *testing.T, id, owner, image, state string, memMiB int, created time.Time) {
	t.Helper()
	_, err := f.raw.Exec(`INSERT INTO leases (id, owner, image, created_at, expires_at, last_active, state, memory_mb)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, owner, image, created.Format(time.RFC3339Nano),
		f.now.Add(time.Hour).Format(time.RFC3339Nano), f.now.Format(time.RFC3339Nano), state, memMiB)
	if err != nil {
		t.Fatal(err)
	}
}

// computed runs one records computation over the fixture's rows.
func (f *recFixture) computed(t *testing.T) records {
	t.Helper()
	rec, _, ok := f.c.collectRecords(context.Background(), f.now)
	if !ok {
		t.Fatal("collectRecords failed")
	}
	return rec
}

// recHistoryTableMissing: an older backend's catalog (no lease_history)
// reads as no history, not as an error — the plan's "no history yet".
func TestRecHistoryTableMissing(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DBPath: filepath.Join(dir, "spoond.db"), UsersFile: filepath.Join(dir, "users.json"), StoragePath: dir}
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	c := newCollector(cfg)
	_, avail, ok := c.collectRecords(context.Background(), time.Now())
	if !ok || avail {
		t.Fatalf("avail=%v ok=%v, want no table with the read succeeding", avail, ok)
	}
}

// TestRecBusiest: the most leases running at once, and when it first
// reached that. A lease released exactly as another starts counts only
// the new one (ends sort before starts at equal times).
func TestRecBusiest(t *testing.T) {
	f := newRecFixture(t)
	day := func(d int, h, m int) time.Time {
		return time.Date(2026, 10, 10+d, h, m, 0, 0, time.UTC)
	}
	// Three overlapping on 10/11 at 09:00 — a fourth starts exactly as
	// the first ends (10:00): ends before starts keeps the count at 3.
	f.addHist(t, "a", "u-honey", "go-base", 1024, day(0, 9, 0), day(1, 10, 0), "released", 0, 0, 0)
	f.addHist(t, "b", "u-honey", "go-base", 1024, day(1, 9, 0), day(1, 12, 0), "released", 0, 0, 0)
	f.addHist(t, "c", "u-honey", "go-base", 1024, day(1, 9, 30), day(2, 9, 0), "released", 0, 0, 0)
	f.addHist(t, "d", "u-honey", "py-base", 1024, day(1, 10, 0), day(1, 11, 0), "released", 0, 0, 0)
	r := f.computed(t)
	if r.BusiestN != 3 {
		t.Fatalf("busiest = %d, want 3", r.BusiestN)
	}
	if want := day(1, 9, 30); !r.BusiestAt.Equal(want) {
		t.Fatalf("busiest at %v, want %v (when it first reached 3)", r.BusiestAt, want)
	}
}

// TestRecOneImage: the most leases of one image at once, its name and
// when — other images running beside it do not compete.
func TestRecOneImage(t *testing.T) {
	f := newRecFixture(t)
	at := time.Date(2026, 10, 11, 8, 0, 0, 0, time.UTC)
	// go-base peaks at 2; py-base at 3, an hour later.
	f.addHist(t, "g1", "u-honey", "go-base", 512, at, at.Add(2*time.Hour), "released", 0, 0, 0)
	f.addHist(t, "g2", "u-honey", "go-base", 512, at, at.Add(time.Hour), "released", 0, 0, 0)
	py := at.Add(time.Hour)
	f.addHist(t, "p1", "u-honey", "py-base", 512, py, py.Add(3*time.Hour), "released", 0, 0, 0)
	f.addHist(t, "p2", "u-honey", "py-base", 512, py, py.Add(2*time.Hour), "released", 0, 0, 0)
	f.addHist(t, "p3", "u-honey", "py-base", 512, py, py.Add(time.Hour), "released", 0, 0, 0)
	r := f.computed(t)
	if r.OneImageImg != "py-base" || r.OneImageN != 3 {
		t.Fatalf("one image = %s ×%d, want py-base ×3", r.OneImageImg, r.OneImageN)
	}
	if !r.OneImageAt.Equal(py) {
		t.Fatalf("one image at %v, want %v", r.OneImageAt, py)
	}
}

// TestRecLongestRun: the longest ended_at − created_at, with its image
// and start date. A live lease (still running at the frame's now) wins
// over every ended one.
func TestRecLongestRun(t *testing.T) {
	f := newRecFixture(t)
	start := time.Date(2026, 10, 10, 6, 0, 0, 0, time.UTC)
	f.addHist(t, "a", "u-honey", "go-base", 1024, start, start.Add(21*time.Hour+19*time.Minute), "released", 0, 0, 0)
	f.addHist(t, "b", "u-honey", "py-base", 1024, start, start.Add(5*time.Hour), "released", 0, 0, 0)
	// A live lease started 30 h before the frame's now: still running,
	// so its run is the longest and its end is now.
	f.addLive(t, "live-1", "u-honey", "go-base", "running", 1024, f.now.Add(-30*time.Hour))
	r := f.computed(t)
	if r.LongestDur != 30*time.Hour {
		t.Fatalf("longest = %v, want the live lease's 30h", r.LongestDur)
	}
	if r.LongestImg != "go-base" || !r.LongestEnd.Equal(f.now) {
		t.Fatalf("longest row = %s %v, want go-base ending now", r.LongestImg, r.LongestEnd)
	}
}

// TestRecShortestRun: the shortest released lease that ran at least a
// second — lost leases and sub-second runs never qualify.
func TestRecShortestRun(t *testing.T) {
	f := newRecFixture(t)
	at := time.Date(2026, 10, 11, 7, 0, 0, 0, time.UTC)
	f.addHist(t, "a", "u-honey", "go-base", 512, at, at.Add(2*time.Second), "released", 0, 0, 0)
	f.addHist(t, "b", "u-honey", "py-base", 512, at, at.Add(time.Second+500*time.Millisecond), "released", 0, 0, 0)
	// Lost, though only a second: excluded from the record.
	f.addHist(t, "c", "u-honey", "go-base", 512, at, at.Add(time.Second), "lost", 0, 0, 0)
	// Released but under a second: excluded too.
	f.addHist(t, "d", "u-honey", "go-base", 512, at, at.Add(800*time.Millisecond), "released", 0, 0, 0)
	r := f.computed(t)
	if r.ShortestDur != 1500*time.Millisecond || r.ShortestImg != "py-base" {
		t.Fatalf("shortest = %v %s, want 1.5s py-base", r.ShortestDur, r.ShortestImg)
	}
}

// TestRecTopOwner: the owner with the most leases, by name — an
// owner id is counted under its identity-store name, and the excluded
// test owner's leases count nowhere.
func TestRecTopOwner(t *testing.T) {
	f := newRecFixture(t)
	at := time.Date(2026, 10, 11, 7, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		f.addHist(t, fmt.Sprintf("h%d", i), "u-honey", "go-base", 512, at.Add(time.Duration(i)*time.Minute), at.Add(time.Hour), "released", 0, 0, 0)
	}
	for i := 0; i < 9; i++ {
		f.addHist(t, fmt.Sprintf("c%d", i), "u-conf", "go-base", 512, at, at.Add(time.Hour), "released", 0, 0, 0)
	}
	f.addHist(t, "x1", "legacy-consumer", "go-base", 512, at, at.Add(time.Hour), "released", 0, 0, 0)
	r := f.computed(t)
	if r.TopOwner != "honey" || r.TopOwnerN != 4 {
		t.Fatalf("top owner = %s · %d, want honey · 4 (conformance's 9 and the legacy name excluded)", r.TopOwner, r.TopOwnerN)
	}
}

// TestRecLifetime: lease count, total lease-hours and total memory
// GiB-hours (memory_mb × hours / 1024) over history plus live leases.
func TestRecLifetime(t *testing.T) {
	f := newRecFixture(t)
	at := time.Date(2026, 10, 11, 6, 0, 0, 0, time.UTC)
	// 2 GiB for 2 h = 4 GiB·h; 1 GiB for 1 h = 1 GiB·h.
	f.addHist(t, "a", "u-honey", "go-base", 2048, at, at.Add(2*time.Hour), "released", 0, 0, 0)
	f.addHist(t, "b", "u-honey", "go-base", 1024, at, at.Add(time.Hour), "released", 0, 0, 0)
	// A live lease: 1 GiB held for 1 h at the frame's now.
	f.addLive(t, "live-1", "u-honey", "py-base", "running", 1024, f.now.Add(-time.Hour))
	r := f.computed(t)
	if r.LifetimeN != 3 {
		t.Fatalf("lifetime leases = %d, want 3 (live included)", r.LifetimeN)
	}
	if want := 2 + 1 + 1.0; diff(r.LifetimeHrs, want) {
		t.Fatalf("lifetime hours = %v, want %v", r.LifetimeHrs, want)
	}
	if want := 4 + 1 + 1.0; diff(r.LifetimeGiBH, want) {
		t.Fatalf("lifetime GiB·h = %v, want %v", r.LifetimeGiBH, want)
	}
}

// diff reports whether got is outside ±0.01 of want (the lifetime
// totals are decimal expectations, not exact floats).
func diff(got, want float64) bool { return got < want-0.01 || got > want+0.01 }

// TestRecBusiestDay: the most leases created on one Pacific calendar
// day — UTC evenings are the next day in Pacific, so the zone matters.
func TestRecBusiestDay(t *testing.T) {
	f := newRecFixture(t)
	// 2026-10-11 23:30 UTC is 10/11 4:30p Pacific; 10/12 02:00 UTC is
	// already 10/11 7:00p Pacific. Three leases on Pacific 10/11, two
	// on 10/12.
	f.addHist(t, "a", "u-honey", "go-base", 512, time.Date(2026, 10, 11, 23, 30, 0, 0, time.UTC), f.now, "released", 0, 0, 0)
	f.addHist(t, "b", "u-honey", "go-base", 512, time.Date(2026, 10, 12, 2, 0, 0, 0, time.UTC), f.now, "released", 0, 0, 0)
	f.addHist(t, "c", "u-honey", "go-base", 512, time.Date(2026, 10, 12, 2, 30, 0, 0, time.UTC), f.now, "released", 0, 0, 0)
	f.addHist(t, "d", "u-honey", "go-base", 512, time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC), f.now, "released", 0, 0, 0)
	f.addHist(t, "e", "u-honey", "go-base", 512, time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC), f.now, "released", 0, 0, 0)
	r := f.computed(t)
	if r.BusyDayN != 3 {
		t.Fatalf("busiest day = %d leases, want 3", r.BusyDayN)
	}
	if got, want := r.BusyDay.In(pacific).Format("2006-01-02"), "2026-10-11"; got != want {
		t.Fatalf("busiest day = %s, want %s", got, want)
	}
}

// TestRecBiggestFleet: the most leases one owner had running at once —
// two owners overlapping do not pool.
func TestRecBiggestFleet(t *testing.T) {
	f := newRecFixture(t)
	at := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	f.addHist(t, "a", "u-honey", "go-base", 512, at, at.Add(3*time.Hour), "released", 0, 0, 0)
	f.addHist(t, "b", "u-honey", "go-base", 512, at.Add(30*time.Minute), at.Add(2*time.Hour), "released", 0, 0, 0)
	other := at.Add(15 * time.Minute)
	f.addHist(t, "c", "u-agent", "go-base", 512, other, other.Add(time.Hour), "released", 0, 0, 0)
	f.addHist(t, "d", "u-agent", "go-base", 512, other, other.Add(2*time.Hour), "released", 0, 0, 0)
	r := f.computed(t)
	// Both owners reach 2; agent gets there first (9:15 before 9:30).
	if r.FleetOwner != "agent" || r.FleetN != 2 {
		t.Fatalf("fleet = %s ×%d, want agent ×2", r.FleetOwner, r.FleetN)
	}
	if !r.FleetAt.Equal(other) {
		t.Fatalf("fleet at %v, want %v (when agent first reached 2)", r.FleetAt, other)
	}
}

// TestRecMostMemory: the largest peak_mib, with its image and end date;
// a peak of 0 (never measured) never wins.
func TestRecMostMemory(t *testing.T) {
	f := newRecFixture(t)
	at := time.Date(2026, 10, 12, 3, 0, 0, 0, time.UTC)
	f.addHist(t, "a", "u-honey", "go-base", 2048, at, at.Add(time.Hour), "released", 3950, 0, 0)
	f.addHist(t, "b", "u-honey", "py-base", 1024, at, at.Add(2*time.Hour), "released", 6200, 0, 0)
	f.addHist(t, "c", "u-honey", "py-base", 1024, at, at.Add(3*time.Hour), "released", 0, 0, 0)
	r := f.computed(t)
	if r.MostMemMiB != 6200 || r.MostMemImg != "py-base" {
		t.Fatalf("most memory = %d %s, want 6200 py-base", r.MostMemMiB, r.MostMemImg)
	}
	if !r.MostMemEnd.Equal(at.Add(2 * time.Hour)) {
		t.Fatalf("most memory end = %v, want %v", r.MostMemEnd, at.Add(2*time.Hour))
	}
}

// TestRecNoLosses: whole days since the newest lost row, with its date
// and time; a history with no lost row means "no losses yet".
func TestRecNoLosses(t *testing.T) {
	f := newRecFixture(t)
	at := time.Date(2026, 10, 12, 3, 15, 0, 0, time.UTC)
	f.addHist(t, "a", "u-honey", "go-base", 512, at.Add(-time.Hour), at, "lost", 0, 0, 0)
	// An older loss does not move the newest.
	f.addHist(t, "b", "u-honey", "go-base", 512, at.Add(-48*time.Hour), at.Add(-47*time.Hour), "lost", 0, 0, 0)
	r := f.computed(t)
	if !r.LastLoss.Equal(at) {
		t.Fatalf("last loss = %v, want %v", r.LastLoss, at)
	}
	// 10/12 3:15a → 10/14 12:00 is 2 whole days (8 h short of 3).
	if r.NoLossDays != 2 {
		t.Fatalf("no losses = %d days, want 2", r.NoLossDays)
	}

	// No lost row at all: LastLoss zero, and the drawn value the
	// plan's "no losses yet".
	f2 := newRecFixture(t)
	f2.addHist(t, "a", "u-honey", "go-base", 512, at, at.Add(time.Hour), "released", 0, 0, 0)
	r2 := f2.computed(t)
	if !r2.LastLoss.IsZero() || r2.NoLossDays != 0 {
		t.Fatalf("no-loss history = %v/%d, want zero", r2.LastLoss, r2.NoLossDays)
	}
	if got := recordValues(r2)[3][1].value; got != recNoLossYet {
		t.Fatalf("no-loss value = %q, want %q", got, recNoLossYet)
	}
}

// TestRecLongestNap: the largest max_nap_secs with its image; a
// history with no nap over zero draws the "counting from 3.8" place-
// holder instead.
func TestRecLongestNap(t *testing.T) {
	f := newRecFixture(t)
	at := time.Date(2026, 10, 12, 3, 0, 0, 0, time.UTC)
	f.addHist(t, "a", "u-honey", "go-base", 512, at, at.Add(time.Hour), "released", 0, 0, 0)
	f.addHist(t, "b", "u-honey", "py-base", 512, at, at.Add(2*time.Hour), "released", 0, 0, 4*3600)
	f.addHist(t, "c", "u-honey", "py-base", 512, at, at.Add(3*time.Hour), "released", 0, 0, 90)
	r := f.computed(t)
	if r.LongestNap != 4*time.Hour || r.LongestNapImg != "py-base" {
		t.Fatalf("longest nap = %v %s, want 4h py-base", r.LongestNap, r.LongestNapImg)
	}

	// No nap recorded: the 3.8 place-holder text.
	f2 := newRecFixture(t)
	f2.addHist(t, "a", "u-honey", "go-base", 512, at, at.Add(time.Hour), "released", 0, 0, 0)
	r2 := f2.computed(t)
	if r2.LongestNap != 0 || r2.LongestNapImg != "" {
		t.Fatalf("napless history = %v %s, want zero", r2.LongestNap, r2.LongestNapImg)
	}
}

// TestRecFastestStart: the smallest start_ms over zero, with image and
// end date; start_ms of 0 (unknown, pre-3.8) never counts, and a
// history with none draws the place-holder.
func TestRecFastestStart(t *testing.T) {
	f := newRecFixture(t)
	at := time.Date(2026, 10, 13, 3, 0, 0, 0, time.UTC)
	f.addHist(t, "a", "u-honey", "go-base", 512, at, at.Add(time.Hour), "released", 0, 41000, 0)
	f.addHist(t, "b", "u-honey", "py-base", 512, at, at.Add(2*time.Hour), "released", 0, 280, 0)
	f.addHist(t, "c", "u-honey", "py-base", 512, at, at.Add(3*time.Hour), "released", 0, 0, 0)
	r := f.computed(t)
	if r.FastestMs != 280 || r.FastestImg != "py-base" {
		t.Fatalf("fastest start = %d %s, want 280 py-base", r.FastestMs, r.FastestImg)
	}
	if !r.FastestEnd.Equal(at.Add(2 * time.Hour)) {
		t.Fatalf("fastest end = %v, want %v", r.FastestEnd, at.Add(2*time.Hour))
	}

	f2 := newRecFixture(t)
	f2.addHist(t, "a", "u-honey", "go-base", 512, at, at.Add(time.Hour), "released", 0, 0, 0)
	r2 := f2.computed(t)
	if r2.FastestMs != 0 {
		t.Fatalf("startless history = %d, want 0 (the place-holder)", r2.FastestMs)
	}
}

// TestRecTieBreaks: every tie keeps the earlier value — an earlier
// time for the day record, the alphabetically first owner for top
// owner, and the first-read row for longest run.
func TestRecTieBreaks(t *testing.T) {
	f := newRecFixture(t)
	day1 := time.Date(2026, 10, 11, 20, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	// Two leases on each of two Pacific days: the earlier day wins.
	f.addHist(t, "a", "u-honey", "go-base", 512, day1, f.now, "released", 0, 0, 0)
	f.addHist(t, "b", "u-honey", "go-base", 512, day1.Add(time.Hour), f.now, "released", 0, 0, 0)
	f.addHist(t, "c", "u-agent", "go-base", 512, day2, f.now, "released", 0, 0, 0)
	f.addHist(t, "d", "u-agent", "go-base", 512, day2.Add(time.Hour), f.now, "released", 0, 0, 0)
	// Two runs of the same length from a third owner; the first read
	// (the earlier created_at) wins the longest-run tie.
	f.addHist(t, "e", "u-beta", "go-base", 512, day1.Add(10*time.Minute), day1.Add(10*time.Minute+3*time.Hour), "released", 0, 0, 0)
	f.addHist(t, "g", "u-beta", "py-base", 512, day2.Add(10*time.Minute), day2.Add(10*time.Minute+3*time.Hour), "released", 0, 0, 0)
	r := f.computed(t)
	if got, want := r.BusyDay.In(pacific).Format("2006-01-02"), "2026-10-11"; got != want {
		t.Errorf("busiest day = %s, want the earlier %s", got, want)
	}
	// honey and agent tie at 2 each: the alphabetically earlier name.
	if r.TopOwner != "agent" {
		t.Errorf("top owner = %q, want agent", r.TopOwner)
	}
	if r.LongestImg != "go-base" {
		t.Errorf("longest run = %s, want the first-read go-base", r.LongestImg)
	}
}

// TestRecSweepEndsBeforeStarts: the sweep rule — ends before starts at
// equal times — so the fleet count never double-counts the swap.
func TestRecSweepEndsBeforeStarts(t *testing.T) {
	f := newRecFixture(t)
	at := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	swap := at.Add(time.Hour)
	f.addHist(t, "a", "u-honey", "go-base", 512, at, swap, "released", 0, 0, 0)
	f.addHist(t, "b", "u-honey", "go-base", 512, swap, swap.Add(time.Hour), "released", 0, 0, 0)
	f.addHist(t, "c", "u-honey", "go-base", 512, swap, swap.Add(2*time.Hour), "released", 0, 0, 0)
	r := f.computed(t)
	if r.FleetN != 2 {
		t.Fatalf("fleet = %d, want 2 (ends before starts at the swap)", r.FleetN)
	}
}

// TestRecordsForCaches: recordsFor recomputes at most once a minute —
// two calls inside the TTL reuse the cached result even when the
// underlying history changed.
func TestRecordsForCaches(t *testing.T) {
	f := newRecFixture(t)
	at := time.Date(2026, 10, 12, 3, 0, 0, 0, time.UTC)
	f.addHist(t, "a", "u-honey", "go-base", 512, at, at.Add(time.Hour), "released", 0, 0, 0)
	ctx := context.Background()
	r1 := f.c.recordsFor(ctx, f.now)
	if r1.LifetimeN != 1 {
		t.Fatalf("first = %d leases, want 1", r1.LifetimeN)
	}
	// More history lands; the minute has not passed, so the cache holds.
	f.addHist(t, "b", "u-honey", "go-base", 512, at, at.Add(2*time.Hour), "released", 0, 0, 0)
	if r2 := f.c.recordsFor(ctx, f.now.Add(30*time.Second)); r2.LifetimeN != 1 {
		t.Fatalf("cached = %d leases, want the cached 1", r2.LifetimeN)
	}
	if r3 := f.c.recordsFor(ctx, f.now.Add(recordsTTL)); r3.LifetimeN != 2 {
		t.Fatalf("after the TTL = %d leases, want a recompute's 2", r3.LifetimeN)
	}
}

// TestRecLiveLeaseInSweep: a live lease joins the "at once" sweep with
// its end at the frame's now.
func TestRecLiveLeaseInSweep(t *testing.T) {
	f := newRecFixture(t)
	at := f.now.Add(-2 * time.Hour)
	f.addHist(t, "a", "u-honey", "go-base", 512, at, at.Add(time.Hour), "released", 0, 0, 0)
	f.addLive(t, "live-1", "u-honey", "go-base", "running", 512, f.now.Add(-time.Hour))
	f.addLive(t, "live-2", "u-honey", "go-base", "suspended", 512, f.now.Add(-time.Hour))
	r := f.computed(t)
	if r.BusiestN != 2 || !r.BusiestAt.Equal(f.now.Add(-time.Hour)) {
		t.Fatalf("busiest = %d at %v, want 2 at the live leases' start", r.BusiestN, r.BusiestAt)
	}
}

// TestRecLostLiveRowsSkipped: a live row in state 'lost' is already at
// its end and a 'creating' one has not started its run, so neither
// joins the records — while a running live lease beside them does.
func TestRecLostLiveRowsSkipped(t *testing.T) {
	f := newRecFixture(t)
	f.addLive(t, "lost-1", "u-honey", "go-base", "lost", 512, f.now.Add(-2*time.Hour))
	r := f.computed(t)
	if r.LifetimeN != 0 {
		t.Fatalf("lifetime = %d, want 0 (only a lost live row, which is at its end)", r.LifetimeN)
	}
	// The control: a running live lease beside the lost one counts.
	f.addLive(t, "run-1", "u-honey", "go-base", "running", 512, f.now.Add(-time.Hour))
	r = f.computed(t)
	if r.LifetimeN != 1 {
		t.Fatalf("lifetime = %d, want 1 (the running lease)", r.LifetimeN)
	}
}

// TestRecRecoveredLeaseCountsOnce: a lease marked lost gets its history
// row then; if it was recovered it is live again under the same id. It
// counts once — the history row, which carries the loss.
func TestRecRecoveredLeaseCountsOnce(t *testing.T) {
	f := newRecFixture(t)
	created := f.now.Add(-3 * time.Hour)
	lostAt := f.now.Add(-2 * time.Hour)
	f.addHist(t, "rec-1", "u-honey", "go-base", 512, created, lostAt, "lost", 0, 0, 0)
	f.addLive(t, "rec-1", "u-honey", "go-base", "recovered", 512, created)
	r := f.computed(t)
	if r.LifetimeN != 1 {
		t.Fatalf("lifetime = %d, want 1 (the recovered lease once)", r.LifetimeN)
	}
	if !r.LastLoss.Equal(lostAt) {
		t.Fatalf("last loss = %v, want the history row's %v", r.LastLoss, lostAt)
	}
}
