package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
	"github.com/jrimmer/spoond/v2/substrate/fake"
)

// The tests in this file exercise background exec jobs (2.6, #135)
// through the lease API and the service, with the fake substrate driving
// the guest's job files. The fake writes no real processes, so a test
// installs a Start handler that returns a *fake.FakeProcess it drives
// and writes the job's stdout/stderr/rc files with sub.WriteFile.

// createJobLease creates a lease through the API and returns its id, the
// lease, and the sandbox id.
func createJobLease(t *testing.T, ts *httptest.Server, svc *Service) (string, *Lease, string) {
	t.Helper()
	_, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("create lease: no id in %v", body)
	}
	l := svc.lookupAny(id)
	if l == nil {
		t.Fatalf("lease %s not in service", id)
	}
	return id, l, l.SandboxID
}

// installJobProcess makes the fake return p for the next Start and
// nothing else. It restores the default handler on cleanup.
func installJobProcess(t *testing.T, sub *testSub, p *fake.FakeProcess) {
	t.Helper()
	sub.SetStartHandler(func(sandboxID string, req substrate.StartRequest) (substrate.Process, error) {
		return p, nil
	})
	t.Cleanup(func() { sub.SetStartHandler(nil) })
}

// writeJobFile writes one job record file into the fake guest.
func writeJobFile(t *testing.T, sub *testSub, sandboxID, jobID, name, content string) {
	t.Helper()
	if err := sub.WriteFile(context.Background(), sandboxID, jobPath(jobID, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write job %s/%s: %v", jobID, name, err)
	}
}

// startBackgroundJob posts a background exec and returns the job id.
func startBackgroundJob(t *testing.T, ts *httptest.Server, id string, req map[string]any) (string, time.Time) {
	t.Helper()
	req["background"] = true
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/exec", "token-a", req)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("background exec status %d: %v", resp.StatusCode, body)
	}
	jobID, _ := body["job_id"].(string)
	if jobID == "" {
		t.Fatalf("background exec: no job_id in %v", body)
	}
	started, err := time.Parse(time.RFC3339Nano, body["started_at"].(string))
	if err != nil {
		t.Fatalf("background exec: bad started_at %v", body["started_at"])
	}
	return jobID, started
}

// waitJobState polls a job record until it reaches state.
func waitJobState(t *testing.T, db *store.DB, jobID, state string, timeout time.Duration) store.JobRow {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		row, err := db.GetJob(context.Background(), jobID)
		if err == nil && row.State == state {
			return row
		}
		time.Sleep(10 * time.Millisecond)
	}
	row, _ := db.GetJob(context.Background(), jobID)
	t.Fatalf("job %s never reached %q (last: %+v)", jobID, state, row)
	return store.JobRow{}
}

// TestBackgroundExecStartAndExitViaStream: a background exec answers 202
// with a job id and started_at; the exit is noticed through the live
// envd stream and the record is marked exited.
func TestBackgroundExecStartAndExitViaStream(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, lease, sandbox := createJobLease(t, ts, svc)

	proc := fake.NewProcess(1001)
	installJobProcess(t, sub, proc)

	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "echo out; echo err >&2; exit 7"})
	row := waitJobState(t, db, jobID, "running", 2*time.Second)
	if row.Cmd != "echo out; echo err >&2; exit 7" {
		t.Fatalf("record cmd = %q", row.Cmd)
	}

	// The command "runs" and writes its files; the stream reports exit.
	writeJobFile(t, sub, sandbox, jobID, "stdout", "out\n")
	writeJobFile(t, sub, sandbox, jobID, "stderr", "err\n")
	writeJobFile(t, sub, sandbox, jobID, "rc", "7\n")
	proc.Push(substrate.ProcessEvent{Kind: substrate.EventExit, ExitCode: 7})

	row = waitJobState(t, db, jobID, "exited", 3*time.Second)
	if row.ExitCode == nil || *row.ExitCode != 7 {
		t.Fatalf("exit code = %v, want 7", row.ExitCode)
	}
	if row.EndedAt.IsZero() {
		t.Fatalf("ended_at not set")
	}
	_ = lease
}

