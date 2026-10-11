package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v3/substrate"
)

// TestBiggestBorrowerRefusal: the requester is refused only when, after
// its request, it is over its slice and no other owner's used/slice is
// higher.
func TestBiggestBorrowerRefusal(t *testing.T) {
	cases := []struct {
		name    string
		owners  []takeBackOwner
		request int
		refuse  bool
	}{
		{"borrows nothing yet", []takeBackOwner{
			{Owner: "req", UsedMiB: 0, SliceMiB: 1024},
			{Owner: "b", UsedMiB: 0, SliceMiB: 1024}}, 2048, false},
		{"inside its slice after the request", []takeBackOwner{
			{Owner: "req", UsedMiB: 512, SliceMiB: 2048},
			{Owner: "b", UsedMiB: 0, SliceMiB: 2048}}, 1024, false},
		{"over its slice, nobody borrows more", []takeBackOwner{
			{Owner: "req", UsedMiB: 4096, SliceMiB: 2048},
			{Owner: "b", UsedMiB: 0, SliceMiB: 2048}}, 1024, true},
		{"over its slice, another borrows more", []takeBackOwner{
			{Owner: "req", UsedMiB: 2048, SliceMiB: 2048},
			{Owner: "b", UsedMiB: 6144, SliceMiB: 2048}}, 1024, false},
		{"equal ratio counts as biggest", []takeBackOwner{
			{Owner: "req", UsedMiB: 2048, SliceMiB: 2048},
			{Owner: "b", UsedMiB: 3072, SliceMiB: 2048}}, 1024, true},
		{"lone owner over the whole box", []takeBackOwner{
			{Owner: "req", UsedMiB: 4096, SliceMiB: 4096}}, 1024, true},
		{"requester unknown to the snapshot", []takeBackOwner{
			{Owner: "b", UsedMiB: 0, SliceMiB: 2048}}, 1024, false},
		{"requester without a slice", []takeBackOwner{
			{Owner: "req", UsedMiB: 4096, SliceMiB: 0}}, 1024, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := biggestBorrowerRefusal(c.owners, "req", c.request)
			if (err != nil) != c.refuse {
				t.Fatalf("refusal = %v, want refuse=%v", err, c.refuse)
			}
			if err != nil && !errors.Is(err, errBoxFull) {
				t.Fatalf("refusal %v does not unwrap to errBoxFull", err)
			}
		})
	}
}

// TestBoxFullBiggestBorrowerEveryPath: an owner that fills the box with
// its own leases is the biggest borrower, so its next admission on every
// path that re-admits memory — create, resume, warm restart, clone,
// fork — is 429 box_full at once: nothing is paused and nothing waits.
// Another owner's create on the same full box is served by take-back.
func TestBoxFullBiggestBorrowerEveryPath(t *testing.T) {
	svc, db, sub, ids := newFairShareService(t)
	alice := addIdentityUser(t, ids, "alice")
	bob := addIdentityUser(t, ids, "bob")
	ctx := context.Background()
	seedImage(t, db, "mid", 1024)
	roomyDisk(svc)
	// 4096 MiB box, each sandbox 1024 MiB: two 2048 MiB slices.
	installDynamicNode(t, svc, sub, 2048, 0, 512)

	var al []*Lease
	for i := 0; i < 4; i++ {
		l, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "mid", ttl: time.Hour, persistent: true})
		if err != nil {
			t.Fatalf("grant alice %d: %v", i, err)
		}
		al = append(al, l)
	}
	// One suspended alice lease for the resume and restart paths; a
	// fifth lease refills the box behind it.
	if _, err := svc.suspend(ctx, alice.ID, al[3].ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	dropNodeCache(svc)
	warmFairShares(svc, ctx)
	if _, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "mid", ttl: time.Hour, persistent: true}); err != nil {
		t.Fatalf("refill: %v", err)
	}

	paths := []struct {
		name string
		do   func() error
	}{
		{"create", func() error {
			_, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "mid", ttl: time.Hour})
			return err
		}},
		{"resume", func() error { _, err := svc.resume(ctx, alice.ID, al[3].ID); return err }},
		{"restart", func() error { _, err := svc.restart(ctx, alice.ID, al[3].ID, "warm"); return err }},
		{"clone", func() error { _, _, err := svc.clone(ctx, alice.ID, al[0].ID); return err }},
		{"fork", func() error {
			_, _, err := svc.fork(ctx, alice.ID, al[0].ID, 1, false, time.Hour, "", "")
			return err
		}},
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			dropNodeCache(svc)
			warmFairShares(svc, ctx)
			err := p.do()
			if !isBoxFull(err) {
				t.Fatalf("%s by the biggest borrower = %v, want box_full", p.name, err)
			}
			if !strings.Contains(err.Error(), "no other owner borrows more") {
				t.Fatalf("%s refusal %q should say the requester is the biggest borrower", p.name, err)
			}
			for _, l := range al[:3] {
				if l.Suspended {
					t.Fatalf("%s paused lease %s for its own owner", p.name, l.ID)
				}
			}
		})
	}

	// Bob is inside his slice: take-back pauses one of alice's leases
	// and his create succeeds.
	dropNodeCache(svc)
	warmFairShares(svc, ctx)
	if _, err := svc.grantLease(ctx, leaseRequest{owner: bob.ID, image: "mid", ttl: time.Hour}); err != nil {
		t.Fatalf("bob's create on alice's full box: %v", err)
	}
}

