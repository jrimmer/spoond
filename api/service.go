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
	"fmt"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jrimmer/spoond/forkd"
	"github.com/jrimmer/spoond/identity"
	"github.com/jrimmer/spoond/metrics"
)

// Lease is a sandbox granted to a consumer for a bounded lifetime.
type Lease struct {
	ID         string // unguessable lease id
	Owner      string `json:"owner"` // owner identity (user id or legacy consumer id)
	Image      string // snapshot tag
	ForkdID    string // underlying forkd sandbox id
	Address    string // guest address, e.g. "10.42.0.2:8888"
	CreatedAt  time.Time
	ExpiresAt  time.Time
	Persistent bool      // interactive/persistent lease: not TTL-swept, keep-alive extends
	LastActive time.Time // last activity (exec/stream/proxy/keepalive), for idle sweep
	Workspace  string    // workspace name when workspace-backed (suspend/resume support)
	Suspended  bool      // workspace-backed lease is currently suspended
	Name       string    // optional friendly name/tag (unique per owner; resolved by ssh/proxy)
	NetPolicy  string    // egress policy: none|lan|internet|restricted ("" = lan)
	NetAllow   []string  // allowlist for restricted policy
	Comment    string    // optional free-text annotation (set/cleared via ctl comment)
	released   bool
}

// ShareMode selects which surfaces a share covers.
type ShareMode string

const (
	ShareSSH  ShareMode = "ssh"  // SSH attach / ctl access
	ShareHTTP ShareMode = "http" // proxy / exec API access
)

// Share grants a user access to a lease owned by someone else (T6/#33).
type Share struct {
	LeaseID   string    // the shared lease
	Grantee   string    // user id receiving access
	Mode      ShareMode // ssh | http
	ExpiresAt time.Time // zero = never expires
	CreatedAt time.Time
}

// Store holds the live leases and the warm pool.
type Store struct {
	mu     sync.Mutex
	leases map[string]*Lease
	// pool holds pre-forked forkd sandbox ids per image tag.
	pool map[string][]string
	// shares maps lease id -> grantee id -> share (T6/#33).
	shares map[string]map[string]*Share
	// pending counts in-flight lease creations per owner (T4/#31 quota
	// reservation, security review #37 H2): a slot is reserved under
	// the same lock as the quota count and released when the lease is
	// inserted or the grant fails, closing the check-then-create race.
	pending map[string]int
	// records holds run checkpoints (issue #55): a before/after snapshot
	// pair per sandbox run, replayable via grantFromSnapshot.
	records map[string]*Record
}

