package api

import (
	"context"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v3/substrate"
)

// The overcommit cap (MEMORY_OVERCOMMIT, owner 2026-10-14): free-page
// reporting hands freed guest memory back to the host, so the free
// hugepages a node reports can exceed what the running leases may fault
// back in. Admission must never promise more lease memory than
// MEMORY_OVERCOMMIT x the pool, whatever the free pages say.

// fullFreeNode resets the fake node to an 8 GiB pool with every page
// free — what a host looks like after its VMs reported their memory
// back — and refreshes the service's cache from it.
func fullFreeNode(t *testing.T, svc *Service, sub *testSub) {
	t.Helper()
	sub.SetNodeInfo(substrate.NodeInfo{Status: "healthy", HugepagesTotal: 4096, HugepageSizeBytes: 2 << 20}, nil)
	dropNodeCache(svc)
	svc.updateNodeMetrics(context.Background())
}

// TestOvercommitShortfallMath: running + admitted-since-refresh +
// request against ratio x pool; a zero ratio (the cap off) and a cold
// cache never refuse.
func TestOvercommitShortfallMath(t *testing.T) {
	svc := &Service{}
	if got := svc.overcommitShortfallLocked(100000, 1024); got != 0 {
		t.Fatalf("cold cache: shortfall %d, want 0", got)
	}
	svc.nodeInfoCache = substrate.NodeInfo{HugepagesTotal: 4096, HugepageSizeBytes: 2 << 20} // 8192 MiB
	svc.nodeInfoAt = time.Now()
	if got := svc.overcommitShortfallLocked(100000, 1024); got != 0 {
		t.Fatalf("cap off (ratio 0): shortfall %d, want 0", got)
	}
	svc.cfg.MemoryOvercommit = 1.25 // cap 10240 MiB
	if got := svc.overcommitShortfallLocked(9216, 1024); got != 0 {
		t.Fatalf("exactly at the cap: shortfall %d, want 0", got)
	}
	if got := svc.overcommitShortfallLocked(9216, 2048); got != 1024 {
		t.Fatalf("1 GiB over: shortfall %d, want 1024", got)
	}
	svc.inflightAdmitMiB = 1024 // an admission whose lease is not running yet
	if got := svc.overcommitShortfallLocked(8192, 1024); got != 0 {
		t.Fatalf("with the in-flight admission, at the cap: shortfall %d, want 0", got)
	}
	if got := svc.overcommitShortfallLocked(8192, 2048); got != 1024 {
		t.Fatalf("with the in-flight admission, 1 GiB over: shortfall %d, want 1024", got)
	}
}

// TestOvercommitCapRefusesWhenFreePagesSayYes: with MEMORY_OVERCOMMIT=1.0
// on an 8 GiB pool, eight 1 GiB leases fill the cap; after their memory
// is reported back (every page free again) a ninth is still refused.
func TestOvercommitCapRefusesWhenFreePagesSayYes(t *testing.T) {
	svc, db, sub, ids := newFairShareService(t)
	svc.cfg.MemoryOvercommit = 1.0
	alice := addIdentityUser(t, ids, "alice")
	ctx := context.Background()
	seedImage(t, db, "one", 1024)
	roomyDisk(svc)
	for i := 0; i < 8; i++ {
		fullFreeNode(t, svc, sub)
		if _, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "one", ttl: time.Hour, persistent: true}); err != nil {
			t.Fatalf("grant %d of 8: %v", i+1, err)
		}
	}
	fullFreeNode(t, svc, sub)
	if fits, err := svc.fits(ctx, 1024); err != nil || fits {
		t.Fatalf("fits = %v, %v; want false (8 GiB promised on an 8 GiB pool at ratio 1.0)", fits, err)
	}
	if _, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "one", ttl: time.Hour, persistent: true}); err == nil {
		t.Fatal("ninth 1 GiB lease granted past the overcommit cap")
	}
}

