// Package fake provides an in-memory substrate.Substrate for unit tests.
package fake

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/jrimmer/spoond/v2/substrate"
)

// Fake implements substrate.Substrate in memory.
type Fake struct {
	// Calls records one entry per call, "<Method> <first id argument>",
	// e.g. "Create i0123...", "Pause i0123...", "NodeInfo".
	Calls []string

	mu        sync.Mutex
	sandboxes map[string]substrate.Sandbox
	files     map[string]*memFS // per-sandbox in-memory filesystem
	procs     map[uint32]*FakeProcess
	nextPID   uint32
	// jobProcs and jobPIDOf track guest processes started by a
	// background-job wrapper (spoond-job): JobProcess drives their exit
	// and JobState reads what they left in dir (2.6, #135).
	jobProcs map[uint32]*FakeProcess
	jobPIDOf map[string]uint32
	// jobDir is the host directory a "real wrapper" job runner writes a
	// job's files into; empty means the runner is disabled.
	jobDir string
	// execRunner makes Exec run its argv on the host with req.Env in the
	// process environment instead of echoing the argv. Tests enable it to
	// pin that exec env reaches the command and never argv.
	execRunner bool

	execHandler  func(sandboxID string, req substrate.ExecRequest) substrate.ExecResult
	lastExec     substrate.ExecRequest
	startHandler func(sandboxID string, req substrate.StartRequest) (substrate.Process, error)
	// startCtx is the context of the most recent Start, kept so tests can
	// pin that a background job's start context outlives its HTTP request
	// (2.6 #135).
	startCtx   context.Context
	nodeInfo   substrate.NodeInfo
	nodeInfoFn func(ctx context.Context) (substrate.NodeInfo, error)
	nodeErr    error
	healthErrs map[string]error
	fails      []failSpec
}

type failSpec struct {
	method string
	nth    int // 1-based; 0 = every call
	err    error
}

// New returns an empty Fake. Its default NodeInfo is Status "healthy" with
// 1<<20 free 2 MiB pages and no outstanding work.
func New() *Fake {
	return &Fake{
		sandboxes:  map[string]substrate.Sandbox{},
		files:      map[string]*memFS{},
		procs:      map[uint32]*FakeProcess{},
		jobProcs:   map[uint32]*FakeProcess{},
		jobPIDOf:   map[string]uint32{},
		healthErrs: map[string]error{},
		nodeInfo: substrate.NodeInfo{
			Status:            "healthy",
			HugepagesTotal:    1 << 20,
			HugepageSizeBytes: 2 << 20,
		},
	}
}

// SetExecHandler overrides Exec's default echo behaviour. It receives
// the whole request, so tests can observe the process environment.
func (f *Fake) SetExecHandler(h func(sandboxID string, req substrate.ExecRequest) substrate.ExecResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execHandler = h
}

// SetStartHandler overrides Start. Tests use it to capture the returned
// substrate.Process (to script background-job exits, 2.6 #135); nil
// restores the default empty process.
func (f *Fake) SetStartHandler(h func(sandboxID string, req substrate.StartRequest) (substrate.Process, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startHandler = h
}

// StartContext returns the context the most recent Start was called
// with. A background job's start context must outlive the HTTP request
// that asked for it, so a test can cancel that request and pin that the
// context the substrate holds stays live (2.6 #135).
func (f *Fake) StartContext() context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startCtx
}

// EnableJobRunner makes Start actually run background jobs (2.6, #135)
// instead of returning an inert process: a command whose argv is the
// spoond job wrapper is executed for real, writing its stdout/stderr/rc
// files into host directory dir, and the fake hands the test a process
// it drives. req.Env's SPOOND_JOBS_DIR and SPOOND_SECRETS_DIR are
// rewritten to subdirectories of dir, so the wrapper never touches the
// test host's /var/lib/spoond or /run/secrets. Jobs started this way are
// listed by JobProcess and JobState. Call before any job starts; nil
// disables the runner.
func (f *Fake) EnableJobRunner(dir string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobDir = dir
}

// EnableExecRunner makes Exec actually run its argv (through exec, the
// way envd's process API does) with req.Env in the process environment,
// instead of echoing the argv. Tests enable it to pin that exec env
// reaches the command and never appears in argv. DisableExecRunner
// restores the echo default.
func (f *Fake) EnableExecRunner() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execRunner = true
}

