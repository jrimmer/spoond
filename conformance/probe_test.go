//go:build conformance

package conformance

import (
	"fmt"
	"strings"
	"testing"
)

// TestProbeCmdRenderedCleanly pins the probe command construction: every
// template receives exactly its own arguments, so the rendered command
// carries no fmt error markers (`%!(EXTRA ...)` — whose parentheses break
// the guest shell), and both the host and the port are interpolated.
func TestProbeCmdRenderedCleanly(t *testing.T) {
	begin(t)
	const host = "10.11.0.4"
	for _, tc := range []struct {
		name string
		port int
	}{
		{"raw probe, port 9042", 9042},
		{"TLS probe, port 443", 443},
	} {
		cmd := probeCmd(host, tc.port)
		if strings.Contains(cmd, "%!") {
			failf(t, "%s: fmt error marker in rendered command: %q", tc.name, cmd)
		}
		if !strings.Contains(cmd, fmt.Sprintf("%q, %d", host, tc.port)) {
			failf(t, "%s: host/port not interpolated as (\"host\", port): %q", tc.name, cmd)
		}
		if tc.port == 443 && !strings.Contains(cmd, fmt.Sprintf("server_hostname=%q", host)) {
			failf(t, "%s: server_hostname not interpolated: %q", tc.name, cmd)
		}
	}
}
