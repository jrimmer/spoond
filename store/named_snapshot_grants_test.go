package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestNamedSnapshotGrants: a grant is idempotent (the repeat keeps the
// first created_at), lists by giver and by receiver, deletes once
// (ErrNotFound after), and a user delete drops what it gave and what it
// received while leaving unrelated grants (#141).
func TestNamedSnapshotGrants(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	t0 := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

	first, err := db.InsertNamedSnapshotGrant(ctx, NamedSnapshotGrantRow{Owner: "pool", Name: "honey/warm", Grantee: "honey", CreatedAt: t0})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	again, err := db.InsertNamedSnapshotGrant(ctx, NamedSnapshotGrantRow{Owner: "pool", Name: "honey/warm", Grantee: "honey", CreatedAt: t0.Add(time.Hour)})
	if err != nil || !again.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("repeat grant = %+v (%v), want the first row (created %v)", again, err, t0)
	}
	for _, g := range []NamedSnapshotGrantRow{
		{Owner: "pool", Name: "other", Grantee: "carol", CreatedAt: t0},
		{Owner: "honey", Name: "mine", Grantee: "pool", CreatedAt: t0},
		{Owner: "dave", Name: "x", Grantee: "carol", CreatedAt: t0},
	} {
		if _, err := db.InsertNamedSnapshotGrant(ctx, g); err != nil {
			t.Fatalf("grant %+v: %v", g, err)
		}
	}
	given, _ := db.NamedSnapshotGrantsGiven(ctx, "pool")
	if len(given) != 2 || given[0].Name != "honey/warm" || given[1].Name != "other" {
		t.Fatalf("given by pool = %+v", given)
	}
	received, _ := db.NamedSnapshotGrantsReceived(ctx, "honey")
	if len(received) != 1 || received[0].Owner != "pool" || received[0].Name != "honey/warm" {
		t.Fatalf("received by honey = %+v", received)
	}

	if err := db.DeleteNamedSnapshotGrant(ctx, "pool", "other", "carol"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := db.DeleteNamedSnapshotGrant(ctx, "pool", "other", "carol"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second revoke = %v, want ErrNotFound", err)
	}

	if err := db.DeleteNamedSnapshotGrantsOfUser(ctx, "honey"); err != nil {
		t.Fatalf("delete grants of honey: %v", err)
	}
	if _, err := db.GetNamedSnapshotGrant(ctx, "pool", "honey/warm", "honey"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("grant honey received survived the user delete: %v", err)
	}
	if _, err := db.GetNamedSnapshotGrant(ctx, "honey", "mine", "pool"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("grant honey gave survived the user delete: %v", err)
	}
	if _, err := db.GetNamedSnapshotGrant(ctx, "dave", "x", "carol"); err != nil {
		t.Fatalf("unrelated grant was deleted: %v", err)
	}
}
