// Package settings lists every environment setting a spoond binary
// reads (#137), so a configuration file can be checked against it: a
// key no binary reads is a typo or a setting that no longer exists, and
// it would otherwise be ignored in silence. TestKnownMatchesSource keeps
// the list equal to what the source actually reads.
//
// Each setting also carries a type and the binary that reads it, and
// Validate checks a set value against its type, so a bad value fails
// startup with one message naming every problem (2026-10-11
// validated-config design, item 1) instead of being silently ignored —
// nearly every reader in spoond falls back to a default on a
// malformed value.
package settings

import (
	"bufio"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
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
	"CONFORMANCE_LAN_DNS",
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
	"DASH_RECORDS_EXCLUDE",
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
	"E2B_ROOT_FS_PATH",
	"E2B_TEAM_ID",
	"E2B_TEMPLATE_STORAGE_PATH",
	"E2B_TOKEN_SEED_FILE",
	"EVENTS_TOKEN",
	"EXEC_CONCURRENCY_MAX",
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
	"GIVE_BACK",
	"GIVE_BACK_MARGIN_MIB",
	"GUEST_DIAL_MAX",
	"HOST_API_PORT",
	"HOST_GUEST_SERVICE_ADDR",
	"HOST_GUEST_SERVICE_PORT",
	"IDLE_SUSPEND_DEFAULT_SECS",
	"IMAGE_BUILD_CACHE_KEEP",
	"IMAGE_MAP",
	"IMAGE_REGISTRY",
	"JOB_MAX_RUNTIME",
	"JOB_OUTPUT_MAX_BYTES",
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
	"MEMORY_FLOOR_MIB",
	"MEMORY_OVERCOMMIT",
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
	"SELF_TOKEN_API_URL",
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
	"SPOOND_TOKEN",
	"SPOOND_URL",
	"SWEEP_TIMEOUT",
	"TLS_CERT",
	"TLS_KEY",
	"UNDRAIN_CONCURRENCY",
	"UNDRAIN_READY_TIMEOUT",
	"UNDRAIN_RESUME_RETRIES",
	"UNDRAIN_RESUME_WINDOW",
	"USERS_FILE",
}

// Type is the kind of value a setting carries, derived from how the
// code parses it.
type Type int

const (
	// TypeInt is a whole number: the code parses it with strconv.Atoi
	// (or ParseInt), so "-3" parses even where the reader then ignores
	// the sign — and "2h" would silently fall back to the default.
	TypeInt Type = iota
	// TypeBool is a switch. The readers accept the usual spellings: envBoolOr
	// (SANDBOX_PROBE) counts "0" "false" "no" as off, anything else on; the
	// == "1" readers (GC_DELETE, SPOOND_DRAIN_INSECURE, the conformance
	// flags) count only "1" on.
	TypeBool
	// TypeDuration is a time: the readers take a Go duration
	// (time.ParseDuration, "5m", "1h30m") or, for every duration
	// setting except DASH_INTERVAL, a bare whole number of seconds
	// (strconv.Atoi first, like envDurationOrZero); DASH_INTERVAL is
	// read only as a Go duration.
	TypeDuration
	// TypePercent is a float share of something, in percent: parsed
	// with strconv.ParseFloat by its readers, so 62.5 is accepted;
	// DISK_RESERVE_PCT is clamped to 0..50 by the code, the rest is
	// bounded 0..100 where the code bounds it at all, and Validate
	// draws the 0..100 line (a share cannot be negative or over 100).
	TypePercent
	// TypeURL is a base URL: parsed with url.Parse and used as
	// http.NewRequest's target or a Connect client's base, which
	// refuse a URL with no scheme.
	TypeURL
	// TypeAddr is a host:port listener or dial target: parsed with
	// net.SplitHostPort, which requires a port.
	TypeAddr
	// TypeList is comma-separated values (pairs, paths, names);
	// the readers split on "," and the shape of the entries is the
	// value's own business. The two lists whose entries are already
	// validated with a startup error — SIZE_PRESETS (a name=vcpu:mem_mb
	// spec) and SPOOND_GUEST_DNS_ADDR (bare IPs) — are TypeString here:
	// their real check is stricter than "splits on commas", and it
	// already refuses to start.
	TypeList
	// TypeString is anything else: a path, a name, a token, a unit
	// name, an enum the reader switches on — read verbatim, so
	// nothing to check beyond set/not set.
	TypeString
)

