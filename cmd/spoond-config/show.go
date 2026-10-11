package spoondconfig

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jrimmer/spoond/v3/settings"
)

// The `spoond config show` subcommand (#137): what each setting is,
// where it came from, and whether the running backend has it. Read-only
// like the rest of the config tooling — it runs `systemctl show`, reads
// the listed env files and GETs one admin endpoint.
//
//	spoond config show [--unit spoond-backend] [--api URL] [--json]
//
// The one subcommand is "show"; `spoond config` alone prints the usage.

// Running cell values and the SOURCE names, exactly as printed.
const (
	runSame    = "same"
	runDiffers = "differs — restart to apply"
	runNotSet  = "not set in running backend"
	runSkip    = "n/a" // unknown setting: the backend does not read it
	runUnknown = "?"   // running backend unreachable
)

// runningConfig is the GET /api/admin/config body.
type runningConfig struct {
	StartedAt string `json:"started_at"`
	Settings  []struct {
		Name  string `json:"name"`
		Set   bool   `json:"set"`
		Value string `json:"value"`
	} `json:"settings"`
}

// row is one table line.
type row struct {
	name    string
	value   string // masked for display; a secret value is never printed
	source  string // file path, "unit Environment=", or "default"
	running string // a run* constant
	note    string // e.g. "(unknown setting)"
	drift   bool   // the live backend's value is not the unit's: a restart applies it
}

// DefaultBindAddr is the bind assumed when the unit env does not set
// BIND_ADDR: the backend's own built-in default.
const DefaultBindAddr = "127.0.0.1:8890"

// adminConfigPath is the endpoint the comparison fetches, appended to
// the base --api / BIND_ADDR URL.
const adminConfigPath = "/api/admin/config"

// configShowFlags is the flag set of `spoond config show`.
func configShowFlags() *flag.FlagSet {
	fs := flag.NewFlagSet("config show", flag.ExitOnError)
	fs.String("unit", "spoond-backend", "systemd unit whose environment is resolved")
	fs.String("api", "", "backend base URL (default: the unit's BIND_ADDR or https://127.0.0.1:8890)")
	fs.Bool("json", false, "machine-readable JSON output")
	fs.Bool("all", false, "list every known setting, not just the ones set or drifting")
	return fs
}

// Main is the entry point of `spoond config show`.
func Main(args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintf(os.Stderr, "usage: spoond config show [--unit spoond-backend] [--api URL] [--all] [--json]\n\n  show   each setting's value (masked), where it came from and whether the\n         running backend has it (GET /api/admin/config)\n")
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	if args[0] != "show" {
		fmt.Fprintf(os.Stderr, "spoond config: unknown subcommand %q (want show)\n", args[0])
		return 2
	}
	fs := configShowFlags()
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "spoond config: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	unit := fs.Lookup("unit").Value.String()
	apiFlag := fs.Lookup("api").Value.String()
	jsonOut := fs.Lookup("json").Value.String() == "true"
	showAll = fs.Lookup("all").Value.String() == "true"
	read := func(path string) (string, error) {
		b, err := os.ReadFile(path)
		return string(b), err
	}
	return run(unit, apiFlag, jsonOut, os.Stdout, systemctlProps, read, fetchRunning)
}

// showAll lists every known setting (--all). By default the table
// shows only what the unit sets or what differs from the running
// backend: most of the 172 known settings belong to other binaries
// (the dashboard, the runner, the conformance suite) and would bury
// the few that matter.
var showAll bool

// setOrDrifting keeps the rows a file or the unit sets, plus any whose
// value differs from the running backend.
func setOrDrifting(rows []row) []row {
	var out []row
	for _, r := range rows {
		if r.source != fromDefault || r.drift {
			out = append(out, r)
		}
	}
	return out
}

// runner is the `systemctl show` command runner (injected in tests).
type runner func(unit string, props ...string) (map[string]string, error)

// reader reads an EnvironmentFile (injected in tests).
type reader func(path string) (string, error)

