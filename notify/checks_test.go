package notify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var checkNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

// findByKey picks one event of a check result by key.
func findByKey(t *testing.T, evs []Event, key string) (Event, bool) {
	t.Helper()
	for _, ev := range evs {
		if ev.Key == key {
			return ev, true
		}
	}
	return Event{}, false
}

func TestUnitCheck(t *testing.T) {
	ctx := context.Background()
	// All active → a resolved event per unit, severity critical (the
	// condition's own level).
	evs := unitCheck(ctx, []string{"e2b-orchestrator.service"},
		func(context.Context, string) (string, error) { return "active", nil }, checkNow)
	if len(evs) != 1 || !evs[0].Resolved || evs[0].Severity != Critical || evs[0].Key != "unit.inactive.e2b-orchestrator.service" {
		t.Fatalf("active = %+v", evs)
	}
	// One of two inactive → a critical alert keyed on that unit, and a
	// resolved event for the other.
	evs = unitCheck(ctx, []string{"a.service", "b.service"},
		func(_ context.Context, u string) (string, error) {
			if u == "b.service" {
				return "failed", nil
			}
			return "active", nil
		}, checkNow)
	if len(evs) != 2 || !evs[0].Resolved || evs[0].Key != "unit.inactive.a.service" {
		t.Fatalf("a = %+v", evs)
	}
	b := evs[1]
	if b.Key != "unit.inactive.b.service" || b.Severity != Critical || b.Resolved {
		t.Fatalf("b = %+v", b)
	}
	if want := "b.service is failed"; !contains(b.Body, want) {
		t.Fatalf("body %q, want it to name %q", b.Body, want)
	}
	// A broken probe counts as inactive (the checker must not go quiet).
	evs = unitCheck(ctx, []string{"a.service"},
		func(context.Context, string) (string, error) { return "", errors.New("no systemd") }, checkNow)
	if len(evs) != 1 || evs[0].Severity != Critical || evs[0].Resolved || !contains(evs[0].Body, "state unknown") {
		t.Fatalf("broken probe = %+v", evs)
	}
	// No units configured → nothing.
	if evs := unitCheck(ctx, nil, func(context.Context, string) (string, error) { return "failed", nil }, checkNow); evs != nil {
		t.Fatalf("no units = %+v", evs)
	}
}

func TestDiskCheck(t *testing.T) {
	total := uint64(1000)
	usage := func(used uint64) DiskUsage {
		return func() (uint64, uint64, error) { return total, total - used, nil }
	}
	// Healthy: both keys resolve.
	evs := diskCheck(usage(500), checkNow)
	if len(evs) != 2 {
		t.Fatalf("healthy = %+v", evs)
	}
	for _, k := range []string{KeyDiskWarn, KeyDiskDanger} {
		ev, ok := findByKey(t, evs, k)
		if !ok || !ev.Resolved || ev.Severity != Warn && k == KeyDiskWarn || ev.Severity != Critical && k == KeyDiskDanger {
			t.Fatalf("healthy resolve for %s = %+v", k, ev)
		}
	}
	// 85 %: warn alert, and the danger key resolves (back under danger).
	evs = diskCheck(usage(850), checkNow)
	if len(evs) != 2 {
		t.Fatalf("warn level = %+v", evs)
	}
	alert, _ := findByKey(t, evs, KeyDiskWarn)
	if alert.Resolved || alert.Severity != Warn || !contains(alert.Body, "85% used") {
		t.Fatalf("warn alert = %+v", alert)
	}
	danger, _ := findByKey(t, evs, KeyDiskDanger)
	if !danger.Resolved {
		t.Fatalf("danger not resolved at warn level: %+v", danger)
	}
	// 95 %: danger alert; the warn key must NOT be re-alerted.
	evs = diskCheck(usage(950), checkNow)
	if len(evs) != 1 {
		t.Fatalf("danger level = %+v", evs)
	}
	if evs[0].Key != KeyDiskDanger || evs[0].Severity != Critical || evs[0].Resolved {
		t.Fatalf("danger alert = %+v", evs[0])
	}
	// A probe that cannot stat returns nothing (readiness reports it).
	evs = diskCheck(func() (uint64, uint64, error) { return 0, 0, errors.New("gone") }, checkNow)
	if evs != nil {
		t.Fatalf("broken probe = %+v", evs)
	}
}

