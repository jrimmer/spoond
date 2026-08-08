package runner

import (
	"context"
	"testing"

	runnerv1 "gitea.dev/actions-proto-go/runner/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// fakeLease is an in-memory LeaseClient for tests.
type fakeLease struct {
	created []string
	execs   []string
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
	if r, ok := f.results[cmd]; ok {
		return r, nil
	}
	return &ExecResult{Stdout: "ok\n", Exit: 0}, nil
}

func (f *fakeLease) Delete(ctx context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

// fakeProto is a no-op protocol client for tests.
type fakeProto struct {
	updates []*runnerv1.TaskState
	logs    []string
}

func (f *fakeProto) UpdateTask(ctx context.Context, state *runnerv1.TaskState, outputs map[string]string) error {
	f.updates = append(f.updates, state)
	return nil
}

func (f *fakeProto) UpdateLog(ctx context.Context, taskID, index int64, rows []*runnerv1.LogRow, noMore bool) error {
	for _, r := range rows {
		f.logs = append(f.logs, r.GetContent())
	}
	return nil
}

func testTask(t *testing.T, payload string) *runnerv1.Task {
	t.Helper()
	ctx, _ := structpb.NewStruct(map[string]any{"repository": "test/repo"})
	return &runnerv1.Task{
		Id:              1,
		WorkflowPayload: []byte(payload),
		Context:         ctx,
		Secrets:         map[string]string{},
		Vars:            map[string]string{},
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
	proto := &fakeProto{}
	exec := &Executor{
		Lease:        lease,
		Proto:        proto,
		Labels:       map[string]string{"ubuntu-latest": "py-base"},
		DefaultImage: "py-base",
		TTL:          600,
	}
	if err := exec.Run(context.Background(), testTask(t, payload)); err != nil {
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
	if len(proto.updates) != 1 || proto.updates[0].GetResult() != runnerv1.Result_RESULT_SUCCESS {
		t.Fatalf("expected success update, got %+v", proto.updates)
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
	proto := &fakeProto{}
	exec := &Executor{
		Lease:        lease,
		Proto:        proto,
		Labels:       map[string]string{"ubuntu-latest": "py-base"},
		DefaultImage: "py-base",
		TTL:          600,
	}
	if err := exec.Run(context.Background(), testTask(t, payload)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(proto.updates) != 1 || proto.updates[0].GetResult() != runnerv1.Result_RESULT_FAILURE {
		t.Fatalf("expected failure update, got %+v", proto.updates)
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
	proto := &fakeProto{}
	exec := &Executor{
		Lease:        lease,
		Proto:        proto,
		Labels:       map[string]string{"ubuntu-latest": "py-base"},
		DefaultImage: "py-base",
		TTL:          600,
	}
	// Force create to fail by using an unknown image.
	exec.Labels = map[string]string{}
	exec.DefaultImage = "no-such-image"
	_ = exec.Run(context.Background(), testTask(t, payload))
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
