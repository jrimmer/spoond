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

// Restore in place (2.3, #121): POST /api/leases/{id}/restore replaces
// the lease's sandbox with one from one of the lease's own kept
// checkpoints — the guest rolls back to that snapshot while the lease
// keeps its id, holder, name, network policy and exposed ports. Unlike
// a cold restart, the source build is a checkpoint the owner pinned, so
// the lease's work since it is what gets thrown away.

// restoreableBuild reports whether the lease may restore to the build:
// it must be one of the lease's own checkpoints (its newest checkpoint,
// or any checkpoint it pinned with keep) or one of its kept builds. A
// pause build, another lease's checkpoint or another owner's build is
// not restorable.
func (s *Service) restoreableBuild(ctx context.Context, l *Lease, buildID string) (store.BuildRow, bool) {
	if buildID == "" {
		return store.BuildRow{}, false
	}
	b, err := s.db.GetBuild(ctx, buildID)
	if err != nil {
		return store.BuildRow{}, false
	}
	if b.Owner != l.Owner || b.State == "deleted" {
		return store.BuildRow{}, false
	}
	// The lease's own newest checkpoint is always restorable.
	if b.BuildID == l.LastCheckpointBuildID {
		return b, true
	}
	// Older checkpoints only as kept builds: unpinned snapshots can be
	// GC'd at any pass, so restoring to one is a race the owner should
	// not rely on — pin it first.
	kept, err := s.db.LeaseKeepsBuild(ctx, l.ID, buildID)
	if err != nil || !kept {
		return store.BuildRow{}, false
	}
	return b, true
}

// restore replaces the lease's sandbox with one from the given build,
// keeping the id, holder, name, policy, exposed ports and
// checkpoint_interval. Modeled on recoverFromCheckpoint: the guest's
// memory does not continue from where its processes left it, so the
// generation bumps and the guest file is rewritten; the create-time
// secrets are re-written into the fresh sandbox; the lease comes back
// running. Callers own the busy window.
func (s *Service) restore(ctx context.Context, l *Lease, b store.BuildRow) error {
	img, err := s.db.GetImage(ctx, l.Image)
	if err != nil {
		return err
	}
	if l.Suspended {
		// A suspended lease holds no hugepages, so its charge was freed
		// at suspend; restoring it brings a running sandbox back, and
		// that sandbox runs the image's current memory_mb — the charge
		// to re-admit before the sandbox is created (#128). A restore
		// adds no lease, so only the memory cap applies. (A running
		// restore is already charged, and with its own charge — no new
		// admission.)
		if err := s.reserveQuota(l.Owner, 1, img.MemoryMB, false); err != nil {
			return err
		}
		defer func() { s.releaseQuotaReservation(l.Owner, 1, img.MemoryMB) }()
		// Class re-admission (#128 part 2), as for a resume: a
		// demand-burst lease stays burst and re-passes the reserve; a
		// guarantee-burst one may fall back to guaranteed.
		class, err := s.admitClass(ctx, l.Owner, img.MemoryMB, l.Burst, l.ID)
		if err != nil {
			return err
		}
		l.Class = class
	}
	// The fresh sandbox exists before the old one goes (as restartCold):
	// a failed create leaves the lease exactly as it was.
	//
	// resume=false with a fresh sandbox id (unlike recoverFromCheckpoint,
	// which resumes the same sandbox id) follows the fork shape: per A2
	// §3.6 the boot path is chosen by the build's snapshot metadata, so a
	// checkpoint build still boots from its memory snapshot; a fresh id
	// avoids resurrecting the same execution on the substrate.
	sb, err := s.createSandbox(ctx, img, b, false, "", l)
	if err != nil {
		return err
	}
	// A release that landed while the restore create ran must not be
	// undone by the save below (spoond-775, spoond-63a).
	if s.leaseReleased(l) {
		s.log.Printf("restore: lease %s was released during its restore; stopping sandbox %s", l.ID, sb.ID)
		s.deleteSandboxWithRetries(sb.ID, l.ID, "released")
		s.deleteSandboxRow(sb.ID)
		s.endCreatingSandbox(sb.ID)
		return errLeaseReleased
	}
	if old := l.SandboxID; old != "" && old != sb.ID {
		_ = s.sub.Delete(ctx, old)
		s.deleteSandboxRow(old)
	}
	s.store.mu.Lock()
	if l.released {
		s.store.mu.Unlock()
		s.deleteSandboxWithRetries(sb.ID, l.ID, "released")
		s.deleteSandboxRow(sb.ID)
		s.endCreatingSandbox(sb.ID)
		return errLeaseReleased
	}
	l.SandboxID = sb.ID
	l.HostIP = sb.HostIP
	l.ExposedIP = sb.HostIP
	l.BuildID = b.BuildID
	l.setState("running")
	l.Suspended = false
	// The restored sandbox runs the image's current memory_mb: the lease
	// keeps the charge it was admitted with (#128).
	l.MemoryMB = img.MemoryMB
	// A drained lease paused into its pause build; the restore replaced
	// that sandbox with a running one, so the flag would linger and
	// undrain would later try to resume a running lease. Clear it.
	l.Drained = false
	// The pause builds stop being the lease's resume point: the next
	// suspend writes a fresh one (the restored guest is not what the old
	// pause builds snapshotted).
	l.ResumeBuildID = ""
	l.LastActive = time.Now()
	s.bumpGenerationLocked(l)
	s.saveLeaseLocked(l)
	s.store.mu.Unlock()
	s.endCreatingSandbox(sb.ID)
	s.writeGeneration(l)
	// The restored sandbox has no crash-recovery budget (spoond-dxq B2).
	s.clearRecoveryRetries(l)
	// The restored guest does not continue the memory the jobs ran in:
	// every running job is lost (2.6, #135).
	s.markLeaseJobsLost(ctx, l.ID, l.Owner, "lease restored to a checkpoint; the job did not survive")
	// A fresh sandbox never had the lease's secrets: re-write them
	// (create-time only; exec-time secrets ride their request) (#80).
	s.restageCreateSecrets(ctx, l, "restore")
	if len(l.ExposePorts) > 0 {
		s.refreshPeersAsync(ctx)
	}
	s.emitLeaseEvent(l.ID, l.Owner, LeaseRestored, "restored from build "+b.BuildID)
	return nil
}

