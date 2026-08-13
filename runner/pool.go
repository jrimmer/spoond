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
	return c
}

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

// run is the worker's main loop. It registers once (or restores
// saved credentials), then polls for jobs indefinitely. Unlike the
// design, the worker does NOT re-register after each
// job — it stays on the same registration and keeps polling.
//
// If savedState is non-nil, the worker first restores the saved UUID
// and token and attempts to poll. If the first Fetch fails (stale
// credentials), it falls back to registering fresh.
func (w *worker) run(ctx context.Context, savedState *RunnerStateEntry, onRegister func(RunnerStateEntry)) {
	var version int64
	restored := false

	// Phase 1: establish registration.
	if savedState != nil && savedState.UUID != "" {
		w.impl.Restore(savedState.UUID, savedState.Token, savedState.ID)
		restored = true
		log.Printf("worker %d: restored %s (id %d)", w.id, w.name, savedState.ID)
	} else {
		if err := w.registerFresh(ctx); err != nil {
			return // registerFresh already retried and logged
		}
		onRegister(w.impl.Credentials())
	}
	w.setState(workerIdle)

	// Phase 2: poll for jobs indefinitely.
	for {
		if ctx.Err() != nil {
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
				if err := w.registerFresh(ctx); err != nil {
					return
				}
				onRegister(w.impl.Credentials())
				w.setState(workerIdle)
			}
			time.Sleep(2 * time.Second)
			continue
		}
		version = newVer
		if job == nil {
			time.Sleep(2 * time.Second)
			continue
		}

		w.setState(workerBusy)
		log.Printf("worker %d: executing job %d", w.id, job.ID)
		if err := w.impl.Run(ctx, job); err != nil {
			log.Printf("worker %d: job %d failed: %v", w.id, job.ID, err)
		}
		// Stay registered — just go back to idle and keep polling.
		w.setState(workerIdle)
	}
}

// registerFresh registers with Forgejo. Registration is always persistent
// Retries with backoff until success or context cancellation.
func (w *worker) registerFresh(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if st, _ := w.snapshot(); st == workerStopped {
			return fmt.Errorf("worker stopped")
		}
		if _, err := w.impl.Register(ctx, w.name, w.token, w.labels); err != nil {
			log.Printf("worker %d: register: %v", w.id, err)
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
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
	}
}

// Start cleans up stale runners, spawns the floor workers, and begins
// the scaling coordinator.
func (p *RunnerPool) Start(ctx context.Context) {
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
	workerName := fmt.Sprintf("%s-%d", p.name, p.nextID)
	w := &worker{
		id:        p.nextID,
		impl:      p.newWorker(),
		name:      workerName,
		token:     p.token,
		labels:    p.labels,
		state:     workerIdle,
		idleSince: time.Now(),
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

	go w.run(ctx, savedPtr, onRegister)
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