// DisableExecRunner restores Exec's default echo behaviour.
func (f *Fake) DisableExecRunner() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execRunner = false
}

// JobProcess returns the process the fake created for the background
// job with this job id, or nil. The test drives its exit by finishing
// the real command and pushing a ProcessEvent, or simply lets the real
// process write rc and waits with JobDone.
func (f *Fake) JobProcess(jobID string) *FakeProcess {
	f.mu.Lock()
	defer f.mu.Unlock()
	pid, ok := f.jobPIDOf[jobID]
	if !ok {
		return nil
	}
	return f.jobProcs[pid]
}

// JobDone reports whether the wrapper for jobID has written rc (the
// guest-side source of truth).
func (f *Fake) JobDone(jobID string) bool {
	f.mu.Lock()
	dir := f.jobDir
	f.mu.Unlock()
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "jobs", jobID, "rc"))
	return err == nil
}

// JobPID returns the command pid the wrapper recorded for jobID.
func (f *Fake) JobPID(jobID string) (int, bool) {
	f.mu.Lock()
	dir := f.jobDir
	f.mu.Unlock()
	if dir == "" {
		return 0, false
	}
	b, err := os.ReadFile(filepath.Join(dir, "jobs", jobID, "pid"))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, false
	}
	return pid, true
}

// KillProcessGroup signals a process group, the way the guest /bin/kill
// does for a job's pid (2.6, #135).
func (f *Fake) KillProcessGroup(pid int, kill bool) error {
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	return syscall.Kill(-pid, sig)
}

// StopAllJobs kills every wrapper-run job's process group and removes
// the runner's temp files. Tests register it as cleanup so a long-running
// job (a `sleep 600`) does not outlive the suite.
func (f *Fake) StopAllJobs() {
	f.mu.Lock()
	dir := f.jobDir
	jobs := make([]string, 0, len(f.jobPIDOf))
	for id := range f.jobPIDOf {
		jobs = append(jobs, id)
	}
	f.mu.Unlock()
	for _, id := range jobs {
		if pid, ok := f.JobPID(id); ok {
			_ = f.KillProcessGroup(pid, true)
		}
	}
	if dir != "" {
		_ = os.RemoveAll(filepath.Join(dir, "jobs"))
		_ = os.RemoveAll(filepath.Join(dir, "secrets"))
	}
}

// JobState is what a finished (or running) real-wrapper job left in its
// host directory: the rc contents ("" when still running) and the
// stdout/stderr bytes. A missing file is empty.
func (f *Fake) JobState(jobID string) (rc, stdout, stderr string, err error) {
	f.mu.Lock()
	dir := f.jobDir
	f.mu.Unlock()
	if dir == "" {
		return "", "", "", fmt.Errorf("fake: job runner is not enabled")
	}
	read := func(name string) string {
		b, e := os.ReadFile(filepath.Join(dir, "jobs", jobID, name))
		if e != nil {
			return ""
		}
		return string(b)
	}
	return read("rc"), read("stdout"), read("stderr"), nil
}

// LastExec returns the most recent Exec request, so tests can observe
// its argv and environment.
func (f *Fake) LastExec() substrate.ExecRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastExec
}

// SetNodeInfo fixes what NodeInfo returns.
func (f *Fake) SetNodeInfo(info substrate.NodeInfo, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodeInfo, f.nodeErr = info, err
}

// FailCall makes the nth (1-based) call of method return err; nth 0 = every
// call.
func (f *Fake) FailCall(method string, nth int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails = append(f.fails, failSpec{method: method, nth: nth, err: err})
}

// SetHealthErr makes Health(sandboxID) return err.
func (f *Fake) SetHealthErr(sandboxID string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.healthErrs[sandboxID] = err
}

// Kill removes a sandbox from List without a Delete call (simulates a crash).
func (f *Fake) Kill(sandboxID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sandboxes, sandboxID)
}

// Proc returns the process started with this pid.
func (f *Fake) Proc(pid uint32) *FakeProcess {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.procs[pid]
}

// CallLog returns a copy of Calls, taken under the fake's lock, so tests
// can read it while handlers are still calling the fake.
func (f *Fake) CallLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Calls...)
}

