package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jrimmer/spoond/v3/identity"
)

// newMemQuotaServer bootstraps admin + a user over images of the given
// sizes (name -> memory_mb). Returns the server, its handler, the
// user's token and its id. There are no per-owner caps since 3.0 (fair
// shares, FS4); the tests here cover the memory charge and the
// in-flight reservation, which fair-share usage still reads.
func newMemQuotaServer(t *testing.T, images map[string]int) (*Server, http.Handler, string, string) {
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
	req := httptest.NewRequest("POST", "/api/leases", strings.NewReader(body))
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

// usedMiBFor reads used_mib from GET /api/users/me.
func usedMiBFor(t *testing.T, h http.Handler, token string) int {
	t.Helper()
	rec, body := doUsersReq(t, h, "GET", "/api/users/me", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("users/me: %d %s", rec.Code, rec.Body.String())
	}
	return int(body["user"].(map[string]any)["used_mib"].(float64))
}

// TestMemQuotaRacingCreatesChargeOnce: two racing creates both land, and
// the in-flight reservations (pending / pendingMiB) leave no residue —
// a reservation is counted exactly once, through pendingMiB until the
// lease lands and through the lease after.
func TestMemQuotaRacingCreatesChargeOnce(t *testing.T) {
	srv, h, tok, uid := newMemQuotaServer(t, map[string]int{"mid": 2048})

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
	for _, c := range codes {
		if c != http.StatusCreated {
			t.Fatalf("racing creates: codes=%v, want both 201", codes)
		}
	}
	if got := usedMiBFor(t, h, tok); got != 4096 {
		t.Fatalf("used_mib after the race = %d, want 4096", got)
	}
	svc := srv.svc
	svc.store.mu.Lock()
	pending, pendingMiB := svc.store.pending[uid], svc.store.pendingMiB[uid]
	svc.store.mu.Unlock()
	if pending != 0 || pendingMiB != 0 {
		t.Fatalf("after the race pending = %d, pendingMiB = %d; want 0, 0", pending, pendingMiB)
	}
}

// TestReserveQuotaPendingReservation: reserveQuota reserves n slots and
// n*memoryMB under the store lock, the reservation shows in usedMiB
// while it is in flight, and releaseQuotaReservation drops it again.
func TestReserveQuotaPendingReservation(t *testing.T) {
	srv, _, _, uid := newMemQuotaServer(t, map[string]int{"mid": 2048})
	svc := srv.svc

	if err := svc.reserveQuota(uid, 3, 1024, true); err != nil {
		t.Fatalf("reserveQuota: %v", err)
	}
	svc.store.mu.Lock()
	pending, pendingMiB := svc.store.pending[uid], svc.store.pendingMiB[uid]
	svc.store.mu.Unlock()
	if pending != 3 || pendingMiB != 3072 {
		t.Fatalf("in flight pending = %d, pendingMiB = %d; want 3, 3072", pending, pendingMiB)
	}
	if got := svc.usedMiB(uid); got != 3072 {
		t.Fatalf("usedMiB with a reservation in flight = %d, want 3072", got)
	}
	svc.releaseQuotaReservation(uid, 3, 1024)
	if got := svc.usedMiB(uid); got != 0 {
		t.Fatalf("usedMiB after release = %d, want 0", got)
	}
}

// TestMemQuotaSuspendedNotCharged: a suspended lease holds no
// hugepages, so its MiB is no longer charged.
func TestMemQuotaSuspendedNotCharged(t *testing.T) {
	srv, h, tok, _ := newMemQuotaServer(t, map[string]int{"big": 4096})
	svc := srv.svc
	owner := ownerIDFor(t, h, tok)

	rec, first := createPersistentAs(t, h, tok, "big")
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create: %d %s", rec.Code, rec.Body.String())
	}
	if got := usedMiBFor(t, h, tok); got != 4096 {
		t.Fatalf("used_mib running = %d, want 4096", got)
	}
	if _, err := svc.suspend(context.Background(), owner, first["id"].(string)); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if got := usedMiBFor(t, h, tok); got != 0 {
		t.Fatalf("used_mib suspended = %d, want 0", got)
	}
}

// TestMemQuotaRestartFitsAfterSuspend: a cold restart of a suspended
// lease brings it back running.
func TestMemQuotaRestartFitsAfterSuspend(t *testing.T) {
	srv, h, tok, uid := newMemQuotaServer(t, map[string]int{"big": 4096})
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
	if rec := postLeaseAction(t, h, tok, "/api/leases/"+id+"/restart?mode=cold", ""); rec.Code != http.StatusOK {
		t.Fatalf("cold restart = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if l := svc.lookup(uid, id); l == nil || !l.live() {
		t.Fatal("cold restart should have brought the lease back running")
	}
}

// TestMemQuotaReleaseFrees: deleting a lease frees its MiB.
func TestMemQuotaReleaseFrees(t *testing.T) {
	_, h, tok, _ := newMemQuotaServer(t, map[string]int{"big": 4096})

	rec, first := createSandboxAs(t, h, tok, "big")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if got := usedMiBFor(t, h, tok); got != 4096 {
		t.Fatalf("used_mib = %d, want 4096", got)
	}
	del := httptest.NewRecorder()
	dreq := httptest.NewRequest("DELETE", "/api/leases/"+first["id"].(string), nil)
	dreq.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(del, dreq)
	if del.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", del.Code)
	}
	if got := usedMiBFor(t, h, tok); got != 0 {
		t.Fatalf("used_mib after release = %d, want 0", got)
	}
}

