package commandadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jrimmer/forkd-service/runner"
)

// fakeSandbox is an in-memory SandboxProvider for tests.
type fakeSandbox struct {
	created []string
	deleted []string
	results map[string]*runner.ExecResult // keyed by cmd
	// blockExec, if non-nil, blocks Exec until closed (for quota tests).
	blockExec chan struct{}
	// started is closed when a blocking Exec begins.
	started chan struct{}
}

func newFakeSandbox() *fakeSandbox {
	return &fakeSandbox{results: map[string]*runner.ExecResult{}}
}

func (f *fakeSandbox) Create(ctx context.Context, image string, ttl int) (string, error) {
	if image == "no-such-image" {
		return "", errNoSuchImage
	}
	id := "sb-" + image
	f.created = append(f.created, id)
	return id, nil
}

var errNoSuchImage = &fakeErr{"no such image"}

type fakeErr struct{ msg string }

func (e *fakeErr) Error() string { return e.msg }

func (f *fakeSandbox) Exec(ctx context.Context, id, cmd, cwd string, env map[string]string, timeout int) (*runner.ExecResult, error) {
	if f.blockExec != nil {
		if f.started != nil {
			close(f.started)
		}
		<-f.blockExec
	}
	if r, ok := f.results[cmd]; ok {
		return r, nil
	}
	return &runner.ExecResult{Stdout: "ok\n", Exit: 0}, nil
}

func (f *fakeSandbox) Delete(ctx context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func newTestServer(maxConcurrent int) *httptest.Server {
	sb := newFakeSandbox()
	s := New(Config{
		Sandbox:       sb,
		Tokens:        map[string]string{"token-a": "consumer-a"},
		MaxConcurrent: maxConcurrent,
		DefaultTTL:    300,
		MaxTTL:        3600,
		MaxTimeout:    300,
		DefaultImage:  "py-base",
	})
	return httptest.NewServer(s.Handler())
}

func doRun(t *testing.T, url, token string, body map[string]any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/run", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestRunHappyPath(t *testing.T) {
	ts := newTestServer(0)
	defer ts.Close()
	code, out := doRun(t, ts.URL, "token-a", map[string]any{
		"image":   "py-base",
		"command": "echo hi",
	})
	if code != 200 {
		t.Fatalf("expected 200, got %d: %v", code, out)
	}
	if out["stdout"] != "ok\n" {
		t.Fatalf("expected stdout 'ok\\n', got %v", out["stdout"])
	}
	if out["exit"] != float64(0) {
		t.Fatalf("expected exit 0, got %v", out["exit"])
	}
	if out["job_id"] == "" {
		t.Fatal("expected job_id")
	}
}

func TestRunUnauthorized(t *testing.T) {
	ts := newTestServer(0)
	defer ts.Close()
	code, out := doRun(t, ts.URL, "bad-token", map[string]any{"command": "echo hi"})
	if code != 401 {
		t.Fatalf("expected 401, got %d", code)
	}
	if out["error"] != "unauthorized" {
		t.Fatalf("expected unauthorized error, got %v", out["error"])
	}
}

func TestRunMissingCommand(t *testing.T) {
	ts := newTestServer(0)
	defer ts.Close()
	code, out := doRun(t, ts.URL, "token-a", map[string]any{"image": "py-base"})
	if code != 400 {
		t.Fatalf("expected 400, got %d", code)
	}
	if out["error"] != "command is required" {
		t.Fatalf("expected command required error, got %v", out["error"])
	}
}

func TestRunQuota(t *testing.T) {
	// Use a blocking sandbox so the first request holds its slot while
	// the second arrives.
	sb := newFakeSandbox()
	block := make(chan struct{})
	sb.blockExec = block
	sb.started = make(chan struct{})
	s := New(Config{
		Sandbox:       sb,
		Tokens:        map[string]string{"token-a": "consumer-a"},
		MaxConcurrent: 1,
		DefaultTTL:    300,
		MaxTTL:        3600,
		MaxTimeout:    300,
		DefaultImage:  "py-base",
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// First request blocks in exec, holding the slot.
	done := make(chan int)
	go func() {
		code, _ := doRun(t, ts.URL, "token-a", map[string]any{"command": "block"})
		done <- code
	}()
	// Wait for the first to acquire the slot.
	<-sb.started
	// Second request should be rate-limited.
	code, out := doRun(t, ts.URL, "token-a", map[string]any{"command": "echo hi"})
	if code != 429 {
		t.Fatalf("expected 429, got %d", code)
	}
	if out["error"] == "" {
		t.Fatal("expected rate-limit error")
	}
	// Release the first.
	close(block)
	if c := <-done; c != 200 {
		t.Fatalf("expected first 200, got %d", c)
	}
}

func TestRunReleasesSandbox(t *testing.T) {
	sb := newFakeSandbox()
	s := New(Config{
		Sandbox:      sb,
		Tokens:       map[string]string{"token-a": "consumer-a"},
		DefaultTTL:   300,
		MaxTTL:       3600,
		MaxTimeout:   300,
		DefaultImage: "py-base",
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	code, _ := doRun(t, ts.URL, "token-a", map[string]any{"command": "echo hi"})
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if len(sb.created) != 1 || len(sb.deleted) != 1 {
		t.Fatalf("expected 1 create + 1 delete, got %d create %d delete", len(sb.created), len(sb.deleted))
	}
	if sb.deleted[0] != sb.created[0] {
		t.Fatalf("released %s but created %s", sb.deleted[0], sb.created[0])
	}
}
