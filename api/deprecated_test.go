package api

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jrimmer/spoond/v3/identity"
)

// scrapeDeprecated scrapes /metrics and returns the value of the given
// spoond_deprecated_requests_total{kind} sample (0 when the series has
// no increments yet).
func scrapeDeprecated(t *testing.T, ts *httptest.Server, adminTok, kind string) float64 {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+adminTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status %d, want 200", resp.StatusCode)
	}
	body := readAll(t, resp)
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "spoond_deprecated_requests_total{kind=\""+kind+"\"} ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("malformed sample line: %q", line)
		}
		var v float64
		if _, err := fmt.Sscanf(fields[1], "%g", &v); err != nil {
			t.Fatalf("sample value %q: %v", fields[1], err)
		}
		return v
	}
	return 0
}

// TestDeprecatedSandboxesAlias: a request that arrives on the
// /api/sandboxes spelling (the alias 4.0 removes) gets the RFC 8594
// headers — Deprecation: true plus a Link naming the same route spelled
// on /api/leases — and bumps
// spoond_deprecated_requests_total{kind="sandboxes_alias"}. The very
// same calls made on the primary /api/leases path get neither header and
// move no counter: the rewrite marks them as the primary path family.
func TestDeprecatedSandboxesAlias(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ids, err := identity.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	svc.SetIdentities(ids)
	// The first user in the store is the admin, so it may scrape
	// /metrics; the counter itself counts every authenticated alias use.
	if _, err := ids.AddUser("owner", identity.KindPerson, []string{"SHA256:fp-owner"}, "owner-tok"); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(svc, NewImageRegistry(db))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Create through the primary path so the setup itself counts nothing.
	_, create := doReq(t, "POST", ts.URL+"/api/leases", "owner-tok", map[string]any{"image": "py-base", "persistent": true})
	id := create["id"].(string)

	if n := scrapeDeprecated(t, ts, "owner-tok", "sandboxes_alias"); n != 0 {
		t.Fatalf("fresh server: sandboxes_alias counter = %v, want 0", n)
	}

	// Alias GET: both headers, counter +1.
	aliasResp, aliasBody := doReq(t, "GET", ts.URL+"/api/sandboxes/"+id, "owner-tok", nil)
	if aliasResp.StatusCode != http.StatusOK {
		t.Fatalf("alias GET status %d: %v", aliasResp.StatusCode, aliasBody)
	}
	if got := aliasResp.Header.Get("Deprecation"); got != "true" {
		t.Fatalf("alias GET: Deprecation = %q, want true", got)
	}
	if got := aliasResp.Header.Get("Link"); got != `</api/leases/`+id+`>; rel="successor-version"` {
		t.Fatalf("alias GET: Link = %q, want the same route on /api/leases", got)
	}
	if n := scrapeDeprecated(t, ts, "owner-tok", "sandboxes_alias"); n != 1 {
		t.Fatalf("sandboxes_alias counter = %v after the alias GET, want 1", n)
	}

	// The same GET on the primary spelling: no headers, counter unmoved.
	primResp, primBody := doReq(t, "GET", ts.URL+"/api/leases/"+id, "owner-tok", nil)
	if primResp.StatusCode != http.StatusOK {
		t.Fatalf("primary GET status %d: %v", primResp.StatusCode, primBody)
	}
	if primResp.Header.Get("Deprecation") != "" || primResp.Header.Get("Link") != "" {
		t.Fatalf("primary GET: unexpected deprecation headers %v", primResp.Header)
	}
	if n := scrapeDeprecated(t, ts, "owner-tok", "sandboxes_alias"); n != 1 {
		t.Fatalf("sandboxes_alias counter = %v after the primary GET, want 1", n)
	}

	// A sub-path alias request maps its successor Link one-to-one and
	// counts too.
	sub, subBody := doReq(t, "GET", ts.URL+"/api/sandboxes/queue", "owner-tok", nil)
	if sub.StatusCode != http.StatusOK {
		t.Fatalf("alias queue status %d: %v", sub.StatusCode, subBody)
	}
	if got := sub.Header.Get("Link"); got != `</api/leases/queue>; rel="successor-version"` {
		t.Fatalf("alias queue: Link = %q, want the /api/leases/queue successor", got)
	}
	if n := scrapeDeprecated(t, ts, "owner-tok", "sandboxes_alias"); n != 2 {
		t.Fatalf("sandboxes_alias counter = %v after the alias queue call, want 2", n)
	}

	// The identical queue call on the primary spelling: neither.
	primSub, primSubBody := doReq(t, "GET", ts.URL+"/api/leases/queue", "owner-tok", nil)
	if primSub.StatusCode != http.StatusOK {
		t.Fatalf("primary queue status %d: %v", primSub.StatusCode, primSubBody)
	}
	if primSub.Header.Get("Deprecation") != "" || primSub.Header.Get("Link") != "" {
		t.Fatalf("primary queue: unexpected deprecation headers %v", primSub.Header)
	}
	if n := scrapeDeprecated(t, ts, "owner-tok", "sandboxes_alias"); n != 2 {
		t.Fatalf("sandboxes_alias counter = %v after the primary queue call, want 2", n)
	}
}

