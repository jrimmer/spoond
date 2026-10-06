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

// newCrashServer builds a lease API server with the crash test set to
// enabled, plus an identity store holding an admin ("admin-tok") and a
// plain user ("plain-tok"). Leases are granted to consumer-a, whose
// legacy token is "token-a"; "token-b" (consumer-b) is another
// non-admin caller.
func newCrashServer(t *testing.T, enabled bool) (*httptest.Server, *Service, *store.DB, *testSub) {
	t.Helper()
	svc, db, sub := newTestService(t)
	svc.cfg.CrashTest = enabled
	seedImage(t, db, "py-base", 2048)
	ids, err := identity.NewStore("")
	if err != nil {
		t.Fatalf("identity store: %v", err)
	}
	// The first user is an admin; the second a plain user.
	if _, err := ids.AddUser("root", identity.KindAgent, []string{"SHA256:fp-root"}, "admin-tok"); err != nil {
		t.Fatalf("admin user: %v", err)
	}
	if _, err := ids.AddUser("plain", identity.KindAgent, []string{"SHA256:fp-plain"}, "plain-tok"); err != nil {
		t.Fatalf("plain user: %v", err)
	}
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(db))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, svc, db, sub
}

// crashURL is the crash-test route for a lease on the primary path.
func crashURL(ts *httptest.Server, id string) string {
	return ts.URL + "/api/leases/" + id + "/crash-test"
}

