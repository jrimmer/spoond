package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
)

// newReadyzServer builds a server with every source healthy: the fake
// substrate's default NodeInfo (healthy, most of the pool free), a live
// temp database and an injected disk reader reporting 80 % used — under
// the danger level, without trusting the CI host's own disks.
func newReadyzServer(t *testing.T) (*httptest.Server, *Service, *testSub) {
	t.Helper()
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	svc.cfg.TemplateStoragePath = t.TempDir()
	svc.diskCapacity = func(string) (uint64, uint64, error) { return 100, 20, nil }
	srv := NewServer(svc, NewImageRegistry(db))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, svc, sub
}

// getReadyz fetches /readyz without a token and decodes the body.
// When a test has just flipped a dependency, the 5 s cache may still
// hold the previous verdict: it polls until the answer matches want
// (well past the cache window) so tests stay fast on a fresh cache and
// correct on a warm one.
func getReadyz(t *testing.T, ts *httptest.Server, want int) (*http.Response, readyzResult) {
	t.Helper()
	deadline := time.Now().Add(readyzCacheFor + 2*time.Second)
	for {
		resp, err := http.Get(ts.URL + "/readyz")
		if err != nil {
			t.Fatalf("readyz: %v", err)
		}
		var out readyzResult
		err = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("decode readyz body: %v", err)
		}
		if resp.StatusCode == want || time.Now().After(deadline) {
			return resp, out
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// checkByName returns the named check from a result.
func checkByName(t *testing.T, r readyzResult, name string) readyCheck {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q check in %+v", name, r.Checks)
	return readyCheck{}
}

// A healthy backend is ready: 200, status ok, and every named check
// passing. No bearer token is sent — the endpoint is auth-exempt.
func TestReadyzHealthy(t *testing.T) {
	ts, _, _ := newReadyzServer(t)

	resp, r := getReadyz(t, ts, http.StatusOK)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200: %+v", resp.StatusCode, r)
	}
	if r.Status != "ok" {
		t.Fatalf("status = %q, want ok: %+v", r.Status, r.Checks)
	}
	for _, want := range []string{"orchestrator", "database", "disk", "hugepages"} {
		c := checkByName(t, r, want)
		if !c.OK {
			t.Errorf("check %q failed: %s", c.Name, c.Detail)
		}
	}
}

// Each failing check fails the whole response with 503, naming itself
// and a reason, while the passing checks stay listed.
func TestReadyzEachFailingCheck(t *testing.T) {
	t.Run("orchestrator unreachable", func(t *testing.T) {
		ts, _, sub := newReadyzServer(t)
		sub.SetNodeInfo(substrate.NodeInfo{}, errors.New("node down"))
		assertNotReady(t, ts, "orchestrator")
	})

	t.Run("orchestrator unhealthy", func(t *testing.T) {
		ts, _, sub := newReadyzServer(t)
		info, _ := sub.NodeInfo(t.Context())
		info.Status = "draining"
		sub.SetNodeInfo(info, nil)
		assertNotReady(t, ts, "orchestrator")
	})

	t.Run("database", func(t *testing.T) {
		ts, svc, _ := newReadyzServer(t)
		db, err := store.Open(filepath.Join(t.TempDir(), "broken.db"))
		if err != nil {
			t.Fatalf("open stand-in db: %v", err)
		}
		// Close the underlying file handles but keep the DB value: the
		// ping must then fail, as it would with a wedged catalog.
		db.Close()
		defer func() { svc.db = db }()
		svc.db = db
		assertNotReady(t, ts, "database")
	})

	t.Run("disk unreadable", func(t *testing.T) {
		ts, svc, _ := newReadyzServer(t)
		svc.cfg.TemplateStoragePath = filepath.Join(t.TempDir(), "missing", "store")
		svc.diskCapacity = statfsCapacity // the real statfs: the path does not exist
		assertNotReady(t, ts, "disk")
	})

	t.Run("disk past danger level", func(t *testing.T) {
		ts, svc, _ := newReadyzServer(t)
		svc.diskCapacity = func(string) (uint64, uint64, error) { return 100, 5, nil } // 95 % used
		assertNotReady(t, ts, "disk")
	})

	t.Run("hugepages past danger level", func(t *testing.T) {
		ts, _, sub := newReadyzServer(t)
		sub.SetNodeInfo(substrate.NodeInfo{
			Status: "healthy", HugepagesTotal: 100, HugepagesUsed: 95, HugepageSizeBytes: 1,
		}, nil)
		assertNotReady(t, ts, "hugepages")
	})

	t.Run("hugepages no pool", func(t *testing.T) {
		ts, _, sub := newReadyzServer(t)
		sub.SetNodeInfo(substrate.NodeInfo{Status: "healthy"}, nil)
		assertNotReady(t, ts, "hugepages")
	})
}

// assertNotReady asserts 503 with status fail and the named check
// failing with a non-empty detail, and that every other still-passing
// check stays listed. Other checks may fail with it — a node-down
// orchestrator legitimately fails both NodeInfo-derived checks — but
// each must name a reason too.
func assertNotReady(t *testing.T, ts *httptest.Server, name string) {
	t.Helper()
	// The 5 s cache would answer the previous state: each assertion in
	// this file gets a fresh server, but wait out the cache boundary in
	// case the healthy probe of a subtest ran just before.
	resp, r := getReadyz(t, ts, http.StatusServiceUnavailable)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %+v", resp.StatusCode, r)
	}
	if r.Status != "fail" {
		t.Fatalf("status = %q, want fail", r.Status)
	}
	failing := checkByName(t, r, name)
	if failing.OK {
		t.Fatalf("check %q reports ok", name)
	}
	if failing.Detail == "" {
		t.Fatalf("check %q carries no detail", name)
	}
	for _, c := range r.Checks {
		if c.Name != name && !c.OK && c.Detail == "" {
			t.Errorf("check %q also failed but carries no detail", c.Name)
		}
	}
}