// fetcher GETs the running backend's admin config (injected in tests).
type fetcher func(url, token string) (runningConfig, error)

// run resolves the unit env, asks the running backend, and prints the
// table or the --json document. It returns the exit code: 0 when the
// config is shown (an unreachable running backend still shows the unit's
// view), 1 when the unit env cannot be resolved at all.
func run(unit, apiFlag string, jsonOut bool, stdout io.Writer, sysctl runner, read reader, fetch fetcher) int {
	show, err := sysctl(unit, "EnvironmentFiles", "Environment")
	if err != nil {
		fmt.Fprintf(os.Stderr, "spoond config: systemctl show %s: %v\n", unit, err)
		return 1
	}
	// The unit merge — Environment= first, then every EnvironmentFile=
	// in order, later wins — is the env the unit hands the backend
	// process. The same merged env also supplies this command's own
	// ADMIN_TOKEN to authenticate with and BIND_ADDR to dial, so the
	// comparison is against the backend these very files start, not
	// against the caller's shell.
	env := unitEnv(show, read)
	if len(env) == 0 && show["EnvironmentFiles"] == "" && show["Environment"] == "" {
		fmt.Fprintf(os.Stderr, "spoond config: %s sets no environment\n", unit)
		return 1
	}
	// The <SECRET>_FILE= lines stay their own rows — design item 2:
	// "config show shows the path for a _FILE setting (never the
	// content)" — each with the secret's base row (hidden-from-file
	// value) beside it (resolveFileSecrets). The raw merge (rawEnv) —
	// not these display entries — supplies the admin call's own
	// ADMIN_TOKEN below and BIND_ADDR to dial, so the comparison is
	// against the backend these very files start, not the caller's
	// shell.
	rawEnv := env
	env = resolveFileSecrets(env)
	byName := map[string]entry{}
	for _, e := range env {
		byName[e.name] = e
	}
	effective := func(name, def string) string {
		// Reads the raw merge, never the resolved display env: the latter
		// carries decorated rows (a _FILE row's path, the
		// hidden-from-file constant) whose values are never a setting's
		// usable value.
		if v := mergedGet(rawEnv)(name); v != "" {
			return v
		}
		return def
	}
	apiURL := apiFlag
	if apiURL == "" {
		apiURL = defaultAPIURL(effective("BIND_ADDR", DefaultBindAddr))
	}
	// The admin token authenticates the way the backend resolves it
	// (#137 C2, settings.Get as the backend runs it): NAME= in the env,
	// else NAME_FILE= naming a value file. Both forms set, or an
	// unreadable file, is this command's own error right here rather
	// than a bogus 401 against the running backend — the display rows
	// (a decorated token row) are never sent as the bearer token.
	readValue := func(path string) ([]byte, error) {
		// The value-file read goes through the same injected reader as
		// the env files (os.ReadFile in production), so tests fake both.
		s, err := read(path)
		return []byte(s), err
	}
	adminToken, err := settings.Get("ADMIN_TOKEN", mergedGet(rawEnv), readValue)
	if err != nil {
		fmt.Fprintf(os.Stderr, "spoond config: %v\n", err)
		return 1
	}
	rc, rcErr := fetch(apiURLFor(apiURL)+adminConfigPath, adminToken)
	unreached := rcErr != nil

	if jsonOut {
		renderJSON(stdout, unit, apiURL, byName, rc, rcErr, unreached)
		return 0
	}
	rows := buildRows(byName, rc, unreached)
	if !showAll {
		rows = setOrDrifting(rows)
	}
	printTable(stdout, rows, rcErr, apiURL)
	return 0
}

// hasSpec reports whether name is a real spec (settings.Known's own
// entry, like SPOOND_ADMIN_TOKEN_FILE): a setting whose name happens to
// end in _FILE, not a secret's IsKnown-rule spelling.
func hasSpec(name string) bool {
	_, ok := settings.SpecOf(name)
	return ok
}

