package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jrimmer/spoond/v2/metrics"
	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
)

// snapshotGate is a blocking Pause for the fake substrate: each call
// enters, signals entered, and waits for a release. It tracks the peak
// concurrency so a test can pin the limiter's width.
type snapshotGate struct {
	entered chan string
	release chan struct{}

	mu       sync.Mutex
	inFlight int
	peak     int
	calls    int
}

func newSnapshotGate() *snapshotGate {
	return &snapshotGate{
		entered: make(chan string, 32),
		release: make(chan struct{}),
	}
}

func (g *snapshotGate) pause(_ context.Context, sandboxID, _ string) (string, substrate.BuildRefs, error) {
	g.mu.Lock()
	g.inFlight++
	g.calls++
	if g.inFlight > g.peak {
		g.peak = g.inFlight
	}
	g.mu.Unlock()
	g.entered <- sandboxID
	<-g.release
	g.mu.Lock()
	g.inFlight--
	g.mu.Unlock()
	return uuid.NewString(), substrate.BuildRefs{}, nil
}

func (g *snapshotGate) stats() (calls, peak int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls, g.peak
}

// waitForEntered reads one entered sandbox id, failing on timeout.
func (g *snapshotGate) waitForEntered(t *testing.T) string {
	t.Helper()
	select {
	case id := <-g.entered:
		return id
	case <-time.After(5 * time.Second):
		t.Fatal("no snapshot write reached the substrate")
		return ""
	}
}

// setSnapshotWidth replaces the default limiter's width (0 = unlimited).
func setSnapshotWidth(svc *Service, width int) {
	svc.snapshotLimiters.def = newSnapshotLimiter(width, svc.now, svc.log.Printf)
}

// setDrainSnapshotWidth replaces the drain limiter's width.
func setDrainSnapshotWidth(svc *Service, width int) {
	svc.snapshotLimiters.drain = newSnapshotLimiter(width, svc.now, svc.log.Printf)
}

// grantTwoIdleLeases seeds an image and grants two persistent leases.
func grantTwoIdleLeases(t *testing.T, svc *Service, db *store.DB) (*Lease, *Lease) {
	t.Helper()
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	a, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant a: %v", err)
	}
	b, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant b: %v", err)
	}
	return a, b
}

// TestSnapshotWritesOneAtATime pins the default limiter: at width 1 two
// concurrent suspends reach the substrate one at a time; at width 0
// they run together (the pre-fix behaviour).
func TestSnapshotWritesOneAtATime(t *testing.T) {
	for _, width := range []int{1, 0} {
		name := "width-1-serialised"
		wantPeak := 1
		if width == 0 {
			name = "width-0-unlimited"
			wantPeak = 2
		}
		t.Run(name, func(t *testing.T) {
			svc, db, sub := newTestService(t)
			a, b := grantTwoIdleLeases(t, svc, db)
			setSnapshotWidth(svc, width)
			gate := newSnapshotGate()
			sub.pauseFn = gate.pause

			ctx := context.Background()
			var wg sync.WaitGroup
			errCh := make(chan error, 2)
			for _, l := range []*Lease{a, b} {
				wg.Add(1)
				go func(l *Lease) {
					defer wg.Done()
					_, err := svc.suspend(ctx, "c", l.ID)
					errCh <- err
				}(l)
			}

			// Both writes must reach the substrate before either is
			// released; at width 1 only one may be there at a time.
			first := gate.waitForEntered(t)
			if wantPeak == 2 {
				gate.waitForEntered(t)
			} else {
				select {
				case id := <-gate.entered:
					t.Fatalf("second write %s entered while the first %s held the only slot", id, first)
				case <-time.After(200 * time.Millisecond):
				}
			}
			if calls, peak := gate.stats(); calls != wantPeak || peak != wantPeak {
				t.Fatalf("width %d: calls=%d peak=%d, want %d", width, calls, peak, wantPeak)
			}

			// Release every entered write and (for width 1) the waiter.
			for range wantPeak {
				gate.release <- struct{}{}
			}
			if wantPeak == 1 {
				gate.waitForEntered(t)
				gate.release <- struct{}{}
			}
			wg.Wait()
			close(errCh)
			for err := range errCh {
				if err != nil {
					t.Fatalf("suspend: %v", err)
				}
			}
			for _, l := range []*Lease{a, b} {
				if !l.Suspended {
					t.Fatalf("lease %s not suspended", l.ID)
				}
			}
		})
	}
}

