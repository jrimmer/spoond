# Installing spoond

spoond is the control plane; the sandboxes themselves are run by E2B's
orchestrator (see [substrate.md](substrate.md)). Installing the platform
means bringing up one host with both:

1. **The E2B host** — kernel modules, sysctls and hugepages, storage, the
   pinned Firecracker/kernel/busybox artifacts, our orchestrator and envd
   binaries, the host firewall, the local container registry and the
   OpenTelemetry collector.
2. **spoond** — one Go binary (`spoond backend`, `spoond gateway`, the
   runner, the dashboard), the SQLite database, the identity store and
   the image catalog.

There is no single installer yet; the steps below are the procedure the
production host was installed with, and every script referenced lives in
`deploy/e2b/` in this repo. `deploy/install-spoond.sh` is the forkd-era
installer and is no longer the path.

## Prerequisites

- A Linux host with KVM, kernel 6.x, cgroup v2, and a reflink-capable
  filesystem (ZFS 2.2+ block cloning, XFS or btrfs) for the build store.
- Go 1.27.1+ (the module requires `go 1.27.1`).
- docker (for the local registry and the image builds), nftables,
  iproute2, util-linux, e2fsprogs, rsync, curl, iptables.
- The E2B fork built or cloned, for the orchestrator and envd binaries.

```bash
apt-get install -y --no-install-recommends \
  iptables rsync e2fsprogs iproute2 util-linux curl nftables
git clone https://git.lacy.casa/lacy.casa/spoond.git && cd spoond
```

## 1. Host setup

Run `deploy/e2b/host-setup.sh` as root. It is idempotent and covers the
host steps of the spec's U04: packages and the `nbd`/`tun`/`kvm` modules
(`nbds_max=256`), the sysctl file (`vm.nr_hugepages`, `vm.max_map_count`,
`net.ipv4.ip_forward`), the MSS-clamp rule, the ZFS dataset and
directories, the pinned Firecracker/kernel/busybox artifacts (fetched by
SHA-256), the `e2b_guard` nftables table and its oneshot unit, and the
local registry container on `127.0.0.1:5000`.

Two things it deliberately does **not** do, because they are one-off and
host-specific:

- **Hugepages sizing.** The script writes `vm.nr_hugepages = 12288`
  (24 GiB), the bring-up value used beside the previous substrate. Size
  it for your workload instead: sandbox memory is hugepage-backed, so the
  sum of every running sandbox's `memory_mb` must fit. The production
  host runs 24576 (48 GiB). Apply changes with `sysctl -p` on that one
  file, never `sysctl --system`.
- **The orchestrator and envd binaries.** They come from our fork of
  E2B's runtime, not from upstream releases:

  ```bash
  install -D -m 0755 /root/src/e2b-runtime/packages/envd/bin/envd /fc-envd/envd
  install -D -m 0755 /root/src/e2b-runtime/packages/orchestrator/bin/orchestrator \
    /usr/local/lib/e2b/orchestrator
  ```

## 2. Orchestrator configuration

Install `/etc/e2b/orchestrator.env` from `deploy/e2b/orchestrator.env`
and `/etc/e2b/flags.json` from `deploy/e2b/flags.json`, then edit the
addresses: `SANDBOXES_HOST_NETWORK_CIDR` and `SANDBOXES_VRT_NETWORK_CIDR`
must not collide with anything on the host (production uses
`10.11.0.0/16` and `10.12.0.0/16`), and `ORCHESTRATOR_BASE_PATH`,
`TEMPLATE_STORAGE_URL`, `BUILD_CACHE_STORAGE_URL` and `TMPDIR` point at
the dataset created in step 1. Redis, ClickHouse, LaunchDarkly and log
shipping stay **unset** — that is what keeps the orchestrator
standalone.

Then:

```bash
chmod 600 /etc/e2b/orchestrator.env /etc/e2b/flags.json
install -d -m 700 /etc/spoond
test -s /etc/spoond/e2b-token-seed || openssl rand -hex 32 > /etc/spoond/e2b-token-seed
chmod 600 /etc/spoond/e2b-token-seed
install -D -m 644 deploy/e2b/e2b-orchestrator.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now e2b-guard e2b-orchestrator
curl -fsS http://127.0.0.1:5008/health     # {"status":"healthy",...}
```

`/etc/spoond/e2b-token-seed` is the HMAC seed for every envd/traffic
token; losing it invalidates all of them, so back it up with the rest of
`/etc/spoond`.

## 3. Metrics plumbing (optional but recommended)

Install the OpenTelemetry collector with `deploy/e2b/otelcol-config.yaml`
as `/etc/otelcol-contrib/config.yaml` **before** installing the package
(the package starts the service on install, and its default config would
bind an already-used port). The collector takes OTLP on
`127.0.0.1:14317` and exposes Prometheus on `127.0.0.1:19464`; point
`OTEL_PROM_URL` at the latter to have spoond's `/metrics` include the
orchestrator's own metrics.

## 4. Build and install spoond

```bash
export PATH=/usr/local/go/bin:$PATH
go build -o /opt/spoond/spoond ./cmd/spoond
/opt/spoond/spoond help
install -D -m 644 deploy/spoond-backend.service deploy/spoond-sshd-gateway.service \
  deploy/spoond-runner.service /etc/systemd/system/
```

The backend unit sources `/etc/spoond/backend.env` (0600). Minimum
contents for a new deployment — see [setup.md](setup.md) for the full
variable reference:

```ini
CONSUMER_TOKENS=<random>=<consumer>          # required; the backend exits without it
SPOOND_DB_PATH=/var/lib/spoond/spoond.db
E2B_GRPC_ADDR=127.0.0.1:5008
E2B_PROXY_URL=http://127.0.0.1:5007
E2B_TOKEN_SEED_FILE=/etc/spoond/e2b-token-seed
IMAGE_REGISTRY=localhost:5000
HOST_GUEST_SERVICE_ADDR=<host primary IP>    # required; what guests use to reach host services
HOST_GUEST_SERVICE_PORT=8891
E2B_TEMPLATE_STORAGE_PATH=/forkdcache/e2b/storage/templates
SPOOND_BACKUP_DIR=/var/lib/spoond/backups
USERS_FILE=/var/lib/spoond/users.json        # identity store: multi-user tenancy
BOOTSTRAP_TOKEN=$(openssl rand -hex 24)      # gates the first (admin) user
GATEWAY_TOKEN=$(openssl rand -hex 32)        # the SSH gateway's service token
ADMIN_TOKEN=$(openssl rand -hex 32)          # /api/admin/* (drain, undrain, reconcile)
METRICS_TOKEN=$(openssl rand -hex 32)        # scrape-only /metrics (Prometheus, dashboard)
BIND_ADDR=0.0.0.0:8890
PROXY_ADDR=0.0.0.0:8891
```

Write the gateway environment to `/etc/spoond-gateway.env` (0600) with
`SPOOND_GATEWAY_TOKEN=<the GATEWAY_TOKEN value>`; the gateway unit reads
it so the token never appears in `ExecStart`.

Point the orchestrator's drain hooks at the backend
(`/etc/e2b/drain.env`, 0600) — this is what makes orchestrator restarts
lossless:

```ini
SPOOND_DRAIN_URL=https://127.0.0.1:8890
SPOOND_ADMIN_TOKEN_FILE=/etc/spoond/admin-token
SPOOND_DRAIN_INSECURE=1
```

…and add to `e2b-orchestrator.service`'s `[Service]` section:

```ini
EnvironmentFile=/etc/e2b/drain.env
ExecStop=/opt/spoond/spoond drain --stop
ExecStartPost=/opt/spoond/spoond drain --start
```

Keep the unit's `TimeoutStartSec=420` and `TimeoutStopSec=330`: with
systemd's default 90 s, a slow undrain would fail the start and
`Restart=always` would loop drain and restart.

## 5. Build the image catalog

Every image the platform may grant must exist as an E2B template build
recorded in the catalog before the backend can grant it:

```bash
cd /root/src/spoond
/opt/spoond/spoond images build --all \
  --manifest images/manifest.yaml --context images
/opt/spoond/spoond images list
```

This runs on the host as root, uses docker, pushes to
`$IMAGE_REGISTRY` (default `localhost:5000`) and drives a template build
per manifest entry with `baked: true`. Details and per-image behaviour:
[ci-jobs.md](ci-jobs.md).

## 6. First user and services

```bash
systemctl daemon-reload
systemctl enable --now spoond-backend spoond-sshd-gateway
curl -fsS https://127.0.0.1:8890/healthz       # {"status":"ok","orchestrator":"healthy"}

# bootstrap the first (admin) user with your SSH public key
FP=$(ssh-keygen -lf ~/.ssh/id_ed25519.pub | awk '{print $2}')
set -a; . /etc/spoond/backend.env; set +a
curl -s -X POST https://127.0.0.1:8890/api/users \
  -H "Authorization: Bearer ${CONSUMER_TOKENS%%=*}" \
  -H "X-Bootstrap-Token: $BOOTSTRAP_TOKEN" -H 'Content-Type: application/json' \
  -d "{\"name\":\"you\",\"kind\":\"person\",\"fingerprints\":[\"$FP\"]}"
```

Then the runner and the dashboard, if you want them:

```bash
systemctl enable --now spoond-runner        # needs /etc/spoond-runner.env
spoond dash hash '<password>'               # bcrypt hash for DASH_PASSWORD_HASH
# /etc/spoond/dash.env: DASH_USER, DASH_PASSWORD_HASH, METRICS_TOKEN,
#                      DASH_TLS_CERT, DASH_TLS_KEY — see operations.md
systemctl enable --now spoond-dash   # your unit wrapping `spoond dash`
```

## 7. Verify

`spoond doctor` is the single command that checks the result. It reads
the deployed backend environment, so run it the same way:

```bash
set -a; . /etc/spoond/backend.env; set +a
/opt/spoond/spoond doctor            # PASS/FAIL/WARN table, exit 0 = nothing failing
/opt/spoond/spoond doctor --json     # machine-readable
```

It checks the configuration, the orchestrator's `/health` and node info
(status, running sandboxes, free hugepages), the registry, the token
seed, the SQLite database and its migration version, the image catalog
(every `baked` image has a ready current build), the pinned artifacts by
SHA-256 (plus the Firecracker and kernel versions builds still use),
storage headroom, the lease API listener and `/healthz`, the SSH gateway
port, the LLM upstream and TLS. The full list is in
[operations.md](operations.md).

Finally, end to end:

```bash
ssh new@<this-host> -p 2222     # creates a dev sandbox and drops you in tmux
```

See also: [setup.md](setup.md) for the complete env/flag reference,
[api.md](api.md) for the HTTP API, [ctl.md](ctl.md) for the control
plane, [operations.md](operations.md) for day-2 work.
