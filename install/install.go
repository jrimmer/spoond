// Package install is the `spoond install` framework: ordered phases run
// against a root prefix with one command runner and one report, so a
// person on a fresh host gets a working spoond and can run the command
// again safely.
//
// Rules for every phase (docs/plans/2026-10-16-4.0-install.md, I):
//
//   - Idempotent: compare before write, report created/changed/kept.
//   - --dry-run prints the plan and changes nothing.
//   - Never overwrite a secret or env file that exists: report kept.
//   - Never print a secret: the report names the file, not the value.
//   - Every path is under Env.Root (/ in production).
//   - Commands go through Env.Run so tests fake them.
//   - Stop at the first failure with what to do next.
package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// Env is the phase environment: everything a phase touches goes through
// it, so tests fill in a temp Root, a fake Runner and their own Out.
type Env struct {
	// Root is the path every file and every command-cwd resolves
	// under; "/" in production, a t.TempDir() in tests.
	Root string
	// Run executes host commands (apt, modprobe, sysctl, zfs,
	// systemctl, docker); nil phases are read-only and do not call it.
	Run Runner
	// Out receives the progress report. Secrets never reach it.
	Out io.Writer
	// DryRun stops every write and every command that changes the
	// host (exec); read-only queries still run. The report shows what
	// a real run would do.
	DryRun bool
	// Version is the spoond binary's own version ("v4.0.0"); empty
	// for a development build. The runtime phase downloads the
	// release's spoond-runtime tarball that matches it.
	Version string
	// Opt holds the install flags; each phase reads its own fields.
	Opt Options
	// Prober, Spacer and Arch stand in for the host in the check
	// phase: the port probe, the free-space probe and the binary's
	// own architecture. Nil and empty mean the real host; tests set
	// them so a check run never binds a port or reads the real
	// filesystem.
	Prober Prober
	Spacer Spacer
	Arch   string
}

// prober returns env.Prober, or the production probe when unset.
func (env *Env) prober() Prober {
	if env.Prober != nil {
		return env.Prober
	}
	return sysProber{}
}

// spacer returns env.Spacer, or the production probe when unset.
func (env *Env) spacer() Spacer {
	if env.Spacer != nil {
		return env.Spacer
	}
	return sysSpacer{}
}

// arch returns env.Arch, or the binary's own target when unset: the
// check phase's amd64 answer.
func (env *Env) arch() string {
	if env.Arch != "" {
		return env.Arch
	}
	return runtime.GOARCH
}

// Runner runs one command and returns its stdout. Tests fake it; the
// production runner is os/exec.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Cmd is the Runner over os/exec: stdout is captured, stderr goes to
// Stderr (os.Stderr when unset), and a non-zero exit is an error
// carrying the captured output so the failure names what went wrong.
type Cmd struct {
	// Stderr receives command stderr; nil means os.Stderr, so a
	// failed apt or systemctl shows on the host console.
	Stderr io.Writer
}

// Run implements Runner with os/exec, capturing stdout and letting
// stderr through so failures show on the host console.
func (c Cmd) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = c.stderr()
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%s %s: %w", name, joinArgs(args), err)
	}
	return stdout.Bytes(), nil
}

// stderr returns the writer command stderr goes to.
func (c Cmd) stderr() io.Writer {
	if c.Stderr != nil {
		return c.Stderr
	}
	return os.Stderr
}

// joinArgs renders args for error text; it only carries command
// arguments (flags, paths), never generated secret values.
func joinArgs(args []string) string {
	buf := new(bytes.Buffer)
	for i, a := range args {
		if i > 0 {
			buf.WriteByte(' ')
		}
		buf.WriteString(a)
	}
	return buf.String()
}

// query runs a read-only command (dpkg-query, zpool list, docker
// inspect, systemctl is-active). It runs in a dry run too: it changes
// nothing, and the plan needs its answer.
func (env *Env) query(ctx context.Context, name string, args ...string) ([]byte, error) {
	return env.Run.Run(ctx, name, args...)
}

// exec runs a command that changes the host (apt-get install, modprobe,
// zpool create, systemctl enable). In a dry run it runs nothing and
// returns nil; the caller reports the row either way.
func (env *Env) exec(ctx context.Context, name string, args ...string) error {
	if env.DryRun {
		return nil
	}
	_, err := env.Run.Run(ctx, name, args...)
	return err
}

// Result is what a phase did to one item.
type Result string

const (
	// Created: the file did not exist and was written.
	Created Result = "created"
	// Changed: the file existed with other content and was replaced
	// (secrets and env files never take this path — writeFileKeep
	// reports kept instead).
	Changed Result = "changed"
	// Kept: the file exists and was left alone (writeKeep, or an
	// exact content match).
	Kept Result = "kept"
	// Skipped: the whole item was skipped (a dependency the phase
	// does not manage, or --dry-run on something it only inspects).
	Skipped Result = "skipped"
	// Failed: the item failed; the phase error says what to do next.
	Failed Result = "failed"
)

