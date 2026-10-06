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
	// Metrics (issue #20): service-owned Prometheus metrics served at
	// /metrics alongside the orchestrator's collector output.
	metrics *metrics.BackendMetrics
	// readyz (issue #81) caches the readiness result for 5 s so an
	// external uptime monitor can poll /readyz cheaply.
	readyz *readyzState
	// authFails (security review #37 L5) throttles repeated failed
	// token auths per client IP.
	authFails *authFailLimiter
	// busy (security review #37 rescan F9) caps concurrent exec/stream
	// operations per owner so a tenant can't saturate the controller
	// with in-flight activity (quota covers lease count, not activity).
	busyMu    sync.Mutex
	busyCount map[string]int
	busyMax   int

	// fileXfer bounds concurrent file-content transfers backend-wide:
	// each buffers up to maxFileBytes in memory.
	fileXfer chan struct{}

	// dials (2.2, #113) caps concurrent guest port dials per owner, in
	// its own pool: dialing a guest port is not exec/stream activity and
	// the two caps must not eat each other's slots.
	dialMu    sync.Mutex
	dialCount map[string]int
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

// Metrics exposes the server's Prometheus collector, for wiring that
// happened after NewServerWithLLM and needs to feed the same registry
// (the webhook notifier's spoond_notifications_total, 2.2 #117).
func (s *Server) Metrics() *metrics.BackendMetrics { return s.metrics }

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
		busyCount: map[string]int{}, busyMax: 8, fileXfer: make(chan struct{}, maxFileTransfers),
		dialCount: map[string]int{}, metrics: metrics.NewBackendMetrics()}
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
	s.mux.HandleFunc("GET /api/sandboxes/queue", s.handleQueue)
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
	// lease. The /api/leases alias covers both via rewriteLeasePath; the
	// events-only EVENTS_TOKEN is admitted before that rewrite, on
	// GET /api/leases/events only (api/server.go).
	s.mux.HandleFunc("GET /api/sandboxes/events", s.handleLeaseEvents)
	s.mux.HandleFunc("GET /api/sandboxes/{id}/events", s.handleLeaseEventsOne)
	// Held leases (2.1): set or clear what holds a lease later. Owner or
	// admin; the handler 404s for anyone else, like the other lease
	// routes.
	s.mux.HandleFunc("PUT /api/sandboxes/{id}/holder", s.handleHolder)
	// Per-lease checkpoint interval (2.3, #122): owner or admin, 404 for
	// anyone else.
	s.mux.HandleFunc("PUT /api/sandboxes/{id}/checkpoint-policy", s.handleCheckpointPolicy)
	// Per-lease idle reclamation threshold (2.5, #129 part 2): owner or
	// admin, 404 for anyone else.
	s.mux.HandleFunc("PUT /api/sandboxes/{id}/idle-policy", s.handleIdlePolicy)
	// Restore in place to a kept checkpoint (2.3, #121): owner or admin,
	// 404 for anyone else and for a build that is not the lease's own.
	s.mux.HandleFunc("POST /api/sandboxes/{id}/restore", s.handleRestore)
	// Lease file operations (#114): download/upload/stat/mkdir/remove a
	// guest file through the substrate. Owner or admin; 404 for anyone
	// else, 409 while suspended.
	s.mux.HandleFunc("GET /api/sandboxes/{id}/files/{path...}", s.handleFileDownload)
	s.mux.HandleFunc("PUT /api/sandboxes/{id}/files/{path...}", s.handleFileUpload)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/files/{path...}", s.handleFileOp)
	s.mux.HandleFunc("DELETE /api/sandboxes/{id}/files/{path...}", s.handleFileDelete)
	// Owner-blind resume for held leases (2.1): the SSH gateway resumes
	// a rule-1-suspended held lease on attach, where the capability is
	// the lease id/name and no owner id is known.
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	// Readiness (issue #81): for external uptime monitors — every check
	// must pass, not just the orchestrator answering. Auth-exempt like
	// /healthz.
	s.readyz = &readyzState{check: s.svc.runReadyz}
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	// Guest port dial (2.2, #113): a WebSocket carrying raw bytes to a
	// guest TCP port inside the lease, over substrate.DialGuest. Owner or
	// admin; anyone else gets the same 404 as the other lease routes.
	s.mux.HandleFunc("GET /api/sandboxes/{id}/ports/{port}/dial", s.handleGuestDial)
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

