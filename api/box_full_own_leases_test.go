package api

// The own_leases body of the biggest-borrower box_full refusal
// (spoond-2rkm): the refusal lists the requester's own running leases,
// least recently used first, so the caller can see what holds its memory
// and pause one itself. Nothing new is paused and no other owner's lease
// is ever listed. A disk/pinned box_full (boxFullError, no leases)
// answers without the key.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ownLeasesOf pulls own_leases out of a decoded JSON body as the raw
// array, so each test reads exactly what the wire carried.
func ownLeasesOf(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["own_leases"]
	if !ok {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		t.Fatalf("own_leases is %T, want an array", raw)
	}
	out := make([]map[string]any, 0, len(arr))
	for _, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("own_leases element is %T, want an object", e)
		}
		out = append(out, m)
	}
	return out
}

// TestBoxFullOwnLeasesHTTP: a create refused with the biggest-borrower
// box_full carries own_leases over HTTP — the requester's own running
// leases, LRU first, with pinned flagged and the name echoed — and never
// another owner's lease. Another owner's create on the same box is
// served by take-back and succeeds as a 201.
func TestBoxFullOwnLeasesHTTP(t *testing.T) {
	svc, db, sub, ids := newFairShareService(t)
	alice := addIdentityUser(t, ids, "alice")
	bob := addIdentityUser(t, ids, "bob")
	// The fair-share service clears the legacy tokens; map the test
	// tokens straight onto the identity users, so doReq authenticates as
	// them without changing the owner set (the ids already exist).
	svc.tokens = map[string]string{"token-a": alice.ID, "token-b": bob.ID}
	ctx := context.Background()
	seedImage(t, db, "mid", 1024)
	roomyDisk(svc)
	// 5120 MiB box, each sandbox 1024 MiB: two 2560 MiB slices. The
	// five leases granted below fill it exactly, so the next admission
	// needs take-back (or is refused).
	installDynamicNode(t, svc, sub, 2560, 0, 512)

	// Alice runs one lease over her slice: 4096 of a 2560 slice. The
	// leases are made clearly LRU-ordered and one is pinned, so the
	// listing has something to say.
	var al []*Lease
	for i := 0; i < 4; i++ {
		l, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "mid", ttl: time.Hour, persistent: true})
		if err != nil {
			t.Fatalf("grant alice %d: %v", i, err)
		}
		al = append(al, l)
	}
	if _, err := svc.setName(alice.ID, al[0].ID, "alice-lru"); err != nil {
		t.Fatalf("tag: %v", err)
	}
	if _, err := svc.setPinned(alice.ID, al[1].ID, true); err != nil {
		t.Fatalf("pin: %v", err)
	}
	// Bob holds the rest of the box inside his slice, and (being the
	// oldest lease on the box) would be the first row of a leaked
	// whole-box listing.
	bLease, err := svc.grantLease(ctx, leaseRequest{owner: bob.ID, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant bob: %v", err)
	}
	base := time.Now().Add(-time.Hour)
	svc.store.mu.Lock()
	// LRU-ordered so the wire order is al[3] (oldest), al[2], al[1]
	// (pinned), al[0] (youngest).
	for i, l := range al {
		l.LastActive = base.Add(time.Duration(4-i) * time.Minute)
		svc.saveLeaseLocked(l)
	}
	bLease.LastActive = base.Add(-time.Minute)
	svc.saveLeaseLocked(bLease)
	svc.store.mu.Unlock()

	// The HTTP server rides the same service, so doReq answers from the
	// live store view.
	ts := httptest.NewServer(NewServer(svc, NewImageRegistry(db)).Handler())
	t.Cleanup(ts.Close)

	dropNodeCache(svc)
	warmFairShares(svc, ctx)
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "mid", "ttl": 3600})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("alice's over-slice create = %d (%v), want 429 box_full", resp.StatusCode, body)
	}
	if body["code"] != "box_full" {
		t.Fatalf("code = %v, want box_full", body["code"])
	}
	leases := ownLeasesOf(t, body)
	if len(leases) != 4 {
		t.Fatalf("own_leases has %d rows, want alice's 4 running leases (%v)", len(leases), body["own_leases"])
	}
	// LRU first; bob's lease (the oldest of the box) must not appear.
	wantOrder := []string{al[3].ID, al[2].ID, al[1].ID, al[0].ID}
	for i, w := range wantOrder {
		if got, _ := leases[i]["id"].(string); got != w {
			t.Fatalf("own_leases[%d].id = %s, want %s (LRU first); rows: %v", i, got, w, leases)
		}
	}
	if mb, _ := leases[0]["memory_mb"].(float64); mb != 1024 {
		t.Fatalf("own_leases[0].memory_mb = %v, want 1024", leases[0]["memory_mb"])
	}
	if name, _ := leases[3]["name"].(string); name != "alice-lru" {
		t.Fatalf("own_leases[3].name = %v, want alice-lru", leases[3]["name"])
	}
	if pinned, _ := leases[2]["pinned"].(bool); !pinned {
		t.Fatalf("own_leases[2].pinned = %v, want true for %s", leases[2]["pinned"], al[1].ID)
	}
	if pinned, _ := leases[0]["pinned"].(bool); pinned {
		t.Fatalf("own_leases[0].pinned = %v, want false", leases[0]["pinned"])
	}
	if _, ok := leases[0]["last_active"]; !ok {
		t.Fatal("own_leases[0] carries no last_active")
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), bLease.ID) {
		t.Fatalf("own_leases leaked another owner's lease %s: %s", bLease.ID, raw)
	}

	// Nothing was paused by the refusal: the listing is the only effect.
	for _, l := range al {
		if l.Suspended {
			t.Fatalf("lease %s was paused by a box_full listing", l.ID)
		}
	}

	// A create by another owner on the same box is take-back fodder, not
	// a refusal: bob is inside his slice, so an over-slice alice lease is
	// paused and his create succeeds without own_leases.
	dropNodeCache(svc)
	warmFairShares(svc, ctx)
	resp2, body2 := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-b", map[string]any{"image": "mid", "ttl": 3600})
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("bob's create = %d (%v), want 201 (take-back pauses an over-slice lease)", resp2.StatusCode, body2)
	}
	if _, present := body2["own_leases"]; present {
		t.Fatalf("a 201 create carries own_leases: %v", body2)
	}
}

