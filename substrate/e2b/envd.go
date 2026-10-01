package e2b

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/jrimmer/spoond/substrate"
	"github.com/jrimmer/spoond/substrate/e2b/gen/envd/process"
	"github.com/jrimmer/spoond/substrate/e2b/gen/envd/process/processconnect"
)

// envdPort is the guest port envd listens on; requests to it go through the
// orchestrator sandbox proxy (A2 §4, §5).
const envdPort = 49983

const (
	defaultExecTimeout = 30 * time.Second
	execKillGrace      = 2 * time.Second
)

// envdHeaders carries the per-sandbox routing and auth headers added to every
// proxied envd request.
type envdHeaders struct {
	sandboxID string
	token     string
	user      string // never empty; defaults to root
}

func (h *envdHeaders) set(headers http.Header) {
	headers.Set("E2b-Sandbox-Id", h.sandboxID)
	headers.Set("E2b-Sandbox-Port", strconv.Itoa(envdPort))
	headers.Set("X-Access-Token", h.token)
	headers.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(h.user+":")))
}

// WrapUnary implements connect.Interceptor for unary calls.
func (h *envdHeaders) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		h.set(req.Header())
		return next(ctx, req)
	}
}

// WrapStreamingClient implements connect.Interceptor for client streaming
// calls (Start).
func (h *envdHeaders) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		h.set(conn.RequestHeader())
		return conn
	}
}

// WrapStreamingHandler implements connect.Interceptor; unused client-side.
func (h *envdHeaders) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// envdTransport is an http.RoundTripper that adds the envd headers to every
// request, including plain HTTP calls like GET /health.
type envdTransport struct {
	base http.RoundTripper
	h    envdHeaders
}

func (t *envdTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	t.h.set(r.Header)
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}

func defaultUser(user string) string {
	if user == "" {
		return "root"
	}
	return user
}

// envdClient returns an HTTP client that speaks to one sandbox's envd through
// the orchestrator proxy.
func (c *Client) envdClient(sandboxID, user string) *http.Client {
	h := envdHeaders{sandboxID: sandboxID, token: c.EnvdToken(sandboxID), user: defaultUser(user)}
	return &http.Client{Transport: &envdTransport{base: c.http.Transport, h: h}}
}

// envdProcess returns the Connect client for one sandbox's envd process
// service.
func (c *Client) envdProcess(sandboxID, user string) processconnect.ProcessClient {
	h := envdHeaders{sandboxID: sandboxID, token: c.EnvdToken(sandboxID), user: defaultUser(user)}
	hc := &http.Client{Transport: &envdTransport{base: c.http.Transport, h: h}}
	return processconnect.NewProcessClient(hc, c.cfg.ProxyURL, connect.WithInterceptors(&h))
}

// Health checks envd's GET /health through the proxy.
func (c *Client) Health(ctx context.Context, sandboxID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.cfg.ProxyURL, "/")+"/health", nil)
	if err != nil {
		return fmt.Errorf("e2b: health %s: %w", sandboxID, err)
	}
	resp, err := c.envdClient(sandboxID, "").Do(req)
	if err != nil {
		return fmt.Errorf("e2b: health %s: %w", sandboxID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return fmt.Errorf("e2b: health %s: unexpected status %s", sandboxID, resp.Status)
}

// Start starts an interactive guest process.
func (c *Client) Start(ctx context.Context, sandboxID string, req substrate.StartRequest) (substrate.Process, error) {
	p, err := c.startProcess(ctx, sandboxID, req)
	if err != nil {
		if !c.listed(ctx, sandboxID) {
			return nil, fmt.Errorf("%w: %v", substrate.ErrNotFound, err)
		}
		return nil, err
	}
	return p, nil
}

func (c *Client) startProcess(ctx context.Context, sandboxID string, req substrate.StartRequest) (*guestProcess, error) {
	if len(req.Args) == 0 {
		return nil, fmt.Errorf("e2b: start %s: no args", sandboxID)
	}
	cfg := &process.ProcessConfig{
		Cmd:  req.Args[0],
		Args: req.Args[1:],
		Envs: req.Env,
	}
	if req.Cwd != "" {
		cfg.Cwd = proto.String(req.Cwd)
	}
	start := &process.StartRequest{
		Process: cfg,
		Stdin:   proto.Bool(req.Stdin),
	}
	if req.PTY {
		cols, rows := req.Cols, req.Rows
		if cols == 0 {
			cols = 80
		}
		if rows == 0 {
			rows = 24
		}
		start.Pty = &process.PTY{Size: &process.PTY_Size{Cols: cols, Rows: rows}}
	}
	pctx, cancel := context.WithCancel(ctx)
	proc := c.envdProcess(sandboxID, req.User)
	stream, err := proc.Start(pctx, connect.NewRequest(start))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("e2b: start %s: %w", sandboxID, err)
	}
	p := &guestProcess{
		client:    proc,
		ctx:       pctx,
		cancel:    cancel,
		sandboxID: sandboxID,
		pty:       req.PTY,
		stream:    stream,
		pidReady:  make(chan struct{}),
		events:    make(chan substrate.ProcessEvent, 64),
	}
	go p.readLoop()
	return p, nil
}

