package api

// spoond-j3a: a substrate RPC that never returns must not wedge the
// background sweeper, the lease's busy flag or the snapshot limiter. The
// e2b substrate bounds every call (substrate/e2b/deadline_test.go); these
// tests pin the service-side bound that frees the loop even if a
// substrate call only honours its context.

import (
	"context"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
)

// blockingPauseSub is a substrate stand-in whose Pause blocks until its
// context is cancelled, modelling the hung RPC. Every other method
// delegates to the fake, so the service's bookkeeping still runs.
type blockingPauseSub struct {
	*testSub

	mu       sync.Mutex
	starts   int
	returned int
}

func newBlockingPauseSub(t *testing.T) *blockingPauseSub {
	return &blockingPauseSub{testSub: newTestSub()}
}

func (b *blockingPauseSub) Pause(ctx context.Context, sandboxID, templateID string) (string, substrate.BuildRefs, error) {
	b.mu.Lock()
	b.starts++
	b.mu.Unlock()
	<-ctx.Done() // hang until the sweep's bound cancels us
	b.mu.Lock()
	b.returned++
	b.mu.Unlock()
	return "", substrate.BuildRefs{}, ctx.Err()
}

func (b *blockingPauseSub) counts() (starts, returned int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.starts, b.returned
}

// testServiceWithSub builds a service whose substrate is the given
// wrapper, reusing the fake and temp DB.
func testServiceWithSub(t *testing.T, sub substrate.Substrate) (*Service, *store.DB) {
	t.Helper()
	db := newTestDB(t)
	svc := NewService(sub, db, map[string]string{
		"token-a": "consumer-a", "token-b": "consumer-b", "legacy-tok": "legacy-consumer",
	}, ServiceConfig{DefaultTTL: 60 * time.Second, MaxTTL: 10 * time.Minute,
		ProxyURL: "http://127.0.0.1:1"})
	svc.log = log.New(io.Discard, "", 0)
	return svc, db
}

// TestSweepStageBoundFreesBusy: an idle suspend whose substrate Pause
// hangs is abandoned at the sweep bound, the lease's busy flag clears,
// and the snapshot limiter is released, so a hung RPC holds neither.
func TestSweepStageBoundFreesBusy(t *testing.T) {
	sub := newBlockingPauseSub(t)
	svc, db := testServiceWithSub(t, sub)
	seedImage(t, db, "py-base", 2048)
	svc.cfg.IdleTimeout = time.Millisecond
	svc.sweepTimeout = 100 * time.Millisecond

	ctx := context.Background()
	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.store.mu.Lock()
	l.LastActive = time.Now().Add(-time.Minute)
	svc.store.mu.Unlock()

	start := time.Now()
	svc.runSweepStage(ctx, "sweepExpired", svc.sweepExpired)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("runSweepStage took %s, want ~the 100ms bound", elapsed)
	}
	if starts, _ := sub.counts(); starts != 1 {
		t.Fatalf("Pause starts=%d, want the hung call to have been released", starts)
	}

	// The deferred endBusy ran when the stage was abandoned: the lease is
	// not stuck busy and the snapshot limiter has no leaked slot.
	deadline := time.Now().Add(2 * time.Second)
	for {
		svc.store.mu.Lock()
		busy := l.busy
		svc.store.mu.Unlock()
		_, returned := sub.counts()
		if !busy && !svc.snapshotBusy() && returned == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lease still busy=%v limiterBusy=%v returned=%d after the sweep bound", busy, svc.snapshotBusy(), returned)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSweepStageBoundReturns: runSweepStage returns once the bound
// elapses even though the stage's substrate call is still hanging, so
// the sweeper's tick is never blocked indefinitely.
func TestSweepStageBoundReturns(t *testing.T) {
	sub := newBlockingPauseSub(t)
	svc, db := testServiceWithSub(t, sub)
	seedImage(t, db, "py-base", 2048)
	svc.cfg.IdleTimeout = time.Millisecond
	svc.sweepTimeout = 100 * time.Millisecond

	ctx := context.Background()
	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.store.mu.Lock()
	l.LastActive = time.Now().Add(-time.Minute)
	svc.store.mu.Unlock()

	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.runSweepStage(ctx, "sweepExpired", svc.sweepExpired)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runSweepStage did not return within the sweep bound")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("runSweepStage took %s, want ~the 100ms bound", elapsed)
	}
}
