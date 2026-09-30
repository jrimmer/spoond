//go:build conformance

package conformance

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testRec is one entry of the results JSON (U02 §Harness requirements).
type testRec struct {
	Test       string         `json:"test"`
	Status     string         `json:"status"`
	DurationMS int64          `json:"duration_ms"`
	Metrics    map[string]any `json:"metrics"`
	Error      string         `json:"error"`
}

var (
	cfg         config
	cl          *client
	resultsPath string
	runStart    time.Time
	results     []*testRec
	cur         *testRec // the running test's record (tests run sequentially)
)

func init() {
	ts := time.Now().UTC().Format("20060102T150405")
	flag.String("results", "results/"+ts+"-"+envOr("CONFORMANCE_SUBSTRATE", "unknown")+".json",
		"results JSON path (absolute when run via `go test -args`; TestMain creates the parent directory)")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// loadConfig reads every configuration variable. Required variables must
// be present, even when the spec allows them to be empty
// (CONFORMANCE_PROXY_SECRET).
func loadConfig() bool {
	req := []struct {
		key string
		dst *string
	}{
		{"CONFORMANCE_API", &cfg.API},
		{"CONFORMANCE_TOKEN", &cfg.Token},
		{"CONFORMANCE_SSH", &cfg.SSH},
		{"CONFORMANCE_SUBSTRATE", &cfg.Substrate},
		{"CONFORMANCE_BACKEND_UNIT", &cfg.BackendUnit},
		{"CONFORMANCE_SSH_GATEWAY", &cfg.SSHGateway},
		{"CONFORMANCE_SSH_KEY", &cfg.SSHKey},
		{"CONFORMANCE_USER", &cfg.User},
		{"CONFORMANCE_USER_ID", &cfg.UserID},
		{"CONFORMANCE_PROXY_URL", &cfg.ProxyURL},
		{"CONFORMANCE_PROXY_SECRET", &cfg.ProxySecret},
		{"CONFORMANCE_PROXY_SUFFIX", &cfg.ProxySuffix},
		{"CONFORMANCE_GUEST_SERVICE", &cfg.GuestService},
	}
	ok := true
	for _, r := range req {
		v, present := os.LookupEnv(r.key)
		if !present {
			fmt.Fprintf(os.Stderr, "conformance: missing required env %s\n", r.key)
			ok = false
			continue
		}
		*r.dst = v
	}
	if !ok {
		return false
	}
	cfg.Destructive = os.Getenv("CONFORMANCE_DESTRUCTIVE") == "1"
	images := envOr("CONFORMANCE_IMAGES", "py-base,go-base,dev-base,elixir-base,elixir-release,llm-review,scylla")
	for _, name := range strings.Split(images, ",") {
		if name = strings.TrimSpace(name); name != "" {
			cfg.Images = append(cfg.Images, name)
		}
	}
	return true
}

// TestMain loads the configuration, runs the suite and writes the results
// JSON file named by -results.
func TestMain(m *testing.M) {
	flag.Parse()
	runStart = time.Now()
	if f := flag.Lookup("results"); f != nil {
		resultsPath = f.Value.String()
	}
	if !loadConfig() {
		os.Exit(2)
	}
	cl = newClient(cfg)
	code := m.Run()
	writeResults(time.Since(runStart))
	os.Exit(code)
}

// writeResults writes the results file: an array of test records, led by a
// "_run" record carrying the run-level facts (the conformance user id is
// recorded here).
func writeResults(dur time.Duration) {
	if resultsPath == "" {
		return
	}
	if dir := filepath.Dir(resultsPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "conformance: create results dir: %v\n", err)
			return
		}
	}
	meta := &testRec{
		Test:       "_run",
		Status:     "pass",
		DurationMS: dur.Milliseconds(),
		Metrics: map[string]any{
			"api":          cfg.API,
			"substrate":    cfg.Substrate,
			"backend_unit": cfg.BackendUnit,
			"user":         cfg.User,
			"user_id":      cfg.UserID,
			"started":      runStart.UTC().Format(time.RFC3339),
		},
	}
	recs := append([]*testRec{meta}, results...)
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "conformance: marshal results: %v\n", err)
		return
	}
	if err := os.WriteFile(resultsPath, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "conformance: write results: %v\n", err)
	}
}

