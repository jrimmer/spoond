package runner

import (
	"context"
	"fmt"
	"strings"
)

// Executor runs a job in a sandbox. It depends only on the ports
// (SandboxProvider, JobSink) and is transport-agnostic: the same core
// drives a Forgejo task, an exe.dev harness, or a pi/code-harness job.
type Executor struct {
	Sandbox SandboxProvider
	Sink    JobSink
	Labels  map[string]string // runs-on label -> image tag
	// DefaultImage is used when no label maps to an image.
	DefaultImage string
	// TTL is the sandbox lease TTL in seconds.
	TTL int
}

// Run executes a job's workflow in a sandbox, streaming logs and
// reporting the final state. It always releases the sandbox.
func (e *Executor) Run(ctx context.Context, job *Job) error {
	wf, err := ParseWorkflow(job.Workflow)
	if err != nil {
		return e.fail(ctx, job, err)
	}
	// The payload is a single expanded job; pick the first job.
	var wfJob *WorkflowJob
	for _, j := range wf.Jobs {
		wfJob = j
		break
	}
	if wfJob == nil {
		return e.fail(ctx, job, fmt.Errorf("no job in workflow"))
	}

	image := e.imageFor(wfJob)
	sandboxID, err := e.Sandbox.Create(ctx, image, e.TTL)
	if err != nil {
		return e.fail(ctx, job, fmt.Errorf("create sandbox: %w", err))
	}
	defer e.Sandbox.Delete(context.Background(), sandboxID)

	state := &JobState{
		ID:     job.ID,
		Result: ResultSuccess,
	}
	ctx2 := &EvalContext{
		GitHub:  job.Context,
		Env:     map[string]string{},
		Secrets: job.Secrets,
		Vars:    job.Vars,
		Steps:   map[string]map[string]string{},
	}

	var logIndex int64
	for i, step := range wfJob.Steps {
		stepState := &StepState{
			ID:       int64(i),
			Result:   ResultSuccess,
			LogIndex: logIndex,
		}
		// Evaluate the step's run command with context.
		cmd := ctx2.Eval(step.Run)
		env := map[string]string{}
		for k, v := range step.Env {
			env[k] = ctx2.Eval(v)
		}
		res, err := e.Sandbox.Exec(ctx, sandboxID, cmd, "", env, 300)
		if err != nil {
			stepState.Result = ResultFailure
			state.Result = ResultFailure
			e.log(ctx, job, logIndex, "step failed: "+err.Error())
			logIndex++
			stepState.LogLength = 1
			state.Steps = append(state.Steps, *stepState)
			break
		}
		// Stream stdout/stderr as log rows.
		rows := splitLog(res.Stdout, res.Stderr)
		e.log(ctx, job, logIndex, strings.Join(rows, "\n"))
		logIndex += int64(len(rows))
		stepState.LogLength = int64(len(rows))
		if res.Exit != 0 {
			stepState.Result = ResultFailure
			state.Result = ResultFailure
		}
		state.Steps = append(state.Steps, *stepState)
		if res.Exit != 0 {
			break
		}
	}
	return e.Sink.Report(ctx, state, nil)
}

// imageFor maps a job's runs-on labels to an image tag.
func (e *Executor) imageFor(job *WorkflowJob) string {
	for _, label := range job.RunsOnLabels() {
		if img, ok := e.Labels[label]; ok {
			return img
		}
	}
	return e.DefaultImage
}

// log streams a log row to the job sink.
func (e *Executor) log(ctx context.Context, job *Job, index int64, content string) {
	if content == "" {
		return
	}
	_ = e.Sink.Log(ctx, job.ID, index, []*LogRow{{Content: content}}, false)
}

// fail reports a job failure and returns the error.
func (e *Executor) fail(ctx context.Context, job *Job, err error) error {
	_ = e.Sink.Report(ctx, &JobState{ID: job.ID, Result: ResultFailure}, nil)
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
