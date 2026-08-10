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
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/forkd-service/forkd"
)

// fakeForkd is a minimal in-memory forkd controller for tests.
type fakeForkd struct {
	snapshots     []forkd.SnapshotInfo
	sandboxes     map[string]forkd.SandboxInfo
	workspaces    map[string]*forkd.WorkspaceInfo
	nextID        int
	killed        []string
	suspended     []string
	resumed       []string
	deadSandboxes map[string]bool
	netns         string // reported for spawned sandboxes ("" = none)
}

func newFakeForkd() *fakeForkd {
	return &fakeForkd{
		snapshots:  []forkd.SnapshotInfo{{Tag: "py-base", Bootable: true}},
		sandboxes:  make(map[string]forkd.SandboxInfo),
		workspaces: make(map[string]*forkd.WorkspaceInfo),
	}
}

func (f *fakeForkd) ListSnapshots(ctx context.Context) ([]forkd.SnapshotInfo, error) {
	return f.snapshots, nil
}

func (f *fakeForkd) SnapshotExists(ctx context.Context, tag string) (bool, error) {
	for _, s := range f.snapshots {
		if s.Tag == tag {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeForkd) Spawn(ctx context.Context, tag string, n int, perChildNetns bool, memoryLimitMiB int) ([]forkd.SandboxInfo, error) {
	var out []forkd.SandboxInfo
	for i := 0; i < n; i++ {
		id := "sb-" + string(rune('a'+f.nextID))
		f.nextID++
		f.sandboxes[id] = forkd.SandboxInfo{ID: id, SnapshotTag: tag, GuestAddr: "10.42.0.2:8888", Netns: f.netns}
		out = append(out, f.sandboxes[id])
	}
	return out, nil
}

func (f *fakeForkd) ListSandboxes(ctx context.Context) ([]forkd.SandboxInfo, error) {
	var out []forkd.SandboxInfo
	for _, s := range f.sandboxes {
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeForkd) Kill(ctx context.Context, id string) error {
	f.killed = append(f.killed, id)
	delete(f.sandboxes, id)
	return nil
}

func (f *fakeForkd) Exec(ctx context.Context, id string, args []string, timeoutSecs int) (*forkd.ExecResult, error) {
	return &forkd.ExecResult{Stdout: "ok\n", Stderr: "", ExitCode: 0}, nil
}

func (f *fakeForkd) Ping(ctx context.Context, id string) error {
	if f.deadSandboxes != nil && f.deadSandboxes[id] {
		return fmt.Errorf("forkd: sandbox %s not found (status 404)", id)
	}
	return nil
}

func (f *fakeForkd) Branch(ctx context.Context, id, tag string) (string, error) {
	if f.deadSandboxes != nil && f.deadSandboxes[id] {
		return "", fmt.Errorf("forkd: sandbox %s not found (status 404)", id)
	}
	branchTag := tag
	if branchTag == "" {
		branchTag = "branch-" + id
	}
	f.snapshots = append(f.snapshots, forkd.SnapshotInfo{Tag: branchTag})
	return branchTag, nil
}

func (f *fakeForkd) CreateWorkspace(ctx context.Context, name, tag string, perChildNetns bool) (*forkd.WorkspaceInfo, error) {
	f.nextID++
	sbID := fmt.Sprintf("sb-%d", f.nextID)
	ws := &forkd.WorkspaceInfo{
		ID:                "ws-" + sbID,
		Name:              name,
		SourceSnapshotTag: tag,
		Status:            "running",
		LiveSandboxID:     sbID,
	}
	f.workspaces[name] = ws
	f.sandboxes[sbID] = forkd.SandboxInfo{ID: sbID, GuestAddr: "10.42.0." + fmt.Sprint(f.nextID+1) + ":8888"}
	return ws, nil
}

func (f *fakeForkd) SuspendWorkspace(ctx context.Context, name string) error {
	ws, ok := f.workspaces[name]
	if !ok {
		return fmt.Errorf("forkd: workspace %s not found (status 404)", name)
	}
	ws.Status = "suspended"
	f.suspended = append(f.suspended, name)
	return nil
}

func (f *fakeForkd) ResumeWorkspace(ctx context.Context, name string) (*forkd.WorkspaceInfo, error) {
	ws, ok := f.workspaces[name]
	if !ok {
		return nil, fmt.Errorf("forkd: workspace %s not found (status 404)", name)
	}
	f.nextID++
	newID := fmt.Sprintf("sb-%d", f.nextID)
	ws.Status = "running"
	ws.LiveSandboxID = newID
	f.sandboxes[newID] = forkd.SandboxInfo{ID: newID, GuestAddr: "10.42.0." + fmt.Sprint(f.nextID+1) + ":8888"}
	f.resumed = append(f.resumed, name)
	return ws, nil
}

func (f *fakeForkd) DeleteWorkspace(ctx context.Context, name string) error {
	ws, ok := f.workspaces[name]
	if !ok {
		return fmt.Errorf("forkd: workspace %s not found (status 404)", name)
	}
	delete(f.workspaces, name)
	f.killed = append(f.killed, ws.LiveSandboxID)
	return nil
}

func (f *fakeForkd) Metrics(ctx context.Context) ([]byte, error) {
	return []byte("# HELP forkd_sandboxes_active\n# TYPE forkd_sandboxes_active gauge\nforkd_sandboxes_active 0\n"), nil
}

// newTestServer builds a lease API server backed by a fake forkd.
func newTestServer(t *testing.T) (*httptest.Server, *fakeForkd) {
	t.Helper()
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"token-a": "consumer-a", "token-b": "consumer-b"}, 0, 60*time.Second, 10*time.Minute)
	reg := NewImageRegistry(ff, "py-base")
	srv := NewServer(svc, reg)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, ff
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
	if !strings.Contains(string(raw), "forkd_sandboxes_active") {
		t.Fatalf("expected forkd metrics in body, got: %s", raw)
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
	ts, ff := newTestServer(t)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)
	resp, _ := doReq(t, "DELETE", ts.URL+"/api/sandboxes/"+id, "token-a", nil)
	if resp.StatusCode != 204 {
		t.Fatalf("delete status %d", resp.StatusCode)
	}
	if len(ff.killed) != 1 {
		t.Fatalf("expected 1 kill, got %d", len(ff.killed))
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

func TestImages(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, body := doReq(t, "GET", ts.URL+"/api/images", "token-a", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("images status %d", resp.StatusCode)
	}
	imgs, _ := body["images"].([]any)
	if len(imgs) != 1 || imgs[0] != "py-base" {
		t.Fatalf("expected [py-base], got %v", imgs)
	}
}

// TestTTLSweeper verifies the background sweeper reclaims expired
// leases and kills the underlying sandbox.
func TestTTLSweeper(t *testing.T) {
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"token-a": "consumer-a"}, 0, 60*time.Second, 10*time.Minute)
	reg := NewImageRegistry(ff, "py-base")
	srv := NewServer(svc, reg)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Create a lease with a 1-second TTL.
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 1})
	id := create["id"].(string)

	// Run the sweeper once with a short tick.
	svc.sweepInterval = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	svc.Start(ctx)
	time.Sleep(1500 * time.Millisecond)
	cancel()

	// The lease should be gone and the sandbox killed.
	if len(ff.killed) != 1 {
		t.Fatalf("expected 1 kill, got %d", len(ff.killed))
	}
	resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/exec", "token-a", map[string]any{"cmd": "echo"})
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404 after TTL expiry, got %d", resp.StatusCode)
	}
}

