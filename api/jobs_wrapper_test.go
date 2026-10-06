package api

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/substrate"
)

// These tests run the real guest wrapper through the fake's job runner:
// the wrapper script executes for real, its setsid child runs, the rc
// file is written rc.tmp-then-rename, the secrets list is removed and
// stdout/stderr are captured. They complement jobs_test.go, which scripts
// exits by hand to pin the backend's timing.

// jobHostPath maps a job file to the runner's host directory.
func jobHostPath(dir, jobID, name string) string {
	return filepath.Join(dir, "jobs", jobID, name)
}

// TestBackgroundExecRealWrapper: the real wrapper starts the command,
// records stdout/stderr and an atomically-written rc, and the stream
// notices the exit.
func TestBackgroundExecRealWrapper(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	dir := useJobRunner(t, sub)
	id, _, _ := createJobLease(t, ts, svc)

	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sh -c 'echo out; echo err >&2; exit 7'"})
	row := waitJobState(t, db, jobID, "exited", 5*time.Second)
	if row.ExitCode == nil || *row.ExitCode != 7 {
		t.Fatalf("exit code = %v, want 7", row.ExitCode)
	}
	if !strings.Contains(row.StderrTail, "err") {
		t.Fatalf("stderr tail = %q, want err", row.StderrTail)
	}
	rc, stdout, stderr, err := sub.Fake.JobState(jobID)
	if err != nil {
		t.Fatalf("job state: %v", err)
	}
	if strings.TrimSpace(rc) != "7" {
		t.Fatalf("guest rc = %q, want 7", rc)
	}
	if stdout != "out\n" || stderr != "err\n" {
		t.Fatalf("guest output = stdout %q stderr %q", stdout, stderr)
	}
	// The atomic write leaves no rc.tmp behind.
	if _, err := os.Stat(jobHostPath(dir, jobID, "rc.tmp")); !os.IsNotExist(err) {
		t.Fatalf("rc.tmp left behind: %v", err)
	}
	// The pid file was written before the command was reaped.
	if _, ok := sub.Fake.JobPID(jobID); !ok {
		t.Fatalf("no pid recorded")
	}

	// The record view reads the output from the guest files.
	_, body := doReq(t, "GET", ts.URL+"/api/sandboxes/"+id+"/jobs/"+jobID, "token-a", nil)
	if body["stdout"] != "out\n" || body["stderr"] != "err\n" {
		t.Fatalf("record view = stdout %q stderr %q", body["stdout"], body["stderr"])
	}
	_ = svc
}

// TestBackgroundExecOverLimitStderr: stderr larger than the 4 KiB tail is
// still recorded (the read must be a true tail, not an oversized prefix
// read).
func TestBackgroundExecOverLimitStderr(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	useJobRunner(t, sub)
	id, _, _ := createJobLease(t, ts, svc)

	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{
		"cmd": "head -c 6000 /dev/zero | tr '\\0' 'x' >&2; exit 3",
	})
	row := waitJobState(t, db, jobID, "exited", 5*time.Second)
	if row.ExitCode == nil || *row.ExitCode != 3 {
		t.Fatalf("exit code = %v, want 3", row.ExitCode)
	}
	if len(row.StderrTail) != jobStderrTailBytes {
		t.Fatalf("stderr tail = %d bytes, want %d", len(row.StderrTail), jobStderrTailBytes)
	}
	if strings.Trim(row.StderrTail, "x") != "" {
		t.Fatalf("stderr tail has non-x bytes: %q", row.StderrTail)
	}
}

// TestBackgroundExecRealWrapperSecrets: the real wrapper removes the
// job's staged secrets at exit and they never reach the record.
func TestBackgroundExecRealWrapperSecrets(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	dir := useJobRunner(t, sub)
	id, _, _ := createJobLease(t, ts, svc)

	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{
		"cmd":     "sleep 0.2",
		"secrets": map[string]string{"TOKEN": "super-secret-value"},
	})
	if data, err := os.ReadFile(filepath.Join(dir, "secrets", "TOKEN")); err != nil || string(data) != "super-secret-value" {
		t.Fatalf("secret not staged: %q %v", data, err)
	}
	row := waitJobState(t, db, jobID, "exited", 5*time.Second)
	if strings.Contains(row.Cmd, "super-secret-value") || strings.Contains(row.StderrTail, "super-secret-value") {
		t.Fatalf("secret value leaked into record: %+v", row)
	}
	// The wrapper removed the staged file at exit.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "secrets", "TOKEN")); os.IsNotExist(err) {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("secret still staged after exit")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestBackgroundExecRealWrapperSignal: a running job's process group is
