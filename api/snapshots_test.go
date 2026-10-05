package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// captureLogs redirects the service's log output into an in-memory
// buffer for capturedLogs to fetch (tests assert on log lines; the
// logger is swapped, not the global, so no t.Parallel anywhere near).
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (lc *logCapture) Write(p []byte) (int, error) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	return lc.buf.Write(p)
}

// captureLogs swaps svc's logger to a capture and restores it at
// cleanup.
func captureLogs(t *testing.T, svc *Service) *logCapture {
	t.Helper()
	lc := &logCapture{}
	old := svc.log
	svc.log = log.New(lc, "", 0)
	t.Cleanup(func() { svc.log = old })
	return lc
}

// capturedLogs returns everything written to the capture so far.
func capturedLogs(t *testing.T, svc *Service) string {
	t.Helper()
	lc, ok := svc.log.Writer().(*logCapture)
	if !ok {
		t.Fatalf("service logger is %T, want *logCapture (captureLogs first)", svc.log.Writer())
	}
	lc.mu.Lock()
	defer lc.mu.Unlock()
	return lc.buf.String()
}

// writeBuildFiles creates buildID's directory under the template
// storage root with files of the given sizes, the way the substrate
// leaves a freshly written build (#125). It returns the size the OS
// itself reports for the build (stat blocks × 512, i.e. allocated, not
// apparent, size) — the number under test, whatever the host's
// allocation unit. Tests must compare against this, not against the
// sum of the requested sizes: a filesystem with >4 KiB blocks
// (64 KiB-page arm64 ext4, some ZFS record sizes) rounds 4096 bytes up.
func writeBuildFiles(t *testing.T, root, buildID string, sizes ...int) int64 {
	t.Helper()
	allocated, err := writeBuildDir(filepath.Join(root, buildID), sizes...)
	if err != nil {
		t.Fatalf("write build files: %v", err)
	}
	return allocated
}

// writeBuildDir is writeBuildFiles's core without the *testing.T: it
// returns errors instead of calling t.Fatal, so closures that run on
// the HTTP handler's goroutine (the fake's checkpointFn/pauseFn) can
// use it — testing forbids Fatal off the test goroutine.
func writeBuildDir(dir string, sizes ...int) (int64, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	for i, n := range sizes {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d", i)), make([]byte, n), 0o644); err != nil {
			return 0, fmt.Errorf("write file: %w", err)
		}
	}
	allocated, err := store.BuildDiskUsage(dir)
	if err != nil {
		return 0, fmt.Errorf("measure %s: %w", dir, err)
	}
	return allocated, nil
}

// allocatedSize returns dir's allocated size via store.BuildDiskUsage,
// failing the test when the walk itself fails.
func allocatedSize(t *testing.T, dir string) int64 {
	t.Helper()
	allocated, err := store.BuildDiskUsage(dir)
	if err != nil {
		t.Fatalf("measure %s: %v", dir, err)
	}
	return allocated
}

// TestBuildDiskUsageDistinguishesEmptyFromFailed: store.BuildDiskUsage
// reports a readable-but-empty (or missing) build directory as 0 with
// no error, but a directory it cannot read as an error — the write-time
// path must not treat a legitimately 0-byte build as a failed stat
// (#125).
func TestBuildDiskUsageDistinguishesEmptyFromFailed(t *testing.T) {
	root := t.TempDir()

	// A missing directory is an unwritten build: 0, no error.
	size, err := store.BuildDiskUsage(filepath.Join(root, "absent"))
	if size != 0 || err != nil {
		t.Fatalf("missing dir: size=%d err=%v, want 0, nil", size, err)
	}

	// A readable, empty directory is a legitimate 0-byte build.
	empty := filepath.Join(root, "empty")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	size, err = store.BuildDiskUsage(empty)
	if size != 0 || err != nil {
		t.Fatalf("empty dir: size=%d err=%v, want 0, nil", size, err)
	}

	// A walk that fails part way through (here: an unreadable
	// subdirectory) reports the error; its total is partial and callers
	// must not store it. Permission bits can't create an unreadable
	// directory in environments where the tests run as root, so make
	// the failure one no euid escapes: a file path where a directory
	// is needed (ENOTDIR).
	file := filepath.Join(root, "f")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := store.BuildDiskUsage(filepath.Join(file, "sub")); err == nil {
		t.Fatal("failed walk: err=nil, want a measurement failure")
	}
}

