package api

// A create or resume whose substrate create fails because the HOST ran
// out of memory (the snapshot load's mmap memfd or a uffdio copy) must
// answer the capacity_wait the substrate's own capacity refusals get —
// 503 with a Retry-After and code, waitable — not a 500
// "failed to grant lease" (spoond-6ayi). The gRPC error carries only
// text, so the tests fake the orchestrator's messages verbatim.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v3/metrics"
	"github.com/jrimmer/spoond/v3/substrate"
)

// hostENOMEMErrors are the messages the orchestrator sent on the
// 2026-10-14 incident, as they reach spoond (the gRPC error's text).
var hostENOMEMErrors = []string{
	"failed to start FC: error loading snapshot: run_sandbox: mmap memfd: cannot allocate memory",
	"resume: failed uffdio copy: cannot allocate memory",
	"Cannot Allocate Memory", // the case varies by source
}

// expectHostENOMEMCapacityWait pins the shared refusal shape: 503 with
// Retry-After: 30 and code capacity_wait, whatever the path.
func expectHostENOMEMCapacityWait(t *testing.T, resp *http.Response, body map[string]any) {
	t.Helper()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d (%v), want 503", resp.StatusCode, body)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "30" {
		t.Fatalf("Retry-After = %q, want 30", ra)
	}
	if code, _ := body["code"].(string); code != "capacity_wait" {
		t.Fatalf("code = %v, want capacity_wait: %v", body["code"], body)
	}
}

// enomemCreate installs a createFn whose substrate create fails with the
// given ENOMEM text.
func enomemCreate(sub *testSub, msg string) {
	sub.createFn = func(context.Context, substrate.CreateRequest) (substrate.Sandbox, error) {
		return substrate.Sandbox{}, fmt.Errorf("failed to start FC: %s", msg)
	}
}

// TestCreateHostENOMEMAnswersCapacityWait: a plain create whose snapshot
// load hits host ENOMEM answers 503 capacity_wait with a Retry-After,
// not the 500 "failed to grant lease" (spoond-6ayi). Each of the
// messages the incident showed, the case variations included.
func TestCreateHostENOMEMAnswersCapacityWait(t *testing.T) {
	for _, msg := range hostENOMEMErrors {
		t.Run(msg, func(t *testing.T) {
			ts, _, _, sub := newTestServerWithService(t)
			enomemCreate(sub, msg)
			t.Cleanup(func() { sub.createFn = nil })
			resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
				map[string]any{"image": "py-base", "ttl": 300})
			expectHostENOMEMCapacityWait(t, resp, body)
			resp.Body.Close()
			if msg, _ := body["error"].(string); !strings.Contains(strings.ToLower(msg), "cannot allocate memory") {
				t.Fatalf("body should keep the orchestrator's detail, got %v", body)
			}
		})
	}
}

// TestCreateHostENOMEMCountedAndClean: the refusal is counted with the
// other capacity refusals (spoond_capacity_rejections_total), no lease
// is left behind, and both the owner's in-flight quota reservation
// (pending / pendingMiB) and the service's in-flight admission debit
// (inflightAdmitMiB) are back to zero, exactly as on any failed create.
func TestCreateHostENOMEMCountedAndClean(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	m := metrics.NewBackendMetrics()
	svc.SetMetrics(m)
	enomemCreate(sub, hostENOMEMErrors[0])
	t.Cleanup(func() { sub.createFn = nil })
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
		map[string]any{"image": "py-base", "ttl": 300})
	expectHostENOMEMCapacityWait(t, resp, body)
	resp.Body.Close()

	if leases := svc.leasesOfOwner("consumer-a"); len(leases) != 0 {
		t.Fatalf("leases after the ENOMEM create = %d, want 0", len(leases))
	}
	svc.store.mu.Lock()
	pending, pendingMiB := svc.store.pending["consumer-a"], svc.store.pendingMiB["consumer-a"]
	svc.store.mu.Unlock()
	if pending != 0 || pendingMiB != 0 {
		t.Fatalf("pending = %d, pendingMiB = %d after the failed create; want 0, 0", pending, pendingMiB)
	}
	svc.nodeInfoMu.Lock()
	inflight := svc.inflightAdmitMiB
	svc.nodeInfoMu.Unlock()
	if inflight != 0 {
		t.Fatalf("inflightAdmitMiB = %d after the failed create; want 0", inflight)
	}
	if got := counterValue(t, m.CapacityRej); got != 1 {
		t.Fatalf("spoond_capacity_rejections_total = %v, want 1", got)
	}
}

