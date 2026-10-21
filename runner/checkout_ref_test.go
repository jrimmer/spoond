package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	runnerv1 "gitea.dev/actions-proto-go/runner/v1"
	"gitea.dev/actions-proto-go/runner/v1/runnerv1connect"
)

// refLease records checkout commands and fails the ones failCmd picks.
type refLease struct {
	*fakeLease
	cmds    []string
	failCmd func(cmd string) bool
}

func (f *refLease) Exec(ctx context.Context, id, cmd, cwd string, env map[string]string, timeout int) (*ExecResult, error) {
	f.cmds = append(f.cmds, cmd)
	if f.failCmd != nil && f.failCmd(cmd) {
		return &ExecResult{Stderr: "fatal: no such ref\n", Exit: 128}, nil
	}
	return &ExecResult{Stdout: "ok\n", Exit: 0}, nil
}

func runCheckout(t *testing.T, payload string, ghCtx map[string]string, failCmd func(string) bool) (*refLease, *fakeSink) {
	t.Helper()
	lease := &refLease{fakeLease: newFakeLease(), failCmd: failCmd}
	sink := &fakeSink{}
	exec := &Executor{
		Sandbox:      lease,
		Sink:         sink,
		Labels:       map[string]string{"ubuntu-latest": "py-base"},
		DefaultImage: "py-base",
		TTL:          600,
		RepoBaseURL:  "https://code.example.com",
	}
	job := testJob(payload)
	job.Context = ghCtx
	if err := exec.Run(context.Background(), job); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(sink.reports) != 1 {
		t.Fatalf("reports = %+v, want one", sink.reports)
	}
	return lease, sink
}

func indexOf(cmds []string, sub string) int {
	for i, c := range cmds {
		if strings.Contains(c, sub) {
			return i
		}
	}
	return -1
}

const checkoutPayload = `
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: make test
`

// A pull request's job tests the pushed commit, not the default branch
// the shallow clone lands on.
func TestCheckoutFetchesTriggeringSha(t *testing.T) {
	lease, sink := runCheckout(t, checkoutPayload, map[string]string{
		"repository": "o/r", "sha": "0123abcd0123abcd0123abcd0123abcd0123abcd", "ref": "refs/pull/7/head",
	}, nil)
	if sink.reports[0].Result != ResultSuccess {
		t.Fatalf("result = %v, want success", sink.reports[0].Result)
	}
	clone := indexOf(lease.cmds, "git clone")
	fetch := indexOf(lease.cmds, "fetch --depth 1 origin 0123abcd0123abcd0123abcd0123abcd0123abcd")
	co := indexOf(lease.cmds, "checkout --detach FETCH_HEAD")
	run := indexOf(lease.cmds, "make test")
	if clone < 0 || fetch < clone || co < fetch || run < co {
		t.Fatalf("want clone, fetch sha, checkout, run in order; got %q", lease.cmds)
	}
	if indexOf(lease.cmds, "refs/pull/7/head") >= 0 {
		t.Fatalf("fetched github.ref although the sha fetch worked: %q", lease.cmds)
	}
}

// with: ref: wins over github.sha.
func TestCheckoutUsesWithRef(t *testing.T) {
	payload := `
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          ref: release/3.11
      - run: make test
`
	lease, _ := runCheckout(t, payload, map[string]string{"repository": "o/r", "sha": "abc123"}, nil)
	if indexOf(lease.cmds, "fetch --depth 1 origin release/3.11") < 0 {
		t.Fatalf("with.ref not fetched: %q", lease.cmds)
	}
	if indexOf(lease.cmds, "abc123") >= 0 {
		t.Fatalf("github.sha fetched although with.ref was given: %q", lease.cmds)
	}
}

// A server that refuses a fetch by sha still yields the PR's tree via
// github.ref, and the step counts as passed.
func TestCheckoutFallsBackToRef(t *testing.T) {
	lease, sink := runCheckout(t, checkoutPayload, map[string]string{
		"repository": "o/r", "sha": "abc123", "ref": "refs/pull/7/head",
	}, func(c string) bool { return strings.Contains(c, "origin abc123") })
	if sink.reports[0].Result != ResultSuccess {
		t.Fatalf("result = %v, want success after the github.ref fallback", sink.reports[0].Result)
	}
	if indexOf(lease.cmds, "fetch --depth 1 origin refs/pull/7/head") < 0 || indexOf(lease.cmds, "make test") < 0 {
		t.Fatalf("want ref fallback then run; got %q", lease.cmds)
	}
}

