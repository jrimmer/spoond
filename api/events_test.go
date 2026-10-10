package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/identity"
	"github.com/jrimmer/spoond/v2/substrate"
)

// collectEvents drains a subscription channel into a slice until the
// channel is closed (Close was called).
func collectEvents(events <-chan LeaseEvent) []LeaseEvent {
	var out []LeaseEvent
	for ev := range events {
		out = append(out, ev)
	}
	return out
}

// eventTypes lists the types of events in order.
func eventTypes(events []LeaseEvent) []LeaseEventType {
	out := make([]LeaseEventType, len(events))
	for i, ev := range events {
		out[i] = ev.Type
	}
	return out
}

// eventsFor keeps only one lease's events.
func eventsFor(events []LeaseEvent, id string) []LeaseEvent {
	var out []LeaseEvent
	for _, ev := range events {
		if ev.LeaseID == id {
			out = append(out, ev)
		}
	}
	return out
}

// TestEventLifecyclePerPath pins requirement #1: every lifecycle path
// emits exactly its event, with the lease's owner, a per-process
// monotonic seq and the bus's per-start epoch.
func TestEventLifecyclePerPath(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	all := svc.Subscribe(EventFilter{})

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.checkpointLease(ctx, l); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if _, err := svc.suspend(ctx, "c", l.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if _, err := svc.resume(ctx, "c", l.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, err := svc.restart(ctx, "c", l.ID, ""); err != nil {
		t.Fatalf("restart: %v", err)
	}
	svc.release(ctx, l)
	all.Close() // stop the flow; everything emitted is buffered

	mine := eventsFor(collectEvents(all.C), l.ID)
	// restart of a persistent running lease = pause + resume, so its
	// suspended/resumed pair rides along; the per-step single events are
	// pinned by the sequence below.
	want := []LeaseEventType{
		LeaseCreated, LeaseCheckpointed, LeaseSuspended, LeaseResumed,
		LeaseSuspended, LeaseResumed, LeaseRestarted, LeaseReleased,
	}
	if got := eventTypes(mine); len(got) != len(want) {
		t.Fatalf("lifecycle events = %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("lifecycle events = %v, want %v", got, want)
			}
		}
	}
	// Envelope fields: monotonic seq, this bus's epoch, the owner, an
	// at stamp, and exactly one event per step.
	for i, ev := range mine {
		if ev.Owner != "c" {
			t.Fatalf("event %d owner = %q, want c", i, ev.Owner)
		}
		if ev.Epoch == "" || ev.Epoch != svc.bus.epoch {
			t.Fatalf("event %d epoch = %q, want the bus epoch %q", i, ev.Epoch, svc.bus.epoch)
		}
		if ev.At.IsZero() {
			t.Fatalf("event %d has no at stamp", i)
		}
		if i > 0 && ev.Seq <= mine[i-1].Seq {
			t.Fatalf("event %d seq %d not greater than predecessor %d", i, ev.Seq, mine[i-1].Seq)
		}
	}
	if mine[0].Detail == "" {
		t.Fatal("created event carries no detail")
	}
}

// TestEventForkHolderPaths covers the fork, holder label set/clear and
// pin/unpin emissions (fork emits one created per forked lease; the
// holder paths emit holder_set/holder_cleared; the pin route emits
// pinned/unpinned).
func TestEventForkHolderPaths(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	all := svc.Subscribe(EventFilter{})

	src, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	forks, _, err := svc.fork(ctx, "c", src.ID, 2, false, time.Minute, "", "")
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if len(forks) != 2 {
		t.Fatalf("forked %d leases, want 2", len(forks))
	}

	// Holder label set, pin, unpin, holder clear. A holder label never
	// pins (FS5).
	if _, err := svc.setHolder("c", src.ID, "ci-job", ""); err != nil {
		t.Fatalf("setHolder: %v", err)
	}
	if l := svc.lookup("c", src.ID); l == nil || l.Pinned {
		t.Fatalf("a holder label pinned the lease: %+v", l)
	}
	if _, err := svc.setPinned("c", src.ID, true); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if _, err := svc.setPinned("c", src.ID, false); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	if _, err := svc.setHolder("c", src.ID, "", ""); err != nil {
		t.Fatalf("clear holder: %v", err)
	}
	all.Close()

	mine := eventsFor(collectEvents(all.C), src.ID)
	var got []LeaseEventType
	for _, ev := range mine {
		switch ev.Type {
		case LeaseCreated, LeaseHolderSet, LeasePinned, LeaseUnpinned, LeaseHolderCleared:
			got = append(got, ev.Type)
		}
	}
	want := []LeaseEventType{LeaseCreated, LeaseHolderSet, LeasePinned, LeaseUnpinned, LeaseHolderCleared}
	if len(got) != len(want) {
		t.Fatalf("holder/pin-path events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("holder/pin-path events = %v, want %v", got, want)
		}
	}
}

// TestEventCloneEmitsCreated: the clone path grants a brand-new lease,
// so it emits created for the clone (next to the source's
// checkpointed) — every grant of a new lease id emits one.
func TestEventCloneEmitsCreated(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	all := svc.Subscribe(EventFilter{})

	src, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant source: %v", err)
	}
	cloned, buildID, err := svc.clone(ctx, "c", src.ID)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if buildID == "" {
		t.Fatal("clone returned no checkpoint build id")
	}
	all.Close()

	cloneEvents := eventsFor(collectEvents(all.C), cloned.ID)
	if got := eventTypes(cloneEvents); len(got) != 1 || got[0] != LeaseCreated {
		t.Fatalf("clone's own events = %v, want exactly one %s", got, LeaseCreated)
	}
	if !strings.Contains(cloneEvents[0].Detail, src.ID) || !strings.Contains(cloneEvents[0].Detail, buildID) {
		t.Fatalf("created detail = %q, want the source id and the checkpoint build", cloneEvents[0].Detail)
	}
	if cloneEvents[0].Owner != "c" {
		t.Fatalf("created owner = %q, want c", cloneEvents[0].Owner)
	}
}

