package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// RunnerStateEntry is the persisted Forgejo credential for one worker.
// It allows the worker to reconnect to its existing runner entry after
// a process restart instead of registering a new one.
type RunnerStateEntry struct {
	UUID  string `json:"uuid"`
	Token string `json:"token"`
	ID    int64  `json:"id"`
}

// RunnerState is the on-disk state for all workers, keyed by worker name.
type RunnerState map[string]RunnerStateEntry

// RunnerWorker is the per-worker job loop contract. A worker registers
// with Forgejo, fetches jobs, and executes them. Implemented by
// ForgejoAdapter + Executor; injectable for tests.
type RunnerWorker interface {
	Register(ctx context.Context, name, token string, labels []string) (int64, error)
	Restore(uuid, token string, id int64)
	RunnerID() int64
	Deregister(adminToken string) error
	Credentials() RunnerStateEntry
	Fetch(ctx context.Context, version int64) (*Job, int64, error)
	Run(ctx context.Context, job *Job) error
}

// WorkerImpl adapts ForgejoAdapter + Executor to RunnerWorker.
type WorkerImpl struct {
	Adapter *ForgejoAdapter
	Exec    *Executor
}

func (w *WorkerImpl) Register(ctx context.Context, name, token string, labels []string) (int64, error) {
	return w.Adapter.Register(ctx, name, token, labels)
}
func (w *WorkerImpl) Restore(uuid, token string, id int64) {
	w.Adapter.Restore(uuid, token, id)
}
func (w *WorkerImpl) RunnerID() int64 {
	return w.Adapter.RunnerID()
}
func (w *WorkerImpl) Deregister(adminToken string) error {
	return w.Adapter.DeleteRunner(adminToken, w.Adapter.RunnerID())
}
func (w *WorkerImpl) Credentials() RunnerStateEntry {
	return RunnerStateEntry{
		UUID:  w.Adapter.runnerUUID,
		Token: w.Adapter.runnerToken,
		ID:    w.Adapter.RunnerID(),
	}
}
func (w *WorkerImpl) Fetch(ctx context.Context, version int64) (*Job, int64, error) {
	return w.Adapter.Fetch(ctx, version)
}
func (w *WorkerImpl) Run(ctx context.Context, job *Job) error {
	return w.Exec.Run(ctx, job)
}

// SetCreateWaitNotifier wires the executor's create-wait callback (L3):
// the pool calls it on a fresh worker to learn when a job is waiting for
// a sandbox, so a shutdown can cancel it without the grace.
func (w *WorkerImpl) SetCreateWaitNotifier(fn func(bool)) {
	w.Exec.OnCreateWait = fn
}

// PoolConfig configures the adaptive runner pool.
type PoolConfig struct {
	Floor     int // minimum registered runners (always kept)
	Max       int // maximum registered runners
	ScaleStep int // how many to add/remove per scale event

	ScaleUpDelay   time.Duration // how long all-busy before scaling up
	ScaleDownDelay time.Duration // how long idle before scaling down
	PollInterval   time.Duration

	// StateFile is the path to persist runner UUIDs between restarts.
	// If empty, no state is persisted (workers register fresh every
	// restart — runner entries will accumulate).
	StateFile string

	// AdminToken is a Forgejo admin API token used to delete stale
	// offline runner entries on startup and on scale-down. If empty,
	// cleanup is skipped (stale entries remain until manually removed).
	AdminToken string

	// ForgejoURL is the Forgejo base URL for admin REST API calls
	// (runner cleanup). Required if AdminToken is set.
	ForgejoURL string

	// Leases is the lease API client the pool sweeps at startup (#119):
	// when set, Start releases every job lease (comment
	// "forgejo job <id> …") a previous process of this runner left
	// behind. At start this process runs no jobs, so every job-labelled
	// lease of its token is an orphan.
	Leases LeaseSweeper

	// StopGrace is how long Stop lets running jobs finish after a
	// shutdown begins before it cancels them (RUNNER_STOP_GRACE). Jobs
	// hang off the pool's own jobs context, not the Start context, so
	// only Stop can cancel them. Zero falls back to 10 minutes;
	// negative means cancel immediately.
	StopGrace time.Duration
}

