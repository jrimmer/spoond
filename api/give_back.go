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
// lease's memory plus the floor plus GIVE_BACK_MARGIN_MIB free, and
// inside the overcommit cap — gives way to creates waiting for
// admission, and stays quiet for two minutes after any take-back, so a
// lease is not handed back only to be taken again. The resume runs the
// path a holder's call runs (busy flag, drain gate, admission) and never
// takes back another lease to make its room.

const (
	// DefaultGiveBackMarginMiB is GIVE_BACK_MARGIN_MIB when unset: the
	// free memory give-back keeps on top of the lease and the floor.
	DefaultGiveBackMarginMiB = 1024
	// giveBackQuiet is how long after any take-back give-back waits.
	giveBackQuiet = 2 * time.Minute
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
// stays quiet for giveBackQuiet after it.
func (s *Service) noteTakeBack() {
	s.lastTakeBackAt.Store(s.now().UnixNano())
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
// Leases in their retry back-off, busy or still marked Drained are passed
// over (the undrain owns a drained lease's resume).
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
		if pick == nil || l.SuspendedAt.Before(pick.SuspendedAt) ||
			(l.SuspendedAt.Equal(pick.SuspendedAt) && l.ID < pick.ID) {
			pick = l
		}
	}
	return pick
}

// giveBackOne resumes at most one taken-back lease when the box has room
// for it (free is this tick's fresh reading, floor the configured floor).
// It reports whether a lease came back. Called by the low-memory loop
// only, so giveBackSkip has a single writer.
func (s *Service) giveBackOne(ctx context.Context, free, floor int) bool {
	if s.cfg.GiveBackOff || s.draining.Load() {
		return false
	}
	now := s.now()
	if last := s.lastTakeBackAt.Load(); last != 0 && now.Sub(time.Unix(0, last)) < giveBackQuiet {
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
	// younger jumps ahead of it.
	if free < l.MemoryMB+floor+s.giveBackMarginMiB() || s.overcommitShortfall(l.MemoryMB) > 0 {
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
	if s.metrics != nil {
		s.metrics.MemoryGiveBacksTotal.Inc()
	}
	s.log.Printf("give back: lease %s (owner %s) resumed after %s, %d MiB free", l.ID, l.Owner, paused, free)
	return true
}
