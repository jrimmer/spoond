package spoondcapabilities

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// sample is a minimal capability document shaped like the API's.
func sample() map[string]any {
	return map[string]any{
		"version": "v3.6.1-test",
		"api": map[string]any{
			"base":    "/api",
			"primary": "/api/leases",
			"alias":   "/api/sandboxes",
			"auth":    "Bearer user token",
		},
		"endpoints": []map[string]any{
			{"method": "GET", "path": "/api/capabilities", "summary": "this document"},
			{"method": "POST", "path": "/api/admin/drain", "summary": "drain the node", "admin": true},
		},
		"images": []map[string]any{
			{"name": "py-base", "capability": "python", "description": "Python base", "vcpu": 2, "memory_mb": 1024, "disk_mb": 4096, "min_memory_mb": 512, "warm_sizes": []string{}},
		},
		"sizes": map[string]any{
			"presets": []map[string]any{
				{"name": "small", "vcpu": 1, "memory_mb": 2048},
			},
			"max_vcpu":      16,
			"max_memory_mb": 32768,
		},
		"network_policies": []map[string]any{
			{"name": "none", "detail": "nothing at all"},
			{"name": "restricted", "detail": "host services, DNS, the allowlist (default)"},
		},
		"surfaces": map[string]any{
			"mcp": map[string]any{
				"binary": "spoond-dev-mcp",
				"tools":  []string{"shell", "read_file"},
			},
		},
	}
}

// TestMainFetchesAndPrints drives Main against a test backend: the
// bearer token must ride the request, a 200 parses, and the summary
// shows the headline blocks. It also pins that a wrong token surfaces
// the backend's 401 with a non-zero exit.
func TestMainFetchesAndPrints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/capabilities" || r.Method != "GET" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok1" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"no or unknown token"}`))
			return
		}
		b, _ := json.Marshal(sample())
		_, _ = w.Write(b)
	}))
	defer srv.Close()

	body, code := runMain(t, []string{"--url", srv.URL, "--token", "tok1"}, false)
	if code != 0 {
		t.Fatalf("exit %d, output:\n%s", code, body)
	}
	for _, want := range []string{"spoond v3.6.1-test", "images:", "py-base", "network policies", "endpoints (2 routes, 1 admin-only)", "/api/capabilities", "mcp: spoond-dev-mcp (stdio), tools: shell, read_file"} {
		if !strings.Contains(body, want) {
			t.Errorf("summary lacks %q:\n%s", want, body)
		}
	}
	// --json prints the raw document.
	js, code := runMain(t, []string{"--url", srv.URL, "--token", "tok1", "--json"}, false)
	if code != 0 {
		t.Fatalf("json exit %d:\n%s", code, js)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(js), &out); err != nil {
		t.Fatalf("json is not an object: %v", err)
	}
	if out["version"] != "v3.6.1-test" {
		t.Errorf("json version = %v", out["version"])
	}
	// A rejected token exits non-zero with the backend's error.
	_, code = runMain(t, []string{"--url", srv.URL, "--token", "wrong"}, false)
	if code == 0 {
		t.Error("expected non-zero exit for a 401")
	}
}

// TestMainNeedsToken: no token anywhere is a usage error, exit 2, and
// it names the env knobs rather than hitting the network unauthenticated.
func TestMainNeedsToken(t *testing.T) {
	t.Setenv("TOKEN", "")
	t.Setenv("CONSUMER_TOKENS", "")
	body, code := runMain(t, nil, true)
	if code != 2 {
		t.Fatalf("exit %d, want 2, output:\n%s", code, body)
	}
	if !strings.Contains(body, "TOKEN") {
		t.Errorf("error should name TOKEN:\n%s", body)
	}
}

// runMain captures stdout/stderr around Main.
func runMain(t *testing.T, args []string, envOff bool) (string, int) {
	t.Helper()
	saved := osStdoutSwap(new(strings.Builder))
	code := Main(args)
	stdout := saved.restore()
	return stdout, code
}

// outSwap swaps os.Stdout's file for a pipe Main's fmt.Fprint(os.Stdout, …)
// writes into, returning the restore function that recovers the
// original and hands back what was printed.
type outSwap struct {
	old *os.File
	pr  *os.File
	pw  *os.File
}

func osStdoutSwap(buf *strings.Builder) outSwap {
	sw := outSwap{old: os.Stdout}
	savedStderr := os.Stderr
	sw.pr, sw.pw, _ = os.Pipe()
	os.Stdout = sw.pw
	os.Stderr = sw.pw // errors land in the same capture
	_ = savedStderr
	return sw
}

// restore drains the pipe into the caller's buffer and puts the real
// stdout back.
func (sw outSwap) restore() string {
	os.Stdout = sw.old
	_ = sw.pw.Close()
	b, _ := io.ReadAll(sw.pr)
	_ = sw.pr.Close()
	return string(b)
}
