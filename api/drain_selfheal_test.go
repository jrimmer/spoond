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

// TestStartAdoptsNodeDraining is spoond-52c B1: a backend that starts
// while the node reports draining (a restart between drain and undrain)
// must not believe it is healthy and undrain mid-planned-stop. At Start
// it adopts the drain with a fresh DRAIN_MAX_SECS clock, so the heal
// leaves it until the limit passes and then clears it.
func TestStartAdoptsNodeDraining(t *testing.T) {
	svc, _, sub := newTestService(t)
	seedImage(t, svc.db, "py-base", 2048)
	ctx := context.Background()

	// Drain some leases, then simulate a backend restart: the process
	// state is forgotten but the node still reports draining and the
	// lease rows keep Drained.
	leases := grantAndDrain(t, svc, 1)
	sub.SetNodeInfo(substrate.NodeInfo{Status: "draining", HugepagesTotal: 1 << 20, HugepageSizeBytes: 2 << 20}, nil)
	svc.draining.Store(false)
	svc.drainStartedAt.Store(0)
	svc.cfg.DrainMaxSecs = 1

	// Start's adoption path sets spoond's draining state and a fresh
	// DRAIN_MAX_SECS clock (it runs before the loops start).
	startCtx, stop := context.WithCancel(context.Background())
	svc.Start(startCtx)
	stop()
	if !svc.draining.Load() || svc.drainStartedAt.Load() == 0 {
		t.Fatalf("adopt: draining=%v startedAt=%d, want both set", svc.draining.Load(), svc.drainStartedAt.Load())
	}

	// A heal pass before the limit must not clear it (the node reports
	// draining, not healthy): the lease is still drained and suspended.
	svc.healDrain(ctx)
	if !svc.draining.Load() {
		t.Fatal("the adopted drain was cleared before DRAIN_MAX_SECS")
	}
	if !leases[0].Drained || leases[0].State == "running" {
		t.Fatalf("lease = state=%s drained=%v, want suspended and drained mid-planned-stop", leases[0].State, leases[0].Drained)
	}

	// Past the limit the heal clears it and resumes the lease.
	svc.drainStartedAt.Store(svc.now().Add(-5 * time.Second).UnixNano())
	svc.healDrain(ctx)
	if svc.draining.Load() {
		t.Fatal("the adopted drain was not cleared past DRAIN_MAX_SECS")
	}
	if leases[0].Drained || leases[0].State != "running" {
		t.Fatalf("lease = state=%s drained=%v, want running and undrained", leases[0].State, leases[0].Drained)
	}
}

// TestHealDoesNotUndrainOtherProcessesDrain is spoond-52c B1: when spoond
// is not draining but the node reports 'draining' (someone else's planned
// stop) and a lease of ours is still Drained, the heal resumes nothing
// until the node reports healthy again.
func TestHealDoesNotUndrainOtherProcessesDrain(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	svc.cfg.UndrainResumeRetries = 0

	leases := grantAndDrain(t, svc, 1)
	// Forget spoond's own drain state, keep the node draining.
	svc.draining.Store(false)
	svc.drainStartedAt.Store(0)
	sub.SetNodeInfo(substrate.NodeInfo{Status: "draining", HugepagesTotal: 1 << 20, HugepageSizeBytes: 2 << 20}, nil)

	svc.healDrain(ctx)
	if !leases[0].Drained || leases[0].State != "suspended" {
		t.Fatalf("lease = state=%s drained=%v, want it left suspended while the node drains", leases[0].State, leases[0].Drained)
	}

	// The node comes back healthy: the heal resumes it.
	sub.SetNodeInfo(substrate.NodeInfo{Status: "healthy", HugepagesTotal: 1 << 20, HugepageSizeBytes: 2 << 20}, nil)
	svc.healDrain(ctx)
	if leases[0].Drained || leases[0].State != "running" {
		t.Fatalf("lease = state=%s drained=%v, want running once healthy", leases[0].State, leases[0].Drained)
	}
}