// TestIdleSweeper verifies persistent leases are auto-suspended (not
// deleted) after idleTimeout without activity, and that touch() keeps
// them alive.
func TestIdleSweeper(t *testing.T) {
	ff := newFakeForkd()
	svc := NewServiceWithIdle(ff, map[string]string{"token-a": "consumer-a"}, 0, 60*time.Second, 10*time.Minute, 400*time.Millisecond)
	reg := NewImageRegistry(ff, "py-base")
	srv := NewServer(svc, reg)
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
	if len(ff.suspended) != 0 {
		t.Fatalf("expected 0 suspends while touched, got %d", len(ff.suspended))
	}

	// Now stop touching; the sweeper should suspend the workspace within
	// ~1s. The lease is workspace-backed, so it is suspended, not killed.
	ctx2, cancel2 := context.WithCancel(context.Background())
	svc.Start(ctx2)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(ff.suspended) == 0 {
		time.Sleep(100 * time.Millisecond)
	}
	cancel2()
	if len(ff.suspended) != 1 {
		t.Fatalf("expected 1 idle suspend, got %d", len(ff.suspended))
	}
	if len(ff.killed) != 0 {
		t.Fatalf("expected 0 kills on idle, got %d (suspended lease should stay)", len(ff.killed))
	}
	// The lease is still listable (suspended, not released).
	resp, _ := doReq(t, "GET", ts.URL+"/api/sandboxes", "token-a", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 listing after suspend, got %d", resp.StatusCode)
	}
	if len(ff.suspended) != 1 {
		t.Fatalf("expected 1 idle suspend, got %d", len(ff.suspended))
	}
}

