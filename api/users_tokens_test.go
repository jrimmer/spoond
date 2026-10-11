package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v3/identity"
)

// newTokenTestServer builds a Server with an identity store, a
// bootstrapped admin (token t-admin) and a plain user (token t-user).
// Returns the handler, the identity store and the plain user. The log
// goes to a buffer the test can assert on (a mint or revoke names the
// token, never its secret).
func newTokenTestServer(t *testing.T) (http.Handler, *identity.Store, *identity.User, *bytes.Buffer) {
	t.Helper()
	svc, db, _ := newTestService(t)
	ids, err := identity.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(db))
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
	return h, ids, &identity.User{ID: alice["id"].(string), Name: "alice"}, &logs
}

// mintNamed mints a named token over the HTTP API and returns the
// secret and the response record.
func mintNamed(t *testing.T, h http.Handler, callerToken, userID, name string, ttlJSON string) (secret string, rec map[string]any, status int) {
	t.Helper()
	body := fmt.Sprintf(`{"name":%q,%s}`, name, ttlJSON)
	recorder, parsed := doUsersReq(t, h, "POST", "/api/users/"+userID+"/tokens", callerToken, body)
	return tokenSecret(parsed), parsed, recorder.Code
}

// tokenSecret extracts the "token" field (the one-time secret).
func tokenSecret(body map[string]any) string {
	if body == nil {
		return ""
	}
	tok, _ := body["token"].(string)
	return tok
}

// TestTokensMintListRevoke walks the full lifecycle over HTTP: mint
// (secret once), authenticate with it, list (no secret), revoke
// (204), and the revoked secret stops authenticating.
func TestTokensMintListRevoke(t *testing.T) {
	h, ids, alice, _ := newTokenTestServer(t)
	secret, body, status := mintNamed(t, h, "t-user", alice.ID, "ci", `"ttl_secs":0`)
	if status != http.StatusCreated {
		t.Fatalf("mint status %d: %v", status, body)
	}
	if secret == "" {
		t.Fatal("mint must answer the secret once")
	}
	if body["id"] == "" || body["name"] != "ci" || body["created_at"] == "" || body["expires_at"] != "" {
		t.Fatalf("mint body = %v", body)
	}
	// The minted token authenticates as alice.
	req := httptest.NewRequest("GET", "/api/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("minted token auth: %d %s", rec.Code, rec.Body.String())
	}
	var me struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.User.ID != alice.ID {
		t.Fatalf("minted token resolves to %q, want %q", me.User.ID, alice.ID)
	}
	// Listing shows the token without any secret.
	rec2, body2 := doUsersReq(t, h, "GET", "/api/users/"+alice.ID+"/tokens", "t-user", "")
	if rec2.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec2.Code, rec2.Body.String())
	}
	if raw := rec2.Body.String(); strings.Contains(raw, secret) || strings.Contains(raw, "secret") {
		t.Fatalf("secret shape leaked in listing: %s", raw)
	}
	toks := body2["tokens"].([]any)
	if len(toks) != 2 {
		t.Fatalf("tokens = %v, want 2 rows (default, ci)", toks)
	}
	row := toks[0].(map[string]any)
	if row["name"] != "default" || row["id"] == "" {
		t.Fatalf("row 0 = %v, want the default token", row)
	}
	if _, has := row["hash"]; has {
		t.Fatalf("row carries a hash field: %v", row)
	}
	// Revoke the minted token.
	tokenID := body["id"].(string)
	rec3, _ := doUsersReq(t, h, "DELETE", "/api/users/"+alice.ID+"/tokens/"+tokenID, "t-user", "")
	if rec3.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rec3.Code, rec3.Body.String())
	}
	// The revoked secret stops authenticating.
	rec4 := httptest.NewRecorder()
	req4 := httptest.NewRequest("GET", "/api/users/me", nil)
	req4.Header.Set("Authorization", "Bearer "+secret)
	h.ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token auth = %d, want 401", rec4.Code)
	}
	// And it is gone from the list.
	_, body5 := doUsersReq(t, h, "GET", "/api/users/"+alice.ID+"/tokens", "t-user", "")
	toks = body5["tokens"].([]any)
	if len(toks) != 1 || toks[0].(map[string]any)["name"] != "default" {
		t.Fatalf("list after revoke = %v, want only default", toks)
	}
	_ = ids
}

