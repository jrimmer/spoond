package api

import (
	"context"
	"errors"
	"sort"
	"time"
)

// Preemption (#128 part 3): a guaranteed admission that cannot get its
// hugepages reclaims them from burst leases by suspending them through
// the existing pause path (memory continues on resume, the generation
// does not change). Preemptions are serialised — one preempting
// admission at a time, under preemptMu — so two guaranteed creates
// cannot each preempt for themselves. Each preempted lease is stamped
// preempted_at and the resume queue brings it back when capacity allows.

// DefaultPreemptDiskFloorPct is the snapshot-disk free percentage the
// preemptor must leave after a pause, when PREEMPT_DISK_FLOOR_PCT is
// unset: a pause writes a snapshot, and filling the disk to admit work
// would be a worse failure than a refused create.
const DefaultPreemptDiskFloorPct = 15

// preemptResumeInterval is how often the resume queue looks for
// preempted leases that fit again.
const preemptResumeInterval = 15 * time.Second

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
	return freeMiB >= uint64(s.burstReserveMiB())+uint64(memoryMB), nil
}

// creditNodeInfoLocked returns memoryMB MiB of hugepages to the cached
// NodeInfo when a lease is paused: the next admission inside the same
// cache window sees them free, exactly as debitNodeInfoLocked takes them
// on admission. The cache's timestamp is refreshed so the credited
// reading is not immediately overwritten by a stale refresh. Caller
// holds nodeInfoMu.
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
	s.nodeInfoAt = s.now()
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
		if l.released || l.busy || l.Suspended || !l.live() || l.Class != ClassBurst {
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

// preemptForGuaranteed suspends burst leases (lowest priority, then
// newest, then the owner furthest over its guarantee) until the node can
// host memoryMB with the reserve intact, then returns. The caller holds
// preemptMu and performs the debit.
//
// It first checks whether the disk-allowed candidates can free enough:
// when they cannot but the disk-blocked ones would, the disk floor is
// what stops the admission and it returns errPreemptCannot **without
// preempting anything**. When not even every candidate together is
// enough, there is nothing to gain from preempting and it falls through
// to the ordinary capacity check. A NodeInfo read failure likewise
// falls through rather than failing here (the ordinary check answers).
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
	need := uint64(s.burstReserveMiB()) + uint64(memoryMB)
	if freeMiB+freeable < need {
		if diskBlocked && freeMiB+freeable+blocked >= need {
			// The disk floor is the only thing in the way: refuse
			// without suspending any lease.
			return errPreemptCannot
		}
		// Not enough memory exists at all: preempting would only
		// suspend leases for an admission that cannot succeed.
		return nil
	}

	for _, v := range candidates {
		if !s.preemptDiskOK(v) {
			continue
		}
		if err := s.preemptLease(ctx, v, owner); err != nil {
			if !errors.Is(err, errLeaseBusy) {
				s.log.Printf("preempt: lease %s: %v", v.ID, err)
			}
			continue
		}
		if fits, err = s.guaranteedFits(ctx, memoryMB); err != nil {
			s.log.Printf("preempt: node info: %v", err)
			return nil
		}
		if fits {
			return nil
		}
	}
	return nil
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
	if l.released || l.busy || l.Suspended || !l.live() || l.Class != ClassBurst {
		s.store.mu.Unlock()
		return errLeaseBusy
	}
	s.store.mu.Unlock()

	if _, err := s.pauseLease(ctx, l, false); err != nil {
		return err
	}

	s.store.mu.Lock()
	if !l.Suspended || l.busy {
		// A concurrent resume won the race: pauseLease cleared busy as
		// it returned, so a resume may have taken it and still be in
		// its sub calls with Suspended true. Testing busy as well stops
		// us stamping (and crediting memory for) a lease that is coming
		// back, and keeps a spurious preempted event/counter off the
		// stream.
		s.store.mu.Unlock()
		return errLeaseBusy
	}
	l.PreemptedAt = s.now()
	s.saveLeaseLocked(l)
	s.store.mu.Unlock()

	if s.metrics != nil {
		s.metrics.PreemptionsTotal.Inc()
	}
	s.emitLeaseEvent(l.ID, l.Owner, LeasePreempted, "for a guaranteed lease of "+targetOwner)
	// The pause freed this lease's hugepages: let the next fits check in
	// this preemption window see them.
	s.nodeInfoMu.Lock()
	s.creditNodeInfoLocked(l.MemoryMB)
	s.nodeInfoMu.Unlock()
	// A preemption frees capacity: retry waiting creates (#129).
	s.wakeAdmissionQueue()
	return nil
}

// runPreemptResumeLoop resumes preempted leases every 15 s when they fit
// again. It stops with ctx and skips while the node is draining.
func (s *Service) runPreemptResumeLoop(ctx context.Context) {
	t := time.NewTicker(preemptResumeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.draining.Load() {
				continue
			}
			s.resumePreempted(ctx)
		}
	}
}

// resumePreempted resumes preempted leases, oldest preemption first,
// through the normal resume path: the lease re-passes the memory quota
// and re-decides its class, so it comes back as a burst lease if the
// owner is still above the guarantee. A lease that does not fit yet is
// left for a later tick.
//
// A preempted lease that comes back classified guaranteed may itself
// preempt other burst leases (normal admission does that). The cascade
// is bounded — each tick only brings back preempted leases — and is the
// spec-conforming consequence of the class re-decision, not a leak.
func (s *Service) resumePreempted(ctx context.Context) {
	type victim struct {
		l  *Lease
		at time.Time
	}
	s.store.mu.Lock()
	var victims []victim
	for _, l := range s.store.leases {
		if l.released || l.PreemptedAt.IsZero() || l.State != "suspended" {
			continue
		}
		victims = append(victims, victim{l, l.PreemptedAt})
	}
	s.store.mu.Unlock()
	sort.SliceStable(victims, func(i, j int) bool { return victims[i].at.Before(victims[j].at) })

	for _, v := range victims {
		if _, err := s.resumeLease(ctx, v.l); err != nil {
			if !errors.Is(err, errLeaseBusy) {
				s.log.Printf("preempt: resume %s: %v", v.l.ID, err)
			}
			continue
		}
		s.log.Printf("preempt: resumed %s (preempted %s ago)", v.l.ID, s.now().Sub(v.at).Round(time.Second))
	}
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
