package api

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jrimmer/spoond/v2/substrate"
)

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
