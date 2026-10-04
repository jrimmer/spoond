package api

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/common/expfmt"

	"github.com/jrimmer/spoond/v2/identity"
	"github.com/jrimmer/spoond/v2/metrics"
	"github.com/jrimmer/spoond/v2/substrate"
)

// Server is the HTTP lease API.
type Server struct {
	svc       *Service
	reg       *ImageRegistry
	mux       *http.ServeMux
	llm       *llmGateway
	heartbeat *leaseHeartbeat
	assetsDir string // static assets dir served at /assets/ on the proxy listener

	// Proxy authentication (U7/T7): mode "" or "off" = open capability
	// model (today's behavior); "forward-auth" = the proxy listener
	// requires X-Proxy-Auth == secret and resolves the authenticated
	// user from Remote-User before owner-scoping lookups.
	proxyAuthMode   string
	proxyAuthSecret string
	// proxyTrustedPeers (security review #37 M3): when non-empty, the
	// forward-auth gate only honors Remote-User from these peer
	// addresses/CIDRs — defense in depth so a direct client to :8891
	// can't spoof a username even with the shared secret. Set via
	// PROXY_AUTH_TRUSTED_PEERS (comma-separated).
	proxyTrustedPeers []*net.IPNet
	// bootstrapToken (security review #37 H3/M4) gates the first-user
	// bootstrap when configured: the store-empty creation must present
	// X-Bootstrap-Token matching it. Set via BOOTSTRAP_TOKEN env.
	bootstrapToken string
	// adminToken (U10) gates POST /api/admin/{drain,undrain,reconcile}
	// with Authorization: Bearer <ADMIN_TOKEN>. It is not a user or
	// consumer token; empty (the default) disables the admin routes.
	adminToken string
	// metrics (issue #20): service-owned Prometheus metrics served at
	// /metrics alongside the orchestrator's collector output.
	metrics *metrics.BackendMetrics
	// authFails (security review #37 L5) throttles repeated failed
	// token auths per client IP.
	authFails *authFailLimiter
	// busy (security review #37 rescan F9) caps concurrent exec/stream
	// operations per owner so a tenant can't saturate the controller
	// with in-flight activity (quota covers lease count, not activity).
	busyMu    sync.Mutex
	busyCount map[string]int
	busyMax   int
}

// acquireBusy reserves an exec/stream slot for owner. Returns false when
// the per-owner cap is already reached (caller should 429).
func (s *Server) acquireBusy(owner string) bool {
	s.busyMu.Lock()
	defer s.busyMu.Unlock()
	if s.busyCount[owner] >= s.busyMax {
		return false
	}
	s.busyCount[owner]++
	return true
}

// releaseBusy frees an exec/stream slot acquired by acquireBusy.
func (s *Server) releaseBusy(owner string) {
	s.busyMu.Lock()
	defer s.busyMu.Unlock()
	if s.busyCount[owner] <= 1 {
		delete(s.busyCount, owner)
	} else {
		s.busyCount[owner]--
	}
}

// SetBootstrapToken configures the first-user bootstrap gate
// (security review #37 H3/M4). Empty = bootstrap open to any
// authenticated caller (legacy behavior).
func (s *Server) SetBootstrapToken(tok string) {
	s.bootstrapToken = tok
}

// SetAdminToken configures the /api/admin/* bearer token (U10). Empty
// (the default) disables the admin routes; a configured token is
// required on every admin request and compared in constant time.
func (s *Server) SetAdminToken(tok string) {
	s.adminToken = tok
}

// SetProxyAuth configures the public proxy listener's auth gate
// (U7/T7). mode "off" (or "") keeps the capability model; mode
// "forward-auth" requires the shared secret + Remote-User identity.
// trustedPeers is a comma-separated list of IPs/CIDRs that may present
// Remote-User (security review #37 M3); empty = any peer with the
// secret.
func (s *Server) SetProxyAuth(mode, secret, trustedPeers string) {
	s.proxyAuthMode = mode
	s.proxyAuthSecret = secret
	s.proxyTrustedPeers = nil
	for _, cidr := range strings.Split(trustedPeers, ",") {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		if ip := net.ParseIP(cidr); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			s.proxyTrustedPeers = append(s.proxyTrustedPeers, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		if _, n, err := net.ParseCIDR(cidr); err == nil {
			s.proxyTrustedPeers = append(s.proxyTrustedPeers, n)
		}
	}
}

// NewServer wires the lease API routes onto a mux. openRouterURL and
// openRouterKey are the LLM gateway upstream (empty = gateway disabled);
// the key is held process-side and never exposed to sandboxes.
func NewServer(svc *Service, reg *ImageRegistry) *Server {
	return NewServerWithLLM(svc, reg, "", "", "", nil)
}

// NewServerWithLLM wires the lease API routes plus an optional per-lease
// LLM gateway. openRouterURL is the OpenAI-compatible API base; the key
// stays in this process. defaultModel is the upstream fallback model
// (applied when a requested id isn't in modelMap); modelMap translates
// exe.dev catalog model ids to upstream ids.
func NewServerWithLLM(svc *Service, reg *ImageRegistry, openRouterURL, openRouterKey, defaultModel string, modelMap map[string]string) *Server {
	s := &Server{svc: svc, reg: reg, mux: http.NewServeMux(), authFails: newAuthFailLimiter(),
		busyCount: map[string]int{}, busyMax: 8, metrics: metrics.NewBackendMetrics()}
	// The guest heartbeat lives only on the guest-service listener
	// (ProxyHandler); the lease id in the path is the capability.
	s.heartbeat = newLeaseHeartbeat(svc)
	s.heartbeat.metrics = s.metrics
	if openRouterURL != "" {
		// svc.identities must be installed (SetIdentities) before
		// NewServerWithLLM for per-user LLM key enforcement (U8/T8).
		s.llm = newLLMGateway(svc.log, svc.lookupAny, svc.identities, openRouterURL, openRouterKey, defaultModel, modelMap)
		s.llm.metrics = s.metrics
	}
	s.svc.SetMetrics(s.metrics)
	s.mux.HandleFunc("POST /api/sandboxes", s.handleCreate)
	s.mux.HandleFunc("GET /api/sandboxes", s.handleList)
	s.mux.HandleFunc("GET /api/sandboxes/{id}", s.handleGetSandbox)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/exec", s.handleExec)
	s.mux.HandleFunc("DELETE /api/sandboxes/{id}", s.handleDelete)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/keepalive", s.handleKeepAlive)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/suspend", s.handleSuspend)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/resume", s.handleResume)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/restart", s.handleRestart)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/checkpoint", s.handleCheckpoint)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/tag", s.handleTag)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/comment", s.handleComment)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/prompt", s.handlePrompt)
	s.mux.HandleFunc("GET /api/sandboxes/{id}/endpoint", s.handleEndpoint)
	s.mux.HandleFunc("GET /api/sandboxes/{id}/stat", s.handleStat)
	s.mux.HandleFunc("GET /api/sandboxes/{id}/stream", s.handleStream)
	// Live egress policy change (U09).
	s.mux.HandleFunc("POST /api/sandboxes/{id}/network", s.handleNetwork)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/clone", s.handleClone)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/fork", s.handleFork)
	// Sharing (T6/#33).
	s.mux.HandleFunc("POST /api/sandboxes/{id}/share", s.handleShareGrant)
	s.mux.HandleFunc("DELETE /api/sandboxes/{id}/share/{grantee}", s.handleShareRevoke)
	s.mux.HandleFunc("GET /api/shares", s.handleShareList)
	s.mux.HandleFunc("GET /api/images", s.handleImages)
	s.mux.HandleFunc("GET /api/names/{name}", s.handleByName)
	// Snapshot catalog (U11): list and delete the caller's builds.
	s.mux.HandleFunc("GET /api/snapshots", s.handleSnapshots)
	s.mux.HandleFunc("DELETE /api/snapshots/{build_id}", s.handleSnapshotDelete)
	// Admin endpoints (U10): drain, undrain and crash reconcile. Auth is
	// done in api/admin.go (ADMIN_TOKEN is not a consumer token, so
	// authMiddleware lets /api/admin/ through).
	s.mux.HandleFunc("POST /api/admin/drain", s.handleAdminDrain)
	s.mux.HandleFunc("POST /api/admin/undrain", s.handleAdminUndrain)
	s.mux.HandleFunc("POST /api/admin/reconcile", s.handleAdminReconcile)
	// Lease event streams (2.2, #115): Server-Sent Events of every lease
	// lifecycle change, the caller's leases (admins see all) or one
	// lease. The /api/leases alias covers both via rewriteLeasePath.
	s.mux.HandleFunc("GET /api/sandboxes/events", s.handleLeaseEvents)
	s.mux.HandleFunc("GET /api/sandboxes/{id}/events", s.handleLeaseEventsOne)
	// Held leases (2.1): set or clear what holds a lease later. Owner or
	// admin; the handler 404s for anyone else, like the other lease
	// routes.
	s.mux.HandleFunc("PUT /api/sandboxes/{id}/holder", s.handleHolder)
	// Owner-blind resume for held leases (2.1): the SSH gateway resumes
	// a rule-1-suspended held lease on attach, where the capability is
	// the lease id/name and no owner id is known.
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	// Identity endpoints (epic #26 T1): user management + key resolution.
	if s.svc.identities != nil {
		s.mux.HandleFunc("GET /api/users", s.handleUsersList)
		s.mux.HandleFunc("GET /api/users/me", s.handleUsersMe)
		s.mux.HandleFunc("GET /api/users/by-name/{name}", s.handleUsersByName)
		s.mux.HandleFunc("GET /api/users/by-key", s.handleUsersByKey)
		s.mux.HandleFunc("GET /api/identity-status", s.handleIdentityStatus)
		s.mux.HandleFunc("POST /api/users", s.handleUsersCreate)
		s.mux.HandleFunc("POST /api/users/{id}/quota", s.handleUsersQuota)
		s.mux.HandleFunc("POST /api/users/{id}/llm-key", s.handleUsersLLMKey) // U8/T8
		s.mux.HandleFunc("DELETE /api/users/{id}", s.handleUsersDelete)
	}
	if s.llm != nil {
		// The LLM gateway is auth-exempt (lease id in path is the
		// capability); it MUST be mounted on the outer handler after
		// authMiddleware. Handler() does that via authExempt prefix.
		s.mux.Handle(llmGatewayPrefix, s.llm)
	}
	return s
}

