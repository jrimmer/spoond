// Command forkdctl is a thin CLI wrapper over the forkd control plane.
//
// The API surface is the SSH-as-API control plane (same model as exe.dev:
// "the exe.dev API is SSH"): every command here is literally an exec
// request to `ssh ctl@<host> "<verb> ..."` against forkd-sshd-gateway.
// The CLI adds zero business logic — it resolves args, builds the SSH
// command line, runs it, and prints the JSON response. All state lives in
// the backend; the CLI is a client like any other.
//
// Usage:
//
//	forkdctl new [image]            create a sandbox (dev/go/py/elixir/llm)
//	forkdctl ls                     list sandboxes (JSON)
//	forkdctl rm <id>                delete a sandbox
//	forkdctl keepalive <id>         extend a persistent lease
//	forkdctl suspend <id>           suspend (snapshot + stop)
//	forkdctl resume <id>            resume from snapshot
//	forkdctl restart <id>           reboot (snapshot + fresh sandbox)
//	forkdctl cp <id> [tag]          clone a sandbox
//	forkdctl shelly <id>            install + start the Shelley coding agent
//	forkdctl tag <id> <name>        give the sandbox a friendly name
//	forkdctl prompt <id> <message>  ask the agent in a sandbox something
//	forkdctl ssh <id|name>          drop into a shell (delegates to ssh)
//	forkdctl help
//
// Environment: FORKD_CTL_HOST (default sandbox.lacy.casa), FORKD_CTL_PORT
// (default 2222), FORKD_CTL_KEY (default ~/.ssh/id_ed25519).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(1)
	}
	verb := args[0]
	rest := args[1:]

	host := envOr("FORKD_CTL_HOST", "sandbox.lacy.casa")
	port := envOr("FORKD_CTL_PORT", "2222")
	key := envOr("FORKD_CTL_KEY", filepath.Join(homeDir(), ".ssh", "id_ed25519"))

	switch verb {
	case "help", "--help", "-h":
		usage()
		return
	case "ssh":
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "usage: forkdctl ssh <id|name>")
			os.Exit(1)
		}
		runSSH(host, port, key, rest[0], rest[1:])
		return
	case "prompt":
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "usage: forkdctl prompt <id> <message>")
			os.Exit(1)
		}
		cmd := fmt.Sprintf("prompt %s %s", rest[0], strings.Join(rest[1:], " "))
		printJSON(runCtl(host, port, key, cmd))
		return
	case "tag":
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "usage: forkdctl tag <id> <name>")
			os.Exit(1)
		}
		printJSON(runCtl(host, port, key, fmt.Sprintf("tag %s %s", rest[0], rest[1])))
		return
	case "comment":
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "usage: forkdctl comment <id> [text...] (no text clears)")
			os.Exit(1)
		}
		printJSON(runCtl(host, port, key, fmt.Sprintf("comment %s %s", rest[0], strings.Join(rest[1:], " "))))
		return
	case "whoami":
		printJSON(runCtl(host, port, key, "whoami"))
		return
	case "restart", "resume", "suspend", "keepalive", "shelly", "agent":
		if len(rest) < 1 {
			fmt.Fprintf(os.Stderr, "usage: forkdctl %s <id>\n", verb)
			os.Exit(1)
		}
		v := verb
		if verb == "agent" {
			v = "shelly"
		}
		printJSON(runCtl(host, port, key, fmt.Sprintf("%s %s", v, rest[0])))
		return
	case "rm":
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "usage: forkdctl rm <id>")
			os.Exit(1)
		}
		printJSON(runCtl(host, port, key, fmt.Sprintf("rm %s", rest[0])))
		return
	case "cp", "clone":
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "usage: forkdctl cp <id> [tag]")
			os.Exit(1)
		}
		cmd := fmt.Sprintf("cp %s", rest[0])
		if len(rest) > 1 {
			cmd += " " + rest[1]
		}
		printJSON(runCtl(host, port, key, cmd))
		return
	case "new":
		cmd := "new"
		if len(rest) > 0 {
			cmd = "new " + rest[0]
		}
		printJSON(runCtl(host, port, key, cmd))
		return
	case "ls":
		printJSON(runCtl(host, port, key, "ls"))
		return
	default:
		fmt.Fprintf(os.Stderr, "forkdctl: unknown command %q\n\n", verb)
		usage()
		os.Exit(1)
	}
}

func runCtl(host, port, key, cmd string) string {
	sshArgs := []string{
		"-p", port,
		"-i", key,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-o", "BatchMode=yes",
		fmt.Sprintf("ctl@%s", host),
		cmd,
	}
	c := exec.Command("ssh", sshArgs...)
	out, err := c.Output() // stdout only; stderr stays out of the JSON stream
	if err != nil {
		// The gateway closes the channel after one command, so ssh exits
		// non-zero even on success — the JSON on stdout is authoritative.
		if !json.Valid(out) {
			return fmt.Sprintf(`{"error":%q}`, strings.TrimSpace(string(out)))
		}
	}
	return strings.TrimSpace(string(out))
}

func runSSH(host, port, key, target string, extra []string) {
	sshArgs := []string{
		"-p", port,
		"-i", key,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
		fmt.Sprintf("%s@%s", target, host),
	}
	sshArgs = append(sshArgs, extra...)
	c := exec.Command("ssh", sshArgs...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		os.Exit(exitCode(err))
	}
}

func printJSON(s string) {
	// Validate it's JSON so garbage doesn't get printed as if the API
	// returned it; the gateway always replies JSON.
	var v any
	if json.Unmarshal([]byte(s), &v) == nil {
		fmt.Println(s)
		return
	}
	fmt.Fprintln(os.Stderr, s)
	os.Exit(1)
}

func usage() {
	fmt.Fprint(os.Stderr, `forkdctl — forkd control plane CLI (thin wrapper over ssh ctl@)

usage:
  forkdctl new [image]            create a sandbox (dev/go/py/elixir/llm)
  forkdctl ls                     list sandboxes
  forkdctl rm <id>                delete a sandbox
  forkdctl keepalive <id>         extend a persistent lease
  forkdctl suspend <id>           suspend (snapshot + stop)
  forkdctl resume <id>            resume from snapshot
  forkdctl restart <id>           reboot (snapshot + fresh sandbox)
  forkdctl cp <id> [tag]          clone a sandbox
  forkdctl shelly <id>            install + start the Shelley coding agent
  forkdctl tag <id> <name>        give the sandbox a friendly name
  forkdctl comment <id> [text]    set/clear a free-text annotation
  forkdctl whoami                 show the authenticated key identity
  forkdctl prompt <id> <message>  ask the agent in a sandbox something
  forkdctl ssh <id|name>          drop into a shell
  forkdctl help

env: FORKD_CTL_HOST (sandbox.lacy.casa), FORKD_CTL_PORT (2222), FORKD_CTL_KEY (~/.ssh/id_ed25519)
`)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "."
}

func exitCode(err error) int {
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return 1
}
