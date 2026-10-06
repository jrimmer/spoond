package notify

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// stepClock is a controllable clock for tests: Now returns the current
// step; Advance moves it. Mutex-guarded: the notifier's sleeper
// goroutine (fakeSleep) advances it from another goroutine than the
// test's own.
type stepClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stepClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TestParseFormat: the three documented formats parse case-insensitively
// (json included), anything else is an error naming the valid set.
func TestParseFormat(t *testing.T) {
	for in, want := range map[string]Format{
		"ntfy": FormatNtfy, "slack": FormatSlack, "json": FormatJSON,
		"NTFY": FormatNtfy, " Slack ": FormatSlack,
	} {
		got, err := ParseFormat(in)
		if err != nil {
			t.Fatalf("ParseFormat(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("ParseFormat(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := ParseFormat(""); err == nil {
		t.Fatal("empty format accepted")
	}
	if _, err := ParseFormat("carrier-pigeon"); err == nil {
		t.Fatal("unknown format accepted")
	}
}

func TestParseWebhooks(t *testing.T) {
	// Unset/empty is no webhooks, not an error.
	if hooks, err := ParseWebhooks(""); err != nil || len(hooks) != 0 {
		t.Fatalf("empty: %v %v", hooks, err)
	}
	hooks, err := ParseWebhooks(`[
		{"url":"https://ntfy.example/x","format":"ntfy"},
		{"url":"http://127.0.0.1:1/slack","format":"slack","min_severity":"warn"},
		{"url":"https://hooks.example/json","format":"json","events":["disk.*","lease.lost.abc"],"headers":{"X-Token":"s"}}
	]`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(hooks) != 3 {
		t.Fatalf("got %d hooks", len(hooks))
	}
	if hooks[1].MinSeverity != Warn {
		t.Fatalf("hook 1 min_severity = %q, want warn", hooks[1].MinSeverity)
	}
	if hooks[0].MinSeverity != Info {
		t.Fatalf("default min_severity = %q, want info", hooks[0].MinSeverity)
	}
	if hooks[2].Headers["X-Token"] != "s" {
		t.Fatal("headers lost")
	}
	for _, bad := range []string{
		`[{"format":"ntfy"}]`,                         // no url
		`[{"url":"ftp://x","format":"ntfy"}]`,         // not http(s)
		`[{"url":"https://x","format":"pigeon"}]`,     // bad format
		`[{"url":"https://x","min_severity":"loud"}]`, // bad severity
		`not json`, // not a list
		`[{"url":"https://x","format":"json"}]{"x":1}`, // trailing junk
	} {
		if _, err := ParseWebhooks(bad); err == nil {
			t.Fatalf("ParseWebhooks(%q) accepted", bad)
		}
	}
	// Errors name the index, never the URL.
	_, err = ParseWebhooks(`[{"url":"https://ok","format":"ntfy"},{"url":"https://secret.example/tok","format":"pigeon"}]`)
	if err == nil || err.Error() != `NOTIFY_WEBHOOKS[1]: unknown format "pigeon" (want ntfy|slack|json)` {
		t.Fatalf("error = %v, want indexed message without URL", err)
	}
}

func TestRedactedURLStripsSecrets(t *testing.T) {
	for in, want := range map[string]string{
		"https://ntfy.example/x?access=topsecret":        "https://ntfy.example",
		"https://user:secretpw@hooks.example.com/x":      "https://hooks.example.com",
		"http://127.0.0.1:9993/topic,second?tok=1#frag":  "http://127.0.0.1:9993",
		"https://hooks.example.com/services/T00/B00/XXX": "https://hooks.example.com",
		"::::": "(unparseable)",
	} {
		if got := (Webhook{URL: in}).RedactedURL(); got != want {
			t.Fatalf("RedactedURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWebhookMatches(t *testing.T) {
	w := Webhook{MinSeverity: Warn, Events: []string{"disk.*", "lease.lost.abc"}}
	if w.matches(Event{Severity: Info}) {
		t.Fatal("info passed a warn floor")
	}
	if !w.matches(Event{Severity: Warn, Key: "disk.warn"}) {
		t.Fatal("warn at the warn floor rejected")
	}
	if !w.matches(Event{Severity: Critical, Key: "disk.warn"}) {
		t.Fatal("glob match failed")
	}
	if !w.matches(Event{Severity: Critical, Key: "lease.lost.abc"}) {
		t.Fatal("exact match failed")
	}
	if w.matches(Event{Severity: Critical, Key: "unit.inactive"}) {
		t.Fatal("unlisted key matched")
	}
	all := Webhook{}
	if !all.matches(Event{Severity: Info, Key: "anything"}) {
		t.Fatal("filter-free webhook rejected an event")
	}
}

func TestRender(t *testing.T) {
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	ev := Event{Key: "disk.warn", Severity: Warn, Title: "disk past warn",
		Body: "85% used", At: at}

	// ntfy: severity maps to priority and tags.
	body, ct, err := render(FormatNtfy, ev, "https://ntfy.example/mytopic")
	if err != nil || ct != "application/json" {
		t.Fatalf("ntfy render: %v %q", err, ct)
	}
	var ntfy struct {
		Topic    string   `json:"topic"`
		Title    string   `json:"title"`
		Message  string   `json:"message"`
		Priority int      `json:"priority"`
		Tags     []string `json:"tags"`
	}
	if err := json.Unmarshal(body, &ntfy); err != nil {
		t.Fatalf("ntfy body: %v", err)
	}
	if ntfy.Priority != 4 || len(ntfy.Tags) != 1 || ntfy.Tags[0] != "warning" {
		t.Fatalf("ntfy priority/tags = %d %v, want 4 [warning]", ntfy.Priority, ntfy.Tags)
	}
	if ntfy.Topic != "mytopic" {
		t.Fatalf("ntfy topic = %q, want mytopic (the URL path's last segment)", ntfy.Topic)
	}
	if ntfy.Title != "disk past warn" || ntfy.Message != "85% used" {
		t.Fatalf("ntfy title/message = %q %q", ntfy.Title, ntfy.Message)
	}

	// slack: {"text": …}, title + body, resolved prefixed.
	text := Event{Key: "k", Severity: Critical, Title: "down", Body: "bad", Resolved: true, At: at}
	body, _, err = render(FormatSlack, text, "")
	if err != nil {
		t.Fatalf("slack render: %v", err)
	}
	var slack struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &slack); err != nil {
		t.Fatalf("slack body: %v", err)
	}
	if slack.Text != "Resolved: down\nbad" {
		t.Fatalf("slack text = %q", slack.Text)
	}

	// json: the event object itself, severity preserved (a resolved
	// critical still travels as critical; Resolved is the marker).
	body, _, err = render(FormatJSON, text, "")
	if err != nil {
		t.Fatalf("json render: %v", err)
	}
	var back Event
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatalf("json body: %v", err)
	}
	if back.Key != "k" || back.Severity != Critical || !back.Resolved || back.Title != "down" {
		t.Fatalf("json round-trip = %+v", back)
	}
	if !back.At.Equal(at) {
		t.Fatalf("json At = %v, want %v", back.At, at)
	}

	// Resolved info events carry the info severity (checks stamp the
	// condition's own level; this is just the pass-through).
	body, _, _ = render(FormatJSON, Event{Key: "k", Severity: Info, Resolved: true, At: at}, "")
	_ = json.Unmarshal(body, &back)
	if back.Severity != Info {
		t.Fatalf("resolved info rendered as %q", back.Severity)
	}
}

// TestSeverityRank pins the min_severity ordering the filter relies on.
func TestSeverityRank(t *testing.T) {
	if !(Info.rank() < Warn.rank() && Warn.rank() < Critical.rank()) {
		t.Fatal("severity ordering broken")
	}
	if Severity("bogus").rank() != 0 {
		t.Fatal("unknown severity should rank as info")
	}
}

// recordingMetrics records every Notification call for assertions.
// Mutex-guarded: the notifier's workers call it from their own
// goroutines while the test reads counts.
type recordingMetrics struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingMetrics) Notification(webhook, severity, result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, webhook+"|"+severity+"|"+result)
}

func (r *recordingMetrics) count(webhook, result string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if len(c) > len(webhook+"|") && c[:len(webhook)+1] == webhook+"|" && c[len(c)-len(result):] == result {
			n++
		}
	}
	return n
}

// TestEnqueueStampsAndDefaults: Enqueue fills in severity and time and
// refuses empty keys.
func TestEnqueueStampsAndDefaults(t *testing.T) {
	n := New(Config{})
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	clock := &stepClock{now: now}
	n.now = clock.Now
	// No dispatcher: the test itself reads the queue (a running
	// dispatchLoop would race it for the event).

	n.Enqueue(Event{}) // empty key: dropped silently
	n.Enqueue(Event{Key: "k"})
	select {
	case ev := <-n.queue:
		if ev.Key != "k" || ev.Severity != Info || !ev.At.Equal(now) {
			t.Fatalf("enqueued = %+v", ev)
		}
	default:
		t.Fatal("event not queued")
	}
}

// TestNtfyPublishURL: the topic leaves the path (it travels in the
// body); a path prefix and the query (?auth=) stay.
func TestNtfyPublishURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://ntfy.example/alerts":                  "https://ntfy.example/",
		"https://ntfy.example/alerts/":                 "https://ntfy.example/",
		"https://ntfy.example.com/ntfy/alerts?auth=tk": "https://ntfy.example.com/ntfy/?auth=tk",
		"https://u:p@ntfy.example/alerts":              "https://u:p@ntfy.example/",
	} {
		if got := ntfyPublishURL(in); got != want {
			t.Errorf("ntfyPublishURL(%q) = %q, want %q", in, got, want)
		}
	}
}
