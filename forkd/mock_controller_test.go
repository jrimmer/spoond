package forkd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// mockController is an in-process forkd-controller HTTP server for
// exercising Client's wire contract and error handling without a real
// controller or firecracker. It speaks the controller's REST surface
// (snapshots, sandboxes, exec/ping/branch, workspaces, metrics) with
// in-memory state, records every request, and supports injection:
//
//   - Latency: sleep before responding (simulates a slow controller).
//   - Hang: block until the client disconnects (simulates a wedged
//     controller that accepts TCP but never answers).
//   - ForceStatus: override the status for a "METHOD /path" pair.
//
// It complements (not replaces) the interface-level fakeForkd in the
// api package: that fake sits behind the ForkdClient interface for
// Service tests, while this mock sits behind Client's HTTP transport.
type mockController struct {
	mu         sync.Mutex
	snapshots  map[string]SnapshotInfo
	sandboxes  map[string]SandboxInfo
	workspaces map[string]WorkspaceInfo
	nextID     int
	calls      []mockCall

	latency     time.Duration
	hang        bool
	forceStatus map[string]int // "METHOD /path" -> status

	mux *http.ServeMux
}

type mockCall struct {
	Method string
	Path   string
	Body   map[string]any
}

// newMockController returns a controller mock with one bootable snapshot
// (py-base) registered, mirroring a freshly-started controller.
func newMockController() *mockController {
	m := &mockController{
		snapshots:   map[string]SnapshotInfo{"py-base": {Tag: "py-base", Dir: "/var/lib/forkd/snapshots/py-base", Bootable: true}},
		sandboxes:   make(map[string]SandboxInfo),
		workspaces:  make(map[string]WorkspaceInfo),
		forceStatus: make(map[string]int),
	}
	m.mux = http.NewServeMux()
	m.mux.HandleFunc("GET /v1/snapshots", m.listSnapshots)
	m.mux.HandleFunc("GET /v1/snapshots/{tag}/info", m.snapshotInfo)
	m.mux.HandleFunc("POST /v1/sandboxes", m.spawn)
	m.mux.HandleFunc("GET /v1/sandboxes", m.listSandboxes)
	m.mux.HandleFunc("DELETE /v1/sandboxes/{id}", m.kill)
	m.mux.HandleFunc("POST /v1/sandboxes/{id}/exec", m.exec)
	m.mux.HandleFunc("POST /v1/sandboxes/{id}/ping", m.ping)
	m.mux.HandleFunc("POST /v1/sandboxes/{id}/branch", m.branch)
	m.mux.HandleFunc("GET /metrics", m.metrics)
	m.mux.HandleFunc("POST /v1/workspaces", m.createWorkspace)
	m.mux.HandleFunc("POST /v1/workspaces/{name}/suspend", m.suspend)
	m.mux.HandleFunc("POST /v1/workspaces/{name}/resume", m.resume)
	m.mux.HandleFunc("DELETE /v1/workspaces/{name}", m.deleteWorkspace)
	return m
}

func (m *mockController) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.calls = append(m.calls, mockCall{Method: r.Method, Path: r.URL.Path, Body: decodeBody(r)})
	status, forced := m.forceStatus[r.Method+" "+r.URL.Path]
	m.mu.Unlock()

	if m.latency > 0 {
		time.Sleep(m.latency)
	}
	if m.hang {
		<-r.Context().Done() // wedged: block until the client gives up
		return
	}
	if forced {
		writeMockJSON(w, status, map[string]any{"error": "forced status"})
		return
	}
	m.mux.ServeHTTP(w, r)
}

// callCount returns the number of requests the mock has served, safe for
// concurrent access from the client goroutine.
func (m *mockController) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// newClient returns a Client wired to this mock via httptest.
func (m *mockController) newClient(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "test-token")
}

// newClientWithTimeout is newClient with an explicit HTTP client timeout.
func (m *mockController) newClientWithTimeout(t *testing.T, timeout time.Duration) *Client {
	t.Helper()
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	return NewClientWithTimeout(srv.URL, "test-token", timeout)
}

// ---- endpoint handlers ----

func (m *mockController) listSnapshots(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SnapshotInfo, 0, len(m.snapshots))
	for _, s := range m.snapshots {
		out = append(out, s)
	}
	writeMockJSON(w, http.StatusOK, out)
}

func (m *mockController) snapshotInfo(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	s, ok := m.snapshots[r.PathValue("tag")]
	m.mu.Unlock()
	if !ok {
		writeMockJSON(w, http.StatusNotFound, map[string]any{"error": "snapshot not found"})
		return
	}
	writeMockJSON(w, http.StatusOK, s)
}

