package api

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Low-memory take-back (MEMORY_FLOOR_MIB, spoond-l3lq). The overcommit
// cap (MEMORY_OVERCOMMIT) keeps admission from promising more memory
// than the running VMs may fault back in; the floor is the running
// counterpart: every 15 s the backend reads the node's free hugepages
// fresh and, when they sit under the floor, takes back exactly one
// lease (pressureVictim: the least recently used unpinned lease of the
// owner furthest over their fair slice, non-busy before busy). One
// lease per tick: the pause's snapshot write is what frees the pages,
// so the next tick — not this one — judges the result, and a floor
// breach sheds load a lease at a time instead of pausing a stack at
// once.
//
// Anti-cycling (round 2): admission keeps the floor as headroom, so a
// lease the loop paused resumes only with the floor free on top of its
// own memory and is not paused again at once. The loop stands down
// while draining and on bad readings (a node that is not healthy or
// reports no pages is no usable reading).
// A lease still marked Drained (an undrain pending on it) is never
// picked, so a lease stuck Drained cannot switch the floor off for
// good — the loop keeps taking back around it.

const (
	// DefaultMemoryFloorMiB is MEMORY_FLOOR_MIB when unset: keep 2 GiB
	// of hugepage memory free.
	DefaultMemoryFloorMiB = 2048
	// pressureInterval is how often the loop checks the floor.
	pressureInterval = 15 * time.Second
	// pressureAlertEvery is how often a floor that cannot be met may
	// repeat its alert.
	pressureAlertEvery = 10 * time.Minute
	// pressurePauseTimeout bounds one floor pause: a snapshot write runs
	// about 9 s for a 4 GiB lease on sb and up to 35 s under I/O load,
	// so 2 min is the bound of a bounded operation, not a guess.
	pressurePauseTimeout = 2 * time.Minute
	// pressureRequester is the requester name the loop pauses with: it
	// names the loop in the lease's take-back stamp and the suspended /
	// take_back events. It is never an owner id, so takeBackPause's
	// own-lease refusal never matches it.
	pressureRequester = "memory_pressure"
)

// pressureLoop runs until ctx is done: every pressureInterval, keep the
// box's free hugepage memory at or above MEMORY_FLOOR_MIB.
func (s *Service) pressureLoop(ctx context.Context) {
	t := time.NewTicker(s.pressureCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.pressureCheck(ctx)
		}
	}
}

// committedMiB is the memory the box has committed to live VMs: the
// running and recovered leases' memory (runningMemoryMiB). What the
// gauge spoond_memory_committed_mib reports each tick.
func (s *Service) committedMiB() int {
	return s.runningMemoryMiB()
}

// pressureSkip reports why a tick must not take anything back at all,
// before any node read (the committed gauge has already moved): the
// floor is off, or the drain is in charge of the leases. A lease still
// marked Drained is NOT a tick skip (a lease stuck Drained would
// switch the floor off for good): pressureVictim just never picks it,
// the way it never picks a pinned lease, and the floor keeps taking
// back from the rest of the box.
func (s *Service) pressureSkip(floor int) bool {
	if floor == 0 {
		return true // the floor is off (the gauge has already moved)
	}
	return s.draining.Load()
}

