package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
	"github.com/jrimmer/spoond/v2/substrate/fake"
)

// The tests in this file exercise background exec jobs (2.6, #135)
// through the lease API and the service. Most tests drive the guest
// files directly through the fake so they can script the exact timing of
// an exit; the tests in jobs_wrapper_test.go run the real guest wrapper
// through the fake's job runner and cover the wrapper, its setsid child,
// the rc atomic write and the secrets cleanup.

// useJobRunner enables the fake's real-wrapper job runner on a private
// temp directory and registers cleanup for any still-running job.
func useJobRunner(t *testing.T, sub *testSub) string {
	t.Helper()
	dir := t.TempDir()
	sub.Fake.EnableJobRunner(dir)
	t.Cleanup(sub.Fake.StopAllJobs)
	return dir
}

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

// TestJobUTF8SafeCuts: cutDetail and jobExitDetail never split a
// multi-byte rune, and invalid guest bytes become U+FFFD in a stored or
// emitted tail.
func TestJobUTF8SafeCuts(t *testing.T) {
	// "aébcdef": é is two bytes at [1,3). A cut at byte 2 would split it.
	s := "aébcdef"
	got := cutDetail(s, 5)
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "...") {
		t.Fatalf("cutDetail split a rune: %q", got)
	}
	if got != "a..." {
		t.Fatalf("cutDetail = %q, want a...", got)
	}

	// A 1 KiB detail whose boundary lands inside a multi-byte rune.
	long := strings.Repeat("é", 1000)
	detail := jobExitDetail(0, long)
	if len(detail) > jobEventDetailBytes {
		t.Fatalf("detail = %d bytes, want <= %d", len(detail), jobEventDetailBytes)
	}
	if !utf8.ValidString(detail) {
		t.Fatalf("jobExitDetail split a rune: %q", detail)
	}

	// Invalid UTF-8 from the guest becomes U+FFFD before it is stored or
	// emitted.
	bad := "ok \xff\xfe end"
	valid := toValidUTF8(bad)
	if !utf8.ValidString(valid) || !strings.Contains(valid, "\uFFFD") {
		t.Fatalf("toValidUTF8 = %q", valid)
	}

	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)
	p := fake.NewProcess(1020)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})

	// A stderr tail that begins with an invalid byte: the stored form is
	// valid UTF-8.
	writeJobFile(t, sub, sandbox, jobID, "stderr", "\xffoops")
	writeJobFile(t, sub, sandbox, jobID, "rc", "1\n")
	p.Push(substrate.ProcessEvent{Kind: substrate.EventExit, ExitCode: 1})
	row := waitJobState(t, db, jobID, "exited", 2*time.Second)
	if !utf8.ValidString(row.StderrTail) {
		t.Fatalf("stored stderr tail is not valid UTF-8: %q", row.StderrTail)
	}
	if !strings.ContainsRune(row.StderrTail, '\uFFFD') {
		t.Fatalf("invalid byte not replaced in stored tail: %q", row.StderrTail)
	}
}

// TestJobSignalOnSuspendedLease: signalling a running job whose lease is
// suspended answers 409 without reaching the substrate (there is no
// running sandbox to exec in until the lease resumes).
func TestJobSignalOnSuspendedLease(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	_, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "persistent": true})
	id := body["id"].(string)

	p := fake.NewProcess(1019)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})
	if _, err := svc.suspend(context.Background(), "consumer-a", id); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	before := calls(sub.Fake, "Exec")
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/jobs/"+jobID+"/signal", "token-a", map[string]any{"signal": "TERM"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("signal on suspended lease = %d, want 409: %v", resp.StatusCode, body)
	}
	if got := calls(sub.Fake, "Exec"); got != before {
		t.Fatalf("signal on suspended lease reached the substrate: Exec calls %d -> %d", before, got)
	}
}

