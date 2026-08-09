// Package api implements the forkd ephemeral-backend lease API.
//
// A sandbox is a lease: create with an image tag + TTL, use via exec,
// release via delete or TTL expiry. Consumers never manage forkd
// snapshots, netns, or warm pools directly.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/jrimmer/forkd-service/forkd"
)

// Lease is a sandbox granted to a consumer for a bounded lifetime.
type Lease struct {
	ID        string // unguessable lease id
	Owner     string // consumer id that owns this lease
	Image     string // snapshot tag
	ForkdID   string // underlying forkd sandbox id
	Address   string // guest address, e.g. "10.42.0.2:8888"
	CreatedAt time.Time
	ExpiresAt time.Time
	released  bool
}

// Store holds the live leases and the warm pool.
type Store struct {
	mu     sync.Mutex
	leases map[string]*Lease
	// pool holds pre-forked forkd sandbox ids per image tag.
	pool map[string][]string
}

func newStore() *Store {
	return &Store{
		leases: make(map[string]*Lease),
		pool:   make(map[string][]string),
	}
}

// newID returns a random unguessable hex id.
func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ForkdClient is the subset of the forkd controller API the lease
// service needs. *forkd.Client satisfies it; tests use a fake.
type ForkdClient interface {
	ListSnapshots(ctx context.Context) ([]forkd.SnapshotInfo, error)
	SnapshotExists(ctx context.Context, tag string) (bool, error)
	Spawn(ctx context.Context, tag string, n int, perChildNetns bool, memoryLimitMiB int) ([]forkd.SandboxInfo, error)
	ListSandboxes(ctx context.Context) ([]forkd.SandboxInfo, error)
	Kill(ctx context.Context, id string) error
	Exec(ctx context.Context, id string, args []string, timeoutSecs int) (*forkd.ExecResult, error)
	Ping(ctx context.Context, id string) error
	Metrics(ctx context.Context) ([]byte, error)
}

// Service is the lease API backend.
type Service struct {
	forkd ForkdClient
	store *Store
	// tokens maps a consumer token to its consumer id.
	tokens map[string]string
	// poolSize is the warm-pool size per image.
	poolSize int
	// defaultTTL is used when a request omits ttl.
	defaultTTL time.Duration
	// maxTTL caps a requested ttl.
	maxTTL time.Duration
	// sweepInterval is the TTL-sweeper tick (overridable in tests).
	sweepInterval time.Duration
	log           *log.Logger
}

// NewService builds a lease service. tokens maps consumer tokens to
// consumer ids. poolSize is the warm-pool size per image (0 disables
// pre-forking). defaultTTL and maxTTL bound lease lifetimes. knownImages
// seeds the warm-pool map so refillPool pre-forks every image at
// startup — without this, an image only becomes warm after its first
// grant, leaving the pool cold after a backend restart.
func NewService(fc ForkdClient, tokens map[string]string, poolSize int, defaultTTL, maxTTL time.Duration, knownImages ...string) *Service {
	s := &Service{
		forkd:         fc,
		store:         newStore(),
		tokens:        tokens,
		poolSize:      poolSize,
		defaultTTL:    defaultTTL,
		maxTTL:        maxTTL,
		sweepInterval: 5 * time.Second,
		log:           log.Default(),
	}
	for _, img := range knownImages {
		if img == "" {
			continue
		}
		s.store.mu.Lock()
		if _, ok := s.store.pool[img]; !ok {
			s.store.pool[img] = nil
		}
		s.store.mu.Unlock()
	}
	return s
}

// Start begins the TTL sweeper and warm-pool refill. It runs until ctx
// is cancelled.
func (s *Service) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(s.sweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.sweepExpired(ctx)
				s.refillPool(ctx)
			}
		}
	}()
}

// refillPool pre-forks poolSize sandboxes for each known image so
// grants can be served from the warm pool instead of cold-spawning.
func (s *Service) refillPool(ctx context.Context) {
	if s.poolSize <= 0 {
		return
	}
	s.store.mu.Lock()
	images := make([]string, 0, len(s.store.pool))
	for img := range s.store.pool {
		images = append(images, img)
	}
	s.store.mu.Unlock()
	for _, img := range images {
		s.warmPool(ctx, img)
	}
}

