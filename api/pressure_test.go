// Low-memory take-back tests (MEMORY_FLOOR_MIB, spoond-l3lq). The loop
// (api/pressure.go) is driven one tick at a time through pressureCheck:
// below the floor it pauses exactly one lease through pressureVictim
// (the LRU unpinned lease of the owner furthest over their slice), it
// stands down while draining, while an undrain is pending or on a bad
// reading, and a floor nothing can be taken back for raises the
// memory_floor alert at most once per 10 min.
//
// The arithmetic the fixtures fix: installDynamicNode reports
// HugepagesUsed = baseUsed + (live sandboxes) x pagesPerLease pages,
// and with the fake's 2 MiB page size free MiB = (totalPages - used)
// x 2. Pausing a lease removes its sandbox from the fake, so the next
// reading really is 512 pages (1 GiB) lighter. onlyIdentityOwners
// drops the legacy token owners newTestService installs, so the fair
// slice (HugepagesTotal x 2 MiB / n) is split over exactly the test's
// identity users.

package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v3/metrics"
	"github.com/jrimmer/spoond/v3/substrate"
)

// newPressureService is a take-back service with metrics installed, a
// roomy snapshot disk and a huge node the tests resize dynamically (a
// pause really frees its pages, as on a real host).
func newPressureService(t *testing.T) (*Service, *testSub, context.Context) {
	t.Helper()
	svc, db, sub := newTestService(t)
	svc.SetMetrics(metrics.NewBackendMetrics())
	onlyIdentityOwners(svc)
	seedImage(t, db, "mid", 1024)
	seedImage(t, db, "big", 8192)
	// Huge initially, so grants admit without pressure.
	sub.SetNodeInfo(substrate.NodeInfo{
		Status:            "healthy",
		HugepagesTotal:    1 << 20,
		HugepageSizeBytes: 2 << 20,
	}, nil)
	svc.cfg.TemplateStoragePath = t.TempDir()
	roomyDisk(svc)
	svc.diskUsage = func(dir string) (int64, error) { return 1024 << 20, nil }
	return svc, sub, context.Background()
}

// suspendedLeases returns the ids of the currently suspended leases.
func suspendedLeases(svc *Service) map[string]bool {
	svc.store.mu.Lock()
	defer svc.store.mu.Unlock()
	out := map[string]bool{}
	for id, l := range svc.store.leases {
		if l.Suspended {
			out[id] = true
		}
	}
	return out
}

// collectEventKinds drains the subscription's buffered events and
// returns the kinds it saw.
func collectEventKinds(t *testing.T, sub *EventSubscription) map[LeaseEventType]int {
	t.Helper()
	kinds := map[LeaseEventType]int{}
	for {
		select {
		case ev := <-sub.C:
			kinds[ev.Type]++
		default:
			return kinds
		}
	}
}

