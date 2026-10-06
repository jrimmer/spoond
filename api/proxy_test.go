package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseProxyHost(t *testing.T) {
	cases := []struct {
		host     string
		wantID   string
		wantPort int
		wantOK   bool
	}{
		{"a1b2c3d4e5f60718293a4b5c6d7e8f90.sandbox.example.com", "a1b2c3d4e5f60718293a4b5c6d7e8f90", 3000, true},
		{"a1b2c3d4e5f60718293a4b5c6d7e8f90.sandbox.example.com:443", "a1b2c3d4e5f60718293a4b5c6d7e8f90", 3000, true},
		{"a1b2c3d4e5f60718293a4b5c6d7e8f90-8080.sandbox.example.com", "a1b2c3d4e5f60718293a4b5c6d7e8f90", 8080, true},
		{"A1B2C3D4E5F60718293A4B5C6D7E8F90.sandbox.example.com", "a1b2c3d4e5f60718293a4b5c6d7e8f90", 3000, true},
		{"sandbox.example.com", "", 0, false},
		{"nope.sandbox.example.com", "nope", 3000, true},                                                                       // friendly name (resolves at lookup)
		{"mybox-9000.sandbox.example.com", "mybox", 9000, true},                                                                // name + port
		{"mybox-0.sandbox.example.com", "mybox-0", 3000, true},                                                                 // hyphenated name, not a port
		{"toolongid123456789012345678901234567890.sandbox.example.com", "toolongid123456789012345678901234567890", 3000, true}, // 39 chars — valid name
		{"abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnopqrstuvwxyz0123456789.sandbox.example.com", "", 0, false},         // >63 chars
		{"a1b2c3d4e5f60718293a4b5c6d7e8f90-0.sandbox.example.com", "", 0, false},                                               // port 0 invalid
		{"a1b2c3d4e5f60718293a4b5c6d7e8f90-70000.sandbox.example.com", "", 0, false},                                           // port >65535 invalid
		{"other.example.com", "", 0, false},
	}
	for _, c := range cases {
		id, port, ok := parseProxyHost(c.host, defaultProxyHostSuffix)
		if id != c.wantID || port != c.wantPort || ok != c.wantOK {
			t.Errorf("parseProxyHost(%q) = (%q,%d,%v), want (%q,%d,%v)",
				c.host, id, port, ok, c.wantID, c.wantPort, c.wantOK)
		}
	}
}

