package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// DefaultMaxKeptPerLease is the per-lease kept-checkpoint cap (#126)
// when MAX_KEPT_PER_LEASE is unset: four restore points per lease, so a
// keep-happy loop cannot pin the whole catalog. 0 = no cap.
const DefaultMaxKeptPerLease = 4

// keptCapError is returned by checkpointLeaseBusy when a keep would
// push the lease past its kept-checkpoint cap (#126). The checkpoint is
// not taken and nothing is evicted; the API maps it to 409.
type keptCapError struct{ cap int }

func (e *keptCapError) Error() string {
	return fmt.Sprintf("kept checkpoint limit reached (%d per lease); unpin one with DELETE /api/snapshots/{build_id}", e.cap)
}

// checkKeptCap refuses a keep on a lease already holding cap kept
// checkpoints (#126): nothing is evicted, and the caller must unpin one
// (DELETE /api/snapshots/{build_id}) to free a slot. cap 0 = no cap.
// Called inside the busy window, before the checkpoint runs, so a
// rejected keep costs no snapshot write; the busy flag already excludes
// a concurrent keep on the same lease, so the count cannot grow under
// us.
func (s *Service) checkKeptCap(ctx context.Context, l *Lease, cap int) error {
	if cap <= 0 {
		return nil
	}
	n, err := s.db.CountKeptBuilds(ctx, l.ID)
	if err != nil {
		// Unknowable is not over: let the keep through rather than block
		// checkpoints on a catalog hiccup; keepBuild logs its own failures.
		s.log.Printf("checkpoint: count kept builds of %s: %v", l.ID, err)
		return nil
	}
	if n >= cap {
		return &keptCapError{cap: cap}
	}
	return nil
}

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
		if _, err := s.checkpointLeaseBusy(ctx, l, false); err != nil {
			if !errors.Is(err, errLeaseBusy) {
				s.log.Printf("checkpoint: lease %s: %v", l.ID, err)
			}
		}
	}
}

// checkpointLeaseBusy runs checkpointLease with the busy guard: a
// second operation on a busy lease returns errLeaseBusy. With keep set,
// the pin is written inside the busy window, before the guard drops: a
// release racing the checkpoint then deletes the lease's kept rows
// after the insert, not before it (a pin written after the window would
// fail the FK or outlive a finished release).
func (s *Service) checkpointLeaseBusy(ctx context.Context, l *Lease, keep bool) (store.BuildRow, error) {
	s.store.mu.Lock()
	if l.busy {
		s.store.mu.Unlock()
		return store.BuildRow{}, errLeaseBusy
	}
	l.busy = true
	s.store.mu.Unlock()
	defer s.endBusy(l)
	// The per-lease cap is checked inside the busy window, before the
	// checkpoint runs (#126): a lease at the cap answers 409 and takes
	// no snapshot at all.
	if keep {
		if cerr := s.checkKeptCap(ctx, l, s.cfg.MaxKeptPerLease); cerr != nil {
			return store.BuildRow{}, cerr
		}
	}
	b, err := s.checkpointLease(ctx, l)
	if err == nil && keep {
		// The owner's byte budget (#126) runs before the pin: the build's
		// size is only known now that it is written. Over budget, the pin
		// is refused and the error names the build — the checkpoint stays
		// written but unpinned (GC-able), and the route answers 409 with
		// the build_id so the caller can retry without keep.
		var budget int64
		if s.identities != nil {
			if u := s.identities.UserByID(l.Owner); u != nil {
				budget = u.MaxKeptBytes
			}
		}
		if berr := s.enforceKeptBudget(ctx, l.Owner, b.BuildID, budget); berr != nil {
			return b, berr
		}
		if kerr := s.keepBuild(ctx, l.ID, b.BuildID); kerr != nil {
			// The checkpoint stands; only the pin failed.
			s.log.Printf("checkpoint: keep %s: %v", b.BuildID, kerr)
		} else {
			// The pin moved the kept totals (#126): the gauges follow
			// without waiting for the next GC pass.
			s.UpdateKeptMetrics(ctx)
		}
	}
	return b, err
}

// keptBudgetError is returned by checkpointLeaseBusy when a keep would
// push the owner's kept bytes past their budget (#126). The checkpoint
// was written but is NOT pinned; buildID names it so the caller can
// retry without keep. The API maps it to 409 with the build_id in the
// body.
type keptBudgetError struct {
	budget, kept, build int64
	buildID             string
}

func (e *keptBudgetError) Error() string {
	return fmt.Sprintf("kept checkpoint byte budget exceeded (%d + %d would pass %d); the checkpoint %s was written but not kept — retry without keep, or unpin builds with DELETE /api/snapshots/{build_id}",
		e.kept, e.build, e.budget, e.buildID)
}

