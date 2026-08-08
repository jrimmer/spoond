package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jrimmer/hyper-forgejo-runner/forkd"
)

// fakeForkd is a minimal in-memory forkd controller for tests.
type fakeForkd struct {
	snapshots []forkd.SnapshotInfo
	sandboxes map[string]forkd.SandboxInfo
	nextID    int
	killed    []string
}

func newFakeForkd() *fakeForkd {
	return &fakeForkd{
		snapshots: []forkd.SnapshotInfo{{Tag: "py-base", Bootable: true}},
		sandboxes: make(map[string]forkd.SandboxInfo),
	}
}

func (f *fakeForkd) ListSnapshots(ctx context.Context) ([]forkd.SnapshotInfo, error) {
	return f.snapshots, nil
}

func (f *fakeForkd) Spawn(ctx context.Context, tag string, n int, perChildNetns bool, memoryLimitMiB int) ([]forkd.SandboxInfo, error) {
	var out []forkd.SandboxInfo
	for i := 0; i < n; i++ {
		id := "sb-" + string(rune('a'+f.nextID))
		f.nextID++
		f.sandboxes[id] = forkd.SandboxInfo{ID: id, SnapshotTag: tag, GuestAddr: "10.42.0.2:8888"}
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

func (f *fakeForkd) Ping(ctx context.Context, id string) error { return nil }

// newTestServer builds a lease API server backed by a fake forkd.
func newTestServer(t *testing.T) (*httptest.Server, *fakeForkd) {
	t.Helper()
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"token-a": "consumer-a", "token-b": "consumer-b"}, 0, 60*time.Second, 10*time.Minute)
	reg := NewImageRegistry(ff, time.Minute)
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
	// The list endpoint returns a bare array; decode it directly.
	req, _ := http.NewRequest("GET", ts.URL+"/api/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer token-a")
	lresp, _ := http.DefaultClient.Do(req)
	var leases []map[string]any
	_ = json.NewDecoder(lresp.Body).Decode(&leases)
	lresp.Body.Close()
	if len(leases) != 1 || leases[0]["id"] != id {
		t.Fatalf("expected 1 lease with id %s, got %+v", id, leases)
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
