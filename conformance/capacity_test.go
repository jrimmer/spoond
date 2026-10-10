//go:build conformance

package conformance

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// Group C: 3.0 fair shares (take-back and box_full). Every owner has an
// equal slice of the box; a create that does not fit pauses the least
// recently used lease of the owner furthest over its slice (take-back),
// and the paused lease returns on its holder's next work call. An owner
// that already holds at least its slice and is the biggest borrower is
// refused at once with 429 box_full. C1 fills the host, so group C runs
// only with CONFORMANCE_CAPACITY=1 and never on a shared host. It needs
// a second identity user's token (CONFORMANCE_SECOND_TOKEN): the
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

// leasesAs lists the owner's leases with their token.
func leasesAs(t *testing.T, token string) []leaseInfo {
	t.Helper()
	var out []leaseInfo
	withToken(token, func() {
		st, body, err := cl.list()
		if err != nil || st != 200 {
			return
		}
		var resp struct {
			Sandboxes []leaseInfo `json:"sandboxes"`
		}
		if json.Unmarshal(body, &resp) == nil {
			out = resp.Sandboxes
		}
	})
	return out
}

// errorCode reads the machine-readable "code" from a refusal body.
func errorCode(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	if c, ok := m["code"].(string); ok {
		return c
	}
	return ""
}

// filler is one lease the second user created to fill the host: its id
// and, when writeMarkers was set, its /dev/shm marker value.
type filler struct {
	id     string
	marker string
}

// fillHostAs fills the host as token until a create is refused with 429
// box_full (the biggest-borrower refusal) or 503 (no room take-back can
// close). Each lease is persistent and, when writeMarkers is set,
// carries a /dev/shm marker. It fails on any other 429 (there are no
// per-owner caps left in 3.0) and after 300 admitted leases. The leases
// are registered for cleanup.
func fillHostAs(t *testing.T, token, image string, writeMarkers bool) []filler {
	t.Helper()
	var out []filler
	for i := 0; ; i++ {
		body := map[string]any{"image": image, "ttl": 600, "persistent": true}
		var st int
		var data []byte
		var err error
		withToken(token, func() {
			st, data, err = cl.create(body)
		})
		if err != nil {
			failf(t, "fill create %d: %v", i, err)
		}
		switch st {
		case 503:
			return out
		case 429:
			if code := errorCode(data); code != "box_full" {
				failf(t, "fill create %d: 429 %s, want box_full (3.0 has no per-owner caps)", i, truncate(data))
			}
			return out
		case 201:
		default:
			failf(t, "fill create %d: status %d: %s", i, st, truncate(data))
		}
		var l leaseInfo
		if err := json.Unmarshal(data, &l); err != nil {
			failf(t, "fill create %d: bad body: %v", i, err)
		}
		trackAs(t, token, l.ID)
		f := filler{id: l.ID}
		if writeMarkers {
			f.marker = randMarker()
			withToken(token, func() {
				execOK(t, l.ID, "echo "+f.marker+" > /dev/shm/capacity-marker")
			})
		}
		out = append(out, f)
		if i >= 300 {
			failf(t, "filled 300 leases without a refusal")
		}
	}
}

