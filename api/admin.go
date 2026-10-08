package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jrimmer/spoond/v2/substrate"
)

// Admin endpoints (U10): POST /api/admin/drain pauses every running
// sandbox so the orchestrator can restart losslessly, POST
// /api/admin/undrain resumes the drained leases afterwards, and POST
// /api/admin/reconcile recovers leases after an orchestrator crash.
//
// Authentication is Authorization: Bearer <ADMIN_TOKEN>, compared in
// constant time. ADMIN_TOKEN is not a user or consumer token, so
// authMiddleware lets the /api/admin/ prefix through and this file does
// the check. With no ADMIN_TOKEN configured the routes answer 404; a
// wrong or missing token answers 401.

// drainFailure is one lease the drain or undrain could not handle.
// Attempts is how many resume attempts undrain made before giving up
// (0 and omitted for a drain pause, which makes no resume).
type drainFailure struct {
	ID       string `json:"id"`
	Error    string `json:"error"`
	Attempts int    `json:"attempts,omitempty"`
}

// drainResult is the POST /api/admin/drain response.
type drainResult struct {
	Paused      int            `json:"paused"`
	Failed      []drainFailure `json:"failed"`
	PoolDeleted int            `json:"pool_deleted"`
	Quiesced    bool           `json:"quiesced"`
}

// undrainResult is the POST /api/admin/undrain response.
type undrainResult struct {
	Resumed int            `json:"resumed"`
	Failed  []drainFailure `json:"failed"`
}

// adminOK authenticates an admin request. 404 when no ADMIN_TOKEN is
// configured (the routes are disabled), 401 on a wrong or missing
// token.
func (s *Server) adminOK(w http.ResponseWriter, r *http.Request) bool {
	if s.adminToken == "" {
		writeError(w, http.StatusNotFound, "not found")
		return false
	}
	auth := r.Header.Get("Authorization")
	token := strings.TrimPrefix(auth, "Bearer ")
	if token == "" || token == auth || subtle.ConstantTimeCompare([]byte(token), []byte(s.adminToken)) != 1 {
		writeError(w, http.StatusUnauthorized, "invalid admin token")
		return false
	}
	return true
}

func (s *Server) handleAdminDrain(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(w, r) {
		return
	}
	// A detached context: a client that gives up waiting (the drain hook's
	// own timeout, a dropped connection) must not cancel the pauses
	// half-way and leave leases running into the stop. The bound keeps
	// the work finite.
	ctx, cancel := adminContext(r.Context(), adminDrainTimeout)
	defer cancel()
	res, err := s.svc.drain(ctx)
	if err != nil {
		// Nothing changed: the node stays healthy and undrained.
		writeError(w, http.StatusServiceUnavailable, "orchestrator unreachable: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleAdminUndrain(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(w, r) {
		return
	}
	ctx, cancel := adminContext(r.Context(), adminUndrainTimeout)
	defer cancel()
	writeJSON(w, http.StatusOK, s.svc.undrain(ctx))
}

// handleAdminReconcile runs the crash reconciliation now and returns
// its summary (U10).
func (s *Server) handleAdminReconcile(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, s.svc.reconcileCrash(r.Context()))
}

// drainConcurrency bounds the concurrent pauses of the drain.
const drainConcurrency = 4

// Admin call bounds. A drain or undrain runs on a context detached from
// its request (a client disconnect must not cancel a pause or resume
// half-way) and bounded so the work is finite. The drain bound covers
// the drain's own 180 s quiesce wait plus the pause phase; the undrain
// bound covers its 120 s node wait plus the resumes.
const (
	adminDrainTimeout   = 6 * time.Minute
	adminUndrainTimeout = 5 * time.Minute
)

// adminContext detaches ctx from its caller and bounds it. context
// values (the admin token's audit fields, if any) are kept; the
// cancellation from the request is dropped.
func adminContext(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), d)
}

// DefaultUndrainConcurrency is how many drained leases the admin
// undrain resumes at once when UNDRAIN_CONCURRENCY is unset: two, so
// restoring a batch of large memory snapshots does not stack the whole
// node's I/O and memory at once (spoond-urm).
const DefaultUndrainConcurrency = 2

// DefaultUndrainResumeRetries is how many extra attempts the admin
// undrain gives a resume that failed with a retryable envd/start error
// when UNDRAIN_RESUME_RETRIES is unset: two, so a transient "syncing took
// too long" does not lose the lease (spoond-urm).
const DefaultUndrainResumeRetries = 2

