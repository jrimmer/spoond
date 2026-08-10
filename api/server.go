package api

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/sys/unix"
)

// Server is the HTTP lease API.
type Server struct {
	svc       *Service
	reg       *ImageRegistry
	mux       *http.ServeMux
	llm       *llmGateway
	assetsDir string // static assets dir served at /assets/ on the proxy listener
}

// NewServer wires the lease API routes onto a mux. openRouterURL and
// openRouterKey are the LLM gateway upstream (empty = gateway disabled);
// the key is held process-side and never exposed to sandboxes.
func NewServer(svc *Service, reg *ImageRegistry) *Server {
	return NewServerWithLLM(svc, reg, "", "", "", nil)
}

// NewServerWithLLM wires the lease API routes plus an optional per-lease
// LLM gateway. openRouterURL is the OpenAI-compatible API base; the key
// stays in this process. defaultModel is the upstream fallback model
// (applied when a requested id isn't in modelMap); modelMap translates
// exe.dev catalog model ids to upstream ids.
func NewServerWithLLM(svc *Service, reg *ImageRegistry, openRouterURL, openRouterKey, defaultModel string, modelMap map[string]string) *Server {
	s := &Server{svc: svc, reg: reg, mux: http.NewServeMux()}
	if openRouterURL != "" {
		s.llm = newLLMGateway(svc.log, svc.lookupAny, openRouterURL, openRouterKey, defaultModel, modelMap)
	}
	s.mux.HandleFunc("POST /api/sandboxes", s.handleCreate)
	s.mux.HandleFunc("GET /api/sandboxes", s.handleList)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/exec", s.handleExec)
	s.mux.HandleFunc("DELETE /api/sandboxes/{id}", s.handleDelete)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/keepalive", s.handleKeepAlive)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/suspend", s.handleSuspend)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/resume", s.handleResume)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/restart", s.handleRestart)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/tag", s.handleTag)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/prompt", s.handlePrompt)
	s.mux.HandleFunc("GET /api/sandboxes/{id}/endpoint", s.handleEndpoint)
	s.mux.HandleFunc("GET /api/sandboxes/{id}/stream", s.handleStream)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/clone", s.handleClone)
	s.mux.HandleFunc("GET /api/images", s.handleImages)
	s.mux.HandleFunc("GET /api/names/{name}", s.handleByName)
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	if s.llm != nil {
		// The LLM gateway is auth-exempt (lease id in path is the
		// capability); it MUST be mounted on the outer handler after
		// authMiddleware. Handler() does that via authExempt prefix.
		s.mux.Handle(llmGatewayPrefix, s.llm)
	}
	return s
}

// handleHealthz reports liveness without auth, for Gatus/load-balancer
// checks. It returns 200 if the service is up.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// handleMetrics proxies forkd-controller's Prometheus metrics so the
// controller can stay loopback-bound while VictoriaMetrics scrapes the
// backend. Requires a consumer token (auth middleware).
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	body, err := s.svc.forkd.Metrics(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to fetch metrics")
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// Handler returns the HTTP handler with auth middleware applied.
func (s *Server) Handler() http.Handler {
	return s.authMiddleware(s.mux)
}

// authMiddleware authenticates the bearer token and injects the
// consumer id into the request context. /healthz is exempt (liveness);
// the /llm/ prefix is exempt too — the lease id in the path is the
// capability, and sandboxes hold no consumer token.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, llmGatewayPrefix) {
			next.ServeHTTP(w, r)
			return
		}
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

