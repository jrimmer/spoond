package api

// Structured suspension facts (#145 D6): every automatic suspend stamps
// suspend_reason, suspend_policy_step, suspend_build_id and suspended_at
// on the lease, emits them as structured fields on the suspended event
// and (when set) returns them from GET and the 409 lease_suspended body.
// A resume, restore, cold restart or recovery clears them. A hand or
// drain suspend carries no reason.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// suspendFactsOf reads a lease's structured suspension fields under the
// store lock.
func suspendFactsOf(t *testing.T, svc *Service, id string) (reason, step, build string, at time.Time) {
	t.Helper()
	svc.store.mu.Lock()
	defer svc.store.mu.Unlock()
	l := svc.store.leases[id]
	if l == nil {
		t.Fatalf("lease %s not in the store", id)
	}
	return l.SuspendReason, l.SuspendPolicyStep, l.SuspendBuildID, l.SuspendedAt
}

// suspendedEventSuspension returns the last `suspended` event for the
// lease among evs.
func suspendedEventSuspension(t *testing.T, evs []LeaseEvent, id string) LeaseEvent {
	t.Helper()
	var got *LeaseEvent
	for i := range evs {
		if evs[i].LeaseID == id && evs[i].Type == LeaseSuspended {
			got = &evs[i]
		}
	}
	if got == nil {
		t.Fatalf("no suspended event for %s among %v", id, eventTypes(evs))
	}
	return *got
}

// TestSuspendFactsIdleSweep: the plain IDLE_TIMEOUT_SECS sweep (reason
// idle) stamps the fields and emits them on the suspended event.
func TestSuspendFactsIdleSweep(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	base := time.Now()
	svc.now = func() time.Time { return base }
	svc.cfg.IdleTimeout = time.Minute
	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.store.mu.Lock()
	l.LastActive = base.Add(-2 * time.Minute)
	svc.store.mu.Unlock()

	all := svc.Subscribe(EventFilter{})
	svc.sweepExpired(ctx)
	all.Close()

	reason, step, build, at := suspendFactsOf(t, svc, l.ID)
	if reason != suspendReasonIdle {
		t.Fatalf("suspend_reason = %q, want %q", reason, suspendReasonIdle)
	}
	if step != "" || build == "" || at.IsZero() {
		t.Fatalf("suspend facts = step %q build %q at %v, want empty step, a build and a time", step, build, at)
	}
	if build != l.ResumeBuildID {
		t.Fatalf("suspend_build_id = %q, want the pause build %q", build, l.ResumeBuildID)
	}
	ev := suspendedEventSuspension(t, eventsFor(collectEvents(all.C), l.ID), l.ID)
	if ev.Reason != suspendReasonIdle || ev.BuildID != build || ev.PolicyStep != "" {
		t.Fatalf("suspended event = reason %q step %q build %q, want idle/%q", ev.Reason, ev.PolicyStep, ev.BuildID, build)
	}
	if ev.Detail != "paused into build "+build {
		t.Fatalf("suspended detail = %q, want the build text", ev.Detail)
	}
}

// TestSuspendFactsIdleSuspend: a per-lease idle_suspend suspension
// records reason idle_suspend.
func TestSuspendFactsIdleSuspend(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	base := time.Now()
	svc.now = func() time.Time { return base }
	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.setIdlePolicy(l, 60); err != nil {
		t.Fatalf("setIdlePolicy: %v", err)
	}
	svc.store.mu.Lock()
	l.LastActive = base
	svc.store.mu.Unlock()

	svc.suspendIdleLeases(ctx, base.Add(2*time.Minute))

	reason, _, build, at := suspendFactsOf(t, svc, l.ID)
	if reason != suspendReasonIdleSuspend {
		t.Fatalf("suspend_reason = %q, want %q", reason, suspendReasonIdleSuspend)
	}
	if build == "" || at.IsZero() {
		t.Fatalf("suspend facts = build %q at %v, want both set", build, at)
	}
}

