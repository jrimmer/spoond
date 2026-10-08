package api

// spoond-d76: follow-ups to the spoond-775 release race. A release that
// lands between a pause's build write and its suspended save must not
// let the pause report success (and its caller count a suspension, emit
// a suspended event or credit memory twice), a checkpoint's resume-fresh
// sandbox must be stopped when the release's own delete lost the race,
// and the GC/drain bookkeeping must not resurrect or misreport a
// released lease.

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

// releaseDuringPause arms the pauseBeforeSuspend hook so that a release
// runs in the window between a pause's build write and its store-lock
// re-check. It returns a function that clears the hook.
func releaseDuringPause(svc *Service, reason string) func() {
	svc.pauseBeforeSuspend = func(l *Lease) {
		svc.pauseBeforeSuspend = nil
		svc.releaseBecause(context.Background(), l, reason)
	}
	return func() { svc.pauseBeforeSuspend = nil }
}

// TestPauseReleaseRaceUnderLock: a release between a pause's build write
// and its suspended save makes the pause report errLeaseReleased. No
// suspended event is emitted, the lease is not marked suspended, and its
// memory is credited exactly once (release credited it; the pause must
// not credit it a second time).
func TestPauseReleaseRaceUnderLock(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	svc.SetMetrics(metrics.NewBackendMetrics())
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	// Fill the cached NodeInfo so credits are observable. 2048 MiB at a
	// 2 MiB hugepage is 1024 pages.
	svc.nodeInfoMu.Lock()
	svc.nodeInfoCache = substrate.NodeInfo{Status: "healthy", HugepageSizeBytes: 2 << 20, HugepagesUsed: 4096}
	svc.nodeInfoAt = time.Now()
	svc.nodeInfoMu.Unlock()

	all := svc.Subscribe(EventFilter{LeaseID: l.ID})
	defer all.Close()
	clearHook := releaseDuringPause(svc, "released through the API")
	defer clearHook()

	if _, err := svc.pauseLease(ctx, l, false); !errors.Is(err, errLeaseReleased) {
		t.Fatalf("pause error = %v, want errLeaseReleased", err)
	}
	if l.Suspended {
		t.Fatal("a released lease was marked suspended by the pause")
	}

	all.Close()
	events := collectEvents(all.C)
	for _, ev := range events {
		if ev.Type == LeaseSuspended {
			t.Fatalf("a suspended event was emitted for a released lease: %v", eventTypes(events))
		}
	}
	// Exactly one credit: 4096 - 1024.
	svc.nodeInfoMu.Lock()
	used := svc.nodeInfoCache.HugepagesUsed
	svc.nodeInfoMu.Unlock()
	if used != 3072 {
		t.Fatalf("HugepagesUsed = %d after the race, want 3072 (credited once)", used)
	}
	if n := counterValue(t, svc.metrics.IdleSuspendsTotal); n != 0 {
		t.Fatalf("spoond_idle_suspends_total = %g, want 0", n)
	}
}

// TestPreemptReleaseRaceNoMetric: preemption skips a lease released while
// its pause ran: no preemption stamp, counter or event.
func TestPreemptReleaseRaceNoMetric(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	svc.SetMetrics(metrics.NewBackendMetrics())
	svc.cfg.BurstReserveMiB = 0
	ctx := context.Background()

	l, err := svc.grantLease(ctx, leaseRequest{owner: "c", image: "py-base", ttl: time.Minute, persistent: true, burst: true})
	if err != nil {
		t.Fatalf("grant burst: %v", err)
	}
	all := svc.Subscribe(EventFilter{LeaseID: l.ID})
	defer all.Close()
	clearHook := releaseDuringPause(svc, "released through the API")
	defer clearHook()

	err = svc.preemptLease(ctx, l, "guaranteed-owner")
	if !errors.Is(err, errLeaseReleased) {
		t.Fatalf("preemptLease error = %v, want errLeaseReleased", err)
	}
	if n := counterValue(t, svc.metrics.PreemptionsTotal); n != 0 {
		t.Fatalf("spoond_preemptions_total = %g, want 0", n)
	}
	if ids := preemptedIDs(svc); len(ids) != 0 {
		t.Fatalf("lease was marked preempted after release: %v", ids)
	}
	all.Close()
	for _, ev := range collectEvents(all.C) {
		if ev.Type == LeasePreempted {
			t.Fatal("a preempted event was emitted for a released lease")
		}
	}
}

