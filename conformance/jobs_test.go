//go:build conformance

package conformance

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Group J (2.6, #135): background exec jobs. Always on: the group runs a
// command in the background, follows its exit through the API and the
// event stream, and signals a long-running job. See docs/api.md.

// TestJ1_BackgroundJobExit: start `sh -c 'echo out; echo err >&2; sleep
// 3; exit 7'` in the background; GET with wait=30 returns exited,
// exit_code 7, stdout "out", stderr "err", and the event stream carried
// job_exited with "exit 7".
func TestJ1_BackgroundJobExit(t *testing.T) {
	rec := begin(t)
	l := createLease(t, map[string]any{"image": "py-base", "ttl": 600})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, stop := cl.watchEvents(ctx, l.ID)
	defer stop()

	st, body, err := cl.startJob(l.ID, execReq{Cmd: "sh -c 'echo out; echo err >&2; sleep 3; exit 7'"})
	if err != nil {
		failf(t, "start job: %v", err)
	}
	if st != 202 {
		failf(t, "start job status %d, want 202: %s", st, truncate(body))
	}
	var start jobStart
	if err := json.Unmarshal(body, &start); err != nil || start.JobID == "" {
		failf(t, "start job bad body: %v (%s)", err, truncate(body))
	}
	rec.set("job_id", start.JobID)

	// Long-poll until the job ends (or the 30 s wait expires).
	t0 := time.Now()
	st, body, err = cl.readJob(l.ID, start.JobID, 30)
	if err != nil {
		failf(t, "read job: %v", err)
	}
	if st != 200 {
		failf(t, "read job status %d: %s", st, truncate(body))
	}
	var read jobRead
	if err := json.Unmarshal(body, &read); err != nil {
		failf(t, "read job bad body: %v", err)
	}
	rec.set("wait_ms", time.Since(t0).Milliseconds())
	if read.Job.State != "exited" {
		failf(t, "job state = %q, want exited", read.Job.State)
	}
	if read.Job.ExitCode == nil || *read.Job.ExitCode != 7 {
		failf(t, "job exit_code = %v, want 7", read.Job.ExitCode)
	}
	if strings.TrimSpace(read.Stdout) != "out" {
		failf(t, "job stdout = %q, want out", read.Stdout)
	}
	if strings.TrimSpace(read.Stderr) != "err" {
		failf(t, "job stderr = %q, want err", read.Stderr)
	}

	// The event stream carried job_exited with "exit 7".
	deadline := time.After(10 * time.Second)
	sawExit := false
	for !sawExit {
		select {
		case data, ok := <-events:
			if !ok {
				failf(t, "event stream closed before job_exited")
			}
			var ev struct {
				LeaseID string `json:"lease_id"`
				Type    string `json:"type"`
				Detail  string `json:"detail"`
			}
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				continue
			}
			if ev.Type == "gap" {
				continue
			}
			if ev.LeaseID == l.ID && ev.Type == "job_exited" {
				if !strings.Contains(ev.Detail, "exit 7") {
					failf(t, "job_exited detail = %q, want exit 7", ev.Detail)
				}
				sawExit = true
			}
		case <-deadline:
			failf(t, "no job_exited event for %s", l.ID)
		}
	}
}

// TestJ2_BackgroundJobSignal: start `sleep 600`, signal TERM, and it
// exits within 10 s.
func TestJ2_BackgroundJobSignal(t *testing.T) {
	begin(t)
	l := createLease(t, map[string]any{"image": "py-base", "ttl": 600})

	st, body, err := cl.startJob(l.ID, execReq{Cmd: "sleep 600"})
	if err != nil {
		failf(t, "start job: %v", err)
	}
	if st != 202 {
		failf(t, "start job status %d: %s", st, truncate(body))
	}
	var start jobStart
	if err := json.Unmarshal(body, &start); err != nil || start.JobID == "" {
		failf(t, "start job bad body: %v (%s)", err, truncate(body))
	}

	st, body, err = cl.signalJob(l.ID, start.JobID, "TERM")
	if err != nil {
		failf(t, "signal: %v", err)
	}
	if st != 200 {
		failf(t, "signal status %d: %s", st, truncate(body))
	}

	// The job must report exited within 10 s. TERM to the process group
	// reaches the command shell, so the recorded rc is 143 (128+15); a
	// record merely marked lost (a generation-bump side effect) is not an
	// exit and must not pass this case.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, body, err = cl.readJob(l.ID, start.JobID, 2)
		if err != nil {
			failf(t, "read job: %v", err)
		}
		if st != 200 {
			failf(t, "read job status %d: %s", st, truncate(body))
		}
		var read jobRead
		if err := json.Unmarshal(body, &read); err != nil {
			failf(t, "read job bad body: %v", err)
		}
		if read.Job.State == "lost" {
			failf(t, "job marked lost, want exited after TERM")
		}
		if read.Job.State == "exited" {
			if read.Job.ExitCode == nil || *read.Job.ExitCode != 143 {
				failf(t, "job exit_code = %v, want 143 after TERM", read.Job.ExitCode)
			}
			return
		}
	}
	failf(t, "job %s still running 10 s after TERM", start.JobID)
}

// TestJ3_BackgroundJobMaxRuntime: a job may request a shorter
// max_runtime_secs than the host JOB_MAX_RUNTIME and is then killed and
// marked exited with reason timed_out within a few seconds (spoond-wb5).
// The 1 s request is shorter than any host default, so the test pins the
// per-job cap path; the reconcile loop runs every 10 s, hence the
// generous window.
func TestJ3_BackgroundJobMaxRuntime(t *testing.T) {
	begin(t)
	l := createLease(t, map[string]any{"image": "py-base", "ttl": 600})

	st, body, err := cl.startJob(l.ID, execReq{Cmd: "sleep 600", MaxRuntimeSecs: 1})
	if err != nil {
		failf(t, "start job: %v", err)
	}
	if st != 202 {
		failf(t, "start job status %d: %s", st, truncate(body))
	}
	var start jobStart
	if err := json.Unmarshal(body, &start); err != nil || start.JobID == "" {
		failf(t, "start job bad body: %v (%s)", err, truncate(body))
	}

	// The reconcile loop runs every 10 s; allow it plus the kill a
	// generous window.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		st, body, err = cl.readJob(l.ID, start.JobID, 2)
		if err != nil {
			failf(t, "read job: %v", err)
		}
		if st != 200 {
			failf(t, "read job status %d: %s", st, truncate(body))
		}
		var read jobRead
		if err := json.Unmarshal(body, &read); err != nil {
			failf(t, "read job bad body: %v", err)
		}
		if read.Job.State == "exited" {
			if read.Job.Reason != "timed_out" {
				failf(t, "job reason = %q, want timed_out", read.Job.Reason)
			}
			if read.Job.ExitCode == nil || *read.Job.ExitCode != 124 {
				failf(t, "job exit_code = %v, want 124", read.Job.ExitCode)
			}
			return
		}
	}
	failf(t, "job %s not timed out within 30 s", start.JobID)
}
