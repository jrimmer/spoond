// Command spoond-images is spoond's image pipeline (U07): every image is
// defined by a Dockerfile in images/, built with docker, pushed to the
// local registry, and turned into an E2B template build recorded in
// SQLite. A release machine republishes the built images to a remote
// registry (publish), and an install pulls them from there instead of
// running docker build (build --base-registry).
//
// Usage:
//
//	spoond images build <name>  build one image
//	spoond images build --all   build every manifest entry with baked: true
//	spoond images list          print the image catalog
//	spoond images publish <name> | --all
//	                            republish built images to --prefix
//
// Flags:
//
//	--manifest       image manifest path (default images/manifest.yaml)
//	--context        docker build context directory (default images)
//	--db             SQLite database path (default $SPOOND_DB_PATH)
//	--registry       registry to push to (default $IMAGE_REGISTRY, localhost:5000)
//	--base-registry  pull <prefix>/<name>:<base-tag> from here instead of docker build
//	--base-tag       tag pulled with --base-registry (default latest)
//	--prefix         publish destination: images go to <prefix>/<name>:<tag>
//	--tag            publish tag (default latest)
//
// An entry's warm_sizes name presets from $SIZE_PRESETS (the backend's
// setting; default sizes.DefaultSpec): each gets one more template build
// at that preset's vcpu and memory.
//
// Runs on the E2B host as root and talks to the orchestrator via
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
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jrimmer/spoond/v4/sizes"
	"github.com/jrimmer/spoond/v4/store"
	"github.com/jrimmer/spoond/v4/substrate"
	"github.com/jrimmer/spoond/v4/substrate/e2b"
)

const (
	startCmd = "/usr/local/bin/spoond-guest-init"
	readyCmd = "test -f /run/spoond-guest-ready"
)

// buildTimeout bounds one template build. It reads SPOOND_BUILD_TIMEOUT
// through substrate.BuildTimeoutFromEnv from this process's own
// environment; the backend reads the same variable in its environment to
// tune the GC's stale-row sweep, so a deployment must set it for both
// (spoond-rzz).
var buildTimeout = substrate.BuildTimeoutFromEnv()

// Every docker invocation goes through these vars — the seam tests
// replace to record the commands without running docker.

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

// runCapture streams a command's stdout like runCmd and captures its
// stderr too, so a failed docker pull or push can be classified (an
// image missing from the local registry vs. a registry that needs
// `docker login`) while the user still sees docker's output.
var runCapture = func(ctx context.Context, stdout io.Writer, name string, args ...string) (string, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	err := cmd.Run()
	return stderr.String(), err
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
	// MinMemoryMB is the smallest memory a lease of this image may ask
	// for (#151). 0 = unset: 512, or memory_mb when that is smaller.
	MinMemoryMB int `yaml:"min_memory_mb"`
	// WarmSizes names the SIZE_PRESETS presets built as extra memory
	// snapshots beside the default size (#151), so a lease at one of
	// them starts warm. Unset (as opposed to an empty list) inherits
	// the base's.
	WarmSizes []string `yaml:"warm_sizes"`
	// From names a catalog image this one is built on: its current
	// digest is passed as the BASE build argument, and its env, shape
	// (vcpu, memory, disk), min_memory_mb and warm sizes are inherited
	// unless set here. Used for layers such as a worker or tooling
	// layer on a base image.
	From string `yaml:"from"`
	// BuildArgs are passed to docker build as --build-arg KEY=VALUE.
	BuildArgs map[string]string `yaml:"build_args"`
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
	all := fs.Bool("all", false, "build or publish every manifest entry with baked: true")
	baseRegistry := fs.String("base-registry", "", "pull <prefix>/<name>:<base-tag> from here instead of docker build")
	baseTag := fs.String("base-tag", "latest", "tag pulled with --base-registry")
	prefix := fs.String("prefix", "", "publish destination prefix: images go to <prefix>/<name>:<tag>")
	tag := fs.String("tag", "latest", "publish tag")

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
		return cmdBuild(ctx, positional[1:], *all, *manifestPath, *dbPath, buildOpts{
			registry: *registry, contextDir: *contextDir,
			baseRegistry: *baseRegistry, baseTag: *baseTag,
		})
	case "publish":
		return cmdPublish(ctx, positional[1:], *all, *manifestPath, *registry, *prefix, *tag)
	default:
		fmt.Fprintf(os.Stderr, "spoond images: unknown command %q\n\n", positional[0])
		usage(fs)
		return 2
	}
}

