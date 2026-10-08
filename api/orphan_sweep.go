package api

import (
	"context"
	"sync"
	"time"
)

// Periodic orphan sandbox sweep (spoond-abc, spoond-63a). The sweep is
// the backstop for a lost path's bounded delete that still failed and for
// any guest left over from an earlier incarnation. It never touches a
// sandbox a live operation owns:
//
//   - a sandbox a lease row names in any non-lost, non-released state is
//     kept (a running, suspended or busy lease owns its guest);
//   - a sandbox a pool entry names is kept (a warm VM);
//   - a sandbox a creation currently holds in flight is kept, from the
//     moment its id is chosen until the create returns;
//   - a sandbox no lease and no pool entry claims is deleted only when it
//     was already unclaimed on the previous pass, keyed by sandbox id and
//     StartedAt, so a create whose lease row lands a moment later is
//     never swept;
//   - the whole sweep is skipped while the node is draining.
//
// The pool and lease claims are re-checked under the store lock right
// before each Delete, so a claim that lands mid-pass still wins.

// defaultOrphanSweepInterval is how often the periodic orphan sandbox
// sweep runs when Start launches it (spoond-abc). It is frequent enough
// that a lost path's failed delete is retried within a sweep tick, and
// rare enough that the substrate List is not spammed. Tests shorten the
// Service field directly.
const defaultOrphanSweepInterval = 60 * time.Second

// orphanSweepState is the periodic orphan sweep's in-memory state.
type orphanSweepState struct {
	mu sync.Mutex
	// creating holds the sandbox ids a Service create currently owns,
	// from the moment its id is chosen until the caller has claimed the
	// sandbox (recorded its lease or pool entry) or cleaned it up. A
	// sandbox in flight is never swept, even on a second pass, so a
	// create that is slow (a cold boot, a probe) is not deleted out from
	// under itself.
	creating map[string]struct{}
	// seen holds the sandbox ids that were unclaimed on the previous
	// pass, keyed by id to the StartedAt the substrate reported. A
	// sandbox unclaimed twice in a row with the same StartedAt is an
	// orphan; a reused id (a new boot with a new StartedAt) starts over.
	seen map[string]time.Time
}

func newOrphanSweepState() orphanSweepState {
	return orphanSweepState{
		creating: map[string]struct{}{},
		seen:     map[string]time.Time{},
	}
}

// beginCreatingSandbox records a sandbox id as in flight for the orphan
// sweep. Every path that chooses a sandbox id calls it: createSandbox is
// the one choke point, so grant, resume, restore, restart, fork, clone,
// crash recovery, the warm pool and a named-snapshot start are covered.
func (s *Service) beginCreatingSandbox(sandboxID string) {
	if sandboxID == "" {
		return
	}
	s.orphanSweep.mu.Lock()
	s.orphanSweep.creating[sandboxID] = struct{}{}
	s.orphanSweep.mu.Unlock()
}

// endCreatingSandbox drops a sandbox id from the in-flight set once the
// create returned (claimed or not).
func (s *Service) endCreatingSandbox(sandboxID string) {
	if sandboxID == "" {
		return
	}
	s.orphanSweep.mu.Lock()
	delete(s.orphanSweep.creating, sandboxID)
	s.orphanSweep.mu.Unlock()
}

// sandboxInFlight reports whether a creation currently holds sandboxID.
func (s *Service) sandboxInFlight(sandboxID string) bool {
	s.orphanSweep.mu.Lock()
	defer s.orphanSweep.mu.Unlock()
	_, ok := s.orphanSweep.creating[sandboxID]
	return ok
}

// sweepOrphanSandboxes deletes the substrate sandboxes the service has
// given up on: a lost lease's sandbox (its bounded delete on the lost
// path may have failed), an unclaimed sandbox seen unclaimed twice, and
// the sandbox ids a failed delete remembered. It is skipped entirely
// while the node is draining, and never touches a pool, live, busy or
// in-flight sandbox. spoond-abc, spoond-63a.
func (s *Service) sweepOrphanSandboxes(ctx context.Context) {
	if s.draining.Load() {
		return
	}
	s.sweepOrphanSandboxesNow(ctx)
}

