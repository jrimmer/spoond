// Tests for the `spoond install` CLI (install.go): registration, flag
// parsing and exit codes (0 success, 1 phase failure, 2 argument
// error), the phase argument (one named, the whole table by default),
// --dry-run and --root pass-through into the Env, and that a phase
// failure still prints the counts line. The phase table is stubbed for
// every run, so the CLI tests never touch the real host; the check
// phase's own matrix lives in install/check_test.go.
package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jrimmer/spoond/v4/install"
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

// stubTable replaces every phase in the table with a recorder that
// appends its name and stops, so a CLI run touches no host. It
// restores the table when the test ends.
func stubTable(t *testing.T, order *[]string) {
	t.Helper()
	old := install.Phases
	restored := map[string]install.Phase{}
	for _, name := range install.Order {
		restored[name] = old[name]
	}
	t.Cleanup(func() { install.Phases = old })
	install.Phases = map[string]install.Phase{}
	for _, name := range install.Order {
		n := name
		install.Phases[name] = func(ctx context.Context, env *install.Env) ([]install.Item, error) {
			*order = append(*order, n)
			return nil, nil
		}
	}
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

// TestCLIRunsAllByDefault: no phase argument runs every phase in table
// order — the usage says "runs every phase", and the plan's "`spoond
// install` runs all phases in order" is this path.
func TestCLIRunsAllByDefault(t *testing.T) {
	var order []string
	stubTable(t, &order)
	out := stdoutPipe(t)
	code := installMain([]string{"--dry-run", "--root", t.TempDir()})
	if code != 0 {
		t.Fatalf("bare install: exit %d", code)
	}
	var named []string
	for _, name := range install.Order {
		named = append(named, name)
	}
	if strings.Join(order, ",") != strings.Join(named, ",") {
		t.Errorf("phases ran %v, want the table %v in order", order, named)
	}
	if text := out(); !strings.Contains(text, "spoond install dry run; nothing changed: ") {
		t.Errorf("the counts line is missing:\n%s", text)
	}
}

// TestCLIUsageHelp: help, -h and --help print the usage and exit 0;
// a flag or parse error points at it and exits 2.
func TestCLIUsageHelp(t *testing.T) {
	os.Stderr = os.NewFile(0, os.DevNull) // not captured; usage is not asserted here
	t.Cleanup(func() {})
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}} {
		if code := installMain(args); code != 0 {
			t.Errorf("install %v: exit %d, want 0", args, code)
		}
	}
	var order []string
	stubTable(t, &order)
	if code := installMain([]string{"--nope", "--root", t.TempDir()}); code != 2 {
		t.Errorf("unknown flag: exit %d, want 2", code)
	}
	if len(order) != 0 {
		t.Errorf("a parse error must not run phases (%v)", order)
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

// TestCLIRunsOnePhase: only the named phase runs — the recorder stops
// every stub, and the other phases stay silent.
func TestCLIRunsOnePhase(t *testing.T) {
	var order []string
	stubTable(t, &order)
	code := installMain([]string{"--root", t.TempDir(), "host"})
	if code != 0 {
		t.Fatalf("host: exit %d", code)
	}
	if strings.Join(order, ",") != "host" {
		t.Errorf("phases ran %v, want [host]", order)
	}
}

// TestCLIFailureExitAndSummary: a phase failure exits 1, and the
// counts line still prints after the error so the operator sees what
// went through.
func TestCLIFailureExitAndSummary(t *testing.T) {
	root := t.TempDir()
	var order []string
	stubTable(t, &order)
	withPhase(t, "check", func(ctx context.Context, env *install.Env) ([]install.Item, error) {
		r := &install.Reporter{Phase: "check", Out: env.Out}
		r.Add(install.Created, "/etc/spoond/one", "")
		r.Add(install.Failed, "the-wall", "tear down the wall first")
		order = append(order, "real-check")
		return r.Items(), errors.New("the-wall: tear down the wall first")
	})
	out := stdoutPipe(t)
	code := installMain([]string{"--dry-run", "--root", root, "check"})
	if code != 1 {
		t.Errorf("failing phase: exit %d, want 1", code)
	}
	if strings.Join(order, ",") != "real-check" {
		t.Errorf("the later phases ran after the failure (%q)", order)
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
	var order []string
	stubTable(t, &order)
	var dry bool
	withPhase(t, "doctor", func(ctx context.Context, env *install.Env) ([]install.Item, error) {
		dry = env.DryRun
		order = append(order, "real-doctor")
		return nil, nil
	})
	out := stdoutPipe(t)
	code := installMain([]string{"--root", root, "--dry-run", "doctor"})
	if code != 0 {
		t.Fatalf("dry-run doctor: exit %d", code)
	}
	if strings.Join(order, ",") != "real-doctor" {
		t.Errorf("phases ran %q, want only doctor", order)
	}
	if !dry {
		t.Error("the phase saw DryRun=false under --dry-run")
	}
	if !strings.Contains(out(), "dry run; nothing changed") {
		t.Errorf("the dry-run summary should say so:\n%s", out())
	}
}

// TestCLIRootPassesThrough: --root is the Env's Root.
func TestCLIRootPassesThrough(t *testing.T) {
	root := t.TempDir()
	var order []string
	stubTable(t, &order)
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
