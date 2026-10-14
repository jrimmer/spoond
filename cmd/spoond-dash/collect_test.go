package spoonddash

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v3/store"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
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

// TestDiskRoomFromMetrics: the collector reads the snapshot disk's
// room from the backend's gauges, and leaves it unknown when the scrape
// does not carry them.
func TestDiskRoomFromMetrics(t *testing.T) {
	parse := func(text string) map[string]*dto.MetricFamily {
		t.Helper()
		p := expfmt.NewTextParser(model.UTF8Validation)
		fams, err := p.TextToMetricFamilies(strings.NewReader(text))
		if err != nil {
			t.Fatal(err)
		}
		return fams
	}
	c := newCollector(Config{Interval: time.Second, History: 10})
	var s Snapshot
	c.fromMetrics(&s, parse("spoond_snapshot_disk_reserved_bytes 104857600\nspoond_snapshot_disk_usable_bytes -2048\n"), time.Now())
	if !s.DiskRoomKnown || s.DiskRoom != (DiskRoom{Reserved: 100 << 20, Usable: -2048}) {
		t.Fatalf("room = %+v known=%v, want reserved 100 MiB, usable -2048", s.DiskRoom, s.DiskRoomKnown)
	}
	s = Snapshot{}
	c.fromMetrics(&s, parse("spoond_leases_active 1\n"), time.Now())
	if s.DiskRoomKnown {
		t.Fatal("room known without the gauges")
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
	if want := "07:19:02  held_action  fedcba0987  nightly"; lines[0].Text != want || lines[0].Style != "warn" {
		t.Fatalf("line 0 = %+v, want %q", lines[0], want)
	}
	if want := "07:19:01  released     abcdef0123  jason"; lines[1].Text != want || lines[1].Style != "dim" {
		t.Fatalf("line 1 = %+v, want %q", lines[1], want)
	}
	if want := "07:19:00  lost         1234567890  agent"; lines[2].Text != want || lines[2].Style != "warn" {
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

// TestEventLinesIdleSuspended: the events panel draws an idle_suspended
// event as a warn line naming the idle duration (2.5, #129 part 2).
func TestEventLinesIdleSuspended(t *testing.T) {
	c := newCollector(Config{EventsToken: "events-tok"})
	at := time.Date(2026, 10, 4, 7, 19, 2, 0, time.UTC)
	c.events.add(dashEvent{At: at, Type: "idle_suspended", LeaseID: "abcdef0123", Subject: "jason", Detail: "idle for 1m0s"})
	lines := c.eventLines(at)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	l := lines[0]
	if !strings.Contains(l.Text, "idle_suspended") || !strings.Contains(l.Text, "jason") ||
		!strings.Contains(l.Text, "idle for 1m0s") {
		t.Fatalf("idle_suspended line = %+v, want type, subject and detail", l)
	}
	if l.Style != "warn" {
		t.Fatalf("idle_suspended style = %q, want warn", l.Style)
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
	c.lastRow = []LeaseRow{{ID: "abcdef0123", Comment: "forgejo: example.com/site #218"}}
	lines = c.eventLines(time.Unix(1_800_000_000, 0))
	// (cut to the subject column's cap, eventSubjectMax)
	if !strings.HasSuffix(lines[0].Text, ellipsize("forgejo: example.com/site #218", eventSubjectMax)) {
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
	want := "04:15:59  suspended  12aeb66651  conformance  paused into build 1ede0933"
	if got := c.eventLines(at)[0].Text; got != want {
		t.Fatalf("line = %q\nwant   %q", got, want)
	}
}

// TestEventLinesGC: a lease-less gc event draws with the ok colour, a
// "spoond" subject and its count-and-freed detail; a per-lease panel
// never sees it because the collector's subscription is all-leases. A
// gc pass whose detail names a stale-build failure draws warn instead
// (spoond-rzz).
func TestEventLinesGC(t *testing.T) {
	c := newCollector(Config{EventsToken: "t"})
	at := time.Date(2026, 10, 5, 4, 15, 59, 0, time.UTC)
	c.events.add(dashEvent{At: at, Type: "gc", Detail: "3 builds deleted · 1.50 GiB freed"})
	lines := c.eventLines(at)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	l := lines[0]
	if l.Style != "ok" {
		t.Fatalf("gc line style = %q, want ok", l.Style)
	}
	if !strings.Contains(l.Text, "gc") || !strings.Contains(l.Text, "spoond") ||
		!strings.Contains(l.Text, "3 builds deleted · 1.50 GiB freed") {
		t.Fatalf("gc line = %+v, want type, spoond subject and detail", l)
	}
	// The lease id column is empty for a lease-less event.
	if strings.Contains(l.Text, "  ") && !strings.HasPrefix(l.Text, "04:15:59  gc") {
		t.Fatalf("gc line does not start with the time and type: %q", l.Text)
	}

	// A stale-build failure is a gc pass that went wrong: warn.
	c.events.add(dashEvent{At: at.Add(time.Second), Type: "gc",
		Detail: "stale build 1ede0933 failed · build timed out"})
	failed := c.eventLines(at.Add(time.Second))[0]
	if failed.Style != "warn" {
		t.Fatalf("stale-build gc line style = %q, want warn", failed.Style)
	}
	if !strings.Contains(failed.Text, "stale build 1ede0933") {
		t.Fatalf("stale-build gc line = %+v", failed)
	}
}

// TestEventLinesCIJobStart: a created event whose lease's comment is the
// runner's job label ("forgejo job <id> <url>", executor.go) is
// re-labelled as the job's start (2.6, #132): the type word "runner" in
// the green ok style, the short lease id in its own column, the subject
// naming the job ("ci job N started"). A created event for any other
// lease stays a plain created line.
func TestEventLinesCIJobStart(t *testing.T) {
	c := newCollector(Config{EventsToken: "t"})
	at := time.Date(2026, 10, 5, 4, 15, 59, 0, time.UTC)
	c.lastRow = []LeaseRow{{ID: "abcdef0123", Comment: "forgejo job 3609 https://code.example.com/site/actions/runs/91/jobs/3609"}}
	c.events.add(dashEvent{At: at, Type: "created", LeaseID: "abcdef0123", Subject: "ci",
		Detail: "granted from image py-base in 61 ms"})
	lines := c.eventLines(at)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	l := lines[0]
	want := "04:15:59  runner  abcdef0123  ci job 3609 started  granted from image py-base in 61 ms"
	if l.Text != want {
		t.Fatalf("ci job start line =\n%q\nwant\n%q", l.Text, want)
	}
	if l.Style != "ok" {
		t.Fatalf("ci job start style = %q, want ok", l.Style)
	}

	// A created event for any other lease stays a plain created line,
	// text-styled, id column filled.
	c.events.add(dashEvent{At: at, Type: "created", LeaseID: "abcdef9999", Subject: "jason",
		Detail: "granted from image py-base in 2 ms"})
	plain := c.eventLines(at)[0]
	if !strings.Contains(plain.Text, "created") || !strings.Contains(plain.Text, "abcdef9999") || plain.Style != "text" {
		t.Fatalf("plain created line broken: %+v", plain)
	}
}

// TestEventLinesSweepSummary: one sweep event (the backend's #132
// summary of an idle-sweep pass that suspended leases) draws
// "idle sweep · N suspended" (the drawn word, not the bus's bare
// "sweep"), lease-less, subject spoond, its type word and line dim — the
// news is the per-lease idle_suspended lines. The idle sweep's
// suspension column is the detail, and the drawn type word sizes the
// type column ("idle sweep", 10 runes) for the other lines too.
func TestEventLinesSweepSummary(t *testing.T) {
	c := newCollector(Config{EventsToken: "t"})
	at := time.Date(2026, 10, 5, 4, 15, 59, 0, time.UTC)
	c.events.add(dashEvent{At: at, Type: "sweep", Detail: "2 suspended"})
	lines := c.eventLines(at)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	l := lines[0]
	want := "04:15:59  idle sweep              spoond  2 suspended"
	if l.Text != want {
		t.Fatalf("sweep line =\n%q\nwant\n%q", l.Text, want)
	}
	if l.Style != "dim" {
		t.Fatalf("sweep line style = %q, want dim", l.Style)
	}

	// The lease id column is empty for the lease-less summary, like the
	// gc line's (TestEventLinesGC): the padded empty id column leaves one
	// 2-space gap around a 10-space id field between the columns.
	i := strings.Index(l.Text, "idle sweep")
	j := strings.Index(l.Text, "spoond")
	if i < 0 || j < 0 || j-i-10 != 14 {
		t.Fatalf("the lease id column is not collapsed for a lease-less sweep: %q", l.Text)
	}
}

// TestEventLinesLeaseLessPlaceholder: the box_full and admin_unpin
// events carry the placeholder lease id "-"; the events panel still
// names spoond as their subject, not the placeholder (spoond-k0uz L10).
func TestEventLinesLeaseLessPlaceholder(t *testing.T) {
	c := newCollector(Config{EventsToken: "t"})
	at := time.Date(2026, 10, 5, 4, 15, 59, 0, time.UTC)
	c.events.add(dashEvent{At: at, Type: "box_full", LeaseID: "-",
		Detail: "4096 MiB request for \"owner-b\": every take-back candidate is pinned"})
	l := c.eventLines(at)[0]
	if !strings.Contains(l.Text, "spoond") {
		t.Fatalf("lease-less box_full line = %+v, want the spoond subject", l)
	}
	// The placeholder id is drawn in the id column (documented, L10),
	// but the subject is spoond, never the placeholder.
	if !strings.Contains(l.Text, "box_full  -") {
		t.Fatalf("the placeholder id is not shown in the id column: %q", l.Text)
	}
}

// TestEventLinesFailedCIReleaseWarn: a released event whose reason names
// a failure (the runner's ✗) draws warn; a plain release stays dim.
func TestEventLinesFailedCIReleaseWarn(t *testing.T) {
	c := newCollector(Config{EventsToken: "t"})
	at := time.Date(2026, 10, 5, 4, 15, 59, 0, time.UTC)
	c.events.add(dashEvent{At: at.Add(-time.Second), Type: "released", LeaseID: "abcdef0124",
		Subject: "jason", Detail: "deleted through the API"})
	c.events.add(dashEvent{At: at, Type: "released", LeaseID: "abcdef0123",
		Subject: "ci", Detail: "ci job 3604 ✗ 4m10s"})
	lines := c.eventLines(at)
	if lines[0].Style != "warn" {
		t.Fatalf("failed CI release style = %q, want warn", lines[0].Style)
	}
	if lines[1].Style != "dim" {
		t.Fatalf("plain release style = %q, want dim", lines[1].Style)
	}
}

// TestEventLinesNewDetails: the created and checkpointed details pass
// through with their build ids shortened for the panel.
func TestEventLinesNewDetails(t *testing.T) {
	c := newCollector(Config{EventsToken: "t"})
	at := time.Date(2026, 10, 5, 4, 15, 59, 0, time.UTC)
	c.events.add(dashEvent{At: at.Add(-time.Second), Type: "created", LeaseID: "abcdef0123",
		Subject: "jason", Detail: "granted from image py-base in 61 ms"})
	c.events.add(dashEvent{At: at, Type: "checkpointed", LeaseID: "abcdef0123",
		Subject: "jason", Detail: "540 ms · build 9e1f2ab3-20dc-40ee-9350-e6f77652a856"})
	lines := c.eventLines(at)
	if !strings.Contains(lines[1].Text, "granted from image py-base in 61 ms") {
		t.Fatalf("created line = %q", lines[1].Text)
	}
	if !strings.Contains(lines[0].Text, "540 ms · build 9e1f2ab3") ||
		strings.Contains(lines[0].Text, "9e1f2ab3-20dc") {
		t.Fatalf("checkpointed line = %q, want the short build id", lines[0].Text)
	}
}

// TestParsePressure: /proc/pressure/io's some and full lines carry their
// avg10 and avg60 percentages; a malformed value is skipped, not
// mistaken for a calm zero.
func TestParsePressure(t *testing.T) {
	some10, some60, full10, full60, ok := parsePressure([]byte(
		"some avg10=0.30 avg60=0.21 avg300=0.10 total=1234\n" +
			"full avg10=0.00 avg60=0.00 avg300=0.00 total=0\n"))
	if !ok || some10 != 0.30 || some60 != 0.21 || full10 != 0 || full60 != 0 {
		t.Fatalf("parse = %v %v %v %v ok=%v", some10, some60, full10, full60, ok)
	}
	// A full stall: the values the notification trips on.
	if _, _, f10, f60, ok := parsePressure([]byte("some avg10=1.0 avg60=2.5\nfull avg10=20.0 avg60=15.5\n")); !ok || f10 != 20 || f60 != 15.5 {
		t.Fatalf("full parse = %v %v ok=%v", f10, f60, ok)
	}
	// An empty or unrecognised file says "no PSI", not "no pressure".
	if _, _, _, _, ok := parsePressure([]byte("# nothing\n")); ok {
		t.Fatal("empty pressure file must report !ok")
	}
	// A malformed avg is not treated as zero.
	if _, _, _, f60, _ := parsePressure([]byte("full avg10=1 avg60=oops\n")); f60 != 0 {
		t.Fatalf("malformed avg60 = %v", f60)
	}
}

// TestParseDiskstats: the device's write sectors and busy ms come from
// fields 10 and 13; another device's line is ignored and an unknown
// device reports !ok.
func TestParseDiskstats(t *testing.T) {
	sample := []byte(
		"8 0 sda 1 2 3 4 5 6 7 8 9 10 11\n" +
			"259 0 nvme0n1 1 2 3 4 5 6 3000 7 8 30 9\n")
	got, ok := parseDiskstats(sample, "nvme0n1")
	if !ok || got.writeSectors != 3000 || got.ioMs != 30 {
		t.Fatalf("nvme0n1 = %+v ok=%v, want 3000 sectors and 30 ms", got, ok)
	}
	if _, ok := parseDiskstats(sample, "nvme9n9"); ok {
		t.Fatal("unknown device must report !ok")
	}
	if _, ok := parseDiskstats([]byte("nonsense\n"), "sda"); ok {
		t.Fatal("short line must report !ok")
	}
}

// TestFromIODelta: the collector turns two /proc/diskstats samples into
// a write rate and a busy share over the collection interval, and reads
// the PSI gauges from the pressure file.
func TestFromIODelta(t *testing.T) {
	dir := t.TempDir()
	pressure := filepath.Join(dir, "io")
	diskstats := filepath.Join(dir, "diskstats")
	if err := os.WriteFile(pressure, []byte("some avg10=0.30 avg60=0.20\nfull avg10=0.00 avg60=0.00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first := "259 0 nvme0n1 0 0 0 0 0 0 1000 0 0 500 0\n"
	if err := os.WriteFile(diskstats, []byte(first), 0o644); err != nil {
		t.Fatal(err)
	}
	c := newCollector(Config{StoragePath: dir, DiskDevice: "nvme0n1"})
	c.pressurePath, c.diskstatsPath = pressure, diskstats
	t0 := time.Unix(1_800_000_000, 0)
	c.now = func() time.Time { return t0 }

	var s Snapshot
	c.fromIO(&s, t0)
	if !s.IOAvail || s.IOSome60 != 0.2 || s.IOFull60 != 0 || s.DiskDevice != "nvme0n1" {
		t.Fatalf("first sample: %+v", s)
	}
	if s.DiskWriteMB != 0 || s.DiskBusyPct != 0 {
		t.Fatalf("first sample has no delta: write=%v busy=%v", s.DiskWriteMB, s.DiskBusyPct)
	}

	// The next sample, 10 s later: 20000 more sectors (10 MiB) written
	// and 3500 more ms busy (35% of the 10 s wall clock).
	second := "259 0 nvme0n1 0 0 0 0 0 0 21000 0 0 4000 0\n"
	if err := os.WriteFile(diskstats, []byte(second), 0o644); err != nil {
		t.Fatal(err)
	}
	s = Snapshot{}
	c.fromIO(&s, t0.Add(10*time.Second))
	if want := 1.0; s.DiskWriteMB != want {
		t.Fatalf("write MB/s = %v, want %v", s.DiskWriteMB, want)
	}
	if want := 35.0; s.DiskBusyPct != want {
		t.Fatalf("busy pct = %v, want %v", s.DiskBusyPct, want)
	}

	// A missing PSI file (kernel without PSI) hides the item: no
	// IOAvail, no error.
	c.pressurePath = filepath.Join(dir, "missing")
	var noPSI Snapshot
	c.fromIO(&noPSI, t0)
	if noPSI.IOAvail {
		t.Fatal("missing /proc/pressure/io must leave IOAvail false")
	}
}

// TestDevMajorMinor: the statfs dev_t encoding round-trips for typical
// disk and partition numbers.
func TestDevMajorMinor(t *testing.T) {
	cases := []struct {
		dev          uint64
		major, minor uint32
	}{
		{0x0800, 8, 0},    // /dev/sda
		{0x0801, 8, 1},    // /dev/sda1
		{0x10300, 259, 0}, // /dev/nvme0n1: major 259 (0x103), minor 0
	}
	for _, tc := range cases {
		maj, min := devMajorMinor(tc.dev)
		if maj != tc.major || min != tc.minor {
			t.Errorf("devMajorMinor(%#x) = %d:%d, want %d:%d", tc.dev, maj, min, tc.major, tc.minor)
		}
	}
}

// TestBlockDevicePartition: a partition resolves to its parent whole
// disk (nvme0n1p1 -> nvme0n1), a whole disk stays itself, and an
// unresolvable major:minor is empty.
func TestBlockDevicePartition(t *testing.T) {
	sys := t.TempDir()
	// /sys/dev/block/259:0 -> ../../devices/.../nvme0n1 (whole disk)
	// /sys/dev/block/259:1 -> .../nvme0n1/nvme0n1p1 (partition)
	disk := filepath.Join(sys, "devices", "pci", "nvme0n1")
	part := filepath.Join(disk, "nvme0n1p1")
	if err := os.MkdirAll(part, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(part, "partition"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sys, "dev", "block"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(disk, filepath.Join(sys, "dev", "block", "259:0")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(part, filepath.Join(sys, "dev", "block", "259:1")); err != nil {
		t.Fatal(err)
	}
	if got := blockDevice(sys, 259, 0); got != "nvme0n1" {
		t.Errorf("whole disk = %q, want nvme0n1", got)
	}
	if got := blockDevice(sys, 259, 1); got != "nvme0n1" {
		t.Errorf("partition = %q, want its parent nvme0n1", got)
	}
	if got := blockDevice(sys, 8, 0); got != "" {
		t.Errorf("unresolvable = %q, want empty", got)
	}
}

// imagesFixture is an images-panel sort test's fixture: a catalog DB
// with the full schema (image_uses, lease_history from migration 29),
// and a collector pointing at it.
type imagesFixture struct {
	c   *collector
	raw *sql.DB
	now time.Time
}

// newImagesFixture builds the fixture in a temp dir, clock fixed at
// 2026-10-14 12:00 UTC.
func newImagesFixture(t *testing.T) *imagesFixture {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{DBPath: filepath.Join(dir, "spoond.db"), UsersFile: filepath.Join(dir, "users.json"), StoragePath: dir}
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	return &imagesFixture{raw: raw, now: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC), c: newCollector(cfg)}
}

// addImage inserts one built image (current_build_id set, so the panel
// lists it).
func (f *imagesFixture) addImage(t *testing.T, name string, vcpu, memMiB int) {
	t.Helper()
	_, err := f.raw.Exec(`INSERT INTO images (name, template_id, current_build_id, vcpu, memory_mb, disk_mb, updated_at)
		VALUES (?, 't', 'b', ?, ?, 1024, ?)`,
		name, vcpu, memMiB, f.now.Add(-24*time.Hour).Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
}

// setUses writes an image's lifetime grant count.
func (f *imagesFixture) setUses(t *testing.T, image string, n int) {
	t.Helper()
	if _, err := f.raw.Exec(`INSERT INTO image_uses (image, uses) VALUES (?, ?)
		ON CONFLICT(image) DO UPDATE SET uses = excluded.uses`, image, n); err != nil {
		t.Fatal(err)
	}
}

// addHist inserts one lease_history row.
func (f *imagesFixture) addHist(t *testing.T, leaseID, image string, ended time.Time) {
	t.Helper()
	if _, err := f.raw.Exec(`INSERT INTO lease_history (lease_id, owner, image, memory_mb, created_at, ended_at, outcome)
		VALUES (?, 'u-1', ?, 1024, ?, ?, 'released')`,
		leaseID, image, ended.Add(-time.Hour).Format(time.RFC3339Nano), ended.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
}

// addLive inserts one live lease on an image.
func (f *imagesFixture) addLive(t *testing.T, id, image, state string) {
	t.Helper()
	if _, err := f.raw.Exec(`INSERT INTO leases (id, owner, image, created_at, expires_at, last_active, state)
		VALUES (?, 'u-1', ?, ?, ?, ?, ?)`,
		id, image, f.now.Add(-time.Minute).Format(time.RFC3339Nano),
		f.now.Add(time.Hour).Format(time.RFC3339Nano), f.now.Format(time.RFC3339Nano), state); err != nil {
		t.Fatal(err)
	}
}

// images runs one fromDB and returns the panel's image order.
func (f *imagesFixture) images(t *testing.T) []ImageRow {
	t.Helper()
	var s Snapshot
	if err := f.c.fromDB(&s, f.now); err != nil {
		t.Fatal(err)
	}
	return s.Images
}

// TestImagesSortByUse pins the images panel's order: live leases lead,
// then most recent use — the newest lease_history end, a live lease
// counting as in use now — then lifetime uses, then name.
func TestImagesSortByUse(t *testing.T) {
	f := newImagesFixture(t)
	// live-now: a live lease, so its last use is the frame's now.
	f.addImage(t, "live-now", 2, 2048)
	f.addLive(t, "l1", "live-now", "running")
	// old-live: two live leases, so it leads live-now (live first).
	f.addImage(t, "old-live", 2, 2048)
	f.addLive(t, "l2", "old-live", "suspended")
	f.addLive(t, "l3", "old-live", "suspended")
	// fresh: no live lease, the newest history end.
	f.addImage(t, "fresh", 2, 2048)
	f.addHist(t, "h1", "fresh", f.now.Add(-2*time.Hour))
	// stale: no live lease, an older history end.
	f.addImage(t, "stale", 2, 2048)
	f.addHist(t, "h2", "stale", f.now.Add(-3*24*time.Hour))
	// by-uses: no live lease, no history; more lifetime uses wins.
	f.addImage(t, "by-uses", 2, 2048)
	f.setUses(t, "by-uses", 9)
	// by-name: nothing at all; the name breaks the tie.
	f.addImage(t, "zeta", 2, 2048)
	f.addImage(t, "alpha", 2, 2048)

	got := f.images(t)
	var names []string
	for _, im := range got {
		names = append(names, im.Name)
	}
	want := []string{"old-live", "live-now", "fresh", "stale", "by-uses", "alpha", "zeta"}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("order = %v, want %v", names, want)
	}
	// A live lease counts its last use as the frame's now, history or
	// not; an ended-only image's last use is its newest end.
	for _, im := range got {
		switch im.Name {
		case "live-now", "old-live":
			if !im.LastUsed.Equal(f.now) {
				t.Errorf("%s: LastUsed = %v, want the frame's now", im.Name, im.LastUsed)
			}
		case "fresh":
			if !im.LastUsed.Equal(f.now.Add(-2 * time.Hour)) {
				t.Errorf("fresh: LastUsed = %v, want the newest history end", im.LastUsed)
			}
		}
	}
}

// TestImagesSortMissingHistoryTable: a catalog without lease_history yet
// (an older backend) sorts on live, then uses, then name — the missing
// table is no history, not an error.
func TestImagesSortMissingHistoryTable(t *testing.T) {
	f := newImagesFixture(t)
	if _, err := f.raw.Exec(`DROP TABLE lease_history`); err != nil {
		t.Fatal(err)
	}
	f.addImage(t, "quiet", 2, 2048)
	f.addImage(t, "busy", 2, 2048)
	f.setUses(t, "busy", 5)
	f.addImage(t, "held", 2, 2048)
	f.addLive(t, "l1", "held", "running")
	got := f.images(t)
	if len(got) != 3 || got[0].Name != "held" || got[1].Name != "busy" || got[2].Name != "quiet" {
		t.Fatalf("order = %v", got)
	}
}

// TestSortImages pins sortImages' comparisons directly, history or no
// history: an empty LastUsed (no rows at all yet) sorts last among its
// live tier.
func TestSortImages(t *testing.T) {
	now := fixedNow
	imgs := []ImageRow{
		{Name: "b-live", Live: 1, LastUsed: now.Add(-time.Hour)},
		{Name: "a-dead", Uses: 1},
		{Name: "a-live", Live: 1, LastUsed: now},
		{Name: "z-dead", Uses: 2},
	}
	sortImages(imgs)
	want := []string{"a-live", "b-live", "z-dead", "a-dead"}
	for i, w := range want {
		if imgs[i].Name != w {
			t.Fatalf("order = %v, want %v", imgs, want)
		}
	}
}
