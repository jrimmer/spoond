# U03 — E2B fork, patches P1–P5, and builds

## Purpose

Create our patch-queue fork of `e2b-dev/runtime`, apply five small patches,
and build the orchestrator and envd binaries that U04 installs.

## Preconditions

- OPERATOR has created an empty repository `lacy.casa/e2b-runtime` on
  `code.lacy.casa`, and given push access to the implementer's credentials.
- vm2 has Go 1.27.1 at `/usr/local/go` (U01 step 10). That step is this
  unit's only dependency on U01. All builds in this unit
  run **on vm2** (x86_64). The orchestrator needs cgo, so it cannot be
  cross-compiled from arm64.

## Facts relied on

- **Base commit:** `e473dd130015ca1ff8cf9300341e25034c38e178` (2026-09-30).
  - Upstream URL: `https://github.com/e2b-dev/runtime.git` (renamed from
    `e2b-dev/infra`).
  - Go module paths remain `github.com/e2b-dev/infra/packages/...` (A2 §1).
- **Orchestrator:**
  - `packages/orchestrator`, `go 1.26.8`, cgo required (A3 A3.2, A3.3).
  - Build: `make -C packages/orchestrator build-local`, output
    `packages/orchestrator/bin/orchestrator` (A3 A3.1).
- **envd:** `packages/envd`, static build: `make -C packages/envd build`,
  output `packages/envd/bin/envd`.
- **Proto generation:**
  - Commands come from `packages/orchestrator/generate.go` (A2 §1).
  - Tool versions: `protoc` 34.1, `protoc-gen-go` v1.36.11,
    `protoc-gen-go-grpc` v1.6.1.
  - Output: `packages/shared/pkg/grpc/orchestrator/`,
    `.../orchestrator-info/`, `.../template-manager/`.
- **Patch sites, exact code at the base commit:**
  - P1: A3 P1, including "Minimal change points (P1-FC)".
  - P2: A3 P2.
  - P3: A3 P3.
  - P4: A3 P4.1–P4.4.
  - P5: A3 P5.1–P5.3.

If any quoted function, file or line region is not found at the base commit
as A3 shows it, STOP (README rule 3).

## Steps

### 1. Mirror and branches (on vm2)

Every shell in this unit starts with
`export PATH=/usr/local/go/bin:/usr/local/bin:$PATH`, because `make` and
`go test` call `go` from `PATH`.

```bash
export PATH=/usr/local/go/bin:/usr/local/bin:$PATH
apt-get install -y --no-install-recommends gcc libc6-dev linux-libc-dev make git unzip
mkdir -p /root/src && cd /root/src
git clone https://github.com/e2b-dev/runtime.git e2b-runtime
cd e2b-runtime
git checkout -b upstream e473dd130015ca1ff8cf9300341e25034c38e178
git remote rename origin github
git remote add origin https://code.lacy.casa/lacy.casa/e2b-runtime.git
git push origin upstream
git checkout -b spoond upstream
```

### 2. Proto toolchain (on vm2)

```bash
cd /tmp
curl -fsSLO https://github.com/protocolbuffers/protobuf/releases/download/v34.1/protoc-34.1-linux-x86_64.zip
unzip -o protoc-34.1-linux-x86_64.zip -d /usr/local/protoc-34.1
ln -sf /usr/local/protoc-34.1/bin/protoc /usr/local/bin/protoc
GOBIN=/usr/local/bin /usr/local/go/bin/go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
GOBIN=/usr/local/bin /usr/local/go/bin/go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.1
```

Define the regeneration command, used after P3 and P4. Run it from
`packages/orchestrator`. It is `generate.go` without `mise exec` and without
mockery:

