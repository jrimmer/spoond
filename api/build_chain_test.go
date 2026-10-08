package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/metrics"
	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
	"github.com/jrimmer/spoond/v2/substrate/e2b"
)

// Pause-chain measurement (spoond-p9j): store.BuildChain walks a lease's
// chain, GET /api/leases/{id} reports its depth and bytes, and each
// pause observes them in the bounded-cardinality histograms.

// chainTestServer returns a server whose pauses leave real build files
// under a temp storage root, so the chain's recorded bytes are nonzero.
func chainTestServer(t *testing.T) (*Service, *store.DB, *testSub) {
	t.Helper()
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	root := t.TempDir()
	svc.cfg.TemplateStoragePath = root
	sub.pauseFn = func(ctx context.Context, sandboxID, templateID string) (string, substrate.BuildRefs, error) {
		id := e2b.NewUUID()
		if _, err := writeBuildDir(filepath.Join(root, id), 4096, 8192); err != nil {
			return "", substrate.BuildRefs{}, err
		}
		return id, substrate.BuildRefs{}, nil
	}
	t.Cleanup(func() { sub.pauseFn = nil })
	return svc, db, sub
}

// TestLeaseDetailPauseChain: a persistent lease suspended, resumed and
// suspended again accumulates a pause chain; GET /api/leases/{id} reports
// its depth and summed recorded bytes. The numbers follow the build the
// lease is suspended into (resume_build_id), so the depth grows by one
// per pause.
func TestLeaseDetailPauseChain(t *testing.T) {
	svc, _, _ := chainTestServer(t)
	ctx := context.Background()

	l, err := svc.grant(ctx, "consumer-a", "py-base", 0, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}

	// First pause: new build → template, two builds.
	if _, err := svc.suspend(ctx, "consumer-a", l.ID); err != nil {
		t.Fatalf("suspend 1: %v", err)
	}
	depth, bytes, err := svc.leaseChainStats(ctx, l)
	if err != nil {
		t.Fatalf("chain stats 1: %v", err)
	}
	if depth != 2 {
		t.Fatalf("first pause chain depth = %d, want 2 (pause + template)", depth)
	}
	if bytes <= 0 {
		t.Fatalf("first pause chain bytes = %d, want the pause build's recorded size", bytes)
	}

	// Resume then suspend again: the second pause's parent is the first
	// pause build, so the chain is three deep.
	if _, err := svc.resume(ctx, "consumer-a", l.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, err := svc.suspend(ctx, "consumer-a", l.ID); err != nil {
		t.Fatalf("suspend 2: %v", err)
	}
	detail := svc.leaseDetailMap(l)
	if got := detail["chain_depth"]; got != 3 {
		t.Fatalf("chain_depth = %v, want 3", got)
	}
	if got, ok := detail["chain_bytes"].(int64); ok {
		if got <= 0 {
			t.Fatalf("chain_bytes = %d, want a positive sum", got)
		}
	} else {
		t.Fatalf("chain_bytes = %v (%T), want an int64", detail["chain_bytes"], detail["chain_bytes"])
	}
}

// TestLeaseDetailPauseChainHTTP exercises the chain fields through the
// real GET /api/leases/{id} route, so the JSON shape a client sees is
// pinned (int64 depth and bytes, not a float or missing field).
func TestLeaseDetailPauseChainHTTP(t *testing.T) {
	svc, db, _ := chainTestServer(t)
	srv := NewServer(svc, NewImageRegistry(db))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	ctx := context.Background()

	l, err := svc.grant(ctx, "consumer-a", "py-base", 0, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.suspend(ctx, "consumer-a", l.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	resp, body := doReq(t, "GET", ts.URL+"/api/leases/"+l.ID, "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get lease: status %d: %v", resp.StatusCode, body)
	}
	if got := body["chain_depth"]; got != float64(2) {
		t.Fatalf("chain_depth = %v, want 2", got)
	}
	if got, ok := body["chain_bytes"].(float64); !ok || got <= 0 {
		t.Fatalf("chain_bytes = %v (%T), want a positive number", body["chain_bytes"], body["chain_bytes"])
	}
}

// TestPauseChainMetrics: each pause observes the chain depth and bytes in
// the unlabeled histograms, so the cardinality stays bounded. The counts
// grow by one per pause; a fresh server has none.
func TestPauseChainMetrics(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)
	root := t.TempDir()
	svc.cfg.TemplateStoragePath = root

	if n, ok := metricSample(t, ts, "token-a", "spoond_pause_chain_depth_count"); !ok || n != 0 {
		t.Fatalf("fresh: spoond_pause_chain_depth_count = %v (present %v), want 0", n, ok)
	}

	ctx := context.Background()
	l, err := svc.grant(ctx, "consumer-a", "py-base", 0, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.suspend(ctx, "consumer-a", l.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	if n, ok := metricSample(t, ts, "token-a", "spoond_pause_chain_depth_count"); !ok || n != 1 {
		t.Fatalf("after one pause: spoond_pause_chain_depth_count = %v (present %v), want 1", n, ok)
	}
	if n, ok := metricSample(t, ts, "token-a", "spoond_pause_chain_bytes_count"); !ok || n != 1 {
		t.Fatalf("after one pause: spoond_pause_chain_bytes_count = %v (present %v), want 1", n, ok)
	}
}

// TestLeaseDetailChainOmitsWhenUnmeasurable: a lease with no build (a
// persisted suspended lease loaded before its first pause) omits the
// chain fields rather than reporting a misleading zero.
func TestLeaseDetailChainOmitsWhenUnmeasurable(t *testing.T) {
	svc, _, _ := chainTestServer(t)
	l := &Lease{ID: "l-empty", Owner: "consumer-a", Image: "py-base"}
	detail := svc.leaseDetailMap(l)
	if _, ok := detail["chain_depth"]; ok {
		t.Fatalf("chain_depth = %v, want the field omitted for a lease with no build", detail["chain_depth"])
	}
	if _, ok := detail["chain_bytes"]; ok {
		t.Fatalf("chain_bytes = %v, want the field omitted for a lease with no build", detail["chain_bytes"])
	}
}

// histogramCountAndSum reads one histogram family's sample count and sum
// from the service's metrics registry.
func histogramCountAndSum(t *testing.T, svc *Service, name string) (uint64, float64) {
	t.Helper()
	mfs, err := svc.metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		if len(mf.GetMetric()) == 0 {
			break
		}
		h := mf.GetMetric()[0].GetHistogram()
		return h.GetSampleCount(), h.GetSampleSum()
	}
	t.Fatalf("histogram %s not found", name)
	return 0, 0
}

// TestPauseChainObservedAfterSettle: the pause-chain bytes histogram is
// observed from the settle path, not at insert time, so it includes the
// pause build's memory snapshot when that lands (and commits on ZFS)
// only after Pause returns. Here the memfile is written after the build
// row is inserted; the observation must carry the settled size, not the
// header-only reading taken at insert.
func TestPauseChainObservedAfterSettle(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	svc.SetMetrics(metrics.NewBackendMetrics())
	root := t.TempDir()
	svc.cfg.TemplateStoragePath = root
	svc.SetBuildSizeSettle(5*time.Millisecond, 3*time.Second)
	svc.sizeSettleQuiet = 30 * time.Millisecond

	var dir string
	sub.pauseFn = func(ctx context.Context, sandboxID, templateID string) (string, substrate.BuildRefs, error) {
		id := e2b.NewUUID()
		d := filepath.Join(root, id)
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", substrate.BuildRefs{}, err
		}
		// Headers only: the memory file has not landed when Pause returns.
		if err := os.WriteFile(filepath.Join(d, "headers"), make([]byte, 64000), 0o644); err != nil {
			return "", substrate.BuildRefs{}, err
		}
		dir = d
		// The snapshot write finishes after the row is inserted.
		time.AfterFunc(30*time.Millisecond, func() {
			_ = os.WriteFile(filepath.Join(d, "memfile"), make([]byte, 1<<20), 0o644)
		})
		return id, substrate.BuildRefs{}, nil
	}
	t.Cleanup(func() { sub.pauseFn = nil })

	ctx := context.Background()
	l, err := svc.grant(ctx, "consumer-a", "py-base", 0, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.suspend(ctx, "consumer-a", l.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		n, sum := histogramCountAndSum(t, svc, "spoond_pause_chain_bytes")
		if n == 1 {
			want, err := store.BuildDiskUsage(dir)
			if err != nil {
				t.Fatalf("measure settled dir: %v", err)
			}
			if int64(sum) != want {
				t.Fatalf("pause_chain_bytes observed %d, want the settled chain size %d", int64(sum), want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pause_chain_bytes sample count = %d, want 1", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