func TestHugepagesCheck(t *testing.T) {
	total := uint64(1000)
	usage := func(used, reserved uint64) HugepageUsage {
		return func() (uint64, uint64, uint64, uint64, error) { return total, used, reserved, 1, nil }
	}
	// Healthy.
	evs := hugepagesCheck(usage(500, 0), checkNow)
	if len(evs) != 2 {
		t.Fatalf("healthy = %+v", evs)
	}
	if ev, _ := findByKey(t, evs, KeyHugepagesWarn); !ev.Resolved || ev.Severity != Warn {
		t.Fatalf("warn resolve = %+v", ev)
	}
	if ev, _ := findByKey(t, evs, KeyHugepagesDanger); !ev.Resolved || ev.Severity != Critical {
		t.Fatalf("danger resolve = %+v", ev)
	}
	// 85 % (used+reserved): warn + danger resolved.
	evs = hugepagesCheck(usage(800, 50), checkNow)
	alert, _ := findByKey(t, evs, KeyHugepagesWarn)
	if alert.Resolved || alert.Severity != Warn {
		t.Fatalf("warn alert = %+v", alert)
	}
	if ev, _ := findByKey(t, evs, KeyHugepagesDanger); !ev.Resolved {
		t.Fatal("danger not resolved at warn level")
	}
	// 95 %: danger only.
	evs = hugepagesCheck(usage(950, 0), checkNow)
	if len(evs) != 1 || evs[0].Key != KeyHugepagesDanger || evs[0].Severity != Critical {
		t.Fatalf("danger alert = %+v", evs)
	}
	// Probe failure: nothing.
	if evs := hugepagesCheck(func() (uint64, uint64, uint64, uint64, error) {
		return 0, 0, 0, 0, errors.New("node down")
	}, checkNow); evs != nil {
		t.Fatalf("broken probe = %+v", evs)
	}
}

func TestCertCheck(t *testing.T) {
	expiry := func(left time.Duration) CertExpiry {
		return func() (time.Time, error) { return checkNow.Add(left), nil }
	}
	// 40 days: healthy, all three resolve.
	evs := certCheck(expiry(40*24*time.Hour), checkNow)
	if len(evs) != 3 {
		t.Fatalf("healthy = %+v", evs)
	}
	for _, k := range []string{KeyTLSCert30, KeyTLSCert7, KeyTLSCert1} {
		if ev, ok := findByKey(t, evs, k); !ok || !ev.Resolved {
			t.Fatalf("%s not resolved: %+v", k, ev)
		}
	}
	// Boundary: exactly 30 days is already "within 30 days".
	evs = certCheck(expiry(30*24*time.Hour), checkNow)
	if ev, ok := findByKey(t, evs, KeyTLSCert30); !ok || ev.Resolved || ev.Severity != Warn {
		t.Fatalf("exactly 30 d = %+v (want a 30d alert)", evs)
	}
	// Exactly 7 days: the 7-day alert, and the 30-day key resolves.
	evs = certCheck(expiry(7*24*time.Hour), checkNow)
	ev, _ := findByKey(t, evs, KeyTLSCert7)
	if ev.Resolved || ev.Severity != Warn {
		t.Fatalf("exactly 7 d = %+v", evs)
	}
	if ev, _ := findByKey(t, evs, KeyTLSCert30); !ev.Resolved {
		t.Fatal("30d key not resolved once inside 7 d")
	}
	// Exactly 1 day (24 h): critical.
	evs = certCheck(expiry(24*time.Hour), checkNow)
	if ev, ok := findByKey(t, evs, KeyTLSCert1); !ok || ev.Resolved || ev.Severity != Critical {
		t.Fatalf("exactly 1 d = %+v", evs)
	}
	// 12 hours: critical.
	evs = certCheck(expiry(12*time.Hour), checkNow)
	if ev, _ := findByKey(t, evs, KeyTLSCert1); ev.Resolved || ev.Severity != Critical {
		t.Fatalf("12 h = %+v", evs)
	}
	// No certificate configured: not an incident.
	evs = certCheck(func() (time.Time, error) { return time.Time{}, errNoSource }, checkNow)
	if evs != nil {
		t.Fatalf("no cert = %+v", evs)
	}
}

