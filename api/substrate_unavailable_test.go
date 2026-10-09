package api

import (
	"net/http"
	"testing"

	"github.com/jrimmer/spoond/v2/substrate"
)

// TestExecSubstrateUnavailableIsRetryable covers the API side of the
// substrate-unknown contract: a substrate operation that could not
// confirm the sandbox's state (the orchestrator List failed) answers 503
// with a Retry-After and code substrate_unavailable, and the lease is not
// marked lost. 410 lease_lost is final for clients, so an orchestrator
// stall must never become one.
func TestExecSubstrateUnavailableIsRetryable(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)
	sub.FailCall("Exec", 0, substrate.ErrUnavailable)
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/exec", "token-a", map[string]any{"cmd": "echo hi"})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("substrate unavailable exec = %d: %v, want 503", resp.StatusCode, body)
	}
	if body["code"] != "substrate_unavailable" {
		t.Fatalf("code = %v, want substrate_unavailable: %v", body["code"], body)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatalf("503 must carry a Retry-After: %v", resp.Header)
	}
	if got := leaseState(svc, id); got == "lost" {
		t.Fatalf("lease state after a substrate-unknown exec = %q, must not be lost", got)
	}
}

// TestExecSandboxConfirmedGoneIsStillGone pins the other side: a
// substrate that listed the sandbox and found it absent still answers 410
// lease_lost.
func TestExecSandboxConfirmedGoneIsStillGone(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)
	svc.store.mu.Lock()
	sandboxID := svc.store.leases[id].SandboxID
	svc.store.mu.Unlock()
	sub.Kill(sandboxID)
	sub.FailCall("Exec", 0, substrate.ErrNotFound)
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/exec", "token-a", map[string]any{"cmd": "echo hi"})
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("confirmed-gone exec = %d: %v, want 410", resp.StatusCode, body)
	}
}

// leaseState reads a lease's state under the store lock.
func leaseState(svc *Service, id string) string {
	svc.store.mu.Lock()
	defer svc.store.mu.Unlock()
	if l := svc.store.leases[id]; l != nil {
		return l.State
	}
	return ""
}