// pressureCheck is one tick of the low-memory loop: read the node
// fresh, and below the floor pause one lease — pressureVictim's pick:
// the least recently used unpinned lease of the owner furthest over
// their fair slice, non-busy before busy. Leases still marked Drained
// are skipped like pinned ones (an undrain owns their resume), so a
// lease stuck Drained cannot switch the floor off for good.
func (s *Service) pressureCheck(ctx context.Context) {
	// What the box has promised to live VMs, for the gauge. The store
	// read needs no node round trip, so the gauge moves every tick —
	// with the floor off, while draining, and while the orchestrator is
	// unreachable alike.
	if s.metrics != nil {
		s.metrics.MemoryCommittedMiB.Set(float64(s.committedMiB()))
	}
	floor := s.cfg.MemoryFloorMiB
	if s.pressureSkip(floor) {
		return
	}
	// A fresh reading, not the admission cache: the floor is a standing
	// condition, and the credits and debits inside the cache's TTL would
	// blur it. The next admission refreshes the cache anyway.
	info, err := s.sub.NodeInfo(ctx)
	if err != nil {
		s.log.Printf("memory floor: node info: %v", err)
		return
	}
	// Bad readings are not a floor breach (round 2): an orchestrator
	// still booting or wedged reports no pages (or lies with a draining
	// status), and taking back on that would pause leases for nothing.
	if info.Status != "healthy" || info.HugepagesTotal == 0 {
		return
	}
	free := int(info.FreeHugepageBytes() / (1024 * 1024))
	if free >= floor {
		return
	}
	short := floor - free

	// One take-back at a time box-wide (the same lock admission holds
	// while it takes back): a floor pause and a concurrent admission must
	// not each pause a different lease for themselves. Draining may have
	// started since the skip above: re-check here, under the lock — the
	// drain itself pauses through pauseLease without taking preemptMu, so
	// this re-check is what keeps the two apart.
	s.preemptMu.Lock()
	defer s.preemptMu.Unlock()
	if s.draining.Load() {
		return
	}

	owners := s.takeBackOwners(ctx)
	v, ok := pressureVictim(owners)
	if !ok {
		// Nothing can be taken back (every candidate pinned, or no owner
		// over their slice): say so, but at most once per 10 min — a
		// standing condition must not flood the event stream. The alert
		// is its own event kind so a floor breach is not misread as an
		// admission's box_full refusal (which names a requester).
		now := s.now()
		if s.pressureAlertAt.IsZero() || now.Sub(s.pressureAlertAt) >= pressureAlertEvery {
			s.pressureAlertAt = now
			s.pressureFloorAlert(short)
		}
		return
	}
	l := s.lookupAny(v.LeaseID)
	if l == nil {
		return // released between the views and the pause
	}
	// The pause must not die with a shutdown that cancels ctx
	// mid-snapshot: a half-written snapshot would be the one state the
	// resume cannot repair. Detach from the loop's cancel and bound the
	// pause with a timeout instead (a snapshot write is bounded work).
	pauseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pressurePauseTimeout)
	defer cancel()
	if err := s.takeBackPause(pauseCtx, l, pressureRequester, v.Ratio); err != nil {
		if !errors.Is(err, errLeaseBusy) && !errors.Is(err, errLeaseReleased) {
			s.log.Printf("memory floor: lease %s: %v", v.LeaseID, err)
		}
		// Refused (a pin, release or job won the race): leave it to the
		// next tick to re-plan on fresh views.
		return
	}
	if s.metrics != nil {
		s.metrics.MemoryPressureTakebacksTotal.Inc()
	}
	s.log.Printf("memory floor: %d MiB free of a %d MiB floor: took back lease %s (%d MiB, owner ratio %.2f)",
		free, floor, v.LeaseID, v.MemoryMiB, v.Ratio)
}

// pressureFloorAlert raises the low-memory loop's alert when nothing
// can be taken back for the floor: short MiB of hugepages are missing
// and every candidate is pinned or Drained — or no owner is over their
// slice (a busy lease is still takeable, only ranked last). It is
// its own event kind, memory_floor — not box_full, which names an
// admission's refused requester — and notifies like one (a person must
// unpin or release leases to free room; nothing was paused).
func (s *Service) pressureFloorAlert(short int) {
	s.log.Printf("memory_floor: %d MiB below the floor; nothing can be taken back (every candidate pinned, or no owner over their slice); nothing was paused or released", short)
	s.emitLeaseEvent("", "", LeaseMemoryFloor,
		fmt.Sprintf("%d MiB below the memory floor; nothing can be taken back: every candidate is pinned, or no owner is over their slice", short))
}
