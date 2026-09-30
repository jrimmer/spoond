package api

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/jrimmer/spoond/store"
)

// TestPersistRoundTrip grants a lease, names it and shares it on a
// service backed by a SQLite store, then verifies a NEW service on the
// same database loads the lease, its name and the share. Shutdown must
// not kill anything: state survives the backend (U05).
func TestPersistRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "spoond.db")

	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"t": "c"}, 0, time.Minute, 10*time.Minute)
	svc.log = log.New(io.Discard, "", 0)
	svc.SetDB(db)

	l, err := svc.grant(ctx, "c", "py-base", 0, time.Minute, false, "", nil)
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
	// fakeForkd.killed records Kill AND DeleteWorkspace calls; neither
	// may happen on shutdown.
	if len(ff.killed) != 0 {
		t.Fatalf("shutdown killed sandboxes/workspaces: %v", ff.killed)
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
	svc2 := NewService(ff, map[string]string{"t": "c"}, 0, time.Minute, 10*time.Minute)
	svc2.log = log.New(io.Discard, "", 0)
	svc2.SetDB(db2)
	if err := svc2.LoadState(ctx); err != nil {
		t.Fatalf("load state: %v", err)
	}

	if got := svc2.lookup("c", l.ID); got == nil {
		t.Fatalf("lease %s not loaded", l.ID)
	} else if got.Name != "my-lease" {
		t.Fatalf("loaded lease name = %q, want %q", got.Name, "my-lease")
	}
	if svc2.lookupByName("my-lease") == nil {
		t.Fatalf("name %q not resolvable after load", "my-lease")
	}
	if svc2.lookupWithShare("friend", l.ID, ShareHTTP) == nil {
		t.Fatalf("share for %q not loaded", "friend")
	}
}
