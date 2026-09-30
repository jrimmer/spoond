# 01 — Architecture, addresses and configuration

Reference for every unit. Values here are **fixed**. Units refer to them by
name.

## Component diagram

```
consumers: runner, MCP, ACP, CFOS, CLI, browsers, SSH users
        │  lease API (unchanged contract, additions only)
        ▼
spoond-backend  (Go 1.27.1)                        spoond-sshd-gateway
  ├─ api/          lease API, proxy, stream           │ relays SSH sessions
  ├─ store/        SQLite (modernc.org/sqlite)        │ to /api/sandboxes/{id}/stream
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

## Addresses and ports on vm2

| Name | Value | Notes |
|---|---|---|
| `HOST_PRIMARY_IP` | `10.1.0.11` | vm2 on `vmbr0` (default-route interface) |
| `HOST_GUEST_SERVICE_ADDR` | `10.1.0.11` | address guests use to reach host services (replaces forkd's `10.43.0.1`) |
| Sandbox host CIDR | `10.11.0.0/16` | each sandbox's `HostIP` (a /32) |
| Sandbox veth CIDR | `10.12.0.0/16` | veth/vpeer /31 pairs |
| Guest IP (every sandbox) | `169.254.0.21` | fixed by E2B; isolated per netns |
| Orchestrator gRPC + `/health` | `0.0.0.0:5008` | firewalled to loopback (U04) |
| Orchestrator sandbox proxy | `0.0.0.0:5007` | firewalled to loopback (U04) |
| Hyperloop | `0.0.0.0:5010` | reachable only from sandbox sources (U04) |
| TCP egress proxy | `0.0.0.0:5016`, `:5017`, `:5018` | reachable only from sandbox sources (U04) |
| pprof | `127.0.0.1:6060` | unchanged |
| Local registry | `127.0.0.1:5000` | docker `registry:2` |
| OTel collector OTLP | `127.0.0.1:14317` | port 4317 is already used on vm2 |
| OTel Prometheus exporter | `127.0.0.1:19464` | scraped by spoond `/metrics` (U11) |
| spoond backend | `0.0.0.0:8890` | unchanged |
| spoond proxy / LLM gateway | `0.0.0.0:8891` | unchanged; guests reach it at `10.1.0.11:8891` |
| forkd controller | `127.0.0.1:8889` | removed in U12 |
| Staging backend | `0.0.0.0:18890` | U08–U12 only |
| Staging proxy / LLM gateway | `0.0.0.0:18891` | U08–U12 only |
| Staging SSH gateway | `0.0.0.0:12222` | U09–U12 only |

## Directories on vm2

| Path | Purpose | Created in |
|---|---|---|
| `/fc-versions/v1.14-0.2.0/amd64/firecracker` | Firecracker binary (E2B release) | U04 |
| `/fc-kernels/vmlinux-6.1.177_5008931/amd64/vmlinux.bin` | guest kernel | U04 |
| `/fc-busybox/1.36.1/amd64/busybox` | busybox for template builds | U04 |
| `/fc-envd/envd` | envd built from our fork | U04 |
| `/usr/local/lib/e2b/orchestrator` | orchestrator built from our fork | U04 |
| `/forkdcache/e2b/` | ZFS dataset `forkdcache/e2b` (all E2B state) | U04 |
| `/forkdcache/e2b/orchestrator/` | `ORCHESTRATOR_BASE_PATH` | U04 |
| `/forkdcache/e2b/storage/templates/` | `TEMPLATE_STORAGE_URL` (all builds) | U04 |
| `/forkdcache/e2b/storage/build-cache/` | `BUILD_CACHE_STORAGE_URL` | U04 |
| `/forkdcache/e2b/tmp/` | orchestrator `TMPDIR` (sockets, FIFOs) | U04 |
| `/etc/e2b/orchestrator.env` | orchestrator environment (below) | U04 |
| `/etc/spoond/e2b-token-seed` | 64 hex chars, `0600` (envd/traffic token seed) | U04 |
| `/etc/nftables.d/e2b-guard.nft` | host firewall table `inet e2b_guard` | U04 |
| `/etc/systemd/system/e2b-guard.service` | oneshot unit that loads `e2b-guard.nft` | U04 |
| `/root/src/spoond` | spoond checkout on vm2 (branch `feat/e2b-substrate` until U12, then `main`) | U07 |
| `/opt/spoond-staging/spoond` | staging spoond binary | U07 |
| `/etc/spoond-staging/` | staging `backend.env` and `gateway.env` (0600) | U08, U09 |
| `/var/lib/spoond/spoond.db` | spoond SQLite database | U05 |
| `/var/lib/spoond/backups/` | production `VACUUM INTO` backups | U11 |
| `/var/lib/spoond/backups-staging/` | staging `VACUUM INTO` backups | U11 |

## `/etc/e2b/orchestrator.env` (exact contents)

```ini
NODE_ID=vm2
NODE_IP=127.0.0.1
ENVIRONMENT=prod
ORCHESTRATOR_SERVICES=orchestrator,template-manager
GRPC_PORT=5008
PROXY_PORT=5007
PROVIDER=gcp
FIRECRACKER_VERSIONS_DIR=/fc-versions
HOST_KERNELS_DIR=/fc-kernels
HOST_BUSYBOX_DIR=/fc-busybox
BUSYBOX_VERSION=1.36.1
HOST_ENVD_PATH=/fc-envd/envd
DEFAULT_FIRECRACKER_VERSION=v1.14-0.2.0
DEFAULT_KERNEL_VERSION=vmlinux-6.1.177_5008931
ORCHESTRATOR_BASE_PATH=/forkdcache/e2b/orchestrator
SANDBOX_DIR=/fc-vm
ORCHESTRATOR_LOCK_PATH=/run/e2b-orchestrator.lock
TEMPLATE_STORAGE_URL=file:///forkdcache/e2b/storage/templates
BUILD_CACHE_STORAGE_URL=file:///forkdcache/e2b/storage/build-cache
ARTIFACTS_REGISTRY_PROVIDER=Local
LOCAL_UPLOAD_BASE_URL=http://127.0.0.1:5008
NBD_POOL_SIZE=32
NETWORK_VERSION=1
SANDBOXES_HOST_NETWORK_CIDR=10.11.0.0/16
SANDBOXES_VRT_NETWORK_CIDR=10.12.0.0/16
OTEL_COLLECTOR_GRPC_ENDPOINT=127.0.0.1:14317
TMPDIR=/forkdcache/e2b/tmp
E2B_FLAG_OVERRIDES_FILE=/etc/e2b/flags.json
```

`E2B_FLAG_OVERRIDES_FILE` is introduced by patch P5 (U03). Unset variables
(`REDIS_URL`, `CLICKHOUSE_CONNECTION_STRING`, `LAUNCH_DARKLY_API_KEY`,
`LOGS_COLLECTOR_ADDRESS`) stay **unset**. That disables Redis, ClickHouse,
LaunchDarkly and log shipping (A3 A2.6).

## `/etc/e2b/flags.json` (exact contents, read by patch P5)

```json
{
  "max-sandboxes-per-node": 100,
  "max-starting-instances-per-node": 6,
  "in-place-checkpoint": false,
  "use-sync-wp": false
}
```

Rationale, fixed:
- **100 sandboxes** fits vm2's RAM.
- **6 concurrent starts** is twice the default, for 16 vCPUs.
- **In-place checkpoint stays off.** It is E2B's newer path, and the default
  resume-fresh path is the tested one.

## spoond configuration: new environment variables

Added to `spoond-backend`'s environment file (`/etc/forkd-backend.env` on
vm2 per `deploy/README.md`; renamed in U12). The repo's
`deploy/spoond-backend.service` names `/etc/spoond-backend.env` instead, so
U02 step 0 records the real path from `systemctl cat spoond-backend`; every
later mention of `/etc/forkd-backend.env` means that recorded path. Existing
variables keep their meaning (A1 §8), except where a unit says otherwise.
`CONSUMER_TOKENS` is required by the backend (it exits without it).

| Variable | Value on vm2 | Default in code | Meaning |
|---|---|---|---|
| `SPOOND_DB_PATH` | `/var/lib/spoond/spoond.db` | `/var/lib/spoond/spoond.db` | SQLite file (U05) |
| `E2B_GRPC_ADDR` | `127.0.0.1:5008` | `127.0.0.1:5008` | orchestrator gRPC |
| `E2B_PROXY_URL` | `http://127.0.0.1:5007` | `http://127.0.0.1:5007` | orchestrator sandbox proxy |
| `E2B_TOKEN_SEED_FILE` | `/etc/spoond/e2b-token-seed` | same | HMAC seed file (64 hex chars) |
| `E2B_TEAM_ID` | `5b0f4e3a-8c1d-4f2e-9a6b-7d3c2e1f0a95` | same | fixed team UUID sent on every request |
| `IMAGE_REGISTRY` | `localhost:5000` | `localhost:5000` | where U07 pushes images |
| `HOST_GUEST_SERVICE_ADDR` | `10.1.0.11` | none (required) | host address guests use for spoond services |
| `HOST_GUEST_SERVICE_PORT` | `8891` (staging `18891`) | `8891` | host port guests use (the spoond proxy / LLM gateway) |
| `ADMIN_TOKEN` | secret | empty (admin routes disabled) | bearer token for `/api/admin/*` (U10) |
| `E2B_TEMPLATE_STORAGE_PATH` | `/forkdcache/e2b/storage/templates` | same | build storage root, for disk accounting (U11) |
| `GC_DELETE` | `0` until U12 step 17, then `1` | `0` | `1` lets GC delete builds (U11) |
| `SPOOND_BACKUP_DIR` | `/var/lib/spoond/backups` (staging `/var/lib/spoond/backups-staging`) | `/var/lib/spoond/backups` | daily SQLite backups (U11) |
| `CHECKPOINT_INTERVAL_MINS` | `60` | `60` | periodic checkpoint of persistent leases; `0` disables (U10) |
| `OTEL_PROM_URL` | `http://127.0.0.1:19464/metrics` | empty | appended to spoond `/metrics` when set (U11) |

