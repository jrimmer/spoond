package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jrimmer/spoond/identity"
	"github.com/jrimmer/spoond/metrics"
)

// llmGatewayPrefix is the path prefix for the per-lease LLM gateway.
// Guests reach it at http(s)://<host>:<port>/llm/<lease-id>/<provider>/...
// The lease id in the path is the capability (same model as the SSH
// gateway and the HTTP proxy hostname): a guest that knows its lease id
// can use the gateway, and the real provider key never enters the VM.
const llmGatewayPrefix = "/llm/"

// llmGateway is the OpenAI-compatible per-lease LLM proxy following the
// exe.dev gateway contract. Shelley's Gateway source (modelsources)
// calls {gateway}/<provider>/... with an "implicit" credential; the
// gateway injects the real provider key server-side.
//
// Provider prefixes match the exe.dev contract:
//
//	/openai/...                 OpenAI-compatible chat/responses
//	/fireworks/inference/...    Fireworks (OpenAI-compatible)
//	/xai/...                    xAI (OpenAI-compatible)
//	/anthropic/...              Anthropic (not supported -> 501)
//
// All OpenAI-compatible providers are routed to the same configured
// upstream (OpenRouter, ollama.com, llama.cpp, ...).
type llmGateway struct {
	log          *log.Logger
	lookup       func(leaseID string) *Lease
	users        *identity.Store // per-user LLM keys (U8/T8); nil = legacy open mode
	upstreamURL  *url.URL
	key          string
	providers    []string          // ordered longest-first provider prefixes
	modelMap     map[string]string // exe.dev catalog id -> upstream model
	defaultModel string            // fallback when the requested id isn't mapped
	// Per-user concurrent request cap (U8/T8): maxConcurrent 0 =
	// unlimited. inflight counts in-flight forwarded requests per lease
	// owner (the authenticated user after the key check).
	maxConcurrent int
	inflightMu    sync.Mutex
	inflight      map[string]int
	// requireKey (security review #37 C2): when true (identity store
	// present and the caller did NOT opt into legacy-open), a lease
	// whose owner has no LLM key configured is DENIED instead of
	// silently open. Set via LLM_OPEN_LEGACY=1 to keep the pre-U8
	// capability model for keyless owners.
	requireKey bool
	// metrics (issue #20): LLM gateway Prometheus metrics.
	metrics *metrics.BackendMetrics
}

// newLLMGateway wires the OpenAI-compatible upstream. upstreamURL is the
// API base (e.g. https://openrouter.ai/api/v1); key is the server-side
// credential, never exposed to guests. modelMap translates exe.dev
// catalog model ids to upstream model ids (e.g. "gpt-oss-20b-fireworks"
// -> "gpt-oss:20b"); defaultModel is applied when a requested id is
// neither mapped nor known to the upstream (Shelley also probes
// gpt-5.x/claude ids for slug generation etc.). users is the identity
// store consulted for per-user LLM keys; nil keeps the gateway open for
// every lease (legacy single-user behavior).
func newLLMGateway(log *log.Logger, lookup func(string) *Lease, users *identity.Store, upstreamURL, key, defaultModel string, modelMap map[string]string) *llmGateway {
	u, err := url.Parse(strings.TrimSuffix(upstreamURL, "/"))
	if err != nil {
		panic("llm gateway: bad upstream URL: " + err.Error())
	}
	return &llmGateway{
		log:          log,
		lookup:       lookup,
		users:        users,
		upstreamURL:  u,
		key:          key,
		providers:    []string{"/fireworks/inference", "/openai", "/xai"},
		modelMap:     modelMap,
		defaultModel: defaultModel,
		inflight:     map[string]int{},
	}
}

