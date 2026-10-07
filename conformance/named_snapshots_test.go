//go:build conformance

package conformance

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Group S7 (2.7, #83): named snapshots. Always on and non-destructive: it
// creates and deletes only its own leases and snapshot name, and a
// snapshot save checkpoints a lease without stopping it. It covers the
// spec's save/replay/start/retention/delete flow end to end:
//
//  1. save a lease that wrote a marker file, with an idempotency key;
//  2. replay the save: same version, 200;
//  3. start from the name: the marker is present, /run/spoond/lease-id is
//     the new id, /run/secrets holds only the new lease's create-time
//     secrets, and the guest clock is within 1 s of the API host's;
//  4. save again: latest moves to 2, and @1 still starts;
//  5. `keep: 1` drops v1 once no live lease uses it;
//  6. deleting a version a live lease started from answers 409;
//  7. deleting it after that lease is released answers 204.
//
// Every lease and the name are cleaned up, also on failure.

// snapshotSaveResult is the save response shape.
type snapshotSaveResult struct {
	Name      string `json:"name"`
	Version   int64  `json:"version"`
	BuildID   string `json:"build_id"`
	Image     string `json:"image"`
	MemoryMB  int64  `json:"memory_mb"`
	SizeBytes int64  `json:"size_bytes"`
	CreatedAt string `json:"created_at"`
}

// namedSnapshotEntry is one name in the list response.
type namedSnapshotEntry struct {
	Name     string `json:"name"`
	Latest   int64  `json:"latest"`
	Versions []struct {
		Version int64 `json:"version"`
		InUse   int64 `json:"in_use"`
	} `json:"versions"`
}

// trackNamedSnapshot registers a best-effort `DELETE {name}` (all
// versions) with ?force=1 in t.Cleanup, ignoring 404. Register it before
// the leases: cleanups run LIFO, so the leases are deleted first and the
// forced delete does not block on one that still uses a version.
func trackNamedSnapshot(t *testing.T, name string) {
	t.Cleanup(func() {
		st, _, err := cl.deleteNamedSnapshot(name, true)
		if err != nil {
			t.Logf("cleanup: delete snapshot %s: %v", name, err)
			return
		}
		if st != 204 && st != 404 {
			t.Logf("cleanup: delete snapshot %s: status %d", name, st)
		}
	})
}

// saveNamedSnapshotOK saves the lease and fails the test unless it
// answers the wanted status, returning the decoded row.
func saveNamedSnapshotOK(t *testing.T, lease, name, key string, keep, want int) snapshotSaveResult {
	t.Helper()
	st, body, err := cl.saveNamedSnapshot(lease, name, key, keep)
	if err != nil {
		failf(t, "save %s as %s: %v", lease, name, err)
	}
	if st != want {
		failf(t, "save %s as %s: status %d, want %d: %s", lease, name, st, want, truncate(body))
	}
	var row snapshotSaveResult
	if err := json.Unmarshal(body, &row); err != nil || row.Name != name || row.Version == 0 || row.BuildID == "" {
		failf(t, "save %s as %s: bad body %q: %v", lease, name, truncate(body), err)
	}
	return row
}

// namedSnapshotLatest returns one name's entry from the list route for
// the given prefix, and whether the name was present.
func namedSnapshotLatest(t *testing.T, prefix, name string) (namedSnapshotEntry, bool) {
	t.Helper()
	st, body, err := cl.listNamedSnapshots(prefix)
	if err != nil || st != 200 {
		failf(t, "list snapshots %q: status %d: %v %s", prefix, st, err, truncate(body))
	}
	var list struct {
		Snapshots []namedSnapshotEntry `json:"snapshots"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		failf(t, "list snapshots: bad body %q: %v", truncate(body), err)
	}
	for _, e := range list.Snapshots {
		if e.Name == name {
			return e, true
		}
	}
	return namedSnapshotEntry{}, false
}

// guestSecrets lists /run/secrets in the lease, sorted, space-joined.
func guestSecrets(t *testing.T, id string) string {
	t.Helper()
	out := execOK(t, id, "ls -1 /run/secrets 2>/dev/null | sort | tr '\\n' ' '")
	return strings.TrimSpace(out)
}

// TestS7_NamedSnapshots covers the save, replay, start-from, retention
// and delete flow.
func TestS7_NamedSnapshots(t *testing.T) {
	rec := begin(t)

	name := "conf-" + randMarker()
	trackNamedSnapshot(t, name)

	// A stray version from a failed run would break the version numbers:
	// delete any existing name first.
	if st, _, err := cl.deleteNamedSnapshot(name, true); err != nil {
		failf(t, "pre-clean %s: %v", name, err)
	} else if st != 204 && st != 404 {
		failf(t, "pre-clean %s: status %d", name, st)
	}

	// 1. A source lease with a marker file and a create-time secret.
	src := createLease(t, map[string]any{
		"image":      "py-base",
		"persistent": true,
		"ttl":        600,
		"secrets":    map[string]string{"src": "source-secret"},
	})
	marker := randMarker()
	execOK(t, src.ID, "echo "+marker+" > /root/snapshot-marker && sync")

	// 1. Save it with a key.
	key := "conformance/" + name
	t0 := time.Now()
	v1 := saveNamedSnapshotOK(t, src.ID, name, key, 0, 201)
	rec.set("save_ms", time.Since(t0).Milliseconds())
	if v1.Version != 1 {
		failf(t, "first save version = %d, want 1", v1.Version)
	}
	if v1.Image != "py-base" || v1.MemoryMB <= 0 {
		failf(t, "first save row = %+v, want image py-base and a memory size", v1)
	}

	// 2. Replay the same key: the same version, 200, no new version.
	replay := saveNamedSnapshotOK(t, src.ID, name, key, 0, 200)
	if replay.Version != v1.Version || replay.BuildID != v1.BuildID {
		failf(t, "replay = v%d %s, want v%d %s", replay.Version, replay.BuildID, v1.Version, v1.BuildID)
	}

	// 3. Start from the name: the marker is present, the lease id is the
	//    copy's, /run/secrets holds only the copy's create-time secret,
	//    and the guest clock is within 1 s of the API host's.
	t0 = time.Now()
	copy1 := createLease(t, map[string]any{
		"snapshot": name,
		"secrets":  map[string]string{"dst": "copy-secret"},
	})
	rec.set("start_from_ms", time.Since(t0).Milliseconds())
	if copy1.ID == src.ID {
		failf(t, "start from snapshot reused the source id")
	}
	if copy1.Snapshot == nil || copy1.Snapshot.Name != name || copy1.Snapshot.Version != 1 {
		failf(t, "copy snapshot object = %+v, want %s@1", copy1.Snapshot, name)
	}
	if got := execOK(t, copy1.ID, "cat /root/snapshot-marker 2>/dev/null || echo missing"); got != marker {
		failf(t, "copy marker = %q, want %q", got, marker)
	}
	if got := execOK(t, copy1.ID, "cat /run/spoond/lease-id"); got != copy1.ID {
		failf(t, "copy /run/spoond/lease-id = %q, want %q", got, copy1.ID)
	}
	if g := leaseGeneration(t, copy1.ID); g != 1 {
		failf(t, "copy generation = %d, want 1", g)
	}
	if got := guestSecrets(t, copy1.ID); got != "dst" {
		failf(t, "copy /run/secrets = %q, want only the copy's create-time secret \"dst\"", got)
	}

	// The guest clock, within 1 s of the API host's clock (Date header).
	// Sample the host clock immediately before and after the guest exec so
	// a slow exec does not look like skew.
	hostBefore, err := cl.hostClock()
	if err != nil {
		failf(t, "host clock: %v", err)
	}
	guestEpoch, err := strconv.ParseInt(strings.TrimSpace(execOK(t, copy1.ID, "date +%s")), 10, 64)
	if err != nil {
		failf(t, "guest clock not epoch seconds: %v", err)
	}
	hostAfter, err := cl.hostClock()
	if err != nil {
		failf(t, "host clock: %v", err)
	}
	if guestEpoch < hostBefore.Unix()-1 || guestEpoch > hostAfter.Unix()+1 {
		failf(t, "guest clock %d s is more than 1 s from the host (window %d..%d)", guestEpoch, hostBefore.Unix(), hostAfter.Unix())
	}
	rec.set("clock_skew_s", guestEpoch-hostBefore.Unix())

	// 4. Save again with a new key: latest moves to 2.
	v2 := saveNamedSnapshotOK(t, src.ID, name, key+"-2", 0, 201)
	if v2.Version != 2 {
		failf(t, "second save version = %d, want 2", v2.Version)
	}
	if e, ok := namedSnapshotLatest(t, "", name); !ok {
		failf(t, "list has no %s", name)
	} else if e.Latest != 2 {
		failf(t, "list latest = %d, want 2", e.Latest)
	}

	// @1 still starts.
	copyAt1 := createLease(t, map[string]any{"snapshot": name + "@1"})
	if copyAt1.Snapshot == nil || copyAt1.Snapshot.Version != 1 {
		failf(t, "start @1 snapshot = %+v, want version 1", copyAt1.Snapshot)
	}
	if got := execOK(t, copyAt1.ID, "cat /root/snapshot-marker 2>/dev/null || echo missing"); got != marker {
		failf(t, "copy @1 marker = %q, want %q", got, marker)
	}

	// 5. Release every lease started from v1, then `keep: 1` drops it.
	for _, id := range []string{copy1.ID, copyAt1.ID} {
		st, body, err := cl.delete(id)
		if err != nil || st != 204 {
			failf(t, "delete %s: status %d: %v %s", id, st, err, truncate(body))
		}
	}
	if st, body, err := cl.setNamedSnapshotKeep(name, 1); err != nil || st != 200 {
		failf(t, "set keep 1: status %d: %v %s", st, err, truncate(body))
	}
	if st, _, err := cl.showNamedSnapshot(name + "@1"); err != nil {
		failf(t, "show @1: %v", err)
	} else if st != 404 {
		failf(t, "show @1 after keep 1: status %d, want 404", st)
	}
	if e, ok := namedSnapshotLatest(t, "", name); !ok {
		failf(t, "list has no %s after keep 1", name)
	} else if e.Latest != 2 || len(e.Versions) != 1 {
		failf(t, "after keep 1: latest %d with %d versions, want latest 2 with 1", e.Latest, len(e.Versions))
	}

	// 6. Delete a version a live lease started from: 409.
	copy2 := createLease(t, map[string]any{"snapshot": name + "@2"})
	if st, body, err := cl.deleteNamedSnapshot(name+"@2", false); err != nil {
		failf(t, "delete in-use @2: %v", err)
	} else if st != 409 {
		failf(t, "delete in-use @2: status %d, want 409: %s", st, truncate(body))
	}

	// 7. Release the lease, then the delete answers 204.
	st, body, err := cl.delete(copy2.ID)
	if err != nil || st != 204 {
		failf(t, "delete copy2: status %d: %v %s", st, err, truncate(body))
	}
	if st, body, err := cl.deleteNamedSnapshot(name+"@2", false); err != nil {
		failf(t, "delete @2: %v", err)
	} else if st != 204 {
		failf(t, "delete @2: status %d, want 204: %s", st, truncate(body))
	}
}
