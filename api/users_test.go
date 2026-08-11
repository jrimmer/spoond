package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/identity"
)

// newTestServerWithIdentities builds a Server with an identity store and a
// fake forkd client (no real controller).
func newTestServerWithIdentities(t *testing.T) (*Server, *identity.Store) {
	t.Helper()
	fc := &fakeForkd{}
	svc := NewService(fc, map[string]string{"legacy-tok": "legacy-consumer"}, 0, 300*time.Second, 3600*time.Second)
	ids, err := identity.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(fc))
	return srv, ids
}

func doUsersReq(t *testing.T, h http.Handler, method, path, token, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != "" {
		buf.WriteString(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var parsed map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
	return rec, parsed
}

func TestUsersBootstrapFirstIsAdmin(t *testing.T) {
	srv, ids := newTestServerWithIdentities(t)
	h := srv.Handler()

	// Create a user with a legacy token (any authenticated caller can
	// bootstrap the first user).
	rec, body := doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"jason","kind":"person","fingerprints":["SHA256:aaa"],"token":"newuser-tok"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	u := body["user"].(map[string]any)
	if u["admin"] != true {
		t.Fatalf("first user must be admin, got %v", u["admin"])
	}
	if ids.Count() != 1 {
		t.Fatalf("expected 1 user, got %d", ids.Count())
	}

	// Second user is not admin; a non-admin cannot create more.
	rec2, _ := doUsersReq(t, h, "POST", "/api/users", "newuser-tok", `{"name":"alice","kind":"person","fingerprints":["SHA256:bbb"],"token":"alice-tok"}`)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("second create: %d %s", rec2.Code, rec2.Body.String())
	}
	rec3, body3 := doUsersReq(t, h, "POST", "/api/users", "alice-tok", `{"name":"mallory","kind":"person","fingerprints":["SHA256:ccc"]}`)
	if rec3.Code != http.StatusForbidden {
		t.Fatalf("non-admin create should 403, got %d %v", rec3.Code, body3)
	}
	// The admin (first user) can create.
	rec4, _ := doUsersReq(t, h, "POST", "/api/users", "newuser-tok", `{"name":"mallory","kind":"agent","fingerprints":["SHA256:ccc"]}`)
	if rec4.Code != http.StatusCreated {
		t.Fatalf("admin create should work: %d %s", rec4.Code, rec4.Body.String())
	}
}

func TestUsersByKeyResolution(t *testing.T) {
	srv, _ := newTestServerWithIdentities(t)
	h := srv.Handler()
	// bootstrap
	rec, _ := doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"jason","fingerprints":["SHA256:fp1"],"token":"t1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap: %d", rec.Code)
	}

	rec2, body := doUsersReq(t, h, "GET", "/api/users/by-key?fingerprint=SHA256:fp1", "legacy-tok", "")
	if rec2.Code != http.StatusOK {
		t.Fatalf("by-key: %d %s", rec2.Code, rec2.Body.String())
	}
	u := body["user"].(map[string]any)
	if u["name"] != "jason" {
		t.Fatalf("by-key name: %v", u["name"])
	}

	rec3, _ := doUsersReq(t, h, "GET", "/api/users/by-key?fingerprint=SHA256:nope", "legacy-tok", "")
	if rec3.Code != http.StatusNotFound {
		t.Fatalf("unknown key should 404, got %d", rec3.Code)
	}
}

func TestUsersTokenResolutionAndLegacyFallback(t *testing.T) {
	srv, _ := newTestServerWithIdentities(t)
	h := srv.Handler()
	// bootstrap with a user token
	rec, _ := doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"jason","fingerprints":["SHA256:fp"],"token":"user-tok"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap: %d", rec.Code)
	}
	// user token resolves (owner = user id)
	rec2, _ := doUsersReq(t, h, "GET", "/api/sandboxes", "user-tok", "")
	if rec2.Code != http.StatusOK {
		t.Fatalf("user token auth: %d", rec2.Code)
	}
	// legacy token still resolves (backward compat)
	rec3, _ := doUsersReq(t, h, "GET", "/api/sandboxes", "legacy-tok", "")
	if rec3.Code != http.StatusOK {
		t.Fatalf("legacy token auth: %d", rec3.Code)
	}
	// unknown token rejected
	rec4, _ := doUsersReq(t, h, "GET", "/api/sandboxes", "nope", "")
	if rec4.Code != http.StatusUnauthorized {
		t.Fatalf("unknown token should 401, got %d", rec4.Code)
	}
}

func TestUsersDeleteRequiresAdmin(t *testing.T) {
	srv, _ := newTestServerWithIdentities(t)
	h := srv.Handler()
	// bootstrap admin jason (token t1), then alice (token t2)
	if rec, _ := doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"jason","fingerprints":["SHA256:fp1"],"token":"t1"}`); rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap: %d", rec.Code)
	}
	rec2, body2 := doUsersReq(t, h, "POST", "/api/users", "t1", `{"name":"alice","fingerprints":["SHA256:fp2"],"token":"t2"}`)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("alice create: %d %s", rec2.Code, rec2.Body.String())
	}
	aliceID := body2["user"].(map[string]any)["id"].(string)

	// alice (non-admin) cannot delete jason
	rec3, _ := doUsersReq(t, h, "DELETE", "/api/users/"+aliceID, "t2", "")
	if rec3.Code != http.StatusForbidden {
		t.Fatalf("non-admin delete should 403, got %d", rec3.Code)
	}
	// jason (admin) can delete alice
	rec4 := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/api/users/"+aliceID, nil)
	req.Header.Set("Authorization", "Bearer t1")
	h.ServeHTTP(rec4, req)
	if rec4.Code != http.StatusNoContent {
		t.Fatalf("admin delete: %d %s", rec4.Code, rec4.Body.String())
	}
}

func TestUsersDuplicateRejected(t *testing.T) {
	srv, _ := newTestServerWithIdentities(t)
	h := srv.Handler()
	if rec, _ := doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"jason","fingerprints":["SHA256:fp"],"token":"t1"}`); rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap: %d", rec.Code)
	}
	// duplicate name
	rec2, body2 := doUsersReq(t, h, "POST", "/api/users", "t1", `{"name":"jason","fingerprints":["SHA256:fp2"],"token":"t2"}`)
	if rec2.Code != http.StatusConflict {
		t.Fatalf("duplicate name should 409, got %d %v", rec2.Code, body2)
	}
	// duplicate key
	rec3, _ := doUsersReq(t, h, "POST", "/api/users", "t1", `{"name":"bob","fingerprints":["SHA256:fp"],"token":"t3"}`)
	if rec3.Code != http.StatusConflict {
		t.Fatalf("duplicate key should 409, got %d", rec3.Code)
	}
}

func TestUsersListShowsUsers(t *testing.T) {
	srv, _ := newTestServerWithIdentities(t)
	h := srv.Handler()
	if rec, _ := doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"jason","fingerprints":["SHA256:fp1"],"token":"t1"}`); rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap: %d", rec.Code)
	}
	rec, body := doUsersReq(t, h, "GET", "/api/users", "t1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	users, ok := body["users"].([]any)
	if !ok || len(users) != 1 {
		t.Fatalf("list users: %v", body)
	}
	// token hash must never be serialized
	raw := rec.Body.String()
	if strings.Contains(raw, "token_hash") {
		t.Fatalf("token hash leaked in response: %s", raw)
	}
}