// keptBytesOfOwner sums size_bytes over the owner's kept, non-deleted
// builds (#126). Rows whose build is gone (the build row deleted under
// the pin, or the pin written for an unknown build) count nothing: a
// pin on thin air holds no disk.
func (s *Service) keptBytesOfOwner(ctx context.Context, owner string) (int64, error) {
	keptBy, err := s.db.ListKeptBuilds(ctx)
	if err != nil {
		return 0, err
	}
	builds, err := s.db.ListBuilds(ctx)
	if err != nil {
		return 0, err
	}
	size := make(map[string]int64, len(builds))
	for _, b := range builds {
		if b.Owner == owner && b.State != "deleted" {
			size[b.BuildID] = b.SizeBytes
		}
	}
	var total int64
	for _, ids := range keptBy {
		for _, id := range ids {
			total += size[id]
		}
	}
	return total, nil
}

// enforceKeptBudget is the owner-side half of the kept caps (#126). The
// checkpoint is already written when this runs (the size is only known
// then): when the fresh build would push the owner's kept bytes past
// maxKeptBytes, the pin is refused — the build stays an ordinary,
// GC-able checkpoint — and a keptBudgetError naming the build comes
// back, so the caller can retry without keep. maxKeptBytes 0 = no
// budget; a caller with no identity row is governed by nothing.
func (s *Service) enforceKeptBudget(ctx context.Context, owner, buildID string, maxKeptBytes int64) error {
	if maxKeptBytes <= 0 {
		return nil
	}
	b, err := s.db.GetBuild(ctx, buildID)
	if err != nil {
		// No row, no size: the pin itself will fail later and log. Let
		// the keep try.
		return nil
	}
	kept, err := s.keptBytesOfOwner(ctx, owner)
	if err != nil {
		// Unknowable is not over: pin anyway rather than leave the keep
		// silently unkept on a catalog hiccup.
		s.log.Printf("checkpoint: kept bytes of %s: %v", owner, err)
		return nil
	}
	if kept+b.SizeBytes > maxKeptBytes {
		return &keptBudgetError{budget: maxKeptBytes, kept: kept, build: b.SizeBytes, buildID: buildID}
	}
	return nil
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
		if writeLeaseLostErr(w, err) {
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to set checkpoint policy")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":                  updated.ID,
		"checkpoint_interval": s.svc.effectiveCheckpointInterval(updated),
		"ok":                  true,
	})
}

// handleIdlePolicy sets the lease's own idle_suspend threshold
// (2.5, #129 part 2). Owner or admin; anyone else gets 404 like every
// owner-scoped route. Body {"idle_suspend": N} with the same validation
// as create: 0 (never) or 60..604800 seconds. A non-zero value needs a
// persistent lease (suspension needs persistence) and answers 400
// otherwise. Emits an idle_policy lease event carrying the new effective
// seconds.
func (s *Server) handleIdlePolicy(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	var req struct {
		IdleSuspend *int64 `json:"idle_suspend"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.IdleSuspend == nil {
		writeError(w, http.StatusBadRequest, "idle_suspend is required")
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
	if err := validateIdleSuspend(*req.IdleSuspend); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if *req.IdleSuspend != 0 && !lease.Persistent {
		writeError(w, http.StatusBadRequest, "idle_suspend needs a persistent lease")
		return
	}
	updated, err := s.svc.setIdlePolicy(lease, *req.IdleSuspend)
	if err != nil {
		if writeLeaseLostErr(w, err) {
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to set idle policy")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":           updated.ID,
		"idle_suspend": s.svc.effectiveIdleSuspend(updated),
		"ok":           true,
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
	keep := false
	if r.Body != nil {
		var req struct {
			Keep bool `json:"keep"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			// An empty body is "keep nothing"; a malformed one is a
			// client error, like every other route.
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		keep = req.Keep
	}
	lease := s.svc.lookup(owner, id)
	if lease == nil {
		writeError(w, http.StatusNotFound, "lease not found")
		return
	}
	if !lease.live() {
		if lease.State == "lost" {
			writeLeaseLost(w, lease)
			return
		}
		writeError(w, http.StatusConflict, "lease is not running")
		return
	}
	b, err := s.svc.checkpointLeaseBusy(r.Context(), lease, keep)
	if err != nil {
		var capErr *keptCapError
		var budgetErr *keptBudgetError
		switch {
		case errors.Is(err, errLeaseBusy):
			writeError(w, http.StatusConflict, err.Error())
		case errors.As(err, &capErr):
			// At the per-lease cap (#126): nothing was taken, nothing
			// evicted; the caller unpins a build to free a slot.
			writeError(w, http.StatusConflict, capErr.Error())
		case errors.As(err, &budgetErr):
			// Over the owner's kept-bytes budget (#126): the checkpoint was
			// written but not pinned, so the caller can retry without
			// keep; the body names the build for that retry.
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":    budgetErr.Error(),
				"build_id": budgetErr.buildID,
				"kept":     false,
			})
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
		"kept":     keep,
	})
}
