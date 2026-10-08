package e2b

import (
	"github.com/jrimmer/spoond/v2/substrate"
	orchestrator "github.com/jrimmer/spoond/v2/substrate/e2b/gen/orchestrator"
)

// dnsFallback is added to allowed_cidrs whenever allowed_domains is present
// and no private guest resolver is configured, so the guest can resolve those
// domains. It carries an explicit /32: the fork's layer-2 decision
// (tcpfirewall) walks allowed_cidrs with net.ParseCIDR and, on a bare
// address, returns "invalid allowed CIDR" for every connection that reaches
// that loop — including a connection to an allow-listed private IP that only
// appears in allowed_private. A mixed allowlist (domains plus IPs) then loses
// HTTPS to those IPs (spoond-4pa).
//
// The fallback is public, so adding it with the domain grant also opens
// 8.8.8.8 to the guest. The egress API's allowed_cidrs cannot express a TCP
// port scope (only allowed_private carries TcpPorts), so the fallback cannot
// be limited to port 53; it is dropped entirely when the deployment configures
// a private guest resolver (substrate.Egress.GuestDNS). Production does
// (SPOOND_GUEST_DNS_ADDR=10.1.0.2,10.1.0.3, each already granted port 53 by
// api/service.go's dnsAllowance), so production sends no public fallback and a
// domain-bearing restricted lease gains no route to 8.8.8.8. A deployment
// without a configured resolver keeps the fallback, because the guest then has
// no way to resolve allow-listed domains at all.
const dnsFallback = "8.8.8.8/32"

// egressConfig converts substrate.Egress into the orchestrator's egress
// config, following E2B's buildEgressConfig pattern. It never aliases the
// caller's slices: the DNS fallback is appended to a copy of AllowedCIDRs.
func egressConfig(eg substrate.Egress) *orchestrator.SandboxNetworkEgressConfig {
	// Copy rather than append in place: appending the fallback to the
	// caller's slice would write past its length into shared backing storage.
	allowedCIDRs := append([]string(nil), eg.AllowedCIDRs...)
	if len(eg.AllowedDomains) > 0 && !eg.GuestDNS {
		allowedCIDRs = append(allowedCIDRs, dnsFallback)
	}
	cfg := &orchestrator.SandboxNetworkEgressConfig{
		AllowedCidrs:   allowedCIDRs,
		DeniedCidrs:    eg.DeniedCIDRs,
		AllowedDomains: eg.AllowedDomains,
	}
	for _, p := range eg.Private {
		cfg.AllowedPrivate = append(cfg.AllowedPrivate, &orchestrator.SandboxPrivateAllowance{
			Cidr:     p.CIDR,
			TcpPorts: p.TCPPorts,
		})
	}
	return cfg
}
