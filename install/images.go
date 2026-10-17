// The images phase: the image catalog (docs/plans/2026-10-16-4.0-install.md,
// the `images` row). It writes the embedded build context
// (github.com/jrimmer/spoond/v3/images) to /var/lib/spoond/images, picks
// the images the install wants from --images, asks the installed binary
// which of them already have a current build, and runs one
// `spoond images build` per missing image. The build's environment
// values come from /etc/spoond/backend.env and go on the command line
// as a plain `env KEY=VALUE` prefix — and only the five keys the build
// needs: no token or other backend.env secret ever reaches an argv.
package install

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jrimmer/spoond/v3/images"
)

func init() { Phases[PhaseImages] = imagesPhase }

const (
	// imagesContextPath is where the embedded build context lands,
	// the directory layout kept: manifest.yaml, the dockerfiles,
	// the hooks and keys, guest/.
	imagesContextPath = "/var/lib/spoond/images"
	// imagesBackendEnvPath is the env file the build values come
	// from (the spoond phase wrote it).
	imagesBackendEnvPath = "/etc/spoond/backend.env"
	// imagesSpoondBin is the installed binary the catalog query and
	// every build run through.
	imagesSpoondBin = "/opt/spoond/spoond"
)

// imagesBuildEnvKeys are the backend.env keys an image build needs,
// in the order they join the `env` prefix. Everything else in
// backend.env — admin and consumer tokens, LLM keys — stays off the
// command line.
var imagesBuildEnvKeys = []string{
	"IMAGE_REGISTRY",
	"SPOOND_DB_PATH",
	"E2B_GRPC_ADDR",
	"E2B_TOKEN_SEED_FILE",
	"SPOOND_GUEST_DNS_ADDR",
}

// imagesMinimalNames is the `minimal` catalog: the two base images
// (--images minimal, the default).
var imagesMinimalNames = []string{"py-base", "go-base"}

func imagesPhase(ctx context.Context, env *Env) ([]Item, error) {
	r := &Reporter{Phase: PhaseImages, Out: env.Out}

	// The build context: a release install has only the spoond
	// binary, so the context ships inside it and lands here before
	// anything else. Unchanged files are kept.
	if err := imagesWriteContext(env, r); err != nil {
		return r.Items(), err
	}

	// Which images this install wants.
	var names []string
	switch env.Opt.images() {
	case ImagesNone:
		r.Add(Skipped, "images", "--images none: no catalog built")
		return r.Items(), nil
	case ImagesAll:
		var err error
		if names, err = imagesManifestBaked(); err != nil {
			return r.Items(), err
		}
	default: // ImagesMinimal
		names = imagesMinimalNames
	}

	// What the catalog already has.
	built, err := imagesWithBuild(ctx, env)
	if err != nil {
		return r.Items(), err
	}

	// --base-registry is a later slice: say so, build locally.
	if env.Opt.BaseRegistry != "" {
		r.Add(Skipped, "--base-registry", "not implemented in this slice; building the images locally")
	}

	// Build the missing ones, one at a time: a failure stops the
	// phase with the fix.
	for _, name := range names {
		if built[name] {
			r.Add(Kept, name, "current build already in the catalog")
			continue
		}
		if err := imagesBuild(ctx, env, name); err != nil {
			note := imagesBuildFailedNote(name)
			r.Add(Failed, name, note)
			return r.Items(), errors.New(note)
		}
		if env.DryRun {
			r.Add(Created, name, "dry-run: nothing built")
			continue
		}
		r.Add(Created, name, "built")
	}
	return r.Items(), nil
}

// imagesWriteContext writes every embedded file under
// /var/lib/spoond/images, the layout kept: env.writeFile compares
// before it writes, so a rerun keeps every unchanged file. Files land
// 0644 except the .sh hooks at the top level (the elixir postgres
// hook, the scylla init hook): 0755. The guest scripts keep 0644 on
// disk — their exec bits come from the dockerfiles' COPY --chmod.
func imagesWriteContext(env *Env, r *Reporter) error {
	return fs.WalkDir(images.Context, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		mode := fs.FileMode(0o644)
		if !strings.Contains(path, "/") && strings.HasSuffix(path, ".sh") {
			mode = 0o755
		}
		b, err := fs.ReadFile(images.Context, path)
		if err != nil {
			return fmt.Errorf("embedded %s: %w", path, err)
		}
		return env.writeFile(r, filepath.Join(imagesContextPath, path), b, mode)
	})
}

