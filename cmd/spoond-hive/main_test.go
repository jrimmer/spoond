package spoondhive

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jrimmer/spoond/v2/hive"
)

// withEnv sets environment variables for one test and restores them.
func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range []string{"SPOOND_API", "SPOOND_TOKEN"} {
		old, had := os.LookupEnv(k)
		os.Unsetenv(k)
		t.Cleanup(func() {
			if had {
				os.Setenv(k, old)
			} else {
				os.Unsetenv(k)
			}
		})
	}
	for k, v := range kv {
		os.Setenv(k, v)
	}
}

// writeHiveYAML writes a hive.yaml and returns its path.
func writeHiveYAML(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hive.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// catalog is an httptest lease API serving an image catalog.
func catalog(t *testing.T, images ...string) *httptest.Server {
	t.Helper()
	body, err := json.Marshal(map[string]any{"images": images})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/images" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// capture runs fn with stdout and stderr captured.
func capture(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = wOut, wErr
	outCh := make(chan string, 1)
	go func() {
		var b bytes.Buffer
		_, _ = b.ReadFrom(rOut)
		outCh <- b.String()
	}()
	errCh := make(chan string, 1)
	go func() {
		var b bytes.Buffer
		_, _ = b.ReadFrom(rErr)
		errCh <- b.String()
	}()
	fn()
	wOut.Close()
	wErr.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	return <-outCh, <-errCh
}

// validYAML is the C10 example, with an image the test catalog has.
const validYAML = `project: hrmny
repo: ssh://git@git.lacy.casa/lacy.casa/hrmny.git
base_image: elixir-release
gates:
  - mix test
needs: []
`

func TestCheckExitZeroWhenNothingFailed(t *testing.T) {
	srv := catalog(t, "elixir-release", "go-base")
	withEnv(t, map[string]string{"SPOOND_API": srv.URL, "SPOOND_TOKEN": "token-a"})
	file := writeHiveYAML(t, validYAML)
	code, out, errOut := run(t, "check", file)
	if code != 0 {
		t.Fatalf("exit %d, want 0 (stdout:\n%s\nstderr:\n%s)", code, out, errOut)
	}
	want := `✓ hive.yaml
✓ base image
- worker image: skipped (runs on the host: POST /hive/check)
- deploy key: skipped (runs on the host: POST /hive/check)
- trial lease: skipped (runs on the host: POST /hive/check)
- gates: skipped (runs on the host: POST /hive/check)
- budget: skipped (runs on the host: POST /hive/check)
Next: enlist: POST /hive/projects
`
	if out != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", out, want)
	}
}

func TestCheckExitOneWhenACheckFailed(t *testing.T) {
	srv := catalog(t, "go-base") // no elixir-release
	withEnv(t, map[string]string{"SPOOND_API": srv.URL, "SPOOND_TOKEN": "token-a"})
	file := writeHiveYAML(t, validYAML)
	code, out, _ := run(t, "check", file)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(out, "✗ base image:") {
		t.Errorf("stdout does not report the failure:\n%s", out)
	}
	if !strings.HasPrefix(hiveLastLine(out), "Next: set base_image to an image in the catalog") {
		t.Errorf("last line %q", hiveLastLine(out))
	}
}

func TestCheckExitOneWhenSchemaFails(t *testing.T) {
	srv := catalog(t, "elixir-release")
	withEnv(t, map[string]string{"SPOOND_API": srv.URL, "SPOOND_TOKEN": "token-a"})
	file := writeHiveYAML(t, strings.Replace(validYAML, "base_image: elixir-release", "base_image: \"\"", 1))
	code, out, _ := run(t, "check", file)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(out, "✗ hive.yaml:") {
		t.Errorf("stdout:\n%s", out)
	}
}

func TestCheckExitOneWhenFileIsUnparseable(t *testing.T) {
	srv := catalog(t)
	withEnv(t, map[string]string{"SPOOND_API": srv.URL, "SPOOND_TOKEN": "token-a"})
	file := writeHiveYAML(t, "project: hrmny\nunknown: yes\n")
	code, _, errOut := run(t, "check", file)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errOut, "field unknown not found") {
		t.Errorf("stderr %q does not name the unknown field", errOut)
	}
}

