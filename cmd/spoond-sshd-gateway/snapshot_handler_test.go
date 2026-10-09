//go:build linux

package spoondgateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// recordedReq is one request the fake backend saw.
type recordedReq struct {
	Method string
	Path   string
	Query  string
	Body   string
}

// withFakeBackend points the gateway at an httptest server that records
// every request and answers the minimal JSON each route needs. It
// restores the backend globals on cleanup.
func withFakeBackend(t *testing.T, answer func(w http.ResponseWriter, r *http.Request)) *[]recordedReq {
	t.Helper()
	var reqs []recordedReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		reqs = append(reqs, recordedReq{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: string(b)})
		answer(w, r)
	}))
	t.Cleanup(srv.Close)

	oldURL, oldTok := *backendURL, *backendTok
	*backendURL, *backendTok = srv.URL, "test-token"
	t.Cleanup(func() { *backendURL, *backendTok = oldURL, oldTok })
	return &reqs
}

// TestCtlCreateSnapshot exercises `create --snapshot` through the real
// runControlCommand: it POSTs /api/sandboxes with the snapshot field and
// prints the created lease JSON.
func TestCtlCreateSnapshot(t *testing.T) {
	reqs := withFakeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"0123456789abcdef0123456789abcdef","snapshot":{"name":"spoond/warm","version":3,"build_id":"b1"}}`))
	})
	out := runControlCommand(t.Context(), "create --snapshot spoond/warm@3 --ttl 120", nil, "alice", "", "")
	if !strings.Contains(out, `"id":"0123456789abcdef0123456789abcdef"`) {
		t.Fatalf("create output = %q", out)
	}
	if len(*reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(*reqs))
	}
	got := (*reqs)[0]
	if got.Method != http.MethodPost || got.Path != "/api/sandboxes" {
		t.Fatalf("request = %s %s, want POST /api/sandboxes", got.Method, got.Path)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(got.Body), &body); err != nil {
		t.Fatalf("body %q: %v", got.Body, err)
	}
	if body["snapshot"] != "spoond/warm@3" || body["ttl"] != float64(120) {
		t.Fatalf("body = %v", body)
	}
}

// TestCtlSnapshotSave exercises `snapshot save`: POST to the lease's
// snapshots route with the name, key and keep.
func TestCtlSnapshotSave(t *testing.T) {
	reqs := withFakeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"name":"spoond/warm","version":1,"build_id":"b1","image":"py-base","memory_mb":2048,"size_bytes":100,"created_at":"2026-10-06T09:12:30Z"}`))
	})
	out := runControlCommand(t.Context(), "snapshot save lease123 spoond/warm --key fl/1 --keep 5", nil, "alice", "", "")
	if !strings.Contains(out, `"version":1`) {
		t.Fatalf("save output = %q", out)
	}
	if len(*reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(*reqs))
	}
	got := (*reqs)[0]
	if got.Method != http.MethodPost || got.Path != "/api/sandboxes/lease123/snapshots" {
		t.Fatalf("request = %s %s", got.Method, got.Path)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(got.Body), &body); err != nil {
		t.Fatalf("body %q: %v", got.Body, err)
	}
	if body["name"] != "spoond/warm" || body["idempotency_key"] != "fl/1" || body["keep"] != float64(5) {
		t.Fatalf("body = %v", body)
	}
}

// TestCtlSnapshotList exercises `snapshot ls [prefix]`: it GETs the list
// route with the escaped prefix and prints a table by default, raw JSON
// with --json.
func TestCtlSnapshotList(t *testing.T) {
	reqs := withFakeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"snapshots":[{"name":"spoond/warm","latest":2,"versions":[{"version":2,"build_id":"feedfacefeedface","image":"py-base","memory_mb":2048,"size_bytes":1073741824,"created_at":"2026-10-06T09:12:30Z","in_use":0,"stale":false}]}]}`))
	})
	out := runControlCommand(t.Context(), "snapshot ls spoond/", nil, "alice", "", "")
	if !strings.Contains(out, "spoond/warm") || !strings.Contains(out, "NAME") {
		t.Fatalf("pretty list = %q", out)
	}
	if got := (*reqs)[0]; got.Path != "/api/named-snapshots" || got.Query != "prefix=spoond%2F" {
		t.Fatalf("request = %s?%s", got.Path, got.Query)
	}

	reqs2 := withFakeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"snapshots":[]}`))
	})
	out = runControlCommand(t.Context(), "snapshot ls --json", nil, "alice", "", "")
	if out != `{"snapshots":[]}` {
		t.Fatalf("json list = %q", out)
	}
	if got := (*reqs2)[0]; got.Query != "" {
		t.Fatalf("json request query = %q, want none", got.Query)
	}
}

