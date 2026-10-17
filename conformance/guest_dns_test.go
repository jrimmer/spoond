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
// LAN address a guest resolver answers for the API's host name. It has no default: unset, the cases skip (same rule as
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
// connect fails). curl exits non-zero on a refused path (35 when the
// egress proxy drops a restricted guest's handshake, 7 on a refused
// connect), which is the expected answer under restricted, so the exec
// itself must not fail on it.
func guestAPICode(t *testing.T, id, apiBase string) string {
	script := "curl -sk --max-time 8 -o /dev/null -w '%{http_code}' " + apiBase + "/healthz 2>/dev/null || true"
	return strings.TrimSpace(execOK(t, id, script))
}

// guestLeaseRoundTrip creates and deletes a lease from inside the lease
// `from`, with the test token, through apiBase. The body is not
// interpolated into the guest's shell: the token travels as one
// exec-time secret, the guest script reads it from its /run/secrets
// file. It answers whether both calls worked; the created lease is
// deleted from inside too (a failed delete fails the exec, and the
// lease's 120 s TTL bounds any leftover).
func guestLeaseRoundTrip(t *testing.T, from, apiBase string) bool {
	t.Helper()
	script := `
set -e
TOK=$(cat /run/secrets/CONFORMANCE_TOKEN)
api='` + apiBase + `'
resp=$(curl -sk --max-time 15 -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"image":"py-base","ttl":120}' "$api/api/leases")
id=$(printf '%s' "$resp" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
[ -n "$id" ] || { echo "no id in create answer: $resp" >&2; exit 3; }
printf 'created:%s\n' "$id"
code=$(curl -sk --max-time 15 -o /dev/null -w '%{http_code}' -X DELETE \
  -H "Authorization: Bearer $TOK" "$api/api/leases/$id")
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

		// The wildcard-zone name resolves to the LAN address. `none` has
		// no egress at all, DNS included (docs/api.md §Network policy),
		// so only its resolver list is checked.
		if policy == "none" {
			continue
		}
		resolveExpectLAN(t, l.ID, policy, name)
		rec.set("resolve:"+policy, cfg.LANDNS)
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
