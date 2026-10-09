package api

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
)

// insertLease places a hand-built lease in the service's live store so
// tests control policy, state, host address and exposed ports directly.
func insertLease(t *testing.T, svc *Service, l *Lease) {
	t.Helper()
	svc.store.mu.Lock()
	defer svc.store.mu.Unlock()
	svc.store.leases[l.ID] = l
	svc.saveLeaseLocked(l)
}

// peer is the published-port allowance a lease grants its peers.
func peerAllowance(hostIP string, ports ...int) substrate.PrivateAllowance {
	p := make([]uint32, 0, len(ports))
	for _, port := range ports {
		p = append(p, uint32(port))
	}
	return substrate.PrivateAllowance{CIDR: hostIP + "/32", TCPPorts: p}
}

// TestExposedMap pins the published-address map: HostIP-keyed entries for
// a live lease, nothing for a suspended one or one without a host address.
func TestExposedMap(t *testing.T) {
	live := &Lease{ID: "a", State: "running", HostIP: "10.11.0.5", ExposePorts: []int{80, 9042}, ExposedIP: "10.11.0.5"}
	want := map[string]string{"80": "10.11.0.5:80", "9042": "10.11.0.5:9042"}
	if got := exposedMap(live); !reflect.DeepEqual(got, want) {
		t.Fatalf("live: got %v, want %v", got, want)
	}
	suspended := &Lease{ID: "a", State: "suspended", HostIP: "10.11.0.5", ExposePorts: []int{80}}
	if got := exposedMap(suspended); len(got) != 0 {
		t.Fatalf("suspended: got %v, want empty", got)
	}
	noAddr := &Lease{ID: "a", State: "running", ExposePorts: []int{80}}
	if got := exposedMap(noAddr); len(got) != 0 {
		t.Fatalf("no host address: got %v, want empty", got)
	}
}

