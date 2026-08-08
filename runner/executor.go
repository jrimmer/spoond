package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	runnerv1 "gitea.dev/actions-proto-go/runner/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// LeaseClient is the subset of the lease API the executor needs.
type LeaseClient interface {
	Create(ctx context.Context, image string, ttl int) (string, error)
	Exec(ctx context.Context, id, cmd, cwd string, env map[string]string, timeout int) (*ExecResult, error)
	Delete(ctx context.Context, id string) error
}

// ExecResult is the result of a single exec call.
type ExecResult struct {
	Stdout string
	Stderr string
	Exit   int
}

// HTTPLeaseClient is a LeaseClient backed by the forkd-backend HTTP API.
type HTTPLeaseClient struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

// Create grants a new sandbox lease.
func (c *HTTPLeaseClient) Create(ctx context.Context, image string, ttl int) (string, error) {
	body, _ := json.Marshal(map[string]any{"image": image, "ttl": ttl})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/sandboxes", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("create sandbox: status %d", resp.StatusCode)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// Exec runs a command in a sandbox.
func (c *HTTPLeaseClient) Exec(ctx context.Context, id, cmd, cwd string, env map[string]string, timeout int) (*ExecResult, error) {
	body, _ := json.Marshal(map[string]any{"cmd": cmd, "cwd": cwd, "env": env, "timeout": timeout})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/sandboxes/"+id+"/exec", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("exec: status %d", resp.StatusCode)
	}
	var out ExecResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Delete releases a sandbox.
func (c *HTTPLeaseClient) Delete(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.BaseURL+"/api/sandboxes/"+id, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("delete sandbox: status %d", resp.StatusCode)
	}
	return nil
}

// ProtoClient is the subset of the Forgejo runner protocol the
// executor needs. *Client satisfies it; tests use a fake.
type ProtoClient interface {
	UpdateTask(ctx context.Context, state *runnerv1.TaskState, outputs map[string]string) error
	UpdateLog(ctx context.Context, taskID, index int64, rows []*runnerv1.LogRow, noMore bool) error
}

// Executor runs a workflow job in a forkd sandbox.
type Executor struct {
	Lease  LeaseClient
	Proto  ProtoClient
	Labels map[string]string // runs-on label -> image tag
	// DefaultImage is used when no label maps to an image.
	DefaultImage string
	// TTL is the sandbox lease TTL in seconds.
	TTL int
}

// Run executes a single task's job in a sandbox, streaming logs and
// reporting the final state. It always releases the sandbox.
func (e *Executor) Run(ctx context.Context, task *runnerv1.Task) error {
	wf, err := ParseWorkflow(task.GetWorkflowPayload())
	if err != nil {
		return e.fail(ctx, task, err)
	}
	// The payload is a single expanded job; pick the first job.
	var job *Job
	for _, j := range wf.Jobs {
		job = j
		break
	}
	if job == nil {
		return e.fail(ctx, task, fmt.Errorf("no job in workflow"))
	}

	image := e.imageFor(job)
	sandboxID, err := e.Lease.Create(ctx, image, e.TTL)
	if err != nil {
		return e.fail(ctx, task, fmt.Errorf("create sandbox: %w", err))
	}
	defer e.Lease.Delete(context.Background(), sandboxID)

	state := &runnerv1.TaskState{
		Id:        task.GetId(),
		Result:    runnerv1.Result_RESULT_SUCCESS,
		StartedAt: now(),
	}
	ctx2 := &EvalContext{
		GitHub:  map[string]string{"repository": task.GetContext().GetFields()["repository"].GetStringValue()},
		Env:     map[string]string{},
		Secrets: task.GetSecrets(),
		Vars:    task.GetVars(),
		Steps:   map[string]map[string]string{},
	}

	var logIndex int64
	for i, step := range job.Steps {
		stepState := &runnerv1.StepState{
			Id:        int64(i),
			Result:    runnerv1.Result_RESULT_SUCCESS,
			StartedAt: now(),
			LogIndex:  logIndex,
		}
		// Evaluate the step's run command with context.
		cmd := ctx2.Eval(step.Run)
		env := map[string]string{}
		for k, v := range step.Env {
			env[k] = ctx2.Eval(v)
		}
		res, err := e.Lease.Exec(ctx, sandboxID, cmd, "", env, 300)
		if err != nil {
			stepState.Result = runnerv1.Result_RESULT_FAILURE
			state.Result = runnerv1.Result_RESULT_FAILURE
			e.log(ctx, task, logIndex, "step failed: "+err.Error())
			logIndex++
			stepState.LogLength = 1
			stepState.StoppedAt = now()
			state.Steps = append(state.Steps, stepState)
			break
		}
		// Stream stdout/stderr as log rows.
		rows := splitLog(res.Stdout, res.Stderr)
		e.log(ctx, task, logIndex, strings.Join(rows, "\n"))
		logIndex += int64(len(rows))
		stepState.LogLength = int64(len(rows))
		stepState.StoppedAt = now()
		if res.Exit != 0 {
			stepState.Result = runnerv1.Result_RESULT_FAILURE
			state.Result = runnerv1.Result_RESULT_FAILURE
		}
		state.Steps = append(state.Steps, stepState)
		if res.Exit != 0 {
			break
		}
	}
	state.StoppedAt = now()
	return e.Proto.UpdateTask(ctx, state, nil)
}

// imageFor maps a job's runs-on labels to an image tag.
func (e *Executor) imageFor(job *Job) string {
	for _, label := range job.RunsOnLabels() {
		if img, ok := e.Labels[label]; ok {
			return img
		}
	}
	return e.DefaultImage
}

// log streams a log row to Forgejo.
func (e *Executor) log(ctx context.Context, task *runnerv1.Task, index int64, content string) {
	if content == "" {
		return
	}
	_ = e.Proto.UpdateLog(ctx, task.GetId(), index, []*runnerv1.LogRow{
		{Time: now(), Content: content},
	}, false)
}

// fail reports a task failure and returns the error.
func (e *Executor) fail(ctx context.Context, task *runnerv1.Task, err error) error {
	state := &runnerv1.TaskState{
		Id:        task.GetId(),
		Result:    runnerv1.Result_RESULT_FAILURE,
		StartedAt: now(),
		StoppedAt: now(),
	}
	_ = e.Proto.UpdateTask(ctx, state, nil)
	return err
}

// splitLog splits combined stdout/stderr into log lines.
func splitLog(stdout, stderr string) []string {
	combined := stdout
	if stderr != "" {
		combined += "\n" + stderr
	}
	var out []string
	for _, line := range strings.Split(combined, "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func now() *timestamppb.Timestamp {
	return timestamppb.New(time.Now())
}
