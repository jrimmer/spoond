package runner

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jrimmer/spoond/v2/metrics"
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
	// JobTimeout bounds the whole job (including waiting for capacity on
	// Create). Zero leaves the caller's context in charge; the pool's
	// job context (RUNNER_STOP_GRACE) is the normal bound. Set via
	// RUNNER_JOB_TIMEOUT.
	JobTimeout time.Duration
	// RepoBaseURL is the git host base URL used to construct clone URLs
	// for actions/checkout (e.g. https://code.example.com). The repo path
	// comes from the github.repository context. Required for checkout.
	RepoBaseURL string
	// WorkspaceDir is the directory inside the sandbox where the repo is
	// checked out and run steps execute. Defaults to /workspace.
	WorkspaceDir string
	// StepTimeout is the per-step exec timeout in seconds. Defaults to
	// 300 when zero. Compile-heavy CI steps (a full mix release, cargo
	// builds) routinely exceed the old hardcoded 300s — set via
	// EXEC_TIMEOUT_SECS on the runner.
	StepTimeout int
	// Metrics (issue #20): runner Prometheus metrics. Nil = no metrics.
	Metrics *metrics.RunnerMetrics
	// RecordDir is where failed jobs are recorded as JSON (one file per
	// job). Empty disables recording. Set via JOB_RECORD_DIR.
	RecordDir string
	// ForgejoURL is the Forgejo instance base URL (e.g.
	// https://code.example.com). When set, the lease the job runs in is
	// labelled with the job (#119): its comment is
	// "forgejo job <id> <job URL>" — see the LeaseLabeler port in
	// ports.go. Empty disables the label.
	ForgejoURL string
}

// checkoutRe matches a uses: actions/checkout step (any version).
var checkoutRe = regexp.MustCompile(`(?i)^actions/checkout(@.*)?$`)