// TestProxyDirectorHeaders pins the ReverseProxy Rewrite: exactly the
// three E2B routing headers reach the orchestrator proxy, the inbound
// gate/auth headers are stripped first, X-Forwarded-Host/-Proto come
// from the inbound request, and the path and query are kept.
func TestProxyDirectorHeaders(t *testing.T) {
	var mu sync.Mutex
	var gotHost, gotURI string
	var gotHeader http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotHost = r.Host
		gotHeader = r.Header.Clone()
		gotURI = r.URL.RequestURI()
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	svc, db, _ := newTestService(t)
	svc.cfg.ProxyURL = upstream.URL
	seedImage(t, db, "py-base", 2048)
	l, err := svc.grant(t.Context(), "u-1", "py-base", time.Minute, false, "restricted", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.store.mu.Lock()
	l.HostIP = "10.11.0.5"
	svc.store.mu.Unlock()
	sandboxID := l.SandboxID

	ph := NewServer(svc, NewImageRegistry(db)).ProxyHandler()
	req := httptest.NewRequest("GET", "http://"+l.ID+"-8080.sandbox.example.com/app?q=1", nil)
	req.Header.Set("X-Proxy-Auth", "gate-secret")
	req.Header.Set("Remote-User", "jason")
	req.Header.Set("X-Spoond-User-Id", "u-9")
	req.Header.Set("X-Bootstrap-Token", "bootstrap-secret")
	req.Header.Set("E2b-Client-Version", "forged")
	req.Header.Set("E2b-Sandbox-Id", "forged-sandbox")
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	ph.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy status %d: %s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	host, uri, h := gotHost, gotURI, gotHeader
	mu.Unlock()
	if host != upstream.URL[len("http://"):] {
		t.Fatalf("upstream Host = %q, want the proxy URL host", host)
	}
	if uri != "/app?q=1" {
		t.Fatalf("upstream URI = %q, want path and query kept", uri)
	}
	if v := h.Get("E2b-Sandbox-Id"); v != sandboxID {
		t.Fatalf("E2b-Sandbox-Id = %q, want %q", v, sandboxID)
	}
	if v := h.Get("E2b-Sandbox-Port"); v != "8080" {
		t.Fatalf("E2b-Sandbox-Port = %q, want 8080", v)
	}
	if want := "fake-traffic-" + sandboxID; h.Get("e2b-traffic-access-token") != want {
		t.Fatalf("e2b-traffic-access-token = %q, want %q", h.Get("e2b-traffic-access-token"), want)
	}
	// Exactly the three E2B headers: inbound E2b-* headers were stripped.
	for name := range h {
		ln := strings.ToLower(name)
		if strings.HasPrefix(ln, "e2b-") && ln != "e2b-sandbox-id" && ln != "e2b-sandbox-port" && ln != "e2b-traffic-access-token" {
			t.Fatalf("unexpected E2B header %q survived: %v", name, h)
		}
	}
	for _, stripped := range []string{"X-Proxy-Auth", "Remote-User", "X-Spoond-User-Id", "X-Bootstrap-Token"} {
		if v := h.Get(stripped); v != "" {
			t.Fatalf("%s reached the upstream: %q", stripped, v)
		}
	}
	if v := h.Get("X-Forwarded-Host"); v != l.ID+"-8080.sandbox.example.com" {
		t.Fatalf("X-Forwarded-Host = %q, want the inbound Host", v)
	}
	if v := h.Get("X-Forwarded-Proto"); v != "https" {
		t.Fatalf("X-Forwarded-Proto = %q, want https from the inbound header", v)
	}
	if h.Get("X-Forwarded-For") == "" {
		t.Fatal("X-Forwarded-For missing (the client address is appended)")
	}

	// No TLS and no inbound proto header → http.
	req2 := httptest.NewRequest("GET", "http://"+l.ID+".sandbox.example.com/", nil)
	rec2 := httptest.NewRecorder()
	ph.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("proxy status %d", rec2.Code)
	}
	mu.Lock()
	h = gotHeader
	mu.Unlock()
	if v := h.Get("X-Forwarded-Proto"); v != "http" {
		t.Fatalf("X-Forwarded-Proto = %q, want http", v)
	}
	if v := h.Get("X-Forwarded-Host"); v != l.ID+".sandbox.example.com" {
		t.Fatalf("X-Forwarded-Host = %q", v)
	}
	if v := h.Get("E2b-Sandbox-Port"); v != "3000" {
		t.Fatalf("E2b-Sandbox-Port = %q, want the default 3000", v)
	}
}

// TestProxyRefusesEnvdPortAndSuspended: guest port 49983 is refused with
// 403, and a suspended lease answers 409.
func TestProxyRefusesEnvdPortAndSuspended(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	id := create["id"].(string)

	proxy := NewServer(svc, NewImageRegistry(db)).ProxyHandler()

	// 49983 → 403.
	req := httptest.NewRequest("GET", "http://"+id+"-49983.sandbox.example.com/", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("envd port status %d, want 403", rec.Code)
	}

	// Suspended → 409.
	resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/suspend", "token-a", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("suspend status %d", resp.StatusCode)
	}
	req = httptest.NewRequest("GET", "http://"+id+"-3000.sandbox.example.com/", nil)
	rec = httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("suspended proxy status %d, want 409", rec.Code)
	}
}
