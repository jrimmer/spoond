package api

import (
	"context"
	"fmt"
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
	info := s.nodeInfoCache
	taken := info.HugepagesUsed + info.HugepagesReserved
	if taken >= info.HugepagesTotal {
		return 0, nil
	}
	return (info.HugepagesTotal - taken) * info.HugepageSizeBytes / (1024 * 1024), nil
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
	free := (info.HugepagesTotal - info.HugepagesUsed - info.HugepagesReserved) * info.HugepageSizeBytes
	if info.Status != "healthy" {
		return fmt.Errorf("%w: node status %s", substrate.ErrCapacity, info.Status)
	}
	need := uint64(memoryMB) * 1024 * 1024
	if free < need {
		return fmt.Errorf("%w: %d bytes of hugepage memory free, need %d", substrate.ErrCapacity, free, need)
	}
	return nil
}

// classify decides a lease's class (#128 part 2): burst when the
// request forced it (preemptible even within the guarantee) or when the
// owner's running charge — the leases live now plus the reservations
// in flight, this lease already among them — passes their
// guaranteed_mib; guaranteed otherwise — including every lease of a
// user without a guaranteed_mib, which keeps today's behaviour, and
// owners without an identity-store user (legacy consumer tokens). The
// charge is read at the moment of the decision: the first lease past
// the guarantee is the one that bursts.
func (s *Service) classify(owner string, burst bool) string {
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
	if s.usedMiB(owner) > u.GuaranteedMiB {
		return ClassBurst
	}
	return ClassGuaranteed
}

// admitClass decides a lease's class and holds a burst lease to the
// burst reserve (#128 part 2). It returns the class — to be stamped on
// the lease when admitted — and the refusal, if any: a burst lease is
// admitted only while the node's free hugepages stay above
// BurstReserveMiB after its own. The plain hugepage capacity check is
// not repeated here: createSandbox runs it for every cold create, and
// a guaranteed lease's admission is exactly what the reserve protects.
func (s *Service) admitClass(ctx context.Context, owner string, memoryMB int, burst bool) (string, error) {
	class := s.classify(owner, burst)
	s.nodeInfoMu.Lock()
	defer s.nodeInfoMu.Unlock()
	if class != ClassBurst {
		// A guaranteed admission skips the reserve but still takes its
		// hugepages from the cached reading.
		s.debitNodeInfoLocked(memoryMB)
		return class, nil
	}
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
