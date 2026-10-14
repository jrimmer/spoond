package api

import (
	"context"
	"errors"
	"time"
)

// Low-memory take-back (MEMORY_FLOOR_MIB, spoond-l3lq). The overcommit
// cap (MEMORY_OVERCOMMIT) keeps admission from promising more memory
// than the running VMs may fault back in; the floor is the running
// counterpart: every 15 s the backend reads the node's free hugepages
// fresh and, when they sit under the floor, takes back exactly one
// lease through the ordinary take-back rules (memVictims over the
// fair-share views, takeBackPause). One lease per tick: the pause's
// snapshot write is what frees the pages, so the next tick — not this
// one — judges the result, and a floor breach sheds load a lease at a
// time instead of pausing a stack at once.
//
// The loop is admission's backstop, not a second policy: it never
// touches pinned leases or owners inside their slice (memVictims skips
// both), a lease paused here resumes on its holder's next work call
// like any take-back, and a floor nothing can be taken back for alerts
// at most once per 10 min instead of every tick.

const (
	// DefaultMemoryFloorMiB is MEMORY_FLOOR_MIB when unset: keep 4 GiB
	// of hugepage memory free.
	DefaultMemoryFloorMiB = 4096
	// pressureInterval is how often the loop checks the floor.
	pressureInterval = 15 * time.Second
	// pressureAlertEvery is how often a floor that cannot be met may
	// repeat its box_full alert.
	pressureAlertEvery = 10 * time.Minute
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

// pressureCheck is one tick of the low-memory loop: read the node
// fresh, and below the floor pause one lease — the least recently used
// unpinned lease of the owner furthest over their fair slice, the same
// choice admission's take-back makes.
func (s *Service) pressureCheck(ctx context.Context) {
	floor := s.cfg.MemoryFloorMiB
	if floor == 0 {
		return // the floor is off
	}
	// Draining pauses every lease and undrain resumes it (U10): the
	// floor must not fight the drain for the same leases.
	if s.draining.Load() {
		return
	}
	// What the box has promised to live VMs, for the gauge. The store
	// read needs no node round trip, so the gauge moves every tick even
	// while the orchestrator is unreachable.
	committed := s.committedMiB()
	if s.metrics != nil {
		s.metrics.MemoryCommittedMiB.Set(float64(committed))
	}
	// A fresh reading, not the admission cache: the floor is a standing
	// condition, and the credits and debits inside the cache's TTL would
	// blur it. The next admission refreshes the cache anyway.
	info, err := s.sub.NodeInfo(ctx)
	if err != nil {
		s.log.Printf("memory floor: node info: %v", err)
		return
	}
	free := int(info.FreeHugepageBytes() / (1024 * 1024))
	if free >= floor {
		return
	}
	short := floor - free

	// One take-back at a time box-wide (the same lock admission holds):
	// a floor pause and a concurrent admission must not each pause a
	// different lease for themselves.
	s.preemptMu.Lock()
	defer s.preemptMu.Unlock()

	owners := s.takeBackOwners(ctx)
	victims := memVictims(owners, "", short, short)
	if len(victims) == 0 {
		// Nothing can be taken back (every candidate pinned, or no owner
		// over their slice): say so, but at most once per 10 min — a
		// standing condition must not flood the event stream.
		now := s.now()
		if s.pressureAlertAt.IsZero() || now.Sub(s.pressureAlertAt) >= pressureAlertEvery {
			s.pressureAlertAt = now
			s.boxFullAlert("", short, "memory below floor; nothing can be taken back")
		}
		return
	}
	// The first victim only: the pause itself frees the pages, and the
	// next tick re-reads free memory before deciding again.
	v := victims[0]
	l := s.lookupAny(v.LeaseID)
	if l == nil {
		return // released between the views and the pause
	}
	if err := s.takeBackPause(ctx, l, pressureRequester, v.Ratio); err != nil {
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
