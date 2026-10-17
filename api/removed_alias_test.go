package api

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jrimmer/spoond/v3/identity"
)

// doRaw is doReq without the JSON decode: it returns the status, the
// raw body and the response headers (the 410 answer carries headers a
// decoded map would hide).
func doRaw(t *testing.T, method, url, token string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, url, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request %s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body), resp.Header
}

// newRecordingUpstream is a fake LLM upstream that answers 200 for
// anything (the LLM gateway tests' shape, without the auth recording
// this file does not assert).
func newRecordingUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// TestRemovedSandboxesAlias answers every /api/sandboxes request with
// 410 and the JSON error the rest of the API uses, plus the successor
// Link naming the same route under /api/leases. The gate runs before
// auth, so the codes below need no token at all.
func TestRemovedSandboxesAlias(t *testing.T) {
	ts, _ := newTestServer(t)

	for _, tc := range []struct {
		method, path, successor string
	}{
		{"GET", "/api/sandboxes", "/api/leases"},
		{"POST", "/api/sandboxes", "/api/leases"},
		{"GET", "/api/sandboxes/queue", "/api/leases/queue"},
		{"GET", "/api/sandboxes/0123456789abcdef0123456789abcdef", "/api/leases/0123456789abcdef0123456789abcdef"},
		{"DELETE", "/api/sandboxes/0123456789abcdef0123456789abcdef", "/api/leases/0123456789abcdef0123456789abcdef"},
		{"POST", "/api/sandboxes/0123456789abcdef0123456789abcdef/exec", "/api/leases/0123456789abcdef0123456789abcdef/exec"},
		{"GET", "/api/sandboxes/events", "/api/leases/events"},
	} {
		code, body, header := doRaw(t, tc.method, ts.URL+tc.path, "")
		if code != http.StatusGone {
			t.Fatalf("%s %s: %d, want 410", tc.method, tc.path, code)
		}
		// writeErrorCode encodes the map, so the code field sorts first.
		wantBody := `{"code":"path_removed","error":"/api/sandboxes is removed in 4.0; use /api/leases"}` + "\n"
		if body != wantBody {
			t.Fatalf("%s %s: body = %q, want %q", tc.method, tc.path, body, wantBody)
		}
		if got := header.Get("Link"); got != `<`+tc.successor+`>; rel="successor-version"` {
			t.Fatalf("%s %s: Link = %q, want the %s successor", tc.method, tc.path, got, tc.successor)
		}
		if got := header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("%s %s: Content-Type = %q, want application/json", tc.method, tc.path, got)
		}
	}

	// A path that merely starts with the same letters is not the alias:
	// the segment boundary holds, it 404s like any unknown route.
	resp, _ := doReq(t, "GET", ts.URL+"/api/sandboxesXY", "token-a", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /api/sandboxesXY = %d, want 404", resp.StatusCode)
	}

	// The alias gate sits before auth, so the response arrives with no
	// token; with a wrong token the answer is the same 410, not a 401.
	code, _, _ := doRaw(t, "GET", ts.URL+"/api/sandboxes", "not-a-token")
	if code != http.StatusGone {
		t.Fatalf("410 must precede auth: got %d for a bad token", code)
	}

	// The same calls under /api/leases answer normally.
	prim, primBody := doReq(t, "GET", ts.URL+"/api/leases", "token-a", nil)
	if prim.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/leases: %d: %v", prim.StatusCode, primBody)
	}
	if prim.Header.Get("Link") != "" {
		t.Fatalf("primary path must not carry a successor Link: %v", prim.Header)
	}
}

// TestRemovedAliasKeylessLLM: an identity-user lease owner without an
// LLM key always gets 401 on /llm/ — 4.0 removes the LLM_OPEN_LEGACY
// opt-out that used to keep keyless owners open. Legacy consumer-owned
// leases keep the capability model (no identity store → no key check).
func TestRemovedAliasKeylessLLM(t *testing.T) {
	upstream := newRecordingUpstream(t)

	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ids, err := identity.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	svc.SetIdentities(ids)
	if _, err := ids.AddUser("owner", identity.KindPerson, []string{"SHA256:fp-owner"}, "owner-tok"); err != nil {
		t.Fatal(err)
	}
	// The env setting that used to open keyless owners is now unknown to
	// the backend: nothing reads it.
	t.Setenv("LLM_OPEN_LEGACY", "1")
	srv := NewServerWithLLM(svc, NewImageRegistry(db), upstream.URL, "host-key", "", nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	_, create := doReq(t, "POST", ts.URL+"/api/leases", "owner-tok", map[string]any{"image": "py-base", "persistent": true})
	id := create["id"].(string)
	llmURL := ts.URL + "/llm/" + id + "/openai/chat/completions"

	// Keyless identity-user owner with LLM_OPEN_LEGACY set in the env:
	// the setting is ignored and the answer is still 401.
	if resp := llmReq(t, llmURL, ""); resp.StatusCode != 401 {
		t.Fatalf("keyless owner with LLM_OPEN_LEGACY=1: expected 401, got %d", resp.StatusCode)
	}
}

// TestRemovedAliasConsumerKeepsCapabilityModel: a legacy consumer token
// (no identity store) keeps the open LLM gateway — the capability model
// is not deprecated.
func TestRemovedAliasConsumerKeepsCapabilityModel(t *testing.T) {
	upstream := newRecordingUpstream(t)

	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	srv := NewServerWithLLM(svc, NewImageRegistry(db), upstream.URL, "host-key", "", nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	_, create := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)
	llmURL := ts.URL + "/llm/" + id + "/openai/chat/completions"

	if resp := llmReq(t, llmURL, ""); resp.StatusCode != 200 {
		t.Fatalf("capability-model call: expected 200, got %d", resp.StatusCode)
	}
}
