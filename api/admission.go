package api

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jrimmer/spoond/v2/substrate"
)

// Lease classes (#128 part 2). Guaranteed is today's admission: the
// owner's running charge with this lease stays within their
// guaranteed_mib (a user without one keeps every lease guaranteed).
// Burst covers work above the guarantee — it is admitted only while the
// node keeps its burst reserve of free hugepages, and it is preemptible
// even inside another user's guarantee.
const (
	ClassGuaranteed = "guaranteed"
	ClassBurst      = "burst"
)

// DefaultBurstReserveMiB is the burst reserve when BURST_RESERVE_MIB is
// unset: 8 GiB of hugepages kept free of burst leases, so guaranteed
// work (and crash recovery) always has room to land.
// TODO(FS2b-1 step 2): remove
const DefaultBurstReserveMiB = 8192

// nodeInfoCacheTTL bounds how long freeHugepageMiBLocked trusts its cached
// NodeInfo: long enough that a burst of admissions costs the
// orchestrator one round trip, short enough that the reserve cannot be
// raced past for long. The cache is filled on demand here and by the
// node gauges' loop — the same NodeInfo behind spoond_node_hugepages_free_bytes.
const nodeInfoCacheTTL = 15 * time.Second

// errBurstReserve is returned when a burst lease cannot be admitted
// because the node's free hugepages would dip under the burst reserve.
// The lease API maps it to 503 "no burst capacity" with Retry-After: 30.
var errBurstReserve = fmt.Errorf("no burst capacity")

// burstReserveMiB is the effective burst reserve in MiB (0 = disabled;
// a negative configuration reads as 0).
func (s *Service) burstReserveMiB() int {
	if s.cfg.BurstReserveMiB < 0 {
		return 0
	}
	return s.cfg.BurstReserveMiB
}

// freeHugepageMiB reports the node's free hugepage memory in MiB, from
// the substrate's NodeInfo cached for at most nodeInfoCacheTTL, less
// what admissions have taken since that reading (see debitNodeInfoLocked).
// A failed refresh answers with the last good value — staleness beats a
// wrong refusal — and errors only when nothing has ever been cached.
// The caller holds nodeInfoMu across the call and its debit, on
// purpose: concurrent admissions then share one round trip instead of
// stampeding the orchestrator, and two burst admissions cannot both
// pass against the same reading.
func (s *Service) freeHugepageMiBLocked(ctx context.Context) (uint64, error) {
	if s.nodeInfoAt.IsZero() || s.now().Sub(s.nodeInfoAt) >= nodeInfoCacheTTL {
		if info, err := s.sub.NodeInfo(ctx); err == nil {
			s.nodeInfoCache = info
			s.nodeInfoAt = s.now()
		} else if s.nodeInfoAt.IsZero() {
			return 0, fmt.Errorf("node info: %w", err)
		}
	}
	return s.nodeInfoCache.FreeHugepageBytes() / (1024 * 1024), nil
}

// debitNodeInfoLocked marks memoryMB MiB of hugepages used in the cached
// NodeInfo when a lease is admitted, so the next admission inside the
// same cache window sees them gone: without it, every burst admission
// in a 15 s window passed against one reading and together they could
// eat the whole reserve. A debit for a create that then fails only
// makes burst admission stricter until the next refresh. No-op while
// nothing is cached. The caller holds nodeInfoMu.
func (s *Service) debitNodeInfoLocked(memoryMB int) {
	info := &s.nodeInfoCache
	if s.nodeInfoAt.IsZero() || info.HugepageSizeBytes == 0 || memoryMB <= 0 {
		return
	}
	bytes := uint64(memoryMB) * 1024 * 1024
	info.HugepagesUsed += (bytes + info.HugepageSizeBytes - 1) / info.HugepageSizeBytes
}

// admit checks that the node can host a sandbox of memoryMB MiB: enough
// free hugepages (D12) and a healthy node. Otherwise it returns
// substrate.ErrCapacity, which the handlers map to HTTP 503
// {"error":"capacity: <reason>"}. Every refusal is counted (U11).
func (s *Service) admit(ctx context.Context, memoryMB int) error {
	if err := s.admitCapacity(ctx, memoryMB); err != nil {
		if s.metrics != nil {
			s.metrics.CapacityRej.Inc()
		}
		return err
	}
	return nil
}

func (s *Service) admitCapacity(ctx context.Context, memoryMB int) error {
	info, err := s.sub.NodeInfo(ctx)
	if err != nil {
		return fmt.Errorf("node info: %w", err)
	}
	free := info.FreeHugepageBytes()
	if info.Status != "healthy" {
		return fmt.Errorf("%w: node status %s", substrate.ErrCapacity, info.Status)
	}
	need := uint64(memoryMB) * 1024 * 1024
	if free < need {
		return fmt.Errorf("%w: %d bytes of hugepage memory free, need %d", substrate.ErrCapacity, free, need)
	}
	return nil
}

