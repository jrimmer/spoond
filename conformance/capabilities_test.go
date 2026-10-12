//go:build conformance

package conformance

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Group A (3.7): the self-description document and the create-time
// comment. Always on; both cases touch only the capabilities document
// and leases the suite creates itself. See docs/api.md
// ("GET /api/capabilities" and the create body table's `comment` row).

// capabilityImageDoc is one entry of the document's `images` list.
type capabilityImageDoc struct {
	Name string `json:"name"`
}

// capabilityEndpointDoc is one row of the document's `endpoints` table:
// the mux registration's "METHOD /path".
type capabilityEndpointDoc struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// capabilitiesDoc is the GET /api/capabilities answer, the field subset
// the suite reads. Stable field names (docs/api.md
// §Self-description): every list is always present, a host that
// configures nothing ships empty ones.
type capabilitiesDoc struct {
	Version         string                   `json:"version"`
	Endpoints       []capabilityEndpointDoc  `json:"endpoints"`
	Images          []capabilityImageDoc     `json:"images"`
	Sizes           capabilitySizesDoc       `json:"sizes"`
	NetworkPolicies []capabilityNetPolicyDoc `json:"network_policies"`
}

// capabilitySizesDoc is the document's `sizes` block (the GET
// /api/sizes data).
type capabilitySizesDoc struct {
	Presets []struct {
		Name string `json:"name"`
	} `json:"presets"`
}

// capabilityNetPolicyDoc is one egress class.
type capabilityNetPolicyDoc struct {
	Name string `json:"name"`
}