// begin registers the test's results record. It must be the first call of
// every test.
func begin(t *testing.T) *testRec {
	rec := &testRec{Test: t.Name(), Metrics: map[string]any{}}
	start := time.Now()
	results = append(results, rec)
	cur = rec
	t.Cleanup(func() {
		rec.DurationMS = time.Since(start).Milliseconds()
		switch {
		case t.Failed():
			rec.Status = "fail"
		case t.Skipped():
			rec.Status = "skip"
		default:
			rec.Status = "pass"
		}
		if rec.Error == "" && t.Failed() {
			rec.Error = "test failed"
		}
		cur = nil
	})
	return rec
}

func (r *testRec) set(metric string, v any) {
	r.Metrics[metric] = v
}

// failf records the message in the results and fails the test.
func failf(t *testing.T, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if cur != nil && cur.Error == "" {
		cur.Error = msg
	}
	t.Fatalf("%s", msg)
}

// skipf skips the test (recorded as "skip" in the results).
func skipf(t *testing.T, format string, args ...any) {
	t.Skipf(format, args...)
}

// hostRun runs a host-level check: `sh -c <script>` locally when
// CONFORMANCE_SSH=local, otherwise `ssh <CONFORMANCE_SSH> <script>`. It
// fails the test on a non-zero exit and returns trimmed stdout.
func hostRun(t *testing.T, script string) string {
	var stdout, stderr bytes.Buffer
	var cmd *exec.Cmd
	if cfg.SSH == "local" {
		cmd = exec.Command("sh", "-c", script)
	} else {
		cmd = exec.Command("ssh", cfg.SSH, script)
	}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		failf(t, "hostRun %q: %v (stderr: %s)", script, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String())
}

// execOK runs cmd in the lease and fails the test unless the exec answers
// 200 with exit == 0. It returns trimmed stdout.
func execOK(t *testing.T, id, cmd string) string {
	st, body, err := cl.exec(id, execReq{Cmd: cmd})
	if err != nil {
		failf(t, "exec %s %q: %v", id, cmd, err)
	}
	if st != 200 {
		failf(t, "exec %s %q: status %d: %s", id, cmd, st, truncate(body))
	}
	var out execResult
	if err := json.Unmarshal(body, &out); err != nil {
		failf(t, "exec %s %q: bad body: %v", id, cmd, err)
	}
	if out.Exit != 0 {
		failf(t, "exec %s %q: exit %d: stderr: %s", id, cmd, out.Exit, strings.TrimSpace(out.Stderr))
	}
	return strings.TrimSpace(out.Stdout)
}

// execAlive is a non-fatal exec probe used by group R polling.
func execAlive(id string) bool {
	st, body, err := cl.exec(id, execReq{Cmd: "echo alive"})
	if err != nil || st != 200 {
		return false
	}
	var out execResult
	if err := json.Unmarshal(body, &out); err != nil {
		return false
	}
	return out.Exit == 0
}

// randMarker returns a random 12-hex string for marker values.
func randMarker() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// canTCP runs the suite's TCP reachability probe in the lease and reports
// whether the destination is reachable. The probe is substrate-aware:
//
//   - forkd denies at SYN (netns firewall), so a plain connect probe
//     decides: timeout 5 bash -c '</dev/tcp/HOST/PORT' && echo yes || echo no
//   - e2b enforces egress with a transparent TCP proxy: the guest's
//     connect() always succeeds (the local proxy accepts) and a denied
//     destination closes at the data phase. Reachability there means the
//     connection supports the data phase, probed with
//     timeout 3 bash -c 'exec 3<>/dev/tcp/HOST/PORT && head -c 1 <&3'
//     where exit 0 (a byte arrived) or 124 (timeout — connection still
//     open, silent server) is reachable, and any other exit (1/2 — EOF or
//     reset, i.e. the proxy closed us) is blocked.
//
// Allowed-but-silent services (e.g. scylla 9042 waiting for CQL) hit the
// timeout path and count as reachable, which is correct.
func canTCP(t *testing.T, id, host string, port int) bool {
	if cfg.Substrate == "e2b" {
		code := probeExit(t, id, fmt.Sprintf("timeout 3 bash -c 'exec 3<>/dev/tcp/%s/%d && head -c 1 <&3'", host, port))
		return code == 0 || code == 124
	}
	out := execOK(t, id, fmt.Sprintf("timeout 5 bash -c '</dev/tcp/%s/%d' && echo yes || echo no", host, port))
	return out == "yes"
}

