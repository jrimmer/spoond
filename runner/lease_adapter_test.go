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
