// The runtime phase: the E2B substrate's binaries, pinned artifacts and
// orchestrator configuration. It replaces deploy/e2b/host-setup.sh
// step 5 (the artifacts) and step 7 (the host firewall) and
// docs/install.md section 2 (the orchestrator configuration) — the
// plan's runtime row (docs/plans/2026-10-16-4.0-install.md).
package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jrimmer/spoond/v3/deploy"
)

// What the phase writes and creates, every path resolved under
// Env.Root.
const (
	orchestratorBinPath  = "/usr/local/lib/e2b/orchestrator" // the fork orchestrator binary
	envdBinPath          = "/fc-envd/envd"                   // envd, the guest agent
	runtimeStampPath     = "/usr/local/lib/e2b/RUNTIME"      // the stamp build-runtime.sh writes
	fcVMPath             = "/fc-vm"                          // the orchestrator's sandbox directory
	orchestratorEnvPath  = "/etc/e2b/orchestrator.env"       // operator-editable config
	flagsJSONPath        = "/etc/e2b/flags.json"             // operator-editable feature flags
	spoondDirPath        = "/etc/spoond"                     // where every secret lives
	tokenSeedPath        = "/etc/spoond/e2b-token-seed"      // the envd/traffic HMAC seed
	guardNftPath         = "/etc/nftables.d/e2b-guard.nft"   // the nftables table
	guardUnitPath        = "/etc/systemd/system/e2b-guard.service"
	orchestratorUnitPath = "/etc/systemd/system/e2b-orchestrator.service"

	orchestratorShipped    = "e2b/e2b-orchestrator.service" // the names in deploy.Files
	orchestratorEnvShipped = "e2b/orchestrator.env"
	flagsShipped           = "e2b/flags.json"
	guardNftShipped        = "e2b/e2b-guard.nft"
	guardUnitShipped       = "e2b/e2b-guard.service"

	orchestratorUnit = "e2b-orchestrator" // the systemctl unit names
	guardUnit        = "e2b-guard"
)

// runtimeHealthURL is the orchestrator's health endpoint; the phase
// polls it once the orchestrator is (re)started.
const runtimeHealthURL = "http://127.0.0.1:5008/health"

// Health poll bounds: an orchestrator that has not answered healthy in
// 120 s is not coming up. healthPollEvery re-checks every 2 s (tests
// shorten both, so a failing-health run is quick).
const (
	healthDeadline    = 120 * time.Second
	healthPollEvery   = 2 * time.Second
	healthHTTPTimeout = 3 * time.Second
)

// runtimeHTTPTimeout bounds one HTTP fetch whole: a stuck download
// must not hang the install.
const runtimeHTTPTimeout = 5 * time.Minute

// devBuildHint is the failure of a development build with neither
// --runtime nor a version to download: the operator builds the
// artifact locally and passes it.
const devBuildHint = "development build: pass --runtime spoond-runtime_<v>_linux_amd64.tar.gz (deploy/release/build-runtime.sh builds one)"

// Fetcher downloads a URL's bytes whole: the runtime tarball, its
// SHA256SUMS and the pinned artifacts. Production is httpFetcher
// (net/http with a timeout); a test stubs runtimeState.Fetch so no
// network is touched.
type Fetcher interface {
	Fetch(ctx context.Context, url string) ([]byte, error)
}

// HealthChecker asks the orchestrator's /health once and reports nil
// when it answered healthy. Production is httpHealth (net/http); a
// test stubs runtimeState.Health so no port is bound.
type HealthChecker interface {
	Check(ctx context.Context, url string) error
}

// httpFetcher is the production Fetcher: a GET whose whole request is
// bounded by a timeout, the body read into memory (the caller checks
// its sha256 before any write, so no partial file ever lands), a
// non-200 an error naming URL and status.
type httpFetcher struct {
	// Timeout is the whole-request bound; zero means the default.
	Timeout time.Duration
}

// Fetch implements Fetcher with net/http.
func (h httpFetcher) Fetch(ctx context.Context, url string) ([]byte, error) {
	timeout := h.Timeout
	if timeout == 0 {
		timeout = runtimeHTTPTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: read: %w", url, err)
	}
	return b, nil
}

