// Package api implements the lease API backend on the Substrate
// interface (U08): every lease operation is an E2B sandbox operation
// recorded in SQLite. Consumers never manage templates, builds, pools
// or egress directly.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jrimmer/spoond/identity"
	"github.com/jrimmer/spoond/metrics"
	"github.com/jrimmer/spoond/store"
	"github.com/jrimmer/spoond/substrate"
	"github.com/jrimmer/spoond/substrate/e2b"
)

// Lease is a sandbox granted to a consumer for a bounded lifetime.
type Lease struct {
	ID         string // unguessable lease id
	Owner      string `json:"owner"` // owner identity (user id or legacy consumer id)
	Image      string // image name in the catalog
	SandboxID  string // underlying E2B sandbox id
	HostIP     string // host-side address of the running sandbox, no port
	BuildID    string // build the running sandbox was created from ("" when suspended)
	TemplateID string // template of the lease's image (looked up, not persisted)
	CreatedAt  time.Time
	ExpiresAt  time.Time
	Persistent bool      // interactive lease: not TTL-swept, keep-alive extends
	LastActive time.Time // last activity (exec/stream/proxy/keepalive), for idle sweep
	Suspended  bool      // derived from State == "suspended"
	Name       string    // optional friendly name/tag (unique per owner; resolved by ssh/proxy)
	NetPolicy  string    // egress policy: none|lan|internet|restricted ("" = restricted)
	NetAllow   []string  // allowlist for restricted policy
	// ExposePorts are guest TCP ports published to other sandboxes:
	// peers reach them as their own egress policy permits
	// (peerAllowances).
	ExposePorts []int
	// ExposedIP mirrors HostIP (kept for the store column).
	ExposedIP string
	Comment   string // optional free-text annotation (set/cleared via ctl comment)
	// Lifecycle state kept in the store (U05); later units set the
	// checkpoint/recovery fields. State is running|suspended|recovered|lost
	// ("" = derived from Suspended).
	State                 string
	ResumeBuildID         string    // build a suspended lease resumes from
	LastCheckpointBuildID string    // newest checkpoint build of this lease
	LastCheckpointAt      time.Time // zero = never checkpointed
	RecoveredFrom         time.Time // zero = never recovered
	// pooled marks a lease served from the warm pool: the sandbox's envd
	// default SPOOND_LEASE_ID is "pool" (env vars cannot be updated after
	// create), so exec/stream/stat/prompt add the lease id per request.
	// Not persisted.
	released bool
	pooled   bool
}

// live reports whether the lease has a running sandbox. "recovered" is
// introduced in U10 and behaves exactly like "running".
func (l *Lease) live() bool { return l.State == "running" || l.State == "recovered" }

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
	// pool holds pre-created E2B sandbox ids per image name, oldest first.
	pool map[string][]string
	// shares maps lease id -> grantee id -> share (T6/#33).
	shares map[string]map[string]*Share
	// pending counts in-flight lease creations per owner (T4/#31 quota
	// reservation, security review #37 H2): a slot is reserved under
	// the same store lock as the quota count and released when the lease
	// is inserted or the grant fails, closing the check-then-create race.
	pending map[string]int
	// lastActiveDirty batches touch() updates; the sweeper flushes them
	// to the store once per tick instead of writing on every activity.
	lastActiveDirty map[string]time.Time
}

func newStore() *Store {
	return &Store{
		leases:          make(map[string]*Lease),
		pool:            make(map[string][]string),
		shares:          make(map[string]map[string]*Share),
		pending:         make(map[string]int),
		lastActiveDirty: make(map[string]time.Time),
	}
}

// newID returns a random unguessable hex id.
func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ServiceConfig carries the constructor tunables.
type ServiceConfig struct {
	PoolSize                        int
	DefaultTTL, MaxTTL, IdleTimeout time.Duration
	HostGuestAddr                   string        // HOST_GUEST_SERVICE_ADDR
	HostGuestPort                   int           // HOST_GUEST_SERVICE_PORT
	CheckpointEvery                 time.Duration // U10
}

// Service is the lease API backend.
type Service struct {
	sub substrate.Substrate
	// db persists leases, shares, the pool and the sandbox catalog
	// (U05/U08). Required.
	db    *store.DB
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
	// cfg carries the pool size, TTL bounds, idle timeout and the
	// host-service address guests use (LLM gateway, proxy, assets).
	cfg ServiceConfig
	// probeEnabled runs integrityProbe inside each sandbox before it is
	// pooled or handed to a lease. probeTimeout bounds that exec.
	probeEnabled bool
	probeTimeout time.Duration
	// sweepInterval is the TTL-sweeper tick (overridable in tests).
	sweepInterval time.Duration
	// refreshMu serializes refreshPeers runs, which are scheduled
	// asynchronously after lifecycle events (U09).
	refreshMu sync.Mutex
	// appliedEgress remembers the canonical JSON of the egress config
	// last applied to each lease's sandbox, keyed by lease id, so
	// refreshPeers only calls UpdateEgress on change.
	appliedMu     sync.Mutex
	appliedEgress map[string]string
	log           *log.Logger
	// metrics (issue #20): service-level Prometheus metrics.
	metrics *metrics.BackendMetrics
	// stopLoops cancels the background sweeper/refiller started by Start.
	stopLoops context.CancelFunc
}

// NewService builds the lease service on sub. db is required: every
// mutation is persisted (U05). tokens maps legacy consumer tokens to
// consumer ids.
func NewService(sub substrate.Substrate, db *store.DB, tokens map[string]string, cfg ServiceConfig) *Service {
	return &Service{
		sub:           sub,
		db:            db,
		store:         newStore(),
		tokens:        tokens,
		cfg:           cfg,
		sweepInterval: 5 * time.Second,
		appliedEgress: map[string]string{},
		log:           log.Default(),
		probeEnabled:  true,
		probeTimeout:  20 * time.Second,
	}
}

// SetMetrics installs the Prometheus metrics collector (issue #20).
// Called by the Server after NewServerWithLLM so the service can
// record pool, lease and quota events.
func (s *Service) SetMetrics(m *metrics.BackendMetrics) {
	s.metrics = m
}

// SetGatewayToken marks the SSH gateway's service token, enabling
// trusted impersonation (U6/T5): requests carrying this token may set
// X-Spoond-User-Id to act as the SSH-authenticated user.
func (s *Service) SetGatewayToken(tok string) {
	s.gatewayToken = tok
}

