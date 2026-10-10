package api

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/store"
)

// TestPersistRoundTrip grants a lease, names it and shares it on a
// service backed by a SQLite store, then verifies a NEW service on the
// same database loads the lease, its name, its share, and the sandbox
// fields (TemplateID from the image row, BuildID from the sandboxes
// row). Shutdown must not kill anything: state survives the backend
// (U05, U08).
func TestPersistRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/spoond.db"

	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	sub := newTestSub()
	img := seedImage(t, db, "py-base", 2048)
	svc := NewService(sub, db, map[string]string{"t": "c"}, ServiceConfig{DefaultTTL: time.Minute, MaxTTL: 10 * time.Minute})

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.setName("c", l.ID, "my-lease"); err != nil {
		t.Fatalf("set name: %v", err)
	}
	if err := svc.GrantShare("c", l.ID, "friend", ShareHTTP, 0); err != nil {
		t.Fatalf("grant share: %v", err)
	}

	svc.Shutdown(ctx)
	// fake.Fake.CallLog() records every Delete; none may happen on shutdown.
	if got := calls(sub.Fake, "Delete"); got != 0 {
		t.Fatalf("shutdown deleted sandboxes: %d", got)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close first db: %v", err)
	}

	// A new service (new handle, same file) loads the persisted state.
	db2, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { db2.Close() })
	svc2 := NewService(sub, db2, map[string]string{"t": "c"}, ServiceConfig{DefaultTTL: time.Minute, MaxTTL: 10 * time.Minute})
	if err := svc2.LoadState(ctx); err != nil {
		t.Fatalf("load state: %v", err)
	}

	got := svc2.lookup("c", l.ID)
	if got == nil {
		t.Fatalf("lease %s not loaded", l.ID)
	}
	if got.Name != "my-lease" {
		t.Fatalf("loaded lease name = %q, want %q", got.Name, "my-lease")
	}
	if got.TemplateID != img.TemplateID {
		t.Fatalf("loaded lease template = %q, want %q", got.TemplateID, img.TemplateID)
	}
	if got.BuildID != img.CurrentBuildID {
		t.Fatalf("loaded lease build = %q, want %q", got.BuildID, img.CurrentBuildID)
	}
	if svc2.lookupByName("my-lease") == nil {
		t.Fatalf("name %q not resolvable after load", "my-lease")
	}
	if svc2.lookupWithShare("friend", l.ID, ShareHTTP) == nil {
		t.Fatalf("share for %q not loaded", "friend")
	}
}

// TestPersistSuspendedLeaseLoads: a suspended lease has no sandboxes row,
// so it loads with BuildID "" and ResumeBuildID set.
func TestPersistSuspendedLeaseLoads(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	sub := newTestSub()
	seedImage(t, db, "py-base", 2048)
	svc := NewService(sub, db, map[string]string{"t": "c"}, ServiceConfig{DefaultTTL: time.Minute, MaxTTL: 10 * time.Minute})

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.suspend(ctx, "c", l.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	wantResume := l.ResumeBuildID

	// A new service loads the suspended lease with no BuildID.
	svc2 := NewService(sub, db, map[string]string{"t": "c"}, ServiceConfig{DefaultTTL: time.Minute, MaxTTL: 10 * time.Minute})
	if err := svc2.LoadState(ctx); err != nil {
		t.Fatalf("load state: %v", err)
	}
	got := svc2.lookup("c", l.ID)
	if got == nil {
		t.Fatal("suspended lease not loaded")
	}
	if got.BuildID != "" {
		t.Fatalf("suspended lease BuildID = %q, want \"\"", got.BuildID)
	}
	if got.ResumeBuildID != wantResume {
		t.Fatalf("ResumeBuildID = %q, want %q", got.ResumeBuildID, wantResume)
	}
	if !got.Suspended {
		t.Fatal("suspended flag not restored")
	}
}

// TestPersistClassRoundTrip: a lease's class and priority (#128 part 2)
// survive a restart — grant one burst lease with a priority, shut the
// service down, and verify a new service on the same database loads
// both (an unstamped lease reads as guaranteed).
func TestPersistClassRoundTrip(t *testing.T) {
	t.Skip("TODO(FS2b-1 step 3): class behaviour removed")
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "spoond.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sub := newTestSub()
	seedImage(t, db, "py-base", 2048)
	svc := NewService(sub, db, map[string]string{"t": "c"}, ServiceConfig{DefaultTTL: time.Minute, MaxTTL: 10 * time.Minute})

	l, err := svc.grantLease(ctx, leaseRequest{owner: "c", image: "py-base", ttl: time.Minute, burst: true, priority: 7})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if l.Class != ClassBurst || l.Priority != 7 {
		t.Fatalf("granted lease class/priority = %s/%d, want burst/7", l.Class, l.Priority)
	}
	// A plain lease on the same service is guaranteed.
	plain, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("plain grant: %v", err)
	}

	svc.Shutdown(ctx)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db2, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { db2.Close() })
	svc2 := NewService(sub, db2, map[string]string{"t": "c"}, ServiceConfig{DefaultTTL: time.Minute, MaxTTL: 10 * time.Minute})
	if err := svc2.LoadState(ctx); err != nil {
		t.Fatalf("load state: %v", err)
	}
	got := svc2.lookup("c", l.ID)
	if got == nil {
		t.Fatalf("lease %s not loaded", l.ID)
	}
	if got.Class != ClassBurst || got.Priority != 7 {
		t.Fatalf("loaded lease class/priority = %s/%d, want burst/7", got.Class, got.Priority)
	}
	if p := svc2.lookup("c", plain.ID); p == nil || p.Class != ClassGuaranteed {
		t.Fatalf("plain lease class = %v, want guaranteed", p)
	}
}
