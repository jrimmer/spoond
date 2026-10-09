# DONE spoond-50m4: layer-1 firewall refuses non-TCP to host addresses

Found in the spoond-6s9 layer-3 review (2026-10-08). The fix is in the
orchestrator fork (`e2b-runtime`), branch `work/spoond-50m4` off
`origin/spoond` (`546fd529`), recorded there as P11.

## The hole

`orchestrator/pkg/sandbox/network/firewall.go` merged an `AllowedPrivate`
allowance such as `10.0.0.0/8` into `predefinedAllowSet`, and that rule
accepted every protocol. The layer-2 `tcpfirewall` host-address guard
(spoond-6s9) covered TCP only, so guest UDP and ICMP could reach the
host's veth IPs and host UDP services under a broad private allowance.

## The fix

Layer 1 now carries a `filtered_host_denylist` set built from the same
interface enumeration layer 2 guards. A non-TCP destination in that set
is dropped before the always-allow rule; TCP stays with the layer-2
port-scoped guard. The host set is captured in the host network
namespace (the enumeration lists the caller's interfaces) and refreshed
on create, configure, update and reset.

`tcpfirewall.hostAddrSet.HostAddrCIDRs` forces a fresh enumeration and
takes the blocking refresh path, so the one-shot `ApplyRules` never
builds the set from an expired or fail-closed snapshot. The layer-1 set
is IPv4 only and omits the unspecified `0.0.0.0/8` block, which the
interval-set builder cannot encode (it rejects an unspecified start):
leaving it in aborted the whole rule application.

The filter rule construction was split into pure builders so the
generated rule set is asserted without a netns or root.

## Test

`TestApplyRules_UDPToHostVethIPRefusedUnderPrivateAllowance` — under a
`10.0.0.0/8` allowance, UDP and ICMP to a host veth IP are dropped,
UDP/ICMP to a non-host `10.x` address are accepted, and TCP is left to
layer 2. Plus hostaddrs tests for the one-shot fresh enumeration, the
in-flight-refresh wait, and the IPv4 filtering.

## Race fix (rounds 3–4)

google/nftables v0.3.0 allocates set IDs from an unsynchronised
package-level counter (`set.go: allocSetID++`), so the two new tests
raced under `-race` when they allocated nftables sets in parallel. Both
tests now run without `t.Parallel` (marked `//nolint:paralleltest` like
`firewall_maxboundary_test.go`), including the subtests of the named
regression, and every set-building call site in the fork is serialized
behind one process-wide mutex (`WithNftablesSetLock`) in the network
package: the slot firewall's five `set.New` calls and the v2 host
firewall's two `AddSet` calls. In production the slot firewall is built
by the single pool `Populate` goroutine, but the v2 host firewall builds
its sets on a startup goroutine, so the counter is reachable
concurrently; the guard covers both rather than relying on that
scheduling.

`TestNewFirewall_ConcurrentSetCreation` builds several firewalls on
separate connections at once, so the lock — not scheduling — is what
keeps the counter race-free. With the lock temporarily removed the test
fails `-race` on `set.go:505`, evidence the test actually exercises the
race. `go test -race -count=10` on the two host-deny tests plus the
concurrency test passes 10/10, as root and as a non-root user (with
`HOME`/`GOCACHE` under `/tmp`, `GOMODCACHE=/opt/gomod`). Race output tail:

```
=== RUN   TestFilterRules_HostDropPrecedesAllow
--- PASS: TestFilterRules_HostDropPrecedesAllow (0.00s)
=== RUN   TestNewFirewall_ConcurrentSetCreation
--- PASS: TestNewFirewall_ConcurrentSetCreation (0.00s)
PASS
ok  github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/network 1.096s
```

## Gates

- `go build ./...` — clean
- `go vet ./pkg/sandbox/network/...` — clean
- `gofmt -l` on the touched packages — empty
- `go test -race -count=10` on the named host-deny tests and the
  concurrency test — pass 10/10 (root and non-root)
- the touched package tests pass; the 5 DSCP failures, the v2
  forward-probe failure and the rootless-Docker container test are
  pre-existing environment-only (missing kernel DSCP module / netns
  capture / no rootless Docker), confirmed on `origin/spoond`
- the new tests use `t.TempDir()`/no `/work`, `/run/honey` or `/opt/honey`
  path, and run as a non-root user
- live firewall on no host was changed

## Fork commit

`work/spoond-50m4` in `e2b-runtime`, one squashed commit (P11), SHA
`6c8eaf36354922bab756fb79b68907208b160f03` (round 4; supersedes
`a107cc3dc5bfbee9df1776c6b5175a1ebf61b26e`). `origin/work/spoond-50m4`
was force-pushed with `--force-with-lease` to this SHA.
