package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jrimmer/spoond/v2/identity"
)

// newQuotaTestServer bootstraps admin (legacy token) + a user whose
// max_ttl is 10s (the one quota knob left since 3.0).
func newQuotaTestServer(t *testing.T) (http.Handler, string) {
	t.Helper()
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ids, _ := identity.NewStore("")
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(db))
	h := srv.Handler()

	// bootstrap admin
	rec, _ := doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"admin","fingerprints":["SHA256:fp-a"],"token":"admin-tok"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap: %d", rec.Code)
	}
	// create limited user
	rec, body := doUsersReq(t, h, "POST", "/api/users", "admin-tok", `{"name":"limited","fingerprints":["SHA256:fp-b"],"token":"lim-tok"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create limited: %d", rec.Code)
	}
	uid := body["user"].(map[string]any)["id"].(string)
	// set quota: max TTL 10s
	rec2, _ := doUsersReq(t, h, "POST", "/api/users/"+uid+"/quota", "admin-tok", `{"max_ttl":10}`)
	if rec2.Code != http.StatusOK {
		t.Fatalf("set quota: %d %s", rec2.Code, rec2.Body.String())
	}
	return h, "lim-tok"
}

func createAs(h http.Handler, token string) int {
	req := httptest.NewRequest("POST", "/api/sandboxes", strings.NewReader(`{"image":"py-base","ttl":60}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestQuotaTTLClamp(t *testing.T) {
	h, tok := newQuotaTestServer(t)
	// user max_ttl = 10s; request 60s must clamp to 10s
	req := httptest.NewRequest("POST", "/api/sandboxes", strings.NewReader(`{"image":"py-base","ttl":60}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d", rec.Code)
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	ttl, _ := created["ttl"].(float64)
	if int(ttl) != 10 {
		t.Fatalf("ttl should clamp to 10, got %v", ttl)
	}
}

func TestQuotaAdminOnly(t *testing.T) {
	h, tok := newQuotaTestServer(t)
	// non-admin cannot set quota
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/users/u-nope/quota", strings.NewReader(`{"max_ttl":5}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin quota set should 403, got %d", rec.Code)
	}
}

func TestQuotaUncappedLegacyConsumer(t *testing.T) {
	// legacy consumer-token callers are never capped
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	srv := NewServer(svc, NewImageRegistry(db))
	h := srv.Handler()
	for i := 0; i < 5; i++ {
		if c := createAs(h, "legacy-tok"); c != http.StatusCreated {
			t.Fatalf("legacy create %d: %d", i, c)
		}
	}
}
