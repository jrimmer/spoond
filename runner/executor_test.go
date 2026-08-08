package runner

import (
	"context"
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
	job.Context = map[string]string{"repository": "lacy.casa/forkd-service"}
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
		if strings.Contains(c, "git") && strings.Contains(c, "lacy.casa/forkd-service.git") {
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
