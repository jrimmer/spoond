// Low-memory take-back tests (MEMORY_FLOOR_MIB, spoond-l3lq). The loop
// (api/pressure.go) is driven one tick at a time through pressureCheck:
// below the floor it pauses exactly one lease through pressureVictim
// (the LRU unpinned lease of the owner furthest over their slice; below
// MEMORY_CRITICAL_MIB any owner's, pinned leases last), it
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
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v4/metrics"
	"github.com/jrimmer/spoond/v4/substrate"
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
	seedImage(t, db, "big6", 6144)
	seedImage(t, db, "big4", 4096)
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

// TestPressureSparesDrainedLease: a live lease still marked Drained (an
// undrain pending on it) is never the victim — but, unlike s.draining,
// it does not skip the tick: the loop pauses a healthy lease instead,
// so a lease stuck Drained cannot switch the floor off for good.
func TestPressureSparesDrainedLease(t *testing.T) {
	svc, sub, ctx := newPressureService(t)
	alice := preemptOwner(t, svc, "alice")
	bob := preemptOwner(t, svc, "bob")
	// Five 1024 MiB leases over alice's share, and bob's single lease —
	// the box's LRU — inside his slice.
	leases := make([]*Lease, 0, 5)
	for i := 0; i < 5; i++ {
		l, err := svc.grantLease(ctx, leaseRequest{owner: alice, image: "mid", ttl: time.Hour, persistent: true})
		if err != nil {
			t.Fatalf("grant alice %d: %v", i, err)
		}
		leases = append(leases, l)
	}
	bobL, err := svc.grantLease(ctx, leaseRequest{owner: bob, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant bob: %v", err)
	}
	base := time.Unix(1_000_000, 0)
	svc.store.mu.Lock()
	for i, l := range leases {
		l.LastActive = base.Add(-time.Duration(len(leases)-i) * time.Hour) // leases[0] is alice's LRU
	}
	bobL.LastActive = base.Add(-48 * time.Hour) // the box's LRU overall, but inside bob's slice
	svc.store.mu.Unlock()

	// 6 live sandboxes at 512 pages hold 3072 of the 4096 pages,
	// baseUsed 512: used 3584 of 4096 pages, free = 1024 MiB, below the
	// 4096 MiB floor by one lease.
	installDynamicNode(t, svc, sub, 4096, 512, 512)
	svc.cfg.MemoryFloorMiB = 4096
	warmFairShares(svc, ctx)
	// An undrain in flight: alice's LRU lease holds the Drained flag (as
	// a drain whose undrain deferred leaves it), s.draining false.
	leases[0].Drained = true

	svc.pressureCheck(ctx)

	susp := suspendedLeases(svc)
	if susp[leases[0].ID] {
		t.Fatalf("the Drained lease %s was paused", leases[0].ID)
	}
	if len(susp) != 1 || !susp[leases[1].ID] {
		t.Fatalf("suspended = %v, want exactly alice's next LRU lease %s (the tick is not skipped for a Drained lease)", susp, leases[1].ID)
	}
	if bobL.Suspended {
		t.Fatal("bob's inside-slice lease was paused")
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
// taken back for (every candidate busy with a running job, pinned or
// not: the critical level takes pinned leases but never busy ones),
// the tick raises the memory_floor alert — its own event kind, not
// box_full — at most once per 10 min; past the window it repeats.
func TestPressureAlertOncePerTenMinutes(t *testing.T) {
	svc, sub, ctx := newPressureService(t)
	alice := preemptOwner(t, svc, "alice")
	bob := preemptOwner(t, svc, "bob")
	// bob's one lease holds him to his fair half, so alice's slice is
	// that half too and not the whole idle pool; it is inside his slice
	// and never touched.
	bobL, err := svc.grantLease(ctx, leaseRequest{owner: bob, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant bob: %v", err)
	}
	var pins []*Lease
	for i := 0; i < 3; i++ {
		l, err := svc.grantLease(ctx, leaseRequest{owner: alice, image: "mid", ttl: time.Hour, persistent: true})
		if err != nil {
			t.Fatalf("grant alice %d: %v", i, err)
		}
		if i == 0 {
			if _, err := svc.setPinned(alice, l.ID, true); err != nil {
				t.Fatalf("pin %d: %v", i, err)
			}
		}
		svc.incRunningJob(l.ID)
		pins = append(pins, l)
	}

	now := time.Now()
	svc.now = func() time.Time { return now }
	// 4 live sandboxes at 256 pages hold 1024 pages, baseUsed 256: used
	// 1280 of the 1536-page (3072 MiB) node, free = 512 MiB, under the
	// 4096 MiB floor. Two owners share the 3072 MiB: a 1536 MiB slice,
	// alice's 3072 is 2x over — but every lease of hers runs a job and
	// bob is inside his slice, so nothing can be taken back.
	installDynamicNode(t, svc, sub, 1536, 256, 256)
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
		t.Fatalf("suspended = %v, want none (every candidate busy)", susp)
	}

	// Ten minutes on: the alert may repeat.
	now = now.Add(pressureAlertEvery)
	svc.pressureCheck(ctx)
	if kinds = collectEventKinds(t, events); kinds[LeaseMemoryFloor] != 1 || len(kinds) != 1 {
		t.Fatalf("a tick past the 10-minute window raised %v, want exactly one memory_floor event", kinds)
	}
	for _, l := range pins {
		if l.Suspended {
			t.Fatal("a busy lease was paused")
		}
	}
	if bobL.Suspended {
		t.Fatal("bob's inside-slice lease was paused")
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

// TestAdmitFloorHeadroom: with MEMORY_FLOOR_MIB=2048 on an 8 GiB pool
// with 6 GiB free, admission keeps the floor as headroom: a 6 GiB
// request (free 6 GiB > need 6 GiB, but 6 - 2 < 6) takes back or
// refuses, a 4 GiB request (6 - 2 >= 4) is granted. With the floor at
// 0 the same 6 GiB request passes: the old behaviour.
func TestAdmitFloorHeadroom(t *testing.T) {
	svc, sub, ctx := newPressureService(t)
	alice := preemptOwner(t, svc, "alice")
	bob := preemptOwner(t, svc, "bob")
	// The floor sits at the round-2 default 2048.
	svc.cfg.MemoryFloorMiB = 2048

	// An 8 GiB pool (4096 2 MiB pages) with 2 GiB used by work outside
	// spoond: free reads 3072 pages = 6144 MiB. The static fake node is
	// warmed into the admission cache (warmFairShares), because the
	// cache's own lazy TTL would race the first admission.
	sub.SetNodeInfo(substrate.NodeInfo{
		Status:            "healthy",
		HugepagesTotal:    4096,
		HugepagesUsed:     1024,
		HugepageSizeBytes: 2 << 20,
	}, nil)
	warmFairShares(svc, ctx)

	// 6 GiB request: free (6144) covers memoryMB (6144) alone, but not
	// memoryHeadroom (8192): fits is false, take-back finds no victim
	// (nobody holds a lease), and the ordinary capacity check answers:
	// the grant is refused.
	_, err := svc.grantLease(ctx, leaseRequest{owner: alice, image: "big6", ttl: time.Hour, persistent: true})
	if err == nil {
		t.Fatal("6 GiB request granted without floor headroom")
	}
	if !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("6 GiB request error = %v, want a capacity refusal", err)
	}
	// 4 GiB request: 6144 - 2048 >= 4096 — granted, and the sandbox is
	// really created.
	l, err := svc.grantLease(ctx, leaseRequest{owner: bob, image: "big4", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("4 GiB request refused: %v", err)
	}
	if l.State != "running" {
		t.Fatalf("4 GiB lease state = %q, want running", l.State)
	}

	// Floor off (0): the same 6 GiB request is the plain free-vs-need
	// comparison again and passes (old behaviour).
	svc2, sub2, ctx2 := newPressureService(t)
	alice2 := preemptOwner(t, svc2, "alice")
	svc2.cfg.MemoryFloorMiB = 0
	sub2.SetNodeInfo(substrate.NodeInfo{
		Status:            "healthy",
		HugepagesTotal:    4096,
		HugepagesUsed:     1024,
		HugepageSizeBytes: 2 << 20,
	}, nil)
	warmFairShares(svc2, ctx2)
	if _, err := svc2.grantLease(ctx2, leaseRequest{owner: alice2, image: "big6", ttl: time.Hour, persistent: true}); err != nil {
		t.Fatalf("floor off: 6 GiB request refused: %v", err)
	}
}

// TestPressureBusyTopOwnerFallsToNextOwner: every lease of the owner
// furthest over is busy (takeBackPause refuses busy leases), so the tick
// must pass that owner over and pause the next owner's idle lease — not
// offer the busy one each tick and stall. With every lease busy the
// tick pauses nothing and raises the memory_floor alert.
func TestPressureBusyTopOwnerFallsToNextOwner(t *testing.T) {
	svc, sub, ctx := newPressureService(t)
	alice := preemptOwner(t, svc, "alice")
	bob := preemptOwner(t, svc, "bob")
	// A 4096 MiB slice each of the 8192 MiB pool: alice's three 8192
	// MiB leases (6x, all busy) and bob's one (2x, idle).
	busy := make([]*Lease, 0, 3)
	for i := 0; i < 3; i++ {
		l, err := svc.grantLease(ctx, leaseRequest{owner: alice, image: "big", ttl: time.Hour, persistent: true})
		if err != nil {
			t.Fatalf("grant alice %d: %v", i, err)
		}
		busy = append(busy, l)
	}
	bobL, err := svc.grantLease(ctx, leaseRequest{owner: bob, image: "big", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant bob: %v", err)
	}
	svc.store.mu.Lock()
	for _, l := range busy {
		l.busy = true
	}
	svc.store.mu.Unlock()
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	svc.cfg.MemoryFloorMiB = 6144
	warmFairShares(svc, ctx)

	if !svc.pressureCheck(ctx) {
		t.Fatal("tick paused nothing; want bob's idle lease")
	}
	if susp := suspendedLeases(svc); len(susp) != 1 || !susp[bobL.ID] {
		t.Fatalf("suspended = %v, want exactly bob's lease %s", susp, bobL.ID)
	}

	// Now every running lease is busy: nothing to pause, and the alert.
	events := svc.Subscribe(EventFilter{})
	defer events.Close()
	warmFairShares(svc, ctx)
	if svc.pressureCheck(ctx) {
		t.Fatal("tick paused a lease although every running lease is busy")
	}
	for _, l := range busy {
		if l.Suspended {
			t.Fatal("a busy lease was paused")
		}
	}
	if kinds := collectEventKinds(t, events); kinds[LeaseMemoryFloor] != 1 {
		t.Fatalf("events = %v, want one memory_floor alert", kinds)
	}
}

// TestAdmitCapacityWithoutFloor: a lease coming back to what the box
// already held (un-take-back, undrain) is admitted without the floor
// headroom; a new request still needs it.
func TestAdmitCapacityWithoutFloor(t *testing.T) {
	svc, sub, ctx := newPressureService(t)
	svc.cfg.MemoryFloorMiB = 2048
	sub.SetNodeInfo(substrate.NodeInfo{
		Status:            "healthy",
		HugepagesTotal:    4096,
		HugepagesUsed:     1024, // 6144 MiB free
		HugepageSizeBytes: 2 << 20,
	}, nil)
	if err := svc.admitCapacity(ctx, 6144); err == nil {
		t.Fatal("new 6 GiB request admitted without the 2 GiB floor free on top")
	}
	if err := svc.admitCapacity(withoutFloor(ctx), 6144); err != nil {
		t.Fatalf("returning 6 GiB lease refused: %v", err)
	}
	// Take-back's view honours the same flag: fits (and the need it
	// works to) drop the floor for a returning lease, so an undrain does
	// not take back other owners' leases to win floor headroom.
	warmFairShares(svc, ctx)
	if ok, err := svc.fits(ctx, 6144); err != nil || ok {
		t.Fatalf("fits(new 6 GiB) = %v, %v; want false (floor not free on top)", ok, err)
	}
	if ok, err := svc.fits(withoutFloor(ctx), 6144); err != nil || !ok {
		t.Fatalf("fits(returning 6 GiB) = %v, %v; want true", ok, err)
	}
	if got := svc.memoryHeadroom(withoutFloor(ctx), 6144); got != 6144 {
		t.Fatalf("headroom for a returning lease = %d, want 6144", got)
	}
}

// TestAdmitResumeLeavesFloorFreeNearFloor: a take-back-paused lease
// resumes through the holder's ordinary path (resumeForUse), which
// re-passes admission with the floor as headroom (spoond-154's
// floor-buffer rule). Near the floor that means refused: with free
// below the lease's memory plus the floor, the resume answers capacity
// and the lease stays paused with the floor still free (nothing
// debited, no other lease taken back); exactly at memory + floor the
// resume is granted, and the node's free hugepages once the VM holds
// its pages are exactly the floor.
func TestAdmitResumeLeavesFloorFreeNearFloor(t *testing.T) {
	// One MiB short of the lease's 1024 plus the 2048 floor: the resume
	// is refused and nothing moves.
	svc, _, ctx, ls, _ := giveBackFixture(t, 1, 3070)
	l := ls[0]
	warmFairShares(svc, ctx) // the fixture's freeMiB is below; re-warm after the dynamic node install
	if free, err := svc.cachedFreeHugepageMiB(ctx); err != nil || free != 3070 {
		t.Fatalf("setup: cached free = %d, %v; want 3070", free, err)
	}
	if _, err := svc.resumeForUse(ctx, l); !errors.Is(err, substrate.ErrCapacity) {
		t.Fatalf("resume at free 3070 (need 1024 + 2048) = %v, want a capacity refusal", err)
	}
	if !l.Suspended || l.State != "suspended" {
		t.Fatalf("refused resume left the lease state %q suspended=%v, want still suspended", l.State, l.Suspended)
	}
	if free, err := svc.cachedFreeHugepageMiB(ctx); err != nil || free != 3070 {
		t.Fatalf("cached free after the refused resume = %d, %v; want 3070 (nothing debited)", free, err)
	}

	// Exactly memory + floor: the resume is granted, and the node's free
	// hugepages after the VM holds its pages are exactly the floor —
	// admission started the VM only into its own memory, never into the
	// floor's buffer.
	svc2, _, ctx2, ls2, _ := giveBackFixture(t, 1, 3072)
	l2 := ls2[0]
	warmFairShares(svc2, ctx2)
	if _, err := svc2.resumeForUse(ctx2, l2); err != nil {
		t.Fatalf("resume at exactly 1024 + 2048 free: %v", err)
	}
	if l2.State != "running" || l2.Suspended {
		t.Fatalf("resumed lease state = %q suspended=%v, want running", l2.State, l2.Suspended)
	}
	// The refreshed cache carries the debit: 3072 - 1024 = 2048, the
	// floor, still free.
	if free, err := svc2.cachedFreeHugepageMiB(ctx2); err != nil || free != 2048 {
		t.Fatalf("cached free after the resume = %d, %v; want 2048 (the floor free after the lease's full size)", free, err)
	}
}

// TestAdmitRefreshesNearFloor: spoond-154 — inside the node cache's TTL
// (15 s), an admission whose request plus the floor sits within
// nearFloorMarginMiB of the cached free value refreshes the NodeInfo and
// is judged on the fresh reading: the fake's free pages dropped between
// the two reads (a VM faulted its memory back in), and the cached value
// would have admitted what the node no longer has. Away from that edge
// the cache behaves as before — a request comfortably above it does not
// make a second call the cached reading already answers.
func TestAdmitRefreshesNearFloor(t *testing.T) {
	svc, sub, ctx := newPressureService(t)
	alice := preemptOwner(t, svc, "alice")
	svc.cfg.MemoryFloorMiB = 2048

	// The node reads 4096 MiB free; the fake's nodeInfoFn is replaced so
	// the NEXT read reports the collapse to 512 MiB free — inside the
	// cache's 15 s window, the shape a fast drop has.
	sub.SetNodeInfo(substrate.NodeInfo{
		Status: "healthy", HugepagesTotal: 4096, HugepageSizeBytes: 2 << 20, HugepagesUsed: 2048,
	}, nil)
	warmFairShares(svc, ctx) // fills the cache (and the fair snapshot)
	if free, err := svc.cachedFreeHugepageMiB(ctx); err != nil || free != 4096 {
		t.Fatalf("setup: cached free = %d, %v; want 4096", free, err)
	}
	sub.SetNodeInfo(substrate.NodeInfo{
		Status: "healthy", HugepagesTotal: 4096, HugepageSizeBytes: 2 << 20, HugepagesUsed: 3584,
	}, nil)

	// The 1024 MiB request plus the 2048 floor passes the old cache
	// (4096 >= 3072) but not the fresh value (512). Without spoond-154's
	// near-floor refresh freeHugepageMiBForRequestLocked would never have
	// called the substrate again within the TTL and the create would
	// wrongly be granted.
	nCalls := len(sub.CallLog())
	_, err := svc.grantLease(ctx, leaseRequest{owner: alice, image: "mid", ttl: time.Hour, persistent: true})
	if err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("1024 MiB request inside the cache window = %v, want a capacity refusal on the fresh reading", err)
	}
	node := 0
	for _, c := range sub.CallLog()[nCalls:] {
		if c == "NodeInfo" {
			node++
		}
	}
	if node < 2 {
		t.Fatalf("%d NodeInfo reads during the near-floor grant, want at least 2 (the cache was fresh; the near-floor refresh re-read the substrate inside the TTL)", node)
	}

	// The same shape with the box away from the floor's edge: a request
	// whose headroom need is met with plenty to spare trusts the cached
	// reading and makes no refresh call for it.
	svc2, sub2, ctx2 := newPressureService(t)
	bob := preemptOwner(t, svc2, "bob")
	svc2.cfg.MemoryFloorMiB = 2048
	sub2.SetNodeInfo(substrate.NodeInfo{
		Status: "healthy", HugepagesTotal: 1 << 22, HugepageSizeBytes: 2 << 20, HugepagesUsed: 0,
	}, nil) // free 8192 MiB of an 8 GiB pool: far above the 1024 + 2048 + 2048 edge
	warmFairShares(svc2, ctx2)
	callsBefore := len(sub2.CallLog())
	if _, err := svc2.grantLease(ctx2, leaseRequest{owner: bob, image: "mid", ttl: time.Hour, persistent: true}); err != nil {
		t.Fatalf("grant with room: %v", err)
	}
	node = 0
	for _, c := range sub2.CallLog()[callsBefore:] {
		if c == "NodeInfo" {
			node++
		}
	}
	if node != 1 {
		t.Fatalf("%d NodeInfo reads during the roomy grant, want 1 (admitCapacity's own check; no near-floor refresh away from the edge)", node)
	}
}

// TestPressureChecksEverySecond: the loop's pressureInterval is one
// second — its normal cadence is a 1 s check with free memory far above
// the floor (spoond-154), not the old 15 s — while the near-floor fast
// interval stays a second too (spoond-90ia).
func TestPressureChecksEverySecond(t *testing.T) {
	if pressureInterval != time.Second {
		t.Fatalf("pressureInterval = %s, want 1s (spoond-154)", pressureInterval)
	}
	if pressureFastInterval != time.Second {
		t.Fatalf("pressureFastInterval = %s, want 1s", pressureFastInterval)
	}
	svc, sub, ctx, _, _ := giveBackFixture(t, 1, 3072) // 3072 < 2 x 2048
	svc.pressureCheck(ctx)
	if got := svc.pressureNextInterval(); got != time.Second {
		t.Fatalf("next interval near the floor = %s, want 1s", got)
	}
	installDynamicNode(t, svc, sub, 4096, 0, 512) // 8192 MiB free, far above the floor
	svc.pressureCheck(ctx)
	if got := svc.pressureNextInterval(); got != time.Second {
		t.Fatalf("next interval far above the floor = %s, want 1s (a whole second at every distance)", got)
	}
}

// TestPressureVictimLevels: at the floor pressureVictim takes only an
// unpinned lease of an owner over their slice (a lower-ranked over-slice
// owner's over a pinned LRU lease of the top owner) and never a pinned
// lease or an inside-slice owner's; with nothing like that left it
// answers none. Critical, it takes over-slice unpinned leases first,
// then inside-slice owners' unpinned leases (highest used/slice first,
// the no-slice owners after them by memory used, LRU inside an owner),
// and only then a pinned lease, by the same owner ranking with the
// over-slice owners first. Busy and Drained leases stay out at every
// level, pinned or not.
func TestPressureVictimLevels(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	at := func(h int) time.Time { return base.Add(time.Duration(h) * time.Hour) }
	owners := func() []takeBackOwner {
		return []takeBackOwner{
			{Owner: "alice", UsedMiB: 6000, SliceMiB: 1000, Leases: []takeBackLease{
				{ID: "a-pin-old", MemoryMiB: 1000, Pinned: true, LastActive: at(1)},
				{ID: "a-pin-new", MemoryMiB: 1000, Pinned: true, LastActive: at(5)},
				{ID: "a-pin-busy", MemoryMiB: 1000, Pinned: true, Busy: true, LastActive: at(0)},
			}},
			{Owner: "bob", UsedMiB: 2000, SliceMiB: 1000, Leases: []takeBackLease{
				{ID: "b-pin", MemoryMiB: 1000, Pinned: true, LastActive: at(0)},
				{ID: "b-free", MemoryMiB: 1000, LastActive: at(9)},
			}},
			// carol is inside her slice at the lower ratio, dave at the
			// higher one; fred and erin have no slice at all, erin using
			// more.
			{Owner: "carol", UsedMiB: 500, SliceMiB: 1000, Leases: []takeBackLease{
				{ID: "c-inside", MemoryMiB: 500, LastActive: at(0)},
			}},
			{Owner: "dave", UsedMiB: 900, SliceMiB: 1000, Leases: []takeBackLease{
				{ID: "d-pin", MemoryMiB: 300, Pinned: true, LastActive: at(0)},
				{ID: "d-new", MemoryMiB: 300, LastActive: at(8)},
				{ID: "d-old", MemoryMiB: 300, LastActive: at(2)},
			}},
			{Owner: "fred", UsedMiB: 1000, Leases: []takeBackLease{
				{ID: "f-noslice", MemoryMiB: 1000, LastActive: at(0)},
			}},
			{Owner: "erin", UsedMiB: 4000, Leases: []takeBackLease{
				{ID: "e-noslice", MemoryMiB: 4000, LastActive: at(0)},
			}},
		}
	}
	// drop returns a copy of os without the named leases.
	drop := func(os []takeBackOwner, ids ...string) []takeBackOwner {
		gone := map[string]bool{}
		for _, id := range ids {
			gone[id] = true
		}
		out := append([]takeBackOwner(nil), os...)
		for i := range out {
			var kept []takeBackLease
			for _, l := range out[i].Leases {
				if !gone[l.ID] {
					kept = append(kept, l)
				}
			}
			out[i].Leases = kept
		}
		return out
	}
	want := func(name string, os []takeBackOwner, critical bool, id string, pinned, inside bool) {
		t.Helper()
		v, ok := pressureVictim(os, critical)
		if !ok || v.LeaseID != id || v.Pinned != pinned || v.InsideSlice != inside {
			t.Fatalf("%s: victim = %+v ok=%v, want %s (pinned %v, inside %v)", name, v, ok, id, pinned, inside)
		}
	}

	// An unpinned over-slice candidate exists (bob's): it goes, unmarked,
	// at both levels.
	want("floor", owners(), false, "b-free", false, false)
	want("critical", owners(), true, "b-free", false, false)

	// No unpinned lease of an over-slice owner left: at the floor nothing,
	// neither a pinned lease nor an inside-slice owner's.
	if v, ok := pressureVictim(drop(owners(), "b-free"), false); ok {
		t.Fatalf("floor with only pinned and inside-slice leases: victim = %+v, want none", v)
	}
	// Critical: the inside-slice owner with the highest ratio (dave), his
	// LRU unpinned lease, then his next; then carol; then the owners with
	// no slice, erin (more memory used) before fred.
	os := drop(owners(), "b-free")
	want("critical inside", os, true, "d-old", false, true)
	want("critical inside next", drop(os, "d-old"), true, "d-new", false, true)
	want("critical lower ratio", drop(os, "d-old", "d-new"), true, "c-inside", false, true)
	want("critical no slice", drop(os, "d-old", "d-new", "c-inside"), true, "e-noslice", false, true)
	want("critical no slice next", drop(os, "d-old", "d-new", "c-inside", "e-noslice"), true, "f-noslice", false, true)
	// Nothing unpinned anywhere: the top over-slice owner's LRU pinned,
	// non-busy lease, marked Pinned; then bob's; then the inside-slice
	// owner's pin.
	os = drop(owners(), "b-free", "d-old", "d-new", "c-inside", "e-noslice", "f-noslice")
	want("critical pinned", os, true, "a-pin-old", true, false)
	want("critical pinned next owner", drop(os, "a-pin-old", "a-pin-new"), true, "b-pin", true, false)
	want("critical pinned inside", drop(os, "a-pin-old", "a-pin-new", "b-pin"), true, "d-pin", true, true)
	if v, ok := pressureVictim(drop(os, "a-pin-old", "a-pin-new", "b-pin"), false); ok {
		t.Fatalf("floor with only pinned leases: victim = %+v, want none", v)
	}

	// Busy and Drained leases are never taken, at any level, pinned or
	// not.
	os = []takeBackOwner{
		{Owner: "alice", UsedMiB: 6000, SliceMiB: 1000, Leases: []takeBackLease{
			{ID: "a-busy", MemoryMiB: 1000, Pinned: true, Busy: true, LastActive: at(0)},
			{ID: "a-drained", MemoryMiB: 1000, Pinned: true, Drained: true, LastActive: at(1)},
			{ID: "a-free-busy", MemoryMiB: 1000, Busy: true, LastActive: at(0)},
		}},
		{Owner: "carol", UsedMiB: 500, SliceMiB: 1000, Leases: []takeBackLease{
			{ID: "c-busy", MemoryMiB: 250, Busy: true, LastActive: at(0)},
			{ID: "c-drained", MemoryMiB: 250, Drained: true, LastActive: at(1)},
		}},
	}
	for _, critical := range []bool{false, true} {
		if v, ok := pressureVictim(os, critical); ok {
			t.Fatalf("critical=%v: victim = %+v, want none (every lease busy or Drained)", critical, v)
		}
	}
}

// TestPressureCriticalLevel: the critical level holds only strictly
// under MEMORY_CRITICAL_MIB, never with the level at 0, and never with
// a level at or over the floor (the backend turns such a setting off at
// startup; a config set in code gets the same answer).
func TestPressureCriticalLevel(t *testing.T) {
	for _, c := range []struct {
		critical, floor, free int
		want                  bool
	}{
		{2048, 10240, 2047, true},
		{2048, 10240, 2048, false},
		{0, 10240, 0, false},
		{10240, 10240, 100, false},
		{12288, 10240, 100, false},
	} {
		s := &Service{cfg: ServiceConfig{MemoryCriticalMiB: c.critical}}
		if got := s.pressureCritical(c.free, c.floor); got != c.want {
			t.Errorf("critical %d, floor %d, free %d: critical = %v, want %v", c.critical, c.floor, c.free, got, c.want)
		}
	}
}

// TestPressureTakesPinnedLastResort: with every lease of the only
// over-slice owner pinned (one explicitly, one by an in-flight pinned
// exec) and the inside-slice owner's lease busy, a tick between the
// critical level and the floor pauses nothing and raises the lease-less
// memory_floor alert. Below the critical level the floor takes the LRU
// pinned lease anyway, keeps it paused (the post-pause pin re-check does
// not resume it), logs it as CRITICAL and PINNED and raises a critical
// memory_floor alert naming the lease. Admission take-back still refuses
// the other pinned lease.
func TestPressureTakesPinnedLastResort(t *testing.T) {
	svc, sub, ctx := newPressureService(t)
	alice := preemptOwner(t, svc, "alice")
	bob := preemptOwner(t, svc, "bob")
	// bob's one unpinned lease is inside his slice and runs a job: never
	// a candidate, at any level.
	bobL, err := svc.grantLease(ctx, leaseRequest{owner: bob, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant bob: %v", err)
	}
	svc.incRunningJob(bobL.ID)
	old, err := svc.grantLease(ctx, leaseRequest{owner: alice, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant old: %v", err)
	}
	newer, err := svc.grantLease(ctx, leaseRequest{owner: alice, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant newer: %v", err)
	}
	// old is pinned by an exec in flight, newer by its owner.
	svc.incExecPin(old.ID)
	if _, err := svc.setPinned(alice, newer.ID, true); err != nil {
		t.Fatalf("pin: %v", err)
	}
	base := time.Unix(1_000_000, 0)
	svc.store.mu.Lock()
	old.LastActive = base.Add(-2 * time.Hour)
	newer.LastActive = base.Add(-time.Minute)
	bobL.LastActive = base.Add(-10 * time.Hour) // the box's LRU, but busy and inside his slice
	svc.store.mu.Unlock()
	// 3 live sandboxes at 256 pages hold 768 pages, baseUsed 512: used
	// 1280 of the 1536-page (3072 MiB) node, free 512 MiB under the
	// 2048 MiB floor; alice's 2048 MiB is over her 1536 MiB slice.
	installDynamicNode(t, svc, sub, 1536, 512, 256)
	svc.cfg.MemoryFloorMiB = 2048
	svc.cfg.MemoryCriticalMiB = 256 // under the 512 MiB free: the floor's level
	warmFairShares(svc, ctx)

	var logBuf bytes.Buffer
	svc.log = log.New(&logBuf, "", 0)
	events := svc.Subscribe(EventFilter{})
	defer events.Close()
	alertOf := func() *LeaseEvent {
		var alert *LeaseEvent
		for done := false; !done; {
			select {
			case ev := <-events.C:
				if ev.Type == LeaseMemoryFloor {
					ev := ev
					alert = &ev
				}
			default:
				done = true
			}
		}
		return alert
	}

	// At the floor, above the critical level: nothing but pinned leases
	// of an over-slice owner, so nothing is taken.
	if svc.pressureCheck(ctx) {
		t.Fatalf("a tick above the critical level took something; log: %s", logBuf.String())
	}
	if susp := suspendedLeases(svc); len(susp) != 0 {
		t.Fatalf("above the critical level suspended = %v, want none", susp)
	}
	if alert := alertOf(); alert == nil || alert.LeaseID != "" {
		t.Fatalf("memory_floor alert above the critical level = %+v, want the lease-less one", alert)
	}

	// Below the critical level the pinned LRU lease goes.
	svc.cfg.MemoryCriticalMiB = 1024
	logBuf.Reset()
	if !svc.pressureCheck(ctx) {
		t.Fatalf("tick took nothing; want the pinned LRU lease. log: %s", logBuf.String())
	}
	if susp := suspendedLeases(svc); len(susp) != 1 || !susp[old.ID] {
		t.Fatalf("suspended = %v, want exactly the LRU pinned lease %s (kept paused)", susp, old.ID)
	}
	if old.TakeBackFor != pressureRequester {
		t.Fatalf("take-back requester = %q, want %q", old.TakeBackFor, pressureRequester)
	}
	out := logBuf.String()
	if !strings.Contains(out, "memory floor: CRITICAL: 512 MiB free (critical 1024 MiB, floor 2048 MiB): took back lease "+old.ID) ||
		!strings.Contains(out, "over its slice, PINNED)") {
		t.Fatalf("log = %q, want the CRITICAL PINNED take-back line for %s", out, old.ID)
	}
	if !strings.Contains(out, "take back: pausing lease "+old.ID) || !strings.Contains(out, "PINNED (last resort") {
		t.Fatalf("log = %q, want the take-back start line marked PINNED", out)
	}
	alert := alertOf()
	if alert == nil || alert.LeaseID != old.ID || alert.Owner != alice || !strings.HasPrefix(alert.Detail, "CRITICAL: ") || !strings.Contains(alert.Detail, "PINNED") {
		t.Fatalf("memory_floor alert = %+v, want a CRITICAL one naming PINNED lease %s of %s", alert, old.ID, alice)
	}
	rec := &sink{}
	svc.SetNotifier(rec)
	svc.notifyLeaseEvent(*alert)
	if key, sev, _ := rec.at(0); key != "memory.floor.critical."+old.ID || sev != "critical" {
		t.Fatalf("notification = %q/%q, want memory.floor.critical.%s/critical", key, sev, old.ID)
	}

	// Fairness never takes a pinned lease, floor or no floor.
	if err := svc.takeBackPause(ctx, newer, "req", 2.0); !errors.Is(err, errLeaseBusy) {
		t.Fatalf("admission takeBackPause of a pinned lease = %v, want errLeaseBusy", err)
	}
	svc.decExecPin(old.ID)
	svc.decRunningJob(bobL.ID)
}

// TestPressureCriticalTakesAnyOwner: a box where alice is over her
// slice with one unpinned lease and pinned ones, and bob and carol sit
// inside theirs. Between the critical level and the floor the tick
// takes alice's unpinned lease and then nothing: the inside-slice
// owners and the pins are left, with the lease-less alert. Below the
// critical level it takes, one lease per tick, carol's lease (the
// higher ratio inside her slice), then bob's LRU first, and only then
// alice's pinned leases, LRU first — each logged as CRITICAL with the
// owner's slice position and pin state, and each raising a
// lease-scoped memory_floor alert.
func TestPressureCriticalTakesAnyOwner(t *testing.T) {
	svc, sub, ctx := newPressureService(t)
	alice := preemptOwner(t, svc, "alice")
	bob := preemptOwner(t, svc, "bob")
	carol := preemptOwner(t, svc, "carol")
	grant := func(owner, image string, age time.Duration, pinned bool) *Lease {
		t.Helper()
		l, err := svc.grantLease(ctx, leaseRequest{owner: owner, image: image, ttl: time.Hour, persistent: true})
		if err != nil {
			t.Fatalf("grant: %v", err)
		}
		if pinned {
			if _, err := svc.setPinned(owner, l.ID, true); err != nil {
				t.Fatalf("pin: %v", err)
			}
		}
		svc.store.mu.Lock()
		l.LastActive = time.Unix(1_000_000, 0).Add(-age)
		svc.store.mu.Unlock()
		return l
	}
	// Three owners share the 16384 MiB pool below: a 5461 MiB slice
	// each. alice's seven 1024 MiB leases (7168) are over it, and stay
	// over (6144) once her one unpinned lease is gone; carol's 4096 MiB
	// lease (ratio 0.75) and bob's two 1024 MiB leases (0.375) are
	// inside theirs. The box's oldest leases are alice's pins.
	aFree := grant(alice, "mid", time.Hour, false)
	aPinOld := grant(alice, "mid", 40*time.Hour, true)
	var aPins []*Lease
	for i := 0; i < 5; i++ {
		aPins = append(aPins, grant(alice, "mid", time.Duration(30-i)*time.Hour, true))
	}
	bOld := grant(bob, "mid", 5*time.Hour, false)
	bNew := grant(bob, "mid", 3*time.Hour, false)
	c := grant(carol, "big4", 2*time.Hour, false)
	// 10 live sandboxes at 512 pages hold 5120 of the 8192 pages: free
	// 6144 MiB, and every pause frees another 1024, so free stays under
	// the 12288 MiB floor for every tick below.
	installDynamicNode(t, svc, sub, 8192, 0, 512)
	svc.cfg.MemoryFloorMiB = 12288
	svc.cfg.MemoryCriticalMiB = 4096 // under the free memory: the floor's level
	warmFairShares(svc, ctx)

	var logBuf bytes.Buffer
	svc.log = log.New(&logBuf, "", 0)
	events := svc.Subscribe(EventFilter{})
	defer events.Close()
	floorAlerts := func() []LeaseEvent {
		var out []LeaseEvent
		for {
			select {
			case ev := <-events.C:
				if ev.Type == LeaseMemoryFloor {
					out = append(out, ev)
				}
			default:
				return out
			}
		}
	}

	// At the floor: alice's unpinned lease, then nothing.
	if !svc.pressureCheck(ctx) {
		t.Fatalf("first tick took nothing; log: %s", logBuf.String())
	}
	if susp := suspendedLeases(svc); len(susp) != 1 || !susp[aFree.ID] {
		t.Fatalf("suspended = %v, want alice's unpinned lease %s", susp, aFree.ID)
	}
	if strings.Contains(logBuf.String(), "CRITICAL") {
		t.Fatalf("a take above the critical level logged CRITICAL: %s", logBuf.String())
	}
	if as := floorAlerts(); len(as) != 0 {
		t.Fatalf("a take above the critical level alerted: %+v", as)
	}
	warmFairShares(svc, ctx)
	if svc.pressureCheck(ctx) {
		t.Fatalf("a tick above the critical level took an inside-slice or pinned lease; suspended %v", suspendedLeases(svc))
	}
	if as := floorAlerts(); len(as) != 1 || as[0].LeaseID != "" {
		t.Fatalf("alerts = %+v, want the one lease-less memory_floor alert", as)
	}

	// Critical: free runs 7168 to 10240 MiB over the ticks below, all
	// under an 11264 MiB critical level.
	svc.cfg.MemoryCriticalMiB = 11264
	for i, want := range []struct {
		l    *Lease
		desc string
	}{
		{c, "inside its slice, unpinned"},
		{bOld, "inside its slice, unpinned"},
		{bNew, "inside its slice, unpinned"},
		{aPinOld, "over its slice, PINNED"},
	} {
		warmFairShares(svc, ctx)
		logBuf.Reset()
		if !svc.pressureCheck(ctx) {
			t.Fatalf("critical tick %d took nothing; log: %s", i, logBuf.String())
		}
		if susp := suspendedLeases(svc); len(susp) != i+2 || !susp[want.l.ID] {
			t.Fatalf("critical tick %d: suspended = %v, want %s added; log: %s", i, susp, want.l.ID, logBuf.String())
		}
		if out := logBuf.String(); !strings.Contains(out, "memory floor: CRITICAL: ") ||
			!strings.Contains(out, "took back lease "+want.l.ID) || !strings.Contains(out, want.desc+")") {
			t.Fatalf("critical tick %d: log = %q, want a CRITICAL line for %s (%s)", i, out, want.l.ID, want.desc)
		}
		if as := floorAlerts(); len(as) != 1 || as[0].LeaseID != want.l.ID || !strings.Contains(as[0].Detail, want.desc) {
			t.Fatalf("critical tick %d: alerts = %+v, want one naming %s (%s)", i, as, want.l.ID, want.desc)
		}
	}
	for _, p := range aPins {
		if p.Suspended {
			t.Fatalf("pinned lease %s went before the LRU pin", p.ID)
		}
	}
	if got := counterValue(t, svc.metrics.MemoryPressureTakebacksTotal); got != 5 {
		t.Fatalf("take-back counter = %v, want 5", got)
	}
}

// TestPressurePinMidPauseOfUnpinnedVictimWins: the floor's last-resort
// policy is for a victim pressureVictim handed out as pinned. A lease it
// picked unpinned and that its owner pins while the pause runs is
// resumed at once (the pin wins, as for admission take-back), and the
// tick counts nothing as taken.
func TestPressurePinMidPauseOfUnpinnedVictimWins(t *testing.T) {
	svc, sub, ctx := newPressureService(t)
	alice := preemptOwner(t, svc, "alice")
	bob := preemptOwner(t, svc, "bob")
	var a []*Lease
	for i := 0; i < 3; i++ {
		l, err := svc.grantLease(ctx, leaseRequest{owner: alice, image: "mid", ttl: time.Hour, persistent: true})
		if err != nil {
			t.Fatalf("grant alice %d: %v", i, err)
		}
		a = append(a, l)
	}
	if _, err := svc.grantLease(ctx, leaseRequest{owner: bob, image: "mid", ttl: time.Hour, persistent: true}); err != nil {
		t.Fatalf("grant bob: %v", err)
	}
	svc.store.mu.Lock()
	a[0].LastActive = time.Unix(1_000_000, 0) // alice's LRU: the victim
	svc.store.mu.Unlock()
	// 4 live sandboxes at 256 pages hold 1024 pages, baseUsed 512: used
	// 1536 of the 2048-page (4096 MiB) node, free 1024 MiB under the
	// 2048 MiB floor. Two owners: a 2048 MiB slice each, alice's 3072
	// over it. The paused lease fits back (free 1536 MiB after the
	// pause; an un-done pause is not held to the floor).
	installDynamicNode(t, svc, sub, 2048, 512, 256)
	svc.cfg.MemoryFloorMiB = 2048
	warmFairShares(svc, ctx)
	svc.pauseBeforeSuspend = func(l *Lease) {
		svc.pauseBeforeSuspend = nil
		if _, err := svc.setPinned(alice, l.ID, true); err != nil {
			t.Errorf("pin mid-pause: %v", err)
		}
	}
	if svc.pressureCheck(ctx) {
		t.Fatal("tick counted a take-back the pin un-did")
	}
	svc.store.mu.Lock()
	pinned, suspended := a[0].Pinned, a[0].Suspended
	svc.store.mu.Unlock()
	if !pinned || suspended {
		t.Fatalf("alice's LRU pinned=%v suspended=%v, want pinned and running again", pinned, suspended)
	}
}
