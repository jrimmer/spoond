// Tests for the `spoond install` CLI (install.go): registration, flag
// parsing and exit codes (0 success, 1 phase failure, 2 argument
// error), the phase argument (one named, all by default), --dry-run
// and --root pass-through into the Env, and that a phase failure still
// prints the counts line. The check phase runs against a fake host
// tree so the CLI tests never touch the real host; the other phases
// are stubbed.
package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jrimmer/spoond/v3/install"
)

// stdoutPipe swaps os.Stdout for a pipe and returns a closer that
// ends the capture, waits for the reader and returns what was written.
// installMain's report and counts line go to the process stdout.
func stdoutPipe(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = buf.ReadFrom(r)
		_ = r.Close()
		close(done)
	}()
	old := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = old })
	var once sync.Once
	return func() string {
		once.Do(func() {
			// installMain has returned: no further writes, so close
			// the write end and let the reader hit EOF.
			_ = w.Close()
			<-done
		})
		return buf.String()
	}
}

// fakeProber answers the port probe from a table: a port in the map
// is busy, anything else is free; see install/check_test.go.
type fakeProber struct{ busy map[string]bool }

func (f fakeProber) ListenOK(port string) (bool, error) {
	return !f.busy[port], nil
}

// fakeSpacer answers the free-space probe with a fixed byte count; see
// install/check_test.go.
type fakeSpacer struct{ free uint64 }

func (f fakeSpacer) FreeBytes(string) (uint64, error) { return f.free, nil }