func TestCheckJSON(t *testing.T) {
	srv := catalog(t, "elixir-release")
	withEnv(t, map[string]string{"SPOOND_API": srv.URL, "SPOOND_TOKEN": "token-a"})
	file := writeHiveYAML(t, validYAML)
	code, out, _ := run(t, "check", "--json", file)
	if code != 0 {
		t.Fatalf("exit %d, want 0 (stdout:\n%s)", code, out)
	}
	var got struct {
		Checks []struct {
			Name   string      `json:"name"`
			Status hive.Status `json:"status"`
			Detail string      `json:"detail"`
			Remedy string      `json:"remedy"`
		} `json:"checks"`
		Next string `json:"next"`
		OK   bool   `json:"ok"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("parse JSON output %q: %v", out, err)
	}
	if len(got.Checks) != len(hive.CheckNames) {
		t.Fatalf("%d checks, want %d", len(got.Checks), len(hive.CheckNames))
	}
	for i, c := range got.Checks {
		if c.Name != hive.CheckNames[i] {
			t.Errorf("check %d is %q, want %q", i, c.Name, hive.CheckNames[i])
		}
	}
	if !got.OK {
		t.Error("ok is false")
	}
	if got.Next != "enlist: POST /hive/projects" {
		t.Errorf("next %q", got.Next)
	}
	if got.Checks[1].Status != hive.Pass {
		t.Errorf("base image status %s", got.Checks[1].Status)
	}
}

func TestCheckJSONFailureExit(t *testing.T) {
	srv := catalog(t) // empty catalog
	withEnv(t, map[string]string{"SPOOND_API": srv.URL, "SPOOND_TOKEN": "token-a"})
	file := writeHiveYAML(t, validYAML)
	code, out, _ := run(t, "check", "--json", file)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(out, `"next"`) {
		t.Errorf("JSON output has no next:\n%s", out)
	}
	if !strings.Contains(out, `"status": "fail"`) {
		t.Errorf("JSON output has no failure:\n%s", out)
	}
}

func TestCheckFlagAfterFile(t *testing.T) {
	srv := catalog(t, "elixir-release")
	withEnv(t, map[string]string{"SPOOND_API": srv.URL, "SPOOND_TOKEN": "token-a"})
	file := writeHiveYAML(t, validYAML)
	code, out, _ := run(t, "check", file, "--json")
	if code != 0 {
		t.Fatalf("exit %d, want 0 (stdout:\n%s)", code, out)
	}
	if !strings.Contains(out, `"next": "enlist: POST /hive/projects"`) {
		t.Errorf("stdout has no next:\n%s", out)
	}
}

func TestCheckUsage(t *testing.T) {
	for _, args := range [][]string{{"check"}, {"check", "a", "b"}} {
		code, _, errOut := run(t, args...)
		if code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
		if !strings.Contains(errOut, "usage:") {
			t.Errorf("%v: stderr %q", args, errOut)
		}
	}
}

func TestNoCommandIsUsage(t *testing.T) {
	code, _, errOut := run(t)
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(errOut, "usage:") {
		t.Errorf("stderr %q", errOut)
	}
	if !strings.Contains(errOut, "SPOOND_API") {
		t.Errorf("usage does not mention SPOOND_API: %q", errOut)
	}
}

func TestUnknownCommand(t *testing.T) {
	code, _, errOut := run(t, "frobnicate")
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(errOut, "unknown command") {
		t.Errorf("stderr %q", errOut)
	}
}

func TestHelp(t *testing.T) {
	code, _, errOut := run(t, "help")
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.Contains(errOut, "check <file>") {
		t.Errorf("stderr %q", errOut)
	}
}

func TestDefaultAPI(t *testing.T) {
	if hive.DefaultAPI != "https://vm2.lacy.casa:8890" {
		t.Errorf("DefaultAPI %q", hive.DefaultAPI)
	}
}

// run runs Main with the given arguments, capturing its output.
func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var code int
	out, errOut := capture(t, func() { code = Main(args) })
	return code, out, errOut
}

func hiveLastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}
