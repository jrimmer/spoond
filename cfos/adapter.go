// Package cfos implements the CFOS/Sandstorm execution adapter (ticket
// #17): a thin HTTP bridge that accepts CFOS executeCode-shaped requests
// and runs them in forkd microVMs via the lease API, returning
// stdout/stderr/exit to the CFOS chat.
//
// Hexagonal: the adapter depends only on the SandboxProvider port (the
// lease HTTP API), never on forkd internals. It is stateless per
// KTD5 — each call creates a sandbox, runs the code once, returns the
// result, and releases the lease.
package cfos

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jrimmer/forkd-service/runner"
)

// Config wires the adapter.
type Config struct {
	// Sandbox is the SandboxProvider (e.g. the lease HTTP API).
	Sandbox runner.SandboxProvider
	// Tokens maps consumer bearer tokens to consumer ids.
	Tokens map[string]string
	// MaxTimeout caps a requested exec timeout in seconds.
	MaxTimeout int
	// DefaultTTL is the sandbox lease TTL in seconds.
	DefaultTTL int
	// DefaultImage is used when the request does not declare a language.
	DefaultImage string
}

// executeRequest mirrors CFOS's executeCode contract. Code is the
// Workers-style JS/TS snippet; language selects the image; bindings are
// passed through as env so the in-sandbox code can reach them via
// process.env (the gatekeeper bridge, U4, maps them to HTTP stubs).
type executeRequest struct {
	Code     string            `json:"code"`
	Language string            `json:"language"`
	Bindings map[string]string `json:"bindings"`
	Timeout  int               `json:"timeout"`
	TTL      int               `json:"ttl"`
}

type executeResponse struct {
	JobID  string `json:"job_id"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Exit   int    `json:"exit"`
}

// Server is the CFOS adapter HTTP server.
type Server struct {
	cfg    Config
	mu     sync.Mutex
	nextID int64
}

// New builds a CFOS adapter server.
func New(cfg Config) *Server {
	if cfg.MaxTimeout == 0 {
		cfg.MaxTimeout = 300
	}
	if cfg.DefaultTTL == 0 {
		cfg.DefaultTTL = 300
	}
	if cfg.DefaultImage == "" {
		cfg.DefaultImage = "js-base"
	}
	return &Server{cfg: cfg}
}

// Handler returns the HTTP handler for the adapter.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/execute", s.handleExecute)
	return s.auth(mux)
}

// auth wraps the mux with bearer-token auth.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if _, ok := s.cfg.Tokens[token]; !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleExecute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req executeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Code) == "" {
		writeError(w, http.StatusBadRequest, "code is required")
		return
	}

	image, err := ImageFor(req.Language, s.cfg.DefaultImage)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ttl := req.TTL
	if ttl <= 0 {
		ttl = s.cfg.DefaultTTL
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	if timeout > s.cfg.MaxTimeout {
		timeout = s.cfg.MaxTimeout
	}

	// The in-sandbox command materializes the code + bindings, runs it,
	// and prints the result. Bindings become env vars; language selects
	// the runner (node for js, python3 otherwise, sh fallback).
	cmd := buildRunCommand(req.Code, req.Bindings, req.Language)

	sandboxID, err := s.cfg.Sandbox.Create(r.Context(), image, ttl)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to create sandbox: "+err.Error())
		return
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.cfg.Sandbox.Delete(ctx, sandboxID)
	}()

	out, err := s.cfg.Sandbox.Exec(r.Context(), sandboxID, cmd, "", nil, timeout)
	if err != nil {
		writeError(w, http.StatusBadGateway, "exec failed: "+err.Error())
		return
	}

	s.mu.Lock()
	s.nextID++
	jobID := fmt.Sprintf("cfos-%d", s.nextID)
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, executeResponse{
		JobID:  jobID,
		Stdout: out.Stdout,
		Stderr: out.Stderr,
		Exit:   out.Exit,
	})
}

// buildRunCommand materializes the code into a runnable command. For JS
// we write the code to /tmp/main.mjs and run node; for other languages
// we run the code through the corresponding interpreter when known,
// falling back to sh -c for plain shell snippets. Bindings are exported
// as environment variables (sh-style quoting; values are JSON-encoded so
// the sandbox can JSON.parse them).
func buildRunCommand(code string, bindings map[string]string, language string) string {
	var b strings.Builder
	for k, v := range bindings {
		// export FOO='<json>'  — sh single-quote safe: JSON uses double
		// quotes, so wrapping in single quotes is always valid.
		b.WriteString(fmt.Sprintf("export %s='%s'\n", sanitizeEnvKey(k), jsonEncode(v)))
	}
	switch strings.ToLower(language) {
	case "javascript", "js", "typescript", "ts":
		b.WriteString("cat > /tmp/main.mjs <<'FORKD_EOF'\n")
		b.WriteString(code)
		b.WriteString("\nFORKD_EOF\nnode /tmp/main.mjs\n")
	case "go":
		b.WriteString("cat > /tmp/main.go <<'FORKD_EOF'\n")
		b.WriteString(code)
		b.WriteString("\nFORKD_EOF\n(cd /tmp && go run main.go)\n")
	case "python", "py":
		b.WriteString("cat > /tmp/main.py <<'FORKD_EOF'\n")
		b.WriteString(code)
		b.WriteString("\nFORKD_EOF\npython3 /tmp/main.py\n")
	default:
		b.WriteString(code)
		if !strings.HasSuffix(strings.TrimSpace(code), "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func sanitizeEnvKey(k string) string {
	var b strings.Builder
	for _, r := range k {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "BINDING"
	}
	return b.String()
}

func jsonEncode(v string) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
