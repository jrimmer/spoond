package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jrimmer/spoond/v2/substrate/fake"
)

// sub2 digs the fake substrate out of the service for call counting:
// NewServerWithService keeps svc.sub unexported, but the test package
// is api itself, so the field is reachable through the wrapper.
func sub2(svc *Service) *fake.Fake {
	return svc.sub.(*testSub).Fake
}

// TestKeptCapFifthKeepConflicts: the 5th keep on one lease is 409 with
// the cap message and takes no checkpoint at all (#126). The first four
// keeps stand untouched — nothing is evicted.
func TestKeptCapFifthKeepConflicts(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.MaxKeptPerLease = 4
	seedImage(t, db, "py-base", 2048)

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	id := create["id"].(string)
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "token-a",
			map[string]any{"keep": true})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("keep %d: status %d: %v, want 200", i+1, resp.StatusCode, body)
		}
	}
	keeps, err := db.ListKeptBuilds(ctx)
	if err != nil || len(keeps[id]) != 4 {
		t.Fatalf("kept rows after 4 keeps = %v (%v), want 4", keeps, err)
	}

	// The 5th keep is over the cap: 409 naming the cap and the unpin
	// route, and no checkpoint is taken — the fake's checkpoint count
	// and the kept rows both stand still.
	callsBefore := calls(sub2(svc), "Checkpoint")
	resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "token-a",
		map[string]any{"keep": true})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("5th keep: status %d (%v), want 409", resp.StatusCode, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "kept checkpoint limit reached (4 per lease)") ||
		!strings.Contains(msg, "DELETE /api/snapshots/{build_id}") {
		t.Fatalf("409 body = %v, want the cap message with the unpin hint", body)
	}
	if got := calls(sub2(svc), "Checkpoint"); got != callsBefore {
		t.Fatalf("rejected keep took %d checkpoint(s), want 0", got-callsBefore)
	}
	keeps, err = db.ListKeptBuilds(ctx)
	if err != nil || len(keeps[id]) != 4 {
		t.Fatalf("kept rows after the rejected keep = %v (%v), want still 4 (nothing evicted)", keeps, err)
	}

	// A plain checkpoint (no keep) still works at the cap: the cap only
	// governs keeps.
	if resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "token-a", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("plain checkpoint at the cap: status %d (%v), want 200", resp.StatusCode, body)
	}

	// MAX_KEPT_PER_LEASE=0 disables the cap: the 5th keep lands.
	svc.cfg.MaxKeptPerLease = 0
	resp, body = doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "token-a",
		map[string]any{"keep": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("keep with the cap off: status %d (%v), want 200", resp.StatusCode, body)
	}
	keeps, err = db.ListKeptBuilds(ctx)
	if err != nil || len(keeps[id]) != 5 {
		t.Fatalf("kept rows with the cap off = %v (%v), want 5", keeps, err)
	}
}

// TestKeptCapUnpinFreesSlot: unpinning one kept build
// (DELETE /api/snapshots/{build_id}) frees a slot, so the next keep
// succeeds again (#126).
func TestKeptCapUnpinFreesSlot(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.MaxKeptPerLease = 2
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	id := create["id"].(string)

	var builds []string
	for i := 0; i < 2; i++ {
		resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "token-a",
			map[string]any{"keep": true})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("keep %d: status %d: %v", i+1, resp.StatusCode, body)
		}
		builds = append(builds, body["build_id"].(string))
	}
	if resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "token-a",
		map[string]any{"keep": true}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("3rd keep: status %d (%v), want 409", resp.StatusCode, body)
	}

	// Unpin one: the delete route drops the kept row before its kept-set
	// check, so the DELETE frees the slot even though build[0] itself is
	// still in use (it is an ancestor of the lease's live build, which
	// no delete may remove while the lease runs) — the route answers 409
	// for the build, but the pin is gone.
	if resp, body := doReq(t, "DELETE", ts.URL+"/api/snapshots/"+builds[0], "token-a", nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("unpin %s: status %d (%v), want 409 (the build is the live build's ancestor)", builds[0], resp.StatusCode, body)
	}
	keeps, err := db.ListKeptBuilds(context.Background())
	if err != nil || len(keeps[id]) != 1 {
		t.Fatalf("kept rows after the unpin = %v (%v), want 1 (the pin is dropped)", keeps, err)
	}
	resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "token-a",
		map[string]any{"keep": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("keep after the unpin: status %d (%v), want 200", resp.StatusCode, body)
	}
	keeps, err = db.ListKeptBuilds(context.Background())
	if err != nil || len(keeps[id]) != 2 {
		t.Fatalf("kept rows after the retry keep = %v (%v), want 2", keeps, err)
	}
}
