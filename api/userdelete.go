package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"
)

// userDeleteReason is the release reason every lease of a removed user
// carries (spoond-q4j). It is the `released` event's detail, so a
// dashboard reader can tell a user-deletion release from an owner's own
// DELETE.
const userDeleteReason = "user_deleted"

// userDeleteTimeout bounds the whole cleanup that follows removal of an
// identity: releasing every lease and dropping every named snapshot and
// kept build. It is detached from the request (a client disconnect must
// not leave the cleanup half done) but finite.
const userDeleteTimeout = 5 * time.Minute

// userDeleteResult lists what removing a user removed (spoond-q4j): the
// leases released, the jobs cancelled, the named snapshot versions
// dropped (name@version) and the kept builds unpinned. The slices are
// JSON arrays, never null, so a client can iterate them unconditionally.
type userDeleteResult struct {
	User       string   `json:"user"`
	Leases     []string `json:"leases"`
	Jobs       []string `json:"jobs"`
	Snapshots  []string `json:"snapshots"`
	KeptBuilds []string `json:"kept_builds"`
}

// The cleanup step names a userDeleteStepError reports (spoond-y0jj): a
// store call that failed part-way through the cleanup. They are stable
// wire values, so a client can tell an admin which step to retry.
const (
	userDeleteStepListJobs       = "list_jobs"
	userDeleteStepListKeptBuilds = "list_kept_builds"
	userDeleteStepUnpinBuilds    = "unpin_kept_builds"
	userDeleteStepDropSnapshots  = "drop_named_snapshots"
)

// userDeleteStepError names the cleanup step whose store call failed. A
// failed step does not stop the rest of the cleanup: whatever the other
// steps removed is still reported. handleUsersDelete answers 500 with
// the partial result and this step so the admin retries; a retry
// completes the cleanup (spoond-y0jj).
type userDeleteStepError struct {
	Step string
	Err  error
}

func (e *userDeleteStepError) Error() string {
	return fmt.Sprintf("%s: %v", e.Step, e.Err)
}

func (e *userDeleteStepError) Unwrap() error { return e.Err }

