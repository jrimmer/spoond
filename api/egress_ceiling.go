package api

import (
	"net"
	"net/http"
	"sort"
	"strings"
)

// Egress ceiling (#164): an admin may cap the network access a user's
// leases can ever have. A create, a clone or fork, and a live network
// change are refused (403 egress_ceiling) when the policy they would set
// is wider than the owner's ceiling, so an agent that controls its own
// lease cannot grant itself new destinations. Rendering applies it too:
// a restricted lease's allowlist entries outside the ceiling are left
// out of the egress the substrate enforces (egressForLocked), so an
// entry that changes meaning later (a peer reference whose lease is
// gone becomes a domain) never widens access. No ceiling (nil) keeps
// today's behaviour.

// egressOutsideCeiling returns the parts of policy and allow that the
// owner's ceiling does not cover (nil when all of it fits, or when the
// owner has no ceiling). An allowlist entry that names one of the
// owner's own leases is a peer reference and fits; a reference to
// another owner's lease does not.
func (s *Service) egressOutsideCeiling(owner, policy string, allow []string) []string {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	return s.egressOutsideCeilingLocked(owner, policy, allow)
}

// egressOutsideCeilingOf is egressOutsideCeiling for an existing lease's
// own policy (a clone or fork copies it), read under the lock. A lease
// not found reports nothing: the copy then fails on its own.
func (s *Service) egressOutsideCeilingOf(owner, id string) []string {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l := s.store.leases[id]
	if l == nil || l.Owner != owner {
		return nil
	}
	return s.egressOutsideCeilingLocked(owner, l.NetPolicy, l.NetAllow)
}

// ceilingOf is the owner's ceiling (nil = none).
func (s *Service) ceilingOf(owner string) []string {
	if s.identities == nil {
		return nil
	}
	u := s.identities.UserByID(owner)
	if u == nil || u.EgressCeiling == nil {
		return nil
	}
	return *u.EgressCeiling
}

// egressOutsideCeilingLocked is egressOutsideCeiling with s.store.mu
// held.
func (s *Service) egressOutsideCeilingLocked(owner, policy string, allow []string) []string {
	if s.identities == nil {
		return nil
	}
	u := s.identities.UserByID(owner)
	if u == nil || u.EgressCeiling == nil {
		return nil
	}
	return s.outsideCeilingLocked(owner, *u.EgressCeiling, policy, allow)
}

// outsideCeilingLocked is the check against a given ceiling (the owner's
// stored one, or a proposed one for a dry run). s.store.mu held.
func (s *Service) outsideCeilingLocked(owner string, ceiling []string, policy string, allow []string) []string {
	if policy == "" {
		policy = string(PolicyRestricted)
	}
	switch NetworkPolicy(policy) {
	case PolicyNone:
		return nil
	case PolicyInternet:
		if !hasWord(ceiling, "internet") {
			return []string{"network_policy internet"}
		}
		return nil
	case PolicyLAN:
		if !hasWord(ceiling, "lan") && !hasWord(ceiling, "internet") {
			return []string{"network_policy lan"}
		}
		return nil
	}
	var out []string
	for _, e := range allow {
		e = strings.TrimSpace(e)
		if e == "" || s.isOwnPeerReferenceLocked(owner, e) {
			continue
		}
		if !entryWithinCeiling(e, ceiling) {
			out = append(out, e)
		}
	}
	return out
}

// ceilingImpact is one existing lease a proposed ceiling would narrow.
type ceilingImpact struct {
	LeaseID string   `json:"lease_id"`
	Name    string   `json:"name,omitempty"`
	Holder  string   `json:"holder,omitempty"`
	State   string   `json:"state"`
	Policy  string   `json:"network_policy"`
	Outside []string `json:"outside"`
	// Effect says what happens to the lease: a restricted lease loses the
	// outside entries the next time its egress is applied; a lan or
	// internet lease keeps its policy, but its owner can no longer copy
	// it or change it to anything outside the ceiling.
	Effect string `json:"effect"`
}

