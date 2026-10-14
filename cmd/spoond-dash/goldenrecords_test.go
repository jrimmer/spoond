package spoonddash

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// goldenNow is the frame the records goldens compute against: two days
// and change after the fixture's newest loss, so "no losses for" reads
// a whole number.
var goldenNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// goldenHistory is the lease_history rows the records goldens draw:
// enough history for every record to have a value — four
// honey-go-worker leases at once on 10/11 (busiest, one image and the
// fleet), a 21h 19m run, a 2s run, a 3994 MiB (3.9 GiB) peak, a lost
// lease on 10/12 3:15a, a 5 h nap and a 41 ms start. The excluded
// conformance owner's leases (more of everything: a 9000 MiB peak, a
// 10 h nap, a 9 ms start, a newer loss) must count nowhere.
func goldenHistory() []recLease {
	day := func(d, h, m int) time.Time { // Pacific wall clock
		return time.Date(2026, 10, d, h, m, 0, 0, pacific)
	}
	lease := func(owner, image string, memMiB int, created, ended time.Time, outcome string, peak, startMs, napSecs int64) recLease {
		return recLease{Owner: owner, Image: image, MemoryMB: memMiB,
			Created: created, Ended: ended, Lost: outcome == "lost",
			PeakMiB: peak, StartMs: startMs, MaxNapSecs: napSecs}
	}
	return []recLease{
		// Four honey-go-worker leases at once, 10/11 10:30a–12:00p: the
		// busiest hour, the one-image record and honey's fleet. None of
		// them runs longer than the 21h 19m lease below.
		lease("honey", "honey-go-worker", 1024, day(10, 18, 0), day(11, 13, 0), "released", 2048, 0, 0),
		lease("honey", "honey-go-worker", 1024, day(11, 9, 0), day(11, 13, 0), "released", 0, 0, 0),
		lease("honey", "honey-go-worker", 1024, day(11, 10, 0), day(12, 6, 0), "released", 0, 0, 0),
		lease("honey", "honey-go-worker", 1024, day(11, 10, 30), day(11, 12, 0), "released", 0, 0, 0),
		// Three py-base leases at once on 10/9 3:00p: the other candidate
		// for one image — go-worker's four beats it a day later.
		lease("honey", "py-base", 512, day(9, 15, 0), day(9, 18, 0), "released", 0, 0, 0),
		lease("honey", "py-base", 512, day(9, 15, 0), day(9, 17, 0), "released", 0, 0, 0),
		lease("honey", "py-base", 512, day(9, 15, 0), day(9, 16, 0), "released", 0, 0, 0),
		// The long, the short and the heavy.
		lease("honey", "honey-go-worker", 1024, day(10, 8, 0), day(11, 5, 19), "released", 0, 0, 0),
		lease("honey", "py-base", 512, day(9, 16, 0), day(9, 16, 0).Add(2*time.Second), "released", 0, 0, 0),
		lease("honey", "honey-go-worker", 2048, day(12, 16, 0), day(13, 12, 26), "released", 3994, 0, 0),
		// A loss on 10/12 3:15a, a 5 h nap, a 41 ms start on 10/13.
		lease("honey", "go-base", 1024, day(12, 2, 0), day(12, 3, 15), "lost", 0, 0, 0),
		lease("honey", "honey-go-worker", 1024, day(11, 7, 0), day(11, 9, 0), "released", 0, 0, 5*3600),
		lease("honey", "honey-go-worker", 1024, day(13, 10, 0), day(13, 11, 0), "released", 0, 41, 0),
		// The conformance suite's leases: bigger numbers everywhere, and
		// a loss newer than honey's. Excluded owners count nowhere.
		lease("conformance", "go-base", 4096, day(10, 1, 0), day(13, 23, 0), "released", 9000, 9, 36000),
		lease("conformance", "go-base", 4096, day(10, 1, 0), day(13, 23, 30), "released", 9000, 9, 36000),
		lease("conformance", "go-base", 4096, day(10, 1, 0), day(14, 1, 0), "lost", 0, 0, 0),
	}
}

