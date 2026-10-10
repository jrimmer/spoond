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
// short-circuit fits applies.
func (s *Service) cachedFreeHugepageMiB(ctx context.Context) (uint64, error) {
	s.nodeInfoMu.Lock()
	defer s.nodeInfoMu.Unlock()
	return s.freeHugepageMiBLocked(ctx)
}

// preemptedCountLocked reports how many live leases carry a preemption
// stamp (for the gauge). Classes are gone, so nothing stamps it any
// more: pre-3.0 rows keep their historical value until the lease next
// leaves the suspended state, and the gauge reads 0 on any 3.0 box.
// Caller holds the store lock.
func (s *Service) preemptedCountLocked() int {
	n := 0
	for _, l := range s.store.leases {
		if !l.released && !l.PreemptedAt.IsZero() && l.State == "suspended" {
			n++
		}
	}
	return n
}

// takeableFreeableMiB is the memory of the running leases take-back may
// pause: live, idle, unpinned. It is an upper bound (the selector still
// leaves owners inside their slice alone).
func (s *Service) takeableFreeableMiB() uint64 {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	var n uint64
	for _, l := range s.store.leases {
		if l.released || l.busy || l.Suspended || !l.live() || l.Pinned {
			continue
		}
		n += uint64(l.MemoryMB)
	}
	return n
}

// pinnedFreeableMiB is the memory of the running leases that are pinned
// and so are not take-back candidates. It lets admitMemory tell "no room
// exists" from "every candidate is pinned".
func (s *Service) pinnedFreeableMiB() uint64 {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	var n uint64
	for _, l := range s.store.leases {
		if l.released || l.busy || l.Suspended || !l.live() || !l.Pinned {
			continue
		}
		n += uint64(l.MemoryMB)
	}
	return n
}
