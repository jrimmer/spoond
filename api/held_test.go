package api

// Limits on held leases that act automatically (2.1): every rule fires
// at its threshold and not before, a heartbeat prevents the idle
// suspend, renewal extends a hold and is capped at the maximum, a lapse
// suspends and never releases, pressure
// shortens the idle threshold, the critical rule releases
// oldest-suspended first and stops at the recovery level, a running
// held lease is never released, and every action is counted and
// recorded. A lapsed hold suspends a running lease and never releases
// it; only leases a rule suspended, untouched since, are ever released. The tests use the fake substrate, the injectable clock
// (svc.now) and an injectable free-space reader (svc.diskCapacity).

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/jrimmer/spoond/v2/metrics"
	"github.com/jrimmer/spoond/v2/substrate"
)

// heldCounter reads the current value of
// spoond_held_actions_total{rule,action} from the service's registry.
func heldCounter(t *testing.T, svc *Service, rule, action string) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := svc.metrics.HeldActions.WithLabelValues(rule, action).Write(m); err != nil {
		t.Fatalf("held_actions_total{%s,%s}: %v", rule, action, err)
	}
	return m.GetCounter().GetValue()
}

// holdOf returns the lease's hold fields (under the store lock).
func holdOf(t *testing.T, svc *Service, id string) (holder string, expires time.Time) {
	t.Helper()
	svc.store.mu.Lock()
	defer svc.store.mu.Unlock()
	l := svc.store.leases[id]
	if l == nil {
		t.Fatalf("lease %s not found", id)
	}
	return l.Holder, l.HoldExpiresAt
}

// TestHeldIdleSuspendsAtThresholdNotBefore (rule 1): a held lease idle
// for HeldIdleTimeout is suspended — memory and hugepages freed, nothing
// deleted — and not one tick earlier.
func TestHeldIdleSuspendsAtThresholdNotBefore(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.SetMetrics(metrics.NewBackendMetrics())

	base := time.Now()
	svc.cfg.HeldIdleTimeout = time.Hour
	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "ci-job", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	sbID := l.SandboxID
	cur := base
	svc.now = func() time.Time { return cur }

	// 59 minutes in: below the threshold, still running.
	cur = base.Add(59 * time.Minute)
	svc.runHeldRules(ctx, cur)
	if l.Suspended {
		t.Fatal("held lease suspended before the idle threshold")
	}

	// Past the threshold: suspended, and the sandbox is gone (paused)
	// while the lease itself stays.
	cur = base.Add(2 * time.Hour)
	svc.runHeldRules(ctx, cur)
	if !l.Suspended || l.State != "suspended" {
		t.Fatalf("held lease not suspended past the threshold: state=%s", l.State)
	}
	if got := calls(sub.Fake, "Pause "+sbID); got != 1 {
		t.Fatalf("pause calls = %d, want 1", got)
	}
	if got := calls(sub.Fake, "Delete "+sbID); got != 0 {
		t.Fatalf("idle suspend deleted the sandbox (%d deletes), want 0", got)
	}
	if l.LastAction != "idle/suspend_idle" || l.LastActionAt.IsZero() {
		t.Fatalf("action not recorded: %q at %v", l.LastAction, l.LastActionAt)
	}

	// The counter counted it.
	if n := heldCounter(t, svc, "idle", "suspend_idle"); n != 1 {
		t.Fatalf("held_actions_total{idle,suspend_idle} = %g, want 1", n)
	}
}

// TestHeartbeatPreventsHeldIdleSuspend (rule 1): guest heartbeat
// activity counts as activity and keeps the held lease un-suspended;
// without it the same age suspends.
func TestHeartbeatPreventsHeldIdleSuspend(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.SetMetrics(metrics.NewBackendMetrics())
	svc.cfg.HeldIdleTimeout = 60 * time.Millisecond

	active, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "ci-job", "", nil)
	if err != nil {
		t.Fatalf("grant active: %v", err)
	}
	stale, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "ci-job-2", "", nil)
	if err != nil {
		t.Fatalf("grant stale: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	if !svc.markActive(active.ID) {
		t.Fatal("heartbeat (markActive) rejected a live held lease")
	}
	svc.runHeldRules(ctx, time.Now())
	if active.Suspended {
		t.Fatal("heartbeat-active held lease was suspended")
	}
	if !stale.Suspended {
		t.Fatal("idle held lease was not suspended")
	}
}

