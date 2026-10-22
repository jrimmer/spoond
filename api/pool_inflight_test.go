package api

// The in-flight holds behind the overcommit cap (spoond-fi5v): a VM
// whose lease row is not committed yet must still count in the cap,
// whether it is a pooled grant's popped sandbox or the warm-pool fill's
// create in flight. Both hold their hugepages from the moment they
// exist, so a cold admission racing them must see the memory promised.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v4/metrics"
	"github.com/jrimmer/spoond/v4/substrate"
)

// overcommitShortfallHere reads the box's cap shortfall for a 1024 MiB
// request through the production door (the same figure
// overcommitShortfallLocked sums): running leases plus the in-flight
// counter against ratio x the cached pool.
func overcommitShortfallHere(t *testing.T, svc *Service, memoryMB int) int {
	t.Helper()
	return svc.overcommitShortfall(memoryMB)
}

// inflightAdmitMiBNow reads the in-flight counter.
func inflightAdmitMiBNow(svc *Service) int {
	svc.nodeInfoMu.Lock()
	defer svc.nodeInfoMu.Unlock()
	return svc.inflightAdmitMiB
}

// TestPooledGrantInflightDuringGrant: while a pooled grant is blocked
// inside the substrate's Health (a fake Health that hangs on the popped
// VM until the test lets it), the box's overcommit cap and the in-flight
// counter already see the VM's MiB — a cold admission racing the grant
// cannot pass a cap the box is past — and once the grant settles the
// count is exact: the lease holds the MiB and nothing is left in
// flight.
func TestPooledGrantInflightDuringGrant(t *testing.T) {
	svc, db, sub, ids := newFairShareService(t)
	svc.cfg.MemoryOvercommit = 1.0
	alice := addIdentityUser(t, ids, "alice")
	svc.probeEnabled = false
	ctx := context.Background()
	img := seedImage(t, db, "py-base", 2048)
	roomyDisk(svc)
	// Four 2 GiB leases of an 8 GiB pool at ratio 1.0 fill the cap;
	// suspending one makes exact room for the fill: three live leases
	// plus the pool VM sit at the cap (8192), which a stock is allowed
	// to reach but not pass.
	for i := 0; i < 4; i++ {
		fullFreeNode(t, svc, sub)
		if _, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "py-base", ttl: time.Hour, persistent: true}); err != nil {
			t.Fatalf("lease %d of 4: %v", i+1, err)
		}
	}
	l := svcStoreLeases(t, svc)
	if _, err := svc.suspend(ctx, alice.ID, l[0].ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	fullFreeNode(t, svc, sub)
	svc.cfg.PoolSize = 1
	svc.warmPool(ctx, img)
	svc.store.mu.Lock()
	n := len(svc.store.pool["py-base"])
	svc.store.mu.Unlock()
	if n != 1 {
		t.Fatalf("setup: pool stocked %d sandbox(es), want 1", n)
	}
	if got := svc.runningMemoryMiB(); got != 8192 {
		t.Fatalf("setup: running %d MiB, want 8192 (three 2048 leases + the pool VM) at the cap", got)
	}
	fullFreeNode(t, svc, sub)
	dropNodeCache(svc)
	svc.updateNodeMetrics(ctx)
	// Hold every Health the grant makes: the pop's validation blocks,
	// the lease row is not committed, the VM is still counted in the
	// pool's poolMiB — the shape the in-flight raise exists for.
	releaseHealth := make(chan struct{})
	sub.setHealthFn(func(ctx context.Context, sandboxID string) error {
		<-releaseHealth
		return nil
	})
	defer sub.setHealthFn(nil)
	done := make(chan struct{})
	var granted *Lease
	var gerr error
	go func() {
		defer close(done)
		usr := alice.ID
		granted, gerr = svc.grantLease(ctx, leaseRequest{owner: usr, image: "py-base", ttl: time.Hour, persistent: true})
	}()
	deadline := time.After(5 * time.Second)
	for inflightAdmitMiBNow(svc) != 2048 {
		svc.store.mu.Lock()
		pl := len(svc.store.pool["py-base"])
		svc.store.mu.Unlock()
		select {
		case <-deadline:
			t.Fatalf("the popped VM never reached the in-flight figure, or the grant did not block: in flight %d, pool %d", inflightAdmitMiBNow(svc), pl)
		case <-time.After(time.Millisecond):
		}
	}
	// In flight, the cap sees the VM: the pop moved the VM out of the
	// pool's permanent ledger and into the in-flight counter (three 2048
	// leases = 6144, plus the pop's promise), so one more 1024 MiB
	// admission would pass the 8192 MiB cap — the same figure a cold
	// admission's cap check runs.
	if short := overcommitShortfallHere(t, svc, 1024); short <= 0 {
		t.Fatalf("overcommitShortfall(1024) = %d while the pool grant is in flight, want > 0 (the box is at the cap)", short)
	}
	// Still blocked: the lease row is not committed yet.
	select {
	case <-done:
		t.Fatal("the grant returned before Health was released")
	default:
	}
	close(releaseHealth)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the grant never returned after Health was released")
	}
	if gerr != nil {
		t.Fatalf("the pooled grant on the box at the cap: %v", gerr)
	}
	if granted == nil || !granted.pooled {
		t.Fatal("the grant did not come from the pool")
	}
	// Settled: the lease holds the MiB and the in-flight counter went
	// back to nothing. The box is back exactly at the cap with no
	// in-flight help (shortfall(0) = 0); a double-counted VM would read
	// 10240 and answer 2048 here.
	if got := inflightAdmitMiBNow(svc); got != 0 {
		t.Fatalf("after the grant: in flight %d MiB, want 0", got)
	}
	if got := overcommitShortfallHere(t, svc, 0); got != 0 {
		t.Fatalf("after the grant: shortfall(0) = %d, want 0 (three 2 GiB leases + the granted VM = 8192 MiB on an 8192 MiB pool at ratio 1.0)", got)
	}
	if got := svc.runningMemoryMiB(); got != 8192 {
		t.Fatalf("after the grant: running %d MiB, want 8192 (the pool VM became the lease, counted once)", got)
	}
}

