# Plan: hyper-runner — Forgejo Actions Runner on Hyper microVMs

**Date:** 2026-07-12  
**Status:** Approved — Phase 1 implementation  
**Author:** Jason + Hermes  
**Repository:** `jrimmer/hyper-forgejo-runner` (own repo)

---

## 1. Problem

Forgejo Actions runs on `act_runner` (10.1.0.47), which executes jobs as Docker containers on the host. We have Hyper — a Firecracker microVM orchestrator — that provides isolated, ephemeral execution environments. Running CI jobs in microVMs instead of host Docker containers gives us:

- **Isolation** — each job gets a fresh VM, no shared state
- **No Docker dependency** — Hyper manages the lifecycle
- **Unified platform** — same execution engine as the command adapter (exe.dev/Replit-style product)

`sandbox-api` already bridges Forgejo and Hyper for a subset of workflows, but it's a REST API that requires custom workflow YAML per repo. `hyper-runner` replaces `act_runner` directly — it speaks the Forgejo runner gRPC protocol natively, so **existing `.forgejo/workflows/*.yml` files work unmodified**.

## 2. Goals

- **G1:** Register as a Forgejo runner, receive tasks via the runner gRPC protocol
- **G2:** Parse `WorkflowPayload` YAML, evaluate `${{ }}` expressions, execute steps
- **G3:** Execute each step inside a Hyper microVM via guest-agent `Exec` RPC
- **G4:** Stream logs back to Forgejo UI in real-time via `UpdateLog`
- **G5:** Report step/job results via `UpdateTask`
- **G6:** Support the actions used by our repos today: `checkout`, `setup-go`, `setup-node`, `run:`
- **G7:** Extensible action handler registry — add new actions in single files

## 3. Non-Goals

- Modifying `sandbox-api` (stays as-is)
- Full GitHub Actions engine (matrix, reusable workflows, composite actions — Phase 4+)
- Docker-based actions (`docker://` image steps — Phase 6)
- GitHub Marketplace actions (beyond hardcoded handlers)
- Renovate (has its own runner)
- Runner auto-registration UI (manual `register` command for now)

## 4. Architecture

```
┌─────────────┐     connectrpc (HTTP/2)      ┌──────────────────┐
│   Forgejo    │ ◄──────────────────────────►│   hyper-runner   │
│  (10.1.0.47) │   Register/Declare/         │  (10.1.0.80)     │
│              │   FetchTask/UpdateTask/      │                  │
│              │   UpdateLog                 │  ┌────────────┐  │
│              │                             │  │ YAML parse │  │
│              │                             │  │ + expr eval│  │
│              │                             │  └─────┬──────┘  │
│              │                             │        │         │
│              │                             │  ┌─────▼──────┐  │
│              │                             │  │  Step      │  │
│              │                             │  │  Executor  │  │
│              │                             │  │  (action   │  │
│              │                             │  │  handlers) │  │
│              │                             │  └─────┬──────┘  │
│              │                             │        │         │
│              │      gRPC (Hyper)           │  ┌─────▼──────┐  │
│              │                             │  │ Hyper      │  │
│              │                             │  │ gRPC client│  │
│              │                             │  │ (CreateVm, │  │
│              │                             │  │  StopVm)   │  │
│              │                             │  └─────┬──────┘  │
│              │                             │        │         │
│              │                             │  ┌─────▼──────┐  │
│              │                             │  │ Guest-agent│  │
│              │                             │  │ Unix socket│  │
│              │                             │  │ (Exec)     │  │
│              │                             │  └────────────┘  │
└─────────────┘                             └──────────────────┘
                                                   │
                                                   ▼
                                           ┌──────────────────┐
                                           │  Hyper cluster    │
                                           │  (Firecracker)    │
                                           │  172.30.0.1:50051 │
                                           └──────────────────┘
```

### 4.1 Package Layout