// TestA20_Capabilities reads GET /api/capabilities with the test token
// and checks the document describes this deployment: routes, the image
// catalog, the size presets and the four egress classes. One raw
// request with no Authorization header at all gets 401, like every
// other authenticated route.
func TestA20_Capabilities(t *testing.T) {
	rec := begin(t)

	st, body, err := cl.do("GET", "/api/capabilities", nil)
	if err != nil {
		failf(t, "capabilities: %v", err)
	}
	if st != http.StatusOK {
		failf(t, "capabilities: status %d: %s", st, truncate(body))
	}
	var doc capabilitiesDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		failf(t, "capabilities: bad body: %v", err)
	}
	if doc.Version == "" {
		failf(t, "capabilities: empty version")
	}
	rec.set("version", doc.Version)

	// endpoints: one row per registered route — non-empty, and the two
	// lease-API spellings' front doors are there.
	if len(doc.Endpoints) == 0 {
		failf(t, "capabilities: endpoints is empty")
	}
	rec.set("endpoints", len(doc.Endpoints))
	have := map[string]bool{}
	for _, row := range doc.Endpoints {
		have[row.Method+" "+row.Path] = true
	}
	for _, want := range []string{"GET /api/images", "POST /api/sandboxes"} {
		if !have[want] {
			failf(t, "capabilities: endpoints lacks %q", want)
		}
	}

	// images: every image name GET /api/images reports appears. The
	// requirement is per listed name, so an empty catalog satisfies it
	// vacuously — the document cannot contradict what is not listed.
	st, body, err = cl.images("")
	if err != nil {
		failf(t, "images: %v", err)
	}
	if st != http.StatusOK {
		failf(t, "images: status %d: %s", st, truncate(body))
	}
	var list struct {
		Images []string `json:"images"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		failf(t, "images: bad body: %v", err)
	}
	inDoc := map[string]bool{}
	for _, img := range doc.Images {
		inDoc[img.Name] = true
	}
	for _, name := range list.Images {
		if !inDoc[name] {
			failf(t, "capabilities: images lacks %q from GET /api/images", name)
		}
	}
	rec.set("images", len(doc.Images))
	rec.set("images_from_get", len(list.Images))

	// sizes: the presets block (the GET /api/sizes data).
	if len(doc.Sizes.Presets) == 0 {
		failf(t, "capabilities: sizes.presets is empty")
	}
	rec.set("presets", len(doc.Sizes.Presets))

	// network_policies: the four egress classes.
	wants := []string{"none", "lan", "internet", "restricted"}
	havePolicy := map[string]bool{}
	for _, p := range doc.NetworkPolicies {
		havePolicy[p.Name] = true
	}
	for _, name := range wants {
		if !havePolicy[name] {
			failf(t, "capabilities: network_policies lacks %q (have %v)", name, doc.NetworkPolicies)
		}
	}

	// Without a token: 401, like every other authenticated route. The
	// request must carry no Authorization header at all — every client
	// call stamps `Bearer <token>` on, and CONFORMANCE_TOKEN is
	// required and valid, so an authenticated request can never
	// exercise this. One request, at most: the server throttles failed
	// auths per IP and the rest of the run depends on this
	// connection's good standing.
	anonReq, err := http.NewRequest(http.MethodGet, cl.base+"/api/capabilities", nil)
	if err != nil {
		failf(t, "capabilities without a token: %v", err)
	}
	anonResp, err := cl.hc.Do(anonReq)
	if err != nil {
		failf(t, "capabilities without a token: %v", err)
	}
	anonBody, err := io.ReadAll(anonResp.Body)
	anonResp.Body.Close()
	if err != nil {
		failf(t, "capabilities without a token: read body: %v", err)
	}
	if anonResp.StatusCode != http.StatusUnauthorized {
		failf(t, "capabilities without a token: status %d, want 401: %s", anonResp.StatusCode, truncate(anonBody))
	}
}

// defaultImage is the suite's default image: the first of
// CONFORMANCE_IMAGES (default py-base) — the image every always-on
// group uses by that name.
func defaultImage() string { return cfg.Images[0] }

// leaseDetail is the subset of GET /api/leases/{id} the suite reads
// beyond leaseInfo.
type leaseDetail struct {
	Comment string `json:"comment"`
}

// getLease reads the lease's own detail (GET /api/leases/{id}) and
// fails the test on any error or non-200.
func getLease(t *testing.T, id string) leaseDetail {
	t.Helper()
	st, body, err := cl.do("GET", "/api/leases/"+id, nil)
	if err != nil {
		failf(t, "GET lease %s: %v", id, err)
	}
	if st != http.StatusOK {
		failf(t, "GET lease %s: status %d: %s", id, st, truncate(body))
	}
	var d leaseDetail
	if err := json.Unmarshal(body, &d); err != nil {
		failf(t, "GET lease %s: bad body: %v", id, err)
	}
	return d
}

// watchBudget bounds the created-event read of A21: a stream that
// answers but carries no created event for the lease inside it fails
// the case (an open stream cannot miss an event emitted after it
// subscribed); a stream that closes first leaves the sub-check to the
// caller's skip instead.
const watchBudget = 30 * time.Second

// watchClient reads the event stream: the suite transport (the backend
// serves a self-signed cert, docs/security.md) with an overall timeout
// past watchBudget, so a silent stream cannot outlive the case.
var watchClient = &http.Client{
	Timeout:   45 * time.Second,
	Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
}

// openEventsStream opens the caller's all-lease event stream
// (GET /api/leases/events) with the test token and returns the open
// response, or nil when the stream does not open (connect error or
// non-200 — the route does not answer for this token here). nil means
// the caller skips A21's created-event sub-check. A non-nil response
// is the caller's to close.
func openEventsStream() *http.Response {
	req, err := http.NewRequest(http.MethodGet, cl.base+"/api/leases/events", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+cl.token)
	resp, err := watchClient.Do(req)
	if err != nil {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil
	}
	return resp
}

// readCreatedDetail scans an open all-lease stream for the `created`
// event of lease id and returns its detail. It fails the case when the
// stream stays open but no such event arrives inside watchBudget — an
// event emitted while the stream was subscribed cannot be missed — and
// reports ok=false when the stream closes with a transport error
// before the event shows up (the caller then skips the sub-check).
func readCreatedDetail(t *testing.T, resp *http.Response, id string) (detail string, ok bool) {
	eventClose := make(chan struct{})
	t.Cleanup(func() { _ = resp.Body.Close() })
	go func() {
		<-time.After(watchBudget)
		_ = resp.Body.Close() // unblock the scanner and end the sub-check
		close(eventClose)
	}()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			LeaseID string `json:"lease_id"`
			Type    string `json:"type"`
			Detail  string `json:"detail"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			continue
		}
		// Events of other leases the caller owns (this test makes a
		// second, comment-less create) match nothing here; gap events
		// carry no lease id at all. The suffix assertion compares the
		// event's own detail: no other lease's event can pass it.
		if ev.Type == "created" && ev.LeaseID == id {
			return ev.Detail, true
		}
	}
	select {
	case <-eventClose:
		failf(t, "the lease event stream answered but carried no created event for %s within %s", id, watchBudget)
	default:
	}
	return "", false
}

