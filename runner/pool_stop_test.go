package runner

import (
	"context"
	"sync"
	"testing"
	"time"
)

// drainingWorker is a RunnerWorker whose Run behaves like the real
// executor around a sandbox lease: it "creates" a lease, holds it for
// the duration of the job and releases it ("Delete") when its run ends
// — however that happens (job finished, or the pool's Stop cancelled
// the job after the grace period). Tests observe the lease lifecycle
// without an HTTP backend, through the same wiring production uses:
// Start(ctx) with a cancellable context standing in for the signal
// context, then Stop.
type drainingWorker struct {
	mu       sync.Mutex
	regs     int
	busy     bool
	lease    string          // created while a job is "running"
	jobDur   time.Duration   // how long a job runs before ending on its own
	runCnt   int             // jobs Run was called for
	cancel   bool            // a job's context was cancelled under it
	runCtx   context.Context // the live job context, for liveness probes
	releases []string        // lease ids released so far, in order
	release  chan struct{}   // closed on the first release
}

func newDrainingWorker() *drainingWorker {
	return &drainingWorker{release: make(chan struct{})}
}

func (f *drainingWorker) Register(ctx context.Context, name, token string, labels []string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.regs++
	return int64(f.regs), nil
}
func (f *drainingWorker) Restore(uuid, token string, id int64) {}
func (f *drainingWorker) RunnerID() int64                      { return 0 }
func (f *drainingWorker) Deregister(adminToken string) error   { return nil }
func (f *drainingWorker) Credentials() RunnerStateEntry        { return RunnerStateEntry{} }

func (f *drainingWorker) Fetch(ctx context.Context, version int64) (*Job, int64, error) {
	f.mu.Lock()
	busy := f.busy
	f.mu.Unlock()
	if busy {
		return &Job{ID: 7}, version, nil
	}
	// Idle: no job, but yield to the loop like the real poll does (the
	// pool tests set PollInterval to milliseconds; keep this far below
	// the 1 s sleep cap).
	time.Sleep(5 * time.Millisecond)
	return nil, version, nil
}

// Run acquires a lease and releases it when the run ends — after a
// bounded "job" of at most jobDur, or on an earlier context
// cancellation (the pool's Stop after its grace). Releasing on the
// cancelled path is the executor's deferred Delete.
func (f *drainingWorker) Run(ctx context.Context, job *Job) error {
	f.mu.Lock()
	f.lease = "lease-for-job-7"
	id := f.lease
	f.runCnt++
	f.runCtx = ctx
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.lease = ""
		f.releases = append(f.releases, id)
		first := len(f.releases) == 1
		f.mu.Unlock()
		if first {
			close(f.release)
		}
	}()
	jobDur := 30 * time.Millisecond
	if f.jobDur > 0 {
		jobDur = f.jobDur
	}
	timer := time.NewTimer(jobDur)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		f.mu.Lock()
		f.cancel = true
		f.mu.Unlock()
		return ctx.Err()
	}
}
func (f *drainingWorker) setBusy(b bool) {
	f.mu.Lock()
	f.busy = b
	f.mu.Unlock()
}

// fetchDelayWorker delays each Fetch so a shutdown can race an
// in-flight fetch; it then offers a job unconditionally.
type fetchDelayWorker struct {
	drainingWorker
	delay time.Duration
}

func (f *fetchDelayWorker) Fetch(ctx context.Context, version int64) (*Job, int64, error) {
	time.Sleep(f.delay)
	return &Job{ID: 9}, version, nil
}

func (f *drainingWorker) leases() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lease == "" {
		return nil
	}
	return []string{f.lease}
}

func (f *drainingWorker) releasedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.releases)
}

// runContextAlive reports whether the current job's context is still
// live (production invariant: a shutdown signal must not cancel it).
func (f *drainingWorker) runContextAlive() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runCtx != nil && f.runCtx.Err() == nil
}

func (f *drainingWorker) wasCancelled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancel
}

func (f *drainingWorker) ranJobs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runCnt
}

