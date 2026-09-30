package e2b

import (
	"github.com/jrimmer/spoond/substrate"
	orchestrator "github.com/jrimmer/spoond/substrate/e2b/gen/orchestrator"
)

// dnsFallback is added to allowed_cidrs whenever allowed_domains is present,
// so the guest can resolve those domains (A2 §3.2 buildEgressConfig; guest
// DNS is 8.8.8.8, A3 D5).
const dnsFallback = "8.8.8.8"

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
