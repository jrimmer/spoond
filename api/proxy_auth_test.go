package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jrimmer/spoond/identity"
)

// newProxyAuthServer builds a Server with forward-auth enabled and two
// users; returns the proxy handler, the API handler, and jason's id.
func newProxyAuthServer(t *testing.T) (http.Handler, http.Handler, string) {
	t.Helper()
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"legacy-tok": "legacy-consumer"}, 0, 60*time.Second, 10*time.Minute)
	ids, _ := identity.NewStore("")
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(ff, "py-base"))
	srv.SetProxyAuth("forward-auth", "s3cret")
	apiH := srv.Handler()

	// bootstrap admin + jason
	if rec, _ := doUsersReq(t, apiH, "POST", "/api/users", "legacy-tok", `{"name":"admin","fingerprints":["SHA256:fp-x"],"token":"admin-tok"}`); rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap: %d", rec.Code)
	}
	if rec, body := doUsersReq(t, apiH, "POST", "/api/users", "admin-tok", `{"name":"jason","fingerprints":["SHA256:fp-j"],"token":"jason-tok"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create jason: %d", rec.Code)
	} else {
		_ = body
	}
	_, users := doUsersReq(t, apiH, "GET", "/api/users", "admin-tok", "")
	var jasonID string
	for _, u := range users["users"].([]any) {
		m := u.(map[string]any)
		if m["name"] == "jason" {
			jasonID = m["id"].(string)
		}
	}
	return srv.ProxyHandler(), apiH, jasonID
}

func TestProxyAuthRequiresSecret(t *testing.T) {
	ph, _, _ := newProxyAuthServer(t)
	req := httptest.NewRequest("GET", "http://deadbeef.sandbox.lacy.casa/", nil)
	req.Header.Set("Remote-User", "jason")
	rec := httptest.NewRecorder()
	ph.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing secret should 403, got %d", rec.Code)
	}
}

func TestProxyAuthWrongSecret(t *testing.T) {
	ph, _, _ := newProxyAuthServer(t)
	req := httptest.NewRequest("GET", "http://deadbeef.sandbox.lacy.casa/", nil)
	req.Header.Set("Remote-User", "jason")
	req.Header.Set("X-Proxy-Auth", "wrong")
	rec := httptest.NewRecorder()
	ph.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong secret should 403, got %d", rec.Code)
	}
}

func TestProxyAuthRequiresUser(t *testing.T) {
	ph, _, _ := newProxyAuthServer(t)
	req := httptest.NewRequest("GET", "http://deadbeef.sandbox.lacy.casa/", nil)
	req.Header.Set("X-Proxy-Auth", "s3cret")
	rec := httptest.NewRecorder()
	ph.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing Remote-User should 401, got %d", rec.Code)
	}
}

func TestProxyAuthUnknownUser(t *testing.T) {
	ph, _, _ := newProxyAuthServer(t)
	req := httptest.NewRequest("GET", "http://deadbeef.sandbox.lacy.casa/", nil)
	req.Header.Set("X-Proxy-Auth", "s3cret")
	req.Header.Set("Remote-User", "mallory")
	rec := httptest.NewRecorder()
	ph.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unknown Remote-User should 403, got %d", rec.Code)
	}
}

func TestProxyAuthOwnerScopesLookup(t *testing.T) {
	ph, apiH, _ := newProxyAuthServer(t)
	// jason's own lease id, valid auth: lookup succeeds (owner-scoped),
	// so the proxy proceeds to dial and fails with 502 (no netns in
	// unit test). 404 would mean the owner-scope lookup missed.
	lid := createLeaseAs(apiH, "jason-tok")
	if lid == "" {
		t.Fatal("no lease id available")
	}
	req := httptest.NewRequest("GET", "http://"+lid+".sandbox.lacy.casa/", nil)
	req.Header.Set("X-Proxy-Auth", "s3cret")
	req.Header.Set("Remote-User", "jason")
	rec := httptest.NewRecorder()
	ph.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("owner lease should resolve (502 dial), got %d", rec.Code)
	}
}
