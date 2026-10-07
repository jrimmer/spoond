# U02 — Conformance suite and forkd baseline

## Purpose

An executable suite that defines "done" for the whole project. It exercises
spoond **through the lease API** (the compatibility contract, D5), plus a few
host-level checks (`hostRun`). It is written once, run against forkd now as the
baseline, and run against E2B in U12.

## Preconditions

U01 is done.

## Facts relied on (A1 §5)

- **Auth:** `Authorization: Bearer <token>` on every `/api/*` call.
- **Create:** `POST /api/sandboxes` with body
  `{"image","ttl","memory_mib","persistent","network_policy","egress_allowlist","expose_ports"}`.
  - Returns `201` and
    `{"id","owner","address","image","ttl","persistent","expires_at","exposed"}`.
  - `exposed` is a map of port (string) → `"ip:port"`.
- **Exec:** `POST /api/sandboxes/{id}/exec` with body
  `{"cmd":"<shell string>","cwd","env","timeout"}`.
  - Returns `200` and `{"stdout","stderr","exit"}`.
  - A suspended lease returns `409`.
- **Delete:** `DELETE /api/sandboxes/{id}` returns `204`.
- **Suspend:** `POST /api/sandboxes/{id}/suspend` returns `200`
  `{"id","status":"suspended",...}`.
- **Resume:** `POST /api/sandboxes/{id}/resume` returns `200` `{"id","status":"running",...}`.
- **Restart:** `POST /api/sandboxes/{id}/restart` returns `200`
  `{"id","status":"running","message":"sandbox restarted"}`.
- **Keepalive:** `POST /api/sandboxes/{id}/keepalive` with `{"ttl":n}` returns
  `200` `{"id","persistent":true,"expires_at"}`.
- **Clone:** `POST /api/sandboxes/{id}/clone` with `{"tag"?}` returns `201`
  `{"id","image","source","branch_tag","persistent","expires_at"}`.
- **Stat:** `GET /api/sandboxes/{id}/stat` returns
  `{"cpu":{"load1"},"mem":{"used_mib","total_mib"},"disk":{"used_mib","total_mib"},"net":{"rx_bytes","tx_bytes"}}`.
- **List:** `GET /api/sandboxes` returns `{"sandboxes":[...]}`. **Images:**
  `GET /api/images` returns `{"images":[...]}`.
- **Stream:** `GET /api/sandboxes/{id}/stream` is a WebSocket. The protocol
  is in A1 §5.6:
  - first client frame `{"args":[...],"cwd","env","pty":bool}`;
  - server frames `{"stream":"started",...}`, `{"out":"..."}`,
    `{"exit_code":N}`;
  - client frames `{"in":"..."}` and `{"action":"stop"}`.
- **Policies:** `none|lan|internet|restricted`. The default at create is
  `restricted` (A1 §5.4).
- **Metrics and health:** `GET /metrics` (Prometheus text), `GET /healthz`.

## Deliverables

- New package `conformance/` at the repo root. Every file carries the build
  tag `//go:build conformance`, so `go test ./...` never runs it.
- `conformance/README.md`: how to run it (copy the "Running" section below).
- `conformance/results/` (git-ignored except `.gitkeep`): JSON results.
- `conformance/baseline-forkd.json`: the committed forkd baseline results.

## Configuration (environment variables read by the suite)

Every variable except the per-run ones (`CONFORMANCE_SUBSTRATE`,
`CONFORMANCE_GUEST_SERVICE`, `CONFORMANCE_DESTRUCTIVE`,
`CONFORMANCE_CAPACITY`) comes from an env
file loaded with `set -a; . <file>; set +a`: `/etc/spoond/conformance.env`
for production (provisioned before kickoff, README §Production conformance
credentials) and `/etc/spoond-staging/conformance.env` for staging (U08).

