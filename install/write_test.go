package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestWriteAtomicReplacesRunningBinary: an upgrade replaces the binary
// a running service executes. Writing into it fails with "text file
// busy"; the rename does not.
func TestWriteAtomicReplacesRunningBinary(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary")
	}
	b, err := os.ReadFile(sleep)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "svc")
	if err := os.WriteFile(bin, b, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	if err := writeAtomic(bin, []byte("new build"), 0o755); err != nil {
		t.Fatalf("replace a running binary: %v", err)
	}
	got, err := os.ReadFile(bin)
	if err != nil || string(got) != "new build" {
		t.Fatalf("after replace: %q, %v", got, err)
	}
	fi, _ := os.Stat(bin)
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("mode %#o, want 0755", fi.Mode().Perm())
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(bin), ".svc.new-*"))
	if len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}
