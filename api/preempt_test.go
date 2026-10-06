package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/identity"
	"github.com/jrimmer/spoond/v2/metrics"
	"github.com/jrimmer/spoond/v2/substrate"
)

// Preemption tests (#128 part 3). The fake substrate's NodeInfo is made
// controllable by a function that counts the fake's live sandboxes, so a
// pause really frees hugepages: the same shape the orchestrator reports
// in production.

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
	return svc, sub, context.Background()
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

// TestPreemptionOrderPriorityThenNewest: preemption takes the lowest
// priority first, then the newest lease.
func TestPreemptionOrderPriorityThenNewest(t *testing.T) {
	svc, sub, ctx := newPreemptService(t)
	// Room for the burst leases; the guaranteed lease below needs two
	// of the three preempted.
	installDynamicNode(t, svc, sub, 4096, 0, 512)

	old := burstLease(t, svc, ctx, "burst-a", "mid")
	newest := burstLease(t, svc, ctx, "burst-b", "mid")
	lowest := burstLease(t, svc, ctx, "burst-c", "mid")

	base := time.Now()
	svc.store.mu.Lock()
	old.CreatedAt = base.Add(-2 * time.Hour)
	newest.CreatedAt = base.Add(-time.Minute)
	lowest.CreatedAt = base.Add(-time.Hour)
	old.Priority = 5
	newest.Priority = 5
	lowest.Priority = -1 // must go first
	for _, l := range []*Lease{old, newest, lowest} {
		svc.saveLeaseLocked(l)
	}
	svc.store.mu.Unlock()

	// Fill the node so only the leases themselves are free, then ask for
	// 2048 MiB: two 1024 MiB leases must be preempted.
	installDynamicNode(t, svc, sub, 2048, 2048-3*512, 512)
	guaranteed, err := svc.grantLease(ctx, leaseRequest{owner: "guaranteed", image: "big", ttl: time.Hour})
	if err != nil {
		t.Fatalf("guaranteed create: %v", err)
	}
	if guaranteed.Class != ClassGuaranteed {
		t.Fatalf("guaranteed create class = %s", guaranteed.Class)
	}

	got := preemptedIDs(svc)
	if !got[lowest.ID] {
		t.Fatalf("lowest-priority lease %s was not preempted (preempted: %v)", lowest.ID, got)
	}
	if !got[newest.ID] {
		t.Fatalf("newest lease %s was not preempted second (preempted: %v)", newest.ID, got)
	}
	if got[old.ID] {
		t.Fatalf("older same-priority lease %s was preempted, want the newest first", old.ID)
	}
}

// TestPreemptionFreesEnoughAndNoMore: preemption stops as soon as the
// guaranteed lease fits, leaving the other burst leases running.
func TestPreemptionFreesEnoughAndNoMore(t *testing.T) {
	svc, sub, ctx := newPreemptService(t)
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	a := burstLease(t, svc, ctx, "burst-a", "mid")
	b := burstLease(t, svc, ctx, "burst-b", "mid")
	c := burstLease(t, svc, ctx, "burst-c", "mid")
	_ = c

	// One preemption frees 1024 MiB, enough for a 1024 MiB guaranteed
	// lease: exactly one of the three goes.
	installDynamicNode(t, svc, sub, 3072, 3072-3*512, 512)
	if _, err := svc.grantLease(ctx, leaseRequest{owner: "guaranteed", image: "mid", ttl: time.Hour}); err != nil {
		t.Fatalf("guaranteed create: %v", err)
	}
	got := preemptedIDs(svc)
	if len(got) != 1 {
		t.Fatalf("preempted %d leases, want exactly 1 (%v)", len(got), got)
	}
	running := 0
	svc.store.mu.Lock()
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

// TestPreemptionAutomaticResume: the resume queue brings preempted
// leases back when capacity returns, oldest preemption first, clearing
// preempted and emitting "resumed" with detail "after preemption".
func TestPreemptionAutomaticResume(t *testing.T) {
	svc, sub, ctx := newPreemptService(t)
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	burstLease(t, svc, ctx, "burst-a", "mid")
	burstLease(t, svc, ctx, "burst-b", "mid")

	installDynamicNode(t, svc, sub, 3072, 3072-2*512, 512)
	if _, err := svc.grantLease(ctx, leaseRequest{owner: "guaranteed", image: "mid", ttl: time.Hour}); err != nil {
		t.Fatalf("guaranteed create: %v", err)
	}
	pre := preemptedIDs(svc)
	if len(pre) != 1 {
		t.Fatalf("want 1 preempted lease, got %v", pre)
	}
	var victim *Lease
	for id := range pre {
		victim = svc.lookup("burst-a", id)
		if victim == nil {
			victim = svc.lookup("burst-b", id)
		}
	}
	if victim == nil {
		t.Fatal("preempted lease not found")
	}
	if victim.Generation != 1 {
		t.Fatalf("preemption changed the generation to %d, want 1", victim.Generation)
	}

	// Watch the resumed event.
	subEvents := svc.Subscribe(EventFilter{LeaseID: victim.ID})
	defer subEvents.Close()

	// The guaranteed lease releases, capacity returns; the queue resumes
	// the victim.
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	svc.resumePreempted(ctx)
	if victim.Suspended || !victim.PreemptedAt.IsZero() {
		t.Fatalf("victim after resume: suspended=%v preemptedAt=%v", victim.Suspended, victim.PreemptedAt)
	}
	if victim.Generation != 1 {
		t.Fatalf("resume changed the generation to %d, want 1", victim.Generation)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-subEvents.C:
			if ev.Type == LeaseResumed {
				if ev.Detail != "after preemption" {
					t.Fatalf("resumed detail = %q, want %q", ev.Detail, "after preemption")
				}
				return
			}
		case <-deadline:
			t.Fatal("no resumed event for the preempted lease")
		}
	}
}

