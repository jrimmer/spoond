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

## Gates

- `go build ./...` — clean
- `go vet ./pkg/sandbox/network/... ./pkg/tcpfirewall/...` — clean
- `gofmt -l` on the touched packages — empty
- the touched package tests pass; the 5 DSCP failures and the v2
  forward-probe failure are pre-existing environment-only (missing
  kernel DSCP module / netns capture), confirmed on `origin/spoond`
- the new tests pass as a non-root user with no `/work` path involved
- live firewall on no host was changed

## Fork commit

`work/spoond-50m4` in `e2b-runtime`, one squashed commit (P11).
