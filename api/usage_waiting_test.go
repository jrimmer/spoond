package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v4/substrate"
)

// TestUsageWaitingCreates: GET /api/usage carries "waiting", the
// caller's own queued creates with their place in the whole queue
// (#140 U2). Another owner's tickets are not listed but still count
// toward the position, and a finished ticket drops out.
func TestUsageWaitingCreates(t *testing.T) {
	_, h, svc, sub, ids := newAdmitServer(t)
	installDynamicNode(t, svc, sub, 1<<20, 0, 0)
	roomyDisk(svc)
	one, two := uid(t, ids, "tok-1"), uid(t, ids, "tok-2")
	// tok-1 runs more, so tok-2's tickets are tried first.
	addRunningLease(svc, "l-one", one, 4096)
	addRunningLease(svc, "l-two", two, 1024)
	svc.updateNodeMetrics(context.Background())
	svc.invalidateFairShares()

	req := func(owner, image string) leaseRequest {
		return leaseRequest{owner: owner, image: image, ttl: time.Minute}
	}
	tOne := svc.newAdmissionTicket(one, req(one, "mid"), substrate.ErrCapacity, time.Minute)
	tTwoA := svc.newAdmissionTicket(two, req(two, "small"), substrate.ErrCapacity, time.Minute)
	tTwoB := svc.newAdmissionTicket(two, req(two, "mid"), substrate.ErrCapacity, time.Minute)
	t.Cleanup(svc.drainQueue)

	waiting := func(tok string) []any {
		t.Helper()
		rec, body := doUsersReq(t, h, "GET", "/api/usage", tok, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/usage as %s: %d %s", tok, rec.Code, rec.Body.String())
		}
		w, ok := body["waiting"].([]any)
		if !ok {
			t.Fatalf("GET /api/usage as %s: waiting = %#v, want a list", tok, body["waiting"])
		}
		return w
	}
	check := func(entry any, id, image string, pos float64) {
		t.Helper()
		e := entry.(map[string]any)
		if e["id"] != id || e["image"] != image || e["position"] != pos || e["reason"] != "capacity" {
			t.Fatalf("waiting entry = %v, want id %s image %s position %v reason capacity", e, id, image, pos)
		}
	}

	w := waiting("tok-1")
	if len(w) != 1 {
		t.Fatalf("tok-1 waiting = %v, want its one ticket", w)
	}
	check(w[0], tOne.id, "mid", 3)

	w = waiting("tok-2")
	if len(w) != 2 {
		t.Fatalf("tok-2 waiting = %v, want its two tickets", w)
	}
	check(w[0], tTwoA.id, "small", 1)
	check(w[1], tTwoB.id, "mid", 2)

	// A finished ticket leaves the queue: tok-1 moves up.
	svc.finishTicket(tTwoA)
	check(waiting("tok-1")[0], tOne.id, "mid", 2)

	// Nothing queued: an empty list, not null.
	if w := waiting("admin-tok"); len(w) != 0 {
		t.Fatalf("admin waiting = %v, want empty", w)
	}
}
