// Command spoond-hive is the hive's CLI front door: the checks an
// enlistment has to pass, run against a hive.yaml, from anywhere.
//
// The engine lives in the hive package; this command reads the file and
// the environment (SPOOND_API, SPOOND_TOKEN), runs the checks with the
// offline environment, and prints the report — text by default, JSON
// with --json. The exit code is 0 when nothing failed and 1 otherwise.
//
// The steps that need the host (building the worker image, cloning with
// the deploy key, the trial lease, the gates, the budget) report Skip
// here; POST /hive/check on the instance runs them for real.
//
// Usage:
//
//	spoond hive check <file> [--json]
package spoondhive

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/jrimmer/spoond/hive"
)

// Main runs the hive subcommand and returns the process exit code.
func Main(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "help", "--help", "-h":
		usage()
		return 0
	case "check":
		return cmdCheck(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "spoond hive: unknown command %q\n\n", args[0])
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: spoond hive <command> [args...]

commands:
  check <file>   run the enlistment checks against a hive.yaml
                 (--json for machine-readable output)

environment:
  SPOOND_API     lease API base URL (default `+hive.DefaultAPI+`)
  SPOOND_TOKEN   consumer bearer token
`)
}

// cmdCheck runs the checks against one hive.yaml file. Flags are
// accepted before or after the file name.
func cmdCheck(args []string) int {
	fs := flag.NewFlagSet("hive check", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "print the report as JSON")
	var files []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		files = append(files, rest[0])
		rest = rest[1:]
	}
	if len(files) != 1 {
		fmt.Fprintln(os.Stderr, "usage: spoond hive check <file> [--json]")
		return 2
	}

	p, err := hive.ParseFile(files[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "spoond hive: %v\n", err)
		return 1
	}
	env := hive.EnvFromEnv()
	rep := hive.Run(context.Background(), &p, env)

	if *jsonOut {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "spoond hive: render report: %v\n", err)
			return 1
		}
		fmt.Println(string(b))
	} else {
		fmt.Print(rep.String())
	}
	if rep.OK() {
		return 0
	}
	return 1
}
