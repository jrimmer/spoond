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

// newGatewayScopedServer: admin + two users (a, b), gateway token trusted.
func newGatewayScopedServer(t *testing.T) (http.Handler, string) {
	t.Helper()
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"legacy-tok": "legacy-consumer", "gw-tok": "gateway"}, 0, 60*time.Second, 10*time.Minute)
	ids, _ := identity.NewStore("")
	svc.SetIdentities(ids)
	svc.SetGatewayToken("gw-tok")
	srv := NewServer(svc, NewImageRegistry(ff, "py-base"))
	h := srv.Handler()

	// bootstrap admin, then users a + b
	if rec, _ := doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"admin","fingerprints":["SHA256:fp-x"],"token":"admin-tok"}`); rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap: %d", rec.Code)
	}
	if rec, body := doUsersReq(t, h, "POST", "/api/users", "admin-tok", `{"name":"a","fingerprints":["SHA256:fp-a"],"token":"tok-a"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create a: %d", rec.Code)
	} else {
		_ = body
	}
	if rec, _ := doUsersReq(t, h, "POST", "/api/users", "admin-tok", `{"name":"b","fingerprints":["SHA256:fp-b"],"token":"tok-b"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create b: %d", rec.Code)
	}
	return h, "gw-tok"
}

// doUsersReqList is a convenience: list users, fail on non-200.
func doUsersReqList(t *testing.T, h http.Handler) map[string]any {
	t.Helper()
	rec, body := doUsersReq(t, h, "GET", "/api/users", "admin-tok", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	return body
}

// gwCreate creates a lease impersonating the given user via the gateway token.
func gwCreate(h http.Handler, gwTok, userID string) (*httptest.ResponseRecorder, map[string]any) {
	req := httptest.NewRequest("POST", "/api/sandboxes", strings.NewReader(`{"image":"py-base","ttl":60}`))
	req.Header.Set("Authorization", "Bearer "+gwTok)
	if userID != "" {
		req.Header.Set("X-Spoond-User-Id", userID)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var parsed map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
	return rec, parsed
}

func TestGatewayImpersonationCreatesAsUser(t *testing.T) {
	h, gwTok := newGatewayScopedServer(t)
	body := doUsersReqList(t, h)
	var aID string
	for _, u := range body["users"].([]any) {
		m := u.(map[string]any)
		if m["name"] == "a" {
			aID = m["id"].(string)
		}
	}
	if aID == "" {
		t.Fatal("user a not found")
	}
	// impersonate a
	crec, created := gwCreate(h, gwTok, aID)
	if crec.Code != http.StatusCreated {
		t.Fatalf("impersonated create: %d %s", crec.Code, crec.Body.String())
	}
	if owner, _ := created["owner"].(string); owner != aID {
		t.Fatalf("owner = %q, want %q (impersonated user)", owner, aID)
	}
}

func TestGatewayImpersonationUnknownUserRejected(t *testing.T) {
	h, gwTok := newGatewayScopedServer(t)
	crec, _ := gwCreate(h, gwTok, "u-does-not-exist")
	if crec.Code != http.StatusForbidden {
		t.Fatalf("unknown impersonated user should 403, got %d", crec.Code)
	}
}

func TestNonGatewayTokenCannotImpersonate(t *testing.T) {
	h, _ := newGatewayScopedServer(t)
	// a regular user token + X-Spoond-User-Id must be ignored (the
	// request acts as the token's own owner, not the header).
	req := httptest.NewRequest("POST", "/api/sandboxes", strings.NewReader(`{"image":"py-base","ttl":60}`))
	req.Header.Set("Authorization", "Bearer tok-a")
	req.Header.Set("X-Spoond-User-Id", "u-anyone")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("regular token create: %d", rec.Code)
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	// owner must be tok-a's user id, not the header value
	owner, _ := created["owner"].(string)
	if owner == "u-anyone" {
		t.Fatal("non-gateway token must not honor X-Spoond-User-Id")
	}
}

func TestCrossOwnerRmDenied(t *testing.T) {
	h, gwTok := newGatewayScopedServer(t)
	body := doUsersReqList(t, h)
	var aID, bID string
	for _, u := range body["users"].([]any) {
		m := u.(map[string]any)
		switch m["name"] {
		case "a":
			aID = m["id"].(string)
		case "b":
			bID = m["id"].(string)
		}
	}
	// a creates a lease
	crec, created := gwCreate(h, gwTok, aID)
	if crec.Code != http.StatusCreated {
		t.Fatalf("a create: %d", crec.Code)
	}
	leaseID, _ := created["id"].(string)

	// b tries to rm a's lease via gateway impersonation -> 404
	del := httptest.NewRecorder()
	dreq := httptest.NewRequest("DELETE", "/api/sandboxes/"+leaseID, nil)
	dreq.Header.Set("Authorization", "Bearer "+gwTok)
	dreq.Header.Set("X-Spoond-User-Id", bID)
	h.ServeHTTP(del, dreq)
	if del.Code != http.StatusNotFound {
		t.Fatalf("cross-owner rm should 404, got %d %s", del.Code, del.Body.String())
	}

	// a can rm it
	del2 := httptest.NewRecorder()
	dreq2 := httptest.NewRequest("DELETE", "/api/sandboxes/"+leaseID, nil)
	dreq2.Header.Set("Authorization", "Bearer "+gwTok)
	dreq2.Header.Set("X-Spoond-User-Id", aID)
	h.ServeHTTP(del2, dreq2)
	if del2.Code != http.StatusNoContent {
		t.Fatalf("owner rm should 204, got %d", del2.Code)
	}
}
