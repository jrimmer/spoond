package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
	"github.com/jrimmer/spoond/v2/substrate/e2b"
	"github.com/jrimmer/spoond/v2/substrate/fake"
)

// testSub is the fake substrate with an exec handler that answers the
// integrity probe: a sandbox is healthy unless the test marks it
// otherwise (probeFail by id, or probeFailAll for fresh creates too).
type testSub struct {
	*fake.Fake
	probeFail    map[string]string
	probeFailAll bool
	execStdout   string // canned stdout for non-probe execs ("" = "ok\n")

	// rootfsFail, keyed by sandbox id, makes the rootfs liveness probe
	// answer the given stderr with exit code 1 ("Input/output error" for
	// a dead disk). rootfsErr, keyed the same way, makes it a transport
	// failure instead; rootfsTimeout makes it the substrate's own timeout
	// marker (exit 124, nil error) (spoond-5ca).
	rootfsFail    map[string]string
	rootfsErr     map[string]bool
	rootfsTimeout map[string]bool

	// onRootfsProbe, when set, runs while a rootfs probe exec is being
	// served, before its outcome is decided. Tests use it to start an
	// admin drain mid-pass (spoond-5ca).
	onRootfsProbe func(sandboxID string)

	// checkpointFn/pauseFn, when set, replace the fake's Checkpoint and
	// Pause: they mint the build id and may leave the fresh build's
	// files on disk (the build-size-at-write-time tests, #125).
	checkpointFn func(ctx context.Context, sandboxID string) (string, substrate.BuildRefs, error)
	pauseFn      func(ctx context.Context, sandboxID, templateID string) (string, substrate.BuildRefs, error)
	// createFn, when set, replaces the fake's Create: it may run while a
	// create is in flight (the crash-reconcile-rootfs-probe race test).
	createFn func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error)

	// lastStart records the most recent Start request (the stream tests
	// pin the initial PTY size it carries).
	startMu   sync.Mutex
	lastStart substrate.StartRequest

	// rootfsProbeMu guards rootfsProbes, the count of rootfs liveness
	// probe execs the fake served (spoond-5ca).
	rootfsProbeMu sync.Mutex
	rootfsProbes  int

	// scrubLeftover, when set, makes the secrets-scrub exec leave that
	// file behind and report it: it exercises the save's scrub_failed
	// abort (2.7, #83 B1/S2).
	scrubLeftover string

	// execBefore, when set, runs at the start of every Exec the fake
	// serves. A test uses it to observe the guest's state at the first
	// exec (the integrity probe) of a create (2.7, #83 A4/A7).
	execBefore func(sandboxID string, req substrate.ExecRequest)
}

// LastStart returns the most recent Start request.
func (ts *testSub) LastStart() substrate.StartRequest {
	ts.startMu.Lock()
	defer ts.startMu.Unlock()
	return ts.lastStart
}

// Start delegates to the fake and records the request.
func (ts *testSub) Start(ctx context.Context, sandboxID string, req substrate.StartRequest) (substrate.Process, error) {
	ts.startMu.Lock()
	ts.lastStart = req
	ts.startMu.Unlock()
	return ts.Fake.Start(ctx, sandboxID, req)
}

// Checkpoint delegates to checkpointFn when set, the fake otherwise.
func (ts *testSub) Checkpoint(ctx context.Context, sandboxID string) (string, substrate.BuildRefs, error) {
	if ts.checkpointFn != nil {
		return ts.checkpointFn(ctx, sandboxID)
	}
	return ts.Fake.Checkpoint(ctx, sandboxID)
}

// Pause delegates to pauseFn when set, the fake otherwise.
func (ts *testSub) Pause(ctx context.Context, sandboxID, templateID string) (string, substrate.BuildRefs, error) {
	if ts.pauseFn != nil {
		return ts.pauseFn(ctx, sandboxID, templateID)
	}
	return ts.Fake.Pause(ctx, sandboxID, templateID)
}

// Create delegates to createFn when set, the fake otherwise.
func (ts *testSub) Create(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
	if ts.createFn != nil {
		return ts.createFn(ctx, req)
	}
	return ts.Fake.Create(ctx, req)
}

func newTestSub() *testSub {
	ts := &testSub{Fake: fake.New(), probeFail: map[string]string{}, rootfsFail: map[string]string{}, rootfsErr: map[string]bool{}, rootfsTimeout: map[string]bool{}}
	ts.Fake.SetExecHandler(ts.exec)
	return ts
}

// isRootfsProbeReq reports whether req is the rootfs liveness probe.
func isRootfsProbeReq(req substrate.ExecRequest) bool {
	return len(req.Args) == 3 && req.Args[2] == rootfsProbe
}

// Exec services the rootfs liveness probe's injectable outcomes and
// delegates everything else to the fake.
func (ts *testSub) Exec(ctx context.Context, sandboxID string, req substrate.ExecRequest) (substrate.ExecResult, error) {
	if ts.execBefore != nil {
		ts.execBefore(sandboxID, req)
	}
	if isRootfsProbeReq(req) {
		ts.rootfsProbeMu.Lock()
		ts.rootfsProbes++
		ts.rootfsProbeMu.Unlock()
		if ts.onRootfsProbe != nil {
			ts.onRootfsProbe(sandboxID)
		}
		if ts.rootfsErr[sandboxID] {
			return substrate.ExecResult{}, fmt.Errorf("exec %s: agent unreachable", sandboxID)
		}
		if ts.rootfsTimeout[sandboxID] {
			// The e2b backend kills a probe that outlives its timer and
			// returns exit 124 with a nil error rather than an error.
			return substrate.ExecResult{Stderr: "[spoond] exec timed out after 10s", ExitCode: 124}, nil
		}
		if msg, bad := ts.rootfsFail[sandboxID]; bad {
			return substrate.ExecResult{Stderr: msg, ExitCode: 1}, nil
		}
		return substrate.ExecResult{ExitCode: 0}, nil
	}
	return ts.Fake.Exec(ctx, sandboxID, req)
}

// RootfsProbeCalls returns how many rootfs liveness probe execs the
// fake has served.
func (ts *testSub) RootfsProbeCalls() int {
	ts.rootfsProbeMu.Lock()
	defer ts.rootfsProbeMu.Unlock()
	return ts.rootfsProbes
}