// signalled through the API and the real wrapper records the exit.
func TestBackgroundExecRealWrapperSignal(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	useJobRunner(t, sub)
	installJobKillExec(t, sub)
	id, _, _ := createJobLease(t, ts, svc)

	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})
	// The 202 may beat the wrapper's pid write; signalJob waits briefly.
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/jobs/"+jobID+"/signal", "token-a", map[string]any{"signal": "TERM"})
	if resp.StatusCode != 200 {
		t.Fatalf("signal status %d: %v", resp.StatusCode, body)
	}
	row := waitJobState(t, db, jobID, "exited", 10*time.Second)
	if row.ExitCode == nil || *row.ExitCode != 143 {
		t.Fatalf("exit code = %v, want 143", row.ExitCode)
	}
}

// installJobKillExec runs the backend's process-group kill against the
// fake's real wrapper jobs: it parses the fixed kill argv and delivers
// the signal to the signed pid, the way the guest /bin/kill would.
func installJobKillExec(t *testing.T, sub *testSub) {
	t.Helper()
	sub.SetExecHandler(func(sandboxID string, args []string) substrate.ExecResult {
		if len(args) >= 6 && args[0] == "/bin/sh" && strings.HasPrefix(args[2], "kill ") {
			sig := args[4]
			pid, err := strconv.Atoi(args[5])
			if err != nil {
				return substrate.ExecResult{Stderr: "bad pid", ExitCode: 2}
			}
			kill := sig == "KILL"
			if err := sub.Fake.KillProcessGroup(pid, kill); err != nil {
				return substrate.ExecResult{Stderr: err.Error(), ExitCode: 1}
			}
			return substrate.ExecResult{ExitCode: 0}
		}
		return sub.exec(sandboxID, args)
	})
	t.Cleanup(func() { sub.SetExecHandler(nil) })
}

// TestBackgroundExecRealWrapperCwd: cwd is applied by the wrapper.
func TestBackgroundExecRealWrapperCwd(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	useJobRunner(t, sub)
	id, _, _ := createJobLease(t, ts, svc)

	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "pwd", "cwd": "/tmp"})
	waitJobState(t, db, jobID, "exited", 5*time.Second)
	_, stdout, _, err := sub.Fake.JobState(jobID)
	if err != nil {
		t.Fatalf("job state: %v", err)
	}
	if strings.TrimSpace(stdout) != "/tmp" {
		t.Fatalf("pwd = %q, want /tmp", stdout)
	}
}

// TestBackgroundExecRealWrapperEnv: env values reach the command and are
// not stored in the record or in a guest file.
func TestBackgroundExecRealWrapperEnv(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	dir := useJobRunner(t, sub)
	id, _, _ := createJobLease(t, ts, svc)

	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{
		"cmd": "printf '%s' \"$GREETING\"",
		"env": map[string]string{"GREETING": "env-secret-value"},
	})
	row := waitJobState(t, db, jobID, "exited", 5*time.Second)
	if strings.Contains(row.Cmd, "env-secret-value") {
		t.Fatalf("env value leaked into cmd: %q", row.Cmd)
	}
	_, stdout, _, err := sub.Fake.JobState(jobID)
	if err != nil {
		t.Fatalf("job state: %v", err)
	}
	if stdout != "env-secret-value" {
		t.Fatalf("stdout = %q, want the env value", stdout)
	}
	// No env file is written into the job directory.
	entries, err := os.ReadDir(filepath.Join(dir, "jobs", jobID))
	if err != nil {
		t.Fatalf("read job dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "env" {
			t.Fatalf("job directory contains an env file")
		}
	}
}

// TestBackgroundExecRealWrapperCleanupOnPrune: a pruned record's guest
// files go with it.
func TestBackgroundExecRealWrapperCleanupOnPrune(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	dir := useJobRunner(t, sub)
	svc.cfg.JobRetentionSecs = 1
	id, _, _ := createJobLease(t, ts, svc)

	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "echo bye"})
	waitJobState(t, db, jobID, "exited", 5*time.Second)

	ctx := context.Background()
	// Move the service clock past the retention window so the exited
	// record is pruned, then its guest files must go with it.
	svc.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	svc.pruneJobs(ctx)
	if _, err := db.GetJob(ctx, jobID); err == nil {
		t.Fatalf("record not pruned")
	}
	if _, err := os.Stat(jobHostPath(dir, jobID, "stdout")); !os.IsNotExist(err) {
		t.Fatalf("guest job files not cleaned up: %v", err)
	}
}

var _ substrate.Substrate = (*testSub)(nil)