// TestDrainUsesItsOwnWidth: the admin drain pauses through the drain
// limiter, so with the default width at 1 the drain still runs two
// pause writes at once.
func TestDrainUsesItsOwnWidth(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var leases []*Lease
	for range 3 {
		l, err := svc.grant(ctx, "c", "py-base", time.Hour, false, "", nil, "", "", nil)
		if err != nil {
			t.Fatalf("grant: %v", err)
		}
		leases = append(leases, l)
	}
	setSnapshotWidth(svc, 1)
	setDrainSnapshotWidth(svc, 2)
	gate := newSnapshotGate()
	sub.pauseFn = gate.pause

	done := make(chan struct{})
	go func() { defer close(done); _, _ = svc.drain(ctx) }()

	// Two drain pauses run; the default width of 1 does not constrain
	// them.
	a := gate.waitForEntered(t)
	b := gate.waitForEntered(t)
	if a == b {
		t.Fatalf("the same sandbox %s entered twice", a)
	}
	select {
	case id := <-gate.entered:
		t.Fatalf("third write %s entered while two held the drain slots", id)
	case <-time.After(200 * time.Millisecond):
	}
	if calls, peak := gate.stats(); calls != 2 || peak != 2 {
		t.Fatalf("drain calls=%d peak=%d, want 2", calls, peak)
	}

	// Let the first two finish; the third takes a freed slot.
	gate.release <- struct{}{}
	gate.release <- struct{}{}
	gate.waitForEntered(t)
	gate.release <- struct{}{}
	// The drain now waits for the node to quiesce with a context that is
	// about to be cancelled.
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not return after cancellation")
	}
	for _, l := range leases {
		if l.busy {
			t.Fatalf("lease %s left busy after the drain", l.ID)
		}
	}
}

// TestSnapshotLimiterBusySkipsSweep: while the limiter has no free
// slot, an idle sweep tick suspends nothing and the next tick retries.
func TestSnapshotLimiterBusySkipsSweep(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	setSnapshotWidth(svc, 1)

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

	// Occupy the only slot with a write that never runs.
	release, err := svc.snapshotAcquire(ctx, false)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !svc.snapshotBusy() {
		t.Fatal("limiter reports free while its only slot is held")
	}
	svc.suspendIdleLeases(ctx, base.Add(2*time.Minute))
	if l.Suspended {
		t.Fatal("the idle sweep suspended a lease while the limiter was busy")
	}
	// The next tick, with the slot free, retries and suspends.
	release()
	svc.suspendIdleLeases(ctx, base.Add(2*time.Minute))
	if !l.Suspended {
		t.Fatal("the idle sweep did not retry the lease once the limiter was free")
	}
}

// TestSnapshotLimiterBusySkipsHeldRules is removed with the held rules
// (FS5).

// TestSnapshotLimiterContextCancel: a write waiting for a slot returns
// the caller's context error when it is cancelled, and leaks no slot.
func TestSnapshotLimiterContextCancel(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	setSnapshotWidth(svc, 1)

	ctx := context.Background()
	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}

	release, err := svc.snapshotAcquire(ctx, false)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	waitCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := svc.pauseLease(waitCtx, l, false)
		done <- err
	}()
	// Let the goroutine take the lease's busy flag and reach the limiter,
	// then cancel while it waits.
	deadline := time.Now().Add(2 * time.Second)
	for {
		svc.store.mu.Lock()
		busy := l.busy
		svc.store.mu.Unlock()
		if busy || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting write returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiting write did not return after cancellation")
	}
	// The lease is not left busy, and the limiter holds no leaked slot:
	// the only in-flight write is the one this test holds.
	svc.store.mu.Lock()
	busy := l.busy
	svc.store.mu.Unlock()
	if busy {
		t.Fatal("lease left busy after a cancelled wait")
	}
	release()
	if n := svc.snapshotLimiters.def.inFlight.Load(); n != 0 {
		t.Fatalf("snapshot limiter in-flight = %d after the held write released, want 0", n)
	}
}

// TestSnapshotWriteMetrics: a snapshot write observes the wait histogram
// and the in-flight gauge drops back to zero after it finishes.
func TestSnapshotWriteMetrics(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	svc.SetMetrics(metrics.NewBackendMetrics())
	setSnapshotWidth(svc, 1)

	ctx := context.Background()
	release, err := svc.snapshotAcquire(ctx, false)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if got := gaugeValue(t, svc, "spoond_snapshot_writes_in_flight"); got != 1 {
		t.Fatalf("in-flight gauge = %v while a write holds the slot, want 1", got)
	}
	release()
	if got := gaugeValue(t, svc, "spoond_snapshot_writes_in_flight"); got != 0 {
		t.Fatalf("in-flight gauge = %v after release, want 0", got)
	}
	mfs, err := svc.metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var samples uint64
	for _, mf := range mfs {
		if mf.GetName() == "spoond_snapshot_write_wait_seconds" {
			for _, m := range mf.GetMetric() {
				samples += m.GetHistogram().GetSampleCount()
			}
		}
	}
	if samples != 1 {
		t.Fatalf("snapshot_write_wait_seconds sample count = %d, want 1", samples)
	}
}
