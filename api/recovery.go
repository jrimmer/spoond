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
		if l.LastCheckpointBuildID == "" {
			// No checkpoint to recover from: the running state is gone.
			s.store.mu.Lock()
			l.setState("lost")
			s.saveLeaseLocked(l)
			s.store.mu.Unlock()
			s.deleteSandboxRow(l.SandboxID)
			summary.Lost++
			s.emitLeaseEvent(l.ID, l.Owner, LeaseLost, "no checkpoint to recover from; the running state is gone")
			s.log.Printf("recovery: lease %s lost (checkpoint %s)", l.ID, formatRFC3339(l.LastCheckpointAt))
			continue
		}
		if err := s.recoverFromCheckpoint(ctx, l); err != nil {
			s.store.mu.Lock()
			l.setState("lost")
			s.saveLeaseLocked(l)
			s.store.mu.Unlock()
			summary.Lost++
			s.emitLeaseEvent(l.ID, l.Owner, LeaseLost, fmt.Sprintf("recovery from checkpoint %s failed: %v", l.LastCheckpointBuildID, err))
			s.log.Printf("recovery: lease %s lost (checkpoint %s): %v", l.ID, formatRFC3339(l.LastCheckpointAt), err)
			continue
		}
		summary.Recovered++
		s.emitLeaseEvent(l.ID, l.Owner, LeaseRecovered, fmt.Sprintf("recovered from checkpoint %s", l.LastCheckpointBuildID))
		s.log.Printf("recovery: lease %s recovered (checkpoint %s)", l.ID, formatRFC3339(l.LastCheckpointAt))
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
	s.bumpGenerationLocked(l)
	s.store.mu.Unlock()
	s.writeGeneration(l)
	return nil
}