func (t Type) String() string {
	switch t {
	case TypeInt:
		return "int"
	case TypeBool:
		return "bool"
	case TypeDuration:
		return "duration"
	case TypePercent:
		return "percent"
	case TypeURL:
		return "url"
	case TypeAddr:
		return "addr"
	case TypeList:
		return "list"
	case TypeString:
		return "string"
	}
	return "unknown"
}

// Binary names one spoond component that reads settings. A spec's
// Binary is the process whose environment the setting belongs to; the
// backend validates its own (its environment file is /etc/spoond/backend.env).
type Binary int

const (
	// Backend is spoond-backend (the lease API, "spoond backend").
	Backend Binary = iota
	// Dash is the dashboard, spoond dash (and `spoond top`, which reads
	// its COLUMNS).
	Dash
	// Runner is spoond-runner, the Forgejo Actions worker.
	Runner
	// Gateway is spoond-sshd-gateway, the SSH and ctl plane.
	Gateway
	// Images is `spoond images`, the image build pipeline.
	Images
	// Doctor is `spoond doctor`, the read-only triage command.
	Doctor
	// Conformance is the `conformance` test suite (its _test.go files).
	Conformance
	// Drain is the systemd drain/undrain hook, `spoond drain`.
	Drain
	// Other is a binary spoond ships but with only string settings
	// (ctl, mcp, config show): nothing to type-check yet. No spec
	// holds this value; it keeps the door named for them.
	Other
)

func (b Binary) String() string {
	switch b {
	case Backend:
		return "backend"
	case Dash:
		return "dash"
	case Runner:
		return "runner"
	case Gateway:
		return "gateway"
	case Images:
		return "images"
	case Doctor:
		return "doctor"
	case Conformance:
		return "conformance"
	case Drain:
		return "drain"
	case Other:
		return "other"
	}
	return "unknown"
}

// Spec is one setting's shape: its name, the kind of value the reading
// code parses it as, the binary whose environment it is read from, and
// whether the value is a secret (C2, 2026-10-11 validated-config design:
// a secret may be given as <NAME>_FILE instead).
type Spec struct {
	Name   string
	Type   Type
	Binary Binary
	// Secret marks a setting whose whole value is a credential: a leak
	// grants access. The backend reads it through Get, so the env file
	// may carry <NAME>_FILE=/path (0600) instead of NAME=value, and the
	// display helpers report set-ness the same either way.
	Secret bool
}

