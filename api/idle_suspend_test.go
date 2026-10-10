package api

// Per-lease idle reclamation (2.5, #129 part 2): the effective
// idle_suspend value (absent → host default; 0 → never; bounds;
// non-persistent 400), the policy PUT, persistence across a backend
// restart, the sweep suspending after the threshold and not before,
// files and guest dial counting as activity, the setting overriding
// IDLE_TIMEOUT_SECS and held rule 1 in both directions, the shared disk
// floor skipping, stale release of an idle-suspended lease and never of
// a preempted one, and exec auto-resuming an idle-suspended lease (with
// its /dev/shm marker) while a hand-suspended lease still answers 409.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/metrics"
	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
)

// idleSuspendsCounter reads spoond_idle_suspends_total from the
// service's registry.
func idleSuspendsCounter(t *testing.T, svc *Service) float64 {
	t.Helper()
	if svc.metrics == nil {
		t.Fatal("metrics not installed")
	}
	return counterValue(t, svc.metrics.IdleSuspendsTotal)
}

// TestIdleSuspendEffectiveValue: absent → host default; 0 → never; a
// lease with its own value keeps it; the bounds are enforced.
func TestIdleSuspendEffectiveValue(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	svc.cfg.IdleSuspendDefault = 300
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if l.IdleSuspend != idleSuspendHost {
		t.Fatalf("stored idle_suspend = %d, want -1 (the host default)", l.IdleSuspend)
	}
	if got := svc.effectiveIdleSuspend(l); got != 300 {
		t.Fatalf("effective idle_suspend = %d, want the host default 300", got)
	}

	// The lease's own 0 (never) beats the host default.
	if _, err := svc.setIdlePolicy(l, 0); err != nil {
		t.Fatalf("setIdlePolicy: %v", err)
	}
	if got := svc.effectiveIdleSuspend(l); got != 0 {
		t.Fatalf("effective idle_suspend = %d, want the lease's own 0", got)
	}

	// Bounds: 0 (never) and 60..604800 are valid, anything else is not.
	if err := validateIdleSuspend(0); err != nil {
		t.Fatalf("validateIdleSuspend(0): %v", err)
	}
	for _, ok := range []int64{60, 3600, 604800} {
		if err := validateIdleSuspend(ok); err != nil {
			t.Fatalf("validateIdleSuspend(%d): %v", ok, err)
		}
	}
	for _, bad := range []int64{-1, 59, 604801} {
		err := validateIdleSuspend(bad)
		if err == nil {
			t.Fatalf("validateIdleSuspend(%d) accepted", bad)
		}
		if !strings.Contains(err.Error(), "idle_suspend") {
			t.Fatalf("validateIdleSuspend(%d) error does not name the field: %v", bad, err)
		}
	}
}

