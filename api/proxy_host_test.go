package api

import "testing"

func TestParseProxyHost2SingleLabel(t *testing.T) {
	cases := []struct {
		host        string
		label, user string
		port        int
		ok          bool
	}{
		{"deadbeef.sandbox.example.com", "deadbeef", "", 3000, true},
		{"deadbeef-8080.sandbox.example.com", "deadbeef", "", 8080, true},
		{"myweb.sandbox.example.com", "myweb", "", 3000, true},
		{"myweb-3001.sandbox.example.com", "myweb", "", 3001, true},
		{"sandbox.example.com", "", "", 0, false},
		{"deadbeef.sandbox.example.com:443", "deadbeef", "", 3000, true},
	}
	for _, c := range cases {
		label, user, port, ok := parseProxyHost2(c.host, defaultProxyHostSuffix)
		if ok != c.ok || label != c.label || user != c.user || port != c.port {
			t.Errorf("parseProxyHost2(%q) = (%q,%q,%d,%v), want (%q,%q,%d,%v)",
				c.host, label, user, port, ok, c.label, c.user, c.port, c.ok)
		}
	}
}

func TestParseProxyHost2TwoLabel(t *testing.T) {
	cases := []struct {
		host        string
		label, user string
		port        int
		ok          bool
	}{
		{"deadbeef.jason.sandbox.example.com", "deadbeef", "jason", 3000, true},
		{"myweb.trina.sandbox.example.com", "myweb", "trina", 3000, true},
		{"a.b.c.sandbox.example.com", "", "", 0, false}, // too many labels
		{"deadbeef..sandbox.example.com", "", "", 0, false},
	}
	for _, c := range cases {
		label, user, port, ok := parseProxyHost2(c.host, defaultProxyHostSuffix)
		if ok != c.ok || label != c.label || user != c.user || port != c.port {
			t.Errorf("parseProxyHost2(%q) = (%q,%q,%d,%v), want (%q,%q,%d,%v)",
				c.host, label, user, port, ok, c.label, c.user, c.port, c.ok)
		}
	}
}
