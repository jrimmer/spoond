package spoondctl

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jrimmer/spoond/v2/internal/env"
)

// Each renamed variable spoondctl reads proves the three-way contract:
// the 2.0 name wins, the deprecated name still works, and the deprecated
// name warns exactly once. Each test resolves through env.Get with the
// same names and defaults Main uses.

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

// unsetCtl unsets the ctl variables for the duration of the test.
// t.Setenv registers the restore, so the ambient values (if any) come
// back after the test.
func unsetCtl(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func TestCtlHostNames(t *testing.T) {
	unsetCtl(t, "SPOOND_CTL_HOST", "FORKD_CTL_HOST")
	t.Setenv("SPOOND_CTL_HOST", "new.example.com")
	t.Setenv("FORKD_CTL_HOST", "old.example.com")
	if got := env.Get("SPOOND_CTL_HOST", "sandbox.lacy.casa"); got != "new.example.com" {
		t.Fatalf("SPOOND_CTL_HOST must win, got %q", got)
	}
	unsetCtl(t, "SPOOND_CTL_HOST")
	warnings := captureWarnings(t, func() {
		if got := env.Get("SPOOND_CTL_HOST", "sandbox.lacy.casa"); got != "old.example.com" {
			t.Fatalf("FORKD_CTL_HOST fallback must still work, got %q", got)
		}
	})
	if len(warnings) != 1 {
		t.Fatalf("want exactly one deprecation warning, got %q", warnings)
	}
}

func TestCtlPortNames(t *testing.T) {
	unsetCtl(t, "SPOOND_CTL_PORT", "FORKD_CTL_PORT")
	t.Setenv("SPOOND_CTL_PORT", "2200")
	t.Setenv("FORKD_CTL_PORT", "2201")
	if got := env.Get("SPOOND_CTL_PORT", "2222"); got != "2200" {
		t.Fatalf("SPOOND_CTL_PORT must win, got %q", got)
	}
	unsetCtl(t, "SPOOND_CTL_PORT")
	warnings := captureWarnings(t, func() {
		if got := env.Get("SPOOND_CTL_PORT", "2222"); got != "2201" {
			t.Fatalf("FORKD_CTL_PORT fallback must still work, got %q", got)
		}
	})
	if len(warnings) != 1 {
		t.Fatalf("want exactly one deprecation warning, got %q", warnings)
	}
}

func TestCtlKeyNames(t *testing.T) {
	unsetCtl(t, "SPOOND_CTL_KEY", "FORKD_CTL_KEY")
	t.Setenv("SPOOND_CTL_KEY", "/new/key")
	t.Setenv("FORKD_CTL_KEY", "/old/key")
	if got := env.Get("SPOOND_CTL_KEY", filepath.Join(homeDir(), ".ssh", "id_ed25519")); got != "/new/key" {
		t.Fatalf("SPOOND_CTL_KEY must win, got %q", got)
	}
	unsetCtl(t, "SPOOND_CTL_KEY")
	warnings := captureWarnings(t, func() {
		if got := env.Get("SPOOND_CTL_KEY", filepath.Join(homeDir(), ".ssh", "id_ed25519")); got != "/old/key" {
			t.Fatalf("FORKD_CTL_KEY fallback must still work, got %q", got)
		}
	})
	if len(warnings) != 1 {
		t.Fatalf("want exactly one deprecation warning, got %q", warnings)
	}
}

func TestCtlDefaults(t *testing.T) {
	unsetCtl(t, "SPOOND_CTL_HOST", "SPOOND_CTL_PORT", "SPOOND_CTL_KEY",
		"FORKD_CTL_HOST", "FORKD_CTL_PORT", "FORKD_CTL_KEY")
	if got := env.Get("SPOOND_CTL_HOST", "sandbox.lacy.casa"); got != "sandbox.lacy.casa" {
		t.Fatalf("default host, got %q", got)
	}
	if got := env.Get("SPOOND_CTL_PORT", "2222"); got != "2222" {
		t.Fatalf("default port, got %q", got)
	}
	if got := env.Get("SPOOND_CTL_KEY", filepath.Join(homeDir(), ".ssh", "id_ed25519")); got != filepath.Join(homeDir(), ".ssh", "id_ed25519") {
		t.Fatalf("default key, got %q", got)
	}
}
