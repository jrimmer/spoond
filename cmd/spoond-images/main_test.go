package spoondimages

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v3/sizes"
	"github.com/jrimmer/spoond/v3/store"
	"github.com/jrimmer/spoond/v3/substrate"
	"github.com/jrimmer/spoond/v3/substrate/fake"
)

// stubSubstrate records the BuildTemplate request and answers with a
// fixed result (or error). When storageRoot is set, BuildTemplate leaves
// the fresh build's files under root/<buildID> the way the orchestrator
// leaves them (#125).
type stubSubstrate struct {
	*fake.Fake
	req         substrate.BuildRequest
	reqs        []substrate.BuildRequest // every request, in order
	res         substrate.BuildResult
	err         error
	storageRoot string
	// beforeBuild runs at the top of BuildTemplate; tests use it to
	// cancel the caller's context mid-build (a SIGKILL/reboot stand-in).
	beforeBuild func()
}

func (s *stubSubstrate) BuildTemplate(ctx context.Context, req substrate.BuildRequest) (substrate.BuildResult, error) {
	if s.beforeBuild != nil {
		s.beforeBuild()
	}
	if err := ctx.Err(); err != nil {
		return substrate.BuildResult{}, err
	}
	s.req = req
	s.reqs = append(s.reqs, req)
	if s.storageRoot != "" {
		dir := filepath.Join(s.storageRoot, req.BuildID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return substrate.BuildResult{}, err
		}
		for i, n := range []int{4096, 8192} {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d", i)), make([]byte, n), 0o644); err != nil {
				return substrate.BuildResult{}, err
			}
		}
	}
	if s.err != nil {
		return substrate.BuildResult{}, s.err
	}
	return s.res, nil
}

// cmdLog records the commands runCmd saw, reset by setup.
var cmdLog []string

// testImage is a baked manifest entry with a dockerfile.
var testImage = manifestImage{
	Name:       "py-base",
	Baked:      true,
	Dockerfile: "py-base.dockerfile",
	VCPU:       2,
	MemoryMB:   1024,
	DiskMB:     4096,
	Env:        map[string]string{"PATH": "/usr/local/bin"},
}

