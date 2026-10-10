package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jrimmer/spoond/v2/identity"
	"github.com/jrimmer/spoond/v2/substrate/fake"
)

// sub2 digs the fake substrate out of the service for call counting:
// NewServerWithService keeps svc.sub unexported, but the test package
// is api itself, so the field is reachable through the wrapper.
func sub2(svc *Service) *fake.Fake {
	return svc.sub.(*testSub).Fake
}

// TestKeptCapFifthKeepConflicts: the 5th keep on one lease is 409 with
// the cap message and takes no checkpoint at all (#126). The first four
// keeps stand untouched — nothing is evicted.
func TestKeptCapFifthKeepConflicts(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.MaxKeptPerLease = 4
	seedImage(t, db, "py-base", 2048)

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	id := create["id"].(string)
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "token-a",
			map[string]any{"keep": true})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("keep %d: status %d: %v, want 200", i+1, resp.StatusCode, body)
		}
	}
	keeps, err := db.ListKeptBuilds(ctx)
	if err != nil || len(keeps[id]) != 4 {
		t.Fatalf("kept rows after 4 keeps = %v (%v), want 4", keeps, err)
	}

	// The 5th keep is over the cap: 409 naming the cap and the unpin
	// route, and no checkpoint is taken — the fake's checkpoint count
	// and the kept rows both stand still.
	callsBefore := calls(sub2(svc), "Checkpoint")
	resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "token-a",
		map[string]any{"keep": true})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("5th keep: status %d (%v), want 409", resp.StatusCode, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "kept checkpoint limit reached (4 per lease)") ||
		!strings.Contains(msg, "DELETE /api/snapshots/{build_id}") {
		t.Fatalf("409 body = %v, want the cap message with the unpin hint", body)
	}
	if got := calls(sub2(svc), "Checkpoint"); got != callsBefore {
		t.Fatalf("rejected keep took %d checkpoint(s), want 0", got-callsBefore)
	}
	keeps, err = db.ListKeptBuilds(ctx)
	if err != nil || len(keeps[id]) != 4 {
		t.Fatalf("kept rows after the rejected keep = %v (%v), want still 4 (nothing evicted)", keeps, err)
	}

	// A plain checkpoint (no keep) still works at the cap: the cap only
	// governs keeps.
	if resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "token-a", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("plain checkpoint at the cap: status %d (%v), want 200", resp.StatusCode, body)
	}

	// MAX_KEPT_PER_LEASE=0 disables the cap: the 5th keep lands.
	svc.cfg.MaxKeptPerLease = 0
	resp, body = doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "token-a",
		map[string]any{"keep": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("keep with the cap off: status %d (%v), want 200", resp.StatusCode, body)
	}
	keeps, err = db.ListKeptBuilds(ctx)
	if err != nil || len(keeps[id]) != 5 {
		t.Fatalf("kept rows with the cap off = %v (%v), want 5", keeps, err)
	}
}

// TestKeptCapUnpinFreesSlot: unpinning one kept build
// (DELETE /api/snapshots/{build_id}) frees a slot, so the next keep
// succeeds again (#126).
func TestKeptCapUnpinFreesSlot(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.MaxKeptPerLease = 2
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	roomyDisk(svc) // the create and every keep must fit (FS3b)

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	id := create["id"].(string)

	var builds []string
	for i := 0; i < 2; i++ {
		resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "token-a",
			map[string]any{"keep": true})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("keep %d: status %d: %v", i+1, resp.StatusCode, body)
		}
		builds = append(builds, body["build_id"].(string))
	}
	if resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "token-a",
		map[string]any{"keep": true}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("3rd keep: status %d (%v), want 409", resp.StatusCode, body)
	}

	// Unpin one: the delete route drops the kept row before its kept-set
	// check, so the DELETE frees the slot even though build[0] itself is
	// still in use (it is an ancestor of the lease's live build, which
	// no delete may remove while the lease runs) — the route answers 409
	// for the build, but the pin is gone.
	if resp, body := doReq(t, "DELETE", ts.URL+"/api/snapshots/"+builds[0], "token-a", nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("unpin %s: status %d (%v), want 409 (the build is the live build's ancestor)", builds[0], resp.StatusCode, body)
	}
	keeps, err := db.ListKeptBuilds(context.Background())
	if err != nil || len(keeps[id]) != 1 {
		t.Fatalf("kept rows after the unpin = %v (%v), want 1 (the pin is dropped)", keeps, err)
	}
	resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "token-a",
		map[string]any{"keep": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("keep after the unpin: status %d (%v), want 200", resp.StatusCode, body)
	}
	keeps, err = db.ListKeptBuilds(context.Background())
	if err != nil || len(keeps[id]) != 2 {
		t.Fatalf("kept rows after the retry keep = %v (%v), want 2", keeps, err)
	}
}

