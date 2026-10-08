package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// defaultProxyHostSuffix is the wildcard hostname suffix the HTTP proxy
// routes when no deployment-specific suffix is configured. Operators set
// SPOOND_PROXY_HOST_SUFFIX (ServiceConfig.ProxyHostSuffix) to their own
// wildcard domain.
const defaultProxyHostSuffix = ".sandbox.example.com"

// proxySuffix is the configured wildcard suffix, or the generic default
// when unset.
func (s *Service) proxySuffix() string {
	if s.cfg.ProxyHostSuffix != "" {
		return s.cfg.ProxyHostSuffix
	}
	return defaultProxyHostSuffix
}

// defaultProxyPort is the guest port used when the hostname carries none.
// exe.dev uses the Dockerfile EXPOSE port; we have no Dockerfiles, so the
// convention is port 3000 unless the caller names another via
// <lease-id>-<port>.<suffix>.
const defaultProxyPort = 3000

// envdPort is the guest's management port (envd's HTTP + Connect-RPC
// listener). It is never exposed through spoond's proxy.
const envdPort = 49983

// ProxyHandler returns the HTTP handler for the public proxy listener
// (plain HTTP on an internal port; Caddy fronts it with wildcard TLS).
// Every request's Host header names a lease:
// <lease-id>.<suffix> → guest:3000, <lease-id>-<port>.<suffix> →
// guest:<port>, where <suffix> is SPOOND_PROXY_HOST_SUFFIX. The lease id
// in the hostname is the capability (same model as SSH).
//
// Under forward-auth (U7/T7) the capability model is replaced: the
// proxy requires X-Proxy-Auth == the shared secret and resolves the
// authenticated user from Remote-User, then owner-scopes every lookup.
func (s *Server) ProxyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Decide by Host first (#144): a request whose Host names a
		// lease goes to the guest with its path untouched, so a guest
		// app's own /assets/, /lease/ or /llm/ routes are never shadowed
		// by the host-service routes below. This pre-check uses the same
		// parser as handleProxy.
		if _, _, _, ok := parseProxyHost2(r.Host, s.svc.proxySuffix()); ok {
			// Forward-auth gate (U7/T7): off/"" = capability model.
			if s.proxyAuthMode == "forward-auth" {
				if !s.proxyAuthOK(w, r) {
					return
				}
			}
			s.handleProxy(w, r)
			return
		}

		// Everything below rides the guest-service listener
		// (http://<HOST_GUEST_SERVICE_ADDR>:8891/...), whose Host is not a
		// lease hostname.
		//
		// The LLM gateway also lives on the plain-HTTP proxy listener:
		// guests reach it at http://<HOST_GUEST_SERVICE_ADDR>:8891/llm/<lease-id>/...,
		// avoiding TLS validation of the backend's self-signed cert.
		if s.llm != nil && strings.HasPrefix(r.URL.Path, llmGatewayPrefix) {
			s.llm.ServeHTTP(w, r)
			return
		}
		// The lease heartbeat rides the same guest-service listener:
		// guests reach it at http://<host>:8891/lease/<lease-id>/active,
		// which every egress policy — restricted included — permits.
		if strings.HasPrefix(r.URL.Path, leaseHeartbeatPrefix) {
			s.heartbeat.ServeHTTP(w, r)
			return
		}
		// Static assets (e.g. the shelley agent binary) served to guests
		// at http://<HOST_GUEST_SERVICE_ADDR>:8891/assets/<file>. This is how
		// a lease fetches tooling that is too big for the exec API cmdline.
		if s.assetsDir != "" && strings.HasPrefix(r.URL.Path, "/assets/") {
			// Containment (security review #37 rescan): never rely on the
			// stdlib's incidental dot-dot rejection for a host-filesystem
			// read on an unauthenticated path. Resolve inside assetsDir
			// and refuse anything that escapes it.
			rel := strings.TrimPrefix(r.URL.Path, "/assets/")
			p := filepath.Join(s.assetsDir, filepath.FromSlash(rel))
			if !strings.HasPrefix(p, filepath.Clean(s.assetsDir)+string(filepath.Separator)) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			http.ServeFile(w, r, p)
			return
		}
		// Not a lease hostname and no guest-service route: handleProxy
		// answers the same 404 as before.
		s.handleProxy(w, r)
	})
}

