package api

import (
	"context"
	"testing"
	"time"
)

func TestDbg19(t *testing.T) {
	svc, db, sub, ids := newFairShareService(t)
	svc.cfg.MemoryOvercommit = 1.0
	alice := addIdentityUser(t, ids, "alice")
	svc.probeEnabled = false
	ctx := context.Background()
	seedImage(t, db, "py-base", 2048)
	roomyDisk(svc)
	for i := 0; i < 4; i++ {
		fullFreeNode(t, svc, sub)
		if _, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "py-base", ttl: longHR(), persistent: true}); err != nil {
			t.Fatalf("lease %d of 4: %v", i+1, err)
		}
	}
	l := svcStoreLeases(t, svc)
	if _, err := svc.suspend(ctx, alice.ID, l[0].ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	fullFreeNode(t, svc, sub)
	svc.cfg.PoolSize = 1
	svc.warmPool(ctx, seedImage(t, db, "py-base", 2048))
	_, _ = svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "py-base", ttl: longHR(), persistent: true})
}

func longHR() time.Duration { return time.Hour }
