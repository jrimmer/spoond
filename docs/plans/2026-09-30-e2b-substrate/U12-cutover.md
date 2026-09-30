# U12 — Cutover: production onto E2B, soak, remove forkd

## Purpose

Switch production spoond from forkd to E2B, verify, soak for 7 days, then
delete forkd and its tooling. Includes an exact rollback path.

## Preconditions

- U02–U11 are done.
- **The full conformance suite passes against staging**, all groups
  including R, with every budget met. The one allowed failure is
  `TestI3_DockerInDocker` (a recorded known limitation). Results are recorded in
  `docs/plans/2026-09-30-e2b-substrate/RESULTS.md`.
- **OPERATOR sent the user notice at least 7 days before the window:**
  > "On <date> spoond moves to a new sandbox engine. Persistent sandboxes
  > created before then will be deleted; copy out anything you need.
  > Images, commands, SSH and URLs keep working."

  forkd snapshots are not migrated (non-goal).
- OPERATOR schedules a maintenance window of 2 hours.

## Facts relied on

- **Production:**
  - backend `spoond-backend` with env file `/etc/forkd-backend.env` (read
    this as the path recorded in U02 step 0 everywhere in this unit);
  - gateway `spoond-sshd-gateway` with `/etc/spoond-gateway.env`;
  - runner `spoond-runner`;
  - forkd services `forkd-controller` and `forkd-netns`;
  - binary `/opt/spoond/spoond`.
- **Production DB** `/var/lib/spoond/spoond.db` exists since U05 and holds
  forkd leases.
- **Staging** runs `/opt/spoond-staging/spoond` against
  `/var/lib/spoond/staging.db`.

## Steps

### Before the window (does not disturb production)

1. OPERATOR merges `feat/e2b-substrate` into `main`.
2. On vm2:
   ```bash
   cd /root/src/spoond && git fetch && git checkout main && git pull
   go build -o /opt/spoond/spoond.next ./cmd/spoond
   ```
3. Build the production image catalog into a **new** DB file:
   ```bash
   SPOOND_DB_PATH=/var/lib/spoond/spoond-next.db E2B_GRPC_ADDR=127.0.0.1:5008 \
   E2B_PROXY_URL=http://127.0.0.1:5007 E2B_TOKEN_SEED_FILE=/etc/spoond/e2b-token-seed \
   E2B_TEAM_ID=5b0f4e3a-8c1d-4f2e-9a6b-7d3c2e1f0a95 IMAGE_REGISTRY=localhost:5000 \
   /opt/spoond/spoond.next images build --all --manifest /root/src/spoond/images/manifest.yaml --context /root/src/spoond/images
   /opt/spoond/spoond.next images list --db /var/lib/spoond/spoond-next.db   # 7 rows with build ids
   ```
4. Prepare the production env file `/etc/spoond/backend.env` (0600):
   - first create `/etc/spoond/admin-token`:
     `openssl rand -hex 32 > /etc/spoond/admin-token && chmod 600 /etc/spoond/admin-token`;
   - start from `/etc/forkd-backend.env`;
   - **remove** `FORKD_URL`, `FORKD_TOKEN`, `FORKD_HTTP_TIMEOUT_SECS`,
     `NETPOL_DNS` and `KNOWN_IMAGES`;
   - **add**:
     ```ini
     SPOOND_DB_PATH=/var/lib/spoond/spoond.db
     E2B_GRPC_ADDR=127.0.0.1:5008
     E2B_PROXY_URL=http://127.0.0.1:5007
     E2B_TOKEN_SEED_FILE=/etc/spoond/e2b-token-seed
     E2B_TEAM_ID=5b0f4e3a-8c1d-4f2e-9a6b-7d3c2e1f0a95
     E2B_TEMPLATE_STORAGE_PATH=/forkdcache/e2b/storage/templates
     IMAGE_REGISTRY=localhost:5000
     HOST_GUEST_SERVICE_ADDR=10.1.0.11
     HOST_GUEST_SERVICE_PORT=8891
     CHECKPOINT_INTERVAL_MINS=60
     OTEL_PROM_URL=http://127.0.0.1:19464/metrics
     SPOOND_BACKUP_DIR=/var/lib/spoond/backups
     POOL_SIZE=<the value recorded for S4 in RESULTS.md>
     ADMIN_TOKEN=<value of /etc/spoond/admin-token>
     ```

