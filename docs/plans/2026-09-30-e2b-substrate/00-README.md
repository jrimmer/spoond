---
artifact_contract: ce-unified-plan/v1
artifact_readiness: implementation-ready
product_contract_source: ce-plan-bootstrap
execution: code
---

# spoond on E2B's runtime: implementation spec

This directory is a complete, self-contained specification for replacing
forkd with E2B's open-source node runtime as spoond's microVM substrate. It is
written to be implemented **exactly as stated** by a coding model with no
further research. Every decision is already made. When this spec says
"do X", do X; when it gives a value, use that value.

## How to use this spec (read fully before starting)

1. **Work unit by unit, in the order of the Unit Index.** Each unit file
   (`Uxx-*.md`) is self-contained and lists its preconditions. Do not start a
   unit whose preconditions are unmet.
2. **Do exactly what the unit says, and nothing else.** No extra features, no
   refactors of unrelated code, no renames that are not listed, no dependency
   changes that are not listed, no "while I'm here" fixes.
3. **Facts are stated, not discovered.** Each unit lists the facts it relies
   on, with the appendix section that proves each one. **If the code you find
   contradicts a stated fact** (a function is missing, a signature differs, a
   file is elsewhere), **STOP**. Report the exact contradiction (file, line,
   expected, found) and do not improvise a workaround.
4. **Commits.**
   - Use the commit messages given in each unit. They follow
     `type(scope): summary`.
   - One commit per "Commit" marker in a unit.
   - **Never** add AI attribution, `Co-Authored-By` trailers, or "Generated
     with" text to commits, code comments, docs or PRs.
5. **Tests gate every commit.**
   - In spoond: `test -z "$(gofmt -l .)" && go build ./... && go vet ./... && go test ./...` must pass before each commit.
   - In the E2B fork, the unit gives the exact commands.
6. **The host is live.** host runs production CI (forkd until U12, then
   E2B). The human is its only user. On host, run only the commands a unit
   lists. Never restart `forkd-controller`, `spoond-backend`,
   `spoond-runner`, `spoond-sshd-gateway` or `e2b-orchestrator` unless the
   unit says so. **Every production-affecting step runs under the
   Autonomous window protocol below**; there is no other gate.
7. **Secrets.** Never commit tokens, seeds or keys. Secrets live in root-owned
   `0600` files under `/etc/spoond/` or `/etc/e2b/` on the host, created by the
   operator or by the unit's commands.
8. **Where something is marked OPERATOR**, a human must do it (it needs
   credentials the implementing model does not have). Stop and ask for it.
   Both remaining OPERATOR items are **already done** (2026-09-30): the fork
   repository `example.com/e2b-runtime` exists (empty, private), and
   `/etc/spoond/conformance.env` is provisioned on the host (below). host also
   has a deploy key with write access to both repositories, and its git
   config rewrites `https://git.example.com/` to SSH, so host can clone and push
   with the URLs this spec uses.
9. **Workers and verifiers never touch host.** The Ops runner (see
   `02-orchestration.md`) executes every host command, verbatim from a unit.
10. **Merges to `main`.** The orchestrator merges `feat/e2b-substrate` into
    `main` (U05 and U12 step 1) once the unit's worker tests pass, its
    verifier returns PASS, and its required conformance result (if any)
    passes. No human merge is needed.

## Production conformance credentials (OPERATOR; done 2026-09-30)

The human runs a provisioning script (outside this spec) that creates, in
the **production** identity store, the user `conformance` (kind `agent`,
**not** admin: only the first identity user is admin and there is no promote
API) with quota `max_leases=20`, an SSH key `/etc/spoond/conformance_ed25519`
registered by fingerprint, and writes `/etc/spoond/conformance.env` (root,
0600):

```ini
CONFORMANCE_API=https://spoond.example.com:8890
CONFORMANCE_TOKEN=<token>
CONFORMANCE_USER=conformance
CONFORMANCE_USER_ID=<user id>
CONFORMANCE_SSH=local
CONFORMANCE_SSH_KEY=/etc/spoond/conformance_ed25519
CONFORMANCE_SSH_GATEWAY=127.0.0.1:2222
CONFORMANCE_PROXY_URL=http://127.0.0.1:8891
CONFORMANCE_PROXY_SECRET=<production PROXY_AUTH_SECRET, may be empty>
CONFORMANCE_PROXY_SUFFIX=.sandbox.example.com
CONFORMANCE_BACKEND_UNIT=spoond-backend
```

