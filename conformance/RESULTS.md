# Forkd baseline: recorded deviation causes

Purpose: U12 compares its E2B conformance run against the committed forkd
baseline (`conformance/baseline-forkd.json`). This file records **why** the
baseline deviates from a green run, so those numbers are compared against
understood causes, not treated as regressions or as targets to reproduce.

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
vm2's own addresses; only granted ports reach host services. The committed
baseline corroborates this: `conformance/baseline-forkd.json` records
`TestN1_Policies` as `fail` with error
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

On e2b, denied destinations close at the data phase: E2B's transparent
egress proxy accepts every guest `connect()` locally, and a destination
the policy denies is closed (EOF/reset) only once data phase begins —
never at the SYN. The suite's `canTCP` probe is therefore substrate-aware:

- **e2b** — probe
  `timeout 3 bash -c 'exec 3<>/dev/tcp/HOST/PORT && head -c 1 <&3'`;
  exit 0 (a byte arrived) or 124 (timeout — connection still open, silent
  server) means **reachable**; any other exit (1/2 — EOF or reset, i.e.
  the proxy closed us) means **blocked**.
- **forkd** — the plain connect probe
  `timeout 5 bash -c '</dev/tcp/HOST/PORT' && echo yes || echo no`:
  forkd blocks at SYN, so a failed connect is a denial.