### In the window (exact order)

5. `systemctl stop spoond-runner`.
6. Record the rollback state:
   ```bash
   cp /opt/spoond/spoond /opt/spoond/spoond.forkd-final
   cp /etc/systemd/system/spoond-backend.service /root/spoond-backend.service.forkd-final
   cp /etc/forkd-backend.env /root/forkd-backend.env.forkd-final
   ```
7. `systemctl stop spoond-backend spoond-sshd-gateway`.
8. Switch the DB and binary:
   ```bash
   mv /var/lib/spoond/spoond.db /var/lib/spoond/spoond-forkd-final.db
   mv /var/lib/spoond/spoond-next.db /var/lib/spoond/spoond.db
   mv /opt/spoond/spoond.next /opt/spoond/spoond
   ```
9. Edit `/etc/systemd/system/spoond-backend.service`:
   - **replace** the existing `EnvironmentFile=` line with
     `EnvironmentFile=-/etc/spoond/backend.env`;
   - delete the `Environment=FORKD_URL=...` line;
   - in `After=`, replace `forkd-controller.service` with
     `e2b-orchestrator.service`, and add `Wants=e2b-orchestrator.service`.

   Edit `/etc/spoond-gateway.env`: remove any `SHELLY_BINARY_URL` and
   `LLM_GATEWAY_URL` overrides, so the new defaults (`10.1.0.11:8891`)
   apply.
10. Point the drain at production. Edit `/etc/e2b/drain.env`:
    ```ini
    SPOOND_DRAIN_URL=https://127.0.0.1:8890
    SPOOND_ADMIN_TOKEN_FILE=/etc/spoond/admin-token
    SPOOND_DRAIN_INSECURE=1
    ```
    In `e2b-orchestrator.service`, change both `/opt/spoond-staging/spoond`
    paths to `/opt/spoond/spoond`.
11. Stop staging and forkd:
    ```bash
    systemctl disable --now spoond-backend-staging spoond-sshd-gateway-staging
    systemctl disable --now spoond-watchdog.timer spoond-watchdog.service 2>/dev/null || true
    systemctl disable --now forkd-controller forkd-netns
    ```
    The watchdog (`forkd-spawn-watchdog.sh`) runs
    `systemctl start forkd-controller` when it sees a spawn outage, so it is
    disabled before forkd.
12. `systemctl daemon-reload && systemctl start spoond-backend spoond-sshd-gateway`.
    `curl -fsS https://vm2.lacy.casa:8890/healthz` returns `"status":"ok"`.
13. Raise the hugepages to 48 GiB:
    ```bash
    sed -i 's/^vm.nr_hugepages = .*/vm.nr_hugepages = 24576/' /etc/sysctl.d/90-e2b.conf
    sysctl -p /etc/sysctl.d/90-e2b.conf; grep HugePages_Total /proc/meminfo   # 24576
    ```
    If it is short, use the U04 step 2 compaction procedure. Report the
    number if it is still short.
14. Run the conformance suite against **production** from
    `/root/src/spoond` (on `main`) with `CONFORMANCE_API=https://vm2.lacy.casa:8890`,
    `CONFORMANCE_SUBSTRATE=e2b`, `CONFORMANCE_SSH=local`,
    `CONFORMANCE_BACKEND_UNIT=spoond-backend`,
    `CONFORMANCE_PROXY_URL=http://127.0.0.1:8891`,
    `CONFORMANCE_SSH_GATEWAY=127.0.0.1:2222`,
    `CONFORMANCE_GUEST_SERVICE=10.1.0.11:8891`, the production
    `PROXY_AUTH_SECRET`, and `CONFORMANCE_DESTRUCTIVE=1` (the runner is
    stopped). The conformance user (OPERATOR, before the window) is an
    **admin** in the production identity store with quota ≥ 20 and the
    `CONFORMANCE_SSH_KEY` key registered. All groups must pass, except the
    allowed I3 failure. Append the results to `RESULTS.md`.
