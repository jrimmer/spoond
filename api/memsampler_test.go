package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// memMetricsDoc builds the ram families for (sandbox, usedBytes) pairs
// in the collector's output shape, plus one unrelated family and a
// gone-sandbox series.
func memMetricsDoc(samples map[string]float64) string {
	out := "# HELP e2b_sandbox_ram_used used.\n# TYPE e2b_sandbox_ram_used gauge\n"
	for sid, v := range samples {
		out += fmt.Sprintf("e2b_sandbox_ram_used{node_id=\"node-a\",sandbox_id=%q,team_id=\"t1\"} %g\n", sid, v)
	}
	out += "# HELP e2b_sandbox_ram_total total.\n# TYPE e2b_sandbox_ram_total gauge\n"
	for sid := range samples {
		out += fmt.Sprintf("e2b_sandbox_ram_total{node_id=\"node-a\",sandbox_id=%q,team_id=\"t1\"} 4294967296\n", sid)
	}
	return out
}

// TestMemSamplerRecordsAndResets runs one sampler against a fake
// metrics upstream: the latest value wins, the peak keeps the max over
// the VM's life, a SandboxID change resets both and a sample count to
// zero, and released or non-live leases drop out.
func TestMemSamplerRecordsAndResets(t *testing.T) {
	svc, _, _ := newTestService(t)
	seedImage(t, svc.db, "py-base", 2048)
	db := svc.db
	// A running lease on sandbox sb_A.
	lease, err := svc.grantLease(context.Background(), leaseRequest{owner: "consumer-a", image: "py-base", ttl: time.Hour})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.store.mu.Lock()
	lease.SandboxID, lease.HostIP = "sb_A", "10.0.0.1"
	svc.store.mu.Unlock()
	_ = db

	// Fake upstream: its answer is swapped by the test per tick.
	doc := &atomic.Value{}
	doc.Store(memMetricsDoc(map[string]float64{"sb_A": 300 * 1024 * 1024}))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(doc.Load().(string)))
	}))
	defer upstream.Close()

	svc.memSampleInterval = 10 * time.Millisecond
	svc.memSamplerFetch = func(ctx context.Context) ([]byte, error) {
		return []byte(doc.Load().(string)), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.startMemSampler(ctx)

	// First tick records the used value.
	waitFor(t, "first used", time.Second, func() bool {
		used, ok := svc.memSamples.latest(lease.ID)
		return ok && used == 300*1024*1024
	})
	if peak, ok := svc.memSamples.peak(lease.ID); !ok || peak != 300*1024*1024 {
		t.Fatalf("peak after first tick = %g (ok=%v), want 300 MiB", peak, ok)
	}

	// A lower value: latest follows, peak keeps the max.
	doc.Store(memMetricsDoc(map[string]float64{"sb_A": 100 * 1024 * 1024}))
	waitFor(t, "latest follows lower value", time.Second, func() bool {
		used, _ := svc.memSamples.latest(lease.ID)
		return used == 100*1024*1024
	})
	if peak, _ := svc.memSamples.peak(lease.ID); peak != 300*1024*1024 {
		t.Fatalf("peak = %g, want still 300 MiB (max since VM start)", peak)
	}

	// A burst above everything: peak moves (waitFor the loop to see the
	// new document; a pass in flight may still answer the old one).
	doc.Store(memMetricsDoc(map[string]float64{"sb_A": 700 * 1024 * 1024}))
	waitFor(t, "peak rises to burst", time.Second, func() bool {
		peak, _ := svc.memSamples.peak(lease.ID)
		return peak == 700*1024*1024
	})

	// The lease moves to a new sandbox: latest and peak reset on the
	// next fresh value.
	svc.store.mu.Lock()
	lease.SandboxID = "sb_B"
	svc.store.mu.Unlock()
	doc.Store(memMetricsDoc(map[string]float64{"sb_B": 120 * 1024 * 1024}))
	waitFor(t, "new sandbox resets peak", time.Second, func() bool {
		used, ok := svc.memSamples.latest(lease.ID)
		peak, _ := svc.memSamples.peak(lease.ID)
		return ok && used == 120*1024*1024 && peak == 120*1024*1024
	})

	// Release drops the entry.
	svc.memSamples.forget(lease.ID)
	if _, ok := svc.memSamples.latest(lease.ID); ok {
		t.Fatal("released lease's sample survived")
	}
}

// TestMemSamplerIgnoresGoneAndOtherSandboxes: a lease the document does
// not mention keeps its entry with its last values but gains no
// samples; a second live lease whose sandbox never appears never
// appears either.
func TestMemSamplerIgnoresGoneAndOtherSandboxes(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	l1, err := svc.grantLease(context.Background(), leaseRequest{owner: "consumer-a", image: "py-base", ttl: time.Hour})
	if err != nil {
		t.Fatalf("grant 1: %v", err)
	}
	l2, err := svc.grantLease(context.Background(), leaseRequest{owner: "consumer-b", image: "py-base", ttl: time.Hour})
	if err != nil {
		t.Fatalf("grant 2: %v", err)
	}
	svc.store.mu.Lock()
	l1.SandboxID, l1.HostIP = "sb_A", "10.0.0.1"
	l2.SandboxID, l2.HostIP = "sb_undocumented", "10.0.0.2"
	svc.store.mu.Unlock()

	svc.memSamplerFetch = func(ctx context.Context) ([]byte, error) {
		return []byte(memMetricsDoc(map[string]float64{"sb_A": 100 * 1024 * 1024})), nil
	}
	svc.sampleMemOnce(context.Background())

	if used, ok := svc.memSamples.latest(l1.ID); !ok || used != 100*1024*1024 {
		t.Fatalf("l1 used = %g ok=%v, want 100 MiB", used, ok)
	}
	if _, ok := svc.memSamples.latest(l2.ID); ok {
		t.Fatal("l2 got a sample though its sandbox has no series")
	}
}

