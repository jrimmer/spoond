package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/identity"
	"github.com/jrimmer/spoond/v2/metrics"
	"github.com/jrimmer/spoond/v2/substrate"
)

// Preemption tests (#128 part 3, updated by FS2a spoond-pxsn). The fake
// substrate's NodeInfo is made controllable by a function that counts the
// fake's live sandboxes, so a pause really frees hugepages: the same shape
// the orchestrator reports in production. Since FS2a the victim choice is
// the fair-shares take-back selector, so a burst owner must be a box owner
// (identity user or token owner) with memory over their slice to be
// touchable: preemptOwner below grants one.

// dynNode is a controllable NodeInfo: baseUsed pages are held outside
// leases, and every live sandbox on the fake counts pagesPerLease.
type dynNode struct {
	totalPages    uint64
	baseUsed      uint64
	pagesPerLease uint64
}

// installDynamicNode points the fake's NodeInfo at the running sandbox
// count and drops the service's cache so the next read sees it.
func installDynamicNode(t *testing.T, svc *Service, sub *testSub, totalPages, baseUsed, pagesPerLease uint64) {
	t.Helper()
	sub.SetNodeInfoFunc(func(ctx context.Context) (substrate.NodeInfo, error) {
		sbs, err := sub.List(ctx)
		if err != nil {
			return substrate.NodeInfo{}, err
		}
		used := baseUsed + uint64(len(sbs))*pagesPerLease
		return substrate.NodeInfo{
			Status:            "healthy",
			HugepagesTotal:    totalPages,
			HugepagesUsed:     used,
			HugepageSizeBytes: 2 << 20,
		}, nil
	})
	dropNodeCache(svc)
}

// dropNodeCache forgets the cached NodeInfo so the next admission reads
// the fake again.
func dropNodeCache(svc *Service) {
	svc.nodeInfoMu.Lock()
	svc.nodeInfoAt = time.Time{}
	svc.nodeInfoMu.Unlock()
}

// preemptOwner grants owner an identity user and returns its id, so the
// fair-shares take-back selector sees the lease (a lease owner that is
// neither an identity user nor a token owner is not a box owner and can
// never be over its slice).
func preemptOwner(t *testing.T, svc *Service, name string) string {
	t.Helper()
	if svc.identities == nil {
		ids, err := identity.NewStore("")
		if err != nil {
			t.Fatal(err)
		}
		svc.SetIdentities(ids)
	}
	u, err := svc.identities.AddUser(name, identity.KindPerson, nil, "")
	if err != nil {
		t.Fatalf("add user %s: %v", name, err)
	}
	return u.ID
}

// warmFairShares refreshes the NodeInfo cache and invalidates the fair
// snapshot, so a take-back choice made now reads the current usage. (In
// production the node-metrics loop keeps both warm; grantLease already
// invalidates the snapshot.)
func warmFairShares(svc *Service, ctx context.Context) {
	svc.updateNodeMetrics(ctx)
	svc.invalidateFairShares()
}

// newPreemptService builds a service over a fake and an image "mid" of
// 1024 MiB, with the burst reserve off (so the preemption math is the
// only thing under test) and no disk floor unless a test sets one.
func newPreemptService(t *testing.T) (*Service, *testSub, context.Context) {
	t.Helper()
	svc, db, sub := newTestService(t)
	seedImage(t, db, "mid", 1024)
	seedImage(t, db, "big", 2048)
	svc.cfg.BurstReserveMiB = 0
	svc.SetMetrics(metrics.NewBackendMetrics())
	// Huge initially, so burst leases admit without reserve pressure.
	sub.SetNodeInfo(substrate.NodeInfo{
		Status:            "healthy",
		HugepagesTotal:    1 << 20,
		HugepageSizeBytes: 2 << 20,
	}, nil)
	// FS2a: the victim choice reads the fair-share snapshot, whose memory
	// slice exists only when every capacity read works. The snapshot disk
	// read is stubbed roomy (the tests below that exercise the disk floor
	// override this) and the storage root exists, so take-back sees real
	// slices and statfs stays off the temp dir.
	svc.cfg.TemplateStoragePath = t.TempDir()
	var diskTotal uint64 = 100 << 30
	svc.diskCapacity = func(string) (uint64, uint64, error) { return diskTotal, diskTotal, nil }
	// Pause builds measure at their memory_mb, so a taken-back lease
	// moves its owner's disk usage like a real pause would.
	svc.diskUsage = func(dir string) (int64, error) { return 1024 << 20, nil }
	return svc, sub, context.Background()
}

// newPreemptServer is newPreemptService plus an HTTP server and its DB,
// for the resume-on-use tests that go through the API.
func newPreemptServer(t *testing.T) (*httptest.Server, *Service, *testSub, context.Context) {
	t.Helper()
	svc, db, sub := newTestService(t)
	seedImage(t, db, "mid", 1024)
	seedImage(t, db, "big", 2048)
	svc.cfg.BurstReserveMiB = 0
	svc.SetMetrics(metrics.NewBackendMetrics())
	sub.SetNodeInfo(substrate.NodeInfo{
		Status:            "healthy",
		HugepagesTotal:    1 << 20,
		HugepageSizeBytes: 2 << 20,
	}, nil)
	// FS2a: same fair-share groundwork as newPreemptService — a known
	// snapshot disk and per-pause build sizes — so the take-back selector
	// sees real slices.
	svc.cfg.TemplateStoragePath = t.TempDir()
	var diskTotal uint64 = 100 << 30
	svc.diskCapacity = func(string) (uint64, uint64, error) { return diskTotal, diskTotal, nil }
	svc.diskUsage = func(dir string) (int64, error) { return 1024 << 20, nil }
	srv := NewServer(svc, NewImageRegistry(db))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, svc, sub, context.Background()
}

