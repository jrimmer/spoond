// Package runner implements a Forgejo Actions runner that executes
// each job in a forkd sandbox obtained from the lease API.
package runner

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"
	runnerv1 "gitea.dev/actions-proto-go/runner/v1"
	"gitea.dev/actions-proto-go/runner/v1/runnerv1connect"
)

// Client is a thin wrapper over the Forgejo runner Connect RPC service.
type Client struct {
	svc runnerv1connect.RunnerServiceClient
}

// NewClient builds a runner protocol client against the Forgejo server.
// baseURL is the Forgejo instance URL (e.g. https://code.lacy.casa).
func NewClient(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{
		svc: runnerv1connect.NewRunnerServiceClient(hc, baseURL),
	}
}

// Register registers this runner with Forgejo and returns the runner id.
func (c *Client) Register(ctx context.Context, name, token string, labels []string, ephemeral bool) (int64, error) {
	resp, err := c.svc.Register(ctx, connect.NewRequest(&runnerv1.RegisterRequest{
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

// FetchTask polls for the next task. It blocks until a task is available
// or the context is cancelled.
func (c *Client) FetchTask(ctx context.Context, tasksVersion int64) (*runnerv1.Task, int64, error) {
	resp, err := c.svc.FetchTask(ctx, connect.NewRequest(&runnerv1.FetchTaskRequest{
		TasksVersion: tasksVersion,
	}))
	if err != nil {
		return nil, tasksVersion, fmt.Errorf("fetch task: %w", err)
	}
	return resp.Msg.GetTask(), resp.Msg.GetTasksVersion(), nil
}

// UpdateTask reports the task state (result, step states) to Forgejo.
func (c *Client) UpdateTask(ctx context.Context, state *runnerv1.TaskState, outputs map[string]string) error {
	_, err := c.svc.UpdateTask(ctx, connect.NewRequest(&runnerv1.UpdateTaskRequest{
		State:   state,
		Outputs: outputs,
	}))
	if err != nil {
		return fmt.Errorf("update task: %w", err)
	}
	return nil
}

// UpdateLog streams log rows for a task to Forgejo.
func (c *Client) UpdateLog(ctx context.Context, taskID, index int64, rows []*runnerv1.LogRow, noMore bool) error {
	_, err := c.svc.UpdateLog(ctx, connect.NewRequest(&runnerv1.UpdateLogRequest{
		TaskId: taskID,
		Index:  index,
		Rows:   rows,
		NoMore: noMore,
	}))
	if err != nil {
		return fmt.Errorf("update log: %w", err)
	}
	return nil
}
