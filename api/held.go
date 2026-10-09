package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// Limits on held leases that act automatically (2.1, #111 follow-up).
//
// Owner decision 2026-10-03: a held lease must never be able to keep
// memory or disk forever, and nobody watches the dashboard — so the
// limits act on their own. Every automatic action is logged (one line
// naming the lease, the holder, the rule and its numbers), counted in
// spoond_held_actions_total{rule,action}, and recorded on the lease
// (last_action, last_action_at, returned by the lease API).
//
// The rules run in the existing sweep tick (sweepExpired) and skip
// while the node is draining. A hold also lapses on its own: it lasts
// HoldTTL from when it was set or renewed, at most HoldTTLMax (for an
// explicit hold_ttl). A lapsed hold suspends a running lease and never
// releases one: the lease stays held (no expiry, so the TTL sweep never
// touches it), rules 1 and 2 still apply, and renewing restores a
// normal hold. Rule 4 shortens rule 1's idle threshold under pressure;
// rule 5 releases leases a rule suspended when the snapshot disk runs
// critical, oldest suspension first.
//
// Nothing running is ever released automatically: rules 2 and 5 only
// take leases that a rule suspended (idle, pressure or a lapse) and
// that saw no activity since.

// Held-lease rule names, as used in log lines, the
// spoond_held_actions_total{rule} label and the recorded last_action.
const (
	heldRuleIdle     = "idle"     // rule 1: idle held lease suspended
	heldRuleStale    = "stale"    // rule 2: suspended held lease released
	heldRuleExpiry   = "expiry"   // rule 3: hold lapsed; a running lease is suspended
	heldRulePressure = "pressure" // rule 4: pressure shortens rule 1
	heldRuleCritical = "critical" // rule 5: critical disk releases suspended leases
)

// Held-lease action names, as used in the
// spoond_held_actions_total{action} label and the recorded last_action.
const (
	heldActionRelease      = "release"
	heldActionExpire       = "expire"
	heldActionSuspendIdle  = "suspend_idle"
	heldActionSuspendLapse = "suspend_lapsed"
)

// Defaults for the held-lease limits (owner decision 2026-10-03).
// cmd/spoond-backend maps the environment onto them, so an unset
// variable means the value below; a ServiceConfig field of 0 disables
// the rule it configures.
const (
	DefaultHeldIdleTimeout      = 4 * time.Hour       // HELD_IDLE_TIMEOUT_SECS (14400)
	DefaultHeldSuspendedRelease = 7 * 24 * time.Hour  // HELD_SUSPENDED_RELEASE_SECS (604800)
	DefaultHoldTTL              = 7 * 24 * time.Hour  // HOLD_TTL_SECS (604800)
	DefaultHoldTTLMax           = 30 * 24 * time.Hour // HOLD_TTL_MAX_SECS (2592000)
	DefaultPressureDiskFreePct  = 15                  // PRESSURE_DISK_FREE_PCT
	DefaultPressureHeldIdle     = 30 * time.Minute    // PRESSURE_HELD_IDLE_SECS (1800)
	DefaultCriticalDiskFreePct  = 5                   // CRITICAL_DISK_FREE_PCT
	DefaultCriticalRecoverPct   = 10                  // CRITICAL_DISK_RECOVER_PCT
)

// heldAction records one automatic action on a held lease: the log
// line (lease, holder, rule, action and the numbers that triggered
// it), the spoond_held_actions_total{rule,action} counter, the
// lease's last_action/last_action_at record (persisted) and one
// held_action event on the bus (detail = rule, action and numbers).
// now is the instant of the action; detail is the human-readable
// numbers. Call with s.store.mu held (like setHoldLocked).
func (s *Service) heldAction(ctx context.Context, l *Lease, rule, action, detail string, now time.Time) {
	if l.released {
		// A release after the rule's pause returned success but before
		// this record must not bump a counter, save or emit an event for
		// a released lease (spoond-15i).
		return
	}
	if l.Holder != "" {
		s.log.Printf("held lease %s (holder %q): %s %s: %s", l.ID, l.Holder, rule, action, detail)
	} else {
		s.log.Printf("held lease %s: %s %s: %s", l.ID, rule, action, detail)
	}
	if s.metrics != nil {
		s.metrics.HeldActions.WithLabelValues(rule, action).Inc()
	}
	l.LastAction = rule + "/" + action
	l.LastActionAt = now
	s.saveLeaseLocked(l)
	s.emitLeaseEvent(l.ID, l.Owner, LeaseHeldAction, rule+"/"+action+": "+detail)
}