// TestSuspendFactsHoldLapseAndPressure: a lapsed hold (reason
// hold_lapsed) and rule 1 under pressure (reason pressure) are distinct.
func TestSuspendFactsHoldLapseAndPressure(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	base := time.Now()
	cur := base
	svc.now = func() time.Time { return cur }
	svc.cfg.HeldIdleTimeout = 4 * time.Hour
	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "ci-job", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.store.mu.Lock()
	l.LastActive = base
	l.HoldExpiresAt = base.Add(time.Minute)
	svc.store.mu.Unlock()

	// The hold lapses: a running lease is suspended with hold_lapsed.
	cur = base.Add(2 * time.Minute)
	svc.expireHolds(ctx, cur)
	reason, _, build, at := suspendFactsOf(t, svc, l.ID)
	if reason != suspendReasonHoldLapsed {
		t.Fatalf("hold-lapse suspend_reason = %q, want %q", reason, suspendReasonHoldLapsed)
	}
	if build == "" || at.IsZero() {
		t.Fatalf("hold-lapse suspend facts = build %q at %v", build, at)
	}

	// Now a held lease suspended by rule 1 under disk pressure records
	// pressure.
	p, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "ci-job-2", "", nil)
	if err != nil {
		t.Fatalf("grant pressured: %v", err)
	}
	svc.cfg.PressureDiskFreePct = 15
	svc.cfg.PressureHeldIdle = 30 * time.Minute
	svc.cfg.TemplateStoragePath = t.TempDir()
	svc.diskCapacity = func(string) (uint64, uint64, error) { return 100, 10, nil }
	svc.store.mu.Lock()
	p.LastActive = cur
	svc.store.mu.Unlock()
	svc.suspendIdleHeld(ctx, cur.Add(31*time.Minute), 30*time.Minute, "disk 10.0% free")
	reason, _, build, at = suspendFactsOf(t, svc, p.ID)
	if reason != suspendReasonPressure {
		t.Fatalf("pressure suspend_reason = %q, want %q", reason, suspendReasonPressure)
	}
	if build == "" || at.IsZero() {
		t.Fatalf("pressure suspend facts = build %q at %v", build, at)
	}
}

// TestSuspendFactsPreempt: a preemption records reason preempt.
func TestSuspendFactsPreempt(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.cfg.PreemptDiskFloorPct = 0
	svc.diskCapacity = func(string) (uint64, uint64, error) { return 100, 100, nil }

	base := time.Now()
	svc.now = func() time.Time { return base }
	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.store.mu.Lock()
	l.Class = ClassBurst
	svc.store.mu.Unlock()

	if err := svc.preemptLease(ctx, l, "other-owner"); err != nil {
		t.Fatalf("preemptLease: %v", err)
	}
	reason, _, build, at := suspendFactsOf(t, svc, l.ID)
	if reason != suspendReasonPreempt {
		t.Fatalf("preempt suspend_reason = %q, want %q", reason, suspendReasonPreempt)
	}
	if build == "" || at.IsZero() {
		t.Fatalf("preempt suspend facts = build %q at %v", build, at)
	}
}

