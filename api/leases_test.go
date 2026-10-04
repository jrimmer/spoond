package api

// The /api/leases path family (2.0): every /api/sandboxes route is also
// served there, in one rewrite at the top of the handler chain, with
// identical auth and responses. /api/leases is the documented primary
// path; /api/sandboxes stays as a permanent alias (D5).

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// TestLeaseAliasCRUD covers create, get, list, exec and delete through
// /api/leases and checks each response matches the /api/sandboxes twin:
// same status, same body — the rewrite happens before auth and the mux,
// so there is nothing to diverge.
func TestLeaseAliasCRUD(t *testing.T) {
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

	// The same create through the alias must return an identical body.
	respAlias, leaseAlias := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
		map[string]any{"image": "py-base", "ttl": 300})
	if respAlias.StatusCode != http.StatusCreated {
		t.Fatalf("create via /api/sandboxes: %d: %v", respAlias.StatusCode, leaseAlias)
	}
	// ids differ by design; expires_at is derived from each grant's own
	// wall clock and the two requests are back to back, so either may
	// straddle a second boundary. Normalise both before comparing — the
	// alias must not change anything that is not inherently per-request.
	leaseAlias["id"] = id
	leaseAlias["expires_at"] = lease["expires_at"]
	if rawL, rawS := jsonBody(t, lease), jsonBody(t, leaseAlias); rawL != rawS {
		t.Fatalf("create responses diverge:\nleases: %s\nsandboxes: %s", rawL, rawS)
	}

	// Get through both paths.
	respL, gotL := doReq(t, "GET", ts.URL+"/api/leases/"+id, "token-a", nil)
	respS, gotS := doReq(t, "GET", ts.URL+"/api/sandboxes/"+id, "token-a", nil)
	if respL.StatusCode != http.StatusOK || respS.StatusCode != http.StatusOK {
		t.Fatalf("get: leases %d, sandboxes %d", respL.StatusCode, respS.StatusCode)
	}
	if jsonBody(t, gotL) != jsonBody(t, gotS) {
		t.Fatalf("get responses diverge:\nleases: %v\nsandboxes: %v", gotL, gotS)
	}

	// List through both paths sees the lease created through either.
	for _, base := range []string{"/api/leases", "/api/sandboxes"} {
		resp, list := doReq(t, "GET", ts.URL+base, "token-a", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list via %s: %d", base, resp.StatusCode)
		}
		if rows := list["sandboxes"].([]any); len(rows) != 2 {
			t.Fatalf("list via %s: %d rows, want 2", base, len(rows))
		}
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

	// Delete through /api/leases, then the get is 404 on both paths.
	resp, _ = doReq(t, "DELETE", ts.URL+"/api/leases/"+id, "token-a", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete via /api/leases: %d", resp.StatusCode)
	}
	for _, base := range []string{"/api/leases", "/api/sandboxes"} {
		resp, body := doReq(t, "GET", ts.URL+base+"/"+id, "token-a", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("get after delete via %s: %d: %v", base, resp.StatusCode, body)
		}
	}
}

// TestLeaseAliasCrossVisibility: a lease created through one path family
// is visible and usable through the other.
func TestLeaseAliasCrossVisibility(t *testing.T) {
	ts, _ := newTestServer(t)

	resp, lease := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
		map[string]any{"image": "py-base", "ttl": 300})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create via /api/sandboxes: %d: %v", resp.StatusCode, lease)
	}
	id, _ := lease["id"].(string)

	// Visible through /api/leases...
	resp, got := doReq(t, "GET", ts.URL+"/api/leases/"+id, "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get via /api/leases: %d: %v", resp.StatusCode, got)
	}
	// ...executable through /api/leases...
	resp, exec := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/exec", "token-a",
		map[string]any{"cmd": "echo via-leases"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exec via /api/leases: %d: %v", resp.StatusCode, exec)
	}
	// ...and deletable through /api/leases.
	resp, _ = doReq(t, "DELETE", ts.URL+"/api/leases/"+id, "token-a", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete via /api/leases: %d", resp.StatusCode)
	}
}

