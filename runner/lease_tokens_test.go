package runner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// writeTokenFile writes a token file with the given mode and returns its
// path.
func writeTokenFile(t *testing.T, dir, name, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // umask may have narrowed it
		t.Fatal(err)
	}
	return p
}

// TestParseLeaseTokenMap: an "owner/repo" key maps that repository, an
// "owner" key every repository of it, the repository's entry beats its
// owner's, keys match case-insensitively, entries are trimmed and empty
// ones dropped, and the file's trailing newline is not part of the
// token.
func TestParseLeaseTokenMap(t *testing.T) {
	dir := t.TempDir()
	web := writeTokenFile(t, dir, "web", "tok-web\n", 0o600)
	acme := writeTokenFile(t, dir, "acme", "tok-acme\r\n", 0o600)
	other := writeTokenFile(t, dir, "other", "tok-other", 0o600)

	v := " Acme/Web = " + web + " ,, acme=" + acme + ", example.org/site=" + other + " ,"
	m, warns, err := ParseLeaseTokenMap(v, os.ReadFile)
	if err != nil {
		t.Fatalf("ParseLeaseTokenMap: %v", err)
	}
	if len(warns) != 0 {
		t.Fatalf("warnings for 0600 files: %v", warns)
	}
	if got, want := m.Keys(), []string{"Acme/Web", "acme", "example.org/site"}; !slices.Equal(got, want) {
		t.Fatalf("Keys = %v, want %v", got, want)
	}
	for _, c := range []struct {
		repo string
		want string // "" = unmapped
	}{
		{"acme/web", "tok-web"},           // exact repo
		{"ACME/WEB", "tok-web"},           // case-insensitive
		{"acme/api", "tok-acme"},          // owner
		{"Acme/Other", "tok-acme"},        // owner, case-insensitive
		{"example.org/site", "tok-other"}, // dotted owner
		{"example.org/blog", ""},          // a repo entry does not cover its owner
		{"someone/else", ""},
		{"acme", ""}, // not owner/repo
		{"", ""},
	} {
		got, ok := m.TokenFor(c.repo)
		if ok != (c.want != "") || got != c.want {
			t.Errorf("TokenFor(%q) = (%q, %v), want %q", c.repo, got, ok, c.want)
		}
	}
}

// TestParseLeaseTokenMapIdentities: two keys whose files hold the same
// token are one identity (one client per worker, one sweep).
func TestParseLeaseTokenMapIdentities(t *testing.T) {
	dir := t.TempDir()
	a := writeTokenFile(t, dir, "a", "tok-shared\n", 0o600)
	b := writeTokenFile(t, dir, "b", "tok-shared", 0o600)
	c := writeTokenFile(t, dir, "c", "tok-c", 0o600)
	m, _, err := ParseLeaseTokenMap("one/x="+a+",two="+b+",three="+c, os.ReadFile)
	if err != nil {
		t.Fatalf("ParseLeaseTokenMap: %v", err)
	}
	ids := m.Identities()
	if len(ids) != 2 {
		t.Fatalf("got %d identities, want 2: %+v", len(ids), ids)
	}
	if ids[0].Token != "tok-shared" || !slices.Equal(ids[0].Keys, []string{"one/x", "two"}) {
		t.Errorf("identity 0 = %+v, want tok-shared for [one/x two]", ids[0])
	}
	if ids[1].Token != "tok-c" || !slices.Equal(ids[1].Keys, []string{"three"}) {
		t.Errorf("identity 1 = %+v, want tok-c for [three]", ids[1])
	}
}

// TestParseLeaseTokenMapEmpty: unset (or only separators) is an empty
// map — no keys, no router, nothing mapped — so every job uses
// LEASE_TOKEN exactly as before.
func TestParseLeaseTokenMapEmpty(t *testing.T) {
	for _, v := range []string{"", " ", " , ,"} {
		m, warns, err := ParseLeaseTokenMap(v, func(string) ([]byte, error) {
			t.Fatal("read a file for an empty map")
			return nil, nil
		})
		if err != nil || len(warns) != 0 {
			t.Fatalf("ParseLeaseTokenMap(%q) = (%v, %v)", v, warns, err)
		}
		if len(m.Keys()) != 0 || len(m.Identities()) != 0 {
			t.Fatalf("ParseLeaseTokenMap(%q) mapped %v", v, m.Keys())
		}
		if r := m.Router(func(string) SandboxProvider { return newFakeLease() }); r != nil {
			t.Fatalf("Router for an empty map is not nil")
		}
		if _, ok := m.TokenFor("acme/web"); ok {
			t.Fatalf("empty map maps acme/web")
		}
	}
	// A nil map (never parsed) behaves the same.
	var m *LeaseTokenMap
	if m.Keys() != nil || m.Identities() != nil || m.Router(nil) != nil {
		t.Fatal("nil map is not empty")
	}
}