// Run executes a job's workflow in a sandbox, streaming logs and
// reporting the final state. It always releases the sandbox.
func (e *Executor) Run(ctx context.Context, job *Job) error {
	if e.JobTimeout > 0 {
		// Bound the whole job, Create's capacity wait included: a runner
		// that dropped its job timeout would let a waiting create hold a
		// worker forever. The deferred cancel runs after the final report
		// (report uses a fresh context once this one is dead).
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.JobTimeout)
		defer cancel()
	}
	jobStart := time.Now()
	if e.Metrics != nil {
		e.Metrics.JobsActive.Inc()
		defer e.Metrics.JobsActive.Dec()
	}
	// Label the lease with the job before creating it (#119): the
	// comment names the job and links to it, and the orphan sweep at the
	// next start finds a dead runner's leases by it. A comment, not a
	// holder: a holder would make the lease held, and held leases are
	// checkpointed periodically, pausing the job's sandbox.
	if l, ok := e.Sandbox.(LeaseLabeler); ok && e.ForgejoURL != "" {
		l.WithLabel(fmt.Sprintf("%s%d %s/actions/runs/%d/jobs/%d",
			JobLabelPrefix, job.ID, strings.TrimRight(e.ForgejoURL, "/"), e.runID(job), job.ID))
	}
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

	state := &JobState{
		ID:     job.ID,
		Result: ResultSuccess,
	}
	// Release the lease with the job's outcome and duration (2.5, #132
	// part 2), so the lease's `released` event says why it went. A
	// provider without the LeaseReleaser capability gets the plain
	// delete.
	defer func() {
		reason := releaseReason(job, state.Result, time.Since(jobStart))
		if r, ok := e.Sandbox.(LeaseReleaser); ok {
			if err := r.DeleteReason(context.Background(), sandboxID, reason); err != nil {
				log.Printf("executor: job %d release with reason: %v", job.ID, err)
			}
			return
		}
		_ = e.Sandbox.Delete(context.Background(), sandboxID)
	}()
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
	// The step that failed, with its output tail, held for the failure
	// record — the sink streams logs to Forgejo and keeps nothing readable.
	var failed *StepState
	var failedStdout, failedStderr string
	for i, step := range wfJob.Steps {
		stepState := &StepState{
			ID:       int64(i),
			Name:     stepName(&step),
			Result:   ResultSuccess,
			LogIndex: logIndex,
		}
		// actions/checkout: clone the repo into the workspace.
		if step.Uses != "" && checkoutRe.MatchString(step.Uses) {
			if err := e.checkout(ctx, sandboxID, ws, job, ctx2, stepState, &logIndex); err != nil {
				state.Result = ResultFailure
				stepState.Result = ResultFailure
				stepState.Exit = -1
				state.Steps = append(state.Steps, *stepState)
				failed = stepState
				failedStderr = err.Error()
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
		// Inject the generic CI repo/commit vars that tools like
		// reviewdog's gitea reporter require (it looks for
		// CI_REPO_OWNER/CI_REPO_NAME/CI_COMMIT/CI_PULL_REQUEST).
		// GitHub Actions sets these implicitly; our runner must too so
		// `-reporter=gitea-pr-review` can resolve owner/repo/PR.
		// Step-level env wins, so explicit overrides are honoured.
		if v := ctx2.Eval("${{ github.repository }}"); v != "" {
			if owner, name, ok := strings.Cut(v, "/"); ok {
				if env["CI_REPO_OWNER"] == "" {
					env["CI_REPO_OWNER"] = owner
				}
				if env["CI_REPO_NAME"] == "" {
					env["CI_REPO_NAME"] = name
				}
			}
		}
		if env["CI_COMMIT"] == "" {
			env["CI_COMMIT"] = ctx2.Eval("${{ github.sha }}")
		}
		// CI=true signals "non-interactive" to every tool (pnpm and friends
		// otherwise prompt on /dev/console — which never EOFs — and hang
		// forever; cost us a full debugging cycle on the cytale pipeline).
		if env["CI"] == "" {
			env["CI"] = "true"
		}
		// GitHub Actions always provides PATH; the guest agent REPLACES the
		// environment with the step's env map when one is supplied, and its
		// own default PATH is not applied to that case — so a step env
		// without PATH loses /usr/bin entirely (broken tools in
		// otherwise-green jobs).
		if env["PATH"] == "" {
			env["PATH"] = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
		}
		// Corepack aborts (exit 1) when it must fetch the packageManager-
		// pinned tool and no TTY is available for its download prompt. The
		// sandbox has registry egress, so let it download silently.
		if env["COREPACK_ENABLE_DOWNLOAD_PROMPT"] == "" {
			env["COREPACK_ENABLE_DOWNLOAD_PROMPT"] = "0"
		}
		// Identity vars vanish with the same env replacement. Tests that
		// derive the local login name (OpenSSH interop gate) abort with
		// "neither USER nor LOGNAME is set". CI sandboxes exec as root.
		if env["USER"] == "" {
			env["USER"] = "root"
		}
		if env["LOGNAME"] == "" {
			env["LOGNAME"] = "root"
		}
		// GitHub Actions exposes GITHUB_RUN_ID and friends implicitly;
		// without them, workflows that key per-run state (e.g. a test
		// keyspace named cytale_ci_${GITHUB_RUN_ID:-local}) silently fall
		// back to their default — and EVERY run then shares one state
		// bucket: cancelled runs leave rows that poison the next suite
		// (clashing state between jobs, same-sha drift).
		// Preference: context run_id, then run_number, then the task id
		// (unique per attempt, so even a re-run isolates its state).
		if env["GITHUB_RUN_ID"] == "" {
			env["GITHUB_RUN_ID"] = firstNonEmpty(
				ctx2.Eval("${{ github.run_id }}"),
				ctx2.Eval("${{ github.run_number }}"),
				strconv.FormatInt(job.ID, 10))
		}
		if env["GITHUB_RUN_NUMBER"] == "" {
			env["GITHUB_RUN_NUMBER"] = firstNonEmpty(
				ctx2.Eval("${{ github.run_number }}"),
				strconv.FormatInt(job.ID, 10))
		}
		if env["GITHUB_SHA"] == "" {
			env["GITHUB_SHA"] = ctx2.Eval("${{ github.sha }}")
		}
		if env["GITHUB_REF"] == "" {
			env["GITHUB_REF"] = ctx2.Eval("${{ github.ref }}")
		}
		if env["CI_PULL_REQUEST"] == "" {
			env["CI_PULL_REQUEST"] = ctx2.Eval("${{ github.event.pull_request.number }}")
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
		stepTimeout := e.StepTimeout
		if stepTimeout <= 0 {
			stepTimeout = 300
		}
		// Heartbeat while the step runs: Forgejo reaps tasks that stop
		// reporting, and a long silent step (a full mix release, a cargo
		// build) emits no log rows until it completes — the task gets
		// declared failed server-side after ~13 minutes of silence.
		// Keepalive rows use the step's starting index; the goroutine
		// must not touch logIndex (owned by this loop).
		kaDone := make(chan struct{})
		go func() {
			t := time.NewTicker(60 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-kaDone:
					return
				case <-t.C:
				}
				_ = e.Sink.Keepalive(ctx, job.ID)
			}
		}()
		res, err := e.Sandbox.Exec(ctx, sandboxID, cmd, cwd, env, stepTimeout)
		close(kaDone)
		if err != nil {
			if e.Metrics != nil {
				e.Metrics.ExecErrors.WithLabelValues("500").Inc()
			}
			stepState.Result = ResultFailure
			stepState.Exit = -1
			state.Result = ResultFailure
			e.log(ctx, job, logIndex, "step failed: "+err.Error())
			logIndex++
			stepState.LogLength = 1
			state.Steps = append(state.Steps, *stepState)
			failed = stepState
			failedStderr = err.Error()
			break
		}
		// Stream stdout/stderr as log rows.
		rows := splitLog(res.Stdout, res.Stderr)
		logIndex += e.logLines(ctx, job, logIndex, rows)
		stepState.LogLength = int64(len(rows))
		stepState.Exit = res.Exit
		if res.Exit != 0 {
			stepState.Result = ResultFailure
			state.Result = ResultFailure
			e.log(ctx, job, logIndex, fmt.Sprintf("step exited %d (stderr tail: %s)", res.Exit, tailStr(res.Stderr, 300)))
			logIndex++
			stepState.LogLength++
			failed = stepState
			failedStdout, failedStderr = res.Stdout, res.Stderr
		}
		state.Steps = append(state.Steps, *stepState)
		e.log(ctx, job, logIndex, fmt.Sprintf("step %d (%s) exit=%d", i, stepName(&step), res.Exit))
		logIndex++
		stepState.LogLength++
		if res.Exit != 0 {
			break
		}
	}
	// A dead job context at the end of the loop means the job was
	// cancelled, not failed: the runner's graceful stop spent
	// RUNNER_STOP_GRACE and pulled the plug. Record that as the job's
	// result so Forgejo shows a cancelled task instead of a red one.
	if ctx.Err() != nil {
		state.Result = ResultCancelled
	}
	// Name the failing step in the runner's own journal: this line is what an
	// operator sees on the host, and "result=1 steps=5" alone does not say
	// which step died or why.
	if state.Result == ResultFailure && failed != nil {
		log.Printf("executor: job %d final result=%d steps=%d failed_step=%d(%s) exit=%d",
			job.ID, int(state.Result), len(state.Steps), failed.ID, failed.Name, failed.Exit)
	} else {
		log.Printf("executor: job %d final result=%d steps=%d", job.ID, int(state.Result), len(state.Steps))
	}
	if state.Result == ResultFailure {
		writeJobRecord(e.RecordDir, jobRecord(job, state, failed, failedStdout, failedStderr, time.Since(jobStart)))
	}
	if e.Metrics != nil {
		result := "success"
		switch state.Result {
		case ResultFailure:
			result = "failure"
		case ResultCancelled, ResultSkipped:
			result = "cancelled"
		}
		e.Metrics.JobsTotal.WithLabelValues(result).Inc()
		e.Metrics.JobDur.Observe(time.Since(jobStart).Seconds())
	}
	return e.report(ctx, state, nil)
}

// reportTimeout bounds a final report made after the job's own context
// died: the runner must not hang on a wedged sink while the shutdown
// grace (and systemd's TimeoutStopSec) tick down.
const reportTimeout = 15 * time.Second

// report sends the job's final state to the sink. When ctx is already
// dead — the runner's graceful stop cancelled the job after
// RUNNER_STOP_GRACE — the report goes out on a fresh bounded context
// instead: reporting into the dead context would fail outright and
// Forgejo would never hear the final state, keeping the task running
// until its stale-task reaper stops it.
func (e *Executor) report(ctx context.Context, state *JobState, outputs map[string]string) error {
	if ctx.Err() != nil {
		rctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
		defer cancel()
		ctx = rctx
	}
	return e.Sink.Report(ctx, state, outputs)
}

// stepName returns a short label for a step for logging.
func stepName(step *Step) string {
	if step.Uses != "" {
		return "uses:" + step.Uses
	}
	if step.Name != "" {
		return step.Name
	}
	return "run"
}

// runID is the workflow run a job belongs to, for the label's URL. The
// context carries it as run_id (a string); the job id — unique per
// attempt, so it never collides across runs — is the fallback.
func (e *Executor) runID(job *Job) int64 {
	if n, err := strconv.ParseInt(job.Context["run_id"], 10, 64); err == nil && n > 0 {
		return n
	}
	return job.ID
}

// jobRecord builds the failure record for a finished job. Every executed step
// is listed so the record shows how far the job got; the failing step — always
// the last one appended, since the step loop breaks on failure — carries the
// tail of its output.
func jobRecord(job *Job, state *JobState, failed *StepState, stdout, stderr string, dur time.Duration) *JobRecord {
	rec := &JobRecord{
		JobID:    job.ID,
		Result:   "failure",
		Duration: dur.Round(time.Millisecond).String(),
	}
	for _, s := range state.Steps {
		rec.Steps = append(rec.Steps, StepRecord{
			Index:  int(s.ID),
			Name:   s.Name,
			Exit:   s.Exit,
			Result: resultLabel(s.Result),
			Logs:   s.LogLength,
		})
	}
	if failed != nil && len(rec.Steps) > 0 {
		last := &rec.Steps[len(rec.Steps)-1]
		last.Stdout = tailText(stdout, recordTail)
		last.Stderr = tailText(stderr, recordTail)
	}
	return rec
}

// resultLabel names a Result for a record. Result has no String method, and
// the JSON should be readable without one.
func resultLabel(r Result) string {
	switch r {
	case ResultSuccess:
		return "success"
	case ResultFailure:
		return "failure"
	case ResultCancelled:
		return "cancelled"
	case ResultSkipped:
		return "skipped"
	}
	return "unknown"
}

// checkout clones the job's repository into the workspace inside the
// sandbox. It is the sandbox equivalent of actions/checkout: the runner
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
		stepState.Result = ResultFailure
		e.log(ctx, job, *logIndex, "checkout: REPO_BASE_URL is not configured")
		*logIndex++
		stepState.LogLength = 1
		return fmt.Errorf("checkout: REPO_BASE_URL is not configured")
	}
	base = strings.TrimRight(base, "/")
	cloneURL := base + "/" + repo + ".git"

	// Auth via GITHUB_TOKEN (provided by Forgejo in job secrets), sent
	// as an extra header so the token never appears in the URL. Passed
	// via GIT_CONFIG_COUNT env — git's native multi-config mechanism —
	// in the exec's environment, never the command string, so the token
	// is neither injectable (security review #37 C3) nor visible in the
	// guest's /proc/<pid>/cmdline. The charset check stays as a sanity
	// check on what Forgejo hands us.
	token := ctx2.Eval("${{ secrets.GITHUB_TOKEN }}")
	var authEnv map[string]string
	if token != "" {
		if !regexp.MustCompile(`^[A-Za-z0-9_.\-]+$`).MatchString(token) {
			e.log(ctx, job, *logIndex, "checkout: GITHUB_TOKEN contains characters unsafe for shell env (not a standard token)")
			*logIndex++
			stepState.Result = ResultFailure
			stepState.LogLength = 1
			return fmt.Errorf("checkout: GITHUB_TOKEN has unsafe characters")
		}
		authEnv = map[string]string{
			"GIT_CONFIG_COUNT":   "1",
			"GIT_CONFIG_KEY_0":   "http.extraheader",
			"GIT_CONFIG_VALUE_0": "Authorization: token " + token,
		}
	}

	// Ensure the workspace exists and is clean, then clone into it.
	// The warm pool reuses the same rootfs across jobs, so /workspace may
	// hold a previous job's checkout (and its _build artifacts). Remove it
	// first so `git clone` never fails with "destination path already exists".
	cmds := []struct {
		cmd string
		env map[string]string
	}{
		{"rm -rf " + ws, nil},
		{"mkdir -p " + ws, nil},
		{"git clone --depth 1 " + cloneURL + " " + ws, authEnv},
	}
	for _, step := range cmds {
		c := step.cmd
		res, err := e.Sandbox.Exec(ctx, sandboxID, c, "", step.env, 300)
		if err != nil {
			stepState.Result = ResultFailure
			e.log(ctx, job, *logIndex, "checkout: "+err.Error())
			*logIndex++
			stepState.LogLength = 1
			return err
		}
		rows := splitLog(res.Stdout, res.Stderr)
		if len(rows) > 0 {
			*logIndex += e.logLines(ctx, job, *logIndex, rows)
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

// log streams a log row to the job sink. Upload failures are journaled, not
// swallowed: a dropped row otherwise presents as a job that "died" wherever
// the log stops (we have seen a 1.8 KB stored log for a job whose step
// produced 1.4 MB — the mix output never reached Forgejo and the lane looked
// like it never got past pnpm bootstrap).
func (e *Executor) log(ctx context.Context, job *Job, index int64, content string) {
	if content == "" {
		return
	}
	if err := e.Sink.Log(ctx, job.ID, index, []*LogRow{{Content: content}}, false); err != nil {
		log.Printf("executor: job %d log upload failed at row %d: %v", job.ID, index, err)
	}
}

const (
	// logBatchRows bounds the rows sent per UpdateLog call. One giant row
	// (a whole step's output joined with newlines) or one giant batch is
	// what got silently dropped by the sink path before this existed.
	logBatchRows = 200
	// logRowMax caps an individual row's content; longer lines (minified
	// bundles, embedded artifacts) are split so no content is lost.
	logRowMax = 8192
)

// logLines streams rows in bounded batches and returns the number of row
// slots consumed (including splits). Each batch is a separate UpdateLog call
// so one rejected batch cannot take the whole step's log with it; failures
// are journaled and the remaining batches are still attempted.
func (e *Executor) logLines(ctx context.Context, job *Job, index int64, rows []string) int64 {
	sent := int64(0)
	emit := func(batch []string) {
		protoRows := make([]*LogRow, 0, len(batch))
		for _, line := range batch {
			for len(line) > logRowMax {
				protoRows = append(protoRows, &LogRow{Content: line[:logRowMax]})
				line = line[logRowMax:]
				sent++
			}
			if line != "" {
				protoRows = append(protoRows, &LogRow{Content: line})
				sent++
			}
		}
		if len(protoRows) == 0 {
			return
		}
		if err := e.Sink.Log(ctx, job.ID, index+sent-int64(len(protoRows)), protoRows, false); err != nil {
			log.Printf("executor: job %d log upload failed near row %d (%d rows dropped from Forgejo): %v",
				job.ID, index+sent, len(protoRows), err)
		}
	}
	for start := 0; start < len(rows); start += logBatchRows {
		end := start + logBatchRows
		if end > len(rows) {
			end = len(rows)
		}
		emit(rows[start:end])
	}
	return sent
}

// fail reports a job failure and returns the error. Failures that happen
// before the step loop (no workflow, no sandbox) are recorded too: "cannot
// create sandbox" is exactly the class of failure a consumer cannot see from
// Forgejo.
func (e *Executor) fail(ctx context.Context, job *Job, err error) error {
	_ = e.report(ctx, &JobState{ID: job.ID, Result: ResultFailure}, nil)
	writeJobRecord(e.RecordDir, &JobRecord{JobID: job.ID, Result: "failure", Error: err.Error()})
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

// tailStr returns the last n characters of s, for compact error logging.
func tailStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// firstNonEmpty returns the first argument that is not "", or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
