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
- **No user notice.** The human is the only user of vm2. Existing forkd
  leases, including persistent sandboxes, are deleted at cutover, which is
  accepted. forkd snapshots are not migrated (non-goal).
- **The cutover runs as an Autonomous window** (`00-README.md` §Autonomous
  window protocol), under the human's standing authorization. No human
  schedules it.
- `/etc/spoond/conformance.env` exists (README §Production conformance
  credentials).

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

1. The orchestrator merges `feat/e2b-substrate` into `main` with `--no-ff`,
   and pushes (README rule 10). This requires the preconditions above to hold.
2. On vm2 (Ops runner):
   ```bash
   export PATH=/usr/local/go/bin:$PATH
   cd /root/src/spoond && git fetch && git checkout main && git pull --ff-only
   go build -o /opt/spoond/spoond.next ./cmd/spoond
   /opt/spoond/spoond.next help >/dev/null 2>&1; test $? -le 2
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

### Cutover (Autonomous window, `00-README.md`; steps 5–15 in order)

- **Rollback artifacts** (created in step 6, then checked by protocol step 1
  before step 7 runs):
  - `/opt/spoond/spoond.forkd-final` (runs `help`);
  - `/root/spoond-backend.service.forkd-final`;
  - `/root/forkd-backend.env.forkd-final`;
  - `/root/spoond-gateway.env.forkd-final`;
  - `/root/drain.env.pre-cutover`;
  - `/root/e2b-orchestrator.service.pre-cutover`.

  After step 8, `/var/lib/spoond/spoond-forkd-final.db` must also pass the
  protocol's integrity check.
- **Rollback commands:** §Rollback below.
- **On any failure** in steps 7–14: run §Rollback, run `window_smoke`, then
  `BLOCKED` (protocol step 4).

5. `systemctl stop spoond-runner` (protocol step 3), after `window_idle`
   returns 0 (protocol step 2).
6. Record the rollback state, then run the protocol's rollback-ready check
   on the listed artifacts:
   ```bash
   cp /opt/spoond/spoond /opt/spoond/spoond.forkd-final
   cp /etc/systemd/system/spoond-backend.service /root/spoond-backend.service.forkd-final
   cp /etc/forkd-backend.env /root/forkd-backend.env.forkd-final
   cp /etc/spoond-gateway.env /root/spoond-gateway.env.forkd-final
   cp /etc/e2b/drain.env /root/drain.env.pre-cutover
   cp /etc/systemd/system/e2b-orchestrator.service /root/e2b-orchestrator.service.pre-cutover
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
    `/root/src/spoond` (on `main`):
    ```bash
    export PATH=/usr/local/go/bin:$PATH
    set -a; . /etc/spoond/conformance.env; set +a
    export CONFORMANCE_SUBSTRATE=e2b CONFORMANCE_GUEST_SERVICE=10.1.0.11:8891 CONFORMANCE_DESTRUCTIVE=1
    cd /root/src/spoond && go test -tags conformance -count=1 -timeout 90m -v ./conformance/ \
      -args -results "$PWD/conformance/results/$(date +%Y%m%dT%H%M%S)-prod-e2b.json"
    ```
    - The runner is stopped, so group R is safe.
    - The production conformance user is **not** an admin, so L6 may record
      `403` for `/metrics` (U02).
    - All groups must pass, except the allowed I3 failure.
    - The Ops runner copies the results file back (`scp`), and the
      orchestrator appends a summary to `RESULTS.md`.
    - Any failure → §Rollback.
15. `systemctl start spoond-runner` (protocol step 5).

    Then watch the first 3 CI jobs, for up to 4 hours: their runner journal
    lines `executor: job <id> final result=<n>`. **Do not roll back on CI
    job failures** (a job can fail on its own merits). If 2 or more of the
    first 3 end with `result` ≠ 0, write `BLOCKED-U12.md` with the job ids
    and their logs for the human, and keep production on E2B.

### Rollback (any time before step 20; exact)

```bash
systemctl stop spoond-runner spoond-backend spoond-sshd-gateway
systemctl stop e2b-orchestrator
sed -i 's/^vm.nr_hugepages = .*/vm.nr_hugepages = 0/' /etc/sysctl.d/90-e2b.conf
sysctl -w vm.nr_hugepages=0
cp /opt/spoond/spoond.forkd-final /opt/spoond/spoond
cp /root/spoond-backend.service.forkd-final /etc/systemd/system/spoond-backend.service
cp /root/spoond-gateway.env.forkd-final /etc/spoond-gateway.env
cp /root/drain.env.pre-cutover /etc/e2b/drain.env
cp /root/e2b-orchestrator.service.pre-cutover /etc/systemd/system/e2b-orchestrator.service
mv /var/lib/spoond/spoond.db /var/lib/spoond/spoond-e2b-rolledback.db
cp /var/lib/spoond/spoond-forkd-final.db /var/lib/spoond/spoond.db
systemctl daemon-reload
systemctl enable --now forkd-netns forkd-controller
systemctl enable --now spoond-watchdog.timer 2>/dev/null || true
systemctl start spoond-backend spoond-sshd-gateway spoond-runner
```

Leases created on E2B are lost on rollback. The orchestrator is stopped so
its hugepages (48 GiB after step 13) return to forkd. It stays stopped, and
the unit is `BLOCKED` for the human with the failure details. Rollback is
run by the Ops runner under the protocol; it never needs a human to start.

### Soak (7 days after step 15)

16. Each day, append a dated line to `RESULTS.md` with:
    - `spoond doctor` output (all `ok`);
    - the `spoond_leases{state="lost"}` value, which must be 0 unless an
      orchestrator crash happened (record the crash);
    - the CI success rate from Forgejo for the day, compared with the
      7 days before cutover.
17. **On day 7, enable GC (Autonomous window).**
    1. Collect every `gc: would delete <build_id> kind=<k> image=<i>` line
       from the last 7 days (`journalctl -u spoond-backend --since -7d`).
    2. Check each against `/var/lib/spoond/spoond.db`:
       - the build exists;
       - `kind` is `pause` or `checkpoint`, or it is a `template` build that
         is **not** any image's `current_build_id`;
       - it is not the `parent_build_id` of any non-deleted build;
       - it is not a `ref_build_id` in `build_refs`.
    3. If every candidate passes: append `GC_DELETE=1` to
       `/etc/spoond/backend.env`, then `systemctl restart spoond-backend`
       inside an Autonomous window. The rollback artifact is a copy of
       `/etc/spoond/backend.env` taken first; rollback is restoring it and
       restarting.
    4. If any candidate fails: do not enable GC, and write `BLOCKED-U12.md`
       with the failing candidates.

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
20. **On vm2, 30 days after step 15, with no rollback** (authorized by the
    human; record the due date in `STATUS.md` at step 15, and run it then,
    through the Ops runner or a human):
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

- Production passes the full conformance suite on E2B (I3 may fail as a
  recorded known limitation).
- The 7-day soak meets the Definition of Done in `00-README.md`.
- forkd code, tooling and data are removed.
- The docs are updated.