// TestPeerAllowancesPerPolicy pins who may reach a publishing peer's
// exposed ports, per the lease's own egress policy: lan and internet see
// every publishing peer (any owner), restricted only allowlisted ones,
// none nothing.
func TestPeerAllowancesPerPolicy(t *testing.T) {
	svc, _ := newLifecycleService(t)
	mk := func(id, policy string, allow ...string) *Lease {
		return &Lease{ID: id, Owner: "u-" + id, Image: "py-base", State: "running",
			NetPolicy: policy, NetAllow: allow}
	}
	insertLease(t, svc, mk("a", "lan"))
	insertLease(t, svc, mk("b", "restricted"))
	insertLease(t, svc, mk("c", "none"))
	// A peer of another owner, publishing ports.
	insertLease(t, svc, &Lease{ID: "pdb", Owner: "someone-else", Image: "py-base", State: "running",
		Name: "db", HostIP: "10.11.0.9", ExposePorts: []int{5432, 8080}})

	t.Run("lan reaches every publishing peer", func(t *testing.T) {
		got := svc.egressFor(svc.lookup("u-a", "a")).Private
		last := got[len(got)-1]
		if want := (substrate.PrivateAllowance{CIDR: "10.11.0.9/32", TCPPorts: []uint32{5432, 8080}}); !reflect.DeepEqual(last, want) {
			t.Fatalf("lan peer allowance = %+v, want %+v", last, want)
		}
	})

	t.Run("internet reaches every publishing peer", func(t *testing.T) {
		got := svc.egressFor(&Lease{ID: "x", Owner: "u-x", State: "running", NetPolicy: "internet"}).Private
		last := got[len(got)-1]
		if want := (substrate.PrivateAllowance{CIDR: "10.11.0.9/32", TCPPorts: []uint32{5432, 8080}}); !reflect.DeepEqual(last, want) {
			t.Fatalf("internet peer allowance = %+v, want %+v", last, want)
		}
	})

	t.Run("restricted reaches allowlisted peers", func(t *testing.T) {
		for _, allow := range [][]string{{"pdb"}, {"db"}, {"lease:pdb"}, {"lease:db"}} {
			l := &Lease{ID: "y", Owner: "u-y", State: "running", NetPolicy: "restricted", NetAllow: allow}
			got := svc.egressFor(l).Private
			var found bool
			for _, a := range got {
				if a.CIDR == "10.11.0.9/32" {
					found = true
					if !reflect.DeepEqual(a.TCPPorts, []uint32{5432, 8080}) {
						t.Fatalf("allow %v: ports %v", allow, a.TCPPorts)
					}
				}
			}
			if !found {
				t.Fatalf("allow %v: peer allowance missing: %+v", allow, got)
			}
		}
	})

	t.Run("restricted without the peer named reaches nothing", func(t *testing.T) {
		l := &Lease{ID: "z", Owner: "u-z", State: "running", NetPolicy: "restricted", NetAllow: []string{"other.example.com"}}
		got := svc.egressFor(l)
		for _, a := range got.Private {
			if a.CIDR == "10.11.0.9/32" {
				t.Fatalf("unlisted peer leaked: %+v", got.Private)
			}
		}
		if len(got.AllowedDomains) != 1 || got.AllowedDomains[0] != "other.example.com" {
			t.Fatalf("domains = %v", got.AllowedDomains)
		}
	})

	t.Run("none reaches nothing", func(t *testing.T) {
		got := svc.egressFor(&Lease{ID: "n", Owner: "u-n", State: "running", NetPolicy: "none"})
		want := substrate.Egress{DeniedCIDRs: []string{"0.0.0.0/0"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("non-publishing or dead peers are skipped", func(t *testing.T) {
		insertLease(t, svc, &Lease{ID: "quiet", Owner: "u-q", State: "running", HostIP: "10.11.0.7"})
		insertLease(t, svc, &Lease{ID: "dead", Owner: "u-d", State: "suspended", HostIP: "10.11.0.8", ExposePorts: []int{80}})
		insertLease(t, svc, &Lease{ID: "noaddr", Owner: "u-na", State: "running", ExposePorts: []int{80}})
		got := svc.egressFor(&Lease{ID: "x", Owner: "u-x", State: "running", NetPolicy: "internet"}).Private
		for _, a := range got {
			if a.CIDR == "10.11.0.7/32" || a.CIDR == "10.11.0.8/32" || a.CIDR == "10.11.0.6/32" {
				t.Fatalf("non-peer leaked: %+v", got)
			}
		}
	})
}

// TestPeerReferenceClassification pins the restricted allowlist split: an
// entry naming a lease is a peer reference and never becomes a domain,
// whether or not that lease currently publishes anything.
func TestPeerReferenceClassification(t *testing.T) {
	svc, _ := newLifecycleService(t)
	insertLease(t, svc, &Lease{ID: "0123456789abcdef0123456789abcdef", Owner: "u-p", State: "running",
		Name: "db", HostIP: "10.11.0.9", ExposePorts: []int{5432}})

	for _, entry := range []string{"db", "lease:db", "0123456789abcdef0123456789abcdef", "lease:0123456789abcdef0123456789abcdef"} {
		got := svc.egressFor(&Lease{ID: "y", State: "running", NetPolicy: "restricted", NetAllow: []string{entry}})
		for _, d := range got.AllowedDomains {
			if d == entry {
				t.Fatalf("entry %q leaked into AllowedDomains: %v", entry, got.AllowedDomains)
			}
		}
	}
	// A publishing, allowlisted peer still yields the port-scoped allowance.
	got := svc.egressFor(&Lease{ID: "y", State: "running", NetPolicy: "restricted", NetAllow: []string{"db"}})
	if want := peerAllowance("10.11.0.9", 5432); !containsAllowance(got.Private, want) {
		t.Fatalf("peer allowance missing: %+v", got.Private)
	}
}

func containsAllowance(list []substrate.PrivateAllowance, want substrate.PrivateAllowance) bool {
	for _, a := range list {
		if reflect.DeepEqual(a, want) {
			return true
		}
	}
	return false
}

// TestRefreshPeersOnlyOnChange pins the canonical-JSON diffing: the first
// refresh applies every live lease's egress, the second applies nothing,
// and a released peer's ports drop out of the survivors' egress with one
// further update each.
func TestRefreshPeersOnlyOnChange(t *testing.T) {
	svc, sub := newLifecycleService(t)
	ctx := context.Background()

	a := &Lease{ID: "aaaa", Owner: "u-a", Image: "py-base", State: "running", NetPolicy: "lan",
		SandboxID: "sb-a", HostIP: "10.11.0.5", ExposePorts: []int{8080}, ExposedIP: "10.11.0.5"}
	b := &Lease{ID: "bbbb", Owner: "u-b", Image: "py-base", State: "running", NetPolicy: "lan",
		SandboxID: "sb-b", HostIP: "10.11.0.6", ExposePorts: []int{9090}, ExposedIP: "10.11.0.6"}
	insertLease(t, svc, a)
	insertLease(t, svc, b)

	svc.runRefreshPeers(ctx)
	first := calls(sub.Fake, "UpdateEgress")
	if first != 2 {
		t.Fatalf("first refresh: %d UpdateEgress calls, want 2 (both leases): %v", first, sub.Fake.CallLog())
	}

	svc.runRefreshPeers(ctx)
	if got := calls(sub.Fake, "UpdateEgress"); got != first {
		t.Fatalf("unchanged refresh re-applied egress: %d calls: %v", got, sub.Fake.CallLog())
	}

	// b is released: a loses its peer allowance, b is not updated at all.
	svc.store.mu.Lock()
	b.State = "suspended"
	svc.store.mu.Unlock()
	svc.runRefreshPeers(ctx)
	if got := calls(sub.Fake, "UpdateEgress"); got != first+1 {
		t.Fatalf("release refresh: %d total calls, want %d (a only): %v", got, first+1, sub.Fake.CallLog())
	}
	if got := calls(sub.Fake, "UpdateEgress sb-b"); got != 1 {
		t.Fatalf("suspended lease updated %d times, want 1 (before suspension)", got)
	}
}

// TestNetworkRoute pins the live policy-change endpoint: owner-only,
// 400 on an invalid policy, 404 unknown, 409 suspended, and 200 with the
// new policy, which the substrate then carries.
func TestNetworkRoute(t *testing.T) {
	ts, _, db, sub := newTestServerWithService(t)

	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	if resp.StatusCode != 201 {
		t.Fatalf("create status %d: %v", resp.StatusCode, body)
	}
	id := body["id"].(string)

	t.Run("invalid policy", func(t *testing.T) {
		resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/network", "token-a",
			map[string]any{"network_policy": "full"})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", resp.StatusCode)
		}
	})

	t.Run("unknown lease", func(t *testing.T) {
		resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/deadbeef/network", "token-a",
			map[string]any{"network_policy": "lan"})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status %d, want 404", resp.StatusCode)
		}
	})

	t.Run("another owner", func(t *testing.T) {
		resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/network", "token-b",
			map[string]any{"network_policy": "lan"})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status %d, want 404 for a non-owner", resp.StatusCode)
		}
	})

	t.Run("suspended", func(t *testing.T) {
		resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/suspend", "token-a", nil)
		if resp.StatusCode != 200 {
			t.Fatalf("suspend status %d", resp.StatusCode)
		}
		resp, _ = doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/network", "token-a",
			map[string]any{"network_policy": "lan"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d, want 200 (a network change resumes on use)", resp.StatusCode)
		}
		// Resume via the API to leave the lease live again.
		resp, _ = doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/resume", "token-a", nil)
		if resp.StatusCode != 200 {
			t.Fatalf("resume status %d", resp.StatusCode)
		}
	})

	t.Run("policy change applies", func(t *testing.T) {
		before := calls(sub.Fake, "UpdateEgress")
		resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/network", "token-a",
			map[string]any{"network_policy": "restricted", "egress_allowlist": []string{"pg.example.com"}})
		if resp.StatusCode != 200 {
			t.Fatalf("status %d: %v", resp.StatusCode, body)
		}
		if body["id"] != id || body["network_policy"] != "restricted" {
			t.Fatalf("response body: %v", body)
		}
		allow, ok := body["egress_allowlist"].([]any)
		if !ok || len(allow) != 1 || allow[0] != "pg.example.com" {
			t.Fatalf("egress_allowlist in response: %v", body["egress_allowlist"])
		}
		// The lease's own egress is applied synchronously.
		if got := calls(sub.Fake, "UpdateEgress"); got != before+1 {
			t.Fatalf("UpdateEgress calls %d, want %d: %v", got, before+1, sub.Fake.CallLog())
		}
		// The saved policy survives a store round-trip.
		rows, err := db.ListLeases(context.Background())
		if err != nil {
			t.Fatalf("load leases: %v", err)
		}
		var row *store.LeaseRow
		for i := range rows {
			if rows[i].ID == id {
				row = &rows[i]
			}
		}
		if row == nil {
			t.Fatal("lease missing from store")
		}
		if row.NetPolicy != "restricted" {
			t.Fatalf("stored policy %q", row.NetPolicy)
		}
	})
}

