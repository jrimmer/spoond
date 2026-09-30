package api

import (
	"strings"
	"testing"
)

func TestPolicyCommands(t *testing.T) {
	tests := []struct {
		name      string
		policy    NetworkPolicy
		allow     []string
		wantFlush bool
		wantDrop  bool
	}{
		{"none", PolicyNone, nil, true, true},
		{"lan", PolicyLAN, nil, true, true},
		{"internet", PolicyInternet, nil, true, false},
		{"restricted", PolicyRestricted, []string{"93.184.216.34", "1.1.1.1"}, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmds := policyCommands(tt.policy, tt.allow, []string{"10.1.0.2"})
			// every policy starts with a flush of FORWARD
			flushed := len(cmds) > 0 && cmds[0][0] == "-F" && cmds[0][1] == "FORWARD"
			if flushed != tt.wantFlush {
				t.Fatalf("flush expected %v, got %v (%v)", tt.wantFlush, flushed, cmds)
			}
			dropped := false
			for _, c := range cmds {
				if strings.Join(c, " ") == "-A FORWARD -m comment --comment forkd-netpolicy:lan -j DROP" ||
					strings.Join(c, " ") == "-A FORWARD -m comment --comment forkd-netpolicy:none -j DROP" ||
					strings.Contains(strings.Join(c, " "), "forkd-netpolicy:restricted") {
					dropped = true
				}
			}
			if dropped != tt.wantDrop {
				t.Fatalf("drop rule expected %v, got %v (%v)", tt.wantDrop, dropped, cmds)
			}
			if tt.policy == PolicyRestricted {
				// allowlist entries must each produce an ACCEPT rule
				accepts := 0
				for _, c := range cmds {
					if len(c) > 1 && c[len(c)-1] == "ACCEPT" && c[0] == "-A" {
						accepts++
					}
				}
				if accepts < len(tt.allow)+1 { // +1 for ESTABLISHED,RELATED
					t.Fatalf("restricted should ACCEPT allowlist entries, got %d accepts: %v", accepts, cmds)
				}
			}
		})
	}
}

func TestResolveEntry(t *testing.T) {
	// CIDR passthrough
	got := resolveEntry("10.0.0.0/8")
	if len(got) != 1 || got[0] != "10.0.0.0/8" {
		t.Fatalf("cidr passthrough: %v", got)
	}
	// bare IP passthrough
	got = resolveEntry("1.1.1.1")
	if len(got) != 1 || got[0] != "1.1.1.1" {
		t.Fatalf("ip passthrough: %v", got)
	}
	// syntactically invalid entry -> empty (no crash, no rule)
	got = resolveEntry("not a domain with spaces")
	if len(got) != 0 {
		t.Fatalf("invalid entry should be empty: %v", got)
	}
}

func TestValidNetworkPolicy(t *testing.T) {
	for _, p := range []string{"none", "lan", "internet", "restricted"} {
		if !ValidNetworkPolicy(p) {
			t.Fatalf("expected %q valid", p)
		}
	}
	for _, p := range []string{"", "full", "NONE", "external"} {
		if ValidNetworkPolicy(p) {
			t.Fatalf("expected %q invalid", p)
		}
	}
}