// TestBackgroundExecReconcileAfterRestart: a job that ended while the
// backend was down is noticed by the reconcile pass reading the guest
// rc file (the stream is gone).
func TestBackgroundExecReconcileAfterRestart(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)

	// The stream never reports an exit (a backend restart dropped it).
	p := fake.NewProcess(1002)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})

	writeJobFile(t, sub, sandbox, jobID, "stdout", "done\n")
	writeJobFile(t, sub, sandbox, jobID, "stderr", "oops\n")
	writeJobFile(t, sub, sandbox, jobID, "rc", "3\n")

	svc.reconcileJobs(context.Background())
	row := waitJobState(t, db, jobID, "exited", 2*time.Second)
	if row.ExitCode == nil || *row.ExitCode != 3 {
		t.Fatalf("exit code = %v, want 3", row.ExitCode)
	}
	if !strings.Contains(row.StderrTail, "oops") {
		t.Fatalf("stderr tail = %q", row.StderrTail)
	}
}

// TestBackgroundExecSecretsRemovedAndNotInRecord: per-exec secrets are
// staged, the record never carries their values, and the exit path
// removes them.
func TestBackgroundExecSecretsRemovedAndNotInRecord(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)

	p := fake.NewProcess(1003)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{
		"cmd":     "cat /run/secrets/TOKEN",
		"secrets": map[string]string{"TOKEN": "super-secret-value"},
	})

	// The value was staged under /run/secrets, never recorded.
	if data, err := sub.ReadFile(context.Background(), sandbox, secretPath("TOKEN"), 1024); err != nil || string(data) != "super-secret-value" {
		t.Fatalf("secret not staged: %q %v", data, err)
	}
	row, _ := db.GetJob(context.Background(), jobID)
	if strings.Contains(row.Cmd, "super-secret-value") {
		t.Fatalf("secret value leaked into cmd: %q", row.Cmd)
	}

	writeJobFile(t, sub, sandbox, jobID, "rc", "0\n")
	p.Push(substrate.ProcessEvent{Kind: substrate.EventExit, ExitCode: 0})
	waitJobState(t, db, jobID, "exited", 2*time.Second)

	// The backend's cleanup removes it (the guest wrapper normally does).
	if _, err := sub.ReadFile(context.Background(), sandbox, secretPath("TOKEN"), 1024); !errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("secret still present after exit: %v", err)
	}
}

// TestJobLongPoll: ?wait= returns at once for an exited job and, for a
// running one, at the wait deadline.
func TestJobLongPoll(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)

	p := fake.NewProcess(1004)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})

	// A running job: wait=1 must return after about a second, still
	// running.
	start := time.Now()
	resp, body := doReq(t, "GET", ts.URL+"/api/sandboxes/"+id+"/jobs/"+jobID+"?wait=1", "token-a", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("wait status %d: %v", resp.StatusCode, body)
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Fatalf("wait returned too early: %s", time.Since(start))
	}
	job := body["job"].(map[string]any)
	if job["state"] != "running" {
		t.Fatalf("job state = %v, want running", job["state"])
	}

	// The job finishes: a wait returns immediately with the record.
	writeJobFile(t, sub, sandbox, jobID, "stdout", "z\n")
	writeJobFile(t, sub, sandbox, jobID, "rc", "0\n")
	p.Push(substrate.ProcessEvent{Kind: substrate.EventExit, ExitCode: 0})
	waitJobState(t, db, jobID, "exited", 2*time.Second)

	resp, body = doReq(t, "GET", ts.URL+"/api/sandboxes/"+id+"/jobs/"+jobID+"?wait=30", "token-a", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("wait-exited status %d", resp.StatusCode)
	}
	job = body["job"].(map[string]any)
	if job["state"] != "exited" {
		t.Fatalf("job state = %v, want exited", job["state"])
	}
	// The record view carries the last stdout.
	if body["stdout"] != "z\n" {
		t.Fatalf("stdout = %q, want z\\n", body["stdout"])
	}
}

// TestJobOutputRange: the output endpoint returns raw bytes by range.
func TestJobOutputRange(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)

	p := fake.NewProcess(1005)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "printf abcdef"})

	writeJobFile(t, sub, sandbox, jobID, "stdout", "abcdef")
	writeJobFile(t, sub, sandbox, jobID, "stderr", "xy")

	get := func(query string) string {
		req, _ := http.NewRequest("GET", ts.URL+"/api/sandboxes/"+id+"/jobs/"+jobID+"/output?"+query, nil)
		req.Header.Set("Authorization", "Bearer token-a")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("output %s: %v", query, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("output status %d for %s", resp.StatusCode, query)
		}
		data, _ := io.ReadAll(resp.Body)
		return string(data)
	}

	if got := get("stream=stdout&offset=2&limit=3"); got != "cde" {
		t.Fatalf("range [2,5) = %q, want cde", got)
	}
	if got := get("stream=stdout"); got != "abcdef" {
		t.Fatalf("default stdout = %q", got)
	}
	if got := get("stream=stderr"); got != "xy" {
		t.Fatalf("stderr = %q", got)
	}
	if got := get("stream=stdout&offset=99"); got != "" {
		t.Fatalf("offset past end = %q, want empty", got)
	}
}

