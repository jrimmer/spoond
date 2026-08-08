# forkd-service deployment

Two systemd services run on vm2 (10.1.0.11):

1. **forkd-backend** — the lease API (`:8890`) with the warm pool
2. **forkd-runner** — the Forgejo Actions runner (adaptive pool)

## 1. forkd-backend (lease API)

### Build

```bash
go build -o forkd-backend ./cmd/forkd-backend
```

### Deploy

```bash
scp forkd-backend root@10.1.0.11:/opt/forkd-backend/
scp deploy/forkd-backend.service root@10.1.0.11:/etc/systemd/system/
```

On vm2, create `/etc/forkd-backend.env`:

```bash
cat > /etc/forkd-backend.env <<'EOF'
CONSUMER_TOKENS=<token>=<consumer>,<token2>=<consumer2>
POOL_SIZE=3
TLS_CERT=/etc/forkd-backend/tls/fullchain.pem
TLS_KEY=/etc/forkd-backend/tls/privkey.pem
EOF
chmod 600 /etc/forkd-backend.env
```

- `CONSUMER_TOKENS` — comma-separated `token=consumer` pairs; consumers
  authenticate with these bearer tokens
- `POOL_SIZE` — pre-fork that many sandboxes per image so grants are served
  from the warm pool (milliseconds) instead of cold-spawning. 0 disables
- `TLS_CERT`/`TLS_KEY` — serve HTTPS. On vm2 this uses the Let's Encrypt
  cert for `vm2.lacy.casa` (see TLS below)

Then:

```bash
systemctl daemon-reload
systemctl enable --now forkd-backend
systemctl status forkd-backend
```

### Verify

```bash
curl -s -H "Authorization: Bearer <token>" https://vm2.lacy.casa:8890/api/images
```

## 2. forkd-runner (Forgejo Actions)

Runs an **adaptive pool** of concurrent runner workers: one process
registers N runners with Forgejo, scales up when all are busy, and scales
back down to a floor when load subsides.

### Build

```bash
go build -o forkd-runner ./cmd/forkd-runner
```

### Deploy

```bash
scp forkd-runner root@10.1.0.11:/opt/forkd-runner/
scp deploy/forkd-runner.service root@10.1.0.11:/etc/systemd/system/
```

On vm2, create `/etc/forkd-runner.env`:

```bash
cat > /etc/forkd-runner.env <<'EOF'
FORGEJO_URL=https://code.lacy.casa
RUNNER_TOKEN=<registration token>
RUNNER_NAME=forkd-runner
RUNNER_LABELS=forkd
LEASE_URL=https://vm2.lacy.casa:8890
LEASE_TOKEN=<consumer token>
DEFAULT_IMAGE=py-base
RUNNER_FLOOR=3
RUNNER_MAX=12
RUNNER_SCALE_STEP=3
SCALE_UP_DELAY=10s
SCALE_DOWN_DELAY=60s
EOF
chmod 600 /etc/forkd-runner.env
```

Pool tuning:
- `RUNNER_FLOOR` — minimum registered runners always kept (default 3)
- `RUNNER_MAX` — maximum registered runners (default 12)
- `RUNNER_SCALE_STEP` — runners added/removed per scale event (default 3)
- `SCALE_UP_DELAY` — how long all workers must be busy before scaling up
- `SCALE_DOWN_DELAY` — how long a worker must be idle before scaling down

Then:

```bash
systemctl daemon-reload
systemctl enable --now forkd-runner
systemctl status forkd-runner
```

### Verify

```bash
# pool starts at floor
journalctl -u forkd-runner | grep 'pool: spawned'

# scale-up: fire N concurrent jobs; pool grows by SCALE_STEP
# scale-down: after jobs finish, pool returns to FLOOR
journalctl -u forkd-runner | grep -E 'spawned worker|stopped worker'
```

## TLS

The backend serves TLS on `:8890` using vm2's Let's Encrypt cert for
`vm2.lacy.casa`. vm2's `/etc/hosts` pins that hostname to 10.1.0.11 so the
runner reaches the backend directly (not via Caddy).
