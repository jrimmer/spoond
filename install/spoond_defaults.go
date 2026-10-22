// The spoond phase's item 1 defaults, resolved by query (never exec, so
// they work in a dry run): the hostname to issue the certificate for,
// the host IP guests reach host services on, and the guest DNS
// resolver.
package install

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
)

// hostDefaults is item 1's answer: every row the spoond phase needs to
// render the files.
type hostDefaults struct {
	// hostName is the certificate and gateway name: --host-name,
	// else `hostname -f`.
	hostName string
	// hostIP is the address guests reach host services on
	// (HOST_GUEST_SERVICE_ADDR): the src of the default route.
	hostIP string
	// guestDNS is the first usable resolver, or "" with a note.
	guestDNS string
	// guestDNSNote says the resolver file's state when guestDNS is "".
	guestDNSNote string
}

// spoondDefaults resolves the defaults and adds their rows. Every
// lookup is a query (or a file read under Root), so a dry run resolves
// the same plan a real run would render.
func spoondDefaults(ctx context.Context, env *Env, r *Reporter) (hostDefaults, error) {
	var dd hostDefaults

	// hostname: --host-name, else `hostname -f`.
	if env.Opt.HostName != "" {
		dd.hostName = env.Opt.HostName
		r.Add(Kept, "host name", dd.hostName)
	} else {
		out, err := env.query(ctx, "hostname", "-f")
		if err != nil {
			return dd, fmt.Errorf("hostname -f: %w (pass --host-name)", err)
		}
		fqdn := strings.TrimSpace(string(out))
		if fqdn == "" {
			return dd, fmt.Errorf("hostname -f printed nothing (pass --host-name to set the TLS and gateway name)")
		}
		dd.hostName = fqdn
		r.Add(Created, "host name", "from hostname -f; pass --host-name to pin it to "+fqdn)
	}

	// host IP: the src of the IPv4 default route. A failed query
	// (no route: `ip route get` answers "Network is unreachable")
	// is the same outcome as a route with no src: no address to
	// tell the guests.
	ip, err := hostIP(ctx, env)
	switch {
	case err != nil:
		return dd, err
	case ip != "":
		dd.hostIP = ip
		r.Add(Created, "host IP", "default route's source address; pass --host-name only names the host, HOST_GUEST_SERVICE_ADDR in "+backendEnvPath+" overrides the IP")
	default:
		return dd, noRouteFailure(r)
	}

	// guest DNS: the first usable nameserver.
	resolver, note := guestResolver(env)
	dd.guestDNS, dd.guestDNSNote = resolver, note
	if resolver == "" {
		r.Add(Skipped, "guest DNS", note)
	} else {
		r.Add(Created, "guest DNS", resolver+" (the first usable nameserver in /etc/resolv.conf)")
	}
	return dd, nil
}

// hostIP is the src address of `ip -4 -o route get 1.1.1.1`, the classic
// "which address leaves by the default route" query. "(ip route get) fails
// on the real host only when no route (or source) answers; both shapes
// come back as ("", nil) — noRouteFailure's — and the route-without-src
// case (a route line without a src field) likewise.
func hostIP(ctx context.Context, env *Env) (string, error) {
	out, err := env.query(ctx, "ip", "-4", "-o", "route", "get", "1.1.1.1")
	if err != nil {
		// `ip route get` fails exactly when it cannot answer the
		// route (no default route, network unreachable): the "no
		// IPv4 default route" failure.
		return "", nil
	}
	fields := strings.Fields(string(out))
	for i, field := range fields {
		if field == "src" && i+1 < len(fields) {
			return fields[i+1], nil
		}
	}
	return "", nil
}

// noRouteFailure adds the failed host-IP row and returns the phase's
// error: the fix names the env file to set (there is no flag yet, so
// the operator edits /etc/spoond/backend.env and reruns).
func noRouteFailure(r *Reporter) error {
	note := errNoIPv4.Error() + " (" + backendEnvPath + ", then rerun)"
	r.Add(Failed, "host IP", note)
	return fmt.Errorf("%s", errNoIPv4)
}

// guestResolver scans resolv.conf for the first nameserver address that
// is not 127.* or ::1: the guests' resolver address, granted on port 53
// and baked into the guest image (docs/install.md section 4). A
// loopback-only or missing file yields ("", note): no resolver
// allowance, the note says so.
func guestResolver(env *Env) (string, string) {
	full := env.Resolve(resolvConfPath)
	if _, err := os.Stat(full); err != nil && env.Opt.Container {
		// The init service writes under --root ./config, which has
		// no resolv.conf: the container's own file is the host's
		// under host networking.
		full = containerResolvConf
	}
	fh, err := os.Open(full)
	if err != nil {
		return "", "resolver file unreadable; guests get no DNS resolver allowance"
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		addr, ok := strings.CutPrefix(line, "nameserver")
		if !ok {
			continue
		}
		addr = strings.TrimSpace(addr)
		if addr == "" || isLoopbackResolver(addr) {
			continue
		}
		return addr, ""
	}
	return "", noStoreNote
}

// containerResolvConf is the resolver file read under --container when
// Root has none (a test points it at a fixture).
var containerResolvConf = "/etc/resolv.conf"

// isLoopbackResolver reports whether addr is this resolver's own
// loopback (127.* or ::1): forwarding them into a guest would send the
// guest's DNS to itself.
func isLoopbackResolver(addr string) bool {
	if strings.HasPrefix(addr, "127.") {
		return true
	}
	if addr == "::1" {
		return true
	}
	return false
}

// resolvConfPath is where the guest resolver is read from.
const resolvConfPath = "/etc/resolv.conf"