// Concurrent polls past the cache window share one evaluation: the
// first runs the checks, the rest wait for it, and the checks run once
// — never once per waiter. Tested against the state machine directly so
// no test has to sit out the 5 s window.
func TestReadyzSingleFlight(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	runs := 0
	st := &readyzState{check: func() readyzResult {
		runs++
		if runs == 1 {
			close(started)
		}
		<-release
		return readyzResult{Status: "fail", Checks: []readyCheck{{Name: "orchestrator", Detail: "down"}}}
	}}

	leader := make(chan readyzResult, 1)
	go func() { leader <- st.readyz() }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first poll never ran the checks")
	}

	// Five more polls arrive while the checks run: they must join the
	// evaluation, not start their own.
	const waiters = 5
	results := make(chan readyzResult, waiters)
	for i := 0; i < waiters; i++ {
		go func() { results <- st.readyz() }()
	}
	time.Sleep(50 * time.Millisecond)
	if runs != 1 {
		t.Fatalf("checks ran %d times while waiters joined, want 1", runs)
	}

	close(release)
	want := <-leader
	for i := 0; i < waiters; i++ {
		select {
		case got := <-results:
			if got.Status != want.Status {
				t.Fatalf("waiter %d got %+v, leader had %+v", i, got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("waiter %d never returned", i)
		}
	}

	// The completed evaluation is the cached answer: a further poll
	// within the window must not re-run the checks either.
	if got := st.readyz(); got.Status != want.Status {
		t.Fatalf("cached poll: %+v", got)
	}
	if runs != 1 {
		t.Fatalf("checks ran %d times in total, want 1", runs)
	}
}

// The auth-exempt surface: /readyz answers without a bearer token, like
// /healthz, and does not accept one meant to be something else.
func TestReadyzNoAuth(t *testing.T) {
	ts, _, _ := newReadyzServer(t)
	resp, _ := doReq(t, "GET", ts.URL+"/readyz", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 without a token, got %d", resp.StatusCode)
	}
	resp, _ = doReq(t, "GET", ts.URL+"/readyz", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with an unrelated token, got %d", resp.StatusCode)
	}
}

// The whole response is cached for 5 s: a poller hitting /readyz every
// second sees one evaluation, and the fake's NodeInfo call count proves
// it. The cache is the polling contract, so this test polls a fresh
// server without waiting out the window.
func TestReadyzCachedFor5s(t *testing.T) {
	ts, _, sub := newReadyzServer(t)

	poll := func(t *testing.T) (*http.Response, readyzResult) {
		t.Helper()
		resp, err := http.Get(ts.URL + "/readyz")
		if err != nil {
			t.Fatalf("readyz: %v", err)
		}
		var out readyzResult
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		resp.Body.Close()
		return resp, out
	}

	if _, r := poll(t); r.Status != "ok" {
		t.Fatalf("first poll: %+v", r)
	}
	first := calls(sub.Fake, "NodeInfo")
	if first == 0 {
		t.Fatal("no NodeInfo call recorded for the first evaluation")
	}
	for i := 0; i < 3; i++ {
		resp, r := poll(t)
		if resp.StatusCode != http.StatusOK || r.Status != "ok" {
			t.Fatalf("cached poll %d: %d %+v", i, resp.StatusCode, r)
		}
	}
	if got := calls(sub.Fake, "NodeInfo"); got != first {
		t.Fatalf("cached polls re-evaluated: NodeInfo called %d → %d", first, got)
	}

	// Past the cache window the next poll re-evaluates and must see the
	// orchestrator's new state.
	sub.SetNodeInfo(substrate.NodeInfo{}, errors.New("node down"))
	deadline := time.Now().Add(readyzCacheFor + 2*time.Second)
	for {
		resp, r := poll(t)
		if resp.StatusCode == http.StatusServiceUnavailable {
			if r.Status != "fail" {
				t.Fatalf("fail body: %+v", r)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("readiness never re-evaluated after the cache window")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Every check is bounded: a substrate whose NodeInfo hangs must not
// wedge the response past the per-check timeout (plus scheduling
// slack).
func TestReadyzCheckBounded(t *testing.T) {
	ts, _, sub := newReadyzServer(t)
	sub.SetNodeInfoFunc(func(ctx context.Context) (substrate.NodeInfo, error) {
		<-ctx.Done() // hang until the check's own timeout cancels us
		return substrate.NodeInfo{}, ctx.Err()
	})
	start := time.Now()
	resp, r := getReadyz(t, ts, http.StatusServiceUnavailable)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %+v", resp.StatusCode, r)
	}
	c := checkByName(t, r, "orchestrator")
	if c.OK {
		t.Fatal("hung orchestrator reported ok")
	}
	if elapsed := time.Since(start); elapsed > readyzCheckTimeout+2*time.Second {
		t.Fatalf("hung checks took %s, want ≤ ~%s", elapsed, readyzCheckTimeout)
	}
}

// The per-route metrics cover /readyz: the request counter splits by
// path so a monitor's polling does not blur into other routes.
func TestReadyzInPerRouteMetrics(t *testing.T) {
	ts, _, _ := newReadyzServer(t)
	if _, r := getReadyz(t, ts, http.StatusOK); r.Status != "ok" {
		t.Fatalf("readyz: %+v", r)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer token-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	body := readAll(t, resp)
	if !strings.Contains(body, `spoond_http_requests_total{code="200",method="GET",path="/readyz"`) {
		t.Errorf("metrics lack the /readyz series:\n%s", tail(body, 600))
	}
}
