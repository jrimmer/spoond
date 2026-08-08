package runner

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	runnerv1 "gitea.dev/actions-proto-go/runner/v1"
	"gitea.dev/actions-proto-go/runner/v1/runnerv1connect"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ForgejoAdapter is a JobSource + JobSink backed by the Forgejo runner
// Connect RPC protocol. It converts between the domain Job/JobState
// types and the Forgejo proto types.
type ForgejoAdapter struct {
	svc runnerv1connect.RunnerServiceClient
}

// NewForgejoAdapter builds a Forgejo runner protocol adapter.
// baseURL is the Forgejo instance URL (e.g. https://code.lacy.casa).
// The runner protocol is mounted under /api/actions, matching the
// official forgejo-runner.
func NewForgejoAdapter(baseURL string, hc *http.Client) *ForgejoAdapter {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	baseURL = strings.TrimRight(baseURL, "/") + "/api/actions"
	return &ForgejoAdapter{
		svc: runnerv1connect.NewRunnerServiceClient(hc, baseURL),
	}
}

// Register registers this runner with Forgejo and returns the runner id.
func (a *ForgejoAdapter) Register(ctx context.Context, name, token string, labels []string, ephemeral bool) (int64, error) {
	resp, err := a.svc.Register(ctx, connect.NewRequest(&runnerv1.RegisterRequest{
		Name:      name,
		Token:     token,
		Labels:    labels,
		Ephemeral: ephemeral,
		Version:   "forkd-runner-v0.1",
	}))
	if err != nil {
		return 0, fmt.Errorf("register: %w", err)
	}
	if resp.Msg.GetRunner() == nil {
		return 0, fmt.Errorf("register: no runner returned")
	}
	return resp.Msg.GetRunner().GetId(), nil
}

// Fetch implements JobSource. It polls Forgejo for the next task and
// converts it to a domain Job.
func (a *ForgejoAdapter) Fetch(ctx context.Context, version int64) (*Job, int64, error) {
	resp, err := a.svc.FetchTask(ctx, connect.NewRequest(&runnerv1.FetchTaskRequest{
		TasksVersion: version,
	}))
	if err != nil {
		return nil, version, fmt.Errorf("fetch task: %w", err)
	}
	task := resp.Msg.GetTask()
	if task == nil {
		return nil, resp.Msg.GetTasksVersion(), nil
	}
	job := &Job{
		ID:       task.GetId(),
		Workflow: task.GetWorkflowPayload(),
		Secrets:  task.GetSecrets(),
		Vars:     task.GetVars(),
		Context:  structToMap(task.GetContext()),
	}
	return job, resp.Msg.GetTasksVersion(), nil
}

// Report implements JobSink. It converts a domain JobState to the
// Forgejo TaskState and reports it.
func (a *ForgejoAdapter) Report(ctx context.Context, state *JobState, outputs map[string]string) error {
	ts := &runnerv1.TaskState{
		Id:     state.ID,
		Result: resultToProto(state.Result),
	}
	for _, s := range state.Steps {
		ts.Steps = append(ts.Steps, &runnerv1.StepState{
			Id:        s.ID,
			Result:    resultToProto(s.Result),
			LogIndex:  s.LogIndex,
			LogLength: s.LogLength,
		})
	}
	_, err := a.svc.UpdateTask(ctx, connect.NewRequest(&runnerv1.UpdateTaskRequest{
		State:   ts,
		Outputs: outputs,
	}))
	if err != nil {
		return fmt.Errorf("update task: %w", err)
	}
	return nil
}

// Log implements JobSink. It streams log rows to Forgejo.
func (a *ForgejoAdapter) Log(ctx context.Context, jobID, index int64, rows []*LogRow, noMore bool) error {
	protoRows := make([]*runnerv1.LogRow, 0, len(rows))
	for _, r := range rows {
		protoRows = append(protoRows, &runnerv1.LogRow{
			Time:    timestamppb.New(r.Time),
			Content: r.Content,
		})
	}
	_, err := a.svc.UpdateLog(ctx, connect.NewRequest(&runnerv1.UpdateLogRequest{
		TaskId: jobID,
		Index:  index,
		Rows:   protoRows,
		NoMore: noMore,
	}))
	if err != nil {
		return fmt.Errorf("update log: %w", err)
	}
	return nil
}

// resultToProto maps a domain Result to the Forgejo proto Result.
func resultToProto(r Result) runnerv1.Result {
	switch r {
	case ResultSuccess:
		return runnerv1.Result_RESULT_SUCCESS
	case ResultFailure:
		return runnerv1.Result_RESULT_FAILURE
	case ResultCancelled:
		return runnerv1.Result_RESULT_CANCELLED
	case ResultSkipped:
		return runnerv1.Result_RESULT_SKIPPED
	}
	return runnerv1.Result_RESULT_UNSPECIFIED
}

// structToMap flattens a protobuf Struct into a string map.
func structToMap(s *structpb.Struct) map[string]string {
	if s == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(s.GetFields()))
	for k, v := range s.GetFields() {
		out[k] = v.GetStringValue()
	}
	return out
}
