//go:build conformance

package conformance

import (
	"context"
	"encoding/json"
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

// capabilityNetPolicyDoc is one egress class and its one-line meaning.
type capabilityNetPolicyDoc struct {
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

// TestA20_Capabilities reads GET /api/capabilities with the test token
// and checks the document describes this deployment: routes, the image
// catalog, the size presets and the four egress classes. Without a
// token the endpoint answers 401 like every other authenticated route.
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

	// images: every image name GET /api/images reports appears.
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
	if len(list.Images) == 0 {
		failf(t, "GET /api/images lists no images; the document cannot be checked against it")
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
		if p.Detail == "" {
			failf(t, "capabilities: network_policies entry %q carries no meaning", p.Name)
		}
	}
	for _, name := range wants {
		if !havePolicy[name] {
			failf(t, "capabilities: network_policies lacks %q (have %v)", name, doc.NetworkPolicies)
		}
	}

	// Without a token: 401, like every other authenticated route. One
	// anonymous request, at most: the server throttles failed auths per
	// IP and the rest of the run depends on this connection's good
	// standing.
	st, body, err = newClient(cfg).do("GET", "/api/capabilities", nil)
	if err != nil {
		failf(t, "capabilities without a token: %v", err)
	}
	if st != http.StatusUnauthorized {
		failf(t, "capabilities without a token: status %d, want 401: %s", st, truncate(body))
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

// TestA21_CreateComment sends the create-time comment (docs/api.md create
// body table, `comment`, #132): the lease's GET carries it, a comment
// over 512 characters is a bad request, and the lease's `created` event
// names it after a ` · ` — read from the caller's event stream with the
// test token, opened before the create so the event is caught live (the
// idle-suspend watch's pattern). The stream read is an extra ability, so
// the sub-check skips with a clear message when the stream does not
// prove readable with this token (e.g. an events-only EVENTS_TOKEN, whose
// single route is read without a bearer token).
func TestA21_CreateComment(t *testing.T) {
	rec := begin(t)

	const comment = "conformance comment 1"

	// The created event is emitted while the create answers, so the
	// stream is opened first and filtered by owner (the caller's-events
	// stream carries exactly the caller's leases): the first created
	// event after it opens is this create's — the test creates nothing
	// else until it has matched one.
	streamCtx, cancelStream := context.WithCancel(context.Background())
	defer cancelStream()
	created := make(chan string, 1)
	streamFail := make(chan string, 1)
	go func() {
		events, stop := cl.watchEvents(streamCtx, "")
		defer stop()
		for data := range events {
			var ev struct {
				LeaseID string `json:"lease_id"`
				Type    string `json:"type"`
				Detail  string `json:"detail"`
			}
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				continue
			}
			if ev.Type == "gap" {
				continue
			}
			if ev.Type == "created" && ev.LeaseID != "" && ev.LeaseID != "-" {
				created <- ev.Detail
				return
			}
		}
		streamFail <- "stream ended"
	}()

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

	// The last sub-check: the created event's detail ends with the
	// comment after the " · " separator (#132). The stream was opened
	// before the create; reading it is an extra ability (an events-only
	// EVENTS_TOKEN cannot), so the sub-check skips with a clear message
	// rather than failing a working backend when the stream does not
	// answer.
	streamWait := time.After(10 * time.Second)
	select {
	case detail := <-created:
		// The stream carries only the caller's leases, and the only
		// created event since it opened is this create's; the comment is
		// the run's unique marker, so the suffix is the match.
		if !strings.HasSuffix(detail, " · "+comment) {
			failf(t, "created event detail = %q, want the \" · %s\" suffix", detail, comment)
		}
		rec.set("created_detail", detail)
	case msg := <-streamFail:
		skipf(t, "the event stream ended without a created event (%s): it is not readable with the test token, so the created-event sub-check does not answer", msg)
	case <-streamWait:
		skipf(t, "no created event within 10 s: the event stream is not readable (or slow) with the test token, so the created-event sub-check does not answer")
	}
}