// maxExecTimeout caps a single exec call so it cannot run far past the
// lease TTL or tie up the controller indefinitely.
const maxExecTimeout = 300 // seconds

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
		Persistent bool   `json:"persistent"`
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
	// Cap the requested TTL in seconds BEFORE converting to a duration,
	// so a huge ttl value cannot overflow time.Duration and bypass the
	// maxTTL cap (yielding a near-zero lease).
	ttlSecs := req.TTL
	if ttlSecs <= 0 {
		ttlSecs = int(s.svc.defaultTTL / time.Second)
	}
	if maxSecs := int(s.svc.maxTTL / time.Second); ttlSecs > maxSecs {
		ttlSecs = maxSecs
	}
	// The TTL cap above already bounds persistent leases; keep-alive
	// lets the consumer extend them (up to maxTTL per call).
	ttl := time.Duration(ttlSecs) * time.Second
	lease, err := s.svc.grant(r.Context(), ownerFrom(r.Context()), req.Image, req.MemoryMiB, ttl, req.Persistent)
	if err != nil {
		s.svc.log.Printf("create: grant %s: %v", req.Image, err)
		writeError(w, http.StatusInternalServerError, "failed to grant sandbox")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         lease.ID,
		"address":    lease.Address,
		"image":      lease.Image,
		"ttl":        int(ttl.Seconds()),
		"persistent": lease.Persistent,
		"expires_at": lease.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// handleEndpoint resolves a lease to the sandbox's live network endpoint
// (netns + guest addr). Used by the SSH gateway to reach sshd inside the
// VM. The lease owner must match (a lease id is a capability: anyone who
// holds it can resolve + connect).
func (s *Server) handleEndpoint(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	lease := s.svc.lookup(owner, id)
	if lease == nil {
		writeError(w, http.StatusNotFound, "sandbox not found")
		return
	}
	ep, err := s.svc.resolveEndpoint(r.Context(), lease)
	if err != nil {
		writeError(w, http.StatusNotFound, "sandbox not running")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         lease.ID,
		"forkd_id":   ep.ForkdID,
		"image":      lease.Image,
		"netns":      ep.Netns,
		"guest_addr": ep.GuestAddr,
	})
}

