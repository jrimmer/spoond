package api

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v4/identity"
)

// newLLMTestServer builds a Server with an LLM gateway backed by a fake
// upstream that records the Authorization header it received. svc gets
// an identity store with the given users pre-registered.
func newLLMTestServer(t *testing.T, upstreamURL string, users func(*identity.Store)) (*httptest.Server, *identity.Store, func() string) {
	t.Helper()
	ts, ids, gotAuth, _ := newLLMTestServerSvc(t, upstreamURL, users)
	return ts, ids, gotAuth
}

// newLLMTestServerSvc is newLLMTestServer that also returns the Service.
func newLLMTestServerSvc(t *testing.T, upstreamURL string, users func(*identity.Store)) (*httptest.Server, *identity.Store, func() string, *Service) {
	t.Helper()
	var mu sync.Mutex
	gotAuth := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		fmt.Fprintf(w, `{"ok":true}`)
	}))
	t.Cleanup(upstream.Close)

	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ids, err := identity.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	svc.SetIdentities(ids)
	if users != nil {
		users(ids)
	}
	reg := NewImageRegistry(db)
	url := upstreamURL
	if url == "" {
		url = upstream.URL
	}
	srv := NewServerWithLLM(svc, reg, url, "host-key", "", nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, ids, func() string {
		mu.Lock()
		defer mu.Unlock()
		return gotAuth
	}, svc
}

