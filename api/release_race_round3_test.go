package api

// spoond-15i: round-3 follow-ups to the release races. A checkpoint's
// list failure must still stop a resumed guest, a release after a
// paused pause must not bump the idle/held counters or emit events, a
// recovery must not delete a half-started sandbox a second time or call
// it a resume cleanup, and the fake's NodeInfo must record under its
// lock.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/metrics"
	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
)

// waitSandboxGone polls until the bounded deletes have run. The
// checkpoint cleanup is detached and retried, so the test waits for the
// sandbox to leave the fake rather than racing it. It reads the
// underlying fake directly, not testSub.List (a test may override it).
func waitSandboxGone(t *testing.T, sub *testSub, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sbs, err := sub.Fake.List(context.Background())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		gone := true
		for _, live := range sbs {
			if live.ID == id {
				gone = false
				break
			}
		}
		if gone {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("sandbox %s is still live after the checkpoint cleanup", id)
}

// TestCheckpointReleaseRaceListFailsStillDeletes: a checkpoint whose
// List fails (the client is gone, the context was cancelled) and whose
// lease is then released must still stop the resumed guest under the
// source sandbox id. List reports nothing, so afterCheckpoint must
// delete the id directly rather than silently leaving the guest
// running (spoond-15i).
//
// The release's own delete runs first and would delete the pre-checkpoint
// sandbox; to pin the afterCheckpoint cleanup rather than that one, the
// checkpoint re-creates the sandbox in the fake after the release, as the
// resume-fresh path does, and only the deletes after the release count.
func TestCheckpointReleaseRaceListFailsStillDeletes(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	sbID := l.SandboxID

	started := make(chan struct{})
	unblock := make(chan struct{})
	sub.checkpointFn = func(ctx context.Context, sandboxID string) (string, substrate.BuildRefs, error) {
		close(started)
		<-unblock
		// The checkpoint's resume-fresh path brings the guest back under
		// the source id after the release already deleted the old
		// sandbox. Re-create it here so the release's delete cannot be
		// mistaken for the cleanup this test pins.
		if _, err := sub.Fake.Create(context.Background(), substrate.CreateRequest{
			SandboxID: sandboxID, TemplateID: l.TemplateID, BuildID: l.BuildID,
		}); err != nil {
			t.Errorf("re-create resumed sandbox: %v", err)
		}
		return "b-ckpt-listfail", substrate.BuildRefs{}, nil
	}
	sub.listFn = func(ctx context.Context) ([]substrate.Sandbox, error) {
		return nil, errors.New("client gone")
	}
	var mu sync.Mutex
	var deletes []string
	sub.deleteFn = func(ctx context.Context, id string) error {
		mu.Lock()
		deletes = append(deletes, id)
		mu.Unlock()
		return sub.Fake.Delete(ctx, id)
	}
	t.Cleanup(func() {
		sub.checkpointFn = nil
		sub.listFn = nil
		sub.deleteFn = nil
	})

	done := make(chan error, 1)
	go func() {
		_, err := svc.checkpointLeaseBusy(ctx, l, false)
		done <- err
	}()
	<-started

	svc.releaseBecause(ctx, l, "released through the API")
	mu.Lock()
	beforeRelease := len(deletes)
	mu.Unlock()
	close(unblock)

	if err := <-done; !errors.Is(err, errLeaseReleased) {
		t.Fatalf("checkpoint error = %v, want errLeaseReleased", err)
	}
	waitSandboxGone(t, sub, sbID)
	mu.Lock()
	got := len(deletes)
	mu.Unlock()
	if got <= beforeRelease {
		t.Fatalf("sandbox %s was not deleted after the release and a failed List (deletes %d, before %d)", sbID, got, beforeRelease)
	}
	if _, err := db.GetSandboxByLease(ctx, l.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("released lease still has a sandboxes row: %v", err)
	}
}

// TestIdleSuspendReleaseAfterPauseNoMetric: a release that lands after
// pauseLease has returned success but before recordIdleSuspend bumps no
// spoond_idle_suspends_total and emits no idle_suspended event.
func TestIdleSuspendReleaseAfterPauseNoMetric(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	svc.SetMetrics(metrics.NewBackendMetrics())
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.setIdlePolicy(l, 60); err != nil {
		t.Fatalf("setIdlePolicy: %v", err)
	}
	svc.store.mu.Lock()
	l.LastActive = time.Now().Add(-time.Hour)
	svc.store.mu.Unlock()

	// The release lands after the pause commits its suspended save; the
	// idle sweep's own record then sees a released lease. The sweep calls
	// pauseLease then recordIdleSuspend synchronously with its re-check
	// under the lock before the pause, so the release cannot be seen
	// there; recordIdleSuspend must catch it.
	all := svc.Subscribe(EventFilter{LeaseID: l.ID})
	defer all.Close()

	if _, err := svc.pauseLease(ctx, l, false); err != nil {
		t.Fatalf("pause: %v", err)
	}
	svc.releaseBecause(ctx, l, "released through the API")
	svc.recordIdleSuspend(l, time.Now().Add(-time.Hour), time.Now())

	if n := counterValue(t, svc.metrics.IdleSuspendsTotal); n != 0 {
		t.Fatalf("spoond_idle_suspends_total = %g, want 0", n)
	}
	all.Close()
	for _, ev := range collectEvents(all.C) {
		if ev.Type == LeaseIdleSuspended {
			t.Fatal("an idle_suspended event was emitted for a released lease")
		}
	}
}

// TestHeldActionReleaseAfterPauseNoMetric: the held rules do not bump
// spoond_held_actions_total or emit held_action for a lease released
// after the rule's pause succeeded.
func TestHeldActionReleaseAfterPauseNoMetric(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	svc.SetMetrics(metrics.NewBackendMetrics())
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "ci-job", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	all := svc.Subscribe(EventFilter{LeaseID: l.ID})
	defer all.Close()

	svc.releaseBecause(ctx, l, "released through the API")
	svc.store.mu.Lock()
	svc.heldAction(ctx, l, heldRuleIdle, heldActionSuspendIdle, "idle", time.Now())
	svc.store.mu.Unlock()

	if n := heldCounter(t, svc, heldRuleIdle, heldActionSuspendIdle); n != 0 {
		t.Fatalf("held_actions_total{idle,suspend_idle} = %g, want 0", n)
	}
	all.Close()
	for _, ev := range collectEvents(all.C) {
		if ev.Type == LeaseHeldAction {
			t.Fatal("a held_action event was emitted for a released lease")
		}
	}
}

// TestRecoveryDoesNotDoubleDeleteHalfSandbox: a recovery whose Create
// fails deletes the half-started sandbox exactly once (createSandbox's
// own cleanup) and logs no resume cleanup line. The recovery pass must
// not issue its own second delete (spoond-15i).
func TestRecoveryDoesNotDoubleDeleteHalfSandbox(t *testing.T) {
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

	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume {
			return substrate.Sandbox{}, errors.New("envd start failed")
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	before := calls(sub.Fake, "Delete")
	out := svc.recoverOneLease(ctx, l)
	if out.Result != "recovering" {
		t.Fatalf("recovery result = %q, want recovering", out.Result)
	}
	if got := calls(sub.Fake, "Delete") - before; got != 1 {
		t.Fatalf("Delete calls = %d, want 1 (only createSandbox's cleanup)", got)
	}
}

// TestFakeNodeInfoFuncRecordsUnderLock runs a NodeInfo override
// concurrently with a Delete so the race detector can catch an unlocked
// Calls append (spoond-15i).
func TestFakeNodeInfoFuncRecordsUnderLock(t *testing.T) {
	sub := newTestSub()
	sub.SetNodeInfoFunc(func(context.Context) (substrate.NodeInfo, error) {
		return substrate.NodeInfo{Status: "healthy"}, nil
	})
	defer sub.SetNodeInfoFunc(nil)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = sub.NodeInfo(context.Background())
		}()
		go func() {
			defer wg.Done()
			_ = sub.Delete(context.Background(), "sb-x")
		}()
	}
	wg.Wait()
}
