// Exported test support for the renamed-variable helpers, following the
// export_test.go convention: compiled only under go test, so other
// packages' tests can drive Get and observe the deprecation warning
// without adding anything to the production package.
package env

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

// ResetDeprecationWarnings clears the process-wide once-set so a test can
// observe a deprecation warning that earlier reads already triggered.
func ResetDeprecationWarnings() {
	mu.Lock()
	warned = map[string]bool{}
	mu.Unlock()
}

// CaptureWarnings redirects the standard logger into a buffer for
// the duration of fn and returns the non-empty lines logged. Use it
// around the configuration read whose warning is under test.
func CaptureWarnings(t *testing.T, fn func()) []string {
	t.Helper()
	var buf bytes.Buffer
	flags := log.Flags()
	out := log.Writer()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(out)
		log.SetFlags(flags)
	}()
	fn()
	var lines []string
	for _, l := range strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// UnsetForTest unsets keys for the duration of the test, failing when a
// value inherited from the environment cannot be cleared.
func UnsetForTest(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		t.Setenv(k, "")
		if v := os.Getenv(k); v != "" {
			t.Fatalf("%s must be unset for this test, found %q", k, v)
		}
		os.Unsetenv(k)
	}
}