// buildRowsUnreached assembles the rows when the backend did not answer:
// every known row runs "?" (nothing can be classified) with the unit's
// value and source, the known secrets' _FILE spellings beside them,
// unknown keys keep their note.
func buildRowsUnreached(byName map[string]entry) []row {
	var rows []row
	for _, name := range settings.Known {
		rows = append(rows, unknownRow(name, byName, true))
	}
	var fileForms, unknown []string
	for name := range byName {
		if !settings.IsKnown(name) {
			unknown = append(unknown, name)
		} else if _, isFile := strings.CutSuffix(name, "_FILE"); isFile && !hasSpec(name) {
			fileForms = append(fileForms, name)
		}
	}
	slices.Sort(fileForms)
	slices.Sort(unknown)
	for _, name := range fileForms {
		rows = append(rows, unknownRow(name, byName, true))
	}
	for _, name := range unknown {
		rows = append(rows, unknownRow(name, byName, false))
	}
	return rows
}

func unknownRow(name string, byName map[string]entry, known bool) row {
	e, setHere := byName[name]
	src := fromDefault
	if setHere {
		src = e.source
	}
	return row{
		name:    name,
		value:   maskForDisplay(name, e.value),
		source:  src,
		running: runUnknown,
		note:    rowNote(setHere, known),
	}
}

// maskForDisplay masks a row's value the usual way (settings.HideIfSecret)
// except for the file-secret display values (#137 C2): the _FILE row's
// path is not a secret, and the decorations (hidden-from-file, a
// both-set refusal, an unset marker) are masked by construction — the
// generic rule must not strip any of them back to the bare "(hidden)".
func maskForDisplay(name, value string) string {
	if _, isFile := strings.CutSuffix(name, "_FILE"); isFile && settings.IsKnown(name) {
		// A <SECRET>_FILE row for a known secret: its value is the
		// resolved path (never the secret's content) or one of the
		// decorations — display it verbatim.
		return value
	}
	if value == settings.HiddenFromFile || refDecorated(value) {
		return value
	}
	return settings.HideIfSecret(name, value)
}

// buildRows assembles the rows: every settings.Known name in its sorted
// order — set ones with their resolved value and source, unset ones with
// source default — then any unknown key the unit env sets (sorted, with
// the "(unknown setting)" note; a typo or a removed setting, the same
// class settings.CheckEnvFile warns about) and, sorted among themselves,
// every known secret's <NAME>_FILE spelling the unit or the env
// carried (resolveFileSecrets's path row must survive to the table —
// the design's "the path for a _FILE setting"). Values are masked here
// so no rendering path can print a secret. With unreached set the
// backend did not answer: every RUNNING cell becomes "?" and nothing
// counts as drift.
func buildRows(byName map[string]entry, rc runningConfig, unreached bool) []row {
	if unreached {
		return buildRowsUnreached(byName)
	}
	run := runningValues(rc)
	var rows []row
	for _, name := range settings.Known {
		rows = append(rows, oneRow(name, byName, run, true))
	}
	var fileForms, unknown []string
	for name := range byName {
		if !settings.IsKnown(name) {
			unknown = append(unknown, name)
		} else if _, isFile := strings.CutSuffix(name, "_FILE"); isFile && !hasSpec(name) {
			// A secret's _FILE spelling from IsKnown's rule: not its own
			// spec (settings.Known carries the real _FILE-named settings,
			// like SPOOND_ADMIN_TOKEN_FILE), but the path row belongs
			// beside its base name's row.
			fileForms = append(fileForms, name)
		}
	}
	slices.Sort(fileForms)
	slices.Sort(unknown)
	for _, name := range fileForms {
		rows = append(rows, oneRow(name, byName, run, true))
	}
	for _, name := range unknown {
		rows = append(rows, oneRow(name, byName, run, false))
	}
	return rows
}

// oneRow builds the row for one setting name.
func oneRow(name string, byName map[string]entry, run map[string]runningValue, known bool) row {
	e, setHere := byName[name]
	src := fromDefault
	value := ""
	if setHere {
		src, value = e.source, e.value
	}
	r := row{
		name:   name,
		value:  maskForDisplay(name, value),
		source: src,
		note:   rowNote(setHere, known),
	}
	r.running, r.drift = classify(name, r.value, setHere, run, known)
	return r
}