// DefaultDrainMaxSecs is how long a drain may stay in effect while the
// node is healthy before spoond undrains itself when DRAIN_MAX_SECS is
// unset (DrainMaxSecs 0): 900 s. It is longer than a planned
// orchestrator restart, so a normal drain/undrain cycle never trips it;
// a drain whose undrain never arrives does not refuse creates forever.
const DefaultDrainMaxSecs = 900

// DefaultDrainResumeMaxAge is how long the drain self-heal loop keeps
// retrying a lease whose resume is deferred when DRAIN_RESUME_MAX_AGE is
// unset (DrainResumeMaxAge 0): 24 h. Past it the loop stops retrying,
// keeps the lease suspended (its snapshot is intact) and emits a
// drain_gave_up event, leaving the exit to the owner or the idle rules.
const DefaultDrainResumeMaxAge = 24 * time.Hour

// Drain self-heal backoff for a deferred resume: the first retry is the
// next pass (drainHealInterval), doubling to drainHealBackoffMax so a
// permanently deferred lease is not resumed every 15 s for ever.
const (
	drainHealBackoffMin = drainHealInterval
	drainHealBackoffMax = 10 * time.Minute
)

// drainHealInterval is how often the drain self-heal loop looks for a
// stale drain or for drained leases to resume.
const drainHealInterval = 15 * time.Second

// undrainRetryBackoff is the pause between undrain resume attempts.
const undrainRetryBackoff = 500 * time.Millisecond

// drain pauses every live lease (persistent or not) into a pause build
// and marks it Drained, deletes the warm pool, then waits until the node
// reports no running sandboxes and no outstanding work. On a
// SetDraining failure nothing is changed and the error is returned (the
// handler answers 503 and s.draining stays false). Per-lease failures
// are recorded and the drain continues.
// Drain pauses write snapshots through the drain's own limiter
// (DRAIN_SNAPSHOT_CONCURRENCY, default 2) rather than the default one
// (SNAPSHOT_WRITE_CONCURRENCY, default 1), so a planned restart can
// finish a batch of pauses inside the unit's drain window. The unit's
// TimeoutStopSec must cover leases × per-pause time / drain width.
func (s *Service) drain(ctx context.Context) (drainResult, error) {
	// Hold off the rootfs probe's recovery for the whole drain: a pass
	// already past its draining check must not delete a sandbox and run
	// a recovery once SetDraining has told the substrate to drain
	// (spoond-5ca). SetDraining runs under the write side, so the probe
	// sees one consistent ordering.
	s.drainGate.Lock()
	if err := s.sub.SetDraining(ctx, true); err != nil {
		s.drainGate.Unlock()
		return drainResult{}, err
	}
	s.draining.Store(true)
	s.drainClearPending.Store(false)
	s.drainStartedAt.Store(s.now().UnixNano())
	s.drainGate.Unlock()
	res := drainResult{Failed: []drainFailure{}}

	// Waiting creates are pointless on a draining node: answer them all
	// with 503 draining at once (#129 part 1).
	s.drainQueue()

	// Pause every live lease, up to 4 at a time.
	s.store.mu.Lock()
	var targets []*Lease
	for _, l := range s.store.leases {
		if !l.released && l.live() {
			targets = append(targets, l)
		}
	}
	s.store.mu.Unlock()

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, drainConcurrency)
	for _, l := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(l *Lease) {
			defer wg.Done()
			defer func() { <-sem }()
			if _, err := s.pauseLease(ctx, l, true); err != nil {
				if errors.Is(err, errLeaseReleased) {
					// The lease was released while its pause ran: there is
					// nothing left to drain, so it is skipped rather than
					// reported as a failure (spoond-d76).
					return
				}
				// A lease left running into the orchestrator stop must be
				// visible outside the HTTP response, which a hook that has
				// already given up never reads (spoond-52c R2).
				s.log.Printf("drain: pause %s failed; left running into the stop: %v", l.ID, err)
				s.emitLeaseEvent(l.ID, l.Owner, LeaseDrainFailed, err.Error())
				mu.Lock()
				res.Failed = append(res.Failed, drainFailure{ID: l.ID, Error: err.Error()})
				mu.Unlock()
				return
			}
			mu.Lock()
			res.Paused++
			mu.Unlock()
		}(l)
	}
	wg.Wait()

	// The warm pool would be orphaned by the restart: delete it.
	s.store.mu.Lock()
	var poolIDs []string
	for _, ids := range s.store.pool {
		poolIDs = append(poolIDs, ids...)
	}
	s.store.mu.Unlock()
	for _, id := range poolIDs {
		if err := s.sub.Delete(ctx, id); err != nil {
			s.log.Printf("drain: delete pool sandbox %s: %v", id, err)
			continue
		}
		s.store.mu.Lock()
		s.removePoolLocked(id)
		for img, ids := range s.store.pool {
			kept := ids[:0]
			for _, pid := range ids {
				if pid != id {
					kept = append(kept, pid)
				}
			}
			s.store.pool[img] = kept
		}
		s.store.mu.Unlock()
		res.PoolDeleted++
	}

	// Wait until the node is quiet: no running sandboxes, no
	// outstanding work (in-flight snapshot uploads count as work).
	deadline := time.Now().Add(180 * time.Second)
	for {
		info, err := s.sub.NodeInfo(ctx)
		if err == nil && info.RunningSandboxes == 0 && info.OutstandingWork == 0 {
			res.Quiesced = true
			break
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			s.log.Printf("drain: quiesce wait cancelled: %v", ctx.Err())
			return res, nil
		case <-time.After(time.Second):
		}
	}
	return res, nil
}

