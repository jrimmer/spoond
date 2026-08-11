# Operations

Runbook for operating a spoond deployment: warm pool, watchdog,
diagnostics, and common failure modes.

## Component health

| Check | Command |
|---|---|
| Backend | `systemctl is-active forkd-backend` |
| Gateway | `systemctl is-active forkd-sshd-gateway` |
| Controller (forkd) | `systemctl is-active forkd-controller` |
| Warm pool | `pgrep -c firecracker` (expect `POOL_SIZE × images`) |
| Lease API | `curl -s https://127.0.0.1:8890/healthz` |
| Metrics | `curl -s -H "Authorization: Bearer $ADMIN_TOKEN" https://127.0.0.1:8890/metrics` (admin-only in identity-store mode) |
| Identity store | `test -f /etc/spoond-backend-users.json && stat -c '%a' /etc/spoond-backend-users.json` (expect `600`) |

## Warm pool

`POOL_SIZE` pre-forks sandboxes per image so grants are served in
milliseconds. After a backend restart the pool refills over ~90s
(18 firecrackers = 6 images × 3). During the window, cold spawns are
slower and can transiently fail with `failed to grant sandbox` 500s —
those are artifacts, not product bugs.

## Spawn-outage watchdog

`forkd-spawn-watchdog` (timer, every 5 min) auto-recovers from the
known spawn outage: it captures diagnostics to
`/var/log/forkd/watchdog/`, kills firecrackers, removes stale daemon
dirs, restarts the controller, and lets the backend reconcile.

**Known loop (stale-pool-map wedge):** if the controller is SIGKILLed
(by the watchdog's own recovery) while the backend's in-memory pool map
still holds the dead sandbox ids, `refillPool` silently skips and the
pool stays empty; the watchdog re-triggers every 5 min. Discriminator:
`pgrep -c firecracker` == 0, backend active, controller `/v1/sandboxes`
== 0, no warmPool error lines in the backend journal.

**Fix:**

```bash
systemctl stop forkd-watchdog.timer
systemctl restart forkd-backend          # clears pool map + stale leases
# ~90s later: expect 18 firecrackers
pgrep -c firecracker
# VERIFY the previously-failing spawn path BEFORE re-enabling:
curl -s -X POST https://127.0.0.1:8890/api/sandboxes \
  -H "Authorization: Bearer $TOKEN" -d '{"image":"dev-base","persistent":true,"ttl":300}'
#   → expect an id; exec uname -m in it; delete it; then:
systemctl start forkd-watchdog.timer
```

Do NOT run the watchdog manually mid-recovery — its 15-min error
lookback re-triggers on errors already fixed and it will SIGKILL the
controller you just rebuilt.

## Backend restart = lease loss

The backend keeps leases in memory only. Restarting it drops all leases
and the pool map (workspace snapshots persist in the controller as
`Stale`). Acceptable when leases are disposable; avoid during active
work.

## Common failures

| Symptom | Cause | Fix |
|---|---|---|
| `connection refused` on :2222 | gateway down/restarting | `systemctl restart forkd-sshd-gateway` |
| `Text file busy` on deploy | overwrote a running binary | deploy to `.new` then `mv` (see deploy scripts) |
| `child-1.sock never appeared within 10s` | controller busy / cold spawn | wait for pool refill; check watchdog tarballs |
| `pooled sb-… is stale (controller forgot it)` | controller restart pruned pool | backend restart (above) |
| `sandbox is suspended; resume it first` | lease suspended, op needs live VM | `resume <id>` first |
| `failed to grant sandbox` 500 | pool refill window | retry after ~30s |
| exec `proxy.golang.org` blocked | `lan` policy has no internet | use `network_policy: internet` or allowlist |

## Diagnostics first

Before any recovery: capture state (the watchdog tarball pattern):

```bash
# controller + backend journals, firecracker table, daemon dirs, netns
journalctl -u forkd-controller --since "30 min ago" --no-pager > /tmp/ctl.log
journalctl -u forkd-backend    --since "30 min ago" --no-pager > /tmp/be.log
ps -eo pid,etimes,comm,args | grep '[f]irecracker' > /tmp/fc.txt
ls -la /var/run/netns/ > /tmp/netns.txt
curl -s http://127.0.0.1:8889/v1/sandboxes > /tmp/controller-sandboxes.json
```

## Backups

The backend has no persistent state to back up (leases are in-memory;
workspaces live in the controller). Backup the controller's workspace
snapshots, the gateway key dir, and — in identity-store mode — the
**user store** (it holds users, key fingerprints, quotas; losing it
loses all SSH identities):

```bash
# controller workspace dir + gateway keys + user store
tar czf /backup/forkd-$(date +%F).tar.gz \
  /var/lib/forkd /etc/forkd-gateway /etc/spoond-backend-users.json
```

## Users & identity (v1.1)

- **Revoking access** = `DELETE /api/users/{id}` (or
  `ssh-key rm <user-id>`); with the identity store present the gateway
  treats it as authoritative, so removal is immediate — no key-dir
  cleanup needed.
- **Quotas** are per-user (`max_leases`/`max_ttl` via
  `POST /api/users/{id}/quota`); over-cap creates return `429`. A user
  with `max_leases: 0` is unlimited.
- **Salt rotation / token hashes**: token and LLM-key hashes are
  HMAC-SHA256 with a per-store salt (sidecar `<users-file>.salt`); back
  up the salt alongside the store or existing hashes become
  unverifiable on restore.
- **Forward-auth proxy** (`PROXY_AUTH_MODE=forward-auth`): the
  `PROXY_AUTH_SECRET` is shared with the IdP/Caddy; ensure Caddy strips
  inbound `X-Proxy-Auth`/`Remote-User` headers so guests can't spoof
  them, and keep `PROXY_AUTH_TRUSTED_PEERS` to the proxy's own CIDR.