```
hyper-runner/
├── main.go                  # Entry point: config load → register → poll loop
├── config.go                # Env-based config (Forgejo URL, token, Hyper addr, labels)
├── runner/
│   ├── client.go            # connectrpc client wrapper (Register/Declare/FetchTask/Update*)
│   ├── poll.go              # FetchTask long-poll loop, task dispatch
│   └── register.go          # One-time registration + .runner file management
├── workflow/
│   ├── parser.go            # Parse WorkflowPayload YAML → Job + Steps
│   ├── expr.go              # ${{ }} expression evaluator (github.*, secrets.*, env.*, vars.*)
│   ├── types.go             # Workflow, Job, Step, context structs
│   └── action/
│       ├── handler.go       # Handler interface + Executor abstraction
│       ├── registry.go      # "actions/checkout@v4" → Handler mapping
│       ├── checkout.go      # actions/checkout: git clone + checkout SHA
│       ├── setup_go.go      # actions/setup-go: download Go, set PATH/GOROOT
│       ├── setup_node.go    # actions/setup-node: download Node, set PATH
│       └── run.go           # run: shell execution (/bin/sh -c)
├── hyper/
│   ├── client.go            # Hyper gRPC client (CreateVm, StopVm, ListVms)
│   ├── agent.go             # Guest-agent client (Exec, Health) via Unix socket
│   └── proto/               # Generated Go stubs for hyper.proto + agent.proto
└── go.mod
```

### 4.2 Key Interfaces

**Action handler** — the extension point:

```go
package action

// Handler executes a single workflow step inside a VM.
type Handler interface {
    Execute(ctx context.Context, step *Step, exec Executor, logger Logger) (exitCode int, err error)
}

// Executor runs a command in the guest VM via guest-agent.
type Executor interface {
    Exec(argv []string, env map[string]string, cwd string) (stdout, stderr string, exitCode int, err error)
    ExecStreaming(argv []string, env map[string]string, cwd string, onLine func(string)) (exitCode int, err error)
}

// Logger sends log lines back to Forgejo via UpdateLog.
type Logger interface {
    Log(line string)
    Logf(format string, args ...any)
}
```

**Registration in `registry.go`:**

```go
var handlers = map[string]Handler{
    "actions/checkout":  &CheckoutHandler{},
    "actions/setup-go":  &SetupGoHandler{},
    "actions/setup-node": &SetupNodeHandler{},
}

func Resolve(uses string) (Handler, bool) {
    // Strip @version suffix: "actions/checkout@v4" → "actions/checkout"
    key := strings.Split(uses, "@")[0]
    h, ok := handlers[key]
    return h, ok
}
```

### 4.3 Task Execution Flow

```
FetchTask (blocking) → receive Task
  │
  ├── Parse WorkflowPayload YAML → extract job's steps
  ├── Build expression context from Task.Context, Secrets, Vars, Needs
  ├── CreateVm (template based on labels or config)
  ├── Wait for guest-agent Health → ok
  │
  ├── For each Step:
  │   ├── Evaluate `if:` condition (if present) — skip if false
  │   ├── Resolve action handler (registry) or fall back to `run:`
  │   ├── Merge step env + job env + context-derived env
  │   ├── handler.Execute(step, exec, logger)
  │   │   ├── logger.Log("[step name]")  → UpdateLog
  │   │   ├── exec.ExecStreaming(...)     → guest-agent Exec
  │   │   └── each stdout/stderr line     → logger.Log() → UpdateLog
  │   ├── Update StepState (result, timestamps, logIndex/length)
  │   ├── UpdateTask (state with all steps)
  │   └── If exit code != 0 and `if-fails` = stop → break
  │
  ├── StopVm
  ├── Final UpdateTask (job result = success/failure)
  └── UpdateLog (noMore = true)
```

### 4.4 Expression Evaluator

Initial scope — the `${{ }}` patterns used by our workflows:

| Pattern | Source | Example |
|---|---|---|
| `github.sha` | `Task.Context` struct | `${{ github.sha }}` → commit SHA |
| `github.ref` | `Task.Context` struct | `${{ github.ref }}` → `refs/heads/main` |
| `github.repository` | `Task.Context` struct | `${{ github.repository }}` → `jrimmer/scout` |
| `github.event_name` | `Task.Context` struct | `push`, `pull_request` |
| `secrets.*` | `Task.Secrets` map | `${{ secrets.FORGEJO_TOKEN }}` |
| `env.*` | Step/job `env:` blocks | `${{ env.GO_VERSION }}` |
| `vars.*` | `Task.Vars` map | `${{ vars.NODE_VERSION }}` |

Implementation: a recursive-descent parser for `${{ expr }}` that handles:
- Dot access: `github.sha`, `secrets.X`
- String literals: `'main'`
- Comparison: `==`, `!=`
- Boolean: `&&`, `||`, `!`
- Function calls (Phase 2): `contains()`, `startsWith()`, `success()`, `failure()`

~150 lines for Phase 1.

### 4.5 Protocol Details

**Forgejo runner protocol** (connectrpc, not standard gRPC):

| RPC | Request | Response | Purpose |
|---|---|---|---|
| `Register` | `{name, token, labels, version}` | `{runner: {id, uuid, token}}` | One-time registration |
| `Declare` | `{version, labels, capabilities}` | `{runner}` | Announce capabilities after registration |
| `FetchTask` | `{tasks_version}` | `{task, tasks_version}` | Long-poll for next task |
| `UpdateTask` | `{state: {id, result, steps[]}, outputs}` | `{state, sent_outputs}` | Report step/job state |
| `UpdateLog` | `{task_id, index, rows[], no_more}` | `{ack_index}` | Stream log lines |

Connect client construction:
```go
client := runnerv1connect.NewRunnerServiceClient(
    http.DefaultClient,
    forgejoURL,  // http://10.1.0.47:3000
    connect.WithGRPC(),  // use gRPC binary protocol
)
```

**Hyper gRPC** (standard gRPC, not connectrpc):

| RPC | Purpose |
|---|---|
| `CreateVm` | Boot microVM from template image |
| `StopVm` | Tear down VM |
| `GetVm` | Locate VM (health check) |

**Guest-agent gRPC** (Unix socket, per-VM):

| RPC | Purpose |
|---|---|
| `Health` | Check agent readiness (poll until ok) |
| `Exec` | Run command, capture stdout/stderr + exit code |

Guest-agent socket: `unix:///srv/hyper/socks/grpc-{vm_id}.sock`

