package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v3/identity"
	"github.com/jrimmer/spoond/v3/store"
)

// newAdminServer builds a lease API server with an admin token and
// exposes the service (drain tests drive both the HTTP surface and the
// leases directly).
func newAdminServer(t *testing.T, token string) (*httptest.Server, *Service, *store.DB, *testSub) {
	t.Helper()
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	srv := NewServer(svc, NewImageRegistry(db))
	srv.SetAdminToken(token)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, svc, db, sub
}

// TestAdminAuth pins the admin endpoint contract: the routes answer 404
// with no ADMIN_TOKEN configured, 401 on a wrong or missing token, and
// 200 with the configured one.
func TestAdminAuth(t *testing.T) {
	t.Run("no token configured: 404", func(t *testing.T) {
		ts, _, _, _ := newAdminServer(t, "")
		for _, path := range []string{"/api/admin/drain", "/api/admin/undrain", "/api/admin/reconcile"} {
			resp, body := doReq(t, "POST", ts.URL+path, "admin-tok", nil)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s with unconfigured admin token = %d (%v), want 404", path, resp.StatusCode, body)
			}
		}
	})
	t.Run("wrong and missing tokens: 401", func(t *testing.T) {
		ts, _, _, _ := newAdminServer(t, "admin-tok")
		resp, _ := doReq(t, "POST", ts.URL+"/api/admin/drain", "wrong-tok", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong admin token = %d, want 401", resp.StatusCode)
		}
		resp, _ = doReq(t, "POST", ts.URL+"/api/admin/drain", "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("missing admin token = %d, want 401", resp.StatusCode)
		}
	})
	t.Run("correct token: 200", func(t *testing.T) {
		ts, _, _, _ := newAdminServer(t, "admin-tok")
		resp, body := doReq(t, "POST", ts.URL+"/api/admin/drain", "admin-tok", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("drain with admin token = %d (%v), want 200", resp.StatusCode, body)
		}
	})
}

// TestDrainUndrainRoundTrip: drain pauses every live lease (persistent
// or not) and marks it Drained; undrain resumes exactly those. A lease
// that was already suspended is not live and must not be touched.
func TestDrainUndrainRoundTrip(t *testing.T) {
	ts, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()

	persistent, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant persistent: %v", err)
	}
	plain, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant plain: %v", err)
	}
	suspended, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant suspended: %v", err)
	}
	if _, err := svc.suspend(ctx, "c", suspended.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	suspResumeBuild := suspended.ResumeBuildID

	resp, body := doReq(t, "POST", ts.URL+"/api/admin/drain", "admin-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("drain = %d (%v), want 200", resp.StatusCode, body)
	}
	if body["paused"] != float64(2) {
		t.Fatalf("drain paused = %v, want 2", body["paused"])
	}
	if body["quiesced"] != true {
		t.Fatalf("drain quiesced = %v, want true", body["quiesced"])
	}
	if failed, ok := body["failed"].([]any); !ok || len(failed) != 0 {
		t.Fatalf("drain failed = %v, want an empty list", body["failed"])
	}

	for _, l := range []*Lease{persistent, plain} {
		if !l.Drained {
			t.Fatalf("lease %s not marked Drained", l.ID)
		}
		if l.State != "suspended" || !l.Suspended || l.ResumeBuildID == "" {
			t.Fatalf("drained lease %s = %+v, want suspended with a resume build", l.ID, l)
		}
		if got := calls(sub.Fake, "Pause "+l.SandboxID); got != 1 {
			t.Fatalf("lease %s paused %d times, want 1 (calls %v)", l.ID, got, sub.Fake.CallLog())
		}
	}
	if suspended.Drained {
		t.Fatal("already-suspended lease must not be drained")
	}
	if suspended.ResumeBuildID != suspResumeBuild {
		t.Fatal("already-suspended lease was paused again")
	}
	if ids := sub.sandboxesLive(t); len(ids) != 0 {
		t.Fatalf("sandboxes still live after drain: %v", ids)
	}

	// Undrain resumes exactly the drained leases.
	resp, body = doReq(t, "POST", ts.URL+"/api/admin/undrain", "admin-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("undrain = %d (%v), want 200", resp.StatusCode, body)
	}
	if body["resumed"] != float64(2) {
		t.Fatalf("undrain resumed = %v, want 2", body["resumed"])
	}
	if failed, ok := body["failed"].([]any); !ok || len(failed) != 0 {
		t.Fatalf("undrain failed = %v, want an empty list", body["failed"])
	}
	for _, l := range []*Lease{persistent, plain} {
		if l.Drained {
			t.Fatalf("undrained lease %s still marked Drained", l.ID)
		}
		if !l.live() {
			t.Fatalf("undrained lease %s = %+v, want live", l.ID, l)
		}
		// The resume is a snapshot create with the same sandbox id: the
		// fake saw the id twice, on the grant and on the resume.
		if got := calls(sub.Fake, "Create "+l.SandboxID); got != 2 {
			t.Fatalf("lease %s: %d creates of its sandbox id, want 2 (grant + resume) (calls %v)", l.ID, got, sub.Fake.CallLog())
		}
	}
	if suspended.Drained || !suspended.Suspended {
		t.Fatalf("undrain touched the already-suspended lease: %+v", suspended)
	}
	if svc.draining.Load() {
		t.Fatal("undrain left the service draining")
	}
}

