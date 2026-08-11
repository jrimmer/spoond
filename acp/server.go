// Package acp implements the spoond ACP endpoint (ticket #24): a native Agent Client
// Protocol (ACP) agent endpoint for spoond.
//
// ACP is the JSON-RPC 2.0 / NDJSON-over-stdio contract that hosts like
// Buzz's buzz-acp use to attach agent CLIs. The agent's tools execute
// in forkd microVM leases; its LLM comes from the forkd LLM gateway;
// keys stay off-VM. This is the "agent plane" companion to #23's "tool
// plane".
//
// Transport contract (from the ACP spec as implemented by Buzz):
// newline-delimited JSON-RPC 2.0 over stdio. Methods:
//
//	initialize                   — protocol version negotiation
//	session/new                  — create a session (cwd + mcpServers)
//	session/prompt               — send a turn, get a stop reason
//	session/cancel               — cancel an in-flight turn
//	(server->client) session/update            — streaming updates
//	(server->client) session/request_permission — permission prompts
//
// v1 scope (minimal but honest): single model, shell + file tools,
// single-turn context per prompt, no persistent conversation store.
// Session = lease lifecycle: session/new creates/keeps a lease,
// session/prompt turns run in it, session/cancel releases it.
package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
)

// Protocol version we speak.
const protocolVersion = 1

// ---------- message types (client -> agent) ----------

// Request is a JSON-RPC 2.0 request.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// InitializeParams is the client's initialize payload.
type InitializeParams struct {
	ProtocolVersion    int            `json:"protocolVersion"`
	ClientCapabilities map[string]any `json:"clientCapabilities,omitempty"`
	ClientInfo         map[string]any `json:"clientInfo,omitempty"`
}

