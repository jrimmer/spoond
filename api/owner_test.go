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

// TestOwnerSerialization verifies U2: lease owner is serialized in
// create and list responses, and reflects the identity-store user id
// when the caller is an identity user (falling back to consumer name).
func TestOwnerSerialization(t *testing.T) {
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"legacy-tok": "legacy-consumer"}, 0, 60*time.Second, 10*time.Minute)
	ids, _ := identity.NewStore("")
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(ff, "py-base"))
	h := srv.Handler()

	// Bootstrap an identity user with its own token.
	rec, _ := doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"jason","fingerprints":["SHA256:fp"],"token":"user-tok"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap: %d", rec.Code)
	}

	// Create a lease as the identity user; response must carry owner = user id.
	create := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/sandboxes", strings.NewReader(`{"image":"py-base","ttl":60}`))
	req.Header.Set("Authorization", "Bearer user-tok")
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(create, req)
	if create.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", create.Code, create.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(create.Body.Bytes(), &created)
	owner, ok := created["owner"].(string)
	if !ok || owner == "" {
		t.Fatalf("create response missing owner: %s", create.Body.String())
	}
	if owner == "legacy-consumer" {
		t.Fatalf("owner should be the identity user id, got %q", owner)
	}

	// List response must carry owner too (via Lease struct json tag).
	list := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/api/sandboxes", nil)
	req2.Header.Set("Authorization", "Bearer user-tok")
	h.ServeHTTP(list, req2)
	if list.Code != http.StatusOK {
		t.Fatalf("list: %d", list.Code)
	}
	if !strings.Contains(list.Body.String(), `"owner"`) {
		t.Fatalf("list missing owner field: %s", list.Body.String())
	}
}

// TestOwnerSerializationLegacy verifies legacy consumer-token owners are
// serialized as the consumer name (backward compat).
func TestOwnerSerializationLegacy(t *testing.T) {
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"legacy-tok": "legacy-consumer"}, 0, 60*time.Second, 10*time.Minute)
	srv := NewServer(svc, NewImageRegistry(ff, "py-base"))
	h := srv.Handler()

	create := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/sandboxes", strings.NewReader(`{"image":"py-base","ttl":60}`))
	req.Header.Set("Authorization", "Bearer legacy-tok")
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(create, req)
	if create.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", create.Code, create.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(create.Body.Bytes(), &created)
	if owner, _ := created["owner"].(string); owner != "legacy-consumer" {
		t.Fatalf("legacy owner = %q, want legacy-consumer", owner)
	}
}

// TestOwnerScopedByName verifies the /api/names endpoint is owner-scoped:
// user A cannot resolve user B's named lease.
func TestOwnerScopedByName(t *testing.T) {
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"legacy-tok": "legacy-consumer"}, 0, 60*time.Second, 10*time.Minute)
	ids, _ := identity.NewStore("")
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(ff, "py-base"))
	h := srv.Handler()

	// Bootstrap admin + create a named lease as user-a.
	rec, _ := doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"a","fingerprints":["SHA256:fp-a"],"token":"tok-a"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap a: %d", rec.Code)
	}
	rec, _ = doUsersReq(t, h, "POST", "/api/users", "tok-a", `{"name":"b","fingerprints":["SHA256:fp-b"],"token":"tok-b"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create b: %d", rec.Code)
	}

	create := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/sandboxes", strings.NewReader(`{"image":"py-base","ttl":60,"persistent":true}`))
	req.Header.Set("Authorization", "Bearer tok-a")
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(create, req)
	if create.Code != http.StatusCreated {
		t.Fatalf("create named: %d %s", create.Code, create.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(create.Body.Bytes(), &created)
	leaseID, _ := created["id"].(string)
	if leaseID == "" {
		t.Fatalf("no lease id: %s", create.Body.String())
	}
	// Name the lease via the tag endpoint.
	tag := httptest.NewRecorder()
	reqT := httptest.NewRequest("POST", "/api/sandboxes/"+leaseID+"/tag", strings.NewReader(`{"name":"mylease"}`))
	reqT.Header.Set("Authorization", "Bearer tok-a")
	reqT.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(tag, reqT)
	if tag.Code != http.StatusOK {
		t.Fatalf("tag: %d %s", tag.Code, tag.Body.String())
	}

	// Owner can resolve.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/api/names/mylease", nil)
	req2.Header.Set("Authorization", "Bearer tok-a")
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("owner resolve: %d", rec2.Code)
	}
	// Other user cannot.
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("GET", "/api/names/mylease", nil)
	req3.Header.Set("Authorization", "Bearer tok-b")
	h.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusNotFound {
		t.Fatalf("cross-owner resolve should 404, got %d", rec3.Code)
	}
}
