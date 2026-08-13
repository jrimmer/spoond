package spoonddoctor

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/jrimmer/spoond/capabilities"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns
// the captured output.
func captureStdout(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	code := fn()
	_ = w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()
	return code, buf.String()
}

func TestDoctorCapabilitiesJSON(t *testing.T) {
	code, out := captureStdout(t, func() int { return Main([]string{"capabilities", "--json"}) })
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var doc capabilities.Document
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("unmarshal: %v\noutput: %s", err, out)
	}
	if doc.Name != "spoond" || len(doc.Surfaces) != 10 {
		t.Fatalf("unexpected doc: name=%q surfaces=%d", doc.Name, len(doc.Surfaces))
	}
}

// TestDoctorCapabilitiesFlagFirst verifies `spoond doctor --json
// capabilities` (flag-first) routes to the subcommand rather than the
// health checks (correctness finding).
func TestDoctorCapabilitiesFlagFirst(t *testing.T) {
	code, out := captureStdout(t, func() int { return Main([]string{"--json", "capabilities"}) })
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var doc capabilities.Document
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("flag-first did not route to capabilities: %v\noutput: %s", err, out)
	}
}

func TestDoctorCapabilitiesText(t *testing.T) {
	code, out := captureStdout(t, func() int { return Main([]string{"capabilities"}) })
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"capability surface", "lease-api", "shell", "session/new", "rust-base"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q", want)
		}
	}
}