// TestSuspendResume verifies explicit suspend/resume verbs on a
// workspace-backed persistent lease, and that resume refreshes the
// sandbox id.
func TestSuspendResume(t *testing.T) {
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"token-a": "consumer-a"}, 0, 60*time.Second, 10*time.Minute)
	reg := NewImageRegistry(ff, "py-base")
	srv := NewServer(svc, reg)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "persistent": true})
	id := create["id"].(string)

	// Suspend.
	resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/suspend", "token-a", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 suspend, got %d", resp.StatusCode)
	}
	if len(ff.suspended) != 1 {
		t.Fatalf("expected 1 suspend, got %d", len(ff.suspended))
	}

	// Resume.
	resp2, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/resume", "token-a", nil)
	if resp2.StatusCode != 200 {
		t.Fatalf("expected 200 resume, got %d", resp2.StatusCode)
	}
	if len(ff.resumed) != 1 {
		t.Fatalf("expected 1 resume, got %d", len(ff.resumed))
	}

	// Delete releases the workspace.
	doReq(t, "DELETE", ts.URL+"/api/sandboxes/"+id, "token-a", nil)
	if len(ff.killed) != 1 {
		t.Fatalf("expected 1 workspace delete/kill, got %d", len(ff.killed))
	}
}

// TestWarmPoolGrant verifies a grant is served from the warm pool when
// sandboxes are pre-forked.
func TestWarmPoolGrant(t *testing.T) {
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"token-a": "consumer-a"}, 2, 60*time.Second, 10*time.Minute)
	reg := NewImageRegistry(ff, "py-base")
	srv := NewServer(svc, reg)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Pre-fork 2 sandboxes into the pool.
	ctx := context.Background()
	svc.warmPool(ctx, "py-base")
	svc.store.mu.Lock()
	poolLen := len(svc.store.pool["py-base"])
	svc.store.mu.Unlock()
	if poolLen != 2 {
		t.Fatalf("expected 2 warm sandboxes in pool, got %d", poolLen)
	}

	// Grant should consume from the pool without spawning.
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	if create["id"] == "" {
		t.Fatal("expected a lease id")
	}
	// One pool sandbox consumed, one remains.
	svc.store.mu.Lock()
	poolLen = len(svc.store.pool["py-base"])
	svc.store.mu.Unlock()
	if poolLen != 1 {
		t.Fatalf("expected 1 sandbox remaining in pool after grant, got %d", poolLen)
	}
}

// TestBuildShellArgs verifies cwd/env are quoted and the command is
// wrapped in a single shell invocation.
func TestBuildShellArgs(t *testing.T) {
	args := buildShellArgs("echo hi", "/tmp", map[string]string{"FOO": "bar"})
	if len(args) != 3 || args[0] != "/bin/bash" || args[1] != "-c" {
		t.Fatalf("unexpected args: %v", args)
	}
	joined := args[2]
	if !strings.Contains(joined, "cd '/tmp' &&") {
		t.Fatalf("expected cd with quoted cwd, got: %s", joined)
	}
	if !strings.Contains(joined, "export 'FOO'='bar';") {
		t.Fatalf("expected export with quoted key and value, got: %s", joined)
	}
	if !strings.Contains(joined, "echo hi") {
		t.Fatalf("expected command preserved, got: %s", joined)
	}
}

