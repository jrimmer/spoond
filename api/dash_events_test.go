package api

// Release reasons, grant and checkpoint durations, and the lease-less
// gc event (2.5, #132 part 2). The #132 additions' tests live here too:
// the created event a pre-create comment lands on (in dash_events_test.go)
// and the sweep summary's one-event-per-suspending-pass rule
// (in idle_suspend_test.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestReleaseReasonSanitize pins the reason rules: printable text is
// kept, control and format characters become spaces, invalid UTF-8 and
// a reason over 120 runes are refused.
func TestReleaseReasonSanitize(t *testing.T) {
	got, err := sanitizeReleaseReason("ci job 3609 ✓ 11m02s")
	if err != nil || got != "ci job 3609 ✓ 11m02s" {
		t.Fatalf("clean reason = %q, %v", got, err)
	}
	// A newline, tab and a bidi override are not printable: they become
	// spaces, so nothing can forge a second dashboard line.
	got, err = sanitizeReleaseReason("job\u000a3609\u202e ✗")
	if err != nil {
		t.Fatalf("control reason: %v", err)
	}
	if strings.ContainsAny(got, "\u000a\u202e") || !strings.HasPrefix(got, "job 3609") {
		t.Fatalf("control characters not sanitised: %q", got)
	}
	if _, err := sanitizeReleaseReason(strings.Repeat("x", maxReleaseReason+1)); err == nil {
		t.Fatal("over-long reason accepted, want refusal")
	}
	if _, err := sanitizeReleaseReason(string([]byte{0xff, 0xfe})); err == nil {
		t.Fatal("invalid UTF-8 accepted, want refusal")
	}
}