func (ts *testSub) exec(sandboxID string, req substrate.ExecRequest) substrate.ExecResult {
	args := req.Args
	if len(args) == 3 && args[0] == "/bin/bash" && args[2] == secretsScrubScript {
		removed, left, err := ts.Fake.ScrubSecrets(sandboxID)
		if err != nil {
			return substrate.ExecResult{Stderr: err.Error(), ExitCode: 1}
		}
		if ts.scrubLeftover != "" {
			// Simulate a file the scrub could not remove: the save must
			// abort rather than checkpoint it.
			_ = ts.Fake.WriteFile(context.Background(), sandboxID, secretsDir+"/"+ts.scrubLeftover, []byte("x"), 0o600)
			left = append(left, ts.scrubLeftover)
		}
		out := strings.Join(removed, "\n") + "\n" + scrubSeparator + "\n" + strings.Join(left, "\n")
		if len(left) > 0 {
			out += "\n"
		}
		return substrate.ExecResult{Stdout: out, ExitCode: 0}
	}
	if len(args) == 3 && args[0] == "sh" && args[2] == integrityProbe {
		if reason, bad := ts.probeFail[sandboxID]; bad || ts.probeFailAll {
			return substrate.ExecResult{Stdout: "PROBE_FAIL " + reason + "\n", ExitCode: 1}
		}
		return substrate.ExecResult{Stdout: "PROBE_OK\n"}
	}
	stdout := ts.execStdout
	if stdout == "" {
		stdout = "ok\n"
	}
	return substrate.ExecResult{Stdout: stdout, ExitCode: 0}
}

// sandboxesLive lists the sandbox ids the fake currently tracks.
func (ts *testSub) sandboxesLive(t *testing.T) []string {
	t.Helper()
	sbs, err := ts.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	ids := make([]string, 0, len(sbs))
	for _, sb := range sbs {
		ids = append(ids, sb.ID)
	}
	return ids
}

// calls counts the fake's recorded calls by method name.
func calls(f *fake.Fake, method string) int {
	n := 0
	for _, c := range f.CallLog() {
		if c == method || strings.HasPrefix(c, method+" ") {
			n++
		}
	}
	return n
}

// newTestDB opens a SQLite store in a temp directory.
func newTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "spoond.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// seedImage records an image and its current (ready) build in the catalog.
func seedImage(t *testing.T, db *store.DB, name string, memoryMB int) store.ImageRow {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	img := store.ImageRow{
		Name: name, TemplateID: e2b.NewTemplateID(), CurrentBuildID: e2b.NewUUID(),
		Digest: "localhost:5000/" + name + "@sha256:0123456789abcdef",
		VCPU:   2, MemoryMB: memoryMB, DiskMB: 10240, UpdatedAt: now,
	}
	if err := db.UpsertImage(ctx, img); err != nil {
		t.Fatalf("seed image: %v", err)
	}
	b := store.BuildRow{
		BuildID: img.CurrentBuildID, Kind: "template", TemplateID: img.TemplateID,
		Image: name, State: "ready", VCPU: 2, MemoryMB: memoryMB, DiskMB: 10240,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.InsertBuild(ctx, b); err != nil {
		t.Fatalf("seed build: %v", err)
	}
	return img
}

// newTestService builds a Service over a fake substrate and a temp DB.
// ProxyURL points at an unroutable loopback port: the fake substrate has
// no network, so proxy tests that reach the dial get a 502.
func newTestService(t *testing.T) (*Service, *store.DB, *testSub) {
	t.Helper()
	sub := newTestSub()
	db := newTestDB(t)
	svc := NewService(sub, db, map[string]string{
		"token-a": "consumer-a", "token-b": "consumer-b", "legacy-tok": "legacy-consumer",
	}, ServiceConfig{DefaultTTL: 60 * time.Second, MaxTTL: 10 * time.Minute,
		ProxyURL: "http://127.0.0.1:1"})
	svc.log = log.New(io.Discard, "", 0)
	return svc, db, sub
}

// newTestServer builds a lease API server backed by a fake substrate and
// a temp DB with py-base seeded.
func newTestServer(t *testing.T) (*httptest.Server, *testSub) {
	t.Helper()
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	srv := NewServer(svc, NewImageRegistry(db))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, sub
}

// newTestServerWithService exposes the service as well (for sweeper and
// pool tests).
func newTestServerWithService(t *testing.T) (*httptest.Server, *Service, *store.DB, *testSub) {
	t.Helper()
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	srv := NewServer(svc, NewImageRegistry(db))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, svc, db, sub
}

func doReq(t *testing.T, method, url, token string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, url, &buf)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request %s %s: %v", method, url, err)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	return resp, out
}

func TestCreateAndList(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	if resp.StatusCode != 201 {
		t.Fatalf("create status %d: %v", resp.StatusCode, body)
	}
	id := body["id"].(string)
	if id == "" {
		t.Fatal("expected non-empty id")
	}
	// list mine
	resp, list := doReq(t, "GET", ts.URL+"/api/sandboxes", "token-a", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("list status %d", resp.StatusCode)
	}
	_ = list
	// The list endpoint returns {sandboxes: [...]}; decode it directly.
	req, _ := http.NewRequest("GET", ts.URL+"/api/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer token-a")
	lresp, _ := http.DefaultClient.Do(req)
	var listResp struct {
		Sandboxes []map[string]any `json:"sandboxes"`
	}
	_ = json.NewDecoder(lresp.Body).Decode(&listResp)
	lresp.Body.Close()
	if len(listResp.Sandboxes) != 1 || listResp.Sandboxes[0]["id"] != id {
		t.Fatalf("expected 1 lease with id %s, got %+v", id, listResp.Sandboxes)
	}
}

func TestCreateUnknownImage(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "nope", "ttl": 300})
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404, got %d: %v", resp.StatusCode, body)
	}
}

