package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v4/metrics"
	"github.com/jrimmer/spoond/v4/substrate/fake"
	dto "github.com/prometheus/client_model/go"
)

// TestReleaseSettlesRunningJobs: releasing a lease with a job still
// running brings the running-jobs gauge back down and emits job_lost:
// the job's row cascades away with the lease, so no exit or reconcile
// path would ever finish it.
func TestReleaseSettlesRunningJobs(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	m := metrics.NewBackendMetrics()
	svc.SetMetrics(m)
	id, lease, _ := createJobLease(t, ts, svc)
	installJobProcess(t, sub, fake.NewProcess(1009))
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})

	gauge := func() float64 {
		var d dto.Metric
		if err := m.JobsRunning.Write(&d); err != nil {
			t.Fatal(err)
		}
		return d.GetGauge().GetValue()
	}
	if g := gauge(); g != 1 {
		t.Fatalf("running gauge with one job = %v, want 1", g)
	}

	events := svc.Subscribe(EventFilter{LeaseID: id})
	defer events.Close()
	svc.releaseBecause(context.Background(), lease, "deleted through the API")

	if g := gauge(); g != 0 {
		t.Fatalf("running gauge after the release = %v, want 0", g)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events.C:
			if ev.Type == LeaseJobLost {
				if ev.Detail != "job "+jobID+": lease released" {
					t.Fatalf("job_lost detail = %q", ev.Detail)
				}
				return
			}
		case <-deadline:
			t.Fatal("no job_lost event after releasing a lease with a running job")
		}
	}
}

// TestRunningJobKeepsLeaseOutOfIdleSuspend: a lease with its own
// idle_suspend is not suspended while a background job runs on it.
func TestRunningJobKeepsLeaseOutOfIdleSuspend(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	ctx := context.Background()
	l, err := svc.grant(ctx, "consumer-a", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.setIdlePolicy(l, 60); err != nil {
		t.Fatalf("setIdlePolicy: %v", err)
	}
	installJobProcess(t, sub, fake.NewProcess(1011))
	startBackgroundJob(t, ts, l.ID, map[string]any{"cmd": "sleep 600"})
	svc.store.mu.Lock()
	l.LastActive = time.Now().Add(-10 * time.Minute)
	svc.store.mu.Unlock()
	svc.suspendIdleLeases(ctx, time.Now())
	if l.Suspended {
		t.Fatal("idle_suspend suspended a lease with a running background job")
	}
}

// TestCrashLostMarksJobsLost: a lease lost in a crash (no checkpoint to
// recover from) takes its running jobs with it: they are marked lost,
// not left running forever.
func TestCrashLostMarksJobsLost(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	svc.cfg.CrashTest = true
	id, _, _ := createJobLease(t, ts, svc)
	installJobProcess(t, sub, fake.NewProcess(1013))
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})

	resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/crash-test", "token-a", nil)
	if resp.StatusCode != 200 || body["result"] != "lost" {
		t.Fatalf("crash test = %d %v, want 200 lost", resp.StatusCode, body)
	}
	waitJobState(t, db, jobID, "lost", 2*time.Second)
}

// TestReleasedLeaseKeepsJobRecords: a lease's job records outlive it
// (ended_jobs). A job still running at the release reads back lost,
// reason lease_released, with the stderr tail the guest held at the
// release; its owner still lists and gets it, nobody else does, and the
// sweep prunes it once the retention window has run out.
func TestReleasedLeaseKeepsJobRecords(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	id, lease, sandboxID := createJobLease(t, ts, svc)
	installJobProcess(t, sub, fake.NewProcess(1009))
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "worker-start"})
	writeJobFile(t, sub, sandboxID, jobID, "stderr", "mail: HELLO failed: connection refused\n")

	svc.releaseBecause(context.Background(), lease, "released from inside the lease")

	resp, body := doReq(t, "GET", ts.URL+"/api/leases/"+id+"/jobs", "token-a", nil)
	if resp.StatusCode != http.StatusOK || body["lease_ended"] != true {
		t.Fatalf("list after release: %d %v", resp.StatusCode, body)
	}
	if jobs, _ := body["jobs"].([]any); len(jobs) != 1 {
		t.Fatalf("list after release: jobs %v", body["jobs"])
	}
	resp, body = doReq(t, "GET", ts.URL+"/api/leases/"+id+"/jobs/"+jobID, "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get after release: %d %v", resp.StatusCode, body)
	}
	job, _ := body["job"].(map[string]any)
	if job["state"] != "lost" || job["reason"] != "lease_released" || job["ended_at"] == "" {
		t.Fatalf("archived job = %v", job)
	}
	if want := "mail: HELLO failed: connection refused\n"; body["stderr"] != want || job["stderr_tail"] != want {
		t.Fatalf("archived stderr %q, tail %q, want %q", body["stderr"], job["stderr_tail"], want)
	}
	if resp, _ := doReq(t, "GET", ts.URL+"/api/leases/"+id+"/jobs/nope", "token-a", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown job after release: %d, want 404", resp.StatusCode)
	}
	for _, path := range []string{"/jobs", "/jobs/" + jobID} {
		if resp, _ := doReq(t, "GET", ts.URL+"/api/leases/"+id+path, "token-b", nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("another owner's GET %s after release: %d, want 404", path, resp.StatusCode)
		}
	}

	svc.now = func() time.Time { return time.Now().Add(svc.jobRetention() + time.Hour) }
	svc.pruneJobs(context.Background())
	if resp, _ := doReq(t, "GET", ts.URL+"/api/leases/"+id+"/jobs/"+jobID, "token-a", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("pruned job: %d, want 404", resp.StatusCode)
	}
}