// SetSandboxProbe configures the per-create integrity probe. enabled=false
// turns it off (every sandbox is then handed out unverified); timeout<=0
// leaves the default in place.
func (s *Service) SetSandboxProbe(enabled bool, timeout time.Duration) {
	s.probeEnabled = enabled
	if timeout > 0 {
		s.probeTimeout = timeout
	}
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

// exposedMap renders a lease's published ports as {"<port>": "<ip>:<port>"}.
// The published address is the sandbox's host address; only a live lease
// with a host address has anything to reach.
func exposedMap(l *Lease) map[string]string {
	out := map[string]string{}
	if !l.live() || l.HostIP == "" {
		return out
	}
	for _, p := range l.ExposePorts {
		out[fmt.Sprint(p)] = net.JoinHostPort(l.HostIP, fmt.Sprint(p))
	}
	return out
}

// lanRanges is RFC 1918 minus the sandbox networks 10.11.0.0/16 and
// 10.12.0.0/16: the private CIDRs the lan and internet policies permit.
var lanRanges = []string{
	"10.0.0.0/13",
	"10.8.0.0/15",
	"10.10.0.0/16",
	"10.13.0.0/16",
	"10.14.0.0/15",
	"10.16.0.0/12",
	"10.32.0.0/11",
	"10.64.0.0/10",
	"10.128.0.0/9",
	"172.16.0.0/12",
	"192.168.0.0/16",
}

// egressForLocked builds the lease's E2B egress policy. hostSvc is the
// host-service allowance every guest needs (LLM gateway, proxy, assets);
// dns is the guest resolver. The caller holds s.store.mu: in U09
// peerAllowances reads other leases.
func (s *Service) egressForLocked(l *Lease) substrate.Egress {
	hostSvc := substrate.PrivateAllowance{
		CIDR:     s.cfg.HostGuestAddr + "/32",
		TCPPorts: []uint32{uint32(s.cfg.HostGuestPort)},
	}
	dns := substrate.PrivateAllowance{CIDR: "10.1.0.1/32", TCPPorts: []uint32{53}}
	policy := l.NetPolicy
	if policy == "" {
		policy = string(PolicyRestricted)
	}
	switch NetworkPolicy(policy) {
	case PolicyNone:
		return substrate.Egress{DeniedCIDRs: []string{"0.0.0.0/0"}}
	case PolicyInternet:
		// Public destinations stay allowed; listing the LAN ranges as
		// private allowances matches forkd, where internet flushed all
		// rules and private/LAN addresses stayed reachable.
		return substrate.Egress{Private: append(lanPrivate(l, hostSvc, dns), s.peerAllowances(l)...)}
	case PolicyLAN:
		return substrate.Egress{
			DeniedCIDRs: []string{"0.0.0.0/0"},
			Private:     append(lanPrivate(l, hostSvc, dns), s.peerAllowances(l)...),
		}
	default: // restricted: the default when empty
		eg := substrate.Egress{
			DeniedCIDRs: []string{"0.0.0.0/0"},
			Private:     []substrate.PrivateAllowance{hostSvc, dns},
		}
		for _, entry := range l.NetAllow {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			cidr := entry
			if ip := net.ParseIP(entry); ip != nil {
				cidr = ip.String() + "/32"
			} else if _, n, err := net.ParseCIDR(entry); err == nil {
				cidr = n.String()
			} else {
				// A NetAllow entry that names a lease (id, name, or the
				// "lease:"-prefixed forms) is a peer reference, not a
				// domain: peers are permitted through peerAllowances,
				// never resolved as domains (U09).
				if s.isPeerReference(entry) {
					continue
				}
				eg.AllowedDomains = append(eg.AllowedDomains, entry)
				continue
			}
			if isPrivateCIDR(cidr) {
				eg.Private = append(eg.Private, substrate.PrivateAllowance{CIDR: cidr})
			} else {
				eg.AllowedCIDRs = append(eg.AllowedCIDRs, cidr)
			}
		}
		eg.Private = append(eg.Private, s.peerAllowances(l)...)
		return eg
	}
}

// lanPrivate is the allowance list of the lan and internet policies:
// the LAN ranges (empty port scope), then the host service and DNS.
func lanPrivate(l *Lease, hostSvc, dns substrate.PrivateAllowance) []substrate.PrivateAllowance {
	out := make([]substrate.PrivateAllowance, 0, len(lanRanges)+2)
	for _, cidr := range lanRanges {
		out = append(out, substrate.PrivateAllowance{CIDR: cidr})
	}
	out = append(out, hostSvc, dns)
	return out
}

// isPrivateCIDR reports whether cidr lies within 10/8, 172.16/12,
// 192.168/16, 127/8 or 169.254/16.
func isPrivateCIDR(cidr string) bool {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return false
	}
	for _, base := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8", "169.254.0.0/16"} {
		_, b, _ := net.ParseCIDR(base)
		if b.Contains(n.IP) {
			return true
		}
	}
	return false
}

// egressFor is egressForLocked with the store lock taken. Call it where
// the lock is not held; call egressForLocked where it is (item 18 paths).
func (s *Service) egressFor(l *Lease) substrate.Egress {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	return s.egressForLocked(l)
}

// peerAllowances permits egress into ranges other leases own (U09): the
// exposed ports of every other live lease that publishes them, on any
// owner — parity with the old shared bridge, where every published port
// was reachable from every sandbox whose own policy let it route there.
// Under restricted, a peer counts only when the allowlist names it. It
// is always called with s.store.mu held (from egressForLocked).
func (s *Service) peerAllowances(l *Lease) []substrate.PrivateAllowance {
	policy := l.NetPolicy
	if policy == "" {
		policy = string(PolicyRestricted)
	}
	if NetworkPolicy(policy) == PolicyNone {
		return nil
	}
	restricted := NetworkPolicy(policy) == PolicyRestricted
	var out []substrate.PrivateAllowance
	for _, p := range s.store.leases {
		if p.ID == l.ID || len(p.ExposePorts) == 0 || !p.live() || p.HostIP == "" {
			continue
		}
		if restricted && !netAllowNamesPeer(l.NetAllow, p) {
			continue
		}
		ports := make([]uint32, 0, len(p.ExposePorts))
		for _, port := range p.ExposePorts {
			ports = append(ports, uint32(port))
		}
		out = append(out, substrate.PrivateAllowance{CIDR: p.HostIP + "/32", TCPPorts: ports})
	}
	return out
}

// netAllowNamesPeer reports whether an egress allowlist names peer p:
// by id, by friendly name, or in the "lease:"+id / "lease:"+name forms.
func netAllowNamesPeer(allow []string, p *Lease) bool {
	for _, a := range allow {
		a = strings.TrimSpace(a)
		a = strings.TrimPrefix(a, "lease:")
		if a == p.ID || (p.Name != "" && a == p.Name) {
			return true
		}
	}
	return false
}

// isPeerReference reports whether a restricted allowlist entry names a
// lease — by id, by friendly name, or "lease:"-prefixed — and is
// therefore a peer reference rather than a domain. Called with
// s.store.mu held (from egressForLocked).
func (s *Service) isPeerReference(entry string) bool {
	name := strings.TrimPrefix(strings.TrimSpace(entry), "lease:")
	if name == "" {
		return false
	}
	for _, p := range s.store.leases {
		if p.ID == name || (p.Name != "" && p.Name == name) {
			return true
		}
	}
	return false
}

// canonicalEgress renders eg as the canonical JSON refreshPeers compares
// against the last applied value: sorted CIDR and domain lists, Private
// sorted by CIDR, and every allowance's TCPPorts sorted.
func canonicalEgress(eg substrate.Egress) string {
	eg.AllowedCIDRs = append([]string(nil), eg.AllowedCIDRs...)
	eg.DeniedCIDRs = append([]string(nil), eg.DeniedCIDRs...)
	eg.AllowedDomains = append([]string(nil), eg.AllowedDomains...)
	sort.Strings(eg.AllowedCIDRs)
	sort.Strings(eg.DeniedCIDRs)
	sort.Strings(eg.AllowedDomains)
	eg.Private = append([]substrate.PrivateAllowance(nil), eg.Private...)
	sort.Slice(eg.Private, func(i, j int) bool { return eg.Private[i].CIDR < eg.Private[j].CIDR })
	for i, a := range eg.Private {
		ports := append([]uint32(nil), a.TCPPorts...)
		sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
		eg.Private[i].TCPPorts = ports
	}
	b, err := json.Marshal(eg)
	if err != nil {
		return "" // Egress carries only strings and ints: cannot fail
	}
	return string(b)
}

// recordAppliedEgress remembers the canonical form of the egress config
// just applied to a lease's sandbox (create, pooled grant, or a live
// update), so the next refreshPeers does not re-apply it.
func (s *Service) recordAppliedEgress(leaseID string, eg substrate.Egress) {
	s.appliedMu.Lock()
	defer s.appliedMu.Unlock()
	s.appliedEgress[leaseID] = canonicalEgress(eg)
}