// sweepOrphanSandboxesNow runs one sweep pass without the drain check,
// so startup ReconcileOrphans can run it even when the persisted drain
// flag is still set.
func (s *Service) sweepOrphanSandboxesNow(ctx context.Context) {
	// Retry the ids a previous pass's failed delete recorded first. Doing
	// this before the fresh sweep keeps a delete that fails in this pass
	// on the books for the next tick rather than retrying it immediately
	// (a sandbox that failed a delete once should not be hammered in the
	// same pass).
	deleted := s.retryRememberedOrphans(ctx)
	deleted += s.sweepSandboxOrphans(ctx)
	if deleted > 0 {
		s.log.Printf("orphan sweep: deleted %d sandbox(es) left by lost or unclaimed lease(s)", deleted)
	}
}

// retryRememberedOrphans deletes the sandbox ids a failed delete
// recorded. A successful delete drops the id; a failed one keeps it for
// the next tick. An id a lease or pool now claims is dropped without a
// delete.
func (s *Service) retryRememberedOrphans(ctx context.Context) int {
	deleted := 0
	for _, id := range s.orphanSandboxSnapshot() {
		if !s.orphanSafeToDelete(id) {
			s.forgetOrphanSandbox(id)
			continue
		}
		if err := s.sub.Delete(ctx, id); err != nil {
			s.log.Printf("orphan sweep: retry delete of %s failed: %v", id, err)
			continue
		}
		s.forgetOrphanSandbox(id)
		deleted++
		if s.metrics != nil {
			s.metrics.LeaseOrphaned.Inc()
		}
	}
	return deleted
}

// sweepSandboxOrphans lists the substrate and deletes the sandboxes this
// pass considers orphans: a lost lease's sandbox (state lost, not busy),
// and an unclaimed sandbox seen unclaimed on two consecutive passes. A
// List failure logs and deletes nothing (leases are never marked lost on
// a list failure). It returns how many sandboxes it deleted.
func (s *Service) sweepSandboxOrphans(ctx context.Context) int {
	sbs, err := s.sub.List(ctx)
	if err != nil {
		s.log.Printf("orphan sweep: list sandboxes failed: %v", err)
		return 0
	}

	// The in-flight set is snapshotted once; a create that starts after
	// this point cannot be an unclaimed sandbox the substrate listed
	// before it, and the per-delete re-check closes the rest.
	s.orphanSweep.mu.Lock()
	inFlight := make(map[string]bool, len(s.orphanSweep.creating))
	for id := range s.orphanSweep.creating {
		inFlight[id] = true
	}
	s.orphanSweep.mu.Unlock()

	type candidate struct {
		id        string
		startedAt time.Time
	}
	var lost []candidate
	var unclaimed []candidate
	s.store.mu.Lock()
	poolIDs := make(map[string]bool)
	for _, ids := range s.store.pool {
		for _, id := range ids {
			poolIDs[id] = true
		}
	}
	leaseByID := make(map[string]*Lease, len(s.store.leases))
	for _, l := range s.store.leases {
		if l.SandboxID != "" {
			leaseByID[l.SandboxID] = l
		}
	}
	for _, sb := range sbs {
		if inFlight[sb.ID] || poolIDs[sb.ID] {
			continue
		}
		l := leaseByID[sb.ID]
		switch {
		case l == nil:
			unclaimed = append(unclaimed, candidate{sb.ID, sb.StartedAt})
		case !l.busy && l.State == "lost":
			lost = append(lost, candidate{sb.ID, sb.StartedAt})
		}
	}
	s.store.mu.Unlock()

	// Two-pass rule for unclaimed sandboxes: only one already unclaimed
	// with the same StartedAt on the previous pass is deleted. The next
	// pass's seen set is exactly this pass's unclaimed set.
	s.orphanSweep.mu.Lock()
	prev := s.orphanSweep.seen
	next := make(map[string]time.Time, len(unclaimed))
	var orphans []string
	for _, c := range unclaimed {
		if t, ok := prev[c.id]; ok && t.Equal(c.startedAt) {
			orphans = append(orphans, c.id)
			continue
		}
		next[c.id] = c.startedAt
	}
	s.orphanSweep.seen = next
	s.orphanSweep.mu.Unlock()

	deleted := 0
	for _, c := range lost {
		if !s.orphanSafeToDelete(c.id) {
			continue
		}
		if s.deleteOrphanSandbox(ctx, c.id) {
			deleted++
		}
	}
	for _, id := range orphans {
		if !s.orphanSafeToDelete(id) {
			continue
		}
		if s.deleteOrphanSandbox(ctx, id) {
			deleted++
		}
	}
	return deleted
}

