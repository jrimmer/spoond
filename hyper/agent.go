package hyper

import (
	"context"
	"fmt"
	"log"
	"net"
	"time"

	"code.lacy.casa/jrimmer/hyper-forgejo-runner/hyper/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// AgentClient is a gRPC client for the guest-agent service running inside a VM.
//
// The guest-agent listens on vsock port 1024 inside the Firecracker guest.
// The host reaches it through a per-VM relay that exposes a Unix domain socket
// at /srv/hyper/socks/grpc-{vm_id}.sock. The relay performs the vsock CONNECT
// handshake; from our side it's a normal Unix-socket gRPC connection.
//
// tonic (the Rust gRPC server inside the guest) requires the :authority header
// to be set, so we inject grpc.WithAuthority("localhost") on the dial.
type AgentClient struct {
	conn   *grpc.ClientConn
	client proto.GuestAgentClient
}

// NewAgentClient dials the guest-agent at addr.  addr should be a Unix socket
// path in the form  unix:///srv/hyper/socks/grpc-{vm_id}.sock
// or just the bare path (we normalise it).
func NewAgentClient(addr string) (*AgentClient, error) {
	target := addr
	// Allow callers to pass a bare path like /srv/hyper/socks/grpc-abc.sock
	// and normalise to the unix:/// scheme that grpc.Dial expects.
	if !startsWithUnixScheme(target) && isUnixSocketPath(target) {
		target = "unix://" + target
	}

	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		// tonic requires :authority to be set or it rejects the request.
		grpc.WithAuthority("localhost"),
		grpc.WithContextDialer(func(ctx context.Context, s string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, "unix", stripUnixScheme(s))
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("dial guest-agent %s: %w", addr, err)
	}
	return &AgentClient{
		conn:   conn,
		client: proto.NewGuestAgentClient(conn),
	}, nil
}

// Health checks whether the guest-agent is alive and ready.
func (a *AgentClient) Health(ctx context.Context) (bool, error) {
	resp, err := a.client.Health(ctx, &proto.HealthRequest{})
	if err != nil {
		return false, fmt.Errorf("health check: %w", err)
	}
	return resp.Ok, nil
}

// WaitForHealth polls the guest-agent's Health RPC at 1-second intervals
// until it reports ok=true or the timeout expires. This is the standard
// boot-readiness gate: a freshly created VM may take several seconds before
// the guest-agent is up and accepting connections.
func (a *AgentClient) WaitForHealth(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		ok, err := a.Health(ctx)
		if ok {
			return nil
		}
		if err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("guest-agent did not become healthy within %s: last error: %w", timeout, err)
			}
			return fmt.Errorf("guest-agent did not become healthy within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Exec runs a command in the guest VM and returns captured output.
// argv is the command and its arguments (argv[0] is the executable).
// env is merged with (or replaces) the agent's default environment.
// cwd is the working directory; empty string means the agent's default.
func (a *AgentClient) Exec(ctx context.Context, argv []string, env map[string]string, cwd string) (*proto.ExecResponse, error) {
	req := &proto.ExecRequest{
		Argv: argv,
		Env:  env,
	}
	if cwd != "" {
		req.Cwd = &cwd
	}
	resp, err := a.client.Exec(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("exec %v: %w", argv, err)
	}
	return resp, nil
}

// ExecSimple is a convenience wrapper for Exec with no env or cwd.
// Returns stdout, stderr, and exit code as a tuple.
func (a *AgentClient) ExecSimple(ctx context.Context, argv ...string) (stdout, stderr []byte, exitCode int32, err error) {
	resp, err := a.Exec(ctx, argv, nil, "")
	if err != nil {
		return nil, nil, -1, err
	}
	return resp.Stdout, resp.Stderr, resp.ExitCode, nil
}

// Close releases the gRPC connection.
func (a *AgentClient) Close() error {
	return a.conn.Close()
}

// --- helpers ---

func startsWithUnixScheme(s string) bool {
	return len(s) >= 7 && s[:7] == "unix://"
}

func isUnixSocketPath(s string) bool {
	// Heuristic: if it starts with / and contains .sock, treat it as a
	// Unix socket path. This avoids misclassifying a host:port addr.
	return len(s) > 0 && s[0] == '/' && hasSuffix(s, ".sock")
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func stripUnixScheme(s string) string {
	if len(s) >= 7 && s[:7] == "unix://" {
		return s[7:]
	}
	return s
}

// formatSocketPath returns the standard Unix socket path for a given vm_id.
// This is used by the runner to dial the guest-agent after CreateVm.
func formatSocketPath(vmID string) string {
	return fmt.Sprintf("unix:///srv/hyper/socks/grpc-%s.sock", vmID)
}

// logf is a package-level logger so callers can override it if needed.
var logf = log.Printf