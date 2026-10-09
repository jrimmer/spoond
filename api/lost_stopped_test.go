package api

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
)

// spoond-63a: every transition to lost stops the guest. These tests drive
// each lost path with a create or resume that leaves a sandbox behind and
// assert the fake holds none afterwards, plus a delete that fails once is
// retried.

// failCreateAfterStart replaces Create so the first call for sandboxID
// really creates the VM and then reports the failure a create can hit
// after the VM started. A recovery must delete that half-started guest.
func failCreateAfterStart(t *testing.T, sub *testSub, sandboxID string) {
	t.Helper()
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.SandboxID == sandboxID {
			if _, err := sub.Fake.Create(ctx, req); err != nil {
				return substrate.Sandbox{}, err
			}
			return substrate.Sandbox{}, errors.New("create failed after the VM started")
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })
}

// failCreateAfterStartPermanent is failCreateAfterStart but reports a
// permanent error (the build is gone), the only resume failure that still
// loses a lease (spoond-638d).
func failCreateAfterStartPermanent(t *testing.T, sub *testSub, sandboxID string) {
	t.Helper()
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.SandboxID == sandboxID {
			if _, err := sub.Fake.Create(ctx, req); err != nil {
				return substrate.Sandbox{}, err
			}
			return substrate.Sandbox{}, fmt.Errorf("load build b-missing: %w", store.ErrNotFound)
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })
}

// TestReconcileLostDeletesHalfStartedSandbox: a crash recovery whose
// create fails after the VM started marks the lease lost and stops the
// half-started sandbox.
func TestReconcileLostDeletesHalfStartedSandbox(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	// One attempt: the failed create spends the recovery budget
	// (spoond-dxq), so the lease goes lost through loseRecovery — the
	// budget-spent path that must still stop the half-started guest.
	svc.cfg.RecoveryRetryAttempts = 1

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.checkpointLease(ctx, l); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	// The crash: the sandbox vanishes without a Delete.
	sub.Fake.Kill(l.SandboxID)
	// The recovery create starts a VM under the same id, then fails.
	failCreateAfterStart(t, sub, l.SandboxID)

	summary := svc.reconcileCrash(ctx)
	if summary.Lost != 1 || summary.Recovered != 0 {
		t.Fatalf("summary = %+v, want one loss", summary)
	}
	if l.State != "lost" {
		t.Fatalf("lease state = %q, want lost", l.State)
	}
	if sandboxOnFake(t, sub, l.SandboxID) {
		t.Fatalf("the half-started sandbox %s still runs after the lease went lost", l.SandboxID)
	}
}

// TestUndrainLostDeletesHalfStartedSandbox: an undrain resume that fails
// after starting its VM with a permanent error (the build is gone) loses
// the lease and the half-started sandbox is stopped.
func TestUndrainLostDeletesHalfStartedSandbox(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	// A single attempt: the first failure is the final one.
	svc.cfg.UndrainResumeRetries = 0

	leases := grantAndDrain(t, svc, 1)
	target := leases[0]
	sandbox := target.SandboxID
	failCreateAfterStartPermanent(t, sub, sandbox)

	res := svc.undrain(ctx)
	if res.Resumed != 0 || len(res.Failed) != 1 {
		t.Fatalf("undrain = %+v, want one failure", res)
	}
	if target.State != "lost" {
		t.Fatalf("lease state = %q, want lost", target.State)
	}
	if sandboxOnFake(t, sub, sandbox) {
		t.Fatalf("the half-started sandbox %s still runs after the lease went lost", sandbox)
	}
}

// TestUndrainHalfStartedSandboxStoppedWhenSuspended: an undrain resume
// that fails after starting its VM with a recoverable error leaves the
// lease suspended with reason resume_failed; the half-started sandbox is
// still stopped so the next resume-on-use can reuse the id (spoond-638d).
func TestUndrainHalfStartedSandboxStoppedWhenSuspended(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	svc.cfg.UndrainResumeRetries = 0

	leases := grantAndDrain(t, svc, 1)
	target := leases[0]
	sandbox := target.SandboxID
	failCreateAfterStart(t, sub, sandbox)

	res := svc.undrain(ctx)
	if res.Resumed != 0 || len(res.Failed) != 1 {
		t.Fatalf("undrain = +%v, want one failure", res)
	}
	if target.State != "suspended" || !target.Drained {
		t.Fatalf("lease state = %q drained = %v, want suspended and drained", target.State, target.Drained)
	}
	if target.SuspendReason != suspendReasonResumeFailed {
		t.Fatalf("suspend_reason = %q, want %q", target.SuspendReason, suspendReasonResumeFailed)
	}
	if sandboxOnFake(t, sub, sandbox) {
		t.Fatalf("the half-started sandbox %s still runs after the resume was deferred", sandbox)
	}
}