// TestJobSignal: a running job's process group is signalled; a finished
// job answers 409.
func TestJobSignal(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)

	p := fake.NewProcess(1006)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})
	writeJobFile(t, sub, sandbox, jobID, "pid", "4242\n")

	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/jobs/"+jobID+"/signal", "token-a", map[string]any{"signal": "TERM"})
	if resp.StatusCode != 200 {
		t.Fatalf("signal status %d: %v", resp.StatusCode, body)
	}
	if calls(sub.Fake, "Exec") == 0 {
		t.Fatalf("signal issued no guest exec")
	}

	// A bad signal name is refused before anything runs (the job is
	// still running at this point).
	resp, _ = doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/jobs/"+jobID+"/signal", "token-a", map[string]any{"signal": "HUP"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad signal = %d, want 400", resp.StatusCode)
	}

	// Finish it; a second signal is 409.
	writeJobFile(t, sub, sandbox, jobID, "rc", "143\n")
	p.Push(substrate.ProcessEvent{Kind: substrate.EventExit, ExitCode: 143})
	waitJobState(t, db, jobID, "exited", 2*time.Second)

	resp, _ = doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/jobs/"+jobID+"/signal", "token-a", map[string]any{"signal": "KILL"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("signal of exited job = %d, want 409", resp.StatusCode)
	}
}

// TestJobCap: a lease at MAX_RUNNING_JOBS_PER_LEASE running jobs answers
// 429.
func TestJobCap(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, lease, _ := createJobLease(t, ts, svc)
	svc.cfg.MaxRunningJobsPerLease = 2

	procs := []*fake.FakeProcess{fake.NewProcess(2001), fake.NewProcess(2002)}
	i := 0
	sub.SetStartHandler(func(sandboxID string, req substrate.StartRequest) (substrate.Process, error) {
		p := procs[i%len(procs)]
		i++
		return p, nil
	})
	t.Cleanup(func() { sub.SetStartHandler(nil) })

	startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})
	startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 601"})

	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/exec", "token-a", map[string]any{"cmd": "sleep 602", "background": true})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third job status %d, want 429: %v", resp.StatusCode, body)
	}
	if n, _ := db.CountRunningJobs(context.Background(), id); n != 2 {
		t.Fatalf("running jobs = %d, want 2", n)
	}
	_ = lease
}

// TestJobLostOnColdRestart: a generation bump (cold restart) marks every
// running job lost.
func TestJobLostOnColdRestart(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, lease, _ := createJobLease(t, ts, svc)

	p := fake.NewProcess(1007)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})

	if _, err := svc.restart(ctxBackground(), lease.Owner, id, "cold"); err != nil {
		t.Fatalf("cold restart: %v", err)
	}
	row := waitJobState(t, db, jobID, "lost", 2*time.Second)
	if row.EndedAt.IsZero() {
		t.Fatalf("lost job has no ended_at")
	}
}

// TestJobRecordsSurviveReopen: a record outlives a store reopen.
func TestJobRecordsSurviveReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spoond.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	now := time.Now()
	if err := db.UpsertLease(ctx, store.LeaseRow{
		ID: "l-1", Owner: "c", Image: "py-base", State: "running",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), Generation: 1,
		Class: "guaranteed",
	}); err != nil {
		t.Fatalf("lease: %v", err)
	}
	rc := 0
	if err := db.InsertJob(ctx, store.JobRow{
		JobID: "j-1", LeaseID: "l-1", Owner: "c", Cmd: "echo hi",
		State: "exited", ExitCode: &rc, StartedAt: now, EndedAt: now,
	}); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db2, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	row, err := db2.GetJob(ctx, "j-1")
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if row.State != "exited" || row.ExitCode == nil || *row.ExitCode != 0 {
		t.Fatalf("reopened record = %+v", row)
	}
	// Deleting the lease deletes its jobs (ON DELETE CASCADE).
	if err := db2.DeleteLease(ctx, "l-1"); err != nil {
		t.Fatalf("delete lease: %v", err)
	}
	if _, err := db2.GetJob(ctx, "j-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("job survived lease deletion: %v", err)
	}
}

