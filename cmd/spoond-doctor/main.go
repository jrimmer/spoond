// Command spoond-doctor is a dependency/connectivity checker for a spoond
// deployment. It exercises every external surface the backend depends on —
// the E2B orchestrator (health, NodeInfo), the image registry, the lease
// API listener, the SSH gateway port, the token seed, the SQLite catalog
// and its images' builds, the pinned E2B artifacts, the build storage, TLS
// material, the LLM upstream and disk — and reports a PASS/FAIL/WARN
// table with a non-zero exit when anything is failing.
//
// It reads the same environment the backend uses (SPOOND_DB_PATH, the
// E2B_* variables through e2b.FromEnv(), E2B_TEMPLATE_STORAGE_PATH,
// CONSUMER_TOKENS, BIND_ADDR, LLM_UPSTREAM_URL, LLM_UPSTREAM_KEY,
// TLS_CERT, TLS_KEY), so running `spoond doctor` on a host is a true
// reflection of the deployed backend config.
package spoonddoctor

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jrimmer/spoond/v4/install"

	"github.com/jrimmer/spoond/v4/internal/tlsfiles"
	"github.com/jrimmer/spoond/v4/notify"
	"github.com/jrimmer/spoond/v4/settings"
	"github.com/jrimmer/spoond/v4/store"
	"github.com/jrimmer/spoond/v4/substrate/e2b"

	_ "modernc.org/sqlite" // register the "sqlite" driver for the read-only probe
)

type checkResult struct {
	name   string
	status string // PASS | FAIL | WARN
	detail string
}

// doctorArtifacts are the pinned E2B artifacts with the SHA-256 values
// they must have: the table spoond install and the Docker image install
// from.
var doctorArtifacts = install.Pins()

// inContainer reports a Docker install: the image sets
// SPOOND_CONTAINER=1, and there is no systemd to ask about units.
func inContainer() bool { return os.Getenv("SPOOND_CONTAINER") == "1" }

func Main(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "machine-readable JSON output")
	manifestPath := fs.String("manifest", "", "an image manifest whose baked images must be in the catalog (optional)")
	_ = fs.Parse(args)

	results := runChecks(*manifestPath)

	if *jsonOut {
		type out struct {
			Checks []checkResult `json:"checks"`
			Pass   int           `json:"pass"`
			Fail   int           `json:"fail"`
			Warn   int           `json:"warn"`
			Ok     bool          `json:"ok"`
		}
		o := out{Checks: results, Ok: true}
		for _, r := range results {
			switch r.status {
			case "PASS":
				o.Pass++
			case "FAIL":
				o.Fail++
				o.Ok = false
			case "WARN":
				o.Warn++
			}
		}
		b, _ := json.MarshalIndent(o, "", "  ")
		fmt.Println(string(b))
	} else {
		for _, r := range results {
			fmt.Printf("%-5s %-28s %s\n", r.status, r.name, r.detail)
		}
		fail := 0
		for _, r := range results {
			if r.status == "FAIL" {
				fail++
			}
		}
		if fail > 0 {
			fmt.Printf("\n%d check(s) failing\n", fail)
		} else {
			fmt.Println("\nall checks passed")
		}
	}
	for _, r := range results {
		if r.status == "FAIL" {
			return 1
		}
	}
	return 0
}

func runChecks(manifestPath string) []checkResult {
	cfg, cfgErr := e2b.FromEnv()
	var out []checkResult
	out = append(out, checkConfig()...)
	out = append(out, checkEnvFiles(envFileDir())...)
	out = append(out, checkOrchestrator(cfg, cfgErr)...)
	out = append(out, checkRegistry()...)
	out = append(out, checkTokenSeed()...)
	out = append(out, checkDB()...)
	out = append(out, checkLeases()...)
	out = append(out, checkCatalog(manifestPath)...)
	out = append(out, checkArtifacts()...)
	out = append(out, checkStorage()...)
	out = append(out, checkBackend()...)
	out = append(out, checkGatewayPort()...)
	out = append(out, checkLLM()...)
	out = append(out, checkTLS()...)
	out = append(out, checkDisk()...)
	out = append(out, checkWebhooks()...)
	out = append(out, checkNotifyFailures()...)
	out = append(out, checkDrainUnit()...)
	return out
}

// checkConfig validates that the backend's required env is present and sane.
func checkConfig() []checkResult {
	var out []checkResult

	// Secrets resolve the backend's way (#137 C2): NAME= or NAME_FILE=.
	rawTokens, terr := settings.Get("CONSUMER_TOKENS", os.Getenv, os.ReadFile)
	tokens := strings.TrimSpace(rawTokens)
	if terr != nil {
		out = append(out, checkResult{"config: CONSUMER_TOKENS", "FAIL", terr.Error()})
	} else if tokens == "" {
		out = append(out, checkResult{"config: CONSUMER_TOKENS", "FAIL", "unset — backend refuses to start"})
	} else {
		n := len(strings.Split(tokens, ","))
		out = append(out, checkResult{"config: CONSUMER_TOKENS", "PASS", fmt.Sprintf("%d consumer(s)", n)})
	}

	bind := strings.TrimSpace(os.Getenv("BIND_ADDR"))
	if bind == "" {
		bind = "127.0.0.1:8890"
		out = append(out, checkResult{"config: BIND_ADDR", "WARN", "unset — defaulting to " + bind})
	} else {
		out = append(out, checkResult{"config: BIND_ADDR", "PASS", bind})
	}
	return out
}

