package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
)

// Periodic and manual checkpoints (U10): persistent leases are
// checkpointed on a timer so a crash loses at most the work since the
// last snapshot, and the owner can checkpoint on demand via
// POST /api/sandboxes/{id}/checkpoint.

// checkpointLoopTick is how often the periodic pass looks for due
// leases (2.3, #122): every minute; a lease's effective interval (a
// minute at the smallest) decides whether it is due.
const checkpointLoopTick = time.Minute

// checkpointIntervalSpacing separates two periodic checkpoints so a
// batch of leases does not produce one burst of snapshot writes.
const checkpointIntervalSpacing = 2 * time.Second

// runCheckpointLoop runs checkpointIdleLeases every minute (2.3,
// #122): the pass itself picks the leases whose effective interval is
// due, so an all-never host does no work at all.
func (s *Service) runCheckpointLoop(ctx context.Context) {
	t := time.NewTicker(checkpointLoopTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.checkpointIdleLeases(ctx)
		}
	}
}

// checkpointIdleLeases checkpoints every live lease whose effective
// checkpoint interval (its own, else the host default; 0 = never) is
// due: interval > 0, last checkpoint older than the interval, and the
// lease has been active since its last checkpoint (a lease that saw no
// activity has nothing new to protect). Persistent leases and held
// leases alike — being held no longer means being checkpointed (2.3,
// #122). One lease at a time, 2 s apart; busy leases are skipped.
func (s *Service) checkpointIdleLeases(ctx context.Context) {
	now := s.now()
	s.store.mu.Lock()
	var targets []*Lease
	for _, l := range s.store.leases {
		if l.released || !l.live() || l.busy {
			continue
		}
		interval := s.effectiveCheckpointInterval(l)
		if interval <= 0 {
			continue
		}
		if !l.LastCheckpointAt.Add(time.Duration(interval) * time.Second).Before(now) {
			continue
		}
		if !l.LastActive.After(l.LastCheckpointAt) {
			continue
		}
		targets = append(targets, l)
	}
	s.store.mu.Unlock()

	for i, l := range targets {
		if i > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(checkpointIntervalSpacing):
			}
		}
		if _, err := s.checkpointLeaseBusy(ctx, l); err != nil {
			if !errors.Is(err, errLeaseBusy) {
				s.log.Printf("checkpoint: lease %s: %v", l.ID, err)
			}
		}
	}
}

// checkpointLeaseBusy runs checkpointLease with the busy guard: a
// second operation on a busy lease returns errLeaseBusy.
func (s *Service) checkpointLeaseBusy(ctx context.Context, l *Lease) (store.BuildRow, error) {
	s.store.mu.Lock()
	if l.busy {
		s.store.mu.Unlock()
		return store.BuildRow{}, errLeaseBusy
	}
	l.busy = true
	s.store.mu.Unlock()
	defer s.endBusy(l)
	return s.checkpointLease(ctx, l)
}

// handleCheckpointPolicy sets the lease's own checkpoint interval
// (2.3, #122). Owner or admin; anyone else gets 404 like every
// owner-scoped route. Body {"checkpoint_interval": N} with the same
// validation as create: 0 (never) or 60..604800 seconds. Emits a
// checkpoint_policy lease event carrying the new effective seconds.
func (s *Server) handleCheckpointPolicy(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	var req struct {
		CheckpointInterval *int64 `json:"checkpoint_interval"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.CheckpointInterval == nil {
		writeError(w, http.StatusBadRequest, "checkpoint_interval is required")
		return
	}
	lease := s.svc.lookup(owner, id)
	if lease == nil && isAdmin(r) {
		lease = s.svc.lookupAny(id)
	}
	if lease == nil {
		writeError(w, http.StatusNotFound, "lease not found")
		return
	}
	if err := validateCheckpointInterval(*req.CheckpointInterval); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := s.svc.setCheckpointPolicy(lease, *req.CheckpointInterval)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to set checkpoint policy")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":                  updated.ID,
		"checkpoint_interval": s.svc.effectiveCheckpointInterval(updated),
		"ok":                  true,
	})
}

// keepBuild pins a checkpoint build of the lease (2.3, #121): the build
// joins the GC's kept set while the lease lives and is a restore point
// for POST /api/leases/{id}/restore. Keeping the same build twice keeps
// the first kept_at.
func (s *Service) keepBuild(ctx context.Context, leaseID, buildID string) error {
	return s.db.KeepBuild(ctx, leaseID, buildID, s.now())
}

// handleCheckpoint checkpoints one lease on demand. Owner only, live
// leases only (409 otherwise, including busy). The optional body
// {"keep":true} pins the checkpoint build: it joins the GC's kept set
// while the lease lives and can be restored in place (2.3, #121).
func (s *Server) handleCheckpoint(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	var req struct {
		Keep bool `json:"keep"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req) // optional body
	}
	lease := s.svc.lookup(owner, id)
	if lease == nil {
		writeError(w, http.StatusNotFound, "lease not found")
		return
	}
	if !lease.live() {
		writeError(w, http.StatusConflict, "lease is not running")
		return
	}
	b, err := s.svc.checkpointLeaseBusy(r.Context(), lease)
	if err != nil {
		switch {
		case errors.Is(err, errLeaseBusy):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, substrate.ErrCapacity):
			writeError(w, http.StatusServiceUnavailable, "capacity: "+err.Error())
		default:
			s.svc.log.Printf("checkpoint %s: %v", id, err)
			writeError(w, http.StatusInternalServerError, "checkpoint failed")
		}
		return
	}
	if req.Keep {
		if err := s.svc.keepBuild(r.Context(), lease.ID, b.BuildID); err != nil {
			s.svc.log.Printf("checkpoint: keep %s: %v", b.BuildID, err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       lease.ID,
		"build_id": b.BuildID,
		"at":       formatRFC3339(lease.LastCheckpointAt),
		"kept":     req.Keep,
	})
}
