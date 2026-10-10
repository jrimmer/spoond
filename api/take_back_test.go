package api

import (
	"testing"
	"time"
)

func tbL(id string, mem int, age time.Duration) takeBackLease {
	return takeBackLease{ID: id, MemoryMiB: mem, LastActive: time.Unix(1_000_000, 0).Add(-age)}
}

func ids(vs []takeBackVictim) []string {
	var out []string
	for _, v := range vs {
		out = append(out, v.LeaseID)
	}
	return out
}

func eqStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestMemVictimsInsideSliceNeverChosen(t *testing.T) {
	owners := []takeBackOwner{
		{Owner: "req", UsedMiB: 0, SliceMiB: 8192},
		{Owner: "in", UsedMiB: 8192, SliceMiB: 8192, Leases: []takeBackLease{tbL("a", 4096, time.Hour)}},
	}
	if got := memVictims(owners, "req", 1024); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func TestMemVictimsRatioNotAbsolute(t *testing.T) {
	owners := []takeBackOwner{
		{Owner: "req", UsedMiB: 0, SliceMiB: 8192},
		{Owner: "honey", UsedMiB: 28672, SliceMiB: 12288, Leases: []takeBackLease{tbL("h1", 4096, time.Hour)}},
		{Owner: "pool", UsedMiB: 20480, SliceMiB: 8192, Leases: []takeBackLease{tbL("p1", 4096, time.Hour)}},
	}
	got := memVictims(owners, "req", 4096)
	if !eqStrs(ids(got), []string{"p1"}) {
		t.Fatalf("got %v, want [p1]", ids(got))
	}
	if got[0].Owner != "pool" || got[0].Ratio != 2.5 || got[0].MemoryMiB != 4096 {
		t.Fatalf("bad victim %+v", got[0])
	}
}

func TestMemVictimsLRUWithinOwner(t *testing.T) {
	owners := []takeBackOwner{
		{Owner: "req", SliceMiB: 8192},
		{Owner: "o", UsedMiB: 16384, SliceMiB: 8192, Leases: []takeBackLease{
			tbL("new", 4096, time.Minute), tbL("old", 4096, 3*time.Hour), tbL("mid", 4096, time.Hour), tbL("x", 4096, time.Minute),
		}},
	}
	got := memVictims(owners, "req", 4096)
	if !eqStrs(ids(got), []string{"old"}) {
		t.Fatalf("got %v, want [old]", ids(got))
	}
}

func TestMemVictimsBusyAfterIdle(t *testing.T) {
	busy := tbL("busy", 4096, 5*time.Hour)
	busy.Busy = true
	owners := []takeBackOwner{
		{Owner: "req", SliceMiB: 8192},
		{Owner: "o", UsedMiB: 16384, SliceMiB: 8192, Leases: []takeBackLease{busy, tbL("idle", 4096, time.Minute), tbL("k", 4096, time.Minute)}},
	}
	got := memVictims(owners, "req", 4096)
	if !eqStrs(ids(got), []string{"idle"}) {
		t.Fatalf("got %v, want [idle]", ids(got))
	}
}

func TestMemVictimsPinnedNever(t *testing.T) {
	pin := tbL("pin", 4096, 9*time.Hour)
	pin.Pinned = true
	owners := []takeBackOwner{
		{Owner: "req", SliceMiB: 8192},
		{Owner: "o", UsedMiB: 12288, SliceMiB: 8192, Leases: []takeBackLease{pin, tbL("free", 4096, time.Minute), tbL("k", 4096, time.Minute)}},
	}
	got := memVictims(owners, "req", 4096)
	if !eqStrs(ids(got), []string{"free"}) {
		t.Fatalf("got %v, want [free]", ids(got))
	}
	owners[1].Leases = []takeBackLease{pin}
	if got := memVictims(owners, "req", 4096); got != nil {
		t.Fatalf("only pinned: got %v, want nil", got)
	}
}

func TestMemVictimsRequesterFurtherOverGetsNil(t *testing.T) {
	owners := []takeBackOwner{
		{Owner: "req", UsedMiB: 16384, SliceMiB: 8192},
		{Owner: "o", UsedMiB: 12288, SliceMiB: 8192, Leases: []takeBackLease{tbL("a", 4096, time.Hour), tbL("b", 4096, time.Hour), tbL("c", 4096, time.Hour)}},
	}
	if got := memVictims(owners, "req", 4096); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func TestMemVictimsStopsOnceNeedFits(t *testing.T) {
	owners := []takeBackOwner{
		{Owner: "req", SliceMiB: 8192},
		{Owner: "o", UsedMiB: 24576, SliceMiB: 8192, Leases: []takeBackLease{
			tbL("a", 4096, 4*time.Hour), tbL("b", 4096, 3*time.Hour), tbL("c", 4096, 2*time.Hour), tbL("d", 4096, time.Hour),
		}},
	}
	got := memVictims(owners, "req", 6000)
	if !eqStrs(ids(got), []string{"a", "b"}) {
		t.Fatalf("got %v, want [a b]", ids(got))
	}
	// Caller's view is untouched.
	if owners[1].UsedMiB != 24576 || len(owners[1].Leases) != 4 {
		t.Fatalf("input mutated: %+v", owners[1])
	}
}

func TestMemVictimsZeroSliceSafe(t *testing.T) {
	owners := []takeBackOwner{
		{Owner: "req"},
		{Owner: "o", UsedMiB: 4096, SliceMiB: 0, Leases: []takeBackLease{tbL("a", 4096, time.Hour)}},
	}
	if got := memVictims(owners, "req", 1024); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

// TestTakeBackPauseGuardRace: takeBackPause re-checks running, unpinned and
// not-busy in the same critical section where it sets busy. The race to
// cover: after a victim is selected, the owner pins the lease (or a job
// starts, or the lease is released or paused) before takeBackPause runs;
// the pause must then be refused, nothing paused, no event emitted. Run
// the pin and the pause concurrently under -race and assert that a lease
// pinned first is never paused.
func TestTakeBackPauseGuardRace(t *testing.T) {
	t.Skip("TODO(FS2a step 2): guard race")
}