// undrainConcurrency is the width of the undrain's resume pool: the
// configured UNDRAIN_CONCURRENCY, with 0 = unlimited (cmd maps an unset
// variable to DefaultUndrainConcurrency).
func (s *Service) undrainConcurrency() int {
	if s.cfg.UndrainConcurrency < 0 {
		return 0
	}
	return s.cfg.UndrainConcurrency
}

// undrainResumeRetries is how many extra attempts a retryable resume
// failure gets: the configured UNDRAIN_RESUME_RETRIES (0 disables
// retries; cmd maps an unset variable to DefaultUndrainResumeRetries).
func (s *Service) undrainResumeRetries() int {
	if s.cfg.UndrainResumeRetries < 0 {
		return 0
	}
	return s.cfg.UndrainResumeRetries
}

// undrainAdmissionRefusal reports whether err is one of the transient
// admission answers (over quota, no burst room, no preemption room): they
// keep the lease drained for a later undrain and must not be retried in a
// tight loop here.
func undrainAdmissionRefusal(err error) bool {
	return errors.Is(err, errQuotaExceeded) || errors.Is(err, errBurstReserve) || errors.Is(err, errPreemptCannot)
}

// resumeRetryable reports whether a failed undrain resume is worth
// retrying. The envd/start failures a busy node answers on resume
// ("syncing took too long", a context deadline, envd init not healthy)
// are transient; an image or build that is gone is not.
func resumeRetryable(err error) bool {
	if err == nil {
		return false
	}
	// Permanent: the image or build the resume needs is gone. A retry
	// cannot bring it back, so the lease goes lost at once.
	if permanentNotFound(err) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"syncing took too long",
		"failed to init envd",
		"failed to init new envd",
		"envd not healthy",
		"context deadline exceeded",
		"deadline exceeded",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// undrainDeferred reports whether a failed undrain resume must leave
// the lease Drained for a later attempt rather than marking it lost. An
// admission refusal, a capacity answer and a cancelled or bounded
// context are transient conditions the next undrain (or the self-heal
// loop) can retry; a sandbox that will not come back is not covered and
// still becomes lost.
func undrainDeferred(err error) bool {
	return undrainAdmissionRefusal(err) ||
		errors.Is(err, errLeaseBusy) ||
		errors.Is(err, substrate.ErrCapacity) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, errDraining)
}

// resumeDrainedLease runs one drained lease through resumeLease, giving a
// retryable envd/start failure up to the configured number of extra
// attempts with a short backoff. It returns the last error (nil on
// success) and how many attempts were made. An admission refusal is
// returned at once: it keeps the lease drained for a later undrain. While
// spoond is draining the resume is deferred at once (the node would
// refuse it), so a heal pass that begins before an admin drain clears the
// state does not race a Create into the stop (spoond-52c S1).
func (s *Service) resumeDrainedLease(ctx context.Context, l *Lease) (error, int) {
	maxAttempts := 1 + s.undrainResumeRetries()
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if s.draining.Load() {
			return errDraining, attempt
		}
		_, err = s.resumeLease(ctx, l)
		if err == nil {
			return nil, attempt
		}
		if undrainAdmissionRefusal(err) {
			return err, attempt
		}
		if !resumeRetryable(err) || attempt == maxAttempts {
			return err, attempt
		}
		s.log.Printf("undrain: resume %s attempt %d/%d failed (retrying): %v", l.ID, attempt, maxAttempts, err)
		// resumeLeaseBody's createSandbox error path already deleted any
		// half-started sandbox inside the busy window, so the retry can
		// reuse the same id; just space the attempts out.
		select {
		case <-ctx.Done():
			return err, attempt
		case <-time.After(undrainRetryBackoff):
		}
	}
	return err, maxAttempts
}

