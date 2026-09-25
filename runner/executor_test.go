package runner

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// fakeLease is an in-memory SandboxProvider for tests.
type fakeLease struct {
	created []string
	execs   []string
	envs    []map[string]string
	deleted []string
	results map[string]*ExecResult // keyed by cmd
}

func newFakeLease() *fakeLease {
	return &fakeLease{results: map[string]*ExecResult{}}
}

func (f *fakeLease) Create(ctx context.Context, image string, ttl int) (string, error) {
	if image == "no-such-image" {
		return "", errNoSuchImage
	}
	id := "sb-test-" + image
	f.created = append(f.created, id)
	return id, nil
}

var errNoSuchImage = &fakeErr{"no such image"}

type fakeErr struct{ msg string }

func (e *fakeErr) Error() string { return e.msg }

func (f *fakeLease) Exec(ctx context.Context, id, cmd, cwd string, env map[string]string, timeout int) (*ExecResult, error) {
	f.execs = append(f.execs, cmd)
	f.envs = append(f.envs, env)
	if r, ok := f.results[cmd]; ok {
		return r, nil
	}
	return &ExecResult{Stdout: "ok\n", Exit: 0}, nil
}

func (f *fakeLease) Delete(ctx context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

// fakeSink is a no-op JobSink for tests.
type fakeSink struct {
	reports []*JobState
	logs    []string
}

func (f *fakeSink) Report(ctx context.Context, state *JobState, outputs map[string]string) error {
	f.reports = append(f.reports, state)
	return nil
}

func (f *fakeSink) Keepalive(ctx context.Context, jobID int64) error {
	return nil
}

func (f *fakeSink) Log(ctx context.Context, jobID, index int64, rows []*LogRow, noMore bool) error {
	for _, r := range rows {
		f.logs = append(f.logs, r.Content)
	}
	return nil
}

func testJob(payload string) *Job {
	return &Job{
		ID:       1,
		Workflow: []byte(payload),
		Secrets:  map[string]string{},
		Vars:     map[string]string{},
		Context:  map[string]string{"repository": "test/repo"},
	}
}

func TestExecutorHappyPath(t *testing.T) {
	payload := `
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hello
`
	lease := newFakeLease()
	sink := &fakeSink{}
	exec := &Executor{
		Sandbox:      lease,
		Sink:         sink,
		Labels:       map[string]string{"ubuntu-latest": "py-base"},
		DefaultImage: "py-base",
		TTL:          600,
	}
	if err := exec.Run(context.Background(), testJob(payload)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(lease.created) != 1 {
		t.Fatalf("expected 1 sandbox created, got %d", len(lease.created))
	}
	if len(lease.deleted) != 1 {
		t.Fatalf("expected sandbox released, got %d deletes", len(lease.deleted))
	}
	if lease.deleted[0] != lease.created[0] {
		t.Fatalf("released %s but created %s", lease.deleted[0], lease.created[0])
	}
	if len(sink.reports) != 1 || sink.reports[0].Result != ResultSuccess {
		t.Fatalf("expected success report, got %+v", sink.reports)
	}
}

func TestExecutorFailingStep(t *testing.T) {
	payload := `
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: exit 1
`
	lease := newFakeLease()
	lease.results["exit 1"] = &ExecResult{Stderr: "boom\n", Exit: 1}
	sink := &fakeSink{}
	exec := &Executor{
		Sandbox:      lease,
		Sink:         sink,
		Labels:       map[string]string{"ubuntu-latest": "py-base"},
		DefaultImage: "py-base",
		TTL:          600,
	}
	if err := exec.Run(context.Background(), testJob(payload)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(sink.reports) != 1 || sink.reports[0].Result != ResultFailure {
		t.Fatalf("expected failure report, got %+v", sink.reports)
	}
	// Sandbox must still be released on failure.
	if len(lease.deleted) != 1 {
		t.Fatalf("expected sandbox released on failure, got %d deletes", len(lease.deleted))
	}
}

func TestExecutorReleasesSandboxOnCreateError(t *testing.T) {
	payload := `
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`
	lease := newFakeLease()
	sink := &fakeSink{}
	exec := &Executor{
		Sandbox:      lease,
		Sink:         sink,
		Labels:       map[string]string{},
		DefaultImage: "no-such-image",
		TTL:          600,
	}
	_ = exec.Run(context.Background(), testJob(payload))
	// No sandbox was created, so nothing to release.
	if len(lease.created) != 0 {
		t.Fatalf("expected no sandbox created, got %d", len(lease.created))
	}
}

func TestParseWorkflow(t *testing.T) {
	payload := `
name: CI
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`
	wf, err := ParseWorkflow([]byte(payload))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if wf.Name != "CI" {
		t.Fatalf("expected name CI, got %s", wf.Name)
	}
	job := wf.Jobs["build"]
	if job == nil {
		t.Fatal("expected build job")
	}
	labels := job.RunsOnLabels()
	if len(labels) != 1 || labels[0] != "ubuntu-latest" {
		t.Fatalf("expected [ubuntu-latest], got %v", labels)
	}
	if len(job.Steps) != 1 || job.Steps[0].Run != "echo hi" {
		t.Fatalf("expected 1 step with run 'echo hi', got %+v", job.Steps)
	}
}

func TestEvalContext(t *testing.T) {
	c := &EvalContext{
		GitHub:  map[string]string{"repository": "test/repo"},
		Env:     map[string]string{"FOO": "bar"},
		Secrets: map[string]string{"TOKEN": "secret"},
		Vars:    map[string]string{"V": "1"},
		Steps:   map[string]map[string]string{"build": {"id": "abc"}},
	}
	got := c.Eval("echo ${{ github.repository }} ${{ env.FOO }} ${{ secrets.TOKEN }} ${{ vars.V }}")
	want := "echo test/repo bar secret 1"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

// checkoutRecordingLease records exec commands so tests can assert the
// checkout sequence.
type checkoutRecordingLease struct {
	*fakeLease
	cmds []string
	cwds []string
}

func (f *checkoutRecordingLease) Exec(ctx context.Context, id, cmd, cwd string, env map[string]string, timeout int) (*ExecResult, error) {
	f.cmds = append(f.cmds, cmd)
	f.cwds = append(f.cwds, cwd)
	return &ExecResult{Stdout: "ok\n", Exit: 0}, nil
}

func TestExecutorCheckoutClonesRepo(t *testing.T) {
	payload := `
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: go test ./...
`
	lease := &checkoutRecordingLease{fakeLease: newFakeLease()}
	sink := &fakeSink{}
	exec := &Executor{
		Sandbox:      lease,
		Sink:         sink,
		Labels:       map[string]string{"ubuntu-latest": "py-base"},
		DefaultImage: "py-base",
		TTL:          600,
		RepoBaseURL:  "https://code.lacy.casa",
	}
	job := testJob(payload)
	job.Secrets = map[string]string{"GITHUB_TOKEN": "tok123"}
	job.Context = map[string]string{"repository": "lacy.casa/spoond"}
	if err := exec.Run(context.Background(), job); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(sink.reports) != 1 || sink.reports[0].Result != ResultSuccess {
		t.Fatalf("expected success, got %+v", sink.reports)
	}
	// The checkout step should have issued mkdir + git clone with the
	// repo URL and token header, and the run step should use /workspace.
	var sawClone, sawRun bool
	for _, c := range lease.cmds {
		if strings.Contains(c, "git") && strings.Contains(c, "lacy.casa/spoond.git") {
			sawClone = true
			if !strings.Contains(c, "Authorization: token tok123") {
				t.Fatalf("clone missing token header: %s", c)
			}
		}
		if strings.Contains(c, "go test ./...") {
			sawRun = true
		}
	}
	if !sawClone {
		t.Fatalf("expected git clone command, got: %v", lease.cmds)
	}
	if !sawRun {
		t.Fatalf("expected run step to execute, got: %v", lease.cmds)
	}
	// The run step after checkout must use /workspace as cwd.
	for i, c := range lease.cmds {
		if strings.Contains(c, "go test ./...") {
			if lease.cwds[i] != "/workspace" {
				t.Fatalf("run step cwd = %q, want /workspace", lease.cwds[i])
			}
		}
	}
}

func TestExecutorRunWithoutCheckoutUsesEmptyCwd(t *testing.T) {
	payload := `
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hello
`
	lease := &checkoutRecordingLease{fakeLease: newFakeLease()}
	sink := &fakeSink{}
	exec := &Executor{
		Sandbox:      lease,
		Sink:         sink,
		Labels:       map[string]string{"ubuntu-latest": "py-base"},
		DefaultImage: "py-base",
		TTL:          600,
	}
	if err := exec.Run(context.Background(), testJob(payload)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(sink.reports) != 1 || sink.reports[0].Result != ResultSuccess {
		t.Fatalf("expected success, got %+v", sink.reports)
	}
	// No checkout step -> run step must use empty cwd (not /workspace).
	for i, c := range lease.cmds {
		if strings.Contains(c, "echo hello") {
			if lease.cwds[i] != "" {
				t.Fatalf("run step cwd = %q, want empty (no checkout)", lease.cwds[i])
			}
		}
	}
}

func TestExecutorCheckoutMissingRepo(t *testing.T) {
	payload := `
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: echo hi
`
	lease := &checkoutRecordingLease{fakeLease: newFakeLease()}
	sink := &fakeSink{}
	exec := &Executor{
		Sandbox:      lease,
		Sink:         sink,
		Labels:       map[string]string{"ubuntu-latest": "py-base"},
		DefaultImage: "py-base",
		TTL:          600,
	}
	job := testJob(payload)
	job.Context = map[string]string{} // no repository
	if err := exec.Run(context.Background(), job); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(sink.reports) != 1 || sink.reports[0].Result != ResultFailure {
		t.Fatalf("expected failure, got %+v", sink.reports)
	}
}

func TestExecutorJobLevelEnv(t *testing.T) {
	payload := `
jobs:
  build:
    runs-on: ubuntu-latest
    env:
      JOB_VAR: job-value
    steps:
      - run: echo $JOB_VAR
`
	lease := newFakeLease()
	sink := &fakeSink{}
	exec := &Executor{
		Sandbox:      lease,
		Sink:         sink,
		Labels:       map[string]string{"ubuntu-latest": "py-base"},
		DefaultImage: "py-base",
		TTL:          600,
	}
	if err := exec.Run(context.Background(), testJob(payload)); err != nil {
		t.Fatalf("run: %v", err)
	}
	// The run step should receive the job-level env var.
	found := false
	for _, env := range lease.envs {
		if env["JOB_VAR"] == "job-value" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected job-level env JOB_VAR=job-value in exec env, got: %+v", lease.envs)
	}
}

func TestExecutorInjectsCIEnvVars(t *testing.T) {
	payload := `
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`
	lease := newFakeLease()
	sink := &fakeSink{}
	exec := &Executor{
		Sandbox:      lease,
		Sink:         sink,
		Labels:       map[string]string{"ubuntu-latest": "py-base"},
		DefaultImage: "py-base",
		TTL:          600,
	}
	job := testJob(payload)
	// Flat context keys are looked up under github.* by EvalContext.
	job.Context = map[string]string{
		"repository": "jrimmer/netcrawl",
		"sha":        "abc123",
	}
	if err := exec.Run(context.Background(), job); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(lease.envs) == 0 {
		t.Fatalf("expected exec env captured")
	}
	env := lease.envs[0]
	if env["CI_REPO_OWNER"] != "jrimmer" {
		t.Fatalf("CI_REPO_OWNER = %q, want jrimmer (env=%+v)", env["CI_REPO_OWNER"], env)
	}
	if env["CI_REPO_NAME"] != "netcrawl" {
		t.Fatalf("CI_REPO_NAME = %q, want netcrawl (env=%+v)", env["CI_REPO_NAME"], env)
	}
	if env["CI_COMMIT"] != "abc123" {
		t.Fatalf("CI_COMMIT = %q, want abc123 (env=%+v)", env["CI_COMMIT"], env)
	}
}

// recordingSink records Log calls with their row counts and starting indexes
// so batching behaviour is observable.
type recordingSink struct {
	fakeSink
	calls []struct {
		index int64
		rows  int
	}
	failEvery int // when >0, every Nth Log call returns an error
	n          int
}

func (r *recordingSink) Log(ctx context.Context, jobID, index int64, rows []*LogRow, noMore bool) error {
	r.n++
	r.calls = append(r.calls, struct {
		index int64
		rows  int
	}{index, len(rows)})
	if r.failEvery > 0 && r.n%r.failEvery == 0 {
		return fmt.Errorf("simulated upload failure")
	}
	return r.fakeSink.Log(ctx, jobID, index, rows, noMore)
}

func TestLogLinesBatches(t *testing.T) {
	e := &Executor{Sink: &recordingSink{}}
	rows := make([]string, 450)
	for i := range rows {
		rows[i] = fmt.Sprintf("row %d", i)
	}
	consumed := e.logLines(t.Context(), &Job{ID: 7}, 10, rows)
	if consumed != 450 {
		t.Fatalf("consumed = %d, want 450", consumed)
	}
	rs := e.Sink.(*recordingSink)
	if len(rs.calls) != 3 {
		t.Fatalf("calls = %d, want 3 (200+200+50)", len(rs.calls))
	}
	if rs.calls[0].index != 10 || rs.calls[1].index != 210 || rs.calls[2].index != 410 {
		t.Fatalf("batch indexes = %v, want 10/210/410", rs.calls)
	}
}

func TestLogLinesSplitsLongRows(t *testing.T) {
	e := &Executor{Sink: &recordingSink{}}
	consumed := e.logLines(t.Context(), &Job{ID: 8}, 0, []string{strings.Repeat("x", logRowMax*2 + 5)})
	rs := e.Sink.(*recordingSink)
	total := 0
	for _, c := range rs.calls {
		total += c.rows
	}
	if consumed != 3 || total != 3 {
		t.Fatalf("consumed=%d rows=%d, want 3 (8192+8192+5)", consumed, total)
	}
}

func TestLogLinesContinuesPastFailures(t *testing.T) {
	e := &Executor{Sink: &recordingSink{failEvery: 2}}
	rows := make([]string, logBatchRows*2) // two batches; the second upload fails
	for i := range rows {
		rows[i] = "x"
	}
	consumed := e.logLines(t.Context(), &Job{ID: 9}, 0, rows)
	if consumed != int64(len(rows)) {
		t.Fatalf("consumed = %d, want %d (failures must not stop batching)", consumed, int64(len(rows)))
	}
	rs := e.Sink.(*recordingSink)
	if len(rs.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(rs.calls))
	}
}

func TestExecutorEnvDefaults(t *testing.T) {
	payload := `
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: probe-defaults
      - env:
          PATH: /custom/bin
          COREPACK_ENABLE_DOWNLOAD_PROMPT: "1"
        run: probe-override
`
	lease := newFakeLease()
	sink := &fakeSink{}
	exec := &Executor{
		Sandbox:      lease,
		Sink:         sink,
		Labels:       map[string]string{"ubuntu-latest": "py-base"},
		DefaultImage: "py-base",
		TTL:          600,
	}
	if err := exec.Run(context.Background(), testJob(payload)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(lease.envs) != 2 {
		t.Fatalf("expected 2 step execs, got %d", len(lease.envs))
	}
	def := lease.envs[0]
	if def["PATH"] != "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" {
		t.Fatalf("default PATH = %q", def["PATH"])
	}
	if def["COREPACK_ENABLE_DOWNLOAD_PROMPT"] != "0" {
		t.Fatalf("default COREPACK_ENABLE_DOWNLOAD_PROMPT = %q", def["COREPACK_ENABLE_DOWNLOAD_PROMPT"])
	}
	if def["CI"] != "true" {
		t.Fatalf("default CI = %q", def["CI"])
	}
	if def["USER"] != "root" || def["LOGNAME"] != "root" {
		t.Fatalf("default USER/LOGNAME = %q/%q, want root/root", def["USER"], def["LOGNAME"])
	}
	ovr := lease.envs[1]
	if ovr["PATH"] != "/custom/bin" {
		t.Fatalf("explicit PATH must win, got %q", ovr["PATH"])
	}
	if ovr["COREPACK_ENABLE_DOWNLOAD_PROMPT"] != "1" {
		t.Fatalf("explicit COREPACK_ENABLE_DOWNLOAD_PROMPT must win, got %q", ovr["COREPACK_ENABLE_DOWNLOAD_PROMPT"])
	}
}

func TestExecutorGitHubRunIDInjection(t *testing.T) {
	cases := []struct {
		name    string
		context map[string]string
		want    string
	}{
		{"run_id present", map[string]string{"run_id": "4242"}, "4242"},
		{"run_number fallback", map[string]string{"run_number": "1174"}, "1174"},
		{"task id last resort", map[string]string{}, "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := `
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: probe
`
			lease := newFakeLease()
			sink := &fakeSink{}
			exec := &Executor{
				Sandbox:      lease,
				Sink:         sink,
				Labels:       map[string]string{"ubuntu-latest": "py-base"},
				DefaultImage: "py-base",
				TTL:          600,
			}
			job := testJob(payload)
			job.Context = tc.context
			if err := exec.Run(context.Background(), job); err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := lease.envs[0]["GITHUB_RUN_ID"]; got != tc.want {
				t.Fatalf("GITHUB_RUN_ID = %q, want %q", got, tc.want)
			}
		})
	}
}
