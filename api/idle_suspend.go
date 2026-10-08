package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jrimmer/spoond/v2/substrate"
)

// Per-lease idle reclamation (2.5, #129 part 2): a persistent lease whose
// effective idle_suspend is > 0 and that has been idle that long is
// suspended through the normal pause path — memory and hugepages are
// freed into a pause build, nothing is deleted, the generation does not
// change, and the next call resumes it. For such a lease the setting is
// the only idle threshold: it replaces the plain IDLE_TIMEOUT_SECS sweep
// and held rule 1, and the guaranteed-unheld-idle pressure step does not
// reclaim it (the idle sweep does, on its own threshold; a burst lease
// stays reclaimable by the pressure order's burst steps, like
// preemption). Leases without a
// value keep those rules exactly.
//
// Idle suspension shares preemption's snapshot-disk floor
// (PREEMPT_DISK_FLOOR_PCT, api/preempt.go): a pause that would take the
// disk under it is skipped this sweep and retried on the next one.

// idleSuspendRule is the recorded LastAction rule ("idle_suspend"): a
// lease suspended by this rule may later be released by the stale-release
// (rule 2) and critical-disk (rule 5) held-lease rules, like any other
// rule suspension.
const idleSuspendRule = "idle_suspend"

// suspendIdleLeases suspends every persistent lease idle past its own
// effective idle_suspend, through the normal pause path. It never
// suspends a lease with idle_suspend 0 (or no host default), a
// non-persistent lease, or a lease whose pause would take the snapshot
// disk under the preemption floor.
func (s *Service) suspendIdleLeases(ctx context.Context, now time.Time) {
	var idle []*Lease
	s.store.mu.Lock()
	for _, l := range s.store.leases {
		// A running background job is activity (2.6, #135): never
		// suspend a lease mid-job.
		if l.released || l.Suspended || l.busy || !l.Persistent || s.hasRunningJobLocked(l.ID) {
			continue
		}
		threshold := time.Duration(s.effectiveIdleSuspend(l)) * time.Second
		if threshold <= 0 || !now.After(l.LastActive.Add(threshold)) {
			continue
		}
		idle = append(idle, l)
	}
	s.store.mu.Unlock()

	// Suspend in small, staggered batches, like the plain idle sweep:
	// each suspension is a snapshot write on the node, and a backlog
	// must not produce one big burst. When the process-wide limiter is
	// busy (a hand suspend, a checkpoint or the drain is writing), the
	// sweep stands down for this tick and retries the next one instead
	// of queueing its batch behind the running write.
	const maxIdleSuspendPerTick = 3
	suspended := 0
	for _, l := range idle {
		if suspended >= maxIdleSuspendPerTick {
			break
		}
		if s.snapshotBusy() {
			break
		}
		// Re-check under the lock just before pausing: activity (an exec,
		// a heartbeat, the files API) can land between the collection pass
		// and this pause, and a lease that moved in the meantime must not
		// be suspended.
		s.store.mu.Lock()
		threshold := time.Duration(s.effectiveIdleSuspend(l)) * time.Second
		lastActive := l.LastActive
		skip := l.released || l.Suspended || l.busy || !l.Persistent || threshold <= 0 ||
			!now.After(lastActive.Add(threshold))
		s.store.mu.Unlock()
		if skip {
			continue
		}
		if !s.preemptDiskOK(l) {
			s.log.Printf("idle suspend: lease %s: pause would take the snapshot disk under the %.0f%% floor; skipping this sweep",
				l.ID, s.preemptDiskFloorPct())
			continue
		}
		if _, err := s.pauseLeaseWith(ctx, l, false, suspendPolicy{reason: suspendReasonIdleSuspend}); err != nil {
			// A release that raced the pause is not an idle-suspend error:
			// the lease is gone and nothing was suspended (spoond-15i).
			if !errors.Is(err, errLeaseBusy) && !errors.Is(err, errLeaseReleased) {
				s.log.Printf("idle suspend: lease %s: %v", l.ID, err)
			}
			continue
		}
		s.recordIdleSuspend(l, lastActive, now)
		suspended++
		time.Sleep(500 * time.Millisecond)
	}
}