// TestLostSandboxDeleteRetried: a substrate Delete that fails once is
// retried, so the lost lease's sandbox is still stopped.
func TestLostSandboxDeleteRetried(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	// One attempt: the failed create spends the recovery budget
	// (spoond-dxq), so the lease goes lost through loseRecovery — the
	// budget-spent path that must still stop the half-started guest.
	svc.cfg.RecoveryRetryAttempts = 1
	// Shrink the retry pause; keep the default attempt count.
	svc.lostSandboxDeleteBackoff = time.Millisecond

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.checkpointLease(ctx, l); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	sub.Fake.Kill(l.SandboxID)
	failCreateAfterStart(t, sub, l.SandboxID)
	// The first Delete (the lost transition's) fails; the retry succeeds.
	sub.FailCall("Delete", 1, errors.New("transient delete failure"))

	if summary := svc.reconcileCrash(ctx); summary.Lost != 1 {
		t.Fatalf("summary = %+v, want one loss", summary)
	}
	if got := calls(sub.Fake, "Delete "+l.SandboxID); got < 2 {
		t.Fatalf("Delete calls = %d, want the failed attempt and its retry", got)
	}
	if sandboxOnFake(t, sub, l.SandboxID) {
		t.Fatalf("the sandbox %s survived a delete that failed once", l.SandboxID)
	}
}

// TestRootfsProbeLostRetriesDelete: the rootfs probe's pre-recovery
// delete is best effort; when it fails the lost transition retries and
// stops the dead sandbox.
func TestRootfsProbeLostRetriesDelete(t *testing.T) {
	svc, sub, l := newRootfsProbeService(t, false)
	ctx := context.Background()
	svc.lostSandboxDeleteBackoff = time.Millisecond
	sandbox := l.SandboxID
	sub.rootfsFail[sandbox] = "dd: error reading '/dev/vda': Input/output error"
	// The pre-recovery delete fails once; the lost transition retries.
	sub.FailCall("Delete", 1, errors.New("transient delete failure"))

	for i := 0; i < rootfsProbeFailuresThreshold; i++ {
		svc.probeRootfsLeases(ctx)
	}
	if l.State != "lost" {
		t.Fatalf("lease state = %q, want lost", l.State)
	}
	if sandboxOnFake(t, sub, sandbox) {
		t.Fatalf("the dead sandbox %s survived a pre-delete that failed once", sandbox)
	}
}

// TestOrphanSweepStopsLostSandbox: a lost path's bounded delete can give
// up; the periodic orphan sweep then treats the lost lease's sandbox as
// an orphan and stops it.
func TestOrphanSweepStopsLostSandbox(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	// One attempt: the failed create spends the recovery budget
	// (spoond-dxq), so the lease goes lost through loseRecovery — the
	// budget-spent path that must still stop the half-started guest.
	svc.cfg.RecoveryRetryAttempts = 1
	// Shrink the retry pause and give up after one attempt, as a
	// substrate that is down would force.
	svc.lostSandboxDeleteBackoff = time.Millisecond
	svc.lostSandboxDeleteAttempts = 1

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.checkpointLease(ctx, l); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	sub.Fake.Kill(l.SandboxID)
	failCreateAfterStart(t, sub, l.SandboxID)
	// Every attempt of the lost transition's delete fails. The failed
	// resume's own cleanup runs first (spoond-52c), then the lost path's
	// bounded delete; both must fail so the sandbox is still running for
	// the sweep's later delete (a fresh call) to find.
	sub.FailCall("Delete", 1, errors.New("substrate unreachable"))
	sub.FailCall("Delete", 2, errors.New("substrate unreachable"))

	if summary := svc.reconcileCrash(ctx); summary.Lost != 1 {
		t.Fatalf("summary = %+v, want one loss", summary)
	}
	if !sandboxOnFake(t, sub, l.SandboxID) {
		t.Fatal("precondition: the half-started sandbox should still run after the bounded delete gave up")
	}

	svc.sweepOrphanSandboxes(ctx)
	if sandboxOnFake(t, sub, l.SandboxID) {
		t.Fatalf("the orphan sweep left the lost lease's sandbox %s running", l.SandboxID)
	}
}