```bash
cd /root/src/e2b-runtime/packages/orchestrator
protoc --go_out=../shared/pkg/grpc/orchestrator/ --go_opt=paths=source_relative --go-grpc_out=../shared/pkg/grpc/orchestrator/ --go-grpc_opt=paths=source_relative orchestrator.proto
protoc --go_out=../shared/pkg/grpc/template-manager/ --go_opt=paths=source_relative --go_opt=Morchestrator.proto=github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator --go-grpc_out=../shared/pkg/grpc/template-manager/ --go-grpc_opt=paths=source_relative template-manager.proto
```

**Sanity check before any patch:** run the regeneration command on the
unmodified `spoond` branch. `git status` must show **no changes**. If it
shows changes, the toolchain differs from upstream's, so STOP.

### 3. Patch P1: scope startup reclaim to our Firecracker processes

Why: startup reclaim SIGKILLs **every** process named `firecracker` on the
host (A3 P1), which would kill forkd's VMs on vm2.

Changes, all in `packages/orchestrator/pkg/startupreclaim/`:
1. `reclaim.go`: add the field `FirecrackerVersionsDir string` to `Config`.
2. `packages/orchestrator/pkg/factories/run.go`, in the `startupreclaim.Run`
   call (A3 P1, around L789–L794): set
   `FirecrackerVersionsDir: config.FirecrackerVersionsDir`.
3. `reclaim.go`: pass `config.FirecrackerVersionsDir` into
   `reclaimFirecrackers(ctx, config.ProcDir, config.FirecrackerVersionsDir)`.
4. `firecracker.go`:
   - change `discoverFirecrackerPIDs(procDir string)` to
     `discoverFirecrackerPIDs(procDir, versionsDir string)`;
   - change `isFirecrackerCmdline(cmdline []string)` to
     `isFirecrackerCmdline(cmdline []string, versionsDir string) bool`,
     which returns true only when **both** hold:
     - the existing basename check passes;
     - `versionsDir != ""`, and
       `strings.HasPrefix(filepath.Clean(cmdline[0]), filepath.Clean(versionsDir)+"/")`.
5. `firecracker_test.go`: update the calls to the new signatures, and add
   table cases:
   - `/fc-versions/v1.14-0.2.0/amd64/firecracker` with `versionsDir=/fc-versions` → true;
   - `/usr/local/bin/firecracker` with `versionsDir=/fc-versions` → false;
   - `firecracker` (relative) → false;
   - any path with `versionsDir=""` → false.

Do **not** change NBD, netns, cgroup or file reclaim. On vm2 nothing else
uses NBD, `ns-<int>` names, `/sys/fs/cgroup/e2b` or the orchestrator's
`TMPDIR`.

Run `cd packages/orchestrator && go test ./pkg/startupreclaim/...`, which
must pass.

**Commit:** `fix(orchestrator): scope startup reclaim to our Firecracker binaries`

### 4. Patch P2: configurable shutdown grace and bounded drain

1. `packages/orchestrator/pkg/cfg/model.go`, `Config` struct: add
   ```go
   ShutdownAdmissionGrace time.Duration `env:"SHUTDOWN_ADMISSION_GRACE" envDefault:"15s"`
   SandboxDrainTimeout    time.Duration `env:"SANDBOX_DRAIN_TIMEOUT"    envDefault:"0s"`
   ```
2. `packages/orchestrator/pkg/factories/run.go`, the block quoted in A3 P2:
   - Replace
     ```go
     if !env.IsLocal() {
         time.Sleep(15 * time.Second)
     }
     ```
     with
     ```go
     if !env.IsLocal() && config.ShutdownAdmissionGrace > 0 {
         time.Sleep(config.ShutdownAdmissionGrace)
     }
     ```
   - Where `DrainSandboxes` is called with `closeCtx`: when
     `config.SandboxDrainTimeout > 0`, call it with
     `drainCtx, drainCancel := context.WithTimeout(closeCtx, config.SandboxDrainTimeout)`
     and `defer drainCancel()`. Otherwise keep `closeCtx`.
   - If the drain returns an error with
     `errors.Is(err, context.DeadlineExceeded)`, log it at Warn and continue
     shutdown. Do not fail. Other errors keep their existing handling.
