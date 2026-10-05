package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/identity"
)

// newMemQuotaServer bootstraps admin + a quota'd user over images of
// the given sizes (name -> memory_mb). Returns the server, its handler,
// the user's token and its id.
func newMemQuotaServer(t *testing.T, images map[string]int, quota string) (*Server, http.Handler, string, string) {
	t.Helper()
	svc, db, _ := newTestService(t)
	for name, mb := range images {
		seedImage(t, db, name, mb)
	}
	ids, _ := identity.NewStore("")
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(db))
	h := srv.Handler()

	if rec, _ := doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"admin","fingerprints":["SHA256:fp-a"],"token":"admin-tok"}`); rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap admin: %d", rec.Code)
	}
	rec, body := doUsersReq(t, h, "POST", "/api/users", "admin-tok", `{"name":"worker","fingerprints":["SHA256:fp-b"],"token":"work-tok"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create user: %d", rec.Code)
	}
	uid := body["user"].(map[string]any)["id"].(string)
	if rec2, _ := doUsersReq(t, h, "POST", "/api/users/"+uid+"/quota", "admin-tok", quota); rec2.Code != http.StatusOK {
		t.Fatalf("set quota: %d %s", rec2.Code, rec2.Body.String())
	}
	return srv, h, "work-tok", uid
}

func createSandboxAs(t *testing.T, h http.Handler, token, image string) (*httptest.ResponseRecorder, map[string]any) {
	return createSandboxBody(t, h, token, `{"image":"`+image+`","ttl":60}`)
}

// createPersistentAs creates a persistent lease (suspendable).
func createPersistentAs(t *testing.T, h http.Handler, token, image string) (*httptest.ResponseRecorder, map[string]any) {
	return createSandboxBody(t, h, token, `{"image":"`+image+`","ttl":60,"persistent":true}`)
}

func createSandboxBody(t *testing.T, h http.Handler, token, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/sandboxes", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var parsed map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
	return rec, parsed
}

// postLeaseAction POSTs to a lease action route (resume, restart,
// restore) as the token and returns the recorder.
func postLeaseAction(t *testing.T, h http.Handler, token, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestMemQuota429OverMaxMiB: a 4096 + 4096 pair over a max_mib of 6144
// admits the first lease and answers 429 naming the memory limit for
// the second.
func TestMemQuota429OverMaxMiB(t *testing.T) {
	_, h, tok, _ := newMemQuotaServer(t, map[string]int{"big": 4096}, `{"max_mib":6144}`)

	rec, _ := createSandboxAs(t, h, tok, "big")
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create: %d %s", rec.Code, rec.Body.String())
	}
	rec, _ = createSandboxAs(t, h, tok, "big")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second create = %d, want 429", rec.Code)
	}
	if msg := rec.Body.String(); !strings.Contains(msg, "6144") || !strings.Contains(msg, "memory") {
		t.Fatalf("429 body should name the memory limit, got %s", msg)
	}
	// users/me agrees on the charge: one 4096 lease.
	rec, body := doUsersReq(t, h, "GET", "/api/users/me", tok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("users/me: %d", rec.Code)
	}
	u := body["user"].(map[string]any)
	if got := int(u["used_mib"].(float64)); got != 4096 {
		t.Fatalf("used_mib = %d, want 4096", got)
	}
	if got := int(u["max_mib"].(float64)); got != 6144 {
		t.Fatalf("max_mib = %d, want 6144", got)
	}
	if _, ok := u["guaranteed_mib"]; !ok {
		t.Fatal("users/me should carry guaranteed_mib")
	}
}

// TestMemQuotaExactFitRace: two racing creates where the cap exactly
// fits one lease (2048 image over max_mib 2048 — the admitted lease
// fills the cap to the byte) must admit exactly one under concurrency —
// a reservation in flight must never be counted twice (which would
// refuse the second create even though only one landed).
func TestMemQuotaExactFitRace(t *testing.T) {
	_, h, tok, _ := newMemQuotaServer(t, map[string]int{"mid": 2048}, `{"max_mib":2048}`)

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec, _ := createSandboxAs(t, h, tok, "mid")
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()
	ok, refused := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			ok++
		case http.StatusTooManyRequests:
			refused++
		}
	}
	if ok != 1 || refused != 1 {
		t.Fatalf("exact-fit racing creates: %d created, %d refused, want one of each (codes=%v)", ok, refused, codes)
	}
	// The single grant's charge is the whole story: used_mib is 2048,
	// and the reservation left no residue.
	rec, body := doUsersReq(t, h, "GET", "/api/users/me", tok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("users/me: %d", rec.Code)
	}
	if got := int(body["user"].(map[string]any)["used_mib"].(float64)); got != 2048 {
		t.Fatalf("used_mib after the race = %d, want 2048", got)
	}
}

