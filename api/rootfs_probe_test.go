package api

import (
	"context"
	"strings"
	"testing"
	"time"
)

// newRootfsProbeService builds a Service with py-base seeded and the
// rootfs probe enabled, plus a granted lease.
func newRootfsProbeService(t *testing.T, persistent bool) (*Service, *testSub, *Lease) {
	t.Helper()
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	l, err := svc.grant(context.Background(), "consumer-a", "py-base", time.Minute, persistent, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	return svc, sub, l
}

// TestRootfsProbeRecoversFromCheckpoint: three consecutive EIO probe
// failures recover the lease from its last checkpoint, generation +1,
// through the dead sandbox's deletion first.
func TestRootfsProbeRecoversFromCheckpoint(t *testing.T) {
	svc, sub, l := newRootfsProbeService(t, true)
	ctx := context.Background()
	if _, err := svc.checkpointLease(ctx, l); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	oldSandbox := l.SandboxID
	buildBefore := l.LastCheckpointBuildID
	sub.rootfsFail[oldSandbox] = "dd: error reading '/dev/vda': Input/output error"

	// Two failures are not enough: the lease stays running.
	svc.probeRootfsLeases(ctx)
	svc.probeRootfsLeases(ctx)
	if l.State != "running" || l.Generation != 1 {
		t.Fatalf("after 2 failures lease = %s gen %d, want running gen 1", l.State, l.Generation)
	}
	if got := calls(sub.Fake, "Delete "+oldSandbox); got != 0 {
		t.Fatalf("Delete calls after 2 failures = %d, want 0", got)
	}

	// The third crosses the threshold: recover like a crash.
	svc.probeRootfsLeases(ctx)
	if l.State != "recovered" || !l.live() {
		t.Fatalf("after 3 failures lease state = %q, want live recovered", l.State)
	}
	if l.Generation != 2 {
		t.Fatalf("generation = %d, want 2", l.Generation)
	}
	if l.BuildID != buildBefore {
		t.Fatalf("recovered BuildID = %q, want the checkpoint build %q", l.BuildID, buildBefore)
	}
	if got := calls(sub.Fake, "Delete "+oldSandbox); got != 1 {
		t.Fatalf("Delete calls for the dead sandbox = %d, want 1", got)
	}
	if got := calls(sub.Fake, "Create "+oldSandbox); got != 2 {
		t.Fatalf("Create calls = %d, want 2 (grant + recovery)", got)
	}
	if _, err := svc.db.GetSandbox(ctx, oldSandbox); err != nil {
		t.Fatalf("recovered sandbox row missing: %v", err)
	}
}

// TestRootfsProbeLostWithoutCheckpoint: three consecutive EIO failures
// on a lease with no checkpoint mark it lost, with the root-disk detail
// on the lost event.
func TestRootfsProbeLostWithoutCheckpoint(t *testing.T) {
	svc, sub, l := newRootfsProbeService(t, false)
	ctx := context.Background()
	oldSandbox := l.SandboxID
	sub.rootfsFail[oldSandbox] = "dd: error reading '/dev/vda': Input/output error"
	esub := svc.Subscribe(EventFilter{})

	svc.probeRootfsLeases(ctx)
	svc.probeRootfsLeases(ctx)
	svc.probeRootfsLeases(ctx)
	esub.Close()

	if l.State != "lost" || l.live() {
		t.Fatalf("lease = %+v, want lost", l)
	}
	if _, err := svc.db.GetSandbox(ctx, oldSandbox); err == nil {
		t.Fatal("the lost lease's stale sandbox row survived")
	}
	if got := calls(sub.Fake, "Delete "+oldSandbox); got != 1 {
		t.Fatalf("Delete calls = %d, want 1", got)
	}
	events := collectEvents(esub.C)
	var lostDetails []string
	for _, ev := range events {
		if ev.LeaseID == l.ID && ev.Type == LeaseLost {
			lostDetails = append(lostDetails, ev.Detail)
		}
	}
	if len(lostDetails) < 2 {
		t.Fatalf("lost event details = %v, want the root-disk marker and the recovery reason", lostDetails)
	}
	if lostDetails[0] != "root disk unreadable (I/O errors)" {
		t.Fatalf("first lost detail = %q, want the root-disk marker", lostDetails[0])
	}
}

// TestRootfsProbeSuccessResetsFailures: a success between failures
// clears the count, so two failures on either side of it never reach
// the threshold.
func TestRootfsProbeSuccessResetsFailures(t *testing.T) {
	svc, sub, l := newRootfsProbeService(t, false)
	ctx := context.Background()
	sb := l.SandboxID

	sub.rootfsFail[sb] = "dd: error reading '/dev/vda': Input/output error"
	svc.probeRootfsLeases(ctx)
	svc.probeRootfsLeases(ctx)

	// A healthy read resets the consecutive count.
	delete(sub.rootfsFail, sb)
	svc.probeRootfsLeases(ctx)

	// Two more failures must not trip the three-in-a-row threshold.
	sub.rootfsFail[sb] = "dd: error reading '/dev/vda': Input/output error"
	svc.probeRootfsLeases(ctx)
	svc.probeRootfsLeases(ctx)

	if l.State != "running" || l.released {
		t.Fatalf("lease = %s (released=%v), want running after a reset", l.State, l.released)
	}
	if got := calls(sub.Fake, "Delete "+sb); got != 0 {
		t.Fatalf("Delete calls = %d, want 0", got)
	}
}

// TestRootfsProbeSkipsBusyAndDraining: a busy lease and a draining
// host are both left alone.
func TestRootfsProbeSkipsBusyAndDraining(t *testing.T) {
	svc, sub, l := newRootfsProbeService(t, false)
	ctx := context.Background()
	sb := l.SandboxID
	sub.rootfsFail[sb] = "dd: error reading '/dev/vda': Input/output error"

	// Busy: no probe exec at all.
	svc.store.mu.Lock()
	l.busy = true
	svc.store.mu.Unlock()
	svc.probeRootfsLeases(ctx)
	if got := sub.RootfsProbeCalls(); got != 0 {
		t.Fatalf("probe calls for a busy lease = %d, want 0", got)
	}
	svc.store.mu.Lock()
	l.busy = false
	svc.store.mu.Unlock()

	// Draining: same.
	svc.draining.Store(true)
	t.Cleanup(func() { svc.draining.Store(false) })
	svc.probeRootfsLeases(ctx)
	if got := sub.RootfsProbeCalls(); got != 0 {
		t.Fatalf("probe calls while draining = %d, want 0", got)
	}
	if l.State != "running" {
		t.Fatalf("lease state = %q, want running", l.State)
	}
}

// TestRootfsProbeRecentExecSkips: an exec that succeeded within the
// probe interval proves liveness, so the next pass runs no probe.
func TestRootfsProbeRecentExecSkips(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	ctx := context.Background()
	l, err := svc.grant(ctx, "consumer-a", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	// A real exec through the API records the success.
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+l.ID+"/exec", "token-a", map[string]any{"cmd": "true"})
	if resp.StatusCode != 200 {
		t.Fatalf("exec = %d (%v), want 200", resp.StatusCode, body)
	}
	sub.rootfsFail[l.SandboxID] = "dd: error reading '/dev/vda': Input/output error"

	svc.probeRootfsLeases(ctx)
	if got := sub.RootfsProbeCalls(); got != 0 {
		t.Fatalf("probe calls after a recent successful exec = %d, want 0", got)
	}
	if l.State != "running" {
		t.Fatalf("lease state = %q, want running", l.State)
	}
}

// TestRootfsProbeAllTransportFailingDoesNothing: when every probe in a
// pass fails at the transport (the orchestrator is unreachable), no
// failure is counted and no lease is touched.
func TestRootfsProbeAllTransportFailingDoesNothing(t *testing.T) {
	svc, sub, l1 := newRootfsProbeService(t, false)
	ctx := context.Background()
	l2, err := svc.grant(ctx, "consumer-a", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant second: %v", err)
	}
	sub.rootfsErr[l1.SandboxID] = true
	sub.rootfsErr[l2.SandboxID] = true

	for i := 0; i < rootfsProbeFailuresThreshold+1; i++ {
		svc.probeRootfsLeases(ctx)
	}
	if l1.State != "running" || l2.State != "running" {
		t.Fatalf("states = %s, %s, want running", l1.State, l2.State)
	}
	if got := calls(sub.Fake, "Delete "+l1.SandboxID); got != 0 {
		t.Fatalf("Delete calls = %d, want 0", got)
	}
	if got := calls(sub.Fake, "Delete "+l2.SandboxID); got != 0 {
		t.Fatalf("Delete calls = %d, want 0", got)
	}
}

// TestRootfsProbeTimeoutCountsAsFailure: a probe exec that returns a
// transport error (a 10 s timeout) counts toward the threshold when
// another lease in the same pass answered with an I/O error, which
// proves the orchestrator itself is reachable.
func TestRootfsProbeTimeoutCountsAsFailure(t *testing.T) {
	svc, sub, l := newRootfsProbeService(t, true)
	ctx := context.Background()
	if _, err := svc.checkpointLease(ctx, l); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	// A second lease answers EIO: the pass is mixed, not an
	// orchestrator-wide outage.
	dead, err := svc.grant(ctx, "consumer-a", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant second: %v", err)
	}
	sub.rootfsFail[dead.SandboxID] = "dd: error reading '/dev/vda': Input/output error"
	sub.rootfsErr[l.SandboxID] = true

	for i := 0; i < rootfsProbeFailuresThreshold; i++ {
		svc.probeRootfsLeases(ctx)
	}
	// The transitively-failing lease is recovered from its checkpoint;
	// the EIO one has no checkpoint and goes through recovery the same
	// way (untested here beyond not panicking).
	if l.State != "recovered" || l.Generation != 2 {
		t.Fatalf("lease = %s gen %d, want recovered gen 2", l.State, l.Generation)
	}
}

// TestRootfsProbeIgnoresNonIOError: a non-zero exit that is not an I/O
// error (the guest answered) is not a failure.
func TestRootfsProbeIgnoresNonIOError(t *testing.T) {
	svc, sub, l := newRootfsProbeService(t, false)
	ctx := context.Background()
	sub.rootfsFail[l.SandboxID] = "dd: failed to open '/dev/vda': No such file or directory"

	for i := 0; i < rootfsProbeFailuresThreshold+1; i++ {
		svc.probeRootfsLeases(ctx)
	}
	if l.State != "running" {
		t.Fatalf("lease state = %q, want running", l.State)
	}
}

// TestRootfsProbeDisabled: ROOTFS_PROBE_SECS=0 runs no probe.
func TestRootfsProbeDisabled(t *testing.T) {
	svc, sub, l := newRootfsProbeService(t, false)
	svc.SetRootfsProbe(0)
	sub.rootfsFail[l.SandboxID] = "dd: error reading '/dev/vda': Input/output error"
	svc.probeRootfsLeases(context.Background())
	if got := sub.RootfsProbeCalls(); got != 0 {
		t.Fatalf("probe calls while disabled = %d, want 0", got)
	}
}

// TestRootfsProbeScriptReadsRootDevice: the probe script bypasses the
// page cache and falls back to /dev/vda.
func TestRootfsProbeScriptReadsRootDevice(t *testing.T) {
	for _, want := range []string{"findmnt -no SOURCE /", "iflag=direct", "/dev/vda", "skip="} {
		if !strings.Contains(rootfsProbe, want) {
			t.Errorf("probe script does not contain %q:\n%s", want, rootfsProbe)
		}
	}
}