// TestDeprecatedAliasLegacyConsumer: a CONSUMER_TOKENS caller using the
// alias gets the headers and the counter; the /api/leases spelling of
// the same call gets neither.
func TestDeprecatedAliasLegacyConsumer(t *testing.T) {
	ts, _ := newTestServer(t)

	resp, body := doReq(t, "GET", ts.URL+"/api/sandboxes", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("alias list status %d: %v", resp.StatusCode, body)
	}
	if resp.Header.Get("Deprecation") != "true" || resp.Header.Get("Link") != `</api/leases>; rel="successor-version"` {
		t.Fatalf("alias list: headers = Deprecation %q, Link %q", resp.Header.Get("Deprecation"), resp.Header.Get("Link"))
	}
	// No identity store: /metrics accepts any valid token.
	if n := scrapeDeprecated(t, ts, "token-a", "sandboxes_alias"); n != 1 {
		t.Fatalf("sandboxes_alias counter = %v after the alias list, want 1", n)
	}

	prim, primBody := doReq(t, "GET", ts.URL+"/api/leases", "token-a", nil)
	if prim.StatusCode != http.StatusOK {
		t.Fatalf("primary list status %d: %v", prim.StatusCode, primBody)
	}
	if prim.Header.Get("Deprecation") != "" || prim.Header.Get("Link") != "" {
		t.Fatalf("primary list: unexpected deprecation headers %v", prim.Header)
	}
	if n := scrapeDeprecated(t, ts, "token-a", "sandboxes_alias"); n != 1 {
		t.Fatalf("sandboxes_alias counter = %v after the primary list, want 1", n)
	}
}

// TestDeprecatedHeadersOnAuthRefusal: the headers ride even a call the
// API refuses, so a misconfigured caller still learns the surface goes
// away; the counter counts only authenticated usage.
func TestDeprecatedHeadersOnAuthRefusal(t *testing.T) {
	ts, _ := newTestServer(t)

	resp, _ := doReq(t, "GET", ts.URL+"/api/sandboxes", "not-a-token", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("refused call status %d, want 401", resp.StatusCode)
	}
	if resp.Header.Get("Deprecation") != "true" || resp.Header.Get("Link") != `</api/leases>; rel="successor-version"` {
		t.Fatalf("refused call: headers = Deprecation %q, Link %q", resp.Header.Get("Deprecation"), resp.Header.Get("Link"))
	}
	// The refused call bumped nothing.
	if n := scrapeDeprecated(t, ts, "token-a", "sandboxes_alias"); n != 0 {
		t.Fatalf("refused call: sandboxes_alias counter = %v, want 0", n)
	}
}

// newDeprecatedLLMTestServer builds a Server with an LLM gateway backed
// by a fake upstream and requireKey set like production's
// LLM_OPEN_LEGACY=unset (requireKey) or =1 (legacy-open). It returns the
// running server, the Server (for the metrics registry) and the store.
func newDeprecatedLLMTestServer(t *testing.T, requireKey bool, users func(*identity.Store)) (*httptest.Server, *Server, *identity.Store) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	srv := NewServerWithLLM(svc, NewImageRegistry(db), upstream.URL, "host-key", "", nil)
	srv.SetLLMRequireKey(requireKey)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, srv, ids
}

