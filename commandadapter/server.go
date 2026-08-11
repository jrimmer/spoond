// Package commandadapter implements the command adapter: a synchronous,
// caller-driven HTTP front-end over the lease API. A caller posts a
// command/snippet + image and gets the result back. It depends only on
// the SandboxProvider port, not on forkd or the lease API internals.
package commandadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jrimmer/spoond/runner"
)

// Config wires the adapter.
type Config struct {
	// Sandbox is the SandboxProvider (e.g. the lease HTTP API).
	Sandbox runner.SandboxProvider
	// Tokens maps consumer bearer tokens to consumer ids.
	Tokens map[string]string
	// MaxConcurrent caps in-flight jobs per consumer (0 = unlimited).
	MaxConcurrent int
	// DefaultTTL is the sandbox lease TTL in seconds.
	DefaultTTL int
	// MaxTTL caps a requested TTL in seconds.
	MaxTTL int
	// MaxTimeout caps a requested exec timeout in seconds.
	MaxTimeout int
	// DefaultImage is used when no image is specified.
	DefaultImage string
}

// runRequest is the POST /v1/run body.
type runRequest struct {
	Image   string            `json:"image"`
	Command string            `json:"command"`
	Cwd     string            `json:"cwd"`
	Env     map[string]string `json:"env"`
	Timeout int               `json:"timeout"`
	TTL     int               `json:"ttl"`
}

// runResponse is the synchronous result.
type runResponse struct {
	JobID      string `json:"job_id"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Exit       int    `json:"exit"`
	DurationMS int64  `json:"duration_ms"`
}

// Server is the command adapter HTTP server.
type Server struct {
	cfg Config
	// inFlight tracks concurrent jobs per consumer.
	mu       sync.Mutex
	inFlight map[string]int
	// nextID is a monotonic job id.
	nextID int64
}

// New builds a command adapter server.
func New(cfg Config) *Server {
	if cfg.DefaultTTL == 0 {
		cfg.DefaultTTL = 300
	}
	if cfg.MaxTTL == 0 {
		cfg.MaxTTL = 3600
	}
	if cfg.MaxTimeout == 0 {
		cfg.MaxTimeout = 300
	}
	if cfg.DefaultImage == "" {
		cfg.DefaultImage = "py-base"
	}
	return &Server{
		cfg:      cfg,
		inFlight: map[string]int{},
	}
}

// Handler returns the HTTP handler for the adapter.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/run", s.handleRun)
	return s.auth(mux)
}

// auth wraps the mux with bearer-token auth, resolving the consumer id.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		consumer, ok := s.cfg.Tokens[token]
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		ctx := context.WithValue(r.Context(), consumerKey{}, consumer)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type consumerKey struct{}

func consumerFrom(ctx context.Context) string {
	v, _ := ctx.Value(consumerKey{}).(string)
	return v
}

// handleRun executes a single command in a sandbox and returns the result.
func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req runRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Command == "" {
		writeError(w, http.StatusBadRequest, "command is required")
		return
	}
	consumer := consumerFrom(r.Context())
	if !s.acquire(consumer) {
		writeError(w, http.StatusTooManyRequests, "rate limited: too many concurrent jobs")
		return
	}
	defer s.release(consumer)

	image := req.Image
	if image == "" {
		image = s.cfg.DefaultImage
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = s.cfg.DefaultTTL
	}
	if ttl > s.cfg.MaxTTL {
		ttl = s.cfg.MaxTTL
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	if timeout > s.cfg.MaxTimeout {
		timeout = s.cfg.MaxTimeout
	}

	start := time.Now()
	sandboxID, err := s.cfg.Sandbox.Create(r.Context(), image, ttl)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create sandbox")
		return
	}
	defer s.cfg.Sandbox.Delete(context.Background(), sandboxID)

	res, err := s.cfg.Sandbox.Exec(r.Context(), sandboxID, req.Command, req.Cwd, req.Env, timeout)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "exec failed")
		return
	}
	writeJSON(w, http.StatusOK, runResponse{
		JobID:      fmt.Sprintf("job-%d", s.nextID),
		Stdout:     res.Stdout,
		Stderr:     res.Stderr,
		Exit:       res.Exit,
		DurationMS: time.Since(start).Milliseconds(),
	})
}

// acquire reserves a concurrent-job slot for a consumer. Returns false
// if the consumer is at its cap.
func (s *Server) acquire(consumer string) bool {
	if s.cfg.MaxConcurrent <= 0 {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight[consumer] >= s.cfg.MaxConcurrent {
		return false
	}
	s.inFlight[consumer]++
	return true
}

// release frees a concurrent-job slot for a consumer.
func (s *Server) release(consumer string) {
	if s.cfg.MaxConcurrent <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight[consumer] > 0 {
		s.inFlight[consumer]--
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