func llmReq(t *testing.T, url, llmKey string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	if llmKey != "" {
		req.Header.Set("Authorization", "Bearer "+llmKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

// TestLLMGatewayPerUserKey verifies U8/T8: when the lease owner has an
// LLM key set, /llm/ requires it (missing/wrong/foreign keys -> 401),
// and the correct key passes through with the host key still injected
// upstream (the user key never leaks).
func TestLLMGatewayPerUserKey(t *testing.T) {
	ts, _, gotAuth := newLLMTestServer(t, "", func(ids *identity.Store) {
		owner, err := ids.AddUser("owner", identity.KindPerson, []string{"SHA256:fp-owner"}, "owner-tok")
		if err != nil {
			t.Fatal(err)
		}
		if err := ids.SetLLMKey(owner.ID, "slk-owner-secret"); err != nil {
			t.Fatal(err)
		}
		if _, err := ids.AddUser("other", identity.KindPerson, []string{"SHA256:fp-other"}, "other-tok"); err != nil {
			t.Fatal(err)
		}
	})

	// Create a persistent lease owned by the identity user.
	_, create := doReq(t, "POST", ts.URL+"/api/leases", "owner-tok", map[string]any{"image": "py-base", "persistent": true})
	id := create["id"].(string)
	llmURL := ts.URL + "/llm/" + id + "/openai/chat/completions"

	// 1. Missing key -> 401 (key-required owner).
	if resp := llmReq(t, llmURL, ""); resp.StatusCode != 401 {
		t.Fatalf("missing key: expected 401, got %d", resp.StatusCode)
	}
	// 2. Wrong key -> 401.
	if resp := llmReq(t, llmURL, "slk-wrong"); resp.StatusCode != 401 {
		t.Fatalf("wrong key: expected 401, got %d", resp.StatusCode)
	}
	// 3. Another user's valid key -> 401 (keys are per-owner; the owner
	// check is folded into key verification).
	if resp := llmReq(t, llmURL, "slk-other"); resp.StatusCode != 401 {
		t.Fatalf("foreign key: expected 401, got %d", resp.StatusCode)
	}
	// 4. Correct key -> 200, and the upstream still sees the host key.
	if resp := llmReq(t, llmURL, "slk-owner-secret"); resp.StatusCode != 200 {
		t.Fatalf("correct key: expected 200, got %d", resp.StatusCode)
	}
	if a := gotAuth(); a != "Bearer host-key" {
		t.Fatalf("upstream auth = %q, want host key (user key must not leak)", a)
	}
}

// TestLLMGatewaySelfToken verifies #162: a lease created with
// self_token_llm may present its own self token instead of the owner's
// LLM key. A self token without the opt-in gets nothing. The token works
// only on its own lease's path, not on the owner's other leases, stops
// working when the lease ends, and never reaches the upstream. An owner
// without an LLM key stays denied, self token or not.
func TestLLMGatewaySelfToken(t *testing.T) {
	ts, _, gotAuth, svc := newLLMTestServerSvc(t, "", func(ids *identity.Store) {
		owner, err := ids.AddUser("owner", identity.KindAgent, nil, "owner-tok")
		if err != nil {
			t.Fatal(err)
		}
		if err := ids.SetLLMKey(owner.ID, "slk-owner-secret"); err != nil {
			t.Fatal(err)
		}
		if _, err := ids.AddUser("keyless", identity.KindAgent, nil, "keyless-tok"); err != nil {
			t.Fatal(err)
		}
	})
	create := func(tok string, llm bool) (string, string) {
		_, body := doReq(t, "POST", ts.URL+"/api/leases", tok, map[string]any{"image": "py-base", "persistent": true, "self_token": true, "self_token_llm": llm})
		id, _ := body["id"].(string)
		st := svc.selfTokenFor(id)
		if id == "" || !isSelfTokenBearer(st) {
			t.Fatalf("create self-token lease: %v (token %q)", body, st)
		}
		return id, st
	}
	llmURL := func(id string) string { return ts.URL + "/llm/" + id + "/openai/chat/completions" }
	a, tokA := create("owner-tok", true)
	b, _ := create("owner-tok", true)
	n, tokN := create("owner-tok", false)

	if resp := llmReq(t, llmURL(n), tokN); resp.StatusCode != 401 {
		t.Fatalf("self token without self_token_llm: expected 401, got %d", resp.StatusCode)
	}
	if resp, body := doReq(t, "POST", ts.URL+"/api/leases", "owner-tok", map[string]any{"image": "py-base", "self_token_llm": true}); resp.StatusCode != 400 {
		t.Fatalf("self_token_llm without self_token: expected 400, got %d %v", resp.StatusCode, body)
	}
	if _, got := doReq(t, "GET", ts.URL+"/api/leases/"+a, "owner-tok", nil); got["self_token_llm"] != true {
		t.Fatalf("GET does not report self_token_llm: %v", got)
	}

	if resp := llmReq(t, llmURL(a), tokA); resp.StatusCode != 200 {
		t.Fatalf("own self token: expected 200, got %d", resp.StatusCode)
	}
	if got := gotAuth(); got != "Bearer host-key" {
		t.Fatalf("upstream auth = %q, want the host key (self token must not leak)", got)
	}
	if resp := llmReq(t, llmURL(b), tokA); resp.StatusCode != 401 {
		t.Fatalf("self token on the owner's other lease: expected 401, got %d", resp.StatusCode)
	}
	if resp := llmReq(t, llmURL(a), "spl_made-up"); resp.StatusCode != 401 {
		t.Fatalf("unknown self token: expected 401, got %d", resp.StatusCode)
	}
	if resp, _ := doReq(t, "DELETE", ts.URL+"/api/leases/"+a, "owner-tok", nil); resp.StatusCode >= 300 {
		t.Fatalf("release: status %d", resp.StatusCode)
	}
	if resp := llmReq(t, llmURL(a), tokA); resp.StatusCode == 200 {
		t.Fatal("self token still works after its lease ended")
	}
	k, tokK := create("keyless-tok", true)
	if resp := llmReq(t, llmURL(k), tokK); resp.StatusCode != 401 {
		t.Fatalf("keyless owner with a self token: expected 401, got %d", resp.StatusCode)
	}
}

// TestLLMGatewayKeylessIdentityOwnerDenied verifies the 4.0 rule: with
// the identity store present but the lease owner having NO LLM key, /llm/
// answers 401 — the LLM_OPEN_LEGACY opt-out that used to keep keyless
// owners open is gone.
func TestLLMGatewayKeylessIdentityOwnerDenied(t *testing.T) {
	ts, _, _ := newLLMTestServer(t, "", func(ids *identity.Store) {
		// Owner exists but never gets an LLM key.
		if _, err := ids.AddUser("owner", identity.KindPerson, []string{"SHA256:fp-owner"}, "owner-tok"); err != nil {
			t.Fatal(err)
		}
	})
	_, create := doReq(t, "POST", ts.URL+"/api/leases", "owner-tok", map[string]any{"image": "py-base", "persistent": true})
	id := create["id"].(string)
	llmURL := ts.URL + "/llm/" + id + "/openai/chat/completions"

	// No key on the owner: 401, whatever the caller carries.
	if resp := llmReq(t, llmURL, ""); resp.StatusCode != 401 {
		t.Fatalf("owner without key must be denied: expected 401, got %d", resp.StatusCode)
	}
}

// TestLLMGatewayPerUserConcurrencyCap verifies the U8/T8 per-user
// in-flight cap: while one request is being served, a second gets 429;
// after the first completes the slot frees up.
func TestLLMGatewayPerUserConcurrencyCap(t *testing.T) {
	// Upstream that blocks until released, signaling when entered.
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-release
		fmt.Fprintf(w, `{"ok":true}`)
	}))
	t.Cleanup(upstream.Close)

	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ids, err := identity.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	svc.SetIdentities(ids)
	owner, err := ids.AddUser("owner", identity.KindPerson, []string{"SHA256:fp-owner"}, "owner-tok")
	if err != nil {
		t.Fatal(err)
	}
	if err := ids.SetLLMKey(owner.ID, "slk-secret"); err != nil {
		t.Fatal(err)
	}
	reg := NewImageRegistry(db)
	srv := NewServerWithLLM(svc, reg, upstream.URL, "host-key", "", nil)
	srv.SetLLMMaxConcurrent(1)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	_, create := doReq(t, "POST", ts.URL+"/api/leases", "owner-tok", map[string]any{"image": "py-base", "persistent": true})
	id := create["id"].(string)
	llmURL := ts.URL + "/llm/" + id + "/openai/chat/completions"

	// First request occupies the single slot (blocks in the upstream).
	// Built inline (not via llmReq) so failures report over the channel
	// instead of calling t.Fatal from a non-test goroutine.
	firstDone := make(chan int, 1)
	go func() {
		req, err := http.NewRequest("POST", llmURL, strings.NewReader(`{"model":"m"}`))
		if err != nil {
			firstDone <- -1
			return
		}
		req.Header.Set("Authorization", "Bearer slk-secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			firstDone <- -1
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		firstDone <- resp.StatusCode
	}()
	<-entered

	// Second request must be rejected while the slot is busy.
	if resp := llmReq(t, llmURL, "slk-secret"); resp.StatusCode != 429 {
		t.Fatalf("concurrent request: expected 429, got %d", resp.StatusCode)
	}

	// Release the first; the slot frees and new requests succeed.
	close(release)
	if code := <-firstDone; code != 200 {
		t.Fatalf("first request: expected 200, got %d", code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if resp := llmReq(t, llmURL, "slk-secret"); resp.StatusCode == 200 {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("request after release never succeeded (last status %d)", resp.StatusCode)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