// deleteHalfSandbox removes a sandbox a failed or cut-off Create may have
// left half-started, best effort, and drops its row so the next resume can
// reuse the same sandbox id (spoond-52c S2).
func (s *Service) deleteHalfSandbox(ctx context.Context, l *Lease) {
	if l.SandboxID == "" {
		return
	}
	if err := s.sub.Delete(context.WithoutCancel(ctx), l.SandboxID); err != nil {
		s.log.Printf("undrain: resume %s cleanup: %v", l.ID, err)
	}
	s.deleteSandboxRow(l.SandboxID)
}

// undrain waits (up to 120 s) for the orchestrator to answer NodeInfo,
// clears the draining state, then resumes exactly the drained leases,
// UNDRAIN_CONCURRENCY (default 2) at a time. A resume that fails with a
// retryable envd/start error is retried UNDRAIN_RESUME_RETRIES (default
// 2) times with a short backoff before the lease becomes lost; an
// admission refusal, a capacity answer or a bounded context keeps the
// lease drained for a later undrain.
//
// The undrain serialisation is one-way (spoond-52c R4): the self-heal
// loop defers to an undrain (undraining), but an admin undrain runs even
// while a heal pass holds the flag, so a caller that wants the undrain
// now gets it; a lease both reach is protected by its busy flag.
func (s *Service) undrain(ctx context.Context) undrainResult {
	res := undrainResult{Failed: []drainFailure{}}
	s.undraining.Store(true)
	defer s.undraining.Store(false)

	deadline := time.Now().Add(120 * time.Second)
	for {
		_, err := s.sub.NodeInfo(ctx)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			s.log.Printf("undrain: node info still failing after 120s: %v", err)
			break
		}
		select {
		case <-ctx.Done():
			// The caller went away: change nothing, the node stays
			// draining and a retry can pick it up.
			s.log.Printf("undrain: cancelled before the node answered: %v", ctx.Err())
			return res
		case <-time.After(time.Second):
		}
	}

	// The check-and-clear runs under the write side of drainGate, so a
	// heal pass that decided to clear the drain cannot concurrently resume
	// leases the admin undrain is also resuming (spoond-52c S1).
	s.drainGate.Lock()
	if err := s.sub.SetDraining(ctx, false); err != nil {
		s.drainGate.Unlock()
		// A stuck node drain is spoond's own drain state to heal: keep the
		// flags so the self-heal loop retries the clear with backoff even
		// when no drained lease remains (spoond-52c R1). The node still
		// refuses creates, so leave the leases drained too — resuming now
		// would only defer every one of them.
		s.drainClearPending.Store(true)
		s.log.Printf("undrain: clear draining failed; keeping the drain for the self-heal loop: %v", err)
		return res
	}
	s.draining.Store(false)
	s.drainClearPending.Store(false)
	s.drainStartedAt.Store(0)
	s.drainGate.Unlock()

	s.store.mu.Lock()
	var targets []*Lease
	for _, l := range s.store.leases {
		if l.Drained && !l.released {
			targets = append(targets, l)
		}
	}
	s.store.mu.Unlock()

	var mu sync.Mutex
	var wg sync.WaitGroup
	// A width of 0 means unlimited; otherwise a slot bounds how many
	// large snapshot resumes run at once.
	var sem chan struct{}
	if width := s.undrainConcurrency(); width > 0 {
		sem = make(chan struct{}, width)
	}
	acquire := func() {
		if sem != nil {
			sem <- struct{}{}
		}
	}
	release := func() {
		if sem != nil {
			<-sem
		}
	}
	for _, l := range targets {
		wg.Add(1)
		acquire()
		go func(l *Lease) {
			defer wg.Done()
			defer release()
			err, attempts, deferred := s.drainResumeOutcome(ctx, l)
			if err == nil {
				mu.Lock()
				res.Resumed++
				mu.Unlock()
				if attempts > 1 {
					s.log.Printf("undrain: resume %s succeeded on attempt %d", l.ID, attempts)
				}
				return
			}
			if errors.Is(err, errLeaseReleased) {
				// The lease was released while its resume started: nothing
				// to resume and nothing failed (spoond-775).
				return
			}
			mu.Lock()
			res.Failed = append(res.Failed, drainFailure{ID: l.ID, Error: err.Error(), Attempts: attempts})
			mu.Unlock()
			if deferred {
				s.emitLeaseEvent(l.ID, l.Owner, LeaseDrainDeferred, fmt.Sprintf("after %d attempt(s): %v", attempts, err))
				s.log.Printf("undrain: resume %s deferred after %d attempt(s): %v", l.ID, attempts, err)
			}
		}(l)
	}
	wg.Wait()
	return res
}

