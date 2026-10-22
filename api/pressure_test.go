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
	"errors"
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