// TestBuildShellArgsQuoting verifies embedded single quotes are escaped.
func TestBuildShellArgsQuoting(t *testing.T) {
	args := buildShellArgs("echo", "", map[string]string{"X": "it's"})
	joined := args[2]
	if !strings.Contains(joined, `export 'X'='it'\''s';`) {
		t.Fatalf("expected single-quote escaping, got: %s", joined)
	}
}

// TestShutdownKillsLeasesAndPool verifies graceful shutdown releases
// every lease and pooled sandbox so a backend restart never orphans VMs.
func TestShutdownKillsLeasesAndPool(t *testing.T) {
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"t": "c"}, 2, time.Minute, 10*time.Minute)
	svc.log = log.New(io.Discard, "", 0)

	// Grant a lease and warm the pool.
	ctx := context.Background()
	l, err := svc.grant(ctx, "c", "py-base", 0, time.Minute, false, "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.warmPool(ctx, "py-base") // fills pool to 2
	if got := len(ff.sandboxes); got < 2 {
		t.Fatalf("expected >=2 sandboxes after warm, got %d", got)
	}

	svc.Shutdown(ctx)
	// Every sandbox the fake ever created should be killed:
	// l.ForkdID + 2 pooled = 3.
	_ = l
	if len(ff.killed) < 3 {
		t.Fatalf("expected >=3 kills (lease + 2 pool), got %d: %v", len(ff.killed), ff.killed)
	}
	if len(ff.sandboxes) != 0 {
		t.Fatalf("expected all sandboxes killed, %d remain", len(ff.sandboxes))
	}
}

// TestReconcileOrphansKillsForeignSandboxes verifies startup
// reconciliation kills controller sandboxes that this backend did not
// create (e.g. leftovers from a previous incarnation).
func TestReconcileOrphansKillsForeignSandboxes(t *testing.T) {
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"t": "c"}, 2, time.Minute, 10*time.Minute)
	svc.log = log.New(io.Discard, "", 0)

	ctx := context.Background()
	// A foreign sandbox already exists in the controller (previous incarnation).
	foreign, err := ff.Spawn(ctx, "py-base", 1, true, 0)
	if err != nil {
		t.Fatalf("foreign spawn: %v", err)
	}
	// Grant our own lease (would be empty at true startup, but proves the
	// mine/not-mine split).
	ours, err := svc.grant(ctx, "c", "py-base", 0, time.Minute, false, "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}

	svc.ReconcileOrphans(ctx)

	// Foreign killed, ours kept.
	killedForeign := false
	for _, id := range ff.killed {
		if id == foreign[0].ID {
			killedForeign = true
		}
	}
	if !killedForeign {
		t.Fatalf("expected foreign sandbox %s killed, killed: %v", foreign[0].ID, ff.killed)
	}
	if _, stillAlive := ff.sandboxes[ours.ForkdID]; !stillAlive {
		t.Fatalf("our own sandbox %s should not be killed", ours.ForkdID)
	}
}

// TestGrantDropsStalePooledSandbox verifies grant validates pooled
// sandboxes against the controller and cold-spawns when the pooled one
// is gone (e.g. after a controller restart).
func TestGrantDropsStalePooledSandbox(t *testing.T) {
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"t": "c"}, 2, time.Minute, 10*time.Minute)
	svc.log = log.New(io.Discard, "", 0)

	ctx := context.Background()
	// Warm the pool, then mark its member stale (as if the controller
	// restarted and forgot it).
	svc.warmPool(ctx, "py-base")
	ff.deadSandboxes = map[string]bool{}
	for id := range ff.sandboxes {
		ff.deadSandboxes[id] = true
	}

	l, err := svc.grant(ctx, "c", "py-base", 0, time.Minute, false, "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	// The granted sandbox must be a FRESH spawn (not the stale pooled id).
	if ff.deadSandboxes[l.ForkdID] {
		t.Fatalf("granted stale pooled sandbox %s", l.ForkdID)
	}
	if len(ff.killed) == 0 {
		t.Fatalf("expected stale pooled sandbox to be killed")
	}
}