// drainResumeOutcome runs one drained lease through resumeDrainedLease and
// applies the result, the shared tail of the admin undrain and the drain
// self-heal loop. On success it clears the lease's Drained flag (under the
// store lock, unless the lease was released meanwhile — S4). A transient
// refusal (admission, capacity, busy, draining, a bounded context) leaves
// Drained set and returns deferred=true; a permanent failure loses the
// lease, emitting its lost event and dropping its heal backoff. It is safe
// to call concurrently for different leases.
func (s *Service) drainResumeOutcome(ctx context.Context, l *Lease) (error, int, bool) {
	err, attempts := s.resumeDrainedLease(ctx, l)
	if err == nil {
		s.clearDrainHeal(l.ID)
		s.store.mu.Lock()
		if !l.released {
			l.Drained = false
			s.saveLeaseLocked(l)
		}
		s.store.mu.Unlock()
		return nil, attempts, false
	}
	if undrainDeferred(err) || ctx.Err() != nil {
		// A failed or cut-off resume can leave a half-started sandbox
		// behind; resumeLease's createSandbox error path already deletes it
		// inside the busy window, before a retry can reuse the id
		// (spoond-52c S2/NIT). Only the lease's row needs saving here.
		s.store.mu.Lock()
		if !l.released {
			s.saveLeaseLocked(l)
		}
		s.store.mu.Unlock()
		return err, attempts, true
	}
	reason := fmt.Sprintf("drain resume failed after %d attempt(s): %v", attempts, err)
	s.clearDrainHeal(l.ID)
	s.store.mu.Lock()
	if l.released || l.State == "lost" {
		// A lease released or already lost while the resume was in flight
		// is not lost again: clear its Drained flag if it still carries it
		// and save nothing else, so no second lost event follows
		// (spoond-775 class, spoond-52c NIT).
		if !l.released && l.Drained {
			l.Drained = false
			s.saveLeaseLocked(l)
		}
		s.store.mu.Unlock()
		return err, attempts, false
	}
	setLostReason(l, reason)
	l.setState("lost")
	l.Drained = false
	s.saveLeaseLocked(l)
	s.store.mu.Unlock()
	s.emitLeaseEvent(l.ID, l.Owner, LeaseLost, reason)
	// A lease started from a named snapshot no longer protects it once
	// lost (#83 S5).
	s.rerunSnapshotRetention(ctx, l)
	s.log.Printf("undrain: resume %s failed after %d attempt(s): %v", l.ID, attempts, err)
	return err, attempts, false
}

// startDrainHealLoop starts the drain self-heal loop (spoond-52c H3):
// every drainHealInterval it undrains a drain that outlived
// DRAIN_MAX_SECS while the node is healthy, and resumes any lease that
// is still Drained (an undrain that failed, or a drain whose undrain
// never arrived). It runs until ctx ends.
func (s *Service) startDrainHealLoop(ctx context.Context) {
	t := time.NewTicker(drainHealInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// The pass runs under the sweep bound, so one wedged substrate
			// call cannot wedge the self-heal loop (spoond-52c NIT).
			s.runSweepStage(ctx, "healDrain", s.healDrain)
		}
	}
}

// drainMaxSecs is the effective DRAIN_MAX_SECS. DrainMaxSecs <= 0 means
// the default; a negative configured value disables the automatic
// undrain for tests that drive the loops by hand.
func (s *Service) drainMaxSecs() int {
	if s.cfg.DrainMaxSecs < 0 {
		return 0
	}
	if s.cfg.DrainMaxSecs == 0 {
		return DefaultDrainMaxSecs
	}
	return s.cfg.DrainMaxSecs
}

