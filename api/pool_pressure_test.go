package api

import (
	"context"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v3/store"
	"github.com/jrimmer/spoond/v3/substrate"
)

// Warm-pool tests for the overcommit cap and the low-memory floor
// (#41tk): a pool VM holds its hugepages around the clock, so it must
// count toward MEMORY_OVERCOMMIT (runningMemoryMiB), a warm-pool fill
// must stop short of the cap and of the floor, and the low-memory tick
// must shrink the pool before it pauses any lease.

// TestRunningMemoryMiBCountsPool: two stocked pool VMs count into
// runningMemoryMiB; taking one by a grant moves the charge onto the
// lease (counted once, no double), and dropping one empties it out.
func TestRunningMemoryMiBCountsPool(t *testing.T) {
	svc, db, sub := newTestService(t)
	img := seedImage(t, db, "py-base", 2048)
	svc.cfg.PoolSize = 2
	ctx := context.Background()

	svc.warmPool(ctx, img)
	if got := svc.runningMemoryMiB(); got != 4096 {
		t.Fatalf("after warming 2x2048 MiB: runningMemoryMiB = %d, want 4096", got)
	}

	// A grant pops one pool sandbox: total is unchanged (one counted as
	// the pool entry, one as the lease).
	if _, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if got := svc.runningMemoryMiB(); got != 4096 {
		t.Fatalf("after the pooled grant: runningMemoryMiB = %d, want 4096 (2048 pool + 2048 lease)", got)
	}

	// Drop the remaining one: only the lease's 2048 is left.
	svc.store.mu.Lock()
	remain := append([]string(nil), svc.store.pool["py-base"]...)
	svc.store.mu.Unlock()
	if len(remain) != 1 {
		t.Fatalf("pool holds %d entries after the grant, want 1", len(remain))
	}
	svc.dropOnePoolSandbox(ctx)
	if got := svc.runningMemoryMiB(); got != 2048 {
		t.Fatalf("after dropping the last pool sandbox: runningMemoryMiB = %d, want 2048", got)
	}
	// The substrate guest and the rows really went.
	for _, c := range sub.CallLog() {
		if c == "Delete "+remain[0] {
			return
		}
	}
	t.Fatalf("pooled sandbox %s was not deleted; calls: %v", remain[0], sub.CallLog())
}

// TestWarmPoolStopsAtOvercommitCap: the cap (1.0 x an 8 GiB pool) is
// already full of leases; a warm-pool fill must stock nothing, not
// take back, and not create.
func TestWarmPoolStopsAtOvercommitCap(t *testing.T) {
	svc, db, sub, ids := newFairShareService(t)
	svc.cfg.MemoryOvercommit = 1.0
	alice := addIdentityUser(t, ids, "alice")
	ctx := context.Background()
	img := seedImage(t, db, "one", 1024)
	roomyDisk(svc)
	// Fill the cap with eight 1 GiB leases (8 GiB pool at ratio 1.0).
	for i := 0; i < 8; i++ {
		fullFreeNode(t, svc, sub)
		if _, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "one", ttl: time.Hour, persistent: true}); err != nil {
			t.Fatalf("grant %d of 8: %v", i+1, err)
		}
	}
	fullFreeNode(t, svc, sub) // every page reads free; the cap is full
	dropNodeCache(svc)
	svc.updateNodeMetrics(ctx) // the fill reads the cache; leases warmed it
	svc.cfg.PoolSize = 1

	svc.warmPool(ctx, img)

	svc.store.mu.Lock()
	n := len(svc.store.pool["one"])
	svc.store.mu.Unlock()
	if n != 0 {
		t.Fatalf("warm pool stocked %d sandbox(es) past the overcommit cap, want 0", n)
	}
	if got := calls(sub.Fake, "Create"); got != 0 {
		t.Fatalf("warm pool made %d create(s) past the cap, want 0", got)
	}
	for _, c := range sub.CallLog() {
		if c == "Pause" || (len(c) > 6 && c[:6] == "Pause ") {
			t.Fatalf("warm-pool fill took back a lease; calls: %v", sub.CallLog())
		}
	}
	// With the cap cleared (one lease suspended), the same fill stocks.
	l := svcStoreLeases(t, svc)
	svc.suspend(ctx, alice.ID, l[0].ID)
	fullFreeNode(t, svc, sub)
	svc.warmPool(ctx, img)
	svc.store.mu.Lock()
	n = len(svc.store.pool["one"])
	svc.store.mu.Unlock()
	if n != 1 {
		t.Fatalf("after room freed, warm pool stocked %d sandbox(es), want 1", n)
	}
}

