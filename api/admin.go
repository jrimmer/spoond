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

	"github.com/jrimmer/spoond/v2/store"
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
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, substrate.ErrNotFound) || errors.Is(err, errNotFound) {
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
// returned at once: it keeps the lease drained for a later undrain.
func (s *Service) resumeDrainedLease(ctx context.Context, l *Lease) (error, int) {
	maxAttempts := 1 + s.undrainResumeRetries()
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
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
		// A failed resume can leave a half-started sandbox behind; remove
		// it (best effort) so the retry can reuse the same sandbox id.
		if dErr := s.sub.Delete(context.WithoutCancel(ctx), l.SandboxID); dErr != nil {
			s.log.Printf("undrain: resume %s cleanup before retry: %v", l.ID, dErr)
		}
		select {
		case <-ctx.Done():
			return err, attempt
		case <-time.After(undrainRetryBackoff):
		}
	}
	return err, maxAttempts
}

// undrain waits (up to 120 s) for the orchestrator to answer NodeInfo,
// clears the draining state, then resumes exactly the drained leases,
// UNDRAIN_CONCURRENCY (default 2) at a time. A resume that fails with a
// retryable envd/start error is retried UNDRAIN_RESUME_RETRIES (default
// 2) times with a short backoff before the lease becomes lost; an
// admission refusal, a capacity answer or a bounded context keeps the
// lease drained for a later undrain.
func (s *Service) undrain(ctx context.Context) undrainResult {
	res := undrainResult{Failed: []drainFailure{}}
	// Serialise against the self-heal loop: two undrains resuming the
	// same drained leases would double the resume load and race their
	// Drained flags.
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

	if err := s.sub.SetDraining(ctx, false); err != nil {
		s.log.Printf("undrain: clear draining: %v", err)
	}
	s.draining.Store(false)
	s.drainStartedAt.Store(0)

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
			err, attempts := s.resumeDrainedLease(ctx, l)
			if err == nil {
				s.store.mu.Lock()
				l.Drained = false
				s.saveLeaseLocked(l)
				s.store.mu.Unlock()
				mu.Lock()
				res.Resumed++
				mu.Unlock()
				if attempts > 1 {
					s.log.Printf("undrain: resume %s succeeded on attempt %d", l.ID, attempts)
				}
				return
			}
			if undrainDeferred(err) || ctx.Err() != nil {
				// Over the owner's memory cap, a burst lease that would dip
				// the node under its burst reserve (#128 part 2), a
				// guaranteed lease that could not preempt for room (#128
				// part 3), a capacity answer or a cancelled/bounded context:
				// each is a transient refusal, so the lease keeps its Drained
				// flag and a later undrain (or the self-heal loop) retries
				// it — refusing a resume must not lose the lease the way a
				// failed resume (a sandbox that would not come back) does.
				s.store.mu.Lock()
				s.saveLeaseLocked(l)
				s.store.mu.Unlock()
				mu.Lock()
				res.Failed = append(res.Failed, drainFailure{ID: l.ID, Error: err.Error(), Attempts: attempts})
				mu.Unlock()
				s.emitLeaseEvent(l.ID, l.Owner, LeaseDrainDeferred, fmt.Sprintf("after %d attempt(s): %v", attempts, err))
				s.log.Printf("undrain: resume %s deferred after %d attempt(s): %v", l.ID, attempts, err)
				return
			}
			s.store.mu.Lock()
			l.setState("lost")
			l.Drained = false
			s.saveLeaseLocked(l)
			s.store.mu.Unlock()
			s.emitLeaseEvent(l.ID, l.Owner, LeaseLost, fmt.Sprintf("undrain resume failed after %d attempt(s): %v", attempts, err))
			// A lease started from a named snapshot no longer protects it
			// once lost (#83 S5).
			s.rerunSnapshotRetention(ctx, l)
			mu.Lock()
			res.Failed = append(res.Failed, drainFailure{ID: l.ID, Error: err.Error(), Attempts: attempts})
			mu.Unlock()
			s.log.Printf("undrain: resume %s failed after %d attempt(s): %v", l.ID, attempts, err)
		}(l)
	}
	wg.Wait()
	return res
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
			s.healDrain(ctx)
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

// healDrain is one self-heal pass. It first decides whether a stale
// drain must be lifted: draining for longer than DRAIN_MAX_SECS while
// the node reports healthy means the undrain never came (the hook timed
// out, the unit did not restart), and a node that refuses every create
// forever is exactly what the owner principle forbids. The automatic
// undrain logs and emits its own event. Then it resumes every lease
// still Drained, so a lease whose undrain was deferred or failed is
// brought back with the same bounded retries an admin undrain uses.
func (s *Service) healDrain(ctx context.Context) {
	if s.undraining.Load() {
		return
	}
	// With nothing draining and no drained lease there is nothing to do:
	// skip the substrate call entirely.
	if !s.draining.Load() && !s.hasDrainedLeases() {
		return
	}
	// Resolve the node once, up front: the self-heal loop never waits on
	// an unreachable node for 120 s (the admin undrain does) and must not
	// lose leases to transport errors while the node is down. An
	// unreachable or unhealthy node leaves every drain and Drained lease
	// exactly as it is, for the next pass.
	info, err := s.sub.NodeInfo(ctx)
	if err != nil {
		s.log.Printf("drain self-heal: node info: %v", err)
		return
	}
	healthy := info.Status == "healthy" || info.Status == "draining"
	if s.draining.Load() {
		if !healthy || !s.drainStale(info) {
			return
		}
		started := time.Unix(0, s.drainStartedAt.Load())
		s.log.Printf("drain self-heal: drain has lasted %s (limit %ds) while the node is healthy; undraining",
			time.Since(started).Round(time.Second), s.drainMaxSecs())
		s.emitLeaseEvent("", "", LeaseDrainHealed, fmt.Sprintf("drain lasted %s", time.Since(started).Round(time.Second)))
	}
	if !healthy {
		return
	}
	s.undrain(ctx)
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
	if time.Since(time.Unix(0, started)) <= time.Duration(max)*time.Second {
		return false
	}
	return info.Status == "healthy" || info.Status == "draining"
}