// TestEventRecoveredLostPaths: the crash reconcile emits recovered for
// a lease resumed from its checkpoint and lost for one without.
func TestEventRecoveredLostPaths(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	all := svc.Subscribe(EventFilter{})

	ck, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant checkpointed: %v", err)
	}
	if _, err := svc.checkpointLease(ctx, ck); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	bare, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant bare: %v", err)
	}
	sub.Fake.Kill(ck.SandboxID)
	sub.Fake.Kill(bare.SandboxID)
	if summary := svc.reconcileCrash(ctx); summary.Recovered != 1 || summary.Lost != 1 {
		t.Fatalf("summary = %+v, want {Recovered:1, Lost:1}", summary)
	}
	all.Close()

	mine := collectEvents(all.C)
	ckEvents := eventsFor(mine, ck.ID)
	if got := eventTypes(ckEvents); len(got) != 3 || got[2] != LeaseRecovered {
		t.Fatalf("checkpointed lease events = %v, want …%s", got, LeaseRecovered)
	}
	bareEvents := eventsFor(mine, bare.ID)
	if got := eventTypes(bareEvents); len(got) != 2 || got[1] != LeaseLost {
		t.Fatalf("bare lease events = %v, want …%s", got, LeaseLost)
	}
	if ckEvents[2].Detail == "" || bareEvents[1].Detail == "" {
		t.Fatal("recovered/lost events carry no detail")
	}
}

// TestEventOrderingMonotonicPerProcess: concurrent grants still get a
// gap-free, monotonic seq, and every event carries the same epoch.
func TestEventOrderingMonotonicPerProcess(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	all := svc.Subscribe(EventFilter{})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				l, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
				if err != nil {
					t.Errorf("grant: %v", err)
					return
				}
				svc.release(ctx, l)
			}
		}()
	}
	wg.Wait()
	all.Close()
	events := collectEvents(all.C)
	if len(events) != 80 {
		t.Fatalf("got %d events, want 80 (8 workers × 5 × created+released)", len(events))
	}
	for i, ev := range events {
		if ev.Epoch != svc.bus.epoch {
			t.Fatalf("event %d epoch = %q, want %q", i, ev.Epoch, svc.bus.epoch)
		}
		if i > 0 && ev.Seq <= events[i-1].Seq {
			t.Fatalf("seq not monotonic at %d: %d then %d", i, events[i-1].Seq, ev.Seq)
		}
	}
	if events[0].Seq != 1 {
		t.Fatalf("first seq = %d, want 1 (sequence starts at the first event)", events[0].Seq)
	}
}

// TestEventFilterSelection: a subscriber's filter narrows by owner and
// lease.
func TestEventFilterSelection(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	la, err := svc.grant(ctx, "consumer-a", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant a: %v", err)
	}
	lb, err := svc.grant(ctx, "consumer-b", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant b: %v", err)
	}

	ownerA := svc.Subscribe(EventFilter{Owner: "consumer-a"})
	leaseB := svc.Subscribe(EventFilter{LeaseID: lb.ID})

	svc.release(ctx, la)
	svc.release(ctx, lb)
	ownerA.Close()
	leaseB.Close()

	aEvents := collectEvents(ownerA.C)
	for _, ev := range aEvents {
		if ev.Owner != "consumer-a" {
			t.Fatalf("owner filter delivered %s of lease owned by %s", ev.Type, ev.Owner)
		}
	}
	if len(aEvents) != 1 || aEvents[0].Type != LeaseReleased {
		t.Fatalf("owner-filtered events = %v, want one released", eventTypes(aEvents))
	}
	bEvents := collectEvents(leaseB.C)
	if len(bEvents) != 1 || bEvents[0].Type != LeaseReleased {
		t.Fatalf("lease-filtered events = %v, want one released", eventTypes(bEvents))
	}
}

