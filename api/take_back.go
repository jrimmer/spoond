package api

import (
	"context"
	"errors"
	"sort"
	"time"
)

// Memory take-back (#145 FS2a). When a requester needs memory the box
// cannot hand out, running leases of owners who are over their fair slice
// are paused to make room. This file holds the pure victim selection
// (memVictims) and the stubs for the stateful steps.

// takeBackLease is one running lease as the selector sees it.
type takeBackLease struct {
	ID         string
	MemoryMiB  int
	Pinned     bool      // a pinned lease is never taken
	Busy       bool      // a running job (or in-flight operation); taken last
	LastActive time.Time // older means less recently used
}

// takeBackOwner is one owner's memory view: what they use now, their fair
// slice, and their running leases.
type takeBackOwner struct {
	Owner             string
	UsedMiB, SliceMiB int
	Leases            []takeBackLease
}

// takeBackVictim is one lease chosen to be paused. Ratio is the owner's
// used/slice at the moment of the pick, before the lease was given up.
type takeBackVictim struct {
	Owner, LeaseID string
	Ratio          float64
	MemoryMiB      int
}

func tbRatio(used, slice int) float64 {
	if slice <= 0 {
		return 0
	}
	return float64(used) / float64(slice)
}

// memVictims picks the leases to pause so requester can have needMiB.
//
// It repeats: take the owner with the highest used/slice ratio who is
// over their slice (used > slice, so an owner inside their slice is never
// touched) and whose ratio after giving up the candidate lease still
// stays above the requester's ratio after the request ((used+need)/slice
// of the requester). The point is to never push an owner below the
// requester, only to even them out. Within the owner the candidate is the
// least recently used unpinned lease; leases that are busy rank after
// every non-busy one; pinned leases are never taken. The owner's used is
// reduced after each pick. It stops as soon as the freed total reaches
// needMiB and returns nil when the need cannot be met (nothing is
// partially returned). A zero or negative slice has no ratio and is never
// a source. The requester is never its own victim.
func memVictims(owners []takeBackOwner, requester string, needMiB int) []takeBackVictim {
	if needMiB <= 0 {
		return nil
	}
	// Work on copies so the caller's views stay untouched.
	work := make([]takeBackOwner, len(owners))
	reqAfter := 0.0
	for i, o := range owners {
		o.Leases = append([]takeBackLease(nil), o.Leases...)
		work[i] = o
		if o.Owner == requester {
			reqAfter = tbRatio(o.UsedMiB+needMiB, o.SliceMiB)
		}
	}
	sort.SliceStable(work, func(i, j int) bool { return work[i].Owner < work[j].Owner })

	var out []takeBackVictim
	freed := 0
	for freed < needMiB {
		bestOwner, bestLease := -1, -1
		bestRatio := 0.0
		for i := range work {
			o := &work[i]
			if o.Owner == requester || o.SliceMiB <= 0 || o.UsedMiB <= o.SliceMiB {
				continue
			}
			li := tbPickLease(o.Leases)
			if li < 0 {
				continue
			}
			if tbRatio(o.UsedMiB-o.Leases[li].MemoryMiB, o.SliceMiB) <= reqAfter {
				continue
			}
			if r := tbRatio(o.UsedMiB, o.SliceMiB); bestOwner < 0 || r > bestRatio {
				bestOwner, bestLease, bestRatio = i, li, r
			}
		}
		if bestOwner < 0 {
			return nil
		}
		o := &work[bestOwner]
		l := o.Leases[bestLease]
		out = append(out, takeBackVictim{Owner: o.Owner, LeaseID: l.ID, Ratio: bestRatio, MemoryMiB: l.MemoryMiB})
		freed += l.MemoryMiB
		o.UsedMiB -= l.MemoryMiB
		o.Leases = append(o.Leases[:bestLease], o.Leases[bestLease+1:]...)
	}
	return out
}

// tbPickLease returns the index of the lease to take next: unpinned,
// non-busy before busy, least recently used first (ID breaks ties). -1
// when nothing is takeable.
func tbPickLease(ls []takeBackLease) int {
	best := -1
	for i, l := range ls {
		if l.Pinned {
			continue
		}
		if best < 0 {
			best = i
			continue
		}
		b := ls[best]
		switch {
		case l.Busy != b.Busy:
			if !l.Busy {
				best = i
			}
		case !l.LastActive.Equal(b.LastActive):
			if l.LastActive.Before(b.LastActive) {
				best = i
			}
		case l.ID < b.ID:
			best = i
		}
	}
	return best
}

// takeBackPause pauses one victim lease for the requester.
//
// It must re-check, in the same store-lock critical section in which it
// sets busy, that the lease is still running, unpinned, not released and
// not busy (a pin, release or job that landed after victim selection
// wins). Then it pauses the lease (pauseLeaseWith, reason take_back),
// emits lease.suspended with reason take_back carrying the victim's
// ratio and the requester, and marks the lease so the preempt auto-resume
// does not bring it back.
func (s *Service) takeBackPause(ctx context.Context, l *Lease, requester string, ratio float64) error {
	// TODO(FS2a step 2): implement per the doc comment.
	return errors.New("takeBackPause: not implemented")
}

// takeBackOwners builds the selector's view of every owner: memory used
// and slice from fairShares, plus the owner's running leases.
func (s *Service) takeBackOwners(ctx context.Context) []takeBackOwner {
	// TODO(FS2a step 3): for each owner in s.fairShares(ctx).owners take
	// Memory.UsedMiB / Memory.SliceMiB, and under s.store.mu list the
	// owner's live, unreleased leases as takeBackLease (MemoryMB,
	// Pinned, busy || s.runningJobs[id] > 0, LastActive).
	return nil
}
