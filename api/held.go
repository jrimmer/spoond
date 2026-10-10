package api

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Pins and one clock (FS5, owner decision 2026-10-08, superseding every
// hold/held rule and the retention clocks): "We don't pre-emptively do
// anything. No auto idle pause, no different clocks. One behavior. A
// user can pin anything they want, even to the point of abuse, and
// spoond honors it. Never pausing. Pins don't pause. All paused VMs are
// deleted 30d from pause date."
//
//   - A pinned lease is never paused or deleted before its own expiry.
//     The lease's TTL still applies (pin and expiry work together); a
//     pinned persistent lease stays until the owner releases it. There
//     is no limit on how many leases an owner pins. Only the owner
//     (unpin, DELETE) changes a pin.
//   - Take-back (memory and disk) touches only unpinned leases. If
//     nothing unpinned can be taken for a request, the request answers
//     429 box_full and an alert is raised.
//   - Spoond acts only when a request needs room. There is no pressure
//     sweep, no idle pause of any kind and no proactive disk threshold.
//     Spoond's own garbage (orphan dirs, leftovers) is still cleaned as
//     housekeeping by the GC.
//   - One clock: every paused lease is released PAUSED_RELEASE_DAYS
//     (default 30) after its pause date, whatever paused it, preceded by
//     a lease.paused_expiring event 24 h before. Resuming clears the
//     date. Nothing else deletes on a timer.
//   - A lease's own TTL (unpinned or pinned) and its own idle_suspend
//     opt-in are unchanged and caller-chosen; a resumed lease comes back
//     on the next call.

// Defaults for FS5. cmd/spoond-backend maps the environment onto the
// ServiceConfig fields; 0 means the value below.
const (
	// DefaultPausedReleaseDays is PAUSED_RELEASE_DAYS: every paused lease
	// is released this many days after its pause date.
	DefaultPausedReleaseDays = 30
	// DefaultPinnedIdleNoticeDays is PINNED_IDLE_NOTICE_DAYS: a pinned
	// lease whose last API activity is older than this is flagged
	// (visibility only).
	DefaultPinnedIdleNoticeDays = 7
	// pausedExpiringLead is how long before a paused lease's release its
	// lease.paused_expiring warning is emitted.
	pausedExpiringLead = 24 * time.Hour
)

// heldActionSuspendIdle is the recorded action of an idle_suspend
// suspension, shared with the per-lease idle reclamation path.
const heldActionSuspendIdle = "suspend_idle"

// pausedRelease is the effective one-clock release age.
func (s *Service) pausedRelease() time.Duration {
	days := s.cfg.PausedReleaseDays
	if days <= 0 {
		days = DefaultPausedReleaseDays
	}
	return time.Duration(days) * 24 * time.Hour
}

// pinnedIdleNotice is the effective pinned-idle notice age.
func (s *Service) pinnedIdleNotice() time.Duration {
	days := s.cfg.PinnedIdleNoticeDays
	if days <= 0 {
		days = DefaultPinnedIdleNoticeDays
	}
	return time.Duration(days) * 24 * time.Hour
}

// setPinned pins or unpins a lease. A pin is the only thing that keeps
// spoond from pausing or deleting a lease before its own expiry; holder
// and holder_url are plain labels and never pin (FS5). Only the owner
// (or an admin, resolved by the route) changes a pin. Emits a pinned or
// unpinned event.
func (s *Service) setPinned(owner, id string, pinned bool) (*Lease, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l := s.store.leases[id]
	if l == nil || l.Owner != owner || l.released {
		return nil, errNotFound
	}
	if err := lostErr(l); err != nil {
		return nil, err
	}
	if l.Pinned == pinned {
		return l, nil
	}
	l.Pinned = pinned
	if !pinned {
		// Unpinning clears the idle-notice flag: it only describes a
		// pinned lease.
		l.PinnedIdleSince = time.Time{}
	}
	s.saveLeaseLocked(l)
	if pinned {
		s.emitLeaseEvent(id, owner, LeasePinned, "pinned; never paused or released before its own expiry")
	} else {
		s.emitLeaseEvent(id, owner, LeaseUnpinned, "unpinned; take-back may pause it again")
	}
	return l, nil
}

// unpinByHolderPrefix clears the pinned flag of every lease whose holder
// label starts with prefix, returning the count. It exists for the
// 2.9→3.0 window: migration 0022 turns every live hold into a pin, and
// pool-spawn held its worker leases by holder label. Admin only.
func (s *Service) unpinByHolderPrefix(ctx context.Context, prefix string) (int64, error) {
	n, err := s.db.UnpinLeasesByHolderPrefix(ctx, prefix)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		// The in-memory leases carry Pinned; reload them so the running
		// process agrees with the store.
		s.store.mu.Lock()
		for _, l := range s.store.leases {
			if !l.Pinned {
				continue
			}
			if hasPrefix(l.Holder, prefix) {
				l.Pinned = false
				l.PinnedIdleSince = time.Time{}
				s.saveLeaseLocked(l)
			}
		}
		s.store.mu.Unlock()
		s.emitLeaseEvent("", "", LeaseAdminUnpin, fmt.Sprintf("%d lease(s) unpinned by holder prefix %q", n, prefix))
	}
	return n, nil
}

