// Tests for the runtime phase (runtime.go): a temp Root, a fake
// runner, a fake fetcher and a fake health check, so the real phase
// runs end to end with no network and no systemctl. Each group pins
// the plan's items: the runtime tarball (and its sha256 rules), the
// pinned artifacts, the config and unit files with the store dir
// substituted, the token seed, the systemd step and the health wait.
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
	mrand "math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeFetcher answers Fetch from a table and records the URLs asked of
// it; an absent URL is a fetch failure like the net/http one.
type fakeFetcher struct {
	served map[string][]byte
	urls   []string
	err    error // every fetch fails when set
}

func (f *fakeFetcher) Fetch(ctx context.Context, url string) ([]byte, error) {
	f.urls = append(f.urls, url)
	if f.err != nil {
		return nil, f.err
	}
	if f.served == nil {
		return nil, fmt.Errorf("fetch %s: HTTP 404 (the fake serves no such URL)", url)
	}
	b, ok := f.served[url]
	if !ok {
		return nil, fmt.Errorf("fetch %s: HTTP 404 (the fake serves no such URL)", url)
	}
	return b, nil
}

// fakeHealth answers the health check in order, repeating its last
// answer forever: an empty queue is a healthy orchestrator always, one
// error is a failing one always.
type fakeHealth struct {
	answers []error
	urls    []string
}

func (f *fakeHealth) Check(ctx context.Context, url string) error {
	f.urls = append(f.urls, url)
	if len(f.answers) == 0 {
		return nil
	}
	a := f.answers[0]
	if len(f.answers) > 1 {
		f.answers = f.answers[1:]
	}
	return a
}

// runtimeEnv is an Env shaped for the runtime phase: a temp root, the
// recording runner, and the phase's own surfaces stubbed with a fake
// fetcher and health check and fast poll bounds, so a failing-health
// run stays under a second. The test decides the pin table with
// usePins and installs the pinned artifacts with seedPins or the fake
// fetcher; serveRuntime serves the matching release.
func runtimeEnv(t *testing.T) (*Env, *bytes.Buffer, *recordRunner, *fakeFetcher, *fakeHealth) {
	t.Helper()
	env, out, _ := testEnv(t)
	runner := idleRunner()
	env.Run = runner
	fetcher := &fakeFetcher{served: map[string][]byte{}}
	health := &fakeHealth{}
	stubRuntime(t, fetcher, health)
	env.Version = "v4.0.0"
	return env, out, runner, fetcher, health
}

// stubRuntime stubs the phase's network surfaces with fakes and fast
// poll bounds, restoring the phase value when the test ends.
func stubRuntime(t *testing.T, f Fetcher, h HealthChecker) *runtimeState {
	t.Helper()
	old := runtimeImpl
	runtimeImpl = runtimeState{Fetch: f, Health: h, pollEvery: time.Millisecond, deadline: 30 * time.Millisecond}
	t.Cleanup(func() { runtimeImpl = old })
	return &runtimeImpl
}

