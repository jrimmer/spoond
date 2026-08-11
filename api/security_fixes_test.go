package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jrimmer/spoond/identity"
)

// newSecServer builds a server with two users (admin + jason) and a
// non-admin third user (mallory). Returns handler + mallory's token.
func newSecServer(t *testing.T) (http.Handler, string) {
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
	if rec, _ := doUsersReq(t, h, "POST", "/api/users", "admin-tok", `{"name":"jason","fingerprints":["SHA256:fp-j"],"token":"jason-tok"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create jason: %d", rec.Code)
	}
	rec, _ := doUsersReq(t, h, "POST", "/api/users", "admin-tok", `{"name":"mallory","fingerprints":["SHA256:fp-m"],"token":"mallory-tok"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create mallory: %d", rec.Code)
	}
	return h, "mallory-tok"
}

// TestC1UserListAdminOnly: non-admin (and legacy consumer) cannot list
// the user directory; admin can.
func TestC1UserListAdminOnly(t *testing.T) {
	h, malloryTok := newSecServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/users", nil)
	req.Header.Set("Authorization", "Bearer "+malloryTok)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin list should 403, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/users", nil)
	req.Header.Set("Authorization", "Bearer admin-tok")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin list should 200, got %d", rec.Code)
	}
}

// TestC1UsersMe: a non-admin can read their own record.
func TestC1UsersMe(t *testing.T) {
	h, malloryTok := newSecServer(t)
	rec, body := doUsersReq(t, h, "GET", "/api/users/me", malloryTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("me should 200, got %d", rec.Code)
	}
	name := body["user"].(map[string]any)["name"].(string)
	if name != "mallory" {
		t.Fatalf("me name = %q, want mallory", name)
	}
}

// TestC1UsersByNameMinimal: by-name lookup returns id+name only, no
// admin flag / fingerprints, for any authenticated caller.
func TestC1UsersByNameMinimal(t *testing.T) {
	h, malloryTok := newSecServer(t)
	rec, body := doUsersReq(t, h, "GET", "/api/users/by-name/jason", malloryTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("by-name should 200, got %d", rec.Code)
	}
	u := body["user"].(map[string]any)
	if _, has := u["admin"]; has {
		t.Fatal("by-name response must not include admin flag")
	}
	if _, has := u["fingerprints"]; has {
		t.Fatal("by-name response must not include fingerprints")
	}
	if u["name"] != "jason" {
		t.Fatalf("name = %v", u["name"])
	}
}

// TestC1UsersByNameUnknown: unknown name → 404 (no user enumeration).
func TestC1UsersByNameUnknown(t *testing.T) {
	h, malloryTok := newSecServer(t)
	rec, _ := doUsersReq(t, h, "GET", "/api/users/by-name/ghost", malloryTok, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown by-name should 404, got %d", rec.Code)
	}
}

// TestH3BootstrapToken: with a bootstrap token configured, store-empty
// user creation requires the header; without it, forbidden.
func TestH3BootstrapToken(t *testing.T) {
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"legacy-tok": "legacy-consumer"}, 0, 60*time.Second, 10*time.Minute)
	ids, _ := identity.NewStore("")
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(ff, "py-base"))
	srv.SetBootstrapToken("boot-secret")
	h := srv.Handler()

	// No bootstrap header → 403
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/users", strings.NewReader(`{"name":"admin","fingerprints":["SHA256:fp-x"],"token":"admin-tok"}`))
	req.Header.Set("Authorization", "Bearer legacy-tok")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bootstrap without token should 403, got %d", rec.Code)
	}

	// Wrong token → 403
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/users", strings.NewReader(`{"name":"admin","fingerprints":["SHA256:fp-x"],"token":"admin-tok"}`))
	req.Header.Set("Authorization", "Bearer legacy-tok")
	req.Header.Set("X-Bootstrap-Token", "wrong")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bootstrap with wrong token should 403, got %d", rec.Code)
	}

	// Correct token → 201
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/users", strings.NewReader(`{"name":"admin","fingerprints":["SHA256:fp-x"],"token":"admin-tok"}`))
	req.Header.Set("Authorization", "Bearer legacy-tok")
	req.Header.Set("X-Bootstrap-Token", "boot-secret")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap with token should 201, got %d", rec.Code)
	}
}