3. `packages/orchestrator/pkg/template/server/main.go`
   (`consumerStatusCheckGracePeriod`, A3 P2):
   - replace the constant `consumerStatusCheckGracePeriod` with a package
     variable read once at package init:
     `TEMPLATE_MANAGER_SHUTDOWN_GRACE` parsed with `time.ParseDuration`;
     unset or unparsable means `15 * time.Second`;
   - use it where the constant was used (`time.After(...)`, A3 P2), keeping
     the existing `ENVIRONMENT=local` guard;
   - `0` means no wait (skip the `time.After` wait).
4. Add both new variables to the U04 env file:
   ```ini
   SHUTDOWN_ADMISSION_GRACE=0s
   SANDBOX_DRAIN_TIMEOUT=60s
   TEMPLATE_MANAGER_SHUTDOWN_GRACE=0s
   ```

Run `cd packages/orchestrator && go build ./... && go vet ./pkg/factories/... ./pkg/cfg/...`.

**Commit:** `feat(orchestrator): configurable shutdown grace and bounded sandbox drain`

### 5. Patch P3: report each sandbox's host IP

1. `packages/orchestrator/orchestrator.proto`:
   - in `message RunningSandbox`, add `string host_ip = 10;`;
   - in `message SandboxCreateResponse`, add `string host_ip = 5;`.
2. Run the regeneration command (step 2).
3. `packages/orchestrator/pkg/server/sandboxes.go`:
   - where `RunningSandbox` is built for `List` (A3 P3, around L675–L685),
     set `HostIp` to `sbx.Slot.HostIPString()` when `sbx.Slot != nil`,
     otherwise `""`;
   - where `SandboxCreateResponse` is built (around L443–L452), set
     `HostIp` the same way.

Run `cd packages/orchestrator && go build ./...`.

**Commit:** `feat(orchestrator): report sandbox host IP in Create and List`

### 6. Patch P4: per-sandbox private allowances with TCP port scoping, and a host-address guard

1. **Proto** (`packages/orchestrator/orchestrator.proto`):
   - add the message
     ```proto
     message SandboxPrivateAllowance {
       // IPv4 CIDR inside the always-denied private ranges, e.g. "10.1.0.11/32" or "10.0.0.0/13".
       string cidr = 1;
       // TCP destination ports allowed to this CIDR. Empty = every TCP port.
       repeated uint32 tcp_ports = 2;
     }
     ```
   - in `message SandboxNetworkEgressConfig`, add
     `repeated SandboxPrivateAllowance allowed_private = 8;`;
   - regenerate (step 2).
2. **Layer 1, per-netns nftables** (`packages/orchestrator/pkg/sandbox/network/firewall.go`,
   `slot.go`; A3 P4.2 and P4.4). Apply exactly the P4.4 "Layer 1"
   instructions, with `allowedPrivate []string` = the `cidr` of every
   allowance:
   - `Firewall.bufferUserRules`: build the always-allow set from
     `fw.allowedRanges` plus `allowedPrivate`, deduplicated.
   - Thread `allowedPrivate` through `ApplyRules`, `Slot.ConfigureInternet`,
     `Slot.UpdateInternet` and `Slot.ResetInternet` (pass `nil`), and the
     `NewFirewall` initial apply (`nil`).
   - In `Slot.ConfigureInternet`, add `len(allowedPrivate) > 0` to the
     `hasUserRules` condition.
   - Read the list from `sbx.Config.GetNetworkEgress().GetAllowedPrivate()`
     wherever the egress config is read today.
