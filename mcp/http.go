package mcp

// http.go — MCP Streamable HTTP transport (2025-03-26 spec).
//
// The server accepts JSON-RPC 2.0 messages via HTTP POST and returns
// responses directly as application/json.  This is the simplest viable
// transport for stateless tools: each POST gets one JSON-RPC response.
//
// For backward compatibility with older MCP clients (2024-11-05 spec),
// the GET /sse endpoint opens an SSE stream and sends an `endpoint`
// event whose data is the URL to POST messages to.  Responses are
// delivered as `message` events on the same SSE stream.
//
// Authentication: Bearer token via the Authorization header.  The token
// is configured by the caller (typically FORKD_AGENT_TOKEN).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// HTTPConfig configures the HTTP transport.
type HTTPConfig struct {
	// Addr is the listen address (e.g. ":9090" or "127.0.0.1:9090").
	Addr string
	// Token is the expected bearer token. If empty, no auth is enforced
	// (useful for local testing; production should always set this).
	Token string
	// PathPrefix is the URL prefix for the MCP endpoint (default "/mcp").
	PathPrefix string
}

// RunHTTP serves MCP over HTTP until ctx is cancelled or the server
// errors.  It blocks the calling goroutine.
func (s *Server) RunHTTP(ctx context.Context, cfg HTTPConfig) error {
	if cfg.PathPrefix == "" {
		cfg.PathPrefix = "/mcp"
	}
	s.httpPathPrefix = cfg.PathPrefix

	mux := http.NewServeMux()

	// Streamable HTTP: POST /mcp → JSON-RPC request/response.
	mux.HandleFunc("POST "+cfg.PathPrefix, s.handleHTTPPost)

	// SSE transport (backward compat): GET /sse → SSE stream.
	mux.HandleFunc("GET /sse", s.handleSSE)

	// Health check.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"ok":true}`)
	})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.authMiddleware(cfg.Token, mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Shut down on context cancellation.
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	s.log.Printf("mcp: HTTP transport listening on %s (POST %s, GET /sse)", cfg.Addr, cfg.PathPrefix)
	err := srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// authMiddleware wraps h with bearer-token authentication. If token is
// empty, the middleware is a no-op (for local development).
func (s *Server) authMiddleware(token string, h http.Handler) http.Handler {
	if token == "" {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" {
			http.Error(w, `{"error":"missing Authorization header"}`, http.StatusUnauthorized)
			return
		}
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) || strings.TrimSpace(auth[len(prefix):]) != token {
			http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// handleHTTPPost handles a Streamable HTTP POST: parse one JSON-RPC
// message from the body, dispatch it via handleLine, and return the
// JSON-RPC response.
func (s *Server) handleHTTPPost(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest, -32700, "read body: "+err.Error())
		return
	}
	// Skip empty lines / whitespace.
	if len(strings.TrimSpace(string(body))) == 0 {
		writeHTTPError(w, http.StatusBadRequest, -32700, "empty body")
		return
	}
	resp := s.handleLine(r.Context(), body)
	if resp == nil {
		// Notification (no response expected).
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("MCP-Protocol-Version", "2024-11-05")
	_ = json.NewEncoder(w).Encode(resp)
}

// --- SSE transport (backward compatibility) ---

// sseSession is one SSE client connection.
type sseSession struct {
	id      string
	events  chan []byte
	closed  chan struct{}
	closeMu sync.Mutex
	done    bool
}

func (sess *sseSession) close() {
	sess.closeMu.Lock()
	defer sess.closeMu.Unlock()
	if sess.done {
		return
	}
	sess.done = true
	close(sess.closed)
}

// handleSSE opens an SSE stream.  The first event sent is `endpoint`
// with the URL the client should POST messages to.  Subsequent
// `message` events carry JSON-RPC responses.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Determine the POST endpoint URL.  We use the same path prefix
	// configuration; the client POSTs to /mcp (or whatever PathPrefix
	// is configured to).
	endpointURL := s.sseEndpointURL(r)

	// Generate a session ID and register the session so the POST
	// handler can route responses back via this SSE stream.
	sessID := generateID()
	sess := &sseSession{
		id:     sessID,
		events: make(chan []byte, 64),
		closed: make(chan struct{}),
	}
	s.registerSession(sessID, sess)
	defer s.unregisterSession(sessID, sess)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Send the endpoint event.
	fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", endpointURL)
	flusher.Flush()

	// Keep the stream alive until the client disconnects or the
	// session is closed.
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-sess.closed:
			return
		case msg := <-sess.events:
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
			flusher.Flush()
		case <-ticker.C:
			// Heartbeat comment to keep the connection alive.
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// sseEndpointURL returns the URL the SSE client should POST messages to.
// It includes the session ID as a query parameter so the POST handler
// can route the response back to the correct SSE stream.
func (s *Server) sseEndpointURL(r *http.Request) string {
	// Build the endpoint URL from the request's scheme + host + path.
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	// Use the configured PathPrefix for POST messages.
	path := "/mcp"
	if s.httpPathPrefix != "" {
		path = s.httpPathPrefix
	}
	return fmt.Sprintf("%s://%s%s", scheme, r.Host, path)
}

// registerSession/unregisterSession manage SSE sessions for the POST
// handler to route responses back.
func (s *Server) registerSession(id string, sess *sseSession) {
	s.sseSessionsMu.Lock()
	defer s.sseSessionsMu.Unlock()
	if s.sseSessions == nil {
		s.sseSessions = make(map[string]*sseSession)
	}
	s.sseSessions[id] = sess
}

func (s *Server) unregisterSession(id string, sess *sseSession) {
	s.sseSessionsMu.Lock()
	defer s.sseSessionsMu.Unlock()
	delete(s.sseSessions, id)
}

func generateID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func readBody(r *http.Request) ([]byte, error) {
	const maxBody = 16 << 20 // 16 MiB
	r.Body = http.MaxBytesReader(nil, r.Body, maxBody)
	return io.ReadAll(r.Body)
}

func writeHTTPError(w http.ResponseWriter, status, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"error":   map[string]any{"code": code, "message": msg},
	})
}
