package spoondrain

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// recordingServer is an httptest server that records the requests it
// received and answers with a fixed body and status.
type recordingServer struct {
	*httptest.Server

	mu       sync.Mutex
	paths    []string
	tokens   []string
	body     string
	status   int
	insecure bool
}

func newRecordingServer(t *testing.T, status int, body string) *recordingServer {
	t.Helper()
	rs := &recordingServer{body: body, status: status}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		defer rs.mu.Unlock()
		rs.paths = append(rs.paths, r.URL.Path)
		rs.tokens = append(rs.tokens, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rs.status)
		_, _ = io.WriteString(w, rs.body)
	}))
	t.Cleanup(rs.Server.Close)
	return rs
}

func (rs *recordingServer) calls() (int, string, string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.paths) == 0 {
		return 0, "", ""
	}
	tok := ""
	if len(rs.tokens) > 0 {
		tok = rs.tokens[len(rs.tokens)-1]
	}
	return len(rs.paths), rs.paths[len(rs.paths)-1], tok
}

// writeTokenFile writes a token file the way openssl does: one line
// with a trailing newline.
func writeTokenFile(t *testing.T, token string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "admin-token")
	if err := os.WriteFile(p, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// runMain runs Main with os.Stdout captured and returns its exit code
// and what it printed.
func runMain(t *testing.T, args ...string) (int, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	code := Main(args)
	w.Close()
	os.Stdout = old
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return code, string(out)
}

func setEnv(t *testing.T, url, tokenFile string) {
	t.Helper()
	t.Setenv("SPOOND_DRAIN_URL", url)
	t.Setenv("SPOOND_ADMIN_TOKEN_FILE", tokenFile)
	t.Setenv("SPOOND_DRAIN_INSECURE", "")
}

// TestStopSkipsOnFailedServiceResult: systemd sets SERVICE_RESULT for
// ExecStop; a drain must not run after a crash or kill.
func TestStopSkipsOnFailedServiceResult(t *testing.T) {
	rs := newRecordingServer(t, http.StatusOK, `{"paused":0}`)
	setEnv(t, rs.URL, writeTokenFile(t, "tok"))
	t.Setenv("SERVICE_RESULT", "failed")

	code, out := runMain(t, "--stop")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if out != "drain: skipped (SERVICE_RESULT=failed)\n" {
		t.Fatalf("output = %q, want the skip line", out)
	}
	if n, _, _ := rs.calls(); n != 0 {
		t.Fatalf("server called %d times, want 0", n)
	}
}

// TestStopRunsOnSuccessResult: SERVICE_RESULT=success (or unset) drains.
func TestStopRunsOnSuccessResult(t *testing.T) {
	rs := newRecordingServer(t, http.StatusOK, `{"paused":1,"failed":[],"pool_deleted":0,"quiesced":true}`)
	setEnv(t, rs.URL, writeTokenFile(t, "tok"))
	t.Setenv("SERVICE_RESULT", "success")

	code, out := runMain(t, "--stop")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if n, path, auth := rs.calls(); n != 1 || path != "/api/admin/drain" || auth != "Bearer tok" {
		t.Fatalf("calls = %d path = %q auth = %q, want 1 drain call with the token", n, path, auth)
	}
	if !strings.Contains(out, `"quiesced":true`) {
		t.Fatalf("output = %q, want the response JSON", out)
	}
}

// TestStartPostsUndrain: --start calls undrain with the longer timeout
// path, regardless of SERVICE_RESULT.
func TestStartPostsUndrain(t *testing.T) {
	rs := newRecordingServer(t, http.StatusOK, `{"resumed":1,"failed":[]}`)
	setEnv(t, rs.URL, writeTokenFile(t, "sekrit"))
	t.Setenv("SERVICE_RESULT", "failed") // only --stop consults it

	code, _ := runMain(t, "--start")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if n, path, auth := rs.calls(); n != 1 || path != "/api/admin/undrain" || auth != "Bearer sekrit" {
		t.Fatalf("calls = %d path = %q auth = %q, want 1 undrain call with the token", n, path, auth)
	}
}

// TestMismatchedListsExitWithoutCalling: lists of different lengths log
// the error and exit 0 without calling anything.
func TestMismatchedListsExitWithoutCalling(t *testing.T) {
	rs := newRecordingServer(t, http.StatusOK, `{}`)
	setEnv(t, rs.URL+","+rs.URL, writeTokenFile(t, "tok")) // two URLs, one token file

	code, _ := runMain(t, "--stop")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if n, _, _ := rs.calls(); n != 0 {
		t.Fatalf("server called %d times, want 0", n)
	}
}

// TestConnectionFailureExitsZero: a failed drain must never block the
// stop.
func TestConnectionFailureExitsZero(t *testing.T) {
	setEnv(t, "http://127.0.0.1:1", writeTokenFile(t, "tok"))

	code, _ := runMain(t, "--stop")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 on connection failure", code)
	}
}

