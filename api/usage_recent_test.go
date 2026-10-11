package api

import (
	"fmt"
	"net/http"
	"testing"
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
	got := b.recentFor("o", usageRecentTypes, 3)
	for i, want := range []int{leaseEventRingSize + 6, leaseEventRingSize + 5, leaseEventRingSize + 4} {
		if got[i].Detail != fmt.Sprint(want) {
			t.Fatalf("recent[%d] = %s, want %d", i, got[i].Detail, want)
		}
	}
}
