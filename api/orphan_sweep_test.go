package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/substrate"
)

// Orphan sweep tests (spoond-abc, spoond-63a): the periodic sweep deletes
// a sandbox no lease and no pool entry claims only on the second
// consecutive pass, never while a create holds it in flight, never while
// draining, and never a pool sandbox.

// TestOrphanSweepTwoPassOnly: an unclaimed sandbox survives the first
// pass (a create whose lease row has not landed yet) and is deleted on
// the second pass once it is still unclaimed.
func TestOrphanSweepTwoPassOnly(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	sb, err := sub.Create(ctx, substrate.CreateRequest{SandboxID: "unclaimed-1"})
	if err != nil {
		t.Fatalf("create unclaimed: %v", err)
	}

	svc.sweepOrphanSandboxes(ctx)
	if !sandboxOnFake(t, sub, sb.ID) {
		t.Fatalf("the unclaimed sandbox %s was swept on the first pass", sb.ID)
	}
	svc.sweepOrphanSandboxes(ctx)
	if sandboxOnFake(t, sub, sb.ID) {
		t.Fatalf("the unclaimed sandbox %s survived the second pass", sb.ID)
	}
}

// TestOrphanSweepResetsOnNewStartedAt: a sandbox id reused with a new
// StartedAt (a fresh boot) is not swept on the pass after its first
// sighting, even though the id was unclaimed before.
func TestOrphanSweepResetsOnNewStartedAt(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	sb, err := sub.Create(ctx, substrate.CreateRequest{SandboxID: "reused-1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// First pass: sighting.
	svc.sweepOrphanSandboxes(ctx)
	// The sandbox is replaced by a fresh boot with the same id (a new
	// StartedAt); it must start its two-pass count over.
	sub.Fake.Kill(sb.ID)
	if _, err := sub.Create(ctx, substrate.CreateRequest{SandboxID: sb.ID}); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	svc.sweepOrphanSandboxes(ctx)
	if !sandboxOnFake(t, sub, sb.ID) {
		t.Fatalf("a reused sandbox id was swept on its first new sighting")
	}
	svc.sweepOrphanSandboxes(ctx)
	if sandboxOnFake(t, sub, sb.ID) {
		t.Fatalf("the reused sandbox survived its second consecutive sighting")
	}
}

// TestOrphanSweepSkipsInFlight: a sandbox a create holds in flight is
// never swept, even across several passes.
func TestOrphanSweepSkipsInFlight(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	sb, err := sub.Create(ctx, substrate.CreateRequest{SandboxID: "inflight-1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	svc.beginCreatingSandbox(sb.ID)
	t.Cleanup(func() { svc.endCreatingSandbox(sb.ID) })

	for i := 0; i < 3; i++ {
		svc.sweepOrphanSandboxes(ctx)
	}
	if !sandboxOnFake(t, sub, sb.ID) {
		t.Fatalf("an in-flight create's sandbox %s was swept", sb.ID)
	}
	// Once the create finishes unclaimed, the two-pass rule applies.
	svc.endCreatingSandbox(sb.ID)
	svc.sweepOrphanSandboxes(ctx)
	if !sandboxOnFake(t, sub, sb.ID) {
		t.Fatalf("the sandbox was swept before its second unclaimed pass")
	}
	svc.sweepOrphanSandboxes(ctx)
	if sandboxOnFake(t, sub, sb.ID) {
		t.Fatalf("the unclaimed sandbox survived after the create finished")
	}
}

// TestOrphanSweepCreateBlockedNeverSwept: a real Service create that
// blocks on the substrate is never swept while it is in flight.
func TestOrphanSweepCreateBlockedNeverSwept(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	entered := make(chan struct{})
	release := make(chan struct{})
	var created string
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		created = req.SandboxID
		close(entered)
		<-release
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	done := make(chan error, 1)
	go func() {
		_, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
		done <- err
	}()
	<-entered

	// Sweep twice while the create is blocked: the in-flight id must
	// survive both passes.
	svc.sweepOrphanSandboxes(ctx)
	svc.sweepOrphanSandboxes(ctx)
	if created == "" || svc.sandboxInFlight(created) == false {
		t.Fatalf("setup: sandbox %q should be in flight", created)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("grant: %v", err)
	}
	if !sandboxOnFake(t, sub, created) {
		t.Fatalf("the granted sandbox %s was not created", created)
	}
}

// TestOrphanSweepSkipsPool: a warm-pool sandbox is never swept.
func TestOrphanSweepSkipsPool(t *testing.T) {
	svc, db, sub := newTestService(t)
	img := seedImage(t, db, "py-base", 2048)
	svc.cfg.PoolSize = 1
	ctx := context.Background()

	svc.warmPool(ctx, img)
	svc.store.mu.Lock()
	pooled := append([]string(nil), svc.store.pool["py-base"]...)
	svc.store.mu.Unlock()
	if len(pooled) != 1 {
		t.Fatalf("expected 1 pooled sandbox, got %d", len(pooled))
	}

	svc.sweepOrphanSandboxes(ctx)
	svc.sweepOrphanSandboxes(ctx)
	if !sandboxOnFake(t, sub, pooled[0]) {
		t.Fatalf("the pooled sandbox %s was swept", pooled[0])
	}
}

// TestOrphanSweepSkippedWhileDraining: the whole sweep is skipped while
// the node is draining, so a sandbox mid-pause is never deleted.
func TestOrphanSweepSkippedWhileDraining(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	sb, err := sub.Create(ctx, substrate.CreateRequest{SandboxID: "drain-1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	svc.draining.Store(true)
	svc.sweepOrphanSandboxes(ctx)
	svc.sweepOrphanSandboxes(ctx)
	if !sandboxOnFake(t, sub, sb.ID) {
		t.Fatalf("the sweep deleted %s while draining", sb.ID)
	}
	svc.draining.Store(false)
}

// TestOrphanSweepRetryNotResurrected: a remembered sandbox id that a
// lease now claims (or a pool holds) is not deleted by the retry, and is
// forgotten.
func TestOrphanSweepRetryNotResurrected(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.rememberOrphanSandbox(l.SandboxID)
	svc.sweepOrphanSandboxes(ctx)
	if !sandboxOnFake(t, sub, l.SandboxID) {
		t.Fatalf("the sweep deleted a live lease's remembered sandbox %s", l.SandboxID)
	}
	if ids := svc.orphanSandboxSnapshot(); len(ids) != 0 {
		t.Fatalf("remembered ids = %v, want none after a skipped retry", ids)
	}
}

// TestOrphanSafeToDelete pins the final guard: only a lost, non-busy
// lease's sandbox and a truly unclaimed one are safe.
func TestOrphanSafeToDelete(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	live, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if svc.orphanSafeToDelete(live.SandboxID) {
		t.Fatal("a live lease's sandbox was reported safe to delete")
	}

	svc.store.mu.Lock()
	live.setState("lost")
	svc.saveLeaseLocked(live)
	svc.store.mu.Unlock()
	if !svc.orphanSafeToDelete(live.SandboxID) {
		t.Fatal("a lost lease's sandbox was reported unsafe to delete")
	}

	svc.store.mu.Lock()
	live.setState("lost")
	live.busy = true
	svc.store.mu.Unlock()
	if svc.orphanSafeToDelete(live.SandboxID) {
		t.Fatal("a busy lost lease's sandbox was reported safe to delete")
	}
	svc.store.mu.Lock()
	live.busy = false
	svc.store.mu.Unlock()

	svc.beginCreatingSandbox("new-id")
	if svc.orphanSafeToDelete("new-id") {
		t.Fatal("an in-flight sandbox was reported safe to delete")
	}
	svc.endCreatingSandbox("new-id")
	if !svc.orphanSafeToDelete("new-id") {
		t.Fatal("an unclaimed sandbox was reported unsafe to delete")
	}
}

// TestOrphanSweepDeletesUnclaimedAfterFailedDelete: an unclaimed
// sandbox whose first delete fails is remembered and retried, not lost.
func TestOrphanSweepDeletesUnclaimedAfterFailedDelete(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	sb, err := sub.Create(ctx, substrate.CreateRequest{SandboxID: "retry-1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// First pass: sighting. Second pass: delete, but the Delete fails.
	svc.sweepOrphanSandboxes(ctx)
	sub.FailCall("Delete", 1, errors.New("substrate unreachable"))
	svc.sweepOrphanSandboxes(ctx)
	if !sandboxOnFake(t, sub, sb.ID) {
		t.Fatal("precondition: the delete should have failed")
	}
	if ids := svc.orphanSandboxSnapshot(); len(ids) != 1 || ids[0] != sb.ID {
		t.Fatalf("remembered ids = %v, want [%s]", ids, sb.ID)
	}
	// Next pass: the retry succeeds.
	svc.sweepOrphanSandboxes(ctx)
	if sandboxOnFake(t, sub, sb.ID) {
		t.Fatalf("the remembered sandbox %s survived the retry", sb.ID)
	}
}
