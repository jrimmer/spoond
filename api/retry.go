package api

import (
	"errors"
	"time"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
)

// Bounded retry of crash recovery and preempt-resume (spoond-dxq). A
// lease whose sandbox died must not be marked lost on a transient
// failure (a busy node's envd start, a context deadline), and it must
// not be retried forever either: every in-flight recovery and
// preempt-resume gets a bounded budget, so the state always exits —
// recovered or lost — without waiting on a person.
//
// Recovery retries anything that is not permanent (deliberately the
// inverse of resumeRetryable): a retry could still clear it. Only a
// substrate capacity refusal waits for room without counting an attempt
// — and even that wait is bounded by the window, because a lease whose
// sandbox is gone must eventually get an exit. A preempted lease is
// different: it is parked by spoond itself and its resume is a wait for
// the room the preemption was meant to free, so an admission/capacity
// refusal neither counts nor starts the window and it waits for room
// indefinitely.

const (
	// DefaultRecoveryRetryAttempts is how many failed crash-recovery
	// attempts a lease gets before it is marked lost when
	// RECOVERY_RETRY_ATTEMPTS is unset. The reconcile loop runs every
	// 30 s, so this is roughly the first minutes of a node coming back.
	DefaultRecoveryRetryAttempts = 3
	// DefaultRecoveryRetryWindow is the wall-clock backstop on a lease
	// kept in recovery (RECOVERY_RETRY_WINDOW): even a capacity refusal
	// that never resolves gives up after this. It bounds the
	// wait-for-capacity case the attempt count alone would not.
	DefaultRecoveryRetryWindow = 30 * time.Minute
	// DefaultPreemptResumeRetries is how many failed resume attempts a
	// preempted lease gets from the preemption resume queue before it is
	// marked lost (PREEMPT_RESUME_RETRIES). The queue runs every 15 s.
	DefaultPreemptResumeRetries = 3
)

// retryBudget is the bounded retry state of one in-flight recovery or
// preempt-resume: how many counted failures it has had and when the
// first one happened (the window's origin). leaseID lets the lease API
// find a recovery budget keyed by the sandbox that failed.
type retryBudget struct {
	attempts int
	since    time.Time
	leaseID  string
}

// recoveryRetryLimit is the number of failed recovery attempts a lease
// gets. A non-positive configuration reads as the default.
func (s *Service) recoveryRetryLimit() int {
	if s.cfg.RecoveryRetryAttempts <= 0 {
		return DefaultRecoveryRetryAttempts
	}
	return s.cfg.RecoveryRetryAttempts
}

// recoveryRetryWindow is how long a lease may stay in recovery since its
// first failed attempt. A non-positive configuration reads as the
// default.
func (s *Service) recoveryRetryWindow() time.Duration {
	if s.cfg.RecoveryRetryWindow <= 0 {
		return DefaultRecoveryRetryWindow
	}
	return s.cfg.RecoveryRetryWindow
}

// preemptResumeLimit is the number of failed resume attempts a
// preempted lease gets from the resume queue. A non-positive
// configuration reads as the default.
func (s *Service) preemptResumeLimit() int {
	if s.cfg.PreemptResumeRetries <= 0 {
		return DefaultPreemptResumeRetries
	}
	return s.cfg.PreemptResumeRetries
}

// noteRetryFailure records one counted failed attempt for key in m and
// reports how many attempts are on the books and whether the budget is
// spent so the caller must give up. It is for a failure a retry could
// clear (an envd start, a deadline, any non-permanent recovery error). A
// capacity wait uses noteRetryWait instead: it is not a failure and does
// not count, and a preempt admission refusal does not touch the budget
// at all. The entry is dropped when the budget is spent (the caller is
// about to end the state).
func (s *Service) noteRetryFailure(m map[string]*retryBudget, key, leaseID string, limit int, window time.Duration) (int, bool) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	b := m[key]
	if b == nil {
		b = &retryBudget{leaseID: leaseID}
		m[key] = b
	}
	b.attempts++
	if b.since.IsZero() {
		b.since = s.now()
	}
	attempts := b.attempts
	if limit > 0 && attempts >= limit {
		delete(m, key)
		return attempts, true
	}
	if window > 0 && s.now().Sub(b.since) >= window {
		delete(m, key)
		return attempts, true
	}
	return attempts, false
}

// noteRetryWait records a recovery that is waiting for substrate
// capacity rather than failing: it does not count an attempt (the node
// may host the lease later), but it starts the window, so a wait that
// never ends is still bounded. The entry is dropped when the window is
// spent. (A preempted lease's capacity wait instead calls
// resetRetryWindow, so a long park between counted failures never ages
// an intact lease out — spoond-dxq SH1.)
func (s *Service) noteRetryWait(m map[string]*retryBudget, key, leaseID string, window time.Duration) (int, bool) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	b := m[key]
	if b == nil {
		b = &retryBudget{leaseID: leaseID}
		m[key] = b
	}
	if b.since.IsZero() {
		b.since = s.now()
	}
	if window > 0 && s.now().Sub(b.since) >= window {
		delete(m, key)
		return b.attempts, true
	}
	return b.attempts, false
}

// resetRetryWindow clears the window origin of an existing retry budget
// but keeps its counted attempts. A capacity wait calls it: the
// wall-clock window must measure only an unbroken run of counted
// failures, so a long wait for room between two counted failures cannot
// age an intact lease out (spoond-dxq SH1). A missing budget is a no-op.
func (s *Service) resetRetryWindow(m map[string]*retryBudget, key string) {
	s.retryMu.Lock()
	if b := m[key]; b != nil {
		b.since = time.Time{}
	}
	s.retryMu.Unlock()
}

