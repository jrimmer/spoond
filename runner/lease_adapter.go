package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// HTTPLeaseClient is a SandboxProvider backed by the forkd-backend
// lease HTTP API.
type HTTPLeaseClient struct {
	BaseURL    string
	Token      string
	Client     *http.Client
	NetPolicy  string   // egress policy: none|lan|internet|restricted (default: lan)
	NetAllow   []string // allowlist IPs/CIDRs for restricted policy
}

// NewHTTPLeaseClient builds a lease API adapter.
func NewHTTPLeaseClient(baseURL, token string) *HTTPLeaseClient {
	return &HTTPLeaseClient{
		BaseURL:   baseURL,
		Token:     token,
		Client:    &http.Client{Timeout: 600 * time.Second},
		NetPolicy: "lan", // CI sandboxes need LAN egress to reach Forgejo
	}
}

// Create grants a new sandbox lease.
func (c *HTTPLeaseClient) Create(ctx context.Context, image string, ttl int) (string, error) {
	payload := map[string]any{"image": image, "ttl": ttl}
	if c.NetPolicy != "" {
		payload["network_policy"] = c.NetPolicy
	}
	if len(c.NetAllow) > 0 {
		payload["egress_allowlist"] = c.NetAllow
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/sandboxes", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("create sandbox: status %d", resp.StatusCode)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// Exec runs a command in a sandbox.
func (c *HTTPLeaseClient) Exec(ctx context.Context, id, cmd, cwd string, env map[string]string, timeout int) (*ExecResult, error) {
	body, _ := json.Marshal(map[string]any{"cmd": cmd, "cwd": cwd, "env": env, "timeout": timeout})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/sandboxes/"+id+"/exec", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("exec: status %d", resp.StatusCode)
	}
	var out ExecResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Delete releases a sandbox.
func (c *HTTPLeaseClient) Delete(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.BaseURL+"/api/sandboxes/"+id, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("delete sandbox: status %d", resp.StatusCode)
	}
	return nil
}