// hasPrefix is strings.HasPrefix without importing strings here.
func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// leasePausedExpiredLocked reports whether l is due for its
// paused-release: unreleased, actually suspended, not busy, past its
// pause deadline. Call with s.store.mu held.
func (s *Service) leasePausedExpiredLocked(l *Lease, now time.Time, release time.Duration) bool {
	return !l.released && l.Suspended && !l.busy && !l.PausedAt.IsZero() &&
		!now.Before(l.PausedAt.Add(release))
}

// releaseIfPausedExpired releases l with the given reason only if it is
// still a suspended, unreleased, unbusy lease past its pause deadline —
// re-checked under the store lock at the moment of the release, so a
// resume (or a pause that never finished) landing between a sweep's
// collection pass and this call cannot delete a running lease
// (spoond-k0uz R3-3). It reports whether the release ran.
func (s *Service) releaseIfPausedExpired(ctx context.Context, l *Lease, now time.Time, reason string) bool {
	s.store.mu.Lock()
	due := s.leasePausedExpiredLocked(l, now, s.pausedRelease())
	s.store.mu.Unlock()
	if !due {
		return false
	}
	s.releaseBecause(ctx, l, reason)
	return true
}

// releasePausedLeases implements the one clock: every lease paused at
// least PAUSED_RELEASE_DAYS ago is released with reason paused_expired.
// The pause date is PausedAt, set by every pause (take-back, the lease's
// own idle_suspend, the admin drain and POST /pause); resuming clears
// it. A pin protects only a running VM: a paused pinned lease (the
// owner's own POST /pause, or a pinned lease a failed drain resume left
// suspended) is on the same clock, per the 2026-10-09 owner decision.
// Only a lease that is still suspended is acted on: a running lease with
// a stale PausedAt (a row a rolled-back 2.9 resumed) is never deleted
// for its pause date (spoond-k0uz R3-3). It does not run while draining.
func (s *Service) releasePausedLeases(ctx context.Context, now time.Time) {
	release := s.pausedRelease()
	if release <= 0 {
		return
	}
	var due []*Lease
	s.store.mu.Lock()
	for _, l := range s.store.leases {
		if !s.leasePausedExpiredLocked(l, now, release) {
			continue
		}
		due = append(due, l)
	}
	s.store.mu.Unlock()
	for _, l := range due {
		s.releaseIfPausedExpired(ctx, l, now, "paused_expired")
	}
}