// sseTestServer builds an API test server plus its URL and returns a
// helper reading the SSE stream until the test's context is done.
type sseStream struct {
	t          *testing.T
	body       io.ReadCloser
	scanner    *bufio.Scanner
	events     chan sseEvent
	lastID     string
	lastIDMu   sync.Mutex
	cancelFunc context.CancelFunc
}

type sseEvent struct {
	ID      string
	Type    string
	Payload map[string]any
}

// startSSE opens the stream at url with token, parsing events in the
// background until ctx is done.
func startSSE(t *testing.T, ts *httptest.Server, ctx context.Context, url, token, lastEventID string) *sseStream {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE connect: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("SSE connect status = %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		resp.Body.Close()
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	stream := &sseStream{
		t:          t,
		body:       resp.Body,
		scanner:    bufio.NewScanner(resp.Body),
		events:     make(chan sseEvent, 128),
		cancelFunc: func() { resp.Body.Close() },
	}
	go stream.pump(ctx)
	t.Cleanup(stream.stop)
	return stream
}

// pump parses the SSE wire format: id:, event:, data: lines separated
// by blank lines.
func (s *sseStream) pump(ctx context.Context) {
	defer close(s.events)
	var cur sseEvent
	var data strings.Builder
	flush := func() {
		if cur.Type == "" && data.Len() == 0 {
			return
		}
		cur.Payload = map[string]any{}
		if data.Len() > 0 {
			_ = json.Unmarshal([]byte(data.String()), &cur.Payload)
		}
		s.lastIDMu.Lock()
		s.lastID = cur.ID
		s.lastIDMu.Unlock()
		select {
		case s.events <- cur:
		case <-ctx.Done():
		}
		cur, data = sseEvent{}, strings.Builder{}
	}
	for s.scanner.Scan() {
		line := s.scanner.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "id: "):
			cur.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			cur.Type = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data.WriteString(strings.TrimPrefix(line, "data: "))
		}
	}
	flush()
}

// stop tears the stream down.
func (s *sseStream) stop() {
	s.cancelFunc()
	<-s.events // pump ends when the body closes or the context does
}

// next waits for the next event or fails the test.
func (s *sseStream) next() sseEvent {
	s.t.Helper()
	select {
	case ev, ok := <-s.events:
		if !ok {
			s.t.Fatal("SSE stream ended unexpectedly")
		}
		return ev
	case <-time.After(10 * time.Second):
		s.t.Fatal("timed out waiting for an SSE event")
		return sseEvent{}
	}
}

// expectNone asserts no event arrives within d (used across
// owner-filtering checks where another user's activity must not leak).
func (s *sseStream) expectNone(d time.Duration, why string) {
	s.t.Helper()
	select {
	case ev, ok := <-s.events:
		if ok {
			s.t.Fatalf("%s: unexpected event %s (id %s)", why, ev.Type, ev.ID)
		}
	case <-time.After(d):
	}
}

// TestSSEResumeByLastEventID: a second connection carrying the first
// stream's last id resumes after that event — everything it missed
// arrives, in order, before the live events.
func TestSSEResumeByLastEventID(t *testing.T) {
	ts, _ := newEventsTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s1 := startSSE(t, ts, ctx, ts.URL+"/api/leases/events", "token-a", "")
	l1, createBody := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	if l1.StatusCode != 201 {
		t.Fatalf("create: %d %v", l1.StatusCode, createBody)
	}
	id := createBody["id"].(string)

	first := s1.next() // the created event
	if first.Type != "created" || first.Payload["lease_id"] != id {
		t.Fatalf("first event = %+v, want created for %s", first, id)
	}
	s1.stop()

	// Two more events land while nobody watches: a tag (no event) and a
	// checkpoint (checkpointed) — then the second connection resumes
	// from first.ID and must receive everything after it.
	if _, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "token-a", nil); body == nil {
		t.Fatal("checkpoint request failed")
	}
	time.Sleep(200 * time.Millisecond)

	s2 := startSSE(t, ts, ctx, ts.URL+"/api/leases/events", "token-a", first.ID)
	ck := s2.next()
	if ck.Type != "checkpointed" || ck.Payload["lease_id"] != id {
		t.Fatalf("resumed event = %+v, want checkpointed for %s", ck, id)
	}
	if ck.ID == first.ID {
		t.Fatal("resume redelivered the Last-Event-ID event itself")
	}
	// Resume is replay-from-ring plus live: further events flow.
	l2, _ := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	if l2.StatusCode != 201 {
		t.Fatalf("second create: %d", l2.StatusCode)
	}
	if ev := s2.next(); ev.Type != "created" {
		t.Fatalf("post-resume event = %+v, want created", ev)
	}
	cancel()
}

