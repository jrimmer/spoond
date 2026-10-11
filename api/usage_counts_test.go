package api

import (
	"net/http"
	"testing"
)

// addLease inserts a live lease into the in-memory store for counting
// tests. It does not touch the store, so it only serves the handlers
// that read the in-memory map (usage counts, fair shares).
func addLease(svc *Service, id, owner, state string, pinned bool) {
	svc.store.mu.Lock()
	svc.store.leases[id] = &Lease{
		ID: id, Owner: owner, State: state,
		Suspended: state == "suspended", Pinned: pinned,
	}
	svc.store.mu.Unlock()
}

// TestUsageLeaseCounts: GET /api/usage and GET /api/users/me carry the
// caller's lease counts beside "share" (#140 U1): running, paused,
// pinned, lost and running jobs, owner-scoped from the in-memory map.
func TestUsageLeaseCounts(t *testing.T) {
	srv, svc, _, ids := newFairShareServer(t)
	adminTok, adminID := bootstrapAdmin(t, srv)
	other := addIdentityUser(t, ids, "other")
	h := srv.Handler()

	// One owner: 2 running (1 pinned), 1 paused, 1 lost, 1 running job.
	addLease(svc, "run-a", adminID, "running", false)
	addLease(svc, "run-b", adminID, "running", true)
	addLease(svc, "paused-a", adminID, "suspended", false)
	addLease(svc, "lost-a", adminID, "lost", false)
	svc.incRunningJob("run-a")

	// Another owner's leases are not counted.
	addLease(svc, "other-run", other.ID, "running", true)
	addLease(svc, "other-paused", other.ID, "suspended", false)
	addLease(svc, "other-lost", other.ID, "lost", false)
	svc.incRunningJob("other-run")

	want := map[string]float64{
		"running":      2,
		"paused":       1,
		"pinned":       1,
		"lost":         1,
		"running_jobs": 1,
	}

	for _, path := range []string{"/api/usage", "/api/users/me"} {
		rec, body := doUsersReq(t, h, "GET", path, adminTok, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
		}
		leases, ok := body["leases"].(map[string]any)
		if !ok {
			t.Fatalf("GET %s missing leases object: %v", path, body)
		}
		for key, wantN := range want {
			if got := leases[key]; got != wantN {
				t.Fatalf("GET %s leases[%s] = %v, want %v", path, key, got, wantN)
			}
		}
	}
}