// ctxProxyOwnerKey carries the authenticated proxy owner (user id or
// legacy consumer name) resolved by the forward-auth gate. The inbound
// Remote-User header is never read again after this.
type ctxProxyOwnerKey struct{}

func proxyOwnerFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxProxyOwnerKey{}).(string)
	return v
}

// proxyAuthOK implements the forward-auth gate: shared-secret check
// (constant-time), Remote-User presence, and identity resolution. It
// stashes the resolved owner in the request context.
func (s *Server) proxyAuthOK(w http.ResponseWriter, r *http.Request) bool {
	secret := r.Header.Get("X-Proxy-Auth")
	if secret == "" || subtle.ConstantTimeCompare([]byte(secret), []byte(s.proxyAuthSecret)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	user := strings.TrimSpace(r.Header.Get("Remote-User"))
	if user == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	// Trusted-peer enforcement (security review #37 M3): when peers are
	// configured, only a trusted reverse proxy may present Remote-User.
	// A direct client to the listener can't spoof an identity even with
	// the shared secret.
	if len(s.proxyTrustedPeers) > 0 && !s.peerTrusted(r.RemoteAddr) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	owner := user // legacy single-user: Remote-User is the owner directly
	if s.svc.identities != nil {
		u := s.svc.identities.UserByName(user)
		if u == nil {
			http.Error(w, "unknown user", http.StatusForbidden)
			return false
		}
		owner = u.ID
	}
	*r = *r.WithContext(context.WithValue(r.Context(), ctxProxyOwnerKey{}, owner))
	return true
}

// peerTrusted reports whether a RemoteAddr host is in the trusted-peer
// set (security review #37 M3).
func (s *Server) peerTrusted(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range s.proxyTrustedPeers {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// SetAssetsDir enables static asset serving on the proxy listener.
func (s *Server) SetAssetsDir(dir string) { s.assetsDir = dir }

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	label, hostUser, port, ok := parseProxyHost2(r.Host, s.svc.proxySuffix())
	if !ok {
		http.Error(w, "unknown lease hostname", http.StatusNotFound)
		return
	}
	var lease *Lease
	// Under forward-auth, the authenticated owner scopes every lookup
	// (U7/T7): a user only ever reaches their own leases, by id or
	// friendly name. A per-user hostname segment (<label>.<user>...) must
	// match the authenticated owner. In the capability model (off) the
	// hostname is the credential and owner-blind lookups are used.
	if owner := proxyOwnerFrom(r.Context()); owner != "" {
		if hostUser != "" {
			// Resolve the user segment to an identity id and require
			// it to be the authenticated owner (no cross-user URLs).
			// With no identity store (legacy single-user) the segment
			// must equal the raw Remote-User owner.
			if s.svc.identities == nil {
				if hostUser != owner {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
			} else {
				hu := s.svc.identities.UserByName(hostUser)
				if hu == nil || hu.ID != owner {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
			}
		}
		lease = s.svc.lookupUserScoped(owner, label)
	} else if len(label) == 32 && isHex(label) {
		// Capability model: ONLY the unguessable 32-hex lease id is a
		// credential. Friendly names are NOT capabilities (they're
		// guessable: "web", "demo", "api"...) and resolving them
		// cross-tenant here would expose every tenant's sandbox to
		// anyone on the network (security review #37 rescan F4).
		// Friendly-name routing requires forward-auth, where lookups
		// are owner-scoped.
		lease = s.svc.lookupAny(label)
	} else if s.svc.identities == nil && label != "" {
		// Legacy single-user mode (no identity store): there is only
		// one tenant, so a friendly name carries no cross-tenant
		// exposure. Keep the old behavior for these deployments.
		lease = s.svc.lookupByName(label)
	} else {
		http.Error(w, "unknown lease hostname", http.StatusNotFound)
		return
	}
	if lease == nil {
		http.Error(w, "lease not found", http.StatusNotFound)
		return
	}
	// envd is never exposed through spoond's proxy: guest port 49983 is
	// the sandbox's management surface, reachable only with its traffic
	// token through the orchestrator.
	if port == envdPort {
		http.Error(w, "port 49983 is envd and cannot be proxied", http.StatusForbidden)
		return
	}
	s.svc.touch(lease.ID) // proxied web traffic is activity for the idle sweeper
	// A suspended lease has no running sandbox; resume it first.
	if lease.Suspended {
		writeLeaseSuspended(w)
		return
	}
	// A lease lost in a substrate crash has no sandbox to proxy to (U10).
	// 410 lease_lost carries the reason and points at DELETE.
	if !s.ensureLive(w, lease) {
		return
	}
	// The target is the sandbox's host address; the orchestrator's
	// sandbox proxy routes it into the sandbox. A lease without a host
	// address has no running sandbox.
	if lease.HostIP == "" {
		http.Error(w, "lease not running", http.StatusBadGateway)
		return
	}
	proxyURL, err := url.Parse(s.svc.cfg.ProxyURL)
	if err != nil || proxyURL.Scheme == "" || proxyURL.Host == "" {
		http.Error(w, "proxy misconfigured", http.StatusInternalServerError)
		return
	}

	// Reverse proxy through the orchestrator's sandbox proxy (HTTP only,
	// routed by the E2b-Sandbox-Id/-Port headers below — it routes by
	// headers only when the Host is an IP, hence Out.Host). Both HTTP and
	// WebSocket upgrades work through this.
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// Security review #37 rescan F2: NEVER forward the forward-auth
			// gate headers to the guest app. Guests are tenant-controlled;
			// if X-Proxy-Auth (the shared secret with Caddy) or Remote-User
			// reached them, any tenant could harvest the secret and
			// impersonate anyone through the proxy. Strip all gate/auth
			// headers and every E2b-* routing header FIRST (defense in
			// depth: Caddy strips on ingress too, and :8891 is directly
			// reachable), THEN set this proxy's own.
			for _, h := range []string{"X-Proxy-Auth", "Remote-User", "X-Spoond-User-Id", "X-Bootstrap-Token", "e2b-traffic-access-token"} {
				pr.Out.Header.Del(h)
			}
			for name := range pr.Out.Header {
				if strings.HasPrefix(strings.ToLower(name), "e2b-") {
					pr.Out.Header.Del(name)
				}
			}
			// Scheme and host from the orchestrator proxy; path and query
			// stay as the client sent them.
			pr.SetURL(proxyURL)
			pr.Out.Host = proxyURL.Host
			// Preserve the public hostname for guest apps: they see
			// Host: <proxy host>, so frameworks that validate the Host
			// (e.g. dev servers with host allowlists) must consult
			// X-Forwarded-Host. U12 step 19 records this behaviour
			// change in docs/api.md.
			pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
			proto := "http"
			if pr.In.TLS != nil || strings.EqualFold(pr.In.Header.Get("X-Forwarded-Proto"), "https") {
				proto = "https"
			}
			pr.Out.Header.Set("X-Forwarded-Proto", proto)
			// X-Forwarded-For: append the client address to any inbound
			// chain. ReverseProxy only does this on the Director path;
			// the Rewrite path strips it, so re-add it here.
			if clientIP, _, err := net.SplitHostPort(pr.In.RemoteAddr); err == nil {
				if chain := pr.In.Header.Get("X-Forwarded-For"); chain != "" {
					clientIP = chain + ", " + clientIP
				}
				pr.Out.Header.Set("X-Forwarded-For", clientIP)
			}
			pr.Out.Header.Set("E2b-Sandbox-Id", lease.SandboxID)
			pr.Out.Header.Set("E2b-Sandbox-Port", strconv.Itoa(port))
			pr.Out.Header.Set("e2b-traffic-access-token", s.svc.sub.TrafficToken(lease.SandboxID))
		},
		Transport: &http.Transport{IdleConnTimeout: 30 * time.Second},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return
			}
			http.Error(w, "proxy error: "+err.Error(), http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

// parseProxyHost extracts a lease id and guest port from a proxy Host
// header. Accepted forms (suffix = SPOOND_PROXY_HOST_SUFFIX):
//
//	<32-hex-lease-id>.<suffix>        → port 3000
//	<32-hex-lease-id>-<port>.<suffix> → that port
//
// Returns ok=false for anything else (including the bare apex hostname).
func parseProxyHost(host, suffix string) (leaseID string, port int, ok bool) {
	h := strings.ToLower(strings.TrimSpace(host))
	// Strip any explicit :port from the Host header (rare on 443, cheap).
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.HasSuffix(h, "]") {
		if _, err := strconv.Atoi(h[i+1:]); err == nil {
			h = h[:i]
		}
	}
	if !strings.HasSuffix(h, suffix) {
		return "", 0, false
	}
	label := strings.TrimSuffix(h, suffix)
	if label == "" {
		return "", 0, false
	}
	if i := strings.LastIndexByte(label, '-'); i > 0 {
		if p, err := strconv.Atoi(label[i+1:]); err == nil && p > 0 && p < 65536 {
			return label[:i], p, true
		}
		// A bad port suffix on a 32-hex id is a malformed proxy URL, not
		// a name. A non-id prefix is just a hyphenated name candidate.
		if len(label[:i]) == 32 && isHex(label[:i]) {
			return "", 0, false
		}
	}
	// No port suffix: the label is either a 32-hex lease id or a friendly
	// name assigned via the tag endpoint. Both resolve to a lease.
	if !isValidLabel(label) {
		return "", 0, false
	}
	return label, defaultProxyPort, true
}

// parseProxyHost2 is the U7/T7 extension of parseProxyHost. It accepts
// the legacy single-label form plus the per-user form (suffix =
// SPOOND_PROXY_HOST_SUFFIX):
//
//	<label>.<suffix>        → user "" (any/legacy)
//	<label>.<user>.<suffix> → user <user>
//
// Returns user="" when the hostname has no user segment. The caller
// (handleProxy) decides whether the user segment is allowed for the
// authenticated owner.
func parseProxyHost2(host, suffix string) (label, user string, port int, ok bool) {
	h := strings.ToLower(strings.TrimSpace(host))
	// Strip any explicit :port from the Host header (rare on 443, cheap).
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.HasSuffix(h, "]") {
		if _, err := strconv.Atoi(h[i+1:]); err == nil {
			h = h[:i]
		}
	}
	if !strings.HasSuffix(h, suffix) {
		return "", "", 0, false
	}
	pre := strings.TrimSuffix(h, suffix)
	if pre == "" {
		return "", "", 0, false
	}
	// Two-label form: label.<user>.
	if i := strings.IndexByte(pre, '.'); i >= 0 {
		label, user = pre[:i], pre[i+1:]
		if !isValidLabel(label) || !isValidLabel(user) {
			return "", "", 0, false
		}
		return label, user, defaultProxyPort, true
	}
	// Single-label form: delegate to parseProxyHost (port suffix etc.).
	label, port, ok = parseProxyHost(host, suffix)
	if !ok {
		return "", "", 0, false
	}
	return label, "", port, true
}

// isValidLabel accepts a 32-hex lease id or a friendly name
// ([a-z0-9][a-z0-9-]{0,62}, no dots — the suffix owns the dots).
func isValidLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i, c := range s {
		ok := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' && i > 0
		if !ok {
			return false
		}
	}
	return true
}

func isHex(s string) bool {
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}
