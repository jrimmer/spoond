package main

import (
	"strings"
	"testing"
)

func TestPrettySandboxTable(t *testing.T) {
	b := []byte(`{"sandboxes":[
		{"id":"0123456789abcdef0123456789abcdef","image":"dev-base","address":"10.42.0.2:8888","expires":1786408845,"persistent":true,"suspended":false,"name":"devbox"},
		{"id":"feedfacefeedfacefeedfacefeedface","image":"py-base","address":"10.42.0.3:8888","expires":1786400000,"persistent":false,"suspended":true,"name":"","comment":"demo box"}
	]}`)
	out := prettySandboxTable(b)
	if !strings.Contains(out, "ID") || !strings.Contains(out, "IMAGE") {
		t.Fatalf("missing header: %q", out)
	}
	if !strings.Contains(out, "0123456789ab…") {
		t.Fatalf("id prefix not truncated: %q", out)
	}
	if !strings.Contains(out, "running*") {
		t.Fatalf("persistent state marker missing: %q", out)
	}
	if !strings.Contains(out, "suspended") {
		t.Fatalf("suspended state missing: %q", out)
	}
	if !strings.Contains(out, "devbox") || !strings.Contains(out, "demo box") {
		t.Fatalf("name/comment column missing: %q", out)
	}
}

func TestPrettySandboxTableEmpty(t *testing.T) {
	if got := prettySandboxTable([]byte(`{"sandboxes":[]}`)); got != "no sandboxes" {
		t.Fatalf("empty: %q", got)
	}
}

func TestPrettySandboxTablePassThrough(t *testing.T) {
	// Not our JSON shape -> pass through raw.
	raw := `{"error":"boom"}`
	if got := prettySandboxTable([]byte(raw)); got != raw {
		t.Fatalf("passthrough: %q", got)
	}
}

func TestPrettyStat(t *testing.T) {
	b := []byte(`{"cpu":{"load1":0.25},"mem":{"used_mib":512,"total_mib":1536},"disk":{"used_mib":1024,"total_mib":4096},"net":{"rx_bytes":1000,"tx_bytes":2000}}`)
	out := prettyStat(b)
	for _, want := range []string{"cpu : load1 0.25", "mem : 512 / 1536 MiB used", "disk: 1024 / 4096 MiB used", "net : rx 1000 bytes, tx 2000 bytes"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
}

func TestRunControlCommandJSONFlag(t *testing.T) {
	// whoami --json and --json whoami both return raw JSON.
	if got := runControlCommand(t.Context(), "whoami --json", nil, "jrimmer"); got != `{"user":"ctl","key":"jrimmer"}` {
		t.Fatalf("whoami --json: %q", got)
	}
	if got := runControlCommand(t.Context(), "--json whoami", nil, "jrimmer"); got != `{"user":"ctl","key":"jrimmer"}` {
		t.Fatalf("--json whoami: %q", got)
	}
	// default pretty
	if got := runControlCommand(t.Context(), "whoami", nil, "jrimmer"); got != "user: ctl (key: jrimmer)" {
		t.Fatalf("whoami default: %q", got)
	}
}