// drainHealState is the self-heal backoff of one lease whose resume was
// deferred: when the next attempt may run, the doubling backoff, the
// cause last seen (so drain_deferred is emitted only on a change) and
// when spoond first started deferring it (the DRAIN_RESUME_MAX_AGE
// bound).
type drainHealState struct {
	firstAt time.Time
	nextAt  time.Time
	backoff time.Duration
	cause   string
	tries   int
	// gaveUp is set once the DRAIN_RESUME_MAX_AGE bound is spent: the
	// heal stops retrying this lease and does not emit again, so a pass
	// after the bound is a no-op. An admin undrain still retries the
	// lease (it goes through undrain, not this state).
	gaveUp bool
}

// drainHealFor returns the heal backoff state of a lease, creating it on
// first sight.
func (s *Service) drainHealFor(id string) *drainHealState {
	s.drainHealMu.Lock()
	defer s.drainHealMu.Unlock()
	h := s.drainHeal[id]
	if h == nil {
		h = &drainHealState{firstAt: s.now()}
		s.drainHeal[id] = h
	}
	return h
}

// clearDrainHeal forgets a lease's self-heal backoff after a successful or
// terminal resume, so a later deferral starts fresh.
func (s *Service) clearDrainHeal(id string) {
	s.drainHealMu.Lock()
	delete(s.drainHeal, id)
	s.drainHealMu.Unlock()
}

// pruneDrainHeal drops the heal backoff state of every lease that is not
// one of the current drained targets (kept is the set of lease ids the
// pass is about to consider). Without it an entry survives the lease
// being resumed, restored or released elsewhere and makes a later
// planned restart's deferral skip silently or give up on the first pass
// (spoond-52c B3).
func (s *Service) pruneDrainHeal(kept []*Lease) {
	live := make(map[string]struct{}, len(kept))
	for _, l := range kept {
		live[l.ID] = struct{}{}
	}
	s.drainHealMu.Lock()
	for id := range s.drainHeal {
		if _, ok := live[id]; !ok {
			delete(s.drainHeal, id)
		}
	}
	s.drainHealMu.Unlock()
}

// drainHealStatus snapshots a state's gaveUp flag and firstAt under the
// mutex, so the heal pass reads them consistently while another goroutine
// may clear the entry.
func (s *Service) drainHealStatus(h *drainHealState) (gaveUp bool, firstAt time.Time) {
	s.drainHealMu.Lock()
	defer s.drainHealMu.Unlock()
	return h.gaveUp, h.firstAt
}

// markDrainHealGaveUp marks a state as given up under the mutex.
func (s *Service) markDrainHealGaveUp(h *drainHealState) {
	s.drainHealMu.Lock()
	h.gaveUp = true
	s.drainHealMu.Unlock()
}

// setDrainHealCause records the last deferral cause and reports whether it
// changed from the previous one (the first deferral has no previous
// cause, so it reports true). It is the state the drain_deferred event is
// gated on (spoond-52c B2).
func (s *Service) setDrainHealCause(h *drainHealState, cause string) bool {
	s.drainHealMu.Lock()
	defer s.drainHealMu.Unlock()
	changed := h.cause != cause
	h.cause = cause
	return changed
}

// healDue reports whether a lease's next self-heal resume attempt is due
// and, when it is, consumes the slot (advancing the backoff) so one pass
// does not retry the same lease twice. It returns the attempt number and
// the backoff now in effect (for the log line). It is false while the
// lease is inside its backoff window.
func (s *Service) healDue(h *drainHealState) (bool, int, time.Duration) {
	s.drainHealMu.Lock()
	defer s.drainHealMu.Unlock()
	now := s.now()
	if !h.nextAt.IsZero() && now.Before(h.nextAt) {
		return false, h.tries, h.backoff
	}
	h.tries++
	if h.backoff == 0 {
		h.backoff = drainHealBackoffMin
	} else {
		h.backoff *= 2
		if h.backoff > drainHealBackoffMax {
			h.backoff = drainHealBackoffMax
		}
	}
	h.nextAt = now.Add(h.backoff)
	return true, h.tries, h.backoff
}

// drainNodeInfoLogDue rate-limits the self-heal loop's "node info" log
// while the orchestrator is down: at most once per drainHealBackoffMax
// rather than every pass (spoond-52c NIT).
func (s *Service) drainNodeInfoLogDue() bool {
	s.drainHealMu.Lock()
	defer s.drainHealMu.Unlock()
	now := s.now()
	if !s.drainNodeInfoLogAt.IsZero() && now.Sub(s.drainNodeInfoLogAt) < drainHealBackoffMax {
		return false
	}
	s.drainNodeInfoLogAt = now
	return true
}

