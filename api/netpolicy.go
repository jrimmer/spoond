package api

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strings"
)

// NetworkPolicy is a sandbox's egress policy, enforced with iptables
// FORWARD rules inside the sandbox's child network namespace. This is a
// service-layer concern (hexagonal): the controller only hands out a
// netns per sandbox; spoond decides what may leave it.
type NetworkPolicy string

const (
	PolicyNone       NetworkPolicy = "none"       // no egress at all
	PolicyLAN        NetworkPolicy = "lan"        // RFC1918 + link-local only (default)
	PolicyInternet   NetworkPolicy = "internet"   // full egress (host NAT applies)
	PolicyRestricted NetworkPolicy = "restricted" // allowlisted IPs/CIDRs/domains only
)

// ValidNetworkPolicy reports whether p is a known policy name.
func ValidNetworkPolicy(p string) bool {
	switch NetworkPolicy(p) {
	case PolicyNone, PolicyLAN, PolicyInternet, PolicyRestricted:
		return true
	}
	return false
}

// PolicyApplier is the port for enforcing a policy inside a netns. The
// production implementation shells to ip netns exec + iptables; tests
// inject a fake that records calls.
type PolicyApplier interface {
	Apply(ctx context.Context, netns string, policy NetworkPolicy, allowlist []string) error
}

// NetnsPolicyApplier enforces policies with iptables FORWARD rules inside
// the given network namespace. Idempotent: it flushes the FORWARD chain
// first (the chain is per-sandbox and the netns pool is reused), then
// installs the rules for the requested policy.
type NetnsPolicyApplier struct {
	// DNSAllowlist is always permitted under PolicyRestricted so the
	// guest can still resolve names the allowlist was built from.
	DNSAllowlist []string
}

