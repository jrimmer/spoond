package api

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// Preemption (#128 part 3): a guaranteed admission that cannot get its
// hugepages reclaims them from burst leases by suspending them through
// the existing pause path (memory continues on resume, the generation
// does not change). Preemptions are serialised — one preempting
// admission at a time, under preemptMu — so two guaranteed creates
// cannot each preempt for themselves. Each preempted lease is stamped
// preempted_at and comes back on its holder's next work call, like every
// other suspended lease (#145 D2): there is no background auto-resume.

// DefaultPreemptDiskFloorPct is the snapshot-disk free percentage the
// preemptor must leave after a pause, when PREEMPT_DISK_FLOOR_PCT is
// unset: a pause writes a snapshot, and filling the disk to admit work
// would be a worse failure than a refused create.
const DefaultPreemptDiskFloorPct = 15

// errPreemptCannot is returned when a guaranteed admission ran out of
// hugepages and could not preempt any burst lease because the snapshot
// disk is too full. The lease API maps it to 503
// "capacity: cannot preempt (snapshot disk low)" with Retry-After: 30.
var errPreemptCannot = errors.New("cannot preempt (snapshot disk low)")

// preemptDiskFloorPct is the effective disk floor: the configured value,
// or the default when unset.
func (s *Service) preemptDiskFloorPct() float64 {
	if s.cfg.PreemptDiskFloorPct <= 0 {
		return DefaultPreemptDiskFloorPct
	}
	return s.cfg.PreemptDiskFloorPct
}

// guaranteedFits reports whether the node's free hugepages (from the
// cache, already net of earlier admissions) can host memoryMB while
// leaving the burst reserve free — that is, free − reserve ≥ memoryMB.
// An unhealthy node reports true: there is nothing to reclaim for, and
// the ordinary capacity check answers. A node that cannot be read
// reports false so the caller may try to preempt; if nothing can be
// preempted the plain capacity check still answers.
func (s *Service) guaranteedFits(ctx context.Context, memoryMB int) (bool, error) {
	s.nodeInfoMu.Lock()
	defer s.nodeInfoMu.Unlock()
	freeMiB, err := s.freeHugepageMiBLocked(ctx)
	if err != nil {
		return false, err
	}
	if s.nodeInfoCache.Status != "healthy" {
		return true, nil
	}
	// The burst reserve keeps room free *for* guaranteed work: burst
	// leases may not dip into it, a guaranteed lease may. So a
	// guaranteed admission fits whenever its own memory is free, and
	// preempts only when it is not.
	return freeMiB >= uint64(memoryMB), nil
}

// creditNodeInfoLocked returns memoryMB MiB of hugepages to the cached
// NodeInfo when a lease is paused: the next admission inside the same
// cache window sees them free, exactly as debitNodeInfoLocked takes them
// on admission. The cache's timestamp is left alone: credits and debits
// only adjust the reading inside its TTL, and the next refresh replaces
// it with the substrate's own figure (accurate once the pause or delete
// has returned), so they never accumulate drift. Caller holds
// nodeInfoMu.
// creditNodeInfo is creditNodeInfoLocked with nodeInfoMu taken. A
// release or pause calls it; neither runs with nodeInfoMu held.
func (s *Service) creditNodeInfo(memoryMB int) {
	s.nodeInfoMu.Lock()
	s.creditNodeInfoLocked(memoryMB)
	s.nodeInfoMu.Unlock()
	if memoryMB > 0 {
		s.admitQ.capacityGen.Add(1)
	}
}

func (s *Service) creditNodeInfoLocked(memoryMB int) {
	info := &s.nodeInfoCache
	if s.nodeInfoAt.IsZero() || info.HugepageSizeBytes == 0 || memoryMB <= 0 {
		return
	}
	bytes := uint64(memoryMB) * 1024 * 1024
	pages := (bytes + info.HugepageSizeBytes - 1) / info.HugepageSizeBytes
	if pages > info.HugepagesUsed {
		info.HugepagesUsed = 0
	} else {
		info.HugepagesUsed -= pages
	}
}

// ownerOverGuaranteeLocked is how far over its guaranteed_mib an owner
// currently is, 0 when it has no guarantee or is within it. Caller
// holds the store lock.
func (s *Service) ownerOverGuaranteeLocked(owner string) int {
	if s.identities == nil {
		return 0
	}
	u := s.identities.UserByID(owner)
	if u == nil || u.GuaranteedMiB <= 0 {
		return 0
	}
	over := s.usedMiBLocked(owner) - u.GuaranteedMiB
	if over < 0 {
		return 0
	}
	return over
}