// TestBoxFullCountsOnlyTakeableOwners: the pinned box_full pre-check
// counts only the leases of owners take-back may take from — other
// owners over their slice and further over it than the requester would
// be. The requester's own leases and an inside-slice owner's pinned
// leases do not decide it.
func TestBoxFullCountsOnlyTakeableOwners(t *testing.T) {
	t.Run("over-slice owner all pinned is box_full", func(t *testing.T) {
		svc, db, sub, ids := newFairShareService(t)
		alice := addIdentityUser(t, ids, "alice")
		bob := addIdentityUser(t, ids, "bob")
		ctx := context.Background()
		seedImage(t, db, "mid", 1024)
		roomyDisk(svc)
		installDynamicNode(t, svc, sub, 2048, 0, 512) // 4096 MiB, two 2048 MiB slices
		for i := 0; i < 3; i++ {
			l, err := svc.grantLease(ctx, leaseRequest{owner: bob.ID, image: "mid", ttl: time.Hour})
			if err != nil {
				t.Fatalf("grant bob %d: %v", i, err)
			}
			if _, err := svc.setPinned(bob.ID, l.ID, true); err != nil {
				t.Fatalf("pin: %v", err)
			}
		}
		a, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "mid", ttl: time.Hour})
		if err != nil {
			t.Fatalf("grant alice: %v", err)
		}
		dropNodeCache(svc)
		warmFairShares(svc, ctx)
		_, err = svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "mid", ttl: time.Hour})
		if !isBoxFull(err) {
			t.Fatalf("alice's create with only bob's pinned leases to take = %v, want box_full", err)
		}
		if a.Suspended {
			t.Fatal("the requester's own lease was paused")
		}
	})
	t.Run("inside-slice owner's pins are a wait", func(t *testing.T) {
		svc, db, sub, ids := newFairShareService(t)
		alice := addIdentityUser(t, ids, "alice")
		bob := addIdentityUser(t, ids, "bob")
		ctx := context.Background()
		seedImage(t, db, "mid", 1024)
		roomyDisk(svc)
		installDynamicNode(t, svc, sub, 2048, 0, 512)
		l, err := svc.grantLease(ctx, leaseRequest{owner: bob.ID, image: "mid", ttl: time.Hour})
		if err != nil {
			t.Fatalf("grant bob: %v", err)
		}
		if _, err := svc.setPinned(bob.ID, l.ID, true); err != nil {
			t.Fatalf("pin: %v", err)
		}
		// The other 3072 MiB are held outside spoond.
		installDynamicNode(t, svc, sub, 2048, 1536, 512)
		warmFairShares(svc, ctx)
		_, err = svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "mid", ttl: time.Hour})
		if isBoxFull(err) || !errors.Is(err, substrate.ErrCapacity) {
			t.Fatalf("create blocked by outside memory and an inside-slice pin = %v, want a capacity wait", err)
		}
	})
}

// TestTakeBackCountsInFlightMemory: an owner's admitted creates that are
// still in flight (store.pendingMiB) count toward its usage, so it
// cannot read a falsely low ratio and take back from an owner who is in
// fact no further over the slice than it is. Box 5120 MiB, two 2560 MiB
// slices: bob runs 4096 (1.6); alice runs 1024 with 2048 more in flight
// and asks for 1024 (its own reservation is pending too). Counted
// honestly alice would sit at 1.6 after the request, so bob is not
// taken from.
func TestTakeBackCountsInFlightMemory(t *testing.T) {
	svc, db, sub, ids := newFairShareService(t)
	alice := addIdentityUser(t, ids, "alice")
	bob := addIdentityUser(t, ids, "bob")
	ctx := context.Background()
	seedImage(t, db, "mid", 1024)
	roomyDisk(svc)
	installDynamicNode(t, svc, sub, 2560, 0, 512)
	var bl []*Lease
	for i := 0; i < 4; i++ {
		l, err := svc.grantLease(ctx, leaseRequest{owner: bob.ID, image: "mid", ttl: time.Hour, persistent: true})
		if err != nil {
			t.Fatalf("grant bob %d: %v", i, err)
		}
		bl = append(bl, l)
	}
	if _, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "mid", ttl: time.Hour}); err != nil {
		t.Fatalf("grant alice: %v", err)
	}
	dropNodeCache(svc)
	warmFairShares(svc, ctx)
	svc.store.mu.Lock()
	svc.store.pendingMiB[alice.ID] = 3072 // 2048 in flight + this request's 1024
	svc.store.mu.Unlock()

	undo, err := svc.admitMemory(ctx, alice.ID, 1024)
	if err == nil {
		undo()
	}
	for _, l := range bl {
		if l.Suspended {
			t.Fatalf("bob's lease %s was taken back for an owner whose in-flight memory puts it as far over its slice", l.ID)
		}
	}
}