// burstLease creates a burst lease owned by owner over the given image
// and returns it.
func burstLease(t *testing.T, svc *Service, ctx context.Context, owner, image string) *Lease {
	t.Helper()
	l, err := svc.grantLease(ctx, leaseRequest{owner: owner, image: image, ttl: time.Hour, persistent: true, burst: true})
	if err != nil {
		t.Fatalf("grant burst lease: %v", err)
	}
	return l
}

// preemptedIDs returns the ids of the leases currently marked preempted.
func preemptedIDs(svc *Service) map[string]bool {
	svc.store.mu.Lock()
	defer svc.store.mu.Unlock()
	out := map[string]bool{}
	for id, l := range svc.store.leases {
		if !l.PreemptedAt.IsZero() {
			out[id] = true
		}
	}
	return out
}

// pausedIDs returns the ids of the leases currently suspended (the
// take-back view of "preempted": the stamps differ, the pause is the
// same).
func pausedIDs(svc *Service) map[string]bool {
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

// TestTakeBackOrderOwnerRatioThenLRU: take-back takes the owner
// furthest over their slice first (ratio, not absolute usage), and
// within an owner the least recently used lease (FS2a spoond-pxsn;
// priority and newest-first were the pre-FS2a preemption order). Two
// owners are equally over, so the tie is broken by the selector's
// stable owner order; each gives up its least recently used lease and
// the request is satisfied.
func TestTakeBackOrderOwnerRatioThenLRU(t *testing.T) {
	svc, sub, ctx := newPreemptService(t)
	// Room for the burst leases; the guaranteed lease below needs two
	// leases' worth of room, one from each over-slice owner.
	installDynamicNode(t, svc, sub, 4096, 0, 512)

	overA := preemptOwner(t, svc, "aaa") // sorts before zzz: tied ratios go to it first
	overB := preemptOwner(t, svc, "zzz")
	old := burstLease(t, svc, ctx, overA, "mid")
	newest := burstLease(t, svc, ctx, overA, "mid")
	fillA := burstLease(t, svc, ctx, overA, "mid")
	fillA2 := burstLease(t, svc, ctx, overA, "mid")
	bOld := burstLease(t, svc, ctx, overB, "mid")
	bNewest := burstLease(t, svc, ctx, overB, "mid")
	fillB := burstLease(t, svc, ctx, overB, "mid")
	fillB2 := burstLease(t, svc, ctx, overB, "mid")

	// LRU order per owner: the oldest lease of each owner is the one to
	// go; the "newest" ones must be left running.
	base := time.Now().Add(-5 * time.Hour)
	svc.store.mu.Lock()
	for i, l := range []*Lease{old, newest, fillA, fillA2, bOld, bNewest, fillB, fillB2} {
		l.LastActive = base.Add(time.Duration(i%4) * time.Hour)
		svc.saveLeaseLocked(l)
	}
	svc.store.mu.Unlock()

	// Both owners run 4096 MiB — eight leases fill the 8192 MiB pool
	// exactly (slice 1638 MiB over five box owners, ratio 2.5 each, far
	// over) — and the two LRU leases together free the 2048 MiB the big
	// guaranteed lease needs. (One owner alone could not give both: the
	// second give-up would drop it to the requester's own post-request
	// ratio, which the selector never does.)
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	warmFairShares(svc, ctx)
	guaranteed, err := svc.grantLease(ctx, leaseRequest{owner: "guaranteed", image: "big", ttl: time.Hour})
	if err != nil {
		t.Fatalf("guaranteed create: %v", err)
	}
	if guaranteed.Class != ClassGuaranteed {
		t.Fatalf("guaranteed create class = %s", guaranteed.Class)
	}

	got := pausedIDs(svc)
	if len(got) != 2 {
		t.Fatalf("paused %d leases, want exactly 2 (%v)", len(got), got)
	}
	if !got[old.ID] || !got[bOld.ID] {
		t.Fatalf("the least recently used lease of each owner was not paused (paused: %v; want %s and %s)", got, old.ID, bOld.ID)
	}
	for _, keep := range []*Lease{newest, fillA, fillA2, bNewest, fillB, fillB2} {
		if got[keep.ID] {
			t.Fatalf("lease %s was paused though older leases of its owner freed enough", keep.ID)
		}
	}
	// No preemption stamps: take-back suppresses auto-resume (#145 D2);
	// the holder's next work call resumes the lease.
	for _, l := range []*Lease{old, bOld} {
		if !l.PreemptedAt.IsZero() {
			t.Fatalf("take-back stamped preempted_at on %s", l.ID)
		}
	}
}

// TestPreemptionFreesEnoughAndNoMore: take-back stops as soon as the
// guaranteed lease fits, leaving the other burst leases running.
func TestPreemptionFreesEnoughAndNoMore(t *testing.T) {
	svc, sub, ctx := newPreemptService(t)
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	owner := preemptOwner(t, svc, "heavy")
	a := burstLease(t, svc, ctx, owner, "mid")
	b := burstLease(t, svc, ctx, owner, "mid")
	c := burstLease(t, svc, ctx, owner, "mid")

	// LRU order a → b → c.
	svc.store.mu.Lock()
	base := time.Now().Add(-3 * time.Hour)
	a.LastActive = base
	b.LastActive = base.Add(time.Hour)
	c.LastActive = base.Add(2 * time.Hour)
	svc.saveLeaseLocked(a)
	svc.saveLeaseLocked(b)
	svc.saveLeaseLocked(c)
	svc.store.mu.Unlock()

	// The owner runs 3072 MiB on a box with three other owners and a
	// 3072 MiB pool (slice 768 MiB each): ratio 4, far over, and the
	// three leases fill it exactly. One 1024 MiB guaranteed lease needs
	// one lease paused; the selector takes the least recently used and
	// stops (giving more would push the owner toward the requester, and
	// the room is already there).
	installDynamicNode(t, svc, sub, 1536, 0, 512)
	warmFairShares(svc, ctx)
	if _, err := svc.grantLease(ctx, leaseRequest{owner: "guaranteed", image: "mid", ttl: time.Hour}); err != nil {
		t.Fatalf("guaranteed create: %v", err)
	}
	got := pausedIDs(svc)
	if len(got) != 1 {
		t.Fatalf("paused %d leases, want exactly 1 (%v)", len(got), got)
	}
	if !got[a.ID] {
		t.Fatalf("least-recently-used lease %s was not the one paused (paused: %v)", a.ID, got)
	}
	svc.store.mu.Lock()
	running := 0
	for _, l := range []*Lease{a, b, c} {
		if !l.Suspended {
			running++
		}
	}
	svc.store.mu.Unlock()
	if running != 2 {
		t.Fatalf("running burst leases = %d, want 2", running)
	}
}

// TestPreemptionDiskFloorRefuses: when pausing a candidate would take
// the snapshot disk under the floor, the guaranteed admission answers
// 503 "capacity: cannot preempt (snapshot disk low)" with Retry-After,
// and nothing is preempted.
func TestPreemptionDiskFloorRefuses(t *testing.T) {
	svc, sub, ctx := newPreemptService(t)
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	l := burstLease(t, svc, ctx, "burst-a", "mid")

	// The disk is at 10% free and the pause is estimated at 1024 MiB of
	// a 100 GiB disk: far under the 15% floor.
	svc.cfg.TemplateStoragePath = t.TempDir()
	svc.cfg.PreemptDiskFloorPct = 15
	var total uint64 = 100 << 30
	svc.diskCapacity = func(string) (uint64, uint64, error) {
		return total, total / 10, nil
	}
	installDynamicNode(t, svc, sub, 2048, 2048-512, 512)

	_, err := svc.grantLease(ctx, leaseRequest{owner: "guaranteed", image: "mid", ttl: time.Hour})
	if !errors.Is(err, errPreemptCannot) {
		t.Fatalf("guaranteed create err = %v, want errPreemptCannot", err)
	}
	if !strings.Contains(err.Error(), "cannot preempt (snapshot disk low)") {
		t.Fatalf("error %q should name the disk floor", err.Error())
	}
	if l.Suspended || !l.PreemptedAt.IsZero() {
		t.Fatal("disk-floor refusal must not preempt the burst lease")
	}
}

// TestPreemptionDiskFloorRefusedHTTP maps the floor refusal to the
// documented 503: "capacity: cannot preempt (snapshot disk low)" with
// Retry-After: 30.
func TestPreemptionDiskFloorRefusedHTTP(t *testing.T) {
	srv, h, sub, tok, _ := newClassServer(t, map[string]int{"mid": 1024}, `{"max_mib":8192}`)
	svc := srv.svc
	svc.cfg.BurstReserveMiB = 0
	svc.cfg.PreemptDiskFloorPct = 15
	svc.cfg.TemplateStoragePath = t.TempDir()
	var total uint64 = 100 << 30
	svc.diskCapacity = func(string) (uint64, uint64, error) {
		return total, total / 10, nil
	}
	// A burst lease on the roomy node, then the node fills.
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	code, body, _ := createBodyResp(t, h, tok, `{"image":"mid","ttl":60,"burst":true}`)
	if code != http.StatusCreated {
		t.Fatalf("burst create: %d %s", code, body)
	}
	installDynamicNode(t, svc, sub, 2048, 2048-512, 512)

	code, body, hdr := createBodyResp(t, h, tok, `{"image":"mid","ttl":60}`)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("guaranteed create = %d %s, want 503", code, body)
	}
	if !strings.Contains(body, "capacity: cannot preempt (snapshot disk low)") {
		t.Fatalf("503 body = %s, want the preemption refusal", body)
	}
	if ra := hdr.Get("Retry-After"); ra != "30" {
		t.Fatalf("Retry-After = %q, want 30", ra)
	}
}

