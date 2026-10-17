//go:build !noinstall

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
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

      --dry-run  print what would change, run no command, write nothing
      --root DIR write under DIR instead of / (tests; production omits it)

  A run that fails is safe to rerun: every phase reports each item as
  created, changed or kept, and a secret or env file that exists is
  never overwritten (kept).
`

// installMain is the `spoond install` entry point: parse flags, build
// the Env (a temp-rooted run for tests and --root; the host for real
// runs), run all phases or the one named, exit non-zero on failure.
func installMain(args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, installUsage) }
	dryRun := fs.Bool("dry-run", false, "show what would change, write nothing, run no command")
	root := fs.String("root", "/", "write under DIR instead of / (tests)")
	if err := fs.Parse(args); err != nil {
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
		Root:   *root,
		Run:    install.Cmd{},
		Out:    os.Stdout,
		DryRun: *dryRun,
	}
	items, err := install.RunAll(ctx, env, names...)
	if err != nil {
		if errors.Is(err, install.ErrBadPhase) {
			fmt.Fprintf(os.Stderr, "spoond install: %v\n", err)
			fs.Usage()
			return 2
		}
		fmt.Fprintf(os.Stderr, "spoond install: %v\n", err)
		// A phase failure already printed its failed row and the fix;
		// the summary line under it says what went through first.
		summarize(items, *dryRun)
		return 1
	}
	summarize(items, *dryRun)
	return 0
}

// summarize prints the final counts line: "spoond install done: …"
// after success, "spoond install dry run; nothing written, no command
// run: …" after a dry run, and the same line under a failure, so the
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
		verb = "dry run; nothing written, no command run"
	}
	fmt.Printf("spoond install %s: %d created, %d changed, %d kept, %d skipped, %d failed\n",
		verb, created, changed, kept, skipped, failed)
}