// TestDeprecatedKeylessLLM: an /llm/ request on a keyless identity-user
// owner's lease is served only because LLM_OPEN_LEGACY=1 kept it open
// (requireKey false). It bumps
// spoond_deprecated_requests_total{kind="llm_keyless"}. With the flag
// unset the very same call is denied 401 and counts nothing; with the
// owner's LLM key configured (a keyed call) the count does not move.
func TestDeprecatedKeylessLLM(t *testing.T) {
	ts, _, _ := newDeprecatedLLMTestServer(t, false /* legacy-open */, func(ids *identity.Store) {
		// The owner exists but never gets an LLM key.
		if _, err := ids.AddUser("owner", identity.KindPerson, []string{"SHA256:fp-owner"}, "owner-tok"); err != nil {
			t.Fatal(err)
		}
	})

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "owner-tok", map[string]any{"image": "py-base", "persistent": true})
	id := create["id"].(string)
	llmURL := ts.URL + "/llm/" + id + "/openai/chat/completions"

	if n := scrapeDeprecated(t, ts, "owner-tok", "llm_keyless"); n != 0 {
		t.Fatalf("fresh server: llm_keyless counter = %v, want 0", n)
	}

	// Keyless, served under legacy-open: 200 and the counter moves.
	if resp := llmReq(t, llmURL, ""); resp.StatusCode != 200 {
		t.Fatalf("keyless legacy-open call: expected 200, got %d", resp.StatusCode)
	}
	if n := scrapeDeprecated(t, ts, "owner-tok", "llm_keyless"); n != 1 {
		t.Fatalf("llm_keyless counter = %v after the keyless call, want 1", n)
	}

	// The same call again (the notice stays throttled; the counter
	// counts every request).
	if resp := llmReq(t, llmURL, ""); resp.StatusCode != 200 {
		t.Fatalf("second keyless call: expected 200, got %d", resp.StatusCode)
	}
	if n := scrapeDeprecated(t, ts, "owner-tok", "llm_keyless"); n != 2 {
		t.Fatalf("llm_keyless counter = %v after the second keyless call, want 2", n)
	}
}

// TestDeprecatedKeylessDeniedWithoutLegacyOpen: without LLM_OPEN_LEGACY
// (requireKey true, the production default) the keyless call is denied
// 401 and the counter stays put — only served keyless requests count.
func TestDeprecatedKeylessDeniedWithoutLegacyOpen(t *testing.T) {
	ts, _, _ := newDeprecatedLLMTestServer(t, true /* requireKey, like LLM_OPEN_LEGACY unset */, func(ids *identity.Store) {
		if _, err := ids.AddUser("owner", identity.KindPerson, []string{"SHA256:fp-owner"}, "owner-tok"); err != nil {
			t.Fatal(err)
		}
	})

	_, create := doReq(t, "POST", ts.URL+"/api/leases", "owner-tok", map[string]any{"image": "py-base", "persistent": true})
	id := create["id"].(string)
	llmURL := ts.URL + "/llm/" + id + "/openai/chat/completions"

	if resp := llmReq(t, llmURL, ""); resp.StatusCode != 401 {
		t.Fatalf("keyless call with requireKey: expected 401, got %d", resp.StatusCode)
	}
	if n := scrapeDeprecated(t, ts, "owner-tok", "llm_keyless"); n != 0 {
		t.Fatalf("llm_keyless counter = %v after the denied call, want 0", n)
	}
}

// TestDeprecatedKeyedLLMNotCounted: when the lease owner has an LLM key
// configured and the caller presents it, the request needs no legacy
// exception — it must not show up in the keyless counter, whatever the
// LLM_OPEN_LEGACY setting.
func TestDeprecatedKeyedLLMNotCounted(t *testing.T) {
	ts, _, _ := newDeprecatedLLMTestServer(t, false, func(ids *identity.Store) {
		owner, err := ids.AddUser("owner", identity.KindPerson, []string{"SHA256:fp-owner"}, "owner-tok")
		if err != nil {
			t.Fatal(err)
		}
		if err := ids.SetLLMKey(owner.ID, "slk-owner-secret"); err != nil {
			t.Fatal(err)
		}
	})

	_, create := doReq(t, "POST", ts.URL+"/api/leases", "owner-tok", map[string]any{"image": "py-base", "persistent": true})
	id := create["id"].(string)
	llmURL := ts.URL + "/llm/" + id + "/openai/chat/completions"

	if resp := llmReq(t, llmURL, "slk-owner-secret"); resp.StatusCode != 200 {
		t.Fatalf("keyed call: expected 200, got %d", resp.StatusCode)
	}
	if n := scrapeDeprecated(t, ts, "owner-tok", "llm_keyless"); n != 0 {
		t.Fatalf("llm_keyless counter = %v after a keyed call, want 0", n)
	}
}

