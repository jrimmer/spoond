// Package settings lists every environment setting a spoond binary
// reads (#137), so a configuration file can be checked against it: a
// key no binary reads is a typo or a setting that no longer exists, and
// it would otherwise be ignored in silence. TestKnownMatchesSource keeps
// the list equal to what the source actually reads.
package settings

import (
	"bufio"
	"sort"
	"strings"
)

// Known is every setting name a spoond binary reads, sorted.
var Known = []string{
	"ADMIN_TOKEN",
	"ASSETS_DIR",
	"BACKUP_MAX_AGE_SECS",
	"BIND_ADDR",
	"BOOTSTRAP_TOKEN",
	"CHECKPOINT_INTERVAL_MINS",
	"COLUMNS",
	"CONFORMANCE_API",
	"CONFORMANCE_BACKEND_UNIT",
	"CONFORMANCE_CAPACITY",
	"CONFORMANCE_CAPACITY_IMAGE",
	"CONFORMANCE_CRASH_TEST",
	"CONFORMANCE_DESTRUCTIVE",
	"CONFORMANCE_GUEST_SERVICE",
	"CONFORMANCE_IDLE_IMAGE",
	"CONFORMANCE_IMAGES",
	"CONFORMANCE_LAN_TARGET",
	"CONFORMANCE_MIXED_BLOCKED_PRIVATE",
	"CONFORMANCE_MIXED_DOMAIN",
	"CONFORMANCE_MIXED_PRIVATE",
	"CONFORMANCE_MIXED_PRIVATE_DOMAIN",
	"CONFORMANCE_MIXED_PRIVATE_PORT",
	"CONFORMANCE_MIXED_SSH_PORT",
	"CONFORMANCE_PROXY_SECRET",
	"CONFORMANCE_PROXY_SUFFIX",
	"CONFORMANCE_PROXY_URL",
	"CONFORMANCE_SECOND_TOKEN",
	"CONFORMANCE_SSH",
	"CONFORMANCE_SSH_GATEWAY",
	"CONFORMANCE_SSH_KEY",
	"CONFORMANCE_SUBSTRATE",
	"CONFORMANCE_TOKEN",
	"CONFORMANCE_USER",
	"CONFORMANCE_USER_ID",
	"CONSUMER_TOKENS",
	"CRASH_TEST",
	"DASH_ADDR",
	"DASH_AUTH",
	"DASH_DISK_DEVICE",
	"DASH_EVENTS_TOKEN",
	"DASH_HISTORY",
	"DASH_HOST",
	"DASH_INTERVAL",
	"DASH_IO_FULL_BAD_PCT",
	"DASH_IO_FULL_WARN_PCT",
	"DASH_PASSWORD_HASH",
	"DASH_PROJECT_URL",
	"DASH_SERVICES",
	"DASH_TLS_CERT",
	"DASH_TLS_KEY",
	"DASH_USER",
	"DASH_WIDTH",
	"DEFAULT_IMAGE",
	"DEFAULT_TTL_SECS",
	"DISK_RESERVE_PCT",
	"DRAIN_MAX_SECS",
	"DRAIN_RESUME_MAX_AGE",
	"DRAIN_SNAPSHOT_CONCURRENCY",
	"E2B_CHECKPOINT_TIMEOUT",
	"E2B_CONTROL_TIMEOUT",
	"E2B_CREATE_TIMEOUT",
	"E2B_DELETE_TIMEOUT",
	"E2B_ENVD_VERSION",
	"E2B_FIRECRACKER_VERSION",
	"E2B_GRPC_ADDR",
	"E2B_NODEINFO_TIMEOUT",
	"E2B_PAUSE_TIMEOUT",
	"E2B_PROXY_URL",
	"E2B_TEAM_ID",
	"E2B_TEMPLATE_STORAGE_PATH",
	"E2B_TOKEN_SEED_FILE",
	"EVENTS_TOKEN",
	"EXEC_TIMEOUT_SECS",
	"FORGEJO_ADMIN_TOKEN",
	"FORGEJO_URL",
	"GATEWAY_ADDR",
	"GATEWAY_IMAGE_ALIASES",
	"GATEWAY_METRICS_LISTEN",
	"GATEWAY_SSH_IMAGES",
	"GATEWAY_TOKEN",
	"GC_DELETE",
	"GC_LOST_GRACE",
	"GC_LOST_GRACE_PERSISTENT",
	"HOST_API_PORT",
	"HOST_GUEST_SERVICE_ADDR",
	"HOST_GUEST_SERVICE_PORT",
	"IDLE_SUSPEND_DEFAULT_SECS",
	"IMAGE_MAP",
	"IMAGE_REGISTRY",
	"JOB_MAX_RUNTIME",
	"JOB_RECORD_DIR",
	"JOB_RETENTION_SECS",
	"KEPT_DISK_WARN_PCT",
	"LEASE_NETPOL",
	"LEASE_NET_ALLOW",
	"LEASE_TOKEN",
	"LEASE_TTL",
	"LEASE_URL",
	"LLM_DEFAULT_MODEL",
	"LLM_GATEWAY_URL",
	"LLM_MAX_CONCURRENT_PER_USER",
	"LLM_MODEL_MAP",
	"LLM_OPEN_LEGACY",
	"LLM_UPSTREAM_KEY",
	"LLM_UPSTREAM_URL",
	"MAX_ADMIT_WAIT_SECS",
	"MAX_EXEC_BODY_BYTES",
	"MAX_EXEC_TIMEOUT_SECS",
	"MAX_KEPT_PER_LEASE",
	"MAX_LEASE_MEMORY_MB",
	"MAX_LEASE_VCPU",
	"MAX_NAMED_SNAPSHOTS",
	"MAX_RUNNING_JOBS_PER_LEASE",
	"MAX_TTL_SECS",
	"METRICS_LISTEN",
	"METRICS_SERVER_NAME",
	"METRICS_TOKEN",
	"METRICS_URL",
	"NOTIFY_STATE_FILE",
	"NOTIFY_UNITS",
	"NOTIFY_WEBHOOKS",
	"ORPHAN_MIN_AGE_SECS",
	"ORPHAN_QUARANTINE_SECS",
	"ORPHAN_REAP",
	"OTEL_PROM_URL",
	"PAUSED_RELEASE_DAYS",
	"PINNED_IDLE_NOTICE_DAYS",
	"POOL_SIZE",
	"PROXY_ADDR",
	"PROXY_AUTH_MODE",
	"PROXY_AUTH_SECRET",
	"PROXY_AUTH_TRUSTED_PEERS",
	"RECOVERY_RETRY_ATTEMPTS",
	"RECOVERY_RETRY_WINDOW",
	"REPO_BASE_URL",
	"ROOTFS_PROBE_SECS",
	"RUNNER_ADMIT_WAIT_SECS",
	"RUNNER_FLOOR",
	"RUNNER_LABELS",
	"RUNNER_MAX",
	"RUNNER_NAME",
	"RUNNER_SCALE_STEP",
	"RUNNER_STATE_FILE",
	"RUNNER_TOKEN",
	"SANDBOX_PROBE",
	"SANDBOX_PROBE_TIMEOUT_SECS",
	"SATURATION_CPU_SOME_PCT",
	"SATURATION_IO_FULL_PCT",
	"SERVICE_RESULT",
	"SHELLY_BINARY_URL",
	"SIZE_PRESETS",
	"SNAPSHOT_KEEP_VERSIONS",
	"SNAPSHOT_WRITE_CONCURRENCY",
	"SPOOND_ADMIN_TOKEN_FILE",
	"SPOOND_BACKUP_DIR",
	"SPOOND_BUILD_TIMEOUT",
	"SPOOND_DB_PATH",
	"SPOOND_DRAIN_INSECURE",
	"SPOOND_DRAIN_URL",
	"SPOOND_ENV_DIR",
	"SPOOND_GATEWAY_TOKEN",
	"SPOOND_GUEST_DNS_ADDR",
	"SPOOND_PROXY_HOST_SUFFIX",
	"SPOOND_SSH_CONNECTION_ADDR",
	"SWEEP_TIMEOUT",
	"TLS_CERT",
	"TLS_KEY",
	"UNDRAIN_CONCURRENCY",
	"UNDRAIN_READY_TIMEOUT",
	"UNDRAIN_RESUME_RETRIES",
	"UNDRAIN_RESUME_WINDOW",
	"USERS_FILE",
}

