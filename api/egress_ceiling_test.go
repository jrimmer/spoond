package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v4/identity"
)

func TestEntryWithinCeiling(t *testing.T) {
	ceiling := []string{"192.0.2.0/24", "198.51.100.5", "example.com", "*.example.net", "fd00::/64"}
	cases := []struct {
		entry string
		want  bool
	}{
		{"192.0.2.203", true},
		{"192.0.2.0/25", true},
		{"192.0.2.0/24", true},
		{"192.0.2.0/23", false}, // wider than the ceiling's /24
		{"192.0.3.1", false},
		{"198.51.100.5", true},
		{"198.51.100.5/32", true},
		{"198.51.100.6", false},
		{"example.com", true},
		{"EXAMPLE.com.", true},
		{"api.example.com", false}, // no wildcard for example.com
		{"mail.example.net", true},
		{"example.net", false}, // "*." covers subdomains only
		{"evil-example.net", false},
		{"fd00::1", true},
		{"203.0.113.8", false},
	}
	for _, c := range cases {
		if got := entryWithinCeiling(c.entry, ceiling); got != c.want {
			t.Errorf("entryWithinCeiling(%q) = %v, want %v", c.entry, got, c.want)
		}
	}
}

func TestNormalizeCeilingEntry(t *testing.T) {
	for in, want := range map[string]string{
		"lan": "lan", "internet": "internet", "192.0.2.7": "192.0.2.7", "192.0.2.7/24": "192.0.2.0/24",
		"Example.COM.": "example.com", "*.Example.net": "*.example.net",
	} {
		if got, err := identity.NormalizeCeilingEntry(in); err != nil || got != want {
			t.Errorf("NormalizeCeilingEntry(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "*", "*.", "a*b.com", "http://x.com", "nodot"} {
		if _, err := identity.NormalizeCeilingEntry(bad); err == nil {
			t.Errorf("NormalizeCeilingEntry(%q) accepted", bad)
		}
	}
}

// TestEgressCeilingEnforced: with a ceiling set by an admin, a create, a
// live network change and a copy are refused (403 egress_ceiling) when
// they reach past it, and allowed inside it; clearing the ceiling
// restores today's behaviour.
func TestEgressCeilingEnforced(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ids, _ := identity.NewStore("")
	svc.SetIdentities(ids)
	h := NewServer(svc, NewImageRegistry(db)).Handler()
	if rec, _ := doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"admin","fingerprints":["SHA256:fp-a"],"token":"admin-tok"}`); rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap: %d", rec.Code)
	}
	rec, body := doUsersReq(t, h, "POST", "/api/users", "admin-tok", `{"name":"agent","kind":"agent","token":"agent-tok"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create agent: %d %s", rec.Code, rec.Body.String())
	}
	uid := body["user"].(map[string]any)["id"].(string)

	create := func(policy string, allow []string) (int, map[string]any) {
		b, _ := json.Marshal(map[string]any{"image": "py-base", "persistent": true, "network_policy": policy, "egress_allowlist": allow})
		rec, body := doUsersReq(t, h, "POST", "/api/leases", "agent-tok", string(b))
		return rec.Code, body
	}
	// A lease made before any ceiling, with wide access.
	code, wide := create("internet", nil)
	if code != http.StatusCreated {
		t.Fatalf("pre-ceiling create: %d %v", code, wide)
	}
	wideID := wide["id"].(string)

	// The agent itself may not set a ceiling.
	if rec, _ := doUsersReq(t, h, "POST", "/api/users/"+uid+"/egress-ceiling", "agent-tok", `{"egress_ceiling":[]}`); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin set ceiling: %d", rec.Code)
	}
	if rec, _ := doUsersReq(t, h, "POST", "/api/users/"+uid+"/egress-ceiling", "admin-tok", `{"egress_ceiling":["bad entry"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad ceiling entry: %d", rec.Code)
	}
	// A restricted lease made before the ceiling, half inside it.
	code, half := create("restricted", []string{"192.0.2.4", "203.0.113.4"})
	if code != http.StatusCreated {
		t.Fatalf("pre-ceiling restricted create: %d %v", code, half)
	}
	halfID := half["id"].(string)

	// A dry run lists what the ceiling would narrow and saves nothing.
	rec, body = doUsersReq(t, h, "POST", "/api/users/"+uid+"/egress-ceiling", "admin-tok", `{"egress_ceiling":["192.0.2.0/24","*.example.net"],"dry_run":true}`)
	if rec.Code != http.StatusOK || body["dry_run"] != true {
		t.Fatalf("dry run: %d %v", rec.Code, body)
	}
	affected := map[string]string{}
	for _, a := range body["affected"].([]any) {
		m := a.(map[string]any)
		affected[m["lease_id"].(string)] = fmt.Sprint(m["outside"])
	}
	if len(affected) != 2 || affected[wideID] != "[network_policy internet]" || affected[halfID] != "[203.0.113.4]" {
		t.Fatalf("dry run affected = %v", affected)
	}
	if _, me := doUsersReq(t, h, "GET", "/api/users/me", "agent-tok", ""); me["user"].(map[string]any)["egress_ceiling"] != nil {
		t.Fatalf("dry run saved the ceiling: %v", me["user"])
	}

	rec, body = doUsersReq(t, h, "POST", "/api/users/"+uid+"/egress-ceiling", "admin-tok", `{"egress_ceiling":["192.0.2.0/24","*.example.net"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set ceiling: %d %s", rec.Code, rec.Body.String())
	}
	if c, _ := body["user"].(map[string]any)["egress_ceiling"].([]any); len(c) != 2 {
		t.Fatalf("user view egress_ceiling = %v", body["user"])
	}
	// The set applies at once: the half-inside lease's enforced egress
	// drops 203.0.113.4 without any other trigger.
	deadline := time.Now().Add(5 * time.Second)
	for {
		svc.appliedMu.Lock()
		applied := svc.appliedEgress[halfID]
		svc.appliedMu.Unlock()
		if strings.Contains(applied, "192.0.2.4") && !strings.Contains(applied, "203.0.113.4") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("applied egress after set = %s, want 192.0.2.4 kept and 203.0.113.4 gone", applied)
		}
		time.Sleep(20 * time.Millisecond)
	}

	for _, c := range []struct {
		policy string
		allow  []string
		want   int
	}{
		{"none", nil, http.StatusCreated},
		{"restricted", nil, http.StatusCreated},
		{"restricted", []string{"192.0.2.203", "mail.example.net"}, http.StatusCreated},
		{"restricted", []string{"192.0.2.203", "203.0.113.8"}, http.StatusForbidden},
		{"restricted", []string{"github.com"}, http.StatusForbidden},
		{"lan", nil, http.StatusForbidden},
		{"internet", nil, http.StatusForbidden},
	} {
		code, body := create(c.policy, c.allow)
		if code != c.want {
			t.Errorf("create %s %v: %d, want %d (%v)", c.policy, c.allow, code, c.want, body)
		}
		if c.want == http.StatusForbidden && body["code"] != "egress_ceiling" {
			t.Errorf("create %s %v: code %v, want egress_ceiling", c.policy, c.allow, body["code"])
		}
	}

	// A live widening past the ceiling is refused; one inside it is not.
	code, in := create("restricted", []string{"192.0.2.5"})
	if code != http.StatusCreated {
		t.Fatalf("create inside: %d", code)
	}
	inID := in["id"].(string)
	if rec, body := doUsersReq(t, h, "POST", "/api/leases/"+inID+"/network", "agent-tok", `{"network_policy":"restricted","egress_allowlist":["203.0.113.1"]}`); rec.Code != http.StatusForbidden || body["code"] != "egress_ceiling" {
		t.Fatalf("widen past ceiling: %d %v", rec.Code, body)
	}
	if rec, _ := doUsersReq(t, h, "POST", "/api/leases/"+inID+"/network", "agent-tok", `{"network_policy":"restricted","egress_allowlist":["192.0.2.9"]}`); rec.Code != http.StatusOK {
		t.Fatalf("change inside ceiling: %d %s", rec.Code, rec.Body.String())
	}
	// A copy of the pre-ceiling internet lease would carry its policy.
	if rec, body := doUsersReq(t, h, "POST", "/api/leases/"+wideID+"/clone", "agent-tok", `{}`); rec.Code != http.StatusForbidden || body["code"] != "egress_ceiling" {
		t.Fatalf("clone past ceiling: %d %v", rec.Code, body)
	}
	if rec, body := doUsersReq(t, h, "POST", "/api/leases/"+wideID+"/fork", "agent-tok", `{"count":1}`); rec.Code != http.StatusForbidden || body["code"] != "egress_ceiling" {
		t.Fatalf("fork past ceiling: %d %v", rec.Code, body)
	}

	// A reference to a lease that is not the owner's own does not fit.
	if code, body := create("restricted", []string{"lease:not-mine"}); code != http.StatusForbidden {
		t.Fatalf("foreign lease reference: %d %v", code, body)
	}
	// Rendering applies the ceiling too: an entry outside it never
	// reaches the substrate, whatever the stored allowlist says.
	eg := svc.egressFor(&Lease{Owner: uid, NetPolicy: "restricted", NetAllow: []string{"192.0.2.10", "203.0.113.9", "mail.example.net", "other.example.org"}})
	if got := strings.Join(eg.AllowedCIDRs, ","); strings.Contains(got, "203.0.113.9") {
		t.Fatalf("rendered CIDRs include an entry outside the ceiling: %s", got)
	}
	if got := strings.Join(eg.AllowedDomains, ","); got != "mail.example.net" {
		t.Fatalf("rendered domains = %q, want only mail.example.net", got)
	}
	if got := strings.Join(eg.AllowedCIDRs, ","); !strings.Contains(got, "192.0.2.10/32") {
		t.Fatalf("rendered CIDRs lost an entry inside the ceiling: %s", got)
	}

	// Clearing the ceiling restores today's behaviour.
	if rec, _ := doUsersReq(t, h, "POST", "/api/users/"+uid+"/egress-ceiling", "admin-tok", `{"egress_ceiling":null}`); rec.Code != http.StatusOK {
		t.Fatalf("clear ceiling: %d", rec.Code)
	}
	if code, body := create("internet", nil); code != http.StatusCreated {
		t.Fatalf("create after clear: %d %v", code, body)
	}
}