// rowNote is the trailing note: a unit-set key no spoond binary reads
// is "(unknown setting)".
func rowNote(setHere, known bool) string {
	if setHere && !known {
		return "(unknown setting)"
	}
	return ""
}

// runningValue is one name's state in the running backend.
type runningValue struct {
	reported bool // the name was in the response at all
	set      bool
	value    string // as the endpoint sent it: never a raw secret
}

// runningValues indexes the running response by name.
func runningValues(rc runningConfig) map[string]runningValue {
	out := make(map[string]runningValue, len(rc.Settings))
	for _, s := range rc.Settings {
		out[s.Name] = runningValue{reported: true, set: s.Set, value: s.Value}
	}
	return out
}

// classify decides the RUNNING cell and the drift flag for one row.
// The comparison runs on the masked values: for a plain setting that is
// exact, and for a secret it can at least prove both-sides-set (the
// endpoint masks with the same rule, so neither side ever holds a raw
// value). One-sided set flags are not masked, so a secret only the unit
// or only the backend has is still classified as drift. A
// <SECRET>_FILE row does not participate beyond set-ness: the endpoint
// reports the base setting only (a file-resolved one set, with the
// same hidden-from-file constant), so the path row — set here, absent
// there — reads runNotSet without counting drift (the base name's row
// carries the comparison); a <SECRET>_FILE row standing alone (no base
// row) means the spelling is all there is, so it counts drift.
func classify(name, maskedHere string, setHere bool, run map[string]runningValue, known bool) (string, bool) {
	if !known {
		// No spoond binary reads this name; the endpoint reports known
		// names only, so there is nothing to compare against.
		return runSkip, false
	}
	if base, isFile := strings.CutSuffix(name, "_FILE"); isFile && !hasSpec(name) {
		// A secret's _FILE spelling (the rule's name, no spec of its own):
		// the endpoint never reports it. A real _FILE-named setting
		// (SPOOND_ADMIN_TOKEN_FILE) takes the plain path below.
		if setHere {
			// The _FILE spelling the operator wrote: expected — the base
			// setting (and its drift) lives one row up, where it is
			// reported. Not drift of this row itself.
			if run[base].reported {
				return runNotSet, false
			}
		}
		return runNotSet, true
	}
	rs, reported := run[name]
	switch {
	case reported && rs.set:
		if rs.value == maskedHere {
			return runSame, false
		}
		return runDiffers, true
	case reported && !rs.set:
		if setHere {
			return runNotSet, true
		}
		return runSame, false
	default:
		// Not in the response (an older backend without the name):
		// treated like unset in the running backend.
		if setHere {
			return runNotSet, true
		}
		return runSame, false
	}
}

// printTable renders the NAME / VALUE / SOURCE / RUNNING table and the
// footer. With an unreachable backend every RUNNING cell is "?" and the
// footer says so; with one that answered it counts the drift.
func printTable(stdout io.Writer, rows []row, rcErr error, apiURL string) {
	w := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tVALUE\tSOURCE\tRUNNING")
	for _, r := range rows {
		line := fmt.Sprintf("%s\t%s\t%s\t%s", r.name, r.value, r.source, r.running)
		if r.note != "" {
			line += " " + r.note
		}
		fmt.Fprintln(w, line)
	}
	w.Flush()
	if rcErr != nil {
		fmt.Fprintf(stdout, "\nrunning backend: %s (%s)\n", rcErr, apiURL)
		fmt.Fprintf(stdout, "RUNNING is %q for every row — could not compare against the live backend\n", runUnknown)
		return
	}
	drift := 0
	for _, r := range rows {
		if r.drift {
			drift++
		}
	}
	if drift > 0 {
		fmt.Fprintf(stdout, "\n%d setting(s) differ from the running backend — restart the unit to apply\n", drift)
		return
	}
	fmt.Fprintf(stdout, "\nevery setting matches the running backend\n")
}

