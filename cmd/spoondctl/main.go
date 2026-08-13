// Command spoondctl is a thin CLI wrapper over the spoond control plane.
//
// The API surface is the SSH-as-API control plane (same model as exe.dev:
// "the exe.dev API is SSH"): every command here is literally an exec
// request to `ssh ctl@<host> "<verb> ..."` against spoond-sshd-gateway.
// The CLI adds zero business logic — it resolves args, builds the SSH
// command line, runs it, and prints the JSON response. All state lives in
// the backend; the CLI is a client like any other.
//
// Usage:
//
//	spoondctl new [image]            create a sandbox (dev/go/py/elixir/llm)
//	spoondctl ls                     list sandboxes (JSON)
//	spoondctl rm <id>                delete a sandbox
//	spoondctl keepalive <id>         extend a persistent lease
//	spoondctl suspend <id>           suspend (snapshot + stop)
//	spoondctl resume <id>            resume from snapshot
//	spoondctl restart <id>           reboot (snapshot + fresh sandbox)
//	spoondctl cp <id> [tag]          clone a sandbox
//	spoondctl shelly <id>            install + start the Shelley coding agent
//	spoondctl tag <id> <name>        give the sandbox a friendly name
//	spoondctl prompt <id> <message>  ask the agent in a sandbox something
//	spoondctl ssh <id|name>          drop into a shell (delegates to ssh)
//	spoondctl env ls                  list per-PR ephemeral environments
//	spoondctl env new <repo> <pr> [image]   create/ensure a PR environment
//	spoondctl env rm <repo> <pr>      tear down a PR environment
//	spoondctl env id <repo> <pr>      print the environment's sandbox id
//	spoondctl help
//
// Environment: FORKD_CTL_HOST (default sandbox.lacy.casa), FORKD_CTL_PORT
// (default 2222), FORKD_CTL_KEY (default ~/.ssh/id_ed25519).
package spoondctl

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func Main(args []string) int {
	if len(args) == 0 {
		usage()
		return 1
	}
	verb := args[0]
	rest := args[1:]

	host := envOr("FORKD_CTL_HOST", "sandbox.lacy.casa")
	port := envOr("FORKD_CTL_PORT", "2222")
	key := envOr("FORKD_CTL_KEY", filepath.Join(homeDir(), ".ssh", "id_ed25519"))

	switch verb {
	case "help", "--help", "-h":
		usage()
		return 0
	case "ssh":
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "usage: spoondctl ssh <id|name>")
			return 1
		}
		runSSH(host, port, key, rest[0], rest[1:])
		return 0
	case "prompt":
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "usage: spoondctl prompt <id> <message>")
			return 1
		}
		cmd := fmt.Sprintf("prompt %s %s", rest[0], strings.Join(rest[1:], " "))
		printJSON(runCtl(host, port, key, cmd))
		return 0
	case "tag":
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "usage: spoondctl tag <id> <name>")
			return 1
		}
		printJSON(runCtl(host, port, key, fmt.Sprintf("tag %s %s", rest[0], rest[1])))
		return 0
	case "comment":
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "usage: spoondctl comment <id> [text...] (no text clears)")
			return 1
		}
		printJSON(runCtl(host, port, key, fmt.Sprintf("comment %s %s", rest[0], strings.Join(rest[1:], " "))))
		return 0
	case "whoami":
		printJSON(runCtl(host, port, key, "whoami"))
		return 0
	case "restart", "resume", "suspend", "keepalive", "shelly", "agent":
		if len(rest) < 1 {
			fmt.Fprintf(os.Stderr, "usage: spoondctl %s <id>\n", verb)
			return 1
		}
		v := verb
		if verb == "agent" {
			v = "shelly"
		}
		printJSON(runCtl(host, port, key, fmt.Sprintf("%s %s", v, rest[0])))
		return 0
	case "rm":
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "usage: spoondctl rm <id>")
			return 1
		}
		printJSON(runCtl(host, port, key, fmt.Sprintf("rm %s", rest[0])))
		return 0
	case "cp", "clone":
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "usage: spoondctl cp <id> [tag]")
			return 1
		}
		cmd := fmt.Sprintf("cp %s", rest[0])
		if len(rest) > 1 {
			cmd += " " + rest[1]
		}
		printJSON(runCtl(host, port, key, cmd))
		return 0
	case "new":
		cmd := "new"
		if len(rest) > 0 {
			cmd = "new " + rest[0]
		}
		printJSON(runCtl(host, port, key, cmd))
		return 0
	case "ls":
		printJSON(runCtl(host, port, key, "ls"))
		return 0
	case "env":
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "usage: spoondctl env ls|new <repo> <pr> [image]|rm <repo> <pr>|id <repo> <pr>")
			return 1
		}
		switch rest[0] {
		case "ls":
			printJSON(runCtl(host, port, key, "env ls"))
		case "new":
			if len(rest) < 3 {
				fmt.Fprintln(os.Stderr, "usage: spoondctl env new <repo> <pr> [image]")
				return 1
			}
			cmd := fmt.Sprintf("env new %s %s", rest[1], rest[2])
			if len(rest) > 3 {
				cmd += " " + rest[3]
			}
			printJSON(runCtl(host, port, key, cmd))
		case "rm":
			if len(rest) < 3 {
				fmt.Fprintln(os.Stderr, "usage: spoondctl env rm <repo> <pr>")
				return 1
			}
			printJSON(runCtl(host, port, key, fmt.Sprintf("env rm %s %s", rest[1], rest[2])))
		case "id":
			if len(rest) < 3 {
				fmt.Fprintln(os.Stderr, "usage: spoondctl env id <repo> <pr>")
				return 1
			}
			printJSON(runCtl(host, port, key, fmt.Sprintf("env id %s %s", rest[1], rest[2])))
		default:
			fmt.Fprintf(os.Stderr, "spoondctl env: unknown subcommand %q\n", rest[0])
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "spoondctl: unknown command %q\n\n", verb)
		usage()
		return 1
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
	fmt.Fprint(os.Stderr, `spoondctl — spoond control plane CLI (thin wrapper over ssh ctl@)

usage:
  spoondctl new [image]            create a sandbox (dev/go/py/elixir/llm)
  spoondctl ls                     list sandboxes
  spoondctl rm <id>                delete a sandbox
  spoondctl keepalive <id>         extend a persistent lease
  spoondctl suspend <id>           suspend (snapshot + stop)
  spoondctl resume <id>            resume from snapshot
  spoondctl restart <id>           reboot (snapshot + fresh sandbox)
  spoondctl cp <id> [tag]          clone a sandbox
  spoondctl shelly <id>            install + start the Shelley coding agent
  spoondctl tag <id> <name>        give the sandbox a friendly name
  spoondctl comment <id> [text]    set/clear a free-text annotation
  spoondctl whoami                 show the authenticated key identity
  spoondctl prompt <id> <message>  ask the agent in a sandbox something
  spoondctl ssh <id|name>          drop into a shell
  spoondctl env ls                 list per-PR ephemeral environments
  spoondctl env new <repo> <pr> [image]  create/ensure a PR environment
  spoondctl env rm <repo> <pr>     tear down a PR environment
  spoondctl env id <repo> <pr>     print the environment's sandbox id
  spoondctl help

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