func TestCreateNoAuth(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes", "", map[string]any{"image": "py-base"})
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestCreateBadToken(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes", "wrong", map[string]any{"image": "py-base"})
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestHealthzNoAuth(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, _ := doReq(t, "GET", ts.URL+"/healthz", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestMetricsRequiresAuth(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, _ := doReq(t, "GET", ts.URL+"/metrics", "", nil)
	if resp.StatusCode != 401 {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestMetricsWithAuth(t *testing.T) {
	ts, _ := newTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer token-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "spoond_leases_active") {
		t.Fatalf("expected service-owned metrics in body, got: %s", raw)
	}
}

func TestExec(t *testing.T) {
	ts, _ := newTestServer(t)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/exec", "token-a", map[string]any{"cmd": "echo hi", "cwd": "/tmp", "env": map[string]string{"FOO": "bar"}})
	if resp.StatusCode != 200 {
		t.Fatalf("exec status %d: %v", resp.StatusCode, body)
	}
	if body["stdout"] != "ok\n" {
		t.Fatalf("unexpected stdout: %v", body["stdout"])
	}
}

// TestExecSandboxGone maps a substrate not-found to 410 Gone so callers
// can distinguish a permanently dead sandbox from a transient failure.
func TestExecSandboxGone(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)
	// The sandbox disappears beneath the lease (e.g. the node forgot it):
	// the substrate answers execs with not-found.
	svc.store.mu.Lock()
	sandboxID := svc.store.leases[id].SandboxID
	svc.store.mu.Unlock()
	sub.Kill(sandboxID)
	sub.FailCall("Exec", 0, substrate.ErrNotFound)
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/exec", "token-a", map[string]any{"cmd": "echo hi"})
	if resp.StatusCode != 410 {
		t.Fatalf("expected 410, got %d: %v", resp.StatusCode, body)
	}
	if body["error"] != "lease no longer exists" {
		t.Fatalf("error body: %v", body["error"])
	}
}

// TestExecBusyIsNotGone: while a lifecycle operation holds the lease
// (the periodic checkpoint pauses the sandbox for the length of the
// snapshot), a substrate not-found is 409 with Retry-After, never 410:
// a client must not abandon a lease that is only checkpointing.
func TestExecBusyIsNotGone(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)
	svc.store.mu.Lock()
	svc.store.leases[id].busy = true
	svc.store.mu.Unlock()
	sub.FailCall("Exec", 0, substrate.ErrNotFound)
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/exec", "token-a", map[string]any{"cmd": "echo hi"})
	if resp.StatusCode != 409 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("busy lease: got %d (Retry-After %q): %v, want 409 with Retry-After", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
}

func TestExecCrossConsumerDenied(t *testing.T) {
	ts, _ := newTestServer(t)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)
	// consumer-b tries to exec into consumer-a's sandbox
	resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/exec", "token-b", map[string]any{"cmd": "echo hi"})
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404 for cross-consumer exec, got %d", resp.StatusCode)
	}
}

func TestDelete(t *testing.T) {
	ts, sub := newTestServer(t)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)
	resp, _ := doReq(t, "DELETE", ts.URL+"/api/sandboxes/"+id, "token-a", nil)
	if resp.StatusCode != 204 {
		t.Fatalf("delete status %d", resp.StatusCode)
	}
	if got := calls(sub.Fake, "Delete"); got != 1 {
		t.Fatalf("expected 1 sandbox delete, got %d", got)
	}
	sbs, err := sub.List(context.Background())
	if err != nil || len(sbs) != 0 {
		t.Fatalf("expected no sandboxes left, got %v (%v)", sbs, err)
	}
	// exec after delete -> 404
	resp, _ = doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/exec", "token-a", map[string]any{"cmd": "echo"})
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404 after delete, got %d", resp.StatusCode)
	}
}

func TestComment(t *testing.T) {
	ts, _ := newTestServer(t)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)

	// set a comment
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/comment", "token-a", map[string]any{"comment": "integration demo box"})
	if resp.StatusCode != 200 {
		t.Fatalf("comment set status %d", resp.StatusCode)
	}
	if body["comment"] != "integration demo box" {
		t.Fatalf("comment echo mismatch: %v", body["comment"])
	}

	// list includes it
	_, list := doReq(t, "GET", ts.URL+"/api/sandboxes", "token-a", nil)
	sbs, _ := list["sandboxes"].([]any)
	if len(sbs) != 1 {
		t.Fatalf("expected 1 sandbox, got %d", len(sbs))
	}
	first, _ := sbs[0].(map[string]any)
	if first["comment"] != "integration demo box" {
		t.Fatalf("list comment mismatch: %v", first["comment"])
	}

	// clear it
	resp, body = doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/comment", "token-a", map[string]any{"comment": ""})
	if resp.StatusCode != 200 || body["comment"] != "" {
		t.Fatalf("comment clear failed: %d %v", resp.StatusCode, body["comment"])
	}

	// cross-consumer denied
	resp, _ = doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/comment", "token-b", map[string]any{"comment": "nope"})
	if resp.StatusCode != 404 {
		t.Fatalf("cross-consumer comment expected 404, got %d", resp.StatusCode)
	}
}

// TestImages lists catalog images with a current build; an image whose
// build is unset is not offered.
func TestImages(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	if err := db.UpsertImage(context.Background(), store.ImageRow{
		Name: "ghost", TemplateID: e2b.NewTemplateID(),
		VCPU: 2, MemoryMB: 2048, DiskMB: 10240, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed unbuilt image: %v", err)
	}
	ts := httptest.NewServer(NewServer(svc, NewImageRegistry(db)).Handler())
	t.Cleanup(ts.Close)

	resp, body := doReq(t, "GET", ts.URL+"/api/images", "token-a", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("images status %d", resp.StatusCode)
	}
	imgs, _ := body["images"].([]any)
	if len(imgs) != 1 || imgs[0] != "py-base" {
		t.Fatalf("expected [py-base], got %v", imgs)
	}

	// detail=1 returns the catalog rows.
	resp, body = doReq(t, "GET", ts.URL+"/api/images?detail=1", "token-a", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("images detail status %d", resp.StatusCode)
	}
	details, _ := body["images"].([]any)
	if len(details) != 1 {
		t.Fatalf("expected 1 detail row, got %v", details)
	}
	row := details[0].(map[string]any)
	for _, k := range []string{"name", "build_id", "template_id", "digest", "vcpu", "memory_mb", "disk_mb", "updated_at"} {
		if _, ok := row[k]; !ok {
			t.Fatalf("detail row missing %q: %v", k, row)
		}
	}
	if row["name"] != "py-base" || row["memory_mb"] != float64(2048) {
		t.Fatalf("detail row: %v", row)
	}
}

// TestTTLSweeper verifies the background sweeper reclaims expired
// leases and deletes the underlying sandbox.
func TestTTLSweeper(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)

	// Create a lease with a 1-second TTL.
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 1})
	id := create["id"].(string)

	// Run the sweeper once with a short tick.
	svc.sweepInterval = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	svc.Start(ctx)
	time.Sleep(1500 * time.Millisecond)
	cancel()

	// The lease should be gone and the sandbox deleted.
	if got := calls(sub.Fake, "Delete"); got != 1 {
		t.Fatalf("expected 1 delete, got %d", got)
	}
	resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/exec", "token-a", map[string]any{"cmd": "echo"})
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404 after TTL expiry, got %d", resp.StatusCode)
	}
}

