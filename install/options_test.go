package install

import (
	"os"
	"strings"
	"testing"
)

func TestOptionsValidate(t *testing.T) {
	cases := []struct {
		name string
		opt  Options
		want string // substring of the error; empty means valid
	}{
		{"zero", Options{}, ""},
		{"hugepages count", Options{Hugepages: "12288"}, ""},
		{"hugepages auto", Options{Hugepages: "auto"}, ""},
		{"hugepages word", Options{Hugepages: "lots"}, "--hugepages"},
		{"hugepages zero", Options{Hugepages: "0"}, "--hugepages"},
		{"images none", Options{Images: "none"}, ""},
		{"images manifest", Options{Images: "/srv/catalog/manifest.yaml"}, ""},
		{"images all (4.0 catalog)", Options{Images: "all"}, "ships no image catalog"},
		{"images minimal (4.0 catalog)", Options{Images: "minimal"}, "ships no image catalog"},
		{"base registry with a manifest", Options{Images: "m.yaml", BaseRegistry: "r.example.com/x"}, ""},
		{"base registry alone", Options{BaseRegistry: "r.example.com/x"}, "--base-registry"},
		{"tls pair", Options{TLSCert: "c", TLSKey: "k"}, ""},
		{"tls cert alone", Options{TLSCert: "c"}, "--tls-cert"},
		{"runner pair", Options{RunnerURL: "u", RunnerTokenFile: "f"}, ""},
		{"runner url alone", Options{RunnerURL: "u"}, "--runner-url"},
		{"config-only alone", Options{ConfigOnly: true}, "--config-only"},
		{"config-only in container", Options{ConfigOnly: true, Container: true}, ""},
	}
	for _, c := range cases {
		err := c.opt.Validate()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: error %v, want one naming %s", c.name, err, c.want)
		}
	}
}

func TestOptionsDefaults(t *testing.T) {
	var o Options
	if o.storeDir() != DefaultStoreDir || o.hugepages() != HugepagesAuto || o.images() != ImagesNone {
		t.Fatalf("defaults: store %q hugepages %q images %q", o.storeDir(), o.hugepages(), o.images())
	}
}

// A changed file takes the requested mode, and a file with the right
// content but the wrong mode is fixed rather than kept.
func TestWriteFileFixesMode(t *testing.T) {
	env, _, _ := testEnv(t)
	r := &Reporter{Phase: "t"}
	const path = "/etc/spoond/x.env"
	full := env.Resolve(path)
	if err := os.MkdirAll(env.Resolve("/etc/spoond"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := env.writeFile(r, path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, mode := read(t, env, path); mode != 0o600 {
		t.Fatalf("changed file mode %o, want 600", mode)
	}

	if err := os.Chmod(full, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := env.writeFile(r, path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, mode := read(t, env, path); mode != 0o600 {
		t.Fatalf("same content, wrong mode: mode %o, want 600", mode)
	}
	items := r.Items()
	if got := items[len(items)-1].Result; got != Changed {
		t.Fatalf("mode-only fix reported %s, want changed", got)
	}

	if err := env.writeFile(r, path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	items = r.Items()
	if got := items[len(items)-1].Result; got != Kept {
		t.Fatalf("unchanged file reported %s, want kept", got)
	}
}
