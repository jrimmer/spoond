# Conformance results

Notes on running the suite and interpreting its results files
(`results/`, committed baseline `baseline-forkd.json`). How to run the
suite is in `README.md`.

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
