package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jrimmer/spoond/v2/substrate"
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
// while the node is draining. A hold also expires on its own: it lasts
// HoldTTL from when it was set or renewed, at most HoldTTLMax (for an
// explicit hold_ttl). Past expiry the holder fields are cleared and the
// lease follows the normal TTL and idle rules again. Rule 4 shortens
// rule 1's idle threshold under pressure; rule 5 releases
// already-suspended held leases when the snapshot disk runs critical,
// oldest suspension first, never a running one.

// Held-lease rule names, as used in log lines, the
// spoond_held_actions_total{rule} label and the recorded last_action.
const (
	heldRuleIdle     = "idle"     // rule 1: idle held lease suspended
	heldRuleStale    = "stale"    // rule 2: suspended held lease released
	heldRuleExpiry   = "expiry"   // rule 3: hold expired, holder cleared
	heldRulePressure = "pressure" // rule 4: pressure shortens rule 1
	heldRuleCritical = "critical" // rule 5: critical disk releases suspended leases
)

// Held-lease action names, as used in the
// spoond_held_actions_total{action} label and the recorded last_action.
const (
	heldActionRelease     = "release"
	heldActionExpire      = "expire"
	heldActionSuspendIdle = "suspend_idle"
)

// Defaults for the held-lease limits (owner decision 2026-10-03).
// cmd/spoond-backend maps the environment onto them, so an unset
// variable means the value below; a ServiceConfig field of 0 disables
// the rule it configures.
const (
	DefaultHeldIdleTimeout      = 4 * 24 * time.Hour  // HELD_IDLE_TIMEOUT_SECS (14400)
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
// it), the spoond_held_actions_total{rule,action} counter and the
// lease's last_action/last_action_at record (persisted). now is the
// instant of the action; detail is the human-readable numbers.
func (s *Service) heldAction(ctx context.Context, l *Lease, rule, action, detail string, now time.Time) {
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
}

// clearHold removes the hold: holder fields and hold timestamps are
// reset and the lease follows the normal TTL and idle rules from then
// on. Call with s.store.mu held.
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
	now := s.now()
	if holder == "" {
		clearHoldLocked(l)
	} else {
		s.setHoldLocked(l, holder, holderURL, holdTTL, now)
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
	now := s.now()
	if holder == "" {
		clearHoldLocked(l)
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
	s.saveLeaseLocked(l)
	return l, nil
}

// expireHolds clears the holder of every held lease whose hold has
// passed its HoldExpiresAt. The lease then follows the normal TTL and
// idle rules: an already-expired TTL releases it at the same sweep.
// Returns the ids of the leases whose hold expired.
func (s *Service) expireHolds(ctx context.Context, now time.Time) []string {
	s.store.mu.Lock()
	var expired []*Lease
	for _, l := range s.store.leases {
		if l.released || !l.held() || l.HoldExpiresAt.IsZero() || now.Before(l.HoldExpiresAt) {
			continue
		}
		remaining := l.ExpiresAt.Sub(now)
		s.heldAction(ctx, l, heldRuleExpiry, heldActionExpire,
			fmt.Sprintf("hold set at %s, expired at %s (ttl %s); %s",
				l.HoldSetAt.Format(time.RFC3339), l.HoldExpiresAt.Format(time.RFC3339),
				l.HoldExpiresAt.Sub(l.HoldSetAt).Round(time.Second),
				heldSweepNote(!l.Persistent, remaining)),
			now)
		clearHoldLocked(l)
		s.saveLeaseLocked(l)
		expired = append(expired, l)
	}
	s.store.mu.Unlock()
	ids := make([]string, 0, len(expired))
	for _, l := range expired {
		ids = append(ids, l.ID)
	}
	return ids
}

// heldSweepNote describes what now happens to an expired hold: either
// the next sweep releases the lease (its TTL has already passed) or it
// is protected until then.
func heldSweepNote(expired bool, remaining time.Duration) string {
	if expired {
		return "ttl already passed; the next sweep releases it"
	}
	return fmt.Sprintf("ttl runs %s more; normal sweeping resumes", remaining.Round(time.Second))
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
			free := (info.HugepagesTotal - info.HugepagesUsed - info.HugepagesReserved) * info.HugepageSizeBytes
			if info.Status == "healthy" && free < need {
				return true, fmt.Sprintf("hugepages %d bytes free < %d needed for a default lease", free, need)
			}
		}
	}
	return false, ""
}

// defaultAdmitMemoryMB is the memory a default-size lease needs
// (admission refuses below it): the smallest seeded image's footprint
// in tests, 2048 MiB in production terms. The pressure rule uses it as
// "would admission refuse a default-size lease".
const defaultAdmitMemoryMB = 2048

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
// its builds). Draining must be checked by the caller (the sweep does).
func (s *Service) releaseHeld(ctx context.Context, l *Lease) {
	s.release(ctx, l)
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
	gcCtx, cancel := context.WithTimeout(context.Background(), gcAge)
	defer cancel()
	_ = s.gcOnce(gcCtx) // the GC runs first; its errors do not stop the rule
	pct, ok := s.freePercent(s.cfg.TemplateStoragePath)
	if !ok || pct >= crit {
		return
	}
	for {
		victim, suspendedAt, ok := s.oldestSuspendedHeld(now)
		if !ok {
			return
		}
		s.heldActionLocked(ctx, victim, heldRuleCritical, heldActionRelease,
			fmt.Sprintf("disk %.1f%% free < %.0f%% critical; suspended at %s (%s ago), oldest first; recovery at %.0f%%",
				pct, crit, suspendedAt.Format(time.RFC3339), now.Sub(suspendedAt).Round(time.Second), recover),
			now)
		s.releaseHeld(ctx, victim)
		// Keep releasing until the free percentage is back above the
		// recovery level (or nothing suspended is left). Releases do not
		// free disk directly — the GC reclaims the builds — so a pass
		// releases every candidate when the level cannot be reached.
		if next, ok := s.freePercent(s.cfg.TemplateStoragePath); !ok || next >= recover {
			return
		}
	}
}

