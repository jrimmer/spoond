// Tests for the images phase (images.go): a temp Root, a fake runner
// answering `spoond images list` from a captured sample, and a
// backend.env and an operator manifest written under the root. Every
// test pins one rule: the guest pieces land under /var/lib/spoond/guest
// and survive a rerun unchanged; none builds nothing and a manifest
// builds its baked entries; images already in the catalog are kept and
// the missing ones run exactly the env + build command, flags before
// `build`; no backend.env token value reaches any command line; a
// failed build fails the phase with the fix after the earlier images
// built; a dry run writes nothing and builds nothing.
package install

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sampleImagesList is captured `spoond images list` output, tabwriter
// shape and all: tools has a current build (seven fields), app does
// not (its empty BUILD and DIGEST cells collapse, leaving five).
const sampleImagesList = `NAME   BUILD                                 DIGEST                                        VCPU  MEM_MB  DISK_MB  UPDATED_AT
tools  5f0c3e9a-4b1d-4c2e-9a77-1d2e3f405162  localhost:5000/tools@sha256:1a2b3c4d5e6f7a8b  2     2048    6144     2026-10-16T08:00:00Z
app                                                                                       2     1024    4096     2026-10-16T07:00:00Z
`

// testManifest is the operator manifest the build tests pass as
// --images: three baked entries in file order and one that is not.
const testManifest = `images:
  - name: app
    baked: true
    dockerfile: app.dockerfile
  - name: tools
    baked: true
    dockerfile: tools.dockerfile
  - name: draft
    baked: false
    dockerfile: draft.dockerfile
  - name: db
    baked: true
    dockerfile: db.dockerfile
`

// testManifestPath is where writeManifest puts it (under the root).
const testManifestPath = "/srv/catalog/manifest.yaml"