// recordIdleSuspend records an idle suspension like the held rules do:
// one log line, the spoond_idle_suspends_total counter, the lease's
// last_action/last_action_at (persisted) and one idle_suspended event
// whose detail names how long the lease had been idle.
func (s *Service) recordIdleSuspend(l *Lease, lastActive, now time.Time) {
	idleFor := now.Sub(lastActive).Round(time.Second)
	s.store.mu.Lock()
	if l.released {
		// The lease was released after pauseLease returned success but
		// before this record: neither the counter nor the event may be
		// bumped for a released lease (spoond-15i).
		s.store.mu.Unlock()
		return
	}
	s.log.Printf("idle suspend: lease %s idle since %s (%s), suspending",
		l.ID, lastActive.Format(time.RFC3339), idleFor)
	if s.metrics != nil {
		s.metrics.IdleSuspendsTotal.Inc()
	}
	l.LastAction = idleSuspendRule + "/" + heldActionSuspendIdle
	l.LastActionAt = now
	s.saveLeaseLocked(l)
	// Emit under the lock: the bus never takes the store lock, and a
	// release landing after this check must not leave an idle_suspended
	// event for a released lease on the stream (spoond-15i).
	s.emitLeaseEvent(l.ID, l.Owner, LeaseIdleSuspended, "idle for "+idleFor.String())
	s.store.mu.Unlock()
}

// idleSuspended reports whether l is suspended by the idle_suspend rule
// and only that — a lease the resume queue preempted, one suspended
// by hand or the drain, or one whose resume/checkpoint is in flight is
// not. Call with s.store.mu held.
func idleSuspended(l *Lease) bool {
	// Every pause records its own LastAction (pauseLeaseBody), so the
	// idle_suspend marker here always describes the current suspension.
	return l.Suspended && !l.busy && !l.Drained && l.PreemptedAt.IsZero() &&
		l.LastAction == idleSuspendRule+"/"+heldActionSuspendIdle
}

// isIdleSuspended is idleSuspended with the lock taken.
func (s *Service) isIdleSuspended(l *Lease) bool {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	return idleSuspended(l)
}

// ensureRunning serves the next call on a lease that idle_suspend
// suspended: it resumes the lease through the normal resume path
// (admission, class and quota apply) and then lets the caller serve. A
// refusal answers what resume would (429/503 with Retry-After). A lease
// suspended any other way keeps today's 409 "suspended (resume it
// first)". Returns false when the caller must stop (the response is
// written).
func (s *Server) ensureRunning(w http.ResponseWriter, r *http.Request, l *Lease) bool {
	if !l.Suspended {
		return true
	}
	if !s.svc.isIdleSuspended(l) {
		writeLeaseSuspended(w, s.svc.leaseSuspendReason(l.ID))
		return false
	}
	if _, err := s.svc.resumeLease(r.Context(), l); err != nil {
		s.writeResumeRefusal(w, l.ID, err)
		return false
	}
	return true
}

// writeResumeRefusal maps a failed resume onto the response the resume
// route would give: a quota or burst-reserve refusal is 429/503 with a
// Retry-After, a busy lease is 409, a lost sandbox 410 lease_lost with
// the reason. It is shared by the resume route and the idle auto-resume
// paths so the two cannot drift.
func (s *Server) writeResumeRefusal(w http.ResponseWriter, id string, err error) {
	if writeLeaseLostErr(w, err) {
		// The reason already lives in the error message; writeLeaseLostErr
		// keeps it verbatim under code lease_lost.
		return
	}
	switch {
	case errors.Is(err, errNotPersistent):
		writeError(w, http.StatusBadRequest, "lease is not a workspace-backed persistent lease")
	case errors.Is(err, errLeaseBusy):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errLeaseReleased):
		writeError(w, http.StatusNotFound, "lease not found")
	case errors.Is(err, errQuotaExceeded):
		writeError(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, errPreemptCannot):
		writeErrorAfter(w, http.StatusServiceUnavailable, burstRetryAfterSecs, "capacity: "+err.Error())
	case errors.Is(err, errBurstReserve):
		writeErrorAfter(w, http.StatusServiceUnavailable, burstRetryAfterSecs, err.Error())
	case errors.Is(err, errOwnerGone):
		// The owner's identity was removed while the resume was in
		// flight (spoond-q4j): the user is gone, so the resume is
		// refused rather than run ownerless.
		writeError(w, http.StatusForbidden, "owner deleted")
	case errors.Is(err, substrate.ErrCapacity):
		writeError(w, http.StatusServiceUnavailable, "capacity: "+err.Error())
	default:
		s.svc.log.Printf("resume %s: %v", id, err)
		writeError(w, http.StatusInternalServerError, "resume failed")
	}
}
