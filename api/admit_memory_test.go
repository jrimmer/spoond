package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v4/substrate"
)

// Tests for the admission undo (spoond-ljv2): a create or resume that
// fails after admitMemory debited the cached NodeInfo must give the
// debit back, or for the rest of the cache TTL fits() reads less free
// memory than there is and take-back may pause a lease for nothing.

// cachedFreeMiB reads the service's cached free hugepage memory in MiB.
// The cache is kept warm across a test by the static fake NodeInfo, so
// the value reflects the admission debits and credits only.
func cachedFreeMiB(t *testing.T, svc *Service, ctx context.Context) uint64 {
	t.Helper()
	free, err := svc.cachedFreeHugepageMiB(ctx)
	if err != nil {
		t.Fatalf("cached free hugepage MiB: %v", err)
	}
	return free
}

// admitTestService builds a service over a fake with a healthy 8 GiB
// hugepage node and the py-base image.
func admitTestService(t *testing.T) (*Service, context.Context) {
	t.Helper()
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	sub.SetNodeInfo(substrate.NodeInfo{
		Status:            "healthy",
		HugepagesTotal:    1 << 12,
		HugepageSizeBytes: 2 << 20,
	}, nil)
	return svc, context.Background()
}

// TestAdmitMemoryFailedCreateCreditsBack: a create whose sandbox
// creation fails leaves the cached free memory exactly as it was before
// the attempt — the admission debit is undone. Mutation: drop the undo
// on grantLease's failure paths, which leaves the 2048 MiB debit in the
// cache until the next refresh.
func TestAdmitMemoryFailedCreateCreditsBack(t *testing.T) {
	svc, ctx := admitTestService(t)
	before := cachedFreeMiB(t, svc, ctx)

	svc.sub.(*testSub).createFn = func(context.Context, substrate.CreateRequest) (substrate.Sandbox, error) {
		return substrate.Sandbox{}, errors.New("substrate create failed")
	}
	if _, err := svc.grantLease(ctx, leaseRequest{owner: "consumer-a", image: "py-base", ttl: time.Hour}); err == nil {
		t.Fatal("grant with a failing create returned no error")
	}
	if after := cachedFreeMiB(t, svc, ctx); after != before {
		t.Fatalf("cached free memory after a failed create = %d MiB, want %d MiB (the debit was not credited back)", after, before)
	}
}

// TestAdmitMemoryUndoCreditsOnce: undo is safe to call more than once;
// only the first call credits. Mutation: credit unconditionally, which
// would over-credit twice the admitted memory.
func TestAdmitMemoryUndoCreditsOnce(t *testing.T) {
	svc, ctx := admitTestService(t)
	before := cachedFreeMiB(t, svc, ctx)

	undo, err := svc.admitMemory(ctx, "consumer-a", 1024)
	if err != nil {
		t.Fatalf("admitMemory: %v", err)
	}
	if got := cachedFreeMiB(t, svc, ctx); got != before-1024 {
		t.Fatalf("cached free memory after admission = %d MiB, want %d MiB", got, before-1024)
	}
	undo(true)
	undo(true)
	if got := cachedFreeMiB(t, svc, ctx); got != before {
		t.Fatalf("cached free memory after undo twice = %d MiB, want %d MiB (undo credited more than once)", got, before)
	}
}

// TestAdmitMemorySuccessfulCreateKeepsDebit: a successful create keeps
// its debit, so the next admission inside the cache window sees the
// memory gone. Mutation: undo on success too, which would let concurrent
// creates overcommit the box.
func TestAdmitMemorySuccessfulCreateKeepsDebit(t *testing.T) {
	svc, ctx := admitTestService(t)
	before := cachedFreeMiB(t, svc, ctx)

	if _, err := svc.grantLease(ctx, leaseRequest{owner: "consumer-a", image: "py-base", ttl: time.Hour}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if after := cachedFreeMiB(t, svc, ctx); after != before-2048 {
		t.Fatalf("cached free memory after a successful create = %d MiB, want %d MiB (the debit must stay)", after, before-2048)
	}
}
