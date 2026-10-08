package api

import (
	"context"
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
func (s *Service) deleteUserData(ctx context.Context, owner string) userDeleteResult {
	res := userDeleteResult{
		User:       owner,
		Leases:     []string{},
		Jobs:       []string{},
		Snapshots:  []string{},
		KeptBuilds: []string{},
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
	res.Jobs = append(res.Jobs, s.cancelUserJobs(ctx, owner)...)

	// Snapshot the pins before the releases drop them per lease, so the
	// response can name every build that stopped being a GC root.
	if kept, err := s.db.ListKeptBuildsOfOwner(ctx, owner); err != nil {
		s.log.Printf("user delete: list kept builds of %s: %v", owner, err)
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
	if extra, err := s.db.DeleteKeptBuildsOfOwner(ctx, owner); err != nil {
		s.log.Printf("user delete: unpin kept builds of %s: %v", owner, err)
	} else {
		for _, id := range extra {
			s.log.Printf("user delete: unpinned build %s of %s", id, owner)
		}
		res.KeptBuilds = mergeUnique(res.KeptBuilds, extra)
	}

	// Named snapshots are dropped outright, forced: their leases are
	// already released, so no live lease can be running from a version. A
	// save already inside its checkpoint (holding secretsGate.beginSave)
	// can still insert a row after this delete; saveNamedSnapshot's
	// leaseReleased check drops most of that window, and a row that slips
	// through has no live lease, so the next retention or GC pass prunes
	// it. Waiting on the per-lease gate here would serialise the cleanup
	// behind an in-flight checkpoint for no lasting gain.
	rows, err := s.db.DeleteNamedSnapshotsOfOwner(ctx, owner)
	if err != nil {
		s.log.Printf("user delete: drop named snapshots of %s: %v", owner, err)
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
	return res
}

// cancelUserJobs signals every running job of one owner's leases with
// TERM and marks it lost with detail "user deleted", returning the job
// ids it settled. A signal that cannot be delivered (the guest is gone)
// is logged and the job is still settled, so no running count or secret
// name survives the user. It always returns a non-nil slice, so a caller
// can append it into a JSON array without turning the field null.
func (s *Service) cancelUserJobs(ctx context.Context, owner string) []string {
	rows, err := s.db.ListRunningJobsOfOwner(ctx, owner)
	if err != nil {
		s.log.Printf("user delete: list running jobs of %s: %v", owner, err)
		return []string{}
	}
	out := make([]string, 0, len(rows))
	for _, j := range rows {
		sandboxID := ""
		if l := s.lookupAny(j.LeaseID); l != nil {
			sandboxID = l.SandboxID
			if l.live() {
				if err := s.signalJob(ctx, l, j, "TERM"); err != nil {
					s.log.Printf("user delete: cancel job %s of %s: %v", j.JobID, owner, err)
				}
			}
		}
		s.log.Printf("user delete: cancelled job %s of %s", j.JobID, owner)
		s.markJobLost(ctx, j, sandboxID, j.Generation, "user deleted")
		out = append(out, j.JobID)
	}
	return out
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

// handleUsersDelete removes a user (admin only) and cleans up the state
// that would otherwise outlive the identity (spoond-q4j): it releases
// the user's leases (reason user_deleted), cancels their running jobs,
// drops their named snapshots, unpins their kept builds and answers 200
// with what it removed. Before this cleanup an owner with no user had no
// quota, so their leases, snapshots and builds stayed and were uncapped.
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
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), userDeleteTimeout)
	defer cancel()
	res := s.svc.deleteUserData(ctx, id)
	writeJSON(w, http.StatusOK, map[string]any{"removed": res})
}
