package spoondrunner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	runnerv1 "gitea.dev/actions-proto-go/runner/v1"
	"gitea.dev/actions-proto-go/runner/v1/runnerv1connect"

	"github.com/jrimmer/spoond/v4/runner"
	"github.com/jrimmer/spoond/v4/settings"
)

// The fake backend pair here stands in for Forgejo and the spoond lease
// API, so Main can run for real — signal context, pool, executor,
// HTTP lease client — and the test can watch a SIGTERM drain a running
// job the way production does: grace first, then cancel, report
// cancelled to Forgejo, delete the lease, exit 0.

// fakeForgejo is a minimal Forgejo runner protocol server: it registers
// the runner, hands out one job when armed, and records every UpdateTask.
type fakeForgejo struct {
	mu  sync.Mutex
	srv *httptest.Server

	armed   bool // hand out the job on the next FetchTask
	fetches int
	updates []string // results seen in UpdateTask, in order

	releaseOnce sync.Once
	releaseAll  chan struct{} // closed by release(): unparks handlers a cancelled ctx cannot reach
}

func (f *fakeForgejo) handler() http.Handler {
	actionsmux := http.NewServeMux()
	p, h := runnerv1connect.NewRunnerServiceHandler(f)
	actionsmux.Handle(p, h)
	root := http.NewServeMux()
	root.Handle("/api/actions/", http.StripPrefix("/api/actions", actionsmux))
	return root
}

func (f *fakeForgejo) start(t *testing.T) {
	t.Helper()
	f.releaseAll = make(chan struct{})
	f.srv = httptest.NewServer(f.handler())
	t.Cleanup(f.srv.Close)
}

// release unparks parked long-poll handlers (see Keepalive).
func (f *fakeForgejo) release() {
	f.releaseOnce.Do(func() { close(f.releaseAll) })
}

func (f *fakeForgejo) fetchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetches
}

func (f *fakeForgejo) updateResults() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.updates...)
}

// Register implements runnerv1connect.RunnerServiceHandler.
func (f *fakeForgejo) Register(ctx context.Context, req *connect.Request[runnerv1.RegisterRequest]) (*connect.Response[runnerv1.RegisterResponse], error) {
	resp := connect.NewResponse(&runnerv1.RegisterResponse{
		Runner: &runnerv1.Runner{Id: 1, Uuid: "uuid-1", Token: "rtok"},
	})
	return resp, nil
}

// Declare implements runnerv1connect.RunnerServiceHandler.
func (f *fakeForgejo) Declare(ctx context.Context, req *connect.Request[runnerv1.DeclareRequest]) (*connect.Response[runnerv1.DeclareResponse], error) {
	return connect.NewResponse(&runnerv1.DeclareResponse{}), nil
}

