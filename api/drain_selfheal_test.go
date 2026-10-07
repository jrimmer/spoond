package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/substrate"
)

// TestUndrainDetachedContextIsUsed pins H1: the admin handlers run on a
// context detached from the request, so a client that goes away does not
// cancel the drain or undrain half-way.
func TestUndrainDetachedContextIsUsed(t *testing.T) {
	_, svc, _, _ := newAdminServer(t, "admin-tok")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client hung up before the handler did anything

	l, err := svc.grant(context.Background(), "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	dctx, dcancel := adminContext(ctx, adminDrainTimeout)
	defer dcancel()
	res, err := svc.drain(dctx)
	if err != nil {
		t.Fatalf("drain with a cancelled caller: %v", err)
	}
	if res.Paused != 1 || l.State != "suspended" {
		t.Fatalf("drain = +%v, lease = %+v; want 1 paused and suspended", res, l)
	}

	uctx, ucancel := adminContext(ctx, adminUndrainTimeout)
	defer ucancel()
	ures := svc.undrain(uctx)
	if ures.Resumed != 1 || l.State != "running" {
		t.Fatalf("undrain with a cancelled caller = +%v, lease = %+v; want 1 resumed and running", ures, l)
	}
}

// TestDrainPausesSurviveCancelledCaller is the H1 regression: a drain
// whose caller's context is cancelled part-way must not abandon the
// remaining pauses and leave a lease running into the stop. The drain
// runs on a detached context, so the rest still pause.
func TestDrainPausesSurviveCancelledCaller(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	base := context.Background()

	leases := make([]*Lease, 0, 3)
	for range 3 {
		l, err := svc.grant(base, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
		if err != nil {
			t.Fatalf("grant: %v", err)
		}
		leases = append(leases, l)
	}

	// Cancel the caller's context as the first pause lands. The drain's
	// own context is detached, so the rest must still pause.
	callerCtx, cancel := context.WithCancel(base)
	var paused atomic.Int32
	sub.pauseFn = func(ctx context.Context, sandboxID, templateID string) (string, substrate.BuildRefs, error) {
		if paused.Add(1) == 1 {
			cancel()
		}
		return sub.Fake.Pause(ctx, sandboxID, templateID)
	}
	t.Cleanup(func() { sub.pauseFn = nil })

	dctx, dcancel := adminContext(callerCtx, adminDrainTimeout)
	defer dcancel()
	res, err := svc.drain(dctx)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.Paused != 3 || len(res.Failed) != 0 {
		t.Fatalf("drain = +%v, want 3 paused and no failures despite the cancelled caller", res)
	}
	for _, l := range leases {
		if !l.Drained || l.State != "suspended" {
			t.Fatalf("lease %s = state=%s drained=%v, want suspended and drained", l.ID, l.State, l.Drained)
		}
	}
}

// TestUndrainContextErrorsKeepDrained is the H1 regression: a resume
// that fails with a context error keeps the lease Drained for a later
// attempt instead of marking it lost.
func TestUndrainContextErrorsKeepDrained(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	svc.cfg.UndrainResumeRetries = 0

	leases := grantAndDrain(t, svc, 1)
	target := leases[0]

	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume {
			return substrate.Sandbox{}, context.Canceled
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	res := svc.undrain(ctx)
	if res.Resumed != 0 || len(res.Failed) != 1 {
		t.Fatalf("undrain = +%v, want 0 resumed and 1 failed", res)
	}
	if target.State == "lost" || !target.Drained {
		t.Fatalf("lease = state=%s drained=%v, want still drained (not lost)", target.State, target.Drained)
	}
}

// TestUndrainCapacityKeepsDrained pins that a capacity refusal from the
// substrate keeps the lease Drained for retry.
func TestUndrainCapacityKeepsDrained(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	svc.cfg.UndrainResumeRetries = 0

	leases := grantAndDrain(t, svc, 1)
	target := leases[0]

	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume {
			return substrate.Sandbox{}, substrate.ErrCapacity
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	res := svc.undrain(ctx)
	if res.Resumed != 0 || len(res.Failed) != 1 {
		t.Fatalf("undrain = +%v, want 0 resumed and 1 failed", res)
	}
	if target.State == "lost" || !target.Drained {
		t.Fatalf("lease = state=%s drained=%v, want still drained", target.State, target.Drained)
	}
}

// TestDrainDeferredEvent: a deferred undrain resume emits drain_deferred
// naming the lease, so the owner can see why the lease stayed drained.
func TestDrainDeferredEvent(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	svc.cfg.UndrainResumeRetries = 0

	leases := grantAndDrain(t, svc, 1)
	target := leases[0]

	events := svc.Subscribe(EventFilter{LeaseID: target.ID})
	defer events.Close()

	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume {
			return substrate.Sandbox{}, errQuotaExceeded
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	svc.undrain(ctx)
	select {
	case ev := <-events.C:
		if ev.Type != LeaseDrainDeferred {
			t.Fatalf("event = %s, want drain_deferred", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no drain_deferred event was emitted")
	}
}

// TestSelfHealResumesDrainedLeases is H2: the drain self-heal pass
// resumes a lease left Drained by a failed undrain, with no admin call.
func TestSelfHealResumesDrainedLeases(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	svc.cfg.UndrainResumeRetries = 0
	// Pin the limit off so the pass only does the Drained recovery (the
	// stale-drain branch has its own test).
	svc.cfg.DrainMaxSecs = -1

	leases := grantAndDrain(t, svc, 2)
	target := leases[0]

	var refuse atomic.Bool
	refuse.Store(true)
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume && req.SandboxID == target.SandboxID && refuse.Load() {
			return substrate.Sandbox{}, errQuotaExceeded
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	res := svc.undrain(ctx)
	if res.Resumed != 1 || len(res.Failed) != 1 {
		t.Fatalf("first undrain = +%v, want 1 resumed and 1 deferred", res)
	}
	if !target.Drained {
		t.Fatal("the refused lease must stay drained")
	}

	refuse.Store(false)
	svc.healDrain(ctx)
	if target.Drained || target.State != "running" {
		t.Fatalf("after self-heal: state=%s drained=%v, want running and undrained", target.State, target.Drained)
	}
	if svc.draining.Load() {
		t.Fatal("self-heal left the service draining")
	}
}

// TestSelfHealUndrainsStaleDrain is H3: a drain older than DRAIN_MAX_SECS
// on a healthy node undrains itself, logs and emits an event.
func TestSelfHealUndrainsStaleDrain(t *testing.T) {
	_, svc, _, _ := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	svc.cfg.DrainMaxSecs = 1

	leases := grantAndDrain(t, svc, 2)
	if !svc.draining.Load() {
		t.Fatal("drain did not set draining")
	}

	events := svc.Subscribe(EventFilter{})
	defer events.Close()

	svc.drainStartedAt.Store(svc.now().Add(-2 * time.Second).UnixNano())
	svc.healDrain(ctx)

	if svc.draining.Load() {
		t.Fatal("stale drain was not cleared")
	}
	for _, l := range leases {
		if l.Drained || l.State != "running" {
			t.Fatalf("lease %s = state=%s drained=%v, want running and undrained", l.ID, l.State, l.Drained)
		}
	}
	found := false
	for {
		select {
		case ev := <-events.C:
			if ev.Type == LeaseDrainHealed {
				found = true
			}
			continue
		default:
		}
		break
	}
	if !found {
		t.Fatal("no drain_healed event was emitted")
	}
}

// TestSelfHealLeavesDrainWhileNodeUnhealthy: a stale drain on an
// unhealthy node is not lifted — the node may be coming back.
func TestSelfHealLeavesDrainWhileNodeUnhealthy(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	svc.cfg.DrainMaxSecs = 1

	grantAndDrain(t, svc, 1)
	svc.drainStartedAt.Store(svc.now().Add(-10 * time.Second).UnixNano())
	sub.SetNodeInfo(substrate.NodeInfo{Status: "unhealthy"}, nil)

	svc.healDrain(ctx)
	if !svc.draining.Load() {
		t.Fatal("an unhealthy node's drain must not be lifted by self-heal")
	}
}

// TestResumeOwnCallClearsDrained is L4: an owner's own resume clears the
// Drained flag, so resume-on-next-call keeps working and a later undrain
// cannot resume a lease the owner is running. The scenario is a lease
// left Drained (a deferred undrain, or a drain that predates a backend
// restart) that the owner resumes directly.
func TestResumeOwnCallClearsDrained(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !l.Drained {
		t.Fatal("drain did not mark the lease drained")
	}
	// A backend restart between drain and undrain: the node reports
	// healthy again, the lease row keeps Drained.
	if err := sub.SetDraining(ctx, false); err != nil {
		t.Fatalf("clear node draining: %v", err)
	}
	svc.draining.Store(false)
	if _, err := svc.resume(ctx, "c", l.ID); err != nil {
		t.Fatalf("owner resume: %v", err)
	}
	if l.Drained {
		t.Fatal("an owner's own resume must clear Drained")
	}
	if !l.live() {
		t.Fatalf("lease = %+v, want running", l)
	}
}

// TestHealthzReportsDraining: /healthz exposes the drain state so a
// monitor can tell a draining node from a healthy one.
func TestHealthzReportsDraining(t *testing.T) {
	ts, svc, _, _ := newAdminServer(t, "admin-tok")
	ctx := context.Background()

	resp, body := doReq(t, "POST", ts.URL+"/api/admin/drain", "admin-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("drain = %d (%v), want 200", resp.StatusCode, body)
	}
	if !svc.draining.Load() {
		t.Fatal("drain did not set draining")
	}

	hresp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	raw := readAll(t, hresp)
	if hresp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d (%s), want 200", hresp.StatusCode, raw)
	}
	if !strings.Contains(raw, `"draining":true`) {
		t.Fatalf("healthz body = %s, want draining true", raw)
	}

	svc.undrain(ctx)
	hresp, err = http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz after undrain: %v", err)
	}
	raw = readAll(t, hresp)
	if !strings.Contains(raw, `"draining":false`) {
		t.Fatalf("healthz body after undrain = %s, want draining false", raw)
	}
}

// TestReadyzReportsDraining: /readyz lists a draining check that never
// fails readiness (the create route's 503 draining is the refusal), but
// names the state.
func TestReadyzReportsDraining(t *testing.T) {
	_, svc, _ := newReadyzServer(t)

	find := func(r readyzResult) readyCheck {
		for _, c := range r.Checks {
			if c.Name == "draining" {
				return c
			}
		}
		t.Fatalf("no draining check in %+v", r.Checks)
		return readyCheck{}
	}
	if c := find(svc.runReadyz()); c.Detail != "off" {
		t.Fatalf("draining detail = %q, want off", c.Detail)
	}
	svc.draining.Store(true)
	defer svc.draining.Store(false)

	r := svc.runReadyz()
	if r.Status != "ok" {
		t.Fatalf("status = %q, want ok while only draining: %+v", r.Status, r.Checks)
	}
	if c := find(r); !c.OK || c.Detail == "off" {
		t.Fatalf("draining check = %+v, want ok and a draining detail", c)
	}
}

// TestFailedUndrainClearRetriesUntilItSucceeds is spoond-52c R1: a
// SetDraining(false) that fails during an undrain keeps spoond's own
// draining state, and the self-heal loop retries the clear with backoff
// even though no drained lease remains, emitting an event when it
// finally clears.
func TestFailedUndrainClearRetriesUntilItSucceeds(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()

	var clearedFailures atomic.Int32
	clearedFailures.Store(1)
	sub.setDrainingFn = func(ctx context.Context, draining bool) error {
		if !draining && clearedFailures.Add(-1) >= 0 {
			return errors.New("orchestrator unreachable")
		}
		return sub.Fake.SetDraining(ctx, draining)
	}
	t.Cleanup(func() { sub.setDrainingFn = nil })

	svc.cfg.UndrainResumeRetries = 0
	leases := grantAndDrain(t, svc, 1)

	events := svc.Subscribe(EventFilter{})
	defer events.Close()

	// The undrain's clear fails; the lease stays drained and the pending
	// clear is recorded.
	ures := svc.undrain(ctx)
	if ures.Resumed != 0 {
		t.Fatalf("undrain = +%v, want nothing resumed while the clear fails", ures)
	}
	if !svc.draining.Load() || !svc.drainClearPending.Load() {
		t.Fatalf("after a failed undrain clear: draining=%v pending=%v, want both true", svc.draining.Load(), svc.drainClearPending.Load())
	}

	// Clear the only lease's Drained flag by hand, so the pending clear
	// is the only thing left to heal (R1: it must retry with no drained
	// lease remaining). The fake then succeeds on the heal pass.
	leases[0].Drained = false

	svc.healDrain(ctx)
	if svc.draining.Load() || svc.drainClearPending.Load() {
		t.Fatalf("after the successful retry: draining=%v pending=%v, want both false", svc.draining.Load(), svc.drainClearPending.Load())
	}
	found := false
	for {
		select {
		case ev := <-events.C:
			if ev.Type == LeaseDrainHealed {
				found = true
			}
			continue
		default:
		}
		break
	}
	if !found {
		t.Fatal("no drain_healed event was emitted when the clear finally succeeded")
	}
}

// TestUndrainKeepsDrainWhenClearFails pins the undrain half of R1: a
// failed SetDraining(false) leaves the node draining and the drained
// leases untouched, so the self-heal loop can retry.
func TestUndrainKeepsDrainWhenClearFails(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	svc.cfg.UndrainResumeRetries = 0

	grantAndDrain(t, svc, 1)
	sub.setDrainingFn = func(ctx context.Context, draining bool) error {
		if !draining {
			return errors.New("orchestrator unreachable")
		}
		return sub.Fake.SetDraining(ctx, draining)
	}
	t.Cleanup(func() { sub.setDrainingFn = nil })

	res := svc.undrain(ctx)
	if res.Resumed != 0 {
		t.Fatalf("undrain = +%v, want nothing resumed while the node stays draining", res)
	}
	if !svc.draining.Load() || !svc.drainClearPending.Load() {
		t.Fatalf("draining=%v pending=%v, want both true after a failed clear", svc.draining.Load(), svc.drainClearPending.Load())
	}
}

// TestDrainFailureIsLoggedAndEmitted is spoond-52c R2: a lease a drain
// cannot pause is named in the response, logged, and emitted as a
// drain_failed owner event, so a lease left running into an orchestrator
// stop is visible outside the HTTP response.
func TestDrainFailureIsLoggedAndEmitted(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()

	leases := make([]*Lease, 0, 2)
	for range 2 {
		l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
		if err != nil {
			t.Fatalf("grant: %v", err)
		}
		leases = append(leases, l)
	}
	target := leases[0]

	events := svc.Subscribe(EventFilter{LeaseID: target.ID})
	defer events.Close()

	sub.pauseFn = func(ctx context.Context, sandboxID, templateID string) (string, substrate.BuildRefs, error) {
		if sandboxID == target.SandboxID {
			return "", substrate.BuildRefs{}, errors.New("snapshot write failed")
		}
		return sub.Fake.Pause(ctx, sandboxID, templateID)
	}
	t.Cleanup(func() { sub.pauseFn = nil })

	res, err := svc.drain(ctx)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.Paused != 1 || len(res.Failed) != 1 || res.Failed[0].ID != target.ID {
		t.Fatalf("drain = +%v, want 1 paused and 1 failure for %s", res, target.ID)
	}
	if target.Drained {
		t.Fatal("a lease whose pause failed must not be marked drained")
	}
	select {
	case ev := <-events.C:
		if ev.Type != LeaseDrainFailed {
			t.Fatalf("event = %s, want drain_failed", ev.Type)
		}
		if !strings.Contains(ev.Detail, "snapshot write failed") {
			t.Fatalf("event detail = %q, want the pause error", ev.Detail)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no drain_failed event was emitted")
	}
}

// TestAdminHandlersContinueOnCancelledRequest is spoond-52c R3: the
// handler wiring, not just svc.drain, runs on the detached context. A
// request cancelled as the first pause lands still drains every lease,
// and an already-cancelled request still undrains them.
func TestAdminHandlersContinueOnCancelledRequest(t *testing.T) {
	ts, svc, _, sub := newAdminServer(t, "admin-tok")
	base := context.Background()

	leases := make([]*Lease, 0, 3)
	for range 3 {
		l, err := svc.grant(base, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
		if err != nil {
			t.Fatalf("grant: %v", err)
		}
		leases = append(leases, l)
	}

	ctx, cancel := context.WithCancel(base)
	var paused atomic.Int32
	sub.pauseFn = func(pctx context.Context, sandboxID, templateID string) (string, substrate.BuildRefs, error) {
		if paused.Add(1) == 1 {
			cancel()
		}
		return sub.Fake.Pause(pctx, sandboxID, templateID)
	}
	t.Cleanup(func() { sub.pauseFn = nil })

	req := httptest.NewRequest("POST", "/api/admin/drain", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer admin-tok")
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { ts.Config.Handler.ServeHTTP(rr, req); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("drain handler did not finish")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("drain = %d (%s), want 200 despite the cancelled request", rr.Code, rr.Body.String())
	}
	for _, l := range leases {
		if !l.Drained || l.State != "suspended" {
			t.Fatalf("lease %s = state=%s drained=%v, want suspended and drained", l.ID, l.State, l.Drained)
		}
	}

	// Undrain through the handler with a cancelled request too.
	uctx, ucancel := context.WithCancel(base)
	ucancel()
	ureq := httptest.NewRequest("POST", "/api/admin/undrain", nil).WithContext(uctx)
	ureq.Header.Set("Authorization", "Bearer admin-tok")
	urr := httptest.NewRecorder()
	ts.Config.Handler.ServeHTTP(urr, ureq)
	if urr.Code != http.StatusOK {
		t.Fatalf("undrain = %d (%s), want 200 despite the cancelled request", urr.Code, urr.Body.String())
	}
	for _, l := range leases {
		if l.Drained || l.State != "running" {
			t.Fatalf("lease %s = state=%s drained=%v, want running and undrained", l.ID, l.State, l.Drained)
		}
	}
}

// TestAdminUndrainDoesNotDeferToHeal pins spoond-52c R4's choice: the
// undraining serialisation is one-way. The self-heal loop defers to an
// undrain, but an admin undrain runs even while a heal pass holds the
// flag, so a caller that wants the undrain now gets it.
func TestAdminUndrainDoesNotDeferToHeal(t *testing.T) {
	_, svc, _, _ := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	svc.cfg.UndrainResumeRetries = 0

	leases := grantAndDrain(t, svc, 1)

	// Simulate a heal pass in flight: the loop would have skipped itself,
	// but the admin undrain must not.
	svc.undraining.Store(true)
	defer svc.undraining.Store(false)

	res := svc.undrain(ctx)
	if res.Resumed != 1 {
		t.Fatalf("undrain = +%v, want the lease resumed even with the heal flag set", res)
	}
	if leases[0].Drained || leases[0].State != "running" {
		t.Fatalf("lease = state=%s drained=%v, want running and undrained", leases[0].State, leases[0].Drained)
	}
}