// mapModel rewrites the request body's "model" field through the
// gateway's model map / default, so Shelley's exe.dev-catalog ids work
// against the real upstream. Returns the original body when nothing
// changes.
func (g *llmGateway) mapModel(body []byte) []byte {
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil || probe.Model == "" {
		return body
	}
	mapped := probe.Model
	if m, ok := g.modelMap[mapped]; ok {
		mapped = m
	} else if g.defaultModel != "" {
		mapped = g.defaultModel
	}
	if mapped == probe.Model {
		return body
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	m["model"] = mapped
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// ServeHTTP handles a /llm/ request. The main server's authMiddleware
// exempts this prefix (capability = lease id in path), so the handler
// itself validates the lease. When the lease owner has a per-user LLM
// key configured (U8/T8), the request must present it in the standard
// OpenAI-compatible Authorization header; the user key only authorizes
// the caller and never reaches the upstream (the host key below
// authenticates to the provider). Owners without a key keep the legacy
// open behavior.
func (g *llmGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, llmGatewayPrefix)
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		http.Error(w, "usage: /llm/<lease-id>/<provider>/...", http.StatusBadRequest)
		return
	}
	leaseID := rest[:slash]
	sub := rest[slash:] // e.g. "/openai/chat/completions"

	lease := g.lookup(leaseID)
	start := time.Now()
	provider := "unknown"
	if lease == nil {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if lease.Suspended {
		http.Error(w, "sandbox is suspended; resume it first", http.StatusConflict)
		return
	}

	// Per-user LLM key auth (U8/T8): when the lease owner has a key
	// configured, the caller must present it. A key that verifies is by
	// construction the owner's key, so this check doubles as the
	// lease-owner check (the U9 share case extends this predicate).
	// Security review #37 C2: when the identity store is present and the
	// deployment has not opted into legacy-open, a keyless owner's lease
	// is DENIED rather than silently open — otherwise any token holder
	// could burn the host's LLM quota through another user's lease.
	if g.users != nil {
		owner := g.users.UserByID(lease.Owner)
		if owner != nil && owner.LLMKeyHash != "" {
			auth := r.Header.Get("Authorization")
			key := strings.TrimPrefix(auth, "Bearer ")
			if key == "" || key == auth || !g.users.LLMKeyOK(owner.ID, key) {
				if g.metrics != nil {
					g.metrics.LLMKeyFail.Inc()
				}
				http.Error(w, "missing or invalid LLM key (admin: POST /api/users/{id}/llm-key)", http.StatusUnauthorized)
				return
			}
		} else if owner != nil && g.requireKey {
			// Identity-store user WITHOUT a key: deny unless the
			// deployment opted into legacy-open. (Legacy consumer-owned
			// leases — owner not in the store — keep the capability
			// model: the operator controls those tokens.)
			http.Error(w, "LLM access requires the lease owner to configure an LLM key (admin: POST /api/users/{id}/llm-key)", http.StatusUnauthorized)
			return
		}
	}

	// Per-user concurrent request cap (U8/T8): count only forwarded
	// requests (auth already passed). maxConcurrent 0 = unlimited.
	if g.maxConcurrent > 0 {
		g.inflightMu.Lock()
		if g.inflight[lease.Owner] >= g.maxConcurrent {
			g.inflightMu.Unlock()
			if g.metrics != nil {
				g.metrics.LLMRateLimit.Inc()
			}
			http.Error(w, "too many concurrent LLM requests for this user", http.StatusTooManyRequests)
			return
		}
		g.inflight[lease.Owner]++
		g.inflightMu.Unlock()
		defer func() {
			g.inflightMu.Lock()
			if g.inflight[lease.Owner] <= 1 {
				delete(g.inflight, lease.Owner)
			} else {
				g.inflight[lease.Owner]--
			}
			g.inflightMu.Unlock()
		}()
	}

	// Match the longest provider prefix and forward the remainder to the
	// upstream base (e.g. /openai/chat/completions ->
	// <upstream>/chat/completions).
	var tail string
	matched := false
	for _, p := range g.providers {
		if sub == p || strings.HasPrefix(sub, p+"/") {
			tail = strings.TrimPrefix(sub, p)
			matched = true
			provider = strings.TrimPrefix(p, "/")
			break
		}
	}
	if !matched {
		if strings.HasPrefix(sub, "/anthropic") {
			http.Error(w, "provider not configured (anthropic requires a direct key; use /openai)", http.StatusNotImplemented)
			return
		}
		http.Error(w, "provider not configured (only /openai, /fireworks/inference, /xai)", http.StatusNotImplemented)
		return
	}

	// Rewrite the target: upstream base + tail. The upstream base may
	// itself carry a version path (e.g. https://ollama.com/v1); avoid
	// doubling it when the tail also starts with /v1 (Shelley's
	// fireworks client calls /v1/chat/completions under the provider
	// prefix).
	target := *g.upstreamURL
	target.Path = strings.TrimSuffix(g.upstreamURL.Path, "/") + tail
	if strings.HasSuffix(g.upstreamURL.Path, "/v1") && strings.HasPrefix(tail, "/v1/") {
		target.Path = strings.TrimSuffix(g.upstreamURL.Path, "/v1") + tail
	}
	target.RawQuery = r.URL.RawQuery

	// Read the body, apply the model remap, then forward explicitly.
	// We do NOT use httputil.ReverseProxy here: its Director cannot
	// change ContentLength reliably (it re-copies the inbound length
	// after Director runs, which breaks when the remap changes body
	// size -> "ContentLength=N with Body length M"). A direct client
	// keeps body, length, and streaming fully under our control.
	var body []byte
	if r.Body != nil {
		// Cap the request body (security review #37 rescan F10): an
		// unbounded ReadAll lets a caller push arbitrary memory into the
		// gateway (and upstream). LLM chat bodies are small; 1 MiB is
		// generous for any chat/completions payload.
		body, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
		r.Body.Close()
	}
	if g.modelMap != nil || g.defaultModel != "" {
		body = g.mapModel(body)
	}

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, "llm gateway: "+err.Error(), http.StatusBadRequest)
		return
	}
	outReq.Header = r.Header.Clone()
	outReq.Header.Set("Authorization", "Bearer "+g.key)
	outReq.Header.Del("X-Lease-Id")
	outReq.Host = g.upstreamURL.Host

	transport := &http.Transport{
		DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		// Allow response streaming (no buffering of SSE).
		ResponseHeaderTimeout: 60 * time.Second,
		MaxIdleConns:          16,
		IdleConnTimeout:       60 * time.Second,
	}
	if g.metrics != nil {
		g.metrics.LLMReqs.WithLabelValues(provider).Inc()
		g.metrics.LLMInflight.WithLabelValues(lease.Owner).Inc()
		defer func() {
			g.metrics.LLMDur.WithLabelValues(provider).Observe(time.Since(start).Seconds())
			g.metrics.LLMInflight.WithLabelValues(lease.Owner).Dec()
		}()
	}
	resp, err := transport.RoundTrip(outReq)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			g.log.Printf("llm gateway: %s: %v", leaseID, err)
		}
		if g.metrics != nil {
			g.metrics.LLMErrors.WithLabelValues(provider, "502").Inc()
		}
		http.Error(w, "llm gateway error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy the response headers, then stream the body (SSE-safe).
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.Header().Del("Server")
	if g.metrics != nil && resp.StatusCode >= 400 {
		g.metrics.LLMErrors.WithLabelValues(provider, strconv.Itoa(resp.StatusCode)).Inc()
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			return
		}
	}
}