// TestDrainContinuesPastFailedPause: one lease's pause fails, the drain
// records the failure and still pauses the rest.
func TestDrainContinuesPastFailedPause(t *testing.T) {
	ts, svc, _, sub := newAdminServer(t, "admin-tok")
	ctx := context.Background()

	var leases []*Lease
	for i := range 3 {
		l, err := svc.grant(ctx, "c", "py-base", time.Minute, i == 0, "", nil, "", "", nil)
		if err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
		leases = append(leases, l)
	}
	// The 2nd Pause call fails, whichever lease it lands on.
	sub.FailCall("Pause", 2, errors.New("snapshot failed"))

	resp, body := doReq(t, "POST", ts.URL+"/api/admin/drain", "admin-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("drain = %d (%v), want 200", resp.StatusCode, body)
	}
	if body["paused"] != float64(2) {
		t.Fatalf("drain paused = %v, want 2", body["paused"])
	}
	failed, ok := body["failed"].([]any)
	if !ok || len(failed) != 1 {
		t.Fatalf("drain failed = %v, want exactly one entry", body["failed"])
	}
	f := failed[0].(map[string]any)
	if f["error"] != "snapshot failed" || f["id"] == "" {
		t.Fatalf("failed entry = %v, want {id, error}", f)
	}

	var drainedCount, liveCount int
	for _, l := range leases {
		if l.Drained {
			drainedCount++
		}
		if l.live() {
			liveCount++
		}
	}
	if drainedCount != 2 || liveCount != 1 {
		t.Fatalf("after drain: %d drained, %d live, want 2 and 1", drainedCount, liveCount)
	}

	// Undrain resumes the two that made it; the still-live one was never
	// drained and must not be touched.
	resp, body = doReq(t, "POST", ts.URL+"/api/admin/undrain", "admin-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("undrain = %d (%v), want 200", resp.StatusCode, body)
	}
	if body["resumed"] != float64(2) {
		t.Fatalf("undrain resumed = %v, want 2", body["resumed"])
	}
	if failed, ok := body["failed"].([]any); !ok || len(failed) != 0 {
		t.Fatalf("undrain failed = %v, want an empty list", body["failed"])
	}
}

// newAdminServerWithIdentities builds a lease API server with an admin
// token and an identity store holding an admin user ("root", token
// user-admin-tok) and a non-admin ("alice", token user-alice-tok).
func newAdminServerWithIdentities(t *testing.T) (*httptest.Server, *Service, *identity.Store) {
	t.Helper()
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ids, err := identity.NewStore("")
	if err != nil {
		t.Fatalf("identity store: %v", err)
	}
	// The first user is admin (KTD-2); the second is not.
	if _, err := ids.AddUser("root", identity.KindAgent, nil, "user-admin-tok"); err != nil {
		t.Fatalf("admin user: %v", err)
	}
	if _, err := ids.AddUser("alice", identity.KindPerson, nil, "user-alice-tok"); err != nil {
		t.Fatalf("non-admin user: %v", err)
	}
	if u := ids.UserByName("root"); u == nil || !u.Admin {
		t.Fatal("first identity user must be admin")
	}
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(db))
	srv.SetAdminToken("admin-tok")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	_ = sub
	return ts, svc, ids
}

// mustUser returns the named user from the identity store, failing the
// test when absent.
func mustUser(t *testing.T, ids *identity.Store, name string) *identity.User {
	t.Helper()
	u := ids.UserByName(name)
	if u == nil {
		t.Fatalf("user %s not found", name)
	}
	return u
}