// TestPreemptionResumeOnUse: a taken-back lease comes back on its
// holder's next work call instead of on a background queue (#145 D2,
// FS2a spoond-pxsn). The exec resumes it through the normal path,
// clearing the take-back stamp and emitting "resumed" with detail
// "after take-back", and the generation does not change (the
// pause/resume continues the memory).
func TestPreemptionResumeOnUse(t *testing.T) {
	ts, svc, sub, ctx := newPreemptServer(t)
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	l1 := burstLease(t, svc, ctx, "consumer-a", "mid") // exec'd via token-a below
	burstLease(t, svc, ctx, "consumer-a", "mid")
	l3 := burstLease(t, svc, ctx, "consumer-a", "mid")
	svc.store.mu.Lock()
	l1.LastActive = time.Now().Add(-time.Hour)
	l3.LastActive = time.Now()
	svc.saveLeaseLocked(l1)
	svc.saveLeaseLocked(l3)
	svc.store.mu.Unlock()

	// consumer-a runs three leases (3072 MiB) and fills the 3072 MiB
	// pool exactly (three box owners, slice 1024 MiB each): ratio 3. A
	// 1024 MiB guaranteed lease takes the owner's least recently used
	// lease — the holder execs it through token-a below — while the
	// owner stays above the requester's post-request ratio.
	installDynamicNode(t, svc, sub, 1536, 0, 512)
	warmFairShares(svc, ctx)
	if _, err := svc.grantLease(ctx, leaseRequest{owner: "guaranteed", image: "mid", ttl: time.Hour}); err != nil {
		t.Fatalf("guaranteed create: %v", err)
	}
	if !l1.Suspended || l3.Suspended {
		t.Fatalf("take-back paused the wrong lease: l1=%v l3=%v", l1.Suspended, l3.Suspended)
	}
	victim := l1
	if victim.SuspendReason != suspendReasonTakeBack || victim.TakeBackFor != "guaranteed" {
		t.Fatalf("victim stamps: reason=%q for=%q, want take_back/guaranteed", victim.SuspendReason, victim.TakeBackFor)
	}
	if victim.Generation != 1 {
		t.Fatalf("take-back changed the generation to %d, want 1", victim.Generation)
	}

	// Watch the resumed event.
	subEvents := svc.Subscribe(EventFilter{LeaseID: victim.ID})
	defer subEvents.Close()

	// Capacity returns: the holder's next work call resumes it.
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+victim.ID+"/exec", "token-a",
		map[string]any{"cmd": "echo hi"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exec on a taken-back lease: %d: %v", resp.StatusCode, body)
	}
	if victim.Suspended || victim.TakeBackFor != "" {
		t.Fatalf("victim after resume: suspended=%v takeBackFor=%q", victim.Suspended, victim.TakeBackFor)
	}
	if victim.Generation != 1 {
		t.Fatalf("resume changed the generation to %d, want 1", victim.Generation)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-subEvents.C:
			if ev.Type == LeaseResumed {
				if ev.Detail != "after take-back" {
					t.Fatalf("resumed detail = %q, want %q", ev.Detail, "after take-back")
				}
				return
			}
		case <-deadline:
			t.Fatal("no resumed event for the taken-back lease")
		}
	}
}

