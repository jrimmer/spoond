# Forkd baseline: recorded deviation causes

Purpose: U12 compared its E2B conformance run against the forkd baseline
recorded before the cutover (the forkd substrate and its baseline file were
removed with the cutover, U12 step 18). This file records **why** that
baseline deviated from a green run, so the recorded causes stay readable
after the fact.

Source: the architect's analysis of 2026-09-30 (cited as such throughout).

## Accepted baseline failures

### 1. Stream 500 and proxy 502 — known forkd bug

The stream relay and the proxy dial `127.0.0.1:8888` **inside the sandbox
netns**, where nothing listens. This is a forkd bug, not a spoond contract
failure. U08 (stream) and U09 (proxy) replace both paths on E2B, so these
failures are accepted baseline failures and must not be expected after
cutover.

### 2. Missing images on production forkd — I1, I2, L1, S1, S2, N5

The images `dev-base`, `elixir-base`, `llm-review` and `rust-base` are
missing from production forkd; only `py-base`, `go-base`, `elixir-release`,
`scylla` and a leftover clone exist. Per the architect's analysis, the
Dockerfile comments point at the vm1 consolidation as the cause.

This explains the baseline failures of every test that needs a missing
image: `TestI1_ImagesListed`, `TestI2_RealWorkloads`,
`TestL1_CreateExecEachImage`, `TestS1_SuspendResumeKeepsProcesses` and
`TestS2_CloneRunningSandbox` (both dev-based), and the SSH gateway PTY EOF
in `TestN5_SSHGateway` (the gateway's default image is `dev-base`).

These are accepted baseline failures until the cutover rebuilds all seven
images (U07).

### 3. N1 internet expectation — spec bug (fixed)

N1's old `internet` expectation (`canTCP(10.1.0.203,443)=no`) was a spec
bug: in forkd, `internet` means everything — `policyCommands` flushes all
rules, so private/LAN addresses are reachable. The spec wrongly redefined
E2B `internet` as "public only; private denied". Fixed 2026-09-30: the U08
egress mapping and N1 now assert `internet` = public **plus** the LAN
ranges. The host-address guard (U03 P4) still keeps sandboxes away from
vm2's own addresses; only granted ports reach host services. The baseline
corroborates this: it recorded `TestN1_Policies` as `fail` with error
`"internet: 10.1.0.203:443 reachable, want blocked"` — on forkd the sandbox
did reach the LAN address under `internet`, and only the wrong expectation
made the test fail.

## Baseline measurements (2026-09-30 run)

From the forkd baseline run of 2026-09-30 (architect's analysis):

| Metric | Value |
|---|---|
| `create_p50` | 54 ms |
| `create_p95` | 55 ms |
| `create_ms` (py-base) | 12 ms |
| `create_ms` (go-base) | 11 ms |

## Probe semantics

On e2b, egress denial is visible only in the data phase, in **two
modes**:

- **Layer-1 deny** (policy/cidr): the SYN is **blackholed** — the guest's
  `connect()` hangs until its own timeout.
- **Layer-2 deny** (port-scoped allowances): the `connect()` succeeds
  against the transparent proxy, then the connection is **closed before
  any data** (immediate EOF).

So reachability is classified by the data-phase outcome: **reachable =
connect OK AND (data received OR connection still open after a write)**;
**blocked = connect timeout, or EOF/reset before any data**. (A shell
probe cannot decide this: `head -c1` exits 0 on an empty EOF.)

The e2b probe runs `python3` in the lease (via the exec path): connect
with a 3s timeout, `sendall(b"P")`, `recv(1)` with a 2s timeout; it prints
exactly `blocked` (connect timeout/refused or reset, or EOF with no data)
or `ok` (any data byte, or recv timeout — silent-open = alive, e.g.
scylla 9042 waiting for CQL). Any other output or a non-zero exit is a
harness error, not a verdict.

**Port 443 is special: tcpproxy SNI routing.** 443 listeners route on the
TLS ClientHello, so the proxy needs a *parseable ClientHello* before any
allow/deny decision — a 1-byte write can never complete one, and a denied
443 destination therefore sits silent-open, which the write+read probe
would misread as reachable. For port 443 the probe performs a **real TLS
client handshake** (`socket.create_connection` 3s timeout;
`ssl.create_default_context()` with `check_hostname=False`,
`verify_mode=ssl.CERT_NONE`; wrap; then `recv(1)` 2s timeout):

- **reachable** ("ok"): the handshake established, **or the peer answered
  with a TLS alert** — an alert is a peer response, so bytes flowed
  bidirectionally through the egress path. SNI-strict servers alert on
  IP-literal hellos that carry no SNI (e.g. the LAN edge answering
  `tlsv1 alert internal error`); that rejection is not a policy block.
- **blocked** ("blocked"): connect failure/timeout, or a clean close
  before any peer bytes (EOF/reset — the egress path closed us).

Non-443 ports keep the write+read probe.

forkd (the substrate the baseline ran on) blocked at SYN, so the baseline
kept the plain connect probe
`timeout 5 bash -c '</dev/tcp/HOST/PORT' && echo yes || echo no`: a failed
connect was a denial.

## R2 netns semantics

`TestR2_OrchestratorCrash` compares **netns sets**, not counts. This is a
recorded deviation from the unit text ("the count equals `N0`"):
count-equality cannot hold on a real host.

- **Throwaway prefetch-harvest sandboxes** carry netns that die with a
  crash by design, so before/after counts drift while the actual netns
  set is untouched.
- **Historical debris** makes absolute counts meaningless.

Ops evidence during an R1–R3 window: the host netns *set* was identical
across the whole window (37/37 names, empty `comm` diff) while raw counts
read 43 vs 35.

The check therefore:

1. captures the sorted `ns-*` name list at test start (baseline `B`);
2. after the crash-recovery assertions, deletes the test's own leases and
   lets the host settle 5 s;
3. captures the sorted list `L` and fails only when `comm -13 B L` is
   non-empty — i.e. when a netns created during the crash window **leaks**
   after the test's own sandboxes are gone.

The failure message logs both counts and the leaked names.