// setup wires fakes for docker and the substrate and opens a temp DB.
func setup(t *testing.T, sub *stubSubstrate) *store.DB {
	t.Helper()
	cmdLog = nil
	runCmd = func(ctx context.Context, stdout io.Writer, name string, args ...string) error {
		cmdLog = append(cmdLog, name+" "+strings.Join(args, " "))
		return nil
	}
	runOut = func(ctx context.Context, name string, args ...string) (string, error) {
		return "localhost:5000/py-base@sha256:" + strings.Repeat("ab", 32), nil
	}
	runCapture = func(ctx context.Context, stdout io.Writer, name string, args ...string) (string, error) {
		cmdLog = append(cmdLog, name+" "+strings.Join(args, " "))
		return "", nil
	}
	newSubstrate = func() (substrate.Substrate, error) { return sub, nil }
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

const wantDigest = "localhost:5000/py-base@sha256:abababababababababababababababababababababababababababababababab"

// TestBuildOneSuccess: a successful build updates the image and build
// rows and runs docker build, push and inspect in order.
func TestBuildOneSuccess(t *testing.T) {
	sub := &stubSubstrate{res: substrate.BuildResult{
		KernelVersion: "vmlinux-6.1.158", FirecrackerVersion: "v1.14-0.2.0",
		EnvdVersion: "0.1.40", DiskSizeMB: 5000,
	}}
	db := setup(t, sub)
	ctx := context.Background()

	out := &bytes.Buffer{}
	if err := buildOne(ctx, db, sub, testImage, sizes.Default(), buildOpts{registry: "localhost:5000", contextDir: "images"}, out); err != nil {
		t.Fatalf("buildOne: %v", err)
	}

	wantCmds := []string{
		"docker build --pull -f images/py-base.dockerfile -t localhost:5000/py-base:latest images",
		"docker push localhost:5000/py-base:latest",
		"docker image rm localhost:5000/py-base:latest",
		"docker image prune -f",
		"docker builder prune -f --filter until=72h",
		"docker builder prune -f --keep-storage 10GB",
		"docker buildx prune -f --filter until=72h",
		"docker buildx prune -f --keep-storage 10GB",
	}
	if len(cmdLog) != len(wantCmds) {
		t.Fatalf("docker commands = %v", cmdLog)
	}
	for i, want := range wantCmds {
		if cmdLog[i] != want {
			t.Fatalf("docker command %d = %q, want %q", i, cmdLog[i], want)
		}
	}

	img, err := db.GetImage(ctx, "py-base")
	if err != nil {
		t.Fatalf("GetImage: %v", err)
	}
	if img.CurrentBuildID == "" || img.Digest != wantDigest ||
		img.TemplateID == "" || img.VCPU != 2 || img.MemoryMB != 1024 || img.DiskMB != 4096 ||
		img.StartCmd != startCmd || img.ReadyCmd != readyCmd {
		t.Fatalf("image row after build = %+v", img)
	}

	build, err := db.GetBuild(ctx, img.CurrentBuildID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if build.Kind != "template" || build.TemplateID != img.TemplateID || build.Image != "py-base" ||
		build.State != "ready" || build.Error != "" ||
		build.KernelVersion != "vmlinux-6.1.158" ||
		build.FirecrackerVersion != "v1.14-0.2.0" ||
		build.EnvdVersion != "0.1.40" || build.DiskMB != 5000 ||
		build.VCPU != 2 || build.MemoryMB != 1024 ||
		build.SizeBytes != 0 { // no storage root: the fake wrote nothing, so 0
		t.Fatalf("build row after build = %+v", build)
	}
	if build.CreatedAt.After(time.Now()) {
		t.Fatalf("build created_at in the future: %v", build.CreatedAt)
	}

	// The template build got the pushed digest ref, the sizing and the
	// guest commands.
	if sub.req.FromImage != wantDigest || sub.req.TemplateID != img.TemplateID ||
		sub.req.BuildID != build.BuildID ||
		sub.req.VCPU != 2 || sub.req.MemoryMB != 1024 || sub.req.DiskSizeMB != 4096 ||
		sub.req.StartCmd != startCmd || sub.req.ReadyCmd != readyCmd {
		t.Fatalf("BuildTemplate request = %+v", sub.req)
	}
	if want := "built py-base build=" + build.BuildID + " digest=" + wantDigest + "\n"; out.String() != want {
		t.Fatalf("stdout = %q, want %q", out.String(), want)
	}
}

// TestTemplateBuildSizeAtWriteTime: a template build records its disk
// size when it is written (#125), the same write-time measurement the
// service gives checkpoint and pause builds — not only after the next
// hourly accounting pass.
func TestTemplateBuildSizeAtWriteTime(t *testing.T) {
	sub := &stubSubstrate{res: substrate.BuildResult{
		KernelVersion: "vmlinux-6.1.158", FirecrackerVersion: "v1.14-0.2.0",
		EnvdVersion: "0.1.40", DiskSizeMB: 5000,
	}, storageRoot: t.TempDir()}
	// templateBuildSize reads the storage root from the environment,
	// like the service does.
	t.Setenv("E2B_TEMPLATE_STORAGE_PATH", sub.storageRoot)
	db := setup(t, sub)
	ctx := context.Background()

	if err := buildOne(ctx, db, sub, testImage, sizes.Default(), buildOpts{registry: "localhost:5000", contextDir: "images"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("buildOne: %v", err)
	}

	build, err := db.GetBuild(ctx, sub.req.BuildID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	// The stub wrote f0 (4096 B) and f1 (8192 B); compare against what
	// the OS allocated for them, the number the helper stores.
	want, err := store.BuildDiskUsage(filepath.Join(sub.storageRoot, sub.req.BuildID))
	if err != nil {
		t.Fatalf("measure build dir: %v", err)
	}
	if want < 8192 {
		t.Fatalf("allocated size = %d, want at least 8192", want)
	}
	if build.SizeBytes != want {
		t.Fatalf("template build size_bytes = %d, want %d (measured at write time)", build.SizeBytes, want)
	}
}

// TestBuildOneFailureKeepsImageRow: a failed template build marks the
// build row failed and leaves the image row describing the last good
// build.
func TestBuildOneFailureKeepsImageRow(t *testing.T) {
	sub := &stubSubstrate{}
	db := setup(t, sub)
	ctx := context.Background()

	// A last good build to keep.
	good := store.ImageRow{
		Name: "py-base", TemplateID: "tpl0123456789abcdefgh",
		CurrentBuildID: "b-good", Digest: wantDigest,
		VCPU: 2, MemoryMB: 1024, DiskMB: 4096,
		StartCmd: startCmd, ReadyCmd: readyCmd, UpdatedAt: time.Now(),
	}
	if err := db.UpsertImage(ctx, good); err != nil {
		t.Fatalf("seed image: %v", err)
	}

	sub.err = errors.New("template build failed: last logs:\nstep 3 died")
	err := buildOne(ctx, db, sub, testImage, sizes.Default(), buildOpts{registry: "localhost:5000", contextDir: "images"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("buildOne succeeded, want failure")
	}

	img, err := db.GetImage(ctx, "py-base")
	if err != nil {
		t.Fatalf("GetImage: %v", err)
	}
	if img.CurrentBuildID != "b-good" || img.Digest != wantDigest || img.TemplateID != good.TemplateID {
		t.Fatalf("image row changed after failure = %+v", img)
	}

	builds, err := db.ListBuilds(ctx)
	if err != nil || len(builds) != 1 {
		t.Fatalf("ListBuilds = %+v, %v", builds, err)
	}
	if builds[0].State != "failed" ||
		!strings.Contains(builds[0].Error, "template build failed") {
		t.Fatalf("failed build row = %+v", builds[0])
	}
}

// TestBuildOneFailureUpdateDetached: when BuildTemplate fails because
// the build context is already done (a timeout, or the caller gave up),
// the failure still reaches the store. A failure written through the
// cancelled context would be lost, leaving the row building forever and
// pinning its whole ancestor chain as a GC root (spoond-4yl).
func TestBuildOneFailureUpdateDetached(t *testing.T) {
	sub := &stubSubstrate{}
	// Cancel the build context between the building insert and the
	// BuildTemplate call, standing in for a timeout that fires exactly
	// when the orchestrator would have answered.
	var cancel context.CancelFunc
	sub.beforeBuild = func() { cancel() }
	db := setup(t, sub)
	ctx, c := context.WithCancel(context.Background())
	cancel = c

	if err := buildOne(ctx, db, sub, testImage, sizes.Default(), buildOpts{registry: "localhost:5000", contextDir: "images"}, &bytes.Buffer{}); err == nil {
		t.Fatal("buildOne succeeded, want a cancelled build to fail")
	}

	builds, err := db.ListBuilds(context.Background())
	if err != nil || len(builds) != 1 {
		t.Fatalf("ListBuilds = %+v, %v", builds, err)
	}
	if builds[0].State != "failed" {
		t.Fatalf("cancelled build state = %q, want failed", builds[0].State)
	}
	if !strings.Contains(builds[0].Error, "context canceled") {
		t.Fatalf("cancelled build error = %q", builds[0].Error)
	}
}

// TestTemplateIDStableAcrossBuilds: the template id is created once for
// the image name and reused by every later build.
func TestTemplateIDStableAcrossBuilds(t *testing.T) {
	sub := &stubSubstrate{}
	db := setup(t, sub)
	ctx := context.Background()

	if err := buildOne(ctx, db, sub, testImage, sizes.Default(), buildOpts{registry: "localhost:5000", contextDir: "images"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("first buildOne: %v", err)
	}
	firstTID := sub.req.TemplateID
	if firstTID == "" {
		t.Fatal("empty template id")
	}
	firstRow, err := db.GetImage(ctx, "py-base")
	if err != nil {
		t.Fatalf("GetImage 1: %v", err)
	}

	if err := buildOne(ctx, db, sub, testImage, sizes.Default(), buildOpts{registry: "localhost:5000", contextDir: "images"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("second buildOne: %v", err)
	}
	if sub.req.TemplateID != firstTID {
		t.Fatalf("template id regenerated: %s, want %s", sub.req.TemplateID, firstTID)
	}
	secondRow, err := db.GetImage(ctx, "py-base")
	if err != nil {
		t.Fatalf("GetImage 2: %v", err)
	}
	if secondRow.TemplateID != firstTID {
		t.Fatalf("row template id = %s, want %s", secondRow.TemplateID, firstTID)
	}
	if secondRow.CurrentBuildID == "" || secondRow.CurrentBuildID == firstRow.CurrentBuildID {
		t.Fatalf("second build id = %q", secondRow.CurrentBuildID)
	}
}

// TestBuildOneNoDockerfile: a baked entry without a dockerfile errors.
func TestBuildOneNoDockerfile(t *testing.T) {
	sub := &stubSubstrate{}
	db := setup(t, sub)
	img := testImage
	img.Dockerfile = ""
	if err := buildOne(context.Background(), db, sub, img, sizes.Default(), buildOpts{registry: "localhost:5000", contextDir: "images"}, &bytes.Buffer{}); err == nil {
		t.Fatal("buildOne succeeded without a dockerfile, want error")
	}
}

// A layer image built From a catalog image gets the base's digest as
// BASE, inherits its shape and env (its own env keys win), and passes
// its own build args, sorted.
func TestBuildOneFromBase(t *testing.T) {
	sub := &stubSubstrate{res: substrate.BuildResult{DiskSizeMB: 5000}}
	db := setup(t, sub)
	ctx := context.Background()
	if err := buildOne(ctx, db, sub, testImage, sizes.Default(), buildOpts{registry: "localhost:5000", contextDir: "images"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("base build: %v", err)
	}
	cmdLog = nil

	layer := manifestImage{
		Name: "py-base-worker", Baked: true, Dockerfile: "worker.dockerfile", From: "py-base",
		Env:       map[string]string{"HOME": "/root"},
		BuildArgs: map[string]string{"WARM": "true", "A": "1"},
	}
	if err := buildOne(ctx, db, sub, layer, sizes.Default(), buildOpts{registry: "localhost:5000", contextDir: "images"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("layer build: %v", err)
	}
	want := "docker build --pull --build-arg A=1 --build-arg BASE=" + wantDigest +
		" --build-arg WARM=true --build-arg WARM_ENV=export HOME='/root'; export PATH='/usr/local/bin';" +
		" -f images/worker.dockerfile -t localhost:5000/py-base-worker:latest images"
	if len(cmdLog) == 0 || cmdLog[0] != want {
		t.Fatalf("docker build = %q, want %q", cmdLog, want)
	}
	row, err := db.GetImage(ctx, "py-base-worker")
	if err != nil {
		t.Fatalf("get layer: %v", err)
	}
	if row.VCPU != 2 || row.MemoryMB != 1024 || row.Env["PATH"] != "/usr/local/bin" || row.Env["HOME"] != "/root" {
		t.Fatalf("layer row = %+v, want the base's shape and merged env", row)
	}
}

// resolveBase injects the deployment's guest DNS resolver as a build
// arg: from SPOOND_GUEST_DNS_ADDR when the manifest entry does not set
// its own, absent when neither does, and the manifest entry wins when
// both do.
func TestResolveBaseGuestDNS(t *testing.T) {
	db := setup(t, &stubSubstrate{})
	img := manifestImage{Name: "x", Dockerfile: "x.dockerfile"}
	has := func(flags []string, want string) bool {
		for _, f := range flags {
			if f == want {
				return true
			}
		}
		return false
	}

	t.Setenv("SPOOND_GUEST_DNS_ADDR", "")
	if _, flags, err := resolveBase(context.Background(), db, img); err != nil {
		t.Fatalf("resolveBase: %v", err)
	} else if has(flags, "SPOOND_GUEST_DNS_ADDR=") {
		t.Fatalf("unset env must not inject a resolver, flags = %v", flags)
	}

	t.Setenv("SPOOND_GUEST_DNS_ADDR", "10.0.0.2")
	if _, flags, err := resolveBase(context.Background(), db, img); err != nil {
		t.Fatalf("resolveBase: %v", err)
	} else if !has(flags, "SPOOND_GUEST_DNS_ADDR=10.0.0.2") {
		t.Fatalf("env resolver not injected, flags = %v", flags)
	}

	img.BuildArgs = map[string]string{"SPOOND_GUEST_DNS_ADDR": "192.0.2.53"}
	if _, flags, err := resolveBase(context.Background(), db, img); err != nil {
		t.Fatalf("resolveBase: %v", err)
	} else if !has(flags, "SPOOND_GUEST_DNS_ADDR=192.0.2.53") || has(flags, "SPOOND_GUEST_DNS_ADDR=10.0.0.2") {
		t.Fatalf("manifest build_arg must win, flags = %v", flags)
	}
}

// TestBuildOnePruneCommands: after a build both dockerd's default
// builder and the current buildx builder are pruned, by age and by the
// IMAGE_BUILD_CACHE_KEEP size cap (spoond-vdoq: the age rule alone let
// a day of builds keep 14 GB of fresh cache). Mutation: drop a prune
// command, or pass no --keep-storage.
func TestBuildOnePruneCommands(t *testing.T) {
	sub := &stubSubstrate{}
	db := setup(t, sub)
	t.Setenv("IMAGE_BUILD_CACHE_KEEP", "4GB")

	if err := buildOne(context.Background(), db, sub, testImage, sizes.Default(), buildOpts{registry: "localhost:5000", contextDir: "images"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("buildOne: %v", err)
	}
	var prunes []string
	for _, c := range cmdLog {
		if strings.Contains(c, "prune") {
			prunes = append(prunes, c)
		}
	}
	want := []string{
		"docker image prune -f",
		"docker builder prune -f --filter until=72h",
		"docker builder prune -f --keep-storage 4GB",
		"docker buildx prune -f --filter until=72h",
		"docker buildx prune -f --keep-storage 4GB",
	}
	if !slices.Equal(prunes, want) {
		t.Fatalf("prune commands = %v, want %v", prunes, want)
	}
}

// TestBuildOnePushedRefRemoved: after the digest is read the exact ref
// just pushed is removed locally (spoond-07fn: the tagged local copy
// filled the root disk otherwise), before the prunes. Mutation: move
// the rm before the inspect, drop it, or swap the order with a prune.
func TestBuildOnePushedRefRemoved(t *testing.T) {
	sub := &stubSubstrate{res: substrate.BuildResult{DiskSizeMB: 5000}}
	db := setup(t, sub)
	// Log the digest read too, so the order can be asserted.
	runOut = func(ctx context.Context, name string, args ...string) (string, error) {
		cmdLog = append(cmdLog, name+" "+strings.Join(args, " "))
		return wantDigest, nil
	}
	if err := buildOne(context.Background(), db, sub, testImage, sizes.Default(), buildOpts{registry: "localhost:5000", contextDir: "images"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("buildOne: %v", err)
	}
	var got []string
	for i, c := range cmdLog {
		switch {
		case strings.HasSuffix(c, "push localhost:5000/py-base:latest"),
			strings.HasPrefix(c, "docker inspect"),
			strings.HasPrefix(c, "docker image rm"),
			strings.Contains(c, "prune"):
			got = append(got, fmt.Sprintf("%d: %s", i, c))
		}
	}
	want := []string{
		"1: docker push localhost:5000/py-base:latest",
		"2: docker inspect --format {{index .RepoDigests 0}} localhost:5000/py-base:latest",
		"3: docker image rm localhost:5000/py-base:latest",
		"4: docker image prune -f",
		"5: docker builder prune -f --filter until=72h",
		"6: docker builder prune -f --keep-storage 10GB",
		"7: docker buildx prune -f --filter until=72h",
		"8: docker buildx prune -f --keep-storage 10GB",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("post-build steps = %v, want %v (rm of the pushed ref after push/inspect, before the prunes)", got, want)
	}
}

// TestBuildOneRmFailureBestEffort: a failing `docker image rm` never
// fails the build; it warns like the prunes do.
func TestBuildOneRmFailureBestEffort(t *testing.T) {
	sub := &stubSubstrate{res: substrate.BuildResult{DiskSizeMB: 5000}}
	db := setup(t, sub)
	// The inspect (runOut above) answers a digest, only the rm fails.
	runCmd = func(ctx context.Context, stdout io.Writer, name string, args ...string) error {
		cmdLog = append(cmdLog, name+" "+strings.Join(args, " "))
		if len(args) == 3 && args[0] == "image" && args[1] == "rm" {
			return errors.New("image is referenced in one or more repositories")
		}
		return nil
	}
	// The digest read succeeds (runOut above answers a digest), only the
	// rm fails.
	out := &bytes.Buffer{}
	err := buildOne(context.Background(), db, sub, testImage, sizes.Default(), buildOpts{registry: "localhost:5000", contextDir: "images"}, out)
	if err != nil {
		t.Fatalf("buildOne: %v", err)
	}
	if !strings.Contains(out.String(), "warning: docker image rm") {
		t.Fatalf("stdout %q misses the image rm warning", out.String())
	}
	row, err := db.GetImage(context.Background(), testImage.Name)
	if err != nil {
		t.Fatalf("GetImage: %v", err)
	}
	if row.Digest != wantDigest || row.CurrentBuildID == "" {
		t.Fatalf("image row = %+v", row)
	}
}

// TestBuildOnePruneFailureBestEffort: every prune failing (a broken
// docker, say) must not fail the build; the default builder's prunes
// warn on stdout, the buildx prunes stay silent (a missing buildx
// plugin is the normal case, not a build problem).
func TestBuildOnePruneFailureBestEffort(t *testing.T) {
	sub := &stubSubstrate{res: substrate.BuildResult{DiskSizeMB: 5000}}
	db := setup(t, sub)
	runCmd = func(ctx context.Context, stdout io.Writer, name string, args ...string) error {
		cmdLog = append(cmdLog, name+" "+strings.Join(args, " "))
		for _, a := range args {
			if a == "prune" {
				return errors.New("no such command")
			}
		}
		return nil
	}

	out := &bytes.Buffer{}
	if err := buildOne(context.Background(), db, sub, testImage, sizes.Default(), buildOpts{registry: "localhost:5000", contextDir: "images"}, out); err != nil {
		t.Fatalf("buildOne: %v", err)
	}
	if n := len(cmdLog); n != 8 {
		t.Fatalf("docker commands = %d, want all 8 (build, push, inspect, image rm and 4 prunes)", n)
	}
	if len(sub.reqs) != 1 {
		t.Fatalf("BuildTemplate calls = %d, want 1", len(sub.reqs))
	}
	if !strings.Contains(out.String(), "warning: docker image prune") ||
		!strings.Contains(out.String(), "warning: docker builder prune") {
		t.Fatalf("stdout %q misses the image/builder prune warnings", out.String())
	}
	if strings.Contains(out.String(), "buildx") {
		t.Fatalf("stdout %q warns on the buildx prunes, want silent skip", out.String())
	}
}

// TestBuildOneWarmSizes: an entry with warm_sizes gets its default build
// and then one template build per preset, from the same digest at the
// preset's shape and the image's disk, each recorded as a ready template
// build and as the image's warm size; min_memory_mb lands on the image
// row. A rebuild that drops a preset stops serving it. Mutation: skip
// the warm loop (no extra builds, no rows), or build it at the image's
// shape (requests mismatch).
func TestBuildOneWarmSizes(t *testing.T) {
	sub := &stubSubstrate{res: substrate.BuildResult{DiskSizeMB: 5000}}
	db := setup(t, sub)
	ctx := context.Background()
	img := testImage
	img.MinMemoryMB = 1024
	img.WarmSizes = []string{"medium", "large"}

	out := &bytes.Buffer{}
	if err := buildOne(ctx, db, sub, img, sizes.Default(), buildOpts{registry: "localhost:5000", contextDir: "images"}, out); err != nil {
		t.Fatalf("buildOne: %v", err)
	}
	row, err := db.GetImage(ctx, "py-base")
	if err != nil {
		t.Fatalf("GetImage: %v", err)
	}
	if row.MinMemoryMB != 1024 || row.VCPU != 2 || row.MemoryMB != 1024 {
		t.Fatalf("image row = %+v", row)
	}
	if len(sub.reqs) != 3 {
		t.Fatalf("BuildTemplate calls = %d, want default + 2 warm sizes", len(sub.reqs))
	}
	for i, want := range [][2]uint32{{2, 1024}, {2, 4096}, {4, 8192}} {
		r := sub.reqs[i]
		if r.VCPU != want[0] || r.MemoryMB != want[1] || r.DiskSizeMB != 4096 ||
			r.FromImage != wantDigest || r.TemplateID != row.TemplateID {
			t.Fatalf("BuildTemplate %d = %+v, want %d/%d from the image's digest", i, r, want[0], want[1])
		}
	}
	if sub.reqs[0].BuildID != row.CurrentBuildID {
		t.Fatalf("current build %s is not the default-size build %s", row.CurrentBuildID, sub.reqs[0].BuildID)
	}
	warm, err := db.ListImageSizes(ctx, "py-base")
	if err != nil {
		t.Fatalf("ListImageSizes: %v", err)
	}
	if len(warm) != 2 || warm[0].Size != "large" || warm[0].BuildID != sub.reqs[2].BuildID ||
		warm[0].VCPU != 4 || warm[0].MemoryMB != 8192 ||
		warm[1].Size != "medium" || warm[1].BuildID != sub.reqs[1].BuildID ||
		warm[1].VCPU != 2 || warm[1].MemoryMB != 4096 {
		t.Fatalf("image sizes = %+v", warm)
	}
	for _, w := range warm {
		b, err := db.GetBuild(ctx, w.BuildID)
		if err != nil {
			t.Fatalf("GetBuild %s: %v", w.BuildID, err)
		}
		if b.Kind != "template" || b.State != "ready" || b.Image != "py-base" || b.TemplateID != row.TemplateID ||
			b.VCPU != w.VCPU || b.MemoryMB != w.MemoryMB || b.DiskMB != 5000 {
			t.Fatalf("warm build row = %+v", b)
		}
	}
	if !strings.Contains(out.String(), "built py-base size=large build="+sub.reqs[2].BuildID+"\n") {
		t.Fatalf("stdout = %q", out.String())
	}

	// warm_sizes now [large]: medium is no longer served, large is the
	// new build.
	img.WarmSizes = []string{"large"}
	sub.reqs = nil
	if err := buildOne(ctx, db, sub, img, sizes.Default(), buildOpts{registry: "localhost:5000", contextDir: "images"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	warm, err = db.ListImageSizes(ctx, "py-base")
	if err != nil {
		t.Fatalf("ListImageSizes: %v", err)
	}
	if len(sub.reqs) != 2 || len(warm) != 1 || warm[0].Size != "large" || warm[0].BuildID != sub.reqs[1].BuildID {
		t.Fatalf("after rebuild: %d builds, sizes %+v", len(sub.reqs), warm)
	}
}

// TestBuildOneWarmSizeRejected: an unknown preset, or one whose memory
// is below the image's effective minimum, fails before docker or any
// template build runs. Mutation: drop either check in warmPresets.
func TestBuildOneWarmSizeRejected(t *testing.T) {
	for _, tc := range []struct {
		name  string
		minMB int
		warm  []string
		want  string
	}{
		{"unknown preset", 0, []string{"huge"}, `warm size "huge" is not a size preset`},
		{"below min_memory_mb", 4096, []string{"small"}, "below the image's min_memory_mb 4096"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub := &stubSubstrate{}
			db := setup(t, sub)
			img := testImage
			img.MinMemoryMB = tc.minMB
			img.WarmSizes = tc.warm
			err := buildOne(context.Background(), db, sub, img, sizes.Default(), buildOpts{registry: "localhost:5000", contextDir: "images"}, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if len(cmdLog) != 0 || len(sub.reqs) != 0 {
				t.Fatalf("built anyway: docker %v, templates %d", cmdLog, len(sub.reqs))
			}
		})
	}
}

// TestBuildOneFromBaseInheritsSizes: a layer built From a catalog image
// inherits its min_memory_mb and warm sizes unless it sets its own; an
// explicit empty warm_sizes builds none.
func TestBuildOneFromBaseInheritsSizes(t *testing.T) {
	sub := &stubSubstrate{res: substrate.BuildResult{DiskSizeMB: 5000}}
	db := setup(t, sub)
	ctx := context.Background()
	base := testImage
	base.MinMemoryMB = 768
	base.WarmSizes = []string{"large"}
	if err := buildOne(ctx, db, sub, base, sizes.Default(), buildOpts{registry: "localhost:5000", contextDir: "images"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("base build: %v", err)
	}

	layer := manifestImage{Name: "py-base-worker", Baked: true, Dockerfile: "worker.dockerfile", From: "py-base"}
	if err := buildOne(ctx, db, sub, layer, sizes.Default(), buildOpts{registry: "localhost:5000", contextDir: "images"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("layer build: %v", err)
	}
	row, err := db.GetImage(ctx, "py-base-worker")
	if err != nil {
		t.Fatalf("get layer: %v", err)
	}
	warm, err := db.ListImageSizes(ctx, "py-base-worker")
	if err != nil {
		t.Fatalf("ListImageSizes: %v", err)
	}
	if row.MinMemoryMB != 768 || len(warm) != 1 || warm[0].Size != "large" {
		t.Fatalf("layer min_memory_mb %d, warm sizes %+v; want the base's", row.MinMemoryMB, warm)
	}

	layer.WarmSizes = []string{}
	if err := buildOne(ctx, db, sub, layer, sizes.Default(), buildOpts{registry: "localhost:5000", contextDir: "images"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("layer rebuild: %v", err)
	}
	if warm, _ := db.ListImageSizes(ctx, "py-base-worker"); len(warm) != 0 {
		t.Fatalf("explicit empty warm_sizes kept %+v", warm)
	}
}

// TestCmdBuildUnknownWarmSize: `spoond images build` reads warm_sizes
// from the manifest and refuses an unknown preset before opening the
// catalog or running docker.
func TestCmdBuildUnknownWarmSize(t *testing.T) {
	setup(t, &stubSubstrate{})
	t.Setenv("SIZE_PRESETS", "") // the default presets: medium, not huge
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(manifestPath, []byte(`images:
  - name: py-base
    baked: true
    dockerfile: py-base.dockerfile
    vcpu: 2
    memory_mb: 1024
    disk_mb: 4096
    warm_sizes: [medium, huge]
`), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	dbPath := filepath.Join(dir, "never.db")
	if code := cmdBuild(context.Background(), []string{"py-base"}, false, manifestPath, dbPath,
		buildOpts{registry: "localhost:5000", contextDir: "images"}); code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	if len(cmdLog) != 0 {
		t.Fatalf("docker ran: %v", cmdLog)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("catalog opened before the preset check: %v", err)
	}
}

// A From naming an image with no build is refused before docker runs.
func TestBuildOneFromUnknownBase(t *testing.T) {
	sub := &stubSubstrate{}
	db := setup(t, sub)
	layer := manifestImage{Name: "x-worker", Dockerfile: "worker.dockerfile", From: "nope"}
	err := buildOne(context.Background(), db, sub, layer, sizes.Default(), buildOpts{registry: "localhost:5000", contextDir: "images"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "base image nope is not in the catalog") {
		t.Fatalf("err = %v", err)
	}
	if len(cmdLog) != 0 {
		t.Fatalf("docker ran: %v", cmdLog)
	}
}

// publish runs over a temp manifest and a recording runCapture, with
// docker failures set per image name ("none" by default).
type publishHarness struct {
	manifestPath string
	errs         map[string]string // image name -> docker error text
}

func newPublishHarness(t *testing.T, imagesYAML string) *publishHarness {
	t.Helper()
	setup(t, &stubSubstrate{})
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(manifestPath, []byte(imagesYAML), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	h := &publishHarness{manifestPath: manifestPath}
	runCapture = func(ctx context.Context, stdout io.Writer, name string, args ...string) (string, error) {
		cmdLog = append(cmdLog, name+" "+strings.Join(args, " "))
		for _, a := range args {
			if img, ok := strings.CutPrefix(a, "localhost:5000/"); ok {
				img, _, _ := strings.Cut(img, ":")
				if msg := h.errs[img]; msg != "" {
					return msg, errors.New("exit status 1")
				}
			}
		}
		return "", nil
	}
	return h
}

// TestPublishTwoImages: publish runs pull, tag, push and the
// best-effort rm in order, with exactly the right refs, for every image
// named.
func TestPublishTwoImages(t *testing.T) {
	h := newPublishHarness(t, `images:
  - name: py-base
    baked: true
  - name: go-base
    baked: true
  - name: rust-base
    baked: false
`)
	code := cmdPublish(context.Background(), []string{"py-base", "go-base"}, false,
		h.manifestPath, "localhost:5000", "ghcr.io/jrimmer/spoond-images", "v4.0.0")
	if code != 0 {
		t.Fatalf("exit code %d, want 0", code)
	}
	want := []string{
		"docker pull localhost:5000/py-base:latest",
		"docker tag localhost:5000/py-base:latest ghcr.io/jrimmer/spoond-images/py-base:v4.0.0",
		"docker push ghcr.io/jrimmer/spoond-images/py-base:v4.0.0",
		"docker image rm ghcr.io/jrimmer/spoond-images/py-base:v4.0.0 localhost:5000/py-base:latest",
		"docker pull localhost:5000/go-base:latest",
		"docker tag localhost:5000/go-base:latest ghcr.io/jrimmer/spoond-images/go-base:v4.0.0",
		"docker push ghcr.io/jrimmer/spoond-images/go-base:v4.0.0",
		"docker image rm ghcr.io/jrimmer/spoond-images/go-base:v4.0.0 localhost:5000/go-base:latest",
	}
	if !slices.Equal(cmdLog, want) {
		t.Fatalf("commands = %v, want %v", cmdLog, want)
	}
}

// TestPublishAllBakedOnly: --all publishes every baked: true entry and
// skips the rest (baked: false rust-base).
func TestPublishAllBakedOnly(t *testing.T) {
	h := newPublishHarness(t, `images:
  - name: py-base
    baked: true
  - name: rust-base
    baked: false
  - name: go-base
    baked: true
`)
	code := cmdPublish(context.Background(), nil, true, h.manifestPath, "localhost:5000", "example.com/base", "v1")
	if code != 0 {
		t.Fatalf("exit code %d, want 0", code)
	}
	for _, c := range cmdLog {
		if strings.Contains(c, "rust-base") {
			t.Fatalf("published non-baked rust-base: %v", cmdLog)
		}
	}
	if n := strings.Count(strings.Join(cmdLog, "\n"), "docker pull "); n != 2 {
		t.Fatalf("pulls = %d, want 2 (py-base, go-base): %v", n, cmdLog)
	}
}

// TestPublishMissingImageContinues: an image missing from the local
// registry fails that image alone with a pointer at `spoond images
// build <name>`, the rest still publish, and the exit code is 1.
func TestPublishMissingImageContinues(t *testing.T) {
	h := newPublishHarness(t, `images:
  - name: py-base
    baked: true
  - name: go-base
    baked: true
  - name: elixir-base
    baked: true
`)
	h.errs = map[string]string{"go-base": "Error response from daemon: manifest unknown"}
	code := cmdPublish(context.Background(), nil, true, h.manifestPath, "localhost:5000", "example.com/base", "v1")
	if code != 1 {
		t.Fatalf("exit code %d, want 1 (a failed publish must fail the run)", code)
	}
	// py-base and elixir-base still publish; go-base stops at the pull.
	var pulls, pushes, goBaseCmds int
	for _, c := range cmdLog {
		switch {
		case strings.HasPrefix(c, "docker pull "):
			pulls++
		case strings.HasPrefix(c, "docker push "):
			pushes++
		}
		if strings.Contains(c, "go-base") {
			goBaseCmds++
		}
	}
	if pulls != 3 || pushes != 2 {
		t.Fatalf("pulls = %d, pushes = %d over %v, want 3 and 2", pulls, pushes, cmdLog)
	}
	if goBaseCmds != 1 {
		t.Fatalf("go-base ran %d commands, want 1 (the failing pull): %v", goBaseCmds, cmdLog)
	}
	// The failure names the missing image and how to make it exist.
	err := publishOne(context.Background(), "go-base", "localhost:5000", "example.com/base", "v1", io.Discard)
	if err == nil || !strings.Contains(err.Error(),
		"build go-base first (spoond images build go-base)") {
		t.Fatalf("err = %v, want the build-first hint", err)
	}
}

// TestPublishPushAuthErrorSaysLogin: a push rejected as unauthenticated
// tells the operator to `docker login` the registry host first.
func TestPublishPushAuthErrorSaysLogin(t *testing.T) {
	h := newPublishHarness(t, `images:
  - name: py-base
    baked: true
`)
	code := cmdPublish(context.Background(), []string{"py-base"}, false,
		h.manifestPath, "localhost:5000", "ghcr.io/jrimmer/spoond-images", "v4.0.0")
	if code != 0 {
		t.Fatalf("exit code %d, want 0", code)
	}
	// Now fail the push with an auth error: the run fails, no rm runs,
	// and the hint names the registry host to log in to.
	runCapture = func(ctx context.Context, stdout io.Writer, name string, args ...string) (string, error) {
		cmdLog = append(cmdLog, name+" "+strings.Join(args, " "))
		if len(args) > 0 && args[0] == "push" {
			return "unauthorized: authentication required", errors.New("exit status 1")
		}
		return "", nil
	}
	cmdLog = nil
	code = cmdPublish(context.Background(), []string{"py-base"}, false,
		h.manifestPath, "localhost:5000", "ghcr.io/jrimmer/spoond-images", "v4.0.0")
	if code != 1 {
		t.Fatalf("exit code %d, want 1", code)
	}
	for _, c := range cmdLog {
		if strings.HasPrefix(c, "docker image rm") {
			t.Fatalf("rm ran after a failed push: %v", cmdLog)
		}
	}
	err := publishOne(context.Background(), "py-base", "localhost:5000",
		"ghcr.io/jrimmer/spoond-images", "v4.0.0", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "docker login ghcr.io first") {
		t.Fatalf("err = %v, want a docker login hint", err)
	}
}

// TestPublishNeedsPrefix: publish without --prefix, without a tag, or
// without a name is a usage error, and an unknown image name is refused
// before any docker command.
func TestPublishNeedsPrefix(t *testing.T) {
	h := newPublishHarness(t, `images:
  - name: py-base
    baked: true
`)
	if code := cmdPublish(context.Background(), []string{"py-base"}, false, h.manifestPath, "localhost:5000", "", "v1"); code != 2 {
		t.Fatalf("missing --prefix: exit %d, want 2", code)
	}
	if code := cmdPublish(context.Background(), []string{"py-base"}, false, h.manifestPath, "localhost:5000", "example.com/base", ""); code != 2 {
		t.Fatalf("empty --tag: exit %d, want 2", code)
	}
	if code := cmdPublish(context.Background(), nil, false, h.manifestPath, "localhost:5000", "example.com/base", "v1"); code != 2 {
		t.Fatalf("no name or --all: exit %d, want 2", code)
	}
	if code := cmdPublish(context.Background(), []string{"nope"}, false, h.manifestPath, "localhost:5000", "example.com/base", "v1"); code != 1 {
		t.Fatalf("unknown name: exit %d, want 1", code)
	}
	if len(cmdLog) != 0 {
		t.Fatalf("docker ran on a usage error: %v", cmdLog)
	}
}

// TestBuildOnePullsPublishedBase: with -base-registry the docker build is
// replaced by a pull of the published tag re-tagged to the registry ref,
// and the rest of the build (push, digest read, template build) runs
// unchanged. Mutation: keep the docker build, or tag the wrong ref.
func TestBuildOnePullsPublishedBase(t *testing.T) {
	sub := &stubSubstrate{res: substrate.BuildResult{DiskSizeMB: 5000}}
	db := setup(t, sub)
	opts := buildOpts{
		registry: "localhost:5000", contextDir: "images",
		baseRegistry: "ghcr.io/jrimmer/spoond-images", baseTag: "v4.0.0",
	}
	out := &bytes.Buffer{}
	if err := buildOne(context.Background(), db, sub, testImage, sizes.Default(), opts, out); err != nil {
		t.Fatalf("buildOne: %v", err)
	}
	if len(cmdLog) < 2 || cmdLog[0] != "docker pull ghcr.io/jrimmer/spoond-images/py-base:v4.0.0" ||
		cmdLog[1] != "docker tag ghcr.io/jrimmer/spoond-images/py-base:v4.0.0 localhost:5000/py-base:latest" {
		t.Fatalf("commands = %v, want pull then tag of the published base", cmdLog)
	}
	for _, c := range cmdLog {
		if strings.HasPrefix(c, "docker build ") {
			t.Fatalf("docker build ran despite -base-registry: %v", cmdLog)
		}
	}
	if cmdLog[2] != "docker push localhost:5000/py-base:latest" {
		t.Fatalf("after the tag: %v, want the push", cmdLog[2])
	}
	// Both tags leave dockerd after the push (spoond-07fn).
	wantRm := "docker image rm localhost:5000/py-base:latest ghcr.io/jrimmer/spoond-images/py-base:v4.0.0"
	if !slices.Contains(cmdLog, wantRm) {
		t.Fatalf("commands = %v, want %q", cmdLog, wantRm)
	}
	// The pulled image is treated exactly like a built one: digest,
	// template build, catalog row.
	row, err := db.GetImage(context.Background(), "py-base")
	if err != nil || row.Digest != wantDigest || row.CurrentBuildID == "" {
		t.Fatalf("image row = %+v, %v", row, err)
	}
	if len(sub.reqs) != 1 || sub.reqs[0].FromImage != wantDigest {
		t.Fatalf("BuildTemplate requests = %+v", sub.reqs)
	}
}

// TestBuildOnePullFallbackBuilds: a pull that finds nothing prints one
// line and falls back to the local docker build, with the same argv a
// build without -base-registry would run.
func TestBuildOnePullFallbackBuilds(t *testing.T) {
	sub := &stubSubstrate{res: substrate.BuildResult{DiskSizeMB: 5000}}
	db := setup(t, sub)
	runCapture = func(ctx context.Context, stdout io.Writer, name string, args ...string) (string, error) {
		cmdLog = append(cmdLog, name+" "+strings.Join(args, " "))
		return "manifest unknown", errors.New("exit status 1")
	}
	opts := buildOpts{
		registry: "localhost:5000", contextDir: "images",
		baseRegistry: "ghcr.io/jrimmer/spoond-images", baseTag: "latest",
	}
	out := &bytes.Buffer{}
	if err := buildOne(context.Background(), db, sub, testImage, sizes.Default(), opts, out); err != nil {
		t.Fatalf("buildOne: %v", err)
	}
	if cmdLog[0] != "docker pull ghcr.io/jrimmer/spoond-images/py-base:latest" {
		t.Fatalf("first command = %q, want the pull", cmdLog[0])
	}
	wantBuild := "docker build --pull -f images/py-base.dockerfile -t localhost:5000/py-base:latest images"
	if cmdLog[1] != wantBuild {
		t.Fatalf("fallback build = %q, want %q (byte-identical to the no-flag build)", cmdLog[1], wantBuild)
	}
	if !strings.Contains(out.String(), "building locally") {
		t.Fatalf("stdout %q misses the fallback line", out.String())
	}
	row, err := db.GetImage(context.Background(), "py-base")
	if err != nil || row.CurrentBuildID == "" {
		t.Fatalf("image row = %+v, %v", row, err)
	}
}

// TestBuildOneFallbackKeepsBuildArgs: resolveBase runs even when the
// pull succeeds the build away — and its build args are there when the
// pull fails and the local build runs (a From layer still gets BASE).
func TestBuildOneFallbackKeepsBuildArgs(t *testing.T) {
	sub := &stubSubstrate{res: substrate.BuildResult{DiskSizeMB: 5000}}
	db := setup(t, sub)
	if err := buildOne(context.Background(), db, sub, testImage, sizes.Default(),
		buildOpts{registry: "localhost:5000", contextDir: "images"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("base build: %v", err)
	}
	cmdLog = nil
	runCapture = func(ctx context.Context, stdout io.Writer, name string, args ...string) (string, error) {
		cmdLog = append(cmdLog, name+" "+strings.Join(args, " "))
		return "manifest unknown", errors.New("exit status 1")
	}
	layer := manifestImage{
		Name: "py-base-worker", Baked: true, Dockerfile: "worker.dockerfile", From: "py-base",
		BuildArgs: map[string]string{"WARM": "true"},
	}
	opts := buildOpts{
		registry: "localhost:5000", contextDir: "images",
		baseRegistry: "ghcr.io/jrimmer/spoond-images", baseTag: "latest",
	}
	if err := buildOne(context.Background(), db, sub, layer, sizes.Default(), opts, &bytes.Buffer{}); err != nil {
		t.Fatalf("layer build: %v", err)
	}
	if cmdLog[0] != "docker pull ghcr.io/jrimmer/spoond-images/py-base-worker:latest" {
		t.Fatalf("first command = %q, want the pull", cmdLog[0])
	}
	want := "docker build --pull --build-arg BASE=" + wantDigest +
		" --build-arg WARM=true --build-arg WARM_ENV=export PATH='/usr/local/bin';" +
		" -f images/worker.dockerfile -t localhost:5000/py-base-worker:latest images"
	if cmdLog[1] != want {
		t.Fatalf("fallback build = %q, want %q", cmdLog[1], want)
	}
}

// TestCmdBuildBaseRegistry: the -base-registry/-base-tag flags reach
// buildOne for every target, and the catalog ends up as if built.
func TestCmdBuildBaseRegistry(t *testing.T) {
	sub := &stubSubstrate{res: substrate.BuildResult{DiskSizeMB: 5000}}
	setup(t, sub)
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(manifestPath, []byte(`images:
  - name: py-base
    baked: true
    dockerfile: py-base.dockerfile
  - name: go-base
    baked: true
    dockerfile: go-base.dockerfile
`), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	code := cmdBuild(context.Background(), nil, true, manifestPath, filepath.Join(dir, "cat.db"),
		buildOpts{
			registry: "localhost:5000", contextDir: "images",
			baseRegistry: "ghcr.io/jrimmer/spoond-images", baseTag: "v4.0.0",
		})
	if code != 0 {
		t.Fatalf("exit code %d, want 0", code)
	}
	var pulls []string
	for _, c := range cmdLog {
		if strings.HasPrefix(c, "docker pull ") {
			pulls = append(pulls, c)
		}
		if strings.HasPrefix(c, "docker build ") {
			t.Fatalf("docker build ran with -base-registry: %v", cmdLog)
		}
	}
	want := []string{
		"docker pull ghcr.io/jrimmer/spoond-images/py-base:v4.0.0",
		"docker pull ghcr.io/jrimmer/spoond-images/go-base:v4.0.0",
	}
	if !slices.Equal(pulls, want) {
		t.Fatalf("pulls = %v, want %v", pulls, want)
	}
	if len(sub.reqs) != 2 {
		t.Fatalf("BuildTemplate calls = %d, want 2", len(sub.reqs))
	}
	ctx := context.Background()
	cat, err := store.Open(filepath.Join(dir, "cat.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	if _, err := cat.GetImage(ctx, "go-base"); err != nil {
		t.Fatalf("GetImage go-base: %v", err)
	}
}

// TestBuildOneWithoutBaseRegistryIsUnchanged: with no -base-registry the
// docker build argv is byte-identical to the pre-R5 build (the flags
// default to nothing, no extra docker command appears).
func TestBuildOneWithoutBaseRegistryIsUnchanged(t *testing.T) {
	sub := &stubSubstrate{res: substrate.BuildResult{DiskSizeMB: 5000}}
	db := setup(t, sub)
	if err := buildOne(context.Background(), db, sub, testImage, sizes.Default(),
		buildOpts{registry: "localhost:5000", contextDir: "images"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("buildOne: %v", err)
	}
	want := []string{
		"docker build --pull -f images/py-base.dockerfile -t localhost:5000/py-base:latest images",
		"docker push localhost:5000/py-base:latest",
	}
	if len(cmdLog) < 2 || cmdLog[0] != want[0] || cmdLog[1] != want[1] {
		t.Fatalf("commands = %v, want %v", cmdLog, want)
	}
	for _, c := range cmdLog {
		if strings.HasPrefix(c, "docker pull") || strings.HasPrefix(c, "docker tag") {
			t.Fatalf("unexpected pull/tag without -base-registry: %v", cmdLog)
		}
	}
}