// setHold timesets a hold on a lease: holder fields (already validated)
// plus HoldSetAt/HoldExpiresAt. An explicit holdTTL > 0 is capped at
// HoldTTLMax and starts now; otherwise the hold lasts the default
// HoldTTL. Call with s.store.mu held.
func (s *Service) setHoldLocked(l *Lease, holder, holderURL string, holdTTL time.Duration, now time.Time) {
	l.Holder = holder
	l.HolderUrl = holderURL
	l.HoldSetAt = now
	ttl := s.cfg.HoldTTL
	if ttl <= 0 {
		ttl = DefaultHoldTTL
	}
	if holdTTL > 0 {
		max := s.cfg.HoldTTLMax
		if max <= 0 {
			max = DefaultHoldTTLMax
		}
		if holdTTL > max {
			holdTTL = max
		}
		ttl = holdTTL
	}
	l.HoldExpiresAt = now.Add(ttl)
	l.HoldTTL = ttl // what this hold was actually granted (persisted)
}

// clearHold removes the hold: holder fields and hold timestamps are
// reset (HoldTTL back to "the default") and the lease follows the
// normal TTL and idle rules from then on. Call with s.store.mu held.
func clearHoldLocked(l *Lease) {
	l.Holder = ""
	l.HolderUrl = ""
	l.HoldSetAt = time.Time{}
	l.HoldExpiresAt = time.Time{}
	l.HoldTTL = 0
}

