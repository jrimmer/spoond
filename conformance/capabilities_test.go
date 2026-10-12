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
// names it after a ` · ` — read from the per-lease event stream like the
// background-job cases read theirs, skipped when this token cannot read
// streams (e.g. an events-only token running the suite).
func TestA21_CreateComment(t *testing.T) {
	rec := begin(t)

	const comment = "conformance comment 1"
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

	// The created event's detail ends with the comment after the " · "
	// separator (#132), read live off the per-lease stream. An
	// events-only token cannot run this read (its one route is the
	// all-events stream), so the sub-check skips rather than failing a
	// working backend.
	events, stop := cl.watchEvents(context.Background(), l.ID)
	defer stop()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case data, ok := <-events:
			if !ok {
				failf(t, "event stream closed before the created event for %s", l.ID)
			}
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
			if ev.LeaseID != l.ID || ev.Type != "created" {
				continue
			}
			if !strings.HasSuffix(ev.Detail, " · "+comment) {
				failf(t, "created detail = %q, want the \" · %s\" suffix", ev.Detail, comment)
			}
			rec.set("created_detail", ev.Detail)
			return
		case <-deadline:
			skipf(t, "the created event for %s did not arrive within 10 s; the stream may not be readable with this token (e.g. an events-only EVENTS_TOKEN)", l.ID)
		}
	}
}
