// Tests for the images phase (images.go): a temp Root, a fake runner
// answering `spoond images list` from a captured sample, and a
// backend.env written under the root. Every test pins one rule: the
// context lands under /var/lib/spoond/images with the hook modes and
// survives a rerun unchanged; none/minimal/all pick the right names;
// images already in the catalog are kept and the missing ones run
// exactly the env + build command, flags before `build`; no backend.env
// token value reaches any command line; a failed build fails the phase
// with the fix after the earlier images built; a dry run writes
// nothing and builds nothing.
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
// shape and all: go-base has a current build (seven fields), py-base
// does not (its empty BUILD and DIGEST cells collapse, leaving five).
const sampleImagesList = `NAME     BUILD                                 DIGEST                                          VCPU  MEM_MB  DISK_MB  UPDATED_AT
go-base  5f0c3e9a-4b1d-4c2e-9a77-1d2e3f405162  localhost:5000/go-base@sha256:1a2b3c4d5e6f7a8b  2     2048    6144     2026-10-16T08:00:00Z
py-base                                                                                        2     1024    4096     2026-10-16T07:00:00Z
`

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

// contextPaths are the embedded files a written context must have,
// with their modes: the .sh hooks 0755, everything else 0644.
var contextPaths = map[string]os.FileMode{
	"/var/lib/spoond/images/manifest.yaml":                0o644,
	"/var/lib/spoond/images/py-base.dockerfile":           0o644,
	"/var/lib/spoond/images/go-base.dockerfile":           0o644,
	"/var/lib/spoond/images/scylla-init-hook.sh":          0o755,
	"/var/lib/spoond/images/elixir-base-postgres-hook.sh": 0o755,
	"/var/lib/spoond/images/guest/spoond-guest-init":      0o644,
	"/var/lib/spoond/images/guest/spoond-tmux.sh":         0o755,
}

// TestImagesPhaseWritesContext pins where the build context lands and
// its modes: under /var/lib/spoond/images with the layout kept, the
// .sh hooks 0755 and the rest 0644 (guest/spoond-guest-init is
// chmod'ed by COPY --chmod inside the dockerfile, not on disk). A
// second run keeps every file: writeFile compares before it writes.
func TestImagesPhaseWritesContext(t *testing.T) {
	env, out, _ := imagesTestEnv(t)
	env.Opt.Images = ImagesNone // the context write is what this test watches

	runImagesPhase(t, env)
	for path, want := range contextPaths {
		b, err := os.ReadFile(env.Resolve(path))
		if err != nil {
			t.Fatalf("context file %s: %v", path, err)
		}
		if len(b) == 0 {
			t.Errorf("context file %s is empty", path)
		}
		fi, err := os.Stat(env.Resolve(path))
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode = %#o, want %#o", path, got, want)
		}
	}
	if _, err := os.Stat(env.Resolve("/var/lib/spoond/images/README.md")); !os.IsNotExist(err) {
		t.Error("README.md was written; it is not build context")
	}
	if !strings.Contains(out.String(), "created") {
		t.Errorf("first run report, want created rows:\n%s", out.String())
	}

	// Second run: every context file kept, content untouched.
	out.Reset()
	before := map[string][]byte{}
	for path := range contextPaths {
		b, _ := os.ReadFile(env.Resolve(path))
		before[path] = b
	}
	items := runImagesPhase(t, env)
	for path := range contextPaths {
		b, err := os.ReadFile(env.Resolve(path))
		if err != nil || string(b) != string(before[path]) {
			t.Errorf("second run changed %s: %v", path, err)
		}
	}
	kept := 0
	for _, it := range items {
		if it.Result == Kept && strings.HasPrefix(it.Name, "/var/lib/spoond/images/") {
			kept++
		}
	}
	// 17 files ride in the embedded context (pinned in
	// images/embed_test.go); a stricter floor here would churn with
	// every new image.
	if kept < len(contextPaths) {
		t.Errorf("second run kept %d context files, want at least %d; rows:\n%s",
			kept, len(contextPaths), out.String())
	}
}

// TestImagesPhaseSelection pins none/minimal/all against the runner:
// none is one skipped row and returns; minimal builds py-base and
// go-base; all builds every baked manifest entry (rust-base is
// baked: false and never in).
func TestImagesPhaseSelection(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		env, out, runner := imagesTestEnv(t)
		env.Opt.Images = ImagesNone
		items := runImagesPhase(t, env)
		// The context write comes first, so its rows are there too;
		// the last row is the none verdict.
		if n := len(items); n < 2 || items[n-1].Result != Skipped || items[n-1].Name != "images" {
			t.Fatalf("last row = %+v, want the skipped none row after the context rows", items)
		}
		for _, it := range items[:len(items)-1] {
			if strings.HasPrefix(it.Name, "py-") || strings.HasPrefix(it.Name, "go-") || it.Result == Failed {
				t.Fatalf("none built rows beyond the context: %+v", items)
			}
		}
		if len(runner.calls) != 0 {
			t.Errorf("none ran commands: %v", runner.calls)
		}
		if !strings.Contains(out.String(), "skipped") {
			t.Errorf("report, want a skipped row:\n%s", out.String())
		}
	})

	t.Run("minimal", func(t *testing.T) {
		env, _, runner := imagesTestEnv(t)
		env.Opt.Images = ImagesMinimal
		runImagesPhase(t, env)
		// go-base already has a current build in the sample
		// catalog, so only py-base is missing.
		got := runner.buildNames()
		want := []string{"py-base"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("built %v, want %v (calls: %v)", got, want, runner.calls)
		}
	})

	t.Run("all", func(t *testing.T) {
		env, _, runner := imagesTestEnv(t)
		env.Opt.Images = ImagesAll
		runImagesPhase(t, env)
		// All the baked entries but the one the sample catalog
		// already has.
		got := runner.buildNames()
		want := []string{"py-base", "dev-base", "elixir-base", "elixir-release",
			"llm-review", "rust-qt-base", "scylla"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("built %v, want the manifest's baked images %v", got, want)
		}
	})
}