// deleteOrphanSandbox deletes one orphan and counts it. A failed delete
// keeps the id so a later pass can try again.
func (s *Service) deleteOrphanSandbox(ctx context.Context, id string) bool {
	if err := s.sub.Delete(ctx, id); err != nil {
		s.log.Printf("orphan sweep: delete orphan %s failed: %v", id, err)
		s.rememberOrphanSandbox(id)
		return false
	}
	if s.metrics != nil {
		s.metrics.LeaseOrphaned.Inc()
	}
	return true
}

// orphanSafeToDelete is the final guard before the sweep deletes a
// sandbox: it re-checks, under the store lock, that no creation holds id
// in flight, no pool entry claims it, and no lease owns it in a state
// other than lost. A live, suspended or busy lease's sandbox is never
// safe. Call without s.store.mu.
func (s *Service) orphanSafeToDelete(id string) bool {
	if s.sandboxInFlight(id) {
		return false
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	for _, ids := range s.store.pool {
		for _, pid := range ids {
			if pid == id {
				return false
			}
		}
	}
	for _, l := range s.store.leases {
		if l.SandboxID != id {
			continue
		}
		return !l.busy && l.State == "lost"
	}
	return true
}

// forgetOrphanSandbox drops a remembered sandbox id once it is gone or
// no longer safe to delete.
func (s *Service) forgetOrphanSandbox(sandboxID string) {
	s.orphanMu.Lock()
	delete(s.orphanSandboxIDs, sandboxID)
	s.orphanMu.Unlock()
}

// ReconcileOrphans aligns the substrate with the state loaded from the
// store. If the sandbox list fails, nothing changes (leases are never
// marked lost on a list failure). The first pass deletes substrate
// sandboxes no lease row and no pool entry names — a foreign leftover
// from a previous incarnation. The per-lease crash handling (lost/
// recovered marking, U10) runs next via reconcileCrash, and the orphan
// sweep runs last: it stops a lost lease's leftover sandbox and any
// remembered failed delete, before the first periodic tick.
func (s *Service) ReconcileOrphans(ctx context.Context) {
	if n := s.sweepForeignSandboxes(ctx); n > 0 {
		s.log.Printf("reconcile: deleted %d orphaned sandbox(es) from a previous incarnation", n)
	}

	s.reconcileCrash(ctx)

	// A lost lease's sandbox is an orphan and reconcileCrash's own
	// bounded delete may have given up on it. Sweep now, before the
	// first tick, plus any id a prior failed delete recorded.
	s.sweepOrphanSandboxesNow(ctx)
}

// sweepForeignSandboxes deletes every substrate sandbox no lease row and
// no pool entry claims — the startup first pass. It returns how many it
// deleted and prunes nothing from the pool: reconcileCrash owns pool
// pruning. A List failure logs and deletes nothing.
func (s *Service) sweepForeignSandboxes(ctx context.Context) int {
	sbs, err := s.sub.List(ctx)
	if err != nil {
		s.log.Printf("reconcile: list sandboxes failed: %v", err)
		return 0
	}
	s.store.mu.Lock()
	claimed := make(map[string]bool)
	for _, l := range s.store.leases {
		if l.SandboxID != "" {
			claimed[l.SandboxID] = true
		}
	}
	for _, ids := range s.store.pool {
		for _, id := range ids {
			claimed[id] = true
		}
	}
	s.store.mu.Unlock()

	deleted := 0
	for _, sb := range sbs {
		if claimed[sb.ID] {
			continue
		}
		// Never sweep a sandbox a creation currently holds, even on the
		// startup pass: a grant that raced startup must not lose its
		// guest.
		if s.sandboxInFlight(sb.ID) {
			continue
		}
		if s.deleteOrphanSandbox(ctx, sb.ID) {
			deleted++
		}
	}
	return deleted
}