func (a *NetnsPolicyApplier) Apply(ctx context.Context, netns string, policy NetworkPolicy, allowlist []string) error {
	if netns == "" {
		return fmt.Errorf("no netns to apply policy to")
	}
	cmds := policyCommands(policy, allowlist, a.DNSAllowlist)
	for _, args := range cmds {
		full := append([]string{"netns", "exec", netns, "iptables"}, args...)
		out, err := exec.CommandContext(ctx, "ip", full...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("iptables in netns %s: %v: %s", netns, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// policyCommands returns the iptables args to enforce policy in a netns.
// FORWARD chain governs guest↔outside traffic (tap0 ↔ veth0); the chain
// is flushed first so rules are idempotent across netns pool reuse.
func policyCommands(policy NetworkPolicy, allowlist, dnsAllow []string) [][]string {
	flush := []string{"-F", "FORWARD"}
	switch policy {
	case PolicyNone:
		return [][]string{
			flush,
			{"-A", "FORWARD", "-m", "comment", "--comment", "forkd-netpolicy:none", "-j", "DROP"},
		}
	case PolicyLAN:
		return [][]string{
			flush,
			{"-A", "FORWARD", "-m", "state", "--state", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
			{"-A", "FORWARD", "-d", "10.0.0.0/8", "-j", "ACCEPT"},
			{"-A", "FORWARD", "-d", "172.16.0.0/12", "-j", "ACCEPT"},
			{"-A", "FORWARD", "-d", "192.168.0.0/16", "-j", "ACCEPT"},
			{"-A", "FORWARD", "-m", "comment", "--comment", "forkd-netpolicy:lan", "-j", "DROP"},
		}
	case PolicyInternet:
		return [][]string{flush} // default FORWARD policy is ACCEPT
	case PolicyRestricted:
		var cmds [][]string
		cmds = append(cmds, flush)
		cmds = append(cmds, []string{"-A", "FORWARD", "-m", "state", "--state", "ESTABLISHED,RELATED", "-j", "ACCEPT"})
		// Always let the guest reach the configured resolvers.
		for _, dns := range dnsAllow {
			cmds = append(cmds, []string{"-A", "FORWARD", "-p", "udp", "--dport", "53", "-d", dns, "-j", "ACCEPT"})
			cmds = append(cmds, []string{"-A", "FORWARD", "-p", "tcp", "--dport", "53", "-d", dns, "-j", "ACCEPT"})
		}
		for _, entry := range allowlist {
			for _, ip := range resolveEntry(entry) {
				cmds = append(cmds, []string{"-A", "FORWARD", "-d", ip, "-j", "ACCEPT"})
			}
		}
		cmds = append(cmds, []string{"-A", "FORWARD", "-m", "comment", "--comment", "forkd-netpolicy:restricted", "-j", "DROP"})
		return cmds
	}
	return nil
}

// resolveEntry turns an allowlist entry into one or more IPs/CIDRs.
// CIDRs and bare IPs pass through; domain names are resolved with the
// host resolver (IPv4 first — IPv6 is not provisioned on the bridge).
func resolveEntry(entry string) []string {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return nil
	}
	if ip := net.ParseIP(entry); ip != nil {
		return []string{entry}
	}
	if _, _, err := net.ParseCIDR(entry); err == nil {
		return []string{entry}
	}
	ips, err := net.LookupIP(entry)
	if err != nil || len(ips) == 0 {
		return nil
	}
	var out []string
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			out = append(out, v4.String())
		}
	}
	return out
}

// Inbound port exposure — a lease can publish guest TCP ports on its
// bridge-facing address so OTHER sandboxes (and the host) can reach a
// service it runs: a CI job's database, for one. Everything else about the
// netns stays egress-only.
//
// The shape inside the lease's netns:
//
//	nat    PREROUTING -i veth0 --dport P  → DNAT guest:P
//	filter FORWARD    -i veth0 → tap  -d guest --dport P  ACCEPT   (inserted
//	filter FORWARD    -i tap → veth0  -s guest --sport P  ESTABLISHED ACCEPT
//	                                                        above the policy)
//
// so a published port answers its own connections and nothing more: under
// PolicyNone the guest still cannot open a connection of its own.
//
// Reachability is bounded by the bridge, not by this code: veth0 sits on
// forkd-br0 (10.43.0.0/16), which the LAN cannot route into, so a published
// port is visible to the host and to sandboxes whose own policy lets them
// reach 10.43.0.0/16 (lan, internet, or an allowlisted restricted lease).

const (
	netnsUplink = "veth0"      // bridge-facing interface in every child netns
	netnsTap    = "forkd-tap0" // guest-facing interface in every child netns
	// MaxExposedPorts bounds one lease's published ports.
	MaxExposedPorts = 8
)

// reservedGuestPorts can never be published: the guest agent (:8888) is an
// unauthenticated root exec endpoint and Shelley (:9000) an agent loop.
// Publishing either would hand every bridge peer a root shell.
var reservedGuestPorts = map[int]string{8888: "the guest exec agent", 9000: "the Shelley agent"}

// ValidateExposePorts checks a requested port list: 1..65535, no reserved
// port, no duplicates, at most MaxExposedPorts. It returns the list sorted.
func ValidateExposePorts(ports []int) ([]int, error) {
	if len(ports) > MaxExposedPorts {
		return nil, fmt.Errorf("at most %d exposed ports", MaxExposedPorts)
	}
	seen := map[int]bool{}
	out := make([]int, 0, len(ports))
	for _, p := range ports {
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("exposed port %d is out of range", p)
		}
		if what, reserved := reservedGuestPorts[p]; reserved {
			return nil, fmt.Errorf("port %d is %s and cannot be exposed", p, what)
		}
		if seen[p] {
			return nil, fmt.Errorf("exposed port %d is listed twice", p)
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Ints(out)
	return out, nil
}

// PortExposer is the optional capability of a PolicyApplier that publishes
// guest ports. Expose MUST be called on every (re)application, with an empty
// list too: the netns pool is reused, and a previous lease's DNAT rules would
// otherwise survive into the next tenant. It returns the netns's
// bridge-facing IP — the host part of every published address.
type PortExposer interface {
	Expose(ctx context.Context, netns, guestHost string, ports []int) (bridgeIP string, err error)
}

// Expose implements PortExposer with iptables inside the netns. It runs
// AFTER Apply, which flushed FORWARD; the accepts are inserted at the top so
// they precede the policy's final DROP.
func (a *NetnsPolicyApplier) Expose(ctx context.Context, netns, guestHost string, ports []int) (string, error) {
	if netns == "" {
		return "", fmt.Errorf("no netns to expose ports in")
	}
	for _, args := range exposeCommands(guestHost, ports) {
		full := append([]string{"netns", "exec", netns, "iptables"}, args...)
		out, err := exec.CommandContext(ctx, "ip", full...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("iptables in netns %s: %v: %s", netns, err, strings.TrimSpace(string(out)))
		}
	}
	out, err := exec.CommandContext(ctx, "ip", "-n", netns, "-4", "-o", "addr", "show", "dev", netnsUplink).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("read %s address in netns %s: %v: %s", netnsUplink, netns, err, strings.TrimSpace(string(out)))
	}
	ip := parseIPv4Addr(string(out))
	if ip == "" {
		return "", fmt.Errorf("no IPv4 address on %s in netns %s", netnsUplink, netns)
	}
	return ip, nil
}

// exposeCommands returns the iptables args that publish ports. The first
// command always flushes nat PREROUTING — the only user of that chain in a
// child netns — so a reused netns starts clean even when ports is empty.
func exposeCommands(guestHost string, ports []int) [][]string {
	cmds := [][]string{{"-t", "nat", "-F", "PREROUTING"}}
	for _, p := range ports {
		port := fmt.Sprint(p)
		cmds = append(cmds,
			[]string{"-t", "nat", "-A", "PREROUTING", "-i", netnsUplink, "-p", "tcp", "--dport", port,
				"-m", "comment", "--comment", "forkd-expose", "-j", "DNAT", "--to-destination", guestHost + ":" + port},
			[]string{"-I", "FORWARD", "1", "-i", netnsUplink, "-o", netnsTap, "-p", "tcp", "-d", guestHost, "--dport", port,
				"-m", "comment", "--comment", "forkd-expose", "-j", "ACCEPT"},
			[]string{"-I", "FORWARD", "1", "-i", netnsTap, "-o", netnsUplink, "-p", "tcp", "-s", guestHost, "--sport", port,
				"-m", "state", "--state", "ESTABLISHED", "-m", "comment", "--comment", "forkd-expose", "-j", "ACCEPT"},
		)
	}
	return cmds
}

// parseIPv4Addr pulls the address out of `ip -4 -o addr show` output
// ("3: veth0    inet 10.43.0.10/16 brd … scope global veth0").
func parseIPv4Addr(out string) string {
	f := strings.Fields(out)
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "inet" {
			if ip, _, err := net.ParseCIDR(f[i+1]); err == nil && ip.To4() != nil {
				return ip.String()
			}
		}
	}
	return ""
}
