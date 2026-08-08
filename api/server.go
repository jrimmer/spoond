package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Server is the HTTP lease API.
type Server struct {
	svc  *Service
	reg  *ImageRegistry
	mux  *http.ServeMux
}

// NewServer wires the lease API routes onto a mux.
func NewServer(svc *Service, reg *ImageRegistry) *Server {
	s := &Server{svc: svc, reg: reg, mux: http.NewServeMux()}
	s.mux.HandleFunc("POST /api/sandboxes", s.handleCreate)
	s.mux.HandleFunc("GET /api/sandboxes", s.handleList)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/exec", s.handleExec)
	s.mux.HandleFunc("DELETE /api/sandboxes/{id}", s.handleDelete)
	s.mux.HandleFunc("GET /api/images", s.handleImages)
	return s
}

// Handler returns the HTTP handler with auth middleware applied.
func (s *Server) Handler() http.Handler {
	return s.authMiddleware(s.mux)
}

// authMiddleware authenticates the bearer token and injects the
// consumer id into the request context.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")
		if token == "" || token == auth {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		owner, ok := s.svc.tokens[token]
		if !ok {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		ctx := context.WithValue(r.Context(), ctxOwnerKey{}, owner)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type ctxOwnerKey struct{}

func ownerFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxOwnerKey{}).(string)
	return v
}

// handleCreate grants a new sandbox lease.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Image      string `json:"image"`
		TTL        int    `json:"ttl"` // seconds
		MemoryMiB  int    `json:"memory_mib"`
		Network    string `json:"network"`
		InitCmd    string `json:"init_cmd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Image == "" {
		writeError(w, http.StatusBadRequest, "image is required")
		return
	}
	ok, err := s.reg.Has(r.Context(), req.Image)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "image registry unavailable")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "unknown image tag: "+req.Image)
		return
	}
	ttl := time.Duration(req.TTL) * time.Second
	if ttl <= 0 {
		ttl = s.svc.defaultTTL
	}
	if ttl > s.svc.maxTTL {
		ttl = s.svc.maxTTL
	}
	lease, err := s.svc.grant(r.Context(), ownerFrom(r.Context()), req.Image, req.MemoryMiB, ttl)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to grant sandbox: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":      lease.ID,
		"address": lease.Address,
		"image":   lease.Image,
		"ttl":     int(ttl.Seconds()),
	})
}

// handleList returns the caller's leases.
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	s.svc.store.mu.Lock()
	defer s.svc.store.mu.Unlock()
	var out []map[string]any
	for _, l := range s.svc.store.leases {
		if l.Owner == owner && !l.released {
			out = append(out, map[string]any{
				"id":      l.ID,
				"image":   l.Image,
				"address": l.Address,
				"expires": l.ExpiresAt.Unix(),
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleExec runs a command in a sandbox owned by the caller.
func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	lease := s.svc.lookup(owner, id)
	if lease == nil {
		writeError(w, http.StatusNotFound, "sandbox not found")
		return
	}
	var req struct {
		Cmd     string            `json:"cmd"`
		Cwd     string            `json:"cwd"`
		Env     map[string]string `json:"env"`
		Timeout int               `json:"timeout"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Cmd == "" {
		writeError(w, http.StatusBadRequest, "cmd is required")
		return
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	args := buildShellArgs(req.Cmd, req.Cwd, req.Env)
	res, err := s.svc.forkd.Exec(r.Context(), lease.ForkdID, args, timeout)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "exec failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stdout":   res.Stdout,
		"stderr":   res.Stderr,
		"exit":     res.ExitCode,
	})
}

// handleDelete releases a sandbox owned by the caller.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	lease := s.svc.lookup(owner, id)
	if lease == nil {
		writeError(w, http.StatusNotFound, "sandbox not found")
		return
	}
	s.svc.release(r.Context(), lease)
	w.WriteHeader(http.StatusNoContent)
}

// handleImages lists available image tags.
func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	tags, err := s.reg.Tags(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "image registry unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"images": tags})
}

// buildShellArgs wraps a command with cwd/env into a single shell
// invocation, since forkd's exec takes argv and no cwd/env.
func buildShellArgs(cmd, cwd string, env map[string]string) []string {
	var parts []string
	if cwd != "" {
		parts = append(parts, "cd "+shellQuote(cwd)+" &&")
	}
	for k, v := range env {
		parts = append(parts, "export "+k+"="+shellQuote(v)+";")
	}
	parts = append(parts, cmd)
	return []string{"/bin/sh", "-c", strings.Join(parts, " ")}
}

// shellQuote wraps s in single quotes, escaping embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