// httpHealth asks /health once: HTTP 200 and a body naming itself
// healthy is a pass; anything else is a failed poll (the phase
// retries until its deadline).
type httpHealth struct{}

// Check implements HealthChecker with net/http.
func (httpHealth) Check(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: healthHTTPTimeout}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if !bytes.Contains(body, []byte(`"status":"healthy"`)) {
		return fmt.Errorf("answered, not healthy: %s", bytes.TrimSpace(body))
	}
	return nil
}

// artifactBucket is the base of every pinned URL (the B= variable of
// deploy/e2b/host-setup.sh).
const artifactBucket = "https://storage.googleapis.com/e2b-artifact-binaries"

// pinnedArtifact is one artifact the orchestrator starts a sandbox
// from, fetched from E2B's public bucket: URL, host path, mode and the
// sha256 pinning the content.
type pinnedArtifact struct {
	url    string
	path   string
	mode   os.FileMode
	sha256 string
}

// pinned is the pin table: the exact URLs, paths, modes and sha256 of
// deploy/e2b/host-setup.sh (the pinned Firecracker, guest kernel and
// busybox). TestPinnedTableMatchesHostSetup reads that script and
// requires every row to match it, so the table and the operator
// script cannot drift apart. Order is install order.
var pinned = []pinnedArtifact{
	{
		url:    artifactBucket + "/firecrackers/v1.14-0.2.0/amd64/firecracker",
		path:   "/fc-versions/v1.14-0.2.0/amd64/firecracker",
		mode:   0o755,
		sha256: "ef22aec7cbffcf6cc44a8436a4db79f9e6fe5c52218c81af321dd20f10ad6e5d",
	},
	{
		url:    artifactBucket + "/kernels/vmlinux-6.1.177_5008931/amd64/vmlinux.bin",
		path:   "/fc-kernels/vmlinux-6.1.177_5008931/amd64/vmlinux.bin",
		mode:   0o644,
		sha256: "9191ced12d24e6e381753a7ab12ec850877524d453eae75c20aeb176f3b5ad05",
	},
	{
		url:    artifactBucket + "/busybox/1.36.1/amd64/busybox",
		path:   "/fc-busybox/1.36.1/amd64/busybox",
		mode:   0o755,
		sha256: "d7cce939adb09a41a22a5f846d22ba8d576b38dbb2b46a5c77a3a3e27ec52520",
	},
}

// runtimeState is the phase: its two network surfaces are pluggable so
// the tests fake them (the production net/http fetcher and health
// check are the defaults). The phase is one package-level value, like
// the phases table itself; the tests stub it and restore it.
type runtimeState struct {
	// Fetch is nil in production (httpFetcher); a test swaps in a
	// fake fetcher that answers from a table and records the calls.
	Fetch Fetcher
	// Health is nil in production (httpHealth); a test swaps in a
	// fake health check.
	Health HealthChecker
	// pollEvery is the delay between health checks; zero means the
	// production 2 s.
	pollEvery time.Duration
	// deadline bounds the whole health wait; zero means 120 s.
	deadline time.Duration
}

// runtimeImpl is the registered phase value; runtime_test.go stubs its
// fields for one test at a time (tests in this package never run in
// parallel).
var runtimeImpl = runtimeState{}

// run is the phase body, in the plan's order: the runtime binaries,
// the pinned artifacts, /fc-vm, the e2b config, the token seed, the
// firewall, the orchestrator unit, systemd (daemon-reload, enable,
// restart) and the health wait. Every added row, the systemd step's
// included, is in the returned items (r.Items() is read after the
// last step ran, so no row is lost).
func runtimePhase(ctx context.Context, env *Env) ([]Item, error) {
	return runtimeImpl.run(ctx, env)
}

func init() { Phases[PhaseRuntime] = runtimePhase }

