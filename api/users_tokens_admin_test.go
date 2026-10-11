package api

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jrimmer/spoond/v3/identity"
)

// doUsersReqFromIPCounter hands each refusal probe of a route walk its
// own fake client address (192.0.2.x), so a batch of wrong-token
// refusals reads 401 per probe instead of the limiter's 429.
var doUsersReqFromIPCounter int

// newAdminMintServer is a token server fixture with the env ADMIN_TOKEN
// configured (root's break-glass, #138 A3): an admin user jason
// (t-admin), a plain user alice (t-user) and the legacy consumer token
// legacy-tok. Returns the server (its Handler() is built fresh by the
// caller, so route mounts made after the call are included), the log
// buffer the tests assert on, and alice's id and name.
func newAdminMintServer(t *testing.T) (*Server, *bytes.Buffer, string, string) {
	t.Helper()
	svc, db, _ := newTestService(t)
	ids, err := identity.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(db))
	srv.SetAdminToken("root-admin-tok")
	var logs bytes.Buffer
	svc.log = log.New(&logs, "", 0)
	h := srv.Handler()
	if rec, body := doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"jason","fingerprints":["SHA256:fp-admin"],"token":"t-admin"}`); rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap: %d %s", rec.Code, body)
	}
	rec2, body2 := doUsersReq(t, h, "POST", "/api/users", "t-admin", `{"name":"alice","fingerprints":["SHA256:fp-alice"],"token":"t-user"}`)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("alice create: %d %s", rec2.Code, rec2.Body.String())
	}
	alice := body2["user"].(map[string]any)
	return srv, &logs, alice["id"].(string), alice["name"].(string)
}

// doUsersReqAddr is doUsersReq with an explicit RemoteAddr, so a walk
// of refusals can put each probe on its own fake client address
// (TEST-NET-1) instead of tripping the per-IP failed-auth limiter.
func doUsersReqAddr(t *testing.T, h http.Handler, method, path, token, body, addr string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != "" {
		buf.WriteString(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.RemoteAddr = addr
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var parsed map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
	return rec, parsed
}

// TestAdminTokenMintsForAnyUser (#138 A3): the break-glass ADMIN_TOKEN
// mints a working, logged token for a plain user and for a second user
// — the maintenance-window shape, minting for whoever is locked out —
// each answered once, never logged with the secret. The minted token
// authenticates as its user.
func TestAdminTokenMintsForAnyUser(t *testing.T) {
	srv, logs, aliceID, _ := newAdminMintServer(t)
	h := srv.Handler()

	// A second plain user, so "any user" is not only the fixture's one.
	rec, body := doUsersReq(t, h, "POST", "/api/users", "t-admin", `{"name":"mallory","fingerprints":["SHA256:fp-mallory"],"token":"t-mallory"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mallory create: %d %s", rec.Code, rec.Body.String())
	}
	malloryID := body["user"].(map[string]any)["id"].(string)

	type target struct {
		desc, id, name string
	}
	for _, tg := range []target{
		{"plain user", aliceID, "alice-window"},
		{"second plain user", malloryID, "mallory-window"},
	} {
		rec, parsed := doUsersReq(t, h, "POST", "/api/users/"+tg.id+"/tokens", "root-admin-tok",
			`{"name":"`+tg.name+`","ttl_secs":3600}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s: mint = %d %s, want 201", tg.desc, rec.Code, rec.Body.String())
		}
		secret := tokenSecret(parsed)
		if secret == "" || parsed["id"] == "" || parsed["expires_at"] == "" {
			t.Fatalf("%s: mint shape = %v", tg.desc, parsed)
		}
		if line := logs.String(); !strings.Contains(line, "ADMIN_TOKEN") ||
			!strings.Contains(line, tg.name) || !strings.Contains(line, parsed["id"].(string)) ||
			strings.Contains(line, secret) {
			t.Fatalf("%s: mint log = %q (want name/id, no secret)", tg.desc, line)
		}
		logs.Reset()

		// The minted token authenticates as its user.
		req := httptest.NewRequest("GET", "/api/users/me", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		rec2 := httptest.NewRecorder()
		h.ServeHTTP(rec2, req)
		if rec2.Code != http.StatusOK {
			t.Fatalf("%s: minted token auth = %d %s", tg.desc, rec2.Code, rec2.Body.String())
		}
		var me struct {
			User struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"user"`
		}
		if err := json.Unmarshal(rec2.Body.Bytes(), &me); err != nil {
			t.Fatal(err)
		}
		if me.User.ID != tg.id {
			t.Fatalf("%s: token resolves to %q, want %q", tg.desc, me.User.ID, tg.id)
		}
	}
}