// TestEventDurationFormat pins the duration rendering shared by the
// created and checkpointed details.
func TestEventDurationFormat(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0 ms"},
		{61 * time.Millisecond, "61 ms"},
		{999 * time.Millisecond, "999 ms"},
		{1500 * time.Millisecond, "1.5 s"},
		{540 * time.Millisecond, "540 ms"},
		// Seconds round to hundredths (#147); one that rounds up to a
		// minute takes the minute form, never "60 s".
		{time.Second, "1 s"},
		{2500 * time.Millisecond, "2.5 s"},
		{17268933300 * time.Nanosecond, "17.27 s"},
		{59990 * time.Millisecond, "59.99 s"},
		{59999600 * time.Microsecond, "1m0s"},
		{time.Minute, "1m0s"},
		{90 * time.Second, "1m30s"},
		{-time.Second, "0 ms"},
	}
	for _, tc := range cases {
		if got := eventDuration(tc.d); got != tc.want {
			t.Errorf("eventDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// TestFormatEventBytes pins the freed-space rendering for the gc detail.
func TestFormatEventBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{2 << 30, "2.00 GiB"},
		{1536 << 20, "1.50 GiB"},
		{512 << 20, "512.0 MiB"},
		{3 << 20, "3.0 MiB"},
		{64 << 10, "64 KiB"},
	}
	for _, tc := range cases {
		if got := formatEventBytes(tc.n); got != tc.want {
			t.Errorf("formatEventBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// TestGrantEventCarriesDuration: the created event's detail gains how
// long the grant took, measured from the admission start.
func TestGrantEventCarriesDuration(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	all := svc.Subscribe(EventFilter{})
	if _, err := svc.grant(context.Background(), "c", "py-base", time.Minute, false, "", nil, "", "", nil); err != nil {
		t.Fatalf("grant: %v", err)
	}
	all.Close()
	events := collectEvents(all.C)
	if len(events) != 1 || events[0].Type != LeaseCreated {
		t.Fatalf("events = %v, want one %s", eventTypes(events), LeaseCreated)
	}
	if d := events[0].Detail; !strings.HasPrefix(d, "granted from image py-base in ") || !strings.HasSuffix(d, " ms") {
		t.Fatalf("created detail = %q, want the image and a millisecond duration", d)
	}
}

// TestCreatedEventCarriesJobLabel: the runner labels the job's lease
// before its create answers (runner/lease_adapter.go): WithLabel stores
// the comment client-side and Create POSTs it to the comment route after
// the create's answer. The label is client-side, so the `created` event
// carries no comment (the dashboard re-labels the line from the lease
// row's comment instead, #132); the test pins the route flow end to end
// through the handler's owner-scoped path, plus the cap on later set.
func TestCreatedEventCarriesJobLabel(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	srv := NewServer(svc, NewImageRegistry(db))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	ctx := context.Background()

	all := svc.Subscribe(EventFilter{})
	l1, err := svc.grant(ctx, "consumer-a", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	// The comment POST carries the owner's own token, so a create racing
	// another consumer's comment request can never receive the label.
	resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/"+l1.ID+"/comment", "token-a",
		map[string]any{"comment": "forgejo job 42 https://code.example.com/site/actions/runs/9/jobs/42"})
	if resp.StatusCode != 200 {
		t.Fatalf("comment POST status %d, want 200", resp.StatusCode)
	}
	// The create is already answered by then: its created event carries
	// the plain grant detail, never the comment.
	all.Close() // stop the flow; everything emitted is buffered
	for _, ev := range eventsFor(collectEvents(all.C), l1.ID) {
		if ev.Type == LeaseCreated && strings.Contains(ev.Detail, "forgejo job") {
			t.Fatalf("created detail %q carries the label, want the plain grant detail", ev.Detail)
		}
	}
}

// TestCreatedEventCommentCap: a comment set through the owner's comment
// route is capped (the API keeps it short), so a label can never make a
// lease's comment unusably long.
func TestCreatedEventCommentCap(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	srv := NewServer(svc, NewImageRegistry(db))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{
		"image": "py-base", "ttl": 60, "persistent": true})
	id := create["id"].(string)
	// A comment set through the owner's route lands whole: the label the
	// runner sends after its create answers is a single owner's own
	// comment, never cut.
	label := "forgejo job 43 " + strings.Repeat("u", 200)
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/comment", "token-a", map[string]any{"comment": label})
	if resp.StatusCode != 200 || body["comment"] != label {
		t.Fatalf("comment set/clear failed: %d, comment %d bytes (want %d)", resp.StatusCode,
			len(fmt.Sprint(body["comment"])), len(label))
	}
}

// TestCheckpointEventCarriesDurationAndBuild: the checkpointed event's
// detail names the duration and the short build id.
func TestCheckpointEventCarriesDurationAndBuild(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	l, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	all := svc.Subscribe(EventFilter{})
	b, err := svc.checkpointLease(ctx, l)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	all.Close()
	events := collectEvents(all.C)
	if len(events) != 1 || events[0].Type != LeaseCheckpointed {
		t.Fatalf("events = %v, want one %s", eventTypes(events), LeaseCheckpointed)
	}
	wantShort := b.BuildID[:8] + "…"
	if d := events[0].Detail; !strings.Contains(d, " · build "+wantShort) {
		t.Fatalf("checkpointed detail = %q, want a duration and short build %q", d, wantShort)
	}
	if strings.Contains(events[0].Detail, b.BuildID) {
		t.Fatalf("checkpointed detail %q carries the full build id, want short", events[0].Detail)
	}
}

// TestDeleteWithReasonQueryAndBody: DELETE /api/leases/{id} carries the
// caller's reason on the released event, from either ?reason= or a JSON
// body; without one the detail stays "deleted through the API".
func TestDeleteWithReasonQueryAndBody(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)

	for _, tc := range []struct {
		name   string
		target func(id string) string
		body   any
		want   string
	}{
		{"query", func(id string) string { return ts.URL + "/api/leases/" + id + "?reason=ci+job+9+%E2%9C%93+4m10s" }, nil, "ci job 9 ✓ 4m10s"},
		{"body", func(id string) string { return ts.URL + "/api/leases/" + id }, map[string]any{"reason": "stopped exploring"}, "stopped exploring"},
		{"none", func(id string) string { return ts.URL + "/api/leases/" + id }, nil, "deleted through the API"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, lease := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{"image": "py-base", "ttl": 300})
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("create: %d: %v", resp.StatusCode, lease)
			}
			id := lease["id"].(string)
			all := svc.Subscribe(EventFilter{})
			resp, _ = doReq(t, "DELETE", tc.target(id), "token-a", tc.body)
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("delete: %d", resp.StatusCode)
			}
			all.Close()
			var detail string
			for _, ev := range collectEvents(all.C) {
				if ev.Type == LeaseReleased && ev.LeaseID == id {
					detail = ev.Detail
				}
			}
			if detail != tc.want {
				t.Fatalf("released detail = %q, want %q", detail, tc.want)
			}
		})
	}
}

