package api

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Ordered reclaim under memory pressure (#145 D1, owner decision
// 2026-10-08). Rule 4 used to shorten every held lease's idle threshold
// with no order; that is replaced by one ordered policy, shared with
// preemption (api/preempt.go). The policy names steps in
// PRESSURE_ORDER; within a step the oldest rule is kept: lowest
// priority first, then the newest lease, then the owner furthest over
// its guarantee, then the id. A guaranteed *held* lease is never taken:
// it is protected, and no step names it.
//
// PRESSURE_ORDER defaults to "burst-unheld,burst-held,
// guaranteed-unheld-idle". A burst lease may be reclaimed whatever its
// holder; a guaranteed lease only when it is unheld and has been idle
// for PRESSURE_IDLE_SECS (default 1800), so a lease of one's own
// guarantee that is actually being watched is left alone. An unknown
// step is a fatal configuration error at startup.

// PressureStep names one step of the reclaim order (#145 D1).
type PressureStep string

const (
	// PressureStepBurstUnheld reclaims running, unheld burst leases.
	PressureStepBurstUnheld PressureStep = "burst-unheld"
	// PressureStepBurstHeld reclaims running, held burst leases.
	PressureStepBurstHeld PressureStep = "burst-held"
	// PressureStepGuaranteedUnheldIdle reclaims running, unheld
	// guaranteed leases idle for at least PressureIdle.
	PressureStepGuaranteedUnheldIdle PressureStep = "guaranteed-unheld-idle"
)

// DefaultPressureOrder is PRESSURE_ORDER's default: burst work first
// (unheld, then held), then a guaranteed lease only once it is unheld
// and idle past PressureIdle.
const DefaultPressureOrder = "burst-unheld,burst-held,guaranteed-unheld-idle"

// DefaultPressureIdle is PRESSURE_IDLE_SECS' default: how long an unheld
// guaranteed lease must be idle before the guaranteed-unheld-idle step
// reclaims it (30 min).
const DefaultPressureIdle = 30 * time.Minute

// validPressureStep reports whether name is a known step.
func validPressureStep(name PressureStep) bool {
	switch name {
	case PressureStepBurstUnheld, PressureStepBurstHeld, PressureStepGuaranteedUnheldIdle:
		return true
	}
	return false
}

// defaultPressureOrderSteps returns the parsed default order.
func defaultPressureOrderSteps() []PressureStep {
	return []PressureStep{PressureStepBurstUnheld, PressureStepBurstHeld, PressureStepGuaranteedUnheldIdle}
}

// ParsePressureOrder parses and validates a PRESSURE_ORDER value: a
// comma-separated list of known steps, no duplicates. An empty value
// returns the default order. An unknown, empty or repeated step is an
// error, which the backend treats as a fatal configuration error.
func ParsePressureOrder(s string) ([]PressureStep, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return defaultPressureOrderSteps(), nil
	}
	var out []PressureStep
	seen := map[PressureStep]bool{}
	for _, part := range strings.Split(s, ",") {
		step := PressureStep(strings.TrimSpace(part))
		if step == "" {
			return nil, fmt.Errorf("empty step")
		}
		if !validPressureStep(step) {
			return nil, fmt.Errorf("unknown step %q", step)
		}
		if seen[step] {
			return nil, fmt.Errorf("duplicate step %q", step)
		}
		seen[step] = true
		out = append(out, step)
	}
	return out, nil
}

// pressureOrderSteps is the effective reclaim order: the configured one,
// or the default. The backend validates PRESSURE_ORDER at startup, so a
// value that reaches here has already parsed.
func (s *Service) pressureOrderSteps() []PressureStep {
	if len(s.pressureSteps) > 0 {
		return s.pressureSteps
	}
	return defaultPressureOrderSteps()
}

// pressureIdle is the idle threshold the guaranteed-unheld-idle step
// uses: PRESSURE_IDLE_SECS, or DefaultPressureIdle.
func (s *Service) pressureIdle() time.Duration {
	if s.cfg.PressureIdle > 0 {
		return s.cfg.PressureIdle
	}
	return DefaultPressureIdle
}

