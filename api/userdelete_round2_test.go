package api

// Round-2 review follow-ups for spoond-q4j (user delete cleanup):
// a clone/fork that commits after its owner was deleted must be refused
// and leave no sandbox behind (B1), a named save that reaches its insert
// after the owner's snapshots were dropped must refuse (S1), an unknown
// or legacy-token id must not be deleted (S2), and a lifecycle handler
// that meets errOwnerGone answers 403 (S3).

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
)

// TestCloneRefusesDeletedOwner: a clone that reserved quota and then
// spent seconds in checkpointLease must not commit an ownerless lease
// when the owner was deleted in that window. The source lease survives
// and no clone sandbox is left on the substrate (spoond-q4j B1).
func TestCloneRefusesDeletedOwner(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	const owner = "u-clone-gone"

	src, err := svc.grant(ctx, owner, "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant source: %v", err)
	}

	started := make(chan struct{})
	unblock := make(chan struct{})
	sub.checkpointFn = func(ctx context.Context, sandboxID string) (string, substrate.BuildRefs, error) {
		close(started)
		<-unblock
		return "b-clone-gone", substrate.BuildRefs{}, nil
	}
	t.Cleanup(func() { sub.checkpointFn = nil })

	type result struct {
		lease *Lease
		err   error
	}
	done := make(chan result, 1)
	go func() {
		l, _, err := svc.clone(ctx, owner, src.ID)
		done <- result{l, err}
	}()
	<-started
	// The delete lands while the clone is in its checkpoint.
	svc.markOwnerDeleted(owner)
	close(unblock)

	got := <-done
	if !errors.Is(got.err, errOwnerGone) {
		t.Fatalf("clone error = %v, want errOwnerGone", got.err)
	}
	if got.lease != nil {
		t.Fatalf("clone returned lease %+v, want nil", got.lease)
	}
	if ls := svc.leasesOfOwner(owner); len(ls) != 1 || ls[0].ID != src.ID {
		t.Fatalf("leases of deleted owner = %v, want only the source", ls)
	}
	// Exactly the source sandbox remains; the clone's fresh sandbox was
	// stopped, not leaked.
	sbs, err := sub.List(ctx)
	if err != nil {
		t.Fatalf("list sandboxes: %v", err)
	}
	if len(sbs) != 1 || sbs[0].ID != src.SandboxID {
		t.Fatalf("sandboxes after refused clone = %v, want only %s", sbs, src.SandboxID)
	}
}

// TestForkRefusesDeletedOwnerRollsBack: a fork that meets the owner-delete
// mark on its second child must roll back the first child too, stop each
// sandbox exactly once, and leave no fork lease behind (spoond-q4j B1).
func TestForkRefusesDeletedOwnerRollsBack(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	const owner = "u-fork-gone"

	src, err := svc.grant(ctx, owner, "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant source: %v", err)
	}

	var mu sync.Mutex
	childIDs := []string{}
	createCalls := 0
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		mu.Lock()
		createCalls++
		call := createCalls
		mu.Unlock()
		if call == 2 {
			// The delete lands while the second child's sandbox is
			// being created, after the first child committed.
			svc.markOwnerDeleted(owner)
		}
		childIDs = append(childIDs, req.SandboxID)
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	deletes := map[string]int{}
	sub.deleteFn = func(ctx context.Context, id string) error {
		mu.Lock()
		deletes[id]++
		mu.Unlock()
		return sub.Fake.Delete(ctx, id)
	}
	t.Cleanup(func() { sub.deleteFn = nil })

	_, _, err = svc.fork(ctx, owner, src.ID, 2, true, time.Minute, "", "")
	if !errors.Is(err, errOwnerGone) {
		t.Fatalf("fork error = %v, want errOwnerGone", err)
	}
	// Only the source survives, and its sandbox is only one.
	if ls := svc.leasesOfOwner(owner); len(ls) != 1 || ls[0].ID != src.ID {
		t.Fatalf("leases of deleted owner = %v, want only the source", ls)
	}
	mu.Lock()
	ids := append([]string(nil), childIDs...)
	counts := map[string]int{}
	for k, v := range deletes {
		counts[k] = v
	}
	mu.Unlock()
	if len(ids) != 2 {
		t.Fatalf("created %d fork sandboxes, want 2", len(ids))
	}
	for _, id := range ids {
		if counts[id] != 1 {
			t.Fatalf("sandbox %s deleted %d time(s), want exactly once", id, counts[id])
		}
		if sandboxOnFake(t, sub, id) {
			t.Fatalf("rolled-back fork sandbox %s is still live", id)
		}
	}
}