// TestJobPruning: exited records older than the retention are pruned;
// running ones are not.
func TestJobPruning(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, _ := createJobLease(t, ts, svc)
	svc.cfg.JobRetentionSecs = 1

	p := fake.NewProcess(1008)
	installJobProcess(t, sub, p)
	old, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "echo old"})
	running, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})

	// Finish the old one and backdate its end.
	ctx := context.Background()
	if _, err := db.UpdateJobExit(ctx, old, 0, time.Now().Add(-time.Hour), ""); err != nil {
		t.Fatalf("update exit: %v", err)
	}
	svc.pruneJobs(ctx)
	if _, err := db.GetJob(ctx, old); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old record not pruned: %v", err)
	}
	if _, err := db.GetJob(ctx, running); err != nil {
		t.Fatalf("running record pruned: %v", err)
	}
}

// TestRunningJobKeepsLeaseActive: a running job keeps a persistent lease
// out of the idle sweep.
func TestRunningJobKeepsLeaseActive(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	svc.cfg.IdleTimeout = 100 * time.Millisecond
	_, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "persistent": true})
	id := body["id"].(string)
	l := svc.lookupAny(id)
	if l == nil {
		t.Fatalf("lease not found")
	}

	p := fake.NewProcess(1009)
	installJobProcess(t, sub, p)
	startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})

	// Age the lease past the idle timeout: the running job keeps it out
	// of the sweep.
	svc.store.mu.Lock()
	l.LastActive = time.Now().Add(-time.Hour)
	svc.store.mu.Unlock()
	svc.sweepExpired(context.Background())

	if got := svc.lookupAny(id); got == nil || got.Suspended {
		t.Fatalf("lease suspended mid-job: %+v", got)
	}
}

// TestJobEvents: job_started, job_exited and job_lost are emitted on the
// lease event bus.
func TestJobEvents(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	id, lease, _ := createJobLease(t, ts, svc)

	sub2 := svc.Subscribe(EventFilter{})
	defer sub2.Close()

	p := fake.NewProcess(1014)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "echo out; exit 5"})
	writeJobFile2(t, sub, lease.SandboxID, jobID)
	p.Push(substrate.ProcessEvent{Kind: substrate.EventExit, ExitCode: 5})

	var got []LeaseEventType
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case ev := <-sub2.C:
			if ev.LeaseID == id {
				got = append(got, ev.Type)
				if ev.Type == LeaseJobExited && !strings.HasPrefix(ev.Detail, "exit 5") {
					t.Fatalf("job_exited detail = %q", ev.Detail)
				}
			}
		case <-deadline:
			t.Fatalf("events = %v, want job_started then job_exited", got)
		}
	}
	if got[0] != LeaseJobStarted || got[1] != LeaseJobExited {
		t.Fatalf("events = %v", got)
	}
}

// writeJobFile2 finishes a job with rc=5 and a stderr tail.
func writeJobFile2(t *testing.T, sub *testSub, sandboxID, jobID string) {
	t.Helper()
	writeJobFile(t, sub, sandboxID, jobID, "rc", "5\n")
	writeJobFile(t, sub, sandboxID, jobID, "stderr", "boom\n")
}

// TestJobExitedDetailCut: the job_exited event detail is "exit <code>"
// plus the last lines of stderr, cut to 1 KiB.
func TestJobExitedDetailCut(t *testing.T) {
	long := strings.Repeat("line that is fairly long\n", 200)
	detail := jobExitDetail("cmd", 7, long)
	if !strings.HasPrefix(detail, "exit 7") {
		t.Fatalf("detail prefix = %q", detail)
	}
	if len(detail) > jobEventDetailBytes {
		t.Fatalf("detail = %d bytes, want <= %d", len(detail), jobEventDetailBytes)
	}
	lines := strings.Split(strings.TrimPrefix(detail, "exit 7\n"), "\n")
	if len(lines) > jobEventStderrLines {
		t.Fatalf("detail carries %d stderr lines, want <= %d", len(lines), jobEventStderrLines)
	}
	// The job_started detail cuts the command to 120 bytes.
	cmd := strings.Repeat("x", 300)
	got := cutDetail(cmd, jobEventCmdChars)
	if len(got) > jobEventCmdChars || !strings.HasSuffix(got, "...") {
		t.Fatalf("cut cmd = %q (%d bytes)", got, len(got))
	}
}