// TestMemQuotaSuspendedNotCharged: a suspended lease holds no
// hugepages, so its MiB is freed and another create fits.
func TestMemQuotaSuspendedNotCharged(t *testing.T) {
	srv, h, tok, _ := newMemQuotaServer(t, map[string]int{"big": 4096}, `{"max_mib":6144}`)
	svc := srv.svc
	owner := ownerIDFor(t, h, tok)

	rec, first := createPersistentAs(t, h, tok, "big")
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := svc.suspend(context.Background(), owner, first["id"].(string)); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	// 4096 charged before; 0 now — the second create fits under 6144.
	rec, _ = createSandboxAs(t, h, tok, "big")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create after suspend: %d %s", rec.Code, rec.Body.String())
	}
}

// TestMemQuotaResumeChecked: resuming a suspended lease re-passes the
// memory check — when the budget is already spent elsewhere, the resume
// answers 429 and the lease stays suspended.
func TestMemQuotaResumeChecked(t *testing.T) {
	srv, h, tok, uid := newMemQuotaServer(t, map[string]int{"big": 4096}, `{"max_mib":4096}`)
	svc := srv.svc
	owner := ownerIDFor(t, h, tok)

	rec, first := createPersistentAs(t, h, tok, "big")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	id := first["id"].(string)
	if _, err := svc.suspend(context.Background(), owner, id); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	// Spend the budget on a second lease while the first is suspended.
	rec, _ = createSandboxAs(t, h, tok, "big")
	if rec.Code != http.StatusCreated {
		t.Fatalf("second create: %d %s", rec.Code, rec.Body.String())
	}
	// Resuming the first would push the charge to 8192 over a 4096 cap.
	rec2 := postLeaseAction(t, h, tok, "/api/sandboxes/"+id+"/resume", "")
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("resume = %d, want 429 (%s)", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "memory") {
		t.Fatalf("resume 429 should name the memory limit, got %s", rec2.Body.String())
	}
	// The lease stays suspended.
	l := svc.lookup(uid, id)
	if l == nil || !l.Suspended {
		t.Fatal("refused resume must leave the lease suspended")
	}
}

// TestMemQuotaResumeNotCountCapped: resuming your own suspended lease
// adds no lease, so a user at their max_leases cap can still resume it
// (the count cap stays for creates; memory is what resume re-checks).
func TestMemQuotaResumeNotCountCapped(t *testing.T) {
	srv, h, tok, uid := newMemQuotaServer(t, map[string]int{"big": 4096}, `{"max_leases":1}`)
	svc := srv.svc
	owner := ownerIDFor(t, h, tok)

	rec, first := createPersistentAs(t, h, tok, "big")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	id := first["id"].(string)
	if _, err := svc.suspend(context.Background(), owner, id); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	// The suspended lease still counts toward max_leases, so a create
	// is refused by the count cap…
	rec, _ = createSandboxAs(t, h, tok, "big")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("create at the count cap = %d, want 429", rec.Code)
	}
	// …but the resume of the user's own lease must not be.
	if rec := postLeaseAction(t, h, tok, "/api/sandboxes/"+id+"/resume", ""); rec.Code != http.StatusOK {
		t.Fatalf("resume at the count cap = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if l := svc.lookup(uid, id); l == nil || !l.live() {
		t.Fatal("resume should have brought the lease back running")
	}
}

