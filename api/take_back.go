package api

import (
	"context"
	"fmt"
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
				// The LRU lease would drop this owner below the
				// requester: try the owner's leases in LRU order and
				// take the first that passes, so a smaller lease is
				// not lost just because a bigger one cannot go.
				li = tbPickLeaseFor(o, reqAfter)
				if li < 0 {
					continue
				}
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

// tbPickLeaseFor returns the first lease of o in take-back order (see
// tbPickLease) whose give-up keeps o's ratio above reqAfter; -1 when no
// lease of o passes.
func tbPickLeaseFor(o *takeBackOwner, reqAfter float64) int {
	order := make([]int, 0, len(o.Leases))
	for i := range o.Leases {
		order = append(order, i)
	}
	sort.SliceStable(order, func(a, b int) bool {
		x, y := o.Leases[order[a]], o.Leases[order[b]]
		if x.Pinned != y.Pinned {
			return !x.Pinned
		}
		if x.Busy != y.Busy {
			return !x.Busy
		}
		if !x.LastActive.Equal(y.LastActive) {
			return x.LastActive.Before(y.LastActive)
		}
		return x.ID < y.ID
	})
	for _, i := range order {
		if tbRatio(o.UsedMiB-o.Leases[i].MemoryMiB, o.SliceMiB) > reqAfter {
			return i
		}
	}
	return -1
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
// It re-checks, in the same store-lock critical section in which it
// marks the lease busy, that the lease is still a legal victim —
// running, unreleased, unpinned, not already suspended and not busy —
// so a pin, release, resume or job that landed between victim selection
// and this call wins and the pause is refused (errLeaseBusy). Setting
// busy under the same lock closes the gap: a later pin or exec sees
// busy and waits or fails instead of racing the pause. Then it runs the
// pause sub-work (pauseLeaseBody, reason take_back — takeBackPause owns
// the busy window), emits lease.suspended
// with reason take_back carrying the victim owner's ratio and the
// requester, and stamps the lease so no auto-resume brings it back into
// a take-back of the lease that displaced it (no ping-pong).
func (s *Service) takeBackPause(ctx context.Context, l *Lease, requester string, ratio float64) error {
	s.store.mu.Lock()
	if l.released || l.busy || l.Suspended || !l.live() || l.Pinned ||
		s.hasRunningJobLocked(l.ID) || l.Owner == requester {
		s.store.mu.Unlock()
		return errLeaseBusy
	}
	l.busy = true
	s.store.mu.Unlock()
	defer s.endBusy(l)

	if _, err := s.pauseLeaseBody(ctx, l, false, suspendPolicy{reason: suspendReasonTakeBack}); err != nil {
		return err
	}

	// Record the take-back like preemptLease records a preemption: the
	// stamp is re-checked under the lock, so a concurrent resume that
	// already brought the lease back leaves no stale record (the same
	// shape spoond-d76 pinned for the preemption stamp). We still own
	// the busy window here (busy is ours until endBusy), so busy being
	// set is expected; only a release or a resumed lease skips the stamp.
	s.store.mu.Lock()
	if !l.Suspended || l.released {
		s.store.mu.Unlock()
		return errLeaseBusy
	}
	l.TakeBackAt = s.now()
	l.TakeBackFor = requester
	l.TakeBackRatio = ratio
	l.LastAction, l.LastActionAt = pauseActionTakeBack, l.TakeBackAt
	s.saveLeaseLocked(l)
	s.store.mu.Unlock()

	if s.metrics != nil {
		s.metrics.TakeBacksTotal.Inc()
	}
	s.log.Printf("take back: lease %s (owner %s, ratio %.2f) paused for %s", l.ID, l.Owner, ratio, requester)
	s.emitLeaseEvent(l.ID, l.Owner, LeaseTakeBack,
		fmt.Sprintf("for %s: owner %.2f over their slice; it resumes on the holder's next work call", requester, ratio))
	// The pause already credited this lease's hugepages to the cached
	// reading and woke the admission queue (pauseLeaseBody).
	return nil
}

// takeBackOwners builds the selector's view of every owner: memory used
// and slice from fairShares, plus the owner's running leases. The fair
// snapshot supplies each owner's UsedMiB and SliceMiB; the store lock
// then lists the owner's live, unreleased leases (running only — a
// suspended lease holds no hugepages to take back). A lease is busy
// when it has an in-flight operation or a running background job: both
// rank last in the within-owner order. Callers must re-check every
// candidate at the moment of the pause (takeBackPause).
func (s *Service) takeBackOwners(ctx context.Context) []takeBackOwner {
	snap := s.fairShares(ctx)
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	out := make([]takeBackOwner, 0, len(snap.owners))
	byOwner := map[string]*takeBackOwner{}
	for _, o := range snap.owners {
		out = append(out, takeBackOwner{Owner: o.Owner, UsedMiB: o.Memory.UsedMiB, SliceMiB: o.Memory.SliceMiB})
	}
	for i := range out {
		byOwner[out[i].Owner] = &out[i]
	}
	for _, l := range s.store.leases {
		if l.released || l.Owner == "" || !l.live() {
			continue
		}
		v := byOwner[l.Owner]
		if v == nil {
			// A lease of an owner the fair snapshot does not know (an
			// owner added after it was computed, say): its memory is in
			// the box, so give it the owner row the snapshot missed.
			out = append(out, takeBackOwner{Owner: l.Owner})
			v = &out[len(out)-1]
			byOwner[l.Owner] = v
		}
		v.Leases = append(v.Leases, takeBackLease{
			ID:         l.ID,
			MemoryMiB:  l.MemoryMB,
			Pinned:     l.Pinned,
			Busy:       l.busy || s.hasRunningJobLocked(l.ID),
			LastActive: l.LastActive,
		})
	}
	return out
}