// TestJobSignalFinishedDoesNotResume (review R5): signalling a job that
// is no longer running answers 409 without resuming the lease, so a
// finished job cannot make a suspended lease spend hugepages for
// nothing. The job record is checked before the resume.
func TestJobSignalFinishedDoesNotResume(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	_, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "persistent": true})
	id := body["id"].(string)
	l := svc.lookupAny(id)
	if l == nil {
		t.Fatalf("lease %s not in service", id)
	}
	sandbox := l.SandboxID

	p := fake.NewProcess(1007)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})
	writeJobFile(t, sub, sandbox, jobID, "rc", "143\n")
	p.Push(substrate.ProcessEvent{Kind: substrate.EventExit, ExitCode: 143})
	waitJobState(t, db, jobID, "exited", 2*time.Second)

	if _, err := svc.suspend(context.Background(), "consumer-a", id); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	createsBefore := calls(sub.Fake, "Create")
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/jobs/"+jobID+"/signal", "token-a", map[string]any{"signal": "TERM"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("signal of a finished job = %d, want 409: %v", resp.StatusCode, body)
	}
	if !l.Suspended {
		t.Fatal("signalling a finished job resumed the suspended lease")
	}
	if got := calls(sub.Fake, "Create"); got != createsBefore {
		t.Fatalf("signalling a finished job issued %d Create(s), want none", got-createsBefore)
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

// TestJobStartPerLeaseLock: the start lock is per lease. Two concurrent
// starts on one lease at cap-1 admit exactly one; starts on two leases
// do not wait for each other even while one substrate Start is blocked.
func TestJobStartPerLeaseLock(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	idA, leaseA, _ := createJobLease(t, ts, svc)
	idB, _, _ := createJobLease(t, ts, svc)
	svc.cfg.MaxRunningJobsPerLease = 1

	// Block Start for lease A's sandbox until released, so its first
	// start holds A's lock across the whole substrate round trip; starts
	// for lease B are not blocked.
	release := make(chan struct{})
	var once sync.Once
	sub.SetStartHandler(func(sandboxID string, req substrate.StartRequest) (substrate.Process, error) {
		if sandboxID == leaseA.SandboxID {
			<-release
		}
		return fake.NewProcess(3001), nil
	})
	t.Cleanup(func() {
		once.Do(func() { close(release) })
		sub.SetStartHandler(nil)
	})

	status := func(id string) int {
		resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/exec", "token-a", map[string]any{"cmd": "sleep 600", "background": true})
		return resp.StatusCode
	}

	// A start on lease B must complete even though lease A's start is
	// still blocked in the substrate: the locks are independent.
	bDone := make(chan int, 1)
	go func() { bDone <- status(idB) }()
	select {
	case code := <-bDone:
		if code != http.StatusAccepted {
			t.Fatalf("lease B start = %d, want 202", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lease B start waited on lease A's start lock")
	}

	// Two concurrent starts on lease A at cap-1: exactly one admits.
	aCodes := make(chan int, 2)
	go func() { aCodes <- status(idA) }()
	go func() { aCodes <- status(idA) }()
	// Let the first acquire the lock and block in Start; the second then
	// waits on the same lock rather than racing the cap check.
	time.Sleep(200 * time.Millisecond)
	once.Do(func() { close(release) })

	got := []int{<-aCodes, <-aCodes}
	accepted, rejected := 0, 0
	for _, c := range got {
		switch c {
		case http.StatusAccepted:
			accepted++
		case http.StatusTooManyRequests:
			rejected++
		}
	}
	if accepted != 1 || rejected != 1 {
		t.Fatalf("concurrent starts on one lease = %v, want one 202 and one 429", got)
	}
	if n, _ := db.CountRunningJobs(context.Background(), idA); n != 1 {
		t.Fatalf("running jobs on lease A = %d, want 1", n)
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

	if _, err := svc.restart(context.Background(), lease.Owner, id, "cold"); err != nil {
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
	detail := jobExitDetail(7, long)
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

// TestJobStartOnSuspendedResumes: a background exec on a hand-suspended
// lease resumes it on use (#145 D2) and starts the job, instead of the
// old 409 lease_suspended.
func TestJobStartOnSuspendedResumes(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)
	_, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "persistent": true})
	id := body["id"].(string)
	if _, err := svc.suspend(context.Background(), "consumer-a", id); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	l := svc.lookupAny(id)
	resp, out := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/exec", "token-a", map[string]any{"cmd": "echo", "background": true})
	if resp.StatusCode == http.StatusConflict {
		t.Fatalf("suspended background exec answered 409; it must resume on use: %v", out)
	}
	if l.Suspended {
		t.Fatalf("the background exec did not resume the lease: %v", out)
	}
}

// TestJobStartOnLostLease: a background exec on a lost lease is 409
// lease_lost.
func TestJobStartOnLostLease(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)
	id, lease, _ := createJobLease(t, ts, svc)
	svc.store.mu.Lock()
	lease.setState("lost")
	lease.LostReason = "substrate crash"
	svc.saveLeaseLocked(lease)
	svc.store.mu.Unlock()

	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/exec", "token-a", map[string]any{"cmd": "echo", "background": true})
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("lost background exec = %d, want 410", resp.StatusCode)
	}
	if body["code"] != "lease_lost" {
		t.Fatalf("code = %v, want lease_lost", body["code"])
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

// TestJobSignalRejectsHostilePid: the pid file is guest-writable state,
// so a non-numeric pid is refused before any guest exec runs — it must
// never become shell input.
func TestJobSignalRejectsHostilePid(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)

	p := fake.NewProcess(1015)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})
	writeJobFile(t, sub, sandbox, jobID, "pid", "123; touch /tmp/spoond-pwned\n")

	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/jobs/"+jobID+"/signal", "token-a", map[string]any{"signal": "TERM"})
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("hostile pid accepted: %v", body)
	}
	// No guest exec ran with the injected text.
	for _, c := range sub.Fake.CallLog() {
		if strings.Contains(c, "pwned") {
			t.Fatalf("injected text reached the substrate: %q", c)
		}
	}
	if _, err := os.Stat("/tmp/spoond-pwned"); err == nil {
		_ = os.Remove("/tmp/spoond-pwned")
		t.Fatalf("injection executed on the host")
	}
	_ = svc
	_ = db
}