| Variable | Required | Meaning |
|---|---|---|
| `CONFORMANCE_API` | yes | e.g. `https://spoond.example.com:8890` |
| `CONFORMANCE_TOKEN` | yes | the token of the identity user `conformance` (kind `agent`, `max_leases=20`). In production it is **not** admin (only the first identity user is admin, and there is no promote API), so `GET /metrics` answers `403` there; on staging it is the first user and therefore admin (L6) |
| `CONFORMANCE_SSH` | yes | `local` or `user@host`. `local` runs host-level checks with `sh -c` on the machine running the suite; any other value runs them with `ssh <value>`. On host the value is `local` |
| `CONFORMANCE_SUBSTRATE` | yes | `forkd` or `e2b`. Selects the host-level check implementations |
| `CONFORMANCE_BACKEND_UNIT` | yes | the systemd unit of the backend under test: `spoond-backend` (production) or `spoond-backend-staging` (staging). R3 restarts exactly this unit |
| `CONFORMANCE_DESTRUCTIVE` | no | `1` enables group R (restarts and crashes). Default off |
| `CONFORMANCE_CAPACITY` | no | `1` enables group C (preemption, #128 part 3). Default off: C1 fills the host |
| `CONFORMANCE_SECOND_TOKEN` | group C | the token of a second, **non-admin** identity user. C1 creates its guaranteed lease as that user because the production conformance user is not admin and there is no promote API. Provisioned on the host beside `CONFORMANCE_TOKEN`; unset means C1 skips |
| `CONFORMANCE_IMAGES` | no | comma list. Default `py-base,go-base,dev-base,elixir-base,elixir-release,llm-review,scylla` |
| `CONFORMANCE_SSH_GATEWAY` | yes | `127.0.0.1:2222` (gateway `--listen :2222`, A1 §7.1) |
| `CONFORMANCE_SSH_KEY` | yes | path to the conformance user's private key; its fingerprint was registered when the user was created |
| `CONFORMANCE_USER` | yes | the conformance user's **name**, `conformance` (used as `Remote-User` for the proxy) |
| `CONFORMANCE_USER_ID` | yes | the conformance user's id (informational; recorded in the results file) |
| `CONFORMANCE_PROXY_URL` | yes | `http://127.0.0.1:8891` (production) or `http://127.0.0.1:18891` (staging). If the backend env sets `PROXY_AUTH_TRUSTED_PEERS`, it must contain `127.0.0.1/32`, otherwise N3 gets 403 (checked in step 0) |
| `CONFORMANCE_PROXY_SECRET` | yes (may be empty) | the backend's `PROXY_AUTH_SECRET`; empty when the backend runs without forward-auth |
| `CONFORMANCE_PROXY_SUFFIX` | yes | `.sandbox.example.com` |
| `CONFORMANCE_GUEST_SERVICE` | yes | host service address guests use (see N6) |
| `CONFORMANCE_MIXED_PRIVATE` | vm2 | private (LAN) IP a restricted lease allowlists together with a public domain (N9). **No default**: when unset (or `CONFORMANCE_MIXED_BLOCKED_PRIVATE` unset), N9 skips so a run never probes an assumed LAN address |
| `CONFORMANCE_MIXED_PRIVATE_PORT` | no | the TLS port of `CONFORMANCE_MIXED_PRIVATE` (N9). Default `443` |
| `CONFORMANCE_MIXED_DOMAIN` | no | public domain the same restricted lease allowlists (N9). Default `example.com` |
| `CONFORMANCE_MIXED_BLOCKED_PRIVATE` | vm2 | private IP the same restricted lease does **not** allowlist; a connection to it must be blocked (N9). **No default**: N9 skips with `CONFORMANCE_MIXED_PRIVATE` when unset |

## Running

The suite runs **on the host as root** from the checkout `/root/src/spoond`
(host checks, loopback proxy and gateway access), with
`CONFORMANCE_SSH=local`.

```bash
export PATH=/usr/local/go/bin:$PATH
test -d /root/src/spoond || git clone https://git.example.com/example/spoond.git /root/src/spoond
# BRANCH is the branch the unit under test names: feat/e2b-substrate for
# U02 and U06–U11; main for U05's R3 run and for U12.
cd /root/src/spoond && git fetch && git checkout "$BRANCH" && git pull --ff-only
go test -tags conformance -count=1 -timeout 60m -v ./conformance/ \
  -args -results "$PWD/conformance/results/$(date +%Y%m%dT%H%M%S)-$CONFORMANCE_SUBSTRATE.json"
```

Use the branch that holds the suite: `feat/e2b-substrate` before the U05
merge, `main` after U12. The `-results` path must be absolute: `go test` runs the test binary in the
package directory. The flag's default is
`results/<YYYYMMDDTHHMMSS>-<CONFORMANCE_SUBSTRATE>.json` relative to
`conformance/`. `TestMain` creates the parent directory if it is missing.

To run a single test, add `-run '^<TestName>$'` before `-args`.

**Defaults for every test unless the test says otherwise:** `image` =
`py-base`, `ttl` = 600, `persistent` = false, no `network_policy` (the
backend default, `restricted`).

**Group R (destructive) protocol.** A run with `CONFORMANCE_DESTRUCTIVE=1`
is an **(Autonomous window)** step (`00-README.md`): the Ops runner does the
rollback-ready check, waits for `window_idle`, stops `spoond-runner`, runs
the suite with `CONFORMANCE_DESTRUCTIVE=1` and the correct
`CONFORMANCE_BACKEND_UNIT`, verifies `/healthz` and `window_smoke`, and
always starts `spoond-runner` again.
- **Rollback artifacts:** none (group R changes no files).
- **Rollback commands (on any failure):**
  `systemctl restart e2b-orchestrator` (e2b runs only), then
  `systemctl restart <CONFORMANCE_BACKEND_UNIT>`; on forkd,
  `systemctl restart spoond-backend`.

On forkd, R1 always skips (it would restart `forkd-controller` and kill every
production VM) and R2 always skips (`e2b` only). The only group R test ever
run against forkd is R3, and only as
`-run '^TestR3_BackendRestart$'` (U05).

## Harness requirements

- `conformance/client.go` holds a small HTTP client:
  - `create`, `exec`, `delete`, `suspend`, `resume`, `restart`, `keepalive`,
    `clone`, `fork`, `stat`, `list`, `images` and `stream`;
  - `stream` uses `github.com/gorilla/websocket`, already a dependency.
- **Host checks:** a helper `hostRun(t, script string) string` runs
  `sh -c <script>` locally when `CONFORMANCE_SSH=local`, otherwise
  `ssh <CONFORMANCE_SSH> <script>`, and fails the test on a non-zero exit.
- **Every test registers `t.Cleanup` that deletes every lease it created.**
  Ignore `404` on delete.
- **Results:** a `TestMain` writes a JSON file with an array of
  `{"test","status":"pass|fail|skip","duration_ms","metrics":{...},"error"}`.
  Metrics are the timing values named in each test.
- **Timing:** measure wall-clock time around the HTTP call only.
- **Exec helper:** `execOK(t, id, cmd) string` fails the test unless
  `exit == 0`, and returns trimmed stdout.
- **Unique markers:** a helper returns a random 12-hex string for marker
  values.

## Tests (names are exact; each is a Go test function)

### Group L — lifecycle and streaming

- **`TestL1_CreateExecEachImage`**: for each image in `CONFORMANCE_IMAGES`:
  1. `create {"image":I,"ttl":600}` returns `201`, and `id` is non-empty.
  2. `execOK(id, "echo ok")` returns `ok`.
  3. `exec(id, "echo err >&2; exit 3")` gives `exit==3` and `stderr=="err\n"`.
  4. Delete it and expect `204`.

  Record metric `create_ms` per image.
- **`TestL2_ExecTimeout`**: `exec(id, "sleep 30", timeout=2)` returns within
  10 s with a non-zero `exit`. A following `execOK(id,"echo alive")` returns
  `alive`.
- **`TestL3_StreamPTY`**:
  1. Open the stream with `{"args":["/bin/bash","-l"],"pty":true}`.
  2. Expect a `{"stream":"started"}` frame within 10 s.
  3. Send `{"in":"echo MARK$((40+2))\n"}`.
  4. Within 10 s, some `out` frame contains `MARK42`.
  5. Send `{"in":"exit 7\n"}`.
  6. Expect `{"exit_code":7}` within 10 s.
- **`TestL4_StreamNoPTY`**: open the stream with
  `{"args":["/bin/sh","-c","echo A; echo B"],"pty":false}`. The concatenated
  `out` equals `"A\nB\n"`, then `exit_code` is `0`.
- **`TestL5_KeepaliveAndTTL`**:
  1. `create {"image":"py-base","ttl":20}`.
  2. After 30 s, `exec` returns `404` (the TTL sweeper released it).
  3. Create `{"image":"py-base","ttl":20,"persistent":true}` and call
     `keepalive {"ttl":120}`. Expect `200` with `"persistent":true` and
     `expires_at` at least 110 s after the call.
  4. At 30 s after its create, `execOK(id,"echo alive")` returns `alive`.

  (Keepalive only applies to persistent leases; a non-persistent lease gets
  `400`.)
- **`TestL6_StatAndHealth`**:
  1. `stat` returns `mem.total_mib > 0` and `disk.total_mib > 0`.
  2. `GET /healthz` returns `200`.
  3. `GET /metrics` returns `200` or `403`. Record the status as metric
     `metrics_status`. If it is `200`, the body contains
     `spoond_leases_active`. On staging (the conformance user is admin) it
     must be `200`: the test fails on `403` when
     `CONFORMANCE_BACKEND_UNIT=spoond-backend-staging`.

### Group S — state (memory)

- **`TestS1_SuspendResumeKeepsProcesses`**:
  1. Create `{"image":"dev-base","persistent":true,"ttl":3600}`.
  2. Start a counter:
     `execOK("nohup sh -c 'i=0; while :; do i=$((i+1)); echo $i > /tmp/ctr; sleep 0.2; done' >/dev/null 2>&1 & echo started")`.
  3. Start tmux: `execOK("tmux new-session -d -s conf 'sleep 100000'; tmux ls")`.
  4. Wait 2 s, then read `a := /tmp/ctr` immediately before step 5.
  5. Suspend. Record `suspend_ms`.
  6. Assert `exec` returns `409`.
  7. Wait 5 s.
  8. Resume.
  9. Read `b := /tmp/ctr` immediately, then read `c` after 2 s.
  10. Assert `a ≤ b ≤ a + ceil(suspend_ms/200) + 10`. The counter may run
      until the pause freezes the VM, but it did not advance through the 5 s
      wait (25 more steps).
  11. Assert `c > b`. The process is running again.
  12. Assert `tmux ls` output contains `conf:`.

  Record `suspend_ms` and `resume_ms`.
- **`TestS2_CloneRunningSandbox`**:
  1. Create a persistent `dev-base` lease.
  2. Start the counter.
  3. Write marker `M` to `/root/marker`.
  4. Clone and expect `201`.
  5. In the clone: `/root/marker == M`, and the counter file is advancing
     (two reads 1 s apart differ).
  6. In the source: the counter is still advancing.
  7. Write `/root/marker=S` in the source and `C` in the clone. Each reads
     back its own value.

  Record `clone_ms`.
- **`TestS3_ForkToEight`**: requires the new route from U08
  (`POST /api/sandboxes/{id}/fork` with `{"count":8}` returning `201`
  `{"source":id,"ids":[...8 ids]}`).
  1. Create a persistent `py-base` lease.
  2. Start the counter.
  3. Fork 8.
  4. All 8 have an advancing counter.
  5. Write a distinct marker in each and read each back.

  Record `fork8_ms`. **On forkd this test is expected to fail** (route
  absent); record `fail` in the baseline.
- **`TestS4_CreateLatency`**: for `py-base`, create and delete 10 times
  sequentially. Record `create_p50_ms` and `create_p95_ms`.

### Group D — storage

- **`TestD1_PrivateWritableDisks`**:
  1. Create two `py-base` leases.
  2. In each: `execOK("echo $MARK > /etc/conformance-marker && sync")` with
     different markers.
  3. Each reads its own marker.
  4. Host check (`e2b` only): with `hostRun`, assert that no file under
     `/forkdcache/e2b/storage/templates/<py-base current build id>/` has an
     mtime later than the test start. Get the build id from
     `GET /api/images?detail=1`, a U08 addition.
- **`TestD2_CloneOutlivesSource`**:
  1. Create a persistent lease.
  2. Write a marker.
  3. Clone.
  4. **Delete the source.**
  5. Wait 5 s.
  6. The clone still reads the marker.
  7. Suspend and resume the clone. It still reads the marker.
- **`TestD3_SnapshotDeletion`**: requires U10 (`POST /api/sandboxes/{id}/checkpoint`)
  and U11 (`DELETE /api/snapshots/{build_id}`). Expected to fail on forkd.
  1. Create persistent lease A.
  2. Checkpoint A twice: `c1`, then `c2`. A now runs from `c2`, and `c2`'s
     parent is `c1`.
  3. `DELETE /api/snapshots/c1` returns `409`: it has a descendant and is an
     ancestor of A's build.
  4. `DELETE /api/snapshots/c2` returns `409`: A runs from it.
  5. Create persistent lease B and suspend it. Record its pause build `p`
     from `GET /api/sandboxes/B` (field `resume_build_id`, added in U11).
  6. Delete lease B.
  7. `DELETE /api/snapshots/p` returns `204`.
  8. A second `DELETE /api/snapshots/p` returns `404`.

### Group N — network

For every N test, a helper `canTCP(id, host, port) bool` runs:
`execOK(id, "timeout 5 bash -c '</dev/tcp/HOST/PORT' && echo yes || echo no")`.
`py-base` has bash.

- **`TestN1_Policies`**:
  `10.0.0.203:443` is `git.example.com` on the LAN: a known-open private
  TCP service that is not host itself.
  1. `none`: `canTCP(1.1.1.1,443)=no` and `canTCP(10.0.0.203,443)=no`.
  2. `internet`: `canTCP(1.1.1.1,443)=yes` and `canTCP(10.0.0.203,443)=yes`
     (E2B `internet` allows public plus the LAN ranges, as forkd does).
  3. `lan`: `canTCP(10.0.0.203,443)=yes` and `canTCP(1.1.1.1,443)=no`.
     Also `canTCP(10.0.0.11,22)=no`: the host's own addresses are refused
     except the granted service port (`e2b` only; skip on forkd).
  4. `restricted` with `egress_allowlist:["example.com"]`:
     `curl -sS -o /dev/null -w '%{http_code}' https://example.com` prints a
     3-digit code, and `https://www.google.com` fails.
- **`TestN2_LivePolicyChange`**: requires U09
  (`POST /api/sandboxes/{id}/network {"network_policy":"internet"}`).
  1. Create with `none`.
  2. `canTCP(1.1.1.1,443)=no`.
  3. Change the policy to `internet`.
  4. Within 5 s, `canTCP=yes`.
- **`TestN3_ProxyURL`**:
  1. Create `py-base` with `ttl:600`.
  2. `exec` `nohup python3 -m http.server 8080 >/dev/null 2>&1 &`, then
     `sleep 1`.
  3. `GET CONFORMANCE_PROXY_URL/` with headers:
     - `Host: <lease-id>-8080<CONFORMANCE_PROXY_SUFFIX>`
       (A1 §6.3 `parseProxyHost`);
     - `X-Proxy-Auth: <CONFORMANCE_PROXY_SECRET>`;
     - `Remote-User: <CONFORMANCE_USER>`.
  4. Expect status `200`, and a body containing `Directory listing`.
- **`TestN4_ExposePortsPeer`**:
  1. Create `scylla` with `{"expose_ports":[9042],"ttl":900}`. Read
     `exposed["9042"]` as `H:P`.
  2. Create `py-base` A with `network_policy:"internet"`.
     `canTCP(A, H, P) = yes`.
  3. Create `py-base` B with `network_policy:"none"`. `canTCP(B, H, P) = no`.
  4. For port 22 on H from A: `canTCP(A, H, 22) = no`. Only exposed ports
     are reachable.
- **`TestN5_SSHGateway`**:
  1. Connect with `golang.org/x/crypto/ssh` to `CONFORMANCE_SSH_GATEWAY` as
     user `new` (a username beginning `new` creates a sandbox of the default
     SSH image `dev-base`, A1 §7.3), with `CONFORMANCE_SSH_KEY`.
  2. Get an interactive session: run `echo SSHOK` over a PTY and read
     `SSHOK`.
  3. Run a non-PTY exec: `uname -s` returns `Linux`.
  4. Upload a 1 MiB file over SFTP and read it back identical. Add the
     module with `go get github.com/pkg/sftp@latest` in this unit; only
     `conformance/*.go` imports it.
  5. Open a second SSH connection as user `ctl` with the same key and exec
     `ls --json`. The output contains the lease id created in step 1 (find
     it the same way for cleanup).
- **`TestN6_LLMGateway`**: create `py-base` with `restricted` and no
  allowlist. `canTCP(CONFORMANCE_GUEST_SERVICE, …) = yes`, where
  `CONFORMANCE_GUEST_SERVICE` is `host:port`:
  - `10.0.0.11:18891` for staging;
  - `10.0.0.11:8891` for production on E2B;
  - `10.43.0.1:8891` on forkd.
- **`TestN9_MixedRestrictedAllowlist`** (skips unless
  `CONFORMANCE_MIXED_PRIVATE` and `CONFORMANCE_MIXED_BLOCKED_PRIVATE` are
  set, so a run never probes an assumed LAN address): create `py-base` with
  `restricted` and `egress_allowlist: [CONFORMANCE_MIXED_PRIVATE,
  CONFORMANCE_MIXED_DOMAIN]` (default domain `example.com`, port
  `CONFORMANCE_MIXED_PRIVATE_PORT` default `443`).
  1. `canTCP(CONFORMANCE_MIXED_PRIVATE, CONFORMANCE_MIXED_PRIVATE_PORT) = yes`
     — a domain in the allowlist must not break the allow-listed private IP
     (spoond-4pa).
  2. `canTCP(CONFORMANCE_MIXED_DOMAIN, 443) = yes`.
  3. `canTCP(www.google.com, 443) = no` — an unlisted public host stays
     blocked.
  4. `canTCP(CONFORMANCE_MIXED_BLOCKED_PRIVATE, 443) = no` — an unlisted
     private address stays blocked.

### Group R — restarts and crashes (only with `CONFORMANCE_DESTRUCTIVE=1`)

- **`TestR1_PlannedRestart`** (`e2b` only; `t.Skip` on `forkd`):
  1. Create 5 persistent `dev-base` leases, each with a counter and a tmux
     session.
  2. `hostRun("systemctl restart e2b-orchestrator")` (the unit's `ExecStop`
     runs the drain, U10).
  3. Poll every 2 s, up to 180 s, until every lease answers
     `execOK(id,"echo alive")`.
  4. All 5 leases still exist, the counters advance, and `tmux ls` shows the
     session.

  Record `restart_total_ms` = the wall-clock time from the start of the
  `hostRun` in step 2 to the end of step 3 (this metric is the one exception
  to "time only the HTTP call").
- **`TestR2_OrchestratorCrash`** (`e2b` only):
  1. Create 3 persistent leases.
  2. Checkpoint each via `POST /api/sandboxes/{id}/checkpoint` (U10 route).
  3. Write marker `after` to `/root/m` *after* the checkpoint.
  4. Record `N0 := hostRun("ip netns list | grep -c '^ns-' || true")`.
     Then `hostRun("systemctl kill -s SIGKILL e2b-orchestrator")`. systemd
     restarts it with `Restart=always`.
  5. Poll `GET /api/sandboxes/{id}` for each lease every 2 s, up to 180 s,
     until it reports `"state":"recovered"`.
  6. Each lease reports `"state":"recovered"`.
  7. `/root/m` is absent (the state is from the checkpoint).
  8. `execOK` works.
  9. Host checks:
     - connected NBD devices,
       `hostRun("ls -d /sys/block/nbd*/pid 2>/dev/null | wc -l")`, equal the
       number of E2B Firecracker processes,
       `hostRun("pgrep -f '^/fc-versions/' | wc -l")` (one NBD rootfs per
       running sandbox);
     - `hostRun("ip netns list | grep -c '^ns-' || true")` equals `N0`.
- **`TestR3_BackendRestart`**:
  1. Create 2 persistent leases.
  2. `hostRun("systemctl restart " + CONFORMANCE_BACKEND_UNIT)`.
  3. Poll `GET /healthz` every 1 s, up to 60 s, until `200`.
  4. Both leases still exist and `execOK` works. (On forkd before U05 this
     fails, because leases are in memory.)

### Group I — images

- **`TestI1_ImagesListed`**: `GET /api/images` contains every name in
  `CONFORMANCE_IMAGES`.
- **`TestI2_RealWorkloads`**:
  1. In `elixir-release`: run `mix --version`, `cargo --version` and
     `pnpm --version`.
  2. In `go-base`: `go version` contains `go1.27`.
  3. In `dev-base`: `tmux -V` and `sshd -V 2>&1`.
  4. In `py-base`: `python3 --version`.

  All must exit 0.
- **`TestI3_DockerInDocker`** (`e2b` only):
  1. In `dev-base` with `internet`, run
     `apt-get install -y docker.io && (dockerd >/tmp/d.log 2>&1 &) && sleep 8 && docker run --rm hello-world`.
  2. The output contains `Hello from Docker!`.

  If this fails because of guest kernel features, record the failure. **U12
  decides**: a failure here does **not** block cutover (DinD is not used by
  current CI). It is reported as a known limitation.

## Budgets (E2B must meet these in U12; forkd baseline only records)

| Metric | Budget |
|---|---|
| `create_p95_ms` (py-base) | ≤ 2000 |
| `create_ms` (every image) | ≤ 5000 |
| `clone_ms` | ≤ 5000 |
| `fork8_ms` | ≤ 10000 |
| `suspend_ms` (dev-base, 2 GiB) | ≤ 15000 |
| `resume_ms` | ≤ 3000 |
| `restart_total_ms` (5 leases) | ≤ 120000 |

If E2B misses a budget in U12, record the measured value in the results, and
**STOP for an OPERATOR decision**. Do not tune E2B flags on your own.

## Steps

0. **Record the production backend env file path (Ops runner, read-only):**
   `ssh root@spoond.example.com 'systemctl cat spoond-backend | grep EnvironmentFile'`.
   Write the path into `STATUS.md` as `BACKEND_ENV_FILE`. Every later mention
   of `/etc/forkd-backend.env` in this spec means this path. If the unit has
   more than one `EnvironmentFile=` line, STOP. Also run
   `ssh root@spoond.example.com "grep '^PROXY_AUTH_TRUSTED_PEERS=' <path>"`; if
   the line exists and does not contain `127.0.0.1`, STOP (N3 would get
   403).
1. Implement the harness and all tests above in `conformance/`.
2. Add `conformance/results/*` to `.gitignore`, except `.gitkeep`.
3. **Commit:** `test(conformance): substrate conformance suite`.
4. Check that `/etc/spoond/conformance.env` exists on the host (Ops runner:
   `test -s /etc/spoond/conformance.env`). If not, `BLOCKED` on the
   OPERATOR.
5. Run the suite against production forkd on the host **without** group R (Ops
   runner): `set -a; . /etc/spoond/conformance.env; set +a`, then
   `CONFORMANCE_SUBSTRATE=forkd CONFORMANCE_GUEST_SERVICE=10.43.0.1:8891`,
   `CONFORMANCE_DESTRUCTIVE` unset. The Ops runner copies the results file
   into the worker's worktree as `conformance/baseline-forkd.json`
   (`scp root@spoond.example.com:<results file> <worktree>/conformance/baseline-forkd.json`);
   the worker commits it.
6. **Commit:** `test(conformance): forkd baseline results`.

## Done when

- The suite compiles, runs, and produces the JSON results.
- The forkd baseline is committed. Expected forkd failures: S3, D3, N2.
  Skipped: I3, R1, R2, R3 (group R not run) and the `e2b`-only host
  checks.

## Do not

- Do not modify application code in this unit.
- Do not run group R outside the Autonomous window protocol.