// TestIdleSuspendReleaseRaceNoMetric: the idle sweep does not count a
// suspension for a lease released while its pause ran.
func TestIdleSuspendReleaseRaceNoMetric(t *testing.T) {
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

	clearHook := releaseDuringPause(svc, "released through the API")
	defer clearHook()
	svc.suspendIdleLeases(ctx, time.Now())

	if n := counterValue(t, svc.metrics.IdleSuspendsTotal); n != 0 {
		t.Fatalf("spoond_idle_suspends_total = %g, want 0", n)
	}
	if l.Suspended {
		t.Fatal("the idle sweep suspended a released lease")
	}
}

// TestCheckpointReleaseRaceStopsResumedGuest: a checkpoint whose
// resume-fresh started a sandbox under the source id after the release's
// own delete must stop that sandbox (detached, bounded) and drop its
// row, so the released guest does not run on holding hugepages.
func TestCheckpointReleaseRaceStopsResumedGuest(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	sbID := l.SandboxID

	// The release's own delete lands while the checkpoint is in flight,
	// then the orchestrator resumes a fresh sandbox under the same id;
	// List reports it after the unblock (resume-fresh).
	started := make(chan struct{})
	unblock := make(chan struct{})
	sub.checkpointFn = func(ctx context.Context, sandboxID string) (string, substrate.BuildRefs, error) {
		close(started)
		<-unblock
		return "b-ckpt-race", substrate.BuildRefs{}, nil
	}
	var mu sync.Mutex
	var deletes []string
	sub.deleteFn = func(ctx context.Context, id string) error {
		mu.Lock()
		deletes = append(deletes, id)
		mu.Unlock()
		return sub.Fake.Delete(ctx, id)
	}
	sub.listFn = func(ctx context.Context) ([]substrate.Sandbox, error) {
		return []substrate.Sandbox{{ID: sbID, BuildID: "b-ckpt-race"}}, nil
	}
	t.Cleanup(func() {
		sub.checkpointFn = nil
		sub.deleteFn = nil
		sub.listFn = nil
	})

	done := make(chan error, 1)
	go func() {
		_, err := svc.checkpointLeaseBusy(ctx, l, false)
		done <- err
	}()
	<-started

	svc.releaseBecause(ctx, l, "released through the API")
	close(unblock)

	if err := <-done; !errors.Is(err, errLeaseReleased) {
		t.Fatalf("checkpoint error = %v, want errLeaseReleased", err)
	}
	// The release deleted once and afterCheckpoint stopped the resumed
	// guest a second time.
	mu.Lock()
	n := len(deletes)
	mu.Unlock()
	if n < 2 {
		t.Fatalf("sandbox %s delete calls = %d, want the release's and the checkpoint's cleanup", sbID, n)
	}
	if _, err := db.GetSandboxByLease(ctx, l.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("released lease still has a sandboxes row: %v", err)
	}
}

// TestUpdateLeaseLostAtDoesNotResurrectRow: the GC's stored-only stamp is
// an UPDATE, so a lease row a concurrent release deleted between the GC's
// list and its stamp is not re-inserted as a phantom.
func TestUpdateLeaseLostAtDoesNotResurrectRow(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	now := time.Now()
	row := store.LeaseRow{
		ID: "l-race", Owner: "c", Image: "py-base", SandboxID: "sb-x",
		CreatedAt: now, ExpiresAt: now.Add(time.Minute), LastActive: now,
		State: "lost", Class: ClassGuaranteed,
	}
	if err := db.UpsertLease(ctx, row); err != nil {
		t.Fatalf("plant lost row: %v", err)
	}
	// The release wins between the GC's list and its stamp.
	if err := db.DeleteLease(ctx, "l-race"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	svc.stampLostAt(ctx, row, now)
	if _, err := db.GetLease(ctx, "l-race"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stampLostAt re-inserted a released row: %v", err)
	}
}

// TestDrainSkipsReleasedLease: a lease released while its drain pause ran
// is skipped, not listed as a drain failure.
func TestDrainSkipsReleasedLease(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	if _, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil); err != nil {
		t.Fatalf("grant: %v", err)
	}
	clearHook := releaseDuringPause(svc, "released through the API")
	defer clearHook()

	res, err := svc.drain(ctx)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("drain reported failures for a released lease: %+v", res.Failed)
	}
	if res.Paused != 0 {
		t.Fatalf("drain counted %d pauses for a released lease", res.Paused)
	}
}
