# spoond deployment

A new host is installed with `spoond install`, or with Docker Compose from
[docker/](docker/compose.yaml); both are in
[docs/install.md](../docs/install.md). This file is the reference for the
shipped units and for updating an installed host by hand.

Four systemd units run on the host, counting the E2B substrate unit they
depend on:

0. **e2b-orchestrator** — the sandbox substrate (Firecracker microVM
   lifecycle, snapshots, sandbox proxy). Deployed from the
   `example.com/e2b-runtime` fork; see [docs/install.md](../docs/install.md)
   and [docs/substrate.md](../docs/substrate.md).
1. **spoond-backend** — the lease API (`:8890`), warm pool, SQLite state
2. **spoond-sshd-gateway** — the SSH gateway (`:2222`) + ctl plane
3. **spoond-runner** — the Forgejo Actions runner (adaptive pool)

> Full user docs: [docs/install.md](../docs/install.md),
> [docs/api.md](../docs/api.md), [docs/ctl.md](../docs/ctl.md),
> [docs/usage.md](../docs/usage.md),
> [docs/operations.md](../docs/operations.md). This file is the
> deploy-specific quick reference. Upgrading the production host to 2.7
> (generic defaults): see
> [PRODUCTION-ENV-2.7.md](PRODUCTION-ENV-2.7.md) for the exact values
> vm2 must set before the new binaries run.

## 0. e2b-orchestrator (substrate)

Installed by `spoond install runtime`: the `e2b-orchestrator.service`
unit with `deploy/e2b/orchestrator.env`, the token seed
(`/etc/spoond/e2b-token-seed`), the `flags.json` override and the
`e2b-guard` host firewall. The OpenTelemetry collector
(`deploy/e2b/otelcol-config.yaml`) is optional and installed by hand.

```bash
install -m 644 deploy/e2b/e2b-orchestrator.service /etc/systemd/system/
install -m 600 deploy/e2b/orchestrator.env /etc/e2b/orchestrator.env
systemctl daemon-reload && systemctl enable --now e2b-orchestrator
```

The unit carries the drain hooks (U10): `ExecStop` pauses every running
sandbox before the SIGTERM lands, `ExecStartPost` resumes them once the
orchestrator is up. Deliberate restarts therefore lose nothing — see the
[drain protocol](../docs/operations.md).

## 1. spoond-backend (lease API)

### Build and deploy

```bash
go build -o /opt/spoond/spoond ./cmd/spoond
install -m 644 deploy/spoond-backend.service /etc/systemd/system/
```