// TestIdleSweeper verifies persistent leases are auto-suspended (not
// deleted) after IdleTimeout without activity, and that touch() keeps
// them alive.
func TestIdleSweeper(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	svc.cfg.IdleTimeout = 400 * time.Millisecond
	srv := NewServer(svc, NewImageRegistry(db))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Persistent lease; idle timeout is 400ms.
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "persistent": true})
	id := create["id"].(string)

	// Keep it alive with periodic touches (exec counts as activity).
	ctx, cancel := context.WithCancel(context.Background())
	svc.sweepInterval = 50 * time.Millisecond
	svc.Start(ctx)
	for i := 0; i < 6; i++ {
		time.Sleep(150 * time.Millisecond)
		svc.touch(id)
	}
	cancel()

	// Still alive: touches outpace the idle timeout.
	if got := calls(sub.Fake, "Pause"); got != 0 {
		t.Fatalf("expected 0 pauses while touched, got %d", got)
	}

	// Now stop touching; the sweeper should suspend the lease within
	// ~1s. The lease is suspended, not deleted.
	ctx2, cancel2 := context.WithCancel(context.Background())
	svc.Start(ctx2)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && calls(sub.Fake, "Pause") == 0 {
		time.Sleep(100 * time.Millisecond)
	}
	cancel2()
	if got := calls(sub.Fake, "Pause"); got != 1 {
		t.Fatalf("expected 1 idle pause, got %d", got)
	}
	if got := calls(sub.Fake, "Delete"); got != 0 {
		t.Fatalf("expected 0 deletes on idle, got %d (suspended lease should stay)", got)
	}
	// The lease is still listable (suspended, not released).
	resp, _ := doReq(t, "GET", ts.URL+"/api/sandboxes", "token-a", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 listing after suspend, got %d", resp.StatusCode)
	}
}

// TestSuspendResume verifies explicit suspend/resume verbs on a
// persistent lease: suspend pauses the sandbox into a build, resume
// creates again with the same sandbox id, and delete removes it.
func TestSuspendResume(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "persistent": true})
	id := create["id"].(string)
	createsBefore := calls(sub.Fake, "Create")

	// Suspend.
	resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/suspend", "token-a", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 suspend, got %d", resp.StatusCode)
	}
	if got := calls(sub.Fake, "Pause"); got != 1 {
		t.Fatalf("expected 1 pause, got %d", got)
	}
	svc.store.mu.Lock()
	l := svc.store.leases[id]
	suspended := l.Suspended
	resumeBuild := l.ResumeBuildID
	svc.store.mu.Unlock()
	if !suspended || resumeBuild == "" {
		t.Fatalf("expected suspended lease with a resume build, got suspended=%v build=%q", suspended, resumeBuild)
	}

	// Resume.
	resp2, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/resume", "token-a", nil)
	if resp2.StatusCode != 200 {
		t.Fatalf("expected 200 resume, got %d", resp2.StatusCode)
	}
	if got := calls(sub.Fake, "Create"); got != createsBefore+1 {
		t.Fatalf("expected 1 more create after resume, got %d (before %d)", calls(sub.Fake, "Create"), createsBefore)
	}
	svc.store.mu.Lock()
	l = svc.store.leases[id]
	sandboxID := l.SandboxID
	buildID := l.BuildID
	svc.store.mu.Unlock()
	if !l.live() || buildID != resumeBuild {
		t.Fatalf("expected running lease on the resume build, got state=%q build=%q", l.State, buildID)
	}

	// Delete releases the sandbox.
	doReq(t, "DELETE", ts.URL+"/api/sandboxes/"+id, "token-a", nil)
	if got := calls(sub.Fake, "Delete"); got != 1 {
		t.Fatalf("expected 1 delete, got %d", got)
	}
	if got := calls(sub.Fake, "Delete"); got == 1 && !strings.Contains(sub.Fake.CallLog()[len(sub.Fake.CallLog())-1], sandboxID) {
		t.Fatalf("expected the lease's sandbox %s deleted, calls: %v", sandboxID, sub.Fake.CallLog())
	}
}

// TestWarmPoolGrant verifies a grant is served from the warm pool when
// sandboxes are pre-created, and the served lease is marked pooled.
func TestWarmPoolGrant(t *testing.T) {
	svc, db, sub := newTestService(t)
	img := seedImage(t, db, "py-base", 2048)
	svc.cfg.PoolSize = 2
	srv := NewServer(svc, NewImageRegistry(db))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Pre-create 2 sandboxes into the pool.
	ctx := context.Background()
	svc.warmPool(ctx, img)
	svc.store.mu.Lock()
	poolLen := len(svc.store.pool["py-base"])
	svc.store.mu.Unlock()
	if poolLen != 2 {
		t.Fatalf("expected 2 warm sandboxes in pool, got %d", poolLen)
	}
	if got := calls(sub.Fake, "Create"); got != 2 {
		t.Fatalf("expected 2 creates for the pool, got %d", got)
	}

	// Grant should consume from the pool without creating.
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id, _ := create["id"].(string)
	if id == "" {
		t.Fatal("expected a lease id")
	}
	if got := calls(sub.Fake, "Create"); got != 2 {
		t.Fatalf("expected pool grant to not create, creates=%d", got)
	}
	// One pool sandbox consumed, one remains; the served lease is marked
	// pooled and carries the build it runs from.
	svc.store.mu.Lock()
	poolLen = len(svc.store.pool["py-base"])
	l := svc.store.leases[id]
	pooled, buildID := l.pooled, l.BuildID
	svc.store.mu.Unlock()
	if poolLen != 1 {
		t.Fatalf("expected 1 sandbox remaining in pool after grant, got %d", poolLen)
	}
	if !pooled {
		t.Fatal("expected the pool-served lease to be marked pooled")
	}
	if buildID != img.CurrentBuildID {
		t.Fatalf("pooled lease build = %q, want %q", buildID, img.CurrentBuildID)
	}
}