// oldestSuspendedHeld returns the held lease that has been suspended
// longest (state "suspended", non-released, with a holder) — rule 5's
// victim. Live leases are never candidates. Call with s.store.mu NOT
// held (the returned pointer is used outside the lock, like the
// sweeper's other victims).
func (s *Service) oldestSuspendedHeld(now time.Time) (*Lease, time.Time, bool) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	var best *Lease
	bestAt := now
	for _, l := range s.store.leases {
		if l.released || !l.held() || l.State != "suspended" {
			continue
		}
		at := l.LastActionAt
		if at.IsZero() || at.After(now) {
			// Suspended before last_action was recorded (by the drain, by
			// hand): fall back to the lease's own activity clock so the
			// order stays stable and old.
			at = l.LastActive
			if at.After(now) {
				at = now
			}
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

// releaseStaleHeld implements rule 2: a held lease suspended by rule 1
// (or 4) and untouched for HeldSuspendedRelease is released (deleted);
// the GC reclaims its builds. The lease's LastActionAt (recorded by
// rule 1's suspend, or falling back to LastActive) starts the clock.
func (s *Service) releaseStaleHeld(ctx context.Context, now time.Time) {
	release := s.cfg.HeldSuspendedRelease
	if release <= 0 {
		return
	}
	var stale []*Lease
	s.store.mu.Lock()
	for _, l := range s.store.leases {
		if l.released || !l.held() || l.State != "suspended" {
			continue
		}
		at := l.LastActionAt
		if at.IsZero() || at.After(now) {
			at = l.LastActive
			if at.After(now) {
				at = now
			}
		}
		if now.Sub(at) >= release {
			stale = append(stale, l)
		}
	}
	s.store.mu.Unlock()
	for _, l := range stale {
		at := l.LastActionAt
		if at.IsZero() {
			at = l.LastActive
		}
		s.heldActionLocked(ctx, l, heldRuleStale, heldActionRelease,
			fmt.Sprintf("suspended at %s, untouched for %s >= %s", at.Format(time.RFC3339),
				now.Sub(at).Round(time.Second), release),
			now)
		s.releaseHeld(ctx, l)
	}
}

// runHeldRules runs the held-lease rules for one sweep tick, in order:
// hold expiry (3), stale-release (2), then idle-suspend (1, shortened
// by pressure (4)) — pressure is evaluated once per tick — and
// critical-release (5) last, so a tick that both suspends and frees
// leaves the disk check looking at the resulting state. Skips while
// draining (the sweep already checked; this guards direct callers).
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
		if !now.After(l.LastActive.Add(timeout)) {
			continue
		}
		idle = append(idle, l)
	}
	s.store.mu.Unlock()
	for _, l := range idle {
		detail := fmt.Sprintf("idle since %s, %s >= %s", l.LastActive.Format(time.RFC3339),
			now.Sub(l.LastActive).Round(time.Second), timeout)
		if pressure != "" {
			detail += "; pressure: " + pressure
		}
		rule := heldRuleIdle
		if pressure != "" {
			rule = heldRulePressure
		}
		if _, err := s.pauseLease(ctx, l, false); err != nil {
			if !errors.Is(err, errLeaseBusy) {
				s.log.Printf("held lease %s: idle suspend: %v", l.ID, err)
			}
			continue
		}
		s.heldActionLocked(ctx, l, rule, heldActionSuspendIdle, detail, now)
	}
}

// lookupHeld returns a live held lease by id, regardless of owner. Used
// by the SSH gateway so an attach can resume a rule-1-suspended held
// lease it does not own.
func (s *Service) lookupHeld(id string) *Lease {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l := s.store.leases[id]
	if l == nil || l.released || !l.held() {
		return nil
	}
	return l
}

// resumeHeldGateway resumes a held lease on attach: the gateway's
// capability is the lease name or id, not an owner id, so resume
// cannot require one. Only held leases go through here; a live lease
// is returned as-is (nothing to resume) and any other error propagates.
func (s *Service) resumeHeldGateway(ctx context.Context, id string) (*Lease, error) {
	l := s.lookupHeld(id)
	if l == nil {
		return nil, errNotFound
	}
	if !l.Suspended {
		return l, nil
	}
	return s.resumeLease(ctx, l)
}

// handleHeldResume answers POST /api/leases/{id}/resume: the SSH
// gateway's owner-blind resume for a held lease suspended by rule 1
// (resuming on next use is what makes the suspension safe). Unheld
// leases get the same 404 as the other owner-scoped routes — they have
// the owner-checked /api/sandboxes/{id}/resume.
func (s *Server) handleHeldResume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	lease, err := s.svc.resumeHeldGateway(r.Context(), id)
	if err != nil {
		switch {
		case errors.Is(err, errNotFound):
			writeError(w, http.StatusNotFound, "lease not found")
		case errors.Is(err, errLeaseBusy):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, substrate.ErrCapacity):
			writeError(w, http.StatusServiceUnavailable, "capacity: "+err.Error())
		default:
			s.svc.log.Printf("held resume %s: %v", id, err)
			writeError(w, http.StatusInternalServerError, "resume failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      lease.ID,
		"status":  "running",
		"address": lease.HostIP,
	})
}
