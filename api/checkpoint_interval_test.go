package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/store"
)

// TestCheckpointIntervalZeroNeverPicked: an interval of 0 (never) is
// never picked by the periodic pass, however active the lease is.
func TestCheckpointIntervalZeroNeverPicked(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.store.mu.Lock()
	l.CheckpointInterval = 0 // never
	svc.saveLeaseLocked(l)
	svc.store.mu.Unlock()

	svc.touch(l.ID)
	svc.checkpointIdleLeases(ctx)
	if got := calls(sub.Fake, "Checkpoint "+l.SandboxID); got != 0 {
		t.Fatalf("never lease checkpointed %d times, want 0", got)
	}
}

// TestCheckpointIntervalCustomHonoured: a lease with its own interval is
// picked once the interval has elapsed since its last checkpoint and it
// has been active since — and not while the last checkpoint is inside
// the interval.
func TestCheckpointIntervalCustomHonoured(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.store.mu.Lock()
	l.CheckpointInterval = 600 // ten minutes
	// Pretend the lease was checkpointed 5 minutes ago: inside the
	// interval, so not due despite activity after that checkpoint.
	l.LastCheckpointAt = svc.now().Add(-5 * time.Minute)
	svc.saveLeaseLocked(l)
	svc.store.mu.Unlock()

	// Active, but the last checkpoint is inside the interval: not due.
	svc.touch(l.ID)
	svc.checkpointIdleLeases(ctx)
	if got := calls(sub.Fake, "Checkpoint "+l.SandboxID); got != 0 {
		t.Fatalf("lease picked inside its interval: %d, want 0", got)
	}

	// Age the last checkpoint past the interval: due (LastActive is
	// after LastCheckpointAt from the grant).
	svc.store.mu.Lock()
	l.LastCheckpointAt = svc.now().Add(-11 * time.Minute)
	svc.saveLeaseLocked(l)
	svc.store.mu.Unlock()
	svc.checkpointIdleLeases(ctx)
	if got := calls(sub.Fake, "Checkpoint "+l.SandboxID); got != 1 {
		t.Fatalf("due lease checkpointed %d times, want 1", got)
	}

	// Active again with a fresh checkpoint: not due until the next
	// interval elapses.
	svc.touch(l.ID)
	svc.checkpointIdleLeases(ctx)
	if got := calls(sub.Fake, "Checkpoint "+l.SandboxID); got != 1 {
		t.Fatalf("freshly checkpointed lease picked again: %d total, want 1", got)
	}
}

// TestCheckpointIntervalHeldNoIntervalNotPicked: being held does not
// put a lease on the periodic pass any more (2.3, #122) — a held lease
// with no interval (host default never) is not picked.
func TestCheckpointIntervalHeldNoIntervalNotPicked(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Hour, false, "", nil, "ci-job", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.touch(l.ID)
	svc.checkpointIdleLeases(ctx)
	if got := calls(sub.Fake, "Checkpoint "+l.SandboxID); got != 0 {
		t.Fatalf("held lease without interval checkpointed %d times, want 0", got)
	}
}

// TestCheckpointIntervalHostDefault: a lease storing -1 (the host
// default) is picked on the host's interval; a lease with its own value
// is not.
func TestCheckpointIntervalHostDefault(t *testing.T) {
	svc, db, sub := newTestService(t)
	svc.cfg.CheckpointIntervalDefault = 300
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if got := svc.effectiveCheckpointInterval(l); got != 300 {
		t.Fatalf("effective interval = %d, want the host default 300", got)
	}
	if l.CheckpointInterval != -1 {
		t.Fatalf("stored interval = %d, want -1 (the host default)", l.CheckpointInterval)
	}

	// A fresh lease (never checkpointed, active) is due on the host
	// default's clock exactly like on its own.
	svc.checkpointIdleLeases(ctx)
	if got := calls(sub.Fake, "Checkpoint "+l.SandboxID); got != 1 {
		t.Fatalf("lease on the host default checkpointed %d times, want 1", got)
	}

	// Checkpoint fresh and no activity since: not due until the host
	// default has elapsed.
	svc.checkpointIdleLeases(ctx)
	if got := calls(sub.Fake, "Checkpoint "+l.SandboxID); got != 1 {
		t.Fatalf("lease picked again inside the host default: %d total, want 1", got)
	}

	// Age past the host default: due.
	svc.store.mu.Lock()
	l.LastCheckpointAt = svc.now().Add(-6 * time.Minute)
	svc.saveLeaseLocked(l)
	svc.store.mu.Unlock()
	svc.checkpointIdleLeases(ctx)
	if got := calls(sub.Fake, "Checkpoint "+l.SandboxID); got != 2 {
		t.Fatalf("lease due on the host default checkpointed %d times, want 2", got)
	}

	// The host default never reaches a lease with its own interval.
	svc.touch(l.ID)
	svc.store.mu.Lock()
	l.CheckpointInterval = 0 // own "never" beats the host default
	l.LastCheckpointAt = svc.now().Add(-6 * time.Minute)
	svc.saveLeaseLocked(l)
	svc.store.mu.Unlock()
	if got := svc.effectiveCheckpointInterval(l); got != 0 {
		t.Fatalf("effective interval = %d, want the lease's own 0", got)
	}
	svc.touch(l.ID)
	svc.checkpointIdleLeases(ctx)
	if got := calls(sub.Fake, "Checkpoint "+l.SandboxID); got != 2 {
		t.Fatalf("own never over host default checkpointed %d times total, want 2", got)
	}
}

