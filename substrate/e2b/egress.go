package e2b

import (
	"github.com/jrimmer/spoond/v2/substrate"
	orchestrator "github.com/jrimmer/spoond/v2/substrate/e2b/gen/orchestrator"
)

// dnsFallback is added to allowed_cidrs whenever allowed_domains is present,
// so the guest can resolve those domains (A2 §3.2 buildEgressConfig; guest
// DNS is 8.8.8.8, A3 D5). It carries an explicit /32: the fork's layer-2
// decision (tcpfirewall) walks allowed_cidrs with net.ParseCIDR and, on a
// bare address, returns "invalid allowed CIDR" for every connection that
// reaches that loop — including a connection to an allow-listed private IP
// that only appears in allowed_private. A mixed allowlist (domains plus IPs)
// then loses HTTPS to those IPs (spoond-4pa).
//
// It deliberately does not follow SPOOND_GUEST_DNS_ADDR. That address is
// private (production: 10.1.0.2) and api/service.go already grants it on
// port 53 as an allowed_private allowance, so the guest reaches it without
// this entry. This entry is the public fallback upstream's buildEgressConfig
// assumes; making it follow the configured resolver would require the pure
// egressConfig to know the resolver (a substrate.Egress API change) and
// would drop the fallback when the resolver is unset, breaking domain
// resolution in that case. Kept as-is, and documented here.
const dnsFallback = "8.8.8.8/32"

// egressConfig converts substrate.Egress into the orchestrator's egress
// config, following E2B's buildEgressConfig pattern.
func egressConfig(eg substrate.Egress) *orchestrator.SandboxNetworkEgressConfig {
	cfg := &orchestrator.SandboxNetworkEgressConfig{
		AllowedCidrs:   eg.AllowedCIDRs,
		DeniedCidrs:    eg.DeniedCIDRs,
		AllowedDomains: eg.AllowedDomains,
	}
	if len(eg.AllowedDomains) > 0 {
		cfg.AllowedCidrs = append(cfg.AllowedCidrs, dnsFallback)
	}
	for _, p := range eg.Private {
		cfg.AllowedPrivate = append(cfg.AllowedPrivate, &orchestrator.SandboxPrivateAllowance{
			Cidr:     p.CIDR,
			TcpPorts: p.TCPPorts,
		})
	}
	return cfg
}
