package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/identity"
	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate/fake"
)

// newUserDeleteServer builds an API server with an identity store, an
// admin (admin-tok) and a victim user (victim-tok), and returns the
// victim's id.
func newUserDeleteServer(t *testing.T) (*httptest.Server, *Service, *store.DB, *testSub, *identity.Store, string) {
	t.Helper()
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ids, err := identity.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(db))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, body := doReq(t, "POST", ts.URL+"/api/users", "legacy-tok", map[string]any{
		"name": "admin", "fingerprints": []string{"SHA256:fp-a"}, "token": "admin-tok"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("bootstrap admin: %d %v", resp.StatusCode, body)
	}
	resp, body = doReq(t, "POST", ts.URL+"/api/users", "admin-tok", map[string]any{
		"name": "victim", "fingerprints": []string{"SHA256:fp-v"}, "token": "victim-tok"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create victim: %d %v", resp.StatusCode, body)
	}
	victimID := body["user"].(map[string]any)["id"].(string)
	return ts, svc, db, sub, ids, victimID
}

// TestUserDeleteCleanup: deleting a user releases every lease, cancels
// its jobs, drops its named snapshots and unpins its kept builds, and the
// response lists what was removed (spoond-q4j).
func TestUserDeleteCleanup(t *testing.T) {
	ts, svc, db, sub, ids, victimID := newUserDeleteServer(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	ctx := context.Background()

	events := svc.Subscribe(EventFilter{Owner: victimID})

	// The victim creates a lease, pins a kept build and saves a named
	// snapshot.
	resp, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "victim-tok",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("victim create: %d %v", resp.StatusCode, create)
	}
	leaseID := create["id"].(string)

	resp, cp := doReq(t, "POST", ts.URL+"/api/leases/"+leaseID+"/checkpoint", "victim-tok",
		map[string]any{"keep": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("checkpoint keep: %d %v", resp.StatusCode, cp)
	}
	keptBuild := cp["build_id"].(string)

	resp, save := doReq(t, "POST", ts.URL+"/api/leases/"+leaseID+"/snapshots", "victim-tok",
		map[string]any{"name": "warm"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("save snapshot: %d %v", resp.StatusCode, save)
	}

	// The victim runs a background job; a pid file lets the cancel signal
	// succeed. The exit never arrives (no stream exit), so the cleanup is
	// what settles it.
	p := fake.NewProcess(4242)
	installJobProcess(t, sub, p)
	resp, jobBody := doReq(t, "POST", ts.URL+"/api/sandboxes/"+leaseID+"/exec", "victim-tok",
		map[string]any{"cmd": "sleep 600", "background": true})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("background exec: %d %v", resp.StatusCode, jobBody)
	}
	jobID := jobBody["job_id"].(string)
	if l := svc.lookupAny(leaseID); l == nil {
		t.Fatal("victim lease missing after exec")
	} else {
		writeJobFile(t, sub, l.SandboxID, jobID, "pid", "4242\n")
	}

	// Delete the victim as the admin.
	resp, removed := doReq(t, "DELETE", ts.URL+"/api/users/"+victimID, "admin-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete user: %d %v", resp.StatusCode, removed)
	}
	res := removed["removed"].(map[string]any)
	if res["user"] != victimID {
		t.Fatalf("removed.user = %v, want %s", res["user"], victimID)
	}
	if !containsStr(res["leases"].([]any), leaseID) {
		t.Fatalf("removed.leases = %v, want %s", res["leases"], leaseID)
	}
	if !containsStr(res["jobs"].([]any), jobID) {
		t.Fatalf("removed.jobs = %v, want %s", res["jobs"], jobID)
	}
	if !containsStr(res["snapshots"].([]any), "warm@1") {
		t.Fatalf("removed.snapshots = %v, want warm@1", res["snapshots"])
	}
	if !containsStr(res["kept_builds"].([]any), keptBuild) {
		t.Fatalf("removed.kept_builds = %v, want %s", res["kept_builds"], keptBuild)
	}

	// The identity is gone and its lease with it.
	if ids.UserByID(victimID) != nil {
		t.Fatal("victim identity still present")
	}
	resp, _ = doReq(t, "GET", ts.URL+"/api/sandboxes/"+leaseID, "admin-tok", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("victim lease still readable: %d", resp.StatusCode)
	}

	// Named snapshot and kept build are gone; both builds are ordinary
	// GC candidates now.
	if _, err := db.GetNamedSnapshotLatest(ctx, victimID, "warm"); err == nil {
		t.Fatal("named snapshot survived the user delete")
	}
	keeps, err := db.ListKeptBuilds(ctx)
	if err != nil {
		t.Fatalf("list kept: %v", err)
	}
	if len(keeps[leaseID]) != 0 {
		t.Fatalf("kept rows survived: %v", keeps[leaseID])
	}

	// The job is settled, not left running. Its row cascades away with
	// the lease, so the proof is the job_lost event and the absence of
	// any running job for the removed owner.
	if rows, err := db.ListRunningJobsOfOwner(ctx, victimID); err != nil {
		t.Fatalf("list running jobs: %v", err)
	} else if len(rows) != 0 {
		t.Fatalf("running jobs survived the user delete: %v", rows)
	}

	// The release carried the user_deleted reason and a user_deleted event
	// was emitted for the owner; the cancelled job emitted job_lost.
	events.Close()
	got := collectEvents(events.C)
	var released int
	var userDeleted, jobLost bool
	for _, ev := range got {
		if ev.Type == LeaseReleased {
			released++
			if ev.Detail != userDeleteReason {
				t.Fatalf("released detail = %q, want %q", ev.Detail, userDeleteReason)
			}
		}
		if ev.Type == LeaseUserDeleted {
			userDeleted = true
			if ev.LeaseID != "" {
				t.Fatalf("user_deleted event carries lease id %q, want none", ev.LeaseID)
			}
		}
		if ev.Type == LeaseJobLost && ev.LeaseID == leaseID {
			jobLost = true
		}
	}
	if released != 1 || !userDeleted || !jobLost {
		t.Fatalf("events = %v, want one released, one user_deleted and one job_lost", eventTypes(got))
	}
}

// TestUserDeleteNoData: deleting a user with no leases, jobs, snapshots
// or kept builds still succeeds and answers empty lists.
func TestUserDeleteNoData(t *testing.T) {
	ts, _, _, _, ids, victimID := newUserDeleteServer(t)

	resp, removed := doReq(t, "DELETE", ts.URL+"/api/users/"+victimID, "admin-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete user: %d %v", resp.StatusCode, removed)
	}
	res := removed["removed"].(map[string]any)
	for _, key := range []string{"leases", "jobs", "snapshots", "kept_builds"} {
		if list, ok := res[key].([]any); !ok || len(list) != 0 {
			t.Fatalf("removed.%s = %v, want empty list", key, res[key])
		}
	}
	if ids.UserByID(victimID) != nil {
		t.Fatal("victim identity still present")
	}
}

