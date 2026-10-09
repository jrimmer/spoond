package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/substrate"
)

// B1/B2 guard tests (spoond-63a round 2): a lost transition never deletes
// a guest an owner operation is bringing back, and a create that finishes
// after its lease was released is stopped without resurrecting the lease.
// The preempt-resume loss guard tests went with the background preempt
// auto-resume (#145 D2): spoond no longer loses a lease because a resume
// failed — the error goes to the caller.

// TestUndrainLossAllowed is the direct rule test for the undrain loss
// block (B1): only a suspended, non-busy, non-released, not-already-lost
// lease may be lost there.
func TestUndrainLossAllowed(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name     string
		shape    func(*Lease)
		canLose  bool
		released bool
		wasLost  bool
	}{
		{"suspended idle", func(l *Lease) { l.setState("suspended") }, true, false, false},
		{"busy", func(l *Lease) { l.setState("suspended"); l.busy = true }, false, false, false},
		{"running", func(l *Lease) { l.setState("running") }, false, false, false},
		{"released", func(l *Lease) { l.setState("suspended"); l.released = true }, false, true, false},
		{"already lost", func(l *Lease) { l.setState("lost"); l.LostAt = now }, false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := &Lease{}
			tc.shape(l)
			released, alreadyLost, canLose := undrainLossAllowed(l)
			if canLose != tc.canLose || released != tc.released || alreadyLost != tc.wasLost {
				t.Fatalf("undrainLossAllowed = (released=%v, alreadyLost=%v, canLose=%v), want (%v, %v, %v)",
					released, alreadyLost, canLose, tc.released, tc.wasLost, tc.canLose)
			}
		})
	}
}

// TestUndrainSkipsBusyLease: a drained lease another operation holds
// (busy) is not lost by the undrain, so its guest is not deleted out from
// under the operation. The errLeaseBusy skip and the loss block's own
// busy check (TestUndrainLossAllowed) together keep an owner resume from
// being lost (B1).
func TestUndrainSkipsBusyLease(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	svc.cfg.UndrainResumeRetries = 0

	// A lease with a running sandbox, marked draining and busy: an owner
	// operation holds it while the undrain runs.
	target, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	sandbox := target.SandboxID
	svc.store.mu.Lock()
	target.Drained = true
	target.busy = true
	svc.saveLeaseLocked(target)
	svc.store.mu.Unlock()
	t.Cleanup(func() {
		svc.store.mu.Lock()
		target.busy = false
		svc.store.mu.Unlock()
	})

	res := svc.undrain(ctx)
	if len(res.Failed) != 1 {
		t.Fatalf("undrain = %+v, want one failed entry", res)
	}
	if target.State == "lost" {
		t.Fatal("a busy drained lease was marked lost")
	}
	if svc.lookupAny(target.ID) == nil {
		t.Fatal("the busy lease row was dropped")
	}
	if !sandboxOnFake(t, sub, sandbox) {
		t.Fatalf("the busy lease's sandbox %s was deleted", sandbox)
	}
}

// TestCreateAfterReleaseStopsSandbox: a resume whose create finishes
// after the lease was released stops the fresh sandbox and saves nothing
// (spoond-775, spoond-63a B2).
func TestCreateAfterReleaseStopsSandbox(t *testing.T) {
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
	sandbox := l.SandboxID

	entered := make(chan struct{})
	proceed := make(chan struct{})
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		close(entered)
		<-proceed
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	done := make(chan error, 1)
	go func() {
		_, err := svc.resumeLease(ctx, l)
		done <- err
	}()
	<-entered

	// Release the lease while its resume create is in flight.
	svc.release(ctx, l)
	close(proceed)
	err = <-done
	if !errors.Is(err, errLeaseReleased) {
		t.Fatalf("resume error = %v, want errLeaseReleased", err)
	}
	if _, ok := svc.store.leases[l.ID]; ok {
		t.Fatal("the released lease was resurrected by a late resume")
	}
	if sandboxOnFake(t, sub, sandbox) {
		t.Fatalf("the fresh sandbox %s was left running after the release", sandbox)
	}
}

