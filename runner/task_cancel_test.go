package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	runnerv1 "gitea.dev/actions-proto-go/runner/v1"
	"gitea.dev/actions-proto-go/runner/v1/runnerv1connect"
)

// cancelledSink answers every keepalive that the server cancelled the
// task, the way Forgejo does after a newer push supersedes it.
type cancelledSink struct{ fakeSink }

func (s *cancelledSink) Keepalive(ctx context.Context, jobID int64) error {
	return ErrTaskCancelled
}

// TestExecutorStopsServerCancelledStep (#160): a step in flight when the
// server cancels the task stops at the next keepalive, the job reports
// cancelled and its lease is released, instead of running to the end on
// a revoked token.
func TestExecutorStopsServerCancelledStep(t *testing.T) {
	lease := &blockingLease{fakeLease: *newFakeLease(), started: make(chan struct{})}
	sink := &cancelledSink{}
	exec := &Executor{
		Sandbox:               lease,
		Sink:                  sink,
		Labels:                map[string]string{"ubuntu-latest": "py-base"},
		DefaultImage:          "py-base",
		TTL:                   600,
		StepKeepaliveInterval: 10 * time.Millisecond,
	}
	done := make(chan error, 1)
	go func() {
		done <- exec.Run(context.Background(), testJob("jobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: sleep 600\n"))
	}()
	<-lease.started
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the job ran on after the server cancelled it")
	}
	if len(sink.reports) != 1 || sink.reports[0].Result != ResultCancelled {
		t.Fatalf("reports = %+v, want one cancelled", sink.reports)
	}
	if len(lease.deleted) != 1 {
		t.Fatalf("deleted = %v, want the job's lease released", lease.deleted)
	}
}

// TestExecutorStopsServerCancelledCreateWait (#160): a job still waiting
// for a sandbox when the server cancels it stops waiting.
func TestExecutorStopsServerCancelledCreateWait(t *testing.T) {
	lease := &blockingCreateLease{fakeLease: *newFakeLease(), started: make(chan struct{})}
	sink := &cancelledSink{}
	exec := &Executor{
		Sandbox:                 lease,
		Sink:                    sink,
		Labels:                  map[string]string{"ubuntu-latest": "py-base"},
		DefaultImage:            "py-base",
		TTL:                     600,
		CreateKeepaliveInterval: 10 * time.Millisecond,
		CreateLogInterval:       time.Hour,
	}
	done := make(chan error, 1)
	go func() {
		done <- exec.Run(context.Background(), testJob("jobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n"))
	}()
	<-lease.started
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the create kept waiting after the server cancelled the job")
	}
	if len(sink.reports) != 1 || sink.reports[0].Result != ResultCancelled {
		t.Fatalf("reports = %+v, want one cancelled", sink.reports)
	}
}

// updateTaskSvc answers UpdateTask with a fixed result.
type updateTaskSvc struct {
	runnerv1connect.RunnerServiceClient
	result runnerv1.Result
}

func (s *updateTaskSvc) UpdateTask(ctx context.Context, req *connect.Request[runnerv1.UpdateTaskRequest]) (*connect.Response[runnerv1.UpdateTaskResponse], error) {
	return connect.NewResponse(&runnerv1.UpdateTaskResponse{
		State: &runnerv1.TaskState{Id: req.Msg.GetState().GetId(), Result: s.result},
	}), nil
}

// TestKeepaliveReportsServerCancel (#160): the adapter turns Forgejo's
// "this task is cancelled" answer into ErrTaskCancelled, and a running
// task's answer into nil.
func TestKeepaliveReportsServerCancel(t *testing.T) {
	a := &ForgejoAdapter{svc: &updateTaskSvc{result: runnerv1.Result_RESULT_CANCELLED}}
	if err := a.Keepalive(context.Background(), 7); !errors.Is(err, ErrTaskCancelled) {
		t.Fatalf("keepalive on a cancelled task = %v, want ErrTaskCancelled", err)
	}
	a = &ForgejoAdapter{svc: &updateTaskSvc{result: runnerv1.Result_RESULT_UNSPECIFIED}}
	if err := a.Keepalive(context.Background(), 7); err != nil {
		t.Fatalf("keepalive on a running task = %v, want nil", err)
	}
}
