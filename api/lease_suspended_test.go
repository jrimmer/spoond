package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jrimmer/spoond/v2/substrate/fake"
)

// The 409 a suspended lease answers is written with code lease_suspended
// so a client can tell it apart from the other 409 (busy, code
// lease_busy) without matching the message text. Every refusal site must
// answer JSON {"error":"lease is suspended; resume it first",
// "code":"lease_suspended"} with status 409.

// requireLeaseSuspended fails unless status is 409 and body carries the
// exact suspended-lease error and code.
func requireLeaseSuspended(t *testing.T, what string, status int, body map[string]any) {
	t.Helper()
	if status != http.StatusConflict {
		t.Fatalf("%s: status = %d, want 409: %v", what, status, body)
	}
	if body["code"] != "lease_suspended" {
		t.Fatalf("%s: code = %v, want lease_suspended: %v", what, body["code"], body)
	}
	if body["error"] != leaseSuspendedMessage {
		t.Fatalf("%s: error = %v, want %q", what, body["error"], leaseSuspendedMessage)
	}
}

// suspendedLease creates a persistent lease and suspends it through the
// public verb, so the refused routes see a hand-suspended lease (not one
// the idle sweep would auto-resume).
func suspendedLease(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
		map[string]any{"image": "py-base", "persistent": true})
	id, _ := create["id"].(string)
	if id == "" {
		t.Fatalf("create: no id in %v", create)
	}
	if resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/suspend", "token-a", nil); resp.StatusCode != 200 {
		t.Fatalf("suspend: status %d: %v", resp.StatusCode, body)
	}
	return id
}

// TestLeaseSuspendedErrorCodeNetwork: a network-policy change on a
// suspended lease answers 409 lease_suspended.
func TestLeaseSuspendedErrorCodeNetwork(t *testing.T) {
	ts, _, _, _ := newTestServerWithService(t)
	id := suspendedLease(t, ts)
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/network", "token-a",
		map[string]any{"network_policy": "lan"})
	requireLeaseSuspended(t, "network", resp.StatusCode, body)
}

// TestLeaseSuspendedErrorCodePrompt: a prompt on a suspended lease
// answers 409 lease_suspended.
func TestLeaseSuspendedErrorCodePrompt(t *testing.T) {
	ts, _, _, _ := newTestServerWithService(t)
	id := suspendedLease(t, ts)
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/prompt", "token-a",
		map[string]any{"message": "hi"})
	requireLeaseSuspended(t, "prompt", resp.StatusCode, body)
}

// TestLeaseSuspendedErrorCodeStat: the stat probe on a suspended lease
// answers 409 lease_suspended.
func TestLeaseSuspendedErrorCodeStat(t *testing.T) {
	ts, _, _, _ := newTestServerWithService(t)
	id := suspendedLease(t, ts)
	resp, body := doReq(t, "GET", ts.URL+"/api/sandboxes/"+id+"/stat", "token-a", nil)
	requireLeaseSuspended(t, "stat", resp.StatusCode, body)
}

// TestLeaseSuspendedErrorCodeFork: forking a suspended source answers
// 409 lease_suspended.
func TestLeaseSuspendedErrorCodeFork(t *testing.T) {
	ts, _, _, _ := newTestServerWithService(t)
	id := suspendedLease(t, ts)
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/fork", "token-a",
		map[string]any{"count": 1})
	requireLeaseSuspended(t, "fork", resp.StatusCode, body)
}

// TestLeaseSuspendedErrorCodeExec: exec (through ensureRunning) on a
// hand-suspended lease answers 409 lease_suspended.
func TestLeaseSuspendedErrorCodeExec(t *testing.T) {
	ts, _, _, _ := newTestServerWithService(t)
	id := suspendedLease(t, ts)
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/exec", "token-a",
		map[string]any{"cmd": "echo hi"})
	requireLeaseSuspended(t, "exec", resp.StatusCode, body)
}

// TestLeaseSuspendedErrorCodeJobSignal: signalling a running job whose
// lease is suspended answers 409 lease_suspended without reaching the
// substrate.
func TestLeaseSuspendedErrorCodeJobSignal(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
		map[string]any{"image": "py-base", "persistent": true})
	id, _ := create["id"].(string)

	p := fake.NewProcess(1021)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})

	// Suspend after the job started so the record exists and reads as
	// running; the signal must be refused before any substrate call.
	if _, err := svc.suspend(context.Background(), "consumer-a", id); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	before := calls(sub.Fake, "Exec")
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/jobs/"+jobID+"/signal", "token-a",
		map[string]any{"signal": "TERM"})
	requireLeaseSuspended(t, "job signal", resp.StatusCode, body)
	if got := calls(sub.Fake, "Exec"); got != before {
		t.Fatalf("job signal reached the substrate: Exec calls %d -> %d", before, got)
	}
}

// TestLeaseSuspendedErrorCodeProxy: a proxied guest request to a
// suspended lease answers JSON 409 lease_suspended (the site was
// plain-text http.Error before).
func TestLeaseSuspendedErrorCodeProxy(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	id := suspendedLease(t, ts)

	handler := NewServer(svc, NewImageRegistry(db)).ProxyHandler()
	req := httptest.NewRequest("GET", "http://"+id+"-3000.sandbox.example.com/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("proxy: content-type = %q, want application/json", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("proxy: body is not JSON: %v (%s)", err, rec.Body.String())
	}
	requireLeaseSuspended(t, "proxy", rec.Code, body)
}

// TestLeaseSuspendedErrorCodeHeartbeat: the guest heartbeat on a
// suspended lease answers JSON 409 lease_suspended.
func TestLeaseSuspendedErrorCodeHeartbeat(t *testing.T) {
	ts, srv, _, _, _, _ := newHeartbeatTestServer(t)
	id := suspendedLease(t, ts)

	req := httptest.NewRequest("POST", leaseHeartbeatPrefix+id+"/active", nil)
	rec := httptest.NewRecorder()
	srv.heartbeat.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("heartbeat: content-type = %q, want application/json", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("heartbeat: body is not JSON: %v (%s)", err, rec.Body.String())
	}
	requireLeaseSuspended(t, "heartbeat", rec.Code, body)
}

// TestLeaseSuspendedErrorCodeLLM: the per-lease LLM gateway on a
// suspended lease answers JSON 409 lease_suspended.
func TestLeaseSuspendedErrorCodeLLM(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	srv := NewServerWithLLM(svc, NewImageRegistry(db), "http://127.0.0.1:1", "host-key", "", nil)
	h := httptest.NewServer(srv.Handler())
	t.Cleanup(h.Close)
	id := suspendedLease(t, h)

	req := httptest.NewRequest("POST", llmGatewayPrefix+id+"/openai/chat/completions",
		strings.NewReader(`{"model":"m"}`))
	rec := httptest.NewRecorder()
	srv.llm.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("llm: content-type = %q, want application/json", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("llm: body is not JSON: %v (%s)", err, rec.Body.String())
	}
	requireLeaseSuspended(t, "llm", rec.Code, body)
}