// writeManifest writes testManifest under the root and points
// --images at it.
func writeManifest(t *testing.T, env *Env) {
	t.Helper()
	path := env.Resolve(testManifestPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(testManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	env.Opt.Images = testManifestPath
}

// listCmd is the catalog query the phase runs.
const listCmd = "/opt/spoond/spoond images list"

// catalogHeader is `spoond images list` output for an empty catalog.
const catalogHeader = "NAME     BUILD  DIGEST  VCPU  MEM_MB  DISK_MB  UPDATED_AT\n"

// imagesRunner is the fake runner for the images phase: it answers the
// catalog query with a fixed output and records everything else,
// failing builds of the images named in failBuilds.
type imagesRunner struct {
	calls []string
	// listOut is the `spoond images list` stdout.
	listOut []byte
	// listErr, when set, is the query's error.
	listErr error
	// failBuilds names the images whose build fails.
	failBuilds map[string]bool
}

func (r *imagesRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, call)
	if call == listCmd {
		return r.listOut, r.listErr
	}
	if strings.HasSuffix(call, " build") || strings.Contains(call, " build ") {
		if fields := strings.Fields(call); len(fields) > 0 {
			last := fields[len(fields)-1]
			if r.failBuilds[last] {
				return nil, fmt.Errorf("%s: exit status 1", call)
			}
		}
	}
	return nil, nil
}

// buildCalls returns the recorded build commands, in order.
func (r *imagesRunner) buildCalls() []string {
	var out []string
	for _, c := range r.calls {
		if strings.Contains(c, " build ") {
			out = append(out, c)
		}
	}
	return out
}

// buildNames returns the image names of the recorded build calls.
func (r *imagesRunner) buildNames() []string {
	var names []string
	for _, c := range r.buildCalls() {
		if i := strings.LastIndex(c, " build "); i >= 0 {
			names = append(names, c[i+len(" build "):])
		}
	}
	return names
}

// imagesTestEnv returns an Env over a temp root whose runner answers
// the catalog query with sampleImagesList, plus its report buffer.
func imagesTestEnv(t *testing.T) (*Env, *bytes.Buffer, *imagesRunner) {
	t.Helper()
	out := &bytes.Buffer{}
	runner := &imagesRunner{listOut: []byte(sampleImagesList)}
	env := &Env{Root: t.TempDir(), Run: runner, Out: out}
	return env, out, runner
}

// runImagesPhase runs the images phase once and fails the test on a
// phase error.
func runImagesPhase(t *testing.T, env *Env) []Item {
	t.Helper()
	items, err := RunPhase(context.Background(), env, PhaseImages, Phases[PhaseImages])
	if err != nil {
		t.Fatalf("images phase: %v", err)
	}
	return items
}

// writeBackendEnv writes a backend.env under the root: the five build
// keys plus two secrets that must never reach a command line.
func writeBackendEnv(t *testing.T, env *Env) {
	t.Helper()
	path := env.Resolve("/etc/spoond/backend.env")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := `BIND_ADDR=127.0.0.1:8890
ADMIN_TOKEN=admin-secret-value
CONSUMER_TOKENS=consumer-secret-value
IMAGE_REGISTRY=localhost:5000
SPOOND_DB_PATH=/var/lib/spoond/spoond.db
E2B_GRPC_ADDR=127.0.0.1:5008
E2B_TOKEN_SEED_FILE=/etc/spoond/e2b-token-seed
SPOOND_GUEST_DNS_ADDR=10.1.0.2
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// guestPaths are the embedded files a run must write, all 0755.
var guestPaths = []string{
	"/var/lib/spoond/guest/spoond-guest-init",
	"/var/lib/spoond/guest/spoond-tmux.sh",
}

// TestImagesPhaseWritesGuest pins where the guest pieces land and
// their mode: under /var/lib/spoond/guest, 0755. A second run keeps
// every file: writeFile compares before it writes.
func TestImagesPhaseWritesGuest(t *testing.T) {
	env, out, _ := imagesTestEnv(t)

	runImagesPhase(t, env)
	for _, path := range guestPaths {
		fi, err := os.Stat(env.Resolve(path))
		if err != nil {
			t.Fatalf("guest file %s: %v", path, err)
		}
		if fi.Size() == 0 {
			t.Errorf("guest file %s is empty", path)
		}
		if got := fi.Mode().Perm(); got != 0o755 {
			t.Errorf("%s mode = %#o, want 0755", path, got)
		}
	}
	if !strings.Contains(out.String(), "created") {
		t.Errorf("first run report, want created rows:\n%s", out.String())
	}

	out.Reset()
	items := runImagesPhase(t, env)
	kept := 0
	for _, it := range items {
		if it.Result == Kept && strings.HasPrefix(it.Name, "/var/lib/spoond/guest/") {
			kept++
		}
	}
	if kept != len(guestPaths) {
		t.Errorf("second run kept %d guest files, want %d; rows:\n%s", kept, len(guestPaths), out.String())
	}
}

// TestImagesPhaseNone pins the default: spoond ships no images, so
// the phase writes the guest pieces, reports one skipped row and runs
// nothing.
func TestImagesPhaseNone(t *testing.T) {
	for _, images := range []string{"", ImagesNone} {
		env, out, runner := imagesTestEnv(t)
		env.Opt.Images = images
		items := runImagesPhase(t, env)
		if n := len(items); n < 2 || items[n-1].Result != Skipped || items[n-1].Name != "images" {
			t.Fatalf("--images %q: last row = %+v, want the skipped row after the guest rows", images, items)
		}
		if len(runner.calls) != 0 {
			t.Errorf("--images %q ran commands: %v", images, runner.calls)
		}
		if !strings.Contains(out.String(), "spoond ships no images") {
			t.Errorf("--images %q report, want the no-images row:\n%s", images, out.String())
		}
	}
}

// TestImagesPhaseManifest pins the build set: every baked entry in
// file order, minus the one the catalog already has; draft is not
// baked and never in.
func TestImagesPhaseManifest(t *testing.T) {
	env, _, runner := imagesTestEnv(t)
	writeManifest(t, env)
	runImagesPhase(t, env)
	got := runner.buildNames()
	want := []string{"app", "db"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("built %v, want %v (calls: %v)", got, want, runner.calls)
	}
}

// TestImagesPhaseMissingManifest fails the phase with the read error
// before anything runs.
func TestImagesPhaseMissingManifest(t *testing.T) {
	env, _, runner := imagesTestEnv(t)
	env.Opt.Images = "/srv/none/manifest.yaml"
	_, err := RunPhase(context.Background(), env, PhaseImages, Phases[PhaseImages])
	if err == nil || !strings.Contains(err.Error(), "read the image manifest") {
		t.Fatalf("err = %v, want the manifest read failure", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("ran commands: %v", runner.calls)
	}
}

// TestImagesPhaseBuildCommand pins the exact build command: an `env`
// prefix of only the five backend.env keys that exist, then the
// binary, then the flags BEFORE `build` (Go's flag package stops at
// the first positional) with the manifest's directory as the context,
// then the image name.
func TestImagesPhaseBuildCommand(t *testing.T) {
	env, _, runner := imagesTestEnv(t)
	writeManifest(t, env)
	writeBackendEnv(t, env)
	runImagesPhase(t, env)

	want := "env IMAGE_REGISTRY=localhost:5000 SPOOND_DB_PATH=/var/lib/spoond/spoond.db " +
		"E2B_GRPC_ADDR=127.0.0.1:5008 E2B_TOKEN_SEED_FILE=/etc/spoond/e2b-token-seed " +
		"SPOOND_GUEST_DNS_ADDR=10.1.0.2 " +
		"/opt/spoond/spoond images -manifest /srv/catalog/manifest.yaml " +
		"-context /srv/catalog build app"
	if len(runner.calls) != 3 {
		t.Fatalf("calls = %v, want the list query and two builds", runner.calls)
	}
	if runner.calls[0] != listCmd {
		t.Errorf("first call = %q, want the catalog query", runner.calls[0])
	}
	if runner.calls[1] != want {
		t.Errorf("build call =\n  %s\nwant\n  %s", runner.calls[1], want)
	}
}

// TestImagesPhaseBuildEnvKeysFromBackendEnvOnly pins the secret rule:
// backend.env carries tokens, the recorded build lines carry none of
// them — every recorded call is scanned, not just the build.
func TestImagesPhaseBuildEnvKeysFromBackendEnvOnly(t *testing.T) {
	env, _, runner := imagesTestEnv(t)
	writeManifest(t, env)
	writeBackendEnv(t, env)
	runImagesPhase(t, env)

	for _, secret := range []string{"admin-secret-value", "consumer-secret-value", "BIND_ADDR=127.0.0.1:8890"} {
		for _, c := range runner.calls {
			if strings.Contains(c, secret) {
				t.Errorf("a backend.env value reached the command line:\n%s", c)
			}
		}
	}
	// And the five build keys do appear on the build line.
	builds := runner.buildCalls()
	if len(builds) == 0 {
		t.Fatal("no build ran")
	}
	for _, k := range []string{"IMAGE_REGISTRY=", "SPOOND_DB_PATH=", "E2B_GRPC_ADDR=", "E2B_TOKEN_SEED_FILE=", "SPOOND_GUEST_DNS_ADDR="} {
		if !strings.Contains(builds[0], k) {
			t.Errorf("build line lacks %s:\n%s", k, builds[0])
		}
	}
}

// TestImagesPhaseMissingBackendEnv builds without backend.env: the
// five keys are simply left out and the plain build command runs.
func TestImagesPhaseMissingBackendEnv(t *testing.T) {
	env, _, runner := imagesTestEnv(t)
	writeManifest(t, env)
	runImagesPhase(t, env)

	// Without backend.env no KEY=VALUE joins the prefix, but the
	// command still runs under env (1).
	want := "env /opt/spoond/spoond images -manifest /srv/catalog/manifest.yaml " +
		"-context /srv/catalog build app"
	if len(runner.calls) != 3 || runner.calls[1] != want {
		t.Errorf("calls = %v, want the query and %q first", runner.calls, want)
	}
}

// TestImagesPhaseFailedBuild pins the failure: the build after the
// good ones fails, the phase error carries the fix, the rows before it
// show the earlier images built.
func TestImagesPhaseFailedBuild(t *testing.T) {
	env, _, runner := imagesTestEnv(t)
	writeManifest(t, env)
	runner.listOut = []byte(catalogHeader) // an empty catalog: everything is missing
	runner.failBuilds = map[string]bool{"tools": true}

	items, err := RunPhase(context.Background(), env, PhaseImages, Phases[PhaseImages])
	if err == nil {
		t.Fatal("phase succeeded, want the tools build failure")
	}
	wantErr := "spoond images build tools failed; rerun `spoond install images` after fixing it"
	if err.Error() != "images: "+wantErr {
		t.Errorf("err = %q, want the phase-prefixed %q", err, wantErr)
	}
	var failed bool
	for _, it := range items {
		if it.Result == Failed && it.Name == "tools" && it.Note == wantErr {
			failed = true
		}
	}
	if !failed {
		t.Errorf("no failed row for tools with the fix; rows: %+v", items)
	}
	// Earlier images were built before the failure stopped the phase.
	got := runner.buildNames()
	if len(got) != 2 || got[0] != "app" || got[1] != "tools" {
		t.Errorf("built %v before stopping, want app then the failed tools", got)
	}
}

// TestImagesPhaseDryRun pins the dry run: no guest file is written,
// the catalog query may run, no build does.
func TestImagesPhaseDryRun(t *testing.T) {
	env, _, runner := imagesTestEnv(t)
	writeManifest(t, env)
	env.DryRun = true
	runImagesPhase(t, env)

	if _, err := os.Stat(env.Resolve("/var/lib/spoond/guest")); !os.IsNotExist(err) {
		t.Error("dry run wrote the guest pieces")
	}
	if builds := runner.buildCalls(); len(builds) != 0 {
		t.Errorf("dry run built images: %v", builds)
	}
	if len(runner.calls) != 1 || runner.calls[0] != listCmd {
		t.Errorf("calls = %v, want only the catalog query", runner.calls)
	}
}

// TestImagesPhaseBaseRegistry: --base-registry goes to each build
// (spoond images build pulls, falling back to a local build); without
// it every build is local, release binary or not: spoond publishes no
// images to pull.
func TestImagesPhaseBaseRegistry(t *testing.T) {
	for _, tc := range []struct {
		name, version, flag, want string
	}{
		{"flag", "v4.1.0", "registry.example.com/base", "build -base-registry registry.example.com/base app"},
		{"release builds locally", "v4.1.0", "", ""},
		{"dev builds locally", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, _, runner := imagesTestEnv(t)
			writeManifest(t, env)
			env.Opt.BaseRegistry = tc.flag
			env.Version = tc.version
			runImagesPhase(t, env)
			builds := runner.buildCalls()
			if len(builds) == 0 {
				t.Fatalf("no build ran; calls: %v", runner.calls)
			}
			if tc.want == "" && strings.Contains(builds[0], "-base-registry") {
				t.Errorf("build = %q, want a local build", builds[0])
			}
			if tc.want != "" && !strings.Contains(builds[0], tc.want) {
				t.Errorf("build = %q, want %q", builds[0], tc.want)
			}
		})
	}
}

// TestParseImagesList runs the parser over the captured sample and its
// edge cases: the header row, empty output and buildless rows stay
// out; a built row is named.
func TestParseImagesList(t *testing.T) {
	got := parseImagesList([]byte(sampleImagesList))
	if len(got) != 1 || got[0] != "tools" {
		t.Fatalf("parseImagesList(sample) = %v, want [tools]", got)
	}
	header := "NAME  BUILD  DIGEST  VCPU  MEM_MB  DISK_MB  UPDATED_AT\n"
	if got := parseImagesList([]byte(header)); got != nil {
		t.Errorf("header parsed as %v, want nothing", got)
	}
	if got := parseImagesList(nil); got != nil {
		t.Errorf("empty output parsed as %v, want nothing", got)
	}
	// An image literally named NAME with a build: the header line is
	// skipped by its NAME BUILD pair, the data row still wins.
	row := fmt.Sprintf("%-8s%-40s%-48s%-6s%-8s%-9s%s\n",
		"NAME", "b-1", "sha256:abc", "2", "1024", "4096", "2026-10-16T08:00:00Z")
	if got := parseImagesList([]byte(header + row)); len(got) != 1 || got[0] != "NAME" {
		t.Errorf("data row parsed as %v, want [NAME]", got)
	}
	// A buildless row for it (five fields after the collapse) is
	// not a current build.
	empty := fmt.Sprintf("%-8s%-6s%-8s%-9s%s\n", "NAME", "2", "1024", "4096", "2026-10-16T08:00:00Z")
	if got := parseImagesList([]byte(header + empty)); got != nil {
		t.Errorf("buildless row parsed as %v, want nothing", got)
	}
}
