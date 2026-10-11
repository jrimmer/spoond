package spoondbackend

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v3/api"
	"github.com/jrimmer/spoond/v3/sizes"
)

// TestParseGuestDNS pins the startup validation of SPOOND_GUEST_DNS_ADDR:
// empty is allowed (no allowance); bare IPs (trimmed) are accepted; a
// non-empty value that names no address or names a non-IP is rejected.
func TestParseGuestDNS(t *testing.T) {
	cases := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{"", nil, false},
		{"   ", nil, true},
		{" , ", nil, true},
		{",", nil, true},
		{"10.1.0.2", []string{"10.1.0.2"}, false},
		{"10.1.0.2,10.1.0.3", []string{"10.1.0.2", "10.1.0.3"}, false},
		{" 10.1.0.2 , 10.1.0.3 ,", []string{"10.1.0.2", "10.1.0.3"}, false},
		{"not-an-ip", nil, true},
		{"10.1.0.2,not-an-ip", nil, true},
		{"10.1.0.0/24", nil, true},
	}
	for _, tc := range cases {
		got, err := parseGuestDNS(tc.in)
		if (err != nil) != tc.wantErr {
			t.Fatalf("parseGuestDNS(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
		}
		if !tc.wantErr && !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("parseGuestDNS(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestEnvDurationOrZero pins spoond-52c NIT: DRAIN_RESUME_MAX_AGE reads a
// zero or negative Go duration from the environment (0 means the default,
// a negative disables the bound), unlike envDurationOr, which ignores
// non-positive values. A bare integer is a number of seconds (matching
// envDuration), so it is not silently read as the default 24h.
func TestEnvDurationOrZero(t *testing.T) {
	const key = "DRAIN_RESUME_MAX_AGE_TEST"

	t.Setenv(key, "0s")
	if got := envDurationOrZero(key, 24*time.Hour); got != 0 {
		t.Fatalf("envDurationOrZero(0s) = %s, want 0", got)
	}
	t.Setenv(key, "-5m")
	if got := envDurationOrZero(key, 24*time.Hour); got != -5*time.Minute {
		t.Fatalf("envDurationOrZero(-5m) = %s, want -5m", got)
	}
	t.Setenv(key, "9h")
	if got := envDurationOrZero(key, 24*time.Hour); got != 9*time.Hour {
		t.Fatalf("envDurationOrZero(9h) = %s, want 9h", got)
	}
	// A bare integer is seconds, including 0 (the default) and a negative
	// value (disable the bound).
	t.Setenv(key, "3600")
	if got := envDurationOrZero(key, 24*time.Hour); got != time.Hour {
		t.Fatalf("envDurationOrZero(3600) = %s, want 1h", got)
	}
	t.Setenv(key, "0")
	if got := envDurationOrZero(key, 24*time.Hour); got != 0 {
		t.Fatalf("envDurationOrZero(0) = %s, want 0", got)
	}
	t.Setenv(key, "-5")
	if got := envDurationOrZero(key, 24*time.Hour); got != -5*time.Second {
		t.Fatalf("envDurationOrZero(-5) = %s, want -5s", got)
	}
	// A missing or malformed value falls back to the default.
	t.Setenv(key, "not-a-duration")
	if got := envDurationOrZero(key, 24*time.Hour); got != 24*time.Hour {
		t.Fatalf("envDurationOrZero(malformed) = %s, want the default", got)
	}
}

// TestParseJobMaxRuntime pins JOB_MAX_RUNTIME parsing (spoond-wb5): empty
// or malformed is 0 (the 24 h default), a negative value disables the
// cap, a Go duration or whole seconds both work, a fractional duration is
// kept as parsed (jobMaxRuntimeSecs rounds it up to whole seconds), a
// negative value under one second is rejected (it would round to 0 = the
// default rather than "off"), and a bare integer too large to represent
// is clamped rather than overflowing the seconds conversion.
func TestParseJobMaxRuntime(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"24h", 24 * time.Hour, false},
		{"3600", time.Hour, false},
		{"-1", -time.Second, false},
		{"-5m", -5 * time.Minute, false},
		{"-500ms", 0, true},
		{"500ms", 500 * time.Millisecond, false},
		{"1500ms", 1500 * time.Millisecond, false},
		{"not-a-duration", 0, false},
		// A bare integer above the ~9.2e9 seconds a Duration can hold is
		// clamped, not wrapped negative (which would read as off).
		{"9223372036854775807", time.Duration(9223372036) * time.Second, false},
	}
	for _, tc := range cases {
		got, err := parseJobMaxRuntime(tc.in)
		if (err != nil) != tc.wantErr {
			t.Fatalf("parseJobMaxRuntime(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
		}
		if !tc.wantErr && got != tc.want {
			t.Fatalf("parseJobMaxRuntime(%q) = %s, want %s", tc.in, got, tc.want)
		}
		if tc.in == "9223372036854775807" && got < 0 {
			t.Fatalf("parseJobMaxRuntime(%q) = %s, want a non-negative clamp", tc.in, got)
		}
	}
}

// TestJobMaxRuntimeSecs pins the whole-second conversion (spoond-wb5): a
// positive fractional duration rounds up to the next whole second (so a
// sub-second cap is 1 s, not 0 = the default, and 1500ms is 2 s, not a
// truncated 1 s), zero stays the default, and a negative stays negative
// (off).
func TestJobMaxRuntimeSecs(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want int64
	}{
		{0, 0},
		{500 * time.Millisecond, 1},
		{time.Second, 1},
		{1500 * time.Millisecond, 2},
		{24 * time.Hour, 86400},
		{-time.Second, -1},
		{-5 * time.Minute, -300},
	}
	for _, tc := range cases {
		if got := jobMaxRuntimeSecs(tc.in); got != tc.want {
			t.Fatalf("jobMaxRuntimeSecs(%s) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestWarnRemovedEnv pins the FS5 removed-variable warning: each removed
// variable that is still set logs one line, and an unset one stays
// silent. The variables are ignored, never read into the config.
func TestWarnRemovedEnv(t *testing.T) {
	set := map[string]string{
		"HELD_IDLE_TIMEOUT_SECS":      "14400",
		"HELD_SUSPENDED_RELEASE_SECS": "604800",
		"HOLD_TTL_SECS":               "604800",
		"HOLD_TTL_MAX_SECS":           "2592000",
		"PRESSURE_HELD_IDLE_SECS":     "1800",
		"IDLE_TIMEOUT_SECS":           "3600",
	}
	for _, name := range removedEnvVars {
		if _, ok := set[name]; !ok {
			t.Fatalf("removed env %s has no test case", name)
		}
	}
	var lines []string
	logf := captureLogger{&lines}
	warnRemovedEnv(logf, func(name string) string { return set[name] })
	if len(lines) != len(removedEnvVars) {
		t.Fatalf("warnRemovedEnv logged %d lines, want %d: %v", len(lines), len(removedEnvVars), lines)
	}
	lines = nil
	warnRemovedEnv(logf, func(string) string { return "" })
	if len(lines) != 0 {
		t.Fatalf("warnRemovedEnv logged with nothing set: %v", lines)
	}
}

// TestSizeConfig pins the lease size settings (#151): unset reads as the
// default presets, the host's CPU count and 32768 MiB; set values are
// used; a malformed preset spec or a limit that is not a positive
// integer is a startup error.
func TestSizeConfig(t *testing.T) {
	presets, vcpu, mem, err := sizeConfig(func(string) string { return "" })
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if !reflect.DeepEqual(presets, sizes.Default()) || vcpu != runtime.NumCPU() || mem != api.DefaultMaxLeaseMemoryMB {
		t.Fatalf("defaults = %+v %d %d", presets, vcpu, mem)
	}

	env := map[string]string{
		"SIZE_PRESETS":        "small=1:1024,big=4:6144",
		"MAX_LEASE_VCPU":      "8",
		"MAX_LEASE_MEMORY_MB": "16384",
	}
	presets, vcpu, mem, err = sizeConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	want := []sizes.Preset{{Name: "small", VCPU: 1, MemoryMB: 1024}, {Name: "big", VCPU: 4, MemoryMB: 6144}}
	if !reflect.DeepEqual(presets, want) || vcpu != 8 || mem != 16384 {
		t.Fatalf("set = %+v %d %d", presets, vcpu, mem)
	}

	for key, bad := range map[string]string{
		"SIZE_PRESETS":        "small=3:1024",
		"MAX_LEASE_VCPU":      "0",
		"MAX_LEASE_MEMORY_MB": "32g",
	} {
		_, _, _, err := sizeConfig(func(k string) string {
			if k == key {
				return bad
			}
			return ""
		})
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s=%q: err = %v, want a startup error naming it", key, bad, err)
		}
	}
}

// captureLogger collects Printf output for the warning test.
type captureLogger struct{ lines *[]string }

func (c captureLogger) Printf(format string, args ...any) {
	*c.lines = append(*c.lines, format)
}

// TestSaturationConfig (#136 stage 2): unset targets take the defaults,
// 0 is accepted (that check off), and anything not a percentage fails
// startup.
func TestSaturationConfig(t *testing.T) {
	io, cpu, err := saturationConfig(func(string) string { return "" })
	if err != nil || io != api.DefaultSaturationIOFullPct || cpu != api.DefaultSaturationCPUSomePct {
		t.Fatalf("defaults = %v, %v, %v", io, cpu, err)
	}
	env := map[string]string{"SATURATION_IO_FULL_PCT": "0", "SATURATION_CPU_SOME_PCT": "62.5"}
	io, cpu, err = saturationConfig(func(k string) string { return env[k] })
	if err != nil || io != 0 || cpu != 62.5 {
		t.Fatalf("set = %v, %v, %v", io, cpu, err)
	}
	for _, bad := range []string{"-1", "101", "lots"} {
		env := map[string]string{"SATURATION_CPU_SOME_PCT": bad}
		if _, _, err := saturationConfig(func(k string) string { return env[k] }); err == nil {
			t.Fatalf("SATURATION_CPU_SOME_PCT=%q accepted", bad)
		}
	}
}

// TestBackendValidateEntry (#137 C1) checks the backend's validation
// entry as Main wires it: a bad int (MAX_TTL_SECS=2h, which every TTL
// reader would silently fall back from) refuses startup, the fatal
// message names every bad setting joined with "; " and never echoes a
// secret-named setting's value, and the sb-like environment (the
// production host's /etc/spoond/backend.env) passes clean. A child
// process is the only way to observe a log.Fatalf: Main runs there and
// the exit status and stderr are the assertion.
func TestBackendValidateEntry(t *testing.T) {
	if os.Getenv("SPOOND_TEST_VALIDATE_MAIN") == "" {
		return // the assertions run in the child processes, below
	}
	Main(nil) // must log.Fatalf and never return
}

// BuildTestBinary builds the umbrella spoond binary into t.TempDir()
// once per test and returns its path (skipping the test when no Go
// toolchain is available, e.g. cross-built test runs).
func BuildTestBinary(t *testing.T, pkg string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "spoond")
	cmd := exec.Command("go", "build", "-o", bin, pkg)
	if out, err := cmd.CombinedOutput(); err != nil {
		if execErr, ok := err.(*exec.Error); ok && execErr.Err == exec.ErrNotFound {
			t.Skipf("no go toolchain: %v", err)
		}
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func TestBackendValidateEntryChildren(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: no child builds")
	}
	spoondBin := BuildTestBinary(t, "../../cmd/spoond")
	runChild := func(env map[string]string) (code int, stderr string) {
		cmd := exec.Command(spoondBin, "backend")
		cmd.Env = append(os.Environ(), "SPOOND_TEST_VALIDATE_MAIN=1")
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		var buf bytes.Buffer
		cmd.Stderr = &buf
		err := cmd.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), buf.String()
		}
		if err != nil {
			t.Fatalf("run child: %v\nstderr:\n%s", err, buf.String())
		}
		return 0, buf.String()
	}

	// A bad int and a bad address: both named, joined with "; ".
	code, stderr := runChild(map[string]string{
		"MAX_TTL_SECS":            "2h",
		"BIND_ADDR":               "8890-only",
		"CONSUMER_TOKENS":         "abc=forgejo",
		"HOST_GUEST_SERVICE_ADDR": "10.1.0.11",
	})
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, `config: MAX_TTL_SECS="2h": not an integer`) {
		t.Fatalf("stderr = %q, want the MAX_TTL_SECS message", stderr)
	}
	if !strings.Contains(stderr, `config: BIND_ADDR="8890-only":`) {
		t.Fatalf("stderr = %q, want the BIND_ADDR message in the same fatal line", stderr)
	}
	if strings.Count(stderr, "config: ") != 2 {
		t.Fatalf("stderr = %q, want both problems in one message", stderr)
	}

	// The process environment carries a secret too (METRICS_TOKEN, as
	// the real deploy does): it must not appear in the fatal line, which
	// only echoes a plain-typed setting's value. (Every token-typed
	// setting is a TypeString, so it never errors — and if a typed
	// secret ever fails, settings.Validate masks it to "(hidden)".)
	code, stderr = runChild(map[string]string{
		"METRICS_TOKEN":           "sekrit-value",
		"MAX_TTL_SECS":            "soon",
		"CONSUMER_TOKENS":         "abc=forgejo",
		"HOST_GUEST_SERVICE_ADDR": "10.1.0.11",
	})
	if code != 1 || !strings.Contains(stderr, "MAX_TTL_SECS") || strings.Contains(stderr, "sekrit-value") {
		t.Fatalf("secret leak: exit %d stderr %q, want no METRICS_TOKEN value in the fatal line", code, stderr)
	}

	// The sb-like environment passes validation: Main gets past the
	// config gate (it then fails later on the token seed, whose absence
	// proves nothing about config; the assertion is no "config: " line).
	code, stderr = runChild(map[string]string{
		"CONSUMER_TOKENS":           "changeme=forgejo",
		"HOST_GUEST_SERVICE_ADDR":   "10.1.0.11",
		"HOST_GUEST_SERVICE_PORT":   "8891",
		"SPOOND_GUEST_DNS_ADDR":     "10.1.0.2",
		"SPOOND_PROXY_HOST_SUFFIX":  ".sandbox.lacy.casa",
		"E2B_GRPC_ADDR":             "127.0.0.1:5008",
		"E2B_PROXY_URL":             "http://127.0.0.1:5007",
		"E2B_TEMPLATE_STORAGE_PATH": t.TempDir(),
		"BIND_ADDR":                 "127.0.0.1:8890",
		"PROXY_ADDR":                "0.0.0.0:8891",
		"SPOOND_DB_PATH":            filepath.Join(t.TempDir(), "spoond.db"),
		"SPOOND_BACKUP_DIR":         t.TempDir(),
		"IMAGE_REGISTRY":            "localhost:5000",
		"E2B_TOKEN_SEED_FILE":       filepath.Join(t.TempDir(), "seed"),
		"PAUSED_RELEASE_DAYS":       "30",
		"PINNED_IDLE_NOTICE_DAYS":   "7",
		"RECOVERY_RETRY_WINDOW":     "30m",
		"UNDRAIN_READY_TIMEOUT":     "180s",
		"UNDRAIN_RESUME_WINDOW":     "5m",
		"SWEEP_TIMEOUT":             "15m",
		"SPOOND_BUILD_TIMEOUT":      "90m",
		"BACKUP_MAX_AGE_SECS":       "93600",
		"KEPT_DISK_WARN_PCT":        "40",
		"DISK_RESERVE_PCT":          "5",
		"SATURATION_IO_FULL_PCT":    "20",
		"SATURATION_CPU_SOME_PCT":   "75",
		"JOB_MAX_RUNTIME":           "24h",
		"DRAIN_RESUME_MAX_AGE":      "24h",
		"GC_LOST_GRACE":             "24h",
		"GC_LOST_GRACE_PERSISTENT":  "168h",
		"SANDBOX_PROBE":             "1",
		"NOTIFY_WEBHOOKS":           `[{"url":"https://hooks.example/T0/B0/x","format":"json"}]`,
		"NOTIFY_UNITS":              "none",
	})
	if code != 1 {
		t.Fatalf("sb-like env: exit %d, want 1 (Main proceeds past validation and dies later); stderr:\n%s", code, stderr)
	}
	if strings.Contains(stderr, "config: ") {
		t.Fatalf("sb-like env refused at validation:\n%s", stderr)
	}
}