// handleStream opens a WebSocket to a sandbox and relays an interactive
// PTY session: the client sends {"args":[...],"cwd":...} as the first
// message; output streams back as text frames; client text frames are
// written to the process stdin. Protocol matches the agent's "stream"
// action (line-delimited JSON on the agent side).
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	lease := s.svc.lookup(owner, id)
	if lease == nil {
		writeError(w, http.StatusNotFound, "sandbox not found")
		return
	}
	s.svc.touch(id) // stream attach is activity for the idle sweeper
	// A suspended workspace-backed lease has no running sandbox; resume
	// first.
	if lease.Suspended {
		writeError(w, http.StatusConflict, "sandbox is suspended; resume it first")
		return
	}
	ep, err := s.svc.resolveEndpoint(r.Context(), lease)
	if err != nil {
		writeError(w, http.StatusNotFound, "sandbox not running")
		return
	}

	upgrader := websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true }, // consumer-token auth above
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()

	// First message: the exec request.
	mt, payload, err := ws.ReadMessage()
	if err != nil {
		return
	}
	if mt != websocket.TextMessage {
		ws.WriteMessage(websocket.TextMessage, []byte(`{"error":"first message must be JSON text"}`))
		return
	}
	var req struct {
		Args []string          `json:"args"`
		Cwd  string            `json:"cwd"`
		Env  map[string]string `json:"env"`
		Pty  *bool             `json:"pty"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte(`{"error":"bad request JSON"}`))
		return
	}
	if len(req.Args) == 0 {
		ws.WriteMessage(websocket.TextMessage, []byte(`{"error":"args required"}`))
		return
	}
	pty := true
	if req.Pty != nil {
		pty = *req.Pty
	}

	// Dial the agent inside the sandbox's netns.
	agentAddr := net.JoinHostPort(ep.GuestHost, "8888")
	agent, err := dialInNetns(ep.Netns, agentAddr)
	if err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte(`{"error":"agent unreachable: `+err.Error()+`"}`))
		return
	}
	defer agent.Close()

	startReq := map[string]any{
		"action": "stream",
		"args":   req.Args,
		"cwd":    req.Cwd,
		"env":    req.Env,
		"pty":    pty,
	}
	line, _ := json.Marshal(startReq)
	if _, err := agent.Write(append(line, '\n')); err != nil {
		return
	}

	// Agent -> WS relay (line-delimited JSON).
	agentDone := make(chan struct{})
	go func() {
		defer close(agentDone)
		br := bufio.NewReader(agent)
		for {
			ln, err := br.ReadBytes('\n')
			if len(ln) > 0 {
				_ = ws.WriteMessage(websocket.TextMessage, ln)
			}
			if err != nil {
				return
			}
		}
	}()

	// WS -> agent relay: {"in":"...","action":"stop"} messages.
	for {
		mt, payload, err := ws.ReadMessage()
		if err != nil {
			break
		}
		if mt != websocket.TextMessage {
			continue
		}
		var msg struct {
			In     string `json:"in"`
			Action string `json:"action"`
		}
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		if msg.Action == "stop" {
			break
		}
		if msg.In != "" {
			out, _ := json.Marshal(map[string]string{"in": msg.In})
			if _, err := agent.Write(append(out, '\n')); err != nil {
				break
			}
		}
	}

	<-agentDone
}

// dialInNetns enters the given network namespace on a locked thread,
// dials addr, and returns the connection (usable from any thread once
// established — the socket is already bound in the target netns).
func dialInNetns(netns, addr string) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		f, err := os.Open(filepath.Join("/var/run/netns", netns))
		if err != nil {
			ch <- result{nil, fmt.Errorf("open netns %s: %w", netns, err)}
			return
		}
		defer f.Close()
		if err := unix.Setns(int(f.Fd()), unix.CLONE_NEWNET); err != nil {
			ch <- result{nil, fmt.Errorf("setns %s: %w", netns, err)}
			return
		}
		d, err := net.DialTimeout("tcp", addr, 10*time.Second)
		ch <- result{d, err}
	}()
	r := <-ch
	return r.conn, r.err
}

// handleKeepAlive extends a persistent lease's expiry. The caller must
// own the lease. Body may carry {"ttl": seconds} (capped at maxTTL).
func (s *Server) handleKeepAlive(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	var req struct {
		TTL int `json:"ttl"` // seconds; 0 = maxTTL
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	ttl := time.Duration(req.TTL) * time.Second
	lease, err := s.svc.keepAlive(owner, id, ttl)
	if err != nil {
		switch err {
		case errNotFound:
			writeError(w, http.StatusNotFound, "sandbox not found")
		case errNotPersistent:
			writeError(w, http.StatusBadRequest, "sandbox is not a persistent lease")
		default:
			writeError(w, http.StatusInternalServerError, "keepalive failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         lease.ID,
		"persistent": true,
		"expires_at": lease.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// handleSuspend suspends a workspace-backed persistent lease: the
// controller snapshots the sandbox and stops it. The lease stays and can
// be resumed.
func (s *Server) handleSuspend(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	lease, err := s.svc.suspend(r.Context(), owner, id)
	if err != nil {
		switch err {
		case errNotFound:
			writeError(w, http.StatusNotFound, "sandbox not found")
		case errNotPersistent:
			writeError(w, http.StatusBadRequest, "sandbox is not a workspace-backed persistent lease")
		default:
			s.svc.log.Printf("suspend %s: %v", id, err)
			writeError(w, http.StatusInternalServerError, "suspend failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      lease.ID,
		"status":  "suspended",
		"message": "sandbox suspended; state snapshot kept (resume to restore)",
	})
}

// handleRestart reboots a persistent lease (workspace-backed: snapshot +
// resume; plain: kill + cold spawn).
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	lease, err := s.svc.restart(r.Context(), owner, id)
	if err != nil {
		switch err {
		case errNotFound:
			writeError(w, http.StatusNotFound, "sandbox not found")
		case errNotPersistent:
			writeError(w, http.StatusBadRequest, "sandbox is not a persistent lease")
		default:
			s.svc.log.Printf("restart %s: %v", id, err)
			writeError(w, http.StatusInternalServerError, "restart failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      lease.ID,
		"status":  "running",
		"message": "sandbox restarted",
	})
}

// handleTag assigns a friendly name to a lease.
func (s *Server) handleTag(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	lease, err := s.svc.setName(owner, id, req.Name)
	if err != nil {
		if err == errNotFound {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":   lease.ID,
		"name": lease.Name,
		"ok":   true,
	})
}

// handlePrompt sends a message to the Shelley coding agent running inside
// a lease and returns the agent's reply. Requires the agent to be up
// (see the `shelly` ctl verb / runShelly). Implements `shelley prompt`
// from exe.dev's CLI surface as an LLM-callable API command.
func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	var req struct {
		Message string `json:"message"`
		Model   string `json:"model,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Message) == "" {
		writeError(w, http.StatusBadRequest, "{\"message\":\"...\"} required")
		return
	}
	lease := s.svc.lookup(owner, id)
	if lease == nil {
		writeError(w, http.StatusNotFound, "sandbox not found")
		return
	}
	if lease.Suspended {
		writeError(w, http.StatusConflict, "sandbox is suspended; resume it first")
		return
	}
	model := req.Model
	if model == "" {
		model = "gpt-oss-20b-fireworks"
	}
	msg64 := base64.StdEncoding.EncodeToString([]byte(req.Message))
	mod64 := base64.StdEncoding.EncodeToString([]byte(model))
	script := fmt.Sprintf(`set -e
MSG=$(echo %s | base64 -d)
MOD=$(echo %s | base64 -d)
RESP=$(curl -sf --max-time 60 -H 'Content-Type: application/json' \
  -d "{\"message\":$(printf '%%s' "$MSG" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))'),\"model\":\"$MOD\"}" \
  http://127.0.0.1:9000/api/conversations/new) || { echo "SHELLEY_NOT_RUNNING"; exit 1; }
CID=$(echo "$RESP" | python3 -c 'import sys,json;print(json.load(sys.stdin)["conversation_id"])')
for i in $(seq 1 40); do
  sleep 5
  OUT=$(curl -sf --max-time 10 http://127.0.0.1:9000/api/conversation/$CID 2>/dev/null || true)
  AGENT=$(echo "$OUT" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    raise SystemExit
for m in d.get("messages", []):
    if m.get("type") == "agent":
        ld = json.loads(m.get("llm_data") or "{}")
        content = ld.get("Content") or []
        text = " ".join(c.get("Text","") for c in content if isinstance(c, dict))
        if text.strip():
            print(text)
            raise SystemExit
' 2>/dev/null || true)
  if [ -n "$AGENT" ]; then echo "$AGENT"; exit 0; fi
done
echo "AGENT_TIMEOUT"`, msg64, mod64)

	s.svc.log.Printf("prompt %s: %s", id, req.Message)
	start := time.Now()
	res, err := s.svc.forkd.Exec(r.Context(), lease.ForkdID, buildShellArgs(script, "", nil), 240)
	if err != nil {
		s.svc.log.Printf("prompt %s: %v (dur=%s)", id, err, time.Since(start))
		writeError(w, http.StatusBadGateway, "agent exec failed: "+err.Error())
		return
	}
	s.svc.log.Printf("prompt %s: exit=%d stdout=%d dur=%s", id, res.ExitCode, len(res.Stdout), time.Since(start))
	out := res.Stdout
	if strings.Contains(out, "SHELLEY_NOT_RUNNING") {
		writeError(w, http.StatusConflict, "shelley agent is not running in this sandbox — use the shelly ctl verb first")
		return
	}
	if strings.Contains(out, "AGENT_TIMEOUT") {
		writeError(w, http.StatusGatewayTimeout, "agent did not reply within 200s")
		return
	}
	if res.ExitCode != 0 {
		writeError(w, http.StatusBadGateway, "agent exec failed: "+tailStr(res.Stderr, 500))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      id,
		"message": req.Message,
		"reply":   strings.TrimSpace(out),
	})
}

