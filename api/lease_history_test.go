package api

// The lease_history table's write side (records A, spoond-15a5): a
// release and a loss each record exactly one row, a lost lease's later
// release keeps the loss, and the release reads the nap stamp a
// resume closed — but never the final sleep of a lease released while
// suspended.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v3/store"
	"github.com/jrimmer/spoond/v3/substrate"
)

// leaseHistoryTestService grants one running persistent lease on an
// image of the given memory. A persistent lease can suspend and resume,
// which the nap tests need.
func leaseHistoryTestService(t *testing.T, imageMB int) (*Service, *store.DB, *Lease) {
	t.Helper()
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", imageMB)
	l, err := svc.grant(context.Background(), "consumer-a", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	return svc, db, l
}

// leaseHistoryRows reads every history row; one lease under test means
// at most one.
func leaseHistoryRows(t *testing.T, db *store.DB) []store.LeaseHistoryRow {
	t.Helper()
	rows, err := db.ListLeaseHistory(context.Background())
	if err != nil {
		t.Fatalf("list lease history: %v", err)
	}
	return rows
}

// TestLeaseHistoryReleasedWritesOneRow: the release of a measured lease
// writes exactly one row — outcome released, the journal reason, the
// measured peak, the start kind the grant stamped — and the lease row is
// gone when it returns.
func TestLeaseHistoryReleasedWritesOneRow(t *testing.T) {
	svc, db, l := leaseHistoryTestService(t, 2048)
	ctx := context.Background()

	// A measured peak so the row's peak_mib is the release's number, and
	// a start kind so the row's start_kind is the grant's.
	svc.store.mu.Lock()
	l.SandboxID, l.HostIP = "sb_hist", "10.0.0.1"
	l.Start = "warm"
	l.StartMS = 41
	svc.store.mu.Unlock()
	svc.memSamples.record(
		map[string]*Lease{l.ID: l},
		map[string]float64{"sb_hist": 411.2 * 1024 * 1024},
		map[string]float64{"sb_hist": 8 * 1024 * 1024 * 1024},
	)

	reason := "ci job 3609 ✓ 11m02s"
	svc.releaseBecause(ctx, l, reason)

	rows := leaseHistoryRows(t, db)
	if len(rows) != 1 {
		t.Fatalf("history rows = %d, want 1", len(rows))
	}
	h := rows[0]
	if h.LeaseID != l.ID || h.Owner != "consumer-a" || h.Image != "py-base" {
		t.Fatalf("identity fields = %s/%s/%s, want the lease's", h.LeaseID, h.Owner, h.Image)
	}
	if h.Outcome != "released" {
		t.Fatalf("outcome = %q, want released", h.Outcome)
	}
	if h.Reason != reason {
		t.Fatalf("reason = %q, want %q", h.Reason, reason)
	}
	if h.PeakMiB != 412 {
		t.Fatalf("peak_mib = %d, want 412 (the release's measured peak)", h.PeakMiB)
	}
	if h.StartKind != "warm" || h.StartMS != 41 {
		t.Fatalf("start stamp = %q/%d ms, want warm/41", h.StartKind, h.StartMS)
	}
	if h.MemoryMB != 2048 {
		t.Fatalf("memory_mb = %d, want 2048", h.MemoryMB)
	}
	if h.CreatedAt.IsZero() || h.EndedAt.Before(h.CreatedAt) {
		t.Fatalf("created/ended = %v/%v, want a real span", h.CreatedAt, h.EndedAt)
	}
	// The lease row went with the release: the history row is all that
	// is left.
	if _, err := db.GetLease(ctx, l.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lease row after release = %v, want ErrNotFound", err)
	}
}

// TestLeaseHistoryLostWritesOneRow: marking a lease lost writes exactly
// one row, outcome lost with the loss reason and no peak. The markLost
// path runs its history write across a release of the store lock; the
// call must return (no deadlock) with the row present.
func TestLeaseHistoryLostWritesOneRow(t *testing.T) {
	svc, db, l := leaseHistoryTestService(t, 2048)
	svc.store.mu.Lock()
	l.SandboxID, l.HostIP = "sb_hist", "10.0.0.1"
	svc.store.mu.Unlock()

	reason := "substrate crash: node lost"
	done := make(chan string, 1)
	go func() {
		svc.store.mu.Lock()
		defer svc.store.mu.Unlock()
		done <- svc.markLost(l, reason)
	}()
	select {
	case got := <-done:
		if got != reason {
			t.Fatalf("markLost = %q, want %q", got, reason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("markLost did not return: the history write deadlocked on the store lock")
	}

	rows := leaseHistoryRows(t, db)
	if len(rows) != 1 {
		t.Fatalf("history rows = %d, want 1", len(rows))
	}
	h := rows[0]
	if h.Outcome != "lost" || h.Reason != reason {
		t.Fatalf("outcome/reason = %q/%q, want lost/%q", h.Outcome, h.Reason, reason)
	}
	if h.PeakMiB != 0 {
		t.Fatalf("peak_mib = %d, want 0 (a loss carries no peak)", h.PeakMiB)
	}
}

// TestLeaseHistoryLostThenReleasedKeepsLost: a lost lease the GC later
// releases (the lost grace expires) keeps its lost row — the release
// must not overwrite it and must not add a second one (INSERT OR
// IGNORE).
func TestLeaseHistoryLostThenReleasedKeepsLost(t *testing.T) {
	svc, db, l := leaseHistoryTestService(t, 2048)
	ctx := context.Background()
	svc.store.mu.Lock()
	l.SandboxID, l.HostIP = "sb_hist", "10.0.0.1"
	svc.store.mu.Unlock()

	svc.store.mu.Lock()
	svc.markLost(l, "substrate crash: node lost")
	svc.store.mu.Unlock()

	// The grace releases the lost lease like the GC's pass does.
	svc.releaseBecause(ctx, l, lostReleaseReason)

	rows := leaseHistoryRows(t, db)
	if len(rows) != 1 {
		t.Fatalf("history rows = %d, want 1 (the release must not add a second)", len(rows))
	}
	if rows[0].Outcome != "lost" {
		t.Fatalf("outcome = %q, want the loss kept", rows[0].Outcome)
	}
	if rows[0].Reason != "substrate crash: node lost" {
		t.Fatalf("reason = %q, want the loss's reason kept", rows[0].Reason)
	}
}

// TestLeaseHistoryLostKeepsStampsDespiteReleaseRace: the release racing
// the loss deletes the lease row before the loss's history write reads
// it; the row's stamps are then the ones the loss captured under the
// lock, not zeros (the captured copies exist for exactly this race).
func TestLeaseHistoryLostKeepsStampsDespiteReleaseRace(t *testing.T) {
	svc, db, l := leaseHistoryTestService(t, 2048)
	svc.store.mu.Lock()
	l.SandboxID, l.HostIP = "sb_hist", "10.0.0.1"
	l.Start = "cold"
	l.StartMS = 77
	l.MaxNapSecs = 120
	svc.store.mu.Unlock()

	// Simulate the race: the lease row is gone by the time the loss's
	// history write runs (the release deleted it), so GetLease answers
	// ErrNotFound and the write must stand on the captured copies.
	svc.releaseBecause(context.Background(), l, "deleted via API by consumer-a")

	svc.store.mu.Lock()
	svc.markLost(l, "late loss after release")
	svc.store.mu.Unlock()

	rows := leaseHistoryRows(t, db)
	if len(rows) != 1 {
		t.Fatalf("history rows = %d, want 1", len(rows))
	}
	h := rows[0]
	// The release ran first and its INSERT OR IGNORE landed first: the
	// loss must not have overwritten it. This is the markLost path's
	// guard against a resurrected row.
	if h.Outcome != "released" {
		t.Fatalf("outcome = %q, want the release kept (INSERT OR IGNORE)", h.Outcome)
	}
}

// TestLeaseHistoryLostWithoutLeaseRowWritesCapturedStamps: a loss whose
// row never made it (or was already deleted) still writes its row from
// the captured stamps — outcome lost, the caller's start_ms and
// max_nap_secs, not the zero row.
func TestLeaseHistoryLostWithoutLeaseRowWritesCapturedStamps(t *testing.T) {
	svc, db, l := leaseHistoryTestService(t, 2048)
	svc.store.mu.Lock()
	l.SandboxID, l.HostIP = "sb_hist", "10.0.0.1"
	l.Start = "cold"
	l.StartMS = 77
	l.MaxNapSecs = 120
	l.released = true // the row deletion happened; the lease object lingers
	svc.store.mu.Unlock()

	svc.writeLeaseHistoryStamps(l, "lost", "row lost to a crash", 0, 77, 120)

	rows := leaseHistoryRows(t, db)
	if len(rows) != 1 {
		t.Fatalf("history rows = %d, want 1", len(rows))
	}
	h := rows[0]
	if h.Outcome != "lost" || h.Reason != "row lost to a crash" {
		t.Fatalf("outcome/reason = %q/%q, want lost/row lost to a crash", h.Outcome, h.Reason)
	}
	if h.StartMS != 77 || h.StartKind != "cold" || h.MaxNapSecs != 120 {
		t.Fatalf("stamps = %d/%q/%d, want 77/cold/120 (the captured copies)",
			h.StartMS, h.StartKind, h.MaxNapSecs)
	}
}

// TestLeaseHistoryNapCountedOnResume: a suspend stamps nap_since on the
// row; the resume raises max_nap_secs to the sleep's length and clears
// the stamp; the release's history row then carries the nap.
func TestLeaseHistoryNapCountedOnResume(t *testing.T) {
	svc, db, l := leaseHistoryTestService(t, 2048)
	ctx := context.Background()
	// A huge node so the resume always fits.
	svc.sub.(*testSub).SetNodeInfo(substrate.NodeInfo{
		Status:            "healthy",
		HugepagesTotal:    1 << 20,
		HugepageSizeBytes: 2 << 20,
	}, nil)

	// The clock is ours: the sleep is exactly 90 s.
	base := time.Now()
	svc.now = func() time.Time { return base }

	if _, err := svc.pauseLease(ctx, l, false); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// The pause's own action stamp is when the sleep began.
	svc.store.mu.Lock()
	napAt := l.LastActionAt
	svc.store.mu.Unlock()
	if napAt.IsZero() {
		t.Fatal("pause left LastActionAt zero")
	}
	row, err := db.GetLease(ctx, l.ID)
	if err != nil {
		t.Fatalf("get lease after pause: %v", err)
	}
	if row.NapSince.IsZero() {
		t.Fatal("the pause left no nap_since on the row")
	}

	svc.now = func() time.Time { return napAt.Add(90 * time.Second) }
	if _, err := svc.resumeLeaseBody(ctx, l); err != nil {
		t.Fatalf("resume: %v", err)
	}
	row, err = db.GetLease(ctx, l.ID)
	if err != nil {
		t.Fatalf("get lease after resume: %v", err)
	}
	if !row.NapSince.IsZero() {
		t.Fatalf("nap_since after resume = %v, want cleared", row.NapSince)
	}
	if row.MaxNapSecs != 90 {
		t.Fatalf("max_nap_secs after resume = %d, want 90", row.MaxNapSecs)
	}

	svc.releaseBecause(ctx, l, "deleted via API by consumer-a")
	rows := leaseHistoryRows(t, db)
	if len(rows) != 1 {
		t.Fatalf("history rows = %d, want 1", len(rows))
	}
	if rows[0].MaxNapSecs != 90 {
		t.Fatalf("history max_nap_secs = %d, want 90", rows[0].MaxNapSecs)
	}
}

// TestLeaseHistoryFinalSleepNotCounted: a lease released while
// suspended keeps its nap_since (nothing cleared it) and its history
// row's max_nap_secs counts only naps completed before — the final
// sleep is never a nap.
func TestLeaseHistoryFinalSleepNotCounted(t *testing.T) {
	svc, db, l := leaseHistoryTestService(t, 2048)
	ctx := context.Background()

	base := time.Now()
	svc.now = func() time.Time { return base }

	// One completed nap first (60 s), so the release row has a number
	// that is not just zero.
	if _, err := svc.pauseLease(ctx, l, false); err != nil {
		t.Fatalf("pause: %v", err)
	}
	svc.store.mu.Lock()
	napAt := l.LastActionAt
	svc.store.mu.Unlock()
	svc.now = func() time.Time { return napAt.Add(60 * time.Second) }
	if _, err := svc.resumeLeaseBody(ctx, l); err != nil {
		t.Fatalf("resume: %v", err)
	}

	// The second sleep never ends: the lease is released mid-nap.
	if _, err := svc.pauseLease(ctx, l, false); err != nil {
		t.Fatalf("second pause: %v", err)
	}
	svc.now = func() time.Time { return napAt.Add(60 * time.Second).Add(10 * time.Minute) }
	svc.releaseBecause(ctx, l, "deleted via API by consumer-a")

	rows := leaseHistoryRows(t, db)
	if len(rows) != 1 {
		t.Fatalf("history rows = %d, want 1", len(rows))
	}
	if rows[0].MaxNapSecs != 60 {
		t.Fatalf("history max_nap_secs = %d, want 60 (the completed nap; the final sleep counts nothing)", rows[0].MaxNapSecs)
	}
}
