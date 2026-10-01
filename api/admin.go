package api

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
	"sync"
	"time"
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
type drainFailure struct {
	ID    string `json:"id"`
	Error string `json:"error"`
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
	res, err := s.svc.drain(r.Context())
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
	writeJSON(w, http.StatusOK, s.svc.undrain(r.Context()))
}

// drainConcurrency bounds the concurrent pauses and resumes of the
// drain and undrain.
const drainConcurrency = 4

// drain pauses every live lease (persistent or not) into a pause build
// and marks it Drained, deletes the warm pool, then waits until the node
// reports no running sandboxes and no outstanding work. On a
// SetDraining failure nothing is changed and the error is returned (the
// handler answers 503 and s.draining stays false). Per-lease failures
// are recorded and the drain continues.
func (s *Service) drain(ctx context.Context) (drainResult, error) {
	if err := s.sub.SetDraining(ctx, true); err != nil {
		return drainResult{}, err
	}
	s.draining.Store(true)
	res := drainResult{Failed: []drainFailure{}}

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

// undrain waits (up to 120 s) for the orchestrator to answer NodeInfo,
// clears the draining state, then resumes exactly the drained leases,
// up to 4 at a time. A lease that fails to resume becomes lost.
func (s *Service) undrain(ctx context.Context) undrainResult {
	res := undrainResult{Failed: []drainFailure{}}

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
	sem := make(chan struct{}, drainConcurrency)
	for _, l := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(l *Lease) {
			defer wg.Done()
			defer func() { <-sem }()
			if _, err := s.resumeLease(ctx, l); err != nil {
				s.store.mu.Lock()
				l.State = "lost"
				l.Drained = false
				s.saveLeaseLocked(l)
				s.store.mu.Unlock()
				mu.Lock()
				res.Failed = append(res.Failed, drainFailure{ID: l.ID, Error: err.Error()})
				mu.Unlock()
				s.log.Printf("undrain: resume %s failed: %v", l.ID, err)
				return
			}
			s.store.mu.Lock()
			l.Drained = false
			s.saveLeaseLocked(l)
			s.store.mu.Unlock()
			mu.Lock()
			res.Resumed++
			mu.Unlock()
		}(l)
	}
	wg.Wait()
	return res
}
