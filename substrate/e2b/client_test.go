package e2b

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/substrate"
)

func TestNewSandboxID(t *testing.T) {
	re := regexp.MustCompile(`^i[a-z0-9]{20}$`)
	for range 100 {
		if id := NewSandboxID(); !re.MatchString(id) {
			t.Fatalf("NewSandboxID() = %q, want match %s", id, re)
		}
	}
}

func TestNewTemplateID(t *testing.T) {
	re := regexp.MustCompile(`^[a-z0-9]{20}$`)
	for range 100 {
		if id := NewTemplateID(); !re.MatchString(id) {
			t.Fatalf("NewTemplateID() = %q, want match %s", id, re)
		}
	}
}

func TestNewUUID(t *testing.T) {
	if NewUUID() == NewUUID() {
		t.Fatal("NewUUID returned the same value twice")
	}
}

func TestTokens(t *testing.T) {
	c := &Client{cfg: Config{TokenSeed: []byte("0123456789abcdef0123456789abcdef")}}
	const id = "i0123456789abcdefghij"

	// Deterministic for a fixed seed.
	if got, want := c.EnvdToken(id), "65b1506ff31c12a31cd870491baf288f69b783a9d232deb22cf2ca295c50c7b1"; got != want {
		t.Fatalf("EnvdToken = %q, want %q", got, want)
	}
	if got, want := c.TrafficToken(id), "364e14581a5780f9eb5989c6797652c7fb3e4a985ca2a4f474caad3548ab2ef3"; got != want {
		t.Fatalf("TrafficToken = %q, want %q", got, want)
	}
	// envd and traffic tokens differ.
	if c.EnvdToken(id) == c.TrafficToken(id) {
		t.Fatal("envd and traffic tokens are equal")
	}
}

func TestEgressConfig(t *testing.T) {
	eg := substrate.Egress{
		AllowedCIDRs:   []string{"0.0.0.0/0"},
		DeniedCIDRs:    []string{"192.0.2.0/24"},
		AllowedDomains: []string{"example.com"},
		Private: []substrate.PrivateAllowance{
			{CIDR: "10.0.0.11/32", TCPPorts: []uint32{8891}},
			{CIDR: "10.0.0.12/32"},
		},
	}
	cfg := egressConfig(eg)

	// 8.8.8.8/32 is added to allowed CIDRs when domains are present. It
	// must be a parseable CIDR: the fork's layer-2 decision uses
	// net.ParseCIDR, which rejects a bare address.
	var hasDNS bool
	for _, c := range cfg.GetAllowedCidrs() {
		if c == "8.8.8.8/32" {
			hasDNS = true
		}
	}
	if !hasDNS {
		t.Fatalf("allowed_cidrs %v missing 8.8.8.8/32", cfg.GetAllowedCidrs())
	}
	if got := cfg.GetAllowedCidrs()[0]; got != "0.0.0.0/0" {
		t.Fatalf("allowed_cidrs[0] = %q", got)
	}
	// The fallback is appended to a copy: the caller's slice keeps its
	// length and backing storage.
	if got := eg.AllowedCIDRs; len(got) != 1 || got[0] != "0.0.0.0/0" {
		t.Fatalf("egressConfig aliased the caller's AllowedCIDRs: %v", got)
	}
	if got := cfg.GetDeniedCidrs(); len(got) != 1 || got[0] != "192.0.2.0/24" {
		t.Fatalf("denied_cidrs = %v", got)
	}
	if got := cfg.GetAllowedDomains(); len(got) != 1 || got[0] != "example.com" {
		t.Fatalf("allowed_domains = %v", got)
	}
	if len(cfg.GetAllowedPrivate()) != 2 {
		t.Fatalf("allowed_private = %v", cfg.GetAllowedPrivate())
	}
	p0 := cfg.GetAllowedPrivate()[0]
	if p0.GetCidr() != "10.0.0.11/32" || len(p0.GetTcpPorts()) != 1 || p0.GetTcpPorts()[0] != 8891 {
		t.Fatalf("allowed_private[0] = %+v", p0)
	}
	p1 := cfg.GetAllowedPrivate()[1]
	if p1.GetCidr() != "10.0.0.12/32" || len(p1.GetTcpPorts()) != 0 {
		t.Fatalf("allowed_private[1] = %+v", p1)
	}

	// No domains: no 8.8.8.8 fallback.
	plain := egressConfig(substrate.Egress{AllowedCIDRs: []string{"0.0.0.0/0"}})
	for _, c := range plain.GetAllowedCidrs() {
		if c == "8.8.8.8/32" {
			t.Fatalf("allowed_cidrs %v must not contain 8.8.8.8/32 without domains", plain.GetAllowedCidrs())
		}
	}
	if len(plain.GetAllowedPrivate()) != 0 {
		t.Fatalf("allowed_private = %v", plain.GetAllowedPrivate())
	}

	// A configured private guest resolver replaces the public fallback: no
	// 8.8.8.8, and the resolver allowance is left to api/service.go.
	resolved := egressConfig(substrate.Egress{
		AllowedCIDRs:   []string{"0.0.0.0/0"},
		AllowedDomains: []string{"example.com"},
		GuestDNS:       true,
	})
	for _, c := range resolved.GetAllowedCidrs() {
		if c == "8.8.8.8/32" {
			t.Fatalf("allowed_cidrs %v must not contain 8.8.8.8/32 when a guest resolver is configured", resolved.GetAllowedCidrs())
		}
	}
	if len(resolved.GetAllowedCidrs()) != 1 || resolved.GetAllowedCidrs()[0] != "0.0.0.0/0" {
		t.Fatalf("allowed_cidrs = %v", resolved.GetAllowedCidrs())
	}
}

