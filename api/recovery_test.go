package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/substrate"
)

// TestReconcileCrashRecoversFromCheckpoint: after the orchestrator dies
// (sandboxes vanish without Deletes), a lease with a checkpoint is
// resumed from it with the same sandbox id and becomes recovered; a
// lease without one becomes lost and its stale sandbox row goes.
func TestReconcileCrashRecoversFromCheckpoint(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	ck, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant checkpointed: %v", err)
	}
	if _, err := svc.checkpointLease(ctx, ck); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	bare, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant bare: %v", err)
	}
	if _, err := db.GetSandbox(ctx, bare.SandboxID); err != nil {
		t.Fatalf("bare lease sandbox row missing: %v", err)
	}

	// The crash: every running sandbox disappears without a Delete.
	sub.Fake.Kill(ck.SandboxID)
	sub.Fake.Kill(bare.SandboxID)

	createsBefore := len(sub.Fake.CallLog())
	summary := svc.reconcileCrash(ctx)
	if summary.Recovered != 1 || summary.Lost != 1 {
		t.Fatalf("summary = %+v, want {Recovered:1, Lost:1}", summary)
	}

	// The recovery create carried the SAME sandbox id.
	var sawResume bool
	for _, c := range sub.Fake.CallLog()[createsBefore:] {
		if c == "Create "+ck.SandboxID {
			sawResume = true
		}
		if c == "Create "+bare.SandboxID {
			t.Fatalf("the checkpoint-less lease was recreated: %v", sub.Fake.CallLog()[createsBefore:])
		}
	}
	if !sawResume {
		t.Fatalf("no create of the checkpointed lease's sandbox id: %v", sub.Fake.CallLog()[createsBefore:])
	}

	if ck.State != "recovered" {
		t.Fatalf("checkpointed lease state = %q, want recovered", ck.State)
	}
	if !ck.live() {
		t.Fatal("recovered lease must be live")
	}
	if ck.BuildID != ck.LastCheckpointBuildID {
		t.Fatalf("recovered BuildID = %q, want the checkpoint build %q", ck.BuildID, ck.LastCheckpointBuildID)
	}
	if ck.RecoveredFrom.IsZero() || !ck.RecoveredFrom.Equal(ck.LastCheckpointAt) {
		t.Fatalf("RecoveredFrom = %v, want the checkpoint time %v", ck.RecoveredFrom, ck.LastCheckpointAt)
	}
	if bare.State != "lost" || bare.live() {
		t.Fatalf("checkpoint-less lease = %+v, want lost", bare)
	}
	if _, err := db.GetSandbox(ctx, bare.SandboxID); err == nil {
		t.Fatal("the lost lease's stale sandbox row survived")
	}
	// The recovered lease has a fresh sandboxes row for its sandbox id.
	if row, err := db.GetSandbox(ctx, ck.SandboxID); err != nil {
		t.Fatalf("recovered sandbox row: %v", err)
	} else if row.BuildID != ck.LastCheckpointBuildID {
		t.Fatalf("recovered sandbox row build = %q, want %q", row.BuildID, ck.LastCheckpointBuildID)
	}
}

// TestReconcileCrashListFailureChangesNothing: a List failure never
// marks leases lost.
func TestReconcileCrashListFailureChangesNothing(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	sub.FailCall("List", 1, context.DeadlineExceeded)

	summary := svc.reconcileCrash(ctx)
	if summary.Recovered != 0 || summary.Lost != 0 {
		t.Fatalf("summary = %+v, want zeros", summary)
	}
	if l.State != "running" {
		t.Fatalf("lease state = %q after a list failure, want running", l.State)
	}
}

// TestLostLeaseExec409: a lost lease answers exec with 409 code
// lease_lost and the reason, and GET names the reason.
func TestLostLeaseExec409(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)

	svc.store.mu.Lock()
	svc.store.leases[id].State = "lost"
	svc.store.leases[id].LostReason = "substrate crashed"
	svc.store.mu.Unlock()

	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/exec", "token-a", map[string]any{"cmd": "echo hi"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("exec on a lost lease = %d (%v), want 409", resp.StatusCode, body)
	}
	if body["code"] != "lease_lost" {
		t.Fatalf("code = %v, want lease_lost", body["code"])
	}
	msg, _ := body["error"].(string)
	for _, want := range []string{"substrate lost this lease's sandbox", "substrate crashed", "DELETE the lease to free its quota"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error = %q, want it to contain %q", msg, want)
		}
	}
	// GET shows the lost state and the reason.
	_, detail := doReq(t, "GET", ts.URL+"/api/sandboxes/"+id, "token-a", nil)
	if detail["state"] != "lost" || detail["lost_reason"] != "substrate crashed" {
		t.Fatalf("detail = %v, want state lost and lost_reason substrate crashed", detail)
	}
}