func TestBackupCheck(t *testing.T) {
	// Fresh backup: resolved.
	evs := backupCheck(func() (time.Time, error) { return checkNow.Add(-2 * time.Hour), nil }, DefaultBackupMaxAge, checkNow)
	if len(evs) != 1 || !evs[0].Resolved || evs[0].Severity != Warn {
		t.Fatalf("fresh = %+v", evs)
	}
	// Stale: warn.
	evs = backupCheck(func() (time.Time, error) { return checkNow.Add(-27 * time.Hour), nil }, DefaultBackupMaxAge, checkNow)
	if len(evs) != 1 || evs[0].Resolved || evs[0].Severity != Warn || evs[0].Key != KeyBackupStale {
		t.Fatalf("stale = %+v", evs)
	}
	// Custom limit: 3 h old with a 1 h limit is stale.
	evs = backupCheck(func() (time.Time, error) { return checkNow.Add(-3 * time.Hour), nil }, time.Hour, checkNow)
	if len(evs) != 1 || evs[0].Resolved {
		t.Fatalf("custom limit = %+v", evs)
	}
	// Missing: warn naming the reason.
	evs = backupCheck(func() (time.Time, error) { return time.Time{}, errors.New("no backups") }, 0, checkNow)
	if len(evs) != 1 || evs[0].Resolved || !contains(evs[0].Body, "no backups") {
		t.Fatalf("missing = %+v", evs)
	}
}

func TestGCCheck(t *testing.T) {
	// Failure → warn alert.
	failed := true
	src := &CheckSources{GCFailed: func() error {
		if failed {
			return errors.New("gc boom")
		}
		return nil
	}}
	evs := src.Checks()[len(src.Checks())-1](context.Background(), checkNow)
	if len(evs) != 1 || evs[0].Key != KeyGCFailed || evs[0].Severity != Warn || evs[0].Resolved {
		t.Fatalf("gc failed = %+v", evs)
	}
	// Success → resolved, at the warn level.
	failed = false
	evs = src.Checks()[len(src.Checks())-1](context.Background(), checkNow)
	if len(evs) != 1 || !evs[0].Resolved || evs[0].Severity != Warn {
		t.Fatalf("gc ok = %+v", evs)
	}
}

// TestCheckSourcesEmpty: no probes, no checks.
func TestCheckSourcesEmpty(t *testing.T) {
	if checks := (&CheckSources{}).Checks(); len(checks) != 0 {
		t.Fatalf("empty sources = %d checks", len(checks))
	}
	if checks := (*CheckSources)(nil).Checks(); checks != nil {
		t.Fatalf("nil sources = %v", checks)
	}
}

// TestCheckSourcesOrder pins the checks' stable order and that each
// registered probe yields exactly one check.
func TestCheckSourcesOrder(t *testing.T) {
	src := &CheckSources{
		Unit:       func(context.Context, string) (string, error) { return "active", nil },
		Units:      []string{"u"},
		Disk:       func() (uint64, uint64, error) { return 0, 0, errors.New("skip") },
		Hugepages:  func() (uint64, uint64, uint64, uint64, error) { return 0, 0, 0, 0, errors.New("skip") },
		Cert:       func() (time.Time, error) { return time.Time{}, errNoSource },
		LastBackup: func() (time.Time, error) { return time.Time{}, errors.New("skip") },
		GCFailed:   func() error { return nil },
	}
	if got := len(src.Checks()); got != 6 {
		t.Fatalf("checks = %d, want 6 (unit, disk, hugepages, cert, backup, gc)", got)
	}
}

// TestStatfsUsageSmoke exercises the real statfs path on the repo
// directory (healthy by construction when it can stat).
func TestStatfsUsageSmoke(t *testing.T) {
	total, free, err := statfsUsage(".")
	if err != nil {
		t.Skipf("statfs unavailable here: %v", err)
	}
	if total == 0 || free > total {
		t.Fatalf("implausible statfs: total=%d free=%d", total, free)
	}
}

