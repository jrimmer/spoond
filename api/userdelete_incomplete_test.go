package api

// Round-4 review follow-ups for spoond-q4j (user delete cleanup):
// a store step that fails part-way through deleteUserData must answer
// 500 with the partial "removed" body and "incomplete": true (plus the
// failed step) so the admin retries, and a ticket for an already-deleted
// owner must be refused before it parks (spoond-y0jj).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/jrimmer/spoond/v2/identity"
	"github.com/jrimmer/spoond/v2/store"
)

// TestUserDeleteIncompleteAnswers500AndRetryCompletes: when a cleanup
// store step fails, DELETE /api/users/{id} answers 500 with the partial
// removed body, "incomplete": true and the failed "step" instead of a
// silent 200; a retry with the store healthy completes the cleanup
// (spoond-y0jj).
func TestUserDeleteIncompleteAnswers500AndRetryCompletes(t *testing.T) {
	ts, svc, _, _, ids, victimID := newUserDeleteServer(t)
	svc.cfg.TemplateStoragePath = t.TempDir()

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
	if l := svc.lookupAny(leaseID); l == nil {
		t.Fatal("victim lease missing before delete")
	}

	// Fail only the named-snapshot drop, as a busy SQLite would. The
	// other steps still run, so the partial body names what was removed.
	injected := errors.New("database is locked")
	svc.userDeleteStoreErr = func(step string) error {
		if step == userDeleteStepDropSnapshots {
			return injected
		}
		return nil
	}

	resp, body := doReq(t, "DELETE", ts.URL+"/api/users/"+victimID, "admin-tok", nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("incomplete delete = %d %v, want 500", resp.StatusCode, body)
	}
	if incomplete, _ := body["incomplete"].(bool); !incomplete {
		t.Fatalf("incomplete = %v, want true", body["incomplete"])
	}
	if step, _ := body["step"].(string); step != userDeleteStepDropSnapshots {
		t.Fatalf("step = %q, want %q", step, userDeleteStepDropSnapshots)
	}
	res, ok := body["removed"].(map[string]any)
	if !ok {
		t.Fatalf("removed = %T, want an object", body["removed"])
	}
	if !containsStr(res["leases"].([]any), leaseID) {
		t.Fatalf("partial removed.leases = %v, want %s", res["leases"], leaseID)
	}
	if !containsStr(res["kept_builds"].([]any), keptBuild) {
		t.Fatalf("partial removed.kept_builds = %v, want %s", res["kept_builds"], keptBuild)
	}

	// A retry with the store healthy completes the cleanup and reports
	// the snapshot that was still there.
	svc.userDeleteStoreErr = nil
	resp, body = doReq(t, "DELETE", ts.URL+"/api/users/"+victimID, "admin-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retry delete = %d %v, want 200", resp.StatusCode, body)
	}
	if _, present := body["incomplete"]; present {
		t.Fatalf("retry body has incomplete: %v", body)
	}
	res = body["removed"].(map[string]any)
	if !containsStr(res["snapshots"].([]any), "warm@1") {
		t.Fatalf("retry removed.snapshots = %v, want warm@1", res["snapshots"])
	}
	if ids.UserByID(victimID) != nil {
		t.Fatal("victim identity survived the retry")
	}
}