// TestC1_TakeBackPausesBiggestBorrower: the second user fills the host
// with persistent leases, each carrying a /dev/shm marker, until a
// create is refused. The conformance user's create does not fit the full
// host: take-back pauses the least recently used lease of the second
// user (the biggest borrower) with suspend_reason take_back. Deleting
// the conformance lease makes room; exec on the paused lease as its
// holder resumes it with its marker intact and its generation unchanged.
func TestC1_TakeBackPausesBiggestBorrower(t *testing.T) {
	requireCapacity(t)
	rec := begin(t)

	image := envOr("CONFORMANCE_CAPACITY_IMAGE", "py-base")
	secondTok := cfg.SecondToken
	confTok := cfg.Token

	fills := fillHostAs(t, secondTok, image, true)
	rec.set("second_leases", len(fills))
	markers := map[string]string{}
	for _, f := range fills {
		markers[f.id] = f.marker
	}

	// The conformance user's create cannot fit the full host: it takes
	// back room from the owner furthest over its slice.
	conformance := createLeaseAs(t, confTok, map[string]any{
		"image": image, "ttl": 600, "persistent": true,
	})

	var victim leaseInfo
	waitFor(t, 30*time.Second, "a second-user lease paused by take_back", func() bool {
		for _, l := range leasesAs(t, secondTok) {
			if markers[l.ID] != "" && l.State == "suspended" && l.SuspendReason == "take_back" {
				victim = l
				return true
			}
		}
		return false
	})
	if victim.ID == "" {
		failf(t, "no second-user lease reported state suspended with suspend_reason take_back")
	}
	rec.set("victim", victim.ID)
	genBefore := victim.Generation

	// Delete the conformance lease; the paused lease comes back on its
	// holder's next work call.
	withToken(confTok, func() {
		if st, b, err := cl.delete(conformance.ID); err != nil || st != 204 {
			failf(t, "delete conformance lease: status %d: %s (%v)", st, truncate(b), err)
		}
	})

	var gotMarker string
	withToken(secondTok, func() {
		gotMarker = execOK(t, victim.ID, "cat /dev/shm/capacity-marker")
	})
	if gotMarker != markers[victim.ID] {
		failf(t, "/dev/shm marker after resume = %q, want %q", gotMarker, markers[victim.ID])
	}
	m, ok := leaseAs(t, secondTok, victim.ID)
	if !ok {
		failf(t, "victim %s not readable after resume", victim.ID)
	}
	if strField(m, "state") != "running" {
		failf(t, "victim state after resume = %q, want running", strField(m, "state"))
	}
	if strField(m, "suspend_reason") != "" {
		failf(t, "victim suspend_reason after resume = %q, want empty", strField(m, "suspend_reason"))
	}
	if got := int64(numberField(m, "generation")); got != genBefore {
		failf(t, "generation changed across take-back and resume: %d -> %d", genBefore, got)
	}
}

// TestC2_BiggestBorrowerBoxFull: with the host filled by the second
// user, its next create with "wait": 60 is refused at once with 429
// box_full: the requester is the biggest borrower, so there is no one to
// take from and nothing to wait for.
func TestC2_BiggestBorrowerBoxFull(t *testing.T) {
	requireCapacity(t)
	rec := begin(t)

	image := envOr("CONFORMANCE_CAPACITY_IMAGE", "py-base")
	secondTok := cfg.SecondToken

	fills := fillHostAs(t, secondTok, image, false)
	rec.set("second_leases", len(fills))

	start := time.Now()
	var st int
	var body []byte
	var err error
	withToken(secondTok, func() {
		st, body, err = cl.create(map[string]any{
			"image": image, "ttl": 600, "persistent": true, "wait": 60,
		})
	})
	elapsed := time.Since(start)
	if err != nil {
		failf(t, "create with wait %v", err)
	}
	if st != 429 {
		failf(t, "create with wait on a full host = %d, want 429 box_full: %s", st, truncate(body))
	}
	if code := errorCode(body); code != "box_full" {
		failf(t, "create with wait code = %q, want box_full: %s", code, truncate(body))
	}
	if elapsed > 5*time.Second {
		failf(t, "box_full refusal took %s, want it refused at once", elapsed)
	}
	rec.set("box_full_ms", elapsed.Milliseconds())
}

// TestC3_WaitOnIdleHostAnswersAtOnce: a create with `"wait": 30` on an
// idle host is admitted at once with waited_ms below 1000 (#129 part 1),
// always on and outside group C.
func TestC3_WaitOnIdleHostAnswersAtOnce(t *testing.T) {
	rec := begin(t)
	image := envOr("CONFORMANCE_IDLE_IMAGE", "py-base")

	start := time.Now()
	st, body, err := cl.create(map[string]any{"image": image, "ttl": 60, "wait": 30})
	if err != nil {
		failf(t, "create with wait: %v", err)
	}
	elapsed := time.Since(start)
	if st != 201 {
		failf(t, "idle-host create with wait = %d, want 201: %s", st, truncate(body))
	}
	var l leaseInfo
	if err := json.Unmarshal(body, &l); err != nil {
		failf(t, "idle-host create: bad body: %v", err)
	}
	trackAs(t, cfg.Token, l.ID)
	if l.WaitedMS >= 1000 {
		failf(t, "idle-host create waited_ms = %d, want < 1000", l.WaitedMS)
	}
	if elapsed > 5*time.Second {
		failf(t, "idle-host create took %s, want it admitted at once", elapsed)
	}
	rec.set("waited_ms", l.WaitedMS)
}

func strField(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
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
