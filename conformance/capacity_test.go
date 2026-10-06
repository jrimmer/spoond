//go:build conformance

package conformance

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// Group C: preemption (#128 part 3). C1 fills the host, so it runs only
// with CONFORMANCE_CAPACITY=1 and never on a shared host. It needs a
// second identity user's token (CONFORMANCE_SECOND_TOKEN): the
// production conformance user is not admin and there is no promote API,
// so the suite cannot create one itself.

func requireCapacity(t *testing.T) {
	if os.Getenv("CONFORMANCE_CAPACITY") != "1" {
		skipf(t, "group C requires CONFORMANCE_CAPACITY=1 (it fills the host)")
	}
	if cfg.SecondToken == "" {
		skipf(t, "group C requires CONFORMANCE_SECOND_TOKEN (a second, non-admin identity user)")
	}
}

// withToken runs fn with the client's bearer token temporarily replaced.
// The conformance suite runs sequentially, so this is safe.
func withToken(tok string, fn func()) {
	old := cl.token
	cl.token = tok
	defer func() { cl.token = old }()
	fn()
}

// trackAs registers a lease for deletion with its owner's token in
// t.Cleanup. 404 on delete is ignored.
func trackAs(t *testing.T, token, id string) {
	t.Cleanup(func() {
		withToken(token, func() {
			st, _, err := cl.delete(id)
			if err != nil {
				t.Logf("cleanup: delete %s: %v", id, err)
				return
			}
			if st != 204 && st != 404 {
				t.Logf("cleanup: delete %s: status %d", id, st)
			}
		})
	})
}

// createLeaseAs creates a lease with the given token and registers it
// for cleanup (deleted with the same token).
func createLeaseAs(t *testing.T, token string, body map[string]any) leaseInfo {
	t.Helper()
	var l leaseInfo
	withToken(token, func() {
		st, data, err := cl.create(body)
		if err != nil {
			failf(t, "create %v: %v", body, err)
		}
		if st != 201 {
			failf(t, "create %v: status %d: %s", body, st, truncate(data))
		}
		if err := json.Unmarshal(data, &l); err != nil {
			failf(t, "create %v: bad body: %v", body, err)
		}
		if l.ID == "" {
			failf(t, "create %v: empty id in body %s", body, truncate(data))
		}
	})
	t.Cleanup(func() {
		withToken(token, func() {
			if st, _, err := cl.delete(l.ID); err != nil {
				t.Logf("cleanup: delete %s: %v", l.ID, err)
			} else if st != 204 && st != 404 {
				t.Logf("cleanup: delete %s: status %d", l.ID, st)
			}
		})
	})
	return l
}

// leaseAs fetches GET /api/leases/{id} with the owner's token.
func leaseAs(t *testing.T, token, id string) (map[string]any, bool) {
	t.Helper()
	var m map[string]any
	ok := false
	withToken(token, func() {
		st, body, err := cl.do("GET", "/api/leases/"+id, nil)
		if err != nil || st != 200 {
			return
		}
		if json.Unmarshal(body, &m) == nil {
			ok = true
		}
	})
	return m, ok
}