// isEventsToken reports whether the request carries the events-only
// EVENTS_TOKEN (constant-time compare). An unset token matches nothing.
func (s *Server) isEventsToken(r *http.Request) bool {
	want := s.svc.cfg.EventsToken
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
	// CollectMetrics also refreshes the kept-checkpoint gauges (#126).
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
// auth path and one route table. The events-only EVENTS_TOKEN is
// admitted even before that rewrite, on exactly GET /api/leases/events:
// its one route, matched on the spelling it is documented and
// contracted on.
func (s *Server) Handler() http.Handler {
	authed := s.authMiddleware(s.mux)
	rewrite := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if eventsPath(r.URL.Path) && s.isEventsToken(r) {
			if !s.allowEventsToken(w, r) {
				return
			}
			// Hand the request to the stream handler on the route the mux
			// knows, carrying the marker its second gate reads. Every other
			// path or spelling carries the token no further than
			// authMiddleware's refusal.
			r.URL.Path = apiSandboxPathPrefix + "/events"
			authed.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxEventsToken{}, true)))
			return
		}
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
				// The files tail is a guest path: it would give the
				// request counters per-file cardinality, so only the
				// route stays.
				if strings.HasPrefix(parts[1], "files/") {
					return "/api/sandboxes/:id/files"
				}
				// The guest dial path carries the guest port; keeping it
				// would give the request counter one series per port.
				if strings.HasPrefix(parts[1], "ports/") && strings.HasSuffix(parts[1], "/dial") {
					return "/api/sandboxes/:id/ports/:port/dial"
				}
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
// consumer id into the request context. /healthz and /readyz are
// exempt (liveness and readiness, issue #81); the /llm/ prefix is
// exempt — the lease id in the path is the capability, and sandboxes
// hold no consumer token.
// /api/admin/ is exempt because ADMIN_TOKEN is not a user/consumer
// token; api/admin.go authenticates those routes itself.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" ||
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
		// The events-only EVENTS_TOKEN was admitted at the top of the
		// chain, on exactly GET /api/leases/events — the marker below is
		// set nowhere else. A request still carrying the token here (the
		// canonical /api/sandboxes spelling, the one-lease streams, any
		// other route at all) never passed that gate and is refused
		// before consumer auth gets to count it.
		if s.isEventsToken(r) {
			if r.Context().Value(ctxEventsToken{}) != true {
				writeError(w, http.StatusUnauthorized, "invalid token")
				return
			}
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
type ctxEventsToken struct{} // set when the caller authenticated with the events-only EVENTS_TOKEN

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

// eventsPath reports whether p is exactly the events-only EVENTS_
// TOKEN's one route: /api/leases/events (GET is allowEventsToken's
// business). The admission runs before the /api/leases rewrite — the
// mux itself only knows the /api/sandboxes spellings — so the route is
// matched literally, on the spelling it is documented under; every
// other path, spelling or depth is refused.
func eventsPath(p string) bool {
	return p == "/api/leases/events"
}

// allowEventsToken admits a request that authenticated with the
// events-only EVENTS_TOKEN on its one route: 405 for any method but GET
// (a stream is a read). Everything else about the route stays the
// stream handler's job. An unset token never gets here — isEventsToken
// matches nothing when EVENTS_TOKEN is empty.
func (s *Server) allowEventsToken(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "events token is read-only")
		return false
	}
	return true
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