// TestBuildShellArgs verifies cwd is quoted and the command is wrapped in
// a single shell invocation. Env values never enter argv; they travel in
// ExecRequest.Env.
func TestBuildShellArgs(t *testing.T) {
	args := buildShellArgs("echo hi", "/tmp")
	if len(args) != 3 || args[0] != "/bin/bash" || args[1] != "-c" {
		t.Fatalf("unexpected args: %v", args)
	}
	joined := args[2]
	if !strings.Contains(joined, "cd '/tmp' &&") {
		t.Fatalf("expected cd with quoted cwd, got: %s", joined)
	}
	if !strings.Contains(joined, "echo hi") {
		t.Fatalf("expected command preserved, got: %s", joined)
	}
}

// TestBuildShellArgsQuoting verifies embedded single quotes in cwd are
// escaped.
func TestBuildShellArgsQuoting(t *testing.T) {
	args := buildShellArgs("echo", "/tmp/it's")
	joined := args[2]
	if !strings.Contains(joined, `cd '/tmp/it'\''s' &&`) {
		t.Fatalf("expected single-quote escaping, got: %s", joined)
	}
}

// TestBuildShellArgsNoEnvInArgv pins the security fix: neither env keys
// nor values appear anywhere in the argv built for an exec, so nothing
// is readable in the guest's /proc/<pid>/cmdline.
func TestBuildShellArgsNoEnvInArgv(t *testing.T) {
	args := buildShellArgs("echo hi", "/workspace")
	joined := strings.Join(args, "\x00")
	for _, secret := range []string{"AMAIL_TOKEN", "deploy-key-value", "FOO", "bar"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("argv contains env key or value %q: %v", secret, args)
		}
	}
}

// TestExecEnvNotInArgvAndVisibleToCommand drives an exec with env through
// the API and pins both halves of the fix: the request carries the env
// where the process environment can see it, and its argv holds neither
// the key nor the value.
func TestExecEnvNotInArgvAndVisibleToCommand(t *testing.T) {
	ts, _, _, sub := newTestServerWithService(t)
	_, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("create lease: no id in %v", body)
	}

	sub.SetExecHandler(func(sandboxID string, req substrate.ExecRequest) substrate.ExecResult {
		return substrate.ExecResult{Stdout: req.Env["FOO"] + "\n"}
	})
	t.Cleanup(func() { sub.SetExecHandler(nil) })

	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/exec", "token-a",
		map[string]any{"cmd": "echo hi", "env": map[string]string{"FOO": "env-secret-value"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exec status %d: %v", resp.StatusCode, body)
	}
	if stdout, _ := body["stdout"].(string); strings.TrimSpace(stdout) != "env-secret-value" {
		t.Fatalf("command did not see env: stdout=%q", stdout)
	}
	// The recorded request carries the env and its argv carries neither
	// the key nor the value.
	execReq := sub.LastExec()
	if execReq.Env["FOO"] != "env-secret-value" {
		t.Fatalf("ExecRequest.Env = %v, want FOO=env-secret-value", execReq.Env)
	}
	joined := strings.Join(execReq.Args, "\x00")
	if strings.Contains(joined, "FOO") || strings.Contains(joined, "env-secret-value") {
		t.Fatalf("env leaked into argv: %v", execReq.Args)
	}
}

// TestShutdownKeepsLeasesAndPool verifies graceful shutdown stops the
// background loops WITHOUT releasing leases or deleting pooled
// sandboxes: the state persists in the store and the next incarnation
// reloads it (U05).
func TestShutdownKeepsLeasesAndPool(t *testing.T) {
	svc, db, sub := newTestService(t)
	img := seedImage(t, db, "py-base", 2048)
	svc.cfg.PoolSize = 2

	// Grant a lease and warm the pool.
	ctx := context.Background()
	l, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.warmPool(ctx, img) // fills pool to 2
	if got := len(sub.sandboxesLive(t)); got < 2 {
		t.Fatalf("expected >=2 sandboxes after warm, got %d", got)
	}

	svc.Shutdown(ctx)
	if got := calls(sub.Fake, "Delete"); got != 0 {
		t.Fatalf("expected no deletes on shutdown, got %d", got)
	}
	var live bool
	for _, id := range svc.LiveLeases() {
		if id == l.ID {
			live = true
		}
	}
	if !live {
		t.Fatalf("lease %s no longer live after shutdown", l.ID)
	}
}

// TestReconcileOrphansDeletesForeignSandboxes verifies startup
// reconciliation deletes substrate sandboxes that this backend did not
// create (e.g. leftovers from a previous incarnation).
func TestReconcileOrphansDeletesForeignSandboxes(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)

	ctx := context.Background()
	// A foreign sandbox already exists on the substrate (previous incarnation).
	foreign, err := sub.Create(ctx, substrate.CreateRequest{SandboxID: e2b.NewSandboxID()})
	if err != nil {
		t.Fatalf("foreign create: %v", err)
	}
	// Grant our own lease (would be empty at true startup, but proves the
	// mine/not-mine split).
	ours, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}

	svc.ReconcileOrphans(ctx)

	// Foreign deleted, ours kept.
	deleted := false
	for _, c := range sub.Fake.CallLog() {
		if c == "Delete "+foreign.ID {
			deleted = true
		}
	}
	if !deleted {
		t.Fatalf("expected foreign sandbox %s deleted, calls: %v", foreign.ID, sub.Fake.CallLog())
	}
	alive, err := sub.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sb := range alive {
		if sb.ID == ours.SandboxID {
			found = true
		}
	}
	if !found {
		t.Fatalf("our own sandbox %s should not be deleted", ours.SandboxID)
	}
}

// TestGrantDiscardsUnhealthyPooledSandbox verifies grant validates pooled
// sandboxes (envd health) and cold-creates when the pooled one is bad
// (e.g. its guest agent never came up).
func TestGrantDiscardsUnhealthyPooledSandbox(t *testing.T) {
	svc, db, sub := newTestService(t)
	img := seedImage(t, db, "py-base", 2048)
	svc.cfg.PoolSize = 1

	ctx := context.Background()
	// Warm the pool, then mark its member unhealthy.
	svc.warmPool(ctx, img)
	svc.store.mu.Lock()
	pooled := append([]string(nil), svc.store.pool["py-base"]...)
	svc.store.mu.Unlock()
	if len(pooled) != 1 {
		t.Fatalf("expected 1 pooled sandbox, got %d", len(pooled))
	}
	sub.SetHealthErr(pooled[0], fmt.Errorf("envd unreachable"))

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	// The granted sandbox must be a FRESH create (not the stale pooled id).
	if l.SandboxID == pooled[0] {
		t.Fatalf("granted unhealthy pooled sandbox %s", pooled[0])
	}
	if got := calls(sub.Fake, "Delete "+pooled[0]); got != 1 {
		t.Fatalf("expected the unhealthy pooled sandbox to be deleted, calls: %v", sub.Fake.CallLog())
	}
}

