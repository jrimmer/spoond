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
// over their slice (used > slice, so an owner inside their slice is
// never touched) and whose ratio after giving up the candidate lease
// still stays above the requester's ratio after their whole request
// ((used+requestMiB)/slice of the requester — the take-back is for the
// request, even when only the needMiB shortfall is still missing). The
// point is to never push an owner below the requester, only to even
// them out. Within the owner the candidate is the least recently used
// unpinned lease; leases that are busy rank after every non-busy one;
// pinned leases are never taken. The owner's used is reduced after each
// pick. It stops as soon as the freed total reaches needMiB and returns
// nil when the need cannot be met (nothing is partially returned). A
// zero or negative slice has no ratio and is never a source. The
// requester is never its own victim.
func memVictims(owners []takeBackOwner, requester string, needMiB, requestMiB int) []takeBackVictim {
	if needMiB <= 0 {
		return nil
	}
	if requestMiB < needMiB {
		// The ratio must never assume less work for the requester than
		// the take-back is actually for.
		requestMiB = needMiB
	}
	// Work on copies so the caller's views stay untouched.
	work := make([]takeBackOwner, len(owners))
	reqAfter := 0.0
	for i, o := range owners {
		o.Leases = append([]takeBackLease(nil), o.Leases...)
		work[i] = o
		if o.Owner == requester {
			reqAfter = tbRatio(o.UsedMiB+requestMiB, o.SliceMiB)
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
		if o.Leases[i].Pinned {
			// A pinned lease is never a take-back candidate (FS5), not
			// even in the fall-through: sorting it last is not enough,
			// it must be skipped outright.
			continue
		}
		order = append(order, i)
	}
	sort.SliceStable(order, func(a, b int) bool {
		x, y := o.Leases[order[a]], o.Leases[order[b]]
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
// busy and waits or fails instead of racing the pause.
//
// A pin that lands DURING the pause body still wins: after the pause
// returns, the same lock re-checks pinned and running jobs, and a lease
// that became pinned (or gained a job) while paused for take-back is
// resumed at once and counted as NOT freed — a pin protects a running
// VM, so it un-does the pause (FS5). The un-doing resume leaves busy
// set, so the pin that won the race cannot be answered by a busy
// refusal in the gap before the lease is running again: the caller's
// endBusy clears busy in the same breath the lease becomes running.
// The re-resume fails only when the box cannot host the lease again;
// the pin survives suspended then, and the caller's free-memory
// re-check sees the smaller credit.
//
// The successful pause emits lease.suspended with reason take_back
// carrying the victim owner's ratio and the requester, and stamps the
// lease so no auto-resume brings it back into a take-back of the lease
// that displaced it (no ping-pong).
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

	buildID, err := s.pauseLeaseBody(ctx, l, false, suspendPolicy{reason: suspendReasonTakeBack, takeBackRatio: ratio, takeBackFor: requester})
	if err != nil {
		return err
	}

	// Record the take-back like preemptLease records a preemption: the
	// stamp is re-checked under the lock, so a concurrent resume that
	// already brought the lease back leaves no stale record (the same
	// shape spoond-d76 pinned for the preemption stamp). This critical
	// section also settles the pin-during-pause race: busy is still
	// ours, so a pin that landed while the pause ran is visible here and
	// un-does the pause (a pinned lease is never left suspended for
	// take-back). We still own the busy window (until endBusy), so busy
	// being set is expected; only a release or a resumed lease skips it.
	s.store.mu.Lock()
	if !l.Suspended || l.released {
		s.store.mu.Unlock()
		return errLeaseBusy
	}
	if l.Pinned || s.hasRunningJobLocked(l.ID) {
		// The owner pinned the lease (or started a job on it) while the
		// pause was in flight: the pin wins, the lease is brought back at
		// once and the caller counts nothing as freed. The immediate
		// resume stays inside this critical section's protection: busy
		// blocks every other operation until endBusy below, so nothing
		// can interleave on the lease between this check and the resume.
		s.store.mu.Unlock()
		if err := s.unTakeBackResume(ctx, l, buildID); err != nil {
			// The box cannot host the lease again right now: the pin
			// stands, the lease waits suspended for capacity like any
			// pinned pause (the holder's next work call resumes it) and
			// it stays pinned and suspended, stamped for take-back so
			// the resume-on-use path carries the detail. Nothing counts
			// as freed: the pause's credit is still in the cache and
			// covers the suspended lease's memory. unTakeBackResume
			// already cleared busy (nothing changed that it protects).
			s.log.Printf("take back: lease %s pinned during its pause but not resumable: %v", l.ID, err)
			return errLeaseBusy
		}
		s.log.Printf("take back: lease %s was pinned mid-pause; resumed at once, nothing taken", l.ID)
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

// unTakeBackResume brings a lease paused for take-back back at once,
// inside the caller's busy window: a pin that landed mid-pause must
// leave the lease running again, not suspended (FS5: pins don't pause).
// It is the sub work of resumeLease with the memory re-check, minus the
// quota reservation it does not need (no admission class change: the
// lease keeps what it had). Because it skips resumeLease's admission, it
// debits the cached node reading itself: the pause credited the lease's
// hugepages, and the sandbox is back now, so the cache must give them up
// again — the same debit resumeLease's admitClass path performs. Without
// it the caller believes more memory is free than the box has and
// refuses a request that its next victim could have served.
//
// On success it leaves the lease busy: the whole point is that no pin,
// exec or release can slip in between the re-check that decided to
// un-do the pause and the moment the lease is running again — the
// caller's endBusy closes that window atomically with the lease
// becoming running. It clears busy itself only when the resume failed
// (the lease stays suspended, so nothing changed that a busy flag needs
// to protect).
func (s *Service) unTakeBackResume(ctx context.Context, l *Lease, buildID string) error {
	if s.leaseReleased(l) {
		// A release slipped past the busy flag before it was set:
		// pauseLeaseBody will return errLeaseReleased at its own
		// re-check and endBusy is already the release's. Un-dosing the
		// pause would stop the fresh sandbox of a lease the owner no
		// longer has (the lease's own busy must not gate the cleanup of
		// a released lease, spoond-775).
		return errLeaseReleased
	}
	l.ResumeBuildID = buildID
	if _, err := s.resumeLeaseBody(ctx, l); err != nil {
		s.endBusy(l)
		return err
	}
	s.nodeInfoMu.Lock()
	s.debitNodeInfoLocked(l.MemoryMB)
	s.nodeInfoMu.Unlock()
	s.writeGeneration(l)
	// busy stays set: the caller's endBusy clears it now that the lease
	// is running, so no operation can interleave on it in between.
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
	// Owner -> index into out. Pointers into out would go stale when a
	// lease of an unknown owner appends a row and out reallocates: the
	// earlier appends would take through a dangling copy and their
	// leases would be lost.
	byOwner := map[string]int{}
	for _, o := range snap.owners {
		byOwner[o.Owner] = len(out)
		out = append(out, takeBackOwner{Owner: o.Owner, UsedMiB: o.Memory.UsedMiB, SliceMiB: o.Memory.SliceMiB})
	}
	for _, l := range s.store.leases {
		if l.released || l.Owner == "" || !l.live() {
			continue
		}
		i, ok := byOwner[l.Owner]
		if !ok {
			// A lease of an owner the fair snapshot does not know (an
			// owner added after it was computed, say): its memory is in
			// the box, so give it the owner row the snapshot missed.
			i = len(out)
			byOwner[l.Owner] = i
			out = append(out, takeBackOwner{Owner: l.Owner})
		}
		v := &out[i]
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
