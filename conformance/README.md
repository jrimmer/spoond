# spoond conformance suite

The executable definition of "done" for the substrate project (U02). It
exercises spoond **through the lease API** — the compatibility contract —
plus a few host-level checks (`hostRun`). It runs against the E2B
substrate (U12 cutover, 2026-10-01).

Every file in this package carries `//go:build conformance`, so a plain
`go test ./...` never builds or runs it.

## Running

The suite runs **on the host as root** from the checkout `/root/src/spoond`
(host checks, loopback proxy and gateway access), with
`CONFORMANCE_SSH=local`.

```bash
export PATH=/usr/local/go/bin:$PATH
test -d /root/src/spoond || git clone https://git.example.com/example/spoond.git /root/src/spoond
# BRANCH is the branch under test: main for everything since the U12
# cutover; a feature branch while its unit is in flight.
cd /root/src/spoond && git fetch && git checkout "$BRANCH" && git pull --ff-only
set -a; . /etc/spoond/conformance.env; set +a
CONFORMANCE_SUBSTRATE=e2b CONFORMANCE_GUEST_SERVICE=10.0.0.11:8891 \
go test -tags conformance -count=1 -timeout 90m -v ./conformance/ \
  -args -results "$PWD/conformance/results/$(date +%Y%m%dT%H%M%S)-$CONFORMANCE_SUBSTRATE.json"
```

Per-run variables: `CONFORMANCE_SUBSTRATE` (`e2b`),
`CONFORMANCE_GUEST_SERVICE`, `CONFORMANCE_DESTRUCTIVE` (unset/absent
enables nothing; `1` enables group R, which is an Autonomous window step),
`CONFORMANCE_CAPACITY` (unset/absent enables nothing; `1` enables group C,
which fills the host), `CONFORMANCE_CRASH_TEST` (unset/absent enables
nothing; `1` enables the crash-test case X1).
The `-results` path must be absolute: `go test` runs the test binary in the
package directory. To run a single test, add `-run '^<TestName>$'` before
`-args`.

Group R runs only with `CONFORMANCE_DESTRUCTIVE=1`; it restarts backend,
gateway and orchestrator units, so it never runs against production traffic.

Group C (preemption, #128 part 3) runs only with `CONFORMANCE_CAPACITY=1`
because it fills the host. It also needs `CONFORMANCE_SECOND_TOKEN`, the
token of a second, non-admin identity user: it creates the guaranteed
lease as that second user (the conformance user is not admin and can
neither create users nor set quotas), so it must be provisioned on the
host like `CONFORMANCE_TOKEN`.

The crash-test case X1 (`TestX1_CrashTestRecovers`) runs only with
`CONFORMANCE_CRASH_TEST=1`, against a backend started with
`CRASH_TEST=1` (the route answers 404 otherwise). It uses the ordinary
conformance token: it crashes and recovers only leases it created
itself, so no admin token is needed.

The named-snapshot case S7 (`TestS7_NamedSnapshots`, 2.7, #83) is always
on and non-destructive: it creates and deletes only the leases and the
snapshot name it uses. It saves a lease that wrote a marker file,
replays the save, starts a lease from the snapshot (checking the marker,
the new `/run/spoond/lease-id`, that `/run/secrets` holds only the new
lease's create-time secrets, and that the guest clock is within 1 s of
the host), then checks retention with `keep: 1`, the in-use delete
`409`, and the `204` delete. Every lease and the name are cleaned up,
also on failure.

## Configuration

See `U02-conformance-suite.md` §Configuration for the full table. Required:
`CONFORMANCE_API`, `CONFORMANCE_TOKEN`, `CONFORMANCE_SSH`,
`CONFORMANCE_SUBSTRATE`, `CONFORMANCE_BACKEND_UNIT`,
`CONFORMANCE_SSH_GATEWAY`, `CONFORMANCE_SSH_KEY`, `CONFORMANCE_USER`,
`CONFORMANCE_USER_ID`, `CONFORMANCE_PROXY_URL`,
`CONFORMANCE_PROXY_SECRET` (may be empty), `CONFORMANCE_PROXY_SUFFIX`,
`CONFORMANCE_GUEST_SERVICE`. Optional: `CONFORMANCE_DESTRUCTIVE`,
`CONFORMANCE_CAPACITY`, `CONFORMANCE_SECOND_TOKEN`,
`CONFORMANCE_CRASH_TEST`,
`CONFORMANCE_LAN_TARGET` (a private `host:port` N1 probes under the
`internet` and `lan` policies; **no default**, so N1 skips when unset),
`CONFORMANCE_IMAGES` (default
`py-base,go-base,dev-base,elixir-base,elixir-release,llm-review,scylla`).

## Results

TestMain writes a JSON array of
`{"test","status":"pass|fail|skip","duration_ms","metrics":{...},"error"}`
to the `-results` path (default
`results/<YYYYMMDDTHHMMSS>-<CONFORMANCE_SUBSTRATE>.json` relative to this
directory), creating the parent directory if needed. E2B run results are
committed by the unit that runs them (`docs/plans/2026-09-30-e2b-substrate/RESULTS.md`).
