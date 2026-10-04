package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// HTTPLeaseClient is a SandboxProvider backed by the spoond backend's
// lease HTTP API.
//
// A client is safe for concurrent use across Create/Exec/Delete/Sweep
// calls. WithLabel, however, mutates shared state: on a client whose
// Creates run concurrently, a label could land on another caller's
// lease. The runner never does this — every worker owns a private
// client and runs one job at a time — so keep that shape: one client
// per concurrent creator, WithLabel immediately before the Create it
// applies to.
type HTTPLeaseClient struct {
	BaseURL   string
	Token     string
	Client    *http.Client
	NetPolicy string   // egress policy: none|lan|internet|restricted (default: internet for CI)
	NetAllow  []string // allowlist IPs/CIDRs for restricted policy

	// labelMu guards the label the next Create applies (set via
	// WithLabel, a LeaseLabeler capability).
	labelMu sync.Mutex
	label   string
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

// SetHTTPTimeout raises the lease client's overall timeout so long exec
// calls (EXEC_TIMEOUT_SECS) are not cut at the default 600s by the
// runner's own HTTP client. Must exceed the backend's exec timeout.
func (c *HTTPLeaseClient) SetHTTPTimeout(d time.Duration) {
	c.Client.Timeout = d
}

// WithLabel sets the comment the lease the next Create grants gets:
// "forgejo job <id> <url>", the job it runs. Implements LeaseLabeler.
// The orphan sweep finds the runner's leases by it.
func (c *HTTPLeaseClient) WithLabel(label string) {
	c.labelMu.Lock()
	defer c.labelMu.Unlock()
	c.label = label
}

// Create grants a new sandbox lease.
func (c *HTTPLeaseClient) Create(ctx context.Context, image string, ttl int) (string, error) {
	c.labelMu.Lock()
	label := c.label
	c.labelMu.Unlock()
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
	if label != "" {
		// The label is best effort: a lease without it still runs its
		// job; it only escapes the orphan sweep if the runner then dies.
		if err := c.comment(ctx, out.ID, label); err != nil {
			log.Printf("lease %s: label %q: %v", out.ID, label, err)
		}
	}
	return out.ID, nil
}

// comment sets the lease's comment (POST /api/leases/{id}/comment).
func (c *HTTPLeaseClient) comment(ctx context.Context, id, text string) error {
	body, _ := json.Marshal(map[string]string{"comment": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/leases/"+id+"/comment", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("comment: status %d", resp.StatusCode)
	}
	return nil
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
			// 410 Gone = sandbox permanently dead; don't retry.
			// 500/502/503 = transient backend error; retry.
			// Other non-200 = client error; don't retry.
			if resp.StatusCode == http.StatusGone ||
				resp.StatusCode < 500 {
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
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.BaseURL+"/api/leases/"+id, nil)
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

// leaseRow is one row of GET /api/leases (the fields the sweep reads).
type leaseRow struct {
	ID      string `json:"id"`
	Comment string `json:"comment"`
}

// SweepOrphans releases the job leases a previous runner process left
// behind: it lists this token's leases and deletes each one whose
// comment names a Forgejo job and that keep does not claim. Implements
// LeaseSweeper. A list or delete failure aborts the sweep (it is
// reported, not fatal: the next restart tries again).
func (c *HTTPLeaseClient) SweepOrphans(ctx context.Context, keep func(id string) bool) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/leases", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.Client.Do(req)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return 0, fmt.Errorf("list leases: status %d", resp.StatusCode)
	}
	var out struct {
		Sandboxes []leaseRow `json:"sandboxes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		resp.Body.Close()
		return 0, err
	}
	resp.Body.Close()
	deleted := 0
	for _, l := range out.Sandboxes {
		if !strings.HasPrefix(l.Comment, JobLabelPrefix) {
			continue // not a runner job lease: never touched
		}
		if keep != nil && keep(l.ID) {
			continue // a job this process runs
		}
		dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := c.Delete(dctx, l.ID)
		cancel()
		if err != nil {
			return deleted, fmt.Errorf("delete orphan lease %s (%q): %w", l.ID, l.Comment, err)
		}
		log.Printf("sweep: deleted orphan lease %s (%q)", l.ID, l.Comment)
		deleted++
	}
	return deleted, nil
}