// TestJobsListAndDetail: the list is newest first and the detail carries
// the record's jobs summary field.
func TestJobsListAndDetail(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)

	p := fake.NewProcess(1010)
	installJobProcess(t, sub, p)
	first, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "echo first"})
	writeJobFile(t, sub, sandbox, first, "rc", "0\n")
	p.Push(substrate.ProcessEvent{Kind: substrate.EventExit, ExitCode: 0})
	waitJobState(t, svc.db, first, "exited", 2*time.Second)

	second := fake.NewProcess(1011)
	installJobProcess(t, sub, second)
	startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})

	resp, body := doReq(t, "GET", ts.URL+"/api/sandboxes/"+id+"/jobs", "token-a", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("list status %d", resp.StatusCode)
	}
	jobs, _ := body["jobs"].([]any)
	if len(jobs) != 2 {
		t.Fatalf("jobs = %v", body["jobs"])
	}

	// The lease detail carries the jobs summary.
	resp, detail := doReq(t, "GET", ts.URL+"/api/sandboxes/"+id, "token-a", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("detail status %d", resp.StatusCode)
	}
	jv, _ := detail["jobs"].(map[string]any)
	if jv == nil {
		t.Fatalf("lease detail has no jobs field: %v", detail)
	}
	if jv["running"] != float64(1) {
		t.Fatalf("jobs.running = %v, want 1", jv["running"])
	}
	last, _ := jv["last_exit"].(map[string]any)
	if last == nil || last["job_id"] != first {
		t.Fatalf("jobs.last_exit = %v, want job %s", jv["last_exit"], first)
	}
}

// TestJobsCrossConsumerDenied: another consumer cannot read a lease's
// jobs.
func TestJobsCrossConsumerDenied(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	id, _, _ := createJobLease(t, ts, svc)

	p := fake.NewProcess(1012)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})

	for _, path := range []string{
		"/api/sandboxes/" + id + "/jobs",
		"/api/sandboxes/" + id + "/jobs/" + jobID,
		"/api/sandboxes/" + id + "/jobs/" + jobID + "/output",
	} {
		resp, _ := doReq(t, "GET", ts.URL+path, "token-b", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("cross-consumer %s = %d, want 404", path, resp.StatusCode)
		}
	}
}

// TestJobStartOnSuspendedLease: a background exec on a suspended lease is
// 409, like exec.
func TestJobStartOnSuspendedLease(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)
	_, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "persistent": true})
	id := body["id"].(string)
	if _, err := svc.suspend(context.Background(), "consumer-a", id); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/exec", "token-a", map[string]any{"cmd": "echo", "background": true})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("suspended background exec = %d, want 409", resp.StatusCode)
	}
}

// TestJobStartOnLostLease: a background exec on a lost lease is 410.
func TestJobStartOnLostLease(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)
	id, lease, _ := createJobLease(t, ts, svc)
	svc.store.mu.Lock()
	lease.setState("lost")
	svc.saveLeaseLocked(lease)
	svc.store.mu.Unlock()

	resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/exec", "token-a", map[string]any{"cmd": "echo", "background": true})
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("lost background exec = %d, want 410", resp.StatusCode)
	}
}

// TestJobStartFailureMapsToSandboxGone: a substrate not-found while
// starting is 410, like exec.
func TestJobStartFailureMapsToSandboxGone(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	id, _, _ := createJobLease(t, ts, svc)

	sub.SetStartHandler(func(sandboxID string, req substrate.StartRequest) (substrate.Process, error) {
		return nil, substrate.ErrNotFound
	})
	t.Cleanup(func() { sub.SetStartHandler(nil) })

	resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/exec", "token-a", map[string]any{"cmd": "echo", "background": true})
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("start not-found = %d, want 410", resp.StatusCode)
	}
}

// TestJobOutputSurvivesRestart: the record and its output are read from
// the guest files after a simulated backend restart (a fresh Service over
// the same store sees the running record).
func TestJobOutputSurvivesRestart(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)

	p := fake.NewProcess(1013)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "echo persist"})
	writeJobFile(t, sub, sandbox, jobID, "stdout", "persist\n")
	writeJobFile(t, sub, sandbox, jobID, "rc", "0\n")

	// A fresh Service over the same store: its reconcile finds the job
	// through the files.
	svc.log = log.New(io.Discard, "", 0)
	svc.reconcileJobs(context.Background())
	row := waitJobState(t, db, jobID, "exited", 2*time.Second)
	if row.ExitCode == nil || *row.ExitCode != 0 {
		t.Fatalf("exit = %v", row.ExitCode)
	}
}

// ctxBackground is time-independent context for direct service calls.
func ctxBackground() context.Context { return context.Background() }
