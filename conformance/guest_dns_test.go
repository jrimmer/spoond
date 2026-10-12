//go:build conformance

package conformance

// Guest networking beyond the egress probe (spoond-ndh): the lease API
// from inside leases, the guest's DNS, and a mixed restricted allowlist
// reached over the guest's own resolver. The bare reachability probing
// is TestN1's job; this file covers
//
//   - TestN10_LeaseAPIFromGuests — the lease API is reachable from an
//     `internet` and a `lan` lease (healthz without auth, then create
//     and delete with the test token) and unreachable from a
//     `restricted` one (docs/api.md §Network policy: "lan and internet
//     guests may additionally reach the lease API itself ...; restricted
//     and none may not").
//   - TestN12_GuestDNS — under every policy the guest's
//     /etc/resolv.conf names no public resolver, and the deployment's
//     wildcard-zone name (e.g. the API host under *.LAN zone) resolves
//     to the expected LAN address.
//   - TestN13_MixedAllowlistViaGuestDNS — a restricted lease whose
//     allowlist mixes a LAN host (with its own wildcard-zone name) and
//     a public domain reaches both by name.
//
// Every LAN assumption is behind CONFORMANCE_LAN_DNS: the LAN address
// the guest resolver must answer for a wildcard-zone name. It has no
// default, like CONFORMANCE_LAN_TARGET — the cases skip with a clear
// message when it is unset, so a run never probes an assumed LAN
// address.

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"
)

// requireLanDNS gates the guest-DNS cases on CONFORMANCE_LAN_DNS, the
// LAN address a guest resolver answers for a *.CONFORMANCE_LAN_DNS_ZONE
// name. It has no default: unset, the cases skip (same rule as
// CONFORMANCE_LAN_TARGET in N1), so a run never probes an assumed LAN
// address.
func requireLanDNS(t *testing.T) {
	if cfg.LANDNS == "" {
		skipf(t, "CONFORMANCE_LAN_DNS is unset; set it to the LAN address the guest resolver answers for the wildcard zone (e.g. the host's own address; sb does) to exercise the guest-side lease API and DNS")
	}
}

// apiGuestURL returns the API base URL and its host part for the
// guest-side probes. The guest reaches the same base URL the harness
// does (the production env names the host's public API name, which the
// guest resolver answers from the LAN).
func apiGuestURL(t *testing.T) (base, host string) {
	base = strings.TrimRight(strings.TrimSpace(cl.base), "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		failf(t, "CONFORMANCE_API %q does not parse into a URL with a host", cfg.API)
	}
	return base, u.Hostname()
}

// lanCIDRAddr reports whether addr is an IPv4 address in the spoond-free
// part of the private 10.x space: any 10.x outside the lease networks
// 10.11/16 and 10.12/16 (api/service.go's lanRanges is RFC 1918 minus
// the lease networks) — the shape of the LAN host address a guest
// resolver answers with.
func lanCIDRAddr(addr string) bool {
	ip := parseIPv4(addr)
	if ip == nil || ip[0] != 10 {
		return false
	}
	// A sandbox's own address lives in 10.11/16 or 10.12/16; the host
	// LAN never does.
	return ip[1] != 11 && ip[1] != 12
}

// parseIPv4 parses a dot-decimal IPv4 address into 4 bytes, nil on
// anything else.
func parseIPv4(s string) []byte {
	var b [4]byte
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return nil
	}
	for i, p := range parts {
		if p == "" || len(p) > 3 {
			return nil
		}
		n := 0
		for _, c := range []byte(p) {
			if c < '0' || c > '9' {
				return nil
			}
			n = n*10 + int(c-'0')
			if n > 255 {
				return nil
			}
		}
		if len(p) > 1 && p[0] == '0' {
			return nil
		}
		b[i] = byte(n)
	}
	return b[:]
}

