package forkd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestServer returns a Client wired to an httptest server that
// records requests and serves canned responses.
func newTestServer(t *testing.T, handler http.HandlerFunc) (*Client, *[]map[string]any) {
	t.Helper()
	var calls []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		calls = append(calls, map[string]any{
			"method": r.Method,
			"path":   r.URL.Path,
			"body":   body,
		})
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "test-token"), &calls
}

func TestListSnapshots(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`[{"tag":"py-base","dir":"/var/lib/forkd/snapshots/py-base","created_at_unix":1717000000,"bootable":true}]`))
	})
	snaps, err := c.ListSnapshots(context.Background())
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snaps) != 1 || snaps[0].Tag != "py-base" {
		t.Fatalf("expected 1 snapshot py-base, got %+v", snaps)
	}
	if !snaps[0].Bootable {
		t.Fatal("expected bootable=true")
	}
}

func TestSpawn(t *testing.T) {
	c, calls := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`[{"id":"sb-0000","snapshot_tag":"py-base","guest_addr":"10.42.0.2:8888"}]`))
	})
	sbs, err := c.Spawn(context.Background(), "py-base", 1, true, 256)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if len(sbs) != 1 || sbs[0].ID != "sb-0000" {
		t.Fatalf("expected 1 sandbox sb-0000, got %+v", sbs)
	}
	// Verify the request body carried the right fields.
	body := (*calls)[0]["body"].(map[string]any)
	if body["snapshot_tag"] != "py-base" || body["n"] != float64(1) || body["per_child_netns"] != true || body["memory_limit_mib"] != float64(256) {
		t.Fatalf("unexpected spawn body: %+v", body)
	}
}

func TestSpawnUnknownTag(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"snapshot tag not found"}`))
	})
	_, err := c.Spawn(context.Background(), "nope", 1, false, 0)
	if err == nil {
		t.Fatal("expected error for unknown tag")
	}
	fe, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if fe.StatusCode != 404 {
		t.Fatalf("expected status 404, got %d", fe.StatusCode)
	}
}

func TestExec(t *testing.T) {
	c, calls := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"stdout":"4\n","stderr":"","exit_code":0}`))
	})
	res, err := c.Exec(context.Background(), "sb-0000", []string{"python3", "-c", "print(2+2)"}, 30)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Stdout != "4\n" || res.ExitCode != 0 {
		t.Fatalf("unexpected exec result: %+v", res)
	}
	body := (*calls)[0]["body"].(map[string]any)
	if body["timeout_secs"] != float64(30) {
		t.Fatalf("expected timeout_secs=30, got %+v", body)
	}
}

func TestExecDeadSandbox(t *testing.T) {
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"sandbox not found"}`))
	})
	_, err := c.Exec(context.Background(), "sb-dead", []string{"echo"}, 5)
	if err == nil {
		t.Fatal("expected error for dead sandbox")
	}
	fe, ok := err.(*Error)
	if !ok || fe.StatusCode != 404 {
		t.Fatalf("expected *Error status 404, got %v", err)
	}
}

func TestKill(t *testing.T) {
	c, calls := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	})
	if err := c.Kill(context.Background(), "sb-0000"); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if (*calls)[0]["method"] != "DELETE" || (*calls)[0]["path"] != "/v1/sandboxes/sb-0000" {
		t.Fatalf("unexpected kill call: %+v", (*calls)[0])
	}
}

func TestAuthHeaderSent(t *testing.T) {
	var gotAuth string
	c, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`[]`))
	})
	_, _ = c.ListSnapshots(context.Background())
	if gotAuth != "Bearer test-token" {
		t.Fatalf("expected Bearer test-token, got %q", gotAuth)
	}
}