// TestParseLeaseTokenMapErrors: a malformed entry, a key mapped twice,
// or a missing, empty or unreadable file fails, naming the entry — and
// no message carries a token (a value that is not an absolute path may
// be one, so it is never echoed).
func TestParseLeaseTokenMapErrors(t *testing.T) {
	dir := t.TempDir()
	good := writeTokenFile(t, dir, "good", "tok-good-secret", 0o600)
	empty := writeTokenFile(t, dir, "empty", "\n", 0o600)
	for _, c := range []struct {
		name, v, want string
	}{
		{"no equals", "acme/web=" + good + ",inline-secret-token", "entry 2 is not KEY=/path/to/token-file"},
		{"empty key", "=" + good, `entry 1: key "" is not owner/repo or owner`},
		{"three parts", "a/b/c=" + good, `key "a/b/c" is not owner/repo or owner`},
		{"empty owner", "/web=" + good, `key "/web" is not owner/repo or owner`},
		{"empty repo", "acme/=" + good, `key "acme/" is not owner/repo or owner`},
		{"space in key", "ac me=" + good, `key "ac me" is not owner/repo or owner`},
		{"inline token", "acme=inline-secret-token", "acme: the value must be the absolute path of a token file"},
		{"empty value", "acme=", "acme: the value must be the absolute path"},
		{"relative path", "acme=tokens/acme", "acme: the value must be the absolute path"},
		{"duplicate key", "acme/web=" + good + ",ACME/web=" + good, "ACME/web is mapped twice"},
		{"duplicate owner", "acme=" + good + ",Acme=" + good, "Acme is mapped twice"},
		{"missing file", "acme=" + filepath.Join(dir, "gone"), "LEASE_TOKEN_MAP: acme: read " + filepath.Join(dir, "gone")},
		{"empty file", "acme=" + empty, "LEASE_TOKEN_MAP: acme: " + empty + " is empty"},
		{"directory", "acme=" + dir, "LEASE_TOKEN_MAP: acme: read " + dir},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, _, err := ParseLeaseTokenMap(c.v, os.ReadFile)
			if err == nil {
				t.Fatalf("ParseLeaseTokenMap(%q) = %v, want an error", c.v, m.Keys())
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %q, want it to contain %q", err, c.want)
			}
			for _, secret := range []string{"tok-good-secret", "inline-secret-token"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error %q carries a token", err)
				}
			}
		})
	}
}

// TestParseLeaseTokenMapModeWarning: a token file readable by group or
// others is a warning naming the key and the file, as LEASE_TOKEN_FILE's
// is (settings.FileModeWarning) — never the token; 0600 is clean.
func TestParseLeaseTokenMapModeWarning(t *testing.T) {
	dir := t.TempDir()
	open := writeTokenFile(t, dir, "open", "tok-open-secret", 0o644)
	closed := writeTokenFile(t, dir, "closed", "tok-closed", 0o600)
	m, warns, err := ParseLeaseTokenMap("acme="+open+",other/x="+closed, os.ReadFile)
	if err != nil {
		t.Fatalf("ParseLeaseTokenMap: %v", err)
	}
	if len(warns) != 1 {
		t.Fatalf("warnings = %v, want one (for acme)", warns)
	}
	w := warns[0]
	if !strings.HasPrefix(w, "LEASE_TOKEN_MAP: acme: "+open+": mode 0644") {
		t.Errorf("warning = %q, want it to name acme, the file and its mode", w)
	}
	if strings.Contains(w, "tok-open-secret") {
		t.Errorf("warning %q carries the token", w)
	}
	// A warning, not a refusal: the entry is mapped.
	if tok, ok := m.TokenFor("acme/web"); !ok || tok != "tok-open-secret" {
		t.Errorf("TokenFor(acme/web) = (%q, %v) after a mode warning", tok, ok)
	}
}

// fakeIdentityLease is one lease identity's provider: a SandboxProvider
// that labels (LeaseLabeler) and releases with a reason (LeaseReleaser)
// like HTTPLeaseClient, recording every call so a test can tell which
// identity a job's lease went through.
type fakeIdentityLease struct {
	*fakeLease
	token   string
	labels  []string
	reasons []string // "<id>: <reason>"
}

func newFakeIdentityLease(token string) *fakeIdentityLease {
	return &fakeIdentityLease{fakeLease: newFakeLease(), token: token}
}

func (f *fakeIdentityLease) WithLabel(label string) { f.labels = append(f.labels, label) }

func (f *fakeIdentityLease) DeleteReason(ctx context.Context, id, reason string) error {
	f.reasons = append(f.reasons, id+": "+reason)
	return nil
}