// TestSuspendFactsClearedOnResume: resume, restore, cold restart and
// crash recovery all clear the structured suspension facts.
func TestSuspendFactsClearedOnResume(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.pauseLeaseWith(ctx, l, false, suspendPolicy{reason: suspendReasonIdle}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := svc.resume(ctx, "c", l.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	reason, step, build, at := suspendFactsOf(t, svc, l.ID)
	if reason != "" || step != "" || build != "" || !at.IsZero() {
		t.Fatalf("facts after resume = %q/%q/%q/%v, want empty", reason, step, build, at)
	}

	// A cold restart clears them too.
	if _, err := svc.pauseLeaseWith(ctx, l, false, suspendPolicy{reason: suspendReasonIdle}); err != nil {
		t.Fatalf("pause before cold restart: %v", err)
	}
	if _, err := svc.restart(ctx, "c", l.ID, "cold"); err != nil {
		t.Fatalf("cold restart: %v", err)
	}
	reason, step, build, at = suspendFactsOf(t, svc, l.ID)
	if reason != "" || step != "" || build != "" || !at.IsZero() {
		t.Fatalf("facts after cold restart = %q/%q/%q/%v, want empty", reason, step, build, at)
	}

	// setState is the shared seam restore and recovery run through: it
	// clears the facts for every state but suspended.
	svc.store.mu.Lock()
	l.SuspendReason = suspendReasonPressure
	l.SuspendPolicyStep = "pressure/disk"
	l.SuspendBuildID = "b-x"
	l.SuspendedAt = time.Now()
	l.setState("recovered")
	svc.store.mu.Unlock()
	reason, step, build, at = suspendFactsOf(t, svc, l.ID)
	if reason != "" || step != "" || build != "" || !at.IsZero() {
		t.Fatalf("facts after setState(recovered) = %q/%q/%q/%v, want empty", reason, step, build, at)
	}
}

// TestSuspendFactsHandAndDrainNoReason: a hand suspend and a drain
// pause carry no structured facts (no automatic reason).
func TestSuspendFactsHandAndDrainNoReason(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.suspend(ctx, "c", l.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	reason, step, build, at := suspendFactsOf(t, svc, l.ID)
	if reason != "" || step != "" || build != "" || !at.IsZero() {
		t.Fatalf("hand suspend facts = %q/%q/%q/%v, want empty", reason, step, build, at)
	}
}

// TestSuspendFactsPersistedAndServed: the fields survive a store reload
// and appear in GET (when set) and in the 409 lease_suspended body.
func TestSuspendFactsPersistedAndServed(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	id := suspendedLease(t, ts)
	// suspendedLease hand-suspends: stamp an automatic reason directly to
	// exercise the persistence and GET/409 paths.
	build := "b-145"
	svc.store.mu.Lock()
	l := svc.store.leases[id]
	l.SuspendReason = suspendReasonIdleSuspend
	l.SuspendPolicyStep = "pressure/disk"
	l.SuspendBuildID = build
	l.SuspendedAt = time.Now().UTC().Truncate(time.Second)
	svc.saveLeaseLocked(l)
	svc.store.mu.Unlock()

	reason, step, gotBuild, at := suspendFactsOf(t, svc, id)
	if reason != suspendReasonIdleSuspend || step != "pressure/disk" || gotBuild != build || at.IsZero() {
		t.Fatalf("in-memory facts = %q/%q/%q/%v", reason, step, gotBuild, at)
	}
	// The store round-trips them.
	row, err := db.GetLease(context.Background(), id)
	if err != nil {
		t.Fatalf("get lease: %v", err)
	}
	if row.SuspendReason != suspendReasonIdleSuspend || row.SuspendPolicyStep != "pressure/disk" ||
		row.SuspendBuildID != build || !row.SuspendedAt.Equal(at) {
		t.Fatalf("stored facts = %+v", row)
	}

	// GET serves them.
	resp, body := doReq(t, "GET", ts.URL+"/api/leases/"+id, "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status %d: %v", resp.StatusCode, body)
	}
	if body["suspend_reason"] != suspendReasonIdleSuspend || body["suspend_policy_step"] != "pressure/disk" ||
		body["suspend_build_id"] != build || body["suspended_at"] == nil {
		t.Fatalf("GET body = %v", body)
	}

	// The 409 carries the reason.
	resp, body = doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/network", "token-a",
		map[string]any{"network_policy": "lan"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("network status = %d, want 409", resp.StatusCode)
	}
	if body["code"] != "lease_suspended" || body["reason"] != suspendReasonIdleSuspend {
		t.Fatalf("409 body = %v, want the suspended code and reason", body)
	}
}

// TestLeaseSuspended409NoReasonForHandSuspend: a hand-suspended lease
// omits the reason, so older clients see the same body as before.
func TestLeaseSuspended409NoReasonForHandSuspend(t *testing.T) {
	ts, _, _, _ := newTestServerWithService(t)
	id := suspendedLease(t, ts)
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/network", "token-a",
		map[string]any{"network_policy": "lan"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if _, ok := body["reason"]; ok {
		t.Fatalf("409 body = %v, want no reason for a hand suspend", body)
	}
}

// TestSuspendEventStructuredWire: the SSE data line carries the
// structured fields and omits them when empty.
func TestSuspendEventStructuredWire(t *testing.T) {
	ev := LeaseEvent{
		Seq: 1, Epoch: "e", At: time.Now(), LeaseID: "l", Owner: "o",
		Type: LeaseSuspended, Detail: "paused into build b1",
		Reason: suspendReasonPreempt, BuildID: "b1",
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(marshalLeaseEvent(&ev)), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["reason"] != suspendReasonPreempt || m["build_id"] != "b1" {
		t.Fatalf("wire = %v, want reason and build_id", m)
	}
	if _, ok := m["policy_step"]; ok {
		t.Fatalf("wire = %v, want policy_step omitted when empty", m)
	}

	bare := LeaseEvent{Seq: 2, Epoch: "e", At: time.Now(), LeaseID: "l", Owner: "o",
		Type: LeaseSuspended, Detail: "paused into build b2"}
	m = map[string]any{}
	if err := json.Unmarshal([]byte(marshalLeaseEvent(&bare)), &m); err != nil {
		t.Fatalf("unmarshal bare: %v", err)
	}
	for _, k := range []string{"reason", "policy_step", "build_id"} {
		if _, ok := m[k]; ok {
			t.Fatalf("bare wire = %v, want %s omitted", m, k)
		}
	}
}
