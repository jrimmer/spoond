// Command spoond-backend runs the lease API backend on the E2B
// substrate (U08).
//
// Configuration is read from the environment:
//
//	E2B_GRPC_ADDR     orchestrator gRPC (default 127.0.0.1:5008)
//	E2B_PROXY_URL     orchestrator sandbox proxy (default http://127.0.0.1:5007)
//	E2B_TOKEN_SEED_FILE  envd/traffic HMAC seed file (default /etc/spoond/e2b-token-seed)
//	E2B_TEAM_ID       fixed team UUID sent on every request
//	E2B_ENVD_VERSION  the host's envd version (empty = unknown). A named
//	                  snapshot saved against a different envd cannot start
//	                  here (2.7, #83).
//	E2B_FIRECRACKER_VERSION  the host's firecracker version (empty =
//	                  unknown); likewise gates a named-snapshot start.
//	BIND_ADDR         listen address (default 127.0.0.1:8890)
//	PROXY_ADDR        public proxy listener (e.g. 0.0.0.0:8891)
//	SPOOND_DB_PATH    SQLite database path (default /var/lib/spoond/spoond.db)
//	TLS_CERT, TLS_KEY  serve HTTPS when both are set; comma-separated lists
//	                   serve several pairs by SNI and reload on change
//	CONSUMER_TOKENS   comma-separated token=consumer pairs (e.g. "abc=forgejo,def=pi")
//	POOL_SIZE         warm-pool size per image (default 0 = disabled)
//	DEFAULT_TTL_SECS  default lease TTL (default 300)
//	MAX_TTL_SECS      max lease TTL (default 3600)
//	HOST_GUEST_SERVICE_ADDR  address guests use to reach host services (required)
//	HOST_GUEST_SERVICE_PORT  host port guests use (default 8891)
//	SPOOND_GUEST_DNS_ADDR  guest DNS resolver(s) granted on port 53 and baked
//	                  into the guest image; comma-separated (empty = no
//	                  resolver allowance)
//	SPOOND_PROXY_HOST_SUFFIX  wildcard hostname suffix the HTTP proxy
//	                  routes (default .sandbox.example.com)
//	METRICS_TOKEN     bearer that may read /metrics and nothing else
//	                  (Prometheus, spoond dash); empty disables
//	EVENTS_TOKEN      bearer that may read the lease event streams and
//	                  nothing else (spoond dash); empty disables
//	HOST_API_PORT     lease API port lan/internet guests may reach on
//	                  HOST_GUEST_SERVICE_ADDR (default: BIND_ADDR's port)
//	CHECKPOINT_INTERVAL_MINS  default per-lease checkpoint interval in
//	                  minutes for leases without their own (2.3; default 0 =
//	                  never; a lease's checkpoint_interval overrides)
//	MAX_KEPT_PER_LEASE  kept-checkpoint cap per lease (#126; default 4;
//	                  0 = no cap). A keep on a lease already at the cap
//	                  answers 409; nothing is evicted.
//	MAX_NAMED_SNAPSHOTS  per-owner cap on distinct named-snapshot names
//	                  (2.7, #83; default 64; 0 = no cap).
//	SNAPSHOT_KEEP_VERSIONS  default named-snapshot retention: the last N
//	                  versions of a name survive a save (2.7, #83;
//	                  default 3).
//	ADMIN_TOKEN       bearer token for /api/admin/* (empty disables)
//	E2B_TEMPLATE_STORAGE_PATH  build storage root, for disk accounting
//	                  (default /forkdcache/e2b/storage/templates)
//	SPOOND_BUILD_TIMEOUT  how long a template build may run before the
//	                  GC treats a still-`building` row as stale and fails
//	                  it; a Go duration or seconds (spoond-4yl). The same
//	                  variable bounds `spoond images build`. Default 1h.
//	SPOOND_BACKUP_DIR directory for daily SQLite backups (U11; default
//	                  /var/lib/spoond/backups; VACUUM INTO daily at 03:00
//	                  local, plus at start when the newest is older than 24 h)
//	LLM_UPSTREAM_URL  OpenAI-compatible LLM API base for the per-lease
//	                  LLM gateway (e.g. https://openrouter.ai/api/v1)
//	LLM_UPSTREAM_KEY  server-side key for that upstream (never sent to
//	                  sandboxes; empty disables the gateway)
//	LLM_MAX_CONCURRENT_PER_USER  per-user in-flight LLM gateway request
//	                  cap (0 = unlimited; U8/T8). Per-user LLM keys are
//	                  store data, set via POST /api/users/{id}/llm-key,
//	                  not env config.
//	GC_LOST_GRACE_PERSISTENT     how long a persistent lease's snapshots
//	                  stay kept after the lease is lost (Go duration;
//	                  default 168h = 7 d)
//	GC_LOST_GRACE   how long a non-persistent lease's snapshots stay
//	                  kept after the lease is lost (Go duration;
//	                  default 24h = 1 d)
//	ORPHAN_REAP   what the GC does with an orphan build directory
//	                  under E2B_TEMPLATE_STORAGE_PATH: off (hard stop,
//	                  change nothing), dryrun (log it; default), or
//	                  quarantine (move it to
//	                  <storage path>/../quarantine/<id> and delete it
//	                  only after ORPHAN_QUARANTINE_SECS)
//	ORPHAN_MIN_AGE_SECS  don't reap a build directory under
//	                  E2B_TEMPLATE_STORAGE_PATH modified more recently
//	                  than this (default 3600): it may be in use or still
//	                  being written
//	ORPHAN_QUARANTINE_SECS  how long a quarantined orphan waits
//	                  before it may be deleted (default 86400)
//	SPOOND_DB_PATH   path to the SQLite database (default /var/lib/spoond/spoond.db)
//	PAUSED_RELEASE_DAYS  days after its pause date that every paused
//	                  lease is released (default 30)
//	PINNED_IDLE_NOTICE_DAYS  days of no API activity after which a
//	                  pinned lease is flagged (default 7; visibility only)
//	IDLE_TIMEOUT_SECS, HELD_IDLE_TIMEOUT_SECS, HELD_SUSPENDED_RELEASE_SECS,
//	HOLD_TTL_SECS, HOLD_TTL_MAX_SECS, PRESSURE_HELD_IDLE_SECS: removed.
//	                  A set variable is logged as removed and ignored.
//	NOTIFY_WEBHOOKS  JSON list of webhook receivers for events that
//	                  need a person (2.2 #117): [{"url":..., "format":
//	                  "ntfy"|"slack"|"json", "min_severity":
//	                  "info"|"warn"|"critical", "events": ["glob*",
//	                  ...], "headers": {...}]. Unset disables.
//	NOTIFY_UNITS     systemd units the notifier watches, comma-separated
//	                  (default e2b-orchestrator.service,
//	                  spoond-sshd-gateway.service; "none" watches none)
//	NOTIFY_STATE_FILE  where dropped deliveries are mirrored for
//	                  `spoond doctor` (default: notify-state.json next
//	                  to the database)
//	BACKUP_MAX_AGE_SECS  how old the newest database backup may get
//	                  before the notifier warns (default 93600 = 26 h)
//	KEPT_DISK_WARN_PCT  kept-checkpoint share of the snapshot disk past
//	                  which the notifier's disk.kept check warns and the
//	                  dashboard's Notifications panel shows a message
//	                  (#126; default 40; 0 disables both)
//	MAX_ADMIT_WAIT_SECS  how long a create may wait for admission when
//	                  it sends "wait" (#129 part 1; default 600; 0
//	                  disables waiting)
//	SNAPSHOT_WRITE_CONCURRENCY  how many memory-snapshot writes
//	                  (substrate Pause/Checkpoint) may run at once,
//	                  process-wide (spoond-t1s; default 1; 0 = unlimited,
//	                  the pre-fix behaviour)
//	DRAIN_SNAPSHOT_CONCURRENCY  how many of those writes the admin
//	                  drain may run at once, so a planned orchestrator
//	                  restart can pause a batch of leases inside the
//	                  unit's drain window (default 2; 0 = unlimited)
//	UNDRAIN_CONCURRENCY  how many drained leases the admin undrain may
//	                  resume at once, so restoring a batch of large
//	                  memory snapshots does not stack the node's I/O and
//	                  memory (spoond-urm; default 2; 0 = unlimited)
//	UNDRAIN_RESUME_RETRIES  how many extra attempts a resume the admin
//	                  undrain failed with a retryable envd/start error
//	                  gets before the lease is marked lost (spoond-urm;
//	                  default 2; 0 disables retries)
//	RECOVERY_RETRY_ATTEMPTS  how many failed crash-recovery attempts a
//	                  lease gets before it is marked lost (spoond-dxq;
//	                  default 3)
//	RECOVERY_RETRY_WINDOW  how long a lease may stay in recovery since
//	                  its first failed attempt, whatever the failure kind
//	                  (spoond-dxq; default 30m; a Go duration or seconds)
//	MAX_RUNNING_JOBS_PER_LEASE  the per-lease background-job cap; a
//	                  start past it answers 429 (2.6, #135)
//	JOB_RETENTION_SECS  how long an exited background-job record is kept
//	                  before pruning (2.6, #135; default 604800 = 7 d)
//	JOB_MAX_RUNTIME  how long a background exec job may run before the
//	                  reconcile pass kills it and marks it exited with
//	                  reason timed_out (spoond-wb5; default 24h = 86400;
//	                  a Go duration or seconds; a job may ask for a
//	                  shorter max_runtime_secs, never a longer one; a
//	                  negative value disables the cap; the cap is held as
//	                  whole seconds, so a positive fractional duration
//	                  rounds up and a negative value under one second is
//	                  rejected, and a bare integer too large to represent
//	                  is clamped)
//	CRASH_TEST       "1" or "true" enables POST /api/leases/{id}/crash-test,
//	                  which crashes one lease and runs it through crash
//	                  recovery (owner or admin; default off, the route
//	                  then answers 404). For hosts that run crash suites.
package spoondbackend

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jrimmer/spoond/v2/api"
	"github.com/jrimmer/spoond/v2/identity"
	"github.com/jrimmer/spoond/v2/internal/tlsfiles"
	"github.com/jrimmer/spoond/v2/metrics"
	"github.com/jrimmer/spoond/v2/notify"
	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
	"github.com/jrimmer/spoond/v2/substrate/e2b"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDurationOr(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// envDurationOrZero is envDurationOr but accepts a zero or negative value
// too. DrainResumeMaxAge reads 0 as "use the default" and a negative value
// as "disable the bound", so the variable can express a negative one from
// the environment (spoond-52c NIT). Like envDuration, a plain integer is
// read as a number of seconds, so a bare "3600" is an hour rather than a
// silently ignored value that falls back to the default (spoond-52c NIT).
// A missing or malformed value returns def.
func envDurationOrZero(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	return def
}

func envFloatOr(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

// backupStale reports whether dir holds no <prefix>-*.db backup newer
// than 24 h (a missing or unreadable newest backup counts as stale).
func backupStale(dir, prefix string) bool {
	matches, _ := filepath.Glob(filepath.Join(dir, prefix+"-*.db"))
	if len(matches) == 0 {
		return true
	}
	sort.Strings(matches)
	info, err := os.Stat(matches[len(matches)-1])
	return err != nil || time.Since(info.ModTime()) > 24*time.Hour
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// removedEnvVars are the environment variables FS5 (2026-10-08) removed:
// every held-lease and idle rule they configured is gone, and pins plus
// the one paused-release clock replace them. A set variable is warned
// about once at startup and ignored, so an operator's old EnvironmentFile
// does not silently look effective.
var removedEnvVars = []string{
	"HELD_IDLE_TIMEOUT_SECS",
	"HELD_SUSPENDED_RELEASE_SECS",
	"HOLD_TTL_SECS",
	"HOLD_TTL_MAX_SECS",
	"PRESSURE_HELD_IDLE_SECS",
	"IDLE_TIMEOUT_SECS",
}

// warnRemovedEnv logs one line per removed variable that is still set.
// get reads the environment (os.Getenv in production, a fake in tests).
func warnRemovedEnv(logf interface{ Printf(string, ...any) }, get func(string) string) {
	for _, name := range removedEnvVars {
		if get(name) != "" {
			logf.Printf("%s is removed and ignored: pins replace holds, and one clock releases every paused lease PAUSED_RELEASE_DAYS after its pause date", name)
		}
	}
}

// notifyBackupMaxAge is the backup check's age limit in seconds
// (BACKUP_MAX_AGE_SECS): how old the newest database backup may get
// before the notifier warns, 93600 s = 26 h by default (the daily
// 03:00 run plus one missed day). The same knob the backup loop's own
// staleness check uses.
func notifyBackupMaxAge() time.Duration {
	return notify.BackupMaxAgeSecs(envIntOr("BACKUP_MAX_AGE_SECS", 0))
}

// keptDiskWarnPct is the kept-checkpoint disk share (#126) past which
// the notifier's disk.kept check warns: KEPT_DISK_WARN_PCT, default 40.
// A configured 0 disables the check (and the dashboard's kept-disk
// notification, which reads the same knob) — the check is only
// registered when the value is positive.
func keptDiskWarnPct() float64 {
	// Parsed as a float, as the dashboard parses the same knob, so a
	// fractional setting (40.5) means the same in both.
	if v := os.Getenv("KEPT_DISK_WARN_PCT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return f
		}
	}
	return notify.DefaultKeptDiskWarnPct
}

// parseGuestDNS validates SPOOND_GUEST_DNS_ADDR: a comma-separated list
// of bare IP addresses. An empty value means "no resolver allowance"
// (allowed); a non-empty value that yields no address (separators or
// whitespace only) is an error, so a typo fails startup rather than
// silently restoring the public DNS fallback.
func parseGuestDNS(v string) ([]string, error) {
	var addrs []string
	for _, addr := range strings.Split(v, ",") {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		if net.ParseIP(addr) == nil {
			return nil, fmt.Errorf("%q is not an IP address", addr)
		}
		addrs = append(addrs, addr)
	}
	if v != "" && len(addrs) == 0 {
		return nil, fmt.Errorf("%q names no resolver address", v)
	}
	return addrs, nil
}

// parseJobMaxRuntime parses JOB_MAX_RUNTIME: a Go duration or a whole
// number of seconds (like envDurationOrZero). An empty or malformed
// value is 0, the 24 h default; a negative value disables the cap. The
// cap is held as whole seconds, so a fractional Go duration is rounded
// up to the next whole second when it is stored (see jobMaxRuntimeSecs):
// 1500ms is 2 s, not a truncated 1 s, and 500ms is 1 s, not a 0 that
// reads as the default. A negative value smaller than one second is
// rejected: it would round to 0 and mean the default rather than "off"
// (spoond-wb5). A bare integer too large to convert is clamped to the
// largest representable magnitude rather than wrapping.
func parseJobMaxRuntime(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	var d time.Duration
	if n, err := strconv.Atoi(v); err == nil {
		d = secondsToDurationClamped(int64(n))
	} else if parsed, err := time.ParseDuration(v); err == nil {
		d = parsed
	} else {
		return 0, nil
	}
	if d < 0 && d > -time.Second {
		return 0, fmt.Errorf("%s is less than a second; a negative value disables the cap", v)
	}
	return d, nil
}

// jobMaxRuntimeSecs converts a parsed JOB_MAX_RUNTIME duration to the
// whole seconds the service config holds. A positive fractional duration
// rounds up to the next whole second, so a sub-second cap never truncates
// to 0 (= the 24 h default) and a cap between whole seconds never grants
// less than asked; zero stays 0 (the default) and a negative disables the
// cap.
func jobMaxRuntimeSecs(d time.Duration) int64 {
	if d <= 0 {
		return int64(d / time.Second)
	}
	secs := int64(d / time.Second)
	if d%time.Second != 0 {
		secs++
	}
	return secs
}

// secondsToDurationClamped converts a whole number of seconds to a
// Duration, clamping at the largest representable magnitude in both
// directions. time.Duration counts nanoseconds, so a bare integer above
// ~9.2e9 would otherwise overflow the multiplication and wrap (a large
// cap could silently become a small one, or a large negative "off" could
// wrap positive and enable a cap); the same clamp the service applies to
// JOB_MAX_RUNTIME seconds (spoond-wb5).
func secondsToDurationClamped(secs int64) time.Duration {
	const maxSecs = (1<<63 - 1) / int64(time.Second)
	if secs > maxSecs {
		return time.Duration(maxSecs) * time.Second
	}
	if secs < -maxSecs {
		return -time.Duration(maxSecs) * time.Second
	}
	return time.Duration(secs) * time.Second
}

// envBoolOr accepts the usual off-words ("0", "false", "no") as false and
// anything else as true, so a typo fails open to the default rather than
// silently disabling a check.
func envBoolOr(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "":
		return def
	case "0", "false", "no":
		return false
	default:
		return true
	}
}

// notifyUnits is the systemd units the notifier watches: NOTIFY_UNITS
// (comma-separated; "none" watches nothing, for hosts without systemd)
// or notify.DefaultUnits.
func notifyUnits() []string {
	v := strings.TrimSpace(os.Getenv("NOTIFY_UNITS"))
	switch v {
	case "":
		return notify.DefaultUnits
	case "none":
		return nil
	}
	return strings.Split(v, ",")
}

// newNotifier builds the webhook notifier: every outcome counted in
// spoond_notifications_total{webhook,severity,result} (the webhook
// label is the receiver's index — never the URL), dropped deliveries
// mirrored to NOTIFY_STATE_FILE for `spoond doctor`, and the hugepage
// check reading the orchestrator's NodeInfo.
func newNotifier(hooks []notify.Webhook, dbPath string, sub *e2b.Client, m *metrics.BackendMetrics) *notify.Notifier {
	statePath := os.Getenv("NOTIFY_STATE_FILE")
	if statePath == "" {
		statePath = filepath.Join(filepath.Dir(dbPath), "notify-state.json")
	}
	n := notify.New(notify.Config{
		Webhooks:  hooks,
		StatePath: statePath,
		Log:       log.Default(),
		Metrics:   metricsSink{m},
	})
	n.SetHugepages(notify.HugepagesFromNodeInfo(func(ctx context.Context) (uint64, uint64, uint64, uint64, error) {
		info, err := sub.NodeInfo(ctx)
		if err != nil {
			return 0, 0, 0, 0, err
		}
		return info.HugepagesTotal, info.HugepagesUsed, info.HugepagesReserved, info.HugepageSizeBytes, nil
	}))
	return n
}

// metricsSink adapts the backend's Prometheus collector to the
// notifier's Metrics interface: one increment of
// spoond_notifications_total{webhook,severity,result} per outcome.
type metricsSink struct {
	m *metrics.BackendMetrics
}

func (s metricsSink) Notification(webhook, severity, result string) {
	s.m.Notifications.WithLabelValues(webhook, severity, result).Inc()
}

func Main(args []string) int {
	bindAddr := envOr("BIND_ADDR", "127.0.0.1:8890")
	proxyAddr := envOr("PROXY_ADDR", "") // e.g. 0.0.0.0:8891 (Caddy wildcard front)
	// TLS_CERT and TLS_KEY may list several pairs (comma-separated, paired by
	// position), chosen by SNI; the first is the default (internal/tlsfiles).
	tlsPairs, err := tlsfiles.Parse(os.Getenv("TLS_CERT"), os.Getenv("TLS_KEY"))
	if err != nil {
		log.Fatalf("TLS_CERT/TLS_KEY: %v", err)
	}
	poolSize := envIntOr("POOL_SIZE", 0)
	defaultTTL := time.Duration(envIntOr("DEFAULT_TTL_SECS", 300)) * time.Second
	maxTTL := time.Duration(envIntOr("MAX_TTL_SECS", 3600)) * time.Second
	hostGuestAddr := os.Getenv("HOST_GUEST_SERVICE_ADDR")
	if hostGuestAddr == "" {
		log.Fatal("HOST_GUEST_SERVICE_ADDR is required (the address guests use to reach host services)")
	}
	// The guest's DNS resolver(s) (SPOOND_GUEST_DNS_ADDR,
	// comma-separated). Each is granted to every lease's egress policy
	// and baked into the guest image for spoond-guest-init. Empty = no
	// resolver allowance; a non-empty value that names no address
	// (spaces or commas only) is fatal, so a typo cannot silently
	// restore the public DNS fallback.
	guestDNSAddr := os.Getenv("SPOOND_GUEST_DNS_ADDR")
	if _, err := parseGuestDNS(guestDNSAddr); err != nil {
		log.Fatalf("SPOOND_GUEST_DNS_ADDR: %v", err)
	}
	// The wildcard hostname suffix the HTTP proxy routes
	// (SPOOND_PROXY_HOST_SUFFIX). Empty = the generic default.
	proxyHostSuffix := os.Getenv("SPOOND_PROXY_HOST_SUFFIX")
	hostGuestPort := envIntOr("HOST_GUEST_SERVICE_PORT", 8891)
	defaultAPIPort := 0
	if _, p, err := net.SplitHostPort(bindAddr); err == nil {
		defaultAPIPort, _ = strconv.Atoi(p)
	}
	hostAPIPort := envIntOr("HOST_API_PORT", defaultAPIPort)
	// Checkpointing (2.3, #122): CHECKPOINT_INTERVAL_MINS is the default
	// per-lease interval for leases without their own (-1), itself
	// defaulting to 0 = never. A lease's own checkpoint_interval (0 or
	// 60..604800 seconds) overrides it.
	checkpointDefault := time.Duration(envIntOr("CHECKPOINT_INTERVAL_MINS", 0)) * time.Minute
	// Idle reclamation (2.5, #129 part 2): IDLE_SUSPEND_DEFAULT_SECS is
	// the default idle_suspend for persistent leases without their own
	// (-1), itself defaulting to 0 = never; a lease's own idle_suspend
	// overrides it. Kept in seconds (not a duration) like the lease field.
	idleSuspendDefault := int64(envIntOr("IDLE_SUSPEND_DEFAULT_SECS", 0))
	storagePath := envOr("E2B_TEMPLATE_STORAGE_PATH", "/forkdcache/e2b/storage/templates")
	// Template build timeout (spoond-4yl): the GC fails a build still
	// `building` for longer than twice this. SPOOND_BUILD_TIMEOUT (a Go
	// duration or seconds) is read here from the backend's own
	// environment; `spoond images build` reads the same variable in its
	// environment, so both must be set (spoond-rzz).
	buildTimeout := substrate.BuildTimeoutFromEnv()
	// The burst lease reserve (#128 part 2): BURST_RESERVE_MIB keeps
	// this much hugepage memory free of burst leases, so guaranteed
	// work always has room to land; 0 disables the reserve.
	// TODO(FS2b-1 step 2): remove
	burstReserveMiB := envIntOr("BURST_RESERVE_MIB", api.DefaultBurstReserveMiB)
	// The preemption disk floor (#128 part 3): a preemption pause must
	// leave this percentage of the snapshot disk free, estimated from
	// the burst lease's memory_mb. PREEMPT_DISK_FLOOR_PCT, default 15;
	// fractional values (e.g. 12.5) are honoured.
	// TODO(FS2b-1 step 2): remove
	preemptDiskFloorPct := envFloatOr("PREEMPT_DISK_FLOOR_PCT", api.DefaultPreemptDiskFloorPct)
	// Queued admission (#129 part 1): MAX_ADMIT_WAIT_SECS caps how long
	// a create may wait for room. 0 disables waiting (the request's
	// "wait" field is accepted and ignored).
	maxAdmitWaitSecs := envIntOr("MAX_ADMIT_WAIT_SECS", api.DefaultMaxAdmitWaitSecs)
	// Snapshot write pacing (spoond-t1s): every substrate Pause and
	// Checkpoint goes through one process-wide limiter. 0 means
	// unlimited (the old behaviour); an unset variable uses the
	// documented default. DRAIN_SNAPSHOT_CONCURRENCY is the drain's own
	// width, so the admin drain can pause a batch inside TimeoutStopSec.
	snapshotWriteConcurrency := envIntOr("SNAPSHOT_WRITE_CONCURRENCY", api.DefaultSnapshotWriteConcurrency)
	drainSnapshotConcurrency := envIntOr("DRAIN_SNAPSHOT_CONCURRENCY", api.DefaultDrainSnapshotConcurrency)
	// Undrain resume pacing (spoond-urm): a bounded resume width and a
	// couple of retries for a transient envd start/sync failure, so a
	// busy node does not lose 4 GiB leases to "syncing took too long".
	// spoond-638d adds a wait for the orchestrator to be ready before the
	// first resume and a generous window for an indeterminate
	// (unavailable/connection-reset) error, which never loses a lease.
	undrainConcurrency := envIntOr("UNDRAIN_CONCURRENCY", api.DefaultUndrainConcurrency)
	undrainResumeRetries := envIntOr("UNDRAIN_RESUME_RETRIES", api.DefaultUndrainResumeRetries)
	undrainResumeWindow := envDurationOrZero("UNDRAIN_RESUME_WINDOW", api.DefaultUndrainResumeWindow)
	undrainReadyTimeout := envDurationOrZero("UNDRAIN_READY_TIMEOUT", api.DefaultUndrainReadyTimeout)
	// Drain self-heal (spoond-52c): a drain that outlives DRAIN_MAX_SECS
	// while the node is healthy undrains itself rather than refusing
	// every create forever. 0 means the default; a negative disables it.
	drainMaxSecs := envIntOr("DRAIN_MAX_SECS", api.DefaultDrainMaxSecs)
	// DRAIN_RESUME_MAX_AGE bounds how long the self-heal loop retries a
	// lease whose resume stays deferred before leaving it suspended for
	// the owner. 0 means the default; a negative disables the bound.
	drainResumeMaxAge := envDurationOrZero("DRAIN_RESUME_MAX_AGE", api.DefaultDrainResumeMaxAge)
	// Bounded crash-recovery retries (spoond-dxq): a transient failure
	// keeps the lease recovering instead of losing it, and a permanently
	// failing one gives up after a bounded budget.
	recoveryRetryAttempts := envIntOr("RECOVERY_RETRY_ATTEMPTS", api.DefaultRecoveryRetryAttempts)
	recoveryRetryWindow := envDurationOr("RECOVERY_RETRY_WINDOW", api.DefaultRecoveryRetryWindow)
	// Lost-lease snapshot grace (owner decision 2026-10-02): the GC keeps
	// a lost lease's resume/checkpoint builds for this long before they
	// become candidates.
	lostGracePersistent := envDurationOr("GC_LOST_GRACE_PERSISTENT", 7*24*time.Hour)
	lostGrace := envDurationOr("GC_LOST_GRACE", 24*time.Hour)
	// Pins and one clock (FS5, owner decision 2026-10-08): PAUSED_RELEASE_DAYS
	// is how many days after its pause date every paused lease is released
	// (default 30); PINNED_IDLE_NOTICE_DAYS is the pinned-idle notice age
	// (default 7, visibility only).
	pausedReleaseDays := envIntOr("PAUSED_RELEASE_DAYS", api.DefaultPausedReleaseDays)
	pinnedIdleNoticeDays := envIntOr("PINNED_IDLE_NOTICE_DAYS", api.DefaultPinnedIdleNoticeDays)
	// Removed hold/idle variables (FS5): warn once at startup and ignore.
	warnRemovedEnv(log.Default(), os.Getenv)

	// Parse consumer tokens: "abc=forgejo,def=pi"
	tokens := map[string]string{}
	for _, pair := range strings.Split(os.Getenv("CONSUMER_TOKENS"), ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) == 2 {
			tokens[parts[0]] = parts[1]
		}
	}
	if len(tokens) == 0 {
		log.Fatal("CONSUMER_TOKENS is required (token=consumer,comma-separated)")
	}

	// The E2B substrate: orchestrator over gRPC, envd through the proxy.
	cfg, err := e2b.FromEnv()
	if err != nil {
		log.Fatalf("substrate: %v", err)
	}
	sub, err := e2b.New(cfg)
	if err != nil {
		log.Fatalf("substrate: %v", err)
	}

	// Persistence (U05): open the SQLite store and load the previous
	// incarnation's leases, shares and pool before reconciling or
	// starting any loop, so the reconciler sees the loaded state.
	dbPath := envOr("SPOOND_DB_PATH", "/var/lib/spoond/spoond.db")
	db, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	// Background jobs (spoond-wb5): JOB_MAX_RUNTIME is a Go duration or a
	// whole number of seconds; 0 is the 24 h default and a negative value
	// disables the cap. The cap is held as whole seconds, so a positive
	// fractional duration rounds up (jobMaxRuntimeSecs) and a negative
	// value under one second is rejected here rather than rounding to 0,
	// which would silently mean the default rather than "off".
	jobMaxRuntime, err := parseJobMaxRuntime(os.Getenv("JOB_MAX_RUNTIME"))
	if err != nil {
		log.Fatalf("JOB_MAX_RUNTIME: %v", err)
	}

	svc := api.NewService(sub, db, tokens, api.ServiceConfig{
		PoolSize:                  poolSize,
		DefaultTTL:                defaultTTL,
		MaxTTL:                    maxTTL,
		HostGuestAddr:             hostGuestAddr,
		HostGuestPort:             hostGuestPort,
		GuestDNSAddr:              guestDNSAddr,
		ProxyHostSuffix:           proxyHostSuffix,
		HostAPIPort:               hostAPIPort,
		MetricsToken:              os.Getenv("METRICS_TOKEN"),
		EventsToken:               os.Getenv("EVENTS_TOKEN"),
		ProxyURL:                  cfg.ProxyURL,
		CheckpointIntervalDefault: int64(checkpointDefault / time.Second),
		IdleSuspendDefault:        idleSuspendDefault,
		TemplateStoragePath:       storagePath,
		BuildTimeout:              buildTimeout,
		LostGracePersistent:       lostGracePersistent,
		LostGrace:                 lostGrace,
		PausedReleaseDays:         pausedReleaseDays,
		PinnedIdleNoticeDays:      pinnedIdleNoticeDays,
		MaxKeptPerLease:           envIntOr("MAX_KEPT_PER_LEASE", api.DefaultMaxKeptPerLease),
		MaxNamedSnapshots:         envIntOr("MAX_NAMED_SNAPSHOTS", api.DefaultMaxNamedSnapshots),
		SnapshotKeepVersions:      envIntOr("SNAPSHOT_KEEP_VERSIONS", api.DefaultSnapshotKeepVersions),
		// TODO(FS2b-1 step 2): remove
		BurstReserveMiB: burstReserveMiB,
		// TODO(FS2b-1 step 2): remove
		PreemptDiskFloorPct: preemptDiskFloorPct,
		// Background exec jobs (2.6, #135): per-lease running cap and
		// exited-record retention. JOB_MAX_RUNTIME (spoond-wb5) caps how
		// long a job may run; it takes a Go duration or a number of
		// seconds, a job may ask for a shorter max_runtime_secs but never a
		// longer one, and 0 (unset) is the 24 h default while a negative
		// value disables the cap.
		MaxRunningJobsPerLease:   envIntOr("MAX_RUNNING_JOBS_PER_LEASE", api.DefaultMaxRunningJobsPerLease),
		JobRetentionSecs:         int64(envIntOr("JOB_RETENTION_SECS", api.DefaultJobRetentionSecs)),
		JobMaxRuntimeSecs:        jobMaxRuntimeSecs(jobMaxRuntime),
		MaxAdmitWaitSecs:         maxAdmitWaitSecs,
		SnapshotWriteConcurrency: snapshotWriteConcurrency,
		DrainSnapshotConcurrency: drainSnapshotConcurrency,
		UndrainConcurrency:       undrainConcurrency,
		UndrainResumeRetries:     undrainResumeRetries,
		UndrainResumeWindow:      undrainResumeWindow,
		UndrainReadyTimeout:      undrainReadyTimeout,
		RecoveryRetryAttempts:    recoveryRetryAttempts,
		RecoveryRetryWindow:      recoveryRetryWindow,
		// spoond-j3a: bound one background sweep stage so a hung
		// substrate RPC frees the loop and the lease's busy flag.
		SweepTimeout:      envDurationOr("SWEEP_TIMEOUT", api.DefaultSweepTimeout),
		DrainMaxSecs:      drainMaxSecs,
		DrainResumeMaxAge: drainResumeMaxAge,
		CrashTest:         os.Getenv("CRASH_TEST") == "1" || os.Getenv("CRASH_TEST") == "true",
	})
	// A fresh build's memory file lands after Checkpoint/Pause return:
	// re-measure it until its size settles (#125).
	svc.SetBuildSizeSettle(2*time.Second, 10*time.Minute)

	// Per-create integrity probe: a sandbox with a corrupt toolchain answers
	// a ping and then fails the job deep inside a build, so verify it from
	// inside the guest before pooling or leasing it. SANDBOX_PROBE=0 disables.
	svc.SetSandboxProbe(envBoolOr("SANDBOX_PROBE", true), time.Duration(envIntOr("SANDBOX_PROBE_TIMEOUT_SECS", 20))*time.Second)

	// Rootfs liveness probe (spoond-5ca): every ROOTFS_PROBE_SECS read one
	// block of each running lease's root device with O_DIRECT, and recover a
	// guest whose disk answers I/O errors like a crash. 0 disables it.
	svc.SetRootfsProbe(envIntOr("ROOTFS_PROBE_SECS", api.DefaultRootfsProbeSecs))

	// LLM gateway model map: "exe.dev-id=upstream-id,exe.dev-id2=upstream2".
	// Shelley sends exe.dev catalog ids; the gateway rewrites them to the
	// configured upstream's models. Unmapped ids fall back to defaultModel.
	llmModelMap := map[string]string{}
	if v := os.Getenv("LLM_MODEL_MAP"); v != "" {
		for _, pair := range strings.Split(v, ",") {
			parts := strings.SplitN(pair, "=", 2)
			if len(parts) == 2 {
				llmModelMap[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
			}
		}
	}
	// Identity store (epic #26 T1): users + key/token resolution. The
	// store file is optional; when absent the backend runs in legacy
	// single-user mode (consumer tokens only) until a user is created.
	// The first user created becomes the admin (KTD-2).
	if usersFile := os.Getenv("USERS_FILE"); usersFile != "" {
		ids, err := identity.NewStore(usersFile)
		if err != nil {
			log.Fatalf("identity store: %v", err)
		}
		svc.SetIdentities(ids)
		log.Printf("identity store: %s (%d user(s))", usersFile, ids.Count())
	}
	// Trusted gateway impersonation (U6/T5): the SSH gateway's service
	// token, used to act as the SSH-authenticated user on ctl calls.
	if gt := os.Getenv("GATEWAY_TOKEN"); gt != "" {
		svc.SetGatewayToken(gt)
		log.Printf("gateway token: trusted impersonation enabled")
	}

	reg := api.NewImageRegistry(db)
	srv := api.NewServerWithLLM(svc, reg, os.Getenv("LLM_UPSTREAM_URL"), os.Getenv("LLM_UPSTREAM_KEY"), os.Getenv("LLM_DEFAULT_MODEL"), llmModelMap)
	// Per-user LLM gateway concurrency cap (U8/T8): 0 = unlimited.
	if n := envIntOr("LLM_MAX_CONCURRENT_PER_USER", 0); n > 0 {
		srv.SetLLMMaxConcurrent(n)
	}
	// Security review #37 C2: when an identity store is present, deny
	// /llm/ for identity users WITHOUT an LLM key unless the operator
	// explicitly opts into the pre-U8 capability model.
	srv.SetLLMRequireKey(os.Getenv("LLM_OPEN_LEGACY") == "")
	// Public proxy auth (U7/T7): off (default) = capability model;
	// forward-auth = require X-Proxy-Auth secret + Remote-User identity
	// on every hostname-routed proxy request. The secret is shared with
	// the Caddy forward-auth block (never exposed to guests).
	srv.SetProxyAuth(os.Getenv("PROXY_AUTH_MODE"), os.Getenv("PROXY_AUTH_SECRET"), os.Getenv("PROXY_AUTH_TRUSTED_PEERS"))
	// First-user bootstrap gate (security review #37 H3/M4). Unset keeps
	// the legacy open-bootstrap for single-operator setups; set it on
	// multi-user deployments so a leaked consumer token can't claim
	// admin on a fresh store.
	srv.SetBootstrapToken(os.Getenv("BOOTSTRAP_TOKEN"))
	// Admin API (U10): /api/admin/drain, /undrain, /reconcile. The token
	// is not a consumer token; empty disables the admin routes.
	srv.SetAdminToken(os.Getenv("ADMIN_TOKEN"))

	// Static assets (shelley binary etc.) served to guests on the proxy
	// listener at /assets/<file> (default off; set ASSETS_DIR to enable).
	if d := os.Getenv("ASSETS_DIR"); d != "" {
		srv.SetAssetsDir(d)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := svc.LoadState(ctx); err != nil {
		log.Fatalf("store: load state: %v", err)
	}

	backupDir := envOr("SPOOND_BACKUP_DIR", notify.DefaultBackupDir)
	backupPrefix := strings.TrimSuffix(filepath.Base(dbPath), filepath.Ext(dbPath))
	// Webhook notifications (2.2, #117): events that need a person —
	// lease lost, held-lease rule actions, and the periodic checks —
	// delivered to the NOTIFY_WEBHOOKS receivers. Unset keeps the
	// notifier off entirely. Webhook URLs and headers may carry
	// secrets; only the notifier's redacted forms are ever logged.
	if hooks, err := notify.ParseWebhooks(os.Getenv("NOTIFY_WEBHOOKS")); err != nil {
		log.Fatalf("notify: %v", err)
	} else if len(hooks) > 0 {
		notifier := newNotifier(hooks, dbPath, sub, srv.Metrics())
		svc.SetNotifier(notifier)
		src := notify.ProductionSources(
			notifyUnits(),
			storagePath, backupDir, backupPrefix,
			notifyBackupMaxAge(),
			svc.GCLastError(),
		)
		// The admin drain state (spoond-52c H3): node.draining warns while
		// the drain is in effect and resolves when it clears.
		src.Draining = svc.Draining
		src.DrainWarnAfter = svc.DrainWarnAfter()
		for _, c := range src.Checks() {
			notifier.AddCheck(c)
		}
		// Kept checkpoints vs the snapshot disk (#126): disk.kept warns
		// past KEPT_DISK_WARN_PCT (default 40; 0 = off). The critical-disk
		// rule never deletes a kept build, so only a person can act on it.
		if warn := keptDiskWarnPct(); warn > 0 {
			keptSrc := &notify.CheckSources{KeptDisk: svc.KeptDiskProbe(storagePath), KeptWarnPct: warn}
			for _, c := range keptSrc.Checks() {
				notifier.AddCheck(c)
			}
		}
		notifier.Start(ctx)
		defer notifier.Stop()
		log.Printf("notify: %d webhook(s) configured", len(hooks))
	}

	// Delete any substrate sandboxes no loaded lease or pool entry
	// claims, then recover every lease that has a checkpoint from the
	// crash (U10 reconcileCrash) before warming the pool, so capacity is
	// never double-booked.
	svc.ReconcileOrphans(ctx)

	svc.Start(ctx)

	// Database backups (U11): daily at 03:00 local time, and once at
	// start when no backup is newer than 24 h. The file prefix is the
	// database file's basename without extension.
	go func() {
		if backupStale(backupDir, backupPrefix) {
			if err := db.Backup(context.Background(), backupDir, 7); err != nil {
				log.Printf("backup: %v", err)
			}
		}
		for {
			now := time.Now()
			next := time.Date(now.Year(), now.Month(), now.Day(), 3, 0, 0, 0, now.Location())
			if !next.After(now) {
				next = next.Add(24 * time.Hour)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(next.Sub(now)):
			}
			if err := db.Backup(context.Background(), backupDir, 7); err != nil {
				log.Printf("backup: %v", err)
			}
		}
	}()

	// Slow-loris / header hardening (security review #37 rescan F10):
	// both listeners get read-header timeouts + header size caps so a
	// LAN/guest client can't hold connections open forever with trickled
	// headers or send megabyte header floods.
	newHTTPServer := func(addr string, handler http.Handler) *http.Server {
		return &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			MaxHeaderBytes:    1 << 20,
		}
	}
	httpSrv := newHTTPServer(bindAddr, srv.Handler())

	// Optional second listener: the public HTTP proxy (wildcard
	// *.sandbox.example.com via Caddy). Plain HTTP — Caddy terminates TLS.
	var proxySrv *http.Server
	if proxyAddr != "" {
		proxySrv = newHTTPServer(proxyAddr, srv.ProxyHandler())
		go func() {
			log.Printf("proxy listening on %s (wildcard sandbox hostnames)", proxyAddr)
			if err := proxySrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("proxy: %v", err)
			}
		}()
	}

	// Graceful shutdown: stop the background loops and flush pending
	// LastActive updates. Leases, shares and pool entries persist in the
	// SQLite store and the next incarnation loads them (U05).
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.Printf("received %v, shutting down (%d leases kept in store)", sig, len(svc.LiveLeases()))
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()
		svc.Shutdown(shutdownCtx)
		db.Close()
		_ = httpSrv.Shutdown(shutdownCtx)
		if proxySrv != nil {
			_ = proxySrv.Shutdown(shutdownCtx)
		}
		cancel()
	}()

	log.Printf("spoond-backend listening on %s (substrate %s, %d consumer(s), pool=%d)", bindAddr, cfg.GRPCAddr, len(tokens), poolSize)
	log.Printf("template build timeout: %s (SPOOND_BUILD_TIMEOUT; the GC fails a `building` row older than %s)", buildTimeout, 2*buildTimeout)
	if tlsPairs != nil {
		certs, cerr := tlsfiles.New(tlsPairs, log.Printf)
		if cerr != nil {
			log.Fatalf("tls: %v", cerr)
		}
		log.Printf("tls: serving %s (first is the default; files re-read every minute)", strings.Join(certs.Names(), " | "))
		go certs.Watch(ctx, time.Minute)
		httpSrv.TLSConfig = certs.Config()
		err = httpSrv.ListenAndServeTLS("", "")
	} else {
		err = httpSrv.ListenAndServe()
	}
	if err != nil && err != http.ErrServerClosed {
		log.Fatalf("server: %v", err)
	}
	return 0
}
