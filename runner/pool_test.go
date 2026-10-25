package runner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeWorker is a controllable runnerWorker for pool tests.
type fakeWorker struct {
	mu          sync.Mutex
	regs        int
	declared    [][]string
	declareErr  error
	fetches     int
	busy        bool // when true, Fetch blocks (simulates a running job)
	stopCh      chan struct{}
	stopped     bool
	registerErr error
}

func newFakeWorker() *fakeWorker {
	return &fakeWorker{stopCh: make(chan struct{})}
}

func (f *fakeWorker) Register(ctx context.Context, name, token string, labels []string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.regs++
	if f.registerErr != nil {
		return 0, f.registerErr
	}
	return int64(f.regs), nil
}

func (f *fakeWorker) Restore(uuid, token string, id int64) {}
func (f *fakeWorker) Declare(ctx context.Context, labels []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.declared = append(f.declared, append([]string(nil), labels...))
	return f.declareErr
}
func (f *fakeWorker) RunnerID() int64                    { return 0 }
func (f *fakeWorker) Deregister(adminToken string) error { return nil }
func (f *fakeWorker) Credentials() RunnerStateEntry      { return RunnerStateEntry{} }

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
	p := NewRunnerPool(cfg, func() RunnerWorker { return newFakeWorker() }, "r", "tok", []string{"spoond"})
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
	p := NewRunnerPool(cfg, func() RunnerWorker { return newFakeWorker() }, "r", "tok", []string{"spoond"})
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
	p := NewRunnerPool(cfg, func() RunnerWorker { return newFakeWorker() }, "r", "tok", []string{"spoond"})
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

func TestWithDefaultsRaisesMaxToFloor(t *testing.T) {
	c := PoolConfig{Floor: 3, Max: 1}.withDefaults()
	if c.Max != 3 {
		t.Fatalf("Max = %d, want 3 (raised to Floor)", c.Max)
	}
	if got := (PoolConfig{Floor: 4, Max: 4}).withDefaults().Max; got != 4 {
		t.Fatalf("Max = %d, want 4 (Max == Floor left alone)", got)
	}
	if got := (PoolConfig{Floor: 3, Max: 12}).withDefaults().Max; got != 12 {
		t.Fatalf("Max = %d, want 12 (Max above Floor left alone)", got)
	}
	if got := (PoolConfig{Max: 1}).withDefaults().Max; got != 3 {
		t.Fatalf("Max = %d, want 3 (raised to default Floor)", got)
	}
}

// TestRestoredWorkerDeclaresLabels (#161): a worker restored from saved
// credentials re-sends its current labels to Forgejo instead of keeping
// the ones it first registered with, and does not register again.
func TestRestoredWorkerDeclaresLabels(t *testing.T) {
	f := newFakeWorker()
	w := &worker{id: 0, impl: f, name: "r-0", token: "tok", labels: []string{"ubuntu-latest", "rust-gpui-accept"}}
	ctx, cancel := context.WithCancel(context.Background())
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		w.run(ctx, stop, nil, &RunnerStateEntry{UUID: "u", Token: "t", ID: 7}, func(RunnerStateEntry) {})
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		n, regs := len(f.declared), f.regs
		var got []string
		if n > 0 {
			got = f.declared[0]
		}
		f.mu.Unlock()
		if n > 0 {
			if strings.Join(got, ",") != "ubuntu-latest,rust-gpui-accept" || regs != 0 {
				t.Fatalf("declared %v, registers %d; want the current labels and no register", got, regs)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("restored worker never declared its labels")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(stop)
	cancel()
	<-done
}

// TestRestoredWorkerDeclareFailureKeepsCredentials (#161): a failed
// declare is only logged; the worker keeps its restored credentials and
// fetches with them rather than registering again. (An idle worker waits
// 2 s between fetches, so one fetch is what fits the deadline.)
func TestRestoredWorkerDeclareFailureKeepsCredentials(t *testing.T) {
	f := newFakeWorker()
	f.declareErr = errors.New("declare: unimplemented")
	w := &worker{id: 0, impl: f, name: "r-0", token: "tok", labels: []string{"ubuntu-latest"}}
	ctx, cancel := context.WithCancel(context.Background())
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		w.run(ctx, stop, nil, &RunnerStateEntry{UUID: "u", Token: "t", ID: 7}, func(RunnerStateEntry) {})
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		fetches, regs := f.fetches, f.regs
		f.mu.Unlock()
		if regs != 0 {
			t.Fatalf("a failed declare led to %d register(s)", regs)
		}
		if fetches >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker never fetched after a failed declare")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(stop)
	cancel()
	<-done
}