func (p *runtimeState) run(ctx context.Context, env *Env) ([]Item, error) {
	r := &Reporter{Phase: PhaseRuntime, Out: env.Out}

	if err := p.installBinaries(r, ctx, env); err != nil {
		return r.Items(), err
	}
	for _, art := range pinned {
		if err := p.installPinned(r, ctx, env, art); err != nil {
			return r.Items(), err
		}
	}
	if err := mkdirMode(r, env, fcVMPath, 0o755); err != nil {
		return r.Items(), err
	}
	for _, c := range operatorConfigs() {
		if err := installOperatorConfig(r, env, c); err != nil {
			return r.Items(), err
		}
	}
	if err := installSeed(r, env); err != nil {
		return r.Items(), err
	}
	if err := installGuard(r, env); err != nil {
		return r.Items(), err
	}
	if err := installOrchestratorUnit(r, env); err != nil {
		return r.Items(), err
	}
	// Last: the changed set above decides restart and the health wait.
	// The rows land in r before Items() is read, so none is lost.
	err := p.orchestration(r, ctx, env)
	return r.Items(), err
}

// fetch runs the phase's fetcher (production or test fake).
func (p *runtimeState) fetch(ctx context.Context, url string) ([]byte, error) {
	if p.Fetch != nil {
		return p.Fetch.Fetch(ctx, url)
	}
	return httpFetcher{}.Fetch(ctx, url)
}

// downloadName is the release tarball's file name for a version: the
// tag with its leading v ("spoond-runtime_4.0.0_linux_amd64.tar.gz"
// for v4.0.0).
func downloadName(version string) string {
	return "spoond-runtime_" + strings.TrimPrefix(version, "v") + "_linux_amd64.tar.gz"
}

// installBinaries puts the orchestrator, envd and the RUNTIME stamp on
// the host: from --runtime, from the release tarball that matches the
// binary's own version, or — a development build — not at all.
func (p *runtimeState) installBinaries(r *Reporter, ctx context.Context, env *Env) error {
	if file := env.Opt.Runtime; file != "" {
		orch, envd, stamp, err := localRuntime(file)
		if err != nil {
			return err
		}
		return writeRuntime(r, env, orch, envd, stamp)
	}
	if env.Version == "" {
		return errors.New(devBuildHint)
	}
	if env.DryRun {
		planRelease(r, env)
		return nil
	}
	orch, envd, stamp, err := p.releaseRuntime(ctx, env)
	if err != nil {
		return err
	}
	return writeRuntime(r, env, orch, envd, stamp)
}

// planRelease reports what a real run would download and install, in a
// dry run, without fetching: one row per target, decided by what is
// already on disk.
func planRelease(r *Reporter, env *Env) {
	name := downloadName(env.Version)
	note := "would download " + name + ", check its line in SHA256SUMS and install (dry-run fetches nothing)"
	for _, path := range []string{orchestratorBinPath, envdBinPath, runtimeStampPath} {
		if _, err := os.Stat(env.Resolve(path)); err == nil {
			r.Add(Kept, path, note+" — but it is here and would be compared first")
			continue
		}
		r.Add(Created, path, note)
	}
}

// writeRuntime installs the three runtime members, each compared by
// content so identical binaries are kept.
func writeRuntime(r *Reporter, env *Env, orchestrator, envd, stamp []byte) error {
	if err := env.writeFile(r, orchestratorBinPath, orchestrator, 0o755); err != nil {
		return err
	}
	if err := env.writeFile(r, envdBinPath, envd, 0o755); err != nil {
		return err
	}
	return env.writeFile(r, runtimeStampPath, stamp, 0o644)
}

// releaseRuntime downloads the release tarball for env.Version and its
// SHA256SUMS, checks the tarball's line in SHA256SUMS, and untars the
// members. Both downloads land in memory first, so a wrong release
// writes nothing.
func (p *runtimeState) releaseRuntime(ctx context.Context, env *Env) (orchestrator, envd, stamp []byte, err error) {
	name := downloadName(env.Version)
	base := "https://github.com/jrimmer/spoond/releases/download/" + env.Version
	tgz, err := p.fetch(ctx, base+"/"+name)
	if err != nil {
		return nil, nil, nil, err
	}
	sums, err := p.fetch(ctx, base+"/SHA256SUMS")
	if err != nil {
		return nil, nil, nil, err
	}
	if !sumsList(sums, sha256sum(tgz)) {
		return nil, nil, nil, fmt.Errorf("SHA256SUMS: the %s tarball's sha256 is not in it — the release is broken or mid-publish; retry, or pin a --runtime tarball", name)
	}
	return unpackRuntime(tgz, "the release tarball")
}

