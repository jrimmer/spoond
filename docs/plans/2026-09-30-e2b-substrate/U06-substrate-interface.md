# U06 — `Substrate` interface and E2B client

## Purpose

A Go package that exposes E2B's orchestrator and envd to the rest of spoond
through one interface, plus an in-memory fake for unit tests. There is **no
forkd adapter**. Production keeps running the pre-U06 binary on forkd until
U12, and all E2B work runs on a staging instance (U08).

## Preconditions

- U05 is done.
- U04 is done: the orchestrator is healthy on vm2, and the registry is up.

## Facts relied on

- **Orchestrator gRPC:**
  - plaintext on `127.0.0.1:5008`;
  - services `SandboxService`, `TemplateService` and `InfoService` on one
    listener;
  - `orchestrator.proto` has **no `package` statement**, so method paths are
    `/SandboxService/Create` and so on;
  - the protos are in A2 §1, with our P3 and P4 additions from U03.
- **How E2B's API fills `SandboxConfig`** (A2 §3.2). Copy that pattern
  exactly:
  - `BaseTemplateId` = `TemplateId` (the image's template id);
  - `TeamId` = `E2B_TEAM_ID`;
  - `BuildId`;
  - `SandboxId`;
  - `ExecutionId` = a new UUID on **every** Create;
  - `KernelVersion`, `FirecrackerVersion`, `EnvdVersion` from the build
    record;
  - `EnvVars`, `Metadata`;
  - `EnvdAccessToken`;
  - `MaxSandboxLength` = 720 (hours);
  - `HugePages=true` (D12);
  - `RamMb`, `Vcpu`, `TotalDiskSizeMb` from the build record;
  - `Snapshot` = true only for resume;
  - `AutoPause=false`;
  - `Network{Egress}`;
  - `StartTime` = now;
  - `EndTime` = lease expiry.
- **Fork:** `Checkpoint(sandbox_id, build_id=new UUID)`, then N ×
  `Create(snapshot=false, build_id=<checkpoint build>, new sandbox_id)`
  (A2 §3.3).
- **Pause** needs a new UUID `build_id` and the sandbox's `template_id`.
  **Resume** is `Create(snapshot=true, same sandbox_id, build_id=<pause build>)`
  (A2 §3.4, §3.5).
- **Egress:** when `allowed_domains` is non-empty, `8.8.8.8` must be added to
  `allowed_cidrs` so DNS works (A2 §3.2 `buildEgressConfig`). Guest DNS is
  `8.8.8.8` (A3 D5).
- **IDs** (A2 §8):
  - `sandbox_id` = `"i"` + 20 chars from `[a-z0-9]`;
  - `template_id` = 20 chars from `[a-z0-9]`;
  - build, execution and team ids are UUIDs.
- **envd:**
  - guest port 49983, reached **through the orchestrator proxy**
    `http://127.0.0.1:5007`;
  - headers `E2b-Sandbox-Id: <id>`, `E2b-Sandbox-Port: 49983`,
    `X-Access-Token: <envd token>`;
  - run-as user via `Authorization: Basic base64("<user>:")`;
  - the Host must be an IP or `localhost` for header routing (A2 §4, §5);
  - Connect-RPC procedures `/process.Process/<Method>` (A2 §2.1).
  - Signals available: **only** `SIGNAL_SIGTERM` (15) and `SIGNAL_SIGKILL` (9).
- **Template build:**
  - `TemplateService.TemplateCreate`, then poll `TemplateBuildStatus` until
    `Completed` or `Failed` (A2 §3.7, §6);
  - the status cache lives 10 minutes, so poll every 2 s;
  - on completion, `metadata` carries `kernelVersion`, `firecrackerVersion`,
    `envdVersionKey` and `rootfsSizeKey`.
- **exec today** (A1 §5.9): `buildShellArgs(cmd,cwd,env)` =
  `["/bin/bash","-c","cd '<cwd>' && export 'K'='V'; <cmd>"]`. Timeout
  default 30 s, max `MAX_EXEC_TIMEOUT_SECS` (300). A missing sandbox maps to
  HTTP 410.

## Deliverables

```
substrate/
  substrate.go          interface + types (exact below)
  errors.go             ErrNotFound, ErrCapacity
  fake/fake.go          in-memory fake implementing Substrate
  fake/fake_test.go
  e2b/client.go         Client implementing Substrate
  e2b/config.go         Config + FromEnv()
  e2b/ids.go            NewSandboxID, NewTemplateID, NewUUID
  e2b/tokens.go         token seed + HMAC functions
  e2b/envd.go           envd Connect clients through the proxy
  e2b/egress.go         Egress -> proto
  e2b/client_live_test.go   (build tag e2blive)
  e2b/proto/            copies of the protos (never edited)
  e2b/gen/              generated Go (committed)
  e2b/gen.sh            regeneration script
```

## Dependencies (go.mod)

Add:
- `google.golang.org/grpc v1.84.0`
- `github.com/google/uuid v1.6.0`

`connectrpc.com/connect` and `google.golang.org/protobuf` are already
present (upgraded in U01). Do not add E2B's Go modules as dependencies. We
generate our own code from the protos.

## Proto copies and generation

1. Copy these files from the fork's `spoond` branch (U03) into
   `substrate/e2b/proto/`, **unchanged**:
   - `packages/orchestrator/orchestrator.proto`
   - `packages/orchestrator/template-manager.proto`
   - `packages/orchestrator/info.proto`
   - `packages/envd/spec/process/process.proto` → `proto/envd/process/process.proto`
   - `packages/envd/spec/filesystem/filesystem.proto` → `proto/envd/filesystem/filesystem.proto`

   Record the fork commit hash in `substrate/e2b/proto/SOURCE`.
2. `substrate/e2b/gen.sh` (exact). It runs **on vm2** (the only machine with
   `protoc` 34.1, installed in U03), from `/root/src/spoond`, with
   `export PATH=/usr/local/go/bin:/root/go/bin:/usr/local/bin:$PATH`.

   **Hand-off, fixed:** the worker first commits the proto copies and
   `gen.sh`, then pushes its unit branch (`impl/U06-…`) to `origin`. The Ops
   runner then, on vm2:
   1. `cd /root/src/spoond && git fetch origin && git checkout impl/U06-<slug> && git pull --ff-only`;
   2. runs `substrate/e2b/gen.sh`;
   3. commits `substrate/e2b/gen/` with the U06 commit message 1
      (`feat(substrate): E2B protos and generated clients`), as author
      `jrimmer <jason@rimmer.net>`;
   4. pushes the branch.

   The worker then `git pull`s and continues. Workers never run `gen.sh`
   themselves.

   Tool versions:
   - `protoc` 34.1;
   - `protoc-gen-go` **v1.36.12**;
   - `protoc-gen-go-grpc` **v1.6.2**;
   - `protoc-gen-connect-go` **v1.21.0**.

   Install the plugins with
   `go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12`,
   `go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2` and
   `go install connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.21.0`.

   ```bash
   #!/usr/bin/env bash
   set -euo pipefail
   cd "$(dirname "$0")"
   P=github.com/jrimmer/spoond/substrate/e2b/gen
   rm -rf gen && mkdir -p gen/orchestrator gen/info gen/template gen/envd/process gen/envd/filesystem
   M="--go_opt=Morchestrator.proto=$P/orchestrator --go-grpc_opt=Morchestrator.proto=$P/orchestrator \
      --go_opt=Minfo.proto=$P/info --go-grpc_opt=Minfo.proto=$P/info \
      --go_opt=Mtemplate-manager.proto=$P/template --go-grpc_opt=Mtemplate-manager.proto=$P/template"
   protoc -I proto --go_out=gen/orchestrator --go_opt=paths=source_relative --go-grpc_out=gen/orchestrator --go-grpc_opt=paths=source_relative $M proto/orchestrator.proto
   protoc -I proto --go_out=gen/info --go_opt=paths=source_relative --go-grpc_out=gen/info --go-grpc_opt=paths=source_relative $M proto/info.proto
   protoc -I proto --go_out=gen/template --go_opt=paths=source_relative --go-grpc_out=gen/template --go-grpc_opt=paths=source_relative $M proto/template-manager.proto
   protoc -I proto/envd --go_out=gen/envd --go_opt=paths=source_relative --go_opt=Mprocess/process.proto=$P/envd/process --connect-go_out=gen/envd --connect-go_opt=paths=source_relative --connect-go_opt=Mprocess/process.proto=$P/envd/process process/process.proto
   protoc -I proto/envd --go_out=gen/envd --go_opt=paths=source_relative --go_opt=Mfilesystem/filesystem.proto=$P/envd/filesystem --connect-go_out=gen/envd --connect-go_opt=paths=source_relative --connect-go_opt=Mfilesystem/filesystem.proto=$P/envd/filesystem filesystem/filesystem.proto
   gofmt -w gen
   ```
3. Run it and commit `gen/`. `go build ./substrate/...` must pass. If
   `info.proto` or `template-manager.proto` import other repo protos not
   copied here, add those files to `proto/` too, unchanged, and to `M`. If
   an import cannot be satisfied, STOP.

## `substrate/substrate.go` (exact)

```go
package substrate

import (
	"context"
	"net"
	"time"
)

// PrivateAllowance permits egress into an otherwise-denied private range (patch P4).
type PrivateAllowance struct {
	CIDR     string   // e.g. "10.1.0.11/32"
	TCPPorts []uint32 // empty = all TCP ports
}

// Egress is a sandbox's outbound policy in E2B's terms (built by U09 from spoond policies).
type Egress struct {
	AllowedCIDRs   []string
	DeniedCIDRs    []string
	AllowedDomains []string
	Private        []PrivateAllowance
}

type BuildRequest struct {
	TemplateID string // stable per image name
	BuildID    string // new UUID per build
	FromImage  string // e.g. "localhost:5000/py-base@sha256:<digest>"
	VCPU       uint32
	MemoryMB   uint32
	DiskSizeMB uint32
	StartCmd   string // may be ""
	ReadyCmd   string // may be ""
}

// BuildRefs lists every build whose data a build's artifacts reference
// (all ancestor layers plus the build itself), from E2B's SchedulingMetadata
// rootfs_build_ids and memfile_build_ids (A3 C1). U11's GC keeps them.
type BuildRefs struct {
	RootfsBuildIDs  []string
	MemfileBuildIDs []string
}

type BuildResult struct {
	BuildID            string
	KernelVersion      string
	FirecrackerVersion string
	EnvdVersion        string
	DiskSizeMB         uint32 // from metadata rootfsSizeKey
	Refs               BuildRefs // from metadata.schedulingMetadata
	Log                []string
}

type CreateRequest struct {
	TemplateID, BuildID, SandboxID                string
	KernelVersion, FirecrackerVersion, EnvdVersion string
	VCPU, MemoryMB, DiskSizeMB                    uint32
	Resume                                        bool // true: SandboxID must be the paused sandbox's id
	EnvVars, Metadata                             map[string]string
	EndAt                                         time.Time
	Egress                                        Egress
}

type Sandbox struct {
	ID, ExecutionID, TemplateID, BuildID, HostIP string
	VCPU, MemoryMB                               uint32
	StartedAt, EndAt                             time.Time
}

type NodeInfo struct {
	Status            string // "healthy" | "draining" | "unhealthy" | "standby" | "shutting_down"
	RunningSandboxes  int
	OutstandingWork   int
	HugepagesTotal    uint64
	HugepagesUsed     uint64
	HugepagesReserved uint64
	HugepageSizeBytes uint64
}

type ExecRequest struct {
	Args    []string // argv; spoond passes buildShellArgs(...)
	Timeout time.Duration // 0 = 30 s
	User    string // "" = "root"
}

type ExecResult struct {
	Stdout, Stderr string
	ExitCode       int
}

type StartRequest struct {
	Args       []string
	Env        map[string]string
	Cwd        string
	PTY        bool
	Cols, Rows uint32 // PTY size; 0 = 80x24
	User       string // "" = "root"
	Stdin      bool   // true: keep the process's stdin open for Write/CloseStdin
}

type EventKind int

const (
	EventStarted EventKind = iota // PID set
	EventStdout                   // Data set
	EventStderr                   // Data set
	EventPTY                      // Data set
	EventExit                     // ExitCode set
	EventError                    // Err set
)

type ProcessEvent struct {
	Kind     EventKind
	PID      uint32
	Data     []byte
	ExitCode int
	Err      string
}

// Process is an interactive guest process. Events is closed after EventExit or EventError.
type Process interface {
	Events() <-chan ProcessEvent
	Write(data []byte) error      // PTY input when started with PTY, else stdin
	Resize(cols, rows uint32) error
	Signal(kill bool) error        // false = SIGTERM, true = SIGKILL
	CloseStdin() error             // non-PTY only
	Close() error                  // stop streaming; does not kill the process
}

type Substrate interface {
	BuildTemplate(ctx context.Context, req BuildRequest) (BuildResult, error)
	DeleteBuild(ctx context.Context, templateID, buildID string) error

	Create(ctx context.Context, req CreateRequest) (Sandbox, error)
	List(ctx context.Context) ([]Sandbox, error)
	Delete(ctx context.Context, sandboxID string) error // nil when already gone
	Pause(ctx context.Context, sandboxID, templateID string) (buildID string, refs BuildRefs, err error)
	Checkpoint(ctx context.Context, sandboxID string) (buildID string, refs BuildRefs, err error)
	UpdateEgress(ctx context.Context, sandboxID string, eg Egress) error
	UpdateEndAt(ctx context.Context, sandboxID string, endAt time.Time) error

	NodeInfo(ctx context.Context) (NodeInfo, error)
	SetDraining(ctx context.Context, draining bool) error

	Health(ctx context.Context, sandboxID string) error // envd GET /health
	Exec(ctx context.Context, sandboxID string, req ExecRequest) (ExecResult, error)
	Start(ctx context.Context, sandboxID string, req StartRequest) (Process, error)
	DialGuest(ctx context.Context, sandboxID, hostIP string, port int) (net.Conn, error)
	TrafficToken(sandboxID string) string
}
```

`substrate/errors.go`:

```go
var (
	ErrNotFound = errors.New("substrate: not found") // sandbox or build unknown to the orchestrator
	ErrCapacity = errors.New("substrate: capacity")  // node refused (sandbox cap or starting limit)
)
```

## `substrate/e2b` behaviour (exact)

- **`Config`:**
  - fields `GRPCAddr`, `ProxyURL`, `TeamID`, `TokenSeed []byte`;
  - `func FromEnv() (Config, error)` reads these variables, using the
    default when a variable is unset or empty:
    - `E2B_GRPC_ADDR`, default `127.0.0.1:5008`;
    - `E2B_PROXY_URL`, default `http://127.0.0.1:5007`;
    - `E2B_TEAM_ID`, default `5b0f4e3a-8c1d-4f2e-9a6b-7d3c2e1f0a95`;
    - `E2B_TOKEN_SEED_FILE`, default `/etc/spoond/e2b-token-seed`. The seed is
      the file content, trimmed, as bytes; error if the file is unreadable or
      the trimmed content is shorter than 32 bytes.
- **Connection:** `func New(cfg Config) (*Client, error)` dials gRPC with
  `grpc.NewClient(cfg.GRPCAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))`
  and builds the `SandboxService`, `TemplateService` and `InfoService`
  clients.
- **IDs:**
  - `NewSandboxID()` = `"i"` + 20 chars drawn from `crypto/rand`, over the
    alphabet `abcdefghijklmnopqrstuvwxyz0123456789`;
  - `NewTemplateID()` = the same without the `"i"`;
  - `NewUUID()` = `uuid.NewString()`.
- **Tokens:**
  - `EnvdToken(id)` = `hex(HMAC-SHA256(seed, id))`;
  - `TrafficToken(id)` = `hex(HMAC-SHA256(seed, "sandbox-traffic-"+id))`.
- **`Create`:**
  - Build the `SandboxCreateRequest` as in Facts.
  - Always set `EnvdAccessToken=EnvdToken(id)` and
    `Network.Ingress.TrafficAccessToken=TrafficToken(id)`.
  - Map gRPC `ResourceExhausted` → `ErrCapacity` (wrapped with the gRPC
    message), and `NotFound` → `ErrNotFound`. The same mapping applies to
    `Pause` and `Checkpoint`, which return `ResourceExhausted` when the node
    is busy persisting another snapshot (A2 §3.6).
  - Return `Sandbox` with `HostIP` from the response's `host_ip` (P3).
  - After Create returns, poll `Health` every 200 ms for up to 30 s. If it
    never passes, `Delete` the sandbox and return an error.
- **`List`:** map each `RunningSandbox` `rs`: `ID = rs.GetSandboxId()`,
  `ExecutionID = rs.GetExecutionId()`, `TemplateID =
  rs.GetConfig().GetTemplateId()`, `BuildID = rs.GetConfig().GetBuildId()`
  (`RunningSandbox` has no top-level template or build id; they exist only
  in the deprecated `config`, A2 §1.1), `HostIP = rs.GetHostIp()` (P3),
  `VCPU = uint32(rs.GetVcpu())`, `MemoryMB = uint32(rs.GetRamMb())`,
  `StartedAt`/`EndAt` from `start_time`/`end_time`.
- **`Delete`:** gRPC `Delete{sandbox_id}`. `NotFound` returns nil.
- **`Pause`:** `buildID := NewUUID()`; call
  `Pause{sandbox_id, template_id, build_id}`. Then poll `NodeInfo` every
  500 ms until `OutstandingWork == 0`, up to 120 s. `outstanding_work`
  counts every in-flight tracked operation on the node, including template
  builds and other pauses (A3 A6), so it may not reach 0: on timeout, log
  `e2b: pause <sandbox_id>: outstanding_work still <n> after 120s` at Warn
  and continue. The timeout is **not** an error. Return `buildID` and the
  response's `scheduling_metadata` `rootfs_build_ids`/`memfile_build_ids` as
  `BuildRefs`.
- **`Checkpoint`:** `buildID := NewUUID()`;
  `Checkpoint{sandbox_id, build_id}`. The same `OutstandingWork` wait and
  log-and-continue rule apply. Return `buildID` and the response's
  `scheduling_metadata` as `BuildRefs`.
- **`UpdateEgress`:** `Update{sandbox_id, egress}`. **`UpdateEndAt`:**
  `Update{sandbox_id, end_time}`.
- **`NodeInfo`:** `InfoService.ServiceInfo`; map the status enum to the
  lowercase strings above, and copy the hugepage metrics
  (`metric_hugepages_*`, `metric_hugepage_size_bytes`), running count and
  `outstanding_work`.
- **`SetDraining`:** `ServiceStatusOverride{Draining}` or `{Healthy}`.
- **`BuildTemplate`:**
  - Call `TemplateCreate` with `TemplateConfig{templateID, buildID,
    memoryMB, vCpuCount, diskSizeMB, startCommand, readyCommand,
    teamID=TeamID, fromImage}`, plus `force=false`.
  - Poll `TemplateBuildStatus{templateID, buildID}` every 2 s, with no
    overall timeout except `ctx`.
  - On `Completed`, return versions from `metadata`, and `Refs` from
    `metadata.schedulingMetadata.rootfs_build_ids` and `memfile_build_ids`. On `Failed`, return an
    error that includes `reason` and the last 50 log lines.
- **`DeleteBuild`:** `TemplateBuildDelete{buildID, templateID}`.
- **envd calls:**
  - Base URL is `cfg.ProxyURL`.
  - A Connect interceptor and an `http.RoundTripper` add
    `E2b-Sandbox-Id`, `E2b-Sandbox-Port: 49983`,
    `X-Access-Token: EnvdToken(id)` and `Authorization: Basic base64(user+":")`
    with `user` defaulting to `root`.
  - `Health` = `GET {ProxyURL}/health` with those headers, expecting `204`
    or `200`.
- **`Start`:**
  - Call `process.Process/Start` with
    `ProcessConfig{cmd=Args[0], args=Args[1:], envs=Env}`, `cwd` set only
    when `Cwd != ""`, `stdin=req.Stdin`, and `pty={size{cols,rows}}` when
    PTY (0 cols or rows means 80×24).
  - Translate each `StartResponse.event`:
    - `start` → `EventStarted`;
    - `data.stdout`, `data.stderr`, `data.pty` → the matching event;
    - `end` → `EventExit` (with `exit_code`);
    - `keepalive` → ignore.
  - `Write`: `SendInput` with `ProcessInput{pty}` when PTY, else `{stdin}`.
  - `Resize`: `Update{pid, pty{size}}`.
  - `Signal`: `SendSignal{pid, SIGTERM|SIGKILL}`.
  - `CloseStdin`: `CloseStdin{pid}`.
  - The selector is always `{pid}`, from the start event.
- **`Exec`:**
  - `Start` without a PTY and with `Stdin=false`, collecting stdout and
    stderr until `EventExit`.
  - `Timeout == 0` means 30 s. Stop at `min(Timeout, ctx deadline)`.
  - **On timeout:** `Signal(true)`, wait up to 2 s for `EventExit`, and
    return `ExitCode=124` with
    `Stderr += "\n[spoond] exec timed out after <N>s\n"`.
  - When `Start` or the event stream fails, call `List`. If the sandbox id
    is not listed, return `ErrNotFound`; otherwise return the original
    error. (The proxy's HTTP status for an unknown sandbox is not quoted in
    the appendices, so it is not relied on.) `Start` applies the same rule.
- **`DialGuest`:** `net.DialTimeout("tcp", hostIP+":"+port, 5s)`. This works
  from the host because the orchestrator DNATs `HostIP` to the guest (A3 D4).
  Used by the SSH gateway only for non-envd ports, and by `expose_ports`
  checks.

## `substrate/fake`

An in-memory implementation for unit tests, `fake.New() *Fake`. Exact API:

```go
type Fake struct {
	Calls []string // one entry per call, "<Method> <first id argument>", e.g. "Create i0123...", "Pause i0123...", "NodeInfo"
	// unexported state
}

func New() *Fake
func (f *Fake) SetExecHandler(h func(sandboxID string, args []string) substrate.ExecResult)
func (f *Fake) SetNodeInfo(info substrate.NodeInfo, err error)   // default: Status "healthy", 1<<20 free 2 MiB pages, OutstandingWork 0
func (f *Fake) FailCall(method string, nth int, err error)        // the nth (1-based) call of method returns err; nth 0 = every call
func (f *Fake) SetHealthErr(sandboxID string, err error)          // Health(sandboxID) returns err
func (f *Fake) Kill(sandboxID string)                             // remove from List without a Delete call (simulates a crash)
func (f *Fake) Proc(pid uint32) *FakeProcess                      // the process started with this pid

type FakeProcess struct {
	Inputs  [][]byte   // every Write
	Resizes [][2]uint32 // every Resize
	Signals []bool     // every Signal (true = SIGKILL)
	StdinClosed, Closed bool
}
func (p *FakeProcess) Push(ev substrate.ProcessEvent) // deliver an event to Events()
```

- Sandboxes are map entries. `Create` stores one, `Delete` and `Pause`
  remove one, `List` returns them.
- `Exec` runs nothing: it returns stdout = the args joined with spaces,
  exit 0, unless `SetExecHandler` is set.
- `Start` assigns pids from 1 upward, emits `EventStarted`, and then only
  what the test `Push`es. `Events()` closes after a pushed `EventExit` or
  `EventError`.
- `Pause`, `Checkpoint` and `BuildTemplate` return new UUIDs and empty
  scheduling metadata.
- `NodeInfo` returns what `SetNodeInfo` set.

## Tests

- `substrate/e2b` unit tests with no network:
  - `NewSandboxID` matches `^i[a-z0-9]{20}$`, and `NewTemplateID` matches
    `^[a-z0-9]{20}$`;
  - tokens are deterministic for a fixed seed and differ between envd and
    traffic;
  - the egress conversion adds `8.8.8.8` to allowed CIDRs iff domains are
    present, and maps `Private` into `allowed_private`.
- `substrate/fake/fake_test.go`: the interface is satisfied (a compile
  check), and the call log records calls.
- **`substrate/e2b/client_live_test.go`**, with build tag `e2blive`. Run on
  vm2 with `E2B_*` env set:
  1. `BuildTemplate` from `docker.io/library/debian:12` with 2 vCPU,
     1024 MB, 4096 MB disk and `ReadyCmd` empty. It completes, and the
     versions are non-empty.
  2. `Create` from that build. `HostIP` matches `^10\.11\.`.
  3. `Exec(["/bin/bash","-c","echo ok"])` returns `ok`.
  4. `Exec(["/bin/bash","-c","sleep 30"], 2s)` returns exit `124`.
  5. `Start` with a PTY runs `bash -l`; write `echo X$((1+1))\n`, see `X2`,
     then write `exit\n` and see `EventExit`.
  6. `Checkpoint`, then `Create` 2 sandboxes from the checkpoint build. Both
     answer `Exec`.
  7. `Pause` a sandbox, then `Create(Resume=true)` with the same id. A file
     written before the pause still exists.
  8. `Delete` everything; `List` no longer contains them.
  9. `DeleteBuild` for the builds created.

  Run it with: `go test -tags e2blive -count=1 -timeout 30m ./substrate/e2b/`.

## Commits

1. `feat(substrate): E2B protos and generated clients`
2. `feat(substrate): Substrate interface, E2B client and fake`

## Done when

- `go test ./...` passes.
- The live test passes on vm2.

## Do not

- Do not edit the proto copies.
- Do not import `github.com/e2b-dev/infra/...`.
- Do not wire the Substrate into `api/` yet. That is U08.