On host, create `/etc/spoond/backend.env` (mode 0600). The full variable
reference is in [docs/install.md](../docs/install.md#spoond-backend-lease-api). Example files for every unit live
beside the units: `deploy/spoond-backend.env.example`,
`deploy/spoond-gateway.env.example`, `deploy/spoond-runner.env.example`
and `deploy/e2b/spoond-netwatch.env.example` — their headers name the
installed target paths (`/etc/spoond/backend.env`,
`/etc/spoond-gateway.env`, `/etc/spoond/runner.env`). An optional
`/etc/spoond/llm.env` (0600) may hold the per-lease LLM gateway's key
pair (`LLM_UPSTREAM_URL`, `LLM_UPSTREAM_KEY`, `LLM_DEFAULT_MODEL`) when
an operator keeps it apart from the backend file; the backend unit
reads llm.env then backend.env, and `backend.env` wins. The minimum:

```bash
cat > /etc/spoond/backend.env <<'EOF'
CONSUMER_TOKENS=<token>=<consumer>,<token2>=<consumer2>
E2B_TOKEN_SEED_FILE=/etc/spoond/e2b-token-seed
HOST_GUEST_SERVICE_ADDR=10.0.0.11
SPOOND_GUEST_DNS_ADDR=10.0.0.2,10.0.0.3
SPOOND_PROXY_HOST_SUFFIX=.sandbox.example.com
TLS_CERT=/etc/spoond/tls/fullchain.pem
TLS_KEY=/etc/spoond/tls/privkey.pem
EOF
chmod 600 /etc/spoond/backend.env
```

- `CONSUMER_TOKENS` — comma-separated `token=consumer` pairs; consumers
  authenticate with these bearer tokens
- `E2B_TOKEN_SEED_FILE` — the seed file shared with the orchestrator
  (envd/traffic tokens derive from it)
- `HOST_GUEST_SERVICE_ADDR` — the host address guests use to reach the
  proxy/LLM gateway and the lease API
- `SPOOND_GUEST_DNS_ADDR` — the guest resolver(s), comma-separated;
  each is granted port 53 and baked into the image for
  `spoond-guest-init`
- `TLS_CERT`/`TLS_KEY` — serve HTTPS on :8890. On host this uses the
  Let's Encrypt cert for `sandbox.example.com` (see TLS below)

Then:

```bash
systemctl daemon-reload
systemctl enable --now spoond-backend
systemctl status spoond-backend
```

### Verify

```bash
spoond doctor
curl -s -H "Authorization: Bearer <token>" https://sandbox.example.com:8890/api/images
```

## 2. spoond-sshd-gateway (SSH gateway + ctl plane)

### Build and deploy

```bash
go build -o /opt/spoond/spoond ./cmd/spoond
install -m 644 deploy/spoond-sshd-gateway.service /etc/systemd/system/
```

The unit runs with `--client-keys /etc/spoond-gateway/keys` — a
**directory**; each `*.pub` file is a user (legacy allowlist mode). With
`USERS_FILE` set on the backend, the identity store is the single source
of truth and the directory is ignored. Add a user = drop their `.pub`
into the dir + restart, or `ssh-key add` via the ctl plane.

The backend credential lives in `/etc/spoond-gateway.env` (mode 0600) as
`SPOOND_GATEWAY_TOKEN` — never in `ExecStart`, so `/proc/<pid>/cmdline`
cannot expose it (it is admin-equivalent: the gateway impersonates the
authenticated SSH user via `X-Spoond-User-Id`). This is the one env
file left outside `/etc/spoond/`, matching the unit's single
`EnvironmentFile=` line.

### Verify

```bash
ssh ctl@sandbox.example.com "ls"
ssh new@sandbox.example.com    # auto-create + attach
```

## 3. spoond-runner (Forgejo Actions)

Runs an **adaptive pool** of concurrent runner workers: one process
registers N runners with Forgejo, scales up when all are busy, and scales
back down to a floor when load subsides.

### Build and deploy

```bash
go build -o /opt/spoond/spoond ./cmd/spoond
install -m 644 deploy/spoond-runner.service /etc/systemd/system/
```

On host, create `/etc/spoond/runner.env` (mode 0600):

```bash
cat > /etc/spoond/runner.env <<'EOF'
FORGEJO_URL=https://git.example.com
RUNNER_TOKEN=<registration token>
RUNNER_NAME=spoond-runner
RUNNER_LABELS=ubuntu-latest,go,golang,elixir,elixir-base,llm-review,elixir-release,release
LEASE_URL=https://sandbox.example.com:8890
LEASE_TOKEN=<consumer token>
IMAGE_MAP=ubuntu-latest=py-base,go=go-base,golang=go-base,elixir=elixir-base,elixir-base=elixir-base,llm-review=llm-review,dev=dev-base,elixir-release=elixir-release,release=elixir-release
DEFAULT_IMAGE=py-base
RUNNER_FLOOR=3
RUNNER_MAX=12
RUNNER_SCALE_STEP=3
SCALE_UP_DELAY=10s
SCALE_DOWN_DELAY=60s
JOB_RECORD_DIR=/var/lib/spoond/jobs
EOF
chmod 600 /etc/spoond/runner.env
```

Pool tuning (`RUNNER_FLOOR`, `RUNNER_MAX`, `RUNNER_SCALE_STEP`,
`SCALE_UP_DELAY`, `SCALE_DOWN_DELAY`, `JOB_RECORD_DIR`) is documented in
[docs/install.md](../docs/install.md#spoond-runner-forgejo-actions-optional) and
[docs/ci-jobs.md](../docs/ci-jobs.md).

Then:

```bash
systemctl daemon-reload
systemctl enable --now spoond-runner
systemctl status spoond-runner
```

### Verify

```bash
# pool starts at floor
journalctl -u spoond-runner | grep 'pool: spawned'

# scale-up: fire N concurrent jobs; pool grows by RUNNER_SCALE_STEP
# scale-down: after jobs finish, pool returns to RUNNER_FLOOR
journalctl -u spoond-runner | grep -E 'spawned worker|stopped worker'
```

## TLS

The backend serves TLS on `:8890` using the host's Let's Encrypt cert for
`sandbox.example.com`. the host's `/etc/hosts` pins that hostname to 10.0.0.11
so the runner reaches the backend directly (not via Caddy). The proxy /
LLM gateway listener (`:8891`) stays plain HTTP behind Caddy, which
fronts the `*.sandbox.example.com` wildcard; see
`deploy/caddy-sandbox-forwardauth.conf` for the forward-auth block.
