package api

// The biggest-borrower box_full refusal honours the warm pool
// (spoond-18xe): grantLease runs admitMemory only when no pool sandbox
// served the request (spoond-41tk), so a pooled grant skipped
// biggestBorrowerRefusal and an owner far over its fair slice could
// still take a stocked pool VM. The refusal is now judged on the
// take-back view (takeBackOwnersFor) BEFORE any pool sandbox is popped,
// so a pooled grant for the biggest borrower answers 429 box_full with
// nothing taken back and nothing debited, while an in-slice owner is
// served from the pool as before.

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v3/metrics"
	"github.com/jrimmer/spoond/v3/substrate"
)

// fixedNode is one NodeInfo behind a lock: the stocked-pool fixtures
// reset the reading between the pool fill (which must see free pages
// and cap room) and the refused or served grant (which must judge the
// box as it stands), and the fake serves it fresh on every read so no
// read refreshes its way past the fixture.
type fixedNode struct {
	mu   sync.Mutex
	info substrate.NodeInfo
}

func (n *fixedNode) set(info substrate.NodeInfo) {
	n.mu.Lock()
	n.info = info
	n.mu.Unlock()
}

func (n *fixedNode) read(ctx context.Context) (substrate.NodeInfo, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.info, nil
}

// serveFixedNode points the fake's NodeInfo at node and returns the
// service whose cache must be dropped for the next read to see it (the
// caller drops it: dropNodeCache(svc)).
func serveFixedNode(sub *testSub, node *fixedNode) {
	sub.fakeFixed = node
	sub.SetNodeInfoFunc(node.read)
}

// The fixtures' arithmetic (installDynamicNode): HugepagesUsed =
// (live sandboxes) x pagesPerLease pages, the fake's page is 2 MiB, so
// an 8192 MiB box is 4096 pages, a 1024 MiB lease is 512 pages, and two
// active owners (bob's suspended lease keeps him active for fair
// shares) hold a 4096 MiB slice each.