// TestLeaseAliasAuth pins that /api/leases is gated exactly like
// /api/sandboxes: no token is 401, a wrong token is 401, and an owned
// lease is not visible to another consumer's token.
func TestLeaseAliasAuth(t *testing.T) {
	ts, _ := newTestServer(t)

	// No token → 401 on both path families.
	for _, base := range []string{"/api/leases", "/api/sandboxes"} {
		resp, _ := doReq(t, "POST", ts.URL+base, "", map[string]any{"image": "py-base"})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("no-token create via %s: %d, want 401", base, resp.StatusCode)
		}
		resp, _ = doReq(t, "GET", ts.URL+base, "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("no-token list via %s: %d, want 401", base, resp.StatusCode)
		}
	}

	// Wrong token → 401 on /api/leases too.
	resp, _ := doReq(t, "GET", ts.URL+"/api/leases", "wrong", nil)
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

// TestLeaseAliasSubroutes pins the whole subroute surface: lifecycle,
// tag and share subroutes all answer under /api/leases with the same
// status codes their /api/sandboxes twins give.
func TestLeaseAliasSubroutes(t *testing.T) {
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
		{"tag", "POST", "/api/leases/" + id + "/tag", map[string]any{"name": "aliased"}, http.StatusOK},
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
			respL, _ := doReq(t, tc.method, ts.URL+tc.path, "token-a", tc.body)
			// The twin must answer identically.
			twinPath := apiSandboxPathPrefix + strings.TrimPrefix(tc.path, "/api/leases")
			respS, _ := doReq(t, tc.method, ts.URL+twinPath, "token-a", tc.body)
			if respL.StatusCode != tc.want || respS.StatusCode != tc.want {
				t.Fatalf("%s %s: leases %d, sandboxes %d, want %d",
					tc.method, tc.path, respL.StatusCode, respS.StatusCode, tc.want)
			}
		})
	}
}

// TestLeaseAliasStream pins that the WebSocket upgrade works under the
// /api/leases path too.
func TestLeaseAliasStream(t *testing.T) {
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
	ws.WriteMessage(websocket.TextMessage, []byte(`{"args":["echo","alias-ok"],"pty":false}`))
	_, payload, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read started frame: %v", err)
	}
	if !strings.Contains(string(payload), `"started"`) {
		t.Fatalf("first frame = %s, want started", payload)
	}
}

// TestLeaseAliasNotRewritten pins the segment boundary: /api/leasesX is
// not a lease path, is not rewritten, and 404s like any unknown route.
func TestLeaseAliasNotRewritten(t *testing.T) {
	ts, _ := newTestServer(t)

	for _, p := range []string{"/api/leasesX", "/api/leasesaw"} {
		resp, _ := doReq(t, "GET", ts.URL+p, "token-a", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404 (no rewrite)", p, resp.StatusCode)
		}
		resp, _ = doReq(t, "POST", ts.URL+p, "token-a", map[string]any{"image": "py-base"})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("POST %s = %d, want 404 (no rewrite)", p, resp.StatusCode)
		}
	}
}

// TestLeaseAliasMetricLabels pins that the alias does not split the
// request counters in two: a request through /api/leases records the
// same /api/sandboxes path label as its twin.
func TestLeaseAliasMetricLabels(t *testing.T) {
	if got, want := rewriteLeasePath("/api/leases"), "/api/sandboxes"; got != want {
		t.Fatalf("rewriteLeasePath(/api/leases) = %q, want %q", got, want)
	}
	if got, want := rewriteLeasePath("/api/leases/x/exec"), "/api/sandboxes/x/exec"; got != want {
		t.Fatalf("rewriteLeasePath(/api/leases/x/exec) = %q, want %q", got, want)
	}
	for _, p := range []string{"/api/leasesX", "/api/leasesaw", "/api/sandboxes"} {
		if got := rewriteLeasePath(p); got != p {
			t.Fatalf("rewriteLeasePath(%q) = %q, want it unchanged", p, got)
		}
	}
	if got, want := normalizePath("/api/leases/0123456789abcdef0123456789abcdef/exec"), "/api/sandboxes/:id/exec"; got != want {
		t.Fatalf("normalizePath(lease alias) = %q, want %q", got, want)
	}
	// The files tail is a guest path: it must not enter the metric label.
	if got, want := normalizePath("/api/leases/0123456789abcdef0123456789abcdef/files/etc/secret"), "/api/sandboxes/:id/files"; got != want {
		t.Fatalf("normalizePath(files) = %q, want %q", got, want)
	}
}

// jsonBody marshals v deterministically for response comparison.
func jsonBody(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