// SetHeartbeatMetrics installs the Prometheus collector on the heartbeat
// handler. NewServerWithLLM already wires it; tests that build the
// handler standalone call this to assert the counter.
func (s *Server) SetHeartbeatMetrics(m *metrics.BackendMetrics) {
	if s.heartbeat != nil {
		s.heartbeat.metrics = m
	}
}

// SetLLMMaxConcurrent caps in-flight LLM gateway requests per user
// (U8/T8); 0 disables the cap. Call after NewServerWithLLM, before
// serving. Configured via LLM_MAX_CONCURRENT_PER_USER.
func (s *Server) SetLLMMaxConcurrent(n int) {
	if s.llm != nil {
		s.llm.maxConcurrent = n
	}
}

// SetLLMRequireKey controls whether identity-store users WITHOUT an
// LLM key are denied on /llm/ (security review #37 C2). Default false
// keeps legacy-open; set true unless LLM_OPEN_LEGACY=1.
func (s *Server) SetLLMRequireKey(v bool) {
	if s.llm != nil {
		s.llm.requireKey = v
	}
}

// handleHealthz reports liveness and orchestrator reachability without
// auth, for Gatus/load-balancer checks (U11): 200
// {"status":"ok","orchestrator":"<NodeInfo.Status>"} when NodeInfo
// succeeds, 503 {"status":"degraded","orchestrator":"unreachable"} when
// it fails.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	body := `{"status":"degraded","orchestrator":"unreachable"}`
	code := http.StatusServiceUnavailable
	if info, err := s.svc.sub.NodeInfo(r.Context()); err == nil {
		code = http.StatusOK
		body = fmt.Sprintf(`{"status":"ok","orchestrator":%q}`, info.Status)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

// isMetricsToken reports whether the request carries the scrape-only
// METRICS_TOKEN (constant-time compare). An unset token matches nothing.
func (s *Server) isMetricsToken(r *http.Request) bool {
	want := s.svc.cfg.MetricsToken
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// handleMetrics emits spoond's own Prometheus metrics (issue #20),
// gathered from the live Service state (pool, leases, identity) and
// rendered via the prometheus registry. Requires admin when the
// identity store is present (security review #37 M5), or the
// scrape-only METRICS_TOKEN.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.svc.identities != nil && !s.isMetricsToken(r) && !s.requireAdmin(w, r) {
		return
	}
	// Update live gauges from current service state before gathering.
	s.collectServiceMetrics()
	// Gather service-owned metrics from the registry.
	mfs, err := s.metrics.Registry.Gather()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "gather metrics: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	enc := expfmt.NewEncoder(w, expfmt.FmtText)
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			return
		}
	}
	// Orchestrator passthrough (U11): when OTEL_PROM_URL is set, fetch
	// the collector's Prometheus output and append it verbatim after a
	// marker line.
	if url := os.Getenv("OTEL_PROM_URL"); url != "" {
		resp, err := otelClient.Get(url)
		if err != nil {
			fmt.Fprintf(w, "# orchestrator metrics unavailable: %v\n", err)
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			fmt.Fprintf(w, "# orchestrator metrics unavailable: %v\n", err)
			return
		}
		fmt.Fprintln(w, "# --- orchestrator (otel) ---")
		_, _ = w.Write(body)
	}
}

// otelClient fetches the collector's Prometheus output for /metrics
// (U11); the 2 s timeout keeps a slow collector from stalling scrapes.
var otelClient = &http.Client{Timeout: 2 * time.Second}

// collectServiceMetrics updates live gauge metrics from the current
// service and identity state (issue #20). Called before gathering
// metrics for /metrics so gauges reflect the moment of scrape.
func (s *Server) collectServiceMetrics() {
	s.svc.CollectMetrics(s.metrics)
	// Identity user counts.
	if s.svc.identities != nil {
		persons, agents, admins := 0, 0, 0
		for _, u := range s.svc.identities.Users() {
			if u.Admin {
				admins++
			}
			if u.Kind == identity.KindAgent {
				agents++
			} else {
				persons++
			}
		}
		s.metrics.IdentityUsers.WithLabelValues("person").Set(float64(persons))
		s.metrics.IdentityUsers.WithLabelValues("agent").Set(float64(agents))
		s.metrics.IdentityAdmins.Set(float64(admins))
	}
	// Busy slots per owner.
	s.busyMu.Lock()
	for owner, n := range s.busyCount {
		s.metrics.BusySlots.WithLabelValues(owner).Set(float64(n))
	}
	s.busyMu.Unlock()
}

// apiLeasePathPrefix is the lease API's primary, documented path
// prefix; apiSandboxPathPrefix is its permanent alias. Every route
// registered under /api/sandboxes is served identically under
// /api/leases (2.0, D5): requests to the primary /api/leases form are
// rewritten onto the registered /api/sandboxes routes at the top of the
// handler chain — before auth and the mux — so there is one route
// table, one auth path and one set of metric labels.
const (
	apiLeasePathPrefix   = "/api/leases"
	apiSandboxPathPrefix = "/api/sandboxes"
)

// rewriteLeasePath maps an /api/leases… path onto its /api/sandboxes…
// twin. Only a whole path segment matches: /api/leasesX is not a lease
// path and stays itself (the mux then 404s it, as before).
func rewriteLeasePath(p string) string {
	switch {
	case p == apiLeasePathPrefix:
		return apiSandboxPathPrefix
	case strings.HasPrefix(p, apiLeasePathPrefix+"/"):
		return apiSandboxPathPrefix + p[len(apiLeasePathPrefix):]
	}
	return p
}