// TestBoxFullOwnLeasesNoOtherOwners: the refusal is built only from the
// requester's take-back row, so even when another owner holds the rest of
// the box only the requester's leases can appear; LRU first (ID breaks
// ties) and every flag carried.
func TestBoxFullOwnLeasesNoOtherOwners(t *testing.T) {
	owners := []takeBackOwner{
		{Owner: "req", UsedMiB: 4096, SliceMiB: 2048, Leases: []takeBackLease{
			{ID: "r-new", Name: "fresh", MemoryMiB: 1024, LastActive: time.Unix(1_000_100, 0)},
			{ID: "r-old", MemoryMiB: 3072, Pinned: true, LastActive: time.Unix(1_000_000, 0)},
			{ID: "r-busy", MemoryMiB: 4096, Busy: true, LastActive: time.Unix(1_000_050, 0)},
		}},
		{Owner: "bob", UsedMiB: 2048, SliceMiB: 2048, Leases: []takeBackLease{
			{ID: "b-1", MemoryMiB: 2048, LastActive: time.Unix(1_000_001, 0)},
			{ID: "b-2", MemoryMiB: 2048, Pinned: true, LastActive: time.Unix(1_000_000, 0)},
		}},
	}
	err := biggestBorrowerRefusal(owners, "req", 1024)
	if err == nil {
		t.Fatal("refusal = nil, want box_full")
	}
	var bf *memBoxFullError
	if !errors.As(err, &bf) {
		t.Fatalf("refusal %v is not a *memBoxFullError", err)
	}
	got := make([]string, 0, len(bf.Leases))
	for _, l := range bf.Leases {
		got = append(got, l.ID)
		if strings.HasPrefix(l.ID, "b-") {
			t.Fatalf("own_leases lists another owner's lease %s", l.ID)
		}
	}
	want := []string{"r-old", "r-busy", "r-new"} // LRU first
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("own_leases order = %v, want %v (LRU first)", got, want)
		}
	}
	if !bf.Leases[0].Pinned {
		t.Fatal("the pinned lease's flag was lost")
	}
	if !bf.Leases[1].Busy {
		t.Fatal("the busy lease's flag was lost")
	}
	if bf.Leases[2].Name != "fresh" {
		t.Fatalf("the lease name was lost: %q", bf.Leases[2].Name)
	}
	if bf.Leases[0].MemoryMB != 3072 || bf.Leases[2].MemoryMB != 1024 {
		t.Fatalf("memory_mb lost: %+v", bf.Leases)
	}
	raw, _ := json.Marshal(bf.Leases)
	if strings.Contains(string(raw), `"b-1"`) || strings.Contains(string(raw), `"bob"`) {
		t.Fatalf("the refusal JSON leaks another owner's rows: %s", raw)
	}
}