// TestIdleSuspendCreateValidation: a non-zero idle_suspend on a
// non-persistent lease answers 400; a persistent one is accepted; the
// bounds are enforced at create too.
func TestIdleSuspendCreateValidation(t *testing.T) {
	h, _, _ := newShareTestServer(t)

	create := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/leases", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok-a")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	// A non-persistent lease may not set a non-zero value.
	rec := create(`{"image":"py-base","ttl":120,"idle_suspend":60}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-persistent idle_suspend=60: %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "idle_suspend") {
		t.Fatalf("non-persistent refusal does not name the field: %s", rec.Body)
	}
	// A persistent lease accepts 0, 60 and 604800.
	for _, ok := range []string{"0", "60", "604800"} {
		rec := create(`{"image":"py-base","ttl":120,"persistent":true,"idle_suspend":` + ok + `}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("persistent idle_suspend=%s: %d, want 201: %s", ok, rec.Code, rec.Body)
		}
	}
	// Out-of-bounds values are 400, naming the field.
	for _, bad := range []string{"59", "604801", "-5"} {
		rec := create(`{"image":"py-base","ttl":120,"persistent":true,"idle_suspend":` + bad + `}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("idle_suspend=%s: %d, want 400", bad, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "idle_suspend") {
			t.Fatalf("idle_suspend=%s refusal does not name the field: %s", bad, rec.Body)
		}
	}
}

// TestIdleSuspendPolicyPut: the owner and an admin may set the policy;
// another owner gets 404; a non-persistent lease answers 400; the row
// field shows the effective value and an idle_policy event is emitted.
func TestIdleSuspendPolicyPut(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.cfg.IdleSuspendDefault = 300

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	all := svc.Subscribe(EventFilter{})
	if _, err := svc.setIdlePolicy(l, 120); err != nil {
		t.Fatalf("setIdlePolicy: %v", err)
	}
	all.Close()
	if l.IdleSuspend != 120 {
		t.Fatalf("stored idle_suspend = %d, want 120", l.IdleSuspend)
	}
	events := eventsFor(collectEvents(all.C), l.ID)
	found := false
	for _, ev := range events {
		if ev.Type == LeaseIdlePolicy {
			found = true
			if !strings.Contains(ev.Detail, "120") {
				t.Fatalf("idle_policy detail = %q, want the effective seconds", ev.Detail)
			}
		}
	}
	if !found {
		t.Fatalf("no idle_policy event among %v", eventTypes(events))
	}

	// The detail map shows the effective value.
	if got, _ := svc.leaseDetailMap(l)["idle_suspend"].(int64); got != 120 {
		t.Fatalf("detail idle_suspend = %v, want 120", svc.leaseDetailMap(l)["idle_suspend"])
	}
}

// TestIdleSuspendPolicyPutHTTP: the route's owner/admin scoping and
// validation, including the non-persistent 400 and the admin branch.
func TestIdleSuspendPolicyPutHTTP(t *testing.T) {
	h, _, _ := newShareTestServer(t)
	lid := createLeaseAs(h, "tok-a")
	if lid == "" {
		t.Fatal("a could not create lease")
	}
	put := func(token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/api/leases/"+lid+"/idle-policy", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	// A non-persistent lease (createLeaseAs) refuses a non-zero value.
	if rec := put("tok-a", `{"idle_suspend":60}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-persistent PUT idle_suspend=60: %d, want 400: %s", rec.Code, rec.Body)
	}
	// Another owner gets 404.
	if rec := put("tok-b", `{"idle_suspend":120}`); rec.Code != http.StatusNotFound {
		t.Fatalf("other owner PUT: %d, want 404", rec.Code)
	}
	// Out-of-bounds is 400 naming the field.
	for _, bad := range []string{"59", "604801"} {
		rec := put("tok-a", `{"idle_suspend":`+bad+`}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("PUT idle_suspend=%s: %d, want 400", bad, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "idle_suspend") {
			t.Fatalf("PUT idle_suspend=%s error does not name the field: %s", bad, rec.Body)
		}
	}
	// Setting 0 on a non-persistent lease is allowed (never).
	rec := put("tok-a", `{"idle_suspend":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT idle_suspend=0: %d, want 200: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"idle_suspend":0`) {
		t.Fatalf("PUT response missing idle_suspend 0: %s", rec.Body)
	}

	// An admin may set another owner's persistent lease: create one as a
	// and PUT it as the admin (a non-zero value is allowed here).
	preq := httptest.NewRequest("POST", "/api/leases",
		strings.NewReader(`{"image":"py-base","ttl":120,"persistent":true}`))
	preq.Header.Set("Authorization", "Bearer tok-a")
	preq.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, preq)
	if rec.Code != http.StatusCreated {
		t.Fatalf("persistent create: %d, want 201: %s", rec.Code, rec.Body)
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	plid, _ := created["id"].(string)
	areq := httptest.NewRequest("PUT", "/api/leases/"+plid+"/idle-policy", strings.NewReader(`{"idle_suspend":120}`))
	areq.Header.Set("Authorization", "Bearer admin-tok")
	areq.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, areq)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin PUT: %d, want 200: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"idle_suspend":120`) {
		t.Fatalf("admin PUT response missing idle_suspend 120: %s", rec.Body)
	}
}

// TestIdleSuspendForkNonPersistentDefault: a non-persistent fork of a
// lease with its own idle_suspend takes the host default (stored -1,
// effective 0), keeping held rule 1 in force for it; a persistent fork
// inherits the source's value (2.5, #129 part 2).
func TestIdleSuspendForkNonPersistentDefault(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.cfg.IdleSuspendDefault = 300

	src, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant src: %v", err)
	}
	if _, err := svc.setIdlePolicy(src, 120); err != nil {
		t.Fatalf("setIdlePolicy: %v", err)
	}

	// A non-persistent fork cannot carry a value: stored -1, effective 0.
	np, _, err := svc.fork(ctx, "c", src.ID, 1, false, time.Hour, "", "")
	if err != nil {
		t.Fatalf("fork non-persistent: %v", err)
	}
	if np[0].IdleSuspend != idleSuspendHost {
		t.Fatalf("non-persistent fork stored idle_suspend = %d, want -1", np[0].IdleSuspend)
	}
	if got := svc.effectiveIdleSuspend(np[0]); got != 0 {
		t.Fatalf("non-persistent fork effective idle_suspend = %d, want 0", got)
	}
	// A persistent fork inherits the source's 120.
	p, _, err := svc.fork(ctx, "c", src.ID, 1, true, time.Hour, "", "")
	if err != nil {
		t.Fatalf("fork persistent: %v", err)
	}
	if p[0].IdleSuspend != 120 {
		t.Fatalf("persistent fork stored idle_suspend = %d, want 120", p[0].IdleSuspend)
	}
}

// TestIdleSuspendNonPersistentNeverSuspended: a non-persistent lease
// can never be idle-suspended (there is no snapshot to resume from),
// whatever the host default; the held rules that once covered held
// non-persistent leases are gone (FS5).
func TestIdleSuspendNonPersistentNeverSuspended(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.cfg.IdleSuspendDefault = 300

	base := time.Now()
	svc.now = func() time.Time { return base }
	l, err := svc.grant(ctx, "c", "py-base", time.Hour, false, "", nil, "ci-job", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.store.mu.Lock()
	l.LastActive = base
	svc.store.mu.Unlock()

	if got := svc.effectiveIdleSuspend(l); got != 0 {
		t.Fatalf("non-persistent effective idle_suspend = %d, want 0", got)
	}
	svc.suspendIdleLeases(ctx, base.Add(2*time.Minute))
	svc.sweepExpired(ctx)
	if l.Suspended {
		t.Fatal("a non-persistent lease was idle-suspended")
	}
}

// TestIdleSuspendPersistsAcrossRestart: an idle_suspend set on a lease
// survives a backend restart (a new Service over the same store).
func TestIdleSuspendPersistsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "spoond.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sub := newTestSub()
	seedImage(t, db, "py-base", 2048)
	svc := NewService(sub, db, map[string]string{"t": "c"}, ServiceConfig{DefaultTTL: time.Minute, MaxTTL: 10 * time.Minute})
	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.setIdlePolicy(l, 120); err != nil {
		t.Fatalf("setIdlePolicy: %v", err)
	}
	svc.Shutdown(ctx)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db2, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { db2.Close() })
	svc2 := NewService(sub, db2, map[string]string{"t": "c"}, ServiceConfig{DefaultTTL: time.Minute, MaxTTL: 10 * time.Minute})
	if err := svc2.LoadState(ctx); err != nil {
		t.Fatalf("load state: %v", err)
	}
	got := svc2.lookup("c", l.ID)
	if got == nil {
		t.Fatalf("lease %s not loaded", l.ID)
	}
	if got.IdleSuspend != 120 {
		t.Fatalf("loaded idle_suspend = %d, want 120", got.IdleSuspend)
	}
}

