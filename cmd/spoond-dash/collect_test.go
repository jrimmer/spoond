package spoonddash

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// sseServer is a minimal /api/leases/events stand-in: it answers GETs
// that carry the right bearer, records the Last-Event-ID each connection
// sent, streams one event, stays open until its channel is closed (or
// the client goes away) — so a reconnect against it is observable.
type sseServer struct {
	t        *testing.T
	token    string
	URL      string
	mu       sync.Mutex
	lastIDs  []string
	closed   chan struct{}
	once     sync.Once
	requests int
}

func newSSEServer(t *testing.T, token string) *sseServer {
	s := &sseServer{t: t, token: token, closed: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests++
		s.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+s.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/api/leases/events" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		s.mu.Lock()
		s.lastIDs = append(s.lastIDs, r.Header.Get("Last-Event-ID"))
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no streaming", http.StatusInternalServerError)
			return
		}
		// The bus's replayed events carry ids <epoch>-<seq>; the first
		// connection gets one and a reconnect must come back for it.
		fmt.Fprintf(w, "id: epoch1-1\nevent: created\ndata: {\"seq\":1,\"epoch\":\"epoch1\",\"at\":\"2026-10-04T12:00:00Z\",\"lease_id\":\"abcdef0123456789\",\"owner\":\"ci\",\"type\":\"created\",\"detail\":\"granted from image py-base\"}\n\n")
		fl.Flush()
		<-s.closed // hold the stream open until the test ends it
	}))
	s.URL = srv.URL
	t.Cleanup(func() {
		s.once.Do(func() { close(s.closed) })
		srv.Close()
	})
	return s
}

func (s *sseServer) lastID(n int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n >= len(s.lastIDs) {
		s.t.Fatalf("connection %d never arrived (have %d)", n, len(s.lastIDs))
	}
	return s.lastIDs[n]
}

func (s *sseServer) end() { s.once.Do(func() { close(s.closed) }) }

// TestStreamEventsSSE: the collector's subscription authenticates with
// the events token, buffers what the stream sends (newest first, at
// most maxEvents), and resumes a fresh connection with the last id it
// saw as Last-Event-ID.
func TestStreamEventsSSE(t *testing.T) {
	srv := newSSEServer(t, "events-tok")
	c := newCollector(Config{MetricsServerName: "127.0.0.1"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.streamEvents(ctx, srv.URL+"/api/leases/events", "events-tok")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.events.newest(1)) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	evs := c.events.newest(maxEvents)
	if len(evs) != 1 {
		t.Fatalf("buffer = %d events, want the one the server streamed", len(evs))
	}
	ev := evs[0]
	if ev.Type != "created" || ev.LeaseID != "abcdef0123" || ev.Subject != "ci" {
		t.Fatalf("buffered event = %+v", ev)
	}
	if want := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC); !ev.At.Equal(want) {
		t.Fatalf("event at %v, want %v", ev.At, want)
	}

	// End the open stream: the loop reconnects, sending the id it saw as
	// Last-Event-ID so the backend replays exactly the missed events.
	srv.end()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		srv.mu.Lock()
		n := srv.requests
		srv.mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := srv.lastID(1); got != "epoch1-1" {
		t.Fatalf("reconnect Last-Event-ID = %q, want epoch1-1", got)
	}
}

