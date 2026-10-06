//go:build conformance

package conformance

import (
	"encoding/json"
	"os"
	"testing"
)

// TestX1_CrashTestRecovers runs one lease through the crash-test
// endpoint as its owner: a checkpointed lease comes back "recovered"
// (generation +1, state from the checkpoint), a lease without one
// becomes "lost". It touches only its own leases, but the backend must
// run with CRASH_TEST=1 (the route is 404 otherwise), so it runs only
// with CONFORMANCE_CRASH_TEST=1.
func TestX1_CrashTestRecovers(t *testing.T) {
	begin(t)
	if os.Getenv("CONFORMANCE_CRASH_TEST") != "1" {
		skipf(t, "the crash test requires CONFORMANCE_CRASH_TEST=1 (and CRASH_TEST=1 on the backend)")
	}

	// A checkpointed lease: /root/a is in the checkpoint, /root/b is not.
	l := createLease(t, map[string]any{"image": "py-base", "persistent": true, "ttl": 3600})
	execOK(t, l.ID, "echo a > /root/a")
	st, body, err := cl.checkpoint(l.ID)
	if err != nil {
		failf(t, "checkpoint: %v", err)
	}
	if st != 200 {
		failf(t, "checkpoint: status %d: %s", st, truncate(body))
	}
	execOK(t, l.ID, "echo b > /root/b")

	var res struct {
		ID         string `json:"id"`
		Result     string `json:"result"`
		Generation int64  `json:"generation"`
		State      string `json:"state"`
	}
	st, body, err = crashLease(l.ID)
	if err != nil {
		failf(t, "crash: %v", err)
	}
	if st != 200 {
		failf(t, "crash: status %d: %s", st, truncate(body))
	}
	if err := json.Unmarshal(body, &res); err != nil {
		failf(t, "crash: bad body %q: %v", truncate(body), err)
	}
	if res.ID != l.ID {
		failf(t, "crash id = %q, want %q", res.ID, l.ID)
	}
	if res.Result != "recovered" {
		failf(t, "crash result = %q, want recovered", res.Result)
	}
	if res.Generation != 2 {
		failf(t, "crash generation = %d, want 2", res.Generation)
	}
	if res.State != "recovered" {
		failf(t, "crash state = %q, want recovered", res.State)
	}
	if got := execOK(t, l.ID, "cat /root/a"); got != "a" {
		failf(t, "checkpointed file /root/a = %q, want a", got)
	}
	if got := execOK(t, l.ID, "cat /root/b 2>/dev/null || echo gone"); got != "gone" {
		failf(t, "post-checkpoint file /root/b = %q, want gone", got)
	}

	// A lease without a checkpoint is lost.
	bare := createLease(t, map[string]any{"image": "py-base", "persistent": true, "ttl": 3600})
	st, body, err = crashLease(bare.ID)
	if err != nil {
		failf(t, "crash bare: %v", err)
	}
	if st != 200 {
		failf(t, "crash bare: status %d: %s", st, truncate(body))
	}
	if err := json.Unmarshal(body, &res); err != nil {
		failf(t, "crash bare: bad body %q: %v", truncate(body), err)
	}
	if res.Result != "lost" || res.State != "lost" {
		failf(t, "crash bare = %+v, want lost", res)
	}

	for _, id := range []string{l.ID, bare.ID} {
		st, body, err := cl.delete(id)
		if err != nil {
			failf(t, "delete %s: %v", id, err)
		}
		if st != 204 && st != 404 {
			failf(t, "delete %s: status %d: %s", id, st, truncate(body))
		}
	}
}

// crashLease POSTs the crash-test route for a lease with the client's
// own token: the conformance user owns the lease.
func crashLease(id string) (int, []byte, error) {
	return cl.do("POST", "/api/leases/"+id+"/crash-test", nil)
}