// classify decides a lease's class (#128 part 2): burst when the request
// forced it (preemptible even within the guarantee) or when the owner's
// guaranteed charge — the memory of its live *guaranteed* leases,
// other than self (a lease being re-admitted) — plus this lease's
// memoryMB would pass their guaranteed_mib; guaranteed otherwise,
// including every lease of a user without a guaranteed_mib (today's
// behaviour) and owners without an identity-store user (legacy
// consumer tokens). Burst leases do not count against the guarantee: an
// owner whose guaranteed leases went away gets the room back for the
// next lease, and promoteBurst moves running burst leases into it.
func (s *Service) classify(owner string, burst bool, memoryMB int, self string) string {
	if burst {
		return ClassBurst
	}
	if s.identities == nil {
		return ClassGuaranteed
	}
	u := s.identities.UserByID(owner)
	if u == nil || u.GuaranteedMiB <= 0 {
		return ClassGuaranteed
	}
	s.store.mu.Lock()
	charge := s.guaranteedMiBLocked(owner, self)
	s.store.mu.Unlock()
	if charge+memoryMB > u.GuaranteedMiB {
		return ClassBurst
	}
	return ClassGuaranteed
}

// guaranteedMiBLocked sums the memory of owner's live guaranteed leases,
// leaving out self. Caller holds the store lock.
func (s *Service) guaranteedMiBLocked(owner, self string) int {
	n := 0
	for _, l := range s.store.leases {
		if l.released || l.Owner != owner || !l.live() || l.ID == self || l.Class == ClassBurst {
			continue
		}
		n += l.MemoryMB
	}
	return n
}

// promoteBurst moves an owner's running burst leases to guaranteed,
// oldest first, while they fit their guaranteed_mib (#128). A lease
// created with "burst": true stays burst. Promotion changes bookkeeping
// only — the VM is untouched — and emits a "promoted" event. It runs
// when an owner's guaranteed lease goes (release, pause) and on the
// resume queue's tick, so the guarantee stays filled as leases churn.
func (s *Service) promoteBurst(owner string) {
	if s.identities == nil {
		return
	}
	u := s.identities.UserByID(owner)
	if u == nil || u.GuaranteedMiB <= 0 {
		return
	}
	s.store.mu.Lock()
	var cand []*Lease
	for _, l := range s.store.leases {
		if !l.released && l.Owner == owner && l.live() && !l.busy && l.Class == ClassBurst && !l.Burst {
			cand = append(cand, l)
		}
	}
	sort.Slice(cand, func(i, j int) bool { return cand[i].CreatedAt.Before(cand[j].CreatedAt) })
	charge := s.guaranteedMiBLocked(owner, "")
	var promoted []*Lease
	for _, l := range cand {
		if charge+l.MemoryMB > u.GuaranteedMiB {
			break
		}
		l.Class = ClassGuaranteed
		charge += l.MemoryMB
		s.saveLeaseLocked(l)
		promoted = append(promoted, l)
	}
	s.store.mu.Unlock()
	for _, l := range promoted {
		s.emitLeaseEvent(l.ID, l.Owner, LeasePromoted, "to guaranteed: the owner's guarantee has room")
	}
}

// promoteAllBurst runs promoteBurst for every owner with a running
// burst lease (the promote loop's tick).
func (s *Service) promoteAllBurst() {
	s.store.mu.Lock()
	owners := map[string]bool{}
	for _, l := range s.store.leases {
		if !l.released && l.live() && l.Class == ClassBurst && !l.Burst {
			owners[l.Owner] = true
		}
	}
	s.store.mu.Unlock()
	for o := range owners {
		s.promoteBurst(o)
	}
}

// promoteInterval is how often the promote loop fills every owner's
// guarantee as leases churn. It used to be the preemption resume queue's
// tick; it outlives that queue (#145 D2).
const promoteInterval = 15 * time.Second

// runPromoteLoop keeps every owner's guarantee filled as leases churn
// (#128): every promoteInterval it promotes running burst leases into
// the guarantee. It stops with ctx and skips while the node is draining.
func (s *Service) runPromoteLoop(ctx context.Context) {
	t := time.NewTicker(promoteInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.draining.Load() {
				continue
			}
			s.promoteAllBurst()
		}
	}
}

// TODO(FS2b-1 step 2): delete
func (s *Service) admitClass(ctx context.Context, owner string, memoryMB int, burst bool, self string) (string, error) {
	class := s.classify(owner, burst, memoryMB, self)
	if class != ClassBurst {
		// A guaranteed admission preempts burst leases when the node
		// cannot host it, then takes its hugepages from the cached
		// reading (#128 part 3).
		if err := s.admitGuaranteed(ctx, owner, memoryMB); err != nil {
			if s.metrics != nil && errors.Is(err, errPreemptCannot) {
				s.metrics.CapacityRej.Inc()
			}
			return class, err
		}
		return class, nil
	}

	s.nodeInfoMu.Lock()
	defer s.nodeInfoMu.Unlock()
	freeMiB, err := s.freeHugepageMiBLocked(ctx)
	if err != nil {
		if s.metrics != nil {
			s.metrics.CapacityRej.Inc()
		}
		return class, err
	}
	if reserve := uint64(s.burstReserveMiB()); freeMiB < reserve+uint64(memoryMB) {
		if s.metrics != nil {
			s.metrics.CapacityRej.Inc()
		}
		return class, fmt.Errorf("%w: a burst lease of %d MiB would leave the node under its %d MiB reserve (%d MiB free)",
			errBurstReserve, memoryMB, s.burstReserveMiB(), freeMiB)
	}
	s.debitNodeInfoLocked(memoryMB)
	return class, nil
}