// TestJobGetOutputKeysStable: the record view always carries stdout and
// stderr, even when the guest cannot be read (a suspended lease's envd is
// gone), so the JSON shape does not depend on lease state.
func TestJobGetOutputKeysStable(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	_, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "persistent": true})
	id := body["id"].(string)

	// Start the job, then suspend the lease: the sandbox is gone from the
	// fake's file view, so the outputs read nothing.
	p := fake.NewProcess(1016)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})
	l := svc.lookupAny(id)
	sub.Fake.Kill(l.SandboxID)

	resp, body := doReq(t, "GET", ts.URL+"/api/sandboxes/"+id+"/jobs/"+jobID, "token-a", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get status %d", resp.StatusCode)
	}
	if _, ok := body["stdout"]; !ok {
		t.Fatalf("stdout key missing: %v", body)
	}
	if _, ok := body["stderr"]; !ok {
		t.Fatalf("stderr key missing: %v", body)
	}
}

// TestJobStartContextDetached: the envd Start stream must outlive the
// HTTP request that started the job. The watcher holds that stream to
// notice the exit at once, so if it were derived from the request's
// context it would be canceled when the 202 finalises or the client
// disconnects, and the at-once path would be dead. The substrate must
// see a context that is still live after the response is written.
func TestJobStartContextDetached(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)

	p := fake.NewProcess(1017)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})

	startCtx := sub.Fake.StartContext()
	if startCtx == nil {
		t.Fatalf("Start never recorded a context")
	}
	select {
	case <-startCtx.Done():
		t.Fatalf("start context already canceled after the 202: %v", startCtx.Err())
	default:
	}

	// The exit is still noticed through the stream, so the watcher's
	// stream was not canceled with the request.
	writeJobFile(t, sub, sandbox, jobID, "rc", "0\n")
	p.Push(substrate.ProcessEvent{Kind: substrate.EventExit, ExitCode: 0})
	waitJobState(t, db, jobID, "exited", 3*time.Second)
	_ = svc
}

