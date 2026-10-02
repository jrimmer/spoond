# spoond: verbatim facts for the forkd → E2B orchestrator substrate swap

Source: `/home/jrimmer/Work/spoond`, branch `docs/microsandbox-recheck` (HEAD `e46fda4`), extracted 2026-09-30. Nothing in the repo was modified.
All code blocks are verbatim copies of the named line ranges (produced mechanically from the files, not retyped). Line numbers are 1-based.

Flags used below:
- **NOTE**: an observation that matters for the rewrite.
- **AMBIGUOUS**: the code and comments disagree, or the behaviour can't be settled from the repo.

---

## 0. Headline facts (read first)

1. **Lease state is in memory only. Nothing persists it.** `api.Store` is a mutex plus four Go maps (§3.3). No file, no JSON, no DB, nothing written on mutation. After a backend restart every lease is forgotten, and `ReconcileOrphans` kills every controller sandbox the new process didn't create (§3.9). The only persisted store in the repo is the **identity store** (`identity/store.go`: a JSON file written on every mutation via temp+rename, path from `USERS_FILE`, with a `<file>.salt` sidecar) (§3.3).
2. **Only 6 Go files import `github.com/jrimmer/spoond/forkd`:** `api/service.go`, `api/server.go`, `api/probe_test.go`, `api/server_test.go`, `cmd/spoond-backend/main.go`, `cmd/spoond-doctor/main.go`. Everything else (runner, mcp, acp, cfos, commandadapter, gateway) talks to the **lease HTTP API** (§14, §15).
3. **Some handlers call `s.svc.forkd` directly, not through a `Service` method:** `handleExec` (`Exec`), `handlePrompt` (`Exec`), `handleStat` (`Exec`), `handleClone` (`Branch`), `handleMetrics` (`Metrics`). `ImageRegistry` calls `SnapshotExists`/`ListSnapshots`.
4. **Guest reachability is netns-based in three places.** Each does its own `setns` into `/var/run/netns/<Netns>`: `api/netns_linux.go dialInNetns` (used by `handleStream` and the HTTP proxy), and `cmd/spoond-sshd-gateway/main.go dialSandbox`, which has a separate copy of the setns code. The gateway learns `netns` and `guest_addr` from `GET /api/sandboxes/{id}/endpoint`.
5. **go.mod status:** `connectrpc.com/connect v1.20.0` and `google.golang.org/protobuf v1.36.11` are already **direct** dependencies (used by the Forgejo runner proto `gitea.dev/actions-proto-go`). `google.golang.org/grpc` is **absent**. **No sqlite driver** is present (neither `modernc.org/sqlite` nor `mattn/go-sqlite3`).

---

## 1. go.mod

`go.mod` L1-23:

```text
module github.com/jrimmer/spoond

go 1.25.0

require (
	connectrpc.com/connect v1.20.0
	gitea.dev/actions-proto-go v0.6.0
	github.com/gorilla/websocket v1.5.3
	github.com/prometheus/client_golang v1.24.1
	golang.org/x/crypto v0.54.0
	golang.org/x/sys v0.47.0
	google.golang.org/protobuf v1.36.11
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.70.1 // indirect
	github.com/prometheus/procfs v0.21.1 // indirect
)
```

- Module path: `github.com/jrimmer/spoond`
- Go version: `go 1.25.0` (no `toolchain` line)
- Direct deps (8): `connectrpc.com/connect v1.20.0`, `gitea.dev/actions-proto-go v0.6.0`, `github.com/gorilla/websocket v1.5.3`, `github.com/prometheus/client_golang v1.24.1`, `golang.org/x/crypto v0.54.0`, `golang.org/x/sys v0.47.0`, `google.golang.org/protobuf v1.36.11`, `gopkg.in/yaml.v3 v3.0.1`
- Indirect deps (6): beorn7/perks, cespare/xxhash/v2, munnerz/goautoneg, prometheus/client_model, prometheus/common, prometheus/procfs.
- `google.golang.org/grpc`: **absent**. `connectrpc.com/connect`: **present (direct)**. `google.golang.org/protobuf`: **present (direct)**. sqlite (any driver): **absent**.
- `tests/integration/wsclient` is a **separate module**, `module forkd-itest/wsclient`, `go 1.23`, requiring `github.com/gorilla/websocket v1.5.3`.
- Where connect/protobuf are used: `runner/client.go` (a Forgejo ping stub via `pingv1connect`) and `runner/forgejo_adapter.go` (the Forgejo runner protocol).

---

## 2. Repository layout

Top-level entries: `acp/ api/ cfos/ cmd/ commandadapter/ deploy/ docs/ forkd/ identity/ images/ mcp/ metrics/ runner/ scripts/ tests/ workflow/`, plus `.forgejo/` (CI), `.claude/`, `README.md`, `CONTRIBUTING.md`, `LICENSE`, `NOTICE`, `go.mod`, `go.sum`.

| Package / dir | Purpose (one line) |
|---|---|
| `api` | Lease service (`Service`, in-memory `Store`, warm pool, TTL/idle sweeper), HTTP lease API (`Server`), HTTP proxy, LLM gateway, users/shares endpoints, netns egress policy (iptables), `dialInNetns`. **Main rewrite target.** |
| `forkd` | Typed HTTP client for the forkd-controller REST API (`/v1/...`). **To be replaced.** |
| `identity` | User store (person/agent, SSH fingerprints, token hash, LLM key hash, quotas); JSON-file persisted. |
| `metrics` | Prometheus metric definitions (backend, gateway, runner) plus a forkd→`spoond_controller_` rename helper. |
| `runner` | Forgejo Actions runner: job executor, workflow parsing/expr, adaptive runner pool, Forgejo connect adapter, `HTTPLeaseClient` (lease API client, also used by mcp/acp/cfos). |
| `mcp` | MCP server (stdio and streamable HTTP) exposing shell/file tools backed by `runner.SandboxProvider`. |
| `acp` | Agent Client Protocol (JSON-RPC over stdio) endpoint: agent loop with the LLM via `/llm/<lease>/openai/...` and tools run through `SandboxProvider`. |
| `cfos` | CFOS/Sandstorm `executeCode` bridge (`POST /v1/execute`) over `SandboxProvider`. |
| `commandadapter` | Synchronous "run a command in a fresh sandbox" HTTP adapter over `SandboxProvider`. **Not wired into any `cmd/`** (only its own tests import it). |
| `workflow`, `workflow/action` | Standalone Forgejo workflow types/parser/expr and action handlers. **Not imported by any other package** (the runner has its own `runner/workflow.go`). |
| `images/` | Image manifest, dockerfiles, scylla boot hook, `validate-image.py`. |
| `deploy/` | systemd units, bake scripts, forkd rollout/watchdog scripts, installer, Caddy snippet, guest `rootfs-init`. |
| `tests/integration/` | Bash integration suite against a live stack (host) plus a `wsclient` Go helper. |
| `scripts/forkd-curl` | Authenticated curl wrapper for the lease API. |
| `docs/` | api.md, ci-jobs.md, ctl.md, install.md, operations.md, security.md, setup.md, substrate-backends.md, usage.md, design/, plans/ (including the untracked `docs/plans/2026-09-29-001-feat-e2b-runtime-substrate-plan.md`). |

### cmd/ binaries

| Dir | Go package | Purpose |
|---|---|---|
| `cmd/spoond` | `main` | **The deployed binary.** A multi-call dispatcher: `spoond backend|gateway|acp|mcp|runner|ctl|doctor`. Each subcommand is registered in a build-tag-gated file (`backend.go` `//go:build !nobackend`, `gateway.go` `//go:build !nogateway && linux`, `acp.go` `!noacp`, `mcp.go` `!nomcp`, `runner.go` `!norunner`, `ctl.go` `!noctl`, `doctor.go` `!nodoctor`). |
| `cmd/spoond-backend` | `spoondbackend` (library, `Main(args []string) int`) | lease API backend |
| `cmd/spoond-sshd-gateway` | `spoondgateway` (`main.go` is `//go:build linux`; `ctl_other.go` `!linux` stub) | SSH gateway plus `ctl@` control plane |
| `cmd/spoond-runner` | `spoondrunner` | Forgejo runner |
| `cmd/spoond-doctor` | `spoonddoctor` | deployment checker |
| `cmd/spoond-acp` | `spoondacp` | ACP endpoint |
| `cmd/spoond-dev-mcp` | `spoondmcp` | MCP server |
| `cmd/spoondctl` | `spoondctl` | thin CLI that runs `ssh ctl@host "<verb>"` (env `FORKD_CTL_HOST` default `sandbox.example.com`, `FORKD_CTL_PORT` 2222, `FORKD_CTL_KEY`) |
| `cmd/cfos-adapter` | `main` (standalone binary) | CFOS adapter (env `LEASE_URL` default `https://127.0.0.1:8890`, `LEASE_TOKEN`, `ADAPTER_TOKEN`, `ADAPTER_ADDR` `:8893`, `DEFAULT_IMAGE` `js-base`) |

`cmd/spoond/main.go` L1-68:

```go
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
	for _, name := range []string{"backend", "gateway", "acp", "mcp", "runner", "ctl", "doctor"} {
		if c, ok := commands[name]; ok {
			fmt.Fprintf(os.Stderr, "  %-9s %s\n", c.name, c.desc)
		}
	}
	fmt.Fprint(os.Stderr, "\nbuild tags (exclude modules): nobackend, nogateway, noacp, nomcp, norunner, noctl, nodoctor\n")
}
```

---

## 3. api/service.go (1349 lines)

### 3.1 Package doc and imports

`api/service.go` L1-23:

```go
// Package api implements the forkd ephemeral-backend lease API.
//
// A sandbox is a lease: create with an image tag + TTL, use via exec,
// release via delete or TTL expiry. Consumers never manage forkd
// snapshots, netns, or warm pools directly.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jrimmer/spoond/forkd"
	"github.com/jrimmer/spoond/identity"
	"github.com/jrimmer/spoond/metrics"
)
```

### 3.2 Lease, ShareMode, Share

**NOTE:** `Lease` has **no JSON tags except `Owner`**. It is never serialized directly: API responses are built as `map[string]any` (see `list`, `handleCreate`, and the rest). `released` is unexported.

`api/service.go` L25-65:

```go
// Lease is a sandbox granted to a consumer for a bounded lifetime.
type Lease struct {
	ID         string // unguessable lease id
	Owner      string `json:"owner"` // owner identity (user id or legacy consumer id)
	Image      string // snapshot tag
	ForkdID    string // underlying forkd sandbox id
	Address    string // guest address, e.g. "10.42.0.2:8888"
	CreatedAt  time.Time
	ExpiresAt  time.Time
	Persistent bool      // interactive/persistent lease: not TTL-swept, keep-alive extends
	LastActive time.Time // last activity (exec/stream/proxy/keepalive), for idle sweep
	Workspace  string    // workspace name when workspace-backed (suspend/resume support)
	Suspended  bool      // workspace-backed lease is currently suspended
	Name       string    // optional friendly name/tag (unique per owner; resolved by ssh/proxy)
	NetPolicy  string    // egress policy: none|lan|internet|restricted ("" = lan)
	NetAllow   []string  // allowlist for restricted policy
	// ExposePorts are guest TCP ports published on the lease's bridge-facing
	// address (netpolicy.go). ExposedIP is that address, refreshed on every
	// (re)application because a resume or restart lands in a different netns.
	ExposePorts []int
	ExposedIP   string
	Comment     string // optional free-text annotation (set/cleared via ctl comment)
	released    bool
}

// ShareMode selects which surfaces a share covers.
type ShareMode string

const (
	ShareSSH  ShareMode = "ssh"  // SSH attach / ctl access
	ShareHTTP ShareMode = "http" // proxy / exec API access
)

// Share grants a user access to a lease owned by someone else (T6/#33).
type Share struct {
	LeaseID   string    // the shared lease
	Grantee   string    // user id receiving access
	Mode      ShareMode // ssh | http
	ExpiresAt time.Time // zero = never expires
	CreatedAt time.Time
}
```

### 3.3 Store (persistence: none)

`api/service.go` L67-96:

```go
// Store holds the live leases and the warm pool.
type Store struct {
	mu     sync.Mutex
	leases map[string]*Lease
	// pool holds pre-forked forkd sandbox ids per image tag.
	pool map[string][]string
	// shares maps lease id -> grantee id -> share (T6/#33).
	shares map[string]map[string]*Share
	// pending counts in-flight lease creations per owner (T4/#31 quota
	// reservation, security review #37 H2): a slot is reserved under
	// the same lock as the quota count and released when the lease is
	// inserted or the grant fails, closing the check-then-create race.
	pending map[string]int
}

func newStore() *Store {
	return &Store{
		leases:  make(map[string]*Lease),
		pool:    make(map[string][]string),
		shares:  make(map[string]map[string]*Share),
		pending: make(map[string]int),
	}
}

// newID returns a random unguessable hex id.
func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
```

**Persistence answer:** there is no persistence of `Store`. No code in `api/` writes leases, pool, shares or pending to disk. The in-memory `pool map[string][]string` holds forkd sandbox ids per image tag. After a restart, `ReconcileOrphans` treats every controller sandbox as an orphan (§3.9).

For comparison, the identity store (the only persisted state), in `identity/store.go`:

`identity/store.go` L1-11:

```go
// Package identity implements the spoond user/identity store (epic #26 T1).
//
// A User is a person or an agent with its own SSH key fingerprints and
// API token hash. The first user registered becomes the admin (KTD-2:
// "first user is admin"). The store is the single source of truth for
// key→user and token→user resolution; the gateway and the API both ask it.
//
// Persistence: the store is written to a JSON file on every mutation
// (atomic write via temp+rename). A nil filename keeps the store
// in-memory only (tests).
package identity
```

`identity/store.go` L38-72:

```go
// User is a first-class identity: a person or an agent.
type User struct {
	ID           string   `json:"id"`           // stable user id (e.g. "u-<hex>")
	Name         string   `json:"name"`         // display/ctl name, unique
	Kind         Kind     `json:"kind"`         // person | agent
	Admin        bool     `json:"admin"`        // KTD-2: first user is admin
	Fingerprints []string `json:"fingerprints"` // SHA256 fingerprints of SSH keys
	TokenHash    string   `json:"token_hash"`   // SHA256 of the bearer token ("" = none)
	// LLMKeyHash is the SHA256 of the user's LLM gateway key (U8/T8,
	// "" = none). When set, /llm/ requests on this user's leases must
	// present the matching key; the API layer never exposes it.
	LLMKeyHash string `json:"llm_key_hash"` // SHA256 of the LLM gateway key ("" = none)
	CreatedAt  string `json:"created_at"`   // RFC3339
	// Quota (T4/#31). 0 = no per-user cap (global defaults apply).
	MaxLeases int `json:"max_leases"` // max concurrent leases (0 = unlimited)
	MaxTTL    int `json:"max_ttl"`    // max lease TTL seconds (0 = global max applies)
}

// Store is a thread-safe user registry with optional JSON persistence.
type Store struct {
	mu      sync.RWMutex
	users   map[string]*User  // by id
	byFP    map[string]string // fingerprint -> user id
	byToken map[string]string // token hash -> user id
	byName  map[string]string // name -> user id
	file    string
	// salt (security review #37 M1) makes stored token/LLM-key hashes
	// HMAC-SHA256(salt, secret) instead of unsalted SHA256, defeating
	// offline dictionary attacks if the users file leaks. It is loaded
	// from <file>.salt (or generated for a fresh store); a pre-existing
	// store without a sidecar stays in legacy plain-SHA256 mode so old
	// hashes keep verifying. The salt never changes once chosen.
	salt []byte
}

```

`save()` (L170-188) does `json.MarshalIndent(users, "", "  ")`, then `os.WriteFile(tmp, data, 0o600)`, then `os.Rename(tmp, s.file)`. The path comes from the backend env `USERS_FILE` (§8). `<file>.salt` holds the HMAC salt (L99-125).

### 3.4 ForkdClient interface

`api/service.go` L98-114:

```go
// ForkdClient is the subset of the forkd controller API the lease
// service needs. *forkd.Client satisfies it; tests use a fake.
type ForkdClient interface {
	ListSnapshots(ctx context.Context) ([]forkd.SnapshotInfo, error)
	SnapshotExists(ctx context.Context, tag string) (bool, error)
	Spawn(ctx context.Context, tag string, n int, perChildNetns bool, memoryLimitMiB int) ([]forkd.SandboxInfo, error)
	ListSandboxes(ctx context.Context) ([]forkd.SandboxInfo, error)
	Kill(ctx context.Context, id string) error
	Exec(ctx context.Context, id string, args []string, timeoutSecs int) (*forkd.ExecResult, error)
	Ping(ctx context.Context, id string) error
	Branch(ctx context.Context, id, tag string) (string, error)
	CreateWorkspace(ctx context.Context, name, tag string, perChildNetns bool) (*forkd.WorkspaceInfo, error)
	SuspendWorkspace(ctx context.Context, name string) error
	ResumeWorkspace(ctx context.Context, name string) (*forkd.WorkspaceInfo, error)
	DeleteWorkspace(ctx context.Context, name string) error
	Metrics(ctx context.Context) ([]byte, error)
}
```

### 3.5 Service struct and setters

`api/service.go` L116-207:

```go
// Service is the lease API backend.
type Service struct {
	forkd ForkdClient
	store *Store
	// tokens maps a consumer token to its consumer id (legacy mode).
	tokens map[string]string
	// identities is the user/identity store (epic #26 T1). When set,
	// bearer-token auth resolves against it first; tokens map remains as
	// the backward-compatible fallback for single-user deployments.
	identities *identity.Store
	// gatewayToken is the SSH gateway's service token (U6/T5). When a
	// request authenticates with it, the X-Spoond-User-Id header is
	// honored so the gateway can act as the SSH-authenticated user; the
	// backend's owner-scoping then applies to that user, not the gateway
	// service identity. Empty disables impersonation.
	gatewayToken string
	// poolSize is the warm-pool size per image.
	poolSize int
	// probeEnabled runs integrityProbe inside each sandbox before it is
	// pooled or handed to a lease. probeTimeout bounds that exec.
	probeEnabled bool
	probeTimeout time.Duration
	// defaultTTL is used when a request omits ttl.
	defaultTTL time.Duration
	// maxTTL caps a requested ttl.
	maxTTL time.Duration
	// sweepInterval is the TTL-sweeper tick (overridable in tests).
	sweepInterval time.Duration
	// idleTimeout is how long a persistent lease may sit idle before the
	// sweeper reclaims it (auto-suspend). 0 disables idle sweeping.
	idleTimeout time.Duration
	log         *log.Logger
	// netpol applies egress policy to a lease's child netns. Nil means
	// policy enforcement is disabled (tests, or deployments without
	// root ip netns access).
	netpol PolicyApplier
	// netpolDNS is the resolver set allowed under PolicyRestricted.
	netpolDNS []string
	// metrics (issue #20): service-level Prometheus metrics.
	metrics *metrics.BackendMetrics
}

// SetNetpol installs the egress-policy applier and the DNS resolvers
// allowed under the restricted policy. Call before serving.
// SetMetrics installs the Prometheus metrics collector (issue #20).
// Called by the Server after NewServerWithLLM so the service can
// record pool, lease, and netpol events.
func (s *Service) SetMetrics(m *metrics.BackendMetrics) {
	s.metrics = m
}

func (s *Service) SetNetpol(a PolicyApplier, dns []string) {
	s.netpol = a
	s.netpolDNS = dns
}

// SetGatewayToken marks the SSH gateway's service token, enabling
// trusted impersonation (U6/T5): requests carrying this token may set
// X-Spoond-User-Id to act as the SSH-authenticated user.
func (s *Service) SetGatewayToken(tok string) {
	s.gatewayToken = tok
}

// SetSandboxProbe configures the per-spawn integrity probe. enabled=false
// turns it off (every sandbox is then handed out unverified); timeout<=0
// leaves the default in place.
func (s *Service) SetSandboxProbe(enabled bool, timeout time.Duration) {
	s.probeEnabled = enabled
	if timeout > 0 {
		s.probeTimeout = timeout
	}
}

// SetIdentities installs the identity store used for token→user and
// key→user resolution. Call before serving; when set, the first user in
// the store is the admin (KTD-2) and legacy consumer tokens still work.
func (s *Service) SetIdentities(ids *identity.Store) {
	s.identities = ids
}

// ResolveOwner resolves a bearer token to an owner identity. It prefers
// the identity store (user id) and falls back to the legacy consumer
// token map for single-user deployments.
func (s *Service) ResolveOwner(token string) (string, bool) {
	if s.identities != nil {
		if u := s.identities.UserByToken(token); u != nil {
			return u.ID, true
		}
	}
	owner, ok := s.tokens[token]
	return owner, ok
}
```

### 3.6 Method inventory

| Method (receiver `*Service`) | Line | What it does | ForkdClient calls |
|---|---|---|---|
| `SetMetrics(m *metrics.BackendMetrics)` | 163 | install metrics | none |
| `SetNetpol(a PolicyApplier, dns []string)` | 167 | install the egress policy applier plus DNS allowlist | none |
| `SetGatewayToken(tok string)` | 175 | enable gateway impersonation | none |
| `SetSandboxProbe(enabled bool, timeout time.Duration)` | 182 | configure the integrity probe | none |
| `SetIdentities(ids *identity.Store)` | 192 | install the identity store | none |
| `ResolveOwner(token string) (string, bool)` | 199 | bearer token → owner id (identity store first, then legacy `tokens` map) | none |
| `applyNetpol(ctx, l *Lease) error` | 212 | apply the iptables egress policy plus the expose DNAT inside the lease's netns; sets `l.ExposedIP` | `ListSandboxes` (via `resolveEndpoint`) |
| `CanExposePorts() bool` | 256 | is `netpol` a `PortExposer` | none |
| `exposedMap(l *Lease) map[string]string` (func) | 262 | `{"<port>":"<ip>:<port>"}` | none |
| `NewService(fc ForkdClient, tokens map[string]string, poolSize int, defaultTTL, maxTTL time.Duration, knownImages ...string) *Service` (func) | 279 | constructor (idle 0) | none |
| `NewServiceWithIdle(fc ForkdClient, tokens map[string]string, poolSize int, defaultTTL, maxTTL, idleTimeout time.Duration, knownImages ...string) *Service` (func) | 285 | constructor; seeds `pool[img]=nil` for known images | none |
| `Start(ctx)` | 314 | goroutine ticker (`sweepInterval` 5s): `sweepExpired` then `refillPool` | indirect |
| `refillPool(ctx)` | 332 | `warmPool` for every image key in `pool` | indirect |
| `sweepExpired(ctx)` | 354 | release TTL-expired non-persistent leases; idle-suspend (max 3 per tick, 500ms apart) workspace leases; release idle non-workspace persistent leases | `SuspendWorkspace`, and via `release` |
| `touch(id string)` | 407 | bump `LastActive` | none |
| `keepAlive(owner, id string, ttl time.Duration) (*Lease, error)` | 417 | extend a persistent lease's `ExpiresAt` (capped at maxTTL) | none |
| `release(ctx, l *Lease)` | 442 | delete the workspace, or kill the sandbox; remove the lease on success or on "not found" | `DeleteWorkspace` or `Kill` |
| `reserveQuota(owner string) error` | 501 | atomic per-user lease cap check plus pending reservation | none |
| `releaseQuotaReservation(owner string)` | 528 | drop the reservation | none |
| `grant(ctx, owner, image string, memoryMiB int, ttl time.Duration, persistent bool, netPolicy string, netAllow []string, exposePorts ...int) (*Lease, error)` | 541 | create a lease: persistent → workspace; else warm pool (ping plus probe) or cold spawn; then netpol | `CreateWorkspace`, `ListSandboxes`, `Exec`(probe), `DeleteWorkspace`, `Ping`, `Kill`, `Spawn` |
| `fillEndpoint(ctx, lease *Lease) error` | 668 | set `lease.Address` from the controller list | `ListSandboxes` |
| `suspend(ctx, owner, id string) (*Lease, error)` | 687 | suspend a workspace lease | `SuspendWorkspace` |
| `resume(ctx, owner, id string) (*Lease, error)` | 715 | resume a workspace; new ForkdID; fill endpoint; netpol | `ResumeWorkspace`, `ListSandboxes` |
| `restart(ctx, owner, id string) (*Lease, error)` | 754 | workspace: suspend (if running) then resume; plain persistent: kill plus cold spawn | `SuspendWorkspace`, `ResumeWorkspace`, `Kill`, `Spawn`, `ListSandboxes` |
| `setName(owner, id, name string) (*Lease, error)` | 806 | friendly name `[a-z0-9][a-z0-9-]*` ≤63, unique per owner | none |
| `setComment(owner, id, comment string) (*Lease, error)` | 834 | comment ≤512 chars | none |
| `lookupByName(name string) *Lease` | 852 | any owner, by name | none |
| `lookupByNameForOwner(owner, name string) *Lease` | 866 | owner-scoped by name | none |
| `lookupUserScoped(owner, label string) *Lease` | 881 | owner-scoped by id or name (proxy forward-auth) | none |
| `GrantShare(owner, leaseID, grantee string, mode ShareMode, ttl time.Duration) error` | 898 | add or replace a share | none |
| `RevokeShare(owner, leaseID, grantee string) error` | 932 | remove a share | none |
| `ListShares(owner string) []*Share` | 950 | shares on the owner's leases, newest first | none |
| `lookupWithShare(caller, leaseID string, mode ShareMode) *Lease` | 971 | owner or valid share of that mode | none |
| `grantFromSnapshot(ctx, owner, tag string, ttl time.Duration, persistent bool) (*Lease, error)` | 995 | the clone path: lease from a branch tag, bypassing the pool | `CreateWorkspace`, `ListSandboxes`, `Exec`, `DeleteWorkspace`, `Spawn` |
| `lookup(owner, id string) *Lease` | 1050 | owner-scoped by id | none |
| `lookupAny(id string) *Lease` | 1063 | any owner, by id | none |
| `resolveEndpoint(ctx, l *Lease) (*Endpoint, error)` | 1084 | netns plus guest addr from the controller | `ListSandboxes` |
| `list(owner string) []map[string]any` | 1107 | the `GET /api/sandboxes` rows | none |
| `probeSandbox(ctx, id string) error` | 1160 | run `integrityProbe` via exec | `Exec` |
| `warmPool(ctx, image string)` | 1179 | spawn one at a time up to poolSize; probe each | `Spawn`, `Exec`, `Kill` |
| `LiveLeases() []string` | 1221 | ids of unreleased leases | none |
| `Shutdown(ctx)` | 1237 | release all leases, kill pooled ids | via `release`, `Kill` |
| `CollectMetrics(m *metrics.BackendMetrics)` | 1268 | set gauges pool_ready/pool_cap/leases_active/quota_reservations/shares_active | none |
| `ReconcileOrphans(ctx)` | 1305 | kill controller sandboxes not in leases or pool | `ListSandboxes`, `Kill` |

Unused `ForkdClient` methods: `ListSnapshots` and `SnapshotExists` are used only by `ImageRegistry` (§5.10). `Branch` is used only by `handleClone`. `Metrics` is used only by `handleMetrics`. `Exec` is also called directly by `handleExec`, `handlePrompt` and `handleStat`.

### 3.7 applyNetpol, CanExposePorts, exposedMap

`api/service.go` L209-271:

```go
// applyNetpol enforces a lease's egress policy inside its child netns.
// It is called after grant (fresh sandbox), after resume (new sandbox),
// and after restart. A nil applier disables enforcement.
func (s *Service) applyNetpol(ctx context.Context, l *Lease) error {
	if s.netpol == nil {
		return nil
	}
	if l.NetPolicy == "" {
		l.NetPolicy = string(PolicyLAN)
	}
	// Always apply rules — the netns pool reuses network namespaces, so a
	// previous lease may have left stale FORWARD rules (e.g. restricted).
	// policyCommands flushes FORWARD first, making this idempotent.
	ep, err := s.resolveEndpoint(ctx, l)
	if err != nil {
		return fmt.Errorf("resolve endpoint for policy: %w", err)
	}
	allow := l.NetAllow
	if allow == nil {
		allow = []string{}
	}
	if err := s.netpol.Apply(ctx, ep.Netns, NetworkPolicy(l.NetPolicy), allow); err != nil {
		return err
	}
	// Exposure runs on EVERY application, empty or not: it flushes the
	// netns's DNAT rules, which a reused pool netns may still carry from its
	// previous tenant.
	exposer, ok := s.netpol.(PortExposer)
	if !ok {
		if len(l.ExposePorts) > 0 {
			return fmt.Errorf("port exposure is not supported by this policy applier")
		}
		return nil
	}
	ip, err := exposer.Expose(ctx, ep.Netns, ep.GuestHost, l.ExposePorts)
	if err != nil {
		return err
	}
	l.ExposedIP = ""
	if len(l.ExposePorts) > 0 {
		l.ExposedIP = ip
	}
	return nil
}

// CanExposePorts reports whether this service can publish guest ports —
// only when network policy enforcement (and so netns access) is installed.
func (s *Service) CanExposePorts() bool {
	_, ok := s.netpol.(PortExposer)
	return ok
}

// exposedMap renders a lease's published ports as {"<port>": "<ip>:<port>"}.
func exposedMap(l *Lease) map[string]string {
	out := map[string]string{}
	if l.ExposedIP == "" {
		return out
	}
	for _, p := range l.ExposePorts {
		out[fmt.Sprint(p)] = net.JoinHostPort(l.ExposedIP, fmt.Sprint(p))
	}
	return out
}
```

### 3.8 Constructors, Start, refillPool, sweepExpired, touch, keepAlive, release

`api/service.go` L273-484:

```go
// NewService builds a lease service. tokens maps consumer tokens to
// consumer ids. poolSize is the warm-pool size per image (0 disables
// pre-forking). defaultTTL and maxTTL bound lease lifetimes. knownImages
// seeds the warm-pool map so refillPool pre-forks every image at
// startup — without this, an image only becomes warm after its first
// grant, leaving the pool cold after a backend restart.
func NewService(fc ForkdClient, tokens map[string]string, poolSize int, defaultTTL, maxTTL time.Duration, knownImages ...string) *Service {
	return NewServiceWithIdle(fc, tokens, poolSize, defaultTTL, maxTTL, 0, knownImages...)
}

// NewServiceWithIdle is NewService plus an idle auto-suspend timeout for
// persistent leases. idleTimeout 0 disables idle sweeping.
func NewServiceWithIdle(fc ForkdClient, tokens map[string]string, poolSize int, defaultTTL, maxTTL, idleTimeout time.Duration, knownImages ...string) *Service {
	s := &Service{
		forkd:         fc,
		store:         newStore(),
		tokens:        tokens,
		poolSize:      poolSize,
		defaultTTL:    defaultTTL,
		maxTTL:        maxTTL,
		idleTimeout:   idleTimeout,
		sweepInterval: 5 * time.Second,
		log:           log.Default(),
		probeEnabled:  true,
		probeTimeout:  20 * time.Second,
	}
	for _, img := range knownImages {
		if img == "" {
			continue
		}
		s.store.mu.Lock()
		if _, ok := s.store.pool[img]; !ok {
			s.store.pool[img] = nil
		}
		s.store.mu.Unlock()
	}
	return s
}

// Start begins the TTL sweeper and warm-pool refill. It runs until ctx
// is cancelled.
func (s *Service) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(s.sweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.sweepExpired(ctx)
				s.refillPool(ctx)
			}
		}
	}()
}

// refillPool pre-forks poolSize sandboxes for each known image so
// grants can be served from the warm pool instead of cold-spawning.
func (s *Service) refillPool(ctx context.Context) {
	if s.poolSize <= 0 {
		return
	}
	s.store.mu.Lock()
	images := make([]string, 0, len(s.store.pool))
	for img := range s.store.pool {
		images = append(images, img)
	}
	s.store.mu.Unlock()
	for _, img := range images {
		s.warmPool(ctx, img)
	}
}

// sweepExpired kills and removes leases whose TTL has passed. Persistent
// leases are not TTL-swept (the consumer keeps them alive via keep-alive,
// and disposes via delete), but when idleTimeout is set they are
// auto-suspended after that long without activity (exec/stream/proxy/
// keep-alive all bump LastActive). Workspace-backed leases are suspended
// (state snapshot kept, cheap to resume); plain persistent leases are
// released as before.
func (s *Service) sweepExpired(ctx context.Context) {
	s.store.mu.Lock()
	var expired []*Lease
	var idleSuspend []*Lease
	now := time.Now()
	for _, l := range s.store.leases {
		if l.released {
			continue
		}
		if !l.Persistent && now.After(l.ExpiresAt) {
			expired = append(expired, l)
			continue
		}
		if l.Persistent && s.idleTimeout > 0 && now.After(l.LastActive.Add(s.idleTimeout)) {
			if l.Workspace != "" && !l.Suspended {
				s.log.Printf("idle sweep: suspending persistent lease %s (idle since %s)", l.ID, l.LastActive.Format(time.RFC3339))
				idleSuspend = append(idleSuspend, l)
			} else if l.Workspace == "" {
				s.log.Printf("idle sweep: reclaiming persistent lease %s (idle since %s)", l.ID, l.LastActive.Format(time.RFC3339))
				expired = append(expired, l)
			}
		}
	}
	s.store.mu.Unlock()
	// Suspend idle workspace leases in small, staggered batches. Each
	// suspend is a controller snapshot write that briefly blocks the
	// controller's accept loop; a large backlog (e.g. after a long test
	// session) must not produce one big pause that drops incoming
	// connections. Cap per tick and space them out — with the 5s sweep
	// tick, 13 idle leases clear in ~25s instead of a single burst.
	const maxSuspendPerTick = 3
	suspended := 0
	for _, l := range idleSuspend {
		if suspended >= maxSuspendPerTick {
			break
		}
		if err := s.forkd.SuspendWorkspace(ctx, l.Workspace); err != nil {
			s.log.Printf("idle sweep: suspend %s: %v", l.ID, err)
			continue
		}
		s.store.mu.Lock()
		l.Suspended = true
		s.store.mu.Unlock()
		suspended++
		time.Sleep(500 * time.Millisecond)
	}
	for _, l := range expired {
		s.release(ctx, l)
	}
}

// touch records activity on a lease so the idle sweeper doesn't reclaim
// it. Returns nil for unknown/released leases.
func (s *Service) touch(id string) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if l := s.store.leases[id]; l != nil && !l.released {
		l.LastActive = time.Now()
	}
}

// keepAlive extends a persistent lease's expiry so the sweeper never
// reclaims it. Non-persistent leases are rejected (their TTL is fixed).
func (s *Service) keepAlive(owner, id string, ttl time.Duration) (*Lease, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l, ok := s.store.leases[id]
	if !ok || l.Owner != owner || l.released {
		return nil, errNotFound
	}
	if !l.Persistent {
		return nil, errNotPersistent
	}
	if ttl <= 0 {
		ttl = s.maxTTL
	}
	if ttl > s.maxTTL {
		ttl = s.maxTTL
	}
	l.ExpiresAt = time.Now().Add(ttl)
	l.LastActive = time.Now() // keep-alive is activity
	return l, nil
}

// release kills the underlying forkd sandbox and removes the lease.
// The lease is only removed from the store after a successful kill, so
// a transient Kill failure leaves it in place for the sweeper to retry
// rather than leaking the sandbox.
func (s *Service) release(ctx context.Context, l *Lease) {
	s.store.mu.Lock()
	if l.released {
		s.store.mu.Unlock()
		return
	}
	l.released = true
	s.store.mu.Unlock()

	// Workspace-backed leases: delete the workspace (kills the live
	// sandbox if any AND removes the state snapshot). Plain leases: kill
	// the sandbox.
	var err error
	if l.Workspace != "" {
		err = s.forkd.DeleteWorkspace(ctx, l.Workspace)
		if err != nil && strings.Contains(err.Error(), "not found") {
			err = nil // workspace already gone
		}
	} else {
		err = s.forkd.Kill(ctx, l.ForkdID)
	}
	if err != nil {
		// A 404 means the sandbox is already gone (e.g. the controller
		// restarted and forgot it) — that's the goal state, not a
		// failure. Treat it as released so we don't retry forever.
		if strings.Contains(err.Error(), "not found") {
			s.log.Printf("release: kill %s: already gone (removing lease)", l.ForkdID)
			s.store.mu.Lock()
			delete(s.store.leases, l.ID)
			s.store.mu.Unlock()
			return
		}
		s.log.Printf("release: kill %s failed: %v (will retry)", l.ForkdID, err)
		// Re-open the lease so the sweeper retries the kill.
		s.store.mu.Lock()
		l.released = false
		s.store.mu.Unlock()
		return
	}
	s.store.mu.Lock()
	delete(s.store.leases, l.ID)
	s.store.mu.Unlock()
}
```

### 3.9 Quota, grant, fillEndpoint

`api/service.go` L486-683:

