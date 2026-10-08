package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
)

// The spoond-775 class: an asynchronous operation (checkpoint, pause,
// resume, restore, recovery) that finishes after its lease was released
// must not write the lease row back. release() removes the lease from
// memory and deletes its row; a late save would resurrect it as a zombie
// state=running row that no in-memory lease backs, charged to its owner
// until the lost/grace machinery catches up.

// TestSaveLeaseLockedDropsReleasedLease: the last-line guard in
// saveLeaseLocked refuses to write a released lease back even when a
// caller reaches it directly.
func TestSaveLeaseLockedDropsReleasedLease(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.releaseBecause(ctx, l, "released through the API")
	if _, err := db.GetLease(ctx, l.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lease row after release: %v, want ErrNotFound", err)
	}

	// A late caller (an operation that finished after the release) tries
	// to save it back.
	svc.store.mu.Lock()
	svc.saveLeaseLocked(l)
	svc.store.mu.Unlock()
	if _, err := db.GetLease(ctx, l.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("released lease row was written back: %v", err)
	}
}

// TestCheckpointReleaseRaceNoZombie: a checkpoint that finishes after
// its lease was released leaves no lease row and no sandboxes row behind,
// and its build is unreferenced (GC-able).
func TestCheckpointReleaseRaceNoZombie(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}

	started := make(chan struct{})
	unblock := make(chan struct{})
	sub.checkpointFn = func(ctx context.Context, sandboxID string) (string, substrate.BuildRefs, error) {
		close(started)
		<-unblock
		return "b-race", substrate.BuildRefs{}, nil
	}
	t.Cleanup(func() { sub.checkpointFn = nil })

	done := make(chan error, 1)
	go func() {
		_, err := svc.checkpointLeaseBusy(ctx, l, false)
		done <- err
	}()
	<-started

	// The release wins the race while the checkpoint is in flight.
	svc.releaseBecause(ctx, l, "released through the API")
	close(unblock)

	if err := <-done; !errors.Is(err, errLeaseReleased) {
		t.Fatalf("checkpoint error = %v, want errLeaseReleased", err)
	}
	if _, err := db.GetLease(ctx, l.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lease row after the race: %v, want ErrNotFound", err)
	}
	if _, ok := svc.store.leases[l.ID]; ok {
		t.Fatal("released lease is back in memory")
	}
	// The checkpoint build exists but nothing references it: the next GC
	// may reclaim it.
	if _, err := db.GetBuild(ctx, "b-race"); err != nil {
		t.Fatalf("checkpoint build missing: %v", err)
	}
	kept, err := svc.keptBuilds(ctx)
	if err != nil {
		t.Fatalf("keptBuilds: %v", err)
	}
	if kept["b-race"] {
		t.Fatal("the released lease's checkpoint build is still a GC root")
	}
}

// TestPauseReleaseRaceNoZombie: a pause that finishes after its lease was
// released leaves no lease row and no sandboxes row behind.
func TestPauseReleaseRaceNoZombie(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}

	started := make(chan struct{})
	unblock := make(chan struct{})
	sub.pauseFn = func(ctx context.Context, sandboxID, templateID string) (string, substrate.BuildRefs, error) {
		close(started)
		<-unblock
		return "b-pause-race", substrate.BuildRefs{}, nil
	}
	t.Cleanup(func() { sub.pauseFn = nil })

	done := make(chan error, 1)
	go func() {
		_, err := svc.pauseLease(ctx, l, false)
		done <- err
	}()
	<-started

	svc.releaseBecause(ctx, l, "released through the API")
	close(unblock)

	if err := <-done; !errors.Is(err, errLeaseReleased) {
		t.Fatalf("pause error = %v, want errLeaseReleased", err)
	}
	if _, err := db.GetLease(ctx, l.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lease row after the race: %v, want ErrNotFound", err)
	}
	if _, ok := svc.store.leases[l.ID]; ok {
		t.Fatal("released lease is back in memory")
	}
	kept, err := svc.keptBuilds(ctx)
	if err != nil {
		t.Fatalf("keptBuilds: %v", err)
	}
	if kept["b-pause-race"] {
		t.Fatal("the released lease's pause build is still a GC root")
	}
}