// TestNamedSnapshotRefusesDeletedOwner: a save that passed its
// leaseReleased check and reached the row insert after the owner's
// snapshots were dropped must refuse instead of inserting a row that
// retention and the GC would keep forever (spoond-q4j S1).
func TestNamedSnapshotRefusesDeletedOwner(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	const owner = "u-save-gone"

	l, err := svc.grant(ctx, owner, "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	// The save point that models the delete landing between the
	// leaseReleased check and the insert.
	svc.saveInterrupt = func(ctx context.Context, l *Lease, buildID string) error {
		svc.markOwnerDeleted(l.Owner)
		return nil
	}
	t.Cleanup(func() { svc.saveInterrupt = nil })

	_, _, serr := svc.saveNamedSnapshot(ctx, l, "warm", "", 3)
	if serr == nil || serr.code != "not_found" {
		t.Fatalf("save of a deleted owner = %v, want not_found", serr)
	}
	if _, err := db.GetNamedSnapshotLatest(ctx, owner, "warm"); err == nil {
		t.Fatal("named snapshot row survived the owner-delete guard")
	}
}

// TestUsersDeleteUnknownAndLegacy: an id that is neither a known identity
// nor has any remaining state answers 404, and a legacy token-map owner
// answers 409 and is left untouched (spoond-q4j S2).
func TestUsersDeleteUnknownAndLegacy(t *testing.T) {
	ts, svc, _, _, _, _ := newUserDeleteServer(t)

	// Unknown, no state: 404.
	resp, _ := doReq(t, "DELETE", ts.URL+"/api/users/u-does-not-exist", "admin-tok", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete unknown id = %d, want 404", resp.StatusCode)
	}

	// A legacy token-map owner has no identity row but still
	// authenticates; the delete must refuse it and not mark it.
	resp, _ = doReq(t, "DELETE", ts.URL+"/api/users/legacy-consumer", "admin-tok", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete legacy token owner = %d, want 409", resp.StatusCode)
	}
	if svc.deletedOwners["legacy-consumer"] {
		t.Fatal("legacy token owner was marked deleted")
	}
}

// TestUsersDeleteCleansStateWithoutIdentity: an id whose identity was
// already removed (a partial cleanup) but that still owns state must
// still be cleaned and answer 200, so a retry after a partial delete
// works (spoond-q4j S2).
func TestUsersDeleteCleansStateWithoutIdentity(t *testing.T) {
	ts, _, db, _, ids, victimID := newUserDeleteServer(t)
	ctx := context.Background()

	resp, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "victim-tok",
		map[string]any{"image": "py-base", "ttl": 300})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("victim create: %d %v", resp.StatusCode, create)
	}
	leaseID := create["id"].(string)

	// The identity is gone but the lease (and its state) remains: the
	// delete must finish the cleanup, not answer 404.
	if err := ids.RemoveUser(victimID); err != nil {
		t.Fatalf("RemoveUser: %v", err)
	}
	resp, removed := doReq(t, "DELETE", ts.URL+"/api/users/"+victimID, "admin-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retry delete after partial cleanup = %d %v", resp.StatusCode, removed)
	}
	res := removed["removed"].(map[string]any)
	if !containsStr(res["leases"].([]any), leaseID) {
		t.Fatalf("removed.leases = %v, want %s", res["leases"], leaseID)
	}
	if _, err := db.GetLease(ctx, leaseID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lease row after retry cleanup: %v, want ErrNotFound", err)
	}
}

// TestLifecycleHandlersMapOwnerGone: clone, fork, restart and resume
// answer 403 owner deleted when their service call returns errOwnerGone
// (spoond-q4j S3).
func TestLifecycleHandlersMapOwnerGone(t *testing.T) {
	ts, svc, _, _, _, victimID := newUserDeleteServer(t)

	// A running lease for clone/fork, and a suspended one for
	// restart/resume.
	resp, running := doReq(t, "POST", ts.URL+"/api/sandboxes", "victim-tok",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create running: %d %v", resp.StatusCode, running)
	}
	resp, susp := doReq(t, "POST", ts.URL+"/api/sandboxes", "victim-tok",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create suspended: %d %v", resp.StatusCode, susp)
	}
	resp, _ = doReq(t, "POST", ts.URL+"/api/sandboxes/"+susp["id"].(string)+"/suspend", "victim-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("suspend: %d", resp.StatusCode)
	}

	svc.markOwnerDeleted(victimID)

	cases := []struct {
		name string
		path string
		body any
	}{
		{"clone", "/api/sandboxes/" + running["id"].(string) + "/clone", nil},
		{"fork", "/api/sandboxes/" + running["id"].(string) + "/fork", map[string]any{"count": 1}},
		{"restart", "/api/sandboxes/" + susp["id"].(string) + "/restart", nil},
		{"resume", "/api/leases/" + susp["id"].(string) + "/resume", nil},
	}
	for _, tc := range cases {
		resp, body := doReq(t, "POST", ts.URL+tc.path, "victim-tok", tc.body)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s after owner delete = %d %v, want 403", tc.name, resp.StatusCode, body)
		}
		if msg, _ := body["error"].(string); msg != "owner deleted" {
			t.Fatalf("%s refusal = %q, want %q", tc.name, msg, "owner deleted")
		}
	}
}