```go
// grant creates a new lease for owner from the warm pool (or spawns a
// fresh sandbox when the pool is empty). Persistent leases are intended
// for interactive use: they are not TTL-swept (see keepAlive) and the
// consumer drives their lifecycle.
// errQuotaExceeded is returned when a user hits their concurrent-lease
// cap (T4/#31). The API layer maps it to HTTP 429.
var errQuotaExceeded = fmt.Errorf("lease quota exceeded")

// reserveQuota enforces a user's concurrent-lease cap before granting
// and RESERVES a slot atomically (security review #37 H2): the count
// and the reservation happen under the same store lock, so concurrent
// creates cannot both pass max_leases. The caller MUST call
// releaseQuotaReservation when the grant finishes (success or failure).
// Returns errQuotaExceeded when the cap is hit. Owners without an
// identity-store user (legacy consumer tokens) are uncapped.
func (s *Service) reserveQuota(owner string) error {
	if s.identities == nil {
		return nil
	}
	u := s.identities.UserByID(owner)
	if u == nil || u.MaxLeases <= 0 {
		return nil
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	active := 0
	for _, l := range s.store.leases {
		if !l.released && l.Owner == owner {
			active++
		}
	}
	if active+s.store.pending[owner] >= u.MaxLeases {
		if s.metrics != nil {
			s.metrics.QuotaExceeded.Inc()
		}
		return errQuotaExceeded
	}
	s.store.pending[owner]++
	return nil
}

// releaseQuotaReservation drops a reservation made by reserveQuota.
func (s *Service) releaseQuotaReservation(owner string) {
	if s.identities == nil {
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if s.store.pending[owner] <= 1 {
		delete(s.store.pending, owner)
	} else {
		s.store.pending[owner]--
	}
}

func (s *Service) grant(ctx context.Context, owner, image string, memoryMiB int, ttl time.Duration, persistent bool, netPolicy string, netAllow []string, exposePorts ...int) (*Lease, error) {
	if err := s.reserveQuota(owner); err != nil {
		return nil, err
	}
	// The reservation becomes the real lease when it's stored below;
	// the deferred release runs on BOTH success and failure: on failure
	// it frees the slot, on success the lease is already counted as
	// active so the pending reservation must be dropped (security
	// review #37 H2). Both mutations take the same store lock, so a
	// concurrent reserveQuota sees a consistent active+pending count.
	defer func() { s.releaseQuotaReservation(owner) }()
	grantStart := time.Now()
	if s.metrics != nil {
		s.metrics.LeasesTotal.Inc()
		s.metrics.LeaseGrantDur.Observe(time.Since(grantStart).Seconds())
	}
	lease := &Lease{
		ID:          newID(),
		Owner:       owner,
		Image:       image,
		CreatedAt:   time.Now(),
		ExpiresAt:   time.Now().Add(ttl),
		Persistent:  persistent,
		LastActive:  time.Now(),
		NetPolicy:   netPolicy,
		NetAllow:    netAllow,
		ExposePorts: exposePorts,
	}

	// Persistent leases are workspace-backed so they can suspend/resume
	// (the workspace keeps a state snapshot while stopped). TTL leases
	// use the warm pool for fast grants.
	if persistent {
		ws, err := s.forkd.CreateWorkspace(ctx, "ws-"+lease.ID, image, true)
		if err != nil {
			return nil, fmt.Errorf("create workspace: %w", err)
		}
		lease.Workspace = ws.Name
		lease.ForkdID = ws.LiveSandboxID
		if err := s.fillEndpoint(ctx, lease); err != nil {
			return nil, fmt.Errorf("resolve workspace sandbox: %w", err)
		}
		if err := s.probeSandbox(ctx, lease.ForkdID); err != nil {
			_ = s.forkd.DeleteWorkspace(ctx, ws.Name)
			return nil, fmt.Errorf("workspace sandbox failed the integrity probe: %w", err)
		}
		if err := s.applyNetpol(ctx, lease); err != nil {
			return nil, fmt.Errorf("apply network policy: %w", err)
		}
		s.store.mu.Lock()
		s.store.leases[lease.ID] = lease
		s.store.mu.Unlock()
		return lease, nil
	}

	// Try the warm pool first, validating that the pooled sandbox still
	// exists in the controller. After a controller restart the backend's
	// in-memory pool holds stale IDs (the controller forgot them); a
	// stale ID would 404 on exec and leave the consumer hanging.
	var forkdID, addr string
	for {
		s.store.mu.Lock()
		pool := s.store.pool[image]
		if len(pool) > 0 {
			forkdID = pool[len(pool)-1]
			s.store.pool[image] = pool[:len(pool)-1]
		}
		// Ensure the image is registered so the warm-pool refill knows to
		// pre-fork it.
		if _, known := s.store.pool[image]; !known {
			s.store.pool[image] = nil
		}
		s.store.mu.Unlock()

		if forkdID == "" {
			break
		}
		// Verify the pooled sandbox is still alive; if not, drop it and
		// try the next one (or cold-spawn below).
		if err := s.forkd.Ping(ctx, forkdID); err != nil {
			s.log.Printf("grant: pooled %s (%s) is stale (controller forgot it), dropping", forkdID, image)
			_ = s.forkd.Kill(ctx, forkdID)
			forkdID = ""
			continue
		}
		// Alive is not the same as sane. A pooled sandbox from a bad image
		// generation answers a ping and then fails the job 48s in, so
		// recycle it here and let the pool refill.
		if err := s.probeSandbox(ctx, forkdID); err != nil {
			s.log.Printf("grant: pooled %s (%s) failed the integrity probe, recycling: %v", forkdID, image, err)
			_ = s.forkd.Kill(ctx, forkdID)
			forkdID = ""
			continue
		}
		addr = ""
		break
	}
	if forkdID == "" {
		sbs, err := s.forkd.Spawn(ctx, image, 1, true, memoryMiB)
		if err != nil {
			return nil, err
		}
		if len(sbs) == 0 {
			return nil, errNoSandbox
		}
		forkdID = sbs[0].ID
		addr = sbs[0].GuestAddr
		if err := s.probeSandbox(ctx, forkdID); err != nil {
			_ = s.forkd.Kill(ctx, forkdID)
			return nil, fmt.Errorf("spawned sandbox failed the integrity probe: %w", err)
		}
	}

	lease.ForkdID = forkdID
	lease.Address = addr
	if err := s.applyNetpol(ctx, lease); err != nil {
		return nil, fmt.Errorf("apply network policy: %w", err)
	}
	s.store.mu.Lock()
	s.store.leases[lease.ID] = lease
	s.store.mu.Unlock()
	return lease, nil
}

// fillEndpoint looks up a lease's sandbox in the controller and fills in
// the Address (guest addr). Used after workspace create/resume where the
// controller response does not carry the endpoint.
func (s *Service) fillEndpoint(ctx context.Context, lease *Lease) error {
	if lease.ForkdID == "" {
		return fmt.Errorf("no sandbox id")
	}
	sbs, err := s.forkd.ListSandboxes(ctx)
	if err != nil {
		return err
	}
	for _, sb := range sbs {
		if sb.ID == lease.ForkdID {
			lease.Address = sb.GuestAddr
			return nil
		}
	}
	return fmt.Errorf("sandbox %s not in controller list", lease.ForkdID)
}
```

**NOTE (grant):**
- The persistent path always calls `CreateWorkspace(ctx, "ws-"+lease.ID, image, true)` and ignores `memoryMiB`.
- On the pooled path, `addr` is set to `""`, so `lease.Address` stays empty for pool-served leases. `Address` is set only for cold spawns and for workspaces (via `fillEndpoint`).
- The `LeaseGrantDur` observation happens at the start of `grant`, so it measures about 0.
- The `metrics.LeasesTotal` increment also happens before success.
- If `applyNetpol` fails after a successful spawn, the sandbox is **not** killed (it leaks until `ReconcileOrphans` at the next restart).

### 3.10 suspend, resume, restart

`api/service.go` L685-801:

```go
// suspend suspends a workspace-backed persistent lease: the controller
// snapshots the sandbox and stops it. The lease stays; resume restores.
func (s *Service) suspend(ctx context.Context, owner, id string) (*Lease, error) {
	s.store.mu.Lock()
	l := s.store.leases[id]
	if l == nil || l.Owner != owner || l.released {
		s.store.mu.Unlock()
		return nil, errNotFound
	}
	if !l.Persistent {
		s.store.mu.Unlock()
		return nil, errNotPersistent
	}
	if l.Workspace == "" {
		s.store.mu.Unlock()
		return nil, errNotPersistent // not workspace-backed (older lease)
	}
	s.store.mu.Unlock()

	if err := s.forkd.SuspendWorkspace(ctx, l.Workspace); err != nil {
		return nil, err
	}
	s.store.mu.Lock()
	l.Suspended = true
	s.store.mu.Unlock()
	return l, nil
}

// resume restores a suspended workspace-backed lease and refreshes the
// lease's sandbox id (resume spawns a fresh sandbox).
func (s *Service) resume(ctx context.Context, owner, id string) (*Lease, error) {
	s.store.mu.Lock()
	l := s.store.leases[id]
	if l == nil || l.Owner != owner || l.released {
		s.store.mu.Unlock()
		return nil, errNotFound
	}
	if !l.Persistent {
		s.store.mu.Unlock()
		return nil, errNotPersistent
	}
	if l.Workspace == "" {
		s.store.mu.Unlock()
		return nil, errNotPersistent
	}
	s.store.mu.Unlock()

	ws, err := s.forkd.ResumeWorkspace(ctx, l.Workspace)
	if err != nil {
		return nil, err
	}
	s.store.mu.Lock()
	l.ForkdID = ws.LiveSandboxID
	l.LastActive = time.Now()
	l.Suspended = false
	s.store.mu.Unlock()
	if err := s.fillEndpoint(ctx, l); err != nil {
		return nil, err
	}
	if err := s.applyNetpol(ctx, l); err != nil {
		return nil, err
	}
	return l, nil
}

// restart reboots a workspace-backed persistent lease: suspend (snapshot
// + stop) then resume (fresh sandbox from the state snapshot). Idempotent
// for suspended leases (resume alone). Plain leases get a kill + cold
// spawn; non-workspace persistent leases return notPersistent.
func (s *Service) restart(ctx context.Context, owner, id string) (*Lease, error) {
	s.store.mu.Lock()
	l := s.store.leases[id]
	if l == nil || l.Owner != owner || l.released {
		s.store.mu.Unlock()
		return nil, errNotFound
	}
	if !l.Persistent {
		s.store.mu.Unlock()
		return nil, errNotPersistent
	}
	workspace := l.Workspace
	suspended := l.Suspended
	s.store.mu.Unlock()

	if workspace != "" {
		if !suspended {
			if err := s.forkd.SuspendWorkspace(ctx, workspace); err != nil {
				return nil, err
			}
		}
		return s.resume(ctx, owner, id)
	}
	// Plain persistent lease: kill the sandbox, then cold-spawn the image
	// and re-grant the lease on the fresh sandbox.
	if err := s.forkd.Kill(ctx, l.ForkdID); err != nil {
		return nil, err
	}
	sbs, err := s.forkd.Spawn(ctx, l.Image, 1, false, 0)
	if err != nil {
		return nil, err
	}
	if len(sbs) == 0 {
		return nil, fmt.Errorf("spawn returned no sandboxes")
	}
	s.store.mu.Lock()
	l.ForkdID = sbs[0].ID
	l.Suspended = false
	l.LastActive = time.Now()
	s.store.mu.Unlock()
	if err := s.fillEndpoint(ctx, l); err != nil {
		return nil, err
	}
	if err := s.applyNetpol(ctx, l); err != nil {
		return nil, err
	}
	return l, nil
}
```

**NOTE:** `restart` on a plain persistent lease calls `Spawn(ctx, l.Image, 1, false, 0)` with `perChildNetns=false`. Every other spawn passes `true`. **AMBIGUOUS** whether that was intended. Also, since every persistent lease created by `grant` or `grantFromSnapshot` is workspace-backed, the plain-persistent branch is only reachable for "older" leases.

### 3.11 Names, comments, lookups

`api/service.go` L803-893:

```go
// setName assigns a friendly name to a lease. Names must be non-empty,
// at most 63 chars, [a-z0-9][a-z0-9-]* (lowercase, hyphenable, no dots —
// dots belong to the proxy hostname), and unique per owner.
func (s *Service) setName(owner, id, name string) (*Lease, error) {
	if name == "" || len(name) > 63 {
		return nil, fmt.Errorf("name must be 1-63 chars")
	}
	for i, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' && i > 0
		if !ok {
			return nil, fmt.Errorf("name must match [a-z0-9][a-z0-9-]*")
		}
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l := s.store.leases[id]
	if l == nil || l.Owner != owner || l.released {
		return nil, errNotFound
	}
	for _, other := range s.store.leases {
		if other != l && other.Owner == owner && !other.released && other.Name == name {
			return nil, fmt.Errorf("name %q already in use by lease %s", name, other.ID)
		}
	}
	l.Name = name
	return l, nil
}

// setComment sets or clears the free-text annotation on a lease.
// Empty string clears it; comments are informational only (no
// uniqueness constraint, unlike names).
func (s *Service) setComment(owner, id, comment string) (*Lease, error) {
	if len(comment) > 512 {
		return nil, fmt.Errorf("comment must be <= 512 chars")
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l := s.store.leases[id]
	if l == nil || l.Owner != owner || l.released {
		return nil, errNotFound
	}
	l.Comment = comment
	return l, nil
}

// lookupByName returns a live lease with the given name regardless of
// owner. Used by the SSH gateway (username = name) and the public proxy
// (<name>.sandbox.example.com); both treat the name as the capability, the
// same model as lease ids. Names are unique per owner.
func (s *Service) lookupByName(name string) *Lease {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	for _, l := range s.store.leases {
		if !l.released && l.Name == name {
			return l
		}
	}
	return nil
}

// lookupByNameForOwner returns a live lease with the given name owned by
// the given owner (names are unique per owner). Used by the API's
// /api/names endpoint so a caller can only resolve their own names.
func (s *Service) lookupByNameForOwner(owner, name string) *Lease {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	for _, l := range s.store.leases {
		if !l.released && l.Owner == owner && l.Name == name {
			return l
		}
	}
	return nil
}

// lookupUserScoped resolves a proxy hostname label (hex lease id or
// friendly name) to a lease owned by the given user. Used by the HTTP
// proxy under forward-auth (U7/T7): the proxy never resolves another
// owner's leases by id or name.
func (s *Service) lookupUserScoped(owner, label string) *Lease {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	for _, l := range s.store.leases {
		if l.released || l.Owner != owner {
			continue
		}
		if l.ID == label || l.Name == label {
			return l
		}
	}
	return nil
}
```

### 3.12 Shares

`api/service.go` L895-989:

```go
// GrantShare shares a lease with another user (T6/#33). Only the owner
// can grant; an existing share is replaced. mode selects the surface
// (ssh | http); ttl limits the share lifetime (0 = never expires).
func (s *Service) GrantShare(owner, leaseID, grantee string, mode ShareMode, ttl time.Duration) error {
	l := s.lookup(owner, leaseID)
	if l == nil {
		return fmt.Errorf("sandbox not found")
	}
	if grantee == "" || grantee == owner {
		return fmt.Errorf("grantee must be a different user")
	}
	if s.identities != nil && s.identities.UserByID(grantee) == nil {
		return fmt.Errorf("no such user: %s", grantee)
	}
	if mode != ShareSSH && mode != ShareHTTP {
		return fmt.Errorf("share mode must be ssh or http")
	}
	sh := &Share{
		LeaseID:   leaseID,
		Grantee:   grantee,
		Mode:      mode,
		CreatedAt: time.Now(),
	}
	if ttl > 0 {
		sh.ExpiresAt = time.Now().Add(ttl)
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if s.store.shares[leaseID] == nil {
		s.store.shares[leaseID] = make(map[string]*Share)
	}
	s.store.shares[leaseID][grantee] = sh
	return nil
}

// RevokeShare removes a grant (T6/#33). Only the owner can revoke.
// Idempotent.
func (s *Service) RevokeShare(owner, leaseID, grantee string) error {
	l := s.lookup(owner, leaseID)
	if l == nil {
		return fmt.Errorf("sandbox not found")
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if m := s.store.shares[leaseID]; m != nil {
		delete(m, grantee)
		if len(m) == 0 {
			delete(s.store.shares, leaseID)
		}
	}
	return nil
}

// ListShares returns shares granted on the caller's leases (T6/#33),
// newest first.
func (s *Service) ListShares(owner string) []*Share {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	var out []*Share
	for _, m := range s.store.shares {
		for _, sh := range m {
			l := s.store.leases[sh.LeaseID]
			if l == nil || l.released || l.Owner != owner {
				continue
			}
			cp := *sh
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// lookupWithShare returns a live lease the caller may access, either as
// owner or via a valid share covering mode. Used by exec/proxy/endpoint
// so shared leases work without weakening owner scoping.
func (s *Service) lookupWithShare(caller, leaseID string, mode ShareMode) *Lease {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l := s.store.leases[leaseID]
	if l == nil || l.released {
		return nil
	}
	if l.Owner == caller {
		return l
	}
	sh := s.store.shares[leaseID][caller]
	if sh == nil || sh.Mode != mode {
		return nil
	}
	if !sh.ExpiresAt.IsZero() && time.Now().After(sh.ExpiresAt) {
		return nil
	}
	return l
}
```

### 3.13 grantFromSnapshot, lookup, lookupAny

`api/service.go` L991-1071:

```go
// grantFromSnapshot spawns a sandbox directly from a specific snapshot
// tag (not via the warm pool) and grants a lease on it. Used by clone:
// the branch tag is fresh, has no pool, and must not be registered as a
// refillable image (refillPool would start pre-forking clone tags).
func (s *Service) grantFromSnapshot(ctx context.Context, owner, tag string, ttl time.Duration, persistent bool) (*Lease, error) {
	// Quota enforcement (security review #37 rescan F1): clone must not
	// bypass the per-user lease cap. Reserve atomically and release on
	// completion (the reservation becomes the real lease on success).
	if err := s.reserveQuota(owner); err != nil {
		return nil, err
	}
	defer func() { s.releaseQuotaReservation(owner) }()
	lease := &Lease{
		ID:         newID(),
		Owner:      owner,
		Image:      tag,
		CreatedAt:  time.Now(),
		ExpiresAt:  time.Now().Add(ttl),
		Persistent: persistent,
		LastActive: time.Now(),
	}
	if persistent {
		ws, err := s.forkd.CreateWorkspace(ctx, "ws-"+lease.ID, tag, true)
		if err != nil {
			return nil, fmt.Errorf("create workspace: %w", err)
		}
		lease.Workspace = ws.Name
		lease.ForkdID = ws.LiveSandboxID
		if err := s.fillEndpoint(ctx, lease); err != nil {
			return nil, fmt.Errorf("resolve workspace sandbox: %w", err)
		}
		if err := s.probeSandbox(ctx, lease.ForkdID); err != nil {
			_ = s.forkd.DeleteWorkspace(ctx, ws.Name)
			return nil, fmt.Errorf("workspace sandbox failed the integrity probe: %w", err)
		}
		if err := s.applyNetpol(ctx, lease); err != nil {
			return nil, fmt.Errorf("apply network policy: %w", err)
		}
		s.store.mu.Lock()
		s.store.leases[lease.ID] = lease
		s.store.mu.Unlock()
		return lease, nil
	}
	sbs, err := s.forkd.Spawn(ctx, tag, 1, true, 0)
	if err != nil {
		return nil, err
	}
	if len(sbs) == 0 {
		return nil, errNoSandbox
	}
	lease.ForkdID = sbs[0].ID
	lease.Address = sbs[0].GuestAddr
	s.store.mu.Lock()
	s.store.leases[lease.ID] = lease
	s.store.mu.Unlock()
	return lease, nil
}

// lookup returns the lease with the given id if it belongs to owner.
func (s *Service) lookup(owner, id string) *Lease {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l := s.store.leases[id]
	if l == nil || l.Owner != owner || l.released {
		return nil
	}
	return l
}

// lookupAny returns a live lease by id regardless of owner. Used by the
// public HTTP proxy where the lease id in the hostname is the capability
// (same model as the SSH gateway).
func (s *Service) lookupAny(id string) *Lease {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l := s.store.leases[id]
	if l == nil || l.released {
		return nil
	}
	return l
}
```

**NOTE (grantFromSnapshot):** the lease gets no `NetPolicy`, `NetAllow` or `ExposePorts`. On the **non-persistent** path it never calls `probeSandbox` or `applyNetpol`, so a non-persistent clone keeps whatever FORWARD rules the reused netns had. `handleClone` always passes `persistent=true`, so in practice the workspace path runs, and `applyNetpol` there defaults `NetPolicy` "" to `lan`, **not** `restricted`, since `applyNetpol` itself sets `PolicyLAN` when the field is empty.

### 3.14 Endpoint, resolveEndpoint, list

`api/service.go` L1073-1130:

```go
// Endpoint describes how to reach a leased sandbox's guest agent.
type Endpoint struct {
	ForkdID   string
	Netns     string
	GuestAddr string
	GuestHost string
}

// resolveEndpoint finds the live sandbox info (netns + guest addr) for a
// lease by asking the controller. GuestHost is the host part of the
// guest address (the agent port is always 8888).
func (s *Service) resolveEndpoint(ctx context.Context, l *Lease) (*Endpoint, error) {
	sbs, err := s.forkd.ListSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	for _, sb := range sbs {
		if sb.ID == l.ForkdID {
			host, _, err := net.SplitHostPort(sb.GuestAddr)
			if err != nil {
				host = sb.GuestAddr
			}
			return &Endpoint{
				ForkdID:   sb.ID,
				Netns:     sb.Netns,
				GuestAddr: sb.GuestAddr,
				GuestHost: host,
			}, nil
		}
	}
	return nil, fmt.Errorf("sandbox %s not running", l.ForkdID)
}

// list returns the caller's live leases as plain maps.
func (s *Service) list(owner string) []map[string]any {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	var out []map[string]any
	for _, l := range s.store.leases {
		if l.Owner == owner && !l.released {
			out = append(out, map[string]any{
				"id":               l.ID,
				"owner":            l.Owner,
				"image":            l.Image,
				"address":          l.Address,
				"expires":          l.ExpiresAt.Unix(),
				"persistent":       l.Persistent,
				"suspended":        l.Suspended,
				"name":             l.Name,
				"comment":          l.Comment,
				"net_policy":       l.NetPolicy,
				"egress_allowlist": l.NetAllow,
				"exposed":          exposedMap(l),
			})
		}
	}
	return out
}
```

### 3.15 integrityProbe, probeSandbox, warmPool

`api/service.go` L1132-1217:

```go
// integrityProbe is run inside a sandbox before it is pooled or leased. It
// checks what the toolchain files DO, not whether they execute: a corrupted
// /usr/bin/uname can be a valid ELF of plausible size that exits 0 while
// printing some other program's name, so an exit-status check passes it. A
// name-based check is no better — it has to guess at non-GNU tools (busybox
// uname announces itself as BusyBox). Behaviour is the thing that stays true
// across images.
//
// The carriers here are the ones observed in practice: a swapped or
// unexecutable uname (which breaks every `OS=$(uname -s)` build hook) and a
// swapped tr. Extend the script if a new carrier shows up; a probe that does
// not answer at all counts as a failure, since the guest is the only vantage
// point from which this corruption is visible.
const integrityProbe = `p=""
if command -v uname >/dev/null 2>&1; then
  v=$(uname -s 2>&1)
  case "$v" in Linux|linux) ;; *) p="uname -s -> $v" ;; esac
fi
if [ -z "$p" ] && command -v tr >/dev/null 2>&1; then
  v=$(echo a-b | tr - _ 2>&1)
  case "$v" in a_b) ;; *) p="tr a-b - _ -> $v" ;; esac
fi
if [ -z "$p" ]; then echo PROBE_OK; else echo "PROBE_FAIL $p"; exit 1; fi
`

// probeSandbox runs integrityProbe inside a sandbox. nil means usable. An
// unreachable guest agent is a failure rather than a pass: a probe that
// cannot run has told us nothing about the sandbox.
func (s *Service) probeSandbox(ctx context.Context, id string) error {
	if !s.probeEnabled {
		return nil
	}
	res, err := s.forkd.Exec(ctx, id, []string{"sh", "-c", integrityProbe}, int(s.probeTimeout.Seconds()))
	if err != nil {
		return fmt.Errorf("probe: %w", err)
	}
	out := strings.TrimSpace(res.Stdout)
	if res.ExitCode != 0 || out != "PROBE_OK" {
		if out == "" {
			out = strings.TrimSpace(res.Stderr)
		}
		return fmt.Errorf("integrity probe failed (exit %d): %s", res.ExitCode, out)
	}
	return nil
}

// warmPool pre-forks poolSize sandboxes per image tag.
func (s *Service) warmPool(ctx context.Context, image string) {
	if s.poolSize <= 0 {
		return
	}
	s.store.mu.Lock()
	cur := len(s.store.pool[image])
	s.store.mu.Unlock()
	if cur >= s.poolSize {
		return
	}
	// Spawn one child at a time. forkd's restore_many restores all N
	// children concurrently, and a large snapshot (e.g. elixir-base at
	// 2 GiB) can take longer than forkd's 5s socket timeout when several
	// restores run at once, failing the whole batch. Serializing keeps
	// each restore under the timeout.
	for i := cur; i < s.poolSize; i++ {
		sbs, err := s.forkd.Spawn(ctx, image, 1, true, 0)
		if err != nil {
			s.log.Printf("warmPool: spawn %s: %v", image, err)
			return
		}
		// Verify before pooling, so a bad generation is recycled here
		// rather than served to a job. Stop rather than loop: a tag that
		// fails the probe will keep failing it, and retrying spawns a
		// sandbox per attempt.
		for _, sb := range sbs {
			if err := s.probeSandbox(ctx, sb.ID); err != nil {
				s.log.Printf("warmPool: %s sandbox %s failed the integrity probe, recycling: %v", image, sb.ID, err)
				_ = s.forkd.Kill(ctx, sb.ID)
				return
			}
		}
		s.store.mu.Lock()
		for _, sb := range sbs {
			s.store.pool[image] = append(s.store.pool[image], sb.ID)
		}
		s.store.mu.Unlock()
	}
}
```

### 3.16 LiveLeases, Shutdown, CollectMetrics, ReconcileOrphans, errors

`api/service.go` L1219-1349:

```go
// LiveLeases returns the count of active (unreleased) leases. Used by
// the shutdown log line.
func (s *Service) LiveLeases() []string {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	ids := make([]string, 0, len(s.store.leases))
	for id, l := range s.store.leases {
		if !l.released {
			ids = append(ids, id)
		}
	}
	return ids
}

// Shutdown kills every lease and pooled sandbox. Called on SIGTERM/
// SIGINT so a backend restart never orphans warm VMs in the controller
// (the controller has no client-liveness concept, so orphaned VMs
// would otherwise hold netns slots forever).
func (s *Service) Shutdown(ctx context.Context) {
	s.store.mu.Lock()
	all := make([]*Lease, 0, len(s.store.leases))
	for _, l := range s.store.leases {
		all = append(all, l)
	}
	// Pool ids are not full leases; kill them directly.
	poolIDs := make([]string, 0)
	for _, ids := range s.store.pool {
		poolIDs = append(poolIDs, ids...)
	}
	s.store.mu.Unlock()

	for _, l := range all {
		s.release(ctx, l)
	}
	for _, id := range poolIDs {
		if err := s.forkd.Kill(ctx, id); err != nil {
			s.log.Printf("shutdown: kill pooled %s failed: %v", id, err)
		}
	}
}

// ReconcileOrphans kills controller sandboxes that this backend did not
// create. On startup the in-memory lease/pool maps are empty, so any
// live sandbox belongs to a previous backend incarnation (or a foreign
// client); killing them frees netns slots that would otherwise be held
// forever.
// CollectMetrics updates live gauge metrics from the current service
// state (issue #20). Called by the Server before gathering metrics
// for /metrics. All store access is under the store lock.
func (s *Service) CollectMetrics(m *metrics.BackendMetrics) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	// Pool: ready count per image.
	for img, ids := range s.store.pool {
		m.PoolReady.WithLabelValues(img).Set(float64(len(ids)))
	}
	// Pool cap = poolSize × number of known images.
	if s.poolSize > 0 {
		m.PoolCap.Set(float64(s.poolSize * len(s.store.pool)))
	}

	// Leases: count non-released.
	active := 0
	for _, l := range s.store.leases {
		if !l.released {
			active++
		}
	}
	m.LeasesActive.Set(float64(active))

	// Quota reservations.
	pending := 0
	for _, n := range s.store.pending {
		pending += n
	}
	m.QuotaReserved.Set(float64(pending))

	// Shares.
	shares := 0
	for _, grantees := range s.store.shares {
		shares += len(grantees)
	}
	m.SharesActive.Set(float64(shares))
}

func (s *Service) ReconcileOrphans(ctx context.Context) {
	sbs, err := s.forkd.ListSandboxes(ctx)
	if err != nil {
		s.log.Printf("reconcile: list sandboxes failed: %v", err)
		return
	}
	s.store.mu.Lock()
	mine := make(map[string]bool)
	for _, l := range s.store.leases {
		mine[l.ForkdID] = true
	}
	for _, ids := range s.store.pool {
		for _, id := range ids {
			mine[id] = true
		}
	}
	s.store.mu.Unlock()

	killed := 0
	for _, sb := range sbs {
		if mine[sb.ID] {
			continue
		}
		if err := s.forkd.Kill(ctx, sb.ID); err != nil {
			s.log.Printf("reconcile: kill orphan %s failed: %v", sb.ID, err)
			continue
		}
		killed++
		if s.metrics != nil {
			s.metrics.LeaseOrphaned.Inc()
		}
	}
	if killed > 0 {
		s.log.Printf("reconcile: killed %d orphaned sandbox(es) from a previous incarnation", killed)
	}
}

var errNoSandbox = &leaseError{"no sandbox granted"}
var errNotFound = &leaseError{"sandbox not found"}
var errNotPersistent = &leaseError{"sandbox is not a persistent lease"}

// leaseError is a simple sentinel error.
type leaseError struct{ msg string }

func (e *leaseError) Error() string { return e.msg }
```

**NOTE:** the doc comment above `CollectMetrics` (L1260-1264) actually describes `ReconcileOrphans`; the comments were misplaced when `CollectMetrics` was inserted.

---

## 4. forkd/client.go (313 lines): types and HTTP routes

Client and constructor: `NewClient(baseURL, token string) *Client` (default HTTP timeout 600s), `SetHTTPTimeout(d time.Duration)`. Every request sets `Content-Type: application/json` and, when a token is configured, `Authorization: Bearer <token>`. Transport errors are retried 6 times with backoff `attempt*250ms`. Non-2xx responses decode `{"error": "..."}` into `*forkd.Error{StatusCode, Message}`, whose `Error()` is `"forkd: <msg> (status N)"`. Callers detect a missing sandbox with `strings.Contains(err.Error(), "not found")`, or with `err.(*forkd.Error).StatusCode == 404` in `handleExec` and `SnapshotExists`.

`forkd/client.go` L18-83:

```go
// Client talks to a forkd-controller daemon.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClient returns a Client for the controller at baseURL (e.g.
// "http://127.0.0.1:8889"). If token is non-empty it is sent as a
// Bearer token on every request.
func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 600 * time.Second},
	}
}

// SetHTTPTimeout overrides the client's overall per-request timeout. The
// default 600s silently caps exec calls of the same name — a long-running
// CI step (mix release, cargo build) dies at exactly 10 minutes even when
// the requested exec timeout is larger. Set to maxExecTimeout + slack.
func (c *Client) SetHTTPTimeout(d time.Duration) {
	c.http.Timeout = d
}

// SnapshotInfo mirrors the controller's SnapshotInfo JSON shape.
type SnapshotInfo struct {
	Tag           string `json:"tag"`
	Dir           string `json:"dir"`
	CreatedAtUnix int64  `json:"created_at_unix"`
	Status        string `json:"status,omitempty"`
	Bootable      bool   `json:"bootable,omitempty"`
}

// SandboxInfo mirrors the controller's SandboxInfo JSON shape.
type SandboxInfo struct {
	ID             string `json:"id"`
	SnapshotTag    string `json:"snapshot_tag"`
	Netns          string `json:"netns,omitempty"`
	GuestAddr      string `json:"guest_addr,omitempty"`
	CreatedAtUnix  int64  `json:"created_at_unix"`
	PID            int    `json:"pid,omitempty"`
	MemoryLimitMiB int    `json:"memory_limit_mib,omitempty"`
}

// ExecResult is the outcome of a command run inside a sandbox.
type ExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// Error is a typed error carrying the controller's HTTP status and
// error message when one is available.
type Error struct {
	StatusCode int
	Message    string
}

func (e *Error) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("forkd: %s (status %d)", e.Message, e.StatusCode)
	}
	return fmt.Sprintf("forkd: status %d", e.StatusCode)
}
```

`forkd/client.go` L266-276:

```go
// WorkspaceInfo mirrors the controller's workspace record.
type WorkspaceInfo struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	SourceSnapshotTag string `json:"source_snapshot_tag"`
	CurrentStateTag   string `json:"current_state_tag"`
	Status            string `json:"status"` // running | suspended
	LiveSandboxID     string `json:"live_sandbox_id"`
	CreatedAtUnix     int64  `json:"created_at_unix"`
	LastActiveUnix    int64  `json:"last_active_unix"`
}
```

| Method | HTTP | Request body | Response |
|---|---|---|---|
| `ListSnapshots(ctx) ([]SnapshotInfo, error)` | `GET /v1/snapshots` | none | `[]SnapshotInfo` |
| `SnapshotExists(ctx, tag) (bool, error)` | `GET /v1/snapshots/{tag}/info` | none | 200 → true, 404 → false |
| `Spawn(ctx, tag, n, perChildNetns, memoryLimitMiB) ([]SandboxInfo, error)` | `POST /v1/sandboxes` | `{"snapshot_tag","n","per_child_netns", "memory_limit_mib" (only if >0)}` | `[]SandboxInfo` |
| `ListSandboxes(ctx) ([]SandboxInfo, error)` | `GET /v1/sandboxes` | none | `[]SandboxInfo` |
| `Kill(ctx, id) error` | `DELETE /v1/sandboxes/{id}` | none | ignored |
| `Exec(ctx, id, args []string, timeoutSecs int) (*ExecResult, error)` | `POST /v1/sandboxes/{id}/exec` | `{"args":[...],"timeout_secs":N}` | `ExecResult` |
| `Ping(ctx, id) error` | `POST /v1/sandboxes/{id}/ping` | none | `map[string]any` (ignored) |
| `Branch(ctx, id, tag) (string, error)` | `POST /v1/sandboxes/{id}/branch` | `{"tag":tag}` | `{"tag":...}` (error if empty) |
| `Metrics(ctx) ([]byte, error)` | `GET /metrics` | none (**no auth header sent**, and no retry) | raw Prometheus text |
| `CreateWorkspace(ctx, name, tag, perChildNetns) (*WorkspaceInfo, error)` | `POST /v1/workspaces` | `{"name","snapshot_tag","per_child_netns"}` | `WorkspaceInfo` |
| `SuspendWorkspace(ctx, name) error` | `POST /v1/workspaces/{name}/suspend` | `{}` | ignored |
| `ResumeWorkspace(ctx, name) (*WorkspaceInfo, error)` | `POST /v1/workspaces/{name}/resume` | `{}` | `WorkspaceInfo` (`LiveSandboxID` changes) |
| `DeleteWorkspace(ctx, name) error` | `DELETE /v1/workspaces/{name}` | none | ignored |

**NOTE:** the forkd exec API takes **argv only, with no cwd and no env**. `api.buildShellArgs` (§5.13) folds cwd and env into a `/bin/bash -c` string.

---

## 5. api/server.go (1396 lines) plus shares.go, users.go, images.go, llmgateway.go

### 5.1 Server struct

`api/server.go` L26-62:

```go
// Server is the HTTP lease API.
type Server struct {
	svc       *Service
	reg       *ImageRegistry
	mux       *http.ServeMux
	llm       *llmGateway
	assetsDir string // static assets dir served at /assets/ on the proxy listener

	// Proxy authentication (U7/T7): mode "" or "off" = open capability
	// model (today's behavior); "forward-auth" = the proxy listener
	// requires X-Proxy-Auth == secret and resolves the authenticated
	// user from Remote-User before owner-scoping lookups.
	proxyAuthMode   string
	proxyAuthSecret string
	// proxyTrustedPeers (security review #37 M3): when non-empty, the
	// forward-auth gate only honors Remote-User from these peer
	// addresses/CIDRs — defense in depth so a direct client to :8891
	// can't spoof a username even with the shared secret. Set via
	// PROXY_AUTH_TRUSTED_PEERS (comma-separated).
	proxyTrustedPeers []*net.IPNet
	// bootstrapToken (security review #37 H3/M4) gates the first-user
	// bootstrap when configured: the store-empty creation must present
	// X-Bootstrap-Token matching it. Set via BOOTSTRAP_TOKEN env.
	bootstrapToken string
	// metrics (issue #20): service-owned Prometheus metrics served at
	// /metrics alongside namespaced controller passthrough.
	metrics *metrics.BackendMetrics
	// authFails (security review #37 L5) throttles repeated failed
	// token auths per client IP.
	authFails *authFailLimiter
	// busy (security review #37 rescan F9) caps concurrent exec/stream
	// operations per owner so a tenant can't saturate the controller
	// with in-flight activity (quota covers lease count, not activity).
	busyMu    sync.Mutex
	busyCount map[string]int
	busyMax   int
}
```

### 5.2 Routes (NewServerWithLLM)

`api/server.go` L123-187:

```go
// NewServer wires the lease API routes onto a mux. openRouterURL and
// openRouterKey are the LLM gateway upstream (empty = gateway disabled);
// the key is held process-side and never exposed to sandboxes.
func NewServer(svc *Service, reg *ImageRegistry) *Server {
	return NewServerWithLLM(svc, reg, "", "", "", nil)
}

// NewServerWithLLM wires the lease API routes plus an optional per-lease
// LLM gateway. openRouterURL is the OpenAI-compatible API base; the key
// stays in this process. defaultModel is the upstream fallback model
// (applied when a requested id isn't in modelMap); modelMap translates
// exe.dev catalog model ids to upstream ids.
func NewServerWithLLM(svc *Service, reg *ImageRegistry, openRouterURL, openRouterKey, defaultModel string, modelMap map[string]string) *Server {
	s := &Server{svc: svc, reg: reg, mux: http.NewServeMux(), authFails: newAuthFailLimiter(),
		busyCount: map[string]int{}, busyMax: 8, metrics: metrics.NewBackendMetrics()}
	if openRouterURL != "" {
		// svc.identities must be installed (SetIdentities) before
		// NewServerWithLLM for per-user LLM key enforcement (U8/T8).
		s.llm = newLLMGateway(svc.log, svc.lookupAny, svc.identities, openRouterURL, openRouterKey, defaultModel, modelMap)
		s.llm.metrics = s.metrics
	}
	s.svc.SetMetrics(s.metrics)
	s.mux.HandleFunc("POST /api/sandboxes", s.handleCreate)
	s.mux.HandleFunc("GET /api/sandboxes", s.handleList)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/exec", s.handleExec)
	s.mux.HandleFunc("DELETE /api/sandboxes/{id}", s.handleDelete)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/keepalive", s.handleKeepAlive)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/suspend", s.handleSuspend)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/resume", s.handleResume)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/restart", s.handleRestart)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/tag", s.handleTag)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/comment", s.handleComment)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/prompt", s.handlePrompt)
	s.mux.HandleFunc("GET /api/sandboxes/{id}/endpoint", s.handleEndpoint)
	s.mux.HandleFunc("GET /api/sandboxes/{id}/stat", s.handleStat)
	s.mux.HandleFunc("GET /api/sandboxes/{id}/stream", s.handleStream)
	s.mux.HandleFunc("POST /api/sandboxes/{id}/clone", s.handleClone)
	// Sharing (T6/#33).
	s.mux.HandleFunc("POST /api/sandboxes/{id}/share", s.handleShareGrant)
	s.mux.HandleFunc("DELETE /api/sandboxes/{id}/share/{grantee}", s.handleShareRevoke)
	s.mux.HandleFunc("GET /api/shares", s.handleShareList)
	s.mux.HandleFunc("GET /api/images", s.handleImages)
	s.mux.HandleFunc("GET /api/names/{name}", s.handleByName)
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	// Identity endpoints (epic #26 T1): user management + key resolution.
	if s.svc.identities != nil {
		s.mux.HandleFunc("GET /api/users", s.handleUsersList)
		s.mux.HandleFunc("GET /api/users/me", s.handleUsersMe)
		s.mux.HandleFunc("GET /api/users/by-name/{name}", s.handleUsersByName)
		s.mux.HandleFunc("GET /api/users/by-key", s.handleUsersByKey)
		s.mux.HandleFunc("GET /api/identity-status", s.handleIdentityStatus)
		s.mux.HandleFunc("POST /api/users", s.handleUsersCreate)
		s.mux.HandleFunc("POST /api/users/{id}/quota", s.handleUsersQuota)
		s.mux.HandleFunc("POST /api/users/{id}/llm-key", s.handleUsersLLMKey) // U8/T8
		s.mux.HandleFunc("DELETE /api/users/{id}", s.handleUsersDelete)
	}
	if s.llm != nil {
		// The LLM gateway is auth-exempt (lease id in path is the
		// capability); it MUST be mounted on the outer handler after
		// authMiddleware. Handler() does that via authExempt prefix.
		s.mux.Handle(llmGatewayPrefix, s.llm)
	}
	return s
}
```

Middleware order: `Handler()` = `metricsMiddleware(authMiddleware(mux))`. Auth is exempt for `/healthz` and for the `/llm/` prefix. Per-owner busy cap: `busyMax: 8` concurrent exec/stream.

