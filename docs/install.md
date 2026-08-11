# Installing spoond

Two install paths:

1. **forkd + spoond (full stack)** — forkd-controller provides the microVM
   orchestration (Firecracker, warm pool, guest agent); spoond is the lease
   API, SSH gateway, and agent endpoints on top of it.
2. **spoond only** — you already run forkd-controller (or want the lease API
   against a controller on another host); only the spoond services are
   installed here.

Both paths assume a Linux host (Debian/Ubuntu-style) with a Go toolchain
(1.22+). The install script is written so an agent can run it line-by-line
and verify with `spoond doctor`.

## Prerequisites (both paths)

```bash
# Go toolchain (1.22+)
apt-get install -y golang-go   # or install from https://go.dev/dl/

# clone the repo
git clone https://github.com/jrimmer/spoond && cd spoond
```

## Path 1: forkd + spoond (full stack)

```bash
# 1. forkd-controller — separate upstream project.
#    Follow its own install docs (github.com/jrimmer/forkd) and leave it
#    running with its API on http://127.0.0.1:8889.
# 2. spoond on top:
./deploy/install-spoond.sh --with-forkd
```

What the script does (idempotent, `.prev` backups):

1. Builds `spoond` (one binary, all services).
2. Installs to `$PREFIX/spoond/spoond` (default `/opt`).
3. Creates `/etc/spoond-backend.env` (consumer tokens — **edit before
   starting**), `/etc/spoond-gateway/keys/` (SSH key allowlist).
4. Installs systemd units: `spoond-backend`, `spoond-sshd-gateway`,
   `spoond-watchdog.{service,timer}`.
5. Starts services, then runs `spoond doctor` to verify every dependency
   (forkd-controller, lease API, gateway port, LLM upstream, warm pool, TLS,
   disk).

**First-run steps after the script (v1.1 — identity store mode):**

```bash
# 1. Set real consumer tokens + optional TLS, and enable the identity store:
sudoedit /etc/spoond-backend.env
#   CONSUMER_TOKENS=token=consumer,...
#   USERS_FILE=/etc/spoond-backend-users.json   ← multi-user tenancy (v1.1)
#   BOOTSTRAP_TOKEN=$(openssl rand -hex 24)      ← gate first-user bootstrap
#   GATEWAY_TOKEN=<same token the gateway uses>   ← SSH user impersonation
sudo systemctl restart spoond-backend

# 2. Bootstrap the first (admin) user with your SSH public key.
#    With BOOTSTRAP_TOKEN set this is a direct API call:
FP=$(ssh-keygen -lf ~/.ssh/id_ed25519.pub | awk '{print $2}')
curl -s -X POST https://127.0.0.1:8890/api/users \
  -H "Authorization: Bearer $CONSUMER_TOKEN" -H "X-Bootstrap-Token: $BOOTSTRAP_TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"you\",\"kind\":\"person\",\"fingerprints\":[\"$FP\"]}"

# 3. Verify:
sudo /opt/spoond/spoond doctor
ssh new@<this-host> -p 2222     # creates a sandbox, drops you in
```

> **Legacy single-user mode** (no `USERS_FILE`): instead of step 2, drop
> your public key into the gateway allowlist and restart:
> `echo "ssh-ed25519 AAAA… you@laptop" | sudo tee /etc/spoond-gateway/keys/you.pub &&
> sudo systemctl restart spoond-sshd-gateway`

## Path 2: spoond only

You have forkd-controller running elsewhere (e.g. another host, or you only
need the lease API / gateway against a remote controller).

```bash
# point FORKD_URL at your controller before starting
export FORKD_URL=http://<controller-host>:8889
./deploy/install-spoond.sh      # no --with-forkd
```

The script leaves `FORKD_URL` at `http://127.0.0.1:8889` in
`/etc/spoond-backend.env` — edit it to your controller before starting
services, or the `doctor` forkd checks will fail (as they should).

## Verification

`spoond doctor` is the single command that checks everything:

```bash
sudo /opt/spoond/spoond doctor            # human-readable table, exit 0 = all pass
sudo /opt/spoond/spoond doctor --json     # machine-readable
```

Checks: config env, forkd-controller connectivity + sandbox list, lease API
listener + `/healthz` (TLS-aware, uses the cert's own SAN), SSH gateway port,
LLM gateway upstream (key present + `/models` probe), warm pool size, TLS
material, disk fill.

See also: [docs/setup.md](setup.md) for the full env/flag reference,
[docs/api.md](api.md) for the HTTP API, [docs/ctl.md](ctl.md) for the control
plane.