// loadState reads the runner state file from disk. Returns an empty
// (not nil) map if the file doesn't exist or is unreadable.
func loadState(path string) RunnerState {
	st := RunnerState{}
	if path == "" {
		return st
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return st // missing file is normal on first boot
	}
	_ = json.Unmarshal(data, &st) // corrupt file → fresh start
	return st
}

// saveState writes the runner state file atomically (temp + rename).
func saveState(path string, st RunnerState) {
	if path == "" {
		return
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// CleanupStaleRunners queries the Forgejo admin API for offline runners
// whose names start with namePrefix and deletes them. This prevents
// accumulation of dead entries from process restarts, scale-downs, or
// crashes. Returns the number of runners deleted.
func CleanupStaleRunners(baseURL, adminToken, namePrefix string) int {
	if adminToken == "" || baseURL == "" || namePrefix == "" {
		return 0
	}
	deleted := 0
	for page := 1; page <= 20; page++ {
		url := fmt.Sprintf("%s/api/v1/admin/actions/runners?limit=50&page=%d", strings.TrimRight(baseURL, "/"), page)
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			break
		}
		req.Header.Set("Authorization", "token "+adminToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			break
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			break
		}
		var runners []struct {
			ID     int64  `json:"id"`
			Name   string `json:"name"`
			Status string `json:"status"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&runners); err != nil {
			resp.Body.Close()
			break
		}
		resp.Body.Close()
		if len(runners) == 0 {
			break
		}
		for _, r := range runners {
			if r.Status == "offline" && strings.HasPrefix(r.Name, namePrefix) {
				if err := deleteRunnerByID(baseURL, adminToken, r.ID); err == nil {
					deleted++
				}
			}
		}
	}
	return deleted
}

// deleteRunnerByID deletes a single runner via the Forgejo admin API.
func deleteRunnerByID(baseURL, adminToken string, id int64) error {
	url := fmt.Sprintf("%s/api/v1/admin/actions/runners/%d", strings.TrimRight(baseURL, "/"), id)
	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c PoolConfig) withDefaults() PoolConfig {
	if c.Floor <= 0 {
		c.Floor = 3
	}
	if c.Max <= 0 {
		c.Max = 12
	}
	// spawn enforces Max as a hard cap, so a Max below Floor registers
	// fewer runners than the floor promises (Floor=3 with Max=1 starts
	// one worker and serializes every job).
	if c.Max < c.Floor {
		log.Printf("pool: max=%d is below floor=%d; raising max to floor", c.Max, c.Floor)
		c.Max = c.Floor
	}
	if c.ScaleStep <= 0 {
		c.ScaleStep = 3
	}
	if c.ScaleUpDelay <= 0 {
		c.ScaleUpDelay = 10 * time.Second
	}
	if c.ScaleDownDelay <= 0 {
		c.ScaleDownDelay = 60 * time.Second
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 5 * time.Second
	}
	if c.StopGrace == 0 {
		c.StopGrace = DefaultStopGrace
	}
	return c
}

// DefaultStopGrace is the default RUNNER_STOP_GRACE: how long a
// shutting-down runner lets running jobs finish before Stop cancels
// them (and their leases are released by the executor's deferred
// Delete).
const DefaultStopGrace = 600 * time.Second

type workerState int

const (
	workerIdle workerState = iota
	workerBusy
	workerStopped
)

// worker is one registered runner loop.
type worker struct {
	id     int
	impl   RunnerWorker
	name   string
	token  string
	labels []string

	mu        sync.Mutex
	state     workerState
	idleSince time.Time

	// createWait is true while the worker's job is blocked in Create
	// waiting for a sandbox (L3). The pool's Stop cancels such a job at
	// once: it has no sandbox and no work to finish, so there is no grace
	// to spend. Guarded by mu.
	createWait bool

	// cancelJob cancels the job-run context run derives per job (nil
	// while no job is being set up); done is closed when the worker loop
	// exits. Both are guarded by mu. The pool's Stop cancels job runs
	// after the grace period — via the jobs context the run context
	// derives from, so the executor's deferred Delete can still release
	// each job lease on a live context — and waits for the loops to
	// finish, so no goroutine outlives the shutdown.
	cancelJob context.CancelFunc
	done      chan struct{}
}

func (w *worker) setState(s workerState) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.state = s
	if s == workerIdle {
		w.idleSince = time.Now()
	}
}

func (w *worker) snapshot() (workerState, time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state, w.idleSince
}

// setCreateWait records whether the worker's job is currently waiting in
// Create for a sandbox (L3).
func (w *worker) setCreateWait(waiting bool) {
	w.mu.Lock()
	w.createWait = waiting
	w.mu.Unlock()
}

// createWaiting reports whether the worker's job is waiting in Create
// for a sandbox (L3).
func (w *worker) createWaiting() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.createWait
}

// run is the worker's main loop. It registers once (or restores
// saved credentials), then polls for jobs indefinitely. Unlike the
// design, the worker does NOT re-register after each
// job — it stays on the same registration and keeps polling.
//
// stop is the pool's shutdown signal (closed by Stop). It ends the loop
// promptly — no new jobs are fetched once the pool drains — without
// touching the job contexts, which hang off the pool's jobs context
// instead: Stop alone decides when running jobs end (after the grace
// period), so a shutdown signal cannot kill them early, and a job Stop
// cancelled can still report its final state to Forgejo (the jobs
// context is still live while the job winds down). The loop exits when
// Stop closes stop, the pool context is cancelled, or the worker is
// stopped by the scaler.
//
// If savedState is non-nil, the worker first restores the saved UUID
// and token and attempts to poll. If the first Fetch fails (stale
// credentials), it falls back to registering fresh.
func (w *worker) run(ctx context.Context, stop <-chan struct{}, jobs *jobContexts, savedState *RunnerStateEntry, onRegister func(RunnerStateEntry)) {
	defer func() {
		w.mu.Lock()
		if w.done != nil {
			close(w.done)
			w.done = nil
		}
		w.mu.Unlock()
	}()
	var version int64
	restored := false

	// Phase 1: establish registration.
	if savedState != nil && savedState.UUID != "" {
		w.impl.Restore(savedState.UUID, savedState.Token, savedState.ID)
		restored = true
		log.Printf("worker %d: restored %s (id %d)", w.id, w.name, savedState.ID)
	} else {
		if err := w.registerFresh(ctx, stop); err != nil {
			return // registerFresh already retried and logged
		}
		onRegister(w.impl.Credentials())
	}
	w.setState(workerIdle)

	// Phase 2: poll for jobs indefinitely.
	for {
		if w.stopping(ctx, stop) {
			return
		}
		if st, _ := w.snapshot(); st == workerStopped {
			return
		}
		job, newVer, err := w.impl.Fetch(ctx, version)
		if err != nil {
			log.Printf("worker %d: fetch: %v", w.id, err)
			// If we restored and the first fetch fails, credentials are
			// likely stale — fall back to fresh registration.
			if restored {
				log.Printf("worker %d: restored credentials may be stale, re-registering", w.id)
				restored = false
				if err := w.registerFresh(ctx, stop); err != nil {
					return
				}
				onRegister(w.impl.Credentials())
				w.setState(workerIdle)
			}
			if !w.sleep(ctx, stop, 2*time.Second) {
				return
			}
			continue
		}
		version = newVer
		if job == nil {
			if !w.sleep(ctx, stop, 2*time.Second) {
				return
			}
			continue
		}
		// A fetch can be in flight when Stop fires; never launch what
		// comes back once the pool is draining.
		if w.stopping(ctx, stop) {
			return
		}

		w.setState(workerBusy)
		log.Printf("worker %d: executing job %d", w.id, job.ID)
		// The job runs on the pool's jobs context (Stop cancels it after
		// the grace period), never on the loop's ctx: a shutdown signal
		// must not cancel a running job — Stop owns that decision (#119).
		jobCtx, cancel := jobs.derive()
		w.mu.Lock()
		w.cancelJob = cancel
		w.mu.Unlock()
		runErr := w.impl.Run(jobCtx, job)
		cancel()
		w.setCreateWait(false)
		w.mu.Lock()
		w.cancelJob = nil
		w.mu.Unlock()
		if runErr != nil {
			log.Printf("worker %d: job %d failed: %v", w.id, job.ID, runErr)
		}
		// Stay registered — just go back to idle and keep polling.
		w.setState(workerIdle)
		// A shutdown ends the loop here: no new jobs are fetched once
		// the process is draining (also covers a ctx cancellation).
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		default:
		}
	}
}

// stopping reports whether the loop should exit: the pool context is
// cancelled, or the pool is shutting down (stop closed).
func (w *worker) stopping(ctx context.Context, stop <-chan struct{}) bool {
	select {
	case <-ctx.Done():
		return true
	case <-stop:
		return true
	default:
		return false
	}
}

// sleep waits d or until the context is cancelled or the pool shuts
// down; it returns false when the loop should exit.
func (w *worker) sleep(ctx context.Context, stop <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-stop:
		return false
	case <-t.C:
		return true
	}
}

// registerFresh registers with Forgejo. Registration is always persistent
// Retries with backoff until success, the pool context being cancelled,
// or the pool shutting down (stop closed).
func (w *worker) registerFresh(ctx context.Context, stop <-chan struct{}) error {
	for {
		if w.stopping(ctx, stop) {
			return ctx.Err()
		}
		if st, _ := w.snapshot(); st == workerStopped {
			return fmt.Errorf("worker stopped")
		}
		if _, err := w.impl.Register(ctx, w.name, w.token, w.labels); err != nil {
			log.Printf("worker %d: register: %v", w.id, err)
			if !w.sleep(ctx, stop, 5*time.Second) {
				return ctx.Err()
			}
			continue
		}
		log.Printf("worker %d: registered %s", w.id, w.name)
		return nil
	}
}

// RunnerPool runs a set of concurrent runner workers with adaptive
// scaling: it keeps Floor runners registered, scales up by ScaleStep
// when all are busy, and scales back down to Floor when load subsides.
type RunnerPool struct {
	cfg       PoolConfig
	newWorker func() RunnerWorker
	name      string
	token     string
	labels    []string

	mu      sync.Mutex
	workers map[int]*worker
	nextID  int

	// state is the persisted runner credentials, loaded at startup
	// and updated whenever a worker registers or re-registers.
	state     RunnerState
	stateLock sync.Mutex

	// jobs is the lifetime context of the pool's running jobs: worker
	// loops derive each job's context from it. It is cancelled only by
	// Stop — after the grace period — never by the shutdown signal, so
	// Stop alone decides when running jobs end and a cancelled job can
	// still report to Forgejo while it winds down.
	jobs *jobContexts

	// stopCh is closed by Stop; stopOnce keeps it idempotent.
	stopCh   chan struct{}
	stopOnce sync.Once
}

// jobContexts is the pool's job-lifetime context: every job's context
// derives from it, and only the pool's Stop cancels it — after the
// grace period, never at the shutdown signal. derive returns a job's
// context and its cancel.
type jobContexts struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func newJobContexts() *jobContexts {
	ctx, cancel := context.WithCancel(context.Background())
	return &jobContexts{ctx: ctx, cancel: cancel}
}

// derive returns a per-job context derived from the pool's jobs
// context.
func (j *jobContexts) derive() (context.Context, context.CancelFunc) {
	return context.WithCancel(j.ctx)
}

// NewRunnerPool builds an adaptive runner pool. newWorker must return a
// fresh RunnerWorker per call (each worker needs its own registration).
func NewRunnerPool(cfg PoolConfig, newWorker func() RunnerWorker, name, token string, labels []string) *RunnerPool {
	cfg = cfg.withDefaults()
	return &RunnerPool{
		cfg:       cfg,
		newWorker: newWorker,
		name:      name,
		token:     token,
		labels:    labels,
		workers:   map[int]*worker{},
		state:     loadState(cfg.StateFile),
		jobs:      newJobContexts(),
		stopCh:    make(chan struct{}),
	}
}

// Stop shuts the pool down gracefully. It closes the stop channel — the
// workers stop fetching (an in-flight fetch result is discarded, never
// launched) and the coordinator returns — and lets running jobs finish
// for up to StopGrace. When the grace is spent it cancels the jobs
// context, which cancels every running job; each job's executor then
// reports its final state (cancelled) to Forgejo on a context that is
// still live, and its deferred Delete releases the job's lease. Stop
// waits for every worker loop to exit, so no goroutine outlives the
// shutdown; the caller's context is never touched. It is safe to call
// more than once.
func (p *RunnerPool) Stop() {
	p.stopOnce.Do(func() {
		close(p.stopCh)
		deadline := time.Now().Add(p.cfg.StopGrace)
		if p.cfg.StopGrace > 0 {
			log.Printf("pool: draining, waiting up to %s for running jobs", p.cfg.StopGrace)
		}
		// A job that has not obtained its sandbox yet (still waiting in
		// Create for capacity) has no work to finish: cancel it at once
		// (L3), so the drain does not spend RUNNER_STOP_GRACE on it. A
		// job running steps keeps the existing grace.
		p.cancelWaitingJobs()
		for {
			p.mu.Lock()
			busy := 0
			for _, w := range p.workers {
				if st, _ := w.snapshot(); st == workerBusy {
					busy++
				}
			}
			p.mu.Unlock()
			if busy == 0 {
				break
			}
			if p.cfg.StopGrace >= 0 && time.Now().Before(deadline) {
				// A job may enter its create wait while another finishes;
				// cancel it too rather than letting it wait out the grace.
				p.cancelWaitingJobs()
				time.Sleep(20 * time.Millisecond)
				continue
			}
			// Grace spent (or negative): cancel the jobs context. Every
			// running job's context dies with it — the executor reports
			// the cancelled state to Forgejo on a live context and its
			// deferred Delete releases the job's lease.
			log.Printf("pool: stop grace spent, cancelling %d running job(s)", busy)
			p.jobs.cancel()
			break
		}
		// Wait for every worker loop to see the shutdown (no goroutine
		// outlives it), in parallel so the total wait is bounded by the
		// per-worker cap, not its multiple.
		p.mu.Lock()
		workers := make([]*worker, 0, len(p.workers))
		for _, w := range p.workers {
			workers = append(workers, w)
		}
		p.mu.Unlock()
		var wg sync.WaitGroup
		for _, w := range workers {
			wg.Add(1)
			go func(w *worker) {
				defer wg.Done()
				w.mu.Lock()
				done := w.done
				w.mu.Unlock()
				if done == nil {
					return
				}
				select {
				case <-done:
				case <-time.After(30 * time.Second):
					log.Printf("pool: worker %d did not exit within 30s", w.id)
				}
			}(w)
		}
		wg.Wait()
		log.Printf("pool: stopped")
	})
}

// cancelWaitingJobs cancels every worker job that is still waiting in
// Create for a sandbox (L3). Such a job has no sandbox and no work to
// finish, so a shutdown must not spend RUNNER_STOP_GRACE on it; the
// executor's Create returns the cancellation and reports the job
// cancelled. Jobs running steps are left for the grace.
func (p *RunnerPool) cancelWaitingJobs() {
	p.mu.Lock()
	workers := make([]*worker, 0, len(p.workers))
	for _, w := range p.workers {
		workers = append(workers, w)
	}
	p.mu.Unlock()
	for _, w := range workers {
		w.mu.Lock()
		waiting := w.createWait
		cancel := w.cancelJob
		w.mu.Unlock()
		if waiting && cancel != nil {
			log.Printf("pool: cancelling worker %d's job while it waits for a sandbox", w.id)
			cancel()
		}
	}
}

// Start cleans up stale runners, releases the previous process's
// orphaned job leases, spawns the floor workers, and begins the scaling
// coordinator. The context governs the pool's own loops (polling,
// scaling, registering) — never the running jobs: those hang off the
// pool's jobs context, which only Stop cancels (after the grace
// period), so a shutdown signal cannot kill a job mid-run (#119).
func (p *RunnerPool) Start(ctx context.Context) {
	// Release the job leases (#119) a previous process of this runner
	// left behind: at start this process runs nothing, so every
	// job-labelled lease of its token is an orphan, which would otherwise
	// keep its sandbox until its TTL runs out. The sweep runs detached from ctx
	// (a shutdown racing the sweep must not leave half-deleted
	// orphans); each delete carries its own timeout in the adapter.
	if p.cfg.Leases != nil {
		sweepCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		n, err := p.cfg.Leases.SweepOrphans(sweepCtx, nil)
		cancel()
		if err != nil {
			log.Printf("pool: orphan lease sweep incomplete: %v", err)
		} else if n > 0 {
			log.Printf("pool: swept %d orphaned job lease(s)", n)
		}
	}
	// Clean up stale offline runners from previous process lifetimes.
	if p.cfg.AdminToken != "" && p.cfg.ForgejoURL != "" {
		n := CleanupStaleRunners(p.cfg.ForgejoURL, p.cfg.AdminToken, p.name)
		if n > 0 {
			log.Printf("pool: cleaned up %d stale offline runners", n)
		}
	}
	for i := 0; i < p.cfg.Floor; i++ {
		p.spawn(ctx)
	}
	go p.coordinate(ctx)
}

func (p *RunnerPool) spawn(ctx context.Context) *worker {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.workers) >= p.cfg.Max {
		return nil
	}
	// No new workers once a shutdown has begun — draining means draining.
	select {
	case <-p.stopCh:
		return nil
	default:
	}
	workerName := fmt.Sprintf("%s-%d", p.name, p.nextID)
	w := &worker{
		id:        p.nextID,
		impl:      p.newWorker(),
		name:      workerName,
		token:     p.token,
		labels:    p.labels,
		state:     workerIdle,
		idleSince: time.Now(),
		done:      make(chan struct{}),
	}
	// Let the worker's executor report create waits, so Stop can cancel a
	// still-waiting job without the grace (L3).
	if r, ok := w.impl.(CreateWaitReporter); ok {
		r.SetCreateWaitNotifier(w.setCreateWait)
	}
	p.nextID++
	p.workers[w.id] = w

	// Look up saved state for this worker name.
	p.stateLock.Lock()
	saved := p.state[workerName]
	p.stateLock.Unlock()
	var savedPtr *RunnerStateEntry
	if saved.UUID != "" {
		savedPtr = &saved
	}

	// onRegister is called whenever the worker registers or
	// re-registers, so we can persist the new credentials.
	onRegister := func(entry RunnerStateEntry) {
		p.stateLock.Lock()
		p.state[workerName] = entry
		saveState(p.cfg.StateFile, p.state)
		p.stateLock.Unlock()
	}

	go w.run(ctx, p.stopCh, p.jobs, savedPtr, onRegister)
	log.Printf("pool: spawned worker %d (total %d)", w.id, len(p.workers))
	return w
}

func (p *RunnerPool) stop(w *worker) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.workers[w.id]; !ok {
		return
	}
	w.setState(workerStopped)
	delete(p.workers, w.id)

	// Deregister from Forgejo and remove from saved state.
	if p.cfg.AdminToken != "" {
		if err := w.impl.Deregister(p.cfg.AdminToken); err != nil {
			log.Printf("pool: worker %d deregister: %v", w.id, err)
		}
	}
	p.stateLock.Lock()
	delete(p.state, w.name)
	saveState(p.cfg.StateFile, p.state)
	p.stateLock.Unlock()

	log.Printf("pool: stopped worker %d (total %d)", w.id, len(p.workers))
}

func (p *RunnerPool) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.workers)
}

func (p *RunnerPool) coordinate(ctx context.Context) {
	t := time.NewTicker(p.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopCh:
			return
		case <-t.C:
			p.scale(ctx)
		}
	}
}

// scale applies the adaptive policy once.
func (p *RunnerPool) scale(ctx context.Context) {
	p.mu.Lock()
	total := len(p.workers)
	if total == 0 {
		p.mu.Unlock()
		return
	}
	busy := 0
	var idleWorkers []*worker
	now := time.Now()
	for _, w := range p.workers {
		st, _ := w.snapshot()
		switch st {
		case workerBusy:
			busy++
		case workerIdle:
			idleWorkers = append(idleWorkers, w)
		}
	}
	p.mu.Unlock()

	// Scale up: all busy -> add ScaleStep (up to Max).
	if busy == total && total < p.cfg.Max {
		toAdd := p.cfg.ScaleStep
		if total+toAdd > p.cfg.Max {
			toAdd = p.cfg.Max - total
		}
		for i := 0; i < toAdd; i++ {
			p.spawn(ctx)
		}
		return
	}

	// Scale down: idle workers beyond Floor, idle for ScaleDownDelay.
	if total > p.cfg.Floor {
		for _, w := range idleWorkers {
			if total <= p.cfg.Floor {
				break
			}
			if now.Sub(w.idleSince) >= p.cfg.ScaleDownDelay {
				p.stop(w)
				total--
			}
		}
	}
}
