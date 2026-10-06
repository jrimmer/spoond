package api

import (
	"context"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/metrics"
	"github.com/jrimmer/spoond/v2/substrate/fake"
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