// preemptionCandidates returns the running burst leases that may be
// preempted, in preemption order: lowest priority first, then the
// newest lease, then the owner furthest over its guarantee. It snapshots
// the store; each candidate is re-checked just before it is paused.
func (s *Service) preemptionCandidates() []*Lease {
	s.store.mu.Lock()
	var out []*Lease
	for _, l := range s.store.leases {
		if l.released || l.busy || l.Suspended || !l.live() || l.Class != ClassBurst || l.Pinned {
			continue
		}
		out = append(out, l)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		// Lowest priority first.
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		// Then newest.
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		// Then the owner furthest over its guarantee.
		ao, bo := s.ownerOverGuaranteeLocked(a.Owner), s.ownerOverGuaranteeLocked(b.Owner)
		if ao != bo {
			return ao > bo
		}
		// Deterministic tiebreak so the order is stable.
		return a.ID < b.ID
	})
	s.store.mu.Unlock()
	return out
}

// preemptDiskOK reports whether pausing l (estimated at its memory_mb,
// the future snapshot's size) leaves the snapshot disk at or above the
// preemption floor. A disk that cannot be read does not block
// preemption — same as the held-lease rules.
func (s *Service) preemptDiskOK(l *Lease) bool {
	if s.cfg.TemplateStoragePath == "" {
		return true
	}
	total, free, err := s.diskCapacity(s.cfg.TemplateStoragePath)
	if err != nil || total == 0 {
		return true
	}
	// The pause writes a snapshot: free shrinks by roughly the lease's
	// memory_mb, so the floor must hold after that.
	est := uint64(l.MemoryMB) * 1024 * 1024
	after := uint64(0)
	if free > est {
		after = free - est
	}
	return float64(after)/float64(total)*100 >= s.preemptDiskFloorPct()
}

// admitGuaranteed admits a guaranteed lease (#128 part 3): under
// preemptMu it first reclaims hugepages by preempting burst leases until
// free − reserve ≥ memoryMB, then debits the admission from the cached
// reading. The mutex spans preemption and the debit, so two concurrent
// guaranteed admissions cannot each preempt for themselves: the first
// one's debit is visible to the second.
func (s *Service) admitGuaranteed(ctx context.Context, owner string, memoryMB int) error {
	s.preemptMu.Lock()
	defer s.preemptMu.Unlock()
	if err := s.preemptForGuaranteed(ctx, owner, memoryMB); err != nil {
		return err
	}
	s.nodeInfoMu.Lock()
	s.debitNodeInfoLocked(memoryMB)
	s.nodeInfoMu.Unlock()
	return nil
}

