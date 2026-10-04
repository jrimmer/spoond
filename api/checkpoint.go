package api

import (
	"context"
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

// checkpointIntervalSpacing separates two periodic checkpoints so a
// batch of leases does not produce one burst of snapshot writes.
const checkpointIntervalSpacing = 2 * time.Second

// runCheckpointLoop checkpoints idle-dirty persistent leases every
// CHECKPOINT_INTERVAL_MINS. An interval of 0 disables the loop.
func (s *Service) runCheckpointLoop(ctx context.Context) {
	if s.cfg.CheckpointEvery <= 0 {
		return
	}
	t := time.NewTicker(s.cfg.CheckpointEvery)
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

// checkpointIdleLeases checkpoints every live lease that has been
// active since its last checkpoint (a lease that saw no activity since
// its last snapshot has nothing new to protect): persistent leases and
// held leases (a non-empty holder puts a plain lease on this pass so
// the holder's work survives a crash). One lease at a time, 2 s apart;
// busy leases are skipped.
func (s *Service) checkpointIdleLeases(ctx context.Context) {
	s.store.mu.Lock()
	var targets []*Lease
	for _, l := range s.store.leases {
		if l.released || !l.live() || l.busy {
			continue
		}
		if !l.Persistent && !l.held() {
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

// handleCheckpoint checkpoints one lease on demand. Owner only, live
// leases only (409 otherwise, including busy).
func (s *Server) handleCheckpoint(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
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
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       lease.ID,
		"build_id": b.BuildID,
		"at":       formatRFC3339(lease.LastCheckpointAt),
	})
}
