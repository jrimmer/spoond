package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
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
func (s *Server) ProxyHandler() http.Handler {
	return http.HandlerFunc(s.handleProxy)
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	leaseID, port, ok := parseProxyHost(r.Host)
	if !ok {
		http.Error(w, "unknown sandbox hostname", http.StatusNotFound)
		return
	}
	lease := s.svc.lookupAny(leaseID)
	if lease == nil {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
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
	}
	// No port suffix: the whole label must look like a lease id.
	if len(label) != 32 || !isHex(label) {
		return "", 0, false
	}
	return label, defaultProxyPort, true
}

func isHex(s string) bool {
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}