// shouldLogPreemptCap reports whether the capacity-wait line for a
// preempted lease may be logged now: at most once per lease per interval
// (preemptCapLogInterval), so a long wait does not fill the log with one
// line every resume tick (spoond-dxq SH2 NIT).
func (s *Service) shouldLogPreemptCap(leaseID string) bool {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	last := s.preemptCapLogAt[leaseID]
	if !last.IsZero() && s.now().Sub(last) < preemptCapLogInterval {
		return false
	}
	s.preemptCapLogAt[leaseID] = s.now()
	return true
}

// clearPreemptCapLog drops a lease's capacity-wait log timestamp when it
// resumes or is released, so a later preemption logs its first wait.
func (s *Service) clearPreemptCapLog(leaseID string) {
	s.retryMu.Lock()
	delete(s.preemptCapLogAt, leaseID)
	s.retryMu.Unlock()
}

// clearRetry drops a retry budget by key after its recovery or resume
// succeeded or its sandbox changed: the next failure starts a fresh
// budget.
func (s *Service) clearRetry(m map[string]*retryBudget, key string) {
	if key == "" {
		return
	}
	s.retryMu.Lock()
	delete(m, key)
	s.retryMu.Unlock()
}

// retryPending reports whether key has an in-flight retry budget in m.
func (s *Service) retryPending(m map[string]*retryBudget, key string) bool {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	_, ok := m[key]
	return ok
}

// recoveryPendingSandboxes snapshots the sandbox ids with a pending
// recovery retry, so the rootfs probe can skip those leases without
// taking retryMu under the store lock (spoond-dxq SH2).
func (s *Service) recoveryPendingSandboxes() map[string]bool {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	out := make(map[string]bool, len(s.recoveryRetries))
	for key := range s.recoveryRetries {
		out[key] = true
	}
	return out
}

// recoveryPending reports whether the sandbox that failed for a lease is
// still awaiting another recovery pass. The recovery budget is keyed by
// the sandbox id: a lease that got a new sandbox (restart, restore,
// resume) has no pending retry for it, so reconcile never rolls a
// healthy lease back to an old checkpoint (spoond-dxq B2).
func (s *Service) recoveryPending(sandboxID string) bool {
	return s.retryPending(s.recoveryRetries, sandboxID)
}

// recoveryStatus reports the pending recovery retry of a lease: how many
// counted attempts it has, the attempt limit and since when, so GET can
// expose it. It only matches the sandbox the lease currently has; a
// budget left over from a sandbox the lease has left is ignored and
// dropped.
func (s *Service) recoveryStatus(l *Lease) (attempt, of int, since time.Time, ok bool) {
	if l == nil {
		return 0, 0, time.Time{}, false
	}
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	for key, b := range s.recoveryRetries {
		if b.leaseID != l.ID {
			continue
		}
		if key != l.SandboxID {
			// The lease has a new sandbox: the budget is stale.
			delete(s.recoveryRetries, key)
			continue
		}
		return b.attempts, s.recoveryRetryLimit(), b.since, true
	}
	return 0, 0, time.Time{}, false
}

// clearRecoveryRetries drops every recovery budget belonging to a lease
// (its current sandbox and any stale one from a sandbox it has left).
// Call it on every path that gives the lease a new sandbox — restart,
// restore, resume — and on recovery success, loss and release.
func (s *Service) clearRecoveryRetries(l *Lease) {
	if l == nil {
		return
	}
	s.retryMu.Lock()
	delete(s.recoveryRetries, l.SandboxID)
	for key, b := range s.recoveryRetries {
		if b.leaseID == l.ID {
			delete(s.recoveryRetries, key)
		}
	}
	s.retryMu.Unlock()
}

// recoveryWaitForCapacity reports whether a recovery failure is a
// substrate capacity refusal — the only recovery error that waits for
// capacity instead of counting an attempt. Admission refusals
// (errQuotaExceeded, errBurstReserve, errPreemptCannot) cannot reach a
// live lease's recovery: recoverFromCheckpoint skips admission for a
// running lease, and a suspended lease's refusal is a failure that a
// retry could clear, so it is counted (spoond-dxq S4).
func recoveryWaitForCapacity(err error) bool {
	return err != nil && errors.Is(err, substrate.ErrCapacity)
}

// preemptWaitForCapacity reports whether a preempted lease's resume was
// refused for room. Such a lease waits for capacity indefinitely by
// design: the preemption parked it to free the room, so the refusal
// neither counts an attempt nor starts/extends the window. Only a
// counted (non-admission) failure is bounded (spoond-dxq B1).
func preemptWaitForCapacity(err error) bool {
	if err == nil {
		return false
	}
	return undrainAdmissionRefusal(err) || errors.Is(err, substrate.ErrCapacity)
}

// recoveryFailurePermanent reports whether err can never succeed on a
// retry: the checkpoint's image or build is gone. Those lose the lease
// at once. Everything else is transient and gets the bounded retry.
func recoveryFailurePermanent(err error) bool {
	return err != nil && permanentNotFound(err)
}

// permanentNotFound reports whether err says a stored image or build is
// gone: a retry cannot bring it back.
func permanentNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound) || errors.Is(err, substrate.ErrNotFound) || errors.Is(err, errNotFound)
}