// TestRecoveryCreateAfterReleaseStopsSandbox: a recovery create that
// finishes after the lease was released stops the fresh sandbox and skips
// every save (spoond-775, spoond-63a B2).
func TestRecoveryCreateAfterReleaseStopsSandbox(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l := recoverTargetCheckpoint(t, svc, sub, ctx)
	sandbox := l.SandboxID

	entered := make(chan struct{})
	proceed := make(chan struct{})
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume && req.SandboxID == sandbox {
			close(entered)
			<-proceed
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	done := make(chan recoverySummary, 1)
	go func() {
		done <- svc.reconcileCrash(ctx)
	}()
	<-entered

	svc.release(ctx, l)
	close(proceed)
	out := <-done
	if out.Lost != 0 {
		t.Fatalf("summary = %+v, want no loss for a released lease", out)
	}
	if _, ok := svc.store.leases[l.ID]; ok {
		t.Fatal("the released lease was resurrected by a late recovery")
	}
	if sandboxOnFake(t, sub, sandbox) {
		t.Fatalf("the fresh sandbox %s was left running after the release", sandbox)
	}
}

// TestRestoreCreateAfterReleaseStopsSandbox: a restore create that
// finishes after the lease was released stops the fresh sandbox and skips
// every save.
func TestRestoreCreateAfterReleaseStopsSandbox(t *testing.T) {
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
	b, err := svc.db.GetBuild(ctx, l.LastCheckpointBuildID)
	if err != nil {
		t.Fatalf("get build: %v", err)
	}
	old := l.SandboxID

	entered := make(chan struct{})
	proceed := make(chan struct{})
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if !req.Resume {
			close(entered)
			<-proceed
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	done := make(chan error, 1)
	go func() {
		done <- svc.restoreBusy(ctx, l, b)
	}()
	<-entered

	svc.release(ctx, l)
	close(proceed)
	err = <-done
	if !errors.Is(err, errLeaseReleased) {
		t.Fatalf("restore error = %v, want errLeaseReleased", err)
	}
	// The old sandbox was stopped by the release; the fresh one too.
	if sandboxOnFake(t, sub, old) {
		t.Fatalf("the old sandbox %s survived the release", old)
	}
	if ids := sub.sandboxesLive(t); len(ids) != 0 {
		t.Fatalf("sandboxes left running after the release: %v", ids)
	}
}

// TestRestartColdCreateAfterReleaseStopsSandbox: a cold restart create
// that finishes after the lease was released stops the fresh sandbox and
// skips every save.
func TestRestartColdCreateAfterReleaseStopsSandbox(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	old := l.SandboxID

	entered := make(chan struct{})
	proceed := make(chan struct{})
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if !req.Resume {
			close(entered)
			<-proceed
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	done := make(chan error, 1)
	go func() {
		_, err := svc.restart(ctx, "c", l.ID, "cold")
		done <- err
	}()
	<-entered

	svc.release(ctx, l)
	close(proceed)
	err = <-done
	if !errors.Is(err, errLeaseReleased) {
		t.Fatalf("restart error = %v, want errLeaseReleased", err)
	}
	if sandboxOnFake(t, sub, old) {
		t.Fatalf("the old sandbox %s survived the release", old)
	}
	if ids := sub.sandboxesLive(t); len(ids) != 0 {
		t.Fatalf("sandboxes left running after the release: %v", ids)
	}
}

// TestRestartRecheckAfterReleaseStopsSandbox (G2): a release that lands
// between restart's early released check and its store lock still stops
// the fresh sandbox and writes no lease row back. The hook fires exactly
// in that window.
func TestRestartRecheckAfterReleaseStopsSandbox(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	sandbox := l.SandboxID

	svc.restartBeforeRecheck = func() {
		// Land the release after restart's first released check, before
		// the store lock. The fresh sandbox already exists.
		svc.release(ctx, l)
	}
	t.Cleanup(func() { svc.restartBeforeRecheck = nil })

	_, err = svc.restart(ctx, "c", l.ID, "")
	if !errors.Is(err, errLeaseReleased) {
		t.Fatalf("restart error = %v, want errLeaseReleased", err)
	}
	if _, ok := svc.store.leases[l.ID]; ok {
		t.Fatal("the released lease was resurrected by restart's late save")
	}
	if sandboxOnFake(t, sub, sandbox) {
		t.Fatalf("the fresh sandbox %s was left running after the release", sandbox)
	}
	if ids := sub.sandboxesLive(t); len(ids) != 0 {
		t.Fatalf("sandboxes left running after the release: %v", ids)
	}
}
