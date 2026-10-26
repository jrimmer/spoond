package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/common/expfmt"

	"github.com/jrimmer/spoond/v4/identity"
	"github.com/jrimmer/spoond/v4/metrics"
	"github.com/jrimmer/spoond/v4/substrate"
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
	// startedAt is when this server was built, answered as started_at by
	// GET /api/admin/config (#137) so a caller can tell whether the
	// backend restarted since the unit env changed.
	startedAt time.Time
	// fileSecrets (#137 C2): the secrets this process resolved from a
	// <NAME>_FILE value file rather than the environment. The backend
	// sets nothing in its own env, so GET /api/admin/config would report
	// those names unset and `spoond config show` would cry drift where
	// the value matches; the registry reports them set. Guarded by
	// fileSecretsMu; written once at startup before the listeners open.
	fileSecretsMu sync.Mutex
	fileSecrets   map[string]string
	// busy (security review #37 rescan F9) caps concurrent exec/stream
	// operations per owner so a tenant can't saturate the controller
	// with in-flight activity (quota covers lease count, not activity).
	// busyMax is the HOST default (EXEC_CONCURRENCY_MAX); a user with a
	// per-identity override (identity.User.ExecConcurrency) counts
	// against that instead, resolved per request in acquireBusy.
	busyMu    sync.Mutex
	busyCount map[string]int
	busyMax   int

	// fileXfer bounds concurrent file-content transfers backend-wide:
	// each buffers up to maxFileBytes in memory.
	fileXfer chan struct{}

	// dials (2.2, #113) caps concurrent guest port dials per owner, in
	// its own pool: dialing a guest port is not exec/stream activity and
	// the two caps must not eat each other's slots. dialMax is the HOST
	// default (GUEST_DIAL_MAX); a user with a per-identity override
	// (identity.User.GuestDial) counts against that instead, resolved
	// per request in acquireDial.
	dialMu    sync.Mutex
	dialCount map[string]int
	dialMax   int
}

