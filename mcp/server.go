// Package mcp implements forkd-dev-mcp (ticket #23): a Model Context
// Protocol server that exposes forkd microVM sandboxes as agent tools.
//
// Transport: newline-delimited JSON-RPC 2.0 over stdio (the standard MCP
// stdio transport — agents spawn the server as a subprocess and speak
// JSON-RPC). Hand-rolled to keep the dependency footprint at zero; the
// protocol surface we implement is small and stable:
//
//	initialize            (capability negotiation)
//	notifications/initialized
//	tools/list            (tool registry)
//	tools/call            (execute a tool in a forkd sandbox)
//
// Hexagonal: the server depends only on the runner.SandboxProvider port
// (the lease HTTP API), never on forkd internals — same shape as the
// cfos adapter. v1 is stateless per call (create -> exec -> release),
// matching the ticket's KTD5-style design; persistent session-scoped
// leases are a v2 follow-on.
package mcp

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/jrimmer/spoond/runner"
)

// Server is the MCP server. It reads JSON-RPC messages from in and
// writes responses to out.
type Server struct {
	log     *log.Logger
	sandbox runner.SandboxProvider
	image   string
	ttl     int
	timeout int
	in      io.Reader
	out     io.Writer
	tools   []Tool
	mu      sync.Mutex
	closed  bool
}

// Config wires the server.
type Config struct {
	// Sandbox is the SandboxProvider (e.g. the lease HTTP API).
	Sandbox runner.SandboxProvider
	// Image is the default sandbox image (e.g. dev-base).
	Image string
	// TTL is the sandbox lease TTL in seconds.
	TTL int
	// Timeout is the default exec timeout in seconds.
	Timeout int
	// In/Out are the stdio streams (default os.Stdin/os.Stdout).
	In  io.Reader
	Out io.Writer
	// Log is the logger (default log.Default()).
	Log *log.Logger
}

// Tool describes one MCP tool.
type Tool struct {
	Name        string                                                      `json:"name"`
	Description string                                                      `json:"description"`
	InputSchema map[string]any                                              `json:"inputSchema"`
	Handler     func(ctx context.Context, args map[string]any) (any, error) `json:"-"`
}

// New builds the MCP server.
func New(cfg Config) *Server {
	if cfg.TTL == 0 {
		cfg.TTL = 600
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 60
	}
	if cfg.Image == "" {
		cfg.Image = "dev-base"
	}
	if cfg.Log == nil {
		cfg.Log = log.Default()
	}
	s := &Server{
		log:     cfg.Log,
		sandbox: cfg.Sandbox,
		image:   cfg.Image,
		ttl:     cfg.TTL,
		timeout: cfg.Timeout,
		in:      cfg.In,
		out:     cfg.Out,
	}
	s.tools = []Tool{
		{
			Name:        "shell",
			Description: "Run a shell command in an isolated forkd microVM sandbox. Returns stdout, stderr and exit code. Use for any command execution.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string", "description": "The shell command to run"},
					"cwd":     map[string]any{"type": "string", "description": "Working directory (default: /root)"},
					"timeout": map[string]any{"type": "integer", "description": "Exec timeout in seconds"},
				},
				"required": []string{"command"},
			},
			Handler: s.toolShell,
		},
		{
			Name:        "read_file",
			Description: "Read a file from the sandbox filesystem. Returns the file contents as text.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "Absolute path to read"},
				},
				"required": []string{"path"},
			},
			Handler: s.toolReadFile,
		},
		{
			Name:        "write_file",
			Description: "Write a file in the sandbox filesystem. Creates parent directories. Overwrites existing content.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    map[string]any{"type": "string", "description": "Absolute path to write"},
					"content": map[string]any{"type": "string", "description": "File content"},
				},
				"required": []string{"path", "content"},
			},
			Handler: s.toolWriteFile,
		},
		{
			Name:        "edit_file",
			Description: "Replace a unique substring in a sandbox file with new text. Use for targeted edits; write_file for full rewrites.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":       map[string]any{"type": "string", "description": "Absolute path to edit"},
					"old_string": map[string]any{"type": "string", "description": "Exact text to find (must be unique)"},
					"new_string": map[string]any{"type": "string", "description": "Replacement text"},
				},
				"required": []string{"path", "old_string", "new_string"},
			},
			Handler: s.toolEditFile,
		},
		{
			Name:        "list_files",
			Description: "List files in a sandbox directory (ls -la style).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "Directory to list (default: /root)"},
				},
			},
			Handler: s.toolListFiles,
		},
		{
			Name:        "status",
			Description: "Report sandbox status: OS, kernel, architecture, lease info. No arguments.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			Handler:     s.toolStatus,
		},
	}
	return s
}

