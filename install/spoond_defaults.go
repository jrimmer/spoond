// The spoond phase's item 1 defaults, resolved by query (never exec, so
// they work in a dry run): the hostname to issue the certificate for,
// the host IP guests reach host services on, the guest DNS resolver and
// the token seed file the runtime phase made.
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
	// seedFile is the HMAC seed's path (backend.env points at it).
	seedFile string
	// seedPresent reports whether the seed file exists.
	seedPresent bool
	// seedSource says the seed came from the runtime phase
	// (kept) or is missing (the healthz wait still works: the
	// backend refuses leases without it, not health).
	seedSource string
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

	// host IP: the src of the IPv4 default route.
	ip, err := hostIP(ctx, env)
	switch {
	case err != nil:
		return dd, err
	case ip != "":
		dd.hostIP = ip
		r.Add(Created, "host IP", "default route's source address; pass --host-name only names the host, HOST_GUEST_SERVICE_ADDR in "+backendEnvPath+" overrides the IP")
	default:
		// No route: the fix names the env file to set, there is no
		// flag yet (docs/install.md section 4's HOST_GUEST_SERVICE_ADDR).
		note := errNoIPv4.Error() + " (" + backendEnvPath + ", then rerun)"
		r.Add(Failed, "host IP", note)
		return dd, fmt.Errorf("%s", errNoIPv4)
	}

	// guest DNS: the first usable nameserver.
	resolver, note := guestResolver(env)
	dd.guestDNS, dd.guestDNSNote = resolver, note
	if resolver == "" {
		r.Add(Skipped, "guest DNS", note)
	} else {
		r.Add(Created, "guest DNS", resolver+" (the first usable nameserver in /etc/resolv.conf)")
	}

	// the token seed file the runtime phase installed.
	dd.seedFile = e2bTokenSeedPath
	if _, err := os.Stat(env.Resolve(dd.seedFile)); err != nil {
		r.Add(Skipped, dd.seedFile, "the runtime phase has not installed the envd/traffic HMAC seed yet; spoond backend refuses leases without it")
	} else {
		dd.seedPresent = true
		r.Add(Kept, dd.seedFile, "kept (the runtime phase generates it)")
	}
	return dd, nil
}

// hostIP is the src address of `ip -4 -o route get 1.1.1.1`, the classic
// "which address leaves by the default route" query. "" when the route
// exists but names no source, an error when the query itself fails.
func hostIP(ctx context.Context, env *Env) (string, error) {
	out, err := env.query(ctx, "ip", "-4", "-o", "route", "get", "1.1.1.1")
	if err != nil {
		return "", fmt.Errorf("finding the host IP: %w", err)
	}
	for _, field := range strings.Fields(string(out)) {
		if a, ok := strings.CutPrefix(field, "src"); ok {
			return a, nil
		}
	}
	return "", nil
}

// guestResolver scans resolv.conf for the first nameserver address that
// is not 127.* or ::1: the guest's DNS target (HOST_GUEST_SERVICE... no:
// the guests' resolver address the backend grants on port 53). A
// loopback-only file yields ("", note).
func guestResolver(env *Env) (string, string) {
	full := env.Resolve(resolvConfPath)
	fh, err := os.Open(full)
	if err != nil {
		return "", "@/etc/resolv.conf is unreadable; guests get no DNS resolver allowance"
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
