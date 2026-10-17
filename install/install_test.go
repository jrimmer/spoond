// Tests for the install framework (install.go, phases.go): write
// semantics (created/changed/kept, keep=true never overwrites, mode
// 0600), dry-run writing nothing, RunAll's ordering and stop-on-error,
// bad phase names, and the production Cmd runner against real `true`
// and `false`. Every test drives the file helpers through phases, the
// same path a real phase takes.
package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// phaseOf builds a named phase from per-file write calls. It is the
// shape every real phase has: a Reporter over env.Out and file writes.
func phaseOf(name string, writes ...func(ctx context.Context, env *Env, r *Reporter) error) Phase {
	return func(ctx context.Context, env *Env) ([]Item, error) {
		r := &Reporter{Phase: name, Out: env.Out}
		for _, w := range writes {
			if err := w(ctx, env, r); err != nil {
				return r.Items(), err
			}
		}
		return r.Items(), nil
	}
}

// testEnv returns an Env over a temp root with a buffering Out and a
// runner recording every call.
func testEnv(t *testing.T) (*Env, *bytes.Buffer, *recordRunner) {
	t.Helper()
	out := &bytes.Buffer{}
	runner := &recordRunner{}
	env := &Env{Root: t.TempDir(), Run: runner, Out: out}
	return env, out, runner
}

// read resolves path under env.Root and returns the file's content and
// mode, for assertions on what the helpers actually wrote.
func read(t *testing.T, env *Env, path string) ([]byte, os.FileMode) {
	t.Helper()
	b, err := os.ReadFile(env.Resolve(path))
	if err != nil {
		t.Fatalf("read %s (under %s): %v", path, env.Root, err)
	}
	fi, err := os.Stat(env.Resolve(path))
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return b, fi.Mode().Perm()
}

// mustNotExist fails when a path exists under the root.
func mustNotExist(t *testing.T, env *Env, path string) {
	t.Helper()
	if _, err := os.Stat(env.Resolve(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s exists under %s, want absent (stat: %v)", path, env.Root, err)
	}
}

// recordRunner records (name, args) pairs and answers from a table.
type recordRunner struct {
	calls []string
	// reply maps "name arg1 arg2" to stdout bytes and an error; an
	// absent command is an error, so a bug shows as a failure.
	reply map[string]fakeReply
}

type fakeReply struct {
	out []byte
	err error
}

func (r *recordRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	if r.reply == nil {
		return nil, fmt.Errorf("unexpected command %s %v", name, args)
	}
	rep, ok := r.reply[strings.Join(append([]string{name}, args...), " ")]
	if !ok {
		return nil, fmt.Errorf("unexpected command %s %v", name, args)
	}
	return rep.out, rep.err
}

// TestWriteFileCreatedChangedKept walks one file through the three
// outcomes: absent -> created, same content -> kept, different content
// -> changed (an ordinary file may be overwritten).
func TestWriteFileCreatedChangedKept(t *testing.T) {
	env, out, _ := testEnv(t)

	fn := PhaseCheck
	Phases[fn] = phaseOf(fn, func(ctx context.Context, env *Env, r *Reporter) error {
		return env.writeFile(r, "/etc/spoond/units.conf", []byte("v1\n"), 0o644)
	})
	if _, err := RunAll(context.Background(), env, fn); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "created") {
		t.Errorf("first run report, want a created row:\n%s", got)
	}
	b, mode := read(t, env, "/etc/spoond/units.conf")
	if string(b) != "v1\n" {
		t.Errorf("content = %q, want %q", b, "v1\n")
	}
	if mode != 0o644 {
		t.Errorf("mode = %#o, want the requested 0644", mode)
	}

	// Same content again: kept, and the file is untouched.
	out.Reset()
	if _, err := RunAll(context.Background(), env, fn); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "kept") || strings.Contains(got, "changed") {
		t.Errorf("second run report, want kept and no changed:\n%s", got)
	}

	// Different content: changed.
	out.Reset()
	Phases[fn] = phaseOf(fn, func(ctx context.Context, env *Env, r *Reporter) error {
		return env.writeFile(r, "/etc/spoond/units.conf", []byte("v2\n"), 0o644)
	})
	items, err := RunAll(context.Background(), env, fn)
	if err != nil {
		t.Fatalf("third run: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("third run produced %d rows, want 1", len(items))
	}
	if items[0].Result != Changed {
		t.Errorf("third run result = %s, want changed", items[0].Result)
	}
	b, _ = read(t, env, "/etc/spoond/units.conf")
	if string(b) != "v2\n" {
		t.Errorf("content after change = %q, want %q", b, "v2\n")
	}
}

