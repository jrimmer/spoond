package api

import (
	"context"
	"errors"
	"time"
)

// Give-back (spoond-aeo6, owner 2026-10-17): a lease spoond's own
// take-back paused — the fair-share admission take-back or the
// low-memory floor, both suspend_reason take_back — is resumed by spoond
// once it is safe again. A pause spoond makes to protect itself is
// spoond's to undo: unattended work (a pool worker, a CI job) has no
// caller to wake it, and on 2026-10-17 two such leases sat paused for
// 8 h. A pause the caller made (a hand suspend, idle_suspend, the drain)
// is the caller's and still comes back only on its next call.
//
// The low-memory loop runs give-back on a tick that finds the box at or
// above the floor: at most one lease per tick, the longest-paused first
// and strictly in that order. It waits for comfortable room — the
// lease's memory plus the floor plus GIVE_BACK_MARGIN_MIB free once the
// running leases have faulted in all the memory they were granted, and
// inside the overcommit cap — gives way to creates waiting for
// admission, and stays quiet for two minutes after any take-back and
// fifteen after a floor take-back, so a lease is not handed back only to
// be taken again. The resume runs the path a holder's call runs (busy
// flag, drain gate, admission) and never takes back another lease to
// make its room.
//
// Flapping (spoond-90ia, 2026-10-17): an 8 GiB lease was taken and given
// back four times in 80 minutes, then two running leases died of
// hugepage exhaustion. Free memory read high because the running VMs had
// not yet touched their memory; the fit now counts that unfaulted
// headroom as spoken for. A lease taken back again after a give-back is
// also held for giveBackHoldBase, doubling with each re-take up to
// giveBackHoldMax.

const (
	// DefaultGiveBackMarginMiB is GIVE_BACK_MARGIN_MIB when unset: the
	// free memory give-back keeps on top of the lease and the floor.
	DefaultGiveBackMarginMiB = 1024
	// giveBackQuiet is how long after any take-back give-back waits.
	giveBackQuiet = 2 * time.Minute
	// giveBackFloorQuiet is how long after a floor take-back give-back
	// waits: the floor breached because running VMs were faulting in
	// memory, and they may not be done.
	giveBackFloorQuiet = 15 * time.Minute
	// giveBackHoldBase is the hold on a lease taken back again after a
	// give-back; each further re-take doubles it, up to giveBackHoldMax.
	giveBackHoldBase = 15 * time.Minute
	giveBackHoldMax  = 4 * time.Hour
	// giveBackStable is how long a given-back lease must stay running for
	// its next take-back to count as a fresh start, not a re-take.
	giveBackStable = time.Hour
	// giveBackRetryAfter is how long a lease whose give-back resume
	// failed is skipped.
	giveBackRetryAfter = 10 * time.Minute
	// giveBackTimeout bounds one give-back resume (a restore from a
	// snapshot is bounded work; an 8 GiB restore can take a minute
	// under load).
	giveBackTimeout = 5 * time.Minute
)

// noTakeBackKey marks an admission that must not take back anything for
// its room: a give-back resume fits in free memory or waits.
type noTakeBackKey struct{}

func withoutTakeBack(ctx context.Context) context.Context {
	return context.WithValue(ctx, noTakeBackKey{}, true)
}

// isGiveBack reports whether ctx is a give-back resume's.
func isGiveBack(ctx context.Context) bool {
	return ctx.Value(noTakeBackKey{}) != nil
}

// noteTakeBack stamps the time of a take-back of any kind: give-back
// stays quiet for giveBackQuiet after it, and for giveBackFloorQuiet
// after a floor take-back.
func (s *Service) noteTakeBack(floor bool) {
	now := s.now().UnixNano()
	s.lastTakeBackAt.Store(now)
	if floor {
		s.lastFloorTakeBackAt.Store(now)
	}
}

// holdGiveBackLocked starts a lease's give-back hold when it is taken
// back again after a give-back: giveBackHoldBase for the first re-take,
// doubling with each one after, capped at giveBackHoldMax. A lease that
// ran giveBackStable after its give-back starts the count afresh. Caller
// holds store.mu, with l.TakeBackAt already stamped.
func (s *Service) holdGiveBackLocked(l *Lease) {
	if l.givenBackAt.IsZero() {
		return
	}
	if l.TakeBackAt.Sub(l.givenBackAt) >= giveBackStable {
		l.giveBackRetakes = 0
	}
	l.giveBackRetakes++
	hold := giveBackHoldBase
	for i := 1; i < l.giveBackRetakes && hold < giveBackHoldMax; i++ {
		hold *= 2
	}
	hold = min(hold, giveBackHoldMax)
	l.giveBackHoldUntil = l.TakeBackAt.Add(hold)
	s.log.Printf("give back: lease %s taken back again (re-take %d); held for %s", l.ID, l.giveBackRetakes, hold)
}