Every production conformance run and every production smoke test loads it
with `set -a; . /etc/spoond/conformance.env; set +a`. Per-run variables
(`CONFORMANCE_SUBSTRATE`, `CONFORMANCE_GUEST_SERVICE`,
`CONFORMANCE_DESTRUCTIVE`) are set by the run. The staging equivalent,
`/etc/spoond-staging/conformance.env`, is created by the Ops runner in U08.

## Autonomous window protocol

The human has given standing authorization for production-affecting steps,
provided forkd and spoond can be restored if anything fails. Every step a
unit marks **"(Autonomous window)"** is executed by the Ops runner exactly
like this; nothing else is needed from a human.

1. **Rollback-ready check.** Every rollback artifact the step lists
   (previous binary, unit file, env file, DB copy, as applicable) exists and
   is verified (`test -s <file>`; for a binary also `<binary> help`
   exits 0 or 2; for a DB copy `python3 -c "import sqlite3;sqlite3.connect('<copy>').execute('PRAGMA integrity_check').fetchone()[0]=='ok' or exit(1)"`). If
   any is missing or fails: do not act; `BLOCKED`.
2. **Idle wait.** Poll every 60 s until `window_idle` returns 0; give up
   after 24 h → `BLOCKED`. `window_idle` (exact):
   ```bash
   window_idle() {
     # No backend exec/stream/create log lines in the last 10 minutes.
     n=$(journalctl -u spoond-backend --since -10min -o cat | grep -cE '(exec|stream|create): ')
     [ "$n" -eq 0 ] || return 1
     # Every job the runner started since it last started has ended.
     since=$(systemctl show spoond-runner -p ActiveEnterTimestamp --value)
     log=$(journalctl -u spoond-runner --since "${since:--24h}" -o cat)
     for j in $(printf '%s\n' "$log" | grep -oE 'executing job [0-9]+' | awk '{print $3}' | sort -u); do
       printf '%s\n' "$log" | grep -qE "job $j (final result=|failed:)" || return 1
     done
     return 0
   }
   ```
   (The runner logs `worker N: executing job <id>`, then either
   `executor: job <id> final result=...` or `worker N: job <id> failed: ...`;
   the backend logs `exec: <id>: ...`. U08 keeps those backend log lines.)
   The runner half looks only at jobs since the runner last started: every
   window stops the runner, and a job killed by that stop never logs an
   end, but it cannot still be running. (Until 2026-10-02 this scanned the
   last 24 h, so one interrupted or failed job blocked every window for up
   to a day.)
   Take every step baseline that creates leases or runs commands in them
   (lease-state snapshots, pre-checks, probe leases) **after** the idle
   wait, immediately before acting: the window's own lease activity would
   otherwise trip the backend half of `window_idle`.
3. **Act.** `systemctl stop spoond-runner`, perform the step exactly as the
   unit writes it, then verify:
   - `curl -fsS <backend>/healthz` returns `200`;
   - `window_smoke` returns 0 (exact):
     ```bash
     window_smoke() {
       set -a; . /etc/spoond/conformance.env; set +a
       H="Authorization: Bearer $CONFORMANCE_TOKEN"
       id=$(curl -fsS -H "$H" -d '{"image":"py-base","ttl":120}' "$CONFORMANCE_API/api/sandboxes" \
            | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])') || return 1
       out=$(curl -fsS -H "$H" -d '{"cmd":"echo ok"}' "$CONFORMANCE_API/api/sandboxes/$id/exec" \
            | python3 -c 'import json,sys; print(json.load(sys.stdin)["stdout"].strip())')
       curl -fsS -X DELETE -H "$H" "$CONFORMANCE_API/api/sandboxes/$id" >/dev/null
       [ "$out" = ok ]
     }
     ```
     plus any extra verification the step lists.
4. **On any failure** (a command exits non-zero, a verification fails):
   run the step's rollback commands immediately, run `window_smoke` against
   the restored system, then `BLOCKED` with the failing command, its output,
   and the post-rollback smoke result.
5. **Always** finish with `systemctl start spoond-runner`, on success and on
   failure.

Every step marked "(Autonomous window)" names its rollback artifacts and
its rollback commands. A step without them must not be run.

## What we are building (one paragraph)

spoond is the control plane: identity, quotas, leases, sharing, gateway,
proxy, policy, catalog and SQLite state. E2B's **orchestrator** (a per-node Go
service driving Firecracker microVMs) is the data plane. We run it standalone
from a **patch-queue fork** of `github.com/e2b-dev/runtime`, driven over gRPC.
E2B's guest agent **envd** runs inside every sandbox and is spoken to with
Connect-RPC through the orchestrator's proxy. Images are defined as container
images (Dockerfiles), pushed to a local registry, and turned into E2B
"templates" (a booted, snapshotted microVM). Every sandbox create is a
memory-snapshot restore, so starts are warm. Fork, pause/resume and checkpoint
are native.