// refreshPeers re-applies every live lease's egress config whose value
// changed (U09): peer allowances move when leases expose ports, go live,
// or are released, and each affected sandbox needs an UpdateEgress. The
// store lock is held only to read state; substrate calls run without it.
// Callers must hold refreshMu (see runRefreshPeers).
func (s *Service) refreshPeers(ctx context.Context) {
	type update struct {
		leaseID, sandboxID, canon string
		eg                        substrate.Egress
	}
	s.store.mu.Lock()
	var upds []update
	for _, l := range s.store.leases {
		policy := l.NetPolicy
		if policy == "" {
			policy = string(PolicyRestricted)
		}
		if !l.live() || NetworkPolicy(policy) == PolicyNone || l.SandboxID == "" {
			continue
		}
		eg := s.egressForLocked(l)
		upds = append(upds, update{leaseID: l.ID, sandboxID: l.SandboxID, canon: canonicalEgress(eg), eg: eg})
	}
	s.store.mu.Unlock()

	for _, u := range upds {
		s.appliedMu.Lock()
		unchanged := s.appliedEgress[u.leaseID] == u.canon
		s.appliedMu.Unlock()
		if unchanged {
			continue
		}
		if err := s.sub.UpdateEgress(ctx, u.sandboxID, u.eg); err != nil {
			s.log.Printf("refreshPeers: update egress for %s: %v", u.leaseID, err)
			continue
		}
		s.appliedMu.Lock()
		s.appliedEgress[u.leaseID] = u.canon
		s.appliedMu.Unlock()
	}
}

// refreshPeersAsync schedules one refreshPeers run on a goroutine. Runs
// are serialized by refreshMu; ctx cancellation does not stop the run —
// the refresh must outlive the request that triggered it. Called after
// grant/resume/restart/clone/fork/release of a lease with ExposePorts
// and after any network policy change (U09).
func (s *Service) refreshPeersAsync(ctx context.Context) {
	go s.runRefreshPeers(ctx)
}

// runRefreshPeers runs refreshPeers serialized by refreshMu.
func (s *Service) runRefreshPeers(ctx context.Context) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	s.refreshPeers(context.WithoutCancel(ctx))
}

// createSandbox admits and creates one sandbox for lease l from build b.
// sandboxID "" allocates a new E2B sandbox id; resume reuses the paused
// sandbox's id. On success the sandboxes row is upserted (upsert because
// resume reuses the sandbox id).
func (s *Service) createSandbox(ctx context.Context, img store.ImageRow, b store.BuildRow, resume bool, sandboxID string, l *Lease) (substrate.Sandbox, error) {
	if err := s.admit(ctx, b.MemoryMB); err != nil {
		return substrate.Sandbox{}, err
	}
	if sandboxID == "" {
		sandboxID = e2b.NewSandboxID()
	}
	env := make(map[string]string, len(img.Env)+2)
	for k, v := range img.Env {
		env[k] = v
	}
	env["SPOOND_LEASE_ID"] = l.ID
	env["SPOOND_GATEWAY_URL"] = "http://" + s.cfg.HostGuestAddr + ":" + strconv.Itoa(s.cfg.HostGuestPort)
	eg := s.egressFor(l)
	sb, err := s.sub.Create(ctx, substrate.CreateRequest{
		TemplateID:         b.TemplateID,
		BuildID:            b.BuildID,
		SandboxID:          sandboxID,
		KernelVersion:      b.KernelVersion,
		FirecrackerVersion: b.FirecrackerVersion,
		EnvdVersion:        b.EnvdVersion,
		VCPU:               uint32(b.VCPU),
		MemoryMB:           uint32(b.MemoryMB),
		DiskSizeMB:         uint32(b.DiskMB),
		Resume:             resume,
		EnvVars:            env,
		Metadata:           map[string]string{"lease_id": l.ID, "owner": l.Owner},
		EndAt:              l.ExpiresAt,
		Egress:             eg,
	})
	if err != nil {
		return substrate.Sandbox{}, err
	}
	s.recordAppliedEgress(l.ID, eg)
	leaseID := l.ID
	if leaseID == "pool" {
		leaseID = "" // pool placeholder: pool sandboxes have no lease
	}
	s.upsertSandboxRow(store.SandboxRow{
		SandboxID:   sb.ID,
		LeaseID:     leaseID,
		BuildID:     sb.BuildID,
		ExecutionID: sb.ExecutionID,
		HostIP:      sb.HostIP,
		VCPU:        int(sb.VCPU),
		MemoryMB:    int(sb.MemoryMB),
		StartedAt:   sb.StartedAt,
		EndAt:       sb.EndAt,
	})
	return sb, nil
}

// Start begins the TTL sweeper and warm-pool refill. It runs until ctx
// is cancelled or Shutdown stops it.
func (s *Service) Start(ctx context.Context) {
	ctx, s.stopLoops = context.WithCancel(ctx)
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

// refillPool pre-creates cfg.PoolSize sandboxes for every image with a
// current build so grants can be served from the warm pool instead of
// cold-creating.
func (s *Service) refillPool(ctx context.Context) {
	if s.cfg.PoolSize <= 0 {
		return
	}
	imgs, err := s.db.ListImages(ctx)
	if err != nil {
		s.log.Printf("refillPool: list images: %v", err)
		return
	}
	for _, img := range imgs {
		if img.CurrentBuildID == "" {
			continue
		}
		s.warmPool(ctx, img)
	}
}

// sweepExpired releases leases whose TTL has passed. Persistent leases
// are not TTL-swept (the consumer keeps them alive via keep-alive and
// disposes via delete), but when IdleTimeout is set every persistent
// lease is auto-suspended after that long without activity
// (exec/stream/proxy/keep-alive all bump LastActive). A suspended
// sandbox keeps its state snapshot and is cheap to resume.
func (s *Service) sweepExpired(ctx context.Context) {
	s.store.mu.Lock()
	s.flushLastActiveLocked(ctx)
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
		if l.Persistent && s.cfg.IdleTimeout > 0 && !l.Suspended && now.After(l.LastActive.Add(s.cfg.IdleTimeout)) {
			s.log.Printf("idle sweep: suspending persistent lease %s (idle since %s)", l.ID, l.LastActive.Format(time.RFC3339))
			idleSuspend = append(idleSuspend, l)
		}
	}
	s.store.mu.Unlock()
	// Suspend idle leases in small, staggered batches. Each suspend is a
	// snapshot write on the node; a large backlog (e.g. after a long test
	// session) must not produce one big burst. Cap per tick and space
	// them out — with the 5s sweep tick, 13 idle leases clear in ~25s
	// instead of a single burst.
	const maxSuspendPerTick = 3
	suspended := 0
	for _, l := range idleSuspend {
		if suspended >= maxSuspendPerTick {
			break
		}
		if _, err := s.suspend(ctx, l.Owner, l.ID); err != nil {
			s.log.Printf("idle sweep: suspend %s: %v", l.ID, err)
			continue
		}
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
		now := time.Now()
		l.LastActive = now
		s.store.lastActiveDirty[id] = now
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
		ttl = s.cfg.MaxTTL
	}
	if ttl > s.cfg.MaxTTL {
		ttl = s.cfg.MaxTTL
	}
	l.ExpiresAt = time.Now().Add(ttl)
	l.LastActive = time.Now() // keep-alive is activity
	s.saveLeaseLocked(l)
	return l, nil
}

