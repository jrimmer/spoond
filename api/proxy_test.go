package api

import "testing"

func TestParseProxyHost(t *testing.T) {
	cases := []struct {
		host     string
		wantID   string
		wantPort int
		wantOK   bool
	}{
		{"a1b2c3d4e5f60718293a4b5c6d7e8f90.sandbox.lacy.casa", "a1b2c3d4e5f60718293a4b5c6d7e8f90", 3000, true},
		{"a1b2c3d4e5f60718293a4b5c6d7e8f90.sandbox.lacy.casa:443", "a1b2c3d4e5f60718293a4b5c6d7e8f90", 3000, true},
		{"a1b2c3d4e5f60718293a4b5c6d7e8f90-8080.sandbox.lacy.casa", "a1b2c3d4e5f60718293a4b5c6d7e8f90", 8080, true},
		{"A1B2C3D4E5F60718293A4B5C6D7E8F90.sandbox.lacy.casa", "a1b2c3d4e5f60718293a4b5c6d7e8f90", 3000, true},
		{"sandbox.lacy.casa", "", 0, false},
		{"nope.sandbox.lacy.casa", "nope", 3000, true},        // friendly name (resolves at lookup)
		{"mybox-9000.sandbox.lacy.casa", "mybox", 9000, true}, // name + port
		{"mybox-0.sandbox.lacy.casa", "mybox-0", 3000, true},  // hyphenated name, not a port
		{"toolongid123456789012345678901234567890.sandbox.lacy.casa", "toolongid123456789012345678901234567890", 3000, true}, // 39 chars — valid name
		{"abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnopqrstuvwxyz0123456789.sandbox.lacy.casa", "", 0, false},         // >63 chars
		{"a1b2c3d4e5f60718293a4b5c6d7e8f90-0.sandbox.lacy.casa", "", 0, false},                                               // port 0 invalid
		{"a1b2c3d4e5f60718293a4b5c6d7e8f90-70000.sandbox.lacy.casa", "", 0, false},                                           // port >65535 invalid
		{"other.example.com", "", 0, false},
	}
	for _, c := range cases {
		id, port, ok := parseProxyHost(c.host)
		if id != c.wantID || port != c.wantPort || ok != c.wantOK {
			t.Errorf("parseProxyHost(%q) = (%q,%d,%v), want (%q,%d,%v)",
				c.host, id, port, ok, c.wantID, c.wantPort, c.wantOK)
		}
	}
}
