package api

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
)

// spoond-638d: a drain --start resume must not run before a restarted
// orchestrator is ready for sandbox creates, and an orchestrator that is
// not ready (Unavailable, a connection reset) must never lose a lease.
// Losing a lease on a planned restart is the worst outcome; the holder's
// next work call can always retry.

// shortUndrainReadiness shrinks the undrain's readiness wait and backoff
// so a test drives the not-ready path without production pauses.
func shortUndrainReadiness(svc *Service) {
	svc.cfg.UndrainReadyTimeout = 500 * time.Millisecond
	svc.undrainReadyPoll = 5 * time.Millisecond
	svc.undrainBackoffMin = time.Millisecond
	svc.undrainBackoffMax = 10 * time.Millisecond
	svc.cfg.UndrainResumeWindow = 2 * time.Second
}

// TestUndrainWaitsForOrchestratorReadyBeforeFirstResume: the undrain does
// not run the first resume until the orchestrator answers both NodeInfo
// and a List. A resume that ran early would use the same not-ready
// orchestrator and fail, the incident this guards against.
func TestUndrainWaitsForOrchestratorReadyBeforeFirstResume(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	shortUndrainReadiness(svc)
	svc.cfg.UndrainResumeRetries = 0

	leases := grantAndDrain(t, svc, 1)
	target := leases[0]

	// The node is not ready for the first few probes: NodeInfo succeeds
	// but List fails, exactly the window between the orchestrator process
	// listening and its sandbox service working. Once ready, both answer.
	var listCalls atomic.Int32
	sub.listFn = func(ctx context.Context) ([]substrate.Sandbox, error) {
		if listCalls.Add(1) <= 2 {
			return nil, errors.New("connection reset by peer")
		}
		return sub.Fake.List(ctx)
	}
	t.Cleanup(func() { sub.listFn = nil })

	var resumeCalls atomic.Int32
	sub.createFn = func(c context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume {
			// Every resume must run only after the node is ready.
			if listCalls.Load() <= 2 {
				resumeCalls.Add(1)
			}
		}
		return sub.Fake.Create(c, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	res := svc.undrain(ctx)
	if res.Resumed != 1 || len(res.Failed) != 0 {
		t.Fatalf("undrain = +%v, want 1 resumed and no failures", res)
	}
	if resumeCalls.Load() != 0 {
		t.Fatalf("%d resume(s) ran before the orchestrator was ready", resumeCalls.Load())
	}
	if target.State != "running" || target.Drained {
		t.Fatalf("lease = state=%s drained=%v, want running and undrained", target.State, target.Drained)
	}
}

// TestUndrainNotReadyRetriesBeyondEnvBudgetAndNeverLosesLease: an
// Unavailable/connection-reset resume failure is indeterminate — it says
// nothing about the sandbox — so it is retried with backoff for the
// window and never counts toward the envd attempt budget. With
// UNDRAIN_RESUME_RETRIES=0 a single not-ready failure would have ended
// the lease before; now several are retried and the resume succeeds.
func TestUndrainNotReadyRetriesBeyondEnvBudgetAndNeverLosesLease(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	shortUndrainReadiness(svc)
	svc.cfg.UndrainResumeRetries = 0
	// A window long enough for the handful of not-ready answers below.
	svc.cfg.UndrainResumeWindow = 30 * time.Second

	leases := grantAndDrain(t, svc, 1)
	target := leases[0]
	targetSandbox := target.SandboxID

	var attempts atomic.Int32
	sub.createFn = func(c context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume && req.SandboxID == targetSandbox {
			if attempts.Add(1) <= 4 {
				return substrate.Sandbox{}, errors.New("rpc error: code = Unavailable desc = error reading from server: connection reset by peer")
			}
		}
		return sub.Fake.Create(c, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	res := svc.undrain(ctx)
	if res.Resumed != 1 || len(res.Failed) != 0 {
		t.Fatalf("undrain = +%+v, want 1 resumed and no failures", res)
	}
	if got := attempts.Load(); got != 5 {
		t.Fatalf("resume attempts = %d, want 5 (four not-ready then success)", got)
	}
	if target.State != "running" || target.Drained {
		t.Fatalf("lease = state=%s drained=%v, want running and undrained", target.State, target.Drained)
	}
	if !target.LostAt.IsZero() {
		t.Fatal("a not-ready orchestrator must never stamp the lease lost")
	}
}

// TestUndrainNotReadyWindowExhaustedLeavesSuspended: a not-ready resume
// that stays failing past the window is not lost either: the lease is
// left suspended and drained (deferred) so the self-heal loop and the
// holder's next work call can retry, with its snapshot intact.
func TestUndrainNotReadyWindowExhaustedLeavesSuspended(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	shortUndrainReadiness(svc)
	svc.cfg.UndrainResumeRetries = 0
	svc.cfg.UndrainResumeWindow = 20 * time.Millisecond

	leases := grantAndDrain(t, svc, 1)
	target := leases[0]
	targetSandbox := target.SandboxID

	sub.createFn = func(c context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume && req.SandboxID == targetSandbox {
			return substrate.Sandbox{}, errors.New("rpc error: code = Unavailable desc = error reading from server: connection reset by peer")
		}
		return sub.Fake.Create(c, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	res := svc.undrain(ctx)
	if res.Resumed != 0 || len(res.Failed) != 1 {
		t.Fatalf("undrain = +%v, want 0 resumed and 1 failed", res)
	}
	if target.State == "lost" || target.LostAt != (time.Time{}) {
		t.Fatalf("lease = state=%s lostAt=%v, want not lost", target.State, target.LostAt)
	}
	if !target.Drained {
		t.Fatal("a window-exhausted not-ready resume must keep the lease drained for a later attempt")
	}
}

// TestUndrainPermanentErrorStillLosesLease: only a permanent error (the
// build the resume needs is gone) still loses the lease, because no retry
// can bring the snapshot back.
func TestUndrainPermanentErrorStillLosesLease(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	shortUndrainReadiness(svc)
	svc.cfg.UndrainResumeRetries = 2

	leases := grantAndDrain(t, svc, 1)
	target := leases[0]
	targetSandbox := target.SandboxID

	sub.createFn = func(c context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume && req.SandboxID == targetSandbox {
			return substrate.Sandbox{}, fmt.Errorf("load build b-missing: %w", store.ErrNotFound)
		}
		return sub.Fake.Create(c, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	res := svc.undrain(ctx)
	if res.Resumed != 0 || len(res.Failed) != 1 {
		t.Fatalf("undrain = +%v, want 0 resumed and 1 failed", res)
	}
	if target.State != "lost" {
		t.Fatalf("lease state = %s, want lost for a permanent error", target.State)
	}
	if target.SuspendReason == suspendReasonResumeFailed {
		t.Fatal("a permanent error must not be reported as resume_failed")
	}
}

// TestResumeOnUseAfterResumeFailed: a lease the undrain left suspended
// with reason resume_failed (a definitive but recoverable failure) comes
// back on the holder's next work call through the normal resume-on-use
// path, with the flag cleared and the suspension facts dropped
// (spoond-638d).
func TestResumeOnUseAfterResumeFailed(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	shortUndrainReadiness(svc)
	svc.cfg.UndrainResumeRetries = 0

	leases := grantAndDrain(t, svc, 1)
	target := leases[0]
	targetSandbox := target.SandboxID

	var refuse atomic.Bool
	refuse.Store(true)
	sub.createFn = func(c context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume && req.SandboxID == targetSandbox && refuse.Load() {
			return substrate.Sandbox{}, errors.New("create failed after the VM started")
		}
		return sub.Fake.Create(c, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	if res := svc.undrain(ctx); res.Resumed != 0 || len(res.Failed) != 1 {
		t.Fatalf("undrain = +%+v, want the resume deferred", res)
	}
	if target.SuspendReason != suspendReasonResumeFailed || !target.Drained {
		t.Fatalf("lease = reason=%q drained=%v, want resume_failed and drained", target.SuspendReason, target.Drained)
	}

	// The node is healthy again: the holder's next work call resumes it
	// through the normal path and clears the structured facts.
	refuse.Store(false)
	if _, err := svc.resumeForUse(ctx, target); err != nil {
		t.Fatalf("resume-on-use after resume_failed: %v", err)
	}
	if target.State != "running" || target.Drained || target.Suspended {
		t.Fatalf("lease = state=%s drained=%v suspended=%v, want running and undrained", target.State, target.Drained, target.Suspended)
	}
	if target.SuspendReason != "" {
		t.Fatalf("suspend_reason = %q, want it cleared by the resume", target.SuspendReason)
	}
}

// TestUndrainNotReadyClassification pins what counts as an indeterminate
// (never-lose) resume failure: a gRPC Unavailable code and the common
// transport text are; a permanent not-found and an envd start error are
// not. A permanent error is separated out even when its text carries a
// marker, so it still loses the lease.
func TestUndrainNotReadyClassification(t *testing.T) {
	notReady := []error{
		substrate.ErrUnavailable,
		fmt.Errorf("rpc error: code = Unavailable desc = error reading from server: connection reset by peer"),
		errors.New("connection reset by peer"),
		errors.New("unexpected EOF"),
		errors.New("transport is closing"),
	}
	for _, err := range notReady {
		if !undrainNotReady(err) {
			t.Errorf("undrainNotReady(%v) = false, want true", err)
		}
	}
	ready := []error{
		errors.New("failed to init envd: syncing took too long"),
		fmt.Errorf("load build b: %w", store.ErrNotFound),
		errQuotaExceeded,
	}
	for _, err := range ready {
		if undrainNotReady(err) {
			t.Errorf("undrainNotReady(%v) = true, want false", err)
		}
	}
	// A permanent error with a transport-looking message is still not
	// classified as not-ready: permanentNotFound is checked first in the
	// retry loop.
	if !permanentNotFound(fmt.Errorf("connection reset: %w", store.ErrNotFound)) {
		t.Fatal("a wrapped not-found lost its permanence")
	}
}

// TestUndrainReadyRequiresListNotJustNodeInfo: readiness is not met until
// a List succeeds too. An orchestrator that answers NodeInfo but refuses
// List has not accepted the sandbox service; resuming then would fail.
func TestUndrainReadyRequiresListNotJustNodeInfo(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	shortUndrainReadiness(svc)

	sub.listFn = func(ctx context.Context) ([]substrate.Sandbox, error) {
		return nil, errors.New("orchestrator not serving")
	}
	t.Cleanup(func() { sub.listFn = nil })

	if err := svc.undrainReady(context.Background()); err == nil {
		t.Fatal("undrainReady succeeded with a failing List")
	}
}

// TestUndrainReadyRequiresNodeInfo: a node that cannot even answer
// NodeInfo is not ready either, and a node that answers again becomes
// ready.
func TestUndrainReadyRequiresNodeInfo(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	shortUndrainReadiness(svc)

	sub.SetNodeInfo(substrate.NodeInfo{}, errors.New("node down"))
	if err := svc.undrainReady(context.Background()); err == nil {
		t.Fatal("undrainReady succeeded with a failing NodeInfo")
	}
	sub.SetNodeInfo(substrate.NodeInfo{Status: "healthy"}, nil)
	if err := svc.undrainReady(context.Background()); err != nil {
		t.Fatalf("undrainReady once the node answers: %v", err)
	}
}