// TestMarkLostDoesNotResurrectReleased: a lease released while a loss
// is being recorded is not resurrected and its already-stopped sandbox
// is not deleted a second time.
func TestMarkLostDoesNotResurrectReleased(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.release(ctx, l)
	deletesBefore := calls(sub.Fake, "Delete")

	svc.store.mu.Lock()
	svc.markLost(l, "lost after release")
	svc.store.mu.Unlock()

	if l.State == "lost" {
		t.Fatalf("a released lease was marked lost")
	}
	if _, ok := svc.store.leases[l.ID]; ok {
		t.Fatalf("a released lease was resurrected")
	}
	if got := calls(sub.Fake, "Delete"); got != deletesBefore {
		t.Fatalf("Delete calls rose from %d to %d for an already-released lease", deletesBefore, got)
	}
}

// TestOrphanSweepRetriesRememberedSandbox: a release whose substrate
// Delete failed records the sandbox id; the periodic sweep retries it
// even though the lease row is gone.
func TestOrphanSweepRetriesRememberedSandbox(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	sandbox := l.SandboxID
	// The release's delete fails; the id is remembered for the sweep.
	sub.FailCall("Delete", 1, errors.New("substrate unreachable"))
	svc.release(ctx, l)
	if !sandboxOnFake(t, sub, sandbox) {
		t.Fatal("precondition: the released lease's sandbox should still run")
	}

	svc.sweepOrphanSandboxes(ctx)
	if sandboxOnFake(t, sub, sandbox) {
		t.Fatalf("the orphan sweep did not retry the released lease's remembered sandbox %s", sandbox)
	}
	// The id is dropped once it is gone.
	if ids := svc.orphanSandboxSnapshot(); len(ids) != 0 {
		t.Fatalf("remembered orphan ids = %v after a successful sweep, want none", ids)
	}
}

// TestOrphanSweepLeavesBusyLostLease: a lost lease with a recovery in
// flight (busy) is not swept: the sandbox its recovery just created must
// not be deleted out from under it.
func TestOrphanSweepLeavesBusyLostLease(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.store.mu.Lock()
	l.setState("lost")
	l.busy = true
	svc.saveLeaseLocked(l)
	svc.store.mu.Unlock()
	t.Cleanup(func() {
		svc.store.mu.Lock()
		l.busy = false
		svc.store.mu.Unlock()
	})

	svc.sweepOrphanSandboxes(ctx)
	if !sandboxOnFake(t, sub, l.SandboxID) {
		t.Fatalf("the orphan sweep deleted a busy lost lease's sandbox %s", l.SandboxID)
	}
}

// TestOrphanSweepLeavesLiveLease: the periodic orphan sweep never touches
// a live lease's sandbox.
func TestOrphanSweepLeavesLiveLease(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.sweepOrphanSandboxes(ctx)
	if !sandboxOnFake(t, sub, l.SandboxID) {
		t.Fatalf("the orphan sweep deleted a live lease's sandbox %s", l.SandboxID)
	}
}

// TestReconcileOrphansTwoPass: startup reconciliation deletes a foreign
// sandbox in the first pass and a lost lease's leftover sandbox in the
// second, while leaving a live lease alone.
func TestReconcileOrphansTwoPass(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	// A foreign sandbox from a previous incarnation.
	foreign, err := sub.Create(ctx, substrate.CreateRequest{SandboxID: "foreign-1"})
	if err != nil {
		t.Fatalf("foreign create: %v", err)
	}
	// A lost lease whose sandbox still runs (a lost path's delete failed).
	lost, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant lost: %v", err)
	}
	svc.store.mu.Lock()
	lost.setState("lost")
	svc.saveLeaseLocked(lost)
	svc.store.mu.Unlock()
	// A live lease that must survive both passes.
	live, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant live: %v", err)
	}

	svc.ReconcileOrphans(ctx)

	if sandboxOnFake(t, sub, foreign.ID) {
		t.Fatalf("the foreign sandbox %s survived the first pass", foreign.ID)
	}
	if sandboxOnFake(t, sub, lost.SandboxID) {
		t.Fatalf("the lost lease's sandbox %s survived the second pass", lost.SandboxID)
	}
	if !sandboxOnFake(t, sub, live.SandboxID) {
		t.Fatalf("the live lease's sandbox %s was swept", live.SandboxID)
	}
}