// TestWarmPoolFillInflightDuringFill: while the warm-pool fill is
// blocked inside the substrate's Create, the box's overcommit cap and
// the in-flight counter already see the VM the fill decided to stock —
// a cold admission racing the fill cannot pass a cap the fill is about
// to fill — and once the VM is stocked (or dropped) the hold is
// released: the pool's poolMiB takes over the counting and nothing is
// left in flight.
func TestWarmPoolFillInflightDuringFill(t *testing.T) {
	svc, db, sub := newTestService(t)
	svc.cfg.MemoryOvercommit = DefaultMemoryOvercommit
	ctx := context.Background()
	img := seedImage(t, db, "py-base", 2048)
	roomyDisk(svc)
	// An 8 GiB pool at 1.25 gives a 10240 MiB cap; two live 2 GiB leases
	// and the 2 GiB VM the fill stocks sit well inside it.
	fullFreeNode(t, svc, sub)
	for i := 0; i < 2; i++ {
		fullFreeNode(t, svc, sub)
		if _, err := svc.grantLease(ctx, leaseRequest{owner: "consumer-a", image: "py-base", ttl: time.Hour, persistent: true}); err != nil {
			t.Fatalf("lease %d of 2: %v", i+1, err)
		}
	}
	fullFreeNode(t, svc, sub)
	dropNodeCache(svc)
	svc.updateNodeMetrics(ctx)
	svc.cfg.PoolSize = 1
	// Hold every Create the fill makes: the VM exists on the fake but
	// nothing has stocked it — runningMemoryMiB does not count it and
	// the cap must not be blind to it.
	releaseCreate := make(chan struct{})
	sub.SetCreateFn(func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		<-releaseCreate
		return sub.Fake.Create(ctx, req)
	})
	defer sub.SetCreateFn(nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.warmPool(ctx, img)
	}()
	deadline := time.After(5 * time.Second)
	for inflightAdmitMiBNow(svc) != 2048 {
		select {
		case <-deadline:
			t.Fatalf("the fill's VM never reached the in-flight figure, or the fill did not block: in flight %d", inflightAdmitMiBNow(svc))
		case <-time.After(time.Millisecond):
		}
	}
	// In flight, a cold admission sees the VM: two live 2048 leases hold
	// 4096 (in runningMemoryMiB), the fill's promise rides the in-flight
	// counter (runningMemoryMiB is blind to it), so a 5120 MiB request
	// sits 1024 over the 10240 cap — a cap blind to the fill would
	// answer 0 and admit it.
	if short := overcommitShortfallHere(t, svc, 5120); short != 1024 {
		t.Fatalf("overcommitShortfall(5120) while the fill is in flight = %d, want 1024 (4096 live + 4096 in flight + 5120 - 10240)", short)
	}
	close(releaseCreate)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the fill never returned after Create was released")
	}
	// Stocked: the pool's poolMiB counts the VM from now on, the hold is
	// gone, and the cap is unchanged from what runningMemoryMiB sees.
	if got := inflightAdmitMiBNow(svc); got != 0 {
		t.Fatalf("after the fill: in flight %d MiB, want 0 (the poolMiB took over the counting)", got)
	}
	if short := overcommitShortfallHere(t, svc, 5120); short != 1024 {
		t.Fatalf("after the stock: shortfall(5120) = %d, want 1024 (6144 counted + 5120 - 10240)", short)
	}
	svc.store.mu.Lock()
	n := len(svc.store.pool["py-base"])
	svc.store.mu.Unlock()
	if n != 1 {
		t.Fatalf("the fill stocked %d sandbox(es), want 1", n)
	}
}