// TestPressureBelowFloorPausesOneLRU: one tick below the floor pauses
// exactly one lease — the least recently used unpinned lease of the
// biggest borrower — with the take-back reason and the loop as the
// requester, and the next tick (the pause having freed its pages) stays
// quiet. The node's cached reading claims more free memory than the
// node really has (as credits inside the cache TTL can): the tick reads
// fresh and pauses anyway.
func TestPressureBelowFloorPausesOneLRU(t *testing.T) {
	svc, sub, ctx := newPressureService(t)
	alice := preemptOwner(t, svc, "alice")
	bob := preemptOwner(t, svc, "bob")
	// Two owners share the 16384 MiB pool below: an 8192 MiB slice
	// each. Alice's nine 1024 MiB leases (9216) put her over; bob's
	// single 1024 is inside his.
	var a []*Lease
	for i := 0; i < 9; i++ {
		l, err := svc.grantLease(ctx, leaseRequest{owner: alice, image: "mid", ttl: time.Hour, persistent: true})
		if err != nil {
			t.Fatalf("grant alice %d: %v", i, err)
		}
		a = append(a, l)
	}
	b, err := svc.grantLease(ctx, leaseRequest{owner: bob, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant bob: %v", err)
	}
	base := time.Unix(1_000_000, 0)
	svc.store.mu.Lock()
	for i, l := range a {
		l.LastActive = base.Add(-time.Duration(len(a)-i) * time.Hour) // a[0] is the LRU
	}
	svc.store.mu.Unlock()

	// 10 live sandboxes at 512 pages hold 5120 of the 8192 pages;
	// baseUsed 1536: used 6656 pages, free = 1536 pages = 3072 MiB,
	// under the 4096 MiB floor.
	installDynamicNode(t, svc, sub, 8192, 1536, 512)
	svc.cfg.MemoryFloorMiB = 4096
	warmFairShares(svc, ctx)
	// The cache claims 1024 pages (2048 MiB) more free than the node: a
	// loop that trusted the cache would see 5120 MiB and skip.
	svc.nodeInfoMu.Lock()
	svc.nodeInfoCache.HugepagesUsed -= 1024
	svc.nodeInfoMu.Unlock()

	svc.pressureCheck(ctx)

	susp := suspendedLeases(svc)
	if len(susp) != 1 || !susp[a[0].ID] {
		t.Fatalf("suspended = %v, want exactly the LRU lease %s of the biggest borrower", susp, a[0].ID)
	}
	if got := a[0].SuspendReason; got != suspendReasonTakeBack {
		t.Fatalf("suspend reason = %q, want %q", got, suspendReasonTakeBack)
	}
	if got := a[0].TakeBackFor; got != pressureRequester {
		t.Fatalf("take-back requester = %q, want %q", got, pressureRequester)
	}
	if b.Suspended {
		t.Fatal("bob's lease was paused")
	}
	if got := counterValue(t, svc.metrics.MemoryPressureTakebacksTotal); got != 1 {
		t.Fatalf("take-back counter = %v, want 1", got)
	}

	// The pause freed its pages: the reading is 512 pages lighter, free
	// = 4096 MiB = exactly the floor, so the next tick pauses nothing.
	svc.pressureCheck(ctx)
	if susp = suspendedLeases(svc); len(susp) != 1 {
		t.Fatalf("after the second tick suspended = %v, want still exactly one", susp)
	}
	if got := counterValue(t, svc.metrics.MemoryPressureTakebacksTotal); got != 1 {
		t.Fatalf("take-back counter after the second tick = %v, want 1", got)
	}
}

// TestPressureSingleLeaseOwnerOverSlice: an owner whose ONLY lease sits
// over its slice IS a candidate — the old memVictims refused her
// because pausing the lease would drop her below the requester's
// after-request ratio; pressureVictim has no requester. Her lease goes,
// never the box-LRU lease of the owner inside their slice.
func TestPressureSingleLeaseOwnerOverSlice(t *testing.T) {
	svc, sub, ctx := newPressureService(t)
	alice := preemptOwner(t, svc, "alice")
	bob := preemptOwner(t, svc, "bob")
	// Two owners share the 8192 MiB pool below: a 4096 MiB slice each.
	// Alice's single 8192 MiB lease is twice her slice; bob's single
	// 1024 MiB lease is a quarter of his.
	aliceL, err := svc.grantLease(ctx, leaseRequest{owner: alice, image: "big", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant alice: %v", err)
	}
	bobL, err := svc.grantLease(ctx, leaseRequest{owner: bob, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant bob: %v", err)
	}
	svc.store.mu.Lock()
	bobL.LastActive = time.Unix(1_000_000, 0) // the box's LRU, but inside the slice
	svc.store.mu.Unlock()

	// 2 live sandboxes at 512 pages hold 1024 pages, baseUsed 3072:
	// used 4096 of the 4096-page (8192 MiB) node, free 0 MiB — below
	// the 2048 MiB floor. The only take-back candidate is alice's
	// single over-slice lease.
	installDynamicNode(t, svc, sub, 4096, 3072, 512)
	svc.cfg.MemoryFloorMiB = 2048
	warmFairShares(svc, ctx)

	svc.pressureCheck(ctx)

	susp := suspendedLeases(svc)
	if len(susp) != 1 || !susp[aliceL.ID] {
		t.Fatalf("suspended = %v, want exactly alice's single over-slice lease %s", susp, aliceL.ID)
	}
	if bobL.Suspended {
		t.Fatal("bob's inside-slice lease was paused")
	}
}

// TestPressureAtOrAboveFloorDoesNothing: at exactly the floor and above
// it no lease is paused, and the committed-memory gauge still moves
// every tick.
func TestPressureAtOrAboveFloorDoesNothing(t *testing.T) {
	svc, sub, ctx := newPressureService(t)
	alice := preemptOwner(t, svc, "alice")
	bob := preemptOwner(t, svc, "bob")
	for i := 0; i < 9; i++ {
		if _, err := svc.grantLease(ctx, leaseRequest{owner: alice, image: "mid", ttl: time.Hour, persistent: true}); err != nil {
			t.Fatalf("grant alice %d: %v", i, err)
		}
	}
	if _, err := svc.grantLease(ctx, leaseRequest{owner: bob, image: "mid", ttl: time.Hour, persistent: true}); err != nil {
		t.Fatalf("grant bob: %v", err)
	}
	// 10 live sandboxes at 256 pages hold 2560 of the 4096 pages,
	// baseUsed 0: free = 1536 pages = 3072 MiB, exactly the floor. A
	// victim exists (alice is over her 8192 MiB slice), but the floor is
	// met and nothing moves.
	installDynamicNode(t, svc, sub, 4096, 0, 256)
	svc.cfg.MemoryFloorMiB = 3072
	warmFairShares(svc, ctx)

	svc.pressureCheck(ctx)
	if susp := suspendedLeases(svc); len(susp) != 0 {
		t.Fatalf("at the floor suspended = %v, want none", susp)
	}
	// The gauge reads the live leases' memory: ten 1024 MiB leases.
	if got := gaugeValue(t, svc, "spoond_memory_committed_mib"); got != 10240 {
		t.Fatalf("committed gauge = %v, want 10240", got)
	}

	svc.cfg.MemoryFloorMiB = 2048 // comfortably below the free reading
	svc.pressureCheck(ctx)
	if susp := suspendedLeases(svc); len(susp) != 0 {
		t.Fatalf("above the floor suspended = %v, want none", susp)
	}
	if got := counterValue(t, svc.metrics.MemoryPressureTakebacksTotal); got != 0 {
		t.Fatalf("take-back counter = %v, want 0", got)
	}
}

// TestPressureSparesPinnedAndInsideSlice: below the floor, the pinned
// lease of the over-slice owner and the older leases of owners inside
// their slice all stay running; the owner's LRU unpinned lease is the
// one that goes.
func TestPressureSparesPinnedAndInsideSlice(t *testing.T) {
	svc, sub, ctx := newPressureService(t)
	alice := preemptOwner(t, svc, "alice")
	bob := preemptOwner(t, svc, "bob")
	carol := preemptOwner(t, svc, "carol")
	// Three owners share the 8192 MiB pool below: a 2730 MiB slice
	// each, so alice's 3072 MiB is over it and bob's and carol's 1024
	// inside.
	pin, err := svc.grantLease(ctx, leaseRequest{owner: alice, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant pin: %v", err)
	}
	mid, err := svc.grantLease(ctx, leaseRequest{owner: alice, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant mid: %v", err)
	}
	fresh, err := svc.grantLease(ctx, leaseRequest{owner: alice, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant fresh: %v", err)
	}
	bobL, err := svc.grantLease(ctx, leaseRequest{owner: bob, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant bob: %v", err)
	}
	if _, err := svc.grantLease(ctx, leaseRequest{owner: carol, image: "mid", ttl: time.Hour, persistent: true}); err != nil {
		t.Fatalf("grant carol: %v", err)
	}
	if _, err := svc.setPinned(alice, pin.ID, true); err != nil {
		t.Fatalf("pin: %v", err)
	}
	base := time.Unix(1_000_000, 0)
	svc.store.mu.Lock()
	pin.LastActive = base.Add(-10 * time.Hour) // the box's LRU, but pinned
	bobL.LastActive = base.Add(-9 * time.Hour) // older than mid, but inside the slice
	mid.LastActive = base.Add(-2 * time.Hour)  // alice's LRU unpinned lease
	fresh.LastActive = base.Add(-time.Minute)
	svc.store.mu.Unlock()

	// 5 live sandboxes at 512 pages hold 2560 of the 4096 pages: free =
	// 3072 MiB, under the 4096 MiB floor by exactly one lease's 1024.
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	svc.cfg.MemoryFloorMiB = 4096
	warmFairShares(svc, ctx)

	svc.pressureCheck(ctx)

	susp := suspendedLeases(svc)
	if len(susp) != 1 || !susp[mid.ID] {
		t.Fatalf("suspended = %v, want exactly alice's LRU unpinned lease %s", susp, mid.ID)
	}
	if pin.Suspended {
		t.Fatal("the pinned lease was paused")
	}
	if bobL.Suspended {
		t.Fatal("a lease of an owner inside their slice was paused")
	}
	if fresh.Suspended {
		t.Fatal("the most recent lease was paused over its older sibling")
	}
}

// TestPressureTopOwnerPinnedFallsToNextOwner: when the owner with the
// highest ratio has ONLY pinned leases, the next over-slice owner's
// unpinned lease goes instead — the tick pauses rather than alerts.
func TestPressureTopOwnerPinnedFallsToNextOwner(t *testing.T) {
	svc, sub, ctx := newPressureService(t)
	alice := preemptOwner(t, svc, "alice")
	bob := preemptOwner(t, svc, "bob")
	// Two owners share the 8192 MiB pool: a 4096 MiB slice each.
	// Alice's three pinned 8192 MiB leases put her furthest over (6x);
	// bob's single 8192 MiB lease is over his (2x) and unpinned.
	pins := make([]*Lease, 0, 3)
	for i := 0; i < 3; i++ {
		l, err := svc.grantLease(ctx, leaseRequest{owner: alice, image: "big", ttl: time.Hour, persistent: true})
		if err != nil {
			t.Fatalf("grant pin %d: %v", i, err)
		}
		if _, err := svc.setPinned(alice, l.ID, true); err != nil {
			t.Fatalf("pin %d: %v", i, err)
		}
		pins = append(pins, l)
	}
	bobL, err := svc.grantLease(ctx, leaseRequest{owner: bob, image: "big", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant bob: %v", err)
	}
	// 4 live sandboxes at 512 pages hold 2048 of the 4096 pages: free =
	// 4096 MiB, under the 6144 MiB floor.
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	svc.cfg.MemoryFloorMiB = 6144
	warmFairShares(svc, ctx)

	svc.pressureCheck(ctx)

	susp := suspendedLeases(svc)
	if len(susp) != 1 || !susp[bobL.ID] {
		t.Fatalf("suspended = %v, want exactly bob's lease %s (alice is top but every lease of hers is pinned)", susp, bobL.ID)
	}
	for _, p := range pins {
		if p.Suspended {
			t.Fatal("a pinned lease was paused")
		}
	}
}

// TestPressureBusyAfterNonBusy: two leases of the one over-slice owner,
// the older one busy (an in-flight operation): the tick takes the
// non-busy one even though it is younger.
func TestPressureBusyAfterNonBusy(t *testing.T) {
	svc, sub, ctx := newPressureService(t)
	alice := preemptOwner(t, svc, "alice")
	bob := preemptOwner(t, svc, "bob")
	// Two owners share the 8192 MiB pool: a 4096 MiB slice. Alice's
	// five 1024 MiB leases (5120) put her over; bob's single 1024 is
	// inside his. Of alice's leases the oldest is busy.
	leases := make([]*Lease, 0, 5)
	for i := 0; i < 5; i++ {
		l, err := svc.grantLease(ctx, leaseRequest{owner: alice, image: "mid", ttl: time.Hour, persistent: true})
		if err != nil {
			t.Fatalf("grant alice %d: %v", i, err)
		}
		leases = append(leases, l)
	}
	if _, err := svc.grantLease(ctx, leaseRequest{owner: bob, image: "mid", ttl: time.Hour, persistent: true}); err != nil {
		t.Fatalf("grant bob: %v", err)
	}
	busyL, freeL := leases[0], leases[1]
	base := time.Unix(1_000_000, 0)
	svc.store.mu.Lock()
	busyL.LastActive = base.Add(-9 * time.Hour) // the LRU, but busy
	freeL.LastActive = base.Add(-1 * time.Hour) // the oldest non-busy
	busyL.busy = true                           // an in-flight operation ranks after every non-busy lease
	svc.store.mu.Unlock()

	// 6 live sandboxes at 512 pages hold 3072 of the 4096 pages: free =
	// 2048 MiB, under the 3072 MiB floor.
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	svc.cfg.MemoryFloorMiB = 3072
	warmFairShares(svc, ctx)

	svc.pressureCheck(ctx)

	susp := suspendedLeases(svc)
	if len(susp) != 1 || !susp[freeL.ID] {
		t.Fatalf("suspended = %v, want exactly the non-busy lease %s (the busy LRU %s is taken last)", susp, freeL.ID, busyL.ID)
	}
	if busyL.Suspended {
		t.Fatal("the busy lease was paused while a non-busy one existed")
	}
}

// TestPressureTickSkippedWhileDraining: the tick stands down while
// s.draining is set — no pause, no node read. Without the drain the
// same state has a victim below the floor, so the skip is the drain's.
func TestPressureTickSkippedWhileDraining(t *testing.T) {
	svc, sub, ctx := newPressureService(t)
	alice := preemptOwner(t, svc, "alice")
	bob := preemptOwner(t, svc, "bob")
	// Two owners, an 8192 MiB slice each on the pool below: alice's
	// five 1024 MiB leases over it, bob's single lease inside.
	for i := 0; i < 5; i++ {
		if _, err := svc.grantLease(ctx, leaseRequest{owner: alice, image: "mid", ttl: time.Hour, persistent: true}); err != nil {
			t.Fatalf("grant alice %d: %v", i, err)
		}
	}
	if _, err := svc.grantLease(ctx, leaseRequest{owner: bob, image: "mid", ttl: time.Hour, persistent: true}); err != nil {
		t.Fatalf("grant bob: %v", err)
	}
	// 6 live sandboxes at 512 pages: used 3072 + 512 base = 3584 of
	// 4096 pages, free = 1024 MiB, far below the 4096 MiB floor.
	installDynamicNode(t, svc, sub, 4096, 512, 512)
	svc.cfg.MemoryFloorMiB = 4096
	warmFairShares(svc, ctx)
	nCalls := len(sub.CallLog())
	svc.draining.Store(true)
	defer svc.draining.Store(false)

	svc.pressureCheck(ctx)

	if susp := suspendedLeases(svc); len(susp) != 0 {
		t.Fatalf("while draining suspended = %v, want none", susp)
	}
	if got := len(sub.CallLog()); got != nCalls {
		t.Fatalf("draining tick made %d substrate calls, want 0", got-nCalls)
	}
	if got := counterValue(t, svc.metrics.MemoryPressureTakebacksTotal); got != 0 {
		t.Fatalf("take-back counter = %v, want 0", got)
	}
}

// TestPressureTickSkippedWhileAnyLeaseDrained: an undrain pending (a
// live lease still marked Drained) skips the tick the way draining
// does, even though s.draining is false again.
func TestPressureTickSkippedWhileAnyLeaseDrained(t *testing.T) {
	svc, sub, ctx := newPressureService(t)
	alice := preemptOwner(t, svc, "alice")
	bob := preemptOwner(t, svc, "bob")
	for i := 0; i < 5; i++ {
		if _, err := svc.grantLease(ctx, leaseRequest{owner: alice, image: "mid", ttl: time.Hour, persistent: true}); err != nil {
			t.Fatalf("grant alice %d: %v", i, err)
		}
	}
	if _, err := svc.grantLease(ctx, leaseRequest{owner: bob, image: "mid", ttl: time.Hour, persistent: true}); err != nil {
		t.Fatalf("grant bob: %v", err)
	}
	installDynamicNode(t, svc, sub, 4096, 512, 512) // free = 1024 MiB
	svc.cfg.MemoryFloorMiB = 4096
	warmFairShares(svc, ctx)
	// An undrain in flight: one live lease holds the Drained flag (as a
	// drain whose undrain deferred leaves it), s.draining false.
	svc.store.mu.Lock()
	for _, l := range svc.store.leases {
		l.Drained = true
		break
	}
	svc.store.mu.Unlock()
	nCalls := len(sub.CallLog())

	svc.pressureCheck(ctx)

	if susp := suspendedLeases(svc); len(susp) != 0 {
		t.Fatalf("while a lease is Drained suspended = %v, want none", susp)
	}
	if got := len(sub.CallLog()); got != nCalls {
		t.Fatalf("tick with a Drained lease made %d substrate calls, want 0", got-nCalls)
	}
}

// TestPressureTickSkippedOnBadReading: a non-healthy Status and an
// empty HugepagesTotal are each not a floor breach — the tick returns
// before touching any lease, but still moves the committed gauge.
func TestPressureTickSkippedOnBadReading(t *testing.T) {
	svc, sub, ctx := newPressureService(t)
	alice := preemptOwner(t, svc, "alice")
	for i := 0; i < 3; i++ {
		if _, err := svc.grantLease(ctx, leaseRequest{owner: alice, image: "mid", ttl: time.Hour, persistent: true}); err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
	}
	svc.cfg.MemoryFloorMiB = 4096
	warmFairShares(svc, ctx)

	// A draining node: free reads low, but a non-healthy status is not
	// a breach.
	sub.SetNodeInfo(substrate.NodeInfo{Status: "draining", HugepagesTotal: 512, HugepageSizeBytes: 2 << 20}, nil)
	dropNodeCache(svc)
	svc.pressureCheck(ctx)
	if susp := suspendedLeases(svc); len(susp) != 0 {
		t.Fatalf("draining status suspended = %v, want none", susp)
	}

	// A booting orchestrator: no pages read at all.
	sub.SetNodeInfo(substrate.NodeInfo{Status: "healthy", HugepagesTotal: 0, HugepageSizeBytes: 2 << 20}, nil)
	dropNodeCache(svc)
	svc.pressureCheck(ctx)
	if susp := suspendedLeases(svc); len(susp) != 0 {
		t.Fatalf("no-pages reading suspended = %v, want none", susp)
	}
	if got := counterValue(t, svc.metrics.MemoryPressureTakebacksTotal); got != 0 {
		t.Fatalf("take-back counter = %v, want 0", got)
	}
	// The gauge moved on both skipped ticks: three 1024 MiB leases.
	if got := gaugeValue(t, svc, "spoond_memory_committed_mib"); got != 3072 {
		t.Fatalf("committed gauge on skipped ticks = %v, want 3072", got)
	}
}

// TestPressureGaugeMovesWithFloorOff: with the floor at 0 the tick
// still sets the committed gauge — the metric must not freeze at its
// last value with the floor off — and pauses nothing.
func TestPressureGaugeMovesWithFloorOff(t *testing.T) {
	svc, sub, ctx := newPressureService(t)
	alice := preemptOwner(t, svc, "alice")
	for i := 0; i < 2; i++ {
		if _, err := svc.grantLease(ctx, leaseRequest{owner: alice, image: "mid", ttl: time.Hour, persistent: true}); err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
	}
	installDynamicNode(t, svc, sub, 4096, 512, 512) // free = 5120 MiB
	svc.cfg.MemoryFloorMiB = 0                      // off
	warmFairShares(svc, ctx)

	svc.pressureCheck(ctx)

	if susp := suspendedLeases(svc); len(susp) != 0 {
		t.Fatalf("floor off suspended = %v, want none", susp)
	}
	if got := gaugeValue(t, svc, "spoond_memory_committed_mib"); got != 2048 {
		t.Fatalf("committed gauge with the floor off = %v, want 2048", got)
	}
	if got := counterValue(t, svc.metrics.MemoryPressureTakebacksTotal); got != 0 {
		t.Fatalf("take-back counter = %v, want 0", got)
	}
}

// TestPressureAlertOncePerTenMinutes: below a floor nothing can be
// taken back for (every candidate pinned), the tick raises the
// memory_floor alert — its own event kind, not box_full — at most once
// per 10 min; past the window it repeats.
func TestPressureAlertOncePerTenMinutes(t *testing.T) {
	svc, sub, ctx := newPressureService(t)
	alice := preemptOwner(t, svc, "alice")
	preemptOwner(t, svc, "bob") // inside his slice (holds nothing); never touched
	var pins []*Lease
	for i := 0; i < 3; i++ {
		l, err := svc.grantLease(ctx, leaseRequest{owner: alice, image: "mid", ttl: time.Hour, persistent: true})
		if err != nil {
			t.Fatalf("grant alice %d: %v", i, err)
		}
		if _, err := svc.setPinned(alice, l.ID, true); err != nil {
			t.Fatalf("pin %d: %v", i, err)
		}
		pins = append(pins, l)
	}

	now := time.Now()
	svc.now = func() time.Time { return now }
	// 3 live sandboxes at 256 pages hold 768 pages, baseUsed 512: used
	// 1280 of the 1536-page (3072 MiB) node, free = 512 MiB, under the
	// 4096 MiB floor. Two owners share the 3072 MiB: a 1536 MiB slice,
	// alice's 3072 is 2x over — but every lease of hers is pinned and
	// nobody else borrows, so nothing can be taken back.
	installDynamicNode(t, svc, sub, 1536, 512, 256)
	svc.cfg.MemoryFloorMiB = 4096
	warmFairShares(svc, ctx)

	events := svc.Subscribe(EventFilter{})
	defer events.Close()

	svc.pressureCheck(ctx)
	kinds := collectEventKinds(t, events)
	if kinds[LeaseMemoryFloor] != 1 || len(kinds) != 1 {
		t.Fatalf("first below-floor tick raised %v, want exactly one memory_floor event", kinds)
	}

	// Ten seconds later: still the same 10-minute window, no repeat.
	now = now.Add(10 * time.Second)
	svc.pressureCheck(ctx)
	if kinds = collectEventKinds(t, events); len(kinds) != 0 {
		t.Fatalf("a tick inside the 10-minute window raised %v, want none", kinds)
	}
	if susp := suspendedLeases(svc); len(susp) != 0 {
		t.Fatalf("suspended = %v, want none (every candidate pinned)", susp)
	}

	// Ten minutes on: the alert may repeat.
	now = now.Add(pressureAlertEvery)
	svc.pressureCheck(ctx)
	if kinds = collectEventKinds(t, events); kinds[LeaseMemoryFloor] != 1 || len(kinds) != 1 {
		t.Fatalf("a tick past the 10-minute window raised %v, want exactly one memory_floor event", kinds)
	}
	for _, l := range pins {
		if l.Suspended {
			t.Fatal("a pinned lease was paused")
		}
	}
	if got := counterValue(t, svc.metrics.MemoryPressureTakebacksTotal); got != 0 {
		t.Fatalf("take-back counter = %v, want 0", got)
	}
}

// TestPressurePauseSurvivesCancelledLoopContext: a floor pause runs on
// a context detached from the loop's — a shutdown must not cut a
// snapshot write in half — bounded by its own timeout.
func TestPressurePauseSurvivesCancelledLoopContext(t *testing.T) {
	svc, sub, ctx := newPressureService(t)
	alice := preemptOwner(t, svc, "alice")
	// One owner gets the whole 2048 MiB pool below as her slice; her
	// three 1024 MiB leases are 1.5x it — over, with takeable leases.
	l1, err := svc.grantLease(ctx, leaseRequest{owner: alice, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant 1: %v", err)
	}
	if _, err := svc.grantLease(ctx, leaseRequest{owner: alice, image: "mid", ttl: time.Hour, persistent: true}); err != nil {
		t.Fatalf("grant 2: %v", err)
	}
	if _, err := svc.grantLease(ctx, leaseRequest{owner: alice, image: "mid", ttl: time.Hour, persistent: true}); err != nil {
		t.Fatalf("grant 3: %v", err)
	}
	// 3 live sandboxes at 256 pages hold 768 of the 1024-page (2048
	// MiB) node, baseUsed 0: free = 512 MiB, under the floor; alice's
	// 3072 MiB of a 2048 MiB slice is over it.
	installDynamicNode(t, svc, sub, 1024, 0, 256)
	svc.cfg.MemoryFloorMiB = 2048
	warmFairShares(svc, ctx)

	// Cancel the loop's context before the tick: the pause must still
	// land the snapshot.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	svc.pressureCheck(cancelled)

	susp := suspendedLeases(svc)
	if len(susp) != 1 || !susp[l1.ID] {
		t.Fatalf("suspended = %v, want exactly the LRU lease %s (the loop's cancelled ctx must not stop the pause)", susp, l1.ID)
	}
	saw := false
	for _, c := range sub.CallLog() {
		if strings.HasPrefix(c, "Pause ") {
			saw = true
		}
	}
	if !saw {
		t.Fatal("no Pause call reached the substrate")
	}
}