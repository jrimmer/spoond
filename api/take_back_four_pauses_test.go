package api

import (
	"context"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/metrics"
	"github.com/jrimmer/spoond/v2/substrate"
)

// An admission that needs four pauses must get all four.
func TestPreemptionTakesFourPausesWhenNeeded(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "small", 512)
	seedImage(t, db, "big", 2048)
	svc.cfg.BurstReserveMiB = 0
	svc.SetMetrics(metrics.NewBackendMetrics())
	sub.SetNodeInfo(substrate.NodeInfo{Status: "healthy", HugepagesTotal: 1 << 20, HugepageSizeBytes: 2 << 20}, nil)
	svc.cfg.TemplateStoragePath = t.TempDir()
	var diskTotal uint64 = 100 << 30
	svc.diskCapacity = func(string) (uint64, uint64, error) { return diskTotal, diskTotal, nil }
	svc.diskUsage = func(dir string) (int64, error) { return 512 << 20, nil }
	ctx := context.Background()
	installDynamicNode(t, svc, sub, 8192, 0, 256)
	owner := preemptOwner(t, svc, "heavy")
	var ls []*Lease
	for i := 0; i < 8; i++ {
		ls = append(ls, burstLease(t, svc, ctx, owner, "small"))
	}
	svc.store.mu.Lock()
	for i, l := range ls {
		l.LastActive = time.Now().Add(-time.Duration(10-i) * time.Hour)
	}
	svc.store.mu.Unlock()
	installDynamicNode(t, svc, sub, 2048, 0, 256) // full: 8 x 256 pages
	warmFairShares(svc, ctx)
	t.Logf("views: %+v", svc.takeBackOwners(ctx))
	_, err := svc.grantLease(ctx, leaseRequest{owner: "guaranteed", image: "big", ttl: time.Hour})
	n := 0
	for _, l := range ls {
		if l.Suspended {
			n++
		}
	}
	t.Logf("err=%v paused=%d", err, n)
	if err != nil {
		t.Fatalf("a 2048 MiB admission needing four 512 MiB take-backs failed: %v (paused %d)", err, n)
	}
}
