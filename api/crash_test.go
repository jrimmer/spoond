package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/identity"
	"github.com/jrimmer/spoond/v2/store"
)

// newCrashServer builds a lease API server with an ADMIN_TOKEN plus an
// identity store holding an admin ("root-tok") and a non-admin
// ("bob-tok"). A lease owner can act as bob; ADMIN_TOKEN is distinct so
// the two admin paths can be told apart.
func newCrashServer(t *testing.T) (*httptest.Server, *Service, *store.DB, *testSub) {
	t.Helper()
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ids, _ := identity.NewStore("")
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(db))
	srv.SetAdminToken("operator-tok")
	h := srv.Handler()

	if rec, _ := doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"admin","fingerprints":["SHA256:fp-a"],"token":"root-tok"}`); rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap admin: %d", rec.Code)
	}
	if rec, _ := doUsersReq(t, h, "POST", "/api/users", "root-tok", `{"name":"bob","fingerprints":["SHA256:fp-b"],"token":"bob-tok"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create bob: %d", rec.Code)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts, svc, db, sub
}

// TestCrashTestRecoversFromCheckpoint: a lease with a checkpoint recovers
// from it, generation +1, the checkpoint build in use, and a fresh
// sandbox row.
func TestCrashTestRecoversFromCheckpoint(t *testing.T) {
	ts, svc, _, sub := newCrashServer(t)
	ctx := context.Background()

	l, err := svc.grant(ctx, "u-bob", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	// A real admin/user lease: rebind the owner to bob so the token path
	// is realistic. (grant above runs through the service directly.)
	if _, err := svc.checkpointLease(ctx, l); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	buildBefore := l.LastCheckpointBuildID
	if buildBefore == "" {
		t.Fatal("checkpoint left no build id")
	}
	oldSandbox := l.SandboxID

	resp, body := doReq(t, "POST", ts.URL+"/api/admin/leases/"+l.ID+"/crash", "operator-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("crash = %d (%v), want 200", resp.StatusCode, body)
	}
	if body["id"] != l.ID {
		t.Fatalf("id = %v, want %q", body["id"], l.ID)
	}
	if body["result"] != "recovered" {
		t.Fatalf("result = %v, want recovered", body["result"])
	}
	if body["generation"] != float64(2) {
		t.Fatalf("generation = %v, want 2", body["generation"])
	}
	if body["state"] != "recovered" {
		t.Fatalf("state = %v, want recovered", body["state"])
	}
	if l.State != "recovered" || !l.live() {
		t.Fatalf("lease state = %q, want live recovered", l.State)
	}
	if l.BuildID != buildBefore {
		t.Fatalf("recovered BuildID = %q, want the checkpoint build %q", l.BuildID, buildBefore)
	}
	if !l.RecoveredFrom.Equal(l.LastCheckpointAt) {
		t.Fatalf("RecoveredFrom = %v, want %v", l.RecoveredFrom, l.LastCheckpointAt)
	}
	// The sandbox was deleted and recreated from the checkpoint build.
	if got := calls(sub.Fake, "Delete "+oldSandbox); got != 1 {
		t.Fatalf("Delete calls for %s = %d, want 1", oldSandbox, got)
	}
	if got := calls(sub.Fake, "Create "+l.SandboxID); got != 2 {
		t.Fatalf("Create calls for %s = %d, want 2 (grant + recovery)", l.SandboxID, got)
	}
}

// TestCrashTestLostWithoutCheckpoint: a lease with no checkpoint is
// marked lost and its stale sandbox row goes.
func TestCrashTestLostWithoutCheckpoint(t *testing.T) {
	ts, svc, db, _ := newCrashServer(t)
	ctx := context.Background()

	l, err := svc.grant(ctx, "u-bob", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := db.GetSandbox(ctx, l.SandboxID); err != nil {
		t.Fatalf("sandbox row missing before crash: %v", err)
	}

	resp, body := doReq(t, "POST", ts.URL+"/api/admin/sandboxes/"+l.ID+"/crash", "operator-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("crash = %d (%v), want 200", resp.StatusCode, body)
	}
	if body["result"] != "lost" {
		t.Fatalf("result = %v, want lost", body["result"])
	}
	if body["state"] != "lost" {
		t.Fatalf("state = %v, want lost", body["state"])
	}
	if body["generation"] != float64(1) {
		t.Fatalf("generation = %v, want 1 (lost does not bump)", body["generation"])
	}
	if l.State != "lost" || l.live() {
		t.Fatalf("lease = %+v, want lost", l)
	}
	if _, err := db.GetSandbox(ctx, l.SandboxID); err == nil {
		t.Fatal("the lost lease's stale sandbox row survived")
	}
}

// TestCrashTestNonAdmin403: a valid non-admin user token is refused 403.
func TestCrashTestNonAdmin403(t *testing.T) {
	ts, svc, _, _ := newCrashServer(t)
	ctx := context.Background()
	l, err := svc.grant(ctx, "u-bob", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	resp, body := doReq(t, "POST", ts.URL+"/api/admin/leases/"+l.ID+"/crash", "bob-tok", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin crash = %d (%v), want 403", resp.StatusCode, body)
	}
	if l.State != "running" {
		t.Fatalf("lease state = %q after a refused crash, want running", l.State)
	}
}

// TestCrashTestDisabled404: with no ADMIN_TOKEN the route is disabled.
func TestCrashTestDisabled404(t *testing.T) {
	ts, _, _, _ := newAdminServer(t, "")
	resp, body := doReq(t, "POST", ts.URL+"/api/admin/leases/does-not-matter/crash", "admin-tok", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("crash without ADMIN_TOKEN = %d (%v), want 404", resp.StatusCode, body)
	}
}

// TestCrashTestUnknownLease404: an unknown lease answers 404.
func TestCrashTestUnknownLease404(t *testing.T) {
	ts, _, _, _ := newCrashServer(t)
	resp, body := doReq(t, "POST", ts.URL+"/api/admin/leases/deadbeef/crash", "operator-tok", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown lease crash = %d (%v), want 404", resp.StatusCode, body)
	}
}

// TestCrashTestSuspended409: a suspended lease has nothing running to
// crash.
func TestCrashTestSuspended409(t *testing.T) {
	ts, svc, _, _ := newCrashServer(t)
	ctx := context.Background()
	l, err := svc.grant(ctx, "u-bob", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.suspend(ctx, "u-bob", l.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	resp, body := doReq(t, "POST", ts.URL+"/api/admin/leases/"+l.ID+"/crash", "operator-tok", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("suspended crash = %d (%v), want 409", resp.StatusCode, body)
	}
	if l.State != "suspended" {
		t.Fatalf("lease state = %q, want suspended", l.State)
	}
}

// TestCrashTestBusy409: a busy lease is refused.
func TestCrashTestBusy409(t *testing.T) {
	ts, svc, _, _ := newCrashServer(t)
	ctx := context.Background()
	l, err := svc.grant(ctx, "u-bob", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.store.mu.Lock()
	l.busy = true
	svc.store.mu.Unlock()
	t.Cleanup(func() {
		svc.store.mu.Lock()
		l.busy = false
		svc.store.mu.Unlock()
	})
	resp, body := doReq(t, "POST", ts.URL+"/api/admin/leases/"+l.ID+"/crash", "operator-tok", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("busy crash = %d (%v), want 409", resp.StatusCode, body)
	}
}

// TestCrashTestAlreadyLost410: a lease already lost answers 410.
func TestCrashTestAlreadyLost410(t *testing.T) {
	ts, svc, _, _ := newCrashServer(t)
	ctx := context.Background()
	l, err := svc.grant(ctx, "u-bob", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.store.mu.Lock()
	l.setState("lost")
	svc.store.mu.Unlock()
	resp, body := doReq(t, "POST", ts.URL+"/api/admin/leases/"+l.ID+"/crash", "operator-tok", nil)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("lost crash = %d (%v), want 410", resp.StatusCode, body)
	}
	if body["error"] != lostLeaseMessage {
		t.Fatalf("error = %v, want %q", body["error"], lostLeaseMessage)
	}
}

// TestCrashTestEventOrder: the crash_test marker precedes the recovery
// event on the lease event stream, for a recovery and for a loss.
func TestCrashTestEventOrder(t *testing.T) {
	ts, svc, _, _ := newCrashServer(t)
	ctx := context.Background()

	// Recovered: checkpointed lease.
	sub := svc.Subscribe(EventFilter{})
	l, err := svc.grant(ctx, "u-bob", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.checkpointLease(ctx, l); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	resp, body := doReq(t, "POST", ts.URL+"/api/admin/leases/"+l.ID+"/crash", "operator-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("crash = %d (%v)", resp.StatusCode, body)
	}

	// Lost: no checkpoint.
	bare, err := svc.grant(ctx, "u-bob", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant bare: %v", err)
	}
	resp, body = doReq(t, "POST", ts.URL+"/api/admin/leases/"+bare.ID+"/crash", "operator-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("crash bare = %d (%v)", resp.StatusCode, body)
	}
	sub.Close()
	all := collectEvents(sub.C)

	assertCrashOrder(t, eventsFor(all, l.ID), LeaseRecovered)
	assertCrashOrder(t, eventsFor(all, bare.ID), LeaseLost)
}

// assertCrashOrder checks that exactly one crash_test event sits
// immediately before the want recovery event, and carries the operator
// detail.
func assertCrashOrder(t *testing.T, events []LeaseEvent, want LeaseEventType) {
	t.Helper()
	var types []LeaseEventType
	for _, ev := range events {
		types = append(types, ev.Type)
	}
	idx := -1
	for i, typ := range types {
		if typ == LeaseCrashTest {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("no crash_test event in %v", types)
	}
	if idx+1 >= len(types) || types[idx+1] != want {
		t.Fatalf("event after crash_test = %v, want %s (events %v)", types[idx+1:], want, types)
	}
	if events[idx].Detail != "crashed by an admin" {
		t.Fatalf("crash_test detail = %q, want %q", events[idx].Detail, "crashed by an admin")
	}
}

// TestCrashTestIdentityAdminToken: an identity-store admin user's token
// drives the crash test too, so an operator can use their own credentials
// with ADMIN_TOKEN configured.
func TestCrashTestIdentityAdminToken(t *testing.T) {
	ts, svc, _, _ := newCrashServer(t)
	ctx := context.Background()
	l, err := svc.grant(ctx, "u-bob", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	resp, body := doReq(t, "POST", ts.URL+"/api/admin/leases/"+l.ID+"/crash", "root-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("identity-admin crash = %d (%v), want 200", resp.StatusCode, body)
	}
	if body["result"] != "lost" {
		t.Fatalf("result = %v, want lost", body["result"])
	}
}
