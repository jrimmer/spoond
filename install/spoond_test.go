// Tests for the spoond phase (spoond*.go): a temp Root, a fake runner
// answering the two queries (`hostname -f`, `ip -4 -o route get`) and
// recording every exec, and a fake HTTP fetcher capturing the health
// GETs and the bootstrap POST. Every item's row, mode and content
// contract is pinned, the idempotent second run, the existing
// backend.env's token propagation, --container --config-only, the dry
// run, the store-dir substitution, the TLS pair and the first user.
package install

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"golang.org/x/crypto/bcrypt"
)

// reply is a runner answer: stdout bytes and an error.
type reply = fakeReply

// spoondRunner is the fake runner: hostname -f and the ip route query
// are queries that run in a dry run too, so they are answered from
// here; everything else is recorded (a changing command that arrives
// is a test failure unless the table says otherwise).
type spoondRunner struct {
	hostname string
	ip       string
	// calls records every invocation, queries included.
	calls []string
	// extra answers one-off queries (systemctl is-active) on top of
	// the defaults.
	extra map[string]reply
}

func (s *spoondRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	full := strings.Join(append([]string{name}, args...), " ")
	s.calls = append(s.calls, full)
	switch {
	case full == "hostname -f":
		return []byte(s.hostname + "\n"), nil
	case full == "ip -4 -o route get 1.1.1.1":
		if s.ip == "" {
			return nil, fmt.Errorf("rt_cache: no route to host")
		}
		return []byte(fmt.Sprintf("1.1.1.1 from 203.0.113.9 via 203.0.113.1 dev eth0 src %s uid 0\n", s.ip)), nil
	}
	if s.extra != nil {
		if rep, ok := s.extra[full]; ok {
			return rep.out, rep.err
		}
	}
	if name == "systemctl" && !strings.HasPrefix(full, "systemctl is-active") {
		// systemctl daemon-reload/enable/restart succeed and print
		// nothing; is-active answers from extra (its non-zero exit
		// means "not running").
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected command %s", full)
}

// The `ip -4 -o route get 1.1.1.1` line the real ip prints, with a src.
const routeLine = "1.1.1.1 from 203.0.113.9 via 203.0.113.1 dev eth0 src 10.0.0.11 uid 0\n"

// spoondFetch is the fake HTTP fetcher: it records the requests and
// answers from a script. The tokens ride its captured requests (the
// test asserts on them there); they never reach Out.
type spoondFetch struct {
	requests  []*http.Request
	bodies    []string
	healthGet int
	// getCodes are the /healthz answers in order; the last repeats
	// when the list runs out.
	getCodes []int
	// postCode is the user POST's answer.
	postCode int
}

// codesAt returns the GET answer for try n.
func (f *spoondFetch) Get(r *http.Request) (int, error) {
	f.healthGet++
	f.requests = append(f.requests, r)
	i := f.healthGet - 1
	if i >= len(f.getCodes) {
		i = len(f.getCodes) - 1
	}
	if f.getCodes[i] == 0 {
		return 0, fmt.Errorf("get failed")
	}
	return f.getCodes[i], nil
}

func (f *spoondFetch) Post(r *http.Request) (int, error) {
	f.requests = append(f.requests, r)
	body, _ := readBody(r)
	f.bodies = append(f.bodies, body)
	return f.postCode, nil
}

// readBody re-reads a request's body as text (the fake builds the
// request with a bytes reader, so draining it is safe).
func readBody(r *http.Request) (string, error) {
	b, err := ioAll(r.Body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ioAll reads the whole reader, EOF-tolerant (a nil GET body reads no
// bytes).
func ioAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	var out []byte
	buf := make([]byte, 512)
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			if isEOF(err) {
				return out, nil
			}
			return out, err
		}
	}
}

// isEOF reports whether err is the end of the reader.
func isEOF(err error) bool {
	return err.Error() == "EOF" || err.Error() == "http: read on closed response body"
}

// spoondTestEnv builds an Env over a temp root for the spoond phase:
// the fake runner answers the two queries, the fake fetcher answers
// 200 on the first /healthz try and 201 on the user POST, and the seed
// file exists (the runtime phase's output).
func spoondTestEnv(t *testing.T) (*Env, *bytes.Buffer, *spoondRunner, *spoondFetch) {
	t.Helper()
	out := &bytes.Buffer{}
	run := &spoondRunner{
		hostname: "sb.example.com",
		ip:       "10.0.0.11",
		// A host the phase is installing is running the backend
		// (the default answer; active-unit tests override it).
		extra: map[string]reply{
			"systemctl is-active --quiet spoond-backend": {out: []byte("active\n")},
		},
	}
	fetch := &spoondFetch{getCodes: []int{http.StatusOK}, postCode: http.StatusCreated}
	env := &Env{
		Root: t.TempDir(),
		Run:  run,
		Out:  out,
		Opt: Options{
			HostName: "sb.example.com",
			AdminKey: adminKeyPath(t),
		},
	}
	spoondFakeExec(t, env)
	writeSeed(t, env)
	restoreFetcher(t, fetch)
	return env, out, run, fetch
}

// adminKeyPath writes a real ed25519 public key (an authorized_keys
// line ssh.ParseAuthorizedKey accepts) and returns its path.
func adminKeyPath(t *testing.T) string {
	t.Helper()
	key, err := adminKeyGenerate()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519.pub")
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeSeed creates the token seed file the runtime phase installs.
func writeSeed(t *testing.T, env *Env) {
	t.Helper()
	full := env.Resolve(e2bTokenSeedPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(strings.Repeat("ab", 32)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// restoreFetcher sets the fetcher hook and clears it after the test.
func restoreFetcher(t *testing.T, f *spoondFetch) {
	t.Helper()
	HealthFetcher = f
	t.Cleanup(func() { HealthFetcher = nil })
}

// fakeExec is one binary-copy test double: the fake executable's path,
// its bytes, and record of copies.
type fakeExec struct {
	path  string
	bytes []byte
}

// spoondFakeExec installs the binary-copy hooks for one env: a fake
// executable file under the temp dir (so a run copies real bytes to
// Root/opt/spoond/spoond).
func spoondFakeExec(t *testing.T, env *Env) {
	t.Helper()
	dir := filepath.Join(env.Root, "fake-bin-home")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "spoond")
	if err := os.WriteFile(src, []byte("fake-spoond-binary\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldExec, oldCopy := execPath, copyExec
	execPath = func() (string, error) { return src, nil }
	copyExec = func(dst string) (bool, error) { return spoondCopyFile(src, dst, modeBin) }
	t.Cleanup(func() { execPath, copyExec = oldExec, oldCopy })
}

// setHealthWait swaps the health wait for the test and restores it.
func setHealthWait(t *testing.T, d time.Duration) {
	t.Helper()
	waitOverride = d
	t.Cleanup(func() { waitOverride = 0 })
}

// runSpoond runs the real phase and returns the rows, the output and
// the error.
func runSpoond(t *testing.T, env *Env) ([]Item, string, error) {
	t.Helper()
	items, err := RunPhase(context.Background(), env, PhaseSpoond, spoondPhase)
	return items, env.Out.(*bytes.Buffer).String(), err
}

// rowsByName indexes the report by row name (a name repeats: the
// directory rows and file rows share names only by design error) — the
// tests mostly scan the list, this keeps single-row lookups short.
func rowsByName(t *testing.T, items []Item) map[string][]Item {
	t.Helper()
	m := map[string][]Item{}
	for _, it := range items {
		m[it.Name] = append(m[it.Name], it)
	}
	return m
}

// resultOf returns the one row's result, failing when the name has
// not exactly one row.
func resultOf(t *testing.T, items []Item, name string) (Item, bool) {
	t.Helper()
	var found []Item
	for _, it := range items {
		if it.Name == name {
			found = append(found, it)
		}
	}
	if len(found) != 1 {
		return Item{}, false
	}
	return found[0], true
}

// envVal reads one KEY from a written env file under the root.
func envVal(t *testing.T, env *Env, path, key string) string {
	t.Helper()
	vals, err := parseEnvFile(env.Resolve(path))
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return vals[key]
}

// isHex reports whether s is all lowercase hex of an even length.
func isHex(s string) bool {
	if len(s) == 0 || len(s)%2 != 0 {
		return false
	}
	for _, r := range s {
		if !unicode.In(r, unicode.ASCII_Hex_Digit) || (unicode.IsUpper(r)) {
			return false
		}
	}
	return true
}

// The full happy path: every file exists with its mode and content,
// the tokens agree across the files, and the generated secrets are
// hex strings of the right length.
func TestSpoondFirstRun(t *testing.T) {
	env, out, run, fetch := spoondTestEnv(t)
	items, text, err := runSpoond(t, env)
	if err != nil {
		t.Fatalf("first run: %v\n%s", err, text)
	}
	_ = items

	// Every config file written, with the right modes.
	for _, tc := range []struct {
		path string
		mode os.FileMode
	}{
		{backendEnvPath, 0o600},
		{adminTokenPath, 0o600},
		{drainEnvPath, 0o600},
		{gatewayEnvPath, 0o600},
		{dashEnvPath, 0o600},
		{dashPasswordPath, 0o600},
		{tlsCertPath, 0o644},
		{tlsKeyPath, 0o600},
	} {
		if _, mode := read(t, env, tc.path); mode != tc.mode {
			t.Errorf("%s mode = %#o, want %#o", tc.path, mode, tc.mode)
		}
	}
	for _, dir := range spoondDirs {
		fi, err := os.Stat(env.Resolve(dir))
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s = dir %v mode %#o, want a dir at 0700", dir, fi.IsDir(), fi.Mode().Perm())
		}
	}

	// backend.env carries every key of docs/install.md section 4 and
	// hex secrets of the right lengths. SPOOND_GUEST_DNS_ADDR is
	// checked apart: no resolv.conf here means it is legitimately
	// empty.
	for _, key := range []string{
		"CONSUMER_TOKENS", "SPOOND_DB_PATH", "E2B_GRPC_ADDR", "E2B_PROXY_URL",
		"E2B_TOKEN_SEED_FILE", "IMAGE_REGISTRY", "HOST_GUEST_SERVICE_ADDR",
		"HOST_GUEST_SERVICE_PORT", "SPOOND_PROXY_HOST_SUFFIX",
		"E2B_TEMPLATE_STORAGE_PATH", "SPOOND_BACKUP_DIR", "USERS_FILE",
		"BOOTSTRAP_TOKEN", "GATEWAY_TOKEN", "ADMIN_TOKEN", "METRICS_TOKEN",
		"EVENTS_TOKEN", "TLS_CERT", "TLS_KEY", "BIND_ADDR", "PROXY_ADDR",
	} {
		if envVal(t, env, backendEnvPath, key) == "" {
			t.Errorf("%s is missing %s", backendEnvPath, key)
		}
	}
	consumer := envVal(t, env, backendEnvPath, "CONSUMER_TOKENS")
	if !strings.HasSuffix(consumer, "=local") {
		t.Errorf("CONSUMER_TOKENS = %q, want ...=local", consumer)
	}
	tok := strings.TrimSuffix(consumer, "=local")
	if len(tok) != 48 || !isHex(tok) {
		t.Errorf("consumer secret = %d chars, want 48 lowercase hex", len(tok))
	}
	for key, want := range map[string]int{
		"BOOTSTRAP_TOKEN": 48,
		"GATEWAY_TOKEN":   64,
		"ADMIN_TOKEN":     64,
		"METRICS_TOKEN":   64,
		"EVENTS_TOKEN":    64,
	} {
		got := envVal(t, env, backendEnvPath, key)
		if len(got) != want || !isHex(got) {
			t.Errorf("%s = %d chars, want %d lowercase hex", key, len(got), want)
		}
	}

	// The derived files carry the same tokens: the gateway env and
	// admin-token and dash.env all agree with backend.env.
	if got, want := envVal(t, env, gatewayEnvPath, "SPOOND_GATEWAY_TOKEN"), envVal(t, env, backendEnvPath, "GATEWAY_TOKEN"); got != want {
		t.Errorf("gateway env token != backend.env's GATEWAY_TOKEN")
	}
	adminBytes, adminMode := read(t, env, adminTokenPath)
	if want := envVal(t, env, backendEnvPath, "ADMIN_TOKEN"); string(adminBytes) != want {
		t.Errorf("admin-token %q != ADMIN_TOKEN %q", adminBytes, want)
	} else if adminMode != 0o600 {
		t.Errorf("admin-token mode = %#o, want 0600", adminMode)
	}
	if got, want := envVal(t, env, dashEnvPath, "METRICS_TOKEN"), envVal(t, env, backendEnvPath, "METRICS_TOKEN"); got != want {
		t.Errorf("dash.env METRICS_TOKEN != backend.env's")
	}
	if got, want := envVal(t, env, dashEnvPath, "DASH_EVENTS_TOKEN"), envVal(t, env, backendEnvPath, "EVENTS_TOKEN"); got != want {
		t.Errorf("dash.env DASH_EVENTS_TOKEN != backend.env's EVENTS_TOKEN")
	}
	pwBytes, pwMode := read(t, env, dashPasswordPath)
	if pwMode != 0o600 {
		t.Errorf("dash-password mode = %#o, want 0600", pwMode)
	}
	if hash := envVal(t, env, dashEnvPath, "DASH_PASSWORD_HASH"); len(string(pwBytes)) != 20 || !verifyBcrypt(string(pwBytes), hash) {
		t.Errorf("DASH_PASSWORD_HASH does not verify the 20-character password (len %d)", len(pwBytes))
	}
	// The drain env names its three keys; the gateway env the four.
	for _, kv := range [][2]string{
		{drainEnvPath, "SPOOND_DRAIN_URL"},
		{drainEnvPath, "SPOOND_ADMIN_TOKEN_FILE"},
		{drainEnvPath, "SPOOND_DRAIN_INSECURE"},
		{gatewayEnvPath, "SPOOND_GATEWAY_HOST"},
		{gatewayEnvPath, "HOST_GUEST_SERVICE_ADDR"},
		{gatewayEnvPath, "HOST_GUEST_SERVICE_PORT"},
	} {
		if envVal(t, env, kv[0], kv[1]) == "" {
			t.Errorf("%s is missing %s", kv[0], kv[1])
		}
	}
	if got, want := envVal(t, env, gatewayEnvPath, "SPOOND_GATEWAY_HOST"), "sb.example.com"; got != want {
		t.Errorf("SPOOND_GATEWAY_HOST = %q, want the host name", got)
	}

	// backend.env's rendered shape: the store dir and the host
	// specifics.
	if got, want := envVal(t, env, backendEnvPath, "E2B_TEMPLATE_STORAGE_PATH"), "/forkdcache/e2b/storage/templates"; got != want {
		t.Errorf("E2B_TEMPLATE_STORAGE_PATH = %q, want %q", got, want)
	}
	if got, want := envVal(t, env, backendEnvPath, "HOST_GUEST_SERVICE_ADDR"), "10.0.0.11"; got != want {
		t.Errorf("HOST_GUEST_SERVICE_ADDR = %q, want the default route's src", got)
	}
	if got, want := envVal(t, env, backendEnvPath, "SPOOND_PROXY_HOST_SUFFIX"), ".sb.example.com"; got != want {
		t.Errorf("SPOOND_PROXY_HOST_SUFFIX = %q, want .sb.example.com", got)
	}
	if got := envVal(t, env, backendEnvPath, "SPOOND_GUEST_DNS_ADDR"); got != "" {
		// resolv.conf was not written in this env: no resolver
		// allowance is expected.
		t.Errorf("SPOOND_GUEST_DNS_ADDR = %q without a resolv.conf; want empty", got)
	}

	// The units were written (0644) with the store dir substituted —
	// the shipped e2b-orchestrator.service is the one that carries
	// /forkdcache/e2b; the spoond units carry no store path.
	for _, u := range spoondUnits {
		b, mode := read(t, env, u.path)
		if mode != 0o644 {
			t.Errorf("%s mode = %#o, want 0644", u.path, mode)
		}
		if !strings.Contains(string(b), "[Unit]") {
			t.Errorf("%s does not look like a unit: %q", u.path, firstLine(string(b)))
		}
	}
	orch, _ := read(t, env, orchestratorUnitPath)
	if !strings.Contains(string(orch), DefaultStoreDir+"/tmp") {
		t.Errorf("the orchestrator unit does not carry the store dir: %q", firstLine(string(orch)))
	}

	// The changing commands of a first run.
	joined := strings.Join(run.calls, "\n")
	if !strings.Contains(joined, "systemctl daemon-reload") {
		t.Errorf("no daemon-reload in %v", run.calls)
	}
	if !strings.Contains(joined, "systemctl enable --now spoond-backend spoond-sshd-gateway spoond-dash spoond-drain") {
		t.Errorf("no enable --now of the four units in %v", run.calls)
	}
	// One restart: this run was the first (a fresh root), but a
	// first run counts as a run when its env file and binary both
	// changed; the guard asked is-active and the unit's start stamp.
	// No restart happened: enable --now started the unit seconds ago
	// (at the horizon guard), and restarting this run's own start
	// would start a fresh install's backend twice.
	if got := countSubstring(joined, "systemctl restart spoond-backend"); got != 0 {
		t.Errorf("restart spoond-backend = %d, want 0 (enable --now started it this run)", got)
	}
	if got := countSubstring(joined, "systemctl is-active"); got != 1 {
		t.Errorf("is-active asked %d times, want 1 (the restart's guard)", got)
	}
	if got := countSubstring(joined, "systemctl show -p ActiveEnterTimestampMonotonic"); got != 1 {
		t.Errorf("the unit's start stamp read %d times, want 1", got)
	}

	// One health GET (200 at once) and one user POST (201).
	if fetch.healthGet != 1 {
		t.Errorf("health GETs = %d, want 1", fetch.healthGet)
	}
	if len(fetch.bodies) != 1 {
		t.Fatalf("user POSTs = %d, want 1", len(fetch.bodies))
	}
	if !strings.Contains(out.String(), "first admin user") {
		t.Errorf("the user row is missing:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "spoond-backend is healthy") {
		t.Errorf("the health row is missing:\n%s", out.String())
	}
}

// firstLine is the string's first line.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// countSubstring counts the non-overlapping occurrences of needle.
func countSubstring(haystack, needle string) int {
	return strings.Count(haystack, needle)
}

// verifyBcrypt says whether pw verifies against the bcrypt hash.
func verifyBcrypt(pw, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// jsonUnmarshal decodes JSON into v.
func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

func TestSpoondSecondRunKeepsEverything(t *testing.T) {
	env, _, run, fetch := spoondTestEnv(t)
	if _, _, err := runSpoond(t, env); err != nil {
		t.Fatalf("first run: %v", err)
	}
	before := map[string][]byte{}
	for _, p := range []string{backendEnvPath, adminTokenPath, drainEnvPath, gatewayEnvPath, dashEnvPath, dashPasswordPath, tlsCertPath, tlsKeyPath} {
		b, _ := read(t, env, p)
		before[p] = b
	}

	run.calls = nil // only the second run's commands are asserted
	env.Out.(*bytes.Buffer).Reset()
	fetch.healthGet, fetch.bodies = 0, nil
	fetch.postCode = http.StatusForbidden // the bootstrap window is closed after the first user
	items, text, err := runSpoond(t, env)
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, text)
	}
	for _, p := range []string{backendEnvPath, adminTokenPath, drainEnvPath, gatewayEnvPath, dashEnvPath, dashPasswordPath, tlsCertPath, tlsKeyPath} {
		if b, _ := read(t, env, p); !bytes.Equal(b, before[p]) {
			t.Errorf("%s changed on the second run", p)
		}
	}
	// No changing command; only the restart's is-active query.
	for _, call := range run.calls {
		if strings.Contains(call, "daemon-reload") || strings.Contains(call, "restart") {
			t.Errorf("second run ran a changing command: %s", call)
		}
	}
	if got := countSubstring(strings.Join(run.calls, "\n"), "systemctl enable"); got != 1 {
		t.Errorf("second run: enable --now should still run (idempotent), got %d", got)
	}
	if fetch.healthGet != 1 {
		t.Errorf("second run: 1 health GET, got %d", fetch.healthGet)
	}
	// The second run's POST (users.json is the backend's, not the
	// installer's) answers 403 — the bootstrap window closed — and
	// the phase continues with a skipped row, not an error.
	if len(fetch.bodies) > 1 {
		t.Errorf("second run: more than one POST")
	}
	// The report: no created row for a config or secret file, no new
	// secret. All the env rows are kept.
	for _, it := range items {
		switch it.Name {
		case backendEnvPath, adminTokenPath, drainEnvPath, gatewayEnvPath, dashEnvPath, dashPasswordPath, tlsKeyPath:
			if it.Result != Kept {
				t.Errorf("%s row = %s on the second run, want kept", it.Name, it.Result)
			}
		}
	}
}

// TestSpoondExistingBackendEnv: only backend.env exists — the derived
// files take its tokens and nothing is regenerated.
func TestSpoondExistingBackendEnv(t *testing.T) {
	env, _, _, fetch := spoondTestEnv(t)
	const (
		consumer  = "aaaa=local"
		bootstrap = "bbbb"
		gateway   = "cccc"
		admin     = "dddd"
		events    = "eeee"
		metrics   = "ffff"
	)
	writeTestBackendEnv(t, env, map[string]string{
		"CONSUMER_TOKENS": consumer,
		"BOOTSTRAP_TOKEN": bootstrap,
		"GATEWAY_TOKEN":   gateway,
		"ADMIN_TOKEN":     admin,
		"EVENTS_TOKEN":    events,
	})
	items, text, err := runSpoond(t, env)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, text)
	}
	if envVal(t, env, backendEnvPath, "CONSUMER_TOKENS") != consumer {
		t.Errorf("backend.env was touched: CONSUMER_TOKENS = %q", envVal(t, env, backendEnvPath, "CONSUMER_TOKENS"))
	}
	// The derived files carry its tokens.
	if got, want := envVal(t, env, gatewayEnvPath, "SPOOND_GATEWAY_TOKEN"), gateway; got != want {
		t.Errorf("gateway token = %q, want backend.env's %q", got, want)
	}
	if got, _ := read(t, env, adminTokenPath); string(got) != admin {
		t.Errorf("admin-token = %q, want backend.env's ADMIN_TOKEN %q", got, admin)
	}
	if got, want := envVal(t, env, dashEnvPath, "DASH_EVENTS_TOKEN"), events; got != want {
		t.Errorf("DASH_EVENTS_TOKEN = %q, want backend.env's %q", got, want)
	}
	// The fresh dash.env's metrics token is 64 hex chars.
	if got := envVal(t, env, dashEnvPath, "METRICS_TOKEN"); len(got) != 64 || !isHex(got) {
		t.Errorf("dash METRICS_TOKEN = %d chars, want 64 lowercase hex", len(got))
	}
	// The POST's Authorization carries the bare first consumer token
	// (the docs' ${CONSUMER_TOKENS%%=*}), not the whole
	// token=consumer value; the bootstrap header the file's token.
	if len(fetch.bodies) != 1 {
		t.Fatalf("POSTs = %d, want 1", len(fetch.bodies))
	}
	req := fetch.requests[len(fetch.requests)-1]
	if got, want := req.Header.Get("Authorization"), "Bearer aaaa"; got != want {
		t.Errorf("Authorization = %q, want the bare consumer token %q", got, want)
	}
	if got := req.Header.Get("X-Bootstrap-Token"); got != bootstrap {
		t.Errorf("X-Bootstrap-Token = %q, want backend.env's %q", got, bootstrap)
	}
	_ = items
	// The backend.env row says kept.
	row, ok := resultOf(t, items, backendEnvPath)
	if !ok || row.Result != Kept {
		t.Errorf("backend.env row = %+v, want kept", row)
	}
}

// writeTestBackendEnv writes a backend.env with the given keys (plus a
// handful of the standard keys, so the parse has a realistic file).
func writeTestBackendEnv(t *testing.T, env *Env, extra map[string]string) {
	t.Helper()
	m := map[string]string{
		"SPOOND_DB_PATH": "/var/lib/spoond/spoond.db",
		"BIND_ADDR":      "0.0.0.0:8890",
	}
	for k, v := range extra {
		m[k] = v
	}
	full := env.Resolve(backendEnvPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, renderEnvFile(m), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSpoondNoSecretInOutput: every generated token, the dashboard
// password and the bcrypt hash stay out of the report and out of the
// returned error text.
func TestSpoondNoSecretInOutput(t *testing.T) {
	env, out, _, _ := spoondTestEnv(t)
	items, _, err := runSpoond(t, env)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// Collect every secret value the files carry and scan for it.
	values := []string{}
	vals, _ := parseEnvFile(env.Resolve(backendEnvPath))
	values = append(values, tokenOf(vals["CONSUMER_TOKENS"]), vals["BOOTSTRAP_TOKEN"], vals["GATEWAY_TOKEN"], vals["ADMIN_TOKEN"], vals["EVENTS_TOKEN"])
	gateVals, _ := parseEnvFile(env.Resolve(gatewayEnvPath))
	values = append(values, gateVals["SPOOND_GATEWAY_TOKEN"])
	dashVals, _ := parseEnvFile(env.Resolve(dashEnvPath))
	values = append(values, dashVals["METRICS_TOKEN"], dashVals["DASH_EVENTS_TOKEN"], dashVals["DASH_PASSWORD_HASH"])
	if b, _ := read(t, env, dashPasswordPath); len(b) > 0 {
		values = append(values, string(b))
	}
	if b, _ := read(t, env, adminTokenPath); len(b) > 0 {
		values = append(values, string(b))
	}
	text := out.String()
	for _, it := range items {
		text += it.Note
	}
	if err != nil {
		text += err.Error()
	}
	for _, v := range values {
		if v == "" {
			continue
		}
		if strings.Contains(text, v) {
			t.Errorf("a secret value appears in the report: len %d", len(v))
		}
	}
	// The report still says where the password is.
	if !strings.Contains(out.String(), dashPasswordPath) {
		t.Errorf("the dashboard row should name its file:\n%s", out.String())
	}
}

// tokenOf reads the first token=consumer pair's token ("a=local,b=x" -> "a").
func tokenOf(consumer string) string {
	first, _, _ := strings.Cut(consumer, ",")
	tok, _, _ := strings.Cut(first, "=")
	return tok
}

// TestSpoondSelfSignedCert: the generated certificate parses, carries
// the DNS and IP SANs, and the pair's modes are right; with
// --tls-cert/--tls-key the pair is pointed at and a missing file fails.
func TestSpoondSelfSignedCert(t *testing.T) {
	env, _, _, _ := spoondTestEnv(t)
	env.Opt.HostName = "sb.example.com"
	if _, _, err := runSpoond(t, env); err != nil {
		t.Fatalf("run: %v", err)
	}
	pemBytes, mode := read(t, env, tlsCertPath)
	if mode != 0o644 {
		t.Errorf("cert mode = %#o, want 0644", mode)
	}
	blk, rest := pem.Decode(swap(pemBytes))
	if blk == nil {
		t.Fatal("no certificate PEM in " + tlsCertPath)
	}
	if len(rest) != 0 {
		t.Errorf("certificate PEM carries trailing bytes")
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatalf("parse the certificate: %v", err)
	}
	if cert.Subject.CommonName != "sb.example.com" {
		t.Errorf("CN = %q, want the host name", cert.Subject.CommonName)
	}
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != "sb.example.com" {
		t.Errorf("DNS SANs = %v, want sb.example.com", cert.DNSNames)
	}
	if len(cert.IPAddresses) != 1 || cert.IPAddresses[0].String() != "10.0.0.11" {
		t.Errorf("IP SANs = %v, want 10.0.0.11", cert.IPAddresses)
	}
	if cert.PublicKeyAlgorithm != x509.ECDSA {
		t.Errorf("key algorithm = %v, want ECDSA (P-256)", cert.PublicKeyAlgorithm)
	}
	if pk, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || pk.Curve != elliptic.P256() {
		t.Errorf("public key = %T, want an ECDSA P-256", cert.PublicKey)
	}
	if cert.NotAfter.Sub(cert.NotBefore) < 365*24*time.Hour*9 {
		t.Errorf("validity = %v, want about ten years", cert.NotAfter.Sub(cert.NotBefore))
	}

	// The key file parses as an EC private key and matches the cert.
	keyPEM, mode := read(t, env, tlsKeyPath)
	if mode != 0o600 {
		t.Errorf("key mode = %#o, want 0600", mode)
	}
	keyBlk, _ := pem.Decode(swap(keyPEM))
	if keyBlk == nil || keyBlk.Type != "EC PRIVATE KEY" {
		t.Fatalf("no EC PRIVATE KEY in " + tlsKeyPath)
	}
	if _, err := x509.ParseECPrivateKey(keyBlk.Bytes); err != nil {
		t.Errorf("parse the private key: %v", err)
	}
}

// swap is an identity for pem.Decode's argument (kept for symmetry).
func swap(pemBytes []byte) []byte { return pemBytes }

// TestSpoondTLSFlaggedPairMissing: --tls-cert pointing at a file that
// does not exist fails with the fix.
func TestSpoondTLSFlaggedPairMissing(t *testing.T) {
	env, _, _, _ := spoondTestEnv(t)
	env.Opt.TLSCert = "/etc/spoond-tls/missing.pem"
	env.Opt.TLSKey = "/etc/spoond-tls/missing.key"
	items, _, err := runSpoond(t, env)
	if err == nil {
		t.Fatal("want a failure for a missing --tls-cert file")
	}
	row := firstFailedRow(t, items)
	if row.Name != env.Opt.TLSCert {
		t.Errorf("failed row = %q, want the missing cert's name", row.Name)
	}
	if !strings.Contains(row.Note, "is missing") {
		t.Errorf("the fix should say what is missing: %q", row.Note)
	}
	if _, err := os.Stat(env.Resolve(tlsCertPath)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the self-signed pair should not have been made: %v", err)
	}
}

// TestSpoondTLSFlaggedPair: an existing pair is pointed at; the
// self-signed one is not made.
func TestSpoondTLSFlaggedPair(t *testing.T) {
	env, _, _, _ := spoondTestEnv(t)
	dir := filepath.Join(env.Root, "operator-tls")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	certP, keyP := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certP, []byte("operator-cert"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyP, []byte("operator-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	env.Opt.TLSCert = "/operator-tls/cert.pem" // under root: env.Resolve joins
	env.Opt.TLSKey = "/operator-tls/key.pem"
	items, text, err := runSpoond(t, env)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, text)
	}
	if got := envVal(t, env, backendEnvPath, "TLS_CERT"); !strings.HasSuffix(got, "/operator-tls/cert.pem") {
		t.Errorf("TLS_CERT = %q, want the operator's path", got)
	}
	if got := envVal(t, env, backendEnvPath, "TLS_KEY"); !strings.HasSuffix(got, "/operator-tls/key.pem") {
		t.Errorf("TLS_KEY = %q, want the operator's path", got)
	}
	if got := envVal(t, env, dashEnvPath, "DASH_TLS_CERT"); !strings.HasSuffix(got, "/operator-tls/cert.pem") {
		t.Errorf("DASH_TLS_CERT = %q, want the operator's path", got)
	}
	if _, err := os.Stat(env.Resolve(tlsCertPath)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a self-signed pair was made beside the operator's: %v", err)
	}
	row, ok := resultOf(t, items, env.Opt.TLSCert+" + "+env.Opt.TLSKey)
	if !ok || row.Result != Kept {
		t.Errorf("the operator pair row = %+v, want kept", row)
	}
}

// TestSpoondTLSCertWithoutKey: --tls-cert and --tls-key go together
// (the flags validate), a cert without a key file fails the stat.
func TestSpoondTLSFlaggedHalfPair(t *testing.T) {
	env, _, _, _ := spoondTestEnv(t)
	dir := filepath.Join(env.Root, "op-tls")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cert.pem"), []byte("c"), 0o644); err != nil {
		t.Fatal(err)
	}
	env.Opt.TLSCert = "/op-tls/cert.pem"
	env.Opt.TLSKey = "/op-tls/nope.pem"
	items, _, err := runSpoond(t, env)
	if err == nil {
		t.Fatal("want a failure for the missing key file")
	}
	firstFailedRow(t, items)
	if _, err := os.Stat(env.Resolve(tlsCertPath)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a self-signed pair was made: %v", err)
	}
}

// TestSpoondConfigOnly: --container --config-only writes the config
// and runs no command (no systemctl, no binary copy, no health).
func TestSpoondConfigOnly(t *testing.T) {
	env, out, run, fetch := spoondTestEnv(t)
	env.Opt.Container = true
	env.Opt.ConfigOnly = true
	items, text, err := runSpoond(t, env)
	if err != nil {
		t.Fatalf("config-only run: %v\n%s", err, text)
	}
	_ = out
	// No changing command (systemctl, a binary copy): only the
	// host-IP query may run, and a config-only run has nothing to
	// start or reload.
	for _, call := range run.calls {
		if strings.Contains(call, "systemctl") {
			t.Errorf("config-only ran %s", call)
		}
	}
	if fetch.healthGet != 0 {
		t.Errorf("config-only made %d health GETs", fetch.healthGet)
	}
	if len(fetch.bodies) != 0 {
		t.Errorf("config-only made %d POSTs", len(fetch.bodies))
	}
	// The config is there.
	for _, p := range []string{backendEnvPath, gatewayEnvPath, dashEnvPath, drainEnvPath, adminTokenPath, dashPasswordPath, tlsCertPath, tlsKeyPath} {
		if _, err := os.Stat(env.Resolve(p)); err != nil {
			t.Errorf("config-only did not write %s: %v", p, err)
		}
	}
	// /opt/spoond was not touched.
	if _, err := os.Stat(env.Resolve(spoondBinPath)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("config-only copied the binary: %v", err)
	}
	// The units were not written.
	if _, err := os.Stat(env.Resolve(spoondBackendUnitPath())); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("config-only wrote the units")
	}
	// The items after the config stop never ran: no binary row.
	if _, ok := resultOf(t, items, spoondBinPath); ok {
		t.Errorf("config-only reported a binary row")
	}
	if _, ok := resultOf(t, items, "forgejo runner"); ok {
		t.Errorf("config-only reported the runner row")
	}
}

// spoondBackendUnitPath is the units table's backend path (a func here
// so the table's literals stay in spoond.go).
func spoondBackendUnitPath() string { return spoondUnits[0].path }

// TestSpoondContainerSkipsBinary: --container (without config-only)
// runs health and units through the fake but skips the binary copy.
func TestSpoondContainerSkipsBinary(t *testing.T) {
	env, out, run, fetch := spoondTestEnv(t)
	env.Opt.Container = true
	items, text, err := runSpoond(t, env)
	if err != nil {
		t.Fatalf("container run: %v\n%s", err, text)
	}
	if _, err := os.Stat(env.Resolve(spoondBinPath)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the binary was copied under --container")
	}
	row, ok := resultOf(t, items, spoondBinPath)
	if !ok || row.Result != Skipped {
		t.Errorf("the binary row = %+v, want skipped", row)
	}
	// enable --now is skipped too (docker compose runs the services).
	for _, call := range run.calls {
		if strings.Contains(call, "systemctl enable") {
			t.Errorf("enable --now ran under --container: %s", call)
		}
	}
	// ... and so is the restart; health still waited on.
	if fetch.healthGet != 1 {
		t.Errorf("health GETs = %d, want 1 (the container still comes up)", fetch.healthGet)
	}
	if !strings.Contains(out.String(), "docker compose runs the services") {
		t.Errorf("the services row should say why:\n%s", out.String())
	}
}

// TestSpoondTLSCertWithoutKey: the self-signed keep rule is a pair
// rule — a leftover cert.pem without privkey.pem fails with the fix
// (the mirror case, a key without a cert, fails the same way).
func TestSpoondTLSCertWithoutKeyFails(t *testing.T) {
	env, _, _, _ := spoondTestEnv(t)
	full := env.Resolve(tlsCertPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("leftover cert"), 0o644); err != nil {
		t.Fatal(err)
	}
	items, _, err := runSpoond(t, env)
	if err == nil {
		t.Fatal("want a failure for a cert without its key")
	}
	if !strings.Contains(err.Error(), tlsKeyPath) {
		t.Errorf("the failure should name the missing key: %v", err)
	}
	if _, err := os.Stat(env.Resolve(tlsKeyPath)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a key was made beside the leftover certificate: %v", err)
	}
	if _, err := os.Stat(env.Resolve(tlsKeyPath)); err == nil {
		t.Errorf("privkey.pem exists")
	}
	row := firstFailedRow(t, items)
	if row.Name != tlsCertPath {
		t.Errorf("failed row = %q, want the existing cert's name", row.Name)
	}
}

// TestSpoondTLSKeyWithoutCertFails: the mirror half pair fails too.
func TestSpoondTLSKeyWithoutCertFails(t *testing.T) {
	env, _, _, _ := spoondTestEnv(t)
	full := env.Resolve(tlsKeyPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("leftover key"), 0o600); err != nil {
		t.Fatal(err)
	}
	items, _, err := runSpoond(t, env)
	if err == nil {
		t.Fatal("want a failure for a key without its certificate")
	}
	if !strings.Contains(err.Error(), tlsCertPath) {
		t.Errorf("the failure should name the missing certificate: %v", err)
	}
	row := firstFailedRow(t, items)
	if row.Name != tlsKeyPath {
		t.Errorf("failed row = %q, want the existing key's name", row.Name)
	}
}

// TestSpoondStoreDir: --store-dir lands in backend.env and every unit.
func TestSpoondStoreDir(t *testing.T) {
	env, _, _, _ := spoondTestEnv(t)
	env.Opt.StoreDir = "/data/e2b"
	items, text, err := runSpoond(t, env)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, text)
	}
	if got, want := envVal(t, env, backendEnvPath, "E2B_TEMPLATE_STORAGE_PATH"), "/data/e2b/storage/templates"; got != want {
		t.Errorf("E2B_TEMPLATE_STORAGE_PATH = %q, want %q", got, want)
	}
	found := false
	for _, u := range spoondUnits {
		b, _ := read(t, env, u.path)
		if strings.Contains(string(b), "/data/e2b") {
			found = true
		}
	}
	if !found {
		t.Errorf("no unit carries the substituted store dir")
	}
	// The default store dir appears nowhere once substituted away.
	for _, u := range spoondUnits {
		b, _ := read(t, env, u.path)
		if strings.Contains(string(b), "/forkdcache/e2b") {
			t.Errorf("%s kept the default store dir under --store-dir", u.path)
		}
	}
	_ = items
}

// TestSpoondFirstUserPosted: the POST's body and headers exactly match
// docs/install.md section 6.
func TestSpoondFirstUserPosted(t *testing.T) {
	env, _, _, fetch := spoondTestEnv(t)
	items, text, err := runSpoond(t, env)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, text)
	}
	if len(fetch.bodies) != 1 {
		t.Fatalf("POSTs = %d, want 1", len(fetch.bodies))
	}
	req := fetch.requests[len(fetch.requests)-1]
	if req.Method != http.MethodPost || req.URL.String() != backendURL+"/api/users" {
		t.Errorf("request = %s %s, want POST %s/api/users", req.Method, req.URL, backendURL)
	}
	var body struct {
		Name         string   `json:"name"`
		Kind         string   `json:"kind"`
		Fingerprints []string `json:"fingerprints"`
	}
	if err := jsonUnmarshal([]byte(fetch.bodies[0]), &body); err != nil {
		t.Fatalf("the POST body is not the expected JSON: %v\n%s", err, fetch.bodies[0])
	}
	if body.Name != "admin" || body.Kind != "person" || len(body.Fingerprints) != 1 || !strings.HasPrefix(body.Fingerprints[0], "SHA256:") {
		t.Errorf("POST body = %+v", body)
	}
	// The fingerprint matches the key file's ssh-keygen -lf value.
	fp, err := sshFingerprintFile(env.Opt.AdminKey)
	if err != nil {
		t.Fatal(err)
	}
	if body.Fingerprints[0] != fp {
		t.Errorf("fingerprint = %q, want %q", body.Fingerprints[0], fp)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	_ = items
}