// TestEgressConfigTwoResolversNoPublicDNS pins the two-resolver
// deployment's public-DNS hygiene at the substrate boundary: with two
// private guest resolvers configured (GuestDNS), a domain-bearing egress
// carries both resolver allowances and never the public 8.8.8.8
// fallback.
func TestEgressConfigTwoResolversNoPublicDNS(t *testing.T) {
	eg := substrate.Egress{
		AllowedDomains: []string{"pg.example.com"},
		Private: []substrate.PrivateAllowance{
			{CIDR: "10.1.0.2/32", TCPPorts: []uint32{53}},
			{CIDR: "10.1.0.3/32", TCPPorts: []uint32{53}},
		},
		GuestDNS: true,
	}
	cfg := egressConfig(eg)
	for _, c := range cfg.GetAllowedCidrs() {
		if c == "8.8.8.8/32" {
			t.Fatalf("allowed_cidrs %v must not contain 8.8.8.8/32 with two private resolvers", cfg.GetAllowedCidrs())
		}
	}
	if got := cfg.GetAllowedPrivate(); len(got) != 2 || got[0].GetCidr() != "10.1.0.2/32" || got[1].GetCidr() != "10.1.0.3/32" {
		t.Fatalf("allowed_private = %v, want both resolvers", got)
	}
}

type stubNodeInfo struct {
	level int // outstanding work returned by every poll
	err   error
	calls int
}

func (s *stubNodeInfo) nodeInfo(ctx context.Context) (substrate.NodeInfo, error) {
	s.calls++
	return substrate.NodeInfo{OutstandingWork: s.level}, s.err
}

func TestWaitOutstandingReturnsWhenWorkBackAtBaseline(t *testing.T) {
	// Node was at 2 before the call; the persist pushed it to 3; it comes
	// back down to 2. The wait must return as soon as work <= before,
	// without reaching the bound.
	stub := &stubNodeInfo{level: 2}
	start := time.Now()
	waitOutstanding(t.Context(), "i0123456789abcdefghij", "pause", 2, stub.nodeInfo, time.Millisecond, time.Second)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("wait returned after %s, wanted a quick return at the baseline", elapsed)
	}
	if stub.calls == 0 {
		t.Fatal("NodeInfo was never polled")
	}
}

func TestWaitOutstandingGivesUpAtBound(t *testing.T) {
	// Work stays above the baseline (never reaches 0 in practice): the wait
	// must give up after the bound and never error.
	stub := &stubNodeInfo{level: 1}
	start := time.Now()
	waitOutstanding(t.Context(), "i0123456789abcdefghij", "checkpoint", 0, stub.nodeInfo, time.Millisecond, 50*time.Millisecond)
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("wait returned after %s, wanted the full bound", elapsed)
	}
	if stub.calls < 2 {
		t.Fatalf("polled %d times, wanted at least a baseline and one poll", stub.calls)
	}
}

func TestWaitOutstandingTreatsPollErrorsAsAboveBaseline(t *testing.T) {
	// Poll errors keep the wait going until the bound; still no error out.
	stub := &stubNodeInfo{err: errors.New("node info unavailable")}
	start := time.Now()
	waitOutstanding(t.Context(), "i0123456789abcdefghij", "pause", 0, stub.nodeInfo, time.Millisecond, 30*time.Millisecond)
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Fatalf("wait returned after %s, wanted the full bound", elapsed)
	}
}
