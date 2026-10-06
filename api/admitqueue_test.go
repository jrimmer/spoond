package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/identity"
	"github.com/jrimmer/spoond/v2/metrics"
)

// Queued-admission tests (#129 part 1). The fake substrate's NodeInfo is
// controllable via installDynamicNode (api/preempt_test.go), so a "full"
// node is a small total with enough live sandboxes that the next create
// cannot fit. The fillers and waiters are all guaranteed, so no
// preemption can mask a capacity refusal.

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
	svc.cfg.BurstReserveMiB = 0
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

// setQuota sets a user's quota directly.
func setQuota(t *testing.T, ids *identity.Store, userID string, maxLeases, guaranteed, maxMiB int) {
	t.Helper()
	if err := ids.SetQuota(userID, maxLeases, 0, guaranteed, maxMiB, 0); err != nil {
		t.Fatalf("set quota: %v", err)
	}
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

// TestAdmitWaitBurstReserveTimesOut: a burst create waiting out
// errBurstReserve times out with the 503 "no burst capacity" and its
// Retry-After.
func TestAdmitWaitBurstReserveTimesOut(t *testing.T) {
	_, h, svc, sub, _ := newAdmitServer(t)
	svc.cfg.BurstReserveMiB = 4096 // larger than the whole node
	installDynamicNode(t, svc, sub, 1024, 0, 512)
	svc.admitQ.tick = 20 * time.Millisecond

	r := waitCreate(t, h, "tok-1", `{"image":"mid","ttl":60,"burst":true,"wait":1}`)
	if r.code != http.StatusServiceUnavailable {
		t.Fatalf("burst-reserve timeout = %d, want 503 (%v)", r.code, r.body)
	}
	if msg, _ := r.body["error"].(string); !strings.Contains(msg, "no burst capacity") {
		t.Fatalf("want no burst capacity, got %v", r.body)
	}
	if ra := r.hdr.Get("Retry-After"); ra != "30" {
		t.Fatalf("Retry-After = %q, want 30", ra)
	}
	if waited, _ := r.body["waited_ms"].(float64); waited < 900 {
		t.Fatalf("waited_ms = %v, want >= 900", r.body["waited_ms"])
	}
}

// TestAdmitWaitFairShare: of two waiting creates, the owner with
// guaranteed_mib headroom is admitted first even though it queued later.
func TestAdmitWaitFairShare(t *testing.T) {
	_, h, svc, sub, ids := newAdmitServer(t)
	setQuota(t, ids, uid(t, ids, "tok-1"), 0, 4096, 8192)
	fillers := fillTwo(t, h, sub, svc, "tok-1")
	// No periodic pass: the release's own wake-up (its last step, after
	// the room is credited) runs the pass. A tick landing mid-release
	// could try the guaranteed create before the room appears and the
	// other just after; fair share is best-effort across that window
	// (docs/api.md), and this test checks the ordering itself.
	svc.admitQ.tick = time.Hour

	// The no-guarantee owner queues first…
	resNoGuarantee := startCreate(t, h, context.Background(), "tok-2", `{"image":"mid","ttl":60,"wait":60}`)
	waitDepth(t, svc, 1)
	// …then the guaranteed owner queues, and should win the freed slot.
	resGuaranteed := startCreate(t, h, context.Background(), "tok-1", `{"image":"mid","ttl":60,"wait":60}`)
	waitDepth(t, svc, 2)

	// Let the pass that queuing started finish before freeing room, so
	// the release's own wake-up decides who gets it.
	svc.admitQ.admitMu.Lock()
	svc.admitQ.admitMu.Unlock()
	deleteLease(t, h, "tok-1", fillers[0])

	r := waitResult(t, resGuaranteed)
	if r.code != http.StatusCreated {
		t.Fatalf("guaranteed waiting create = %d, want 201 (%v)", r.code, r.body)
	}
	// The no-guarantee create must still be waiting.
	select {
	case r2 := <-resNoGuarantee:
		t.Fatalf("no-guarantee create should have waited, got %d", r2.code)
	case <-time.After(300 * time.Millisecond):
	}
	svc.drainQueue()
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

// TestAdmitWaitMaxLeasesWaitable: since 2.5.1 the lease-count cap is
// waitable: a create at the cap queues ("lease cap") and is admitted
// once one of the owner's own leases is released.
func TestAdmitWaitMaxLeasesWaitable(t *testing.T) {
	_, h, svc, _, ids := newAdmitServer(t)
	setQuota(t, ids, uid(t, ids, "tok-1"), 1, 0, 0) // one lease only
	svc.admitQ.tick = time.Hour                     // the release wakes the queue
	first := waitCreate(t, h, "tok-1", `{"image":"small","ttl":60}`)
	if first.code != http.StatusCreated {
		t.Fatalf("first create: %d %v", first.code, first.body)
	}
	events := svc.Subscribe(EventFilter{})
	defer events.Close()
	res := startCreate(t, h, context.Background(), "tok-1", `{"image":"small","ttl":60,"wait":30}`)
	waitDepth(t, svc, 1)
	var detail string
	deadline := time.Now().Add(3 * time.Second)
	for detail == "" && time.Now().Before(deadline) {
		select {
		case ev := <-events.C:
			if ev.Type == LeaseQueued {
				detail = ev.Detail
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !strings.HasPrefix(detail, "lease cap; position 1 of 1") {
		t.Fatalf("queued detail = %q, want lease cap; position 1 of 1", detail)
	}
	svc.admitQ.admitMu.Lock()
	svc.admitQ.admitMu.Unlock()
	deleteLease(t, h, "tok-1", first.body["id"].(string))
	if r := waitResult(t, res); r.code != http.StatusCreated {
		t.Fatalf("lease-cap waiting create = %d, want 201 (%v)", r.code, r.body)
	}
}

// TestAdmitWaitMaxLeasesTimesOut: at the lease-count cap with nothing
// released, the wait ends in the cap's own 429.
func TestAdmitWaitMaxLeasesTimesOut(t *testing.T) {
	_, h, svc, _, ids := newAdmitServer(t)
	setQuota(t, ids, uid(t, ids, "tok-1"), 1, 0, 0)
	svc.admitQ.tick = 10 * time.Millisecond
	if r := waitCreate(t, h, "tok-1", `{"image":"small","ttl":60}`); r.code != http.StatusCreated {
		t.Fatalf("first create: %d %v", r.code, r.body)
	}
	r := waitCreate(t, h, "tok-1", `{"image":"small","ttl":60,"wait":1}`)
	if r.code != http.StatusTooManyRequests {
		t.Fatalf("lease-cap wait timeout = %d, want 429 (%v)", r.code, r.body)
	}
	if _, ok := r.body["waited_ms"]; !ok {
		t.Fatalf("timed-out create should carry waited_ms: %v", r.body)
	}
}

// TestAdmitWaitMemoryCapWaitable: the memory cap refusal is waitable:
// once another lease frees the budget, the create is admitted.
func TestAdmitWaitMemoryCapWaitable(t *testing.T) {
	_, h, svc, sub, ids := newAdmitServer(t)
	setQuota(t, ids, uid(t, ids, "tok-1"), 0, 0, 1024) // exactly one 1024 MiB lease
	svc.admitQ.tick = 10 * time.Millisecond
	installDynamicNode(t, svc, sub, 1<<20, 0, 0)
	filler := waitCreate(t, h, "tok-1", `{"image":"mid","ttl":60}`)
	if filler.code != http.StatusCreated {
		t.Fatalf("filler: %d %v", filler.code, filler.body)
	}
	res := startCreate(t, h, context.Background(), "tok-1", `{"image":"mid","ttl":60,"wait":60}`)
	waitDepth(t, svc, 1)
	deleteLease(t, h, "tok-1", filler.body["id"].(string))
	r := waitResult(t, res)
	if r.code != http.StatusCreated {
		t.Fatalf("memory-cap waiting create = %d, want 201 (%v)", r.code, r.body)
	}
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
	go func() { _, _ = svc.drain(context.Background()) }()
	r := waitResult(t, res)
	if r.code != http.StatusServiceUnavailable {
		t.Fatalf("drained queued create = %d, want 503 (%v)", r.code, r.body)
	}
	if msg, _ := r.body["error"].(string); !strings.Contains(msg, "draining") {
		t.Fatalf("drained body = %v, want draining", r.body)
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