// TestPreemptionSerialised: two concurrent guaranteed creates take back
// once between them, because one pause frees enough for both and
// preemptMu makes the second see the first's freed memory.
func TestPreemptionSerialised(t *testing.T) {
	svc, sub, ctx := newPreemptService(t)
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	owner := preemptOwner(t, svc, "heavy")
	burstLease(t, svc, ctx, owner, "mid")
	burstLease(t, svc, ctx, owner, "mid")
	burstLease(t, svc, ctx, owner, "mid")
	burstLease(t, svc, ctx, owner, "mid")

	// Five box owners on a 5120 MiB pool; the owner runs four leases
	// (4096 MiB, ratio 3.2) and the free 1024 MiB admits the first
	// guaranteed lease. The second finds no free memory and takes the
	// owner's oldest lease back — which leaves the owner above
	// consumer-a's post-request ratio — while preemptMu makes it see
	// the first create's debit: between the two creates exactly one
	// lease is paused.
	installDynamicNode(t, svc, sub, 2560, 0, 512)
	warmFairShares(svc, ctx)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.grantLease(ctx, leaseRequest{owner: "consumer-a", image: "mid", ttl: time.Hour})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("guaranteed create %d: %v", i, err)
		}
	}
	svc.store.mu.Lock()
	paused, victim := 0, ""
	for _, l := range svc.store.leases {
		if l.Suspended {
			paused++
			victim = l.Owner
		}
	}
	svc.store.mu.Unlock()
	if paused != 1 {
		t.Fatalf("concurrent creates paused %d leases, want exactly 1 (serialised)", paused)
	}
	if victim != owner {
		t.Fatalf("the paused lease belongs to %q, want the over-slice owner", victim)
	}
}

// TestPreemptionAPIReportsTakenBack: a taken-back lease reports
// "suspended" with reason take_back, carries no generation bump, and
// emits the take_back event naming the requesting owner and the
// over-slice ratio (the old preempted reporting went with the
// pre-FS2a victim choice).
func TestPreemptionAPIReportsTakenBack(t *testing.T) {
	svc, sub, ctx := newPreemptService(t)
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	victim := burstLease(t, svc, ctx, "consumer-a", "mid")
	filler := burstLease(t, svc, ctx, "consumer-a", "mid")
	filler2 := burstLease(t, svc, ctx, "consumer-a", "mid")

	events := svc.Subscribe(EventFilter{LeaseID: victim.ID})
	defer events.Close()

	// consumer-a fills the 3072 MiB pool exactly (three box owners,
	// slice 1024): ratio 3. The guaranteed lease takes the LRU lease.
	installDynamicNode(t, svc, sub, 1536, 0, 512)
	warmFairShares(svc, ctx)
	if _, err := svc.grantLease(ctx, leaseRequest{owner: "guaranteed", image: "mid", ttl: time.Hour}); err != nil {
		t.Fatalf("guaranteed create: %v", err)
	}
	if !victim.Suspended || filler.Suspended || filler2.Suspended {
		t.Fatalf("take-back paused the wrong lease: v=%v f=%v f2=%v", victim.Suspended, filler.Suspended, filler2.Suspended)
	}

	m := leaseMap(victim, svc.effectiveCheckpointInterval(victim), svc.effectiveIdleSuspend(victim))
	if got := m["preempted"]; got != false {
		t.Fatalf("preempted field = %v, want false (take-back does not stamp preempted)", got)
	}
	if victim.SuspendReason != suspendReasonTakeBack {
		t.Fatalf("suspend reason = %q, want take_back", victim.SuspendReason)
	}
	if victim.Generation != 1 {
		t.Fatalf("take-back bumped the generation to %d, want 1", victim.Generation)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events.C:
			if ev.Type == LeaseTakeBack {
				if !strings.Contains(ev.Detail, "for guaranteed") || !strings.Contains(ev.Detail, "3.00") {
					t.Fatalf("take-back detail = %q, want the requester and the 3.00 ratio", ev.Detail)
				}
				return
			}
		case <-deadline:
			t.Fatal("no take-back event")
		}
	}
}

