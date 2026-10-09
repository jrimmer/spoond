package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
)

// Recovery and preempt-resume retries (spoond-dxq): a transient failure
// keeps a lease recovering under a bounded budget, a permanent one loses
// it at once, and a capacity refusal waits for capacity (still bounded).

// recoverTargetCheckpoint grants a checkpointed lease and kills its
// sandbox, leaving it for the crash reconcile.
func recoverTargetCheckpoint(t *testing.T, svc *Service, sub *testSub, ctx context.Context) *Lease {
	t.Helper()
	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.checkpointLease(ctx, l); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	sub.Fake.Kill(l.SandboxID)
	return l
}

// TestRecoveryRetriesTransientThenSucceeds: a recovery that fails twice
// with a transient error keeps the lease recovering, and the third
// reconcile pass succeeds — the lease is not lost on a transient failure.
func TestRecoveryRetriesTransientThenSucceeds(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.cfg.RecoveryRetryAttempts = 3

	l := recoverTargetCheckpoint(t, svc, sub, ctx)
	sb := l.SandboxID

	var attempts int
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume && req.SandboxID == sb {
			attempts++
			if attempts <= 2 {
				return substrate.Sandbox{}, errors.New("failed to init envd: syncing took too long")
			}
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	// First pass: transient failure, lease stays recovering.
	out := svc.reconcileCrash(ctx)
	if out.Recovered != 0 || out.Lost != 0 {
		t.Fatalf("first pass summary = %+v, want no counts (recovering)", out)
	}
	if l.State != "running" || !l.live() {
		t.Fatalf("after 1 failure state = %q, want running (still live) and not lost", l.State)
	}
	if !l.LostAt.IsZero() {
		t.Fatal("a transient failure must not stamp the lease lost")
	}

	// Second pass: still recovering.
	if out := svc.reconcileCrash(ctx); out.Lost != 0 {
		t.Fatalf("second pass summary = %+v, want no loss", out)
	}
	if l.State != "running" {
		t.Fatalf("after 2 failures state = %q, want running", l.State)
	}

	// Third pass: the resume succeeds.
	out = svc.reconcileCrash(ctx)
	if out.Recovered != 1 || out.Lost != 0 {
		t.Fatalf("third pass summary = %+v, want one recovery and no loss", out)
	}
	if l.State != "recovered" || !l.live() {
		t.Fatalf("after success state = %q, want live recovered", l.State)
	}
	if attempts != 3 {
		t.Fatalf("resume attempts = %d, want 3", attempts)
	}
}

// TestRecoveryPermanentTransientGivesUp: a recovery that always fails
// transiently loses the lease once its attempt budget is spent, with the
// attempts named in the reason.
func TestRecoveryPermanentTransientGivesUp(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.cfg.RecoveryRetryAttempts = 3

	l := recoverTargetCheckpoint(t, svc, sub, ctx)
	sb := l.SandboxID
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume && req.SandboxID == sb {
			return substrate.Sandbox{}, errors.New("failed to init envd: syncing took too long")
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	esub := svc.Subscribe(EventFilter{LeaseID: l.ID})
	defer esub.Close()

	for i := 0; i < 3; i++ {
		out := svc.reconcileCrash(ctx)
		if i < 2 && out.Lost != 0 {
			t.Fatalf("pass %d summary = %+v, want no loss yet", i, out)
		}
	}
	if l.State != "lost" {
		t.Fatalf("state after 3 failures = %q, want lost", l.State)
	}
	if !strings.Contains(l.LostReason, "after 3 attempt(s)") {
		t.Fatalf("lost reason = %q, want it to name 3 attempts", l.LostReason)
	}
	if l.LostAt.IsZero() {
		t.Fatal("a permanently failing recovery must stamp the lease lost")
	}
	esub.Close()
	events := collectEvents(esub.C)
	var lost LeaseEvent
	for _, ev := range events {
		if ev.Type == LeaseLost {
			lost = ev
		}
	}
	if lost.Type != LeaseLost {
		t.Fatalf("no lost event in %v", eventTypes(events))
	}
	if !strings.Contains(lost.Detail, "after 3 attempt(s)") {
		t.Fatalf("lost event detail = %q, want the attempt count", lost.Detail)
	}
}

// TestRecoveryNonRetryableLostAtOnce: a missing checkpoint build is
// permanent, so the lease is lost on the first pass, not retried.
func TestRecoveryNonRetryableLostAtOnce(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.cfg.RecoveryRetryAttempts = 3

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	// A checkpoint build id that has no row: the recovery cannot load it.
	svc.store.mu.Lock()
	l.LastCheckpointBuildID = "b-does-not-exist"
	l.LastCheckpointAt = time.Now()
	svc.saveLeaseLocked(l)
	svc.store.mu.Unlock()
	sub.Fake.Kill(l.SandboxID)

	out := svc.reconcileCrash(ctx)
	if out.Lost != 1 || out.Recovered != 0 {
		t.Fatalf("summary = %+v, want one loss", out)
	}
	if l.State != "lost" {
		t.Fatalf("state = %q, want lost", l.State)
	}
	if !recoveryFailurePermanent(store.ErrNotFound) {
		t.Fatal("store.ErrNotFound must be classified permanent")
	}
}

// TestRecoveryCapacityRefusalWaits: an admission refusal keeps the lease
// recovering without counting an attempt, and does not lose it while the
// window lasts.
// TestRecoveryCapacityRefusalWaits: a substrate capacity refusal keeps
// the lease recovering without counting an attempt, so a small attempt
// budget never runs out while the node has no room.
func TestRecoveryCapacityRefusalWaits(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.cfg.RecoveryRetryAttempts = 2 // small attempt budget
	svc.cfg.RecoveryRetryWindow = time.Hour

	l := recoverTargetCheckpoint(t, svc, sub, ctx)
	sb := l.SandboxID
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume && req.SandboxID == sb {
			return substrate.Sandbox{}, substrate.ErrCapacity
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	// Many passes: a substrate capacity refusal is a wait, not a failure
	// count, so the small attempt budget never runs out.
	for i := 0; i < 5; i++ {
		out := svc.reconcileCrash(ctx)
		if out.Lost != 0 {
			t.Fatalf("pass %d lost a lease waiting for capacity: %+v", i, out)
		}
		if l.State != "running" {
			t.Fatalf("pass %d state = %q, want running (still recovering)", i, l.State)
		}
	}
	if l.LostReason != "" {
		t.Fatalf("a waiting lease carries a loss reason %q", l.LostReason)
	}
}

// TestRecoveryWindowBoundsCapacityWait: a capacity refusal that never
// resolves still gives up once the recovery window has passed, so no
// state waits for ever.
func TestRecoveryWindowBoundsCapacityWait(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.cfg.RecoveryRetryAttempts = 100
	svc.cfg.RecoveryRetryWindow = time.Minute

	now := time.Now()
	svc.now = func() time.Time { return now }

	l := recoverTargetCheckpoint(t, svc, sub, ctx)
	sb := l.SandboxID
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume && req.SandboxID == sb {
			return substrate.Sandbox{}, substrate.ErrCapacity
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	if out := svc.reconcileCrash(ctx); out.Lost != 0 || l.State != "running" {
		t.Fatalf("first pass = %+v state %q, want running (still recovering)", out, l.State)
	}
	// Advance beyond the window and retry: now it gives up.
	now = now.Add(2 * time.Minute)
	if out := svc.reconcileCrash(ctx); out.Lost != 1 {
		t.Fatalf("after the window summary = %+v, want one loss", out)
	}
	if l.State != "lost" {
		t.Fatalf("state after the window = %q, want lost", l.State)
	}
}

// TestRecoveryRetryBudgetClearedOnSuccess: a successful recovery drops
// the lease's retry budget, so a later failure starts fresh.
func TestRecoveryRetryBudgetClearedOnSuccess(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.cfg.RecoveryRetryAttempts = 2

	l := recoverTargetCheckpoint(t, svc, sub, ctx)
	sb := l.SandboxID
	var fail bool
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume && req.SandboxID == sb && fail {
			return substrate.Sandbox{}, errors.New("syncing took too long")
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	fail = true
	if out := svc.reconcileCrash(ctx); out.Lost != 0 {
		t.Fatalf("first failure lost the lease: %+v", out)
	}
	fail = false
	if out := svc.reconcileCrash(ctx); out.Recovered != 1 {
		t.Fatalf("successful recovery summary = %+v, want one recovery", out)
	}
	svc.retryMu.Lock()
	_, present := svc.recoveryRetries[sb]
	svc.retryMu.Unlock()
	if present {
		t.Fatal("the retry budget survived a successful recovery")
	}
}

// TestRecoveryBudgetKeyedBySandbox: a transient recovery failure then a
// cold restart gives the lease a new sandbox; the next reconcile must
// leave the healthy lease alone instead of rolling it back to the old
// checkpoint (spoond-dxq B2).
func TestRecoveryBudgetKeyedBySandbox(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.cfg.RecoveryRetryAttempts = 3

	l := recoverTargetCheckpoint(t, svc, sub, ctx)
	failedSandbox := l.SandboxID
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume && req.SandboxID == failedSandbox {
			return substrate.Sandbox{}, errors.New("syncing took too long")
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	// The first pass leaves a pending retry keyed by the failed sandbox.
	if out := svc.reconcileCrash(ctx); out.Lost != 0 {
		t.Fatalf("first failure lost the lease: %+v", out)
	}
	if !svc.recoveryPending(failedSandbox) {
		t.Fatal("no pending recovery retry keyed by the failed sandbox")
	}

	// A cold restart gives the lease a new sandbox (and clears the
	// budget). Reconcile must not touch the healthy lease.
	sub.createFn = nil
	if _, err := svc.restartCold(ctx, l.Owner, l); err != nil {
		t.Fatalf("cold restart: %v", err)
	}
	newSandbox := l.SandboxID
	if newSandbox == failedSandbox {
		t.Fatal("cold restart reused the failed sandbox id")
	}
	if svc.recoveryPending(newSandbox) {
		t.Fatal("a recovery budget survived the cold restart under the new sandbox")
	}
	before := l.Generation
	if out := svc.reconcileCrash(ctx); out.Recovered != 0 || out.Lost != 0 {
		t.Fatalf("reconcile after cold restart = %+v, want no action on the healthy lease", out)
	}
	if l.State != "running" || l.Generation != before {
		t.Fatalf("cold-restarted lease changed: state=%q generation=%d want running/%d", l.State, l.Generation, before)
	}
}

// TestRecoveryTransientEmitsRetryEvent: every transient recovery failure
// emits a recovery_retry event naming the attempt and the cause, and GET
// exposes the pending retry (spoond-dxq S3).
func TestRecoveryTransientEmitsRetryEvent(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.cfg.RecoveryRetryAttempts = 3

	l := recoverTargetCheckpoint(t, svc, sub, ctx)
	sb := l.SandboxID
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume && req.SandboxID == sb {
			return substrate.Sandbox{}, errors.New("syncing took too long")
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	esub := svc.Subscribe(EventFilter{LeaseID: l.ID})
	defer esub.Close()

	if out := svc.reconcileCrash(ctx); out.Lost != 0 {
		t.Fatalf("transient failure lost the lease: %+v", out)
	}

	// GET exposes the pending retry.
	detail := svc.leaseDetailMap(l)
	rec, ok := detail["recovery"].(map[string]any)
	if !ok {
		t.Fatalf("GET has no recovery object: %v", detail)
	}
	if rec["attempt"] != 1 || rec["of"] != 3 {
		t.Fatalf("recovery = %v, want attempt 1 of 3", rec)
	}
	if rec["since"] == "" || rec["since"] == nil {
		t.Fatalf("recovery since = %v, want a timestamp", rec["since"])
	}

	esub.Close()
	var retry LeaseEvent
	for _, ev := range collectEvents(esub.C) {
		if ev.Type == LeaseRetry {
			retry = ev
		}
	}
	if retry.Type != LeaseRetry {
		t.Fatal("no recovery_retry event on a transient recovery failure")
	}
	if !strings.Contains(retry.Detail, "attempt 1/3") || !strings.Contains(retry.Detail, "syncing took too long") {
		t.Fatalf("recovery_retry detail = %q, want the attempt and cause", retry.Detail)
	}
}

// TestRecoveryKeepsLeaseWhenReleased: a release that races a recovery
// loss must win: the released lease is not resurrected with a lost state
// and no lost event follows (spoond-775 class).
func TestRecoveryKeepsLeaseWhenReleased(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}

	esub := svc.Subscribe(EventFilter{LeaseID: l.ID})
	defer esub.Close()

	// Mark the lease released, then lose it as a recovery would.
	svc.releaseBecause(ctx, l, "deleted through the API")
	if out := svc.loseRecovery(ctx, l, "recovery failed", "boom"); out.Result != "lost" {
		t.Fatalf("loseRecovery result = %q, want lost", out.Result)
	}
	if l.State == "lost" {
		t.Fatal("a released lease was resurrected to lost")
	}
	if l.LostReason != "" || l.LostAt != (time.Time{}) {
		t.Fatalf("released lease carries loss state: reason=%q lostAt=%v", l.LostReason, l.LostAt)
	}
	if _, ok := svc.store.leases[l.ID]; ok {
		t.Fatal("the released lease row came back")
	}

	esub.Close()
	for _, ev := range collectEvents(esub.C) {
		if ev.Type == LeaseLost {
			t.Fatalf("a lost event followed a release: %v", ev)
		}
	}
}

// TestReleaseDropsRetryBudget: releasing a lease drops its recovery
// budget (spoond-dxq S2). The background preempt-resume budget it used to
// drop too is gone with that loop (#145 D2).
func TestReleaseDropsRetryBudgets(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l := recoverTargetCheckpoint(t, svc, sub, ctx)
	sb := l.SandboxID
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume && req.SandboxID == sb {
			return substrate.Sandbox{}, errors.New("syncing took too long")
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	if out := svc.reconcileCrash(ctx); out.Lost != 0 {
		t.Fatalf("first failure lost the lease: %+v", out)
	}
	if !svc.recoveryPending(sb) {
		t.Fatal("setup: expected a recovery budget present")
	}

	sub.createFn = nil
	svc.releaseBecause(ctx, l, "deleted through the API")

	if svc.retryPending(svc.recoveryRetries, sb) {
		t.Fatal("the recovery budget survived the release")
	}
}