// Specs is every known setting with its type and owner binary, in the
// same order as Known (sorted by name). Secret is true exactly when
// HideIfSecret's name rule would mask the value (every <name> whose
// upper case carries TOKEN, KEY, SECRET, PASSWORD or HASH) — the C2
// design's secret list, derived from how the readers use the value.
var Specs = []Spec{
	{"ADMIN_TOKEN", TypeString, Backend, true},
	{"ASSETS_DIR", TypeString, Backend, false}, // bare path: the proxy listener serves /assets/<file> from it
	{"BACKUP_MAX_AGE_SECS", TypeInt, Backend, false},
	{"BIND_ADDR", TypeAddr, Backend, false},
	{"BOOTSTRAP_TOKEN", TypeString, Backend, true},
	{"CHECKPOINT_INTERVAL_MINS", TypeInt, Backend, false},
	{"COLUMNS", TypeInt, Dash, false},
	{"CONFORMANCE_API", TypeURL, Conformance, false},
	{"CONFORMANCE_BACKEND_UNIT", TypeString, Conformance, false}, // systemctl unit name
	{"CONFORMANCE_CAPACITY", TypeBool, Conformance, false},
	{"CONFORMANCE_CAPACITY_IMAGE", TypeString, Conformance, false},
	{"CONFORMANCE_CRASH_TEST", TypeBool, Conformance, false},
	{"CONFORMANCE_DESTRUCTIVE", TypeBool, Conformance, false},
	{"CONFORMANCE_GUEST_SERVICE", TypeAddr, Conformance, false}, // host:port the suite probes from a lease
	{"CONFORMANCE_IDLE_IMAGE", TypeString, Conformance, false},
	{"CONFORMANCE_IMAGES", TypeList, Conformance, false},
	{"CONFORMANCE_LAN_DNS", TypeString, Conformance, false},               // a LAN IP the guest resolver answers with (no default)
	{"CONFORMANCE_LAN_TARGET", TypeAddr, Conformance, false},              // host:port (parseLANTarget)
	{"CONFORMANCE_MIXED_BLOCKED_PRIVATE", TypeString, Conformance, false}, // bare host or IP, probed with its own port setting
	{"CONFORMANCE_MIXED_DOMAIN", TypeString, Conformance, false},
	{"CONFORMANCE_MIXED_PRIVATE", TypeString, Conformance, false}, // bare host or IP, probed with its own port setting
	{"CONFORMANCE_MIXED_PRIVATE_DOMAIN", TypeString, Conformance, false},
	{"CONFORMANCE_MIXED_PRIVATE_PORT", TypeInt, Conformance, false},
	{"CONFORMANCE_MIXED_SSH_PORT", TypeInt, Conformance, false},
	{"CONFORMANCE_PROXY_SECRET", TypeString, Conformance, true},
	{"CONFORMANCE_PROXY_SUFFIX", TypeString, Conformance, false},
	{"CONFORMANCE_PROXY_URL", TypeURL, Conformance, false},
	{"CONFORMANCE_SECOND_TOKEN", TypeString, Conformance, true},
	{"CONFORMANCE_SSH", TypeString, Conformance, false},       // an ssh destination, or "local"
	{"CONFORMANCE_SSH_GATEWAY", TypeAddr, Conformance, false}, // ssh.Dial target host:port
	{"CONFORMANCE_SSH_KEY", TypeString, Conformance, false},   // private key file path
	{"CONFORMANCE_SUBSTRATE", TypeString, Conformance, false},
	{"CONFORMANCE_TOKEN", TypeString, Conformance, true},
	{"CONFORMANCE_USER", TypeString, Conformance, false},
	{"CONFORMANCE_USER_ID", TypeString, Conformance, false},
	{"CONSUMER_TOKENS", TypeList, Backend, true}, // token=consumer pairs
	{"CRASH_TEST", TypeBool, Backend, false},
	{"DASH_ADDR", TypeAddr, Dash, false},
	{"DASH_AUTH", TypeBool, Dash, false},
	{"DASH_DISK_DEVICE", TypeString, Dash, false},
	{"DASH_EVENTS_TOKEN", TypeString, Dash, true},
	{"DASH_HISTORY", TypeInt, Dash, false},
	{"DASH_HOST", TypeString, Dash, false},
	{"DASH_INTERVAL", TypeDuration, Dash, false}, // the only duration read as a Go duration only (no bare seconds)
	{"DASH_IO_FULL_BAD_PCT", TypePercent, Dash, false},
	{"DASH_IO_FULL_WARN_PCT", TypePercent, Dash, false},
	{"DASH_PASSWORD_HASH", TypeString, Dash, true},
	{"DASH_PROJECT_URL", TypeString, Dash, false}, // a schemeless value is accepted: the code prepends https://
	{"DASH_RECORDS_EXCLUDE", TypeList, Dash, false},
	{"DASH_SERVICES", TypeList, Dash, false},
	{"DASH_TLS_CERT", TypeList, Dash, false}, // certificate paths, paired with DASH_TLS_KEY
	{"DASH_TLS_KEY", TypeList, Dash, false},  // key paths, paired with DASH_TLS_CERT
	{"DASH_USER", TypeString, Dash, false},
	{"DASH_WIDTH", TypeInt, Dash, false},
	{"DEFAULT_IMAGE", TypeString, Runner, false},
	{"DEFAULT_TTL_SECS", TypeInt, Backend, false},
	{"DISK_RESERVE_PCT", TypePercent, Backend, false}, // the code clamps 0..50; still a share of the disk
	{"DRAIN_MAX_SECS", TypeInt, Backend, false},
	{"DRAIN_RESUME_MAX_AGE", TypeDuration, Backend, false},
	{"DRAIN_SNAPSHOT_CONCURRENCY", TypeInt, Backend, false},
	{"E2B_CHECKPOINT_TIMEOUT", TypeDuration, Backend, false},
	{"E2B_CONTROL_TIMEOUT", TypeDuration, Backend, false},
	{"E2B_CREATE_TIMEOUT", TypeDuration, Backend, false},
	{"E2B_DELETE_TIMEOUT", TypeDuration, Backend, false},
	{"E2B_ENVD_VERSION", TypeString, Backend, false},
	{"E2B_FIRECRACKER_VERSION", TypeString, Backend, false},
	{"E2B_GRPC_ADDR", TypeAddr, Backend, false},
	{"E2B_NODEINFO_TIMEOUT", TypeDuration, Backend, false},
	{"E2B_PAUSE_TIMEOUT", TypeDuration, Backend, false},
	{"E2B_PROXY_URL", TypeURL, Backend, false},
	{"E2B_ROOT_FS_PATH", TypeString, Backend, false}, // the readyz root-disk check's path; tests only
	{"E2B_TEAM_ID", TypeString, Backend, false},      // a UUID, read verbatim
	{"E2B_TEMPLATE_STORAGE_PATH", TypeString, Backend, false},
	{"E2B_TOKEN_SEED_FILE", TypeString, Backend, false}, // seed file path: a path, not the secret itself
	{"EVENTS_TOKEN", TypeString, Backend, true},
	// Per-owner concurrent exec/stream cap; a positive value
	// is the host default every identity without an override counts
	// against.
	{"EXEC_CONCURRENCY_MAX", TypeInt, Backend, false},
	{"EXEC_TIMEOUT_SECS", TypeInt, Runner, false},
	{"FORGEJO_ADMIN_TOKEN", TypeString, Runner, true},
	{"FORGEJO_URL", TypeURL, Runner, false},
	{"GATEWAY_ADDR", TypeAddr, Doctor, false},
	{"GATEWAY_IMAGE_ALIASES", TypeList, Gateway, false}, // short=full pairs
	{"GATEWAY_METRICS_LISTEN", TypeAddr, Gateway, false},
	{"GATEWAY_SSH_IMAGES", TypeList, Gateway, false},
	{"GATEWAY_TOKEN", TypeString, Backend, true},
	{"GC_DELETE", TypeBool, Backend, false},
	{"GC_LOST_GRACE", TypeDuration, Backend, false},
	{"GC_LOST_GRACE_PERSISTENT", TypeDuration, Backend, false},
	// on or off (refused at startup otherwise; TypeString: on/off is not a bool spelling here).
	{"GIVE_BACK", TypeString, Backend, false},
	// A whole number of MiB, 0 = no margin.
	{"GIVE_BACK_MARGIN_MIB", TypeInt, Backend, false},
	// Per-owner concurrent guest-port-dial cap; a positive
	// value is the host default every identity without an override
	// counts against.
	{"GUEST_DIAL_MAX", TypeInt, Backend, false},
	{"HOST_API_PORT", TypeInt, Backend, false},
	{"HOST_GUEST_SERVICE_ADDR", TypeString, Backend, false}, // a bare host or IP: the code builds <addr>/32 CIDRs from it
	{"HOST_GUEST_SERVICE_PORT", TypeInt, Backend, false},
	{"IDLE_SUSPEND_DEFAULT_SECS", TypeInt, Backend, false},
	{"IMAGE_BUILD_CACHE_KEEP", TypeString, Images, false}, // a docker size (10GB, 1024MiB) passed plainly to --keep-storage; read verbatim
	{"IMAGE_MAP", TypeList, Runner, false},                // label=image pairs
	{"IMAGE_REGISTRY", TypeString, Images, false},
	{"JOB_MAX_RUNTIME", TypeDuration, Backend, false},
	{"JOB_OUTPUT_MAX_BYTES", TypeInt, Backend, false},
	{"JOB_RECORD_DIR", TypeString, Runner, false},
	{"JOB_RETENTION_SECS", TypeInt, Backend, false},
	{"KEPT_DISK_WARN_PCT", TypePercent, Backend, false},
	{"LEASE_NETPOL", TypeString, Runner, false}, // none|lan|internet|restricted
	{"LEASE_NET_ALLOW", TypeList, Runner, false},
	{"LEASE_TOKEN", TypeString, Runner, true},
	{"LEASE_TTL", TypeInt, Runner, false},
	{"LEASE_URL", TypeURL, Runner, false},
	{"LLM_DEFAULT_MODEL", TypeString, Backend, false},
	{"LLM_GATEWAY_URL", TypeURL, Gateway, false},
	{"LLM_MAX_CONCURRENT_PER_USER", TypeInt, Backend, false},
	{"LLM_MODEL_MAP", TypeList, Backend, false}, // pattern=model pairs
	{"LLM_UPSTREAM_KEY", TypeString, Backend, true},
	{"LLM_UPSTREAM_URL", TypeURL, Backend, false},
	{"MAX_ADMIT_WAIT_SECS", TypeInt, Backend, false},
	{"MAX_EXEC_BODY_BYTES", TypeInt, Backend, false},
	{"MAX_EXEC_TIMEOUT_SECS", TypeInt, Backend, false},
	{"MAX_KEPT_PER_LEASE", TypeInt, Backend, false},
	{"MAX_LEASE_MEMORY_MB", TypeInt, Backend, false},
	{"MAX_LEASE_VCPU", TypeInt, Backend, false},
	{"MAX_NAMED_SNAPSHOTS", TypeInt, Backend, false},
	{"MAX_RUNNING_JOBS_PER_LEASE", TypeInt, Backend, false},
	{"MAX_TTL_SECS", TypeInt, Backend, false},
	// A whole number of MiB, 0 = the floor is off (refused at startup
	// when not a whole number or negative).
	{"MEMORY_FLOOR_MIB", TypeInt, Backend, false},
	// A float >= 1.0, refused at startup otherwise (TypeString: no float type).
	{"MEMORY_OVERCOMMIT", TypeString, Backend, false},
	{"METRICS_LISTEN", TypeAddr, Runner, false},
	{"METRICS_SERVER_NAME", TypeString, Dash, false},
	{"METRICS_TOKEN", TypeString, Backend, true},
	{"METRICS_URL", TypeURL, Dash, false},
	{"NOTIFY_STATE_FILE", TypeString, Backend, false},
	{"NOTIFY_UNITS", TypeList, Backend, false},      // "none" watches nothing
	{"NOTIFY_WEBHOOKS", TypeString, Backend, false}, // a JSON list, validated by notify.ParseWebhooks; a path is never its credential, the receiver URL is
	{"ORPHAN_MIN_AGE_SECS", TypeInt, Backend, false},
	{"ORPHAN_QUARANTINE_SECS", TypeInt, Backend, false},
	{"ORPHAN_REAP", TypeString, Backend, false}, // off|dryrun|quarantine
	{"OTEL_PROM_URL", TypeURL, Backend, false},
	{"PAUSED_RELEASE_DAYS", TypeInt, Backend, false},
	{"PINNED_IDLE_NOTICE_DAYS", TypeInt, Backend, false},
	{"POOL_SIZE", TypeInt, Backend, false},
	{"PROXY_ADDR", TypeAddr, Backend, false},
	{"PROXY_AUTH_MODE", TypeString, Backend, false}, // off|forward-auth
	{"PROXY_AUTH_SECRET", TypeString, Backend, true},
	{"PROXY_AUTH_TRUSTED_PEERS", TypeList, Backend, false}, // IPs and CIDRs
	{"RECOVERY_RETRY_ATTEMPTS", TypeInt, Backend, false},
	{"RECOVERY_RETRY_WINDOW", TypeDuration, Backend, false},
	{"REPO_BASE_URL", TypeURL, Runner, false},
	{"ROOTFS_PROBE_SECS", TypeInt, Backend, false},
	{"RUNNER_ADMIT_WAIT_SECS", TypeInt, Runner, false},
	{"RUNNER_FLOOR", TypeInt, Runner, false},
	{"RUNNER_LABELS", TypeList, Runner, false},
	{"RUNNER_MAX", TypeInt, Runner, false},
	{"RUNNER_NAME", TypeString, Runner, false},
	{"RUNNER_SCALE_STEP", TypeInt, Runner, false},
	{"RUNNER_STATE_FILE", TypeString, Runner, false},
	{"RUNNER_TOKEN", TypeString, Runner, true},
	{"SANDBOX_PROBE", TypeBool, Backend, false},
	{"SANDBOX_PROBE_TIMEOUT_SECS", TypeInt, Backend, false},
	{"SATURATION_CPU_SOME_PCT", TypePercent, Backend, false},
	{"SATURATION_IO_FULL_PCT", TypePercent, Backend, false},
	{"SELF_TOKEN_API_URL", TypeURL, Backend, true}, // its name carries TOKEN, so config show hides it
	{"SERVICE_RESULT", TypeString, Drain, false},   // systemd's own variable, read verbatim
	{"SHELLY_BINARY_URL", TypeURL, Gateway, false},
	{"SIZE_PRESETS", TypeString, Backend, false}, // name=vcpu:memory_mb spec; sizes.Parse fails startup on a malformed value
	{"SNAPSHOT_KEEP_VERSIONS", TypeInt, Backend, false},
	{"SNAPSHOT_WRITE_CONCURRENCY", TypeInt, Backend, false},
	{"SPOOND_ADMIN_TOKEN_FILE", TypeList, Drain, false}, // token file paths, paired with SPOOND_DRAIN_URL
	{"SPOOND_BACKUP_DIR", TypeString, Backend, false},
	{"SPOOND_BUILD_TIMEOUT", TypeDuration, Backend, false},
	{"SPOOND_DB_PATH", TypeString, Backend, false},
	{"SPOOND_DRAIN_INSECURE", TypeBool, Drain, false},
	{"SPOOND_DRAIN_URL", TypeList, Drain, false}, // backend URLs, paired with SPOOND_ADMIN_TOKEN_FILE
	{"SPOOND_ENV_DIR", TypeString, Doctor, false},
	{"SPOOND_GATEWAY_TOKEN", TypeString, Gateway, true},
	{"SPOOND_GUEST_DNS_ADDR", TypeList, Backend, false}, // bare IPs; parseGuestDNS fails startup on a non-IP
	{"SPOOND_PROXY_HOST_SUFFIX", TypeString, Backend, false},
	{"SPOOND_SSH_CONNECTION_ADDR", TypeString, Gateway, false}, // the bare host reported in SSH_CONNECTION
	{"SPOOND_TOKEN", TypeString, Doctor, true},                 // the bearer `spoond capabilities` sends; secret-valued
	{"SPOOND_URL", TypeURL, Doctor, false},                     // the backend base URL `spoond capabilities` reads
	{"SWEEP_TIMEOUT", TypeDuration, Backend, false},
	{"TLS_CERT", TypeList, Backend, false}, // certificate paths, paired with TLS_KEY
	{"TLS_KEY", TypeList, Backend, false},  // key paths, paired with TLS_CERT
	{"UNDRAIN_CONCURRENCY", TypeInt, Backend, false},
	{"UNDRAIN_READY_TIMEOUT", TypeDuration, Backend, false},
	{"UNDRAIN_RESUME_RETRIES", TypeInt, Backend, false},
	{"UNDRAIN_RESUME_WINDOW", TypeDuration, Backend, false},
	{"USERS_FILE", TypeString, Backend, false}, // identity store path
}