// restoreBusy runs restore with the busy guard: a second operation on a
// busy lease returns errLeaseBusy.
func (s *Service) restoreBusy(ctx context.Context, l *Lease, b store.BuildRow) error {
	s.store.mu.Lock()
	if l.busy {
		s.store.mu.Unlock()
		return errLeaseBusy
	}
	l.busy = true
	s.store.mu.Unlock()
	defer s.endBusy(l)
	return s.restore(ctx, l, b)
}

// handleRestore restores a lease in place to one of its own kept
// checkpoints (2.3, #121). Owner or admin, others 404; a live or
// suspended lease (a lost lease answers 410 lease_lost like every other
// route — restore must not resurrect it); the build must be one of the
// lease's own checkpoints or kept builds (else 404); 409 while busy.
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	lease := s.svc.lookup(owner, id)
	if lease == nil && isAdmin(r) {
		lease = s.svc.lookupAny(id)
	}
	if lease == nil {
		writeError(w, http.StatusNotFound, "lease not found")
		return
	}
	if !s.ensureLive(w, lease) {
		return
	}
	var req struct {
		BuildID string `json:"build_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	b, ok := s.svc.restoreableBuild(r.Context(), lease, req.BuildID)
	if !ok {
		writeError(w, http.StatusNotFound, "snapshot not found")
		return
	}
	if err := s.svc.restoreBusy(r.Context(), lease, b); err != nil {
		switch {
		case errors.Is(err, errLeaseBusy):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, errLeaseReleased):
			writeError(w, http.StatusNotFound, "lease not found")
		case errors.Is(err, errPreemptCannot):
			// A guaranteed lease that could not preempt (#128 part 3):
			// the snapshot disk is too full to pause a burst lease.
			writeErrorAfter(w, http.StatusServiceUnavailable, burstRetryAfterSecs, "capacity: "+err.Error())
		case errors.Is(err, errBurstReserve):
			// A burst lease restored into a full reserve (#128 part 2):
			// 503 with a retry hint, the lease stays as it was.
			writeErrorAfter(w, http.StatusServiceUnavailable, burstRetryAfterSecs, err.Error())
		case errors.Is(err, substrate.ErrCapacity):
			writeError(w, http.StatusServiceUnavailable, "capacity: "+err.Error())
		case errors.Is(err, errQuotaExceeded):
			// Restoring a suspended lease brings a running sandbox (and
			// its hugepages) back, so it re-passes the memory check
			// (#128): over max_mib answers 429 and the lease stays as it
			// was.
			writeError(w, http.StatusTooManyRequests, err.Error())
		default:
			s.svc.log.Printf("restore %s: %v", id, err)
			writeError(w, http.StatusInternalServerError, "restore failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         lease.ID,
		"build_id":   b.BuildID,
		"generation": lease.Generation,
		"status":     "running",
		// When the restored checkpoint was taken, for clients' logs.
		"build_created_at": formatRFC3339(b.CreatedAt),
	})
}
