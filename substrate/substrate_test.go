package substrate

import "testing"

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