// Handler returns the HTTP handler with auth + metrics middleware
// applied. The /api/leases → /api/sandboxes rewrite sits at the top of
// the chain — before auth and the mux — so both path families share one
// auth path and one route table.
func (s *Server) Handler() http.Handler {
	authed := s.authMiddleware(s.mux)
	rewrite := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := rewriteLeasePath(r.URL.Path); p != r.URL.Path {
			r.URL.Path = p
		}
		authed.ServeHTTP(w, r)
	})
	return s.metricsMiddleware(rewrite)
}

// metricsMiddleware records HTTP request count and latency by path
// and method (issue #20). Wraps the outermost handler so it sees all
// requests including auth failures.
func (s *Server) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		path := normalizePath(r.URL.Path)
		s.metrics.HTTPReqs.WithLabelValues(path, r.Method, strconv.Itoa(sw.status)).Inc()
		s.metrics.HTTPDur.WithLabelValues(path).Observe(time.Since(start).Seconds())
	})
}

// statusWriter wraps http.ResponseWriter to capture the status code.
// It forwards Hijack so WebSocket upgrades (stream) work through the
// metrics middleware, and Flush so the Server-Sent Event streams can
// push bytes as they are produced.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

// Flush forwards a flush to the underlying writer when it supports one
// (http.ResponseWriter does not force it), so streamed responses are
// not buffered by the middleware wrapper.
// Unwrap lets http.ResponseController reach the connection (write
// deadlines on the event streams).
func (sw *statusWriter) Unwrap() http.ResponseWriter { return sw.ResponseWriter }

func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards the connection hijack to the underlying writer.
func (sw *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := sw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("websocket upgrade: response writer does not implement Hijacker")
	}
	sw.status = http.StatusSwitchingProtocols
	return h.Hijack()
}

// normalizePath reduces high-cardinality paths (sandbox ids, user ids)
// to stable labels for metrics.
func normalizePath(p string) string {
	// The /api/leases alias reports the /api/sandboxes labels: the
	// rewrite happens before the mux, so the request counters must not
	// split each route's series in two either.
	p = rewriteLeasePath(p)
	// Guest-service lease routes: the lease id is a capability, so it is
	// reduced like any other id to keep metrics cardinality bounded.
	// The documented route leaves "/active" as the only remainder.
	if strings.HasPrefix(p, "/lease/") {
		rest := strings.TrimPrefix(p, "/lease/")
		if i := strings.IndexByte(rest, '/'); i > 0 {
			return "/lease/:id" + rest[i:]
		} else if rest != "" {
			return "/lease/:id"
		}
		return "/lease/"
	}
	// Replace hex ids with :id
	if len(p) > 40 && isHexPath(p) {
		return "/api/sandboxes/:id"
	}
	if strings.HasPrefix(p, "/api/sandboxes/") {
		rest := strings.TrimPrefix(p, "/api/sandboxes/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) > 0 && len(parts[0]) >= 32 {
			if len(parts) > 1 {
				return "/api/sandboxes/:id/" + parts[1]
			}
			return "/api/sandboxes/:id"
		}
	}
	if strings.HasPrefix(p, "/api/users/") {
		rest := strings.TrimPrefix(p, "/api/users/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) > 0 && strings.HasPrefix(parts[0], "u-") {
			if len(parts) > 1 {
				return "/api/users/:id/" + parts[1]
			}
			return "/api/users/:id"
		}
	}
	return p
}

func isHexPath(p string) bool {
	for _, c := range p {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || c == '/' || c == '-') {
			return false
		}
	}
	return true
}