// TestJobWatchClosedStreamLeavesRunning: if the envd stream ends without
// an exit or error event (the backend's client saw its context die), the
// detached command may still run, so the record stays running for the
// reconcile pass rather than being marked exited.
func TestJobWatchClosedStreamLeavesRunning(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, _ := createJobLease(t, ts, svc)

	p := fake.NewProcess(1018)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})

	// Close the stream without an exit event: the watcher must not
	// finish the record.
	_ = p.Close()
	time.Sleep(100 * time.Millisecond)
	row, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if row.State != "running" {
		t.Fatalf("job state = %q after a silently closed stream, want running", row.State)
	}
	_ = svc
}

// TestJobMaxRuntimeKillsJob: a job that has spent its effective max
// runtime is killed and marked exited with reason timed_out, its exit
// code is recorded and a job_exited event names the cap. The cap is
// checked by reconcileJobs, so a `sleep infinity` cannot run forever
// (spoond-wb5).
func TestJobMaxRuntimeKillsJob(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)
	svc.cfg.JobMaxRuntimeSecs = 3600

	p := fake.NewProcess(1021)
	installJobProcess(t, sub, p)
	jobs := svc.Subscribe(EventFilter{})
	defer jobs.Close()
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})
	writeJobFile(t, sub, sandbox, jobID, "pid", "4242\n")
	writeJobFile(t, sub, sandbox, jobID, "stderr", "still here\n")

	// The requested runtime is stored on the record and shortens the host
	// cap; it is never longer than the cap.
	row, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if row.MaxRuntimeSecs != 3600 {
		t.Fatalf("max_runtime_secs = %d, want 3600 (the host cap)", row.MaxRuntimeSecs)
	}

	// Move the service clock past the cap and reconcile.
	base := time.Now()
	svc.now = func() time.Time { return base.Add(2 * time.Hour) }
	t.Cleanup(func() { svc.now = time.Now })
	svc.reconcileJobs(context.Background())

	running := db.CountRunningJobs
	if n, _ := running(context.Background(), id); n != 0 {
		t.Fatalf("running jobs after timeout = %d, want 0", n)
	}
	got := waitJobState(t, db, jobID, "exited", 2*time.Second)
	if got.Reason != "timed_out" {
		t.Fatalf("reason = %q, want timed_out", got.Reason)
	}
	if got.ExitCode == nil || *got.ExitCode != 124 {
		t.Fatalf("exit code = %v, want 124", got.ExitCode)
	}
	if !strings.Contains(got.StderrTail, "still here") {
		t.Fatalf("stderr tail = %q", got.StderrTail)
	}
	if svc.hasRunningJob(id) {
		t.Fatalf("running-job count not cleared")
	}

	// A job_exited event names the cap. job_started precedes it.
	var exited []string
	deadline := time.After(2 * time.Second)
	for len(exited) == 0 {
		select {
		case ev := <-jobs.C:
			if ev.LeaseID == id && ev.Type == LeaseJobExited {
				exited = append(exited, ev.Detail)
			}
		case <-deadline:
			t.Fatalf("no job_exited event for the timed-out job")
		}
	}
	if !strings.HasPrefix(exited[0], "timed out: exit 124") {
		t.Fatalf("job_exited detail = %q, want it to name the cap", exited[0])
	}
}

// TestJobShorterRequestedRuntimeWins: a job's own max_runtime_secs is
// used when it is shorter than the host cap, the host cap wins when the
// request is longer, and a negative request is refused.
func TestJobShorterRequestedRuntimeWins(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	id, _, _ := createJobLease(t, ts, svc)
	svc.cfg.JobMaxRuntimeSecs = 3600

	p := fake.NewProcess(1022)
	installJobProcess(t, sub, p)
	short, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600", "max_runtime_secs": 60})
	row, err := svc.db.GetJob(context.Background(), short)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if row.MaxRuntimeSecs != 60 {
		t.Fatalf("shorter request: max_runtime_secs = %d, want 60", row.MaxRuntimeSecs)
	}

	long, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 601", "max_runtime_secs": 999999})
	row, err = svc.db.GetJob(context.Background(), long)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if row.MaxRuntimeSecs != 3600 {
		t.Fatalf("longer request: max_runtime_secs = %d, want the 3600 host cap", row.MaxRuntimeSecs)
	}

	resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/exec", "token-a", map[string]any{"cmd": "sleep 602", "background": true, "max_runtime_secs": -1})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative max_runtime_secs = %d, want 400", resp.StatusCode)
	}
}