**Important:** grpc-go requires `grpc.default_authority=localhost` and `grpc.http2.authority_override=localhost` options when connecting via Unix sockets — tonic (the guest agent's gRPC server) rejects the default authority header.

### 4.6 Template Selection

Map workflow `runs-on:` labels to Hyper templates:

| Label | Template | Image | Instance Type |
|---|---|---|---|
| `ubuntu-latest` | `ubuntu-base` | `docker.io/library/ubuntu:24.04` | DECI (2 vCPU, 1 GiB) |
| `go` / `golang` | `ubuntu-base` | `docker.io/library/ubuntu:24.04` | DECI (2 vCPU, 1 GiB) |
| `node` | `node-test` | `docker.io/library/node:22-alpine` | BASE (4 vCPU, 2 GiB) |
| `python` | `python-test` | `docker.io/library/python:3.12-alpine` | DECI (2 vCPU, 1 GiB) |
| `alpine` | `alpine-smoke` | `docker.io/library/alpine:3.20` | MILLI (0.5 vCPU, 256 MiB) |

Configurable via `hyper-runner.yaml`.

## 5. Build & Deploy

### 5.1 Build Environment

**Build host: ci-runner (10.1.0.190)** — Ubuntu 24.04 VM, native Linux build, Go to be installed.

Deploy the compiled binary to 10.1.0.80 (sandbox-api host, where Hyper runs).

```bash
# On ci-runner (10.1.0.190):
sudo apt install -y golang-go
cd /tmp && git clone git@git.lacy.casa:jrimmer/hyper-forgejo-runner.git
cd hyper-forgejo-runner && go build -o hyper-runner ./cmd/hyper-runner
scp hyper-runner root@10.1.0.80:/opt/hyper-runner/
```

### 5.2 Dependencies

| Module | Version | Purpose |
|---|---|---|
| `code.gitea.io/actions-proto-go` | v0.6.0 | Forgejo runner protocol (connectrpc stubs) |
| `connectrpc.com/connect` | v1.15.0 | connectrpc client (required by actions-proto-go) |
| `google.golang.org/grpc` | latest | Hyper gRPC + guest-agent gRPC |
| `google.golang.org/protobuf` | v1.32.0+ | Proto runtime |
| `gopkg.in/yaml.v3` | latest | WorkflowPayload YAML parsing |
| `github.com/go-git/go-git/v5` | latest | `actions/checkout` implementation |

No Docker SDK. No actionlint. No act.

### 5.3 Proto Generation

Hyper and agent protos need Go stubs. Copy `.proto` files from sandbox-api and generate:

```bash
# From hyper-runner/ directory
protoc --go_out=. --go-grpc_out=. hyper.proto
protoc --go_out=. --go-grpc_out=. agent.proto
```

The Forgejo runner proto stubs come pre-generated in `actions-proto-go` v0.6.0 — no protoc needed for those.

### 5.4 Deployment

```
# On 10.1.0.80 (sandbox-api host):
/opt/hyper-runner/hyper-runner          # binary
/opt/hyper-runner/hyper-runner.yaml     # config
/opt/hyper-runner/.runner               # registration file (auto-generated)

# systemd service: /etc/systemd/system/hyper-runner.service
[Unit]
Description=Hyper Forgejo Runner
After=network.target

[Service]
Type=simple
ExecStart=/opt/hyper-runner/hyper-runner daemon --config /opt/hyper-runner/hyper-runner.yaml
Restart=on-failure
RestartSec=5
Environment=HOME=/root

[Install]
WantedBy=multi-user.target
```

### 5.5 Registration

```bash
/opt/hyper-runner/hyper-runner register \
  --instance http://10.1.0.47:3000 \
  --name hyper-runner \
  --labels "ubuntu-latest:host,go:host,node:host,python:host" \
  --token <registration-token from Forgejo admin>
```

This writes `.runner` file with UUID + auth token. Done once.

### 5.6 Migration from act_runner

1. Stop `act-runner` on 10.1.0.47: `systemctl stop act-runner`
2. De-register `sandbox-host-runner` in Forgejo admin (or leave it idle)
3. Register `hyper-runner` against the same Forgejo instance
4. Start `hyper-runner` on 10.1.0.80
5. Verify a test workflow runs

No workflow file changes needed — same `runs-on: ubuntu-latest` labels.

## 6. Implementation Phases

### Phase 1 — MVP (this plan)

**Delivers:** working runner that executes `checkout → setup-go → go test` and `checkout → run: shell` workflows.

| Component | Lines (est.) | Status |
|---|---|---|
| `runner/client.go` | ~150 | New |
| `runner/poll.go` | ~100 | New |
| `runner/register.go` | ~80 | New |
| `workflow/parser.go` | ~200 | New |
| `workflow/expr.go` | ~150 | New |
| `workflow/types.go` | ~80 | New |
| `action/handler.go` | ~30 | New |
| `action/registry.go` | ~40 | New |
| `action/checkout.go` | ~80 | New |
| `action/setup_go.go` | ~100 | New |
| `action/setup_node.go` | ~80 | New |
| `action/run.go` | ~50 | New |
| `hyper/client.go` | ~120 | New |
| `hyper/agent.go` | ~100 | New |
| `hyper/proto/` | generated | New |
| `config.go` | ~60 | New |
| `main.go` | ~80 | New |
| **Total** | **~1,500** | |

### Phase 2 — Conditionals + step env

- `if:` conditionals on steps (`${{ success() }}`, `${{ failure() }}`, `${{ github.ref == 'refs/heads/main' }}`)
- `with:` inputs to actions
- Step outputs (`${{ steps.X.outputs.Y }}`)
- `setup-elixir` handler (for netcrawl)
- `continue-on-error`

### Phase 3 — Artifacts + caching

- `actions/cache` — cache directories between runs (S3 or local)
- `actions/upload-artifact`, `actions/download-artifact`
- Log retention backend

### Phase 4 — Matrix + job dependencies

- `strategy.matrix` — expand into multiple VM executions
- Job outputs (`outputs:`) propagated via `Needs`
- `needs:` context in expressions

### Phase 5 — Composite actions

- Local action repos (`.forgejo/actions/`)
- `uses: ./path/to/action` resolution
- Nested step execution

### Phase 6 — Docker image steps

- `container: image` → Hyper `LoadImage` + `CreateVm`
- `docker://` action references → boot as VM

## 7. Risks & Mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| **Expression evaluator misses edge cases** | Workflows fail on unparseable `${{ }}` | Start with exact patterns from our repos; log unparseable expressions as warnings, pass through as literal strings |
| **Guest-agent socket not ready when Exec is called** | First steps fail with connection refused | Poll `Health` with 1s interval, 30s timeout (matches sandbox-api pattern) |
| **WorkflowPayload format differs from expectation** | Parser fails on valid workflows | Test against all 9 workflows in our repos before shipping; log raw payload on parse failure |
| **connectrpc vs grpc compatibility** | Runner can't talk to Forgejo | `actions-proto-go` uses `connect.WithGRPC()` — confirmed compatible with Forgejo's gRPC endpoint |
| **Hyper capacity exhaustion** | CreateVm returns RESOURCE_EXHAUSTED | Config `max_concurrent` (default 3, matching act_runner); FetchTask blocks until a slot is free |
| **No internet in microVM** | `git clone`, `go install` fail | Checkout uses host-side `git clone` then copies into VM via Exec; or use a template image with git pre-cached |

## 8. Testing Strategy

### 8.1 Unit Tests

- `workflow/expr_test.go` — expression evaluator against all patterns in our workflows
- `workflow/parser_test.go` — parse each repo's workflow YAML, assert step extraction
- `action/registry_test.go` — handler resolution, version stripping
- `hyper/agent_test.go` — mock Unix socket, test Exec + Health

### 8.2 Integration Test

A test workflow in `sandbox-test-demo` repo:

```yaml
# .forgejo/workflows/hyper-runner-test.yml
on: [push]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'
      - run: go test ./...
      - run: echo "hyper-runner works!"
```

### 8.3 Regression

Before cutover, run all 9 existing workflows against `hyper-runner` in parallel with `act_runner` still active (different labels). Compare results.

## 9. Resolved Decisions

| # | Question | Decision |
|---|---|---|
| 1 | Build host | **ci-runner (10.1.0.190)** — native Linux build, Go to be installed |
| 2 | Repository | **Own repo: `jrimmer/hyper-forgejo-runner`** |
| 3 | Template selection | **Label → template map** (config file). `container:` override deferred to Phase 6 |
| 4 | Checkout strategy | **Clone inside VM** — Hyper VMs have LAN access to Forgejo |

## 10. References

- `actions-proto-go` v0.6.0: `code.gitea.io/actions-proto-go`, connectrpc-based
- Hyper gRPC proto: `/opt/sandbox-api/src/proto/hyper.proto` (v0, package `hyper.grpc.v0`)
- Agent gRPC proto: `/opt/sandbox-api/src/proto/agent.proto` (v1, package `hyper.agent.v1`)
- Guest-agent socket: `unix:///srv/hyper/socks/grpc-{vm_id}.sock`
- Hyper gRPC endpoint: `172.30.0.1:50051` (Docker bridge gateway)
- act_runner config: `/var/lib/act-runner/config.yaml` (10.1.0.47)
- act_runner registration: `/var/lib/act-runner/.runner` (runner id=12, `sandbox-host-runner`)
- Template images: `TEMPLATE_IMAGES` in `job_manager.py` (alpine, ubuntu, node, python, playwright)