// TestMemQuotaUndrainSurvivesCountCap: undrain resumes drained leases
// through the resume path, which now checks only memory — a user at
// their max_leases cap gets their drained leases back, not lost ones.
func TestMemQuotaUndrainSurvivesCountCap(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "big", 4096)
	ids, _ := identity.NewStore("")
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(db))
	srv.SetAdminToken("admin-tok")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	u, err := ids.AddUser("worker", identity.KindPerson, []string{"SHA256:fp-b"}, "work-tok")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := ids.SetQuota(u.ID, 1, 0, 0, 0, 0); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	ctx := context.Background()
	l, err := svc.grant(ctx, u.ID, "big", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	resp, body := doReq(t, "POST", ts.URL+"/api/admin/drain", "admin-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("drain = %d (%v), want 200", resp.StatusCode, body)
	}
	resp, body = doReq(t, "POST", ts.URL+"/api/admin/undrain", "admin-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("undrain = %d (%v), want 200", resp.StatusCode, body)
	}
	if body["resumed"] != float64(1) {
		t.Fatalf("undrain resumed = %v, want 1", body["resumed"])
	}
	if failed, ok := body["failed"].([]any); !ok || len(failed) != 0 {
		t.Fatalf("undrain failed = %v, want an empty list", body["failed"])
	}
	if got := l.State; got != "running" {
		t.Fatalf("drained lease after undrain = %s, want running (not lost)", got)
	}
	if ids := sub.sandboxesLive(t); len(ids) != 1 {
		t.Fatalf("undrained lease's sandbox should be live, got %v", ids)
	}
}

// TestMemQuotaRestartColdChecked: a cold restart of a suspended lease
// brings a fresh running guest back, so it re-passes the memory check —
// over max_mib it answers 429 and the lease stays suspended, unchanged.
func TestMemQuotaRestartColdChecked(t *testing.T) {
	srv, h, tok, uid := newMemQuotaServer(t, map[string]int{"big": 4096}, `{"max_mib":4096}`)
	svc := srv.svc
	owner := ownerIDFor(t, h, tok)

	rec, first := createPersistentAs(t, h, tok, "big")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	id := first["id"].(string)
	sandbox := svc.lookup(uid, id).SandboxID
	if _, err := svc.suspend(context.Background(), owner, id); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	// Spend the budget while the lease is suspended.
	rec, _ = createSandboxAs(t, h, tok, "big")
	if rec.Code != http.StatusCreated {
		t.Fatalf("second create: %d %s", rec.Code, rec.Body.String())
	}
	rec = postLeaseAction(t, h, tok, "/api/sandboxes/"+id+"/restart?mode=cold", "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("cold restart = %d %s, want 429", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "memory") {
		t.Fatalf("cold restart 429 should name the memory limit, got %s", rec.Body.String())
	}
	l := svc.lookup(uid, id)
	if l == nil || !l.Suspended || l.SandboxID != sandbox {
		t.Fatal("refused cold restart must leave the lease suspended, untouched")
	}
}

// TestMemQuotaRestartWarmChecked: a warm restart of a suspended lease
// resumes it, so it re-passes the memory check too.
func TestMemQuotaRestartWarmChecked(t *testing.T) {
	srv, h, tok, uid := newMemQuotaServer(t, map[string]int{"big": 4096}, `{"max_mib":4096}`)
	svc := srv.svc
	owner := ownerIDFor(t, h, tok)

	rec, first := createPersistentAs(t, h, tok, "big")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	id := first["id"].(string)
	if _, err := svc.suspend(context.Background(), owner, id); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	rec, _ = createSandboxAs(t, h, tok, "big")
	if rec.Code != http.StatusCreated {
		t.Fatalf("second create: %d %s", rec.Code, rec.Body.String())
	}
	rec = postLeaseAction(t, h, tok, "/api/sandboxes/"+id+"/restart", "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("warm restart of a suspended lease = %d %s, want 429", rec.Code, rec.Body.String())
	}
	if l := svc.lookup(uid, id); l == nil || !l.Suspended {
		t.Fatal("refused warm restart must leave the lease suspended")
	}
}

