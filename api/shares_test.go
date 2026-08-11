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

// newShareTestServer: admin + users a (owner) + b (grantee).
func newShareTestServer(t *testing.T) (http.Handler, string, string) {
	t.Helper()
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"legacy-tok": "legacy-consumer"}, 0, 60*time.Second, 10*time.Minute)
	ids, _ := identity.NewStore("")
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(ff, "py-base"))
	h := srv.Handler()

	if rec, _ := doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"admin","fingerprints":["SHA256:fp-x"],"token":"admin-tok"}`); rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap: %d", rec.Code)
	}
	if rec, _ := doUsersReq(t, h, "POST", "/api/users", "admin-tok", `{"name":"a","fingerprints":["SHA256:fp-a"],"token":"tok-a"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create a: %d", rec.Code)
	}
	if rec, body := doUsersReq(t, h, "POST", "/api/users", "admin-tok", `{"name":"b","fingerprints":["SHA256:fp-b"],"token":"tok-b"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create b: %d", rec.Code)
	} else {
		_ = body
	}
	// get user ids
	_, users := doUsersReq(t, h, "GET", "/api/users", "admin-tok", "")
	var aID, bID string
	for _, u := range users["users"].([]any) {
		m := u.(map[string]any)
		switch m["name"] {
		case "a":
			aID = m["id"].(string)
		case "b":
			bID = m["id"].(string)
		}
	}
	return h, aID, bID
}

// createLeaseAs creates a lease with a token and returns its id.
func createLeaseAs(h http.Handler, token string) string {
	req := httptest.NewRequest("POST", "/api/sandboxes", strings.NewReader(`{"image":"py-base","ttl":120}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		return ""
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id, _ := created["id"].(string)
	return id
}

// execAs runs an exec on a lease with a token, returns status code.
func execAs(h http.Handler, token, leaseID string) int {
	req := httptest.NewRequest("POST", "/api/sandboxes/"+leaseID+"/exec", strings.NewReader(`{"cmd":"echo hi"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestShareGrantEnablesExec(t *testing.T) {
	h, aID, bID := newShareTestServer(t)
	lid := createLeaseAs(h, "tok-a")
	if lid == "" {
		t.Fatal("a could not create lease")
	}
	// b cannot exec before share
	if c := execAs(h, "tok-b", lid); c != http.StatusNotFound {
		t.Fatalf("pre-share exec should 404, got %d", c)
	}
	// a shares with b (http mode)
	grant := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/sandboxes/"+lid+"/share", strings.NewReader(`{"grantee":"`+bID+`","mode":"http"}`))
	req.Header.Set("Authorization", "Bearer tok-a")
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(grant, req)
	if grant.Code != http.StatusCreated {
		t.Fatalf("grant: %d %s", grant.Code, grant.Body.String())
	}
	// b can exec now
	if c := execAs(h, "tok-b", lid); c != http.StatusOK {
		t.Fatalf("post-share exec should 200, got %d", c)
	}
	// list shares shows it
	ls := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/api/shares", nil)
	req2.Header.Set("Authorization", "Bearer tok-a")
	h.ServeHTTP(ls, req2)
	if !strings.Contains(ls.Body.String(), `"grantee":"`+bID+`"`) {
		t.Fatalf("share list missing grantee: %s", ls.Body.String())
	}
	_ = aID
}

func TestShareRevokeDenies(t *testing.T) {
	h, _, bID := newShareTestServer(t)
	lid := createLeaseAs(h, "tok-a")
	// grant
	g := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/sandboxes/"+lid+"/share", strings.NewReader(`{"grantee":"`+bID+`","mode":"http"}`))
	req.Header.Set("Authorization", "Bearer tok-a")
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(g, req)
	if g.Code != http.StatusCreated {
		t.Fatalf("grant: %d", g.Code)
	}
	// revoke
	r := httptest.NewRecorder()
	req2 := httptest.NewRequest("DELETE", "/api/sandboxes/"+lid+"/share/"+bID, nil)
	req2.Header.Set("Authorization", "Bearer tok-a")
	h.ServeHTTP(r, req2)
	if r.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", r.Code)
	}
	// b denied again
	if c := execAs(h, "tok-b", lid); c != http.StatusNotFound {
		t.Fatalf("post-revoke exec should 404, got %d", c)
	}
}

func TestShareExpiryHonored(t *testing.T) {
	h, _, bID := newShareTestServer(t)
	lid := createLeaseAs(h, "tok-a")
	// share with ttl=1s
	g := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/sandboxes/"+lid+"/share", strings.NewReader(`{"grantee":"`+bID+`","mode":"http","ttl":1}`))
	req.Header.Set("Authorization", "Bearer tok-a")
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(g, req)
	if g.Code != http.StatusCreated {
		t.Fatalf("grant: %d", g.Code)
	}
	// immediately allowed
	if c := execAs(h, "tok-b", lid); c != http.StatusOK {
		t.Fatalf("pre-expiry exec should 200, got %d", c)
	}
	// after expiry denied
	time.Sleep(1100 * time.Millisecond)
	if c := execAs(h, "tok-b", lid); c != http.StatusNotFound {
		t.Fatalf("post-expiry exec should 404, got %d", c)
	}
}

func TestShareOwnerOnlyGrant(t *testing.T) {
	h, _, bID := newShareTestServer(t)
	lid := createLeaseAs(h, "tok-a")
	// b cannot share a's lease with itself
	g := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/sandboxes/"+lid+"/share", strings.NewReader(`{"grantee":"`+bID+`","mode":"http"}`))
	req.Header.Set("Authorization", "Bearer tok-b")
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(g, req)
	if g.Code != http.StatusNotFound {
		t.Fatalf("non-owner grant should 404, got %d", g.Code)
	}
	// owner cannot share with a non-existent user (grantee must exist)
	g2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/api/sandboxes/"+lid+"/share", strings.NewReader(`{"grantee":"self","mode":"http"}`))
	req2.Header.Set("Authorization", "Bearer tok-a")
	req2.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(g2, req2)
	if g2.Code != http.StatusBadRequest {
		t.Fatalf("grant to unknown user should 400, got %d", g2.Code)
	}
}
