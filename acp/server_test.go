package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jrimmer/spoond/runner"
)

// fakeAgent is an in-memory Agent for protocol tests.
type fakeAgent struct {
	mu       sync.Mutex
	sessions map[string]string // sessionID -> leaseID
	prompts  int
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{sessions: map[string]string{}}
}

func (f *fakeAgent) NewSession(ctx context.Context, cwd string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lease := "lease-" + string(rune(len(f.sessions)+'a'))
	return lease, nil
}

func (f *fakeAgent) Prompt(ctx context.Context, sessionID, system, user string, update func(Notification)) (*PromptResult, error) {
	f.mu.Lock()
	f.prompts++
	f.mu.Unlock()
	// Emit one update then end turn.
	update(Notification{
		Method: "session/update",
		Params: UpdateParams{Update: AgentMessage{Type: "agent_message", Content: []AgentContent{{Type: "text", Text: "hello from " + sessionID}}}},
	})
	return &PromptResult{StopReason: StopReasonEndTurn}, nil
}

func (f *fakeAgent) Cancel(sessionID string) {}

func (f *fakeAgent) Release(ctx context.Context, sessionID string) error {
	return nil
}

// roundTrip runs one request line through the server and returns the
// first response line.
func roundTrip(t *testing.T, s *Server, req map[string]any) map[string]any {
	t.Helper()
	reqB, _ := json.Marshal(req)
	in := bytes.NewBuffer(append(reqB, '\n'))
	var out bytes.Buffer
	// The server's io.Reader is fixed at construction; for tests we
	// rebuild with the same Agent but fresh streams. State lives in the
	// Server struct (sessions map), so to preserve state across calls
	// callers should pass a server whose session map we reuse via
	// runSession.
	srv := New(Config{Agent: s.agent, In: in, Out: &out, Log: log.New(&bytes.Buffer{}, "", 0)})
	srv.sessions = s.sessions
	srv.seq = s.seq
	_ = srv.Run(context.Background())
	sc := bufio.NewScanner(&out)
	// The agent emits session/update notifications (method-only JSON-RPC
	// messages) before the final response; skip them for the response.
	var resp map[string]any
	found := false
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad response: %v", err)
		}
		if _, isNotification := m["method"]; isNotification {
			continue
		}
		resp = m
		found = true
		break
	}
	if !found {
		t.Fatalf("no response for %v", req)
	}
	s.sessions = srv.sessions
	s.seq = srv.seq
	return resp
}

func TestInitialize(t *testing.T) {
	s := New(Config{Agent: newFakeAgent(), Log: log.New(&bytes.Buffer{}, "", 0)})
	resp := roundTrip(t, s, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": 1},
	})
	r, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result, got %v", resp)
	}
	if r["protocolVersion"] != float64(protocolVersion) {
		t.Fatalf("wrong protocol version: %v", r["protocolVersion"])
	}
	info := r["agentInfo"].(map[string]any)
	if info["name"] != "forkd-acp" {
		t.Fatalf("wrong agent name: %v", info["name"])
	}
}

func TestSessionNewAndPrompt(t *testing.T) {
	fa := newFakeAgent()
	s := New(Config{Agent: fa, Log: log.New(&bytes.Buffer{}, "", 0)})

	// session/new
	resp := roundTrip(t, s, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "session/new",
		"params": map[string]any{"cwd": "/root"},
	})
	r := resp["result"].(map[string]any)
	sid := r["sessionId"].(string)
	if sid == "" {
		t.Fatalf("no session id: %v", r)
	}

	// session/prompt
	resp = roundTrip(t, s, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "session/prompt",
		"params": map[string]any{
			"sessionId": sid,
			"prompt":    []any{map[string]any{"type": "text", "text": "run uname -a"}},
		},
	})
	pr, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected prompt result, got %v", resp)
	}
	sr, ok := pr["stopReason"].(map[string]any)
	if !ok || sr["type"] != "end_turn" {
		t.Fatalf("expected end_turn, got %v", pr)
	}
	fa.mu.Lock()
	prompts := fa.prompts
	fa.mu.Unlock()
	if prompts != 1 {
		t.Fatalf("expected 1 prompt, got %d", prompts)
	}
}

