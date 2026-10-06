package api

import (
	"context"
	"fmt"
)

// Crash recovery (U10 R16/D4): an orchestrator crash kills every
// running sandbox. reconcileCrash resumes every lease that has a
// checkpoint — with the same sandbox id, from the checkpoint build —
// and marks the rest lost. It runs once at backend start (from
// ReconcileOrphans), every 30 s in the background, and immediately when
// NodeInfo goes from failing to succeeding.

// lostLeaseMessage is the 410 error body for a lease whose sandbox died
// in a substrate crash.
const lostLeaseMessage = "lease lost in a substrate crash; delete this lease"

// recoverySummary is the reconcileCrash result and the
// POST /api/admin/reconcile response.
type recoverySummary struct {
	Recovered int `json:"recovered"`
	Lost      int `json:"lost"`
}

// reconcileCrash reconciles the lease state with the sandboxes that
// survived on the node. If List fails, nothing changes: leases are
// never marked lost on a list failure.
func (s *Service) reconcileCrash(ctx context.Context) recoverySummary {
	sbs, err := s.sub.List(ctx)
	if err != nil {
		s.log.Printf("reconcileCrash: list sandboxes failed: %v", err)
		return recoverySummary{}
	}
	present := make(map[string]bool, len(sbs))
	for _, sb := range sbs {
		present[sb.ID] = true
	}

	s.store.mu.Lock()
	var targets []*Lease
	for _, l := range s.store.leases {
		if l.released || !l.live() || l.busy {
			continue
		}
		if present[l.SandboxID] {
			continue
		}
		targets = append(targets, l)
	}
	s.store.mu.Unlock()

	var summary recoverySummary
	for _, l := range targets {
		if s.recoverOneLease(ctx, l).Result == "recovered" {
			summary.Recovered++
		} else {
			summary.Lost++
		}
	}

	// Pool entries whose sandbox is not live are dead.
	s.store.mu.Lock()
	for img, ids := range s.store.pool {
		kept := ids[:0]
		for _, id := range ids {
			if present[id] {
				kept = append(kept, id)
			} else {
				s.removePoolLocked(id)
			}
		}
		s.store.pool[img] = kept
	}
	s.store.mu.Unlock()

	// Recovered leases change the peer allowances (new host IPs).
	s.refreshPeersAsync(ctx)
	return summary
}

// recoveryOutcome is what recoverOneLease did with one lease: "recovered"
// (it came back from its checkpoint) or "lost" (it had none, or the
// recovery failed). Generation and State are the lease's values after the
// call. The crash test reports it; the reconcile loop counts it.
type recoveryOutcome struct {
	Result     string `json:"result"`
	Generation int64  `json:"generation"`
	State      string `json:"state"`
}

// recoverOneLease runs the per-lease half of crash reconciliation for one
// lease whose sandbox has vanished: from its newest checkpoint when it has
// one (generation +1, event "recovered", state recovered), lost otherwise
// (event "lost"). It is shared by the startup/background reconcile loop
// and the crash test, so both take the identical path. The caller
// has already removed the lease's sandbox (a real or simulated crash);
// this function emits the lease events and writes the log lines.
func (s *Service) recoverOneLease(ctx context.Context, l *Lease) recoveryOutcome {
	if l.LastCheckpointBuildID == "" {
		// No checkpoint to recover from: the running state is gone.
		s.store.mu.Lock()
		l.setState("lost")
		s.saveLeaseLocked(l)
		s.store.mu.Unlock()
		s.deleteSandboxRow(l.SandboxID)
		s.emitLeaseEvent(l.ID, l.Owner, LeaseLost, "no checkpoint to recover from; the running state is gone")
		s.log.Printf("recovery: lease %s lost (checkpoint %s)", l.ID, formatRFC3339(l.LastCheckpointAt))
		return recoveryOutcome{Result: "lost", Generation: l.Generation, State: l.State}
	}
	if err := s.recoverFromCheckpoint(ctx, l); err != nil {
		s.store.mu.Lock()
		l.setState("lost")
		s.saveLeaseLocked(l)
		s.store.mu.Unlock()
		s.emitLeaseEvent(l.ID, l.Owner, LeaseLost, fmt.Sprintf("recovery from checkpoint %s failed: %v", l.LastCheckpointBuildID, err))
		s.log.Printf("recovery: lease %s lost (checkpoint %s): %v", l.ID, formatRFC3339(l.LastCheckpointAt), err)
		return recoveryOutcome{Result: "lost", Generation: l.Generation, State: l.State}
	}
	s.emitLeaseEvent(l.ID, l.Owner, LeaseRecovered, fmt.Sprintf("recovered from checkpoint %s", l.LastCheckpointBuildID))
	s.log.Printf("recovery: lease %s recovered (checkpoint %s)", l.ID, formatRFC3339(l.LastCheckpointAt))
	return recoveryOutcome{Result: "recovered", Generation: l.Generation, State: l.State}
}