// TestPreemptionCandidateOrderOverGuarantee: preemptionCandidates (the
// FS2b gate that will restrict take-back to burst leases) still orders
// its candidates by the pre-FS2a preemption rule: with equal priority
// and age, the owner furthest over its guarantee is preempted first.
// FS2b removes this function with the classes.
func TestPreemptionCandidateOrderOverGuarantee(t *testing.T) {
	svc, sub, ctx := newPreemptService(t)
	installDynamicNode(t, svc, sub, 4096, 0, 512)

	ids, _ := identity.NewStore("")
	svc.SetIdentities(ids)
	// Two users, same guarantee; each with a lease.
	under, err := ids.AddUser("under", identity.KindPerson, []string{"SHA256:u1"}, "tok-u1")
	if err != nil {
		t.Fatalf("add under: %v", err)
	}
	over, err := ids.AddUser("over", identity.KindPerson, []string{"SHA256:u2"}, "tok-u2")
	if err != nil {
		t.Fatalf("add over: %v", err)
	}
	if err := ids.SetQuota(under.ID, 0, 0, 4096, 0, 0); err != nil {
		t.Fatalf("quota under: %v", err)
	}
	// The over user's guarantee is 512 MiB while it runs a 1024 MiB
	// lease, so it sits 512 MiB over; the under user is within its own.
	if err := ids.SetQuota(over.ID, 0, 0, 512, 0, 0); err != nil {
		t.Fatalf("quota over: %v", err)
	}

	l1 := burstLease(t, svc, ctx, under.ID, "mid")
	l2 := burstLease(t, svc, ctx, over.ID, "mid")
	same := time.Now().Add(-time.Hour)
	svc.store.mu.Lock()
	l1.CreatedAt, l2.CreatedAt = same, same
	l1.Priority, l2.Priority = 0, 0
	svc.saveLeaseLocked(l1)
	svc.saveLeaseLocked(l2)
	svc.store.mu.Unlock()

	cands := svc.preemptionCandidates()
	if len(cands) < 2 {
		t.Fatalf("candidates = %d, want at least 2", len(cands))
	}
	if cands[0].ID != l2.ID {
		t.Fatalf("first candidate = %s, want the owner furthest over its guarantee (%s)", cands[0].ID, l2.ID)
	}
}

// TestPreemptionDiskFloorMixedPreemptsNothing: when the disk-allowed
// candidates cannot free enough but the disk-blocked ones would, the
// disk floor is the only obstacle, so the admission answers
// errPreemptCannot and suspends nothing.
func TestPreemptionDiskFloorMixedPreemptsNothing(t *testing.T) {
	svc, sub, ctx := newPreemptService(t)
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	small := burstLease(t, svc, ctx, "burst-a", "mid") // 1024 MiB, disk-allowed
	large := burstLease(t, svc, ctx, "burst-b", "big") // 2048 MiB, disk-blocked

	svc.cfg.TemplateStoragePath = t.TempDir()
	svc.cfg.PreemptDiskFloorPct = 15
	var total uint64 = 100 << 30
	// 16.5 GiB free: a 1024 MiB pause leaves 15.5 GiB (>= 15%), a
	// 2048 MiB one leaves 14.5 GiB (< 15%).
	svc.diskCapacity = func(string) (uint64, uint64, error) {
		return total, 16<<30 + 1<<29, nil
	}
	// The node has 512 MiB free: even pausing the small lease (1024) is
	// not enough for a 2048 MiB guaranteed lease, but pausing the large
	// one (2048) would be.
	installDynamicNode(t, svc, sub, 1024, 512, 512)

	_, err := svc.grantLease(ctx, leaseRequest{owner: "guaranteed", image: "big", ttl: time.Hour})
	if !errors.Is(err, errPreemptCannot) {
		t.Fatalf("guaranteed create err = %v, want errPreemptCannot", err)
	}
	if small.Suspended || !small.PreemptedAt.IsZero() {
		t.Fatal("disk-floor refusal must not preempt the disk-allowed lease")
	}
	if large.Suspended || !large.PreemptedAt.IsZero() {
		t.Fatal("disk-floor refusal must not preempt the disk-blocked lease")
	}
}

// TestPreemptionUnfulfillablePreemptsNothing: when no combination of
// candidates can free enough memory, preemption suspends nothing and
// falls through to the ordinary capacity check (not errPreemptCannot).
func TestPreemptionUnfulfillablePreemptsNothing(t *testing.T) {
	svc, sub, ctx := newPreemptService(t)
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	victim := burstLease(t, svc, ctx, "burst-a", "mid")

	// Zero MiB free and one 1024 MiB burst lease: a 2048 MiB guaranteed
	// lease cannot be hosted even after pausing every candidate.
	installDynamicNode(t, svc, sub, 768, 256, 512)
	_, err := svc.grantLease(ctx, leaseRequest{owner: "guaranteed", image: "big", ttl: time.Hour})
	if err == nil {
		t.Fatal("guaranteed create succeeded, want an ordinary capacity refusal")
	}
	if errors.Is(err, errPreemptCannot) {
		t.Fatalf("unfulfillable admission = errPreemptCannot, want the ordinary capacity check: %v", err)
	}
	if victim.Suspended || !victim.PreemptedAt.IsZero() {
		t.Fatalf("unfulfillable admission preempted a lease (err=%v)", err)
	}
}

// preemptOne takes back a single burst lease of consumer-a for a
// guaranteed create and returns the victim.
func preemptOne(t *testing.T, svc *Service, sub *testSub, ctx context.Context) *Lease {
	t.Helper()
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	victim := burstLease(t, svc, ctx, "consumer-a", "mid")
	burstLease(t, svc, ctx, "consumer-a", "mid")
	burstLease(t, svc, ctx, "consumer-a", "mid")
	installDynamicNode(t, svc, sub, 1536, 0, 512)
	warmFairShares(svc, ctx)
	if _, err := svc.grantLease(ctx, leaseRequest{owner: "guaranteed", image: "mid", ttl: time.Hour}); err != nil {
		t.Fatalf("guaranteed create: %v", err)
	}
	if !victim.Suspended || victim.TakeBackFor == "" {
		t.Fatal("setup: the burst lease was not taken back")
	}
	return victim
}