// authMiddleware authenticates the bearer token and injects the
// consumer id into the request context. /healthz is exempt (liveness);
// the /llm/ prefix is exempt — the lease id in the path is the
// capability, and sandboxes hold no consumer token.
// /api/admin/ is exempt because ADMIN_TOKEN is not a user/consumer
// token; api/admin.go authenticates those routes itself.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" ||
			strings.HasPrefix(r.URL.Path, "/api/admin/") || strings.HasPrefix(r.URL.Path, llmGatewayPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		// The scrape-only token reaches /metrics and nothing else;
		// handleMetrics accepts it in place of an admin.
		if r.URL.Path == "/metrics" && s.isMetricsToken(r) {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")
		if token == "" || token == auth {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		// Throttle repeated FAILED auths per IP (security review #37
		// L5). hit() returns true when the caller is now over the
		// limit; valid tokens always pass and reset the window.
		ip := clientIP(r.RemoteAddr)
		owner, ok := s.svc.ResolveOwner(token)
		if !ok {
			s.metrics.AuthFailures.Inc()
			if s.authFails.hit(ip) {
				s.metrics.AuthThrottled.Inc()
				writeError(w, http.StatusTooManyRequests, "too many failed auth attempts; try again shortly")
				return
			}
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		s.authFails.clear(ip)
		ctx := context.WithValue(r.Context(), ctxOwnerKey{}, owner)
		// Record the resolved identity (user id) when available so handlers
		// can distinguish an identity-store user from a legacy consumer.
		if s.svc.identities != nil {
			if u := s.svc.identities.UserByToken(token); u != nil {
				ctx = context.WithValue(ctx, ctxUserKey{}, u)
			}
		}
		// Trusted gateway impersonation (U6/T5): when the request carries the
		// SSH gateway's service token AND a X-Spoond-User-Id header, act as
		// that user so the backend's owner-scoping applies to the SSH caller
		// rather than the gateway service identity. The header user must
		// exist in the identity store.
		if s.svc.gatewayToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.svc.gatewayToken)) == 1 {
			if uid := r.Header.Get("X-Spoond-User-Id"); uid != "" {
				// Nil-identity guard (security review #37 rescan F12): the
				// gateway token can be configured without an identity store
				// (legacy single-consumer deployments); without this check a
				// request carrying X-Spoond-User-Id would nil-deref and
				// crash the goroutine.
				if s.svc.identities != nil {
					if u := s.svc.identities.UserByID(uid); u != nil {
						ctx = context.WithValue(ctx, ctxOwnerKey{}, u.ID)
						ctx = context.WithValue(ctx, ctxUserKey{}, u)
					} else {
						writeError(w, http.StatusForbidden, "unknown impersonated user")
						return
					}
				}
			}
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type ctxOwnerKey struct{}
type ctxUserKey struct{}

// authFailLimiter throttles repeated failed token auths per client IP
// (security review #37 L5). Tokens are high-entropy so brute force is
// not realistic, but the limiter also bounds /api/users/by-key probing
// and general junk traffic. 5 failures per 30s window → 429.
type authFailLimiter struct {
	mu      sync.Mutex
	fails   map[string][]time.Time // ip -> failure timestamps
	window  time.Duration
	maxHits int
}

func newAuthFailLimiter() *authFailLimiter {
	return &authFailLimiter{
		fails:   map[string][]time.Time{},
		window:  30 * time.Second,
		maxHits: 5,
	}
}

// hit records a failed auth for ip. Returns true when the caller is
// now over the limit and should be throttled.
func (l *authFailLimiter) hit(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cut := now.Add(-l.window)
	kept := l.fails[ip][:0]
	for _, t := range l.fails[ip] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	l.fails[ip] = kept
	return len(kept) > l.maxHits
}

// clear resets the failure window for ip after a successful auth.
func (l *authFailLimiter) clear(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, ip)
}

// clientIP extracts a client IP from RemoteAddr for limiter accounting.
func clientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// userFrom returns the identity-store user attached by authMiddleware,
// or nil for legacy consumer-token callers.
func userFrom(ctx context.Context) *identity.User {
	v, _ := ctx.Value(ctxUserKey{}).(*identity.User)
	return v
}

// maxExecTimeout caps a single exec call so it cannot run far past the
// lease TTL or tie up the controller indefinitely. Overridable via
// MAX_EXEC_TIMEOUT_SECS: compile-heavy CI steps (a Phoenix mix release,
// large cargo builds) legitimately run tens of minutes.
var maxExecTimeout = func() int {
	if v := os.Getenv("MAX_EXEC_TIMEOUT_SECS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 300 // seconds
}()

// ownerFrom extracts the authenticated owner from the request context.
func ownerFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxOwnerKey{}).(string)
	return v
}

// isAdmin reports whether the caller is an identity-store admin user.
// Legacy consumer-token callers are not admins.
func isAdmin(r *http.Request) bool {
	u := userFrom(r.Context())
	return u != nil && u.Admin
}

// handleCreate grants a new sandbox lease.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Image      string   `json:"image"`
		TTL        int      `json:"ttl"` // seconds
		MemoryMiB  int      `json:"memory_mib"`
		Network    string   `json:"network"`  // ignored
		InitCmd    string   `json:"init_cmd"` // ignored
		Persistent bool     `json:"persistent"`
		NetPolicy  string   `json:"network_policy"`
		NetAllow   []string `json:"egress_allowlist"`
		// ExposePorts publishes guest TCP ports for other sandboxes to
		// reach (each peer's egress policy decides reachability).
		ExposePorts []int `json:"expose_ports"`
		// Holder names what holds the lease (a CI job, a person's
		// scratch work) and HolderURL links to it. A non-empty holder
		// keeps the lease out of the TTL and idle sweeps. HoldTTL bounds
		// the hold (2.1): it expires on its own so a forgotten hold
		// cannot pin the lease forever.
		Holder    string `json:"holder"`
		HolderURL string `json:"holder_url"`
		HoldTTL   int    `json:"hold_ttl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Image == "" {
		writeError(w, http.StatusBadRequest, "image is required")
		return
	}
	// Egress policy (security review #37 rescan F3): default restricted —
	// NOT lan. The default must not let a guest reach other tenants'
	// sandboxes. Restricted allows the host service (LLM gateway, assets,
	// proxy) + configured allowlist, and blocks guest→guest. Operators who
	// need full LAN egress opt in explicitly.
	if req.NetPolicy == "" {
		req.NetPolicy = string(PolicyRestricted)
	}
	if !ValidNetworkPolicy(req.NetPolicy) {
		writeError(w, http.StatusBadRequest, "network_policy must be none|lan|internet|restricted")
		return
	}
	expose, err := ValidateExposePorts(req.ExposePorts)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateHolder(req.Holder, req.HolderURL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.HoldTTL < 0 {
		writeError(w, http.StatusBadRequest, "hold_ttl must be >= 0")
		return
	}
	ok, err := s.reg.Has(r.Context(), req.Image)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "image catalog unavailable")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "unknown image tag: "+req.Image)
		return
	}
	// Memory is fixed per image (D16): a snapshot restores with its
	// build's RAM, so a per-lease memory override is impossible.
	if req.MemoryMiB != 0 {
		img, err := s.svc.db.GetImage(r.Context(), req.Image)
		if err != nil {
			writeError(w, http.StatusNotFound, "unknown image tag: "+req.Image)
			return
		}
		if req.MemoryMiB != img.MemoryMB {
			writeError(w, http.StatusBadRequest, fmt.Sprintf(
				"memory is fixed per image on this backend: %s has %d MiB", req.Image, img.MemoryMB))
			return
		}
	}
	// Cap the requested TTL in seconds BEFORE converting to a duration,
	// so a huge ttl value cannot overflow time.Duration and bypass the
	// maxTTL cap (yielding a near-zero lease).
	ttlSecs := req.TTL
	if ttlSecs <= 0 {
		ttlSecs = int(s.svc.cfg.DefaultTTL / time.Second)
	}
	if maxSecs := int(s.svc.cfg.MaxTTL / time.Second); ttlSecs > maxSecs {
		ttlSecs = maxSecs
	}
	// The TTL cap above already bounds persistent leases; keep-alive
	// lets the consumer extend them (up to maxTTL per call).
	ttl := time.Duration(ttlSecs) * time.Second
	// Per-user TTL clamp (T4/#31): a user's max_ttl, when set, is the
	// hard ceiling even below the global max.
	if u := userFrom(r.Context()); u != nil && u.MaxTTL > 0 {
		if userMax := time.Duration(u.MaxTTL) * time.Second; ttl > userMax {
			ttl = userMax
		}
	}
	lease, err := s.svc.grant(r.Context(), ownerFrom(r.Context()), req.Image, ttl, req.Persistent, req.NetPolicy, req.NetAllow, req.Holder, req.HolderURL, expose...)
	if err != nil {
		switch {
		case errors.Is(err, errQuotaExceeded):
			writeError(w, http.StatusTooManyRequests, err.Error())
		case errors.Is(err, errUnknownImage):
			writeError(w, http.StatusNotFound, "unknown image tag: "+req.Image)
		case errors.Is(err, substrate.ErrCapacity):
			writeError(w, http.StatusServiceUnavailable, "capacity: "+err.Error())
		default:
			s.svc.log.Printf("create: grant %s: %v", req.Image, err)
			writeError(w, http.StatusInternalServerError, "failed to grant lease")
		}
		return
	}
	// Stamp the hold's clock on the granted lease (grant itself leaves
	// it zero so unheld grants carry no hold).
	if req.Holder != "" {
		s.svc.store.mu.Lock()
		s.svc.setHoldLocked(lease, req.Holder, req.HolderURL, time.Duration(req.HoldTTL)*time.Second, s.svc.now())
		s.svc.saveLeaseLocked(lease)
		s.svc.store.mu.Unlock()
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":              lease.ID,
		"owner":           lease.Owner,
		"address":         lease.HostIP,
		"image":           lease.Image,
		"ttl":             int(ttl.Seconds()),
		"persistent":      lease.Persistent,
		"expires_at":      lease.ExpiresAt.UTC().Format(time.RFC3339),
		"holder":          lease.Holder,
		"holder_url":      lease.HolderUrl,
		"hold_expires_at": formatRFC3339(lease.HoldExpiresAt),
		"hold_state":      holdState(lease),
		"exposed":         exposedMap(lease),
	})
}

// handleEndpoint reports a lease's sandbox endpoint: the substrate
// sandbox id and the host address guests use.
func (s *Server) handleEndpoint(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	// Shared leases are attachable over SSH (T6/#33).
	lease := s.svc.lookupWithShare(owner, id, ShareSSH)
	if lease == nil {
		writeError(w, http.StatusNotFound, "lease not found")
		return
	}
	// forkd_id keeps its pre-2.0 name: response keys are stored/protocol
	// data and renaming would break clients.
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         lease.ID,
		"forkd_id":   lease.SandboxID,
		"image":      lease.Image,
		"netns":      "",
		"guest_addr": lease.HostIP,
	})
}

// handleStream opens a WebSocket to a sandbox and relays an interactive
// PTY session: the client sends {"args":[...],"cwd":...,"binary":bool}
// as the first message. In text mode (the default), server events come
// back as one text JSON frame each (no trailing newline) and client text
// frames drive the process stdin. In binary mode, process output goes as
// WebSocket binary frames whose first byte selects the channel (1 stdout,
// 2 stderr, 3 pty), client binary frames are raw stdin, and text frames
// carry control JSON. started, exit_code and error stay text JSON frames
// in both modes.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	// Per-owner activity cap (security review #37 rescan F9).
	if !s.acquireBusy(owner) {
		http.Error(w, "too many concurrent exec/stream operations; try again shortly", http.StatusTooManyRequests)
		return
	}
	defer s.releaseBusy(owner)
	lease := s.svc.lookupWithShare(owner, id, ShareHTTP)
	if lease == nil && s.requestHasGatewayToken(r) {
		// The SSH gateway relays session channels through /stream, so a
		// request carrying its service token may attach over an ssh
		// share too (U09).
		lease = s.svc.lookupWithShare(owner, id, ShareSSH)
	}
	if lease == nil {
		writeError(w, http.StatusNotFound, "lease not found")
		return
	}
	s.svc.touch(id) // stream attach is activity for the idle sweeper
	// A suspended lease has no running sandbox; resume it first.
	if lease.Suspended {
		writeError(w, http.StatusConflict, "lease is suspended; resume it first")
		return
	}
	// A lease lost in a substrate crash has no sandbox to attach to; the
	// SSH gateway relays sessions through this route, so it covers SSH
	// too (U10).
	if lease.State == "lost" {
		writeError(w, http.StatusGone, lostLeaseMessage)
		return
	}

	upgrader := websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true }, // consumer-token auth above
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()

	// First message: the exec request.
	mt, payload, err := ws.ReadMessage()
	if err != nil {
		return
	}
	if mt != websocket.TextMessage {
		ws.WriteMessage(websocket.TextMessage, []byte(`{"error":"first message must be JSON text"}`))
		return
	}
	var req struct {
		Args   []string          `json:"args"`
		Cwd    string            `json:"cwd"`
		Env    map[string]string `json:"env"`
		Pty    *bool             `json:"pty"`
		Binary bool              `json:"binary"`
		Cols   uint32            `json:"cols"`
		Rows   uint32            `json:"rows"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte(`{"error":"bad request JSON"}`))
		return
	}
	if len(req.Args) == 0 {
		ws.WriteMessage(websocket.TextMessage, []byte(`{"error":"args required"}`))
		return
	}
	pty := true
	if req.Pty != nil {
		pty = *req.Pty
	}
	cols, rows := req.Cols, req.Rows
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	// A pooled sandbox was created for the "pool" placeholder lease, so
	// its envd default SPOOND_LEASE_ID is "pool"; the real lease id rides
	// every request's env instead.
	if lease.pooled {
		if req.Env == nil {
			req.Env = map[string]string{}
		}
		req.Env["SPOOND_LEASE_ID"] = lease.ID
	}

	proc, err := s.svc.sub.Start(r.Context(), lease.SandboxID, substrate.StartRequest{
		Args:  req.Args,
		Env:   req.Env,
		Cwd:   req.Cwd,
		PTY:   pty,
		Cols:  cols,
		Rows:  rows,
		Stdin: true,
	})
	if err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte(`{"error":"agent unreachable: `+err.Error()+`"}`))
		return
	}
	s.svc.log.Printf("stream: %s: start", lease.ID)

	// Substrate -> WS relay: one frame per event, no trailing newline.
	// The relay runs until EventExit or EventError (Events is closed
	// after either). Process output rides binary frames in binary mode
	// (byte 0 = channel: 1 stdout, 2 stderr, 3 pty); started, exit_code
	// and error are always text JSON.
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		for ev := range proc.Events() {
			switch ev.Kind {
			case substrate.EventStarted:
				line, err := json.Marshal(struct {
					Stream string `json:"stream"`
					PID    uint32 `json:"pid"`
					Pty    bool   `json:"pty"`
				}{"started", ev.PID, pty})
				if err != nil {
					continue
				}
				if err := ws.WriteMessage(websocket.TextMessage, line); err != nil {
					return
				}
			case substrate.EventStdout, substrate.EventStderr, substrate.EventPTY:
				if req.Binary {
					ch := byte(1)
					if ev.Kind == substrate.EventStderr {
						ch = 2
					} else if ev.Kind == substrate.EventPTY {
						ch = 3
					}
					frame := make([]byte, 0, len(ev.Data)+1)
					frame = append(frame, ch)
					frame = append(frame, ev.Data...)
					if err := ws.WriteMessage(websocket.BinaryMessage, frame); err != nil {
						return
					}
					continue
				}
				line, err := json.Marshal(struct {
					Out string `json:"out"`
				}{string(ev.Data)})
				if err != nil {
					continue
				}
				if err := ws.WriteMessage(websocket.TextMessage, line); err != nil {
					return
				}
			case substrate.EventExit:
				line, err := json.Marshal(struct {
					ExitCode int `json:"exit_code"`
				}{ev.ExitCode})
				if err != nil {
					return
				}
				_ = ws.WriteMessage(websocket.TextMessage, line)
				return
			case substrate.EventError:
				line, err := json.Marshal(struct {
					Error string `json:"error"`
				}{ev.Err})
				if err != nil {
					return
				}
				_ = ws.WriteMessage(websocket.TextMessage, line)
				return
			}
		}
	}()

	// WS -> substrate relay: binary frames are raw stdin; text frames
	// are control JSON — {"in":"..."} (text mode only), {"action":"stop"}
	// (SIGTERM), {"action":"kill"} (SIGKILL), {"action":"eof"},
	// {"resize":{"cols":C,"rows":R}}; anything else is ignored.
	for {
		mt, payload, err := ws.ReadMessage()
		if err != nil {
			// The WebSocket closed first: stop streaming but do NOT kill
			// the process (the agent sees EOF).
			_ = proc.Close()
			break
		}
		if mt == websocket.BinaryMessage {
			_ = proc.Write(payload)
			continue
		}
		var msg struct {
			In     string `json:"in"`
			Action string `json:"action"`
			Resize *struct {
				Cols uint32 `json:"cols"`
				Rows uint32 `json:"rows"`
			} `json:"resize"`
		}
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		switch {
		case msg.In != "":
			if !req.Binary {
				_ = proc.Write([]byte(msg.In))
			}
		case msg.Action == "stop":
			// SIGTERM, then keep relaying until the process exits.
			_ = proc.Signal(false)
			<-relayDone
			return
		case msg.Action == "kill":
			// SIGKILL, then keep relaying until the process exits.
			_ = proc.Signal(true)
			<-relayDone
			return
		case msg.Action == "eof":
			_ = proc.CloseStdin()
		case msg.Resize != nil:
			_ = proc.Resize(msg.Resize.Cols, msg.Resize.Rows)
		}
	}
	<-relayDone
}

