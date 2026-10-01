package api

import (
	"testing"
)

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
