//go:build conformance

package conformance

import (
	"encoding/json"
	"testing"
	"time"
)

// TestD1_PrivateWritableDisks gives two sandboxes different markers on the
// same image path and checks each reads back its own. On e2b it also
// checks the host that no file of the template build was touched.
func TestD1_PrivateWritableDisks(t *testing.T) {
	begin(t)

	if cfg.Substrate == "e2b" {
		// Reference file for the mtime check, stamped before any create.
		hostRun(t, "touch /tmp/.conformance-d1-ref")
	}

	l1 := createLease(t, map[string]any{"image": "py-base", "ttl": 600})
	l2 := createLease(t, map[string]any{"image": "py-base", "ttl": 600})

	m1, m2 := randMarker(), randMarker()
	execOK(t, l1.ID, "echo "+m1+" > /etc/conformance-marker && sync")
	execOK(t, l2.ID, "echo "+m2+" > /etc/conformance-marker && sync")

	if got := execOK(t, l1.ID, "cat /etc/conformance-marker"); got != m1 {
		failf(t, "lease 1 marker: got %q, want %q", got, m1)
	}
	if got := execOK(t, l2.ID, "cat /etc/conformance-marker"); got != m2 {
		failf(t, "lease 2 marker: got %q, want %q", got, m2)
	}

	if cfg.Substrate != "e2b" {
		return
	}
	st, body, err := cl.images("?detail=1")
	if err != nil {
		failf(t, "images?detail=1: %v", err)
	}
	if st != 200 {
		failf(t, "images?detail=1: status %d: %s", st, truncate(body))
	}
	var detail struct {
		Images []imageDetail `json:"images"`
	}
	if err := json.Unmarshal(body, &detail); err != nil {
		failf(t, "images?detail=1: bad body: %v", err)
	}
	buildID := ""
	for _, im := range detail.Images {
		if im.Name == "py-base" {
			buildID = im.BuildID
		}
	}
	if buildID == "" {
		failf(t, "images?detail=1: no py-base entry: %s", truncate(body))
	}
	dir := "/forkdcache/e2b/storage/templates/" + buildID
	n := hostRun(t, "test -d "+dir+" && find "+dir+" -type f -newer /tmp/.conformance-d1-ref | wc -l || echo 0")
	if n != "0" {
		failf(t, "%q files under %s were modified during the test", n, dir)
	}
	hostRun(t, "rm -f /tmp/.conformance-d1-ref")
}

// TestD2_CloneOutlivesSource checks that a clone keeps running and keeps
// its data after the source lease is deleted.
func TestD2_CloneOutlivesSource(t *testing.T) {
	begin(t)
	src := createLease(t, map[string]any{"image": "py-base", "persistent": true, "ttl": 600})
	m := randMarker()
	execOK(t, src.ID, "echo "+m+" > /root/marker")

	st, body, err := cl.clone(src.ID, map[string]any{})
	if err != nil {
		failf(t, "clone: %v", err)
	}
	if st != 201 {
		failf(t, "clone: status %d: %s", st, truncate(body))
	}
	var cloned leaseInfo
	if err := json.Unmarshal(body, &cloned); err != nil || cloned.ID == "" {
		failf(t, "clone: bad body %q: %v", truncate(body), err)
	}
	track(t, cloned.ID)

	st, body, err = cl.delete(src.ID)
	if err != nil {
		failf(t, "delete source: %v", err)
	}
	if st != 204 {
		failf(t, "delete source: status %d: %s", st, truncate(body))
	}

	time.Sleep(5 * time.Second)
	if got := execOK(t, cloned.ID, "cat /root/marker"); got != m {
		failf(t, "clone marker after source delete: got %q, want %q", got, m)
	}

	st, body, err = cl.suspend(cloned.ID)
	if err != nil {
		failf(t, "suspend clone: %v", err)
	}
	if st != 200 {
		failf(t, "suspend clone: status %d: %s", st, truncate(body))
	}
	st, body, err = cl.resume(cloned.ID)
	if err != nil {
		failf(t, "resume clone: %v", err)
	}
	if st != 200 {
		failf(t, "resume clone: status %d: %s", st, truncate(body))
	}
	if got := execOK(t, cloned.ID, "cat /root/marker"); got != m {
		failf(t, "clone marker after resume: got %q, want %q", got, m)
	}
}

// TestD3_SnapshotDeletion exercises the U10 checkpoint route and the U11
// snapshot deletion rules. Requires U10/U11; on forkd it fails (routes
// absent), which is the recorded baseline result.
func TestD3_SnapshotDeletion(t *testing.T) {
	begin(t)
	a := createLease(t, map[string]any{"image": "py-base", "persistent": true, "ttl": 600})

	checkpointBuild := func() string {
		st, body, err := cl.checkpoint(a.ID)
		if err != nil {
			failf(t, "checkpoint: %v", err)
		}
		if st != 200 {
			failf(t, "checkpoint: status %d: %s", st, truncate(body))
		}
		var cp struct {
			ID      string `json:"id"`
			BuildID string `json:"build_id"`
			At      string `json:"at"`
		}
		if err := json.Unmarshal(body, &cp); err != nil || cp.BuildID == "" {
			failf(t, "checkpoint: bad body %q: %v", truncate(body), err)
		}
		return cp.BuildID
	}
	c1 := checkpointBuild()
	c2 := checkpointBuild()

	st, body, err := cl.deleteSnapshot(c1)
	if err != nil {
		failf(t, "delete snapshot %s: %v", c1, err)
	}
	if st != 409 {
		failf(t, "delete snapshot %s (ancestor of the running build): status %d, want 409: %s", c1, st, truncate(body))
	}
	st, body, err = cl.deleteSnapshot(c2)
	if err != nil {
		failf(t, "delete snapshot %s: %v", c2, err)
	}
	if st != 409 {
		failf(t, "delete snapshot %s (the running build): status %d, want 409: %s", c2, st, truncate(body))
	}

	b := createLease(t, map[string]any{"image": "py-base", "persistent": true, "ttl": 600})
	st, body, err = cl.suspend(b.ID)
	if err != nil {
		failf(t, "suspend B: %v", err)
	}
	if st != 200 {
		failf(t, "suspend B: status %d: %s", st, truncate(body))
	}
	st, body, err = cl.do("GET", "/api/sandboxes/"+b.ID, nil)
	if err != nil {
		failf(t, "get B: %v", err)
	}
	if st != 200 {
		failf(t, "get B: status %d: %s", st, truncate(body))
	}
	var det leaseInfo
	if err := json.Unmarshal(body, &det); err != nil {
		failf(t, "get B: bad body: %v", err)
	}
	p := det.ResumeBuildID
	if p == "" {
		failf(t, "get B: resume_build_id empty: %s", truncate(body))
	}

	st, body, err = cl.delete(b.ID)
	if err != nil {
		failf(t, "delete B: %v", err)
	}
	if st != 204 {
		failf(t, "delete B: status %d: %s", st, truncate(body))
	}

	st, body, err = cl.deleteSnapshot(p)
	if err != nil {
		failf(t, "delete snapshot %s: %v", p, err)
	}
	if st != 204 {
		failf(t, "delete snapshot %s (released pause build): status %d, want 204: %s", p, st, truncate(body))
	}
	st, body, err = cl.deleteSnapshot(p)
	if err != nil {
		failf(t, "re-delete snapshot %s: %v", p, err)
	}
	if st != 404 {
		failf(t, "re-delete snapshot %s: status %d, want 404: %s", p, st, truncate(body))
	}
}