func pidSelector(pid uint32) *process.ProcessSelector {
	return &process.ProcessSelector{Selector: &process.ProcessSelector_Pid{Pid: pid}}
}

// guestProcess implements substrate.Process over an envd Start stream.
type guestProcess struct {
	client    processconnect.ProcessClient
	ctx       context.Context
	cancel    context.CancelFunc
	sandboxID string
	pty       bool
	stream    *connect.ServerStreamForClient[process.StartResponse]

	mu       sync.Mutex
	pid      uint32
	pidReady chan struct{}

	events chan substrate.ProcessEvent
}

func (p *guestProcess) Events() <-chan substrate.ProcessEvent { return p.events }

func (p *guestProcess) readLoop() {
	defer close(p.events)
	for p.stream.Receive() {
		ev := p.stream.Msg().GetEvent()
		switch e := ev.GetEvent().(type) {
		case *process.ProcessEvent_Start:
			pid := e.Start.GetPid()
			p.mu.Lock()
			if p.pid == 0 {
				p.pid = pid
				close(p.pidReady)
			}
			p.mu.Unlock()
			p.send(substrate.ProcessEvent{Kind: substrate.EventStarted, PID: pid})
		case *process.ProcessEvent_Data:
			switch o := e.Data.GetOutput().(type) {
			case *process.ProcessEvent_DataEvent_Stdout:
				p.send(substrate.ProcessEvent{Kind: substrate.EventStdout, Data: bytes.Clone(o.Stdout)})
			case *process.ProcessEvent_DataEvent_Stderr:
				p.send(substrate.ProcessEvent{Kind: substrate.EventStderr, Data: bytes.Clone(o.Stderr)})
			case *process.ProcessEvent_DataEvent_Pty:
				p.send(substrate.ProcessEvent{Kind: substrate.EventPTY, Data: bytes.Clone(o.Pty)})
			}
		case *process.ProcessEvent_End:
			p.send(substrate.ProcessEvent{Kind: substrate.EventExit, ExitCode: int(e.End.GetExitCode())})
		case *process.ProcessEvent_Keepalive:
			// ignore
		}
	}
	if err := p.stream.Err(); err != nil && p.ctx.Err() == nil {
		p.send(substrate.ProcessEvent{Kind: substrate.EventError, Err: err.Error()})
	}
}

func (p *guestProcess) send(ev substrate.ProcessEvent) {
	select {
	case p.events <- ev:
	case <-p.ctx.Done():
	}
}

