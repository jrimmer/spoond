//go:build linux

package spoondgateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// TestParseSSHKeyAdd (#72): a real authorized-keys line, split on
// whitespace as runControlCommand does, registers in both argument
// orders, with or without a comment and with quotes; a bad key is
// refused with the usage.
func TestParseSSHKeyAdd(t *testing.T) {
	pk, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	spk, err := ssh.NewPublicKey(pk)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(spk)))
	want := ssh.FingerprintSHA256(spk)
	for _, cmd := range []string{
		"ssh-key add " + line + " jason",
		"ssh-key add " + line + " jason@laptop jason",
		`ssh-key add "` + line + ` jason@laptop" jason`,
		"ssh-key add jason " + line,
		"ssh-key add jason " + line + " jason@laptop",
	} {
		fields := strings.Fields(cmd)
		pub, name, err := parseSSHKeyAdd(fields[2:])
		if err != nil {
			t.Fatalf("%q: %v", cmd, err)
		}
		if name != "jason" || ssh.FingerprintSHA256(pub) != want {
			t.Fatalf("%q: name %q key %s, want jason %s", cmd, name, ssh.FingerprintSHA256(pub), want)
		}
	}
	for _, cmd := range []string{"ssh-key add jason", "ssh-key add ssh-ed25519 notbase64 jason"} {
		if _, _, err := parseSSHKeyAdd(strings.Fields(cmd)[2:]); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Fatalf("%q: err %v, want a usage error", cmd, err)
		}
	}
}