// TestCheckpointIntervalValidation: the create field and the policy PUT
// accept 0 or 60..604800 and reject anything else with a 400 naming the
// field.
func TestCheckpointIntervalValidation(t *testing.T) {
	h, _, _ := newShareTestServer(t)

	create := func(body string) int {
		req := httptest.NewRequest("POST", "/api/leases", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok-a")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := create(`{"image":"py-base","ttl":120,"checkpoint_interval":0}`); got != http.StatusCreated {
		t.Fatalf("create checkpoint_interval=0: %d, want 201", got)
	}
	if got := create(`{"image":"py-base","ttl":120,"checkpoint_interval":60}`); got != http.StatusCreated {
		t.Fatalf("create checkpoint_interval=60: %d, want 201", got)
	}
	if got := create(`{"image":"py-base","ttl":120,"checkpoint_interval":604800}`); got != http.StatusCreated {
		t.Fatalf("create checkpoint_interval=604800: %d, want 201", got)
	}
	for _, bad := range []string{"59", "604801", "-5"} {
		req := httptest.NewRequest("POST", "/api/leases",
			strings.NewReader(`{"image":"py-base","ttl":120,"checkpoint_interval":`+bad+`}`))
		req.Header.Set("Authorization", "Bearer tok-a")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("create checkpoint_interval=%s: %d, want 400", bad, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "checkpoint_interval") {
			t.Fatalf("create checkpoint_interval=%s: error does not name the field: %s", bad, rec.Body)
		}
	}
}

// TestCheckpointPolicyPutAndEvent: PUT
// /api/leases/{id}/checkpoint-policy sets the interval for the owner
// (and an admin), others get 404, and a checkpoint_policy event is
// emitted; the row field shows the effective value.
func TestCheckpointPolicyPutAndEvent(t *testing.T) {
	h, _, _ := newShareTestServer(t)
	lid := createLeaseAs(h, "tok-a")
	if lid == "" {
		t.Fatal("a could not create lease")
	}

	all := func() []LeaseEvent { return nil } // replaced below
	_ = all

	// Wrong owner: 404, like every owner-scoped route.
	req := httptest.NewRequest("PUT", "/api/leases/"+lid+"/checkpoint-policy",
		strings.NewReader(`{"checkpoint_interval":120}`))
	req.Header.Set("Authorization", "Bearer tok-b")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("other owner policy PUT: %d, want 404", rec.Code)
	}

	// Validation errors.
	for _, bad := range []string{"59", "604801"} {
		req := httptest.NewRequest("PUT", "/api/leases/"+lid+"/checkpoint-policy",
			strings.NewReader(`{"checkpoint_interval":`+bad+`}`))
		req.Header.Set("Authorization", "Bearer tok-a")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("policy PUT interval=%s: %d, want 400", bad, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "checkpoint_interval") {
			t.Fatalf("policy PUT interval=%s: error does not name the field: %s", bad, rec.Body)
		}
	}

	// The owner sets 120.
	req = httptest.NewRequest("PUT", "/api/leases/"+lid+"/checkpoint-policy",
		strings.NewReader(`{"checkpoint_interval":120}`))
	req.Header.Set("Authorization", "Bearer tok-a")
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner policy PUT: %d, want 200", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("policy PUT body: %v", err)
	}
	if got, _ := resp["checkpoint_interval"].(float64); got != 120 {
		t.Fatalf("policy PUT response interval = %v, want 120", resp["checkpoint_interval"])
	}

	// The row field shows the effective value.
	req = httptest.NewRequest("GET", "/api/leases/"+lid, nil)
	req.Header.Set("Authorization", "Bearer tok-a")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("lease detail: %d, want 200", rec.Code)
	}
	var detail map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("lease detail body: %v", err)
	}
	if got, _ := detail["checkpoint_interval"].(float64); got != 120 {
		t.Fatalf("detail checkpoint_interval = %v, want 120", detail["checkpoint_interval"])
	}
}