// imagesManifestBaked returns the manifest's baked image names in file
// order — the `all` catalog — parsed from the embedded manifest.yaml
// as cmd/spoond-images parses it; only the name and baked fields are
// needed here.
func imagesManifestBaked() ([]string, error) {
	b, err := fs.ReadFile(images.Context, "manifest.yaml")
	if err != nil {
		return nil, err
	}
	var m struct {
		Images []struct {
			Name  string `yaml:"name"`
			Baked bool   `yaml:"baked"`
		} `yaml:"images"`
	}
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse manifest.yaml: %w", err)
	}
	var names []string
	for _, e := range m.Images {
		if e.Baked {
			names = append(names, e.Name)
		}
	}
	return names, nil
}

// imagesWithBuild asks the installed binary for the catalog and
// returns the names that have a current build.
func imagesWithBuild(ctx context.Context, env *Env) (map[string]bool, error) {
	out, err := env.query(ctx, imagesSpoondBin, "images", "list")
	if err != nil {
		return nil, fmt.Errorf("list the image catalog: %w", err)
	}
	built := map[string]bool{}
	for _, name := range parseImagesList(out) {
		built[name] = true
	}
	return built, nil
}

// parseImagesList reads `spoond images list` output and returns the
// names with a current build, in print order. cmdList prints
//
//	NAME BUILD DIGEST VCPU MEM_MB DISK_MB UPDATED_AT
//
// through a tabwriter, which drops empty cells: a row without a
// current build has no digest either (cmdList writes a digest only
// with a build), so it collapses to the five trailing fields. A row
// with seven fields therefore means built; NAME is the first.
func parseImagesList(out []byte) []string {
	var built []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 {
			continue
		}
		// The header row: NAME and BUILD as the first two
		// columns. A data row for an image literally named NAME
		// has a build id there instead.
		if fields[0] == "NAME" && fields[1] == "BUILD" {
			continue
		}
		built = append(built, fields[0])
	}
	return built
}

// imagesBuild runs one build:
//
//	env K=V ... /opt/spoond/spoond images -manifest <ctx>/manifest.yaml -context <ctx> build <name>
//
// The flags go before `build`: Go's flag package stops at the first
// positional argument. The `env` prefix carries only
// imagesBuildEnvKeys, read from backend.env — missing keys are left
// out and the build falls back to spoond-images' own defaults.
func imagesBuild(ctx context.Context, env *Env, name string) error {
	var args []string
	if b, err := os.ReadFile(env.Resolve(imagesBackendEnvPath)); err == nil {
		vals := imagesParseEnvFile(b)
		for _, k := range imagesBuildEnvKeys {
			if v, ok := vals[k]; ok {
				args = append(args, k+"="+v)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", imagesBackendEnvPath, err)
	}
	args = append(args,
		imagesSpoondBin,
		"images",
		"-manifest", filepath.Join(imagesContextPath, "manifest.yaml"),
		"-context", imagesContextPath,
		"build", name,
	)
	return env.exec(ctx, "env", args...)
}

// imagesParseEnvFile reads KEY=VALUE lines — comments (#) and blanks
// skipped, the first = splits — the shape the spoond phase writes
// backend.env in. Values are taken verbatim, no quotes stripped.
func imagesParseEnvFile(content []byte) map[string]string {
	vals := map[string]string{}
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if k = strings.TrimSpace(k); k == "" {
			continue
		}
		vals[k] = strings.TrimSpace(v)
	}
	return vals
}

// imagesBuildFailedNote is the fix a failed build reports.
func imagesBuildFailedNote(name string) string {
	return fmt.Sprintf("spoond images build %s failed; rerun `spoond install images` after fixing it", name)
}
