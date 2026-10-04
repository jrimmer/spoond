package spoonddoctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/notify"
	"github.com/jrimmer/spoond/v2/store"
)

// insertBuild records one build row with the given versions and state.
func insertBuild(t *testing.T, db *store.DB, id, state, fc, kernel string) {
	t.Helper()
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	err := db.InsertBuild(context.Background(), store.BuildRow{
		BuildID: id, Kind: "template", TemplateID: "tpl0123456789abcdefgh",
		Image: "py-base", State: state,
		FirecrackerVersion: fc, KernelVersion: kernel,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("insert build %s: %v", id, err)
	}
}

// TestVersionsInUse lists the distinct versions of non-deleted builds:
// deleted builds and empty version fields are excluded, values are
// deduplicated and sorted. versionsInUse itself opens the database
// read-only, so it must work against an existing, migrated store.
func TestVersionsInUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spoond.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	insertBuild(t, db, "b-1", "ready", "v1.14-0.2.0", "vmlinux-6.1.177_5008931")
	insertBuild(t, db, "b-2", "ready", "v1.14-0.2.0", "vmlinux-6.1.177_5008931")
	insertBuild(t, db, "b-3", "building", "v1.15-0.1.0", "vmlinux-6.1.177_5008931")
	insertBuild(t, db, "b-4", "deleted", "v1.10-0.1.0", "vmlinux-6.1.100")
	insertBuild(t, db, "b-5", "failed", "", "")
	db.Close()

	t.Setenv("SPOOND_DB_PATH", path)
	fc, kernel, err := versionsInUse()
	if err != nil {
		t.Fatalf("versionsInUse: %v", err)
	}
	wantFC := []string{"v1.14-0.2.0", "v1.15-0.1.0"}
	wantKernel := []string{"vmlinux-6.1.177_5008931"}
	if len(fc) != len(wantFC) {
		t.Fatalf("fc = %v, want %v", fc, wantFC)
	}
	for i := range wantFC {
		if fc[i] != wantFC[i] {
			t.Errorf("fc[%d] = %q, want %q", i, fc[i], wantFC[i])
		}
	}
	if len(kernel) != len(wantKernel) {
		t.Fatalf("kernel = %v, want %v", kernel, wantKernel)
	}
	for i := range wantKernel {
		if kernel[i] != wantKernel[i] {
			t.Errorf("kernel[%d] = %q, want %q", i, kernel[i], wantKernel[i])
		}
	}
}