// TestRefillPoolWarmsBuiltImages verifies refillPool warms every image
// with a current build (and only those) at startup.
func TestRefillPoolWarmsBuiltImages(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	// An image without a current build must not be warmed.
	if err := db.UpsertImage(context.Background(), store.ImageRow{
		Name: "ghost", TemplateID: e2b.NewTemplateID(),
		VCPU: 2, MemoryMB: 2048, DiskMB: 10240, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	svc.cfg.PoolSize = 1

	svc.refillPool(context.Background())

	svc.store.mu.Lock()
	pyLen := len(svc.store.pool["py-base"])
	ghostLen := len(svc.store.pool["ghost"])
	svc.store.mu.Unlock()
	if pyLen != 1 {
		t.Fatalf("expected py-base warmed, got %d pool entries", pyLen)
	}
	if ghostLen != 0 {
		t.Fatalf("expected no pool entries for the unbuilt image, got %d", ghostLen)
	}
	if got := calls(sub.Fake, "Create"); got != 1 {
		t.Fatalf("expected 1 create, got %d", got)
	}
}

// TestPersistentLeaseSurvivesSweep verifies a persistent lease is not
// reclaimed by the TTL sweeper, even after its initial expiry.
func TestPersistentLeaseSurvivesSweep(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)

	ctx := context.Background()
	l, err := svc.grant(ctx, "c", "py-base", 50*time.Millisecond, true, "", nil, "", "", nil) // persistent, short TTL
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	time.Sleep(80 * time.Millisecond) // pass the initial expiry

	svc.sweepExpired(ctx)
	if svc.lookup("c", l.ID) == nil {
		t.Fatalf("persistent lease was swept despite being persistent")
	}
}

// TestNonPersistentLeaseIsSwept verifies the sweeper still reclaims
// ordinary leases on expiry (regression guard).
func TestNonPersistentLeaseIsSwept(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)

	ctx := context.Background()
	l, err := svc.grant(ctx, "c", "py-base", 50*time.Millisecond, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	time.Sleep(80 * time.Millisecond)

	svc.sweepExpired(ctx)
	if svc.lookup("c", l.ID) != nil {
		t.Fatalf("non-persistent lease should have been swept")
	}
}

// TestKeepAliveExtendsPersistentLease verifies keepalive pushes the
// expiry forward for persistent leases.
func TestKeepAliveExtendsPersistentLease(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)

	ctx := context.Background()
	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	before := l.ExpiresAt
	time.Sleep(5 * time.Millisecond)

	extended, err := svc.keepAlive("c", l.ID, 5*time.Minute)
	if err != nil {
		t.Fatalf("keepAlive: %v", err)
	}
	if !extended.ExpiresAt.After(before) {
		t.Fatalf("expected expiry to extend, before=%v after=%v", before, extended.ExpiresAt)
	}
	// Unknown owner must not be able to extend someone else's lease.
	if _, err := svc.keepAlive("other", l.ID, time.Minute); err == nil {
		t.Fatalf("expected error extending another owner's lease")
	}
}

// TestKeepAliveRejectsNonPersistent verifies keepalive refuses ordinary
// leases (their TTL is fixed by contract).
func TestKeepAliveRejectsNonPersistent(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)

	ctx := context.Background()
	l, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.keepAlive("c", l.ID, time.Minute); err != errNotPersistent {
		t.Fatalf("expected errNotPersistent, got %v", err)
	}
}

