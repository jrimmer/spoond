package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// receiver is a test webhook endpoint: it records every request's path,
// query, headers and body, and fails the first n deliveries with the
// given status before answering 200.
type receiver struct {
	mu      sync.Mutex
	fail    int // first n requests get failStatus, then 200
	status  int
	reqURLs []string // as the server saw them (r.URL.String())
	bodies  []string
	tokens  []string // the X-Token header per request
	close   func()
}

func newReceiver(fail int, status int) *receiver {
	return &receiver{fail: fail, status: status}
}

func (r *receiver) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.reqURLs = append(r.reqURLs, req.URL.String())
		r.bodies = append(r.bodies, string(b))
		r.tokens = append(r.tokens, req.Header.Get("X-Token"))
		r.mu.Unlock()
		r.mu.Lock()
		fail := r.fail > 0
		if fail {
			r.fail--
		}
		r.mu.Unlock()
		if fail {
			w.WriteHeader(r.status)
			fmt.Fprint(w, "no")
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})
}

// server runs the receiver on an httptest server.
func (r *receiver) server(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(r.handler())
	t.Cleanup(ts.Close)
	return ts
}

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

// text returns the request bodies as JSON objects.
func (r *receiver) payload(t *testing.T, i int) map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out map[string]any
	if err := json.Unmarshal([]byte(r.bodies[i]), &out); err != nil {
		t.Fatalf("body %d not JSON: %v (%q)", i, err, r.bodies[i])
	}
	return out
}

// fakeSleep lets tests drive the retry clock: each call records the
// requested backoff and advances the shared step clock instantly.
type fakeSleep struct {
	clock     *stepClock
	mu        sync.Mutex
	durations []time.Duration
}

func (f *fakeSleep) sleep(ctx context.Context, d time.Duration) bool {
	f.mu.Lock()
	f.durations = append(f.durations, d)
	f.mu.Unlock()
	f.clock.Advance(d)
	select {
	case <-ctx.Done():
		return false
	default:
		return true
	}
}

func (f *fakeSleep) all() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.durations...)
}

func (f *fakeSleep) len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.durations)
}

// syncBuffer is a goroutine-safe bytes.Buffer for log assertions.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testLogger returns a logger discarding output (for tests that only
// need a non-nil logger).
func testLogger(t *testing.T) *log.Logger {
	t.Helper()
	return log.New(io.Discard, "", 0)
}

// newTestNotifier builds a notifier around one webhook pointing at the
// receiver, with the injected clock/sleep. Started; the returned stop
// must be called by the test (deferred by the caller).
func newTestNotifier(t *testing.T, hook Webhook, clock *stepClock, sleep *fakeSleep, m Metrics) (*Notifier, context.CancelFunc) {
	t.Helper()
	n := New(Config{
		Webhooks: []Webhook{hook},
		Now:      clock.Now,
		Sleep:    sleep.sleep,
		Metrics:  m,
	})
	ctx, cancel := context.WithCancel(context.Background())
	n.Start(ctx)
	return n, cancel
}

