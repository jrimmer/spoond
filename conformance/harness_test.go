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
	cfg.SecondToken = os.Getenv("CONFORMANCE_SECOND_TOKEN")
	// The mixed-allowlist case has no LAN-independent defaults: an
	// unset private address means the case skips (vm2 sets all of these).
	// MixedDomain keeps a public default, which needs no LAN.
	cfg.MixedPrivate = os.Getenv("CONFORMANCE_MIXED_PRIVATE")
	cfg.MixedDomain = envOr("CONFORMANCE_MIXED_DOMAIN", "example.com")
	cfg.MixedBlockedPrivate = os.Getenv("CONFORMANCE_MIXED_BLOCKED_PRIVATE")
	cfg.MixedPrivateDomain = os.Getenv("CONFORMANCE_MIXED_PRIVATE_DOMAIN")
	cfg.MixedPrivatePort = 443
	if p := os.Getenv("CONFORMANCE_MIXED_PRIVATE_PORT"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 || n > 65535 {
			fmt.Fprintf(os.Stderr, "conformance: bad CONFORMANCE_MIXED_PRIVATE_PORT %q\n", p)
			ok = false
		} else {
			cfg.MixedPrivatePort = n
		}
	}
	cfg.MixedSSHPort = 22
	if p := os.Getenv("CONFORMANCE_MIXED_SSH_PORT"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 65535 {
			fmt.Fprintf(os.Stderr, "conformance: bad CONFORMANCE_MIXED_SSH_PORT %q\n", p)
			ok = false
		} else {
			cfg.MixedSSHPort = n
		}
	}
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

// e2bProbeCmd classifies reachability by the data-phase outcome, because
// E2B denies egress in two observable ways: layer-1 denies blackhole the
// SYN (connect hangs to its timeout) and layer-2 port-scoped denies
// connect and then close before any data (immediate EOF; notably,
// `head -c1` exits 0 on that EOF, so a shell probe cannot decide this).
// The probe connects with a 3s timeout, writes one byte (provoking the
// policy decision and any server response) and reads with a 2s timeout:
//   - "blocked": connect timeout/refused or reset, or EOF before any data
//   - "ok": any data byte, or read timeout (silent-open = alive, e.g.
//     scylla 9042 waiting for CQL)
const e2bProbeCmd = `python3 -c '
import socket
s = socket.socket()
s.settimeout(3)
try:
    s.connect(("%s", %d))
except Exception:
    print("blocked")
    raise SystemExit
try:
    s.sendall(b"P")
except Exception:
    print("blocked")
    raise SystemExit
s.settimeout(2)
try:
    d = s.recv(1)
except socket.timeout:
    print("ok")
    raise SystemExit
except Exception:
    print("blocked")
    raise SystemExit
print("ok" if d else "blocked")
'`

// e2bTLSProbeCmd is the port-443 variant: 443 listeners use tcpproxy SNI
// routing, which needs a parseable ClientHello before any allow/deny
// decision — a 1-byte write can never complete one, so a denied 443
// destination sits silent-open and the write+read probe would misread it
// as reachable. This probe performs a real TLS client handshake (certificate
// verification off; the verdict is reachability, not trust):
//   - "ok": the handshake established, OR the peer answered with a TLS
//     alert (an alert is a peer response — bytes flowed bidirectionally
//     through the egress path; SNI-strict servers alert on IP-literal
//     hellos that carry no SNI, e.g. `tlsv1 alert internal error`), and
//     the connection is alive afterwards (any data byte, or silent-open
//     read timeout)
//   - "blocked": connect failure/timeout, or a clean close before any
//     peer bytes (EOF/reset — the egress path closed us)
const e2bTLSProbeCmd = `python3 -c '
import socket, ssl
try:
    s = socket.create_connection(("%s", %d), timeout=3)
except Exception:
    print("blocked")
    raise SystemExit
ctx = ssl.create_default_context()
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE
try:
    s = ctx.wrap_socket(s, server_hostname="%s")
except ssl.SSLError as e:
    if e.reason and "ALERT" in e.reason.upper():
        print("ok")
        raise SystemExit
    print("blocked")
    raise SystemExit
except Exception:
    print("blocked")
    raise SystemExit
s.settimeout(2)
try:
    d = s.recv(1)
except socket.timeout:
    print("ok")
    raise SystemExit
except Exception:
    print("blocked")
    raise SystemExit
print("ok" if d else "blocked")
'`

// probeCmd renders the e2b probe command for host:port. Each template
// receives exactly its own arguments — a surplus argument would be
// appended as `%!(EXTRA ...)` and its parentheses break the guest shell.
func probeCmd(host string, port int) string {
	if port == 443 {
		return fmt.Sprintf(e2bTLSProbeCmd, host, port, host)
	}
	return fmt.Sprintf(e2bProbeCmd, host, port)
}

// canTCP runs the suite's TCP reachability probe in the lease and reports
// whether the destination is reachable:
//
//   - port 443: a real TLS client handshake must establish
//     (e2bTLSProbeCmd above — SNI routing decides on the ClientHello)
//   - other ports: reachable means connect OK AND (data received OR
//     the connection still open after a write); blocked means connect
//     timeout or EOF/reset before any data (e2bProbeCmd above).
func canTCP(t *testing.T, id, host string, port int) bool {
	return probeToken(t, id, probeCmd(host, port)) == "ok"
}

// probeCmdSNI renders the TLS probe with an explicit SNI name,
// independent of the destination port. It covers a domain that resolves to
// an allow-listed private IP: the egress decision matches the domain from
// the ClientHello SNI (the host's port-443 path), so the probe must send
// one even when the destination port is not 443.
func probeCmdSNI(host string, port int, sni string) string {
	return fmt.Sprintf(e2bTLSProbeCmd, host, port, sni)
}

// canTCPWithSNI is canTCP with an explicit SNI name.
func canTCPWithSNI(t *testing.T, id, host string, port int, sni string) bool {
	return probeToken(t, id, probeCmdSNI(host, port, sni)) == "ok"
}

// probeToken runs a probe cmd in the lease whose stdout is exactly "ok" or
// "blocked". Anything else — other output, a non-zero exit, a transport or
// HTTP-level failure — is a harness error, not a reachability verdict.
func probeToken(t *testing.T, id, cmd string) string {
	st, body, err := cl.exec(id, execReq{Cmd: cmd, Timeout: 15})
	if err != nil {
		failf(t, "exec %s: %v", id, err)
	}
	if st != 200 {
		failf(t, "exec %s: status %d: %s", id, st, truncate(body))
	}
	var out execResult
	if err := json.Unmarshal(body, &out); err != nil {
		failf(t, "exec %s: bad body: %v", id, err)
	}
	if out.Exit != 0 {
		failf(t, "probe harness error in %s: exit %d: stderr: %s", id, out.Exit, strings.TrimSpace(out.Stderr))
	}
	token := strings.TrimSpace(out.Stdout)
	if token != "ok" && token != "blocked" {
		failf(t, "probe harness error in %s: stdout %q, want ok|blocked", id, token)
	}
	return token
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