// deleteUserData releases a removed user's leases and drops the state
// that would otherwise outlive them (spoond-q4j): every running job is
// cancelled (signalled then marked lost), every lease is released with
// reason user_deleted (which kills its sandbox, drops its shares and
// settles any job still counted), every kept build is unpinned and every
// named snapshot version is deleted outright. Each step logs what it
// removed and the whole cleanup emits one user_deleted event naming the
// counts; the returned result lists them.
//
// It is safe to call for an owner whose identity is already gone: it
// then only clears whatever state still bears that owner id.
//
// A store step that fails is reported, not swallowed: the returned
// error is a *userDeleteStepError naming the step, and the result still
// lists what the other steps removed. A caller that answers the delete
// must surface it as an incomplete cleanup so the admin retries
// (spoond-y0jj).
func (s *Service) deleteUserData(ctx context.Context, owner string) (userDeleteResult, error) {
	res := userDeleteResult{
		User:       owner,
		Leases:     []string{},
		Jobs:       []string{},
		Snapshots:  []string{},
		KeptBuilds: []string{},
	}

	// First failure wins: it is the step the admin should look at, but
	// every step below still runs so a retry has less left to do (and a
	// later step may clear rows an earlier one could not).
	var failed error
	record := func(step string, err error) {
		if err != nil && failed == nil {
			failed = &userDeleteStepError{Step: step, Err: err}
		}
	}

	// Refuse any admission ticket the user parked before the identity was
	// removed, and mark the owner deleted so an admission that raced the
	// delete is refused rather than recreating an uncapped lease
	// (spoond-q4j).
	s.markOwnerDeleted(owner)
	s.cancelQueuedForOwner(owner)

	// Cancel running jobs first, while their leases (and sandboxes) still
	// exist: a TERM reaches the guest's process group, and marking the job
	// lost settles the running gauge, the secret bookkeeping and the
	// event even if the signal could not be delivered. Append, never
	// overwrite, so the response's jobs list stays a JSON array even when
	// the listing query fails.
	jobs, err := s.cancelUserJobs(ctx, owner)
	res.Jobs = append(res.Jobs, jobs...)
	record(userDeleteStepListJobs, err)

	// Snapshot the pins before the releases drop them per lease, so the
	// response can name every build that stopped being a GC root.
	if err := s.userDeleteStoreError(userDeleteStepListKeptBuilds); err != nil {
		s.log.Printf("user delete: list kept builds of %s: %v", owner, err)
		record(userDeleteStepListKeptBuilds, err)
	} else if kept, err := s.db.ListKeptBuildsOfOwner(ctx, owner); err != nil {
		s.log.Printf("user delete: list kept builds of %s: %v", owner, err)
		record(userDeleteStepListKeptBuilds, err)
	} else {
		res.KeptBuilds = append(res.KeptBuilds, kept...)
	}

	// Release every lease of the owner. A lease created by a request still
	// in flight when the identity was removed can appear between passes,
	// so re-scan a bounded number of times. releaseBecause is idempotent,
	// and it removes the lease from the in-memory set, so a pass that
	// finds nothing ends the loop. Three passes are enough: markOwnerDeleted
	// runs before this loop and reserveQuota/grantLease refuse a create
	// once the owner is marked, so a create admitted between passes cannot
	// become a visible lease after the last scan.
	seen := map[string]bool{}
	for pass := 0; pass < 3; pass++ {
		ls := s.leasesOfOwner(owner)
		if len(ls) == 0 {
			break
		}
		for _, l := range ls {
			if seen[l.ID] {
				continue
			}
			seen[l.ID] = true
			s.log.Printf("user delete: releasing lease %s of %s", l.ID, owner)
			s.releaseBecause(ctx, l, userDeleteReason)
			res.Leases = append(res.Leases, l.ID)
		}
	}

	// Safety net: a kept-builds row whose lease row lagged the in-memory
	// set (or a release path that missed it) must not pin a build after
	// the user is gone.
	if err := s.userDeleteStoreError(userDeleteStepUnpinBuilds); err != nil {
		s.log.Printf("user delete: unpin kept builds of %s: %v", owner, err)
		record(userDeleteStepUnpinBuilds, err)
	} else if extra, err := s.db.DeleteKeptBuildsOfOwner(ctx, owner); err != nil {
		s.log.Printf("user delete: unpin kept builds of %s: %v", owner, err)
		record(userDeleteStepUnpinBuilds, err)
	} else {
		for _, id := range extra {
			s.log.Printf("user delete: unpinned build %s of %s", id, owner)
		}
		res.KeptBuilds = mergeUnique(res.KeptBuilds, extra)
	}

	// Named snapshots are dropped outright, forced: their leases are
	// already released, so no live lease can be running from a version.
	// The owner was marked deleted before this drop, and saveNamedSnapshot
	// checks that mark under the owner-delete lock while it holds the same
	// lock across its insert. So a save that reached its insert has either
	// landed before the mark (and this drop removes its row) or found the
	// mark and answered not_found (spoond-q4j S1). Without that check a
	// row could be inserted after this drop and survive: retention keeps
	// the newest versions and the GC treats named_snapshots rows as GC
	// roots, so it would never be reclaimed.
	if err := s.userDeleteStoreError(userDeleteStepDropSnapshots); err != nil {
		s.log.Printf("user delete: drop named snapshots of %s: %v", owner, err)
		record(userDeleteStepDropSnapshots, err)
	} else if rows, err := s.db.DeleteNamedSnapshotsOfOwner(ctx, owner); err != nil {
		s.log.Printf("user delete: drop named snapshots of %s: %v", owner, err)
		record(userDeleteStepDropSnapshots, err)
	} else {
		for _, r := range rows {
			s.log.Printf("user delete: dropped snapshot %s/%s@%d of %s", r.Owner, r.Name, r.Version, owner)
			res.Snapshots = append(res.Snapshots, fmt.Sprintf("%s@%d", r.Name, r.Version))
		}
	}
	sort.Strings(res.KeptBuilds)
	s.UpdateKeptMetrics(ctx)
	s.UpdateNamedSnapshotMetrics(ctx)

	s.log.Printf("user delete: %s removed: %d lease(s), %d job(s), %d snapshot(s), %d kept build(s)",
		owner, len(res.Leases), len(res.Jobs), len(res.Snapshots), len(res.KeptBuilds))
	s.emitLeaseEvent("", owner, LeaseUserDeleted, fmt.Sprintf(
		"removed user %s: %d lease(s), %d job(s), %d snapshot(s), %d kept build(s)",
		owner, len(res.Leases), len(res.Jobs), len(res.Snapshots), len(res.KeptBuilds)))
	return res, failed
}