// TestBoxFullBiggestBorrowerRefusesStockedPool: an owner holding its
// whole slice, with no other owner borrowing more, is refused 429
// box_full by a create a stocked pool COULD serve — but only where a
// cold create would be refused too (spoond-fi5v): the box's cached free
// pages sit below the request plus the floor, the shape the cold
// admission answers box_full on. The pool entry survives for the next
// request: no take-back, no debit, nothing popped.
func TestBoxFullBiggestBorrowerRefusesStockedPool(t *testing.T) {
	svc, db, sub, ids := newFairShareService(t)
	alice := addIdentityUser(t, ids, "alice")
	bob := addIdentityUser(t, ids, "bob")
	svc.SetMetrics(metrics.NewBackendMetrics())
	svc.probeEnabled = false
	ctx := context.Background()
	img := seedImage(t, db, "mid", 1024)
	roomyDisk(svc)
	// 8192 MiB box, each sandbox 1024 MiB: two 4096 MiB slices.
	installDynamicNode(t, svc, sub, 4096, 0, 512)

	// Alice exactly holds her slice: four 1024 MiB leases (4096 of
	// 4096). Bob keeps an active but suspended lease, so fair shares
	// splits the box in two — with nobody else holding hugepages, alice
	// after this file's next 1024 MiB request (5120/4096 = 1.25) is the
	// biggest borrower. The stock needs free pages and cap room: cap
	// off here. The whole test runs with the service's 2 GiB floor
	// (MEMORY_FLOOR_MIB) in place — what the backend passes when unset —
	// so the gate's free check is the check a real box runs.
	svc.cfg.MemoryFloorMiB = DefaultMemoryFloorMiB
	var al []*Lease
	for i := 0; i < 4; i++ {
		l, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "mid", ttl: time.Hour, persistent: true})
		if err != nil {
			t.Fatalf("grant alice %d: %v", i+1, err)
		}
		al = append(al, l)
	}
	var bl []*Lease
	svc.store.mu.Lock()
	for _, ll := range svc.store.leases {
		if ll.Owner == bob.ID {
			bl = append(bl, ll)
		}
	}
	svc.store.mu.Unlock()
	if len(bl) != 0 {
		t.Fatalf("setup: bob holds %d lease(s) before the first, want 0", len(bl))
	}
	if _, err := svc.grantLease(ctx, leaseRequest{owner: bob.ID, image: "mid", ttl: time.Hour, persistent: true}); err != nil {
		t.Fatalf("grant bob: %v", err)
	}
	svc.store.mu.Lock()
	for _, ll := range svc.store.leases {
		if ll.Owner == bob.ID {
			bl = append(bl, ll)
		}
	}
	svc.store.mu.Unlock()
	if len(bl) != 1 {
		t.Fatalf("setup: bob holds %d lease(s), want 1", len(bl))
	}
	if _, err := svc.suspend(ctx, bob.ID, bl[0].ID); err != nil {
		t.Fatalf("suspend bob: %v", err)
	}
	dropNodeCache(svc)
	warmFairShares(svc, ctx)

	// Stock the pool after alice is at her slice: the cap is off, so
	// the stock reads the dynamic node — 2560 pages (5120 MiB) of the
	// 8192 held, 3072 free, not over the 3072 (memory + the floor) one
	// stock needs — exactly at the stock's edge, so the entry stocks
	// and the box is left short with the VM held.
	fullFreeNode(t, svc, sub)
	installDynamicNode(t, svc, sub, 4096, 0, 512) // the pool VM brings it back to 2560 used
	svc.cfg.PoolSize = 1
	svc.warmPool(ctx, img)
	svc.store.mu.Lock()
	poolLen := len(svc.store.pool["mid"])
	svc.store.mu.Unlock()
	if poolLen != 1 {
		t.Fatalf("setup: the pool holds %d sandbox(es), want 1", poolLen)
	}

	// The grant faces the box as it stands: cache warm on the fixed
	// node (5120 MiB of the 8192 held). A short box is one a cold create
	// on it would refuse — its free 3072 sits exactly at the
	// request-plus-floor line (3072), not below it — so the fixture
	// pins Used at 3584 pages (1024 free). A cold create on that
	// reading waits on its 5120 MiB free-memory need (shortfall 4096,
	// every candidate pinned): the gate fires under the same condition
	// and the pooled refusal answers the biggest borrower.
	warmFairShares(svc, ctx)
	cachedNode(t, svc, substrate.NodeInfo{
		Status: "healthy", HugepagesTotal: 4096, HugepageSizeBytes: 2 << 20, HugepagesUsed: 3584,
	})
	refusalsBefore := counterValue(t, svc.metrics.BoxFullTotal)

	err := func() error {
		_, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "mid", ttl: time.Hour, persistent: true})
		return err
	}()
	if !isBoxFull(err) {
		t.Fatalf("the over-slice create with a stocked pool = %v, want box_full", err)
	}
	if got := counterValue(t, svc.metrics.BoxFullTotal); got != refusalsBefore+1 {
		t.Fatalf("box_full counter = %v, want one more than %v", got, refusalsBefore)
	}

	// Nothing was paused, nothing debited, and the pool entry is still
	// stocked: the refusal takes no VM it does not deserve.
	if got := cachedUsedPages(svc); got != 3584 {
		t.Fatalf("after the refused grant the cache reads %d pages used, want 3584 (no debit)", got)
	}
	svc.store.mu.Lock()
	poolLen = len(svc.store.pool["mid"])
	svc.store.mu.Unlock()
	if poolLen != 1 {
		t.Fatalf("the refused grant popped %d of the pool's sandbox(es); want the entry left stocked", 1-poolLen)
	}
	for _, l := range al {
		if l.Suspended {
			t.Fatalf("the refusal paused lease %s (a box_full never takes back)", l.ID)
		}
	}
	// Alice still holds five running leases (her own four plus the one
	// bob's pool VM became; the refusal took no take-back), and the
	// pinned reading stays at 3584 pages used: nothing debited it.
	if got := cachedUsedPages(svc); got != 3584 {
		t.Fatalf("after the refusal the cache reads %d pages used, want 3584 (no debit)", got)
	}
	if n := len(sub.sandboxesLive(t)); n != 5 {
		t.Fatalf("%d sandboxes live after the refusal, want 5 (alice's four leases + the pool VM)", n)
	}

	// Bob is inside his slice: the same stocked pool serves him, as
	// before the fix.
	served, err := svc.grantLease(ctx, leaseRequest{owner: bob.ID, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("bob's create on the same stocked pool: %v", err)
	}
	if !served.pooled {
		t.Fatal("bob's in-slice grant did not come from the pool")
	}
	svc.store.mu.Lock()
	poolLen = len(svc.store.pool["mid"])
	svc.store.mu.Unlock()
	if poolLen != 0 {
		t.Fatalf("bob's grant left %d pool sandbox(es), want 0 (served from the pool)", poolLen)
	}
}