3. **Layer 2, TCP firewall** (`packages/orchestrator/pkg/tcpfirewall/handlers.go`,
   A3 P4.3 and P4.4). Layer 2 has **no** private-range floor for
   IP-addressed TCP (A3 P4.4); layer 1's nftables floor enforces it. Keep it
   that way.
   - Add a pure function (no sandbox lookup, no I/O) with exactly this
     signature, holding **all** of the decision logic:
     ```go
     // egressDecision decides one TCP connection. egress may be nil.
     func egressDecision(egress *orchestrator.SandboxNetworkEgressConfig, hostname string, ip net.IP, port int) (bool, MatchType, error)
     ```
     Its body, in this order:
     1. **Host-address guard** (item 4 below).
     2. If `egress == nil`: return `true, MatchTypeNone, nil` (upstream
        behaviour).
     3. The existing allowed-domain loop, unchanged.
     4. The existing allowed-CIDR loop, unchanged.
     5. **Private allowances** (new):
        ```go
        matched := false
        for _, a := range egress.GetAllowedPrivate() {
            _, n, err := net.ParseCIDR(a.GetCidr())
            if err != nil {
                return false, MatchTypeNone, fmt.Errorf("invalid allowed private CIDR %q: %w", a.GetCidr(), err)
            }
            if n.Contains(ip) {
                matched = true
                if len(a.GetTcpPorts()) == 0 || slices.Contains(a.GetTcpPorts(), uint32(port)) {
                    return true, MatchTypeCIDR, nil
                }
            }
        }
        if matched {
            // Inside an allowed private CIDR, but not on an allowed port.
            return false, MatchTypeCIDR, nil
        }
        ```
     6. The existing denied-CIDR loop, unchanged.
     7. The existing default: `return true, MatchTypeNone, nil`.
   - Change `isEgressAllowed` to
     `func isEgressAllowed(sbx *sandbox.Sandbox, hostname string, ip net.IP, port int) (bool, MatchType, error)`
     whose whole body is
     `return egressDecision(sbx.Config.GetNetworkEgress(), hostname, ip, port)`.
   - Pass `int(c.dstPort)` from `domainHandler` and `cidrOnlyHandler`
     (convert if `dstPort` is not already an `int`).
4. **Host-address guard (layer 2)**. New in `handlers.go`:
   - `var hostAddrs = computeHostAddrs()` at package level, of type
     `[]*net.IPNet`. `computeHostAddrs` returns every IPv4 address from
     `net.InterfaceAddrs()` as a `/32`, plus `127.0.0.0/8`. If
     `net.InterfaceAddrs()` fails, it returns only `127.0.0.0/8`.
   - At the **start** of `egressDecision`: if the destination IP is inside
     any `hostAddrs` entry, return `true, MatchTypeCIDR, nil` **only** if
     some allowance in `egress.GetAllowedPrivate()` (nil-safe) has a `cidr`
     containing the IP **and** a non-empty `tcp_ports` containing `port`.
     Otherwise return `false, MatchTypeCIDR, nil`.
   - This stops sandboxes reaching host services such as the orchestrator
     gRPC, forkd or spoond, except ports spoond explicitly grants.