// TestSSEGapOnUnknownEpoch: a Last-Event-ID from another (older)
// backend run cannot be honoured, so the stream announces a gap first.
func TestSSEGapOnUnknownEpoch(t *testing.T) {
	ts, _ := newEventsTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := startSSE(t, ts, ctx, ts.URL+"/api/leases/events", "token-a", "deadbeef-42")
	gap := s.next()
	if gap.Type != "gap" {
		t.Fatalf("first event = %+v, want gap", gap)
	}
	if !strings.Contains(fmt.Sprint(gap.Payload["detail"]), "epoch") {
		t.Fatalf("gap detail = %v, want it to name the epoch mismatch", gap.Payload["detail"])
	}
	// After the gap, live events flow.
	doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	if ev := s.next(); ev.Type != "created" {
		t.Fatalf("post-gap event = %+v, want created", ev)
	}
	cancel()
}

// TestSSEGapOnMalformedLastEventID: an id that does not parse as
// "<epoch>-<seq>" cannot be honoured, so the stream announces a gap —
// it must not silently start live.
func TestSSEGapOnMalformedLastEventID(t *testing.T) {
	ts, _ := newEventsTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, bad := range []string{"42", "garbage", "-abc-1", "a-b-1"} {
		s := startSSE(t, ts, ctx, ts.URL+"/api/leases/events", "token-a", bad)
		gap := s.next()
		if gap.Type != "gap" {
			t.Fatalf("Last-Event-ID %q: first event = %+v, want gap", bad, gap)
		}
		if !strings.Contains(fmt.Sprint(gap.Payload["detail"]), "Last-Event-ID") {
			t.Fatalf("Last-Event-ID %q: gap detail = %v, want it to name the id", bad, gap.Payload["detail"])
		}
		s.stop()
	}
	cancel()
}

// TestSSEGapWhenPositionLeftRing: a position the ring has aged out of
// must announce the gap over the wire rather than silently resuming
// live.
func TestSSEGapWhenPositionLeftRing(t *testing.T) {
	ts, svc := newEventsTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Emit directly until the ring turns over far past seq 1 (a client's
	// plausible stale position); one grant emits one event.
	for i := 0; i < leaseEventRingSize+10; i++ {
		svc.emitLeaseEvent(fmt.Sprintf("l-%d", i), "c", LeaseCreated, "x")
	}

	// A resume from seq 1 (long evicted) cannot be honoured: gap first.
	s := startSSE(t, ts, ctx, ts.URL+"/api/leases/events", "token-a", svc.bus.Epoch()+"-1")
	gap := s.next()
	if gap.Type != "gap" {
		t.Fatalf("first event = %+v, want gap", gap)
	}
	if !strings.Contains(fmt.Sprint(gap.Payload["detail"]), "no longer in the ring") {
		t.Fatalf("gap detail = %v, want the ring-expiry reason", gap.Payload["detail"])
	}
	// After the gap, live events flow.
	doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	if ev := s.next(); ev.Type != "created" {
		t.Fatalf("post-gap event = %+v, want created", ev)
	}
	cancel()
}

// TestSSEOwnerFiltering: the caller's stream carries only their own
// leases' events, even though the bus is shared, and access is
// re-checked per event.
func TestSSEOwnerFiltering(t *testing.T) {
	ts, svc := newEventsTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// consumer-b creates a lease first so both streams are live.
	_, bBody := doReq(t, "POST", ts.URL+"/api/leases", "token-b", map[string]any{"image": "py-base", "ttl": 300})
	bID := bBody["id"].(string)
	time.Sleep(100 * time.Millisecond)

	sa := startSSE(t, ts, ctx, ts.URL+"/api/leases/events", "token-a", "")
	_, aBody := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	aID := aBody["id"].(string)

	ev := sa.next()
	if ev.Type != "created" || ev.Payload["lease_id"] != aID {
		t.Fatalf("first event for a = %+v, want created of %s", ev, aID)
	}
	// Now b's lease is released: must NOT reach a's stream (the release
	// goes through the service, exactly as production emits it).
	svc.release(ctx, svc.lookup("consumer-b", bID))
	sa.expectNone(500*time.Millisecond, "another owner's release leaked into the stream")

	// a's own release arrives.
	svc.release(ctx, svc.lookup("consumer-a", aID))
	if ev := sa.next(); ev.Type != "released" || ev.Payload["lease_id"] != aID {
		t.Fatalf("own-release event = %+v, want released of %s", ev, aID)
	}
	cancel()
}

// TestSSEPerLease: the per-lease stream carries only that lease, and a
// caller who cannot see the lease gets the standard 404.
func TestSSEPerLease(t *testing.T) {
	ts, _ := newEventsTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, body := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := body["id"].(string)

	s := startSSE(t, ts, ctx, ts.URL+"/api/leases/"+id+"/events", "token-a", "")
	// Other leases come and go; they must not appear here.
	doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	s.expectNone(300*time.Millisecond, "another lease's event leaked into the per-lease stream")

	// A checkpoint on the streamed lease arrives.
	doReq(t, "POST", ts.URL+"/api/leases/"+id+"/checkpoint", "token-a", nil)
	if ev := s.next(); ev.Type != "checkpointed" || ev.Payload["lease_id"] != id {
		t.Fatalf("per-lease event = %+v, want checkpointed of %s", ev, id)
	}

	// Another owner: 404, like every other lease route.
	resp, _ := doReq(t, "GET", ts.URL+"/api/leases/"+id+"/events", "token-b", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-owner per-lease stream status = %d, want 404", resp.StatusCode)
	}

	// Unauthenticated: 401.
	req, _ := http.NewRequest("GET", ts.URL+"/api/leases/"+id+"/events", nil)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated stream status = %d, want 401", resp2.StatusCode)
	}
	cancel()
}