// TestStreamEventsBadTokenBacksOff: a stream the server refuses (a
// wrong token here) is retried with a back-off, not hammered — two
// connections at most within the first second.
func TestStreamEventsBadTokenBacksOff(t *testing.T) {
	srv := newSSEServer(t, "events-tok")
	c := newCollector(Config{EventsToken: "events-tok"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c.streamEvents(ctx, srv.URL+"/api/leases/events", "wrong-token")
	srv.mu.Lock()
	n := srv.requests
	srv.mu.Unlock()
	if n == 0 {
		t.Fatal("the collector never connected")
	}
	if n > 2 { // t=0 and t≈1s: the 2 s back-off keeps the third away
		t.Fatalf("%d connections in 1 s, want at most 2 (backing off)", n)
	}
}

// TestEventsURL: the stream URL is the metrics URL with the events path.
func TestEventsURL(t *testing.T) {
	cases := map[string]string{
		"https://127.0.0.1:8890/metrics":            "https://127.0.0.1:8890/api/leases/events",
		"http://backend.internal:8890/metrics":      "http://backend.internal:8890/api/leases/events",
		"https://vm.example.com:8890/metrics?x=1":   "https://vm.example.com:8890/api/leases/events",
		"https://vm.example.com/api/leases/events/": "https://vm.example.com/api/leases/events",
	}
	for in, want := range cases {
		if got := eventsURL(in); got != want {
			t.Errorf("eventsURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestEventBufferCap: the buffer keeps the newest maxEvents and reports
// them newest first.
func TestEventBufferCap(t *testing.T) {
	buf := &eventBuffer{}
	for i := 0; i < maxEvents+10; i++ {
		buf.add(dashEvent{Type: fmt.Sprintf("e%02d", i)})
	}
	got := buf.newest(maxEvents)
	if len(got) != maxEvents {
		t.Fatalf("got %d events, want %d", len(got), maxEvents)
	}
	if got[0].Type != fmt.Sprintf("e%02d", maxEvents+9) || got[len(got)-1].Type != "e10" {
		t.Fatalf("newest first broken: first %q last %q", got[0].Type, got[len(got)-1].Type)
	}
}

// TestEventLinesFormat: the panel's lines are
// "HH:MM:SS  <type padded to 14>  <lease id 10>  <subject>" in the
// frame clock's zone, styled by type; a gap is a marker line; without
// DASH_EVENTS_TOKEN the panel says so, dim.
func TestEventLinesFormat(t *testing.T) {
	c := newCollector(Config{}) // no events token
	lines := c.eventLines(time.Unix(1_800_000_000, 0))
	if len(lines) != 1 || lines[0].Text != "events need DASH_EVENTS_TOKEN" || lines[0].Style != "dim" {
		t.Fatalf("no-token lines = %+v", lines)
	}

	c.cfg.EventsToken = "events-tok"
	at := time.Date(2026, 10, 4, 7, 19, 2, 0, time.UTC)
	// Arrived in stream order: lost a second before the release, the
	// held action last. The panel shows the newest arrival first.
	c.events.add(dashEvent{At: at.Add(-2 * time.Second), Type: "lost", LeaseID: "1234567890", Subject: "agent"})
	c.events.add(dashEvent{At: at.Add(-time.Second), Type: "released", LeaseID: "abcdef0123", Subject: "jason"})
	c.events.add(dashEvent{At: at, Type: "held_action", LeaseID: "fedcba0987", Subject: "nightly"})

	lines = c.eventLines(at)
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	if want := "07:19:02  held_action     fedcba0987  nightly"; lines[0].Text != want || lines[0].Style != "warn" {
		t.Fatalf("line 0 = %+v, want %q", lines[0], want)
	}
	if want := "07:19:01  released        abcdef0123  jason"; lines[1].Text != want || lines[1].Style != "dim" {
		t.Fatalf("line 1 = %+v, want %q", lines[1], want)
	}
	if want := "07:19:00  lost            1234567890  agent"; lines[2].Text != want || lines[2].Style != "warn" {
		t.Fatalf("line 2 = %+v, want %q", lines[2], want)
	}

	// A gap (events missed on a reconnect) is a marker, not an empty row.
	c.events.add(dashEvent{At: at.Add(time.Second), Type: "gap"})
	if l := c.eventLines(at.Add(time.Second))[0]; l.Text != "07:19:03  ┄ events missed while reconnecting" || l.Style != "warn" {
		t.Fatalf("gap line = %+v", l)
	}

	// Only the newest eventPanelRows lines are shown.
	for i := 0; i < eventPanelRows+3; i++ {
		c.events.add(dashEvent{At: at.Add(time.Duration(i) * time.Second), Type: "created"})
	}
	lines = c.eventLines(at.Add(10 * time.Second))
	if len(lines) != eventPanelRows {
		t.Fatalf("got %d lines, want %d", len(lines), eventPanelRows)
	}
	last := at.Add((eventPanelRows + 2) * time.Second)
	if !strings.HasPrefix(lines[0].Text, last.Format("15:04:05")) {
		t.Fatalf("newest not first: %q", lines[0].Text)
	}
	if strings.Contains(lines[0].Text, "  created") && !strings.Contains(lines[0].Text, "created") {
		t.Fatalf("bad line: %q", lines[0].Text)
	}
}

// TestEventLinesSubjectPrefersHolder: the tail column is the lease's
// holder from the last tick's rows, else its comment, else the event's
// owner — released leases are gone from the table by the time their
// event is drawn, so the owner the event carries names them.
func TestEventLinesSubjectPrefersHolder(t *testing.T) {
	c := newCollector(Config{EventsToken: "events-tok"})
	c.lastRow = []LeaseRow{{ID: "abcdef0123", Holder: "forgejo/job-42", Name: "ignored"}}
	c.events.add(dashEvent{At: time.Unix(1_800_000_000, 0), Type: "created", LeaseID: "abcdef0123", Subject: "ci"})
	lines := c.eventLines(time.Unix(1_800_000_000, 0))
	if !strings.HasSuffix(lines[0].Text, "forgejo/job-42") {
		t.Fatalf("holder not preferred: %q", lines[0].Text)
	}

	// A CI job lease: no holder, but the comment names the job.
	c.lastRow = []LeaseRow{{ID: "abcdef0123", Comment: "forgejo: lacy.casa/site #218"}}
	lines = c.eventLines(time.Unix(1_800_000_000, 0))
	if !strings.HasSuffix(lines[0].Text, "forgejo: lacy.casa/site #218") {
		t.Fatalf("comment not second: %q", lines[0].Text)
	}

	// A lease the table no longer has (released): the owner.
	c.lastRow = nil
	lines = c.eventLines(time.Unix(1_800_000_000, 0))
	if !strings.HasSuffix(lines[0].Text, " ci") {
		t.Fatalf("owner not last resort: %q", lines[0].Text)
	}
}

// TestEventLinesDetail: the event's detail follows the subject, with
// build ids cut to 8 characters.
func TestEventLinesDetail(t *testing.T) {
	c := newCollector(Config{EventsToken: "t"})
	at := time.Date(2026, 10, 5, 4, 15, 59, 0, time.UTC)
	c.events.add(dashEvent{At: at, Type: "suspended", LeaseID: "12aeb66651", Subject: "conformance",
		Detail: "paused into build 1ede0933-20dc-40ee-9350-e6f77652a856"})
	want := "04:15:59  suspended       12aeb66651  conformance                       paused into build 1ede0933"
	if got := c.eventLines(at)[0].Text; got != want {
		t.Fatalf("line = %q\nwant   %q", got, want)
	}
}
