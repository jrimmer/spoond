package api

import (
	"context"
	"strings"
	"testing"
	"time"
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

// fakeNetpol records Apply calls.
type fakeNetpol struct {
	calls []string
	err   error
}

func (f *fakeNetpol) Apply(_ context.Context, netns string, p NetworkPolicy, allow []string) error {
	f.calls = append(f.calls, netns+"|"+string(p)+"|"+strings.Join(allow, ","))
	return f.err
}

// TestApplyNetpolHooks verifies policy is applied on grant (fresh sandbox)
// and re-applied on resume (new sandbox after restart/suspend).
func TestApplyNetpolHooks(t *testing.T) {
	ff := newFakeForkd()
	ff.netns = "forkd-child-9" // fake reports a netns
	svc := NewService(ff, map[string]string{"t": "c"}, 1, time.Minute, 10*time.Minute, "py-base")
	fp := &fakeNetpol{}
	svc.SetNetpol(fp, []string{"10.1.0.2"})

	// non-lan policy must be applied at grant
	l, err := svc.grant(context.Background(), "c", "py-base", 0, time.Minute, true, string(PolicyRestricted), []string{"example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(fp.calls) != 1 {
		t.Fatalf("expected 1 apply on grant, got %d: %v", len(fp.calls), fp.calls)
	}
	if !strings.Contains(fp.calls[0], "restricted") {
		t.Fatalf("expected restricted policy applied, got %q", fp.calls[0])
	}

	// resume must re-apply (the sandbox was recreated)
	fp.calls = nil
	if _, err := svc.resume(context.Background(), "c", l.ID); err != nil {
		t.Fatal(err)
	}
	if len(fp.calls) != 1 {
		t.Fatalf("expected 1 apply on resume, got %d: %v", len(fp.calls), fp.calls)
	}

	// default (empty) policy is treated as LAN and must be applied to clear
	// stale FORWARD rules from a previous lease that used the same netns.
	fp.calls = nil
	_, err = svc.grant(context.Background(), "c", "py-base", 0, time.Minute, true, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(fp.calls) != 1 {
		t.Fatalf("expected 1 apply for lan policy (clear stale rules), got %d: %v", len(fp.calls), fp.calls)
	}
	if !strings.Contains(fp.calls[0], "lan") {
		t.Fatalf("expected lan policy applied, got %q", fp.calls[0])
	}
}
