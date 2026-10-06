//go:build conformance

package conformance

import (
	"encoding/json"
	"testing"
)

// TestX1_AdminCrashTest runs one lease through the admin crash-test
// endpoint: a checkpointed lease comes back "recovered" (generation +1,
// state from the checkpoint), a lease without one becomes "lost". It is
// always on — it touches only its own leases — but needs
// CONFORMANCE_ADMIN_TOKEN (the backend's ADMIN_TOKEN) and is skipped
// without it.
func TestX1_AdminCrashTest(t *testing.T) {
	begin(t)
	if cfg.AdminToken == "" {
		skipf(t, "the admin crash test requires CONFORMANCE_ADMIN_TOKEN")
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
	st, body, err = crashAsAdmin(t, l.ID)
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
	st, body, err = crashAsAdmin(t, bare.ID)
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

// crashAsAdmin POSTs the admin crash-test route with the ADMIN_TOKEN,
// leaving the client's bearer token untouched for the owner-scoped calls
// around it.
func crashAsAdmin(t *testing.T, id string) (int, []byte, error) {
	t.Helper()
	var st int
	var body []byte
	var err error
	withToken(cfg.AdminToken, func() {
		st, body, err = cl.do("POST", "/api/admin/leases/"+id+"/crash", nil)
	})
	return st, body, err
}
