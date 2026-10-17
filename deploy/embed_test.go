package deploy

import (
	"io/fs"
	"testing"
)

// TestFilesListed checks the embed covers exactly the files the
// installer writes for the 4.0 phases: the top-level units and env
// examples, and the e2b configs. A file added to deploy/ is not
// shipped until it is named in the go:embed pattern.
func TestFilesListed(t *testing.T) {
	want := map[string]bool{
		// top-level units and their env examples
		"spoond-backend.service":          true,
		"spoond-backend.env.example":      true,
		"spoond-sshd-gateway.service":     true,
		"spoond-gateway.env.example":      true,
		"spoond-dash.service":             true,
		"spoond-runner.service":           true,
		"spoond-runner.env.example":       true,
		// e2b orchestrator, firewall, flags, netwatch, otelcol
		// (e2b/spoond-netwatch.env.example is not embedded this slice:
		// netwatch is a runtime-phase unit, installed in slice S4)
		"e2b/e2b-orchestrator.service":    true,
		"e2b/orchestrator.env":            true,
		"e2b/e2b-guard.service":           true,
		"e2b/e2b-guard.nft":               true,
		"e2b/flags.json":                  true,
		"e2b/spoond-netwatch.service":     true,
		"e2b/spoond-netwatch.sh":          true,
		"e2b/otelcol-config.yaml":         true,
		// e2b/*.service also ships the drain and soak units (the drain
		// unit is what the spoond phase installs for --stop hooks)
		"e2b/spoond-drain.service":        true,
		"e2b/spoond-soak.service":         true,
	}
	got := map[string]bool{}
	err := fs.WalkDir(Files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			got[path] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded FS: %v", err)
	}
	for name := range want {
		if !got[name] {
			t.Errorf("deploy.Files missing %q (add it to the go:embed patterns)", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("deploy.Files embeds unexpected %q (embed only what install writes; extend the test when the phase table grows)", name)
		}
	}
}

// TestFilesReadable spot-checks a unit and an env example so an empty
// embed (patterns silently matching nothing is a go:embed footgun) is
// caught, not just a renamed one.
func TestFilesReadable(t *testing.T) {
	const unit = "spoond-backend.service"
	b, err := Files.ReadFile(unit)
	if err != nil {
		t.Fatalf("read %s: %v", unit, err)
	}
	if len(b) == 0 {
		t.Errorf("%s is empty", unit)
	}
	const env = "e2b/orchestrator.env"
	b, err = Files.ReadFile(env)
	if err != nil {
		t.Fatalf("read %s: %v", env, err)
	}
	if len(b) == 0 {
		t.Errorf("%s is empty", env)
	}
}