package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
	"github.com/jrimmer/spoond/v2/substrate/e2b"
)

// Pause-chain measurement (spoond-p9j): buildChainStats walks a lease's
// chain, GET /api/leases/{id} reports its depth and bytes, and each
// pause observes them in the bounded-cardinality histograms.

// TestBuildChainStats pins the walk: it follows parent_build_id from the
// head, stops at a missing or deleted build, sums size_bytes and is
// cycle-safe.
func TestBuildChainStats(t *testing.T) {
	builds := map[string]store.BuildRow{
		"c2": {BuildID: "c2", ParentBuildID: "c1", State: "ready", SizeBytes: 30},
		"c1": {BuildID: "c1", ParentBuildID: "t", State: "ready", SizeBytes: 20},
		"t":  {BuildID: "t", ParentBuildID: "", State: "ready", SizeBytes: 10},
	}
	if depth, bytes := buildChainStats(builds, "c2"); depth != 3 || bytes != 60 {
		t.Fatalf("ready chain: depth=%d bytes=%d, want 3, 60", depth, bytes)
	}
	// An empty head is an empty chain.
	if depth, bytes := buildChainStats(builds, ""); depth != 0 || bytes != 0 {
		t.Fatalf("empty head: depth=%d bytes=%d, want 0, 0", depth, bytes)
	}
	// A missing head is an empty chain (nothing to measure).
	if depth, bytes := buildChainStats(builds, "gone"); depth != 0 || bytes != 0 {
		t.Fatalf("missing head: depth=%d bytes=%d, want 0, 0", depth, bytes)
	}

	// A deleted build and its ancestors are no longer part of the live
	// chain: the GC would have removed their files, so the walk stops.
	builds["c1"] = store.BuildRow{BuildID: "c1", ParentBuildID: "t", State: "deleted", SizeBytes: 20}
	if depth, bytes := buildChainStats(builds, "c2"); depth != 1 || bytes != 30 {
		t.Fatalf("deleted middle: depth=%d bytes=%d, want 1, 30", depth, bytes)
	}

	// A cycle terminates instead of looping forever.
	builds["c1"] = store.BuildRow{BuildID: "c1", ParentBuildID: "c2", State: "ready", SizeBytes: 20}
	if depth, bytes := buildChainStats(builds, "c2"); depth != 2 || bytes != 50 {
		t.Fatalf("cycle: depth=%d bytes=%d, want 2, 50", depth, bytes)
	}
}

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