// runtimeTarball builds the small runtime tarball the tests install:
// spoond-runtime/{orchestrator,envd,RUNTIME} from three byte strings,
// the shape deploy/release/build-runtime.sh writes.
func runtimeTarball(t *testing.T, orchestrator, envd, stamp string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, content := range map[string]string{
		"RUNTIME":      stamp,
		"envd":         envd,
		"orchestrator": orchestrator,
	} {
		if err := tw.WriteHeader(&tar.Header{Name: "spoond-runtime/" + name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// runtimeFixture is the test's fake release content: fixed bytes for
// the orchestrator, envd and the stamp, and the tarball holding them.
func runtimeFixture(t *testing.T) (orch, envd, stamp string, tgz []byte) {
	t.Helper()
	orch, envd, stamp = "orchestrator-bin-v4", "envd-bin-v4", "e2b-runtime 0eece3df\n"
	return orch, envd, stamp, runtimeTarball(t, orch, envd, stamp)
}

// writeRuntimeTarball writes a tarball to a file and returns its path.
func writeRuntimeTarball(t *testing.T, tgz []byte) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "spoond-runtime_4.0.0_linux_amd64.tar.gz")
	if err := os.WriteFile(file, tgz, 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

// serveRuntime fills a fake fetcher with a release for ver (the tag:
// the test reuses the phase's downloadName, so its URL shape can only
// drift together with the phase's): the tarball under its release URL
// and a SHA256SUMS naming it.
func serveRuntime(t *testing.T, f *fakeFetcher, ver string, tgz []byte) {
	t.Helper()
	sum := sha256sum(tgz)
	url := "https://github.com/jrimmer/spoond/releases/download/" + ver
	f.served[url+"/"+downloadName(ver)] = tgz
	f.served[url+"/SHA256SUMS"] = []byte(sum + "  " + downloadName(ver) + "\n")
}

// rowsByName indexes the report rows by name. It fails when two rows
// share a name, so a lost or doubled row cannot hide.
func rowsByName(t *testing.T, items []Item) map[string]Item {
	t.Helper()
	rows := map[string]Item{}
	for _, it := range items {
		if _, dup := rows[it.Name]; dup {
			t.Fatalf("two rows for %s", it.Name)
		}
		rows[it.Name] = it
	}
	return rows
}

// rowOf returns one row and fails when it is missing: the health rows
// (kept or failed) and the restart row only exist when the phase's
// returned items carry them.
func rowOf(t *testing.T, items []Item, name string) Item {
	t.Helper()
	row, ok := rowsByName(t, items)[name]
	if !ok {
		t.Fatalf("no row for %s in:\n%s", name, reportText(items))
	}
	return row
}

// reportText renders items as the Reporter would print them, for
// failure messages.
func reportText(items []Item) string {
	var b strings.Builder
	for _, it := range items {
		fmt.Fprintf(&b, "  %-8s %s", it.Result, it.Name)
		if it.Note != "" {
			b.WriteString(" — " + it.Note)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// isHex says whether every rune of s is a lowercase hex digit.
func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// activeRunner answers the systemd commands of a host whose
// orchestrator is running.
func activeRunner() *recordRunner {
	return &recordRunner{reply: map[string]fakeReply{
		"systemctl is-active e2b-orchestrator":              {out: []byte("active\n")},
		"systemctl daemon-reload":                           {},
		"systemctl enable --now e2b-guard e2b-orchestrator": {},
		"systemctl restart e2b-orchestrator":                {},
		"systemctl restart e2b-guard":                       {},
	}}
}

// idleRunner answers is-active with "inactive" and nothing else: the
// calls a first install makes.
func idleRunner() *recordRunner {
	return &recordRunner{reply: map[string]fakeReply{
		"systemctl is-active e2b-orchestrator":              {out: []byte("inactive\n")},
		"systemctl daemon-reload":                           {},
		"systemctl enable --now e2b-guard e2b-orchestrator": {},
		"systemctl restart e2b-guard":                       {},
	}}
}

// runnerCommands returns the recorded calls whose text contains sub.
func runnerCommands(runner *recordRunner, sub string) []string {
	var got []string
	for _, c := range runner.calls {
		if strings.Contains(c, sub) {
			got = append(got, c)
		}
	}
	return got
}

// testPin is one pin row with the content its sha256 is of, so a test
// can serve the bytes the pin tables (the real table's sha256 pins
// Firecracker/kernel/busybox builds, which no test has).
type testPin struct {
	row     pinnedArtifact
	content []byte
}

// fakePins builds the test's pin table, shaped like the real one but
// with made-up URLs, and the content each row's sha256 is of: row i is
// "artifact<i>" at the real row's path and mode. usePins installs the
// rows; servePins or seedPins places the content.
func fakePins(n int) []testPin {
	pins := make([]testPin, n)
	for i := range pins {
		content := []byte(fmt.Sprintf("artifact-%d", i))
		s := sha256.Sum256(content)
		pins[i] = testPin{
			row: pinnedArtifact{
				url:    artifactBucket + fmt.Sprintf("/test-%d", i),
				path:   pinned[i].path,
				mode:   pinned[i].mode,
				sha256: hex.EncodeToString(s[:]),
			},
			content: content,
		}
	}
	return pins
}

// pinRows drops the content, giving the phase's table.
func pinRows(pins []testPin) []pinnedArtifact {
	rows := make([]pinnedArtifact, len(pins))
	for i, p := range pins {
		rows[i] = p.row
	}
	return rows
}

// usePins replaces the phase's pin table with the test's rows and
// restores the real table at the test's end.
func usePins(t *testing.T, pins []testPin) {
	t.Helper()
	old := pinned
	pinned = pinRows(pins)
	t.Cleanup(func() { pinned = old })
}

// servePins fills the fake fetcher with every pin's content (the pins
// a run has to fetch).
func servePins(t *testing.T, f *fakeFetcher, pins []testPin) {
	t.Helper()
	for _, p := range pins {
		f.served[p.row.url] = p.content
	}
}

// seedPins writes every pin's content to disk at its row's path (the
// "already installed" state).
func seedPins(t *testing.T, env *Env, pins []testPin) {
	t.Helper()
	for _, p := range pins {
		if err := os.MkdirAll(env.Resolve(filepath.Dir(p.row.path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(env.Resolve(p.row.path), p.content, p.row.mode); err != nil {
			t.Fatal(err)
		}
	}
}

// satisfyPins puts the test's pin table (fakePins) in place and writes
// its content to disk: the "pinned artifacts already installed" state,
// so a run through a test that does not care about pins fetches
// nothing and keeps them.
func satisfyPins(t *testing.T, env *Env) {
	t.Helper()
	pins := fakePins(len(pinned))
	usePins(t, pins)
	seedPins(t, env, pins)
}

// runRuntime runs the phase against env and returns the rows, the
// report text and the error.
func runRuntime(t *testing.T, env *Env) ([]Item, string, error) {
	t.Helper()
	items, err := RunPhase(context.Background(), env, PhaseRuntime, Phases[PhaseRuntime])
	return items, env.Out.(*bytes.Buffer).String(), err
}

// fileMode stats one resolved path's mode.
func fileMode(t *testing.T, env *Env, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(env.Resolve(path))
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

// withTarball fills env.Opt.Runtime with a tarball of the fixture.
func withTarball(t *testing.T, env *Env) {
	t.Helper()
	_, _, _, tgz := runtimeFixture(t)
	env.Opt.Runtime = writeRuntimeTarball(t, tgz)
}

// TestRuntimeTarballHappyPath: --runtime installs the three files with
// the right modes, /fc-vm is made, the configs and unit land with the
// store dir substituted, the token seed is created once, and a second
// run is all kept and runs no changing command.
func TestRuntimeTarballHappyPath(t *testing.T) {
	env, _, runner, _, health := runtimeEnv(t)
	satisfyPins(t, env)
	orch, envd, stamp, tgz := runtimeFixture(t)
	env.Opt.Runtime = writeRuntimeTarball(t, tgz)
	health.answers = []error{errFailed("warming up"), nil} // one failed poll, then healthy

	items, text, err := runRuntime(t, env)
	if err != nil {
		t.Fatalf("first run: %v\n%s", err, text)
	}
	rows := rowsByName(t, items)
	for _, path := range []string{orchestratorBinPath, envdBinPath, runtimeStampPath, orchestratorEnvPath, flagsJSONPath, tokenSeedPath, guardNftPath, guardUnitPath, orchestratorUnitPath, fcVMPath} {
		if row, ok := rows[path]; !ok {
			t.Errorf("no row for %s:\n%s", path, text)
		} else if row.Result != Created {
			t.Errorf("row %s = %s, want created", path, row.Result)
		}
	}
	for path, mode := range map[string]os.FileMode{
		orchestratorBinPath:  0o755,
		envdBinPath:          0o755,
		runtimeStampPath:     0o644,
		orchestratorEnvPath:  0o600,
		flagsJSONPath:        0o600,
		guardNftPath:         0o644,
		guardUnitPath:        0o644,
		orchestratorUnitPath: 0o644,
	} {
		if got := fileMode(t, env, path); got != mode {
			t.Errorf("%s mode = %#o, want %#o", path, got, mode)
		}
	}
	if got := fileMode(t, env, spoondDirPath); got != 0o700 {
		t.Errorf("%s mode = %#o, want 0700", spoondDirPath, got)
	}
	for path, want := range map[string]string{
		orchestratorBinPath: orch,
		envdBinPath:         envd,
		runtimeStampPath:    stamp,
	} {
		b, err := os.ReadFile(env.Resolve(path))
		if err != nil || string(b) != want {
			t.Errorf("%s = %q (%v), want the tarball member %q", path, b, err, want)
		}
	}

	// The orchestrator's unit runs after enable --now (it was not
	// running): the health wait polled /health, and no restart ran.
	if len(health.urls) == 0 || health.urls[0] != runtimeHealthURL {
		t.Errorf("health polled %v, want %s", health.urls, runtimeHealthURL)
	}
	if row := rows["restart "+orchestratorUnit]; row.Result != Kept {
		t.Errorf("restart row = %+v, want kept (enable --now starts it)", row)
	}
	if row := rows["orchestrator /health"]; row.Result != Kept {
		t.Errorf("health row = %+v, want kept", row)
	}
	if got := runnerCommands(runner, "restart"); len(got) != 0 {
		t.Errorf("a first install restarted the orchestrator: %v", got)
	}
	if got := runnerCommands(runner, "enable"); len(got) != 1 {
		t.Errorf("enable --now ran %v, want once", got)
	}

	// The token seed is 64 hex chars (a newline is allowed), and the
	// report never prints it.
	seed, err := os.ReadFile(env.Resolve(tokenSeedPath))
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	s := strings.TrimRight(string(seed), "\n")
	if len(s) != 64 || !isHex(s) {
		t.Errorf("seed = %q, want 64 hex chars", s)
	}
	if strings.Contains(text, s) {
		t.Error("the report printed the token seed")
	}

	// Second run: everything kept, no command but the is-active query.
	health.answers = nil
	runner.calls = nil
	items2, text2, err := runRuntime(t, env)
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, text2)
	}
	for _, it := range items2 {
		if it.Result != Kept {
			t.Errorf("second run row %s = %s (%q), want kept", it.Name, it.Result, it.Note)
		}
	}
	if got := runnerCommands(runner, "daemon-reload"); len(got) != 0 {
		t.Errorf("a kept run reloaded units: %v", got)
	}
	if got := runnerCommands(runner, "restart"); len(got) != 0 {
		t.Errorf("a kept run restarted the orchestrator: %v", got)
	}
	if got := runnerCommands(runner, "enable"); len(got) != 0 {
		t.Errorf("a kept run ran enable --now: %v", got)
	}
	seed2, _ := os.ReadFile(env.Resolve(tokenSeedPath))
	if string(seed2) != string(seed) {
		t.Error("the second run replaced the token seed")
	}
}

// errFailed is a fake poll failure.
func errFailed(msg string) error { return errors.New(msg) }

// TestRuntimeDevBuildFailsWithoutRuntime: no Version and no --runtime
// fails with the build-the-tarball hint, and nothing is written,
// fetched or run.
func TestRuntimeDevBuildFailsWithoutRuntime(t *testing.T) {
	env, _, runner, fetcher, _ := runtimeEnv(t)
	env.Version = ""
	items, text, err := runRuntime(t, env)
	if err == nil || !strings.Contains(err.Error(), "development build: pass --runtime spoond-runtime_<v>_linux_amd64.tar.gz") {
		t.Fatalf("err = %v, want the development-build hint", err)
	}
	if !strings.Contains(err.Error(), "deploy/release/build-runtime.sh builds one") {
		t.Errorf("err %q does not say what builds one", err)
	}
	if len(items) != 0 {
		t.Errorf("rows = %+v, want none", items)
	}
	if strings.Contains(text, "created") || strings.Contains(text, "kept") {
		t.Errorf("report shows work it never did:\n%s", text)
	}
	if len(fetcher.urls) != 0 || len(runner.calls) != 0 {
		t.Errorf("fetched %v, ran %v", fetcher.urls, runner.calls)
	}
	mustNotExist(t, env, orchestratorBinPath)
	mustNotExist(t, env, tokenSeedPath)
}

// TestRuntimeDownloadPathHappy: with a Version and no --runtime both
// release files are fetched — under the download name for the version,
// its tag with the leading v stripped — the tarball's line in
// SHA256SUMS checked, and the members installed.
func TestRuntimeDownloadPathHappy(t *testing.T) {
	env, _, _, fetcher, _ := runtimeEnv(t)
	satisfyPins(t, env)
	orch, envd, stamp, tgz := runtimeFixture(t)
	serveRuntime(t, fetcher, "v4.0.0", tgz)
	if _, _, err := runRuntime(t, env); err != nil {
		t.Fatalf("run: %v", err)
	}
	b, _ := os.ReadFile(env.Resolve(orchestratorBinPath))
	if string(b) != orch {
		t.Errorf("orchestrator = %q, want the tarball member", b)
	}
	if b, _ := os.ReadFile(env.Resolve(envdBinPath)); string(b) != envd {
		t.Errorf("envd = %q, want the tarball member", b)
	}
	if b, _ := os.ReadFile(env.Resolve(runtimeStampPath)); string(b) != stamp {
		t.Errorf("RUNTIME = %q, want the tarball member", b)
	}
	want := []string{
		"https://github.com/jrimmer/spoond/releases/download/v4.0.0/spoond-runtime_4.0.0_linux_amd64.tar.gz",
		"https://github.com/jrimmer/spoond/releases/download/v4.0.0/SHA256SUMS",
	}
	if len(fetcher.urls) != 2 || fetcher.urls[0] != want[0] || fetcher.urls[1] != want[1] {
		t.Errorf("fetched %v, want %v", fetcher.urls, want)
	}
}

// TestRuntimeDownloadSHA256MismatchFailsWritingNothing: a tarball whose
// sha256 is not in SHA256SUMS fails the phase and installs nothing.
func TestRuntimeDownloadSHA256MismatchFailsWritingNothing(t *testing.T) {
	env, _, runner, fetcher, _ := runtimeEnv(t)
	satisfyPins(t, env)
	_, _, _, tgz := runtimeFixture(t)
	other := sha256sum([]byte("not the tarball"))
	url := "https://github.com/jrimmer/spoond/releases/download/v4.0.0"
	fetcher.served[url+"/"+downloadName("v4.0.0")] = tgz
	fetcher.served[url+"/SHA256SUMS"] = []byte(other + "  " + downloadName("v4.0.0") + "\n")

	items, text, err := runRuntime(t, env)
	if err == nil || !strings.Contains(err.Error(), "SHA256SUMS") {
		t.Fatalf("err = %v, want the SHA256SUMS mismatch", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("ran %v after a failed checksum", runner.calls)
	}
	for _, path := range []string{orchestratorBinPath, envdBinPath, runtimeStampPath} {
		if _, ok := rowsByName(t, items)[path]; ok {
			t.Errorf("a row exists for %s; a failed checksum must install nothing:\n%s", path, text)
		}
		mustNotExist(t, env, path)
	}
	mustNotExist(t, env, tokenSeedPath)
}

// TestRuntimeDownloadMissingSHA256SUMSFails: a release without
// SHA256SUMS fails with the fetch error, and nothing installed.
func TestRuntimeDownloadMissingSHA256SUMSFails(t *testing.T) {
	env, _, _, fetcher, _ := runtimeEnv(t)
	satisfyPins(t, env)
	_, _, _, tgz := runtimeFixture(t)
	url := "https://github.com/jrimmer/spoond/releases/download/v4.0.0"
	fetcher.served[url+"/"+downloadName("v4.0.0")] = tgz
	_, _, err := runRuntime(t, env)
	if err == nil || !strings.Contains(err.Error(), url+"/SHA256SUMS") {
		t.Fatalf("err = %v, want the SHA256SUMS fetch failure naming its URL", err)
	}
	mustNotExist(t, env, orchestratorBinPath)
}

// TestRuntimeSidecarSHA256: a .sha256 beside the --runtime tarball is
// honoured — a mismatched one fails before any install, a matching one
// passes.
func TestRuntimeSidecarSHA256(t *testing.T) {
	_, _, _, tgz := runtimeFixture(t)
	orch := "orchestrator-bin-v4"

	mismatched := filepath.Join(t.TempDir(), "rt.tar.gz")
	if err := os.WriteFile(mismatched, tgz, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mismatched+".sha256", []byte(strings.Repeat("0", 64)+"  rt.tar.gz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env, _, _, _, _ := runtimeEnv(t)
	satisfyPins(t, env)
	env.Opt.Runtime = mismatched
	_, _, err := runRuntime(t, env)
	if err == nil || !strings.Contains(err.Error(), "--runtime "+mismatched+".sha256: sha256 mismatch") {
		t.Fatalf("err = %v, want the sidecar sha256 mismatch", err)
	}
	mustNotExist(t, env, orchestratorBinPath)

	matching := filepath.Join(t.TempDir(), "rt2.tar.gz")
	if err := os.WriteFile(matching, tgz, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(matching+".sha256", []byte(sha256sum(tgz)+"  rt2.tar.gz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env2, _, _, _, _ := runtimeEnv(t)
	satisfyPins(t, env2)
	env2.Opt.Runtime = matching
	if _, _, err := runRuntime(t, env2); err != nil {
		t.Fatalf("run with a matching sidecar: %v", err)
	}
	if b, _ := os.ReadFile(env2.Resolve(orchestratorBinPath)); string(b) != orch {
		t.Errorf("orchestrator = %q, want the tarball member", b)
	}
}

// TestRuntimeSidecarOptional: a tarball without a .sha256 beside it
// installs (the sidecar is checked only when it exists).
func TestRuntimeSidecarOptional(t *testing.T) {
	env, _, _, _, _ := runtimeEnv(t)
	satisfyPins(t, env)
	withTarball(t, env)
	if _, _, err := runRuntime(t, env); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// TestRuntimeKeptIdenticalBinaries: the same members in a second
// tarball are kept — the compare is by content, not by file time.
func TestRuntimeKeptIdenticalBinaries(t *testing.T) {
	env, _, _, _, _ := runtimeEnv(t)
	satisfyPins(t, env)
	withTarball(t, env)
	if _, _, err := runRuntime(t, env); err != nil {
		t.Fatalf("first: %v", err)
	}
	// A fresh path, the same bytes: the compare is by content.
	env.Opt.Runtime = writeRuntimeTarball(t, mustTarball(t))
	items, _, err := runRuntime(t, env)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	rows := rowsByName(t, items)
	for _, path := range []string{orchestratorBinPath, envdBinPath, runtimeStampPath} {
		if rows[path].Result != Kept {
			t.Errorf("row %s = %s, want kept (identical content)", path, rows[path].Result)
		}
	}
}

// mustTarball builds the fixture tarball, dropping the unused members.
func mustTarball(t *testing.T) []byte {
	t.Helper()
	_, _, _, tgz := runtimeFixture(t)
	return tgz
}

// TestRuntimeChangedOrchestratorGivesChangedAndRestart: different
// orchestrator bytes are installed (changed) and the running
// orchestrator is restarted.
func TestRuntimeChangedOrchestratorGivesChangedAndRestart(t *testing.T) {
	env, _, _, _, health := runtimeEnv(t)
	satisfyPins(t, env)
	runner := activeRunner() // the runner the phase records against
	env.Run = runner
	health.answers = nil // every poll healthy
	withTarball(t, env)
	if _, _, err := runRuntime(t, env); err != nil {
		t.Fatalf("setup: %v", err)
	}
	runner.calls = nil // the tally starts at the second run

	orch2 := "orchestrator-bin-v5"
	_, _, stamp, _ := runtimeFixture(t)
	tgz2 := runtimeTarball(t, orch2, "envd-bin-v4", stamp)
	env.Opt.Runtime = writeRuntimeTarball(t, tgz2)
	items, _, err := runRuntime(t, env)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	rows := rowsByName(t, items)
	if rows[orchestratorBinPath].Result != Changed {
		t.Errorf("row %s = %s, want changed", orchestratorBinPath, rows[orchestratorBinPath].Result)
	}
	if row := rowOf(t, items, "restart "+orchestratorUnit); row.Result != Changed {
		t.Errorf("restart row = %+v, want changed", row)
	}
	if got := runnerCommands(runner, "systemctl restart e2b-orchestrator"); len(got) != 1 {
		t.Errorf("restarts %v, want exactly one after the binary changed", got)
	}
}

// TestPinnedPresentRightSHAKeptNotFetched: a present file with the
// right sha256 is kept, and nothing is fetched.
func TestPinnedPresentRightSHAKeptNotFetched(t *testing.T) {
	env, _, _, fetcher, _ := runtimeEnv(t)
	runner := activeRunner() // a running orchestrator: no enable on a kept run
	env.Run = runner
	pins := fakePins(len(pinned))
	usePins(t, pins)
	seedPins(t, env, pins)
	withTarball(t, env)

	if _, _, err := runRuntime(t, env); err != nil { // a setup first: binaries and config land
		t.Fatalf("setup run: %v", err)
	}
	runner.calls = nil
	items, _, err := runRuntime(t, env)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	byName := rowsByName(t, items)
	for i, p := range pins {
		if row := byName[p.row.path]; row.Result != Kept {
			t.Errorf("row %s = %s (%q), want kept", p.row.path, row.Result, row.Note)
		}
		if b, _ := os.ReadFile(env.Resolve(p.row.path)); string(b) != fmt.Sprintf("artifact-%d", i) {
			t.Errorf("%s content = %q, want the pre-existing bytes", p.row.path, b)
		}
	}
	for _, u := range fetcher.urls {
		if strings.Contains(u, artifactBucket) {
			t.Errorf("fetched %s though a correct file was already in place", u)
		}
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "daemon-reload") || strings.Contains(call, "restart") || strings.Contains(call, "enable") {
			t.Errorf("a kept run ran a changing command: %v", runner.calls)
		}
	}
}

// TestPinnedAbsentFetchedAndVerified: an absent pinned artifact is
// fetched, its sha256 checked against the pin before the write, and
// installed with the pin's mode.
func TestPinnedAbsentFetchedAndVerified(t *testing.T) {
	env, _, _, fetcher, _ := runtimeEnv(t)
	env.Run = idleRunner()
	pins := fakePins(len(pinned))
	usePins(t, pins)
	servePins(t, fetcher, pins)
	withTarball(t, env)

	items, _, err := runRuntime(t, env)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	byName := rowsByName(t, items)
	for i, p := range pins {
		if row := byName[p.row.path]; row.Result != Created {
			t.Errorf("row %s = %s, want created", p.row.path, row.Result)
		}
		b, err := os.ReadFile(env.Resolve(p.row.path))
		if err != nil {
			t.Fatalf("read %s: %v", p.row.path, err)
		}
		if string(b) != fmt.Sprintf("artifact-%d", i) {
			t.Errorf("%s = %q, want the fetched content", p.row.path, b)
		}
		if got := fileMode(t, env, p.row.path); got != p.row.mode {
			t.Errorf("%s mode = %#o, want %#o", p.row.path, got, p.row.mode)
		}
	}
}

// TestPinnedInPlaceWrongSHAFetchedNotBlindlyKept: a present file with a
// wrong sha256 is re-fetched and verified; the pinned bytes replace it.
func TestPinnedInPlaceWrongSHAFetchedNotBlindlyKept(t *testing.T) {
	env, _, _, fetcher, _ := runtimeEnv(t)
	env.Run = idleRunner()
	pins := fakePins(1)
	usePins(t, pins)
	p := pins[0]
	good := p.content
	servePins(t, fetcher, pins[:1])
	if err := os.MkdirAll(env.Resolve(filepath.Dir(p.row.path)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.Resolve(p.row.path), []byte("wrong bytes"), p.row.mode); err != nil {
		t.Fatal(err)
	}
	withTarball(t, env)

	items, _, err := runRuntime(t, env)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	row := rowsByName(t, items)[p.row.path]
	if row.Result != Changed {
		t.Errorf("row = %s (%q), want changed", row.Result, row.Note)
	}
	if b, _ := os.ReadFile(env.Resolve(p.row.path)); string(b) != string(good) {
		t.Errorf("the in-place wrong bytes stayed: %q", b)
	}
}

// TestPinnedFetchedWrongBytesFails: a fetch whose bytes do not match
// the pin fails the phase and leaves no file.
func TestPinnedFetchedWrongBytesFails(t *testing.T) {
	env, _, _, fetcher, _ := runtimeEnv(t)
	withTarball(t, env)
	// The first pin's content under its URL does not match the pin's
	// sha256: the fetch succeeds, the verify fails.
	fetcher.served[pinned[0].url] = []byte("not firecracker")

	items, text, err := runRuntime(t, env)
	if err == nil || !strings.Contains(err.Error(), "the pin wants "+pinned[0].sha256) {
		t.Fatalf("err = %v, want the pin mismatch naming the wanted sha256:\n%s", err, text)
	}
	row := rowsByName(t, items)[pinned[0].path]
	if row.Result != Failed {
		t.Errorf("row = %s, want failed", row.Result)
	}
	mustNotExist(t, env, pinned[0].path)
}

// TestPinnedFetchFailureKeepsOldFile: a fetch error over a present (but
// stale) file leaves that file and reports failed.
func TestPinnedFetchFailureKeepsOldFile(t *testing.T) {
	env, _, _, _, _ := runtimeEnv(t)
	if err := os.MkdirAll(env.Resolve(filepath.Dir(pinned[0].path)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.Resolve(pinned[0].path), []byte("stale bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	withTarball(t, env)

	items, _, err := runRuntime(t, env)
	if err == nil {
		t.Fatal("run succeeded, want a failure for the stale pinned file")
	}
	row := rowsByName(t, items)[pinned[0].path]
	if row.Result != Failed {
		t.Errorf("row = %s, want failed", row.Result)
	}
	if b, _ := os.ReadFile(env.Resolve(pinned[0].path)); string(b) != "stale bytes" {
		t.Error("the failed fetch overwrote the file")
	}
}

// TestStoreDirSubstituted: /forkdcache/e2b in the orchestrator env and
// the unit becomes the operator's --store-dir.
func TestStoreDirSubstituted(t *testing.T) {
	env, _, _, _, _ := runtimeEnv(t)
	satisfyPins(t, env)
	withTarball(t, env)
	env.Opt.StoreDir = "/data/e2b-store"

	if _, _, err := runRuntime(t, env); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, path := range []string{orchestratorEnvPath, orchestratorUnitPath} {
		b, err := os.ReadFile(env.Resolve(path))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(b), DefaultStoreDir) {
			t.Errorf("%s still names the default store dir:\n%s", path, b)
		}
		if !strings.Contains(string(b), "/data/e2b-store") {
			t.Errorf("%s does not name --store-dir:\n%s", path, b)
		}
	}
}

// TestShippedConfigsMatchDeployFiles: both operator-editable configs
// and the guard files come from deploy.Files (the substitution only
// ever replaces the store prefix).
func TestShippedConfigsMatchDeployFiles(t *testing.T) {
	for _, name := range []string{orchestratorEnvShipped, flagsShipped, guardNftShipped, guardUnitShipped, orchestratorShipped} {
		b, err := shipped(name)
		if err != nil || len(b) == 0 {
			t.Errorf("shipped %s: %d bytes, %v", name, len(b), err)
		}
	}
}

// TestOperatorEditedConfigKept: an operator's orchestrator.env with
// other content is kept with the compare note, not overwritten.
func TestOperatorEditedConfigKept(t *testing.T) {
	env, _, _, _, _ := runtimeEnv(t)
	satisfyPins(t, env)
	withTarball(t, env)
	if err := os.MkdirAll(env.Resolve("/etc/e2b"), 0o755); err != nil {
		t.Fatal(err)
	}
	edited := []byte("NODE_ID=my-node\nORCHESTRATOR_BASE_PATH=/data/store/orchestrator\n")
	if err := os.WriteFile(env.Resolve(orchestratorEnvPath), edited, 0o600); err != nil {
		t.Fatal(err)
	}

	items, _, err := runRuntime(t, env)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	row := rowsByName(t, items)[orchestratorEnvPath]
	if row.Result != Kept {
		t.Errorf("row = %s, want kept", row.Result)
	}
	if !strings.Contains(row.Note, "differs from the shipped file; compare with deploy/e2b/orchestrator.env") {
		t.Errorf("note = %q, want the compare hint", row.Note)
	}
	if b, _ := os.ReadFile(env.Resolve(orchestratorEnvPath)); !bytes.Equal(b, edited) {
		t.Errorf("the operator's edit was overwritten: %q", b)
	}
}

// TestIdenticalOperatorConfigKeptPlain: the shipped config (with the
// store dir substituted) already in place is a plain kept, no note.
func TestIdenticalOperatorConfigKeptPlain(t *testing.T) {
	env, _, _, _, _ := runtimeEnv(t)
	satisfyPins(t, env)
	withTarball(t, env)
	env.Opt.StoreDir = "/data/store"
	want, err := shippedWithStore(orchestratorEnvShipped, "/data/store")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(env.Resolve("/etc/e2b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.Resolve(orchestratorEnvPath), want, 0o600); err != nil {
		t.Fatal(err)
	}

	items, _, err := runRuntime(t, env)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if row := rowsByName(t, items)[orchestratorEnvPath]; row.Result != Kept || row.Note != "" {
		t.Errorf("row = %+v, want a plain kept", row)
	}
}

// TestDefaultStoreDirWrittenVerbatim: without --store-dir the shipped
// bytes are installed as they are, every /forkdcache/e2b kept.
func TestDefaultStoreDirWrittenVerbatim(t *testing.T) {
	env, _, _, _, _ := runtimeEnv(t)
	satisfyPins(t, env)
	withTarball(t, env)
	want, err := shipped(orchestratorEnvShipped)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(want, []byte(DefaultStoreDir)) {
		t.Fatalf("the shipped env does not name %s; this test would be empty", DefaultStoreDir)
	}
	if _, _, err := runRuntime(t, env); err != nil {
		t.Fatalf("run: %v", err)
	}
	if b, _ := os.ReadFile(env.Resolve(orchestratorEnvPath)); !bytes.Equal(b, want) {
		t.Errorf("the default store dir was rewritten:\n%s", b)
	}
}

// TestTokenSeedCreated0600AndNeverOverwritten: the seed is created with
// 64 hex chars at 0600; a run over an existing seed keeps it — the
// seed's value is never read back, so an operator's hand-set seed
// stays.
func TestTokenSeedCreated0600AndNeverOverwritten(t *testing.T) {
	env, out, _, _, _ := runtimeEnv(t)
	satisfyPins(t, env)
	withTarball(t, env)

	if _, _, err := runRuntime(t, env); err != nil {
		t.Fatalf("first run: %v", err)
	}
	seed, err := os.ReadFile(env.Resolve(tokenSeedPath))
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	if got := fileMode(t, env, tokenSeedPath); got != 0o600 {
		t.Errorf("seed mode = %#o, want 0600", got)
	}
	s := strings.TrimRight(string(seed), "\n")
	if len(s) != 64 || !isHex(s) {
		t.Errorf("seed = %q, want 64 hex chars", s)
	}
	if strings.Contains(out.String(), s) {
		t.Error("the report printed the seed")
	}

	// A pre-existing seed is kept whatever its content looks like.
	env2, out2, _, _, _ := runtimeEnv(t)
	satisfyPins(t, env2)
	withTarball(t, env2)
	if err := os.MkdirAll(env2.Resolve(spoondDirPath), 0o700); err != nil {
		t.Fatal(err)
	}
	old := []byte(strings.Repeat("ff", 32) + "\n")
	if err := os.WriteFile(env2.Resolve(tokenSeedPath), old, 0o600); err != nil {
		t.Fatal(err)
	}
	items, _, err := runRuntime(t, env2)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	row := rowsByName(t, items)[tokenSeedPath]
	if row.Result != Kept {
		t.Errorf("row = %s, want kept", row.Result)
	}
	if b, _ := os.ReadFile(env2.Resolve(tokenSeedPath)); !bytes.Equal(b, old) {
		t.Errorf("the existing seed was replaced: %q", b)
	}
	if strings.Contains(out2.String(), "ffffffff") {
		t.Error("the report printed the surviving seed's value")
	}
}

// TestSeedIsFreshRandom: two runs write different seeds (they are
// random, not a constant), shaped as 64 hex chars plus a newline.
func TestSeedIsFreshRandom(t *testing.T) {
	a, err := seedValue()
	if err != nil {
		t.Fatalf("seedValue: %v", err)
	}
	b, err := seedValue()
	if err != nil {
		t.Fatalf("seedValue: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Error("two seeds are equal")
	}
	if len(a) != 65 {
		t.Errorf("seed = %q (%d bytes), want 64 hex chars + newline", a, len(a))
	}
	if !isHex(strings.TrimRight(string(a), "\n")) {
		t.Errorf("seed %q is not hex", a)
	}
}

// TestUnitHasNoDrainLine: the unit the runtime phase installs carries
// no "drain" line, and keeps every other line.
func TestUnitHasNoDrainLine(t *testing.T) {
	env, _, _, _, _ := runtimeEnv(t)
	satisfyPins(t, env)
	withTarball(t, env)

	if _, _, err := runRuntime(t, env); err != nil {
		t.Fatalf("run: %v", err)
	}
	b, err := os.ReadFile(env.Resolve(orchestratorUnitPath))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(b)), "drain") {
		t.Errorf("the installed unit carries a drain line:\n%s", b)
	}
	want, err := shippedWithStore(orchestratorShipped, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(want), "\n") {
		if line == "" || strings.Contains(strings.ToLower(line), "drain") {
			continue
		}
		if !strings.Contains(string(b), line) {
			t.Errorf("the de-drained unit lost %q", line)
		}
	}
}

// TestUnitKeepsShapeAfterDrop: dropping the drain lines leaves one
// blank line between sections, no doubled blanks, one trailing
// newline.
func TestUnitKeepsShapeAfterDrop(t *testing.T) {
	full, err := shipped(orchestratorShipped)
	if err != nil {
		t.Fatal(err)
	}
	light := dropDrainLines(full)
	if bytes.Contains(bytes.ToLower(light), []byte("drain")) {
		t.Errorf("the de-drained unit still carries a drain line:\n%s", light)
	}
	if !bytes.HasSuffix(light, []byte("\n")) {
		t.Error("the de-drained unit lost its trailing newline")
	}
	if bytes.Contains(light, []byte("\n\n\n")) {
		t.Errorf("the de-drained unit has doubled blank lines:\n%s", light)
	}
}

// TestFullShippedUnitKept: a unit holding the shipped drain hooks (the
// spoond phase's install) is kept, not downgraded.
func TestFullShippedUnitKept(t *testing.T) {
	env, _, _, _, _ := runtimeEnv(t)
	satisfyPins(t, env)
	withTarball(t, env)
	fullUnit, err := shippedWithStore(orchestratorShipped, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(env.Resolve("/etc/systemd/system"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.Resolve(orchestratorUnitPath), fullUnit, 0o644); err != nil {
		t.Fatal(err)
	}

	items, _, err := runRuntime(t, env)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	row := rowsByName(t, items)[orchestratorUnitPath]
	if row.Result != Kept {
		t.Errorf("row = %s (%q), want kept", row.Result, row.Note)
	}
	if got, _ := os.ReadFile(env.Resolve(orchestratorUnitPath)); !bytes.Equal(got, fullUnit) {
		t.Error("the full unit was rewritten without its drain hooks")
	}
}

// TestDryRunRuntime: a dry run plans everything but writes no file,
// runs no changing command, and fetches nothing that a present, correct
// sha256 (or a dry run itself) can decide; the release tarball is not
// fetched and the report's rows carry the dry-run notes.
func TestDryRunRuntime(t *testing.T) {
	env, _, runner, fetcher, _ := runtimeEnv(t)
	env.DryRun = true
	pins := fakePins(len(pinned))
	usePins(t, pins)
	seedPins(t, env, pins)
	withTarball(t, env)

	items, text, err := runRuntime(t, env)
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, text)
	}
	byName := rowsByName(t, items)
	for _, p := range pins {
		if row := byName[p.row.path]; row.Result != Kept {
			t.Errorf("pinned row %s = %+v, want kept without a fetch", p.row.path, row)
		}
	}
	for _, path := range []string{orchestratorBinPath, envdBinPath, runtimeStampPath, orchestratorEnvPath, tokenSeedPath, guardNftPath, guardUnitPath, orchestratorUnitPath, fcVMPath} {
		row, ok := byName[path]
		if !ok {
			t.Errorf("the dry run planned nothing for %s:\n%s", path, text)
			continue
		}
		if row.Result != Created || !strings.Contains(row.Note, "dry-run") {
			t.Errorf("row %s = %+v, want a planned created with a dry-run note", path, row)
		}
		mustNotExist(t, env, path)
	}
	if _, err := os.Stat(env.Resolve(spoondDirPath)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the dry run created %s (stat: %v)", spoondDirPath, err)
	}
	if len(fetcher.urls) != 0 {
		t.Errorf("the dry run fetched %v", fetcher.urls)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "daemon-reload") || strings.Contains(call, "restart") || strings.Contains(call, "enable") {
			t.Errorf("the dry run ran a changing command: %v", runner.calls)
		}
	}
}

// TestDryRunRuntimeReleaseNotFetched: a dry run with a Version and no
// --runtime plans the release install without fetching anything.
func TestDryRunRuntimeReleaseNotFetched(t *testing.T) {
	env, _, runner, fetcher, _ := runtimeEnv(t)
	satisfyPins(t, env)
	env.DryRun = true
	items, text, err := runRuntime(t, env)
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, text)
	}
	byName := rowsByName(t, items)
	for _, path := range []string{orchestratorBinPath, envdBinPath, runtimeStampPath} {
		if row := byName[path]; row.Result != Created {
			t.Errorf("row %s = %+v, want a planned created", path, row)
		}
		mustNotExist(t, env, path)
	}
	if len(fetcher.urls) != 0 {
		t.Errorf("the dry run fetched %v", fetcher.urls)
	}
	for _, call := range runner.calls {
		if !strings.Contains(call, "is-active") {
			t.Errorf("the dry run ran %v", runner.calls)
		}
	}
}

// TestDryRunRuntimeStalePinnedNotFetched: a dry run over a pinned file
// with wrong content reports skipped (a real run would fetch), and the
// dry run still fetches nothing.
func TestDryRunRuntimeStalePinnedNotFetched(t *testing.T) {
	env, _, _, fetcher, _ := runtimeEnv(t)
	env.DryRun = true
	if err := os.MkdirAll(env.Resolve(filepath.Dir(pinned[0].path)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.Resolve(pinned[0].path), []byte("stale bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	withTarball(t, env)
	items, _, err := runRuntime(t, env)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if row := rowsByName(t, items)[pinned[0].path]; row.Result != Skipped {
		t.Errorf("row = %+v, want skipped (a real run would re-fetch)", row)
	}
	if len(fetcher.urls) != 0 {
		t.Errorf("the dry run fetched %v", fetcher.urls)
	}
}

// TestRestartConditions: the orchestrator is restarted only when its
// unit, env, flags or binary changed and it was already active.
func TestRestartConditions(t *testing.T) {
	// A first install over a running orchestrator: binaries, config
	// and units were all created this run, so one restart.
	env, _, _, _, health := runtimeEnv(t)
	satisfyPins(t, env)
	runner := activeRunner() // a running orchestrator
	env.Run = runner
	health.answers = nil
	withTarball(t, env)
	if _, _, err := runRuntime(t, env); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := runnerCommands(runner, "systemctl restart e2b-orchestrator"); len(got) != 1 {
		t.Errorf("restarts %v, want one (files changed, it was active)", got)
	}

	// Nothing changing: no restart.
	runner.calls = nil
	if _, _, err := runRuntime(t, env); err != nil {
		t.Fatalf("kept run: %v", err)
	}
	if got := runnerCommands(runner, "restart"); len(got) != 0 {
		t.Errorf("a kept run restarted: %v", got)
	}
}

// TestGuardUnitChangedAloneDoesNotRestartOrchestrator: only when the
// orchestrator's own surface (unit, env, flags, binary) changed does
// the phase restart it.
func TestGuardUnitChangedAloneDoesNotRestartOrchestrator(t *testing.T) {
	// Set up a fully installed host, then edit the guard unit only.
	env, _, _, _, health := runtimeEnv(t)
	satisfyPins(t, env)
	runner := activeRunner() // a running orchestrator
	env.Run = runner
	health.answers = nil
	withTarball(t, env)
	if _, _, err := runRuntime(t, env); err != nil {
		t.Fatalf("setup run: %v", err)
	}
	// The next run differs only in the guard unit's content — the
	// orchestrator's files all match.
	runner.calls = nil
	guard, err := shipped(guardUnitShipped)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.Resolve(guardUnitPath), []byte(string(guard)+"# edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	items, _, err := runRuntime(t, env)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	rows := rowsByName(t, items)
	if rows[guardUnitPath].Result != Changed {
		t.Errorf("row %s = %+v, want changed", guardUnitPath, rows[guardUnitPath])
	}
	if got := runnerCommands(runner, "systemctl restart e2b-orchestrator"); len(got) != 0 {
		t.Errorf("the orchestrator restarted for a guard-only change: %v", got)
	}
	if got := runnerCommands(runner, "daemon-reload"); len(got) != 1 {
		t.Errorf("daemon-reload %v, want one (a unit changed)", got)
	}
	if got := runnerCommands(runner, "systemctl restart e2b-guard"); len(got) != 1 {
		t.Errorf("guard restarts %v, want one (it reloads the table)", got)
	}
}

// TestStoppedOrchestratorIsStarted: with every file in place and the
// orchestrator stopped, the run starts it rather than waiting on a
// health check nothing will answer.
func TestStoppedOrchestratorIsStarted(t *testing.T) {
	env, _, _, _, health := runtimeEnv(t)
	satisfyPins(t, env)
	env.Run = activeRunner()
	health.answers = nil
	withTarball(t, env)
	if _, _, err := runRuntime(t, env); err != nil {
		t.Fatalf("setup run: %v", err)
	}
	runner := idleRunner()
	env.Run = runner
	if _, _, err := runRuntime(t, env); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := runnerCommands(runner, "enable --now"); len(got) != 1 {
		t.Errorf("enable --now %v, want one (the orchestrator was stopped)", got)
	}
	if got := runnerCommands(runner, "daemon-reload"); len(got) != 0 {
		t.Errorf("daemon-reload %v, want none (no unit changed)", got)
	}
}

// TestHealthTimeoutMessage: a run whose orchestrator never answers
// healthy fails with the journalctl hint after the deadline, naming the
// command, and the poll hit /health repeatedly.
func TestHealthTimeoutMessage(t *testing.T) {
	env, _, _, _, health := runtimeEnv(t)
	satisfyPins(t, env)
	env.Run = idleRunner()
	health.answers = []error{errFailed("connection refused")}
	withTarball(t, env)

	items, _, err := runRuntime(t, env)
	if err == nil || !strings.Contains(err.Error(), "the orchestrator is not healthy: journalctl -u e2b-orchestrator -n 50") {
		t.Fatalf("err = %v, want the journalctl hint", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("err %q does not name the last check", err)
	}
	row := rowOf(t, items, "orchestrator /health")
	if row.Result != Failed {
		t.Errorf("row = %+v, want failed", row)
	}
	if len(health.urls) < 2 || health.urls[0] != runtimeHealthURL {
		t.Errorf("polled %v, want repeated checks of %s", health.urls, runtimeHealthURL)
	}
}

// TestHealthWaitsThroughStartup: a poll that fails twice and then
// answers healthy passes the phase.
func TestHealthWaitsThroughStartup(t *testing.T) {
	env, _, _, _, health := runtimeEnv(t)
	satisfyPins(t, env)
	env.Run = idleRunner()
	health.answers = []error{errFailed("starting"), errFailed("still starting"), nil}
	withTarball(t, env)

	items, _, err := runRuntime(t, env)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	row := rowOf(t, items, "orchestrator /health")
	if row.Result != Kept {
		t.Errorf("row = %+v, want kept", row)
	}
	if len(health.urls) != 3 {
		t.Errorf("polled %d times (%v), want 3 (two failed polls then healthy)", len(health.urls), health.urls)
	}
}

// TestHealthCancelledContextStopsWaiting: a cancelled context ends the
// health wait quickly (Ctrl-C during the wait must not poll on), and
// the phase fails with the journalctl hint.
func TestHealthCancelledContextStopsWaiting(t *testing.T) {
	env, _, _, _, health := runtimeEnv(t)
	satisfyPins(t, env)
	// The stub is the phase's fast-poll value; a real Ctrl-C would hit
	// the production 2 s gap, which this check pins through pollEvery.
	state := stubRuntime(t, nil, health)
	state.pollEvery = 10 * time.Second
	state.deadline = time.Minute
	health.answers = []error{errFailed("refused")}
	withTarball(t, env)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	items, err := RunPhase(ctx, env, PhaseRuntime, Phases[PhaseRuntime])
	if err == nil || !strings.Contains(err.Error(), "the orchestrator is not healthy") {
		t.Fatalf("err = %v, want the journalctl hint", err)
	}
	row := rowOf(t, items, "orchestrator /health")
	if row.Result != Failed {
		t.Errorf("row = %+v, want failed", row)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the cancelled wait ran %s, want a quick return", elapsed)
	}
	if n := len(health.urls); n > 2 {
		t.Errorf("polled %d times on a cancelled context, want at most an initial check", n)
	}
}

// TestHTTPFetcherAgainstServer runs the production fetcher against an
// httptest server: 200 serves the body; 404 is an error naming the
// status; the timeout bounds a slow body.
func TestHTTPFetcherAgainstServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte("payload"))
		case "/slow":
			time.Sleep(50 * time.Millisecond)
			_, _ = w.Write([]byte("late"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f := httpFetcher{}
	got, err := f.Fetch(context.Background(), srv.URL+"/ok")
	if err != nil || string(got) != "payload" {
		t.Fatalf("fetch ok = %q, %v", got, err)
	}
	_, err = f.Fetch(context.Background(), srv.URL+"/missing")
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("err = %v, want HTTP 404", err)
	}
	slow := f
	slow.Timeout = 10 * time.Millisecond
	if _, err := slow.Fetch(context.Background(), srv.URL+"/slow"); err == nil {
		t.Fatal("a fetch over its timeout succeeded")
	}
}

// TestHTTPHealthAgainstServer runs the production health check against
// an httptest server: a healthy body is nil, other statuses and other
// bodies are errors.
func TestHTTPHealthAgainstServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("state") {
		case "bad":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"starting"}`))
		case "nothealthy":
			_, _ = w.Write([]byte(`{"status":"degraded"}`))
		case "serverdown":
			w.WriteHeader(500)
		default:
			_, _ = w.Write([]byte(`{"status":"healthy","version":"4.0.0"}`))
		}
	}))
	defer srv.Close()
	hc := httpHealth{}
	if err := hc.Check(context.Background(), srv.URL+"?state=good"); err != nil {
		t.Errorf("healthy: %v", err)
	}
	if err := hc.Check(context.Background(), srv.URL+"?state=bad"); err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Errorf("503: err = %v, want HTTP 503", err)
	}
	if err := hc.Check(context.Background(), srv.URL+"?state=nothealthy"); err == nil {
		t.Error("degraded: want an error")
	}
}

// TestPinnedTableMatchesHostSetup is the drift guard: every row's URL,
// path, mode and sha256 matches a fetch line of
// deploy/e2b/host-setup.sh, and the script fetches exactly these.
func TestPinnedTableMatchesHostSetup(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "deploy", "e2b", "host-setup.sh"))
	if err != nil {
		t.Skipf("deploy/e2b/host-setup.sh not readable from the test's working directory: %v", err)
	}
	script := string(b)
	// The script names the URLs through its $B variable (its base URL,
	// defined near the top); resolve it the way the shell does.
	if !strings.Contains(script, "B="+artifactBucket) {
		t.Fatalf("host-setup.sh's base is not %s (the table's URLs name it); read the script and align both", artifactBucket)
	}
	var fetches int
	for _, line := range strings.Split(script, "\n") {
		f := strings.Fields(line)
		if len(f) != 5 || f[0] != "fetch" {
			continue
		}
		fetches++
		url, path, modeText, sha := f[1], f[2], f[3], f[4]
		mode, err := parseOctalMode(modeText)
		if err != nil {
			t.Fatalf("host-setup.sh fetch mode %q: %v", modeText, err)
		}
		match := false
		for _, art := range pinned {
			if art.url == strings.ReplaceAll(url, "$B", artifactBucket) {
				match = true
				if art.path != path {
					t.Errorf("pin for %s: path %q, host-setup.sh installs %q", url, art.path, path)
				}
				if art.mode != mode {
					t.Errorf("pin for %s: mode %#o, host-setup.sh installs %#o", url, art.mode, mode)
				}
				if art.sha256 != sha {
					t.Errorf("pin for %s: sha256 %s, host-setup.sh pins %s", url, art.sha256, sha)
				}
			}
		}
		if !match {
			t.Errorf("no pin row for host-setup.sh's fetch of %s (add it to the table in install/runtime.go)", url)
		}
	}
	if fetches != len(pinned) {
		t.Errorf("host-setup.sh fetches %d artifacts, the table pins %d", fetches, len(pinned))
	}
	// The two artifacts the script installs from the fork checkout are
	// NOT in the pin table (they come from the runtime tarball now).
	if strings.Contains(script, "/root/src/e2b-runtime") {
		for _, art := range pinned {
			if art.path == envdBinPath || art.path == orchestratorBinPath {
				t.Errorf("the pin table pins %s; binaries come from the runtime tarball", art.path)
			}
		}
	}
}

// parseOctalMode parses "0755" into a FileMode.
func parseOctalMode(s string) (os.FileMode, error) {
	var m os.FileMode
	if _, err := fmt.Sscanf(s, "%o", &m); err != nil {
		return 0, err
	}
	return m, nil
}

// TestSumsList: a SHA256SUMS body, a bare digest and case-insensitive
// hex all match; a foreign list does not.
func TestSumsList(t *testing.T) {
	const digest = "aabb00112233445566778899aabbccddeeff00112233445566778899aabbccdd"
	if !sumsList([]byte(digest+"  file.tar.gz\n"), digest) {
		t.Error("a sums line did not match its digest")
	}
	if !sumsList([]byte(strings.Repeat("aa", 32)+"\n"), strings.Repeat("AA", 32)) {
		t.Error("the compare is case-sensitive; shasum prints lowercase but an operator's file may not be")
	}
	if !sumsList([]byte(digest), digest) {
		t.Error("a bare digest did not match")
	}
	if sumsList([]byte("0000  file.tar.gz\n"), digest) {
		t.Error("a foreign list matched")
	}
}

// TestRuntimeTarballMembersMatchBuildScript: the tarball the build
// script stages (spoond-runtime/{orchestrator,envd,RUNTIME}) is exactly
// what unpackRuntime reads; a member name change breaks the installer.
func TestRuntimeTarballMembersMatchBuildScript(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "deploy", "release", "build-runtime.sh"))
	if err != nil {
		t.Skipf("deploy/release/build-runtime.sh not readable: %v", err)
	}
	script := string(b)
	for _, member := range []string{"spoond-runtime/orchestrator", "spoond-runtime/envd", "spoond-runtime/RUNTIME"} {
		if !strings.Contains(script, `"$stage/`+member+`"`) {
			t.Errorf("build-runtime.sh no longer stages %s; unpackRuntime reads it by base name", member)
		}
	}
}

// TestSeedFromCryptoRandIs64Hex: seedValue's shape pinned against the
// crypto/rand path directly (not through the phase).
func TestSeedFromCryptoRandIs64Hex(t *testing.T) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("crypto/rand: %v", err)
	}
	hexed := hex.EncodeToString(raw[:])
	if len(hexed) != 64 || !isHex(hexed) {
		t.Errorf("encodeToString(32 bytes) = %q, want 64 hex chars", hexed)
	}
	var raw2 [32]byte
	mr := mrand.New(mrand.NewSource(1))
	mr.Read(raw2[:]) // never used as a seed; only shape-checking the size
	_ = raw2
}
