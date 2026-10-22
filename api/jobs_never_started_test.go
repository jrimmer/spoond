package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v4/store"
	"github.com/jrimmer/spoond/v4/substrate"
	"github.com/jrimmer/spoond/v4/substrate/fake"
)

// TestJobNeverStartedMarkedLost: the start answers 202, then its stream
// breaks (a 502 from the guest proxy) before the guest reports the
// process started, and the command never runs: no pid file, no rc. The
// reconcile marks the job lost with reason never_started once the grace
// has passed, instead of leaving it running (and its lease awake) until
// the max runtime.
func TestJobNeverStartedMarkedLost(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, _ := createJobLease(t, ts, svc)

	p := fake.NewProcess(1101)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})
	p.Push(substrate.ProcessEvent{Kind: substrate.EventError, Err: "unavailable: HTTP status 502 Bad Gateway"})
	waitFor(t, "the start error recorded", jobWatchTimeout, func() bool {
		svc.timingOutMu.Lock()
		defer svc.timingOutMu.Unlock()
		return svc.jobStartErr[jobID] != ""
	})

	base := time.Now()
	t.Cleanup(func() { svc.now = time.Now })

	// Inside the grace: still running, and the lease is kept awake.
	svc.now = func() time.Time { return base }
	svc.reconcileJobs(context.Background())
	svc.now = func() time.Time { return base.Add(jobNeverStartedGrace / 2) }
	svc.reconcileJobs(context.Background())
	if row, _ := db.GetJob(context.Background(), jobID); row.State != "running" {
		t.Fatalf("job state = %q inside the grace, want running", row.State)
	}

	svc.now = func() time.Time { return base.Add(jobNeverStartedGrace + time.Second) }
	svc.reconcileJobs(context.Background())
	row, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if row.State != "lost" || row.Reason != store.JobReasonNeverStarted {
		t.Fatalf("job = %s/%q, want lost/%s", row.State, row.Reason, store.JobReasonNeverStarted)
	}
	if svc.hasRunningJob(id) {
		t.Fatalf("lease still counts a running job")
	}

	// The API says why, so a client following the job stops waiting.
	_, body := doReq(t, "GET", ts.URL+"/api/leases/"+id+"/jobs/"+jobID, "token-a", nil)
	job, _ := body["job"].(map[string]any)
	if job["state"] != "lost" || job["reason"] != store.JobReasonNeverStarted {
		t.Fatalf("GET job = %v", job)
	}
	if detail := svc.neverStartedDetail(jobID); !strings.Contains(detail, "502") {
		t.Fatalf("detail %q lacks the start error", detail)
	}
}

// TestJobWithPidNotMarkedNeverStarted: a job whose wrapper wrote its pid
// file is running, however long it runs and whatever its stream did:
// only the rc or the max runtime ends it.
func TestJobWithPidNotMarkedNeverStarted(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)

	p := fake.NewProcess(1102)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})
	writeJobFile(t, sub, sandbox, jobID, "pid", "1102\n")
	p.Push(substrate.ProcessEvent{Kind: substrate.EventError, Err: "unavailable: HTTP status 502 Bad Gateway"})

	base := time.Now()
	t.Cleanup(func() { svc.now = time.Now })
	for _, d := range []time.Duration{0, jobNeverStartedGrace + time.Second, 10 * jobNeverStartedGrace} {
		svc.now = func() time.Time { return base.Add(d) }
		svc.reconcileJobs(context.Background())
	}
	if row, _ := db.GetJob(context.Background(), jobID); row.State != "running" {
		t.Fatalf("job with a pid file = %s/%q, want running", row.State, row.Reason)
	}
}

// TestJobNeverStartedWaitsForGuest: while the guest does not answer (a
// sandbox the orchestrator lost for a moment reads as not found too),
// a missing pid file is no evidence, so the job stays running.
func TestJobNeverStartedWaitsForGuest(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)

	p := fake.NewProcess(1103)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})

	sub.Kill(sandbox)
	base := time.Now()
	t.Cleanup(func() { svc.now = time.Now })
	for _, d := range []time.Duration{0, 2 * jobNeverStartedGrace} {
		svc.now = func() time.Time { return base.Add(d) }
		svc.reconcileJobs(context.Background())
	}
	if row, _ := db.GetJob(context.Background(), jobID); row.State != "running" {
		t.Fatalf("job = %s/%q while the guest is unreachable, want running", row.State, row.Reason)
	}
}
