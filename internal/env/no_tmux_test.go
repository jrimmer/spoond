package env

import "testing"

// TestNoTmuxNames pins the three-way contract for the guest script's
// tmux opt-out: the 2.0 name wins, the deprecated FORKD_ name still
// works, and the deprecated name warns exactly once. The guest image
// reads these names in images/guest/spoond-tmux.sh, not through a Go
// helper; this test exercises env.Get directly so the contract stays
// covered even though the shell script itself is only run by the
// integration suite.
func TestNoTmuxNames(t *testing.T) {
	t.Setenv("SPOOND_NO_TMUX", "1")
	t.Setenv("FORKD_NO_TMUX", "1")
	if got := Get("SPOOND_NO_TMUX", ""); got != "1" {
		t.Fatal("SPOOND_NO_TMUX must win")
	}
	ResetDeprecationWarnings()
	warnings := CaptureWarnings(t, func() {
		t.Setenv("SPOOND_NO_TMUX", "")
		if got := Get("SPOOND_NO_TMUX", ""); got != "1" {
			t.Fatal("FORKD_NO_TMUX fallback must still work")
		}
	})
	if len(warnings) != 1 {
		t.Fatalf("want exactly one deprecation warning, got %q", warnings)
	}
	ResetDeprecationWarnings()
	warnings = CaptureWarnings(t, func() {
		UnsetForTest(t, "SPOOND_NO_TMUX", "FORKD_NO_TMUX")
		if Get("SPOOND_NO_TMUX", "") != "" {
			t.Fatal("unset must not opt out of tmux")
		}
	})
	if len(warnings) != 0 {
		t.Fatalf("no warning expected without the deprecated name, got %q", warnings)
	}
}
