package spoondbackend

import (
	"testing"
	"time"
)

// TestEnvDurationOrZero pins spoond-52c NIT: DRAIN_RESUME_MAX_AGE reads a
// zero or negative Go duration from the environment (0 means the default,
// a negative disables the bound), unlike envDurationOr, which ignores
// non-positive values.
func TestEnvDurationOrZero(t *testing.T) {
	const key = "DRAIN_RESUME_MAX_AGE_TEST"

	t.Setenv(key, "0s")
	if got := envDurationOrZero(key, 24*time.Hour); got != 0 {
		t.Fatalf("envDurationOrZero(0s) = %s, want 0", got)
	}
	t.Setenv(key, "-5m")
	if got := envDurationOrZero(key, 24*time.Hour); got != -5*time.Minute {
		t.Fatalf("envDurationOrZero(-5m) = %s, want -5m", got)
	}
	t.Setenv(key, "9h")
	if got := envDurationOrZero(key, 24*time.Hour); got != 9*time.Hour {
		t.Fatalf("envDurationOrZero(9h) = %s, want 9h", got)
	}
	// A missing or malformed value falls back to the default.
	t.Setenv(key, "not-a-duration")
	if got := envDurationOrZero(key, 24*time.Hour); got != 24*time.Hour {
		t.Fatalf("envDurationOrZero(malformed) = %s, want the default", got)
	}
}