// TestHTTPErrorExitsZero: an HTTP error is logged, not fatal.
func TestHTTPErrorExitsZero(t *testing.T) {
	rs := newRecordingServer(t, http.StatusServiceUnavailable, `{"error":"orchestrator unreachable: x"}`)
	setEnv(t, rs.URL, writeTokenFile(t, "tok"))

	code, _ := runMain(t, "--start")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 on HTTP error", code)
	}
}

// TestPairsRunInOrderAndContinue: each pair is called in list order and
// a failure on one pair does not stop the next.
func TestPairsRunInOrderAndContinue(t *testing.T) {
	bad := newRecordingServer(t, http.StatusInternalServerError, `{}`)
	good := newRecordingServer(t, http.StatusOK, `{"resumed":2,"failed":[]}`)
	tok := writeTokenFile(t, "tok")
	// Comma lists: the bad pair first, the good pair second.
	t.Setenv("SPOOND_DRAIN_URL", bad.URL+","+good.URL)
	t.Setenv("SPOOND_ADMIN_TOKEN_FILE", tok+","+tok)

	code, out := runMain(t, "--start")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if n, _, _ := bad.calls(); n != 1 {
		t.Fatalf("first pair called %d times, want 1", n)
	}
	if n, _, _ := good.calls(); n != 1 {
		t.Fatalf("second pair called %d times, want 1", n)
	}
	if !strings.Contains(out, `"resumed":2`) {
		t.Fatalf("output = %q, want the second pair's JSON", out)
	}
}

// TestInsecureSkipsTLSVerification: SPOOND_DRAIN_INSECURE=1 lets the
// call reach a server whose certificate does not match 127.0.0.1.
func TestInsecureSkipsTLSVerification(t *testing.T) {
	rs := &recordingServer{body: `{"paused":0}`, status: http.StatusOK}
	rs.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		defer rs.mu.Unlock()
		rs.paths = append(rs.paths, r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, rs.body)
	}))
	t.Cleanup(rs.Server.Close)
	t.Setenv("SPOOND_DRAIN_URL", rs.URL)
	t.Setenv("SPOOND_ADMIN_TOKEN_FILE", writeTokenFile(t, "tok"))
	t.Setenv("SPOOND_DRAIN_INSECURE", "1")

	code, _ := runMain(t, "--stop")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if n, _, _ := rs.calls(); n != 1 {
		t.Fatalf("server called %d times, want 1", n)
	}

	// Without the flag the TLS verification fails (still exit 0).
	t.Setenv("SPOOND_DRAIN_INSECURE", "")
	code, _ = runMain(t, "--stop")
	if code != 0 {
		t.Fatalf("exit with verification on = %d, want 0", code)
	}
}

// TestUnknownArgAndMissingArg: bad invocations print usage and exit 2.
func TestUnknownArgAndMissingArg(t *testing.T) {
	if code := Main([]string{"--_sideways"}); code != 2 {
		t.Fatalf("unknown arg exit = %d, want 2", code)
	}
	if code := Main(nil); code != 2 {
		t.Fatalf("missing arg exit = %d, want 2", code)
	}
}