// acquireBusy reserves an exec/stream slot for owner. Returns false when
// the per-owner cap is already reached (caller should 429). The cap is
// the owner's identity override (identity.User.ExecConcurrency) when one
// is set, the host default (busyMax, EXEC_CONCURRENCY_MAX) otherwise.
// The limit is read before busyMu is taken, so a request racing an
// override change is judged by the limit in force when it arrived.
func (s *Server) acquireBusy(owner string) bool {
	limit := s.busyMax
	if s.svc != nil && s.svc.identities != nil {
		if u := s.svc.identities.UserByID(owner); u != nil && u.ExecConcurrency > 0 {
			limit = u.ExecConcurrency
		}
	}
	s.busyMu.Lock()
	defer s.busyMu.Unlock()
	if s.busyCount[owner] >= limit {
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

// SetFileSecrets records the secrets the process resolved from a
// <NAME>_FILE value file (#137 C2), so GET /api/admin/config reports
// those names set — with the value still hidden ("(hidden, from file)")
// — although nothing in this process's environment names the secret.
// names maps the setting name (e.g. ADMIN_TOKEN) to its file's value;
// only the set names are recorded, and the values are dropped here:
// the endpoint needs one boolean, never the content.
func (s *Server) SetFileSecrets(names map[string]string) {
	s.fileSecretsMu.Lock()
	defer s.fileSecretsMu.Unlock()
	s.fileSecrets = make(map[string]string, len(names))
	for name := range names {
		s.fileSecrets[name] = ""
	}
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
		busyCount: map[string]int{}, busyMax: hostExecConcurrencyMax(), fileXfer: make(chan struct{}, maxFileTransfers),
		dialCount: map[string]int{}, dialMax: hostGuestDialMax(), metrics: metrics.NewBackendMetrics(),
		startedAt: time.Now()}
	// The guest heartbeat lives only on the guest-service listener
	// (ProxyHandler); the lease id in the path is the capability.
	s.heartbeat = newLeaseHeartbeat(svc)
	s.heartbeat.metrics = s.metrics
	if openRouterURL != "" {
		// svc.identities must be installed (SetIdentities) before
		// NewServerWithLLM for per-user LLM key enforcement (U8/T8).
		s.llm = newLLMGateway(svc.log, svc.lookupAny, svc.resumeForUse, svc.identities, openRouterURL, openRouterKey, defaultModel, modelMap)
		s.llm.selfTokenOK = svc.llmSelfTokenOK
		s.llm.metrics = s.metrics
	}
	s.svc.SetMetrics(s.metrics)
	s.mux.HandleFunc("POST /api/leases", s.handleCreate)
	s.mux.HandleFunc("GET /api/leases", s.handleList)
	s.mux.HandleFunc("GET /api/leases/queue", s.handleQueue)
	s.mux.HandleFunc("GET /api/leases/{id}", s.handleGetSandbox)
	s.mux.HandleFunc("POST /api/leases/{id}/exec", s.handleExec)
	// Background exec jobs (2.6, #135): list, read (with long-poll),
	// stream raw output by range, and signal a job.
	s.mux.HandleFunc("GET /api/leases/{id}/jobs", s.handleJobsList)
	s.mux.HandleFunc("GET /api/leases/{id}/jobs/{job}", s.handleJobGet)
	s.mux.HandleFunc("GET /api/leases/{id}/jobs/{job}/output", s.handleJobOutput)
	s.mux.HandleFunc("POST /api/leases/{id}/jobs/{job}/signal", s.handleJobSignal)
	s.mux.HandleFunc("DELETE /api/leases/{id}/jobs/{job}", s.handleJobDelete)
	s.mux.HandleFunc("DELETE /api/leases/{id}", s.handleDelete)
	s.mux.HandleFunc("POST /api/leases/{id}/keepalive", s.handleKeepAlive)
	s.mux.HandleFunc("POST /api/leases/{id}/suspend", s.handleSuspend)
	s.mux.HandleFunc("POST /api/leases/{id}/resume", s.handleResume)
	s.mux.HandleFunc("POST /api/leases/{id}/restart", s.handleRestart)
	s.mux.HandleFunc("POST /api/leases/{id}/checkpoint", s.handleCheckpoint)
	s.mux.HandleFunc("POST /api/leases/{id}/tag", s.handleTag)
	s.mux.HandleFunc("POST /api/leases/{id}/comment", s.handleComment)
	s.mux.HandleFunc("POST /api/leases/{id}/prompt", s.handlePrompt)
	s.mux.HandleFunc("GET /api/leases/{id}/endpoint", s.handleEndpoint)
	s.mux.HandleFunc("GET /api/leases/{id}/stat", s.handleStat)
	s.mux.HandleFunc("GET /api/leases/{id}/stream", s.handleStream)
	// Live egress policy change (U09).
	s.mux.HandleFunc("POST /api/leases/{id}/network", s.handleNetwork)
	s.mux.HandleFunc("POST /api/leases/{id}/clone", s.handleClone)
	s.mux.HandleFunc("POST /api/leases/{id}/fork", s.handleFork)
	// Sharing (T6/#33). The fair-share admin view is a separate
	// route, GET /api/fair-shares (#145 FS1); GET /api/shares keeps
	// listing the caller's lease grants.
	s.mux.HandleFunc("POST /api/leases/{id}/share", s.handleShareGrant)
	s.mux.HandleFunc("DELETE /api/leases/{id}/share/{grantee}", s.handleShareRevoke)
	s.mux.HandleFunc("GET /api/shares", s.handleShareList)
	// Every owner's fair-share slice and usage, admin-only (#145 FS1).
	s.mux.HandleFunc("GET /api/fair-shares", s.handleSharesList)
	s.mux.HandleFunc("GET /api/images", s.handleImages)
	// Self-description (#52): the endpoint table, this host's images,
	// sizes, network policies and surfaces. Any valid user token, like
	// the images route; see api/capabilities.go.
	s.mux.HandleFunc("GET /api/capabilities", s.handleCapabilities)
	s.mux.HandleFunc("GET /api/names/{name}", s.handleByName)
	// Snapshot catalog (U11): list and delete the caller's builds.
	s.mux.HandleFunc("GET /api/snapshots", s.handleSnapshots)
	s.mux.HandleFunc("DELETE /api/snapshots/{build_id}", s.handleSnapshotDelete)
	// Named snapshots (2.7, #83): save a lease as a named snapshot, and
	// list/show/delete/retune the owner's named snapshots. The {name...}
	// wildcard accepts a slash in the name and an @version suffix.
	s.mux.HandleFunc("POST /api/leases/{id}/snapshots", s.handleLeaseSnapshotSave)
	s.mux.HandleFunc("GET /api/named-snapshots", s.handleNamedSnapshotsList)
	s.mux.HandleFunc("GET /api/named-snapshots/{name...}", s.handleNamedSnapshotShow)
	s.mux.HandleFunc("PUT /api/named-snapshots/{name...}", s.handleNamedSnapshotKeep)
	s.mux.HandleFunc("DELETE /api/named-snapshots/{name...}", s.handleNamedSnapshotDelete)
	// Named-snapshot grants (#141): share a secret-free name with
	// another user, list given/received, revoke.
	s.mux.HandleFunc("POST /api/named-snapshot-grants", s.handleNamedSnapshotGrantCreate)
	s.mux.HandleFunc("GET /api/named-snapshot-grants", s.handleNamedSnapshotGrantList)
	s.mux.HandleFunc("DELETE /api/named-snapshot-grants", s.handleNamedSnapshotGrantDelete)
	// Admin endpoints (U10): drain, undrain and crash reconcile. Auth is
	// done in api/admin.go (ADMIN_TOKEN is not a consumer token, so
	// authMiddleware lets /api/admin/ through).
	s.mux.HandleFunc("POST /api/admin/drain", s.handleAdminDrain)
	s.mux.HandleFunc("POST /api/admin/undrain", s.handleAdminUndrain)
	s.mux.HandleFunc("POST /api/admin/reconcile", s.handleAdminReconcile)
	// Read-only echo of this process's env (#137), what `spoond config
	// show` compares the unit files against.
	s.mux.HandleFunc("GET /api/admin/config", s.handleAdminConfig)
	// FS5 migration window: unpin every lease whose holder label starts
	// with a prefix (pool-spawn held its workers by label).
	s.mux.HandleFunc("POST /api/admin/unpin-by-holder", s.handleAdminUnpinByHolder)
	// Lease event streams (2.2, #115): Server-Sent Events of every lease
	// lifecycle change, the caller's leases (admins see all) or one
	// lease. The events-only EVENTS_TOKEN is admitted before auth, on
	// GET /api/leases/events only (api/server.go).
	s.mux.HandleFunc("GET /api/leases/events", s.handleLeaseEvents)
	s.mux.HandleFunc("GET /api/leases/{id}/events", s.handleLeaseEventsOne)
	// Held leases (2.1): since FS5 (2026-10-08) the holder and holder_url
	// labels have no lifecycle effect; only a pin does. Setting a holder
	// never pins. Owner or admin; the handler 404s for anyone else, like
	// the other lease routes.
	s.mux.HandleFunc("PUT /api/leases/{id}/holder", s.handleHolder)
	// Pin (FS5): a pinned lease is never deleted before its own expiry
	// and is paused only by the low-memory floor, as its last resort.
	// PUT pins, DELETE unpins; owner or admin, 404 for anyone else.
	s.mux.HandleFunc("PUT /api/leases/{id}/pin", s.handlePin)
	s.mux.HandleFunc("DELETE /api/leases/{id}/pin", s.handleUnpin)
	// Per-lease checkpoint interval (2.3, #122): owner or admin, 404 for
	// anyone else.
	s.mux.HandleFunc("PUT /api/leases/{id}/checkpoint-policy", s.handleCheckpointPolicy)
	// Per-lease idle reclamation threshold (2.5, #129 part 2): owner or
	// admin, 404 for anyone else.
	s.mux.HandleFunc("PUT /api/leases/{id}/idle-policy", s.handleIdlePolicy)
	// Restore in place to a kept checkpoint (2.3, #121): owner or admin,
	// 404 for anyone else and for a build that is not the lease's own.
	s.mux.HandleFunc("POST /api/leases/{id}/restore", s.handleRestore)
	// Crash test for one lease (Honey M3): delete its sandbox as a crash
	// would and run the same per-lease recovery the reconcile pass does.
	// Owner or admin, 404 for anyone else; only when the host sets
	// CRASH_TEST (404 otherwise). See api/crash.go.
	s.mux.HandleFunc("POST /api/leases/{id}/crash-test", s.handleCrashTest)
	// Lease file operations (#114): download/upload/stat/mkdir/remove a
	// guest file through the substrate. Owner or admin; 404 for anyone
	// else, 409 while suspended.
	s.mux.HandleFunc("GET /api/leases/{id}/files/{path...}", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("watch") == "1" {
			s.handleFileWatch(w, r)
			return
		}
		s.handleFileDownload(w, r)
	})
	s.mux.HandleFunc("PUT /api/leases/{id}/files/{path...}", s.handleFileUpload)
	s.mux.HandleFunc("POST /api/leases/{id}/files/{path...}", s.handleFileOp)
	s.mux.HandleFunc("DELETE /api/leases/{id}/files/{path...}", s.handleFileDelete)
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
	s.mux.HandleFunc("GET /api/leases/{id}/ports/{port}/dial", s.handleGuestDial)
	// Identity endpoints (epic #26 T1): user management + key resolution.
	if s.svc.identities != nil {
		s.mux.HandleFunc("GET /api/users", s.handleUsersList)
		s.mux.HandleFunc("GET /api/users/me", s.handleUsersMe)
		// GET /api/users/{id} is the admin per-owner usage view (#145
		// FS1); the literal "me" below wins over this wildcard.
		s.mux.HandleFunc("GET /api/users/{id}", s.handleUserUsage)
		s.mux.HandleFunc("GET /api/users/by-name/{name}", s.handleUsersByName)
		s.mux.HandleFunc("GET /api/users/by-key", s.handleUsersByKey)
		s.mux.HandleFunc("GET /api/identity-status", s.handleIdentityStatus)
		// Named per-user tokens (A2, unified auth #138), GET list. The
		// Go mux refuses to register this beside "by-name/{name}"
		// (neither pattern is more specific), so the GET route is a
		// subtree dispatcher that answers exactly "/api/users/{id}/tokens"
		// and 404s every other tail; see the comment on
		// handleUserSubtreeGet in api/users_tokens.go.
		s.mux.HandleFunc("GET /api/users/{id}/", s.handleUserSubtreeGet)
		s.mux.HandleFunc("POST /api/users", s.handleUsersCreate)
		s.mux.HandleFunc("POST /api/users/{id}/quota", s.handleUsersQuota)
		s.mux.HandleFunc("POST /api/users/{id}/llm-key", s.handleUsersLLMKey) // U8/T8
		s.mux.HandleFunc("POST /api/users/{id}/egress-ceiling", s.handleUsersEgressCeiling)
		s.mux.HandleFunc("POST /api/users/{id}/tokens", s.handleUsersTokensCreate)
		s.mux.HandleFunc("DELETE /api/users/{id}/tokens/{token_id}", s.handleUsersTokenRevoke)
		s.mux.HandleFunc("DELETE /api/users/{id}", s.handleUsersDelete)
	}
	// The caller's own fair-share usage (#145 FS1); available with or
	// without an identity store (a legacy consumer token has an owner).
	s.mux.HandleFunc("GET /api/usage", s.handleUsage)
	// The caller's own 7-day usage history (#140 U4), oldest first.
	s.mux.HandleFunc("GET /api/usage/history", s.handleUsageHistory)
	// The size presets and per-lease size limits (#151).
	s.mux.HandleFunc("GET /api/sizes", s.handleSizes)
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

// hostExecConcurrencyMax is the host's per-owner cap on concurrent
// exec/stream operations (security review #37 rescan F9),
// EXEC_CONCURRENCY_MAX; the built-in default 8 keeps the pre-knob
// behaviour. An owner's identity override, when its user carries one,
// replaces this value for that owner alone.
func hostExecConcurrencyMax() int {
	if v := os.Getenv("EXEC_CONCURRENCY_MAX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 8
}

// hostGuestDialMax is the host's per-owner cap on concurrent open guest
// port dials (2.2, #113), GUEST_DIAL_MAX; the built-in default 16 keeps
// the pre-knob behaviour. An owner's identity override, when its user
// carries one, replaces this value for that owner alone.
func hostGuestDialMax() int {
	if v := os.Getenv("GUEST_DIAL_MAX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 16
}

// SetBusyMax overrides the host's exec/stream cap (EXEC_CONCURRENCY_MAX
// tests and callers who build a Server outside NewServer); 0 or less
// keeps what the server already holds. Production wiring reads the env
// in NewServerWithLLM.
func (s *Server) SetBusyMax(n int) {
	if n > 0 {
		s.busyMax = n
	}
}

// SetDialMax overrides the host's guest-dial cap (GUEST_DIAL_MAX); the
// SetBusyMax rules apply.
func (s *Server) SetDialMax(n int) {
	if n > 0 {
		s.dialMax = n
	}
}

// handleHealthz reports liveness and orchestrator reachability without
// auth, for Gatus/load-balancer checks (U11): 200
// {"status":"ok","orchestrator":"<NodeInfo.Status>","draining":bool}
// when NodeInfo succeeds, 503 {"status":"degraded","orchestrator":
// "unreachable"} when it fails. The draining flag makes an admin drain
// visible to monitors instead of having it look like health.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	body := `{"status":"degraded","orchestrator":"unreachable"}`
	code := http.StatusServiceUnavailable
	if info, err := s.svc.sub.NodeInfo(r.Context()); err == nil {
		code = http.StatusOK
		body = fmt.Sprintf(`{"status":"ok","orchestrator":%q,"draining":%t}`, info.Status, s.svc.draining.Load())
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

// apiLeasePathPrefix is the lease API's one path prefix (4.0): the
// routes register under it and requests to the removed /api/sandboxes
// alias answer 410 naming it.
const apiLeasePathPrefix = "/api/leases"

// apiRemovedAlias is the path alias 4.0 removed. A request to it — the
// bare prefix or anything under it — answers 410 with the same JSON
// error shape as the rest of the API, plus an RFC 8594 Link header
// naming the same route spelled on /api/leases.
const apiRemovedAlias = "/api/sandboxes"

// removedAliasGone answers a request on the removed /api/sandboxes
// alias (4.0): 410 Gone, writeErrorCode's JSON error shape, and the
// Link successor naming the same route under /api/leases. It runs
// before auth, so even a misconfigured client learns where the API
// moved; the answer is otherwise identical for every caller.
func removedAliasGone(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Link", `<`+aliasSuccessorPath(r.URL.Path)+`>; rel="successor-version"`)
	writeErrorCode(w, http.StatusGone, "path_removed", apiRemovedAlias+" is removed in 4.0; use /api/leases")
}

// aliasSuccessorPath maps a removed /api/sandboxes path onto its
// /api/leases twin for the successor Link. Only whole segments match:
// /api/sandboxesXY is not the alias and keeps itself (the mux then 404s
// it, as for any unknown path). The result is percent-escaped: it goes
// into a header before auth, and a raw `>` from the request path would
// close the <…> and let the caller add Link entries of their own.
func aliasSuccessorPath(p string) string {
	if !isAPIRemovedAliasPath(p) {
		return apiLeasePathPrefix
	}
	return (&url.URL{Path: apiLeasePathPrefix + strings.TrimPrefix(p, apiRemovedAlias)}).EscapedPath()
}

// isAPIRemovedAliasPath reports whether p is on the removed
// /api/sandboxes lease API (a whole-segment match, so /api/sandboxesXY
// is not one).
func isAPIRemovedAliasPath(p string) bool {
	return p == apiRemovedAlias || strings.HasPrefix(p, apiRemovedAlias+"/")
}

// Handler returns the HTTP handler with auth + metrics middleware
// applied. The removed-alias gate sits at the top of the chain —
// before auth and the mux — so a request to the removed /api/sandboxes
// alias answers 410 (with the successor Link) no matter what it
// carries. The events-only EVENTS_TOKEN is admitted before auth, on
// exactly GET /api/leases/events: its one route, matched on the
// spelling it is documented and contracted on.
func (s *Server) Handler() http.Handler {
	authed := s.authMiddleware(s.mux)
	gate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The /api/sandboxes alias is gone in 4.0: 410 with the
		// successor Link, before anything else looks at the request.
		if isAPIRemovedAliasPath(r.URL.Path) {
			removedAliasGone(w, r)
			return
		}
		if eventsPath(r.URL.Path) && s.isEventsToken(r) {
			if !s.allowEventsToken(w, r) {
				return
			}
			// Hand the request to the stream handler on the route the
			// mux knows, carrying the marker its second gate reads. Every
			// other path or spelling carries the token no further than
			// authMiddleware's refusal.
			r.URL.Path = apiLeasePathPrefix + "/events"
			r = r.WithContext(context.WithValue(r.Context(), ctxEventsToken{}, true))
			authed.ServeHTTP(w, r)
			return
		}
		// Break-glass mint (#138 A3): the env ADMIN_TOKEN authenticates
		// POST /api/users/{id}/tokens alone. The gate is evaluated once
		// here (the handler must see the same verdict the middleware
		// exemption was granted on) and its verdict travels as a marker;
		// a request that never earned it — including ADMIN_TOKEN on any
		// user route but this one — authenticates the ordinary way.
		adminMint := s.IsAdminBearerToken(r) && isAdminTokenMintPath(r.URL.Path) && r.Method == http.MethodPost
		if adminMint {
			r = r.WithContext(context.WithValue(r.Context(), ctxAdminMint{}, true))
		}
		authed.ServeHTTP(w, r)
	})
	return s.metricsMiddleware(gate)
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
		return "/api/leases/:id"
	}
	if strings.HasPrefix(p, "/api/leases/") {
		rest := strings.TrimPrefix(p, "/api/leases/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) > 0 && len(parts[0]) >= 32 {
			if len(parts) > 1 {
				// The files tail is a guest path: it would give the
				// request counters per-file cardinality, so only the
				// route stays.
				if strings.HasPrefix(parts[1], "files/") {
					return "/api/leases/:id/files"
				}
				// The guest dial path carries the guest port; keeping it
				// would give the request counter one series per port.
				if strings.HasPrefix(parts[1], "ports/") && strings.HasSuffix(parts[1], "/dial") {
					return "/api/leases/:id/ports/:port/dial"
				}
				return "/api/leases/:id/" + parts[1]
			}
			return "/api/leases/:id"
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
	// Named-snapshot names are high-cardinality and caller-chosen: the
	// request counters keep the route, not one series per name.
	if strings.HasPrefix(p, "/api/named-snapshots/") {
		return "/api/named-snapshots/:name"
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
		// one-lease streams, any other route at all) never passed that
		// gate and is refused before consumer auth gets to count it.
		if s.isEventsToken(r) {
			if r.Context().Value(ctxEventsToken{}) != true {
				writeError(w, http.StatusUnauthorized, "invalid token")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		// The break-glass token, marked for the mint route by Handler:
		// pass through untouched (no consumer id, no identity user).
		// The mint handler's adminMintExemption re-checks the marker, the
		// path and the method; any other route refuses the token below.
		if r.Context().Value(ctxAdminMint{}) == true {
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
		// A lease-scoped self token (#156) is not a consumer token: it
		// authenticates as the lease's owner and reaches exactly the two
		// routes that may end or keep alive that one lease. Anything else
		// it asks for is refused; an unknown token counts as a bad auth.
		if isSelfTokenBearer(token) {
			lease := s.svc.leaseBySelfToken(token)
			if lease == nil {
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
			if id, ok := selfTokenPathID(r); !ok || id != lease.ID {
				writeErrorCode(w, http.StatusForbidden, "lease_token_scope",
					"this token may only release or keep alive its own lease")
				return
			}
			ctx := context.WithValue(r.Context(), ctxOwnerKey{}, lease.Owner)
			ctx = context.WithValue(ctx, ctxLeaseToken{}, lease.ID)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
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
type ctxAdminMint struct{}   // set when the request was exempted for the break-glass token mint (#138 A3)
type ctxLeaseToken struct{}  // set to the lease id when a lease-scoped self token authenticated the request (#156)

// authFailLimiter throttles repeated failed token auths per client IP
// (security review #37 L5). Tokens are high-entropy so brute force is
// not realistic, but the limiter also bounds /api/users/by-key probing
// and general junk traffic. 5 failures per 30s window → 429.
type authFailLimiter struct {
	mu      sync.Mutex
	fails   map[string][]time.Time // ip -> failure timestamps
	window  time.Duration
	maxHits int
	// lastPrune is when the whole map was last swept of IPs whose
	// failures have all aged out. Without it, the map keeps one entry
	// per failing IP until a success from that IP, even after the IP
	// never comes back (spoond-966 L2).
	lastPrune time.Time
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
	l.pruneLocked(now, cut)
	return len(kept) > l.maxHits
}

// pruneLocked drops every IP whose failures have all aged out of the
// window. At most one sweep runs per window, so a burst of failures
// from many IPs does not turn each hit into a full-map walk.
func (l *authFailLimiter) pruneLocked(now, cut time.Time) {
	if !l.lastPrune.IsZero() && now.Sub(l.lastPrune) < l.window {
		return
	}
	l.lastPrune = now
	for ip, times := range l.fails {
		stale := true
		for _, t := range times {
			if t.After(cut) {
				stale = false
				break
			}
		}
		if stale {
			delete(l.fails, ip)
		}
	}
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
// business). The admission runs before auth — its one route, matched
// literally, on the spelling it is documented under; every other path,
// spelling or depth is refused.
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

// maxExecBodyBytes caps the JSON body of a POST exec and every message
// of a stream WebSocket, whose first frame carries the same
// argv/env/secrets shape and whose later frames carry stdin. One
// authenticated caller must not be able to make the server hold an
// arbitrarily large command or secret set in memory. The cap is
// generous next to the secrets limit (64 KiB total) and leaves room for
// a large cmd and env; the guest itself rejects an over-long argv
// string with E2BIG ("Argument list too long"), so this only bounds the
// host's memory. Overridable via MAX_EXEC_BODY_BYTES.
var maxExecBodyBytes int64 = func() int64 {
	if v := os.Getenv("MAX_EXEC_BODY_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
		// An unusable value silently falls back to the default; the
		// startup warning (logInvalidMaxExecBodyBytes) makes that visible.
	}
	return defaultMaxExecBodyBytes // 8 MiB
}()

// defaultMaxExecBodyBytes is the cap used when MAX_EXEC_BODY_BYTES is
// unset or unusable.
const defaultMaxExecBodyBytes int64 = 8 << 20

// logInvalidMaxExecBodyBytes warns once at startup when
// MAX_EXEC_BODY_BYTES is set to something unusable: the process falls
// back to the default cap silently otherwise, so an operator who meant
// to raise (or lower) it never learns the value was ignored. It logs
// through the package logger first (eligible before a service exists)
// and is otherwise a no-op when the variable is unset or valid.
func logInvalidMaxExecBodyBytes() {
	v := os.Getenv("MAX_EXEC_BODY_BYTES")
	if v == "" {
		return
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
		return
	}
	log.Printf("MAX_EXEC_BODY_BYTES=%q is invalid (need a positive integer); using the %d byte default", v, defaultMaxExecBodyBytes)
}

// init logs an unusable MAX_EXEC_BODY_BYTES once for a process that
// wires the API through NewServer (the backend binary); the alternative
// is a silent fallback to the default.
func init() { logInvalidMaxExecBodyBytes() }

// readExecBody reads a request body under maxExecBodyBytes, answering a
// JSON 413 EFBIG when the cap trips and a JSON 400 EIO for any other read
// error. The caller reads the body before ensureRunning so an oversize
// request cannot resume an idle-suspended lease.
func readExecBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxExecBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErrorCode(w, http.StatusRequestEntityTooLarge,
				"EFBIG", fmt.Sprintf("request body exceeds the %d byte limit", maxExecBodyBytes))
			return nil, false
		}
		writeErrorCode(w, http.StatusBadRequest, "EIO", "invalid JSON body")
		return nil, false
	}
	return body, true
}

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

// capacityRetryAfterSecs is the Retry-After a capacity-refused call
// carries: room frees as leases pause, release and are taken back,
// usually well inside a minute.
const capacityRetryAfterSecs = 30

// drainRetryAfterSecs is the Retry-After on a create refused while the
// node drains for a planned orchestrator restart.
const drainRetryAfterSecs = 30

// handleCreate grants a new sandbox lease.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Image     string `json:"image"`
		TTL       int    `json:"ttl"`        // seconds
		MemoryMiB int    `json:"memory_mib"` // alias of memory_mb
		// Size (#151): a preset name (GET /api/sizes), or memory_mb and
		// vcpu (either alone; the image default fills the other). The
		// image default when none is given. A size with no warm build
		// cold-boots (start "cold").
		Size       string   `json:"size"`
		MemoryMB   int      `json:"memory_mb"`
		VCPU       int      `json:"vcpu"`
		Network    string   `json:"network"`  // ignored
		InitCmd    string   `json:"init_cmd"` // ignored
		Persistent bool     `json:"persistent"`
		NetPolicy  string   `json:"network_policy"`
		NetAllow   []string `json:"egress_allowlist"`
		// Snapshot starts the lease from a named snapshot version instead
		// of the image's current build (2.7, #83): "name" or "name@v".
		// image then may be omitted; when given it must equal the
		// snapshot's image (400).
		Snapshot string `json:"snapshot"`
		// SnapshotOwner (#141) starts from another user's named snapshot
		// (user name or id) through a grant that user gave the caller.
		// Without a grant it answers 404, like an unknown name.
		SnapshotOwner string `json:"snapshot_owner"`
		// ExposePorts publishes guest TCP ports for other sandboxes to
		// reach (each peer's egress policy decides reachability).
		ExposePorts []int `json:"expose_ports"`
		// Holder names what holds the lease (a CI job, a person's
		// scratch work) and HolderURL links to it. Since FS5 these are
		// plain labels with no lifecycle effect; they never pin. Pinned
		// asks spoond never to pause or delete the lease before its own
		// expiry.
		Holder    string `json:"holder"`
		HolderURL string `json:"holder_url"`
		// Comment is the lease's free-text annotation on the create
		// itself (#132): the runner sends its job label
		// ("forgejo job <id> <url>", runner/executor.go) with the create,
		// so the lease and its `created` event name the job from the first
		// moment. The comment route's whole validity applies (the shared
		// validator in api/comment.go: 512 bytes, no control characters;
		// a violation is a bad request).
		Comment string `json:"comment"`
		// Pinned pins the new lease (FS5): spoond never deletes it before
		// its own expiry and pauses it only as the low-memory floor's last
		// resort. A persistent pinned lease stays until the owner
		// releases it.
		Pinned bool `json:"pinned"`
		// CheckpointInterval is the lease's own periodic checkpoint
		// interval in seconds (2.3, #122): 0 = never; omitted (nil) =
		// the host default (CHECKPOINT_INTERVAL_MINS).
		CheckpointInterval *int64 `json:"checkpoint_interval"`
		// IdleSuspend is the lease's own idle reclamation threshold in
		// seconds (2.5, #129 part 2): 0 = never; omitted (nil) = the
		// host default (IDLE_SUSPEND_DEFAULT_SECS). Only persistent
		// leases may set it: suspension needs persistence.
		IdleSuspend *int64 `json:"idle_suspend"`
		// Fair shares (3.0): no burst flag and no priority — every lease
		// admits against the one hugepage pool. The fields are gone from
		// the API (an unknown field in a request body is ignored).
		// Wait is how many seconds a create refused for a waitable reason
		// (#129 part 1) may wait for admission instead. 0 keeps today's
		// behaviour (answer at once); above 0 it is capped at
		// MAX_ADMIT_WAIT_SECS, and negative is a bad request.
		Wait int `json:"wait"`
		// Secrets (#80) become files under /run/secrets in the guest
		// (mode 0600, on a 0700 tmpfs). Values are kept in memory only,
		// never stored, logged or returned.
		Secrets map[string]string `json:"secrets"`
		// SelfToken asks for a lease-scoped credential (#156) that the
		// guest may use to release or keep alive this lease only. It is
		// written into the guest's /run/secrets with the lease id (and
		// the API URL when configured); the token and its hash are never
		// returned.
		SelfToken bool `json:"self_token"`
		// SelfTokenLLM also lets that token reach the LLM gateway in
		// place of the owner's LLM key, for this lease only (#162).
		// Requires self_token; off by default.
		SelfTokenLLM bool `json:"self_token_llm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Image == "" && req.Snapshot == "" {
		writeError(w, http.StatusBadRequest, "image is required")
		return
	}
	if req.SnapshotOwner != "" && req.Snapshot == "" {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", "snapshot_owner needs snapshot")
		return
	}
	if req.SelfTokenLLM && !req.SelfToken {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", "self_token_llm needs self_token")
		return
	}
	// A create from a named snapshot (2.7, #83): resolve the version
	// owner-scoped before anything else. The image is the snapshot's
	// image; an explicit image must match it.
	var start *snapshotStart
	// memory_mib is the old name of memory_mb (#151): either may be
	// sent; both must then agree.
	if req.MemoryMiB != 0 {
		if req.MemoryMB != 0 && req.MemoryMB != req.MemoryMiB {
			writeErrorCode(w, http.StatusBadRequest, "invalid_size", "memory_mb and memory_mib disagree")
			return
		}
		req.MemoryMB = req.MemoryMiB
	}
	sizeVCPU, sizeMemoryMB := 0, 0
	if req.Snapshot != "" {
		owner := ownerFrom(r.Context())
		var serr *namedSnapshotError
		start, serr = s.svc.snapshotStartFromRequest(r.Context(), owner, req.SnapshotOwner, req.Snapshot, req.Image)
		if serr != nil {
			serr.write(w)
			return
		}
		// The start holds its build against delete, retention and GC for
		// the whole create, queued admission included (B1). Release it
		// however the create ends.
		defer s.svc.endStartingBuild(start.row.BuildID)
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
	if s.refuseOutsideCeiling(w, ownerFrom(r.Context()), req.NetPolicy, req.NetAllow) {
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
	// The comment rides the create (#132); the comment route's whole
	// validity applies to it like to any later set — one shared
	// validator (api/comment.go) answers for both routes.
	if err := validateComment(req.Comment); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
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
	if req.Wait < 0 {
		writeError(w, http.StatusBadRequest, "wait must be >= 0")
		return
	}
	if start == nil {
		ok, err := s.reg.Has(r.Context(), req.Image)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "image catalog unavailable")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "unknown image tag: "+req.Image)
			return
		}
		// The lease's size (#151 Z4): a preset or vcpu/memory_mb,
		// checked against the image and the host's limits.
		if req.Size != "" || req.VCPU != 0 || req.MemoryMB != 0 {
			img, err := s.svc.db.GetImage(r.Context(), req.Image)
			if err != nil {
				writeError(w, http.StatusNotFound, "unknown image tag: "+req.Image)
				return
			}
			sizeVCPU, sizeMemoryMB, err = s.requestedSize(r.Context(), img, req.Size, req.VCPU, req.MemoryMB)
			if err != nil {
				writeErrorCode(w, http.StatusBadRequest, "invalid_size", err.Error())
				return
			}
		}
	} else if req.Size != "" || req.VCPU != 0 || (req.MemoryMB != 0 && req.MemoryMB != start.row.MemoryMB) {
		// A named snapshot resumes at the size it was saved at.
		writeErrorCode(w, http.StatusBadRequest, "invalid_size", fmt.Sprintf(
			"a snapshot keeps its own size: %s@%d runs %d MiB", start.row.Name, start.row.Version, start.row.MemoryMB))
		return
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
		comment:       req.Comment,
		pinned:        req.Pinned,
		selfToken:     req.SelfToken,
		selfTokenLLM:  req.SelfTokenLLM,
		createSecrets: secrets, exposePorts: expose,
		snapshot: start,
		vcpu:     sizeVCPU, memoryMB: sizeMemoryMB,
	}
	grantStart := time.Now()
	lease, err := s.svc.grantLease(r.Context(), leaseReq)
	// Queued admission (#129 part 1): a waitable refusal with "wait" set
	// is held in the queue and retried in fair-share order while the
	// request stays open.
	if err != nil && waitRefusal(err) {
		if wait, ok := s.svc.admissionWait(req.Wait); ok {
			t := s.svc.newAdmissionTicket(leaseReq.owner, leaseReq, err, wait)
			if t == nil {
				// The owner was deleted before the ticket parked
				// (spoond-y0jj): answer the refusal at once rather than
				// waiting out a deadline the admission pass would only
				// refuse at the end.
				s.writeCreateRefusal(w, req.Image, req.Snapshot, fmt.Errorf("%w: %w", errOwnerGone, err), 0)
				return
			}
			s.svc.wakeAdmissionQueue()
			var waited time.Duration
			lease, waited, err = s.svc.waitForAdmission(r.Context(), t)
			if err != nil {
				if r.Context().Err() != nil {
					// The client went away: nothing to write.
					return
				}
				s.writeCreateRefusal(w, req.Image, req.Snapshot, err, waited)
				return
			}
			// The queue can admit the ticket in the moment the client
			// disappears (L2): finishTicket lost that race to the client's
			// context, so the granted lease has no runner owner and no job
			// label and would live until its TTL. Release it with a reason
			// and never write to the dead connection.
			if r.Context().Err() != nil {
				s.svc.releaseBecause(context.Background(), lease, "client_gone")
				return
			}
			s.writeCreatedLease(w, r, lease, ckptSet, ckptSecs, idleSet, idleSecs, ttl, waited, req.TTL <= 0)
			return
		}
	}
	if err != nil {
		s.writeCreateRefusal(w, req.Image, req.Snapshot, err, 0)
		return
	}
	// A create that asked to wait but fit at once still reports how long
	// admission took, so a client can always read waited_ms for a wait
	// request.
	waited := time.Duration(0)
	if req.Wait > 0 {
		waited = time.Since(grantStart)
	}
	s.writeCreatedLease(w, r, lease, ckptSet, ckptSecs, idleSet, idleSecs, ttl, waited, req.TTL <= 0)
}

// writeCreateRefusal writes the failure response for a refused create,
// including waited_ms when a wait timed out. It carries the same status,
// body and Retry-After as the immediate refusal. image and snapshot are
// the request's create source, for the log line. A host ENOMEM during the
// substrate create is answered the capacity_wait the substrate's own
// capacity refusals get (spoond-6ayi): subCreate wraps it in
// substrate.ErrCapacity, so it is 503 with a retry hint — and waitable
// through the queue — rather than a 500 a client would not retry.
func (s *Server) writeCreateRefusal(w http.ResponseWriter, image, snapshot string, err error, waited time.Duration) {
	status := http.StatusInternalServerError
	msg := "failed to grant lease"
	retryAfter := 0
	code := ""
	var snapErr *snapshotStartError
	var boxErr *memBoxFullError
	switch {
	case errors.As(err, &snapErr):
		// A lease that cannot start from a named snapshot on this host
		// (2.7, #83): 409 with a machine-readable code and no retry
		// loop.
		status, msg, code = http.StatusConflict, err.Error(), "cannot_start"
	case errors.Is(err, errOwnerGone):
		// The owner's identity was removed while the create was in
		// flight (spoond-q4j): the user is gone, so the create is refused
		// rather than granted ownerless and uncapped. Checked before the
		// quota and capacity refusals because cancelQueuedForOwner wraps
		// the refusal that queued the ticket, which would otherwise match
		// (and answer) as if the owner were still there (N1).
		status, msg = http.StatusForbidden, "owner deleted"
	case errors.Is(err, errUnknownImage):
		status, msg = http.StatusNotFound, "unknown image tag: "+image
	case errors.As(err, &boxErr):
		// The requester is itself the biggest borrower (FS2b): 429
		// box_full whose body lists its own running leases (own_leases,
		// LRU first), so it can see what holds its memory and pause one
		// itself. Nothing was paused or released. A memory refusal sets
		// no Retry-After, so a waited create's waited_ms rides inside
		// the same body.
		writeBoxFull(w, err, waited)
		return
	case isBoxFull(err):
		// Take-back could not make room: every candidate was pinned
		// (FS5). 429 box_full: nothing was paused or released.
		status, msg, code = http.StatusTooManyRequests, err.Error(), "box_full"
	case errors.Is(err, errDraining):
		// A planned orchestrator restart drains the node for a minute or
		// three; tell clients when to come back instead of letting them
		// guess.
		status, msg, retryAfter = http.StatusServiceUnavailable, "draining", drainRetryAfterSecs
	case errors.Is(err, substrate.ErrCapacity):
		// Hugepages full and preemption could not make room, the node
		// is not healthy, or the host itself ran out of memory while the
		// snapshot load or the uffdio copies ran (host ENOMEM,
		// spoond-6ayi): 503 capacity_wait with a retry hint, so a client
		// that did not send "wait" still knows when to come back.
		status, msg, retryAfter = http.StatusServiceUnavailable, "capacity: "+err.Error(), capacityRetryAfterSecs
		code = "capacity_wait"
	default:
		if snapshot != "" {
			s.svc.log.Printf("create: grant snapshot %s: %v", snapshot, err)
		} else {
			s.svc.log.Printf("create: grant %s: %v", image, err)
		}
	}
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	body := map[string]any{"error": msg}
	if code != "" {
		body["code"] = code
	}
	if waited > 0 {
		body["waited_ms"] = waited.Milliseconds()
	}
	writeJSON(w, status, body)
}

// writeCreatedLease writes the 201 for a granted create (both the
// immediate and the queued path): it stamps the request's checkpoint
// interval and idle_suspend, then the usual body plus waited_ms when the
// create waited for admission.
func (s *Server) writeCreatedLease(w http.ResponseWriter, r *http.Request, lease *Lease, ckptSet bool, ckptSecs int64, idleSet bool, idleSecs int64, ttl, waited time.Duration, ttlDefaulted bool) {
	// A non-persistent lease is released at its TTL whatever else it
	// carries. Say so on the create when the caller may not expect it
	// (Honey, 3.0): the TTL was not given, or a pin or holder is set
	// (neither keeps a lease past its TTL since 3.0).
	if !lease.Persistent && (ttlDefaulted || lease.Pinned || lease.Holder != "") {
		w.Header().Set("Warning", fmt.Sprintf(`299 spoond "not persistent: released when its ttl of %d s runs out; send \"persistent\": true to keep it"`, int(ttl.Seconds())))
	}
	s.svc.store.mu.Lock()
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
		"pinned":              lease.Pinned,
		"holder":              lease.Holder,
		"holder_url":          lease.HolderUrl,
		"exposed":             exposedMap(lease),
		"generation":          lease.Generation,
		"checkpoint_interval": s.svc.effectiveCheckpointInterval(lease),
		"idle_suspend":        s.svc.effectiveIdleSuspend(lease),
	}
	// A lease-scoped self token (#156): report that the lease has one;
	// the token and its hash are never returned.
	if lease.SelfToken {
		body["self_token"] = true
	}
	if lease.SelfTokenLLM {
		body["self_token_llm"] = true
	}
	// A lease started from a named snapshot carries what it started from
	// (A3).
	vctx, vcancel := context.WithTimeout(context.WithoutCancel(r.Context()), storeWriteTimeout)
	if view := s.svc.snapshotView(vctx, lease); view != nil {
		body["snapshot"] = view
	}
	vcancel()
	if waited > 0 {
		body["waited_ms"] = waited.Milliseconds()
	}
	// The lease's size and how it started (#151): "warm" resumed a
	// memory snapshot, "cold" booted at a size no warm build has.
	s.svc.store.mu.Lock()
	if lease.VCPU > 0 {
		body["vcpu"] = lease.VCPU
	}
	if lease.MemoryMB > 0 {
		body["memory_mb"] = lease.MemoryMB
	}
	if lease.DiskMB > 0 {
		body["disk_mb"] = lease.DiskMB
	}
	if lease.Start != "" {
		body["start"] = lease.Start
	}
	s.svc.store.mu.Unlock()
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
	if !s.ensureLive(w, lease) {
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

// writeStreamError sends the stream's error frame with a stable code
// beside the message: {"error": msg, "code": code}. The codes are the
// same ones the HTTP refusals carry (the code is read from the same
// table), so a client treats a bad first frame and a failed start the
// same way on both transports.
func writeStreamError(ws *websocket.Conn, code, msg string) {
	line, err := json.Marshal(struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}{msg, code})
	if err != nil {
		return
	}
	_ = ws.WriteMessage(websocket.TextMessage, line)
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
		// The WebSocket upgrade has not happened yet, so this is a plain
		// HTTP answer: the JSON error shape with the code beside the
		// message, like every other refusal (http.Error's text/plain body
		// had no code).
		writeErrorCode(w, http.StatusTooManyRequests, "too_busy", "too many concurrent exec/stream operations; try again shortly")
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
		writeErrorCode(w, http.StatusNotFound, "not_found", "lease not found")
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
	// too (U10). 410 lease_lost carries the reason the lease was lost.
	if !s.ensureLive(w, lease) {
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
	// The first message is the exec request; bound every message to the
	// same size as a POST exec body so a client cannot make the server
	// buffer an unbounded argv/env/secrets frame or stdin chunk.
	ws.SetReadLimit(maxExecBodyBytes)

	// First message: the exec request.
	mt, payload, err := ws.ReadMessage()
	if err != nil {
		return
	}
	if mt != websocket.TextMessage {
		writeStreamError(ws, "EINVAL", "first message must be JSON text")
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
		writeStreamError(ws, "EINVAL", "bad request JSON")
		return
	}
	if len(req.Args) == 0 {
		writeStreamError(ws, "EINVAL", "args required")
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
		// The command could not even be started: the same code a
		// background job's failed start answers with, on the frame the
		// stream's protocol already had.
		writeStreamError(ws, "spawn_error", "agent unreachable: "+err.Error())
		return
	}
	s.svc.log.Printf("stream: %s: start", lease.ID)
	// An open stream is open work for the periodic checkpoint, which
	// would otherwise pause the guest and end this connection
	// (spoond-g0ok).
	s.svc.incOpenWork(lease.ID)
	defer s.svc.decOpenWork(lease.ID)

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
				// A process that errored after it started: the same frame
				// shape as every error on this route, so a client can read
				// all of one route's errors the same way. There is no
				// refusal-style code for an in-flight failure (the codes the
				// start frames carry name refusals), so the code is the
				// transport class.
				line, err := json.Marshal(struct {
					Error string `json:"error"`
					Code  string `json:"code"`
				}{ev.Err, "EIO"})
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
	// A suspended lease has no running sandbox to update, so it resumes
	// on this work call (#145 D2) through the normal resume path. A
	// refused resume answers capacity_wait/box_full/lease_busy.
	lease := s.svc.lookup(owner, id)
	if lease == nil {
		writeError(w, http.StatusNotFound, "lease not found")
		return
	}
	// The ceiling is checked before the resume: a refused change must not
	// spend memory waking the lease (#164).
	if s.refuseOutsideCeiling(w, owner, req.NetPolicy, req.NetAllow) {
		return
	}
	if !s.ensureRunning(w, r, lease) {
		return
	}
	lease, err := s.svc.setNetwork(r.Context(), owner, id, req.NetPolicy, req.NetAllow)
	if err != nil {
		if writeLeaseLostErr(w, err) {
			return
		}
		switch {
		case errors.Is(err, errNotFound):
			writeError(w, http.StatusNotFound, "lease not found")
		case errors.Is(err, errSuspended):
			// A suspend raced the resume: the lease is busy with it now,
			// so answer the retryable lease_busy, never lease_suspended.
			writeErrorCode(w, http.StatusConflict, "lease_busy", "lease is busy; retry")
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
	// Defence in depth for a lease-scoped self token (#156), as in
	// handleDelete.
	if tokenID, ok := leaseTokenID(r.Context()); ok && tokenID != id {
		writeErrorCode(w, http.StatusForbidden, "lease_token_scope",
			"this token may only release or keep alive its own lease")
		return
	}
	var req struct {
		TTL int `json:"ttl"` // seconds; 0 = maxTTL
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	ttl := time.Duration(req.TTL) * time.Second
	lease, err := s.svc.keepAlive(owner, id, ttl)
	if err != nil {
		if writeLeaseLostErr(w, err) {
			return
		}
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
		if writeLeaseLostErr(w, err) {
			return
		}
		switch err {
		case errNotFound:
			writeError(w, http.StatusNotFound, "lease not found")
		case errNotPersistent:
			writeError(w, http.StatusBadRequest, "lease is not a workspace-backed persistent lease")
		case errLeaseBusy:
			writeError(w, http.StatusConflict, err.Error())
		case errLeaseReleased:
			// The lease was released while the pause ran: to the caller it
			// is gone, so 404 like every unknown lease.
			writeError(w, http.StatusNotFound, "lease not found")
		default:
			if isBoxFull(err) {
				// The snapshot write could not fit and take-back had
				// nothing to reclaim (FS3b): the same 429 box_full refusal
				// a create is answered with. The lease stays as it was.
				writeBoxFull(w, err, 0)
				return
			}
			if errors.Is(err, substrate.ErrCapacity) {
				// A structural disk shortage take-back could not close is
				// a wait, never a loss: 503 capacity_wait with a retry
				// hint, the one no-room shape every path shares.
				writeErrorCodeAfter(w, http.StatusServiceUnavailable, capacityRetryAfterSecs, "capacity_wait", "capacity: "+err.Error())
				return
			}
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
		if writeLeaseLostErr(w, err) {
			return
		}
		switch {
		case errors.Is(err, errNotFound):
			writeError(w, http.StatusNotFound, "lease not found")
		case errors.Is(err, errNotPersistent):
			writeError(w, http.StatusBadRequest, "lease is not a persistent lease")
		case errors.Is(err, errLeaseBusy):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, errLeaseReleased):
			writeError(w, http.StatusNotFound, "lease not found")
		case errors.Is(err, errBadRestartMode):
			writeError(w, http.StatusBadRequest, err.Error())
		case isBoxFull(err):
			// Restart re-admits a suspended lease like a resume; a node full
			// of pinned leases refuses it with the create body's 429
			// box_full, not a 500 (spoond-k0uz R3-2). The lease stays as it
			// was.
			writeBoxFull(w, err, 0)
		case errors.Is(err, substrate.ErrCapacity):
			// The restart's re-admission or sandbox start found no room:
			// a wait like every other path, not a failed restart.
			writeErrorCodeAfter(w, http.StatusServiceUnavailable, capacityRetryAfterSecs, "capacity_wait", "capacity: "+err.Error())
		case errors.Is(err, errOwnerGone):
			// The owner's identity was removed while the restart was in
			// flight (spoond-q4j): the user is gone, so the restart is
			// refused rather than run ownerless.
			writeError(w, http.StatusForbidden, "owner deleted")
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
		if writeLeaseLostErr(w, err) {
			return
		}
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
// An empty comment clears it. The create body's `comment` field answers
// to the same validator (api/comment.go), so the two routes never
// disagree on what a comment is.
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
	if err := validateComment(req.Comment); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	lease, err := s.svc.setComment(owner, id, req.Comment)
	if err != nil {
		if writeLeaseLostErr(w, err) {
			return
		}
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

// handleHolder sets or clears a lease's holder label and link. Since FS5
// (owner decision 2026-10-08) these are plain labels: they have no effect
// on the lease's lifecycle and never pin. request
// {"holder":"…","holder_url":"…"}. Both fields empty clears the
// label. Owner or admin; anyone else gets the same 404 as the other
// lease routes (no existence leak).
func (s *Server) handleHolder(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	var req struct {
		Holder    string `json:"holder"`
		HolderURL string `json:"holder_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
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
	// Validation first (400 naming the field).
	if err := validateHolder(req.Holder, req.HolderURL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := s.svc.setHolder(lease.Owner, id, req.Holder, req.HolderURL)
	if err != nil {
		if writeLeaseLostErr(w, err) {
			return
		}
		if err == errNotFound {
			writeError(w, http.StatusNotFound, "lease not found")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         updated.ID,
		"holder":     updated.Holder,
		"holder_url": updated.HolderUrl,
		"ok":         true,
	})
}

// handlePin pins a lease (FS5): PUT /api/leases/{id}/pin. Owner or admin.
func (s *Server) handlePin(w http.ResponseWriter, r *http.Request) {
	s.setPinnedRoute(w, r, true)
}

// handleUnpin unpins a lease (FS5): DELETE /api/leases/{id}/pin. Owner or
// admin.
func (s *Server) handleUnpin(w http.ResponseWriter, r *http.Request) {
	s.setPinnedRoute(w, r, false)
}

// setPinnedRoute is the shared body of the pin and unpin routes: it
// resolves the lease owner-or-admin, 404s otherwise, and writes the
// lease's pin state.
func (s *Server) setPinnedRoute(w http.ResponseWriter, r *http.Request, pinned bool) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	lease := s.svc.lookup(owner, id)
	if lease == nil && isAdmin(r) {
		lease = s.svc.lookupAny(id)
	}
	if lease == nil {
		writeError(w, http.StatusNotFound, "lease not found")
		return
	}
	updated, err := s.svc.setPinned(lease.Owner, id, pinned)
	if err != nil {
		if writeLeaseLostErr(w, err) {
			return
		}
		writeError(w, http.StatusNotFound, "lease not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":     updated.ID,
		"pinned": updated.Pinned,
		"ok":     true,
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
	if !s.ensureLive(w, lease) {
		return
	}
	// A prompt is work: a suspended lease resumes on it (#145 D2).
	if !s.ensureRunning(w, r, lease) {
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
		Args:    buildShellArgs(script, ""),
		Env:     requestEnv(lease, nil),
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
	// A successful agent exec proves the guest is alive (spoond-5ca).
	s.svc.recordRootfsAlive(lease.ID)
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
		writeErrorCode(w, http.StatusTooManyRequests, "too_busy", "too many concurrent exec/stream operations; try again shortly")
		return
	}
	defer s.releaseBusy(owner)
	// Shared leases are executable over the API (T6/#33).
	lease := s.svc.lookupWithShare(owner, id, ShareHTTP)
	if lease == nil {
		writeErrorCode(w, http.StatusNotFound, "not_found", "lease not found")
		return
	}
	// Bound and read the body before the lease is resumed and before
	// touch: an oversize request must be refused with 413 without waking
	// an idle-suspended lease, the server must not hold an unbounded
	// command in memory, and an oversize request is not activity for the
	// idle sweeper (spoond-mrbr NIT).
	body, ok := readExecBody(w, r)
	if !ok {
		return
	}
	// The exec-scoped pin is taken before the resume-on-use below, so a
	// take-back cannot pause the lease between that resume and the pin;
	// it covers secret staging too. Only pin and background are read
	// here: a body that does not decode is answered 400 by the full
	// decode further down, and the defer releases this pin on that path
	// like on every other. A background exec takes no pin (its running
	// job protects the lease).
	var pinPeek struct {
		Pin        bool `json:"pin"`
		Background bool `json:"background"`
	}
	if json.Unmarshal(body, &pinPeek) == nil && pinPeek.Pin && !pinPeek.Background {
		s.svc.incExecPin(lease.ID)
		defer s.svc.decExecPin(lease.ID)
	}
	s.svc.touch(id) // exec is activity for the idle sweeper
	// A suspended workspace-backed lease has no running sandbox. One that
	// idle_suspend suspended resumes first through the normal resume path
	// (2.5, #129 part 2); any other suspension keeps the 409.
	if !s.ensureRunning(w, r, lease) {
		return
	}
	// A lease lost in a substrate crash has nothing to exec into (U10).
	// 410 lease_lost carries the reason and points at DELETE.
	if !s.ensureLive(w, lease) {
		return
	}
	var req struct {
		Cmd     string            `json:"cmd"`
		Cwd     string            `json:"cwd"`
		Env     map[string]string `json:"env"`
		Timeout int               `json:"timeout"`
		// Background (2.6, #135): start the command and answer 202 as soon
		// as it is running, instead of holding the request until it exits.
		// timeout is ignored.
		Background bool `json:"background"`
		// MaxRuntimeSecs (spoond-wb5) shortens the host's JOB_MAX_RUNTIME
		// for this job. It can never lengthen it; 0 or omitted uses the
		// host cap. A negative value is rejected.
		MaxRuntimeSecs int64 `json:"max_runtime_secs"`
		// RetentionSecs (spoond-hwto) shortens the host's
		// JOB_RETENTION_SECS for this job: the record and its guest files
		// are pruned sooner. It can never lengthen the host window; 0 or
		// omitted uses it. A negative value is rejected.
		RetentionSecs int64 `json:"retention_secs"`
		// OutputMaxBytes (spoond-hwto) caps this job's per-stream output:
		// once a guest stdout or stderr file grows past it, the job poller
		// punches the file's front out and keeps the tail. It shortens the
		// host's JOB_OUTPUT_MAX_BYTES (0 = unlimited), never lengthens it;
		// 0 or omitted uses the host value. A negative value is rejected.
		OutputMaxBytes int64 `json:"output_max_bytes"`
		// Secrets (#80) are staged as /run/secrets/<name> files for this
		// command only and removed afterwards. Values are kept in memory
		// only, never stored, logged or returned.
		Secrets map[string]string `json:"secrets"`
		// Pin makes the lease count as pinned, exactly like an explicit
		// pin, for every automatic pause decision (take-back, the
		// low-memory floor, idle suspend) while this synchronous exec is
		// in flight; it ends when the exec returns. A background job is
		// already protected while it runs, so pin adds nothing there.
		// Read (and the pin taken) above, before the resume-on-use.
		Pin bool `json:"pin"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Cmd == "" {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", "cmd is required")
		return
	}
	// max_runtime_secs only applies to a background job, but a negative
	// value is rejected for both, so a client that sends it cannot believe
	// it shortened a synchronous exec's timeout (spoond-wb5 NIT).
	if req.MaxRuntimeSecs < 0 {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", "max_runtime_secs must be non-negative")
		return
	}
	// retention_secs likewise only applies to a background job, but a
	// negative value is rejected for both for the same reason (spoond-hwto).
	if req.RetentionSecs < 0 {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", "retention_secs must be non-negative")
		return
	}
	// output_max_bytes follows its siblings (spoond-hwto): a negative
	// value is refused even on a synchronous exec so a client cannot
	// believe it capped anything.
	if req.OutputMaxBytes < 0 {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", "output_max_bytes must be non-negative")
		return
	}
	execSecrets, err := validateSecrets(req.Secrets)
	if err != nil {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.Background {
		s.handleBackgroundExec(w, r, lease, owner, req.Cmd, req.Cwd, req.Env, execSecrets, req.MaxRuntimeSecs, req.RetentionSecs, req.OutputMaxBytes)
		return
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	if timeout > maxExecTimeout {
		timeout = maxExecTimeout
	}
	args := buildShellArgs(req.Cmd, req.Cwd)
	execEnv := requestEnv(lease, req.Env)
	if len(execSecrets) > 0 {
		// The per-lease secrets gate (2.7, #83 B2): take it before
		// looking at the files, so a save that holds it makes this exec
		// 409 lease_busy rather than letting it stage a secret into the
		// checkpoint. Mark the names staged maximally before writing the
		// files.
		if !s.svc.secretsGate.beginStaging(lease.ID) {
			w.Header().Set("Retry-After", "5")
			writeErrorCode(w, http.StatusConflict, "lease_busy", "a named snapshot save is in progress; retry")
			return
		}
		defer s.svc.secretsGate.endStaging(lease.ID)
		// The lease has had a secret from here on (#141), recorded
		// before the files exist and inside the gate, so a later save
		// sees it.
		s.svc.markHadSecrets(lease)
		// The cleanup runs inside the gate: it is registered after
		// beginStaging, and its re-stage of shadowed create-time secrets
		// takes the gate itself, so it cannot race a save's scrub (R2). The
		// cleanup runs on every exit path — including a half-failed
		// staging — so nothing exec-time outlives the request.
		defer s.svc.cleanupExecSecrets(lease, execSecrets)
		s.svc.markExecSecretsStaged(lease.ID, sortedSecretNames(execSecrets))
		defer s.svc.unmarkExecSecretsStaged(lease.ID, sortedSecretNames(execSecrets))
		if err := s.svc.stageSecrets(r.Context(), lease.SandboxID, execSecrets); err != nil {
			// The error names a secret file name at most, never a value.
			s.svc.log.Printf("exec: stage secrets %s: %v", lease.SandboxID, err)
			writeErrorCode(w, http.StatusInternalServerError, "internal", "failed to stage secrets")
			return
		}
	}
	start := time.Now()
	// A synchronous exec in flight is open work for the periodic
	// checkpoint: a pause fails it, unlike a background job, which the
	// pause merely stops (spoond-g0ok).
	s.svc.incOpenWork(lease.ID)
	defer s.svc.decOpenWork(lease.ID)
	res, err := s.svc.sub.Exec(r.Context(), lease.SandboxID, substrate.ExecRequest{
		Args:    args,
		Env:     execEnv,
		Timeout: time.Duration(timeout) * time.Second,
	})
	if err != nil {
		s.svc.log.Printf("exec: %s: %v (dur=%s)", lease.SandboxID, err, time.Since(start))
		// Map substrate ErrNotFound (sandbox gone from the orchestrator
		// but the lease still exists in our store) to 410 Gone so the
		// caller can distinguish a permanently dead sandbox from a
		// transient exec failure (e.g. node overload, network blip). A
		// substrate that could not confirm the sandbox's state (the
		// orchestrator List failed) is 503 substrate_unavailable instead:
		// 410 is final for clients, so an orchestrator stall must never
		// become one.
		if s.writeSandboxOpErrorFor(w, lease, "exec", err) {
			return
		}
		// Any other failure is the exec's transport: the agent was
		// unreachable, refused the start, or its stream broke mid-run.
		// The substrate cannot tell those apart here, so the code is
		// EIO, not spawn_error (which promises the command never ran).
		writeErrorCode(w, http.StatusInternalServerError, "EIO", "exec failed")
		return
	}
	// A successful exec proves the guest is alive this instant; the
	// rootfs liveness probe skips the lease while this stays recent
	// (spoond-5ca). A non-zero exit is not a success and must not mask a
	// dead root disk that fails every command with EIO.
	s.svc.log.Printf("exec: %s: exit=%d stdout=%d stderr=%d dur=%s", lease.SandboxID, res.ExitCode, len(res.Stdout), len(res.Stderr), time.Since(start))
	if res.ExitCode != 0 {
		s.svc.log.Printf("exec: %s: stderr=%q", lease.SandboxID, tailStr(res.Stderr, 500))
	} else {
		s.svc.recordRootfsAlive(lease.ID)
	}
	resp := map[string]any{
		"stdout": res.Stdout,
		"stderr": res.Stderr,
		"exit":   res.ExitCode,
	}
	// A command the substrate ended at its time limit (TimedOut: the
	// e2b backend's kill at the exec's own timer, exit 124 and the
	// "[spoond] exec timed out" note) carries code timeout, so a client
	// learns the cap, not the command, ended it without matching the
	// note's text or the 124 (a command may produce both itself). The
	// key is present only then.
	if res.TimedOut {
		resp["code"] = "timeout"
	}
	writeJSON(w, http.StatusOK, resp)
}

// writeSandboxGone answers a substrate not-found for a lease the store
// still holds. While a lifecycle operation is in flight on the lease (the
// periodic checkpoint, a suspend, a restart) the orchestrator reports the
// sandbox missing for the length of the snapshot, which can be minutes
// for a large guest: that is 409 lease_busy with Retry-After, not 410.
// Clients treat 410 as the lease being gone for good and abandon its
// work. Only a lease with nothing in flight gets 410 lease_lost.
func (s *Server) writeSandboxGone(w http.ResponseWriter, l *Lease) {
	s.svc.store.mu.Lock()
	busy := l.busy
	s.svc.store.mu.Unlock()
	if busy {
		w.Header().Set("Retry-After", "5")
		writeErrorCode(w, http.StatusConflict, "lease_busy", "lease is busy (a checkpoint or another lifecycle operation is in progress); retry shortly")
		return
	}
	writeErrorCode(w, http.StatusGone, "lease_lost", "lease no longer exists")
}

// substrateUnknownStatusCode is the status a substrate operation answers
// when the orchestrator could not be reached to confirm whether the
// sandbox exists. It is retryable: 410 lease_lost is final for clients,
// so an orchestrator stall must never be reported as one.
const substrateUnknownStatusCode = http.StatusServiceUnavailable

// substrateUnknownRetryAfterSecs is the Retry-After a substrate-unknown
// refusal carries: short, because the orchestrator usually recovers.
const substrateUnknownRetryAfterSecs = 5

// writeSubstrateUnavailable answers a substrate operation that could not
// confirm the sandbox's state (the orchestrator List failed): 503 with a
// Retry-After and the machine-readable code substrate_unavailable. The
// lease is never marked lost on this answer; the caller retries. It
// logs nothing itself: callers that have the lease and the op call
// writeSubstrateUnavailableFor so an orchestrator stall is visible in
// the backend log with the lease id and op.
func (s *Server) writeSubstrateUnavailable(w http.ResponseWriter) {
	writeErrorCodeAfter(w, substrateUnknownStatusCode, substrateUnknownRetryAfterSecs, "substrate_unavailable",
		"the substrate could not confirm the sandbox state (orchestrator unreachable); retry shortly")
}

// writeSubstrateUnavailableFor is writeSubstrateUnavailable plus the one
// log line the substrate-unknown answer carries: lease id, op and
// "substrate unavailable". The client only sees the retryable 503; the
// backend log is where an orchestrator stall is diagnosed.
func (s *Server) writeSubstrateUnavailableFor(w http.ResponseWriter, lease *Lease, op string) {
	s.svc.log.Printf("%s: lease %s sandbox %s: substrate unavailable; answering a retryable 503", op, lease.ID, lease.SandboxID)
	s.writeSubstrateUnavailable(w)
}

// writeSandboxOpError maps a failed substrate operation on a live lease:
// an unavailable decision is 503 substrate_unavailable (retryable, lease
// kept), and a confirmed not-found goes through writeSandboxGone
// (409 busy / 410 lease_lost). It reports whether it wrote a response.
func (s *Server) writeSandboxOpError(w http.ResponseWriter, l *Lease, err error) bool {
	return s.writeSandboxOpErrorFor(w, l, "substrate op", err)
}

// writeSandboxOpErrorFor is writeSandboxOpError with the op named in the
// substrate-unknown log line.
func (s *Server) writeSandboxOpErrorFor(w http.ResponseWriter, l *Lease, op string, err error) bool {
	if errors.Is(err, substrate.ErrUnavailable) {
		s.writeSubstrateUnavailableFor(w, l, op)
		return true
	}
	if errors.Is(err, substrate.ErrNotFound) {
		s.writeSandboxGone(w, l)
		return true
	}
	return false
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
	if !s.ensureLive(w, lease) {
		return
	}
	if lease.Suspended {
		writeLeaseSuspended(w, s.svc.leaseSuspendReason(id))
		return
	}
	const probe = `set -e
echo "== loadavg =="; cat /proc/loadavg
echo "== meminfo =="; grep -E '^(MemTotal|MemAvailable):' /proc/meminfo
echo "== netdev =="; cat /proc/net/dev
echo "== df =="; df -P /
`
	res, err := s.svc.sub.Exec(r.Context(), lease.SandboxID, substrate.ExecRequest{
		Args:    buildShellArgs(probe, ""),
		Env:     requestEnv(lease, nil),
		Timeout: 5 * time.Second,
	})
	if err != nil {
		if s.writeSandboxOpErrorFor(w, lease, "stat", err) {
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
	// A successful probe exec proves the guest is alive (spoond-5ca).
	s.svc.recordRootfsAlive(lease.ID)
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

// handleDelete releases a sandbox owned by the caller. It accepts an
// optional release reason (2.5, #132 part 2): `?reason=` or a JSON body
// {"reason"}, at most 120 printable characters, which the `released`
// event carries; without one the event keeps "deleted through the API".
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	reason, err := releaseReason(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Defence in depth for a lease-scoped self token (#156): the auth
	// middleware already allowed exactly this lease, but the handler
	// re-checks the marker's id against the path.
	if tokenID, ok := leaseTokenID(r.Context()); ok && tokenID != id {
		writeErrorCode(w, http.StatusForbidden, "lease_token_scope",
			"this token may only release or keep alive its own lease")
		return
	}
	lease := s.svc.lookup(owner, id)
	if lease == nil {
		writeError(w, http.StatusNotFound, "lease not found")
		return
	}
	if reason == "" {
		if _, ok := leaseTokenID(r.Context()); ok {
			reason = selfTokenReleaseReason
		} else {
			reason = "deleted through the API"
		}
	}
	s.svc.releaseBecause(r.Context(), lease, reason)
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

	// A copy keeps its source's network policy, so it must fit the
	// owner's egress ceiling as a create would (#164).
	if s.writeCeilingRefusal(w, s.svc.egressOutsideCeilingOf(owner, id)) {
		return
	}
	cloned, buildID, err := s.svc.clone(r.Context(), owner, id)
	if err != nil {
		if writeLeaseLostErr(w, err) {
			return
		}
		switch {
		case errors.Is(err, errNotFound):
			writeError(w, http.StatusNotFound, "lease not found")
		case errors.Is(err, errLeaseBusy):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, errLeaseReleased):
			writeError(w, http.StatusNotFound, "lease not found")
		case isBoxFull(err):
			// The clone's own class admission is box_full when every
			// take-back candidate is pinned (FS5): 429 like create, not a
			// 500 (spoond-k0uz R3-2).
			writeBoxFull(w, err, 0)
		case errors.Is(err, substrate.ErrCapacity):
			writeErrorCodeAfter(w, http.StatusServiceUnavailable, capacityRetryAfterSecs, "capacity_wait", "capacity: "+err.Error())
		case errors.Is(err, errOwnerGone):
			// The owner's identity was removed while the clone was in
			// flight (spoond-q4j): the user is gone, so the clone is
			// refused rather than granted ownerless and uncapped.
			writeError(w, http.StatusForbidden, "owner deleted")
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
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := validateHolder(req.Holder, req.HolderURL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.writeCeilingRefusal(w, s.svc.egressOutsideCeilingOf(owner, id)) {
		return
	}
	leases, buildID, err := s.svc.fork(r.Context(), owner, id, req.Count, req.Persistent, time.Duration(req.TTL)*time.Second, req.Holder, req.HolderURL)
	if err != nil {
		if writeLeaseLostErr(w, err) {
			return
		}
		switch {
		case errors.Is(err, errBadForkCount):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, errNotFound):
			writeError(w, http.StatusNotFound, "lease not found")
		case errors.Is(err, errSuspended):
			s.writeLeaseSuspendedID(w, id)
		case errors.Is(err, errLeaseBusy):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, errLeaseReleased):
			writeError(w, http.StatusNotFound, "lease not found")
		case isBoxFull(err):
			// A fork child's class admission is box_full when every
			// take-back candidate is pinned (FS5): 429 like create, not a
			// 500 (spoond-k0uz R3-2).
			writeBoxFull(w, err, 0)
		case errors.Is(err, substrate.ErrCapacity):
			writeErrorCodeAfter(w, http.StatusServiceUnavailable, capacityRetryAfterSecs, "capacity_wait", "capacity: "+err.Error())
		case errors.Is(err, errOwnerGone):
			// The owner's identity was removed while the fork was in
			// flight (spoond-q4j): the user is gone, so the fork is
			// refused and every child rolled back rather than granted
			// ownerless and uncapped.
			writeError(w, http.StatusForbidden, "owner deleted")
		default:
			s.svc.log.Printf("fork %s: %v", id, err)
			writeError(w, http.StatusInternalServerError, "failed to fork lease")
		}
		return
	}
	ids := make([]string, len(leases))
	for i, l := range leases {
		ids[i] = l.ID
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"source":   id,
		"build_id": buildID,
		"ids":      ids,
	})
}

// handleGetSandbox returns one lease: the same object as an element of
// GET /api/leases plus the lifecycle fields. The owner or an http
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
// catalog rows, each with its effective min_memory_mb and the presets it
// has a warm build for (warm_sizes, #151).
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
			warm, err := s.svc.db.ListImageSizes(r.Context(), img.Name)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "image catalog unavailable")
				return
			}
			warmSizes := []string{}
			for _, ws := range warm {
				warmSizes = append(warmSizes, ws.Size)
			}
			out = append(out, map[string]any{
				"name":          img.Name,
				"build_id":      img.CurrentBuildID,
				"template_id":   img.TemplateID,
				"digest":        img.Digest,
				"vcpu":          img.VCPU,
				"memory_mb":     img.MemoryMB,
				"disk_mb":       img.DiskMB,
				"min_memory_mb": effectiveMinMemoryMB(img),
				"warm_sizes":    warmSizes,
				"updated_at":    img.UpdatedAt.UTC().Format(time.RFC3339),
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

// buildShellArgs wraps a command with its working directory into a
// single shell invocation. The cwd is applied with `cd` so a bad
// directory fails the command exactly as it did before; env travels in
// ExecRequest.Env, never argv, so no value is visible in the guest's
// command line.
func buildShellArgs(cmd, cwd string) []string {
	var parts []string
	if cwd != "" {
		parts = append(parts, "cd "+shellQuote(cwd)+" &&")
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

// writeErrorCode is writeError with a machine-readable code beside the
// message (e.g. capacity_wait, box_full, lease_busy,
// substrate_unavailable).
func writeErrorCode(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

// writeBoxFull writes the 429 box_full refusal every re-admission path
// answers with, including the create path. The whole body is built in
// one place — the message and the code, own_leases when the refusal is
// the memory one for the biggest borrower (memBoxFullError): the
// requester's own running leases, least recently used first, so the
// caller can see what holds its memory and pause one itself (nothing
// new is paused) — and waited_ms when a caller hands it the wait a
// queued create spent, so a client reading waited_ms still sees how
// long the wait took (the key is part of the body, never appended
// bytes: the JSON stays valid by construction). A disk/pinned box_full
// (boxFullError, errBoxFull) has no leases and answers without the
// own_leases key. A memory refusal sets no Retry-After.
func writeBoxFull(w http.ResponseWriter, err error, waited time.Duration) {
	body := map[string]any{"error": err.Error(), "code": "box_full"}
	var bf *memBoxFullError
	if errors.As(err, &bf) && len(bf.Leases) > 0 {
		body["own_leases"] = bf.Leases
	}
	if waited > 0 {
		body["waited_ms"] = waited.Milliseconds()
	}
	writeJSON(w, http.StatusTooManyRequests, body)
}

// writeErrorAfter is writeError with a Retry-After header (seconds):
// the shape a capacity-refused call is answered with.
func writeErrorAfter(w http.ResponseWriter, status int, retryAfter int, msg string) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	writeError(w, status, msg)
}

// writeErrorCodeAfter is writeErrorCode with a Retry-After header: the
// shape a no-room resume refusal carries (#145 D2, capacity_wait) and a
// substrate-unknown refusal carries (substrate_unavailable), so every
// retryable work path answers identically.
func writeErrorCodeAfter(w http.ResponseWriter, status int, retryAfter int, code, msg string) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	writeErrorCode(w, status, code, msg)
}

// tailStr returns the last n bytes of s, prefixed with a truncation marker
// if s was longer than n. Used for logging stderr tails on exec failures.
func tailStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "...[truncated]..." + s[len(s)-n:]
}