## Data model (owned by spoond; SQLite; full DDL in U05)

- **leases**: every lease, persistent or not, suspended or running.
- **shares**: lease shares.
- **pool**: warm-pool entries (sandbox id, image).
- **images**: one row per image name. Current template id, current build id,
  sizing, digest.
- **builds**: one row per E2B build. Kind is `template`, `pause` or
  `checkpoint`. Stores parent build id, versions, state and size.
- **build_refs** (U11): every other build whose blocks a build references,
  from E2B's scheduling metadata; GC keeps them.
- **sandboxes**: one row per running E2B sandbox. Links a lease to a sandbox
  id and records host IP, token reference and last checkpoint.

## Request flow examples (normative)

**Create a lease** (`POST /api/sandboxes {"image":"py-base"}`):
1. Look up the image's current build.
2. Run admission (hugepages and cap).
3. Allocate a `sandbox_id`.
4. Mint tokens.
5. Call `SandboxService.Create` with `snapshot=false`.
6. Store the `sandboxes` and `leases` rows.
7. Apply the network policy via the egress config in the same `Create`.
8. Return the lease.

**Exec** (`POST /api/sandboxes/{id}/exec`): envd `Process.Start` through the
proxy, collecting stdout/stderr until `EndEvent`.

**Clone**:
1. `SandboxService.Checkpoint` into a new build.
2. `SandboxService.Create` from that build with a new `sandbox_id`.
3. A new persistent lease.

**Suspend**: `SandboxService.Pause` into a new build; the lease becomes
`suspended` with `resume_build_id`. **Resume**: `SandboxService.Create` with
`snapshot=true` and the same `sandbox_id`.

## Staging vs production (U06–U12)

There is **no forkd adapter**:
- Production keeps running the spoond binary built from `main` (U01 + U05,
  merged to `main` by the orchestrator and deployed by the Ops runner in an
  Autonomous window at the end of U05; on
  forkd) until the U12 cutover.
- All E2B development runs on a **staging** instance on vm2, sharing the one
  orchestrator:
  - `spoond-backend-staging` (`:18890`/`:18891`, DB
    `/var/lib/spoond/staging.db`, users
    `/var/lib/spoond/staging-users.json`), set up in U08;
  - `spoond-sshd-gateway-staging` (`:12222`), set up in U09.
- The orchestrator's drain hooks point at staging until U12 switches them to
  production.
