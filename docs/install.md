# Installing spoond

spoond is the control plane; the sandboxes themselves are run by E2B's
orchestrator (see [substrate.md](substrate.md)). An install brings up one
host with both: the E2B orchestrator and its pinned Firecracker, guest
kernel and busybox, and spoond's backend, SSH gateway and dashboard, with
the local container registry. spoond ships no sandbox images: the host's
image catalog starts empty, and the operator builds the images leases
run ([images/README.md](../images/README.md)).

There are two ways to install, both from the same release:

- **Native**: `spoond install` on a Debian 13 host. It sets up the host,
  installs every service as a systemd unit.
- **Docker**: `deploy/docker/compose.yaml` runs the same services as
  containers, after one `prepare` run writes the host's kernel settings.

Both are idempotent: a rerun compares before it writes, reports each item
as `created`, `changed` or `kept`, and never overwrites a secret or env
file that exists.

## Requirements

- An x86-64 host with KVM (`/dev/kvm`) and at least 16 GiB of memory.
  Debian 13 is the tested distribution; others work with a warning
  (kernel 6.1 or newer). A virtual machine works when nested
  virtualisation is on.
- A snapshot store with at least 100 GiB free: an existing directory
  (XFS with reflink is recommended; `--store-dir`, default
  `/forkdcache/e2b`) or a spare disk the installer turns into a ZFS pool
  (`--store-disk /dev/<disk>`, native only).
- Free ports 8890 (lease API), 8891 (proxy), 2222 (SSH gateway), 8893
  (dashboard), 5000 (registry), 5007 and 5008 (orchestrator).
- Internet access during the install: apt packages, the release's
  runtime tarball and the pinned Firecracker, kernel and busybox.

Sandbox memory is hugepage-backed. `--hugepages auto` (the default) keeps
`max(6 GiB, 17 %)` of memory for the host and reserves the rest in 2 MiB
pages, both now and on the kernel command line; a 128 GiB host reserves
about 106 GiB, a 16 GiB host 10 GiB. Pass a page count to choose.

`sudo ./spoond install check` runs only the first phase: it reads the
host against this list and changes nothing.

## Native install

Download the release binary and run the installer as root:

```bash
curl -fsSLO https://github.com/jrimmer/spoond/releases/download/v4.1.0/spoond_4.1.0_linux_amd64.tar.gz
tar xzf spoond_4.1.0_linux_amd64.tar.gz
sudo ./spoond install --host-name sandbox.example.com --admin-key ~/.ssh/id_ed25519.pub
```

(`go install github.com/jrimmer/spoond/v4/cmd/spoond@v4.1.0` builds the
same binary.) It runs six phases in order and stops at the first failure
with what to do next. `spoond install <phase>` runs one, and `--dry-run`
prints the plan and changes nothing.

| Phase | Does |
|---|---|
| `check` | root, x86-64, `/dev/kvm`, memory, distribution and kernel, the free ports, the store's free space |
| `host` | apt packages (Docker among them); the `nbd` (`nbds_max=256`), `tun` and `kvm` modules and the nbd udev rule; sysctls and hugepages (also on the kernel command line, through `update-grub`); the legacy iptables backend; the snapshot store and its directories; Docker's store drop-in and the local registry (`registry:2.8.3` on `127.0.0.1:5000`); the AppArmor Unix-socket check |
| `runtime` | the release's `spoond-runtime` tarball (the orchestrator and envd) for this binary's version, checked against `SHA256SUMS`, or `--runtime <tarball>`; the pinned Firecracker, kernel and busybox, each checked by SHA-256; `/etc/e2b/orchestrator.env` and `flags.json`; the envd token seed; the `e2b-guard` firewall and the orchestrator unit; waits for the orchestrator's `/health` |
| `spoond` | the binary as `/opt/spoond/spoond`; every secret, `backend.env`, the drain hook's files, the gateway and dashboard env; TLS from `--tls-cert`/`--tls-key`, else a self-signed pair for `--host-name`; the backend, gateway, dashboard and drain units, started; waits for `/healthz`; the first admin user from `--admin-key` |
| `images` | the guest pieces an image's Dockerfile copies in, as `/var/lib/spoond/guest/` ([images/README.md](../images/README.md)). Nothing is built by default; `--images <manifest>` builds that manifest's baked images, pulled from `--base-registry <prefix>` when given and built locally otherwise |
| `doctor` | `spoond doctor` over the result; the install fails if doctor does |