// TestPreemptionResumeOldestFirst: with capacity for one, the oldest
// preemption resumes first.
func TestPreemptionResumeOldestFirst(t *testing.T) {
	svc, sub, ctx := newPreemptService(t)
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	a := burstLease(t, svc, ctx, "burst-a", "mid")
	b := burstLease(t, svc, ctx, "burst-b", "mid")

	// Force both preempted with distinct preemption instants.
	if err := svc.preemptLease(ctx, a, "guaranteed"); err != nil {
		t.Fatalf("preempt a: %v", err)
	}
	if err := svc.preemptLease(ctx, b, "guaranteed"); err != nil {
		t.Fatalf("preempt b: %v", err)
	}
	svc.store.mu.Lock()
	a.PreemptedAt = time.Now().Add(-time.Hour)
	b.PreemptedAt = time.Now().Add(-time.Minute)
	svc.saveLeaseLocked(a)
	svc.saveLeaseLocked(b)
	svc.store.mu.Unlock()

	// Room for exactly one 1024 MiB lease.
	installDynamicNode(t, svc, sub, 2048, 1536, 512)
	svc.resumePreempted(ctx)
	if a.Suspended {
		t.Fatal("oldest preemption should have resumed first")
	}
	if !b.Suspended {
		t.Fatal("newer preemption resumed while only one lease fits")
	}
}

// TestPreemptionSerialised: two concurrent guaranteed creates preempt
// once between them, because one preemption frees enough for both and
// the preemption mutex makes the second see the first's freed memory.
func TestPreemptionSerialised(t *testing.T) {
	svc, sub, ctx := newPreemptService(t)
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	burstLease(t, svc, ctx, "burst-a", "mid")

	// The burst lease's 1024 MiB is the only free memory; the two
	// guaranteed leases each need 1024 MiB.
	installDynamicNode(t, svc, sub, 2048, 1024, 512)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = svc.grantLease(ctx, leaseRequest{owner: "guaranteed", image: "mid", ttl: time.Hour})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("guaranteed create %d: %v", i, err)
		}
	}
	if got := preemptedIDs(svc); len(got) != 1 {
		t.Fatalf("concurrent creates preempted %d leases, want exactly 1 (serialised)", len(got))
	}
}

// TestPreemptionAPIReportsPreempted: a preempted lease reports
// "preempted": true in list and detail, carries no generation bump, and
// emits one "preempted" event naming the guaranteed lease's owner.
func TestPreemptionAPIReportsPreempted(t *testing.T) {
	svc, sub, ctx := newPreemptService(t)
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	victim := burstLease(t, svc, ctx, "burst-a", "mid")

	events := svc.Subscribe(EventFilter{LeaseID: victim.ID})
	defer events.Close()

	installDynamicNode(t, svc, sub, 2048, 2048-512, 512)
	if _, err := svc.grantLease(ctx, leaseRequest{owner: "guaranteed", image: "mid", ttl: time.Hour}); err != nil {
		t.Fatalf("guaranteed create: %v", err)
	}

	m := leaseMap(victim, svc.effectiveCheckpointInterval(victim), svc.effectiveIdleSuspend(victim))
	if got := m["preempted"]; got != true {
		t.Fatalf("preempted field = %v, want true", got)
	}
	if victim.Generation != 1 {
		t.Fatalf("preemption bumped the generation to %d, want 1", victim.Generation)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events.C:
			if ev.Type == LeasePreempted {
				if ev.Detail != "for a guaranteed lease of guaranteed" {
					t.Fatalf("preempted detail = %q", ev.Detail)
				}
				return
			}
		case <-deadline:
			t.Fatal("no preempted event")
		}
	}
}