// recoverFromCheckpoint resumes a lease from its checkpoint build with
// the same sandbox id (the UpsertSandbox in createSandbox replaces the
// stale row) and marks it recovered. The guest's memory did not continue
// from where its processes left it, so the generation bumps (2.2) and
// the new value is written into the guest.
func (s *Service) recoverFromCheckpoint(ctx context.Context, l *Lease) error {
	img, err := s.db.GetImage(ctx, l.Image)
	if err != nil {
		return err
	}
	b, err := s.db.GetBuild(ctx, l.LastCheckpointBuildID)
	if err != nil {
		return err
	}
	if !l.live() {
		// A suspended lease is uncharged (no hugepages), so turning its
		// sandbox back on here passes the memory check like any resume
		// (#128) — before the sandbox is created. The charge is the
		// image's CURRENT memory_mb (stamped below): the recovered
		// sandbox runs that value, so it is both what admission reserves
		// with and what the deferred release drops — the same rule as
		// restartCold and restore. Reserving with the lease's stale stamp
		// would admit an outdated charge (and leak the difference). A
		// running lease is already charged with its own stamp and
		// re-admits nothing. A recovery adds no lease, so only the
		// memory cap applies.
		if err := s.reserveQuota(l.Owner, 1, img.MemoryMB, false); err != nil {
			return err
		}
		defer s.releaseQuotaReservation(l.Owner, 1, img.MemoryMB)
		// Class re-admission (#128 part 2), as for a resume: recovery
		// brings hugepages back, so a burst lease re-passes the reserve
		// (the reconciler's own reserve work lands in #128 part 3; until
		// then a burst lease keeps its demand-burst standing here).
		class, err := s.admitClass(ctx, l.Owner, img.MemoryMB, l.Burst)
		if err != nil {
			return err
		}
		l.Class = class
	}
	sb, err := s.createSandbox(ctx, img, b, true, l.SandboxID, l)
	if err != nil {
		return err
	}
	s.store.mu.Lock()
	l.setState("recovered")
	l.RecoveredFrom = l.LastCheckpointAt
	l.BuildID = l.LastCheckpointBuildID
	l.HostIP = sb.HostIP
	l.ExposedIP = sb.HostIP
	// The recovered sandbox runs the image's current memory_mb: the
	// lease keeps the charge it was admitted with (#128) — stamped 0
	// rows (pre-quota leases over a vanished image) stay uncharged.
	l.MemoryMB = img.MemoryMB
	s.bumpGenerationLocked(l)
	s.saveLeaseLocked(l)
	s.store.mu.Unlock()
	s.writeGeneration(l)
	// Crash recovery rebuilt the guest from a checkpoint: its memory did
	// not continue, so every running job is lost (2.6, #135).
	s.markLeaseJobsLost(ctx, l.ID, l.Owner, "lease recovered from a checkpoint; the job did not survive")
	// Crash recovery replaced the sandbox; put the lease's create-time
	// secrets back into the fresh tmpfs (#80).
	s.restageCreateSecrets(ctx, l, "recovery")
	return nil
}