// TestTakenBackColdRestartRunsAgain: a path other than resume that runs
// a taken-back lease again (here a cold restart) brings it back running
// with the take-back stamp cleared.
func TestTakenBackColdRestartRunsAgain(t *testing.T) {
	svc, sub, ctx := newPreemptService(t)
	victim := preemptOne(t, svc, sub, ctx)

	installDynamicNode(t, svc, sub, 1<<20, 0, 512)
	if _, err := svc.restartCold(ctx, "consumer-a", victim); err != nil {
		t.Fatalf("cold restart: %v", err)
	}
	if victim.State != "running" || victim.TakeBackFor != "" {
		t.Fatalf("after a cold restart state=%s takeBackFor=%q, want running and no take-back stamp", victim.State, victim.TakeBackFor)
	}
	if m := leaseMap(victim, svc.effectiveCheckpointInterval(victim), svc.effectiveIdleSuspend(victim)); m["preempted"] != false {
		t.Fatalf("preempted field = %v, want false", m["preempted"])
	}
}

// TestTakenBackLostLeaseKeepsNoStamp: a taken-back lease that is lost
// loses the take-back stamp with its state (setState clears it), so no
// view reports a lost lease as taken back.
func TestTakenBackLostLeaseKeepsNoStamp(t *testing.T) {
	svc, sub, ctx := newPreemptService(t)
	victim := preemptOne(t, svc, sub, ctx)
	svc.store.mu.Lock()
	victim.setState("lost")
	svc.store.mu.Unlock()
	if victim.State != "lost" || victim.TakeBackFor != "" {
		t.Fatalf("lost taken-back lease: state=%s takeBackFor=%q, want lost and no stamp", victim.State, victim.TakeBackFor)
	}
}

// TestPreemptedHeldLeaseStaleReleased is removed with the stale-release
// rule (FS5). TestPreemptionNeverPausesPinned covers the pin contract.

// TestPreemptionNeverPausesPinned: a pinned burst lease is never a
// take-back victim (FS5); the same owner's unpinned lease goes instead,
// even though the pinned one is older.
func TestPreemptionNeverPausesPinned(t *testing.T) {
	svc, sub, ctx := newPreemptService(t)
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	// Three burst leases of one owner; the first is the oldest and gets
	// pinned, so the LRU pick must skip it.
	pinned, err := svc.grantLease(ctx, leaseRequest{owner: "consumer-a", image: "mid", ttl: time.Hour, burst: true})
	if err != nil {
		t.Fatalf("grant pinned: %v", err)
	}
	victim := burstLease(t, svc, ctx, "consumer-a", "mid")
	filler := burstLease(t, svc, ctx, "consumer-a", "mid")
	svc.store.mu.Lock()
	victim.LastActive = time.Now().Add(-time.Hour)
	filler.LastActive = time.Now()
	svc.saveLeaseLocked(victim)
	svc.saveLeaseLocked(filler)
	svc.store.mu.Unlock()
	if _, err := svc.setPinned("consumer-a", pinned.ID, true); err != nil {
		t.Fatalf("pin: %v", err)
	}

	// The owner fills the 3072 MiB pool (slice 1024, ratio 3); a 1024
	// MiB guaranteed lease takes the least recently used UNPINNED lease.
	installDynamicNode(t, svc, sub, 1536, 0, 512)
	warmFairShares(svc, ctx)
	if _, err := svc.grantLease(ctx, leaseRequest{owner: "guaranteed", image: "mid", ttl: time.Hour}); err != nil {
		t.Fatalf("guaranteed grant: %v", err)
	}
	if pinned.Suspended {
		t.Fatal("a pinned lease was taken back")
	}
	if !victim.Suspended || filler.Suspended {
		t.Fatalf("the unpinned LRU lease was not the victim: victim=%v filler=%v", victim.Suspended, filler.Suspended)
	}
}

// TestBoxFullWhenOnlyPinnedCandidates: when the only take-back
// candidates are pinned, a guaranteed admission answers box_full and
// pauses nothing (FS5).
func TestBoxFullWhenOnlyPinnedCandidates(t *testing.T) {
	svc, sub, ctx := newPreemptService(t)
	installDynamicNode(t, svc, sub, 512, 0, 512)
	pinned, err := svc.grantLease(ctx, leaseRequest{owner: "burst-a", image: "mid", ttl: time.Hour, burst: true})
	if err != nil {
		t.Fatalf("grant pinned: %v", err)
	}
	if _, err := svc.setPinned("burst-a", pinned.ID, true); err != nil {
		t.Fatalf("pin: %v", err)
	}

	// A 1024 MiB guaranteed lease cannot fit; the only candidate is
	// pinned, so this is box_full, not a preemption.
	_, err = svc.grantLease(ctx, leaseRequest{owner: "guaranteed", image: "mid", ttl: time.Hour})
	if !isBoxFull(err) {
		t.Fatalf("guaranteed grant err = %v, want box_full", err)
	}
	if pinned.Suspended {
		t.Fatal("a pinned lease was paused for a box_full request")
	}
}