// drainResumeMaxAge is the effective DRAIN_RESUME_MAX_AGE. A negative
// configured value disables the bound for tests that drive the loop by
// hand; 0 means the default.
func (s *Service) drainResumeMaxAge() time.Duration {
	if s.cfg.DrainResumeMaxAge < 0 {
		return 0
	}
	if s.cfg.DrainResumeMaxAge == 0 {
		return DefaultDrainResumeMaxAge
	}
	return s.cfg.DrainResumeMaxAge
}

// healDrain is one self-heal pass. It first decides whether a stale drain
// must be lifted: draining for longer than DRAIN_MAX_SECS while the node
// reports healthy means the undrain never came (the hook timed out, the
// unit did not restart), and a node that refuses every create forever is
// exactly what the owner principle forbids. The automatic undrain logs
// and emits its own event. Then it resumes leases still Drained, each on
// its own backoff, so a lease whose undrain was deferred or failed is
// brought back with the same bounded retries an admin undrain uses, and a
// permanently deferred lease is given up on after DRAIN_RESUME_MAX_AGE
// rather than retried every pass for ever (spoond-52c B2). A failed
// node-drain clear is retried here too, whether or not a drained lease
// remains, and the clear emits its event when it finally succeeds (R1).
func (s *Service) healDrain(ctx context.Context) {
	if s.undraining.Load() {
		return
	}
	// With nothing draining, no drain clear pending and no drained lease
	// there is nothing to do: skip the substrate call entirely.
	if !s.draining.Load() && !s.drainClearPending.Load() && !s.hasDrainedLeases() {
		return
	}
	// Resolve the node once, up front: the self-heal loop never waits on
	// an unreachable node for 120 s (the admin undrain does) and must not
	// lose leases to transport errors while the node is down. An
	// unreachable node leaves every drain and Drained lease exactly as it
	// is, for the next pass.
	info, err := s.sub.NodeInfo(ctx)
	if err != nil {
		// A down orchestrator must not log every 15 s; the rate limiter
		// keeps it to at most one line per drainHealBackoffMax
		// (spoond-52c NIT).
		if s.drainNodeInfoLogDue() {
			s.log.Printf("drain self-heal: node info: %v", err)
		}
		return
	}
	healthy := info.Status == "healthy"
	reachable := healthy || info.Status == "draining"

	// spoond's own drain, or a drain clear a failed undrain left pending:
	// clear the node's drain, either because our drain has outlived
	// DRAIN_MAX_SECS (H3: a node that refuses every create for ever is what
	// the owner principle forbids) or because the clear is pending (R1:
	// retried every pass, whether or not a drained lease remains). A
	// pending clear retries while the node answers at all (its 'draining'
	// is spoond's own SetDraining, which the failed clear left in place);
	// drainStale accepts a node reporting 'draining' for the same reason.
	if s.draining.Load() || s.drainClearPending.Load() {
		clearNow := s.drainClearPending.Load() && reachable
		if !clearNow {
			clearNow = s.drainStale(info)
		}
		if clearNow {
			started := time.Unix(0, s.drainStartedAt.Load())
			s.drainGate.Lock()
			if err := s.sub.SetDraining(ctx, false); err != nil {
				s.drainGate.Unlock()
				s.log.Printf("drain self-heal: clear node draining retry: %v", err)
				return
			}
			s.drainClearPending.Store(false)
			s.draining.Store(false)
			s.drainStartedAt.Store(0)
			s.drainGate.Unlock()
			// The node accepted the clear, so it accepts work again; a
			// lease still Drained resumes in this same pass rather than
			// waiting a tick.
			healthy = true
			lasted := s.now().Sub(started).Round(time.Second)
			s.log.Printf("drain self-heal: node draining cleared after %s", lasted)
			s.emitLeaseEvent("", "", LeaseDrainHealed, fmt.Sprintf("drain lasted %s", lasted))
		}
	}

	// A lease still Drained can only resume once the node is healthy and
	// spoond itself is no longer draining (B1: if spoond is not draining
	// but the node reports 'draining', it is someone else's planned stop -
	// do not fight it).
	if !healthy || s.draining.Load() || s.drainClearPending.Load() {
		return
	}

	// Resume each still-drained lease on its own backoff.
	s.store.mu.Lock()
	var targets []*Lease
	for _, l := range s.store.leases {
		if l.Drained && !l.released {
			targets = append(targets, l)
		}
	}
	s.store.mu.Unlock()
	// Drop the backoff state of leases that are no longer drained targets:
	// an owner resume, restore or release clears them elsewhere, but a
	// lease lost or given up by another path must not leave a stale
	// entry that silently skips a later planned restart (spoond-52c B3).
	s.pruneDrainHeal(targets)
	for _, l := range targets {
		h := s.drainHealFor(l.ID)
		gaveUp, firstAt := s.drainHealStatus(h)
		maxAge := s.drainResumeMaxAge()
		if maxAge > 0 && !gaveUp && s.now().Sub(firstAt) >= maxAge {
			// The bound is spent: stop retrying, keep the lease suspended
			// (its snapshot is intact - not lost) and tell the owner
			// (spoond-52c B2). Mark the state so the log and event fire
			// once, not every pass; an admin undrain can still retry it.
			s.markDrainHealGaveUp(h)
			reason := fmt.Sprintf("resume deferred for over %s; leaving the lease suspended for the owner", maxAge)
			s.log.Printf("drain self-heal: giving up on %s after %s: %s", l.ID, s.now().Sub(firstAt).Round(time.Second), reason)
			s.emitLeaseEvent(l.ID, l.Owner, LeaseDrainGaveUp, reason)
			continue
		}
		if gaveUp {
			continue
		}
		due, tries, backoff := s.healDue(h)
		if !due {
			continue
		}
		// Hold the read side of drainGate across the resume, as
		// recoverDeadRootfs does, so an admin drain cannot take the write
		// side and set draining while a heal-started Create is in flight
		// (spoond-52c S1). resumeDrainedLease re-checks draining before
		// each attempt for the gap after the flag is set.
		s.drainGate.RLock()
		err, attempts, deferred := s.drainResumeOutcome(ctx, l)
		s.drainGate.RUnlock()
		if err == nil {
			s.log.Printf("drain self-heal: resumed %s", l.ID)
			continue
		}
		if !deferred {
			continue // drainResumeOutcome logged and emitted the loss
		}
		if s.setDrainHealCause(h, err.Error()) {
			// Only on a state change (first deferral, cause change): a
			// permanently deferred lease does not emit a drain_deferred
			// per lease per pass (spoond-52c B2).
			s.emitLeaseEvent(l.ID, l.Owner, LeaseDrainDeferred, fmt.Sprintf("after %d attempt(s): %v", attempts, err))
		}
		s.log.Printf("drain self-heal: resume %s deferred (attempt %d, next in %s): %v", l.ID, tries, backoff, err)
	}
}

