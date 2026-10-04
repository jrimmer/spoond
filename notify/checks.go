package notify

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Condition keys. Each periodic condition has exactly one stable key,
// so its repeats dedupe to one per hour and a clearing emits one
// resolved event for the same key.
const (
	KeyUnitInactive    = "unit.inactive"
	KeyDiskWarn        = "disk.warn"
	KeyDiskDanger      = "disk.danger"
	KeyHugepagesWarn   = "hugepages.warn"
	KeyHugepagesDanger = "hugepages.danger"
	KeyTLSCert30       = "tls.cert.30d"
	KeyTLSCert7        = "tls.cert.7d"
	KeyTLSCert1        = "tls.cert.1d"
	KeyGCFailed        = "gc.failed"
	KeyBackupStale     = "backup.stale"
)

// Warn/danger levels for the snapshot disk and the hugepage pool: the
// dashboard's meters (cmd/spoond-dash: disk 80 %/90 % used, hugepages
// 80 %/92 % used).
const (
	DiskWarnUsedPct        = 80.0
	DiskDangerUsedPct      = 90.0
	HugepagesWarnUsedPct   = 80.0
	HugepagesDangerUsedPct = 92.0
)

// DefaultBackupMaxAge is how old the newest database backup may get
// before the backup check alerts (BACKUP_MAX_AGE_SECS, default 93600
// = 26 h: the 03:00 daily run plus one missed day).
const DefaultBackupMaxAge = 93600 * time.Second

// DefaultDiskPath and DefaultBackupDir mirror the backend's defaults
// (E2B_TEMPLATE_STORAGE_PATH, SPOOND_BACKUP_DIR): the snapshot disk
// and the backup directory the periodic checks watch when the env is
// unset. DefaultUnits are the systemd units whose absence means the
// deployment is down (the backend itself, and the SSH gateway).
var (
	DefaultDiskPath   = "/forkdcache/e2b/storage/templates"
	DefaultBackupDir  = "/var/lib/spoond/backups"
	DefaultBackupPref = "spoond"
	DefaultUnits      = []string{"spoond-backend.service", "spoond-sshd-gateway.service"}
)

// SystemdUnit reports a systemd unit's ActiveState. Replaced in tests.
type SystemdUnit func(ctx context.Context, unit string) (activeState string, err error)

// DiskUsage reports the snapshot disk's total and free bytes
// (statfs). Replaced in tests.
type DiskUsage func() (total, free uint64, err error)

// HugepageUsage reports the hugepage pool's total/used/reserved page
// counts and page size (the substrate's NodeInfo). Replaced in tests.
type HugepageUsage func() (total, used, reserved uint64, pageBytes uint64, err error)

// CertExpiry reports the configured TLS certificate's NotAfter.
// Replaced in tests.
type CertExpiry func() (time.Time, error)

// LastBackup reports the modification time of the newest database
// backup, or an error when none is usable. Replaced in tests.
type LastBackup func() (time.Time, error)

// GCLastError reports the most recent GC pass's error (nil when the
// last pass succeeded, and nil before the first pass: a GC that has
// not run yet is not a failure). Replaced in tests.
type GCLastError func() error

// CheckSources carries the probes the periodic checks read. Every
// field is optional: a nil source disables its checks (no TLS_CERT
// disables the certificate check, no backup directory the backup
// check) — absence of configuration is not an incident.
type CheckSources struct {
	Unit         SystemdUnit
	Units        []string // systemd units to watch (e.g. spoond-backend)
	Disk         DiskUsage
	Hugepages    HugepageUsage
	Cert         CertExpiry
	LastBackup   LastBackup
	GCFailed     GCLastError
	BackupMaxAge time.Duration // 0 = DefaultBackupMaxAge
}

