package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
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
	// runnerUUID and runnerToken are returned at registration and must
	// be sent as headers on subsequent calls.
	runnerUUID  string
	runnerToken string
	// internalBaseURL, when set, overrides github.api_url and
	// github.server_url in the job Context so workflows talk to the
	// internal Forgejo address (never via the public/Pangolin route).
	internalBaseURL string
}

// NewForgejoAdapter builds a Forgejo runner protocol adapter.
// baseURL is the Forgejo instance URL (e.g. https://code.lacy.casa).
// The runner protocol is mounted under /api/actions, matching the
// official forgejo-runner.
func NewForgejoAdapter(baseURL string, hc *http.Client) *ForgejoAdapter {
	return NewForgejoAdapterWithInternal(baseURL, "", hc)
}

// NewForgejoAdapterWithInternal is like NewForgejoAdapter but also
// accepts an internal base URL used to override github.api_url and
// github.server_url in the job Context.
func NewForgejoAdapterWithInternal(baseURL, internalBaseURL string, hc *http.Client) *ForgejoAdapter {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	baseURL = strings.TrimRight(baseURL, "/") + "/api/actions"
	svc := runnerv1connect.NewRunnerServiceClient(hc, baseURL,
		connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				// Attach runner auth headers if present.
				if a := authFrom(ctx); a != nil {
					for k, v := range a {
						req.Header().Set(k, v)
					}
				}
				return next(ctx, req)
			}
		})),
	)
	return &ForgejoAdapter{svc: svc, internalBaseURL: strings.TrimRight(internalBaseURL, "/")}
}

// authCtxKey is the context key for runner auth headers.
type authCtxKey struct{}

// withAuth returns a context carrying the runner auth headers.
func withAuth(ctx context.Context, headers map[string]string) context.Context {
	return context.WithValue(ctx, authCtxKey{}, headers)
}

// authFrom returns the runner auth headers from ctx, or nil.
func authFrom(ctx context.Context) map[string]string {
	v, _ := ctx.Value(authCtxKey{}).(map[string]string)
	return v
}

// Register registers this runner with Forgejo and returns the runner id.
// The returned UUID and token are stored and sent as headers on
// subsequent protocol calls.
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
	a.runnerUUID = resp.Msg.GetRunner().GetUuid()
	a.runnerToken = resp.Msg.GetRunner().GetToken()
	return resp.Msg.GetRunner().GetId(), nil
}

// authHeaders returns the headers needed to authenticate subsequent
// protocol calls as the registered runner.
func (a *ForgejoAdapter) authHeaders() map[string]string {
	return map[string]string{
		"x-runner-uuid":  a.runnerUUID,
		"x-runner-token": a.runnerToken,
	}
}

// Fetch implements JobSource. It polls Forgejo for the next task and
// converts it to a domain Job.
func (a *ForgejoAdapter) Fetch(ctx context.Context, version int64) (*Job, int64, error) {
	ctx = withAuth(ctx, a.authHeaders())
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
	ctxMap := structToMap(task.GetContext())
	// Override github.api_url / github.server_url to the internal base
	// URL so workflows never route through the public/Pangolin address.
	// Forgejo's context may expose these both as top-level keys
	// (repository, api_url, server_url — what EvalContext.lookup reads
	// for `${{ github.server_url }}`) and/or as prefixed keys
	// (github.server_url). Set both forms unconditionally so reviewdog's
	// GITEA_ADDRESS and similar resolve to the internal host, not
	// code.lacy.casa (which 302-redirects through Pangolin to HTML).
	if a.internalBaseURL != "" {
		ctxMap["github.api_url"] = a.internalBaseURL
		ctxMap["github.server_url"] = a.internalBaseURL
		ctxMap["api_url"] = a.internalBaseURL
		ctxMap["server_url"] = a.internalBaseURL
	}
	job := &Job{
		ID:       task.GetId(),
		Workflow: task.GetWorkflowPayload(),
		Secrets:  task.GetSecrets(),
		Vars:     task.GetVars(),
		Context:  ctxMap,
	}
	return job, resp.Msg.GetTasksVersion(), nil
}

// Report implements JobSink. It converts a domain JobState to the
// Forgejo TaskState and reports it.
func (a *ForgejoAdapter) Report(ctx context.Context, state *JobState, outputs map[string]string) error {
	ctx = withAuth(ctx, a.authHeaders())
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
	ctx = withAuth(ctx, a.authHeaders())
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

// structToMap flattens a protobuf Struct into a string map, recursing
// into nested Structs/Values so dot-notation lookups like
// github.event.pull_request.number resolve. Nested objects become
// dot-notation keys; scalar values become their string form.
func structToMap(s *structpb.Struct) map[string]string {
	out := map[string]string{}
	if s == nil {
		return out
	}
	var walk func(prefix string, v *structpb.Value)
	walk = func(prefix string, v *structpb.Value) {
		switch k := v.GetKind().(type) {
		case *structpb.Value_StructValue:
			for fk, fv := range k.StructValue.GetFields() {
				key := fk
				if prefix != "" {
					key = prefix + "." + fk
				}
				walk(key, fv)
			}
		case *structpb.Value_StringValue:
			out[prefix] = k.StringValue
		case *structpb.Value_NumberValue:
			out[prefix] = strconv.FormatFloat(k.NumberValue, 'f', -1, 64)
		case *structpb.Value_BoolValue:
			out[prefix] = strconv.FormatBool(k.BoolValue)
		case *structpb.Value_ListValue:
			// Represent lists as JSON for simplicity.
			if b, err := json.Marshal(k.ListValue.AsSlice()); err == nil {
				out[prefix] = string(b)
			}
		}
	}
	for k, v := range s.GetFields() {
		walk(k, v)
	}
	return out
}
