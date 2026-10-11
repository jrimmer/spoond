package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v3/store"
	"github.com/jrimmer/spoond/v3/substrate"
)

// addUsageSample inserts one usage_samples row straight into the store.
func addUsageSample(t *testing.T, db *store.DB, owner string, at time.Time, mem int, disk int64, running, paused int) {
	t.Helper()
	if err := db.InsertUsageSample(context.Background(), store.UsageSampleRow{
		Owner: owner, At: at, MemoryUsedMiB: mem, DiskUsedBytes: disk,
		Running: running, Paused: paused,
	}); err != nil {
		t.Fatalf("insert usage sample: %v", err)
	}
}

// TestUsageHistoryHandlerScoping: a caller reads only their own samples,
// oldest first, and the window's default and cap are applied.
func TestUsageHistoryHandlerScoping(t *testing.T) {
	svc, db, _ := newTestService(t)
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	srv := NewServer(svc, NewImageRegistry(db))
	h := srv.Handler()

	// a's samples, oldest first as inserted out of order; b's must never
	// appear in a's response.
	addUsageSample(t, db, "consumer-a", now.Add(-2*time.Hour), 20, 200, 1, 0)
	addUsageSample(t, db, "consumer-a", now.Add(-1*time.Hour), 10, 100, 0, 1)
	addUsageSample(t, db, "consumer-b", now.Add(-1*time.Hour), 99, 999, 9, 9)

	req := httptest.NewRequest("GET", "/api/usage/history?hours=24", nil)
	req.Header.Set("Authorization", "Bearer token-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Hours   int               `json:"hours"`
		Samples []usageSampleJSON `json:"samples"`
	}
	dec := json.NewDecoder(rec.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Hours != 24 {
		t.Fatalf("hours = %d, want 24", got.Hours)
	}
	if len(got.Samples) != 2 {
		t.Fatalf("got %d samples, want 2 (self-scoped)", len(got.Samples))
	}
	// Oldest first: the 10 MiB sample at -1h is last.
	if got.Samples[0].MemoryUsedMiB != 20 || got.Samples[1].MemoryUsedMiB != 10 {
		t.Fatalf("samples not oldest first: %+v", got.Samples)
	}
	if got.Samples[0].Running != 1 || got.Samples[1].Paused != 1 {
		t.Fatalf("running/paused wrong: %+v", got.Samples)
	}

	// b sees only b's sample.
	req = httptest.NewRequest("GET", "/api/usage/history", nil)
	req.Header.Set("Authorization", "Bearer token-b")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("b status %d: %s", rec.Code, rec.Body.String())
	}
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if got.Hours != usageHistoryDefaultHours {
		t.Fatalf("default hours = %d, want %d", got.Hours, usageHistoryDefaultHours)
	}
	if len(got.Samples) != 1 || got.Samples[0].MemoryUsedMiB != 99 {
		t.Fatalf("b saw wrong samples: %+v", got.Samples)
	}
}