// userDeleteStoreError consults the test-only store-error hook for one
// cleanup step; nil in production. It lets a test pin that a failed
// store step answers an incomplete cleanup instead of a 200
// (spoond-y0jj).
func (s *Service) userDeleteStoreError(step string) error {
	if s.userDeleteStoreErr == nil {
		return nil
	}
	return s.userDeleteStoreErr(step)
}

// cancelUserJobs signals every running job of one owner's leases with
// TERM and marks it lost with detail "user deleted", returning the job
// ids it settled. A signal that cannot be delivered (the guest is gone)
// is logged and the job is still settled, so no running count or secret
// name survives the user. It always returns a non-nil slice, so a caller
// can append it into a JSON array without turning the field null. A
// failed listing returns the error alongside the empty slice, so the
// caller can report the cleanup incomplete (spoond-y0jj).
func (s *Service) cancelUserJobs(ctx context.Context, owner string) ([]string, error) {
	if err := s.userDeleteStoreError(userDeleteStepListJobs); err != nil {
		s.log.Printf("user delete: list running jobs of %s: %v", owner, err)
		return []string{}, err
	}
	rows, err := s.db.ListRunningJobsOfOwner(ctx, owner)
	if err != nil {
		s.log.Printf("user delete: list running jobs of %s: %v", owner, err)
		return []string{}, err
	}
	out := make([]string, 0, len(rows))
	for _, j := range rows {
		sandboxID := ""
		if l := s.lookupAny(j.LeaseID); l != nil {
			sandboxID = l.SandboxID
			if l.live() {
				if err := s.signalJob(ctx, l.ID, sandboxID, j, "TERM"); err != nil {
					s.log.Printf("user delete: cancel job %s of %s: %v", j.JobID, owner, err)
				}
			}
		}
		s.log.Printf("user delete: cancelled job %s of %s", j.JobID, owner)
		s.markJobLost(ctx, j, sandboxID, j.Generation, "user deleted")
		out = append(out, j.JobID)
	}
	return out, nil
}

