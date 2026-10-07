package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// setLost marks a lease lost with a reason under the store lock, as the
// crash reconcile does.
func setLost(t *testing.T, svc *Service, id, reason string) {
	t.Helper()
	svc.store.mu.Lock()
	l := svc.store.leases[id]
	l.setState("lost")
	l.LostReason = reason
	svc.saveLeaseLocked(l)
	svc.store.mu.Unlock()
}

// lostBody asserts the 409 code lease_lost shape and that the message
// names the substrate, the reason and DELETE.
func lostBody(t *testing.T, resp *http.Response, body map[string]any, reason string) {
	t.Helper()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d (%v), want 409", resp.StatusCode, body)
	}
	if body["code"] != "lease_lost" {
		t.Fatalf("code = %v, want lease_lost", body["code"])
	}
	msg, _ := body["error"].(string)
	for _, want := range []string{"the substrate lost this lease's sandbox", reason, "DELETE the lease to free its quota"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error = %q, want it to contain %q", msg, want)
		}
	}
}

// TestLostLeaseEveryCallAnswers409: any call that acts on a lost lease
// answers 409 code lease_lost with the reason, not a 500 or a stale
// state. This covers routes that do not go through ensureLive.
func TestLostLeaseEveryCallAnswers409(t *testing.T) {
	cases := []struct {
		name, method, path string
		body               map[string]any
	}{
		{"exec", "POST", "/exec", map[string]any{"cmd": "echo hi"}},
		{"background exec", "POST", "/exec", map[string]any{"cmd": "echo hi", "background": true}},
		{"restart", "POST", "/restart", nil},
		{"keepalive", "POST", "/keepalive", nil},
		{"suspend", "POST", "/suspend", nil},
		{"resume", "POST", "/resume", nil},
		{"checkpoint", "POST", "/checkpoint", nil},
		{"tag", "POST", "/tag", map[string]any{"name": "named"}},
		{"comment", "POST", "/comment", map[string]any{"comment": "hi"}},
		{"holder", "PUT", "/holder", map[string]any{"holder": "h"}},
		{"checkpoint-policy", "PUT", "/checkpoint-policy", map[string]any{"checkpoint_interval": 120}},
		{"network", "POST", "/network", map[string]any{"network_policy": "none"}},
		{"clone", "POST", "/clone", nil},
		{"fork", "POST", "/fork", map[string]any{"count": 1}},
		{"endpoint", "GET", "/endpoint", nil},
		{"jobs", "GET", "/jobs", nil},
		{"prompt", "POST", "/prompt", map[string]any{"message": "hi"}},
		{"share", "POST", "/share", map[string]any{"grantee": "consumer-b", "mode": "http"}},
		{"snapshot save", "POST", "/snapshots", map[string]any{"name": "snap"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, svc, _, _ := newTestServerWithService(t)
			_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
			id := create["id"].(string)
			setLost(t, svc, id, "substrate crash: test reason")

			resp, body := doReq(t, tc.method, ts.URL+"/api/sandboxes/"+id+tc.path, "token-a", tc.body)
			lostBody(t, resp, body, "substrate crash: test reason")
		})
	}
}

// TestLostLeaseFilesAndDialAnswer409: the file routes and guest dial
// answer 409 lease_lost too.
func TestLostLeaseFilesAndDialAnswer409(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)
	setLost(t, svc, id, "rootfs dead")

	resp, body := doReq(t, "GET", ts.URL+"/api/sandboxes/"+id+"/files/motd", "token-a", nil)
	lostBody(t, resp, body, "rootfs dead")

	resp, body = doReq(t, "GET", ts.URL+"/api/sandboxes/"+id+"/ports/8080/dial", "token-a", nil)
	lostBody(t, resp, body, "rootfs dead")
}

// TestLostLeaseGetNamesReason: GET returns state lost and lost_reason.
func TestLostLeaseGetNamesReason(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)
	setLost(t, svc, id, "resume failed after orchestrator restart: boom")

	_, detail := doReq(t, "GET", ts.URL+"/api/sandboxes/"+id, "token-a", nil)
	if detail["state"] != "lost" {
		t.Fatalf("state = %v, want lost", detail["state"])
	}
	if detail["lost_reason"] != "resume failed after orchestrator restart: boom" {
		t.Fatalf("lost_reason = %v, want the reason", detail["lost_reason"])
	}
}

// TestLostLeaseResume409: the resume route answers 409 lease_lost with
// the reason for a lost lease.
func TestLostLeaseResume409(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	id := create["id"].(string)
	setLost(t, svc, id, "resume failed after orchestrator restart: boom")

	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/resume", "token-a", nil)
	lostBody(t, resp, body, "resume failed after orchestrator restart: boom")
}

// TestLostIdleSuspendedExec409: a lease that was idle-suspended when the
// orchestrator crashed is marked lost without clearing Suspended, so its
// next exec auto-resumes and must answer 409 lease_lost, not 500.
func TestLostIdleSuspendedExec409(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	id := create["id"].(string)
	svc.store.mu.Lock()
	l := svc.store.leases[id]
	l.Suspended = true
	l.LastAction = idleSuspendRule + "/" + heldActionSuspendIdle
	l.LastActionAt = time.Now()
	l.setState("lost")
	l.LostReason = "recovered: no checkpoint, running state gone"
	svc.saveLeaseLocked(l)
	svc.store.mu.Unlock()

	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/exec", "token-a", map[string]any{"cmd": "echo hi"})
	lostBody(t, resp, body, "recovered: no checkpoint, running state gone")
}

// TestLostEventCarriesReason: the lost event the reconcile emits names
// the reason, and the reason is persisted so later API calls report it.
func TestLostEventCarriesReason(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	subEvents := svc.Subscribe(EventFilter{})
	l, err := svc.grant(ctx, "consumer-a", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	sub.Fake.Kill(l.SandboxID)
	if summary := svc.reconcileCrash(ctx); summary.Lost != 1 {
		t.Fatalf("summary = %+v, want one loss", summary)
	}
	subEvents.Close()
	events := eventsFor(collectEvents(subEvents.C), l.ID)
	var lost LeaseEvent
	for _, ev := range events {
		if ev.Type == LeaseLost {
			lost = ev
		}
	}
	if lost.Type != LeaseLost {
		t.Fatalf("no lost event in %v", eventTypes(events))
	}
	if !strings.Contains(lost.Detail, "no checkpoint to recover from") {
		t.Fatalf("lost event detail = %q, want the reason", lost.Detail)
	}
	// The reason is persisted and reported by GET.
	if l.LostReason == "" {
		t.Fatal("lost lease has no stored reason")
	}
	if got := svc.leaseDetailMap(l)["lost_reason"]; got != lost.Detail {
		t.Fatalf("detail lost_reason = %v, want the event detail %q", got, lost.Detail)
	}
}

// TestLostReasonKeptOnce: a lease that dips in and out of the lost state
// keeps the reason it first lost for, and leaving the lost state clears
// it.
func TestLostReasonKeptOnce(t *testing.T) {
	l := &Lease{}
	setLostReason(l, "first")
	setLostReason(l, "second")
	if l.LostReason != "first" {
		t.Fatalf("LostReason = %q, want the first reason", l.LostReason)
	}
	l.setState("lost")
	if l.LostReason != "first" {
		t.Fatalf("setState lost cleared the reason: %q", l.LostReason)
	}
	l.setState("running")
	if l.LostReason != "" {
		t.Fatalf("setState running kept the reason: %q", l.LostReason)
	}
}