// TestH2QuotaConcurrent: max_leases=1 with 20 concurrent creates must
// yield exactly 1 success (no TOCTOU blow-past).
func TestH2QuotaConcurrent(t *testing.T) {
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"legacy-tok": "legacy-consumer"}, 0, 60*time.Second, 10*time.Minute)
	ids, _ := identity.NewStore("")
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(ff, "py-base"))
	h := srv.Handler()

	doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"admin","fingerprints":["SHA256:fp-x"],"token":"admin-tok"}`)
	_, body := doUsersReq(t, h, "POST", "/api/users", "admin-tok", `{"name":"u1","fingerprints":["SHA256:fp-1"],"token":"u1-tok"}`)
	uid := body["user"].(map[string]any)["id"].(string)
	doUsersReq(t, h, "POST", "/api/users/"+uid+"/quota", "admin-tok", `{"max_leases":1}`)

	var wg sync.WaitGroup
	codes := make([]int, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/api/sandboxes", strings.NewReader(`{"image":"py-base","ttl":60}`))
			req.Header.Set("Authorization", "Bearer u1-tok")
			h.ServeHTTP(rec, req)
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("concurrent creates with max_leases=1: %d succeeded, want 1 (codes=%v)", ok, codes)
	}
}

// TestM2GatewayTokenConstantTime: impersonation still works via the
// gateway token (behavioral regression).
func TestM2GatewayTokenConstantTime(t *testing.T) {
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"legacy-tok": "legacy-consumer", "gw-tok": "gateway-consumer"}, 0, 60*time.Second, 10*time.Minute)
	ids, _ := identity.NewStore("")
	svc.SetIdentities(ids)
	svc.SetGatewayToken("gw-tok")
	srv := NewServer(svc, NewImageRegistry(ff, "py-base"))
	h := srv.Handler()

	doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"admin","fingerprints":["SHA256:fp-x"],"token":"admin-tok"}`)
	_, body := doUsersReq(t, h, "POST", "/api/users", "admin-tok", `{"name":"jason","fingerprints":["SHA256:fp-j"],"token":"jason-tok"}`)
	jid := body["user"].(map[string]any)["id"].(string)

	// Create a lease as jason via impersonation.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/sandboxes", strings.NewReader(`{"image":"py-base","ttl":60}`))
	req.Header.Set("Authorization", "Bearer gw-tok")
	req.Header.Set("X-Spoond-User-Id", jid)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("impersonated create should 201, got %d", rec.Code)
	}
	// Wrong gateway token must NOT impersonate (would fail as invalid token → 401).
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/sandboxes", strings.NewReader(`{"image":"py-base","ttl":60}`))
	req.Header.Set("Authorization", "Bearer gw-tokX")
	req.Header.Set("X-Spoond-User-Id", jid)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong gateway token should 401, got %d", rec.Code)
	}
}

// TestL5AuthRateLimit: 6+ consecutive failed auths from one IP → 429.
func TestL5AuthRateLimit(t *testing.T) {
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"legacy-tok": "legacy-consumer"}, 0, 60*time.Second, 10*time.Minute)
	ids, _ := identity.NewStore("")
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(ff, "py-base"))
	h := srv.Handler()

	got429 := false
	for i := 0; i < 8; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/sandboxes", nil)
		req.Header.Set("Authorization", "Bearer wrong-token")
		req.RemoteAddr = "203.0.113.9:12345"
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatal("expected 429 after repeated failed auths")
	}
	// A valid token from the same IP still works and resets the window.
	doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"admin","fingerprints":["SHA256:fp-x"],"token":"admin-tok"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer legacy-tok")
	req.RemoteAddr = "203.0.113.9:12345"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid token should still pass after limit, got %d", rec.Code)
	}
}

