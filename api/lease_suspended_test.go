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

// Every automatically or manually suspended lease resumes on its
// holder's next work call (#145 D2). No work path answers the old 409
// lease_suspended any more. The code stays only for the paths that
// genuinely cannot resume a suspended lease because they need a running
// guest and are not "work": GET and status (the stat probe), forking a
// suspended source (a checkpoint needs the guest) and the crash test
// (nothing is running to crash). Those answer 409 lease_suspended with
// the structured reason the suspension carried.

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
// public verb, so the refused routes see a hand-suspended lease.
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

// TestLeaseSuspendedErrorCodeStat: the stat probe needs a running guest
// and is not a resume-on-use work call, so it answers 409
// lease_suspended.
func TestLeaseSuspendedErrorCodeStat(t *testing.T) {
	ts, _, _, _ := newTestServerWithService(t)
	id := suspendedLease(t, ts)
	resp, body := doReq(t, "GET", ts.URL+"/api/sandboxes/"+id+"/stat", "token-a", nil)
	requireLeaseSuspended(t, "stat", resp.StatusCode, body)
}

// TestLeaseSuspendedErrorCodeFork: forking a suspended source needs the
// guest to checkpoint, so it answers 409 lease_suspended.
func TestLeaseSuspendedErrorCodeFork(t *testing.T) {
	ts, _, _, _ := newTestServerWithService(t)
	id := suspendedLease(t, ts)
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/fork", "token-a",
		map[string]any{"count": 1})
	requireLeaseSuspended(t, "fork", resp.StatusCode, body)
}

// requireNoLeaseSuspended fails when a response is the old 409
// lease_suspended body: a work call must resume a suspended lease, never
// answer that code.
func requireNoLeaseSuspended(t *testing.T, what string, status int, body map[string]any) {
	t.Helper()
	if status == http.StatusConflict && body["code"] == "lease_suspended" {
		t.Fatalf("%s answered 409 lease_suspended; it must resume on use: %v", what, body)
	}
}

// TestResumeOnUseExec: exec on a suspended lease resumes it and serves
// the call, never 409 lease_suspended.
func TestResumeOnUseExec(t *testing.T) {
	ts, _, _, _ := newTestServerWithService(t)
	id := suspendedLease(t, ts)
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/exec", "token-a",
		map[string]any{"cmd": "echo hi"})
	requireNoLeaseSuspended(t, "exec", resp.StatusCode, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exec on a suspended lease: %d: %v", resp.StatusCode, body)
	}
}

// TestResumeOnUseNetwork: a network-policy change on a suspended lease
// resumes it (the guest must be running to apply egress) and never
// answers 409 lease_suspended.
func TestResumeOnUseNetwork(t *testing.T) {
	ts, _, _, _ := newTestServerWithService(t)
	id := suspendedLease(t, ts)
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/network", "token-a",
		map[string]any{"network_policy": "lan"})
	requireNoLeaseSuspended(t, "network", resp.StatusCode, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("network on a suspended lease: %d: %v", resp.StatusCode, body)
	}
}

// TestResumeOnUsePrompt: a prompt on a suspended lease resumes it and
// serves the call.
func TestResumeOnUsePrompt(t *testing.T) {
	ts, _, _, _ := newTestServerWithService(t)
	id := suspendedLease(t, ts)
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/prompt", "token-a",
		map[string]any{"message": "hi"})
	requireNoLeaseSuspended(t, "prompt", resp.StatusCode, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("prompt on a suspended lease: %d: %v", resp.StatusCode, body)
	}
}

// TestResumeOnUseJobSignal: signalling a job on a suspended lease
// resumes it first; the signal then succeeds or reports the job not
// running, never 409 lease_suspended.
func TestResumeOnUseJobSignal(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
		map[string]any{"image": "py-base", "persistent": true})
	id, _ := create["id"].(string)

	p := fake.NewProcess(1021)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})

	// Suspend after the job started so the record exists and reads as
	// running; the signal must resume the lease first.
	if _, err := svc.suspend(context.Background(), "consumer-a", id); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/jobs/"+jobID+"/signal", "token-a",
		map[string]any{"signal": "TERM"})
	requireNoLeaseSuspended(t, "job signal", resp.StatusCode, body)
	if svc.lookupAny(id).Suspended {
		t.Fatal("the job signal did not resume the suspended lease")
	}
}

// TestResumeOnUseProxy: a proxied guest request to a suspended lease
// resumes it; the downstream then answers from the fake (never 409).
func TestResumeOnUseProxy(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	id := suspendedLease(t, ts)

	handler := NewServer(svc, NewImageRegistry(db)).ProxyHandler()
	req := httptest.NewRequest("GET", "http://"+id+"-3000.sandbox.example.com/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusConflict {
		t.Fatalf("proxy answered 409; it must resume on use: %s", rec.Body.String())
	}
	if svc.lookupAny(id).Suspended {
		t.Fatal("the proxied request did not resume the suspended lease")
	}
}

// TestLeaseSuspendedErrorCodeHeartbeat: the guest heartbeat is not a
// work call (it only moves LastActive), so a suspended lease answers 409
// lease_suspended.
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

// TestResumeOnUseLLM: the per-lease LLM gateway resumes a suspended
// lease on use and forwards the request (never 409 lease_suspended).
func TestResumeOnUseLLM(t *testing.T) {
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
	if rec.Code == http.StatusConflict {
		t.Fatalf("llm answered 409; it must resume on use: %s", rec.Body.String())
	}
	if svc.lookupAny(id).Suspended {
		t.Fatal("the gateway call did not resume the suspended lease")
	}
}
