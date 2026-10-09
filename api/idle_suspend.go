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
// and held rule 1 (and rule 4's pressure shortening). Leases without a
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

// ensureRunning serves the next work call on a suspended lease: it
// resumes the lease through the normal resume path (admission, class and
// quota apply) and then lets the caller serve. Every suspension resumes
// this way (#145 D2, one rule for every kind of suspend): a lease
// suspended by pressure, preemption, the idle sweep, idle_suspend or a
// lapsed hold (rule 3), and one its holder suspended by hand. A refusal
// answers what resume would: 429 for the owner's own memory quota, 503
// capacity_wait with Retry-After when the node has no room (a structural
// shortage never refuses: the caller waits), 409 lease_busy while its
// pause or another caller's resume is in flight, 410 lease_lost when its
// sandbox is gone. GET, status, events and SSE never call this. A lease
// whose hold lapsed resumes with its hold still lapsed: resuming does not
// renew a hold. Returns false when the caller must stop (the response is
// written).
func (s *Server) ensureRunning(w http.ResponseWriter, r *http.Request, l *Lease) bool {
	if !l.Suspended {
		return true
	}
	if _, err := s.svc.resumeLease(r.Context(), l); err != nil {
		s.writeResumeRefusal(w, l.ID, err)
		return false
	}
	return true
}

// writeResumeRefusal maps a failed resume onto the response the resume
// route would give: a quota refusal is 429, a lease busy with its pause
// or another resume is 409 lease_busy, a lost sandbox 410 lease_lost with
// the reason, and every structural no-room refusal is 503 with
// Retry-After and code capacity_wait. It is shared by the resume route
// and every resume-on-use path so they cannot drift (#145 D2).
func (s *Server) writeResumeRefusal(w http.ResponseWriter, id string, err error) {
	writeResumeRefusal(w, s.svc.log, id, err)
}

// resumeNoRoom reports whether a failed resume was refused for a
// structural shortage of host resources (hugepages, the burst reserve,
// the snapshot disk floor, a draining node). Per the 2026-10-08 owner
// contract such a refusal is a wait, never a loss: the caller gets 503
// capacity_wait with a Retry-After. Only per-owner quota refuses
// otherwise (memory quota 429).
func resumeNoRoom(err error) bool {
	return errors.Is(err, errPreemptCannot) || errors.Is(err, errBurstReserve) ||
		errors.Is(err, substrate.ErrCapacity) || errors.Is(err, errDraining)
}

// resumeNoRoomMessage is the human detail of a no-room resume refusal,
// keeping the specific cause the error carries while the code stays the
// one capacity_wait every path shares.
func resumeNoRoomMessage(err error) string {
	switch {
	case errors.Is(err, errDraining):
		return "draining"
	case errors.Is(err, errBurstReserve):
		return err.Error()
	default:
		// errPreemptCannot and substrate.ErrCapacity keep their
		// "capacity: ..." prefix.
		return "capacity: " + err.Error()
	}
}

// writeResumeRefusal is the free-function form shared by the API server
// and the per-lease LLM gateway, which has no *Server. log names the
// default-case diagnostics.
func writeResumeRefusal(w http.ResponseWriter, log interface{ Printf(string, ...any) }, id string, err error) {
	if writeLeaseLostErr(w, err) {
		// The reason already lives in the error message; writeLeaseLostErr
		// keeps it verbatim under code lease_lost.
		return
	}
	switch {
	case errors.Is(err, errNotPersistent):
		writeError(w, http.StatusBadRequest, "lease is not a workspace-backed persistent lease")
	case errors.Is(err, errLeaseBusy):
		writeErrorCode(w, http.StatusConflict, "lease_busy", err.Error())
	case errors.Is(err, errLeaseReleased):
		writeError(w, http.StatusNotFound, "lease not found")
	case errors.Is(err, errQuotaExceeded):
		writeError(w, http.StatusTooManyRequests, err.Error())
	case resumeNoRoom(err):
		writeErrorCodeAfter(w, http.StatusServiceUnavailable, burstRetryAfterSecs, "capacity_wait", resumeNoRoomMessage(err))
	case errors.Is(err, errOwnerGone):
		// The owner's identity was removed while the resume was in
		// flight (spoond-q4j): the user is gone, so the resume is
		// refused rather than run ownerless.
		writeError(w, http.StatusForbidden, "owner deleted")
	default:
		log.Printf("resume %s: %v", id, err)
		writeError(w, http.StatusInternalServerError, "resume failed")
	}
}