// ProductionSources builds the checks the backend runs: systemd units
// via systemctl, the snapshot disk via statfs, the certificate from
// TLS_CERT/TLS_KEY, the newest backup from the backup directory (its
// age limit from BACKUP_MAX_AGE_SECS, 0 or unset meaning
// DefaultBackupMaxAge), and whatever lastError reports for the GC.
// Hugepage probing needs the substrate, so the caller installs it
// separately — HugepagesFromNodeInfo adapts a NodeInfo fetch.
func ProductionSources(units []string, diskPath, backupDir, backupPrefix, tlsCert, tlsKey string, backupMaxAge time.Duration, lastError GCLastError) *CheckSources {
	if diskPath == "" {
		diskPath = DefaultDiskPath
	}
	if backupDir == "" {
		backupDir = DefaultBackupDir
	}
	if backupPrefix == "" {
		backupPrefix = DefaultBackupPref
	}
	return &CheckSources{
		Unit:  systemdActiveState,
		Units: units,
		Disk: func() (uint64, uint64, error) {
			return statfsUsage(diskPath)
		},
		Cert: func() (time.Time, error) {
			return certNotAfter(tlsCert, tlsKey)
		},
		LastBackup: func() (time.Time, error) {
			return newestBackup(backupDir, backupPrefix)
		},
		BackupMaxAge: backupMaxAge,
		GCFailed:     lastError,
	}
}

// BackupMaxAge secs reads the backup age limit in seconds
// (BACKUP_MAX_AGE_SECS): 0 or a non-positive value means
// DefaultBackupMaxAge.
func BackupMaxAgeSecs(secs int) time.Duration {
	if secs <= 0 {
		return DefaultBackupMaxAge
	}
	return time.Duration(secs) * time.Second
}

// HugepagesFromNodeInfo adapts a substrate NodeInfo fetch into the
// HugepageUsage probe: the pool's total/used/reserved pages and page
// size straight from the orchestrator, as the dashboard reads them.
func HugepagesFromNodeInfo(fetch func(ctx context.Context) (total, used, reserved, pageBytes uint64, err error)) HugepageUsage {
	if fetch == nil {
		return nil
	}
	return func() (uint64, uint64, uint64, uint64, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return fetch(ctx)
	}
}

// Checks builds the periodic checks from src, in a stable order.
func (src *CheckSources) Checks() []Check {
	if src == nil {
		return nil
	}
	var out []Check
	if src.Unit != nil {
		units, state := src.Units, src.Unit
		out = append(out, func(ctx context.Context, now time.Time) []Event {
			return unitCheck(ctx, units, state, now)
		})
	}
	if src.Disk != nil {
		disk := src.Disk
		out = append(out, func(_ context.Context, now time.Time) []Event {
			return diskCheck(disk, now)
		})
	}
	if src.Hugepages != nil {
		hp := src.Hugepages
		out = append(out, func(_ context.Context, now time.Time) []Event {
			return hugepagesCheck(hp, now)
		})
	}
	if src.Cert != nil {
		cert := src.Cert
		out = append(out, func(_ context.Context, now time.Time) []Event {
			return certCheck(cert, now)
		})
	}
	if src.LastBackup != nil {
		latest, maxAge := src.LastBackup, src.BackupMaxAge
		if maxAge <= 0 {
			maxAge = DefaultBackupMaxAge
		}
		out = append(out, func(_ context.Context, now time.Time) []Event {
			return backupCheck(latest, maxAge, now)
		})
	}
	if src.GCFailed != nil {
		gc := src.GCFailed
		out = append(out, func(_ context.Context, now time.Time) []Event {
			if err := gc(); err != nil {
				return []Event{{
					Key:      KeyGCFailed,
					Severity: Warn,
					Title:    "Snapshot GC pass failed",
					Body:     err.Error(),
					At:       now,
				}}
			}
			return []Event{resolved(KeyGCFailed, Warn, now)}
		})
	}
	return out
}

// resolved builds the clearing event for one condition key. The
// severity is the condition's own alert level, not a flat Info: a
// webhook filtered to min_severity warn was told about the problem and
// must also hear that it cleared. Delivery order preserves the flag —
// Resolved is what marks the message, not the severity.
func resolved(key string, severity Severity, now time.Time) Event {
	return Event{Key: key, Severity: severity, Resolved: true, At: now}
}