// TestSpoondFirstUserKept when users.json already holds a user: no
// POST, a kept row.
func TestSpoondFirstUserKept(t *testing.T) {
	env, _, _, fetch := spoondTestEnv(t)
	full := env.Resolve(usersPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(`[{"id":"u-1","name":"someone","kind":"person","admin":true}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	items, text, err := runSpoond(t, env)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, text)
	}
	if len(fetch.bodies) != 0 {
		t.Errorf("a POST ran with users already present")
	}
	row, ok := resultOf(t, items, usersPath)
	if !ok || row.Result != Kept {
		t.Errorf("the users row = %+v, want kept", row)
	}
	if !strings.Contains(text, "the first admin user is kept") {
		t.Errorf("the kept row should say so:\n%s", text)
	}
}

// TestSpoondFirstUserNoKey: without --admin-key the row is skipped
// with the fix.
func TestSpoondFirstUserNoKey(t *testing.T) {
	env, out, _, fetch := spoondTestEnv(t)
	env.Opt.AdminKey = ""
	items, text, err := runSpoond(t, env)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, text)
	}
	if len(fetch.bodies) != 0 {
		t.Errorf("a POST ran without --admin-key")
	}
	row, ok := resultOf(t, items, "first admin user")
	if !ok || row.Result != Skipped {
		t.Errorf("the user row = %+v, want skipped", row)
	}
	if !strings.Contains(text, "pass --admin-key ~/.ssh/id_ed25519.pub") {
		t.Errorf("the skipped row should carry the fix:\n%s", text)
	}
	if !strings.Contains(out.String(), "pass --admin-key") {
		t.Errorf("the report should tell the operator:\n%s", out.String())
	}
}

// TestSpoondDryRun: nothing written, no changing command, no HTTP.
func TestSpoondDryRun(t *testing.T) {
	env, out, run, fetch := spoondTestEnv(t)
	env.DryRun = true
	items, text, err := runSpoond(t, env)
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, text)
	}
	if fetch.healthGet != 0 || len(fetch.bodies) != 0 {
		t.Errorf("the dry run made HTTP requests")
	}
	for _, call := range run.calls {
		// queries only: hostname, ip, is-active
		if !strings.HasPrefix(call, "hostname -f") && !strings.HasPrefix(call, "ip -4 -o route") && !strings.HasPrefix(call, "systemctl is-active") {
			t.Errorf("dry run ran a changing command: %s", call)
		}
	}
	for _, p := range []string{backendEnvPath, adminTokenPath, drainEnvPath, gatewayEnvPath, dashEnvPath, dashPasswordPath, tlsCertPath, tlsKeyPath, spoondBinPath, "/var/lib/spoond"} {
		full := env.Resolve(p)
		if _, err := os.Stat(full); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("dry run created %s", p)
		}
	}
	// The report still plans everything the real run would do.
	if !strings.Contains(out.String(), backendEnvPath) || !strings.Contains(out.String(), tlsKeyPath) {
		t.Errorf("the dry-run report should name the files:\n%s", out.String())
	}
	if len(items) == 0 {
		t.Errorf("the dry run produced no rows")
	}
}

// TestSpoondDefaultsFromQueries: no --host-name reads `hostname -f`;
// an empty IP fails with the env-file fix; the guest resolver skips
// loopback entries.
func TestSpoondDefaultsFromQueries(t *testing.T) {
	t.Run("hostname query", func(t *testing.T) {
		env, _, run, _ := spoondTestEnv(t)
		env.Opt.HostName = ""
		if _, _, err := runSpoond(t, env); err != nil {
			t.Fatalf("run: %v", err)
		}
		if !contains(run.calls, "hostname -f") {
			t.Errorf("hostname -f was not queried: %v", run.calls)
		}
		if got, want := envVal(t, env, gatewayEnvPath, "SPOOND_GATEWAY_HOST"), "sb.example.com"; got != want {
			t.Errorf("SPOOND_GATEWAY_HOST = %q, want the queried hostname", got)
		}
		if got, want := envVal(t, env, backendEnvPath, "SPOOND_PROXY_HOST_SUFFIX"), ".sb.example.com"; got != want {
			t.Errorf("SPOOND_PROXY_HOST_SUFFIX = %q, want the queried hostname's dot", got)
		}
	})
	t.Run("no route", func(t *testing.T) {
		env, _, _, _ := spoondTestEnv(t)
		env.Run.(*spoondRunner).ip = ""
		items, _, err := runSpoond(t, env)
		if err == nil {
			t.Fatal("want a failure for a host without an IPv4 default route")
		}
		row := firstFailedRow(t, items)
		if row.Name != "host IP" {
			t.Errorf("failed row = %q, want host IP", row.Name)
		}
		for _, want := range []string{"no IPv4 default route", "HOST_GUEST_SERVICE_ADDR", backendEnvPath} {
			if !strings.Contains(row.Note, want) {
				t.Errorf("the fix should say %q: %q", want, row.Note)
			}
		}
	})
	t.Run("guest dns from resolv.conf", func(t *testing.T) {
		env, _, _, _ := spoondTestEnv(t)
		writeResolvConf(t, env, "nameserver 127.0.0.53\nnameserver 10.0.0.2\n# tail\n")
		if _, _, err := runSpoond(t, env); err != nil {
			t.Fatal(err)
		}
		if got, want := envVal(t, env, backendEnvPath, "SPOOND_GUEST_DNS_ADDR"), "10.0.0.2"; got != want {
			t.Errorf("SPOOND_GUEST_DNS_ADDR = %q, want the first non-loopback resolver", got)
		}
	})
	t.Run("guest dns loopback only", func(t *testing.T) {
		env, out, _, _ := spoondTestEnv(t)
		writeResolvConf(t, env, "nameserver 127.0.0.53\nnameserver ::1\n")
		items, _, err := runSpoond(t, env)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if got := envVal(t, env, backendEnvPath, "SPOOND_GUEST_DNS_ADDR"); got != "" {
			t.Errorf("SPOOND_GUEST_DNS_ADDR = %q, want empty", got)
		}
		row, ok := resultOf(t, items, "guest DNS")
		if !ok || row.Result != Skipped {
			t.Errorf("the guest DNS row = %+v, want skipped", row)
		}
		if !strings.Contains(out.String(), "no DNS resolver allowance") {
			t.Errorf("the report should say what an empty resolver means:\n%s", out.String())
		}
	})
}

// writeResolvConf writes a resolv.conf under the root.
func writeResolvConf(t *testing.T, env *Env, content string) {
	t.Helper()
	full := env.Resolve(resolvConfPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// TestSpoondHealthWaitsAndFails: the first two tries fail, the third
// answers 200 (a slow start is waited out). The never-healthy path and
// its failure text are pinned by TestSpoondUnhealthyBackendStops.
func TestSpoondHealthWaitsAndFails(t *testing.T) {
	t.Run("healthy on the third try", func(t *testing.T) {
		env, out, _, fetch := spoondTestEnv(t)
		fetch.getCodes = []int{0, 0 /*err*/, http.StatusOK}
		if _, text, err := runSpoond(t, env); err != nil {
			t.Fatalf("run: %v\n%s", err, text)
		}
		if fetch.healthGet < 3 {
			t.Errorf("health GETs = %d, want at least 3", fetch.healthGet)
		}
		if !strings.Contains(out.String(), "spoond-backend is healthy") {
			t.Errorf("the healthy row:\n%s", out.String())
		}
	})

}

// TestSpoondUnhealthyBackendStops: a backend that never answers /healthz
// stops the install after the 60 s wait with the row and error naming
// the journalctl command the operator runs next.
func TestSpoondUnhealthyBackendStops(t *testing.T) {
	env, _, _, fetch := spoondTestEnv(t)
	fetch.getCodes = []int{0} // every try fails
	setHealthWait(t, time.Second)
	items, _, err := runSpoond(t, env)
	if err == nil {
		t.Fatal("want a failure for a never-healthy backend")
	}
	for _, want := range []string{"spoond-backend is not healthy", "journalctl -u spoond-backend -n 50"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure should say %q: %v", want, err)
		}
	}
	row := firstFailedRow(t, items)
	if row.Name != healthzURL() || !strings.HasSuffix(err.Error(), row.Note) {
		t.Errorf("failed row = %+v, want the healthz row carrying the same failure (err %v)", row, err)
	}
	// The phase stops: the first user never ran (no POST).
	if len(fetch.bodies) != 0 {
		t.Errorf("the install continued past the failed health wait")
	}
}

// setUptimeMono pins the boot clock the horizon reads and restores it.
func setUptimeMono(t *testing.T, f func() (int64, error)) {
	t.Helper()
	old := uptimeMono
	uptimeMono = f
	t.Cleanup(func() { uptimeMono = old })
}

// TestSpoondRestartAfterChange: a changed binary is followed by a
// restart when the unit was active before this run started it, and
// skipped when it is inactive, when it was started by this run's
// enable --now, or when its start stamp cannot be read.
func TestSpoondRestartAfterChange(t *testing.T) {
	t.Run("active unit started before this install", func(t *testing.T) {
		env, _, run, _ := spoondTestEnv(t)
		// The boot clock read just before the enable, and the unit's
		// start stamp: started at 500 s, the install ran at 1000 s.
		setUptimeMono(t, func() (int64, error) { return 1_000_000_000, nil })
		run.extra = map[string]reply{
			"systemctl is-active --quiet spoond-backend":                             {out: []byte("active\n")},
			"systemctl show -p ActiveEnterTimestampMonotonic --value spoond-backend": {out: []byte("500000000\n")},
		}
		if _, _, err := runSpoond(t, env); err != nil {
			t.Fatalf("first run: %v", err)
		}
		// Second run with a different binary: restart expected.
		dir := filepath.Join(env.Root, "fake-bin-home")
		if err := os.WriteFile(filepath.Join(dir, "spoond"), []byte("new binary bytes\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		run.calls = nil
		if _, _, err := runSpoond(t, env); err != nil {
			t.Fatalf("second run: %v", err)
		}
		joined := strings.Join(run.calls, "\n")
		if !strings.Contains(joined, "systemctl restart spoond-backend") {
			t.Errorf("no restart after a binary change: %v", run.calls)
		}
	})
	t.Run("unit this run started", func(t *testing.T) {
		env, _, run, _ := spoondTestEnv(t)
		// The unit's start stamp sits just after the horizon: enable
		// --now started it seconds ago, so this run's own start is
		// not restarted a second time.
		setUptimeMono(t, func() (int64, error) { return 1_000_000_000, nil })
		run.extra = map[string]reply{
			"systemctl is-active --quiet spoond-backend":                             {out: []byte("active\n")},
			"systemctl show -p ActiveEnterTimestampMonotonic --value spoond-backend": {out: []byte("1000000001\n")},
		}
		if _, _, err := runSpoond(t, env); err != nil {
			t.Fatalf("run: %v", err)
		}
		for _, call := range run.calls {
			if strings.Contains(call, "systemctl restart") {
				t.Errorf("restarted a unit this run started: %s", call)
			}
		}
	})
	t.Run("unreadable start stamp", func(t *testing.T) {
		env, _, run, _ := spoondTestEnv(t)
		setUptimeMono(t, func() (int64, error) { return 1_000_000_000, nil })
		run.extra = map[string]reply{
			"systemctl is-active --quiet spoond-backend":                             {out: []byte("active\n")},
			"systemctl show -p ActiveEnterTimestampMonotonic --value spoond-backend": {err: fmt.Errorf("no such property")},
		}
		if _, _, err := runSpoond(t, env); err != nil {
			t.Fatalf("run: %v", err)
		}
		for _, call := range run.calls {
			if strings.Contains(call, "systemctl restart") {
				t.Errorf("restarted a unit whose start could not be dated: %s", call)
			}
		}
	})
	t.Run("inactive unit", func(t *testing.T) {
		env, _, run, _ := spoondTestEnv(t)
		if _, _, err := runSpoond(t, env); err != nil {
			t.Fatalf("first run: %v", err)
		}
		run.extra = map[string]reply{} // is-active fails: not running
		dir := filepath.Join(env.Root, "fake-bin-home")
		if err := os.WriteFile(filepath.Join(dir, "spoond"), []byte("another binary\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		run.calls = nil
		if _, _, err := runSpoond(t, env); err != nil {
			t.Fatalf("second run: %v", err)
		}
		for _, call := range run.calls {
			if strings.Contains(call, "systemctl restart") {
				t.Errorf("restarted an inactive unit: %s", call)
			}
		}
	})
}

// TestSpoondSecondRunNoRegeneratedSecrets pins the exact file set a
// second run leaves byte-identical plus the rows' results, and that no
// token is minted for a rerun: dash.env, kept, keeps its metrics token.
func TestSpoondSecondRunNoRegeneratedSecrets(t *testing.T) {
	env, _, _, _ := spoondTestEnv(t)
	if _, _, err := runSpoond(t, env); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first := map[string][]byte{}
	for _, p := range []string{backendEnvPath, adminTokenPath, drainEnvPath, gatewayEnvPath, dashEnvPath, dashPasswordPath, tlsCertPath, tlsKeyPath} {
		first[p], _ = read(t, env, p)
	}
	if _, _, err := runSpoond(t, env); err != nil {
		t.Fatalf("second run: %v", err)
	}
	for p, want := range first {
		got, _ := read(t, env, p)
		if !bytes.Equal(got, want) {
			t.Errorf("%s changed on the second run", p)
		}
	}
	// Kept dash.env keeps its own metrics token (nothing minted for
	// a rerun gets discarded on the keep path).
	if got := envVal(t, env, dashEnvPath, "METRICS_TOKEN"); got == "" || !isHex(got) {
		t.Errorf("dash METRICS_TOKEN = %q, want 64 hex", got)
	}
}

// TestSpoondDashEnvMissingOnRerun: the metric token never appears in
// backend.env, so a host whose dash.env (or dash-password) was deleted
// between runs gets a fresh one in the rewritten dash.env while every
// other file keeps.
func TestSpoondDashEnvMissingOnRerun(t *testing.T) {
	env, _, _, _ := spoondTestEnv(t)
	if _, _, err := runSpoond(t, env); err != nil {
		t.Fatalf("first run: %v", err)
	}
	firstDash, _ := read(t, env, dashEnvPath)
	if err := os.Remove(env.Resolve(dashEnvPath)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runSpoond(t, env); err != nil {
		t.Fatalf("second run: %v", err)
	}
	secondDash, _ := read(t, env, dashEnvPath)
	if bytes.Equal(secondDash, firstDash) {
		t.Fatalf("dash.env was not rewritten after deletion")
	}
	// The other files are untouched, and the fresh dash.env still
	// agrees with backend.env on the tokens backend.env carries.
	if got, want := envVal(t, env, dashEnvPath, "METRICS_TOKEN"), envVal(t, env, backendEnvPath, "METRICS_TOKEN"); got != want {
		t.Errorf("dash METRICS_TOKEN = %q, want backend.env's", got)
	}
	if got, want := envVal(t, env, dashEnvPath, "DASH_EVENTS_TOKEN"), envVal(t, env, backendEnvPath, "EVENTS_TOKEN"); got != want {
		t.Errorf("DASH_EVENTS_TOKEN = %q, want backend.env's", got)
	}
	pw, _ := read(t, env, dashPasswordPath)
	if hash := envVal(t, env, dashEnvPath, "DASH_PASSWORD_HASH"); !verifyBcrypt(string(pw), hash) {
		t.Errorf("the rewritten hash does not verify the kept password")
	}
}

// TestSpoondBinaryCopied: a fresh root gets the fake executable's
// bytes at 0755; a rerun with the same bytes is a kept row and no copy.
func TestSpoondBinaryCopied(t *testing.T) {
	env, out, _, _ := spoondTestEnv(t)
	items, text, err := runSpoond(t, env)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, text)
	}
	b, mode := read(t, env, spoondBinPath)
	if mode != 0o755 {
		t.Errorf("/opt/spoond/spoond mode = %#o, want 0755", mode)
	}
	if string(b) != "fake-spoond-binary\n" {
		t.Errorf("copied bytes = %q, want the fake executable's", b)
	}
	row, ok := resultOf(t, items, spoondBinPath)
	if !ok || row.Result != Changed {
		t.Errorf("the binary row = %+v, want changed", row)
	}

	// Rerun with unchanged bytes: kept, and the unit restart not
	// triggered by the binary (the env is kept too).
	if _, _, err := runSpoond(t, env); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if _, mode := read(t, env, spoondBinPath); mode != 0o755 {
		t.Errorf("rerun mode = %#o", mode)
	}
	if !strings.Contains(out.String(), "already the running binary") {
		t.Errorf("the kept row should say the bytes match:\n%s", out.String())
	}
}
