# Multiple microVM backends?

Assessment of making spoond's microVM substrate pluggable, and whether
Hyper (`harmont-dev/hyper`, Elixir) can be a second backend. Written after
the forkd shared-rootfs work (upstream #317, spoond #66) prompted the
question.

**Re-surveyed 2026-09-12.** The question widened from "should Hyper be a
second backend" to "is forkd the right substrate for general-purpose
microVM infrastructure at all". That wider sweep is the final section of
this file; the Hyper assessment below stands as written.

## The seam already exists

`api/service.go` defines the whole substrate surface consumers see:

```
type ForkdClient interface {
    ListSnapshots, SnapshotExists, Spawn(tag, n, perChildNetns, memoryLimitMiB),
    ListSandboxes, Kill, Exec(id, args, timeoutSecs), Ping, Branch(id, tag),
    CreateWorkspace, SuspendWorkspace, ResumeWorkspace, DeleteWorkspace, Metrics
}
```

Everything above it is substrate-agnostic already — the runner, MCP, ACP,
the CFOS adapter and the command adapter talk to the lease API through
ports; the SSH gateway, HTTP proxy, LLM gateway, identity store, quotas,
warm pool and TTL sweeping are all spoond-side. So a second backend is
*possible* without touching consumers, which is the good news.

Two caveats from reading it closely:

1. **The interface is forkd-shaped.** It leaks `tag`, `perChildNetns`,
   `memoryLimitMiB`, workspace verbs and a `/metrics` passthrough. A
   second backend must either satisfy forkd's model or the interface has
   to be reshaped into a capability-based contract.
2. **The wire assumptions leak further up.** `Endpoint{Netns, GuestHost}`
   with a hard-coded agent port `8888` and sshd on `22`, and the network
   policy applier running `iptables` inside the child netns, all assume
   "a named netns we can `setns` into, with a TCP guest agent".

## What a backend must provide

Hard requirements (from the inventory; losing any of these loses product
capability):

1. Named, enumerable, existence-checkable snapshots (drives `/api/images`, `KNOWN_IMAGES`).
2. Spawn-by-snapshot returning an id + guest address, with per-child network isolation.
3. A per-child netns with a stable name spoond can enter and run `iptables` in — this is what network policy (`none|lan|internet|restricted`) and all host→guest reachability rest on.
4. Exec with argv + timeout returning stdout/stderr/exit, plus a cheap reachability probe.
5. **Streaming PTY exec** — backs the WebSocket stream endpoint, interactive shells, MCP/ACP. No fallback exists.
6. **Branch of a *running* sandbox including RAM state** — backs `clone`/`cp` and the dev-base bake idiom.
7. **Named workspace records with suspend/resume/delete** — backs persistent leases, keepalive, restart, idle auto-suspend, and the whole interactive dev product.
8. A guest agent spoond can install and reach (or an equivalent transport spoond is rewritten to use).
9. Durable idempotent kill + live-sandbox enumeration (orphan reclaim, pool validation).
10. Container-image → bootable snapshot, with per-image rootfs and memory sizing, re-bakeable in place.
11. Restart semantics that preserve snapshot/workspace state while pruning live VMs.

## Hyper's scorecard

Verified by reading its source and docs, and by diffing spoond's old
`hyper/` client against Hyper today.

**What Hyper wins, and genuinely:**

- **Per-VM writable disks are structural.** Every VM and every fork gets
  its own `dm-thin` volume over a read-only composed external origin
  (`thin_pool.ex` `create_external/3`), and `test/e2e/fork_test.exs`
  asserts bidirectional COW isolation. The corruption class driving all of
  this work is absent by design, and there is no drive to re-point because
  Hyper builds the device before boot. FC pinned at v1.16.
- **Container images are the native input**: `LoadImage` → skopeo → umoci
  → `mke2fs` → content-addressed layer. That would replace our bespoke
  `build-rootfs.sh` / `from-image` / sidecar / `pack` pipeline outright.
- **Native cwd/env on exec**, where spoond currently shell-wraps because
  forkd's exec takes argv only.
- **Per-VM networking is mandatory and isolated**: one netns named exactly
  the `vm_id`, veth, TAP, NAT — spoond's netns policy could be ported.
- MIT licensed, so patching is allowed.

**What blocks it as a peer backend:**

| Requirement | Hyper |
|---|---|
| RAM snapshots / branch of a running sandbox (6) | **Refused by design** — README: "does not support RAM snapshotting and will not in the foreseeable future" (issue #72 open, author cites an NDA with a competitor). Its branch is disk-only and crash-consistent. |
| Workspaces + suspend/resume (7) | **Absent** — issue #73 open. Worse: `StopVm` destroys the writable volume and `ThinPool.init/1` *zeroes the pool metadata on every node boot*, so no VM's writable state survives a host restart. |
| Streaming exec (5) | **Absent** — the guest agent's `Exec` is unary/buffered, no PTY, no stdin relay, no signals; timeout is a client-side deadline that does not kill the guest process. And it is not exposed on the public gRPC at all (internal vsock relay, protos say "the contract may change between releases"). |
| Named/enumerable snapshots (1) | **Different model** — content-addressed `img_id`s, no listing RPC, and deriving a re-bootable image from a running VM (`publish_fork_image`) is BEAM-only. |
| Metrics (part of the seam) | **Absent** — OpenTelemetry only, no Prometheus. |
| Ingress + network policy | **Absent** — issue #74 open ("Hyper does not support network ingress"). |
| Guest agent (8) | **Conflicts.** Hyper *is* PID 1 (`init=/hyper-init`), so spoond's Python agent cannot be the init, and Hyper replaces the image's own init — systemd/openrc images are untested territory. |
| Ops footprint | **Heavier**: PostgreSQL, a shared layer filesystem (NFS), Elixir 1.20/OTP 28 + Rust, `dm-thin`/`thin_dump`/`lvm2`/`nftables`/`skopeo`, a setuid-root Rust helper, gRPC off by default and unauthenticated. |
| Project risk | 112 commits, effectively one author, ~0 to `main` since 2026-08-02, one release (2026-07-09). A documented breaking gRPC rename (`v0`→`v1`) already invalidated spoond's client while it was being written. |

**The decisive point:** Hyper can never serve the interactive dev product,
because resuming a lean sandbox with its tmux session intact requires RAM
state, and that is explicitly off Hyper's roadmap. So it cannot be a peer
backend — at best a second tier that runs stateless-ish workloads with
isolated writable disks.

## Verdict

**Make the seam honest; do not write a Hyper backend now.**

Multiple backends is the right *posture* — it is what keeps us from
depending on one young project (both forkd and Hyper qualify) — but the
value comes from a capability-based contract, not from adding a substrate
that serves a strict subset of the product. And the one capability Hyper
clearly wins is the one forkd is about to get anyway: a Firecracker
version bump gives per-child drive paths, which is all the corruption fix
needs.

### Do now (cheap, durable)

1. **Reshape the seam into a substrate contract.** Rename `ForkdClient` to
   `Substrate` (or equivalent), express the eleven requirements above as
   the documented contract, and advertise what a backend supports so
   consumers can degrade deliberately instead of discovering gaps at
   runtime. That endpoint is `/v1/capabilities` — but it lives on the
   unmerged `feat/capabilities-endpoint` branch, not on `main`, and it
   describes spoond's product surfaces rather than the backend's
   capabilities.
2. **Land the forkd fixes**: bump the pinned Firecracker (drive overrides /
   `PATCH /drives` rebinding, FC ≥1.15) and adopt the placeholder-drive +
   CoW pattern — item 2 of #66.
3. **Keep this assessment in the repo** so the Hyper question is answered
   once rather than re-litigated.

### Revisit a second backend on a trigger, not on vibes

- forkd activity stalls (maintainer stops) — the resilience argument becomes real.
- We need per-VM writable disks at scale *before* the FC bump lands.
- Hyper lands suspend/resume (#73), streaming exec, ingress (#74) and a metrics surface — at which point it becomes a candidate peer rather than a subset.
- We want the OCI-native image pipeline badly enough to port it as a *design* onto forkd, which is a smaller move than adopting the substrate.

### If we ever do add a second backend

It will need: a guest-agent strategy (patch Hyper's MIT-licensed PID-1
agent, or run spoond's agent as an orphaned child via a shell wrapper, since
unary exec cannot supervise a daemon), a translation layer for
tags→`img_id` and for spoond's netns assumptions, and an explicit
statement of which product capabilities that tier does **not** serve —
notably interactive dev sandboxes and `clone`/`cp`.

---

# Re-survey, 2026-09-12: is forkd right for general-purpose microVMs?

## How this was done

Question asked: forkd is "not quite general-purpose enough to do everything
from run small functions to doing repository builds, hosting LLM agents,
etc." — is the investment in adapting it still right? The sweep therefore
covered every tier, not just a second backend: sibling orchestrators, full
platforms with an open control plane, the VMM layer, and the function tier.

Method, because the last substrate assessment here published a wrong fact
(see the `drive_overrides` correction above): every claim below was checked
against the project's repo, release tag or raw source. A search snippet is
not evidence, a merged PR is not a released capability, and a project's own
roadmap is not a feature. Two claims that the sweep returned were wrong and
are corrected in place below.

## The headline

**The primitive this platform's value rests on is no longer rare.**

One Firecracker microVM per sandbox, resumed from a memory snapshot, with a
PTY-capable agent inside it, a per-sandbox egress firewall, and a fork of a
*running* sandbox — that set is now shipped, open source, by at least three
projects. One of them, **E2B Runtime**, publishes the *entire* control plane
(the API, the per-node orchestrator, the in-guest agent, the edge router and
the template builder) under Apache-2.0, and states that the same code serves
its cloud, its enterprise deployments, and a single-machine package you run
on your own hardware.

So "nobody else can do this" can no longer be the justification for staying
on forkd. What still justifies it is narrower, and is stated in the
recommendation below: the cost of the alternatives *for this consumer set*,
and the fact that our divergence from forkd is small and upstream-shaped.

## Correction 1: the hard parts are upstream, and our fork is small

"Twisting forkd to support us" is real but bounded. Reading the fork rather
than assuming:

| Capability | Where it actually lives |
|---|---|
| Workspaces + suspend/resume with RAM | **Upstream**, merged 2026-05-20 (`bb9fd31`, PR #122), shipped in release v0.5.3 |
| Branch of a *running* VM including RAM state | **Upstream** (v0.4/v0.5) |
| Snapshot chains / diff snapshots | **Upstream** (v0.5) |
| Per-child rootfs backing — the corruption fix | **Ours**: 8 commits, open as draft #321 |
| Bake hygiene, orphan-Firecracker, bake space | **Ours**: #314/#315/#316, all deployed |
| Guest agent and exec/stream protocol | Upstream contract; we ship and extend the agent in `deploy/rootfs-init/` |

We are also already a merge-author upstream — `#295` and `#299` merged
2026-09-02. The maintenance exposure is therefore "a 9-contributor project
we send patches to", not "a fork we must carry". That is a materially
different risk posture from the one this file assumed last time, and it is
the single most useful thing the re-survey found.

**Correction 2, so that it is not re-derived:** upstream `docs/API.md` on
`dev` does not document the workspace routes, which makes forkd look as if
it has no persistence model at all. The routes exist in the code and in the
v0.5.3 tag (`crates/forkd-controller/src/http.rs`). A doc's silence is not
an absence — though a route's presence is not a released capability either,
which is why this one was checked in the tag.

## The structural boundary: nothing serves "everything"

No substrate here serves both a 50 ms function call and a multi-hour
suspendable developer sandbox, and the reason is structural rather than a
matter of tuning:

- **Sub-millisecond work needs no kernel.** That is the Wasm tier
  (Wasmtime, Spin, wasmCloud); it has no kernel, no persistent userland and
  no suspend/resume primitive, so it cannot host an interactive shell, a
  package install, or a tmux session.
- **Suspend/resume with a live session needs full RAM and device state.**
  That means a microVM (or gVisor's userspace checkpoint), and it is why
  every FaaS-shaped option that lacks it — Kata, flintlock,
  firecracker-containerd, agent-sandbox, Hyper — fails the interactive
  developer product rather than merely being slower.

A platform that claims to span the range therefore spans it with **two
tiers**, or picks a point on it. forkd picks the microVM point, which
already covers everything that needs a real userland: forkd's own harness
targets ~100 ms for 100 children from a warm pool, so "small functions" of
the kind that need a container are served by the same tier as CI and
agents. A Wasm tier is an *addition* for genuinely sub-millisecond work,
not a replacement for anything.

## Candidates

### Full platforms you would adopt rather than extend

| | E2B Runtime | CubeSandbox | AgentENV |
|---|---|---|---|
| Repo / license | `e2b-dev/runtime`, Apache-2.0 | `TencentCloud/CubeSandbox`, Apache-2.0 (Tencent modifications notice) | `kvcache-ai/AgentENV`, MIT |
| Contributors / age | 65 | 103; first release 2026-04 | 31; created 2026-07-23 |
| Isolation | Firecracker | RustVMM + KVM | Firecracker |
| RAM snapshot + live fork | Pause diffs memory and disk to object storage; fork a running sandbox, up to 100 children per request | CubeCoW checkpoints on running sandboxes, clone/rollback to any point; cross-node pause/resume over S3 (v0.7) | Incremental memory+filesystem snapshot, fork a running environment; self-reported <100 ms pause, <50 ms resume |
| In-guest API | `envd`: processes, PTYs, filesystem, watchers, port forwarding | E2B-SDK compatible — swap one env var | E2B-compatible HTTP API |
| Self-host reality | Whole stack Apache-2.0, but the single-machine package is "an evaluation package, not a production deployment pattern"; the supported production path is their Enterprise deployment inside your account | Single-node and multi-node, Terraform, ARM64; Kubernetes deploy is preview | Single node and Kubernetes |
| Ops footprint | Postgres, Redis, ClickHouse, **Nomad**, object storage | MySQL/Postgres + Redis (both mandatory, control-plane state) + CubeMaster/CubeAPI/CubeProxy/Cubelet as processes, the **host's** containerd registered as a shim, and its own guest kernel + guest image. WebUI, CoreDNS and MinIO are optional, and the S3 backend is opt-in | Firecracker + S3 |
| Gap against us | Sovereignty: the supported path is their control plane. No CI runner, SSH-as-API, identity or quota model | Our workspace/SSH control plane would be rebuilt on an E2B-shaped API; pre-1.0 with monthly breaking releases; vendor-authored design | Seven weeks old; no TLS by default; no external security review |

### CubeSandbox as a *forkd replacement*, judged on the substrate contract

The rows above judge CubeSandbox as a platform, which overstates what we
need from it: spoond wraps the substrate, so the guest agent, sshd, tmux,
the four policy modes, the LLM gateway and the CI runner are spoond's work,
not the substrate's. The contract a forkd replacement must satisfy is much
smaller — registry, create-from-snapshot, host→guest reach, exec and
streaming PTY, RAM-state snapshot/branch/pause/resume, per-sandbox egress
enforcement, and OCI images with enumerate/idempotent-kill.

Against that contract, verified in the repo on 2026-09-12:

| Substrate need | CubeSandbox today |
|---|---|
| Named, deletable snapshot registry | **Covered.** Templates and snapshots are one object class: `GET /snapshots`, existence via `GET /templates/{id}` → 404, `DELETE /templates/{id}`. Naming is `metadata`/`alias`, not a first-class name — thin adapter. |
| Create-from-snapshot → an id we own | **Covered.** `POST /sandboxes` → `sandboxID`, plus `pause`, `resume`, `refreshes` (keepalive), `timeout`/TTL and in-place `rollback`. |
| **Host → guest reachability** | **The one real gap.** No guest IP in any client-facing schema (`Sandbox`, `SandboxDetail`), and **no port-mapping endpoint in `openapi.yml`**. Inbound is CubeProxy on `<port>-<id>.<domain>`, HTTP/gRPC only (`CubeProxy/nginx.conf` has no `stream` block); the data plane is `envd` on 49983 behind that proxy. Host ports are auto-allocated from 20000–29999 (`docs/architecture/network.md` §7.4) and Cubelet exposes `GetPortMapping()`/`AddPortMapping()` — node-local internals, not client API. |
| Exec + streaming PTY | **Covered via `envd` ConnectRPC**, proxied with buffering disabled; stdin and resize present, only `SIGKILL` surfaced by their SDKs. Our line-JSON agent on 8888 does not survive as-is. |
| RAM-state branch / pause / resume | **Covered, and best-in-class.** Snapshots preserve "CPU registers, process memory, TCP state (with no external peer), and filesystem mutations"; clone composes create-snapshot + N creates; cross-node resume exists via an S3 backend (**opt-in, preview**). |
| Per-sandbox egress enforcement | **Covered, and richer than ours.** `allow_internet_access`, `allow_out` (IPv4/CIDR/DNS/wildcards), `deny_out`, and L7 `rules` matched on SNI/Host; precedence allow > deny > default-allow. Watch the built-in denies (`10/8`, `127/8`, `169.254/16`, `172.16/12`, `192.168/16`) — our `restricted` mode points the guest at the host bridge for the LLM gateway, so that needs an explicit `allow_out`. Caps 8192/8192/1024. |
| OCI image → bootable, re-bakeable | **Covered, and better than ours.** `POST /templates` from an image with per-template cpu/memory/`writableLayerSize`/`exposedPorts`/env/dns/command/probe, rebuilt in place, readiness gated on an HTTP 2xx probe rather than a sleep. |

Two lifecycle sharp edges before trusting it for reclamation: `DELETE` on a
*paused* sandbox resumes it first and can fail `409 node capacity
unavailable`, and a named adopter documents that a single
`GET /sandboxes` "may omit paused sandboxes" — the same trust spoond
currently places in `ListSandboxes`. That write-up also reports 704 stuck
sandboxes and 1,359 failed kills, with leases + fencing tokens +
mark-and-sweep built on top.

**Verdict:** six of seven covered, one of them (egress policy) better than
forkd's, and the seventh — host→guest reachability — is a *transport
rewrite* rather than a missing feature, with a plausible self-hosted escape
hatch: we own the node, and Cubelet already exposes the port-mapping calls,
so raw TCP to a baked-in guest service may be recoverable without their
client API. That is a far better fit than the platform-level table above
suggests, and it is the thing a spike should settle.

### Peers of forkd — the same primitive, other projects
| Project | Verdict |
|---|---|
| **Mitos** | **Corrected 2026-09-12 — not a layer over forkd, a naming collision.** Verified: `go.mod` carries no forkd dependency, the DaemonSet ships `ghcr.io/mitos-run/mitos-forkd` built from its own `cmd/forkd/`, and it vendors its *own* patched Firecracker (UFFD-WP, memfd CoW). So it is an independent full stack and a competitor at the primitive layer, not a consumer of ours. Kept out of the decision tables on health: 89 stars, 5 contributors (two active humans), no commits since 2026-07-18, and its own ADR 0005 is titled "raw-forkd not multitenant". |
| **Tarit** | Broadest single-project coverage on paper — its own rust-vmm VMM, multi-node orchestrator, SSH/PTY gateway, live snapshots. Disqualified by **AGPL-3.0**, **one contributor**, and v0.1.x. |
| **microsandbox** | libkrun-based, laptop-first, beta with announced breaking changes. Its snapshot model is not the RAM-state resume the interactive product needs. Not a peer. |
| **firecracker-containerd / Flintlock** | Neither exposes a VM snapshot API (flintlock is create/delete/start/stop/pause; CNI still "coming soon"), and both want containerd beside them. They would cost more than they replace. Weave Ignite is archived (2023). |
| **Kata Containers** | Production-grade isolation with real K8s integration, and Firecracker is a supported hypervisor — but VM snapshot/restore is an open issue, so it loses persistence and `clone` outright. It deletes nothing we have. |
| **gVisor** | A userspace kernel with genuine checkpoint/restore, but our own harness measured it at 288.6 s per 100 spawns against forkd's 101 ms. Best understood as a hardening runtime *under* an orchestrator. |
| **agent-sandbox (k8s-sigs)** | Becoming the API standard for agent sandboxes; delegates isolation to gVisor or Kata and has no RAM snapshot. Relevant as an API shape to speak, not as a substrate. |

### The layer category: control planes over a fork primitive

**Scope note, so this is not misread:** this category is about replacing
*spoond's own layer*, not the substrate — spoond is a member of it. It is
recorded only because the first pass mis-filed Mitos here on the strength of
a mistaken "installs the forkd DaemonSet" reading. **Mitos is correctly a
peer of forkd** (see the table above), so for a *forkd replacement* question
the peer table is the one that matters; the conclusion below is about
whether spoond's layer could be adopted rather than grown.

The first pass had no category for *control planes that sit on a fork
primitive* — the category spoond itself occupies — so Mitos was the only
one in view, and it turned out not even to be one. Swept properly:

| Project | What it is | Verdict |
|---|---|---|
| **deepklarity/harness-kit** | The **only verified consumer of `deeplethe/forkd`**: shells out to `forkd` + `forkd-controller` on 8889 against a `vmlinux`, and adds a multi-agent orchestration layer (per-task microVM, workspace staging, MCP proof shim, pre-baked Chromium snapshot). 98 stars, "experimental… edges are rough", idle since 2026-07-15. | Not a control plane to adopt — but the one worked example of integrating forkd, and worth reading as a client reference. |
| **prodioslabs/cellar** | A control plane over the **microsandbox** Go SDK: Raft membership, least-loaded placement, mTLS gateway, unix-socket CLI. No Kubernetes. Its "we have no SDK of our own — point the vendor's SDK at our gateway" posture is the same shape as our seam. 42 stars, one human plus `cursoragent` commits. | A design peer for the *shape*, deleting nothing: no warm pool, TTL, snapshot, fork or egress policy. |
| **opensandbox-group/OpenSandbox** | Ex-Alibaba, Apache-2.0, 15k stars. A genuine control plane — pools, TTL, lifecycle hooks, ingress, egress policy, multi-tenancy, six SDKs — over Docker/K8s and gVisor/Kata/Kata+Firecracker via `RuntimeClass`. | Fails the one thing that matters: default pause/resume is **rootfs-only, no RAM**, so there is no resumable interactive session. |
| **fast-sandbox**, **agent-sandbox/agent-sandbox** | Small k8s runtime planes: warm "Fastlet" pods with pool reuse; REST+MCP over `agent-infra/sandbox`. Note the **name collision** with `kubernetes-sigs/agent-sandbox`, which is a different project. | No RAM snapshot; unproven. |
| **kubeswift-io/kubeswift** | A Kubernetes control plane for **Cloud Hypervisor** with the richest snapshot surface in this group: `SwiftSnapshot` (memory+disk, local or S3), `SwiftRestore`, guest pools, `cloneFromSnapshot`, live migration. | Dropped for the same two reasons as Tarit: **AGPL-3.0** and effectively single-author. |
| **smol-machines/smolvm** | *Substrate-class, and a new find.* 6,033 stars, Apache-2.0, 42 contributors, active: libkrun-based with its **own** live-fork primitive (`smolvm machine branch --from source --count 8 --parallel 8`) and a containerd shim. | The healthiest new primitive found — but libkrun is a local/portable model where "the guest and the VMM pertain to the same security context", so it is not a multi-tenant boundary for untrusted code. |

**The category's honest conclusion:** nothing here clears the bar to replace
spoond's control plane, because nothing pairs a RAM-state fork with the rest
of the list. Each is either a platform adopted wholesale — the same
operational trade refused for CubeSandbox and E2B — or a thin gateway that
would delete nothing. The one thing in it worth *speaking* rather than
adopting is the SIG's `agents.x-k8s.io` API: Mitos already proves the facade
pattern (an upstream API implemented over a foreign engine, with upstream e2e
vendored into CI), but that API does not model RAM fork, so the mapping is
lossy in exactly the dimension this product is built on.

### The VMM layer — no change recommended

Stay on Firecracker (verified latest release **v1.17.0**, 2026-09-10, which
is what the host runs). Two facts settled by checking the release rather
than the tracker:

- `PATCH /drives/{drive_id}` exists and is what our per-child backing uses.
  Its documented contract is for a cooperative post-boot guest, but the
  restore-time re-point is separately boot-verified on the host, which is
  the evidence that matters.
- `drive_overrides` on snapshot load exists in **no released version**;
  PR #5774 is still open. The correction recorded earlier in this file
  stands.

**Cloud Hypervisor** (v53, Linux Foundation) is the only mature VMM that
adds what Firecracker deliberately excludes — virtio-fs, live migration and
VFIO passthrough. If we ever need any of those, that is a swap *inside* the
substrate, not a platform migration, because it comes with no orchestrator.
**GPU means QEMU with VFIO on a normal machine, not a microVM** — a third,
heavier stack, and a deliberate decision rather than a default.

## What an adoption would delete, and what it would not

Decomposed per the discipline this repo has learned to apply, using E2B
Runtime as the strongest candidate:

**Genuinely removes something we have:** the rootfs/image pipeline
(`build-rootfs.sh`, `from-image`, the sidecar, `pack`/`pull`) against its
layered template builder; the per-child backing work (#321) against its CoW
overlay over a read-only template; workspace suspend/resume storage against
its memory+disk diff to object storage with auto-pause and wake-on-traffic;
the guest agent and exec/stream plumbing against `envd`; the netns-level
egress policy against a firewall that inspects SNI and Host; edge routing
for per-port sandbox URLs; and the `/metrics` passthrough against OTel with
ClickHouse.

**Already covered, or not replaced at all:** the Forgejo runner, per-PR
environments, the SSH-as-API control plane, per-user identity and quotas,
MCP/ACP, the LLM gateway and the *semantics* of network policy are all
spoond-side. A substrate swap does not touch them — that is the seam doing
its job, and it is the reason a migration here is a backend-internal change
rather than a rewrite.

**What the deletion costs:** our single Rust daemon plus a small patch set
becomes Postgres + Redis + ClickHouse + Nomad + object storage, and the
supported production shape is someone else's control plane inside our
account. That is the sovereignty and ops-footprint trade this platform
exists to avoid — which is a reason to keep the option, not to take it today.

## Recommendation

**Stay on forkd. Change the justification, and buy the cheap insurance.**

The old justification — only forkd has the state fidelity we need — is
spent. The three that survive scrutiny are:

1. **Nothing else serves our whole consumer set more cheaply.** E2B's
   supported path trades sovereignty for its control plane and adds four
   stateful dependencies; CubeSandbox would have us rebuild the
   workspace/SSH control plane on an E2B-shaped API while tracking a
   pre-1.0 project with a monthly breaking cadence; AgentENV is seven weeks
   old.
2. **Our divergence from forkd is small and upstream-shaped** — eight
   commits for the per-child backing plus three hygiene fixes, against
   primitives that are upstream and released. We are a contributor, not a
   fork maintainer.
3. **Our differentiators are not in any of these projects.** The CI runner,
   SSH-as-API control plane, identity model, agent surfaces and network
   policy semantics have no equivalent in the field, so swapping substrate
   would not buy them — it would only relocate the tier beneath them.

The cheap insurance is still the capability contract, not a second backend —
and the re-survey improved its payoff: three alternatives now speak an
E2B-shaped REST API, so the contract has a concrete second target where
before it had Hyper.

**Concrete, and worth fixing regardless:** `ForkdClient` has no
delete-snapshot method, while upstream exposes `DELETE /v1/snapshots/:tag`
and `POST /v1/snapshots/:tag/compact`. So every `clone-*` and record/replay
branch tag spoond mints can never be reclaimed. That is a spoond gap, not a
forkd one, and it is the kind that grows quietly.

### Decision, 2026-09-12: CubeSandbox declined

Decided against adopting CubeSandbox, and against even a spike, on
**operational** grounds rather than capability grounds: a control-plane
database plus Redis plus five services; monthly breaking minors carrying
stateful migrations validated only from the previous minor; and a
networking model that takes over the host (a default `192.168.0.0/18`
sandbox CIDR on `cube-dev`, 500 pre-created TAPs, a TC ingress filter on
the host NIC, TPROXY mangle chains and fwmark ip rules) — all in exchange
for deleting the image pipeline. The trade is refused. The choice made
instead is to invest in forkd and grow it, keeping the divergence
PR-shaped for as long as upstream accepts patches.

Note what this decision is **not**: it is not a finding that CubeSandbox
lacks surface area. It covers six of the seven substrate needs, and beats
forkd on egress policy and on images. It is a decision about what is worth
operating, and it should not be re-litigated on capability grounds.

## Triggers to re-open this

- **AgentENV, and only AgentENV.** It is the one candidate whose operator
  footprint is comparable to forkd's — a gateway, a scheduler and a node
  runtime, with **no database and no cache**, verified from its
  `deploy/docker-compose.yml` — and it speaks an E2B-compatible API. What
  it lacks is age: created 2026-07-23, no TLS by default, no external
  security review. Re-open when it has a security posture and a version
  worth pinning.
- **E2B Embed is not a watch item** despite being the strongest capability
  match: Postgres, Redis, ClickHouse, Nomad and object storage fail the
  same operational test CubeSandbox failed, and its supported self-host
  shape is someone else's control plane.
- forkd activity stalls, or upstream declines the per-child backing design
  behind #321 — at that point the fork stops being PR-shaped, and porting to
  one of the E2B-shaped runtimes becomes the cheap move rather than the
  expensive one.
- We need GPU, virtio-fs or live migration — a VMM swap (Cloud Hypervisor or
  QEMU), not a platform migration.
- We need cross-node resume: CubeSandbox has it; Firecracker snapshots are
  host-pinned (identical hardware, kernel and CPU model, version-pinned
  vmstate), so ours are not portable between hosts.

## Verification status

Checked directly against repos, tags and raw source on 2026-09-12: all
licenses, contributor counts, star counts, release tags and dates; that
workspaces exist in the v0.5.3 tag; that `drive_overrides` is absent from
Firecracker's latest release; E2B's self-host limitations, quoted from its
own README; CubeSandbox's license text and v0.7 pause/resume notes;
AgentENV's README claims. **Not independently verified:** all cold-start and
pause/resume latency figures (vendor or project numbers, including forkd's
~100 ms/100 children); E2B's "up to a hundred" fork count; CubeSandbox's
`<60 ms` claim. Treat those as claims to measure, not facts to plan on.
