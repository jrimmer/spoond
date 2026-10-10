package api

import (
	"context"
	"errors"
)

// admitMemory admits memoryMB of guest memory for owner against the one
// hugepage pool (FS2b-1, spoond-pxsn). Every lease admits the same way,
// with no guaranteed or burst class: when the request fits the free
// hugepages it debits the cached reading and returns; when it does not,
// take-back (memVictims over takeBackOwners, one takeBackPause at a
// time) pauses the leases of the owners furthest over their slice until
// it fits. A request no take-back can serve returns errBoxFull when only
// pinned leases stand in the way, and otherwise falls through to the
// ordinary capacity check that createSandbox performs.
//
// Under preemptMu, so two concurrent admissions cannot each take back
// for themselves: the first one's debit is visible to the second. A
// resuming lease is suspended, so it is never its own victim; the
// requester's own leases are skipped by memVictims.
func (s *Service) admitMemory(ctx context.Context, owner string, memoryMB int) error {
	s.preemptMu.Lock()
	defer s.preemptMu.Unlock()
	if err := s.takeBackForAdmission(ctx, owner, memoryMB); err != nil {
		return err
	}
	s.nodeInfoMu.Lock()
	s.debitNodeInfoLocked(memoryMB)
	s.nodeInfoMu.Unlock()
	return nil
}

// takeBackForAdmission is the take-back loop of admitMemory. The caller
// holds preemptMu and performs the debit.
func (s *Service) takeBackForAdmission(ctx context.Context, owner string, memoryMB int) error {
	fits, err := s.guaranteedFits(ctx, memoryMB)
	if err != nil {
		// The node could not be read: do not fail the admission here.
		// createSandbox's ordinary capacity check reads NodeInfo again
		// and answers.
		s.log.Printf("preempt: node info: %v", err)
		return nil
	}
	if fits {
		return nil
	}

	freeable := s.takeableRunningMiB()
	freeMiB, err := s.cachedFreeHugepageMiB(ctx)
	if err != nil {
		s.log.Printf("preempt: node info: %v", err)
		return nil
	}
	need := uint64(memoryMB)
	if freeMiB+freeable < need {
		// Pinned leases are not take-back victims (FS5). If the pinned
		// ones alone would free enough, the request is box_full: nothing
		// unpinned can be taken for it.
		if freeMiB+freeable+s.pinnedRunningMiB() >= need {
			s.boxFullAlert(owner, memoryMB)
			return errBoxFull
		}
		// Not enough memory exists at all: taking back would only
		// suspend leases for an admission that cannot succeed.
		return nil
	}

	// The selector works in MiB of freed hugepages and is re-run after
	// every pause: earlier pauses moved usage, so the biggest borrower
	// may have changed (and a taken-back lease must never be picked
	// again — it is suspended and leaves the running-lease views). The
	// need it stops on is the SHORTFALL (memoryMB minus what is free
	// now); the requester's after-request ratio is still computed for
	// the whole memoryMB inside memVictims. A pass in which every
	// candidate is refused (a pin or an exec won each one) re-plans on
	// fresh views and fresh free memory. Only refused rounds count
	// against the bound: a round that pauses something made progress,
	// so an admission that needs several take-backs keeps going — one
	// pause at a time, each pausing a different running lease — and a
	// plan that keeps being refused ends after this many refused rounds
	// and the ordinary capacity check answers.
	const maxRefusedRounds = 3
	refused := 0
	for refused < maxRefusedRounds {
		freeMiB, err = s.cachedFreeHugepageMiB(ctx)
		if err != nil {
			// The node could not be re-read: leave with what the earlier
			// pauses freed. The caller's debit and createSandbox's own
			// capacity check answer from here.
			s.log.Printf("preempt: node info: %v", err)
			return nil
		}
		shortfall := memoryMB - int(freeMiB)
		if shortfall <= 0 {
			return nil
		}
		victims := memVictims(s.takeBackOwners(ctx), owner, shortfall, memoryMB)
		if len(victims) == 0 {
			// Take-back can free nothing: either everyone is inside
			// their slice or the requester is itself the biggest
			// borrower. The ordinary capacity check answers.
			return nil
		}
		// One pause at a time, in the selector's order, re-checking free
		// memory after each and stopping as soon as the request fits.
		paused := false
		for _, v := range victims {
			if fits, err = s.guaranteedFits(ctx, memoryMB); err != nil {
				s.log.Printf("preempt: node info: %v", err)
				return nil
			}
			if fits {
				return nil
			}
			l := s.lookupAny(v.LeaseID)
			if l == nil {
				continue
			}
			if err := s.takeBackPause(ctx, l, owner, v.Ratio); err != nil {
				if !errors.Is(err, errLeaseBusy) && !errors.Is(err, errLeaseReleased) {
					s.log.Printf("preempt: lease %s: %v", v.LeaseID, err)
				}
				continue
			}
			paused = true
			break
		}
		if !paused {
			// Every candidate was refused (a race won each one). The top
			// of the loop re-plans on fresh views and fresh free memory;
			// a plan refused every round exhausts the bound.
			refused++
		}
	}
	// Still short after the bounded re-plans: nothing was gained by
	// trying. The caller's debit and createSandbox's ordinary capacity
	// check answer from here.
	return nil
}

// takeableRunningMiB is the memory of the running leases take-back may
// pause: live, idle, unpinned. It is an upper bound (the selector still
// leaves owners inside their slice alone).
func (s *Service) takeableRunningMiB() uint64 {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	var n uint64
	for _, l := range s.store.leases {
		if l.released || l.busy || l.Suspended || !l.live() || l.Pinned {
			continue
		}
		n += uint64(l.MemoryMB)
	}
	return n
}

// pinnedRunningMiB is the memory of the running leases that are pinned
// and so are not take-back candidates. It lets admitMemory tell "no room
// exists" from "every candidate is pinned".
func (s *Service) pinnedRunningMiB() uint64 {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	var n uint64
	for _, l := range s.store.leases {
		if l.released || l.busy || l.Suspended || !l.live() || !l.Pinned {
			continue
		}
		n += uint64(l.MemoryMB)
	}
	return n
}