// TestIdleSuspendSweepAtThreshold: the sweep suspends a lease idle past
// its own idle_suspend and not one tick earlier; the generation does
// not change and no sandbox is deleted.
func TestIdleSuspendSweepAtThreshold(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.SetMetrics(metrics.NewBackendMetrics())

	base := time.Now()
	svc.now = func() time.Time { return base }
	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.setIdlePolicy(l, 60); err != nil {
		t.Fatalf("setIdlePolicy: %v", err)
	}
	svc.store.mu.Lock()
	l.LastActive = base
	svc.store.mu.Unlock()
	sbID := l.SandboxID

	// 59 s: below the threshold, still running.
	svc.suspendIdleLeases(ctx, base.Add(59*time.Second))
	if l.Suspended {
		t.Fatal("idle-suspended before the threshold")
	}
	// 61 s: suspended through the pause path, nothing deleted.
	svc.suspendIdleLeases(ctx, base.Add(61*time.Second))
	if !l.Suspended || l.State != "suspended" {
		t.Fatalf("lease not suspended past the threshold: state=%s", l.State)
	}
	if got := calls(sub.Fake, "Pause "+sbID); got != 1 {
		t.Fatalf("pause calls = %d, want 1", got)
	}
	if got := calls(sub.Fake, "Delete "+sbID); got != 0 {
		t.Fatalf("idle suspend deleted the sandbox (%d deletes), want 0", got)
	}
	if l.LastAction != idleSuspendRule+"/"+heldActionSuspendIdle || l.LastActionAt.IsZero() {
		t.Fatalf("action not recorded: %q at %v", l.LastAction, l.LastActionAt)
	}
	if l.Generation != 1 {
		t.Fatalf("generation = %d, want 1 (the pause continues the memory)", l.Generation)
	}
	if n := idleSuspendsCounter(t, svc); n != 1 {
		t.Fatalf("spoond_idle_suspends_total = %g, want 1", n)
	}
}