// TestWriteFileDefaultsTo0600 says the mode helpers default to: a
// plain write without a mode lands 0600.
func TestWriteFileDefaultsTo0600(t *testing.T) {
	env, _, _ := testEnv(t)
	fn := PhaseCheck
	Phases[fn] = phaseOf(fn, func(ctx context.Context, env *Env, r *Reporter) error {
		return env.writeFile(r, "/etc/spoond/unit.conf", []byte("x"), 0)
	})
	if _, err := RunAll(context.Background(), env, fn); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, mode := read(t, env, "/etc/spoond/unit.conf"); mode != 0o600 {
		t.Errorf("mode = %#o, want the 0600 default", mode)
	}
}

// TestWriteFileKeepNeverOverwrites is the secret rule: an existing
// secret or env file is kept whatever the phase wanted to write, and
// its content is never read back or printed.
func TestWriteFileKeepNeverOverwrites(t *testing.T) {
	env, out, _ := testEnv(t)
	const path = "/etc/spoond/backend.env"
	if err := os.MkdirAll(env.Resolve(filepath.Dir(path)), 0o755); err != nil {
		t.Fatal(err)
	}
	old := []byte("CONSUMER_TOKENS=operator-secret\n")
	if err := os.WriteFile(env.Resolve(path), old, 0o600); err != nil {
		t.Fatal(err)
	}

	fn := PhaseCheck
	Phases[fn] = phaseOf(fn, func(ctx context.Context, env *Env, r *Reporter) error {
		return env.writeFileKeep(r, path, []byte("CONSUMER_TOKENS=generated-secret\n"))
	})
	items, err := RunAll(context.Background(), env, fn)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(items) != 1 || items[0].Result != Kept {
		t.Errorf("rows = %+v, want one kept row for the existing secret", items)
	}
	b, mode := read(t, env, path)
	if !bytes.Equal(b, old) {
		t.Errorf("existing secret file was overwritten: now %q", b)
	}
	if mode != 0o600 {
		t.Errorf("mode = %#o, want 0600", mode)
	}
	if got := out.String(); strings.Contains(got, "operator-secret") || strings.Contains(got, "generated-secret") {
		t.Errorf("a secret value reached Out; the row names the file, not the value:\n%s", got)
	}

	// Absent file: writeKeep creates it, 0600.
	out.Reset()
	const fresh = "/etc/spoond/gateway.env"
	Phases[fn] = phaseOf(fn, func(ctx context.Context, env *Env, r *Reporter) error {
		return env.writeFileKeep(r, fresh, []byte("GATEWAY_TOKEN=new\n"))
	})
	if _, err := RunAll(context.Background(), env, fn); err != nil {
		t.Fatalf("run: %v", err)
	}
	b, mode = read(t, env, fresh)
	if string(b) != "GATEWAY_TOKEN=new\n" || mode != 0o600 {
		t.Errorf("fresh keep write = %q, mode %#o; want the content and 0600", b, mode)
	}
}