| Method + path | Handler | Request JSON | Response JSON (status) |
|---|---|---|---|
| `POST /api/sandboxes` | `handleCreate` | see §5.4 | 201 `{"id","owner","address","image","ttl","persistent","expires_at","exposed"}` |
| `GET /api/sandboxes` | `handleList` | none | 200 `{"sandboxes":[{"id","owner","image","address","expires"(unix int),"persistent","suspended","name","comment","net_policy","egress_allowlist","exposed"}]}` (`sandboxes` is `null` when empty, because it's a nil slice) |
| `POST /api/sandboxes/{id}/exec` | `handleExec` | `{"cmd","cwd","env":{},"timeout"}` | 200 `{"stdout","stderr","exit"}`; 404, 409 (suspended), 410 (forkd 404), 429, 500 |
| `DELETE /api/sandboxes/{id}` | `handleDelete` | none | 204; 404 |
| `POST /api/sandboxes/{id}/keepalive` | `handleKeepAlive` | optional `{"ttl"}` | 200 `{"id","persistent":true,"expires_at"}`; 400 (not persistent) |
| `POST /api/sandboxes/{id}/suspend` | `handleSuspend` | none | 200 `{"id","status":"suspended","message"}` |
| `POST /api/sandboxes/{id}/resume` | `handleResume` | none | 200 `{"id","status":"running","address"}` |
| `POST /api/sandboxes/{id}/restart` | `handleRestart` | none | 200 `{"id","status":"running","message":"sandbox restarted"}` |
| `POST /api/sandboxes/{id}/tag` | `handleTag` | `{"name"}` | 200 `{"id","name","ok":true}` |
| `POST /api/sandboxes/{id}/comment` | `handleComment` | `{"comment"}` | 200 `{"id","comment","ok":true}` |
| `POST /api/sandboxes/{id}/prompt` | `handlePrompt` | `{"message","model"?}` | 200 `{"id","message","reply"}`; 409/504/502 |
| `GET /api/sandboxes/{id}/endpoint` | `handleEndpoint` | none | 200 `{"id","forkd_id","image","netns","guest_addr"}` |
| `GET /api/sandboxes/{id}/stat` | `handleStat` | none | 200 `statResult` (§5.9) |
| `GET /api/sandboxes/{id}/stream` | `handleStream` | WebSocket (§5.6) | WS frames |
| `POST /api/sandboxes/{id}/clone` | `handleClone` | optional `{"tag"}` | 201 `{"id","image","source","branch_tag","persistent","expires_at"}` |
| `POST /api/sandboxes/{id}/share` | `handleShareGrant` | `{"grantee","mode":"ssh|http","ttl"}` | 201 `{"shared":true,"lease_id","grantee","mode"}` |
| `DELETE /api/sandboxes/{id}/share/{grantee}` | `handleShareRevoke` | none | 204 |
| `GET /api/shares` | `handleShareList` | none | 200 `{"shares":[{"lease_id","grantee","mode","expires_at"?,"created_at"}]}` |
| `GET /api/images` | `handleImages` | none | 200 `{"images":[...tags]}` |
| `GET /api/names/{name}` | `handleByName` | none | 200 `{"id","name","image"}` |
| `GET /healthz` | `handleHealthz` | none (no auth) | 200 `{"status":"ok"}` |
| `GET /metrics` | `handleMetrics` | none (admin if identity store) | Prometheus text plus forkd passthrough |
| `GET /api/users` | `handleUsersList` (admin) | none | `{"users":[UserView]}` |
| `GET /api/users/me` | `handleUsersMe` | none | `{"user":UserView}` |
| `GET /api/users/by-name/{name}` | `handleUsersByName` | none | `{"user":{"id","name"}}` |
| `GET /api/users/by-key?fingerprint=` | `handleUsersByKey` | none | `{"user":{"id","name"}}` |
| `GET /api/identity-status` | `handleIdentityStatus` | none | `{"identity_store":bool}` |
| `POST /api/users` | `handleUsersCreate` | `{"name","kind","fingerprints":[],"token"}` | 201 `{"user":UserView}` |
| `POST /api/users/{id}/quota` | `handleUsersQuota` (admin) | `{"max_leases","max_ttl"}` | `{"user":UserView}` |
| `POST /api/users/{id}/llm-key` | `handleUsersLLMKey` (admin) | `{"llm_key"}` | `{"user":UserView}` |
| `DELETE /api/users/{id}` | `handleUsersDelete` (admin) | none | (see users.go L237-255) |
| `/llm/` prefix (any method) | `llmGateway.ServeHTTP` | OpenAI-compatible; path `/llm/<lease-id>/<provider>/...` | proxied upstream |

User routes are registered only when `svc.identities != nil`, and `/llm/` only when `LLM_UPSTREAM_URL` is set. All errors use `{"error":"<msg>"}` (`writeError`). `UserView` (users.go L15-24): `{"id","name","kind","admin","fingerprints","created_at","max_leases","max_ttl"}`.

### 5.3 handleMetrics, authMiddleware (gateway impersonation)

`api/server.go` L215-248:

```go
// handleMetrics emits service-owned Prometheus metrics (issue #20)
// merged with namespaced controller passthrough. The service-owned
// metrics are gathered from the live Service state (pool, leases,
// identity) and rendered via the prometheus registry; the controller
// metrics are fetched from forkd-controller's /metrics and rewritten
// to spoond_controller_* so service vs controller semantics never
// collide. Requires admin when the identity store is present
// (security review #37 M5).
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.svc.identities != nil && !s.requireAdmin(w, r) {
		return
	}
	// Update live gauges from current service state before gathering.
	s.collectServiceMetrics()
	// Gather service-owned metrics from the registry.
	mfs, err := s.metrics.Registry.Gather()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "gather metrics: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	enc := expfmt.NewEncoder(w, expfmt.FmtText)
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			return
		}
	}
	// Fetch and namespace controller passthrough metrics.
	body, err := s.svc.forkd.Metrics(r.Context())
	if err == nil {
		_, _ = w.Write([]byte(metrics.NamespaceControllerMetrics(body)))
	}
}
```

`api/server.go` L349-417:

```go
// authMiddleware authenticates the bearer token and injects the
// consumer id into the request context. /healthz is exempt (liveness);
// the /llm/ prefix is exempt too — the lease id in the path is the
// capability, and sandboxes hold no consumer token.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, llmGatewayPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")
		if token == "" || token == auth {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		// Throttle repeated FAILED auths per IP (security review #37
		// L5). hit() returns true when the caller is now over the
		// limit; valid tokens always pass and reset the window.
		ip := clientIP(r.RemoteAddr)
		owner, ok := s.svc.ResolveOwner(token)
		if !ok {
			s.metrics.AuthFailures.Inc()
			if s.authFails.hit(ip) {
				s.metrics.AuthThrottled.Inc()
				writeError(w, http.StatusTooManyRequests, "too many failed auth attempts; try again shortly")
				return
			}
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		s.authFails.clear(ip)
		ctx := context.WithValue(r.Context(), ctxOwnerKey{}, owner)
		// Record the resolved identity (user id) when available so handlers
		// can distinguish an identity-store user from a legacy consumer.
		if s.svc.identities != nil {
			if u := s.svc.identities.UserByToken(token); u != nil {
				ctx = context.WithValue(ctx, ctxUserKey{}, u)
			}
		}
		// Trusted gateway impersonation (U6/T5): when the request carries the
		// SSH gateway's service token AND a X-Spoond-User-Id header, act as
		// that user so the backend's owner-scoping applies to the SSH caller
		// rather than the gateway service identity. The header user must
		// exist in the identity store.
		if s.svc.gatewayToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.svc.gatewayToken)) == 1 {
			if uid := r.Header.Get("X-Spoond-User-Id"); uid != "" {
				// Nil-identity guard (security review #37 rescan F12): the
				// gateway token can be configured without an identity store
				// (legacy single-consumer deployments); without this check a
				// request carrying X-Spoond-User-Id would nil-deref and
				// crash the goroutine.
				if s.svc.identities != nil {
					if u := s.svc.identities.UserByID(uid); u != nil {
						ctx = context.WithValue(ctx, ctxOwnerKey{}, u.ID)
						ctx = context.WithValue(ctx, ctxUserKey{}, u)
					} else {
						writeError(w, http.StatusForbidden, "unknown impersonated user")
						return
					}
				}
			}
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type ctxOwnerKey struct{}
type ctxUserKey struct{}
```

### 5.4 Constants plus handleCreate (expose_ports, net policy)

`api/server.go` L479-623:

```go
// maxExecTimeout caps a single exec call so it cannot run far past the
// lease TTL or tie up the controller indefinitely. Overridable via
// MAX_EXEC_TIMEOUT_SECS: compile-heavy CI steps (a Phoenix mix release,
// large cargo builds) legitimately run tens of minutes.
var maxExecTimeout = func() int {
	if v := os.Getenv("MAX_EXEC_TIMEOUT_SECS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 300 // seconds
}()

func ownerFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxOwnerKey{}).(string)
	return v
}

// maxLeaseMemoryMiB caps the per-lease memory a caller may request
// (security review #37 rescan F6). 16 GiB — generous for compute
// workloads, bounded against host OOM.
const maxLeaseMemoryMiB = 16 * 1024

// hostBridgeAllow is the host-side bridge IP that every guest needs
// even under the default restricted policy: the LLM gateway, shelly
// binary assets, and the public proxy all live on the host and guests
// reach them via the bridge (security review #37 rescan F3). This must
// match the bridge IP used by forkd-controller (10.43.0.1) — the same
// constant the gateway uses for SHELLY_BINARY_URL / LLM_GATEWAY_URL.
var hostBridgeAllow = []string{"10.43.0.1"}

// handleCreate grants a new sandbox lease.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Image      string   `json:"image"`
		TTL        int      `json:"ttl"` // seconds
		MemoryMiB  int      `json:"memory_mib"`
		Network    string   `json:"network"`
		InitCmd    string   `json:"init_cmd"`
		Persistent bool     `json:"persistent"`
		NetPolicy  string   `json:"network_policy"`
		NetAllow   []string `json:"egress_allowlist"`
		// ExposePorts publishes guest TCP ports on the lease's bridge-facing
		// address for other sandboxes to reach (netpolicy.go).
		ExposePorts []int `json:"expose_ports"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Image == "" {
		writeError(w, http.StatusBadRequest, "image is required")
		return
	}
	// Egress policy (security review #37 rescan F3): default restricted —
	// NOT lan. The default must not let a guest reach other tenants'
	// sandboxes on the shared bridge (each sandbox carries an
	// unauthenticated root exec agent on :8888 and a shelley agent on
	// :9000). Restricted allows the host bridge (LLM gateway, shelly
	// assets, proxy) + configured allowlist, and blocks guest→guest.
	// Operators who need full LAN egress opt in explicitly.
	if req.NetPolicy == "" {
		req.NetPolicy = string(PolicyRestricted)
	}
	if !ValidNetworkPolicy(req.NetPolicy) {
		writeError(w, http.StatusBadRequest, "network_policy must be none|lan|internet|restricted")
		return
	}
	if req.NetPolicy == string(PolicyRestricted) {
		// Guests always need the host bridge IP (LLM gateway, assets,
		// proxy) even under restricted; without it every default lease
		// would lose guest-side LLM/proxy access. Appending it here
		// keeps the default functional while still blocking peers.
		req.NetAllow = append(req.NetAllow, hostBridgeAllow...)
	}
	expose, err := ValidateExposePorts(req.ExposePorts)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(expose) > 0 && !s.svc.CanExposePorts() {
		writeError(w, http.StatusNotImplemented, "port exposure needs network policy enforcement (NETPOL_DNS) on this backend")
		return
	}
	ok, err := s.reg.Has(r.Context(), req.Image)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "image registry unavailable")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "unknown image tag: "+req.Image)
		return
	}
	// Cap the requested TTL in seconds BEFORE converting to a duration,
	// so a huge ttl value cannot overflow time.Duration and bypass the
	// maxTTL cap (yielding a near-zero lease).
	ttlSecs := req.TTL
	if ttlSecs <= 0 {
		ttlSecs = int(s.svc.defaultTTL / time.Second)
	}
	if maxSecs := int(s.svc.maxTTL / time.Second); ttlSecs > maxSecs {
		ttlSecs = maxSecs
	}
	// The TTL cap above already bounds persistent leases; keep-alive
	// lets the consumer extend them (up to maxTTL per call).
	ttl := time.Duration(ttlSecs) * time.Second
	// Per-user TTL clamp (T4/#31): a user's max_ttl, when set, is the
	// hard ceiling even below the global max.
	if u := userFrom(r.Context()); u != nil && u.MaxTTL > 0 {
		if userMax := time.Duration(u.MaxTTL) * time.Second; ttl > userMax {
			ttl = userMax
		}
	}
	// Cap the memory request (security review #37 rescan F6): an
	// uncapped memory_mib lets a tenant drive a cold spawn with an
	// absurd limit and exhaust host RAM (the warm pool is off by
	// default, so most spawns are cold). The controller clamps per-VM
	// but must never receive an attacker-chosen unbounded value.
	if req.MemoryMiB < 0 {
		req.MemoryMiB = 0
	}
	if req.MemoryMiB > maxLeaseMemoryMiB {
		req.MemoryMiB = maxLeaseMemoryMiB
	}
	lease, err := s.svc.grant(r.Context(), ownerFrom(r.Context()), req.Image, req.MemoryMiB, ttl, req.Persistent, req.NetPolicy, req.NetAllow, expose...)
	if err != nil {
		if err == errQuotaExceeded {
			writeError(w, http.StatusTooManyRequests, err.Error())
			return
		}
		s.svc.log.Printf("create: grant %s: %v", req.Image, err)
		writeError(w, http.StatusInternalServerError, "failed to grant sandbox")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         lease.ID,
		"owner":      lease.Owner,
		"address":    lease.Address,
		"image":      lease.Image,
		"ttl":        int(ttl.Seconds()),
		"persistent": lease.Persistent,
		"expires_at": lease.ExpiresAt.UTC().Format(time.RFC3339),
		"exposed":    exposedMap(lease),
	})
}
```

**NOTE:**
- `Network` (`"network"`) and `InitCmd` (`"init_cmd"`) are decoded but **never used**.
- The default policy at the API layer is `restricted`, with `10.43.0.1` auto-appended to the allowlist. The `Lease` comment ("" = lan) and `applyNetpol`'s own default (`lan`) apply only to leases created elsewhere (clone).
- `restricted` without an allowlist is **not** rejected by the code, because `hostBridgeAllow` is always appended. The integration test "restricted without allowlist rejected" (test_netpolicy.sh) suggests otherwise. **AMBIGUOUS** whether that test currently passes.

### 5.5 handleEndpoint

`api/server.go` L625-650:

```go
// handleEndpoint resolves a lease to the sandbox's live network endpoint
// (netns + guest addr). Used by the SSH gateway to reach sshd inside the
// VM. The lease owner must match (a lease id is a capability: anyone who
// holds it can resolve + connect).
func (s *Server) handleEndpoint(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	// Shared leases are attachable over SSH (T6/#33).
	lease := s.svc.lookupWithShare(owner, id, ShareSSH)
	if lease == nil {
		writeError(w, http.StatusNotFound, "sandbox not found")
		return
	}
	ep, err := s.svc.resolveEndpoint(r.Context(), lease)
	if err != nil {
		writeError(w, http.StatusNotFound, "sandbox not running")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         lease.ID,
		"forkd_id":   ep.ForkdID,
		"image":      lease.Image,
		"netns":      ep.Netns,
		"guest_addr": ep.GuestAddr,
	})
}
```

### 5.6 handleStream: the WebSocket protocol

`api/server.go` L652-795:

```go
// handleStream opens a WebSocket to a sandbox and relays an interactive
// PTY session: the client sends {"args":[...],"cwd":...} as the first
// message; output streams back as text frames; client text frames are
// written to the process stdin. Protocol matches the agent's "stream"
// action (line-delimited JSON on the agent side).
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	// Per-owner activity cap (security review #37 rescan F9).
	if !s.acquireBusy(owner) {
		http.Error(w, "too many concurrent exec/stream operations; try again shortly", http.StatusTooManyRequests)
		return
	}
	defer s.releaseBusy(owner)
	lease := s.svc.lookupWithShare(owner, id, ShareHTTP)
	if lease == nil {
		writeError(w, http.StatusNotFound, "sandbox not found")
		return
	}
	s.svc.touch(id) // stream attach is activity for the idle sweeper
	// A suspended workspace-backed lease has no running sandbox; resume
	// first.
	if lease.Suspended {
		writeError(w, http.StatusConflict, "sandbox is suspended; resume it first")
		return
	}
	ep, err := s.svc.resolveEndpoint(r.Context(), lease)
	if err != nil {
		writeError(w, http.StatusNotFound, "sandbox not running")
		return
	}

	upgrader := websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true }, // consumer-token auth above
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()

	// First message: the exec request.
	mt, payload, err := ws.ReadMessage()
	if err != nil {
		return
	}
	if mt != websocket.TextMessage {
		ws.WriteMessage(websocket.TextMessage, []byte(`{"error":"first message must be JSON text"}`))
		return
	}
	var req struct {
		Args []string          `json:"args"`
		Cwd  string            `json:"cwd"`
		Env  map[string]string `json:"env"`
		Pty  *bool             `json:"pty"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte(`{"error":"bad request JSON"}`))
		return
	}
	if len(req.Args) == 0 {
		ws.WriteMessage(websocket.TextMessage, []byte(`{"error":"args required"}`))
		return
	}
	pty := true
	if req.Pty != nil {
		pty = *req.Pty
	}

	// Dial the agent inside the sandbox's netns. The agent binds
	// 127.0.0.1:8888 (security review #37 rescan F3) — it must NOT be
	// reachable from peer sandboxes on the bridge. dialInNetns enters
	// the guest netns, so loopback resolves to the guest's own agent.
	agentAddr := net.JoinHostPort("127.0.0.1", "8888")
	agent, err := dialInNetns(ep.Netns, agentAddr)
	if err != nil {
		ws.WriteMessage(websocket.TextMessage, []byte(`{"error":"agent unreachable: `+err.Error()+`"}`))
		return
	}
	defer agent.Close()

	startReq := map[string]any{
		"action": "stream",
		"args":   req.Args,
		"cwd":    req.Cwd,
		"env":    req.Env,
		"pty":    pty,
	}
	line, _ := json.Marshal(startReq)
	if _, err := agent.Write(append(line, '\n')); err != nil {
		return
	}

	// Agent -> WS relay (line-delimited JSON).
	agentDone := make(chan struct{})
	go func() {
		defer close(agentDone)
		br := bufio.NewReader(agent)
		for {
			ln, err := br.ReadBytes('\n')
			if len(ln) > 0 {
				_ = ws.WriteMessage(websocket.TextMessage, ln)
			}
			if err != nil {
				return
			}
		}
	}()

	// WS -> agent relay: {"in":"...","action":"stop"} messages.
	for {
		mt, payload, err := ws.ReadMessage()
		if err != nil {
			break
		}
		if mt != websocket.TextMessage {
			continue
		}
		var msg struct {
			In     string `json:"in"`
			Action string `json:"action"`
		}
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		if msg.Action == "stop" {
			break
		}
		if msg.In != "" {
			out, _ := json.Marshal(map[string]string{"in": msg.In})
			if _, err := agent.Write(append(out, '\n')); err != nil {
				break
			}
		}
	}

	<-agentDone
}

// dialInNetns enters the given network namespace on a locked thread,
// dials addr, and returns the connection (usable from any thread once
// established — the socket is already bound in the target netns).
// dialInNetns is defined in netns_linux.go (Linux) and netns_other.go
// (non-Linux stub). It enters the named netns and dials addr.
```

**WebSocket protocol summary** (gorilla/websocket, text frames only; `CheckOrigin` always true; auth is the bearer token on the upgrade request):
- Pre-upgrade HTTP errors: 429 (busy, plain text via `http.Error`), 404 `{"error":"sandbox not found"}`, 409 suspended, 404 "sandbox not running".
- **Client → server, first frame** (text JSON): `{"args":[...](required), "cwd":"...", "env":{"K":"V"}, "pty":bool (default true)}`.
- Server errors as text frames: `{"error":"first message must be JSON text"}`, `{"error":"bad request JSON"}`, `{"error":"args required"}`, `{"error":"agent unreachable: <err>"}`.
- Server → agent (TCP, line JSON): `{"action":"stream","args":[...],"cwd":"...","env":{...},"pty":bool}\n`.
- **Agent → client** (each agent line relayed verbatim as one text frame, **including the trailing `\n`**):
  - `{"stream":"started","pid":N,"pty":bool}`
  - then zero or more `{"out":"<utf8 text>"}` (stdout and stderr merged under pty; **stdout only** without pty, since stderr is piped but never read)
  - then `{"exit_code":N}`
  - or, on agent exception, `{"error":"...","traceback":"...","exit_code":1}`
- **Client → server, subsequent frames:** `{"in":"<text>"}` is forwarded to the agent as `{"in":"..."}\n`. `{"action":"stop"}` makes the **server** break its read loop. It does **not** forward `stop` to the agent; it then waits for the agent reader to finish (the agent sees EOF when the handler returns and closes `agent`, since `defer agent.Close()` runs after `<-agentDone`). Non-text frames and unparsable JSON are ignored. **No resize/window-change message exists.**
- **AMBIGUOUS / probable bug:** the handler dials `127.0.0.1:8888` inside the lease netns, and the comment claims the agent binds loopback. But `forkd-agent.py` binds `0.0.0.0:8888` **inside the guest VM** (§10.5), and the netns loopback is the host-side netns, not the guest. `docs/security.md` says the agent still binds 0.0.0.0:8888 because forkd-controller dials it from the host. So unless forkd-controller installs a loopback forward in the netns, this dial targets nothing. Whether stream works in production can't be told from the repo. `test_stream.sh` exists and asserts it does. Compare `handleProxy`, which dials `GuestHost:port`, the real guest IP.
- **AMBIGUOUS:** `dialInNetns` calls `runtime.UnlockOSThread()` after `setns` (netns_linux.go L27), which returns a thread still in the sandbox netns to the Go scheduler. The gateway's `dialSandbox` has the same pattern.

### 5.7 handleKeepAlive, handleSuspend, handleRestart, handleTag, handleComment

`api/server.go` L797-930:

```go
// handleKeepAlive extends a persistent lease's expiry. The caller must
// own the lease. Body may carry {"ttl": seconds} (capped at maxTTL).
func (s *Server) handleKeepAlive(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	var req struct {
		TTL int `json:"ttl"` // seconds; 0 = maxTTL
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	ttl := time.Duration(req.TTL) * time.Second
	lease, err := s.svc.keepAlive(owner, id, ttl)
	if err != nil {
		switch err {
		case errNotFound:
			writeError(w, http.StatusNotFound, "sandbox not found")
		case errNotPersistent:
			writeError(w, http.StatusBadRequest, "sandbox is not a persistent lease")
		default:
			writeError(w, http.StatusInternalServerError, "keepalive failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         lease.ID,
		"persistent": true,
		"expires_at": lease.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// handleSuspend suspends a workspace-backed persistent lease: the
// controller snapshots the sandbox and stops it. The lease stays and can
// be resumed.
func (s *Server) handleSuspend(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	lease, err := s.svc.suspend(r.Context(), owner, id)
	if err != nil {
		switch err {
		case errNotFound:
			writeError(w, http.StatusNotFound, "sandbox not found")
		case errNotPersistent:
			writeError(w, http.StatusBadRequest, "sandbox is not a workspace-backed persistent lease")
		default:
			s.svc.log.Printf("suspend %s: %v", id, err)
			writeError(w, http.StatusInternalServerError, "suspend failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      lease.ID,
		"status":  "suspended",
		"message": "sandbox suspended; state snapshot kept (resume to restore)",
	})
}

// handleRestart reboots a persistent lease (workspace-backed: snapshot +
// resume; plain: kill + cold spawn).
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	lease, err := s.svc.restart(r.Context(), owner, id)
	if err != nil {
		switch err {
		case errNotFound:
			writeError(w, http.StatusNotFound, "sandbox not found")
		case errNotPersistent:
			writeError(w, http.StatusBadRequest, "sandbox is not a persistent lease")
		default:
			s.svc.log.Printf("restart %s: %v", id, err)
			writeError(w, http.StatusInternalServerError, "restart failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      lease.ID,
		"status":  "running",
		"message": "sandbox restarted",
	})
}

// handleTag assigns a friendly name to a lease.
func (s *Server) handleTag(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	lease, err := s.svc.setName(owner, id, req.Name)
	if err != nil {
		if err == errNotFound {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":   lease.ID,
		"name": lease.Name,
		"ok":   true,
	})
}

// handleComment sets or clears the free-text annotation on a lease.
// An empty comment clears it.
func (s *Server) handleComment(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	var req struct {
		Comment string `json:"comment"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	lease, err := s.svc.setComment(owner, id, req.Comment)
	if err != nil {
		if err == errNotFound {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      lease.ID,
		"comment": lease.Comment,
		"ok":      true,
	})
}
```

### 5.8 handlePrompt (calls forkd.Exec directly; Shelley on guest :9000)

`api/server.go` L932-1019:

```go
// handlePrompt sends a message to the Shelley coding agent running inside
// a lease and returns the agent's reply. Requires the agent to be up
// (see the `shelly` ctl verb / runShelly). Implements `shelley prompt`
// from exe.dev's CLI surface as an LLM-callable API command.
func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	var req struct {
		Message string `json:"message"`
		Model   string `json:"model,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Message) == "" {
		writeError(w, http.StatusBadRequest, "{\"message\":\"...\"} required")
		return
	}
	// Shared leases accept agent prompts too (T6/#33).
	lease := s.svc.lookupWithShare(owner, id, ShareSSH)
	if lease == nil {
		writeError(w, http.StatusNotFound, "sandbox not found")
		return
	}
	if lease.Suspended {
		writeError(w, http.StatusConflict, "sandbox is suspended; resume it first")
		return
	}
	model := req.Model
	if model == "" {
		model = "gpt-oss-20b-fireworks"
	}
	msg64 := base64.StdEncoding.EncodeToString([]byte(req.Message))
	mod64 := base64.StdEncoding.EncodeToString([]byte(model))
	script := fmt.Sprintf(`set -e
MSG=$(echo %s | base64 -d)
MOD=$(echo %s | base64 -d)
RESP=$(curl -sf --max-time 60 -H 'Content-Type: application/json' \
  -d "{\"message\":$(printf '%%s' "$MSG" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))'),\"model\":$(printf '%%s' "$MOD" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))')}" \
  http://127.0.0.1:9000/api/conversations/new) || { echo "SHELLEY_NOT_RUNNING"; exit 1; }
CID=$(echo "$RESP" | python3 -c 'import sys,json;print(json.load(sys.stdin)["conversation_id"])')
for i in $(seq 1 40); do
  sleep 5
  OUT=$(curl -sf --max-time 10 http://127.0.0.1:9000/api/conversation/$CID 2>/dev/null || true)
  AGENT=$(echo "$OUT" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    raise SystemExit
for m in d.get("messages", []):
    if m.get("type") == "agent":
        ld = json.loads(m.get("llm_data") or "{}")
        content = ld.get("Content") or []
        text = " ".join(c.get("Text","") for c in content if isinstance(c, dict))
        if text.strip():
            print(text)
            raise SystemExit
' 2>/dev/null || true)
  if [ -n "$AGENT" ]; then echo "$AGENT"; exit 0; fi
done
echo "AGENT_TIMEOUT"`, msg64, mod64)

	s.svc.log.Printf("prompt %s: %s", id, req.Message)
	start := time.Now()
	res, err := s.svc.forkd.Exec(r.Context(), lease.ForkdID, buildShellArgs(script, "", nil), 240)
	if err != nil {
		s.svc.log.Printf("prompt %s: %v (dur=%s)", id, err, time.Since(start))
		writeError(w, http.StatusBadGateway, "agent exec failed: "+err.Error())
		return
	}
	s.svc.log.Printf("prompt %s: exit=%d stdout=%d dur=%s", id, res.ExitCode, len(res.Stdout), time.Since(start))
	out := res.Stdout
	if strings.Contains(out, "SHELLEY_NOT_RUNNING") {
		writeError(w, http.StatusConflict, "shelley agent is not running in this sandbox — use the shelly ctl verb first")
		return
	}
	if strings.Contains(out, "AGENT_TIMEOUT") {
		writeError(w, http.StatusGatewayTimeout, "agent did not reply within 200s")
		return
	}
	if res.ExitCode != 0 {
		writeError(w, http.StatusBadGateway, "agent exec failed: "+tailStr(res.Stderr, 500))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      id,
		"message": req.Message,
		"reply":   strings.TrimSpace(out),
	})
}
```

### 5.9 handleResume, handleList, handleExec, handleStat (+statResult)

`api/server.go` L1021-1183:

```go
// handleResume restores a suspended workspace-backed lease.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	lease, err := s.svc.resume(r.Context(), owner, id)
	if err != nil {
		switch err {
		case errNotFound:
			writeError(w, http.StatusNotFound, "sandbox not found")
		case errNotPersistent:
			writeError(w, http.StatusBadRequest, "sandbox is not a workspace-backed persistent lease")
		default:
			s.svc.log.Printf("resume %s: %v", id, err)
			writeError(w, http.StatusInternalServerError, "resume failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      lease.ID,
		"status":  "running",
		"address": lease.Address,
	})
}

// handleList returns the caller's leases.
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	leases := s.svc.list(owner)
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": leases})
}

// handleExec runs a command in a sandbox owned by the caller.
func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	// Per-owner activity cap (security review #37 rescan F9).
	if !s.acquireBusy(owner) {
		writeError(w, http.StatusTooManyRequests, "too many concurrent exec/stream operations; try again shortly")
		return
	}
	defer s.releaseBusy(owner)
	// Shared leases are executable over the API (T6/#33).
	lease := s.svc.lookupWithShare(owner, id, ShareHTTP)
	if lease == nil {
		writeError(w, http.StatusNotFound, "sandbox not found")
		return
	}
	s.svc.touch(id) // exec is activity for the idle sweeper
	// A suspended workspace-backed lease has no running sandbox; resume
	// first.
	if lease.Suspended {
		writeError(w, http.StatusConflict, "sandbox is suspended; resume it first")
		return
	}
	var req struct {
		Cmd     string            `json:"cmd"`
		Cwd     string            `json:"cwd"`
		Env     map[string]string `json:"env"`
		Timeout int               `json:"timeout"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Cmd == "" {
		writeError(w, http.StatusBadRequest, "cmd is required")
		return
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	if timeout > maxExecTimeout {
		timeout = maxExecTimeout
	}
	args := buildShellArgs(req.Cmd, req.Cwd, req.Env)
	start := time.Now()
	res, err := s.svc.forkd.Exec(r.Context(), lease.ForkdID, args, timeout)
	if err != nil {
		s.svc.log.Printf("exec: %s: %v (dur=%s)", lease.ForkdID, err, time.Since(start))
		// Map forkd 404 (sandbox killed/expired in the controller but the
		// lease still exists in our store) to 410 Gone so the caller can
		// distinguish a permanently dead sandbox from a transient exec
		// failure (e.g. controller overload, network blip).
		if fe, ok := err.(*forkd.Error); ok && fe.StatusCode == http.StatusNotFound {
			writeError(w, http.StatusGone, "sandbox no longer exists in controller")
			return
		}
		writeError(w, http.StatusInternalServerError, "exec failed")
		return
	}
	s.svc.log.Printf("exec: %s: exit=%d stdout=%d stderr=%d dur=%s", lease.ForkdID, res.ExitCode, len(res.Stdout), len(res.Stderr), time.Since(start))
	if res.ExitCode != 0 {
		s.svc.log.Printf("exec: %s: stderr=%q", lease.ForkdID, tailStr(res.Stderr, 500))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stdout": res.Stdout,
		"stderr": res.Stderr,
		"exit":   res.ExitCode,
	})
}

// handleStat returns lightweight guest-side metrics for a sandbox
// (ticket #25): vCPU load, memory, disk, and network RX/TX. Data comes
// from a one-shot exec probe (KTD5-style, stateless, 5s timeout) —
// zero controller changes; works for any image with procfs.
func (s *Server) handleStat(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	lease := s.svc.lookupWithShare(owner, id, ShareHTTP)
	if lease == nil {
		writeError(w, http.StatusNotFound, "sandbox not found")
		return
	}
	s.svc.touch(id)
	if lease.Suspended {
		writeError(w, http.StatusConflict, "sandbox is suspended; resume it first")
		return
	}
	const probe = `set -e
echo "== loadavg =="; cat /proc/loadavg
echo "== meminfo =="; grep -E '^(MemTotal|MemAvailable):' /proc/meminfo
echo "== netdev =="; cat /proc/net/dev
echo "== df =="; df -P /
`
	res, err := s.svc.forkd.Exec(r.Context(), lease.ForkdID, buildShellArgs(probe, "", nil), 5)
	if err != nil {
		s.svc.log.Printf("stat: %s: %v", lease.ForkdID, err)
		writeError(w, http.StatusInternalServerError, "stat probe failed")
		return
	}
	if res.ExitCode != 0 {
		s.svc.log.Printf("stat: %s: probe exit=%d stderr=%q", lease.ForkdID, res.ExitCode, tailStr(res.Stderr, 300))
		writeError(w, http.StatusInternalServerError, "stat probe exited non-zero")
		return
	}
	stat, perr := parseStatProbe(res.Stdout)
	if perr != nil {
		s.svc.log.Printf("stat: %s: parse: %v (stdout=%q)", lease.ForkdID, perr, tailStr(res.Stdout, 300))
		writeError(w, http.StatusInternalServerError, "stat probe parse failed")
		return
	}
	writeJSON(w, http.StatusOK, stat)
}

// statResult is the shaped /stat response.
type statResult struct {
	CPU struct {
		Load1 float64 `json:"load1"`
	} `json:"cpu"`
	Mem struct {
		UsedMiB  int64 `json:"used_mib"`
		TotalMiB int64 `json:"total_mib"`
	} `json:"mem"`
	Disk struct {
		UsedMiB  int64 `json:"used_mib"`
		TotalMiB int64 `json:"total_mib"`
	} `json:"disk"`
	Net struct {
		RXBytes int64 `json:"rx_bytes"`
		TXBytes int64 `json:"tx_bytes"`
	} `json:"net"`
}
```

**NOTE (handleExec):** default timeout 30s; cap `maxExecTimeout` (env `MAX_EXEC_TIMEOUT_SECS`, default 300). A forkd 404 is mapped to **410 Gone** (the runner relies on 410 meaning permanent). **NOTE (handleStat):** probe via `forkd.Exec` with a 5s timeout; the stdout sections are parsed by `parseStatProbe` (L1188-1248).

### 5.10 handleDelete, handleClone, handleImages, handleByName, buildShellArgs, writeJSON/writeError

`api/server.go` L1257-1387:

```go
// handleDelete releases a sandbox owned by the caller.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	lease := s.svc.lookup(owner, id)
	if lease == nil {
		writeError(w, http.StatusNotFound, "sandbox not found")
		return
	}
	s.svc.release(r.Context(), lease)
	w.WriteHeader(http.StatusNoContent)
}

// handleClone branches a running sandbox into a new snapshot tag and
// grants a fresh lease on the branch. Optional {"tag": "..."} names the
// branch; otherwise the controller auto-generates one. The clone is a
// persistent lease (the source's tmux state, filesystem, and installed
// packages carry over).
func (s *Server) handleClone(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	lease := s.svc.lookup(owner, id)
	if lease == nil {
		writeError(w, http.StatusNotFound, "sandbox not found")
		return
	}
	var req struct {
		Tag string `json:"tag"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // optional body

	// Branch the running sandbox to a new snapshot tag.
	tag := req.Tag
	if tag == "" {
		tag = "clone-" + lease.ID[:8] + "-" + strconv.FormatInt(time.Now().Unix(), 10)
	}
	newTag, err := s.svc.forkd.Branch(r.Context(), lease.ForkdID, tag)
	if err != nil {
		s.svc.log.Printf("clone %s: branch: %v", id, err)
		writeError(w, http.StatusInternalServerError, "failed to branch sandbox")
		return
	}

	// Spawn a sandbox from the branch and grant a lease on it.
	ttl := s.svc.maxTTL
	cloned, err := s.svc.grantFromSnapshot(r.Context(), owner, newTag, ttl, true)
	if err != nil {
		s.svc.log.Printf("clone %s: grant from %s: %v", id, newTag, err)
		// Quota enforcement (security review #37 rescan F1): clone must
		// surface the same 429 as create, not a generic 500.
		if err == errQuotaExceeded {
			writeError(w, http.StatusTooManyRequests, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to spawn clone")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         cloned.ID,
		"image":      cloned.Image,
		"source":     id,
		"branch_tag": newTag,
		"persistent": cloned.Persistent,
		"expires_at": cloned.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// handleImages lists available image tags.
func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	tags, err := s.reg.Tags(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "image registry unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"images": tags})
}

// handleByName resolves a friendly lease name owned by the caller to its
// lease id. Used by the SSH gateway (username = name) and for
// script/LLM convenience. Owner-scoped: names are unique per owner, so
// a caller can only resolve their own.
func (s *Server) handleByName(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	lease := s.svc.lookupByNameForOwner(ownerFrom(r.Context()), name)
	if lease == nil {
		writeError(w, http.StatusNotFound, "no sandbox named "+name)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":    lease.ID,
		"name":  lease.Name,
		"image": lease.Image,
	})
}

// buildShellArgs wraps a command with cwd/env into a single shell
// invocation, since forkd's exec takes argv and no cwd/env. Both env
// keys and values are shell-quoted so a hostile key cannot inject
// shell metacharacters.
func buildShellArgs(cmd, cwd string, env map[string]string) []string {
	var parts []string
	if cwd != "" {
		parts = append(parts, "cd "+shellQuote(cwd)+" &&")
	}
	for k, v := range env {
		parts = append(parts, "export "+shellQuote(k)+"="+shellQuote(v)+";")
	}
	parts = append(parts, cmd)
	// Use bash, not sh. GitHub Actions / Forgejo wrap `run:` steps with
	// `set -euo pipefail`; /bin/sh on Debian is dash, which rejects
	// `-o pipefail` (dash only accepts `-o` options in POSIX form), so
	// steps that contain a pipe fail with "set: Illegal option -o
	// pipefail". Bash is present in all base images and is the GitHub
	// Actions default shell.
	return []string{"/bin/bash", "-c", strings.Join(parts, " ")}
}

// shellQuote wraps s in single quotes, escaping embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
```

`ImageRegistry` (api/images.go) is the image validation used by `handleCreate` and `handleImages`:

`api/images.go` L1-60:

```go
package api

import (
	"context"
)

// ImageRegistry validates requested image tags against forkd-controller
// and lists available tags. It uses the per-tag info endpoint for
// validation (reliable even when the list endpoint is empty) and the
// list endpoint for discovery, falling back to a configured known-tags
// set when the list is empty.
type ImageRegistry struct {
	forkd ForkdClient

	// known is an optional static set of tags to surface when the
	// controller's list endpoint returns nothing.
	known map[string]bool
}

// NewImageRegistry returns a registry backed by forkd-controller.
// knownTags is an optional static set surfaced when the controller list
// is empty.
func NewImageRegistry(fc ForkdClient, knownTags ...string) *ImageRegistry {
	known := make(map[string]bool, len(knownTags))
	for _, t := range knownTags {
		known[t] = true
	}
	return &ImageRegistry{
		forkd: fc,
		known: known,
	}
}

// Has reports whether tag is a known, bootable snapshot. It checks the
// per-tag info endpoint directly so validation is always current.
func (r *ImageRegistry) Has(ctx context.Context, tag string) (bool, error) {
	return r.forkd.SnapshotExists(ctx, tag)
}

// Tags returns the current set of known tags. It prefers the
// controller's list endpoint and falls back to the configured known set
// when the list is empty.
func (r *ImageRegistry) Tags(ctx context.Context) ([]string, error) {
	snaps, err := r.forkd.ListSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(snaps)+len(r.known))
	for _, s := range snaps {
		seen[s.Tag] = true
	}
	for t := range r.known {
		seen[t] = true
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	return out, nil
}
```

### 5.11 shares.go request/response shapes

`api/shares.go` L10-41:

```go
// shareView is the JSON shape of a share.
type shareView struct {
	LeaseID   string `json:"lease_id"`
	Grantee   string `json:"grantee"`
	Mode      string `json:"mode"`
	ExpiresAt string `json:"expires_at,omitempty"`
	CreatedAt string `json:"created_at"`
}

func toShareView(sh *Share) shareView {
	v := shareView{
		LeaseID:   sh.LeaseID,
		Grantee:   sh.Grantee,
		Mode:      string(sh.Mode),
		CreatedAt: sh.CreatedAt.UTC().Format(time.RFC3339),
	}
	if !sh.ExpiresAt.IsZero() {
		v.ExpiresAt = sh.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return v
}

// handleShareGrant shares a lease with another user (T6/#33).
// Owner-only. Body: {"grantee":"<user-id>","mode":"ssh|http","ttl":<seconds>}
func (s *Server) handleShareGrant(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	var req struct {
		Grantee string `json:"grantee"`
		Mode    string `json:"mode"`
		TTL     int    `json:"ttl"` // seconds; 0 = never expires
	}
```

### 5.12 LLM gateway entry (lease-id capability; no forkd use)

`api/llmgateway.go` L22-27:

```go
// llmGatewayPrefix is the path prefix for the per-lease LLM gateway.
// Guests reach it at http(s)://<host>:<port>/llm/<lease-id>/<provider>/...
// The lease id in the path is the capability (same model as the SSH
// gateway and the HTTP proxy hostname): a guest that knows its lease id
// can use the gateway, and the real provider key never enters the VM.
const llmGatewayPrefix = "/llm/"
```

`api/llmgateway.go` L135-156:

```go
func (g *llmGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, llmGatewayPrefix)
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		http.Error(w, "usage: /llm/<lease-id>/<provider>/...", http.StatusBadRequest)
		return
	}
	leaseID := rest[:slash]
	sub := rest[slash:] // e.g. "/openai/chat/completions"

	lease := g.lookup(leaseID)
	start := time.Now()
	provider := "unknown"
	if lease == nil {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	if lease.Suspended {
		http.Error(w, "sandbox is suspended; resume it first", http.StatusConflict)
		return
	}

```

The gateway looks up the lease with `svc.lookupAny`. It never touches the guest; guests call **it**, at `http://10.43.0.1:8891/llm/<lease-id>/...` (the proxy listener, §6.3).

---

## 6. Network layer: api/netpolicy.go, api/netns_linux.go (+ netns_other.go), api/proxy.go

### 6.1 api/netpolicy.go (full)

`api/netpolicy.go` L1-257:

```go
package api

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strings"
)

// NetworkPolicy is a sandbox's egress policy, enforced with iptables
// FORWARD rules inside the sandbox's child network namespace. This is a
// service-layer concern (hexagonal): the controller only hands out a
// netns per sandbox; spoond decides what may leave it.
type NetworkPolicy string

const (
	PolicyNone       NetworkPolicy = "none"       // no egress at all
	PolicyLAN        NetworkPolicy = "lan"        // RFC1918 + link-local only (default)
	PolicyInternet   NetworkPolicy = "internet"   // full egress (host NAT applies)
	PolicyRestricted NetworkPolicy = "restricted" // allowlisted IPs/CIDRs/domains only
)

// ValidNetworkPolicy reports whether p is a known policy name.
func ValidNetworkPolicy(p string) bool {
	switch NetworkPolicy(p) {
	case PolicyNone, PolicyLAN, PolicyInternet, PolicyRestricted:
		return true
	}
	return false
}

// PolicyApplier is the port for enforcing a policy inside a netns. The
// production implementation shells to ip netns exec + iptables; tests
// inject a fake that records calls.
type PolicyApplier interface {
	Apply(ctx context.Context, netns string, policy NetworkPolicy, allowlist []string) error
}

// NetnsPolicyApplier enforces policies with iptables FORWARD rules inside
// the given network namespace. Idempotent: it flushes the FORWARD chain
// first (the chain is per-sandbox and the netns pool is reused), then
// installs the rules for the requested policy.
type NetnsPolicyApplier struct {
	// DNSAllowlist is always permitted under PolicyRestricted so the
	// guest can still resolve names the allowlist was built from.
	DNSAllowlist []string
}

func (a *NetnsPolicyApplier) Apply(ctx context.Context, netns string, policy NetworkPolicy, allowlist []string) error {
	if netns == "" {
		return fmt.Errorf("no netns to apply policy to")
	}
	cmds := policyCommands(policy, allowlist, a.DNSAllowlist)
	for _, args := range cmds {
		full := append([]string{"netns", "exec", netns, "iptables"}, args...)
		out, err := exec.CommandContext(ctx, "ip", full...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("iptables in netns %s: %v: %s", netns, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// policyCommands returns the iptables args to enforce policy in a netns.
// FORWARD chain governs guest↔outside traffic (tap0 ↔ veth0); the chain
// is flushed first so rules are idempotent across netns pool reuse.
func policyCommands(policy NetworkPolicy, allowlist, dnsAllow []string) [][]string {
	flush := []string{"-F", "FORWARD"}
	switch policy {
	case PolicyNone:
		return [][]string{
			flush,
			{"-A", "FORWARD", "-m", "comment", "--comment", "forkd-netpolicy:none", "-j", "DROP"},
		}
	case PolicyLAN:
		return [][]string{
			flush,
			{"-A", "FORWARD", "-m", "state", "--state", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
			{"-A", "FORWARD", "-d", "10.0.0.0/8", "-j", "ACCEPT"},
			{"-A", "FORWARD", "-d", "172.16.0.0/12", "-j", "ACCEPT"},
			{"-A", "FORWARD", "-d", "192.168.0.0/16", "-j", "ACCEPT"},
			{"-A", "FORWARD", "-m", "comment", "--comment", "forkd-netpolicy:lan", "-j", "DROP"},
		}
	case PolicyInternet:
		return [][]string{flush} // default FORWARD policy is ACCEPT
	case PolicyRestricted:
		var cmds [][]string
		cmds = append(cmds, flush)
		cmds = append(cmds, []string{"-A", "FORWARD", "-m", "state", "--state", "ESTABLISHED,RELATED", "-j", "ACCEPT"})
		// Always let the guest reach the configured resolvers.
		for _, dns := range dnsAllow {
			cmds = append(cmds, []string{"-A", "FORWARD", "-p", "udp", "--dport", "53", "-d", dns, "-j", "ACCEPT"})
			cmds = append(cmds, []string{"-A", "FORWARD", "-p", "tcp", "--dport", "53", "-d", dns, "-j", "ACCEPT"})
		}
		for _, entry := range allowlist {
			for _, ip := range resolveEntry(entry) {
				cmds = append(cmds, []string{"-A", "FORWARD", "-d", ip, "-j", "ACCEPT"})
			}
		}
		cmds = append(cmds, []string{"-A", "FORWARD", "-m", "comment", "--comment", "forkd-netpolicy:restricted", "-j", "DROP"})
		return cmds
	}
	return nil
}

// resolveEntry turns an allowlist entry into one or more IPs/CIDRs.
// CIDRs and bare IPs pass through; domain names are resolved with the
// host resolver (IPv4 first — IPv6 is not provisioned on the bridge).
func resolveEntry(entry string) []string {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return nil
	}
	if ip := net.ParseIP(entry); ip != nil {
		return []string{entry}
	}
	if _, _, err := net.ParseCIDR(entry); err == nil {
		return []string{entry}
	}
	ips, err := net.LookupIP(entry)
	if err != nil || len(ips) == 0 {
		return nil
	}
	var out []string
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			out = append(out, v4.String())
		}
	}
	return out
}

// Inbound port exposure — a lease can publish guest TCP ports on its
// bridge-facing address so OTHER sandboxes (and the host) can reach a
// service it runs: a CI job's database, for one. Everything else about the
// netns stays egress-only.
//
// The shape inside the lease's netns:
//
//	nat    PREROUTING -i veth0 --dport P  → DNAT guest:P
//	filter FORWARD    -i veth0 → tap  -d guest --dport P  ACCEPT   (inserted
//	filter FORWARD    -i tap → veth0  -s guest --sport P  ESTABLISHED ACCEPT
//	                                                        above the policy)
//
// so a published port answers its own connections and nothing more: under
// PolicyNone the guest still cannot open a connection of its own.
//
// Reachability is bounded by the bridge, not by this code: veth0 sits on
// forkd-br0 (10.43.0.0/16), which the LAN cannot route into, so a published
// port is visible to the host and to sandboxes whose own policy lets them
// reach 10.43.0.0/16 (lan, internet, or an allowlisted restricted lease).

const (
	netnsUplink = "veth0"      // bridge-facing interface in every child netns
	netnsTap    = "forkd-tap0" // guest-facing interface in every child netns
	// MaxExposedPorts bounds one lease's published ports.
	MaxExposedPorts = 8
)

// reservedGuestPorts can never be published: the guest agent (:8888) is an
// unauthenticated root exec endpoint and Shelley (:9000) an agent loop.
// Publishing either would hand every bridge peer a root shell.
var reservedGuestPorts = map[int]string{8888: "the guest exec agent", 9000: "the Shelley agent"}

// ValidateExposePorts checks a requested port list: 1..65535, no reserved
// port, no duplicates, at most MaxExposedPorts. It returns the list sorted.
func ValidateExposePorts(ports []int) ([]int, error) {
	if len(ports) > MaxExposedPorts {
		return nil, fmt.Errorf("at most %d exposed ports", MaxExposedPorts)
	}
	seen := map[int]bool{}
	out := make([]int, 0, len(ports))
	for _, p := range ports {
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("exposed port %d is out of range", p)
		}
		if what, reserved := reservedGuestPorts[p]; reserved {
			return nil, fmt.Errorf("port %d is %s and cannot be exposed", p, what)
		}
		if seen[p] {
			return nil, fmt.Errorf("exposed port %d is listed twice", p)
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Ints(out)
	return out, nil
}

// PortExposer is the optional capability of a PolicyApplier that publishes
// guest ports. Expose MUST be called on every (re)application, with an empty
// list too: the netns pool is reused, and a previous lease's DNAT rules would
// otherwise survive into the next tenant. It returns the netns's
// bridge-facing IP — the host part of every published address.
type PortExposer interface {
	Expose(ctx context.Context, netns, guestHost string, ports []int) (bridgeIP string, err error)
}

// Expose implements PortExposer with iptables inside the netns. It runs
// AFTER Apply, which flushed FORWARD; the accepts are inserted at the top so
// they precede the policy's final DROP.
func (a *NetnsPolicyApplier) Expose(ctx context.Context, netns, guestHost string, ports []int) (string, error) {
	if netns == "" {
		return "", fmt.Errorf("no netns to expose ports in")
	}
	for _, args := range exposeCommands(guestHost, ports) {
		full := append([]string{"netns", "exec", netns, "iptables"}, args...)
		out, err := exec.CommandContext(ctx, "ip", full...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("iptables in netns %s: %v: %s", netns, err, strings.TrimSpace(string(out)))
		}
	}
	out, err := exec.CommandContext(ctx, "ip", "-n", netns, "-4", "-o", "addr", "show", "dev", netnsUplink).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("read %s address in netns %s: %v: %s", netnsUplink, netns, err, strings.TrimSpace(string(out)))
	}
	ip := parseIPv4Addr(string(out))
	if ip == "" {
		return "", fmt.Errorf("no IPv4 address on %s in netns %s", netnsUplink, netns)
	}
	return ip, nil
}

// exposeCommands returns the iptables args that publish ports. The first
// command always flushes nat PREROUTING — the only user of that chain in a
// child netns — so a reused netns starts clean even when ports is empty.
func exposeCommands(guestHost string, ports []int) [][]string {
	cmds := [][]string{{"-t", "nat", "-F", "PREROUTING"}}
	for _, p := range ports {
		port := fmt.Sprint(p)
		cmds = append(cmds,
			[]string{"-t", "nat", "-A", "PREROUTING", "-i", netnsUplink, "-p", "tcp", "--dport", port,
				"-m", "comment", "--comment", "forkd-expose", "-j", "DNAT", "--to-destination", guestHost + ":" + port},
			[]string{"-I", "FORWARD", "1", "-i", netnsUplink, "-o", netnsTap, "-p", "tcp", "-d", guestHost, "--dport", port,
				"-m", "comment", "--comment", "forkd-expose", "-j", "ACCEPT"},
			[]string{"-I", "FORWARD", "1", "-i", netnsTap, "-o", netnsUplink, "-p", "tcp", "-s", guestHost, "--sport", port,
				"-m", "state", "--state", "ESTABLISHED", "-m", "comment", "--comment", "forkd-expose", "-j", "ACCEPT"},
		)
	}
	return cmds
}

// parseIPv4Addr pulls the address out of `ip -4 -o addr show` output
// ("3: veth0    inet 10.43.0.10/16 brd … scope global veth0").
func parseIPv4Addr(out string) string {
	f := strings.Fields(out)
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "inet" {
			if ip, _, err := net.ParseCIDR(f[i+1]); err == nil && ip.To4() != nil {
				return ip.String()
			}
		}
	}
	return ""
}
```

**Policy semantics and rules** (every command runs as `ip netns exec <netns> iptables <args...>`, in the forkd child netns; the chain is FORWARD, between guest tap `forkd-tap0` and uplink `veth0`):

| Policy | Rules (in order, after `-F FORWARD`) |
|---|---|
| `none` | `-A FORWARD -m comment --comment forkd-netpolicy:none -j DROP` |
| `lan` | ESTABLISHED,RELATED ACCEPT; `-d 10.0.0.0/8` ACCEPT; `-d 172.16.0.0/12` ACCEPT; `-d 192.168.0.0/16` ACCEPT; `forkd-netpolicy:lan` DROP |
| `internet` | flush only (default FORWARD policy ACCEPT; host NAT does egress) |
| `restricted` | ESTABLISHED,RELATED ACCEPT; for each `NETPOL_DNS` resolver: udp and tcp dport 53 `-d dns` ACCEPT; for each allowlist entry (IP, CIDR, or domain resolved on the **host** to IPv4 at apply time): `-d ip` ACCEPT; `forkd-netpolicy:restricted` DROP |

- **LLM gateway reach:** guests use `http://10.43.0.1:8891/llm/<lease-id>/...` and `/assets/...` (the host bridge IP `forkd-br0` 10.43.0.1, proxy listener `PROXY_ADDR`, deployed as `:8891`). Under `restricted`, `handleCreate` appends `10.43.0.1` to the allowlist (`hostBridgeAllow`, server.go L508). Under `lan`, 10.43.0.1 falls inside `10.0.0.0/8`. **NOTE:** traffic to the host's own bridge IP may traverse INPUT in the root netns rather than FORWARD in the child netns, depending on forkd's topology; the child netns FORWARD chain sees it because the guest sits behind `forkd-tap0` in the child netns and the child routes to the bridge via `veth0`.
- **Bridge and CIDRs:** host bridge `forkd-br0` = `10.43.0.0/16`, host side `10.43.0.1`. A child netns's `veth0` holds a `10.43.x.y/16` address, e.g. `10.43.0.10`, which is the `ExposedIP`. The guest address is `10.42.0.2`, with the agent on `:8888`; `Lease.Address` example `"10.42.0.2:8888"`. The scylla hook hardcodes `--broadcast-rpc-address=10.42.0.2`. **Every child uses the same guest IP 10.42.0.2**, isolated by netns. This is inferred from the example, the scylla hook and the tests asserting "10.42" in the hostname; the controller that assigns it is not in this repo.
- **expose_ports:** validated by `ValidateExposePorts` (max 8, range 1..65535, reserved 8888 and 9000, no duplicates, sorted). `Expose` always flushes `nat PREROUTING`, then per port adds a DNAT `veth0:P → guestHost:P` plus two FORWARD accepts inserted at position 1 (comment `forkd-expose`), then reads `veth0`'s IPv4 via `ip -n <netns> -4 -o addr show dev veth0`. The published address is `<veth0 IP>:<port>` in the `exposed` map. It is reachable from the host and from sandboxes whose own policy can reach `10.43.0.0/16`. A create with `expose_ports` returns **501** unless `NETPOL_DNS` is set (because `CanExposePorts` requires the `NetnsPolicyApplier`).
- Reserved ports: `8888` (guest exec agent) and `9000` (Shelley).
- Enforcement is enabled only when the env `NETPOL_DNS` is non-empty (§8). With it empty, `netpol` is nil: no policy is enforced and no expose is possible.

### 6.2 api/netns_linux.go and api/netns_other.go (full)

`api/netns_linux.go` L1-43:

```go
//go:build linux

package api

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/sys/unix"
)

// dialInNetns enters the named network namespace and dials addr.
// Used to connect to the forkd-agent (port 8888) inside a sandbox VM
// from the host. The netns must exist under /var/run/netns/.
func dialInNetns(netns, addr string) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		f, err := os.Open(filepath.Join("/var/run/netns", netns))
		if err != nil {
			ch <- result{nil, fmt.Errorf("open netns %s: %w", netns, err)}
			return
		}
		defer f.Close()
		if err := unix.Setns(int(f.Fd()), unix.CLONE_NEWNET); err != nil {
			ch <- result{nil, fmt.Errorf("setns %s: %w", netns, err)}
			return
		}
		d, err := net.DialTimeout("tcp", addr, 10*time.Second)
		ch <- result{d, err}
	}()
	r := <-ch
	return r.conn, r.err
}
```

`api/netns_other.go` L1-16:

```go
//go:build !linux

package api

import (
	"fmt"
	"net"
)

// dialInNetns is a stub on non-Linux platforms. The real implementation
// (netns_linux.go) uses unix.Setns/CLONE_NEWNET which are Linux-only.
// This stub allows the api package to compile on macOS for testing;
// it is never called in production (spoond runs on Linux).
func dialInNetns(netns, addr string) (net.Conn, error) {
	return nil, fmt.Errorf("dialInNetns: not supported on this platform (netns=%s addr=%s)", netns, addr)
}
```

### 6.3 api/proxy.go (full)

`api/proxy.go` L1-342:

```go
package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// proxyHostSuffix is the wildcard hostname suffix for the HTTP proxy.
// Caddy terminates TLS for *.sandbox.example.com and forwards here.
const proxyHostSuffix = ".sandbox.example.com"

// defaultProxyPort is the guest port used when the hostname carries none.
// exe.dev uses the Dockerfile EXPOSE port; we have no Dockerfiles, so the
// convention is port 3000 unless the caller names another via
// <lease-id>-<port>.sandbox.example.com.
const defaultProxyPort = 3000

// ProxyHandler returns the HTTP handler for the public proxy listener
// (plain HTTP on an internal port; Caddy fronts it with wildcard TLS).
// Every request's Host header names a lease: <lease-id>.sandbox.example.com
// → guest:3000, <lease-id>-<port>.sandbox.example.com → guest:<port>.
// The lease id in the hostname is the capability (same model as SSH).
//
// Under forward-auth (U7/T7) the capability model is replaced: the
// proxy requires X-Proxy-Auth == the shared secret and resolves the
// authenticated user from Remote-User, then owner-scopes every lookup.
func (s *Server) ProxyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The LLM gateway also lives on the plain-HTTP proxy listener:
		// guests reach it at http://10.43.0.1:8891/llm/<lease-id>/...,
		// avoiding TLS validation of the backend's self-signed cert.
		if s.llm != nil && strings.HasPrefix(r.URL.Path, llmGatewayPrefix) {
			s.llm.ServeHTTP(w, r)
			return
		}
		// Static assets (e.g. the shelley agent binary) served to guests
		// at http://10.43.0.1:8891/assets/<file>. This is how a lease
		// fetches tooling that is too big for the exec API cmdline.
		if s.assetsDir != "" && strings.HasPrefix(r.URL.Path, "/assets/") {
			// Containment (security review #37 rescan): never rely on the
			// stdlib's incidental dot-dot rejection for a host-filesystem
			// read on an unauthenticated path. Resolve inside assetsDir
			// and refuse anything that escapes it.
			rel := strings.TrimPrefix(r.URL.Path, "/assets/")
			p := filepath.Join(s.assetsDir, filepath.FromSlash(rel))
			if !strings.HasPrefix(p, filepath.Clean(s.assetsDir)+string(filepath.Separator)) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			http.ServeFile(w, r, p)
			return
		}
		// Forward-auth gate (U7/T7): off/"") = capability model.
		if s.proxyAuthMode == "forward-auth" {
			if !s.proxyAuthOK(w, r) {
				return
			}
		}
		s.handleProxy(w, r)
	})
}

// ctxProxyOwnerKey carries the authenticated proxy owner (user id or
// legacy consumer name) resolved by the forward-auth gate. The inbound
// Remote-User header is never read again after this.
type ctxProxyOwnerKey struct{}

func proxyOwnerFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxProxyOwnerKey{}).(string)
	return v
}

// proxyAuthOK implements the forward-auth gate: shared-secret check
// (constant-time), Remote-User presence, and identity resolution. It
// stashes the resolved owner in the request context.
func (s *Server) proxyAuthOK(w http.ResponseWriter, r *http.Request) bool {
	secret := r.Header.Get("X-Proxy-Auth")
	if secret == "" || subtle.ConstantTimeCompare([]byte(secret), []byte(s.proxyAuthSecret)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	user := strings.TrimSpace(r.Header.Get("Remote-User"))
	if user == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	// Trusted-peer enforcement (security review #37 M3): when peers are
	// configured, only a trusted reverse proxy may present Remote-User.
	// A direct client to the listener can't spoof an identity even with
	// the shared secret.
	if len(s.proxyTrustedPeers) > 0 && !s.peerTrusted(r.RemoteAddr) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	owner := user // legacy single-user: Remote-User is the owner directly
	if s.svc.identities != nil {
		u := s.svc.identities.UserByName(user)
		if u == nil {
			http.Error(w, "unknown user", http.StatusForbidden)
			return false
		}
		owner = u.ID
	}
	*r = *r.WithContext(context.WithValue(r.Context(), ctxProxyOwnerKey{}, owner))
	return true
}

// peerTrusted reports whether a RemoteAddr host is in the trusted-peer
// set (security review #37 M3).
func (s *Server) peerTrusted(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range s.proxyTrustedPeers {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// SetAssetsDir enables static asset serving on the proxy listener.
func (s *Server) SetAssetsDir(dir string) { s.assetsDir = dir }

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	label, hostUser, port, ok := parseProxyHost2(r.Host)
	if !ok {
		http.Error(w, "unknown sandbox hostname", http.StatusNotFound)
		return
	}
	var lease *Lease
	// Under forward-auth, the authenticated owner scopes every lookup
	// (U7/T7): a user only ever reaches their own leases, by id or
	// friendly name. A per-user hostname segment (<label>.<user>...) must
	// match the authenticated owner. In the capability model (off) the
	// hostname is the credential and owner-blind lookups are used.
	if owner := proxyOwnerFrom(r.Context()); owner != "" {
		if hostUser != "" {
			// Resolve the user segment to an identity id and require
			// it to be the authenticated owner (no cross-user URLs).
			// With no identity store (legacy single-user) the segment
			// must equal the raw Remote-User owner.
			if s.svc.identities == nil {
				if hostUser != owner {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
			} else {
				hu := s.svc.identities.UserByName(hostUser)
				if hu == nil || hu.ID != owner {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
			}
		}
		lease = s.svc.lookupUserScoped(owner, label)
	} else if len(label) == 32 && isHex(label) {
		// Capability model: ONLY the unguessable 32-hex lease id is a
		// credential. Friendly names are NOT capabilities (they're
		// guessable: "web", "demo", "api"...) and resolving them
		// cross-tenant here would expose every tenant's sandbox to
		// anyone on the network (security review #37 rescan F4).
		// Friendly-name routing requires forward-auth, where lookups
		// are owner-scoped.
		lease = s.svc.lookupAny(label)
	} else if s.svc.identities == nil && label != "" {
		// Legacy single-user mode (no identity store): there is only
		// one tenant, so a friendly name carries no cross-tenant
		// exposure. Keep the old behavior for these deployments.
		lease = s.svc.lookupByName(label)
	} else {
		http.Error(w, "unknown sandbox hostname", http.StatusNotFound)
		return
	}
	if lease == nil {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	s.svc.touch(lease.ID) // proxied web traffic is activity for the idle sweeper
	ep, err := s.svc.resolveEndpoint(r.Context(), lease)
	if err != nil {
		http.Error(w, "sandbox not running", http.StatusBadGateway)
		return
	}
	target := net.JoinHostPort(ep.GuestHost, strconv.Itoa(port))

	// Reverse proxy with a Transport that dials inside the sandbox netns
	// (the guest IP is only reachable from the host via setns). Both HTTP
	// and WebSocket upgrades work through this.
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: target})
			// Preserve the original Host so sandbox apps see the public
			// hostname (virtual hosting works as expected).
			pr.Out.Host = pr.In.Host
			// Security review #37 rescan F2: NEVER forward the forward-auth
			// gate headers to the guest app. Guests are tenant-controlled;
			// if X-Proxy-Auth (the shared secret with Caddy) or Remote-User
			// reached them, any tenant could harvest the secret and
			// impersonate anyone through the proxy. Strip all gate/auth
			// headers here (Caddy also strips on ingress — defense in
			// depth, spoond must too since :8891 is directly reachable).
			for _, h := range []string{"X-Proxy-Auth", "Remote-User", "X-Spoond-User-Id", "X-Bootstrap-Token"} {
				pr.Out.Header.Del(h)
			}
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				// ReverseProxy gives us the target addr; we dial it inside
				// the lease's netns. dialInNetns ignores addr and dials
				// target directly (bound in the guest netns).
				return dialInNetns(ep.Netns, target)
			},
			IdleConnTimeout: 30 * time.Second,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return
			}
			http.Error(w, "proxy error: "+err.Error(), http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

// parseProxyHost extracts a lease id and guest port from a proxy Host
// header. Accepted forms:
//
//	<32-hex-lease-id>.sandbox.example.com        → port 3000
//	<32-hex-lease-id>-<port>.sandbox.example.com → that port
//
// Returns ok=false for anything else (including the bare apex hostname).
func parseProxyHost(host string) (leaseID string, port int, ok bool) {
	h := strings.ToLower(strings.TrimSpace(host))
	// Strip any explicit :port from the Host header (rare on 443, cheap).
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.HasSuffix(h, "]") {
		if _, err := strconv.Atoi(h[i+1:]); err == nil {
			h = h[:i]
		}
	}
	if !strings.HasSuffix(h, proxyHostSuffix) {
		return "", 0, false
	}
	label := strings.TrimSuffix(h, proxyHostSuffix)
	if label == "" {
		return "", 0, false
	}
	if i := strings.LastIndexByte(label, '-'); i > 0 {
		if p, err := strconv.Atoi(label[i+1:]); err == nil && p > 0 && p < 65536 {
			return label[:i], p, true
		}
		// A bad port suffix on a 32-hex id is a malformed proxy URL, not
		// a name. A non-id prefix is just a hyphenated name candidate.
		if len(label[:i]) == 32 && isHex(label[:i]) {
			return "", 0, false
		}
	}
	// No port suffix: the label is either a 32-hex lease id or a friendly
	// name assigned via the tag endpoint. Both resolve to a lease.
	if !isValidLabel(label) {
		return "", 0, false
	}
	return label, defaultProxyPort, true
}

// parseProxyHost2 is the U7/T7 extension of parseProxyHost. It accepts
// the legacy single-label form plus the per-user form:
//
//	<label>.sandbox.example.com              → user "" (any/legacy)
//	<label>.<user>.sandbox.example.com       → user <user>
//
// Returns user="" when the hostname has no user segment. The caller
// (handleProxy) decides whether the user segment is allowed for the
// authenticated owner.
func parseProxyHost2(host string) (label, user string, port int, ok bool) {
	h := strings.ToLower(strings.TrimSpace(host))
	// Strip any explicit :port from the Host header (rare on 443, cheap).
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.HasSuffix(h, "]") {
		if _, err := strconv.Atoi(h[i+1:]); err == nil {
			h = h[:i]
		}
	}
	if !strings.HasSuffix(h, proxyHostSuffix) {
		return "", "", 0, false
	}
	pre := strings.TrimSuffix(h, proxyHostSuffix)
	if pre == "" {
		return "", "", 0, false
	}
	// Two-label form: label.<user>.
	if i := strings.IndexByte(pre, '.'); i >= 0 {
		label, user = pre[:i], pre[i+1:]
		if !isValidLabel(label) || !isValidLabel(user) {
			return "", "", 0, false
		}
		return label, user, defaultProxyPort, true
	}
	// Single-label form: delegate to parseProxyHost (port suffix etc.).
	label, port, ok = parseProxyHost(host)
	if !ok {
		return "", "", 0, false
	}
	return label, "", port, true
}

// isValidLabel accepts a 32-hex lease id or a friendly name
// ([a-z0-9][a-z0-9-]{0,62}, no dots — the suffix owns the dots).
func isValidLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i, c := range s {
		ok := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' && i > 0
		if !ok {
			return false
		}
	}
	return true
}

func isHex(s string) bool {
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}
```

**Proxy summary:**
- Listener: `PROXY_ADDR`, plain HTTP; Caddy terminates TLS for `*.sandbox.example.com`.
- On the same listener: `/llm/` goes to the LLM gateway, and `/assets/` serves files from `ASSETS_DIR`.
- Host formats:
  - `<32hex-lease-id>.sandbox.example.com` → port 3000
  - `<label>-<port>.sandbox.example.com` → that port
  - `<label>.<user>.sandbox.example.com` → port 3000 (no port form with a user segment)
  - `label` = a 32-hex id or a friendly name.
- Lookups:
  - Under forward-auth: owner-scoped `lookupUserScoped`.
  - Capability mode: only a 32-hex id, via `lookupAny`.
  - Legacy with no identity store: friendly name via `lookupByName`.
- Dial: `resolveEndpoint` (forkd `ListSandboxes`) → `target = GuestHost:port`, dialed with `dialInNetns(ep.Netns, target)` from a custom `http.Transport.DialContext`. `ReverseProxy` preserves the Host header and strips `X-Proxy-Auth`, `Remote-User`, `X-Spoond-User-Id`, `X-Bootstrap-Token`. WebSocket upgrades pass through.
- No `Suspended` check: a suspended lease fails `resolveEndpoint` and returns **502** "sandbox not running".

---

## 7. cmd/spoond-sshd-gateway/main.go (1530 lines, `//go:build linux`)

### 7.1 Flags and env

`cmd/spoond-sshd-gateway/main.go` L45-110:

```go
// envOr returns the value of env key or def when unset/empty. Used for
// flag defaults so the same knobs are settable via environment
// (FORKD_GATEWAY_HOST, SHELLY_BINARY_URL, LLM_GATEWAY_URL).
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var (
	// flags is the gateway's private FlagSet; it stays scoped to this
	// package so the spoond umbrella binary can link every command
	// without flag-name collisions.
	flags = flag.NewFlagSet("spoond-gateway", flag.ExitOnError)

	// gatewayHost is the public hostname advertised in MOTDs.
	gatewayHost = flags.String("gateway-host", envOr("FORKD_GATEWAY_HOST", "sandbox.example.com"), "public hostname advertised in MOTDs")
	listenAddr  = flags.String("listen", ":2222", "listen address")
	hostKeyPath = flags.String("host-key", "/etc/spoond-gateway/ssh_host_ed25519_key", "path to SSH host key (generated if missing)")
	backendURL  = flags.String("backend", "https://127.0.0.1:8890", "spoond-backend base URL")
	// backendTok: the spoond-backend consumer token (required, admin-equivalent).
	// Read from --backend-token flag or SPOOND_GATEWAY_TOKEN env so the
	// value never needs to appear in ExecStart (security review #37 rescan
	// F7: /proc/<pid>/cmdline must not expose the token).
	backendTok = flags.String("backend-token", envOr("SPOOND_GATEWAY_TOKEN", ""), "spoond-backend consumer token (required; or $SPOOND_GATEWAY_TOKEN)")
	// DEPRECATED/ignored (security review #37 rescan F7): the gateway
	// must NOT forward the bootstrap token — bootstrap is an operator
	// action against the backend directly. The flag is accepted so
	// existing units keep working after upgrade.
	bootstrapTok = flags.String("bootstrap-token", "", "DEPRECATED: ignored; bootstrap via direct backend API call")
	clientKeys   = flags.String("client-keys", "", "comma-separated paths to authorized client public keys, or a directory scanned for *.pub files")
	// gatewayKeyPath is the identity the gateway uses to connect INTO
	// sandboxes. Its public half is baked into dev-base authorized_keys.
	gatewayKeyPath = flags.String("gateway-key", "/etc/spoond-gateway/gateway_ed25519", "gateway identity key for nested connections")
	// shellyBinaryURL is where the `shelly` ctl verb fetches the agent
	// binary from inside the sandbox (host-side asset server on the
	// plain-HTTP proxy listener; guests reach it via forkd-br0).
	shellyBinaryURL = flags.String("shelly-binary-url", envOr("SHELLY_BINARY_URL", "http://10.43.0.1:8891/assets/shelley"), "URL the sandbox fetches the shelley binary from")
	// llmGatewayURL is the per-lease LLM gateway base the shelley agent
	// is pointed at (host-side proxy listener; guests reach it via
	// forkd-br0). The lease id is appended.
	llmGatewayURL = flags.String("llm-gateway-url", envOr("LLM_GATEWAY_URL", "http://10.43.0.1:8891/llm/"), "base URL of the per-lease LLM gateway (lease id appended)")
	// shellyModel is the default model id written into shelley.json. It
	// must be an id the LLM gateway's LLM_MODEL_MAP understands (the
	// exe.dev catalog id, not the upstream id).
	shellyModel = flags.String("shelly-model", "gpt-oss-20b-fireworks", "default model id for the shelley agent")

	// sshImages is the set of image tags that have sshd installed and
	// can therefore support interactive SSH sessions. CI images (go-base,
	// py-base, rust-base, etc.) typically don't have sshd. Defaults to
	// dev-base only; operators can add more via GATEWAY_SSH_IMAGES.
	sshImages     = flags.String("ssh-images", envOr("GATEWAY_SSH_IMAGES", "dev-base"), "comma-separated image tags that support interactive SSH (have sshd)")
	metricsListen = flags.String("metrics-listen", envOr("GATEWAY_METRICS_LISTEN", ""), "address for /metrics endpoint (empty = disabled)")

	// extraImageAliases allows operators to add or override short-name
	// → full-tag aliases without code changes. Format: short=full,short=full.
	extraImageAliases = flags.String("image-aliases", envOr("GATEWAY_IMAGE_ALIASES", ""), "comma-separated short=full image aliases (e.g. rust=rust-base,js=js-base)")
)

type endpoint struct {
	ForkdID   string `json:"forkd_id"`
	Netns     string `json:"netns"`
	GuestAddr string `json:"guest_addr"`
	Image     string `json:"image"`
}
```

| Flag | Env fallback | Default |
|---|---|---|
| `--gateway-host` | `FORKD_GATEWAY_HOST` | `sandbox.example.com` |
| `--listen` | none | `:2222` |
| `--host-key` | none | `/etc/spoond-gateway/ssh_host_ed25519_key` |
| `--backend` | none | `https://127.0.0.1:8890` |
| `--backend-token` | `SPOOND_GATEWAY_TOKEN` | "" (required) |
| `--bootstrap-token` | none | ignored (deprecated) |
| `--client-keys` | none | "" (comma list of .pub paths, or a dir scanned for `*.pub`; keys.go L34) |
| `--gateway-key` | none | `/etc/spoond-gateway/gateway_ed25519` |
| `--shelly-binary-url` | `SHELLY_BINARY_URL` | `http://10.43.0.1:8891/assets/shelley` |
| `--llm-gateway-url` | `LLM_GATEWAY_URL` | `http://10.43.0.1:8891/llm/` |
| `--shelly-model` | none | `gpt-oss-20b-fireworks` |
| `--ssh-images` | `GATEWAY_SSH_IMAGES` | `dev-base` |
| `--metrics-listen` | `GATEWAY_METRICS_LISTEN` | "" |
| `--image-aliases` | `GATEWAY_IMAGE_ALIASES` | "" |

The `endpoint` struct the gateway decodes from `/api/sandboxes/{id}/endpoint` is L105-110 (`forkd_id`, `netns`, `guest_addr`, `image`).

### 7.2 Authentication

`cmd/spoond-sshd-gateway/main.go` L112-269:

```go
func Main(args []string) int {
	flags.Parse(args)

	// Start metrics endpoint (issue #20).
	var gwMetrics *metrics.GatewayMetrics
	if *metricsListen != "" {
		gwMetrics = metrics.NewGatewayMetrics()
		go func() {
			mux := http.NewServeMux()
			mux.Handle("/metrics", promhttp.HandlerFor(gwMetrics.Registry, promhttp.HandlerOpts{}))
			log.Printf("gateway metrics on %s", *metricsListen)
			log.Fatal(http.ListenAndServe(*metricsListen, mux))
		}()
	}
	if *backendTok == "" {
		log.Fatal("--backend-token is required")
	}

	// Parse the set of images that support interactive SSH (have sshd
	// installed). Default is dev-base only; operators can add more via
	// GATEWAY_SSH_IMAGES=dev-base,rust-base,...
	sshImageSet = make(map[string]bool)
	for _, img := range strings.Split(*sshImages, ",") {
		img = strings.TrimSpace(img)
		if img != "" {
			sshImageSet[img] = true
		}
	}

	// Parse extra image aliases from GATEWAY_IMAGE_ALIASES (format:
	// short=full,short=full). These are merged into the hardcoded
	// imageAliases map so operators can add new short names without
	// code changes.
	if *extraImageAliases != "" {
		for _, pair := range strings.Split(*extraImageAliases, ",") {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			short, full, ok := strings.Cut(pair, "=")
			if !ok || short == "" || full == "" {
				log.Printf("warning: ignoring malformed image alias %q (expected short=full)", pair)
				continue
			}
			imageAliases[strings.TrimSpace(short)] = strings.TrimSpace(full)
			log.Printf("image alias: %s -> %s", short, full)
		}
	}

	hostKey := loadOrGenerateHostKey(*hostKeyPath)
	gatewayKey := loadOrGenerateKey(*gatewayKeyPath)

	allowed, err := loadAuthorizedKeys(*clientKeys)
	if err != nil {
		log.Fatalf("client keys: %v", err)
	}

	// Identity-store authority (security review #37 H1): when the
	// backend has an identity store, key resolution there is the ONLY
	// gate — the local allowlist is a legacy single-user fallback and
	// must not silently admit keys that the store doesn't know (which
	// would also leave them unscoped after `ssh-key rm`). Probe once at
	// startup; the store's presence doesn't change at runtime.
	identityAuthoritative := false
	if *backendURL != "" && *backendTok != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		b, berr := backendJSON(ctx, http.MethodGet, "/api/identity-status", nil)
		cancel()
		if berr == nil {
			var st struct {
				IdentityStore bool `json:"identity_store"`
			}
			if json.Unmarshal(b, &st) == nil {
				identityAuthoritative = st.IdentityStore
				log.Printf("identity store %v (key resolution %s)",
					identityAuthoritative, map[bool]string{true: "authoritative", false: "local allowlist only"}[identityAuthoritative])
			}
		}
		if identityAuthoritative {
			allowed = nil // local allowlist must not bypass the store
		}
	}

	config := &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if len(allowed) == 0 && !identityAuthoritative {
				if gwMetrics != nil {
					gwMetrics.AuthFailures.WithLabelValues("no_keys").Inc()
				}
				return nil, fmt.Errorf("no client keys configured")
			}
			// Accept any key from the allowed set (backward compatible);
			// the lease id in the username is the real capability.
			for _, pk := range allowed {
				if string(pk.Marshal()) == string(key.Marshal()) {
					perms := &ssh.Permissions{
						Extensions: map[string]string{
							"forkd-key-id": fmt.Sprintf("%s %s", ssh.FingerprintSHA256(pk), pk.Type()),
						},
					}
					// When the backend has an identity store, resolve the
					// key to a user id so `ctl whoami` and future
					// owner-scoping can attribute the connection (T1).
					userID, userName := resolveKeyUser(key)
					if userID != "" {
						perms.Extensions["forkd-user-id"] = userID
						perms.Extensions["forkd-user-name"] = userName
					}
					return perms, nil
				}
			}
			// Identity-authoritative mode: the store must know the key.
			if identityAuthoritative {
				userID, userName := resolveKeyUser(key)
				if userID == "" {
					if gwMetrics != nil {
						gwMetrics.AuthFailures.WithLabelValues("identity_not_found").Inc()
					}
					return nil, fmt.Errorf("unknown client key (not in identity store)")
				}
				perms := &ssh.Permissions{
					Extensions: map[string]string{
						"forkd-key-id":    fmt.Sprintf("%s %s", ssh.FingerprintSHA256(key), key.Type()),
						"forkd-user-id":   userID,
						"forkd-user-name": userName,
					},
				}
				return perms, nil
			}
			if gwMetrics != nil {
				gwMetrics.AuthFailures.WithLabelValues("unknown_key").Inc()
			}
			return nil, fmt.Errorf("unknown client key")
		},
	}
	config.AddHostKey(hostKey)

	ln, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		log.Fatalf("listen %s: %v", *listenAddr, err)
	}
	log.Printf("spoond-sshd-gateway listening on %s", *listenAddr)

	// Connection counter goroutine: wrap the accept loop to count connections.
	// The actual accept loop is below; we instrument it inline.

	for {
		conn, err := ln.Accept()
		if err == nil && gwMetrics != nil {
			gwMetrics.ConnectionsTotal.Inc()
		}
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go handleConn(conn, config, gatewayKey)
	}
}
```

- **Auth model:** public-key only. At startup the gateway probes `GET /api/identity-status`. If an identity store exists, it is **authoritative**: a key must resolve via `GET /api/users/by-key?fingerprint=<SHA256>`, and the local allowlist is dropped. Otherwise any key in `--client-keys` is accepted.
- The resolved user id rides in `Permissions.Extensions["forkd-user-id"]` (also `forkd-key-id` and `forkd-user-name`).
- Every backend call from the connection uses the gateway token plus `X-Spoond-User-Id: <user id>` (the impersonation trusted by the backend's `authMiddleware`).
- The **nested** SSH into the guest uses user `root` and the `--gateway-key` identity. Its public half is baked into the dev-base `authorized_keys`. `HostKeyCallback` is `InsecureIgnoreHostKey`.

### 7.3 Connection routing (username semantics)

`cmd/spoond-sshd-gateway/main.go` L271-447:

```go
func handleConn(conn net.Conn, config *ssh.ServerConfig, gatewayKey ssh.Signer) {
	defer conn.Close()
	sconn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		log.Printf("handshake: %v", err)
		return
	}
	defer sconn.Close()
	user := sconn.User()
	log.Printf("conn: user=%s addr=%s", user, sconn.RemoteAddr())

	// The authenticated key identity (from PublicKeyCallback) rides in
	// the connection permissions, for `ctl whoami`.
	keyID := ""
	userID := ""
	userName := ""
	if sconn.Permissions != nil && sconn.Permissions.Extensions != nil {
		keyID = sconn.Permissions.Extensions["forkd-key-id"]
		userID = sconn.Permissions.Extensions["forkd-user-id"]
		userName = sconn.Permissions.Extensions["forkd-user-name"]
	}
	// All backend calls from this connection run owner-scoped as the SSH
	// user (U6/T5); the gateway impersonates via X-Spoond-User-Id.
	gwCtx := withGatewayUser(context.Background(), userID)

	go ssh.DiscardRequests(reqs)

	// The control plane is a reserved username: `ssh ctl@... "cmd"`.
	// Exec requests become API calls (new/ls/rm/keepalive/cp) and the
	// response is JSON on stdout — no sandbox is dialed.
	if user == "ctl" {
		handleControlPlane(chans, gatewayKey, keyID, userID, userName)
		return
	}

	// Resolve the target: a 32-hex lease id attaches an existing sandbox;
	// a friendly name resolves to its lease; new[-<image>] auto-creates a
	// persistent one (SSH-as-API).
	leaseID := user
	motd := ""
	// Reconnect hint, shown on create AND attach so the user can always
	// get back. Port comes from the listen address (default :2222).
	_, gwPort, _ := net.SplitHostPort(*listenAddr)
	if gwPort == "" {
		gwPort = "22"
	}
	reconnect := func(id string) string {
		return fmt.Sprintf("Reconnect: ssh %s@%s -p %s", id, *gatewayHost, gwPort)
	}
	if !isLeaseID(user) {
		// Try a friendly name first (assigned via `ctl tag <id> <name>`).
		// Anything that's not a lease id and not a new-* create verb is a
		// candidate name; createSandbox rejects unknown verbs below.
		if !strings.HasPrefix(user, "new") {
			if id, ok := resolveName(gwCtx, user); ok {
				leaseID = id
				user = id // keep motd generic below
			}
		}
	}
	if !isLeaseID(user) {
		created, img, err := createSandbox(gwCtx, user)
		if err != nil {
			errMsg := "spoond: " + err.Error() + "\n"
			log.Printf("create for %q failed: %v", user, err)
			// Deliver the error on the first session channel.
			for nc := range chans {
				if nc.ChannelType() != "session" {
					nc.Reject(ssh.UnknownChannelType, "only session channels supported")
					continue
				}
				ch, _, _ := nc.Accept()
				ch.Write([]byte(errMsg))
				ch.Close()
				return
			}
			return
		}
		leaseID = created
		motd = fmt.Sprintf("spoond: created sandbox %s (%s) — tmux 'dev' attached. Detach: Ctrl-b d. %s\n",
			created, img, reconnect(created))
		log.Printf("created sandbox %s (%s) for user %q", created, img, user)
	} else {
		// Attaching to an existing lease: show the id in the tmux footer
		// and print the reconnect hint when the session ends, same as
		// create — the id is just as easy to forget on reconnect.
		motd = fmt.Sprintf("spoond: attached to sandbox %s — tmux 'dev' attached. Detach: Ctrl-b d. %s\n",
			leaseID, reconnect(leaseID))
	}

	// Security review #37 rescan F8: dial with the USER-scoped context,
	// not Background — resolveEndpoint/restartSSHD must run as the SSH
	// user (X-Spoond-User-Id) or attach would resolve as the gateway
	// service identity: user-owned leases would 404 (broken attach in
	// store mode) and gateway-owned leases would be attachable by any
	// user without an ownership check.
	client, err := dialSandbox(gwCtx, leaseID, gatewayKey)
	if err != nil {
		log.Printf("dial sandbox for %s: %v", leaseID, err)
		// Tell the client what happened with a session-level error.
		for nc := range chans {
			if nc.ChannelType() != "session" {
				nc.Reject(ssh.UnknownChannelType, "only session channels supported")
				continue
			}
			ch, _, _ := nc.Accept()
			fmt.Fprintf(ch, "spoond: cannot reach sandbox %s: %v\n", leaseID, err)
			ch.Close()
			return
		}
		return
	}
	defer client.Close()

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "only session channels supported")
			continue
		}
		go handleSession(newChan, client, motd)
	}
}

// isLeaseID reports whether s is a 32-char lowercase hex lease id.
func isLeaseID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// imageAliases maps SSH username image names to backend snapshot tags.
var imageAliases = map[string]string{
	"dev":    "dev-base",
	"go":     "go-base",
	"py":     "py-base",
	"python": "py-base",
	"elixir": "elixir-base",
	"llm":    "llm-review",
	"base":   "dev-base",
}

// sshImageSet is populated from the --ssh-images flag at startup.
var sshImageSet map[string]bool

// fetchKnownImages queries the backend's /api/images endpoint and returns
// the list of known image tags. This is used to validate full image names
// (e.g. "rust-base") that aren't in the imageAliases short-name map.
func fetchKnownImages(ctx context.Context) ([]string, error) {
	b, err := backendJSON(ctx, http.MethodGet, "/api/images", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Images []string `json:"images"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, fmt.Errorf("parse /api/images: %w", err)
	}
	return resp.Images, nil
}

// sshImageTags returns a sorted slice of image tags that support
// interactive SSH, for use in error messages.
func sshImageTags() []string {
	tags := make([]string, 0, len(sshImageSet))
	for tag := range sshImageSet {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return tags
}
```

Username forms:
- `ctl` → control plane.
- 32-hex → attach to that lease.
- `new` / `new-<alias|tag>` → create a persistent lease: `POST /api/sandboxes {"image","persistent":true,"ttl":3600}` via `backendJSONRetry`. Only images in `--ssh-images` are allowed.
- Anything else → friendly-name lookup via `GET /api/names/{name}`.

`cmd/spoond-sshd-gateway/main.go` L449-520:

```go
// createSandbox parses an SSH username of the form new[-<image>] and
// creates a persistent sandbox in the backend. Returns the new lease id
// and the resolved image tag.
func createSandbox(ctx context.Context, user string) (string, string, error) {
	image := "dev-base"
	alias := ""
	if user != "new" {
		rest, ok := strings.CutPrefix(user, "new-")
		if !ok || rest == "" {
			return "", "", fmt.Errorf("unknown command %q — use a lease id or new[-<image>] (new, new-dev, new-go, new-py, new-elixir, new-llm, or new-<full-tag>)", user)
		}
		alias = rest
	}
	if alias != "" {
		tag, ok := imageAliases[alias]
		if !ok {
			// Not a known short alias — try matching as a full image
			// tag against the backend's known images. This lets new
			// images (e.g. rust-base) be used immediately without
			// adding a code-level alias.
			known, err := fetchKnownImages(ctx)
			if err != nil {
				return "", "", fmt.Errorf("cannot verify image %q (backend /api/images unavailable: %v) — try a known short name: dev, go, py, elixir, llm", alias, err)
			}
			found := false
			for _, k := range known {
				if k == alias {
					image = alias
					found = true
					break
				}
			}
			if !found {
				sort.Strings(known)
				return "", "", fmt.Errorf("unknown image %q — try a short name (dev, go, py, elixir, llm) or a known tag: %s", alias, strings.Join(known, ", "))
			}
		} else {
			image = tag
		}
	}

	// Interactive SSH requires an image with sshd. Most CI images (go,
	// py, elixir, rust, etc.) don't have sshd — they're for API exec
	// workflows, not interactive shells. Reject before creating so we
	// don't orphan a sandbox.
	if !sshImageSet[image] {
		return "", "", fmt.Errorf("image %q is not an interactive SSH image (no sshd) — interactive SSH requires one of: %s. Use the backend API for CI images.", image, strings.Join(sshImageTags(), ", "))
	}

	payload, err := json.Marshal(map[string]any{
		"image":      image,
		"persistent": true,
		"ttl":        3600,
	})
	if err != nil {
		return "", "", err
	}
	b, err := backendJSONRetry(ctx, http.MethodPost, "/api/sandboxes", payload)
	if err != nil {
		return "", "", err
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &created); err != nil {
		return "", "", fmt.Errorf("create response: %w", err)
	}
	if created.ID == "" {
		return "", "", fmt.Errorf("create response missing id: %s", strings.TrimSpace(string(b)))
	}
	return created.ID, image, nil
}
```

### 7.4 dialSandbox (how it reaches the guest)

`cmd/spoond-sshd-gateway/main.go` L522-592:

```go
// dialSandbox resolves the lease to its netns+address and opens a nested
// SSH client connection to the sandbox's sshd using the gateway key.
func dialSandbox(ctx context.Context, leaseID string, gatewayKey ssh.Signer) (*ssh.Client, error) {
	ep, err := resolveEndpoint(ctx, leaseID)
	if err != nil {
		return nil, err
	}

	// Restart sshd inside the sandbox. Firecracker restore carries the
	// process table over, but the pre-snapshot sshd's listening socket is
	// dead in the restored netns (the guest kernel re-initializes its
	// network stack on restore). A fresh sshd binds cleanly. We reach the
	// agent via the backend exec API — the agent socket survives restore.
	if err := restartSSHD(ctx, leaseID); err != nil {
		log.Printf("sshd restart for %s: %v", leaseID, err)
	}

	host, port, err := net.SplitHostPort(ep.GuestAddr)
	if err != nil {
		// GuestAddr may be host:port for the agent; sshd is on port 22.
		host = ep.GuestAddr
		port = "22"
	}
	if port == "8888" {
		port = "22"
	}
	target := net.JoinHostPort(host, port)

	// Enter the sandbox's netns on this thread, dial, then return to the
	// default netns for the relay. setns requires a locked thread.
	var dialed net.Conn
	errCh := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		nsPath := filepath.Join("/var/run/netns", ep.Netns)
		f, err := os.Open(nsPath)
		if err != nil {
			errCh <- fmt.Errorf("open netns %s: %w", nsPath, err)
			return
		}
		defer f.Close()
		if err := unix.Setns(int(f.Fd()), unix.CLONE_NEWNET); err != nil {
			errCh <- fmt.Errorf("setns %s: %w", ep.Netns, err)
			return
		}
		d, err := net.DialTimeout("tcp", target, 10*time.Second)
		if err != nil {
			errCh <- fmt.Errorf("dial %s in netns %s: %w", target, ep.Netns, err)
			return
		}
		dialed = d
		errCh <- nil
	}()
	if err := <-errCh; err != nil {
		return nil, err
	}

	cfg := &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(gatewayKey)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // homelab; dev-base host key regenerates per bake
		Timeout:         10 * time.Second,
	}
	clientConn, chans, reqs, err := ssh.NewClientConn(dialed, target, cfg)
	if err != nil {
		dialed.Close()
		return nil, fmt.Errorf("nested ssh to %s: %w", target, err)
	}
	return ssh.NewClient(clientConn, chans, reqs), nil
}
```

Summary:
1. `GET /api/sandboxes/{id}/endpoint` → `netns` and `guest_addr` (e.g. `10.42.0.2:8888`).
2. `restartSSHD` via `POST /api/sandboxes/{id}/exec` (pkill plus a restart of `/usr/sbin/sshd`, because restore kills the old listener).
3. The port is swapped from `8888` to `22`.
4. `setns` into `/var/run/netns/<netns>` on a locked thread, then TCP dial `10.42.0.2:22`.
5. `ssh.NewClientConn` as root with the gateway key.
6. `handleSession` relays pty-req, env, window-change, signal and subsystem requests, then shell or exec, then stdio, then exit-status.

### 7.5 ctl verb dispatch (control plane)

`cmd/spoond-sshd-gateway/main.go` L594-1010:

```go
// handleControlPlane implements the SSH-as-API control plane for the
// reserved `ctl` username. It accepts the first session channel, runs
// the exec command as a forkd API call, writes JSON to the channel and
// closes it. Usage:
//
//	ssh ctl@sandbox.example.com "new [image]"     create a persistent lease
//	ssh ctl@sandbox.example.com "ls"              list leases
//	ssh ctl@sandbox.example.com "rm <lease-id>"   delete a lease
//	ssh ctl@sandbox.example.com "keepalive <id>"  extend a lease
//	ssh ctl@sandbox.example.com "cp <id> [tag]"   clone a sandbox (branch)
//	ssh ctl@sandbox.example.com "help"
func handleControlPlane(chans <-chan ssh.NewChannel, gatewayKey ssh.Signer, keyID, userID, userName string) {
	// Note: ctl command metrics would need gwMetrics passed through the
	// call chain. For now, connection-level metrics are captured here;
	// per-verb metrics require threading gwMetrics through handleControlPlane.
	// This is a follow-up enhancement.
	// All ctl verbs run owner-scoped as the SSH user (U6/T5).
	gwCtx := withGatewayUser(context.Background(), userID)
	// Accept the first session channel; anything else is rejected.
	var ch ssh.Channel
	var reqs <-chan *ssh.Request
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "only session channels supported")
			continue
		}
		var err error
		ch, reqs, err = nc.Accept()
		if err != nil {
			return
		}
		break
	}
	if ch == nil {
		return
	}
	defer ch.Close()

	// Read the exec request from the channel's request stream.
	for req := range reqs {
		if req.Type != "exec" {
			if req.WantReply {
				req.Reply(false, nil)
			}
			continue
		}
		var msg struct {
			Command string
		}
		if err := ssh.Unmarshal(req.Payload, &msg); err != nil {
			fmt.Fprintf(ch, `{"error":"bad exec payload: %v"}`+"\n", err)
			if req.WantReply {
				req.Reply(false, nil)
			}
			return
		}
		if req.WantReply {
			req.Reply(true, nil)
		}
		out := runControlCommand(gwCtx, msg.Command, gatewayKey, keyID, userID, userName)
		ch.Write([]byte(out + "\n"))
		return
	}
}

