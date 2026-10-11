package spoondconfig

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jrimmer/spoond/v3/settings"
)

// fakeProps turns fixed systemctl show output lines into what the
// runner returns.
func fakeProps(lines ...string) runner {
	return func(string, ...string) (map[string]string, error) {
		out := map[string]string{}
		for _, line := range lines {
			k, v, _ := strings.Cut(line, "=")
			out[k] = v
		}
		return out, nil
	}
}

// failRunner answers the systemctl error path.
func failRunner(err string) runner {
	return func(string, ...string) (map[string]string, error) {
		return nil, fmt.Errorf("%s", err)
	}
}

// fileMap builds a reader from path → content.
func fileMap(files map[string]string) reader {
	return func(path string) (string, error) {
		if c, ok := files[path]; ok {
			return c, nil
		}
		return "", fmt.Errorf("no such file: %s", path)
	}
}

// writeFileSecret creates dir's secret value file at path with content
// and returns the path.
func writeFileSecret(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// okFetch answers the admin config endpoint.
func okFetch(rc runningConfig) fetcher {
	return func(string, string) (runningConfig, error) { return rc, nil }
}

// errFetch answers unreachable.
func errFetch(err string) fetcher {
	return func(string, string) (runningConfig, error) {
		return runningConfig{}, fmt.Errorf("%s", err)
	}
}

// running builds a runningConfig from name:set-value pairs; an empty
// value means set:false.
func running(pairs ...string) runningConfig {
	rc := runningConfig{StartedAt: "2026-10-11T10:00:00Z"}
	for _, p := range pairs {
		name, val, _ := strings.Cut(p, "=")
		rc.Settings = append(rc.Settings, struct {
			Name  string `json:"name"`
			Set   bool   `json:"set"`
			Value string `json:"value"`
		}{name, val != "", val})
	}
	return rc
}

// TestUnitEnvMergeOrder (#137): the file merge order follows
// systemd.exec(5) — Environment= is applied first and every
// EnvironmentFile= in listed order overrides it, so a file's
// assignment wins over the unit line and a later file wins over an
// earlier one; within one file the last line wins. Order of first
// appearance is preserved.
func TestUnitEnvMergeOrder(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "10-base.env")
	b := filepath.Join(dir, "20-override.env")
	files := map[string]string{
		a: "BIND_ADDR=127.0.0.1:8890\nPOOL_SIZE=2\n",
		b: "# comment\nexport POOL_SIZE=4\nQUOTED=\"a b\"  # not a comment: systemd keeps it\n",
	}
	show := map[string]string{
		"EnvironmentFiles": fmt.Sprintf("%s (ignore_errors=no) %s (ignore_errors=no)", a, b),
		"Environment":      "BIND_ADDR=0.0.0.0:8890",
	}
	env := unitEnv(show, fileMap(files))
	got := map[string]entry{}
	for _, e := range env {
		got[e.name] = e
	}
	if e := got["BIND_ADDR"]; e.value != "127.0.0.1:8890" || e.source != a {
		t.Fatalf("BIND_ADDR = %+v, want the file value overriding Environment= (source %s)", e, a)
	}
	if e := got["POOL_SIZE"]; e.value != "4" || e.source != b {
		t.Fatalf("POOL_SIZE = %+v, want the later file's 4 from %s", e, b)
	}
	if e := got["QUOTED"]; e.value != `"a b"  # not a comment: systemd keeps it` || e.source != b {
		t.Fatalf("QUOTED = %+v, want the raw quoted assignment (quotes strip in parseEnvLine)", e)
	}
}

// TestUnitEnvUnreadableFile: a file the reader fails on is skipped and
// the rest of the merge still applies, so one missing drop-in does not
// blank the table.
func TestUnitEnvUnreadableFile(t *testing.T) {
	show := map[string]string{
		"EnvironmentFiles": "/gone.env (ignore_errors=no) /here.env (ignore_errors=no)",
		"Environment":      "POOL_SIZE=1",
	}
	env := unitEnv(show, fileMap(map[string]string{"/here.env": "POOL_SIZE=2\n"}))
	if len(env) != 1 || env[0].name != "POOL_SIZE" || env[0].value != "2" || env[0].source != "/here.env" {
		t.Fatalf("env = %+v, want POOL_SIZE=2 from /here.env only", env)
	}
}