// When neither the sha nor the ref can be fetched the job fails; it
// never runs the default branch's tree in their place.
func TestCheckoutFailsWhenNothingFetches(t *testing.T) {
	lease, sink := runCheckout(t, checkoutPayload, map[string]string{
		"repository": "o/r", "sha": "abc123", "ref": "refs/heads/gone",
	}, func(c string) bool { return strings.Contains(c, " fetch ") })
	if sink.reports[0].Result != ResultFailure {
		t.Fatalf("result = %v, want failure", sink.reports[0].Result)
	}
	if indexOf(lease.cmds, "make test") >= 0 {
		t.Fatalf("ran the job on the default branch: %q", lease.cmds)
	}
}

// A ref outside the git ref charset fails the step before anything runs
// (it would be pasted into a shell command).
func TestCheckoutRejectsUnsafeRef(t *testing.T) {
	for _, ref := range []string{"main; rm -rf /", "-uhoh", "a b", "$(id)"} {
		lease, sink := runCheckout(t, checkoutPayload, map[string]string{"repository": "o/r", "sha": ref}, nil)
		if sink.reports[0].Result != ResultFailure {
			t.Fatalf("ref %q: result = %v, want failure", ref, sink.reports[0].Result)
		}
		if indexOf(lease.cmds, "git clone") >= 0 {
			t.Fatalf("ref %q: cloned before rejecting: %q", ref, lease.cmds)
		}
	}
}

// updateTaskClient captures UpdateTask requests.
type updateTaskClient struct {
	runnerv1connect.RunnerServiceClient
	got []*runnerv1.UpdateTaskRequest
}

func (c *updateTaskClient) UpdateTask(ctx context.Context, r *connect.Request[runnerv1.UpdateTaskRequest]) (*connect.Response[runnerv1.UpdateTaskResponse], error) {
	c.got = append(c.got, r.Msg)
	return connect.NewResponse(&runnerv1.UpdateTaskResponse{}), nil
}

// A final report carries stop times, so Forgejo shows a real duration
// and not "Failing after -497923h".
func TestReportSetsStopTimes(t *testing.T) {
	c := &updateTaskClient{}
	a := &ForgejoAdapter{svc: c}
	started := time.Now().Add(-time.Minute)
	stopped := time.Now().Add(-time.Second)
	err := a.Report(context.Background(), &JobState{ID: 9, Result: ResultFailure, Steps: []StepState{
		{ID: 0, Result: ResultSuccess, Started: started, Stopped: stopped},
		{ID: 1, Result: ResultFailure},
	}}, nil)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	ts := c.got[0].GetState()
	if ts.GetStoppedAt() == nil || time.Since(ts.GetStoppedAt().AsTime()) > time.Minute {
		t.Fatalf("task stopped_at = %v, want now", ts.GetStoppedAt())
	}
	s0 := ts.GetSteps()[0]
	if !s0.GetStartedAt().AsTime().Equal(started) || !s0.GetStoppedAt().AsTime().Equal(stopped) {
		t.Fatalf("step 0 times = %v..%v, want %v..%v", s0.GetStartedAt(), s0.GetStoppedAt(), started, stopped)
	}
	if ts.GetSteps()[1].GetStoppedAt() == nil {
		t.Fatalf("step 1 has no stop time")
	}
}

// updateLogClient captures UpdateLog requests.
type updateLogClient struct {
	runnerv1connect.RunnerServiceClient
	got []*runnerv1.UpdateLogRequest
}

func (c *updateLogClient) UpdateLog(ctx context.Context, r *connect.Request[runnerv1.UpdateLogRequest]) (*connect.Response[runnerv1.UpdateLogResponse], error) {
	c.got = append(c.got, r.Msg)
	return connect.NewResponse(&runnerv1.UpdateLogResponse{}), nil
}

// Log rows carry a real time: the row's own when set, else the send
// time, never the zero time.
func TestLogRowsCarryTimes(t *testing.T) {
	c := &updateLogClient{}
	a := &ForgejoAdapter{svc: c}
	stamped := time.Now().Add(-time.Minute)
	if err := a.Log(context.Background(), 9, 0, []*LogRow{{Time: stamped, Content: "a"}, {Content: "b"}}, false); err != nil {
		t.Fatalf("log: %v", err)
	}
	rows := c.got[0].GetRows()
	if !rows[0].GetTime().AsTime().Equal(stamped) {
		t.Fatalf("row 0 time = %v, want %v", rows[0].GetTime().AsTime(), stamped)
	}
	if time.Since(rows[1].GetTime().AsTime()) > time.Minute {
		t.Fatalf("unstamped row time = %v, want now", rows[1].GetTime().AsTime())
	}
}