// TestWarmPoolStopsBelowFreeFloor: an 8 GiB cache reading with 2.5 GiB
// free carries the floor (2048 MiB); a 1024 MiB pool VM fits. A
// reading with 2 GiB free does not: the fill stops with no create.
func TestWarmPoolStopsBelowFreeFloor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		freeB   uint64
		wantN   int
		wantCre int
	}{
		{"fits above the floor with headroom exact", uint64(3072) << 20, 1, 1},
		{"stops at the free-memory floor", uint64(2048) << 20, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, db, sub := newTestService(t)
			img := seedImage(t, db, "py-base", 1024)
			sub.SetNodeInfo(substrate.NodeInfo{Status: "healthy", HugepagesTotal: 8192, HugepageSizeBytes: 2 << 20, HugepagesUsed: 8192*2048/1024/1024 - tc.freeB/(2<<20)}, nil)
			dropNodeCache(svc)
			svc.updateNodeMetrics(context.Background())
			svc.cfg.PoolSize = 1
			dropNodeCache(svc)
			svc.updateNodeMetrics(context.Background()) // warm the fill's free-memory cache

			svc.warmPool(context.Background(), img)

			svc.store.mu.Lock()
			n := len(svc.store.pool["py-base"])
			svc.store.mu.Unlock()
			if n != tc.wantN {
				t.Fatalf("warm pool stocked %d sandbox(es), want %d", n, tc.wantN)
			}
			if got := calls(sub.Fake, "Create"); got != tc.wantCre {
				t.Fatalf("warm pool made %d create(s), want %d", got, tc.wantCre)
			}
		})
	}
}

// TestDropOnePoolSandbox: dropping from a pool of a 2048 and a 1024 MiB
// image frees the 2048 image's NEWEST member and clears its pool and
// poolMiB entries and its sandboxes row; an empty pool answers
// ok=false.
func TestDropOnePoolSandbox(t *testing.T) {
	svc, db, sub := newTestService(t)
	img := seedImage(t, db, "py-base", 2048)
	img2 := seedImage(t, db, "small", 1024)
	ctx := context.Background()

	if freed, ok := svc.dropOnePoolSandbox(ctx); ok || freed != 0 {
		t.Fatalf("empty pool: dropOnePoolSandbox = (%d, %v), want (0, false)", freed, ok)
	}

	svc.cfg.PoolSize = 2
	svc.warmPool(ctx, img)
	if _, err := sub.Create(ctx, substrate.CreateRequest{SandboxID: "small-a", BuildID: img2.CurrentBuildID}); err != nil {
		t.Fatal(err)
	}
	svc.store.mu.Lock()
	svc.store.pool["small"] = []string{"small-a"}
	svc.store.poolMiB["small-a"] = 1024
	ids := append([]string(nil), svc.store.pool["py-base"]...)
	svc.store.mu.Unlock()
	droppedID := ids[len(ids)-1] // the 2048 MiB image's newest member
	svc.db.AddPool(ctx, "small-a", "small")
	svc.db.UpsertSandbox(ctx, storeSandboxRowFor("small-a", img2.CurrentBuildID, "", 1024))

	freed, ok := svc.dropOnePoolSandbox(ctx)
	if !ok {
		t.Fatal("dropOnePoolSandbox on a stocked pool answered ok=false")
	}
	if freed != 2048 {
		t.Fatalf("freed = %d MiB, want 2048 (the fuller image's newest member)", freed)
	}
	svc.store.mu.Lock()
	n1, n2 := len(svc.store.pool["py-base"]), len(svc.store.pool["small"])
	_, mi1 := svc.store.poolMiB[svc.store.pool["py-base"][0]], svc.store.poolMiB["small-a"]
	_, gone := svc.store.poolMiB[droppedID]
	svc.store.mu.Unlock()
	if n1 != 1 || n2 != 1 {
		t.Fatalf("pool sizes after the drop: py-base %d, small %d; want 1 and 1 (small untouched)", n1, n2)
	}
	if gone {
		t.Fatal("the dropped pool entry's poolMiB was kept")
	}
	_ = mi1
	svc.store.mu.Lock()
	keptIDs := append([]string(nil), svc.store.pool["py-base"]...)
	svc.store.mu.Unlock()
	if got := calls(sub.Fake, "Delete "+keptIDs[0]); got != 0 {
		t.Fatalf("a non-dropped pooled sandbox was deleted %d time(s)", got)
	}
	if row, err := svc.db.GetSandbox(ctx, "small-a"); err != nil || row.SandboxID != "small-a" {
		t.Fatalf("the untouched pool image's sandboxes row: %v, %v", row, err)
	}
}