func (f *Fake) record(method, firstID string) error {
	entry := strings.TrimRight(method+" "+firstID, " ")
	f.Calls = append(f.Calls, entry)
	n := 0 // 1-based index of this call among calls of this method
	for _, c := range f.Calls {
		if c == method || strings.HasPrefix(c, method+" ") {
			n++
		}
	}
	for _, spec := range f.fails {
		if spec.method == method && (spec.nth == 0 || spec.nth == n) {
			return spec.err
		}
	}
	return nil
}

func (f *Fake) BuildTemplate(ctx context.Context, req substrate.BuildRequest) (substrate.BuildResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("BuildTemplate", req.TemplateID); err != nil {
		return substrate.BuildResult{}, err
	}
	return substrate.BuildResult{BuildID: req.BuildID}, nil
}

func (f *Fake) DeleteBuild(ctx context.Context, templateID, buildID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.record("DeleteBuild", templateID)
}

func (f *Fake) Create(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Create", req.SandboxID); err != nil {
		return substrate.Sandbox{}, err
	}
	sb := substrate.Sandbox{
		ID:          req.SandboxID,
		ExecutionID: uuid.NewString(),
		TemplateID:  req.TemplateID,
		BuildID:     req.BuildID,
		VCPU:        req.VCPU,
		MemoryMB:    req.MemoryMB,
		StartedAt:   time.Now(),
		EndAt:       req.EndAt,
	}
	f.sandboxes[sb.ID] = sb
	return sb, nil
}

func (f *Fake) List(ctx context.Context) ([]substrate.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("List", ""); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(f.sandboxes))
	for id := range f.sandboxes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]substrate.Sandbox, 0, len(ids))
	for _, id := range ids {
		out = append(out, f.sandboxes[id])
	}
	return out, nil
}

func (f *Fake) Delete(ctx context.Context, sandboxID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Delete", sandboxID); err != nil {
		return err
	}
	delete(f.sandboxes, sandboxID) // nil when already gone
	delete(f.files, sandboxID)     // the sandbox's files go with it
	return nil
}

func (f *Fake) Pause(ctx context.Context, sandboxID, templateID string) (string, substrate.BuildRefs, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Pause", sandboxID); err != nil {
		return "", substrate.BuildRefs{}, err
	}
	delete(f.sandboxes, sandboxID)
	return uuid.NewString(), substrate.BuildRefs{}, nil
}

func (f *Fake) Checkpoint(ctx context.Context, sandboxID string) (string, substrate.BuildRefs, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Checkpoint", sandboxID); err != nil {
		return "", substrate.BuildRefs{}, err
	}
	return uuid.NewString(), substrate.BuildRefs{}, nil
}

func (f *Fake) UpdateEgress(ctx context.Context, sandboxID string, eg substrate.Egress) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.record("UpdateEgress", sandboxID)
}

func (f *Fake) UpdateEndAt(ctx context.Context, sandboxID string, endAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sb, ok := f.sandboxes[sandboxID]; ok {
		sb.EndAt = endAt
		f.sandboxes[sandboxID] = sb
	}
	return f.record("UpdateEndAt", sandboxID)
}

func (f *Fake) NodeInfo(ctx context.Context) (substrate.NodeInfo, error) {
	f.mu.Lock()
	if f.nodeInfoFn != nil {
		fn := f.nodeInfoFn
		f.mu.Unlock()
		if err := f.record("NodeInfo", ""); err != nil {
			return substrate.NodeInfo{}, err
		}
		return fn(ctx)
	}
	defer f.mu.Unlock()
	if err := f.record("NodeInfo", ""); err != nil {
		return substrate.NodeInfo{}, err
	}
	return f.nodeInfo, f.nodeErr
}

// SetNodeInfoFunc overrides NodeInfo with a function (a test that needs
// the call to hang until its context is cancelled, say). Clear it with
// SetNodeInfo.
func (f *Fake) SetNodeInfoFunc(fn func(ctx context.Context) (substrate.NodeInfo, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodeInfoFn = fn
}

func (f *Fake) SetDraining(ctx context.Context, draining bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("SetDraining", ""); err != nil {
		return err
	}
	if draining {
		f.nodeInfo.Status = "draining"
	} else {
		f.nodeInfo.Status = "healthy"
	}
	return nil
}

func (f *Fake) Health(ctx context.Context, sandboxID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Health", sandboxID); err != nil {
		return err
	}
	return f.healthErrs[sandboxID]
}

