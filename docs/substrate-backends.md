# Multiple microVM backends?

Assessment of making spoond's microVM substrate pluggable, and whether
Hyper (`harmont-dev/hyper`, Elixir) can be a second backend. Written after
the forkd shared-rootfs work (upstream #317, spoond #66) prompted the
question.

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
   runtime. There is already a `/v1/capabilities` endpoint to hang this on.
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
