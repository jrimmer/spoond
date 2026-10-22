//go:build !noinstall

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/jrimmer/spoond/v3/install"
)

func init() {
	register(command{
		name: "install",
		desc: "clean native install: ordered phases over a root prefix",
		run:  installMain,
	})
}

// installUsage is the argument help; a failure points at it so the fix
// is always on screen.
const installUsage = `usage: spoond install [--dry-run] [phase]

  With no phase it runs every phase in order and stops at the first
  failure; with a phase it runs that one:

      check      host prerequisites (root, KVM, memory, ports, store)
      host       packages, modules, sysctls, hugepages, Docker, the store
      runtime    the E2B orchestrator, envd and their pinned artifacts
      spoond     spoond's own binary, secrets, units and first user
      images     the base image catalog
      doctor     spoond doctor over the result

  Flags:

      --dry-run              print what would change; change nothing
      --host-name NAME       the name clients reach this host by (TLS, gateway)
      --admin-key FILE       SSH public key for the first admin user
      --tls-cert FILE        an existing certificate (with --tls-key);
      --tls-key FILE         without them a self-signed pair is made
      --hugepages N|auto     2 MiB pages to reserve (default auto)
      --store-disk DEV       make the snapshot store a ZFS pool on DEV
      --store-dir DIR        the snapshot store (default /forkdcache/e2b)
      --runtime FILE         a local spoond-runtime tarball, not the download
      --images SET           minimal (default), all or none
      --base-registry PFX    pull base Docker images from PFX, not build
      --runner-url URL       register a Forgejo Actions runner
      --runner-token-file F  its registration token, read from a file
      --container            run from inside the Docker image (Docker install)
      --config-only          with --container: write config, start nothing

  A run that fails is safe to rerun: every phase reports each item as
  created, changed or kept, and a secret or env file that exists is
  never overwritten (kept).
`

// installMain is the `spoond install` entry point: parse flags, build
// the Env (the host in production, Root for the documented test path),
// run all phases or the one named, and exit non-zero on failure. With
// no phase argument the whole table runs in order.
func installMain(args []string) int {
	for _, a := range args {
		if a == "help" || a == "-h" || a == "--help" {
			fmt.Fprint(os.Stderr, installUsage)
			return 0
		}
	}
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, installUsage) }
	dryRun := fs.Bool("dry-run", false, "show what would change; change nothing")
	root := fs.String("root", "/", "write under DIR instead of /")
	var opt install.Options
	fs.StringVar(&opt.HostName, "host-name", "", "the name clients reach this host by")
	fs.StringVar(&opt.AdminKey, "admin-key", "", "SSH public key file for the first admin user")
	fs.StringVar(&opt.TLSCert, "tls-cert", "", "existing TLS certificate")
	fs.StringVar(&opt.TLSKey, "tls-key", "", "existing TLS key")
	fs.StringVar(&opt.Hugepages, "hugepages", install.HugepagesAuto, "2 MiB pages to reserve, or auto")
	fs.StringVar(&opt.StoreDisk, "store-disk", "", "disk or partition for the snapshot store's ZFS pool")
	fs.StringVar(&opt.StoreDir, "store-dir", install.DefaultStoreDir, "snapshot store directory")
	fs.StringVar(&opt.Runtime, "runtime", "", "local spoond-runtime tarball")
	fs.StringVar(&opt.Images, "images", install.ImagesMinimal, "image catalog: minimal, all or none")
	fs.StringVar(&opt.BaseRegistry, "base-registry", "", "registry prefix to pull base images from")
	fs.StringVar(&opt.RunnerURL, "runner-url", "", "Forgejo URL to register an Actions runner with")
	fs.StringVar(&opt.RunnerTokenFile, "runner-token-file", "", "file holding the runner registration token")
	fs.BoolVar(&opt.Container, "container", false, "run from inside the Docker image")
	fs.BoolVar(&opt.ConfigOnly, "config-only", false, "with --container: write config, start nothing")
	fs.BoolVar(&opt.ArtifactsOnly, "artifacts-only", false, "runtime phase: install the binaries and pinned artifacts only (the Docker image build)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := opt.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "spoond install: %v\n", err)
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintf(os.Stderr, "spoond install: unexpected arguments %v\n", fs.Args()[1:])
		fs.Usage()
		return 2
	}

	// Ctrl-C stops between phases; a running command still finishes
	// (systemd units aside, half a phase is worse than a late one).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var names []string
	if fs.NArg() == 1 {
		names = []string{fs.Arg(0)}
	}

	env := &install.Env{
		Root:    *root,
		Run:     install.Cmd{},
		Out:     os.Stdout,
		DryRun:  *dryRun,
		Opt:     opt,
		Version: releaseVersion(version, readBuildInfo()),
	}
	items, err := install.RunAll(ctx, env, names...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "spoond install: %v\n", err)
		if errors.Is(err, install.ErrBadPhase) {
			fs.Usage()
			return 2
		}
		// A phase failure already printed its failed row and the fix;
		// the counts line under it says what went through first.
		summarize(items, *dryRun)
		return 1
	}
	summarize(items, *dryRun)
	return 0
}

// summarize prints the final counts line: "spoond install done: …"
// after success, "spoond install dry run; nothing changed: …" after a
// dry run, and the same line under a failure, so the
// operator sees what went through before the stop.
func summarize(items []install.Item, dryRun bool) {
	var created, changed, kept, skipped, failed int
	for _, it := range items {
		switch it.Result {
		case install.Created:
			created++
		case install.Changed:
			changed++
		case install.Kept:
			kept++
		case install.Skipped:
			skipped++
		case install.Failed:
			failed++
		}
	}
	verb := "done"
	if dryRun {
		verb = "dry run; nothing changed"
	}
	fmt.Printf("spoond install %s: %d created, %d changed, %d kept, %d skipped, %d failed\n",
		verb, created, changed, kept, skipped, failed)
}

// releaseVersion is the release tag the runtime phase downloads for:
// the -ldflags version, else the module version of a `go install
// ...@vX.Y.Z` build. A pseudo-version, a dirty or pre-release build
// and a plain checkout have no release, so they give "" (the phase then
// asks for --runtime).
func releaseVersion(v string, bi *debug.BuildInfo) string {
	if v == "" && bi != nil {
		v = bi.Main.Version
	}
	if !strings.HasPrefix(v, "v") || strings.ContainsAny(v, "-+") || strings.Count(v, ".") != 2 {
		return ""
	}
	return v
}
