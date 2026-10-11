package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestUsageRecentEvents: GET /api/usage carries "recent", the caller's
// newest refusal and reclaim events from the ring, newest first, at most
// usageRecentLimit (#140 U3). Other owners' events and other types are
// left out.
func TestUsageRecentEvents(t *testing.T) {
	_, h, svc, sub, ids := newAdmitServer(t)
	installDynamicNode(t, svc, sub, 1<<20, 0, 0)
	roomyDisk(svc)
	one, two := uid(t, ids, "tok-1"), uid(t, ids, "tok-2")

	svc.bus.emit("l-1", one, LeaseCreated, "not listed")
	// box_full through the production path, which names the requester.
	for i := 0; i < usageRecentLimit+5; i++ {
		svc.boxFullAlert(one, 1024+i, "every take-back candidate is pinned")
	}
	svc.boxFullAlert(two, 2048, "another owner's refusal")
	svc.bus.emit("l-two", two, LeaseTakeBack, "another owner's")
	// disk_cleanup is host-wide (owner ""): never listed.
	svc.emitDiskCleanup(1<<30, 0)
	svc.bus.emitStructured("l-last", one, LeaseSuspended, "taken back", "take_back", "", "b-1")
	svc.bus.emit("l-2", one, LeaseResumed, "not listed")

	rec, body := doUsersReq(t, h, "GET", "/api/usage", "tok-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/usage: %d %s", rec.Code, rec.Body.String())
	}
	recent, ok := body["recent"].([]any)
	if !ok || len(recent) != usageRecentLimit {
		t.Fatalf("recent = %v, want %d events", body["recent"], usageRecentLimit)
	}
	first := recent[0].(map[string]any)
	if first["type"] != string(LeaseSuspended) || first["reason"] != "take_back" || first["lease_id"] != "l-last" {
		t.Fatalf("newest recent event = %v, want the take-back suspend", first)
	}
	if second := recent[1].(map[string]any); second["type"] != string(LeaseBoxFull) ||
		second["detail"] != fmt.Sprintf("%d MiB request for %q: every take-back candidate is pinned", 1024+usageRecentLimit+4, one) {
		t.Fatalf("second recent event = %v, want the newest box_full", second)
	}
	for _, e := range recent {
		if m := e.(map[string]any); m["owner"] != one {
			t.Fatalf("recent lists another owner's event: %v", m)
		}
	}

	// An owner with no listed events gets an empty list, not null.
	_, body = doUsersReq(t, h, "GET", "/api/usage", "admin-tok", "")
	if r, ok := body["recent"].([]any); !ok || len(r) != 0 {
		t.Fatalf("admin recent = %#v, want empty list", body["recent"])
	}
}

// TestRecentForAcrossRingWrap: once the ring has wrapped, recentFor
// still reads newest first.
func TestRecentForAcrossRingWrap(t *testing.T) {
	b := newEventBus()
	for i := 0; i < leaseEventRingSize+7; i++ {
		b.emit("l", "o", LeaseBoxFull, fmt.Sprint(i))
	}
	got := b.recentFor("o", usageRecentEvent, 3)
	for i, want := range []int{leaseEventRingSize + 6, leaseEventRingSize + 5, leaseEventRingSize + 4} {
		if got[i].Detail != fmt.Sprint(want) {
			t.Fatalf("recent[%d] = %s, want %d", i, got[i].Detail, want)
		}
	}
}

// TestUsageRecentListsSpoondReleases: "recent" lists the releases
// spoond made on its own clock (a TTL expiry, the paused-release clock,
// a lost lease's grace) and not the ones the owner asked for. A real
// TTL sweep produces the listed event.
func TestUsageRecentListsSpoondReleases(t *testing.T) {
	_, h, svc, sub, ids := newAdmitServer(t)
	installDynamicNode(t, svc, sub, 1<<20, 0, 0)
	roomyDisk(svc)
	one := uid(t, ids, "tok-1")
	ctx := context.Background()

	l, err := svc.grantLease(ctx, leaseRequest{owner: one, image: "mid", ttl: time.Millisecond})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	svc.sweepExpired(ctx)
	svc.emitLeaseEvent("l-owner", one, LeaseReleased, "deleted through the API")

	rec, body := doUsersReq(t, h, "GET", "/api/usage", "tok-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/usage: %d %s", rec.Code, rec.Body.String())
	}
	var ttl, owner bool
	for _, e := range body["recent"].([]any) {
		m := e.(map[string]any)
		if m["type"] != string(LeaseReleased) {
			continue
		}
		ttl = ttl || (m["lease_id"] == l.ID && m["detail"] == "TTL expired")
		owner = owner || m["lease_id"] == "l-owner"
	}
	if !ttl || owner {
		t.Fatalf("recent lists the TTL release %v (want true) and the owner's delete %v (want false): %v", ttl, owner, body["recent"])
	}
}

// TestCreateWarnsWhenNotPersistent: a non-persistent create gets a
// Warning header naming its TTL when the TTL was defaulted or a pin or
// holder is set (since 3.0 neither keeps a lease past its TTL); a
// persistent create, or one with an explicit TTL and nothing else, gets
// none.
func TestCreateWarnsWhenNotPersistent(t *testing.T) {
	_, h, svc, sub, _ := newAdmitServer(t)
	installDynamicNode(t, svc, sub, 1<<20, 0, 0)
	roomyDisk(svc)
	for _, c := range []struct {
		body string
		warn bool
	}{
		{`{"image":"mid"}`, true},
		{`{"image":"mid","ttl":60,"pinned":true}`, true},
		{`{"image":"mid","ttl":60,"holder":"flight-1"}`, true},
		{`{"image":"mid","ttl":60}`, false},
		{`{"image":"mid","persistent":true,"pinned":true}`, false},
	} {
		rec, _ := createSandboxBody(t, h, "tok-1", c.body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s: %d %s", c.body, rec.Code, rec.Body.String())
		}
		got := rec.Header().Get("Warning")
		if (got != "") != c.warn || (c.warn && !strings.Contains(got, "not persistent")) {
			t.Fatalf("%s: Warning %q, want warn=%v", c.body, got, c.warn)
		}
	}
}
