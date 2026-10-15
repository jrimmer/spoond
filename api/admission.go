package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jrimmer/spoond/v3/substrate"
)

// nodeInfoCacheTTL bounds how long freeHugepageMiBLocked trusts its cached
// NodeInfo: long enough that a burst of admissions costs the
// orchestrator one round trip, short enough that a taken-back lease's
// credit cannot be raced past for long. The cache is filled on demand
// here and by the node gauges' loop — the same NodeInfo behind
// spoond_node_hugepages_free_bytes.
const nodeInfoCacheTTL = 15 * time.Second

// freeHugepageMiB reports the node's free hugepage memory in MiB, from
// the substrate's NodeInfo cached for at most nodeInfoCacheTTL, less
// what admissions have taken since that reading (see debitNodeInfoLocked).
// A failed refresh answers with the last good value — staleness beats a
// wrong refusal — and errors only when nothing has ever been cached.
// The caller holds nodeInfoMu across the call and its debit, on
// purpose: concurrent admissions then share one round trip instead of
// stampeding the orchestrator, and two admissions cannot both pass
// against the same reading.
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

// isHostMemoryExhausted reports whether err is the host running out of
// memory while the orchestrator started a sandbox: a snapshot load that
// cannot mmap its memfd, or a uffdio copy the kernel refused. The gRPC
// error carries no code for it, only text, so it is recognised by the
// kernel's message (case-insensitive, the case varies by source). Such a
// create or resume is a capacity refusal, not a failure: the node is
// out of room, so the caller waits like any other capacity answer.
func isHostMemoryExhausted(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "cannot allocate memory")
}

// debitNodeInfoLocked marks memoryMB MiB of hugepages used in the cached
// NodeInfo when a lease is admitted, so the next admission inside the
// same cache window sees them gone: without it, several admissions in a
// 15 s window would each pass against one reading and together
// overcommit the box. A debit for a create that then fails only makes
// admission stricter until the next refresh. No-op while nothing is
// cached. The caller holds nodeInfoMu.
func (s *Service) debitNodeInfoLocked(memoryMB int) {
	info := &s.nodeInfoCache
	if s.nodeInfoAt.IsZero() || info.HugepageSizeBytes == 0 || memoryMB <= 0 {
		return
	}
	bytes := uint64(memoryMB) * 1024 * 1024
	info.HugepagesUsed += (bytes + info.HugepageSizeBytes - 1) / info.HugepageSizeBytes
}

// admit checks that the node can host a sandbox of memoryMB MiB: enough
// free hugepages (D12), a healthy node, and — with the floor on
// (MEMORY_FLOOR_MIB, round 2) — the floor free on top of the request,
// the same headroom take-back stops on, so the final capacity check
// cannot grant what admission's take-back refused to free. Otherwise it
// returns substrate.ErrCapacity, which the handlers map to HTTP 503
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

// countHostENOMEM records one host-memory-exhausted substrate refusal:
// the same capacity-refusal counter admission's own refusals use (U11).
func (s *Service) countHostENOMEM(err error) {
	if s.metrics != nil {
		s.metrics.CapacityRej.Inc()
	}
	s.log.Printf("create: host memory exhausted (ENOMEM), answering capacity_wait: %v", err)
}

// noFloorKey marks a context whose admission skips the low-memory floor
// headroom: the lease was running a moment ago (a take-back pause that
// lost to a pin, an undrain after a restart), so bringing it back
// restores what the box already held instead of a new promise.
type noFloorKey struct{}

func withoutFloor(ctx context.Context) context.Context {
	return context.WithValue(ctx, noFloorKey{}, true)
}

// floorFor is the low-memory floor an admission on ctx keeps free on top
// of its request: MEMORY_FLOOR_MIB, or 0 for a lease returning to what
// the box already held (withoutFloor). Every admission step — fits,
// take-back's need and the final capacity check — reads it here.
func (s *Service) floorFor(ctx context.Context) int {
	if ctx.Value(noFloorKey{}) != nil {
		return 0
	}
	return s.cfg.MemoryFloorMiB
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
	floor := s.floorFor(ctx)
	needBytes := uint64(memoryMB+floor) * 1024 * 1024
	if free < needBytes {
		return fmt.Errorf("%w: %d bytes of hugepage memory free, need %d", substrate.ErrCapacity, free, needBytes)
	}
	return nil
}