// burstRetryAfterSecs is the Retry-After a refused burst carries: the
// reserve frees as guaranteed work suspends, usually well inside a
// minute.
const burstRetryAfterSecs = 30

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
		// CheckpointInterval is the lease's own periodic checkpoint
		// interval in seconds (2.3, #122): 0 = never; omitted (nil) =
		// the host default (CHECKPOINT_INTERVAL_MINS).
		CheckpointInterval *int64 `json:"checkpoint_interval"`
		// IdleSuspend is the lease's own idle reclamation threshold in
		// seconds (2.5, #129 part 2): 0 = never; omitted (nil) = the
		// host default (IDLE_SUSPEND_DEFAULT_SECS). Only persistent
		// leases may set it: suspension needs persistence.
		IdleSuspend *int64 `json:"idle_suspend"`
		// Burst asks for a preemptible lease (#128 part 2): it is
		// classified burst even within the owner's guaranteed_mib, and
		// admitted only while the node keeps its burst reserve free.
		// Priority orders preemption within a class: a lower number is
		// preempted first (0 = the default).
		Burst    bool `json:"burst"`
		Priority *int `json:"priority"`
		// Wait is how many seconds a create refused for a waitable reason
		// (#129 part 1) may wait for admission instead. 0 keeps today's
		// behaviour (answer at once); above 0 it is capped at
		// MAX_ADMIT_WAIT_SECS, and negative is a bad request.
		Wait int `json:"wait"`
		// Secrets (#80) become files under /run/secrets in the guest
		// (mode 0600, on a 0700 tmpfs). Values are kept in memory only,
		// never stored, logged or returned.
		Secrets map[string]string `json:"secrets"`
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
	// Per-lease checkpoint interval (2.3, #122): omitted (nil) is the
	// host default; otherwise 0 (never) or 60..604800 seconds.
	ckptSet := false
	var ckptSecs int64
	if req.CheckpointInterval != nil {
		ckptSet = true
		ckptSecs = *req.CheckpointInterval
		if err := validateCheckpointInterval(ckptSecs); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	// Per-lease idle reclamation (2.5, #129 part 2): omitted (nil) is the
	// host default; otherwise 0 (never) or 60..604800 seconds. Only a
	// persistent lease may set a non-zero value: suspension needs
	// persistence.
	idleSet := false
	var idleSecs int64
	if req.IdleSuspend != nil {
		idleSet = true
		idleSecs = *req.IdleSuspend
		if err := validateIdleSuspend(idleSecs); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if idleSecs != 0 && !req.Persistent {
			writeError(w, http.StatusBadRequest, "idle_suspend needs a persistent lease")
			return
		}
	}
	secrets, err := validateSecrets(req.Secrets)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	priority := 0
	if req.Priority != nil {
		if *req.Priority < -128 || *req.Priority > 127 {
			writeError(w, http.StatusBadRequest, "priority must be between -128 and 127")
			return
		}
		priority = *req.Priority
	}
	if req.Wait < 0 {
		writeError(w, http.StatusBadRequest, "wait must be >= 0")
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
	leaseReq := leaseRequest{
		owner: ownerFrom(r.Context()), image: req.Image, ttl: ttl, persistent: req.Persistent,
		netPolicy: req.NetPolicy, netAllow: req.NetAllow, holder: req.Holder, holderURL: req.HolderURL,
		createSecrets: secrets, exposePorts: expose, burst: req.Burst, priority: priority,
	}
	grantStart := time.Now()
	lease, err := s.svc.grantLease(r.Context(), leaseReq)
	// Queued admission (#129 part 1): a waitable refusal with "wait" set
	// is held in the queue and retried in fair-share order while the
	// request stays open.
	if err != nil && waitRefusal(err) {
		if wait, ok := s.svc.admissionWait(req.Wait); ok {
			t := s.svc.newAdmissionTicket(leaseReq.owner, leaseReq, err, wait)
			s.svc.wakeAdmissionQueue()
			var waited time.Duration
			lease, waited, err = s.svc.waitForAdmission(r.Context(), t)
			if err != nil {
				if r.Context().Err() != nil {
					// The client went away: nothing to write.
					return
				}
				s.writeCreateRefusal(w, req.Image, err, waited)
				return
			}
			s.writeCreatedLease(w, lease, req.Holder, req.HolderURL, req.HoldTTL, ckptSet, ckptSecs, idleSet, idleSecs, ttl, waited)
			return
		}
	}
	if err != nil {
		s.writeCreateRefusal(w, req.Image, err, 0)
		return
	}
	// A create that asked to wait but fit at once still reports how long
	// admission took, so a client can always read waited_ms for a wait
	// request.
	waited := time.Duration(0)
	if req.Wait > 0 {
		waited = time.Since(grantStart)
	}
	s.writeCreatedLease(w, lease, req.Holder, req.HolderURL, req.HoldTTL, ckptSet, ckptSecs, idleSet, idleSecs, ttl, waited)
}

// writeCreateRefusal writes the failure response for a refused create,
// including waited_ms when a wait timed out. It carries the same status,
// body and Retry-After as the immediate refusal.
func (s *Server) writeCreateRefusal(w http.ResponseWriter, image string, err error, waited time.Duration) {
	status := http.StatusInternalServerError
	msg := "failed to grant lease"
	retryAfter := 0
	switch {
	case errors.Is(err, errQuotaExceeded):
		status, msg = http.StatusTooManyRequests, err.Error()
	case errors.Is(err, errUnknownImage):
		status, msg = http.StatusNotFound, "unknown image tag: "+image
	case errors.Is(err, errPreemptCannot):
		// A guaranteed lease that could not preempt (#128 part 3): the
		// snapshot disk is too full to pause a burst lease.
		status, msg, retryAfter = http.StatusServiceUnavailable, "capacity: "+err.Error(), burstRetryAfterSecs
	case errors.Is(err, errBurstReserve):
		// A burst lease that would dip the node under its reserve
		// (#128 part 2): 503 with a retry hint, not a generic capacity
		// error.
		status, msg, retryAfter = http.StatusServiceUnavailable, err.Error(), burstRetryAfterSecs
	case errors.Is(err, errDraining):
		status, msg = http.StatusServiceUnavailable, "draining"
	case errors.Is(err, substrate.ErrCapacity):
		status, msg = http.StatusServiceUnavailable, "capacity: "+err.Error()
	default:
		s.svc.log.Printf("create: grant %s: %v", image, err)
	}
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	body := map[string]any{"error": msg}
	if waited > 0 {
		body["waited_ms"] = waited.Milliseconds()
	}
	writeJSON(w, status, body)
}

// writeCreatedLease writes the 201 for a granted create (both the
// immediate and the queued path): it stamps the hold and the request's
// checkpoint interval and idle_suspend, then the usual body plus waited_ms when the
// create waited for admission.
func (s *Server) writeCreatedLease(w http.ResponseWriter, lease *Lease, holder, holderURL string, holdTTL int, ckptSet bool, ckptSecs int64, idleSet bool, idleSecs int64, ttl, waited time.Duration) {
	s.svc.store.mu.Lock()
	if holder != "" {
		s.svc.setHoldLocked(lease, holder, holderURL, time.Duration(holdTTL)*time.Second, s.svc.now())
	}
	if ckptSet {
		lease.CheckpointInterval = ckptSecs
	}
	if idleSet {
		lease.IdleSuspend = idleSecs
	}
	s.svc.saveLeaseLocked(lease)
	s.svc.store.mu.Unlock()
	body := map[string]any{
		"id":                  lease.ID,
		"owner":               lease.Owner,
		"address":             lease.HostIP,
		"image":               lease.Image,
		"ttl":                 int(ttl.Seconds()),
		"persistent":          lease.Persistent,
		"expires_at":          lease.ExpiresAt.UTC().Format(time.RFC3339),
		"holder":              lease.Holder,
		"holder_url":          lease.HolderUrl,
		"hold_expires_at":     formatRFC3339(lease.HoldExpiresAt),
		"hold_state":          holdState(lease),
		"exposed":             exposedMap(lease),
		"generation":          lease.Generation,
		"checkpoint_interval": s.svc.effectiveCheckpointInterval(lease),
		"idle_suspend":        s.svc.effectiveIdleSuspend(lease),
		"class":               leaseClassRow(lease),
		"priority":            lease.Priority,
	}
	if waited > 0 {
		body["waited_ms"] = waited.Milliseconds()
	}
	writeJSON(w, http.StatusCreated, body)
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
	// A suspended lease has no running sandbox. One that idle_suspend
	// suspended resumes first (2.5, #129 part 2); any other suspension
	// keeps the 409.
	if !s.ensureRunning(w, r, lease) {
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

// handleRestart restarts a lease. mode=warm (the default) keeps the
// guest: a persistent one is paused and resumed, a plain one gets a
// fresh guest. mode=cold gives any lease a fresh guest from the image's
// current build, keeping the lease id (#120). The mode comes from
// ?mode= or the JSON body, body winning when both are set.
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	mode := r.URL.Query().Get("mode")
	if r.Body != nil {
		var req struct {
			Mode string `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil && req.Mode != "" {
			mode = req.Mode
		}
	}
	lease, err := s.svc.restart(r.Context(), owner, id, mode)
	if err != nil {
		switch {
		case errors.Is(err, errNotFound):
			writeError(w, http.StatusNotFound, "lease not found")
		case errors.Is(err, errNotPersistent):
			writeError(w, http.StatusBadRequest, "lease is not a persistent lease")
		case errors.Is(err, errLeaseBusy):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, errBadRestartMode):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, errQuotaExceeded):
			// Restarting a suspended lease brings its guest (and its
			// hugepages) back, so it re-passes the memory check (#128):
			// over max_mib answers 429 and the lease stays suspended.
			writeError(w, http.StatusTooManyRequests, err.Error())
		case errors.Is(err, errPreemptCannot):
			// A guaranteed lease that could not preempt (#128 part 3):
			// the snapshot disk is too full to pause a burst lease.
			writeErrorAfter(w, http.StatusServiceUnavailable, burstRetryAfterSecs, "capacity: "+err.Error())
		case errors.Is(err, errBurstReserve):
			// Restart re-admits a suspended lease like a resume, so a
			// burst lease restarting into a full reserve answers 503
			// with a retry hint too (#128 part 2); the lease stays
			// suspended.
			writeErrorAfter(w, http.StatusServiceUnavailable, burstRetryAfterSecs, err.Error())
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
// at its TTL and not idle-suspended (periodic checkpoints follow its
// checkpoint_interval); its hold expires on its own and the automatic
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
		switch {
		case errors.Is(err, errNotFound):
			writeError(w, http.StatusNotFound, "lease not found")
		default:
			s.writeResumeRefusal(w, id, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      lease.ID,
		"status":  "running",
		"address": lease.HostIP,
	})
}

// handleQueue lists the caller's creates waiting for admission (#129),
// each with its position in the whole fair-share queue; an admin sees
// every owner's.
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	if isAdmin(r) {
		owner = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{"queued": s.svc.queuedCreates(owner)})
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
	// A suspended workspace-backed lease has no running sandbox. One that
	// idle_suspend suspended resumes first through the normal resume path
	// (2.5, #129 part 2); any other suspension keeps the 409.
	if !s.ensureRunning(w, r, lease) {
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
		// Secrets (#80) are staged as /run/secrets/<name> files for this
		// command only and removed afterwards. Values are kept in memory
		// only, never stored, logged or returned.
		Secrets map[string]string `json:"secrets"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Cmd == "" {
		writeError(w, http.StatusBadRequest, "cmd is required")
		return
	}
	execSecrets, err := validateSecrets(req.Secrets)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
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
	// Stage this request's secrets (#80) before the command runs. The
	// cleanup below runs on every exit path — including a half-failed
	// staging — so nothing exec-time outlives the request.
	defer func() {
		if len(execSecrets) > 0 {
			// The command is done: its secrets go. A name that shadows a
			// create-time secret gets the lease's value re-written, so the
			// shadowed file outlives the command like every other
			// create-time secret.
			s.svc.removeSecrets(lease.SandboxID, sortedSecretNames(execSecrets))
			if create := s.svc.createSecretsFor(lease.ID); len(create) > 0 {
				var shadowed []string
				for name := range execSecrets {
					if _, ok := create[name]; ok {
						shadowed = append(shadowed, name)
					}
				}
				if len(shadowed) > 0 {
					s.svc.restageSecrets(lease.SandboxID, shadowed, create)
				}
			}
		}
	}()
	if len(execSecrets) > 0 {
		if err := s.svc.stageSecrets(r.Context(), lease.SandboxID, execSecrets); err != nil {
			// The error names a secret file name at most, never a value.
			s.svc.log.Printf("exec: stage secrets %s: %v", lease.SandboxID, err)
			writeError(w, http.StatusInternalServerError, "failed to stage secrets")
			return
		}
	}
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
			s.writeSandboxGone(w, lease)
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

// writeSandboxGone answers a substrate not-found for a lease the store
// still holds. While a lifecycle operation is in flight on the lease (the
// periodic checkpoint, a suspend, a restart) the orchestrator reports the
// sandbox missing for the length of the snapshot, which can be minutes
// for a large guest: that is 409 with Retry-After, not 410. Clients treat
// 410 as the lease being gone for good and abandon its work. Only a
// lease with nothing in flight gets 410.
func (s *Server) writeSandboxGone(w http.ResponseWriter, l *Lease) {
	s.svc.store.mu.Lock()
	busy := l.busy
	s.svc.store.mu.Unlock()
	if busy {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusConflict, "lease is busy (a checkpoint or another lifecycle operation is in progress); retry shortly")
		return
	}
	writeError(w, http.StatusGone, "lease no longer exists")
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
			s.writeSandboxGone(w, lease)
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
	s.svc.releaseBecause(r.Context(), lease, "deleted through the API")
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
		case errors.Is(err, errPreemptCannot):
			// A guaranteed lease that could not preempt (#128 part 3):
			// the snapshot disk is too full to pause a burst lease.
			writeErrorAfter(w, http.StatusServiceUnavailable, burstRetryAfterSecs, "capacity: "+err.Error())
		case errors.Is(err, errBurstReserve):
			writeErrorAfter(w, http.StatusServiceUnavailable, burstRetryAfterSecs, err.Error())
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
		case errors.Is(err, errPreemptCannot):
			// A guaranteed lease that could not preempt (#128 part 3):
			// the snapshot disk is too full to pause a burst lease.
			writeErrorAfter(w, http.StatusServiceUnavailable, burstRetryAfterSecs, "capacity: "+err.Error())
		case errors.Is(err, errBurstReserve):
			writeErrorAfter(w, http.StatusServiceUnavailable, burstRetryAfterSecs, err.Error())
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
	writeJSON(w, http.StatusOK, s.svc.leaseDetailMap(lease))
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

// writeErrorAfter is writeError with a Retry-After header (seconds):
// the shape a refused burst is answered with (#128 part 2).
func writeErrorAfter(w http.ResponseWriter, status int, retryAfter int, msg string) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	writeError(w, status, msg)
}

// tailStr returns the last n bytes of s, prefixed with a truncation marker
// if s was longer than n. Used for logging stderr tails on exec failures.
func tailStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "...[truncated]..." + s[len(s)-n:]
}