// TestBoxFullPooledGrantServesInSliceOwner: an owner inside its slice
// takes a stocked pool VM with no debit and no take-back — the pooled
// grant's own shape (spoond-41tk's no-debit admission), now that the
// biggest-borrower refusal stands in front of the pop. The box is full
// enough that a cold create would have to take back or wait: only the
// pool seam serves it without touching anybody.
func TestBoxFullPooledGrantServesInSliceOwner(t *testing.T) {
	svc, db, sub, ids := newFairShareService(t)
	svc.SetMetrics(metrics.NewBackendMetrics())
	alice := addIdentityUser(t, ids, "alice")
	bob := addIdentityUser(t, ids, "bob")
	svc.probeEnabled = false
	ctx := context.Background()
	img := seedImage(t, db, "mid", 1024)
	roomyDisk(svc)
	// 8192 MiB box, each sandbox 1024 MiB: two 4096 MiB slices.
	installDynamicNode(t, svc, sub, 4096, 0, 512)

	// Alice holds one 1024 MiB lease (0.25 of her slice, inside);
	// bob holds two (0.5, inside). The stock sees free 5120 MiB over
	// the 3072 (memory + the 2 GiB floor) one stock needs.
	if _, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "mid", ttl: time.Hour, persistent: true}); err != nil {
		t.Fatalf("grant alice: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := svc.grantLease(ctx, leaseRequest{owner: bob.ID, image: "mid", ttl: time.Hour, persistent: true}); err != nil {
			t.Fatalf("grant bob %d: %v", i+1, err)
		}
	}
	dropNodeCache(svc)
	warmFairShares(svc, ctx)
	node := &fixedNode{}
	serveFixedNode(sub, node)
	node.set(substrate.NodeInfo{Status: "healthy", HugepagesTotal: 4096, HugepageSizeBytes: 2 << 20, HugepagesUsed: 768})
	dropNodeCache(svc)
	svc.cfg.PoolSize = 1
	svc.warmPool(ctx, img)
	svc.store.mu.Lock()
	poolLen := len(svc.store.pool["mid"])
	svc.store.mu.Unlock()
	if poolLen != 1 {
		t.Fatalf("setup: the pool holds %d sandbox(es), want 1", poolLen)
	}

	// Alice's next lease puts her at 2048 of 4096 (0.5) — still inside
	// her slice after the request — so the pre-pop refusal must not
	// fire and the pool must serve. The cache sits on the box as it
	// stands (2048 MiB of the 8192 held, 6144 free), so a grant that
	// wrongly ran the admission first would debit 1024 MiB off it.
	warmFairShares(svc, ctx)
	cachedNode(t, svc, substrate.NodeInfo{
		Status: "healthy", HugepagesTotal: 4096, HugepageSizeBytes: 2 << 20, HugepagesUsed: 2048,
	})

	granted, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("the in-slice create on a stocked pool = %v, want served from the pool", err)
	}
	if !granted.pooled {
		t.Fatal("the in-slice grant did not come from the pool")
	}
	if got := cachedUsedPages(svc); got != 2048 {
		t.Fatalf("after the pooled grant the cache reads %d pages used, want 2048 (no debit, no take-back)", got)
	}
	svc.store.mu.Lock()
	poolLen = len(svc.store.pool["mid"])
	svc.store.mu.Unlock()
	if poolLen != 0 {
		t.Fatalf("the pooled grant left %d pool sandbox(es), want 0", poolLen)
	}
	for _, live := range svc.store.leases {
		if live.Suspended {
			t.Fatalf("the pooled grant paused lease %s", live.ID)
		}
	}
}