// TestMemQuotaLeaseDetailShowsCharge: GET /api/leases/{id} carries
// the lease's charged_mib.
func TestMemQuotaLeaseDetailShowsCharge(t *testing.T) {
	_, h, tok, _ := newMemQuotaServer(t, map[string]int{"big": 4096})

	rec, first := createSandboxAs(t, h, tok, "big")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest("GET", "/api/leases/"+first["id"].(string), nil)
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
}

// TestMemQuotaPreQuotaLeaseResume: a lease persisted before the charge
// stamp existed (memory_mb 0 in the store) resumes without a memory
// charge — visible to the operator in the log, never blocked on a
// missing stamp.
func TestMemQuotaPreQuotaLeaseResume(t *testing.T) {
	srv, h, tok, uid := newMemQuotaServer(t, map[string]int{"big": 4096})
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
	if rec := postLeaseAction(t, h, tok, "/api/leases/"+id+"/resume", ""); rec.Code != http.StatusOK {
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

// TestMemQuotaRecoveryNoReservationLeak: a successful recovery of a
// suspended lease must drop its memory reservation — a leaked one stays
// in the pending sum forever, inflating used_mib and charging every
// later grant against a phantom (the owner looks like a big borrower
// with nothing running).
func TestMemQuotaRecoveryNoReservationLeak(t *testing.T) {
	srv, h, tok, uid := newMemQuotaServer(t, map[string]int{"mid": 1024})
	svc := srv.svc
	owner := ownerIDFor(t, h, tok)

	rec, first := createPersistentAs(t, h, tok, "mid")
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
	if err := svc.recoverFromCheckpoint(context.Background(), l); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	// The recovered lease is the only charge: used_mib is exactly 1024,
	// with no reservation left behind.
	rec, body := doUsersReq(t, h, "GET", "/api/users/me", tok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("users/me: %d %s", rec.Code, rec.Body.String())
	}
	if got := int(body["user"].(map[string]any)["used_mib"].(float64)); got != 1024 {
		t.Fatalf("used_mib after recovery = %d, want 1024 (a leaked reservation would inflate it)", got)
	}
	// A second 1024 lease adds exactly its own charge: no phantom
	// reservation that never went away.
	if rec, _ = createSandboxAs(t, h, tok, "mid"); rec.Code != http.StatusCreated {
		t.Fatalf("second create after recovery = %d %s, want 201", rec.Code, rec.Body.String())
	}
	if rec, body = doUsersReq(t, h, "GET", "/api/users/me", tok, ""); rec.Code != http.StatusOK {
		t.Fatalf("users/me: %d", rec.Code)
	}
	if got := int(body["user"].(map[string]any)["used_mib"].(float64)); got != 2048 {
		t.Fatalf("used_mib = %d, want 2048", got)
	}
}

// TestMemQuotaRecoveryKeepsCheckpointSize: recovery resumes the
// lease's checkpoint build, which runs at the size it was taken at, so
// that build's memory is what recovery re-admits and stamps (#151 Z1)
// — not the image's current memory_mb, even when the image grew while
// the lease was suspended (the VM would run 1024 while charged 2048).
func TestMemQuotaRecoveryKeepsCheckpointSize(t *testing.T) {
	srv, h, tok, uid := newMemQuotaServer(t, map[string]int{"mid": 1024})
	svc := srv.svc
	owner := ownerIDFor(t, h, tok)

	rec, first := createPersistentAs(t, h, tok, "mid")
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
	// The image's memory_mb grows while the lease is suspended.
	seedImage(t, svc.db, "mid", 2048)
	if err := svc.recoverFromCheckpoint(context.Background(), l); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if l.MemoryMB != 1024 {
		t.Fatalf("recovered lease charge = %d, want the checkpoint build's 1024", l.MemoryMB)
	}
	rec, body := doUsersReq(t, h, "GET", "/api/users/me", tok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("users/me: %d %s", rec.Code, rec.Body.String())
	}
	if got := int(body["user"].(map[string]any)["used_mib"].(float64)); got != 1024 {
		t.Fatalf("used_mib after recovery = %d, want the checkpoint build's 1024", got)
	}
}