// pressureActive reports whether memory pressure is on right now: the
// node is healthy and its free hugepages cannot host a default-size
// lease (defaultAdmitMemoryMB). That is the trigger the held rules
// already used, and the only one: disk pressure no longer shortens or
// reclaims anything (disk is handled by rule 5 and the kept quota,
// #145 D1). why is a human phrase for the log and the pause detail.
func (s *Service) pressureActive(ctx context.Context) (bool, string) {
	need := uint64(defaultAdmitMemoryMB) * 1024 * 1024
	if info, err := s.sub.NodeInfo(ctx); err == nil {
		free := info.FreeHugepageBytes()
		if info.Status == "healthy" && free < need {
			return true, fmt.Sprintf("hugepages %d bytes free < %d needed for a default lease", free, need)
		}
	}
	return false, ""
}

// pressureStepLocked classifies a reclaimable lease into its step, or
// reports that no step covers it. A released, busy, suspended or
// non-live lease is never a candidate; a guaranteed held lease is
// protected. A burst lease is reclaimable by a burst step whatever its
// holder (matching preemption, which may also pause one with a running
// job); a guaranteed unheld lease only once it has been idle for
// PressureIdle, and not if its own idle_suspend reclaims it or a
// background job is running. Call with s.store.mu held.
func (s *Service) pressureStepLocked(l *Lease, now time.Time) (PressureStep, bool) {
	if l.released || l.busy || l.Suspended || !l.live() {
		return "", false
	}
	if l.Class == ClassBurst {
		if l.held() {
			return PressureStepBurstHeld, true
		}
		return PressureStepBurstUnheld, true
	}
	// Guaranteed (an unstamped lease reads as guaranteed).
	if l.held() {
		return "", false // protected from memory pressure
	}
	if s.effectiveIdleSuspend(l) > 0 || s.hasRunningJobLocked(l.ID) {
		return "", false
	}
	if now.Sub(l.LastActive) < s.pressureIdle() {
		return "", false
	}
	return PressureStepGuaranteedUnheldIdle, true
}

// pressureOrdered reports whether step is named in the effective order.
func (s *Service) pressureOrdered(step PressureStep) bool {
	for _, s := range s.pressureOrderSteps() {
		if s == step {
			return true
		}
	}
	return false
}

// pressureBefore orders two candidates within one step: lowest priority
// first, then the newest lease, then the owner furthest over its
// guarantee, then the id (today's preemption order, #145 D1). Caller
// holds the store lock.
func (s *Service) pressureBefore(a, b *Lease) bool {
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	ao, bo := s.ownerOverGuaranteeLocked(a.Owner), s.ownerOverGuaranteeLocked(b.Owner)
	if ao != bo {
		return ao > bo
	}
	return a.ID < b.ID
}

// pressureCandidate is one reclaimable lease and the step that covers it.
type pressureCandidate struct {
	l    *Lease
	step PressureStep
}

// pressureCandidatesAt returns the reclaimable leases in pressure
// order at an instant: the configured steps in order, each holding its
// leases sorted by pressureBefore. It snapshots the store; each
// candidate is re-checked just before it is paused. The sweep passes
// its tick's now.
func (s *Service) pressureCandidatesAt(now time.Time) []pressureCandidate {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	return s.pressureCandidatesLocked(now)
}

// pressureCandidatesLocked is pressureCandidates under a held store
// lock. Caller holds s.store.mu.
func (s *Service) pressureCandidatesLocked(now time.Time) []pressureCandidate {
	order := s.pressureOrderSteps()
	byStep := map[PressureStep][]*Lease{}
	for _, l := range s.store.leases {
		step, ok := s.pressureStepLocked(l, now)
		if !ok || !s.pressureOrdered(step) {
			continue
		}
		byStep[step] = append(byStep[step], l)
	}
	var out []pressureCandidate
	for _, step := range order {
		ls := byStep[step]
		sort.SliceStable(ls, func(i, j int) bool { return s.pressureBefore(ls[i], ls[j]) })
		for _, l := range ls {
			out = append(out, pressureCandidate{l: l, step: step})
		}
	}
	return out
}