func (f *Fake) Exec(ctx context.Context, sandboxID string, req substrate.ExecRequest) (substrate.ExecResult, error) {
	f.mu.Lock()
	if err := f.record("Exec", sandboxID); err != nil {
		f.mu.Unlock()
		return substrate.ExecResult{}, err
	}
	f.lastExec = req
	h := f.execHandler
	runner := f.execRunner
	f.mu.Unlock()
	if h != nil {
		return h(sandboxID, req), nil
	}
	// When the exec runner is enabled, run the argv for real so req.Env
	// reaches the command the way envd's process API does.
	if runner {
		return f.runExec(req)
	}
	return substrate.ExecResult{Stdout: strings.Join(req.Args, " "), ExitCode: 0}, nil
}

// runExec executes an ExecRequest through exec, passing req.Env in the
// process environment and collecting stdout, stderr and the exit code.
// It backs the fake's optional exec runner (EnableExecRunner); secrets
// and cwd are intentionally out of scope (Exec has no cwd field — spoond
// passes `cd` in the argv).
func (f *Fake) runExec(req substrate.ExecRequest) (substrate.ExecResult, error) {
	if len(req.Args) == 0 {
		return substrate.ExecResult{ExitCode: 1}, nil
	}
	cmd := exec.Command(req.Args[0], req.Args[1:]...)
	cmd.Env = os.Environ()
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			return substrate.ExecResult{}, err
		}
	}
	return substrate.ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exitCode}, nil
}

func (f *Fake) Start(ctx context.Context, sandboxID string, req substrate.StartRequest) (substrate.Process, error) {
	f.mu.Lock()
	if err := f.record("Start", sandboxID); err != nil {
		f.mu.Unlock()
		return nil, err
	}
	f.startCtx = ctx
	h := f.startHandler
	runner := f.jobDir
	f.mu.Unlock()
	if h != nil {
		return h(sandboxID, req)
	}
	if runner != "" && isJobWrapperArgs(req.Args) {
		return f.startJobProcess(runner, req)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextPID++
	pid := f.nextPID
	p := newFakeProcess(pid)
	f.procs[pid] = p
	p.Push(substrate.ProcessEvent{Kind: substrate.EventStarted, PID: pid})
	return p, nil
}

// isJobWrapperArgs reports whether args are the spoond background-job
// wrapper invocation: /bin/bash -c <script> spoond-job <job_id> <cmd...>.
func isJobWrapperArgs(args []string) bool {
	return len(args) >= 5 && args[0] == "/bin/bash" && args[1] == "-c" && args[3] == "spoond-job"
}

// The production guest paths a real-wrapper job's files live under. When
// the runner is enabled the fake maps these onto host files so the actual
// wrapper script, its setsid child, the rc.tmp+rename write and the
// secrets cleanup all run for real (2.6, #135).
const (
	fakeJobsDir    = "/var/lib/spoond/jobs"
	fakeSecretsDir = "/run/secrets"
)

// startJobProcess runs a background-job wrapper for real: the wrapper
// script (req.Args[2]) is executed through bash with the job id and the
// command argv, writing the job's files into host dir. The returned
// process carries the wrapper's exit on its stream.
func (f *Fake) startJobProcess(dir string, req substrate.StartRequest) (substrate.Process, error) {
	jobID := req.Args[4]
	cmd := append([]string(nil), req.Args[5:]...)
	env := map[string]string{}
	for k, v := range req.Env {
		env[k] = v
	}
	env["SPOOND_JOBS_DIR"] = filepath.Join(dir, "jobs")
	env["SPOOND_SECRETS_DIR"] = filepath.Join(dir, "secrets")
	script := req.Args[2]

	f.mu.Lock()
	f.nextPID++
	pid := f.nextPID
	p := newFakeProcess(pid)
	f.procs[pid] = p
	f.jobProcs[pid] = p
	f.jobPIDOf[jobID] = pid
	f.mu.Unlock()
	p.Push(substrate.ProcessEvent{Kind: substrate.EventStarted, PID: pid})

	go func() {
		runRealWrapper(script, jobID, cmd, env)
		rc := 0
		if b, err := os.ReadFile(filepath.Join(dir, "jobs", jobID, "rc")); err == nil {
			rc, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		p.Push(substrate.ProcessEvent{Kind: substrate.EventExit, ExitCode: rc})
	}()
	return p, nil
}

// runRealWrapper executes the guest wrapper script through bash.
func runRealWrapper(script, jobID string, cmd []string, env map[string]string) {
	args := []string{"-c", script, "spoond-job", jobID}
	args = append(args, cmd...)
	c := exec.Command("/bin/bash", args...)
	c.Env = os.Environ()
	for k, v := range env {
		c.Env = append(c.Env, k+"="+v)
	}
	_ = c.Run()
}

// hostFilePathLocked maps a production job or secret path onto the
// runner's host directory. Call with f.mu held. Returns ok=false when the
// runner is disabled or the path is not one of the mapped trees.
func (f *Fake) hostFilePathLocked(name string) (string, bool) {
	if f.jobDir == "" {
		return "", false
	}
	switch {
	case name == fakeJobsDir:
		return filepath.Join(f.jobDir, "jobs"), true
	case strings.HasPrefix(name, fakeJobsDir+"/"):
		return filepath.Join(f.jobDir, "jobs", strings.TrimPrefix(name, fakeJobsDir+"/")), true
	case name == fakeSecretsDir:
		return filepath.Join(f.jobDir, "secrets"), true
	case strings.HasPrefix(name, fakeSecretsDir+"/"):
		return filepath.Join(f.jobDir, "secrets", strings.TrimPrefix(name, fakeSecretsDir+"/")), true
	}
	return "", false
}

func fakeReadHost(path string, max int64) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", substrate.ErrNotFound, path)
		}
		return nil, err
	}
	if max >= 0 && int64(len(data)) > max {
		return nil, fmt.Errorf("%w: %s is %d bytes, max %d", substrate.ErrTooLarge, path, len(data), max)
	}
	return data, nil
}

