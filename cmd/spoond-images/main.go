// Command spoond-images is spoond's image pipeline (U07): every image is
// defined by a Dockerfile in images/, built with docker, pushed to the
// local registry, and turned into an E2B template build recorded in
// SQLite.
//
// Usage:
//
//	spoond images build <name>   build one image
//	spoond images build --all    build every manifest entry with baked: true
//	spoond images list           print the image catalog
//
// Flags:
//
//	--manifest  image manifest path (default images/manifest.yaml)
//	--context   docker build context directory (default images)
//	--db        SQLite database path (default $SPOOND_DB_PATH)
//	--registry  registry to push to (default $IMAGE_REGISTRY, localhost:5000)
//
// Runs on vm2 as root and talks to the orchestrator via
// substrate/e2b.FromEnv().
package spoondimages

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jrimmer/spoond/store"
	"github.com/jrimmer/spoond/substrate"
	"github.com/jrimmer/spoond/substrate/e2b"
)

const (
	startCmd = "/usr/local/bin/spoond-guest-init"
	readyCmd = "test -f /run/spoond-guest-ready"

	buildTimeout = 60 * time.Minute
)

// runCmd streams a command's stdout (docker build, docker push).
var runCmd = func(ctx context.Context, stdout io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runOut captures a command's stdout (docker inspect).
var runOut = func(ctx context.Context, name string, args ...string) (string, error) {
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &buf
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(buf.String()), nil
}

// newSubstrate builds the E2B client from the environment; tests
// replace it with a fake.
var newSubstrate = func() (substrate.Substrate, error) {
	cfg, err := e2b.FromEnv()
	if err != nil {
		return nil, err
	}
	return e2b.New(cfg)
}

// manifestImage is one entry of images/manifest.yaml.
type manifestImage struct {
	Name        string            `yaml:"name"`
	Capability  string            `yaml:"capability"`
	Labels      []string          `yaml:"labels"`
	Description string            `yaml:"description"`
	Baked       bool              `yaml:"baked"`
	Dockerfile  string            `yaml:"dockerfile"`
	VCPU        int               `yaml:"vcpu"`
	MemoryMB    int               `yaml:"memory_mb"`
	DiskMB      int               `yaml:"disk_mb"`
	Env         map[string]string `yaml:"env"`
	Notes       string            `yaml:"notes"`
}

type manifest struct {
	Images []manifestImage `yaml:"images"`
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Main runs the images subcommand and returns the process exit code.
// Flags are accepted before or after the subcommand and its arguments
// (spoond images list --db ..., spoond images build --all --manifest ...).
func Main(args []string) int {
	fs := flag.NewFlagSet("images", flag.ContinueOnError)
	manifestPath := fs.String("manifest", "images/manifest.yaml", "image manifest path")
	contextDir := fs.String("context", "images", "docker build context directory")
	dbPath := fs.String("db", envOr("SPOOND_DB_PATH", "/var/lib/spoond/spoond.db"), "SQLite database path")
	registry := fs.String("registry", envOr("IMAGE_REGISTRY", "localhost:5000"), "image registry")
	all := fs.Bool("all", false, "build every manifest entry with baked: true")

	// Parse, harvesting positional arguments (the flag package stops at
	// the first one), until everything is consumed.
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		rest = rest[1:]
	}
	if len(positional) == 0 {
		usage(fs)
		return 2
	}

	ctx := context.Background()
	switch positional[0] {
	case "list":
		return cmdList(ctx, *dbPath)
	case "build":
		return cmdBuild(ctx, positional[1:], *all, *manifestPath, *contextDir, *dbPath, *registry)
	default:
		fmt.Fprintf(os.Stderr, "spoond images: unknown command %q\n\n", positional[0])
		usage(fs)
		return 2
	}
}

func usage(fs *flag.FlagSet) {
	fmt.Fprintf(os.Stderr, "usage: spoond images [-manifest path] [-context dir] [-db path] [-registry host:port] <command>\n\ncommands:\n  build <name>  build one image and turn it into an E2B template build\n  build --all   build every manifest entry with baked: true\n  list          print the image catalog\n\nflags:\n")
	fs.PrintDefaults()
}

// loadManifest reads and parses the manifest file.
func loadManifest(path string) (manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return manifest{}, fmt.Errorf("read manifest: %w", err)
	}
	var m manifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		return manifest{}, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	return m, nil
}

func cmdBuild(ctx context.Context, names []string, all bool, manifestPath, contextDir, dbPath, registry string) int {
	if !all && len(names) == 0 {
		fmt.Fprintln(os.Stderr, "spoond images: build needs a name or --all")
		return 2
	}

	m, err := loadManifest(manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "spoond images: %v\n", err)
		return 1
	}

	var targets []manifestImage
	if all {
		for _, img := range m.Images {
			if img.Baked {
				targets = append(targets, img)
			}
		}
	} else {
		for _, name := range names {
			found := false
			for _, img := range m.Images {
				if img.Name == name {
					targets = append(targets, img)
					found = true
					break
				}
			}
			if !found {
				fmt.Fprintf(os.Stderr, "spoond images: %s: not in %s\n", name, manifestPath)
				return 1
			}
		}
	}

	db, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "spoond images: %v\n", err)
		return 1
	}
	defer db.Close()
	sub, err := newSubstrate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "spoond images: %v\n", err)
		return 1
	}

	for _, img := range targets {
		if err := buildOne(ctx, db, sub, img, registry, contextDir, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "spoond images: %s: %v\n", img.Name, err)
			return 1
		}
	}
	return 0
}