func usage(fs *flag.FlagSet) {
	fmt.Fprintf(os.Stderr, "usage: spoond images [-manifest path] [-context dir] [-db path] [-registry host:port] <command>\n\ncommands:\n  build <name>  build one image and turn it into an E2B template build\n  build --all   build every manifest entry with baked: true\n  list          print the image catalog\n  publish <name> | --all\n                republish built images to --prefix as <prefix>/<name>:<tag>\n\nflags:\n")
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

func cmdBuild(ctx context.Context, names []string, all bool, manifestPath, dbPath string, opts buildOpts) int {
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

	// The size presets (#151) an entry's warm_sizes names, from the same
	// SIZE_PRESETS the backend reads. Unknown names fail every target
	// before the first build; the memory check needs the entry's
	// resolved shape and runs in buildOne, still before its docker build.
	presets, err := sizes.Parse(envOr("SIZE_PRESETS", sizes.DefaultSpec))
	if err != nil {
		fmt.Fprintf(os.Stderr, "spoond images: SIZE_PRESETS: %v\n", err)
		return 1
	}
	for _, img := range targets {
		for _, name := range img.WarmSizes {
			if _, ok := sizes.Lookup(presets, name); !ok {
				fmt.Fprintf(os.Stderr, "spoond images: %s: warm size %q is not a size preset (SIZE_PRESETS)\n", img.Name, name)
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

	fmt.Fprintf(os.Stderr, "spoond images: build timeout is %s (SPOOND_BUILD_TIMEOUT)\n", buildTimeout)
	for _, img := range targets {
		if err := buildOne(ctx, db, sub, img, presets, opts, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "spoond images: %s: %v\n", img.Name, err)
			return 1
		}
	}
	return 0
}

// buildOpts are the docker-facing build options: the registry to push
// to, the build context, and where to pull a prebuilt base image from
// (R5) instead of building it — baseRegistry empty builds.
type buildOpts struct {
	registry   string
	contextDir string
	// baseRegistry, when set, replaces docker build with a pull of
	// <baseRegistry>/<name>:<baseTag> (default latest) re-tagged to the
	// registry ref; a failed pull falls back to the local build.
	baseRegistry string
	baseTag      string
}

// templateBuildSize measures a template build's directory under the
// template storage root (E2B_TEMPLATE_STORAGE_PATH), the same allocated
// blocks × 512 the service's disk accounting sums (#125). A failed walk
// measures 0; the hourly disk accounting pass re-measures.
func templateBuildSize(buildID string) int64 {
	dir := filepath.Join(envOr("E2B_TEMPLATE_STORAGE_PATH", "/forkdcache/e2b/storage/templates"), buildID)
	size, err := store.BuildDiskUsage(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "spoond images: build size %s: %v; storing 0 until the hourly pass\n", buildID, err)
		return 0
	}
	return size
}

// resolveBase applies a manifest entry's From and BuildArgs: it looks up
// the base image's current digest (passed as BASE), fills vcpu, memory,
// disk, min_memory_mb, warm sizes and env from the base where the entry
// leaves them unset (entry env keys win), passes that env as WARM_ENV,
// injects the deployment's guest DNS resolver (SPOOND_GUEST_DNS_ADDR)
// unless the entry sets it, and returns the docker --build-arg flags,
// sorted.
func resolveBase(ctx context.Context, db *store.DB, img manifestImage) (manifestImage, []string, error) {
	vals := map[string]string{}
	for k, v := range img.BuildArgs {
		vals[k] = v
	}
	// The guest DNS resolver baked into every image's spoond-guest-init
	// (SPOOND_GUEST_DNS_ADDR). A manifest entry may override it in its
	// own build_args; unset leaves the Dockerfile's generic empty default
	// (no resolver pinned).
	if _, ok := vals["SPOOND_GUEST_DNS_ADDR"]; !ok {
		if v := os.Getenv("SPOOND_GUEST_DNS_ADDR"); v != "" {
			vals["SPOOND_GUEST_DNS_ADDR"] = v
		}
	}
	if img.From != "" {
		base, err := db.GetImage(ctx, img.From)
		if errors.Is(err, store.ErrNotFound) {
			return img, nil, fmt.Errorf("base image %s is not in the catalog", img.From)
		}
		if err != nil {
			return img, nil, err
		}
		if base.Digest == "" || base.CurrentBuildID == "" {
			return img, nil, fmt.Errorf("base image %s has no current build", img.From)
		}
		vals["BASE"] = base.Digest
		if img.VCPU == 0 {
			img.VCPU = base.VCPU
		}
		if img.MemoryMB == 0 {
			img.MemoryMB = base.MemoryMB
		}
		if img.DiskMB == 0 {
			img.DiskMB = base.DiskMB
		}
		if img.MinMemoryMB == 0 {
			img.MinMemoryMB = base.MinMemoryMB
		}
		if img.WarmSizes == nil {
			warm, err := db.ListImageSizes(ctx, img.From)
			if err != nil {
				return img, nil, err
			}
			for _, w := range warm {
				img.WarmSizes = append(img.WarmSizes, w.Size)
			}
		}
		env := map[string]string{}
		for k, v := range base.Env {
			env[k] = v
		}
		for k, v := range img.Env {
			env[k] = v
		}
		img.Env = env
		// WARM_ENV: the lease's runtime env as shell exports, so a build
		// step (the worker layer's warm command) fills caches where the
		// lease will look for them.
		envKeys := make([]string, 0, len(env))
		for k := range env {
			envKeys = append(envKeys, k)
		}
		sort.Strings(envKeys)
		var exports strings.Builder
		for _, k := range envKeys {
			fmt.Fprintf(&exports, "export %s='%s'; ", k, strings.ReplaceAll(env[k], "'", `'\''`))
		}
		vals["WARM_ENV"] = strings.TrimSpace(exports.String())
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var flags []string
	for _, k := range keys {
		flags = append(flags, "--build-arg", k+"="+vals[k])
	}
	return img, flags, nil
}

// pullBase pulls <baseRegistry>/<name>:<baseTag> and re-tags it to ref
// (the registry tag the rest of buildOne pushes). It reports whether the
// image was pulled, and a fatal error (the tag failed). A failed pull is
// not fatal: it prints one line and the caller falls back to the local
// docker build — the published tags may not cover this image (or
// version) yet.
func pullBase(ctx context.Context, name, baseRegistry, baseTag, ref string, stdout io.Writer) (bool, error) {
	from := baseRegistry + "/" + name + ":" + baseTag
	if _, err := runCapture(ctx, stdout, "docker", "pull", from); err != nil {
		fmt.Fprintf(stdout, "spoond images: %s: no image at %s, building locally\n", name, from)
		return false, nil
	}
	if err := runCmd(ctx, stdout, "docker", "tag", from, ref); err != nil {
		return false, fmt.Errorf("docker tag: %w", err)
	}
	return true, nil
}

// warmPresets resolves an image's warm_sizes against the presets. An
// unknown name, or a preset whose memory is below the image's effective
// minimum, is an error, so a bad manifest entry fails before any build.
func warmPresets(img manifestImage, presets []sizes.Preset) ([]sizes.Preset, error) {
	minMB := sizes.EffectiveMinMemoryMB(img.MinMemoryMB, img.MemoryMB)
	var out []sizes.Preset
	for _, name := range img.WarmSizes {
		p, ok := sizes.Lookup(presets, name)
		if !ok {
			return nil, fmt.Errorf("warm size %q is not a size preset (SIZE_PRESETS)", name)
		}
		if p.MemoryMB < minMB {
			return nil, fmt.Errorf("warm size %s: %d MiB is below the image's min_memory_mb %d", name, p.MemoryMB, minMB)
		}
		out = append(out, p)
	}
	return out, nil
}

// buildOne builds and pushes one image, then starts and waits for its
// E2B template build at the image's default size and one more per warm
// size, recording everything in the catalog. On failure the build row is
// marked failed and the image row keeps describing the last good build.
func buildOne(ctx context.Context, db *store.DB, sub substrate.Substrate, img manifestImage, presets []sizes.Preset, opts buildOpts, stdout io.Writer) error {
	if img.Dockerfile == "" {
		return errors.New("manifest entry has no dockerfile")
	}
	ref := opts.registry + "/" + img.Name + ":latest"

	img, buildArgs, err := resolveBase(ctx, db, img)
	if err != nil {
		return err
	}
	warm, err := warmPresets(img, presets)
	if err != nil {
		return err
	}

	// With --base-registry the image comes from the publish prefix (R5):
	// pull the published tag, re-tag it to the registry ref, and skip
	// the local docker build. resolveBase already ran above: it inherits
	// the entry's base-image shape and env, and its build args are used
	// only by the local build this may fall back to.
	pulled := false
	if opts.baseRegistry != "" {
		var err error
		pulled, err = pullBase(ctx, img.Name, opts.baseRegistry, opts.baseTag, ref, stdout)
		if err != nil {
			return err
		}
	}
	if !pulled {
		// Build locally: docker build with the manifest's build args,
		// streaming the output.
		args := []string{"build", "--pull"}
		args = append(args, buildArgs...)
		args = append(args, "-f", filepath.Join(opts.contextDir, img.Dockerfile), "-t", ref, opts.contextDir)
		if err := runCmd(ctx, stdout, "docker", args...); err != nil {
			return fmt.Errorf("docker build: %w", err)
		}
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
	// The push leaves the local tagged copy (<registry>/<name>:latest —
	// the only ref this build tags) in dockerd, where nothing removes it
	// (spoond-07fn: on 2026-10-13 three builds took the root disk from
	// 47 % to 62 %, 7 GB of it tagged local images the dangling-only
	// `docker image prune` below never touches). The registry holds the
	// canonical copy — template builds pull from it — so drop the exact
	// ref just pushed. Never a blanket `docker image prune -a`: other
	// images on the host (the registry's own, the buildkit container's)
	// must stay. Best effort: a failed rm never fails the build.
	// A pulled base also left its published tag behind; drop it with ref.
	rm := []string{"image", "rm", ref}
	if pulled {
		rm = append(rm, opts.baseRegistry+"/"+img.Name+":"+opts.baseTag)
	}
	if err := runCmd(ctx, stdout, "docker", rm...); err != nil {
		fmt.Fprintf(stdout, "warning: docker image rm: %v\n", err)
	}
	// Re-tagging :latest leaves the previous build's image dangling; the
	// registry holds the canonical copy, so drop dangling images (only
	// those: nothing a container or another tag uses). Best effort: a
	// failed prune never fails the build.
	if err := runCmd(ctx, stdout, "docker", "image", "prune", "-f"); err != nil {
		fmt.Fprintf(stdout, "warning: docker image prune: %v\n", err)
	}
	// Build cache goes too: every image build adds layers to it and
	// nothing else removes them (on 2026-10-11 it had filled the host's
	// root filesystem with 22 GB, and on 2026-10-12 a day of builds left
	// the root disk at 80 % because everything newer than three days
	// survived). Two rules, on two builders: the age rule keeps recent
	// cache, so a rebuild soon after is still fast, and the size cap
	// (IMAGE_BUILD_CACHE_KEEP, default 10 GB) bounds what it can hold.
	// `docker builder prune` reaches only dockerd's default builder; the
	// docker-container builder the host builds with keeps its own cache,
	// pruned through `docker buildx prune` with no --builder flag, so the
	// current builder in `docker buildx ls` is the one pruned. A host
	// without the buildx plugin fails that command ("docker: 'buildx' is
	// not a docker command"); that is the normal case and is skipped
	// silently. Best effort, like above: a failed prune never fails the
	// build.
	keep := envOr("IMAGE_BUILD_CACHE_KEEP", "10GB")
	for _, args := range [][]string{
		{"builder", "prune", "-f", "--filter", "until=72h"},
		{"builder", "prune", "-f", "--keep-storage", keep},
		{"buildx", "prune", "-f", "--filter", "until=72h"},
		{"buildx", "prune", "-f", "--keep-storage", keep},
	} {
		// buildx runs without a warning: a missing plugin must not sound
		// like a build problem on every image build.
		warn := strings.HasPrefix(args[0], "builder")
		if err := runCmd(ctx, stdout, "docker", args...); err != nil && warn {
			fmt.Fprintf(stdout, "warning: docker %s prune: %v\n", args[0], err)
		}
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
			Env:        img.Env,
			UpdatedAt:  time.Now(),
		}
		if err := db.UpsertImage(ctx, row); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	buildID, err := templateBuild(ctx, db, sub, row.TemplateID, img.Name, digest, img.VCPU, img.MemoryMB, img.DiskMB)
	if err != nil {
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
		MinMemoryMB:    img.MinMemoryMB,
		StartCmd:       startCmd,
		ReadyCmd:       readyCmd,
		Env:            img.Env,
		UpdatedAt:      time.Now(),
	}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "built %s build=%s digest=%s\n", img.Name, buildID, digest)

	// The warm sizes (#151): one more template build per preset from the
	// same digest. The previous warm builds hold the previous image, so
	// their rows go as soon as the new default is current (a create in
	// the moment between may still start from one); until its new build
	// is ready, a preset cold-boots from the current build. A preset left out of
	// warm_sizes stays out, and the GC reclaims its old build.
	if err := db.DeleteImageSizesExcept(ctx, img.Name, nil); err != nil {
		return err
	}
	for _, p := range warm {
		id, err := templateBuild(ctx, db, sub, row.TemplateID, img.Name, digest, p.VCPU, p.MemoryMB, img.DiskMB)
		if err != nil {
			return fmt.Errorf("warm size %s: %w", p.Name, err)
		}
		if err := db.UpsertImageSize(ctx, store.ImageSizeRow{
			Image: img.Name, Size: p.Name, BuildID: id,
			VCPU: p.VCPU, MemoryMB: p.MemoryMB, UpdatedAt: time.Now(),
		}); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "built %s size=%s build=%s\n", img.Name, p.Name, id)
	}
	return nil
}

// templateBuild starts and waits for one E2B template build of the
// image's template from digest at the given shape, recording it in the
// catalog: the row is written building first, then ready with what the
// orchestrator produced and its header refs, or failed. It returns the
// new build id; the caller points the image or a warm size at it.
func templateBuild(ctx context.Context, db *store.DB, sub substrate.Substrate, templateID, image, digest string, vcpu, memoryMB, diskMB int) (string, error) {
	buildID := e2b.NewUUID()
	now := time.Now()
	if err := db.InsertBuild(ctx, store.BuildRow{
		BuildID:    buildID,
		Kind:       "template",
		TemplateID: templateID,
		Image:      image,
		State:      "building",
		VCPU:       vcpu,
		MemoryMB:   memoryMB,
		DiskMB:     diskMB,
		CreatedAt:  now,
		UpdatedAt:  now,
	}); err != nil {
		return "", err
	}

	bctx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	res, err := sub.BuildTemplate(bctx, substrate.BuildRequest{
		TemplateID: templateID,
		BuildID:    buildID,
		FromImage:  digest,
		VCPU:       uint32(vcpu),
		MemoryMB:   uint32(memoryMB),
		DiskSizeMB: uint32(diskMB),
		StartCmd:   startCmd,
		ReadyCmd:   readyCmd,
	})
	if err != nil {
		// Record the failure on a context detached from bctx's deadline:
		// when the build timed out (or the caller cancelled) bctx is
		// already done, and writing the failure through it would leave
		// the row building forever — a permanent GC root (spoond-4yl).
		// The detached context is still bounded, so an unhealthy store
		// cannot hang the pipeline.
		uctx, ucancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer ucancel()
		if uerr := db.UpdateBuildState(uctx, buildID, "failed", err.Error(), nil); uerr != nil {
			return "", errors.Join(err, uerr)
		}
		return "", err
	}

	// The ready write deliberately uses the caller's ctx, unlike the
	// failure write above: a finished build must be recorded, but if the
	// operator Ctrl-C'd while BuildTemplate was returning, that ctx is
	// already done and the row stays `building`; the GC's stale-building
	// sweep (twice the build timeout) then fails it and emits a `gc`
	// event (spoond-4yl, spoond-rzz). A detached context here would
	// record a build the caller asked spoond to abandon.
	if err := db.UpdateBuildState(ctx, buildID, "ready", "", &store.BuildRow{
		KernelVersion:      res.KernelVersion,
		FirecrackerVersion: res.FirecrackerVersion,
		EnvdVersion:        res.EnvdVersion,
		DiskMB:             int(res.DiskSizeMB),
		// The template build's size at write time (#125), like the
		// service records for checkpoint and pause builds: measure the
		// build directory the orchestrator just wrote. 0 on a failed
		// stat; the hourly disk accounting pass corrects it.
		SizeBytes: templateBuildSize(buildID),
	}); err != nil {
		return "", err
	}
	// The build's headers reference the blocks of other builds (A3 C3);
	// GC keeps them (U11).
	if err := db.AddBuildRefs(ctx, buildID, append(res.Refs.RootfsBuildIDs, res.Refs.MemfileBuildIDs...)); err != nil {
		return "", err
	}
	return buildID, nil
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