// TestJobEffectiveRuntimeNoOverflow: a request far larger than any real
// cap must be clamped to the host cap, never allowed to overflow the
// seconds-to-Duration conversion into a negative (which would read as
// uncapped and let `sleep infinity` run forever). This is the exact
// spoond-wb5 regression: requested=MaxInt64 used to store 0 (= uncapped).
func TestJobEffectiveRuntimeNoOverflow(t *testing.T) {
	svc := &Service{cfg: ServiceConfig{JobMaxRuntimeSecs: 86400}}
	cases := []struct {
		name      string
		requested int64
		want      time.Duration
	}{
		{"huge request clamps to the host cap", 9223372036854775807, 86400 * time.Second},
		{"just over the duration limit clamps to the host cap", int64(maxJobRuntimeDuration/time.Second) + 1, 86400 * time.Second},
		{"max representable seconds clamps to the host cap", int64(maxJobRuntimeDuration / time.Second), 86400 * time.Second},
		{"a shorter in-range request wins", 60, 60 * time.Second},
		{"a longer in-range request is capped", 5000000000, 86400 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := svc.effectiveJobRuntime(tc.requested); got != tc.want {
				t.Fatalf("effectiveJobRuntime(%d) = %v, want %v", tc.requested, got, tc.want)
			}
		})
	}

	// An uncapped host keeps a huge -- but representable -- request
	// rather than wrapping it negative.
	uncapped := &Service{cfg: ServiceConfig{JobMaxRuntimeSecs: -1}}
	if got := uncapped.effectiveJobRuntime(60); got != 60*time.Second {
		t.Fatalf("uncapped effectiveJobRuntime(60) = %v, want 1m", got)
	}
	if got := uncapped.effectiveJobRuntime(9223372036854775807); got <= 0 {
		t.Fatalf("uncapped effectiveJobRuntime(MaxInt64) = %v, want a positive duration", got)
	}
}

// TestJobHugeRequestStoresHostCap: end to end, a background job asking
// for an absurd max_runtime_secs is recorded with the host cap, not 0.
// A stored 0 means uncapped and lets the job run forever (spoond-wb5).
func TestJobHugeRequestStoresHostCap(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, _ := createJobLease(t, ts, svc)
	svc.cfg.JobMaxRuntimeSecs = 3600

	p := fake.NewProcess(1026)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep infinity", "max_runtime_secs": 9223372036854775807})
	row, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if row.MaxRuntimeSecs != 3600 {
		t.Fatalf("huge request stored max_runtime_secs = %d, want the 3600 host cap", row.MaxRuntimeSecs)
	}
}

// TestJobMaxRuntimeAppliesToSuspendedLease: reconcileJobs must not call
// markActive on a suspended lease's running job — a job does not keep an
// untouched suspended lease out of the held rules — and a job whose cap
// was spent while the lease was suspended is killed by the first
// reconcile after it resumes (spoond-wb5).
func TestJobMaxRuntimeAppliesToSuspendedLease(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	_, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "persistent": true})
	id := body["id"].(string)
	lease := svc.lookupAny(id)
	if lease == nil {
		t.Fatalf("lease not in service")
	}
	sandbox := lease.SandboxID
	svc.cfg.JobMaxRuntimeSecs = 3600

	p := fake.NewProcess(1023)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})
	writeJobFile(t, sub, sandbox, jobID, "pid", "4242\n")

	// Suspend the lease through the normal pause path: the job record
	// stays running, as it did before this change.
	if _, err := svc.suspend(context.Background(), "consumer-a", id); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	l := svc.lookupAny(id)
	if l == nil || !l.Suspended {
		t.Fatalf("lease not suspended: %+v", l)
	}

	// A held lease, so suspendedByRule has a rule suspension to test: the
	// holder plus the idle_suspend rule marker.
	svc.store.mu.Lock()
	l.Holder = "ci-job-1"
	l.LastActive = time.Now().Add(-time.Hour)
	l.LastAction = idleSuspendRule + "/" + heldActionSuspendIdle
	l.LastActionAt = time.Now().Add(-30 * time.Minute)
	lastActive := l.LastActive
	svc.saveLeaseLocked(l)
	svc.store.mu.Unlock()
	svc.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	t.Cleanup(func() { svc.now = time.Now })

	// Past the cap, while the lease is suspended: reconcile must neither
	// advance LastActive nor kill (the paused guest cannot be signalled).
	// The record stays running.
	svc.reconcileJobs(context.Background())
	svc.store.mu.Lock()
	l = svc.store.leases[id]
	unchanged := l != nil && l.LastActive.Equal(lastActive)
	_, byRule := suspendedByRule(l)
	svc.store.mu.Unlock()
	if !unchanged {
		t.Fatalf("reconcileJobs advanced LastActive on a suspended lease")
	}
	if !byRule {
		t.Fatalf("a suspended lease with a running job no longer looks untouched to suspendedByRule")
	}
	suspendedRow, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if suspendedRow.State != "running" {
		t.Fatalf("job in a suspended lease = %q, want running", suspendedRow.State)
	}

	// Resume: the console first reconcile kills the job whose cap was
	// spent while it was paused.
	if _, err := svc.resume(context.Background(), "consumer-a", id); err != nil {
		t.Fatalf("resume: %v", err)
	}
	svc.reconcileJobs(context.Background())
	got := waitJobState(t, db, jobID, "exited", 2*time.Second)
	if got.Reason != "timed_out" {
		t.Fatalf("reason = %q, want timed_out", got.Reason)
	}
}