15. `systemctl start spoond-runner`. Watch the first 3 CI jobs complete.

### Rollback (any time before step 20; exact)

```bash
systemctl stop spoond-runner spoond-backend spoond-sshd-gateway
systemctl stop e2b-orchestrator
sed -i 's/^vm.nr_hugepages = .*/vm.nr_hugepages = 0/' /etc/sysctl.d/90-e2b.conf
sysctl -w vm.nr_hugepages=0
cp /opt/spoond/spoond.forkd-final /opt/spoond/spoond
cp /root/spoond-backend.service.forkd-final /etc/systemd/system/spoond-backend.service
mv /var/lib/spoond/spoond.db /var/lib/spoond/spoond-e2b-rolledback.db
cp /var/lib/spoond/spoond-forkd-final.db /var/lib/spoond/spoond.db
systemctl daemon-reload
systemctl enable --now forkd-netns forkd-controller
systemctl enable --now spoond-watchdog.timer 2>/dev/null || true
systemctl start spoond-backend spoond-sshd-gateway spoond-runner
```

Leases created on E2B are lost on rollback. The orchestrator is stopped so
its hugepages (48 GiB after step 13) return to forkd; it stays stopped until
the OPERATOR decides otherwise.

### Soak (7 days after step 15)

16. Each day, append a dated line to `RESULTS.md` with:
    - `spoond doctor` output (all `ok`);
    - the `spoond_leases{state="lost"}` value, which must be 0 unless an
      orchestrator crash happened (record the crash);
    - the CI success rate from Forgejo for the day, compared with the
      7 days before cutover.
17. On day 7, review the GC dry-run log lines (`gc: would delete`). If every
    candidate is an unreferenced pause or checkpoint build or an old template
    build, set `GC_DELETE=1` in `/etc/spoond/backend.env` and
    `systemctl restart spoond-backend`, in a window (restarting the backend
    no longer affects leases).

### Removal (after a successful soak)

18. **In the spoond repo**, delete:
    - `forkd/`
    - `deploy/bake-elixir-release.sh`, `deploy/bake-js-base.sh`,
      `deploy/bake-py-base.sh`, `deploy/bake-scylla.sh`
    - `deploy/rebuild-dev-base.sh`
    - `deploy/rootfs-init/`
    - `deploy/forkd-patched-rollout.sh`
    - `deploy/forkd-spawn-watchdog.sh`
    - `deploy/spoond-watchdog.service` and `deploy/spoond-watchdog.timer`
    - every Go import of `github.com/jrimmer/spoond/forkd`, and every Go use
      of `FORKD_URL`, `FORKD_TOKEN`, `FORKD_HTTP_TIMEOUT_SECS` and
      `NamespaceControllerMetrics`

    Do **not** rename deployed configuration names that merely contain
    `FORKD` (`FORKD_BACKEND_URL`, `FORKD_AGENT_TOKEN`, `FORKD_IMAGE`,
    `FORKD_LLM_MODEL`, `FORKD_CTL_*`, `FORKD_GATEWAY_HOST`, `FORKD_NO_TMUX`)
    or the gateway's `forkd-*` permission keys.

    Edit:
    - `deploy/spoond-backend.service`: `EnvironmentFile=-/etc/spoond/backend.env`,
      delete `Environment=FORKD_URL=...`, `After=` names
      `e2b-orchestrator.service` instead of `forkd-controller.service`;
    - `deploy/cfos-adapter.service`: `After=` and `Requires=` name
      `spoond-backend.service` instead of `forkd-backend.service`;
    - the `Description=` strings in `deploy/*.service` that say "forkd".

    Run
    `grep -rn 'jrimmer/spoond/forkd\|FORKD_URL\|FORKD_TOKEN\|FORKD_HTTP_TIMEOUT_SECS\|NamespaceControllerMetrics' --include=*.go .`.
    It must return nothing.