var specByName = func() map[string]Spec {
	m := make(map[string]Spec, len(Specs))
	for _, s := range Specs {
		m[s.Name] = s
	}
	return m
}()

// SpecOf returns the spec for a known setting name.
func SpecOf(name string) (Spec, bool) {
	s, ok := specByName[name]
	return s, ok
}

// SpecsFor returns every spec owned by binary, in Known order.
func SpecsFor(binary string) []Spec {
	b, err := parseBinary(binary)
	if err != nil {
		return nil
	}
	var out []Spec
	for _, s := range Specs {
		if s.Binary == b {
			out = append(out, s)
		}
	}
	return out
}

func parseBinary(s string) (Binary, error) {
	for _, b := range []Binary{Backend, Dash, Runner, Gateway, Images, Doctor, Conformance, Drain, Other} {
		if b.String() == s {
			return b, nil
		}
	}
	return 0, fmt.Errorf("unknown binary %q", s)
}

// Validate checks every setting owned by binary whose value get
// returns is set against its type — the check the backend runs first
// thing in main, before doing anything else, so a typo'd value fails
// startup with one message naming every problem instead of a silent
// fallback to the default (2026-10-11 validated-config design, item 1).
// Unset settings keep their defaults. Unknown keys are not a startup
// failure: the process environment also holds systemd's own variables.
//
// get reads the environment — os.Getenv in production, anything else
// in tests. Messages name the setting and the offending value, with
// secret-named values masked to "(hidden)" (HideIfSecret), so a wrong
// token never lands in a log or a journal.
func Validate(binary string, get func(string) string) []error {
	var errs []error
	for _, s := range SpecsFor(binary) {
		v := get(s.Name)
		if v == "" {
			continue
		}
		if err := validateOne(s, v); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// validateOne checks one set value against its spec's type. The
// message names the setting and the value it got — masked when the
// name is a secret's.
func validateOne(s Spec, v string) error {
	shown := HideIfSecret(s.Name, v)
	name := s.Name
	bad := func(why string) error {
		return fmt.Errorf("config: %s=%q: %s", name, shown, why)
	}
	switch s.Type {
	case TypeInt:
		if _, err := strconv.Atoi(v); err != nil {
			return bad("not an integer")
		}
	case TypeBool:
		// The readers' spellings, unioned: envBoolOr counts 0/false/no
		// off and 1/true/yes/on on; the == "1" readers (GC_DELETE,
		// SPOOND_DRAIN_INSECURE, the conformance flags) count only "1";
		// authOn adds "off" to the off words. A value outside the union
		// is either silently true (envBoolOr) or silently false (== "1")
		// in the code — the typo this check exists to catch.
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "0", "false", "no", "off", "1", "true", "yes", "on":
		default:
			return bad("not a boolean (want 0/1, false/true, no/yes or off/on)")
		}
	case TypeDuration:
		if _, err := time.ParseDuration(v); err == nil {
			return nil
		}
		// Every duration setting except DASH_INTERVAL also reads a bare
		// whole number as seconds (envDuration and envDurationOrZero try
		// strconv.Atoi first; SPOOND_BUILD_TIMEOUT, JOB_MAX_RUNTIME too).
		// DASH_INTERVAL is read only as a Go duration, so a bare integer
		// would fail dash's own startup check — refuse it here with the
		// reason, not after the dashboard has bound its listener.
		if s.Name != "DASH_INTERVAL" {
			if _, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				return nil
			}
		}
		return bad("not a duration (seconds or a Go duration like 5m)")
	case TypePercent:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return bad("not a percentage")
		}
		if f < 0 || f > 100 {
			return bad("not a percentage from 0 to 100")
		}
	case TypeURL:
		u, err := url.Parse(v)
		if err != nil || u.Scheme == "" {
			return bad("not a URL with a scheme (e.g. https://host:port/base)")
		}
	case TypeAddr:
		if _, _, err := net.SplitHostPort(v); err != nil {
			return bad(err.Error())
		}
	case TypeList:
		// The split is the type's whole contract; the entries' shape
		// is each reader's own (and SIZE_PRESETS and
		// SPOOND_GUEST_DNS_ADDR already fail startup for theirs).
	case TypeString:
		// Read verbatim; nothing to check.
	}
	return nil
}

