package api

import (
	"context"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/substrate"
)

// appliedEgressHas reports whether a lease id has a memo entry.
func appliedEgressHas(svc *Service, leaseID string) bool {
	svc.appliedMu.Lock()
	defer svc.appliedMu.Unlock()
	_, ok := svc.appliedEgress[leaseID]
	return ok
}

// TestRefreshPeersDropsMemoForNonLiveLease pins the follow-up to the L1
// leak fix: a refresh reaps the memo of a lease that is no longer live
// and unreleased, covering a refresh or network-policy update that
// re-added one after the release cleared it.
func TestRefreshPeersDropsMemoForNonLiveLease(t *testing.T) {
	svc, sub := newLifecycleService(t)
	ctx := context.Background()

	l := &Lease{ID: "aaaa", Owner: "u-a", Image: "py-base", State: "running", NetPolicy: "lan",
		SandboxID: "sb-a", HostIP: "10.11.0.5", ExposePorts: []int{8080}, ExposedIP: "10.11.0.5"}
	insertLease(t, svc, l)
	svc.recordAppliedEgress(l.ID, svc.egressFor(l))

	if !appliedEgressHas(svc, l.ID) {
		t.Fatalf("live lease lost its memo after the first refresh")
	}
	// Suspend the lease: it is no longer live, so its memo must go.
	svc.store.mu.Lock()
	l.State = "suspended"
	svc.store.mu.Unlock()
	svc.runRefreshPeers(ctx)
	if appliedEgressHas(svc, l.ID) {
		t.Errorf("refresh kept the memo of a non-live lease")
	}
	_ = sub
}

// TestRefreshPeersKeepsInFlightMemo: a lease whose egress memo was
// recorded but whose lease row is not in the store yet (a create in
// progress) must keep its memo through a refresh. Dropping it would
// force one redundant UpdateEgress once the create finishes (spoond-ob18).
func TestRefreshPeersKeepsInFlightMemo(t *testing.T) {
	svc, _ := newLifecycleService(t)
	svc.beginAppliedEgress("creating")
	svc.recordAppliedEgress("creating", substrate.Egress{DeniedCIDRs: []string{"0.0.0.0/0"}})

	svc.runRefreshPeers(context.Background())
	if !appliedEgressHas(svc, "creating") {
		t.Errorf("refresh dropped the memo of a lease mid-create")
	}

	// Once the create registers the lease and clears the hold, a refresh
	// that still does not see the lease may reap the memo again.
	svc.endAppliedEgress("creating")
	svc.runRefreshPeers(context.Background())
	if appliedEgressHas(svc, "creating") {
		t.Errorf("refresh kept a stale memo after the in-flight hold cleared")
	}
}

// TestRefreshPeersKeepsPoolMemo: the "pool" placeholder is never in the
// live store, so the reap must spare it.
func TestRefreshPeersKeepsPoolMemo(t *testing.T) {
	svc, _ := newLifecycleService(t)
	svc.recordAppliedEgress("pool", substrate.Egress{DeniedCIDRs: []string{"0.0.0.0/0"}})
	svc.runRefreshPeers(context.Background())
	if !appliedEgressHas(svc, "pool") {
		t.Errorf("refresh dropped the pool placeholder memo")
	}
}

// TestRefreshPeersReapsReAddedReleasedMemo: a memo written after a
// release's cleanup (the re-add race) is dropped by the next refresh,
// so it cannot grow the map for the life of the process.
func TestRefreshPeersReapsReAddedReleasedMemo(t *testing.T) {
	svc, _ := newLifecycleService(t)
	// No lease, no pool: an entry for an unknown lease is stale.
	svc.recordAppliedEgress("deadbeef", substrate.Egress{})
	svc.runRefreshPeers(context.Background())
	if appliedEgressHas(svc, "deadbeef") {
		t.Errorf("refresh kept a stale memo with no live lease")
	}
}

// TestGrantMidCreateRefreshKeepsMemo drives the race directly: a
// refreshPeers runs while a grant is between recording the egress memo
// and inserting the lease. The memo must survive, so the finished grant
// still skips a redundant UpdateEgress (spoond-ob18).
func TestGrantMidCreateRefreshKeepsMemo(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)

	ran := false
	sub.execBefore = func(sandboxID string, req substrate.ExecRequest) {
		if ran || len(req.Args) != 3 || req.Args[2] != integrityProbe {
			return
		}
		ran = true
		// Mid-create: the memo is recorded, the lease is not yet in the
		// store, and the refresh must not reap the memo.
		svc.runRefreshPeers(context.Background())
	}
	t.Cleanup(func() { sub.execBefore = nil })

	l, err := svc.grant(context.Background(), "c", "py-base", time.Minute, true, "lan", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if !ran {
		t.Fatal("the integrity probe never ran; the test did not exercise the window")
	}
	if !appliedEgressHas(svc, l.ID) {
		t.Fatalf("mid-create refresh dropped the memo")
	}
	// The memo matches the applied egress, so a later refresh applies
	// nothing extra.
	before := calls(sub.Fake, "UpdateEgress")
	svc.runRefreshPeers(context.Background())
	if got := calls(sub.Fake, "UpdateEgress"); got != before {
		t.Errorf("post-grant refresh re-applied egress: %d calls, want %d: %v", got, before, sub.Fake.CallLog())
	}
}

// TestGrantFailureDropsAppliedEgress pins the create path's cleanup: a
// grant that fails after the sandbox create (here the integrity probe)
// forgets the memo createSandbox recorded, so the failed lease leaves no
// entry behind.
func TestGrantFailureDropsAppliedEgress(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	sub.probeFailAll = true

	if _, err := svc.grant(context.Background(), "c", "py-base", time.Minute, true, "", nil, "", "", nil); err == nil {
		t.Fatalf("grant succeeded with a failing probe")
	}
	svc.appliedMu.Lock()
	n := len(svc.appliedEgress)
	svc.appliedMu.Unlock()
	if n != 0 {
		t.Errorf("failed grant left %d appliedEgress entries, want 0", n)
	}
}

// TestSuccessfulGrantKeepsAppliedEgress guards the other side: the
// cleanup must not drop a successful grant's memo, so refreshPeers still
// skips a redundant update.
func TestSuccessfulGrantKeepsAppliedEgress(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)

	l, err := svc.grant(context.Background(), "c", "py-base", time.Minute, true, "lan", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if !appliedEgressHas(svc, l.ID) {
		t.Errorf("successful grant has no appliedEgress memo")
	}
}
