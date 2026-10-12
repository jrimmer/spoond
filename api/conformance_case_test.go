package api

// The conformance suite's two 3.7 cases, exercised here on the test
// server with the same assertions the production run makes
// (conformance/capabilities_test.go, group A): the self-description
// document's shape (TestA20_*) and the create-time comment
// (TestA21_*).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// capabilitiesDoc is the field subset of the document the conformance
// case reads.
type capabilitiesDoc struct {
	Version string `json:"version"`
	Images  []struct {
		Name string `json:"name"`
	} `json:"images"`
	Sizes struct {
		Presets []struct {
			Name string `json:"name"`
		} `json:"presets"`
	} `json:"sizes"`
	NetworkPolicies []struct {
		Name string `json:"name"`
	} `json:"network_policies"`
}

// conformanceDoc fetches GET /api/capabilities through ts.
func conformanceDoc(t *testing.T, ts *httptest.Server) capabilitiesDoc {
	t.Helper()
	resp, body := doReq(t, "GET", ts.URL+"/api/capabilities", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("capabilities status %d: %v", resp.StatusCode, body)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	var doc capabilitiesDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("capabilities body: %v", err)
	}
	if doc.Version == "" {
		t.Fatalf("capabilities: empty version")
	}
	if len(doc.Sizes.Presets) == 0 {
		t.Fatalf("capabilities: sizes.presets is empty")
	}
	havePolicy := map[string]bool{}
	for _, p := range doc.NetworkPolicies {
		havePolicy[p.Name] = true
	}
	for _, want := range []string{"none", "lan", "internet", "restricted"} {
		if !havePolicy[want] {
			t.Fatalf("capabilities: network_policies lacks %q", want)
		}
	}
	return doc
}

// TestA20_Capabilities: with any valid user token the endpoint answers
// 200 and every GET /api/images name appears in the document's images.
// Without a token: 401.
func TestA20_Capabilities(t *testing.T) {
	// The default presets come from ServiceConfig left empty
	// (sizeLimitsOrDefault fills them in the document).
	ts, _ := newTestServer(t)

	_ = conformanceDoc(t, ts)

	// Every image name GET /api/images carries appears in the document;
	// the test server has exactly the seeded catalog.
	respImgs, docMap := doReq(t, "GET", ts.URL+"/api/images", "token-a", nil)
	if respImgs.StatusCode != http.StatusOK {
		t.Fatalf("images status %d", respImgs.StatusCode)
	}
	var list struct {
		Images []string `json:"images"`
	}
	rawImgs, err := json.Marshal(docMap)
	if err != nil {
		t.Fatalf("re-marshal images: %v", err)
	}
	if err := json.Unmarshal(rawImgs, &list); err != nil {
		t.Fatalf("images body: %v", err)
	}
	resp, doc := doReq(t, "GET", ts.URL+"/api/capabilities", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("capabilities status %d", resp.StatusCode)
	}
	docImgs, _ := doc["images"].([]any)
	inDoc := map[string]bool{}
	for _, img := range docImgs {
		name, _ := img.(map[string]any)["name"].(string)
		inDoc[name] = true
	}
	for _, name := range list.Images {
		if !inDoc[name] {
			t.Fatalf("capabilities: images lacks %q from GET /api/images", name)
		}
	}

	// Without a token: 401.
	respNoAuth, err := http.Get(ts.URL + "/api/capabilities")
	if err != nil {
		t.Fatalf("capabilities no-token: %v", err)
	}
	respNoAuth.Body.Close()
	if respNoAuth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("capabilities without a token: status %d, want 401", respNoAuth.StatusCode)
	}
}

// TestA21_CreateComment: the create body's comment lands on the lease
// (the detail GET answers it whole — the 201 body does not carry the
// comment field, docs/api.md's response example), a comment over 512
// characters is a bad request that starts nothing, and the created
// event's detail ends with the comment after the " · " separator.
func TestA21_CreateComment(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	srv := NewServer(svc, NewImageRegistry(db))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// The comment rides the create; the lease's detail answers it.
	resp, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
		map[string]any{"image": "py-base", "ttl": 120, "comment": "conformance comment 1"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status %d: %v", resp.StatusCode, create)
	}
	id, _ := create["id"].(string)
	if id == "" {
		t.Fatalf("create body missing id: %v", create)
	}
	if _, ok := create["comment"]; ok {
		t.Fatalf("create response carries a comment field: %v", create)
	}
	respGet, got := doReq(t, "GET", ts.URL+"/api/leases/"+id, "token-a", nil)
	if respGet.StatusCode != http.StatusOK || got["comment"] != "conformance comment 1" {
		t.Fatalf("GET lease comment = %v (status %d), want conformance comment 1", got["comment"], respGet.StatusCode)
	}

	// The created event's detail ends with the comment after " · ".
	// The subscription is taken before the grant: create is the only
	// event source in this test, so closing after the answer guarantees
	// the channel has the event buffered, and collectEvents below
	// terminates.
	all := svc.Subscribe(EventFilter{})
	if _, err := svc.grantLease(context.Background(), leaseRequest{
		owner: "consumer-a", image: "py-base", ttl: time.Minute, persistent: false, comment: "conformance comment 1",
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	all.Close()
	for _, ev := range eventsFor(collectEvents(all.C), id) {
		if ev.Type == LeaseCreated && !strings.HasSuffix(ev.Detail, " · conformance comment 1") {
			t.Fatalf("created detail = %q, want it to end with the comment", ev.Detail)
		}
	}

	// Over 512 characters: 400, nothing starts.
	st, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
		map[string]any{"image": "py-base", "ttl": 120, "comment": strings.Repeat("x", 513)})
	if st.StatusCode != http.StatusBadRequest {
		t.Fatalf("513-char comment status %d, want 400: %v", st.StatusCode, body)
	}
	if _, ok := body["id"]; ok {
		t.Fatalf("refused create answered a lease id: %v", body)
	}
}

// leaseEventsStream is the SSE read of a lease's events over the test
// server (the conformance suite reads exactly this route).
type leaseEventsStream struct {
	t      *testing.T
	c      context.CancelFunc
	events chan string
}

func startLeaseEvents(t *testing.T, ts *httptest.Server, token string) *leaseEventsStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/leases/events", nil)
	if err != nil {
		t.Fatalf("events request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("events connect: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events connect status = %d", resp.StatusCode)
	}
	s := &leaseEventsStream{t: t, c: cancel, events: make(chan string, 64)}
	go readSSEData(t, ctx, resp, s.events)
	t.Cleanup(cancel)
	return s
}

// readSSEData pushes the data lines of the SSE stream onto ch.
func readSSEData(t *testing.T, ctx context.Context, resp *http.Response, ch chan<- string) {
	buf := make([]byte, 1<<16)
	for {
		if ctx.Err() != nil {
			return
		}
		n, err := resp.Body.Read(buf)
		if n > 0 {
			for _, line := range strings.Split(string(buf[:n]), "\n") {
				if data, ok := strings.CutPrefix(line, "data: "); ok {
					select {
					case ch <- data:
					default:
					}
				}
			}
		}
		if err != nil {
			return
		}
	}
}