// TestCtlSnapshotShowAndRm exercises `snapshot show` and `snapshot rm`,
// including the force query.
func TestCtlSnapshotShowAndRm(t *testing.T) {
	reqs := withFakeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Write([]byte(`{"name":"spoond/warm","version":3,"build_id":"abc","image":"go-base","memory_mb":4096,"size_bytes":2254857830,"created_at":"2026-10-06T09:12:30Z","in_use":1,"stale":false}`))
	})
	out := runControlCommand(t.Context(), "snapshot show spoond/warm@3", nil, "alice", "", "")
	if !strings.Contains(out, "spoond/warm@3") || !strings.Contains(out, "build_id : abc") {
		t.Fatalf("show = %q", out)
	}
	if got := (*reqs)[0]; got.Method != http.MethodGet || got.Path != "/api/named-snapshots/spoond/warm@3" {
		t.Fatalf("show request = %s %s", got.Method, got.Path)
	}

	out = runControlCommand(t.Context(), "snapshot rm spoond/warm@3 --force", nil, "alice", "", "")
	if !strings.Contains(out, `"deleted":true`) {
		t.Fatalf("rm = %q", out)
	}
	if got := (*reqs)[1]; got.Method != http.MethodDelete || got.Path != "/api/named-snapshots/spoond/warm@3" || got.Query != "force=1" {
		t.Fatalf("rm request = %s %s?%s", got.Method, got.Path, got.Query)
	}
}

// TestCtlShareListParsesSharesKey: `share ls` must parse the backend's
// top-level "shares" key; a backend that renames it yields the
// pass-through JSON (or "no shares"), not a rendered table.
func TestCtlShareListParsesSharesKey(t *testing.T) {
	withFakeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"shares":[{"lease_id":"0123456789abcdef0123456789abcdef","grantee":"u-grantee","mode":"http","created_at":"2026-10-06T09:00:00Z"}]}`))
	})
	out := runControlCommand(t.Context(), "share ls", nil, "alice", "", "")
	if !strings.Contains(out, "0123456789ab…") && !strings.Contains(out, "0123456789abcdef0123456789abcdef") {
		t.Fatalf("share ls did not render a table: %q", out)
	}
	if strings.Contains(out, "no shares") {
		t.Fatalf("share ls ignored the shares key: %q", out)
	}
}

// TestCtlShareListRejectsRenamedKey is the mutation guard: if the
// backend names the array anything other than "shares", `share ls`
// must not render the parsed table (it falls back to raw JSON).
func TestCtlShareListRejectsRenamedKey(t *testing.T) {
	withFakeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"grants":[{"lease_id":"0123456789abcdef0123456789abcdef","grantee":"u-grantee","mode":"http","created_at":"2026-10-06T09:00:00Z"}]}`))
	})
	out := runControlCommand(t.Context(), "share ls", nil, "alice", "", "")
	if strings.Contains(out, "GRANTEE") || strings.Contains(out, "LEASE ") {
		t.Fatalf("share ls rendered a table from a renamed key: %q", out)
	}
}

// TestCtlSnapshotErrorPropagates: a 4xx from the backend is reported as
// the gateway's {"error":...} JSON.
func TestCtlSnapshotErrorPropagates(t *testing.T) {
	withFakeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"snapshot is in use","code":"snapshot_in_use"}`))
	})
	out := runControlCommand(t.Context(), "snapshot rm spoond/warm", nil, "alice", "", "")
	if !strings.Contains(out, `"error"`) || !strings.Contains(out, "snapshot is in use") {
		t.Fatalf("error output = %q", out)
	}
	// The result must still be valid JSON, which printJSON requires.
	if !json.Valid([]byte(out)) {
		t.Fatalf("not JSON: %q", out)
	}
}