// TestDeprecatedLLMNoIdentityStoreNotCounted: a deployment without an
// identity store has no legacy-open decision to make — the gateway is
// open by construction (the capability model), so its requests are not
// keyless-legacy traffic and count nothing.
func TestDeprecatedLLMNoIdentityStoreNotCounted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"ok":true}`)
	}))
	t.Cleanup(upstream.Close)

	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	srv := NewServerWithLLM(svc, NewImageRegistry(db), upstream.URL, "host-key", "", nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	_, create := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)
	llmURL := ts.URL + "/llm/" + id + "/openai/chat/completions"

	if resp := llmReq(t, llmURL, ""); resp.StatusCode != 200 {
		t.Fatalf("capability-mode call: expected 200, got %d", resp.StatusCode)
	}
	// No identity store: /metrics accepts any valid token.
	if n := scrapeDeprecated(t, ts, "token-a", "llm_keyless"); n != 0 {
		t.Fatalf("llm_keyless counter = %v without an identity store, want 0", n)
	}
}

// TestDeprecatedNoticeThrottled: the once-per-caller-per-hour log line
// fires on the first use and stays quiet on the follow-ups, per caller
// and per kind; the counter counts every request regardless.
func TestDeprecatedNoticeThrottled(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	srv := NewServer(svc, NewImageRegistry(db))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	logBuf, restore := captureServiceLog(t, svc)
	defer restore()

	doReq(t, "GET", ts.URL+"/api/sandboxes", "token-a", nil)
	doReq(t, "GET", ts.URL+"/api/sandboxes", "token-a", nil)
	doReq(t, "GET", ts.URL+"/api/sandboxes/queue", "token-a", nil)
	doReq(t, "GET", ts.URL+"/api/sandboxes", "token-b", nil)

	logs := logBuf.String()
	if n := strings.Count(logs, "deprecated /api/sandboxes request"); n != 3 {
		t.Fatalf("%d deprecated-alias notice lines, want 3 (one per caller+path):\n%s", n, logs)
	}
	if !strings.Contains(logs, "caller=consumer-a") || !strings.Contains(logs, "caller=consumer-b") {
		t.Fatalf("notices must name the caller id, never the token:\n%s", logs)
	}
	if strings.Contains(logs, "token-a") || strings.Contains(logs, "token-b") {
		t.Fatalf("notices leaked a token:\n%s", logs)
	}

	// The LLM gateway's notice is throttled the same way, per owner. The
	// gateway captured the process logger at construction, so its logger
	// is swapped directly.
	llmts, llmsrv, _ := newDeprecatedLLMTestServer(t, false /* legacy-open */, func(ids *identity.Store) {
		if _, err := ids.AddUser("owner", identity.KindPerson, []string{"SHA256:fp-owner"}, "owner-tok"); err != nil {
			t.Fatal(err)
		}
	})
	_, create := doReq(t, "POST", llmts.URL+"/api/sandboxes", "owner-tok", map[string]any{"image": "py-base", "persistent": true})
	id := create["id"].(string)
	llmURL := llmts.URL + "/llm/" + id + "/openai/chat/completions"

	llmBuf := &syncBuffer{}
	llmsrv.llm.log = log.New(llmBuf, "", 0)
	llmReq(t, llmURL, "")
	llmReq(t, llmURL, "")
	if n := strings.Count(llmBuf.String(), "deprecated keyless LLM access"); n != 1 {
		t.Fatalf("%d deprecated-keyless notice lines for two calls, want 1:\n%s", n, llmBuf.String())
	}
	if !strings.Contains(llmBuf.String(), "caller=u-") || strings.Contains(llmBuf.String(), "owner-tok") {
		t.Fatalf("keyless notice must name the owner id, never a token:\n%s", llmBuf.String())
	}
}

// captureServiceLog redirects the service logger into a buffer for the
// duration of the test.
func captureServiceLog(t *testing.T, svc *Service) (buf *syncBuffer, restore func()) {
	t.Helper()
	sb := &syncBuffer{}
	old := svc.log
	svc.log = log.New(sb, "", 0)
	return sb, func() { svc.log = old }
}

// syncBuffer is a tiny concurrency-safe buffer for log capture.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
