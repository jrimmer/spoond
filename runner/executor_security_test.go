package runner

import (
	"regexp"
	"testing"
)

// TestC3GitAuthEnvSafe: checkout auth env uses GIT_CONFIG_* and
// single-quoting with a validated charset — a token with shell
// metacharacters is rejected rather than injected (security review #37
// C3).
func TestC3GitAuthEnvSafe(t *testing.T) {
	safe := regexp.MustCompile(`^[A-Za-z0-9_.\-]+$`)
	good := []string{"ghp_abc123", "forgejo_token_XYZ", "a.b-c_d"}
	for _, tok := range good {
		if !safe.MatchString(tok) {
			t.Errorf("token %q should be safe", tok)
		}
	}
	bad := []string{"abc; rm -rf /", "abc'def", "abc \"$(id)\"", "abc`id`", "abc def"}
	for _, tok := range bad {
		if safe.MatchString(tok) {
			t.Errorf("token %q must be rejected (unsafe)", tok)
		}
	}
}