// keptBudgetSetup gives the box a fixed, roomy snapshot volume and
// returns a function that reads owner's fair DISK slice (the kept
// budget since 3.0, FS4). It fails the test when the slice is not known
// (capacity unreadable) or the box has fewer than two owners: the
// budget tests size builds against the slice, and the slice grows by
// size/N as kept bytes are accounted, so N >= 2 keeps a 0.7-slice
// build's second keep over it.
func keptBudgetSetup(t *testing.T, svc *Service) func(owner string) int64 {
	t.Helper()
	roomyDisk(svc)
	return func(owner string) int64 {
		t.Helper()
		svc.updateNodeMetrics(context.Background()) // warm the node-info cache the view reads
		svc.invalidateFairShares()
		share, ok := svc.fairShareFor(context.Background(), owner)
		if !ok || !share.CapacityKnown || share.Disk.SliceBytes <= 0 {
			t.Fatalf("fair share of %s = %+v (ok %v), want a known disk slice", owner, share, ok)
		}
		if n := len(svc.ownersOfBox()); n < 2 {
			t.Fatalf("box has %d owner(s), want at least 2", n)
		}
		return share.Disk.SliceBytes
	}
}

// buildsMeasure makes every new build measure size bytes at write time.
func buildsMeasure(svc *Service, size int64) {
	svc.diskUsage = func(string) (int64, error) { return size, nil }
}

// TestKeptBudgetOverBudgetKeepNotPinned: a keep whose fresh build would
// push the owner's kept bytes past their fair disk slice is 409, the
// build is NOT pinned (it stays an ordinary, GC-able checkpoint) and the
// body names its build_id for a retry without keep (#126, FS4).
func TestKeptBudgetOverBudgetKeepNotPinned(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	ids, err := identity.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	svc.SetIdentities(ids)
	u, err := ids.AddUser("keeper", identity.KindPerson, []string{"SHA256:fp-k"}, "keeper-tok")
	if err != nil {
		t.Fatal(err)
	}
	sliceOf := keptBudgetSetup(t, svc)

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "keeper-tok",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	id := create["id"].(string)
	ctx := context.Background()

	// Each build is 70% of the slice: one fits, two do not.
	buildsMeasure(svc, sliceOf(u.ID)*7/10)

	// First keep lands: the fresh build fits inside the slice.
	resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "keeper-tok",
		map[string]any{"keep": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first keep: status %d (%v), want 200", resp.StatusCode, body)
	}
	first := body["build_id"].(string)
	keeps, err := db.ListKeptBuilds(ctx)
	if err != nil || len(keeps[id]) != 1 || keeps[id][0] != first {
		t.Fatalf("kept rows = %v (%v), want [%s]", keeps, err, first)
	}

	// Second keep would pass the slice: 409 naming the budget, with the
	// fresh build's id in the body and kept=false.
	resp, body = doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "keeper-tok",
		map[string]any{"keep": true})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("over-budget keep: status %d (%v), want 409", resp.StatusCode, body)
	}
	second, _ := body["build_id"].(string)
	if second == "" || second == first {
		t.Fatalf("409 body build_id = %q, want the fresh build", body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "byte budget") {
		t.Fatalf("409 error = %q, want the budget message", msg)
	}
	if kept, _ := body["kept"].(bool); kept {
		t.Fatalf("409 body kept = %v, want false", body)
	}
	// The fresh build was written (the checkpoint stands) but is not
	// pinned: only the first build is kept.
	keeps, err = db.ListKeptBuilds(ctx)
	if err != nil || len(keeps[id]) != 1 || keeps[id][0] != first {
		t.Fatalf("kept rows after the over-budget keep = %v (%v), want still [%s]", keeps, err, first)
	}
	b, err := db.GetBuild(ctx, second)
	if err != nil || b.State != "ready" {
		t.Fatalf("over-budget build %s state = %v (%v), want ready (written, unpinned)", second, b, err)
	}
	kept, err := svc.keptBuilds(ctx)
	if err != nil {
		t.Fatalf("keptBuilds: %v", err)
	}
	if !kept[first] {
		t.Fatalf("first build %s not kept after the over-budget keep", first)
	}
}