// TestRecoveredLeaseStillServed: a recovered lease behaves exactly like
// a running one, and its state shows recovered until suspend or restart
// resets it to the normal states.
func TestRecoveredLeaseStillServed(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "consumer-a", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.checkpointLease(ctx, l); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	sub.Fake.Kill(l.SandboxID)
	if summary := svc.reconcileCrash(ctx); summary.Recovered != 1 {
		t.Fatalf("summary = %+v, want one recovery", summary)
	}

	// Exec works on a recovered lease...
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+l.ID+"/exec", "token-a", map[string]any{"cmd": "echo hi"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exec on a recovered lease = %d (%v), want 200", resp.StatusCode, body)
	}
	// ...and the detail shows the recovered state.
	resp, detail := doReq(t, "GET", ts.URL+"/api/sandboxes/"+l.ID, "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get recovered lease = %d", resp.StatusCode)
	}
	if detail["state"] != "recovered" {
		t.Fatalf("detail state = %v, want recovered", detail["state"])
	}
	// The list shows it too.
	_, list := doReq(t, "GET", ts.URL+"/api/sandboxes", "token-a", nil)
	rows := list["sandboxes"].([]any)
	var listed bool
	for _, r := range rows {
		row := r.(map[string]any)
		if row["id"] == l.ID {
			listed = true
			if row["state"] != "recovered" {
				t.Fatalf("list row state = %v, want recovered", row["state"])
			}
		}
	}
	if !listed {
		t.Fatalf("recovered lease missing from the list: %v", list)
	}

	// Suspend resets the state to the normal lifecycle.
	if _, err := svc.suspend(ctx, "consumer-a", l.ID); err != nil {
		t.Fatalf("suspend recovered lease: %v", err)
	}
	if _, err := svc.resume(ctx, "consumer-a", l.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if l.State != "running" {
		t.Fatalf("state after suspend+resume = %q, want running", l.State)
	}
}

// TestReconcileCrashRootfsProbeDoesNotRace: while a crash reconcile is
// recreating a lease's sandbox (a multi-minute recovery in production),
// a rootfs probe pass must not count transport failures against it or
// recover it a second time. The reconcile marks the lease busy, and the
// probe's recovery takes the busy flag before acting.
func TestReconcileCrashRootfsProbeDoesNotRace(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.checkpointLease(ctx, l); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	// The crash: the sandbox vanishes, so reconcileCrash recovers it.
	sub.Fake.Kill(l.SandboxID)
	// Its recovery create fails on first use, while the probe pass runs
	// from inside that create: the lease is mid-recovery and busy.
	sub.rootfsErr[l.SandboxID] = true
	svc.rootfsProbeMu.Lock()
	svc.rootfsProbeFails[l.ID] = &rootfsProbeFailure{sandboxID: l.SandboxID, count: rootfsProbeFailuresThreshold - 1}
	svc.rootfsProbeMu.Unlock()

	probed := false
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.SandboxID == l.SandboxID && !probed {
			probed = true
			svc.probeRootfsLeases(ctx)
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	summary := svc.reconcileCrash(ctx)
	if summary.Recovered != 1 || summary.Lost != 0 {
		t.Fatalf("summary = %+v, want {Recovered:1, Lost:0}", summary)
	}
	if l.State != "recovered" || l.Generation != 2 {
		t.Fatalf("lease = %s gen %d, want recovered gen 2", l.State, l.Generation)
	}
	// The probe count was dropped when it found the lease busy; it did
	// not fire a second recovery (which would bump the generation again
	// and create a third sandbox).
	if got := calls(sub.Fake, "Create "+l.SandboxID); got != 2 {
		t.Fatalf("Create calls = %d, want 2 (grant + the reconcile's recovery)", got)
	}
}
