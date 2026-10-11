package api

import (
	"testing"
)

// TestUsageMemoryUsedMib: GET /api/usage and GET /api/users/me's leases
// object gains memory_used_mib — the summed latest measured usage of the
// caller's running leases (ceil per lease), omitted while nothing is
// measured (#130 R1).
func TestUsageMemoryUsedMib(t *testing.T) {
	srv, svc, _, ids := newFairShareServer(t)
	adminTok, adminID := bootstrapAdmin(t, srv)
	other := addIdentityUser(t, ids, "mem-other")
	h := srv.Handler()

	// Two running leases with data (470 and 560 MiB), one running lease
	// without any (a no-series sandbox), one paused: the sum covers the
	// two with data only: 570 after mem-b's second sample.
	withA := addSandboxedLease(svc, "mem-a", adminID, "sb_ma", 4096)
	withB := addSandboxedLease(svc, "mem-b", adminID, "sb_mb", 4096)
	bare := addSandboxedLease(svc, "mem-bare", adminID, "sb_mbare", 4096)
	addLease(svc, "mem-paused", adminID, "suspended", false)
	seedMemEntries(svc, withA, []float64{470 * 1024 * 1024})
	seedMemEntries(svc, withB, []float64{560 * 1024 * 1024, 100 * 1024 * 1024})
	_ = bare

	for _, path := range []string{"/api/usage", "/api/users/me"} {
		rec, body := doUsersReq(t, h, "GET", path, adminTok, "")
		if rec.Code != 200 {
			t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
		}
		leases := body["leases"].(map[string]any)
		if got := toF(t, leases["memory_used_mib"]); got != 570 {
			t.Errorf("GET %s memory_used_mib = %v, want 570 (470 + the latest 100 for mem-b)", path, got)
		}
	}

	// Another owner's lease never counts.
	addSandboxedLease(svc, "mem-other", other.ID, "sb_mo", 4096)
	seedMemEntries(svc, svc.store.leases["mem-other"], []float64{999 * 1024 * 1024})
	rec, body := doUsersReq(t, h, "GET", "/api/usage", adminTok, "")
	_ = rec
	leases := body["leases"].(map[string]any)
	if got := toF(t, leases["memory_used_mib"]); got != 570 {
		t.Errorf("memory_used_mib after other owner = %v, want 570 (the other owner never counts)", got)
	}
}

// TestUsageMemoryUsedMibOmitted: with no measured lease at all the field
// is absent (the API keeps the shape additive).
func TestUsageMemoryUsedMibOmitted(t *testing.T) {
	srv, svc, _, _ := newFairShareServer(t)
	adminTok, _ := bootstrapAdmin(t, srv)
	h := srv.Handler()
	addLease(svc, "mem-nosample", "consumer-x", "running", false)

	rec, body := doUsersReq(t, h, "GET", "/api/usage", adminTok, "")
	if rec.Code != 200 {
		t.Fatalf("GET /api/usage: %d %s", rec.Code, rec.Body.String())
	}
	leases := body["leases"].(map[string]any)
	if _, ok := leases["memory_used_mib"]; ok {
		t.Errorf("memory_used_mib present without any measurement: %v", leases["memory_used_mib"])
	}
}
