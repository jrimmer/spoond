package api

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/substrate"
	"github.com/jrimmer/spoond/v2/substrate/fake"
)

// This file covers the max-runtime follow-ups (spoond-wb5): a kill whose
// record write fails must be retried without a second kill, a job whose
// pid file never appears must back off instead of spending the pid wait
// on every pass, a synchronous exec must reject a negative
// max_runtime_secs, and the failed write is reported through storeError.

// TestFinishJobTimedOutLeavesIntentOnStoreError: when MarkJobTimedOut
// fails, the in-memory timed-out intent stays set so a later pass can
// record the outcome without signalling an already-gone process
// (spoond-wb5 follow-up 1).
func TestFinishJobTimedOutLeavesIntentOnStoreError(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)

	p := fake.NewProcess(1101)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep infinity"})
	writeJobFile(t, sub, sandbox, jobID, "pid", "4242\n")
	row, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	// A canceled context makes ExecContext fail while the reader pool is
	// still usable, so the record stays readable.
	svc.markJobTimingOut(jobID)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.finishJobTimedOut(canceled, row, 124, "", sandbox, row.Generation); err == nil {
		t.Fatalf("finishJobTimedOut with a canceled context = nil, want an error")
	}
	still, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if still.State != "running" {
		t.Fatalf("job after a failed mark = %q, want running", still.State)
	}
	if !svc.jobIsTimingOut(jobID) {
		t.Fatalf("timed-out intent was cleared after a failed mark")
	}

	// The next pass records the outcome and clears the intent.
	if err := svc.finishJobTimedOut(context.Background(), row, 124, "", sandbox, row.Generation); err != nil {
		t.Fatalf("finishJobTimedOut retry: %v", err)
	}
	got := waitJobState(t, db, jobID, "exited", 2*time.Second)
	if got.Reason != "timed_out" {
		t.Fatalf("reason = %q, want timed_out", got.Reason)
	}
	if svc.jobIsTimingOut(jobID) {
		t.Fatalf("timed-out intent not cleared after the record closed")
	}
}

// TestKillJobTimeoutRecordsKeptIntentWithoutSecondKill: once a kill has
// succeeded but its mark failed, the next pass records timed_out without
// signalling the gone process again (spoond-wb5 follow-up 1).
func TestKillJobTimeoutRecordsKeptIntentWithoutSecondKill(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)

	p := fake.NewProcess(1102)
	installJobProcess(t, sub, p)
	var kills atomic.Int32
	sub.SetExecHandler(func(sandboxID string, req substrate.ExecRequest) substrate.ExecResult {
		if isJobKillReq(req) {
			kills.Add(1)
		}
		return sub.exec(sandboxID, req)
	})
	t.Cleanup(func() { sub.SetExecHandler(nil) })

	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep infinity"})
	writeJobFile(t, sub, sandbox, jobID, "pid", "4242\n")
	row, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	// The previous pass killed the process and kept the intent.
	svc.markJobTimingOut(jobID)
	if !svc.killJobTimeout(context.Background(), row, sandbox, row.Generation) {
		t.Fatalf("killJobTimeout = false, want true (the intent says it is already killed)")
	}
	if kills.Load() != 0 {
		t.Fatalf("kill execs = %d, want 0 (no second kill)", kills.Load())
	}
	got := waitJobState(t, db, jobID, "exited", 2*time.Second)
	if got.Reason != "timed_out" {
		t.Fatalf("reason = %q, want timed_out", got.Reason)
	}
	if got.ExitCode == nil || *got.ExitCode != 124 {
		t.Fatalf("exit code = %v, want 124", got.ExitCode)
	}
}

// TestKillJobTimeoutStoreErrorLoggedAndIntentKept: a successful kill whose
// mark fails is reported through storeError and keeps the intent, so the
// next pass retries the mark rather than a kill (spoond-wb5 follow-up 1).
func TestKillJobTimeoutStoreErrorLoggedAndIntentKept(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)

	p := fake.NewProcess(1103)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep infinity"})
	writeJobFile(t, sub, sandbox, jobID, "pid", "4242\n")
	row, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	var logs bytes.Buffer
	svc.log = log.New(&logs, "", 0)
	svc.markJobTimingOut(jobID)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if !svc.killJobTimeout(canceled, row, sandbox, row.Generation) {
		t.Fatalf("killJobTimeout = false, want true")
	}
	if !strings.Contains(logs.String(), "mark_job_timed_out") {
		t.Fatalf("store error not logged: %q", logs.String())
	}
	if !svc.jobIsTimingOut(jobID) {
		t.Fatalf("timed-out intent was cleared after a failed mark")
	}
	still, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if still.State != "running" {
		t.Fatalf("job after a failed mark = %q, want running", still.State)
	}
}