// requestHasGatewayToken reports whether the request's bearer token is
// the SSH gateway's service token (constant-time compare). Such requests
// may reach leases through an ssh share (U09).
func (s *Server) requestHasGatewayToken(r *http.Request) bool {
	if s.svc.gatewayToken == "" {
		return false
	}
	auth := r.Header.Get("Authorization")
	token := strings.TrimPrefix(auth, "Bearer ")
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.svc.gatewayToken)) == 1
}

// handleNetwork changes a lease's egress policy live (U09): the lease is
// updated and saved, the egress config is re-applied to its sandbox, and
// every peer's allowances are refreshed. Owner only.
func (s *Server) handleNetwork(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	var req struct {
		NetPolicy string   `json:"network_policy"`
		NetAllow  []string `json:"egress_allowlist"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !ValidNetworkPolicy(req.NetPolicy) {
		writeError(w, http.StatusBadRequest, "network_policy must be none|lan|internet|restricted")
		return
	}
	lease, err := s.svc.setNetwork(r.Context(), owner, id, req.NetPolicy, req.NetAllow)
	if err != nil {
		switch err {
		case errNotFound:
			writeError(w, http.StatusNotFound, "lease not found")
		case errSuspended:
			writeError(w, http.StatusConflict, "lease is suspended; resume it first")
		default:
			s.svc.log.Printf("network %s: %v", id, err)
			writeError(w, http.StatusInternalServerError, "network update failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":               lease.ID,
		"network_policy":   lease.NetPolicy,
		"egress_allowlist": lease.NetAllow,
	})
}

// handleKeepAlive extends a persistent lease's expiry. The caller must
// own the lease. Body may carry {"ttl": seconds} (capped at maxTTL).
func (s *Server) handleKeepAlive(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	var req struct {
		TTL int `json:"ttl"` // seconds; 0 = maxTTL
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	ttl := time.Duration(req.TTL) * time.Second
	lease, err := s.svc.keepAlive(owner, id, ttl)
	if err != nil {
		switch err {
		case errNotFound:
			writeError(w, http.StatusNotFound, "lease not found")
		case errNotPersistent:
			writeError(w, http.StatusBadRequest, "lease is not a persistent lease")
		default:
			writeError(w, http.StatusInternalServerError, "keepalive failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         lease.ID,
		"persistent": true,
		"expires_at": lease.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// handleSuspend suspends a workspace-backed persistent lease: the
// controller snapshots the sandbox and stops it. The lease stays and can
// be resumed.
func (s *Server) handleSuspend(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	lease, err := s.svc.suspend(r.Context(), owner, id)
	if err != nil {
		switch err {
		case errNotFound:
			writeError(w, http.StatusNotFound, "lease not found")
		case errNotPersistent:
			writeError(w, http.StatusBadRequest, "lease is not a workspace-backed persistent lease")
		case errLeaseBusy:
			writeError(w, http.StatusConflict, err.Error())
		default:
			s.svc.log.Printf("suspend %s: %v", id, err)
			writeError(w, http.StatusInternalServerError, "suspend failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      lease.ID,
		"status":  "suspended",
		"message": "lease suspended; state snapshot kept (resume to restore)",
	})
}

// handleRestart reboots a persistent lease (workspace-backed: snapshot +
// resume; plain: kill + cold spawn).
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	lease, err := s.svc.restart(r.Context(), owner, id)
	if err != nil {
		switch err {
		case errNotFound:
			writeError(w, http.StatusNotFound, "lease not found")
		case errNotPersistent:
			writeError(w, http.StatusBadRequest, "lease is not a persistent lease")
		case errLeaseBusy:
			writeError(w, http.StatusConflict, err.Error())
		default:
			s.svc.log.Printf("restart %s: %v", id, err)
			writeError(w, http.StatusInternalServerError, "restart failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      lease.ID,
		"status":  "running",
		"message": "lease restarted",
	})
}

// handleTag assigns a friendly name to a lease.
func (s *Server) handleTag(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	lease, err := s.svc.setName(owner, id, req.Name)
	if err != nil {
		if err == errNotFound {
			writeError(w, http.StatusNotFound, "lease not found")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":   lease.ID,
		"name": lease.Name,
		"ok":   true,
	})
}

// handleComment sets or clears the free-text annotation on a lease.
// An empty comment clears it.
func (s *Server) handleComment(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	var req struct {
		Comment string `json:"comment"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	lease, err := s.svc.setComment(owner, id, req.Comment)
	if err != nil {
		if err == errNotFound {
			writeError(w, http.StatusNotFound, "lease not found")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      lease.ID,
		"comment": lease.Comment,
		"ok":      true,
	})
}

// handleHolder sets or clears what holds a lease, or renews the hold
// (2.1): request {"holder":"…","holder_url":"…","hold_ttl":secs}.
// Both holder fields empty clears. The same holder renews the hold for
// another HOLD_TTL_SECS (or the explicit, capped hold_ttl) from now; a
// different holder is refused with 409. A held lease is not released
// at its TTL, not idle-suspended, and checkpointed periodically like a
// persistent lease; its hold expires on its own and the automatic
// limits act regardless. Owner or admin; anyone else gets the same 404
// as the other lease routes (no existence leak).
func (s *Server) handleHolder(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	var req struct {
		Holder    string `json:"holder"`
		HolderURL string `json:"holder_url"`
		HoldTTL   int    `json:"hold_ttl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.HoldTTL < 0 {
		writeError(w, http.StatusBadRequest, "hold_ttl must be >= 0")
		return
	}
	lease := s.svc.lookup(owner, id)
	if lease == nil && isAdmin(r) {
		lease = s.svc.lookupAny(id)
	}
	if lease == nil {
		writeError(w, http.StatusNotFound, "lease not found")
		return
	}
	var (
		updated *Lease
		err     error
	)
	// Validation first (400 naming the field), then the holder match:
	// a malformed request is rejected as malformed even if the holder
	// would not match either.
	if err := validateHolder(req.Holder, req.HolderURL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Holder != "" && lease.Holder != "" && req.Holder != lease.Holder {
		// A different holder takes the lease away from the one holding
		// it: refused instead of silently replacing.
		writeError(w, http.StatusConflict, errHolderMismatch.Error())
		return
	}
	if lease.Holder != "" && req.Holder == lease.Holder {
		updated, err = s.svc.renewHolder(lease.Owner, id, req.Holder, req.HolderURL, time.Duration(req.HoldTTL)*time.Second)
	} else {
		updated, err = s.svc.setHolderWithTTL(lease.Owner, id, req.Holder, req.HolderURL, time.Duration(req.HoldTTL)*time.Second)
	}
	if err != nil {
		if err == errNotFound {
			writeError(w, http.StatusNotFound, "lease not found")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":              updated.ID,
		"holder":          updated.Holder,
		"holder_url":      updated.HolderUrl,
		"hold_expires_at": formatRFC3339(updated.HoldExpiresAt),
		"hold_state":      holdState(updated),
		"ok":              true,
	})
}

// handlePrompt sends a message to the Shelley coding agent running inside
// a lease and returns the agent's reply. Requires the agent to be up
// (see the `shelly` ctl verb / runShelly). Implements `shelley prompt`
// from exe.dev's CLI surface as an LLM-callable API command.
func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	var req struct {
		Message string `json:"message"`
		Model   string `json:"model,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Message) == "" {
		writeError(w, http.StatusBadRequest, "{\"message\":\"...\"} required")
		return
	}
	// Shared leases accept agent prompts too (T6/#33).
	lease := s.svc.lookupWithShare(owner, id, ShareSSH)
	if lease == nil {
		writeError(w, http.StatusNotFound, "lease not found")
		return
	}
	if lease.Suspended {
		writeError(w, http.StatusConflict, "lease is suspended; resume it first")
		return
	}
	model := req.Model
	if model == "" {
		model = "gpt-oss-20b-fireworks"
	}
	msg64 := base64.StdEncoding.EncodeToString([]byte(req.Message))
	mod64 := base64.StdEncoding.EncodeToString([]byte(model))
	script := fmt.Sprintf(`set -e
MSG=$(echo %s | base64 -d)
MOD=$(echo %s | base64 -d)
RESP=$(curl -sf --max-time 60 -H 'Content-Type: application/json' \
  -d "{\"message\":$(printf '%%s' "$MSG" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))'),\"model\":$(printf '%%s' "$MOD" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))')}" \
  http://127.0.0.1:9000/api/conversations/new) || { echo "SHELLEY_NOT_RUNNING"; exit 1; }
CID=$(echo "$RESP" | python3 -c 'import sys,json;print(json.load(sys.stdin)["conversation_id"])')
for i in $(seq 1 40); do
  sleep 5
  OUT=$(curl -sf --max-time 10 http://127.0.0.1:9000/api/conversation/$CID 2>/dev/null || true)
  AGENT=$(echo "$OUT" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    raise SystemExit
for m in d.get("messages", []):
    if m.get("type") == "agent":
        ld = json.loads(m.get("llm_data") or "{}")
        content = ld.get("Content") or []
        text = " ".join(c.get("Text","") for c in content if isinstance(c, dict))
        if text.strip():
            print(text)
            raise SystemExit
' 2>/dev/null || true)
  if [ -n "$AGENT" ]; then echo "$AGENT"; exit 0; fi
done
echo "AGENT_TIMEOUT"`, msg64, mod64)

	s.svc.log.Printf("prompt %s: %s", id, req.Message)
	start := time.Now()
	res, err := s.svc.sub.Exec(r.Context(), lease.SandboxID, substrate.ExecRequest{
		Args:    buildShellArgs(script, "", requestEnv(lease, nil)),
		Timeout: 240 * time.Second,
	})
	if err != nil {
		s.svc.log.Printf("prompt %s: %v (dur=%s)", id, err, time.Since(start))
		writeError(w, http.StatusBadGateway, "agent exec failed: "+err.Error())
		return
	}
	s.svc.log.Printf("prompt %s: exit=%d stdout=%d dur=%s", id, res.ExitCode, len(res.Stdout), time.Since(start))
	out := res.Stdout
	if strings.Contains(out, "SHELLEY_NOT_RUNNING") {
		writeError(w, http.StatusConflict, "shelley agent is not running in this lease — use the shelly ctl verb first")
		return
	}
	if strings.Contains(out, "AGENT_TIMEOUT") {
		writeError(w, http.StatusGatewayTimeout, "agent did not reply within 200s")
		return
	}
	if res.ExitCode != 0 {
		writeError(w, http.StatusBadGateway, "agent exec failed: "+tailStr(res.Stderr, 500))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      id,
		"message": req.Message,
		"reply":   strings.TrimSpace(out),
	})
}

