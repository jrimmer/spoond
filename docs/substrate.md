# The E2B substrate

## Glossary

| Term | Meaning |
|---|---|
| host | the machine running spoond and the E2B orchestrator |
| lease | a granted sandbox — the API resource (`POST /api/leases`; `/api/sandboxes` is its permanent alias) |
| bee | an agent worker running in a lease |
| swarm | a group of bees |
| hive | the enlistment service: `/hive/guide`, `/hive/check` |

spoond is a **control plane**: identity, quotas, leases, sharing, the SSH
gateway, the HTTP proxy, the LLM gateway, policy, the image catalog and
SQLite state. The **data plane** — actually running sandboxes — is E2B's
node orchestrator, driven over gRPC from our patch-queue fork of
`github.com/e2b-dev/runtime`. This page is the one-stop description of
that split: what runs where, what the pieces are called, and what happens
when the orchestrator restarts or crashes.

The full design record, with every fixed decision and the per-unit
implementation specs, is
[docs/plans/2026-09-30-e2b-substrate/](plans/2026-09-30-e2b-substrate/00-README.md);
addresses, ports and directories are listed in
[01-architecture.md](plans/2026-09-30-e2b-substrate/01-architecture.md).
The fork is upgraded with the procedure in
[e2b-upgrade.md](runbooks/e2b-upgrade.md).

## Architecture