// TestOvercommitCapAllowsRatio: at the default 1.25 the same pool takes
// ten 1 GiB leases and refuses the eleventh.
func TestOvercommitCapAllowsRatio(t *testing.T) {
	svc, db, sub, ids := newFairShareService(t)
	svc.cfg.MemoryOvercommit = DefaultMemoryOvercommit
	alice := addIdentityUser(t, ids, "alice")
	ctx := context.Background()
	seedImage(t, db, "one", 1024)
	roomyDisk(svc)
	for i := 0; i < 10; i++ {
		fullFreeNode(t, svc, sub)
		if _, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "one", ttl: time.Hour, persistent: true}); err != nil {
			t.Fatalf("grant %d of 10: %v", i+1, err)
		}
	}
	fullFreeNode(t, svc, sub)
	if _, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "one", ttl: time.Hour, persistent: true}); err == nil {
		t.Fatal("eleventh 1 GiB lease granted past 1.25 x 8 GiB")
	}
}

// TestOvercommitTakeBackClearsCapShortfall: the cap is full of alice's
// leases (she borrows far past her slice) and every page reads free;
// bob's request is short only against the cap, so take-back pauses one
// of alice's leases and bob is granted.
func TestOvercommitTakeBackClearsCapShortfall(t *testing.T) {
	svc, db, sub, ids := newFairShareService(t)
	svc.cfg.MemoryOvercommit = 1.0
	alice := addIdentityUser(t, ids, "alice")
	bob := addIdentityUser(t, ids, "bob")
	ctx := context.Background()
	seedImage(t, db, "one", 1024)
	roomyDisk(svc)
	for i := 0; i < 8; i++ {
		fullFreeNode(t, svc, sub)
		if _, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "one", ttl: time.Hour, persistent: true}); err != nil {
			t.Fatalf("grant alice %d: %v", i+1, err)
		}
	}
	fullFreeNode(t, svc, sub)
	svc.invalidateFairShares()
	if _, err := svc.grantLease(ctx, leaseRequest{owner: bob.ID, image: "one", ttl: time.Hour, persistent: true}); err != nil {
		t.Fatalf("bob's grant: %v (take-back should have cleared the cap shortfall)", err)
	}
	running := 0
	svc.store.mu.Lock()
	for _, l := range svc.store.leases {
		if l.Owner == alice.ID && l.live() {
			running++
		}
	}
	svc.store.mu.Unlock()
	if running != 7 {
		t.Fatalf("alice has %d running leases, want 7 (one taken back for bob)", running)
	}
}

// TestOvercommitInflightEndsWithEachAdmission: the in-flight figure is
// back to zero once each admission's lease is live — after grants, a
// resume and a fork — so it never leaks into the cap; and a NodeInfo
// refresh does not reset an admission still in flight.
func TestOvercommitInflightEndsWithEachAdmission(t *testing.T) {
	svc, db, sub, ids := newFairShareService(t)
	svc.cfg.MemoryOvercommit = DefaultMemoryOvercommit
	alice := addIdentityUser(t, ids, "alice")
	ctx := context.Background()
	seedImage(t, db, "one", 1024)
	roomyDisk(svc)
	fullFreeNode(t, svc, sub)
	inflight := func() int {
		svc.nodeInfoMu.Lock()
		defer svc.nodeInfoMu.Unlock()
		return svc.inflightAdmitMiB
	}
	a, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "one", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if got := inflight(); got != 0 {
		t.Fatalf("after a grant: in flight %d MiB, want 0", got)
	}
	if _, err := svc.suspend(ctx, alice.ID, a.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if _, err := svc.resume(ctx, alice.ID, a.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := inflight(); got != 0 {
		t.Fatalf("after a resume: in flight %d MiB, want 0", got)
	}
	if _, _, err := svc.fork(ctx, alice.ID, a.ID, 2, false, time.Hour, "", ""); err != nil {
		t.Fatalf("fork: %v", err)
	}
	if got := inflight(); got != 0 {
		t.Fatalf("after a fork: in flight %d MiB, want 0", got)
	}
	// An admission still in flight survives a cache refresh.
	undo, err := svc.admitMemory(ctx, alice.ID, 1024)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	svc.updateNodeMetrics(ctx)
	if got := inflight(); got != 1024 {
		t.Fatalf("after a refresh: in flight %d MiB, want 1024", got)
	}
	undo(true)
	if got := inflight(); got != 0 {
		t.Fatalf("after a failed admission's undo: in flight %d MiB, want 0", got)
	}
}
