package runner

import (
	"context"
	"fmt"
	"regexp"
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
	// RepoBaseURL is the git host base URL used to construct clone URLs
	// for actions/checkout (e.g. https://code.lacy.casa). The repo path
	// comes from the github.repository context.
	RepoBaseURL string
	// WorkspaceDir is the directory inside the sandbox where the repo is
	// checked out and run steps execute. Defaults to /workspace.
	WorkspaceDir string
}

// checkoutRe matches a uses: actions/checkout step (any version).
var checkoutRe = regexp.MustCompile(`(?i)^actions/checkout(@.*)?$`)

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

	// Workspace where the repo is checked out and run steps execute.
	ws := e.WorkspaceDir
	if ws == "" {
		ws = "/workspace"
	}
	// checkedOut tracks whether a checkout step has run; run steps only
	// use the workspace as cwd once the repo is present there.
	checkedOut := false

	var logIndex int64
	for i, step := range wfJob.Steps {
		stepState := &StepState{
			ID:       int64(i),
			Result:   ResultSuccess,
			LogIndex: logIndex,
		}
		// actions/checkout: clone the repo into the workspace.
		if step.Uses != "" && checkoutRe.MatchString(step.Uses) {
			if err := e.checkout(ctx, sandboxID, ws, job, ctx2, stepState, &logIndex); err != nil {
				state.Result = ResultFailure
				state.Steps = append(state.Steps, *stepState)
				break
			}
			checkedOut = true
			state.Steps = append(state.Steps, *stepState)
			continue
		}
		// Evaluate the step's run command with context.
		cmd := ctx2.Eval(step.Run)
		env := map[string]string{}
		// Job-level env first (lower precedence), then step-level env
		// (higher precedence) — matches GitHub Actions semantics.
		for k, v := range wfJob.Env {
			env[k] = ctx2.Eval(v)
		}
		for k, v := range step.Env {
			env[k] = ctx2.Eval(v)
		}
		// Debug: log which env keys are set (values redacted for secrets).
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		e.log(ctx, job, logIndex, "step env keys: "+strings.Join(keys, ","))
		logIndex++
		// Debug: log which secret keys are available (values redacted).
		sk := make([]string, 0, len(job.Secrets))
		for k := range job.Secrets {
			sk = append(sk, k)
		}
		e.log(ctx, job, logIndex, "job secret keys: "+strings.Join(sk, ","))
		logIndex++
		// Debug: log resolved value lengths for the review-critical vars.
		e.log(ctx, job, logIndex, fmt.Sprintf("resolved: FORGEJO_TOKEN=%d FORGEJO_API=%d PR_NUMBER=%d LLM_API_KEY=%d",
			len(env["FORGEJO_TOKEN"]), len(env["FORGEJO_API"]), len(env["PR_NUMBER"]), len(env["LLM_API_KEY"])))
		logIndex++
		// Use the workspace as cwd only if the repo was checked out.
		cwd := ""
		if checkedOut {
			cwd = ws
		}
		res, err := e.Sandbox.Exec(ctx, sandboxID, cmd, cwd, env, 300)
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

// checkout clones the job's repository into the workspace inside the
// sandbox. It is the forkd equivalent of actions/checkout: the runner
// provides the environment, the workflow asks for the code.
func (e *Executor) checkout(ctx context.Context, sandboxID, ws string, job *Job, ctx2 *EvalContext, stepState *StepState, logIndex *int64) error {
	repo := ctx2.Eval("${{ github.repository }}")
	if repo == "" {
		stepState.Result = ResultFailure
		e.log(ctx, job, *logIndex, "checkout: github.repository is empty")
		*logIndex++
		stepState.LogLength = 1
		return fmt.Errorf("checkout: github.repository is empty")
	}
	base := e.RepoBaseURL
	if base == "" {
		base = "https://code.lacy.casa"
	}
	base = strings.TrimRight(base, "/")
	cloneURL := base + "/" + repo + ".git"

	// Auth via GITHUB_TOKEN (provided by Forgejo in job secrets), sent
	// as an extra header so the token never appears in the URL.
	token := ctx2.Eval("${{ secrets.GITHUB_TOKEN }}")
	authArg := ""
	if token != "" {
		authArg = `-c http.extraheader="Authorization: token ` + token + `"`
	}

	// Ensure the workspace exists and is clean, then clone into it.
	// The warm pool reuses the same rootfs across jobs, so /workspace may
	// hold a previous job's checkout (and its _build artifacts). Remove it
	// first so `git clone` never fails with "destination path already exists".
	cmds := []string{
		"rm -rf " + ws,
		"mkdir -p " + ws,
		"git " + authArg + " clone --depth 1 " + cloneURL + " " + ws,
	}
	for _, c := range cmds {
		res, err := e.Sandbox.Exec(ctx, sandboxID, c, "", nil, 300)
		if err != nil {
			stepState.Result = ResultFailure
			e.log(ctx, job, *logIndex, "checkout: "+err.Error())
			*logIndex++
			stepState.LogLength = 1
			return err
		}
		rows := splitLog(res.Stdout, res.Stderr)
		if len(rows) > 0 {
			e.log(ctx, job, *logIndex, strings.Join(rows, "\n"))
			*logIndex += int64(len(rows))
			stepState.LogLength += int64(len(rows))
		}
		if res.Exit != 0 {
			stepState.Result = ResultFailure
			return fmt.Errorf("checkout: command failed: %s", c)
		}
	}
	return nil
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
