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
	// A fixed clock: the rates must not depend on how long a scrape takes
	// (systemd and host lookups take seconds on a loaded CI lease).
	t0 := time.Unix(1_800_000_000, 0)
	c.now = func() time.Time { return t0 }

	s := c.collect(ctx)
	if s.Leases != 3 || s.ByState["running"] != 2 || s.Granted != 1234 {
		t.Fatalf("gauges: %+v", s)
	}
	if s.Running != 3 || s.Limit != 64 || s.Version != "0.4.2" {
		t.Fatalf("orchestrator section not parsed: running=%d limit=%d version=%q", s.Running, s.Limit, s.Version)
	}
	if s.ReqPerSec != 0 || s.CreatesPerMin != 0 || s.CreateMs != -1 || s.ResumeMs != -1 {
		t.Fatalf("first scrape has no rate: %+v", s)
	}

	c.now = func() time.Time { return t0.Add(10 * time.Second) } // the next scrape, 10 s later
	s = c.collect(ctx)
	if s.ReqPerSec < 2.9 || s.ReqPerSec > 3.1 { // 30 requests over 10 s
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

	for path, want := range map[string]int{"/": 401, "/stream": 401, "/static/vendor/webtui/full.css": 401, "/healthz": 200} {
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
	if rec := get("/"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `id="grid"`) || !strings.Contains(rec.Body.String(), `id="r0"`) {
		t.Fatalf("page: %d\n%s", rec.Code, rec.Body.String())
	}
	if rec := get("/static/vendor/webtui/full.css"); rec.Code != 200 {
		t.Fatalf("vendored WebTUI: %d", rec.Code)
	}

	// The stream's first frame carries history, the signals and the grid
	// as row patches.
	sctx, scancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer scancel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/stream", nil).WithContext(sctx)
	req.SetBasicAuth("watch", "pw")
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{"event: datastar-patch-signals", `"_h":`, `"_s":`, "event: datastar-patch-elements",
		"data: selector #grid", "data: mode inner", `id="r0"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream lacks %q:\n%s", want, body)
		}
	}
	// The selector goes straight into querySelectorAll: it must be a
	// plain id selector, not a bracketed mode suffix nothing matches.
	if strings.Contains(body, "selector #grid[") {
		t.Fatalf("selector carries a bracket suffix Datastar cannot match:\n%s", body)
	}
	if strings.Contains(body, `"Rows"`) {
		t.Fatal("table rows leaked into the signal payload")
	}
}

// DASH_AUTH off serves the page, the stream and the static files with
// no login. The default, a typo or an empty value keeps the login, and
// with the login off DASH_USER and DASH_PASSWORD_HASH are not required.
func TestDashAuthToggle(t *testing.T) {
	for v, want := range map[string]bool{"": true, "on": true, "1": true, "yes": true, "ofF": false,
		"off": false, "0": false, "false": false, "no": false, " No ": false, "of": true} {
		if got := authOn(v); got != want {
			t.Errorf("authOn(%q) = %v, want %v", v, got, want)
		}
	}
	t.Setenv("METRICS_TOKEN", "scrape")
	t.Setenv("DASH_USER", "")
	t.Setenv("DASH_PASSWORD_HASH", "")
	t.Setenv("DASH_AUTH", "")
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NoAuth || requireLogin(cfg) == nil {
		t.Fatalf("default: NoAuth=%v, requireLogin=%v; want the login on and required", cfg.NoAuth, requireLogin(cfg))
	}
	t.Setenv("DASH_AUTH", "off")
	if cfg, err = configFromEnv(); err != nil {
		t.Fatal(err)
	}
	if !cfg.NoAuth || requireLogin(cfg) != nil {
		t.Fatalf("DASH_AUTH=off: NoAuth=%v, requireLogin=%v; want no login", cfg.NoAuth, requireLogin(cfg))
	}

	srv := metricsServer(t, "scrape", []string{frame(1, 1, "1")})
	tc := testConfig(t, srv.URL)
	tc.NoAuth, tc.User, tc.PasswordHash = true, "", ""
	d, err := newDash(tc)
	if err != nil {
		t.Fatal(err)
	}
	h := d.handler()
	for _, path := range []string{"/", "/static/vendor/webtui/full.css", "/healthz"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 || rec.Header().Get("WWW-Authenticate") != "" {
			t.Fatalf("%s with DASH_AUTH off: %d (WWW-Authenticate %q), want 200 and no challenge",
				path, rec.Code, rec.Header().Get("WWW-Authenticate"))
		}
	}
}

// The dashboard's /readyz (issue #81): 200 when every source the
// dashboard reads answers, 503 naming the failing ones. The sources are
// probed live per request (no dashboard-side cache — the collector tick
// is the cache), each under a 2 s bound.
func TestDashReadyz(t *testing.T) {
	srv := metricsServer(t, "scrape", []string{frame(1, 1, "1")})
	cfg := testConfig(t, srv.URL)
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	db.Close() // the catalog the dashboard will open read-only
	if err := os.WriteFile(cfg.UsersFile, []byte(`{"users":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := newDash(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := d.handler()

	readyz := func() (int, string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
		return rec.Code, rec.Body.String()
	}

	code, body := readyz()
	if code != http.StatusOK || !strings.Contains(body, `"status":"ok"`) {
		t.Fatalf("all sources up: %d %s", code, body)
	}
	for _, want := range []string{"metrics", "database", "identity store"} {
		if !strings.Contains(body, `"name":"`+want+`","ok":true`) {
			t.Fatalf("body lacks an ok %q check: %s", want, body)
		}
	}

	// The metrics source dies: 503 with the failing check named and its
	// reason carried, the still-passing checks listed beside it.
	srv.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		code, body = readyz()
		if code == http.StatusServiceUnavailable && strings.Contains(body, `"status":"fail"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("metrics source down, readyz never flipped: %d %s", code, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(body, `"name":"metrics","ok":false`) {
		t.Fatalf("fail body lacks the failing metrics check: %s", body)
	}
	for _, want := range []string{"database", "identity store"} {
		if !strings.Contains(body, `"name":"`+want+`","ok":true`) {
			t.Fatalf("fail body lacks a passing %q check: %s", want, body)
		}
	}
}

// /readyz is auth-exempt like /healthz: an uptime monitor holds no
// dashboard credentials.
func TestDashReadyzNoAuth(t *testing.T) {
	srv := metricsServer(t, "scrape", []string{frame(1, 1, "1")})
	cfg := testConfig(t, srv.URL)
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := os.WriteFile(cfg.UsersFile, []byte(`{"users":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := newDash(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	d.handler().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz without auth: %d, want 200", rec.Code)
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

// TestFromDBBurstCountsAllLiveBurstLeases: the capacity panel's burst
// count comes from the store over every live burst lease, not from the
// ≤40 rows the leases panel reads — a burst lease older than the
// display window still counts.
func TestFromDBBurstCountsAllLiveBurstLeases(t *testing.T) {
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
	// 41 running burst leases (the oldest outside the 40-row window), a
	// guaranteed lease, a lost burst lease (not live) and a suspended
	// burst lease.
	queries := []string{
		`INSERT INTO images (name, template_id, current_build_id, vcpu, memory_mb, disk_mb, updated_at) VALUES ('go-base','t1','b1',2,2048,6144,'` + ts(-48*time.Hour) + `')`,
		`INSERT INTO leases (id, owner, image, created_at, expires_at, last_active, state, class) VALUES ('guaranteed','u-1','go-base','` + ts(-time.Minute) + `','` + ts(time.Hour) + `','` + ts(0) + `','running','guaranteed')`,
		`INSERT INTO leases (id, owner, image, created_at, expires_at, last_active, state, class) VALUES ('lost-burst','u-1','go-base','` + ts(-time.Minute) + `','` + ts(time.Hour) + `','` + ts(0) + `','lost','burst')`,
	}
	for i := 0; i < 41; i++ {
		queries = append(queries, fmt.Sprintf(
			`INSERT INTO leases (id, owner, image, created_at, expires_at, last_active, state, class) VALUES ('burst-%02d','u-1','go-base','%s','%s','%s','running','burst')`,
			i, ts(-time.Duration(i+2)*time.Minute), ts(time.Hour), ts(0)))
	}
	queries = append(queries,
		`INSERT INTO leases (id, owner, image, created_at, expires_at, last_active, state, suspended, class) VALUES ('susp-burst','u-1','go-base','`+ts(-2*time.Hour)+`','`+ts(time.Hour)+`','`+ts(0)+`','suspended',1,'burst')`)
	for _, q := range queries {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	raw.Close()
	os.WriteFile(cfg.UsersFile, []byte(`{"users":[{"id":"u-1","name":"ci"}]}`), 0o600)

	var s Snapshot
	if err := (&collector{cfg: cfg}).fromDB(&s, now); err != nil {
		t.Fatal(err)
	}
	if len(s.Rows) != 40 {
		t.Fatalf("rows = %d, want the display window's 40", len(s.Rows))
	}
	if s.Burst != 42 {
		t.Fatalf("burst = %d, want 42 (41 running + 1 suspended, live; the lost one and the guaranteed one excluded)", s.Burst)
	}
}

// TestFromDBPreemptedCountsOutsideTheWindow: the preempted count comes
// from the store over every live preempted lease (#128 part 3), even one
// older than the leases panel's 40-row window.
func TestFromDBPreemptedCountsOutsideTheWindow(t *testing.T) {
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
	queries := []string{
		`INSERT INTO images (name, template_id, current_build_id, vcpu, memory_mb, disk_mb, updated_at) VALUES ('go-base','t1','b1',2,2048,6144,'` + ts(-48*time.Hour) + `')`,
		// 41 preempted leases, the oldest outside the display window, and
		// one running lease that is not preempted.
		`INSERT INTO leases (id, owner, image, created_at, expires_at, last_active, state, suspended, class, preempted_at) VALUES ('running','u-1','go-base','` + ts(-time.Minute) + `','` + ts(time.Hour) + `','` + ts(0) + `','running',0,'burst','')`,
	}
	for i := 0; i < 41; i++ {
		queries = append(queries, fmt.Sprintf(
			`INSERT INTO leases (id, owner, image, created_at, expires_at, last_active, state, suspended, class, preempted_at) VALUES ('preempt-%02d','u-1','go-base','%s','%s','%s','suspended',1,'burst','%s')`,
			i, ts(-time.Duration(i+2)*time.Minute), ts(time.Hour), ts(0), ts(-time.Duration(i+1)*time.Minute)))
	}
	for _, q := range queries {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	raw.Close()
	os.WriteFile(cfg.UsersFile, []byte(`{"users":[{"id":"u-1","name":"ci"}]}`), 0o600)

	var s Snapshot
	if err := (&collector{cfg: cfg}).fromDB(&s, now); err != nil {
		t.Fatal(err)
	}
	if s.Preempted != 41 {
		t.Fatalf("preempted = %d, want 41 (the running lease excluded)", s.Preempted)
	}
}

// TestFromDBLostPreemptedNotMarked: a lost row keeps its preempted_at
// in the store, but it is not waiting for a resume, so neither the "·p"
// row mark nor the attention-strip count applies.
func TestFromDBLostPreemptedNotMarked(t *testing.T) {
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
		`INSERT INTO leases (id, owner, image, created_at, expires_at, last_active, state, class, preempted_at) VALUES ('lost-preempt','u-1','go-base','` + ts(-time.Minute) + `','` + ts(time.Hour) + `','` + ts(0) + `','lost','burst','` + ts(-time.Minute) + `')`,
		`INSERT INTO leases (id, owner, image, created_at, expires_at, last_active, state, suspended, class, preempted_at) VALUES ('susp-preempt','u-1','go-base','` + ts(-2*time.Minute) + `','` + ts(time.Hour) + `','` + ts(0) + `','suspended',1,'burst','` + ts(-time.Minute) + `')`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	raw.Close()
	os.WriteFile(cfg.UsersFile, []byte(`{"users":[{"id":"u-1","name":"ci"}]}`), 0o600)

	var s Snapshot
	if err := (&collector{cfg: cfg}).fromDB(&s, now); err != nil {
		t.Fatal(err)
	}
	if s.Preempted != 1 {
		t.Fatalf("preempted = %d, want 1 (the lost row excluded)", s.Preempted)
	}
	for _, r := range s.Rows {
		switch r.ID {
		case "lost-preempt":
			if r.Preempted {
				t.Fatal("lost preempted row must not show the ·p mark")
			}
		case "susp-preempt":
			if !r.Preempted {
				t.Fatal("suspended preempted row must show the ·p mark")
			}
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
	os.WriteFile(cfg.UsersFile, []byte(`{"users":[{"id":"u-1","name":"ci"}]}`), 0o600)

	s := Snapshot{}
	if err := (&collector{cfg: cfg}).fromDB(&s, now); err != nil {
		t.Fatal(err)
	}
	if len(s.Rows) != 1 || s.Rows[0].ID != "abcdef0123" || s.Rows[0].Owner != "ci" || s.Rows[0].Age != "5m" || s.Rows[0].Left != "10m" {
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