// TestPressureTickDropsPoolBeforeLeases: below the floor with a
// stocked pool AND a takeable over-slice lease, the tick drops the
// pool sandbox and pauses NO lease; the next tick — the pool gone and
// its pages back — takes the lease, as the empty pool falls through.
func TestPressureTickDropsPoolBeforeLeases(t *testing.T) {
	svc, sub, ctx := newPressureService(t)
	alice := preemptOwner(t, svc, "alice")
	bob := preemptOwner(t, svc, "bob")
	// Two owners share the 8192 MiB pool below: a 4096 MiB slice each.
	// Alice's five 1024 MiB leases put her over; bob's single 1024 is
	// inside his.
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
	base := time.Unix(1_000_000, 0)
	svc.store.mu.Lock()
	for i, l := range leases {
		l.LastActive = base.Add(-time.Duration(len(leases)-i) * time.Hour) // leases[0] is the LRU
	}
	svc.store.mu.Unlock()
	svc.cfg.PoolSize = 1
	img := midImage()
	svc.warmPool(ctx, img)

	// 6 live sandboxes at 512 pages hold 3072 of the 4096 pages,
	// baseUsed 512: used 3584 pages, free = 1024 MiB, far below the
	// 4096 MiB floor.
	installDynamicNode(t, svc, sub, 4096, 512, 512)
	svc.cfg.MemoryFloorMiB = 4096
	warmFairShares(svc, ctx)

	svc.pressureCheck(ctx)

	if susp := suspendedLeases(svc); len(susp) != 0 {
		t.Fatalf("a tick with a stocked pool suspended %v, want none (the pool VM goes first)", susp)
	}
	if got := counterValue(t, svc.metrics.MemoryPressureTakebacksTotal); got != 0 {
		t.Fatalf("take-back counter = %v, want 0 (no lease was paused)", got)
	}
	svc.store.mu.Lock()
	poolLen := len(svc.store.pool["mid"])
	svc.store.mu.Unlock()
	if poolLen != 0 {
		t.Fatalf("the tick left %d pool sandbox(es), want 0", poolLen)
	}

	// The pool VM's pages are back (its sandbox is gone from the fake's
	// List): free = 1536 pages = 3072 MiB, still below the floor, and
	// the pool is empty — the tick takes the lease now.
	l := leases[0]
	if ids := svc.sandboxesLiveIDs(t); len(ids) != 6 {
		t.Fatalf("%d sandboxes live after the pool drop, want 6", len(ids))
	}
	svc.pressureCheck(ctx)
	if susp := suspendedLeases(svc); len(susp) != 1 || !susp[l.ID] {
		t.Fatalf("suspended = %v, want exactly the LRU lease %s once the pool is empty", susp, l.ID)
	}
}

// TestPressureTickDropsPoolSurvivesDeleteFailure: a substrate delete
// that fails still clears the pool entry — the drop is not retried
// for ever, and the orphan sweep owns the leftover.
func TestPressureTickDropsPoolSurvivesDeleteFailure(t *testing.T) {
	svc, sub, ctx := newPressureService(t)
	img := midImage()
	svc.cfg.PoolSize = 1
	svc.warmPool(ctx, img)
	svc.store.mu.Lock()
	id := svc.store.pool["mid"][0]
	svc.store.mu.Unlock()
	sub.deleteFn = func(ctx context.Context, sandboxID string) error {
		return context.DeadlineExceeded
	}
	sub.SetNodeInfo(substrate.NodeInfo{Status: "healthy", HugepagesTotal: 4096, HugepageSizeBytes: 2 << 20, HugepagesUsed: 4096}, nil)
	dropNodeCache(svc)
	svc.updateNodeMetrics(ctx)
	svc.cfg.MemoryFloorMiB = 4096

	// warmFairShares also warms the fair snapshot; nobody holds a lease,
	// so an empty-pool fall-through would alert rather than touch one.
	warmFairShares(svc, ctx)
	svc.pressureCheck(ctx)

	svc.store.mu.Lock()
	poolLen := len(svc.store.pool["mid"])
	_, mi := svc.store.poolMiB[id]
	svc.store.mu.Unlock()
	if poolLen != 0 || mi {
		t.Fatalf("the pool entry survived the tick (len %d, poolMiB %v), want it cleared", poolLen, mi)
	}
	sub.deleteFn = nil
}

// midImage returns the "mid" image row the pressure fixtures seed:
// warmPool only reads MemoryMB (CurrentBuildID is looked up by name).
func midImage() store.ImageRow {
	return store.ImageRow{Name: "mid", MemoryMB: 1024}
}

// sandboxesLiveIDs lists the sandbox ids the test substrate still
// holds.
func (svc *Service) sandboxesLiveIDs(t *testing.T) []string {
	t.Helper()
	return svc.sub.(*testSub).sandboxesLive(t)
}

// svcStoreLeases returns every lease currently in the store, in map-
// random order (the callers need only one lease id, order aside).
func svcStoreLeases(t *testing.T, svc *Service) []*Lease {
	t.Helper()
	svc.store.mu.Lock()
	defer svc.store.mu.Unlock()
	out := make([]*Lease, 0, len(svc.store.leases))
	for _, l := range svc.store.leases {
		out = append(out, l)
	}
	return out
}

// storeSandboxRowFor builds a sandboxes row for id on buildID with the
// given memory, as the pool fixtures need.
func storeSandboxRowFor(id, buildID string, _ string, memoryMB int) store.SandboxRow {
	return store.SandboxRow{SandboxID: id, BuildID: buildID, MemoryMB: memoryMB}
}
