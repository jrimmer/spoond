# DONE spoond-ndh: conformance for the lease API from guests, guest DNS, mixed restricted allowlist

Branch `work/spoond-ndh`. Three new conformance cases in
`conformance/guest_dns_test.go` (build tag `conformance`), next to the
existing group N cases in `conformance/network_test.go`, following their
helpers (`createLease`, `execOK`, `canTCP`, `begin`, `skipf`, `rec.set`)
and env vars (`CONFORMANCE_API`, `CONFORMANCE_TOKEN`,
`CONFORMANCE_GUEST_SERVICE`).

## 1. `TestN10_LeaseAPIFromGuests` — the lease API route from inside

docs/api.md §Network policy: "`lan` and `internet` guests may additionally
reach the lease API itself on the host service address ...; `restricted`
and `none` may not." The case:

- For `internet`, `lan` and `restricted` (with the public-domain
  allowlist, so the restricted refusal is not an empty-allowlist 400),
  one `py-base` lease each.
- Each guest first resolves the API base URL's host (the harness
  interpolates `CONFORMANCE_API`, which names the host's API name): the
  assertion is that the guest's resolver answers the LAN address, i.e.
  the precondition of reaching the host service address at all from
  inside.
- Then, from inside:
  - `curl -sk --max-time 8 $API/healthz` prints the HTTP code. `200` for
    `internet` and `lan` (healthz carries no auth, so the code is the
    whole verdict), `000` (curl's refused/timeouted connect) for
    `restricted`.
  - For the two granted policies, a create+delete round-trip with the
    test token through the same base URL. The token rides an **exec-time
    secret** (`POST …/exec` `secrets`, docs/api.md §Secrets: files under
    `/run/secrets`, mode 0600, removed afterwards — never env, never
    argv), and the guest script reads it from
    `/run/secrets/CONFORMANCE_TOKEN`; the shell script itself carries
    only the base URL. `execReq` gained the `Secrets` field (conformance
    `client.go`).

## 2. `TestN12_GuestDNS` — the guest resolver under every policy

docs/api.md: every policy's egress grants DNS; the guest image resolves
only through the spoond resolvers (`SPOOND_GUEST_DNS_ADDR`), with no
public fallback. The case creates one lease per policy (`none`,
`restricted`, `lan`, `internet`) and asserts, from inside:

- `/etc/resolv.conf` carries no public resolver: every `nameserver`
  address parses as an IPv4 address in the private space (10/8 or
  172.16/12 or 192.168/16).
- A wildcard-zone name — the API base's host, the same name the lease
  API checks resolve — resolves to exactly the expected LAN address
  (`getaddrinfo` AF_INET through the guest's resolv.conf, asserted
  `== CONFORMANCE_LAN_DNS`).

## 3. `TestN13_MixedAllowlistViaGuestDNS` — mixed allowlist, by name

The spoond-4pa regression (N9) probes a mixed allowlist of a private IP
and a public domain. This case does the same through the guest's
resolver: the allowlist mixes N1's LAN target (`CONFORMANCE_LAN_TARGET`,
`host:port` — its address is the LAN IP) and the public domain, and the
guest reaches both, plus the LAN host's own wildcard-zone name (resolved
through the private resolver to the LAN address). An unlisted private
address stays blocked. Shares N9's shape and its helpers.

## Configuration (all read in `harness_test.go loadConfig`)

Both new variables have **no LAN default** and the cases **skip with a
clear message when unset** (the same rule as N1's
`CONFORMANCE_LAN_TARGET` and N9's `CONFORMANCE_MIXED_*` knobs — a run
never probes an assumed LAN address):

- `CONFORMANCE_LAN_DNS` — the LAN address a guest resolver answers for
  the wildcard zone (on sb: `10.1.0.11`, the host service address).
  Gates N10, N12 and N13.
- `CONFORMANCE_LAN_DNS_ZONE` — the deployment's wildcard zone the guest
  resolver resolves from the LAN (default `example.com`; on sb:
  `lacy.casa`). Set it where the zone is configured.

Existing variables used unchanged: `CONFORMANCE_API` (required; its host
is the name the guest resolves and curls), `CONFORMANCE_TOKEN` (the
round-trip's secret, exec-time), `CONFORMANCE_LAN_TARGET` (N13's LAN
host and N9's), `CONFORMANCE_MIXED_DOMAIN` (the public-domain half,
default `example.com`), `CONFORMANCE_GUEST_SERVICE` (untouched, N1/N6).

## Production run (window)

The suite runs on the host from `/root/src/spoond` with `/etc/spoond/conformance.env`:

```bash
set -a; . /etc/spoond/conformance.env; set +a
CONFORMANCE_SUBSTRATE=e2b CONFORMANCE_GUEST_SERVICE=10.1.0.11:8891 \
CONFORMANCE_LAN_DNS=10.1.0.11 CONFORMANCE_LAN_DNS_ZONE=lacy.casa \
go test -tags conformance -count=1 -run \
  '^TestN(10_LeaseAPIFromGuests|12_GuestDNS|13_MixedAllowlistViaGuestDNS)$' \
  -timeout 20m -v ./conformance/
```

(sb's `CONFORMANCE_LAN_TARGET=10.0.0.x:443` value must name a host the
guest's resolver carries as `10.1.0.x.lacy.casa`-style; when it does
not, N13 skips with its reason — always a valid outcome.)

## Gates

- `go build ./...` — clean
- `go vet ./...` — clean
- `gofmt -l .` — empty
- `go vet -tags conformance ./conformance/` — clean
- `go test ./...` — all pass (the conformance package is not built by it)
- `go test -tags conformance -c ./conformance/ -o /dev/null` — compiles
- No-env skips proven (with the required env set and all optional knobs
  unset): N10 and N12 skip "CONFORMANCE_LAN_DNS is unset; …", N13 skips
  "CONFORMANCE_LAN_TARGET is unset; …"
- Also run as a non-root user (`user`, `HOME=/tmp/fakehome`,
  `GOCACHE=/tmp/fakehome/gocache`, no root reads) — the cases skip
  cleanly the same way.