// TestTokensExpiryOverHTTP: a short-ttl token works, then stops
// authenticating once its expiry passes.
func TestTokensExpiryOverHTTP(t *testing.T) {
	h, _, alice, _ := newTokenTestServer(t)
	secret, body, status := mintNamed(t, h, "t-user", alice.ID, "flash", `"ttl_secs":1`)
	if status != http.StatusCreated {
		t.Fatalf("mint: %d %v", status, body)
	}
	if body["expires_at"] == "" {
		t.Fatalf("ttl'd mint must carry expires_at: %v", body)
	}
	req := httptest.NewRequest("GET", "/api/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	do := func() int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := do(); code != http.StatusOK {
		t.Fatalf("pre-expiry auth = %d", code)
	}
	time.Sleep(1100 * time.Millisecond)
	if code := do(); code != http.StatusUnauthorized {
		t.Fatalf("expired token auth = %d, want 401", code)
	}
}

// TestTokensAuthMatrix covers who may manage whose tokens: the user
// itself and an admin yes; another plain user 404; a legacy consumer
// token 403; and the store's routes need an identity user for self.
func TestTokensAuthMatrix(t *testing.T) {
	h, _, alice, _ := newTokenTestServer(t)
	// A second plain user.
	rec0, body0 := doUsersReq(t, h, "POST", "/api/users", "t-admin", `{"name":"mallory","fingerprints":["SHA256:fp-m"],"token":"t-mallory"}`)
	if rec0.Code != http.StatusCreated {
		t.Fatalf("mallory create: %d %s", rec0.Code, rec0.Body.String())
	}
	malloryID := body0["user"].(map[string]any)["id"].(string)

	type case0 struct {
		desc, token string
		code        int
	}
	types := 0
	// POST mint (a distinct name per case so repeats don't 409):
	for _, c := range []case0{
		{"self mints", "t-user", http.StatusCreated},
		{"admin mints for anyone", "t-admin", http.StatusCreated},
		{"another user is 404", "t-mallory", http.StatusNotFound},
		{"legacy consumer token is 403", "legacy-tok", http.StatusForbidden},
		{"no/unknown token is 401", "nope", http.StatusUnauthorized},
	} {
		types++
		rec1, body1 := doUsersReq(t, h, "POST", "/api/users/"+alice.ID+"/tokens", c.token, fmt.Sprintf(`{"name":"x-%d"}`, types))
		if rec1.Code != c.code {
			t.Errorf("%s: POST mint = %d %v, want %d", c.desc, rec1.Code, body1, c.code)
		}
	}
	// GET list:
	for _, c := range []case0{
		{"self lists", "t-user", http.StatusOK},
		{"admin lists anyone", "t-admin", http.StatusOK},
		{"another user is 404", "t-mallory", http.StatusNotFound},
		{"legacy consumer token is 403", "legacy-tok", http.StatusForbidden},
	} {
		rec2, _ := doUsersReq(t, h, "GET", "/api/users/"+alice.ID+"/tokens", c.token, "")
		if rec2.Code != c.code {
			t.Errorf("%s: GET list = %d, want %d", c.desc, rec2.Code, c.code)
		}
	}
	// DELETE revoke: the token id does not have to exist to test the
	// auth shape — a self/admin caller gets 204 (idempotent), others
	// keep their refusal.
	for _, c := range []case0{
		{"self revokes", "t-user", http.StatusNoContent},
		{"admin revokes for anyone", "t-admin", http.StatusNoContent},
		{"another user is 404", "t-mallory", http.StatusNotFound},
		{"legacy consumer token is 403", "legacy-tok", http.StatusForbidden},
	} {
		rec3, _ := doUsersReq(t, h, "DELETE", "/api/users/"+alice.ID+"/tokens/t-nope", c.token, "")
		if rec3.Code != c.code {
			t.Errorf("%s: DELETE = %d, want %d", c.desc, rec3.Code, c.code)
		}
	}
	// Unknown target user is 404 for every caller with a working
	// identity token (self/admin check runs after the lookup).
	recU, _ := doUsersReq(t, h, "POST", "/api/users/u-nope/tokens", "t-admin", `{"name":"x"}`)
	if recU.Code != http.StatusNotFound {
		t.Errorf("unknown user mint = %d, want 404", recU.Code)
	}
	// Minting on someone else's id with mallory's own token: 404, not
	// 403 (owner-scoped shape).
	recM, _ := doUsersReq(t, h, "GET", "/api/users/"+malloryID+"/tokens", "t-user", "")
	if recM.Code != http.StatusNotFound {
		t.Errorf("cross-user list = %d, want 404", recM.Code)
	}
}

