package api

import (
	"context"
	"errors"
	"fmt"
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

// preemptCapLogInterval rate-limits the per-lease "deferred (waiting for
// capacity)" line: a lease parked for room for hours logs its wait at
// most this often, not once per 15 s tick (spoond-dxq SH2).
const preemptCapLogInterval = 10 * time.Minute

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
	need := uint64(memoryMB) // the reserve is for guaranteed work (guaranteedFits)
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
			// Keep every owner's guarantee filled as leases churn (#128).
			s.promoteAllBurst()
		}
	}
}

// resumePreempted resumes preempted leases, oldest preemption first,
// through the normal resume path: the lease re-passes the memory quota
// and re-decides its class, so it comes back as a burst lease if the
// owner is still above the guarantee. A lease that does not fit yet is
// left for a later tick.
//
// A resume that keeps failing with a non-admission error is retried a
// bounded number of times (PREEMPT_RESUME_RETRIES); once the budget is
// spent the lease is marked lost with the reason and a lost event, so a
// permanently failing lease does not create for ever (spoond-dxq). An
// admission/capacity refusal is not a failure — the preemption parked
// the lease to free the very room it now waits for — so it neither
// counts an attempt nor starts the window, and the lease waits for room
// indefinitely (B1). A wait resets the window origin of any budget a
// counted failure started, so only an unbroken run of counted failures
// is bounded by the window (SH1).
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
		_, err := s.resumeLease(ctx, v.l)
		if err == nil {
			s.clearRetry(s.preemptRetries, v.l.ID)
			s.clearPreemptCapLog(v.l.ID)
			s.log.Printf("preempt: resumed %s (preempted %s ago)", v.l.ID, s.now().Sub(v.at).Round(time.Second))
			continue
		}
		if errors.Is(err, errLeaseBusy) {
			continue
		}
		// An admission/capacity refusal is room the preemption was meant
		// to free: the lease keeps waiting without touching its budget
		// count, so a long capacity wait never loses an intact preempted
		// lease (B1). The wait resets the window origin of any budget a
		// counted failure already started, so the window measures only an
		// unbroken run of counted failures and a long capacity wait
		// between them cannot age the lease out either (SH1).
		if preemptWaitForCapacity(err) {
			s.resetRetryWindow(s.preemptRetries, v.l.ID)
			if s.shouldLogPreemptCap(v.l.ID) {
				s.log.Printf("preempt: resume %s deferred (waiting for capacity): %v", v.l.ID, err)
			}
			continue
		}
		attempts, spent := s.noteRetryFailure(s.preemptRetries, v.l.ID, v.l.ID, s.preemptResumeLimit(), s.recoveryRetryWindow())
		if !spent {
			s.log.Printf("preempt: resume %s failed (attempt %d/%d), will retry: %v",
				v.l.ID, attempts, s.preemptResumeLimit(), err)
			continue
		}
		reason := fmt.Sprintf("preempted resume failed after %d attempt(s) within %s: %v",
			attempts, s.recoveryRetryWindow(), err)
		s.losePreempted(ctx, v.l, reason, err.Error())
	}
}

// losePreempted marks a preempted lease lost after its resume budget ran
// out: the reason is stored and carried by a lost event, its running jobs
// (none, a suspended lease has no live guest) are settled and snapshot
// retention runs. The caller has already given up on the resume. A lease
// released while the loss was in flight is left alone: no save, no lost
// event, no job marking (spoond-775).
func (s *Service) losePreempted(ctx context.Context, l *Lease, reason, cause string) {
	s.clearRetry(s.preemptRetries, l.ID)
	s.clearPreemptCapLog(l.ID)
	s.store.mu.Lock()
	released := l.released
	if !released {
		s.markLost(l, reason)
	}
	s.store.mu.Unlock()
	if released {
		return
	}
	s.emitLeaseEvent(l.ID, l.Owner, LeaseLost, reason)
	s.markLeaseJobsLost(ctx, l.ID, l.Owner, "lease lost after preemption; the job did not survive")
	s.rerunSnapshotRetention(ctx, l)
	s.log.Printf("preempt: lease %s lost after failing to resume: %s", l.ID, cause)
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