// SessionNewParams creates a session.
type SessionNewParams struct {
	Cwd            string         `json:"cwd,omitempty"`
	McpServers     []McpServer    `json:"mcpServers,omitempty"`
	PermissionMode string         `json:"permissionMode,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

// McpServer describes an MCP server the client offers to attach.
type McpServer struct {
	Name    string   `json:"name"`
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	URL     string   `json:"url,omitempty"`
}

// PromptParams is a session/prompt turn.
type PromptParams struct {
	SessionID   string              `json:"sessionId"`
	Prompt      []PromptPart        `json:"prompt"`
	Permissions []PermissionRequest `json:"permissions,omitempty"`
}

// PromptPart is one part of a prompt (text, image, ...).
type PromptPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	URL  string `json:"url,omitempty"`
}

// PermissionRequest is an approval a client may have pre-granted.
type PermissionRequest struct {
	RequestID string `json:"requestId"`
}

// CancelParams cancels an in-flight turn.
type CancelParams struct {
	SessionID string `json:"sessionId"`
}

// ---------- message types (agent -> client) ----------

// Response is a JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// InitializeResult is the agent's initialize response.
type InitializeResult struct {
	ProtocolVersion   int            `json:"protocolVersion"`
	AgentCapabilities map[string]any `json:"agentCapabilities"`
	AgentInfo         map[string]any `json:"agentInfo"`
}

// SessionNewResult returns the new session id.
type SessionNewResult struct {
	SessionID string `json:"sessionId"`
}

// PromptResult ends a turn with a stop reason.
type PromptResult struct {
	StopReason map[string]any `json:"stopReason"`
}

// Notification is a server->client push (session/update,
// session/request_permission).
type Notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// UpdateParams is a session/update notification.
type UpdateParams struct {
	SessionID string `json:"sessionId"`
	Update    any    `json:"update"`
}

// AgentMessage is a text message from the agent.
type AgentMessage struct {
	Type      string         `json:"type"`
	MessageID string         `json:"messageId,omitempty"`
	Role      string         `json:"role,omitempty"`
	Content   []AgentContent `json:"content,omitempty"`
}

// AgentContent is one content block of an agent message.
type AgentContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// Stop reasons.
var (
	StopReasonEndTurn  = map[string]any{"type": "end_turn"}
	StopReasonMaxTurns = map[string]any{"type": "max_turns"}
	StopReasonError    = map[string]any{"type": "error", "error": "agent error"}
)

// ---------- server ----------

// Agent is the sandbox+LLM execution core. It is the port the ACP
// server uses; production implementation runs tools in a forkd lease
// and prompts the forkd LLM gateway. Tests can fake it.
type Agent interface {
	// NewSession creates a session-scoped sandbox lease. Returns the
	// lease id.
	NewSession(ctx context.Context, cwd string) (string, error)
	// Prompt runs one turn: prompt the LLM, execute tool calls in the
	// sandbox, repeat until the model stops. update is called for each
	// agent message / tool result as it happens.
	Prompt(ctx context.Context, sessionID, system, user string, update func(Notification)) (*PromptResult, error)
	// Cancel cancels the in-flight Prompt.
	Cancel(sessionID string)
	// Release tears down the session's lease.
	Release(ctx context.Context, sessionID string) error
}

// Server is the ACP JSON-RPC server over stdio.
type Server struct {
	log      *log.Logger
	agent    Agent
	in       io.Reader
	out      io.Writer
	mu       sync.Mutex
	sessions map[string]*session
	seq      int64
	closed   bool
}

type session struct {
	id      string
	leaseID string
	cwd     string
	cancel  context.CancelFunc
}

// Config wires the server.
type Config struct {
	Agent Agent
	In    io.Reader
	Out   io.Writer
	Log   *log.Logger
}

// New builds the ACP server.
func New(cfg Config) *Server {
	if cfg.Log == nil {
		cfg.Log = log.Default()
	}
	return &Server{
		log:      cfg.Log,
		agent:    cfg.Agent,
		in:       cfg.In,
		out:      cfg.Out,
		sessions: map[string]*session{},
	}
}

// Run serves until the input stream closes.
func (s *Server) Run(ctx context.Context) error {
	sc := bufio.NewScanner(s.in)
	sc.Buffer(make([]byte, 1024*1024), 32*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		if err := s.handleLine(ctx, line); err != nil {
			s.log.Printf("handle: %v", err)
		}
	}
	return sc.Err()
}

func (s *Server) handleLine(ctx context.Context, line []byte) error {
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		return s.send(Response{JSONRPC: "2.0", Error: &RPCError{Code: -32700, Message: "parse error"}})
	}
	switch req.Method {
	case "initialize":
		var p InitializeParams
		_ = json.Unmarshal(req.Params, &p)
		return s.send(Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: InitializeResult{
				ProtocolVersion: protocolVersion,
				AgentCapabilities: map[string]any{
					"prompt": map[string]any{"supportsStreaming": true},
				},
				AgentInfo: map[string]any{
					"name":    "spoond-acp",
					"version": "0.1.0",
				},
			},
		})
	case "session/new":
		var p SessionNewParams
		_ = json.Unmarshal(req.Params, &p)
		return s.handleSessionNew(ctx, req.ID, p)
	case "session/prompt":
		var p PromptParams
		_ = json.Unmarshal(req.Params, &p)
		return s.handlePrompt(ctx, req.ID, p)
	case "session/cancel":
		var p CancelParams
		_ = json.Unmarshal(req.Params, &p)
		s.handleCancel(p.SessionID)
		// ACP: cancel is a request with an empty result.
		return s.send(Response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}})
	default:
		return s.send(Response{JSONRPC: "2.0", ID: req.ID, Error: &RPCError{Code: -32601, Message: "method not found: " + req.Method}})
	}
}

func (s *Server) handleSessionNew(ctx context.Context, id json.RawMessage, p SessionNewParams) error {
	leaseID, err := s.agent.NewSession(ctx, p.Cwd)
	if err != nil {
		return s.send(Response{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: -32000, Message: "create session: " + err.Error()}})
	}
	s.mu.Lock()
	sid := fmt.Sprintf("sess-%d", s.seq)
	s.seq++
	s.sessions[sid] = &session{id: sid, leaseID: leaseID, cwd: p.Cwd}
	s.mu.Unlock()
	s.log.Printf("session/new: %s (lease %s)", sid, leaseID)
	return s.send(Response{JSONRPC: "2.0", ID: id, Result: SessionNewResult{SessionID: sid}})
}

func (s *Server) handlePrompt(ctx context.Context, id json.RawMessage, p PromptParams) error {
	s.mu.Lock()
	sess := s.sessions[p.SessionID]
	if sess == nil {
		s.mu.Unlock()
		return s.send(Response{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: -32002, Message: "unknown session: " + p.SessionID}})
	}
	// Prompt only one turn per session at a time.
	if sess.cancel != nil {
		s.mu.Unlock()
		return s.send(Response{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: -32003, Message: "session busy"}})
	}
	pctx, cancel := context.WithCancel(ctx)
	sess.cancel = cancel
	s.mu.Unlock()

	// Assemble the user text from prompt parts.
	var userParts []string
	for _, part := range p.Prompt {
		if part.Type == "text" {
			userParts = append(userParts, part.Text)
		}
	}
	user := strings.Join(userParts, "\n")

	result, err := s.agent.Prompt(pctx, sess.leaseID, "", user, func(n Notification) {
		if up, ok := n.Params.(UpdateParams); ok {
			up.SessionID = p.SessionID
			n.Params = up
		}
		_ = s.send(n)
	})
	if err != nil {
		cancel()
		s.mu.Lock()
		sess.cancel = nil
		s.mu.Unlock()
		return s.send(Response{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: -32000, Message: "prompt: " + err.Error()}})
	}
	cancel()
	s.mu.Lock()
	sess.cancel = nil
	s.mu.Unlock()
	return s.send(Response{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) handleCancel(sessionID string) {
	s.mu.Lock()
	sess := s.sessions[sessionID]
	var cancel context.CancelFunc
	if sess != nil {
		cancel = sess.cancel
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		s.log.Printf("session/cancel: %s", sessionID)
	}
}

// Close releases all sessions.
func (s *Server) Close(ctx context.Context) {
	s.mu.Lock()
	sessions := make([]*session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.sessions = map[string]*session{}
	s.closed = true
	s.mu.Unlock()
	for _, sess := range sessions {
		_ = s.agent.Release(ctx, sess.leaseID)
	}
}

func (s *Server) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	_, err = fmt.Fprintln(s.out, string(b))
	return err
}

// newID returns a fresh message id for notifications that need one.
func (s *Server) newID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return fmt.Sprintf("n%d", s.seq)
}

// ---------- helpers ----------

func encodeJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