// envFileDir is where spoond's systemd EnvironmentFiles live
// (SPOOND_ENV_DIR overrides it for tests and other layouts).
func envFileDir() string {
	if d := os.Getenv("SPOOND_ENV_DIR"); d != "" {
		return d
	}
	return "/etc/spoond"
}

// checkEnvFiles reports, for each spoond env file in dir (*.env), the
// keys no spoond binary reads — a typo or a setting that no longer
// exists, ignored in silence otherwise — keys set twice, where the
// last line silently wins (#137), and the <SECRET>_FILE value files
// every secret setting may be moved into (#137 C2): a missing or
// unreadable one is noted (the backend will refuse to start with it),
// and a file readable by group or others (a mode past 0600) is the leak
// the file form exists to avoid — both WARN (see "Moving a secret to a
// file" in docs/operations.md).
func checkEnvFiles(dir string) []checkResult {
	paths, _ := filepath.Glob(filepath.Join(dir, "*.env"))
	var out []checkResult
	for _, p := range paths {
		b, err := os.ReadFile(p)
		name := "config: " + filepath.Base(p)
		if err != nil {
			out = append(out, checkResult{name, "WARN", "unreadable: " + err.Error()})
			continue
		}
		unknown, dup := settings.CheckEnvFile(string(b))
		fileNotes := checkFileSecrets(string(b))
		var notes []string
		if len(unknown) > 0 {
			notes = append(notes, "not read by any spoond binary: "+strings.Join(unknown, ", "))
		}
		if len(dup) > 0 {
			notes = append(notes, "set more than once (the last wins): "+strings.Join(dup, ", "))
		}
		notes = append(notes, fileNotes...)
		// Warnings only (the task's shape for the _FILE checks, and every
		// other note here): an unreadable value file FAILs nothing by
		// itself — the backend's own startup refusal is the hard failure,
		// and doctor reports the condition for the operator to fix.
		status := "PASS"
		if len(notes) > 0 {
			status = "WARN"
		}
		if status == "PASS" {
			notes = []string{"every key is a known setting"}
		}
		out = append(out, checkResult{name, status, strings.Join(notes, "; ")})
	}
	return out
}

// checkFileSecrets checks the <SECRET>_FILE= assignments of one env
// file's content: each names a value file that must exist and be
// readable only by its owner (mode 0600 or stricter), because the
// file's content is a credential the backend will come up holding. The
// notes name the variable and the path, never a value; a non-secret's
// _FILE name (a typo like BIND_ADDR_FILE) is left to the unknown-key
// check that runs beside this. The value is parsed like an
// EnvironmentFile line (config show's parseEnvLine shape: quotes strip,
// `export ` tolerated), because systemd strips quotes and the check
// would otherwise stat a quoted path and fail a valid file. An empty
// path is noted; a file that cannot be read at all is noted too (the
// backend will refuse to start, so the note says what will fail); a
// merely loose mode is noted (the process still starts). Every note
// lands as a WARN in the table (checkEnvFiles) — the task's severity
// for this check — leaving the exit-code FAILs to the connectivity
// checks.
func checkFileSecrets(content string) []string {
	var notes []string
	plain := map[string]bool{} // secret names set as NAME= in this file
	fileForm := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		base, isFile := strings.CutSuffix(key, "_FILE")
		if !isFile || !settings.IsSecret(base) {
			if settings.IsSecret(key) && value != "" {
				plain[key] = true
			}
			continue
		}
		fileForm[base] = true
		if value == "" {
			notes = append(notes, key+": set with an empty path")
			continue
		}
		// Read, not stat: a file that exists but cannot be read fails
		// the backend's startup just the same. The content is only
		// tested for emptiness, never shown.
		b, err := os.ReadFile(value)
		if err != nil {
			notes = append(notes, key+": "+value+" is missing or unreadable ("+err.Error()+") — the backend refuses to start")
			continue
		}
		if strings.TrimSpace(string(b)) == "" {
			notes = append(notes, key+": "+value+" is empty — the backend refuses to start")
			continue
		}
		if w := settings.FileModeWarning(value); w != "" {
			notes = append(notes, key+": "+w)
		}
	}
	var both []string
	for name := range fileForm {
		if plain[name] {
			both = append(both, name+" and "+name+"_FILE are both set — the backend refuses to start")
		}
	}
	sort.Strings(both)
	return append(notes, both...)
}