// TestSSEAdminSeesAll: an admin's /events stream is not narrowed to the
// admin's own (usually empty) lease set.
func TestSSEAdminSeesAll(t *testing.T) {
	ts, _, ids := newEventsTestServerWithIdentity(t)
	// The helper's first (and only) user is the admin; its token is the
	// one AddUser was called with.
	users := ids.Users()
	if len(users) != 1 || !users[0].Admin {
		t.Fatalf("identity store users = %+v, want exactly one admin", users)
	}
	admin := "admin-tok"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := startSSE(t, ts, ctx, ts.URL+"/api/leases/events", admin, "")
	// A non-admin user creates a lease; the admin stream must carry it.
	_, body := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	aID := body["id"].(string)
	ev := s.next()
	if ev.Type != "created" || ev.Payload["lease_id"] != aID {
		t.Fatalf("admin stream event = %+v, want created of %s", ev, aID)
	}
	cancel()
}

// TestSubscribeSlowSubscriberDrops (requirement #4): a subscriber that
// stops reading has events dropped — the emitters never block — and is
// told about the loss with a gap event once it reads again.
func TestSubscribeSlowSubscriberDrops(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	sub := svc.Subscribe(EventFilter{}) // nobody reads sub.C yet
	defer sub.Close()

	var released *Lease
	for i := 0; i < leaseEventSubBuffer+50; i++ {
		l, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
		if err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
		released = l
		svc.release(ctx, l)
	}
	// The emitter is never blocked: the loop above finished (each
	// iteration grants and releases).

	// Drain: the channel holds leaseEventSubBuffer events; the overflow
	// was dropped. The first event read may itself be a gap (delivered
	// opportunistically while full); what matters is that a gap appears
	// and that the last event reflects the final release.
	var sawGap bool
	var last LeaseEvent
	deadline := time.After(5 * time.Second)
	for {
		done := true
		select {
		case ev, ok := <-sub.C:
			if !ok {
				t.Fatal("subscription closed early")
			}
			if ev.Type == LeaseStreamGap {
				sawGap = true
			} else {
				last = ev
			}
			done = len(sub.C) == 0
			if !done {
				continue
			}
		case <-deadline:
			t.Fatal("timed out draining the slow subscriber")
		}
		if done {
			break
		}
	}
	if !sawGap {
		t.Fatal("no gap event told the slow subscriber about the drops")
	}
	if last.LeaseID != released.ID || last.Type != LeaseReleased {
		t.Fatalf("last delivered event = %+v (%s of %s), want released of the final lease %s",
			last, last.Type, last.LeaseID, released.ID)
	}
	// The channel is empty now; a new event flows (and a trailing gap
	// announces nothing further since drops were already reported).
	l2, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant after drain: %v", err)
	}
	select {
	case ev := <-sub.C:
		if ev.Type != LeaseCreated || ev.LeaseID != l2.ID {
			t.Fatalf("post-drain event = %+v, want created of %s", ev, l2.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event after the subscriber caught up")
	}
}

// TestEventEmitterNeverBlocksOnSlowReader with the store lock held: a
// subscriber wedged at capacity must not stall a mutation running under
// s.store.mu. This pins the invariant that the bus mutex never nests
// inside a store-lock holder's wait.
func TestEventEmitterNeverBlocksOnSlowReader(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	_ = db

	sub := svc.Subscribe(EventFilter{}) // wedged: nobody reads
	defer sub.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.store.mu.Lock()
		svc.emitLeaseEvent("lease-x", "c", LeaseCreated, "under the lock")
		svc.store.mu.Unlock()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("emit blocked while the subscriber was full")
	}
}