// TestKeptBudgetInsideSliceKeeps: keeps whose total stays inside the
// owner's fair disk slice are all pinned.
func TestKeptBudgetInsideSliceKeeps(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	ids, err := identity.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	svc.SetIdentities(ids)
	u, err := ids.AddUser("keeper", identity.KindPerson, []string{"SHA256:fp-k"}, "keeper-tok")
	if err != nil {
		t.Fatal(err)
	}
	sliceOf := keptBudgetSetup(t, svc)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "keeper-tok",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	id := create["id"].(string)

	// Three builds of 30% each: 90% of the slice, all inside it.
	buildsMeasure(svc, sliceOf(u.ID)*3/10)
	for i := 0; i < 3; i++ {
		resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "keeper-tok",
			map[string]any{"keep": true})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("keep %d inside the slice: status %d (%v), want 200", i, resp.StatusCode, body)
		}
	}
	keeps, err := db.ListKeptBuilds(context.Background())
	if err != nil || len(keeps[id]) != 3 {
		t.Fatalf("kept rows = %v (%v), want 3", keeps, err)
	}
}

// TestKeptBudgetUnknownCapacityNoBudget: while the box capacity cannot
// be read the fair slice is unknown, and keptBudgetFor answers 0 (no
// budget) rather than refusing every keep on a wrong number.
func TestKeptBudgetUnknownCapacityNoBudget(t *testing.T) {
	svc, db, _ := newTestService(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	seedImage(t, db, "py-base", 2048)
	ids, _ := identity.NewStore("")
	svc.SetIdentities(ids)
	u, err := ids.AddUser("keeper", identity.KindPerson, nil, "keeper-tok")
	if err != nil {
		t.Fatal(err)
	}
	svc.diskCapacity = func(string) (uint64, uint64, error) { return 0, 0, errors.New("statfs: no") }
	svc.invalidateFairShares()
	if got := svc.keptBudgetFor(context.Background(), u.ID); got != 0 {
		t.Fatalf("keptBudgetFor with unknown capacity = %d, want 0 (no budget)", got)
	}
	// A known capacity gives the slice.
	roomyDisk(svc)
	svc.updateNodeMetrics(context.Background())
	svc.invalidateFairShares()
	share, ok := svc.fairShareFor(context.Background(), u.ID)
	if !ok || !share.CapacityKnown || share.Disk.SliceBytes <= 0 {
		t.Fatalf("fair share = %+v (ok %v), want a known disk slice", share, ok)
	}
	if got := svc.keptBudgetFor(context.Background(), u.ID); got != share.Disk.SliceBytes {
		t.Fatalf("keptBudgetFor = %d, want the disk slice %d", got, share.Disk.SliceBytes)
	}
}

// TestKeptBudgetQuotaFieldRoundTrip: max_kept_bytes rides the quota
// route and the user views (#126).
func TestKeptBudgetQuotaFieldRoundTrip(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ids, _ := identity.NewStore("")
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(db))
	h := srv.Handler()

	rec, _ := doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"admin","fingerprints":["SHA256:fp-a"],"token":"admin-tok"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap: %d", rec.Code)
	}
	rec, body := doUsersReq(t, h, "POST", "/api/users", "admin-tok", `{"name":"bob","fingerprints":["SHA256:fp-b"],"token":"bob-tok"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	uid := body["user"].(map[string]any)["id"].(string)

	rec, body = doUsersReq(t, h, "POST", "/api/users/"+uid+"/quota", "admin-tok",
		`{"max_leases":3,"max_ttl":600,"max_kept_bytes":1073741824}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("quota: %d %s", rec.Code, rec.Body.String())
	}
	u := body["user"].(map[string]any)
	if u["max_kept_bytes"] != float64(1073741824) {
		t.Fatalf("user view max_kept_bytes = %v, want 1073741824", u["max_kept_bytes"])
	}

	// A negative budget is 400.
	rec, _ = doUsersReq(t, h, "POST", "/api/users/"+uid+"/quota", "admin-tok",
		`{"max_kept_bytes":-1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative budget: %d, want 400", rec.Code)
	}
	// Non-admin callers cannot set it.
	rec, _ = doUsersReq(t, h, "POST", "/api/users/"+uid+"/quota", "bob-tok", `{"max_kept_bytes":5}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin quota: %d, want 403", rec.Code)
	}
}