```
consumers: runner, MCP, ACP, CFOS, CLI, browsers, SSH users
        │  lease API (unchanged contract, additions only)
        ▼
spoond-backend  (Go 1.27.1)                        spoond-sshd-gateway
  ├─ api/          lease API, proxy, stream           │ relays SSH sessions
  ├─ store/        SQLite (modernc.org/sqlite)        │ to /api/leases/{id}/stream
  ├─ substrate/    Substrate interface                ┘ (WebSocket)
  │   └─ e2b/      gRPC → orchestrator :5008, envd via proxy :5007
  └─ images/       template build driver (U07)
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

## What E2B is, and how we use it

E2B publishes an Apache-2.0 runtime for AI-code sandboxes. We run **only
its per-node orchestrator** — not its API, dashboard, client proxy,
Postgres, ClickHouse, Redis, Nomad or cloud storage. spoond talks to two
of its listeners on loopback:

| Listener | Address | Used for |
|---|---|---|
| Orchestrator gRPC (plus `GET /health`) | `127.0.0.1:5008` (`E2B_GRPC_ADDR`) | create/list/delete sandboxes, pause, checkpoint, egress updates, node info, template builds |
| Orchestrator sandbox proxy | `127.0.0.1:5007` (`E2B_PROXY_URL`) | HTTP to a sandbox's guest ports, routed by the `E2b-Sandbox-Id`/`E2b-Sandbox-Port` headers |

Everything else it listens on (`:5010` hyperloop, `:5016`–`:5018` TCP
egress proxy) is reachable only from sandbox source addresses and is
blocked from off-host by the `e2b_guard` nftables table; see
[security.md](security.md).

The vocabulary, because spoond's names map onto it:

| spoond says | E2B calls it | Meaning |
|---|---|---|
| image | template | a bootable, snapshotted microVM; `template_id` is 20 chars `[a-z0-9]`, **stable for the life of the image name** |
| build | build | one immutable artifact set (rootfs + memory snapshot + metadata), a UUID; template builds, pauses and checkpoints each make one |
| pause | pause | snapshot to a new build, then stop the sandbox |
| resume | create with `snapshot=true` | restore from that build with the **same sandbox id** |
| checkpoint | checkpoint | snapshot a *running* sandbox; it keeps running from the new build |
| clone / fork | checkpoint + create | one checkpoint, then N creates from it |
| envd | envd | E2B's guest agent (HTTP + Connect-RPC on guest port 49983); spoond's exec, stream, health and file traffic all go through it |

Two properties drive the design:

- **Warm starts.** Every sandbox create is a memory-snapshot restore, so
  a lease is granted in tens of milliseconds rather than seconds. The
  measured production numbers are in the spec's `RESULTS.md`.
- **Fork with memory.** Checkpointing a running sandbox and creating
  from the build gives a fork of a live sandbox *including its RAM* —
  that is what `clone` and `fork` are, and it is why a dev sandbox
  resumes with its tmux session intact.

Memory is **fixed per image** (a snapshot restores with its build's RAM),
so `memory_mib` on create must be `0` or exactly the image's `memory_mb`
(see [api.md](api.md)). E2B builds every template with 2 MiB hugepages,
so spoond always requests them and admits a create only when enough free
hugepage memory exists; running out surfaces as `503 capacity`, never as
an overcommitted host.

## The Substrate interface

`substrate/` is the seam that hides E2B from the rest of spoond:
`api/`, `cmd/spoond-images` and `cmd/spoond-doctor` depend only on the
`substrate.Substrate` interface (`substrate/substrate.go`), never on
gRPC or E2B types. `substrate/e2b` is the only implementation in
production; `substrate/fake` exists for the unit tests. The interface
covers template building, sandbox lifecycle (create, list, delete, pause,
checkpoint), live egress updates, node info and drain signalling, and the
guest surface (health, exec, interactive process start, raw TCP dial,
traffic tokens).

## Images

Images are **container-defined, VM-isolated**: each is a Dockerfile in
`images/`, referenced by `images/manifest.yaml` with sizing (`vcpu`,
`memory_mb`, `disk_mb`) and the env every sandbox gets. `spoond images
build <name>` (or `--all`) builds it with docker, pushes it to the local
registry on `127.0.0.1:5000`, and has the orchestrator's template
manager turn it into a template build. The Dockerfile must be
Debian/Ubuntu/Fedora/Arch/Alpine/NixOS-based (E2B rejects RHEL); the
common tail (`images/guest/spoond-guest-init`) sets guest DNS to the LAN
resolver, drops a container marker for tools such as kaniko, runs
`/etc/spoond/init.d/*` hooks and then waits for the ready file, which is
the moment the snapshot is taken.

Catalog state — the current build of each image and every build ever
recorded, with its kind (`template`, `pause`, `checkpoint`), versions and
size — lives in the same SQLite database as leases. Nothing builds a
rootfs or consults an allowlist. `GET /api/images` is the catalog. (The
`KNOWN_IMAGES` name survives only in a Prometheus help string for the
`pool_cap` gauge.) See
[install.md](install.md) for the build command and
[operations.md](operations.md) for the GC that reclaims unreferenced
builds.

## Restart and crash behaviour

The orchestrator keeps its data plane **in-process**: one process owns
every Firecracker microVM on the node, so an orchestrator restart or
crash kills every running sandbox. This is the accepted trade-off (the
spec's fixed decision D4); spoond makes both cases as harmless as it can.

- **A planned restart is lossless — the drain protocol.** The
  `e2b-orchestrator` unit runs `spoond drain --stop` as `ExecStop` and
  `spoond drain --start` as `ExecStartPost`. `--stop` asks the backend
  to pause every live lease into a pause build (marking it `drained`),
  delete the warm pool, set the node draining and wait until nothing is
  running or in flight; `--start` waits for the node, clears draining and
  resumes exactly the drained leases. If systemd reports a failed unit
  (`SERVICE_RESULT` ≠ `success`) the drain is skipped — there is nothing
  left to pause — and recovery takes over. `--stop` always exits 0: a
  failed drain must never block the stop.
- **A crash recovers what has a checkpoint and reports the rest.** On
  start, every 30 s, and as soon as the orchestrator answers again,
  `reconcileCrash` compares the leases with the sandboxes that survived:
  a lease with a checkpoint is resumed from it (same sandbox id, state
  `recovered`); a lease without one becomes `lost` and answers `410` on
  exec, stream, proxy and SSH until it is deleted. Leases are never
  marked lost when the sandbox list itself cannot be read.
- **Persistent leases are checkpointed periodically** (default every
  `CHECKPOINT_INTERVAL_MINS` = 60 minutes, only when active), which
  bounds how much a crash can cost. `POST /api/leases/{id}/checkpoint`
  does it on demand.

The states a lease can be in are `running`, `suspended`, `recovered` and
`lost`; `recovered` behaves exactly like `running` until the lease is
suspended or restarted. Lease state, leases and the image catalog all
live in SQLite, so **a backend restart loses nothing** — the next
incarnation loads them and reconciles.

## Staying close to upstream

We do not carry a divergent fork. `git.example.com/example/e2b-runtime`
mirrors upstream (`upstream` branch, pristine) plus a short patch series
(`spoond` branch, at most five patches: startup-reclaim scoping, drain
hooks, private egress allowances with a host-address guard, flag
overrides), rebased at most monthly and gated by the conformance suite.
The procedure — including how to rebase, when a new Firecracker or kernel
version may be adopted, and why version directories are never deleted
while builds still reference them — is [e2b-upgrade.md](runbooks/e2b-upgrade.md).

## Related pages

- [install.md](install.md) — bringing up a host: orchestrator, firewall,
  registry, images.
- [operations.md](operations.md) — health, `spoond doctor`, backups, GC,
  restarting the orchestrator and the backend, the dashboard.
- [security.md](security.md) — Firecracker without the jailer, port
  firewalling, the host-address guard, tokens.
- [api.md](api.md) — the lease API that sits on top of all of this.