// TestBuildSizeFailedWalkStoresZero: a build whose write-time
// measurement fails — the directory exists with a readable file in
// it, but the walk cannot read all of it — stores 0, never the
// partial size, and logs the failure (#125); the hourly pass records
// the real number once it can read the files. The failure is forced
// through the diskUsage seam: permission bits cannot make a walk fail
// in environments where the tests run as root, and a stand-in error
// exercises exactly the branch a mid-walk failure takes.
func TestBuildSizeFailedWalkStoresZero(t *testing.T) {
	ts, svc, db, sub, lease := seedSnapshotLease(t, t.TempDir())
	root := svc.cfg.TemplateStoragePath
	var buildID string
	sub.checkpointFn = func(ctx context.Context, sandboxID string) (string, substrate.BuildRefs, error) {
		buildID = e2b.NewUUID()
		// A readable file: the walk has real bytes to (wrongly) report.
		// Runs on the HTTP handler's goroutine — failures must not use
		// t.Fatal(t.F) off the test goroutine.
		dir := filepath.Join(root, buildID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", substrate.BuildRefs{}, err
		}
		if err := os.WriteFile(filepath.Join(dir, "f0"), make([]byte, 8192), 0o644); err != nil {
			return "", substrate.BuildRefs{}, err
		}
		return buildID, substrate.BuildRefs{}, nil
	}
	// The walk fails after reading f0, the way an unreadable
	// subdirectory fails it: a partial total plus the error.
	svc.diskUsage = func(dir string) (int64, error) {
		size, err := store.BuildDiskUsage(dir)
		if err == nil {
			return size, fmt.Errorf("stat %s/sub: permission denied", dir)
		}
		return size, err
	}

	captureLogs(t, svc)
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+lease.ID+"/checkpoint", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("checkpoint: status %d: %v", resp.StatusCode, body)
	}
	id := body["build_id"].(string)
	if id != buildID {
		t.Fatalf("checkpoint build id %q, want %q", id, buildID)
	}
	b, err := db.GetBuild(context.Background(), buildID)
	if err != nil {
		t.Fatalf("get build: %v", err)
	}
	if b.SizeBytes != 0 {
		t.Fatalf("size_bytes = %d, want 0: a failed walk must not store its partial size", b.SizeBytes)
	}
	if got := capturedLogs(t, svc); !strings.Contains(got, "storing 0 until the hourly pass") || !strings.Contains(got, buildID) {
		t.Fatalf("log %q does not record the failed measurement for %s", got, buildID)
	}

	// The hourly pass re-measures and corrects the row: it stores what
	// it read, unlike the write-time path.
	svc.diskUsage = store.BuildDiskUsage
	want := allocatedSize(t, filepath.Join(root, buildID))
	if want < 8192 {
		t.Fatalf("allocated size = %d, want at least 8192", want)
	}
	if err := svc.accountDisk(context.Background()); err != nil {
		t.Fatalf("accountDisk: %v", err)
	}
	b, err = db.GetBuild(context.Background(), buildID)
	if err != nil {
		t.Fatalf("get build: %v", err)
	}
	if b.SizeBytes != want {
		t.Fatalf("after accounting size_bytes = %d, want %d", b.SizeBytes, want)
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
	// directory the way the real substrate leaves it. Compare against
	// the OS's own allocation count, not the requested bytes: the
	// write-time path uses the same stat as the hourly pass. Runs on
	// the HTTP handler's goroutine, so the closure reports failures as
	// the checkpoint's error instead of t.Fatal.
	var want int64
	sub.checkpointFn = func(ctx context.Context, sandboxID string) (string, substrate.BuildRefs, error) {
		id := e2b.NewUUID()
		size, err := writeBuildDir(filepath.Join(svc.cfg.TemplateStoragePath, id), 4096, 8192)
		if err != nil {
			return "", substrate.BuildRefs{}, err
		}
		want = size
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
	if b.SizeBytes != want {
		t.Fatalf("build row size_bytes = %d, want %d", b.SizeBytes, want)
	}

	// /api/snapshots shows it with no hourly pass in between.
	_, list := doReq(t, "GET", ts.URL+"/api/snapshots", "token-a", nil)
	got, ok := snapshotSizeBytes(t, list, buildID)
	if !ok {
		t.Fatalf("checkpoint build %s missing from /api/snapshots: %v", buildID, list)
	}
	if got != want {
		t.Fatalf("snapshot size_bytes = %d, want %d", got, want)
	}

	// The hourly pass re-measures and stores the same number.
	if err := svc.accountDisk(context.Background()); err != nil {
		t.Fatalf("accountDisk: %v", err)
	}
	b, err = db.GetBuild(context.Background(), buildID)
	if err != nil {
		t.Fatalf("get build: %v", err)
	}
	if b.SizeBytes != want {
		t.Fatalf("after accounting size_bytes = %d, want %d", b.SizeBytes, want)
	}
}

// TestPauseBuildSizeAtWriteTime: a suspend's pause build records its
// size at write time too (#125). The drain pauses through the same
// pauseLease path.
func TestPauseBuildSizeAtWriteTime(t *testing.T) {
	ts, svc, db, sub, lease := seedSnapshotLease(t, t.TempDir())
	// Handler-goroutine closure: errors come back as the pause's
	// error, not t.Fatal.
	var want int64
	sub.pauseFn = func(ctx context.Context, sandboxID, templateID string) (string, substrate.BuildRefs, error) {
		id := e2b.NewUUID()
		size, err := writeBuildDir(filepath.Join(svc.cfg.TemplateStoragePath, id), 4096)
		if err != nil {
			return "", substrate.BuildRefs{}, err
		}
		want = size
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
	if b.SizeBytes != want {
		t.Fatalf("pause build size_bytes = %d, want %d", b.SizeBytes, want)
	}
	_, list := doReq(t, "GET", ts.URL+"/api/snapshots", "token-a", nil)
	if got, ok := snapshotSizeBytes(t, list, resume); !ok || got != want {
		t.Fatalf("snapshot pause size_bytes = %d, %v; want %d", got, ok, want)
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

	// The hourly pass corrects the row once the files are there. The
	// OS-allocated size of 2×4096 bytes is at least 8192 — exactly that
	// on 4 KiB-block hosts, more where the allocation unit is larger.
	writeBuildFiles(t, svc.cfg.TemplateStoragePath, buildID, 4096, 4096)
	if err := svc.accountDisk(context.Background()); err != nil {
		t.Fatalf("accountDisk: %v", err)
	}
	b, err = db.GetBuild(context.Background(), buildID)
	if err != nil {
		t.Fatalf("get build: %v", err)
	}
	if b.SizeBytes < 8192 {
		t.Fatalf("after accounting size_bytes = %d, want at least 8192", b.SizeBytes)
	}
}

// TestDrainPauseBuildSizeAtWriteTime: the admin drain pauses every live
// lease through the same pauseLease path, so its pause builds carry
// write-time sizes as well (#125).
func TestDrainPauseBuildSizeAtWriteTime(t *testing.T) {
	_, svc, db, sub, lease := seedSnapshotLease(t, t.TempDir())
	var want int64
	sub.pauseFn = func(ctx context.Context, sandboxID, templateID string) (string, substrate.BuildRefs, error) {
		id := e2b.NewUUID()
		// Handler-goroutine closure: errors come back as the pause's
		// error, not t.Fatal.
		size, err := writeBuildDir(filepath.Join(svc.cfg.TemplateStoragePath, id), 4096, 4096)
		if err != nil {
			return "", substrate.BuildRefs{}, err
		}
		want = size
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
	if b.SizeBytes != want {
		t.Fatalf("drain pause build size_bytes = %d, want %d", b.SizeBytes, want)
	}
}

// TestCloneBuildSizeAtWriteTime: a clone's checkpoint build — the same
// path fork's copies take — records its size at write time (#125),
// visible in the row and in GET /api/snapshots straight after the
// clone, with no hourly accounting pass in between.
func TestCloneBuildSizeAtWriteTime(t *testing.T) {
	ts, svc, db, sub, lease := seedSnapshotLease(t, t.TempDir())
	// The same no-t.Fatal-off-the-handler-goroutine rule as the other
	// checkpointFn/pauseFn closures above.
	var want int64
	sub.checkpointFn = func(ctx context.Context, sandboxID string) (string, substrate.BuildRefs, error) {
		id := e2b.NewUUID()
		size, err := writeBuildDir(filepath.Join(svc.cfg.TemplateStoragePath, id), 4096, 4096, 4096)
		if err != nil {
			return "", substrate.BuildRefs{}, err
		}
		want = size
		return id, substrate.BuildRefs{}, nil
	}

	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+lease.ID+"/clone", "token-a", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("clone: status %d: %v", resp.StatusCode, body)
	}
	buildID := body["branch_tag"].(string)

	b, err := db.GetBuild(context.Background(), buildID)
	if err != nil {
		t.Fatalf("get clone build: %v", err)
	}
	if b.SizeBytes != want {
		t.Fatalf("clone build size_bytes = %d, want %d", b.SizeBytes, want)
	}
	_, list := doReq(t, "GET", ts.URL+"/api/snapshots", "token-a", nil)
	if got, ok := snapshotSizeBytes(t, list, buildID); !ok || got != want {
		t.Fatalf("snapshot clone size_bytes = %d, %v; want %d", got, ok, want)
	}
}