// TestUserDeleteIncompleteFirstStepStillRunsRest: failing the first
// cleanup step (list_jobs) must not stop the rest: the 500 body's
// removed lists still name the lease and the named snapshot, and both
// are actually released and dropped. The test kills a mutation that
// returns after the first failed step (spoond-y0jj).
func TestUserDeleteIncompleteFirstStepStillRunsRest(t *testing.T) {
	ts, svc, db, _, _, victimID := newUserDeleteServer(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	ctx := context.Background()

	resp, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "victim-tok",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("victim create: %d %v", resp.StatusCode, create)
	}
	leaseID := create["id"].(string)
	resp, save := doReq(t, "POST", ts.URL+"/api/leases/"+leaseID+"/snapshots", "victim-tok",
		map[string]any{"name": "warm"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("save snapshot: %d %v", resp.StatusCode, save)
	}

	// Fail the first step (list_jobs). Every later step must still run.
	svc.userDeleteStoreErr = func(step string) error {
		if step == userDeleteStepListJobs {
			return errors.New("database is locked")
		}
		return nil
	}
	defer func() { svc.userDeleteStoreErr = nil }()

	resp, body := doReq(t, "DELETE", ts.URL+"/api/users/"+victimID, "admin-tok", nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("incomplete delete = %d %v, want 500", resp.StatusCode, body)
	}
	if step, _ := body["step"].(string); step != userDeleteStepListJobs {
		t.Fatalf("step = %q, want %q", step, userDeleteStepListJobs)
	}
	res := body["removed"].(map[string]any)
	if !containsStr(res["leases"].([]any), leaseID) {
		t.Fatalf("partial removed.leases = %v, want %s", res["leases"], leaseID)
	}
	if !containsStr(res["snapshots"].([]any), "warm@1") {
		t.Fatalf("partial removed.snapshots = %v, want warm@1", res["snapshots"])
	}

	// The steps after the failed first one really ran: the lease row is
	// gone and the named snapshot row is gone.
	if _, err := db.GetLease(ctx, leaseID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lease row after incomplete delete: %v, want ErrNotFound", err)
	}
	if _, err := db.GetNamedSnapshotLatest(ctx, victimID, "warm"); err == nil {
		t.Fatal("named snapshot survived the incomplete delete")
	}
}

// TestUserDeleteIncompleteKeepsJobsListNonNull: a failed job listing is
// reported as incomplete while the partial body's jobs field stays an
// empty JSON array, never null (spoond-y0jj).
func TestUserDeleteIncompleteKeepsJobsListNonNull(t *testing.T) {
	ts, svc, _, _, _, victimID := newUserDeleteServer(t)

	svc.userDeleteStoreErr = func(step string) error {
		if step == userDeleteStepListJobs {
			return fmt.Errorf("database is locked")
		}
		return nil
	}
	defer func() { svc.userDeleteStoreErr = nil }()

	resp, body := doReq(t, "DELETE", ts.URL+"/api/users/"+victimID, "admin-tok", nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("incomplete delete = %d %v, want 500", resp.StatusCode, body)
	}
	if step, _ := body["step"].(string); step != userDeleteStepListJobs {
		t.Fatalf("step = %q, want %q", step, userDeleteStepListJobs)
	}
	res := body["removed"].(map[string]any)
	if _, ok := res["jobs"].([]any); !ok {
		t.Fatalf("removed.jobs = %v, want an empty JSON array", res["jobs"])
	}
}

// TestUserDeleteRetryAfterRestartStoreErrorAnswers500: after a restart
// the in-memory deleted-owner mark is gone, so a retry of a half-cleaned
// user relies on ownerHasState to decide 404 vs cleanup. When that read
// fails, the delete must answer 500 and leave the snapshot rows alone,
// not a 404 that hides them (spoond-y0jj T2).
func TestUserDeleteRetryAfterRestartStoreErrorAnswers500(t *testing.T) {
	ts, svc, db, _, ids, victimID := newUserDeleteServer(t)
	svc.cfg.TemplateStoragePath = t.TempDir()
	ctx := context.Background()

	resp, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "victim-tok",
		map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("victim create: %d %v", resp.StatusCode, create)
	}
	leaseID := create["id"].(string)
	resp, save := doReq(t, "POST", ts.URL+"/api/leases/"+leaseID+"/snapshots", "victim-tok",
		map[string]any{"name": "warm"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("save snapshot: %d %v", resp.StatusCode, save)
	}

	// Model a restart: the identity, the in-memory lease set and the
	// deleted-owner mark are gone, only the state rows remain.
	if err := ids.RemoveUser(victimID); err != nil {
		t.Fatalf("RemoveUser: %v", err)
	}
	svc.store.mu.Lock()
	delete(svc.store.leases, leaseID)
	svc.store.mu.Unlock()
	svc.ownerDeleteMu.Lock()
	delete(svc.deletedOwners, victimID)
	svc.ownerDeleteMu.Unlock()

	// A busy SQLite during the retry's owner-state read.
	svc.userDeleteStoreErr = func(step string) error {
		if step == userDeleteStepOwnerState {
			return errors.New("database is locked")
		}
		return nil
	}
	defer func() { svc.userDeleteStoreErr = nil }()

	resp, body := doReq(t, "DELETE", ts.URL+"/api/users/"+victimID, "admin-tok", nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("retry with unreadable state = %d %v, want 500", resp.StatusCode, body)
	}
	if _, err := db.GetNamedSnapshotLatest(ctx, victimID, "warm"); err != nil {
		t.Fatalf("named snapshot lost on the failed retry: %v", err)
	}

	// A healthy retry now cleans up and answers 200.
	svc.userDeleteStoreErr = nil
	resp, body = doReq(t, "DELETE", ts.URL+"/api/users/"+victimID, "admin-tok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retry with readable state = %d %v, want 200", resp.StatusCode, body)
	}
	if _, err := db.GetNamedSnapshotLatest(ctx, victimID, "warm"); err == nil {
		t.Fatal("named snapshot survived the successful retry")
	}
}

// TestDeleteUserDataReportsStep: deleteUserData returns a
// *userDeleteStepError naming the failed step and still reports what the
// other steps removed, so a caller can answer an incomplete cleanup
// (spoond-y0jj).
func TestDeleteUserDataReportsStep(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	ids, err := identity.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	svc.SetIdentities(ids)
	u, err := ids.AddUser("gone", identity.KindPerson, nil, "gone-tok")
	if err != nil {
		t.Fatal(err)
	}
	svc.userDeleteStoreErr = func(step string) error {
		if step == userDeleteStepUnpinBuilds {
			return errors.New("database is locked")
		}
		return nil
	}
	defer func() { svc.userDeleteStoreErr = nil }()

	_, err = svc.deleteUserData(ctx, u.ID)
	var stepErr *userDeleteStepError
	if !errors.As(err, &stepErr) {
		t.Fatalf("deleteUserData error = %v, want *userDeleteStepError", err)
	}
	if stepErr.Step != userDeleteStepUnpinBuilds {
		t.Fatalf("step = %q, want %q", stepErr.Step, userDeleteStepUnpinBuilds)
	}
	if !errors.Is(err, stepErr.Err) {
		t.Fatalf("step error does not unwrap to the store error")
	}
}