// TestLLMGateway verifies the per-lease LLM gateway: capability is the
// lease id in the path, the upstream is hit with the server-side key,
// unknown/suspended leases are rejected, and the /llm/ prefix bypasses
// consumer-token auth.
func TestLLMGateway(t *testing.T) {
	// Fake upstream that records the auth header + model it received.
	var gotAuth string
	var gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.Method == "POST" {
			var body struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotModel = body.Model
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
	}))
	t.Cleanup(upstream.Close)

	svc, db, _ := newTestService(t)
	srv := NewServerWithLLM(svc, NewImageRegistry(db), upstream.URL, "sk-server-secret", "fallback-model", map[string]string{"gpt-oss-20b-fireworks": "gpt-oss:20b"})
	seedImage(t, db, "py-base", 2048)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Create a persistent lease so lookupAny finds it.
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "persistent": true})
	id := create["id"].(string)

	// 1. No bearer token: /llm/ must be auth-exempt (lease id is the cap).
	req, _ := http.NewRequest("POST", ts.URL+"/llm/"+id+"/openai/chat/completions", strings.NewReader(`{"model":"gpt-oss-20b-fireworks"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 (auth-exempt, valid lease), got %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"path":"/chat/completions"`) {
		t.Fatalf("expected upstream path /chat/completions, got %s", body)
	}
	if gotAuth != "Bearer sk-server-secret" {
		t.Fatalf("expected server-side key injected, got %q", gotAuth)
	}
	if gotModel != "gpt-oss:20b" {
		t.Fatalf("expected model remapped to gpt-oss:20b, got %q", gotModel)
	}

	// 1a. Unmapped model falls back to the gateway default.
	req1a, _ := http.NewRequest("POST", ts.URL+"/llm/"+id+"/openai/chat/completions", strings.NewReader(`{"model":"gpt-5.4-nano"}`))
	req1a.Header.Set("Content-Type", "application/json")
	resp1a, err := http.DefaultClient.Do(req1a)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp1a.Body)
	resp1a.Body.Close()
	if resp1a.StatusCode != 200 {
		t.Fatalf("expected 200 for unmapped model, got %d", resp1a.StatusCode)
	}
	if gotModel != "fallback-model" {
		t.Fatalf("expected unmapped model -> default fallback-model, got %q", gotModel)
	}

	// 1b. Multi-segment provider prefix: /fireworks/inference/... strips
	// the whole provider prefix before forwarding.
	req1b, _ := http.NewRequest("POST", ts.URL+"/llm/"+id+"/fireworks/inference/chat/completions", strings.NewReader(`{}`))
	resp1b, err := http.DefaultClient.Do(req1b)
	if err != nil {
		t.Fatal(err)
	}
	body1b, _ := io.ReadAll(resp1b.Body)
	resp1b.Body.Close()
	if resp1b.StatusCode != 200 {
		t.Fatalf("expected 200 for fireworks provider, got %d: %s", resp1b.StatusCode, body1b)
	}
	if !strings.Contains(string(body1b), `"path":"/chat/completions"`) {
		t.Fatalf("expected upstream path /chat/completions for fireworks, got %s", body1b)
	}

	// 1c. Shelley fireworks path carries /v1 under the provider prefix;
	// the upstream base (…/v1) must not be doubled.
	req1c, _ := http.NewRequest("POST", ts.URL+"/llm/"+id+"/fireworks/inference/v1/chat/completions", strings.NewReader(`{}`))
	resp1c, err := http.DefaultClient.Do(req1c)
	if err != nil {
		t.Fatal(err)
	}
	body1c, _ := io.ReadAll(resp1c.Body)
	resp1c.Body.Close()
	if resp1c.StatusCode != 200 {
		t.Fatalf("expected 200 for fireworks /v1 path, got %d: %s", resp1c.StatusCode, body1c)
	}
	if !strings.Contains(string(body1c), `"path":"/v1/chat/completions"`) {
		t.Fatalf("expected upstream path /v1/chat/completions (no double /v1), got %s", body1c)
	}

	// 2. Unknown lease -> 404.
	req2, _ := http.NewRequest("POST", ts.URL+"/llm/deadbeefdeadbeefdeadbeefdeadbeef/openai/chat/completions", strings.NewReader(`{}`))
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 404 {
		t.Fatalf("expected 404 for unknown lease, got %d", resp2.StatusCode)
	}

	// 3. Unsupported provider -> 501.
	req3, _ := http.NewRequest("POST", ts.URL+"/llm/"+id+"/anthropic/v1/messages", strings.NewReader(`{}`))
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != 501 {
		t.Fatalf("expected 501 for anthropic provider, got %d", resp3.StatusCode)
	}

	// 4. Suspended lease -> 409.
	respS, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/suspend", "token-a", nil)
	if respS.StatusCode != 200 {
		t.Fatalf("suspend status %d", respS.StatusCode)
	}
	req4, _ := http.NewRequest("POST", ts.URL+"/llm/"+id+"/openai/chat/completions", strings.NewReader(`{}`))
	resp4, err := http.DefaultClient.Do(req4)
	if err != nil {
		t.Fatal(err)
	}
	resp4.Body.Close()
	if resp4.StatusCode != 409 {
		t.Fatalf("expected 409 for suspended lease, got %d", resp4.StatusCode)
	}

	// 5. Malformed path -> 400.
	req5, _ := http.NewRequest("POST", ts.URL+"/llm/just-a-lease-id", nil)
	resp5, err := http.DefaultClient.Do(req5)
	if err != nil {
		t.Fatal(err)
	}
	resp5.Body.Close()
	if resp5.StatusCode != 400 {
		t.Fatalf("expected 400 for malformed path, got %d", resp5.StatusCode)
	}
}

