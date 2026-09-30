# spoond conformance suite

The executable definition of "done" for the substrate project (U02). It
exercises spoond **through the lease API** — the compatibility contract —
plus a few host-level checks (`hostRun`). It runs against forkd (baseline)
and against E2B (U12).

Every file in this package carries `//go:build conformance`, so a plain
`go test ./...` never builds or runs it.

## Running

The suite runs **on vm2 as root** from the checkout `/root/src/spoond`
(host checks, loopback proxy and gateway access), with
`CONFORMANCE_SSH=local`.

```bash
export PATH=/usr/local/go/bin:$PATH
test -d /root/src/spoond || git clone https://code.lacy.casa/lacy.casa/spoond.git /root/src/spoond
# BRANCH is the branch the unit under test names: feat/e2b-substrate for
# U02 and U06–U11; main for U05's R3 run and for U12.
cd /root/src/spoond && git fetch && git checkout "$BRANCH" && git pull --ff-only
set -a; . /etc/spoond/conformance.env; set +a   # staging: /etc/spoond-staging/conformance.env
CONFORMANCE_SUBSTRATE=forkd CONFORMANCE_GUEST_SERVICE=10.43.0.1:8891 \
go test -tags conformance -count=1 -timeout 60m -v ./conformance/ \
  -args -results "$PWD/conformance/results/$(date +%Y%m%dT%H%M%S)-$CONFORMANCE_SUBSTRATE.json"
```

Per-run variables: `CONFORMANCE_SUBSTRATE` (`forkd` or `e2b`),
`CONFORMANCE_GUEST_SERVICE`, `CONFORMANCE_DESTRUCTIVE` (unset/absent
enables nothing; `1` enables group R, which is an Autonomous window step).
The `-results` path must be absolute: `go test` runs the test binary in the
package directory. To run a single test, add `-run '^<TestName>$'` before
`-args`.

Group R never runs against forkd except R3 alone
(`-run '^TestR3_BackendRestart$'`, U05).

## Configuration

See `U02-conformance-suite.md` §Configuration for the full table. Required:
`CONFORMANCE_API`, `CONFORMANCE_TOKEN`, `CONFORMANCE_SSH`,
`CONFORMANCE_SUBSTRATE`, `CONFORMANCE_BACKEND_UNIT`,
`CONFORMANCE_SSH_GATEWAY`, `CONFORMANCE_SSH_KEY`, `CONFORMANCE_USER`,
`CONFORMANCE_USER_ID`, `CONFORMANCE_PROXY_URL`,
`CONFORMANCE_PROXY_SECRET` (may be empty), `CONFORMANCE_PROXY_SUFFIX`,
`CONFORMANCE_GUEST_SERVICE`. Optional: `CONFORMANCE_DESTRUCTIVE`,
`CONFORMANCE_IMAGES` (default
`py-base,go-base,dev-base,elixir-base,elixir-release,llm-review,scylla`).

## Results

TestMain writes a JSON array of
`{"test","status":"pass|fail|skip","duration_ms","metrics":{...},"error"}`
to the `-results` path (default
`results/<YYYYMMDDTHHMMSS>-<CONFORMANCE_SUBSTRATE>.json` relative to this
directory), creating the parent directory if needed. The committed forkd
baseline is `baseline-forkd.json`.