// localRuntime reads the --runtime tarball, checks the .sha256 beside
// it when it exists (build-runtime.sh writes one), and untars the
// members. The tarball is read into memory first, so a wrong checksum
// writes nothing.
func localRuntime(file string) (orchestrator, envd, stamp []byte, err error) {
	tgz, err := os.ReadFile(file)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read --runtime %s: %w", file, err)
	}
	if want, err := os.ReadFile(file + ".sha256"); err == nil {
		if got := sha256sum(tgz); !sumsList(want, got) {
			return nil, nil, nil, fmt.Errorf("--runtime %s.sha256: sha256 mismatch (the tarball is %s)", file, got)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil, fmt.Errorf("read --runtime %s.sha256: %w", file, err)
	}
	return unpackRuntime(tgz, "the --runtime tarball")
}

// sumsList reports whether sums — a SHA256SUMS or .sha256 file of
// "<hex> <whitespace> <name>" lines, or a bare hex digest — names want.
func sumsList(sums []byte, want string) bool {
	if strings.TrimSpace(string(sums)) == want {
		return true
	}
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) > 0 && strings.EqualFold(fields[0], want) {
			return true
		}
	}
	return false
}

// unpackRuntime untars the three members out of the in-memory tarball:
// spoond-runtime/{orchestrator,envd,RUNTIME}, matched by base name.
func unpackRuntime(tgz []byte, name string) (orchestrator, envd, stamp []byte, err error) {
	zr, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s: gzip: %w", name, err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%s: tar: %w", name, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		read := func() ([]byte, error) {
			b, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("%s: read %s: %w", name, hdr.Name, err)
			}
			return b, nil
		}
		switch filepath.Base(hdr.Name) {
		case "orchestrator":
			orchestrator, err = read()
		case "envd":
			envd, err = read()
		case "RUNTIME":
			stamp, err = read()
		}
		if err != nil {
			return nil, nil, nil, err
		}
	}
	if orchestrator == nil || envd == nil || stamp == nil {
		return nil, nil, nil, fmt.Errorf("%s: want the spoond-runtime/{orchestrator,envd,RUNTIME} members", name)
	}
	return
}

// sha256sum is the hex sha256 of b.
func sha256sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// installPinned puts one pinned artifact on the host: a present file
// with the right sha256 is kept and not downloaded; anything else is
// fetched, verified against the pin before any write, and installed
// (a bad fetch leaves the previous file and reports failed).
func (p *runtimeState) installPinned(r *Reporter, ctx context.Context, env *Env, art pinnedArtifact) error {
	full := env.Resolve(art.path)
	exists := true
	if old, err := os.ReadFile(full); err == nil {
		if sha256sum(old) == art.sha256 {
			r.Add(Kept, art.path, "sha256 matches the pin")
			return nil
		}
		// Wrong content: the fetch below replaces it, checked first.
	} else {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read %s: %w", art.path, err)
		}
		exists = false
	}
	if env.DryRun {
		if exists {
			r.Add(Skipped, art.path, "differs from the pin; a real run would re-fetch and verify (dry-run fetches nothing)")
		} else {
			r.Add(Created, art.path, "would fetch the pin and verify its sha256 (dry-run fetches nothing)")
		}
		return nil
	}
	b, err := p.fetch(ctx, art.url)
	if err != nil {
		note := "fetch failed: " + err.Error()
		if exists {
			note += "; the file in place does not match the pin and is left as it is"
		}
		r.Add(Failed, art.path, note)
		return err
	}
	if got := sha256sum(b); got != art.sha256 {
		msg := fmt.Errorf("%s: fetched %s, the pin wants %s — the pinned URL is broken or republished; pin a known version and fix the table in install/runtime.go", art.path, got, art.sha256)
		r.Add(Failed, art.path, msg.Error())
		return msg
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(full), err)
	}
	// WriteFile applies the mode only on create; chmod fixes the mode
	// of an overwritten file too.
	if err := os.WriteFile(full, b, art.mode); err != nil {
		return fmt.Errorf("write %s: %w", full, err)
	}
	if err := os.Chmod(full, art.mode); err != nil {
		return fmt.Errorf("chmod %s: %w", full, err)
	}
	if exists {
		r.Add(Changed, art.path, "re-fetched and verified against the pin")
	} else {
		r.Add(Created, art.path, "fetched, sha256 verified against the pin")
	}
	return nil
}