// guestResolve resolves host in the lease through the guest's resolver
// and returns the address, retrying while the resolver itself is not
// yet reachable through the policy grants (a fresh sandbox's egress
// config applies at once, but the first lookups may race the guest's
// resolver allowance). The answer "ERR:<err>" on a failed lookup after
// the window.
func guestResolve(t *testing.T, id, host string) string {
	// getaddrinfo through the guest's /etc/resolv.conf (libc), AF_INET:
	// the question is what a normal guest tool (curl, python) resolves,
	// not what a raw DNS client does.
	script := `python3 -c '
import socket, sys
try:
    infos = socket.getaddrinfo(sys.argv[1], None, socket.AF_INET)
    print(infos[0][4][0])
except Exception as e:
    print("ERR:" + str(e))
' ` + host
	deadline := time.Now().Add(15 * time.Second)
	for {
		out := execOK(t, id, script)
		if !strings.HasPrefix(out, "ERR:") {
			return out
		}
		if time.Now().After(deadline) {
			return out
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// resolveExpectLAN resolves host in the lease and requires the answer to
// be cfg.LANDNS. Used wherever the guest must get the LAN address, not
// the public edge.
func resolveExpectLAN(t *testing.T, id, policy, host string) {
	got := guestResolve(t, id, host)
	if got != cfg.LANDNS {
		failf(t, "%s: guest resolves %s to %q, want %q (the LAN address)", policy, host, got, cfg.LANDNS)
	}
}

// guestAPICode runs one guest-side HTTP probe of the API's /healthz and
// returns the HTTP code curl reports ("000" when the egress path or the
// connect fails).
func guestAPICode(t *testing.T, id, apiBase string) string {
	script := "curl -sk --max-time 8 -o /dev/null -w '%{http_code}' " + apiBase + "/healthz 2>/dev/null"
	return strings.TrimSpace(execOK(t, id, script))
}

// guestLeaseRoundTrip creates and deletes a lease from inside the lease
// `from`, with the test token, through apiBase. The body is not
// interpolated into the guest's shell: the token travels as one
// exec-time secret, the guest script reads it from its /run/secrets
// file. It answers whether both calls worked; the created lease is
// deleted from inside too, and the caller watches its own count (a
// left-over create shows as an extra lease at the end of the test, so
// the caller's delta check turns both paths red).
func guestLeaseRoundTrip(t *testing.T, from, apiBase string) bool {
	t.Helper()
	script := `
set -e
TOK=$(cat /run/secrets/CONFORMANCE_TOKEN)
api='` + apiBase + `'
resp=$(curl -sk --max-time 15 -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"image":"py-base","ttl":120}' "$api/api/sandboxes")
id=$(printf '%s' "$resp" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
[ -n "$id" ] || { echo "no id in create answer: $resp" >&2; exit 3; }
printf 'created:%s\n' "$id"
code=$(curl -sk --max-time 15 -o /dev/null -w '%{http_code}' -X DELETE \
  -H "Authorization: Bearer $TOK" "$api/api/sandboxes/$id")
[ "$code" = 204 ] || { echo "delete answered $code" >&2; exit 4; }
echo deleted
`
	st, body, err := cl.exec(from, execReq{Cmd: script, Secrets: guestTokenSecret()})
	if err != nil {
		failf(t, "guest lease round-trip exec in %s: %v", from, err)
	}
	if st != 200 {
		failf(t, "guest lease round-trip exec in %s: status %d: %s", from, st, truncate(body))
	}
	var out execResult
	if err := json.Unmarshal(body, &out); err != nil {
		failf(t, "guest lease round-trip in %s: bad body: %v", from, err)
	}
	if out.Exit != 0 {
		failf(t, "guest lease round-trip in %s: exit %d: stderr: %s", from, out.Exit, strings.TrimSpace(out.Stderr))
	}
	return strings.Contains(out.Stdout, "deleted")
}

// guestTokenSecret names the exec-time secret the guest-side lease
// round-trip reads the test token from: a file under /run/secrets,
// never env or argv (docs/api.md §Secrets).
func guestTokenSecret() map[string]string {
	return map[string]string{"CONFORMANCE_TOKEN": cfg.Token}
}

// TestN10_LeaseAPIFromGuests checks the lease API route from inside
// leases (docs/api.md §Network policy): under `internet` and `lan` a
// guest reaches GET /healthz on the API's own base URL and can create
// and delete a lease with the test token (the CI-job-leasing-a-database
// path); under `restricted` the API is unreachable. The guest resolves
// the API's host to the LAN address first: that is the precondition of
// reaching the API at all from inside.
func TestN10_LeaseAPIFromGuests(t *testing.T) {
	rec := begin(t)

	requireLanDNS(t)

	apiBase, apiHost := apiGuestURL(t)
	rec.set("api", apiBase)

	// The API serves /healthz without auth; check it from the harness so
	// a guest-side 000 below cannot be confused with an API outage.
	st, body, err := cl.do("GET", "/healthz", nil)
	if err != nil {
		failf(t, "healthz: %v", err)
	}
	if st != 200 {
		failf(t, "healthz: status %d: %s", st, truncate(body))
	}

	for _, tc := range []struct {
		policy   string
		apiOK    bool
		leaseOK  bool
		allow    []string
		allowMsg string
	}{
		{policy: "internet", apiOK: true, leaseOK: true, allowMsg: "internet needs no allowlist"},
		{policy: "lan", apiOK: true, leaseOK: true, allowMsg: "lan needs no allowlist"},
		{policy: "restricted", apiOK: false, leaseOK: false, allow: []string{cfg.MixedDomain}, allowMsg: "the public-domain allowlist keeps the API out"},
	} {
		l := createLease(t, map[string]any{
			"image":            "py-base",
			"ttl":              600,
			"network_policy":   tc.policy,
			"egress_allowlist": tc.allow,
		})

		// The guest resolver answers the API's host name from the LAN.
		resolveExpectLAN(t, l.ID, tc.policy, apiHost)

		// healthz: no auth involved, so the code is the whole verdict.
		code := guestAPICode(t, l.ID, apiBase)
		rec.set("healthz:"+tc.policy, code)
		switch {
		case tc.apiOK && code != "200":
			failf(t, "%s: GET %s/healthz from inside answered %q, want 200 (%s)", tc.policy, apiBase, code, tc.allowMsg)
		case !tc.apiOK && code == "200":
			failf(t, "%s: GET %s/healthz from inside answered 200, want unreachable (%s)", tc.policy, apiBase, tc.allowMsg)
		}

		// The create/delete round-trip with the test token.
		if tc.apiOK {
			if !guestLeaseRoundTrip(t, l.ID, apiBase) {
				failf(t, "%s: create+delete with the test token from inside did not round-trip", tc.policy)
			}
			rec.set("lease_round_trip:"+tc.policy, "ok")
		}
	}
}

// TestN12_GuestDNS checks the guest's resolver under every network
// policy: /etc/resolv.conf carries no public resolver (deployment DNS
// routes through the private resolvers spoond grants; a public answer
// would hand the wildcard zone to a public edge), and a wildcard-zone
// name resolves to the expected LAN address.
func TestN12_GuestDNS(t *testing.T) {
	rec := begin(t)

	requireLanDNS(t)

	_, apiHost := apiGuestURL(t)
	name := apiHost
	rec.set("name", name)
	rec.set("zone", cfg.LanDNSZone)

	// resolverEntries reads the guest's /etc/resolv.conf nameserver
	// lines.
	resolverEntries := func(id string) []string {
		out := execOK(t, id, "grep -s '^[[:space:]]*nameserver[[:space:]]' /etc/resolv.conf || true")
		var addrs []string
		for _, line := range strings.Split(out, "\n") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				addrs = append(addrs, f[1])
			}
		}
		return addrs
	}
	// isPublicResolver reports whether addr is a resolver outside the
	// private space (a public DNS would route the wildcard zone away
	// from the LAN).
	isPublicResolver := func(addr string) bool {
		ip := parseIPv4(addr)
		if ip == nil {
			return true // not even an IPv4 address: report so
		}
		return ip[0] != 10 && !addrIsPrivateNon10(ip)
	}

	for _, policy := range []string{"none", "restricted", "lan", "internet"} {
		l := createLease(t, map[string]any{
			"image":          "py-base",
			"ttl":            600,
			"network_policy": policy,
		})

		// No public resolver in /etc/resolv.conf. The guest's own file is
		// the deployment's DNS story for every policy; the egress policy
		// may still block the DNS service itself under `none` — the file
		// check is about the resolver list, not about reachability. A
		// nameserver line naming the configured guest resolver
		// (CONFORMANCE_LAN_DNS, where it matches) is the private answer.
		for _, ns := range resolverEntries(l.ID) {
			if isPublicResolver(ns) {
				failf(t, "%s: /etc/resolv.conf names the public resolver %q (want only private spoond resolvers: %s)", policy, ns, truncate([]byte(strings.Join(resolverEntries(l.ID), ", "))))
			}
		}
		rec.set("resolvers:"+policy, strings.Join(resolverEntries(l.ID), ","))

		// The wildcard-zone name resolves to the LAN address. Under
		// `none` the DNS route itself is granted (every policy keeps DNS)
		// — docs/api.md: every policy carries "DNS" in its egress.
		resolveExpectLAN(t, l.ID, policy, name)
		rec.set("resolve:"+policy, cfg.LANDNS)
	}
}

// TestN13_MixedAllowlistViaGuestDNS pins the mixed allowlist reached
// through the guest's resolver: a restricted lease allowlisting a LAN
// host and a public domain reaches both by name. The LAN host is
// CONFORMANCE_LAN_TARGET (N1's target, `host:port`): its address is the
// LAN IP the lease allowlists, and its wildcard-zone name (the same
// host under the deployment's zone) rides the allowlist as a domain so
// the guest resolves it through the private resolver. Skips with
// CONFORMANCE_LAN_TARGET/CONFORMANCE_LAN_DNS unset.
func TestN13_MixedAllowlistViaGuestDNS(t *testing.T) {
	rec := begin(t)

	if cfg.LANTarget == "" {
		skipf(t, "CONFORMANCE_LAN_TARGET is unset; set it to a private host:port that answers on the host (sb does) to exercise the mixed allowlist")
	}
	requireLanDNS(t)

	lanHost, lanPort := cfg.LANTargetHost, cfg.LANTargetPort
	if !lanCIDRAddr(lanHost) {
		skipf(t, "CONFORMANCE_LAN_TARGET %q does not name a LAN address the guest resolver can carry; set a private host:port", cfg.LANTarget)
	}

	// The wildcard-zone name of the LAN host: what the guest resolver
	// must carry this lease to.
	name := lanHost + "." + cfg.LanDNSZone
	rec.set("name", name)

	allow := []string{lanHost, cfg.MixedDomain}
	l := createLease(t, map[string]any{
		"image":            "py-base",
		"ttl":              900,
		"network_policy":   "restricted",
		"egress_allowlist": allow,
	})

	// The guest resolves the LAN host's zone name to the LAN address.
	resolveExpectLAN(t, l.ID, "restricted", name)

	// Both are reachable by name: the mixed allowlist's IP and domain
	// halves, with the IP entry named as its zone name (the guest's
	// resolver must not break either path).
	if !canTCP(t, l.ID, lanHost, lanPort) {
		failf(t, "mixed allowlist: LAN IP %s:%d blocked, want reachable", lanHost, lanPort)
	}
	if !canTCP(t, l.ID, name, lanPort) {
		failf(t, "mixed allowlist: zone name %s:%d blocked, want reachable", name, lanPort)
	}
	if !canTCP(t, l.ID, cfg.MixedDomain, 443) {
		failf(t, "mixed allowlist: domain %s blocked, want reachable", cfg.MixedDomain)
	}

	// An unlisted private address stays blocked.
	if canTCP(t, l.ID, "10.99.99.99", 443) {
		failf(t, "mixed allowlist: unlisted private 10.99.99.99:443 reachable, want blocked")
	}
}

// addrIsPrivateNon10 reports the private-but-not-10 shape (172.16/12,
// 192.168/16) the guest resolvers may also deploy on.
func addrIsPrivateNon10(ip []byte) bool {
	if ip[0] == 172 && ip[1] >= 16 && ip[1] < 32 {
		return true
	}
	return ip[0] == 192 && ip[1] == 168
}
