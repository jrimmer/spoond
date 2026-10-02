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

### Where a pool's disk goes

Each sandbox writes to its own copy of its tag's rootfs, so a pooled sandbox's
accumulated writes — cargo target, pnpm store, `mix _build` — are charged to
that sandbox and released when it dies. This is the opposite of the behaviour
that used to fill the host: writes went into the shared rootfs, so draining the
pool freed nothing and the growth was unbounded.

Two numbers follow from it:

- **Draining the pool now reclaims disk.** `forkd-disk-guard.sh` restarting
  `spoond-runner` releases the pool's copies, which is what makes that guard
  effective rather than cosmetic.
- **A spawn costs one rootfs copy.** Free where the filesystem clones
  (`reflink`) — ZFS 2.2+, XFS, btrfs — and a full copy elsewhere, so on such a
  host `POOL_SIZE` is a storage decision, not just a latency one. Check with
  `zfs list` / `df` before raising it.

Stranded copies are not expected: a killed sandbox removes its own, a restart's
sweep removes those belonging to sandboxes that died with the controller, and
the watchdog removes directories with no live Firecracker. If disk does not come
back after a drain, compare `ls /tmp/forkd-daemon-*/*.ext4` against
`pgrep -c firecracker` before assuming a leak.

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
| `pooled sb-… is stale (controller forgot it)` | controller restart pruned pool | backend restart (above) — see below, the pool does **not** recover on its own |
| `sandbox is suspended; resume it first` | lease suspended, op needs live VM | `resume <id>` first |
| `failed to grant sandbox` 500 | pool refill window | retry after ~30s |
| `spawned sandbox failed the integrity probe` | toolchain corrupt in that image generation | re-bake the image; the bad sandbox is already killed |
| `pooled sb-… failed the integrity probe` | a pooled sandbox predates a fix | expected once per bad sandbox; the pool refills clean |
| `warmPool: spawn <tag>:` repeating forever | tag's snapshot can't restore (e.g. vmstate from an older Firecracker) | re-bake that tag; the backend retries it every 5s until then |
| exec `proxy.golang.org` blocked | `lan` policy has no internet | use `network_policy: internet` or allowlist |

### A controller restart leaves the pool cold until it is drained

`warmPool` sizes the pool with `len(s.store.pool[image])` and returns early at
`poolSize`, but the pool is in memory only and the controller has no
client-liveness concept. After a controller restart the backend still holds
the old ids, so the count reads "full" while every entry is dead: no refill
happens, and the pool stays cold indefinitely. Each grant pops one phantom
(`is stale … dropping`) and then cold-spawns, so grants keep working and the
symptom is invisible except in the journal.

Observed 2026-09-12: three phantom `elixir-release` ids, zero live sandboxes,
and a grant that dropped all three before spawning. `systemctl restart
spoond-backend` clears it, which is why the row above says to restart the
backend rather than the controller.

Do not "fix" this by making the refill more aggressive without checking the
disk budget first: `POOL_SIZE × images` sandboxes at ~12 GiB per child is
larger than the pool's free space on this host, so a refill that spawns
without accounting for phantoms can fill the pool.

A tag that cannot restore is retried on every refill tick — observed at
**1440 failed spawn attempts in 20 minutes** (6 un-restorable tags × a 5s
tick), each one a doomed restore. A per-image backoff after a failed spawn
would cut that to a handful without changing behaviour for healthy tags.

## The integrity probe

`SANDBOX_PROBE` (default on) runs a behaviour check inside each sandbox before
it is pooled or leased — `uname -s` must report Linux, `tr` must translate —
and kills the sandbox on failure. It catches a corrupted toolchain that is
otherwise invisible: the sandbox pings fine and `uname --version` exits 0
while returning another program's output. `docs/ci-jobs.md` has the observed
signatures.

Two operational consequences. A bad image generation now surfaces as probe
failures in the backend journal instead of 48-second build failures, so a run
of them means the image needs re-baking rather than the sandbox layer needing
attention. And `SANDBOX_PROBE=0` is the escape hatch if the probe itself
misbehaves — grants then hand out unverified sandboxes, which is what every
deployment did before it existed.

The probe is a detection net, not a fix: it stops a corrupt sandbox from
costing a debugging cycle, and says nothing about why the image was corrupt.

Failed jobs are recorded as JSON under `/var/lib/spoond/jobs/`
(`JOB_RECORD_DIR`), naming the failing step, its exit code and its output
tail. That is the first place to look for a red build — the runner's Forgejo
logs are not readable back out.

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
