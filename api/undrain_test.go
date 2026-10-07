package api

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
)

// grantAndDrain grants n persistent leases and drains them, leaving each
// Drained with a resume build, the state the admin undrain starts from.
func grantAndDrain(t *testing.T, svc *Service, n int) []*Lease {
	t.Helper()
	ctx := context.Background()
	leases := make([]*Lease, 0, n)
	for i := range n {
		l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
		if err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
		leases = append(leases, l)
	}
	res, err := svc.drain(ctx)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(res.Failed) != 0 || res.Paused != n {
		t.Fatalf("drain = +%v, want %d paused and no failures", res, n)
	}
	return leases
}

// TestUndrainResumeRetriesTransient: a resume that fails once with the
// envd "syncing took too long" error is retried and the lease is resumed,
// not lost. spoond-urm.
func TestUndrainResumeRetriesTransient(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	svc.cfg.UndrainResumeRetries = 2

	leases := grantAndDrain(t, svc, 3)
	targetID := leases[1].ID
	targetSandbox := leases[1].SandboxID

	// The first resume create for the target fails as the node did on
	// 2026-10-07; every later one succeeds.
	var mu sync.Mutex
	failedOnce := false
	attempts := 0
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume && req.SandboxID == targetSandbox {
			mu.Lock()
			attempts++
			first := !failedOnce
			failedOnce = true
			mu.Unlock()
			if first {
				return substrate.Sandbox{}, errors.New("failed to create sandbox: failed to init envd: context canceled with cause: syncing took too long")
			}
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	res := svc.undrain(ctx)
	if res.Resumed != 3 || len(res.Failed) != 0 {
		t.Fatalf("undrain = +%v, want 3 resumed and no failures", res)
	}
	targetLease := svc.lookupAny(targetID)
	if targetLease.State != "running" || targetLease.Drained || targetLease.Suspended {
		t.Fatalf("retried lease = state=%s drained=%v suspended=%v, want running and undrained", targetLease.State, targetLease.Drained, targetLease.Suspended)
	}
	if !targetLease.LostAt.IsZero() {
		t.Fatal("a retried resume must not stamp the lease lost")
	}
	// grant + failed resume + successful retry.
	if got := calls(sub.Fake, "Create "+targetSandbox); got != 2 {
		t.Fatalf("resume creates = %d, want 2 (grant + successful retry) (calls %v)", got, sub.Fake.CallLog())
	}
	if attempts != 2 {
		t.Fatalf("resume attempts = %d, want 2 (one failure, one retry)", attempts)
	}
}

// TestUndrainResumeRetriesPermanent: a resume that keeps failing with a
// retryable envd error is attempted 1+retries times and then the lease
// becomes lost; the failure records how many attempts were made.
func TestUndrainResumeRetriesPermanent(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	svc.cfg.UndrainResumeRetries = 2

	leases := grantAndDrain(t, svc, 2)
	target := leases[0]
	targetSandbox := target.SandboxID

	var attempts atomic.Int32
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume && req.SandboxID == targetSandbox {
			attempts.Add(1)
			return substrate.Sandbox{}, errors.New("failed to init new envd: syncing took too long")
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	res := svc.undrain(ctx)
	if res.Resumed != 1 || len(res.Failed) != 1 {
		t.Fatalf("undrain = +%v, want 1 resumed and 1 failed", res)
	}
	f := res.Failed[0]
	if f.ID != target.ID || f.Attempts != 3 {
		t.Fatalf("failed entry = %+v, want id %s attempts 3", f, target.ID)
	}
	if target.State != "lost" || target.Drained {
		t.Fatalf("lease = state=%s drained=%v, want lost and undrained", target.State, target.Drained)
	}
	if target.LostAt.IsZero() {
		t.Fatal("a permanently failed resume must stamp the lease lost")
	}
	// grant + three resume attempts.
	if got := calls(sub.Fake, "Create "+targetSandbox); got != 1 {
		t.Fatalf("successful resume creates = %d, want 1 (the grant only) (calls %v)", got, sub.Fake.CallLog())
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("resume attempts = %d, want 3 (1 + 2 retries)", got)
	}
}

// TestUndrainResumeNonRetryable: a resume that fails because the build is
// gone is not retried; the lease goes lost on the first attempt.
func TestUndrainResumeNonRetryable(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	svc.cfg.UndrainResumeRetries = 2

	leases := grantAndDrain(t, svc, 1)
	target := leases[0]
	targetSandbox := target.SandboxID

	var attempts atomic.Int32
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume && req.SandboxID == targetSandbox {
			attempts.Add(1)
			return substrate.Sandbox{}, fmt.Errorf("load build %s: %w", "b-missing", store.ErrNotFound)
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	res := svc.undrain(ctx)
	if res.Resumed != 0 || len(res.Failed) != 1 {
		t.Fatalf("undrain = +%v, want 0 resumed and 1 failed", res)
	}
	if res.Failed[0].Attempts != 1 {
		t.Fatalf("non-retryable failure attempts = %d, want 1", res.Failed[0].Attempts)
	}
	if target.State != "lost" {
		t.Fatalf("lease state = %s, want lost", target.State)
	}
	// grant + the single attempt.
	if got := calls(sub.Fake, "Create "+targetSandbox); got != 1 {
		t.Fatalf("successful resume creates = %d, want 1 (the grant only) (calls %v)", got, sub.Fake.CallLog())
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("resume attempts = %d, want 1 (non-retryable)", got)
	}
}

// TestUndrainConcurrencyBound: undrain never runs more resumes at once
// than UNDRAIN_CONCURRENCY. The resume create is held until the test
// releases it, so a third resume cannot start behind the bound.
func TestUndrainConcurrencyBound(t *testing.T) {
	_, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()
	svc.cfg.UndrainConcurrency = 2
	svc.cfg.UndrainResumeRetries = 0

	const n = 4
	grantAndDrain(t, svc, n)

	var inFlight, maxSeen atomic.Int32
	started := make(chan struct{}, 16)
	release := make(chan struct{}, 16)
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume {
			cur := inFlight.Add(1)
			for {
				seen := maxSeen.Load()
				if cur <= seen || maxSeen.CompareAndSwap(seen, cur) {
					break
				}
			}
			started <- struct{}{}
			<-release
			inFlight.Add(-1)
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	done := make(chan undrainResult, 1)
	go func() { done <- svc.undrain(ctx) }()

	// Exactly the bound's worth of resumes start while they are held.
	for range 2 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("undrain did not start the bound's worth of resumes")
		}
	}
	select {
	case <-started:
		t.Fatalf("undrain started a %drd resume while %d were in flight; bound is %d", 3, inFlight.Load(), svc.cfg.UndrainConcurrency)
	case <-time.After(200 * time.Millisecond):
	}

	// Release one: the first held resume finishes and the last lease starts.
	release <- struct{}{}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("undrain did not start the next resume after one finished")
	}
	// Let every held call return.
	for range n {
		release <- struct{}{}
	}

	select {
	case res := <-done:
		if res.Resumed != n || len(res.Failed) != 0 {
			t.Fatalf("undrain = +%v, want %d resumed and no failures", res, n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("undrain did not finish")
	}
	if got := maxSeen.Load(); got > int32(svc.cfg.UndrainConcurrency) {
		t.Fatalf("max concurrent resumes = %d, want <= %d", got, svc.cfg.UndrainConcurrency)
	}
}