// Run serves until the input stream closes.
func (s *Server) Run(ctx context.Context) error {
	sc := bufio.NewScanner(s.in)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		resp := s.handleLine(ctx, line)
		if resp == nil {
			continue
		}
		if err := s.writeJSON(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

// handleLine parses one JSON-RPC message and returns the response
// (nil for notifications).
func (s *Server) handleLine(ctx context.Context, line []byte) map[string]any {
	var msg map[string]any
	if err := json.Unmarshal(line, &msg); err != nil {
		return rpcError(nil, -32700, "parse error: "+err.Error())
	}
	method, _ := msg["method"].(string)
	id, _ := msg["id"]

	switch method {
	case "initialize":
		return map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"result": map[string]any{
				"protocolVersion": firstString(msg["params"], "protocolVersion", "2024-11-05"),
				"capabilities": map[string]any{
					"tools": map[string]any{"listChanged": false},
				},
				"serverInfo": map[string]any{
					"name":    "forkd-dev-mcp",
					"version": "0.1.0",
				},
			},
		}
	case "notifications/initialized":
		return nil
	case "ping":
		return map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}}
	case "tools/list":
		tools := make([]map[string]any, 0, len(s.tools))
		for _, t := range s.tools {
			tools = append(tools, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": t.InputSchema,
			})
		}
		return map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"tools": tools}}
	case "tools/call":
		return s.handleToolCall(ctx, id, msg["params"])
	default:
		return rpcError(&id, -32601, "method not found: "+method)
	}
	return rpcError(&id, -32601, "method not found")
}

func (s *Server) handleToolCall(ctx context.Context, id any, params any) map[string]any {
	p, _ := params.(map[string]any)
	name, _ := p["name"].(string)
	args, _ := p["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	for _, t := range s.tools {
		if t.Name != name {
			continue
		}
		result, err := t.Handler(ctx, args)
		if err != nil {
			return map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"content": []any{
						map[string]any{"type": "text", "text": "error: " + err.Error()},
					},
					"isError": true,
				},
			}
		}
		text, err := json.Marshal(result)
		if err != nil {
			text = []byte(fmt.Sprintf("%v", result))
		}
		return map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"result": map[string]any{
				"content": []any{
					map[string]any{"type": "text", "text": string(text)},
				},
			},
		}
	}
	return rpcError(&id, -32602, "unknown tool: "+name)
}

// withSandbox creates a sandbox, runs fn with it, and always releases
// the lease. This is the stateless-per-call v1 lifecycle.
func (s *Server) withSandbox(ctx context.Context, fn func(id string) (any, error)) (any, error) {
	sandboxID, err := s.sandbox.Create(ctx, s.image, s.ttl)
	if err != nil {
		return nil, fmt.Errorf("create sandbox: %w", err)
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.sandbox.Delete(dctx, sandboxID)
	}()
	return fn(sandboxID)
}

func (s *Server) exec(ctx context.Context, id, cmd string, timeout int) (*runner.ExecResult, error) {
	if timeout <= 0 {
		timeout = s.timeout
	}
	return s.sandbox.Exec(ctx, id, cmd, "", nil, timeout)
}

func (s *Server) toolShell(ctx context.Context, args map[string]any) (any, error) {
	cmd, _ := args["command"].(string)
	if strings.TrimSpace(cmd) == "" {
		return nil, errors.New("command is required")
	}
	cwd, _ := args["cwd"].(string)
	if cwd != "" {
		cmd = "cd " + shellQuote(cwd) + " && " + cmd
	}
	timeout, _ := args["timeout"].(float64)
	return s.withSandbox(ctx, func(id string) (any, error) {
		out, err := s.exec(ctx, id, cmd, int(timeout))
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"sandbox_id": id,
			"stdout":     out.Stdout,
			"stderr":     out.Stderr,
			"exit":       out.Exit,
		}, nil
	})
}

