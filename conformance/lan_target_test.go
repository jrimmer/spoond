//go:build conformance

package conformance

import "testing"

// TestParseLANTarget pins the CONFORMANCE_LAN_TARGET parsing: a well-formed
// `host:port` passes, and an empty host or an out-of-range port is
// rejected (loadConfig then fails rather than probing a bad address).
func TestParseLANTarget(t *testing.T) {
	for _, tc := range []struct {
		in      string
		host    string
		port    int
		wantErr bool
	}{
		{"10.1.0.203:443", "10.1.0.203", 443, false},
		{"git.example.com:8443", "git.example.com", 8443, false},
		{"10.1.0.203:0", "", 0, true},
		{"10.1.0.203:65536", "", 0, true},
		{"10.1.0.203", "", 0, true},
		{":443", "", 0, true},
		{"10.1.0.203:https", "", 0, true},
	} {
		host, port, err := parseLANTarget(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseLANTarget(%q) = %q, %d, nil; want an error", tc.in, host, port)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseLANTarget(%q): %v", tc.in, err)
			continue
		}
		if host != tc.host || port != tc.port {
			t.Errorf("parseLANTarget(%q) = %q, %d; want %q, %d", tc.in, host, port, tc.host, tc.port)
		}
	}
}