// giveBackQuietFor reports whether give-back must stay quiet at now: a
// take-back of any kind in the last giveBackQuiet, or a floor take-back
// in the last giveBackFloorQuiet.
func (s *Service) giveBackQuietFor(now time.Time) bool {
	if last := s.lastTakeBackAt.Load(); last != 0 && now.Sub(time.Unix(0, last)) < giveBackQuiet {
		return true
	}
	last := s.lastFloorTakeBackAt.Load()
	return last != 0 && now.Sub(time.Unix(0, last)) < giveBackFloorQuiet
}

// giveBackMarginMiB is the configured margin (0 when unset reads as the
// default; a negative value means none).
func (s *Service) giveBackMarginMiB() int {
	switch m := s.cfg.GiveBackMarginMiB; {
	case m == 0:
		return DefaultGiveBackMarginMiB
	case m < 0:
		return 0
	default:
		return m
	}
}

// giveBackCandidate is the longest-paused lease spoond took back, or nil.
// Leases in their retry back-off or re-take hold, busy or still marked
// Drained are passed over (the undrain owns a drained lease's resume).
func (s *Service) giveBackCandidate(now time.Time) *Lease {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	var pick *Lease
	for id, l := range s.store.leases {
		if l.released || !l.Suspended || l.SuspendReason != suspendReasonTakeBack || l.busy || l.Drained {
			continue
		}
		if until, ok := s.giveBackSkip[id]; ok && now.Before(until) {
			continue
		}
		if now.Before(l.giveBackHoldUntil) {
			continue
		}
		if pick == nil || l.SuspendedAt.Before(pick.SuspendedAt) ||
			(l.SuspendedAt.Equal(pick.SuspendedAt) && l.ID < pick.ID) {
			pick = l
		}
	}
	return pick
}

// giveBackOne resumes at most one taken-back lease when the box has room
// for it (free and pool are this tick's fresh reading of free and total
// hugepage MiB, floor the configured floor). It reports whether a lease
// came back. Called by the low-memory loop only, so giveBackSkip has a
// single writer.
func (s *Service) giveBackOne(ctx context.Context, free, pool, floor int) bool {
	if s.cfg.GiveBackOff || s.draining.Load() {
		return false
	}
	now := s.now()
	if s.giveBackQuietFor(now) {
		return false
	}
	if s.queueDepth() > 0 {
		return false // waiting creates come first
	}
	l := s.giveBackCandidate(now)
	if l == nil {
		return false
	}
	// Strict order: when the longest-paused lease does not fit, nothing
	// younger jumps ahead of it. The running leases' memory they have not
	// faulted in yet (granted minus what the pool has handed out) is
	// spoken for: they can take it at any moment.
	unfaulted := max(0, s.runningMemoryMiB()-(pool-free))
	if free-unfaulted < l.MemoryMB+floor+s.giveBackMarginMiB() || s.overcommitShortfall(l.MemoryMB) > 0 {
		return false
	}
	paused := now.Sub(l.SuspendedAt).Round(time.Second)
	rctx, cancel := context.WithTimeout(withoutTakeBack(context.WithoutCancel(ctx)), giveBackTimeout)
	defer cancel()
	if _, err := s.resumeForUse(rctx, l); err != nil {
		if !errors.Is(err, errLeaseBusy) && !errors.Is(err, errLeaseReleased) {
			s.log.Printf("give back: lease %s (owner %s): %v; retrying in %s", l.ID, l.Owner, err, giveBackRetryAfter)
		}
		if s.giveBackSkip == nil {
			s.giveBackSkip = map[string]time.Time{}
		}
		s.giveBackSkip[l.ID] = now.Add(giveBackRetryAfter)
		return false
	}
	delete(s.giveBackSkip, l.ID)
	s.store.mu.Lock()
	l.givenBackAt = now
	s.store.mu.Unlock()
	if s.metrics != nil {
		s.metrics.MemoryGiveBacksTotal.Inc()
	}
	s.log.Printf("give back: lease %s (owner %s) resumed after %s, %d MiB free", l.ID, l.Owner, paused, free)
	return true
}