// release deletes the lease's sandbox (nil when already gone), drops the
// sandboxes row, then the lease row; shares cascade. Builds are left for
// U11's GC.
func (s *Service) release(ctx context.Context, l *Lease) {
	s.store.mu.Lock()
	if l.released {
		s.store.mu.Unlock()
		return
	}
	l.released = true
	s.store.mu.Unlock()

	if err := s.sub.Delete(ctx, l.SandboxID); err != nil {
		s.log.Printf("release: delete %s: %v", l.SandboxID, err)
	}
	s.deleteSandboxRow(l.SandboxID)
	s.store.mu.Lock()
	delete(s.store.leases, l.ID)
	delete(s.store.shares, l.ID)
	s.deleteLeaseLocked(l.ID)
	s.store.mu.Unlock()
	if len(l.ExposePorts) > 0 {
		s.refreshPeersAsync(ctx)
	}
}

// errQuotaExceeded is returned when a user hits their concurrent-lease
// cap (T4/#31). The API layer maps it to HTTP 429.
var errQuotaExceeded = fmt.Errorf("lease quota exceeded")

// reserveQuota enforces a user's concurrent-lease cap before granting
// and RESERVES n slots atomically (security review #37 H2): the count
// and the reservation happen under the same store lock, so concurrent
// creates cannot both pass max_leases. The caller MUST call
// releaseQuotaReservation when it finishes (success or failure).
// Returns errQuotaExceeded when the cap is hit. Owners without an
// identity-store user (legacy consumer tokens) are uncapped.
func (s *Service) reserveQuota(owner string, n int) error {
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
	if active+s.store.pending[owner]+n > u.MaxLeases {
		if s.metrics != nil {
			s.metrics.QuotaExceeded.Inc()
		}
		return errQuotaExceeded
	}
	s.store.pending[owner] += n
	return nil
}

// releaseQuotaReservation drops n reservations made by reserveQuota.
func (s *Service) releaseQuotaReservation(owner string, n int) {
	if s.identities == nil {
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if s.store.pending[owner] <= n {
		delete(s.store.pending, owner)
	} else {
		s.store.pending[owner] -= n
	}
}

// imageBuild loads an image row and its current build. An image without
// a row, or whose current build is unset, is unknown (→ 404).
func (s *Service) imageBuild(ctx context.Context, image string) (store.ImageRow, store.BuildRow, error) {
	img, err := s.db.GetImage(ctx, image)
	if errors.Is(err, store.ErrNotFound) {
		return store.ImageRow{}, store.BuildRow{}, errUnknownImage
	}
	if err != nil {
		return store.ImageRow{}, store.BuildRow{}, fmt.Errorf("load image %s: %w", image, err)
	}
	if img.CurrentBuildID == "" {
		return store.ImageRow{}, store.BuildRow{}, errUnknownImage
	}
	b, err := s.db.GetBuild(ctx, img.CurrentBuildID)
	if err != nil {
		return store.ImageRow{}, store.BuildRow{}, fmt.Errorf("load build %s: %w", img.CurrentBuildID, err)
	}
	return img, b, nil
}

// grant creates a new lease for owner: served from the warm pool when
// one is configured and stocked, else a cold create from the image's
// current build. Persistent leases are intended for interactive use:
// they are not TTL-swept (see keepAlive) and the consumer drives their
// lifecycle.
func (s *Service) grant(ctx context.Context, owner, image string, ttl time.Duration, persistent bool, netPolicy string, netAllow []string, exposePorts ...int) (*Lease, error) {
	img, b, err := s.imageBuild(ctx, image)
	if err != nil {
		return nil, err
	}
	if err := s.reserveQuota(owner, 1); err != nil {
		return nil, err
	}
	// The reservation becomes the real lease when it's stored below;
	// the deferred release runs on BOTH success and failure: on failure
	// it frees the slot, on success the lease is already counted as
	// active so the pending reservation must be dropped (security
	// review #37 H2). Both mutations take the same store lock, so a
	// concurrent reserveQuota sees a consistent active+pending count.
	defer func() { s.releaseQuotaReservation(owner, 1) }()
	if s.metrics != nil {
		s.metrics.LeasesTotal.Inc()
	}
	now := time.Now()
	lease := &Lease{
		ID:          newID(),
		Owner:       owner,
		Image:       image,
		CreatedAt:   now,
		ExpiresAt:   now.Add(ttl),
		Persistent:  persistent,
		LastActive:  now,
		NetPolicy:   netPolicy,
		NetAllow:    netAllow,
		ExposePorts: exposePorts,
		State:       "running",
		TemplateID:  img.TemplateID,
	}

	// Pool: pop the oldest entry for the image. The pool serves
	// persistent and non-persistent grants alike. A pooled sandbox was
	// created for the "pool" placeholder lease, so its egress policy and
	// EndAt are updated for the new lease, and its sandboxes row moves to
	// it. Entries from another build, with no row, or unhealthy are
	// discarded and the next one is tried.
	if s.cfg.PoolSize > 0 {
		for {
			s.store.mu.Lock()
			pool := s.store.pool[image]
			var id string
			if len(pool) > 0 {
				id = pool[0]
				s.store.pool[image] = pool[1:]
				s.removePoolLocked(id)
			}
			s.store.mu.Unlock()
			if id == "" {
				break
			}
			row, err := s.db.GetSandbox(ctx, id)
			if errors.Is(err, store.ErrNotFound) {
				s.log.Printf("grant: pooled %s (%s) has no sandboxes row, discarding", id, image)
				s.discardPoolSandbox(ctx, id)
				continue
			}
			if err != nil {
				s.discardPoolSandbox(ctx, id)
				return nil, fmt.Errorf("load pooled sandbox %s: %w", id, err)
			}
			if row.BuildID != img.CurrentBuildID {
				s.log.Printf("grant: pooled %s (%s) was built from %s, want %s, discarding", id, image, row.BuildID, img.CurrentBuildID)
				s.discardPoolSandbox(ctx, id)
				continue
			}
			if err := s.sub.Health(ctx, id); err != nil {
				s.log.Printf("grant: pooled %s (%s) is unhealthy, discarding: %v", id, image, err)
				s.discardPoolSandbox(ctx, id)
				continue
			}
			eg := s.egressFor(lease)
			if err := s.sub.UpdateEgress(ctx, id, eg); err != nil {
				s.discardPoolSandbox(ctx, id)
				return nil, fmt.Errorf("update egress on pooled sandbox: %w", err)
			}
			s.recordAppliedEgress(lease.ID, eg)
			if err := s.sub.UpdateEndAt(ctx, id, lease.ExpiresAt); err != nil {
				s.discardPoolSandbox(ctx, id)
				return nil, fmt.Errorf("update end at on pooled sandbox: %w", err)
			}
			row.LeaseID = lease.ID
			s.upsertSandboxRow(row)
			lease.SandboxID = id
			lease.HostIP = row.HostIP
			lease.ExposedIP = row.HostIP
			lease.BuildID = row.BuildID
			lease.pooled = true
			break
		}
	}

	if lease.SandboxID == "" {
		sb, err := s.createSandbox(ctx, img, b, false, "", lease)
		if err != nil {
			return nil, err
		}
		lease.SandboxID = sb.ID
		lease.HostIP = sb.HostIP
		lease.ExposedIP = sb.HostIP
		lease.BuildID = b.BuildID
	}

	// The integrity probe runs through exec before the sandbox is handed
	// out. On probe failure the sandbox is deleted (A1 §17 item 7's leak,
	// fixed).
	if err := s.probeSandbox(ctx, lease.SandboxID); err != nil {
		_ = s.sub.Delete(ctx, lease.SandboxID)
		s.deleteSandboxRow(lease.SandboxID)
		return nil, err
	}

	s.store.mu.Lock()
	s.store.leases[lease.ID] = lease
	s.saveLeaseLocked(lease)
	s.store.mu.Unlock()
	if len(lease.ExposePorts) > 0 {
		s.refreshPeersAsync(ctx)
	}
	return lease, nil
}

// discardPoolSandbox deletes a pooled sandbox that failed validation:
// the substrate sandbox and its rows go, and grant tries the next entry.
func (s *Service) discardPoolSandbox(ctx context.Context, id string) {
	_ = s.sub.Delete(ctx, id)
	s.deleteSandboxRow(id)
}

// suspend pauses a persistent lease's sandbox into a new build and stops
// it. The lease stays; resume restores it with the same sandbox id.
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
	s.store.mu.Unlock()

	buildID, _, err := s.sub.Pause(ctx, l.SandboxID, l.TemplateID)
	if err != nil {
		return nil, err
	}
	// The pause build records what was snapshotted: versions and sizes
	// copied from the parent build row (refs are stored from U11 on).
	parent, err := s.db.GetBuild(ctx, l.BuildID)
	if err != nil {
		return nil, fmt.Errorf("load parent build %s: %w", l.BuildID, err)
	}
	now := time.Now()
	if err := s.db.InsertBuild(ctx, store.BuildRow{
		BuildID:            buildID,
		Kind:               "pause",
		TemplateID:         l.TemplateID,
		Image:              l.Image,
		ParentBuildID:      l.BuildID,
		SourceSandboxID:    l.SandboxID,
		State:              "ready",
		KernelVersion:      parent.KernelVersion,
		FirecrackerVersion: parent.FirecrackerVersion,
		EnvdVersion:        parent.EnvdVersion,
		VCPU:               parent.VCPU,
		MemoryMB:           parent.MemoryMB,
		DiskMB:             parent.DiskMB,
		CreatedAt:          now,
		UpdatedAt:          now,
	}); err != nil {
		return nil, fmt.Errorf("insert pause build: %w", err)
	}
	s.deleteSandboxRow(l.SandboxID)
	s.store.mu.Lock()
	l.State = "suspended"
	l.Suspended = true
	l.ResumeBuildID = buildID
	s.saveLeaseLocked(l)
	s.store.mu.Unlock()
	return l, nil
}