// operatorConfig is one shipped config file the operator may edit:
// written when absent, kept (with a compare note) when their edit
// differs.
type operatorConfig struct {
	shipped string // the file's name in deploy.Files
	path    string // its installed host path
}

// operatorConfigs lists the operator-editable configs the phase writes.
func operatorConfigs() []operatorConfig {
	return []operatorConfig{
		{shipped: orchestratorEnvShipped, path: orchestratorEnvPath},
		{shipped: flagsShipped, path: flagsJSONPath},
	}
}

// installOperatorConfig writes one operator-editable config: the
// shipped file with the store dir substituted, 0600, compared before it
// writes so the operator's edit is never overwritten.
func installOperatorConfig(r *Reporter, env *Env, c operatorConfig) error {
	b, err := shippedWithStore(c.shipped, env.Opt.storeDir())
	if err != nil {
		return err
	}
	old, err := os.ReadFile(env.Resolve(c.path))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", c.path, err)
	}
	if err == nil && !bytes.Equal(old, b) {
		r.Add(Kept, c.path, fmt.Sprintf("differs from the shipped file; compare with deploy/%s", c.shipped))
		return nil
	}
	return env.writeFile(r, c.path, b, 0o600)
}

// shipped reads a file out of the embedded deploy.Files.
func shipped(name string) ([]byte, error) {
	b, err := deploy.Files.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("shipped %s: %w", name, err)
	}
	return b, nil
}

// shippedWithStore reads a shipped file with every store-dir
// occurrence substituted: /forkdcache/e2b becomes the operator's
// --store-dir.
func shippedWithStore(name, store string) ([]byte, error) {
	b, err := shipped(name)
	if err != nil {
		return nil, err
	}
	if store == "" || store == DefaultStoreDir {
		return b, nil
	}
	return bytes.ReplaceAll(b, []byte(DefaultStoreDir), []byte(store)), nil
}

// installSeed makes /etc/spoond (0700) and writes the token seed once:
// 32 random bytes as 64 hex chars, through writeFileKeep — never read
// back, never printed.
func installSeed(r *Reporter, env *Env) error {
	if err := mkdirMode(r, env, spoondDirPath, 0o700); err != nil {
		return err
	}
	seed, err := seedValue()
	if err != nil {
		return err
	}
	return env.writeFileKeep(r, tokenSeedPath, seed)
}

// seedValue is a fresh seed: 32 random bytes hex-encoded to 64 chars,
// with a trailing newline (openssl rand -hex 32's shape in
// docs/install.md; the readers TrimSpace it off).
func seedValue() ([]byte, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand reads the kernel's randomness; a failure leaves
		// no seed a safe install could continue with.
		return nil, fmt.Errorf("crypto/rand for the token seed: %w", err)
	}
	return []byte(hex.EncodeToString(raw[:]) + "\n"), nil
}

// installGuard writes the host firewall: the nftables table and its
// oneshot unit, both shipped verbatim (plain system config, so drift
// from the shipped files cannot hide).
func installGuard(r *Reporter, env *Env) error {
	if err := installShipped(r, env, guardNftShipped, guardNftPath); err != nil {
		return err
	}
	return installShipped(r, env, guardUnitShipped, guardUnitPath)
}

// installShipped writes one shipped file verbatim at 0644.
func installShipped(r *Reporter, env *Env, name, path string) error {
	b, err := shipped(name)
	if err != nil {
		return err
	}
	return env.writeFile(r, path, b, 0o644)
}

// installOrchestratorUnit installs the orchestrator unit: the shipped
// unit without its drain lines (the hooks need /etc/e2b/drain.env and
// /opt/spoond/spoond, which the spoond phase installs later). An
// installed unit that already equals the full shipped unit — after a
// spoond phase — is kept as is.
func installOrchestratorUnit(r *Reporter, env *Env) error {
	fullUnit, err := shippedWithStore(orchestratorShipped, env.Opt.storeDir())
	if err != nil {
		return err
	}
	light := dropDrainLines(fullUnit)
	old, err := os.ReadFile(env.Resolve(orchestratorUnitPath))
	switch {
	case err == nil && bytes.Equal(old, fullUnit):
		r.Add(Kept, orchestratorUnitPath, "matches the shipped unit, drain hooks included (the spoond phase installed it)")
		return nil
	case err == nil && bytes.Equal(old, light):
		r.Add(Kept, orchestratorUnitPath, "")
		return nil
	case err == nil || errors.Is(err, os.ErrNotExist):
		return env.writeFile(r, orchestratorUnitPath, light, 0o644)
	default:
		return fmt.Errorf("read %s: %w", orchestratorUnitPath, err)
	}
}