// TestEventEpochChangesPerStart: a rebuilt service gets a fresh epoch,
// so clients can tell a restart from a resume. Same process, new bus.
func TestEventEpochChangesPerStart(t *testing.T) {
	svc1, _, _ := newTestService(t)
	svc2, _, _ := newTestService(t)
	if svc1.bus.Epoch() == svc2.bus.Epoch() {
		t.Fatalf("two services share epoch %q", svc1.bus.Epoch())
	}
	if svc1.bus.Epoch() != svc1.bus.Epoch() {
		t.Fatal("epoch not stable within one process")
	}
}

// TestSSEHeartbeat: an idle stream opens with the retry hint and its
// heartbeat comment paces at the documented 15 s. Waiting out the full
// tick would add 15 s to every test run, so the delay is pinned by the
// constant and the stream's immediate preamble is checked at the wire.
func TestSSEHeartbeat(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	if sseHeartbeatEvery != 15*time.Second {
		t.Fatalf("heartbeat every %v, want the documented 15 s", sseHeartbeatEvery)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		svc.streamEvents(w, r, EventFilter{}, false)
	}))
	t.Cleanup(ts.Close)

	// streamEvents derives the owner from the request context (set by
	// the auth middleware in production); inject it here, with the same
	// key.
	req, _ := http.NewRequest("GET", ts.URL, nil)
	reqCtx := context.WithValue(req.Context(), ctxOwnerKey{}, "consumer-a")
	resp, err := http.DefaultClient.Do(req.WithContext(reqCtx))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()
	// The retry hint goes out immediately; the heartbeat ticker is
	// armed next to it.
	preamble := make([]byte, len("retry: 3000\n\n"))
	if _, err := io.ReadFull(resp.Body, preamble); err != nil {
		t.Fatalf("read preamble: %v", err)
	}
	if string(preamble) != "retry: 3000\n\n" {
		t.Fatalf("preamble = %q", string(preamble))
	}
}

// TestEventTypesDocumented guards the wire shape: every lifecycle type
// renders as its documented string.
func TestEventTypesDocumented(t *testing.T) {
	for typ, want := range map[LeaseEventType]string{
		LeaseCreated:        "created",
		LeaseReleased:       "released",
		LeaseSuspended:      "suspended",
		LeaseResumed:        "resumed",
		LeaseCheckpointed:   "checkpointed",
		LeaseSnapshotSaved:  "snapshot_saved",
		LeaseRecovered:      "recovered",
		LeaseLost:           "lost",
		LeaseRestarted:      "restarted",
		LeaseHolderSet:      "holder_set",
		LeaseHolderCleared:  "holder_cleared",
		LeasePinned:         "pinned",
		LeaseUnpinned:       "unpinned",
		LeasePausedExpiring: "paused_expiring",
		LeasePinnedIdle:     "pinned_idle",
		LeaseBoxFull:        "box_full",
		LeaseAdminUnpin:     "admin_unpin",
		LeaseCrashTest:      "crash_test",
		LeaseRetry:          "recovery_retry",
		LeaseRootfsDead:     "rootfs_dead",
		LeaseUserDeleted:    "user_deleted",
		LeaseStreamGap:      "gap",
	} {
		if string(typ) != want {
			t.Fatalf("type %q drifted from its documented value %q", typ, want)
		}
	}
}

// TestParseLeaseEventID covers the id round-trip and rejects malformed
// Last-Event-IDs — including shapes with the sequence embedded in
// further dash-separated parts, which a lastIndex-based parse would
// misread as (epoch, seq).
func TestParseLeaseEventID(t *testing.T) {
	ev := LeaseEvent{Epoch: "abc123", Seq: 42}
	id := leaseEventID(&ev)
	if id != "abc123-42" {
		t.Fatalf("id = %q, want abc123-42", id)
	}
	epoch, seq, ok := parseLeaseEventID(id)
	if !ok || epoch != "abc123" || seq != 42 {
		t.Fatalf("parse(%q) = %q, %d, %v", id, epoch, seq, ok)
	}
	for _, bad := range []string{"", "-1", "abc-", "abc", "abc-1x", "-abc-1", "a-b-1", "  -1"} {
		if _, _, ok := parseLeaseEventID(bad); ok {
			t.Fatalf("parse(%q) accepted a malformed id", bad)
		}
	}
}

