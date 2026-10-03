package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jrimmer/spoond/v2/substrate"
)

// TestLeaseGrantDurationObservedOnGrant: a grant against the fake
// substrate increments spoond_lease_grant_duration_seconds. The metric
// is observed in Service.grant only when the lease is actually returned
// (pool hit or cold create alike), so a scrape after one successful
// create must show count 1.
func TestLeaseGrantDurationObservedOnGrant(t *testing.T) {
	ts, _ := newTestServer(t)

	if n := grantHistogramCount(t, ts); n != 0 {
		t.Fatalf("fresh server: lease_grant_duration count = %d, want 0", n)
	}

	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	if resp.StatusCode != 201 {
		t.Fatalf("create status %d: %v", resp.StatusCode, body)
	}

	if n := grantHistogramCount(t, ts); n != 1 {
		t.Fatalf("after one grant: lease_grant_duration count = %d, want 1", n)
	}
}

// grantHistogramCount scrapes /metrics and sums the
// spoond_lease_grant_duration_seconds_count samples.
func grantHistogramCount(t *testing.T, ts *httptest.Server) uint64 {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer token-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	body := readAll(t, resp)
	var total uint64
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "spoond_lease_grant_duration_seconds_count") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("malformed sample line: %q", line)
		}
		var v float64
		if _, err := fmt.Sscanf(fields[1], "%g", &v); err != nil {
			t.Fatalf("sample value %q: %v", fields[1], err)
		}
		total += uint64(v)
	}
	return total
}

// TestHealthzHealthyAndDegraded: /healthz reports the orchestrator
// status on 200, and 503 "unreachable" when NodeInfo fails (U11).
func TestHealthzHealthyAndDegraded(t *testing.T) {
	ts, sub := newTestServer(t)

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status %d, want 200: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"status":"ok"`) || !strings.Contains(body, `"orchestrator":"healthy"`) {
		t.Errorf("healthz body = %s, want ok + orchestrator status", body)
	}

	sub.SetNodeInfo(substrate.NodeInfo{}, errors.New("node down"))
	resp, err = http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz degraded: %v", err)
	}
	body = readAll(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("degraded healthz status %d, want 503: %s", resp.StatusCode, body)
	}
	if body != `{"status":"degraded","orchestrator":"unreachable"}`+"\n" &&
		!strings.Contains(body, `"status":"degraded"`) {
		t.Errorf("degraded body = %q", body)
	}
}

// TestMetricsOTelPassthrough: with OTEL_PROM_URL set, /metrics appends
// the collector output after the marker line; a failing fetch appends
// the unavailable line.
func TestMetricsOTelPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("# HELP otel_test_metric A test.\notel_test_metric 1\n"))
	}))
	defer upstream.Close()

	ts, _ := newTestServer(t)
	t.Setenv("OTEL_PROM_URL", upstream.URL)
	req, _ := http.NewRequest("GET", ts.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer token-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status %d", resp.StatusCode)
	}
	if !strings.Contains(body, "spoond_leases_active") {
		t.Errorf("metrics body lacks spoond metrics")
	}
	if !strings.Contains(body, "# --- orchestrator (otel) ---") {
		t.Errorf("metrics body lacks the otel marker line")
	}
	if !strings.Contains(body, "otel_test_metric 1") {
		t.Errorf("metrics body lacks the passthrough output")
	}

	t.Setenv("OTEL_PROM_URL", "http://127.0.0.1:1/metrics") // nothing listens
	req2, _ := http.NewRequest("GET", ts.URL+"/metrics", nil)
	req2.Header.Set("Authorization", "Bearer token-a")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	raw := readAll(t, resp2)
	if !strings.Contains(raw, "# orchestrator metrics unavailable:") {
		t.Errorf("metrics body lacks the unavailable line:\n%s", tail(raw, 400))
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	resp.Body.Close()
	return string(b)
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