// leasesOfOwner snapshots the owner's live leases, ordered by id so the
// cleanup (and its logged/reported order) is deterministic.
func (s *Service) leasesOfOwner(owner string) []*Lease {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	out := make([]*Lease, 0)
	for _, l := range s.store.leases {
		if l.Owner == owner && !l.released {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// mergeUnique appends the items of extra that are not already in base.
func mergeUnique(base, extra []string) []string {
	seen := make(map[string]bool, len(base))
	for _, s := range base {
		seen[s] = true
	}
	for _, s := range extra {
		if !seen[s] {
			base = append(base, s)
			seen[s] = true
		}
	}
	return base
}

// ownerHasState reports whether an owner id still has state that a
// user-delete would clean up: a lease, a job, a named snapshot or a
// kept build. handleUsersDelete uses it to tell a real (or half-cleaned)
// user from an id that never existed, so an unknown id answers 404
// instead of silently succeeding (spoond-q4j S2).
func (s *Service) ownerHasState(ctx context.Context, owner string) bool {
	s.store.mu.Lock()
	for _, l := range s.store.leases {
		if l.Owner == owner && !l.released {
			s.store.mu.Unlock()
			return true
		}
	}
	s.store.mu.Unlock()
	if rows, err := s.db.ListLeases(ctx); err != nil {
		s.log.Printf("user delete: list leases of %s: %v", owner, err)
	} else {
		for _, r := range rows {
			if r.Owner == owner {
				return true
			}
		}
	}
	if rows, err := s.db.ListRunningJobsOfOwner(ctx, owner); err != nil {
		s.log.Printf("user delete: list jobs of %s: %v", owner, err)
	} else if len(rows) > 0 {
		return true
	}
	if n, err := s.db.CountNamedSnapshotNames(ctx, owner); err != nil {
		s.log.Printf("user delete: count snapshots of %s: %v", owner, err)
	} else if n > 0 {
		return true
	}
	if kept, err := s.db.ListKeptBuildsOfOwner(ctx, owner); err != nil {
		s.log.Printf("user delete: list kept builds of %s: %v", owner, err)
	} else if len(kept) > 0 {
		return true
	}
	return false
}

// legacyTokenOwner reports whether id is the owner of a legacy consumer
// token (the token map single-user deployments use). Such an owner has
// no identity row but still authenticates through the token fallback, so
// markOwnerDeleted would refuse every one of their future creates for
// the life of the process. handleUsersDelete refuses to delete them
// (spoond-q4j S2).
func (s *Service) legacyTokenOwner(id string) bool {
	for _, owner := range s.tokens {
		if owner == id {
			return true
		}
	}
	return false
}

// handleUsersDelete removes a user (admin only) and cleans up the state
// that would otherwise outlive the identity (spoond-q4j): it releases
// the user's leases (reason user_deleted), cancels their running jobs,
// drops their named snapshots, unpins their kept builds and answers 200
// with what it removed. Before this cleanup an owner with no user had no
// quota, so their leases, snapshots and builds stayed and were uncapped.
//
// An id that is neither a known identity nor has any remaining state
// answers 404: it never existed, so there is nothing to remove. A
// repeated delete of the same real user answers 200 with empty lists
// once the owner is marked deleted, so the documented idempotence holds
// (spoond-q4j N2); an id that was never a user still answers 404. A
// legacy token-map owner answers 409 and is left untouched: it has no
// identity row but still authenticates.
//
// A cleanup whose store step failed answers 500 with the partial
// "removed" body, "incomplete": true and the failed "step", so the
// admin knows to retry rather than believing the user is fully gone
// (spoond-y0jj). A retry completes the cleanup.
func (s *Server) handleUsersDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "user id required")
		return
	}
	if u := userFrom(r.Context()); u != nil && u.ID == id {
		writeError(w, http.StatusBadRequest, "cannot delete yourself")
		return
	}
	// A legacy token-map owner has no identity row but still resolves
	// through the token fallback; marking it deleted would permanently
	// refuse its creates. Refuse the delete instead.
	if s.svc.legacyTokenOwner(id) {
		writeError(w, http.StatusConflict, "cannot delete a legacy token owner; remove its token instead")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), userDeleteTimeout)
	defer cancel()
	// An unknown id with no state left is a 404, not a silent success
	// (spoond-q4j S2). A known identity, an id that was already deleted
	// (deletedOwners, so a repeated delete is idempotent and answers 200),
	// or an id whose cleanup was interrupted and still has state
	// proceeds; RemoveUser is idempotent so a retry after a partial
	// cleanup works.
	known := s.svc.identities != nil && s.svc.identities.UserByID(id) != nil
	if !known && !s.svc.ownerHasState(ctx, id) {
		s.svc.ownerDeleteMu.Lock()
		deleted := s.svc.deletedOwners[id]
		s.svc.ownerDeleteMu.Unlock()
		if !deleted {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
	}
	// Remove the identity first: once its token no longer resolves, no
	// new lease can be attributed to the owner while the cleanup runs.
	if err := s.svc.identities.RemoveUser(id); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("remove: %v", err))
		return
	}
	// Stop any create of this owner that is already admitted or waiting
	// (spoond-q4j): deleteUserData marks the owner deleted before it
	// cleans up, so reserveQuota and grantLease refuse a create that
	// raced the identity removal, and the cleanup releases any lease that
	// slipped through before the mark.
	res, cleanupErr := s.svc.deleteUserData(ctx, id)
	if cleanupErr != nil {
		body := map[string]any{"removed": res, "incomplete": true}
		var stepErr *userDeleteStepError
		if errors.As(cleanupErr, &stepErr) {
			body["step"] = stepErr.Step
			body["error"] = cleanupErr.Error()
		}
		writeJSON(w, http.StatusInternalServerError, body)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": res})
}
