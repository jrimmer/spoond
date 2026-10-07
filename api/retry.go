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
// first one happened (the window's origin).
type retryBudget struct {
	attempts int
	since    time.Time
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

// noteRetryFailure records one failed attempt for id in m and reports
// how many attempts are on the books and whether the budget is spent so
// the caller must give up. countIt false (an admission/capacity refusal)
// does not increment the attempt count — such a lease is waiting for
// capacity, not failing to recover — but it still starts the window, so
// even a wait that never ends is bounded. The entry is dropped when the
// budget is spent (the caller is about to end the state).
func (s *Service) noteRetryFailure(m map[string]*retryBudget, id string, countIt bool, limit int, window time.Duration) (int, bool) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	b := m[id]
	if b == nil {
		b = &retryBudget{since: s.now()}
		m[id] = b
	}
	if countIt {
		b.attempts++
	}
	attempts := b.attempts
	if countIt && limit > 0 && attempts >= limit {
		delete(m, id)
		return attempts, true
	}
	if window > 0 && s.now().Sub(b.since) >= window {
		delete(m, id)
		return attempts, true
	}
	return attempts, false
}

// clearRetry drops a lease's retry budget after its recovery or resume
// succeeded: the next failure starts a fresh budget.
func (s *Service) clearRetry(m map[string]*retryBudget, id string) {
	s.retryMu.Lock()
	delete(m, id)
	s.retryMu.Unlock()
}

// retryPending reports whether id has an in-flight retry budget in m.
func (s *Service) retryPending(m map[string]*retryBudget, id string) bool {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	_, ok := m[id]
	return ok
}

// recoveryPending reports whether a lease's crash recovery is awaiting
// another pass (a previous attempt failed transiently or is waiting for
// capacity).
func (s *Service) recoveryPending(id string) bool {
	return s.retryPending(s.recoveryRetries, id)
}

// recoveryRefusalPending reports whether err is one of the admission
// answers a recovery can wait out (over quota, no burst room, no
// preemption room, the node refusing capacity). Such a lease keeps
// recovering and is retried, bounded by the recovery window rather than
// the attempt count.
func recoveryRefusalPending(err error) bool {
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
