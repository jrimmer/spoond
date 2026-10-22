// The images phase (docs/plans/2026-10-16-4.0-install.md, the `images`
// row). spoond ships no images: the phase writes the embedded guest
// pieces (github.com/jrimmer/spoond/v4/images) to /var/lib/spoond/guest,
// where an operator's build context copies spoond-guest-init from
// (images/README.md). With --images <manifest> it then builds that
// manifest's baked images: it asks the installed binary which already
// have a current build and runs one `spoond images build` per missing
// image. The build's environment values come from
// /etc/spoond/backend.env and go on the command line as a plain
// `env KEY=VALUE` prefix — and only the five keys the build needs: no
// token or other backend.env secret ever reaches an argv.
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

	"github.com/jrimmer/spoond/v4/images"
)

func init() { Phases[PhaseImages] = imagesPhase }

const (
	// imagesGuestRoot is where the embedded guest/ lands:
	// /var/lib/spoond/guest/spoond-guest-init and spoond-tmux.sh.
	imagesGuestRoot = "/var/lib/spoond"
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

func imagesPhase(ctx context.Context, env *Env) ([]Item, error) {
	r := &Reporter{Phase: PhaseImages, Out: env.Out}

	// The guest pieces every operator Dockerfile copies in. Unchanged
	// files are kept.
	if err := imagesWriteGuest(env, r); err != nil {
		return r.Items(), err
	}

	manifest := env.Opt.images()
	if manifest == ImagesNone {
		r.Add(Skipped, "images", "spoond ships no images: build your own with spoond images build (images/README.md), or pass --images <manifest>")
		return r.Items(), nil
	}
	names, err := imagesManifestBaked(env.Resolve(manifest))
	if err != nil {
		r.Add(Failed, manifest, err.Error())
		return r.Items(), err
	}
	if len(names) == 0 {
		r.Add(Skipped, manifest, "no baked images in the manifest")
		return r.Items(), nil
	}

	// What the catalog already has.
	built, err := imagesWithBuild(ctx, env)
	if err != nil {
		return r.Items(), err
	}

	if env.Opt.BaseRegistry != "" {
		r.Add(Kept, "base images", "pulled from "+env.Opt.BaseRegistry+" when published there, else built locally")
	}

	// Build the missing ones, one at a time: a failure stops the
	// phase with the fix.
	for _, name := range names {
		if built[name] {
			r.Add(Kept, name, "current build already in the catalog")
			continue
		}
		if err := imagesBuild(ctx, env, manifest, name); err != nil {
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

// imagesWriteGuest writes every embedded file under /var/lib/spoond
// (guest/...), the layout kept: env.writeFile compares before it
// writes, so a rerun keeps every unchanged file. The files are
// scripts an image runs, so they land 0755.
func imagesWriteGuest(env *Env, r *Reporter) error {
	return fs.WalkDir(images.Context, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(images.Context, path)
		if err != nil {
			return fmt.Errorf("embedded %s: %w", path, err)
		}
		return env.writeFile(r, filepath.Join(imagesGuestRoot, path), b, 0o755)
	})
}

// imagesManifestBaked returns the manifest's baked image names in file
// order, parsed as cmd/spoond-images parses it; only the name and
// baked fields are needed here. path is the host path (Root applied).
func imagesManifestBaked(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the image manifest: %w", err)
	}
	var m struct {
		Images []struct {
			Name  string `yaml:"name"`
			Baked bool   `yaml:"baked"`
		} `yaml:"images"`
	}
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
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
//	env K=V ... /opt/spoond/spoond images -manifest <manifest> -context <its dir> build <name>
//
// The images flags go before `build` (Go's flag package stops at the
// first positional argument); -base-registry is build's own flag, after
// it. The context is the manifest's directory, where
// its dockerfiles sit. The `env` prefix carries only imagesBuildEnvKeys,
// read from backend.env — missing keys are left out and the build falls
// back to spoond-images' own defaults.
func imagesBuild(ctx context.Context, env *Env, manifest, name string) error {
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
		"-manifest", manifest,
		"-context", filepath.Dir(manifest),
		"build",
	)
	if reg := env.Opt.BaseRegistry; reg != "" {
		args = append(args, "-base-registry", reg)
	}
	args = append(args, name)
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