func fakeReadHostRange(path string, offset, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", substrate.ErrNotFound, path)
		}
		return nil, err
	}
	defer f.Close()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, err
		}
	}
	if limit <= 0 {
		return io.ReadAll(f)
	}
	return io.ReadAll(io.LimitReader(f, limit))
}

// DialGuest opens a real TCP connection to hostIP:port. The fake has no
// orchestrator behind it, so tests point hostIP at an in-process listener
// (an echo server on 127.0.0.1); the call is recorded like every other
// method, so FailCall can simulate dial failures.
func (f *Fake) DialGuest(ctx context.Context, sandboxID, hostIP string, port int) (net.Conn, error) {
	f.mu.Lock()
	if err := f.record("DialGuest", sandboxID); err != nil {
		f.mu.Unlock()
		return nil, err
	}
	f.mu.Unlock()
	return net.DialTimeout("tcp", net.JoinHostPort(hostIP, strconv.Itoa(port)), 5*time.Second)
}

func (f *Fake) TrafficToken(sandboxID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = f.record("TrafficToken", sandboxID)
	return "fake-traffic-" + sandboxID
}

// fileFS returns the sandbox's in-memory filesystem, creating an empty one
// when the sandbox is running. nil when the sandbox is unknown.
func (f *Fake) fileFS(sandboxID string) *memFS {
	if _, ok := f.sandboxes[sandboxID]; !ok {
		return nil
	}
	fs, ok := f.files[sandboxID]
	if !ok {
		fs = newMemFS()
		f.files[sandboxID] = fs
	}
	return fs
}

func (f *Fake) WriteFile(ctx context.Context, sandboxID, name string, data []byte, mode os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("WriteFile", sandboxID); err != nil {
		return err
	}
	if host, ok := f.hostFilePathLocked(name); ok {
		if _, exists := f.sandboxes[sandboxID]; !exists {
			return fmt.Errorf("fake: WriteFile %s %s: %w", sandboxID, name, substrate.ErrNotFound)
		}
		if err := os.MkdirAll(filepath.Dir(host), 0o755); err != nil {
			return err
		}
		if mode == 0 {
			mode = 0o644
		}
		return os.WriteFile(host, data, mode.Perm())
	}
	fs := f.fileFS(sandboxID)
	if fs == nil {
		return fmt.Errorf("fake: WriteFile %s %s: %w", sandboxID, name, substrate.ErrNotFound)
	}
	return fs.write(name, data, mode)
}