// Item is one row of the phase report: what was touched and how.
type Item struct {
	// Phase is the phase that produced the row.
	Phase string
	// Name is the row subject: a path under Root for a file, a
	// port like ":8890" or a check name for a probe.
	Name string
	// Result is what happened to it.
	Result Result
	// Note adds one line for the operator (a warning, a fix, or
	// which file a secret went to). Never a secret value.
	Note string
}

// Reporter collects a phase's rows and can print them to Env.Out.
// A phase creates one over env.Out (RunPhase's does), and the Env's
// write helpers add every row to it — including the kept rows, so a
// rerun shows the whole inventory.
type Reporter struct {
	Phase string
	// Out is where rows print as they happen; nil for quiet runs
	// (tests that only inspect the returned rows).
	Out io.Writer
	// items is the ordered report.
	items []Item
}

// Add records a row and prints it now, so a long install shows progress.
func (r *Reporter) Add(res Result, name, note string) Item {
	it := Item{Phase: r.Phase, Name: name, Result: res, Note: note}
	r.items = append(r.items, it)
	if r.Out != nil {
		fmt.Fprintf(r.Out, "  %-8s %s", res, name)
		if note != "" {
			fmt.Fprintf(r.Out, " — %s", note)
		}
		fmt.Fprintln(r.Out)
	}
	return it
}

// Items returns the collected rows.
func (r *Reporter) Items() []Item { return r.items }

// Phase is one install phase: it fills the report and returns the
// first failure, with the fix in the message.
type Phase func(ctx context.Context, env *Env) ([]Item, error)

// Resolve turns a production path (/etc/spoond/...) into one under
// the root prefix: filepath.Join(Root, path). With Root="/" it
// returns the path itself, so production paths stay clean.
func (env *Env) Resolve(path string) string {
	if env.Root == "" || env.Root == "/" {
		return path
	}
	return filepath.Join(env.Root, path)
}

// writeFile writes path with content, mode 0600 unless mode says
// otherwise, comparing before it writes so an unchanged file with the
// right mode is a kept, and honouring DryRun (dry-run reports created or changed but
// writes nothing).
// DryRun before MkdirAll: a dry run changes nothing, not even the
// directories under the target.
func (env *Env) writeFile(r *Reporter, path string, content []byte, mode os.FileMode) error {
	full := env.Resolve(path)
	dir := filepath.Dir(full)
	if mode == 0 {
		mode = 0o600
	}
	res := Created
	if old, err := os.ReadFile(full); err == nil {
		res = Changed
		if bytes.Equal(old, content) {
			fi, err := os.Stat(full)
			if err != nil {
				return fmt.Errorf("stat %s: %w", full, err)
			}
			if fi.Mode().Perm() == mode.Perm() {
				r.Add(Kept, path, "")
				return nil
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", full, err)
	}
	if env.DryRun {
		r.Add(res, path, "dry-run: nothing written")
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := writeAtomic(full, content, mode); err != nil {
		return err
	}
	r.Add(res, path, "")
	return nil
}

// writeAtomic replaces full with content at mode through a temporary
// file in the same directory and a rename, so a running binary (the
// backend's own /opt/spoond/spoond, the orchestrator) is replaced
// rather than written into ("text file busy"), and a reader never sees
// half a file.
func writeAtomic(full string, content []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(full), "."+filepath.Base(full)+".new-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", full, err)
	}
	name := tmp.Name()
	_, werr := tmp.Write(content)
	if werr == nil {
		werr = tmp.Chmod(mode)
	}
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(name, full)
	}
	if werr != nil {
		os.Remove(name)
		return fmt.Errorf("write %s: %w", full, werr)
	}
	return nil
}

// writeFileKeep is writeFile for secrets and env files: it never
// overwrites an existing file. An existing file is reported kept —
// its content is never compared or printed.
func (env *Env) writeFileKeep(r *Reporter, path string, content []byte) error {
	full := env.Resolve(path)
	dir := filepath.Dir(full)
	if _, err := os.Stat(full); err == nil {
		r.Add(Kept, path, "exists, left untouched")
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", full, err)
	}
	if env.DryRun {
		r.Add(Created, path, "dry-run: nothing written")
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.WriteFile(full, content, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", full, err)
	}
	r.Add(Created, path, "")
	return nil
}

// RunPhase runs one phase against env, prefixed with its name in the
// report output: "install check:" then the rows. The returned error is
// the phase's error prefixed once with the name — phases return bare
// errors, the fix text without a prefix of their own.
func RunPhase(ctx context.Context, env *Env, name string, fn Phase) ([]Item, error) {
	if env.Out != nil {
		if env.DryRun {
			fmt.Fprintf(env.Out, "install %s (dry run):\n", name)
		} else {
			fmt.Fprintf(env.Out, "install %s:\n", name)
		}
	}
	items, err := fn(ctx, env)
	if err != nil {
		return items, fmt.Errorf("%s: %w", name, err)
	}
	return items, nil
}
