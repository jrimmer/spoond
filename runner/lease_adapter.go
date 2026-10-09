package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultAdmitWaitSecs is the "wait" the runner asks the backend for on
// every create (RUNNER_ADMIT_WAIT_SECS): when the node is full the
// backend's admission queue holds the create open for up to this long
// instead of refusing at once (#129), so a momentarily busy host no
// longer fails a job at the create. 0 sends no wait.
const DefaultAdmitWaitSecs = 900

// defaultCapacityRetryAfter is the backoff a capacity 503 without a
// Retry-After header uses: the same hint the backend puts on a burst
// refusal (burstRetryAfterSecs).
const defaultCapacityRetryAfter = 30 * time.Second

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

	// AdmitWaitSecs is the "wait" sent on Create (RUNNER_ADMIT_WAIT_SECS):
	// how long the backend may queue the create for admission. Negative
	// reads as 0 (no wait). Set with SetAdmitWait so the client timeout
	// grows to cover it.
	AdmitWaitSecs int

	// labelMu guards the label the next Create applies (set via
	// WithLabel, a LeaseLabeler capability).
	labelMu sync.Mutex
	label   string

	// sleepFn waits for a retry delay or until ctx ends; nil uses the
	// real timer. A test seam for Retry-After waits.
	sleepFn func(context.Context, time.Duration) error
}

// NewHTTPLeaseClient builds a lease API adapter.
func NewHTTPLeaseClient(baseURL, token string) *HTTPLeaseClient {
	return &HTTPLeaseClient{
		BaseURL: baseURL,
		Token:   token,
		// The default timeout must cover a queued create's whole wait
		// (AdmitWaitSecs) plus a margin, or the client would cut a held
		// create before the backend admitted it. Exec raises it further
		// for long steps.
		Client:        &http.Client{Timeout: time.Duration(DefaultAdmitWaitSecs+120) * time.Second},
		NetPolicy:     "lan", // CI sandboxes need LAN egress to reach Forgejo
		AdmitWaitSecs: DefaultAdmitWaitSecs,
	}
}

// SetHTTPTimeout raises the lease client's overall timeout so long exec
// calls (EXEC_TIMEOUT_SECS) are not cut at the default by the runner's
// own HTTP client. It only raises: a shorter value never undoes a
// timeout already set large enough to cover a queued create.
func (c *HTTPLeaseClient) SetHTTPTimeout(d time.Duration) {
	if d > c.Client.Timeout {
		c.Client.Timeout = d
	}
}

// SetAdmitWait sets the wait sent on Create (RUNNER_ADMIT_WAIT_SECS)
// and raises the client timeout to cover it plus a margin, so a create
// the backend holds for admission is not cut by the client's own timeout.
func (c *HTTPLeaseClient) SetAdmitWait(secs int) {
	c.AdmitWaitSecs = secs
	if secs > 0 {
		if need := time.Duration(secs+120) * time.Second; need > c.Client.Timeout {
			c.Client.Timeout = need
		}
	}
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
//
// It sends "wait" (AdmitWaitSecs, RUNNER_ADMIT_WAIT_SECS) so a full
// node's admission queue can hold the create instead of refusing it, and
// it never treats a capacity 503 as final: a 503 is a full (or draining)
// node, so Create logs the wait, sleeps for Retry-After (or a default)
// and tries again until the create is admitted or ctx ends. ctx is the
// job's own timeout — a capacity refusal must never fail a job, but a
// job that has run out of time must stop waiting. Any other refusal
// (bad request, auth, unknown image, quota) is returned at once.
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
	if w := c.admitWaitSecs(); w > 0 {
		// Queued admission (#129): let the backend hold the create for
		// room instead of answering 503 at once.
		payload["wait"] = w
	}
	body, _ := json.Marshal(payload)

	for attempt := 0; ; attempt++ {
		id, retryAfter, capacity, err := c.createOnce(ctx, body)
		if !capacity {
			if err != nil {
				return "", err
			}
			if label != "" {
				// The label is best effort: a lease without it still runs its
				// job; it only escapes the orphan sweep if the runner then dies.
				if err := c.comment(ctx, id, label); err != nil {
					log.Printf("lease %s: label %q: %v", id, label, err)
				}
			}
			return id, nil
		}
		log.Printf("create sandbox: %v; waiting %s then retrying (attempt %d)", err, retryAfter, attempt+1)
		if err := c.waitForRetry(ctx, retryAfter); err != nil {
			return "", fmt.Errorf("create sandbox: waiting for capacity: %w", err)
		}
	}
}

// createOnce posts one create. capacity is true for a 503 (a full or
// draining node), with retryAfter parsed from its Retry-After header (or
// the default when absent). A fresh body reader is built per call so a
// retry never reuses a consumed one.
func (c *HTTPLeaseClient) createOnce(ctx context.Context, body []byte) (id string, retryAfter time.Duration, capacity bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/sandboxes", bytes.NewReader(body))
	if err != nil {
		return "", 0, false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Client.Do(req)
	if err != nil {
		return "", 0, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated {
		var out struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return "", 0, false, err
		}
		return out.ID, 0, false, nil
	}
	if resp.StatusCode == http.StatusServiceUnavailable {
		return "", parseRetryAfter(resp.Header.Get("Retry-After")), true, fmt.Errorf("create sandbox: status %d", resp.StatusCode)
	}
	return "", 0, false, fmt.Errorf("create sandbox: status %d", resp.StatusCode)
}

// admitWaitSecs is the effective wait sent on Create; negative reads as 0.
func (c *HTTPLeaseClient) admitWaitSecs() int {
	if c.AdmitWaitSecs < 0 {
		return 0
	}
	return c.AdmitWaitSecs
}

// parseRetryAfter reads a Retry-After header in seconds; a missing,
// zero or unparsable value falls back to defaultCapacityRetryAfter.
func parseRetryAfter(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultCapacityRetryAfter
	}
	if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	// An HTTP-date is legal, but the backend only sends seconds.
	if t, err := http.ParseTime(raw); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return defaultCapacityRetryAfter
}

// waitForRetry waits d or until ctx ends. sleepFn is a test seam so a
// Retry-After test does not have to sleep through the configured delay.
func (c *HTTPLeaseClient) waitForRetry(ctx context.Context, d time.Duration) error {
	if c.sleepFn != nil {
		return c.sleepFn(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
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
	return c.DeleteReason(ctx, id, "")
}

// DeleteReason releases a sandbox with a reason (2.5, #132 part 2): the
// reason rides the DELETE as JSON {"reason"} and the backend puts it on
// the released event. An empty reason sends no body, so the backend
// keeps "deleted through the API".
func (c *HTTPLeaseClient) DeleteReason(ctx context.Context, id, reason string) error {
	var body *bytes.Reader
	header := ""
	if reason != "" {
		data, _ := json.Marshal(map[string]string{"reason": reason})
		body = bytes.NewReader(data)
		header = "application/json"
	} else {
		body = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.BaseURL+"/api/leases/"+id, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if header != "" {
		req.Header.Set("Content-Type", header)
	}
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