// TestBoxFullStockedPoolHTTP429OwnLeases: the pool-honouring refusal is
// the ordinary biggest-borrower one over HTTP: 429 box_full with
// own_leases (the requester's own running leases, LRU first) and no
// Retry-After — a box_full is never waitable.
func TestBoxFullStockedPoolHTTP429OwnLeases(t *testing.T) {
	svc, db, sub, ids := newFairShareService(t)
	alice := addIdentityUser(t, ids, "alice")
	bob := addIdentityUser(t, ids, "bob")
	// The fair-share service clears the legacy tokens; map the test
	// tokens straight onto the identity users, so doReq authenticates as
	// them without changing the owner set (the ids already exist).
	svc.tokens = map[string]string{"token-a": alice.ID, "token-b": bob.ID}
	svc.SetMetrics(metrics.NewBackendMetrics())
	svc.probeEnabled = false
	ctx := context.Background()
	img := seedImage(t, db, "mid", 1024)
	roomyDisk(svc)
	// 8192 MiB box, each sandbox 1024 MiB: two 4096 MiB slices.
	installDynamicNode(t, svc, sub, 4096, 0, 512)

	// Alice exactly holds her slice: four 1024 MiB leases (4096 of
	// 4096). Bob starts with a lease that is suspended below, so fair
	// shares splits the box in two while he holds no hugepages. The
	// whole test runs with the service's 2 GiB floor (MEMORY_FLOOR_MIB)
	// in place — what the backend passes when unset — so the gate's
	// free check is the check a real box runs.
	svc.cfg.MemoryFloorMiB = DefaultMemoryFloorMiB
	var al []*Lease
	for i := 0; i < 4; i++ {
		l, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "mid", ttl: time.Hour, persistent: true})
		if err != nil {
			t.Fatalf("grant alice %d: %v", i+1, err)
		}
		al = append(al, l)
	}
	if _, err := svc.grantLease(ctx, leaseRequest{owner: bob.ID, image: "mid", ttl: time.Hour, persistent: true}); err != nil {
		t.Fatalf("grant bob: %v", err)
	}
	// LRU-ordered so the wire order is pinned: al[3] (oldest) first.
	base := time.Now().Add(-time.Hour)
	svc.store.mu.Lock()
	for i, l := range al {
		l.LastActive = base.Add(time.Duration(4-i) * time.Minute)
		svc.saveLeaseLocked(l)
	}
	svc.store.mu.Unlock()
	var bl []*Lease
	svc.store.mu.Lock()
	for _, ll := range svc.store.leases {
		if ll.Owner == bob.ID {
			bl = append(bl, ll)
		}
	}
	svc.store.mu.Unlock()
	if _, err := svc.suspend(ctx, bob.ID, bl[0].ID); err != nil {
		t.Fatalf("suspend bob: %v", err)
	}
	dropNodeCache(svc)
	warmFairShares(svc, ctx)
	node := &fixedNode{}
	serveFixedNode(sub, node)
	node.set(substrate.NodeInfo{Status: "healthy", HugepagesTotal: 4096, HugepageSizeBytes: 2 << 20, HugepagesUsed: 1024})
	dropNodeCache(svc)
	svc.cfg.PoolSize = 1
	svc.warmPool(ctx, img)
	svc.store.mu.Lock()
	poolLen := len(svc.store.pool["mid"])
	svc.store.mu.Unlock()
	if poolLen != 1 {
		t.Fatalf("setup: the pool holds %d sandbox(es), want 1", poolLen)
	}
	node.set(substrate.NodeInfo{Status: "healthy", HugepagesTotal: 4096, HugepageSizeBytes: 2 << 20, HugepagesUsed: 3584})
	warmFairShares(svc, ctx)
	// Alice holds her whole slice; the fixed fixture holds the
	// admission reading at 7168 MiB used (1024 free). A cold create on
	// that reading waits on its 5120 MiB need (4096 short, every
	// candidate pinned), so the gate fires exactly where a cold create
	// would refuse, and a grant that wrongly skipped the gate would
	// still find its 1024 free pages and serve: the pooled refusal is
	// the one answer.
	cachedNode(t, svc, substrate.NodeInfo{
		Status: "healthy", HugepagesTotal: 4096, HugepageSizeBytes: 2 << 20, HugepagesUsed: 3584,
	})

	ts := httptest.NewServer(NewServer(svc, NewImageRegistry(db)).Handler())
	t.Cleanup(ts.Close)
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "mid", "ttl": 3600})
	if resp.StatusCode != 429 {
		t.Fatalf("the over-slice create with a stocked pool = %d (%v), want 429", resp.StatusCode, body)
	}
	if body["code"] != "box_full" {
		t.Fatalf("code = %v, want box_full", body["code"])
	}
	leases := ownLeasesOf(t, body)
	if len(leases) != 4 {
		t.Fatalf("own_leases has %d rows, want alice's four running leases: %v", len(leases), body["own_leases"])
	}
	for i, l := range [4]*Lease{al[3], al[2], al[1], al[0]} {
		if got, _ := leases[i]["id"].(string); got != l.ID {
			t.Fatalf("own_leases[%d].id = %s, want %s (LRU first)", i, got, l.ID)
		}
	}
	if got := resp.Header.Get("Retry-After"); got != "" {
		t.Fatalf("the refusal carries Retry-After %q; a box_full is never waitable", got)
	}
	// Nothing was paused by the refusal: the listing is the only effect.
	for _, l := range al {
		if l.Suspended {
			t.Fatalf("lease %s was paused by a box_full listing", l.ID)
		}
	}
}