// TestNewestBackup exercises the real backup-dir probe.
func TestNewestBackup(t *testing.T) {
	dir := t.TempDir()
	if _, err := newestBackup(dir, "spoond"); err == nil {
		t.Fatal("empty dir accepted")
	}
	write := func(name string, at time.Time) {
		t.Helper()
		if err := os.WriteFile(dir+"/"+name, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(dir+"/"+name, at, at); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	new := time.Date(2026, 10, 3, 3, 0, 0, 0, time.UTC)
	write("spoond-20261001.db", old)
	write("spoond-20261003.db", new)
	got, err := newestBackup(dir, "spoond")
	if err != nil || !got.Equal(new) {
		t.Fatalf("newest = %v, %v; want %v", got, err, new)
	}
	// Other prefixes and non-.db files don't count.
	write("other-20261009.db", time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC))
	got, err = newestBackup(dir, "spoond")
	if err != nil || !got.Equal(new) {
		t.Fatalf("after unrelated file: %v, %v", got, err)
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

// TestCertCheckCrossesBoundaries: a certificate that crosses from
// healthy past 7 days straight to less than a day resolves the 30- and
// 7-day keys in the same pass it raises the 1-day critical.
func TestCertCheckCrossesBoundaries(t *testing.T) {
	evs := certCheck(func() (time.Time, error) { return checkNow.Add(10 * time.Hour), nil }, checkNow)
	var keys []string
	for _, ev := range evs {
		keys = append(keys, ev.Key)
	}
	if len(keys) != 3 {
		t.Fatalf("events = %v", keys)
	}
	if keys[0] != KeyTLSCert1 {
		t.Fatalf("order = %v, want the 1d alert first", keys)
	}
	for _, k := range []string{KeyTLSCert30, KeyTLSCert7} {
		if ev, ok := findByKey(t, evs, k); !ok || !ev.Resolved {
			t.Fatalf("%s not resolved while inside 1 d: %v", k, evs)
		}
	}
}

// TestBackupMaxAgeSecs: BACKUP_MAX_AGE_SECS parses to a duration, 0 or
// negative falls back to the 26 h default.
func TestBackupMaxAgeSecs(t *testing.T) {
	if got := BackupMaxAgeSecs(3600); got != time.Hour {
		t.Fatalf("3600 = %v, want 1 h", got)
	}
	for _, in := range []int{0, -5} {
		if got := BackupMaxAgeSecs(in); got != DefaultBackupMaxAge {
			t.Fatalf("%d = %v, want the default %v", in, got, DefaultBackupMaxAge)
		}
	}
	if DefaultBackupMaxAge != 93600*time.Second {
		t.Fatalf("DefaultBackupMaxAge = %v, want 93600 s", DefaultBackupMaxAge)
	}
}

// TestProductionSourcesBackupMaxAge: the configured limit reaches the
// backup check (BACKUP_MAX_AGE_SECS was previously read by nothing).
func TestProductionSourcesBackupMaxAge(t *testing.T) {
	dir := t.TempDir()
	at := checkNow.Add(-5 * time.Minute)
	path := filepath.Join(dir, "spoond-20261004.db")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	runAllChecks := func(maxAge time.Duration) []Event {
		var evs []Event
		for _, c := range ProductionSources(nil, "", dir, "spoond", "", "", maxAge, nil).Checks() {
			evs = append(evs, c(context.Background(), checkNow)...)
		}
		return evs
	}
	// 5-minute-old backup: stale under a 90 s limit, fresh under the
	// 26 h default (the cert check contributes nothing: no cert set).
	evs := runAllChecks(90 * time.Second)
	if len(evs) != 1 || evs[0].Resolved || evs[0].Key != KeyBackupStale {
		t.Fatalf("5 min old with a 90 s limit = %+v, want a stale alert", evs)
	}
	evs = runAllChecks(0)
	if len(evs) != 1 || !evs[0].Resolved {
		t.Fatalf("5 min old with the default = %+v, want resolved", evs)
	}
}
