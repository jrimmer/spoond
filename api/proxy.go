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

// proxyHostSuffix is the wildcard hostname suffix for the HTTP proxy.
// Caddy terminates TLS for *.sandbox.lacy.casa and forwards here.
const proxyHostSuffix = ".sandbox.lacy.casa"

// defaultProxyPort is the guest port used when the hostname carries none.
// exe.dev uses the Dockerfile EXPOSE port; we have no Dockerfiles, so the
// convention is port 3000 unless the caller names another via
// <lease-id>-<port>.sandbox.lacy.casa.
const defaultProxyPort = 3000

// ProxyHandler returns the HTTP handler for the public proxy listener
// (plain HTTP on an internal port; Caddy fronts it with wildcard TLS).
// Every request's Host header names a lease: <lease-id>.sandbox.lacy.casa
// → guest:3000, <lease-id>-<port>.sandbox.lacy.casa → guest:<port>.
// The lease id in the hostname is the capability (same model as SSH).
//
// Under forward-auth (U7/T7) the capability model is replaced: the
// proxy requires X-Proxy-Auth == the shared secret and resolves the
// authenticated user from Remote-User, then owner-scopes every lookup.
func (s *Server) ProxyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The LLM gateway also lives on the plain-HTTP proxy listener:
		// guests reach it at http://10.43.0.1:8891/llm/<lease-id>/...,
		// avoiding TLS validation of the backend's self-signed cert.
		if s.llm != nil && strings.HasPrefix(r.URL.Path, llmGatewayPrefix) {
			s.llm.ServeHTTP(w, r)
			return
		}
		// Static assets (e.g. the shelley agent binary) served to guests
		// at http://10.43.0.1:8891/assets/<file>. This is how a lease
		// fetches tooling that is too big for the exec API cmdline.
		if s.assetsDir != "" && strings.HasPrefix(r.URL.Path, "/assets/") {
			http.ServeFile(w, r, filepath.Join(s.assetsDir, strings.TrimPrefix(r.URL.Path, "/assets/")))
			return
		}
		// Forward-auth gate (U7/T7): off/"") = capability model.
		if s.proxyAuthMode == "forward-auth" {
			if !s.proxyAuthOK(w, r) {
				return
			}
		}
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

// SetAssetsDir enables static asset serving on the proxy listener.
func (s *Server) SetAssetsDir(dir string) { s.assetsDir = dir }

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	label, port, ok := parseProxyHost(r.Host)
	if !ok {
		http.Error(w, "unknown sandbox hostname", http.StatusNotFound)
		return
	}
	var lease *Lease
	// Under forward-auth, the authenticated owner scopes every lookup
	// (U7/T7): a user only ever reaches their own leases, by id or
	// friendly name. In the capability model (off) the hostname is the
	// credential and owner-blind lookups are used, as before.
	if owner := proxyOwnerFrom(r.Context()); owner != "" {
		lease = s.svc.lookupUserScoped(owner, label)
	} else if len(label) == 32 && isHex(label) {
		lease = s.svc.lookupAny(label)
	} else {
		lease = s.svc.lookupByName(label)
	}
	if lease == nil {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	s.svc.touch(lease.ID) // proxied web traffic is activity for the idle sweeper
	ep, err := s.svc.resolveEndpoint(r.Context(), lease)
	if err != nil {
		http.Error(w, "sandbox not running", http.StatusBadGateway)
		return
	}
	target := net.JoinHostPort(ep.GuestHost, strconv.Itoa(port))

	// Reverse proxy with a Transport that dials inside the sandbox netns
	// (the guest IP is only reachable from the host via setns). Both HTTP
	// and WebSocket upgrades work through this.
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: target})
			// Preserve the original Host so sandbox apps see the public
			// hostname (virtual hosting works as expected).
			pr.Out.Host = pr.In.Host
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				// ReverseProxy gives us the target addr; we dial it inside
				// the lease's netns. dialInNetns ignores addr and dials
				// target directly (bound in the guest netns).
				return dialInNetns(ep.Netns, target)
			},
			IdleConnTimeout: 30 * time.Second,
		},
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
// header. Accepted forms:
//
//	<32-hex-lease-id>.sandbox.lacy.casa        → port 3000
//	<32-hex-lease-id>-<port>.sandbox.lacy.casa → that port
//
// Returns ok=false for anything else (including the bare apex hostname).
func parseProxyHost(host string) (leaseID string, port int, ok bool) {
	h := strings.ToLower(strings.TrimSpace(host))
	// Strip any explicit :port from the Host header (rare on 443, cheap).
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.HasSuffix(h, "]") {
		if _, err := strconv.Atoi(h[i+1:]); err == nil {
			h = h[:i]
		}
	}
	if !strings.HasSuffix(h, proxyHostSuffix) {
		return "", 0, false
	}
	label := strings.TrimSuffix(h, proxyHostSuffix)
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
