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

// quotaTestUserID returns the id of newQuotaTestServer's user.
func quotaTestUserID(t *testing.T, h http.Handler, tok string) string {
	t.Helper()
	rec, body := doUsersReq(t, h, "GET", "/api/users/me", tok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("users/me: %d %s", rec.Code, rec.Body.String())
	}
	return body["user"].(map[string]any)["id"].(string)
}

// TestQuotaDeprecatedFieldsAccepted: since 3.0 (fair shares, FS4) the
// quota route still ACCEPTS max_leases, guaranteed_mib, max_mib and
// max_kept_bytes — any value, no 400 — but ignores them, and a request
// that sets any of them answers with Deprecation: true and a 299
// Warning. max_ttl still applies.
func TestQuotaDeprecatedFieldsAccepted(t *testing.T) {
	h, tok := newQuotaTestServer(t)
	uid := quotaTestUserID(t, h, tok)

	for _, body := range []string{
		`{"max_ttl":30,"max_leases":3}`,
		`{"max_ttl":30,"guaranteed_mib":8192,"max_mib":4096}`, // the old guaranteed <= max rule is gone
		`{"max_ttl":30,"max_kept_bytes":-1}`,                  // ignored, so not validated
		`{"max_ttl":30,"max_leases":1,"guaranteed_mib":1,"max_mib":1,"max_kept_bytes":1}`,
	} {
		rec, out := doUsersReq(t, h, "POST", "/api/users/"+uid+"/quota", "admin-tok", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d (%s), want 200", body, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Deprecation"); got != "true" {
			t.Fatalf("%s: Deprecation = %q, want true", body, got)
		}
		if got := rec.Header().Get("Warning"); !strings.HasPrefix(got, "299 - ") || !strings.Contains(got, "ignored since 3.0") {
			t.Fatalf("%s: Warning = %q, want the 299 deprecation warning", body, got)
		}
		u := out["user"].(map[string]any)
		if u["max_ttl"] != float64(30) {
			t.Fatalf("%s: max_ttl = %v, want 30", body, u["max_ttl"])
		}
		for _, f := range deprecatedQuotaFields {
			if _, ok := u[f]; ok {
				t.Fatalf("%s: user view still carries %s: %v", body, f, u)
			}
		}
	}

	// Only max_ttl (or the removed fields at zero): no deprecation headers.
	for _, body := range []string{`{"max_ttl":20}`, `{"max_ttl":20,"max_leases":0,"max_mib":0}`} {
		rec, _ := doUsersReq(t, h, "POST", "/api/users/"+uid+"/quota", "admin-tok", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200", body, rec.Code)
		}
		if rec.Header().Get("Deprecation") != "" || rec.Header().Get("Warning") != "" {
			t.Fatalf("%s: unexpected deprecation headers %v", body, rec.Header())
		}
	}

	// A negative max_ttl is still 400.
	if rec, _ := doUsersReq(t, h, "POST", "/api/users/"+uid+"/quota", "admin-tok", `{"max_ttl":-1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative max_ttl = %d, want 400", rec.Code)
	}
}

// TestUserViewsOmitRemovedQuotaFields: GET /api/users, /api/users/me and
// /api/users/{id}, and the lease detail, no longer return max_leases,
// guaranteed_mib, max_mib or max_kept_bytes (3.0 FS4).
func TestUserViewsOmitRemovedQuotaFields(t *testing.T) {
	h, tok := newQuotaTestServer(t)
	uid := quotaTestUserID(t, h, tok)

	check := func(where string, u map[string]any) {
		t.Helper()
		for _, f := range deprecatedQuotaFields {
			if _, ok := u[f]; ok {
				t.Errorf("%s carries %s: %v", where, f, u)
			}
		}
		if _, ok := u["max_ttl"]; !ok {
			t.Errorf("%s lost max_ttl: %v", where, u)
		}
	}
	_, me := doUsersReq(t, h, "GET", "/api/users/me", tok, "")
	check("users/me", me["user"].(map[string]any))
	_, one := doUsersReq(t, h, "GET", "/api/users/"+uid, "admin-tok", "")
	check("users/{id}", one["user"].(map[string]any))
	_, list := doUsersReq(t, h, "GET", "/api/users", "admin-tok", "")
	for _, u := range list["users"].([]any) {
		check("users list", u.(map[string]any))
	}

	req := httptest.NewRequest("POST", "/api/sandboxes", strings.NewReader(`{"image":"py-base","ttl":5}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	det := httptest.NewRecorder()
	dreq := httptest.NewRequest("GET", "/api/sandboxes/"+created["id"].(string), nil)
	dreq.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(det, dreq)
	var detail map[string]any
	_ = json.Unmarshal(det.Body.Bytes(), &detail)
	for _, f := range []string{"guaranteed_mib", "max_mib"} {
		if _, ok := detail[f]; ok {
			t.Errorf("lease detail carries %s: %v", f, detail)
		}
	}
	if _, ok := detail["charged_mib"]; !ok {
		t.Errorf("lease detail lost charged_mib: %v", detail)
	}
}