// TestTokensMintBodyValidation covers the request shape: invalid JSON,
// a missing name, a negative and an absurd ttl_secs.
func TestTokensMintBodyValidation(t *testing.T) {
	h, _, alice, _ := newTokenTestServer(t)
	for _, tt := range []struct {
		desc, body string
		code       int
	}{
		{"invalid JSON", `{"name":`, http.StatusBadRequest},
		{"missing name", `{}`, http.StatusBadRequest},
		{"blank name", `{"name":"  "}`, http.StatusBadRequest},
		{"negative ttl", `{"name":"x","ttl_secs":-5}`, http.StatusBadRequest},
		{"absurd ttl", `{"name":"x","ttl_secs":99999999999999999999}`, http.StatusBadRequest},
	} {
		rec, body := doUsersReq(t, h, "POST", "/api/users/"+alice.ID+"/tokens", "t-user", tt.body)
		if rec.Code != tt.code {
			t.Errorf("%s: %d %v, want %d", tt.desc, rec.Code, body, tt.code)
		}
	}
	// A duplicate name is a 409.
	if _, _, status := mintNamed(t, h, "t-user", alice.ID, "dup", `"ttl_secs":0`); status != http.StatusCreated {
		t.Fatalf("seed mint: %d", status)
	}
	rec, _ := doUsersReq(t, h, "POST", "/api/users/"+alice.ID+"/tokens", "t-user", `{"name":"dup"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate name = %d, want 409", rec.Code)
	}
	// The legacy reserved name is refused while the default is set.
	rec2, _ := doUsersReq(t, h, "POST", "/api/users/"+alice.ID+"/tokens", "t-user", `{"name":"default"}`)
	if rec2.Code != http.StatusConflict {
		t.Fatalf("reserved name = %d, want 409", rec2.Code)
	}
}

// TestTokensAdminManagesOtherUser: the admin mints for a plain user and
// that user's new token works; the admin also revokes it.
func TestTokensAdminManagesOtherUser(t *testing.T) {
	h, _, alice, _ := newTokenTestServer(t)
	secret, body, status := mintNamed(t, h, "t-admin", alice.ID, "ops", `"ttl_secs":0`)
	if status != http.StatusCreated {
		t.Fatalf("admin mint: %d %v", status, body)
	}
	// Alice authenticates with the admin-minted token.
	req := httptest.NewRequest("GET", "/api/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin-minted token auth: %d", rec.Code)
	}
	tokenID := body["id"].(string)
	rec2, _ := doUsersReq(t, h, "DELETE", "/api/users/"+alice.ID+"/tokens/"+tokenID, "t-admin", "")
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("admin revoke: %d", rec2.Code)
	}
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, req)
	if rec3.Code != http.StatusUnauthorized {
		t.Fatalf("revoked admin-minted token = %d, want 401", rec3.Code)
	}
}

// TestTokensDefaultRevocable: revoking the "default" token takes away
// the user's original single token; minted ones keep working.
func TestTokensDefaultRevocable(t *testing.T) {
	h, _, alice, _ := newTokenTestServer(t)
	// Find the default row's id (t-default).
	_, body := doUsersReq(t, h, "GET", "/api/users/"+alice.ID+"/tokens", "t-user", "")
	toks := body["tokens"].([]any)
	defID := toks[0].(map[string]any)["id"].(string)
	if defID == "" {
		t.Fatal("default row carries no id")
	}
	// Mint a second token that must keep working after the default dies.
	secret2, _, status := mintNamed(t, h, "t-user", alice.ID, "keepme", `"ttl_secs":0`)
	if status != http.StatusCreated {
		t.Fatalf("mint: %d", status)
	}
	rec, _ := doUsersReq(t, h, "DELETE", "/api/users/"+alice.ID+"/tokens/"+defID, "t-user", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke default: %d", rec.Code)
	}
	// The old single token is gone.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/api/users/me", nil)
	req2.Header.Set("Authorization", "Bearer t-user")
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("old single token = %d, want 401", rec2.Code)
	}
	// The named token still works.
	req3 := httptest.NewRequest("GET", "/api/users/me", nil)
	req3.Header.Set("Authorization", "Bearer "+secret2)
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("named token after default revoke = %d, want 200", rec3.Code)
	}
}

// TestTokensLastUsedTracked: authenticating with a named token stamps
// its last_used_at in the listing.
func TestTokensLastUsedTracked(t *testing.T) {
	h, _, alice, _ := newTokenTestServer(t)
	secret, body, status := mintNamed(t, h, "t-user", alice.ID, "tracked", `"ttl_secs":0`)
	if status != http.StatusCreated {
		t.Fatalf("mint: %d", status)
	}
	tokenID := body["id"].(string)
	_, body2 := doUsersReq(t, h, "GET", "/api/users/"+alice.ID+"/tokens", "t-user", "")
	row := findTokenRow(t, body2, tokenID)
	if row["last_used_at"] != "" {
		t.Fatalf("unused token carries last_used %v", row["last_used_at"])
	}
	// Use it, then re-list.
	req := httptest.NewRequest("GET", "/api/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("auth: %d", rec.Code)
	}
	_, body3 := doUsersReq(t, h, "GET", "/api/users/"+alice.ID+"/tokens", "t-user", "")
	row = findTokenRow(t, body3, tokenID)
	if row["last_used_at"] == "" {
		t.Fatal("use must stamp last_used_at")
	}
}

func findTokenRow(t *testing.T, body map[string]any, tokenID string) map[string]any {
	t.Helper()
	toks, _ := body["tokens"].([]any)
	for _, r := range toks {
		row := r.(map[string]any)
		if row["id"] == tokenID {
			return row
		}
	}
	t.Fatalf("token %s not in listing: %v", tokenID, body)
	return nil
}

// TestTokensLogsNameNotSecret: mint and revoke are logged with the
// token's name and id, never its secret.
func TestTokensLogsNameNotSecret(t *testing.T) {
	h, _, alice, logs := newTokenTestServer(t)
	secret, body, status := mintNamed(t, h, "t-user", alice.ID, "logged", `"ttl_secs":3600`)
	if status != http.StatusCreated {
		t.Fatalf("mint: %d", status)
	}
	mintLine := logs.String()
	if !strings.Contains(mintLine, "logged") || !strings.Contains(mintLine, body["id"].(string)) {
		t.Fatalf("mint log missing name/id: %q", mintLine)
	}
	if strings.Contains(mintLine, secret) {
		t.Fatalf("mint log carries the secret: %q", mintLine)
	}
	logs.Reset()
	rec, _ := doUsersReq(t, h, "DELETE", "/api/users/"+alice.ID+"/tokens/"+body["id"].(string), "t-user", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", rec.Code)
	}
	revokeLine := logs.String()
	if !strings.Contains(revokeLine, body["id"].(string)) || strings.Contains(revokeLine, secret) {
		t.Fatalf("revoke log = %q (want id, no secret)", revokeLine)
	}
}