// TestAdminAuthAdminUserToken (#138 A1): an identity-store admin user's
// bearer token reaches /api/admin/* — with and without ADMIN_TOKEN
// configured; a non-admin user's token stays refused (401, the same as
// today); and ADMIN_TOKEN keeps working exactly as before.
func TestAdminAuthAdminUserToken(t *testing.T) {
	t.Run("admin user token with ADMIN_TOKEN configured: allowed", func(t *testing.T) {
		ts, _, _ := newAdminServerWithIdentities(t)
		for _, tok := range []string{"admin-tok", "user-admin-tok"} {
			resp, body := doReq(t, "POST", ts.URL+"/api/admin/reconcile", tok, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("token %q reconcile = %d (%v), want 200", tok, resp.StatusCode, body)
			}
		}
	})
	t.Run("admin user token without ADMIN_TOKEN configured: allowed", func(t *testing.T) {
		svc, db, _ := newTestService(t)
		seedImage(t, db, "py-base", 2048)
		ids, err := identity.NewStore("")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ids.AddUser("root", identity.KindAgent, nil, "user-admin-tok"); err != nil {
			t.Fatal(err)
		}
		svc.SetIdentities(ids)
		srv := NewServer(svc, NewImageRegistry(db))
		// No SetAdminToken call: the env ADMIN_TOKEN is unset.
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)
		resp, body := doReq(t, "POST", ts.URL+"/api/admin/reconcile", "user-admin-tok", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("admin user reconcile without ADMIN_TOKEN = %d (%v), want 200", resp.StatusCode, body)
		}
		// The routes stay disabled for everyone else.
		resp, _ = doReq(t, "POST", ts.URL+"/api/admin/reconcile", "wrong-tok", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("wrong token with no ADMIN_TOKEN = %d, want 404", resp.StatusCode)
		}
	})
	t.Run("non-admin user token: 401 as today", func(t *testing.T) {
		ts, _, ids := newAdminServerWithIdentities(t)
		resp, _ := doReq(t, "POST", ts.URL+"/api/admin/reconcile", "user-alice-tok", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("non-admin user token = %d, want 401", resp.StatusCode)
		}
		// A revoked (removed) user's token is refused the same way.
		if err := ids.RemoveUser(mustUser(t, ids, "alice").ID); err != nil {
			t.Fatalf("remove user: %v", err)
		}
		resp, _ = doReq(t, "POST", ts.URL+"/api/admin/reconcile", "user-alice-tok", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("removed user token = %d, want 401", resp.StatusCode)
		}
	})
	t.Run("ADMIN_TOKEN still works among identity users", func(t *testing.T) {
		ts, _, _ := newAdminServerWithIdentities(t)
		resp, body := doReq(t, "POST", ts.URL+"/api/admin/drain", "admin-tok", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("ADMIN_TOKEN drain = %d (%v), want 200", resp.StatusCode, body)
		}
	})
	t.Run("no identity store: ADMIN_TOKEN behavior unchanged", func(t *testing.T) {
		ts, _, _, _ := newAdminServer(t, "admin-tok")
		resp, _ := doReq(t, "POST", ts.URL+"/api/admin/reconcile", "some-user-tok", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unknown token with no identity store = %d, want 401", resp.StatusCode)
		}
		resp, body := doReq(t, "POST", ts.URL+"/api/admin/reconcile", "admin-tok", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("ADMIN_TOKEN with no identity store = %d (%v), want 200", resp.StatusCode, body)
		}
	})
}

// TestAdminDrainUndrainAsAdminUser runs a full drain/undrain round trip
// authenticated as an identity-store admin user rather than ADMIN_TOKEN,
// so the #138 A1 path is exercised through the Service and not only by
// the reconcile 200 above.
func TestAdminDrainUndrainAsAdminUser(t *testing.T) {
	ts, svc, ids := newAdminServerWithIdentities(t)
	ctx := context.Background()

	// A second admin, so the caller is not merely the store's first user.
	second, err := ids.AddUser("root2", identity.KindAgent, nil, "user-root2-tok")
	if err != nil {
		t.Fatalf("second admin: %v", err)
	}
	second.Admin = true
	// The store has no direct admin setter; SetQuota persists the user
	// (saving the in-memory struct with Admin=true) and leaves MaxTTL
	// at its default.
	if err := ids.SetQuota(second.ID, second.MaxTTL); err != nil {
		t.Fatalf("persist second admin: %v", err)
	}

	if _, err := svc.grant(ctx, "consumer-a", "py-base", time.Minute, true, "", nil, "", "", nil); err != nil {
		t.Fatalf("grant: %v", err)
	}

	resp, body := doReq(t, "POST", ts.URL+"/api/admin/drain", "user-root2-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("drain as admin user = %d (%v), want 200", resp.StatusCode, body)
	}
	if body["paused"] != float64(1) {
		t.Fatalf("drain paused = %v, want 1", body["paused"])
	}
	resp, body = doReq(t, "POST", ts.URL+"/api/admin/undrain", "user-root2-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("undrain as admin user = %d (%v), want 200", resp.StatusCode, body)
	}
	if body["resumed"] != float64(1) {
		t.Fatalf("undrain resumed = %v, want 1", body["resumed"])
	}
}