// ceilingImpactOn lists owner's existing leases that ceiling would
// narrow (#164 dry run), oldest first.
func (s *Service) ceilingImpactOn(owner string, ceiling []string) []ceilingImpact {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	out := []ceilingImpact{}
	for _, l := range s.store.leases {
		if l.Owner != owner || l.released {
			continue
		}
		outside := s.outsideCeilingLocked(owner, ceiling, l.NetPolicy, l.NetAllow)
		if len(outside) == 0 {
			continue
		}
		policy := l.NetPolicy
		if policy == "" {
			policy = string(PolicyRestricted)
		}
		effect := "keeps its policy; copies and network changes outside the ceiling are refused"
		if NetworkPolicy(policy) == PolicyRestricted {
			effect = "loses the outside entries as soon as the ceiling is set"
		}
		out = append(out, ceilingImpact{LeaseID: l.ID, Name: l.Name, Holder: l.Holder,
			State: l.State, Policy: policy, Outside: outside, Effect: effect})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LeaseID < out[j].LeaseID })
	return out
}

// isOwnPeerReferenceLocked reports whether entry names one of owner's
// own leases (by id, name or "lease:" form). s.store.mu held.
func (s *Service) isOwnPeerReferenceLocked(owner, entry string) bool {
	name := strings.TrimPrefix(strings.TrimSpace(entry), "lease:")
	if name == "" {
		return false
	}
	for _, p := range s.store.leases {
		if p.Owner == owner && !p.released && (p.ID == name || (p.Name != "" && p.Name == name)) {
			return true
		}
	}
	return false
}

func hasWord(list []string, w string) bool {
	for _, e := range list {
		if e == w {
			return true
		}
	}
	return false
}

// entryWithinCeiling reports whether one allowlist entry is covered by a
// ceiling: an IP by an equal IP or a containing CIDR, a CIDR by a CIDR
// that contains all of it, a domain by the same domain or a "*." entry
// for one of its parents. Matching is on the entry as written: a domain
// never matches an IP ceiling, since what it resolves to can change.
func entryWithinCeiling(entry string, ceiling []string) bool {
	if ip := net.ParseIP(entry); ip != nil {
		for _, c := range ceiling {
			if cip := net.ParseIP(c); cip != nil && cip.Equal(ip) {
				return true
			}
			if _, n, err := net.ParseCIDR(c); err == nil && n.Contains(ip) {
				return true
			}
		}
		return false
	}
	if _, en, err := net.ParseCIDR(entry); err == nil {
		eOnes, eBits := en.Mask.Size()
		for _, c := range ceiling {
			_, cn, err := net.ParseCIDR(c)
			if err != nil {
				if cip := net.ParseIP(c); cip != nil && eOnes == eBits && cip.Equal(en.IP) {
					return true
				}
				continue
			}
			cOnes, cBits := cn.Mask.Size()
			if cBits == eBits && cOnes <= eOnes && cn.Contains(en.IP) {
				return true
			}
		}
		return false
	}
	d := strings.TrimSuffix(strings.ToLower(entry), ".")
	for _, c := range ceiling {
		if c == d {
			return true
		}
		if parent, ok := strings.CutPrefix(c, "*."); ok && strings.HasSuffix(d, "."+parent) {
			return true
		}
	}
	return false
}

// refuseOutsideCeiling answers 403 egress_ceiling and returns true when
// policy and allow reach past owner's ceiling.
func (s *Server) refuseOutsideCeiling(w http.ResponseWriter, owner, policy string, allow []string) bool {
	return s.writeCeilingRefusal(w, s.svc.egressOutsideCeiling(owner, policy, allow))
}

// writeCeilingRefusal answers 403 egress_ceiling for outside, if any.
func (s *Server) writeCeilingRefusal(w http.ResponseWriter, outside []string) bool {
	if len(outside) == 0 {
		return false
	}
	writeErrorCode(w, http.StatusForbidden, "egress_ceiling", "outside the owner's egress ceiling: "+strings.Join(outside, ", "))
	return true
}
