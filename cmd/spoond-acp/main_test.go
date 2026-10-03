package spoondacp

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/jrimmer/spoond/v2/internal/env"
)

// Each renamed variable this command reads proves the three-way
// contract: the 2.0 name wins, the deprecated name still works, and the
// deprecated name warns exactly once. Each read goes through env.Get —
// the same helper the command resolves through.

// captureWarnings redirects the standard logger into a buffer for the
// duration of fn and returns the non-empty lines logged.
func captureWarnings(t *testing.T, fn func()) []string {
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

func TestBackendURLNames(t *testing.T) {
	t.Setenv("SPOOND_BACKEND_URL", "https://new:8890")
	t.Setenv("FORKD_BACKEND_URL", "https://old:8890")
	if got := env.Get("SPOOND_BACKEND_URL", ""); got != "https://new:8890" {
		t.Fatalf("SPOOND_BACKEND_URL must win, got %q", got)
	}
	t.Setenv("SPOOND_BACKEND_URL", "")
	os.Unsetenv("SPOOND_BACKEND_URL")
	warnings := captureWarnings(t, func() {
		if got := env.Get("SPOOND_BACKEND_URL", ""); got != "https://old:8890" {
			t.Fatalf("FORKD_BACKEND_URL fallback must still work, got %q", got)
		}
	})
	if len(warnings) != 1 {
		t.Fatalf("want exactly one deprecation warning, got %q", warnings)
	}
}

func TestAgentTokenNames(t *testing.T) {
	t.Setenv("SPOOND_AGENT_TOKEN", "new-token")
	t.Setenv("FORKD_AGENT_TOKEN", "old-token")
	if got := env.Get("SPOOND_AGENT_TOKEN", ""); got != "new-token" {
		t.Fatalf("SPOOND_AGENT_TOKEN must win, got %q", got)
	}
	t.Setenv("SPOOND_AGENT_TOKEN", "")
	os.Unsetenv("SPOOND_AGENT_TOKEN")
	warnings := captureWarnings(t, func() {
		if got := env.Get("SPOOND_AGENT_TOKEN", ""); got != "old-token" {
			t.Fatalf("FORKD_AGENT_TOKEN fallback must still work, got %q", got)
		}
	})
	if len(warnings) != 1 {
		t.Fatalf("want exactly one deprecation warning, got %q", warnings)
	}
}

func TestImageNames(t *testing.T) {
	t.Setenv("SPOOND_IMAGE", "go-base")
	t.Setenv("FORKD_IMAGE", "dev-base")
	if got := env.Get("SPOOND_IMAGE", ""); got != "go-base" {
		t.Fatalf("SPOOND_IMAGE must win, got %q", got)
	}
	t.Setenv("SPOOND_IMAGE", "")
	os.Unsetenv("SPOOND_IMAGE")
	warnings := captureWarnings(t, func() {
		if got := env.Get("SPOOND_IMAGE", ""); got != "dev-base" {
			t.Fatalf("FORKD_IMAGE fallback must still work, got %q", got)
		}
	})
	if len(warnings) != 1 {
		t.Fatalf("want exactly one deprecation warning, got %q", warnings)
	}
}

func TestLLMModelNames(t *testing.T) {
	t.Setenv("SPOOND_LLM_MODEL", "m2")
	t.Setenv("FORKD_LLM_MODEL", "m1")
	if got := env.Get("SPOOND_LLM_MODEL", ""); got != "m2" {
		t.Fatalf("SPOOND_LLM_MODEL must win, got %q", got)
	}
	t.Setenv("SPOOND_LLM_MODEL", "")
	os.Unsetenv("SPOOND_LLM_MODEL")
	warnings := captureWarnings(t, func() {
		if got := env.Get("SPOOND_LLM_MODEL", ""); got != "m1" {
			t.Fatalf("FORKD_LLM_MODEL fallback must still work, got %q", got)
		}
	})
	if len(warnings) != 1 {
		t.Fatalf("want exactly one deprecation warning, got %q", warnings)
	}
}