// runControlCommand executes one control-plane command and returns the
// JSON-ish response text written to the client.
//
// Default output is human-readable (ticket #27); `--json` anywhere in
// the command opts into raw machine format for scripts/LLM skills.
func runControlCommand(ctx context.Context, cmd string, gatewayKey ssh.Signer, keyID, userID, userName string) string {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return `{"error":"empty command — try new, ls, rm, keepalive, cp, help"}`
	}
	jsonMode := false
	kept := fields[:0]
	for _, f := range fields {
		if f == "--json" || f == "-j" {
			jsonMode = true
			continue
		}
		kept = append(kept, f)
	}
	fields = kept

	switch fields[0] {
	case "help", "--help", "-h":
		return "commands: new [dev|go|py|elixir|llm], ls [--json], stat <id> [--json], rm <id>, keepalive <id>, suspend <id>, resume <id>, restart <id>, cp <id> [tag], shelly <id>, tag <id> <name>, comment <id> <text>, whoami, prompt <id> <message>, ssh-key ls|add <pubkey> <name>|rm <id>, share add <id> <user> [ssh|http] [ttl]|ls|rm <id> <user> — add --json for raw output"
	case "whoami":
		if keyID == "" {
			if jsonMode {
				return `{"user":"ctl","key":"unknown"}`
			}
			return "user: ctl (key: unknown)"
		}
		if userName != "" {
			if jsonMode {
				return fmt.Sprintf(`{"user":%q,"key":%q,"user_id":%q}`, userName, keyID, userID)
			}
			return fmt.Sprintf("user: %s (key: %s)", userName, keyID)
		}
		if jsonMode {
			return fmt.Sprintf(`{"user":"ctl","key":%q}`, keyID)
		}
		return fmt.Sprintf("user: ctl (key: %s)", keyID)
	case "new":
		user := "new"
		if len(fields) > 1 {
			user = "new-" + fields[1]
		}
		id, img, err := createSandbox(ctx, user)
		if err != nil {
			return fmt.Sprintf(`{"error":"%v"}`, err)
		}
		return fmt.Sprintf(`{"id":%q,"image":%q,"created":true}`, id, img)
	case "ls":
		b, err := backendJSON(ctx, http.MethodGet, "/api/sandboxes", nil)
		if err != nil {
			return fmt.Sprintf(`{"error":"%v"}`, err)
		}
		if jsonMode {
			return strings.TrimSpace(string(b))
		}
		return prettySandboxTable(b)
	case "stat":
		if len(fields) < 2 {
			return `{"error":"usage: stat <lease-id>"}`
		}
		b, err := backendJSON(ctx, http.MethodGet, "/api/sandboxes/"+fields[1]+"/stat", nil)
		if err != nil {
			return fmt.Sprintf(`{"error":"%v"}`, err)
		}
		if jsonMode {
			return strings.TrimSpace(string(b))
		}
		return prettyStat(b)
	case "rm":
		if len(fields) < 2 {
			return `{"error":"usage: rm <lease-id>"}`
		}
		if err := backendJSONErr(ctx, http.MethodDelete, "/api/sandboxes/"+fields[1], nil); err != nil {
			return fmt.Sprintf(`{"error":"%v"}`, err)
		}
		return fmt.Sprintf(`{"id":%q,"deleted":true}`, fields[1])
	case "keepalive", "ka":
		if len(fields) < 2 {
			return `{"error":"usage: keepalive <lease-id>"}`
		}
		b, err := backendJSON(ctx, http.MethodPost, "/api/sandboxes/"+fields[1]+"/keepalive", nil)
		if err != nil {
			return fmt.Sprintf(`{"error":"%v"}`, err)
		}
		return strings.TrimSpace(string(b))
	case "suspend":
		if len(fields) < 2 {
			return `{"error":"usage: suspend <lease-id>"}`
		}
		b, err := backendJSON(ctx, http.MethodPost, "/api/sandboxes/"+fields[1]+"/suspend", nil)
		if err != nil {
			return fmt.Sprintf(`{"error":"%v"}`, err)
		}
		return strings.TrimSpace(string(b))
	case "resume":
		if len(fields) < 2 {
			return `{"error":"usage: resume <lease-id>"}`
		}
		b, err := backendJSON(ctx, http.MethodPost, "/api/sandboxes/"+fields[1]+"/resume", nil)
		if err != nil {
			return fmt.Sprintf(`{"error":"%v"}`, err)
		}
		return strings.TrimSpace(string(b))
	case "cp", "clone":
		if len(fields) < 2 {
			return `{"error":"usage: cp <lease-id> [tag]"}`
		}
		payload := []byte("{}")
		if len(fields) > 2 {
			p, _ := json.Marshal(map[string]string{"tag": fields[2]})
			payload = p
		}
		b, err := backendJSON(ctx, http.MethodPost, "/api/sandboxes/"+fields[1]+"/clone", payload)
		if err != nil {
			return fmt.Sprintf(`{"error":"%v"}`, err)
		}
		return strings.TrimSpace(string(b))
	case "shelly", "agent":
		if len(fields) < 2 {
			return `{"error":"usage: shelly <lease-id>"}`
		}
		return runShelly(ctx, fields[1])
	case "restart":
		if len(fields) < 2 {
			return `{"error":"usage: restart <lease-id>"}`
		}
		b, err := backendJSON(ctx, http.MethodPost, "/api/sandboxes/"+fields[1]+"/restart", nil)
		if err != nil {
			return fmt.Sprintf(`{"error":"%v"}`, err)
		}
		return strings.TrimSpace(string(b))
	case "tag":
		if len(fields) < 3 {
			return `{"error":"usage: tag <lease-id> <name>"}`
		}
		p, _ := json.Marshal(map[string]string{"name": fields[2]})
		b, err := backendJSON(ctx, http.MethodPost, "/api/sandboxes/"+fields[1]+"/tag", p)
		if err != nil {
			return fmt.Sprintf(`{"error":"%v"}`, err)
		}
		return strings.TrimSpace(string(b))
	case "comment":
		if len(fields) < 2 {
			return `{"error":"usage: comment <lease-id> [text...] (no text clears)"}`
		}
		text := ""
		if len(fields) > 2 {
			text = strings.Join(fields[2:], " ")
		}
		p, _ := json.Marshal(map[string]string{"comment": text})
		b, err := backendJSON(ctx, http.MethodPost, "/api/sandboxes/"+fields[1]+"/comment", p)
		if err != nil {
			return fmt.Sprintf(`{"error":"%v"}`, err)
		}
		return strings.TrimSpace(string(b))
	case "prompt":
		if len(fields) < 3 {
			return `{"error":"usage: prompt <lease-id> <message...>"}`
		}
		msg := strings.Join(fields[2:], " ")
		p, _ := json.Marshal(map[string]string{"message": msg})
		// The in-guest agent can take minutes to reply; backendJSON's
		// default client would drop the response at 10s. Use the
		// long-timeout client for this verb only.
		b, err := backendJSONWith(ctx, backendClientLong(), http.MethodPost, "/api/sandboxes/"+fields[1]+"/prompt", p)
		if err != nil {
			return fmt.Sprintf(`{"error":"%v"}`, err)
		}
		return strings.TrimSpace(string(b))
	case "ssh-key", "keys":
		// Identity management (T1). ssh-key ls / ssh-key add <pubkey> <name> / ssh-key rm <user-id>
		if len(fields) < 2 {
			return `{"error":"usage: ssh-key ls | ssh-key add <pubkey> <name> | ssh-key rm <user-id>"}`
		}
		sub := fields[1]
		switch sub {
		case "ls":
			b, err := backendJSON(ctx, http.MethodGet, "/api/users", nil)
			if err != nil {
				return fmt.Sprintf(`{"error":"%v"}`, err)
			}
			if jsonMode {
				return strings.TrimSpace(string(b))
			}
			var resp struct {
				Users []struct {
					ID    string   `json:"id"`
					Name  string   `json:"name"`
					Kind  string   `json:"kind"`
					Admin bool     `json:"admin"`
					Keys  []string `json:"fingerprints"`
				} `json:"users"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal(b, &resp); err != nil || resp.Error != "" {
				return strings.TrimSpace(string(b))
			}
			if len(resp.Users) == 0 {
				return "no users"
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "%-16s %-10s %-6s %s\n", "NAME", "KIND", "ADMIN", "KEYS")
			for _, u := range resp.Users {
				admin := "no"
				if u.Admin {
					admin = "yes"
				}
				keys := ""
				if len(u.Keys) > 0 {
					keys = u.Keys[0]
					if len(u.Keys) > 1 {
						keys += fmt.Sprintf(" (+%d)", len(u.Keys)-1)
					}
				}
				fmt.Fprintf(&sb, "%-16s %-10s %-6s %s\n", u.Name, u.Kind, admin, keys)
			}
			return strings.TrimSuffix(sb.String(), "\n")
		case "add":
			if len(fields) < 4 {
				return `{"error":"usage: ssh-key add <pubkey> <name>"}`
			}
			// Verify + fingerprint the provided public key.
			pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(fields[2]))
			if err != nil {
				return fmt.Sprintf(`{"error":"bad public key: %v"}`, err)
			}
			fp := identity.FingerprintSHA256(pub.Marshal())
			p, _ := json.Marshal(map[string]any{
				"name":         fields[3],
				"kind":         "person",
				"fingerprints": []string{fp},
			})
			b, err := backendJSON(ctx, http.MethodPost, "/api/users", p)
			if err != nil {
				return fmt.Sprintf(`{"error":"%v"}`, err)
			}
			if jsonMode {
				return strings.TrimSpace(string(b))
			}
			return fmt.Sprintf("added user %q with key %s", fields[3], fp)
		case "rm":
			if len(fields) < 3 {
				return `{"error":"usage: ssh-key rm <user-id>"}`
			}
			if err := backendJSONErr(ctx, http.MethodDelete, "/api/users/"+fields[2], nil); err != nil {
				return fmt.Sprintf(`{"error":"%v"}`, err)
			}
			return fmt.Sprintf(`{"deleted":%q}`, fields[2])
		default:
			return `{"error":"unknown ssh-key subcommand — ls, add, rm"}`
		}
	case "share":
		// Sharing (T6/#33). share add <lease-id> <user-id> [ssh|http] [ttl] /
		// share ls / share rm <lease-id> <user-id>
		if len(fields) < 2 {
			return `{"error":"usage: share add <lease-id> <user-id> [ssh|http] [ttl] | share ls | share rm <lease-id> <user-id>"}`
		}
		sub := fields[1]
		switch sub {
		case "add":
			if len(fields) < 4 {
				return `{"error":"usage: share add <lease-id> <user-id-or-name> [ssh|http] [ttl]"}`
			}
			mode := "http"
			ttl := 0
			if len(fields) > 4 && (fields[4] == "ssh" || fields[4] == "http") {
				mode = fields[4]
			}
			if len(fields) > 5 {
				if n, err := strconv.Atoi(fields[5]); err == nil {
					ttl = n
				}
			}
			// The grantee may be a user id or a username; usernames
			// resolve via the minimal by-name endpoint (security review
			// #37 C1 — the full directory is admin-only now).
			grantee := fields[3]
			if !strings.HasPrefix(grantee, "u-") {
				b, err := backendJSON(ctx, http.MethodGet, "/api/users/by-name/"+url.PathEscape(grantee), nil)
				if err != nil {
					return fmt.Sprintf(`{"error":"unknown user %q: %v"}`, grantee, err)
				}
				var ru struct {
					User struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"user"`
					Error string `json:"error"`
				}
				if err := json.Unmarshal(b, &ru); err != nil || ru.Error != "" || ru.User.ID == "" {
					return fmt.Sprintf(`{"error":"unknown user %q"}`, grantee)
				}
				grantee = ru.User.ID
			}
			p, _ := json.Marshal(map[string]any{"grantee": grantee, "mode": mode, "ttl": ttl})
			b, err := backendJSON(ctx, http.MethodPost, "/api/sandboxes/"+fields[2]+"/share", p)
			if err != nil {
				return fmt.Sprintf(`{"error":"%v"}`, err)
			}
			return strings.TrimSpace(string(b))
		case "ls":
			b, err := backendJSON(ctx, http.MethodGet, "/api/shares", nil)
			if err != nil {
				return fmt.Sprintf(`{"error":"%v"}`, err)
			}
			if jsonMode {
				return strings.TrimSpace(string(b))
			}
			var resp struct {
				Shares []struct {
					LeaseID string `json:"lease_id"`
					Grantee string `json:"grantee"`
					Mode    string `json:"mode"`
					Expires string `json:"expires_at"`
				} `json:"shares"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal(b, &resp); err != nil || resp.Error != "" {
				return strings.TrimSpace(string(b))
			}
			if len(resp.Shares) == 0 {
				return "no shares"
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "%-36s %-12s %-6s %s\n", "LEASE", "GRANTEE", "MODE", "EXPIRES")
			for _, sh := range resp.Shares {
				exp := "never"
				if sh.Expires != "" {
					exp = sh.Expires
				}
				fmt.Fprintf(&sb, "%-36s %-12s %-6s %s\n", sh.LeaseID, sh.Grantee, sh.Mode, exp)
			}
			return strings.TrimSuffix(sb.String(), "\n")
		case "rm":
			if len(fields) < 4 {
				return `{"error":"usage: share rm <lease-id> <user-id>"}`
			}
			if err := backendJSONErr(ctx, http.MethodDelete, "/api/sandboxes/"+fields[2]+"/share/"+fields[3], nil); err != nil {
				return fmt.Sprintf(`{"error":"%v"}`, err)
			}
			return fmt.Sprintf(`{"revoked":%q,"grantee":%q}`, fields[2], fields[3])
		default:
			return `{"error":"unknown share subcommand — add, ls, rm"}`
		}
	default:
		return fmt.Sprintf(`{"error":"unknown command %q — try new, ls, rm, keepalive, cp, shelly, restart, tag, prompt, ssh-key, share, help"}`, fields[0])
	}
}
```

| Verb | Backend call(s) |
|---|---|
| `help`/`--help`/`-h` | none |
| `whoami` | none (connection permissions) |
| `new [alias]` | `createSandbox` → `POST /api/sandboxes {"image","persistent":true,"ttl":3600}` (plus `GET /api/images` for non-alias tags) |
| `ls` | `GET /api/sandboxes` (pretty table unless `--json`/`-j`) |
| `stat <id>` | `GET /api/sandboxes/{id}/stat` |
| `rm <id>` | `DELETE /api/sandboxes/{id}` |
| `keepalive`/`ka <id>` | `POST /api/sandboxes/{id}/keepalive` (no body) |
| `suspend <id>` | `POST /api/sandboxes/{id}/suspend` |
| `resume <id>` | `POST /api/sandboxes/{id}/resume` |
| `cp`/`clone <id> [tag]` | `POST /api/sandboxes/{id}/clone` `{}` or `{"tag"}` |
| `shelly`/`agent <id>` | `runShelly`: `GET .../endpoint`, then `POST .../exec {"cmd":<script>,"timeout":200}` (curl the binary from `SHELLY_BINARY_URL`, write shelley.json with `llm_gateway = LLM_GATEWAY_URL+leaseID`, start on :9000); returns `url` `https://<id>-9000.<gateway-host>` |
| `restart <id>` | `POST /api/sandboxes/{id}/restart` |
| `tag <id> <name>` | `POST /api/sandboxes/{id}/tag {"name"}` |
| `comment <id> [text...]` | `POST /api/sandboxes/{id}/comment {"comment"}` |
| `prompt <id> <msg...>` | `POST /api/sandboxes/{id}/prompt {"message"}` (280s client) |
| `ssh-key`/`keys ls` | `GET /api/users` |
| `ssh-key add <pubkey> <name>` | `POST /api/users {"name","kind":"person","fingerprints":[fp]}` |
| `ssh-key rm <user-id>` | `DELETE /api/users/{id}` |
| `share add <id> <user> [ssh|http] [ttl]` | `GET /api/users/by-name/{name}` (if not `u-`), then `POST /api/sandboxes/{id}/share {"grantee","mode","ttl"}` |
| `share ls` | `GET /api/shares` |
| `share rm <id> <user>` | `DELETE /api/sandboxes/{id}/share/{user}` |

### 7.6 Backend client helpers, restartSSHD, resolveEndpoint, resolveName, runShelly

`cmd/spoond-sshd-gateway/main.go` L1241-1496:

```go
// backendClient returns an HTTP client for our own backend (loopback;
// skips TLS verify since the LAN cert doesn't cover 127.0.0.1).
func backendClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
}

// backendClientLong is used for long-running backend calls (e.g. the
// prompt verb, whose in-guest agent can take minutes to reply). The
// lease exec timeout (240s) bounds the real work; give the client
// enough headroom so the backend response is not dropped mid-flight.
func backendClientLong() *http.Client {
	return &http.Client{
		Timeout: 280 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
}

// backendJSON performs a JSON request against the backend with the
// consumer token and returns the response body (or an error on non-2xx).
// Transient connection errors (refused/reset/timeout — e.g. backend busy
// refilling the warm pool) are retried with backoff; the gateway should
// not drop an SSH session because one loopback request got refused.
// backendJSONRetry is backendJSON with a longer retry window for
// create paths. The backend can briefly be unreachable during its idle
// sweep + restart (systemd RestartSec=3, then pool refill); a 3s
// window drops interactive `new@` sessions. This retries transport
// errors with 1/2/4s backoff (~7s total) — long enough to ride out a
// brief restart blip, short enough that an interactive user isn't left
// hanging. If the backend hasn't come back by then it's down for a real
// restart, and waiting longer won't help. HTTP/validation errors are
// still returned immediately (never retried).
func backendJSONRetry(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	var lastErr error
	backoff := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
	for attempt := 0; attempt <= len(backoff); attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff[attempt-1]):
			}
		}
		b, err := backendJSONOnceWith(ctx, backendClient(), method, path, body)
		if err == nil {
			return b, nil
		}
		lastErr = err
		var uerr *url.Error
		if !errors.As(err, &uerr) {
			return nil, err
		}
	}
	return nil, lastErr
}

// backendJSON is backendJSONWith for the short-timeout client; used for
// verbs that should fail fast (list, exec, keepalive).
func backendJSON(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	return backendJSONWith(ctx, backendClient(), method, path, body)
}

// backendJSONWith is backendJSON with an explicit client (e.g. the
// long-timeout client for slow verbs like prompt).
func backendJSONWith(ctx context.Context, client *http.Client, method, path string, body []byte) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}
		b, err := backendJSONOnceWith(ctx, client, method, path, body)
		if err == nil {
			return b, nil
		}
		lastErr = err
		// Only retry transient transport failures, not HTTP/validation errors.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			continue
		}
		return nil, err
	}
	return nil, lastErr
}

func backendJSONOnce(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	return backendJSONOnceWith(ctx, backendClient(), method, path, body)
}

// ctxUserIDKey carries the SSH-authenticated user id (U6/T5). The
// backend honors it via X-Spoond-User-Id only when the gateway's own
// service token is used — the gateway never forwards a user-supplied
// token, so impersonation stays trusted.
type ctxUserIDKey struct{}

// withGatewayUser returns a context that makes backendJSON attach the
// X-Spoond-User-Id header, so ctl verbs run owner-scoped as the SSH
// user instead of the gateway service identity.
func withGatewayUser(ctx context.Context, userID string) context.Context {
	if userID == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxUserIDKey{}, userID)
}

func backendJSONOnceWith(ctx context.Context, client *http.Client, method, path string, body []byte) ([]byte, error) {
	url := strings.TrimRight(*backendURL, "/") + path
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+*backendTok)
	if uid, _ := ctx.Value(ctxUserIDKey{}).(string); uid != "" {
		req.Header.Set("X-Spoond-User-Id", uid)
	}
	// Deliberately NOT forwarding X-Bootstrap-Token (security review
	// #37 rescan F7): the bootstrap token gates the first-user create on
	// a fresh store, and the SSH gateway is the most exposed component.
	// Replaying it unconditionally from here would hand any allowlisted
	// key holder admin on a fresh deployment (gateway starts while the
	// backend is down → allowlist keys authenticate → `ssh-key add`
	// creates the first user as admin). Bootstrap is an operator action
	// via direct API call (curl with the backend's BOOTSTRAP_TOKEN).
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("backend %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return b, nil
}

// restartSSHD pkill's and restarts sshd inside the sandbox via the
// backend exec API. The agent socket survives Firecracker restore; the
// pre-snapshot sshd listener does not. Returns a clear error when the
// image has no sshd (CI images are not interactive).
func restartSSHD(ctx context.Context, leaseID string) error {
	cmd := "command -v /usr/sbin/sshd >/dev/null 2>&1 || echo NO_SSHD_BINARY; pkill -x sshd 2>/dev/null; sleep 1; mkdir -p /run/sshd; /usr/sbin/sshd 2>/dev/null; sleep 1; pgrep -x sshd >/dev/null || echo SSHD_NOT_RUNNING"
	payload, err := json.Marshal(map[string]any{"cmd": cmd})
	if err != nil {
		return err
	}
	b, err := backendJSON(ctx, http.MethodPost, "/api/sandboxes/"+leaseID+"/exec", payload)
	if err != nil {
		return err
	}
	var out struct {
		Stdout string `json:"stdout"`
	}
	_ = json.Unmarshal(b, &out)
	if strings.Contains(out.Stdout, "NO_SSHD_BINARY") {
		return fmt.Errorf("image has no sshd — interactive SSH requires dev-base (use new or new-dev)")
	}
	if strings.Contains(out.Stdout, "SSHD_NOT_RUNNING") {
		return fmt.Errorf("sshd did not start")
	}
	return nil
}

func resolveEndpoint(ctx context.Context, leaseID string) (*endpoint, error) {
	b, err := backendJSON(ctx, http.MethodGet, "/api/sandboxes/"+leaseID+"/endpoint", nil)
	if err != nil {
		return nil, err
	}
	var ep endpoint
	if err := json.Unmarshal(b, &ep); err != nil {
		return nil, err
	}
	return &ep, nil
}

// resolveName looks up a friendly lease name and returns its lease id.
func resolveName(ctx context.Context, name string) (string, bool) {
	b, err := backendJSON(ctx, http.MethodGet, "/api/names/"+name, nil)
	if err != nil {
		return "", false
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.ID == "" {
		return "", false
	}
	return out.ID, true
}

// runShelly implements the `shelly <lease-id>` ctl verb: it bootstraps
// the coding agent inside the lease. The binary is fetched from the
// backend's asset server (plain HTTP on the proxy listener, reachable
// from guests at 10.43.0.1), a shelley.json is written pointing at the
// lease's LLM gateway, and the agent server is started on :9000
// (detached via setsid so the one-shot exec does not kill it). Returns
// JSON with the public web URL.
func runShelly(ctx context.Context, leaseID string) string {
	if _, err := resolveEndpoint(ctx, leaseID); err != nil {
		return fmt.Sprintf(`{"error":"%v"}`, err)
	}
	gw := *llmGatewayURL + leaseID
	script := fmt.Sprintf(`set -e
if [ ! -x /root/shelley ]; then
  curl -sf --max-time 180 %s -o /root/shelley
  chmod +x /root/shelley
fi
printf '{"llm_gateway":%q,"default_model":%q}' > /root/shelley.json
if [ -f /root/shelley.pid ]; then kill $(cat /root/shelley.pid) 2>/dev/null || true; rm -f /root/shelley.pid; fi
setsid /root/shelley --config /root/shelley.json serve --port 9000 --socket none >/root/shelley.log 2>&1 < /dev/null &
echo $! > /root/shelley.pid
sleep 6
if curl -sf --max-time 5 http://127.0.0.1:9000/version >/dev/null 2>&1; then
  echo SHELLEY_UP
else
  echo SHELLEY_DOWN; tail -5 /root/shelley.log
fi`, *shellyBinaryURL, gw, *shellyModel)
	payload, err := json.Marshal(map[string]any{"cmd": script, "timeout": 200})
	if err != nil {
		return fmt.Sprintf(`{"error":"%v"}`, err)
	}
	b, err := backendJSON(ctx, http.MethodPost, "/api/sandboxes/"+leaseID+"/exec", payload)
	if err != nil {
		return fmt.Sprintf(`{"error":"%v"}`, err)
	}
	var out struct {
		Exit   int    `json:"exit"`
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
	}
	_ = json.Unmarshal(b, &out)
	if !strings.Contains(out.Stdout, "SHELLEY_UP") {
		detail := strings.TrimSpace(out.Stdout)
		if out.Stderr != "" {
			detail += " | stderr: " + strings.TrimSpace(out.Stderr)
		}
		return fmt.Sprintf(`{"id":%q,"status":"failed","exit":%d,"detail":%q}`, leaseID, out.Exit, detail)
	}
	return fmt.Sprintf(`{"id":%q,"status":"started","url":"https://%s-9000.%s"}`, leaseID, leaseID, *gatewayHost)
}
```

`backendClient` uses a 10s timeout with `InsecureSkipVerify: true`. `backendJSONWith` retries transport errors 4 times at `attempt*500ms`. `backendJSONRetry` (create) backs off 1/2/4s.

---

## 8. cmd/spoond-backend/main.go (full)

`cmd/spoond-backend/main.go` L1-250:

```go
// Command forkd-backend runs the forkd ephemeral-backend lease API.
//
// Configuration is read from the environment:
//
//	FORKD_URL        forkd-controller base URL (default http://127.0.0.1:8889)
//	FORKD_TOKEN      bearer token for forkd-controller (optional)
//	BIND_ADDR        listen address (default 127.0.0.1:8890)
//	TLS_CERT, TLS_KEY  serve HTTPS when both are set
//	CONSUMER_TOKENS  comma-separated token=consumer pairs (e.g. "abc=forgejo,def=pi")
//	POOL_SIZE        warm-pool size per image (default 0 = disabled)
//	DEFAULT_TTL_SECS default lease TTL (default 300)
//	MAX_TTL_SECS     max lease TTL (default 3600)
//	LLM_UPSTREAM_URL OpenAI-compatible LLM API base for the per-lease
//	                 LLM gateway (e.g. https://openrouter.ai/api/v1)
//	LLM_UPSTREAM_KEY server-side key for that upstream (never sent to
//	                 sandboxes; empty disables the gateway)
//	LLM_MAX_CONCURRENT_PER_USER  per-user in-flight LLM gateway request
//	                 cap (0 = unlimited; U8/T8). Per-user LLM keys are
//	                 store data, set via POST /api/users/{id}/llm-key,
//	                 not env config.
package spoondbackend

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jrimmer/spoond/api"
	"github.com/jrimmer/spoond/forkd"
	"github.com/jrimmer/spoond/identity"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// envBoolOr accepts the usual off-words ("0", "false", "no") as false and
// anything else as true, so a typo fails open to the default rather than
// silently disabling a check.
func envBoolOr(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "":
		return def
	case "0", "false", "no":
		return false
	default:
		return true
	}
}

func Main(args []string) int {
	forkdURL := envOr("FORKD_URL", "http://127.0.0.1:8889")
	forkdToken := os.Getenv("FORKD_TOKEN")
	bindAddr := envOr("BIND_ADDR", "127.0.0.1:8890")
	proxyAddr := envOr("PROXY_ADDR", "") // e.g. 0.0.0.0:8891 (Caddy wildcard front)
	tlsCert := os.Getenv("TLS_CERT")
	tlsKey := os.Getenv("TLS_KEY")
	poolSize := envIntOr("POOL_SIZE", 0)
	idleTimeoutSecs := envIntOr("IDLE_TIMEOUT_SECS", 0) // persistent-lease auto-suspend
	idleTimeout := time.Duration(idleTimeoutSecs) * time.Second
	defaultTTL := time.Duration(envIntOr("DEFAULT_TTL_SECS", 300)) * time.Second
	maxTTL := time.Duration(envIntOr("MAX_TTL_SECS", 3600)) * time.Second

	// Parse consumer tokens: "abc=forgejo,def=pi"
	tokens := map[string]string{}
	for _, pair := range strings.Split(os.Getenv("CONSUMER_TOKENS"), ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) == 2 {
			tokens[parts[0]] = parts[1]
		}
	}
	if len(tokens) == 0 {
		log.Fatal("CONSUMER_TOKENS is required (token=consumer,comma-separated)")
	}

	fc := forkd.NewClient(forkdURL, forkdToken)
	// The client's overall HTTP timeout must exceed the largest exec the
	// backend is willing to forward, or long CI steps die at exactly the
	// client timeout (600s default) regardless of MAX_EXEC_TIMEOUT_SECS.
	if v := os.Getenv("FORKD_HTTP_TIMEOUT_SECS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			fc.SetHTTPTimeout(time.Duration(n) * time.Second)
		}
	}
	// knownTags surfaces baked images even when the controller's list
	// endpoint is empty; add tags here as you bake them. Also seeds the
	// warm pool so every image pre-forks at startup.
	knownTags := []string{}
	if v := os.Getenv("KNOWN_IMAGES"); v != "" {
		knownTags = strings.Split(v, ",")
	}
	svc := api.NewServiceWithIdle(fc, tokens, poolSize, defaultTTL, maxTTL, idleTimeout, knownTags...)
	// Per-spawn integrity probe: a sandbox with a corrupt toolchain answers a
	// ping and then fails the job deep inside a build, so verify it from
	// inside the guest before pooling or leasing it. SANDBOX_PROBE=0 disables.
	svc.SetSandboxProbe(envBoolOr("SANDBOX_PROBE", true), time.Duration(envIntOr("SANDBOX_PROBE_TIMEOUT_SECS", 20))*time.Second)
	// Egress policy enforcement (ticket #13): install iptables FORWARD
	// rules in each lease's child netns. NETPOL_DNS lists resolvers the
	// restricted policy always permits so guests can resolve allowlisted
	// names; empty NETPOL_DNS disables enforcement (no root/netns access).
	if dns := os.Getenv("NETPOL_DNS"); dns != "" {
		svc.SetNetpol(&api.NetnsPolicyApplier{}, strings.Split(dns, ","))
	}
	reg := api.NewImageRegistry(fc, knownTags...)
	// LLM gateway model map: "exe.dev-id=upstream-id,exe.dev-id2=upstream2".
	// Shelley sends exe.dev catalog ids; the gateway rewrites them to the
	// configured upstream's models. Unmapped ids fall back to defaultModel.
	llmModelMap := map[string]string{}
	if v := os.Getenv("LLM_MODEL_MAP"); v != "" {
		for _, pair := range strings.Split(v, ",") {
			parts := strings.SplitN(pair, "=", 2)
			if len(parts) == 2 {
				llmModelMap[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
			}
		}
	}
	// Identity store (epic #26 T1): users + key/token resolution. The
	// store file is optional; when absent the backend runs in legacy
	// single-user mode (consumer tokens only) until a user is created.
	// The first user created becomes the admin (KTD-2).
	if usersFile := os.Getenv("USERS_FILE"); usersFile != "" {
		ids, err := identity.NewStore(usersFile)
		if err != nil {
			log.Fatalf("identity store: %v", err)
		}
		svc.SetIdentities(ids)
		log.Printf("identity store: %s (%d user(s))", usersFile, ids.Count())
	}
	// Trusted gateway impersonation (U6/T5): the SSH gateway's service
	// token, used to act as the SSH-authenticated user on ctl calls.
	if gt := os.Getenv("GATEWAY_TOKEN"); gt != "" {
		svc.SetGatewayToken(gt)
		log.Printf("gateway token: trusted impersonation enabled")
	}

	srv := api.NewServerWithLLM(svc, reg, os.Getenv("LLM_UPSTREAM_URL"), os.Getenv("LLM_UPSTREAM_KEY"), os.Getenv("LLM_DEFAULT_MODEL"), llmModelMap)
	// Per-user LLM gateway concurrency cap (U8/T8): 0 = unlimited.
	if n := envIntOr("LLM_MAX_CONCURRENT_PER_USER", 0); n > 0 {
		srv.SetLLMMaxConcurrent(n)
	}
	// Security review #37 C2: when an identity store is present, deny
	// /llm/ for identity users without an LLM key unless the operator
	// explicitly opts into the pre-U8 capability model.
	srv.SetLLMRequireKey(os.Getenv("LLM_OPEN_LEGACY") == "")
	// Public proxy auth (U7/T7): off (default) = capability model;
	// forward-auth = require X-Proxy-Auth secret + Remote-User identity
	// on every hostname-routed proxy request. The secret is shared with
	// the Caddy forward-auth block (never exposed to guests).
	srv.SetProxyAuth(os.Getenv("PROXY_AUTH_MODE"), os.Getenv("PROXY_AUTH_SECRET"), os.Getenv("PROXY_AUTH_TRUSTED_PEERS"))
	// First-user bootstrap gate (security review #37 H3/M4). Unset keeps
	// the legacy open-bootstrap for single-operator setups; set it on
	// multi-user deployments so a leaked consumer token can't claim
	// admin on a fresh store.
	srv.SetBootstrapToken(os.Getenv("BOOTSTRAP_TOKEN"))

	// Static assets (shelley binary etc.) served to guests on the proxy
	// listener at /assets/<file> (default off; set ASSETS_DIR to enable).
	if d := os.Getenv("ASSETS_DIR"); d != "" {
		srv.SetAssetsDir(d)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)

	// Kill any sandboxes left by a previous backend incarnation before
	// warming the pool, so netns slots are never double-booked.
	svc.ReconcileOrphans(ctx)

	// Slow-loris / header hardening (security review #37 rescan F10):
	// both listeners get read-header timeouts + header size caps so a
	// LAN/guest client can't hold connections open forever with trickled
	// headers or send megabyte header floods.
	newHTTPServer := func(addr string, handler http.Handler) *http.Server {
		return &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			MaxHeaderBytes:    1 << 20,
		}
	}
	httpSrv := newHTTPServer(bindAddr, srv.Handler())

	// Optional second listener: the public HTTP proxy (wildcard
	// *.sandbox.example.com via Caddy). Plain HTTP — Caddy terminates TLS.
	var proxySrv *http.Server
	if proxyAddr != "" {
		proxySrv = newHTTPServer(proxyAddr, srv.ProxyHandler())
		go func() {
			log.Printf("forkd proxy listening on %s (wildcard sandbox hostnames)", proxyAddr)
			if err := proxySrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("proxy: %v", err)
			}
		}()
	}

	// Graceful shutdown: on SIGTERM/SIGINT kill every lease and pooled
	// sandbox before exiting. Without this, a backend restart orphans
	// its warm VMs in the controller (which has no client-liveness), and
	// they hold netns slots until manually reaped.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.Printf("received %v, shutting down (releasing %d leases + warm pool)", sig, len(svc.LiveLeases()))
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()
		svc.Shutdown(shutdownCtx)
		_ = httpSrv.Shutdown(shutdownCtx)
		if proxySrv != nil {
			_ = proxySrv.Shutdown(shutdownCtx)
		}
		cancel()
	}()

	log.Printf("spoond-backend listening on %s (forkd at %s, %d consumer(s), pool=%d)", bindAddr, forkdURL, len(tokens), poolSize)
	var err error
	if tlsCert != "" && tlsKey != "" {
		err = httpSrv.ListenAndServeTLS(tlsCert, tlsKey)
	} else {
		err = httpSrv.ListenAndServe()
	}
	if err != nil && err != http.ErrServerClosed {
		log.Fatalf("server: %v", err)
	}
	return 0
}
```

| Env var | Default | Meaning |
|---|---|---|
| `FORKD_URL` | `http://127.0.0.1:8889` | controller base URL |
| `FORKD_TOKEN` | "" | controller bearer |
| `FORKD_HTTP_TIMEOUT_SECS` | (600 in client) | forkd client overall timeout |
| `BIND_ADDR` | `127.0.0.1:8890` | lease API listen |
| `PROXY_ADDR` | "" (disabled) | proxy/LLM/assets listener (e.g. `0.0.0.0:8891`) |
| `TLS_CERT`, `TLS_KEY` | "" | HTTPS for the lease API when both are set |
| `POOL_SIZE` | 0 | warm pool size per image |
| `IDLE_TIMEOUT_SECS` | 0 | persistent-lease idle auto-suspend |
| `DEFAULT_TTL_SECS` | 300 | default lease TTL |
| `MAX_TTL_SECS` | 3600 | max TTL |
| `CONSUMER_TOKENS` | **required** (fatal if empty) | `token=consumer,...` legacy tokens |
| `KNOWN_IMAGES` | "" | comma tags: seeds the pool and the image list fallback |
| `SANDBOX_PROBE` | true | integrity probe on/off (`0/false/no` = off) |
| `SANDBOX_PROBE_TIMEOUT_SECS` | 20 | probe exec timeout |
| `NETPOL_DNS` | "" (enforcement off) | resolvers for restricted; non-empty enables the `NetnsPolicyApplier` (and expose_ports) |
| `LLM_MODEL_MAP` | "" | `exe-id=upstream-id,...` |
| `USERS_FILE` | "" | identity store JSON path |
| `GATEWAY_TOKEN` | "" | trusted impersonation token |
| `LLM_UPSTREAM_URL`, `LLM_UPSTREAM_KEY`, `LLM_DEFAULT_MODEL` | "" | LLM gateway upstream |
| `LLM_MAX_CONCURRENT_PER_USER` | 0 | per-user LLM in-flight cap |
| `LLM_OPEN_LEGACY` | "" | non-empty means keyless identity users are allowed on /llm |
| `PROXY_AUTH_MODE`, `PROXY_AUTH_SECRET`, `PROXY_AUTH_TRUSTED_PEERS` | "" | proxy forward-auth |
| `BOOTSTRAP_TOKEN` | "" | first-user bootstrap gate |
| `ASSETS_DIR` | "" | `/assets/` dir on the proxy listener |
| `MAX_EXEC_TIMEOUT_SECS` (read in api/server.go L483) | 300 | exec timeout cap |

- **Construction order:** `forkd.NewClient`, then `api.NewServiceWithIdle`, then `SetSandboxProbe`, then (optionally) `SetNetpol(&api.NetnsPolicyApplier{}, dns)`, then `api.NewImageRegistry`, then `SetIdentities`, then `SetGatewayToken`, then `api.NewServerWithLLM`, then the setters.
- **Background loops:** `svc.Start(ctx)` (sweep plus refill every 5s). Then a **one-shot** `svc.ReconcileOrphans(ctx)` runs at startup. Note it runs **after** `Start`, so the first refill tick could race it; the tick is 5s so in practice reconcile runs first.
- **Listeners:** an optional proxy listener goroutine, and a SIGTERM/SIGINT goroutine that calls `svc.Shutdown` (30s), which kills all leases **including persistent workspaces** (`release` calls `DeleteWorkspace`, which removes the state snapshot). **NOTE:** a backend restart therefore destroys every persistent/suspended workspace.

---

## 9. cmd/spoond-doctor/main.go

`spoond doctor [--json]`. Checks, in order:

`cmd/spoond-doctor/main.go` L92-103:

```go
func runChecks() []checkResult {
	var out []checkResult
	out = append(out, checkConfig()...)
	out = append(out, checkForkd()...)
	out = append(out, checkBackend()...)
	out = append(out, checkGatewayPort()...)
	out = append(out, checkLLM()...)
	out = append(out, checkPool()...)
	out = append(out, checkTLS()...)
	out = append(out, checkDisk()...)
	return out
}
```

| Check | What |
|---|---|
| `checkConfig` | `CONSUMER_TOKENS` set (FAIL if not), `FORKD_URL` set (FAIL if unset), `BIND_ADDR` (WARN default) |
| `checkForkd` | TCP reachability of `FORKD_URL`, then `forkd.NewClient(...).ListSandboxes` (5s) |
| `checkBackend` | TCP dial `BIND_ADDR`; GET `/healthz` (https with roots loaded from `TLS_CERT`; the wildcard bind is probed via the cert's DNS SAN or the hostname); expects 200 and `"status":"ok"` |
| `checkGatewayPort` | TCP dial `GATEWAY_ADDR` (default `127.0.0.1:2222`) |
| `checkLLM` | `LLM_UPSTREAM_URL`/`KEY` presence, reachability, `GET <up>/models` with the bearer (200 PASS, 401/403 FAIL) |
| `checkPool` | `forkd ListSandboxes` count (0 → WARN "pool cold") |
| `checkTLS` | `tls.LoadX509KeyPair(TLS_CERT, TLS_KEY)` |
| `checkDisk` | statfs `/`: >90% FAIL, >75% WARN |

Output is a PASS/FAIL/WARN table, or JSON `{"checks","pass","fail","warn","ok"}`; the exit code is non-zero on any FAIL. The forkd-specific checks are `checkForkd` and `checkPool`, plus the `FORKD_URL` config check.

---

## 10. images/ and deploy/rootfs-init/

### 10.1 images/manifest.yaml (full)

`images/manifest.yaml` L1-147:

```yaml
# forkd image manifest
#
# Source of truth for baked forkd snapshot images. One image per
# capability — name by capability, never by repo or project. Many Go
# repos share one `go-base`; many review jobs share one `llm-review`.
#
# This file is what the image-inquiry process consults. When you point
# the agent at a repo, it detects the repo's language/capability, checks
# this manifest, and only bakes a NEW image when a genuinely new
# capability is missing.
#
# Fields:
#   name        baked snapshot tag (must exist on the forkd host)
#   capability  the job capability this image provides (one per image)
#   labels      runs-on labels that map to this image (IMAGE_MAP keys)
#   description what's in it / what it's for
#   baked       true once the snapshot exists on the host
#   rootfs      the ext4 rootfs backing the snapshot (if known)
#   notes       freeform

images:
  - name: py-base
    capability: python
    labels: [python, py, py-base, ubuntu-latest]
    description: "Python 3.12 slim base for Python build/test/CI jobs"
    baked: true
    rootfs: /var/cache/forkd/python-3-12-slim.ext4
    notes: "First image baked 2026-08-07. DEFAULT_IMAGE fallback for the runner."

  - name: go-base
    capability: golang
    labels: [go, golang]
    description: "Go 1.25.12 toolchain (go/gofmt symlinked into /usr/local/bin) + git for Go build/test/CI jobs"
    baked: true
    rootfs: /var/cache/forkd/golang-1-25.ext4
    notes: "Re-baked 2026-08-09 from golang:1.25 (was 1.24 — go.mod needed >=1.25.0). Memory 2 GiB (512 MiB OOM'd the Go compiler). PATH fix: symlink go/gofmt into /usr/local/bin (guest agent uses default PATH, not Docker ENV). GOCACHE/GOPATH default to /root (rootfs, 935M free) — do NOT point them at /tmp (256M tmpfs). Built via CLI snapshot --mem-size-mib 2048 + branch (M2.1)."

  - name: dev-base
    capability: interactive-dev
    labels: [dev]
    description: "Interactive dev environment: tmux 3.4 + sshd + git + python3 + build tools. SSH in, attach to tmux session 'dev'."
    baked: true
    rootfs: /var/cache/forkd/dev-base-rootfs.ext4
    notes: "Baked 2026-08-09 from ubuntu:24.04 + tmux/openssh-server/git/python3/build-essential. 2 GiB memory. /forkd-init.sh patched to: mount devpts, generate ssh host keys, start sshd, and attach-to-or-create tmux session 'dev' on SSH login (via /etc/profile.d/forkd-tmux.sh, skip with FORKD_NO_TMUX). For exe.dev interactive sessions (ticket #18, U4). Branch-built so the memory state has sshd running."

  - name: elixir-base
    capability: elixir
    labels: [elixir]
    description: "Elixir 1.17 / Erlang OTP 27 toolchain + git for Elixir build/test/CI jobs"
    baked: true
    rootfs: /var/cache/forkd/elixir-1-17-3072-*.ext4
    notes: "Baked 2026-08-08 from elixir:1.17 with --size-mib 3072 (needs >1536 MiB). elixir/mix/erl already in /usr/local/bin."

  - name: elixir-release
    capability: elixir-release
    labels: [elixir-release, release]
    description: "Elixir 1.18 / OTP 27 + Rust stable (pinned CARGO_HOME) + Node 22/pnpm + kaniko — full Phoenix+NIF release builds (origin: cytale deploy pipeline)"
    baked: true
    rootfs: /var/cache/forkd/elixir-release-tools-local.ext4
    notes: |
      Bake with deploy/bake-elixir-release.sh (dockerfile: images/elixir-release.dockerfile).
      Sizing per rust-base notes: rootfs 12 GiB, memory 4 GiB — Tantivy-class Rust
      NIF builds + hex deps + node_modules + _build peak well past 8 GiB. Rust homes
      pinned to /usr/local/{rustup,cargo} so cache mounts can never mask toolchain
      binaries. kaniko baked in for daemonless image push (sandboxes have no docker).
      First bake: 2026-09-08. BAKE GOTCHAS (each cost a bake cycle):
      - host docker builds need --network=host --security-opt seccomp=unconfined
        (the host's default docker seccomp profile denies thread creation and
        AF_UNIX; Erlang can't boot there at all — no mix RUNs in the dockerfile).
      - curl | sh silently succeeds with nothing installed when the download
        fails; Rust is COPY'd from rust:1-bookworm with a rustc --version guard.
      - from-image's ext4 cache keys on the image TAG and re-registering over a
        live tag keeps the daemon's old rootfs fd — bake script rm's the cached
        ext4 and forkd rmi's the tag before converting.
      - The guest agent's exec PATH is a hardcoded default; /etc/environment is
        not applied. Toolchains must expose binaries in /usr/local/bin (go-base
        pattern). rustc/cargo are direct symlinks to the toolchain, bypassing
        the rustup proxy (which needs RUSTUP_HOME env the agent won't pass).
      - forkd-init.sh (host-level) now lists LAN resolvers first — sandbox DNS
        is otherwise public, and git.example.com then resolves to the public edge
        whose /v2/ registry path is SSO-gated (302 to login).

  - name: llm-review
    capability: llm-review
    labels: [llm-review]
    description: "Python 3.12 + git + curl for LLM code-review jobs (language-agnostic)"
    baked: true
    rootfs: /var/cache/forkd/llm-review-local.ext4
    notes: "Baked 2026-08-08 from python:3.12-slim + git + curl. Runs .forgejo/scripts/llm-review.py (pure Python stdlib)."

  - name: rust-base
    capability: rust
    labels: [rust, rust-lang]
    description: "Rust stable toolchain + git for Rust build/test/CI jobs"
    baked: false
    rootfs: ""
    notes: |
      Not yet permanently baked. During initial testing (2026-08-11) a rust-base
      snapshot was built from rust:1.85-bookworm but hit several issues:

      1. DISK: cargo test requires full compilation + linking of the
         dependency tree. A 4 GiB rootfs is insufficient — the Rust toolchain
         (~1.5 GiB) + cargo registry (~200 MiB) + build artifacts (2+ GiB)
         + project source exceeds the rootfs. Minimum recommended rootfs:
         8 GiB (or use sparse rootfs per issue #38).

      2. MEMORY: 512 MiB default OOM-kills cargo check during tokio compilation.
         Need at least 4 GiB RAM. Use `forkd from-image --mem-size-mib 4096`
         (issue #39) or re-snapshot with `forkd snapshot --mem-size-mib 4096`.

      3. RUSTUP FRICTION: Docker rust images ship rustup without a default
         toolchain. rust-toolchain.toml with channel="stable" causes rustup to
         download the latest stable, which may not fit on the rootfs. Workarounds:
         - Pre-set `rustup default stable` during image bake (before snapshot)
         - Or bypass rustup: set PATH to the toolchain binaries directly
         - Or set RUSTUP_TOOLCHAIN to the pre-installed version
         - Pin incompatible deps with `cargo update --precise` if using an
           older toolchain (e.g. time needs 1.88, icu_* needs 1.86)

      4. PATH: forkd-agent.py inherits host PATH (issue #41, fixed in PR #44).
         Ensure /etc/environment has the correct PATH including
         /usr/local/cargo/bin before baking the snapshot.

      Recommended bake command (once #38/#39 are resolved):
        forkd from-image rust:1.85-bookworm --tag rust-base \
          --extra python3 --size-mib 8192 --mem-size-mib 4096

      python3 is required for forkd-agent.py (PID 1 guest agent).

  - name: scylla
    capability: cql-database
    labels: []
    description: "ScyllaDB 2026.2.6, single node, developer mode — a SERVICE image: leased per job with expose_ports [9042] and reached from the job's sandbox (#70). Not a runs-on image."
    baked: true
    rootfs: /var/cache/forkd/scylla-service-local-*.ext4
    notes: |
      First baked 2026-09-26; a fresh fork answered CQL 769 ms after restore.
      Bake with deploy/bake-scylla.sh (images/scylla.dockerfile + images/scylla-init-hook.sh).
      Needs deploy/rootfs-init/forkd-init.sh installed first: its cgroup2 mount
      (seastar fails "sstring out of range" without one) and its
      /etc/forkd/init.d hook runner, which starts ScyllaDB BEFORE the snapshot
      so every fork restores serving. Guest 3 GiB, seastar --memory=1200M (1536M does not fit beside seastar's OS reserve),
      --overprovisioned (no busy-polling — idle pool children stay cheap).
      No labels: a job leases it via the lease API, it never runs a job.
      Warm pool: the first lease registers the tag, after which POOL_SIZE
      children stay warm (~2 GiB each) — instant starts, bounded cost.
      Upstream image is RHEL 9: no apt, so no --extra; python3 is already there.
```

### 10.2 Dockerfiles (full)

Base images: `elixir:1.18.4-otp-27` (plus COPY --from `rust:1-bookworm`, `node:22-bookworm-slim`, `gcr.io/kaniko-project/executor:v1.23.2`), `python:3.12-slim`, `scylladb/scylla:2026.2.6` (RHEL 9 based). **There are no dockerfiles for** `go-base`, `dev-base`, `elixir-base`, `llm-review`, `js-base` or `rust-base`. Those were built ad hoc with `forkd from-image <docker image>` (per the manifest notes: `golang:1.25`, `ubuntu:24.04` + tmux/openssh-server/git/python3/build-essential, `elixir:1.17`, `python:3.12-slim` + git + curl, `node:22` + git). dev-base additionally gets its init patched (devpts, ssh host keys, sshd, a tmux login hook `/etc/profile.d/forkd-tmux.sh`, `UsePAM no`) by `deploy/rebuild-dev-base.sh`.

`images/elixir-release.dockerfile` L1-116:

```dockerfile
# elixir-release — capability image for Phoenix/Elixir release builds that
# need the full NIF toolchain (per images/README.md doctrine: shared
# toolbox, name by capability, re-bake in place, never per-repo).
#
# Origin: the cytale deploy pipeline (2026-09-08) — its build job needs
# Elixir 1.18 + Rust (muninn/Tantivy rustler NIF) + Node 22/pnpm in ONE
# job, which no existing image covers (elixir-base is 1.17, no Rust/Node).
# Any Phoenix+NIF repo can reuse it; if a future job needs a different
# single toolchain, prefer a language base instead of growing this one.
#
# Bake (on the forkd host, see deploy/bake-elixir-release.sh):
#   docker build -f images/elixir-release.dockerfile -t elixir-release-tools:local .
#   forkd from-image elixir-release-tools:local --tag elixir-release \
#     --extra python3 --size-mib 12288 --mem-size-mib 4096
#
# Sizing follows the rust-base notes: Rust toolchain + cargo artifacts +
# hex deps + node_modules + _build peak well past 8 GiB; memory 4 GiB
# (512 MiB OOMs Rust compilation).
FROM elixir:1.18.4-otp-27

ENV DEBIAN_FRONTEND=noninteractive
# Tauri 2 Linux target needs WebKitGTK/GTK/libsoup dev headers + the bundler
# helpers. They are BAKED IN (not apt-installed per job): running apt inside
# the sandbox is unreliable because the ext4 conversion randomly corrupts
# /var/lib/apt and /var/lib/dpkg entries (EBADMSG), and the corruption is
# invisible until something touches those paths.
RUN apt-get update -qq \
 && apt-get install -y --no-install-recommends \
      build-essential pkg-config libssl-dev libsrtp2-dev ca-certificates \
      curl git python3 jq xz-utils \
      libwebkit2gtk-4.1-dev libgtk-3-dev libsoup-3.0-dev librsvg2-dev \
      libxdo-dev libayatana-appindicator3-dev patchelf file \
      openssh-server xdg-utils \
 && mkdir -p /run/sshd \
 && rm -rf /var/lib/apt/lists/*
# openssh-server: the suite's OpenSSH interop gate authenticates issued
# certificates against a REAL sshd and FAILS (not skips) when the binary is
# missing — masked for weeks behind the env-gate failures, exposed once the
# runner started providing USER/LOGNAME (infra#26 triage). /run/sshd is
# the privilege-separation dir sshd refuses to start without.
# xdg-utils: Tauri's AppImage bundler shells out to `xdg-mime`, so without it
# the desktop build fails after the .deb and .rpm succeed ("xdg-mime binary
# not found /usr/bin/xdg-mime") and no updater feed is published (cytale
# desktop.yml, every run since the workflow landed).

# Rust — copied wholesale from the stock toolchain image instead of
# rustup-installed: curl/getaddrinfo is unreliable in this host's docker
# build containers (default seccomp denies pthread creation), and `curl | sh` once "passed"
# with no Rust installed when the download failed silently. Pinned homes
# so cache mounts elsewhere can never mask the toolchain binaries.
# The trailing rustc invocation fails the layer if the copy is broken.
ENV RUSTUP_HOME=/usr/local/rustup \
    CARGO_HOME=/usr/local/cargo \
    PATH=/usr/local/cargo/bin:$PATH
COPY --from=rust:1-bookworm /usr/local/rustup /usr/local/rustup
COPY --from=rust:1-bookworm /usr/local/cargo /usr/local/cargo
RUN /usr/local/cargo/bin/rustc --version

# Node 22 + corepack pnpm for SPA builds. Selective copy — a wholesale
# `COPY --from=node /usr/local` would clobber erl/elixir, which live in
# /usr/local/bin on the elixir image.
COPY --from=node:22-bookworm-slim /usr/local/bin/node /usr/local/bin/node
COPY --from=node:22-bookworm-slim /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -sf ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
 && ln -sf ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx \
 && ln -sf ../lib/node_modules/corepack/dist/corepack.js /usr/local/bin/corepack \
 && corepack enable \
 && corepack prepare pnpm@11.24.0 --activate

# kaniko executor — assembles and pushes OCI images with no docker daemon
# (the sandbox has none; kaniko is pure userspace).
COPY --from=gcr.io/kaniko-project/executor:v1.23.2 /kaniko/executor /usr/local/bin/executor

# NOTE: no `mix local.hex` here — Erlang cannot boot inside this host's
# docker build containers (AF_UNIX denied at spawn_init, EACCES) but runs
# fine in the forkd microVM; CI jobs bootstrap hex/rebar themselves in ~5s.

# Guest agent (forkd-agent.py) reads /etc/environment for PATH — but only
# the FIRST PATH= line (appending a second one is silently ignored, which
# once cost us rustc). Rewrite /etc/environment wholesale: cargo first,
# one canonical PATH line; RUSTUP_HOME/CARGO_HOME for good measure.
RUN printf 'PATH=/usr/local/cargo/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\nRUSTUP_HOME=/usr/local/rustup\nCARGO_HOME=/usr/local/cargo\n' > /etc/environment

# The rust:1 image's cargo/bin entries are symlinks to the `rustup` proxy,
# which needs RUSTUP_HOME resolved at runtime — the guest agent may not
# pass it through. Point rustc/cargo straight at the toolchain binary.
RUN ln -sf /usr/local/rustup/toolchains/*/bin/rustc /usr/local/cargo/bin/rustc \
 && ln -sf /usr/local/rustup/toolchains/*/bin/cargo /usr/local/cargo/bin/cargo

# The agent's exec PATH is a hardcoded default that does NOT include
# /usr/local/cargo/bin and (evidence of several failed bakes) does not
# reliably come from /etc/environment either — the go-base bake hit the
# same wall and solved it by symlinking into /usr/local/bin. Do that.
RUN ln -sf /usr/local/cargo/bin/rustc /usr/local/bin/rustc \
 && ln -sf /usr/local/cargo/bin/cargo /usr/local/bin/cargo

# Final guard: fail the BUILD (loudly, before the costly conversion) if
# any tool the CI pipeline needs is missing. The docker→ext4 conversion in
# build-rootfs.sh can silently drop files when the host disk runs tight.
RUN for b in pkg-config rustc cargo node corepack git python3 elixir mix executor curl sshd; do \
      command -v "$b" >/dev/null || { echo "MISSING TOOL: $b" >&2; exit 1; }; \
    done && echo ALL_TOOLS_PRESENT

# Tauri's Linux target links against these at compile time; assert the
# pkg-config entries survive the build (the conversion corruption class has
# bitten this image before).
RUN pkg-config --exists webkit2gtk-4.1 \
 && pkg-config --exists gtk+-3.0 \
 && pkg-config --exists libsoup-3.0 \
 && echo TAURI_SYS_DEPS_OK

# NOTE: DNS/registry reachability is fixed at the INIT level, not here:
# forkd-init.sh (injected post-conversion, lives on the forkd host) now
# lists the LAN resolvers first, so git.example.com resolves to the LAN
# edge whose /v2/ path is not SSO-gated. Image-level /etc/hosts pinning
# does not survive guest boot.
```

`images/py-base.dockerfile` L1-25:

```dockerfile
# py-base — the runner's DEFAULT_IMAGE fallback (IMAGE_MAP: ubuntu-latest ->
# py-base; any label without an explicit mapping lands here). "Python 3.12
# slim base for Python build/test/CI jobs" per images/manifest.yaml; first
# baked 2026-08-07, re-created 2026-09-25 after the infrastructure host consolidation left
# only elixir-release registered (runner jobs on other labels 404'd —
# infra#26 decision b: bake the missing images).
#
# git is REQUIRED: the runner's built-in checkout execs `git clone` inside
# the sandbox. ca-certificates for HTTPS clones. bash ships with slim.
FROM python:3.12-slim

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update -qq \
 && apt-get install -y --no-install-recommends \
      git ca-certificates curl xz-utils file \
 && rm -rf /var/lib/apt/lists/*

# Guard: fail the BUILD loudly if anything the runner or guest agent needs
# is missing (the ext4 conversion can silently drop files when the host
# disk runs tight — same guard pattern as elixir-release).
RUN for b in git python3 pip3 curl; do \
      command -v "$b" >/dev/null || { echo "MISSING TOOL: $b" >&2; exit 1; }; \
    done \
 && git --version && python3 --version
```

`images/scylla.dockerfile` L1-17:

```dockerfile
# scylla — ScyllaDB as a forkd SERVICE image (spoond #70).
#
# Capability, not project: a single-node, developer-mode CQL database that a
# CI job (or any sandbox) leases per run and reaches through `expose_ports`.
# The pinned build matches the one cytale's dev and CI run against.
#
# The image carries a boot hook (/etc/forkd/init.d/, run by forkd-init.sh
# before the agent) that starts ScyllaDB and returns once CQL answers — so the
# bake snapshots it SERVING and every fork restores warm, in milliseconds,
# instead of paying a ~10s cold boot per job.
#
# The RHEL-based upstream image already has python3 (the guest agent's
# interpreter), so the bake passes no --extra (apt would fail here anyway).
FROM scylladb/scylla:2026.2.6
# --chmod, not a RUN chmod: the upstream image's USER is `scylla`, which may
# not chmod a root-owned file.
COPY --chmod=755 scylla-init-hook.sh /etc/forkd/init.d/50-scylla
```

### 10.3 images/scylla-init-hook.sh (full)

`images/scylla-init-hook.sh` L1-43:

```bash
#!/bin/bash
# /etc/forkd/init.d/50-scylla — start ScyllaDB at guest boot (spoond #70).
#
# Runs as a forkd-init.sh hook, before the agent: starts the server detached
# and returns once CQL answers, so a bake snapshots it serving.
#
# Flag syntax matters: the scylla wrapper takes `--flag value` or
# `--flag=value`, but seastar's own options (--smp, --memory) must use `=`, and
# --overprovisioned is a bare flag — `--smp 1` breaks seastar's parser with
# "sstring out of range", the same message a missing cgroup2 mount produces.
#
#   developer-mode    no io/XFS tuning checks (a microVM ext4 rootfs)
#   memory=1200M      seastar reserves RAM for the OS: in the 3 GiB guest only
#                     ~1.37 GiB is left, so 1536M fails "insufficient physical
#                     memory" (measured). 1200M is also the dev-recipe value.
#   overprovisioned   no busy-polling: idle pool children stay cheap
#   tablets disabled  schemas using SimpleStrategy are rejected under tablets
#   rpc 0.0.0.0       reachable through the lease's expose_ports DNAT
set -u

mkdir -p /var/lib/scylla/data /var/lib/scylla/commitlog /var/lib/scylla/hints /var/lib/scylla/view_hints

nohup setsid /usr/bin/scylla \
  --options-file /etc/scylla/scylla.yaml \
  --developer-mode=1 --smp=1 --memory=1200M --overprovisioned \
  --tablets-mode-for-new-keyspaces=disabled \
  --listen-address=127.0.0.1 --rpc-address=0.0.0.0 --broadcast-rpc-address=10.42.0.2 \
  --seed-provider-parameters=seeds=127.0.0.1 --api-address=127.0.0.1 \
  >/tmp/scylla.log 2>&1 </dev/null &
echo $! >/tmp/scylla.pid

# Ready = the CQL port accepts. Bounded well inside forkd-init's hook timeout.
for _ in $(seq 1 150); do
  if (exec 3<>/dev/tcp/127.0.0.1/9042) 2>/dev/null; then
    echo "scylla: CQL up"
    exit 0
  fi
  kill -0 "$(cat /tmp/scylla.pid)" 2>/dev/null || { echo "scylla: exited during start"; tail -20 /tmp/scylla.log; exit 1; }
  sleep 1
done
echo "scylla: CQL not up after 150s"
tail -20 /tmp/scylla.log
exit 1
```

**NOTE:** `--broadcast-rpc-address=10.42.0.2` hardcodes the forkd guest IP. The CQL port 9042 is reached through `expose_ports:[9042]`.

### 10.4 images/validate-image.py (summary)

133 lines, stdlib Python plus `yaml` for the manifest. Usage: `images/validate-image.py [--manifest images/manifest.yaml] <repo-path-or-url>` (URLs are `git clone`d into a tempdir). It detects a capability from marker files plus a regex:
- golang → go-base (`go.mod`/`go.sum`/`Gopkg.toml`, `^module\s`)
- rust → rust-base (`Cargo.toml`/`Cargo.lock`)
- elixir → elixir-base (`mix.exs`/`mix.lock`)
- python → py-base (`pyproject.toml`/`setup.py`/`requirements.txt`/`Pipfile`)
- node → node-base (`package.json`/...)
- llm-review → llm-review

It then checks the manifest for an image with that capability and `baked: true`. Exit codes: **COVERED** (0), **NEEDS** (1, suggests a name), **UNKNOWN** (2). It validates **manifest coverage only**; it doesn't touch images or snapshots.

### 10.5 deploy/rootfs-init/forkd-init.sh (full: guest PID 1)

`deploy/rootfs-init/forkd-init.sh` L1-95:

```bash
#!/bin/bash
# /forkd-init.sh — PID 1 inside the guest. Mounts pseudo-fs, fixes
# DNS to public resolvers, then launches the Python agent.

mount -t proc proc /proc 2>/dev/null
mount -t sysfs sys /sys 2>/dev/null
mount -t devtmpfs devtmpfs /dev 2>/dev/null

# Mount /tmp as tmpfs so per-sandbox scratch writes (agent logs,
# socket paths, hint files used by demos) land in this VM's memory
# instead of the shared on-disk rootfs.ext4. Without this, multiple
# sandboxes booted from the same snapshot file would concurrently
# write to the same inode in the same loop-mounted ext4 and corrupt
# it ("Structure needs cleaning" on the next access).
# tmpfs state survives BRANCH because it lives in guest RAM, which
# is what memory.bin captures.
mount -t tmpfs -o size=256m tmpfs /tmp 2>/dev/null

# Make sure PATH covers both Ubuntu (/usr/bin) and official python (/usr/local/bin)
# images. Subprocess invocations from the agent inherit this.
export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

# Persistent volumes — see VolumeSpec / BootConfig::with_volume on the host.
# The kernel cmdline carries an entry of the form:
#   forkd.mounts=vdb:/opt/cache,vdc:/var/cache/pip
# where each pair is "<device>:<guest mount path>".
mounts="$(grep -oE 'forkd\.mounts=[^ ]+' /proc/cmdline | head -1 | cut -d= -f2-)"
if [ -n "$mounts" ]; then
    IFS=',' read -ra _pairs <<<"$mounts"
    for pair in "${_pairs[@]}"; do
        dev="${pair%%:*}"
        target="${pair#*:}"
        if [ -z "$dev" ] || [ -z "$target" ] || [ "$dev" = "$target" ]; then
            echo "forkd-init: ignoring malformed mount entry '$pair'" >&2
            continue
        fi
        mkdir -p "$target"
        if ! mount "/dev/$dev" "$target" 2>/dev/null; then
            echo "forkd-init: WARN mount /dev/$dev -> $target failed" >&2
        fi
    done
fi

# Ubuntu Docker images symlink /etc/resolv.conf to a systemd-resolved
# stub that doesn't exist in our minimal init. Point to public resolvers
# so the guest can do DNS over the netns + host bridge NAT path.
rm -f /etc/resolv.conf
{
    # LAN resolvers first: split-horizon names (e.g. git.example.com must
    # resolve to the LAN edge, whose /v2/ registry path is not SSO-gated;
    # the public edge 302s it to the login portal). Public fallbacks after.
    echo "nameserver 10.0.0.2"
    echo "nameserver 10.0.0.1"
    echo "nameserver 1.1.1.1"
    echo "nameserver 8.8.8.8"
} > /etc/resolv.conf

# cgroup2. Services that size themselves from cgroups need the hierarchy
# mounted: seastar (ScyllaDB) refuses to start without it, failing with
# "Could not initialize seastar: std::out_of_range (sstring out of range)".
# Harmless for everything else.
mkdir -p /sys/fs/cgroup
mount -t cgroup2 cgroup2 /sys/fs/cgroup 2>/dev/null

# Image boot hooks (spoond #70). An image that must RUN something — a
# database a CI job connects to, an sshd — ships executables in
# /etc/forkd/init.d/. They run in lexical order BEFORE the agent starts, so a
# bake snapshots the service already up and every fork restores warm. A hook
# starts its daemon detached and returns once it is ready; each is bounded, and
# a failing hook is logged, never fatal — the agent must always come up.
if [ -d /etc/forkd/init.d ]; then
    for hook in /etc/forkd/init.d/*; do
        [ -f "$hook" ] && [ -x "$hook" ] || continue
        echo "forkd-init: hook $hook"
        if command -v timeout >/dev/null 2>&1; then
            timeout "${FORKD_HOOK_TIMEOUT:-180}" "$hook" >>/tmp/forkd-init-hooks.log 2>&1
        else
            "$hook" >>/tmp/forkd-init-hooks.log 2>&1
        fi
        rc=$?
        [ "$rc" -eq 0 ] || echo "forkd-init: WARN hook $hook exited $rc (see /tmp/forkd-init-hooks.log)" >&2
    done
fi

echo "forkd-init: launching agent..."
# Find python: Ubuntu has /usr/bin/python3; official python:* images have /usr/local/bin/python3.
for PY in /usr/local/bin/python3 /usr/bin/python3 /usr/local/bin/python /usr/bin/python; do
    if [ -x "$PY" ]; then
        exec "$PY" /forkd-agent.py
    fi
done
echo "forkd-init: ERROR: no python interpreter found in /usr/bin or /usr/local/bin" >&2
# Park PID 1 so the kernel doesn't panic. The agent won't be available
# but at least snapshot/restore plumbing still works for debugging.
exec sleep infinity
```

Summary of what the init does, in order:
1. Mount `/proc`, `/sys`, `/dev` (devtmpfs).
2. Mount `/tmp` as a 256m tmpfs.
3. Export a fixed PATH.
4. Mount volumes from the kernel cmdline `forkd.mounts=vdb:/path,...`.
5. **Rewrite `/etc/resolv.conf`**: nameservers `10.0.0.2`, `10.0.0.1`, `1.1.1.1`, `8.8.8.8`.
6. **Mount cgroup2** at `/sys/fs/cgroup`.
7. **Run hooks:** every executable file in `/etc/forkd/init.d/*` in lexical order, each under `timeout ${FORKD_HOOK_TIMEOUT:-180}`, output appended to `/tmp/forkd-init-hooks.log`. A non-zero exit is logged, never fatal.
8. `exec` python3 `/forkd-agent.py`; fall back to `sleep infinity` if there's no python.

**No network configuration happens in the init.** Guest IP and routing must come from the kernel `ip=` cmdline or from forkd; this can't be determined from the repo. **No devpts mount** here (dev-base's patched init adds one). **No sshd start** here.

### 10.6 deploy/rootfs-init/forkd-agent.py (guest agent, TCP 0.0.0.0:8888)

`deploy/rootfs-init/forkd-agent.py` L1-29:

```python
#!/usr/bin/env python3
"""forkd guest agent — runs as PID 1, warms state into memory, accepts
commands from the host via TCP on port 8888.

Protocol: each request is one JSON object terminated by '\n'. Response is
one JSON object terminated by '\n'. Multiple requests on one connection
are allowed.

Actions:
  {"action": "ping"}
    → {"pong": true, "numpy_version": "1.26.4", "pid": 1}

  {"action": "exec", "args": ["python3", "-c", "print(1+1)"], "timeout": 10}
    → {"stdout": "2\n", "stderr": "", "exit_code": 0}

  {"action": "eval", "code": "1 + numpy.zeros(3).sum()"}
    → {"result": "1.0", "exit_code": 0}

`eval` semantics depend on the recipe. By default the code is evaluated
as a Python expression against the agent's interpreter (numpy is in
scope when available). If /etc/forkd-recipe.env declares
`FORKD_AGENT_LANG=node`, the same action routes to a warm-up subprocess
(launched per `FORKD_WARMUP_CMD`) over a line-JSON bridge — used by the
playwright-browser recipe to evaluate JS against a warmed Chromium.

This file is copied into the rootfs at / by scripts/build-rootfs.sh, then
launched as PID 1 by /forkd-init.sh after the kernel finishes mounting
/proc /sys /dev.
"""
```

- **Environment:** reads `/etc/environment` (`KEY=VALUE`, quotes stripped). Its `PATH` overrides the agent PATH (default `/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`); other keys are added if they aren't already in `os.environ`. A per-request `env` is **merged over** that base (`_subprocess_env`, L105-120). The runner comment claiming the agent "REPLACES the environment" is not what this code does. Either way, the backend never sends env to forkd exec: env is `export`ed inside the bash `-c` string.
- **Optional recipe file** `/etc/forkd-recipe.env` (`FORKD_WARMUP_CMD`, `FORKD_AGENT_LANG`).
- **Protocol:** one JSON request line per connection, then a response.

| Action | Request | Response |
|---|---|---|
| `ping` | `{"action":"ping"}` | `{"pong":true,"numpy_version","pid","agent_lang","warmup_ready","path"}` |
| `exec` | `{"action":"exec","args":[...],"timeout":30,"env":{}?}` | `{"stdout","stderr","exit_code"}` (`subprocess.run`, capture) |
| `eval` | `{"action":"eval","code":"..."}` | `{"result":repr,"exit_code":0}` or `{"result_json"...}` (node bridge) or `{"error","traceback","exit_code":1}` |
| `stream` | `{"action":"stream","args","cwd","env","pty"(default true)}` | `{"stream":"started","pid","pty"}`, then `{"out":...}`*, then `{"exit_code":N}`; inbound `{"in":"..."}` writes stdin, inbound `{"action":"stop"}` terminates |
| other | | `{"error":"unknown action: X","exit_code":1}` |

`deploy/rootfs-init/forkd-agent.py` L302-340:

```python
def handle(conn: socket.socket, addr) -> None:
    try:
        line = _recv_line(conn)
        if not line:
            return
        cmd = json.loads(line)
        action = cmd.get("action")

        if action == "ping":
            _send_json(
                conn,
                {
                    "pong": True,
                    "numpy_version": NUMPY_VERSION,
                    "pid": os.getpid(),
                    "agent_lang": AGENT_LANG,
                    "warmup_ready": _warmup_ready,
                    "path": _CONTAINER_PATH,
                },
            )

        elif action == "exec":
            args = cmd["args"]
            timeout = cmd.get("timeout", 30)
            r = subprocess.run(
                args,
                capture_output=True,
                timeout=timeout,
                env=_subprocess_env(cmd.get("env")),
            )
            _send_json(
                conn,
                {
                    "stdout": r.stdout.decode("utf-8", "replace"),
                    "stderr": r.stderr.decode("utf-8", "replace"),
                    "exit_code": r.returncode,
                },
            )

```

`deploy/rootfs-init/forkd-agent.py` L486-515:

```python
def serve() -> None:
    # Retry bind — eth0 might not be fully up at startup.
    last_err = None
    for _ in range(30):
        try:
            s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            s.bind(("0.0.0.0", 8888))
            s.listen(128)
            break
        except OSError as e:
            last_err = e
            time.sleep(0.2)
    else:
        print(f"forkd: failed to bind 0.0.0.0:8888 after retries: {last_err}", flush=True)
        sys.exit(1)

    print("forkd: agent listening on 0.0.0.0:8888", flush=True)

    while True:
        try:
            conn, addr = s.accept()
            threading.Thread(target=handle, args=(conn, addr), daemon=True).start()
        except Exception as e:
            print(f"forkd: accept error: {e}", flush=True)
            time.sleep(0.1)


if __name__ == "__main__":
    serve()
```

**NOTE:** the forkd controller's exec wire field is `timeout_secs` (forkd/client.go) while the agent's is `timeout`. The controller translates; that code isn't in this repo.

---

## 11. deploy/

| File | Purpose |
|---|---|
| `README.md` | deployment walkthrough (backend env `/etc/forkd-backend.env`, gateway keys `/etc/forkd-gateway/keys`, runner env, watchdog) |
| `spoond-backend.service` | systemd unit: `spoond backend` |
| `spoond-runner.service` | systemd unit: `spoond runner` |
| `spoond-sshd-gateway.service` | systemd unit: `spoond gateway` |
| `spoond-watchdog.service` / `.timer` | oneshot `/usr/local/bin/forkd-spawn-watchdog.sh` every 5 min |
| `cfos-adapter.service` | CFOS adapter unit (`/opt/cfos-adapter/cfos-adapter`, `ADAPTER_ADDR=:8893`; references old `forkd-backend.service` name) |
| `forkd-spawn-watchdog.sh` | detect the forkd spawn outage (journal strings, empty pool), capture a diagnostics tarball, recover (kill firecrackers, clean daemon dirs, never `/var/run/netns`, restart) |
| `forkd-patched-rollout.sh` | build forkd from a branch of the jrimmer/forkd fork (`cargo build -p forkd-cli -p forkd-controller`) and install it; the restart is left to the operator |
| `install-spoond.sh` | installer for spoond (optionally `--with-forkd`), writes `FORKD_URL=http://127.0.0.1:8889` |
| `bake-py-base.sh`, `bake-js-base.sh`, `bake-elixir-release.sh`, `bake-scylla.sh` | bake snapshots ON the forkd host: `docker build`, then `forkd from-image ... --tag`, then verify via direct controller calls (`POST http://127.0.0.1:8889/v1/sandboxes {"snapshot_tag","n":1,"per_child_netns":true}`, `/ping`, `/exec`, `DELETE`) |
| `rebuild-dev-base.sh` | rebuild dev-base (snapshot via controller `{"tag","kernel","rootfs","rw","tap":"forkd-tap0","boot_wait_secs"}`), patch the init (UsePAM no, sshd, tmux hook), bake the gateway pubkey into authorized_keys, branch |
| `caddy-sandbox-forwardauth.conf` | staged Caddy forward-auth block for `*.sandbox.example.com` (review only) |
| `rootfs-init/forkd-init.sh`, `rootfs-init/forkd-agent.py` | guest PID 1 and agent (§10.5/10.6) |

`deploy/spoond-backend.service` L1-20:

```ini
[Unit]
Description=spoond lease API backend (forkd microVM lease service)
After=network-online.target forkd-controller.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=/opt/spoond/spoond backend
Restart=on-failure
RestartSec=3
# forkd-controller is on localhost; the lease API binds localhost by default.
Environment=FORKD_URL=http://127.0.0.1:8889
Environment=BIND_ADDR=127.0.0.1:8890
# Consumer tokens: token=consumer,comma-separated. Set via EnvironmentFile.
EnvironmentFile=-/etc/spoond-backend.env
User=root
Group=root

[Install]
WantedBy=multi-user.target
```

`deploy/spoond-runner.service` L1-16:

```ini
[Unit]
Description=spoond Forgejo Actions runner
After=network-online.target spoond-backend.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=/opt/spoond/spoond runner
EnvironmentFile=/etc/spoond-runner.env
# Persistent runner: stays registered across jobs and restarts.
# State file (RUNNER_STATE_FILE) preserves Forgejo UUID/token between restarts.
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

`deploy/spoond-sshd-gateway.service` L1-30:

```ini
[Unit]
Description=spoond SSH gateway (sandbox.example.com interactive access)
After=network.target spoond-backend.service
# Soft dependency: start after the backend, but do NOT tear down the
# gateway when the backend restarts (Requires= would stop us too — the
# backend restarts on sweep/failure and the gateway must keep serving
# long enough for its create-retry to ride through the blip).
Wants=spoond-backend.service

[Service]
# Credential hygiene (security review #37 rescan): the backend token
# lives in a 0600 EnvironmentFile, NOT in ExecStart — a world-readable
# unit or /proc/<pid>/cmdline must not expose the token (which is
# admin-equivalent: it can impersonate any user via X-Spoond-User-Id).
EnvironmentFile=/etc/spoond-gateway.env
ExecStart=/opt/spoond/spoond gateway \
  --listen :2222 \
  --host-key /etc/spoond-gateway/ssh_host_ed25519_key \
  --gateway-key /etc/spoond-gateway/gateway_ed25519 \
  --client-keys /etc/spoond-gateway/keys \
  --backend https://127.0.0.1:8890 \
  --backend-token ${SPOOND_GATEWAY_TOKEN} \
  --gateway-host sandbox.example.com
Restart=on-failure
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

`deploy/spoond-watchdog.service` L1-6:

```ini
[Unit]
Description=forkd spawn-outage watchdog (auto-recovery)

[Service]
Type=oneshot
ExecStart=/usr/local/bin/forkd-spawn-watchdog.sh
```

`deploy/spoond-watchdog.timer` L1-10:

```ini
[Unit]
Description=Run forkd spawn watchdog every 5 minutes

[Timer]
OnBootSec=5min
OnUnitActiveSec=5min
AccuracySec=30s

[Install]
WantedBy=timers.target
```

**NOTE:** the backend unit orders `After=forkd-controller.service` and runs as root (needed for `ip netns exec`/`setns`). The gateway also needs root/CAP_SYS_ADMIN for `setns`; its unit sets no `User=`, so it runs as root.

---

## 12. metrics/ (metrics/metrics.go)

All metrics use namespace `spoond`. The "used" column is whether any non-test code updates the metric. Many are defined but never set.

### Backend (`BackendMetrics`, served at backend `GET /metrics`)

| Metric | Type | Labels | Used |
|---|---|---|---|
| `spoond_pool_ready` | gauge | image | yes (CollectMetrics) |
| `spoond_pool_cap` | gauge | none | yes |
| `spoond_pool_refill_total` | counter | image | no |
| `spoond_pool_refill_failed_total` | counter | image | no |
| `spoond_pool_refill_duration_seconds` | histogram | image | no |
| `spoond_pool_evicted_total` | counter | image, reason | no |
| `spoond_leases_active` | gauge | none | yes |
| `spoond_leases_queued` | gauge | none | no |
| `spoond_leases_total` | counter | none | yes (grant) |
| `spoond_lease_grant_duration_seconds` | histogram | none | yes (measures ~0, see §3.9) |
| `spoond_lease_operations_total` | counter | op | no |
| `spoond_lease_swept_total` | counter | none | no |
| `spoond_lease_orphaned_total` | counter | none | yes (ReconcileOrphans) |
| `spoond_http_requests_total` | counter | path, method, code | yes |
| `spoond_http_request_duration_seconds` | histogram | path | yes |
| `spoond_llm_requests_total` | counter | provider | yes |
| `spoond_llm_request_duration_seconds` | histogram | provider | yes |
| `spoond_llm_errors_total` | counter | provider, code | yes |
| `spoond_llm_rate_limited_total` | counter | none | yes |
| `spoond_llm_key_auth_failures_total` | counter | none | yes |
| `spoond_llm_keyless_denied_total` | counter | none | no |
| `spoond_llm_inflight` | gauge | owner | yes |
| `spoond_proxy_requests_total` | counter | host | no |
| `spoond_proxy_forward_auth_total` | counter | result | no |
| `spoond_netpol_apply_total` | counter | policy | no |
| `spoond_netpol_apply_errors_total` | counter | none | no |
| `spoond_netpol_apply_duration_seconds` | histogram | none | no |
| `spoond_identity_users` | gauge | kind | yes |
| `spoond_identity_admins` | gauge | none | yes |
| `spoond_auth_failures_total` | counter | none | yes |
| `spoond_auth_throttled_total` | counter | none | yes |
| `spoond_quota_exceeded_total` | counter | none | yes |
| `spoond_quota_reservations` | gauge | none | yes |
| `spoond_shares_active` | gauge | none | yes |
| `spoond_busy_slots` | gauge | owner | yes |
| `spoond_builds_in_flight` | gauge | none | no |
| `spoond_builds_failed_total` | counter | none | no |

The backend `/metrics` also appends the forkd controller's `/metrics` text, renamed `forkd_*` → `spoond_controller_*` by `metrics.NamespaceControllerMetrics` (metrics.go L392-406).

### Gateway (`GatewayMetrics`, on `--metrics-listen`)

`spoond_ssh_sessions_active{image}` (no), `spoond_ssh_connections_total` (yes), `spoond_ssh_auth_failures_total{reason}` (yes; reasons `no_keys`, `identity_not_found`, `unknown_key`), `spoond_ssh_session_duration_seconds` (no), `spoond_ssh_ctl_commands_total{verb}` (no), `spoond_ssh_create_total{image}` (no), `spoond_ssh_image_resolution_total{source}` (no).

### Runner (`RunnerMetrics`, on `METRICS_LISTEN`)

`spoond_runner_jobs_active` (yes), `spoond_runner_jobs_total{result}` (yes), `spoond_runner_job_duration_seconds` (yes), `spoond_runner_exec_retries_total` (no), `spoond_runner_exec_errors_total{code}` (yes), `spoond_runner_checkout_duration_seconds` (no), `spoond_runner_sandbox_create_failed_total` (no).

---

## 13. Tests

### 13.1 Integration suite (tests/integration/)

These are bash tests run against a **live** stack (host): backend on `BE_API` (default `https://127.0.0.1:8890`, curl `-k`), a token from `TOKEN` or parsed from `/etc/spoond-backend.env` `CONSUMER_TOKENS`. `run.sh` builds `wsclient` (`go build` in `tests/integration/wsclient`, a separate module), optionally stages everything to `SSHHOST` and runs it there under `timeout 600`, runs the preflight (grant a dev-base lease), runs each file in order, then prints a summary from `RESULTS_FILE` (`/tmp/forkd-itest-results.txt`). Several files need root on the host: they edit `/etc/forkd-gateway/keys` and `systemctl restart spoond-sshd-gateway`.

`tests/integration/run.sh` L1-75:

```bash
#!/bin/bash
# run.sh — forkd integration test suite orchestrator.
#
# Runs the full integration suite against a live forkd stack (host).
# By default tests run locally against 127.0.0.1:8890; pass SSHHOST to
# stage and run on a remote host (e.g. SSHHOST=root@10.0.0.11).
#
# Usage:
#   tests/integration/run.sh              # run locally on the host
#   SSHHOST=root@10.0.0.11 tests/integration/run.sh   # from Hermes host
set -u
cd "$(dirname "$0")"
DIR="$(pwd)"
SSHHOST="${SSHHOST:-}"
BE_API="${BE_API:-https://127.0.0.1:8890}"

# Build the wsclient test binary.
export PATH="$PATH:/usr/local/go/bin"
echo "== building wsclient =="
(cd "$DIR/wsclient" && go build -o "$DIR/wsclient/wsclient" .) \
  || { echo "wsclient build failed"; exit 1; }
echo "wsclient built"

# If SSHHOST is set, stage everything there and run remotely.
if [ -n "$SSHHOST" ]; then
  echo "== staging to $SSHHOST =="
  ssh -o BatchMode=yes -o ConnectTimeout=6 "$SSHHOST" "mkdir -p /tmp/forkd-itest"
  scp -q -r "$DIR/." "$SSHHOST:/tmp/forkd-itest/"
  # gateway test needs the backend token in its env file; run.sh picks it up from /etc
  # The cap runs REMOTELY (host has GNU timeout; the local machine may
  # be macOS/zsh where `timeout` doesn't exist). If the remote run
  # hangs, timeout kills it and ssh returns.
  ssh -o BatchMode=yes -o ConnectTimeout=6 "$SSHHOST" \
    "cd /tmp/forkd-itest && timeout 600 env BE_API='$BE_API' bash run.sh"
  RC=$?
  exit $RC
fi

# Local run (on the host or wherever the backend is reachable).
source ./lib.sh
echo "== forkd integration suite =="
echo "backend: $BE_API  host: $(hostname)"
echo

# Truncate the shared results file so the summary reflects THIS run only.
: > "$RESULTS_FILE"

# Tolerate a cold pool: ensure at least one dev-base lease can be granted.
echo "== preflight: pool warm =="
L=$(new_lease dev-base 60 false)
if [ -n "$L" ]; then ok "pool grants dev-base"; del_lease "$L"; else bad "pool grants dev-base (cold pool?)"; fi
echo

run_file() { # run_file <name>
  echo "############################################################"
  echo "# $1"
  echo "############################################################"
  bash "$DIR/$1"
  echo
}

run_file test_lease_api.sh
run_file test_images.sh
run_file test_stream.sh
run_file test_gateway.sh
run_file test_ctl.sh
run_file test_ctl_new.sh
run_file test_identity.sh
run_file test_netpolicy.sh
run_file test_proxy.sh
run_file test_mcp.sh
run_file test_acp.sh
run_file test_stat_pretty.sh

summary
```

`tests/integration/lib.sh` L1-108:

```bash
#!/bin/bash
# lib.sh — shared helpers for forkd integration tests.
# Sourced by each test file. Requires: BE_API (backend base URL),
# TOKEN (consumer token), and optional SSHHOST for remote execution.
set -u

# Backend API (https, self-signed — always -k).
BE_API="${BE_API:-https://127.0.0.1:8890}"
TOKEN="${TOKEN:-}"
if [ -z "$TOKEN" ] && [ -f /etc/spoond-backend.env ]; then
  # Format: CONSUMER_TOKENS=<token>=<consumer-id> — take the part
  # before the first '=' after the key.
  TOKEN=$(grep -oE 'CONSUMER_TOKENS=[^ ]+' /etc/spoond-backend.env | cut -d= -f2- | cut -d= -f1)
fi

# Optional: run everything through ssh (tests run from Hermes host).
SSHHOST="${SSHHOST:-}"
run() { # run <cmd...> — locally or via ssh
  if [ -n "$SSHHOST" ]; then
    timeout 120 ssh -o BatchMode=yes -o ConnectTimeout=6 "$SSHHOST" "$@"
  else
    "$@"
  fi
}

# --- counters ------------------------------------------------------------
# Results are appended to $RESULTS_FILE (set by run.sh; truncated there)
# so that test files running in subshells contribute to the parent's
# summary. Without this the orchestrator only sees its own assertions.
RESULTS_FILE="${RESULTS_FILE:-/tmp/forkd-itest-results.txt}"
PASS=0; FAIL=0; FAILED_NAMES=()

ok()   { PASS=$((PASS+1)); echo "OK $1" >> "$RESULTS_FILE"; echo "  ✅ $1"; }
bad()  { FAIL=$((FAIL+1)); FAILED_NAMES+=("$1"); echo "FAIL $1" >> "$RESULTS_FILE"; echo "  ❌ $1"; }

assert_eq() { # assert_eq <desc> <actual> <expected>
  if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (got '$2', want '$3')"; fi
}
assert_contains() { # assert_contains <desc> <haystack> <needle>
  if echo "$2" | grep -q -- "$3"; then ok "$1"; else bad "$1 (missing '$3')"; fi
}
assert_success() { # assert_success <desc> <cmd...>
  if "$@" >/dev/null 2>&1; then ok "$1"; else bad "$1 (cmd failed: $*)"; fi
}
assert_status() { # assert_status <desc> <expected_code> <url>
  local code
  code=$(curl -sk -o /dev/null -w '%{http_code}' "${@:3}")
  assert_eq "$1" "$code" "$2"
}

# --- API helpers ---------------------------------------------------------
api() { # api <method> <path> [data]
  local method="$1" path="$2" data="${3:-}"
  if [ -n "$data" ]; then
    curl -sk -X "$method" "$BE_API$path" -H "Authorization: Bearer $TOKEN" \
      -H "Content-Type: application/json" -d "$data"
  else
    curl -sk -X "$method" "$BE_API$path" -H "Authorization: Bearer $TOKEN"
  fi
}

# new_lease <image> [ttl] [persistent] -> echoes lease id (or empty)
new_lease() {
  local image="${1:-dev-base}" ttl="${2:-120}" pers="${3:-false}"
  local body
  body=$(api POST /api/sandboxes "{\"image\":\"$image\",\"ttl\":$ttl,\"persistent\":$pers}")
  echo "$body" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null
}

del_lease() { api DELETE "/api/sandboxes/$1" >/dev/null; }

# lease_image <id> -> echoes image tag
lease_image() {
  api GET "/api/sandboxes" | python3 -c "
import sys,json
d=json.load(sys.stdin)
for l in d.get('sandboxes', d):
    if l.get('id')=='$1': print(l.get('image',''))
" 2>/dev/null
}

# wait_exec_ok <id> <cmd> — poll exec until stdout contains needle or timeout
wait_agent() {
  local id="$1" needle="${2:-pong}" tries="${3:-12}"
  for i in $(seq 1 "$tries"); do
    sleep 3
    local out
    out=$(api POST "/api/sandboxes/$id/exec" "{\"cmd\":\"echo ready\"}" 2>/dev/null)
    if echo "$out" | grep -q "$needle"; then return 0; fi
  done
  return 1
}

summary() {
  # Aggregate results contributed by subshell test files (results file).
  PASS=$(grep -c '^OK ' "$RESULTS_FILE" 2>/dev/null || true)
  FAIL=$(grep -c '^FAIL ' "$RESULTS_FILE" 2>/dev/null || true)
  FAILED_NAMES=($(grep '^FAIL ' "$RESULTS_FILE" 2>/dev/null | sed 's/^FAIL //'))
  echo
  echo "==================== SUMMARY ===================="
  echo "PASS: $PASS   FAIL: $FAIL"
  if [ "$FAIL" -gt 0 ]; then
    printf 'Failed: %s\n' "${FAILED_NAMES[@]}"
    return 1
  fi
  echo "ALL TESTS PASSED"
  return 0
}
```

| File | Asserts |
|---|---|
| `test_lease_api.sh` | healthz 200 unauth; 401 without/bad token; images lists dev-base/elixir-base/go-base/llm-review/py-base (and needs auth); create dev-base; list shows the lease; lease image and non-persistent; exec output; **exec returns guest hostname containing "10.42"**; **endpoint has `netns`, `guest_addr`, `image`**; agent reachable; unknown image rejected; unknown lease exec 404; short-TTL lease swept; persistent lease survives the sweep window; keepalive extends; delete then 404 on second delete |
| `test_images.sh` | per image (dev-base, go-base, py-base, elixir-base, llm-review): create lease plus a toolchain check (tmux/sshd running, `go version`/compile, python3/pip, elixir, llm-review shell); **"agent has stream action" via `grep -c stream /forkd-agent.py`** |
| `test_stream.sh` | via `wsclient`: started frame, incremental output frames 1-3, exit_code reported, client sees completion; stdin mode round-trips |
| `test_gateway.sh` | temporary key in the `/etc/forkd-gateway/keys` allowlist; `new@` creates a sandbox (MOTD) and drops into the guest; **guest hostname contains "10.42"**; reattach reaches the same sandbox; pty exec; unknown lease, unknown image and CI image rejected |
| `test_ctl.sh` | `ssh ctl@`: help, whoami (pretty/json), new, ls (pretty/json), comment set/clear, keepalive, suspend (**checks `curl http://127.0.0.1:8889/v1/workspaces` shows suspended**), exec on suspended gives 409, resume (**controller shows running**), exec after resume, cp/clone plus branch tag (**then `DELETE http://127.0.0.1:8889/v1/snapshots/<branchtag>`**), rm, unknown command rejected |
| `test_ctl_new.sh` | tag assigns a name; duplicate name rejected; name resolves; name-based ssh; restart returns JSON; exec after restart; **proxy after restart reaches guest network (502 = connected, nothing listening)**; prompt without agent returns a conflict |
| `test_identity.sh` | create users (admin/bootstrap), duplicate rejected, non-admin create rejected, impersonated create owner (gateway token plus `X-Spoond-User-Id`), unknown impersonation 403, cross-owner list isolation and rm denied, quota cap 429, TTL clamp |
| `test_netpolicy.sh` | `none` blocks LAN egress (and is **re-applied after restart**); `lan` allows LAN; `internet` allows egress; `restricted` allows an allowlisted IP and blocks others; unknown policy rejected; "restricted without allowlist rejected" (see the §5.4 ambiguity) |
| `test_proxy.sh` | start `python3 -m http.server` in the guest; fetch `https://<id>.sandbox.example.com` through Caddy; custom `-<port>` plus Host passthrough; unknown lease 404 |
| `test_mcp.sh` | `spoond mcp` stdio: initialize, tools/list has shell and read_file, shell runs in a sandbox (returns sandbox_id), write_file ok |
| `test_acp.sh` | `spoond acp` stdio: initialize, protocol version, session/new, session/prompt returns a result/stop reason or a JSON-RPC error without hanging |
| `test_stat_pretty.sh` | ctl stat returns cpu/mem/net (pretty and `--json`); ctl ls pretty header and truncated id; ls `--json` is the sandboxes array; whoami pretty and json |

Direct forkd-controller coupling in the integration tests: `test_ctl.sh` L86, L97 (`GET :8889/v1/workspaces`) and L120 (`DELETE :8889/v1/snapshots/...`). The `10.42` assertions are in `test_lease_api.sh` L39 and `test_gateway.sh` L32 and L41. The netns assertion is in `test_lease_api.sh` L44. `/forkd-agent.py` is referenced in `test_images.sh` L55.

### 13.2 Go unit tests

`go test ./...` is run by CI (`.forgejo/workflows/test.yml`: gofmt check, `go vet ./...`, `go build ./...`, `go test ./...`, on `runs-on: go`) and documented in CONTRIBUTING/README. **No build tags are needed for tests.** Build tags in the tree: `api/netns_linux.go` (`linux`) and `api/netns_other.go` (`!linux`); gateway `main.go` (`linux`) and `ctl_other.go` (`!linux`); the `cmd/spoond/*.go` exclusion tags (`nobackend`, `nogateway`, `noacp`, `nomcp`, `norunner`, `noctl`, `nodoctor`).

Test packages (tests per file):
- `api` (16 files). Tests use `fakeForkd`, which implements `ForkdClient`, in `api/server_test.go` L20-172, plus `fakeNetpol` (`netpolicy_test.go` L90) and `fakeExposer` (`expose_test.go` L90).
  - server_test 31
  - security_fixes 10
  - users 7
  - expose 6
  - probe 6
  - rescan_fixes 6
  - proxy_auth 5
  - quota 5
  - gateway_scoping 4
  - netpolicy 4
  - shares 4
  - llmgateway 3
  - owner 3
  - proxy_host 2
  - proxy 1
  - proxy_scoping 1
- `forkd` (client_test 8, httptest server)
- `identity` (store 12, salt 3)
- `runner` (executor 15, executor_security 1, expr 6, forgejo_adapter 4, jobrecord 4, lease_adapter 1, pool 4)
- `mcp` (5), `acp` (7), `cfos` (7), `commandadapter` (5)
- `cmd/spoond-sshd-gateway` (main_test 6)

`api/server_test.go` L20-43:

```go
type fakeForkd struct {
	snapshots     []forkd.SnapshotInfo
	sandboxes     map[string]forkd.SandboxInfo
	workspaces    map[string]*forkd.WorkspaceInfo
	nextID        int
	killed        []string
	suspended     []string
	resumed       []string
	deadSandboxes map[string]bool
	netns         string // reported for spawned sandboxes ("" = none)
	execStdout    string // canned stdout for Exec ("" = default "ok\n")
	// probeFail marks sandboxes whose integrity probe fails, by sandbox id;
	// probeFailAll fails the probe for every sandbox (fresh spawns too).
	probeFail    map[string]string
	probeFailAll bool
}

func newFakeForkd() *fakeForkd {
	return &fakeForkd{
		snapshots:  []forkd.SnapshotInfo{{Tag: "py-base", Bootable: true}},
		sandboxes:  make(map[string]forkd.SandboxInfo),
		workspaces: make(map[string]*forkd.WorkspaceInfo),
	}
}
```

---

## 14. runner/ (Forgejo runner): how it uses the lease API

- **Port:** `runner.SandboxProvider` (runner/ports.go L69-73); adapter `HTTPLeaseClient` (runner/lease_adapter.go).

`runner/ports.go` L60-73:

```go
type ExecResult struct {
	Stdout string
	Stderr string
	Exit   int
}

// SandboxProvider grants and releases sandboxes. It is the port the
// executor uses to obtain compute. Adapters: lease HTTP API, direct
// forkd client, exe.dev backend.
type SandboxProvider interface {
	Create(ctx context.Context, image string, ttl int) (string, error)
	Exec(ctx context.Context, id, cmd, cwd string, env map[string]string, timeout int) (*ExecResult, error)
	Delete(ctx context.Context, id string) error
}
```

`runner/lease_adapter.go` L1-139:

```go
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// HTTPLeaseClient is a SandboxProvider backed by the forkd-backend
// lease HTTP API.
type HTTPLeaseClient struct {
	BaseURL   string
	Token     string
	Client    *http.Client
	NetPolicy string   // egress policy: none|lan|internet|restricted (default: internet for CI)
	NetAllow  []string // allowlist IPs/CIDRs for restricted policy
}

// NewHTTPLeaseClient builds a lease API adapter.
func NewHTTPLeaseClient(baseURL, token string) *HTTPLeaseClient {
	return &HTTPLeaseClient{
		BaseURL:   baseURL,
		Token:     token,
		Client:    &http.Client{Timeout: 600 * time.Second},
		NetPolicy: "lan", // CI sandboxes need LAN egress to reach Forgejo
	}
}

// SetHTTPTimeout raises the lease client's overall timeout so long exec
// calls (EXEC_TIMEOUT_SECS) are not cut at the default 600s by the
// runner's own HTTP client. Must exceed the backend's exec timeout.
func (c *HTTPLeaseClient) SetHTTPTimeout(d time.Duration) {
	c.Client.Timeout = d
}

// Create grants a new sandbox lease.
func (c *HTTPLeaseClient) Create(ctx context.Context, image string, ttl int) (string, error) {
	payload := map[string]any{"image": image, "ttl": ttl}
	if c.NetPolicy != "" {
		payload["network_policy"] = c.NetPolicy
	}
	if len(c.NetAllow) > 0 {
		payload["egress_allowlist"] = c.NetAllow
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/sandboxes", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("create sandbox: status %d", resp.StatusCode)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// Exec runs a command in a sandbox. It retries transient backend errors
// (500/502/503) up to 2 times with 1s/2s backoff. Permanent errors (410
// Gone — sandbox no longer exists in the controller) and client errors
// (400/401/403/404) are returned immediately.
func (c *HTTPLeaseClient) Exec(ctx context.Context, id, cmd, cwd string, env map[string]string, timeout int) (*ExecResult, error) {
	body, _ := json.Marshal(map[string]any{"cmd": cmd, "cwd": cwd, "env": env, "timeout": timeout})
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, lastErr
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		// Fresh reader each attempt — reusing a consumed body reader
		// produces "ContentLength=N with Body length 0" on retry.
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/sandboxes/"+id+"/exec", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			// 410 Gone = sandbox permanently dead; don't retry.
			// 500/502/503 = transient backend error; retry.
			// Other non-200 = client error; don't retry.
			if resp.StatusCode == http.StatusGone ||
				resp.StatusCode < 500 {
				return nil, fmt.Errorf("exec: status %d", resp.StatusCode)
			}
			lastErr = fmt.Errorf("exec: status %d", resp.StatusCode)
			continue
		}
		var out ExecResult
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			resp.Body.Close()
			return nil, err
		}
		resp.Body.Close()
		return &out, nil
	}
	return nil, lastErr
}

// Delete releases a sandbox.
func (c *HTTPLeaseClient) Delete(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.BaseURL+"/api/sandboxes/"+id, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("delete sandbox: status %d", resp.StatusCode)
	}
	return nil
}
```

- **Calls made:** `POST /api/sandboxes {"image","ttl","network_policy","egress_allowlist"?}` (expects **201**), `POST /api/sandboxes/{id}/exec {"cmd","cwd","env","timeout"}` (retries 500-class up to 3 attempts; **410 and 4xx are fatal**), `DELETE /api/sandboxes/{id}` (expects **204**).
- **Never used:** persistent, stream, keepalive, `expose_ports`, service leases.
- **No scylla or service leases in the runner.** Nothing in `runner/` or `cmd/spoond-runner` requests `scylla` or `expose_ports`. The manifest says scylla is "leased per job with expose_ports [9042] and reached from the job's sandbox (#70)". How a job does that (e.g. by calling the lease API from inside the job with a token) is **not in this repo**; presumably it lives in the consuming repo's workflow (cytale). **AMBIGUOUS.**
- **Net policy:** `NewHTTPLeaseClient` defaults `NetPolicy: "lan"`, but `cmd/spoond-runner` overrides it with `LEASE_NETPOL` (default **`internet`**) and `LEASE_NET_ALLOW` (comma list).

`cmd/spoond-runner/main.go` L140-168:

```go

	// newWorker builds a fresh ForgejoAdapter + Executor per worker so
	// each registered runner has its own auth headers and job loop.
	newWorker := func() runner.RunnerWorker {
		proto := runner.NewForgejoAdapterWithInternal(forgejoURL, envOr("REPO_BASE_URL", ""), nil)
		lease := runner.NewHTTPLeaseClient(leaseURL, leaseToken)
		// Keep the lease client's HTTP timeout above the per-step exec
		// timeout, or long CI steps die at the client's own 600s cap.
		if stepTimeout > 0 {
			lease.SetHTTPTimeout(time.Duration(stepTimeout+120) * time.Second)
		}
		lease.NetPolicy = envOr("LEASE_NETPOL", "internet")
		if v := os.Getenv("LEASE_NET_ALLOW"); v != "" {
			lease.NetAllow = strings.Split(v, ",")
			for i, a := range lease.NetAllow {
				lease.NetAllow[i] = strings.TrimSpace(a)
			}
		}
		exec := &runner.Executor{
			Sandbox:      lease,
			Metrics:      runnerMetrics,
			Sink:         proto,
			Labels:       imageMap,
			DefaultImage: defaultImage,
			TTL:          ttl,
			RepoBaseURL:  envOr("REPO_BASE_URL", "https://git.example.com"),
			StepTimeout:  stepTimeout,
			RecordDir:    envOr("JOB_RECORD_DIR", "/var/lib/spoond/jobs"),
		}
```

- **Runner env** (cmd/spoond-runner/main.go header): `FORGEJO_URL`, `RUNNER_TOKEN`, `RUNNER_NAME` (forkd-runner), `RUNNER_LABELS` (ubuntu-latest), `LEASE_URL` (`http://127.0.0.1:8890`), `LEASE_TOKEN`, `IMAGE_MAP` (label=image), `DEFAULT_IMAGE` (py-base), `REPO_BASE_URL` (`https://git.example.com`), `LEASE_TTL` (600), `EXEC_TIMEOUT_SECS` (300), `RUNNER_FLOOR/MAX/SCALE_STEP` (3/12/3), `SCALE_UP_DELAY`/`SCALE_DOWN_DELAY`, `RUNNER_STATE_FILE` (`/var/lib/spoond/runner-state.json`), `FORGEJO_ADMIN_TOKEN`, `METRICS_LISTEN`, `LEASE_NETPOL`, `LEASE_NET_ALLOW`, `JOB_RECORD_DIR` (`/var/lib/spoond/jobs`).
- **Image selection:** the first `runs-on` label found in `IMAGE_MAP`, else `DefaultImage` (executor.go L447-454).
- **Guest assumptions** (executor.go): one lease per job, executed as root; workspace `/workspace`; checkout = `git clone --depth 1 <REPO_BASE_URL>/<repo>.git /workspace`, run in the guest (needs `git` in the image and egress to git.example.com; auth via `GIT_CONFIG_COUNT` env with `GITHUB_TOKEN`). Env injected into every step unless already set: `CI_REPO_OWNER`, `CI_REPO_NAME`, `CI_COMMIT`, **`CI=true`**, `PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`, `COREPACK_ENABLE_DOWNLOAD_PROMPT=0`, `USER=root`, `LOGNAME=root`, `GITHUB_RUN_ID`, `GITHUB_RUN_NUMBER`, `GITHUB_SHA`, `GITHUB_REF`, `CI_PULL_REQUEST`. Commands run under `/bin/bash -c` (the backend's `buildShellArgs`), so the images need bash. **No docker inside the guest** (elixir-release bakes kaniko because "sandboxes have no docker"). The guest agent PATH/`/etc/environment` caveats are in the manifest and elixir-release.dockerfile notes (§10).

`runner/executor.go` L150-175:

```go
			}
		}
		if env["CI_COMMIT"] == "" {
			env["CI_COMMIT"] = ctx2.Eval("${{ github.sha }}")
		}
		// CI=true signals "non-interactive" to every tool (pnpm and friends
		// otherwise prompt on /dev/console — which never EOFs — and hang
		// forever; cost us a full debugging cycle on the cytale pipeline).
		if env["CI"] == "" {
			env["CI"] = "true"
		}
		// GitHub Actions always provides PATH; the guest agent REPLACES the
		// environment with the step's env map when one is supplied, and its
		// own default PATH is not applied to that case — so a step env
		// without PATH loses /usr/bin entirely ("date: command not found"
		// in otherwise-green jobs, infra#26).
		if env["PATH"] == "" {
			env["PATH"] = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
		}
		// Corepack aborts (exit 1) when it must fetch the packageManager-
		// pinned tool and no TTY is available for its download prompt. The
		// sandbox has registry egress, so let it download silently.
		if env["COREPACK_ENABLE_DOWNLOAD_PROMPT"] == "" {
			env["COREPACK_ENABLE_DOWNLOAD_PROMPT"] = "0"
		}
		// Identity vars vanish with the same env replacement. Tests that
```

`runner/executor.go` L379-445:

```go
func (e *Executor) checkout(ctx context.Context, sandboxID, ws string, job *Job, ctx2 *EvalContext, stepState *StepState, logIndex *int64) error {
	repo := ctx2.Eval("${{ github.repository }}")
	if repo == "" {
		stepState.Result = ResultFailure
		e.log(ctx, job, *logIndex, "checkout: github.repository is empty")
		*logIndex++
		stepState.LogLength = 1
		return fmt.Errorf("checkout: github.repository is empty")
	}
	base := e.RepoBaseURL
	if base == "" {
		base = "https://git.example.com"
	}
	base = strings.TrimRight(base, "/")
	cloneURL := base + "/" + repo + ".git"

	// Auth via GITHUB_TOKEN (provided by Forgejo in job secrets), sent
	// as an extra header so the token never appears in the URL. Passed
	// via GIT_CONFIG_COUNT env — git's native multi-config mechanism —
	// rather than interpolated into the command string (security review
	// #37 C3). The token charset is validated up front and the value is
	// single-quoted, so no shell metacharacter in the token can break
	// out of the env assignment into command injection.
	token := ctx2.Eval("${{ secrets.GITHUB_TOKEN }}")
	authEnv := ""
	if token != "" {
		if !regexp.MustCompile(`^[A-Za-z0-9_.\-]+$`).MatchString(token) {
			e.log(ctx, job, *logIndex, "checkout: GITHUB_TOKEN contains characters unsafe for shell env (not a standard token)")
			*logIndex++
			stepState.Result = ResultFailure
			stepState.LogLength = 1
			return fmt.Errorf("checkout: GITHUB_TOKEN has unsafe characters")
		}
		authEnv = "GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=http.extraheader GIT_CONFIG_VALUE_0='Authorization: token " + token + "' "
	}

	// Ensure the workspace exists and is clean, then clone into it.
	// The warm pool reuses the same rootfs across jobs, so /workspace may
	// hold a previous job's checkout (and its _build artifacts). Remove it
	// first so `git clone` never fails with "destination path already exists".
	cmds := []string{
		"rm -rf " + ws,
		"mkdir -p " + ws,
		authEnv + "git clone --depth 1 " + cloneURL + " " + ws,
	}
	for _, c := range cmds {
		res, err := e.Sandbox.Exec(ctx, sandboxID, c, "", nil, 300)
		if err != nil {
			stepState.Result = ResultFailure
			e.log(ctx, job, *logIndex, "checkout: "+err.Error())
			*logIndex++
			stepState.LogLength = 1
			return err
		}
		rows := splitLog(res.Stdout, res.Stderr)
		if len(rows) > 0 {
			*logIndex += e.logLines(ctx, job, *logIndex, rows)
			stepState.LogLength += int64(len(rows))
		}
		if res.Exit != 0 {
			stepState.Result = ResultFailure
			return fmt.Errorf("checkout: command failed: %s", c)
		}
	}
	return nil
}

```

---

## 15. mcp/, acp/, cfos/, commandadapter/, workflow/, identity/

None of these import `github.com/jrimmer/spoond/forkd`; verified by grep, and the only importers are listed in §0.2.

- **mcp/** (`mcp/server.go`, `mcp/http.go`; wired by `cmd/spoond-dev-mcp` = `spoond mcp`). Depends only on `runner.SandboxProvider`, built with `runner.NewHTTPLeaseClient(FORKD_BACKEND_URL (default https://127.0.0.1:8890), FORKD_AGENT_TOKEN|FORKD_TOKEN)`, image `FORKD_IMAGE` (default `dev-base`). Lease API calls: `POST /api/sandboxes` (`Create(ctx, s.image, s.ttl)`, server.go L295) with the client's default `network_policy: "lan"`; `POST /api/sandboxes/{id}/exec` for every tool (shell, read/write/edit file via python3 heredocs, so the image needs python3); `DELETE /api/sandboxes/{id}` on release. Transport is stdio or `MCP_TRANSPORT=http` (`MCP_LISTEN` :9090, `MCP_AUTH_TOKEN`, `MCP_PATH` /mcp).
- **acp/** (`acp/server.go`, `acp/agent.go`; wired by `cmd/spoond-acp` = `spoond acp`). Uses `runner.SandboxProvider` (same `HTTPLeaseClient`, same env vars, lease TTL 1800, max 12 turns). Calls: `POST /api/sandboxes` on session/new (`Create(ctx, image, 1800)`), `POST .../exec` for the tools `shell` (60s), `read_file` (base64, 30s) and `write_file` (30s), `DELETE` on session close. It also calls the **LLM gateway** directly: `POST <FORKD_BACKEND_URL>/llm/<lease-id>/openai/chat/completions` (acp/agent.go L83), model `FORKD_LLM_MODEL` (default `gpt-oss-20b-fireworks`). No netns or forkd access.
- **cfos/** (`cfos/adapter.go`, `cfos/image.go`; wired by `cmd/cfos-adapter`). HTTP `POST /v1/execute`; uses `SandboxProvider`: `Create(ctx, image, ttl)` (L135), `Exec(ctx, id, cmd, "", nil, timeout)` (L146), `Delete` (L143). It writes the code to `/tmp/main.{mjs,go,py}` via a heredoc and runs node, `go run` or python3. The lease client is `runner.NewHTTPLeaseClient(LEASE_URL, LEASE_TOKEN)` with the default `lan` policy.
- **commandadapter/** (`commandadapter/server.go`). Synchronous run-command HTTP server over `SandboxProvider`: `Create` (L156), deferred `Delete` (L161), `Exec(ctx, id, req.Command, req.Cwd, req.Env, timeout)` (L163). **Not wired into any binary.**
- **workflow/** (`workflow/*.go`, `workflow/action/*.go`). Pure YAML types, expression eval and action handlers (checkout, run, setup_go, setup_node). **No lease API calls, no network; not imported by any other package.**
- **identity/**. User store only (JSON file). **No lease API or forkd calls.** It's used by `api` (token/user resolution, quotas, LLM keys) and by the gateway (`identity.FingerprintSHA256`).

---

## 16. Every reference to forkd / netns / 8888 / /etc/forkd / forkd-tap0 / veth0 / 10.42. / 10.43. (outside docs/)

Comment-only mentions of the word "forkd" in Go files, and the `forkd` package itself, are omitted; functional references are listed. Docs mentions: `docs/api.md` (L37, 44, 49, 54, 113), `docs/usage.md` (L13, 90), `docs/security.md` (L16-22), `docs/setup.md`, `docs/operations.md`, `docs/ci-jobs.md`, `docs/design/u7-*.md`, `docs/design/u8-*.md`, `docs/substrate-backends.md`, `docs/plans/*`.

### Go code

| File:line | Reference |
|---|---|
| `api/service.go:20` | `import "github.com/jrimmer/spoond/forkd"` |
| `api/service.go:30-31` | `Lease.ForkdID`, `Address // e.g. "10.42.0.2:8888"` |
| `api/service.go:98-114` | `ForkdClient` interface |
| `api/service.go:118` | `Service.forkd ForkdClient` |
| `api/service.go:209-252` | `applyNetpol` (uses `ep.Netns`, `ep.GuestHost`) |
| `api/service.go:574, 1013` | `CreateWorkspace(ctx, "ws-"+lease.ID, ..., true)` (workspace naming `ws-<leaseID>`) |
| `api/service.go:1073-1104` | `Endpoint{ForkdID, Netns, GuestAddr, GuestHost}`, `resolveEndpoint` ("agent port is always 8888") |
| `api/server.go:21` | `import ".../forkd"` (for `*forkd.Error` in handleExec L1105) |
| `api/server.go:244` | `s.svc.forkd.Metrics` |
| `api/server.go:502-508` | `hostBridgeAllow = []string{"10.43.0.1"}` |
| `api/server.go:645-648` | endpoint JSON `forkd_id`, `netns`, `guest_addr` |
| `api/server.go:725-726` | `agentAddr := net.JoinHostPort("127.0.0.1", "8888")`; `dialInNetns(ep.Netns, agentAddr)` |
| `api/server.go:994, 1098, 1146, 1293` | direct `s.svc.forkd.Exec` / `.Branch` |
| `api/netpolicy.go` (whole) | `ip netns exec`, `veth0`, `forkd-tap0`, `forkd-netpolicy:*`, `forkd-expose`, reserved 8888/9000, `10.43.0.0/16` |
| `api/netns_linux.go:28` | `/var/run/netns/<netns>` setns |
| `api/proxy.go:39, 46` | comments `http://10.43.0.1:8891/llm/...`, `/assets/` |
| `api/proxy.go:194-227` | `resolveEndpoint` plus `dialInNetns(ep.Netns, target)` |
| `api/images.go:13, 37, 44` | `ImageRegistry.forkd` → `SnapshotExists`, `ListSnapshots` |
| `cmd/spoond-backend/main.go:35, 71-72, 99-107` | forkd import, `FORKD_URL`, `FORKD_TOKEN`, `FORKD_HTTP_TIMEOUT_SECS`, `forkd.NewClient` |
| `cmd/spoond-doctor/main.go:117-142, 274-278` | `FORKD_URL`, `forkd.NewClient`, `ListSandboxes` |
| `cmd/spoond-sshd-gateway/main.go:83, 87` | `http://10.43.0.1:8891/assets/shelley`, `http://10.43.0.1:8891/llm/` |
| `cmd/spoond-sshd-gateway/main.go:105-110` | `endpoint{ForkdID "forkd_id", Netns "netns", GuestAddr "guest_addr", Image}` |
| `cmd/spoond-sshd-gateway/main.go:209, 217-218, 234-236, 288-290` | SSH permission extension keys `forkd-key-id`, `forkd-user-id`, `forkd-user-name` |
| `cmd/spoond-sshd-gateway/main.go:545-547` | `if port == "8888" { port = "22" }` |
| `cmd/spoond-sshd-gateway/main.go:557-565` | `/var/run/netns` setns |
| `metrics/metrics.go:392-406` | `forkd_` → `spoond_controller_` rewrite |
| `api/server_test.go`, `api/probe_test.go`, `forkd/client_test.go` | fakes and tests of forkd types |

### Non-Go

| File:line | Reference |
|---|---|
| `deploy/spoond-backend.service:3, 12` | `After=... forkd-controller.service`, `FORKD_URL=http://127.0.0.1:8889` |
| `deploy/cfos-adapter.service:3-4` | `forkd-backend.service` (old name) |
| `deploy/rootfs-init/forkd-init.sh:67-83` | `/etc/forkd/init.d` hook runner; `FORKD_HOOK_TIMEOUT` |
| `deploy/rootfs-init/forkd-agent.py:493-503` | bind `0.0.0.0:8888`; `/etc/forkd-recipe.env` |
| `images/scylla.dockerfile:17` | `COPY --chmod=755 scylla-init-hook.sh /etc/forkd/init.d/50-scylla` |
| `images/scylla-init-hook.sh:27` | `--broadcast-rpc-address=10.42.0.2` |
| `images/manifest.yaml` | `/var/cache/forkd/*.ext4` rootfs paths; `forkd from-image` notes; `/etc/forkd/init.d` |
| `deploy/bake-*.sh`, `deploy/rebuild-dev-base.sh` | direct controller calls `http://127.0.0.1:8889/v1/...`, `per_child_netns`, `"tap":"forkd-tap0"`, `FORKD_SCRIPTS_DIR=/usr/local/share/forkd-scripts`, `/etc/forkd-gateway/gateway_ed25519.pub`, `FORKD_NO_TMUX` |
| `deploy/forkd-spawn-watchdog.sh` | firecracker/netns diagnostics, `:8889/v1/sandboxes` |
| `deploy/forkd-patched-rollout.sh` | builds forkd-cli and forkd-controller |
| `deploy/install-spoond.sh:10-11, 55, 84-87` | `FORKD_URL`, `--with-forkd` |
| `deploy/README.md:31-40, 79, 111-129, 167, 192` | `/etc/forkd-backend.env`, `/etc/forkd-gateway/keys`, `/etc/forkd-runner.env`, netns notes |
| `tests/integration/test_ctl.sh:86, 97, 120` | `:8889/v1/workspaces`, `:8889/v1/snapshots/...` |
| `tests/integration/test_lease_api.sh:39, 44` | `"10.42"`, `"netns"` |
| `tests/integration/test_gateway.sh:8, 14, 32, 41` | `/etc/forkd-gateway/keys`, `"10.42"` |
| `tests/integration/test_ctl.sh:8`, `test_ctl_new.sh:8`, `test_stat_pretty.sh:14` | `/etc/forkd-gateway/keys` |
| `tests/integration/test_images.sh:55` | `grep -c stream /forkd-agent.py` |
| `scripts/forkd-curl` | `FORKD_API`, `FORKD_TOKEN_FILE`, `FORKD_TOKEN` (lease API, not the controller) |
| `README.md:13, 54, 147, 150` | netns slots, `10.43.0.1`, `FORKD_GATEWAY_HOST` |
| `images/README.md:83-84` | `/etc/forkd-backend.env`, `/etc/forkd-runner.env` |
| `.forgejo/scripts/llm-review.py:172` | review prompt mentions netns |

**Env-var names with a FORKD_ prefix that consumers depend on** (renaming them is a compatibility decision):
- `FORKD_URL`, `FORKD_TOKEN`, `FORKD_HTTP_TIMEOUT_SECS` (backend)
- `FORKD_GATEWAY_HOST` (gateway)
- `FORKD_BACKEND_URL`, `FORKD_AGENT_TOKEN`, `FORKD_TOKEN`, `FORKD_IMAGE`, `FORKD_LLM_MODEL` (acp/mcp)
- `FORKD_CTL_HOST/PORT/KEY` (spoondctl)
- `FORKD_HOOK_TIMEOUT`, `FORKD_WARMUP_CMD`, `FORKD_AGENT_LANG`, `FORKD_NO_TMUX` (guest)

---

## 17. Open ambiguities (consolidated)

1. **Stream dial target.** `handleStream` dials `127.0.0.1:8888` inside the lease netns, but the agent binds `0.0.0.0:8888` inside the VM (§5.6). Unclear how this works in production.
2. **Thread unlock after setns.** `dialInNetns` and `dialSandbox` unlock the OS thread after `setns` without restoring the netns (§5.6).
3. **Restricted without an allowlist.** The code can't reject it (10.43.0.1 is always appended), yet an integration test expects a rejection (§5.4).
4. **Plain-persistent restart netns flag.** `restart` of a plain persistent lease spawns with `perChildNetns=false` (§3.10).
5. **Non-persistent clone skips probe and policy.** `grantFromSnapshot` on the non-persistent path skips probe and netpol. Clone always uses persistent, where netpol defaults to `lan`, not `restricted` (§3.13).
6. **Pool-served leases have no Address.** `Lease.Address` is empty for warm-pool-served leases (§3.9).
7. **Leak on netpol failure.** A sandbox leaks when `applyNetpol` fails after grant (§3.9).
8. **Shutdown deletes persistent workspaces.** Backend shutdown deletes all persistent workspaces, and there is no lease persistence (§0.1, §8).
9. **Scylla/`expose_ports` consumer is outside the repo.** How CI jobs lease scylla with `expose_ports` isn't here (§14).
10. **Guest network setup is outside the repo.** Guest IP 10.42.0.2 and routing come from forkd, not from `forkd-init.sh` (§10.5).
11. **Agent env semantics.** The runner comment says the agent replaces env; the agent code merges (§10.6).