// TestUsageHistoryHoursWindowAndCap: the hours parameter selects the
// window, an oversize value is clamped to the cap, a bad value is a 400
// and an empty history is an empty list, not a 404.
func TestUsageHistoryHoursWindowAndCap(t *testing.T) {
	svc, db, _ := newTestService(t)
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	srv := NewServer(svc, NewImageRegistry(db))
	h := srv.Handler()

	// One recent sample and one 6 days old (inside the cap's 7-day
	// window, outside a short one).
	addUsageSample(t, db, "consumer-a", now.Add(-time.Hour), 1, 1, 1, 0)
	addUsageSample(t, db, "consumer-a", now.Add(-6*24*time.Hour), 2, 2, 1, 0)

	get := func(query string) (*httptest.ResponseRecorder, map[string]any) {
		req := httptest.NewRequest("GET", "/api/usage/history"+query, nil)
		req.Header.Set("Authorization", "Bearer token-a")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec, body
	}

	// A 2-hour window drops the 6-day-old sample.
	rec, body := get("?hours=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("2h status %d: %s", rec.Code, rec.Body.String())
	}
	if n := len(body["samples"].([]any)); n != 1 {
		t.Fatalf("2h window samples = %d, want 1", n)
	}
	// A cap-sized window includes both.
	rec, body = get("?hours=168")
	if rec.Code != http.StatusOK {
		t.Fatalf("168h status %d", rec.Code)
	}
	if n := len(body["samples"].([]any)); n != 2 {
		t.Fatalf("168h window samples = %d, want 2", n)
	}
	// An oversize value is clamped, not refused, and reports the cap.
	rec, body = get("?hours=100000")
	if rec.Code != http.StatusOK {
		t.Fatalf("oversize status %d", rec.Code)
	}
	if int(body["hours"].(float64)) != usageHistoryMaxHours {
		t.Fatalf("oversize hours = %v, want %d", body["hours"], usageHistoryMaxHours)
	}
	if n := len(body["samples"].([]any)); n != 2 {
		t.Fatalf("oversize window samples = %d, want 2", n)
	}
	// Bad values are 400s.
	for _, q := range []string{"?hours=abc", "?hours=0", "?hours=-3"} {
		if rec, _ := get(q); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", q, rec.Code)
		}
	}
	// Empty history: an owner with no rows reads an empty list, 200.
	req := httptest.NewRequest("GET", "/api/usage/history", nil)
	req.Header.Set("Authorization", "Bearer token-b")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("b empty status %d: %s", rec.Code, rec.Body.String())
	}
	var empty struct {
		Samples []usageSampleJSON `json:"samples"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &empty); err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	if len(empty.Samples) != 0 {
		t.Fatalf("empty history = %+v, want []", empty.Samples)
	}
}

// TestUsageHistorySamplerFakeClock: one sample pass writes a row per
// owner from the fair-share snapshot at the injected clock, and the
// prune drops rows past the 7-day retention.
func TestUsageHistorySamplerFakeClock(t *testing.T) {
	svc, db, _, ids := newFairShareService(t)
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }

	alice := addIdentityUser(t, ids, "alice")
	bob := addIdentityUser(t, ids, "bob")
	addRunningLease(svc, "run-a", alice.ID, 64)
	seedNamedSnapshot(t, db, alice.ID, "big", 300<<20)
	// bob has a paused lease only, so his row's paused count is 1. The
	// paused lease's disk bytes come from its pause build.
	seedPauseBuild(t, db, "pause-b", bob.ID, 100<<20)
	svc.store.mu.Lock()
	svc.store.leases["pause-b"] = &Lease{ID: "pause-b", Owner: bob.ID, State: "suspended", Suspended: true}
	svc.store.mu.Unlock()
	// A stale sample past the retention window must be pruned.
	addUsageSample(t, db, alice.ID, now.Add(-8*24*time.Hour), 5, 5, 5, 5)

	svc.sampleUsage(context.Background())

	for _, o := range svc.fairShares(context.Background()).owners {
		rows, err := db.ListUsageHistory(context.Background(), o.Owner, time.Time{})
		if err != nil {
			t.Fatalf("list %s: %v", o.Owner, err)
		}
		if len(rows) != 1 {
			t.Fatalf("%s has %d samples, want 1 (stale pruned)", o.Owner, len(rows))
		}
		if !rows[0].At.Equal(now) {
			t.Fatalf("%s sample at %v, want %v", o.Owner, rows[0].At, now)
		}
	}

	aliceRows, err := db.ListUsageHistory(context.Background(), alice.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if aliceRows[0].MemoryUsedMiB != 64 {
		t.Fatalf("alice memory = %d, want 64", aliceRows[0].MemoryUsedMiB)
	}
	if aliceRows[0].DiskUsedBytes != 300<<20 {
		t.Fatalf("alice disk = %d, want %d", aliceRows[0].DiskUsedBytes, int64(300<<20))
	}
	if aliceRows[0].Running != 1 || aliceRows[0].Paused != 0 {
		t.Fatalf("alice running/paused = %d/%d, want 1/0", aliceRows[0].Running, aliceRows[0].Paused)
	}
	bobRows, err := db.ListUsageHistory(context.Background(), bob.ID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if bobRows[0].Running != 0 || bobRows[0].Paused != 1 {
		t.Fatalf("bob running/paused = %d/%d, want 0/1", bobRows[0].Running, bobRows[0].Paused)
	}
	if bobRows[0].MemoryUsedMiB != 0 {
		t.Fatalf("bob memory = %d, want 0 (paused holds no hugepages)", bobRows[0].MemoryUsedMiB)
	}

	// A second pass at the same instant writes a second row per owner.
	svc.sampleUsage(context.Background())
	if rows, err := db.ListUsageHistory(context.Background(), alice.ID, time.Time{}); err != nil || len(rows) != 2 {
		t.Fatalf("after second pass: %d rows, %v; want 2", len(rows), err)
	}
}

// TestUsageHistorySamplerSkipsOnUnknownCapacity: a pass whose capacity
// read failed must write nothing, so the zeroed buckets of a partial
// snapshot are never persisted as real history. The next pass with a
// readable capacity writes again.
func TestUsageHistorySamplerSkipsOnUnknownCapacity(t *testing.T) {
	svc, db, sub, ids := newFairShareService(t)
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }

	alice := addIdentityUser(t, ids, "alice")
	addRunningLease(svc, "run-a", alice.ID, 64)
	seedNamedSnapshot(t, db, alice.ID, "big", 300<<20)

	// A cold node-info cache makes totalHugepageMiB fail, so the
	// snapshot's capacityKnown is false and its disk bucket is zero.
	svc.nodeInfoAt = time.Time{}
	svc.sampleUsage(context.Background())
	rows, err := db.ListUsageHistory(context.Background(), alice.ID, time.Time{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("wrote %d row(s) on unknown capacity, want 0: %+v", len(rows), rows)
	}

	// Warm the cache and retry: the pass now writes the real numbers.
	sub.SetNodeInfo(substrate.NodeInfo{
		Status:            "healthy",
		HugepagesTotal:    512,
		HugepageSizeBytes: 2 << 20,
	}, nil)
	svc.updateNodeMetrics(context.Background())
	svc.sampleUsage(context.Background())
	rows, err = db.ListUsageHistory(context.Background(), alice.ID, time.Time{})
	if err != nil {
		t.Fatalf("list after retry: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("after retry: %d rows, want 1", len(rows))
	}
	if rows[0].DiskUsedBytes != 300<<20 || rows[0].MemoryUsedMiB != 64 {
		t.Fatalf("retry row wrong: %+v", rows[0])
	}
}