// TestPoolFallThroughSaturated: the box is saturated and the pool is
// stocked (poolCanServe true at the gate — the shape the gate skips),
// but by the time the grant reaches the pop the pool is empty and the
// grant falls through to a cold create, which waits on the gate like
// any other new boot. A pool entry stocked in between does not re-open
// the gate: the fall-through gates on the shape fixed at the top.
func TestPoolFallThroughSaturated(t *testing.T) {
	svc, db, sub, ids := newFairShareService(t)
	alice := addIdentityUser(t, ids, "alice")
	svc.SetMetrics(metrics.NewBackendMetrics())
	// The cold create that follows must boot nothing while the test
	// holds it: the fall-through must answer at the gate, so nothing
	// ever reaches Create after the fill's own create.
	ctx := context.Background()
	img := seedImage(t, db, "mid", 1024)
	roomyDisk(svc)
	installDynamicNode(t, svc, sub, 1<<20, 0, 512)
	// The gate and its refusal metric: io pressure over the target.
	dir := t.TempDir()
	svc.pressure.root = dir
	svc.cfg.SaturationIOFullPct, svc.cfg.SaturationCPUSomePct = 20, 75
	writePSI(t, dir, 1, 1)
	svc.cfg.PoolSize = 1
	svc.warmPool(ctx, img)
	svc.store.mu.Lock()
	n := len(svc.store.pool["mid"])
	svc.store.mu.Unlock()
	if n != 1 {
		t.Fatalf("setup: pool stocked %d sandbox(es), want 1", n)
	}
	// Calm at the top of this grant: the gate over poolCanServe is
	// skipped, as a pop boots nothing.
	writePSI(t, dir, 1, 1)
	expirePressure(svc)
	// The pool empties between the top and the pop: the entry is dropped
	// under the test's hand (the same shape an unhealthy or vanished
	// entry leaves), while the box saturates for the fall-through.
	createCallsBefore := calls(sub.Fake, "Create")
	svc.store.mu.Lock()
	id := svc.store.pool["mid"][0]
	n = len(svc.store.pool["mid"])
	svc.store.mu.Unlock()
	if n != 1 {
		t.Fatalf("setup: pool holds %d entries, want 1", n)
	}
	if err := svc.sub.Delete(ctx, id); err != nil {
		t.Fatalf("delete the pool entry: %v", err)
	}
	svc.discardPoolSandbox(ctx, id)
	svc.store.mu.Lock()
	delete(svc.store.pool, "mid")
	delete(svc.store.poolMiB, id)
	svc.store.mu.Unlock()
	// Saturate before the pop reads the (now empty) pool.
	writePSI(t, dir, 30, 1)
	expirePressure(svc)
	_, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "mid", ttl: time.Hour, persistent: true})
	var sat *errHostSaturated
	if !errors.As(err, &sat) || !errors.Is(err, substrate.ErrCapacity) {
		t.Fatalf("the fall-through create on a saturated box = %v, want the saturation gate's wait", err)
	}
	if got := calls(sub.Fake, "Create"); got != createCallsBefore {
		t.Fatalf("the fall-through made %d create(s) under pressure, want none (the gate answered first)", got-createCallsBefore)
	}
}
