package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"

	"github.com/jrimmer/spoond/runner"
)

// fakeSandbox is an in-memory SandboxProvider for tests.
type fakeSandbox struct {
	mu      sync.Mutex
	nextID  int
	created int
	deleted int
	files   map[string]string
}

func (f *fakeSandbox) Create(ctx context.Context, image string, ttl int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created++
	f.nextID++
	return fmt.Sprintf("fake-%d", f.nextID), nil
}

func (f *fakeSandbox) Exec(ctx context.Context, id, cmd, cwd string, env map[string]string, timeout int) (*runner.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.files == nil {
		f.files = map[string]string{}
	}
	// Handle the file-op commands the tools build.
	if strings.Contains(cmd, "base64 <") {
		// read_file
		parts := strings.Split(cmd, "'")
		if len(parts) >= 2 {
			content := f.files[parts[1]]
			import2 := base64Encode(content)
			return &runner.ExecResult{Stdout: import2 + "\n", Exit: 0}, nil
		}
	}
	if strings.Contains(cmd, "base64 -d >") {
		// write_file: echo <b64> | base64 -d > '<path>' && ...
		fields := strings.Fields(cmd)
		var b64, path string
		for i, fld := range fields {
			if fld == "-d" && i+2 < len(fields) {
				path = strings.Trim(fields[i+2], "'")
			}
		}
		for i, fld := range fields {
			if fld == "echo" && i+1 < len(fields) {
				b64 = fields[i+1]
			}
		}
		raw := base64Decode(b64)
		f.files[path] = raw
		return &runner.ExecResult{Stdout: "WROTE " + path + " " + fmt.Sprint(len(raw)) + "\n", Exit: 0}, nil
	}
	if strings.Contains(cmd, "EDIT_OK") {
		// edit_file via python heredoc — simulate success
		return &runner.ExecResult{Stdout: "EDIT_OK\n", Exit: 0}, nil
	}
	if strings.Contains(cmd, "uname -a") {
		return &runner.ExecResult{Stdout: "Linux fake 6.1.0 x86_64 GNU/Linux\n", Exit: 0}, nil
	}
	return &runner.ExecResult{Stdout: "ok\n", Exit: 0}, nil
}

func (f *fakeSandbox) Delete(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted++
	return nil
}

func base64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func base64Decode(s string) string {
	raw, _ := base64.StdEncoding.DecodeString(s)
	return string(raw)
}

// roundTrip sends a JSON-RPC request line to the server and returns the
// first response line.
func roundTrip(t *testing.T, s *Server, req map[string]any) map[string]any {
	t.Helper()
	var in bytes.Buffer
	var out bytes.Buffer
	// Rebuild server bound to this buffer pair.
	srv := New(Config{
		Sandbox: s.sandbox,
		Image:   "dev-base",
		TTL:     600,
		Timeout: 60,
		In:      &in,
		Out:     &out,
		Log:     log.New(&bytes.Buffer{}, "", 0),
	})
	reqB, _ := json.Marshal(req)
	in.Write(reqB)
	in.WriteByte('\n')
	_ = srv.Run(context.Background())
	sc := bufio.NewScanner(&out)
	if !sc.Scan() {
		t.Fatalf("no response")
	}
	var resp map[string]any
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		t.Fatalf("bad response: %v", err)
	}
	return resp
}

func TestInitialize(t *testing.T) {
	s := New(Config{Sandbox: &fakeSandbox{}, Log: log.New(&bytes.Buffer{}, "", 0)})
	resp := roundTrip(t, s, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": "2025-03-26", "clientInfo": map[string]any{"name": "test"}},
	})
	if _, ok := resp["result"].(map[string]any); !ok {
		t.Fatalf("expected result, got %v", resp)
	}
	si := resp["result"].(map[string]any)["serverInfo"].(map[string]any)
	if si["name"] != "spoond-mcp" {
		t.Fatalf("wrong server name: %v", si["name"])
	}
}

func TestToolsList(t *testing.T) {
	s := New(Config{Sandbox: &fakeSandbox{}, Log: log.New(&bytes.Buffer{}, "", 0)})
	resp := roundTrip(t, s, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	r := resp["result"].(map[string]any)
	tools := r["tools"].([]any)
	if len(tools) != 6 {
		t.Fatalf("expected 6 tools, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"shell", "read_file", "write_file", "edit_file", "list_files", "status"} {
		if !names[want] {
			t.Fatalf("missing tool %s", want)
		}
	}
}

func TestShellTool(t *testing.T) {
	fb := &fakeSandbox{}
	s := New(Config{Sandbox: fb, Log: log.New(&bytes.Buffer{}, "", 0)})
	resp := roundTrip(t, s, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{
			"name":      "shell",
			"arguments": map[string]any{"command": "uname -a"},
		},
	})
	r := resp["result"].(map[string]any)
	if r["isError"] == true {
		t.Fatalf("unexpected error: %v", r)
	}
	content := r["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(content, "fake") {
		t.Fatalf("expected sandbox output in %q", content)
	}
	if fb.created != 1 || fb.deleted != 1 {
		t.Fatalf("expected 1 create + 1 delete, got %d/%d", fb.created, fb.deleted)
	}
}

func TestUnknownTool(t *testing.T) {
	s := New(Config{Sandbox: &fakeSandbox{}, Log: log.New(&bytes.Buffer{}, "", 0)})
	resp := roundTrip(t, s, map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{"name": "nope", "arguments": map[string]any{}},
	})
	if _, ok := resp["error"]; !ok {
		t.Fatalf("expected error response, got %v", resp)
	}
}

func TestParseError(t *testing.T) {
	s := New(Config{Sandbox: &fakeSandbox{}, Log: log.New(&bytes.Buffer{}, "", 0)})
	var in bytes.Buffer
	var out bytes.Buffer
	srv := New(Config{Sandbox: s.sandbox, In: &in, Out: &out, Log: log.New(&bytes.Buffer{}, "", 0)})
	in.WriteString("{bad json\n")
	_ = srv.Run(context.Background())
	sc := bufio.NewScanner(&out)
	if !sc.Scan() {
		t.Fatalf("no response")
	}
	var resp map[string]any
	_ = json.Unmarshal(sc.Bytes(), &resp)
	if _, ok := resp["error"]; !ok {
		t.Fatalf("expected error, got %v", resp)
	}
}