// seedFakeHost writes the files the check phase reads under root, for
// a passing host; see install/check_test.go for the full matrix.
func seedFakeHost(t *testing.T, root string) {
	t.Helper()
	write := func(rel, content string) {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("etc/os-release", "ID=debian\nVERSION_ID=13\n")
	write("proc/meminfo", "MemTotal:       134217728 kB\n") // 128 GiB
	write("proc/sys/kernel/osrelease", "6.1.0-13-amd64\n")
	write("dev/kvm", "")
}

// withPhase swaps one entry of the phases table for fn and restores it
// afterwards.
func withPhase(t *testing.T, name string, fn install.Phase) {
	t.Helper()
	old, had := install.Phases[name]
	install.Phases[name] = fn
	t.Cleanup(func() {
		if had {
			install.Phases[name] = old
		} else {
			delete(install.Phases, name)
		}
	})
}

// TestCLIRegistered says the command is registered with its name and a
// run entry point, in the shape main.go dispatches.
func TestCLIRegistered(t *testing.T) {
	cmd, ok := commands["install"]
	if !ok {
		t.Fatal("spoond install is not registered")
	}
	if cmd.run == nil {
		t.Error("registered with a nil run")
	}
	if cmd.desc == "" {
		t.Error("registered without a description")
	}
}

// TestCLIUsageBare: no arguments print the usage and exit 2.
func TestCLIUsageBare(t *testing.T) {
	// installMain with no args must print usage and stop; the bare
	// parse must not fall through to running phases on the real host.
	restore := install.TestProbes(fakeProber{}, fakeSpacer{free: 200 << 30})
	defer restore()
	code := installMain([]string{})
	if code != 2 {
		t.Errorf("bare install: exit %d, want 2 (usage)", code)
	}
}

// TestCLIBadPhase: an unknown phase is exit 2, and the usage names the
// phases that exist.
func TestCLIBadPhase(t *testing.T) {
	code := installMain([]string{"--root", t.TempDir(), "frobnicate"})
	if code != 2 {
		t.Errorf("unknown phase: exit %d, want 2", code)
	}
}

// TestCLITooManyPhases: at most one phase name.
func TestCLITooManyPhases(t *testing.T) {
	code := installMain([]string{"--root", t.TempDir(), "check", "host"})
	if code != 2 {
		t.Errorf("two phase args: exit %d, want 2", code)
	}
}

// TestCLIRunsOnePhase: only the named phase runs — the stubbed host
// phase records itself when called, and is silent when only check was
// asked for.
func TestCLIRunsOnePhase(t *testing.T) {
	root := t.TempDir()
	seedFakeHost(t, root)
	ran := ""
	withPhase(t, "host", func(ctx context.Context, env *install.Env) ([]install.Item, error) {
		ran = "host"
		return nil, nil
	})
	// The real check's port and space probes read the test host; the
	// CLI passes its own unswapped Env. Point them at the fakes the
	// install package exposes to tests.
	restore := install.TestProbes(fakeProber{}, fakeSpacer{free: 200 << 30})
	defer restore()
	code := installMain([]string{"--root", root, "check"})
	if code != 0 {
		t.Fatalf("check on a good host: exit %d", code)
	}
	if ran != "" {
		t.Error("the host phase ran when only check was named")
	}
}

// TestCLIRunsAllByDefault: no phase argument runs every phase in
// table order; the stubbed host notes itself between check and the
// unimplemented runtime.
func TestCLIRunsAllByDefault(t *testing.T) {
	root := t.TempDir()
	seedFakeHost(t, root)
	var order []string
	withPhase(t, "host", func(ctx context.Context, env *install.Env) ([]install.Item, error) {
		order = append(order, "host")
		return nil, nil
	})
	// The four phases after check are placeholder rows in this slice;
	// patch them so the whole table can run (check would fail through
	// the real probes otherwise).
	for _, name := range []string{"runtime", "spoond", "images", "doctor"} {
		withPhase(t, name, func(ctx context.Context, env *install.Env) ([]install.Item, error) {
			order = append(order, "patched")
			return nil, nil
		})
	}
	restore := install.TestProbes(fakeProber{}, fakeSpacer{free: 200 << 30})
	defer restore()
	out := stdoutPipe(t)
	code := installMain([]string{"--root", root})
	if code != 0 {
		t.Fatalf("all phases: exit %d", code)
	}
	if len(order) != 5 {
		t.Errorf("phases after check = %v, want host once and the 4 stubs", order)
	}
	if text := out(); !strings.Contains(text, "spoond install done: ") {
		t.Errorf("the counts line is missing:\n%s", text)
	}
}

// TestCLIFailureExitAndSummary: a phase failure exits 1, and the
// counts line still prints after the error so the operator sees what
// went through.
func TestCLIFailureExitAndSummary(t *testing.T) {
	root := t.TempDir()
	ran := ""
	withPhase(t, "check", func(ctx context.Context, env *install.Env) ([]install.Item, error) {
		r := &install.Reporter{Phase: "check", Out: env.Out}
		r.Add(install.Created, "/etc/spoond/one", "")
		r.Add(install.Failed, "the-wall", "tear down the wall first")
		ran = "check"
		return r.Items(), errors.New("check: the-wall: tear down the wall first")
	})
	withPhase(t, "host", func(ctx context.Context, env *install.Env) ([]install.Item, error) {
		ran += "+host"
		return nil, nil
	})
	out := stdoutPipe(t)
	code := installMain([]string{"--dry-run", "--root", root, "check"})
	if code != 1 {
		t.Errorf("failing phase: exit %d, want 1", code)
	}
	if ran != "check" {
		t.Errorf("the later phases ran after the failure (%q)", ran)
	}
	text := out()
	if !strings.Contains(text, "1 created, 0 changed, 0 kept, 0 skipped, 1 failed") {
		t.Errorf("the counts line is missing after a failure:\n%s", text)
	}
	if !strings.Contains(text, "tear down the wall first") {
		t.Errorf("the error with the fix should print before the summary:\n%s", text)
	}
}

// TestCLIDryRunPassesThrough: --dry-run makes the Env's DryRun, and
// the summary says nothing was written.
func TestCLIDryRunPassesThrough(t *testing.T) {
	root := t.TempDir()
	seedFakeHost(t, root)
	var dry bool
	withPhase(t, "doctor", func(ctx context.Context, env *install.Env) ([]install.Item, error) {
		dry = env.DryRun
		return nil, nil
	})
	out := stdoutPipe(t)
	code := installMain([]string{"--root", root, "--dry-run", "doctor"})
	if code != 0 {
		t.Fatalf("dry-run doctor: exit %d", code)
	}
	if !dry {
		t.Error("the phase saw DryRun=false under --dry-run")
	}
	if !strings.Contains(out(), "nothing written, no command run") {
		t.Errorf("the dry-run summary should say so:\n%s", out())
	}
}

// TestCLIRootPassesThrough: --root is the Env's Root.
func TestCLIRootPassesThrough(t *testing.T) {
	root := t.TempDir()
	var seen string
	withPhase(t, "doctor", func(ctx context.Context, env *install.Env) ([]install.Item, error) {
		seen = env.Root
		return nil, nil
	})
	installMain([]string{"--root", root, "doctor"})
	if seen != root {
		t.Errorf("phase saw Root=%q, want the --root value %q", seen, root)
	}
}

// TestCLIRealCheckRunsAgainstRoot: the real check phase through the
// CLI over a seeded temp root. The file readings are real; the port
// and space probes hit the test host, so they are swapped through
// install's test hook, the same one install/check_test.go uses.
func TestCLIRealCheckRunsAgainstRoot(t *testing.T) {
	root := t.TempDir()
	seedFakeHost(t, root)
	restore := install.TestProbes(fakeProber{}, fakeSpacer{free: 200 << 30})
	defer restore()
	out := stdoutPipe(t)
	code := installMain([]string{"--root", root, "check"})
	if code != 0 {
		t.Errorf("check over a good seeded host: exit %d", code)
	}
	if !strings.Contains(out(), "kept     /dev/kvm") {
		t.Errorf("the report should show the real phase's rows:\n%s", out())
	}
}