// resume restores a suspended persistent lease: create with snapshot
// from the pause build, same sandbox id.
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
	resumeBuild := l.ResumeBuildID
	image := l.Image
	s.store.mu.Unlock()

	img, err := s.db.GetImage(ctx, image)
	if err != nil {
		return nil, fmt.Errorf("load image %s: %w", image, err)
	}
	b, err := s.db.GetBuild(ctx, resumeBuild)
	if err != nil {
		return nil, fmt.Errorf("load build %s: %w", resumeBuild, err)
	}
	sb, err := s.createSandbox(ctx, img, b, true, l.SandboxID, l)
	if err != nil {
		return nil, err
	}
	s.store.mu.Lock()
	l.HostIP = sb.HostIP
	l.ExposedIP = sb.HostIP
	l.BuildID = resumeBuild
	l.State = "running"
	l.Suspended = false
	l.LastActive = time.Now()
	s.saveLeaseLocked(l)
	s.store.mu.Unlock()
	if len(l.ExposePorts) > 0 {
		s.refreshPeersAsync(ctx)
	}
	return l, nil
}

// restart reboots a lease. Persistent and running: suspend, then resume
// (lossless through the pause build). Persistent and suspended: resume.
// Non-persistent: delete the sandbox and create a fresh one from the
// image's current build, keeping the lease id (A1 §17 item 4).
func (s *Service) restart(ctx context.Context, owner, id string) (*Lease, error) {
	s.store.mu.Lock()
	l := s.store.leases[id]
	if l == nil || l.Owner != owner || l.released {
		s.store.mu.Unlock()
		return nil, errNotFound
	}
	persistent := l.Persistent
	suspended := l.Suspended
	s.store.mu.Unlock()

	if persistent {
		if !suspended {
			if _, err := s.suspend(ctx, owner, id); err != nil {
				return nil, err
			}
		}
		return s.resume(ctx, owner, id)
	}

	_ = s.sub.Delete(ctx, l.SandboxID)
	s.deleteSandboxRow(l.SandboxID)
	img, b, err := s.imageBuild(ctx, l.Image)
	if err != nil {
		return nil, err
	}
	sb, err := s.createSandbox(ctx, img, b, false, "", l)
	if err != nil {
		return nil, err
	}
	s.store.mu.Lock()
	l.SandboxID = sb.ID
	l.HostIP = sb.HostIP
	l.ExposedIP = sb.HostIP
	l.BuildID = b.BuildID
	l.State = "running"
	l.Suspended = false
	l.LastActive = time.Now()
	s.saveLeaseLocked(l)
	s.store.mu.Unlock()
	if len(l.ExposePorts) > 0 {
		s.refreshPeersAsync(ctx)
	}
	return l, nil
}