5. **Tests.** Add `packages/orchestrator/pkg/tcpfirewall/handlers_allowance_test.go`
   with table tests of `egressDecision` (hostname `noHostnameValue`).
   Unless a row says otherwise, `hostAddrs` is replaced by
   `{127.0.0.0/8}` for the test and restored with `t.Cleanup`:
   - allowance `10.0.0.0/13` with no ports: `10.1.0.5:22` → allowed;
   - allowance `10.11.0.7/32` ports `[9042]`: `10.11.0.7:9042` → allowed,
     `10.11.0.7:22` → **denied** (matched CIDR, wrong port);
   - no allowances (egress with no fields set): `10.1.0.5:22` → **allowed** at
     layer 2 (layer 1's nftables floor enforces the private floor);
   - with `hostAddrs` set to `{10.1.0.11/32, 127.0.0.0/8}`:
     - allowance `10.0.0.0/13` with no ports: `10.1.0.11:8891` → **denied**;
     - allowance `10.1.0.11/32` ports `[8891]`: `10.1.0.11:8891` → allowed,
       `10.1.0.11:5008` → denied;
     - nil egress: `10.1.0.11:22` → denied;
   - `denied_cidrs=["0.0.0.0/0"]` plus allowance `10.11.0.7/32` `[9042]`:
     `10.11.0.7:9042` → allowed, `10.11.0.7:22` → denied.

Run `cd packages/orchestrator && go test ./pkg/tcpfirewall/... && go build ./...`.

**Commit:** `feat(network): per-sandbox private allowances with TCP port scoping and host-address guard`

### 7. Patch P5: flag overrides from a file

1. `packages/shared/pkg/featureflags/flags.go`: add
   ```go
   // OverrideFlagValue sets a flag's value in the offline store. Only effective
   // when LAUNCH_DARKLY_API_KEY is empty.
   func OverrideFlagValue(name string, v ldvalue.Value) {
       launchDarklyOfflineStore.Update(launchDarklyOfflineStore.Flag(name).ValueForAll(v))
   }
   ```
2. `packages/orchestrator/main.go`, in `applyTestFlagOverrides()` (which runs
   before `factories.Run`, A3 P5.3), append:
   - If `os.Getenv("E2B_FLAG_OVERRIDES_FILE")` is non-empty, read that file
     and JSON-decode it into `map[string]json.RawMessage`.
   - For each key, convert with `ldvalue.Parse(raw)` and call
     `featureflags.OverrideFlagValue(key, value)`.
   - Log each key and value at Info.
   - If the file is missing or invalid, **exit the process** with a clear
     error. Do not start with unknown flags.
3. Add `packages/shared/pkg/featureflags/override_test.go`. With
   `LAUNCH_DARKLY_API_KEY` unset, build the offline client the same way the
   package's existing tests do (the static client over
   `launchDarklyOfflineStore`), then:
   - override `max-sandboxes-per-node` to `ldvalue.Int(100)`, and assert
     `IntFlag(ctx, MaxSandboxesPerNode)` returns `100`;
   - override `in-place-checkpoint` to `ldvalue.Bool(false)`, and assert
     `BoolFlag(ctx, InPlaceCheckpointFlag)` returns false.
   If the package has no existing way to build the offline client in a test,
   STOP.

Run `cd packages/shared && go test ./pkg/featureflags/...`.

**Commit:** `feat(orchestrator): apply feature flag overrides from E2B_FLAG_OVERRIDES_FILE`

### 8. PATCHES.md

Create `PATCHES.md` at the repo root of the `spoond` branch:
- one section per patch (P1–P5): title, commit subject, why, files touched,
  and upstream status;
- upstream status is `not yet proposed` for all five at this point.

Also include the base commit hash and the proto tool versions.

**Commit:** `docs: patch series for spoond`

### 9. Build and publish binaries (on vm2)

```bash
export PATH=/usr/local/go/bin:/usr/local/bin:$PATH
cd /root/src/e2b-runtime
make -C packages/orchestrator build-local BUILD_ARCH=amd64
make -C packages/envd build BUILD_ARCH=amd64
file packages/orchestrator/bin/orchestrator packages/envd/bin/envd
sha256sum packages/orchestrator/bin/orchestrator packages/envd/bin/envd | tee /root/src/e2b-runtime-binaries.sha256
git push origin spoond
```

The envd target and output path (`make -C packages/envd build` →
`packages/envd/bin/envd`) are not quoted in the appendices. If the target
does not exist or the file is not produced there, STOP.

`envd` must report `statically linked`; `orchestrator` is dynamically linked
against glibc (that is expected). Record `git rev-parse spoond` and both
SHA-256 values in `PATCHES.md` under "Current build". Commit
(`docs: record current build`) and push.

## Done when

- `origin/upstream` equals `e473dd13…`.
- `origin/spoond` = upstream + 7 commits (P1–P5, PATCHES.md, record
  build).
- The unit tests named above pass.
- Both binaries exist on vm2 at `/root/src/e2b-runtime/packages/*/bin/`.

## Do not

- Do not modify anything outside the listed files.
- Do not run the orchestrator in this unit. That is U04.
- Do not regenerate with different tool versions.