// checkOrchestrator pings the orchestrator's HTTP /health and reads its
// NodeInfo: status, running sandboxes and free hugepages (WARN below
// 4 GiB).
func checkOrchestrator(cfg e2b.Config, cfgErr error) []checkResult {
	if cfgErr != nil {
		return []checkResult{
			{"orchestrator: /health", "FAIL", cfgErr.Error()},
			{"orchestrator: node info", "FAIL", cfgErr.Error()},
		}
	}
	var out []checkResult
	healthURL := "http://" + cfg.GRPCAddr + "/health"
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(healthURL)
	if err != nil {
		out = append(out, checkResult{"orchestrator: /health", "FAIL", fmt.Sprintf("%v", err)})
	} else {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && strings.Contains(string(body), `"status":"healthy"`) {
			out = append(out, checkResult{"orchestrator: /health", "PASS", "healthy"})
		} else {
			out = append(out, checkResult{"orchestrator: /health", "FAIL", fmt.Sprintf("HTTP %d %s", resp.StatusCode, body)})
		}
	}

	cl, err := e2b.New(cfg)
	if err != nil {
		out = append(out, checkResult{"orchestrator: node info", "FAIL", fmt.Sprintf("%v", err)})
		return out
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := cl.NodeInfo(ctx)
	if err != nil {
		out = append(out, checkResult{"orchestrator: node info", "FAIL", fmt.Sprintf("%v", err)})
		return out
	}
	free := info.FreeHugepageBytes()
	detail := fmt.Sprintf("status=%s running=%d hugepages free=%d GiB", info.Status, info.RunningSandboxes, free>>30)
	var warns []string
	if info.Status != "healthy" {
		warns = append(warns, "status is "+info.Status)
	}
	if free < 4<<30 {
		warns = append(warns, fmt.Sprintf("%d GiB hugepages free, want >= 4", free>>30))
	}
	if len(warns) > 0 {
		out = append(out, checkResult{"orchestrator: node info", "WARN", strings.Join(warns, "; ") + " (" + detail + ")"})
	} else {
		out = append(out, checkResult{"orchestrator: node info", "PASS", detail})
	}
	return append(out, checkOrchestratorUnit()...)
}

// checkOrchestratorUnit reads the e2b-orchestrator unit's state from
// systemd and reports where in its start-or-stop cycle it is — the
// 2026-10-12 03:08 PT incident restarted the unit while it was still
// activating (its ExecStartPost `spoond drain --start` still retrying
// resumes), systemd skipped ExecStop, so `spoond drain --stop` never ran
// and running leases were killed with no checkpoint. A unit in
// activating is the one state from which a restart or stop loses
// leases, so doctor flags it: PASS when active/running, WARN when
// activating — the detail says not to restart or stop it now, the drain
// would be skipped (docs/operations.md, Restarting the orchestrator) —
// and FAIL when the unit is failed/inactive or systemd does not answer
// (a failed orchestrator is what every connectivity check above
// already reports, but through the unit it is named as the cause).
func checkOrchestratorUnit() []checkResult {
	const name = "orchestrator: unit state"
	const activatingDetail = "start-post still running (undrain); do not restart or stop it now — the drain would be skipped"
	if inContainer() {
		return []checkResult{{name, "PASS", "Docker install: compose runs the orchestrator, and its stop drains first"}}
	}
	props, err := systemctlShow(e2bOrchestratorUnit, "ActiveState", "SubState")
	if err != nil {
		return []checkResult{{name, "FAIL", fmt.Sprintf("systemctl show %s: %v", e2bOrchestratorUnit, err)}}
	}
	state, sub := props["ActiveState"], props["SubState"]
	switch state {
	case "active":
		return []checkResult{{name, "PASS", fmt.Sprintf("active (%s)", sub)}}
	case "activating":
		return []checkResult{{name, "WARN", activatingDetail}}
	case "failed", "inactive":
		return []checkResult{{name, "FAIL", fmt.Sprintf("%s (%s) — the orchestrator is down; every create and lease call fails", state, sub)}}
	default:
		return []checkResult{{name, "WARN", fmt.Sprintf("unreported unit state %q (%q)", state, sub)}}
	}
}

// e2bOrchestratorUnit is the unit doctor reads the state of. Replaced
// in tests alongside systemctlShow.
var e2bOrchestratorUnit = "e2b-orchestrator"

// checkRegistry verifies the local container registry answers.
func checkRegistry() []checkResult {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:5000/v2/")
	if err != nil {
		return []checkResult{{"registry: /v2/", "FAIL", fmt.Sprintf("%v", err)}}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return []checkResult{{"registry: /v2/", "FAIL", fmt.Sprintf("HTTP %d", resp.StatusCode)}}
	}
	return []checkResult{{"registry: /v2/", "PASS", "http://127.0.0.1:5000/v2/"}}
}

// checkTokenSeed verifies the envd/traffic HMAC seed file: present,
// mode 0600, at least 32 bytes.
func checkTokenSeed() []checkResult {
	path := envOr("E2B_TOKEN_SEED_FILE", "/etc/spoond/e2b-token-seed")
	info, err := os.Stat(path)
	if err != nil {
		return []checkResult{{"token seed: file", "FAIL", fmt.Sprintf("%v", err)}}
	}
	if info.Mode().Perm() != 0o600 {
		return []checkResult{{"token seed: file", "FAIL", fmt.Sprintf("%s: mode %o, want 600", path, info.Mode().Perm())}}
	}
	if info.Size() < 32 {
		return []checkResult{{"token seed: file", "FAIL", fmt.Sprintf("%s: %d bytes, want >= 32", path, info.Size())}}
	}
	return []checkResult{{"token seed: file", "PASS", fmt.Sprintf("%s (%d bytes, 0600)", path, info.Size())}}
}

// checkDB verifies the SQLite store opens (read-only) and its schema is
// at migration version 4 or newer.
func checkDB() []checkResult {
	dbPath := envOr("SPOOND_DB_PATH", "/var/lib/spoond/spoond.db")
	if _, err := os.Stat(dbPath); err != nil {
		return []checkResult{{"store: database", "FAIL", fmt.Sprintf("%v", err)}}
	}
	d, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)&mode=ro")
	if err != nil {
		return []checkResult{{"store: database", "FAIL", err.Error()}}
	}
	defer d.Close()
	var version int
	if err := d.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return []checkResult{{"store: database", "FAIL", fmt.Sprintf("schema_migrations: %v", err)}}
	}
	if version < 4 {
		return []checkResult{{"store: database", "FAIL", fmt.Sprintf("migration version %d, want >= 4", version)}}
	}
	return []checkResult{{"store: database", "PASS", fmt.Sprintf("%s at migration version %d", dbPath, version)}}
}