// calls is how many lease calls of any kind reached the provider.
func (f *fakeIdentityLease) calls() int {
	return len(f.created) + len(f.execs) + len(f.deleted) + len(f.labels) + len(f.reasons)
}

// routedPayload checks out the repository and runs a step, so the job
// makes every kind of lease call: label, create, checkout execs, a step
// exec and the release.
const routedPayload = `
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: make test
`

// TestExecutorRoutesMappedRepository: a job whose repository the map
// covers leases through that identity's provider — the label, Create,
// every Exec (checkout's and the step's) and the release all reach it,
// for the lease it created — and nothing reaches the default
// (LEASE_TOKEN) provider; a job the map does not cover uses the default
// alone. One executor serves both in turn, as a worker does.
func TestExecutorRoutesMappedRepository(t *testing.T) {
	dir := t.TempDir()
	acme := writeTokenFile(t, dir, "acme", "tok-acme", 0o600)
	web := writeTokenFile(t, dir, "web", "tok-web", 0o600)
	m, _, err := ParseLeaseTokenMap("acme="+acme+",acme/web="+web, os.ReadFile)
	if err != nil {
		t.Fatalf("ParseLeaseTokenMap: %v", err)
	}
	built := map[string]*fakeIdentityLease{}
	router := m.Router(func(token string) SandboxProvider {
		if built[token] != nil {
			t.Fatalf("Router built two providers for one token")
		}
		built[token] = newFakeIdentityLease(token)
		return built[token]
	})
	if len(built) != 2 {
		t.Fatalf("Router built %d providers, want one per token (2)", len(built))
	}
	def := newFakeIdentityLease("tok-default")
	e := &Executor{
		Sandbox:     def,
		Sink:        &fakeSink{},
		Labels:      map[string]string{"ubuntu-latest": "ci-image"},
		RepoBaseURL: "https://code.example.com",
		ForgejoURL:  "https://code.example.com",
		SandboxFor:  router,
	}
	job := func(id int64, repo string) *Job {
		j := testJob(routedPayload)
		j.ID = id
		j.Context["repository"] = repo
		return j
	}

	// The repository's own entry beats its owner's.
	if err := e.Run(context.Background(), job(7, "Acme/Web")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertAllCalls(t, built["tok-web"], 7)
	if n := built["tok-acme"].calls() + def.calls(); n != 0 {
		t.Fatalf("acme/web's job made %d calls through other identities", n)
	}
	// The executor itself is untouched: the next job routes afresh.
	if e.Sandbox != SandboxProvider(def) {
		t.Fatal("Run replaced the executor's own Sandbox")
	}

	// The owner's entry covers its other repositories.
	if err := e.Run(context.Background(), job(8, "acme/api")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertAllCalls(t, built["tok-acme"], 8)

	// An unmapped repository uses the default, and only it.
	webCalls, acmeCalls := built["tok-web"].calls(), built["tok-acme"].calls()
	if err := e.Run(context.Background(), job(9, "someone/else")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertAllCalls(t, def, 9)
	if built["tok-web"].calls() != webCalls || built["tok-acme"].calls() != acmeCalls {
		t.Fatal("an unmapped job made calls through a mapped identity")
	}
}

// assertAllCalls checks that one job's every lease call reached p: its
// label, one create, the checkout's and the step's execs, and the
// release of the lease it created.
func assertAllCalls(t *testing.T, p *fakeIdentityLease, jobID int64) {
	t.Helper()
	if len(p.labels) == 0 || !strings.HasPrefix(p.labels[len(p.labels)-1], JobLabelPrefix) ||
		!strings.Contains(p.labels[len(p.labels)-1], "/jobs/"+strconv.FormatInt(jobID, 10)) {
		t.Fatalf("%s: labels %v, want the label of job %d", p.token, p.labels, jobID)
	}
	if len(p.created) == 0 {
		t.Fatalf("%s: no create for job %d", p.token, jobID)
	}
	id := p.created[len(p.created)-1]
	var clone, step bool
	for _, c := range p.execs {
		clone = clone || strings.HasPrefix(c, "git clone ")
		step = step || c == "make test"
	}
	if !clone || !step {
		t.Fatalf("%s: execs %v, want the checkout's clone and the step", p.token, p.execs)
	}
	if len(p.reasons) == 0 || !strings.HasPrefix(p.reasons[len(p.reasons)-1], id+": ") {
		t.Fatalf("%s: releases %v, want lease %s released", p.token, p.reasons, id)
	}
}

// identityBackend is a lease API that keeps each token's leases apart:
// GET /api/leases lists the caller's own, and a DELETE of another
// token's lease is refused.
type identityBackend struct {
	mu      sync.Mutex
	leases  map[string][]leaseRow // token -> its leases
	deleted map[string][]string   // token -> ids it deleted
}

func (b *identityBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/leases":
		rows, ok := b.leases[token]
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"sandboxes": rows})
	case r.Method == http.MethodDelete:
		id := strings.TrimPrefix(r.URL.Path, "/api/leases/")
		for _, l := range b.leases[token] {
			if l.ID == id {
				b.deleted[token] = append(b.deleted[token], id)
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// TestMultiSweeperSweepsEveryToken (#119): the startup sweep runs under
// every lease identity — a previous process may have left orphans under
// any — and under each it still deletes only runner job leases: the
// project's own leases under a mapped token stay.
func TestMultiSweeperSweepsEveryToken(t *testing.T) {
	b := &identityBackend{
		leases: map[string][]leaseRow{
			"tok-default": {{ID: "d-job", Comment: "forgejo job 1 u"}, {ID: "d-own", Comment: "scratch"}},
			"tok-acme":    {{ID: "a-job", Comment: "forgejo job 2 u"}, {ID: "a-own", Comment: "acme dev box"}, {ID: "a-plain"}},
			"tok-web":     {{ID: "w-job-1", Comment: "forgejo job 3 u"}, {ID: "w-job-2", Comment: "forgejo job 4 u"}},
		},
		deleted: map[string][]string{},
	}
	srv := httptest.NewServer(b)
	defer srv.Close()
	ms := MultiSweeper{
		{Name: "LEASE_TOKEN", Sweeper: NewHTTPLeaseClient(srv.URL, "tok-default")},
		{Name: "LEASE_TOKEN_MAP acme", Sweeper: NewHTTPLeaseClient(srv.URL, "tok-acme")},
		{Name: "LEASE_TOKEN_MAP acme/web", Sweeper: NewHTTPLeaseClient(srv.URL, "tok-web")},
	}
	n, err := ms.SweepOrphans(context.Background(), nil)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if n != 4 {
		t.Fatalf("swept %d leases, want 4", n)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for token, want := range map[string][]string{
		"tok-default": {"d-job"},
		"tok-acme":    {"a-job"},
		"tok-web":     {"w-job-1", "w-job-2"},
	} {
		got := append([]string(nil), b.deleted[token]...)
		sort.Strings(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s: deleted %v, want %v", token, got, want)
		}
	}
}

// TestMultiSweeperKeepsGoingOnError: one identity's failed sweep does
// not stop the others; the count is theirs and the error names the
// identity that failed.
func TestMultiSweeperKeepsGoingOnError(t *testing.T) {
	boom := errors.New("list leases: status 500")
	ok1, bad, ok2 := &fakeSweeper{}, &fakeSweeper{sweepErr: boom}, &fakeSweeper{}
	ms := MultiSweeper{
		{Name: "LEASE_TOKEN", Sweeper: ok1},
		{Name: "LEASE_TOKEN_MAP acme", Sweeper: bad},
		{Name: "LEASE_TOKEN_MAP other/x", Sweeper: ok2},
	}
	n, err := ms.SweepOrphans(context.Background(), nil)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "LEASE_TOKEN_MAP acme: ") {
		t.Fatalf("err = %v, want the failure named by its identity", err)
	}
	if strings.Contains(err.Error(), "other/x") || strings.Contains(err.Error(), "LEASE_TOKEN: ") {
		t.Fatalf("err = %v names identities that did not fail", err)
	}
	if ok1.count() != 1 || bad.count() != 1 || ok2.count() != 1 {
		t.Fatalf("sweeps = %d,%d,%d, want each identity swept once", ok1.count(), bad.count(), ok2.count())
	}
	if !ok1.keepWasNil() || !ok2.keepWasNil() {
		t.Fatal("the start sweep's nil keep was not passed through")
	}
	if n != 9 { // fakeSweeper reports 3 each, the failed one included
		t.Fatalf("swept %d, want 9", n)
	}
}

// TestPoolStartSweepsEveryIdentity: the pool's start sweep goes through
// a MultiSweeper as its Leases, so every identity is swept once before
// any worker runs.
func TestPoolStartSweepsEveryIdentity(t *testing.T) {
	a, b := &fakeSweeper{}, &fakeSweeper{}
	cfg := PoolConfig{Floor: 1, Max: 1, Leases: MultiSweeper{{Name: "LEASE_TOKEN", Sweeper: a}, {Name: "LEASE_TOKEN_MAP acme", Sweeper: b}}}
	p := NewRunnerPool(cfg, func() RunnerWorker { return newFakeWorker() }, "r", "tok", []string{"spoond"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()
	if a.count() != 1 || b.count() != 1 {
		t.Fatalf("start sweeps = %d,%d, want each identity swept once", a.count(), b.count())
	}
}