// handleResume restores a suspended workspace-backed lease.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	lease, err := s.svc.resume(r.Context(), owner, id)
	if err == errNotFound && s.requestHasGatewayToken(r) {
		// The SSH gateway resumes a suspended lease before a session
		// starts, for a user it has already authorised; its service token
		// is owner-blind here as it is for stream lookups.
		lease, err = s.svc.resumeAny(r.Context(), id)
	}
	if err != nil {
		switch err {
		case errNotFound:
			writeError(w, http.StatusNotFound, "lease not found")
		case errNotPersistent:
			writeError(w, http.StatusBadRequest, "lease is not a workspace-backed persistent lease")
		case errLeaseBusy:
			writeError(w, http.StatusConflict, err.Error())
		default:
			s.svc.log.Printf("resume %s: %v", id, err)
			writeError(w, http.StatusInternalServerError, "resume failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      lease.ID,
		"status":  "running",
		"address": lease.HostIP,
	})
}

// handleList returns the caller's leases.
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	leases := s.svc.list(owner)
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": leases})
}

// handleExec runs a command in a sandbox owned by the caller.
func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	// Per-owner activity cap (security review #37 rescan F9).
	if !s.acquireBusy(owner) {
		writeError(w, http.StatusTooManyRequests, "too many concurrent exec/stream operations; try again shortly")
		return
	}
	defer s.releaseBusy(owner)
	// Shared leases are executable over the API (T6/#33).
	lease := s.svc.lookupWithShare(owner, id, ShareHTTP)
	if lease == nil {
		writeError(w, http.StatusNotFound, "lease not found")
		return
	}
	s.svc.touch(id) // exec is activity for the idle sweeper
	// A suspended workspace-backed lease has no running sandbox; resume
	// first.
	if lease.Suspended {
		writeError(w, http.StatusConflict, "lease is suspended; resume it first")
		return
	}
	// A lease lost in a substrate crash has nothing to exec into (U10).
	if lease.State == "lost" {
		writeError(w, http.StatusGone, lostLeaseMessage)
		return
	}
	var req struct {
		Cmd     string            `json:"cmd"`
		Cwd     string            `json:"cwd"`
		Env     map[string]string `json:"env"`
		Timeout int               `json:"timeout"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Cmd == "" {
		writeError(w, http.StatusBadRequest, "cmd is required")
		return
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	if timeout > maxExecTimeout {
		timeout = maxExecTimeout
	}
	args := buildShellArgs(req.Cmd, req.Cwd, requestEnv(lease, req.Env))
	start := time.Now()
	res, err := s.svc.sub.Exec(r.Context(), lease.SandboxID, substrate.ExecRequest{
		Args:    args,
		Timeout: time.Duration(timeout) * time.Second,
	})
	if err != nil {
		s.svc.log.Printf("exec: %s: %v (dur=%s)", lease.SandboxID, err, time.Since(start))
		// Map substrate ErrNotFound (sandbox gone from the orchestrator
		// but the lease still exists in our store) to 410 Gone so the
		// caller can distinguish a permanently dead sandbox from a
		// transient exec failure (e.g. node overload, network blip).
		if errors.Is(err, substrate.ErrNotFound) {
			writeError(w, http.StatusGone, "lease no longer exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "exec failed")
		return
	}
	s.svc.log.Printf("exec: %s: exit=%d stdout=%d stderr=%d dur=%s", lease.SandboxID, res.ExitCode, len(res.Stdout), len(res.Stderr), time.Since(start))
	if res.ExitCode != 0 {
		s.svc.log.Printf("exec: %s: stderr=%q", lease.SandboxID, tailStr(res.Stderr, 500))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stdout": res.Stdout,
		"stderr": res.Stderr,
		"exit":   res.ExitCode,
	})
}

// handleStat returns lightweight guest-side metrics for a sandbox
// (ticket #25): vCPU load, memory, disk, and network RX/TX. Data comes
// from a one-shot exec probe (KTD5-style, stateless, 5s timeout) —
// zero controller changes; works for any image with procfs.
func (s *Server) handleStat(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	lease := s.svc.lookupWithShare(owner, id, ShareHTTP)
	if lease == nil {
		writeError(w, http.StatusNotFound, "lease not found")
		return
	}
	s.svc.touch(id)
	if lease.Suspended {
		writeError(w, http.StatusConflict, "lease is suspended; resume it first")
		return
	}
	const probe = `set -e
echo "== loadavg =="; cat /proc/loadavg
echo "== meminfo =="; grep -E '^(MemTotal|MemAvailable):' /proc/meminfo
echo "== netdev =="; cat /proc/net/dev
echo "== df =="; df -P /
`
	res, err := s.svc.sub.Exec(r.Context(), lease.SandboxID, substrate.ExecRequest{
		Args:    buildShellArgs(probe, "", requestEnv(lease, nil)),
		Timeout: 5 * time.Second,
	})
	if err != nil {
		if errors.Is(err, substrate.ErrNotFound) {
			writeError(w, http.StatusGone, "lease no longer exists")
			return
		}
		s.svc.log.Printf("stat: %s: %v", lease.SandboxID, err)
		writeError(w, http.StatusInternalServerError, "stat probe failed")
		return
	}
	if res.ExitCode != 0 {
		s.svc.log.Printf("stat: %s: probe exit=%d stderr=%q", lease.SandboxID, res.ExitCode, tailStr(res.Stderr, 300))
		writeError(w, http.StatusInternalServerError, "stat probe exited non-zero")
		return
	}
	stat, perr := parseStatProbe(res.Stdout)
	if perr != nil {
		s.svc.log.Printf("stat: %s: parse: %v (stdout=%q)", lease.SandboxID, perr, tailStr(res.Stdout, 300))
		writeError(w, http.StatusInternalServerError, "stat probe parse failed")
		return
	}
	writeJSON(w, http.StatusOK, stat)
}

// statResult is the shaped /stat response.
type statResult struct {
	CPU struct {
		Load1 float64 `json:"load1"`
	} `json:"cpu"`
	Mem struct {
		UsedMiB  int64 `json:"used_mib"`
		TotalMiB int64 `json:"total_mib"`
	} `json:"mem"`
	Disk struct {
		UsedMiB  int64 `json:"used_mib"`
		TotalMiB int64 `json:"total_mib"`
	} `json:"disk"`
	Net struct {
		RXBytes int64 `json:"rx_bytes"`
		TXBytes int64 `json:"tx_bytes"`
	} `json:"net"`
}

// parseStatProbe parses the output of the stat probe script into a
// statResult. The script emits section markers (== name ==) followed by
// raw /proc-style lines; we extract what we need by section + field.
func parseStatProbe(out string) (*statResult, error) {
	var st statResult
	section := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		trim := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trim, "== "):
			section = strings.TrimSuffix(strings.TrimPrefix(trim, "== "), " ==")
			continue
		case trim == "":
			continue
		}
		switch section {
		case "loadavg":
			f := strings.Fields(trim)
			if len(f) >= 1 {
				if v, err := strconv.ParseFloat(f[0], 64); err == nil {
					st.CPU.Load1 = v
				}
			}
		case "meminfo":
			f := strings.Fields(trim)
			if len(f) >= 2 {
				switch strings.TrimSuffix(f[0], ":") {
				case "MemTotal":
					st.Mem.TotalMiB = kibToMiB(parseInt64(f[1]))
				case "MemAvailable":
					st.Mem.UsedMiB = st.Mem.TotalMiB - kibToMiB(parseInt64(f[1]))
				}
			}
		case "netdev":
			// Format: iface: rx_bytes ... tx_bytes ...
			if !strings.Contains(trim, ":") {
				continue
			}
			iface := strings.TrimSuffix(strings.SplitN(trim, ":", 2)[0], ":")
			if iface == "lo" {
				continue
			}
			f := strings.Fields(strings.SplitN(trim, ":", 2)[1])
			if len(f) >= 9 {
				st.Net.RXBytes += parseInt64(f[0])
				st.Net.TXBytes += parseInt64(f[8])
			}
		case "df":
			f := strings.Fields(trim)
			// df -P: Filesystem 1K-blocks Used Available Use% Mounted on
			// (6 fields); match the root mount in the LAST field.
			if len(f) >= 6 && f[len(f)-1] == "/" {
				st.Disk.TotalMiB = kibToMiB(parseInt64(f[1]))
				st.Disk.UsedMiB = kibToMiB(parseInt64(f[2]))
			}
		}
	}
	// The probe is useless if nothing was captured.
	if st.Mem.TotalMiB == 0 && st.Disk.TotalMiB == 0 && st.CPU.Load1 == 0 {
		return nil, fmt.Errorf("no usable stat sections")
	}
	return &st, nil
}

func kibToMiB(kib int64) int64 { return kib / 1024 }

func parseInt64(s string) int64 {
	v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return v
}

// handleDelete releases a sandbox owned by the caller.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	lease := s.svc.lookup(owner, id)
	if lease == nil {
		writeError(w, http.StatusNotFound, "lease not found")
		return
	}
	s.svc.release(r.Context(), lease)
	w.WriteHeader(http.StatusNoContent)
}

// handleClone checkpoints a running sandbox and grants a fresh
// persistent lease on the checkpoint build. The optional {"tag": "..."}
// body is accepted and ignored. The clone copies the source's network
// policy and exposes nothing.
func (s *Server) handleClone(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	var req struct {
		Tag string `json:"tag"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // optional body; tag ignored

	cloned, buildID, err := s.svc.clone(r.Context(), owner, id)
	if err != nil {
		switch {
		case errors.Is(err, errNotFound):
			writeError(w, http.StatusNotFound, "lease not found")
		case errors.Is(err, errLeaseBusy):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, errQuotaExceeded):
			// Quota enforcement (security review #37 rescan F1): clone
			// surfaces the same 429 as create, not a generic 500.
			writeError(w, http.StatusTooManyRequests, err.Error())
		case errors.Is(err, substrate.ErrCapacity):
			writeError(w, http.StatusServiceUnavailable, "capacity: "+err.Error())
		default:
			s.svc.log.Printf("clone %s: %v", id, err)
			writeError(w, http.StatusInternalServerError, "failed to clone lease")
		}
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         cloned.ID,
		"image":      cloned.Image,
		"source":     id,
		"branch_tag": buildID,
		"persistent": true,
		"expires_at": cloned.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// handleFork checkpoints a running sandbox once and creates count
// sandboxes from the checkpoint build. Owner only, as for clone (no
// shares).
func (s *Server) handleFork(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	var req struct {
		Count      int    `json:"count"`
		Persistent bool   `json:"persistent"`
		TTL        int    `json:"ttl"` // seconds
		Holder     string `json:"holder"`
		HolderURL  string `json:"holder_url"`
		HoldTTL    int    `json:"hold_ttl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := validateHolder(req.Holder, req.HolderURL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.HoldTTL < 0 {
		writeError(w, http.StatusBadRequest, "hold_ttl must be >= 0")
		return
	}
	leases, buildID, err := s.svc.fork(r.Context(), owner, id, req.Count, req.Persistent, time.Duration(req.TTL)*time.Second, req.Holder, req.HolderURL)
	if err != nil {
		switch {
		case errors.Is(err, errBadForkCount):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, errNotFound):
			writeError(w, http.StatusNotFound, "lease not found")
		case errors.Is(err, errSuspended):
			writeError(w, http.StatusConflict, "lease is suspended; resume it first")
		case errors.Is(err, errLeaseBusy):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, errQuotaExceeded):
			writeError(w, http.StatusTooManyRequests, err.Error())
		case errors.Is(err, substrate.ErrCapacity):
			writeError(w, http.StatusServiceUnavailable, "capacity: "+err.Error())
		default:
			s.svc.log.Printf("fork %s: %v", id, err)
			writeError(w, http.StatusInternalServerError, "failed to fork lease")
		}
		return
	}
	ids := make([]string, len(leases))
	for i, l := range leases {
		ids[i] = l.ID
		if req.Holder != "" {
			s.svc.store.mu.Lock()
			s.svc.setHoldLocked(l, req.Holder, req.HolderURL, time.Duration(req.HoldTTL)*time.Second, s.svc.now())
			s.svc.saveLeaseLocked(l)
			s.svc.store.mu.Unlock()
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"source":          id,
		"build_id":        buildID,
		"ids":             ids,
		"hold_expires_at": formatRFC3339(leases[0].HoldExpiresAt),
		"hold_state":      holdState(leases[0]),
	})
}

// handleGetSandbox returns one lease: the same object as an element of
// GET /api/sandboxes plus the lifecycle fields. The owner or an http
// share is required.
func (s *Server) handleGetSandbox(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	lease := s.svc.lookupWithShare(owner, id, ShareHTTP)
	if lease == nil {
		writeError(w, http.StatusNotFound, "lease not found")
		return
	}
	writeJSON(w, http.StatusOK, leaseDetailMap(lease))
}

// handleImages lists the images with a current build. Without a detail
// query it returns their names; with ?detail=1 it returns the full
// catalog rows.
func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	imgs, err := s.svc.db.ListImages(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "image catalog unavailable")
		return
	}
	if r.URL.Query().Get("detail") == "1" {
		out := []map[string]any{}
		for _, img := range imgs {
			if img.CurrentBuildID == "" {
				continue
			}
			out = append(out, map[string]any{
				"name":        img.Name,
				"build_id":    img.CurrentBuildID,
				"template_id": img.TemplateID,
				"digest":      img.Digest,
				"vcpu":        img.VCPU,
				"memory_mb":   img.MemoryMB,
				"disk_mb":     img.DiskMB,
				"updated_at":  img.UpdatedAt.UTC().Format(time.RFC3339),
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"images": out})
		return
	}
	names := []string{}
	for _, img := range imgs {
		if img.CurrentBuildID != "" {
			names = append(names, img.Name)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"images": names})
}

// handleByName resolves a friendly lease name owned by the caller to its
// lease id. Used by the SSH gateway (username = name) and for
// script/LLM convenience. Owner-scoped: names are unique per owner, so
// a caller can only resolve their own.
func (s *Server) handleByName(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	lease := s.svc.lookupByNameForOwner(ownerFrom(r.Context()), name)
	if lease == nil {
		writeError(w, http.StatusNotFound, "no lease named "+name)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":    lease.ID,
		"name":  lease.Name,
		"image": lease.Image,
	})
}

// requestEnv merges a request's env with the lease id when the lease
// was served from the warm pool: the sandbox's envd default
// SPOOND_LEASE_ID is "pool" and env vars cannot be updated after
// create, so pooled leases carry the real id on every request.
func requestEnv(lease *Lease, env map[string]string) map[string]string {
	if !lease.pooled {
		return env
	}
	out := make(map[string]string, len(env)+1)
	for k, v := range env {
		out[k] = v
	}
	out["SPOOND_LEASE_ID"] = lease.ID
	return out
}

// buildShellArgs wraps a command with cwd/env into a single shell
// invocation, since the substrate's exec takes argv and no cwd/env. Both env
// keys and values are shell-quoted so a hostile key cannot inject
// shell metacharacters.
func buildShellArgs(cmd, cwd string, env map[string]string) []string {
	var parts []string
	if cwd != "" {
		parts = append(parts, "cd "+shellQuote(cwd)+" &&")
	}
	for k, v := range env {
		parts = append(parts, "export "+shellQuote(k)+"="+shellQuote(v)+";")
	}
	parts = append(parts, cmd)
	// Use bash, not sh. GitHub Actions / Forgejo wrap `run:` steps with
	// `set -euo pipefail`; /bin/sh on Debian is dash, which rejects
	// `-o pipefail` (dash only accepts `-o` options in POSIX form), so
	// steps that contain a pipe fail with "set: Illegal option -o
	// pipefail". Bash is present in all base images and is the GitHub
	// Actions default shell.
	return []string{"/bin/bash", "-c", strings.Join(parts, " ")}
}

// shellQuote wraps s in single quotes, escaping embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// tailStr returns the last n bytes of s, prefixed with a truncation marker
// if s was longer than n. Used for logging stderr tails on exec failures.
func tailStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "...[truncated]..." + s[len(s)-n:]
}