func (f *Fake) ReadFile(ctx context.Context, sandboxID, name string, max int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("ReadFile", sandboxID); err != nil {
		return nil, err
	}
	if host, ok := f.hostFilePathLocked(name); ok {
		if _, exists := f.sandboxes[sandboxID]; !exists {
			return nil, fmt.Errorf("fake: ReadFile %s %s: %w", sandboxID, name, substrate.ErrNotFound)
		}
		return fakeReadHost(host, max)
	}
	fs := f.fileFS(sandboxID)
	if fs == nil {
		return nil, fmt.Errorf("fake: ReadFile %s %s: %w", sandboxID, name, substrate.ErrNotFound)
	}
	return fs.read(name, max)
}

// ReadFileRange reads at most limit bytes of name starting at offset,
// for the background-job output endpoints (2.6, #135). A missing file
// wraps substrate.ErrNotFound; an offset past the end yields no bytes.
func (f *Fake) ReadFileRange(ctx context.Context, sandboxID, name string, offset, limit int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("ReadFileRange", sandboxID); err != nil {
		return nil, err
	}
	if host, ok := f.hostFilePathLocked(name); ok {
		if _, exists := f.sandboxes[sandboxID]; !exists {
			return nil, fmt.Errorf("fake: ReadFileRange %s %s: %w", sandboxID, name, substrate.ErrNotFound)
		}
		return fakeReadHostRange(host, offset, limit)
	}
	fs := f.fileFS(sandboxID)
	if fs == nil {
		return nil, fmt.Errorf("fake: ReadFileRange %s %s: %w", sandboxID, name, substrate.ErrNotFound)
	}
	data, err := fs.read(name, int64(1<<62))
	if err != nil {
		return nil, err
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= int64(len(data)) {
		return nil, nil
	}
	end := offset + limit
	if limit <= 0 || end > int64(len(data)) {
		end = int64(len(data))
	}
	return bytes.Clone(data[offset:end]), nil
}

func (f *Fake) Stat(ctx context.Context, sandboxID, name string) (substrate.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Stat", sandboxID); err != nil {
		return substrate.FileInfo{}, err
	}
	if host, ok := f.hostFilePathLocked(name); ok {
		if _, exists := f.sandboxes[sandboxID]; !exists {
			return substrate.FileInfo{}, fmt.Errorf("fake: Stat %s %s: %w", sandboxID, name, substrate.ErrNotFound)
		}
		info, err := os.Stat(host)
		if err != nil {
			if os.IsNotExist(err) {
				return substrate.FileInfo{}, fmt.Errorf("%w: %s", substrate.ErrNotFound, name)
			}
			return substrate.FileInfo{}, err
		}
		return substrate.FileInfo{
			Name:    info.Name(),
			Size:    info.Size(),
			Mode:    info.Mode(),
			ModTime: info.ModTime(),
			IsDir:   info.IsDir(),
		}, nil
	}
	fs := f.fileFS(sandboxID)
	if fs == nil {
		return substrate.FileInfo{}, fmt.Errorf("fake: Stat %s %s: %w", sandboxID, name, substrate.ErrNotFound)
	}
	return fs.stat(name)
}

func (f *Fake) MakeDir(ctx context.Context, sandboxID, name string, mode os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("MakeDir", sandboxID); err != nil {
		return err
	}
	fs := f.fileFS(sandboxID)
	if fs == nil {
		return fmt.Errorf("fake: MakeDir %s %s: %w", sandboxID, name, substrate.ErrNotFound)
	}
	return fs.mkdir(name, mode)
}

func (f *Fake) Remove(ctx context.Context, sandboxID, name string, recursive bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Remove", sandboxID); err != nil {
		return err
	}
	if host, ok := f.hostFilePathLocked(name); ok {
		if _, exists := f.sandboxes[sandboxID]; !exists {
			return fmt.Errorf("fake: Remove %s %s: %w", sandboxID, name, substrate.ErrNotFound)
		}
		if recursive {
			return os.RemoveAll(host)
		}
		if err := os.Remove(host); err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("%w: %s", substrate.ErrNotFound, name)
			}
			return err
		}
		return nil
	}
	fs := f.fileFS(sandboxID)
	if fs == nil {
		return fmt.Errorf("fake: Remove %s %s: %w", sandboxID, name, substrate.ErrNotFound)
	}
	return fs.remove(name, recursive)
}

