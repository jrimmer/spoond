package api

import (
	"context"
	"net/http"
	"testing"
	"time"

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

// TestResumeOnUseThenExecSubstrateUnavailable: the resume-on-use and
// substrate-unknown contracts must compose (spoond-1tb5 + spoond-g077). A
// suspended lease resumes successfully on an exec, but the exec's own
// substrate call then fails to confirm the sandbox (the orchestrator List
// failed): the answer is 503 substrate_unavailable, not capacity_wait /
// lease_busy, and the lease is neither left lost nor marked lost.
func TestResumeOnUseThenExecSubstrateUnavailable(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	ctx := context.Background()
	// Room for the resume, so the only refusal can be the unknown exec.
	sub.SetNodeInfo(substrate.NodeInfo{
		Status:            "healthy",
		HugepagesTotal:    1 << 20,
		HugepageSizeBytes: 2 << 20,
	}, nil)
	l, err := svc.grantLease(ctx, leaseRequest{owner: "consumer-a", image: "py-base", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.suspend(ctx, "consumer-a", l.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if !l.Suspended {
		t.Fatal("precondition: lease not suspended")
	}
	// The resume's Create succeeds; the exec that follows is the call
	// whose substrate List fails.
	sub.FailCall("Exec", 0, substrate.ErrUnavailable)
	resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+l.ID+"/exec", "token-a", map[string]any{"cmd": "echo hi"})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("resume-then-unknown exec = %d: %v, want 503", resp.StatusCode, body)
	}
	if body["code"] != "substrate_unavailable" {
		t.Fatalf("code = %v, want substrate_unavailable: %v", body["code"], body)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatalf("503 must carry a Retry-After: %v", resp.Header)
	}
	if l.Suspended {
		t.Fatal("a successful resume left the lease suspended")
	}
	if l.State == "lost" || !l.LostAt.IsZero() {
		t.Fatalf("the unknown exec marked the lease lost: state=%s lostAt=%v", l.State, l.LostAt)
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