func newStore() *Store {
	return &Store{
		leases:  make(map[string]*Lease),
		pool:    make(map[string][]string),
		shares:  make(map[string]map[string]*Share),
		pending: make(map[string]int),
		records: make(map[string]*Record),
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
	Branch(ctx context.Context, id, tag string) (string, error)
	CreateWorkspace(ctx context.Context, name, tag string, perChildNetns bool) (*forkd.WorkspaceInfo, error)
	SuspendWorkspace(ctx context.Context, name string) error
	ResumeWorkspace(ctx context.Context, name string) (*forkd.WorkspaceInfo, error)
	DeleteWorkspace(ctx context.Context, name string) error
	Metrics(ctx context.Context) ([]byte, error)
}

// Service is the lease API backend.
type Service struct {
	forkd ForkdClient
	store *Store
	// tokens maps a consumer token to its consumer id (legacy mode).
	tokens map[string]string
	// identities is the user/identity store (epic #26 T1). When set,
	// bearer-token auth resolves against it first; tokens map remains as
	// the backward-compatible fallback for single-user deployments.
	identities *identity.Store
	// gatewayToken is the SSH gateway's service token (U6/T5). When a
	// request authenticates with it, the X-Spoond-User-Id header is
	// honored so the gateway can act as the SSH-authenticated user; the
	// backend's owner-scoping then applies to that user, not the gateway
	// service identity. Empty disables impersonation.
	gatewayToken string
	// poolSize is the warm-pool size per image.
	poolSize int
	// defaultTTL is used when a request omits ttl.
	defaultTTL time.Duration
	// maxTTL caps a requested ttl.
	maxTTL time.Duration
	// sweepInterval is the TTL-sweeper tick (overridable in tests).
	sweepInterval time.Duration
	// idleTimeout is how long a persistent lease may sit idle before the
	// sweeper reclaims it (auto-suspend). 0 disables idle sweeping.
	idleTimeout time.Duration
	log         *log.Logger
	// netpol applies egress policy to a lease's child netns. Nil means
	// policy enforcement is disabled (tests, or deployments without
	// root ip netns access).
	netpol PolicyApplier
	// netpolDNS is the resolver set allowed under PolicyRestricted.
	netpolDNS []string
	// metrics (issue #20): service-level Prometheus metrics.
	metrics *metrics.BackendMetrics
}

// SetNetpol installs the egress-policy applier and the DNS resolvers
// allowed under the restricted policy. Call before serving.
// SetMetrics installs the Prometheus metrics collector (issue #20).
// Called by the Server after NewServerWithLLM so the service can
// record pool, lease, and netpol events.
func (s *Service) SetMetrics(m *metrics.BackendMetrics) {
	s.metrics = m
}

func (s *Service) SetNetpol(a PolicyApplier, dns []string) {
	s.netpol = a
	s.netpolDNS = dns
}

// SetGatewayToken marks the SSH gateway's service token, enabling
// trusted impersonation (U6/T5): requests carrying this token may set
// X-Spoond-User-Id to act as the SSH-authenticated user.
func (s *Service) SetGatewayToken(tok string) {
	s.gatewayToken = tok
}

// SetIdentities installs the identity store used for token→user and
// key→user resolution. Call before serving; when set, the first user in
// the store is the admin (KTD-2) and legacy consumer tokens still work.
func (s *Service) SetIdentities(ids *identity.Store) {
	s.identities = ids
}

// ResolveOwner resolves a bearer token to an owner identity. It prefers
// the identity store (user id) and falls back to the legacy consumer
// token map for single-user deployments.
func (s *Service) ResolveOwner(token string) (string, bool) {
	if s.identities != nil {
		if u := s.identities.UserByToken(token); u != nil {
			return u.ID, true
		}
	}
	owner, ok := s.tokens[token]
	return owner, ok
}

// applyNetpol enforces a lease's egress policy inside its child netns.
// It is called after grant (fresh sandbox), after resume (new sandbox),
// and after restart. A nil applier disables enforcement.
func (s *Service) applyNetpol(ctx context.Context, l *Lease) error {
	if s.netpol == nil {
		return nil
	}
	if l.NetPolicy == "" {
		l.NetPolicy = string(PolicyLAN)
	}
	// Always apply rules — the netns pool reuses network namespaces, so a
	// previous lease may have left stale FORWARD rules (e.g. restricted).
	// policyCommands flushes FORWARD first, making this idempotent.
	ep, err := s.resolveEndpoint(ctx, l)
	if err != nil {
		return fmt.Errorf("resolve endpoint for policy: %w", err)
	}
	allow := l.NetAllow
	if allow == nil {
		allow = []string{}
	}
	return s.netpol.Apply(ctx, ep.Netns, NetworkPolicy(l.NetPolicy), allow)
}

// NewService builds a lease service. tokens maps consumer tokens to
// consumer ids. poolSize is the warm-pool size per image (0 disables
// pre-forking). defaultTTL and maxTTL bound lease lifetimes. knownImages
// seeds the warm-pool map so refillPool pre-forks every image at
// startup — without this, an image only becomes warm after its first
// grant, leaving the pool cold after a backend restart.
func NewService(fc ForkdClient, tokens map[string]string, poolSize int, defaultTTL, maxTTL time.Duration, knownImages ...string) *Service {
	return NewServiceWithIdle(fc, tokens, poolSize, defaultTTL, maxTTL, 0, knownImages...)
}

// NewServiceWithIdle is NewService plus an idle auto-suspend timeout for
// persistent leases. idleTimeout 0 disables idle sweeping.
func NewServiceWithIdle(fc ForkdClient, tokens map[string]string, poolSize int, defaultTTL, maxTTL, idleTimeout time.Duration, knownImages ...string) *Service {
	s := &Service{
		forkd:         fc,
		store:         newStore(),
		tokens:        tokens,
		poolSize:      poolSize,
		defaultTTL:    defaultTTL,
		maxTTL:        maxTTL,
		idleTimeout:   idleTimeout,
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

// sweepExpired kills and removes leases whose TTL has passed. Persistent
// leases are not TTL-swept (the consumer keeps them alive via keep-alive,
// and disposes via delete), but when idleTimeout is set they are
// auto-suspended after that long without activity (exec/stream/proxy/
// keep-alive all bump LastActive). Workspace-backed leases are suspended
// (state snapshot kept, cheap to resume); plain persistent leases are
// released as before.
func (s *Service) sweepExpired(ctx context.Context) {
	s.store.mu.Lock()
	var expired []*Lease
	var idleSuspend []*Lease
	now := time.Now()
	for _, l := range s.store.leases {
		if l.released {
			continue
		}
		if !l.Persistent && now.After(l.ExpiresAt) {
			expired = append(expired, l)
			continue
		}
		if l.Persistent && s.idleTimeout > 0 && now.After(l.LastActive.Add(s.idleTimeout)) {
			if l.Workspace != "" && !l.Suspended {
				s.log.Printf("idle sweep: suspending persistent lease %s (idle since %s)", l.ID, l.LastActive.Format(time.RFC3339))
				idleSuspend = append(idleSuspend, l)
			} else if l.Workspace == "" {
				s.log.Printf("idle sweep: reclaiming persistent lease %s (idle since %s)", l.ID, l.LastActive.Format(time.RFC3339))
				expired = append(expired, l)
			}
		}
	}
	s.store.mu.Unlock()
	// Suspend idle workspace leases in small, staggered batches. Each
	// suspend is a controller snapshot write that briefly blocks the
	// controller's accept loop; a large backlog (e.g. after a long test
	// session) must not produce one big pause that drops incoming
	// connections. Cap per tick and space them out — with the 5s sweep
	// tick, 13 idle leases clear in ~25s instead of a single burst.
	const maxSuspendPerTick = 3
	suspended := 0
	for _, l := range idleSuspend {
		if suspended >= maxSuspendPerTick {
			break
		}
		if err := s.forkd.SuspendWorkspace(ctx, l.Workspace); err != nil {
			s.log.Printf("idle sweep: suspend %s: %v", l.ID, err)
			continue
		}
		s.store.mu.Lock()
		l.Suspended = true
		s.store.mu.Unlock()
		suspended++
		time.Sleep(500 * time.Millisecond)
	}
	for _, l := range expired {
		s.release(ctx, l)
	}
}

// touch records activity on a lease so the idle sweeper doesn't reclaim
// it. Returns nil for unknown/released leases.
func (s *Service) touch(id string) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if l := s.store.leases[id]; l != nil && !l.released {
		l.LastActive = time.Now()
	}
}

// keepAlive extends a persistent lease's expiry so the sweeper never
// reclaims it. Non-persistent leases are rejected (their TTL is fixed).
func (s *Service) keepAlive(owner, id string, ttl time.Duration) (*Lease, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l, ok := s.store.leases[id]
	if !ok || l.Owner != owner || l.released {
		return nil, errNotFound
	}
	if !l.Persistent {
		return nil, errNotPersistent
	}
	if ttl <= 0 {
		ttl = s.maxTTL
	}
	if ttl > s.maxTTL {
		ttl = s.maxTTL
	}
	l.ExpiresAt = time.Now().Add(ttl)
	l.LastActive = time.Now() // keep-alive is activity
	return l, nil
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

	// Workspace-backed leases: delete the workspace (kills the live
	// sandbox if any AND removes the state snapshot). Plain leases: kill
	// the sandbox.
	var err error
	if l.Workspace != "" {
		err = s.forkd.DeleteWorkspace(ctx, l.Workspace)
		if err != nil && strings.Contains(err.Error(), "not found") {
			err = nil // workspace already gone
		}
	} else {
		err = s.forkd.Kill(ctx, l.ForkdID)
	}
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
// fresh sandbox when the pool is empty). Persistent leases are intended
// for interactive use: they are not TTL-swept (see keepAlive) and the
// consumer drives their lifecycle.
// errQuotaExceeded is returned when a user hits their concurrent-lease
// cap (T4/#31). The API layer maps it to HTTP 429.
var errQuotaExceeded = fmt.Errorf("lease quota exceeded")

// reserveQuota enforces a user's concurrent-lease cap before granting
// and RESERVES a slot atomically (security review #37 H2): the count
// and the reservation happen under the same store lock, so concurrent
// creates cannot both pass max_leases. The caller MUST call
// releaseQuotaReservation when the grant finishes (success or failure).
// Returns errQuotaExceeded when the cap is hit. Owners without an
// identity-store user (legacy consumer tokens) are uncapped.
func (s *Service) reserveQuota(owner string) error {
	if s.identities == nil {
		return nil
	}
	u := s.identities.UserByID(owner)
	if u == nil || u.MaxLeases <= 0 {
		return nil
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	active := 0
	for _, l := range s.store.leases {
		if !l.released && l.Owner == owner {
			active++
		}
	}
	if active+s.store.pending[owner] >= u.MaxLeases {
		if s.metrics != nil {
			s.metrics.QuotaExceeded.Inc()
		}
		return errQuotaExceeded
	}
	s.store.pending[owner]++
	return nil
}

// releaseQuotaReservation drops a reservation made by reserveQuota.
func (s *Service) releaseQuotaReservation(owner string) {
	if s.identities == nil {
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if s.store.pending[owner] <= 1 {
		delete(s.store.pending, owner)
	} else {
		s.store.pending[owner]--
	}
}

func (s *Service) grant(ctx context.Context, owner, image string, memoryMiB int, ttl time.Duration, persistent bool, netPolicy string, netAllow []string) (*Lease, error) {
	if err := s.reserveQuota(owner); err != nil {
		return nil, err
	}
	// The reservation becomes the real lease when it's stored below;
	// the deferred release runs on BOTH success and failure: on failure
	// it frees the slot, on success the lease is already counted as
	// active so the pending reservation must be dropped (security
	// review #37 H2). Both mutations take the same store lock, so a
	// concurrent reserveQuota sees a consistent active+pending count.
	defer func() { s.releaseQuotaReservation(owner) }()
	grantStart := time.Now()
	if s.metrics != nil {
		s.metrics.LeasesTotal.Inc()
		s.metrics.LeaseGrantDur.Observe(time.Since(grantStart).Seconds())
	}
	lease := &Lease{
		ID:         newID(),
		Owner:      owner,
		Image:      image,
		CreatedAt:  time.Now(),
		ExpiresAt:  time.Now().Add(ttl),
		Persistent: persistent,
		LastActive: time.Now(),
		NetPolicy:  netPolicy,
		NetAllow:   netAllow,
	}

	// Persistent leases are workspace-backed so they can suspend/resume
	// (the workspace keeps a state snapshot while stopped). TTL leases
	// use the warm pool for fast grants.
	if persistent {
		ws, err := s.forkd.CreateWorkspace(ctx, "ws-"+lease.ID, image, true)
		if err != nil {
			return nil, fmt.Errorf("create workspace: %w", err)
		}
		lease.Workspace = ws.Name
		lease.ForkdID = ws.LiveSandboxID
		if err := s.fillEndpoint(ctx, lease); err != nil {
			return nil, fmt.Errorf("resolve workspace sandbox: %w", err)
		}
		if err := s.applyNetpol(ctx, lease); err != nil {
			return nil, fmt.Errorf("apply network policy: %w", err)
		}
		s.store.mu.Lock()
		s.store.leases[lease.ID] = lease
		s.store.mu.Unlock()
		return lease, nil
	}

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

	lease.ForkdID = forkdID
	lease.Address = addr
	if err := s.applyNetpol(ctx, lease); err != nil {
		return nil, fmt.Errorf("apply network policy: %w", err)
	}
	s.store.mu.Lock()
	s.store.leases[lease.ID] = lease
	s.store.mu.Unlock()
	return lease, nil
}

// fillEndpoint looks up a lease's sandbox in the controller and fills in
// the Address (guest addr). Used after workspace create/resume where the
// controller response does not carry the endpoint.
func (s *Service) fillEndpoint(ctx context.Context, lease *Lease) error {
	if lease.ForkdID == "" {
		return fmt.Errorf("no sandbox id")
	}
	sbs, err := s.forkd.ListSandboxes(ctx)
	if err != nil {
		return err
	}
	for _, sb := range sbs {
		if sb.ID == lease.ForkdID {
			lease.Address = sb.GuestAddr
			return nil
		}
	}
	return fmt.Errorf("sandbox %s not in controller list", lease.ForkdID)
}

// suspend suspends a workspace-backed persistent lease: the controller
// snapshots the sandbox and stops it. The lease stays; resume restores.
func (s *Service) suspend(ctx context.Context, owner, id string) (*Lease, error) {
	s.store.mu.Lock()
	l := s.store.leases[id]
	if l == nil || l.Owner != owner || l.released {
		s.store.mu.Unlock()
		return nil, errNotFound
	}
	if !l.Persistent {
		s.store.mu.Unlock()
		return nil, errNotPersistent
	}
	if l.Workspace == "" {
		s.store.mu.Unlock()
		return nil, errNotPersistent // not workspace-backed (older lease)
	}
	s.store.mu.Unlock()

	if err := s.forkd.SuspendWorkspace(ctx, l.Workspace); err != nil {
		return nil, err
	}
	s.store.mu.Lock()
	l.Suspended = true
	s.store.mu.Unlock()
	return l, nil
}

// resume restores a suspended workspace-backed lease and refreshes the
// lease's sandbox id (resume spawns a fresh sandbox).
func (s *Service) resume(ctx context.Context, owner, id string) (*Lease, error) {
	s.store.mu.Lock()
	l := s.store.leases[id]
	if l == nil || l.Owner != owner || l.released {
		s.store.mu.Unlock()
		return nil, errNotFound
	}
	if !l.Persistent {
		s.store.mu.Unlock()
		return nil, errNotPersistent
	}
	if l.Workspace == "" {
		s.store.mu.Unlock()
		return nil, errNotPersistent
	}
	s.store.mu.Unlock()

	ws, err := s.forkd.ResumeWorkspace(ctx, l.Workspace)
	if err != nil {
		return nil, err
	}
	s.store.mu.Lock()
	l.ForkdID = ws.LiveSandboxID
	l.LastActive = time.Now()
	l.Suspended = false
	s.store.mu.Unlock()
	if err := s.fillEndpoint(ctx, l); err != nil {
		return nil, err
	}
	if err := s.applyNetpol(ctx, l); err != nil {
		return nil, err
	}
	return l, nil
}

// restart reboots a workspace-backed persistent lease: suspend (snapshot
// + stop) then resume (fresh sandbox from the state snapshot). Idempotent
// for suspended leases (resume alone). Plain leases get a kill + cold
// spawn; non-workspace persistent leases return notPersistent.
func (s *Service) restart(ctx context.Context, owner, id string) (*Lease, error) {
	s.store.mu.Lock()
	l := s.store.leases[id]
	if l == nil || l.Owner != owner || l.released {
		s.store.mu.Unlock()
		return nil, errNotFound
	}
	if !l.Persistent {
		s.store.mu.Unlock()
		return nil, errNotPersistent
	}
	workspace := l.Workspace
	suspended := l.Suspended
	s.store.mu.Unlock()

	if workspace != "" {
		if !suspended {
			if err := s.forkd.SuspendWorkspace(ctx, workspace); err != nil {
				return nil, err
			}
		}
		return s.resume(ctx, owner, id)
	}
	// Plain persistent lease: kill the sandbox, then cold-spawn the image
	// and re-grant the lease on the fresh sandbox.
	if err := s.forkd.Kill(ctx, l.ForkdID); err != nil {
		return nil, err
	}
	sbs, err := s.forkd.Spawn(ctx, l.Image, 1, false, 0)
	if err != nil {
		return nil, err
	}
	if len(sbs) == 0 {
		return nil, fmt.Errorf("spawn returned no sandboxes")
	}
	s.store.mu.Lock()
	l.ForkdID = sbs[0].ID
	l.Suspended = false
	l.LastActive = time.Now()
	s.store.mu.Unlock()
	if err := s.fillEndpoint(ctx, l); err != nil {
		return nil, err
	}
	if err := s.applyNetpol(ctx, l); err != nil {
		return nil, err
	}
	return l, nil
}

// setName assigns a friendly name to a lease. Names must be non-empty,
// at most 63 chars, [a-z0-9][a-z0-9-]* (lowercase, hyphenable, no dots —
// dots belong to the proxy hostname), and unique per owner.
func (s *Service) setName(owner, id, name string) (*Lease, error) {
	if name == "" || len(name) > 63 {
		return nil, fmt.Errorf("name must be 1-63 chars")
	}
	for i, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' && i > 0
		if !ok {
			return nil, fmt.Errorf("name must match [a-z0-9][a-z0-9-]*")
		}
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l := s.store.leases[id]
	if l == nil || l.Owner != owner || l.released {
		return nil, errNotFound
	}
	for _, other := range s.store.leases {
		if other != l && other.Owner == owner && !other.released && other.Name == name {
			return nil, fmt.Errorf("name %q already in use by lease %s", name, other.ID)
		}
	}
	l.Name = name
	return l, nil
}

// setComment sets or clears the free-text annotation on a lease.
// Empty string clears it; comments are informational only (no
// uniqueness constraint, unlike names).
func (s *Service) setComment(owner, id, comment string) (*Lease, error) {
	if len(comment) > 512 {
		return nil, fmt.Errorf("comment must be <= 512 chars")
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l := s.store.leases[id]
	if l == nil || l.Owner != owner || l.released {
		return nil, errNotFound
	}
	l.Comment = comment
	return l, nil
}

// lookupByName returns a live lease with the given name regardless of
// owner. Used by the SSH gateway (username = name) and the public proxy
// (<name>.sandbox.lacy.casa); both treat the name as the capability, the
// same model as lease ids. Names are unique per owner.
func (s *Service) lookupByName(name string) *Lease {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	for _, l := range s.store.leases {
		if !l.released && l.Name == name {
			return l
		}
	}
	return nil
}

// lookupByNameForOwner returns a live lease with the given name owned by
// the given owner (names are unique per owner). Used by the API's
// /api/names endpoint so a caller can only resolve their own names.
func (s *Service) lookupByNameForOwner(owner, name string) *Lease {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	for _, l := range s.store.leases {
		if !l.released && l.Owner == owner && l.Name == name {
			return l
		}
	}
	return nil
}

// lookupUserScoped resolves a proxy hostname label (hex lease id or
// friendly name) to a lease owned by the given user. Used by the HTTP
// proxy under forward-auth (U7/T7): the proxy never resolves another
// owner's leases by id or name.
func (s *Service) lookupUserScoped(owner, label string) *Lease {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	for _, l := range s.store.leases {
		if l.released || l.Owner != owner {
			continue
		}
		if l.ID == label || l.Name == label {
			return l
		}
	}
	return nil
}

// GrantShare shares a lease with another user (T6/#33). Only the owner
// can grant; an existing share is replaced. mode selects the surface
// (ssh | http); ttl limits the share lifetime (0 = never expires).
func (s *Service) GrantShare(owner, leaseID, grantee string, mode ShareMode, ttl time.Duration) error {
	l := s.lookup(owner, leaseID)
	if l == nil {
		return fmt.Errorf("sandbox not found")
	}
	if grantee == "" || grantee == owner {
		return fmt.Errorf("grantee must be a different user")
	}
	if s.identities != nil && s.identities.UserByID(grantee) == nil {
		return fmt.Errorf("no such user: %s", grantee)
	}
	if mode != ShareSSH && mode != ShareHTTP {
		return fmt.Errorf("share mode must be ssh or http")
	}
	sh := &Share{
		LeaseID:   leaseID,
		Grantee:   grantee,
		Mode:      mode,
		CreatedAt: time.Now(),
	}
	if ttl > 0 {
		sh.ExpiresAt = time.Now().Add(ttl)
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if s.store.shares[leaseID] == nil {
		s.store.shares[leaseID] = make(map[string]*Share)
	}
	s.store.shares[leaseID][grantee] = sh
	return nil
}

// RevokeShare removes a grant (T6/#33). Only the owner can revoke.
// Idempotent.
func (s *Service) RevokeShare(owner, leaseID, grantee string) error {
	l := s.lookup(owner, leaseID)
	if l == nil {
		return fmt.Errorf("sandbox not found")
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if m := s.store.shares[leaseID]; m != nil {
		delete(m, grantee)
		if len(m) == 0 {
			delete(s.store.shares, leaseID)
		}
	}
	return nil
}

// ListShares returns shares granted on the caller's leases (T6/#33),
// newest first.
func (s *Service) ListShares(owner string) []*Share {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	var out []*Share
	for _, m := range s.store.shares {
		for _, sh := range m {
			l := s.store.leases[sh.LeaseID]
			if l == nil || l.released || l.Owner != owner {
				continue
			}
			cp := *sh
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// lookupWithShare returns a live lease the caller may access, either as
// owner or via a valid share covering mode. Used by exec/proxy/endpoint
// so shared leases work without weakening owner scoping.
func (s *Service) lookupWithShare(caller, leaseID string, mode ShareMode) *Lease {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l := s.store.leases[leaseID]
	if l == nil || l.released {
		return nil
	}
	if l.Owner == caller {
		return l
	}
	sh := s.store.shares[leaseID][caller]
	if sh == nil || sh.Mode != mode {
		return nil
	}
	if !sh.ExpiresAt.IsZero() && time.Now().After(sh.ExpiresAt) {
		return nil
	}
	return l
}

// grantFromSnapshot spawns a sandbox directly from a specific snapshot
// tag (not via the warm pool) and grants a lease on it. Used by clone:
// the branch tag is fresh, has no pool, and must not be registered as a
// refillable image (refillPool would start pre-forking clone tags).
func (s *Service) grantFromSnapshot(ctx context.Context, owner, tag string, ttl time.Duration, persistent bool) (*Lease, error) {
	// Quota enforcement (security review #37 rescan F1): clone must not
	// bypass the per-user lease cap. Reserve atomically and release on
	// completion (the reservation becomes the real lease on success).
	if err := s.reserveQuota(owner); err != nil {
		return nil, err
	}
	defer func() { s.releaseQuotaReservation(owner) }()
	lease := &Lease{
		ID:         newID(),
		Owner:      owner,
		Image:      tag,
		CreatedAt:  time.Now(),
		ExpiresAt:  time.Now().Add(ttl),
		Persistent: persistent,
		LastActive: time.Now(),
	}
	if persistent {
		ws, err := s.forkd.CreateWorkspace(ctx, "ws-"+lease.ID, tag, true)
		if err != nil {
			return nil, fmt.Errorf("create workspace: %w", err)
		}
		lease.Workspace = ws.Name
		lease.ForkdID = ws.LiveSandboxID
		if err := s.fillEndpoint(ctx, lease); err != nil {
			return nil, fmt.Errorf("resolve workspace sandbox: %w", err)
		}
		if err := s.applyNetpol(ctx, lease); err != nil {
			return nil, fmt.Errorf("apply network policy: %w", err)
		}
		s.store.mu.Lock()
		s.store.leases[lease.ID] = lease
		s.store.mu.Unlock()
		return lease, nil
	}
	sbs, err := s.forkd.Spawn(ctx, tag, 1, true, 0)
	if err != nil {
		return nil, err
	}
	if len(sbs) == 0 {
		return nil, errNoSandbox
	}
	lease.ForkdID = sbs[0].ID
	lease.Address = sbs[0].GuestAddr
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

// lookupAny returns a live lease by id regardless of owner. Used by the
// public HTTP proxy where the lease id in the hostname is the capability
// (same model as the SSH gateway).
func (s *Service) lookupAny(id string) *Lease {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l := s.store.leases[id]
	if l == nil || l.released {
		return nil
	}
	return l
}

// Endpoint describes how to reach a leased sandbox's guest agent.
type Endpoint struct {
	ForkdID   string
	Netns     string
	GuestAddr string
	GuestHost string
}

// resolveEndpoint finds the live sandbox info (netns + guest addr) for a
// lease by asking the controller. GuestHost is the host part of the
// guest address (the agent port is always 8888).
func (s *Service) resolveEndpoint(ctx context.Context, l *Lease) (*Endpoint, error) {
	sbs, err := s.forkd.ListSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	for _, sb := range sbs {
		if sb.ID == l.ForkdID {
			host, _, err := net.SplitHostPort(sb.GuestAddr)
			if err != nil {
				host = sb.GuestAddr
			}
			return &Endpoint{
				ForkdID:   sb.ID,
				Netns:     sb.Netns,
				GuestAddr: sb.GuestAddr,
				GuestHost: host,
			}, nil
		}
	}
	return nil, fmt.Errorf("sandbox %s not running", l.ForkdID)
}

// list returns the caller's live leases as plain maps.
func (s *Service) list(owner string) []map[string]any {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	var out []map[string]any
	for _, l := range s.store.leases {
		if l.Owner == owner && !l.released {
			out = append(out, map[string]any{
				"id":               l.ID,
				"owner":            l.Owner,
				"image":            l.Image,
				"address":          l.Address,
				"expires":          l.ExpiresAt.Unix(),
				"persistent":       l.Persistent,
				"suspended":        l.Suspended,
				"name":             l.Name,
				"comment":          l.Comment,
				"net_policy":       l.NetPolicy,
				"egress_allowlist": l.NetAllow,
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
// CollectMetrics updates live gauge metrics from the current service
// state (issue #20). Called by the Server before gathering metrics
// for /metrics. All store access is under the store lock.
func (s *Service) CollectMetrics(m *metrics.BackendMetrics) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	// Pool: ready count per image.
	for img, ids := range s.store.pool {
		m.PoolReady.WithLabelValues(img).Set(float64(len(ids)))
	}
	// Pool cap = poolSize × number of known images.
	if s.poolSize > 0 {
		m.PoolCap.Set(float64(s.poolSize * len(s.store.pool)))
	}

	// Leases: count non-released.
	active := 0
	for _, l := range s.store.leases {
		if !l.released {
			active++
		}
	}
	m.LeasesActive.Set(float64(active))

	// Quota reservations.
	pending := 0
	for _, n := range s.store.pending {
		pending += n
	}
	m.QuotaReserved.Set(float64(pending))

	// Shares.
	shares := 0
	for _, grantees := range s.store.shares {
		shares += len(grantees)
	}
	m.SharesActive.Set(float64(shares))
}

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
		if s.metrics != nil {
			s.metrics.LeaseOrphaned.Inc()
		}
	}
	if killed > 0 {
		s.log.Printf("reconcile: killed %d orphaned sandbox(es) from a previous incarnation", killed)
	}
}

var errNoSandbox = &leaseError{"no sandbox granted"}
var errNotFound = &leaseError{"sandbox not found"}
var errNotPersistent = &leaseError{"sandbox is not a persistent lease"}

// leaseError is a simple sentinel error.
type leaseError struct{ msg string }

func (e *leaseError) Error() string { return e.msg }
