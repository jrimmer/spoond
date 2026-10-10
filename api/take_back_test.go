package api

import (
	"context"
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

// TestTakeBackOwnersViews: takeBackOwners builds one view per fair-share
// owner from the snapshot (UsedMiB, SliceMiB) and lists the owner's
// running, unreleased leases under the store lock — with Pinned, busy
// (an in-flight operation or a running background job) and LastActive
// carried through.
func TestTakeBackOwnersViews(t *testing.T) {
	svc, db, _, ids := newFairShareService(t)
	alice := addIdentityUser(t, ids, "alice")
	bob := addIdentityUser(t, ids, "bob")
	ctx := context.Background()
	seedImage(t, db, "mid", 1024)

	// Alice: two running leases (one pinned, one with a running job).
	a1, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant a1: %v", err)
	}
	if _, err := svc.setPinned(alice.ID, a1.ID, true); err != nil {
		t.Fatalf("pin a1: %v", err)
	}
	a2, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant a2: %v", err)
	}
	// Bob: one running lease plus one suspended (which holds no
	// hugepages and must not be listed).
	b1, err := svc.grantLease(ctx, leaseRequest{owner: bob.ID, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant b1: %v", err)
	}
	b2, err := svc.grantLease(ctx, leaseRequest{owner: bob.ID, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant b2: %v", err)
	}
	if _, err := svc.suspend(ctx, bob.ID, b2.ID); err != nil {
		t.Fatalf("suspend b2: %v", err)
	}
	// A running background job marks a2 busy like an in-flight pause.
	svc.incRunningJob(a2.ID)

	// Warm the node cache so the fair snapshot is computed; each lease
	// shows up in its owner's memory usage through the store read.
	svc.updateNodeMetrics(ctx)
	svc.invalidateFairShares()

	views := svc.takeBackOwners(ctx)
	by := map[string]takeBackOwner{}
	for _, v := range views {
		by[v.Owner] = v
	}
	av, okA := by[alice.ID]
	bv, okB := by[bob.ID]
	if !okA || !okB {
		t.Fatalf("views missing owners: %+v", views)
	}
	if av.SliceMiB != bv.SliceMiB || av.SliceMiB == 0 {
		t.Fatalf("slices = %d/%d, want equal nonzero", av.SliceMiB, bv.SliceMiB)
	}
	// Memory usage from the snapshot: 2048 MiB each (two 1024 leases).
	if av.UsedMiB != 2048 || bv.UsedMiB != 1024 {
		t.Fatalf("used = alice %d bob %d, want 2048/1024", av.UsedMiB, bv.UsedMiB)
	}
	if len(av.Leases) != 2 || len(bv.Leases) != 1 {
		t.Fatalf("leases = alice %d bob %d, want 2/1", len(av.Leases), len(bv.Leases))
	}
	la := map[string]takeBackLease{}
	for _, l := range av.Leases {
		la[l.ID] = l
	}
	if !la[a1.ID].Pinned {
		t.Fatal("a1 should carry Pinned")
	}
	if !la[a2.ID].Busy {
		t.Fatal("a2 should be busy through its running job")
	}
	if bv.Leases[0].ID != b1.ID || bv.Leases[0].Busy || bv.Leases[0].Pinned {
		t.Fatalf("b1 view = %+v, want the plain running lease", bv.Leases[0])
	}
	if bv.Leases[0].LastActive.IsZero() {
		t.Fatal("LastActive not carried")
	}
}