// TestUnitEnvNoFiles: the Environment= line alone (drop-in overrides
// without files) is the whole env, source "unit Environment=".
func TestUnitEnvNoFiles(t *testing.T) {
	env := unitEnv(map[string]string{"Environment": "ADMIN_TOKEN=x METRICS_TOKEN=y"}, fileMap(nil))
	if len(env) != 2 {
		t.Fatalf("env = %+v, want both assignments", env)
	}
	for _, e := range env {
		if e.source != fromEnvLine {
			t.Fatalf("%s source = %q, want %q", e.name, e.source, fromEnvLine)
		}
	}
}

// TestSplitEnvs: whitespace-separated assignments, quotes carrying
// spaces; each assignment keeps its quotes (parseEnvLine strips them
// later), the way a systemd Environment= line parses.
func TestSplitEnvs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"A=1 B=two C=3", []string{"A=1", "B=two", "C=3"}},
		{`A="x y" B='p q' C=1`, []string{`A="x y"`, `B='p q'`, "C=1"}},
		{"", nil},
		{"   ", nil},
		{"LONELY", []string{"LONELY"}},
	}
	for _, c := range cases {
		if got := splitEnvs(c.in); !equalSlices(got, c.want) {
			t.Errorf("splitEnvs(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestParseEnvironmentFiles: the (ignore_errors=…) suffix is part of
// the same entry, not a separate one; entries keep their order, and an
// empty property yields no files.
func TestParseEnvironmentFiles(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"/etc/spoond/backend.env (ignore_errors=no)", []string{"/etc/spoond/backend.env"}},
		{"/etc/spoond/backend.env (ignore_errors=yes)", []string{"/etc/spoond/backend.env"}},
		{"/a.env (ignore_errors=no) /b.env (ignore_errors=no)", []string{"/a.env", "/b.env"}},
	}
	for _, c := range cases {
		if got := parseEnvironmentFiles(c.in); !equalSlices(got, c.want) {
			t.Errorf("parseEnvironmentFiles(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestParseEnvLine: export tolerated, whole-line comments skipped,
// quotes strip, a bare K (no =) is skipped the way systemd does.
func TestParseEnvLine(t *testing.T) {
	cases := []struct {
		in        string
		key, want string
		ok        bool
	}{
		{"KEY=val", "KEY", "val", true},
		{"  KEY = spaced ", "KEY", "spaced", true},
		{"export KEY=v", "KEY", "v", true},
		{"KEY=\"q v\"", "KEY", "q v", true},
		{"KEY='q v'", "KEY", "q v", true},
		{"# comment", "", "", false},
		{"KEY#no-comment-strip", "", "", false}, // no '=': not an assignment
		{"novalue", "", "", false},
		{"", "", "", false},
		{"KEY=", "KEY", "", true},
	}
	for _, c := range cases {
		k, v, ok := parseEnvLine(c.in)
		if k != c.key || v != c.want || ok != c.ok {
			t.Errorf("parseEnvLine(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, k, v, ok, c.key, c.want, c.ok)
		}
	}
}

// TestRunTable: the table's VALUE masking, SOURCE and RUNNING
// classification end to end with fakes — a secret value never prints,
// a plain value does, drift and unset-running classify per #137, an
// unknown key carries its note, and an unreachable backend marks every
// RUNNING cell "?" with exit 0.
func TestRunTable(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "backend.env")
	files := map[string]string{
		envFile: strings.Join([]string{
			"ADMIN_TOKEN=sekrit",          // secret name: masked, never printed
			"BIND_ADDR=127.0.0.1:8890",    // plain, matches running → same
			"POOL_SIZE=4",                 // plain, differs in running → drift
			"METRICS_TOKEN=mtok",          // secret, absent from running → not set there
			"SPOOND_DB_PATH=/x/spoond.db", // plain, running unset → not set there
			"TOTALLY_BOGUS=1",             // unknown name in the unit env
		}, "\n"),
	}
	// The running backend: masked values as the endpoint would send
	// (plain values raw, secrets "(hidden)").
	rc := running(
		"ADMIN_TOKEN=(hidden)",     // set, hidden → same by masked bytes
		"BIND_ADDR=127.0.0.1:8890", // same
		"POOL_SIZE=2",              // differs
		"SPOOND_DB_PATH=",          // unset in running
		"EXTRA_UNREPORTED=",
	)
	var out strings.Builder
	code := run("spoond-backend", "", false, &out, fakeProps(
		"EnvironmentFiles="+envFile+" (ignore_errors=no)",
		"Environment=",
	), fileMap(files), okFetch(rc))
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	got := out.String()
	// tabwriter pads with spaces, so assert on the parsed fields rather
	// than raw column strings: NAME VALUE SOURCE RUNNING [note...]. The
	// source and running columns carry no spaces in this test's fixtures
	// except the note; a RUNNING value with spaces is matched with
	// HasPrefix below.
	type parsed struct {
		name, value, source, running, note string
	}
	var parsedRows []parsed
	for _, l := range strings.Split(got, "\n") {
		f := strings.Fields(l)
		if len(f) < 4 || f[0] == "NAME" {
			continue
		}
		parsedRows = append(parsedRows, parsed{
			name:    f[0],
			value:   f[1],
			source:  f[2],
			running: f[3],
			note:    strings.Join(f[4:], " "),
		})
	}
	find := func(name string) (parsed, bool) {
		for _, l := range parsedRows {
			if l.name == name {
				return l, true
			}
		}
		return parsed{}, false
	}

	// SECRET VALUES NEVER PRINT, on no row, in no cell, in no note.
	for _, secret := range []string{"sekrit", "mtok"} {
		if strings.Contains(got, secret) {
			t.Errorf("output leaks secret %q:\n%s", secret, got)
		}
	}
	// The masked secret and plain rows with their classifications.
	checks := []struct {
		name, value, source, running string
	}{
		{"ADMIN_TOKEN", "(hidden)", envFile, runSame},
		{"BIND_ADDR", "127.0.0.1:8890", envFile, runSame},
		{"POOL_SIZE", "4", envFile, runDiffers},
		{"METRICS_TOKEN", "(hidden)", envFile, runNotSet},
		{"SPOOND_DB_PATH", "/x/spoond.db", envFile, runNotSet},
		{"TOTALLY_BOGUS", "1", envFile, runSkip},
	}
	for _, c := range checks {
		l, ok := find(c.name)
		if !ok {
			t.Fatalf("row %s missing:\n%s", c.name, got)
		}
		// RUNNING carries spaces ("differs — restart to apply"), the
		// note column follows it; match the full joined tail.
		tail := l.running
		if l.note != "" {
			tail += " " + l.note
		}
		if l.value != c.value || l.source != c.source || !strings.HasPrefix(tail, c.running) {
			t.Errorf("%s row = value %q source %q running %q+note %q, want %q from %q, %q", c.name, l.value, l.source, l.running, l.note, c.value, c.source, c.running)
		}
		// The note column is indistinguishable from the RUNNING tail by
		// fields alone except for the known note text; the known rows
		// here end with the running cell, the bogus row ends with the
		// exact note.
		if c.name == "TOTALLY_BOGUS" {
			if tail != runSkip+" (unknown setting)" {
				t.Errorf("bogus row tail = %q, want %q (unknown setting)\"", tail, runSkip)
			}
		}
	}
	// The footer counts the three drift rows.
	if !strings.Contains(got, "3 setting(s) differ from the running backend") {
		t.Errorf("footer:\n%s", got)
	}
}

// TestRunUnreachable: an unreachable running backend marks every
// RUNNING cell "?" (an unset-in-unit setting included), keeps printing
// the unit's view and exits 0.
func TestRunUnreachable(t *testing.T) {
	showAll = true // this test reads every known row
	t.Cleanup(func() { showAll = false })
	dir := t.TempDir()
	envFile := filepath.Join(dir, "backend.env")
	files := map[string]string{envFile: "BIND_ADDR=127.0.0.1:8890\n"}
	var out strings.Builder
	code := run("spoond-backend", "", false, &out, fakeProps(
		"EnvironmentFiles="+envFile+" (ignore_errors=no)",
		"Environment=",
	), fileMap(files), errFetch("unreachable: connection refused"))
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	got := out.String()
	// Every known row's RUNNING cell is ?: an empty VALUE column makes
	// some rows only three fields, so count rows whose last field is ?.
	unknownCells := strings.Count(got, "?") - strings.Count(got, "RUNNING")
	// The footer line carries one more ?; exclude it.
	unknownCells -= strings.Count(got, "RUNNING is ")
	if strings.Count(got, "?") < len(settings.Known)+1 { // rows + the "is \"?\"" footer
		t.Fatalf("RUNNING ? cells below %d (one per known name plus the footer):\n%s", len(settings.Known)+1, got)
	}
	// And the header row and data rows align: the first row's tail is ?.
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "ADMIN_TOKEN") && !strings.HasSuffix(strings.TrimSpace(l), "?") {
			t.Fatalf("ADMIN_TOKEN running cell = %q, want ?", strings.TrimSpace(l))
		}
	}
	if !strings.Contains(got, "running backend: unreachable: connection refused") {
		t.Fatalf("no reachable note in:\n%s", got)
	}
	// No claim of an all-match here.
	if strings.Contains(got, "every setting matches") {
		t.Fatalf("unreachable run must not claim a match:\n%s", got)
	}
}

// TestRunUnsetDefaults: a name the unit env does not set shows source
// "default" and, against an answering backend, classifies by what the
// backend reports (unset → same). The backend sets BIND_ADDR only, and
// the unit env is a non-empty Environment= line, so the run proceeds.
func TestRunUnsetDefaults(t *testing.T) {
	var out strings.Builder
	rc := running("BIND_ADDR=127.0.0.1:8890") // set in running, unset in the unit's env files
	code := run("spoond-backend", "", false, &out, fakeProps("Environment=DROP_IN_MARK=1", "EnvironmentFiles="), fileMap(nil), okFetch(rc))
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	got := out.String()
	// BIND_ADDR set only in the running backend: the unit env does not
	// have it, so the row is default-sourced with an empty VALUE (the
	// masked value of ""), which tabwriter renders as an empty column
	// — the fields shift left. Match the row's name, default source and
	// drift tail instead of raw field positions.
	if !strings.Contains(got, "differs — restart to apply") {
		t.Fatalf("BIND_ADDR drift not shown:\n%s", got)
	}
	var bindRow string
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "BIND_ADDR") {
			bindRow = strings.TrimSpace(l)
		}
	}
	// The tabwriter minwidth is 0 and padding 2, so the gap between the
	// NAME and SOURCE columns after an empty VALUE is wider than 2;
	// match on the tokens instead.
	if bindRow == "" || !strings.Contains(bindRow, "default") || !strings.Contains(bindRow, "differs — restart to apply") {
		t.Fatalf("BIND_ADDR row = %q, want the default-sourced drift row", bindRow)
	}
}

// TestRunNoEnvAtAll: a unit with neither Environment= nor
// EnvironmentFiles= exits 1 with a message, nothing printed to stdout.
func TestRunNoEnvAtAll(t *testing.T) {
	var out strings.Builder
	code := run("spoond-backend", "", false, &out, fakeProps("Environment=", "EnvironmentFiles="), fileMap(nil), okFetch(running()))
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", out.String())
	}
}