// TestPreemptedHeldLeaseReleasedByStaleRule (review R3): a preempted
// held lease is subject to rule 2 like any rule-suspended lease. It was
// previously exempted while it "waited for the resume queue", which no
// longer exists; one rule for every kind of suspend means the stale
// release covers it too.
func TestPreemptedHeldLeaseReleasedByStaleRule(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.SetMetrics(metrics.NewBackendMetrics())

	base := time.Now()
	svc.cfg.HeldSuspendedRelease = 7 * 24 * time.Hour
	svc.cfg.HeldIdleTimeout = 0 // rule 1 off: only the stale release acts
	cur := base
	svc.now = func() time.Time { return cur }

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "ci-job", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	sbID := l.SandboxID
	// A burst lease so it is a preemption candidate.
	svc.store.mu.Lock()
	l.Class = ClassBurst
	svc.store.mu.Unlock()
	if err := svc.preemptLease(ctx, l, "another-owner"); err != nil {
		t.Fatalf("preempt: %v", err)
	}
	if !l.Suspended || l.PreemptedAt.IsZero() {
		t.Fatal("setup: lease not suspended and preempted")
	}
	if l.LastAction != pauseActionPreempt {
		t.Fatalf("last_action = %q, want %q", l.LastAction, pauseActionPreempt)
	}
	// Untouched since the preemption: no activity after the stamp.
	svc.store.mu.Lock()
	l.LastActive = l.LastActionAt.Add(-time.Minute)
	svc.store.mu.Unlock()

	// Before the stale limit it is kept; after it, rule 2 releases it.
	cur = base.Add(6 * 24 * time.Hour)
	svc.releaseStaleHeld(ctx, cur)
	if svc.lookup("c", l.ID) == nil {
		t.Fatal("preempted held lease released before HeldSuspendedRelease")
	}
	cur = base.Add(7*24*time.Hour + time.Minute)
	svc.releaseStaleHeld(ctx, cur)
	if svc.lookup("c", l.ID) != nil {
		t.Fatal("preempted held lease not released by rule 2 after the stale limit")
	}
	if got := calls(sub.Fake, "Delete "+sbID); got != 1 {
		t.Fatalf("delete calls = %d, want 1", got)
	}
	if n := heldCounter(t, svc, "stale", "release"); n != 1 {
		t.Fatalf("held_actions_total{stale,release} = %g, want 1", n)
	}
}

// TestPreemptedHeldLeaseReleasedByCriticalRule (review R3): under
// critical disk pressure a preempted held lease is rule 5's victim like
// any other rule-suspended lease, and a preempted lease still inside
// another owner's guarantee is not exempted.
func TestPreemptedHeldLeaseReleasedByCriticalRule(t *testing.T) {
	t.Setenv("GC_DELETE", "1")
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.SetMetrics(metrics.NewBackendMetrics())
	svc.cfg.HeldIdleTimeout = 0
	svc.cfg.CriticalDiskFreePct = 5
	svc.cfg.CriticalDiskRecoverPct = 10
	svc.cfg.TemplateStoragePath = t.TempDir()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "ci-job", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	sbID := l.SandboxID
	svc.store.mu.Lock()
	l.Class = ClassBurst
	svc.store.mu.Unlock()
	if err := svc.preemptLease(ctx, l, "another-owner"); err != nil {
		t.Fatalf("preempt: %v", err)
	}
	svc.store.mu.Lock()
	l.LastActive = l.LastActionAt.Add(-time.Minute)
	svc.store.mu.Unlock()

	// 2% free (< 5% critical): rule 5 must pick the preempted lease.
	svc.diskCapacity = func(string) (uint64, uint64, error) { return 100, 2, nil }
	svc.releaseSuspendedHeldUntil(ctx, time.Now())
	if svc.lookup("c", l.ID) != nil {
		t.Fatal("preempted held lease not released by rule 5 under critical disk")
	}
	if got := calls(sub.Fake, "Delete "+sbID); got != 1 {
		t.Fatalf("delete calls = %d, want 1", got)
	}
	if n := heldCounter(t, svc, "critical", "release"); n != 1 {
		t.Fatalf("held_actions_total{critical,release} = %g, want 1", n)
	}
}

// TestHeldSuspendedReleasedAtThresholdNotBefore (rule 2): a held lease
// suspended by rule 1 and untouched for HeldSuspendedRelease is
// released (deleted), and not before.
func TestHeldSuspendedReleasedAtThresholdNotBefore(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.SetMetrics(metrics.NewBackendMetrics())

	base := time.Now()
	svc.cfg.HeldIdleTimeout = time.Hour
	svc.cfg.HeldSuspendedRelease = 24 * time.Hour
	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "ci-job", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	sbID := l.SandboxID
	cur := base
	svc.now = func() time.Time { return cur }

	// Suspend at +2h (rule 1), then check the release clock.
	cur = base.Add(2 * time.Hour)
	svc.runHeldRules(ctx, cur)
	if !l.Suspended {
		t.Fatal("setup: lease was not suspended by rule 1")
	}

	// 23 h after the suspension: still kept.
	cur = base.Add(2*time.Hour + 23*time.Hour)
	svc.runHeldRules(ctx, cur)
	if svc.lookup("c", l.ID) == nil {
		t.Fatal("suspended held lease released before HeldSuspendedRelease")
	}

	// 26 h after the suspension: released, sandbox deleted; the GC
	// reclaims the builds.
	cur = base.Add(2*time.Hour + 26*time.Hour)
	svc.runHeldRules(ctx, cur)
	if svc.lookup("c", l.ID) != nil {
		t.Fatal("suspended held lease not released past HeldSuspendedRelease")
	}
	if got := calls(sub.Fake, "Delete "+sbID); got != 1 {
		t.Fatalf("delete calls = %d, want 1", got)
	}
	if n := heldCounter(t, svc, "stale", "release"); n != 1 {
		t.Fatalf("held_actions_total{stale,release} = %g, want 1", n)
	}
}

