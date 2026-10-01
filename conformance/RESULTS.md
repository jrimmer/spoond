# Conformance results

Notes on running the suite and interpreting its results files
(`results/`, committed baseline `baseline-forkd.json`). How to run the
suite is in `README.md`.

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

On forkd the plain connect probe
`timeout 5 bash -c '</dev/tcp/HOST/PORT' && echo yes || echo no` is kept:
forkd blocks at SYN, so a failed connect is a denial.