// HiddenFromFile is the VALUE both display surfaces use for a secret
// resolved from a <NAME>_FILE value file (#137 C2): reported set,
// hidden, with the source named. GET /api/admin/config (api) sends it
// and `spoond config show` (cmd/spoond-config) prints and compares it —
// one constant so the two cannot drift out of step. A secret's actual
// content never appears in either surface.
const HiddenFromFile = "(hidden, from file)"

// IsSecret reports whether name names a spec whose value is a secret
// (SpecOf's Secret flag) — HideIfSecret's name rule, as data. The C2
// design's secret list: every name whose upper case carries TOKEN, KEY,
// SECRET, PASSWORD or HASH among the known settings.
func IsSecret(name string) bool {
	s, ok := specByName[name]
	return ok && s.Secret
}

// SecretFileOf returns NAME_FILE for a secret setting name, or "" when
// name is not a known secret. Only secret specs take the _FILE form: a
// name that is not a secret has no file form (a <NAME>_FILE environment
// variable beside a non-secret NAME is the typo CheckEnvFile reports).
func SecretFileOf(name string) string {
	if !IsSecret(name) {
		return ""
	}
	return name + "_FILE"
}

// Get resolves one setting's value the C2 way (2026-10-11
// validated-config design item 2): the environment's NAME is the value;
// otherwise NAME_FILE names a file whose contents are the value, with
// trailing newlines and carriage returns trimmed (a secret saved with a
// closing echo, editor or CRLF newline stays byte-equal to the env
// spelling); setting both forms is an error, an unreadable file is an
// error, and a file with nothing but whitespace is an error. An unset NAME
// with an unset NAME_FILE returns ("", nil) — the reader's own default
// stands, exactly as when only NAME exists and is empty.
//
// get reads the environment (os.Getenv in production, a fake in tests);
// readFile reads a value file (os.ReadFile in production, a fake in
// tests). Only a non-empty NAME wins: an empty NAME= falls back to
// NAME_FILE, so the two spellings cannot argue about what unset means.
// A name with no spec (not a known secret) resolves through the
// environment only — the file form belongs to the secret specs.
func Get(name string, get func(string) string, readFile func(string) ([]byte, error)) (string, error) {
	var file string
	if s, ok := specByName[name]; ok && s.Secret {
		file = name + "_FILE"
	}
	envV, fileV := get(name), get(file) // get(nil-name) is "" for every injected map
	switch {
	case envV != "" && fileV != "":
		return "", fmt.Errorf("%s and %s are both set: give the value once (set %s= or %s=/path, not both)", name, file, name, file)
	case envV != "":
		return envV, nil
	case fileV != "":
		b, err := readFile(fileV)
		if err != nil {
			return "", fmt.Errorf("%s: read %s: %v", name, fileV, err)
		}
		v := strings.TrimRight(string(b), "\r\n")
		if strings.TrimSpace(v) == "" {
			// An empty secret would mean "unset" to its reader, and for
			// some (BOOTSTRAP_TOKEN) unset means open: a truncated or
			// never-written file must fail closed, not quietly unset.
			return "", fmt.Errorf("%s: %s is empty", name, fileV)
		}
		return v, nil
	}
	return "", nil
}