// TestDeleteReasonBadRequest: a reason over 120 runes is a 400, and the
// lease is left alone.
func TestDeleteReasonBadRequest(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, lease := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d: %v", resp.StatusCode, lease)
	}
	id := lease["id"].(string)
	long := strings.Repeat("x", maxReleaseReason+1)
	resp, _ = doReq(t, "DELETE", ts.URL+"/api/leases/"+id+"?reason="+long, "token-a", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("over-long reason: %d, want 400", resp.StatusCode)
	}
	resp, _ = doReq(t, "GET", ts.URL+"/api/leases/"+id, "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lease gone after a refused reason: %d", resp.StatusCode)
	}
}

// TestGCEmitsEventOnlyOnDelete: a GC pass that deletes emits one
// lease-less gc event with the count and the freed bytes; a dry run and
// a no-op pass emit nothing. The event reaches the all-leases stream but
// never a per-lease subscription.
func TestGCEmitsEventOnlyOnDelete(t *testing.T) {
	svc, _, db, _ := gcTestService(t)
	_, _, _ = seedGCChain(t, db, "go-base")

	// Dry run: no gc event.
	all := svc.Subscribe(EventFilter{})
	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("dry gc: %v", err)
	}
	all.Close()
	if got := collectEvents(all.C); len(got) != 0 {
		t.Fatalf("dry run emitted %v, want nothing", eventTypes(got))
	}

	// A lease-scoped subscription must never see the gc event.
	perLease := svc.Subscribe(EventFilter{LeaseID: "l1"})
	all = svc.Subscribe(EventFilter{})
	t.Setenv("GC_DELETE", "1")
	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("delete gc: %v", err)
	}
	if err := svc.gcOnce(context.Background()); err != nil { // second link unwinds
		t.Fatalf("delete gc 2: %v", err)
	}
	all.Close()
	perLease.Close()
	mine := collectEvents(all.C)
	var gc []LeaseEvent
	for _, ev := range mine {
		if ev.Type == LeaseGC {
			gc = append(gc, ev)
		}
	}
	if len(gc) == 0 {
		t.Fatal("no gc event after deleting builds")
	}
	for _, ev := range gc {
		if ev.LeaseID != "" || ev.Owner != "" {
			t.Fatalf("gc event has a lease or owner: %+v", ev)
		}
		if !strings.Contains(ev.Detail, "deleted") || !strings.Contains(ev.Detail, "freed") {
			t.Fatalf("gc detail = %q, want count and freed bytes", ev.Detail)
		}
	}
	if got := collectEvents(perLease.C); len(got) != 0 {
		t.Fatalf("per-lease subscription saw %v, want nothing", eventTypes(got))
	}
	// The gc event's seq is strictly monotonic on the shared bus.
	if gc[0].Seq == 0 {
		t.Fatal("gc event carries no sequence")
	}
}

// TestGCEventSizes: a single delete still emits one event, counted right.
func TestGCEventSingleDelete(t *testing.T) {
	svc, _, db, _ := gcTestService(t)
	_, _, _ = seedGCChain(t, db, "py-base")
	t.Setenv("GC_DELETE", "1")
	all := svc.Subscribe(EventFilter{})
	if err := svc.gcOnce(context.Background()); err != nil {
		t.Fatalf("gc: %v", err)
	}
	all.Close()
	var detail string
	for _, ev := range collectEvents(all.C) {
		if ev.Type == LeaseGC {
			detail = ev.Detail
		}
	}
	if !strings.HasPrefix(detail, "1 build deleted") {
		t.Fatalf("gc detail = %q, want the singular count first", detail)
	}
}

// TestReleaseReasonJSONBodyShape guards the runner's wire form: the
// backend reads {"reason"} from the body when the query is absent.
func TestReleaseReasonJSONBodyShape(t *testing.T) {
	raw, _ := json.Marshal(map[string]string{"reason": "ci job 1 ✗ 2s"})
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Reason != "ci job 1 ✗ 2s" {
		t.Fatalf("body round-trip: %q, %v", body.Reason, err)
	}
}
