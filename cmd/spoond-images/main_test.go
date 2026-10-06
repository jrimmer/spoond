package spoondimages

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
	"github.com/jrimmer/spoond/v2/substrate/fake"
)

// stubSubstrate records the BuildTemplate request and answers with a
// fixed result (or error). When storageRoot is set, BuildTemplate leaves
// the fresh build's files under root/<buildID> the way the orchestrator
// leaves them (#125).
type stubSubstrate struct {
	*fake.Fake
	req         substrate.BuildRequest
	res         substrate.BuildResult
	err         error
	storageRoot string
}

func (s *stubSubstrate) BuildTemplate(ctx context.Context, req substrate.BuildRequest) (substrate.BuildResult, error) {
	s.req = req
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
	if err := buildOne(ctx, db, sub, testImage, "localhost:5000", "images", out); err != nil {
		t.Fatalf("buildOne: %v", err)
	}

	wantCmds := []string{
		"docker build --pull -f images/py-base.dockerfile -t localhost:5000/py-base:latest images",
		"docker push localhost:5000/py-base:latest",
		"docker image prune -f",
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

	if err := buildOne(ctx, db, sub, testImage, "localhost:5000", "images", &bytes.Buffer{}); err != nil {
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
	err := buildOne(ctx, db, sub, testImage, "localhost:5000", "images", &bytes.Buffer{})
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

// TestTemplateIDStableAcrossBuilds: the template id is created once for
// the image name and reused by every later build.
func TestTemplateIDStableAcrossBuilds(t *testing.T) {
	sub := &stubSubstrate{}
	db := setup(t, sub)
	ctx := context.Background()

	if err := buildOne(ctx, db, sub, testImage, "localhost:5000", "images", &bytes.Buffer{}); err != nil {
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

	if err := buildOne(ctx, db, sub, testImage, "localhost:5000", "images", &bytes.Buffer{}); err != nil {
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
	if err := buildOne(context.Background(), db, sub, img, "localhost:5000", "images", &bytes.Buffer{}); err == nil {
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
	if err := buildOne(ctx, db, sub, testImage, "localhost:5000", "images", &bytes.Buffer{}); err != nil {
		t.Fatalf("base build: %v", err)
	}
	cmdLog = nil

	layer := manifestImage{
		Name: "py-base-worker", Baked: true, Dockerfile: "worker.dockerfile", From: "py-base",
		Env:       map[string]string{"HOME": "/root"},
		BuildArgs: map[string]string{"WARM": "true", "A": "1"},
	}
	if err := buildOne(ctx, db, sub, layer, "localhost:5000", "images", &bytes.Buffer{}); err != nil {
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

// A From naming an image with no build is refused before docker runs.
func TestBuildOneFromUnknownBase(t *testing.T) {
	sub := &stubSubstrate{}
	db := setup(t, sub)
	layer := manifestImage{Name: "x-worker", Dockerfile: "worker.dockerfile", From: "nope"}
	err := buildOne(context.Background(), db, sub, layer, "localhost:5000", "images", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "base image nope is not in the catalog") {
		t.Fatalf("err = %v", err)
	}
	if len(cmdLog) != 0 {
		t.Fatalf("docker ran: %v", cmdLog)
	}
}