// TestOwnLeasesLRUOrder: the helper alone — LRU first, ties by ID, and a
// copy (the caller's view is never re-ordered).
func TestOwnLeasesLRUOrder(t *testing.T) {
	ls := []takeBackLease{
		{ID: "z", MemoryMiB: 512, LastActive: time.Unix(500, 0)},
		{ID: "a", MemoryMiB: 512, LastActive: time.Unix(500, 0)}, // ties z, ID wins
		{ID: "m", MemoryMiB: 512, Pinned: true, LastActive: time.Unix(100, 0)},
		{ID: "q", MemoryMiB: 512, Busy: true, LastActive: time.Unix(900, 0)},
	}
	out := ownLeasesLRU(ls)
	var got []string
	for _, l := range out {
		got = append(got, l.ID)
	}
	want := []string{"m", "a", "z", "q"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	if ls[0].ID != "z" || ls[2].ID != "m" {
		t.Fatal("ownLeasesLRU re-ordered the caller's slice")
	}
}

// TestBoxFullPinnedNoOwnLeasesKey: the disk/pinned box_full (the bare
// errBoxFull, which carries no leases) answers over HTTP without the
// own_leases key.
func TestBoxFullPinnedNoOwnLeasesKey(t *testing.T) {
	ts, svc, sub, ctx := newPreemptServer(t)
	installDynamicNode(t, svc, sub, 512, 0, 512)
	pinned, err := svc.grantLease(ctx, leaseRequest{owner: "consumer-b", image: "mid", ttl: time.Hour})
	if err != nil {
		t.Fatalf("grant pinned: %v", err)
	}
	if _, err := svc.setPinned("consumer-b", pinned.ID, true); err != nil {
		t.Fatalf("pin: %v", err)
	}
	warmFairShares(svc, ctx)
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "mid", "ttl": 3600})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("pinned box_full create = %d (%v), want 429", resp.StatusCode, body)
	}
	if body["code"] != "box_full" {
		t.Fatalf("code = %v, want box_full", body["code"])
	}
	if _, present := body["own_leases"]; present {
		t.Fatalf("a pinned box_full body must not carry own_leases: %v", body)
	}
	if pinned.Suspended {
		t.Fatal("the pinned lease was paused")
	}
}

// TestBoxFullLeaselessErrorHasNoKey: a plain errBoxFull never decodes as
// the memory refusal, and a leaseless memBoxFullError carries no leases
// — the shapes behind the pinned/disk box_full bodies.
func TestBoxFullLeaselessErrorHasNoKey(t *testing.T) {
	var bf *memBoxFullError
	if errors.As(errBoxFull, &bf) {
		t.Fatal("bare errBoxFull must not decode as a *memBoxFullError")
	}
	wrapped := &memBoxFullError{Owner: "req", UsedMiB: 4096, SliceMiB: 2048, RequestMiB: 1024}
	if !errors.As(fmt.Errorf("wrapped: %w", wrapped), &bf) || len(bf.Leases) != 0 {
		t.Fatalf("wrapped refusal not decoded or leases non-empty: %v", bf)
	}
}

// TestBoxFullOwnLeasesBodyShape pins the exact JSON keys one own_leases
// row carries, so a renamed field fails here and not in a client.
func TestBoxFullOwnLeasesBodyShape(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	row := ownLeasesLRU([]takeBackLease{
		{ID: "r1", Name: "tagged", MemoryMiB: 2048, Pinned: true, Busy: true, LastActive: now},
	})[0]
	raw, err := json.Marshal(map[string]any{"own_leases": []BoxFullLease{row}})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		OwnLeases []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			MemoryMB   int    `json:"memory_mb"`
			LastActive string `json:"last_active"`
			Pinned     bool   `json:"pinned"`
			Busy       bool   `json:"busy"`
		} `json:"own_leases"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	if len(decoded.OwnLeases) != 1 {
		t.Fatalf("%d rows: %s", len(decoded.OwnLeases), raw)
	}
	r := decoded.OwnLeases[0]
	if r.ID != "r1" || r.Name != "tagged" || r.MemoryMB != 2048 || !r.Pinned || !r.Busy {
		t.Fatalf("row fields lost: %s", raw)
	}
	if r.LastActive != "2023-11-14T22:13:20Z" {
		t.Fatalf("last_active = %q, want the RFC3339 UTC stamp", r.LastActive)
	}
	// An untagged lease carries no name key.
	row2 := ownLeasesLRU([]takeBackLease{{ID: "r2", MemoryMiB: 1, LastActive: now}})[0]
	raw2, _ := json.Marshal(row2)
	if strings.Contains(string(raw2), `"name"`) {
		t.Fatalf("an untagged lease must omit name: %s", raw2)
	}
}