func (p *guestProcess) waitPID(ctx context.Context) (uint32, error) {
	select {
	case <-p.pidReady:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.pid, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// Write sends PTY input when started with PTY, else stdin.
func (p *guestProcess) Write(data []byte) error {
	pid, err := p.waitPID(p.ctx)
	if err != nil {
		return err
	}
	input := &process.ProcessInput{Input: &process.ProcessInput_Stdin{Stdin: data}}
	if p.pty {
		input = &process.ProcessInput{Input: &process.ProcessInput_Pty{Pty: data}}
	}
	if _, err := p.client.SendInput(p.ctx, connect.NewRequest(&process.SendInputRequest{
		Process: pidSelector(pid),
		Input:   input,
	})); err != nil {
		return fmt.Errorf("e2b: write %s: %w", p.sandboxID, err)
	}
	return nil
}

// Resize resizes the PTY.
func (p *guestProcess) Resize(cols, rows uint32) error {
	pid, err := p.waitPID(p.ctx)
	if err != nil {
		return err
	}
	if _, err := p.client.Update(p.ctx, connect.NewRequest(&process.UpdateRequest{
		Process: pidSelector(pid),
		Pty:     &process.PTY{Size: &process.PTY_Size{Cols: cols, Rows: rows}},
	})); err != nil {
		return fmt.Errorf("e2b: resize %s: %w", p.sandboxID, err)
	}
	return nil
}

// Signal sends SIGTERM (false) or SIGKILL (true); envd supports only those.
func (p *guestProcess) Signal(kill bool) error {
	pid, err := p.waitPID(p.ctx)
	if err != nil {
		return err
	}
	sig := process.Signal_SIGNAL_SIGTERM
	if kill {
		sig = process.Signal_SIGNAL_SIGKILL
	}
	if _, err := p.client.SendSignal(p.ctx, connect.NewRequest(&process.SendSignalRequest{
		Process: pidSelector(pid),
		Signal:  sig,
	})); err != nil {
		return fmt.Errorf("e2b: signal %s: %w", p.sandboxID, err)
	}
	return nil
}

// CloseStdin closes the process's stdin (non-PTY only).
func (p *guestProcess) CloseStdin() error {
	pid, err := p.waitPID(p.ctx)
	if err != nil {
		return err
	}
	if _, err := p.client.CloseStdin(p.ctx, connect.NewRequest(&process.CloseStdinRequest{
		Process: pidSelector(pid),
	})); err != nil {
		return fmt.Errorf("e2b: close stdin %s: %w", p.sandboxID, err)
	}
	return nil
}

// Close stops streaming; it does not kill the process.
func (p *guestProcess) Close() error {
	p.cancel()
	return nil
}

// Exec runs a command to completion, collecting stdout and stderr.
func (c *Client) Exec(ctx context.Context, sandboxID string, req substrate.ExecRequest) (substrate.ExecResult, error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultExecTimeout
	}
	// Stop at min(Timeout, ctx deadline).
	if dl, ok := ctx.Deadline(); ok {
		if d := time.Until(dl); d < timeout {
			timeout = d
		}
	}
	p, err := c.startProcess(ctx, sandboxID, substrate.StartRequest{Args: req.Args, User: req.User})
	if err != nil {
		if !c.listed(ctx, sandboxID) {
			return substrate.ExecResult{}, fmt.Errorf("%w: %v", substrate.ErrNotFound, err)
		}
		return substrate.ExecResult{}, err
	}
	var stdout, stderr bytes.Buffer
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			// Kill the process, then give it up to 2 s to exit.
			_ = p.Signal(true)
			deadline := time.Now().Add(execKillGrace)
		waitExit:
			for time.Now().Before(deadline) {
				select {
				case ev, ok := <-p.Events():
					if !ok || ev.Kind == substrate.EventExit {
						break waitExit
					}
				case <-time.After(100 * time.Millisecond):
				}
			}
			return substrate.ExecResult{
				Stdout: stdout.String(),
				Stderr: stderr.String() + fmt.Sprintf("\n[spoond] exec timed out after %ds\n", int(math.Ceil(timeout.Seconds()))),
				ExitCode: 124,
			}, nil
		case <-ctx.Done():
			_ = p.Signal(true)
			return substrate.ExecResult{}, ctx.Err()
		case ev, ok := <-p.Events():
			if !ok {
				return substrate.ExecResult{Stdout: stdout.String(), Stderr: stderr.String()},
					fmt.Errorf("e2b: exec %s: process stream ended without exit", sandboxID)
			}
			switch ev.Kind {
			case substrate.EventStdout, substrate.EventPTY:
				stdout.Write(ev.Data)
			case substrate.EventStderr:
				stderr.Write(ev.Data)
			case substrate.EventExit:
				return substrate.ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: ev.ExitCode}, nil
			case substrate.EventError:
				err := fmt.Errorf("e2b: exec %s: %s", sandboxID, ev.Err)
				if !c.listed(ctx, sandboxID) {
					return substrate.ExecResult{}, fmt.Errorf("%w: %v", substrate.ErrNotFound, err)
				}
				return substrate.ExecResult{}, err
			case substrate.EventStarted:
				// first event; nothing to collect
			}
		}
	}
}

// listed reports whether the orchestrator still lists the sandbox. Used to
// tell a dead sandbox from a transient envd failure, since the proxy's HTTP
// status for an unknown sandbox is not relied on.
func (c *Client) listed(ctx context.Context, sandboxID string) bool {
	list, err := c.List(ctx)
	if err != nil {
		return false
	}
	for _, s := range list {
		if s.ID == sandboxID {
			return true
		}
	}
	return false
}
