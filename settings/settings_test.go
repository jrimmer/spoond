package settings

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// TestSpecsMatchKnown: every Known name has a Spec and every Spec names
// a Known setting — the two lists cannot drift apart (a Spec without a
// Known entry would validate a setting no binary reads; a Known entry
// without a Spec would leave one setting unvalidated).
func TestSpecsMatchKnown(t *testing.T) {
	if len(Specs) != len(Known) {
		t.Fatalf("len(Specs) = %d, len(Known) = %d", len(Specs), len(Known))
	}
	names := map[string]bool{}
	for i, s := range Specs {
		if names[s.Name] {
			t.Fatalf("duplicate spec for %s", s.Name)
		}
		names[s.Name] = true
		if s.Name != Known[i] {
			t.Fatalf("Specs[%d] = %q, want %q (Specs must stay in Known's sorted order)", i, s.Name, Known[i])
		}
	}
	for _, k := range Known {
		if !names[k] {
			t.Fatalf("%s is known but has no spec", k)
		}
	}
	// No spec may claim the placeholder binary: it holds no readers.
	for _, s := range Specs {
		if s.Binary == Other {
			t.Fatalf("%s: Binary Other holds no readers; give it the binary that reads it", s.Name)
		}
	}
}

// envRead matches the ways spoond code reads a setting by name.
var envRead = regexp.MustCompile(`\b(os\.Getenv|os\.LookupEnv|envIntOr|envOr|envDuration|envDurationOr|envDurationOrZero|envBoolOr|pct|limit|get|envInt|envBool|getenv|mustEnv|envFloat|env|envPct)\("([A-Z][A-Z0-9_]{2,})"`)

// envTable matches a setting named in a {"NAME", &dst} table, the way
// the conformance harness lists its required variables.
var envTable = regexp.MustCompile(`\{"([A-Z][A-Z0-9_]{2,})", &`)

