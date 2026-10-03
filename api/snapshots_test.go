package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate/e2b"
)

// seedSnapshotBuild inserts a build row owned by owner with an old
// updated_at, so only the kept-set decides its fate.
func seedSnapshotBuild(t *testing.T, db *store.DB, id, kind, parent, owner, state string) {
	t.Helper()
	old := time.Now().Add(-2 * time.Hour)
	if err := db.InsertBuild(context.Background(), store.BuildRow{
		BuildID: id, Kind: kind, TemplateID: e2b.NewTemplateID(), Image: "py-base",
		ParentBuildID: parent, Owner: owner, State: state,
		CreatedAt: old, UpdatedAt: old,
	}); err != nil {
		t.Fatalf("seed build %s: %v", id, err)
	}
}

// TestSnapshotDeleteTemplateForbidden: deleting a template build is 403
// (template builds have no owner; the kind check precedes the owner
// check).
func TestSnapshotDeleteTemplateForbidden(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	img := seedImage(t, db, "py-base", 2048) // template build, owner ""

	resp, body := doReq(t, "DELETE", ts.URL+"/api/snapshots/"+img.CurrentBuildID, "token-a", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("delete template: status %d, want 403: %v", resp.StatusCode, body)
	}
}

// TestSnapshotDeleteUnknownAndOtherOwner: unknown ids and other owners'
// builds are 404.
func TestSnapshotDeleteUnknownAndOtherOwner(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	img := seedImage(t, db, "py-base", 2048)
	other := e2b.NewUUID()
	seedSnapshotBuild(t, db, other, "pause", img.CurrentBuildID, "consumer-b", "ready")

	resp, _ := doReq(t, "DELETE", ts.URL+"/api/snapshots/"+e2b.NewUUID(), "token-a", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete unknown: status %d, want 404", resp.StatusCode)
	}
	resp, _ = doReq(t, "DELETE", ts.URL+"/api/snapshots/"+other, "token-a", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete another owner's build: status %d, want 404", resp.StatusCode)
	}
}

// TestSnapshotDeleteInUse: a build in the kept set answers 409
// {"error":"snapshot in use"}.
func TestSnapshotDeleteInUse(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	img := seedImage(t, db, "py-base", 2048)
	c1 := e2b.NewUUID()
	seedSnapshotBuild(t, db, c1, "checkpoint", img.CurrentBuildID, "consumer-a", "ready")
	seedGCLease(t, db, "l1", "s1", c1) // sandbox runs c1: c1 and its parent are kept

	for _, id := range []string{img.CurrentBuildID, c1} {
		if id == img.CurrentBuildID {
			continue // the template answers 403, checked above
		}
		resp, body := doReq(t, "DELETE", ts.URL+"/api/snapshots/"+id, "token-a", nil)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("delete in-use %s: status %d, want 409: %v", id, resp.StatusCode, body)
		}
		if body["error"] != "snapshot in use" {
			t.Fatalf("delete in-use %s: body %v, want error \"snapshot in use\"", id, body)
		}
	}
}

// TestSnapshotDeleteThenGone: a released build deletes with 204, then
// answers 404, and the substrate saw the delete.
func TestSnapshotDeleteThenGone(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	img := seedImage(t, db, "py-base", 2048)
	p := e2b.NewUUID()
	seedSnapshotBuild(t, db, p, "pause", img.CurrentBuildID, "consumer-a", "ready")

	resp, body := doReq(t, "DELETE", ts.URL+"/api/snapshots/"+p, "token-a", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete released build: status %d, want 204: %v", resp.StatusCode, body)
	}
	if n := calls(sub.Fake, "DeleteBuild"); n != 1 {
		t.Fatalf("DeleteBuild calls = %d, want 1", n)
	}
	b, err := db.GetBuild(context.Background(), p)
	if err != nil {
		t.Fatalf("get build: %v", err)
	}
	if b.State != "deleted" {
		t.Fatalf("state = %q, want deleted", b.State)
	}

	resp, _ = doReq(t, "DELETE", ts.URL+"/api/snapshots/"+p, "token-a", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("re-delete: status %d, want 404", resp.StatusCode)
	}
}

// TestSnapshotList: the caller's non-deleted builds, with in_use from
// the kept set; other owners' builds stay invisible.
func TestSnapshotList(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	img := seedImage(t, db, "py-base", 2048)
	mine := e2b.NewUUID()
	seedSnapshotBuild(t, db, mine, "pause", img.CurrentBuildID, "consumer-a", "ready")
	seedGCLease(t, db, "l1", "s1", mine) // mine is in use
	other := e2b.NewUUID()
	seedSnapshotBuild(t, db, other, "pause", img.CurrentBuildID, "consumer-b", "ready")

	resp, body := doReq(t, "GET", ts.URL+"/api/snapshots", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list snapshots: status %d: %v", resp.StatusCode, body)
	}
	var out struct {
		Snapshots []struct {
			BuildID       string `json:"build_id"`
			Kind          string `json:"kind"`
			Image         string `json:"image"`
			ParentBuildID string `json:"parent_build_id"`
			SizeBytes     int64  `json:"size_bytes"`
			CreatedAt     string `json:"created_at"`
			InUse         bool   `json:"in_use"`
		} `json:"snapshots"`
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode snapshots: %v", err)
	}
	if len(out.Snapshots) != 1 {
		t.Fatalf("got %d snapshot(s), want 1 (mine only): %v", len(out.Snapshots), out.Snapshots)
	}
	snap := out.Snapshots[0]
	if snap.BuildID != mine || snap.Kind != "pause" || snap.Image != "py-base" ||
		snap.ParentBuildID != img.CurrentBuildID || !snap.InUse {
		t.Errorf("snapshot row = %+v", snap)
	}
	if snap.CreatedAt == "" || !strings.Contains(snap.CreatedAt, "T") {
		t.Errorf("created_at = %q, want RFC 3339", snap.CreatedAt)
	}
}
