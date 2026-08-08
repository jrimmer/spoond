package runner

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeWorker is a controllable runnerWorker for pool tests.
type fakeWorker struct {
	mu       sync.Mutex
	regs     int
	fetches  int
	busy     bool // when true, Fetch blocks (simulates a running job)
	stopCh   chan struct{}
	stopped  bool
	registerErr error
}

func newFakeWorker() *fakeWorker {
	return &fakeWorker{stopCh: make(chan struct{})}
}

func (f *fakeWorker) Register(ctx context.Context, name, token string, labels []string, ephemeral bool) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.regs++
	if f.registerErr != nil {
		return 0, f.registerErr
	}
	return int64(f.regs), nil
}

func (f *fakeWorker) Fetch(ctx context.Context, version int64) (*Job, int64, error) {
	f.mu.Lock()
	f.fetches++
	busy := f.busy
	f.mu.Unlock()
	if busy {
		// return a job so the worker enters busy state and calls Run
		return &Job{ID: int64(f.fetches)}, version, nil
	}
	// idle: no job
	time.Sleep(20 * time.Millisecond)
	return nil, version, nil
}

func (f *fakeWorker) Run(ctx context.Context, job *Job) error {
	// block until stopped or ctx done (simulates a running job)
	select {
	case <-f.stopCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeWorker) setBusy(b bool) {
	f.mu.Lock()
	f.busy = b
	f.mu.Unlock()
}

func (f *fakeWorker) stop() {
	f.mu.Lock()
	if !f.stopped {
		close(f.stopCh)
		f.stopped = true
	}
	f.mu.Unlock()
}

func (f *fakeWorker) regCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.regs
}

// TestPoolStartsAtFloor verifies the pool spawns Floor workers on Start.
func TestPoolStartsAtFloor(t *testing.T) {
	cfg := PoolConfig{Floor: 3, Max: 9, ScaleStep: 3, PollInterval: 20 * time.Millisecond}
	p := NewRunnerPool(cfg, func() RunnerWorker { return newFakeWorker() }, "r", "tok", []string{"forkd"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	// give workers time to register
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.count() == 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := p.count(); got != 3 {
		t.Fatalf("expected 3 workers at floor, got %d", got)
	}
}

// TestPoolScalesUpWhenAllBusy verifies the pool adds ScaleStep workers
// when all current workers are busy.
func TestPoolScalesUpWhenAllBusy(t *testing.T) {
	cfg := PoolConfig{Floor: 3, Max: 9, ScaleStep: 3, PollInterval: 20 * time.Millisecond}
	p := NewRunnerPool(cfg, func() RunnerWorker { return newFakeWorker() }, "r", "tok", []string{"forkd"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	// wait for floor
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && p.count() < 3 {
		time.Sleep(20 * time.Millisecond)
	}

	// mark all busy
	p.mu.Lock()
	for _, w := range p.workers {
		w.impl.(*fakeWorker).setBusy(true)
	}
	p.mu.Unlock()

	// wait for scale-up
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p.count() >= 6 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := p.count(); got < 6 {
		t.Fatalf("expected scale-up to >=6, got %d", got)
	}
}

// TestPoolScalesDownToFloor verifies the pool removes idle workers
// beyond Floor after ScaleDownDelay.
func TestPoolScalesDownToFloor(t *testing.T) {
	cfg := PoolConfig{
		Floor: 3, Max: 9, ScaleStep: 3,
		PollInterval:   20 * time.Millisecond,
		ScaleDownDelay: 100 * time.Millisecond,
	}
	p := NewRunnerPool(cfg, func() RunnerWorker { return newFakeWorker() }, "r", "tok", []string{"forkd"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	// force scale up to 6 by making all busy
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && p.count() < 3 {
		time.Sleep(20 * time.Millisecond)
	}
	p.mu.Lock()
	for _, w := range p.workers {
		w.impl.(*fakeWorker).setBusy(true)
	}
	p.mu.Unlock()
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && p.count() < 6 {
		time.Sleep(20 * time.Millisecond)
	}

	// now make all idle -> should scale back down to floor
	p.mu.Lock()
	for _, w := range p.workers {
		w.impl.(*fakeWorker).setBusy(false)
	}
	p.mu.Unlock()

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p.count() <= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := p.count(); got != 3 {
		t.Fatalf("expected scale-down to floor 3, got %d", got)
	}
}
