package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/metrics"
	"github.com/jrimmer/spoond/v2/store"
)

// newHeartbeatTestServer builds a lease API server whose heartbeat
// handler carries an inspectable metrics collector.
func newHeartbeatTestServer(t *testing.T) (*httptest.Server, *Server, *Service, *store.DB, *testSub, *metrics.BackendMetrics) {
	t.Helper()
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	srv := NewServer(svc, NewImageRegistry(db))
	m := metrics.NewBackendMetrics()
	srv.SetHeartbeatMetrics(m)
	// One test server stands in for both listeners: /lease/ goes to the
	// guest-service handler (where guests reach it), the rest to the API.
	api, guest := srv.Handler(), srv.ProxyHandler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, leaseHeartbeatPrefix) {
			guest.ServeHTTP(w, r)
			return
		}
		api.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts, srv, svc, db, sub, m
}

// The heartbeat is a guest-service route only: the authenticated API
// listener treats /lease/ like any other path and wants a token.
func TestLeaseHeartbeatNotOnAPIListener(t *testing.T) {
	_, srv, _, _, _, _ := newHeartbeatTestServer(t)
	api := httptest.NewServer(srv.Handler())
	t.Cleanup(api.Close)
	if resp := heartbeat(t, api.URL+"/lease/00000000000000000000000000000000/active"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("API listener: status = %d, want 401", resp.StatusCode)
	}
}

// heartbeat posts one heartbeat and returns the response.
func heartbeat(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	resp.Body.Close()
	return resp
}

// TestLeaseHeartbeatSuccess pins the success path: LastActive moves to
// now and is persisted, while ExpiresAt, persistence and state stay
// exactly as they were.
func TestLeaseHeartbeatSuccess(t *testing.T) {
	ts, _, svc, db, _, m := newHeartbeatTestServer(t)

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "persistent": true, "ttl": 600})
	id := create["id"].(string)

	before := *svc.lookup("consumer-a", id)
	// Age the lease so a heartbeat must visibly move LastActive.
	stale := time.Now().Add(-10 * time.Minute)
	svc.store.mu.Lock()
	svc.store.leases[id].LastActive = stale
	svc.store.mu.Unlock()

	resp := heartbeat(t, ts.URL+"/lease/"+id+"/active")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("heartbeat status = %d, want 204", resp.StatusCode)
	}

	// LastActive moved and is persisted.
	l := svc.lookup("consumer-a", id)
	if !l.LastActive.After(stale) {
		t.Fatalf("LastActive did not move: %v (stale %v)", l.LastActive, stale)
	}
	row := leaseRow(t, db, id)
	if row.LastActive.IsZero() || row.LastActive.Before(stale) {
		t.Fatalf("persisted LastActive = %v, want >= %v", row.LastActive, stale)
	}
	// Everything else is untouched.
	if !l.ExpiresAt.Equal(before.ExpiresAt) {
		t.Fatalf("ExpiresAt moved: %v -> %v", before.ExpiresAt, l.ExpiresAt)
	}
	if !row.ExpiresAt.Equal(before.ExpiresAt) {
		t.Fatalf("persisted ExpiresAt moved: %v -> %v", before.ExpiresAt, row.ExpiresAt)
	}
	if !l.Persistent {
		t.Fatalf("persistence changed")
	}
	if l.Suspended || l.State != "running" {
		t.Fatalf("state changed: suspended=%v state=%q", l.Suspended, l.State)
	}
	if got := counterValue(t, m.LeaseHeartbeats); got != 1 {
		t.Fatalf("spoond_lease_heartbeats_total = %v, want 1", got)
	}
}

// TestLeaseHeartbeatUnknownLease: an unknown id answers 404, as do the
// malformed routes under /lease/.
func TestLeaseHeartbeatUnknownLease(t *testing.T) {
	ts, _, _, _, _, m := newHeartbeatTestServer(t)
	for _, id := range []string{"00000000000000000000000000000000", "no-such-lease"} {
		if resp := heartbeat(t, ts.URL+"/lease/"+id+"/active"); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown id %q: status = %d, want 404", id, resp.StatusCode)
		}
	}
	for _, p := range []string{"/lease/", "/lease/x", "/lease/x/nope"} {
		if resp := heartbeat(t, ts.URL+p); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("path %q: status = %d, want 404", p, resp.StatusCode)
		}
	}
	if got := counterValue(t, m.LeaseHeartbeats); got != 0 {
		t.Fatalf("spoond_lease_heartbeats_total = %v, want 0", got)
	}
}