// checkLeases reports every lease in the lost state (a lease whose
// sandbox died in a substrate crash; it answers 410 lease_lost until
// deleted).
// Purely informational — WARN with one line per lost lease naming when
// its snapshots stop being kept by the GC's grace period (a lease lost
// before lost_at was recorded has no such time until the GC stamps it)
// — and never FAIL. It reads the database read-only, like versionsInUse,
// so the doctor never creates or migrates the database it inspects.
func checkLeases() []checkResult {
	lost, err := lostLeases()
	if err != nil {
		return []checkResult{{"leases: lost", "WARN", fmt.Sprintf("list lost leases: %v", err)}}
	}
	if len(lost) == 0 {
		return []checkResult{{"leases: lost", "PASS", "none"}}
	}
	lines := make([]string, 0, len(lost))
	for _, l := range lost {
		lines = append(lines, l.String())
	}
	return []checkResult{{"leases: lost", "WARN",
		fmt.Sprintf("%d lease(s) lost: %s", len(lost), strings.Join(lines, "; "))}}
}

// lostLease is one row of the lostLeases listing.
type lostLease struct {
	ID, Owner, Image string
	Persistent       bool
	// LostAt is the stored lost_at; zero when the lease was lost before
	// the column was recorded, in which case Until is zero too: the
	// backend's GC stamps the row on its next pass and the grace period
	// runs from that stamp. Otherwise Until is the instant the grace
	// period ends.
	LostAt, Until time.Time
}

// String renders the warning line for one lost lease:
// "<id> owner=<owner> image=<image> lost <age> ago (snapshot kept until <time>)".
// A lease with an empty lost_at was lost before tracking began; the
// grace period starts at the next GC pass that stamps it, so the line
// says that instead of computing a keep-until.
func (l lostLease) String() string {
	if l.LostAt.IsZero() {
		return fmt.Sprintf("%s owner=%s image=%s lost before tracking began (grace starts at the next GC pass)",
			l.ID, l.Owner, l.Image)
	}
	return fmt.Sprintf("%s owner=%s image=%s lost %s ago (snapshot kept until %s)",
		l.ID, l.Owner, l.Image,
		duration(time.Since(l.LostAt).Round(time.Second)),
		l.Until.UTC().Format("2006-01-02 15:04 Z07:00"))
}

// lostLeaseGrace mirrors the backend's defaults: 7 days for a
// persistent lease, 1 day for any other.
const (
	lostLeaseGracePersistent = 7 * 24 * time.Hour
	lostLeaseGrace           = 24 * time.Hour
)

