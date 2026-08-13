package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHTTPLeaseClientAgentToken asserts the lease client authenticates
// with the per-agent bearer token it was constructed with (U4: agents as
// users, epic #26 ticket #30). The backend resolves that token to the
// agent's user id, so the client's only job is to send it on every call.
func TestHTTPLeaseClientAgentToken(t *testing.T) {
	const agentToken = "agent-token-abc123"
	var got []string // Authorization headers, in call order

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/sandboxes":
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"sb-1"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec"):
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"stdout":"ok\n","stderr":"","exit":0}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewHTTPLeaseClient(srv.URL, agentToken)
	ctx := context.Background()

	id, err := c.Create(ctx, "dev-base", 600)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := c.Exec(ctx, id, "echo hi", "", nil, 10); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if err := c.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("got %d authenticated calls, want 3", len(got))
	}
	want := "Bearer " + agentToken
	for i, h := range got {
		if h != want {
			t.Errorf("call %d: Authorization = %q, want %q", i, h, want)
		}
	}
}

// TestHTTPLeaseClientExecRetriesTransient verifies Exec retries a
// retryable status (503) and succeeds on the following attempt.
func TestHTTPLeaseClientExecRetriesTransient(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"stdout":"ok\n","stderr":"","exit":0}`))
	}))
	defer srv.Close()
	c := NewHTTPLeaseClient(srv.URL, "t")
	if _, err := c.Exec(context.Background(), "sb-1", "echo hi", "", nil, 10); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (one retry then success)", calls)
	}
}

// TestHTTPLeaseClientExecNoRetryOnGone verifies a permanent 410 (stale
// lease) fails immediately without retry.
func TestHTTPLeaseClientExecNoRetryOnGone(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusGone)
	}))
	defer srv.Close()
	c := NewHTTPLeaseClient(srv.URL, "t")
	if _, err := c.Exec(context.Background(), "sb-1", "echo hi", "", nil, 10); err == nil {
		t.Fatal("expected error for 410")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (410 is permanent)", calls)
	}
}

// TestHTTPLeaseClientExecNoRetryOnNotImplemented verifies a permanent
// 501 (previously retried via the raw >=500 split) now fails immediately.
func TestHTTPLeaseClientExecNoRetryOnNotImplemented(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNotImplemented)
	}))
	defer srv.Close()
	c := NewHTTPLeaseClient(srv.URL, "t")
	if _, err := c.Exec(context.Background(), "sb-1", "echo hi", "", nil, 10); err == nil {
		t.Fatal("expected error for 501")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (501 is permanent)", calls)
	}
}

// TestNewHTTPLeaseClientTimeout asserts the bounded client timeout is
// actually installed (issue #53).
func TestNewHTTPLeaseClientTimeout(t *testing.T) {
	c := NewHTTPLeaseClient("http://127.0.0.1:8890", "t")
	if c.Client.Timeout != leaseClientTimeout {
		t.Errorf("Client.Timeout = %s, want %s", c.Client.Timeout, leaseClientTimeout)
	}
}