// TestJobMaxRuntimeDisabled: a negative JOB_MAX_RUNTIME leaves jobs
// uncapped, and the record stores 0.
func TestJobMaxRuntimeDisabled(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, _ := createJobLease(t, ts, svc)
	svc.cfg.JobMaxRuntimeSecs = -1

	p := fake.NewProcess(1024)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})
	row, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if row.MaxRuntimeSecs != 0 {
		t.Fatalf("uncapped job max_runtime_secs = %d, want 0", row.MaxRuntimeSecs)
	}
	svc.now = func() time.Time { return time.Now().Add(1000 * time.Hour) }
	t.Cleanup(func() { svc.now = time.Now })
	svc.reconcileJobs(context.Background())
	time.Sleep(50 * time.Millisecond)
	still, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if still.State != "running" {
		t.Fatalf("uncapped job state = %q, want running", still.State)
	}
}

// TestJobReconcileDoesNotMarkSuspendedLeaseActive: reconcileJobs must
// not call markActive on a suspended lease's running job, otherwise a
// long job defeats suspendedByRule's untouched test and an idle
// suspension can never become stale (spoond-wb5). A normal exit still
// closes the record.
func TestJobReconcileDoesNotMarkSuspendedLeaseActive(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	_, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "persistent": true})
	id := body["id"].(string)
	lease := svc.lookupAny(id)
	if lease == nil {
		t.Fatalf("lease not in service")
	}
	sandbox := lease.SandboxID

	p := fake.NewProcess(1025)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})

	if _, err := svc.suspend(context.Background(), "consumer-a", id); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	svc.store.mu.Lock()
	l := svc.store.leases[id]
	l.LastActive = time.Now().Add(-time.Hour)
	l.LastAction = idleSuspendRule + "/" + heldActionSuspendIdle
	l.LastActionAt = time.Now().Add(-30 * time.Minute)
	lastActive := l.LastActive
	svc.store.mu.Unlock()

	// Several reconcile passes while the job runs: LastActive must not
	// move.
	for i := 0; i < 3; i++ {
		svc.reconcileJobs(context.Background())
	}
	svc.store.mu.Lock()
	l = svc.store.leases[id]
	unchanged := l != nil && l.LastActive.Equal(lastActive)
	svc.store.mu.Unlock()
	if !unchanged {
		t.Fatalf("reconcileJobs marked a suspended lease active while its job ran")
	}

	// The job's normal exit is still noticed after a resume.
	if _, err := svc.resume(context.Background(), "consumer-a", id); err != nil {
		t.Fatalf("resume: %v", err)
	}
	writeJobFile(t, sub, sandbox, jobID, "rc", "0\n")
	svc.reconcileJobs(context.Background())
	waitJobState(t, db, jobID, "exited", 2*time.Second)
}

