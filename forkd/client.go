// Package forkd is a typed Go client for the forkd-controller HTTP API.
//
// It wraps the controller's REST endpoints (snapshots, sandboxes, exec)
// so the lease backend can manage microVMs without knowing the wire format.
// All forkd API calls are isolated here so upstream API changes are contained.
package forkd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client talks to a forkd-controller daemon.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClient returns a Client for the controller at baseURL (e.g.
// "http://127.0.0.1:8889"). If token is non-empty it is sent as a
// Bearer token on every request.
func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 600 * time.Second},
	}
}

// SnapshotInfo mirrors the controller's SnapshotInfo JSON shape.
type SnapshotInfo struct {
	Tag           string `json:"tag"`
	Dir           string `json:"dir"`
	CreatedAtUnix int64  `json:"created_at_unix"`
	Status        string `json:"status,omitempty"`
	Bootable      bool   `json:"bootable,omitempty"`
}

// SandboxInfo mirrors the controller's SandboxInfo JSON shape.
type SandboxInfo struct {
	ID             string `json:"id"`
	SnapshotTag    string `json:"snapshot_tag"`
	Netns          string `json:"netns,omitempty"`
	GuestAddr      string `json:"guest_addr,omitempty"`
	CreatedAtUnix  int64  `json:"created_at_unix"`
	PID            int    `json:"pid,omitempty"`
	MemoryLimitMiB int    `json:"memory_limit_mib,omitempty"`
}

// ExecResult is the outcome of a command run inside a sandbox.
type ExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// Error is a typed error carrying the controller's HTTP status and
// error message when one is available.
type Error struct {
	StatusCode int
	Message    string
}

func (e *Error) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("forkd: %s (status %d)", e.Message, e.StatusCode)
	}
	return fmt.Sprintf("forkd: status %d", e.StatusCode)
}

// do performs a JSON request and decodes the response into out (if non-nil).
// Non-2xx responses are returned as *Error.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return fmt.Errorf("forkd: encode request: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, &buf)
	if err != nil {
		return fmt.Errorf("forkd: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("forkd: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var eb struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&eb)
		return &Error{StatusCode: resp.StatusCode, Message: eb.Error}
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("forkd: decode response: %w", err)
		}
	}
	return nil
}

// ListSnapshots returns the registered snapshot tags.
func (c *Client) ListSnapshots(ctx context.Context) ([]SnapshotInfo, error) {
	var out []SnapshotInfo
	if err := c.do(ctx, http.MethodGet, "/v1/snapshots", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SnapshotExists reports whether a snapshot tag is registered and
// restorable. It uses the per-tag info endpoint, which is reliable even
// when the list endpoint is empty. (The info endpoint returns 200 for a
// restorable snapshot; bootability is only annotated in the list view.)
func (c *Client) SnapshotExists(ctx context.Context, tag string) (bool, error) {
	var out SnapshotInfo
	err := c.do(ctx, http.MethodGet, "/v1/snapshots/"+tag+"/info", nil, &out)
	if err == nil {
		return true, nil
	}
	if fe, ok := err.(*Error); ok && fe.StatusCode == http.StatusNotFound {
		return false, nil
	}
	return false, err
}

// Spawn forks n sandboxes from the given snapshot tag. It returns the
// created sandboxes. perChildNetns places each child in its own netns.
// memoryLimitMiB is omitted when 0 (forkd rejects an explicit 0 limit).
func (c *Client) Spawn(ctx context.Context, tag string, n int, perChildNetns bool, memoryLimitMiB int) ([]SandboxInfo, error) {
	req := map[string]any{
		"snapshot_tag":    tag,
		"n":               n,
		"per_child_netns": perChildNetns,
	}
	if memoryLimitMiB > 0 {
		req["memory_limit_mib"] = memoryLimitMiB
	}
	var out []SandboxInfo
	if err := c.do(ctx, http.MethodPost, "/v1/sandboxes", req, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListSandboxes returns the active sandboxes.
func (c *Client) ListSandboxes(ctx context.Context) ([]SandboxInfo, error) {
	var out []SandboxInfo
	if err := c.do(ctx, http.MethodGet, "/v1/sandboxes", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Kill terminates a sandbox by id.
func (c *Client) Kill(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/sandboxes/"+id, nil, nil)
}

// Exec runs a command inside a sandbox. args is the argv; timeoutSecs
// bounds the call. It returns stdout, stderr, and the exit code.
func (c *Client) Exec(ctx context.Context, id string, args []string, timeoutSecs int) (*ExecResult, error) {
	req := map[string]any{
		"args":         args,
		"timeout_secs": timeoutSecs,
	}
	var out ExecResult
	if err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+id+"/exec", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Ping round-trips to the guest agent inside a sandbox.
func (c *Client) Ping(ctx context.Context, id string) error {
	var out map[string]any
	return c.do(ctx, http.MethodPost, "/v1/sandboxes/"+id+"/ping", nil, &out)
}

// Metrics fetches the controller's Prometheus metrics as raw text.
func (c *Client) Metrics(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/metrics", nil)
	if err != nil {
		return nil, fmt.Errorf("forkd: build metrics request: %w", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("forkd: metrics: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &Error{StatusCode: resp.StatusCode, Message: "metrics"}
	}
	return io.ReadAll(resp.Body)
}
