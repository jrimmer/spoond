package spoonddash

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/store"
	"golang.org/x/crypto/bcrypt"
)

const sampleMetrics = `# HELP spoond_leases_active Leased (non-warm) sandboxes.
# TYPE spoond_leases_active gauge
spoond_leases_active 3
# TYPE spoond_leases gauge
spoond_leases{state="running"} 2
spoond_leases{state="suspended"} 1
# TYPE spoond_leases_by_image gauge
spoond_leases_by_image{image="go-base"} 2
spoond_leases_by_image{image="py-base"} 1
# TYPE spoond_leases_total counter
spoond_leases_total 1234
# TYPE spoond_node_running_sandboxes gauge
spoond_node_running_sandboxes 3
# TYPE spoond_http_requests_total counter
spoond_http_requests_total{path="/api/sandboxes",method="POST",code="201"} %d
# TYPE spoond_create_duration_seconds histogram
spoond_create_duration_seconds_bucket{resume="false",le="+Inf"} %d
spoond_create_duration_seconds_sum{resume="false"} %s
spoond_create_duration_seconds_count{resume="false"} %d
spoond_create_duration_seconds_bucket{resume="true",le="+Inf"} 3
spoond_create_duration_seconds_sum{resume="true"} 4.2
spoond_create_duration_seconds_count{resume="true"} 3
# --- orchestrator (otel) ---
# TYPE orchestrator_sandbox_limit gauge
orchestrator_sandbox_limit 64
# TYPE orchestrator_status gauge
orchestrator_status{status="healthy",version="0.4.2"} 1
`

func metricsServer(t *testing.T, token string, frames []string) *httptest.Server {
	t.Helper()
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "nope", http.StatusForbidden)
			return
		}
		w.Write([]byte(frames[min(i, len(frames)-1)]))
		i++
	}))
	t.Cleanup(srv.Close)
	return srv
}

func frame(req, n int, sum string) string {
	return fmt.Sprintf(sampleMetrics, req, n, sum, n)
}

func testConfig(t *testing.T, metricsURL string) Config {
	t.Helper()
	dir := t.TempDir()
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	return Config{
		User: "watch", PasswordHash: string(hash), MetricsURL: metricsURL, MetricsToken: "scrape",
		DBPath: filepath.Join(dir, "spoond.db"), UsersFile: filepath.Join(dir, "users.json"),
		StoragePath: dir, Services: []string{"definitely-not-a-unit"}, Interval: time.Second, History: 20,
	}
}

func TestCollectMetricsAndRates(t *testing.T) {
	srv := metricsServer(t, "scrape", []string{frame(100, 10, "5"), frame(130, 12, "5.5")})
	c := newCollector(testConfig(t, srv.URL))
	ctx := context.Background()

	s := c.collect(ctx)
	if s.Leases != 3 || s.ByState["running"] != 2 || s.ByImage["go-base"] != 2 || s.Granted != 1234 {
		t.Fatalf("gauges: %+v", s)
	}
	if s.Running != 3 || s.Limit != 64 || s.Version != "0.4.2" {
		t.Fatalf("orchestrator section not parsed: running=%d limit=%d version=%q", s.Running, s.Limit, s.Version)
	}
	if s.ReqPerSec != 0 || s.CreatesPerMin != 0 || s.CreateMs != -1 || s.ResumeMs != -1 {
		t.Fatalf("first scrape has no rate: %+v", s)
	}

	c.prevAt = c.prevAt.Add(-10 * time.Second) // pretend the last scrape was 10 s ago
	s = c.collect(ctx)
	if s.ReqPerSec < 2.9 || s.ReqPerSec > 3.1 { // 30 requests over ~10 s
		t.Fatalf("reqPerSec = %v, want ~3", s.ReqPerSec)
	}
	if s.CreateMs != 250 || s.CreatesPerMin != 2 { // (5.5-5) s over 2 fresh creates
		t.Fatalf("createMs = %v, createsPerMin = %v, want 250 and 2", s.CreateMs, s.CreatesPerMin)
	}
	if s.ResumeMs != -1 { // the resume series did not move
		t.Fatalf("resumeMs = %v, want -1 (none in the window)", s.ResumeMs)
	}
	if s.Down != 1 || s.Services[0].State == "active" {
		t.Fatalf("missing unit should count as down: %+v", s.Services)
	}
}

func TestScrapeTokenRejectedIsReported(t *testing.T) {
	srv := metricsServer(t, "other", []string{frame(1, 1, "1")})
	s := newCollector(testConfig(t, srv.URL)).collect(context.Background())
	if !strings.Contains(s.Err, "403") {
		t.Fatalf("err = %q, want the 403", s.Err)
	}
}