// dropDrainLines returns the unit without every line mentioning drain —
// the hooks and the whole comment block that documents them — and
// collapses the run of blank lines their removal leaves. It is
// docs/install.md section 2's `grep -v drain`, so the installed unit
// keeps the shipped shape.
func dropDrainLines(unit []byte) []byte {
	var kept [][]byte
	for _, line := range bytes.Split(unit, []byte("\n")) {
		if bytes.Contains(bytes.ToLower(line), []byte("drain")) {
			continue
		}
		kept = append(kept, line)
	}
	// Collapse the blanks the dropped lines left between lines that
	// survived.
	var out []string
	blanks := 0
	for _, line := range kept {
		if len(bytes.TrimSpace(line)) == 0 {
			blanks++
			continue
		}
		if blanks > 0 && len(out) > 0 {
			out = append(out, "")
		}
		blanks = 0
		out = append(out, string(line))
	}
	return []byte(strings.Join(out, "\n") + "\n")
}

// orchestration is the systemd step: daemon-reload when a unit file
// changed; enable --now when a unit changed; a restart only when the
// orchestrator's own surface (its unit, env, flags or binary) changed
// this run and it was already active; then the health wait. A dry run
// plans the restart and runs nothing.
func (p *runtimeState) orchestration(r *Reporter, ctx context.Context, env *Env) error {
	changed := changedSet(r.Items())
	anyUnitChanged := changed[guardUnitPath] || changed[orchestratorUnitPath]
	orchChanged := changed[orchestratorUnitPath] || changed[orchestratorEnvPath] ||
		changed[flagsJSONPath] || changed[orchestratorBinPath]

	// A read-only query, allowed in a dry run: the restart plan needs
	// its answer. Queried before enable --now, so "not running" means
	// enable --now starts it (with whatever files are on disk).
	active := orchestratorActive(ctx, env)

	if env.DryRun {
		p.planOrchestratorRestart(r, orchChanged, active)
		return nil
	}
	if anyUnitChanged {
		if err := env.exec(ctx, "systemctl", "daemon-reload"); err != nil {
			return fmt.Errorf("systemctl daemon-reload: %w", err)
		}
	}
	// A first install puts the units in place; a stopped orchestrator
	// with unchanged units must be started too, or the health wait
	// below fails. enable --now leaves a running unit alone.
	if anyUnitChanged || !active {
		if err := env.exec(ctx, "systemctl", "enable", "--now", guardUnit, orchestratorUnit); err != nil {
			return fmt.Errorf("systemctl enable --now %s %s: %w", guardUnit, orchestratorUnit, err)
		}
	}
	// The guard is a oneshot that loads the table at start: new rules
	// take effect only when it runs again.
	if changed[guardNftPath] || changed[guardUnitPath] {
		if err := env.exec(ctx, "systemctl", "restart", guardUnit); err != nil {
			return fmt.Errorf("systemctl restart %s: %w", guardUnit, err)
		}
		r.Add(Changed, "restart "+guardUnit, "its table or unit changed")
	}
	if orchChanged {
		if active {
			if err := env.exec(ctx, "systemctl", "restart", orchestratorUnit); err != nil {
				return fmt.Errorf("systemctl restart %s: %w", orchestratorUnit, err)
			}
			r.Add(Changed, "restart "+orchestratorUnit, "it was running, and its unit, env, flags or binary changed")
		} else {
			r.Add(Kept, "restart "+orchestratorUnit, "not running; enable --now starts it with the new files")
		}
	}
	return p.waitHealth(r, ctx, env)
}

