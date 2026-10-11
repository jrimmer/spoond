package api

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// admitMemory admits memoryMB of guest memory for owner against the one
// hugepage pool (FS2b-1, spoond-pxsn). Every lease admits the same way,
// with no guaranteed or burst class: when the request fits the free
// hugepages it debits the cached reading and returns; when it does not,
// take-back (memVictims over takeBackOwners, one takeBackPause at a
// time) pauses the leases of the owners furthest over their slice until
// it fits. A request no take-back can serve returns errBoxFull when only
// pinned leases stand in the way or the requester is itself the biggest
// borrower (a *memBoxFullError), and otherwise falls through to the
// ordinary capacity check that createSandbox performs.
//
// Under preemptMu, so two concurrent admissions cannot each take back
// for themselves: the first one's debit is visible to the second. A
// resuming lease is suspended, so it is never its own victim; the
// requester's own leases are skipped by memVictims.
//
// On success admitMemory returns an undo function that gives the debit
// back (creditNodeInfo) when the create or resume it admitted never
// took hold: without it, for the rest of the cache TTL fits() reads
// less free memory than there is and take-back may pause a lease for
// memory that is free. undo credits exactly the admitted memoryMB and
// is safe to call more than once (only the first call credits). On
// error it returns a no-op undo, so a caller can always defer it.
func (s *Service) admitMemory(ctx context.Context, owner string, memoryMB int) (undo func(), err error) {
	s.preemptMu.Lock()
	defer s.preemptMu.Unlock()
	if err := s.takeBackForAdmission(ctx, owner, memoryMB); err != nil {
		return func() {}, err
	}
	s.nodeInfoMu.Lock()
	s.debitNodeInfoLocked(memoryMB)
	s.nodeInfoMu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { s.creditNodeInfo(memoryMB) }) }, nil
}

// takeBackForAdmission is the take-back loop of admitMemory. The caller
// holds preemptMu and performs the debit.
func (s *Service) takeBackForAdmission(ctx context.Context, owner string, memoryMB int) error {
	fits, err := s.fits(ctx, memoryMB)
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

	freeMiB, err := s.cachedFreeHugepageMiB(ctx)
	if err != nil {
		s.log.Printf("preempt: node info: %v", err)
		return nil
	}
	need := uint64(memoryMB)
	owners := s.takeBackOwnersFor(ctx, owner, memoryMB)
	takeable, pinned := takeBackPool(owners, owner)
	if freeMiB+takeable < need {
		// Pinned leases are not take-back victims (FS5). If the pinned
		// leases of the owners over their slice would free enough, the
		// request is box_full: nothing unpinned can be taken for it.
		if freeMiB+takeable+pinned >= need {
			s.boxFullAlert(owner, memoryMB, "every take-back candidate is pinned")
			return errBoxFull
		}
		// The requester itself is the biggest borrower: box_full at
		// once, it does not wait (fair shares).
		if bf := biggestBorrowerRefusal(owners, owner, memoryMB); bf != nil {
			s.boxFullAlert(owner, memoryMB, "the requester is the biggest borrower")
			return bf
		}
		// A shortfall no owner causes (memory held outside spoond, or
		// every owner inside their slice): taking back would only
		// suspend leases for an admission that cannot succeed. The
		// ordinary capacity check answers with a wait.
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
	pausedAny := false
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
		owners = s.takeBackOwnersFor(ctx, owner, memoryMB)
		victims := memVictims(owners, owner, shortfall, memoryMB)
		if len(victims) == 0 {
			// Take-back can free nothing. When the requester would be
			// over its own slice and no other owner borrows more, the
			// requester is the biggest borrower: box_full at once, it
			// does not wait (fair shares). Otherwise (everyone inside
			// their slice, or a bigger borrower whose leases are busy)
			// the ordinary capacity check answers with a wait.
			if !pausedAny {
				if bf := biggestBorrowerRefusal(owners, owner, memoryMB); bf != nil {
					s.boxFullAlert(owner, memoryMB, "the requester is the biggest borrower")
					return bf
				}
			}
			return nil
		}
		// One pause at a time, in the selector's order, re-checking free
		// memory after each and stopping as soon as the request fits.
		paused := false
		for _, v := range victims {
			if fits, err = s.fits(ctx, memoryMB); err != nil {
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
			paused, pausedAny = true, true
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

// memBoxFullError is the memory box_full refusal for a requester that is
// itself the biggest borrower: its usage after the request is over its
// slice and no other owner's ratio is higher, so take-back has no one to
// take from. It unwraps to errBoxFull (429 box_full).
type memBoxFullError struct {
	Owner             string
	UsedMiB, SliceMiB int
	RequestMiB        int
}

func (e *memBoxFullError) Error() string {
	return fmt.Sprintf("box_full: owner %q would use %d MiB of a %d MiB memory slice with this %d MiB request, and no other owner borrows more",
		e.Owner, e.UsedMiB+e.RequestMiB, e.SliceMiB, e.RequestMiB)
}

func (e *memBoxFullError) Unwrap() error { return errBoxFull }

// biggestBorrowerRefusal returns the box_full refusal when requester
// already holds at least its slice and, after requestMiB more, is over
// it and at least as far over it (by used/slice) as every other owner;
// nil otherwise. An owner without
// a slice (unknown to the fair snapshot) is never refused here.
func biggestBorrowerRefusal(owners []takeBackOwner, requester string, requestMiB int) error {
	var req *takeBackOwner
	for i := range owners {
		if owners[i].Owner == requester {
			req = &owners[i]
			break
		}
	}
	if req == nil || req.SliceMiB <= 0 {
		return nil
	}
	// The requester must already hold its whole slice: an owner that
	// borrows nothing yet is short only because of memory outside
	// spoond or a box full of fair-share usage — a wait, not a refusal.
	if req.UsedMiB < req.SliceMiB {
		return nil
	}
	after := tbRatio(req.UsedMiB+requestMiB, req.SliceMiB)
	if after <= 1 {
		return nil
	}
	for _, o := range owners {
		if o.Owner != requester && tbRatio(o.UsedMiB, o.SliceMiB) > after {
			return nil
		}
	}
	return &memBoxFullError{Owner: requester, UsedMiB: req.UsedMiB, SliceMiB: req.SliceMiB, RequestMiB: requestMiB}
}

// takeBackPool sums the memory of the leases take-back could ever reach
// for requester: those of the OTHER owners that are over their slice
// (an owner inside their slice is never touched, and the requester is
// never its own victim). takeable is their unpinned leases, pinned their
// pinned ones. Both are upper bounds: memVictims still stops an owner
// from dropping below the requester.
func takeBackPool(owners []takeBackOwner, requester string) (takeable, pinned uint64) {
	for _, o := range owners {
		if o.Owner == requester || o.SliceMiB <= 0 || o.UsedMiB <= o.SliceMiB {
			continue
		}
		for _, l := range o.Leases {
			if l.Pinned {
				pinned += uint64(l.MemoryMiB)
			} else {
				takeable += uint64(l.MemoryMiB)
			}
		}
	}
	return takeable, pinned
}