// jsonSetting is one --json settings entry.
type jsonSetting struct {
	Name   string `json:"name"`
	Set    bool   `json:"set"`
	Value  string `json:"value"`
	Source string `json:"source"`
}

// renderJSON writes the machine-readable document: every resolved
// setting (masked), the running backend's answer when it could be
// fetched, else the error. Secrets stay masked in --json too.
func renderJSON(stdout io.Writer, unit, apiURL string, byName map[string]entry, rc runningConfig, rcErr error, unreached bool) {
	out := struct {
		Unit     string         `json:"unit"`
		API      string         `json:"api"`
		Settings []jsonSetting  `json:"settings"`
		Running  *runningConfig `json:"running,omitempty"`
		RunErr   string         `json:"running_error,omitempty"`
	}{Unit: unit, API: apiURL, Settings: []jsonSetting{}}
	for _, r := range buildRows(byName, rc, unreached) {
		out.Settings = append(out.Settings, jsonSetting{
			Name:   r.name,
			Set:    r.source != fromDefault,
			Value:  r.value,
			Source: r.source,
		})
	}
	if rcErr != nil {
		out.RunErr = rcErr.Error()
	} else {
		out.Running = &rc
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Fprintln(stdout, string(b))
}

// defaultAPIURL turns the unit's BIND_ADDR into the backend base URL.
// A host that is an IP literal (or a wildcard bind, or empty) is the
// local backend, dialed on the loopback; a hostname implies the
// deployment's TLS certificate names it. The adminHTTPClient decides
// loopback TLS trust; the scheme is always https here because the
// deployed backend defaults to TLS on 8890 (docs "Backend + orchestrator
// together" probes https://$HOST:8890). An explicit --api wins before
// this.
func defaultAPIURL(bind string) string {
	host, port, err := net.SplitHostPort(bind)
	if err != nil || port == "" || port == "0" {
		return "https://127.0.0.1:8890"
	}
	return "https://" + net.JoinHostPort(apiHost(host), port)
}

// apiHost maps the bind's host to the address the local backend is
// reachable on: a wildcard, empty or IP bind becomes the loopback; a
// hostname is kept (it may be the cert's DNS SAN).
func apiHost(host string) string {
	if host == "" || host == "0.0.0.0" || host == "::" || net.ParseIP(host) != nil {
		return "127.0.0.1"
	}
	return host
}

// apiURLFor canonicalizes the base URL before dialing: localhost is
// reached as 127.0.0.1 so the loopback TLS rule below sees one shape.
func apiURLFor(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if u.Hostname() == "localhost" && u.Port() != "" {
		u.Host = net.JoinHostPort("127.0.0.1", u.Port())
	}
	return u.String()
}

// fetchRunning GETs the admin config endpoint with the bearer token.
// export for tests.
var fetchRunning = func(url, token string) (runningConfig, error) {
	return fetchRunningWith(adminHTTPClient(url), url, token)
}

// fetchRunningWith is fetchRunning on an injected client (tests).
func fetchRunningWith(client *http.Client, url, token string) (runningConfig, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return runningConfig{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return runningConfig{}, fmt.Errorf("unreachable: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return runningConfig{}, fmt.Errorf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		return runningConfig{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(body)), 200))
	}
	var rc runningConfig
	if err := json.Unmarshal(body, &rc); err != nil {
		return runningConfig{}, fmt.Errorf("decode response: %v", err)
	}
	return rc, nil
}

// adminHTTPClient builds the client for the local admin call. TLS to
// the loopback skips verification — a deployment's certificate is for
// the hostname while the call goes to 127.0.0.1 — matching how the
// drain hook reaches the local backend; any other host verifies
// normally.
func adminHTTPClient(raw string) *http.Client {
	tr := &http.Transport{}
	if u, err := url.Parse(raw); err == nil {
		switch u.Hostname() {
		case "127.0.0.1", "::1":
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // hostname cert vs the 127.0.0.1 loopback call
		}
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: tr}
}

// truncate cuts a response body mention for a one-line error.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