// planOrchestratorRestart reports what systemd would do to the
// orchestrator, in a dry run.
func (p *runtimeState) planOrchestratorRestart(r *Reporter, orchChanged, active bool) {
	if !orchChanged {
		return
	}
	if active {
		r.Add(Skipped, "restart "+orchestratorUnit, "dry-run: it is running and its unit, env, flags or binary changed; a real run would restart it")
		return
	}
	r.Add(Skipped, "restart "+orchestratorUnit, "dry-run: it is not running; a real run would start it with enable --now")
}

// waitHealth polls the orchestrator's /health until it answers healthy
// or the deadline passes; the failure hands the operator the journal
// command. A cancelled context stops the wait instead of polling on.
func (p *runtimeState) waitHealth(r *Reporter, ctx context.Context, env *Env) error {
	deadline := time.Now().Add(p.pollDeadline())
	var last error
	for {
		if err := ctx.Err(); err != nil {
			// Stopped between polls.
			last = err
			break
		}
		last = p.health().Check(ctx, runtimeHealthURL)
		if last == nil {
			r.Add(Kept, "orchestrator /health", runtimeHealthURL+" answers healthy")
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		if err := ctx.Err(); err != nil {
			// Stopped while the poll was in flight.
			last = err
			break
		}
		select {
		case <-ctx.Done():
			last = ctx.Err()
		case <-time.After(p.pollGap()):
		}
	}
	msg := fmt.Errorf("the orchestrator is not healthy: journalctl -u %s -n 50 (last check: %v)", orchestratorUnit, last)
	r.Add(Failed, "orchestrator /health", runtimeHealthURL+": "+last.Error())
	return msg
}

// health returns the production or the test health checker.
func (p *runtimeState) health() HealthChecker {
	if p.Health != nil {
		return p.Health
	}
	return httpHealth{}
}

// pollGap is the delay between health checks.
func (p *runtimeState) pollGap() time.Duration {
	if p.pollEvery > 0 {
		return p.pollEvery
	}
	return healthPollEvery
}

// pollDeadline is the bound on the whole health wait.
func (p *runtimeState) pollDeadline() time.Duration {
	if p.deadline > 0 {
		return p.deadline
	}
	return healthDeadline
}

// orchestratorActive asks systemd whether the orchestrator is running:
// "active" is the only yes; a missing unit, "inactive" or any error is
// no.
func orchestratorActive(ctx context.Context, env *Env) bool {
	out, err := env.query(ctx, "systemctl", "is-active", orchestratorUnit)
	return err == nil && strings.TrimSpace(string(out)) == "active"
}

// changedSet collects the paths the run created or changed, by report
// name.
func changedSet(items []Item) map[string]bool {
	changed := map[string]bool{}
	for _, it := range items {
		if it.Result == Created || it.Result == Changed {
			changed[it.Name] = true
		}
	}
	return changed
}

// mkdirMode creates dir with an exact mode, or fixes an existing
// directory's mode (/fc-vm 0755; /etc/spoond 0700, a secret dir the
// process umask must not open up).
func mkdirMode(r *Reporter, env *Env, dir string, mode os.FileMode) error {
	full := env.Resolve(dir)
	fi, err := os.Stat(full)
	switch {
	case err == nil && !fi.IsDir():
		return fmt.Errorf("%s exists and is not a directory", full)
	case err == nil && fi.Mode().Perm() == mode:
		r.Add(Kept, dir, "")
		return nil
	case err == nil && env.DryRun:
		r.Add(Skipped, dir, "dry-run: would fix the mode to "+fmt.Sprintf("%#o", mode))
		return nil
	case err == nil:
		if err := os.Chmod(full, mode); err != nil {
			return fmt.Errorf("chmod %s: %w", full, err)
		}
		r.Add(Changed, dir, "mode fixed to "+fmt.Sprintf("%#o", mode))
		return nil
	case env.DryRun:
		r.Add(Created, dir, "dry-run: nothing created")
		return nil
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("stat %s: %w", full, err)
	}
	if err := os.MkdirAll(full, mode); err != nil {
		return fmt.Errorf("create %s: %w", full, err)
	}
	// MkdirAll applies the process umask; set the mode exactly.
	if err := os.Chmod(full, mode); err != nil {
		return fmt.Errorf("chmod %s: %w", full, err)
	}
	r.Add(Created, dir, "")
	return nil
}