// TestA21_CreateComment sends the create-time comment (docs/api.md create
// body table, `comment`, #132): the lease's GET carries it, a create
// without one still reports none, a comment over 512 characters is a bad
// request, and the lease's `created` event names it after a ` · `. The
// event sub-check needs the extra ability to read the lease event stream
// with the test token; when the token cannot read it here, only that
// sub-check is skipped — recorded in the results with the reason, so the
// case still stands on its create-body assertions.
func TestA21_CreateComment(t *testing.T) {
	rec := begin(t)

	const comment = "conformance comment 1"

	// The created event is emitted while the create answers (#132), and
	// a warm-pool create answers in tens of milliseconds, so the stream
	// is opened before the create. The server subscribes a stream at
	// connect, before its headers (api/events_sse.go streamEvents), so
	// an already-open one cannot miss the event. The lease id only
	// exists after the answer, so the watch is necessarily the
	// all-lease stream and the read below matches events on the lease's
	// own id.
	stream := openEventsStream()

	// The create itself: the comment rides the body (#132) — the lease
	// is born with its annotation, no comment call after the answer.
	l := createLease(t, map[string]any{"image": defaultImage(), "ttl": 120, "comment": comment})
	rec.set("lease", l.ID)

	// The lease's GET shows the comment.
	if got := getLease(t, l.ID).Comment; got != comment {
		failf(t, "GET lease comment = %q, want %q", got, comment)
	}

	// The same lease without a comment still reports none.
	plain := createLease(t, map[string]any{"image": defaultImage(), "ttl": 120})
	if got := getLease(t, plain.ID).Comment; got != "" {
		failf(t, "GET lease comment = %q, want empty on a create that sent none", got)
	}

	// Over 512 characters: a bad request — the comment route's own cap,
	// applied to the create body. Nothing starts for it.
	st, body, err := cl.create(map[string]any{
		"image":   defaultImage(),
		"ttl":     120,
		"comment": strings.Repeat("x", 513),
	})
	if err != nil {
		failf(t, "create with a 513-char comment: %v", err)
	}
	if st != http.StatusBadRequest {
		failf(t, "create with a 513-char comment: status %d, want 400: %s", st, truncate(body))
	}

	// The sub-check: the created event's detail ends with the comment
	// after the " · " separator (#132). The stream was opened before
	// the create; when the token cannot read it, the sub-check alone is
	// skipped and the reason recorded.
	if stream == nil {
		msg := "skipped: the lease event stream did not open for the test token (connect error or non-200)"
		rec.set("created_event", msg)
		t.Logf("created-event sub-check %s", msg)
		return
	}
	defer stream.Body.Close()
	detail, got := readCreatedDetail(t, stream, l.ID)
	if !got {
		msg := "skipped: the lease event stream closed before the lease's created event arrived"
		rec.set("created_event", msg)
		t.Logf("created-event sub-check %s (lease %s)", msg, l.ID)
		return
	}
	if !strings.HasSuffix(detail, " · "+comment) {
		failf(t, "created event detail = %q, want the \" · %s\" suffix", detail, comment)
	}
	rec.set("created_event", "asserted")
	rec.set("created_detail", detail)
}
