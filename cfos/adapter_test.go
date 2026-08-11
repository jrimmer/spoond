package cfos

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jrimmer/spoond/runner"
)

// fakeSandbox is an in-memory SandboxProvider for tests.
type fakeSandbox struct {
	created []string
	execs   []string
	deleted []string
	err     error
}

func (f *fakeSandbox) Create(_ context.Context, image string, ttl int) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	id := "sb-fake-" + image
	f.created = append(f.created, image+":"+strconv.Itoa(ttl))
	return id, nil
}

func (f *fakeSandbox) Exec(_ context.Context, id, cmd, cwd string, env map[string]string, timeout int) (*runner.ExecResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.execs = append(f.execs, cmd)
	return &runner.ExecResult{Stdout: "OK\n", Exit: 0}, nil
}

func (f *fakeSandbox) Delete(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func newFake() (*fakeSandbox, *Server) {
	f := &fakeSandbox{}
	s := New(Config{
		Sandbox:      f,
		Tokens:       map[string]string{"test-token": "c1"},
		DefaultImage: "js-base",
	})
	return f, s
}

func TestExecuteHappyPath(t *testing.T) {
	f, s := newFake()
	body, _ := json.Marshal(map[string]any{"code": "console.log('hi')", "language": "js"})
	req := httptest.NewRequest("POST", "/v1/execute", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var resp executeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Stdout != "OK\n" || resp.Exit != 0 {
		t.Fatalf("unexpected resp: %+v", resp)
	}
	if len(f.created) != 1 || !strings.HasPrefix(f.created[0], "js-base") {
		t.Fatalf("should create js-base sandbox: %v", f.created)
	}
	if len(f.deleted) != 1 {
		t.Fatalf("should delete sandbox after run: %v", f.deleted)
	}
	if len(f.execs) != 1 || !strings.Contains(f.execs[0], "node /tmp/main.mjs") {
		t.Fatalf("should run node on main.mjs: %q", f.execs)
	}
}

func TestExecuteMissingCode(t *testing.T) {
	_, s := newFake()
	req := httptest.NewRequest("POST", "/v1/execute", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d want 400", rr.Code)
	}
}

func TestExecuteUnauthorized(t *testing.T) {
	_, s := newFake()
	req := httptest.NewRequest("POST", "/v1/execute", strings.NewReader(`{"code":"x"}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status %d want 401", rr.Code)
	}
}

func TestExecuteUnknownLanguage(t *testing.T) {
	_, s := newFake()
	body, _ := json.Marshal(map[string]any{"code": "x", "language": "cobol"})
	req := httptest.NewRequest("POST", "/v1/execute", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d want 400", rr.Code)
	}
}

func TestExecuteSandboxFailure(t *testing.T) {
	f, s := newFake()
	f.err = &fakeErr{"boom"}
	body, _ := json.Marshal(map[string]any{"code": "x"})
	req := httptest.NewRequest("POST", "/v1/execute", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status %d want 502", rr.Code)
	}
}

type fakeErr struct{ msg string }

func (e *fakeErr) Error() string { return e.msg }

func TestBindingsBecomeEnv(t *testing.T) {
	f, s := newFake()
	body, _ := json.Marshal(map[string]any{
		"code":     "console.log(process.env.FOO)",
		"language": "js",
		"bindings": map[string]string{"FOO": "bar"},
	})
	req := httptest.NewRequest("POST", "/v1/execute", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if len(f.execs) != 1 || !strings.Contains(f.execs[0], "export FOO='\"bar\"'") {
		t.Fatalf("bindings not exported: %q", f.execs)
	}
}

func TestImageFor(t *testing.T) {
	tests := []struct {
		lang string
		want string
		err  bool
	}{
		{"", "js-base", false},
		{"js", "js-base", false},
		{"typescript", "js-base", false},
		{"go", "go-base", false},
		{"python", "py-base", false},
		{"elixir", "elixir-base", false},
		{"cobol", "", true},
	}
	for _, tt := range tests {
		got, err := ImageFor(tt.lang, "js-base")
		if (err != nil) != tt.err {
			t.Fatalf("%q: err %v want %v", tt.lang, err, tt.err)
		}
		if err == nil && got != tt.want {
			t.Fatalf("%q: image %q want %q", tt.lang, got, tt.want)
		}
	}
}