// TestEventRingWraps: the ring keeps the most recent leaseEventRingSize
// events and resume finds positions across the wrap.
func TestEventRingWraps(t *testing.T) {
	bus := newEventBus()
	for i := 1; i <= leaseEventRingSize+10; i++ {
		bus.emit(fmt.Sprintf("l-%d", i), "c", LeaseCreated, "")
	}
	bus.mu.Lock()
	defer bus.mu.Unlock()
	if len(bus.ring) != leaseEventRingSize {
		t.Fatalf("ring length = %d, want %d", len(bus.ring), leaseEventRingSize)
	}
	oldest := bus.ring[bus.start]
	if oldest.Seq != 11 {
		t.Fatalf("oldest buffered seq = %d, want 11 (the first 10 left the ring)", oldest.Seq)
	}
	// Resume from seq 15: the replay starts at 16.
	replay, gap := bus.replayAfterLocked(EventFilter{}, bus.epoch, 15)
	if gap != "" {
		t.Fatalf("resume from a live position gapped: %s", gap)
	}
	if len(replay) != leaseEventRingSize+10-15 {
		t.Fatalf("replay length = %d, want %d", len(replay), leaseEventRingSize+10-15)
	}
	if replay[0].Seq != 16 {
		t.Fatalf("first replayed seq = %d, want 16", replay[0].Seq)
	}
	// A position inside a foreign epoch gaps.
	if _, gap := bus.replayAfterLocked(EventFilter{}, "other", 1); gap == "" {
		t.Fatal("a foreign epoch must gap")
	}
}

// TestResumeAcrossEpochGapsAtAPILevel pins the end-to-end contract of
// requirement #3: the SSE id embeds the epoch, and a resume carrying a
// foreign epoch yields a gap event carrying the current epoch's newest
// position.
func TestResumeAcrossEpochGapsAtAPILevel(t *testing.T) {
	ts, svc := newEventsTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Hand-write a foreign-epoch id; the API must not trust its shape.
	s := startSSE(t, ts, ctx, ts.URL+"/api/leases/events", "token-a", svc.bus.Epoch()+"-999999")
	gap := s.next()
	if gap.Type != "gap" {
		t.Fatalf("first event = %+v, want gap (seq far past the newest)", gap)
	}
	// The gap's id is the current epoch at the newest sequence, so a
	// reconnect from it resumes.
	if e, _, ok := parseLeaseEventID(gap.ID); !ok || e != svc.bus.Epoch() {
		t.Fatalf("gap id = %q, want %s-<newest>", gap.ID, svc.bus.Epoch())
	}
	cancel()
}

// newEventsTestServer builds the standard lease API server for the SSE
// tests.
func newEventsTestServer(t *testing.T) (*httptest.Server, *Service) {
	t.Helper()
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	srv := NewServer(svc, NewImageRegistry(db))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, svc
}

// newEventsTestServerWithIdentity adds an identity store whose first
// user is the admin.
func newEventsTestServerWithIdentity(t *testing.T) (*httptest.Server, *Service, *identity.Store) {
	t.Helper()
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ids, err := identity.NewStore("")
	if err != nil {
		t.Fatalf("identity store: %v", err)
	}
	if _, err := ids.AddUser("root", identity.KindAgent, []string{"SHA256:fp-root"}, "admin-tok"); err != nil {
		t.Fatalf("admin user: %v", err)
	}
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(db))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, svc, ids
}

// compile-time checks that the fake substrate satisfies what the tests
// assume.
var _ substrate.Substrate = (*testSub)(nil)

// TestResumeFromGapPosition: a connect-time gap on a fresh bus carries
// position 0; reconnecting from it replays what happened since instead
// of gapping again.
func TestResumeFromGapPosition(t *testing.T) {
	b := newEventBus()
	sub, _, _, at := b.subscribeWithReplay(EventFilter{}, false, "", 0)
	b.unsubscribe(sub)
	b.emit("l1", "o1", LeaseCreated, "")
	sub, replay, gap, _ := b.subscribeWithReplay(EventFilter{}, true, b.epoch, at)
	defer b.unsubscribe(sub)
	if gap != "" || len(replay) != 1 || replay[0].Seq != 1 {
		t.Fatalf("resume from %d: replay=%v gap=%q, want event 1 and no gap", at, replay, gap)
	}
}