// Resolve is Get against the process environment and filesystem, plus
// FileModeWarning for the value file when the value came from one: the
// one call every binary makes for a secret, so the startup warning is
// the same in all of them. warn is "" when there is nothing to say.
func Resolve(name string) (value, warn string, err error) {
	value, err = Get(name, os.Getenv, os.ReadFile)
	if err != nil || os.Getenv(name) != "" {
		return value, "", err
	}
	if f := SecretFileOf(name); f != "" {
		if p := os.Getenv(f); p != "" {
			warn = FileModeWarning(p)
		}
	}
	return value, warn, nil
}

// FileModeWarning returns a warning line for a path whose file mode is
// readable by group or others (mode 0600 or stricter is clean), or ""
// when the mode is fine or cannot be checked (an unreadable file is
// Get's own startup error; a missing one the operator has not created
// yet is doctor's check).
func FileModeWarning(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Sprintf("%s: mode %.4o, want 600 (readable by group or others)", path, perm)
	}
	return ""
}

// IsKnown reports whether some spoond binary reads key. A secret's
// <NAME>_FILE name is known too: the readers take the secret through
// Get, which builds the file name from the spec, so no os.Getenv names
// it, but the name is a real setting (config show and doctor read the
// env files against this list) and must not be flagged unknown.
func IsKnown(key string) bool {
	if known[key] {
		return true
	}
	name, ok := strings.CutSuffix(key, "_FILE")
	return ok && IsSecret(name)
}

