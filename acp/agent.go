// Package acp agent implementation: the loop that prompts an LLM via
// the forkd LLM gateway and executes tool calls in a forkd microVM
// lease. This is the "native" part of forkd-acp — we control the loop.
package acp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/jrimmer/spoond/runner"
)

// ---------- LLM gateway client ----------

// LLMClient prompts an OpenAI-compatible chat endpoint. The forkd LLM
// gateway exposes /llm/<lease-id>/openai/chat/completions; the lease id
// in the path is the capability (keys stay off-VM).
type LLMClient struct {
	BaseURL string // e.g. https://127.0.0.1:8890
	Model   string // upstream model id (LLM_MODEL_MAP key, e.g. gpt-oss-20b-fireworks)
	Client  *http.Client
}

// ChatMessage is an OpenAI-style message.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatTool is an OpenAI-style function tool.
type ChatTool struct {
	Type     string        `json:"type"`
	Function ChatToolFunc  `json:"function"`
}

type ChatToolFunc struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ChatRequest is the chat/completions body we send.
type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Tools    []ChatTool    `json:"tools,omitempty"`
	MaxTokens int          `json:"max_tokens,omitempty"`
}

// ChatResponse is the chat/completions response we parse.
type ChatResponse struct {
	Choices []struct {
		Message struct {
			Role         string `json:"role"`
			Content      string `json:"content"`
			ToolCalls    []ToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// ToolCall is an OpenAI-style function call.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ChatCompletion calls the gateway for a lease.
func (c *LLMClient) ChatCompletion(ctx context.Context, leaseID string, req *ChatRequest) (*ChatResponse, error) {
	body, _ := json.Marshal(req)
	url := fmt.Sprintf("%s/llm/%s/openai/chat/completions", strings.TrimRight(c.BaseURL, "/"), leaseID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.Client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("llm %s: status %d: %s", url, resp.StatusCode, truncate(string(raw), 300))
	}
	var out ChatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("llm response: %w", err)
	}
	return &out, nil
}

// ---------- tools ----------

// sandboxTool is a tool the agent can call, executed in the lease.
type sandboxTool struct {
	name        string
	description string
	parameters  map[string]any
	run         func(ctx context.Context, sandbox runner.SandboxProvider, leaseID string, args map[string]any) (string, error)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func pyStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

var agentTools = []sandboxTool{
	{
		name:        "shell",
		description: "Run a shell command in the forkd microVM. Returns stdout, stderr, exit code.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string"},
			},
			"required": []string{"command"},
		},
		run: func(ctx context.Context, sandbox runner.SandboxProvider, leaseID string, args map[string]any) (string, error) {
			cmd, _ := args["command"].(string)
			out, err := sandbox.Exec(ctx, leaseID, cmd, "", nil, 60)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("exit=%d\nstdout:\n%s\nstderr:\n%s", out.Exit, out.Stdout, out.Stderr), nil
		},
	},
	{
		name:        "read_file",
		description: "Read a file from the sandbox filesystem.",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string"},
			},
			"required": []string{"path"},
		},
		run: func(ctx context.Context, sandbox runner.SandboxProvider, leaseID string, args map[string]any) (string, error) {
			path, _ := args["path"].(string)
			out, err := sandbox.Exec(ctx, leaseID, "base64 < "+shellQuote(path)+" 2>/dev/null || echo __READ_FAIL__", "", nil, 30)
			if err != nil {
				return "", err
			}
			if strings.Contains(out.Stdout, "__READ_FAIL__") {
				return "", fmt.Errorf("read %s: %s", path, strings.TrimSpace(out.Stderr))
			}
			raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out.Stdout))
			if err != nil {
				return "", fmt.Errorf("decode %s: %w", path, err)
			}
			return string(raw), nil
		},
	},
	{
		name:        "write_file",
		description: "Write a file in the sandbox filesystem (creates parent dirs, overwrites).",
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			"required": []string{"path", "content"},
		},
		run: func(ctx context.Context, sandbox runner.SandboxProvider, leaseID string, args map[string]any) (string, error) {
			path, _ := args["path"].(string)
			content, _ := args["content"].(string)
			b64 := base64.StdEncoding.EncodeToString([]byte(content))
			cmd := fmt.Sprintf("mkdir -p $(dirname %s) && echo %s | base64 -d > %s && echo WROTE %s $(wc -c < %s)",
				shellQuote(path), b64, shellQuote(path), shellQuote(path), shellQuote(path))
			out, err := sandbox.Exec(ctx, leaseID, cmd, "", nil, 30)
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(out.Stdout + "\n" + out.Stderr), nil
		},
	},
}

func toolsToChatTools() []ChatTool {
	out := make([]ChatTool, 0, len(agentTools))
	for _, t := range agentTools {
		out = append(out, ChatTool{
			Type: "function",
			Function: ChatToolFunc{
				Name:        t.name,
				Description: t.description,
				Parameters:  t.parameters,
			},
		})
	}
	return out
}

