//go:build conformance

package conformance

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestI1_IdleSuspendResumesOnNextUse covers per-lease idle reclamation
// (2.5, #129 part 2) end to end: a persistent lease with idle_suspend 60
// and a /dev/shm marker, no activity. The idle sweep suspends it within
// the threshold (watched on the event stream as an idle_suspended
// event), and an exec resumes it first through the normal pause/resume
// path with the marker intact. Always on; the lease is deleted at the
// end.
func TestI1_IdleSuspendResumesOnNextUse(t *testing.T) {
	rec := begin(t)

	image := envOr("CONFORMANCE_IDLE_IMAGE", "py-base")
	l := createLease(t, map[string]any{
		"image": image, "ttl": 600, "persistent": true, "idle_suspend": 60,
	})
	rec.set("lease", l.ID)

	// A marker on /dev/shm proves the guest memory survived the pause and
	// the resume: /dev/shm is tmpfs in the guest's RAM.
	marker := randMarker()
	execOK(t, l.ID, "echo "+marker+" > /dev/shm/idle-marker")

	// The event stream must carry the idle_suspended event. Open it before
	// waiting so the event cannot be missed, then wait up to 150 s.
	suspended := make(chan time.Duration, 1)
	go watchIdleSuspended(l.ID, suspended)
	select {
	case d := <-suspended:
		rec.set("idle_suspend_ms", d.Milliseconds())
	case <-time.After(150 * time.Second):
		failf(t, "no idle_suspended event within 150s")
	}

	if !leaseSuspended(t, l.ID) {
		failf(t, "lease %s is not suspended after the idle sweep", l.ID)
	}

	// The next exec resumes it first and then serves the call.
	t0 := time.Now()
	if got := execOK(t, l.ID, "cat /dev/shm/idle-marker"); got != marker {
		failf(t, "/dev/shm marker after auto-resume = %q, want %q", got, marker)
	}
	rec.set("resume_ms", time.Since(t0).Milliseconds())
	if leaseSuspended(t, l.ID) {
		failf(t, "lease %s still suspended after the auto-resuming exec", l.ID)
	}

	// Delete it (the conformance harness also tracks it for cleanup).
	st, body, err := cl.delete(l.ID)
	if err != nil {
		failf(t, "delete: %v", err)
	}
	if st != 204 {
		failf(t, "delete: status %d: %s", st, truncate(body))
	}
}

// watchIdleSuspended subscribes to the lease's events and reports how
// long the watch ran when it sees an idle_suspended event. It gives up
// when the stream ends or the test finishes.
func watchIdleSuspended(leaseID string, out chan<- time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), 160*time.Second)
	defer cancel()
	start := time.Now()
	url := cl.base + "/api/leases/events?lease_id=" + leaseID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+cl.token)
	resp, err := cl.hc.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		if strings.Contains(line, `"type":"idle_suspended"`) {
			out <- time.Since(start)
			return
		}
	}
}

// leaseSuspended reads a lease's suspended state from the API.
func leaseSuspended(t *testing.T, id string) bool {
	t.Helper()
	st, body, err := cl.do("GET", "/api/leases/"+id, nil)
	if err != nil || st != 200 {
		failf(t, "GET lease %s: status %d: %v %s", id, st, err, truncate(body))
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		failf(t, "lease %s: bad body: %v", id, err)
	}
	if s, _ := m["state"].(string); s == "suspended" {
		return true
	}
	if b, _ := m["suspended"].(bool); b {
		return true
	}
	return false
}