// sweepExpired kills and removes leases whose TTL has passed.
func (s *Service) sweepExpired(ctx context.Context) {
	s.store.mu.Lock()
	var expired []*Lease
	now := time.Now()
	for _, l := range s.store.leases {
		if !l.released && now.After(l.ExpiresAt) {
			expired = append(expired, l)
		}
	}
	s.store.mu.Unlock()
	for _, l := range expired {
		s.release(ctx, l)
	}
}

// release kills the underlying forkd sandbox and removes the lease.
// The lease is only removed from the store after a successful kill, so
// a transient Kill failure leaves it in place for the sweeper to retry
// rather than leaking the sandbox.
func (s *Service) release(ctx context.Context, l *Lease) {
	s.store.mu.Lock()
	if l.released {
		s.store.mu.Unlock()
		return
	}
	l.released = true
	s.store.mu.Unlock()
	err := s.forkd.Kill(ctx, l.ForkdID)
	if err != nil {
		// A 404 means the sandbox is already gone (e.g. the controller
		// restarted and forgot it) — that's the goal state, not a
		// failure. Treat it as released so we don't retry forever.
		if strings.Contains(err.Error(), "not found") {
			s.log.Printf("release: kill %s: already gone (removing lease)", l.ForkdID)
			s.store.mu.Lock()
			delete(s.store.leases, l.ID)
			s.store.mu.Unlock()
			return
		}
		s.log.Printf("release: kill %s failed: %v (will retry)", l.ForkdID, err)
		// Re-open the lease so the sweeper retries the kill.
		s.store.mu.Lock()
		l.released = false
		s.store.mu.Unlock()
		return
	}
	s.store.mu.Lock()
	delete(s.store.leases, l.ID)
	s.store.mu.Unlock()
}

// grant creates a new lease for owner from the warm pool (or spawns a
// fresh sandbox when the pool is empty).
func (s *Service) grant(ctx context.Context, owner, image string, memoryMiB int, ttl time.Duration) (*Lease, error) {
	// Try the warm pool first, validating that the pooled sandbox still
	// exists in the controller. After a controller restart the backend's
	// in-memory pool holds stale IDs (the controller forgot them); a
	// stale ID would 404 on exec and leave the consumer hanging.
	var forkdID, addr string
	for {
		s.store.mu.Lock()
		pool := s.store.pool[image]
		if len(pool) > 0 {
			forkdID = pool[len(pool)-1]
			s.store.pool[image] = pool[:len(pool)-1]
		}
		// Ensure the image is registered so the warm-pool refill knows to
		// pre-fork it.
		if _, known := s.store.pool[image]; !known {
			s.store.pool[image] = nil
		}
		s.store.mu.Unlock()

		if forkdID == "" {
			break
		}
		// Verify the pooled sandbox is still alive; if not, drop it and
		// try the next one (or cold-spawn below).
		if err := s.forkd.Ping(ctx, forkdID); err == nil {
			addr = ""
			break
		}
		s.log.Printf("grant: pooled %s (%s) is stale (controller forgot it), dropping", forkdID, image)
		_ = s.forkd.Kill(ctx, forkdID)
		forkdID = ""
	}
	if forkdID == "" {
		sbs, err := s.forkd.Spawn(ctx, image, 1, true, memoryMiB)
		if err != nil {
			return nil, err
		}
		if len(sbs) == 0 {
			return nil, errNoSandbox
		}
		forkdID = sbs[0].ID
		addr = sbs[0].GuestAddr
	}

	lease := &Lease{
		ID:        newID(),
		Owner:     owner,
		Image:     image,
		ForkdID:   forkdID,
		Address:   addr,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(ttl),
	}
	s.store.mu.Lock()
	s.store.leases[lease.ID] = lease
	s.store.mu.Unlock()
	return lease, nil
}

// lookup returns the lease with the given id if it belongs to owner.
func (s *Service) lookup(owner, id string) *Lease {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l := s.store.leases[id]
	if l == nil || l.Owner != owner || l.released {
		return nil
	}
	return l
}