19. **Docs.** Update these exact files:
    - `docs/install.md`: the E2B host setup (link `deploy/e2b/host-setup.sh`),
      Go 1.27.1, and the image build command.
    - `docs/operations.md`: the drain protocol, crash recovery states,
      `spoond doctor`, backups, GC, and "restarting the orchestrator" and
      "restarting the backend" procedures.
    - `docs/api.md`, with the new routes and fields:
      - routes: `/fork`, `/checkpoint`, `/network`, `GET /api/sandboxes/{id}`,
        `/api/snapshots`, `/api/admin/*`;
      - lease fields: `state`, `recovered_from`, `resume_build_id` and
        `build_id`;
      - the `memory_mib` rule;
      - the binary stream mode and the `resize`/`kill`/`eof` controls.
    - `docs/security.md`:
      - Firecracker runs as root without the jailer;
      - the orchestrator ports are firewalled;
      - the host-address guard (patch P4);
      - tokens.
    - `docs/ci-jobs.md`: images come from Dockerfiles, and are built with
      `spoond images build`.
    - **New** `docs/substrate.md`: one page covering the architecture
      (copy `01-architecture.md`'s diagram), what E2B is and how we use it,
      the crash trade-off in plain words (D4), and a link to the upgrade
      runbook (U13).

    **Commit:** `chore: remove forkd; document the E2B substrate`.
20. **On vm2**, after 30 days with no rollback:
    1. Delete staging-only builds. They are not in the production catalog,
       so production GC never reclaims them. Keep every build the
       production catalog knows or references (its `builds`, their parents,
       and every `build_refs.ref_build_id`, since a production template
       build may reuse a cached staging layer):
       ```bash
       python3 - <<'PY'
       import sqlite3, shutil, os
       prod = sqlite3.connect('/var/lib/spoond/spoond.db')
       keep = set(r[0] for r in prod.execute("SELECT build_id FROM builds"))
       keep |= set(r[0] for r in prod.execute("SELECT parent_build_id FROM builds WHERE parent_build_id <> ''"))
       keep |= set(r[0] for r in prod.execute("SELECT ref_build_id FROM build_refs"))
       stg = sqlite3.connect('/var/lib/spoond/staging.db')
       root = '/forkdcache/e2b/storage/templates'
       for (b,) in stg.execute("SELECT build_id FROM builds WHERE state <> 'deleted'"):
           if b not in keep and os.path.isdir(os.path.join(root, b)):
               print('delete', b)
               shutil.rmtree(os.path.join(root, b))
       PY
       ```
       (`TemplateBuildDelete` is exactly an `rm -rf` of that directory,
       A3 C4.)
    2. Remove forkd and the staging state. The staging unit files,
       `/etc/spoond-staging/` and `/var/lib/spoond/staging-users.json` stay
       (units disabled) for U13. The staging binary is
       rebuilt in U13.
       ```bash
       rm -rf /forkdcache/forkd-data /var/cache/forkd
       rm -f /usr/local/bin/forkd /usr/local/bin/forkd-controller /usr/local/bin/forkd*.bak-*
       rm -f /usr/local/bin/forkd-spawn-watchdog.sh
       rm -f /etc/systemd/system/forkd-controller.service /etc/systemd/system/forkd-netns.service
       rm -f /etc/systemd/system/spoond-watchdog.service /etc/systemd/system/spoond-watchdog.timer
       rm -rf /opt/spoond-staging /usr/local/go1.25.1.bak
       rm -f /var/lib/spoond/staging.db* /opt/spoond/spoond.forkd-final
       systemctl daemon-reload
       ```

## Done when

- Production passes the full conformance suite on E2B.
- The 7-day soak meets the Definition of Done in `00-README.md`.
- forkd code, tooling and data are removed.
- The docs are updated.