// unitCheck emits critical events for configured units whose
// ActiveState is not "active", and one resolved event when every
// watched unit is active again. A probe that cannot run (no systemd,
// a missing unit) counts as inactive: the checker must not go quiet
// on a broken probe.
func unitCheck(ctx context.Context, units []string, state SystemdUnit, now time.Time) []Event {
	var inactive []string
	for _, u := range units {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		st, err := state(ctx, u)
		if err != nil {
			inactive = append(inactive, fmt.Sprintf("%s (state unknown: %v)", u, err))
			continue
		}
		if st != "active" {
			inactive = append(inactive, fmt.Sprintf("%s is %s", u, st))
		}
	}
	if len(units) == 0 {
		return nil
	}
	if len(inactive) > 0 {
		return []Event{{
			Key:      KeyUnitInactive,
			Severity: Critical,
			Title:    "systemd unit not active",
			Body:     strings.Join(inactive, "; "),
			At:       now,
		}}
	}
	return []Event{resolved(KeyUnitInactive, Critical, now)}
}

// diskCheck watches the snapshot disk past the dashboard's warn
// (80 % used) and danger (90 %) levels: warn at warn, critical at
// danger, each its own key, both resolved when back under.
func diskCheck(usage DiskUsage, now time.Time) []Event {
	total, free, err := usage()
	if err != nil || total == 0 {
		return nil // cannot stat: not an incident; readiness reports it
	}
	usedPct := float64(total-free) / float64(total) * 100
	switch {
	case usedPct >= DiskDangerUsedPct:
		return []Event{{
			Key:      KeyDiskDanger,
			Severity: Critical,
			Title:    "Snapshot disk past danger level",
			Body:     fmt.Sprintf("%.0f%% used (%s free), danger level %.0f%%", usedPct, humanBytes(free), DiskDangerUsedPct),
			At:       now,
		}}
	case usedPct >= DiskWarnUsedPct:
		// Past warn but back under danger: the danger condition cleared
		// with the level, so its key resolves here and now.
		return []Event{{
			Key:      KeyDiskWarn,
			Severity: Warn,
			Title:    "Snapshot disk past warn level",
			Body:     fmt.Sprintf("%.0f%% used (%s free), warn level %.0f%%", usedPct, humanBytes(free), DiskWarnUsedPct),
			At:       now,
		}, resolved(KeyDiskDanger, Critical, now)}
	default:
		return []Event{resolved(KeyDiskWarn, Warn, now), resolved(KeyDiskDanger, Critical, now)}
	}
}

// hugepagesCheck watches the hugepage pool past the dashboard's warn
// (80 % used) and danger (92 %) levels. The pool counts as used when
// it is not free: reserved pages sit inside free, and matching the
// dashboard's accounting keeps the two from disagreeing.
func hugepagesCheck(usage HugepageUsage, now time.Time) []Event {
	total, used, reserved, pageBytes, err := usage()
	if err != nil || total == 0 {
		return nil
	}
	busy := min64(used+reserved, total)
	usedPct := float64(busy) / float64(total) * 100
	free := (total - busy) * pageBytes
	what := fmt.Sprintf("%.0f%% of the pool used, %s free", usedPct, humanBytes(free))
	switch {
	case usedPct >= HugepagesDangerUsedPct:
		return []Event{{
			Key:      KeyHugepagesDanger,
			Severity: Critical,
			Title:    "Hugepage pool past danger level",
			Body:     what,
			At:       now,
		}}
	case usedPct >= HugepagesWarnUsedPct:
		// Back under danger but past warn: resolve the danger key here.
		return []Event{{
			Key:      KeyHugepagesWarn,
			Severity: Warn,
			Title:    "Hugepage pool past warn level",
			Body:     what,
			At:       now,
		}, resolved(KeyHugepagesDanger, Critical, now)}
	default:
		return []Event{resolved(KeyHugepagesWarn, Warn, now), resolved(KeyHugepagesDanger, Critical, now)}
	}
}