// HideIfSecret is the masking rule for showing a setting's value
// (#137): a name carrying TOKEN, KEY, SECRET, PASSWORD, HASH or SEED
// (case-insensitive) is a secret and its value is never shown —
// "(hidden)" stands in for it. The admin config endpoint (api) masks
// the values it serves with this, and `spoond config show`
// (cmd/spoond-config) masks the same way, so a secret value exists
// nowhere in any config display that uses it.
//
// Two more cases hide a value whatever its name: a setting listed in
// secretValued (whole values that are secrets without saying so, like
// webhook URLs whose path is the credential), and any value carrying a
// URL with credentials or a query string (user:pass@host, ?token=…).
func HideIfSecret(name, value string) string {
	upper := strings.ToUpper(name)
	for _, marker := range []string{"TOKEN", "KEY", "SECRET", "PASSWORD", "HASH", "SEED"} {
		if strings.Contains(upper, marker) {
			return "(hidden)"
		}
	}
	if secretValued[upper] || urlCarriesSecret(value) {
		return "(hidden)"
	}
	return value
}

// secretValued are settings whose whole value is a secret though the
// name does not say so: NOTIFY_WEBHOOKS lists Slack, Discord and ntfy
// URLs, and their paths are the credential.
var secretValued = map[string]bool{"NOTIFY_WEBHOOKS": true}

// urlCarriesSecret reports whether value holds a URL with credentials
// (scheme://user:pass@host) or a query string (?…=…), the two places a
// URL setting carries a token.
func urlCarriesSecret(value string) bool {
	for _, f := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		i := strings.Index(f, "://")
		if i < 0 {
			continue
		}
		rest := f[i+3:]
		host := rest
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			host = rest[:j]
		}
		if strings.Contains(host, "@") || strings.Contains(rest, "?") {
			return true
		}
	}
	return false
}

// CheckEnvFile reads a systemd EnvironmentFile and returns, sorted, the
// keys no spoond binary reads and the keys set more than once (where
// the last one silently wins). Values are never returned. A known
// secret's <NAME>_FILE spelling counts as known (IsKnown's rule): the
// env files carry it and Get resolves it, though no os.Getenv names it.
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
		if !IsKnown(k) {
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

var known = func() map[string]bool {
	m := make(map[string]bool, len(Known))
	for _, k := range Known {
		m[k] = true
	}
	return m
}()