// TestCheckpointPolicyEventEmitted: the policy PUT emits one
// checkpoint_policy event naming the new effective seconds.
func TestCheckpointPolicyEventEmitted(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	all := svc.Subscribe(EventFilter{})
	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.setCheckpointPolicy(l, 120); err != nil {
		t.Fatalf("setCheckpointPolicy: %v", err)
	}
	all.Close()

	events := eventsFor(collectEvents(all.C), l.ID)
	var found bool
	for _, ev := range events {
		if ev.Type == LeaseCheckpointPolicy {
			found = true
			if !strings.Contains(ev.Detail, "120") {
				t.Fatalf("checkpoint_policy detail = %q, want the effective seconds", ev.Detail)
			}
		}
	}
	if !found {
		t.Fatalf("no checkpoint_policy event among %v", eventTypes(events))
	}
}

// TestCheckpointIntervalRowFieldAndPersistence: the create response, the
// list row and the detail all show the effective value, and the stored
// row keeps the raw -1/0/N value.
func TestCheckpointIntervalRowFieldAndPersistence(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)

	// Host default 300: an omitted field resolves to 300.
	svc.cfg.CheckpointIntervalDefault = 300
	resp, created := doReq(t, "POST", ts.URL+"/api/leases", "token-a",
		map[string]any{"image": "py-base", "ttl": 3600})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d: %v", resp.StatusCode, created)
	}
	if got, _ := created["checkpoint_interval"].(float64); got != 300 {
		t.Fatalf("create response checkpoint_interval = %v, want 300 (host default)", created["checkpoint_interval"])
	}
	id, _ := created["id"].(string)

	// The stored row keeps the raw -1.
	row, err := db.GetLease(context.Background(), id)
	if err != nil {
		t.Fatalf("load row: %v", err)
	}
	if row.CheckpointInterval != -1 {
		t.Fatalf("stored interval = %d, want -1", row.CheckpointInterval)
	}

	// A create with 0 stores 0 and reports never.
	resp, never := doReq(t, "POST", ts.URL+"/api/leases", "token-a",
		map[string]any{"image": "py-base", "ttl": 3600, "checkpoint_interval": 0})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create 0: %d: %v", resp.StatusCode, never)
	}
	if got, _ := never["checkpoint_interval"].(float64); got != 0 {
		t.Fatalf("create 0 response checkpoint_interval = %v, want 0", never["checkpoint_interval"])
	}
	neverID, _ := never["id"].(string)

	// List rows and the detail show the effective values.
	listReq, _ := http.NewRequest("GET", ts.URL+"/api/leases", nil)
	listReq.Header.Set("Authorization", "Bearer token-a")
	listResp, err := http.DefaultClient.Do(listReq)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var list struct {
		Sandboxes []map[string]any `json:"sandboxes"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatalf("list body: %v", err)
	}
	listResp.Body.Close()
	got := map[string]float64{}
	for _, m := range list.Sandboxes {
		idv, _ := m["id"].(string)
		iv, _ := m["checkpoint_interval"].(float64)
		got[idv] = iv
	}
	if got[id] != 300 || got[neverID] != 0 {
		t.Fatalf("list intervals = %v, want %s=300 %s=0", got, id, neverID)
	}

	// The detail of the never lease shows 0.
	resp, detail := doReq(t, "GET", ts.URL+"/api/leases/"+neverID, "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("detail: %d", resp.StatusCode)
	}
	resp.Body.Close()
	if got, _ := detail["checkpoint_interval"].(float64); got != 0 {
		t.Fatalf("detail checkpoint_interval = %v, want 0", detail["checkpoint_interval"])
	}

	// The stored row keeps the 0.
	if row, err := db.GetLease(context.Background(), neverID); err != nil || row.CheckpointInterval != 0 {
		t.Fatalf("stored interval = %d (%v), want 0", row.CheckpointInterval, err)
	}
}

// mustRow loads one lease row or fails the test.
func mustRow(t *testing.T, db *store.DB, id string) store.LeaseRow {
	t.Helper()
	row, err := db.GetLease(context.Background(), id)
	if err != nil {
		t.Fatalf("load row %s: %v", id, err)
	}
	return row
}

// TestCheckpointIntervalCloneForkCopy: clone and fork copy the source's
// interval.
func TestCheckpointIntervalCloneForkCopy(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	src, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.store.mu.Lock()
	src.CheckpointInterval = 120
	svc.saveLeaseLocked(src)
	svc.store.mu.Unlock()

	clone, _, err := svc.clone(ctx, "c", src.ID)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if clone.CheckpointInterval != 120 {
		t.Fatalf("clone interval = %d, want 120", clone.CheckpointInterval)
	}

	forks, _, err := svc.fork(ctx, "c", src.ID, 2, true, time.Hour, "", "")
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	for _, f := range forks {
		if f.CheckpointInterval != 120 {
			t.Fatalf("fork interval = %d, want 120", f.CheckpointInterval)
		}
	}
}
