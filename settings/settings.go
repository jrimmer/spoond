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
	// flags) count only "1" on; == "" readers (LLM_OPEN_LEGACY) accept any
	// non-empty value, so no spelling is wrong and Validate passes it.
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
	// the readers split on "," and the entries' shape is the value's
	// own business (SIZE_PRESETS or SPOOND_GUEST_DNS_ADDR, which are
	// TypeString here, already fail startup on a malformed value).
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
// code parses it as, and the binary whose environment it is read from.
type Spec struct {
	Name   string
	Type   Type
	Binary Binary
}

// Specs is every known setting with its type and owner binary, in the
// same order as Known (sorted by name).
var Specs = []Spec{
	{"ADMIN_TOKEN", TypeString, Backend},
	{"ASSETS_DIR", TypeString, Backend}, // bare path: the proxy listener serves /assets/<file> from it
	{"BACKUP_MAX_AGE_SECS", TypeInt, Backend},
	{"BIND_ADDR", TypeAddr, Backend},
	{"BOOTSTRAP_TOKEN", TypeString, Backend},
	{"CHECKPOINT_INTERVAL_MINS", TypeInt, Backend},
	{"COLUMNS", TypeInt, Dash},
	{"CONFORMANCE_API", TypeURL, Conformance},
	{"CONFORMANCE_BACKEND_UNIT", TypeString, Conformance}, // systemctl unit name
	{"CONFORMANCE_CAPACITY", TypeBool, Conformance},
	{"CONFORMANCE_CAPACITY_IMAGE", TypeString, Conformance},
	{"CONFORMANCE_CRASH_TEST", TypeBool, Conformance},
	{"CONFORMANCE_DESTRUCTIVE", TypeBool, Conformance},
	{"CONFORMANCE_GUEST_SERVICE", TypeAddr, Conformance}, // host:port the suite probes from a lease
	{"CONFORMANCE_IDLE_IMAGE", TypeString, Conformance},
	{"CONFORMANCE_IMAGES", TypeList, Conformance},
	{"CONFORMANCE_LAN_TARGET", TypeAddr, Conformance},              // host:port (parseLANTarget)
	{"CONFORMANCE_MIXED_BLOCKED_PRIVATE", TypeString, Conformance}, // bare host or IP, probed with its own port setting
	{"CONFORMANCE_MIXED_DOMAIN", TypeString, Conformance},
	{"CONFORMANCE_MIXED_PRIVATE", TypeString, Conformance}, // bare host or IP, probed with its own port setting
	{"CONFORMANCE_MIXED_PRIVATE_DOMAIN", TypeString, Conformance},
	{"CONFORMANCE_MIXED_PRIVATE_PORT", TypeInt, Conformance},
	{"CONFORMANCE_MIXED_SSH_PORT", TypeInt, Conformance},
	{"CONFORMANCE_PROXY_SECRET", TypeString, Conformance},
	{"CONFORMANCE_PROXY_SUFFIX", TypeString, Conformance},
	{"CONFORMANCE_PROXY_URL", TypeURL, Conformance},
	{"CONFORMANCE_SECOND_TOKEN", TypeString, Conformance},
	{"CONFORMANCE_SSH", TypeString, Conformance},       // an ssh destination, or "local"
	{"CONFORMANCE_SSH_GATEWAY", TypeAddr, Conformance}, // ssh.Dial target host:port
	{"CONFORMANCE_SSH_KEY", TypeString, Conformance},   // private key file path
	{"CONFORMANCE_SUBSTRATE", TypeString, Conformance},
	{"CONFORMANCE_TOKEN", TypeString, Conformance},
	{"CONFORMANCE_USER", TypeString, Conformance},
	{"CONFORMANCE_USER_ID", TypeString, Conformance},
	{"CONSUMER_TOKENS", TypeList, Backend}, // token=consumer pairs
	{"CRASH_TEST", TypeBool, Backend},
	{"DASH_ADDR", TypeAddr, Dash},
	{"DASH_AUTH", TypeBool, Dash},
	{"DASH_DISK_DEVICE", TypeString, Dash},
	{"DASH_EVENTS_TOKEN", TypeString, Dash},
	{"DASH_HISTORY", TypeInt, Dash},
	{"DASH_HOST", TypeString, Dash},
	{"DASH_INTERVAL", TypeDuration, Dash}, // the only duration read as a Go duration only (no bare seconds)
	{"DASH_IO_FULL_BAD_PCT", TypePercent, Dash},
	{"DASH_IO_FULL_WARN_PCT", TypePercent, Dash},
	{"DASH_PASSWORD_HASH", TypeString, Dash},
	{"DASH_PROJECT_URL", TypeString, Dash}, // a schemeless value is accepted: the code prepends https://
	{"DASH_SERVICES", TypeList, Dash},
	{"DASH_TLS_CERT", TypeList, Dash}, // certificate paths, paired with DASH_TLS_KEY
	{"DASH_TLS_KEY", TypeList, Dash},  // key paths, paired with DASH_TLS_CERT
	{"DASH_USER", TypeString, Dash},
	{"DASH_WIDTH", TypeInt, Dash},
	{"DEFAULT_IMAGE", TypeString, Runner},
	{"DEFAULT_TTL_SECS", TypeInt, Backend},
	{"DISK_RESERVE_PCT", TypePercent, Backend}, // the code clamps 0..50; still a share of the disk
	{"DRAIN_MAX_SECS", TypeInt, Backend},
	{"DRAIN_RESUME_MAX_AGE", TypeDuration, Backend},
	{"DRAIN_SNAPSHOT_CONCURRENCY", TypeInt, Backend},
	{"E2B_CHECKPOINT_TIMEOUT", TypeDuration, Backend},
	{"E2B_CONTROL_TIMEOUT", TypeDuration, Backend},
	{"E2B_CREATE_TIMEOUT", TypeDuration, Backend},
	{"E2B_DELETE_TIMEOUT", TypeDuration, Backend},
	{"E2B_ENVD_VERSION", TypeString, Backend},
	{"E2B_FIRECRACKER_VERSION", TypeString, Backend},
	{"E2B_GRPC_ADDR", TypeAddr, Backend},
	{"E2B_NODEINFO_TIMEOUT", TypeDuration, Backend},
	{"E2B_PAUSE_TIMEOUT", TypeDuration, Backend},
	{"E2B_PROXY_URL", TypeURL, Backend},
	{"E2B_TEAM_ID", TypeString, Backend}, // a UUID, read verbatim
	{"E2B_TEMPLATE_STORAGE_PATH", TypeString, Backend},
	{"E2B_TOKEN_SEED_FILE", TypeString, Backend}, // seed file path
	{"EVENTS_TOKEN", TypeString, Backend},
	{"EXEC_TIMEOUT_SECS", TypeInt, Runner},
	{"FORGEJO_ADMIN_TOKEN", TypeString, Runner},
	{"FORGEJO_URL", TypeURL, Runner},
	{"GATEWAY_ADDR", TypeAddr, Doctor},
	{"GATEWAY_IMAGE_ALIASES", TypeList, Gateway}, // short=full pairs
	{"GATEWAY_METRICS_LISTEN", TypeAddr, Gateway},
	{"GATEWAY_SSH_IMAGES", TypeList, Gateway},
	{"GATEWAY_TOKEN", TypeString, Backend},
	{"GC_DELETE", TypeBool, Backend},
	{"GC_LOST_GRACE", TypeDuration, Backend},
	{"GC_LOST_GRACE_PERSISTENT", TypeDuration, Backend},
	{"HOST_API_PORT", TypeInt, Backend},
	{"HOST_GUEST_SERVICE_ADDR", TypeString, Backend}, // a bare host or IP: the code builds <addr>/32 CIDRs from it
	{"HOST_GUEST_SERVICE_PORT", TypeInt, Backend},
	{"IDLE_SUSPEND_DEFAULT_SECS", TypeInt, Backend},
	{"IMAGE_MAP", TypeList, Runner}, // label=image pairs
	{"IMAGE_REGISTRY", TypeString, Images},
	{"JOB_MAX_RUNTIME", TypeDuration, Backend},
	{"JOB_RECORD_DIR", TypeString, Runner},
	{"JOB_RETENTION_SECS", TypeInt, Backend},
	{"KEPT_DISK_WARN_PCT", TypePercent, Backend},
	{"LEASE_NETPOL", TypeString, Runner}, // none|lan|internet|restricted
	{"LEASE_NET_ALLOW", TypeList, Runner},
	{"LEASE_TOKEN", TypeString, Runner},
	{"LEASE_TTL", TypeInt, Runner},
	{"LEASE_URL", TypeURL, Runner},
	{"LLM_DEFAULT_MODEL", TypeString, Backend},
	{"LLM_GATEWAY_URL", TypeURL, Gateway},
	{"LLM_MAX_CONCURRENT_PER_USER", TypeInt, Backend},
	{"LLM_MODEL_MAP", TypeList, Backend},   // pattern=model pairs
	{"LLM_OPEN_LEGACY", TypeBool, Backend}, // any non-empty value turns it on: no spelling is wrong, so Validate passes every spelling
	{"LLM_UPSTREAM_KEY", TypeString, Backend},
	{"LLM_UPSTREAM_URL", TypeURL, Backend},
	{"MAX_ADMIT_WAIT_SECS", TypeInt, Backend},
	{"MAX_EXEC_BODY_BYTES", TypeInt, Backend},
	{"MAX_EXEC_TIMEOUT_SECS", TypeInt, Backend},
	{"MAX_KEPT_PER_LEASE", TypeInt, Backend},
	{"MAX_LEASE_MEMORY_MB", TypeInt, Backend},
	{"MAX_LEASE_VCPU", TypeInt, Backend},
	{"MAX_NAMED_SNAPSHOTS", TypeInt, Backend},
	{"MAX_RUNNING_JOBS_PER_LEASE", TypeInt, Backend},
	{"MAX_TTL_SECS", TypeInt, Backend},
	{"METRICS_LISTEN", TypeAddr, Runner},
	{"METRICS_SERVER_NAME", TypeString, Dash},
	{"METRICS_TOKEN", TypeString, Backend},
	{"METRICS_URL", TypeURL, Dash},
	{"NOTIFY_STATE_FILE", TypeString, Backend},
	{"NOTIFY_UNITS", TypeList, Backend},      // "none" watches nothing
	{"NOTIFY_WEBHOOKS", TypeString, Backend}, // a JSON list, validated by notify.ParseWebhooks
	{"ORPHAN_MIN_AGE_SECS", TypeInt, Backend},
	{"ORPHAN_QUARANTINE_SECS", TypeInt, Backend},
	{"ORPHAN_REAP", TypeString, Backend}, // off|dryrun|quarantine
	{"OTEL_PROM_URL", TypeURL, Backend},
	{"PAUSED_RELEASE_DAYS", TypeInt, Backend},
	{"PINNED_IDLE_NOTICE_DAYS", TypeInt, Backend},
	{"POOL_SIZE", TypeInt, Backend},
	{"PROXY_ADDR", TypeAddr, Backend},
	{"PROXY_AUTH_MODE", TypeString, Backend}, // off|forward-auth
	{"PROXY_AUTH_SECRET", TypeString, Backend},
	{"PROXY_AUTH_TRUSTED_PEERS", TypeList, Backend}, // IPs and CIDRs
	{"RECOVERY_RETRY_ATTEMPTS", TypeInt, Backend},
	{"RECOVERY_RETRY_WINDOW", TypeDuration, Backend},
	{"REPO_BASE_URL", TypeURL, Runner},
	{"ROOTFS_PROBE_SECS", TypeInt, Backend},
	{"RUNNER_ADMIT_WAIT_SECS", TypeInt, Runner},
	{"RUNNER_FLOOR", TypeInt, Runner},
	{"RUNNER_LABELS", TypeList, Runner},
	{"RUNNER_MAX", TypeInt, Runner},
	{"RUNNER_NAME", TypeString, Runner},
	{"RUNNER_SCALE_STEP", TypeInt, Runner},
	{"RUNNER_STATE_FILE", TypeString, Runner},
	{"RUNNER_TOKEN", TypeString, Runner},
	{"SANDBOX_PROBE", TypeBool, Backend},
	{"SANDBOX_PROBE_TIMEOUT_SECS", TypeInt, Backend},
	{"SATURATION_CPU_SOME_PCT", TypePercent, Backend},
	{"SATURATION_IO_FULL_PCT", TypePercent, Backend},
	{"SERVICE_RESULT", TypeString, Drain}, // systemd's own variable, read verbatim
	{"SHELLY_BINARY_URL", TypeURL, Gateway},
	{"SIZE_PRESETS", TypeString, Backend}, // name=vcpu:memory_mb spec; sizes.Parse fails startup on a malformed value
	{"SNAPSHOT_KEEP_VERSIONS", TypeInt, Backend},
	{"SNAPSHOT_WRITE_CONCURRENCY", TypeInt, Backend},
	{"SPOOND_ADMIN_TOKEN_FILE", TypeList, Drain}, // token file paths, paired with SPOOND_DRAIN_URL
	{"SPOOND_BACKUP_DIR", TypeString, Backend},
	{"SPOOND_BUILD_TIMEOUT", TypeDuration, Backend},
	{"SPOOND_DB_PATH", TypeString, Backend},
	{"SPOOND_DRAIN_INSECURE", TypeBool, Drain},
	{"SPOOND_DRAIN_URL", TypeList, Drain}, // backend URLs, paired with SPOOND_ADMIN_TOKEN_FILE
	{"SPOOND_ENV_DIR", TypeString, Doctor},
	{"SPOOND_GATEWAY_TOKEN", TypeString, Gateway},
	{"SPOOND_GUEST_DNS_ADDR", TypeList, Backend}, // bare IPs; parseGuestDNS fails startup on a non-IP
	{"SPOOND_PROXY_HOST_SUFFIX", TypeString, Backend},
	{"SPOOND_SSH_CONNECTION_ADDR", TypeString, Gateway}, // the bare host reported in SSH_CONNECTION
	{"SWEEP_TIMEOUT", TypeDuration, Backend},
	{"TLS_CERT", TypeList, Backend}, // certificate paths, paired with TLS_KEY
	{"TLS_KEY", TypeList, Backend},  // key paths, paired with TLS_CERT
	{"UNDRAIN_CONCURRENCY", TypeInt, Backend},
	{"UNDRAIN_READY_TIMEOUT", TypeDuration, Backend},
	{"UNDRAIN_RESUME_RETRIES", TypeInt, Backend},
	{"UNDRAIN_RESUME_WINDOW", TypeDuration, Backend},
	{"USERS_FILE", TypeString, Backend}, // identity store path
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
		// LLM_OPEN_LEGACY turns the legacy keyless mode on for any
		// non-empty value (the code tests == ""), so no spelling is
		// wrong for it — every value passes.
		if s.Name == "LLM_OPEN_LEGACY" {
			return nil
		}
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

// IsKnown reports whether some spoond binary reads key.
func IsKnown(key string) bool { return known[key] }

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

var known = func() map[string]bool {
	m := make(map[string]bool, len(Known))
	for _, k := range Known {
		m[k] = true
	}
	return m
}()