// TestCreateHostENOMEMIsWaitable: a create that asks to wait is queued
// on the ENOMEM refusal like any capacity refusal; when the failing
// substrate recovers (the ENOMEM text stops), the queue's retry grants
// the create and the answer is 201.
func TestCreateHostENOMEMIsWaitable(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	var recover atomic.Bool
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if !recover.Load() {
			return substrate.Sandbox{}, errors.New("failed to start FC: error loading snapshot: mmap memfd: cannot allocate memory")
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })
	svc.admitQ.tick = 20 * time.Millisecond
	svc.cfg.MaxAdmitWaitSecs = 60
	// The queue's periodic retry runs only from Start's loops; the test's
	// service has none, so run it here against a stoppable context.
	loopsCtx, stopLoops := context.WithCancel(context.Background())
	t.Cleanup(stopLoops)
	go svc.runAdmitQueueLoop(loopsCtx)

	type result struct {
		code int
		body map[string]any
	}
	res := make(chan result, 1)
	go func() {
		resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
			map[string]any{"image": "py-base", "ttl": 300, "wait": 30})
		res <- result{resp.StatusCode, body}
	}()
	// Let the create land in the queue and fail its first pass, then let
	// the host "recover": the queue's periodic retry must grant it.
	deadline := time.After(5 * time.Second)
	for svc.queueDepth() == 0 {
		select {
		case <-deadline:
			t.Fatal("the ENOMEM create never queued")
		case <-time.After(5 * time.Millisecond):
		}
	}
	recover.Store(true)
	select {
	case r := <-res:
		if r.code != http.StatusCreated {
			t.Fatalf("queued ENOMEM create after recovery = %d (%v), want 201", r.code, r.body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the queued ENOMEM create was never retried")
	}
}

// TestResumeHostENOMEMAnswersCapacityWait: resuming a suspended lease
// whose snapshot failed to load into the host answers 503
// capacity_wait, not 500 "resume failed"; the lease stays suspended and
// neither a quota reservation nor an admission debit is left behind.
func TestResumeHostENOMEMAnswersCapacityWait(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	ctx := context.Background()
	m := metrics.NewBackendMetrics()
	svc.SetMetrics(m)
	l, err := svc.grantLease(ctx, leaseRequest{owner: "consumer-a", image: "py-base", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.suspend(ctx, "consumer-a", l.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	enomemCreate(sub, hostENOMEMErrors[1])
	t.Cleanup(func() { sub.createFn = nil })
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+l.ID+"/resume", "token-a", nil)
	expectHostENOMEMCapacityWait(t, resp, body)
	resp.Body.Close()

	svc.store.mu.Lock()
	still := svc.store.leases[l.ID]
	var busy, suspended bool
	var pending, pendingMiB int
	if still != nil {
		busy, suspended = still.busy, still.Suspended
		pending, pendingMiB = svc.store.pending["consumer-a"], svc.store.pendingMiB["consumer-a"]
	}
	svc.store.mu.Unlock()
	if still == nil || !suspended || busy {
		t.Fatalf("lease after the failed resume: present=%v suspended=%v busy=%v; want present, suspended, not busy", still != nil, suspended, busy)
	}
	if pending != 0 || pendingMiB != 0 {
		t.Fatalf("pending = %d, pendingMiB = %d after the failed resume; want 0, 0", pending, pendingMiB)
	}
	svc.nodeInfoMu.Lock()
	inflight := svc.inflightAdmitMiB
	svc.nodeInfoMu.Unlock()
	if inflight != 0 {
		t.Fatalf("inflightAdmitMiB = %d after the failed resume; want 0", inflight)
	}
	if got := counterValue(t, m.CapacityRej); got != 1 {
		t.Fatalf("spoond_capacity_rejections_total = %v, want 1", got)
	}
}

// TestCreateOtherSubstrateErrorsStay500: only the ENOMEM text is read as
// a capacity refusal; any other substrate create failure keeps the 500.
func TestCreateOtherSubstrateErrorsStay500(t *testing.T) {
	ts, _, _, sub := newTestServerWithService(t)
	sub.createFn = func(context.Context, substrate.CreateRequest) (substrate.Sandbox, error) {
		return substrate.Sandbox{}, errors.New("failed to start FC: vmlinux file not found")
	}
	t.Cleanup(func() { sub.createFn = nil })
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
		map[string]any{"image": "py-base", "ttl": 300})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("non-ENOMEM create = %d (%v), want 500", resp.StatusCode, body)
	}
}