// Rename moves oldPath to newPath, replacing newPath, through the
// in-memory filesystem. It backs the atomic guest-file writes (2.7,
// #83). A missing source wraps substrate.ErrNotFound.
func (f *Fake) Rename(ctx context.Context, sandboxID, oldPath, newPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Rename", sandboxID); err != nil {
		return err
	}
	fs := f.fileFS(sandboxID)
	if fs == nil {
		return fmt.Errorf("fake: Rename %s %s: %w", sandboxID, oldPath, substrate.ErrNotFound)
	}
	return fs.rename(oldPath, newPath)
}

// memFS is an in-memory filesystem with real path semantics: absolute,
// cleaned paths only, directories are explicit nodes, parents are created as
// needed, and modes are kept exactly as given. The root is "" (meaning "/")
// and always exists.
type memFS struct {
	dirs  map[string]os.FileMode // "" is the root, always present
	files map[string]memFile
}

type memFile struct {
	data    []byte
	mode    os.FileMode
	modTime time.Time
}

func newMemFS() *memFS {
	return &memFS{
		dirs:  map[string]os.FileMode{"": 0o755 | os.ModeDir},
		files: map[string]memFile{},
	}
}

// cleanPath normalizes a guest path to the map's key form: absolute and
// cleaned, "/" collapses to "". An empty path stays empty (an error at the
// callers).
func cleanPath(name string) string {
	if name == "" {
		return ""
	}
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	if path.Clean(name) == "/" {
		return ""
	}
	return path.Clean(name)
}

// mkdirAll creates name and every missing parent with mode; existing nodes
// are left alone. name must be clean. The error is ErrNotDir when a file sits
// in the way (ErrExist would suggest overwrite semantics that do not exist
// here).
func (m *memFS) mkdirAll(name string, mode os.FileMode) error {
	parts := strings.Split(strings.TrimPrefix(name, "/"), "/")
	cur := ""
	for _, p := range parts {
		cur = cur + "/" + p
		if _, ok := m.files[cur]; ok {
			return fmt.Errorf("%w: %s", substrate.ErrNotDir, cur)
		}
		if _, ok := m.dirs[cur]; ok {
			continue
		}
		m.dirs[cur] = mode | os.ModeDir
	}
	return nil
}

func (m *memFS) write(name string, data []byte, mode os.FileMode) error {
	name = cleanPath(name)
	if name == "" {
		return fmt.Errorf("%w: %s is a directory path", substrate.ErrInvalidOp, name)
	}
	if _, ok := m.dirs[name]; ok {
		return fmt.Errorf("%w: %s is a directory", substrate.ErrInvalidOp, name)
	}
	if mode == 0 {
		mode = 0o644
	}
	if err := m.mkdirAll(path.Dir(name), 0o755); err != nil {
		return err
	}
	m.files[name] = memFile{data: bytes.Clone(data), mode: mode, modTime: time.Now()}
	return nil
}

func (m *memFS) read(name string, max int64) ([]byte, error) {
	name = cleanPath(name)
	if _, ok := m.dirs[name]; ok {
		return nil, fmt.Errorf("%w: %s is a directory", substrate.ErrInvalidOp, name)
	}
	f, ok := m.files[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", substrate.ErrNotFound, name)
	}
	if int64(len(f.data)) > max {
		return nil, fmt.Errorf("%w: %s is %d bytes, max %d", substrate.ErrTooLarge, name, len(f.data), max)
	}
	return bytes.Clone(f.data), nil
}

func (m *memFS) stat(name string) (substrate.FileInfo, error) {
	name = cleanPath(name)
	if mode, ok := m.dirs[name]; ok {
		return substrate.FileInfo{
			Name:    path.Base(name),
			Mode:    mode,
			IsDir:   true,
			ModTime: time.Time{},
		}, nil
	}
	if f, ok := m.files[name]; ok {
		return substrate.FileInfo{
			Name:    path.Base(name),
			Size:    int64(len(f.data)),
			Mode:    f.mode,
			ModTime: f.modTime,
		}, nil
	}
	return substrate.FileInfo{}, fmt.Errorf("%w: %s", substrate.ErrNotFound, name)
}

func (m *memFS) mkdir(name string, mode os.FileMode) error {
	name = cleanPath(name)
	if name == "" {
		return nil // the root always exists
	}
	if mode == 0 {
		mode = 0o755
	}
	if _, ok := m.files[name]; ok {
		return fmt.Errorf("%w: %s", substrate.ErrExist, name)
	}
	return m.mkdirAll(name, mode)
}