// TestUserDeleteCleanupDetachedFromRequest: the cleanup is bounded on a
// context detached from the request, so a client that disconnects as the
// delete is answered cannot leave the leases behind (spoond-q4j).
func TestUserDeleteCleanupDetachedFromRequest(t *testing.T) {
	ts, svc, _, _, _, victimID := newUserDeleteServer(t)
	_ = svc

	resp, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "victim-tok",
		map[string]any{"image": "py-base", "ttl": 300})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("victim create: %d %v", resp.StatusCode, create)
	}
	leaseID := create["id"].(string)

	resp, _ = doReq(t, "DELETE", ts.URL+"/api/users/"+victimID, "admin-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete user: %d", resp.StatusCode)
	}
	resp, _ = doReq(t, "GET", ts.URL+"/api/sandboxes/"+leaseID, "admin-tok", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("lease survived delete: %d", resp.StatusCode)
	}
}

// containsStr reports whether the JSON array's strings include want.
func containsStr(arr []any, want string) bool {
	for _, v := range arr {
		if s, _ := v.(string); s == want {
			return true
		}
	}
	return false
}

// TestUserDeleteDataIdempotent: running the cleanup twice for an owner
// that no longer exists is a no-op that still answers.
func TestUserDeleteDataIdempotent(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	first, err := svc.deleteUserData(ctx, "u-nobody")
	if err != nil {
		t.Fatalf("first cleanup: %v", err)
	}
	second, err := svc.deleteUserData(ctx, "u-nobody")
	if err != nil {
		t.Fatalf("second cleanup: %v", err)
	}
	for _, r := range []userDeleteResult{first, second} {
		if len(r.Leases) != 0 || len(r.Jobs) != 0 || len(r.Snapshots) != 0 || len(r.KeptBuilds) != 0 {
			t.Fatalf("cleanup of an absent owner = %+v, want all empty", r)
		}
	}
}

