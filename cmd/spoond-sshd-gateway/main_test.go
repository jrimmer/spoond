package spoondgateway

import (
	"os"
	"path/filepath"
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
	if got := runControlCommand(t.Context(), "whoami --json", nil, "jrimmer", "", ""); got != `{"user":"ctl","key":"jrimmer"}` {
		t.Fatalf("whoami --json: %q", got)
	}
	if got := runControlCommand(t.Context(), "--json whoami", nil, "jrimmer", "", ""); got != `{"user":"ctl","key":"jrimmer"}` {
		t.Fatalf("--json whoami: %q", got)
	}
	// default pretty
	if got := runControlCommand(t.Context(), "whoami", nil, "jrimmer", "", ""); got != "user: ctl (key: jrimmer)" {
		t.Fatalf("whoami default: %q", got)
	}
	// identity-store user takes precedence in whoami
	if got := runControlCommand(t.Context(), "whoami", nil, "jrimmer", "u-abc123", "jason"); got != "user: jason (key: jrimmer)" {
		t.Fatalf("whoami with user: %q", got)
	}
	if got := runControlCommand(t.Context(), "whoami --json", nil, "jrimmer", "u-abc123", "jason"); got != `{"user":"jason","key":"jrimmer","user_id":"u-abc123"}` {
		t.Fatalf("whoami --json with user: %q", got)
	}
	// ssh-key help
	if got := runControlCommand(t.Context(), "ssh-key", nil, "jrimmer", "", ""); !strings.Contains(got, "usage: ssh-key") {
		t.Fatalf("ssh-key usage: %q", got)
	}
}

func TestLoadAuthorizedKeysDirScan(t *testing.T) {
	// Generate two key pairs in a temp dir; a directory scan must load
	// both .pub files; a CSV of paths must also work unchanged.
	dir := t.TempDir()
	// ed25519Generate(path) writes the private key to path and the
	// public key to path+".pub" — use bare basenames so the .pub files
	// hold real authorized keys.
	k1 := filepath.Join(dir, "alice")
	k2 := filepath.Join(dir, "bob")
	k3 := filepath.Join(dir, "ignore.txt")
	genKeyFile(t, k1)
	genKeyFile(t, k2)
	writeTestFile(t, k3, "not a key")

	keys, err := loadAuthorizedKeys(dir)
	if err != nil {
		t.Fatalf("dir scan: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("dir scan: want 2 keys, got %d", len(keys))
	}

	keys, err = loadAuthorizedKeys(k1 + ".pub," + k2 + ".pub")
	if err != nil {
		t.Fatalf("csv: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("csv: want 2 keys, got %d", len(keys))
	}
}

func genKeyFile(t *testing.T, path string) {
	t.Helper()
	_, priv, err := ed25519Generate(path)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	_ = priv
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}