// rename moves oldName to newName, replacing newName. Only files are
// moved (the guest builders write files); directories are an error.
func (m *memFS) rename(oldName, newName string) error {
	oldName, newName = cleanPath(oldName), cleanPath(newName)
	f, ok := m.files[oldName]
	if !ok {
		return fmt.Errorf("%w: %s", substrate.ErrNotFound, oldName)
	}
	if _, ok := m.dirs[newName]; ok {
		return fmt.Errorf("%w: %s is a directory", substrate.ErrInvalidOp, newName)
	}
	if err := m.mkdirAll(path.Dir(newName), 0o755); err != nil {
		return err
	}
	delete(m.files, oldName)
	m.files[newName] = f
	return nil
}

func (m *memFS) remove(name string, recursive bool) error {
	name = cleanPath(name)
	if name == "" {
		return fmt.Errorf("%w: refusing to remove the root", substrate.ErrInvalidOp)
	}
	if _, ok := m.files[name]; ok {
		delete(m.files, name)
		return nil
	}
	if _, ok := m.dirs[name]; !ok {
		return fmt.Errorf("%w: %s", substrate.ErrNotFound, name)
	}
	prefix := name + "/"
	for f := range m.files {
		if strings.HasPrefix(f, prefix) {
			if !recursive {
				return fmt.Errorf("%w: %s", substrate.ErrNotEmpty, name)
			}
			delete(m.files, f)
		}
	}
	for d := range m.dirs {
		if strings.HasPrefix(d, prefix) {
			if !recursive {
				return fmt.Errorf("%w: %s", substrate.ErrNotEmpty, name)
			}
			delete(m.dirs, d)
		}
	}
	delete(m.dirs, name)
	return nil
}

// FakeProcess is the substrate.Process returned by Fake.Start.
type FakeProcess struct {
	// Inputs holds every Write.
	Inputs [][]byte
	// Resizes holds every Resize as {cols, rows}.
	Resizes [][2]uint32
	// Signals holds every Signal (true = SIGKILL).
	Signals []bool
	// StdinClosed is set by CloseStdin.
	StdinClosed bool
	// Closed is set by Close, EventExit or EventError.
	Closed bool

	pid    uint32
	events chan substrate.ProcessEvent
	mu     sync.Mutex
}

// ProcState is a copy of a FakeProcess's recorded actions.
type ProcState struct {
	Inputs      [][]byte
	Resizes     [][2]uint32
	Signals     []bool
	StdinClosed bool
	Closed      bool
}

// State returns a copy of the recorded actions, taken under the process's
// lock, so tests can poll it while the relay is still writing.
func (p *FakeProcess) State() ProcState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return ProcState{
		Inputs:      append([][]byte(nil), p.Inputs...),
		Resizes:     append([][2]uint32(nil), p.Resizes...),
		Signals:     append([]bool(nil), p.Signals...),
		StdinClosed: p.StdinClosed,
		Closed:      p.Closed,
	}
}

func newFakeProcess(pid uint32) *FakeProcess {
	return &FakeProcess{pid: pid, events: make(chan substrate.ProcessEvent, 256)}
}

// NewProcess builds a detached FakeProcess a test can drive directly
// (push events, inspect Signals) without going through Start. Used by
// background-job tests (2.6, #135) that script an exit on their own
// schedule.
func NewProcess(pid uint32) *FakeProcess { return newFakeProcess(pid) }

// Push delivers an event to Events(); a pushed EventExit or EventError closes
// the channel.
func (p *FakeProcess) Push(ev substrate.ProcessEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Closed {
		return
	}
	p.events <- ev
	if ev.Kind == substrate.EventExit || ev.Kind == substrate.EventError {
		p.Closed = true
		close(p.events)
	}
}

func (p *FakeProcess) Events() <-chan substrate.ProcessEvent { return p.events }

func (p *FakeProcess) Write(data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Inputs = append(p.Inputs, data)
	return nil
}

func (p *FakeProcess) Resize(cols, rows uint32) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Resizes = append(p.Resizes, [2]uint32{cols, rows})
	return nil
}

func (p *FakeProcess) Signal(kill bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Signals = append(p.Signals, kill)
	return nil
}

func (p *FakeProcess) CloseStdin() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.StdinClosed = true
	return nil
}

func (p *FakeProcess) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.Closed {
		p.Closed = true
		close(p.events)
	}
	return nil
}