// grantCrash grants consumer-a a lease for a crash test.
func grantCrash(t *testing.T, svc *Service, persistent bool) *Lease {
	t.Helper()
	l, err := svc.grant(context.Background(), "consumer-a", "py-base", time.Minute, persistent, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	return l
}

// TestCrashTestDisabled404: with CRASH_TEST off the route answers 404
// "not found", like an unknown route, even for the lease's owner and an
// admin, and the lease is untouched.
func TestCrashTestDisabled404(t *testing.T) {
	ts, svc, _, sub := newCrashServer(t, false)
	l := grantCrash(t, svc, false)
	for _, tok := range []string{"token-a", "admin-tok"} {
		resp, body := doReq(t, "POST", crashURL(ts, l.ID), tok, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("disabled crash by %s = %d (%v), want 404", tok, resp.StatusCode, body)
		}
		if body["error"] != "not found" {
			t.Fatalf("disabled crash error = %v, want %q", body["error"], "not found")
		}
	}
	if l.State != "running" {
		t.Fatalf("lease state = %q after a disabled crash, want running", l.State)
	}
	if got := calls(sub.Fake, "Delete "+l.SandboxID); got != 0 {
		t.Fatalf("Delete calls = %d after a disabled crash, want 0", got)
	}
}

// TestCrashTestRecoversFromCheckpoint: the owner crashes their own
// lease; with a checkpoint it recovers from it, generation +1, the
// checkpoint build in use, and a fresh sandbox.
func TestCrashTestRecoversFromCheckpoint(t *testing.T) {
	ts, svc, _, sub := newCrashServer(t, true)
	ctx := context.Background()

	l := grantCrash(t, svc, true)
	if _, err := svc.checkpointLease(ctx, l); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	buildBefore := l.LastCheckpointBuildID
	if buildBefore == "" {
		t.Fatal("checkpoint left no build id")
	}
	oldSandbox := l.SandboxID

	resp, body := doReq(t, "POST", crashURL(ts, l.ID), "token-a", nil)
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

// TestCrashTestLostWithoutCheckpoint: the owner crashes a lease with no
// checkpoint; it is marked lost and its stale sandbox row goes. Driven
// through the /api/sandboxes alias.
func TestCrashTestLostWithoutCheckpoint(t *testing.T) {
	ts, svc, db, _ := newCrashServer(t, true)
	ctx := context.Background()

	l := grantCrash(t, svc, false)
	if _, err := db.GetSandbox(ctx, l.SandboxID); err != nil {
		t.Fatalf("sandbox row missing before crash: %v", err)
	}

	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+l.ID+"/crash-test", "token-a", nil)
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

// TestCrashTestOtherUser404: a caller who is neither the owner nor an
// admin gets the same 404 as the other lease routes, and the lease is
// untouched — both for another legacy consumer and a plain identity user.
func TestCrashTestOtherUser404(t *testing.T) {
	ts, svc, _, sub := newCrashServer(t, true)
	l := grantCrash(t, svc, false)
	for _, tok := range []string{"token-b", "plain-tok"} {
		resp, body := doReq(t, "POST", crashURL(ts, l.ID), tok, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("crash by %s = %d (%v), want 404", tok, resp.StatusCode, body)
		}
		if body["error"] != "lease not found" {
			t.Fatalf("crash by %s error = %v, want %q", tok, body["error"], "lease not found")
		}
	}
	if l.State != "running" {
		t.Fatalf("lease state = %q after a refused crash, want running", l.State)
	}
	if got := calls(sub.Fake, "Delete "+l.SandboxID); got != 0 {
		t.Fatalf("Delete calls = %d after a refused crash, want 0", got)
	}
}

// TestCrashTestAdminAnyLease: an admin crashes a lease they do not own.
func TestCrashTestAdminAnyLease(t *testing.T) {
	ts, svc, _, _ := newCrashServer(t, true)
	l := grantCrash(t, svc, false)
	resp, body := doReq(t, "POST", crashURL(ts, l.ID), "admin-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin crash = %d (%v), want 200", resp.StatusCode, body)
	}
	if body["result"] != "lost" {
		t.Fatalf("result = %v, want lost", body["result"])
	}
}

// TestCrashTestUnknownLease404: an unknown lease answers 404, for the
// owner and an admin alike; so does a released one.
func TestCrashTestUnknownLease404(t *testing.T) {
	ts, svc, _, _ := newCrashServer(t, true)
	for _, tok := range []string{"token-a", "admin-tok"} {
		resp, body := doReq(t, "POST", crashURL(ts, "deadbeef"), tok, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown lease crash by %s = %d (%v), want 404", tok, resp.StatusCode, body)
		}
	}
	l := grantCrash(t, svc, false)
	if resp, body := doReq(t, "DELETE", ts.URL+"/api/leases/"+l.ID, "token-a", nil); resp.StatusCode >= 300 {
		t.Fatalf("release = %d (%v)", resp.StatusCode, body)
	}
	resp, body := doReq(t, "POST", crashURL(ts, l.ID), "token-a", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("released lease crash = %d (%v), want 404", resp.StatusCode, body)
	}
}

// TestCrashTestSuspended409: a suspended lease has nothing running to
// crash.
func TestCrashTestSuspended409(t *testing.T) {
	ts, svc, _, _ := newCrashServer(t, true)
	l := grantCrash(t, svc, true)
	if _, err := svc.suspend(context.Background(), "consumer-a", l.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	resp, body := doReq(t, "POST", crashURL(ts, l.ID), "token-a", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("suspended crash = %d (%v), want 409", resp.StatusCode, body)
	}
	if l.State != "suspended" {
		t.Fatalf("lease state = %q, want suspended", l.State)
	}
}

// TestCrashTestBusy409: a busy lease is refused.
func TestCrashTestBusy409(t *testing.T) {
	ts, svc, _, _ := newCrashServer(t, true)
	l := grantCrash(t, svc, false)
	svc.store.mu.Lock()
	l.busy = true
	svc.store.mu.Unlock()
	t.Cleanup(func() {
		svc.store.mu.Lock()
		l.busy = false
		svc.store.mu.Unlock()
	})
	resp, body := doReq(t, "POST", crashURL(ts, l.ID), "token-a", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("busy crash = %d (%v), want 409", resp.StatusCode, body)
	}
}

// TestCrashTestAlreadyLost410: a lease already lost answers 410.
func TestCrashTestAlreadyLost410(t *testing.T) {
	ts, svc, _, _ := newCrashServer(t, true)
	l := grantCrash(t, svc, false)
	svc.store.mu.Lock()
	l.setState("lost")
	svc.store.mu.Unlock()
	resp, body := doReq(t, "POST", crashURL(ts, l.ID), "token-a", nil)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("lost crash = %d (%v), want 410", resp.StatusCode, body)
	}
	if body["error"] != lostLeaseMessage {
		t.Fatalf("error = %v, want %q", body["error"], lostLeaseMessage)
	}
}

// TestCrashTestEventOrder: the crash_test marker precedes the recovery
// event on the lease event stream, for a recovery and for a loss, and
// its detail names who crashed the lease: the owner or an admin.
func TestCrashTestEventOrder(t *testing.T) {
	ts, svc, _, _ := newCrashServer(t, true)
	ctx := context.Background()
	sub := svc.Subscribe(EventFilter{})

	// Recovered: a checkpointed lease, crashed by its owner.
	l := grantCrash(t, svc, true)
	if _, err := svc.checkpointLease(ctx, l); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	resp, body := doReq(t, "POST", crashURL(ts, l.ID), "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("crash = %d (%v)", resp.StatusCode, body)
	}

	// Lost: no checkpoint, crashed by an admin.
	bare := grantCrash(t, svc, false)
	resp, body = doReq(t, "POST", crashURL(ts, bare.ID), "admin-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("crash bare = %d (%v)", resp.StatusCode, body)
	}
	sub.Close()
	all := collectEvents(sub.C)

	assertCrashOrder(t, eventsFor(all, l.ID), LeaseRecovered, "crashed by its owner")
	assertCrashOrder(t, eventsFor(all, bare.ID), LeaseLost, "crashed by an admin")
}

// assertCrashOrder checks that exactly one crash_test event sits
// immediately before the want recovery event and carries detail.
func assertCrashOrder(t *testing.T, events []LeaseEvent, want LeaseEventType, detail string) {
	t.Helper()
	var types []LeaseEventType
	idx := -1
	for i, ev := range events {
		types = append(types, ev.Type)
		if ev.Type == LeaseCrashTest {
			if idx >= 0 {
				t.Fatalf("more than one crash_test event in %v", types)
			}
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("no crash_test event in %v", types)
	}
	if idx+1 >= len(types) || types[idx+1] != want {
		t.Fatalf("event after crash_test = %v, want %s (events %v)", types[idx+1:], want, types)
	}
	if events[idx].Detail != detail {
		t.Fatalf("crash_test detail = %q, want %q", events[idx].Detail, detail)
	}
}