// TestRestartModesHTTP pins the restart HTTP surface (#120): no mode and
// mode=warm keep the persistent snapshot round-trip (1 resume create, no
// delete), ?mode=cold and the {"mode":"cold"} body run the fresh-guest
// path (delete + create, generation 2), and an unknown mode is 400.
func TestRestartModesHTTP(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	defer ts.Close()

	create := func() string {
		_, m := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{"image": "py-base", "persistent": true})
		id, _ := m["id"].(string)
		if id == "" {
			t.Fatalf("create: no id in %v", m)
		}
		return id
	}
	leaseState := func(id string) *Lease {
		svc.store.mu.Lock()
		defer svc.store.mu.Unlock()
		return svc.store.leases[id]
	}

	// warm: the default — the persistent lease pauses and resumes.
	warmID := create()
	creates := calls(sub.Fake, "Create")
	deletes := calls(sub.Fake, "Delete")
	resp, m := doReq(t, "POST", ts.URL+"/api/leases/"+warmID+"/restart", "token-a", nil)
	if resp.StatusCode != 200 || m["status"] != "running" {
		t.Fatalf("warm restart = %d %v, want 200 running", resp.StatusCode, m)
	}
	if calls(sub.Fake, "Create") != creates+1 || calls(sub.Fake, "Delete") != deletes {
		t.Fatalf("warm restart must pause+resume (1 create, no delete), calls: %v", sub.Fake.CallLog())
	}
	if l := leaseState(warmID); l.Generation != 1 {
		t.Fatalf("generation after warm restart = %d, want 1", l.Generation)
	}
	resp, m = doReq(t, "POST", ts.URL+"/api/leases/"+warmID+"/restart?mode=warm", "token-a", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("explicit mode=warm = %d (%v), want 200", resp.StatusCode, m)
	}

	// cold via query: the fresh-guest path on the same persistent lease.
	coldID := create()
	oldSandbox := leaseState(coldID).SandboxID
	resp, m = doReq(t, "POST", ts.URL+"/api/leases/"+coldID+"/restart?mode=cold", "token-a", nil)
	if resp.StatusCode != 200 || m["status"] != "running" {
		t.Fatalf("cold restart = %d %v, want 200 running", resp.StatusCode, m)
	}
	if m["id"] != coldID {
		t.Fatalf("cold restart answered id %v, want the lease id kept (%s)", m["id"], coldID)
	}
	if got := calls(sub.Fake, "Delete "+oldSandbox); got != 1 {
		t.Fatalf("cold restart must delete the old sandbox, calls: %v", sub.Fake.CallLog())
	}
	l := leaseState(coldID)
	if l.SandboxID == oldSandbox || !l.live() || l.Generation != 2 || !l.Persistent {
		t.Fatalf("cold-restarted lease = %+v, want a fresh running sandbox, generation 2, still persistent", l)
	}

	// cold via body, on a suspended lease: comes back running.
	bodyID := create()
	if _, err := svc.suspend(t.Context(), "consumer-a", bodyID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	oldSandbox = leaseState(bodyID).SandboxID
	resp, m = doReq(t, "POST", ts.URL+"/api/leases/"+bodyID+"/restart", "token-a", map[string]any{"mode": "cold"})
	if resp.StatusCode != 200 {
		t.Fatalf("cold via body = %d (%v), want 200", resp.StatusCode, m)
	}
	if l := leaseState(bodyID); !l.live() || l.Suspended || l.SandboxID == oldSandbox || l.Generation != 2 {
		t.Fatalf("suspended lease cold-restarted via body = %+v, want running on a fresh sandbox, generation 2", l)
	}

	// An unknown mode is 400, and the lease is untouched.
	badID := create()
	resp, m = doReq(t, "POST", ts.URL+"/api/leases/"+badID+"/restart?mode=hot", "token-a", nil)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(m["error"].(string), "warm or cold") {
		t.Fatalf("mode=hot = %d (%v), want 400 mentioning warm or cold", resp.StatusCode, m)
	}
	resp, _ = doReq(t, "POST", ts.URL+"/api/leases/"+badID+"/restart", "token-a", map[string]any{"mode": "scorched"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("body mode=scorched = %d, want 400", resp.StatusCode)
	}
	if l := leaseState(badID); l.Generation != 1 || !l.live() {
		t.Fatalf("a rejected mode must leave the lease alone: %+v", l)
	}
}

// TestTagAndRestart covers the ctl surface: friendly names (tag) and
// reboot (restart) on persistent leases.
func TestTagAndRestart(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	defer ts.Close()

	create := func() string {
		_, m := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "persistent": true})
		id, _ := m["id"].(string)
		if id == "" {
			t.Fatalf("create: no id in %v", m)
		}
		return id
	}
	id := create()

	// tag: assign a friendly name, then resolve it back.
	_, m := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/tag", "token-a", map[string]any{"name": "webby"})
	if m["ok"] != true || m["name"] != "webby" {
		t.Fatalf("tag response: %v", m)
	}
	// duplicate name on a second lease must fail.
	id2 := create()
	resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id2+"/tag", "token-a", map[string]any{"name": "webby"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate name: status %d, want 400", resp.StatusCode)
	}
	// name resolution endpoint.
	_, m = doReq(t, "GET", ts.URL+"/api/names/webby", "token-a", nil)
	if m["id"] != id {
		t.Fatalf("names response: %v", m)
	}
	// invalid name rejected.
	resp, _ = doReq(t, "POST", ts.URL+"/api/sandboxes/"+id2+"/tag", "token-a", map[string]any{"name": "Bad Name!"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid name: status %d, want 400", resp.StatusCode)
	}

	// restart: a running persistent lease suspends then resumes.
	createsBefore := calls(sub.Fake, "Create")
	_, m = doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/restart", "token-a", map[string]any{})
	if m["status"] != "running" {
		t.Fatalf("restart response: %v", m)
	}
	if calls(sub.Fake, "Create") != createsBefore+1 {
		t.Fatalf("expected the restart to resume the sandbox (1 new create)")
	}
	svc.store.mu.Lock()
	l := svc.store.leases[id]
	svc.store.mu.Unlock()
	if !l.live() {
		t.Fatalf("restarted lease not running: %+v", l)
	}
	// list still contains both, with names.
	_, m = doReq(t, "GET", ts.URL+"/api/sandboxes", "token-a", nil)
	sbs, _ := m["sandboxes"].([]any)
	if len(sbs) != 2 {
		t.Fatalf("want 2 sandboxes, got %d", len(sbs))
	}
	// prompt endpoint exists and answers (fake exec returns "ok";
	// the SHELLEY_NOT_RUNNING 409 path needs a live agent, covered by the
	// integration suite).
	resp, m = doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/prompt", "token-a", map[string]any{"message": "hi"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("prompt: status %d, want 200 (fake exec canned)", resp.StatusCode)
	}
	if _, ok := m["reply"]; !ok {
		t.Fatalf("prompt response missing reply: %v", m)
	}
}

func TestStat(t *testing.T) {
	ts, _, _, sub := newTestServerWithService(t)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)

	// fake exec returns canned stat probe output.
	sub.execStdout = `== loadavg ==
0.25 0.10 0.05 1/12 345
== meminfo ==
MemTotal:       1572864 kB
MemAvailable:    524288 kB
== netdev ==
eth0: 1000 0 0 0 0 0 0 0 2000 0 0 0 0 0 0 0
lo: 999 0 0 0 0 0 0 0 888 0 0 0 0 0 0 0
== df ==
Filesystem     1K-blocks    Used Available Use% Mounted on
/dev/root        4194304   1048576   3145728  25% /
`
	resp, body := doReq(t, "GET", ts.URL+"/api/sandboxes/"+id+"/stat", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stat status %d: %v", resp.StatusCode, body)
	}
	cpu, _ := body["cpu"].(map[string]any)
	if cpu["load1"] != 0.25 {
		t.Fatalf("load1: %v", cpu["load1"])
	}
	mem, _ := body["mem"].(map[string]any)
	if mem["total_mib"] != float64(1536) || mem["used_mib"] != float64(1024) {
		t.Fatalf("mem: %v", mem)
	}
	disk, _ := body["disk"].(map[string]any)
	if disk["total_mib"] != float64(4096) || disk["used_mib"] != float64(1024) {
		t.Fatalf("disk: %v", disk)
	}
	net, _ := body["net"].(map[string]any)
	// eth0 rx=1000 tx=2000; lo excluded.
	if net["rx_bytes"] != float64(1000) || net["tx_bytes"] != float64(2000) {
		t.Fatalf("net: %v", net)
	}
}

func TestStatCrossConsumerDenied(t *testing.T) {
	ts, _ := newTestServer(t)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)
	resp, _ := doReq(t, "GET", ts.URL+"/api/sandboxes/"+id+"/stat", "token-b", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-consumer stat: status %d, want 404", resp.StatusCode)
	}
}

func TestParseStatProbe(t *testing.T) {
	// minimal / partial probe output must still parse
	out := `== loadavg ==
0.75 0.20 0.10 2/40 900
== meminfo ==
MemTotal:        2097152 kB
MemAvailable:    1048576 kB
== netdev ==
== df ==
`
	st, err := parseStatProbe(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if st.CPU.Load1 != 0.75 {
		t.Fatalf("load1: %v", st.CPU.Load1)
	}
	if st.Mem.TotalMiB != 2048 || st.Mem.UsedMiB != 1024 {
		t.Fatalf("mem: %+v", st.Mem)
	}
	// garbage -> error
	if _, err := parseStatProbe("nothing useful"); err == nil {
		t.Fatal("expected error for empty probe")
	}
}
