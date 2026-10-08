package api

import (
	"context"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/substrate"
)

// TestReleaseDropsPerLeaseMaps pins the L1 leak fix: releasing a lease
// removes its appliedEgress memo and any pending exec-time secret
// removals, so neither map grows one entry per released lease for the
// life of the process.
func TestReleaseDropsPerLeaseMaps(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	// grant records the applied egress itself; make sure there is one and
	// stage a deferred secret removal the same way a finishing job would.
	svc.recordAppliedEgress(l.ID, substrate.Egress{DeniedCIDRs: []string{"0.0.0.0/0"}})
	svc.deferSecretRemoval(l.ID, []string{"TOKEN"})

	svc.releaseBecause(ctx, l, "released through the API")

	svc.appliedMu.Lock()
	_, hasEgress := svc.appliedEgress[l.ID]
	svc.appliedMu.Unlock()
	if hasEgress {
		t.Errorf("appliedEgress still has an entry for released lease %s", l.ID)
	}
	svc.secretsMu.Lock()
	_, hasPending := svc.pendingSecretRemovals[l.ID]
	svc.secretsMu.Unlock()
	if hasPending {
		t.Errorf("pendingSecretRemovals still has an entry for released lease %s", l.ID)
	}
}

// TestDeferSecretRemovalAfterReleaseDrops: a finishing job that defers
// an exec-time secret removal after its lease was released must not
// re-add the lease's pending entry — the sandbox is gone, so the save
// that would drain it can never run for this lease (spoond-966
// follow-up).
func TestDeferSecretRemovalAfterReleaseDrops(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.releaseBecause(ctx, l, "released through the API")

	svc.deferSecretRemoval(l.ID, []string{"TOKEN"})
	svc.secretsMu.Lock()
	_, hasPending := svc.pendingSecretRemovals[l.ID]
	svc.secretsMu.Unlock()
	if hasPending {
		t.Errorf("defer after release re-added an entry for lease %s", l.ID)
	}
}