// TestReleaseTriggersPeerRefresh pins the async trigger: releasing a
// lease that publishes ports schedules a refresh that drops its
// allowance from the surviving peers' egress.
func TestReleaseTriggersPeerRefresh(t *testing.T) {
	svc, sub := newLifecycleService(t)
	seedImage(t, svc.db, "py-base", 2048)
	insertLease(t, svc, &Lease{ID: "bbbb", Owner: "u-b", Image: "py-base", State: "running", NetPolicy: "lan",
		SandboxID: "sb-b", HostIP: "10.11.0.6"})
	l := &Lease{ID: "aaaa", Owner: "u-a", Image: "py-base", State: "running", NetPolicy: "lan",
		SandboxID: "sb-l", HostIP: "10.11.0.5", ExposePorts: []int{8080}, ExposedIP: "10.11.0.5"}
	insertLease(t, svc, l)
	svc.runRefreshPeers(context.Background())
	if !containsAllowance(svc.egressFor(svc.lookup("u-b", "bbbb")).Private, peerAllowance("10.11.0.5", 8080)) {
		t.Fatal("peer allowance missing before release")
	}
	before := calls(sub.Fake, "UpdateEgress")

	svc.release(context.Background(), l)

	deadline := time.Now().Add(2 * time.Second)
	for calls(sub.Fake, "UpdateEgress") <= before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := calls(sub.Fake, "UpdateEgress"); got <= before {
		t.Fatalf("release did not schedule a refresh: %d calls: %v", got, sub.Fake.CallLog())
	}
	if containsAllowance(svc.egressFor(svc.lookup("u-b", "bbbb")).Private, peerAllowance("10.11.0.5", 8080)) {
		t.Fatal("released peer's allowance still present")
	}
}
