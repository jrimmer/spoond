package api

// The /api/leases path family (4.0): one spelling, one registration
// per route.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// TestLeasesCRUD covers create, get, list, exec and delete through
// /api/leases: the one lease surface since 4.0.
func TestLeasesCRUD(t *testing.T) {
	ts, _ := newTestServer(t)

	// Create through /api/leases.
	resp, lease := doReq(t, "POST", ts.URL+"/api/leases", "token-a",
		map[string]any{"image": "py-base", "ttl": 300})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create via /api/leases: %d: %v", resp.StatusCode, lease)
	}
	id, _ := lease["id"].(string)
	if id == "" {
		t.Fatalf("create response missing id: %v", lease)
	}

	// Get it back.
	resp, got := doReq(t, "GET", ts.URL+"/api/leases/"+id, "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get via /api/leases: %d: %v", resp.StatusCode, got)
	}
	if got["id"] != id {
		t.Fatalf("get via /api/leases: id = %v, want %v", got["id"], id)
	}

	// List sees the lease.
	resp, list := doReq(t, "GET", ts.URL+"/api/leases", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list via /api/leases: %d", resp.StatusCode)
	}
	if rows := list["sandboxes"].([]any); len(rows) != 1 {
		t.Fatalf("list via /api/leases: %d rows, want 1", len(rows))
	}

	// Exec through /api/leases.
	resp, exec := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/exec", "token-a",
		map[string]any{"cmd": "echo hi"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exec via /api/leases: %d: %v", resp.StatusCode, exec)
	}
	if exit, _ := exec["exit"].(float64); exit != 0 {
		t.Fatalf("exec exit = %v, want 0", exec["exit"])
	}
	if out, _ := exec["stdout"].(string); out == "" {
		t.Fatalf("exec stdout empty: %v", exec)
	}

	// Delete through /api/leases, then the get is 404.
	resp, _ = doReq(t, "DELETE", ts.URL+"/api/leases/"+id, "token-a", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete via /api/leases: %d", resp.StatusCode)
	}
	resp, body := doReq(t, "GET", ts.URL+"/api/leases/"+id, "token-a", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete via /api/leases: %d: %v", resp.StatusCode, body)
	}
}

// TestLeasesAuth pins the auth gate on /api/leases: no token is 401,
// a wrong token is 401, and an owned lease is not visible to another
// consumer's token.
func TestLeasesAuth(t *testing.T) {
	ts, _ := newTestServer(t)

	// No token → 401.
	resp, _ := doReq(t, "POST", ts.URL+"/api/leases", "", map[string]any{"image": "py-base"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token create via /api/leases: %d, want 401", resp.StatusCode)
	}
	resp, _ = doReq(t, "GET", ts.URL+"/api/leases", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token list via /api/leases: %d, want 401", resp.StatusCode)
	}

	// Wrong token → 401 too.
	resp, _ = doReq(t, "GET", ts.URL+"/api/leases", "wrong", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-token list via /api/leases: %d, want 401", resp.StatusCode)
	}

	// Owner scoping is unchanged: token-b cannot see token-a's lease.
	resp, lease := doReq(t, "POST", ts.URL+"/api/leases", "token-a",
		map[string]any{"image": "py-base", "ttl": 300})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d: %v", resp.StatusCode, lease)
	}
	id, _ := lease["id"].(string)
	resp, _ = doReq(t, "GET", ts.URL+"/api/leases/"+id, "token-b", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign-token get via /api/leases: %d, want 404", resp.StatusCode)
	}
}

// TestLeasesSubroutes pins the whole subroute surface: lifecycle,
// tag and share subroutes all answer under /api/leases.
func TestLeasesSubroutes(t *testing.T) {
	ts, _ := newTestServer(t)

	resp, lease := doReq(t, "POST", ts.URL+"/api/leases", "token-a",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d: %v", resp.StatusCode, lease)
	}
	id, _ := lease["id"].(string)

	cases := []struct {
		name   string
		method string
		path   string
		body   any
		want   int
	}{
		{"tag", "POST", "/api/leases/" + id + "/tag", map[string]any{"name": "tagged"}, http.StatusOK},
		{"comment", "POST", "/api/leases/" + id + "/comment", map[string]any{"comment": "hi"}, http.StatusOK},
		{"endpoint", "GET", "/api/leases/" + id + "/endpoint", nil, http.StatusOK},
		{"keepalive", "POST", "/api/leases/" + id + "/keepalive", nil, http.StatusOK},
		{"checkpoint", "POST", "/api/leases/" + id + "/checkpoint", nil, http.StatusOK},
		{"share grant", "POST", "/api/leases/" + id + "/share",
			map[string]any{"grantee": "u-friend", "mode": "http"}, http.StatusCreated},
		{"network bad policy", "POST", "/api/leases/" + id + "/network",
			map[string]any{"network_policy": "bogus"}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, _ := doReq(t, tc.method, ts.URL+tc.path, "token-a", tc.body)
			if resp.StatusCode != tc.want {
				t.Fatalf("%s %s: %d, want %d", tc.method, tc.path, resp.StatusCode, tc.want)
			}
		})
	}
}

// TestLeasesStream pins that the WebSocket upgrade works under
// /api/leases.
func TestLeasesStream(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, lease := doReq(t, "POST", ts.URL+"/api/leases", "token-a",
		map[string]any{"image": "py-base", "ttl": 300})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d: %v", resp.StatusCode, lease)
	}
	id, _ := lease["id"].(string)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/leases/" + id + "/stream"
	hdr := http.Header{"Authorization": {"Bearer token-a"}}
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		t.Fatalf("dial /api/leases stream: %v", err)
	}
	defer ws.Close()
	ws.WriteMessage(websocket.TextMessage, []byte(`{"args":["echo","leases-ok"],"pty":false}`))
	_, payload, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read started frame: %v", err)
	}
	if !strings.Contains(string(payload), `"started"`) {
		t.Fatalf("first frame = %s, want started", payload)
	}
}

// TestLeasePathPins the segment boundary: /api/leasesX is not a lease
// path and 404s like any unknown route.
func TestLeasePathSegmentBoundary(t *testing.T) {
	ts, _ := newTestServer(t)

	for _, p := range []string{"/api/leasesX", "/api/leasesaw"} {
		resp, _ := doReq(t, "GET", ts.URL+p, "token-a", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", p, resp.StatusCode)
		}
		resp, _ = doReq(t, "POST", ts.URL+p, "token-a", map[string]any{"image": "py-base"})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("POST %s = %d, want 404", p, resp.StatusCode)
		}
	}
}

// TestLeaseMetricLabels pins the path labels the request counters
// record: one series per route, never per lease id or guest path.
func TestLeaseMetricLabels(t *testing.T) {
	if got, want := normalizePath("/api/leases/0123456789abcdef0123456789abcdef/exec"), "/api/leases/:id/exec"; got != want {
		t.Fatalf("normalizePath(exec) = %q, want %q", got, want)
	}
	// The files tail is a guest path: it must not enter the metric label.
	if got, want := normalizePath("/api/leases/0123456789abcdef0123456789abcdef/files/etc/secret"), "/api/leases/:id/files"; got != want {
		t.Fatalf("normalizePath(files) = %q, want %q", got, want)
	}
}
