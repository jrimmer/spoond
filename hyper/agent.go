package hyper

import (
	"context"
	"fmt"

	"code.lacy.casa/jrimmer/hyper-forgejo-runner/hyper/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// AgentClient is a gRPC client for the guest-agent service running inside a VM.
type AgentClient struct {
	conn   *grpc.ClientConn
	client proto.GuestAgentClient
}

// NewAgentClient dials the guest-agent at addr (typically via vsock relay).
func NewAgentClient(addr string) (*AgentClient, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
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

// Exec runs a command in the guest VM and returns captured output.
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
		return nil, fmt.Errorf("exec: %w", err)
	}
	return resp, nil
}

// Close releases the gRPC connection.
func (a *AgentClient) Close() error {
	return a.conn.Close()
}