// lostLeases lists the lost leases from the database. A lease with an
// empty lost_at was lost before the column existed; the backend's GC
// stamps it on its next pass and the grace period runs from that stamp,
// so it is listed without a computed age or keep-until.
func lostLeases() ([]lostLease, error) {
	dbPath := envOr("SPOOND_DB_PATH", "/var/lib/spoond/spoond.db")
	d, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)&mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dbPath, err)
	}
	defer d.Close()
	rows, err := d.Query(`SELECT id, owner, image, persistent, lost_at FROM leases WHERE state = 'lost' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list leases: %w", err)
	}
	defer rows.Close()
	var out []lostLease
	for rows.Next() {
		var l lostLease
		var persistent int
		var lostAt string
		if err := rows.Scan(&l.ID, &l.Owner, &l.Image, &persistent, &lostAt); err != nil {
			return nil, fmt.Errorf("list leases: %w", err)
		}
		l.Persistent = persistent == 1
		if lostAt != "" {
			if t, err := time.Parse(time.RFC3339Nano, lostAt); err == nil {
				l.LostAt = t
				grace := lostLeaseGrace
				if l.Persistent {
					grace = lostLeaseGracePersistent
				}
				l.Until = l.LostAt.Add(grace)
			}
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list leases: %w", err)
	}
	return out, nil
}

// duration renders d the way Go prints durations, with 0s for zero.
func duration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.String()
}

// checkCatalog verifies the image catalog: every image row must have a
// current build in ready state. With a manifest, its baked images must
// also have a row. spoond ships no images, so an empty catalog is a
// warning naming how to build one, not a failure.
func checkCatalog(manifestPath string) []checkResult {
	var baked []string
	if manifestPath != "" {
		m, err := loadDoctorManifest(manifestPath)
		if err != nil {
			return []checkResult{{"catalog: images", "FAIL", err.Error()}}
		}
		for _, img := range m.Images {
			if img.Baked {
				baked = append(baked, img.Name)
			}
		}
	}
	dbPath := envOr("SPOOND_DB_PATH", "/var/lib/spoond/spoond.db")
	db, err := store.Open(dbPath)
	if err != nil {
		return []checkResult{{"catalog: images", "FAIL", fmt.Sprintf("open %s: %v", dbPath, err)}}
	}
	defer db.Close()
	ctx := context.Background()
	rows, err := db.ListImages(ctx)
	if err != nil {
		return []checkResult{{"catalog: images", "FAIL", err.Error()}}
	}
	// A row whose current build is missing or not ready is a broken
	// image: a failure. A baked manifest image with no row was never
	// built on this host: a warning naming the build.
	var missing, notBuilt []string
	have := map[string]bool{}
	for _, img := range rows {
		have[img.Name] = true
		if img.CurrentBuildID == "" {
			missing = append(missing, img.Name+" (no current build)")
			continue
		}
		b, err := db.GetBuild(ctx, img.CurrentBuildID)
		if err != nil || b.State != "ready" {
			missing = append(missing, img.Name+" (current build not ready)")
		}
	}
	for _, name := range baked {
		if !have[name] {
			notBuilt = append(notBuilt, name)
		}
	}
	return catalogResult(len(rows), missing, notBuilt)
}

// catalogResult renders the catalog check over n image rows: FAIL for
// a broken build, WARN for manifest images never built here or an
// empty catalog, PASS otherwise.
func catalogResult(n int, missing, notBuilt []string) []checkResult {
	switch {
	case len(missing) > 0:
		return []checkResult{{"catalog: images", "FAIL",
			fmt.Sprintf("%d of %d image(s) lack a ready build: %s", len(missing), n, strings.Join(missing, ", "))}}
	case len(notBuilt) > 0:
		return []checkResult{{"catalog: images", "WARN",
			fmt.Sprintf("%d image(s) ready; not built on this host: %s (spoond images build <name>)", n, strings.Join(notBuilt, ", "))}}
	case n == 0:
		return []checkResult{{"catalog: images", "WARN",
			"no images in the catalog: leases need one (spoond images build; see images/README.md)"}}
	}
	return []checkResult{{"catalog: images", "PASS", fmt.Sprintf("%d image(s) have a ready build", n)}}
}

// doctorManifest is the subset of an image manifest the doctor reads.
type doctorManifest struct {
	Images []struct {
		Name  string `yaml:"name"`
		Baked bool   `yaml:"baked"`
	} `yaml:"images"`
}

func loadDoctorManifest(path string) (doctorManifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return doctorManifest{}, fmt.Errorf("read manifest: %w", err)
	}
	var m doctorManifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		return doctorManifest{}, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	return m, nil
}

// checkArtifacts verifies the pinned E2B artifacts by SHA-256, and prints
// the Firecracker and kernel versions in use (the distinct
// firecracker_version / kernel_version of non-deleted builds), so version
// directories are never removed while builds still reference them. When
// the versions cannot be read, the check drops to WARN unless a SHA-256
// mismatch already failed it.
func checkArtifacts() []checkResult {
	var bad []string
	ok := 0
	for _, a := range doctorArtifacts {
		got, err := sha256File(a.Path)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", a.Path, err))
			continue
		}
		if got != a.SHA256 {
			bad = append(bad, fmt.Sprintf("%s: sha256 %s, want %s", a.Path, got, a.SHA256))
			continue
		}
		ok++
	}
	var status, detail string
	if len(bad) > 0 {
		status, detail = "FAIL", strings.Join(bad, "; ")
	} else {
		status, detail = "PASS", fmt.Sprintf("%d/%d match", ok, len(doctorArtifacts))
	}
	fc, kernel, err := versionsInUse()
	if err != nil {
		if status == "PASS" {
			status = "WARN"
		}
		detail += fmt.Sprintf("; versions in use: unavailable (%v)", err)
	} else {
		detail += fmt.Sprintf("; versions in use: fc=[%s] kernel=[%s]",
			strings.Join(fc, ","), strings.Join(kernel, ","))
	}
	return []checkResult{{"artifacts: sha256", status, detail}}
}

// versionsInUse returns the distinct firecracker_version and
// kernel_version values of every non-deleted build, sorted. It opens the
// database read-only, like checkDB, so the doctor never creates or
// migrates the database it inspects.
func versionsInUse() (fc, kernel []string, err error) {
	dbPath := envOr("SPOOND_DB_PATH", "/var/lib/spoond/spoond.db")
	d, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)&mode=ro")
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", dbPath, err)
	}
	defer d.Close()
	rows, err := d.Query(`SELECT state, firecracker_version, kernel_version FROM builds`)
	if err != nil {
		return nil, nil, fmt.Errorf("list builds: %w", err)
	}
	defer rows.Close()
	fcSet := map[string]bool{}
	kernelSet := map[string]bool{}
	for rows.Next() {
		var state, fcv, kv string
		if err := rows.Scan(&state, &fcv, &kv); err != nil {
			return nil, nil, fmt.Errorf("list builds: %w", err)
		}
		if state == "deleted" {
			continue
		}
		if fcv != "" {
			fcSet[fcv] = true
		}
		if kv != "" {
			kernelSet[kv] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("list builds: %w", err)
	}
	for v := range fcSet {
		fc = append(fc, v)
	}
	for v := range kernelSet {
		kernel = append(kernel, v)
	}
	sort.Strings(fc)
	sort.Strings(kernel)
	return fc, kernel, nil
}

// checkStorage warns when the build storage has under 20 GiB free.
func checkStorage() []checkResult {
	dir := envOr("E2B_TEMPLATE_STORAGE_PATH", "/forkdcache/e2b/storage/templates")
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return []checkResult{{"storage: free space", "WARN", fmt.Sprintf("statfs %s: %v", dir, err)}}
	}
	free := st.Bavail * uint64(st.Bsize)
	if free < 20<<30 {
		return []checkResult{{"storage: free space", "WARN", fmt.Sprintf("%s: %d GiB free, want >= 20", dir, free>>30)}}
	}
	return []checkResult{{"storage: free space", "PASS", fmt.Sprintf("%s: %d GiB free", dir, free>>30)}}
}

// checkBackend verifies the lease API listener is up on BIND_ADDR. The
// healthz endpoint is auth-free, so this works with any consumer token.
func checkBackend() []checkResult {
	var out []checkResult
	bind := strings.TrimSpace(os.Getenv("BIND_ADDR"))
	if bind == "" {
		bind = "127.0.0.1:8890"
	}
	conn, err := net.DialTimeout("tcp", bind, 3*time.Second)
	if err != nil {
		out = append(out, checkResult{"lease API: listener", "FAIL", fmt.Sprintf("connect %s: %v", bind, err)})
		return out
	}
	conn.Close()
	out = append(out, checkResult{"lease API: listener", "PASS", bind})

	// Probe /healthz. For https, trust the local backend by loading its own
	// cert chain from disk — never skip verification (no curl -k). When the
	// bind address is a wildcard the cert SAN (hostname) won't match it, so
	// probe via a DNS SAN from the leaf cert itself (e.g. a cert hostname).
	probeHost := bind
	if h, p, err := net.SplitHostPort(bind); err == nil && (h == "" || h == "0.0.0.0" || h == "::") {
		if san := leafCertSAN(firstTLSCert()); san != "" {
			probeHost = net.JoinHostPort(san, p)
		} else if hn, err := os.Hostname(); err == nil && hn != "" {
			probeHost = net.JoinHostPort(hn, p)
		}
	}
	scheme := "http"
	client := &http.Client{Timeout: 3 * time.Second}
	if cert := firstTLSCert(); cert != "" {
		scheme = "https"
		roots, err := x509PoolFromCert(cert)
		if err != nil {
			out = append(out, checkResult{"lease API: TLS trust", "FAIL", fmt.Sprintf("%v", err)})
			return out
		}
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}
	}
	resp, err := client.Get(scheme + "://" + probeHost + "/healthz")
	if err != nil {
		out = append(out, checkResult{"lease API: /healthz", "WARN", fmt.Sprintf("%v", err)})
		return out
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	if resp.StatusCode == http.StatusOK && strings.Contains(string(body), `"status":"ok"`) {
		out = append(out, checkResult{"lease API: /healthz", "PASS", "200 ok"})
	} else {
		out = append(out, checkResult{"lease API: /healthz", "FAIL", fmt.Sprintf("HTTP %d %s", resp.StatusCode, body)})
	}
	return out
}

// checkGatewayPort verifies the SSH gateway listener (default :2222) is up.
func checkGatewayPort() []checkResult {
	addr := os.Getenv("GATEWAY_ADDR")
	if addr == "" {
		addr = "127.0.0.1:2222"
	}
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return []checkResult{{"ssh gateway: listener", "FAIL", fmt.Sprintf("connect %s: %v", addr, err)}}
	}
	conn.Close()
	return []checkResult{{"ssh gateway: listener", "PASS", addr}}
}

// checkLLM verifies the LLM gateway upstream: key presence, connectivity,
// and key validity against the OpenAI-compatible /models endpoint.
func checkLLM() []checkResult {
	var out []checkResult
	up := strings.TrimSpace(os.Getenv("LLM_UPSTREAM_URL"))
	rawKey, kerr := settings.Get("LLM_UPSTREAM_KEY", os.Getenv, os.ReadFile)
	key := strings.TrimSpace(rawKey)
	if up == "" {
		out = append(out, checkResult{"llm gateway: upstream", "WARN", "LLM_UPSTREAM_URL unset — gateway disabled"})
		return out
	}
	if kerr != nil {
		out = append(out, checkResult{"llm gateway: key", "FAIL", kerr.Error()})
		return out
	}
	if key == "" {
		out = append(out, checkResult{"llm gateway: key", "FAIL", "LLM_UPSTREAM_KEY unset with URL configured"})
	} else {
		out = append(out, checkResult{"llm gateway: key", "PASS", "present"})
	}
	if err := reachable(up); err != nil {
		out = append(out, checkResult{"llm gateway: upstream connect", "FAIL", fmt.Sprintf("%v", err)})
		return out
	}
	out = append(out, checkResult{"llm gateway: upstream connect", "PASS", up})

	// Probe /models with the key. 200 = valid key; 401/403 = reachable but
	// auth rejected; anything else = odd but reachable.
	req, _ := http.NewRequest("GET", strings.TrimRight(up, "/")+"/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		out = append(out, checkResult{"llm gateway: /models", "FAIL", fmt.Sprintf("%v", err)})
		return out
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		out = append(out, checkResult{"llm gateway: /models", "PASS", "200 — key accepted"})
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		out = append(out, checkResult{"llm gateway: /models", "FAIL", fmt.Sprintf("HTTP %d — key rejected", resp.StatusCode)})
	default:
		out = append(out, checkResult{"llm gateway: /models", "WARN", fmt.Sprintf("HTTP %d — reachable, unexpected status", resp.StatusCode)})
	}
	return out
}

// firstTLSCert is the default (first) certificate file of TLS_CERT, or ""
// when TLS is off or misconfigured (checkTLS reports that).
func firstTLSCert() string {
	pairs, err := tlsfiles.Parse(os.Getenv("TLS_CERT"), os.Getenv("TLS_KEY"))
	if err != nil || len(pairs) == 0 {
		return ""
	}
	return pairs[0].Cert
}

// checkTLS verifies every configured certificate/key pair (TLS_CERT and
// TLS_KEY may list several, internal/tlsfiles) is present and loadable,
// and warns when one expires within 14 days.
func checkTLS() []checkResult {
	pairs, err := tlsfiles.Parse(os.Getenv("TLS_CERT"), os.Getenv("TLS_KEY"))
	if err != nil {
		return []checkResult{{"tls: cert/key", "FAIL", err.Error()}}
	}
	if pairs == nil {
		return []checkResult{{"tls: cert/key", "WARN", "not configured — API will serve plain HTTP"}}
	}
	var out []checkResult
	for _, p := range pairs {
		c, err := tls.LoadX509KeyPair(p.Cert, p.Key)
		if err != nil {
			out = append(out, checkResult{"tls: " + p.Cert, "FAIL", err.Error()})
			continue
		}
		leaf, err := x509.ParseCertificate(c.Certificate[0])
		if err != nil {
			out = append(out, checkResult{"tls: " + p.Cert, "FAIL", err.Error()})
			continue
		}
		left := time.Until(leaf.NotAfter)
		detail := fmt.Sprintf("%s, expires %s", strings.Join(leaf.DNSNames, ","), leaf.NotAfter.UTC().Format("2006-01-02"))
		switch {
		case left <= 0:
			out = append(out, checkResult{"tls: " + p.Cert, "FAIL", "expired: " + detail})
		case left < 14*24*time.Hour:
			out = append(out, checkResult{"tls: " + p.Cert, "WARN", "expires soon: " + detail})
		default:
			out = append(out, checkResult{"tls: " + p.Cert, "PASS", detail})
		}
	}
	return out
}

// checkDisk reports the root filesystem fill level.
func checkDisk() []checkResult {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil {
		return []checkResult{{"disk: root", "WARN", fmt.Sprintf("statfs: %v", err)}}
	}
	if st.Blocks == 0 {
		return []checkResult{{"disk: root", "WARN", "statfs: no block info"}}
	}
	pct := float64(st.Blocks-st.Bavail) / float64(st.Blocks) * 100
	if pct > 90 {
		return []checkResult{{"disk: root", "FAIL", fmt.Sprintf("%.0f%% full", pct)}}
	}
	if pct > 75 {
		return []checkResult{{"disk: root", "WARN", fmt.Sprintf("%.0f%% full", pct)}}
	}
	return []checkResult{{"disk: root", "PASS", fmt.Sprintf("%.0f%% used", pct)}}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// sha256File returns the hex SHA-256 of the file at path.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// reachable reports whether a base URL's host:port accepts TCP connections.
// The URL may carry a path (e.g. https://ollama.com/v1) — only the host is
// dialed, with the scheme's default port.
func reachable(baseURL string) error {
	u := baseURL
	if !strings.Contains(u, "://") {
		u = "http://" + u
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return err
	}
	host := parsed.Hostname()
	port := parsed.Port()
	if port == "" {
		switch parsed.Scheme {
		case "https":
			port = "443"
		default:
			port = "80"
		}
	}
	addr := net.JoinHostPort(host, port)
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

// leafCertSAN returns the first DNS SAN of the leaf cert in certPath, or ""
// when the file is missing/unparseable. Used to pick a probe hostname that
// the backend's own cert will validate against.
func leafCertSAN(certPath string) string {
	if certPath == "" {
		return ""
	}
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		return ""
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return ""
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return ""
	}
	if len(cert.DNSNames) > 0 {
		return cert.DNSNames[0]
	}
	return ""
}

// x509PoolFromCert builds a root pool containing the given PEM cert file,
// so a local TLS probe can trust the backend's own cert without skipping
// verification.
func x509PoolFromCert(certPath string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("no certificates parsed from %s", certPath)
	}
	return pool, nil
}

// systemctlShow returns the named properties of a unit (systemctl show
// -p ...). Replaced in tests.
var systemctlShow = func(unit string, props ...string) (map[string]string, error) {
	args := []string{"show", unit}
	for _, p := range props {
		args = append(args, "-p", p)
	}
	b, err := exec.Command("systemctl", args...).Output()
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			out[k] = v
		}
	}
	return out, nil
}

// drainEnvPath is the drain hook's configuration (SPOOND_DRAIN_URL,
// SPOOND_ADMIN_TOKEN_FILE). Replaced in tests.
var drainEnvPath = "/etc/e2b/drain.env"

// checkDrainUnit verifies that a host shutdown drains leases first:
// spoond-drain.service must be enabled, ordered after both the backend
// and the orchestrator (so it stops first and starts last), and able to
// read its configuration and token. Without it a reboot loses every
// running lease (2026-10-02).
func checkDrainUnit() []checkResult {
	const name = "drain: shutdown unit"
	if inContainer() {
		return []checkResult{{name, "PASS", "Docker install: the orchestrator container drains before it stops (stop_grace_period 6m)"}}
	}
	props, err := systemctlShow("spoond-drain.service", "UnitFileState", "ActiveState", "After")
	if err != nil {
		return []checkResult{{name, "FAIL", fmt.Sprintf("systemctl show spoond-drain.service: %v", err)}}
	}
	var problems []string
	if props["UnitFileState"] != "enabled" {
		problems = append(problems, fmt.Sprintf("not enabled (%q); a reboot would lose every running lease", props["UnitFileState"]))
	}
	if props["ActiveState"] != "active" {
		problems = append(problems, fmt.Sprintf("not active (%q), so its ExecStop will not run at shutdown", props["ActiveState"]))
	}
	after := " " + props["After"] + " "
	for _, u := range []string{"spoond-backend.service", "e2b-orchestrator.service"} {
		if !strings.Contains(after, " "+u+" ") {
			problems = append(problems, "not ordered after "+u)
		}
	}
	env, err := os.ReadFile(drainEnvPath)
	if err != nil {
		problems = append(problems, fmt.Sprintf("%s: %v", drainEnvPath, err))
	} else {
		vals := map[string]string{}
		for _, line := range strings.Split(string(env), "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
				vals[k] = v
			}
		}
		if vals["SPOOND_DRAIN_URL"] == "" {
			problems = append(problems, drainEnvPath+": SPOOND_DRAIN_URL unset")
		}
		for _, f := range strings.Split(vals["SPOOND_ADMIN_TOKEN_FILE"], ",") {
			if f = strings.TrimSpace(f); f == "" {
				problems = append(problems, drainEnvPath+": SPOOND_ADMIN_TOKEN_FILE unset")
			} else if _, err := os.ReadFile(f); err != nil {
				problems = append(problems, fmt.Sprintf("token file %s: %v", f, err))
			}
		}
	}
	if len(problems) > 0 {
		return []checkResult{{name, "FAIL", strings.Join(problems, "; ") + " (deploy/e2b/spoond-drain.service; docs/operations.md, Rebooting the host)"}}
	}
	return []checkResult{{name, "PASS", "spoond-drain enabled, active, ordered after spoond-backend and e2b-orchestrator"}}
}

// checkWebhooks validates NOTIFY_WEBHOOKS (the backend's webhook
// notification config) and probes each receiver's reachability with a
// POST of the notifier's own one-line test message (`spoond notify
// test` delivers the same). Webhooks are optional, so an unset
// NOTIFY_WEBHOOKS only WARNs; a configured webhook that does not
// answer 2xx FAILs. Errors and details name webhooks by index and
// redacted scheme://host only — URLs and headers may carry secrets.
func checkWebhooks() []checkResult {
	v := strings.TrimSpace(os.Getenv("NOTIFY_WEBHOOKS"))
	if v == "" {
		return []checkResult{{"notify: webhooks", "WARN", "NOTIFY_WEBHOOKS unset — webhook notifications disabled"}}
	}
	hooks, err := notify.ParseWebhooks(v)
	if err != nil {
		return []checkResult{{"notify: webhooks", "FAIL", err.Error()}}
	}
	res := []checkResult{{"notify: webhooks", "PASS",
		fmt.Sprintf("%d webhook(s) configured", len(hooks))}}
	res = append(res, probeWebhooks(hooks)...)
	return res
}

// probeWebhooks POSTs a test message to every configured webhook, in
// parallel, and reports one reachability check per webhook: PASS on a
// 2xx, FAIL otherwise, with the redacted error. Reuses the notifier's
// sender, so the payload, headers and redaction rules are the ones
// delivery uses.
func probeWebhooks(hooks []notify.Webhook) []checkResult {
	results := notify.SendTest(context.Background(), hooks, webhookProbeTimeout, nil)
	out := make([]checkResult, 0, len(results))
	for _, r := range results {
		name := fmt.Sprintf("notify: webhook %d reachability", r.Webhook)
		if r.Err == nil {
			out = append(out, checkResult{name, "PASS", r.Redacted + " answered"})
			continue
		}
		out = append(out, checkResult{name, "FAIL", r.Redacted + ": " + r.Err.Error()})
	}
	return out
}

// webhookProbeTimeout bounds one webhook's reachability probe (the
// whole exchange: connect, request, response).
const webhookProbeTimeout = 5 * time.Second

// checkNotifyFailures reports the webhook deliveries the backend gave
// up on in the last 24 hours, from the notifier's state file
// (NOTIFY_STATE_FILE, default next to the database). Webhooks are
// named by index; recorded errors are already redacted by the backend.
// A missing state file means nothing was ever dropped.
func checkNotifyFailures() []checkResult {
	path := envOr("NOTIFY_STATE_FILE", filepath.Join(
		filepath.Dir(envOr("SPOOND_DB_PATH", "/var/lib/spoond/spoond.db")), "notify-state.json"))
	fails, err := notify.LoadFailures(path, time.Now())
	if err != nil {
		return []checkResult{{"notify: dropped deliveries", "WARN",
			fmt.Sprintf("read %s: %v", filepath.Base(path), err)}}
	}
	if len(fails) == 0 {
		return []checkResult{{"notify: dropped deliveries", "PASS", "none in the last 24 h"}}
	}
	byHook := map[int]int{}
	for _, f := range fails {
		byHook[f.Webhook]++
	}
	parts := make([]string, 0, len(byHook))
	for hook, n := range byHook {
		parts = append(parts, fmt.Sprintf("webhook %d: %d dropped", hook, n))
	}
	sort.Strings(parts)
	return []checkResult{{"notify: dropped deliveries", "WARN",
		fmt.Sprintf("%d in the last 24 h (%s; %s)", len(fails), strings.Join(parts, ", "), filepath.Base(path))}}
}
