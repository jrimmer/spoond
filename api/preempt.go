package api

import (
	"context"
	"fmt"
)

// Memory take-back helpers shared with the admission path (FS2a/FS2b,
// spoond-pxsn). The guaranteed/burst classes and the preemption that
// took back only burst leases are gone: every lease admits against the
// one hugepage pool and take-back (admitMemory, memVictims,
// takeBackPause) is the only way an over-full box makes room.

// fits reports whether the node's free hugepages (from the cache,
// already net of earlier admissions) can host memoryMB. An unhealthy
// node reports true: there is nothing to reclaim for, and the ordinary
// capacity check answers. A node that cannot be read reports false so
// the caller may try take-back; if nothing can be taken the plain
// capacity check still answers.
func (s *Service) fits(ctx context.Context, memoryMB int) (bool, error) {
	s.nodeInfoMu.Lock()
	defer s.nodeInfoMu.Unlock()
	freeMiB, err := s.freeHugepageMiBLocked(ctx)
	if err != nil {
		return false, err
	}
	if s.nodeInfoCache.Status != "healthy" {
		return true, nil
	}
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

// boxFullAlert logs and announces the box_full refusal (FS5, FS2b): a
// request needed room and take-back could serve none of it — every
// candidate was pinned, or the requester is itself the biggest
// borrower. why names which. Nothing was paused or released; an operator
// can free room by unpinning or releasing leases.
func (s *Service) boxFullAlert(owner string, memoryMB int, why string) {
	s.log.Printf("box_full: %d MiB request for %q cannot be admitted: %s; nothing was paused or released", memoryMB, owner, why)
	if s.metrics != nil {
		s.metrics.BoxFullTotal.Inc()
	}
	s.emitLeaseEvent("-", "", LeaseBoxFull, fmt.Sprintf("%d MiB request for %q: %s", memoryMB, owner, why))
}

// cachedFreeHugepageMiB reads the node's free hugepage memory in MiB from
// the service's cache (filling it on demand), without the healthy-status
// short-circuit fits applies.
func (s *Service) cachedFreeHugepageMiB(ctx context.Context) (uint64, error) {
	s.nodeInfoMu.Lock()
	defer s.nodeInfoMu.Unlock()
	return s.freeHugepageMiBLocked(ctx)
}
