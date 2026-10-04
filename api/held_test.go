package api

// Limits on held leases that act automatically (2.1): every rule fires
// at its threshold and not before, a heartbeat prevents the idle
// suspend, renewal extends a hold and is capped at the maximum, expiry
// clears the holder and hands the lease back to the TTL sweep, pressure
// shortens the idle threshold, the critical rule releases
// oldest-suspended first and stops at the recovery level, a running
// held lease is never released, and every action is counted and
// recorded. The tests use the fake substrate, the injectable clock
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
	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "ci-job", "")
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

	active, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "ci-job", "")
	if err != nil {
		t.Fatalf("grant active: %v", err)
	}
	stale, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "ci-job-2", "")
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
	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "ci-job", "")
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

// TestHoldExpiresAndHandsBackToTTLSweep (rule 3): a hold lasts HoldTTL
// from when it was set, at most HoldTTLMax for an explicit hold_ttl;
// past expiry holder and holder_url are cleared and the lease follows
// the normal TTL rules — an already-expired TTL releases it at the next
// sweep.
func TestHoldExpiresAndHandsBackToTTLSweep(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.SetMetrics(metrics.NewBackendMetrics())

	base := time.Now()
	svc.cfg.HoldTTL = time.Hour
	cur := base
	svc.now = func() time.Time { return cur }

	l, err := svc.grant(ctx, "c", "py-base", 50*time.Millisecond, false, "", nil, "ci-job", "")
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	// The hold's clock starts when the holder is set (setHolderWithTTL
	// uses svc.now = base); the grant itself must not carry one.
	if _, zero := holdOf(t, svc, l.ID); !zero.IsZero() {
		t.Fatalf("grant stamped a hold itself: %v", zero)
	}
	if _, err := svc.setHolderWithTTL("c", l.ID, "ci-job", "", 0); err != nil {
		t.Fatalf("set holder: %v", err)
	}
	holder, expires := holdOf(t, svc, l.ID)
	if holder != "ci-job" || !expires.Equal(base.Add(time.Hour)) {
		t.Fatalf("hold = %q expiring %v, want ci-job at %v", holder, expires, base.Add(time.Hour))
	}

	// Before expiry the lease is still held and survives its TTL.
	cur = base.Add(30 * time.Minute)
	svc.sweepExpired(ctx)
	if svc.lookup("c", l.ID) == nil {
		t.Fatal("held lease swept before its hold expired")
	}

	// At +2 h the hold has expired: the holder is cleared...
	cur = base.Add(2 * time.Hour)
	svc.expireHolds(ctx, cur)
	holder, expires = holdOf(t, svc, l.ID)
	if holder != "" || !expires.IsZero() {
		t.Fatalf("expired hold not cleared: holder=%q expires=%v", holder, expires)
	}
	if n := heldCounter(t, svc, "expiry", "expire"); n != 1 {
		t.Fatalf("held_actions_total{expiry,expire} = %g, want 1", n)
	}
	if l.LastAction != "expiry/expire" {
		t.Fatalf("last_action = %q, want expiry/expire", l.LastAction)
	}

	// ...and the lease, whose TTL passed long ago, is released by the
	// next sweep.
	svc.sweepExpired(ctx)
	if svc.lookup("c", l.ID) != nil {
		t.Fatal("lease survived the sweep after its hold expired")
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

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "ci-job", "")
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
	svc.cfg.TemplateStoragePath = t.TempDir() // an empty dir is never under pressure
	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "ci-job", "")
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
	l2, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "ci-job-2", "")
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
// suspension first until free space is above the recovery level; a
// running held lease is never released.
func TestCriticalReleasesOldestSuspendedFirstToRecovery(t *testing.T) {
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
		l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "ci-job-"+string(rune('1'+i)), "")
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
	svc.store.mu.Unlock()

	// A running held lease exists when the pressure hits; it must never
	// be a candidate.
	running, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "ci-job-running", "")
	if err != nil {
		t.Fatalf("grant running: %v", err)
	}

	// 2% free (< 5% critical); every release reclaims 5% (the GC having
	// run), so after two releases free space is at 12% — above the 10%
	// recovery level — and the third-suspended lease survives.
	var free uint64 = 2
	svc.diskCapacity = func(string) (uint64, uint64, error) { return 100, free, nil }
	svc.sub = &reclaimSub{testSub: sub, freed: &free}

	svc.runHeldRules(ctx, time.Now())

	if svc.lookup("c", suspended[0].ID) != nil {
		t.Fatal("oldest-suspended lease was not released first")
	}
	if svc.lookup("c", suspended[1].ID) != nil {
		t.Fatal("second-oldest suspended lease was not released")
	}
	if svc.lookup("c", suspended[2].ID) == nil {
		t.Fatal("release continued past the recovery level")
	}
	if svc.lookup("c", running.ID) == nil || running.Suspended || running.State != "running" {
		t.Fatalf("running held lease touched: %+v", running)
	}
	if got := calls(sub.Fake, "Delete "+running.SandboxID); got != 0 {
		t.Fatalf("running held lease deleted %d times, want 0", got)
	}
	// Oldest first: suspended[0]'s sandbox was deleted before
	// suspended[1]'s.
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
	if n := heldCounter(t, svc, "critical", "release"); n != 2 {
		t.Fatalf("held_actions_total{critical,release} = %g, want 2", n)
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
	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "ci-job", "")
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
	// The suspended held lease resumes on next use through the
	// owner-blind route (the SSH gateway's path).
	resp, body = doReq(t, "POST", ts.URL+"/api/leases/"+id+"/resume", "token-b", nil)
	if resp.StatusCode != http.StatusOK || body["status"] != "running" {
		t.Fatalf("held resume = %d (%v), want 200 running", resp.StatusCode, body)
	}

	// Expiry hands the lease back to the TTL sweep: create with a tiny
	// TTL, let the hold run out, sweep — released.
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
	if l := svc.lookup("consumer-a", shortID); l != nil {
		t.Fatal("lease whose hold expired was not handed back to the TTL sweep")
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
	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "")
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

// TestHeldResumeRouteOwnerBlind: POST /api/leases/{id}/resume answers
// 404 for an unheld lease (the owner-checked route serves those) and
// for an unknown id.
func TestHeldResumeRouteOwnerBlind(t *testing.T) {
	ts, svc, db, _ := newTestServerWithService(t)
	seedImage(t, db, "py-base", 2048)

	l, err := svc.grant(context.Background(), "consumer-a", "py-base", time.Minute, true, "", nil, "", "")
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	resp, _ := doReq(t, "POST", ts.URL+"/api/leases/"+l.ID+"/resume", "token-b", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unheld lease resume = %d, want 404", resp.StatusCode)
	}
	resp, _ = doReq(t, "POST", ts.URL+"/api/leases/no-such-lease/resume", "token-b", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown lease resume = %d, want 404", resp.StatusCode)
	}
}