// TestHideIfSecret (#137): a name carrying TOKEN, KEY, SECRET,
// PASSWORD, HASH or SEED (case-insensitive, anywhere in the name) is
// masked to "(hidden)"; a plain name passes its value through.
func TestHideIfSecret(t *testing.T) {
	cases := []struct {
		name, value, want string
	}{
		{"ADMIN_TOKEN", "sekrit", "(hidden)"},
		{"LLM_UPSTREAM_KEY", "sk-1", "(hidden)"},
		{"proxy_auth_secret", "s", "(hidden)"}, // case-insensitive
		{"DASH_PASSWORD_HASH", "h", "(hidden)"},
		{"E2B_TOKEN_SEED_FILE", "/x", "(hidden)"}, // SEED-only marker
		{"NOTIONAL_PASSWORD", "p", "(hidden)"},
		{"BIND_ADDR", "127.0.0.1:8890", "127.0.0.1:8890"},
		{"", "x", "x"},
	}
	for _, c := range cases {
		if got := HideIfSecret(c.name, c.value); got != c.want {
			t.Errorf("HideIfSecret(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestKnownMatchesSource: Known is exactly the settings the source reads
// — a new setting must be added to it, and a removed one dropped.
func TestKnownMatchesSource(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			switch rel {
			case ".claude", ".git", "tests", "node_modules":
				return filepath.SkipDir
			}
			if strings.HasSuffix(rel, "/gen") || strings.Contains(rel, "/gen/") {
				return filepath.SkipDir
			}
			return nil
		}
		// The conformance suite is test-only: its settings are in its
		// _test.go files. Everywhere else a _test.go file only sets
		// environment for a test.
		test := strings.HasSuffix(p, "_test.go")
		if !strings.HasSuffix(p, ".go") || (test && !strings.HasPrefix(rel, "conformance/")) {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, m := range envRead.FindAllSubmatch(b, -1) {
			found[string(m[2])] = true
		}
		for _, m := range envTable.FindAllSubmatch(b, -1) {
			found[string(m[1])] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var missing, stale []string
	for k := range found {
		if !IsKnown(k) {
			missing = append(missing, k)
		}
	}
	for _, k := range Known {
		if !found[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 || len(stale) > 0 {
		t.Fatalf("settings.Known is out of date: add %v, remove %v", missing, stale)
	}
	if !slices.IsSorted(Known) {
		t.Fatal("settings.Known must stay sorted")
	}
}

// TestCheckEnvFile: unknown and repeated keys are reported, comments,
// blank lines and export prefixes are understood, and values never
// appear in the result.
func TestCheckEnvFile(t *testing.T) {
	unknown, dup := CheckEnvFile(`# backend
ADMIN_TOKEN=s3cret
export POOL_SIZE=2
BURST_RESERVE_MIB=4096

POOL_SIZE=3
SIZE_PRESET=small=1:2048
`)
	if !slices.Equal(unknown, []string{"BURST_RESERVE_MIB", "SIZE_PRESET"}) {
		t.Fatalf("unknown = %v", unknown)
	}
	if !slices.Equal(dup, []string{"POOL_SIZE"}) {
		t.Fatalf("duplicate = %v", dup)
	}
}

// TestHideIfSecretValues: values that carry a secret without a telling
// name are hidden — webhook lists and URLs with credentials or a query
// string — while a plain URL or path still shows.
func TestHideIfSecretValues(t *testing.T) {
	for _, c := range []struct{ name, value, want string }{
		{"NOTIFY_WEBHOOKS", "https://hooks.slack.com/services/T0/B0/xyz", "(hidden)"},
		{"LLM_UPSTREAM_URL", "https://user:pw@llm.example.com/v1", "(hidden)"},
		{"OTEL_PROM_URL", "http://127.0.0.1:19464/metrics?auth=abc", "(hidden)"},
		{"LLM_UPSTREAM_URL", "https://llm.example.com/v1", "https://llm.example.com/v1"},
		{"SPOOND_DB_PATH", "/var/lib/spoond/spoond.db", "/var/lib/spoond/spoond.db"},
		{"BIND_ADDR", "0.0.0.0:8890", "0.0.0.0:8890"},
	} {
		if got := HideIfSecret(c.name, c.value); got != c.want {
			t.Errorf("HideIfSecret(%s, %q) = %q, want %q", c.name, c.value, got, c.want)
		}
	}
}

// TestValidateTypes is the table of good and bad values per type, one
// block per type. Good values must pass Validate for the spec's owner;
// bad values must come back with exactly one error naming the setting
// and the bad value.
func TestValidateTypes(t *testing.T) {
	cases := []struct {
		name string // a known setting; its spec decides the owner
		good []string
		bad  []string
	}{
		// int: strconv.Atoi ("2h" would silently fall back to the
		// default; that is the typo this type exists to catch).
		{"MAX_TTL_SECS", []string{"0", "3600", "-1"}, []string{"2h", " 3600x", "1.5", "lots"}},
		// bool: the union of the readers' spellings.
		{"SANDBOX_PROBE", []string{"1", "true", "yes", "on", "0", "false", "no", "Off"}, []string{"maybe", "2", "y"}},
		// duration: a Go duration or, for every duration but
		// DASH_INTERVAL, a bare whole number of seconds.
		{"SWEEP_TIMEOUT", []string{"30s", "5m", "1h30m", "600", "90"}, []string{"soon", "1hour", "2d3"}},
		// percent: a float from 0 to 100.
		{"SATURATION_IO_FULL_PCT", []string{"0", "20", "62.5", "100"}, []string{"101", "-1", "half", "20%"}},
		// url: parses with a scheme.
		{"LLM_UPSTREAM_URL", []string{"https://openrouter.ai/api/v1", "http://127.0.0.1:19464/metrics"},
			[]string{"127.0.0.1:8890/a", "localhost", "http://badport:9x/"}},
		// addr: host:port (net.SplitHostPort).
		{"BIND_ADDR", []string{"127.0.0.1:8890", "0.0.0.0:8890", ":8890"}, []string{"127.0.0.1", "8890"}},
		// list: comma-separated; the split is the contract.
		{"NOTIFY_UNITS", []string{"a.service,b.service", "none", "one.service"}, nil},
		// string: read verbatim, nothing to check.
		{"SPOOND_DB_PATH", []string{"/var/lib/spoond/spoond.db", "a b c"}, nil},
		// The exceptions, pinned in the table too: DASH_INTERVAL takes
		// no bare seconds (dash's own reader wants a Go duration), and
		// LLM_OPEN_LEGACY accepts any non-empty value.
		{"DASH_INTERVAL", []string{"2s", "500ms", "5s"}, []string{"2", "soon"}},
		{"LLM_OPEN_LEGACY", []string{"1", "yes-please"}, nil},
	}
	for _, c := range cases {
		spec, ok := SpecOf(c.name)
		if !ok {
			t.Fatalf("%s: not in Specs", c.name)
		}
		for _, g := range c.good {
			if g == "" {
				continue
			}
			errs := Validate(spec.Binary.String(), func(k string) string {
				if k == c.name {
					return g
				}
				return ""
			})
			if len(errs) != 0 {
				t.Errorf("%s=%q (type %v): Validate = %v, want none", c.name, g, spec.Type, errs)
			}
		}
		for _, b := range c.bad {
			if b == "" {
				continue // "" means unset, never validated
			}
			errs := Validate(spec.Binary.String(), func(k string) string {
				if k == c.name {
					return b
				}
				return ""
			})
			if len(errs) != 1 {
				t.Errorf("%s=%q (type %v): Validate = %v (%d errors), want exactly one", c.name, b, spec.Type, errs, len(errs))
				continue
			}
			err := errs[0]
			if !strings.Contains(err.Error(), c.name) {
				t.Errorf("%s=%q: error %v does not name the setting", c.name, b, err)
			}
			if !strings.Contains(err.Error(), b) {
				t.Errorf("%s=%q: error %v does not name the value", c.name, b, err)
			}
		}
	}
	// Two typed real-name cases beyond the per-type blocks: a bool
	// "maybe" on the == "1" reader (the code would read it as off) and
	// a negative percent on the clamped reader (the code would clamp
	// it; it is a typo, not a floor).
	for _, c := range []struct{ name, val string }{
		{"GC_DELETE", "maybe"},
		{"DISK_RESERVE_PCT", "-0.5"},
	} {
		spec, _ := SpecOf(c.name)
		errs := Validate(spec.Binary.String(), func(k string) string {
			if k == c.name {
				return c.val
			}
			return ""
		})
		if len(errs) != 1 {
			t.Errorf("%s=%q: Validate = %v, want one error", c.name, c.val, errs)
		}
	}
}

// TestValidateSkipsUnsetAndOtherBinaries: a setting left unset passes
// (it keeps its default), and an owner's validation ignores another
// binary's badly set setting — the backend must not refuse to start
// over a DASH_INTERVAL it never reads.
func TestValidateSkipsUnsetAndOtherBinaries(t *testing.T) {
	if errs := Validate("backend", func(string) string { return "" }); len(errs) != 0 {
		t.Fatalf("all unset: Validate(backend) = %v, want none", errs)
	}
	env := map[string]string{"DASH_INTERVAL": "not-a-duration", "NOTIFY_UNITS": "x.service"}
	if errs := Validate("backend", func(k string) string { return env[k] }); len(errs) != 0 {
		t.Fatalf("Validate(backend) = %v, want none: the backend reads neither setting", errs)
	}
	env = map[string]string{"DASH_INTERVAL": "not-a-duration", "DASH_HISTORY": "lots"}
	errs := Validate("dash", func(k string) string { return env[k] })
	if len(errs) != 2 {
		t.Fatalf("Validate(dash) = %d errors, want 2", len(errs))
	}
	// Specs walks in Known's sorted order, so the errors come out
	// DASH_HISTORY before DASH_INTERVAL whatever the env's shape.
	for i, want := range []string{"DASH_HISTORY", "DASH_INTERVAL"} {
		if !strings.Contains(errs[i].Error(), want) {
			t.Errorf("Validate(dash)[%d] = %v, want it to name %s", i, errs[i], want)
		}
	}
}

// TestValidateMasksSecrets: a bad value for a secret-named setting is
// reported masked to "(hidden)" — a wrong token must never land in a
// startup log — while a failing plain name shows its value.
func TestValidateMasksSecrets(t *testing.T) {
	err := validateOne(Spec{Name: "RUNNER_TOKEN", Type: TypeInt, Binary: Runner}, "2h")
	if err == nil {
		t.Fatal("RUNNER_TOKEN=2h as int: no error")
	}
	if !strings.Contains(err.Error(), "RUNNER_TOKEN") || !strings.Contains(err.Error(), "(hidden)") {
		t.Fatalf("error %v does not name RUNNER_TOKEN with a hidden value", err)
	}
	if strings.Contains(err.Error(), "2h") {
		t.Fatalf("error %v leaks the value of a secret-named setting", err)
	}
	// The value-only secret (NOTIFY_WEBHOOKS) is masked by the same rule.
	if got := HideIfSecret("NOTIFY_WEBHOOKS", "https://hooks.slack.com/services/T0/B0/xyz"); got != "(hidden)" {
		t.Fatalf("NOTIFY_WEBHOOKS masking = %q", got)
	}
	// A plain name's bad value stays visible.
	err = validateOne(Spec{Name: "MAX_TTL_SECS", Type: TypeInt, Binary: Backend}, "2h")
	if err == nil || !strings.Contains(err.Error(), `="2h"`) {
		t.Fatalf("MAX_TTL_SECS error = %v, want the value shown", err)
	}
}

// TestValidateBackendEntry is the backend's entry point as main runs
// it: a bad MAX_TTL_SECS comes back ready to be joined into the one
// fatal message, and a sb-shaped environment (the production host's
// /etc/spoond/backend.env: PRODUCTION-ENV-2.7 plus the example file)
// passes clean.
func TestValidateBackendEntry(t *testing.T) {
	bad := Validate("backend", func(k string) string {
		switch k {
		case "MAX_TTL_SECS":
			return "2h"
		case "BIND_ADDR":
			return "8890-only"
		}
		return ""
	})
	if len(bad) != 2 {
		t.Fatalf("Validate(backend) = %v (%d), want 2", bad, len(bad))
	}
	joined := strings.Join(errStrings(bad), "; ")
	if !strings.Contains(joined, `config: MAX_TTL_SECS="2h": not an integer`) {
		t.Fatalf("joined = %q, want the MAX_TTL_SECS message", joined)
	}
	if !strings.Contains(joined, `config: BIND_ADDR="8890-only":`) {
		t.Fatalf("joined = %q, want the BIND_ADDR message", joined)
	}

	sbEnv := map[string]string{
		"CONSUMER_TOKENS":             "changeme=forgejo",
		"HOST_GUEST_SERVICE_ADDR":     "10.1.0.11",
		"HOST_GUEST_SERVICE_PORT":     "8891",
		"SPOOND_GUEST_DNS_ADDR":       "10.1.0.2",
		"SPOOND_PROXY_HOST_SUFFIX":    ".sandbox.lacy.casa",
		"E2B_GRPC_ADDR":               "127.0.0.1:5008",
		"E2B_PROXY_URL":               "http://127.0.0.1:5007",
		"E2B_TOKEN_SEED_FILE":         "/etc/spoond/e2b-token-seed",
		"E2B_TEMPLATE_STORAGE_PATH":   "/forkdcache/e2b/storage/templates",
		"IMAGE_REGISTRY":              "localhost:5000",
		"SPOOND_DB_PATH":              "/var/lib/spoond/spoond.db",
		"SPOOND_BACKUP_DIR":           "/var/lib/spoond/backups",
		"BIND_ADDR":                   "127.0.0.1:8890",
		"PROXY_ADDR":                  "0.0.0.0:8891",
		"TLS_CERT":                    "/etc/spoond/tls/fullchain.pem",
		"TLS_KEY":                     "/etc/spoond/tls/privkey.pem",
		"ADMIN_TOKEN":                 "shh",
		"METRICS_TOKEN":               "mtok",
		"EVENTS_TOKEN":                "etok",
		"GATEWAY_TOKEN":               "gtok",
		"BOOTSTRAP_TOKEN":             "btok",
		"USERS_FILE":                  "/var/lib/spoond/users.json",
		"LLM_UPSTREAM_URL":            "https://openrouter.ai/api/v1",
		"LLM_UPSTREAM_KEY":            "sk-test",
		"LLM_MODEL_MAP":               "exe.dev/model-a=upstream-a",
		"LLM_MAX_CONCURRENT_PER_USER": "4",
		"OTEL_PROM_URL":               "http://127.0.0.1:19464/metrics",
		"SPOOND_BUILD_TIMEOUT":        "90m",
		"SWEEP_TIMEOUT":               "15m",
		"SANDBOX_PROBE":               "1",
		"SANDBOX_PROBE_TIMEOUT_SECS":  "20",
		"CHECKPOINT_INTERVAL_MINS":    "0",
		"MAX_KEPT_PER_LEASE":          "4",
		"MAX_NAMED_SNAPSHOTS":         "64",
		"SNAPSHOT_KEEP_VERSIONS":      "3",
		"SNAPSHOT_WRITE_CONCURRENCY":  "2",
		"DRAIN_SNAPSHOT_CONCURRENCY":  "2",
		"UNDRAIN_CONCURRENCY":         "2",
		"UNDRAIN_RESUME_RETRIES":      "2",
		"UNDRAIN_READY_TIMEOUT":       "180s",
		"UNDRAIN_RESUME_WINDOW":       "5m",
		"DRAIN_MAX_SECS":              "900",
		"DRAIN_RESUME_MAX_AGE":        "24h",
		"RECOVERY_RETRY_ATTEMPTS":     "3",
		"RECOVERY_RETRY_WINDOW":       "30m",
		"MAX_RUNNING_JOBS_PER_LEASE":  "16",
		"JOB_RETENTION_SECS":          "604800",
		"JOB_MAX_RUNTIME":             "24h",
		"CRASH_TEST":                  "0",
		"SIZE_PRESETS":                "small=1:2048,medium=2:4096",
		"MAX_LEASE_VCPU":              "8",
		"MAX_LEASE_MEMORY_MB":         "32768",
		"PAUSED_RELEASE_DAYS":         "30",
		"PINNED_IDLE_NOTICE_DAYS":     "7",
		"MAX_ADMIT_WAIT_SECS":         "600",
		"SATURATION_IO_FULL_PCT":      "20",
		"SATURATION_CPU_SOME_PCT":     "75",
		"BACKUP_MAX_AGE_SECS":         "93600",
		"KEPT_DISK_WARN_PCT":          "40",
		"DISK_RESERVE_PCT":            "5",
		"GC_DELETE":                   "0",
		"ORPHAN_REAP":                 "quarantine",
		"ORPHAN_MIN_AGE_SECS":         "3600",
		"ORPHAN_QUARANTINE_SECS":      "86400",
		"POOL_SIZE":                   "0",
		"DEFAULT_TTL_SECS":            "300",
		"MAX_TTL_SECS":                "3600",
		"MAX_EXEC_TIMEOUT_SECS":       "300",
		"MAX_EXEC_BODY_BYTES":         "8388608",
		"ASSETS_DIR":                  "/opt/spoond/assets",
		"NOTIFY_WEBHOOKS":             `[{"url":"https://hooks.example/T0/B0/x","format":"json"}]`,
		"NOTIFY_UNITS":                "e2b-orchestrator.service,spoond-sshd-gateway.service",
		"NOTIFY_STATE_FILE":           "/var/lib/spoond/notify-state.json",
		"ROOTFS_PROBE_SECS":           "20",
		"HOST_API_PORT":               "8890",
	}
	if errs := Validate("backend", func(k string) string { return sbEnv[k] }); len(errs) != 0 {
		t.Fatalf("sb-like env: Validate(backend) = %v, want none", errs)
	}
}

// errStrings maps errors to their messages for joining.
func errStrings(errs []error) []string {
	out := make([]string, len(errs))
	for i, e := range errs {
		out[i] = e.Error()
	}
	return out
}

// TestTypeDerivation pins the type-derivation rules the Spec table was
// built with: a name ending _URL is url unless the code reads it as a
// plain value (DASH_PROJECT_URL gets https:// prepended; SPOOND_DRAIN_URL
// is a comma list of URLs), _ADDR/_LISTEN (and PROXY_ADDR, BIND_ADDR) is
// addr unless the value is a bare host or IP list (HOST_GUEST_SERVICE_ADDR,
// SPOOND_SSH_CONNECTION_ADDR, SPOOND_GUEST_DNS_ADDR), and _PCT is percent
// — so a spec's type always mirrors how the reading code parses the value.
func TestTypeDerivation(t *testing.T) {
	for _, s := range Specs {
		n := s.Name
		switch {
		case n == "DASH_PROJECT_URL" || n == "SPOOND_DRAIN_URL":
			// read verbatim / as a list, deliberately not url
		case strings.HasSuffix(n, "_URL"):
			if s.Type != TypeURL {
				t.Errorf("%s typed %v, want url", n, s.Type)
			}
		case n == "HOST_GUEST_SERVICE_ADDR" || n == "SPOOND_SSH_CONNECTION_ADDR" || n == "SPOOND_GUEST_DNS_ADDR":
			// bare hosts and IP lists, not host:port
		case strings.HasSuffix(n, "_ADDR") || strings.HasSuffix(n, "_LISTEN") || n == "PROXY_ADDR":
			if s.Type != TypeAddr {
				t.Errorf("%s typed %v, want addr", n, s.Type)
			}
		case strings.HasSuffix(n, "_PCT"):
			if s.Type != TypePercent {
				t.Errorf("%s typed %v, want percent", n, s.Type)
			}
		}
		if s.Type.String() == "unknown" || s.Binary.String() == "unknown" || s.Binary == Other {
			t.Errorf("%s carries type %v binary %v", s.Name, s.Type, s.Binary)
		}
	}
}