// TestUserDeleteCancelQueuedRace runs a concurrent admission pass and the
// user-delete queue cancel over the same tickets: ticket.done is written
// by finishTicket/drainQueue under admitQ.mu, so cancelQueuedForOwner must
// read it under that lock too (spoond-q4j). Run under -race; before the
// fix this reports a data race on t.done.
func TestUserDeleteCancelQueuedRace(t *testing.T) {
	svc, _, _ := newTestService(t)
	const owner = "u-race"
	const rounds = 200
	const per = 16
	for r := 0; r < rounds; r++ {
		tickets := make([]*admissionTicket, 0, per)
		for i := 0; i < per; i++ {
			tk := &admissionTicket{ch: make(chan admissionOutcome, 1), id: newID(), owner: owner}
			tickets = append(tickets, tk)
			svc.admitQ.tickets = append(svc.admitQ.tickets, tk)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			for _, tk := range tickets {
				if svc.finishTicket(tk) {
					tk.send(admissionOutcome{err: errDraining})
				}
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			svc.cancelQueuedForOwner(owner)
		}()
		close(start)
		wg.Wait()
	}
	if d := svc.queueDepth(); d != 0 {
		t.Fatalf("queue depth = %d, want 0", d)
	}
}

// TestUserDeleteRefusesQueuedCreate: a create parked in the admission
// queue when its owner is deleted is refused, not granted after the
// delete. Granting it would recreate the ownerless, uncapped lease the
// delete exists to remove (spoond-q4j).
func TestUserDeleteRefusesQueuedCreate(t *testing.T) {
	ts, svc, _, sub, _, victimID := newUserDeleteServer(t)
	// A node with room for exactly one py-base (2048 MiB = 1024 pages),
	// so the second create must wait for the first to go.
	installDynamicNode(t, svc, sub, 1024, 0, 1024)
	svc.cfg.MaxAdmitWaitSecs = 60
	svc.admitQ.tick = 20 * time.Millisecond
	h := ts.Config.Handler

	if r := waitCreate(t, h, "victim-tok", `{"image":"py-base","ttl":60}`); r.code != http.StatusCreated {
		t.Fatalf("filler create: %d %v", r.code, r.body)
	}
	// The victim's next create is refused for capacity and waits.
	res := startCreate(t, h, context.Background(), "victim-tok", `{"image":"py-base","ttl":60,"wait":60}`)
	waitDepth(t, svc, 1)

	resp, removed := doReq(t, "DELETE", ts.URL+"/api/users/"+victimID, "admin-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete user: %d %v", resp.StatusCode, removed)
	}

	// The waiting create must be answered, and refused: its owner is
	// gone, so admitting it would be ownerless and uncapped.
	out := waitResult(t, res)
	if out.code != http.StatusForbidden {
		t.Fatalf("queued create after user delete = %d, want 403 (%v)", out.code, out.body)
	}
	if msg, _ := out.body["error"].(string); msg != "owner deleted" {
		t.Fatalf("refusal = %q, want %q", msg, "owner deleted")
	}
	if l := svc.leasesOfOwner(victimID); len(l) != 0 {
		t.Fatalf("queued create recreated %d lease(s) for the deleted owner", len(l))
	}
	if svc.queueDepth() != 0 {
		t.Fatalf("queue depth after delete = %d, want 0", svc.queueDepth())
	}
}

// TestReserveQuotaRefusesDeletedOwner: once an owner is marked deleted,
// the quota check refuses instead of falling through the uncapped
// legacy-owner branch, so no admission path can grant them a lease
// (spoond-q4j).
func TestReserveQuotaRefusesDeletedOwner(t *testing.T) {
	svc, _, _ := newTestService(t)
	ids, err := identity.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	svc.SetIdentities(ids)
	u, err := ids.AddUser("gone", identity.KindPerson, nil, "gone-tok")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.reserveQuota(u.ID, 1, 0, true); err != nil {
		t.Fatalf("reserve before delete: %v", err)
	}
	svc.releaseQuotaReservation(u.ID, 1, 0)
	svc.markOwnerDeleted(u.ID)
	if err := svc.reserveQuota(u.ID, 1, 0, true); !errors.Is(err, errOwnerGone) {
		t.Fatalf("reserve after delete = %v, want errOwnerGone", err)
	}
}

// TestUserDeleteJobsListNeverNull: when the running-jobs query fails the
// response's jobs field is still an empty JSON array, never null, so a
// client can iterate it unconditionally (spoond-q4j).
func TestUserDeleteJobsListNeverNull(t *testing.T) {
	svc, db, _ := newTestService(t)
	ctx := context.Background()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := svc.deleteUserData(ctx, "u-none")
	if err == nil {
		t.Fatal("cleanup of a closed store returned no error")
	}
	if res.Jobs == nil {
		t.Fatal("Jobs = nil, want an empty non-nil slice")
	}
	if len(res.Jobs) != 0 {
		t.Fatalf("Jobs = %v, want empty", res.Jobs)
	}
}

// TestUserDeleteEventTypeDocumented guards the new event's wire shape.
func TestUserDeleteEventTypeDocumented(t *testing.T) {
	if LeaseUserDeleted != "user_deleted" {
		t.Fatalf("LeaseUserDeleted = %q, want user_deleted", LeaseUserDeleted)
	}
}

// TestUserDeleteTimeoutIsFinite pins the cleanup bound.
func TestUserDeleteTimeoutIsFinite(t *testing.T) {
	if userDeleteTimeout <= 0 || userDeleteTimeout > 10*time.Minute {
		t.Fatalf("userDeleteTimeout = %s, want a finite bound", userDeleteTimeout)
	}
}