// TestPreemptionTakesOnlyTheShortfall: the need the selector stops on
// is the shortfall (request minus what is already free), not the whole
// request — but the requester's after-request ratio is still computed
// for the whole request. Free 512, request 1024, one over-slice owner
// with a single takeable 512 lease: exactly that one pause and the
// create succeeds. Asking the selector to free the whole 1024 would
// make it give up (one 512 lease is all the owner can spare) and the
// create would fail on capacity.
func TestPreemptionTakesOnlyTheShortfall(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "mid", 1024)
	seedImage(t, db, "small", 512)
	svc.cfg.BurstReserveMiB = 0
	svc.SetMetrics(metrics.NewBackendMetrics())
	sub.SetNodeInfo(substrate.NodeInfo{
		Status:            "healthy",
		HugepagesTotal:    1 << 20,
		HugepageSizeBytes: 2 << 20,
	}, nil)
	svc.cfg.TemplateStoragePath = t.TempDir()
	var diskTotal uint64 = 100 << 30
	svc.diskCapacity = func(string) (uint64, uint64, error) { return diskTotal, diskTotal, nil }
	svc.diskUsage = func(dir string) (int64, error) { return 1024 << 20, nil }
	ctx := context.Background()
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	owner := preemptOwner(t, svc, "heavy")
	victim := burstLease(t, svc, ctx, owner, "small")
	filler := burstLease(t, svc, ctx, owner, "mid")
	svc.store.mu.Lock()
	victim.LastActive = time.Now().Add(-2 * time.Hour)
	filler.LastActive = time.Now().Add(-time.Hour)
	svc.saveLeaseLocked(victim)
	svc.saveLeaseLocked(filler)
	svc.store.mu.Unlock()

	// 512 MiB free (2816 total pages − 1536 base pages − two leases at
	// 512 pages each = 256 free pages): a 1024 MiB guaranteed lease is
	// 512 MiB short. The owner runs 1536 MiB; on the 5632 MiB pool with
	// four box owners that is over the 1408 MiB slice, and giving up the
	// 512 MiB lease leaves 1024/1408 — above the requester (not a box
	// owner, after-request ratio 0), so the lease can go. Giving up the
	// 1024 filler instead would drop the owner inside their slice:
	// never.
	installDynamicNode(t, svc, sub, 2816, 1536, 512)
	warmFairShares(svc, ctx)
	if _, err := svc.grantLease(ctx, leaseRequest{owner: "guaranteed", image: "mid", ttl: time.Hour}); err != nil {
		t.Fatalf("guaranteed create: %v", err)
	}
	got := pausedIDs(svc)
	if len(got) != 1 || !got[victim.ID] {
		t.Fatalf("paused %v, want exactly the 512 MiB LRU lease %s (the shortfall takes one lease)", got, victim.ID)
	}
	if filler.Suspended {
		t.Fatal("more than the shortfall was taken")
	}
}

// TestPreemptionPinMidPauseReplans: a lease pinned DURING its take-back
// pause is un-done at once (the pin wins, FS5) and must not swallow the
// pause's cache credit: the un-done resume debits the hugepages the
// pause credited, and the admission re-plans on the next victim instead
// of stopping at the refusal. heavy runs v1 (256 MiB, least recently
// used), v2 (512), filler (512) and v3 (1024); 768 MiB free; a 1024 MiB
// guaranteed request arrives and v1 is pinned inside its pause by the
// pauseBeforeSuspend hook. Expect the grant to SUCCEED: v1 comes back
// running and pinned (nothing taken from it), the re-planned round
// takes v2 for real, and filler and v3 are never touched. Without the
// debit (a) the cache still counts v1's pause credit, reads 1024 free
// and stops — the create then fails on the real 768; without the
// re-plan (b) the refused round ends the take-back and the create fails
// at once — either revert fails this test.
func TestPreemptionPinMidPauseReplans(t *testing.T) {
	svc, db, sub, ctx := newTakeBackService(t)
	seedImage(t, db, "small", 512)
	seedImage(t, db, "q", 256)
	seedImage(t, db, "giga", 1024)
	// The fair snapshot's slices exist only when every capacity read
	// works; this service shape needs the same disk stubs the preempt
	// service gets.
	svc.cfg.TemplateStoragePath = t.TempDir()
	var diskTotal uint64 = 100 << 30
	svc.diskCapacity = func(string) (uint64, uint64, error) { return diskTotal, diskTotal, nil }
	svc.diskUsage = func(dir string) (int64, error) { return 1024 << 20, nil }
	owner := preemptOwner(t, svc, "heavy")
	v1 := burstLease(t, svc, ctx, owner, "q")
	v2 := burstLease(t, svc, ctx, owner, "small")
	filler := burstLease(t, svc, ctx, owner, "small")
	v3 := burstLease(t, svc, ctx, owner, "giga")
	// A definite LRU order: v1, then v2, then filler; v3 is newest and
	// would only be reached if the take-back overshot the shortfall.
	svc.store.mu.Lock()
	base := time.Now().Add(-4 * time.Hour)
	v1.LastActive = base
	v2.LastActive = base.Add(time.Hour)
	filler.LastActive = base.Add(2 * time.Hour)
	v3.LastActive = base.Add(3 * time.Hour)
	svc.saveLeaseLocked(v1)
	svc.saveLeaseLocked(v2)
	svc.saveLeaseLocked(filler)
	svc.saveLeaseLocked(v3)
	svc.store.mu.Unlock()

	// 2432 pages of 2 MiB, 512 pages (1 GiB) per sandbox: the four
	// sandboxes leave 768 MiB free, so the 1024 MiB request is 256
	// short. The four box owners slice the 4864 MiB pool into 1216 MiB;
	// heavy runs 2304 MiB — far over, and every give-up stays above the
	// requester (a non-box owner, after-request ratio 0).
	installDynamicNode(t, svc, sub, 2432, 0, 512)
	warmFairShares(svc, ctx)
	svc.pauseBeforeSuspend = func(l *Lease) {
		svc.pauseBeforeSuspend = nil
		if l.ID != v1.ID {
			t.Errorf("pause reached %s before %s; want the least recently used lease first", l.ID, v1.ID)
		}
		if _, err := svc.setPinned(owner, v1.ID, true); err != nil {
			t.Errorf("pin mid-pause: %v", err)
		}
	}
	if _, err := svc.grantLease(ctx, leaseRequest{owner: "guaranteed", image: "giga", ttl: time.Hour}); err != nil {
		t.Fatalf("guaranteed create: %v (the pin un-does its pause; the take-back must move on)", err)
	}
	svc.pauseBeforeSuspend = nil

	svc.store.mu.Lock()
	v1Pinned, v1Suspended := v1.Pinned, v1.Suspended
	svc.store.mu.Unlock()
	if !v1Pinned || v1Suspended {
		t.Fatalf("v1 pinned=%v suspended=%v, want pinned and running again (a pin un-does its pause)", v1Pinned, v1Suspended)
	}
	got := pausedIDs(svc)
	if !got[v2.ID] {
		t.Fatalf("paused %v, want %s taken for real after the pin un-did %s", got, v2.ID, v1.ID)
	}
	if got[filler.ID] || got[v3.ID] {
		t.Fatalf("paused %v, want %s and %s never touched (v2 alone covered the shortfall)", got, filler.ID, v3.ID)
	}
}

