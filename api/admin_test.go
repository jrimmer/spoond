package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jrimmer/spoond/store"
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

	persistent, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil)
	if err != nil {
		t.Fatalf("grant persistent: %v", err)
	}
	plain, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil)
	if err != nil {
		t.Fatalf("grant plain: %v", err)
	}
	suspended, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil)
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
			t.Fatalf("lease %s paused %d times, want 1 (calls %v)", l.ID, got, sub.Fake.Calls)
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
			t.Fatalf("lease %s: %d creates of its sandbox id, want 2 (grant + resume) (calls %v)", l.ID, got, sub.Fake.Calls)
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
		l, err := svc.grant(ctx, "c", "py-base", time.Minute, i == 0, "", nil)
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
