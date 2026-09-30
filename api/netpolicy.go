package api

import (
	"fmt"
	"sort"
)

// NetworkPolicy is a sandbox's egress policy. spoond decides what may
// leave a sandbox; the substrate enforces it, from the egress config
// carried on create (U08) and live updates (U09).
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

// Inbound port exposure — a lease can publish guest TCP ports so OTHER
// sandboxes (and the host) can reach a service it runs: a CI job's
// database, for one. The published address is the sandbox's host address
// (HostIP); which peers may open a connection is decided by each peer's
// own egress policy (peerAllowances), which is what replaced the old
// shared bridge's flat reachability.

const (
	// MaxExposedPorts bounds one lease's published ports.
	MaxExposedPorts = 8
)

// reservedGuestPorts can never be published: envd (:49983) is the guest
// agent — a control plane, not a service. Publishing it would hand every
// peer a channel into the sandbox's management surface.
var reservedGuestPorts = map[int]string{49983: "envd"}

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