// TestEventsTokenSeesAllStreams: the events-only EVENTS_TOKEN streams
// every owner's events on /api/leases/events, its one route — the
// one-lease streams refuse it.
func TestEventsTokenSeesAllStreams(t *testing.T) {
	ts, svc := newEventsTestServer(t)
	svc.cfg.EventsToken = "events-tok"

	// Two leases, two owners, emitted before the streams open: the
	// stream reads them through Last-Event-ID replay from position 0.
	_, err := svc.grant(context.Background(), "consumer-a", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant a: %v", err)
	}
	l2, err := svc.grant(context.Background(), "consumer-b", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant b: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	all := startSSE(t, ts, ctx, ts.URL+"/api/leases/events", "events-tok", svc.bus.epoch+"-0")
	a, b := all.next(), all.next()
	if a.Type != "created" || b.Type != "created" {
		t.Fatalf("all-events stream = [%q %q], want two created (both owners)", a.Type, b.Type)
	}
	// The id form is <epoch>-<seq>: the dashboard's Last-Event-ID resume
	// key. The replayed events must carry it.
	if id := all.lastIDSnapshot(); !strings.Contains(id, "-") {
		t.Fatalf("event id %q is not <epoch>-<seq>", id)
	}

	// The one-lease streams are outside the events token's scope, even
	// for a lease it could otherwise see: its one route is the
	// all-events stream.
	one := startSSE(t, ts, ctx, ts.URL+"/api/leases/"+l2.ID+"/events", "token-b", svc.bus.epoch+"-0")
	if ev := one.next(); ev.Type != "created" {
		t.Fatalf("one-lease stream first event %q, want created", ev.Type)
	}

	// Live flow: a release on lease 2 reaches both streams (the
	// one-lease stream is filtered to lease 2, the events token's stream
	// sees every owner's).
	svc.release(context.Background(), l2)
	if ev := all.next(); ev.Type != "released" {
		t.Fatalf("events-token stream event %q, want released", ev.Type)
	}
	if ev := one.next(); ev.Type != "released" {
		t.Fatalf("one-lease stream event %q, want released (its filter is the lease)", ev.Type)
	}
}

// lastIDSnapshot reads the last seen SSE id (startSSE tracks it).
func (s *sseStream) lastIDSnapshot() string {
	s.lastIDMu.Lock()
	defer s.lastIDMu.Unlock()
	return s.lastID
}

// TestEventsTokenScopeIsEventsOnly: the EVENTS_TOKEN is refused on
// every route but GET /api/leases/events — the canonical /api/sandboxes
// spelling, the one-lease streams and everything else included. Requests
// hit the handler directly, so the failed-auth throttle (per client IP)
// never trips and the plain status codes are visible.
func TestEventsTokenScopeIsEventsOnly(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	svc.cfg.EventsToken = "events-tok"
	h := NewServer(svc, NewImageRegistry(db)).Handler()

	// Distinct source addresses per call: the failed-auth limiter is per
	// IP and this test makes a dozen failing requests on purpose.
	seq := 0
	do := func(method, path, token string) int {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr = fmt.Sprintf("10.255.%d.%d:%d", seq/250, seq%250, seq)
		seq++
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	// A lease must exist so the {id}/events checks below are meaningful.
	l, err := svc.grant(context.Background(), "consumer-a", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}

	// The streaming GETs are exercised over a live server elsewhere
	// (they block while the stream is open); here just the handler's
	// verdicts. The one-lease streams refuse it in both spellings — same
	// data, one route too wide — as does every deeper path.
	for _, path := range []string{
		"/api/leases/x/y/events",                // not an events route
		"/api/leases/" + l.ID + "/events/extra", // neither
		"/api/leases/" + l.ID + "/events",       // the one-lease form is not its route
		"/api/sandboxes/events",                 // not the documented spelling
	} {
		if got := do("GET", path, "events-tok"); got != http.StatusUnauthorized {
			t.Errorf("GET %s with the events token: %d, want 401", path, got)
		}
	}

	// Every other route refuses it.
	for _, path := range []string{
		"/api/leases", "/api/leases/" + l.ID, "/api/leases/" + l.ID + "/exec", "/api/users", "/metrics",
	} {
		if got := do("GET", path, "events-tok"); got != http.StatusUnauthorized {
			t.Errorf("GET %s with the events token: %d, want 401", path, got)
		}
	}
	// Any method but GET is refused on the stream routes themselves.
	for _, m := range []string{"POST", "DELETE", "PUT"} {
		if got := do(m, "/api/leases/events", "events-tok"); got != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/leases/events with the events token: %d, want 405", m, got)
		}
	}

	// A wrong events token is just an invalid token.
	if got := do("GET", "/api/leases/events", "events-wrong"); got != http.StatusUnauthorized {
		t.Fatalf("wrong events token: %d, want 401", got)
	}

	// Consumer tokens are untouched by all of this.
	if got := do("GET", "/api/sandboxes/"+l.ID, "token-a"); got != http.StatusOK {
		t.Fatalf("consumer token on its lease: %d, want 200", got)
	}

	// An unset EVENTS_TOKEN matches nothing — not even the literal it
	// would have been.
	svc.cfg.EventsToken = ""
	if got := do("GET", "/api/sandboxes/events", ""); got != http.StatusUnauthorized {
		t.Fatalf("stream without credentials: %d, want 401", got)
	}
}

// TestEventsTokenOneLeaseRefused: the events token's one route is the
// all-events stream; the one-lease spelling refuses it outright, even
// for a lease that exists (it would otherwise stream it).
func TestEventsTokenOneLeaseRefused(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	svc.cfg.EventsToken = "events-tok"
	h := NewServer(svc, NewImageRegistry(db)).Handler()
	for _, path := range []string{
		"/api/leases/no-such-lease/events", // unknown lease
		"/api/sandboxes/events",            // the canonical spelling
	} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer events-tok")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s with the events token: %d, want 401", path, rec.Code)
		}
	}
}