// TestPreemptionUndrainDefers: an undrain whose guaranteed lease needs
// room it cannot preempt for (the snapshot disk is under the floor)
// leaves the lease drained and suspended, not lost.
func TestPreemptionUndrainDefers(t *testing.T) {
	svc, sub, ctx := newPreemptService(t)
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	g, err := svc.grantLease(ctx, leaseRequest{owner: "guaranteed", image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("guaranteed create: %v", err)
	}
	if _, err := svc.suspend(ctx, "guaranteed", g.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	svc.store.mu.Lock()
	g.Drained = true
	svc.saveLeaseLocked(g)
	svc.store.mu.Unlock()
	burstLease(t, svc, ctx, "burst-a", "mid")

	// Full node, disk under the floor: the resume would have to preempt
	// and cannot.
	svc.cfg.TemplateStoragePath = t.TempDir()
	svc.cfg.PreemptDiskFloorPct = 15
	var total uint64 = 100 << 30
	svc.diskCapacity = func(string) (uint64, uint64, error) { return total, total / 10, nil }
	installDynamicNode(t, svc, sub, 2048, 2048-512, 512)

	res := svc.undrain(ctx)
	if len(res.Failed) != 1 || res.Resumed != 0 {
		t.Fatalf("undrain = %+v, want the guaranteed lease in failed", res)
	}
	if !g.Drained || g.State != "suspended" || !g.LostAt.IsZero() {
		t.Fatalf("preempt-refused undrain: state=%s drained=%v lost=%v, want suspended, drained, not lost", g.State, g.Drained, !g.LostAt.IsZero())
	}
}

// TestGuaranteedUsesReserveWithoutPreempting: the burst reserve is room
// kept for guaranteed work, so a guaranteed create that fits in free
// memory is admitted without preempting anyone, even when that dips
// into the reserve. Only a guaranteed lease that does not fit takes
// memory back.
func TestGuaranteedUsesReserveWithoutPreempting(t *testing.T) {
	svc, sub, ctx := newPreemptService(t)
	svc.cfg.BurstReserveMiB = 2048
	// Room for the burst leases above the reserve: 16 GiB, nothing used.
	installDynamicNode(t, svc, sub, 8192, 0, 512)
	owner := preemptOwner(t, svc, "heavy")
	victim := burstLease(t, svc, ctx, owner, "mid")
	var others []*Lease
	for i := 0; i < 4; i++ {
		others = append(others, burstLease(t, svc, ctx, owner, "mid"))
	}
	// A definite LRU order: the victim is the least recently used, the
	// others follow in grant order — so the take-back pick is the victim
	// itself, not "some lease of the owner".
	svc.store.mu.Lock()
	base := time.Now().Add(-5 * time.Hour)
	victim.LastActive = base
	for i, l := range others {
		l.LastActive = base.Add(time.Duration(i+1) * time.Hour)
		svc.saveLeaseLocked(l)
	}
	svc.saveLeaseLocked(victim)
	svc.store.mu.Unlock()

	// Now 1 GiB free (5120 pages base + the five 1024 MiB leases on the
	// 16 GiB pool): a 1 GiB guaranteed lease fits, though it takes the
	// node under the 2 GiB reserve. Taking the owner's leases back
	// would restore the reserve, and must not happen.
	installDynamicNode(t, svc, sub, 8192, 5120, 512)
	warmFairShares(svc, ctx)
	if _, err := svc.grantLease(ctx, leaseRequest{owner: "guaranteed", image: "mid", ttl: time.Hour}); err != nil {
		t.Fatalf("guaranteed create: %v", err)
	}
	if victim.Suspended || victim.TakeBackFor != "" {
		t.Fatal("a guaranteed lease that fits took a burst lease back to protect the reserve")
	}

	// Full node (base + the five leases + grant1's sandbox = exactly
	// the 16 GiB pool): the next guaranteed lease does take one of the
	// owner's five leases (5120 MiB over a 4096 MiB slice; giving one
	// leaves the owner strictly above the requester), and the paused
	// lease's room admits the new sandbox.
	installDynamicNode(t, svc, sub, 8192, 8192-6*512, 512)
	warmFairShares(svc, ctx)
	if _, err := svc.grantLease(ctx, leaseRequest{owner: "guaranteed", image: "mid", ttl: time.Hour}); err != nil {
		t.Fatalf("guaranteed create on a full node: %v", err)
	}
	if !victim.Suspended || victim.TakeBackFor == "" {
		t.Fatalf("a guaranteed lease that does not fit should take the owner's least recently used lease (%s) back", victim.ID)
	}
	for _, l := range others {
		if l.Suspended {
			t.Fatalf("lease %s was paused though an older lease of the owner freed enough", l.ID)
		}
	}
}