// TestAdminTokenMintShape: minting for an unknown user is 404, and a
// bad body is the same 400 the user path answers — the break-glass
// changes who may mint, not the request shape.
func TestAdminTokenMintShape(t *testing.T) {
	srv, _, aliceID, _ := newAdminMintServer(t)
	h := srv.Handler()
	rec, _ := doUsersReq(t, h, "POST", "/api/users/u-nope/tokens", "root-admin-tok", `{"name":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown user mint = %d, want 404", rec.Code)
	}
	for _, tt := range []struct {
		desc, body string
		code       int
	}{
		{"invalid JSON", `{"name":`, http.StatusBadRequest},
		{"missing name", `{}`, http.StatusBadRequest},
		{"negative ttl", `{"name":"x","ttl_secs":-1}`, http.StatusBadRequest},
	} {
		rec, _ = doUsersReq(t, h, "POST", "/api/users/"+aliceID+"/tokens", "root-admin-tok", tt.body)
		if rec.Code != tt.code {
			t.Errorf("%s with ADMIN_TOKEN = %d, want %d (shape kept)", tt.desc, rec.Code, tt.code)
		}
	}
}

// TestAdminTokenWrongToken: a wrong or missing bearer token on the
// mint route is 401 exactly as an unknown user token is.
func TestAdminTokenWrongToken(t *testing.T) {
	srv, _, aliceID, _ := newAdminMintServer(t)
	h := srv.Handler()
	rec, body := doUsersReq(t, h, "POST", "/api/users/"+aliceID+"/tokens", "root-admin-toj", `{"name":"w"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong ADMIN_TOKEN on mint = %d (%v), want 401", rec.Code, body)
	}
	// Wrong token on any user route: 401 the same (not the A2 403/404
	// which belong to authenticated callers).
	rec2, _ := doUsersReq(t, h, "POST", "/api/users/"+aliceID+"/quota", "root-admin-toj", `{"max_ttl":60}`)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("wrong ADMIN_TOKEN on quota = %d, want 401", rec2.Code)
	}
}

// TestAdminTokenOnlyMintRoute: the break-glass must stay on POST
// /api/users/{id}/tokens. ADMIN_TOKEN keeps its ordinary refusals (401)
// on the other two token routes, on every other user route, on the
// lease surface and on /metrics; the drain surface keeps working with
// the correct token. The mint table this test walks pins the one-route
// rule.
func TestAdminTokenOnlyMintRoute(t *testing.T) {
	srv, _, aliceID, _ := newAdminMintServer(t)
	h := srv.Handler()

	req401 := func(method, path, body string) {
		t.Helper()
		// A distinct fake client IP per probe: the refusals below must
		// read 401, not the limiter's 429, and a real operator's probes
		// come from one address that would throttle after 5.
		doUsersReqFromIPCounter++
		rec, parsed := doUsersReqAddr(t, h, method, path, "root-admin-tok", body, "192.0.2."+strconv.Itoa(doUsersReqFromIPCounter%250+1)+":4567")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s with ADMIN_TOKEN = %d (%v), want 401", method, path, rec.Code, parsed)
		}
	}
	req401("GET", "/api/users/"+aliceID+"/tokens", "")
	req401("DELETE", "/api/users/"+aliceID+"/tokens/t-default", "")
	req401("GET", "/api/users", "")
	req401("POST", "/api/users", `{"name":"eve"}`)
	req401("POST", "/api/users/"+aliceID+"/quota", `{"max_ttl":60}`)
	req401("POST", "/api/users/"+aliceID+"/llm-key", `{"llm_key":"k"}`)
	req401("DELETE", "/api/users/"+aliceID, "")
	req401("GET", "/api/users/"+aliceID, "")
	req401("GET", "/api/users/me", "")
	req401("GET", "/api/leases", "")
	req401("GET", "/metrics", "")

	// GET on the token list spelling is refused too even when the mint
	// with the same values would be accepted — pinning that the
	// break-glass rides the method, not just the path.
	rec, _ := doUsersReq(t, h, "POST", "/api/users/by-name/alice/tokens", "root-admin-tok", `{"name":"z"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("ADMIN_TOKEN on /by-name/*/tokens = %d, want 401", rec.Code)
	}
}

// TestAdminTokenDrainSurfaceUnchanged: A3 must not change /api/admin/*.
// The correct ADMIN_TOKEN still reaches reconcile and an admin user's
// token still does too; a plain user's window-minted token must not,
// and a wrong ADMIN_TOKEN there keeps its 401.
func TestAdminTokenDrainSurfaceUnchanged(t *testing.T) {
	ts, _, _ := newAdminServerWithIdentities(t)
	for _, tok := range []string{"admin-tok", "user-admin-tok"} {
		resp, body := doReq(t, "POST", ts.URL+"/api/admin/reconcile", tok, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("reconcile with %q = %d (%v), want 200", tok, resp.StatusCode, body)
		}
	}
	resp, _ := doReq(t, "POST", ts.URL+"/api/admin/reconcile", "user-alice-tok", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("plain user token on reconcile = %d, want 401", resp.StatusCode)
	}
	resp, _ = doReq(t, "POST", ts.URL+"/api/admin/reconcile", "admin-toj", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong ADMIN_TOKEN on reconcile = %d, want 401", resp.StatusCode)
	}
}

// TestAdminTokenUnconfigured: with no ADMIN_TOKEN configured the
// break-glass path is inert — the mint route answers legacy consumer
// tokens with the A2 403 and unknown tokens with 401, and nothing
// panics.
func TestAdminTokenUnconfigured(t *testing.T) {
	h, _, alice, _ := newTokenTestServer(t)
	rec, _ := doUsersReq(t, h, "POST", "/api/users/"+alice.ID+"/tokens", "legacy-tok", `{"name":"x"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("legacy consumer mint with no ADMIN_TOKEN = %d, want 403", rec.Code)
	}
	rec2, _ := doUsersReq(t, h, "POST", "/api/users/"+alice.ID+"/tokens", "root-admin-tok", `{"name":"y"}`)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("unknown token with no ADMIN_TOKEN = %d, want 401", rec2.Code)
	}
}
