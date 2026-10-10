package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/substrate"
)

// TestForkBatchAdmissionLeavesNoTakeBackBehind pins the all-or-nothing
// memory admission of a fork batch: the fork admits count*image memory
// once, before any child exists, so a refusal cannot leave an earlier
// child's take-back victim paused.
//
// The box is full: two owners share an 8192 MiB pool, alice holds 1024
// MiB, bob holds 6144 MiB (2048 MiB over his 4096 MiB slice). A three
// child fork is 3072 MiB; take-back could free two of bob's 1024 MiB
// leases but not three (the third would drop bob below the requester),
// so the batch admission never selects a victim and the fork is
// refused with nothing paused. A two child fork on the same box is
// admitted: take-back pauses exactly one of bob's leases.
func TestForkBatchAdmissionLeavesNoTakeBackBehind(t *testing.T) {
	svc, db, sub, ids := newFairShareService(t)
	alice := addIdentityUser(t, ids, "alice")
	bob := addIdentityUser(t, ids, "bob")
	ctx := context.Background()
	seedImage(t, db, "mid", 1024)
	roomyDisk(svc)
	// Huge to start with, so the setup grants admit without pressure.
	installDynamicNode(t, svc, sub, 1<<20, 0, 512)

	src, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant alice: %v", err)
	}
	var bobLeases []*Lease
	for i := 0; i < 6; i++ {
		l, err := svc.grantLease(ctx, leaseRequest{owner: bob.ID, image: "mid", ttl: time.Hour, persistent: true})
		if err != nil {
			t.Fatalf("grant bob %d: %v", i, err)
		}
		bobLeases = append(bobLeases, l)
	}

	// 8192 MiB pool, 1024 MiB per lease: seven running leases leave
	// 1024 MiB free, and each owner's slice is 4096 MiB.
	installDynamicNode(t, svc, sub, 4096, 0, 512)
	warmFairShares(svc, ctx)

	// Three children need 3072 MiB; the take-back selector can free only
	// two leases before bob would fall below alice, so the batch is
	// refused and no lease is paused.
	_, _, err = svc.fork(ctx, alice.ID, src.ID, 3, false, time.Hour, "", "")
	if err == nil {
		t.Fatal("fork of 3 on a box that fits only 2 was not refused")
	}
	if !isBoxFull(err) && !errors.Is(err, substrate.ErrCapacity) {
		t.Fatalf("fork of 3 refused with %v, want box_full or a capacity wait", err)
	}
	for i, l := range bobLeases {
		if l.Suspended {
			t.Fatalf("refused fork paused bob's lease %d (%s): a rolled-back batch must leave no take-back behind", i, l.ID)
		}
		if l.TakeBackFor != "" || !l.TakeBackAt.IsZero() {
			t.Fatalf("refused fork stamped bob's lease %d with a take-back: %v/%q", i, l.TakeBackAt, l.TakeBackFor)
		}
	}
	if src.Suspended {
		t.Fatal("refused fork paused its own source lease")
	}

	// A two child fork (2048 MiB) is admitted on the same box: take-back
	// frees one of bob's leases for it.
	dropNodeCache(svc)
	warmFairShares(svc, ctx)
	created, _, err := svc.fork(ctx, alice.ID, src.ID, 2, false, time.Hour, "", "")
	if err != nil {
		t.Fatalf("fork of 2 on the same box: %v", err)
	}
	if len(created) != 2 {
		t.Fatalf("fork of 2 created %d leases, want 2", len(created))
	}
	paused := 0
	for _, l := range bobLeases {
		if l.Suspended {
			paused++
		}
	}
	if paused != 1 {
		t.Fatalf("fork of 2 paused %d of bob's leases, want exactly 1", paused)
	}
}