// list returns the caller's live leases as plain maps.
func (s *Service) list(owner string) []map[string]any {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	var out []map[string]any
	for _, l := range s.store.leases {
		if l.Owner == owner && !l.released {
			out = append(out, map[string]any{
				"id":      l.ID,
				"image":   l.Image,
				"address": l.Address,
				"expires": l.ExpiresAt.Unix(),
			})
		}
	}
	return out
}

// warmPool pre-forks poolSize sandboxes per image tag.
func (s *Service) warmPool(ctx context.Context, image string) {
	if s.poolSize <= 0 {
		return
	}
	s.store.mu.Lock()
	cur := len(s.store.pool[image])
	s.store.mu.Unlock()
	if cur >= s.poolSize {
		return
	}
	// Spawn one child at a time. forkd's restore_many restores all N
	// children concurrently, and a large snapshot (e.g. elixir-base at
	// 2 GiB) can take longer than forkd's 5s socket timeout when several
	// restores run at once, failing the whole batch. Serializing keeps
	// each restore under the timeout.
	for i := cur; i < s.poolSize; i++ {
		sbs, err := s.forkd.Spawn(ctx, image, 1, true, 0)
		if err != nil {
			s.log.Printf("warmPool: spawn %s: %v", image, err)
			return
		}
		s.store.mu.Lock()
		for _, sb := range sbs {
			s.store.pool[image] = append(s.store.pool[image], sb.ID)
		}
		s.store.mu.Unlock()
	}
}

// LiveLeases returns the count of active (unreleased) leases. Used by
// the shutdown log line.
func (s *Service) LiveLeases() []string {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	ids := make([]string, 0, len(s.store.leases))
	for id, l := range s.store.leases {
		if !l.released {
			ids = append(ids, id)
		}
	}
	return ids
}

// Shutdown kills every lease and pooled sandbox. Called on SIGTERM/
// SIGINT so a backend restart never orphans warm VMs in the controller
// (the controller has no client-liveness concept, so orphaned VMs
// would otherwise hold netns slots forever).
func (s *Service) Shutdown(ctx context.Context) {
	s.store.mu.Lock()
	all := make([]*Lease, 0, len(s.store.leases))
	for _, l := range s.store.leases {
		all = append(all, l)
	}
	// Pool ids are not full leases; kill them directly.
	poolIDs := make([]string, 0)
	for _, ids := range s.store.pool {
		poolIDs = append(poolIDs, ids...)
	}
	s.store.mu.Unlock()

	for _, l := range all {
		s.release(ctx, l)
	}
	for _, id := range poolIDs {
		if err := s.forkd.Kill(ctx, id); err != nil {
			s.log.Printf("shutdown: kill pooled %s failed: %v", id, err)
		}
	}
}

// ReconcileOrphans kills controller sandboxes that this backend did not
// create. On startup the in-memory lease/pool maps are empty, so any
// live sandbox belongs to a previous backend incarnation (or a foreign
// client); killing them frees netns slots that would otherwise be held
// forever.
func (s *Service) ReconcileOrphans(ctx context.Context) {
	sbs, err := s.forkd.ListSandboxes(ctx)
	if err != nil {
		s.log.Printf("reconcile: list sandboxes failed: %v", err)
		return
	}
	s.store.mu.Lock()
	mine := make(map[string]bool)
	for _, l := range s.store.leases {
		mine[l.ForkdID] = true
	}
	for _, ids := range s.store.pool {
		for _, id := range ids {
			mine[id] = true
		}
	}
	s.store.mu.Unlock()

	killed := 0
	for _, sb := range sbs {
		if mine[sb.ID] {
			continue
		}
		if err := s.forkd.Kill(ctx, sb.ID); err != nil {
			s.log.Printf("reconcile: kill orphan %s failed: %v", sb.ID, err)
			continue
		}
		killed++
	}
	if killed > 0 {
		s.log.Printf("reconcile: killed %d orphaned sandbox(es) from a previous incarnation", killed)
	}
}

var errNoSandbox = &leaseError{"no sandbox granted"}

// leaseError is a simple sentinel error.
type leaseError struct{ msg string }

func (e *leaseError) Error() string { return e.msg }
