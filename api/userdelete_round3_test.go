package api

// Round-3 review follow-ups for spoond-q4j (user delete cleanup):
// N1 the queued-create refusal must answer 403 (its quota-cap case went
// with the caps in 3.0; TestUserDeleteRefusesQueuedCreate covers the
// capacity case), N1b a ticket parked after the queue cancel is still
// refused early, N2 a repeated delete of the same real user is
// idempotent and answers 200, N3 restore maps errOwnerGone to 403, and
// N4 the owner-deleted grant path drops its egress memo.

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v4/identity"
	"github.com/jrimmer/spoond/v4/substrate"
)

// TestUserDeleteTicketQueuedAfterCancelRefused: a ticket that raced the
// owner-delete mark and parked just before it is still refused at its
// next admission pass, and since spoond-y0jj a create for an owner
// already marked deleted is refused at newAdmissionTicket, so it never
// parks in the first place (spoond-q4j N1b).
func TestUserDeleteTicketQueuedAfterCancelRefused(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ids, err := identity.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	svc.SetIdentities(ids)
	u, err := ids.AddUser("late", identity.KindPerson, nil, "late-tok")
	if err != nil {
		t.Fatal(err)
	}

	// A ticket that parked just before the mark (a request that passed
	// auth before the removal) is refused at its next admission pass
	// instead of waiting out its deadline. Park it through the normal
	// path, then set the mark: exactly the production race.
	tk := svc.newAdmissionTicket(u.ID, leaseRequest{
		owner: u.ID, image: "py-base", ttl: time.Minute,
	}, substrate.ErrCapacity, time.Minute)
	if tk == nil {
		t.Fatal("ticket for a live owner = nil, want a parked ticket")
	}
	svc.markOwnerDeleted(u.ID)

	svc.tryAdmitQueued(context.Background())

	select {
	case o := <-tk.ch:
		if !errors.Is(o.err, errOwnerGone) {
			t.Fatalf("raced ticket outcome = %v, want errOwnerGone", o.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("raced ticket was not refused early; it is still waiting")
	}
	if svc.queueDepth() != 0 {
		t.Fatalf("queue depth after refusal = %d, want 0", svc.queueDepth())
	}

	// The delete's mark and cancel both ran before this ticket could
	// park; the mark is checked under the queue lock, so it is refused
	// before it enters the queue (spoond-y0jj).
	svc.cancelQueuedForOwner(u.ID)
	if late := svc.newAdmissionTicket(u.ID, leaseRequest{
		owner: u.ID, image: "py-base", ttl: time.Minute,
	}, substrate.ErrCapacity, time.Minute); late != nil {
		t.Fatalf("ticket for deleted owner = %+v, want nil (refused before parking)", late)
	}
	if svc.queueDepth() != 0 {
		t.Fatalf("queue depth after refusal = %d, want 0", svc.queueDepth())
	}
}

// TestUsersDeleteRepeatIsIdempotent: deleting the same real user twice
// answers 200 with empty removed lists the second time; an id that was
// never a user still answers 404 (spoond-q4j N2).
func TestUsersDeleteRepeatIsIdempotent(t *testing.T) {
	ts, _, _, _, _, victimID := newUserDeleteServer(t)

	resp, body := doReq(t, "DELETE", ts.URL+"/api/users/"+victimID, "admin-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first delete = %d %v, want 200", resp.StatusCode, body)
	}
	resp, body = doReq(t, "DELETE", ts.URL+"/api/users/"+victimID, "admin-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second delete = %d %v, want 200", resp.StatusCode, body)
	}
	res := body["removed"].(map[string]any)
	for _, key := range []string{"leases", "jobs", "snapshots", "kept_builds"} {
		list, ok := res[key].([]any)
		if !ok || len(list) != 0 {
			t.Fatalf("removed.%s = %v, want empty list", key, res[key])
		}
	}

	resp, _ = doReq(t, "DELETE", ts.URL+"/api/users/u-never-a-user", "admin-tok", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete never-a-user = %d, want 404", resp.StatusCode)
	}
}

// TestRestoreRefusesDeletedOwner: restoring a suspended lease whose
// owner was deleted answers 403 owner deleted (spoond-q4j N3).
func TestRestoreRefusesDeletedOwner(t *testing.T) {
	ts, svc, _, _, _, victimID := newUserDeleteServer(t)

	resp, create := doReq(t, "POST", ts.URL+"/api/leases", "victim-tok",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %v", resp.StatusCode, create)
	}
	leaseID := create["id"].(string)
	resp, cp := doReq(t, "POST", ts.URL+"/api/leases/"+leaseID+"/checkpoint", "victim-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("checkpoint: %d %v", resp.StatusCode, cp)
	}
	b := cp["build_id"].(string)
	resp, _ = doReq(t, "POST", ts.URL+"/api/leases/"+leaseID+"/suspend", "victim-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("suspend: %d", resp.StatusCode)
	}

	svc.markOwnerDeleted(victimID)
	resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+leaseID+"/restore", "victim-tok",
		map[string]any{"build_id": b})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("restore after owner delete = %d %v, want 403", resp.StatusCode, body)
	}
	if msg, _ := body["error"].(string); msg != "owner deleted" {
		t.Fatalf("restore refusal = %q, want %q", msg, "owner deleted")
	}
}

// TestGrantRefusesDeletedOwnerDropsEgress: a grant that reaches its
// commit guard after the owner was marked deleted drops the egress memo
// recorded while creating the sandbox (spoond-q4j N4).
func TestGrantRefusesDeletedOwnerDropsEgress(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	const owner = "u-egress-gone"
	const leaseID = "l-egress-gone"

	// The delete lands while the sandbox is being created, after
	// createSandbox recorded the lease's egress memo and before
	// grantLease reaches its commit guard.
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		svc.markOwnerDeleted(owner)
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	_, err := svc.grantLease(ctx, leaseRequest{
		owner: owner, image: "py-base", ttl: time.Minute, persistent: true,
		leaseID: leaseID,
	})
	if !errors.Is(err, errOwnerGone) {
		t.Fatalf("grant error = %v, want errOwnerGone", err)
	}
	svc.appliedMu.Lock()
	_, present := svc.appliedEgress[leaseID]
	svc.appliedMu.Unlock()
	if present {
		t.Fatalf("egress memo for the refused grant survived")
	}
}