// setHolderWithTTL sets or clears a lease's holder fields. Both empty
// clears the holder and restores normal sweeping. The route admits the
// owner and admins; this function checks the owner it is given. A
// non-empty holder starts (or restarts) the hold's clock; holdTTL > 0
// sets an explicit lifetime, capped at HoldTTLMax.
func (s *Service) setHolderWithTTL(owner, id, holder, holderURL string, holdTTL time.Duration) (*Lease, error) {
	if err := validateHolder(holder, holderURL); err != nil {
		return nil, err
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l := s.store.leases[id]
	if l == nil || l.Owner != owner || l.released {
		return nil, errNotFound
	}
	if err := lostErr(l); err != nil {
		return nil, err
	}
	now := s.now()
	if holder == "" {
		clearHoldLocked(l)
		s.emitLeaseEvent(id, owner, LeaseHolderCleared, "hold cleared")
	} else {
		s.setHoldLocked(l, holder, holderURL, holdTTL, now)
		s.emitLeaseEvent(id, owner, LeaseHolderSet, fmt.Sprintf("held by %q until %s", holder, formatRFC3339(l.HoldExpiresAt)))
	}
	s.saveLeaseLocked(l)
	return l, nil
}

// renewHolder is the renewal path of PUT /api/leases/{id}/holder: the
// same holder renews the hold for another HoldTTL (or an explicit,
// capped holdTTL) from now. A different holder is refused
// (errHolderMismatch); both fields empty clears the hold.
func (s *Service) renewHolder(owner, id, holder, holderURL string, holdTTL time.Duration) (*Lease, error) {
	if err := validateHolder(holder, holderURL); err != nil {
		return nil, err
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l := s.store.leases[id]
	if l == nil || l.Owner != owner || l.released {
		return nil, errNotFound
	}
	if err := lostErr(l); err != nil {
		return nil, err
	}
	now := s.now()
	if holder == "" {
		clearHoldLocked(l)
		s.emitLeaseEvent(id, owner, LeaseHolderCleared, "hold cleared")
		s.saveLeaseLocked(l)
		return l, nil
	}
	if holder != l.Holder {
		return nil, errHolderMismatch
	}
	if holderURL != "" {
		l.HolderUrl = holderURL
	}
	s.setHoldLocked(l, l.Holder, l.HolderUrl, holdTTL, now)
	s.emitLeaseEvent(id, owner, LeaseHolderSet, fmt.Sprintf("hold renewed by %q until %s", holder, formatRFC3339(l.HoldExpiresAt)))
	s.saveLeaseLocked(l)
	return l, nil
}

// expireHolds lapses every hold past its HoldExpiresAt. A lapsed hold
// keeps the holder (for visibility, and so the held rules keep covering
// the lease) but has no expiry, so the TTL sweep never releases it,
// however long ago its original TTL passed. A running lease is
// suspended (memory and hugepages freed, nothing deleted); rule 2 then
// releases it once it has stayed suspended and untouched for
// HeldSuspendedRelease, and renewing the hold restores it. A lease
// already suspended is left as it is, with the lapse starting rule 2's
// clock. Nothing is released here. Returns the ids whose hold lapsed.
func (s *Service) expireHolds(ctx context.Context, now time.Time) []string {
	var lapsed, running []*Lease
	s.store.mu.Lock()
	for _, l := range s.store.leases {
		if l.released || !l.held() || l.HoldExpiresAt.IsZero() || now.Before(l.HoldExpiresAt) {
			continue
		}
		detail := fmt.Sprintf("hold set at %s lapsed at %s (ttl %s)",
			l.HoldSetAt.Format(time.RFC3339), l.HoldExpiresAt.Format(time.RFC3339),
			l.HoldExpiresAt.Sub(l.HoldSetAt).Round(time.Second))
		l.HoldExpiresAt = time.Time{} // lapsed: still held, no expiry
		if l.Suspended {
			s.heldAction(ctx, l, heldRuleExpiry, heldActionSuspendLapse,
				detail+"; already suspended, released after it stays untouched for the stale limit unless renewed", now)
		} else {
			s.heldAction(ctx, l, heldRuleExpiry, heldActionExpire, detail+"; suspending it", now)
			running = append(running, l)
		}
		lapsed = append(lapsed, l)
	}
	s.store.mu.Unlock()
	for _, l := range running {
		// When the process-wide snapshot limiter is busy (another pause
		// or a checkpoint is writing), stand down for this tick and
		// retry the lease next tick instead of queueing a batch behind
		// the running write (spoond-t1s). The hold stays lapsed either
		// way; rule 1 suspends the lease once idle.
		if s.snapshotBusy() {
			break
		}
		if _, err := s.pauseLeaseWith(ctx, l, false, suspendPolicy{reason: suspendReasonHoldLapsed}); err != nil {
			// Busy or failing: it stays held with no expiry, so rule 1
			// suspends it once idle; nothing is released either way. A
			// release that raced the pause is not a failure to report
			// (spoond-15i).
			if !errors.Is(err, errLeaseBusy) && !errors.Is(err, errLeaseReleased) {
				s.log.Printf("held lease %s: suspend after lapse: %v", l.ID, err)
			}
			continue
		}
		s.heldActionLocked(ctx, l, heldRuleExpiry, heldActionSuspendLapse, "hold lapsed; suspended", now)
	}
	ids := make([]string, 0, len(lapsed))
	for _, l := range lapsed {
		ids = append(ids, l.ID)
	}
	return ids
}

// suspendedByRule reports whether l is suspended because a rule
// suspended it (idle, pressure, a lapse, or preemption) and has seen no
// activity since; it returns the time of that suspension. Only such
// leases may be released by rules 2 and 5: a lease suspended by hand or
// by the drain, or resumed and used since, is never released
// automatically. A preempted lease is subject to the same rules as any
// other rule-suspended lease (#145 D2, review R3): it is no longer
// exempted while it waits for a resume queue that no longer exists.
// Call with s.store.mu held.
func suspendedByRule(l *Lease) (time.Time, bool) {
	if l.released || !l.held() || l.State != "suspended" || !l.Suspended || l.LastActionAt.IsZero() {
		return time.Time{}, false
	}
	// A preempted lease carries last_action "preempt/suspend" (the
	// preemption stamps it after the pause), so it is covered by the
	// rules below just like an idle or pressure suspension.
	switch l.LastAction {
	case heldRuleIdle + "/" + heldActionSuspendIdle,
		heldRulePressure + "/" + heldActionSuspendIdle,
		heldRuleExpiry + "/" + heldActionSuspendLapse,
		pauseActionPreempt,
		// A per-lease idle_suspend suspension (2.5, #129 part 2) is a
		// rule suspension too: rules 2 and 5 may release it once it has
		// stayed idle-suspended and untouched.
		idleSuspendRule + "/" + heldActionSuspendIdle:
	default:
		return time.Time{}, false
	}
	if l.LastActive.After(l.LastActionAt) {
		return time.Time{}, false // used since the rule suspended it
	}
	return l.LastActionAt, true
}

// heldActionLocked is heldAction with the store lock taken: most rule
// actions run on leases collected under the lock earlier in the pass.
func (s *Service) heldActionLocked(ctx context.Context, l *Lease, rule, action, detail string, now time.Time) {
	s.store.mu.Lock()
	s.heldAction(ctx, l, rule, action, detail, now)
	s.store.mu.Unlock()
}

// freePercent is the snapshot store's free space as a percentage of its
// capacity. ok is false when the stat fails or the capacity is 0 — the
// pressure and critical rules then stay off (a disk that cannot be
// read is not a reason to delete held work).
func (s *Service) freePercent(path string) (pct float64, ok bool) {
	total, free, err := s.diskCapacity(path)
	if err != nil || total == 0 {
		return 0, false
	}
	return float64(free) / float64(total) * 100, true
}

// pressureShortensIdle reports whether rule 4 is active: the
// snapshot-disk free space is under PressureDiskFreePct, or free
// hugepages would not admit a default-size lease. Either way rule 1
// uses the shorter pressure threshold.
func (s *Service) pressureShortensIdle(ctx context.Context) (bool, string) {
	threshold := s.cfg.PressureDiskFreePct
	if threshold > 0 {
		if pct, ok := s.freePercent(s.cfg.TemplateStoragePath); ok && pct < threshold {
			return true, fmt.Sprintf("disk %.1f%% free < %.0f%%", pct, threshold)
		}
	}
	if s.cfg.PressureHeldIdle > 0 {
		need := uint64(defaultAdmitMemoryMB) * 1024 * 1024
		if info, err := s.sub.NodeInfo(ctx); err == nil {
			free := info.FreeHugepageBytes()
			if info.Status == "healthy" && free < need {
				return true, fmt.Sprintf("hugepages %d bytes free < %d needed for a default lease", free, need)
			}
		}
	}
	return false, ""
}

// defaultAdmitMemoryMB is the memory a default-size lease needs
// (admission refuses below it). No seeded image is smaller than 1 GiB
// (images/manifest.yaml), so a node that cannot host that much cannot
// host any default lease — using the minimum (not a typical size) keeps
// the pressure rule from under-detecting.
const defaultAdmitMemoryMB = 1024

// heldIdleTimeout is rule 1's effective threshold for this sweep: the
// configured HeldIdleTimeout, or PressureHeldIdle while rule 4 is
// active. Zero disables rule 1 entirely.
func (s *Service) heldIdleTimeout(ctx context.Context, now time.Time) (time.Duration, string) {
	if s.cfg.HeldIdleTimeout <= 0 {
		return 0, ""
	}
	if s.cfg.PressureHeldIdle > 0 {
		if under, why := s.pressureShortensIdle(ctx); under {
			return s.cfg.PressureHeldIdle, why
		}
	}
	return s.cfg.HeldIdleTimeout, ""
}

// releaseHeld deletes a held lease's sandbox and row (the GC reclaims
// its builds; see releaseSuspendedHeldUntil for how the reclaimed
// space becomes visible). Draining must be checked by the caller (the
// sweep does).
func (s *Service) releaseHeld(ctx context.Context, l *Lease) {
	s.releaseBecause(ctx, l, "released by a held-lease rule")
}

// releaseSuspendedHeldUntil implements rule 5: while the snapshot disk
// is under CriticalDiskFreePct, release held leases already suspended
// by rule 1 (or 4), oldest suspension first, until free space is above
// CriticalDiskRecoverPct — the GC having run first. A running (or
// otherwise live) held lease is never released here.
func (s *Service) releaseSuspendedHeldUntil(ctx context.Context, now time.Time) {
	crit := s.cfg.CriticalDiskFreePct
	recover := s.cfg.CriticalDiskRecoverPct
	if crit <= 0 || s.cfg.TemplateStoragePath == "" {
		return
	}
	if recover < crit {
		recover = crit // a recovery level below the critical level never terminates
	}
	pct, ok := s.freePercent(s.cfg.TemplateStoragePath)
	if !ok || pct >= crit {
		return
	}
	// Releasing a lease frees disk only through the GC's deletions. With
	// the GC in dry-run (GC_DELETE unset) nothing is ever freed, so
	// releasing would destroy held work for no gain: refuse, and say so
	// (at most once an hour).
	if os.Getenv("GC_DELETE") != "1" {
		if now.Sub(s.criticalDryRunLogged) >= time.Hour {
			s.criticalDryRunLogged = now
			s.log.Printf("held leases: disk %.1f%% free < %.0f%% critical, but GC_DELETE is not 1 (dry-run GC frees nothing): releasing no held lease", pct, crit)
		}
		return
	}
	// The GC runs first, as the rule requires, but at most every five
	// minutes: it walks the whole build catalog, and the sweep ticks
	// every few seconds. Its budget is the sweep's context, so a slow
	// substrate cannot wedge the sweeper.
	if now.Sub(s.criticalGCAt) >= 5*time.Minute {
		s.criticalGCAt = now
		s.gcOnce(ctx) // its errors do not stop the rule
	}
	if p, ok := s.freePercent(s.cfg.TemplateStoragePath); ok {
		pct = p
	}
	if pct >= crit {
		return
	}
	// One oldest-suspended victim per tick. A release deletes the
	// sandbox but frees no space by itself — the pause build only
	// becomes a GC candidate gcAge (1 h) after the suspension — so
	// measuring again here cannot see the release's effect, and a loop
	// would wipe every suspended lease on an unrecoverable disk (e.g.
	// with the dry-run GC default, where no candidate is ever deleted).
	// Releasing one per tick costs nothing against a 5 s sweep, and each
	// next tick's leading GC pass reclaims the previous victims' builds
	// when GC_DELETE=1, so the recovery level is still approached.
	victim, suspendedAt, ok := s.oldestSuspendedHeld(now)
	if !ok {
		return
	}
	s.heldActionLocked(ctx, victim, heldRuleCritical, heldActionRelease,
		fmt.Sprintf("disk %.1f%% free < %.0f%% critical; suspended at %s (%s ago), oldest first; recovery at %.0f%%",
			pct, crit, suspendedAt.Format(time.RFC3339), now.Sub(suspendedAt).Round(time.Second), recover),
		now)
	s.releaseHeld(ctx, victim)
}

// oldestSuspendedHeld returns the lease a rule suspended longest ago
// (suspendedByRule) — rule 5's victim. Running leases, and leases
// suspended by hand or by the drain, are never candidates. Call with s.store.mu NOT
// held (the returned pointer is used outside the lock, like the
// sweeper's other victims).
func (s *Service) oldestSuspendedHeld(now time.Time) (*Lease, time.Time, bool) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	var best *Lease
	var bestAt time.Time
	for _, l := range s.store.leases {
		at, ok := suspendedByRule(l)
		if !ok {
			continue
		}
		if best == nil || at.Before(bestAt) {
			best, bestAt = l, at
		}
	}
	if best == nil {
		return nil, time.Time{}, false
	}
	return best, bestAt, true
}

// releaseStaleHeld implements rule 2: a lease a rule suspended (idle,
// pressure or a lapse) and untouched since for HeldSuspendedRelease is
// released (deleted); the GC reclaims its builds. The clock starts at
// that suspension (suspendedByRule); a lease suspended by hand or by the
// drain, or used since, is never released here.
func (s *Service) releaseStaleHeld(ctx context.Context, now time.Time) {
	release := s.cfg.HeldSuspendedRelease
	if release <= 0 {
		return
	}
	type victim struct {
		l  *Lease
		at time.Time
	}
	var stale []victim
	s.store.mu.Lock()
	for _, l := range s.store.leases {
		if at, ok := suspendedByRule(l); ok && now.Sub(at) >= release {
			stale = append(stale, victim{l, at})
		}
	}
	s.store.mu.Unlock()
	for _, v := range stale {
		s.heldActionLocked(ctx, v.l, heldRuleStale, heldActionRelease,
			fmt.Sprintf("suspended by %s at %s, untouched for %s >= %s", v.l.LastAction, v.at.Format(time.RFC3339),
				now.Sub(v.at).Round(time.Second), release),
			now)
		s.releaseHeld(ctx, v.l)
	}
}

// runHeldRules runs the held-lease rules for one sweep tick, in order:
// hold lapse (3, suspending running leases, releasing none),
// stale-release (2), then
// idle-suspend (1, shortened by pressure (4)) — pressure is evaluated
// once per tick — and critical-release (5) last, so a tick that both
// suspends and frees leaves the disk check looking at the resulting
// state. Skips while draining (the sweep already checked; this guards
// direct callers).
func (s *Service) runHeldRules(ctx context.Context, now time.Time) {
	if s.draining.Load() {
		return
	}
	s.expireHolds(ctx, now)
	s.releaseStaleHeld(ctx, now)

	timeout, why := s.heldIdleTimeout(ctx, now)
	if timeout > 0 {
		s.suspendIdleHeld(ctx, now, timeout, why)
	}
	s.releaseSuspendedHeldUntil(ctx, now)
}

// suspendIdleHeld suspends every held lease idle for at least timeout
// (rule 1): memory and hugepages are freed into a pause build, nothing
// is deleted, and the lease resumes on next use. pressure names rule
// 4's reason when the timeout came from it ("" = the plain timeout).
func (s *Service) suspendIdleHeld(ctx context.Context, now time.Time, timeout time.Duration, pressure string) {
	var idle []*Lease
	s.store.mu.Lock()
	for _, l := range s.store.leases {
		if l.released || !l.held() || l.Suspended || l.busy {
			continue
		}
		// A lease with a running background job is active (2.6, #135):
		// no held rule may suspend it mid-job. A lease with its own
		// effective idle_suspend is reclaimed on its own threshold by
		// suspendIdleLeases (2.5, #129 part 2): rule 1 and rule 4's
		// shortening do not apply to it.
		if s.hasRunningJobLocked(l.ID) || s.effectiveIdleSuspend(l) > 0 {
			continue
		}
		if !now.After(l.LastActive.Add(timeout)) {
			continue
		}
		idle = append(idle, l)
	}
	s.store.mu.Unlock()
	for _, l := range idle {
		// When the process-wide snapshot limiter is busy (another pause
		// or a checkpoint is writing), stand down for this tick and
		// retry the lease next tick instead of queueing a batch behind
		// the running write (spoond-t1s).
		if s.snapshotBusy() {
			break
		}
		// Re-check under the lock just before pausing: activity (an exec,
		// a heartbeat) can land between the collection pass above and this
		// pause, and a lease that moved in the meantime must not be
		// suspended.
		s.store.mu.Lock()
		lastActive := l.LastActive
		skip := l.released || !l.held() || l.Suspended || l.busy || s.hasRunningJobLocked(l.ID) ||
			s.effectiveIdleSuspend(l) > 0 || !now.After(lastActive.Add(timeout))
		s.store.mu.Unlock()
		if skip {
			continue
		}
		detail := fmt.Sprintf("idle since %s, %s >= %s", lastActive.Format(time.RFC3339),
			now.Sub(lastActive).Round(time.Second), timeout)
		if pressure != "" {
			detail += "; pressure: " + pressure
		}
		rule := heldRuleIdle
		reason := suspendReasonIdle
		if pressure != "" {
			rule = heldRulePressure
			reason = suspendReasonPressure
		}
		if _, err := s.pauseLeaseWith(ctx, l, false, suspendPolicy{reason: reason}); err != nil {
			// A release that raced the pause is not a held-rule error:
			// the lease is gone and nothing was suspended (spoond-15i).
			if !errors.Is(err, errLeaseBusy) && !errors.Is(err, errLeaseReleased) {
				s.log.Printf("held lease %s: idle suspend: %v", l.ID, err)
			}
			continue
		}
		s.heldActionLocked(ctx, l, rule, heldActionSuspendIdle, detail, now)
	}
}
