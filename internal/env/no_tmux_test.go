package env

import "testing"

// NoTmux proves the three-way contract for the guest script's tmux
// opt-out: the 2.0 name wins, the deprecated FORKD_ name still works,
// and the deprecated name warns exactly once. env.NoTmux mirrors the
// precedence in images/guest/spoond-tmux.sh; the script itself is
// exercised by the integration suite.
func TestNoTmuxNames(t *testing.T) {
	t.Setenv("SPOOND_NO_TMUX", "1")
	t.Setenv("FORKD_NO_TMUX", "1")
	if !NoTmux() {
		t.Fatal("SPOOND_NO_TMUX must win")
	}
	warnings := CaptureWarnings(t, func() {
		t.Setenv("SPOOND_NO_TMUX", "")
		if !NoTmux() {
			t.Fatal("FORKD_NO_TMUX fallback must still work")
		}
	})
	if len(warnings) != 1 {
		t.Fatalf("want exactly one deprecation warning, got %q", warnings)
	}
	ResetDeprecationWarnings()
	warnings = CaptureWarnings(t, func() {
		UnsetForTest(t, "SPOOND_NO_TMUX", "FORKD_NO_TMUX")
		if NoTmux() {
			t.Fatal("unset must not opt out of tmux")
		}
	})
	if len(warnings) != 0 {
		t.Fatalf("no warning expected without the deprecated name, got %q", warnings)
	}
}