// FetchTask implements runnerv1connect.RunnerServiceHandler. It hands
// out the job once armed (setJob on the handler), then never again —
// one job is all this scenario needs.
func (f *fakeForgejo) FetchTask(ctx context.Context, req *connect.Request[runnerv1.FetchTaskRequest]) (*connect.Response[runnerv1.FetchTaskResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetches++
	if !f.armed {
		return connect.NewResponse(&runnerv1.FetchTaskResponse{TasksVersion: 1}), nil
	}
	f.armed = false
	return connect.NewResponse(&runnerv1.FetchTaskResponse{
		TasksVersion: 2,
		Task: &runnerv1.Task{
			Id: 42,
			WorkflowPayload: []byte(`
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hello
`),
		},
	}), nil
}

// UpdateTask implements runnerv1connect.RunnerServiceHandler.
func (f *fakeForgejo) UpdateTask(ctx context.Context, req *connect.Request[runnerv1.UpdateTaskRequest]) (*connect.Response[runnerv1.UpdateTaskResponse], error) {
	f.mu.Lock()
	f.updates = append(f.updates, req.Msg.GetState().GetResult().String())
	f.mu.Unlock()
	return connect.NewResponse(&runnerv1.UpdateTaskResponse{}), nil
}

// UpdateLog implements runnerv1connect.RunnerServiceHandler.
func (f *fakeForgejo) UpdateLog(ctx context.Context, req *connect.Request[runnerv1.UpdateLogRequest]) (*connect.Response[runnerv1.UpdateLogResponse], error) {
	return connect.NewResponse(&runnerv1.UpdateLogResponse{}), nil
}

// fakeLeaseAPI is a minimal spoond lease backend: create, a blocking
// exec (the running job), list (the start sweep) and delete.
type fakeLeaseAPI struct {
	mu  sync.Mutex
	srv *httptest.Server

	creates []map[string]any
	deleted []string

	execInFlight  chan struct{} // closed when the job's exec arrives
	execCtxDone   chan struct{} // closed when the exec's request ctx dies
	sweepArrived  chan struct{} // closed when the start sweep lists
	deleteArrived chan struct{} // closed when the job's lease is deleted

	releaseOnce sync.Once
	releaseAll  chan struct{} // closed by release(): unparks handlers a cancelled ctx cannot reach
}

func newFakeLeaseAPI() *fakeLeaseAPI {
	return &fakeLeaseAPI{
		execInFlight:  make(chan struct{}),
		execCtxDone:   make(chan struct{}),
		sweepArrived:  make(chan struct{}),
		deleteArrived: make(chan struct{}),
		releaseAll:    make(chan struct{}),
	}
}

// release unparks every parked handler. Called before the servers
// close: a handler waiting on a notification that a cancelled ctx
// cannot deliver would otherwise keep its connection alive forever.

func (f *fakeLeaseAPI) start(t *testing.T) {
	t.Helper()
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
}

// release unparks every parked handler. Must be called before the
// servers close: a handler waiting on a notification that a cancelled
// ctx cannot deliver would otherwise keep its connection alive
// forever.
func (f *fakeLeaseAPI) release() {
	f.releaseOnce.Do(func() { close(f.releaseAll) })
}

func (f *fakeLeaseAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/leases":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.creates = append(f.creates, body)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"sb-job"}`))
	case r.Method == http.MethodPost && r.URL.Path == "/api/leases/sb-job/exec":
		// The job's one step: run until the runner's job context ends —
		// exactly what a long CI step under graceful shutdown does.
		close(f.execInFlight)
		select {
		case <-r.Context().Done():
		case <-f.releaseAll:
		}
		close(f.execCtxDone)
	case r.Method == http.MethodGet && r.URL.Path == "/api/leases":
		close(f.sweepArrived)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"sandboxes":[]}`))
	case r.Method == http.MethodDelete && r.URL.Path == "/api/leases/sb-job":
		f.mu.Lock()
		f.deleted = append(f.deleted, "sb-job")
		f.mu.Unlock()
		close(f.deleteArrived)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeLeaseAPI) createBodies() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.creates))
	for i, c := range f.creates {
		out[i] = map[string]any{}
		for k, v := range c {
			out[i][k] = v
		}
	}
	return out
}

func (f *fakeLeaseAPI) deletedLeases() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

// waitFor polls pred until it holds or the (sub-second) budget runs out.
func waitFor(t *testing.T, what string, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestMainSIGTERMGraceThenCancel (#119): the real wiring — Main on a
// signal.NotifyContext, a running job, RUNNER_STOP_GRACE — honours the
// grace: SIGTERM does not kill the running job, Stop cancels it when
// the grace is spent, the job's lease is deleted and its final
// (cancelled) state reaches Forgejo, and Main exits 0.
func TestMainSIGTERMGraceThenCancel(t *testing.T) {
	for _, k := range []string{"FORGEJO_URL", "RUNNER_TOKEN", "RUNNER_NAME", "LEASE_URL", "LEASE_TOKEN",
		"RUNNER_FLOOR", "RUNNER_MAX", "RUNNER_STATE_FILE", "JOB_RECORD_DIR", "RUNNER_STOP_GRACE",
		"METRICS_LISTEN", "IMAGE_MAP", "RUNNER_ADMIT_WAIT_SECS", "RUNNER_JOB_TIMEOUT"} {
		os.Unsetenv(k)
	}
	forgejo := &fakeForgejo{}
	forgejo.start(t)
	lease := newFakeLeaseAPI()
	lease.start(t)

	t.Setenv("FORGEJO_URL", forgejo.srv.URL)
	t.Setenv("RUNNER_TOKEN", "reg-tok")
	t.Setenv("RUNNER_NAME", "sigtest")
	t.Setenv("LEASE_URL", lease.srv.URL)
	t.Setenv("LEASE_TOKEN", "lease-tok")
	t.Setenv("RUNNER_FLOOR", "1")
	t.Setenv("RUNNER_MAX", "1")
	t.Setenv("RUNNER_STATE_FILE", t.TempDir()+"/state.json")
	t.Setenv("JOB_RECORD_DIR", t.TempDir())
	t.Setenv("RUNNER_STOP_GRACE", "300ms")
	// The seconds form every sibling *_SECS knob uses — this is the
	// value the hold TTL (timeout + 10 min) must be derived from.

	code := make(chan int, 1)
	go func() { code <- Main(nil) }()

	// Wait for the runner to register and poll.
	waitFor(t, "the runner to start polling", func() bool { return forgejo.fetchCount() > 0 })

	// Hand out the job; wait until its step is executing inside the
	// sandbox (the lease created, exec in flight). The worker's idle
	// poll sleeps 2 s between fetches, so the budget here has to
	// outlast that cycle (a wait bound, not a sleep).
	forgejo.mu.Lock()
	forgejo.armed = true
	forgejo.mu.Unlock()
	select {
	case <-lease.execInFlight:
	case <-time.After(5 * time.Second):
		t.Fatal("job's exec never reached the sandbox")
	}

	// The lease the job runs in is labelled with the job (#119, #132):
	// the create body carries the label as its comment — the lease is
	// born with it (the `created` event names the job) and no comment
	// call follows the create — and is not held (no holder fields: held
	// leases join the periodic checkpoint pass).
	waitFor(t, "the lease create", func() bool { return len(lease.createBodies()) > 0 })
	create := lease.createBodies()[0]
	for _, k := range []string{"holder", "holder_url"} {
		if _, ok := create[k]; ok {
			t.Fatalf("%s sent on create: the job lease must not be held", k)
		}
	}
	// The create carries the admission wait (#129): a full node's queue
	// holds it open instead of answering 503 at once. This run used the
	// default (RUNNER_ADMIT_WAIT_SECS was unset).
	if got, ok := create["wait"].(float64); !ok || int(got) != runner.DefaultAdmitWaitSecs {
		t.Fatalf("create wait = %v, want %d", create["wait"], runner.DefaultAdmitWaitSecs)
	}
	label, _ := create["comment"].(string)
	if want := "forgejo job 42 " + forgejo.srv.URL + "/actions/runs/42/jobs/42"; label != want {
		t.Fatalf("create body comment = %q, want %q", label, want)
	}

	// SIGTERM. systemd's stop signal starts the drain.
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("kill: %v", err)
	}

	// The grace must hold: well past the signal the job is still
	// running — its context is alive, nothing was cancelled or deleted.
	time.Sleep(150 * time.Millisecond)
	select {
	case <-lease.execCtxDone:
		t.Fatal("SIGTERM killed the running job immediately; RUNNER_STOP_GRACE was not honoured")
	default:
	}
	if got := lease.deletedLeases(); len(got) != 0 {
		t.Fatalf("lease deleted %v at signal time, want nothing during the grace", got)
	}

	// The drain: grace spent → job cancelled → lease deleted → Main
	// exits 0 — all well within a couple of grace periods.
	select {
	case rc := <-code:
		if rc != 0 {
			t.Fatalf("Main returned %d, want 0", rc)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Main never returned after SIGTERM")
	}

	// The executor's deferred delete released the job's lease…
	select {
	case <-lease.deleteArrived:
	case <-time.After(time.Second):
		t.Fatal("job lease was never deleted")
	}
	if got := lease.deletedLeases(); len(got) != 1 || got[0] != "sb-job" {
		t.Fatalf("deleted %v, want [sb-job]", got)
	}
	// …and Forgejo heard the final state: cancelled, not failure.
	results := forgejo.updateResults()
	if len(results) == 0 {
		t.Fatal("no UpdateTask ever reached Forgejo; the final state was lost")
	}
	last := results[len(results)-1]
	if last != "RESULT_CANCELLED" {
		t.Fatalf("final UpdateTask result = %s, want RESULT_CANCELLED", last)
	}

	// Unpark the long-poll handlers no cancelled ctx can reach, so the
	// fakes' connections drain and their servers can close.
	lease.release()
	forgejo.release()
}

// TestEnvDurOrBareSeconds: the duration knob accepts the *_SECS form too
// (RUNNER_JOB_TIMEOUT=3600 is an hour), a Go duration, and falls back to
// the default otherwise.
func TestEnvDurOrBareSeconds(t *testing.T) {
	cases := []struct {
		val  string
		want time.Duration
	}{
		{"3600", time.Hour},
		{"10m", 10 * time.Minute},
		{"", 42 * time.Second},
		{"nonsense", 42 * time.Second},
	}
	for _, c := range cases {
		t.Setenv("RUNNER_JOB_TIMEOUT", c.val)
		if got := envDurOr("RUNNER_JOB_TIMEOUT", 42*time.Second); got != c.want {
			t.Errorf("envDurOr(%q) = %s, want %s", c.val, got, c.want)
		}
	}
}

// TestDefaultJobTimeout (L4): RUNNER_JOB_TIMEOUT defaults to six hours, so
// a node that answers 503 forever cannot pin a worker indefinitely. It
// pins the helper Main actually uses (jobTimeoutFromEnv), so a mutation
// of the env read in Main fails the test too (T2).
func TestDefaultJobTimeout(t *testing.T) {
	if runner.DefaultJobTimeout != 6*time.Hour {
		t.Fatalf("DefaultJobTimeout = %s, want 6h", runner.DefaultJobTimeout)
	}
	cases := []struct {
		val  string
		set  bool
		want time.Duration
	}{
		{"", false, 6 * time.Hour},
		{"0", true, 0},
		{"90m", true, 90 * time.Minute},
		{"5400", true, 90 * time.Minute},
	}
	for _, c := range cases {
		if c.set {
			t.Setenv("RUNNER_JOB_TIMEOUT", c.val)
		} else {
			os.Unsetenv("RUNNER_JOB_TIMEOUT")
		}
		if got := jobTimeoutFromEnv(); got != c.want {
			t.Errorf("jobTimeoutFromEnv() with RUNNER_JOB_TIMEOUT=%q = %s, want %s", c.val, got, c.want)
		}
	}
}

// TestSecretsFromFileOrEnv (#137 C2): the runner's secrets resolve
// through settings.Get — RUNNER_TOKEN, LEASE_TOKEN and
// FORGEJO_ADMIN_TOKEN each take a <NAME>_FILE value file (one trailing
// newline trimmed), both forms set is the startup error, and an
// unreadable file is too. Main itself log.Fatal's, so the resolution is
// pinned through settings.Get on exactly those names plus Main's
// required-message change (a missing RUNNER_TOKEN now says
// "or RUNNER_TOKEN_FILE").
func TestSecretsFromFileOrEnv(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tokenFile := write("runner-token", "reg-from-file\n")
	leaseFile := write("lease-token", "lease-from-file")
	adminFile := write("forgejo-token", "admin-from-file\n")

	t.Setenv("RUNNER_TOKEN", "")
	t.Setenv("LEASE_TOKEN", "")
	t.Setenv("FORGEJO_ADMIN_TOKEN", "")
	t.Setenv("RUNNER_TOKEN_FILE", tokenFile)
	t.Setenv("LEASE_TOKEN_FILE", leaseFile)
	t.Setenv("FORGEJO_ADMIN_TOKEN_FILE", adminFile)
	v, err := settings.Get("RUNNER_TOKEN", os.Getenv, os.ReadFile)
	if err != nil || v != "reg-from-file" {
		t.Fatalf("RUNNER_TOKEN from file: (%q, %v)", v, err)
	}
	v, err = settings.Get("LEASE_TOKEN", os.Getenv, os.ReadFile)
	if err != nil || v != "lease-from-file" {
		t.Fatalf("LEASE_TOKEN from file: (%q, %v)", v, err)
	}
	v, err = settings.Get("FORGEJO_ADMIN_TOKEN", os.Getenv, os.ReadFile)
	if err != nil || v != "admin-from-file" {
		t.Fatalf("FORGEJO_ADMIN_TOKEN from file: (%q, %v)", v, err)
	}

	// Both forms set refuses.
	t.Setenv("RUNNER_TOKEN", "env-token")
	if _, err := settings.Get("RUNNER_TOKEN", os.Getenv, os.ReadFile); err == nil || !strings.Contains(err.Error(), "RUNNER_TOKEN and RUNNER_TOKEN_FILE") {
		t.Fatalf("both forms: err = %v, want the both-set error", err)
	}
	// An unreadable file refuses.
	t.Setenv("RUNNER_TOKEN", "")
	t.Setenv("RUNNER_TOKEN_FILE", filepath.Join(dir, "gone"))
	if _, err := settings.Get("RUNNER_TOKEN", os.Getenv, os.ReadFile); err == nil || !strings.Contains(err.Error(), "RUNNER_TOKEN: read ") {
		t.Fatalf("missing file: err = %v, want the read error", err)
	}
	// The names are known settings through their _FILE spellings (config
	// show and doctor must not flag them).
	if !settings.IsKnown("RUNNER_TOKEN_FILE") || !settings.IsKnown("LEASE_TOKEN_FILE") {
		t.Fatal("the runner's _FILE names are not known")
	}
}