// TestWriteFileKeepContentNotCompared pins the difference from
// writeFile: two identical keep writes both report kept but a changed
// keep write reports kept too — the existing content wins either way.
func TestWriteFileKeepContentNotCompared(t *testing.T) {
	env, _, _ := testEnv(t)
	const path = "/etc/spoond/dash.env"
	if err := os.MkdirAll(env.Resolve(filepath.Dir(path)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.Resolve(path), []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fn := PhaseCheck
	Phases[fn] = phaseOf(fn, func(ctx context.Context, env *Env, r *Reporter) error {
		return env.writeFileKeep(r, path, []byte("different\n"))
	})
	items, err := RunAll(context.Background(), env, fn)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(items) != 1 || items[0].Result != Kept {
		t.Errorf("rows = %+v, want one kept row (keep never inspects or replaces content)", items)
	}
	if b, _ := read(t, env, path); string(b) != "old\n" {
		t.Errorf("content = %q, want the pre-existing %q", b, "old\n")
	}
}

// TestDryRunWritesNothingAndRunsNoCommand: the report shows every row
// the real run would produce, but no file is created and the runner is
// never called.
func TestDryRunWritesNothingAndRunsNoCommand(t *testing.T) {
	env, out, runner := testEnv(t)
	env.DryRun = true

	fn := PhaseCheck
	Phases[fn] = phaseOf(fn,
		func(ctx context.Context, env *Env, r *Reporter) error {
			return env.writeFile(r, "/etc/spoond/new.conf", []byte("v1\n"), 0o644)
		},
		func(ctx context.Context, env *Env, r *Reporter) error {
			return env.writeFileKeep(r, "/etc/spoond/new.env", []byte("TOKEN=t\n"))
		},
	)
	items, err := RunAll(context.Background(), env, fn)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("dry run produced %d rows, want 2 (the report still shows the plan):\n%s", len(items), out.String())
	}
	res := map[string]Result{}
	for _, it := range items {
		res[it.Name] = it.Result
	}
	if res["/etc/spoond/new.conf"] != Created || res["/etc/spoond/new.env"] != Created {
		t.Errorf("dry-run rows = %v, want created for both new files", res)
	}
	if !strings.Contains(out.String(), "dry-run: nothing written") {
		t.Errorf("dry-run rows should say they wrote nothing:\n%s", out.String())
	}
	mustNotExist(t, env, "/etc/spoond/new.conf")
	mustNotExist(t, env, "/etc/spoond/new.env")
	if len(runner.calls) != 0 {
		t.Errorf("dry run ran %d commands: %v", len(runner.calls), runner.calls)
	}

	// A dry run over an existing file with the wanted content reports
	// kept without touching it; one with other content reports the
	// change it would make, also without touching it.
	env.DryRun = false
	if err := os.MkdirAll(env.Resolve("/etc/spoond"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.Resolve("/etc/spoond/same.conf"), []byte("same\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.Resolve("/etc/spoond/diff.conf"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env.DryRun = true
	out.Reset()
	Phases[fn] = phaseOf(fn,
		func(ctx context.Context, env *Env, r *Reporter) error {
			return env.writeFile(r, "/etc/spoond/same.conf", []byte("same\n"), 0o644)
		},
		func(ctx context.Context, env *Env, r *Reporter) error {
			return env.writeFile(r, "/etc/spoond/diff.conf", []byte("new\n"), 0o644)
		},
	)
	items, err = RunAll(context.Background(), env, fn)
	if err != nil {
		t.Fatalf("dry run over existing files: %v", err)
	}
	res = map[string]Result{}
	for _, it := range items {
		res[it.Name] = it.Result
	}
	if res["/etc/spoond/same.conf"] != Kept || res["/etc/spoond/diff.conf"] != Changed {
		t.Errorf("dry-run rows = %v, want kept for same.conf and changed for diff.conf", res)
	}
	if b, _ := read(t, env, "/etc/spoond/diff.conf"); string(b) != "old\n" {
		t.Errorf("dry run overwrote diff.conf: %q", b)
	}
}

// TestDryRunKeptSecretNotCreated: a dry run must not create a secret
// file either — writeFileKeep reports created but writes nothing.
func TestDryRunKeptSecretNotCreated(t *testing.T) {
	env, out, _ := testEnv(t)
	env.DryRun = true
	fn := PhaseCheck
	Phases[fn] = phaseOf(fn, func(ctx context.Context, env *Env, r *Reporter) error {
		return env.writeFileKeep(r, "/etc/spoond/token", []byte("TOKEN=secret\n"))
	})
	if _, err := RunAll(context.Background(), env, fn); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	mustNotExist(t, env, "/etc/spoond/token")
	if !strings.Contains(out.String(), "created") {
		t.Errorf("the row still names the planned write:\n%s", out.String())
	}
}

// TestDryRunCreatesNoDirectories: --dry-run changes nothing, not even
// the parent directories a real write would make — a dry run on a
// fresh root leaves it completely untouched.
func TestDryRunCreatesNoDirectories(t *testing.T) {
	env, out, _ := testEnv(t)
	env.DryRun = true
	fn := PhaseCheck
	Phases[fn] = phaseOf(fn,
		func(ctx context.Context, env *Env, r *Reporter) error {
			return env.writeFile(r, "/etc/spoond/deep/unit.conf", []byte("x"), 0o644)
		},
		func(ctx context.Context, env *Env, r *Reporter) error {
			return env.writeFileKeep(r, "/etc/spoond/deep/secret.env", []byte("TOKEN=t"))
		},
	)
	items, err := RunAll(context.Background(), env, fn)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("dry run produced %d rows, want 2", len(items))
	}
	mustNotExist(t, env, "/etc/spoond/deep/unit.conf")
	mustNotExist(t, env, "/etc/spoond/deep/secret.env")
	for _, dir := range []string{"/etc/spoond/deep", "/etc/spoond", "/etc"} {
		if _, err := os.Stat(env.Resolve(dir)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the dry run created the directory %s under the root (stat: %v)", dir, err)
		}
	}
	if !strings.Contains(out.String(), "dry-run: nothing written") {
		t.Errorf("the rows should say they wrote nothing:\n%s", out.String())
	}
}

// TestWriteFileCreatesMissingDirectories: the real (non-dry) write
// still creates the target's parent directories, so a phase can write
// /etc/spoond/... on a fresh root.
func TestWriteFileCreatesMissingDirectories(t *testing.T) {
	env, _, _ := testEnv(t)
	fn := PhaseCheck
	Phases[fn] = phaseOf(fn, func(ctx context.Context, env *Env, r *Reporter) error {
		return env.writeFile(r, "/etc/spoond/units.conf", []byte("v\n"), 0o644)
	})
	if _, err := RunAll(context.Background(), env, fn); err != nil {
		t.Fatalf("run: %v", err)
	}
	if b, _ := read(t, env, "/etc/spoond/units.conf"); string(b) != "v\n" {
		t.Errorf("content = %q, want %q", b, "v\n")
	}
}

// TestResolveMapsEveryPathUnderRoot: paths under a temp root land
// inside it, and with the root empty or / they stay production-clean.
func TestResolveMapsEveryPathUnderRoot(t *testing.T) {
	dir := t.TempDir()
	env := &Env{Root: dir}
	if got, want := env.Resolve("/etc/spoond/x"), filepath.Join(dir, "etc/spoond/x"); got != want {
		t.Errorf("Resolve under a temp root = %s, want %s", got, want)
	}
	env.Root = "/"
	if got := env.Resolve("/etc/spoond/x"); got != "/etc/spoond/x" {
		t.Errorf("Resolve under / = %s, want the path itself", got)
	}
	env.Root = ""
	if got := env.Resolve("/etc/spoond/x"); got != "/etc/spoond/x" {
		t.Errorf("Resolve with an empty root = %s, want the path itself", got)
	}
}

// TestRunAllRunsPhasesInOrderGiven: the names run in the argument
// order (the CLI passes Order for a whole-table run), each phase sees
// the same env, and the report rows carry the phase that wrote them.
func TestRunAllRunsPhasesInOrderGiven(t *testing.T) {
	env, out, _ := testEnv(t)
	base := Phases[PhaseCheck]
	t.Cleanup(func() { Phases[PhaseCheck] = base })
	var saw []string
	Phases[PhaseCheck] = func(ctx context.Context, env *Env) ([]Item, error) {
		saw = append(saw, PhaseCheck)
		r := &Reporter{Phase: PhaseCheck, Out: env.Out}
		r.Add(Kept, "check-item", "")
		return r.Items(), nil
	}
	defer func() { delete(Phases, PhaseCheck) }()
	Phases[PhaseHost] = func(ctx context.Context, env *Env) ([]Item, error) {
		saw = append(saw, PhaseHost)
		r := &Reporter{Phase: PhaseHost, Out: env.Out}
		r.Add(Created, "host-item", "")
		return r.Items(), nil
	}
	items, err := RunAll(context.Background(), env, PhaseHost, PhaseCheck)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if len(saw) != 2 || saw[0] != PhaseHost || saw[1] != PhaseCheck {
		t.Errorf("phases ran %v, want [host check]", saw)
	}
	if len(items) != 2 || items[0].Phase != PhaseHost || items[1].Phase != PhaseCheck {
		t.Errorf("rows = %+v, want host's row first with the right phase names", items)
	}
	if !strings.Contains(out.String(), "install host:") || !strings.Contains(out.String(), "install check:") {
		t.Errorf("output should head each phase with its name:\n%s", out.String())
	}
}

// TestRunAllStopsAtFirstFailure: a failing phase stops the table and
// the error names the phase; the later phases never ran.
func TestRunAllStopsAtFirstFailure(t *testing.T) {
	env, _, _ := testEnv(t)
	want := errors.New("nope: fix the frobnicator")
	Phases[PhaseCheck] = func(ctx context.Context, env *Env) ([]Item, error) {
		r := &Reporter{Phase: PhaseCheck}
		r.Add(Failed, "frobnicator", "fix the frobnicator")
		return r.Items(), want
	}
	defer func() { delete(Phases, PhaseCheck) }()
	ran := ""
	Phases[PhaseHost] = func(ctx context.Context, env *Env) ([]Item, error) {
		ran = PhaseHost
		return nil, nil
	}
	defer func() { delete(Phases, PhaseHost) }()
	items, err := RunAll(context.Background(), env) // full table, host is after check
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want the phase's error", err)
	}
	if !strings.Contains(err.Error(), "check") {
		t.Errorf("error %q does not name the phase", err)
	}
	if ran != "" {
		t.Error("the host phase ran after check failed")
	}
	if len(items) != 1 || items[0].Result != Failed {
		t.Errorf("rows = %+v, want the single failed row", items)
	}
}

// TestRunAllUnknownPhase: the error is ErrBadPhase and names the
// argument plus the valid phases; nothing runs.
func TestRunAllUnknownPhase(t *testing.T) {
	env, _, _ := testEnv(t)
	base := Phases[PhaseCheck]
	Phases[PhaseCheck] = func(ctx context.Context, env *Env) ([]Item, error) { t.Error("check ran"); return nil, nil }
	defer func() { Phases[PhaseCheck] = base }()
	_, err := RunAll(context.Background(), env, "nope")
	if !errors.Is(err, ErrBadPhase) {
		t.Fatalf("err = %v, want ErrBadPhase", err)
	}
	for _, want := range []string{"nope", "check", "doctor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestRunAllNotImplementedPhase: a not-yet-written phase errors with
// ErrNotImplemented and RunAll stops. The doctor phase is the last one
// still on its placeholder (spoond landed in S5, spoond-735v).
func TestRunAllNotImplementedPhase(t *testing.T) {
	env, _, _ := testEnv(t)
	_, err := RunAll(context.Background(), env, PhaseDoctor)
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("err = %v, want ErrNotImplemented", err)
	}
	if !strings.Contains(err.Error(), "not implemented yet") {
		t.Errorf("error %q should say plainly that the phase is not implemented", err)
	}
}

// TestCmdRunTrue shows the production runner: exit 0 gives the stdout
// and no error.
func TestCmdRunTrue(t *testing.T) {
	got, err := Cmd{}.Run(context.Background(), "sh", "-c", "printf hi-from-cmd")
	if err != nil {
		t.Fatalf("Run true: %v", err)
	}
	if string(got) != "hi-from-cmd" {
		t.Errorf("Run true stdout = %q, want %q", got, "hi-from-cmd")
	}
}

// TestCmdRunFalse: a non-zero exit is an error carrying the command
// and the captured stdout, not a lost output.
func TestCmdRunFalse(t *testing.T) {
	got, err := Cmd{}.Run(context.Background(), "sh", "-c", "echo cmd-said-this >&2; echo out-before-fail; exit 3")
	if err == nil {
		t.Fatal("Run false: want an error")
	}
	if !strings.Contains(err.Error(), "sh -c") {
		t.Errorf("error %q does not name the command", err)
	}
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("error %q does not carry the exit status", err)
	}
	if string(got) != "out-before-fail\n" {
		t.Errorf("stdout from a failed run = %q, want the captured bytes, not nil", got)
	}
}

// TestCmdRunCapturesStdoutStderrToProcess shows the split: the runner
// returns only stdout, stderr goes to the runner's own stderr so a
// failure still shows on the host console; the test points Stderr at
// a pipe instead of the test process's stderr.
func TestCmdRunCapturesStdoutStderrToProcess(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Cmd{Stderr: w}.Run(context.Background(), "sh", "-c", "echo through-stdout; echo through-stderr >&2")
	_ = w.Close()
	rest, _ := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(got) != "through-stdout\n" {
		t.Errorf("stdout = %q, want %q", got, "through-stdout\n")
	}
	if !strings.Contains(string(rest), "through-stderr") {
		t.Errorf("stderr didn't reach the process's stderr: %q", rest)
	}
}

// exec runs nothing in a dry run; query runs either way.
func TestExecSkipsInDryRunQueryRuns(t *testing.T) {
	env, _, runner := testEnv(t)
	runner.reply = map[string]fakeReply{
		"apt-get install -y x": {},
		"dpkg-query -W x":      {out: []byte("x 1.0")},
	}
	env.DryRun = true
	if err := env.exec(context.Background(), "apt-get", "install", "-y", "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.query(context.Background(), "dpkg-query", "-W", "x"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || runner.calls[0] != "dpkg-query -W x" {
		t.Fatalf("calls %q, want only the dpkg-query", runner.calls)
	}
	env.DryRun = false
	if err := env.exec(context.Background(), "apt-get", "install", "-y", "x"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || runner.calls[1] != "apt-get install -y x" {
		t.Fatalf("calls %q, want the apt-get to run", runner.calls)
	}
}