// TestCheckLeasesLost: the "leases: lost" check lists every lost lease
// with its owner, image, age and the time the GC stops keeping its
// snapshots, PASSes when there are none, and never FAILs. A lease with
// an empty lost_at (lost before the column existed) has no age and no
// keep-until: the line says the grace period starts at the next GC
// pass, which is the pass that stamps the row.
func TestCheckLeasesLost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spoond.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().UTC()
	seed := func(id string, persistent bool, lostAt time.Time) {
		t.Helper()
		if err := db.UpsertLease(context.Background(), store.LeaseRow{
			ID: id, Owner: "user-1", Image: "py-base", State: "lost",
			Persistent: persistent, LostAt: lostAt,
			CreatedAt: now, ExpiresAt: now, LastActive: now,
		}); err != nil {
			t.Fatalf("seed lease %s: %v", id, err)
		}
	}
	// A running lease is not reported; a lost one is, with the age and
	// the keep-until time of its grace period (7 d persistent, 1 d not).
	if err := db.UpsertLease(context.Background(), store.LeaseRow{
		ID: "l-run", Owner: "user-1", Image: "go-base", State: "running",
		CreatedAt: now, ExpiresAt: now, LastActive: now,
	}); err != nil {
		t.Fatalf("seed running lease: %v", err)
	}
	seed("l-per", true, now.Add(-48*time.Hour))
	seed("l-plain", false, now.Add(-2*time.Hour))
	seed("l-nostamp", false, time.Time{})
	db.Close()

	t.Setenv("SPOOND_DB_PATH", path)
	results := checkLeases()
	if len(results) != 1 {
		t.Fatalf("checkLeases = %+v, want one result", results)
	}
	got := results[0]
	if got.name != "leases: lost" {
		t.Errorf("name = %q", got.name)
	}
	if got.status != "WARN" {
		t.Errorf("status = %q, want WARN", got.status)
	}
	for _, want := range []string{
		"3 lease(s) lost",
		"l-per owner=user-1 image=py-base lost 48h0m0s ago",
		"l-plain owner=user-1 image=py-base lost 2h0m0s ago",
		// The unstamped row carries no age and no keep-until: the grace
		// period starts when the GC stamps it.
		"l-nostamp owner=user-1 image=py-base lost before tracking began (grace starts at the next GC pass)",
	} {
		if !strings.Contains(got.detail, want) {
			t.Errorf("detail lacks %q:\n%s", want, got.detail)
		}
	}
	if strings.Contains(got.detail, "l-run") {
		t.Errorf("a running lease was reported:\n%s", got.detail)
	}
	// The keep-until time is lost_at + the grace period: 48 h ago + 7 d
	// and 2 h ago + 1 d. The unstamped loss has none — it is not counted
	// from now, because the GC's stamp, not the doctor's clock, fixes
	// when its grace period started.
	until := map[string]time.Time{
		"l-per":   now.Add(-48 * time.Hour).Add(7 * 24 * time.Hour),
		"l-plain": now.Add(-2 * time.Hour).Add(24 * time.Hour),
	}
	for id, want := range until {
		wantStr := "snapshot kept until " + want.UTC().Format("2006-01-02 15:04 Z07:00")
		if !strings.Contains(got.detail, id+" ") || !strings.Contains(got.detail, wantStr) {
			t.Errorf("detail lacks %s's %q:\n%s", id, wantStr, got.detail)
		}
	}

	// With no lost leases the check PASSes, and it never FAILs: even a
	// missing database is only a WARN.
	path2 := filepath.Join(t.TempDir(), "clean.db")
	db2, err := store.Open(path2)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db2.Close()
	t.Setenv("SPOOND_DB_PATH", path2)
	results = checkLeases()
	if len(results) != 1 || results[0].status != "PASS" || results[0].detail != "none" {
		t.Fatalf("clean checkLeases = %+v, want PASS none", results)
	}

	t.Setenv("SPOOND_DB_PATH", filepath.Join(t.TempDir(), "missing.db"))
	results = checkLeases()
	if len(results) != 1 || results[0].status != "WARN" {
		t.Fatalf("missing-db checkLeases = %+v, want WARN", results)
	}
}