// TestJobMaxRuntimeKillRetriesAfterFailure: if the cap's kill fails, the
// record must stay running (not be closed as timed_out) so the next
// reconcile retries, otherwise ListRunningJobs never returns it and the
// running-job cap stops counting it — the leak the cap exists to close
// (spoond-wb5 B1).
func TestJobMaxRuntimeKillRetriesAfterFailure(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)
	svc.cfg.JobMaxRuntimeSecs = 3600

	p := fake.NewProcess(1031)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep infinity"})
	writeJobFile(t, sub, sandbox, jobID, "pid", "4242\n")
	writeJobFile(t, sub, sandbox, jobID, "stderr", "still here\n")

	// The first kill fails at the substrate.
	var kills atomic.Int32
	sub.SetExecHandler(func(sandboxID string, req substrate.ExecRequest) substrate.ExecResult {
		if len(req.Args) == 6 && req.Args[0] == "/bin/sh" && strings.HasPrefix(req.Args[2], "kill ") {
			if kills.Add(1) == 1 {
				return substrate.ExecResult{Stderr: "envd hiccup", ExitCode: 1}
			}
			return substrate.ExecResult{ExitCode: 0}
		}
		return substrate.ExecResult{Stdout: "ok\n"}
	})
	t.Cleanup(func() { sub.SetExecHandler(nil) })

	base := time.Now()
	svc.now = func() time.Time { return base.Add(2 * time.Hour) }
	t.Cleanup(func() { svc.now = time.Now })

	svc.reconcileJobs(context.Background())
	still, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if still.State != "running" {
		t.Fatalf("job after a failed kill = %q, want running", still.State)
	}
	if still.Reason != "" {
		t.Fatalf("job after a failed kill reason = %q, want empty", still.Reason)
	}
	if n, _ := db.CountRunningJobs(context.Background(), id); n != 1 {
		t.Fatalf("running jobs after a failed kill = %d, want 1", n)
	}

	// The next reconcile retries and closes the record.
	svc.reconcileJobs(context.Background())
	got := waitJobState(t, db, jobID, "exited", 2*time.Second)
	if got.Reason != "timed_out" {
		t.Fatalf("reason = %q, want timed_out", got.Reason)
	}
	if got.ExitCode == nil || *got.ExitCode != 124 {
		t.Fatalf("exit code = %v, want 124", got.ExitCode)
	}
	if n, _ := db.CountRunningJobs(context.Background(), id); n != 0 {
		t.Fatalf("running jobs after the retried kill = %d, want 0", n)
	}
}

// TestJobMaxRuntimeSkipsBusyLease: a lease busy with an in-flight pause
// (Suspended still false while the snapshot is written) must be skipped
// by the cap, since the substrate cannot be signalled mid-pause and a
// kill would be captured in the snapshot. Once the lease is idle again
// the next reconcile kills the job (spoond-wb5 B1).
func TestJobMaxRuntimeSkipsBusyLease(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)
	svc.cfg.JobMaxRuntimeSecs = 3600

	p := fake.NewProcess(1032)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep infinity"})
	writeJobFile(t, sub, sandbox, jobID, "pid", "4242\n")

	// Mark the lease busy the way an in-flight pause does.
	svc.store.mu.Lock()
	svc.store.leases[id].busy = true
	svc.store.mu.Unlock()

	base := time.Now()
	svc.now = func() time.Time { return base.Add(2 * time.Hour) }
	t.Cleanup(func() { svc.now = time.Now })

	svc.reconcileJobs(context.Background())
	still, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if still.State != "running" {
		t.Fatalf("job on a busy lease = %q, want running", still.State)
	}

	svc.store.mu.Lock()
	svc.store.leases[id].busy = false
	svc.store.mu.Unlock()
	svc.reconcileJobs(context.Background())
	got := waitJobState(t, db, jobID, "exited", 2*time.Second)
	if got.Reason != "timed_out" {
		t.Fatalf("reason = %q, want timed_out", got.Reason)
	}
}