// certCheck watches the configured TLS certificate's expiry at 30 and
// 7 days (warn) and 1 day (critical). Each threshold is its own key;
// passing a deeper threshold resolves the shallower one, and renewal
// resolves the deepest still-open one.
func certCheck(expiry CertExpiry, now time.Time) []Event {
	notAfter, err := expiry()
	if err != nil {
		return nil // no certificate configured: the doctor reports that
	}
	left := notAfter.Sub(now)
	// A certificate with exactly 30, 7 or 1 day left is already inside
	// that threshold: "within 30 days" includes the day itself, so the
	// comparison is days*24h (inclusive), not a whole-day count. Each
	// deeper threshold resolves every shallower one — a certificate can
	// cross several boundaries between two passes.
	switch {
	case left <= 24*time.Hour:
		return []Event{{
			Key:      KeyTLSCert1,
			Severity: Critical,
			Title:    "TLS certificate expires within a day",
			Body:     fmt.Sprintf("expires %s (%s left)", notAfter.UTC().Format(time.RFC3339), left.Round(time.Minute)),
			At:       now,
		}, resolved(KeyTLSCert30, Warn, now), resolved(KeyTLSCert7, Warn, now)}
	case left <= 7*24*time.Hour:
		return []Event{{
			Key:      KeyTLSCert7,
			Severity: Warn,
			Title:    "TLS certificate expires within a week",
			Body:     fmt.Sprintf("expires %s (%s left)", notAfter.UTC().Format(time.RFC3339), left.Round(24*time.Hour)),
			At:       now,
		}, resolved(KeyTLSCert30, Warn, now), resolved(KeyTLSCert1, Critical, now)}
	case left <= 30*24*time.Hour:
		return []Event{{
			Key:      KeyTLSCert30,
			Severity: Warn,
			Title:    "TLS certificate expires within 30 days",
			Body:     fmt.Sprintf("expires %s (%s left)", notAfter.UTC().Format(time.RFC3339), left.Round(24*time.Hour)),
			At:       now,
		}, resolved(KeyTLSCert7, Warn, now), resolved(KeyTLSCert1, Critical, now)}
	default:
		return []Event{resolved(KeyTLSCert30, Warn, now), resolved(KeyTLSCert7, Warn, now), resolved(KeyTLSCert1, Critical, now)}
	}
}

// backupCheck warns when the newest database backup is older than
// maxAge (BACKUP_MAX_AGE_SECS). A missing or unreadable backup counts
// as stale.
func backupCheck(latest LastBackup, maxAge time.Duration, now time.Time) []Event {
	at, err := latest()
	if err != nil {
		return []Event{{
			Key:      KeyBackupStale,
			Severity: Warn,
			Title:    "Database backup missing",
			Body:     fmt.Sprintf("no usable backup found (%v); the age limit is %s", err, maxAge),
			At:       now,
		}}
	}
	age := now.Sub(at)
	if age > maxAge {
		return []Event{{
			Key:      KeyBackupStale,
			Severity: Warn,
			Title:    "Database backup stale",
			Body:     fmt.Sprintf("newest backup is %s old (limit %s), from %s", age.Round(time.Minute), maxAge, at.UTC().Format(time.RFC3339)),
			At:       now,
		}}
	}
	return []Event{resolved(KeyBackupStale, Warn, now)}
}

// systemdActiveState reads one unit's ActiveState via systemctl show.
var systemdActiveState = func(ctx context.Context, unit string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "systemctl", "show", unit, "--property=ActiveState", "--value").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// statfsUsage reports path's total and free bytes; the notify checks
// read it through DiskUsage so tests can replace it.
func statfsUsage(path string) (total, free uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bsize := uint64(st.Bsize)
	return st.Blocks * bsize, st.Bavail * bsize, nil
}

// newestBackup finds the newest <prefix>-*.db file's mod time in dir.
func newestBackup(dir, prefix string) (time.Time, error) {
	if dir == "" {
		return time.Time{}, fmt.Errorf("no backup directory configured")
	}
	matches, err := filepath.Glob(filepath.Join(dir, prefix+"-*.db"))
	if err != nil {
		return time.Time{}, err
	}
	var newest time.Time
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	if newest.IsZero() {
		return time.Time{}, fmt.Errorf("%s: no readable %s-*.db backup", dir, prefix)
	}
	return newest, nil
}

// certNotAfter loads the configured certificate pair and reports its
// leaf's NotAfter. An unconfigured pair is errNoSource, not an error:
// a plain-HTTP deployment has nothing to watch.
var errNoSource = fmt.Errorf("not configured")

func certNotAfter(certFile, keyFile string) (time.Time, error) {
	if certFile == "" || keyFile == "" {
		return time.Time{}, errNoSource
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return time.Time{}, err
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return time.Time{}, err
	}
	return leaf.NotAfter, nil
}

// humanBytes renders a byte count for event bodies.
func humanBytes(n uint64) string {
	const unit = 1 << 10
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for ; n >= div<<10 && exp < 5; exp++ {
		div <<= 10
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func min64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}