// TestHoldLapseSuspendsNeverReleases (rule 3): when a hold lapses
// unrenewed, a running lease is suspended and stays held with no
// expiry, so the TTL sweep never releases it even though its own TTL
// passed long ago. Rule 2 releases it only after it has stayed
// suspended and untouched for HeldSuspendedRelease.
func TestHoldLapseSuspendsNeverReleases(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.SetMetrics(metrics.NewBackendMetrics())

	base := time.Now()
	svc.cfg.HoldTTL = time.Hour
	svc.cfg.HeldSuspendedRelease = 7 * 24 * time.Hour
	svc.cfg.HeldIdleTimeout = 0 // rule 1 off: only the lapse acts here
	cur := base
	svc.now = func() time.Time { return cur }

	l, err := svc.grant(ctx, "c", "py-base", 50*time.Millisecond, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.setHolderWithTTL("c", l.ID, "ci-job", "", 0); err != nil {
		t.Fatalf("set holder: %v", err)
	}

	// Before the lapse the lease is held and survives its (tiny) TTL.
	cur = base.Add(30 * time.Minute)
	svc.sweepExpired(ctx)
	if svc.lookup("c", l.ID) == nil {
		t.Fatal("held lease swept before its hold lapsed")
	}

	// At +2 h the hold has lapsed: the running lease is suspended, still
	// held, with no expiry; nothing is released.
	cur = base.Add(2 * time.Hour)
	svc.expireHolds(ctx, cur)
	holder, expires := holdOf(t, svc, l.ID)
	if holder != "ci-job" || !expires.IsZero() {
		t.Fatalf("lapsed hold: holder=%q expires=%v, want ci-job and no expiry", holder, expires)
	}
	if !l.Suspended {
		t.Fatal("running lease not suspended when its hold lapsed")
	}
	if l.LastAction != "expiry/suspend_lapsed" {
		t.Fatalf("last_action = %q, want expiry/suspend_lapsed", l.LastAction)
	}
	if n := heldCounter(t, svc, "expiry", "expire"); n != 1 {
		t.Fatalf("held_actions_total{expiry,expire} = %g, want 1", n)
	}

	// Its TTL passed hours ago, yet no sweep releases it.
	for _, h := range []time.Duration{3 * time.Hour, 24 * time.Hour, 6 * 24 * time.Hour} {
		cur = base.Add(h)
		svc.sweepExpired(ctx)
		if svc.lookup("c", l.ID) == nil {
			t.Fatalf("lapsed lease released by a sweep at +%s", h)
		}
	}

	// Seven days after the lapse-suspend, untouched: rule 2 releases it.
	cur = base.Add(2*time.Hour + 7*24*time.Hour + time.Minute)
	svc.sweepExpired(ctx)
	if svc.lookup("c", l.ID) != nil {
		t.Fatal("lapsed, suspended, untouched lease not released after the stale limit")
	}
	if n := heldCounter(t, svc, "stale", "release"); n != 1 {
		t.Fatalf("held_actions_total{stale,release} = %g, want 1", n)
	}
}

// TestHoldRenewalExtendsAndCapped: renewal is PUT holder with the same
// holder; it extends the hold from now, and an explicit hold_ttl is
// capped at HoldTTLMax.
func TestHoldRenewalExtendsAndCapped(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	base := time.Now()
	svc.cfg.HoldTTL = 2 * time.Minute
	svc.cfg.HoldTTLMax = 4 * time.Minute
	cur := base
	svc.now = func() time.Time { return cur }

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "ci-job", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.setHolderWithTTL("c", l.ID, "ci-job", "", 0); err != nil {
		t.Fatalf("set holder: %v", err)
	}
	_, first := holdOf(t, svc, l.ID)
	if !first.Equal(base.Add(2 * time.Minute)) {
		t.Fatalf("initial hold expires %v, want %v", first, base.Add(2*time.Minute))
	}

	// Renewal 1 min later: extended from now, not from the original set.
	cur = base.Add(time.Minute)
	if _, err := svc.renewHolder("c", l.ID, "ci-job", "", 0); err != nil {
		t.Fatalf("renew: %v", err)
	}
	_, renewed := holdOf(t, svc, l.ID)
	if !renewed.Equal(base.Add(3 * time.Minute)) {
		t.Fatalf("renewed hold expires %v, want %v", renewed, base.Add(3*time.Minute))
	}

	// An explicit hold_ttl is capped at HoldTTLMax (4 min), not taken
	// literally (an hour).
	if _, err := svc.renewHolder("c", l.ID, "ci-job", "", time.Hour); err != nil {
		t.Fatalf("renew with hold_ttl: %v", err)
	}
	_, capped := holdOf(t, svc, l.ID)
	if !capped.Equal(cur.Add(4 * time.Minute)) {
		t.Fatalf("capped hold expires %v, want %v", capped, cur.Add(4*time.Minute))
	}

	// A different holder cannot take the lease over.
	if _, err := svc.renewHolder("c", l.ID, "someone-else", "", 0); err != errHolderMismatch {
		t.Fatalf("renew by another holder = %v, want errHolderMismatch", err)
	}
	// Clearing still works.
	if _, err := svc.renewHolder("c", l.ID, "", "", 0); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if holder, _ := holdOf(t, svc, l.ID); holder != "" {
		t.Fatalf("holder not cleared: %q", holder)
	}
}

// TestPressureShortensHeldIdle (rule 4): under snapshot-disk pressure
// or hugepage shortage rule 1 uses PRESSURE_HELD_IDLE instead of the
// plain timeout.
func TestPressureShortensHeldIdle(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.SetMetrics(metrics.NewBackendMetrics())

	base := time.Now()
	svc.cfg.HeldIdleTimeout = 4 * time.Hour
	svc.cfg.PressureDiskFreePct = 15
	svc.cfg.PressureHeldIdle = 30 * time.Minute
	svc.cfg.TemplateStoragePath = t.TempDir()
	// No pressure to start with, whatever the test machine's own disk
	// looks like (statfs on the real temp dir made this test depend on
	// how full the host was).
	svc.diskCapacity = func(string) (uint64, uint64, error) { return 100, 90, nil }
	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "ci-job", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	cur := base
	svc.now = func() time.Time { return cur }

	// No pressure: 31 min idle does not suspend (the plain timeout is 4 h).
	svc.runHeldRules(ctx, base.Add(31*time.Minute))
	if l.Suspended {
		t.Fatal("held lease suspended without pressure")
	}

	// Disk pressure (10% free < 15%): the shortened threshold applies —
	// not at 29 min, yes at 31 min.
	var total, free uint64 = 100, 10
	svc.diskCapacity = func(string) (uint64, uint64, error) { return total, free, nil }
	svc.runHeldRules(ctx, base.Add(29*time.Minute))
	if l.Suspended {
		t.Fatal("held lease suspended before the pressure threshold")
	}
	svc.runHeldRules(ctx, base.Add(31*time.Minute))
	if !l.Suspended {
		t.Fatal("held lease not suspended at the pressure threshold")
	}
	if n := heldCounter(t, svc, "pressure", "suspend_idle"); n != 1 {
		t.Fatalf("held_actions_total{pressure,suspend_idle} = %g, want 1", n)
	}

	// Hugepage pressure instead of disk pressure: free hugepages cannot
	// host even the smallest ready build's memory (2048 MiB here —
	// admission would refuse it), so a fresh held lease is suspended at
	// the shortened threshold too. The shortage is set after the grant —
	// admission itself refuses when the hugepages are already short.
	l2, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "ci-job-2", "", nil)
	if err != nil {
		t.Fatalf("grant l2: %v", err)
	}
	sub.SetNodeInfo(substrate.NodeInfo{
		Status: "healthy", HugepagesTotal: 2048, HugepagesUsed: 2048,
		HugepageSizeBytes: 1024 * 1024,
	}, nil)
	svc.diskCapacity = statfsCapacity // back to the real reader (no disk pressure)
	svc.runHeldRules(ctx, base.Add(31*time.Minute))
	if !l2.Suspended {
		t.Fatal("hugepage pressure did not shorten the idle threshold")
	}
}