// TestJobMaxRuntimeAppliesToPreUpgradeRecord: a running job recorded
// before the cap existed (max_runtime_secs 0) is still capped by the
// current JOB_MAX_RUNTIME; a 0 record is not "uncapped" once the host has
// a cap (spoond-wb5 NIT).
func TestJobMaxRuntimeAppliesToPreUpgradeRecord(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, lease, sandbox := createJobLease(t, ts, svc)
	svc.cfg.JobMaxRuntimeSecs = 3600

	// A record as an older backend would have written it: no cap stored.
	started := time.Now().Add(-2 * time.Hour)
	if err := db.InsertJob(context.Background(), store.JobRow{
		JobID: "j-pre-upgrade", LeaseID: id, Owner: "consumer-a", Cmd: "sleep infinity",
		State: "running", StartedAt: started, Generation: lease.Generation,
	}); err != nil {
		t.Fatalf("insert pre-upgrade job: %v", err)
	}
	writeJobFile(t, sub, sandbox, "j-pre-upgrade", "pid", "4242\n")

	svc.reconcileJobs(context.Background())
	got := waitJobState(t, db, "j-pre-upgrade", "exited", 2*time.Second)
	if got.Reason != "timed_out" {
		t.Fatalf("reason = %q, want timed_out", got.Reason)
	}
	if got.ExitCode == nil || *got.ExitCode != 124 {
		t.Fatalf("exit code = %v, want 124", got.ExitCode)
	}
}

// TestJobMaxRuntimeDoesNotRereadLeaseFieldsUnlocked: the cap path must
// read the sandbox id and generation under the store lock (via the
// snapshotted continuity), not off a *Lease returned by lookupAny, which
// races resume/restore. Run with -race (spoond-wb5 B2).
func TestJobMaxRuntimeDoesNotRereadLeaseFieldsUnlocked(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)
	svc.cfg.JobMaxRuntimeSecs = 3600

	p := fake.NewProcess(1033)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep infinity"})
	writeJobFile(t, sub, sandbox, jobID, "pid", "4242\n")

	base := time.Now()
	svc.now = func() time.Time { return base.Add(2 * time.Hour) }
	t.Cleanup(func() { svc.now = time.Now })

	// A concurrent writer mutating SandboxID/Generation while the cap
	// path reads them: a data race would be caught by -race.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			svc.store.mu.Lock()
			if l := svc.store.leases[id]; l != nil {
				l.SandboxID = sandbox
				l.Generation = 1
			}
			svc.store.mu.Unlock()
		}
	}()
	svc.reconcileJobs(context.Background())
	wg.Wait()
	got := waitJobState(t, db, jobID, "exited", 2*time.Second)
	if got.Reason != "timed_out" {
		t.Fatalf("reason = %q, want timed_out", got.Reason)
	}
}

// TestJobWatcherDoesNotRecordTimingOutExit: while the cap is killing a
// job, the live watcher must not close the record as a plain exit — the
// cap owns the outcome. An in-memory flag blocks finishJob in that
// window, so the record stays running until the cap records timed_out
// (spoond-wb5 B1).
func TestJobWatcherDoesNotRecordTimingOutExit(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)

	p := fake.NewProcess(1034)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep infinity"})
	row, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	writeJobFile(t, sub, sandbox, jobID, "rc", "0\n")

	// The cap flags the job, then the watcher sees the kill's signal exit.
	svc.markJobTimingOut(jobID)
	svc.finishJobFromGuest(context.Background(), row, sandbox, row.Generation)
	still, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if still.State != "running" {
		t.Fatalf("job while timing out = %q, want running", still.State)
	}

	// The cap closes it with its own reason and clears the flag.
	svc.finishJobTimedOut(context.Background(), row, 124, "", sandbox, row.Generation)
	got := waitJobState(t, db, jobID, "exited", 2*time.Second)
	if got.Reason != "timed_out" {
		t.Fatalf("reason = %q, want timed_out", got.Reason)
	}
	if svc.jobIsTimingOut(jobID) {
		t.Fatalf("timing-out flag not cleared after the record closed")
	}

	// A later normal exit is no longer blocked.
	if err := svc.finishJob(context.Background(), row, 0, "", sandbox, row.Generation); err != nil {
		t.Fatalf("finishJob after timing out: %v", err)
	}
}
