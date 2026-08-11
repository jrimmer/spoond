// Command spoond is the consolidated single-binary entry point for all
// spoond services. It dispatches to subcommands:
//
//	spoond backend    lease API (warm pool, proxy, LLM gateway)
//	spoond gateway    SSH gateway + ctl plane
//	spoond acp        Agent Client Protocol endpoint
//	spoond mcp        MCP server (stdio)
//	spoond runner     Forgejo Actions runner (adaptive pool)
//	spoond ctl        control-plane CLI (thin ssh ctl@ wrapper)
//
// Modules are optional at build time via Go build tags. Each subcommand
// is registered in a build-tag-gated file (see the files in this
// directory); the default build includes every module:
//
//	go build -o spoond ./cmd/spoond
//
// Exclude any module by negating its tag:
//
//	go build -tags 'nobackend,nomcp,norunner' -o spoond ./cmd/spoond
//
// Supported exclusion tags: nobackend, nogateway, noacp, nomcp,
// norunner, noctl.
package main

import (
	"fmt"
	"os"
)

type command struct {
	name string
	desc string
	run  func(args []string) int
}

var commands = map[string]command{}

func register(cmd command) {
	commands[cmd.name] = cmd
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage()
		if len(args) == 0 {
			os.Exit(2)
		}
		return
	}
	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(os.Stderr, "spoond: unknown command %q\n\n", args[0])
		usage()
		os.Exit(2)
	}
	os.Exit(cmd.run(args[1:]))
}

func usage() {
	fmt.Fprint(os.Stderr, "spoond — isolated ephemeral compute for people and agents (forkd microVM lease service)\n\nusage:\n  spoond <command> [args...]\n\ncommands:\n")
	for _, name := range []string{"backend", "gateway", "acp", "mcp", "runner", "ctl"} {
		if c, ok := commands[name]; ok {
			fmt.Fprintf(os.Stderr, "  %-9s %s\n", c.name, c.desc)
		}
	}
	fmt.Fprint(os.Stderr, "\nbuild tags (exclude modules): nobackend, nogateway, noacp, nomcp, norunner, noctl\n")
}