// TestLeaseHeartbeatReleasedLease: a released lease answers 404 — it is
// indistinguishable from an unknown one, so nothing about other leases
// leaks through the response.
func TestLeaseHeartbeatReleasedLease(t *testing.T) {
	ts, _, svc, _, _, _ := newHeartbeatTestServer(t)

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "persistent": true})
	id := create["id"].(string)
	l := svc.lookup("consumer-a", id)
	svc.store.mu.Lock()
	l.released = true // release() drops the map entry; the handler only sees its absence
	svc.store.mu.Unlock()

	if resp := heartbeat(t, ts.URL+"/lease/"+id+"/active"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("released lease: status = %d, want 404", resp.StatusCode)
	}
}

// TestLeaseHeartbeatSuspendedLease: a suspended lease answers 409 and is
// NOT resumed — the heartbeat never changes lifecycle state.
func TestLeaseHeartbeatSuspendedLease(t *testing.T) {
	ts, _, svc, _, sub, _ := newHeartbeatTestServer(t)

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "persistent": true})
	id := create["id"].(string)
	// Suspend through the public verb so State/Suspended/ResumeBuildID
	// all move together.
	if resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/suspend", "token-a", nil); resp.StatusCode != 200 {
		t.Fatalf("suspend: status %d: %v", resp.StatusCode, body)
	}

	if resp := heartbeat(t, ts.URL+"/lease/"+id+"/active"); resp.StatusCode != http.StatusConflict {
		t.Fatalf("suspended lease: status = %d, want 409", resp.StatusCode)
	}
	l := svc.lookup("consumer-a", id)
	if !l.Suspended || l.State != "suspended" {
		t.Fatalf("heartbeat resumed a suspended lease: suspended=%v state=%q", l.Suspended, l.State)
	}
	if got := calls(sub.Fake, "Create"); got != 1 {
		t.Fatalf("heartbeat created a sandbox: %d creates, want 1 (the grant only)", got)
	}
}

// TestLeaseHeartbeatMethodNotAllowed: every non-POST method answers 405
// with an Allow header, and no write happens.
func TestLeaseHeartbeatMethodNotAllowed(t *testing.T) {
	ts, _, svc, _, _, m := newHeartbeatTestServer(t)

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "persistent": true})
	id := create["id"].(string)
	before := svc.lookup("consumer-a", id).LastActive

	for _, method := range []string{"GET", "HEAD", "PUT", "DELETE", "PATCH", "OPTIONS"} {
		req, err := http.NewRequest(method, ts.URL+"/lease/"+id+"/active", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, resp.StatusCode)
		}
		if allow := resp.Header.Get("Allow"); allow != http.MethodPost {
			t.Fatalf("%s: Allow = %q, want POST", method, allow)
		}
	}
	if !svc.lookup("consumer-a", id).LastActive.Equal(before) {
		t.Fatalf("non-POST method wrote LastActive")
	}
	if got := counterValue(t, m.LeaseHeartbeats); got != 0 {
		t.Fatalf("spoond_lease_heartbeats_total = %v, want 0", got)
	}
}

// TestLeaseHeartbeatWriteLimit pins the 60 s floor: the first heartbeat
// writes, calls inside the window answer 204 without writing, and a call
// after the window writes again.
func TestLeaseHeartbeatWriteLimit(t *testing.T) {
	ts, srv, svc, _, _, m := newHeartbeatTestServer(t)

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "persistent": true})
	id := create["id"].(string)
	url := ts.URL + "/lease/" + id + "/active"

	// First call writes.
	if resp := heartbeat(t, url); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("first heartbeat: status = %d, want 204", resp.StatusCode)
	}
	written := svc.lookup("consumer-a", id).LastActive

	// Calls inside the window still succeed but do not write.
	for i := 0; i < 3; i++ {
		if resp := heartbeat(t, url); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("in-window heartbeat %d: status = %d, want 204", i, resp.StatusCode)
		}
	}
	if !svc.lookup("consumer-a", id).LastActive.Equal(written) {
		t.Fatalf("in-window heartbeat wrote LastActive")
	}
	if got := counterValue(t, m.LeaseHeartbeats); got != 4 {
		t.Fatalf("spoond_lease_heartbeats_total = %v, want 4 (every 204 counts)", got)
	}

	// After the window, the next call writes again.
	srv.heartbeat.writeMu.Lock()
	srv.heartbeat.lastWrite[id] = time.Now().Add(-heartbeatWriteInterval - time.Second)
	srv.heartbeat.writeMu.Unlock()
	if resp := heartbeat(t, url); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("post-window heartbeat: status = %d, want 204", resp.StatusCode)
	}
	if !svc.lookup("consumer-a", id).LastActive.After(written) {
		t.Fatalf("post-window heartbeat did not write LastActive")
	}
}