// waitReleased waits for the lease release, bounded (no sleeps over 1 s).
func waitReleased(t *testing.T, f *drainingWorker) {
	t.Helper()
	select {
	case <-f.release:
	case <-time.After(2 * time.Second):
		t.Fatal("lease was never released")
	}
}

// fakeSweeper records what the pool swept at start.
type fakeSweeper struct {
	mu       sync.Mutex
	calls    int
	keepNil  bool
	deleted  int
	sweepErr error
}

func (s *fakeSweeper) SweepOrphans(ctx context.Context, keep func(id string) bool) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.keepNil = keep == nil
	s.deleted += 3
	return 3, s.sweepErr
}

func (s *fakeSweeper) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *fakeSweeper) keepWasNil() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keepNil
}

// waitBusy starts a one-worker pool (with a sweep-recording Leases) and
// waits until its job is running.
func waitBusy(t *testing.T, cfg PoolConfig, w *drainingWorker) (*RunnerPool, context.CancelFunc) {
	t.Helper()
	p := NewRunnerPool(cfg, func() RunnerWorker { return w }, "r", "tok", []string{"spoond"})
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(w.leases()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if len(w.leases()) == 0 {
		cancel()
		t.Fatal("job never started")
	}
	return p, cancel
}

// TestSignalDoesNotKillRunningJob (#119): the production wiring — a
// cancellable Start context, Stop called after it — must not cancel a
// running job at signal time. Cancelling the context is what SIGTERM
// does to the pool; the job's own context hangs off the pool's jobs
// context and stays live, and only Stop — after RUNNER_STOP_GRACE —
// ends the job and releases its lease.
func TestSignalDoesNotKillRunningJob(t *testing.T) {
	w := newDrainingWorker()
	w.setBusy(true)
	w.jobDur = 5 * time.Second // only Stop's grace expiry can end it
	var sw fakeSweeper
	p, cancel := waitBusy(t, PoolConfig{
		Floor:        1,
		Max:          1,
		PollInterval: 10 * time.Millisecond,
		StopGrace:    80 * time.Millisecond, // short grace: expiry arrives fast
		Leases:       &sw,
	}, w)

	// The signal arrives (this is exactly what SIGTERM does to the
	// Start context in cmd/spoond-runner).
	cancel()
	time.Sleep(50 * time.Millisecond)
	if !w.runContextAlive() {
		t.Fatal("the job's context died with the signal context; RUNNER_STOP_GRACE can never be honoured")
	}
	if w.releasedCount() != 0 {
		t.Fatal("the job's lease was released at signal time, before any grace")
	}

	// Stop drains: the grace expires, the job is cancelled, and the
	// executor's deferred Delete releases the lease.
	start := time.Now()
	p.Stop()
	waitReleased(t, w)
	if !w.wasCancelled() {
		t.Fatal("the job ended without its context being cancelled, though it cannot finish on its own")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Stop took %s, want ≈ grace (80ms) + a little", elapsed)
	}
}

// TestStopGraceLetsJobFinishThenReleasesLease (#119): a job that fits
// inside the grace window finishes on its own — its context is never
// cancelled — and its lease is released; Stop returns well before the
// grace is spent.
func TestStopGraceLetsJobFinishThenReleasesLease(t *testing.T) {
	w := newDrainingWorker()
	w.setBusy(true)
	w.jobDur = 30 * time.Millisecond // finishes well inside the grace
	var sw fakeSweeper
	p, cancel := waitBusy(t, PoolConfig{
		Floor:        1,
		Max:          1,
		PollInterval: 10 * time.Millisecond,
		StopGrace:    500 * time.Millisecond,
		Leases:       &sw,
	}, w)
	defer cancel()

	// Signal first, as production does: the drain must still let the
	// job finish.
	cancel()
	time.Sleep(10 * time.Millisecond)
	if !w.runContextAlive() {
		t.Fatal("signal killed the running job")
	}

	start := time.Now()
	p.Stop()
	waitReleased(t, w)
	if w.wasCancelled() {
		t.Fatal("a job inside the grace window was cancelled instead of being allowed to finish")
	}
	if elapsed := time.Since(start); elapsed > 450*time.Millisecond {
		t.Fatalf("Stop took %s; the job fits the grace, so Stop must not wait it out", elapsed)
	}
	if w.releasedCount() == 0 {
		t.Fatal("lease never released")
	}
}

// TestStopGraceExpiryCancelsJob (#119): a job that ignores the grace
// window is cancelled when it expires, and the cancellation is exactly
// what releases its lease (the executor's deferred Delete).
func TestStopGraceExpiryCancelsJob(t *testing.T) {
	w := newDrainingWorker()
	w.setBusy(true)
	w.jobDur = 5 * time.Second // far beyond the grace: only cancellation ends it
	var sw fakeSweeper
	p, cancel := waitBusy(t, PoolConfig{
		Floor:        1,
		Max:          1,
		PollInterval: 10 * time.Millisecond,
		StopGrace:    50 * time.Millisecond, // expiry is the point
		Leases:       &sw,
	}, w)
	defer cancel()

	start := time.Now()
	p.Stop()
	waitReleased(t, w)
	if !w.wasCancelled() {
		t.Fatal("grace expiry did not cancel the job")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Stop took %s, want ≈ grace (50ms) + a little", elapsed)
	}
}

// TestStopDoesNotRunInFlightFetch (#119): a fetch in flight when Stop
// fires must not launch the job it comes back with — the drain stops
// polling for new jobs, exactly, not approximately.
func TestStopDoesNotRunInFlightFetch(t *testing.T) {
	w := &fetchDelayWorker{drainingWorker: drainingWorker{release: make(chan struct{})}, delay: 250 * time.Millisecond}
	var sw fakeSweeper
	p := NewRunnerPool(PoolConfig{
		Floor:        1,
		Max:          1,
		PollInterval: 10 * time.Millisecond,
		StopGrace:    time.Second,
		Leases:       &sw,
	}, func() RunnerWorker { return w }, "r", "tok", []string{"spoond"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	// The worker's first fetch is in flight (250 ms delay) throughout:
	// Stop is called within a few poll ticks of the spawn, far inside
	// the delay. Stop waits for the loop to exit, so what follows is
	// decided.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && p.count() < 1 {
		time.Sleep(5 * time.Millisecond)
	}
	p.Stop()

	// The fetch landed after the stop; its job must never have run and
	// no lease may have been created for it.
	if n := w.ranJobs(); n != 0 {
		t.Fatalf("%d job(s) ran after Stop; an in-flight fetch must be dropped", n)
	}
	if len(w.leases()) != 0 {
		t.Fatal("a lease was created after Stop")
	}
}

// TestContextEndsWorkerLoopsWithoutStop: the Start context governs the
// pool's own loops — cancelling it ends them even if Stop never comes
// (tests that leak a pool must not leak its goroutines).
func TestContextEndsWorkerLoopsWithoutStop(t *testing.T) {
	var sw fakeSweeper
	p := NewRunnerPool(PoolConfig{
		Floor:        1,
		Max:          1,
		PollInterval: 10 * time.Millisecond,
		Leases:       &sw,
	}, func() RunnerWorker { return newDrainingWorker() }, "r", "tok", []string{"spoond"})
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && p.count() < 1 {
		time.Sleep(5 * time.Millisecond)
	}
	p.mu.Lock()
	var w *worker
	for _, ww := range p.workers {
		w = ww
	}
	w.mu.Lock()
	loopDone := w.done
	w.mu.Unlock()
	p.mu.Unlock()

	cancel()
	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("worker loop outlived its context without a Stop")
	}
	// Clean up for other tests: Stop is still required to cancel the
	// jobs context.
	p.Stop()
}

// TestStopIdlePoolReturnsImmediately: with no running jobs, Stop does
// not wait out the grace period.
func TestStopIdlePoolReturnsImmediately(t *testing.T) {
	var sw fakeSweeper
	p := NewRunnerPool(PoolConfig{
		Floor:        1,
		Max:          1,
		PollInterval: 10 * time.Millisecond,
		StopGrace:    time.Minute,
		Leases:       &sw,
	}, func() RunnerWorker { return newDrainingWorker() }, "r", "tok", []string{"spoond"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && p.count() < 1 {
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	p.Stop()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Stop on an idle pool took %s, want ≪ grace", elapsed)
	}
}

// TestStopIsIdempotent: two Stops are one shutdown.
func TestStopIsIdempotent(t *testing.T) {
	var sw fakeSweeper
	p := NewRunnerPool(PoolConfig{
		Floor:        1,
		Max:          1,
		PollInterval: 10 * time.Millisecond,
		StopGrace:    10 * time.Millisecond,
		Leases:       &sw,
	}, func() RunnerWorker { return newDrainingWorker() }, "r", "tok", []string{"spoond"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && p.count() < 1 {
		time.Sleep(5 * time.Millisecond)
	}
	p.Stop()
	p.Stop() // must not panic or block
}

// TestStartSweepsOrphanLeases (#119): at start, when this process runs
// nothing yet, the pool sweeps every job-labelled lease of its token.
func TestStartSweepsOrphanLeases(t *testing.T) {
	var sw fakeSweeper
	p := NewRunnerPool(PoolConfig{
		Floor:        1,
		Max:          1,
		PollInterval: 10 * time.Millisecond,
		Leases:       &sw,
	}, func() RunnerWorker { return newDrainingWorker() }, "r", "tok", []string{"spoond"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && sw.count() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := sw.count(); got != 1 {
		t.Fatalf("pool swept %d time(s) at start, want exactly 1", got)
	}
	if !sw.keepWasNil() {
		t.Fatal("start sweep passed a keep filter, want nil (everything is an orphan at start)")
	}
	p.Stop()
}

// TestStartSweepErrorIsNotFatal: a failing sweep (backend down) still
// starts the pool — the next restart tries again.
func TestStartSweepErrorIsNotFatal(t *testing.T) {
	sw := &fakeSweeper{sweepErr: context.DeadlineExceeded}
	p := NewRunnerPool(PoolConfig{
		Floor:        1,
		Max:          1,
		PollInterval: 10 * time.Millisecond,
		Leases:       sw,
	}, func() RunnerWorker { return newDrainingWorker() }, "r", "tok", []string{"spoond"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && p.count() < 1 {
		time.Sleep(5 * time.Millisecond)
	}
	if p.count() != 1 {
		t.Fatal("pool did not start after a failed sweep")
	}
	p.Stop()
}

// createWaitWorker is a RunnerWorker whose Run blocks in the create
// wait: it reports waiting=true to the pool's notifier and stays there
// until its context dies — a job that has no sandbox yet when a
// shutdown arrives (L3).
type createWaitWorker struct {
	drainingWorker
	notify    func(bool)
	waiting   chan struct{}
	createBeg chan struct{}
}

func newCreateWaitWorker() *createWaitWorker {
	return &createWaitWorker{waiting: make(chan struct{}), createBeg: make(chan struct{})}
}

func (w *createWaitWorker) SetCreateWaitNotifier(fn func(bool)) { w.notify = fn }

func (w *createWaitWorker) Run(ctx context.Context, job *Job) error {
	if w.notify != nil {
		w.notify(true)
	}
	close(w.createBeg)
	<-ctx.Done()
	if w.notify != nil {
		w.notify(false)
	}
	w.mu.Lock()
	w.cancel = true
	w.releases = append(w.releases, "no-lease")
	w.mu.Unlock()
	return ctx.Err()
}

// executorWorker drives a REAL Executor through the pool, so the
// executor's own createWait reporting is exercised end to end (T1).
// Fetch hands out one job.
type executorWorker struct {
	exec  *Executor
	once  sync.Once
	jobID int64
}

func (w *executorWorker) SetCreateWaitNotifier(fn func(bool)) { w.exec.OnCreateWait = fn }
func (w *executorWorker) Register(ctx context.Context, name, token string, labels []string) (int64, error) {
	return 1, nil
}
func (w *executorWorker) Restore(uuid, token string, id int64) {}
func (w *executorWorker) RunnerID() int64                      { return 1 }
func (w *executorWorker) Deregister(adminToken string) error   { return nil }
func (w *executorWorker) Credentials() RunnerStateEntry        { return RunnerStateEntry{} }
func (w *executorWorker) Fetch(ctx context.Context, version int64) (*Job, int64, error) {
	var job *Job
	w.once.Do(func() {
		id := w.jobID
		if id == 0 {
			id = 42
		}
		job = testJob("jobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n")
		job.ID = id
	})
	if job == nil {
		time.Sleep(5 * time.Millisecond)
	}
	return job, version, nil
}
func (w *executorWorker) Run(ctx context.Context, job *Job) error { return w.exec.Run(ctx, job) }

// stepBlockingLease creates at once, then blocks in Exec until its
// context dies — a job past Create and running a step.
type stepBlockingLease struct {
	fakeLease
	execStarted chan struct{}
}

func (l *stepBlockingLease) Exec(ctx context.Context, id, cmd, cwd string, env map[string]string, timeout int) (*ExecResult, error) {
	close(l.execStarted)
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestStopCancelsJobWaitingInCreateKeepsGraceForRunningJob (T1): on Stop,
// a job PAST Create — running a step — still keeps RUNNER_STOP_GRACE,
// while another job still in Create is cancelled at once. It runs the
// real executor through the pool, so the executor must CLEAR its
// createWait once the sandbox exists: the mutation "never clear
// createWait after Create" leaves the pool believing the job still waits
// and cancels it without the grace, and Stop returns at once.
func TestStopCancelsJobWaitingInCreateKeepsGraceForRunningJob(t *testing.T) {
	lease := &stepBlockingLease{fakeLease: *newFakeLease(), execStarted: make(chan struct{})}
	exec := &Executor{
		Sandbox:      lease,
		Sink:         &fakeSink{},
		Labels:       map[string]string{"ubuntu-latest": "py-base"},
		DefaultImage: "py-base",
		TTL:          600,
	}
	rw := &executorWorker{exec: exec}

	var sw fakeSweeper
	p := NewRunnerPool(PoolConfig{
		Floor:        1,
		Max:          1,
		PollInterval: 10 * time.Millisecond,
		StopGrace:    150 * time.Millisecond,
		Leases:       &sw,
	}, func() RunnerWorker { return rw }, "r", "tok", []string{"spoond"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	select {
	case <-lease.execStarted:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("the job's step never started")
	}

	start := time.Now()
	p.Stop()
	// The running job must keep its grace: Stop cannot return before it.
	if elapsed := time.Since(start); elapsed < 120*time.Millisecond {
		t.Fatalf("Stop took %s; a job past Create must keep RUNNER_STOP_GRACE", elapsed)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Stop took %s, want ≈ grace (150ms) + a little", elapsed)
	}
}

// TestStopCancelsJobWaitingInCreate (L3): a job still waiting in Create
// for a sandbox is cancelled at once on Stop — it has no sandbox and no
// work to finish, so the drain must not spend RUNNER_STOP_GRACE on it.
func TestStopCancelsJobWaitingInCreate(t *testing.T) {
	w := newCreateWaitWorker()
	w.setBusy(true)
	var sw fakeSweeper
	p := NewRunnerPool(PoolConfig{
		Floor:        1,
		Max:          1,
		PollInterval: 10 * time.Millisecond,
		StopGrace:    5 * time.Second, // would stall the test if it were honoured
		Leases:       &sw,
	}, func() RunnerWorker { return w }, "r", "tok", []string{"spoond"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	select {
	case <-w.createBeg:
	case <-time.After(2 * time.Second):
		t.Fatal("job never reached the create wait")
	}

	start := time.Now()
	p.Stop()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Stop took %s; a create-waiting job must not spend the grace", elapsed)
	}
	if !w.wasCancelled() {
		t.Fatal("the create-waiting job was not cancelled")
	}
}
