# Installing spoond

spoond's sandbox substrate is **E2B's node runtime** — the orchestrator,
the template manager and the `envd` guest agent — run standalone from our
patch-queue fork of `github.com/e2b-dev/runtime` (`lacy.casa/e2b-runtime`).
spoond is the control plane on top: identity, quotas, leases, the SSH
gateway, the proxy, the image catalog and SQLite state. There is no
controller package to install from this repo.

## Prerequisites

- A Linux host (Debian/Ubuntu-style, x86_64) with KVM and ZFS.
- **Go 1.27.1** (`apt-get install -y golang-go`, or install from
  <https://go.dev/dl/>), used both to build spoond and the orchestrator and
  `envd` from the fork.
- Docker, for the local image registry and the image builds.
- A clone of this repo and of the E2B fork:
  ```bash
  git clone https://code.lacy.casa/lacy.casa/spoond.git
  git clone https://code.lacy.casa/lacy.casa/e2b-runtime.git
  ```

## 1. Host setup

`deploy/e2b/host-setup.sh` brings up the E2B side as root: packages and
kernel modules, sysctls and hugepages (with a `MemAvailable` guard), the
MSS clamp, the ZFS dataset and directories under `/forkdcache/e2b/`, the
pinned Firecracker, kernel and busybox artifacts (SHA-256 verified), the
`envd` and orchestrator binaries from the fork, the `e2b-guard` nftables
host firewall, and the local Docker registry on `127.0.0.1:5000`.

```bash
./deploy/e2b/host-setup.sh
```

Then apply the remaining steps of
[U04](plans/2026-09-30-e2b-substrate/U04-host-bringup.md): the
`e2b-orchestrator.service` unit with `deploy/e2b/orchestrator.env`, the
token seed (`/etc/spoond/e2b-token-seed`, 64 hex chars, mode 0600), the
`flags.json` override file and the OpenTelemetry collector
(`deploy/e2b/otelcol-config.yaml`).

## 2. Build and install spoond

```bash
go build -o /opt/spoond/spoond ./cmd/spoond
install -m 644 deploy/spoond-backend.service deploy/spoond-sshd-gateway.service \
  deploy/spoond-runner.service /etc/systemd/system/
systemctl daemon-reload
```

Create `/etc/spoond/backend.env` (mode 0600). The variables the backend
reads are:

```ini
CONSUMER_TOKENS=<token>=<owner>,<token2>=<owner2>   # required
SPOOND_DB_PATH=/var/lib/spoond/spoond.db
E2B_GRPC_ADDR=127.0.0.1:5008
E2B_PROXY_URL=http://127.0.0.1:5007
E2B_TOKEN_SEED_FILE=/etc/spoond/e2b-token-seed
E2B_TEAM_ID=5b0f4e3a-8c1d-4f2e-9a6b-7d3c2e1f0a95
E2B_TEMPLATE_STORAGE_PATH=/forkdcache/e2b/storage/templates
IMAGE_REGISTRY=localhost:5000
HOST_GUEST_SERVICE_ADDR=<host IP guests use for spoond services>
HOST_GUEST_SERVICE_PORT=8891
TLS_CERT=/etc/spoond/tls/fullchain.pem
TLS_KEY=/etc/spoond/tls/privkey.pem
```

`USERS_FILE` enables the multi-user identity store,
`BOOTSTRAP_TOKEN` gates first-user bootstrap, `GATEWAY_TOKEN` is the SSH
gateway's backend credential (in `/etc/spoond-gateway.env`, mode 0600),
`ADMIN_TOKEN` enables the `/api/admin/*` routes, `METRICS_TOKEN` is the
scrape-only metrics credential, `OTEL_PROM_URL` appends the orchestrator's
collector output to `/metrics`, `GC_DELETE` allows the catalog GC to
delete builds, `SPOOND_BACKUP_DIR` is the daily `VACUUM INTO` target, and
`CHECKPOINT_INTERVAL_MINS` sets the periodic checkpoint of persistent
leases. `docs/setup.md` has the full reference.

## 3. Build the images

Images are Dockerfiles under `images/`, described by
`images/manifest.yaml`. `spoond images build` pushes each one to the local
registry and turns it into an E2B template (a booted, snapshotted
microVM), so every later create is a warm restore:

```bash
spoond images build --all --manifest images/manifest.yaml --context images
spoond images list --db /var/lib/spoond/spoond.db   # one row per image
```

## 4. Verify

```bash
spoond doctor            # human-readable table, exit 0 = all pass
spoond doctor --json     # machine-readable
```

The doctor exercises the orchestrator (`/health`, `NodeInfo`), the image
registry, the lease API listener and `/healthz`, the SSH gateway port, the
token seed, the SQLite catalog with its baked images, the pinned E2B
artifacts, the build storage, TLS material, the LLM upstream and disk —
reading the same environment the backend uses.

## First user

With `USERS_FILE` set, bootstrap the first (admin) user with an SSH public
key:

```bash
FP=$(ssh-keygen -lf ~/.ssh/id_ed25519.pub | awk '{print $2}')
curl -s -X POST https://127.0.0.1:8890/api/users \
  -H "Authorization: Bearer $CONSUMER_TOKEN" -H "X-Bootstrap-Token: $BOOTSTRAP_TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"you\",\"kind\":\"person\",\"fingerprints\":[\"$FP\"]}"

systemctl enable --now spoond-backend spoond-sshd-gateway spoond-runner
ssh new@<this-host> -p 2222     # creates a sandbox, drops you in
```

> **Legacy single-user mode** (no `USERS_FILE`): instead of the bootstrap
> call, drop a public key into the gateway allowlist and restart:
> `echo "ssh-ed25519 AAAA… you@laptop" | sudo tee /etc/spoond-gateway/keys/you.pub &&
> sudo systemctl restart spoond-sshd-gateway`

See also: [docs/setup.md](setup.md) for the full env/flag reference,
[docs/api.md](api.md) for the HTTP API, [docs/ctl.md](ctl.md) for the
control plane, [docs/operations.md](operations.md) for running it, and
[docs/runbooks/e2b-upgrade.md](runbooks/e2b-upgrade.md) for moving the
fork to a newer upstream.
