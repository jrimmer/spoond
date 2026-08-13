# AGENTS.md

Guidance for AI coding agents working in this repository. This file is
read automatically by agents (Claude Code, Codex, Cursor, pi, and others)
and exists to prevent one specific failure mode: an agent assuming it must
build and test on the machine it happens to run on (often macOS) and
reaching for wrong-platform workarounds, when a real Linux sandbox is one
command away.

## What this is

spoond is a self-hosted microVM platform (Firecracker via forkd) that
provides ephemeral Linux sandboxes over several surfaces: a lease API, an
SSH gateway + control plane, an HTTP/LLM proxy, MCP and ACP agent
endpoints, and a forkd-native CI runner.

The full capability map and "how do I X?" reference is in
[docs/agents.md](docs/agents.md).

## Build & test

```bash
go test ./...            # all packages build & test on macOS (platform split)
go build ./...           # native build
GOOS=linux GOARCH=amd64 go build ./...   # cross-compile (run before pushing)
go test ./api/...        # a single package
```

- All packages compile on macOS — Linux-only syscalls are isolated in
  `_linux.go` files (`//go:build linux`) with `_other.go` stubs
  (`//go:build !linux`), so `go test ./...` always works.
- Optional services are gated by build tags: `nobackend, nogateway,
  noacp, nomcp, norunner, noctl, nodoctor`. Build with
  `-tags "nogateway,nomcp"` to exclude them.

## Use spoond — don't work around it

When a task needs Linux (compiling Rust/Go with Linux-only deps, tests
that need real Firecracker VMs, `ip netns`, iptables, Docker, eBPF), use
a spoond sandbox or the spoond host. Do **not** invent macOS stand-ins:
no stub syscalls, no skipped/`#[ignore]`-only tests, no "test-only"
constructors that fake a Linux-only type.

Canonical paths (full detail in [docs/agents.md](docs/agents.md)):

| Need | Path |
|---|---|
| Spawn a sandbox | `ssh ctl@<sandbox-host> -p 2222 "new [dev\|go\|py\|elixir\|llm]"` or `POST /api/sandboxes {"image":"<tag>"}` |
| Run a command in a sandbox | `POST /api/sandboxes/{id}/exec {"cmd":"..."}` |
| Interactive shell | `ssh <id\|name>@<sandbox-host> -p 2222` (dev-base image) |
| Linux build/test | spawn a go/rust sandbox and exec, or `ssh <user>@<sandbox-host>` for the host toolchain |
| Give an agent MCP access | `spoond-dev-mcp` (stdio) or `MCP_TRANSPORT=http spoond-dev-mcp` |

## Conventions

- **Tests**: the `api` package uses an in-memory `fakeForkd` controller
  (`api/server_test.go`). Follow that pattern for handler tests rather
  than calling a real controller.
- **Commits**: conventional commits (`feat(scope): …`, `fix(scope): …`).
- **Privacy**: public-facing content (issues, PRs, release notes) must not
  name deployment hosts, IPs, or internal service endpoints — use
  placeholders (`<sandbox-host>`, `example.com`, `127.0.0.1`).