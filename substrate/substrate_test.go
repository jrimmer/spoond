package substrate

import (
	"testing"
	"time"
)

// TestFreeHugepageBytesSaturates: free hugepage memory is total minus
// used and reserved, and 0 (never a wrapped uint64) when a reading
// reports more taken than total.
func TestFreeHugepageBytesSaturates(t *testing.T) {
	const page = 2 << 20
	for _, tc := range []struct {
		name                  string
		total, used, reserved uint64
		want                  uint64
	}{
		{"free", 1024, 256, 128, 640 * page},
		{"exactly full", 1024, 896, 128, 0},
		{"over-reported", 1024, 1536, 0, 0},
		{"reserved over", 1024, 512, 1024, 0},
		{"empty pool", 0, 0, 0, 0},
	} {
		n := NodeInfo{HugepagesTotal: tc.total, HugepagesUsed: tc.used, HugepagesReserved: tc.reserved, HugepageSizeBytes: page}
		if got := n.FreeHugepageBytes(); got != tc.want {
			t.Errorf("%s: FreeHugepageBytes = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestBuildTimeoutFromEnv: SPOOND_BUILD_TIMEOUT accepts a Go duration
// or a plain number of seconds, and a missing or malformed value falls
// back to the default. The image pipeline and the backend both read it,
// so they stay on one knob (spoond-4yl).
func TestBuildTimeoutFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  bool
		val  string
		want time.Duration
	}{
		{"unset", false, "", DefaultBuildTimeout},
		{"duration", true, "90m", 90 * time.Minute},
		{"seconds", true, "3600", time.Hour},
		{"zero", true, "0", DefaultBuildTimeout},
		{"negative seconds", true, "-5", DefaultBuildTimeout},
		{"malformed", true, "soon", DefaultBuildTimeout},
		{"duration zero", true, "0s", DefaultBuildTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("SPOOND_BUILD_TIMEOUT", tc.val)
			}
			if got := BuildTimeoutFromEnv(); got != tc.want {
				t.Fatalf("BuildTimeoutFromEnv = %v, want %v", got, tc.want)
			}
		})
	}
}