var known = func() map[string]bool {
	m := make(map[string]bool, len(Known))
	for _, k := range Known {
		m[k] = true
	}
	return m
}()

// IsKnown reports whether some spoond binary reads key.
func IsKnown(key string) bool { return known[key] }

// HideIfSecret is the masking rule for showing a setting's value
// (#137): a name carrying TOKEN, KEY, SECRET, PASSWORD, HASH or SEED
// (case-insensitive) is a secret and its value is never shown —
// "(hidden)" stands in for it. The admin config endpoint (api) masks
// the values it serves with this, and `spoond config show`
// (cmd/spoond-config) masks the same way, so a secret value exists
// nowhere in any config display that uses it.
func HideIfSecret(name, value string) string {
	upper := strings.ToUpper(name)
	for _, marker := range []string{"TOKEN", "KEY", "SECRET", "PASSWORD", "HASH", "SEED"} {
		if strings.Contains(upper, marker) {
			return "(hidden)"
		}
	}
	return value
}

// CheckEnvFile reads a systemd EnvironmentFile and returns, sorted, the
// keys no spoond binary reads and the keys set more than once (where
// the last one silently wins). Values are never returned.
func CheckEnvFile(content string) (unknown, duplicate []string) {
	seen := map[string]int{}
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		seen[strings.TrimSpace(key)]++
	}
	for k, n := range seen {
		if !known[k] {
			unknown = append(unknown, k)
		}
		if n > 1 {
			duplicate = append(duplicate, k)
		}
	}
	sort.Strings(unknown)
	sort.Strings(duplicate)
	return unknown, duplicate
}