// TestIdleSuspendEmitsEvent: the sweep emits an idle_suspended event
// whose detail names how long the lease had been idle.
func TestIdleSuspendEmitsEvent(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	base := time.Now()
	svc.now = func() time.Time { return base }
	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.setIdlePolicy(l, 60); err != nil {
		t.Fatalf("setIdlePolicy: %v", err)
	}
	svc.store.mu.Lock()
	l.LastActive = base
	svc.store.mu.Unlock()

	all := svc.Subscribe(EventFilter{})
	svc.suspendIdleLeases(ctx, base.Add(2*time.Minute))
	all.Close()

	events := eventsFor(collectEvents(all.C), l.ID)
	found := false
	for _, ev := range events {
		if ev.Type == LeaseIdleSuspended {
			found = true
			if !strings.Contains(ev.Detail, "idle for") {
				t.Fatalf("idle_suspended detail = %q, want \"idle for <duration>\"", ev.Detail)
			}
		}
	}
	if !found {
		t.Fatalf("no idle_suspended event among %v", eventTypes(events))
	}
}

// TestIdleSuspendZeroNeverSwept: idle_suspend 0 (or a 0 host default)
// never suspends on the idle sweep.
func TestIdleSuspendZeroNeverSwept(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	base := time.Now()
	svc.now = func() time.Time { return base }
	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.setIdlePolicy(l, 0); err != nil {
		t.Fatalf("setIdlePolicy: %v", err)
	}
	svc.store.mu.Lock()
	l.LastActive = base.Add(-24 * time.Hour)
	svc.store.mu.Unlock()
	svc.suspendIdleLeases(ctx, base)
	if l.Suspended {
		t.Fatal("idle_suspend 0 must never suspend")
	}
}

// TestIdleSuspendFilesCountsAsActivity: a files PUT moves LastActive
// and keeps the lease out of the idle sweep.
func TestIdleSuspendFilesCountsAsActivity(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	ctx := context.Background()
	l, err := svc.grant(ctx, "consumer-a", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.setIdlePolicy(l, 60); err != nil {
		t.Fatalf("setIdlePolicy: %v", err)
	}
	id := l.ID

	svc.store.mu.Lock()
	l.LastActive = time.Now().Add(-2 * time.Minute)
	svc.store.mu.Unlock()
	sbID := l.SandboxID

	resp := filesDo(t, "PUT", filesURL(ts, id, "/act.txt", ""), "token-a", "x")
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("files PUT: %d, want 201", resp.StatusCode)
	}
	svc.suspendIdleLeases(ctx, time.Now())
	if l.Suspended {
		t.Fatal("files activity did not keep the lease out of the idle sweep")
	}
	if got := calls(sub.Fake, "Pause "+sbID); got != 0 {
		t.Fatalf("pause calls = %d, want 0", got)
	}
	// The sweep with no activity since would suspend it, proving the
	// threshold is what kept it.
	svc.store.mu.Lock()
	since := l.LastActive
	svc.store.mu.Unlock()
	svc.suspendIdleLeases(ctx, since.Add(2*time.Minute))
	if !l.Suspended {
		t.Fatal("lease not suspended once the files activity aged out")
	}
}