// buildOne builds and pushes one image, then starts and waits for its
// E2B template build, recording everything in the catalog. On failure
// the build row is marked failed and the image row keeps describing the
// last good build.
func buildOne(ctx context.Context, db *store.DB, sub substrate.Substrate, img manifestImage, registry, contextDir string, stdout io.Writer) error {
	if img.Dockerfile == "" {
		return errors.New("manifest entry has no dockerfile")
	}
	ref := registry + "/" + img.Name + ":latest"

	// Build and push with docker, streaming the output.
	if err := runCmd(ctx, stdout, "docker", "build", "--pull",
		"-f", filepath.Join(contextDir, img.Dockerfile), "-t", ref, contextDir); err != nil {
		return fmt.Errorf("docker build: %w", err)
	}
	if err := runCmd(ctx, stdout, "docker", "push", ref); err != nil {
		return fmt.Errorf("docker push: %w", err)
	}
	digest, err := runOut(ctx, "docker", "inspect", "--format", "{{index .RepoDigests 0}}", ref)
	if err != nil {
		return fmt.Errorf("docker inspect: %w", err)
	}
	if digest == "" {
		return errors.New("docker inspect: empty digest")
	}

	// The image row (and its template id) is created before the first
	// template build; it is stable for the life of the image name.
	row, err := db.GetImage(ctx, img.Name)
	if errors.Is(err, store.ErrNotFound) {
		row = store.ImageRow{
			Name:       img.Name,
			TemplateID: e2b.NewTemplateID(),
			VCPU:       img.VCPU,
			MemoryMB:   img.MemoryMB,
			DiskMB:     img.DiskMB,
			StartCmd:   startCmd,
			ReadyCmd:   readyCmd,
			UpdatedAt:  time.Now(),
		}
		if err := db.UpsertImage(ctx, row); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	buildID := e2b.NewUUID()
	now := time.Now()
	if err := db.InsertBuild(ctx, store.BuildRow{
		BuildID:    buildID,
		Kind:       "template",
		TemplateID: row.TemplateID,
		Image:      img.Name,
		State:      "building",
		VCPU:       img.VCPU,
		MemoryMB:   img.MemoryMB,
		DiskMB:     img.DiskMB,
		CreatedAt:  now,
		UpdatedAt:  now,
	}); err != nil {
		return err
	}

	bctx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	res, err := sub.BuildTemplate(bctx, substrate.BuildRequest{
		TemplateID: row.TemplateID,
		BuildID:    buildID,
		FromImage:  digest,
		VCPU:       uint32(img.VCPU),
		MemoryMB:   uint32(img.MemoryMB),
		DiskSizeMB: uint32(img.DiskMB),
		StartCmd:   startCmd,
		ReadyCmd:   readyCmd,
	})
	if err != nil {
		if uerr := db.UpdateBuildState(ctx, buildID, "failed", err.Error(), nil); uerr != nil {
			return errors.Join(err, uerr)
		}
		return err
	}

	if err := db.UpdateBuildState(ctx, buildID, "ready", "", &store.BuildRow{
		KernelVersion:      res.KernelVersion,
		FirecrackerVersion: res.FirecrackerVersion,
		EnvdVersion:        res.EnvdVersion,
		DiskMB:             int(res.DiskSizeMB),
	}); err != nil {
		return err
	}
	if err := db.UpsertImage(ctx, store.ImageRow{
		Name:           img.Name,
		TemplateID:     row.TemplateID,
		CurrentBuildID: buildID,
		Digest:         digest,
		VCPU:           img.VCPU,
		MemoryMB:       img.MemoryMB,
		DiskMB:         img.DiskMB,
		StartCmd:       startCmd,
		ReadyCmd:       readyCmd,
		UpdatedAt:      time.Now(),
	}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "built %s build=%s digest=%s\n", img.Name, buildID, digest)
	return nil
}

func cmdList(ctx context.Context, dbPath string) int {
	db, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "spoond images: %v\n", err)
		return 1
	}
	defer db.Close()
	images, err := db.ListImages(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "spoond images: %v\n", err)
		return 1
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tBUILD\tDIGEST\tVCPU\tMEM_MB\tDISK_MB\tUPDATED_AT")
	for _, img := range images {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%d\t%s\n",
			img.Name, img.CurrentBuildID, shortDigest(img.Digest),
			img.VCPU, img.MemoryMB, img.DiskMB, img.UpdatedAt.Format(time.RFC3339))
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "spoond images: %v\n", err)
		return 1
	}
	return 0
}

// shortDigest shortens "<registry>/<name>@sha256:<64 hex>" to
// "sha256:<12 hex>"; "" when the digest is unset.
func shortDigest(digest string) string {
	_, hex, ok := strings.Cut(digest, "@sha256:")
	if !ok {
		return ""
	}
	if len(hex) > 12 {
		hex = hex[:12]
	}
	return "sha256:" + hex
}
