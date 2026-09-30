package spoondimages

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/store"
	"github.com/jrimmer/spoond/substrate"
	"github.com/jrimmer/spoond/substrate/fake"
)

// stubSubstrate records the BuildTemplate request and answers with a
// fixed result (or error).
type stubSubstrate struct {
	*fake.Fake
	req substrate.BuildRequest
	res substrate.BuildResult
	err error
}

func (s *stubSubstrate) BuildTemplate(ctx context.Context, req substrate.BuildRequest) (substrate.BuildResult, error) {
	s.req = req
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
		build.VCPU != 2 || build.MemoryMB != 1024 {
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
