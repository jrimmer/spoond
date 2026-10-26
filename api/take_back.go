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
	Name       string // the lease's tag, echoed in the box_full own_leases body
	MemoryMiB  int
	Pinned     bool      // pinned or exec-pinned (pinnedLocked): fairness never takes it, the floor only below critical, last
	OwnPin     bool      // the owner's own pin (Lease.Pinned), for the box_full own_leases row
	ExecPinned bool      // an exec sent with "pin": true is in flight on it
	Busy       bool      // a running job (or in-flight operation); taken last
	Drained    bool      // an undrain still owns its resume; taken never
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
// Pinned, InsideSlice and NoSlice are set only by pressureVictim:
// Pinned for its last critical pass (the lease is pinned and the floor
// takes it anyway), InsideSlice when the owner was at or under their
// slice, NoSlice when the owner had none (a zero slice, no ratio).
type takeBackVictim struct {
	Owner, LeaseID string
	Ratio          float64
	MemoryMiB      int
	Pinned         bool
	InsideSlice    bool
	NoSlice        bool
}

func tbRatio(used, slice int) float64 {
	if slice <= 0 {
		return 0
	}
	return float64(used) / float64(slice)
}

// pressureVictim picks the one lease the low-memory loop pauses
// (MEMORY_FLOOR_MIB, no requester in mind). Owners are ranked by
// used/slice, highest first: the owners over their slice (used > slice,
// slice > 0), then — at the critical level only — the owners inside it
// by the same ratio, and last the owners with no slice (zero or
// negative: no ratio), by memory used. Inside an owner the pick is the
// least recently used lease that is not busy (tbPickLease over the
// non-busy leases). A lease still marked Drained (an undrain pending on
// it) is never picked: the undrain owns its resume, but the rest of the
// box stays under the floor. A single-lease owner over its slice IS a
// candidate: that lease is the one that goes. When the top owner has
// nothing to give, the next owner down the ranking is tried, and so on.
// The requester argument of memVictims does not exist here (the loop is
// nobody's fairness case), so unlike memVictims there is no after-pick
// ratio floor.
//
// Two levels (owner decision 2026-10-25):
//
//   - At the floor (critical false): only unpinned leases of owners
//     over their slice. Owners inside their slice and pinned leases are
//     never touched.
//   - Critical (free under MEMORY_CRITICAL_MIB): any owner, in three
//     passes — (a) unpinned leases of over-slice owners, as above; (b)
//     unpinned leases of the owners inside their slice (then the
//     no-slice owners); (c) pinned leases (explicitly or by an in-flight
//     pinned exec) over the whole ranking, over-slice owners first. A
//     later pass runs only when the earlier ones found nothing anywhere.
//
// The victim carries InsideSlice (its owner was at or under their slice
// at the pick), NoSlice (its owner had none) and Pinned (pass c), so the caller can let
// takeBackPauseWith take a pinned lease and say loudly what it took. ok
// is false when no lease the level allows is left: every one Drained or
// busy — busy leases are left out (see below), pinned or not — or, at
// the floor, no owner over their slice.
func pressureVictim(owners []takeBackOwner, critical bool) (takeBackVictim, bool) {
	// Rank by ratio, highest first (stable, so a tie keeps the view's
	// owner order); the no-slice owners follow by memory used.
	type cand struct {
		idx   int
		ratio float64
	}
	var over, inside, noSlice []cand
	for i := range owners {
		o := &owners[i]
		r := tbRatio(o.UsedMiB, o.SliceMiB)
		switch {
		case o.SliceMiB <= 0:
			noSlice = append(noSlice, cand{idx: i, ratio: r})
		case o.UsedMiB > o.SliceMiB:
			over = append(over, cand{idx: i, ratio: r})
		default:
			inside = append(inside, cand{idx: i, ratio: r})
		}
	}
	byRatio := func(cs []cand) {
		sort.SliceStable(cs, func(a, b int) bool { return cs[a].ratio > cs[b].ratio })
	}
	byRatio(over)
	byRatio(inside)
	sort.SliceStable(noSlice, func(a, b int) bool { return owners[noSlice[a].idx].UsedMiB > owners[noSlice[b].idx].UsedMiB })
	rest := append(inside, noSlice...)

	type pass struct {
		ranked []cand
		pinned bool // this pass takes pinned leases (and only then)
	}
	passes := []pass{{ranked: over}}
	if critical {
		all := append(append([]cand(nil), over...), rest...)
		passes = append(passes, pass{ranked: rest}, pass{ranked: all, pinned: true})
	}
	for _, p := range passes {
		for _, c := range p.ranked {
			o := &owners[c.idx]
			// Busy leases are left out, not ranked last: takeBackPause
			// refuses a busy lease, so offering one would stall every
			// tick on the same refusal and never reach the next owner
			// (or the alert when the whole box is busy).
			idle := make([]takeBackLease, 0, len(o.Leases))
			for _, l := range o.Leases {
				if l.Busy {
					continue
				}
				if p.pinned {
					if !l.Pinned {
						continue // the unpinned passes had it first
					}
					// tbPickLease skips pinned leases; hand it this
					// pass's copy unpinned.
					l.Pinned = false
				}
				idle = append(idle, l)
			}
			li := tbPickLease(idle)
			if li < 0 {
				continue // nothing of the owner this pass may take
			}
			l := idle[li]
			return takeBackVictim{
				Owner: o.Owner, LeaseID: l.ID, Ratio: c.ratio, MemoryMiB: l.MemoryMiB,
				Pinned: p.pinned, NoSlice: o.SliceMiB <= 0,
				InsideSlice: o.SliceMiB > 0 && o.UsedMiB <= o.SliceMiB,
			}, true
		}
	}
	return takeBackVictim{}, false
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
// unpinned lease. Note the own_leases listing in the box_full refusal
// takes the same rows without the selector's skipping. Leases that are
// busy rank after every non-busy one; pinned leases are never taken.
// The owner's used is reduced after each pick. It stops as soon as the
// freed total reaches needMiB and returns nil when the need cannot be
// met (nothing is partially returned). A zero or negative slice has no
// ratio and is never a source. The requester is never its own victim.
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
		if o.Leases[i].Pinned || o.Leases[i].Drained {
			// A pinned lease is never a take-back candidate (FS5), and
			// neither is a Drained one (its undrain owns the resume), not
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
// not Drained (an undrain owns its resume), non-busy before busy, least
// recently used first (ID breaks ties). -1 when nothing is takeable.
func tbPickLease(ls []takeBackLease) int {
	best := -1
	for i, l := range ls {
		if l.Pinned || l.Drained {
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
// A pin that lands DURING the pause body still wins (except under the
// floor's last resort, takeBackPauseWith): after the pause
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
//
// Admission take-back calls it this way and never takes a pinned lease;
// the low-memory floor's last resort uses takeBackPauseWith instead.
func (s *Service) takeBackPause(ctx context.Context, l *Lease, requester string, ratio float64) error {
	_, err := s.takeBackPauseWith(ctx, l, requester, ratio, false)
	return err
}

// takeBackPauseWith is takeBackPause with the floor's last-resort
// policy: takePinned lets a pinned lease (explicitly or by an in-flight
// pinned exec) be the victim, and the post-pause pin re-check then does
// not un-do the pause — the floor took the lease below the critical
// level because no unpinned lease of any owner was left, and resuming
// it at once would put the box back under the floor. Every other rule (busy, a running job, Drained in
// the views, the requester's own lease) still holds. Only the
// low-memory floor passes takePinned; fairness never does. pinned
// reports whether the lease was pinned when the pause took it (the
// guard's own reading, the one the start line logs), so the caller's
// own PINNED log and alert match it.
func (s *Service) takeBackPauseWith(ctx context.Context, l *Lease, requester string, ratio float64, takePinned bool) (pinned bool, err error) {
	s.store.mu.Lock()
	if l.released || l.busy || l.Suspended || !l.live() || (l.pinnedLocked() && !takePinned) ||
		s.hasRunningJobLocked(l.ID) || l.Owner == requester {
		s.store.mu.Unlock()
		return false, errLeaseBusy
	}
	pinned = l.pinnedLocked()
	l.busy = true
	s.store.mu.Unlock()
	defer s.endBusy(l)
	// Logged before the pause, not only after it (#166): a pause that
	// ends early (the lease released mid-pause, a failed snapshot)
	// leaves no success line, and without this one nothing would say
	// why the lease's work was cut.
	if pinned {
		s.log.Printf("take back: pausing lease %s (owner %s, ratio %.2f) for %s; it is PINNED (last resort below the critical level, no unpinned lease left)", l.ID, l.Owner, ratio, requester)
	} else {
		s.log.Printf("take back: pausing lease %s (owner %s, ratio %.2f) for %s", l.ID, l.Owner, ratio, requester)
	}

	buildID, err := s.pauseLeaseBody(ctx, l, false, suspendPolicy{reason: suspendReasonTakeBack, takeBackRatio: ratio, takeBackFor: requester})
	if err != nil {
		return pinned, err
	}
	s.noteTakeBack(requester == pressureRequester)

	// Record the take-back stamp: the stamp is re-checked under the
	// lock, so a concurrent resume that already brought the lease back
	// leaves no stale record (the same shape spoond-d76 pinned for the
	// old preemption stamp). This critical
	// section also settles the pin-during-pause race: busy is still
	// ours, so a pin that landed while the pause ran is visible here and
	// un-does the pause (a pinned lease is never left suspended for
	// take-back). We still own the busy window (until endBusy), so busy
	// being set is expected; only a release or a resumed lease skips it.
	s.store.mu.Lock()
	if !l.Suspended || l.released {
		s.store.mu.Unlock()
		return pinned, errLeaseBusy
	}
	if (l.pinnedLocked() && !takePinned) || s.hasRunningJobLocked(l.ID) {
		// The owner pinned the lease (or a pinned exec began, or a job
		// started on it) while the pause was in flight: the pin wins,
		// the lease is brought back at once and the caller counts
		// nothing as freed. The immediate
		// resume stays inside this critical section's protection: busy
		// blocks every other operation until endBusy below, so nothing
		// can interleave on the lease between this check and the resume.
		s.store.mu.Unlock()
		if err := s.unTakeBackResume(ctx, l, buildID); err != nil {
			// The box cannot host the lease again right now: the pin
			// stands, the lease waits suspended for capacity like any
			// pinned pause (the holder's next work call resumes it) and
			// it stays pinned and suspended, with SuspendReason take_back so
			// the resume-on-use path carries the detail. Nothing counts
			// as freed: the pause's credit is still in the cache and
			// covers the suspended lease's memory. Busy is still set:
			// the lease stays pinned and suspended, and the caller's
			// defer keeps clearing busy.
			s.log.Printf("take back: lease %s pinned during its pause but not resumable: %v", l.ID, err)
			return pinned, errLeaseBusy
		}
		s.log.Printf("take back: lease %s was pinned mid-pause; resumed at once, nothing taken", l.ID)
		return pinned, errLeaseBusy
	}
	l.TakeBackAt = s.now()
	l.TakeBackFor = requester
	l.TakeBackRatio = ratio
	s.holdGiveBackLocked(l)
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
	return pinned, nil
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
// becoming running, and it is the ONLY endBusy on this path: neither
// the failure return nor the success return clears busy here, so
// takeBackPause's single defer owns it end to end and cannot clear a
// busy flag a holder resume set in between.
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
	// The lease was running a moment ago: its return is not held to the
	// floor headroom, or a pin that won the race could leave it paused.
	if _, err := s.resumeLeaseBody(withoutFloor(ctx), l); err != nil {
		// Busy stays set: the lease is suspended and unchanged, and
		// takeBackPause's single defer clears it. Clearing it here too
		// would drop the flag early and open the busy window while this
		// call is still on the stack.
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
			Name:       l.Name,
			MemoryMiB:  l.MemoryMB,
			Pinned:     l.pinnedLocked(),
			OwnPin:     l.Pinned,
			ExecPinned: l.execPins > 0,
			Busy:       l.busy || s.hasRunningJobLocked(l.ID),
			Drained:    l.Drained,
			LastActive: l.LastActive,
		})
	}
	return out
}

// takeBackOwnersFor is takeBackOwners plus the memory still in flight:
// every owner's admitted-but-not-yet-live reservation (store.pendingMiB)
// counts toward its usage, so an owner whose creates are mid-flight
// cannot read a falsely low ratio and take back from others (or slip
// past the biggest-borrower refusal). The requester's own pending
// reservation already holds this request's requestMiB (reserveQuota runs
// before admission) and memVictims / biggestBorrowerRefusal add the
// request themselves, so that much is left out for the requester. A
// lease just granted is briefly counted twice (pending and live) until
// its creator releases the reservation; that over-states an owner by
// one lease for that moment, never under-states it.
func (s *Service) takeBackOwnersFor(ctx context.Context, requester string, requestMiB int) []takeBackOwner {
	owners := s.takeBackOwners(ctx)
	s.store.mu.Lock()
	pending := make(map[string]int, len(s.store.pendingMiB))
	for o, n := range s.store.pendingMiB {
		pending[o] = n
	}
	s.store.mu.Unlock()
	if own := pending[requester]; own > 0 {
		if requestMiB > own {
			requestMiB = own
		}
		pending[requester] = own - requestMiB
	}
	byOwner := make(map[string]int, len(owners))
	for i, o := range owners {
		byOwner[o.Owner] = i
	}
	for o, n := range pending {
		if n <= 0 {
			continue
		}
		if i, ok := byOwner[o]; ok {
			owners[i].UsedMiB += n
		}
	}
	return owners
}