func (m *mockController) spawn(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SnapshotTag    string `json:"snapshot_tag"`
		N              int    `json:"n"`
		PerChildNetns  bool   `json:"per_child_netns"`
		MemoryLimitMiB int    `json:"memory_limit_mib"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeMockJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.snapshots[req.SnapshotTag]; !ok {
		writeMockJSON(w, http.StatusNotFound, map[string]any{"error": "snapshot tag not found"})
		return
	}
	if req.N <= 0 {
		req.N = 1
	}
	out := make([]SandboxInfo, 0, req.N)
	for i := 0; i < req.N; i++ {
		id := fmt.Sprintf("sb-%d", m.nextID)
		m.nextID++
		sb := SandboxInfo{ID: id, SnapshotTag: req.SnapshotTag, GuestAddr: "10.42.0.2:8888"}
		m.sandboxes[id] = sb
		out = append(out, sb)
	}
	writeMockJSON(w, http.StatusCreated, out)
}

func (m *mockController) listSandboxes(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SandboxInfo, 0, len(m.sandboxes))
	for _, s := range m.sandboxes {
		out = append(out, s)
	}
	writeMockJSON(w, http.StatusOK, out)
}

func (m *mockController) kill(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	_, ok := m.sandboxes[r.PathValue("id")]
	delete(m.sandboxes, r.PathValue("id"))
	m.mu.Unlock()
	if !ok {
		writeMockJSON(w, http.StatusNotFound, map[string]any{"error": "sandbox not found"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *mockController) exec(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	_, ok := m.sandboxes[r.PathValue("id")]
	m.mu.Unlock()
	if !ok {
		writeMockJSON(w, http.StatusNotFound, map[string]any{"error": "sandbox not found"})
		return
	}
	writeMockJSON(w, http.StatusOK, ExecResult{Stdout: "ok\n", Stderr: "", ExitCode: 0})
}

func (m *mockController) ping(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	_, ok := m.sandboxes[r.PathValue("id")]
	m.mu.Unlock()
	if !ok {
		writeMockJSON(w, http.StatusNotFound, map[string]any{"error": "sandbox not found"})
		return
	}
	writeMockJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (m *mockController) branch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag string `json:"tag"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	m.mu.Lock()
	_, ok := m.sandboxes[r.PathValue("id")]
	m.mu.Unlock()
	if !ok {
		writeMockJSON(w, http.StatusNotFound, map[string]any{"error": "sandbox not found"})
		return
	}
	m.mu.Lock()
	m.snapshots[req.Tag] = SnapshotInfo{Tag: req.Tag, Bootable: true}
	m.mu.Unlock()
	writeMockJSON(w, http.StatusOK, map[string]any{"tag": req.Tag})
}

func (m *mockController) metrics(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("# HELP forkd_sandboxes_active forkd_sandboxes_active\n# TYPE forkd_sandboxes_active gauge\nforkd_sandboxes_active 0\n"))
}

func (m *mockController) createWorkspace(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name          string `json:"name"`
		SnapshotTag   string `json:"snapshot_tag"`
		PerChildNetns bool   `json:"per_child_netns"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeMockJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.snapshots[req.SnapshotTag]; !ok {
		writeMockJSON(w, http.StatusNotFound, map[string]any{"error": "snapshot tag not found"})
		return
	}
	sbID := fmt.Sprintf("sb-%d", m.nextID)
	m.nextID++
	ws := WorkspaceInfo{
		ID:                "ws-" + sbID,
		Name:              req.Name,
		SourceSnapshotTag: req.SnapshotTag,
		Status:            "running",
		LiveSandboxID:     sbID,
	}
	m.workspaces[req.Name] = ws
	m.sandboxes[sbID] = SandboxInfo{ID: sbID, SnapshotTag: req.SnapshotTag, GuestAddr: "10.42.0.2:8888"}
	writeMockJSON(w, http.StatusCreated, ws)
}

func (m *mockController) suspend(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	ws, ok := m.workspaces[r.PathValue("name")]
	if ok {
		ws.Status = "suspended"
		m.workspaces[r.PathValue("name")] = ws
	}
	m.mu.Unlock()
	if !ok {
		writeMockJSON(w, http.StatusNotFound, map[string]any{"error": "workspace not found"})
		return
	}
	writeMockJSON(w, http.StatusOK, map[string]any{})
}

func (m *mockController) resume(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	ws, ok := m.workspaces[r.PathValue("name")]
	m.mu.Unlock()
	if !ok {
		writeMockJSON(w, http.StatusNotFound, map[string]any{"error": "workspace not found"})
		return
	}
	m.mu.Lock()
	sbID := fmt.Sprintf("sb-%d", m.nextID)
	m.nextID++
	ws.Status = "running"
	ws.LiveSandboxID = sbID
	m.workspaces[ws.Name] = ws
	m.sandboxes[sbID] = SandboxInfo{ID: sbID, SnapshotTag: ws.SourceSnapshotTag, GuestAddr: "10.42.0.2:8888"}
	m.mu.Unlock()
	writeMockJSON(w, http.StatusOK, ws)
}

func (m *mockController) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	ws, ok := m.workspaces[r.PathValue("name")]
	delete(m.workspaces, r.PathValue("name"))
	m.mu.Unlock()
	if !ok {
		writeMockJSON(w, http.StatusNotFound, map[string]any{"error": "workspace not found"})
		return
	}
	m.mu.Lock()
	delete(m.sandboxes, ws.LiveSandboxID)
	m.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// ---- helpers ----

func decodeBody(r *http.Request) map[string]any {
	if r.Body == nil {
		return nil
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	return body
}

func writeMockJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