// waitUntil polls cond until it holds or the deadline passes.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestDeliveryHappyPath: one event fans out to two webhooks, each in
// its own format, and the metric records sent per webhook index.
func TestDeliveryHappyPath(t *testing.T) {
	nf := newReceiver(0, 500)
	js := newReceiver(0, 500)
	nfSrv, jsSrv := nf.server(t), js.server(t)
	clock := &stepClock{now: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
	metrics := &recordingMetrics{}
	n := New(Config{
		Webhooks: []Webhook{
			{URL: nfSrv.URL + "/ntfytopic", Format: FormatNtfy},
			{URL: jsSrv.URL + "?secret=1", Format: FormatJSON, MinSeverity: Critical},
		},
		Now:     clock.Now,
		Metrics: metrics,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n.Start(ctx)

	n.Enqueue(Event{Key: "lease.lost.abc", Severity: Critical, Title: "lost", Body: "gone"})
	waitUntil(t, "delivery to both hooks", func() bool { return nf.count() == 1 && js.count() == 1 })

	if got := nf.payload(t, 0)["title"]; got != "lost" {
		t.Fatalf("ntfy title = %v", got)
	}
	// ntfy takes JSON at the server root, the topic in the body.
	if got := nf.reqURLs[0]; got != "/" {
		t.Fatalf("ntfy request path = %q, want / (JSON publishes go to the root)", got)
	}
	if got := nf.payload(t, 0)["topic"]; got != "ntfytopic" {
		t.Fatalf("ntfy topic = %v", got)
	}
	if got := js.payload(t, 0)["key"]; got != "lease.lost.abc" {
		t.Fatalf("json key = %v", got)
	}
	waitUntil(t, "sent metrics", func() bool {
		return metrics.count("0", ResultSent) == 1 && metrics.count("1", ResultSent) == 1
	})
}

// TestSeverityFilter: an info event never reaches a min_severity warn
// webhook, and a resolved critical does — the resolve carries the
// condition's severity, not a flat info.
func TestSeverityFilter(t *testing.T) {
	r := newReceiver(0, 500)
	srv := r.server(t)
	clock := &stepClock{now: time.Now()}
	n := New(Config{
		Webhooks: []Webhook{{URL: srv.URL, Format: FormatJSON, MinSeverity: Warn}},
		Now:      clock.Now,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n.Start(ctx)

	// Info is filtered; warn passes; a resolved critical passes (the
	// resolve carries the condition's severity, not a flat info).
	n.Enqueue(Event{Key: "other.key", Severity: Info, Title: "info"})
	n.Enqueue(Event{Key: "disk.warn", Severity: Warn, Title: "warn"})
	n.Enqueue(Event{Key: "disk.other", Severity: Critical, Resolved: true, Title: "cleared"})
	waitUntil(t, "two deliveries", func() bool { return r.count() == 2 })

	p0 := r.payload(t, 0)
	if p0["title"] != "warn" || p0["resolved"] != nil {
		t.Fatalf("first delivery = %v", p0)
	}
	p1 := r.payload(t, 1)
	if p1["resolved"] != true || p1["severity"] != "critical" {
		t.Fatalf("resolved delivery = %v (want critical severity)", p1)
	}
}

// TestResolvedDeliveredToWarnWebhook is the reviewer's reproduction:
// a warn alert, then the condition clearing — the min_severity warn
// webhook must receive both the alert and the resolution.
func TestResolvedDeliveredToWarnWebhook(t *testing.T) {
	r := newReceiver(0, 500)
	srv := r.server(t)
	clock := &stepClock{now: time.Now()}
	n := New(Config{
		Webhooks: []Webhook{{URL: srv.URL, Format: FormatJSON, MinSeverity: Warn}},
		Now:      clock.Now,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n.Start(ctx)

	n.Enqueue(Event{Key: "disk.warn", Severity: Warn, Title: "past warn"})
	waitUntil(t, "alert", func() bool { return r.count() == 1 })
	clock.Advance(2 * time.Minute) // out of nothing; dedupe is per-event below
	n.Enqueue(Event{Key: "disk.warn", Severity: Warn, Resolved: true, Title: "cleared"})
	waitUntil(t, "resolve", func() bool { return r.count() == 2 })
	if p := r.payload(t, 1); p["resolved"] != true {
		t.Fatalf("second delivery = %v, want resolved", p)
	}
}

// TestDedupeOncePerHour: the same key is delivered once per hour, an
// alert and its resolve each get their own window, and a condition
// that fires and clears inside the hour notifies both times.
func TestDedupeOncePerHour(t *testing.T) {
	r := newReceiver(0, 500)
	srv := r.server(t)
	clock := &stepClock{now: time.Now()}
	n := New(Config{Webhooks: []Webhook{{URL: srv.URL, Format: FormatJSON}}, Now: clock.Now})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n.Start(ctx)

	n.Enqueue(Event{Key: "unit.inactive", Severity: Critical, Title: "a"})
	n.Enqueue(Event{Key: "unit.inactive", Severity: Critical, Title: "again"})
	n.Enqueue(Event{Key: "unit.inactive", Severity: Critical, Resolved: true, Title: "cleared"})
	waitUntil(t, "alert + resolve within the hour", func() bool { return r.count() == 2 })

	// 59 minutes later both windows are still shut.
	clock.Advance(59 * time.Minute)
	n.Enqueue(Event{Key: "unit.inactive", Severity: Critical, Title: "still down"})
	time.Sleep(20 * time.Millisecond)
	if r.count() != 2 {
		t.Fatalf("dedupe leaked inside the hour: %d deliveries", r.count())
	}
	// At a full hour the key is deliverable again.
	clock.Advance(time.Minute)
	n.Enqueue(Event{Key: "unit.inactive", Severity: Critical, Title: "still down"})
	waitUntil(t, "post-window delivery", func() bool { return r.count() == 3 })
}

// TestRetryBackoffThenDrop: a failing webhook retries 2s, 4s, 8s … and
// gives up once the next backoff would pass the 1 h cap — counted
// dropped, recorded to the state file, redacted in logs.
func TestRetryBackoffThenDrop(t *testing.T) {
	r := newReceiver(1<<30, http.StatusServiceUnavailable) // always fail
	srv := r.server(t)
	clock := &stepClock{now: time.Now()}
	sleep := &fakeSleep{clock: clock}
	statePath := t.TempDir() + "/state.json"
	metrics := &recordingMetrics{}
	n := New(Config{
		Webhooks:  []Webhook{{URL: srv.URL + "/TOKEN-IN-PATH?access=SECRET9", Format: FormatJSON}},
		StatePath: statePath,
		Log:       testLogger(t),
		Now:       clock.Now,
		Sleep:     sleep.sleep,
		Metrics:   metrics,
	})
	ctx, cancel := context.WithCancel(context.Background())
	n.Start(ctx)
	defer cancel()

	n.Enqueue(Event{Key: "gc.failed", Severity: Warn, Title: "gc broke"})
	// Attempts: 1 (fails, backoff 2s) … attempt 11 (fails, backoff
	// 2048s = 34m8s). Attempt 12's backoff would be 4096s > the 1 h cap,
	// so it is never made: the message is dropped there, counted once.
	waitUntil(t, "give-up", func() bool { return metrics.count("0", ResultDropped) == 1 })

	backoffs := sleep.all()
	if len(backoffs) != 11 {
		t.Fatalf("backoffs = %v, want 11 (2s…34m8s)", backoffs)
	}
	want := 2 * time.Second
	for _, d := range backoffs {
		if d != want {
			t.Fatalf("backoff %v, want %v (schedule %v)", d, want, backoffs)
		}
		want *= 2
	}
	if want <= maxBackoff {
		t.Fatalf("next backoff %v should be past the %s cap", want, maxBackoff)
	}

	// The state file carries the redacted failure for the doctor.
	fails, err := LoadFailures(statePath, clock.Now())
	if err != nil || len(fails) != 1 {
		t.Fatalf("state file = %v, %v", fails, err)
	}
	if strings.Contains(fails[0].Error, "SECRET9") || strings.Contains(fails[0].Error, "TOKEN-IN-PATH") {
		t.Fatalf("state file leaked the URL: %q", fails[0].Error)
	}
}

// TestRetryLogRedactsSecrets: with a webhook whose URL carries a token,
// neither the retry log nor the give-up log emits the secret.
func TestRetryLogRedactsSecrets(t *testing.T) {
	r := newReceiver(1<<30, http.StatusInternalServerError)
	srv := r.server(t)
	clock := &stepClock{now: time.Now()}
	sleep := &fakeSleep{clock: clock}
	buf := new(syncBuffer)
	n := New(Config{
		Webhooks: []Webhook{{URL: srv.URL + "/TOKEN-IN-PATH?access=SECRET9", Format: FormatSlack}},
		Log:      log.New(buf, "", 0),
		Now:      clock.Now,
		Sleep:    sleep.sleep,
	})
	ctx, cancel := context.WithCancel(context.Background())
	n.Start(ctx)
	defer cancel()
	n.Enqueue(Event{Key: "k", Severity: Info, Title: "t"})
	waitUntil(t, "first attempts", func() bool { return sleep.len() >= 2 })
	time.Sleep(10 * time.Millisecond)

	if s := buf.String(); strings.Contains(s, "SECRET9") || strings.Contains(s, "TOKEN-IN-PATH") {
		t.Fatalf("log leaked the URL: %q", s)
	}
}

// TestRateLimit: the 32nd delivery within an hour waits instead of
// going out.
func TestRateLimit(t *testing.T) {
	r := newReceiver(0, 500)
	srv := r.server(t)
	clock := &stepClock{now: time.Now()}
	sleep := &fakeSleep{clock: clock}
	n := New(Config{Webhooks: []Webhook{{URL: srv.URL, Format: FormatJSON}}, Now: clock.Now, Sleep: sleep.sleep})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n.Start(ctx)

	for i := 0; i < rateMax; i++ {
		n.Enqueue(Event{Key: fmt.Sprintf("k%d", i), Severity: Info, Title: "t"})
	}
	waitUntil(t, "first 30 delivered", func() bool { return r.count() == rateMax })

	// 31st distinct key inside the window: rate-limited onto the retry
	// track with the wait as its backoff — the window frees when the
	// oldest slot (burst time) ages out at the full hour.
	n.Enqueue(Event{Key: "overflow", Severity: Info, Title: "t"})
	waitUntil(t, "overflow scheduled with a backoff", func() bool { return sleep.len() > 0 })
	clock.Advance(time.Hour)
	waitUntil(t, "overflow delivered after the wait", func() bool { return r.count() == rateMax+1 })
}

// TestWorkerParksWhenIdle: a notifier that receives nothing must not
// spin (regression guard for the retry-clock rework): enqueue one
// event, let it deliver, and confirm the worker stays quiet.
func TestWorkerParksWhenIdle(t *testing.T) {
	r := newReceiver(0, 500)
	srv := r.server(t)
	clock := &stepClock{now: time.Now()}
	n := New(Config{Webhooks: []Webhook{{URL: srv.URL, Format: FormatJSON}}, Now: clock.Now})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n.Start(ctx)
	n.Enqueue(Event{Key: "k", Severity: Info, Title: "t"})
	waitUntil(t, "delivery", func() bool { return r.count() == 1 })
	clock.Advance(5 * time.Minute)
	time.Sleep(20 * time.Millisecond)
	if r.count() != 1 {
		t.Fatalf("idle worker invented deliveries: %d", r.count())
	}
}

// TestLoadFailuresMissingFileIsClean: no state file means no failures,
// not an error (the doctor's healthy case).
func TestLoadFailuresMissingFileIsClean(t *testing.T) {
	fails, err := LoadFailures(t.TempDir()+"/absent.json", time.Now())
	if err != nil || len(fails) != 0 {
		t.Fatalf("missing file = %v, %v; want empty, nil", fails, err)
	}
}

// TestRedactErr keeps URL-bearing error strings' hosts out.
func TestRedactErr(t *testing.T) {
	err := fmt.Errorf(`Post "http://127.0.0.1:1/x?access=SECRET": dial tcp: connection refused`)
	if got := redactErr(err); strings.Contains(got, "SECRET") || strings.Contains(got, "127.0.0.1") {
		t.Fatalf("redactErr left the URL in: %q", got)
	}
	// A real *url.Error keeps its whole cause, colons and all.
	tlsErr := &url.Error{Op: "Post", URL: "https://h.example/x?access=SECRET",
		Err: errors.New("tls: failed to verify certificate: x509: certificate has expired or is not yet valid: current time 2026-10-04T00:00:00Z is after 2026-01-01T00:00:00Z")}
	if got := redactErr(tlsErr); strings.Contains(got, "SECRET") || !strings.HasPrefix(got, "tls: failed to verify certificate: x509: certificate has expired") || !strings.HasSuffix(got, "2026-01-01T00:00:00Z") {
		t.Fatalf("redactErr(url.Error) = %q", got)
	}
	plain := fmt.Errorf("HTTP 500")
	if got := redactErr(plain); got != "HTTP 500" {
		t.Fatalf("redactErr(plain) = %q", got)
	}
}

// TestWebhookRefusesRedirect: a redirecting endpoint is a failure, not
// a silent success at the redirect target — following it would replay
// the configured headers (potential secrets) somewhere else.
func TestWebhookRefusesRedirect(t *testing.T) {
	var hit, landed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		landed.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		http.Redirect(w, r, target.URL+"/landing", http.StatusFound)
	}))
	defer redirector.Close()

	clock := &stepClock{now: time.Now()}
	sleep := &fakeSleep{clock: clock}
	n := New(Config{
		Webhooks: []Webhook{{URL: redirector.URL, Format: FormatJSON, Headers: map[string]string{"X-Token": "secret"}}},
		Now:      clock.Now,
		Sleep:    sleep.sleep,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n.Start(ctx)
	n.Enqueue(Event{Key: "k", Severity: Info, Title: "t"})
	waitUntil(t, "redirect treated as failure", func() bool { return sleep.len() > 0 })
	if !hit.Load() {
		t.Fatal("redirector was not called at all")
	}
	if landed.Load() {
		t.Fatal("the redirect was followed")
	}
}

// TestFreshEventNotBlockedByRetry: a failing webhook's backoff must
// not delay a new event on the same webhook — the worker parks on the
// queue, not inside the sleep (regression for the retry rework).
func TestFreshEventNotBlockedByRetry(t *testing.T) {
	r := newReceiver(1, http.StatusInternalServerError) // first attempt fails, then 200
	srv := r.server(t)
	clock := &stepClock{now: time.Now()}
	// A sleeper that parks until the test releases it: the backoff is
	// outstanding for as long as the test wants, without the clock
	// moving (the fakeSleep below advances instantly, which would fire
	// the retry before the fresh event could be queued).
	hold := make(chan struct{})
	sleepCalled := make(chan struct{}, 1)
	n := New(Config{
		Webhooks: []Webhook{{URL: srv.URL, Format: FormatJSON}},
		Now:      clock.Now,
		Sleep: func(ctx context.Context, d time.Duration) bool {
			select {
			case sleepCalled <- struct{}{}:
			default:
			}
			select {
			case <-hold:
				return true
			case <-ctx.Done():
				return false
			}
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n.Start(ctx)

	// First event fails and goes onto the retry schedule (2 s backoff);
	// the worker parks in the held sleeper.
	n.Enqueue(Event{Key: "first", Severity: Info, Title: "t"})
	waitUntil(t, "worker parked in backoff", func() bool {
		select {
		case <-sleepCalled:
			return true
		default:
			return false
		}
	})

	// A fresh event arrives while the backoff is outstanding. It must
	// be delivered without waiting for the retry clock to move.
	n.Enqueue(Event{Key: "second", Severity: Critical, Title: "t"})
	waitUntil(t, "fresh delivery during backoff", func() bool { return r.count() == 2 })
	if body := r.bodies[1]; !strings.Contains(body, "second") {
		t.Fatalf("second delivery = %q", body)
	}

	// When the backoff elapses (clock moves, sleeper released), the
	// first event's second attempt goes out too.
	clock.Advance(2 * time.Second)
	close(hold)
	waitUntil(t, "retried delivery", func() bool { return r.count() == 3 })
	if body := r.bodies[2]; !strings.Contains(body, "first") {
		t.Fatalf("retry delivery = %q", body)
	}
}

// TestSendTest: one info message to every webhook, in parallel; per-
// webhook results carry the index and redacted URL only.
func TestSendTest(t *testing.T) {
	ok := newReceiver(0, 500)
	bad := newReceiver(1<<30, http.StatusInternalServerError) // always fails
	okSrv, badSrv := ok.server(t), bad.server(t)
	secret := newReceiver(0, 500)
	secretSrv := secret.server(t)

	hooks := []Webhook{
		{URL: okSrv.URL, Format: FormatJSON},
		{URL: badSrv.URL, Format: FormatSlack},
		{URL: secretSrv.URL + "/tok?access=SECRET9", Format: FormatJSON},
	}
	results := SendTest(context.Background(), hooks, 5*time.Second, testLogger(t))
	if len(results) != 3 {
		t.Fatalf("results = %d", len(results))
	}
	if results[0].Err != nil {
		t.Fatalf("webhook 0 failed: %v", results[0].Err)
	}
	if results[1].Err == nil {
		t.Fatal("webhook 1 should have failed")
	}
	if results[2].Err != nil {
		t.Fatalf("webhook 2 failed: %v", results[2].Err)
	}
	if strings.Contains(results[1].Redacted, "SECRET9") || strings.Contains(results[1].Redacted, "/tok") {
		t.Fatalf("result leaked URL details: %q", results[1].Redacted)
	}
	// Every receiver got exactly one info event; json receivers carry
	// the key, the slack receiver carries it in the text.
	for i, r := range []*receiver{ok, bad, secret} {
		if r.count() != 1 {
			t.Fatalf("receiver %d got %d requests", i, r.count())
		}
		p := r.payload(t, 0)
		var title string
		if k, ok := p["key"]; ok {
			if k != "notify.test" {
				t.Fatalf("receiver %d payload = %v", i, p)
			}
			title, _ = p["title"].(string)
		} else if text, ok := p["text"].(string); ok {
			title, _, _ = strings.Cut(text, "\n")
		} else {
			t.Fatalf("receiver %d payload = %v", i, p)
		}
		if title != "spoond notify test" {
			t.Fatalf("receiver %d title = %q", i, title)
		}
	}
	// Errors are redacted end to end.
	if strings.Contains(results[1].Err.Error(), badSrv.URL) {
		t.Fatalf("error text leaked the URL: %q", results[1].Err)
	}
}