// TestC1_PreemptionRefillsGuaranteed: a test user (CONFORMANCE_SECOND_TOKEN,
// no lease cap: it must take leases until the host answers 503) fills the
// host with forced-burst leases. A guaranteed lease created by the
// conformance user (also classified guaranteed: it has no guaranteed_mib)
// then preempts the newest burst lease (preempted=true). Deleting the
// guaranteed lease lets the resume queue bring the burst lease back within
// 60 s with its /dev/shm file intact. "burst": true forces the filler's
// class because without a guaranteed_mib the owner would otherwise keep
// every lease guaranteed, and the suite has no admin access to set quotas.
func TestC1_PreemptionRefillsGuaranteed(t *testing.T) {
	requireCapacity(t)
	rec := begin(t)

	image := envOr("CONFORMANCE_PREEMPT_IMAGE", "py-base")
	// The filler must not have a low max_leases: the conformance user has
	// max_leases=20, which would answer 429 before the host's 503, so the
	// second user (uncapped) takes the fill and the conformance user takes
	// the single guaranteed lease.
	burstTok := cfg.SecondToken
	guaranteedTok := cfg.Token

	// Fill the host with burst leases, each carrying a /dev/shm marker.
	marker := randMarker()
	var burstIDs []string
	var last leaseInfo
	for i := 0; ; i++ {
		var st int
		var body []byte
		var err error
		withToken(burstTok, func() {
			st, body, err = cl.create(map[string]any{
				"image": image, "ttl": 600, "persistent": true, "burst": true,
			})
		})
		if err != nil {
			failf(t, "fill create %d: %v", i, err)
		}
		if st == 503 {
			break
		}
		if st == 429 {
			failf(t, "fill create %d: 429 %s: CONFORMANCE_SECOND_TOKEN's user needs no max_leases cap (group C fills the host)", i, truncate(body))
		}
		if st != 201 {
			failf(t, "fill create %d: status %d: %s", i, st, truncate(body))
		}
		var l leaseInfo
		if err := json.Unmarshal(body, &l); err != nil {
			failf(t, "fill create %d: bad body: %v", i, err)
		}
		trackAs(t, burstTok, l.ID)
		burstIDs = append(burstIDs, l.ID)
		last = l
		execOK(t, l.ID, "echo "+marker+" > /dev/shm/capacity-marker")
		if i >= 200 {
			failf(t, "filled 200 burst leases without a 503")
		}
	}
	rec.set("burst_leases", len(burstIDs))
	if len(burstIDs) == 0 {
		failf(t, "no burst lease was admitted before the 503")
	}

	// The conformance user's guaranteed lease cannot fit: it preempts
	// the newest burst lease.
	guaranteed := createLeaseAs(t, guaranteedTok, map[string]any{
		"image": image, "ttl": 600, "persistent": true,
	})

	waitFor(t, 30*time.Second, "the newest burst lease to be preempted", func() bool {
		m, ok := leaseAs(t, burstTok, last.ID)
		return ok && boolField(m, "preempted") && strField(m, "state") == "suspended"
	})
	m, ok := leaseAs(t, burstTok, last.ID)
	if !ok {
		failf(t, "victim %s not readable", last.ID)
	}
	if !boolField(m, "preempted") {
		failf(t, "newest burst lease %s was not preempted", last.ID)
	}
	if strField(m, "state") != "suspended" {
		failf(t, "preempted lease state = %q, want suspended", strField(m, "state"))
	}
	genBefore := int64(numberField(m, "generation"))

	// Delete the guaranteed lease; the burst lease must resume within
	// 60 s with its /dev/shm file intact.
	withToken(guaranteedTok, func() {
		if st, b, err := cl.delete(guaranteed.ID); err != nil || st != 204 {
			failf(t, "delete guaranteed: status %d: %s (%v)", st, truncate(b), err)
		}
	})
	waitFor(t, 60*time.Second, "the preempted burst lease to resume", func() bool {
		m, ok := leaseAs(t, burstTok, last.ID)
		return ok && !boolField(m, "preempted") && !boolField(m, "suspended") && strField(m, "state") == "running"
	})
	if got := execOK(t, last.ID, "cat /dev/shm/capacity-marker"); got != marker {
		failf(t, "/dev/shm marker after resume = %q, want %q", got, marker)
	}
	if m, ok := leaseAs(t, burstTok, last.ID); ok {
		if got := int64(numberField(m, "generation")); got != genBefore {
			failf(t, "generation changed across preemption and resume: %d -> %d", genBefore, got)
		}
	}
}

func strField(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func boolField(m map[string]any, k string) bool {
	if v, ok := m[k].(bool); ok {
		return v
	}
	return false
}

func numberField(m map[string]any, k string) float64 {
	if v, ok := m[k].(float64); ok {
		return v
	}
	return 0
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Second)
	}
	failf(t, "timed out after %s waiting for %s", d, what)
}