// probeExit runs cmd in the lease and returns its exit code, tolerating
// non-zero exits (probes whose meaning is the exit code). Transport and
// HTTP-level failures still fail the test.
func probeExit(t *testing.T, id, cmd string) int {
	st, body, err := cl.exec(id, execReq{Cmd: cmd, Timeout: 10})
	if err != nil {
		failf(t, "exec %s %q: %v", id, cmd, err)
	}
	if st != 200 {
		failf(t, "exec %s %q: status %d: %s", id, cmd, st, truncate(body))
	}
	var out execResult
	if err := json.Unmarshal(body, &out); err != nil {
		failf(t, "exec %s %q: bad body: %v", id, cmd, err)
	}
	return out.Exit
}

// canTCPAddr is canTCP for a host:port address (CONFORMANCE_GUEST_SERVICE).
func canTCPAddr(t *testing.T, id, addr string) bool {
	host, port, err := splitHostPort(addr)
	if err != nil {
		failf(t, "canTCP: %v", err)
	}
	return canTCP(t, id, host, port)
}

func splitHostPort(addr string) (string, int, error) {
	i := strings.LastIndexByte(addr, ':')
	if i <= 0 {
		return "", 0, fmt.Errorf("no port in %q", addr)
	}
	port, err := strconv.Atoi(addr[i+1:])
	if err != nil {
		return "", 0, fmt.Errorf("bad port in %q: %v", addr, err)
	}
	return addr[:i], port, nil
}

// track registers a lease for deletion in t.Cleanup. 404 on delete is
// ignored (U02 §Harness requirements).
func track(t *testing.T, id string) {
	t.Cleanup(func() {
		st, _, err := cl.delete(id)
		if err != nil {
			t.Logf("cleanup: delete %s: %v", id, err)
			return
		}
		if st != 204 && st != 404 {
			t.Logf("cleanup: delete %s: status %d", id, st)
		}
	})
}

// createLease creates a lease, fails the test on any error and registers
// it for cleanup. It returns the decoded lease.
func createLease(t *testing.T, body map[string]any) leaseInfo {
	st, data, err := cl.create(body)
	if err != nil {
		failf(t, "create %v: %v", body, err)
	}
	if st != 201 {
		failf(t, "create %v: status %d: %s", body, st, truncate(data))
	}
	var l leaseInfo
	if err := json.Unmarshal(data, &l); err != nil {
		failf(t, "create %v: bad body: %v", body, err)
	}
	if l.ID == "" {
		failf(t, "create %v: empty id in body %s", body, truncate(data))
	}
	track(t, l.ID)
	return l
}

// readCtr reads the group S counter file, tolerating the momentary empty
// read of `echo $i > /tmp/ctr`.
func readCtr(t *testing.T, id string) int {
	for range 20 {
		out := execOK(t, id, "cat /tmp/ctr 2>/dev/null || true")
		if n, err := strconv.Atoi(strings.TrimSpace(out)); err == nil {
			return n
		}
		time.Sleep(100 * time.Millisecond)
	}
	failf(t, "%s: counter file /tmp/ctr never readable", id)
	return 0
}

// pct returns the nearest-rank percentile of a sorted slice.
func pct(sorted []int64, p float64) int64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	idx := int(math.Ceil(p/100*float64(n))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return sorted[idx]
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

var threeDigits = regexp.MustCompile(`^[0-9]{3}$`)

// sortedCopy is a tiny helper so call sites read clearly.
func sortedCopy(v []int64) []int64 {
	out := append([]int64(nil), v...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
