package spoondgateway

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/jrimmer/spoond/v2/internal/env"
)

// gatewayHost resolves through the renamed-variable helper: the 2.0
// SPOOND_GATEWAY_HOST name wins, the deprecated FORKD_GATEWAY_HOST name
// still works, and the deprecated name warns exactly once. The warning
// is observed through the standard logger, which is what env.Get uses.
func TestGatewayHostNames(t *testing.T) {
	// Isolate from ambient values and from warnings logged by other
	// tests in this binary.
	unsetenv := func(key string) {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	unsetenv("SPOOND_GATEWAY_HOST")
	unsetenv("FORKD_GATEWAY_HOST")

	t.Setenv("SPOOND_GATEWAY_HOST", "new.example.com")
	t.Setenv("FORKD_GATEWAY_HOST", "old.example.com")
	if got := env.Get("SPOOND_GATEWAY_HOST", "sandbox.example.com"); got != "new.example.com" {
		t.Fatalf("SPOOND_GATEWAY_HOST must win, got %q", got)
	}

	unsetenv("SPOOND_GATEWAY_HOST")
	var buf bytes.Buffer
	flags := log.Flags()
	out := log.Writer()
	log.SetOutput(&buf)
	log.SetFlags(0)
	func() {
		defer func() {
			log.SetOutput(out)
			log.SetFlags(flags)
		}()
		if got := env.Get("SPOOND_GATEWAY_HOST", "sandbox.example.com"); got != "old.example.com" {
			t.Errorf("FORKD_GATEWAY_HOST fallback must still work, got %q", got)
		}
	}()
	if got := buf.String(); strings.Count(got, "FORKD_GATEWAY_HOST is deprecated") != 1 || strings.Count(got, "\n") != 1 {
		t.Fatalf("want exactly one deprecation warning line, got %q", got)
	}
}