// checkpointLease checkpoints a running sandbox into a new build,
// inserts the checkpoint build row (versions and sizes copied from the
// parent build row; refs are stored from U11 on), and applies the
// checkpoint bookkeeping to the source lease (item 18). It returns the
// new build row.
func (s *Service) checkpointLease(ctx context.Context, src *Lease) (store.BuildRow, error) {
	buildID, _, err := s.sub.Checkpoint(ctx, src.SandboxID)
	if err != nil {
		return store.BuildRow{}, err
	}
	parent, err := s.db.GetBuild(ctx, src.BuildID)
	if err != nil {
		return store.BuildRow{}, fmt.Errorf("load parent build %s: %w", src.BuildID, err)
	}
	now := time.Now()
	b := store.BuildRow{
		BuildID:            buildID,
		Kind:               "checkpoint",
		TemplateID:         src.TemplateID,
		Image:              src.Image,
		ParentBuildID:      src.BuildID,
		SourceSandboxID:    src.SandboxID,
		State:              "ready",
		KernelVersion:      parent.KernelVersion,
		FirecrackerVersion: parent.FirecrackerVersion,
		EnvdVersion:        parent.EnvdVersion,
		VCPU:               parent.VCPU,
		MemoryMB:           parent.MemoryMB,
		DiskMB:             parent.DiskMB,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if err := s.db.InsertBuild(ctx, b); err != nil {
		return store.BuildRow{}, fmt.Errorf("insert checkpoint build: %w", err)
	}
	// The source keeps running from the new build (A2 §3.5, §3.6, the
	// "resume-fresh" path), so its build id and — possibly changed — host
	// IP are re-recorded (item 18).
	s.afterCheckpoint(ctx, src, buildID)
	return b, nil
}

// afterCheckpoint records a checkpoint on the source lease: it now runs
// from the checkpoint build; its host IP is re-read from the substrate
// because the resume-fresh path may move it. Call without s.store.mu.
func (s *Service) afterCheckpoint(ctx context.Context, src *Lease, buildID string) {
	sbs, err := s.sub.List(ctx)
	if err != nil {
		s.log.Printf("checkpoint: list sandboxes: %v", err)
	}
	s.store.mu.Lock()
	src.BuildID = buildID
	src.LastCheckpointBuildID = buildID
	src.LastCheckpointAt = time.Now()
	for _, sb := range sbs {
		if sb.ID == src.SandboxID {
			src.HostIP = sb.HostIP
			src.ExposedIP = sb.HostIP
			s.upsertSandboxRow(store.SandboxRow{
				SandboxID:   sb.ID,
				LeaseID:     src.ID,
				BuildID:     sb.BuildID,
				ExecutionID: sb.ExecutionID,
				HostIP:      sb.HostIP,
				VCPU:        int(sb.VCPU),
				MemoryMB:    int(sb.MemoryMB),
				StartedAt:   sb.StartedAt,
				EndAt:       sb.EndAt,
			})
		}
	}
	s.saveLeaseLocked(src)
	s.store.mu.Unlock()
	if len(src.ExposePorts) > 0 {
		s.refreshPeersAsync(ctx)
	}
}

// clone checkpoints a running sandbox into a new build and grants a new
// persistent lease on it. NetPolicy, NetAllow and ExposePorts are copied
// from the source; the image is the source's image name (the TemplateID
// is looked up by image name); the clone expires after maxTTL. The
// request's optional tag is accepted and ignored (A1 §17 item 5).
func (s *Service) clone(ctx context.Context, owner, srcID string) (*Lease, string, error) {
	s.store.mu.Lock()
	src := s.store.leases[srcID]
	if src == nil || src.Owner != owner || src.released {
		s.store.mu.Unlock()
		return nil, "", errNotFound
	}
	s.store.mu.Unlock()

	if err := s.reserveQuota(owner, 1); err != nil {
		return nil, "", err
	}
	defer func() { s.releaseQuotaReservation(owner, 1) }()

	img, _, err := s.imageBuild(ctx, src.Image)
	if err != nil {
		return nil, "", err
	}
	b, err := s.checkpointLease(ctx, src)
	if err != nil {
		return nil, "", err
	}
	now := time.Now()
	lease := &Lease{
		ID:          newID(),
		Owner:       owner,
		Image:       src.Image,
		CreatedAt:   now,
		ExpiresAt:   now.Add(s.cfg.MaxTTL),
		Persistent:  true,
		LastActive:  now,
		NetPolicy:   src.NetPolicy,
		NetAllow:    append([]string(nil), src.NetAllow...),
		ExposePorts: append([]int(nil), src.ExposePorts...),
		State:       "running",
		TemplateID:  img.TemplateID,
	}
	sb, err := s.createSandbox(ctx, img, b, false, "", lease)
	if err != nil {
		return nil, "", err
	}
	lease.SandboxID = sb.ID
	lease.HostIP = sb.HostIP
	lease.ExposedIP = sb.HostIP
	lease.BuildID = b.BuildID
	s.store.mu.Lock()
	s.store.leases[lease.ID] = lease
	s.saveLeaseLocked(lease)
	s.store.mu.Unlock()
	if len(lease.ExposePorts) > 0 {
		s.refreshPeersAsync(ctx)
	}
	return lease, b.BuildID, nil
}

// fork checkpoints a running sandbox once and creates count sandboxes
// from the checkpoint build (1..20). Quota for all count leases is
// reserved up front, all or nothing; if any create fails, every sandbox
// created in this call is deleted, the reservations are released, and
// the error is returned. The source keeps running (item 18 bookkeeping).
func (s *Service) fork(ctx context.Context, owner, srcID string, count int, persistent bool, ttl time.Duration) ([]*Lease, string, error) {
	s.store.mu.Lock()
	src := s.store.leases[srcID]
	if src == nil || src.Owner != owner || src.released {
		s.store.mu.Unlock()
		return nil, "", errNotFound
	}
	if src.Suspended {
		s.store.mu.Unlock()
		return nil, "", errSuspended
	}
	s.store.mu.Unlock()

	if count < 1 || count > 20 {
		return nil, "", errBadForkCount
	}
	if ttl <= 0 {
		ttl = s.cfg.DefaultTTL
	}
	if ttl > s.cfg.MaxTTL {
		ttl = s.cfg.MaxTTL
	}
	if s.identities != nil {
		if u := s.identities.UserByID(owner); u != nil && u.MaxTTL > 0 {
			if userMax := time.Duration(u.MaxTTL) * time.Second; ttl > userMax {
				ttl = userMax
			}
		}
	}

	if err := s.reserveQuota(owner, count); err != nil {
		return nil, "", err
	}

	img, _, err := s.imageBuild(ctx, src.Image)
	if err != nil {
		s.releaseQuotaReservation(owner, count)
		return nil, "", err
	}
	b, err := s.checkpointLease(ctx, src)
	if err != nil {
		s.releaseQuotaReservation(owner, count)
		return nil, "", err
	}

	var created []*Lease
	rollback := func(err error) ([]*Lease, string, error) {
		for _, l := range created {
			_ = s.sub.Delete(ctx, l.SandboxID)
			s.deleteSandboxRow(l.SandboxID)
			s.store.mu.Lock()
			delete(s.store.leases, l.ID)
			s.deleteLeaseLocked(l.ID)
			s.store.mu.Unlock()
		}
		s.releaseQuotaReservation(owner, count)
		return nil, "", err
	}

	now := time.Now()
	for range count {
		lease := &Lease{
			ID:          newID(),
			Owner:       owner, // quota is charged to the caller
			Image:       src.Image,
			CreatedAt:   now,
			ExpiresAt:   now.Add(ttl),
			Persistent:  persistent,
			LastActive:  now,
			NetPolicy:   src.NetPolicy,
			NetAllow:    append([]string(nil), src.NetAllow...),
			ExposePorts: append([]int(nil), src.ExposePorts...),
			State:       "running",
			TemplateID:  img.TemplateID,
		}
		sb, err := s.createSandbox(ctx, img, b, false, "", lease)
		if err != nil {
			return rollback(err)
		}
		lease.SandboxID = sb.ID
		lease.HostIP = sb.HostIP
		lease.ExposedIP = sb.HostIP
		lease.BuildID = b.BuildID
		s.store.mu.Lock()
		s.store.leases[lease.ID] = lease
		s.saveLeaseLocked(lease)
		s.store.mu.Unlock()
		created = append(created, lease)
	}
	s.releaseQuotaReservation(owner, count)
	if len(src.ExposePorts) > 0 {
		s.refreshPeersAsync(ctx)
	}
	return created, b.BuildID, nil
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
	s.saveLeaseLocked(l)
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
	s.saveLeaseLocked(l)
	return l, nil
}

// setNetwork updates a lease's egress policy and allowlist (U09): the
// lease is saved, the egress config is re-applied to its sandbox, and
// every lease's peer allowances are refreshed asynchronously.
func (s *Service) setNetwork(ctx context.Context, owner, id, policy string, allow []string) (*Lease, error) {
	s.store.mu.Lock()
	l := s.store.leases[id]
	if l == nil || l.Owner != owner || l.released {
		s.store.mu.Unlock()
		return nil, errNotFound
	}
	if l.Suspended {
		s.store.mu.Unlock()
		return nil, errSuspended
	}
	l.NetPolicy = policy
	l.NetAllow = allow
	s.saveLeaseLocked(l)
	s.store.mu.Unlock()

	eg := s.egressFor(l)
	if err := s.sub.UpdateEgress(ctx, l.SandboxID, eg); err != nil {
		return nil, fmt.Errorf("update egress: %w", err)
	}
	s.recordAppliedEgress(l.ID, eg)
	s.refreshPeersAsync(ctx)
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
	s.saveShareLocked(sh)
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
	s.deleteShareLocked(leaseID, grantee)
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

// leaseMap renders a lease as one GET /api/sandboxes row.
func leaseMap(l *Lease) map[string]any {
	return map[string]any{
		"id":               l.ID,
		"owner":            l.Owner,
		"image":            l.Image,
		"address":          l.HostIP,
		"expires":          l.ExpiresAt.Unix(),
		"persistent":       l.Persistent,
		"suspended":        l.Suspended,
		"name":             l.Name,
		"comment":          l.Comment,
		"net_policy":       l.NetPolicy,
		"egress_allowlist": l.NetAllow,
		"exposed":          exposedMap(l),
	}
}

// leaseDetailMap is a list row plus the lifecycle fields served by
// GET /api/sandboxes/{id}.
func leaseDetailMap(l *Lease) map[string]any {
	m := leaseMap(l)
	m["state"] = l.State
	m["recovered_from"] = formatRFC3339(l.RecoveredFrom)
	m["last_checkpoint_at"] = formatRFC3339(l.LastCheckpointAt)
	return m
}

// formatRFC3339 renders t in UTC, "" for the zero time.
func formatRFC3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// list returns the caller's live leases as plain maps.
func (s *Service) list(owner string) []map[string]any {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	var out []map[string]any
	for _, l := range s.store.leases {
		if l.Owner == owner && !l.released {
			out = append(out, leaseMap(l))
		}
	}
	return out
}

// integrityProbe is run inside a sandbox before it is pooled or leased. It
// checks what the toolchain files DO, not whether they execute: a corrupted
// /usr/bin/uname can be a valid ELF of plausible size that exits 0 while
// printing some other program's name, so an exit-status check passes it. A
// name-based check is no better — it has to guess at non-GNU tools (busybox
// uname announces itself as BusyBox). Behaviour is the thing that stays true
// across images.
//
// The carriers here are the ones observed in practice: a swapped or
// unexecutable uname (which breaks every `OS=$(uname -s)` build hook) and a
// swapped tr. Extend the script if a new carrier shows up; a probe that does
// not answer at all counts as a failure, since the guest is the only vantage
// point from which this corruption is visible.
const integrityProbe = `p=""
if command -v uname >/dev/null 2>&1; then
  v=$(uname -s 2>&1)
  case "$v" in Linux|linux) ;; *) p="uname -s -> $v" ;; esac
fi
if [ -z "$p" ] && command -v tr >/dev/null 2>&1; then
  v=$(echo a-b | tr - _ 2>&1)
  case "$v" in a_b) ;; *) p="tr a-b - _ -> $v" ;; esac
fi
if [ -z "$p" ]; then echo PROBE_OK; else echo "PROBE_FAIL $p"; exit 1; fi
`

// probeSandbox runs integrityProbe inside a sandbox. nil means usable. An
// unreachable guest agent is a failure rather than a pass: a probe that
// cannot run has told us nothing about the sandbox.
func (s *Service) probeSandbox(ctx context.Context, id string) error {
	if !s.probeEnabled {
		return nil
	}
	res, err := s.sub.Exec(ctx, id, substrate.ExecRequest{Args: []string{"sh", "-c", integrityProbe}, Timeout: s.probeTimeout})
	if err != nil {
		return fmt.Errorf("probe: %w", err)
	}
	out := strings.TrimSpace(res.Stdout)
	if res.ExitCode != 0 || out != "PROBE_OK" {
		if out == "" {
			out = strings.TrimSpace(res.Stderr)
		}
		return fmt.Errorf("integrity probe failed (exit %d): %s", res.ExitCode, out)
	}
	return nil
}

// warmPool pre-creates cfg.PoolSize sandboxes of img under the placeholder
// lease {ID:"pool", Owner:"pool", ExpiresAt: now+24h, NetPolicy:"none"}.
// Their sandboxes rows carry lease_id "".
func (s *Service) warmPool(ctx context.Context, img store.ImageRow) {
	if s.cfg.PoolSize <= 0 {
		return
	}
	s.store.mu.Lock()
	cur := len(s.store.pool[img.Name])
	s.store.mu.Unlock()
	if cur >= s.cfg.PoolSize {
		return
	}
	b, err := s.db.GetBuild(ctx, img.CurrentBuildID)
	if err != nil {
		s.log.Printf("warmPool: %s: load build %s: %v", img.Name, img.CurrentBuildID, err)
		return
	}
	placeholder := &Lease{
		ID:         "pool",
		Owner:      "pool",
		ExpiresAt:  time.Now().Add(24 * time.Hour),
		NetPolicy:  string(PolicyNone),
		State:      "running",
		TemplateID: img.TemplateID,
	}
	// Create one at a time and verify before stocking, so a bad build is
	// recycled here rather than served to a job. Stop rather than loop: a
	// build that fails the probe will keep failing it, and retrying
	// creates a sandbox per attempt.
	for i := cur; i < s.cfg.PoolSize; i++ {
		sb, err := s.createSandbox(ctx, img, b, false, "", placeholder)
		if err != nil {
			s.log.Printf("warmPool: create %s: %v", img.Name, err)
			return
		}
		if err := s.probeSandbox(ctx, sb.ID); err != nil {
			s.log.Printf("warmPool: %s sandbox %s failed the integrity probe, recycling: %v", img.Name, sb.ID, err)
			_ = s.sub.Delete(ctx, sb.ID)
			s.deleteSandboxRow(sb.ID)
			return
		}
		s.store.mu.Lock()
		s.store.pool[img.Name] = append(s.store.pool[img.Name], sb.ID)
		s.addPoolLocked(sb.ID, img.Name)
		s.store.mu.Unlock()
	}
}

// leaseToRow maps an in-memory lease to its store row. An empty State
// is derived from Suspended so the row always carries a valid state.
// HostIP lives in the address column; TemplateID is looked up from
// images and BuildID from sandboxes (neither is a lease column).
func leaseToRow(l *Lease) store.LeaseRow {
	state := l.State
	if state == "" {
		if l.Suspended {
			state = "suspended"
		} else {
			state = "running"
		}
	}
	return store.LeaseRow{
		ID:                    l.ID,
		Owner:                 l.Owner,
		Image:                 l.Image,
		SandboxID:             l.SandboxID,
		Address:               l.HostIP,
		CreatedAt:             l.CreatedAt,
		ExpiresAt:             l.ExpiresAt,
		Persistent:            l.Persistent,
		LastActive:            l.LastActive,
		Suspended:             l.Suspended,
		Name:                  l.Name,
		NetPolicy:             l.NetPolicy,
		NetAllow:              l.NetAllow,
		ExposePorts:           l.ExposePorts,
		ExposedIP:             l.ExposedIP,
		Comment:               l.Comment,
		State:                 state,
		ResumeBuildID:         l.ResumeBuildID,
		LastCheckpointBuildID: l.LastCheckpointBuildID,
		LastCheckpointAt:      l.LastCheckpointAt,
		RecoveredFrom:         l.RecoveredFrom,
	}
}

// rowToLease maps a store row back to an in-memory lease. The Suspended
// flag follows the persisted state.
func rowToLease(r store.LeaseRow) *Lease {
	return &Lease{
		ID:                    r.ID,
		Owner:                 r.Owner,
		Image:                 r.Image,
		SandboxID:             r.SandboxID,
		HostIP:                r.Address,
		CreatedAt:             r.CreatedAt,
		ExpiresAt:             r.ExpiresAt,
		Persistent:            r.Persistent,
		LastActive:            r.LastActive,
		Suspended:             r.State == "suspended",
		Name:                  r.Name,
		NetPolicy:             r.NetPolicy,
		NetAllow:              r.NetAllow,
		ExposePorts:           r.ExposePorts,
		ExposedIP:             r.ExposedIP,
		Comment:               r.Comment,
		State:                 r.State,
		ResumeBuildID:         r.ResumeBuildID,
		LastCheckpointBuildID: r.LastCheckpointBuildID,
		LastCheckpointAt:      r.LastCheckpointAt,
		RecoveredFrom:         r.RecoveredFrom,
	}
}

// storeWriteTimeout bounds a single store write.
const storeWriteTimeout = 5 * time.Second

// storeError records a failed store write: a log line plus the
// spoond_store_errors_total counter. Store errors never fail the
// request — availability wins over durability for a single write.
func (s *Service) storeError(op, id string, err error) {
	s.log.Printf("store: %s %s: %v", op, id, err)
	if s.metrics != nil {
		s.metrics.StoreErrors.WithLabelValues(op).Inc()
	}
}

// The store helpers below are called with s.store.mu held (the sandbox
// row helpers excepted: they run outside lease mutations) and write
// through with a bounded timeout.

func (s *Service) saveLeaseLocked(l *Lease) {
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	if err := s.db.UpsertLease(ctx, leaseToRow(l)); err != nil {
		s.storeError("upsert_lease", l.ID, err)
	}
}

func (s *Service) deleteLeaseLocked(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	if err := s.db.DeleteLease(ctx, id); err != nil {
		s.storeError("delete_lease", id, err)
	}
}

func (s *Service) saveShareLocked(sh *Share) {
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	if err := s.db.UpsertShare(ctx, store.ShareRow{
		LeaseID:   sh.LeaseID,
		Grantee:   sh.Grantee,
		Mode:      string(sh.Mode),
		ExpiresAt: sh.ExpiresAt,
		CreatedAt: sh.CreatedAt,
	}); err != nil {
		s.storeError("upsert_share", sh.LeaseID+"/"+sh.Grantee, err)
	}
}

func (s *Service) deleteShareLocked(leaseID, grantee string) {
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	if err := s.db.DeleteShare(ctx, leaseID, grantee); err != nil {
		s.storeError("delete_share", leaseID+"/"+grantee, err)
	}
}

func (s *Service) addPoolLocked(id, image string) {
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	if err := s.db.AddPool(ctx, id, image); err != nil {
		s.storeError("add_pool", id, err)
	}
}

func (s *Service) removePoolLocked(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	if err := s.db.RemovePool(ctx, id); err != nil {
		s.storeError("remove_pool", id, err)
	}
}

// upsertSandboxRow records a created/resumed/checkpointed sandbox.
func (s *Service) upsertSandboxRow(row store.SandboxRow) {
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	if err := s.db.UpsertSandbox(ctx, row); err != nil {
		s.storeError("upsert_sandbox", row.SandboxID, err)
	}
}

// deleteSandboxRow drops a sandboxes entry.
func (s *Service) deleteSandboxRow(sandboxID string) {
	ctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	if err := s.db.DeleteSandbox(ctx, sandboxID); err != nil {
		s.storeError("delete_sandbox", sandboxID, err)
	}
}

// flushLastActiveLocked writes the batched touch() updates in one
// transaction, then clears the dirty set. A failed flush keeps the set
// so the next tick retries. Called with s.store.mu held.
func (s *Service) flushLastActiveLocked(ctx context.Context) {
	if len(s.store.lastActiveDirty) == 0 {
		return
	}
	dirty := s.store.lastActiveDirty
	ctx, cancel := context.WithTimeout(ctx, storeWriteTimeout)
	defer cancel()
	if err := s.db.UpdateLastActive(ctx, dirty); err != nil {
		ids := make([]string, 0, len(dirty))
		for id := range dirty {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		s.storeError("update_last_active", strings.Join(ids, ","), err)
		return
	}
	s.store.lastActiveDirty = make(map[string]time.Time)
}

// LoadState reads every lease, share and pool entry from the store into
// memory. Each lease's TemplateID comes from its image row; its BuildID
// comes from the sandboxes row carrying its id (a suspended lease has
// none, and its BuildID stays ""). It must run before Start and before
// ReconcileOrphans.
func (s *Service) LoadState(ctx context.Context) error {
	leases, err := s.db.ListLeases(ctx)
	if err != nil {
		return fmt.Errorf("load leases: %w", err)
	}
	shareRows, err := s.db.ListShares(ctx)
	if err != nil {
		return fmt.Errorf("load shares: %w", err)
	}
	pool, err := s.db.ListPool(ctx)
	if err != nil {
		return fmt.Errorf("load pool: %w", err)
	}
	loaded := make(map[string]*Lease, len(leases))
	for _, r := range leases {
		l := rowToLease(r)
		if img, err := s.db.GetImage(ctx, l.Image); err == nil {
			l.TemplateID = img.TemplateID
		}
		if row, err := s.db.GetSandboxByLease(ctx, l.ID); err == nil {
			l.BuildID = row.BuildID
		}
		loaded[l.ID] = l
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	s.store.leases = loaded
	for _, r := range shareRows {
		if s.store.shares[r.LeaseID] == nil {
			s.store.shares[r.LeaseID] = make(map[string]*Share)
		}
		s.store.shares[r.LeaseID][r.Grantee] = &Share{
			LeaseID:   r.LeaseID,
			Grantee:   r.Grantee,
			Mode:      ShareMode(r.Mode),
			ExpiresAt: r.ExpiresAt,
			CreatedAt: r.CreatedAt,
		}
	}
	for img, ids := range pool {
		s.store.pool[img] = ids
	}
	return nil
}

// LiveLeases returns the ids of active (unreleased) leases. Used by
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

// Shutdown stops the background loops and flushes the batched
// LastActive updates to the store. It does NOT release leases or delete
// pooled sandboxes: state persists in the SQLite store and the next
// incarnation loads it via LoadState (U05).
func (s *Service) Shutdown(ctx context.Context) {
	if s.stopLoops != nil {
		s.stopLoops()
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	s.flushLastActiveLocked(ctx)
}

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
	// Pool cap = poolSize × number of stocked images.
	if s.cfg.PoolSize > 0 {
		m.PoolCap.Set(float64(s.cfg.PoolSize * len(s.store.pool)))
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

// ReconcileOrphans aligns the substrate with the state loaded from the
// store. If the sandbox list fails, nothing changes (leases are never
// marked lost on a list failure). Substrate sandboxes that no lease
// (by sandbox id) or pool entry claims are deleted. Live leases whose
// sandbox is missing are marked lost (U10 upgrades this to recovery);
// suspended leases have no live sandbox by design. Pool entries whose
// sandbox is missing are removed.
func (s *Service) ReconcileOrphans(ctx context.Context) {
	sbs, err := s.sub.List(ctx)
	if err != nil {
		s.log.Printf("reconcile: list sandboxes failed: %v", err)
		return
	}
	present := make(map[string]bool, len(sbs))
	for _, sb := range sbs {
		present[sb.ID] = true
	}
	s.store.mu.Lock()
	mine := make(map[string]bool)
	for _, l := range s.store.leases {
		if l.SandboxID != "" {
			mine[l.SandboxID] = true
		}
	}
	for _, ids := range s.store.pool {
		for _, id := range ids {
			mine[id] = true
		}
	}
	var orphans []string
	for _, sb := range sbs {
		if !mine[sb.ID] {
			orphans = append(orphans, sb.ID)
		}
	}
	for _, l := range s.store.leases {
		if !l.live() || present[l.SandboxID] {
			continue
		}
		l.State = "lost"
		s.saveLeaseLocked(l)
	}
	for img, ids := range s.store.pool {
		kept := ids[:0]
		for _, id := range ids {
			if present[id] {
				kept = append(kept, id)
			} else {
				s.removePoolLocked(id)
			}
		}
		s.store.pool[img] = kept
	}
	s.store.mu.Unlock()

	deleted := 0
	for _, id := range orphans {
		if err := s.sub.Delete(ctx, id); err != nil {
			s.log.Printf("reconcile: delete orphan %s failed: %v", id, err)
			continue
		}
		deleted++
		if s.metrics != nil {
			s.metrics.LeaseOrphaned.Inc()
		}
	}
	if deleted > 0 {
		s.log.Printf("reconcile: deleted %d orphaned sandbox(es) from a previous incarnation", deleted)
	}
}

var (
	errNotFound      = &leaseError{"sandbox not found"}
	errNotPersistent = &leaseError{"sandbox is not a persistent lease"}
	errUnknownImage  = &leaseError{"unknown image"}
	errSuspended     = &leaseError{"sandbox is suspended"}
	errBadForkCount  = &leaseError{"count must be 1..20"}
)

// leaseError is a simple sentinel error.
type leaseError struct{ msg string }

func (e *leaseError) Error() string { return e.msg }