// TestImagesPhaseBuildCommand pins the exact build command: an `env`
// prefix of only the five backend.env keys that exist, then the
// binary, then the flags BEFORE `build` (Go's flag package stops at
// the first positional), then the image name.
func TestImagesPhaseBuildCommand(t *testing.T) {
	env, _, runner := imagesTestEnv(t)
	env.Opt.Images = ImagesMinimal
	writeBackendEnv(t, env)
	runImagesPhase(t, env)

	want := "env IMAGE_REGISTRY=localhost:5000 SPOOND_DB_PATH=/var/lib/spoond/spoond.db " +
		"E2B_GRPC_ADDR=127.0.0.1:5008 E2B_TOKEN_SEED_FILE=/etc/spoond/e2b-token-seed " +
		"SPOOND_GUEST_DNS_ADDR=10.1.0.2 " +
		"/opt/spoond/spoond images -manifest /var/lib/spoond/images/manifest.yaml " +
		"-context /var/lib/spoond/images build py-base"
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %v, want the list query and one build", runner.calls)
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
	env.Opt.Images = ImagesAll
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
	env.Opt.Images = ImagesMinimal
	runImagesPhase(t, env)

	// Without backend.env no KEY=VALUE joins the prefix, but the
	// command still runs under env (1).
	want := "env /opt/spoond/spoond images -manifest /var/lib/spoond/images/manifest.yaml " +
		"-context /var/lib/spoond/images build py-base"
	if len(runner.calls) != 2 || runner.calls[1] != want {
		t.Errorf("calls = %v, want the query and %q", runner.calls, want)
	}
}

// TestImagesPhaseFailedBuild pins the failure: the build after the
// good ones fails, the phase error carries the fix, the rows before it
// show the earlier images built.
func TestImagesPhaseFailedBuild(t *testing.T) {
	env, _, runner := imagesTestEnv(t)
	env.Opt.Images = ImagesAll
	runner.listOut = []byte(catalogHeader) // an empty catalog: everything is missing
	runner.failBuilds = map[string]bool{"go-base": true}

	items, err := RunPhase(context.Background(), env, PhaseImages, Phases[PhaseImages])
	if err == nil {
		t.Fatal("phase succeeded, want the go-base build failure")
	}
	wantErr := "spoond images build go-base failed; rerun `spoond install images` after fixing it"
	if err.Error() != "images: "+wantErr {
		t.Errorf("err = %q, want the phase-prefixed %q", err, wantErr)
	}
	var failed bool
	for _, it := range items {
		if it.Result == Failed && it.Name == "go-base" && it.Note == wantErr {
			failed = true
		}
	}
	if !failed {
		t.Errorf("no failed row for go-base with the fix; rows: %+v", items)
	}
	// Earlier images were built before the failure stopped the phase.
	got := runner.buildNames()
	if len(got) != 2 || got[0] != "py-base" || got[1] != "go-base" {
		t.Errorf("built %v before stopping, want py-base then the failed go-base", got)
	}
}

// TestImagesPhaseDryRun pins the dry run: no context file is written,
// the catalog query may run, no build does.
func TestImagesPhaseDryRun(t *testing.T) {
	env, _, runner := imagesTestEnv(t)
	env.Opt.Images = ImagesMinimal
	env.DryRun = true
	runImagesPhase(t, env)

	if _, err := os.Stat(env.Resolve("/var/lib/spoond/images")); !os.IsNotExist(err) {
		t.Error("dry run wrote the build context")
	}
	if builds := runner.buildCalls(); len(builds) != 0 {
		t.Errorf("dry run built images: %v", builds)
	}
	if len(runner.calls) != 1 || runner.calls[0] != listCmd {
		t.Errorf("calls = %v, want only the catalog query", runner.calls)
	}
}

// TestImagesPhaseBaseRegistrySkipped pins the slice boundary:
// --base-registry says it is not implemented here and the build runs
// locally anyway.
func TestImagesPhaseBaseRegistrySkipped(t *testing.T) {
	env, out, runner := imagesTestEnv(t)
	env.Opt.Images = ImagesMinimal
	env.Opt.BaseRegistry = "ghcr.io/jrimmer/spoond-images"
	runImagesPhase(t, env)

	if len(runner.buildNames()) != 1 {
		t.Errorf("base-registry run did not build locally; calls: %v", runner.calls)
	}
	if !strings.Contains(out.String(), "not implemented in this slice") {
		t.Errorf("report, want the base-registry skipped row:\n%s", out.String())
	}
}

// TestParseImagesList runs the parser over the captured sample and its
// edge cases: the header row, empty output and buildless rows stay
// out; a built row is named.
func TestParseImagesList(t *testing.T) {
	got := parseImagesList([]byte(sampleImagesList))
	if len(got) != 1 || got[0] != "go-base" {
		t.Fatalf("parseImagesList(sample) = %v, want [go-base]", got)
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