// TestM5MetricsAdminOnly: with identity store present, non-admin cannot
// read controller metrics.
func TestM5MetricsAdminOnly(t *testing.T) {
	h, malloryTok := newSecServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+malloryTok)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin metrics should 403, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer admin-tok")
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatal("admin metrics should not be 403")
	}
}

// TestC2LLMRequireKey: with requireKey, a keyless owner's lease is
// denied on /llm/ even with a valid token; with an LLM key set, the
// key is required.
func TestC2LLMRequireKey(t *testing.T) {
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"legacy-tok": "legacy-consumer"}, 0, 60*time.Second, 10*time.Minute)
	ids, _ := identity.NewStore("")
	svc.SetIdentities(ids)
	srv := NewServerWithLLM(svc, NewImageRegistry(ff, "py-base"), "https://upstream.example/v1", "host-key", "gpt-x", nil)
	srv.SetLLMRequireKey(true)
	h := srv.Handler()

	doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"admin","fingerprints":["SHA256:fp-x"],"token":"admin-tok"}`)
	_, body := doUsersReq(t, h, "POST", "/api/users", "admin-tok", `{"name":"jason","fingerprints":["SHA256:fp-j"],"token":"jason-tok"}`)
	jid := body["user"].(map[string]any)["id"].(string)
	doUsersReq(t, h, "POST", "/api/users/"+jid+"/llm-key", "admin-tok", `{"llm_key":"user-key-1"}`)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "https://vm/api/sandboxes", strings.NewReader(`{"image":"py-base","ttl":60}`))
	req.Header.Set("Authorization", "Bearer jason-tok")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d", rec.Code)
	}
	var lid string
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/api/sandboxes", nil)
	req2.Header.Set("Authorization", "Bearer jason-tok")
	h.ServeHTTP(rec2, req2)
	// parse id from list body
	var resp struct {
		Sandboxes []struct {
			ID string `json:"id"`
		} `json:"sandboxes"`
	}
	_ = jsonUnmarshal(rec2.Body.Bytes(), &resp)
	if len(resp.Sandboxes) > 0 {
		lid = resp.Sandboxes[0].ID
	}
	if lid == "" {
		t.Fatal("no lease id")
	}

	// Wrong key → 401
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("POST", "/llm/"+lid+"/openai/chat/completions", strings.NewReader(`{}`))
	req3.Header.Set("Authorization", "Bearer wrong-key")
	h.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key should 401, got %d", rec3.Code)
	}

	// Correct key → not 401 (upstream dial fails in test → 502, still proves auth passed)
	rec4 := httptest.NewRecorder()
	req4 := httptest.NewRequest("POST", "/llm/"+lid+"/openai/chat/completions", strings.NewReader(`{}`))
	req4.Header.Set("Authorization", "Bearer user-key-1")
	h.ServeHTTP(rec4, req4)
	if rec4.Code == http.StatusUnauthorized {
		t.Fatal("correct key should not be 401")
	}

	// Remove the key: now requireKey denies even with valid token.
	doUsersReq(t, h, "POST", "/api/users/"+jid+"/llm-key", "admin-tok", `{"llm_key":""}`)
	rec5 := httptest.NewRecorder()
	req5 := httptest.NewRequest("POST", "/llm/"+lid+"/openai/chat/completions", strings.NewReader(`{}`))
	req5.Header.Set("Authorization", "Bearer jason-tok")
	h.ServeHTTP(rec5, req5)
	if rec5.Code != http.StatusUnauthorized {
		t.Fatalf("keyless owner + requireKey should 401, got %d", rec5.Code)
	}
}

func jsonUnmarshal(b []byte, v any) error {
	return json.Unmarshal(b, v)
}
