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
	BaseURL   string
	Token     string
	Client    *http.Client
	NetPolicy string   // egress policy: none|lan|internet|restricted (default: internet for CI)
	NetAllow  []string // allowlist IPs/CIDRs for restricted policy
}

// leaseClientTimeout bounds the runner→backend HTTP client. It must stay
// above the 300s max exec timeout but well below the old 600s so a wedged
// backend cannot hold a step for many minutes (issue #53).
const leaseClientTimeout = 330 * time.Second

// NewHTTPLeaseClient builds a lease API adapter.
func NewHTTPLeaseClient(baseURL, token string) *HTTPLeaseClient {
	return &HTTPLeaseClient{
		BaseURL:   baseURL,
		Token:     token,
		Client:    &http.Client{Timeout: leaseClientTimeout},
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

// Exec runs a command in a sandbox. It retries transient backend errors
// (500/502/503) up to 2 times with 1s/2s backoff. Permanent errors (410
// Gone — sandbox no longer exists in the controller) and client errors
// (400/401/403/404) are returned immediately.
func (c *HTTPLeaseClient) Exec(ctx context.Context, id, cmd, cwd string, env map[string]string, timeout int) (*ExecResult, error) {
	body, _ := json.Marshal(map[string]any{"cmd": cmd, "cwd": cwd, "env": env, "timeout": timeout})
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, lastErr
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		// Fresh reader each attempt — reusing a consumed body reader
		// produces "ContentLength=N with Body length 0" on retry.
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/sandboxes/"+id+"/exec", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			// Shared retry taxonomy (issue #53): retryable statuses
			// (429/500/502/503/504) back off and retry; permanent
			// statuses (410 Gone, 4xx, 501) return immediately.
			if ClassifyStatus(resp.StatusCode) == ClassPermanent {
				return nil, fmt.Errorf("exec: status %d", resp.StatusCode)
			}
			lastErr = fmt.Errorf("exec: status %d", resp.StatusCode)
			continue
		}
		var out ExecResult
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			resp.Body.Close()
			return nil, err
		}
		resp.Body.Close()
		return &out, nil
	}
	return nil, lastErr
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
