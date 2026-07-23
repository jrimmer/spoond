package runner

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"
	pingv1 "gitea.dev/actions-proto-go/ping/v1"
	pingv1connect "gitea.dev/actions-proto-go/ping/v1/pingv1connect"
	runnerv1 "gitea.dev/actions-proto-go/runner/v1"
	runnerv1connect "gitea.dev/actions-proto-go/runner/v1/runnerv1connect"
)

// ForgejoClient wraps the connectrpc client for communicating with the
// Forgejo runner service. It implements the full runner protocol:
// Register, Declare, FetchTask, UpdateTask, and UpdateLog.
//
// The wire protocol is gRPC-over-HTTP/2 (connect.WithGRPC()), which is
// what Forgejo's runner endpoint expects at /runner.v1.RunnerService/*.
type ForgejoClient struct {
	httpClient *http.Client
	baseURL    string
	token      string // runner auth token (from Register response or .runner file)
	runner     runnerv1connect.RunnerServiceClient
}

// NewForgejoClient creates a connectrpc client for the Forgejo runner protocol.
// baseURL is the Forgejo instance URL (e.g. https://code.lacy.casa).
// token is the runner auth token obtained from Register; for the initial
// Register call itself, pass the registration token from the Forgejo admin UI.
func NewForgejoClient(baseURL, token string) *ForgejoClient {
	httpClient := &http.Client{
		Timeout: 0, // no overall timeout; FetchTask is a long-poll
		Transport: &http.Transport{
			MaxIdleConns:        10,
			IdleConnTimeout:     30 * time.Second,
			DisableCompression:  false,
			MaxIdleConnsPerHost: 5,
		},
	}
	return &ForgejoClient{
		httpClient: httpClient,
		baseURL:    baseURL,
		token:      token,
		runner: runnerv1connect.NewRunnerServiceClient(
			httpClient,
			baseURL,
			connect.WithGRPC(),
			connect.WithSendGzip(),
		),
	}
}

// Ping is a health-check RPC to verify connectivity to Forgejo.
func (c *ForgejoClient) Ping(ctx context.Context) (string, error) {
	client := pingv1connect.NewPingServiceClient(c.httpClient, c.baseURL)
	req := connect.NewRequest(&pingv1.PingRequest{Data: "hyper-runner"})
	resp, err := client.Ping(ctx, req)
	if err != nil {
		return "", fmt.Errorf("ping: %w", err)
	}
	return resp.Msg.GetData(), nil
}

// Register registers a new runner with the Forgejo instance.
// The token field should be the registration token from the Forgejo admin UI
// (not a runner auth token). On success, the response contains a runner UUID
// and a long-lived auth token that must be persisted and used for all
// subsequent RPCs.
func (c *ForgejoClient) Register(ctx context.Context, name, token string, labels []string, version string) (*runnerv1.RegisterResponse, error) {
	req := connect.NewRequest(&runnerv1.RegisterRequest{
		Name:    name,
		Token:   token,
		Labels:  labels,
		Version: version,
	})
	resp, err := c.runner.Register(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}
	// Persist the returned runner token for future calls.
	if resp.Msg.Runner != nil && resp.Msg.Runner.Token != "" {
		c.token = resp.Msg.Runner.Token
	}
	return resp.Msg, nil
}

// Declare announces the runner's version and labels to Forgejo. This is
// called after Register (and on reconnect) before entering the FetchTask loop.
func (c *ForgejoClient) Declare(ctx context.Context, version string, labels []string) (*runnerv1.DeclareResponse, error) {
	req := connect.NewRequest(&runnerv1.DeclareRequest{
		Version: version,
		Labels:  labels,
	})
	if c.token != "" {
		req.Header().Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.runner.Declare(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("declare: %w", err)
	}
	return resp.Msg, nil
}

// FetchTask requests the next available task for execution. This is a
// long-poll: the server holds the connection open until a task is available
// or a timeout occurs. The caller should set a generous deadline on ctx
// (e.g. 60s) and call FetchTask again if no task was returned.
//
// tasksVersion is an optimisation: pass the last-seen tasks_version from
// the previous response. If the server's version hasn't changed, it may
// return immediately with no task.
func (c *ForgejoClient) FetchTask(ctx context.Context, tasksVersion int64) (*runnerv1.FetchTaskResponse, error) {
	req := connect.NewRequest(&runnerv1.FetchTaskRequest{
		TasksVersion: tasksVersion,
	})
	if c.token != "" {
		req.Header().Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.runner.FetchTask(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("fetch task: %w", err)
	}
	return resp.Msg, nil
}

// UpdateTask reports task state (running, success, failure) back to Forgejo.
// The state includes step-level results and timestamps.
func (c *ForgejoClient) UpdateTask(ctx context.Context, state *runnerv1.TaskState, outputs map[string]string) (*runnerv1.UpdateTaskResponse, error) {
	req := connect.NewRequest(&runnerv1.UpdateTaskRequest{
		State:   state,
		Outputs: outputs,
	})
	if c.token != "" {
		req.Header().Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.runner.UpdateTask(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("update task: %w", err)
	}
	return resp.Msg, nil
}

// UpdateLog streams log lines back to Forgejo. Each call appends rows
// starting at the given index. Set noMore=true on the final batch.
func (c *ForgejoClient) UpdateLog(ctx context.Context, taskID int64, index int64, rows []*runnerv1.LogRow, noMore bool) (*runnerv1.UpdateLogResponse, error) {
	req := connect.NewRequest(&runnerv1.UpdateLogRequest{
		TaskId: taskID,
		Index:  index,
		Rows:   rows,
		NoMore: noMore,
	})
	if c.token != "" {
		req.Header().Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.runner.UpdateLog(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("update log: %w", err)
	}
	return resp.Msg, nil
}

// SetToken updates the runner auth token (e.g. after loading from .runner file).
func (c *ForgejoClient) SetToken(token string) {
	c.token = token
}

// Token returns the current runner auth token.
func (c *ForgejoClient) Token() string {
	return c.token
}