## Fixed decisions (do not revisit)

| # | Decision |
|---|---|
| D1 | Replace forkd with E2B's orchestrator + template manager + envd. forkd is removed in U12. |
| D2 | spoond is the control plane. **Do not** run E2B's API, dashboard, client-proxy, Postgres, ClickHouse, Redis, Nomad or cloud storage. |
| D3 | Warm start, suspend/resume with memory, and fork of a running sandbox with memory are hard requirements. |
| D4 | Crash trade-off accepted: an orchestrator crash loses running state back to each sandbox's last snapshot. **Planned** restarts are lossless via the drain protocol (U10). Long-term VMs are out of scope (they live on Proxmox, infrastructure host). |
| D5 | The **lease API** (`/api/*` routes in `api/server.go`) is the compatibility contract. Everything beneath it may change. Additive changes only. |
| D6 | Images are "container-defined, VM-isolated": Dockerfiles → local registry → E2B template build. No bake scripts, no `rootfs-init`. |
| D7 | Stay connected to upstream: a mirror plus a short patch series (P1–P5), rebased at most monthly, gated by the conformance suite (U13). |
| D8 | Fork base commit: **`e473dd13`** of `github.com/e2b-dev/runtime` (all facts in appendices A2/A3 were extracted from it). |
| D9 | Firecracker, guest kernel and busybox are E2B's **published binaries**, pinned by SHA-256 (U04). orchestrator and envd are **built from our fork**. |
| D10 | spoond state moves to **SQLite via `modernc.org/sqlite`** (pure Go, embedded engine). This includes leases, shares, pool, and the template/build/snapshot catalog. The identity store (`users.json`) is unchanged. |
| D11 | spoond moves to **Go 1.27.1** and current dependency versions (U01). No downgrades anywhere. |
| D12 | Hugepages are required: E2B builds every template with 2 MiB hugepages (A2 §6), so spoond sets `huge_pages=true` on every create and admits sandboxes only when enough free hugepages exist (U08). |
| D13 | Guest egress policy is enforced by E2B's two layers (per-netns nftables + userspace TCP proxy), extended by patch P4 for per-sandbox private allowances with TCP port scoping. |
| D14 | The SSH gateway relays SSH session channels onto envd processes (PTY, exec, SFTP). No `sshd` in the guest is used by spoond. No `setns` anywhere in spoond. |
| D15 | No forkd adapter. Production stays on the pre-U06 binary (forkd) until U12; E2B work runs on a staging instance on the host (`01-architecture.md` §Staging). |
| D16 | Memory is fixed per image (a snapshot restores with its build's RAM). `memory_mib` on create must be 0 or equal to the image's memory, otherwise 400. |
| D17 | Guest DNS is `10.0.0.1` then `8.8.8.8`, written by each image's start command, so `git.example.com` resolves to the LAN edge. |

## Hosts, repositories, names

| Thing | Value |
|---|---|
| spoond repo | `https://git.example.com/example/spoond` (Go module `github.com/jrimmer/spoond`) |
| spoond work branch | `feat/e2b-substrate`, created from `main` at the start of U01. After U05, the orchestrator merges it into `main` (U05 §Merge and production deploy; rule 10); from U06 on, work continues on `feat/e2b-substrate` rebased on `main` |
| E2B fork repo | `https://git.example.com/example/e2b-runtime` (created 2026-09-30, empty; U03 pushes `upstream` and `spoond`) |
| Fork branches | `upstream` (= E2B `e473dd13`, never edited), `spoond` (= `upstream` + patches P1–P5, in order) |
| Upstream | `https://github.com/e2b-dev/runtime.git` (module paths still `github.com/e2b-dev/infra/...`) |
| Target host | `spoond.example.com`, reached as `root@spoond.example.com`. x86_64, Debian 13, kernel `6.17.13-2-pve`, cgroup v2, 4 KiB pages, glibc 2.41, 16 vCPU, 62 GiB RAM, ZFS pool `forkdcache` (block cloning active) |
| Coexistence | forkd keeps running on the host until U12. E2B uses different CIDRs (`10.11.0.0/16`, `10.12.0.0/16`) from forkd (`10.42.0.0/16`, `10.43.0.0/16`) |

## Glossary

- **Lease**: spoond's user-facing sandbox handle (`Lease` in `api/service.go`).
- **Sandbox**: a running E2B microVM. Its id is `sandbox_id` (format: `"i"` + 20 chars `[a-z0-9]`).
- **Template**: E2B's name for an image. `template_id` = 20 chars `[a-z0-9]`.
- **Build**: one immutable E2B artifact set (rootfs + memory snapshot + metadata), identified by a UUID `build_id`. Template builds, pauses and checkpoints each create a build.
- **Pause**: snapshot a sandbox to a new build, then stop it. Resume = create with `snapshot=true` from that build.
- **Checkpoint**: snapshot a *running* sandbox to a new build; the sandbox keeps running. Fork = checkpoint, then create N sandboxes from that build.
- **envd**: E2B's guest agent (HTTP + Connect-RPC on guest port 49983).
- **Substrate**: the new Go interface in spoond (`substrate/`) that hides E2B from the rest of spoond.

## Unit Index

Read `01-architecture.md` first: addresses, ports, directories and every
configuration value. To run the implementation with agents (orchestrator,
workers, verifiers), follow `02-orchestration.md`.

| Unit | File | Depends on | Summary |
|---|---|---|---|
| U01 | `U01-go-upgrade.md` | none | Go 1.27.1 and dependency upgrades, no regressions |
| U02 | `U02-conformance-suite.md` | U01 | The conformance suite; baseline run against forkd |
| U03 | `U03-e2b-fork-and-patches.md` | U01 step 10 only (Go 1.27.1 on the host) | Fork repo, patches P1–P5, build of orchestrator + envd |
| U04 | `U04-host-bringup.md` | U03 | host host setup, artifacts, systemd, firewall, smoke test |
| U05 | `U05-sqlite-store.md` | U01 | SQLite store for leases, shares, pool, catalog (still on forkd) |
| U06 | `U06-substrate-interface.md` | U04, U05 | `Substrate` interface, E2B client (gRPC + envd), tokens, fake |
| U07 | `U07-image-pipeline.md` | U04, U06 | Dockerfiles for every image, local registry, template builds |
| U08 | `U08-lease-lifecycle.md` | U07 | All lease operations on E2B: create, exec, stream, stat, prompt, suspend/resume/restart, clone, fork, pool, TTL, admission |
| U09 | `U09-networking-and-access.md` | U08 | Policy mapping, exposed ports, HTTP proxy, SSH gateway on envd, host address |
| U10 | `U10-restart-and-crash.md` | U08, U09 | Drain protocol, reconcile, recovered/lost leases, periodic checkpoint |
| U11 | `U11-catalog-gc-and-observability.md` | U08, U10 | Ref-counted snapshot deletion, disk accounting, metrics, health, doctor |
| U12 | `U12-cutover.md` | U02, U09, U10, U11 | Suite green on E2B, soak, switch off forkd, delete forkd code |
| U13 | `U13-upstream-runbook.md` | U12 | Monthly rebase procedure and gate |

U03 and U04 (fork and host) can proceed in parallel with U02 and U05 once U01
step 10 (Go 1.27.1 on the host) is done.

## Appendices (reference; quoted verbatim from source)

- `appendix/A1-spoond-current.md`: spoond's current code, every type, route,
  protocol and env var being replaced. Section numbers are cited as `A1 §n`.
- `appendix/A2-e2b-api.md`: E2B protos (verbatim), envd protos and HTTP
  API, how E2B's own API fills every request field, tokens, IDs, template
  build semantics. Cited as `A2 §n`.
- `appendix/A3-e2b-ops.md`: deploy scripts, env var table, host layout,
  ports, exact patch sites P1–P6, storage layout, networking rules. Cited
  as `A3 <section>`.

When a unit says "see A3 P4.4", open that appendix section. It contains the
exact code, with line numbers at commit `e473dd13`.

## Definition of Done (whole project)

1. U02's conformance suite passes against E2B on the host, including every
   performance budget in `U02-conformance-suite.md` §Budgets. The one allowed
   failure is `TestI3_DockerInDocker`, recorded as a known limitation.
2. Seven consecutive days of real CI traffic run on E2B without a lost lease,
   except in an orchestrator crash (D4).
3. forkd is switched off, and forkd code and deploy tooling are deleted (U12).
4. The fork's patch series has at most 5 patches, each with an upstream PR
   link or a written reason in `PATCHES.md`.
5. `docs/` describes the substrate, the drain protocol, the upgrade runbook
   and the crash trade-off (U12).
