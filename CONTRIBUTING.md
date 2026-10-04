# Contributing

Thanks for wanting to help with spoond! This project is
Apache-2.0 licensed (see `LICENSE`) and developed in the open.

## Code of conduct

Be respectful and constructive. This project is small and
maintainer-driven; questions are welcome in issues before opening PRs.

## Getting started

```bash
git clone <repo-url> spoond && cd spoond
go build ./...
go test ./...
```

Requirements: Go 1.27.1+ (see `go.mod`).

## How to contribute

1. **Open an issue first** — explain the problem or feature, with
   context (what you're trying to do, how you hit the problem, why the
   change is the right one). One issue per PR keeps the conversation
   traceable.
2. **Fork, branch, implement** — branch names like
   `fix/<what>` or `feat/<what>`.
3. **Keep the gates green** before opening the PR:
   - `go build ./...`
   - `go vet ./...`
   - `gofmt -l .` (must list nothing)
   - `go test ./...` (unit tests; the integration suite needs a live
     spoond host — see below)
4. **Sign your commits** with DCO — every commit must carry a
   `Signed-off-by` trailer:
   ```bash
   git commit -s
   ```
   By signing you agree to license your contribution under Apache-2.0
   (LICENSE, Section 5).
5. **Open the PR** referencing the issue. Describe what changed, how
   you verified it, and any trade-offs/drawbacks you're aware of.

## Integration tests

`tests/integration/` exercises the full stack (lease API, SSH gateway,
control plane, MCP, network policy) against a **live spoond host on
the E2B substrate**: the orchestrator, the image catalog, the warm pool.
It cannot run on CI without that infrastructure.

- CI runs only `go build ./...` + `go test ./...` (unit tests).
- To run the full suite locally against a live host:

  ```bash
  SSHHOST=root@<vm> BE_API=https://127.0.0.1:8890 bash tests/integration/run.sh
  ```

  Expect the suite to match the baseline count in the ticket tracker;
  a couple of cold-pool flakes are known (netpolicy exec timing) and
  pass on re-run.

## Design notes

- **Plain Go binaries over Docker** in the deploy targets; the homelab
  runs them under systemd.
- **No image proliferation** — add new toolchains to an existing image's
  Dockerfile, don't add per-feature images.
- Internal traffic never goes through the reverse proxy (Pangolin);
  Caddy fronts only the public endpoints.
- The sandbox substrate is E2B's orchestrator (a patch-queue fork of
  `github.com/e2b-dev/runtime`); spoond speaks to it only through the
  `substrate/` interface.
- Homelab addresses (10.0.0.*, *.example.com) appear as **defaults only**
  and are overridable via env/flags (see `cmd/*/main.go`). Keep it that
  way for new knobs.

## Getting help

Open an issue, or ask in the project's chat if you have one.