// notifyPausedExpiring emits one lease.paused_expiring event 24 h before
// a paused lease is released, once per pause. A paused pinned lease is on
// the same clock, so it gets the same warning. Only a lease still
// suspended is announced (spoond-k0uz R3-3): a running lease's stale
// PausedAt warns about nothing. The lease's PausedExpiryNotified flag
// keeps a restart from repeating it; resuming clears the pause date and
// the flag.
func (s *Service) notifyPausedExpiring(ctx context.Context, now time.Time) {
	release := s.pausedRelease()
	if release <= pausedExpiringLead {
		return
	}
	lead := release - pausedExpiringLead
	var due []*Lease
	s.store.mu.Lock()
	for _, l := range s.store.leases {
		if l.released || !l.Suspended || l.PausedAt.IsZero() || l.PausedExpiryNotified {
			continue
		}
		elapsed := now.Sub(l.PausedAt)
		if elapsed >= lead && elapsed < release {
			l.PausedExpiryNotified = true
			s.saveLeaseLocked(l)
			due = append(due, l)
		}
	}
	s.store.mu.Unlock()
	for _, l := range due {
		s.emitLeaseEvent(l.ID, l.Owner, LeasePausedExpiring,
			fmt.Sprintf("paused since %s; released at %s unless resumed",
				l.PausedAt.Format(time.RFC3339), l.PausedAt.Add(release).Format(time.RFC3339)))
	}
}

// noticePinnedIdle implements the pinned-idle notice (FS5, visibility
// only): a pinned lease whose last API activity (LastActive — no
// heartbeat, no guest activity) is older than PINNED_IDLE_NOTICE_DAYS is
// flagged with PinnedIdleSince and emits one lease.pinned_idle event per
// crossing. Activity clears the flag. Nothing is paused, unpinned or
// released because of it.
func (s *Service) noticePinnedIdle(ctx context.Context, now time.Time) {
	notice := s.pinnedIdleNotice()
	if notice <= 0 {
		return
	}
	type crossing struct {
		l     *Lease
		idle  bool
		since time.Time
	}
	var crossings []crossing
	s.store.mu.Lock()
	for _, l := range s.store.leases {
		if l.released || !l.Pinned {
			continue
		}
		idle := !l.LastActive.IsZero() && now.Sub(l.LastActive) >= notice
		switch {
		case idle && l.PinnedIdleSince.IsZero():
			l.PinnedIdleSince = now
			s.saveLeaseLocked(l)
			crossings = append(crossings, crossing{l: l, idle: true, since: l.LastActive})
		case !idle && !l.PinnedIdleSince.IsZero():
			l.PinnedIdleSince = time.Time{}
			s.saveLeaseLocked(l)
			crossings = append(crossings, crossing{l: l})
		}
	}
	s.store.mu.Unlock()
	for _, c := range crossings {
		if c.idle {
			s.emitLeaseEvent(c.l.ID, c.l.Owner, LeasePinnedIdle,
				fmt.Sprintf("pinned and idle since %s (%s)", c.since.Format(time.RFC3339), now.Sub(c.since).Round(time.Second)))
		} else {
			s.emitLeaseEvent(c.l.ID, c.l.Owner, LeasePinnedIdle, "activity; pinned-idle notice cleared")
		}
	}
}

// pauseClock records the pause date and clears the expiring-notified
// flag for a lease being paused. Call with s.store.mu held.
func (s *Service) pauseClock(l *Lease, now time.Time) {
	l.PausedAt = now
	l.PausedExpiryNotified = false
}

// resumeClock clears a lease's pause date and the expiring-notified flag
// when it comes back running. Call with s.store.mu held.
func resumeClock(l *Lease) {
	l.PausedAt = time.Time{}
	l.PausedExpiryNotified = false
}

// errBoxFull is the take-back refusal: a request needs room, but every
// candidate that could be taken back is pinned. The lease API maps it
// to 429 box_full and raises an alert. A host-structural shortage
// (hugepages, the burst reserve, the snapshot disk floor, a draining
// node) is a wait, never box_full.
var errBoxFull = errors.New("box_full: nothing unpinned can be taken for this request")

// isBoxFull reports whether an error is the take-back box_full refusal.
func isBoxFull(err error) bool { return errors.Is(err, errBoxFull) }

// freePercent is the snapshot store's free space as a percentage of its
// capacity. ok is false when the stat fails or the capacity is 0 — the
// readiness check then fails rather than dividing by zero.
func (s *Service) freePercent(path string) (pct float64, ok bool) {
	total, free, err := s.diskCapacity(path)
	if err != nil || total == 0 {
		return 0, false
	}
	return float64(free) / float64(total) * 100, true
}