// handleResume restores a suspended workspace-backed lease.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	lease, err := s.svc.resume(r.Context(), owner, id)
	if err != nil {
		switch err {
		case errNotFound:
			writeError(w, http.StatusNotFound, "sandbox not found")
		case errNotPersistent:
			writeError(w, http.StatusBadRequest, "sandbox is not a workspace-backed persistent lease")
		default:
			s.svc.log.Printf("resume %s: %v", id, err)
			writeError(w, http.StatusInternalServerError, "resume failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      lease.ID,
		"status":  "running",
		"address": lease.Address,
	})
}

// handleList returns the caller's leases.
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	leases := s.svc.list(owner)
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": leases})
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
	s.svc.touch(id) // exec is activity for the idle sweeper
	// A suspended workspace-backed lease has no running sandbox; resume
	// first.
	if lease.Suspended {
		writeError(w, http.StatusConflict, "sandbox is suspended; resume it first")
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
	if timeout > maxExecTimeout {
		timeout = maxExecTimeout
	}
	args := buildShellArgs(req.Cmd, req.Cwd, req.Env)
	start := time.Now()
	res, err := s.svc.forkd.Exec(r.Context(), lease.ForkdID, args, timeout)
	if err != nil {
		s.svc.log.Printf("exec: %s: %v (dur=%s)", lease.ForkdID, err, time.Since(start))
		writeError(w, http.StatusInternalServerError, "exec failed")
		return
	}
	s.svc.log.Printf("exec: %s: exit=%d stdout=%d stderr=%d dur=%s", lease.ForkdID, res.ExitCode, len(res.Stdout), len(res.Stderr), time.Since(start))
	if res.ExitCode != 0 {
		s.svc.log.Printf("exec: %s: stderr=%q", lease.ForkdID, tailStr(res.Stderr, 500))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stdout": res.Stdout,
		"stderr": res.Stderr,
		"exit":   res.ExitCode,
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

// handleClone branches a running sandbox into a new snapshot tag and
// grants a fresh lease on the branch. Optional {"tag": "..."} names the
// branch; otherwise the controller auto-generates one. The clone is a
// persistent lease (the source's tmux state, filesystem, and installed
// packages carry over).
func (s *Server) handleClone(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	lease := s.svc.lookup(owner, id)
	if lease == nil {
		writeError(w, http.StatusNotFound, "sandbox not found")
		return
	}
	var req struct {
		Tag string `json:"tag"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // optional body

	// Branch the running sandbox to a new snapshot tag.
	tag := req.Tag
	if tag == "" {
		tag = "clone-" + lease.ID[:8] + "-" + strconv.FormatInt(time.Now().Unix(), 10)
	}
	newTag, err := s.svc.forkd.Branch(r.Context(), lease.ForkdID, tag)
	if err != nil {
		s.svc.log.Printf("clone %s: branch: %v", id, err)
		writeError(w, http.StatusInternalServerError, "failed to branch sandbox")
		return
	}

	// Spawn a sandbox from the branch and grant a lease on it.
	ttl := s.svc.maxTTL
	cloned, err := s.svc.grantFromSnapshot(r.Context(), owner, newTag, ttl, true)
	if err != nil {
		s.svc.log.Printf("clone %s: grant from %s: %v", id, newTag, err)
		writeError(w, http.StatusInternalServerError, "failed to spawn clone")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         cloned.ID,
		"image":      cloned.Image,
		"source":     id,
		"branch_tag": newTag,
		"persistent": cloned.Persistent,
		"expires_at": cloned.ExpiresAt.UTC().Format(time.RFC3339),
	})
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

// handleByName resolves a friendly lease name to its lease id. Used by
// the SSH gateway (username = name) and for script/LLM convenience.
func (s *Server) handleByName(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	lease := s.svc.lookupByName(name)
	if lease == nil {
		writeError(w, http.StatusNotFound, "no sandbox named "+name)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":    lease.ID,
		"name":  lease.Name,
		"image": lease.Image,
	})
}

// buildShellArgs wraps a command with cwd/env into a single shell
// invocation, since forkd's exec takes argv and no cwd/env. Both env
// keys and values are shell-quoted so a hostile key cannot inject
// shell metacharacters.
func buildShellArgs(cmd, cwd string, env map[string]string) []string {
	var parts []string
	if cwd != "" {
		parts = append(parts, "cd "+shellQuote(cwd)+" &&")
	}
	for k, v := range env {
		parts = append(parts, "export "+shellQuote(k)+"="+shellQuote(v)+";")
	}
	parts = append(parts, cmd)
	// Use bash, not sh. GitHub Actions / Forgejo wrap `run:` steps with
	// `set -euo pipefail`; /bin/sh on Debian is dash, which rejects
	// `-o pipefail` (dash only accepts `-o` options in POSIX form), so
	// steps that contain a pipe fail with "set: Illegal option -o
	// pipefail". Bash is present in all base images and is the GitHub
	// Actions default shell.
	return []string{"/bin/bash", "-c", strings.Join(parts, " ")}
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

// tailStr returns the last n bytes of s, prefixed with a truncation marker
// if s was longer than n. Used for logging stderr tails on exec failures.
func tailStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "...[truncated]..." + s[len(s)-n:]
}
