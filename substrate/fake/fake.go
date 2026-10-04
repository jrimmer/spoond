// Package fake provides an in-memory substrate.Substrate for unit tests.
package fake

import (
	"context"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	procs     map[uint32]*FakeProcess
	nextPID   uint32

	execHandler func(sandboxID string, args []string) substrate.ExecResult
	nodeInfo    substrate.NodeInfo
	nodeErr     error
	healthErrs  map[string]error
	fails       []failSpec
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
		procs:      map[uint32]*FakeProcess{},
		healthErrs: map[string]error{},
		nodeInfo: substrate.NodeInfo{
			Status:            "healthy",
			HugepagesTotal:    1 << 20,
			HugepageSizeBytes: 2 << 20,
		},
	}
}

// SetExecHandler overrides Exec's default echo behaviour.
func (f *Fake) SetExecHandler(h func(sandboxID string, args []string) substrate.ExecResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execHandler = h
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
	defer f.mu.Unlock()
	if err := f.record("NodeInfo", ""); err != nil {
		return substrate.NodeInfo{}, err
	}
	return f.nodeInfo, f.nodeErr
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
	h := f.execHandler
	f.mu.Unlock()
	if h != nil {
		return h(sandboxID, req.Args), nil
	}
	return substrate.ExecResult{Stdout: strings.Join(req.Args, " "), ExitCode: 0}, nil
}

func (f *Fake) Start(ctx context.Context, sandboxID string, req substrate.StartRequest) (substrate.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Start", sandboxID); err != nil {
		return nil, err
	}
	f.nextPID++
	pid := f.nextPID
	p := newFakeProcess(pid)
	f.procs[pid] = p
	p.Push(substrate.ProcessEvent{Kind: substrate.EventStarted, PID: pid})
	return p, nil
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
