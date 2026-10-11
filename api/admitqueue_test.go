package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v3/identity"
	"github.com/jrimmer/spoond/v3/metrics"
	"github.com/jrimmer/spoond/v3/substrate"
)

// Queued-admission tests (#129 part 1). The fake substrate's NodeInfo is
// controllable via installDynamicNode (api/preempt_test.go), so a "full"
// node is a small total with enough live sandboxes that the next create
// cannot fit.

// newAdmitServer builds a service over a fake with identity users and
// seeded images (mid=1024 MiB, small=256 MiB). It returns the server,
// its handler, the service, the fake and the identity store. User
// "one" carries token tok-1, user "two" tok-2.
func newAdmitServer(t *testing.T) (*Server, http.Handler, *Service, *testSub, *identity.Store) {
	t.Helper()
	svc, db, sub := newTestService(t)
	seedImage(t, db, "mid", 1024)
	seedImage(t, db, "small", 256)
	ids, _ := identity.NewStore("")
	svc.SetIdentities(ids)
	svc.SetMetrics(metrics.NewBackendMetrics())
	svc.cfg.MaxAdmitWaitSecs = 60
	srv := NewServer(svc, NewImageRegistry(db))
	h := srv.Handler()

	if rec, _ := doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"admin","fingerprints":["SHA256:fp-a"],"token":"admin-tok"}`); rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap admin: %d", rec.Code)
	}
	for _, u := range []struct{ name, fp, tok string }{
		{"one", "SHA256:fp-1", "tok-1"},
		{"two", "SHA256:fp-2", "tok-2"},
	} {
		rec, _ := doUsersReq(t, h, "POST", "/api/users", "admin-tok",
			fmt.Sprintf(`{"name":%q,"fingerprints":[%q],"token":%q}`, u.name, u.fp, u.tok))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create user %s: %d %s", u.name, rec.Code, rec.Body.String())
		}
	}
	return srv, h, svc, sub, ids
}

// uid returns a user's id by token.
func uid(t *testing.T, ids *identity.Store, tok string) string {
	t.Helper()
	u := ids.UserByToken(tok)
	if u == nil {
		t.Fatalf("no user for token %s", tok)
	}
	return u.ID
}

type createResult struct {
	code int
	body map[string]any
	hdr  http.Header
}

// startCreate runs a create request on its own goroutine so the test can
// keep the request open while it frees capacity. The caller reads exactly
// one result.
func startCreate(t *testing.T, h http.Handler, ctx context.Context, token, body string) <-chan createResult {
	t.Helper()
	ch := make(chan createResult, 1)
	go func() {
		req := httptest.NewRequestWithContext(ctx, "POST", "/api/sandboxes", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		ch <- createResult{code: rec.Code, body: m, hdr: rec.Header()}
	}()
	return ch
}

// waitCreate runs a create and waits for its result.
func waitCreate(t *testing.T, h http.Handler, token, body string) createResult {
	t.Helper()
	return waitResult(t, startCreate(t, h, context.Background(), token, body))
}

// waitResult reads one create result with a timeout.
func waitResult(t *testing.T, ch <-chan createResult) createResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(15 * time.Second):
		t.Fatal("create did not finish")
		return createResult{}
	}
}

// deleteLease deletes a lease through the API.
func deleteLease(t *testing.T, h http.Handler, token, id string) {
	t.Helper()
	req := httptest.NewRequest("DELETE", "/api/sandboxes/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete %s: %d %s", id, rec.Code, rec.Body.String())
	}
}

// waitDepth waits for the queue to reach n.
func waitDepth(t *testing.T, svc *Service, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for svc.queueDepth() < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := svc.queueDepth(); got != n {
		t.Fatalf("queue depth = %d, want %d", got, n)
	}
}

// fillTwo fills a 2 × 1024 MiB node with guaranteed mid leases owned by
// token and returns their ids. After this the node has no room.
func fillTwo(t *testing.T, h http.Handler, sub *testSub, svc *Service, token string) []string {
	t.Helper()
	installDynamicNode(t, svc, sub, 1024, 0, 512) // 1024 × 2 MiB = 2048 MiB
	var ids []string
	for i := 0; i < 2; i++ {
		r := waitCreate(t, h, token, `{"image":"mid","ttl":60}`)
		if r.code != http.StatusCreated {
			t.Fatalf("filler %d: %d %v", i, r.code, r.body)
		}
		ids = append(ids, r.body["id"].(string))
	}
	// The node is now full: the next create is refused at once.
	if r := waitCreate(t, h, token, `{"image":"mid","ttl":60}`); r.code != http.StatusServiceUnavailable {
		t.Fatalf("third create on a full node = %d, want 503 (%v)", r.code, r.body)
	}
	return ids
}

// TestAdmitWaitZeroUnchanged: wait 0 (the default) answers a full node
// with the immediate 503, exactly as before, and queues nothing.
func TestAdmitWaitZeroUnchanged(t *testing.T) {
	_, h, svc, sub, _ := newAdmitServer(t)
	fillTwo(t, h, sub, svc, "tok-1")

	start := time.Now()
	r := waitCreate(t, h, "tok-1", `{"image":"mid","ttl":60}`)
	if r.code != http.StatusServiceUnavailable {
		t.Fatalf("wait 0 full-node create = %d, want 503 (%v)", r.code, r.body)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("wait 0 refused after %s; it must answer at once", time.Since(start))
	}
	if _, ok := r.body["waited_ms"]; ok {
		t.Fatalf("wait 0 must not carry waited_ms: %v", r.body)
	}
	if n := svc.queueDepth(); n != 0 {
		t.Fatalf("queue depth = %d, want 0", n)
	}
}

// TestAdmitWaitNegativeRejected: a negative wait is a bad request.
func TestAdmitWaitNegativeRejected(t *testing.T) {
	_, h, _, _, _ := newAdmitServer(t)
	r := waitCreate(t, h, "tok-1", `{"image":"mid","ttl":60,"wait":-1}`)
	if r.code != http.StatusBadRequest {
		t.Fatalf("negative wait = %d, want 400 (%v)", r.code, r.body)
	}
}

// TestAdmitWaitDisabled: MAX_ADMIT_WAIT_SECS=0 accepts the field and
// ignores it: the refusal answers at once and nothing queues.
func TestAdmitWaitDisabled(t *testing.T) {
	_, h, svc, sub, _ := newAdmitServer(t)
	svc.cfg.MaxAdmitWaitSecs = 0
	fillTwo(t, h, sub, svc, "tok-1")

	r := waitCreate(t, h, "tok-1", `{"image":"mid","ttl":60,"wait":30}`)
	if r.code != http.StatusServiceUnavailable {
		t.Fatalf("disabled wait create = %d, want 503 (%v)", r.code, r.body)
	}
	if svc.queueDepth() != 0 {
		t.Fatalf("queue depth = %d, want 0", svc.queueDepth())
	}
}

// TestAdmitWaitAdmittedWhenCapacityFrees: a waiting create is admitted
// when a filler is released, before its timeout, and the response carries
// waited_ms.
func TestAdmitWaitAdmittedWhenCapacityFrees(t *testing.T) {
	_, h, svc, sub, _ := newAdmitServer(t)
	fillers := fillTwo(t, h, sub, svc, "tok-1")
	svc.admitQ.tick = 20 * time.Millisecond

	res := startCreate(t, h, context.Background(), "tok-1", `{"image":"mid","ttl":60,"wait":60}`)
	waitDepth(t, svc, 1)
	deleteLease(t, h, "tok-1", fillers[0])

	r := waitResult(t, res)
	if r.code != http.StatusCreated {
		t.Fatalf("waiting create = %d, want 201 (%v)", r.code, r.body)
	}
	waited, ok := r.body["waited_ms"].(float64)
	if !ok || waited <= 0 {
		t.Fatalf("201 body must carry waited_ms > 0, got %v", r.body["waited_ms"])
	}
}

// TestAdmitWaitTimesOut: a waiting create that never fits is answered
// with the original refusal (same status, body), plus waited_ms.
func TestAdmitWaitTimesOut(t *testing.T) {
	_, h, svc, sub, _ := newAdmitServer(t)
	fillTwo(t, h, sub, svc, "tok-1")
	svc.admitQ.tick = 20 * time.Millisecond

	r := waitCreate(t, h, "tok-1", `{"image":"mid","ttl":60,"wait":1}`)
	if r.code != http.StatusServiceUnavailable {
		t.Fatalf("timed-out create = %d, want 503 (%v)", r.code, r.body)
	}
	if msg, _ := r.body["error"].(string); !strings.Contains(msg, "capacity") {
		t.Fatalf("timed-out body should carry the original capacity refusal, got %v", r.body)
	}
	if waited, ok := r.body["waited_ms"].(float64); !ok || waited < 900 {
		t.Fatalf("timed-out body must carry waited_ms >= 900, got %v", r.body["waited_ms"])
	}
	if svc.queueDepth() != 0 {
		t.Fatalf("queue depth after timeout = %d, want 0", svc.queueDepth())
	}
}

// TestAdmitWaitCapacityTimesOut: a create waiting out a capacity
// refusal that take-back cannot serve (every candidate pinned: the
// dynamic node makes the pinned lease hold the box) times out with the
// original 503 and its Retry-After.
func TestAdmitWaitCapacityTimesOut(t *testing.T) {
	_, h, svc, sub, _ := newAdmitServer(t)
	// 512 pages of 2 MiB = 1024 MiB, all held by something outside
	// spoond: no lease can be taken back and none is pinned, so the
	// shortfall is structural (not box_full) and the create waits out
	// its budget.
	installDynamicNode(t, svc, sub, 512, 512, 512)
	svc.admitQ.tick = 20 * time.Millisecond

	r := waitCreate(t, h, "tok-1", `{"image":"mid","ttl":60,"wait":1}`)
	if r.code != http.StatusServiceUnavailable {
		t.Fatalf("pinned-full timeout = %d, want 503 (%v)", r.code, r.body)
	}
	if msg, _ := r.body["error"].(string); !strings.Contains(msg, "capacity") {
		t.Fatalf("timed-out body should carry the original capacity refusal, got %v", r.body)
	}
	if ra := r.hdr.Get("Retry-After"); ra != "30" {
		t.Fatalf("Retry-After = %q, want 30", ra)
	}
	if waited, _ := r.body["waited_ms"].(float64); waited < 900 {
		t.Fatalf("waited_ms = %v, want >= 900", r.body["waited_ms"])
	}
}

// TestAdmitWaitCapacityTimeoutAnswersCapacityWait: a queued create whose
// wait times out for a structural capacity cause carries the same
// capacity_wait code the immediate refusal does, not just the status and
// the Retry-After (gkx2).
func TestAdmitWaitCapacityTimeoutAnswersCapacityWait(t *testing.T) {
	_, h, svc, sub, _ := newAdmitServer(t)
	// The whole box is held outside spoond: no take-back candidate
	// exists, so the create waits out its budget and times out on the
	// capacity refusal that queued it.
	installDynamicNode(t, svc, sub, 512, 512, 512)
	svc.admitQ.tick = 20 * time.Millisecond

	r := waitCreate(t, h, "tok-1", `{"image":"mid","ttl":60,"wait":1}`)
	if r.code != http.StatusServiceUnavailable {
		t.Fatalf("timed-out capacity create = %d, want 503 (%v)", r.code, r.body)
	}
	if ra := r.hdr.Get("Retry-After"); ra != "30" {
		t.Fatalf("Retry-After = %q, want 30", ra)
	}
	if code, _ := r.body["code"].(string); code != "capacity_wait" {
		t.Fatalf("code = %v, want capacity_wait: %v", r.body["code"], r.body)
	}
}

// TestAdmitWakeWhilePassRunning: a release that frees room while an
// admission pass is already running must still be retried, even when no
// periodic tick can help (spoond-vbdj). The old wake-up dropped the
// release's wake because a pass held wakeScheduled, so the waiting
// create sat until the next tick.
func TestAdmitWakeWhilePassRunning(t *testing.T) {
	_, h, svc, sub, _ := newAdmitServer(t)
	fillers := fillTwo(t, h, sub, svc, "tok-1")
	svc.admitQ.tick = time.Hour // no periodic rescue: only the wake can help

	// Install the hold gate before the create starts. The waiting create's
	// own initial passes — the wakeAdmissionQueue goroutine and
	// waitForAdmission's direct retry — both read the hook, so installing
	// it later would race that read (a -race report) and could hold the
	// handler's direct pass instead of the pass this test schedules,
	// letting the test pass even on the old buggy code. hold stays false
	// until the test arms it, so the initial passes run straight through.
	held := make(chan struct{})
	releasePass := make(chan struct{})
	var hold atomic.Bool
	var once atomic.Bool
	var initialPasses atomic.Int64
	svc.admitPassHook = func() {
		initialPasses.Add(1)
		if !hold.Load() || !once.CompareAndSwap(false, true) {
			return
		}
		close(held)
		<-releasePass
	}

	// Queue the waiting create and let both of its initial passes finish,
	// so the pass held below is the only one left to schedule. The hook
	// fires once per completed pass, so reaching two proves the handler's
	// direct pass has judged the queue and will not consume the hold.
	res := startCreate(t, h, context.Background(), "tok-1", `{"image":"mid","ttl":60,"wait":60}`)
	waitDepth(t, svc, 1)
	deadline := time.Now().Add(3 * time.Second)
	for (svc.wakeScheduled.Load() || initialPasses.Load() < 2) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if svc.wakeScheduled.Load() || initialPasses.Load() < 2 {
		t.Fatalf("initial admission passes did not finish: scheduled=%v passes=%d",
			svc.wakeScheduled.Load(), initialPasses.Load())
	}
	// Drain any pass between the hook and its admitMu release.
	svc.admitQ.admitMu.Lock()
	svc.admitQ.admitMu.Unlock()
	if once.Load() {
		t.Fatal("an initial pass held the hook before it was armed")
	}

	// Hold the next pass right after it judges the queue full.
	hold.Store(true)
	svc.wakeAdmissionQueue()
	select {
	case <-held:
	case <-time.After(3 * time.Second):
		t.Fatal("admission pass was not held")
	}

	// Free room while the pass is held, then let it finish: the release's
	// wake must be absorbed and retried, not lost to the tick.
	deleteLease(t, h, "tok-1", fillers[0])
	close(releasePass)

	r := waitResult(t, res)
	if r.code != http.StatusCreated {
		t.Fatalf("waiting create after a mid-pass wake = %d, want 201 (%v)", r.code, r.body)
	}
}

// TestAdmitWaitFairShare: of two waiting creates, the owner with the
// lower fair-share ratio is admitted first even though it queued later
// (3.0 FS4). The node is full of memory held outside spoond, so neither
// owner borrows and both wait; tok-1 runs one small lease, tok-2 none.
func TestAdmitWaitFairShare(t *testing.T) {
	_, h, svc, sub, _ := newAdmitServer(t)
	roomyDisk(svc)
	// 4096 pages (8192 MiB), 128 pages (256 MiB) per sandbox.
	installDynamicNode(t, svc, sub, 4096, 0, 128)
	if r := waitCreate(t, h, "tok-1", `{"image":"small","ttl":60}`); r.code != http.StatusCreated {
		t.Fatalf("tok-1's small lease: %d %v", r.code, r.body)
	}
	// Everything else is held outside spoond: the node is full.
	installDynamicNode(t, svc, sub, 4096, 4096-128, 128)
	svc.updateNodeMetrics(context.Background()) // the fair-share view reads the node-info cache
	// No periodic pass: the explicit wake below runs it, so the test
	// checks the ordering itself.
	svc.admitQ.tick = time.Hour

	// tok-1 (ratio > 0) queues first…
	resHigher := startCreate(t, h, context.Background(), "tok-1", `{"image":"small","ttl":60,"wait":60}`)
	waitDepth(t, svc, 1)
	// …then tok-2 (ratio 0) queues, and should win the freed room.
	resLow := startCreate(t, h, context.Background(), "tok-2", `{"image":"small","ttl":60,"wait":60}`)
	waitDepth(t, svc, 2)

	// Let the pass that queuing started finish, then free room for
	// exactly one small lease.
	svc.admitQ.admitMu.Lock()
	svc.admitQ.admitMu.Unlock()
	installDynamicNode(t, svc, sub, 4096, 4096-256, 128)
	svc.wakeAdmissionQueue()

	r := waitResult(t, resLow)
	if r.code != http.StatusCreated {
		t.Fatalf("low-ratio waiting create = %d, want 201 (%v)", r.code, r.body)
	}
	// The higher-ratio create must still be waiting.
	select {
	case r2 := <-resHigher:
		t.Fatalf("higher-ratio create should have waited, got %d (%v)", r2.code, r2.body)
	case <-time.After(300 * time.Millisecond):
	}
	svc.drainQueue()
}

// TestAdmitOrderByFairShareRatio: orderedTickets puts the lowest
// fair-share ratio first, owners the fair-share view does not know
// after every known owner, and FIFO within equal ratios.
func TestAdmitOrderByFairShareRatio(t *testing.T) {
	_, _, svc, sub, ids := newAdmitServer(t)
	installDynamicNode(t, svc, sub, 1<<20, 0, 0)
	roomyDisk(svc)
	one, two := uid(t, ids, "tok-1"), uid(t, ids, "tok-2")
	// tok-1 runs 4096 MiB, tok-2 runs 1024 MiB: tok-1's ratio is higher.
	addRunningLease(svc, "l-one", one, 4096)
	addRunningLease(svc, "l-two", two, 1024)
	svc.updateNodeMetrics(context.Background()) // the view reads the node-info cache
	svc.invalidateFairShares()
	if share, ok := svc.fairShareFor(context.Background(), one); !ok || !share.CapacityKnown {
		t.Fatalf("fair share of tok-1 = %+v (ok %v), want a known capacity", share, ok)
	}

	req := func(owner string) leaseRequest { return leaseRequest{owner: owner, image: "mid", ttl: time.Minute} }
	tGhost := svc.newAdmissionTicket("ghost", req("ghost"), substrate.ErrCapacity, time.Minute)
	tOne := svc.newAdmissionTicket(one, req(one), substrate.ErrCapacity, time.Minute)
	tTwoA := svc.newAdmissionTicket(two, req(two), substrate.ErrCapacity, time.Minute)
	tTwoB := svc.newAdmissionTicket(two, req(two), substrate.ErrCapacity, time.Minute)
	t.Cleanup(svc.drainQueue)

	got := svc.orderedTickets()
	want := []*admissionTicket{tTwoA, tTwoB, tOne, tGhost}
	if len(got) != len(want) {
		t.Fatalf("ordered %d tickets, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("position %d = owner %s seq %d, want owner %s seq %d", i, got[i].owner, got[i].seq, want[i].owner, want[i].seq)
		}
	}
}

// TestAdmitWaitBackfill: a small create passes a big one that does not
// fit when only enough memory for the small one frees.
func TestAdmitWaitBackfill(t *testing.T) {
	_, h, svc, sub, _ := newAdmitServer(t)
	// Two mid fillers (1024 pages) plus 384 pages held outside leases
	// fill a 1408-page node exactly.
	installDynamicNode(t, svc, sub, 1408, 384, 512)
	for i := 0; i < 2; i++ {
		if r := waitCreate(t, h, "tok-1", `{"image":"mid","ttl":60}`); r.code != http.StatusCreated {
			t.Fatalf("filler %d: %d %v", i, r.code, r.body)
		}
	}
	svc.admitQ.tick = 20 * time.Millisecond

	// Queue the big (1024 MiB) first, then the small (256 MiB).
	resBig := startCreate(t, h, context.Background(), "tok-1", `{"image":"mid","ttl":60,"wait":60}`)
	waitDepth(t, svc, 1)
	resSmall := startCreate(t, h, context.Background(), "tok-1", `{"image":"small","ttl":60,"wait":60}`)
	waitDepth(t, svc, 2)

	// Free only 256 MiB (128 pages) of non-lease use: free = 128 pages,
	// enough for the small but not the big.
	installDynamicNode(t, svc, sub, 1408, 256, 512)
	svc.wakeAdmissionQueue()

	// The fake counts every live sandbox as 512 pages, the small one
	// too, so once it is live the node reports more used than total.
	// Free must read 0 then (NodeInfo.FreeHugepageBytes saturates), not
	// wrap to an enormous figure that would admit the big create.
	rSmall := waitResult(t, resSmall)
	if rSmall.code != http.StatusCreated {
		t.Fatalf("small backfill create = %d, want 201 (%v)", rSmall.code, rSmall.body)
	}
	select {
	case rBig := <-resBig:
		t.Fatalf("big create should not have fit yet, got %d", rBig.code)
	case <-time.After(300 * time.Millisecond):
	}
	svc.drainQueue()
}

// TestAdmitWaitClientCancelDropsTicket: when the client goes away the
// ticket is dropped and no lease is left behind.
func TestAdmitWaitClientCancelDropsTicket(t *testing.T) {
	_, h, svc, sub, _ := newAdmitServer(t)
	fillTwo(t, h, sub, svc, "tok-1")
	svc.admitQ.tick = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	_ = startCreate(t, h, ctx, "tok-1", `{"image":"mid","ttl":60,"wait":60}`)
	waitDepth(t, svc, 1)
	cancel()
	deadline := time.Now().Add(3 * time.Second)
	for svc.queueDepth() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if svc.queueDepth() != 0 {
		t.Fatalf("cancelled ticket left queue depth %d", svc.queueDepth())
	}
	if live := len(sub.sandboxesLive(t)); live != 2 {
		t.Fatalf("live sandboxes = %d, want the 2 fillers", live)
	}
}

// TestAdmitWaitDrainAnswersQueued: a drain answers every waiting create
// with 503 draining.
func TestAdmitWaitDrainAnswersQueued(t *testing.T) {
	srv, h, svc, sub, _ := newAdmitServer(t)
	srv.SetAdminToken("admin-tok")
	fillTwo(t, h, sub, svc, "tok-1")
	svc.admitQ.tick = 20 * time.Millisecond

	res := startCreate(t, h, context.Background(), "tok-1", `{"image":"mid","ttl":60,"wait":60}`)
	waitDepth(t, svc, 1)
	// Wait for the drain before the test returns: it still touches the
	// build storage during its quiesce/pause work, and a goroutine that
	// outlives the test races t.TempDir's cleanup ("directory not
	// empty").
	drainDone := make(chan struct{})
	go func() { defer close(drainDone); _, _ = svc.drain(context.Background()) }()
	r := waitResult(t, res)
	select {
	case <-drainDone:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not return after answering the queued create")
	}
	if r.code != http.StatusServiceUnavailable {
		t.Fatalf("drained queued create = %d, want 503 (%v)", r.code, r.body)
	}
	if msg, _ := r.body["error"].(string); !strings.Contains(msg, "draining") {
		t.Fatalf("drained body = %v, want draining", r.body)
	}
	if ra := r.hdr.Get("Retry-After"); ra != strconv.Itoa(drainRetryAfterSecs) {
		t.Fatalf("drained Retry-After = %q, want %d", ra, drainRetryAfterSecs)
	}
}

// TestAdmitWaitLeaseIDMatchesQueuedEvent: the id announced in the
// `queued` event is the created lease's id.
func TestAdmitWaitLeaseIDMatchesQueuedEvent(t *testing.T) {
	_, h, svc, sub, _ := newAdmitServer(t)
	fillers := fillTwo(t, h, sub, svc, "tok-1")
	svc.admitQ.tick = 20 * time.Millisecond

	events := svc.Subscribe(EventFilter{})
	defer events.Close()
	res := startCreate(t, h, context.Background(), "tok-1", `{"image":"mid","ttl":60,"wait":60}`)
	waitDepth(t, svc, 1)
	var queuedID string
	deadline := time.Now().Add(3 * time.Second)
	for queuedID == "" && time.Now().Before(deadline) {
		select {
		case ev := <-events.C:
			if ev.Type == LeaseQueued {
				queuedID = ev.LeaseID
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	if queuedID == "" {
		t.Fatal("no queued event seen")
	}
	deleteLease(t, h, "tok-1", fillers[0])
	r := waitResult(t, res)
	if r.code != http.StatusCreated {
		t.Fatalf("create = %d, want 201", r.code)
	}
	if got := r.body["id"].(string); got != queuedID {
		t.Fatalf("created id %s != queued event id %s", got, queuedID)
	}
}

// TestAdmitWaitGauge: the queued gauge tracks the queue depth and drops
// back to zero when the queue is answered.
func TestAdmitWaitGauge(t *testing.T) {
	_, h, svc, sub, _ := newAdmitServer(t)
	fillTwo(t, h, sub, svc, "tok-1")
	svc.admitQ.tick = 20 * time.Millisecond

	res := startCreate(t, h, context.Background(), "tok-1", `{"image":"mid","ttl":60,"wait":60}`)
	waitDepth(t, svc, 1)
	if got := gaugeValue(t, svc, "spoond_leases_queued"); got != 1 {
		t.Fatalf("leases_queued = %v, want 1", got)
	}
	svc.drainQueue()
	if r := waitResult(t, res); r.code != http.StatusServiceUnavailable {
		t.Fatalf("queued create after drain = %d, want 503", r.code)
	}
	if got := gaugeValue(t, svc, "spoond_leases_queued"); got != 0 {
		t.Fatalf("leases_queued after drain = %v, want 0", got)
	}
}

// gaugeValue reads one gauge sample from the service's metrics registry.
func gaugeValue(t *testing.T, svc *Service, name string) float64 {
	t.Helper()
	mfs, err := svc.metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		return mf.GetMetric()[0].GetGauge().GetValue()
	}
	t.Fatalf("metric %s not found", name)
	return -1
}

// TestAdmitWaitQueuePosition: the queued event carries the create's
// place in the fair-share order, and GET /api/leases/queue lists the
// caller's waiting creates (an admin's view: everyone's) with their
// positions in the whole queue.
func TestAdmitWaitQueuePosition(t *testing.T) {
	_, h, svc, sub, _ := newAdmitServer(t)
	fillTwo(t, h, sub, svc, "tok-1")
	svc.admitQ.tick = time.Hour // no retries during the test

	events := svc.Subscribe(EventFilter{})
	defer events.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startCreate(t, h, ctx, "tok-1", `{"image":"mid","ttl":60,"wait":60}`)
	waitDepth(t, svc, 1)
	startCreate(t, h, ctx, "tok-2", `{"image":"small","ttl":60,"wait":60}`)
	waitDepth(t, svc, 2)

	var details []string
	deadline := time.Now().Add(3 * time.Second)
	for len(details) < 2 && time.Now().Before(deadline) {
		select {
		case ev := <-events.C:
			if ev.Type == LeaseQueued {
				details = append(details, ev.Detail)
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	if len(details) != 2 || !strings.HasSuffix(details[0], "; position 1 of 1") || !strings.HasSuffix(details[1], "; position 2 of 2") {
		t.Fatalf("queued details = %q, want positions 1 of 1 and 2 of 2", details)
	}

	queue := func(tok string) []any {
		rec, body := doUsersReq(t, h, "GET", "/api/leases/queue", tok, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET queue as %s: %d %s", tok, rec.Code, rec.Body.String())
		}
		q, _ := body["queued"].([]any)
		return q
	}
	one, two, all := queue("tok-1"), queue("tok-2"), queue("admin-tok")
	if len(one) != 1 || len(two) != 1 || len(all) != 2 {
		t.Fatalf("queue sizes tok-1=%d tok-2=%d admin=%d, want 1, 1, 2", len(one), len(two), len(all))
	}
	if p := two[0].(map[string]any)["position"]; p != float64(2) {
		t.Fatalf("tok-2's create position = %v, want 2 (its place in the whole queue)", p)
	}
	if img := one[0].(map[string]any)["image"]; img != "mid" {
		t.Fatalf("tok-1's queued image = %v, want mid", img)
	}
	svc.drainQueue()
}

// TestAdmitCapacityRefusalCarriesRetryAfter: a create refused because
// the node is full answers 503 with a Retry-After, like the burst and
// preempt refusals — so a client that did not send "wait" still knows
// when to come back.
func TestAdmitCapacityRefusalCarriesRetryAfter(t *testing.T) {
	_, h, svc, sub, _ := newAdmitServer(t)
	fillTwo(t, h, sub, svc, "tok-1")

	r := waitCreate(t, h, "tok-1", `{"image":"mid","ttl":60}`)
	if r.code != http.StatusServiceUnavailable {
		t.Fatalf("full-node create = %d, want 503 (%v)", r.code, r.body)
	}
	if ra := r.hdr.Get("Retry-After"); ra != strconv.Itoa(capacityRetryAfterSecs) {
		t.Fatalf("Retry-After = %q, want %d", ra, capacityRetryAfterSecs)
	}
}

// goneContext is a request context that reports a client-gone error
// without its Done channel ever closing, so a test can pin the exact
// moment admission and the client's disappearance interleave (L2): the
// queue admits the ticket while Err already says the client is gone.
type goneContext struct {
	context.Context
	gone atomic.Bool
}

func (c *goneContext) Err() error {
	if c.gone.Load() {
		return context.Canceled
	}
	return nil
}

// TestAdmitGrantedLeaseReleasedWhenClientGone (L2): the queue can admit
// a ticket in the moment the client's context ends — finishTicket sees
// the ticket already done, the lease is granted, and the handler would
// otherwise write a 201 to a dead connection and leak a lease with no
// runner owner and no job label until its TTL. The granted lease must be
// released with reason client_gone and nothing written.
func TestAdmitGrantedLeaseReleasedWhenClientGone(t *testing.T) {
	_, h, svc, sub, _ := newAdmitServer(t)
	fillers := fillTwo(t, h, sub, svc, "tok-1")
	svc.admitQ.tick = time.Hour // only the release's wake may decide

	events := svc.Subscribe(EventFilter{})
	defer events.Close()

	ctx := &goneContext{Context: context.Background()}
	res := startCreate(t, h, ctx, "tok-1", `{"image":"mid","ttl":60,"wait":60}`)
	waitDepth(t, svc, 1)

	// The client's context ends while the ticket waits, then capacity
	// frees: admission and the disappearance meet.
	ctx.gone.Store(true)
	deleteLease(t, h, "tok-1", fillers[0])

	r := waitResult(t, res)
	if r.code == http.StatusCreated {
		t.Fatalf("granted lease written to a gone client: %d %v", r.code, r.body)
	}
	// The granted lease is released with reason client_gone, not leaked.
	deadline := time.Now().Add(3 * time.Second)
	seen := ""
	for seen != "client_gone" && time.Now().Before(deadline) {
		select {
		case ev := <-events.C:
			if ev.Type == LeaseReleased && ev.Detail == "client_gone" {
				seen = ev.Detail
			}
		case <-time.After(10 * time.Millisecond):
		}
	}
	if seen != "client_gone" {
		t.Fatalf("no released event with detail client_gone; got %q", seen)
	}
	if live := len(sub.sandboxesLive(t)); live != 1 {
		t.Fatalf("live sandboxes = %d, want the 1 remaining filler (granted lease not released)", live)
	}
}