// TestJobMaxRuntimePidMissingBacksOff: a job whose pid file never appears
// must not spend jobPID's wait on every reconcile pass. The first attempt
// defers the next kill, a pass inside the backoff does not re-read the
// pid file, and a pass past the backoff tries again (spoond-wb5 follow-up
// 4).
func TestJobMaxRuntimePidMissingBacksOff(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, _ := createJobLease(t, ts, svc)
	svc.cfg.JobMaxRuntimeSecs = 3600

	p := fake.NewProcess(1104)
	installJobProcess(t, sub, p)

	oldWait := jobPIDWait
	jobPIDWait = 10 * time.Millisecond
	t.Cleanup(func() { jobPIDWait = oldWait })

	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep infinity"})
	// No pid file is ever written.

	base := time.Now()
	svc.now = func() time.Time { return base.Add(2 * time.Hour) }
	t.Cleanup(func() { svc.now = time.Now })

	// First pass: the pid file is missing, so the kill fails and the next
	// attempt is deferred.
	svc.reconcileJobs(context.Background())
	if svc.jobKillDue(jobID) {
		t.Fatalf("jobKillDue = true right after a missing pid, want false")
	}
	if svc.jobIsTimingOut(jobID) {
		t.Fatalf("timed-out intent was left set after a failed kill")
	}

	// A pass inside the backoff does not read the pid again: the only
	// ReadFile is the rc check.
	before := readFileCalls(sub)
	svc.reconcileJobs(context.Background())
	if n := readFileCalls(sub) - before; n != 1 {
		t.Fatalf("ReadFile calls inside the backoff = %d, want 1 (rc only)", n)
	}

	// Past the backoff the kill is tried again (and the pid is still
	// missing, so it fails once more).
	svc.now = func() time.Time { return base.Add(2*time.Hour + jobKillPidBackoff + time.Second) }
	if !svc.jobKillDue(jobID) {
		t.Fatalf("jobKillDue = false past the backoff, want true")
	}
	before = readFileCalls(sub)
	svc.reconcileJobs(context.Background())
	if n := readFileCalls(sub) - before; n < 2 {
		t.Fatalf("ReadFile calls past the backoff = %d, want the pid read plus rc", n)
	}

	row, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if row.State != "running" {
		t.Fatalf("job after a missing pid = %q, want running", row.State)
	}
}

// TestJobSynchronousExecRejectsNegativeRuntime: a synchronous exec with a
// negative max_runtime_secs is refused rather than accepted and ignored,
// so a client cannot think it changed the exec's timeout (spoond-wb5
// follow-up 4). A non-negative value on a synchronous exec is still
// ignored and the exec succeeds.
func TestJobSynchronousExecRejectsNegativeRuntime(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	id, _, _ := createJobLease(t, ts, svc)

	p := fake.NewProcess(1105)
	installJobProcess(t, sub, p)

	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/exec", "token-a", map[string]any{
		"cmd": "echo hi", "max_runtime_secs": -1,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("synchronous exec with negative max_runtime_secs = %d, want 400 (%v)", resp.StatusCode, body)
	}

	resp, body = doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/exec", "token-a", map[string]any{
		"cmd": "echo hi", "max_runtime_secs": 60,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("synchronous exec with non-negative max_runtime_secs = %d, want 200 (%v)", resp.StatusCode, body)
	}
}

// readFileCalls counts the fake's ReadFile calls (not ReadFileRange), so a
// test can tell whether jobPID read the pid file in a reconcile pass.
func readFileCalls(sub *testSub) int {
	n := 0
	for _, c := range sub.Fake.CallLog() {
		if strings.HasPrefix(c, "ReadFile ") {
			n++
		}
	}
	return n
}

// isJobKillReq reports whether req is the backend's process-group kill
// exec (the fixed /bin/sh -c 'kill -"$1" -"$2"' argv).
func isJobKillReq(req substrate.ExecRequest) bool {
	return len(req.Args) >= 6 && req.Args[0] == "/bin/sh" && strings.HasPrefix(req.Args[2], "kill ")
}
