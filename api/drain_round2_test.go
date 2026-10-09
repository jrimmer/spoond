package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/substrate"
)

// Round 2 of spoond-638d: the layer-3 review found three gaps in the
// drain-resume outcome and refusal paths. Each test below kills the
// mutation it names.

// TestDrainResumeOutcomeAlreadyLostClearsDrainedQuietly (F1): a lease
// another path already lost makes resumeLease return *leaseLostError.
// drainResumeOutcome must not stamp resume_failed on it and keep it
// Drained (the heal loop would then retry a lease whose sandbox is gone
// for DRAIN_RESUME_MAX_AGE, emitting drain_deferred then drain_gave_up).
// It clears Drained quietly and reports the failure non-deferred.
// Mutation: run markResumeFailed without the lost/busy guard, which
// leaves drained=true reason=resume_failed on the lost lease.
func TestDrainResumeOutcomeAlreadyLostClearsDrainedQuietly(t *testing.T) {
	_, svc, _, _ := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	svc.cfg.UndrainResumeRetries = 0

	leases := grantAndDrain(t, svc, 1)
	target := leases[0]

	// Another path loses the lease while it still carries Drained (the
	// race the review describes): the resume then sees a lost lease.
	svc.store.mu.Lock()
	svc.markLost(target, "rootfs probe gave up")
	svc.store.mu.Unlock()
	if !target.Drained {
		t.Fatal("precondition: the lost lease must still carry Drained")
	}

	// A lost lease with Drained set is still an undrain target, so the
	// admin undrain runs the exact resume that returns *leaseLostError.
	res := svc.undrain(ctx)
	if len(res.Failed) != 1 {
		t.Fatalf("undrain = %+v, want one reported failure", res)
	}
	if target.Drained {
		t.Fatal("a lost lease must have Drained cleared quietly")
	}
	if target.SuspendReason == suspendReasonResumeFailed {
		t.Fatalf("suspend_reason = %q on a lost lease, want it untouched", target.SuspendReason)
	}
	if target.State != "lost" {
		t.Fatalf("lease state = %q, want lost", target.State)
	}
}