// TestIdleSuspendGuestDialCountsAsActivity: a guest dial attach moves
// LastActive and keeps the lease out of the idle sweep.
func TestIdleSuspendGuestDialCountsAsActivity(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)
	ctx := context.Background()
	// A persistent lease with a 60 s idle_suspend: two minutes without
	// the dial's touch would put it past its threshold.
	pl, err := svc.grant(ctx, "consumer-a", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.setIdlePolicy(pl, 60); err != nil {
		t.Fatalf("setIdlePolicy: %v", err)
	}
	id := pl.ID
	host, port := newEchoServer(t)
	pointHostIP(t, svc, id, host, port)

	svc.store.mu.Lock()
	l := svc.store.leases[id]
	l.LastActive = time.Now().Add(-2 * time.Minute)
	svc.store.mu.Unlock()
	sbID := l.SandboxID
	before := l.LastActive

	ws, _, err := dialGuest(t, ts, id, port, "token-a")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()
	// The handler touches the lease before the upgrade; give the
	// goroutine a moment to have done so.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		svc.store.mu.Lock()
		moved := l.LastActive.After(before)
		svc.store.mu.Unlock()
		if moved {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	svc.store.mu.Lock()
	moved := l.LastActive.After(before)
	svc.store.mu.Unlock()
	if !moved {
		t.Fatal("guest dial did not move LastActive")
	}
	svc.suspendIdleLeases(ctx, time.Now())
	if l.Suspended {
		t.Fatal("dial activity did not keep the lease out of the idle sweep")
	}
	if got := calls(svc.sub.(*testSub).Fake, "Pause "+sbID); got != 0 {
		t.Fatalf("pause calls = %d, want 0", got)
	}
}

// TestIdleSuspendOverridesIdleTimeout: a lease's own idle_suspend is
// the only idle threshold — there is no plain idle sweep left (FS5) —
// so a shorter value suspends at its own threshold and a longer one is
// untouched by a sweep at a shorter time.
func TestIdleSuspendOverridesIdleTimeout(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	base := time.Now()
	svc.now = func() time.Time { return base }
	short, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant short: %v", err)
	}
	long, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant long: %v", err)
	}
	if _, err := svc.setIdlePolicy(short, 60); err != nil { // shorter than 1 h
		t.Fatalf("setIdlePolicy short: %v", err)
	}
	if _, err := svc.setIdlePolicy(long, 7200); err != nil { // longer than 1 h
		t.Fatalf("setIdlePolicy long: %v", err)
	}
	svc.store.mu.Lock()
	short.LastActive = base
	long.LastActive = base
	svc.store.mu.Unlock()

	// 61 s: the short lease's own threshold fires; the long lease is
	// still running.
	svc.suspendIdleLeases(ctx, base.Add(61*time.Second))
	if !short.Suspended {
		t.Fatal("the shorter per-lease idle_suspend did not suspend at its threshold")
	}
	if long.Suspended {
		t.Fatal("the longer per-lease idle_suspend suspended early")
	}
	// A sweep 61 min in does not touch the long lease: it has its own
	// (longer) value, and no plain idle sweep exists.
	cur := base.Add(61 * time.Minute)
	svc.now = func() time.Time { return cur }
	svc.sweepExpired(ctx)
	if long.Suspended {
		t.Fatal("a sweep suspended a lease before its own idle_suspend")
	}
	// 2 h+: the long lease's own threshold fires.
	svc.suspendIdleLeases(ctx, base.Add(3*time.Hour))
	if !long.Suspended {
		t.Fatal("the longer per-lease idle_suspend did not suspend at its own threshold")
	}
}

// TestIdleSuspendOverridesHeldRule1 is removed with the held rules
// (FS5); TestIdleSuspendOverridesIdleTimeout covers the caller-chosen
// threshold.

