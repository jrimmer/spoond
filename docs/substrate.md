# The substrate: E2B

spoond does not run virtual machines itself. The sandbox substrate is
**E2B's open-source node runtime** — the orchestrator, the template
manager and the `envd` guest agent — run standalone from our patch-queue
fork of `github.com/e2b-dev/runtime` (`lacy.casa/e2b-runtime`). spoond is
the control plane on top of it: identity, quotas, leases, sharing, the SSH
gateway, the proxy, policy, the image catalog and SQLite state. The
[lease API](api.md) is the compatibility contract; everything beneath it
may change.

```
consumers: runner, MCP, ACP, CFOS, CLI, browsers, SSH users
        │  lease API (unchanged contract, additions only)
        ▼
spoond-backend  (Go 1.27.1)                        spoond-sshd-gateway
  ├─ api/          lease API, proxy, stream           │ relays SSH sessions
  ├─ store/        SQLite (modernc.org/sqlite)        │ to /api/sandboxes/{id}/stream
  ├─ substrate/    Substrate interface                ┘ (WebSocket)
  │   └─ e2b/      gRPC → orchestrator :5008, envd via proxy :5007
  └─ images/       template build driver
        │ gRPC (plaintext, 127.0.0.1:5008)      HTTP (127.0.0.1:5007)
        ▼                                          │ E2b-Sandbox-Id / -Port headers
e2b-orchestrator.service (our fork, root)  ◄───────┘
  ├─ SandboxService / TemplateService / InfoService (gRPC :5008, /health)
  ├─ sandbox proxy :5007 → <HostIP>:<port> (envd = 49983)
  ├─ per sandbox: netns ns-N, veth-N, tap0, NBD rootfs overlay, UFFD memory
  ├─ tcpfirewall :5016/:5017/:5018 (guest TCP egress), hyperloop :5010
  └─ Firecracker v1.14-0.2.0 + kernel vmlinux-6.1.177_5008931 + envd (in guest)
docker: registry:2 on 127.0.0.1:5000  ◄── docker build ◄── images/*.dockerfile
otelcol-contrib: OTLP 127.0.0.1:14317 → Prometheus 127.0.0.1:19464
```

spoond speaks to the orchestrator over gRPC on `127.0.0.1:5008`
(`E2B_GRPC_ADDR`) and to `envd` through the orchestrator's sandbox proxy
on `127.0.0.1:5007` (`E2B_PROXY_URL`), using Connect-RPC. The
`substrate/` Go interface hides E2B from the rest of spoond, so the
control plane never imports the substrate's own types. We run only the
orchestrator and the template manager — not E2B's API, dashboard,
client-proxy, Postgres, ClickHouse, Redis, Nomad or cloud storage.

## What E2B gives us

- **Warm starts.** An image is built once into a *template*: a booted
  microVM that is snapshotted (rootfs plus memory). Every sandbox create
  is a memory-snapshot restore, so a create is a restore, not a boot.
- **Fork, pause/resume and checkpoint.** Snapshotting a running sandbox
  with its memory is native: `pause` snapshots and stops, `checkpoint`
  snapshots and keeps running, `fork` checkpoints and then creates N
  sandboxes from that build.
- **VM isolation.** Every sandbox is its own Firecracker microVM with its
  own network namespace, veth pair and rootfs overlay; guest egress is
  filtered per sandbox by the orchestrator's nftables and TCP proxy.
- **A guest agent to talk to.** `envd` runs inside every guest (port
  49983) and provides exec, file operations and process control over
  Connect-RPC, reached through the orchestrator's proxy. The SSH gateway
  relays SSH session channels onto `envd` — no `sshd` in the guest, and
  no `setns` anywhere in spoond.

## Images

Images are "container-defined, VM-isolated": one Dockerfile per
capability under `images/`, described by `images/manifest.yaml`, pushed to
a local Docker registry, and turned into an E2B template by
`spoond images build` (see [ci-jobs.md](ci-jobs.md)). A *build* is one
immutable artifact set (rootfs, memory snapshot, metadata) identified by a
UUID; every image build, pause and checkpoint creates one, and the catalog
in SQLite tracks them so the GC can delete the unreferenced ones.

## The crash trade-off, in plain words

The orchestrator is a single process that owns every running sandbox. If
it crashes — or the host loses power — every running sandbox dies, and a
lease's running state comes back only as far as its **last snapshot**: the
pause that suspended it, or the periodic checkpoint if it is persistent.
Work done inside the sandbox since that snapshot is gone. That is the
accepted trade-off for warm starts and cheap snapshots.

Two things follow:

- **Planned restarts lose nothing.** The [drain
  protocol](operations.md#the-drain-protocol-planned-orchestrator-restarts)
  pauses every running sandbox, restarts the orchestrator, and resumes
  each one with its memory intact. Use it for every deliberate restart.
- **Unplanned crashes are bounded, not prevented.** Persistent leases are
  checkpointed every `CHECKPOINT_INTERVAL_MINS` (default 60), and
  reconciliation after the crash resumes what it can and marks the rest
  `lost`. Lower the interval if a workload's re-run cost is higher than
  the checkpoint cost.

Long-term VMs are out of scope — they live on Proxmox (vm1), not here.

## Staying close to upstream

The fork is a mirror (`upstream`, never edited) plus a short patch series
(`spoond`, P1–P5), each patch with an upstream PR link or a written
reason. It is rebased onto upstream at most monthly, gated by the
[conformance suite](../conformance/README.md). The procedure is the
[E2B upgrade runbook](runbooks/e2b-upgrade.md) (U13).

## Further reading

- [The implementation spec](plans/2026-09-30-e2b-substrate/00-README.md) —
  architecture, fixed decisions, per-unit specs and ops appendices.
- [install.md](install.md) — host setup and the image build.
- [operations.md](operations.md) — drain, crash recovery, backups, GC.
- [security.md](security.md) — the threat model and hardening notes.
