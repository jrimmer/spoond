package api

import (
	"context"
	"fmt"
	"net"
	"os/exec"
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
