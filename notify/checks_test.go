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

// TestKeptDiskCheck: disk.kept warns when kept checkpoints hold past
// the warn percentage of the snapshot disk, resolves back under it, and
// stays silent on a broken probe or an unreadable disk (#126). A
// negative KeptWarnPct configures the check off.
func TestKeptDiskCheck(t *testing.T) {
	// 400 GiB disk, 160 GiB kept = 40 %: at the default warn level → warn.
	evs := keptDiskCheck(func() (uint64, uint64, error) { return 160 << 30, 400 << 30, nil }, 40, checkNow)
	if len(evs) != 1 || evs[0].Key != KeyDiskKept || evs[0].Severity != Warn || evs[0].Resolved {
		t.Fatalf("kept at warn = %+v", evs)
	}
	if !contains(evs[0].Body, "40% of the snapshot disk") {
		t.Fatalf("body %q lacks the percentage", evs[0].Body)
	}
	// 159.9… GiB kept = under 40 % → resolved.
	evs = keptDiskCheck(func() (uint64, uint64, error) { return 100 << 30, 400 << 30, nil }, 40, checkNow)
	if len(evs) != 1 || !evs[0].Resolved || evs[0].Severity != Warn {
		t.Fatalf("kept under warn = %+v", evs)
	}
	// A broken probe or a zero total is not an incident.
	evs = keptDiskCheck(func() (uint64, uint64, error) { return 0, 0, errors.New("statfs gone") }, 40, checkNow)
	if len(evs) != 0 {
		t.Fatalf("broken probe = %+v, want silence", evs)
	}
	evs = keptDiskCheck(func() (uint64, uint64, error) { return 1 << 30, 0, nil }, 40, checkNow)
	if len(evs) != 0 {
		t.Fatalf("zero total = %+v, want silence", evs)
	}
	// A custom warn level applies.
	evs = keptDiskCheck(func() (uint64, uint64, error) { return 10 << 30, 400 << 30, nil }, 2, checkNow)
	if len(evs) != 1 || evs[0].Resolved {
		t.Fatalf("kept at custom warn = %+v", evs)
	}
	// KeptWarnPct < 0 configures the check off: not registered at all.
	src := &CheckSources{KeptDisk: func() (uint64, uint64, error) { return 399 << 30, 400 << 30, nil }, KeptWarnPct: -1}
	if checks := src.Checks(); len(checks) != 0 {
		t.Fatalf("checks with KeptWarnPct -1 = %d, want 0", len(checks))
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
		LastBackup: func() (time.Time, error) { return time.Time{}, errors.New("skip") },
		GCFailed:   func() error { return nil },
	}
	if got := len(src.Checks()); got != 5 {
		t.Fatalf("checks = %d, want 5 (unit, disk, hugepages, backup, gc)", got)
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
		for _, c := range ProductionSources(nil, "", dir, "spoond", maxAge, nil).Checks() {
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

// TestDrainingCheck: node.draining warns while a healthy node's drain is
// older than the threshold or the node is unhealthy, and resolves when
// it clears; a nil probe yields no check (spoond-52c H3/S3).
func TestDrainingCheck(t *testing.T) {
	healthy := func(forDur time.Duration) func() DrainState {
		return func() DrainState {
			return DrainState{Draining: true, For: forDur, NodeHealthy: true}
		}
	}
	// A healthy node draining under the threshold stays silent (a planned
	// restart under a minute).
	ev := drainingCheck(func() DrainState {
		return DrainState{Draining: true, For: 10 * time.Second, NodeHealthy: true}
	}, time.Minute, checkNow)
	if len(ev) != 1 || !ev[0].Resolved || ev[0].Key != KeyNodeDraining {
		t.Fatalf("healthy brief drain = %+v, want a resolved node.draining", ev)
	}
	// Past the threshold it warns.
	ev = drainingCheck(healthy(2*time.Minute), time.Minute, checkNow)
	if len(ev) != 1 || ev[0].Resolved || ev[0].Key != KeyNodeDraining || ev[0].Severity != Warn {
		t.Fatalf("healthy long drain = %+v, want a warn node.draining", ev)
	}
	// An unhealthy node warns however brief.
	ev = drainingCheck(func() DrainState {
		return DrainState{Draining: true, For: time.Second, NodeHealthy: false}
	}, time.Minute, checkNow)
	if len(ev) != 1 || ev[0].Resolved || ev[0].Key != KeyNodeDraining {
		t.Fatalf("unhealthy node = %+v, want a warn node.draining", ev)
	}
	// Not draining resolves.
	ev = drainingCheck(func() DrainState { return DrainState{} }, time.Minute, checkNow)
	if len(ev) != 1 || !ev[0].Resolved || ev[0].Key != KeyNodeDraining {
		t.Fatalf("undrained = %+v, want a resolved node.draining", ev)
	}

	// The source reaches Checks, and a nil Draining adds no check.
	src := &CheckSources{Draining: healthy(time.Minute)}
	if got := len(src.Checks()); got != 1 {
		t.Fatalf("Draining source checks = %d, want 1", got)
	}
	if got := len((&CheckSources{}).Checks()); got != 0 {
		t.Fatalf("nil Draining checks = %d, want 0", got)
	}
}