func TestUnknownSession(t *testing.T) {
	s := New(Config{Agent: newFakeAgent(), Log: log.New(&bytes.Buffer{}, "", 0)})
	resp := roundTrip(t, s, map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "session/prompt",
		"params": map[string]any{
			"sessionId": "nope",
			"prompt":    []any{},
		},
	})
	if _, ok := resp["error"]; !ok {
		t.Fatalf("expected error, got %v", resp)
	}
}

func TestCancel(t *testing.T) {
	s := New(Config{Agent: newFakeAgent(), Log: log.New(&bytes.Buffer{}, "", 0)})
	resp := roundTrip(t, s, map[string]any{
		"jsonrpc": "2.0", "id": 5, "method": "session/cancel",
		"params": map[string]any{"sessionId": "sess-0"},
	})
	if _, ok := resp["result"]; !ok {
		t.Fatalf("expected empty result, got %v", resp)
	}
}

func TestUnknownMethod(t *testing.T) {
	s := New(Config{Agent: newFakeAgent(), Log: log.New(&bytes.Buffer{}, "", 0)})
	resp := roundTrip(t, s, map[string]any{
		"jsonrpc": "2.0", "id": 6, "method": "bogus",
	})
	if _, ok := resp["error"]; !ok {
		t.Fatalf("expected error, got %v", resp)
	}
}

func TestAgentPromptEndTurn(t *testing.T) {
	// Agent-level: fake LLM endpoint returns no tool calls -> end_turn.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	llm := &LLMClient{BaseURL: srv.URL, Model: "test-model", Client: srv.Client()}
	a := NewAgent(&fakeSandboxProvider{}, llm, "dev-base", 600, 5)
	res, err := a.Prompt(context.Background(), "lease-a", "", "hello", func(Notification) {})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if res.StopReason["type"] != "end_turn" {
		t.Fatalf("expected end_turn, got %v", res.StopReason)
	}
}

func TestAgentPromptToolCall(t *testing.T) {
	// First response asks for a shell tool call; second ends the turn.
	// The fake provider records the exec so we can assert sandbox use.
	fp := &fakeSandboxProvider{}
	responses := []string{
		`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"tc1","type":"function","function":{"name":"shell","arguments":"{\"command\":\"uname -a\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(responses) == 0 {
			t.Fatal("unexpected extra llm call")
		}
		body := responses[0]
		responses = responses[1:]
		w.Write([]byte(body))
	}))
	defer srv.Close()
	llm := &LLMClient{BaseURL: srv.URL, Model: "test-model", Client: srv.Client()}
	a := NewAgent(fp, llm, "dev-base", 600, 5)
	var updates []Notification
	res, err := a.Prompt(context.Background(), "lease-a", "", "list files", func(n Notification) { updates = append(updates, n) })
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if res.StopReason["type"] != "end_turn" {
		t.Fatalf("expected end_turn, got %v", res.StopReason)
	}
	fp.mu.Lock()
	execs := fp.execs
	fp.mu.Unlock()
	if len(execs) != 1 {
		t.Fatalf("expected 1 sandbox exec, got %d (%v)", len(execs), execs)
	}
	foundToolResult := false
	for _, u := range updates {
		up, ok := u.Params.(UpdateParams)
		if !ok {
			continue
		}
		if am, ok := up.Update.(AgentMessage); ok && am.Type == "tool_result" {
			foundToolResult = true
		}
	}
	if !foundToolResult {
		t.Fatalf("expected a tool_result update")
	}
}

// fakeSandboxProvider records execs.
type fakeSandboxProvider struct {
	mu     sync.Mutex
	execs  []string
	nextID int
}

func (f *fakeSandboxProvider) Create(ctx context.Context, image string, ttl int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	return "lease-" + string(rune('a'+f.nextID-1)), nil
}

func (f *fakeSandboxProvider) Exec(ctx context.Context, id, cmd, cwd string, env map[string]string, timeout int) (*runner.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execs = append(f.execs, cmd)
	return &runner.ExecResult{Stdout: "ok\n", Exit: 0}, nil
}

func (f *fakeSandboxProvider) Delete(ctx context.Context, id string) error { return nil }