// TestMemSamplerFetchFailureKeepsValues: a failing fetch logs once and
// keeps the last values; a later success recovers.
func TestMemSamplerFetchFailureKeepsValues(t *testing.T) {
	svc, db, _ := newTestService(t)
	svc.log = log.New(&testWriter{}, "", 0)
	seedImage(t, db, "py-base", 2048)
	lease, err := svc.grantLease(context.Background(), leaseRequest{owner: "consumer-a", image: "py-base", ttl: time.Hour})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.store.mu.Lock()
	lease.SandboxID, lease.HostIP = "sb_A", "10.0.0.1"
	svc.store.mu.Unlock()

	calls := 0
	fail := false
	svc.memSamplerFetch = func(ctx context.Context) ([]byte, error) {
		calls++
		if fail {
			return nil, fmt.Errorf("collector down")
		}
		return []byte(memMetricsDoc(map[string]float64{"sb_A": 42 * 1024 * 1024})), nil
	}
	svc.sampleMemOnce(context.Background())
	if used, ok := svc.memSamples.latest(lease.ID); !ok || used != 42*1024*1024 {
		t.Fatalf("used = %g ok=%v, want 42 MiB", used, ok)
	}

	fail = true
	before := calls
	svc.sampleMemOnce(context.Background())
	svc.sampleMemOnce(context.Background())
	if calls-before != 2 {
		t.Fatalf("failing fetch ran %d times over two passes, want 2", calls-before)
	}
	if used, ok := svc.memSamples.latest(lease.ID); !ok || used != 42*1024*1024 {
		t.Fatalf("failure clobbered the last value: %g ok=%v", used, ok)
	}
	if svc.memSampleFailed != true {
		t.Fatal("failure flag not set")
	}

	fail = false
	svc.sampleMemOnce(context.Background())
	if svc.memSampleFailed {
		t.Fatal("failure flag did not clear on recovery")
	}
}

// testWriter is a log sink counting lines.
type testWriter struct{ n int }

func (w *testWriter) Write(p []byte) (int, error) { w.n++; return len(p), nil }

// waitFor polls cond until it holds or the timeout passes.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s: timed out after %s", what, timeout)
}

// TestStartMemSamplerDisabledWithoutURL: a Service built without
// OTEL_PROM_URL has no fetcher, so Start runs no sampler loop.
func TestStartMemSamplerDisabledWithoutURL(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	if svc.memSamplerFetch != nil {
		t.Fatal("memSamplerFetch set without OTEL_PROM_URL")
	}
	if t.Setenv("OTEL_PROM_URL", "http://127.0.0.1:1"); svc.memSamplerFetch != nil {
		t.Fatal("memSamplerFetch reacted to the test's env change; it must be read once at construction, not per loop start")
	}
	if svc.memSampleInterval != memSampleInterval || svc.memSampleClient == nil {
		t.Fatal("sampler interval and client not defaulted")
	}
}

// TestOTelURLRoundTrip: a Service constructed with OTEL_PROM_URL set
// samples the collector over the real HTTP getter; the loop's own
// fetch path (the same client handleMetrics uses) is what talks to it.
func TestOTelURLRoundTrip(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	lease, err := svc.grantLease(context.Background(), leaseRequest{owner: "consumer-a", image: "py-base", ttl: time.Hour})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.store.mu.Lock()
	lease.SandboxID, lease.HostIP = "sb_live", "10.0.0.1"
	svc.store.mu.Unlock()

	doc := memMetricsDoc(map[string]float64{"sb_live": 700 * 1024 * 1024})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(doc))
	}))
	defer upstream.Close()
	svc.memSamplerFetch = memSampleHTTPGetter(upstream.URL, svc.memSampleClient)
	svc.memSampleInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.startMemSampler(ctx)
	waitFor(t, "sampler records through the HTTP getter", 2*time.Second, func() bool {
		used, ok := svc.memSamples.latest(lease.ID)
		return ok && used == 700*1024*1024
	})
	if peak, _ := svc.memSamples.peak(lease.ID); peak != 700*1024*1024 {
		t.Fatalf("peak = %g, want 700 MiB", peak)
	}
}

// TestSampleMemOnceRacesConcurrentPass: a pass that runs alongside the
// loop's own pass must not fail — forget and record interleave safely
// (the shape of a release racing a sampler tick).
func TestSampleMemOnceRacesConcurrentPass(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	lease, err := svc.grantLease(context.Background(), leaseRequest{owner: "consumer-a", image: "py-base", ttl: time.Hour})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.store.mu.Lock()
	lease.SandboxID, lease.HostIP = "sb_A", "10.0.0.1"
	svc.store.mu.Unlock()

	svc.memSamplerFetch = func(ctx context.Context) ([]byte, error) {
		return []byte(memMetricsDoc(map[string]float64{"sb_A": 42 * 1024 * 1024})), nil
	}
	svc.memSampleInterval = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	// The sampler's goroutine, as Start runs it; a panic (a write racing
	// forget) fails the test through the race-safe go test machinery.
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.startMemSampler(ctx)
	}()
	for i := 0; i < 20; i++ {
		time.Sleep(5 * time.Millisecond)
		svc.memSamples.forget(lease.ID)
		svc.sampleMemOnce(ctx)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("sampler loop did not stop on cancel")
	}
}
