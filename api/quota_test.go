package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/identity"
)

// newQuotaTestServer bootstraps admin (legacy token) + a limited user.
func newQuotaTestServer(t *testing.T) (http.Handler, string) {
	t.Helper()
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"legacy-tok": "legacy-consumer"}, 0, 60*time.Second, 3600*time.Second)
	ids, _ := identity.NewStore("")
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(ff, "py-base"))
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
	// set quota: max 2 leases, max TTL 10s
	rec2, _ := doUsersReq(t, h, "POST", "/api/users/"+uid+"/quota", "admin-tok", `{"max_leases":2,"max_ttl":10}`)
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

func TestQuotaCapReturns429(t *testing.T) {
	h, tok := newQuotaTestServer(t)
	// two leases fit under the cap of 2
	if c := createAs(h, tok); c != http.StatusCreated {
		t.Fatalf("first create: %d", c)
	}
	if c := createAs(h, tok); c != http.StatusCreated {
		t.Fatalf("second create: %d", c)
	}
	// third must be 429
	if c := createAs(h, tok); c != http.StatusTooManyRequests {
		t.Fatalf("third create should 429, got %d", c)
	}
}

func TestQuotaReleaseFreesCap(t *testing.T) {
	h, tok := newQuotaTestServer(t)
	// create 2, delete 1, create again OK
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
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("no id: %s", rec.Body.String())
	}
	if c := createAs(h, tok); c != http.StatusCreated {
		t.Fatalf("second create: %d", c)
	}
	// delete one
	del := httptest.NewRecorder()
	dreq := httptest.NewRequest("DELETE", "/api/sandboxes/"+id, nil)
	dreq.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(del, dreq)
	if del.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", del.Code)
	}
	// cap freed
	if c := createAs(h, tok); c != http.StatusCreated {
		t.Fatalf("create after release should 201, got %d", c)
	}
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
	req := httptest.NewRequest("POST", "/api/users/u-nope/quota", strings.NewReader(`{"max_leases":5}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin quota set should 403, got %d", rec.Code)
	}
}

func TestQuotaUncappedLegacyConsumer(t *testing.T) {
	// legacy consumer-token callers are never capped
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"legacy-tok": "legacy-consumer"}, 0, 60*time.Second, 3600*time.Second)
	srv := NewServer(svc, NewImageRegistry(ff, "py-base"))
	h := srv.Handler()
	for i := 0; i < 5; i++ {
		if c := createAs(h, "legacy-tok"); c != http.StatusCreated {
			t.Fatalf("legacy create %d: %d", i, c)
		}
	}
}