// preemptForGuaranteed takes memory back for an admission that does not
// fit (FS2a, spoond-pxsn): the fair-shares selector (memVictims over
// takeBackOwners) names the leases to pause — the owner furthest over
// their slice first, that owner's least recently used unpinned lease,
// busy leases last, an owner inside their slice never — and one pause
// runs at a time through takeBackPause, with the free-hugepage reading
// re-checked after each. It stops as soon as the admission fits. The
// caller holds preemptMu and performs the debit.
//
// It first checks whether the disk-allowed candidates can free enough:
// when they cannot but the disk-blocked ones would, the disk floor is
// what stops the admission and it returns errPreemptCannot **without
// pausing anything**. When not even every candidate together is enough,
// there is nothing to gain from taking anything and it falls through to
// the ordinary capacity check. A NodeInfo read failure likewise falls
// through rather than failing here (the ordinary check answers).
func (s *Service) preemptForGuaranteed(ctx context.Context, owner string, memoryMB int) error {
	fits, err := s.guaranteedFits(ctx, memoryMB)
	if err != nil {
		// The node could not be read: do not fail the guaranteed
		// admission here. createSandbox's ordinary capacity check reads
		// NodeInfo again and answers; before part 3 guaranteed
		// admissions did not consult NodeInfo at this point at all.
		s.log.Printf("preempt: node info: %v", err)
		return nil
	}
	if fits {
		return nil
	}

	candidates := s.preemptionCandidates()
	var freeable, blocked uint64
	diskBlocked := false
	for _, v := range candidates {
		if s.preemptDiskOK(v) {
			freeable += uint64(v.MemoryMB)
		} else {
			diskBlocked = true
			blocked += uint64(v.MemoryMB)
		}
	}
	freeMiB, err := s.cachedFreeHugepageMiB(ctx)
	if err != nil {
		s.log.Printf("preempt: node info: %v", err)
		return nil
	}
	need := uint64(memoryMB) // the reserve is for guaranteed work (guaranteedFits)
	if freeMiB+freeable < need {
		if diskBlocked && freeMiB+freeable+blocked >= need {
			// The disk floor is the only thing in the way: refuse
			// without suspending any lease.
			return errPreemptCannot
		}
		// Pinned leases are not take-back victims (FS5). If the pinned
		// ones alone would free enough, the request is box_full: nothing
		// unpinned can be taken for it.
		if freeMiB+freeable+s.pinnedFreeableMiB() >= need {
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
	// fresh views and fresh free memory, bounded — a plan that keeps
	// being refused ends after this many rounds and the ordinary
	// capacity check answers.
	const maxRounds = 3
	for rounds := 0; rounds < maxRounds; rounds++ {
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
			if !s.preemptDiskOK(l) {
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
			// of the loop re-plans: takeBackOwners returns fresh views
			// (a refused name is either still there under new facts or
			// gone), and the free figure and shortfall are re-read. A
			// take-back that un-did itself (a pin mid-pause) is already
			// debited back, so the fresh reading does not double-count
			// it as free. A plan that is refused every round exhausts
			// the bound and falls through below.
			continue
		}
	}
	// Still short after the bounded re-plans (every candidate was
	// refused each round, or the node could not be read): nothing was
	// gained by trying. The caller's debit and createSandbox's ordinary
	// capacity check answer from here.
	return nil
}

// pinnedFreeableMiB is the memory of the running burst leases that are
// pinned and would otherwise be take-back candidates. It lets the
// preemptor tell "no room exists" from "every candidate is pinned".
func (s *Service) pinnedFreeableMiB() uint64 {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	var n uint64
	for _, l := range s.store.leases {
		if l.released || l.busy || l.Suspended || !l.live() || l.Class != ClassBurst || !l.Pinned {
			continue
		}
		n += uint64(l.MemoryMB)
	}
	return n
}

// boxFullAlert logs and announces the box_full refusal (FS5): a request
// needed room and every take-back candidate was pinned. Nothing was
// paused or released; an operator can free room by unpinning or
// releasing leases.
func (s *Service) boxFullAlert(owner string, memoryMB int) {
	s.log.Printf("box_full: %d MiB request for %q cannot be admitted: every take-back candidate is pinned; nothing was paused or released", memoryMB, owner)
	if s.metrics != nil {
		s.metrics.BoxFullTotal.Inc()
	}
	s.emitLeaseEvent("-", "", LeaseBoxFull, fmt.Sprintf("%d MiB request for %q: every take-back candidate is pinned", memoryMB, owner))
}

// cachedFreeHugepageMiB reads the node's free hugepage memory in MiB from
// the service's cache (filling it on demand), without the healthy-status
// short-circuit guaranteedFits applies.
func (s *Service) cachedFreeHugepageMiB(ctx context.Context) (uint64, error) {
	s.nodeInfoMu.Lock()
	defer s.nodeInfoMu.Unlock()
	return s.freeHugepageMiBLocked(ctx)
}

// preemptLease suspends one burst lease through the pause path and
// records its preemption: preempted_at is stamped (persisted) and a
// "preempted" event names the guaranteed lease's owner. The lease's
// generation does not change — the pause/resume continues its memory.
// The candidate is re-checked under the store lock first: activity since
// the snapshot (a release, a resume, a concurrent operation) skips it.
func (s *Service) preemptLease(ctx context.Context, l *Lease, targetOwner string) error {
	s.store.mu.Lock()
	if l.released || l.busy || l.Suspended || !l.live() || l.Class != ClassBurst || l.Pinned {
		s.store.mu.Unlock()
		return errLeaseBusy
	}
	s.store.mu.Unlock()

	if _, err := s.pauseLeaseWith(ctx, l, false, suspendPolicy{reason: suspendReasonPreempt}); err != nil {
		return err
	}

	s.store.mu.Lock()
	if !l.Suspended || l.busy || l.released {
		// A concurrent resume won the race: pauseLease cleared busy as
		// it returned, so a resume may have taken it and still be in
		// its sub calls with Suspended true. Testing busy as well stops
		// us stamping (and crediting memory for) a lease that is coming
		// back, and keeps a spurious preempted event/counter off the
		// stream. A release between the pause and this re-check is
		// skipped too: a released lease must not gain a preemption
		// stamp, counter or event (spoond-d76).
		s.store.mu.Unlock()
		return errLeaseBusy
	}
	l.PreemptedAt = s.now()
	l.LastAction, l.LastActionAt = pauseActionPreempt, l.PreemptedAt
	s.saveLeaseLocked(l)
	s.store.mu.Unlock()

	if s.metrics != nil {
		s.metrics.PreemptionsTotal.Inc()
	}
	s.emitLeaseEvent(l.ID, l.Owner, LeasePreempted, "for a guaranteed lease of "+targetOwner)
	// The pause already credited this lease's hugepages to the cached
	// reading (pauseLeaseBody), so the next fits check in this
	// preemption window sees them.
	// A preemption frees capacity: retry waiting creates (#129).
	s.wakeAdmissionQueue()
	return nil
}

// preemptedCountLocked reports how many live leases are currently
// preempted (for the gauge). Caller holds the store lock.
func (s *Service) preemptedCountLocked() int {
	n := 0
	for _, l := range s.store.leases {
		if !l.released && !l.PreemptedAt.IsZero() && l.State == "suspended" {
			n++
		}
	}
	return n
}