func TestCheckDrainUnit(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "admin.token")
	if err := os.WriteFile(tok, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(dir, "drain.env")
	if err := os.WriteFile(env, []byte("SPOOND_DRAIN_URL=https://127.0.0.1:8890\nSPOOND_ADMIN_TOKEN_FILE="+tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldShow, oldEnv := systemctlShow, drainEnvPath
	t.Cleanup(func() { systemctlShow, drainEnvPath = oldShow, oldEnv })
	drainEnvPath = env
	good := map[string]string{"UnitFileState": "enabled", "ActiveState": "active",
		"After": "systemd-journald.socket e2b-orchestrator.service spoond-backend.service basic.target"}
	cases := []struct {
		name   string
		props  map[string]string
		status string
		want   string
	}{
		{"ok", good, "PASS", "ordered after"},
		{"disabled", map[string]string{"UnitFileState": "disabled", "ActiveState": "active", "After": good["After"]}, "FAIL", "not enabled"},
		{"inactive", map[string]string{"UnitFileState": "enabled", "ActiveState": "inactive", "After": good["After"]}, "FAIL", "not active"},
		{"missing order", map[string]string{"UnitFileState": "enabled", "ActiveState": "active", "After": "e2b-orchestrator.service"}, "FAIL", "not ordered after spoond-backend.service"},
	}
	for _, c := range cases {
		systemctlShow = func(string, ...string) (map[string]string, error) { return c.props, nil }
		r := checkDrainUnit()
		if len(r) != 1 || r[0].status != c.status || !strings.Contains(r[0].detail, c.want) {
			t.Errorf("%s: got %+v, want %s containing %q", c.name, r, c.status, c.want)
		}
	}
	systemctlShow = func(string, ...string) (map[string]string, error) { return good, nil }
	drainEnvPath = filepath.Join(dir, "missing.env")
	if r := checkDrainUnit(); r[0].status != "FAIL" || !strings.Contains(r[0].detail, "missing.env") {
		t.Errorf("missing drain.env: got %+v", r)
	}
}

// TestCheckWebhooks: unset warns (notifications are optional), a valid
// list passes with a count plus one reachability check per webhook, a
// malformed list fails naming the index — never the URL.
func TestCheckWebhooks(t *testing.T) {
	t.Setenv("NOTIFY_WEBHOOKS", "")
	res := checkWebhooks()
	if len(res) != 1 || res[0].status != "WARN" {
		t.Fatalf("unset = %+v", res)
	}
	// A reachable receiver: config passes, the probe answers 2xx.
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), "spoond notify test") {
			t.Errorf("probe body = %q, want the test message", b)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("NOTIFY_WEBHOOKS", fmt.Sprintf(
		`[{"url":%q,"format":"json"}]`, srv.URL))
	res = checkWebhooks()
	if len(res) != 2 || res[0].status != "PASS" || res[0].detail != "1 webhook(s) configured" {
		t.Fatalf("valid = %+v", res)
	}
	if res[1].status != "PASS" || !strings.Contains(res[1].name, "webhook 0 reachability") {
		t.Fatalf("probe = %+v", res[1])
	}

	// A dead endpoint: config still passes, reachability FAILs.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer dead.Close()
	deadURL := dead.URL
	dead.Close() // nothing listens there now
	t.Setenv("NOTIFY_WEBHOOKS", fmt.Sprintf(`[{"url":%q,"format":"slack"}]`, deadURL))
	res = checkWebhooks()
	if len(res) != 2 || res[0].status != "PASS" {
		t.Fatalf("dead = %+v", res)
	}
	if res[1].status != "FAIL" {
		t.Fatalf("probe should fail against a closed listener: %+v", res[1])
	}

	// A malformed list fails before anything is probed.
	t.Setenv("NOTIFY_WEBHOOKS", `[{"url":"https://secret.example/tok?access=SECRET","format":"bogus"}]`)
	res = checkWebhooks()
	if len(res) != 1 || res[0].status != "FAIL" {
		t.Fatalf("invalid = %+v", res)
	}
	if strings.Contains(res[0].detail, "SECRET") || strings.Contains(res[0].detail, "secret.example") {
		t.Fatalf("check output leaked the URL: %q", res[0].detail)
	}
	if !strings.Contains(res[0].detail, "NOTIFY_WEBHOOKS[0]") {
		t.Fatalf("error does not name the index: %q", res[0].detail)
	}
}

// TestCheckNotifyFailures: no state file passes clean; recorded
// failures within 24 h warn with per-webhook counts (by index).
func TestCheckNotifyFailures(t *testing.T) {
	t.Setenv("NOTIFY_STATE_FILE", filepath.Join(t.TempDir(), "state.json"))
	res := checkNotifyFailures()
	if len(res) != 1 || res[0].status != "PASS" {
		t.Fatalf("no file = %+v", res)
	}
	path := os.Getenv("NOTIFY_STATE_FILE")
	now := time.Now()
	b, _ := json.Marshal([]notify.Failure{
		{At: now.Add(-time.Hour), Webhook: 1, Error: "HTTP 503"},
		{At: now.Add(-2 * time.Hour), Webhook: 1, Error: "HTTP 503"},
		{At: now.Add(-30 * time.Hour), Webhook: 0, Error: "old, outside the horizon"},
	})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	res = checkNotifyFailures()
	if len(res) != 1 || res[0].status != "WARN" {
		t.Fatalf("with failures = %+v", res)
	}
	if !strings.Contains(res[0].detail, "webhook 1: 2 dropped") || strings.Contains(res[0].detail, "outside the horizon") {
		t.Fatalf("detail = %q", res[0].detail)
	}
}