func (s *Server) toolReadFile(ctx context.Context, args map[string]any) (any, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return nil, errors.New("path is required")
	}
	return s.withSandbox(ctx, func(id string) (any, error) {
		out, err := s.exec(ctx, id, "base64 < "+shellQuote(path)+" 2>/dev/null || echo __READ_FAIL__", 0)
		if err != nil {
			return nil, err
		}
		if strings.Contains(out.Stdout, "__READ_FAIL__") {
			return nil, fmt.Errorf("read %s: %s", path, strings.TrimSpace(out.Stderr))
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out.Stdout))
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		return map[string]any{"path": path, "content": string(raw)}, nil
	})
}

func (s *Server) toolWriteFile(ctx context.Context, args map[string]any) (any, error) {
	path, _ := args["path"].(string)
	content, _ := args["content"].(string)
	if path == "" {
		return nil, errors.New("path is required")
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(content))
	cmd := fmt.Sprintf("mkdir -p $(dirname %s) && echo %s | base64 -d > %s && echo WROTE %s $(wc -c < %s)",
		shellQuote(path), b64, shellQuote(path), shellQuote(path), shellQuote(path))
	return s.withSandbox(ctx, func(id string) (any, error) {
		out, err := s.exec(ctx, id, cmd, 0)
		if err != nil {
			return nil, err
		}
		return map[string]any{"path": path, "result": strings.TrimSpace(out.Stdout + "\n" + out.Stderr)}, nil
	})
}

func (s *Server) toolEditFile(ctx context.Context, args map[string]any) (any, error) {
	path, _ := args["path"].(string)
	oldStr, _ := args["old_string"].(string)
	newStr, _ := args["new_string"].(string)
	if path == "" || oldStr == "" {
		return nil, errors.New("path and old_string are required")
	}
	// Python replace: fail if old_string not found or not unique.
	cmd := "python3 - <<'PYEOF'\nimport sys\np=" + pyStr(path) + "\no=" + pyStr(oldStr) + "\nn=" + pyStr(newStr) + "\ns=open(p).read()\nc=s.count(o)\nif c==0:\n    print('EDIT_FAIL: not found', file=sys.stderr); sys.exit(1)\nif c>1:\n    print('EDIT_FAIL: not unique (' + str(c) + ' matches)', file=sys.stderr); sys.exit(1)\nopen(p,'w').write(s.replace(o,n))\nprint('EDIT_OK')\nPYEOF"
	return s.withSandbox(ctx, func(id string) (any, error) {
		out, err := s.exec(ctx, id, cmd, 0)
		if err != nil {
			return nil, err
		}
		if strings.Contains(out.Stdout+out.Stderr, "EDIT_FAIL") {
			return nil, errors.New(strings.TrimSpace(out.Stderr))
		}
		return map[string]any{"path": path, "result": strings.TrimSpace(out.Stdout)}, nil
	})
}

func (s *Server) toolListFiles(ctx context.Context, args map[string]any) (any, error) {
	path, _ := args["path"].(string)
	if path == "" {
		path = "/root"
	}
	return s.withSandbox(ctx, func(id string) (any, error) {
		out, err := s.exec(ctx, id, "ls -la "+shellQuote(path)+" 2>&1", 0)
		if err != nil {
			return nil, err
		}
		return map[string]any{"path": path, "listing": out.Stdout}, nil
	})
}

func (s *Server) toolStatus(ctx context.Context, args map[string]any) (any, error) {
	return s.withSandbox(ctx, func(id string) (any, error) {
		out, err := s.exec(ctx, id, "uname -a; echo ---; cat /etc/os-release 2>/dev/null | head -2; echo ---; nproc; free -m | head -2", 0)
		if err != nil {
			return nil, err
		}
		return map[string]any{"sandbox_id": id, "status": out.Stdout}, nil
	})
}

func (s *Server) writeJSON(v any) error {
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

func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

func rpcError(id *any, code int, msg string) map[string]any {
	m := map[string]any{
		"jsonrpc": "2.0",
		"error": map[string]any{
			"code":    code,
			"message": msg,
		},
	}
	if id != nil {
		m["id"] = *id
	}
	return m
}

func firstString(v any, key, def string) string {
	if m, ok := v.(map[string]any); ok {
		if s, ok := m[key].(string); ok && s != "" {
			return s
		}
	}
	return def
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func pyStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