// TestNewServiceSeedsPoolFromKnownImages verifies NewService registers
// known images so refillPool warms all of them at startup (not just the
// ones that happen to get granted).
func TestNewServiceSeedsPoolFromKnownImages(t *testing.T) {
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"t": "c"}, 2, time.Minute, 10*time.Minute, "py-base", "go-base", "elixir-base", "llm-review")
	if len(svc.store.pool) != 4 {
		t.Fatalf("expected 4 seeded images, got %d: %v", len(svc.store.pool), svc.store.pool)
	}
	// Empty pool entries must exist so refillPool sees cur=0 < poolSize.
	for _, img := range []string{"py-base", "go-base", "elixir-base", "llm-review"} {
		if _, ok := svc.store.pool[img]; !ok {
			t.Fatalf("image %s not seeded", img)
		}
	}
}

// TestPersistentLeaseSurvivesSweep verifies a persistent lease is not
// reclaimed by the TTL sweeper, even after its initial expiry.
func TestPersistentLeaseSurvivesSweep(t *testing.T) {
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"t": "c"}, 2, time.Minute, 10*time.Minute)
	svc.log = log.New(io.Discard, "", 0)

	ctx := context.Background()
	l, err := svc.grant(ctx, "c", "py-base", 0, 50*time.Millisecond, true, "", nil) // persistent, short TTL
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
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"t": "c"}, 2, time.Minute, 10*time.Minute)
	svc.log = log.New(io.Discard, "", 0)

	ctx := context.Background()
	l, err := svc.grant(ctx, "c", "py-base", 0, 50*time.Millisecond, false, "", nil)
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
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"t": "c"}, 2, time.Minute, 10*time.Minute)
	svc.log = log.New(io.Discard, "", 0)

	ctx := context.Background()
	l, err := svc.grant(ctx, "c", "py-base", 0, time.Minute, true, "", nil)
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
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"t": "c"}, 2, time.Minute, 10*time.Minute)
	svc.log = log.New(io.Discard, "", 0)

	ctx := context.Background()
	l, err := svc.grant(ctx, "c", "py-base", 0, time.Minute, false, "", nil)
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

	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"token-a": "consumer-a"}, 0, 60*time.Second, 10*time.Minute)
	reg := NewImageRegistry(ff, "py-base")
	srv := NewServerWithLLM(svc, reg, upstream.URL, "sk-server-secret", "fallback-model", map[string]string{"gpt-oss-20b-fireworks": "gpt-oss:20b"})
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

// TestTagAndRestart covers the new ctl surface: friendly names (tag) and
// reboot (restart) on workspace-backed persistent leases.
func TestTagAndRestart(t *testing.T) {
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"token-a": "consumer-a"}, 0, 60*time.Second, 10*time.Minute)
	reg := NewImageRegistry(ff, "py-base")
	srv := NewServer(svc, reg)
	ts := httptest.NewServer(srv.Handler())
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

	// restart: workspace-backed lease -> suspend+resume, still listable.
	_, m = doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/restart", "token-a", map[string]any{})
	if m["status"] != "running" {
		t.Fatalf("restart response: %v", m)
	}
	// list still contains both, with names.
	_, m = doReq(t, "GET", ts.URL+"/api/sandboxes", "token-a", nil)
	sbs, _ := m["sandboxes"].([]any)
	if len(sbs) != 2 {
		t.Fatalf("want 2 sandboxes, got %d", len(sbs))
	}
	// prompt endpoint exists and answers (fake exec returns empty output;
	// the SHELLEY_NOT_RUNNING 409 path needs a live agent, covered by the
	// integration suite).
	resp, m = doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/prompt", "token-a", map[string]any{"message": "hi"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("prompt: status %d, want 200 (fake exec empty)", resp.StatusCode)
	}
	if _, ok := m["reply"]; !ok {
		t.Fatalf("prompt response missing reply: %v", m)
	}
}