func TestBasicAuthAndStream(t *testing.T) {
	srv := metricsServer(t, "scrape", []string{frame(1, 1, "1")})
	d, err := newDash(testConfig(t, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.run(ctx)
	h := d.handler()

	for path, want := range map[string]int{"/": 401, "/stream": 401, "/static/css/theme.css": 401, "/healthz": 200} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != want {
			t.Fatalf("%s without auth: %d, want %d", path, rec.Code, want)
		}
	}
	bad := httptest.NewRequest("GET", "/", nil)
	bad.SetBasicAuth("watch", "wrong")
	if rec := httptest.NewRecorder(); func() int { h.ServeHTTP(rec, bad); return rec.Code }() != 401 {
		t.Fatal("wrong password accepted")
	}

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		req.SetBasicAuth("watch", "pw")
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := get("/"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "SANDBOX CONTROL") || !strings.Contains(rec.Body.String(), `id="leases"`) {
		t.Fatalf("page: %d", rec.Code)
	}
	if rec := get("/static/components/gauge/gauge.js"); rec.Code != 200 {
		t.Fatalf("vendored component: %d", rec.Code)
	}

	// The stream's first frame carries history and the three tables.
	sctx, scancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer scancel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/stream", nil).WithContext(sctx)
	req.SetBasicAuth("watch", "pw")
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{"event: datastar-patch-signals", `"_h":`, `"_s":`, "event: datastar-patch-elements",
		`data: elements <table id="leases">`, `data: elements <table id="images">`, `data: elements <div id="services"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `"Rows"`) {
		t.Fatal("table rows leaked into the signal payload")
	}
}

func TestUserNamesLayouts(t *testing.T) {
	for name, doc := range map[string]string{
		"list":      `{"users":[{"id":"u-1","name":"jason","token_hash":"x"}]}`,
		"map":       `{"users":{"u-1":{"id":"u-1","name":"jason"}}}`,
		"bare map":  `{"u-1":{"name":"jason"}}`,
		"bare list": `[{"id":"u-1","name":"jason"}]`,
	} {
		cfg := testConfig(t, "")
		os.WriteFile(cfg.UsersFile, []byte(doc), 0o600)
		if got := (&collector{cfg: cfg}).userNames()["u-1"]; got != "jason" {
			t.Fatalf("%s: got %q", name, got)
		}
	}
}

func TestFromDBReadsLeasesAndImages(t *testing.T) {
	cfg := testConfig(t, "")
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	raw, err := sql.Open("sqlite", cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	ts := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339Nano) }
	for _, q := range []string{
		`INSERT INTO images (name, template_id, current_build_id, vcpu, memory_mb, disk_mb, updated_at) VALUES ('go-base','t1','b1',2,2048,6144,'` + ts(-48*time.Hour) + `')`,
		`INSERT INTO images (name, template_id, vcpu, memory_mb, disk_mb, updated_at) VALUES ('unbuilt','t2',1,512,1024,'` + ts(0) + `')`,
		`INSERT INTO leases (id, owner, image, created_at, expires_at, last_active, net_policy) VALUES ('abcdef0123456789','u-1','go-base','` + ts(-5*time.Minute) + `','` + ts(10*time.Minute) + `','` + ts(0) + `','internet')`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	raw.Close()
	os.WriteFile(cfg.UsersFile, []byte(`{"users":[{"id":"u-1","name":"swarm"}]}`), 0o600)

	s := Snapshot{ByImage: map[string]int{"go-base": 1}}
	if err := (&collector{cfg: cfg}).fromDB(&s, now); err != nil {
		t.Fatal(err)
	}
	if len(s.Rows) != 1 || s.Rows[0].ID != "abcdef0123" || s.Rows[0].Owner != "swarm" || s.Rows[0].Age != "5m" || s.Rows[0].Left != "10m" {
		t.Fatalf("lease rows: %+v", s.Rows)
	}
	if len(s.Images) != 1 || s.Images[0].Name != "go-base" || s.Images[0].Live != 1 || s.Images[0].Updated != "2d ago" {
		t.Fatalf("image rows (unbuilt must be skipped): %+v", s.Images)
	}
}

// The host as measured on 2026-10-02: a 43 GiB pool of 2 MiB pages, five
// leases holding 19 GiB of it (9.9 GiB touched, 9.1 GiB reserved).
func TestMemGaugesExcludePoolAndCountReserved(t *testing.T) {
	var s Snapshot
	memGauges(&s, map[string]uint64{
		"MemTotal": 65649676, "MemAvailable": 10470980, "Hugepagesize": 2048,
		"HugePages_Total": 21973, "HugePages_Free": 16907, "HugePages_Rsvd": 4662,
	})
	if s.MemTotalGiB != 19.7 || s.MemUsedGiB != 9.7 || s.MemUsedPct != 49.3 {
		t.Errorf("memory = %v of %v GiB (%v%%), want 9.7 of 19.7 GiB (49.3%%)", s.MemUsedGiB, s.MemTotalGiB, s.MemUsedPct)
	}
	if s.HugeFreeGiB != 23.9 || s.HugeUsedPct != 44.3 {
		t.Errorf("hugepages = %v GiB free (%v%%), want 23.9 GiB (44.3%%)", s.HugeFreeGiB, s.HugeUsedPct)
	}
}