// TestMemQuotaRestartFitsAfterSuspend: the suspend freed the charge, so
// restarting the suspended lease within the budget works — the check
// admits, it does not block suspended work.
func TestMemQuotaRestartFitsAfterSuspend(t *testing.T) {
	srv, h, tok, uid := newMemQuotaServer(t, map[string]int{"big": 4096}, `{"max_mib":4096}`)
	svc := srv.svc
	owner := ownerIDFor(t, h, tok)

	rec, first := createPersistentAs(t, h, tok, "big")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	id := first["id"].(string)
	if _, err := svc.suspend(context.Background(), owner, id); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if rec := postLeaseAction(t, h, tok, "/api/sandboxes/"+id+"/restart?mode=cold", ""); rec.Code != http.StatusOK {
		t.Fatalf("cold restart within the budget = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if l := svc.lookup(uid, id); l == nil || !l.live() {
		t.Fatal("cold restart should have brought the lease back running")
	}
}

// TestMemQuotaRestoreChecked: restoring a suspended lease in place
// brings a running sandbox back, so it re-passes the memory check.
func TestMemQuotaRestoreChecked(t *testing.T) {
	srv, h, tok, uid := newMemQuotaServer(t, map[string]int{"big": 4096}, `{"max_mib":4096}`)
	svc := srv.svc
	owner := ownerIDFor(t, h, tok)

	rec, first := createPersistentAs(t, h, tok, "big")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	id := first["id"].(string)
	l := svc.lookup(uid, id)
	b, err := svc.checkpointLease(context.Background(), l)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := svc.keepBuild(context.Background(), id, b.BuildID); err != nil {
		t.Fatalf("keep: %v", err)
	}
	if _, err := svc.suspend(context.Background(), owner, id); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	// Spend the budget while the lease is suspended.
	rec, _ = createSandboxAs(t, h, tok, "big")
	if rec.Code != http.StatusCreated {
		t.Fatalf("second create: %d %s", rec.Code, rec.Body.String())
	}
	rec = postLeaseAction(t, h, tok, "/api/sandboxes/"+id+"/restore", `{"build_id":"`+b.BuildID+`"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("restore of a suspended lease = %d %s, want 429", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "memory") {
		t.Fatalf("restore 429 should name the memory limit, got %s", rec.Body.String())
	}
	if l := svc.lookup(uid, id); l == nil || !l.Suspended {
		t.Fatal("refused restore must leave the lease suspended")
	}
}

// TestMemQuotaRecoveryChecked: the crash reconcile turns a suspended
// lease's sandbox back on, so it passes the memory check too.
func TestMemQuotaRecoveryChecked(t *testing.T) {
	srv, h, tok, uid := newMemQuotaServer(t, map[string]int{"big": 4096}, `{"max_mib":4096}`)
	svc := srv.svc
	owner := ownerIDFor(t, h, tok)

	rec, first := createPersistentAs(t, h, tok, "big")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	id := first["id"].(string)
	l := svc.lookup(uid, id)
	if _, err := svc.checkpointLease(context.Background(), l); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if _, err := svc.suspend(context.Background(), owner, id); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	// Spend the budget while the lease is suspended.
	rec, _ = createSandboxAs(t, h, tok, "big")
	if rec.Code != http.StatusCreated {
		t.Fatalf("second create: %d %s", rec.Code, rec.Body.String())
	}
	if err := svc.recoverFromCheckpoint(context.Background(), l); err == nil {
		t.Fatal("recovery of a suspended lease past the memory cap should fail")
	} else if !strings.Contains(err.Error(), "memory") {
		t.Fatalf("recovery error should name the memory limit, got %v", err)
	}
	if l := svc.lookup(uid, id); l == nil || !l.Suspended {
		t.Fatal("refused recovery must leave the lease suspended")
	}
}

// TestMemQuotaForkChargesCount: a fork reserves count times the image's
// memory_mb up front, all or nothing — 2048 (running source) + 2*2048
// children would be 6144 past a 4096 cap, so count 2 answers an error
// naming the memory limit and count 1 fits exactly.
func TestMemQuotaForkChargesCount(t *testing.T) {
	srv, _, _, uid := newMemQuotaServer(t, map[string]int{"mid": 2048}, `{"max_mib":4096}`)
	svc := srv.svc
	ctx := context.Background()

	src, err := svc.grant(ctx, uid, "mid", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant source: %v", err)
	}
	if _, _, err := svc.fork(ctx, uid, src.ID, 2, false, time.Minute, "", ""); err == nil {
		t.Fatal("fork past the memory cap should fail")
	} else if !strings.Contains(err.Error(), "memory") {
		t.Fatalf("fork error should name the memory limit, got %v", err)
	}
	if _, _, err := svc.fork(ctx, uid, src.ID, 1, false, time.Minute, "", ""); err != nil {
		t.Fatalf("fork of 1: %v", err)
	}
}

// TestMemQuotaReleaseFrees: deleting a lease frees its MiB.
func TestMemQuotaReleaseFrees(t *testing.T) {
	_, h, tok, _ := newMemQuotaServer(t, map[string]int{"big": 4096}, `{"max_mib":4096}`)

	rec, first := createSandboxAs(t, h, tok, "big")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	rec, _ = createSandboxAs(t, h, tok, "big")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second create = %d, want 429", rec.Code)
	}
	del := httptest.NewRecorder()
	dreq := httptest.NewRequest("DELETE", "/api/sandboxes/"+first["id"].(string), nil)
	dreq.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(del, dreq)
	if del.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", del.Code)
	}
	if rec, _ = createSandboxAs(t, h, tok, "big"); rec.Code != http.StatusCreated {
		t.Fatalf("create after release: %d %s", rec.Code, rec.Body.String())
	}
}

// TestMemQuotaLeaseDetailShowsCharge: GET /api/sandboxes/{id} carries
// the owner's charged_mib/guaranteed_mib/max_mib.
func TestMemQuotaLeaseDetailShowsCharge(t *testing.T) {
	_, h, tok, _ := newMemQuotaServer(t, map[string]int{"big": 4096}, `{"guaranteed_mib":1024,"max_mib":6144}`)

	rec, first := createSandboxAs(t, h, tok, "big")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest("GET", "/api/sandboxes/"+first["id"].(string), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	det := httptest.NewRecorder()
	h.ServeHTTP(det, req)
	if det.Code != http.StatusOK {
		t.Fatalf("detail: %d", det.Code)
	}
	var detail map[string]any
	if err := json.Unmarshal(det.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if got := int(detail["charged_mib"].(float64)); got != 4096 {
		t.Fatalf("charged_mib = %v, want 4096", detail["charged_mib"])
	}
	if got := int(detail["guaranteed_mib"].(float64)); got != 1024 {
		t.Fatalf("guaranteed_mib = %v, want 1024", detail["guaranteed_mib"])
	}
	if got := int(detail["max_mib"].(float64)); got != 6144 {
		t.Fatalf("max_mib = %v, want 6144", detail["max_mib"])
	}
}

// TestQuotaMemValidation: the quota endpoint validates the memory
// fields.
func TestQuotaMemValidation(t *testing.T) {
	_, h, tok, uid := newMemQuotaServer(t, map[string]int{"big": 4096}, `{"max_mib":6144}`)

	// guaranteed_mib > max_mib must be refused.
	rec, _ := doUsersReq(t, h, "POST", "/api/users/"+uid+"/quota", "admin-tok", `{"guaranteed_mib":8192,"max_mib":6144}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("guaranteed_mib > max_mib = %d, want 400", rec.Code)
	}
	// Negative values must be refused.
	rec, _ = doUsersReq(t, h, "POST", "/api/users/"+uid+"/quota", "admin-tok", `{"max_mib":-1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative max_mib = %d, want 400", rec.Code)
	}
	// Non-admin callers may not set memory quotas.
	rec, _ = doUsersReq(t, h, "POST", "/api/users/"+uid+"/quota", tok, `{"max_mib":100}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin quota set = %d, want 403", rec.Code)
	}
	// Legacy consumers are uncapped regardless of quotas on any row.
	rec, _ = createSandboxAs(t, h, "legacy-tok", "big")
	if rec.Code != http.StatusCreated {
		t.Fatalf("legacy create: %d %s", rec.Code, rec.Body.String())
	}
}

// TestMemQuotaLegacyRowsUnchanged (migration): a user with max_leases >
// 0 and no max_mib keeps working unchanged — the lease count caps as
// before, memory never blocks, and nothing converts max_leases into a
// memory limit.
func TestMemQuotaLegacyRowsUnchanged(t *testing.T) {
	// Two leases of 4096 each pass a count cap of 2 with no max_mib.
	_, h, tok, _ := newMemQuotaServer(t, map[string]int{"big": 4096}, `{"max_leases":2}`)
	for i := 0; i < 2; i++ {
		if rec, _ := createSandboxAs(t, h, tok, "big"); rec.Code != http.StatusCreated {
			t.Fatalf("create %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	rec, body := doUsersReq(t, h, "GET", "/api/users/me", tok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("users/me: %d", rec.Code)
	}
	u := body["user"].(map[string]any)
	if got := int(u["max_leases"].(float64)); got != 2 {
		t.Fatalf("max_leases = %v, want 2", u["max_leases"])
	}
	if got := int(u["max_mib"].(float64)); got != 0 {
		t.Fatalf("max_mib = %v, want 0 (no automatic conversion)", u["max_mib"])
	}
	if got := int(u["used_mib"].(float64)); got != 8192 {
		t.Fatalf("used_mib = %v, want 8192", u["used_mib"])
	}
	// Third create is refused by the count cap, not memory.
	rec, _ = createSandboxAs(t, h, tok, "big")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third create = %d, want 429", rec.Code)
	}
}

// TestMemQuotaPreQuotaLeaseResume: a lease persisted before the charge
// stamp existed (memory_mb 0 in the store) resumes without a memory
// charge — visible to the operator in the log, never blocked on a
// missing stamp.
func TestMemQuotaPreQuotaLeaseResume(t *testing.T) {
	srv, h, tok, uid := newMemQuotaServer(t, map[string]int{"big": 4096}, `{"max_mib":4096}`)
	svc := srv.svc
	owner := ownerIDFor(t, h, tok)

	rec, first := createPersistentAs(t, h, tok, "big")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	id := first["id"].(string)
	l := svc.lookup(uid, id)
	if _, err := svc.suspend(context.Background(), owner, id); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	// Simulate a row written before the stamp: memory_mb back to 0.
	l.MemoryMB = 0
	svc.store.mu.Lock()
	svc.saveLeaseLocked(l)
	svc.store.mu.Unlock()

	// Resume is admitted (the uncharged lease logs and passes), and the
	// row keeps its 0 until the lease is rebuilt.
	if rec := postLeaseAction(t, h, tok, "/api/sandboxes/"+id+"/resume", ""); rec.Code != http.StatusOK {
		t.Fatalf("resume of an unstamped lease = %d %s, want 200", rec.Code, rec.Body.String())
	}
	rows, err := svc.db.ListLeases(context.Background())
	if err != nil {
		t.Fatalf("list leases: %v", err)
	}
	var mb int
	for _, r := range rows {
		if r.ID == id {
			mb = r.MemoryMB
		}
	}
	if mb != 0 {
		t.Fatalf("pre-quota lease should keep memory_mb 0 across a resume, got %d", mb)
	}
}

// ownerIDFor resolves the identity user id behind a token.
func ownerIDFor(t *testing.T, h http.Handler, token string) string {
	t.Helper()
	rec, body := doUsersReq(t, h, "GET", "/api/users/me", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("users/me: %d %s", rec.Code, rec.Body.String())
	}
	return body["user"].(map[string]any)["id"].(string)
}

// TestMemQuotaUndrainDeferredOverQuota: an undrain whose resume fails
// the memory cap leaves the lease drained (retryable), not lost; once
// the cap is raised, undrain brings it back.
func TestMemQuotaUndrainDeferredOverQuota(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "big", 4096)
	ids, _ := identity.NewStore("")
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(db))
	srv.SetAdminToken("admin-tok")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	u, err := ids.AddUser("worker", identity.KindPerson, []string{"SHA256:fp-b"}, "work-tok")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := ids.SetQuota(u.ID, 0, 0, 0, 4096, 0); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	ctx := context.Background()
	l, err := svc.grant(ctx, u.ID, "big", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	// Shrink the cap under the running lease, then drain: the pause
	// frees nothing the check can grant — 4096 over a 2048 cap.
	if err := ids.SetQuota(u.ID, 0, 0, 0, 2048, 0); err != nil {
		t.Fatalf("shrink quota: %v", err)
	}
	resp, body := doReq(t, "POST", ts.URL+"/api/admin/drain", "admin-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("drain = %d (%v), want 200", resp.StatusCode, body)
	}
	resp, body = doReq(t, "POST", ts.URL+"/api/admin/undrain", "admin-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("undrain = %d (%v), want 200", resp.StatusCode, body)
	}
	failed, ok := body["failed"].([]any)
	if !ok || len(failed) != 1 {
		t.Fatalf("undrain failed = %v, want exactly the over-quota lease", body["failed"])
	}
	if got := l.State; got != "suspended" || !l.Drained {
		t.Fatalf("over-quota undrain should leave the lease suspended and drained, got state=%s drained=%v", l.State, l.Drained)
	}
	// Raise the cap; the retry resumes the lease.
	if err := ids.SetQuota(u.ID, 0, 0, 0, 4096, 0); err != nil {
		t.Fatalf("raise quota: %v", err)
	}
	resp, body = doReq(t, "POST", ts.URL+"/api/admin/undrain", "admin-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retry undrain = %d (%v), want 200", resp.StatusCode, body)
	}
	if body["resumed"] != float64(1) {
		t.Fatalf("retry undrain resumed = %v, want 1", body["resumed"])
	}
	if got := l.State; got != "running" {
		t.Fatalf("lease after the retry = %s, want running", got)
	}
}
