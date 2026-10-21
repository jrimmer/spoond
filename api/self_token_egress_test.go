package api

import (
	"testing"

	"github.com/jrimmer/spoond/v3/substrate"
)

// A self-token lease reaches the host's API port, and only that port,
// under every policy but none; a lease without the token never does
// (restricted) or only through HOST_API_PORT (lan, internet).
func TestSelfTokenEgressAllowance(t *testing.T) {
	svc, _ := newLifecycleService(t)
	svc.cfg.HostAPIPort = 0
	svc.cfg.SelfTokenAPIURL = "https://api.example.test:8890"
	api := substrate.PrivateAllowance{CIDR: "10.0.0.11/32", TCPPorts: []uint32{8890}}

	for _, policy := range []string{"restricted", "lan", "internet"} {
		t.Run(policy, func(t *testing.T) {
			got := svc.egressFor(&Lease{NetPolicy: policy, SelfToken: true})
			if !containsAllowance(got.Private, api) {
				t.Fatalf("self-token %s lease lacks the API allowance: %+v", policy, got.Private)
			}
			for _, a := range got.Private {
				if a.CIDR == "10.0.0.11/32" && !(len(a.TCPPorts) == 1 && (a.TCPPorts[0] == 8890 || a.TCPPorts[0] == 8891)) {
					t.Fatalf("host allowance beyond the API and host service: %+v", a)
				}
			}
			plain := svc.egressFor(&Lease{NetPolicy: policy})
			if containsAllowance(plain.Private, api) {
				t.Fatalf("%s lease without a self token reaches the API: %+v", policy, plain.Private)
			}
		})
	}

	if got := svc.egressFor(&Lease{NetPolicy: "none", SelfToken: true}); len(got.Private) != 0 {
		t.Fatalf("none lease got allowances: %+v", got.Private)
	}

	// HOST_API_PORT already names the same port: no duplicate.
	svc.cfg.HostAPIPort = 8890
	n := 0
	for _, a := range svc.egressFor(&Lease{NetPolicy: "lan", SelfToken: true}).Private {
		if a.CIDR == api.CIDR && len(a.TCPPorts) == 1 && a.TCPPorts[0] == 8890 {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("API allowance appears %d times, want 1", n)
	}

	// No URL configured: nothing to reach, so no allowance.
	svc.cfg.HostAPIPort = 0
	svc.cfg.SelfTokenAPIURL = ""
	if got := svc.egressFor(&Lease{NetPolicy: "restricted", SelfToken: true}); containsAllowance(got.Private, api) {
		t.Fatalf("allowance without SELF_TOKEN_API_URL: %+v", got.Private)
	}
}

func TestSelfTokenAPIPort(t *testing.T) {
	for in, want := range map[string]uint32{
		"https://sb.example:8890":  8890,
		"https://sb.example":       443,
		"http://sb.example":        80,
		"":                         0,
		"sb.example:8890":          0,
		"https://sb.example:0":     0,
		"https://sb.example:99999": 0,
	} {
		if got := selfTokenAPIPort(in); got != want {
			t.Errorf("selfTokenAPIPort(%q) = %d, want %d", in, got, want)
		}
	}
}
