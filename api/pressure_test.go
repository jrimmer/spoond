package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v3/metrics"
	"github.com/jrimmer/spoond/v3/substrate"
)

// Low-memory take-back tests (MEMORY_FLOOR_MIB, spoond-l3lq). The loop
// (api/pressure.go) is driven one tick at a time through pressureCheck:
// below the floor it pauses exactly one lease through the ordinary
// take-back rules, at or above it nothing moves, and a floor nothing
// can be taken back for alerts at most once per 10 min.

// newPressureService is a take-back service with metrics installed, a
// roomy snapshot disk and a huge node the tests resize dynamically (a
// pause really frees its pages, as on a real host).
func newPressureService(t *testing.T) (*Service, *testSub, context.Context) {
	t.Helper()
	svc, db, sub := newTestService(t)
	svc.SetMetrics(metrics.NewBackendMetrics())
	seedImage(t, db, "mid", 1024)
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

	// 10 live sandboxes hold 5120 of the 8192 pages; baseUsed 1536 pages
	// leaves 1536 pages = 3072 MiB free, under the 4096 MiB floor.
	installDynamicNode(t, svc, sub, 8192, 1536, 512)
	svc.cfg.MemoryFloorMiB = DefaultMemoryFloorMiB
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

	// The pause freed its pages: the next tick reads the floor met and
	// pauses nothing further.
	svc.pressureCheck(ctx)
	if susp = suspendedLeases(svc); len(susp) != 1 {
		t.Fatalf("after the second tick suspended = %v, want still exactly one", susp)
	}
	if got := counterValue(t, svc.metrics.MemoryPressureTakebacksTotal); got != 1 {
		t.Fatalf("take-back counter after the second tick = %v, want 1", got)
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
	installDynamicNode(t, svc, sub, 8192, 1536, 512) // free = 3072 MiB
	svc.cfg.MemoryFloorMiB = 3072                    // exactly the free reading
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
	// Three owners over a 4096 MiB pool: a 1365 MiB slice each, so
	// alice's 3072 MiB is over it and bob's and carol's 1024 inside.
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

	// 5 live sandboxes hold 2560 of the 4096 pages: free = 3072 MiB,
	// under the 4096 MiB floor by exactly one lease's 1024 MiB.
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	svc.cfg.MemoryFloorMiB = DefaultMemoryFloorMiB
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

// TestPressureFloorZeroDoesNothing: with the floor off the tick returns
// before even reading the node — no pause, no alert, no RPC.
func TestPressureFloorZeroDoesNothing(t *testing.T) {
	svc, sub, ctx := newPressureService(t)
	alice := preemptOwner(t, svc, "alice")
	for i := 0; i < 3; i++ {
		if _, err := svc.grantLease(ctx, leaseRequest{owner: alice, image: "mid", ttl: time.Hour, persistent: true}); err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
	}
	installDynamicNode(t, svc, sub, 4096, 3000, 512) // far below any floor
	svc.cfg.MemoryFloorMiB = 0                       // off
	warmFairShares(svc, ctx)
	nCalls := len(sub.CallLog())

	svc.pressureCheck(ctx)

	if susp := suspendedLeases(svc); len(susp) != 0 {
		t.Fatalf("floor off suspended = %v, want none", susp)
	}
	if got := len(sub.CallLog()); got != nCalls {
		t.Fatalf("floor off made %d substrate calls, want 0", got-nCalls)
	}
	if got := counterValue(t, svc.metrics.MemoryPressureTakebacksTotal); got != 0 {
		t.Fatalf("take-back counter = %v, want 0", got)
	}
}

// TestPressureAlertOncePerTenMinutes: below a floor nothing can be
// taken back for (every candidate pinned), the tick raises the box-full
// alert; an immediately following tick stays quiet, and one past the
// ten-minute window repeats it.
func TestPressureAlertOncePerTenMinutes(t *testing.T) {
	svc, sub, ctx := newPressureService(t)
	alice := preemptOwner(t, svc, "alice")
	bob := preemptOwner(t, svc, "bob")
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
	bobL, err := svc.grantLease(ctx, leaseRequest{owner: bob, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant bob: %v", err)
	}

	now := time.Now()
	svc.now = func() time.Time { return now }
	// 4 live sandboxes hold 2048 pages and baseUsed 1024: free = 2048
	// MiB, under the 4096 MiB floor by 2048 MiB. Alice (3 leases) is
	// over her slice but every lease of hers is pinned; bob is inside
	// his slice — nothing can be taken back.
	installDynamicNode(t, svc, sub, 4096, 1024, 512)
	svc.cfg.MemoryFloorMiB = DefaultMemoryFloorMiB
	warmFairShares(svc, ctx)

	events := svc.Subscribe(EventFilter{})
	defer events.Close()
	// collectAlerts reads the events buffered while the test ran a tick.
	collectAlerts := func() int {
		n := 0
		for {
			select {
			case ev := <-events.C:
				if ev.Type == LeaseBoxFull {
					n++
					if !strings.Contains(ev.Detail, "memory below floor") {
						t.Fatalf("alert detail = %q, want the floor named", ev.Detail)
					}
				}
			default:
				return n
			}
		}
	}

	svc.pressureCheck(ctx)
	if got := collectAlerts(); got != 1 {
		t.Fatalf("first below-floor tick raised %d alerts, want 1", got)
	}

	// Ten seconds later: still the same 10-minute window, no repeat.
	now = now.Add(10 * time.Second)
	svc.pressureCheck(ctx)
	if got := collectAlerts(); got != 0 {
		t.Fatalf("a tick inside the 10-minute window raised %d alerts, want 0", got)
	}
	if susp := suspendedLeases(svc); len(susp) != 0 {
		t.Fatalf("suspended = %v, want none (every candidate pinned)", susp)
	}

	// Ten minutes on: the alert may repeat.
	now = now.Add(pressureAlertEvery)
	svc.pressureCheck(ctx)
	if got := collectAlerts(); got != 1 {
		t.Fatalf("a tick past the 10-minute window raised %d alerts, want 1", got)
	}
	if bobL.Suspended {
		t.Fatal("bob's lease was paused")
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