// TestIdleSuspendDiskFloorSkips: a pause that would take the snapshot
// disk under the shared PREEMPT_DISK_FLOOR_PCT is skipped this sweep and
// suspended once room returns.
func TestIdleSuspendDiskFloorSkips(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	svc.cfg.TemplateStoragePath = t.TempDir()
	svc.cfg.PreemptDiskFloorPct = 15

	base := time.Now()
	svc.now = func() time.Time { return base }
	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.setIdlePolicy(l, 60); err != nil {
		t.Fatalf("setIdlePolicy: %v", err)
	}
	svc.store.mu.Lock()
	l.LastActive = base
	svc.store.mu.Unlock()

	const gib = uint64(1) << 30
	var total, free uint64 = 100 * gib, 16 * gib // 16 GiB free, a 2 GiB pause leaves 14% < 15%
	svc.diskCapacity = func(string) (uint64, uint64, error) { return total, free, nil }
	svc.suspendIdleLeases(ctx, base.Add(2*time.Minute))
	if l.Suspended {
		t.Fatal("idle suspend ignored the snapshot-disk floor")
	}
	// Room returns: the next sweep suspends.
	free = 40 * gib
	svc.suspendIdleLeases(ctx, base.Add(2*time.Minute))
	if !l.Suspended {
		t.Fatal("idle suspend did not retry once the disk had room")
	}
}

// TestPausedLeaseReleasedByOneClock: a lease paused through the pause
// path is released PAUSED_RELEASE_DAYS after its pause date, not before
// (FS5 one clock).
func TestPausedLeaseReleasedByOneClock(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	base := time.Now()
	svc.now = func() time.Time { return base }
	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "ci-job", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.pauseLease(ctx, l, false); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if l.PausedAt.IsZero() {
		t.Fatal("a pause did not stamp paused_at")
	}

	// One day short of the 30-day clock: still kept.
	cur := base.Add(29 * 24 * time.Hour)
	svc.releasePausedLeases(ctx, cur)
	if svc.lookup("c", l.ID) == nil {
		t.Fatal("a paused lease was released before PAUSED_RELEASE_DAYS")
	}
	// Past 30 days: released with reason paused_expired.
	cur = base.Add(30*24*time.Hour + time.Minute)
	svc.releasePausedLeases(ctx, cur)
	if svc.lookup("c", l.ID) != nil {
		t.Fatal("a paused lease was not released at PAUSED_RELEASE_DAYS")
	}
}

// TestPauseExpiringWarningOnce: the lease.paused_expiring warning is
// emitted once 24 h before release, and not repeated.
func TestPauseExpiringWarningOnce(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	base := time.Now()
	svc.now = func() time.Time { return base }
	l, err := svc.grant(ctx, "c", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.pauseLease(ctx, l, false); err != nil {
		t.Fatalf("pause: %v", err)
	}

	// Before the 24 h window: no warning. Collect each pass's events
	// without closing the subscription (Close drains and ends it).
	all := svc.Subscribe(EventFilter{LeaseID: l.ID})
	defer all.Close()
	svc.notifyPausedExpiring(ctx, base.Add(28*24*time.Hour))
	select {
	case ev := <-all.C:
		t.Fatalf("paused_expiring emitted before the 24 h window: %v", ev)
	case <-time.After(50 * time.Millisecond):
	}
	// Inside the window: one warning, then none on a second pass.
	svc.notifyPausedExpiring(ctx, base.Add(30*24*time.Hour-12*time.Hour))
	svc.notifyPausedExpiring(ctx, base.Add(30*24*time.Hour-6*time.Hour))
	var n int
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case ev := <-all.C:
			if ev.Type == LeasePausedExpiring {
				n++
			}
		case <-time.After(20 * time.Millisecond):
		}
	}
	if n != 1 {
		t.Fatalf("paused_expiring emitted %d times, want 1", n)
	}
}