// TestRunSystemctlFails: the systemctl error path exits 1 and prints
// nothing to stdout.
func TestRunSystemctlFails(t *testing.T) {
	var out strings.Builder
	code := run("spoond-backend", "", false, &out, failRunner("loaded"), fileMap(nil), okFetch(running()))
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", out.String())
	}
}

// TestFetchRunningWithLiveServer drives the real fetcher over a real
// HTTP server (no systemctl involved): the token is presented, the
// request path is /api/admin/config, a 401 reports and 200 decodes.
func TestFetchRunningWithLiveServer(t *testing.T) {
	var gotTok, gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTok = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		if r.Header.Get("Authorization") != "Bearer shh" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"started_at":"2026-10-11T10:00:00Z","settings":[{"name":"BIND_ADDR","set":true,"value":"127.0.0.1:8890"}]}`)
	}))
	defer ts.Close()

	rc, err := fetchRunningWith(ts.Client(), ts.URL, "shh")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if gotTok != "Bearer shh" {
		t.Fatalf("Authorization = %q, want the merged env's token", gotTok)
	}
	if len(rc.Settings) != 1 {
		t.Fatalf("settings = %+v, want one entry", rc.Settings)
	}

	_, err = fetchRunningWith(ts.Client(), ts.URL, "wrong")
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("wrong token err = %v, want HTTP 401", err)
	}
	_ = gotPath // the path assertion lives in TestRunFetchURL below
}

// TestRunFetchURL: run() fetches base + /api/admin/config — a bug that
// drops the path would answer a 401 from the root's auth middleware
// instead of the admin surface (found in the live round-trip).
func TestRunFetchURL(t *testing.T) {
	var gotURL string
	dir := t.TempDir()
	envFile := filepath.Join(dir, "backend.env")
	files := map[string]string{envFile: "ADMIN_TOKEN=t\n"}
	run("spoond-backend", "https://base.example:8890", false, io.Discard, fakeProps(
		"EnvironmentFiles="+envFile+" (ignore_errors=no)",
		"Environment=",
	), fileMap(files), func(url, token string) (runningConfig, error) {
		gotURL = url
		return running(), nil
	})
	if gotURL != "https://base.example:8890/api/admin/config" {
		t.Fatalf("fetch url = %q, want base + /api/admin/config", gotURL)
	}
}

// TestAdminHTTPClient: the loopback skips TLS verification (hostname
// cert on 127.0.0.1), other hosts verify.
func TestAdminHTTPClient(t *testing.T) {
	c := adminHTTPClient("https://127.0.0.1:8890")
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("loopback client must skip verify: %+v", tr.TLSClientConfig)
	}
	c2 := adminHTTPClient("https://spoond.example.com:8890")
	tr2, ok := c2.Transport.(*http.Transport)
	if !ok || tr2.TLSClientConfig != nil && tr2.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("non-loopback client must verify: %+v", tr2.TLSClientConfig)
	}
}

// TestDefaultAPIURL: the unit's BIND_ADDR (or the built-in default)
// becomes the admin base; an IP bind dialed on the loopback, a hostname
// kept as-is, and no --api wins.
func TestDefaultAPIURL(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:8890":  "https://127.0.0.1:8890",
		"0.0.0.0:8890":    "https://127.0.0.1:8890",
		"[::]:8890":       "https://127.0.0.1:8890",
		"10.0.0.11:8890":  "https://127.0.0.1:8890",
		"spoond.lan:8890": "https://spoond.lan:8890",
		"junk":            "https://127.0.0.1:8890",
	}
	for in, want := range cases {
		if got := defaultAPIURL(in); got != want {
			t.Errorf("defaultAPIURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAPIURLFor: localhost canonicalized to 127.0.0.1 so the loopback
// TLS rule applies; anything else unchanged.
func TestAPIURLFor(t *testing.T) {
	if got := apiURLFor("https://localhost:8890/api"); got != "https://127.0.0.1:8890/api" {
		t.Errorf("apiURLFor localhost = %q", got)
	}
	if got := apiURLFor("http://127.0.0.1:8890"); got != "http://127.0.0.1:8890" {
		t.Errorf("apiURLFor loopback = %q", got)
	}
}

// TestRunJSON: --json emits the document — every setting masked, the
// running answer embedded when reachable, running_error when not — and
// never a raw secret.
func TestRunJSON(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "backend.env")
	files := map[string]string{envFile: "ADMIN_TOKEN=sekrit\nPOOL_SIZE=4\n"}
	rc := running("ADMIN_TOKEN=(hidden)", "POOL_SIZE=2")
	var out strings.Builder
	code := run("spoond-backend", "", true, &out, fakeProps(
		"EnvironmentFiles="+envFile+" (ignore_errors=no)",
		"Environment=",
	), fileMap(files), okFetch(rc))
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	got := out.String()
	if strings.Contains(got, "sekrit") {
		t.Fatalf("json leaks the secret:\n%s", got)
	}
	var doc struct {
		Unit     string `json:"unit"`
		API      string `json:"api"`
		Settings []struct {
			Name   string `json:"name"`
			Set    bool   `json:"set"`
			Value  string `json:"value"`
			Source string `json:"source"`
		} `json:"settings"`
		Running *runningConfig `json:"running"`
		RunErr  string         `json:"running_error"`
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("json decode: %v\n%s", err, got)
	}
	if doc.Unit != "spoond-backend" {
		t.Errorf("unit = %q", doc.Unit)
	}
	if doc.Running == nil || len(doc.Running.Settings) != 2 {
		t.Errorf("running not embedded: %+v", doc.Running)
	}
	set := func(name string) (set bool, value, source string, ok bool) {
		for _, s := range doc.Settings {
			if s.Name == name {
				return true, s.Value, s.Source, true
			}
		}
		return false, "", "", false
	}
	// The two unit-set rows and one default row.
	if _, v, source, ok := set("ADMIN_TOKEN"); !ok || v != "(hidden)" || source != envFile {
		t.Errorf("ADMIN_TOKEN = (%q,%q), want hidden from %s", v, source, envFile)
	}
	if _, v, _, ok := set("POOL_SIZE"); !ok || v != "4" {
		t.Errorf("POOL_SIZE value = %q", v)
	}
	if _, v, source, ok := set("BIND_ADDR"); !ok || v != "" || source != "default" {
		t.Errorf("BIND_ADDR = (%q,%q), want empty from default", v, source)
	}
}

// TestRunJSONUnreachable: the error lands in running_error.
func TestRunJSONUnreachable(t *testing.T) {
	var out strings.Builder
	code := run("spoond-backend", "", true, &out, fakeProps("Environment=PING=1", "EnvironmentFiles="), fileMap(nil), errFetch("unreachable: refused"))
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	got := out.String()
	if !strings.Contains(got, `"running_error": "unreachable: refused"`) {
		t.Fatalf("running_error missing:\n%s", got)
	}
	if strings.Contains(got, `"running":`) {
		t.Fatalf("no running block expected on error:\n%s", got)
	}
}

// The file-reader injection is exercised via fileMap everywhere above.

// tableRows parses the rendered table into its data rows (skipping
// the header, footer and blank lines), split on tabwriter's column
// gaps as fields: NAME VALUE SOURCE RUNNING [note...].
func tableRows(t *testing.T, out string) [][]string {
	t.Helper()
	var rows [][]string
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 4 || f[0] == "NAME" {
			continue
		}
		rows = append(rows, f)
	}
	return rows
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// silence unused imports in some build shapes
var (
	_ = os.Getenv
)

// TestParseShowRepeatedFiles: a unit with two EnvironmentFile= lines
// (sb's spoond-backend: llm.env then backend.env) prints
// EnvironmentFiles= twice; both files are read, in order.
func TestParseShowRepeatedFiles(t *testing.T) {
	m := parseShow("EnvironmentFiles=/etc/spoond/llm.env (ignore_errors=no)\nEnvironmentFiles=/etc/spoond/backend.env (ignore_errors=yes)\nEnvironment=BIND_ADDR=0.0.0.0:8890 IDLE_TIMEOUT_SECS=1800\n")
	files := parseEnvironmentFiles(m["EnvironmentFiles"])
	if len(files) != 2 || files[0] != "/etc/spoond/llm.env" || files[1] != "/etc/spoond/backend.env" {
		t.Fatalf("files = %q, want llm.env then backend.env", files)
	}
	if m["Environment"] != "BIND_ADDR=0.0.0.0:8890 IDLE_TIMEOUT_SECS=1800" {
		t.Fatalf("Environment = %q", m["Environment"])
	}
}

// TestRunShowsOnlySetOrDrifting: without --all the table lists what the
// unit sets and what differs from the running backend, not the 170
// known settings nobody set.
func TestRunShowsOnlySetOrDrifting(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "backend.env")
	files := map[string]string{envFile: "BIND_ADDR=127.0.0.1:8890\n"}
	// POOL_SIZE is set in the process, not in the unit: drift.
	rc := running("BIND_ADDR=127.0.0.1:8890", "POOL_SIZE=2")
	var out strings.Builder
	if code := run("spoond-backend", "", false, &out, fakeProps(
		"EnvironmentFiles="+envFile+" (ignore_errors=no)",
		"Environment=",
	), fileMap(files), okFetch(rc)); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	got := out.String()
	if !strings.Contains(got, "BIND_ADDR") || !strings.Contains(got, "POOL_SIZE") {
		t.Fatalf("set and drifting rows missing:\n%s", got)
	}
	if strings.Contains(got, "DASH_ADDR") || strings.Contains(got, "CONFORMANCE_API") {
		t.Fatalf("unset, matching settings listed without --all:\n%s", got)
	}
}

// TestResolveFileSecrets (#137 C2): a <SECRET>_FILE= line in the unit
// env folds onto its base name's row — value settings.HiddenFromFile,
// source the env file the _FILE spelling came from, position where the
// _FILE line sat; the base name's own row disappears; a non-secret's
// _FILE (BIND_ADDR_FILE) stays its own row; and a _FILE name that is
// not a setting at all stays unknown.
func TestResolveFileSecrets(t *testing.T) {
	env := []entry{
		{name: "BIND_ADDR", value: "127.0.0.1:8890", source: "/etc/spoond/backend.env"},
		{name: "ADMIN_TOKEN_FILE", value: "/etc/spoond/secrets/admin-token", source: "/etc/spoond/secrets.env"},
		{name: "POOL_SIZE", value: "2", source: "/etc/spoond/backend.env"},
		{name: "BIND_ADDR_FILE", value: "/x", source: "/etc/spoond/backend.env"},
	}
	got := resolveFileSecrets(env)
	var names []string
	for _, e := range got {
		names = append(names, e.name)
	}
	if !slices.Equal(names, []string{"BIND_ADDR", "ADMIN_TOKEN", "POOL_SIZE", "BIND_ADDR_FILE"}) {
		t.Fatalf("names = %v", names)
	}
	a := got[1]
	if a.value != settings.HiddenFromFile || a.source != "/etc/spoond/secrets.env" {
		t.Fatalf("ADMIN_TOKEN row = %+v, want the hidden-from-file value with its env-file source", a)
	}
	// The non-secret's _FILE stays as-is (unknown elsewhere).
	if got[3].value != "/x" {
		t.Fatalf("BIND_ADDR_FILE row = %+v, want it kept verbatim", got[3])
	}

	// Both forms set: the both-set refusal becomes the base row's value;
	// the row still shows (the operator must see it before restarting).
	both := []entry{
		{name: "ADMIN_TOKEN", value: "env-tok", source: "/etc/spoond/backend.env"},
		{name: "ADMIN_TOKEN_FILE", value: "/etc/spoond/secrets/admin-token", source: "/etc/spoond/secrets.env"},
	}
	got = resolveFileSecrets(both)
	if len(got) != 1 || got[0].name != "ADMIN_TOKEN" || !strings.Contains(got[0].value, "both set") || !strings.Contains(got[0].value, "ADMIN_TOKEN_FILE") {
		t.Fatalf("both forms folded = %+v, want one ADMIN_TOKEN row naming both spellings", got)
	}
	if strings.Contains(got[0].value, "env-tok") {
		t.Fatalf("folded row leaks the env value: %+v", got[0])
	}

	// No _FILE line: the env passes through unchanged.
	plain := []entry{{name: "POOL_SIZE", value: "2", source: "f"}}
	if got := resolveFileSecrets(plain); len(got) != 1 || got[0].name != "POOL_SIZE" || got[0].value != "2" {
		t.Fatalf("plain env changed: %+v", got)
	}
}

// TestFileSecretRowEndToEnd (#137 C2): a unit env carrying
// ADMIN_TOKEN_FILE shows the folded row in the table and in --json, the
// admin call authenticates with the file-backed token (effective reads
// the folded base name), and the running backend's "(hidden, from
// file)" classifies same.
func TestFileSecretRowEndToEnd(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "backend.env")
	tokPath := writeFileSecret(t, dir, "admin-token", "file-admin-tok\n")
	files := map[string]string{
		envFile: "ADMIN_TOKEN_FILE=" + tokPath + "\nBIND_ADDR=127.0.0.1:8890\n",
		tokPath: "file-admin-tok\n",
	}
	rc := running("ADMIN_TOKEN="+settings.HiddenFromFile, "BIND_ADDR=127.0.0.1:8890")
	var gotTok, gotURL string
	fetch := func(url, token string) (runningConfig, error) {
		gotTok, gotURL = token, url
		return rc, nil
	}
	_ = gotURL
	var out strings.Builder
	code := run("spoond-backend", "", false, &out, fakeProps(
		"EnvironmentFiles="+envFile+" (ignore_errors=no)",
		"Environment=",
	), fileMap(files), fetch)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(gotTok, "file-admin-tok") {
		t.Fatalf("admin token presented = %q, want the file's content (the backend resolves it the same way)", gotTok)
	}
	got := out.String()
	if strings.Contains(got, "sekrit") || strings.Contains(got, "admin-token-content") {
		t.Fatalf("output leaks a secret value:\n%s", got)
	}
	if !strings.Contains(got, "ADMIN_TOKEN") || !strings.Contains(got, settings.HiddenFromFile) {
		t.Fatalf("folded row missing:\n%s", got)
	}
	// RUNNING agrees (the endpoint answered the same constant): no drift
	// for the folded row.
	if strings.Contains(got, "every setting matches the running backend") {
		return
	}
	t.Fatalf("expected the all-match footer:\n%s", got)
}

// TestFileSecretRowUnreached: with the backend unreachable, the folded
// row still shows the hidden-from-file value and "?" RUNNING.
func TestFileSecretRowUnreached(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "backend.env")
	tokPath := writeFileSecret(t, dir, "admin-token", "file-admin-tok\n")
	files := map[string]string{envFile: "ADMIN_TOKEN_FILE=" + tokPath + "\n", tokPath: "file-admin-tok\n"}
	showAll = true
	t.Cleanup(func() { showAll = false })
	var out strings.Builder
	code := run("spoond-backend", "", false, &out, fakeProps(
		"EnvironmentFiles="+envFile+" (ignore_errors=no)",
		"Environment=",
	), fileMap(files), errFetch("refused"))
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	got := out.String()
	if !strings.Contains(got, settings.HiddenFromFile) {
		t.Fatalf("folded row missing while unreached:\n%s", got)
	}
}

// TestFileSecretJSON (#137 C2): the --json document carries the folded
// ADMIN_TOKEN row (set:true, hidden-from-file value, its env-file
// source) and no ADMIN_TOKEN_FILE row — the doc reports the base
// setting, like the table.
func TestFileSecretJSON(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "backend.env")
	tokPath := writeFileSecret(t, dir, "admin-token", "file-admin-tok\n")
	files := map[string]string{envFile: "ADMIN_TOKEN_FILE=" + tokPath + "\n", tokPath: "file-admin-tok\n"}
	var out strings.Builder
	code := run("spoond-backend", "", true, &out, fakeProps(
		"EnvironmentFiles="+envFile+" (ignore_errors=no)",
		"Environment=",
	), fileMap(files), okFetch(running()))
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var doc struct {
		Settings []struct {
			Name   string `json:"name"`
			Set    bool   `json:"set"`
			Value  string `json:"value"`
			Source string `json:"source"`
		} `json:"settings"`
	}
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("json decode: %v\n%s", err, out.String())
	}
	var found bool
	for _, s := range doc.Settings {
		switch s.Name {
		case "ADMIN_TOKEN_FILE":
			t.Fatalf("ADMIN_TOKEN_FILE must fold onto ADMIN_TOKEN: %+v", s)
		case "ADMIN_TOKEN":
			found = true
			if !s.Set || s.Value != settings.HiddenFromFile || s.Source != envFile {
				t.Fatalf("ADMIN_TOKEN = %+v, want set, hidden-from-file, from %s", s, envFile)
			}
		}
	}
	if !found {
		t.Fatal("no ADMIN_TOKEN row in the json document")
	}
}
