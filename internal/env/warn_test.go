package env

import (
	"strings"
	"testing"
)

func TestWarningLoggedOncePerVariable(t *testing.T) {
	ResetDeprecationWarnings()
	lines := CaptureWarnings(t, func() {
		t.Setenv("FORKD_BACKEND_URL", "https://old:8890")
		if Get("SPOOND_BACKEND_URL", "d") != "https://old:8890" {
			t.Fatal("fallback must still work")
		}
		if Get("SPOOND_BACKEND_URL", "d") != "https://old:8890" {
			t.Fatal("fallback must still work")
		}
		// A different deprecated variable warns separately, exactly once.
		t.Setenv("FORKD_CTL_PORT", "2201")
		if Get("SPOOND_CTL_PORT", "d") != "2201" {
			t.Fatal("fallback must still work")
		}
	})
	joined := strings.Join(lines, "\n")
	if n := strings.Count(joined, "FORKD_BACKEND_URL is deprecated"); n != 1 {
		t.Fatalf("warning for FORKD_BACKEND_URL logged %d times, want exactly 1: %q", n, lines)
	}
	if n := strings.Count(joined, "FORKD_CTL_PORT is deprecated"); n != 1 {
		t.Fatalf("warning for FORKD_CTL_PORT logged %d times, want exactly 1: %q", n, lines)
	}
}

func TestWarningMentionsNewName(t *testing.T) {
	ResetDeprecationWarnings()
	lines := CaptureWarnings(t, func() {
		t.Setenv("FORKD_GATEWAY_HOST", "old.example.com")
		if Get("SPOOND_GATEWAY_HOST", "d") != "old.example.com" {
			t.Fatal("fallback must still work")
		}
	})
	if len(lines) != 1 {
		t.Fatalf("want exactly one warning line, got %q", lines)
	}
	if !strings.Contains(lines[0], "SPOOND_GATEWAY_HOST") {
		t.Fatalf("warning must name the 2.0 variable: %q", lines[0])
	}
}

func TestNoWarningWhenNewNameSet(t *testing.T) {
	ResetDeprecationWarnings()
	lines := CaptureWarnings(t, func() {
		t.Setenv("SPOOND_AGENT_TOKEN", "new-token")
		t.Setenv("FORKD_AGENT_TOKEN", "old-token")
		if Get("SPOOND_AGENT_TOKEN", "") != "new-token" {
			t.Fatal("2.0 name must win")
		}
	})
	if len(lines) != 0 {
		t.Fatalf("no warning expected when the 2.0 name supplies the value, got %q", lines)
	}
}

func TestNoWarningWhenNothingSet(t *testing.T) {
	ResetDeprecationWarnings()
	lines := CaptureWarnings(t, func() {
		UnsetForTest(t, "SPOOND_IMAGE", "FORKD_IMAGE")
		if Get("SPOOND_IMAGE", "dev-base") != "dev-base" {
			t.Fatal("default must be returned")
		}
	})
	if len(lines) != 0 {
		t.Fatalf("no warning expected without the deprecated name, got %q", lines)
	}
}