// TestDrainResumeOutcomeConcurrentResumeWinNotStamped (F1): a lease a
// concurrent resume brought back running while this resume failed must
// not get resume_failed stamped on it. Mutation: call markResumeFailed
// without the undrainLossAllowed guard, which stamps a running lease.
func TestDrainResumeOutcomeConcurrentResumeWinNotStamped(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	svc.cfg.UndrainResumeRetries = 0

	leases := grantAndDrain(t, svc, 1)
	target := leases[0]
	targetSandbox := target.SandboxID

	// The failing resume simulates another path winning the lease: it
	// flips it running before returning the error.
	sub.createFn = func(c context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume && req.SandboxID == targetSandbox {
			svc.store.mu.Lock()
			target.setState("running")
			target.Suspended = false
			svc.store.mu.Unlock()
			return substrate.Sandbox{}, errors.New("create failed after the VM started")
		}
		return sub.Fake.Create(c, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	svc.undrain(ctx)
	if target.SuspendReason == suspendReasonResumeFailed {
		t.Fatalf("suspend_reason = %q stamped on a running lease, want it untouched", target.SuspendReason)
	}
	if target.State != "running" {
		t.Fatalf("lease state = %q, want running", target.State)
	}
}

// TestUndrainNotReadyIgnoresTransportTextInGRPCStatus (F2): a gRPC
// status error carries its own code, so a transport marker inside an
// Internal error ("failed to init envd: ... connection refused") is an
// envd start failure, not a not-ready orchestrator: it must use the
// bounded attempt budget, not the whole UNDRAIN_RESUME_WINDOW. Mutation:
// match the text markers before the "rpc error: code = " check, so the
// Internal error is retried for the window.
func TestUndrainNotReadyIgnoresTransportTextInGRPCStatus(t *testing.T) {
	internal := fmt.Errorf("rpc error: code = Internal desc = failed to init envd: connection refused")
	if undrainNotReady(internal) {
		t.Fatalf("undrainNotReady(%v) = true; an Internal envd failure must use the retry budget", internal)
	}
	// The gRPC Unavailable code is still not-ready whatever its text.
	if !undrainNotReady(fmt.Errorf("rpc error: code = Unavailable desc = connection refused")) {
		t.Fatal("a gRPC Unavailable error must stay not-ready")
	}
	// A bare transport marker (no status prefix) stays not-ready too.
	if !undrainNotReady(errors.New("connection refused")) {
		t.Fatal("a bare connection-refused error must stay not-ready")
	}
}

// TestDrainResumeOutcomeInternalEnvdUsesRetryBudget (F2): an Internal
// gRPC error whose text carries "connection refused" must exhaust the
// envd attempt budget (1 + UNDRAIN_RESUME_RETRIES) and leave the lease
// suspended, rather than being retried for the long not-ready window.
// Mutation: treat the Internal error as not-ready, which runs the window
// instead of the two attempts.
func TestDrainResumeOutcomeInternalEnvdUsesRetryBudget(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	svc.cfg.UndrainResumeRetries = 1
	// A window toward the long production one, but short enough that the
	// mutation (running the window instead of the budget) reaches the
	// attempts assertion quickly rather than hanging for minutes.
	svc.cfg.UndrainResumeWindow = 2 * time.Second
	svc.undrainBackoffMin = time.Millisecond
	svc.undrainBackoffMax = 5 * time.Millisecond

	leases := grantAndDrain(t, svc, 1)
	target := leases[0]
	targetSandbox := target.SandboxID

	var attempts int
	sub.createFn = func(c context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume && req.SandboxID == targetSandbox {
			attempts++
			return substrate.Sandbox{}, fmt.Errorf("rpc error: code = Internal desc = failed to init envd: connection refused")
		}
		return sub.Fake.Create(c, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	res := svc.undrain(ctx)
	if len(res.Failed) != 1 || res.Failed[0].Attempts != 2 {
		t.Fatalf("undrain = %+v, want one failure with 2 attempts (the bounded budget)", res)
	}
	if attempts != 2 {
		t.Fatalf("resume attempts = %d, want 2 (the bounded budget, not the window)", attempts)
	}
	if target.SuspendReason != suspendReasonResumeFailed || !target.Drained {
		t.Fatalf("lease = reason=%q drained=%v, want resume_failed and drained", target.SuspendReason, target.Drained)
	}
}

// TestDrainResumeOutcomeReleasedBeforeDeferral (N1): a lease released
// while its resume ran reports errLeaseReleased, and that is checked
// before the deferral branch so a released lease never makes the heal
// emit drain_deferred, even when the caller's context is also cancelled.
// Mutation: move the errLeaseReleased check below undrainDeferred ||
// ctx.Err(), which returns deferred=true for the released lease.
func TestDrainResumeOutcomeReleasedBeforeDeferral(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	svc.cfg.UndrainResumeRetries = 0

	leases := grantAndDrain(t, svc, 1)
	svc.draining.Store(false) // the direct outcome call is the undrain's resume step
	sub.SetNodeInfo(substrate.NodeInfo{Status: "healthy", HugepagesTotal: 1 << 20, HugepageSizeBytes: 2 << 20}, nil)
	target := leases[0]
	targetSandbox := target.SandboxID

	ctx, cancel := context.WithCancel(context.Background())
	// The resume's Create releases the lease and cancels the caller's
	// context together, so the resume returns errLeaseReleased while
	// ctx.Err() is also set: exactly the case the old ordering folded
	// into the deferred branch.
	sub.createFn = func(c context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume && req.SandboxID == targetSandbox {
			svc.store.mu.Lock()
			target.released = true
			svc.store.mu.Unlock()
			cancel()
		}
		return sub.Fake.Create(c, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	err, _, deferred := svc.drainResumeOutcome(ctx, target, nil, nil)
	if !errors.Is(err, errLeaseReleased) {
		t.Fatalf("drainResumeOutcome = %v, want errLeaseReleased", err)
	}
	if deferred {
		t.Fatal("a released lease must not be reported deferred (the heal would emit drain_deferred)")
	}
}

// TestWriteResumeRefusalSubstrateUnavailable (N2): a resume refused
// because the substrate is unavailable answers the same retryable 503
// substrate_unavailable + Retry-After: 5 the other substrate-unknown
// paths give, not a generic 500 "resume failed". Mutation: drop the
// substrate.ErrUnavailable case, which falls to the default 500.
func TestWriteResumeRefusalSubstrateUnavailable(t *testing.T) {
	rec := httptest.NewRecorder()
	writeResumeRefusal(rec, log.New(io.Discard, "", 0), "lease-1", substrate.ErrUnavailable)
	if rec.Code != substrateUnknownStatusCode {
		t.Fatalf("status = %d, want %d", rec.Code, substrateUnknownStatusCode)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "5" {
		t.Fatalf("Retry-After = %q, want 5", ra)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"code":"substrate_unavailable"`) {
		t.Fatalf("body = %s, want code substrate_unavailable", body)
	}
	if !strings.Contains(body, `"error"`) {
		t.Fatalf("body = %s, want an error message", body)
	}
}

// TestMarkResumeFailedEmitsSuspendedEventAndJournal (N3): leaving a
// lease suspended with reason resume_failed emits the same `suspended`
// event (reason resume_failed) every other automatic suspension carries
// and writes the puqp journal line, so the documented contract holds.
// Mutation: drop the emitSuspendEvent/journalLease calls from
// markResumeFailed.
func TestMarkResumeFailedEmitsSuspendedEventAndJournal(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.cfg.UndrainResumeRetries = 0

	var buf strings.Builder
	svc.log = log.New(&buf, "", 0)
	all := svc.Subscribe(EventFilter{})

	leases := grantAndDrain(t, svc, 1)
	target := leases[0]
	targetSandbox := target.SandboxID

	sub.createFn = func(c context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume && req.SandboxID == targetSandbox {
			return substrate.Sandbox{}, errors.New("failed to init envd: syncing took too long")
		}
		return sub.Fake.Create(c, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	svc.undrain(ctx)
	if target.SuspendReason != suspendReasonResumeFailed {
		t.Fatalf("suspend_reason = %q, want resume_failed", target.SuspendReason)
	}
	all.Close()

	var suspended *LeaseEvent
	events := eventsFor(collectEvents(all.C), target.ID)
	for _, ev := range events {
		if ev.Type == LeaseSuspended && ev.Reason == suspendReasonResumeFailed {
			e := ev
			suspended = &e
		}
	}
	if suspended == nil {
		t.Fatalf("no suspended event with reason resume_failed: %v", eventTypes(events))
	}
	if suspended.BuildID == "" {
		t.Fatal("the resume_failed suspended event carries no build_id")
	}
	// The journal line names the same reason.
	var found bool
	for _, line := range strings.Split(buf.String(), "\n") {
		if !strings.Contains(line, "lease journal:") {
			continue
		}
		f := journalFields(t, strings.TrimSpace(line))
		if f["op"] == journalOpSuspend && f["lease_id"] == target.ID && f["reason"] == suspendReasonResumeFailed {
			found = true
		}
	}
	if !found {
		t.Fatalf("no suspend journal line with reason resume_failed:\n%s", buf.String())
	}
}
