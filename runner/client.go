package runner

import (
	"context"
	"fmt"
	"net/http"

	"connectrpc.com/connect"
	pingv1 "gitea.dev/actions-proto-go/ping/v1"
	pingv1connect "gitea.dev/actions-proto-go/ping/v1/pingv1connect"
)

// ForgejoClient wraps the connectrpc client for communicating with Forgejo.
// This is a stub that will be expanded with Register/Declare/FetchTask/Update* calls.
type ForgejoClient struct {
	httpClient *http.Client
	baseURL    string
	token      string
}

// NewForgejoClient creates a connectrpc client for the Forgejo runner protocol.
func NewForgejoClient(baseURL, token string) *ForgejoClient {
	return &ForgejoClient{
		httpClient: &http.Client{},
		baseURL:    baseURL,
		token:      token,
	}
}

// Ping is a health-check RPC to verify connectivity to Forgejo.
func (c *ForgejoClient) Ping(ctx context.Context) (string, error) {
	// TODO: replace with the real runner service client once registered.
	// This stub exercises the actions-proto-go + connect imports.
	client := pingv1connect.NewPingServiceClient(c.httpClient, c.baseURL)
	req := connect.NewRequest(&pingv1.PingRequest{Data: "hyper-runner"})
	resp, err := client.Ping(ctx, req)
	if err != nil {
		return "", fmt.Errorf("ping: %w", err)
	}
	return resp.Msg.GetData(), nil
}
