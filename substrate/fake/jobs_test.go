package fake

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/substrate"
)

// TestJobRunnerRunsWrapper runs the real background-job wrapper through
// Start: the fake executes it for real, writing stdout/stderr/rc files
// into the runner directory and reporting the exit on the stream.
func TestJobRunnerRunsWrapper(t *testing.T) {
	f := New()
	dir := t.TempDir()
	f.EnableJobRunner(dir)
	t.Cleanup(f.StopAllJobs)

	const sandboxID = "i0123456789abcdefghij"
	if _, err := f.Create(t.Context(), substrate.CreateRequest{SandboxID: sandboxID}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A small stand-in wrapper with the same argv shape and file layout.
	const script = `job_id="$1"; shift
jobs="${SPOOND_JOBS_DIR:-/var/lib/spoond/jobs}"
job_dir="$jobs/$job_id"
mkdir -p "$job_dir" || exit 1
setsid "$@" >"$job_dir/stdout" 2>"$job_dir/stderr" </dev/null &
child=$!
printf '%d\n' "$child" >"$job_dir/pid"
finalize() { rc=$?; if [ ! -f "$job_dir/rc" ]; then printf '%d\n' "$rc" >"$job_dir/rc.tmp" && mv "$job_dir/rc.tmp" "$job_dir/rc"; fi; exit "$rc"; }
trap finalize EXIT
wait "$child"
exit $?`

	req := substrate.StartRequest{
		Args: []string{"/bin/bash", "-c", script, "spoond-job", "job-1", "/bin/bash", "-c", "echo hi; echo boom >&2; exit 5"},
		Env:  map[string]string{"SPOOND_JOBS_DIR": filepath.Join(dir, "jobs")},
	}
	proc, err := f.Start(t.Context(), sandboxID, req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Drain the stream until the exit.
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-proc.Events():
			if !ok {
				t.Fatal("events channel closed without an exit")
			}
			if ev.Kind == substrate.EventExit {
				if ev.ExitCode != 5 {
					t.Fatalf("exit code = %d, want 5", ev.ExitCode)
				}
				goto done
			}
		case <-deadline:
			t.Fatal("no exit event")
		}
	}
done:
	rc, stdout, stderr, err := f.JobState("job-1")
	if err != nil {
		t.Fatalf("JobState: %v", err)
	}
	if strings.TrimSpace(rc) != "5" || stdout != "hi\n" || stderr != "boom\n" {
		t.Fatalf("job files: rc=%q stdout=%q stderr=%q", rc, stdout, stderr)
	}
	if !f.JobDone("job-1") {
		t.Fatalf("JobDone = false after exit")
	}
	if _, ok := f.JobPID("job-1"); !ok {
		t.Fatalf("JobPID missing")
	}

	// A production-path read is redirected to the runner directory.
	data, err := f.ReadFile(context.Background(), sandboxID, "/var/lib/spoond/jobs/job-1/stdout", 1024)
	if err != nil || string(data) != "hi\n" {
		t.Fatalf("redirected ReadFile = %q %v", data, err)
	}
	// And an oversized prefix read still reports ErrTooLarge.
	if _, err := f.ReadFile(context.Background(), sandboxID, "/var/lib/spoond/jobs/job-1/stdout", 1); err == nil {
		t.Fatalf("oversized ReadFile did not error")
	}
	// Remove on the production path deletes the runner directory.
	if err := f.Remove(context.Background(), sandboxID, "/var/lib/spoond/jobs/job-1", true); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "jobs", "job-1")); !os.IsNotExist(err) {
		t.Fatalf("job dir not removed: %v", err)
	}
}

// TestJobRunnerDisabledByDefault: without EnableJobRunner, Start is the
// inert process factory and the production job paths stay in the
// in-memory filesystem.
func TestJobRunnerDisabledByDefault(t *testing.T) {
	f := New()
	const sandboxID = "i0123456789abcdefghij"
	if _, err := f.Create(t.Context(), substrate.CreateRequest{SandboxID: sandboxID}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.Start(t.Context(), sandboxID, substrate.StartRequest{Args: []string{"/bin/echo"}}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := f.WriteFile(t.Context(), sandboxID, "/var/lib/spoond/jobs/x/stdout", []byte("mem"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if data, err := f.ReadFile(t.Context(), sandboxID, "/var/lib/spoond/jobs/x/stdout", 16); err != nil || string(data) != "mem" {
		t.Fatalf("in-memory job file = %q %v", data, err)
	}
	if f.JobDone("x") {
		t.Fatalf("JobDone true without a runner")
	}
}