// adoptDrainState reconciles spoond's own drain bookkeeping with the
// node at Start: a node already reporting 'draining' was drained by a
// previous process (a backend restart between drain and undrain), so
// spoond adopts the drain with a fresh DRAIN_MAX_SECS clock instead of
// believing it is healthy and undraining mid-planned-stop (spoond-52c
// B1).
func (s *Service) adoptDrainState(ctx context.Context) {
	info, err := s.sub.NodeInfo(ctx)
	if err != nil {
		s.log.Printf("drain adopt: node info: %v", err)
		return
	}
	if info.Status != "draining" {
		return
	}
	s.draining.Store(true)
	s.drainStartedAt.Store(s.now().UnixNano())
	s.log.Printf("drain adopt: node reports draining; adopting it (DRAIN_MAX_SECS bounds the undrain)")
}

// hasDrainedLeases reports whether any live lease still carries the
// Drained flag (an undrain that deferred or failed).
func (s *Service) hasDrainedLeases() bool {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	for _, l := range s.store.leases {
		if l.Drained && !l.released {
			return true
		}
	}
	return false
}

// drainStale reports whether the current drain has outlived
// DRAIN_MAX_SECS, given an already-fetched healthy node. A disabled
// limit (0) or an absent start time answers false.
func (s *Service) drainStale(info substrate.NodeInfo) bool {
	max := s.drainMaxSecs()
	if max <= 0 {
		return false
	}
	started := s.drainStartedAt.Load()
	if started == 0 {
		return false
	}
	if s.now().Sub(time.Unix(0, started)) <= time.Duration(max)*time.Second {
		return false
	}
	return info.Status == "healthy" || info.Status == "draining"
}