// TestIdleSuspendExecAutoResumes: exec on a lease suspended by
// idle_suspend resumes it through the normal path and serves the call,
// keeping /dev/shm; the generation does not change. An exec on a lease
// suspended by hand resumes it too (#145 D2, one rule for every kind of
// suspend).
func TestIdleSuspendExecAutoResumes(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	ctx := context.Background()

	// Persistent lease with a /dev/shm marker in the fake's filesystem.
	l, err := svc.grant(ctx, "consumer-a", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.setIdlePolicy(l, 60); err != nil {
		t.Fatalf("setIdlePolicy: %v", err)
	}
	if err := sub.Fake.WriteFile(ctx, l.SandboxID, "/dev/shm/marker", []byte("kept"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	genBefore := l.Generation

	// Suspend through the pause path and record it as an idle_suspend.
	if _, err := svc.pauseLease(ctx, l, false); err != nil {
		t.Fatalf("pause: %v", err)
	}
	svc.store.mu.Lock()
	l.LastAction, l.LastActionAt = idleSuspendRule+"/"+heldActionSuspendIdle, time.Now()
	l.LastActive = time.Now().Add(-2 * time.Minute)
	svc.saveLeaseLocked(l)
	svc.store.mu.Unlock()
	if !l.Suspended {
		t.Fatal("precondition: lease not suspended")
	}

	// Exec resumes it and serves the call.
	resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+l.ID+"/exec", "token-a",
		map[string]any{"cmd": "echo hi"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exec on idle-suspended lease: %d: %v", resp.StatusCode, body)
	}
	if l.Suspended || !l.live() {
		t.Fatalf("lease not resumed by exec: state=%s suspended=%v", l.State, l.Suspended)
	}
	if l.Generation != genBefore {
		t.Fatalf("generation changed across idle suspend and auto-resume: %d -> %d", genBefore, l.Generation)
	}
	// The fake keeps a sandbox's files across a pause/resume when the
	// sandbox id is reused (as the resume path does), so the /dev/shm
	// marker must still be readable after the auto-resume.
	got, err := sub.Fake.ReadFile(ctx, l.SandboxID, "/dev/shm/marker", 64)
	if err != nil || string(got) != "kept" {
		t.Fatalf("/dev/shm marker after auto-resume = %q (%v), want kept", got, err)
	}

	// A lease suspended by hand resumes on its next exec too (#145 D2):
	// every suspend reason shares the one resume-on-use rule.
	manual, err := svc.grant(ctx, "consumer-a", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant manual: %v", err)
	}
	if _, err := svc.pauseLease(ctx, manual, false); err != nil {
		t.Fatalf("pause manual: %v", err)
	}
	resp, body = doReq(t, "POST", ts.URL+"/api/leases/"+manual.ID+"/exec", "token-a",
		map[string]any{"cmd": "echo hi"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exec on hand-suspended lease: %d, want 200 (resume on use): %v", resp.StatusCode, body)
	}
	if manual.Suspended || !manual.live() {
		t.Fatalf("hand-suspended lease not resumed by exec: state=%s suspended=%v", manual.State, manual.Suspended)
	}
}

// TestIdleSuspendExecResumeRefusalQuota: an auto-resume refused by the
// memory quota answers what resume would — 429 naming the limit — and
// leaves the lease suspended.
func TestIdleSuspendExecResumeRefusalQuota(t *testing.T) {
	srv, h, tok, uid := newMemQuotaServer(t, map[string]int{"big": 4096}, `{"max_mib":4096}`)
	svc := srv.svc
	owner := ownerIDFor(t, h, tok)
	ctx := context.Background()

	rec, first := createPersistentAs(t, h, tok, "big")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	id := first["id"].(string)
	l := svc.lookup(uid, id)
	if _, err := svc.setIdlePolicy(l, 60); err != nil {
		t.Fatalf("setIdlePolicy: %v", err)
	}
	if _, err := svc.suspend(ctx, owner, id); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	svc.store.mu.Lock()
	l.LastAction, l.LastActionAt = idleSuspendRule+"/"+heldActionSuspendIdle, time.Now()
	svc.store.mu.Unlock()
	// Spend the budget on a second lease so the resume cannot re-admit.
	rec, _ = createSandboxAs(t, h, tok, "big")
	if rec.Code != http.StatusCreated {
		t.Fatalf("second create: %d %s", rec.Code, rec.Body.String())
	}

	rec2 := postLeaseAction(t, h, tok, "/api/leases/"+id+"/exec", `{"cmd":"echo hi"}`)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("exec auto-resume quota refusal = %d, want 429: %s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "memory") {
		t.Fatalf("429 should name the memory limit, got %s", rec2.Body.String())
	}
	if !l.Suspended {
		t.Fatal("a refused auto-resume left the lease running")
	}
}

// TestIdleSuspendExecResumeRefusalBurst: an auto-resume refused by the
// burst reserve answers what resume would — 503 no burst capacity with
// Retry-After: 30 — and leaves the lease suspended.
func TestIdleSuspendExecResumeRefusalBurst(t *testing.T) {
	srv, h, sub, tok, uid := newClassServer(t, map[string]int{"mid": 1024}, `{"max_mib":8192}`)
	srv.svc.cfg.BurstReserveMiB = 8192
	svc := srv.svc
	owner := ownerIDFor(t, h, tok)
	ctx := context.Background()

	rec, first := createPersistentAs(t, h, tok, "mid")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	id := first["id"].(string)
	l := svc.lookup(uid, id)
	l.Burst = true // the flag the request would have carried
	if _, err := svc.setIdlePolicy(l, 60); err != nil {
		t.Fatalf("setIdlePolicy: %v", err)
	}
	if _, err := svc.suspend(ctx, owner, id); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	svc.store.mu.Lock()
	l.LastAction, l.LastActionAt = idleSuspendRule+"/"+heldActionSuspendIdle, time.Now()
	svc.store.mu.Unlock()
	// Shrink the node so the burst admission cannot fit.
	sub.SetNodeInfo(substrate.NodeInfo{
		Status:            "healthy",
		HugepagesTotal:    512 + 1,
		HugepageSizeBytes: 2 << 20,
	}, nil)
	svc.nodeInfoMu.Lock()
	svc.nodeInfoAt = time.Time{}
	svc.nodeInfoMu.Unlock()

	rec2 := postLeaseAction(t, h, tok, "/api/leases/"+id+"/exec", `{"cmd":"echo hi"}`)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("exec auto-resume burst refusal = %d, want 503: %s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "no burst capacity") {
		t.Fatalf("503 should name the burst capacity, got %s", rec2.Body.String())
	}
	if ra := rec2.Header().Get("Retry-After"); ra != "30" {
		t.Fatalf("Retry-After = %q, want 30", ra)
	}
	if !l.Suspended {
		t.Fatal("a refused auto-resume left the lease running")
	}
}

// TestIdleSuspendStaleMarkerDoesNotResume: a lease once idle-suspended,
// resumed by use and later suspended by hand is resumed by its next exec
// through the same one resume-on-use rule, whatever stale idle_suspend
// LastAction it kept (#145 D2). A drained lease carrying the marker is
// not specially handled either.
func TestIdleSuspendStaleMarkerDoesNotResume(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)
	ctx := context.Background()
	l, err := svc.grant(ctx, "consumer-a", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.setIdlePolicy(l, 60); err != nil {
		t.Fatalf("setIdlePolicy: %v", err)
	}
	svc.store.mu.Lock()
	l.LastActive = time.Now().Add(-2 * time.Minute)
	svc.store.mu.Unlock()
	svc.suspendIdleLeases(ctx, time.Now())
	if !l.Suspended {
		t.Fatal("setup: the lease was not idle-suspended")
	}
	// Use resumes it.
	if _, err := svc.resumeLease(ctx, l); err != nil {
		t.Fatalf("resume: %v", err)
	}
	// Suspended by hand: the stale marker must not count, and this exec
	// resumes it through the same one rule (#145 D2).
	if _, err := svc.suspend(ctx, "consumer-a", l.ID); err != nil {
		t.Fatalf("hand suspend: %v", err)
	}
	resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+l.ID+"/exec", "token-a",
		map[string]any{"cmd": "echo hi"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exec on a hand-suspended lease = %d, want 200 (resume on use): %v", resp.StatusCode, body)
	}
	if l.Suspended || !l.live() {
		t.Fatalf("hand-suspended lease not resumed: state=%s suspended=%v", l.State, l.Suspended)
	}
}