// TestBoxFullStockedPoolServesWhenMemoryCovers (spoond-fi5v): the same
// over-slice owner is SERVED from the pool when the box's cached free
// pages cover the request plus the floor. The takeover commits no new
// hugepages, so the gate stays shut: no box_full, no take-back, no debit,
// and the pop hands the VM over — the refusal never hides behind
// poolCanServe on a box with room.
func TestBoxFullStockedPoolServesWhenMemoryCovers(t *testing.T) {
	svc, db, sub, ids := newFairShareService(t)
	alice := addIdentityUser(t, ids, "alice")
	bob := addIdentityUser(t, ids, "bob")
	svc.SetMetrics(metrics.NewBackendMetrics())
	svc.probeEnabled = false
	ctx := context.Background()
	img := seedImage(t, db, "mid", 1024)
	roomyDisk(svc)
	// 8192 MiB box, each sandbox 1024 MiB: two 4096 MiB slices.
	installDynamicNode(t, svc, sub, 4096, 0, 512)

	// Alice exactly holds her slice (four 1024 MiB leases); bob keeps an
	// active but suspended lease so the fair view splits the box — alice
	// would be the biggest borrower on any cold admission. The whole
	// test runs with the service's 2 GiB floor (MEMORY_FLOOR_MIB) in
	// place, like its refused siblings — so what differs is the box's
	// room, not the floor.
	svc.cfg.MemoryFloorMiB = DefaultMemoryFloorMiB
	var al []*Lease
	for i := 0; i < 4; i++ {
		l, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "mid", ttl: time.Hour, persistent: true})
		if err != nil {
			t.Fatalf("grant alice %d: %v", i+1, err)
		}
		al = append(al, l)
	}
	if _, err := svc.grantLease(ctx, leaseRequest{owner: bob.ID, image: "mid", ttl: time.Hour, persistent: true}); err != nil {
		t.Fatalf("grant bob: %v", err)
	}
	svc.store.mu.Lock()
	var bl *Lease
	for _, ll := range svc.store.leases {
		if ll.Owner == bob.ID {
			bl = ll
		}
	}
	svc.store.mu.Unlock()
	if _, err := svc.suspend(ctx, bob.ID, bl.ID); err != nil {
		t.Fatalf("suspend bob: %v", err)
	}
	dropNodeCache(svc)
	warmFairShares(svc, ctx)

	// Stock the pool with the box's pages back: the stock reads the
	// dynamic node (four of alice's leases hold 2048 of the 4096
	// pages, bob's suspended lease holds none), 6144 free over the
	// 3072 (memory + the floor) one stock needs, and leaves the box at
	// 2560 used.
	fullFreeNode(t, svc, sub)
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	svc.cfg.PoolSize = 1
	svc.warmPool(ctx, img)
	svc.store.mu.Lock()
	poolLen := len(svc.store.pool["mid"])
	svc.store.mu.Unlock()
	if poolLen != 1 {
		t.Fatalf("setup: the pool holds %d sandbox(es), want 1", poolLen)
	}
	// Pin the reading the grant's gate judges LAST, after the fair
	// refresh: 5 live sandboxes (four of alice's leases plus the pool
	// VM) hold 2560 of the 4096 pages in the dynamic fixture — 3072
	// free, one MiB below the gate's need (3073) — so the pinned read
	// gives the gate room: 2048 pages used, 4096 free, over the 3072 a
	// cold create needs with the floor. A box that short is served —
	// the refusal must only fire where a cold create would refuse too.
	warmFairShares(svc, ctx)
	cachedNode(t, svc, substrate.NodeInfo{
		Status: "healthy", HugepagesTotal: 4096, HugepageSizeBytes: 2 << 20, HugepagesUsed: 2048,
	})

	granted, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("the over-slice create on a box with room = %v, want served from the pool", err)
	}
	if !granted.pooled {
		t.Fatal("the over-slice grant on a box with room did not come from the pool")
	}
	if got := cachedUsedPages(svc); got != 2048 {
		t.Fatalf("after the pooled grant the cache reads %d pages used, want 2048 (no debit)", got)
	}
	svc.store.mu.Lock()
	poolLen = len(svc.store.pool["mid"])
	svc.store.mu.Unlock()
	if poolLen != 0 {
		t.Fatalf("the grant left %d pool sandbox(es), want 0 (served from the pool)", poolLen)
	}
	for _, l := range al {
		if l.Suspended {
			t.Fatalf("the served grant paused lease %s (the pool takeover takes nobody back)", l.ID)
		}
	}
}