// TestRestoreReleaseRaceNoZombie: a restore whose fresh sandbox starts
// after its lease was released leaves no lease row and no sandboxes row
// behind, and the fresh sandbox is stopped.
func TestRestoreReleaseRaceNoZombie(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	b, err := svc.checkpointLease(ctx, l)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	started := make(chan struct{})
	unblock := make(chan struct{})
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		close(started)
		<-unblock
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	done := make(chan error, 1)
	go func() {
		done <- svc.restoreBusy(ctx, l, b)
	}()
	<-started

	svc.releaseBecause(ctx, l, "released through the API")
	close(unblock)

	if err := <-done; !errors.Is(err, errLeaseReleased) {
		t.Fatalf("restore error = %v, want errLeaseReleased", err)
	}
	if _, err := db.GetLease(ctx, l.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lease row after the race: %v, want ErrNotFound", err)
	}
	if _, ok := svc.store.leases[l.ID]; ok {
		t.Fatal("released lease is back in memory")
	}
	// The fresh sandbox the restore started is not left behind.
	for _, sb := range sub.sandboxesLive(t) {
		if sb == l.SandboxID {
			t.Fatalf("released lease's sandbox %s is still live", sb)
		}
	}
	if _, err := db.GetSandboxByLease(ctx, l.ID); err == nil {
		t.Fatal("released lease still has a sandboxes row")
	}
}

// TestRecoveryReleaseRaceNoZombie: a recovery whose sandbox starts after
// its lease was released leaves no lease row and no sandboxes row behind,
// and the fresh sandbox is stopped.
func TestRecoveryReleaseRaceNoZombie(t *testing.T) {
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
	sb := l.SandboxID

	started := make(chan struct{})
	unblock := make(chan struct{})
	first := true
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if first && req.SandboxID == sb {
			first = false
			close(started)
			<-unblock
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	done := make(chan recoveryOutcome, 1)
	go func() {
		done <- svc.recoverOneLease(ctx, l)
	}()
	<-started

	svc.releaseBecause(ctx, l, "released through the API")
	close(unblock)

	if out := <-done; out.Result != "released" {
		t.Fatalf("recovery result = %q, want released", out.Result)
	}
	if _, err := db.GetLease(ctx, l.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lease row after the race: %v, want ErrNotFound", err)
	}
	if _, ok := svc.store.leases[l.ID]; ok {
		t.Fatal("released lease is back in memory")
	}
	if _, err := db.GetSandboxByLease(ctx, l.ID); err == nil {
		t.Fatal("released lease still has a sandboxes row")
	}
}

// TestPruneStaleLeaseRows: a lease row with no in-memory twin whose
// sandbox is gone is dropped; a row whose sandbox survives, or that has a
// live twin, is left for the recovery/orphan passes.
func TestPruneStaleLeaseRows(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	now := time.Now()
	zombie := store.LeaseRow{
		ID: "zombie", Owner: "c", Image: "py-base", SandboxID: "sb-gone",
		CreatedAt: now, ExpiresAt: now.Add(time.Minute), LastActive: now,
		State: "running", Class: ClassGuaranteed,
	}
	if err := db.UpsertLease(ctx, zombie); err != nil {
		t.Fatalf("plant zombie: %v", err)
	}
	// A stale row whose sandbox still exists: ReconcileOrphans owns it.
	present := store.LeaseRow{
		ID: "still-present", Owner: "c", Image: "py-base", SandboxID: "sb-alive",
		CreatedAt: now, ExpiresAt: now.Add(time.Minute), LastActive: now,
		State: "running", Class: ClassGuaranteed,
	}
	if err := db.UpsertLease(ctx, present); err != nil {
		t.Fatalf("plant present: %v", err)
	}
	// A live lease with an in-memory twin: reconcileCrash owns it.
	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}

	svc.pruneStaleLeaseRows(ctx, map[string]bool{"sb-alive": true})

	if _, err := db.GetLease(ctx, "zombie"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("zombie row survived: %v", err)
	}
	if _, err := db.GetLease(ctx, "still-present"); err != nil {
		t.Fatalf("row whose sandbox survives was dropped: %v", err)
	}
	if _, err := db.GetLease(ctx, l.ID); err != nil {
		t.Fatalf("live lease's row was dropped: %v", err)
	}
}