// TestPreemptionCandidateOrderOverGuarantee: with equal priority and
// age, the owner furthest over its guarantee is preempted first.
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

// preemptOne preempts a single burst lease of burst-a for a guaranteed
// create and returns the victim.
func preemptOne(t *testing.T, svc *Service, sub *testSub, ctx context.Context) *Lease {
	t.Helper()
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	victim := burstLease(t, svc, ctx, "burst-a", "mid")
	installDynamicNode(t, svc, sub, 2048, 2048-512, 512)
	if _, err := svc.grantLease(ctx, leaseRequest{owner: "guaranteed", image: "mid", ttl: time.Hour}); err != nil {
		t.Fatalf("guaranteed create: %v", err)
	}
	if victim.PreemptedAt.IsZero() || !victim.Suspended {
		t.Fatal("setup: the burst lease was not preempted")
	}
	return victim
}

// TestPreemptedColdRestartClearsFlag: a path other than resume that runs
// a preempted lease again (here a cold restart) ends the preemption, so
// the lease does not report preempted while running, the gauge drops
// it, and the resume queue no longer picks it up.
func TestPreemptedColdRestartClearsFlag(t *testing.T) {
	svc, sub, ctx := newPreemptService(t)
	victim := preemptOne(t, svc, sub, ctx)

	installDynamicNode(t, svc, sub, 1<<20, 0, 512)
	if _, err := svc.restartCold(ctx, "burst-a", victim); err != nil {
		t.Fatalf("cold restart: %v", err)
	}
	if victim.State != "running" || !victim.PreemptedAt.IsZero() {
		t.Fatalf("after a cold restart state=%s preempted=%v, want running and not preempted", victim.State, !victim.PreemptedAt.IsZero())
	}
	if m := leaseMap(victim, svc.effectiveCheckpointInterval(victim), svc.effectiveIdleSuspend(victim)); m["preempted"] != false {
		t.Fatalf("preempted field = %v, want false", m["preempted"])
	}
	svc.store.mu.Lock()
	n := svc.preemptedCountLocked()
	svc.store.mu.Unlock()
	if n != 0 {
		t.Fatalf("preempted gauge count = %d, want 0", n)
	}
}

// TestPreemptedLostLeaseNotCounted: a preempted lease that is lost is no
// longer preempted, so the gauge (like the dashboard) does not count it.
func TestPreemptedLostLeaseNotCounted(t *testing.T) {
	svc, sub, ctx := newPreemptService(t)
	victim := preemptOne(t, svc, sub, ctx)
	svc.store.mu.Lock()
	victim.setState("lost")
	n := svc.preemptedCountLocked()
	svc.store.mu.Unlock()
	if n != 0 || !victim.PreemptedAt.IsZero() {
		t.Fatalf("lost preempted lease: count=%d preempted=%v, want 0 and false", n, !victim.PreemptedAt.IsZero())
	}
}

// TestPreemptedHeldLeaseNeverStaleReleased: a held lease waiting in the
// resume queue is not released by the stale rule, even when it still
// carries an idle-suspend LastAction older than the release limit.
func TestPreemptedHeldLeaseNeverStaleReleased(t *testing.T) {
	svc, sub, ctx := newPreemptService(t)
	victim := preemptOne(t, svc, sub, ctx)
	old := time.Now().Add(-30 * 24 * time.Hour)
	svc.store.mu.Lock()
	victim.Holder = "pool:honey/work-1"
	victim.LastAction = heldRuleIdle + "/" + heldActionSuspendIdle
	victim.LastActionAt = old
	victim.LastActive = old.Add(-time.Hour)
	svc.store.mu.Unlock()
	svc.cfg.HeldSuspendedRelease = time.Hour

	svc.releaseStaleHeld(ctx, time.Now())
	if victim.released || victim.State != "suspended" || victim.PreemptedAt.IsZero() {
		t.Fatalf("stale rule touched a preempted lease: released=%v state=%s", victim.released, victim.State)
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