// pressureVictimValidLocked re-checks a candidate under the store lock
// just before its pause: nothing may have released, resumed or reclassified
// it between the snapshot and the pause. Caller holds s.store.mu.
func (s *Service) pressureVictimValidLocked(c pressureCandidate, now time.Time) bool {
	step, ok := s.pressureStepLocked(c.l, now)
	return ok && step == c.step && s.pressureOrdered(step)
}

// reclaimUnderPressure runs one pressure-order reclaim pass (#145 D1):
// while the node is under memory pressure it pauses reclaimable leases
// one at a time, in order, through the snapshot limiter, re-measuring
// after each and stopping as soon as the pressure clears. Each pause is
// lossless (the normal pause path) and respects the preemption disk
// floor. It is the sweep's replacement for rule 4's blanket shortening.
func (s *Service) reclaimUnderPressure(ctx context.Context, now time.Time) {
	if s.draining.Load() {
		return
	}
	active, why := s.pressureActive(ctx)
	if !active {
		return
	}
	cands := s.pressureCandidatesAt(now)
	for _, c := range cands {
		if s.draining.Load() {
			return
		}
		// Re-measure after every pause: stop the moment the node can
		// host a default lease again.
		if active, _ = s.pressureActive(ctx); !active {
			return
		}
		// One snapshot write at a time: when the limiter is busy, stand
		// down for this tick and retry next tick rather than queueing
		// behind the running write (spoond-t1s).
		if s.snapshotBusy() {
			return
		}
		s.store.mu.Lock()
		valid := s.pressureVictimValidLocked(c, now)
		s.store.mu.Unlock()
		if !valid {
			continue
		}
		if !s.preemptDiskOK(c.l) {
			s.log.Printf("pressure: lease %s: pause would take the snapshot disk under the %.0f%% floor; skipping",
				c.l.ID, s.preemptDiskFloorPct())
			continue
		}
		if _, err := s.pauseLeaseWith(ctx, c.l, false, suspendPolicy{
			reason:     suspendReasonPressure,
			policyStep: string(c.step),
		}); err != nil {
			if !errors.Is(err, errLeaseBusy) && !errors.Is(err, errLeaseReleased) {
				s.log.Printf("pressure: lease %s: %v", c.l.ID, err)
			}
			continue
		}
		s.recordPressureSuspend(ctx, c.l, c.step, why, now)
	}
}

// recordPressureSuspend records one pressure-order pause like the held
// rules do: a log line naming the lease, owner and step, the
// spoond_held_actions_total{pressure,suspend_idle} counter, the lease's
// last_action/last_action_at (persisted, so rule 2 may later release a
// held victim) and a held_action event naming the policy step. Call
// with s.store.mu NOT held.
func (s *Service) recordPressureSuspend(ctx context.Context, l *Lease, step PressureStep, why string, now time.Time) {
	detail := fmt.Sprintf("step %s: %s", step, why)
	s.store.mu.Lock()
	if l.released {
		// A release after the pause returned success but before this
		// record must not save or emit for a released lease (spoond-15i).
		s.store.mu.Unlock()
		return
	}
	s.log.Printf("pressure reclaim: lease %s (owner %s, class %s): step %s: %s",
		l.ID, l.Owner, leaseClassRow(l), step, why)
	if s.metrics != nil {
		s.metrics.HeldActions.WithLabelValues(heldRulePressure, heldActionSuspendIdle).Inc()
	}
	l.LastAction = heldRulePressure + "/" + heldActionSuspendIdle
	l.LastActionAt = now
	s.saveLeaseLocked(l)
	s.emitLeaseEvent(l.ID, l.Owner, LeaseHeldAction, heldRulePressure+"/"+heldActionSuspendIdle+": "+detail)
	s.store.mu.Unlock()
}