// ---------- agent ----------

// SandboxAgent implements acp.Agent: sessions map to forkd leases,
// prompts run the LLM loop with in-sandbox tools.
type SandboxAgent struct {
	log     *log.Logger
	sandbox runner.SandboxProvider
	llm     *LLMClient
	image   string
	ttl     int
	maxTurns int
	mu      sync.Mutex
	cancel  map[string]context.CancelFunc
}

// NewAgent builds the production agent.
func NewAgent(sandbox runner.SandboxProvider, llm *LLMClient, image string, ttl, maxTurns int) *SandboxAgent {
	if image == "" {
		image = "dev-base"
	}
	if ttl == 0 {
		ttl = 1800
	}
	if maxTurns == 0 {
		maxTurns = 12
	}
	return &SandboxAgent{
		log:      log.Default(),
		sandbox:  sandbox,
		llm:      llm,
		image:    image,
		ttl:      ttl,
		maxTurns: maxTurns,
		cancel:   map[string]context.CancelFunc{},
	}
}

// NewSession creates a lease for the session.
func (a *SandboxAgent) NewSession(ctx context.Context, cwd string) (string, error) {
	return a.sandbox.Create(ctx, a.image, a.ttl)
}

// Release tears down the lease.
func (a *SandboxAgent) Release(ctx context.Context, leaseID string) error {
	return a.sandbox.Delete(ctx, leaseID)
}

// Cancel cancels the in-flight prompt for a session (by lease id).
func (a *SandboxAgent) Cancel(leaseID string) {
	a.mu.Lock()
	cancel := a.cancel[leaseID]
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Prompt runs one turn: LLM -> tool calls -> sandbox -> repeat.
// update emits session/update notifications as messages and tool
// results happen.
func (a *SandboxAgent) Prompt(ctx context.Context, leaseID, system, user string, update func(Notification)) (*PromptResult, error) {
	// Register cancellation.
	pctx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	a.cancel[leaseID] = cancel
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.cancel, leaseID)
		a.mu.Unlock()
	}()

	if system != "" {
		// system message is carried by the caller via user text for v1.
	}
	messages := []ChatMessage{{Role: "user", Content: user}}

	update(Notification{
		Method: "session/update",
		Params: UpdateParams{
			Update: AgentMessage{
				Type: "agent_message",
				Role: "assistant",
				Content: []AgentContent{{Type: "text", Text: "Starting turn in forkd sandbox " + leaseID}},
			},
		},
	})

	for turn := 0; turn < a.maxTurns; turn++ {
		if pctx.Err() != nil {
			return nil, pctx.Err()
		}
		resp, err := a.llm.ChatCompletion(pctx, leaseID, &ChatRequest{
			Model:    a.llm.Model,
			Messages: messages,
			Tools:    toolsToChatTools(),
			MaxTokens: 2048,
		})
		if err != nil {
			return nil, err
		}
		if len(resp.Choices) == 0 {
			return nil, fmt.Errorf("no choices from llm")
		}
		choice := resp.Choices[0]
		msg := choice.Message

		// Text content -> notify the client.
		if strings.TrimSpace(msg.Content) != "" {
			update(Notification{
				Method: "session/update",
				Params: UpdateParams{
					Update: AgentMessage{
						Type:    "agent_message",
						Role:    "assistant",
						Content: []AgentContent{{Type: "text", Text: msg.Content}},
					},
				},
			})
		}

		// No tool calls -> end turn.
		if len(msg.ToolCalls) == 0 {
			return &PromptResult{StopReason: StopReasonEndTurn}, nil
		}

		// Execute tool calls in the sandbox.
		messages = append(messages, ChatMessage{Role: "assistant", Content: msg.Content})
		for _, tc := range msg.ToolCalls {
			var args map[string]any
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
			if args == nil {
				args = map[string]any{}
			}
			result, err := a.runTool(pctx, leaseID, tc.Function.Name, args)
			if err != nil {
				result = "error: " + err.Error()
			}
			update(Notification{
				Method: "session/update",
				Params: UpdateParams{
					Update: AgentMessage{
						Type: "tool_result",
						Role: "tool",
						Content: []AgentContent{{Type: "text", Text: fmt.Sprintf("%s: %s", tc.Function.Name, result)}},
					},
				},
			})
			// Feed the tool result back as a tool message.
			messages = append(messages, ChatMessage{
				Role:    "tool",
				Content: result,
			})
		}
	}
	return &PromptResult{StopReason: StopReasonMaxTurns}, nil
}

func (a *SandboxAgent) runTool(ctx context.Context, leaseID, name string, args map[string]any) (string, error) {
	for _, t := range agentTools {
		if t.name == name {
			return t.run(ctx, a.sandbox, leaseID, args)
		}
	}
	return "", fmt.Errorf("unknown tool: %s", name)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
