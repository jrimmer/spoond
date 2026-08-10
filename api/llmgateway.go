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
	"strings"
	"time"
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
	upstreamURL  *url.URL
	key          string
	providers    []string          // ordered longest-first provider prefixes
	modelMap     map[string]string // exe.dev catalog id -> upstream model
	defaultModel string            // fallback when the requested id isn't mapped
}

// newLLMGateway wires the OpenAI-compatible upstream. upstreamURL is the
// API base (e.g. https://openrouter.ai/api/v1); key is the server-side
// credential, never exposed to guests. modelMap translates exe.dev
// catalog model ids to upstream model ids (e.g. "gpt-oss-20b-fireworks"
// -> "gpt-oss:20b"); defaultModel is applied when a requested id is
// neither mapped nor known to the upstream (Shelley also probes
// gpt-5.x/claude ids for slug generation etc.).
func newLLMGateway(log *log.Logger, lookup func(string) *Lease, upstreamURL, key, defaultModel string, modelMap map[string]string) *llmGateway {
	u, err := url.Parse(strings.TrimSuffix(upstreamURL, "/"))
	if err != nil {
		panic("llm gateway: bad upstream URL: " + err.Error())
	}
	return &llmGateway{
		log:          log,
		lookup:       lookup,
		upstreamURL:  u,
		key:          key,
		providers:    []string{"/fireworks/inference", "/openai", "/xai"},
		modelMap:     modelMap,
		defaultModel: defaultModel,
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
// itself validates the lease.
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
	if lease == nil {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if lease.Suspended {
		http.Error(w, "sandbox is suspended; resume it first", http.StatusConflict)
		return
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
		body, _ = io.ReadAll(r.Body)
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
	resp, err := transport.RoundTrip(outReq)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			g.log.Printf("llm gateway: %s: %v", leaseID, err)
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