// reclaimSub simulates the GC reclaiming disk after each release: a
// wrapper around testSub whose Delete bumps a free-bytes counter.
type reclaimSub struct {
	*testSub
	freed *uint64 // added to the reader's free value on each Delete
}

func (r *reclaimSub) Delete(ctx context.Context, sandboxID string) error {
	if err := r.testSub.Delete(ctx, sandboxID); err != nil {
		return err
	}
	*r.freed += 5 // each reclaimed lease's builds free 5% of the store
	return nil
}

// TestCriticalReleasesOldestSuspendedFirstToRecovery (rule 5): under
// critical disk pressure the suspended held leases are released oldest
// suspension first, one per sweep tick, until free space is above the
// recovery level; a running held lease is never released. The
// free-space reader is driven by hand: a release frees no disk in its
// own tick — the builds only become GC candidates after gcAge — so the
// reclaimed space appears on a later tick, the way the GC delivers it.
func TestCriticalReleasesOldestSuspendedFirstToRecovery(t *testing.T) {
	t.Setenv("GC_DELETE", "1")
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.SetMetrics(metrics.NewBackendMetrics())
	svc.cfg.HeldIdleTimeout = 50 * time.Millisecond
	svc.cfg.CriticalDiskFreePct = 5
	svc.cfg.CriticalDiskRecoverPct = 10
	svc.cfg.TemplateStoragePath = t.TempDir()

	// Three held leases go idle and are suspended by rule 1; their
	// suspension times are then staggered by hand so "oldest first" is
	// observable: first < second < third.
	var suspended [3]*Lease
	for i := range suspended {
		l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "ci-job-"+string(rune('1'+i)), "", nil)
		if err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
		suspended[i] = l
	}
	time.Sleep(80 * time.Millisecond)
	svc.runHeldRules(ctx, time.Now())
	for i, l := range suspended {
		if !l.Suspended {
			t.Fatalf("setup: lease %d not suspended", i)
		}
	}
	older := time.Now().Add(-20 * time.Minute)
	mid := time.Now().Add(-10 * time.Minute)
	svc.store.mu.Lock()
	suspended[0].LastActionAt = older
	suspended[1].LastActionAt = mid
	suspended[2].LastActionAt = time.Now()
	for _, l := range suspended { // no activity since each suspension
		l.LastActive = older.Add(-time.Minute)
	}
	svc.store.mu.Unlock()

	// A running held lease exists when the pressure hits; it must never
	// be a candidate.
	running, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "ci-job-running", "", nil)
	if err != nil {
		t.Fatalf("grant running: %v", err)
	}

	// 2% free (< 5% critical). Each victim's reclaim (delivered here by
	// the injectable reader, as the GC would one gcAge later) frees 5%.
	// Rule 1 is switched off from here on: the focus is rule 5, and the
	// still-running lease must stay live for the never-released check.
	var free uint64 = 2
	svc.diskCapacity = func(string) (uint64, uint64, error) { return 100, free, nil }
	svc.cfg.HeldIdleTimeout = 0

	// Tick 1: exactly one release — the oldest suspension — even though
	// the level is far from recovered. The measured percentage in the
	// action's log line is the pre-release one.
	svc.runHeldRules(ctx, time.Now())
	if svc.lookup("c", suspended[0].ID) != nil {
		t.Fatal("oldest-suspended lease was not released first")
	}
	if svc.lookup("c", suspended[1].ID) == nil || svc.lookup("c", suspended[2].ID) == nil {
		t.Fatal("more than the oldest-suspended lease was released in one tick")
	}
	if got := calls(sub.Fake, "Delete "+suspended[0].SandboxID); got != 1 {
		t.Fatalf("oldest victim deletes = %d, want 1", got)
	}
	if n := heldCounter(t, svc, "critical", "release"); n != 1 {
		t.Fatalf("held_actions_total{critical,release} = %g, want 1", n)
	}
	if suspended[0].LastAction != "critical/release" || suspended[0].LastActionAt.IsZero() {
		t.Fatalf("action not recorded: %q at %v", suspended[0].LastAction, suspended[0].LastActionAt)
	}

	// Tick 2: the GC has reclaimed the first victim (2+5=7%), which is
	// above the 5% critical level — nothing more is released, even
	// though 7% is still below the 10% recovery level.
	free = 7
	svc.runHeldRules(ctx, time.Now())
	if svc.lookup("c", suspended[1].ID) == nil || svc.lookup("c", suspended[2].ID) == nil {
		t.Fatal("lease released while the disk was above the critical level")
	}

	// Tick 3: critical again (4%). The second-oldest goes; the tick
	// stops after its one release even though 4+5=9% is still below the
	// recovery level.
	free = 4
	svc.runHeldRules(ctx, time.Now())
	if svc.lookup("c", suspended[1].ID) != nil {
		t.Fatal("second-oldest suspended lease was not released")
	}
	if svc.lookup("c", suspended[2].ID) == nil {
		t.Fatal("more than one lease released in the tick")
	}

	// Tick 4: the reclaim lifts the level to 9%, still under the 10%
	// recovery — but above critical, so the third survives.
	free = 9
	svc.runHeldRules(ctx, time.Now())
	if svc.lookup("c", suspended[2].ID) == nil {
		t.Fatal("release continued past the recovery level")
	}
	if svc.lookup("c", running.ID) == nil || running.Suspended || running.State != "running" {
		t.Fatalf("running held lease touched: %+v", running)
	}
	if got := calls(sub.Fake, "Delete "+running.SandboxID); got != 0 {
		t.Fatalf("running held lease deleted %d times, want 0", got)
	}
	if n := heldCounter(t, svc, "critical", "release"); n != 2 {
		t.Fatalf("held_actions_total{critical,release} = %g, want 2", n)
	}
	// Oldest first across the two release ticks: suspended[0] before
	// suspended[1].
	log := sub.CallLog()
	first, second := -1, -1
	for i, c := range log {
		switch c {
		case "Delete " + suspended[0].SandboxID:
			first = i
		case "Delete " + suspended[1].SandboxID:
			second = i
		}
	}
	if first < 0 || second < 0 || first > second {
		t.Fatalf("release order not oldest-first: %s@%d, %s@%d in %v",
			suspended[0].SandboxID, first, suspended[1].SandboxID, second, log)
	}
}