// TestGoldenRecordsValues pins the twelve records' values for the
// golden history, so the golden frames below can be read against this
// (and so a compute change that shifts a golden names itself here).
func TestGoldenRecordsValues(t *testing.T) {
	r := computeRecords(excludeOwners(goldenHistory()), goldenNow)
	if r.BusiestN != 4 || !r.BusiestAt.Equal(time.Date(2026, 10, 11, 10, 30, 0, 0, pacific)) {
		t.Errorf("busiest = %d at %v, want 4 at 10/11 10:30a", r.BusiestN, r.BusiestAt)
	}
	if r.OneImageImg != "honey-go-worker" || r.OneImageN != 4 {
		t.Errorf("one image = %s ×%d, want honey-go-worker ×4", r.OneImageImg, r.OneImageN)
	}
	if r.LongestDur != 21*time.Hour+19*time.Minute || r.LongestImg != "honey-go-worker" {
		t.Errorf("longest = %v %s, want 21h19m honey-go-worker", r.LongestDur, r.LongestImg)
	}
	if r.ShortestDur != 2*time.Second || r.ShortestImg != "py-base" {
		t.Errorf("shortest = %v %s, want 2s py-base", r.ShortestDur, r.ShortestImg)
	}
	if r.TopOwner != "honey" || r.TopOwnerN != 13 {
		t.Errorf("top owner = %s · %d, want honey · 13", r.TopOwner, r.TopOwnerN)
	}
	if r.LifetimeN != 13 {
		t.Errorf("lifetime = %d leases, want 13", r.LifetimeN)
	}
	// Four created on each of Pacific 10/9 and 10/11: the tie keeps the
	// earlier day.
	if r.BusyDayN != 4 {
		t.Errorf("busiest day = %d, want 4 (10/9 and 10/11 tie, the earlier wins)", r.BusyDayN)
	}
	if got, want := r.BusyDay.In(pacific).Format("2006-01-02"), "2026-10-09"; got != want {
		t.Errorf("busiest day = %s, want %s", got, want)
	}
	if r.FleetOwner != "honey" || r.FleetN != 4 {
		t.Errorf("fleet = %s ×%d, want honey ×4", r.FleetOwner, r.FleetN)
	}
	if r.MostMemMiB != 3994 || r.MostMemImg != "honey-go-worker" {
		t.Errorf("most memory = %d %s, want 3994 honey-go-worker (the excluded 9000 out)", r.MostMemMiB, r.MostMemImg)
	}
	if r.NoLossDays != 2 || !r.LastLoss.Equal(time.Date(2026, 10, 12, 3, 15, 0, 0, pacific)) {
		t.Errorf("no losses = %d days, last %v; want 2 days from the 10/12 3:15a loss (conformance's newer one out)", r.NoLossDays, r.LastLoss)
	}
	if r.LongestNap != 5*time.Hour || r.LongestNapImg != "honey-go-worker" {
		t.Errorf("nap = %v %s, want 5h honey-go-worker (the excluded 10 h out)", r.LongestNap, r.LongestNapImg)
	}
	if r.FastestMs != 41 || r.FastestImg != "honey-go-worker" {
		t.Errorf("fastest = %d ms %s, want 41 ms honey-go-worker (the excluded 9 ms out)", r.FastestMs, r.FastestImg)
	}
	// Lifetime totals: 96.5 lease-hours and 113.9 GiB·h over the
	// thirteen kept leases.
	if diff(r.LifetimeHrs, 96.5006) || diff(r.LifetimeGiBH, 113.9336) {
		t.Errorf("lifetime totals = %v h, %v GiB·h; want ≈96.5 h, ≈113.9 GiB·h", r.LifetimeHrs, r.LifetimeGiBH)
	}
}

// goldenRecordsSnapshot is the sample frame's snapshot with a computed
// records value attached: the panels above draw exactly as the other
// golden does, so the diff is the records panel itself.
func goldenRecordsSnapshot() Snapshot {
	s := sampleSnapshot()
	s.Records = computeRecords(excludeOwners(goldenHistory()), goldenNow)
	return s
}

// TestGoldenRecords compares the records panel against the golden files
// at the default and the minimum width (go test -update rewrites). The
// fixture's history gives every record a value; TestGoldenPlain pins
// the no-history frame.
func TestGoldenRecords(t *testing.T) {
	for _, tc := range []struct {
		w    int
		path string
	}{
		{DefaultWidth, filepath.Join("testdata", "records-104.txt")},
		{minW, filepath.Join("testdata", "records-72.txt")},
	} {
		got, err := drawFrame(goldenRecordsSnapshot(), sampleHist(), tc.w, "spoond.example.com", fixedNow, dashInterval)
		if err != nil {
			t.Fatalf("width %d: %v", tc.w, err)
		}
		text := got.Plain()
		if update {
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(tc.path, []byte(text), 0o644); err != nil {
				t.Fatalf("rewrite %s: %v", tc.path, err)
			}
			continue
		}
		want, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatalf("read golden %s (run go test ./cmd/spoond-dash -update to write it): %v", tc.path, err)
		}
		if string(want) != text {
			t.Errorf("%s differs; run go test ./cmd/spoond-dash -update after checking the diff\n--- golden ---\n%s\n--- got ---\n%s",
				tc.path, want, text)
		}
	}
}