`--host-name` defaults to `hostname -f`; it names the TLS certificate and
what the gateway advertises. Without `--admin-key` no user is created
(see [First-user bootstrap](#first-user-bootstrap)).

When the kernel cannot reserve the hugepages on the running host, the
host phase finishes its other steps and asks for a reboot; run
`spoond install` again after it. A Forgejo Actions runner is not installed
(see [spoond-runner](#spoond-runner-forgejo-actions-optional)).

### What it writes

| Path | Contents |
|---|---|
| `/etc/spoond/backend.env` | the backend's configuration and tokens (0600); the [backend reference](#spoond-backend-lease-api) lists every setting |
| `/etc/spoond/tls/` | the TLS pair the backend and dashboard serve |
| `/etc/spoond/admin-token` | the admin token the drain hooks read |
| `/etc/spoond/e2b-token-seed` | the HMAC seed for every envd and traffic token: losing it invalidates them all |
| `/etc/spoond/dash-password`, `dash.env` | the dashboard's generated password (user `admin`) and its configuration |
| `/etc/spoond-gateway.env`, `/etc/spoond-gateway/` | the gateway's token and host name; its keys (made at first start) |
| `/etc/e2b/orchestrator.env`, `flags.json`, `drain.env` | the orchestrator's configuration and drain hooks |
| `/var/lib/spoond/` | the SQLite database, the identity store (`users.json`), backups and the guest pieces images copy in (`guest/`) |
| `/etc/systemd/system/` | `e2b-guard`, `e2b-orchestrator`, `spoond-backend`, `spoond-sshd-gateway`, `spoond-dash`, `spoond-drain` |

Back up `/etc/spoond` and `/var/lib/spoond` together. Edit `backend.env`
by hand to change a setting (no inline comments: systemd keeps a
trailing `#` as part of the value), then `systemctl restart
spoond-backend`.

## Docker install

The containers run privileged with host networking: this packages
spoond, it does not isolate it. The host needs Docker with Compose and
the kernel settings the `prepare` service writes. From a checkout of the
release tag:

```bash
cd deploy/docker
export SPOOND_HOST_NAME=sandbox.example.com   # or put it in .env
SPOOND_VERSION=<version> docker compose build # the image, from this checkout
docker compose run --rm prepare     # modules, sysctls, hugepages, kernel command line
sudo update-grub                    # the hugepages on the kernel command line
# reboot only if prepare says the kernel could not reserve the pages
docker compose run --rm init        # config and secrets into ./config
docker compose up -d
cp ~/.ssh/id_ed25519.pub config/admin.pub
docker compose run --rm init install spoond --container --root=/config \
  --host-name="$SPOOND_HOST_NAME" --admin-key=/config/admin.pub
docker compose exec spoond spoond-container install images
docker compose exec spoond spoond-container doctor
```

spoond publishes no image: `docker compose build` builds it from the
checkout as `spoond:local` (`SPOOND_IMAGE`), and compose never pulls it.
The build takes the release's runtime tarball from
`deploy/docker/artifacts/` when one is there, else downloads it for
`SPOOND_VERSION`. The snapshot store is `SPOOND_STORE_DIR` on the
host (default `/forkdcache/e2b`; it must exist), and `./config`
(`SPOOND_CONFIG`) holds the configuration, the secrets and the database,
laid out as on a native host under `etc/` and `var/`: back it up.

To build your images, put their directory (manifest, Dockerfiles,
`spoond-guest-init`) under `config/var/lib/spoond/`, which the `spoond`
container sees as `/var/lib/spoond/`, and build there:

```bash
cp -r ~/my-images config/var/lib/spoond/images
docker compose exec spoond cp /var/lib/spoond/guest/spoond-guest-init /var/lib/spoond/images/
docker compose exec spoond spoond-container images \
  -manifest /var/lib/spoond/images/manifest.yaml \
  -context /var/lib/spoond/images build my-image
```

| Service | Does |
|---|---|
| `prepare` (run once) | `spoond install host --container` against the host's `/`: the modules, sysctls, hugepages and kernel command line. Packages, iptables, Docker and the AppArmor check are the host's own |
| `init` (run once) | the runtime and spoond phases' config and secrets under `./config`, starting nothing; rerun without `--config-only` once the stack is up to create the first admin user |
| `registry` | `registry:2.8.3` on `127.0.0.1:5000`, its data in the store |
| `orchestrator` | loads the `e2b-guard` rules and runs the orchestrator; on stop it drains first (`stop_grace_period` 6 min), and undrains once the orchestrator and backend are healthy |
| `spoond` | the backend, with the Docker socket for image builds |
| `gateway`, `dash` | the SSH gateway and the dashboard |

`docker compose exec spoond spoond-container <command>` runs any `spoond`
command with the backend's environment loaded (`doctor`, `images list`,
`ctl`, ...).

## Verify

`spoond doctor` checks the result: the configuration, the orchestrator's
health and free hugepages, the registry, the token seed, the database and
its migrations, the image catalog, the pinned artifacts by SHA-256,
storage headroom, the lease API, the gateway port, the LLM upstream and
TLS. The install runs it last; to run it again on a native host:

```bash
sudo spoond install doctor
```

Then end to end:

```bash
ssh new@sandbox.example.com -p 2222     # creates a dev sandbox and drops you in tmux
```

See [api.md](api.md) for the HTTP API, [ctl.md](ctl.md) for the control
plane and [operations.md](operations.md) for day-2 work. The manual
procedure the installer replaces is in the 3.x tree's
`docs/install.md` and in the substrate plan's U04.

## Configuration reference

### Architecture

```
                 ┌────────────────────── spoond ──────────────────────┐
  SSH :2222 ───▶ │ spoond-sshd-gateway        (dev sandboxes + ctl)   │
  HTTP :8891 ──▶ │   (proxy front, LLM gateway)                       │
                 │                                                    │
  HTTPS :8890 ─▶ │ spoond-backend ──gRPC──▶ e2b-orchestrator :5008    │
                 │   lease API, warm pool, TTL/idle sweeps            │
                 │                                                    │
  Forgejo ─────▶ │ spoond-runner (optional Forgejo Actions worker)    │
                 └────────────────────────────────────────────────────┘
```

The backend runs the control plane (leases, quotas, sharing, the catalog,
SQLite state) and talks to the E2B node orchestrator on loopback to
create, list, pause and checkpoint sandboxes. What the substrate is and
how that split works: [substrate.md](substrate.md).

> **Naming note.** Since 2.0 the service environment variables carry the
> `SPOOND_` prefix (`SPOOND_BACKEND_URL`, `SPOOND_AGENT_TOKEN`, …). The
> pre-2.0 `FORKD_` names still work everywhere but log a one-line
> deprecation warning; see the "Renamed in 2.0" table below.

### spoond-backend (lease API)

#### Environment

| Variable | Default | Purpose |
|---|---|---|
| `E2B_GRPC_ADDR` | `127.0.0.1:5008` | orchestrator gRPC address |
| `E2B_PROXY_URL` | `http://127.0.0.1:5007` | orchestrator sandbox proxy base URL |
| `E2B_TEAM_ID` | `5b0f4e3a-8c1d-4f2e-9a6b-7d3c2e1f0a95` | fixed team UUID sent with every gRPC request |
| `E2B_TOKEN_SEED_FILE` | `/etc/spoond/e2b-token-seed` | envd/traffic HMAC seed file (0600, ≥ 32 bytes; the backend exits without it) |
| `E2B_CREATE_TIMEOUT` | `5m` | per-call bound for orchestrator create/resume; a Go duration or seconds (spoond-j3a) |
| `E2B_PAUSE_TIMEOUT` | `5m` | per-call bound for orchestrator pause |
| `E2B_CHECKPOINT_TIMEOUT` | `5m` | per-call bound for orchestrator checkpoint |
| `E2B_DELETE_TIMEOUT` | `2m` | per-call bound for orchestrator delete |
| `E2B_NODEINFO_TIMEOUT` | `15s` | per-call bound for `NodeInfo` |
| `E2B_CONTROL_TIMEOUT` | `30s` | per-call bound for every other orchestrator call (list, update, drain override, template builds) |
| `SWEEP_TIMEOUT` | `15m` | bound for one background sweep stage (TTL release, the one paused-release clock, pool refill, job prune), so a hung substrate RPC frees the loop and the lease's busy flag |
| `E2B_TEMPLATE_STORAGE_PATH` | `/forkdcache/e2b/storage/templates` | build store — where GC and disk accounting look |
| `SPOOND_BUILD_TIMEOUT` | `1h` | how long a template build may run before the GC fails a still-`building` row as stale (twice this); a Go duration or seconds. The backend reads it from its own environment; `spoond images build` reads it separately, so set it for both (spoond-4yl, spoond-rzz) |
| `IMAGE_REGISTRY` | `localhost:5000` | registry `spoond images build` pushes to |
| `CONSUMER_TOKENS` | *(required)* | comma-separated `token=consumer` pairs, e.g. `abc=forgejo,def=pi` — consumers authenticate with bearer tokens |
| `USERS_FILE` | *(empty)* | identity store path (JSON, chmod 600). Set for multi-user tenancy: per-user keys, tokens, quotas, sharing |
| `BOOTSTRAP_TOKEN` | *(empty)* | gates the first-user bootstrap when the store is empty; unset = legacy open first-create |
| `GATEWAY_TOKEN` | *(empty)* | SSH gateway's service token; lets the gateway call the backend as the authenticated SSH user (trusted impersonation) |
| `ADMIN_TOKEN` | *(empty)* | bearer token for `/api/admin/*` (drain, undrain, reconcile); unset = those routes answer `404` |
| `METRICS_TOKEN` | *(empty)* | scrape-only token for `/metrics`; refused on every other route |
| `EVENTS_TOKEN` | *(empty)* | events-only token for the lease event streams (every owner's events); refused on every other route — spoond dash's `DASH_EVENTS_TOKEN` |
| `BIND_ADDR` | `127.0.0.1:8890` | lease API listen address (`0.0.0.0:8890` behind a proxy) |
| `POOL_SIZE` | `0` | warm-pool size **per image with a current build**; pre-created sandboxes served without a cold restore. `0` disables |
| `SANDBOX_PROBE` | `1` | check each sandbox runs a healthy toolchain before pooling or leasing it; `0` disables (see [ci-jobs.md](ci-jobs.md)) |
| `SANDBOX_PROBE_TIMEOUT_SECS` | `20` | exec timeout for each integrity probe |
| `CHECKPOINT_INTERVAL_MINS` | `0` | default per-lease background checkpoint interval in minutes for leases without their own `checkpoint_interval` (`0` = never; a lease's own interval overrides) |
| `MAX_KEPT_PER_LEASE` | `4` | kept-checkpoint cap per lease (`0` = no cap): a keep on a lease at the cap answers `409` and takes nothing (#126) |
| `MAX_NAMED_SNAPSHOTS` | `64` | distinct named-snapshot names per owner (`0` = no cap): a save that would add one past the cap answers `409 snapshot_limit` (#83) |
| `SNAPSHOT_KEEP_VERSIONS` | `3` | default versions a named snapshot keeps (`1`–`20`); the first save may override it per name and `PUT /api/named-snapshots/{name}` changes it later (#83) |
| `MAX_RUNNING_JOBS_PER_LEASE` | `16` | running background exec jobs per lease; past it a start answers `429` (#135) |
| `JOB_RETENTION_SECS` | `604800` | exited background-job records older than this (seconds) are pruned by the sweeper; running and lost records are kept; a job's own `retention_secs` shortens it (#135, spoond-hwto) |
| `JOB_OUTPUT_MAX_BYTES` | `0` | a running background job's stdout/stderr guest file grown past this (bytes) has its front punched out (`fallocate --punch-hole`) between reconcile passes, keeping the tail; 0 = unlimited; a job's own `output_max_bytes` shortens it, never lengthens it (spoond-hwto) |
| `JOB_MAX_RUNTIME` | `86400` (24 h) | how long a background job may run before the reconcile pass kills it and marks it exited with reason `timed_out`; takes a Go duration (`24h`) or seconds, a job may ask for a shorter `max_runtime_secs` but never a longer one, and a negative value disables the cap (the cap is held as whole seconds: a positive fractional duration rounds up, and a negative value under one second is rejected at startup) (spoond-wb5) |
| `KEPT_DISK_WARN_PCT` | `40` | kept-checkpoint share of the snapshot disk past which the notifier's `disk.kept` check warns and the dashboard's Notifications panel shows a message (`0` = off; #126) |
| `DISK_RESERVE_PCT` | `5` | snapshot disk share kept free: the take-back policy reserves it as the floor and the notifier's `capacity.disk_low` check alerts when free falls below it (clamped to 0–50; #145 FS3) |
| `GC_DELETE` | `0` | `1` = the snapshot catalog GC actually deletes; default dry-run only logs candidates (see [operations.md](operations.md)) |
| `ORPHAN_REAP` | `dryrun` | what the GC does with orphan build directories: `off` (hard stop, change nothing), `dryrun` (log only), or `quarantine` (move aside, delete after `ORPHAN_QUARANTINE_SECS`; see [operations.md](operations.md)) |
| `ORPHAN_MIN_AGE_SECS` | `3600` | don't reap a build directory modified more recently than this (it may still be written; see [operations.md](operations.md)) |
| `ORPHAN_QUARANTINE_SECS` | `86400` | how long a `quarantine`-mode orphan waits before it may be deleted (see [operations.md](operations.md)) |
| `PROXY_ADDR` | *(empty)* | `0.0.0.0:8891` to serve the HTTP proxy/LLM gateway listener (Caddy wildcard fronts it) |
| `PROXY_AUTH_MODE` | `off` | `off` = capability model (lease id is the credential); `forward-auth` = require `X-Proxy-Auth` secret + `Remote-User` identity |
| `PROXY_AUTH_SECRET` | *(empty)* | shared secret for `forward-auth` mode (set by Caddy/IdP; never forwarded to guests) |
| `PROXY_AUTH_TRUSTED_PEERS` | *(empty)* | comma-separated CIDRs allowed to set `Remote-User` |
| `HOST_GUEST_SERVICE_ADDR` | *(empty)* | host address guests use to reach host services (the proxy/LLM gateway) |
| `HOST_GUEST_SERVICE_PORT` | `8891` | host TCP port granted to guests with the above |
| `SPOOND_GUEST_DNS_ADDR` | *(empty)* | guest DNS resolver(s): each is granted to every lease's egress policy on port 53 and baked into the guest image. Comma-separated (`10.1.0.2,10.1.0.3`) for several; empty = no resolver allowance |
| `SPOOND_PROXY_HOST_SUFFIX` | `.sandbox.example.com` | wildcard hostname suffix the HTTP proxy routes (`<id>.<suffix>`, `<id>-<port>.<suffix>`) |
| `SPOOND_SSH_CONNECTION_ADDR` | `HOST_GUEST_SERVICE_ADDR` | gateway: server address reported in `SSH_CONNECTION` (single-host deploys need not set it) |
| `HOST_API_PORT` | `BIND_ADDR`'s port | lease API port `lan`/`internet` guests may reach on `HOST_GUEST_SERVICE_ADDR` (`0` = none) |
| `TLS_CERT` / `TLS_KEY` | *(empty)* | serve HTTPS on :8890 when both set. Comma-separated lists of equal length serve several certificates, chosen by the client's SNI and paired by position; the first is the default (no SNI, a connection by IP, or a name none covers). The files are re-read every minute, so a renewal needs no restart; a pair caught half-written keeps serving the old certificate until it is complete |
| `DEFAULT_TTL_SECS` | `300` | default lease TTL for non-persistent sandboxes |
| `MAX_TTL_SECS` | `3600` | maximum TTL a consumer may request |
| `IDLE_SUSPEND_DEFAULT_SECS` | `0` | default per-lease idle reclamation threshold for leases without their own `idle_suspend` (`0` = never; a lease's own `idle_suspend` overrides; #129 part 2) |
| `PAUSED_RELEASE_DAYS` | `30` | every paused lease is released this many days after its pause date (the one clock, v3.0 FS5) |
| `PINNED_IDLE_NOTICE_DAYS` | `7` | a pinned lease whose last API activity is older than this is flagged `pinned_idle_since` (visibility only; v3.0 FS5) |
| `MEMORY_FLOOR_MIB` | `2048` | free hugepage memory the low-memory floor keeps on the box: below it, every second, one unpinned lease of the owner furthest over their fair slice is paused (`0` disables; see [operations.md](operations.md)) |
| `MEMORY_CRITICAL_MIB` | half the floor, at most `2048` | below this much free hugepage memory the floor may take from any owner: unpinned leases of owners inside their slice too, and pinned leases last, each take logged and alerted as critical. Must be below `MEMORY_FLOOR_MIB`; a value set at or over it turns the level off with a startup warning; `0` turns it off, and so does a floor of `0` ([operations.md](operations.md#pins-and-the-paused-release-clock)) |
| `MAX_ADMIT_WAIT_SECS` | `600` | how long a create may wait for admission when it sends `wait` (`0` disables waiting; #129 part 1) |
| `SNAPSHOT_WRITE_CONCURRENCY` | `1` | how many memory-snapshot writes (substrate `Pause`/`Checkpoint`) may run at once process-wide; every pause and checkpoint goes through this limiter (`0` = unlimited, the pre-fix behaviour; spoond-t1s) |
| `DRAIN_SNAPSHOT_CONCURRENCY` | `2` | how many of those writes the admin drain may run at once, so a planned orchestrator restart can pause a batch of leases inside the unit's `TimeoutStopSec` (`0` = unlimited; see [operations.md](operations.md#restarting-the-orchestrator-planned)) |
| `UNDRAIN_CONCURRENCY` | `2` | how many drained leases the admin undrain may resume at once, so restoring a batch of large memory snapshots does not stack the node's I/O and memory (`0` = unlimited; spoond-urm) |
| `UNDRAIN_RESUME_RETRIES` | `2` | how many extra attempts a resume the admin undrain failed with a retryable envd/start error gets before the lease is left suspended with reason `resume_failed` (with a short backoff; `0` disables retries; spoond-urm, spoond-638d) |
| `UNDRAIN_READY_TIMEOUT` | `180s` | how long the admin undrain waits for a restarted orchestrator to answer `NodeInfo` and a `List` before the first resume, so a resume never runs before the node accepts sandbox creates (a Go duration or seconds; spoond-638d) |
| `UNDRAIN_RESUME_WINDOW` | `5m` | how long a resume that fails with an indeterminate orchestrator error (`Unavailable`, a connection reset) is retried with backoff; such an error never loses the lease (a Go duration or seconds; spoond-638d) |
| `DRAIN_MAX_SECS` | `900` | how long a drain may stay in effect while the node is healthy before spoond undrains itself and resumes the drained leases (`0` means the default; a negative value disables the automatic undrain; spoond-52c) |
| `DRAIN_RESUME_MAX_AGE` | `24h` | how long the drain self-heal loop keeps retrying a lease whose resume stays deferred before it stops and leaves the lease suspended (its snapshot intact) for the owner (`0` means the default; a negative value disables the bound; a Go duration or a plain integer of seconds; spoond-52c) |
| `RECOVERY_RETRY_ATTEMPTS` | `3` | how many failed crash-recovery attempts a lease gets before it is marked `lost`, so a transient busy-node failure does not lose a recoverable lease (`0` means the default; spoond-dxq) |
| `RECOVERY_RETRY_WINDOW` | `30m` | how long a lease may stay in recovery since its first failed attempt, whatever the failure kind, including a substrate capacity refusal waiting for room (a Go duration or seconds; spoond-dxq) |
| `MAX_EXEC_TIMEOUT_SECS` | `300` | ceiling on one exec/stream call's `timeout` (raise it for compile-heavy CI steps) |
| `MAX_EXEC_BODY_BYTES` | `8388608` (8 MiB) | cap on the JSON body of a POST exec and on every message of a `/stream` WebSocket; a bigger body is `413` (and a bigger stream frame closes with code `1009`). The guest's own argv limit is ~128 KiB per string |
| `EXEC_CONCURRENCY_MAX` | `8` | per-owner cap on concurrent exec/stream operations; the 9th and later waiters get `429`. A per-identity override (`exec_concurrency` on `POST /api/users/{id}/quota`, stored with the identity) replaces it for that user alone; `0` there means this default |
| `GUEST_DIAL_MAX` | `16` | per-owner cap on concurrent open guest-port dials (`GET /api/leases/{id}/ports/{port}/dial`); past it a dial gets `429`. A per-identity override (`guest_dial` on `POST /api/users/{id}/quota`) replaces it for that user alone; `0` there means this default |
| `ASSETS_DIR` | *(empty)* | serve static assets (the shelley binary) to guests at `/assets/<file>` on the proxy listener |
| `LLM_UPSTREAM_URL` | *(empty)* | OpenAI-compatible LLM API base for the per-lease LLM gateway |
| `LLM_UPSTREAM_KEY` | *(empty)* | server-side key for that upstream (never sent into sandboxes) |
| `LLM_DEFAULT_MODEL` | *(empty)* | default model id for LLM gateway requests |
| `LLM_MODEL_MAP` | *(empty)* | optional `pattern=model` comma-separated map |
| `LLM_MAX_CONCURRENT_PER_USER` | `0` | in-flight `/llm/` requests per user before `429` (`0` = unlimited) |
| `OTEL_PROM_URL` | *(empty)* | fetch the orchestrator's Prometheus output here and append it to `/metrics` |
| `SPOOND_DB_PATH` | `/var/lib/spoond/spoond.db` | SQLite state: leases, shares, pool, image catalog |
| `SPOOND_BACKUP_DIR` | `/var/lib/spoond/backups` | daily `VACUUM INTO` backups, keep 7 |

#### Run

```bash
export CONSUMER_TOKENS='abc=forgejo,def=pi'
export TLS_CERT=/etc/spoond/tls/fullchain.pem TLS_KEY=/etc/spoond/tls/privkey.pem
./spoond backend
```

#### systemd unit

See `deploy/spoond-backend.service`. The shipped unit reads
`EnvironmentFile=-/etc/spoond/llm.env` then
`EnvironmentFile=-/etc/spoond/backend.env` (`chmod 600`; a later file
wins, so the LLM pair may live in its own `llm.env` and `backend.env`
is the file the backend sources and the operator snippets in
this page and [operations.md](operations.md) read).

`SPOOND_BUILD_TIMEOUT` is read by two processes from two environments:
the backend (for the GC's stale-row sweep) and `spoond images build`
(for the `BuildTemplate` bound). Setting it only in the backend's env
file leaves the image pipeline on the default, and vice versa; set the
same value in both (the backend logs the effective value and its stale
threshold at start, and `spoond images build` prints its bound). See
[operations.md](operations.md#snapshot-catalog-gc) for the sweep.

### spoond-sshd-gateway (SSH + ctl plane)

#### Flags

| Flag | Default | Purpose |
|---|---|---|
| `--listen` | `:2222` | SSH listen address |
| `--host-key` | `/etc/spoond-gateway/ssh_host_ed25519_key` | SSH host key (generated if missing) |
| `--backend` | `https://127.0.0.1:8890` | spoond-backend base URL |
| `--backend-token` | *(required)*; env `SPOOND_GATEWAY_TOKEN` | spoond-backend service token (`GATEWAY_TOKEN`) — admin-equivalent, so it is read from the env file, never `ExecStart` |
| `--client-keys` | *(empty)* | comma-separated paths to authorized client public keys, **or a directory scanned for `*.pub` files** (legacy mode only) |
| `--gateway-key` | `/etc/spoond-gateway/gateway_ed25519` | gateway identity key (kept for unit compatibility; the gateway no longer connects into sandboxes with it) |
| `--gateway-host` | `sandbox.example.com` (env `SPOOND_GATEWAY_HOST`) | public hostname advertised in MOTDs |
| `--shelly-binary-url` | env `SHELLY_BINARY_URL` (`http://HOST_GUEST_SERVICE_ADDR:HOST_GUEST_SERVICE_PORT/assets/shelley`) | URL the sandbox fetches the shelley agent binary from |
| `--llm-gateway-url` | env `LLM_GATEWAY_URL` (`http://HOST_GUEST_SERVICE_ADDR:HOST_GUEST_SERVICE_PORT/llm/`) | base URL of the per-lease LLM gateway the shelley agent is pointed at |
| `--shelly-model` | `gpt-oss-20b-fireworks` | default model id written into shelley.json |
| `--ssh-images` | env `GATEWAY_SSH_IMAGES` *(empty)* | images that may serve interactive sessions (they need sshd and tmux); empty refuses every `new` |
| `--default-image` | env `GATEWAY_DEFAULT_IMAGE` *(empty)* | the image a bare `new` creates; empty means `new` must name one |
| `--metrics-listen` | env `GATEWAY_METRICS_LISTEN` *(empty = off)* | address for the gateway's own `/metrics` |
| `--image-aliases` | env `GATEWAY_IMAGE_ALIASES` *(empty)* | `short=full` image aliases for `ssh new-<short>@`; there are no built-in ones |
| `--bootstrap-token` | *(ignored)* | **deprecated**: accepted for unit compatibility; bootstrap via direct backend call only |

#### Key model

- **Identity store (recommended):** when the backend has `USERS_FILE`
  set, the store is the **single source of truth** for SSH keys. The
  gateway probes `GET /api/identity-status` at startup; in store mode
  every connecting key must resolve to a user (the local `--client-keys`
  allowlist is ignored), and deleting the user (`DELETE /api/users/{id}`)
  revokes access immediately. Create users via the `ssh-key` ctl verb or
  `POST /api/users` — see [api.md](api.md#users--identity).
- **Legacy mode (no identity store):** `--client-keys` lists the keys
  allowed to connect (as `ctl`, `new-*`, or `<lease-id>` usernames).
  Each user key must also be present in the sandbox images'
  `authorized_keys` if you want the gateway to connect into sandboxes on
  your behalf.
- `--gateway-key` is the gateway's own identity; its public half is in
  the image's `authorized_keys` via the image Dockerfile.

SSH sessions are relayed through the backend's `/stream` endpoint
(WebSocket) into the sandbox's PTY, so the gateway needs no network path
into any sandbox — see [substrate.md](substrate.md).

#### First-user bootstrap

On a fresh `USERS_FILE`, the first `POST /api/users` (via `ssh-key add`
or curl) creates the **admin** user. If `BOOTSTRAP_TOKEN` is set, that
create must present `X-Bootstrap-Token: <token>` — bootstrap is an
operator action done directly against the backend (the gateway
deliberately does not forward the token):

```bash
curl -s -X POST https://127.0.0.1:8890/api/users \
  -H "Authorization: Bearer <consumer-token>" \
  -H "X-Bootstrap-Token: <BOOTSTRAP_TOKEN>" \
  -H 'Content-Type: application/json' \
  -d '{"name":"jason","kind":"person","fingerprints":["SHA256:…"]}'
```

#### Run

```bash
./spoond gateway --backend https://127.0.0.1:8890 \
  --backend-token abc
```

### spoond-runner (Forgejo Actions, optional)

Environment-driven; registers as a Forgejo Actions runner and leases
sandboxes as CI workers. Key variables (see `cmd/spoond-runner/main.go`
for the full reference):

| Variable | Purpose |
|---|---|
| `FORGEJO_URL`, `RUNNER_TOKEN` | Forgejo instance and registration token (required) |
| `LEASE_URL` / `LEASE_TOKEN` | backend base URL and the bearer token the runner authenticates with |
| `LEASE_TOKEN_MAP` | `owner/repo` or `owner` → token file path, comma-separated: a mapped repository's jobs lease as their own identity in fair shares; anything unmapped uses `LEASE_TOKEN` ([ci-jobs.md](ci-jobs.md#per-project-lease-identity)) |
| `PIN_JOB_LEASES` | `owner/repo` or `owner` keys, comma-separated, or `*`: a covered job's lease is created pinned, so take-back for another owner's request cannot pause it mid-step; unset pins nothing ([ci-jobs.md](ci-jobs.md#pinning-job-leases)) |
| `IMAGE_MAP` / `DEFAULT_IMAGE` | `runs-on` label → image mapping, and the image for unmapped labels; no default, so a job whose labels map to no image fails ([ci-jobs.md](ci-jobs.md)) |
| `LEASE_TTL` | sandbox lease TTL seconds (default 600) |
| `EXEC_TIMEOUT_SECS` | per-step exec timeout override (0 = the backend's `MAX_EXEC_TIMEOUT_SECS`, default 300) |
| `RUNNER_ADMIT_WAIT_SECS` | seconds a create waits for admission on a full node (`wait`, #129; default 900, 0 = no wait). A create refused for capacity is retried per `Retry-After` until `RUNNER_JOB_TIMEOUT`, so a momentarily full node no longer fails a job |
| `RUNNER_JOB_TIMEOUT` | whole-job timeout as a Go duration or seconds (default 6h). A job's own `timeout-minutes` is the tighter bound, and a job that runs past it is reported cancelled. Bounds the create's capacity retry, so a node answering 503 forever cannot pin a worker |
| `LEASE_NETPOL` | egress policy the runner's leases get (`none` \| `lan` \| `internet` \| `restricted`; default `internet`) |
| `LEASE_NET_ALLOW` | comma-separated allowlist entries added on top of the policy |
| `RUNNER_FLOOR` / `RUNNER_MAX` / `RUNNER_SCALE_STEP` / `SCALE_UP_DELAY` / `SCALE_DOWN_DELAY` | registered-runner pool: floor, cap, step and scale delays |
| `RUNNER_STATE_FILE` | persists runner UUIDs across restarts (default `/var/lib/spoond/runner-state.json`) |
| `REPO_BASE_URL` | git host base URL for `actions/checkout` clones (no default; checkout fails without it) |
| `JOB_RECORD_DIR` | failed-job JSON records (default `/var/lib/spoond/jobs`) |

### Reverse proxy (TLS)

The gateway and proxy speak HTTP on loopback; put a TLS-terminating
reverse proxy in front with a wildcard cert so sandbox URLs work:

- `sandbox.example.com` → gateway :2222 (SSH, keep port 2222 or use a
  non-22 port per the MOTD hints)
- `*.sandbox.example.com` → proxy :8891 (each lease gets
  `<lease-id>.sandbox.example.com` and `<id>-<port>.sandbox.example.com`)

See `deploy/caddy-sandbox-forwardauth.conf` for the staged Caddy snippet
that also fronts the proxy with a forward-auth (Authelia) block — the
reference for the `PROXY_AUTH_MODE=forward-auth` setup in
[security.md](security.md).

### Building from source

One binary, all services; exclude any module with Go build tags:

```bash
go build -o spoond ./cmd/spoond                       # all modules
go build -tags 'nobackend,nomcp,norunner' -o spoond ./cmd/spoond  # subset
```

Subcommands: `backend`, `gateway`, `mcp`, `runner`, `ctl`,
`images`, `drain`, `doctor`, `dash`, `notify`.
Exclusion tags (one per gated file — `cmd/spoond/main.go` prints this
list at `spoond help`): `nobackend`, `nogateway`, `nomcp`,
`norunner`, `noctl`, `noimages`, `nodoctor`, `nodrain`, `nodash`,
`nonotify`.

### Agent endpoint (`mcp`)

`spoond mcp` authenticates to the backend as a **per-agent user**:
leases it creates are owned by that agent's identity.

| Variable | Default | Purpose |
|---|---|---|
| `SPOOND_AGENT_TOKEN` | *(empty)* | per-agent bearer token for this endpoint, provisioned from the users store; **required** |
| `SPOOND_BACKEND_URL` | `https://127.0.0.1:8890` | lease API base URL |
| `SPOOND_IMAGE` | *(none; required)* | the image for the leases it creates |

Create an agent user first (`POST /api/users` with `kind=agent` — it
needs a `token`, not an SSH key), then set `SPOOND_AGENT_TOKEN` to
that user's token. If it is not set, the endpoint fails fast
with provisioning instructions. The pre-2.0 `FORKD_*` names still work
(see "Renamed in 2.0" above).

### Renamed in 2.0

The pre-2.0 `FORKD_`-prefixed configuration variables were renamed to
`SPOOND_`. Each pair works as follows: the `SPOOND_` name is the
primary name; the old `FORKD_` name is still read as a fallback and
logs a one-line deprecation warning (once per variable per process).
When both are set, `SPOOND_` wins.

| 2.0 name | Pre-2.0 name (deprecated) | Used by |
|---|---|---|
| `SPOOND_BACKEND_URL` | `FORKD_BACKEND_URL` | `spoond mcp` — lease API base URL |
| `SPOOND_AGENT_TOKEN` | `FORKD_AGENT_TOKEN` | `spoond mcp` — per-agent bearer token |
| `SPOOND_IMAGE` | `FORKD_IMAGE` | `spoond mcp` — the image for new leases (required) |
| `SPOOND_CTL_HOST` | `FORKD_CTL_HOST` | `spoondctl` — gateway host |
| `SPOOND_CTL_PORT` | `FORKD_CTL_PORT` | `spoondctl` — gateway SSH port |
| `SPOOND_CTL_KEY` | `FORKD_CTL_KEY` | `spoondctl` — SSH private key |
| `SPOOND_GATEWAY_HOST` | `FORKD_GATEWAY_HOST` | `spoond gateway` `--gateway-host` default |
| `SPOOND_NO_TMUX` | `FORKD_NO_TMUX` | guest image: skip the tmux auto-attach on SSH login |

Not renamed: the gateway's `forkd-*` SSH permission keys and the
`forkd_id` field on `GET /api/leases/{id}/endpoint` are stored/protocol
data, not configuration — renaming them would break clients.

One special case: `SPOOND_NO_TMUX` is read by the guest image's login
shell (`/etc/profile.d/spoond-tmux.sh`), not by a long-lived spoond
process. There the deprecation warning is emitted once per user (a
canary file in the home directory dedupes across logins) rather than
once per process.