// TestHealBackoffAndGivesUp is spoond-52c B2: a permanently deferred
// lease is retried with doubling backoff, not every pass, and after
// DRAIN_RESUME_MAX_AGE is left suspended with a drain_gave_up event.
func TestHealBackoffAndGivesUp(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	base := time.Now()
	now := base
	svc.now = func() time.Time { return now }
	svc.cfg.UndrainResumeRetries = 0
	svc.cfg.DrainResumeMaxAge = 30 * time.Second

	leases := grantAndDrain(t, svc, 1)
	target := leases[0]
	// Only the Drained recovery is under test: spoond is not draining and
	// the node is healthy.
	svc.draining.Store(false)
	svc.drainStartedAt.Store(0)
	sub.SetNodeInfo(substrate.NodeInfo{Status: "healthy", HugepagesTotal: 1 << 20, HugepageSizeBytes: 2 << 20}, nil)

	var creates atomic.Int32
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume {
			creates.Add(1)
			return substrate.Sandbox{}, errQuotaExceeded
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	events := svc.Subscribe(EventFilter{LeaseID: target.ID})
	defer events.Close()

	// The first pass attempts and defers.
	svc.healDrain(context.Background())
	if creates.Load() != 1 {
		t.Fatalf("first pass attempts = %d, want 1", creates.Load())
	}
	// A second pass inside the backoff window is skipped.
	svc.healDrain(context.Background())
	if creates.Load() != 1 {
		t.Fatalf("attempts inside the backoff window = %d, want 1", creates.Load())
	}
	// Past the backoff it retries.
	now = now.Add(drainHealBackoffMin + time.Second)
	svc.healDrain(context.Background())
	if creates.Load() != 2 {
		t.Fatalf("attempts after the backoff = %d, want 2", creates.Load())
	}
	// Only one drain_deferred event so far (the cause never changed).
	drainDeferred := 0
	for {
		select {
		case ev := <-events.C:
			if ev.Type == LeaseDrainDeferred {
				drainDeferred++
			}
			continue
		default:
		}
		break
	}
	if drainDeferred != 1 {
		t.Fatalf("drain_deferred events = %d, want 1 (state change only)", drainDeferred)
	}

	// Age past DRAIN_RESUME_MAX_AGE: the loop gives up, keeps the lease
	// suspended and emits drain_gave_up once.
	now = now.Add(31 * time.Second)
	svc.healDrain(context.Background())
	if target.State == "lost" {
		t.Fatal("the lease must be left suspended, not lost")
	}
	if !target.Drained {
		t.Fatal("a given-up lease keeps its Drained flag")
	}
	gaveUp := 0
	for {
		select {
		case ev := <-events.C:
			if ev.Type == LeaseDrainGaveUp {
				gaveUp++
			}
			continue
		default:
		}
		break
	}
	if gaveUp != 1 {
		t.Fatalf("drain_gave_up events = %d, want 1", gaveUp)
	}
	// A later pass makes no further attempt.
	before := creates.Load()
	now = now.Add(time.Hour)
	svc.healDrain(context.Background())
	if creates.Load() != before {
		t.Fatalf("attempts after giving up = %d, want no more than %d", creates.Load(), before)
	}
}

// TestHealSkipsSetDrainingWhenNotDraining is spoond-52c B2: a pass that
// has only a drained lease to resume (spoond is not draining) must not
// call SetDraining(false) at all.
func TestHealSkipsSetDrainingWhenNotDraining(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	svc.cfg.UndrainResumeRetries = 0

	grantAndDrain(t, svc, 1)
	svc.draining.Store(false)
	svc.drainStartedAt.Store(0)
	sub.SetNodeInfo(substrate.NodeInfo{Status: "healthy", HugepagesTotal: 1 << 20, HugepageSizeBytes: 2 << 20}, nil)
	before := calls(sub.Fake, "SetDraining")

	svc.healDrain(ctx)
	if got := calls(sub.Fake, "SetDraining") - before; got != 0 {
		t.Fatalf("SetDraining calls during a Drained-only heal = %d, want 0", got)
	}
}

// TestHealDeletedHalfSandbox is spoond-52c S2: a resume that fails and
// leaves a half-started sandbox has it deleted (best effort) before the
// deferral.
func TestHealDeletedHalfSandbox(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	svc.cfg.UndrainResumeRetries = 0

	leases := grantAndDrain(t, svc, 1)
	target := leases[0]
	svc.draining.Store(false)
	svc.drainStartedAt.Store(0)
	sub.SetNodeInfo(substrate.NodeInfo{Status: "healthy", HugepagesTotal: 1 << 20, HugepageSizeBytes: 2 << 20}, nil)

	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume {
			return substrate.Sandbox{}, errQuotaExceeded
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	before := calls(sub.Fake, "Delete")
	svc.healDrain(ctx)
	if got := calls(sub.Fake, "Delete") - before; got != 1 {
		t.Fatalf("Delete calls during a deferred heal = %d, want 1", got)
	}
	if !target.Drained {
		t.Fatal("the lease must stay drained")
	}
}

// TestResumeOfReleasedLeaseDeletesSandbox is spoond-52c S4 (spoond-775
// class): a release that lands while a resume's Create is in flight must
// not be overwritten by the resume's save, and the sandbox just created
// for the released lease is deleted.
func TestResumeOfReleasedLeaseDeletesSandbox(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.suspend(ctx, "c", l.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	sub.createFn = func(cctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		sb, err := sub.Fake.Create(cctx, req)
		if err != nil {
			return sb, err
		}
		// The owner's DELETE lands mid-Create.
		svc.store.mu.Lock()
		l.released = true
		svc.store.mu.Unlock()
		return sb, nil
	}
	t.Cleanup(func() { sub.createFn = nil })

	before := calls(sub.Fake, "Delete")
	if _, err := svc.resumeLease(ctx, l); !errors.Is(err, errLeaseReleased) {
		t.Fatalf("resume of a released lease = %v, want errLeaseReleased", err)
	}
	if l.State != "suspended" || !l.Suspended {
		t.Fatalf("lease = state=%s suspended=%v, want it left suspended", l.State, l.Suspended)
	}
	if got := calls(sub.Fake, "Delete") - before; got != 1 {
		t.Fatalf("Delete calls = %d, want 1 (the half-started sandbox)", got)
	}
	for _, id := range sub.sandboxesLive(t) {
		if id == l.SandboxID {
			t.Fatalf("the released lease's new sandbox %s is still live", id)
		}
	}
}

// TestHealStateDoesNotLeakAcrossRestarts is spoond-52c B3: the per-lease
// heal backoff must not survive a lease being resumed, restored or
// released (a give-up entry from one drain must not silently skip a
// later planned restart's deferral, or give up on its first pass).
func TestHealStateDoesNotLeakAcrossRestarts(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	base := time.Now()
	now := base
	svc.now = func() time.Time { return now }
	svc.cfg.UndrainResumeRetries = 0
	// A short bound so the first drain's deferral gives up.
	svc.cfg.DrainResumeMaxAge = 30 * time.Second
	ctx := context.Background()

	leases := grantAndDrain(t, svc, 1)
	target := leases[0]
	// Heal only: spoond is not draining and the node is healthy.
	svc.draining.Store(false)
	svc.drainStartedAt.Store(0)
	sub.SetNodeInfo(substrate.NodeInfo{Status: "healthy", HugepagesTotal: 1 << 20, HugepageSizeBytes: 2 << 20}, nil)

	var refuse atomic.Bool
	refuse.Store(true)
	sub.createFn = func(cctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume && refuse.Load() {
			return substrate.Sandbox{}, errQuotaExceeded
		}
		return sub.Fake.Create(cctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	// First pass defers; age past the bound and the heal gives up.
	svc.healDrain(ctx)
	now = now.Add(31 * time.Second)
	svc.healDrain(ctx)
	if !target.Drained {
		t.Fatal("a given-up lease keeps its Drained flag")
	}
	if _, ok := svc.drainHeal[target.ID]; !ok {
		t.Fatal("the heal state should still be present after give-up")
	}

	// The owner resumes by hand: Drained and the heal state must clear.
	refuse.Store(false)
	svc.drainStartedAt.Store(0)
	if _, err := svc.resume(ctx, "c", target.ID); err != nil {
		t.Fatalf("owner resume: %v", err)
	}
	if target.Drained {
		t.Fatal("the owner resume must clear Drained")
	}
	if _, ok := svc.drainHeal[target.ID]; ok {
		t.Fatal("the owner resume must clear the stale heal state (B3)")
	}

	// A later planned restart: drain again and defer the undrain. The
	// stale gave-up state must not skip or immediately give up on it.
	if _, err := svc.drain(ctx); err != nil {
		t.Fatalf("second drain: %v", err)
	}
	refuse.Store(true)
	res := svc.undrain(ctx)
	if res.Resumed != 0 || len(res.Failed) != 1 {
		t.Fatalf("second undrain = +%v, want it deferred", res)
	}
	if !target.Drained {
		t.Fatal("the deferred lease must stay drained")
	}
	// The heal (fresh state, well inside the bound) retries and resumes.
	refuse.Store(false)
	svc.draining.Store(false)
	svc.drainStartedAt.Store(0)
	sub.SetNodeInfo(substrate.NodeInfo{Status: "healthy", HugepagesTotal: 1 << 20, HugepageSizeBytes: 2 << 20}, nil)
	svc.healDrain(ctx)
	if target.Drained || target.State != "running" {
		t.Fatalf("after the second heal: state=%s drained=%v, want running and undrained", target.State, target.Drained)
	}
}

// TestHealPrunesStaleState: a heal pass drops the backoff entry of a
// lease that is no longer a drained target (released, lost or resumed
// elsewhere), so it cannot skip a later deferral (spoond-52c B3).
func TestHealPrunesStaleState(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	svc.cfg.UndrainResumeRetries = 0

	leases := grantAndDrain(t, svc, 2)
	target := leases[0]
	keep := leases[1]
	svc.draining.Store(false)
	svc.drainStartedAt.Store(0)
	sub.SetNodeInfo(substrate.NodeInfo{Status: "healthy", HugepagesTotal: 1 << 20, HugepageSizeBytes: 2 << 20}, nil)

	// Seed a stale entry as if a prior deferral left one, then clear the
	// lease's Drained flag (the owner resumed it). keep stays a drained
	// target so the pass runs and reaches the pruning step.
	_ = svc.drainHealFor(target.ID)
	svc.store.mu.Lock()
	target.Drained = false
	svc.store.mu.Unlock()

	// Do not let the pass resume keep yet (a deferral), so the test only
	// observes the prune.
	svc.cfg.DrainResumeMaxAge = -1
	sub.createFn = func(cctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume {
			return substrate.Sandbox{}, errQuotaExceeded
		}
		return sub.Fake.Create(cctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	svc.healDrain(ctx)
	if _, ok := svc.drainHeal[target.ID]; ok {
		t.Fatal("a pass must prune the heal state of a lease that is not a drained target")
	}
	if !keep.Drained {
		t.Fatal("the other lease must still be drained")
	}
}

// TestHealHoldsDrainGate: the heal pass holds the read side of drainGate
// across a resume, so an admin drain that takes the write side cannot
// start a Create into the stop (spoond-52c S1).
func TestHealHoldsDrainGate(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	svc.cfg.UndrainResumeRetries = 0

	leases := grantAndDrain(t, svc, 1)
	target := leases[0]
	svc.draining.Store(false)
	svc.drainStartedAt.Store(0)
	sub.SetNodeInfo(substrate.NodeInfo{Status: "healthy", HugepagesTotal: 1 << 20, HugepageSizeBytes: 2 << 20}, nil)

	// While the heal's resume Create runs, an admin drain must not be able
	// to take the write side. The heal took the read side, so TryLock fails.
	var tried atomic.Bool
	var blocked atomic.Bool
	sub.createFn = func(cctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume {
			tried.Store(true)
			if svc.drainGate.TryLock() {
				svc.drainGate.Unlock()
			} else {
				blocked.Store(true)
			}
		}
		return sub.Fake.Create(cctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	svc.healDrain(ctx)
	if target.Drained || target.State != "running" {
		t.Fatalf("heal did not resume: state=%s drained=%v", target.State, target.Drained)
	}
	if !tried.Load() {
		t.Fatal("the resume Create did not run")
	}
	if !blocked.Load() {
		t.Fatal("the heal did not hold drainGate.RLock across the resume (S1)")
	}
}

// TestAdmitRefusalHealDoesNotDelete is spoond-52c S2/NIT: a resume that
// is refused before reaching the orchestrator (the node is not healthy,
// so admit returns ErrCapacity) never created a sandbox, so the heal
// must not issue a Delete for it (the id still names the paused
// sandbox) nor log a cleanup line.
func TestAdmitRefusalHealDoesNotDelete(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	svc.cfg.UndrainResumeRetries = 0

	leases := grantAndDrain(t, svc, 1)
	target := leases[0]
	svc.draining.Store(false)
	svc.drainStartedAt.Store(0)
	// The heal's own NodeInfo up front sees a healthy node (so it proceeds
	// to resume), but every later NodeInfo reports unhealthy: admit's
	// capacity check refuses before Create, exactly the pre-Create
	// refusal. The second read is the one admit makes; the heal's first
	// read resolves the node.
	var nodeCalls atomic.Int32
	sub.SetNodeInfoFunc(func(context.Context) (substrate.NodeInfo, error) {
		if nodeCalls.Add(1) == 1 {
			return substrate.NodeInfo{Status: "healthy", HugepagesTotal: 1 << 20, HugepageSizeBytes: 2 << 20}, nil
		}
		return substrate.NodeInfo{Status: "unhealthy", HugepagesTotal: 1 << 20, HugepageSizeBytes: 2 << 20}, nil
	})
	t.Cleanup(func() { sub.SetNodeInfoFunc(nil) })

	var creates atomic.Int32
	sub.createFn = func(cctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume {
			creates.Add(1)
		}
		return sub.Fake.Create(cctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	before := calls(sub.Fake, "Delete")
	svc.healDrain(ctx)
	if creates.Load() != 0 {
		t.Fatalf("Create ran for a refused admission: %d", creates.Load())
	}
	if got := calls(sub.Fake, "Delete") - before; got != 0 {
		t.Fatalf("Delete calls after a pre-Create admit refusal = %d, want 0", got)
	}
	if !target.Drained {
		t.Fatal("the refused lease must stay drained")
	}
}

// TestHealReleasesDrainGateBetweenRetries is spoond-52c S1: a wedged
// Create that the heal retries must not hold drainGate's read side for
// the whole retry budget. An admin drain waiting on the write side gets
// in between the first attempt's backoff and the second attempt, and the
// heal then re-checks draining and defers instead of starting a second
// Create into the stop.
func TestHealReleasesDrainGateBetweenRetries(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	svc.cfg.UndrainResumeRetries = 2

	leases := grantAndDrain(t, svc, 1)
	target := leases[0]
	svc.draining.Store(false)
	svc.drainStartedAt.Store(0)
	sub.SetNodeInfo(substrate.NodeInfo{Status: "healthy", HugepagesTotal: 1 << 20, HugepageSizeBytes: 2 << 20}, nil)

	// Every resume Create fails with a retryable envd error, so the heal
	// retries. Each attempt signals that it ran so the test can drive the
	// waiting admin drain and count attempts at the moment it gets the
	// gate.
	var attemptCount atomic.Int32
	attempted := make(chan struct{}, 8)
	sub.createFn = func(cctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume {
			attemptCount.Add(1)
			attempted <- struct{}{}
			return substrate.Sandbox{}, errors.New("failed to init envd: syncing took too long")
		}
		return sub.Fake.Create(cctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	healDone := make(chan struct{})
	go func() {
		defer close(healDone)
		svc.healDrain(ctx)
	}()

	// Wait for the first attempt, then start a drain that needs the write
	// side. It must acquire the gate in the backoff gap before the heal's
	// second attempt, so it observes exactly one attempt.
	<-attempted
	var attemptsAtAcquire atomic.Int32
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		svc.drainGate.Lock()
		attemptsAtAcquire.Store(attemptCount.Load())
		svc.draining.Store(true)
		svc.drainGate.Unlock()
	}()

	select {
	case <-drainDone:
	case <-time.After(3 * time.Second):
		t.Fatal("a waiting drain did not get the write side between heal retries")
	}
	<-healDone

	if got := attemptsAtAcquire.Load(); got != 1 {
		t.Fatalf("the drain got the gate after %d heal attempts, want 1 (between attempts, not after the budget)", got)
	}
	// After the drain set draining, the heal must not have made another
	// Create attempt, and the lease stays drained.
	if got := attemptCount.Load(); got != 1 {
		t.Fatalf("the heal attempted %d Creates across the drain, want 1 (no Create into the stop)", got)
	}
	if !target.Drained {
		t.Fatal("the deferred lease must stay drained")
	}
}

// TestAdminUndrainClearsStaleGiveUp is spoond-52c NIT: a give-up entry
// left by an earlier restart must not survive an admin undrain that
// defers the lease, so the new deferral starts a fresh heal budget and a
// later heal tries again.
func TestAdminUndrainClearsStaleGiveUp(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	base := time.Now()
	now := base
	svc.now = func() time.Time { return now }
	svc.cfg.UndrainResumeRetries = 0
	svc.cfg.DrainResumeMaxAge = 30 * time.Second
	ctx := context.Background()

	leases := grantAndDrain(t, svc, 1)
	target := leases[0]
	svc.draining.Store(false)
	svc.drainStartedAt.Store(0)
	sub.SetNodeInfo(substrate.NodeInfo{Status: "healthy", HugepagesTotal: 1 << 20, HugepageSizeBytes: 2 << 20}, nil)

	var refuse atomic.Bool
	refuse.Store(true)
	sub.createFn = func(cctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume && refuse.Load() {
			return substrate.Sandbox{}, errQuotaExceeded
		}
		return sub.Fake.Create(cctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	// First deferral, then age past the bound so the heal gives up and
	// leaves a stale gave-up entry.
	svc.healDrain(ctx)
	now = now.Add(31 * time.Second)
	svc.healDrain(ctx)
	if _, ok := svc.drainHeal[target.ID]; !ok {
		t.Fatal("the heal state should be present after give-up")
	}

	// A later admin undrain defers the lease again: it must drop the old
	// gave-up entry so the new deferral gets a fresh budget.
	svc.draining.Store(true)
	res := svc.undrain(ctx)
	if res.Resumed != 0 || len(res.Failed) != 1 {
		t.Fatalf("undrain = +%v, want it deferred", res)
	}
	if _, ok := svc.drainHeal[target.ID]; ok {
		t.Fatal("a deferring admin undrain must clear the stale gave-up entry")
	}

	// Well inside the bound, the next heal retries and resumes.
	refuse.Store(false)
	svc.draining.Store(false)
	svc.drainStartedAt.Store(0)
	sub.SetNodeInfo(substrate.NodeInfo{Status: "healthy", HugepagesTotal: 1 << 20, HugepageSizeBytes: 2 << 20}, nil)
	svc.healDrain(ctx)
	if target.Drained || target.State != "running" {
		t.Fatalf("after the fresh deferral: state=%s drained=%v, want running and undrained", target.State, target.Drained)
	}
}