// TestRulesSkipWhileDraining (rule 6): the held-lease rules do not run
// while the node is draining.
func TestRulesSkipWhileDraining(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.cfg.HeldIdleTimeout = time.Millisecond
	svc.cfg.HoldTTL = time.Hour // no expiry in this test: only the drain skip

	base := time.Now()
	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "ci-job", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.setHolderWithTTL("c", l.ID, "ci-job", "", 0); err != nil {
		t.Fatalf("set holder: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	svc.draining.Store(true)
	svc.runHeldRules(ctx, base.Add(time.Hour))
	if l.Suspended {
		t.Fatal("idle rule ran while draining")
	}
	if holder, _ := holdOf(t, svc, l.ID); holder != "ci-job" {
		t.Fatal("hold expiry ran while draining")
	}
	svc.draining.Store(false)
	svc.runHeldRules(ctx, base.Add(time.Hour))
	if !l.Suspended {
		t.Fatal("idle rule did not run after undrain")
	}
}

// TestHeldLimitsAPI: hold_ttl on create, hold_expires_at and
// last_action returned by the lease API, renewal through PUT with the
// same holder (409 for another), and expiry handing the lease back to
// the TTL sweeper — all through the HTTP surface.
func TestHeldLimitsAPI(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	svc.cfg.HoldTTL = 2 * time.Minute
	svc.cfg.HoldTTLMax = 10 * time.Minute
	svc.cfg.HeldIdleTimeout = 50 * time.Millisecond

	// Create with holder (the default hold window of HoldTTL).
	resp, body := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{
		"image": "py-base", "ttl": 300,
		"holder": "ci-job-42",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d (%v), want 201", resp.StatusCode, body)
	}
	heldExp, ok := body["hold_expires_at"].(string)
	if !ok || heldExp == "" {
		t.Fatalf("create response has no hold_expires_at: %v", body)
	}
	id := body["id"].(string)

	// A later PUT by the same holder renews from now: an explicit
	// hold_ttl (capped at HoldTTLMax, 10 min here) is taken from the
	// renewal instant, so the hold is visibly extended.
	time.Sleep(1100 * time.Millisecond)
	resp, body = doReq(t, "PUT", ts.URL+"/api/leases/"+id+"/holder", "token-a", map[string]any{"holder": "ci-job-42", "hold_ttl": 3600})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("renew = %d (%v), want 200", resp.StatusCode, body)
	}
	renewedExp := body["hold_expires_at"].(string)
	held0, err0 := time.Parse(time.RFC3339, heldExp)
	renewed0, err1 := time.Parse(time.RFC3339, renewedExp)
	if err0 != nil || err1 != nil {
		t.Fatalf("hold_expires_at parse: %v %v", err0, err1)
	}
	// Extended well past the original window, but not past the 10 min
	// cap measured from the renewal.
	if ext := renewed0.Sub(held0); ext < 5*time.Minute || ext > 11*time.Minute {
		t.Fatalf("renewal extended the hold by %s, want ~10 min (the cap)", ext)
	}

	// Another holder is refused (409); a malformed one is 400.
	resp, body = doReq(t, "PUT", ts.URL+"/api/leases/"+id+"/holder", "token-a", map[string]any{"holder": "other"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("other holder = %d (%v), want 409", resp.StatusCode, body)
	}
	resp, _ = doReq(t, "PUT", ts.URL+"/api/leases/"+id+"/holder", "token-a", map[string]any{"holder": "ci-job-42", "holder_url": "not-a-url"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad url = %d, want 400", resp.StatusCode)
	}

	// Rule 1 suspends the idle held lease and records the action; the
	// lease API returns last_action and last_action_at, and the
	// counter counts it.
	time.Sleep(80 * time.Millisecond)
	svc.runHeldRules(ctx, time.Now())
	_, detail := doReq(t, "GET", ts.URL+"/api/leases/"+id, "token-a", nil)
	if detail["last_action"] != "idle/suspend_idle" {
		t.Fatalf("last_action = %v, want idle/suspend_idle", detail["last_action"])
	}
	if at, _ := detail["last_action_at"].(string); at == "" {
		t.Fatalf("last_action_at missing: %v", detail)
	}
	_, list := doReq(t, "GET", ts.URL+"/api/leases", "token-a", nil)
	var listed map[string]any
	for _, row := range list["sandboxes"].([]any) {
		m := row.(map[string]any)
		if m["id"] == id {
			listed = m
		}
	}
	if listed == nil || listed["last_action"] != "idle/suspend_idle" || listed["hold_expires_at"] == "" {
		t.Fatalf("list row missing the held-lease fields: %v", listed)
	}
	// The suspended held lease resumes on next use through the ordinary
	// resume route, for its owner.
	resp, body = doReq(t, "POST", ts.URL+"/api/leases/"+id+"/resume", "token-a", nil)
	if resp.StatusCode != http.StatusOK || body["status"] != "running" {
		t.Fatalf("held resume = %d (%v), want 200 running", resp.StatusCode, body)
	}

	// A lapsed hold suspends the lease and never releases it, even with
	// its TTL long past: create with a tiny TTL, let the hold run out,
	// sweep — still there, suspended, hold_state lapsed.
	resp, body = doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{
		"image": "py-base", "ttl": 1, "holder": "short-job",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("short create = %d (%v), want 201", resp.StatusCode, body)
	}
	shortID := body["id"].(string)
	time.Sleep(80 * time.Millisecond)
	svc.store.mu.Lock()
	svc.store.leases[shortID].HoldExpiresAt = time.Now().Add(-time.Second)
	svc.store.leases[shortID].ExpiresAt = time.Now().Add(-time.Second)
	svc.store.mu.Unlock()
	svc.Shutdown(ctx) // stop the live sweeper; the checked sweep below is ours
	svc.sweepExpired(ctx)
	if l := svc.lookup("consumer-a", shortID); l == nil || !l.Suspended {
		t.Fatalf("lapsed hold: lease released or not suspended: %+v", l)
	}

	// The actions counter is exposed under its documented name.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer token-a")
	mresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	buf := make([]byte, 1<<16)
	n, _ := mresp.Body.Read(buf)
	mresp.Body.Close()
	if !strings.Contains(string(buf[:n]), "spoond_held_actions_total") {
		t.Fatalf("/metrics does not expose spoond_held_actions_total: %s", string(buf[:n]))
	}
}

// TestUnheldLeaseUntouchedByHeldRules: the rules only act on held
// leases — an unheld lease's TTL and idle behaviour are unchanged, and
// its lease rows carry no last_action.
func TestUnheldLeaseUntouchedByHeldRules(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.SetMetrics(metrics.NewBackendMetrics())
	svc.cfg.HeldIdleTimeout = time.Millisecond
	svc.cfg.HeldSuspendedRelease = time.Millisecond

	base := time.Now()
	cur := base
	svc.now = func() time.Time { return cur }
	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	cur = base.Add(time.Hour)
	svc.runHeldRules(ctx, cur)
	if l.Suspended || l.LastAction != "" {
		t.Fatalf("unheld lease acted on: suspended=%v last_action=%q", l.Suspended, l.LastAction)
	}
	if n := heldCounter(t, svc, "idle", "suspend_idle"); n != 0 {
		t.Fatalf("held_actions_total{idle,suspend_idle} = %g, want 0 for an unheld lease", n)
	}
}

// TestResumeHeldLeaseOwnerOrGateway: a held (non-persistent) lease that
// a rule suspended resumes through the ordinary resume route for its
// owner and for the SSH gateway's service token; another user gets 404.
// /api/leases/{id}/resume and /api/sandboxes/{id}/resume are the same
// route.
func TestResumeHeldLeaseOwnerOrGateway(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	seedImage(t, db, "py-base", 2048)
	svc.tokens["gw-tok"] = "gateway" // the gateway's token is a consumer token too
	svc.SetGatewayToken("gw-tok")
	svc.cfg.HeldIdleTimeout = 30 * time.Millisecond
	ctx := context.Background()

	a, err := svc.grant(ctx, "consumer-a", "py-base", time.Minute, false, "", nil, "ci-job", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	b, err := svc.grant(ctx, "consumer-a", "py-base", time.Minute, false, "", nil, "ci-job-2", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	svc.runHeldRules(ctx, time.Now())
	if !a.Suspended || !b.Suspended {
		t.Fatal("setup: held leases not suspended")
	}
	resp, _ := doReq(t, "POST", ts.URL+"/api/leases/"+a.ID+"/resume", "token-b", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("another user's resume = %d, want 404", resp.StatusCode)
	}
	resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+a.ID+"/resume", "token-a", nil)
	if resp.StatusCode != http.StatusOK || body["status"] != "running" {
		t.Fatalf("owner resume of a held lease = %d (%v), want 200 running", resp.StatusCode, body)
	}
	resp, body = doReq(t, "POST", ts.URL+"/api/sandboxes/"+b.ID+"/resume", "gw-tok", nil)
	if resp.StatusCode != http.StatusOK || body["status"] != "running" {
		t.Fatalf("gateway resume = %d (%v), want 200 running", resp.StatusCode, body)
	}
	resp, _ = doReq(t, "POST", ts.URL+"/api/leases/no-such-lease/resume", "gw-tok", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown lease resume = %d, want 404", resp.StatusCode)
	}
}

// TestLapseOfSuspendedHoldDoesNotRelease: a held lease suspended by rule
// 1 whose hold then lapses is not released by the lapse; the lapse
// restarts rule 2's clock, and renewing restores a normal hold.
func TestLapseOfSuspendedHoldDoesNotRelease(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.SetMetrics(metrics.NewBackendMetrics())

	base := time.Now()
	svc.cfg.HoldTTL = 2 * time.Hour
	svc.cfg.HeldIdleTimeout = time.Minute
	svc.cfg.HeldSuspendedRelease = 7 * 24 * time.Hour
	cur := base
	svc.now = func() time.Time { return cur }

	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.setHolderWithTTL("c", l.ID, "ci-job", "", 0); err != nil {
		t.Fatalf("set holder: %v", err)
	}
	cur = base.Add(time.Hour)
	svc.runHeldRules(ctx, cur)
	if !l.Suspended {
		t.Fatal("setup: held lease not suspended by rule 1")
	}

	cur = base.Add(24 * time.Hour)
	svc.expireHolds(ctx, cur)
	if svc.lookup("c", l.ID) == nil {
		t.Fatal("suspended held lease released by its hold's lapse")
	}
	if n := heldCounter(t, svc, "expiry", "release"); n != 0 {
		t.Fatalf("held_actions_total{expiry,release} = %g, want 0", n)
	}
	if l.LastAction != "expiry/suspend_lapsed" || !l.LastActionAt.Equal(cur) {
		t.Fatalf("lapse not recorded: %q at %v", l.LastAction, l.LastActionAt)
	}

	// Rule 2's clock runs from the lapse, not from the earlier suspend.
	cur = base.Add(24*time.Hour + 7*24*time.Hour - time.Minute)
	svc.runHeldRules(ctx, cur)
	if svc.lookup("c", l.ID) == nil {
		t.Fatal("released before the stale limit counted from the lapse")
	}

	// Renewing restores a normal hold: an expiry again, and rule 2 no
	// longer counts the lapse (a fresh suspend restarts it).
	if _, err := svc.renewHolder("c", l.ID, "ci-job", "", 0); err != nil {
		t.Fatalf("renew after lapse: %v", err)
	}
	if _, expires := holdOf(t, svc, l.ID); !expires.Equal(cur.Add(2 * time.Hour)) {
		t.Fatalf("renewed hold expires %v, want %v", expires, cur.Add(2*time.Hour))
	}
}

// TestOnlyRuleSuspendedLeasesAreReleased: a held lease suspended by hand
// is never released by rules 2 or 5, and neither is one a rule
// suspended that was used again since.
func TestOnlyRuleSuspendedLeasesAreReleased(t *testing.T) {
	t.Setenv("GC_DELETE", "1")
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.SetMetrics(metrics.NewBackendMetrics())
	svc.cfg.HeldSuspendedRelease = time.Hour
	svc.cfg.HeldIdleTimeout = 0
	svc.cfg.CriticalDiskFreePct = 5
	svc.cfg.CriticalDiskRecoverPct = 10
	svc.cfg.TemplateStoragePath = t.TempDir()
	svc.diskCapacity = func(string) (uint64, uint64, error) { return 100, 1, nil } // critical

	manual, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "by-hand", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.pauseLease(ctx, manual, false); err != nil {
		t.Fatalf("manual suspend: %v", err)
	}
	used, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "used-again", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.pauseLease(ctx, used, false); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	svc.store.mu.Lock()
	used.LastAction, used.LastActionAt = "idle/suspend_idle", time.Now().Add(-2*time.Hour)
	used.LastActive = time.Now().Add(-time.Hour) // touched after the rule's suspend
	svc.store.mu.Unlock()

	svc.runHeldRules(ctx, time.Now().Add(30*24*time.Hour))
	if svc.lookup("c", manual.ID) == nil {
		t.Fatal("a held lease suspended by hand was released")
	}
	if svc.lookup("c", used.ID) == nil {
		t.Fatal("a lease used since its rule suspension was released")
	}
}

// TestCriticalRefusesUnderDryRunGC: with GC_DELETE unset the GC frees
// nothing, so rule 5 releases nothing however full the disk is.
func TestCriticalRefusesUnderDryRunGC(t *testing.T) {
	t.Setenv("GC_DELETE", "")
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.SetMetrics(metrics.NewBackendMetrics())
	svc.cfg.HeldIdleTimeout = 50 * time.Millisecond
	svc.cfg.CriticalDiskFreePct = 5
	svc.cfg.CriticalDiskRecoverPct = 10
	svc.cfg.TemplateStoragePath = t.TempDir()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "ci-job", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	svc.runHeldRules(ctx, time.Now())
	if !l.Suspended {
		t.Fatal("setup: not suspended by rule 1")
	}
	svc.cfg.HeldIdleTimeout = 0
	svc.diskCapacity = func(string) (uint64, uint64, error) { return 100, 1, nil }
	for i := 0; i < 5; i++ {
		svc.runHeldRules(ctx, time.Now().Add(time.Duration(i)*time.Minute))
	}
	if svc.lookup("c", l.ID) == nil {
		t.Fatal("rule 5 released a lease while the GC is in dry-run")
	}
	if n := heldCounter(t, svc, "critical", "release"); n != 0 {
		t.Fatalf("held_actions_total{critical,release} = %g, want 0", n)
	}
}

// TestCreateHoldTTLCappedAndNeedsHolder: hold_ttl on CREATE is capped
// at HoldTTLMax, and it is ignored when no holder is given (the lease
// is unheld and carries no hold at all).
func TestCreateHoldTTLCappedAndNeedsHolder(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	seedImage(t, db, "py-base", 2048)
	svc.cfg.HoldTTL = time.Minute
	svc.cfg.HoldTTLMax = 5 * time.Minute

	// An explicit hold_ttl of an hour is capped at the 5 min maximum,
	// measured from the create.
	resp, body := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{
		"image": "py-base", "ttl": 300, "holder": "ci-job", "hold_ttl": 3600,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d (%v), want 201", resp.StatusCode, body)
	}
	exp, err := time.Parse(time.RFC3339, body["hold_expires_at"].(string))
	if err != nil {
		t.Fatalf("hold_expires_at = %v: %v", body["hold_expires_at"], err)
	}
	if until := time.Until(exp); until > 6*time.Minute || until < 4*time.Minute {
		t.Fatalf("capped hold runs %s, want ~5 min", until.Round(time.Second))
	}

	// hold_ttl without a holder: ignored — no hold, no expiry.
	resp, body = doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{
		"image": "py-base", "ttl": 300, "hold_ttl": 3600,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("unheld create = %d (%v), want 201", resp.StatusCode, body)
	}
	if exp := body["hold_expires_at"].(string); exp != "" {
		t.Fatalf("unheld create has hold_expires_at %q, want empty", exp)
	}
	svc.store.mu.Lock()
	l := svc.store.leases[body["id"].(string)]
	held := l.held()
	svc.store.mu.Unlock()
	if held {
		t.Fatal("create with hold_ttl but no holder made the lease held")
	}
}

// Create, the holder PUT and fork return hold_state like a lease read.
func TestHoldStateOnCreateAndPut(t *testing.T) {
	ts, _, db, _ := newTestServerWithService(t)
	seedImage(t, db, "py-base", 2048)
	resp, body := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{"image": "py-base", "ttl": 60, "holder": "ci-job"})
	if resp.StatusCode != http.StatusCreated || body["hold_state"] != "active" {
		t.Fatalf("create = %d, hold_state %v, want 201 active", resp.StatusCode, body["hold_state"])
	}
	id := body["id"].(string)
	resp, body = doReq(t, "PUT", ts.URL+"/api/leases/"+id+"/holder", "token-a", map[string]any{"holder": "ci-job"})
	if resp.StatusCode != http.StatusOK || body["hold_state"] != "active" {
		t.Fatalf("renew = %d, hold_state %v, want 200 active", resp.StatusCode, body["hold_state"])
	}
	resp, body = doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{"image": "py-base", "ttl": 60})
	if resp.StatusCode != http.StatusCreated || body["hold_state"] != "" {
		t.Fatalf("unheld create hold_state = %v, want empty", body["hold_state"])
	}
}

// TestResumeRunningLeaseIsNoop: resuming a lease that is already running
// must not restore its pause build again (that would roll the guest's
// memory back); it returns the lease unchanged.
func TestResumeRunningLeaseIsNoop(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.suspend(ctx, "c", l.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if _, err := svc.resume(ctx, "c", l.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	creates := func() int {
		n := 0
		for _, c := range sub.Fake.CallLog() {
			if strings.HasPrefix(c, "Create ") {
				n++
			}
		}
		return n
	}
	before := creates()
	got, err := svc.resume(ctx, "c", l.ID)
	if err != nil {
		t.Fatalf("resume of a running lease: %v", err)
	}
	if got.State != "running" || got.Suspended {
		t.Fatalf("lease after no-op resume: state %q suspended %v", got.State, got.Suspended)
	}
	if n := creates(); n != before {
		t.Fatalf("resume of a running lease restored the snapshot again (%d creates, want %d)", n, before)
	}
}
