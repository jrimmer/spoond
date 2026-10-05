package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
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

// writeBuildFiles creates buildID's directory under the template
// storage root with files of the given sizes, the way the substrate
// leaves a freshly written build (#125).
func writeBuildFiles(t *testing.T, root, buildID string, sizes ...int) {
	t.Helper()
	dir := filepath.Join(root, buildID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	for i, n := range sizes {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d", i)), make([]byte, n), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
	}
}

// snapshotSizeBytes reads one snapshot's size_bytes out of a
// GET /api/snapshots body.
func snapshotSizeBytes(t *testing.T, body map[string]any, buildID string) (int64, bool) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	var out struct {
		Snapshots []struct {
			BuildID   string `json:"build_id"`
			SizeBytes int64  `json:"size_bytes"`
		} `json:"snapshots"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode snapshots: %v", err)
	}
	for _, s := range out.Snapshots {
		if s.BuildID == buildID {
			return s.SizeBytes, true
		}
	}
	return 0, false
}

// seedSnapshotLease grants a running persistent lease over the HTTP
// API. Tests set sub.checkpointFn / sub.pauseFn to leave the fresh
// build's files under root (svc.cfg.TemplateStoragePath) before the
// write-time measurement runs.
func seedSnapshotLease(t *testing.T, root string) (*httptest.Server, *Service, *store.DB, *testSub, *Lease) {
	t.Helper()
	ts, svc, db, sub := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = root
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d: %v", resp.StatusCode, body)
	}
	lease := svc.lookup("consumer-a", body["id"].(string))
	if lease == nil {
		t.Fatal("created lease not found")
	}
	return ts, svc, db, sub, lease
}

// TestCheckpointBuildSizeAtWriteTime: a manual checkpoint (and the
// same path the periodic loop, clone and fork take) records the fresh
// build's size_bytes immediately (#125): the row and GET /api/snapshots
// show it with no hourly accounting pass in between.
func TestCheckpointBuildSizeAtWriteTime(t *testing.T) {
	ts, svc, db, sub, lease := seedSnapshotLease(t, t.TempDir())
	// The fake's Checkpoint only mints a build id; write the build
	// directory the way the real substrate leaves it.
	sub.checkpointFn = func(ctx context.Context, sandboxID string) (string, substrate.BuildRefs, error) {
		id := e2b.NewUUID()
		writeBuildFiles(t, svc.cfg.TemplateStoragePath, id, 4096, 8192) // 12 KiB
		return id, substrate.BuildRefs{}, nil
	}

	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+lease.ID+"/checkpoint", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("checkpoint: status %d: %v", resp.StatusCode, body)
	}
	buildID := body["build_id"].(string)

	b, err := db.GetBuild(context.Background(), buildID)
	if err != nil {
		t.Fatalf("get build: %v", err)
	}
	if b.SizeBytes != 12*1024 {
		t.Fatalf("build row size_bytes = %d, want %d", b.SizeBytes, 12*1024)
	}

	// /api/snapshots shows it with no hourly pass in between.
	_, list := doReq(t, "GET", ts.URL+"/api/snapshots", "token-a", nil)
	got, ok := snapshotSizeBytes(t, list, buildID)
	if !ok {
		t.Fatalf("checkpoint build %s missing from /api/snapshots: %v", buildID, list)
	}
	if got != 12*1024 {
		t.Fatalf("snapshot size_bytes = %d, want %d", got, 12*1024)
	}
}

// TestPauseBuildSizeAtWriteTime: a suspend's pause build records its
// size at write time too (#125). The drain pauses through the same
// pauseLease path.
func TestPauseBuildSizeAtWriteTime(t *testing.T) {
	ts, svc, db, sub, lease := seedSnapshotLease(t, t.TempDir())
	sub.pauseFn = func(ctx context.Context, sandboxID, templateID string) (string, substrate.BuildRefs, error) {
		id := e2b.NewUUID()
		writeBuildFiles(t, svc.cfg.TemplateStoragePath, id, 4096) // 4 KiB
		return id, substrate.BuildRefs{}, nil
	}

	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+lease.ID+"/suspend", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("suspend: status %d: %v", resp.StatusCode, body)
	}
	svc.store.mu.Lock()
	resume := lease.ResumeBuildID
	svc.store.mu.Unlock()

	b, err := db.GetBuild(context.Background(), resume)
	if err != nil {
		t.Fatalf("get pause build: %v", err)
	}
	if b.SizeBytes != 4096 {
		t.Fatalf("pause build size_bytes = %d, want 4096", b.SizeBytes)
	}
	_, list := doReq(t, "GET", ts.URL+"/api/snapshots", "token-a", nil)
	if got, ok := snapshotSizeBytes(t, list, resume); !ok || got != 4096 {
		t.Fatalf("snapshot pause size_bytes = %d, %v; want 4096", got, ok)
	}
}

// TestBuildSizeMeasurementFailureStoresZero: a build directory the
// substrate never wrote (or that cannot be read) stores 0 at write
// time; the hourly pass re-measures and corrects the row.
func TestBuildSizeMeasurementFailureStoresZero(t *testing.T) {
	ts, svc, db, sub, lease := seedSnapshotLease(t, t.TempDir())
	// The fake writes nothing: the build directory is missing.
	sub.checkpointFn = func(ctx context.Context, sandboxID string) (string, substrate.BuildRefs, error) {
		return e2b.NewUUID(), substrate.BuildRefs{}, nil
	}

	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+lease.ID+"/checkpoint", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("checkpoint: status %d: %v", resp.StatusCode, body)
	}
	buildID := body["build_id"].(string)
	b, err := db.GetBuild(context.Background(), buildID)
	if err != nil {
		t.Fatalf("get build: %v", err)
	}
	if b.SizeBytes != 0 {
		t.Fatalf("size_bytes = %d, want 0 (nothing on disk yet)", b.SizeBytes)
	}

	// The hourly pass corrects the row once the files are there.
	writeBuildFiles(t, svc.cfg.TemplateStoragePath, buildID, 4096, 4096)
	if err := svc.accountDisk(context.Background()); err != nil {
		t.Fatalf("accountDisk: %v", err)
	}
	b, err = db.GetBuild(context.Background(), buildID)
	if err != nil {
		t.Fatalf("get build: %v", err)
	}
	if b.SizeBytes != 8192 {
		t.Fatalf("after accounting size_bytes = %d, want 8192", b.SizeBytes)
	}
}

// TestDrainPauseBuildSizeAtWriteTime: the admin drain pauses every live
// lease through the same pauseLease path, so its pause builds carry
// write-time sizes as well (#125).
func TestDrainPauseBuildSizeAtWriteTime(t *testing.T) {
	_, svc, db, sub, lease := seedSnapshotLease(t, t.TempDir())
	sub.pauseFn = func(ctx context.Context, sandboxID, templateID string) (string, substrate.BuildRefs, error) {
		id := e2b.NewUUID()
		writeBuildFiles(t, svc.cfg.TemplateStoragePath, id, 4096, 4096) // 8 KiB
		return id, substrate.BuildRefs{}, nil
	}

	// The full drain waits for the node to go quiet, which the fake
	// never reports; the per-lease pause is the path under test.
	if _, err := svc.pauseLease(context.Background(), lease, true); err != nil {
		t.Fatalf("drain pause: %v", err)
	}

	svc.store.mu.Lock()
	resume := lease.ResumeBuildID
	svc.store.mu.Unlock()
	b, err := db.GetBuild(context.Background(), resume)
	if err != nil {
		t.Fatalf("get pause build: %v", err)
	}
	if b.SizeBytes != 8192 {
		t.Fatalf("drain pause build size_bytes = %d, want 8192", b.SizeBytes)
	}
}
