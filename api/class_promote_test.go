package api

import (
	"net/http"
	"testing"
	"time"
)

// classOf reads a lease's class through the API.
func classOf(t *testing.T, srv *Server, uid, id string) string {
	t.Helper()
	l := srv.svc.lookup(uid, id)
	if l == nil {
		t.Fatalf("lease %s not found", id)
	}
	srv.svc.store.mu.Lock()
	defer srv.svc.store.mu.Unlock()
	return l.Class
}

// TestBurstPromotedWhenGuaranteeFrees: with a 2 GiB guarantee and three
// 1 GiB leases the third is burst; releasing a guaranteed lease
// promotes it (event "promoted"), so the guarantee stays filled as
// leases churn, instead of every survivor drifting to burst.
func TestBurstPromotedWhenGuaranteeFrees(t *testing.T) {
	t.Skip("TODO(FS2b-1 step 3): class behaviour removed")
	srv, h, _, tok, uid := newClassServer(t, map[string]int{"mid": 1024}, `{"guaranteed_mib":2048,"max_mib":8192}`)
	svc := srv.svc
	var ids []string
	for i := 0; i < 3; i++ {
		rec, m := createSandboxBody(t, h, tok, `{"image":"mid","ttl":600}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %d: %d %s", i, rec.Code, rec.Body.String())
		}
		ids = append(ids, m["id"].(string))
	}
	want := []string{ClassGuaranteed, ClassGuaranteed, ClassBurst}
	for i, id := range ids {
		if got := classOf(t, srv, uid, id); got != want[i] {
			t.Fatalf("lease %d class = %s, want %s", i, got, want[i])
		}
	}

	events := svc.Subscribe(EventFilter{LeaseID: ids[2]})
	defer events.Close()
	svc.releaseBecause(t.Context(), svc.lookup(uid, ids[0]), "test")
	if got := classOf(t, srv, uid, ids[2]); got != ClassGuaranteed {
		t.Fatalf("burst lease after a guaranteed one left = %s, want promoted to guaranteed", got)
	}
	deadline := time.After(2 * time.Second)
	for got := false; !got; {
		select {
		case ev := <-events.C:
			got = ev.Type == LeasePromoted
		case <-deadline:
			t.Fatal("no promoted event")
		}
	}
	// The guarantee is full again: the next lease is burst.
	rec, m := createSandboxBody(t, h, tok, `{"image":"mid","ttl":600}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create 4: %d", rec.Code)
	}
	if got := classOf(t, srv, uid, m["id"].(string)); got != ClassBurst {
		t.Fatalf("lease past a full guarantee = %s, want burst", got)
	}
}

// TestBurstLeasesDoNotCountAgainstGuarantee: an owner whose running
// leases are all burst still gets a guaranteed lease while its
// guarantee has room (the old rule compared total usage and made every
// new lease burst).
func TestBurstLeasesDoNotCountAgainstGuarantee(t *testing.T) {
	t.Skip("TODO(FS2b-1 step 3): class behaviour removed")
	srv, h, _, tok, uid := newClassServer(t, map[string]int{"mid": 1024}, `{"guaranteed_mib":2048,"max_mib":8192}`)
	for i := 0; i < 3; i++ {
		if rec, _ := createSandboxBody(t, h, tok, `{"image":"mid","ttl":600,"burst":true}`); rec.Code != http.StatusCreated {
			t.Fatalf("burst create %d: %d", i, rec.Code)
		}
	}
	rec, m := createSandboxBody(t, h, tok, `{"image":"mid","ttl":600}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d", rec.Code)
	}
	if got := classOf(t, srv, uid, m["id"].(string)); got != ClassGuaranteed {
		t.Fatalf("lease with an empty guarantee beside burst leases = %s, want guaranteed", got)
	}
}

// TestRequestedBurstNeverPromoted: a lease created with "burst": true
// stays burst even when the guarantee has room.
func TestRequestedBurstNeverPromoted(t *testing.T) {
	t.Skip("TODO(FS2b-1 step 3): class behaviour removed")
	srv, h, _, tok, uid := newClassServer(t, map[string]int{"mid": 1024}, `{"guaranteed_mib":4096,"max_mib":8192}`)
	rec, m := createSandboxBody(t, h, tok, `{"image":"mid","ttl":600,"burst":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d", rec.Code)
	}
	srv.svc.promoteAllBurst()
	if got := classOf(t, srv, uid, m["id"].(string)); got != ClassBurst {
		t.Fatalf("requested burst lease = %s after promotion, want burst", got)
	}
}
