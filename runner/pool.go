package runner

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// RunnerWorker is the per-worker job loop contract. A worker registers
// with Forgejo, fetches jobs, and executes them. Implemented by
// ForgejoAdapter + Executor; injectable for tests.
type RunnerWorker interface {
	Register(ctx context.Context, name, token string, labels []string, ephemeral bool) (int64, error)
	Fetch(ctx context.Context, version int64) (*Job, int64, error)
	Run(ctx context.Context, job *Job) error
}

// WorkerImpl adapts ForgejoAdapter + Executor to RunnerWorker.
type WorkerImpl struct {
	Adapter *ForgejoAdapter
	Exec    *Executor
}

func (w *WorkerImpl) Register(ctx context.Context, name, token string, labels []string, ephemeral bool) (int64, error) {
	return w.Adapter.Register(ctx, name, token, labels, ephemeral)
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

// run is the worker's main loop. It registers once, then polls for
// jobs. After a job completes, the ephemeral registration is invalidated
// so it re-registers fresh before polling again.
func (w *worker) run(ctx context.Context) {
	var version int64
	for {
		if ctx.Err() != nil {
			return
		}
		if st, _ := w.snapshot(); st == workerStopped {
			return
		}

		if _, err := w.impl.Register(ctx, w.name, w.token, w.labels, true); err != nil {
			log.Printf("worker %d: register: %v", w.id, err)
			time.Sleep(5 * time.Second)
			continue
		}
		w.setState(workerIdle)

		// Poll for jobs on this stable registration.
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
			// Ephemeral registration invalidated after one job; break
			// out to re-register fresh.
			break
		}
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
	}
}

// Start spawns the floor workers and begins the scaling coordinator.
func (p *RunnerPool) Start(ctx context.Context) {
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
	w := &worker{
		id:        p.nextID,
		impl:      p.newWorker(),
		name:      fmt.Sprintf("%s-%d", p.name, p.nextID),
		token:     p.token,
		labels:    p.labels,
		state:     workerIdle,
		idleSince: time.Now(),
	}
	p.nextID++
	p.workers[w.id] = w
	go w.run(ctx)
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