// TestLeaseHeartbeatKeepsIdleSweepAway pins the reason the endpoint
// exists: an idle persistent lease is suspended by the idle sweep, while
// an otherwise identical one whose guest heartbeats is not. Both leases
// start equally idle; only one receives a heartbeat before the sweep.
// (The heartbeat's write floor is 60 s, so the idle timeout in play here
// is an hour — the shape production uses.)
func TestLeaseHeartbeatKeepsIdleSweepAway(t *testing.T) {
	ts, _, svc, _, sub, _ := newHeartbeatTestServer(t)
	svc.cfg.IdleTimeout = time.Hour
	ctx := context.Background()

	// Two persistent leases, both idle for two hours.
	_, a := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "persistent": true})
	_, b := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-b", map[string]any{"image": "py-base", "persistent": true})
	idA, idB := a["id"].(string), b["id"].(string)
	stale := time.Now().Add(-2 * time.Hour)
	svc.store.mu.Lock()
	svc.store.leases[idA].LastActive = stale
	svc.store.leases[idB].LastActive = stale
	svc.store.mu.Unlock()

	// Only the first lease's guest heartbeats (its first call writes).
	if resp := heartbeat(t, ts.URL+"/lease/"+idA+"/active"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("heartbeat on %s: status = %d, want 204", idA, resp.StatusCode)
	}

	// Sweep: the heartbeating lease survives, the silent one suspends.
	svc.sweepExpired(ctx)
	if l := svc.lookup("consumer-a", idA); l == nil {
		t.Fatalf("lease %s vanished", idA)
	} else if l.Suspended {
		t.Fatalf("lease %s was suspended despite a fresh heartbeat (LastActive %v)", idA, l.LastActive)
	}
	if l := svc.lookup("consumer-b", idB); l == nil {
		t.Fatalf("lease %s vanished", idB)
	} else if !l.Suspended {
		t.Fatalf("lease %s should have been idle-suspended", idB)
	}
	if got := calls(sub.Fake, "Pause"); got != 1 {
		t.Fatalf("expected exactly 1 pause (the idle lease), got %d", got)
	}
}

// TestLeaseHeartbeatMetricsPathLabels pins the path normalization for
// /lease/ routes: the lease id collapses to :id so HTTPReqs cardinality
// stays bounded no matter how many leases heartbeat.
func TestLeaseHeartbeatMetricsPathLabels(t *testing.T) {
	cases := map[string]string{
		"/lease/abcdef/active":                           "/lease/:id/active",
		"/lease/0123456789abcdef0123456789abcdef/active": "/lease/:id/active",
		"/lease/0123456789abcdef0123456789abcdef":        "/lease/:id",
		"/lease/":               "/lease/",
		"/api/sandboxes/x/exec": "/api/sandboxes/x/exec",
	}
	for in, want := range cases {
		if got := normalizePath(in); got != want {
			t.Errorf("normalizePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLeaseHeartbeatOnGuestServiceListener pins that the guest-service
// (proxy) listener serves the route too, with no bearer token — the
// listener every network policy, restricted included, can reach.
func TestLeaseHeartbeatOnGuestServiceListener(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	srv := NewServer(svc, NewImageRegistry(db))
	ph := srv.ProxyHandler()

	l, err := svc.grant(context.Background(), "consumer-a", "py-base", time.Minute, true, "restricted", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	stale := time.Now().Add(-time.Hour)
	svc.store.mu.Lock()
	l.LastActive = stale
	svc.store.mu.Unlock()

	req := httptest.NewRequest("POST", "http://10.43.0.1:8891/lease/"+l.ID+"/active", nil)
	rec := httptest.NewRecorder()
	ph.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("guest-service listener: status = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if !svc.lookup("consumer-a", l.ID).LastActive.After(stale) {
		t.Fatalf("guest-service heartbeat did not move LastActive")
	}

	// Wrong method on the same listener.
	req2 := httptest.NewRequest("GET", "http://10.43.0.1:8891/lease/"+l.ID+"/active", nil)
	rec2 := httptest.NewRecorder()
	ph.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusMethodNotAllowed {
		t.Fatalf("guest-service listener GET: status = %d, want 405", rec2.Code)
	}
}
