// Package notify delivers events that need a person to
// operator-configured webhooks (spoond 2.2, #117).
//
// Two things feed it: the lease event bus (a lease lost, a held-lease
// rule that acted) and a periodic pass, once a minute, over the
// conditions nobody should have to watch a dashboard for — a systemd
// unit that is not active, the snapshot disk or the hugepage pool past
// the dashboard's warn/danger levels, the TLS certificate within 30, 7
// or 1 day of expiry, a failed GC pass, a database backup older than
// its age limit. Every condition carries a stable Key: the same key is
// delivered at most once per hour, and when a warn/critical condition
// clears, its key is delivered once more with Resolved set — only for
// keys an alert actually went out for (the notifier keeps the open
// conditions), so a healthy system stays silent.
//
// Delivery is asynchronous: events are queued, matched against each
// webhook (format, minimum severity, optional event-key filter), paced
// by a per-webhook 30-per-hour rate limit, and retried with exponential
// backoff for up to an hour before the message is dropped and counted.
//
// Webhook URLs and headers may carry secrets and are never logged:
// logs, failure records and metrics name a webhook by its index in
// NOTIFY_WEBHOOKS and a redacted scheme://host form only.
package notify

import (
	"context"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Severity grades how much a person needs an event.
type Severity string

const (
	Info     Severity = "info"
	Warn     Severity = "warn"
	Critical Severity = "critical"
)

// rank orders severities for the min_severity filter.
func (s Severity) rank() int {
	switch s {
	case Warn:
		return 1
	case Critical:
		return 2
	default:
		return 0
	}
}

// Event is one thing that needs a person. Key is the condition's
// stable identity — the dedupe and the resolved flag work per key —
// and everything else is presentation.
type Event struct {
	Key      string    `json:"key"`
	Severity Severity  `json:"severity"`
	Title    string    `json:"title"`
	Body     string    `json:"body,omitempty"`
	Resolved bool      `json:"resolved,omitempty"`
	At       time.Time `json:"at"`
}

// Delivery outcomes, as counted in
// spoond_notifications_total{webhook,severity,result}.
const (
	ResultSent        = "sent"
	ResultRetry       = "retry"
	ResultDropped     = "dropped"
	ResultDeduped     = "deduped"
	ResultRateLimited = "rate_limited"
)

// Metrics counts delivery outcomes. The backend's BackendMetrics
// implements it over spoond_notifications_total; nil everywhere in the
// notifier means no counting.
type Metrics interface {
	// Notification counts one outcome. webhook is the webhook's index
	// in NOTIFY_WEBHOOKS (never its URL), or "-" for events dropped
	// before a webhook was chosen.
	Notification(webhook, severity, result string)
}

// Delivery, dedupe and check-loop constants.
const (
	// DefaultCheckEvery is the periodic pass over the registered
	// checks.
	DefaultCheckEvery = time.Minute
	// DefaultQueueSize is the enqueue buffer between emitters (the
	// lease event bus) and the dispatcher.
	DefaultQueueSize = 256
	// dedupeWindow: the same key (same resolved state) is delivered at
	// most once per hour.
	dedupeWindow = time.Hour
	// retryBase is the first backoff; it doubles per attempt.
	retryBase = 2 * time.Second
	// maxBackoff is the retry ceiling: an attempt whose backoff would
	// exceed it is not made and the message is dropped instead.
	maxBackoff = time.Hour
	// httpTimeout bounds one delivery attempt.
	httpTimeout = 10 * time.Second
	// rateMax deliveries per rateWindow per webhook.
	rateMax    = 30
	rateWindow = time.Hour
	// checkBudget bounds one pass over all checks.
	checkBudget = 30 * time.Second
	// failureHorizon is how long a dropped delivery stays reported
	// (mirrored to the state file for spoond doctor).
	failureHorizon = 24 * time.Hour
	// maxFailures caps the state file.
	maxFailures = 1000
)

// Check is one periodic condition. It is called once per pass with the
// pass's clock and returns the events for this pass: an event every
// pass while its condition holds (the notifier's dedupe paces repeats
// to one per hour) and a Resolved event when the check can see the
// condition cleared or superseded by a deeper threshold. The notifier
// delivers a Resolved event only for a key one of its alerts opened
// (see checkEvent): a check cannot know whether anybody was told, so
// resolved events for never-alerted keys are dropped and a healthy
// deployment sends nothing. A check that cannot evaluate its condition
// returns nil rather than guessing.
type Check func(ctx context.Context, now time.Time) []Event

// Failure is one webhook delivery that was given up on after retries.
type Failure struct {
	At      time.Time `json:"at"`
	Webhook int       `json:"webhook"`
	Error   string    `json:"error"`
}

// sleeper waits d or returns false when ctx ended first. Replaced in
// tests to exercise retry schedules without waiting.
type sleeper func(ctx context.Context, d time.Duration) bool

func defaultSleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Config configures a Notifier.
type Config struct {
	Webhooks []Webhook
	// CheckEvery is the periodic pass interval (default: a minute).
	CheckEvery time.Duration
	// QueueSize is the enqueue buffer (default 256).
	QueueSize int
	// StatePath mirrors dropped deliveries for spoond doctor. Empty
	// keeps state in memory only.
	StatePath string
	// Log receives delivery outcomes; webhook URLs and headers are
	// redacted before anything is logged. Nil discards.
	Log *log.Logger
	// Metrics counts outcomes. Nil counts nothing.
	Metrics Metrics
	// HTTPClient delivers; nil uses a client with a 10 s timeout.
	HTTPClient *http.Client
	// Now is the clock (tests). Nil uses time.Now.
	Now func() time.Time
	// Sleep waits out retry backoff (tests). Nil uses real timers.
	Sleep func(ctx context.Context, d time.Duration) bool
}

// Notifier fans events out to the configured webhooks. Create with
// New, register checks with AddCheck, then Start; Enqueue feeds it
// from event sources (the lease event bus). All methods are safe for
// concurrent use.
type Notifier struct {
	cfg    Config
	hooks  []*webhook
	queue  chan Event
	checks []Check
	client *http.Client
	state  *stateFile
	log    *log.Logger
	now    func() time.Time
	sleep  sleeper

	mu        sync.Mutex
	dedupe    map[string]time.Time // key|resolved -> last delivery
	open      map[string]Severity  // condition keys an alert went out for
	hugepages HugepageUsage
	stop      context.CancelFunc
}

// New builds a Notifier over cfg. The webhook list is already
// validated (ParseWebhooks); malformed checks simply return no events.
func New(cfg Config) *Notifier {
	n := &Notifier{
		cfg:    cfg,
		queue:  make(chan Event, maxInt(DefaultQueueSize, cfg.QueueSize)),
		dedupe: map[string]time.Time{},
		open:   map[string]Severity{},
		client: cfg.HTTPClient,
		state:  &stateFile{path: cfg.StatePath},
		log:    cfg.Log,
		now:    cfg.Now,
		sleep:  cfg.Sleep,
	}
	if n.cfg.CheckEvery <= 0 {
		n.cfg.CheckEvery = DefaultCheckEvery
	}
	if n.client == nil {
		n.client = &http.Client{Timeout: httpTimeout, CheckRedirect: refuseRedirect}
	}
	if n.log == nil {
		n.log = log.New(io.Discard, "", 0)
	}
	if n.now == nil {
		n.now = time.Now
	}
	if n.sleep == nil {
		n.sleep = defaultSleep
	}
	for i, w := range cfg.Webhooks {
		n.hooks = append(n.hooks, &webhook{
			Webhook:  w,
			index:    i,
			redacted: w.RedactedURL(),
			ch:       make(chan Event, hookQueue),
		})
	}
	return n
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// AddCheck registers one periodic condition. Call before Start.
func (n *Notifier) AddCheck(c Check) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.checks = append(n.checks, c)
}

// TestHook returns the notifier's test hook: nil-safe helpers tests use
// to drive check passes and observe the queue without waiting on the
// real clock. The notifier need not be started.
func (n *Notifier) TestHook() *TestHook { return &TestHook{n: n} }

// SetHugepages installs the hugepage probe after New (the substrate
// fetch needs the client, which the caller wires up). Nil keeps the
// hugepage checks off.
func (n *Notifier) SetHugepages(h HugepageUsage) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.hugepages = h
}

// Start begins the dispatcher, the per-webhook delivery workers and
// the check loop, all until ctx is cancelled or Stop is called.
func (n *Notifier) Start(ctx context.Context) {
	n.mu.Lock()
	if n.stop != nil {
		n.mu.Unlock()
		return
	}
	ctx, n.stop = context.WithCancel(ctx)
	n.mu.Unlock()
	for _, h := range n.hooks {
		go n.work(ctx, h)
	}
	go n.dispatchLoop(ctx)
	go n.checkLoop(ctx)
}

// Stop ends the loops started by Start. Deliveries mid-retry are
// abandoned.
func (n *Notifier) Stop() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stop != nil {
		n.stop()
		n.stop = nil
	}
}

// Enqueue accepts one event from a source (the lease event bus). It
// never blocks: a full queue drops the event, counted with the "-"
// webhook (no webhook was chosen).
func (n *Notifier) Enqueue(ev Event) {
	if ev.Key == "" {
		return
	}
	if ev.Severity == "" {
		ev.Severity = Info
	}
	if ev.At.IsZero() {
		ev.At = n.now().UTC()
	}
	select {
	case n.queue <- ev:
	default:
		n.metric(-1, string(ev.Severity), ResultDropped)
	}
}

// metric counts one outcome. The webhook label is the index as a
// string ("-" when no webhook was chosen) — never a URL.
func (n *Notifier) metric(webhook int, severity, result string) {
	if n.cfg.Metrics != nil {
		label := strconv.Itoa(webhook)
		if webhook < 0 {
			label = "-"
		}
		n.cfg.Metrics.Notification(label, severity, result)
	}
}

// dispatchLoop delivers queued events to the matching webhooks.
func (n *Notifier) dispatchLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-n.queue:
			n.dispatch(ev)
		}
	}
}

// dispatch admits one event past the dedupe and fans it out to every
// matching webhook's queue (non-blocking; a full webhook queue counts
// as a rate-limited drop for that webhook). An event no webhook wants
// is not an error: the filters did their job.
func (n *Notifier) dispatch(ev Event) {
	if !n.admit(ev) {
		return
	}
	for _, h := range n.hooks {
		if !h.matches(ev) {
			continue
		}
		select {
		case h.ch <- ev:
		default:
			// The webhook worker cannot keep up: treat it like a rate
			// limit — the event is not delivered, and the hour will
			// pace it back in if the condition still holds.
			n.metric(h.index, string(ev.Severity), ResultRateLimited)
		}
	}
}

// admit applies the per-key hourly dedupe: the same key is delivered
// at most once per hour. The alert and its resolution are tracked in
// separate windows — a condition that fires and clears within the hour
// notifies both times (an alert with no resolution would be a lie the
// reader can't tell from silence), while each side alone is still
// capped at one per hour.
func (n *Notifier) admit(ev Event) bool {
	k := ev.Key
	if ev.Resolved {
		k += "\x00resolved"
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.now()
	if last, ok := n.dedupe[k]; ok && now.Sub(last) < dedupeWindow {
		n.metric(-1, string(ev.Severity), ResultDeduped)
		return false
	}
	// Bound the map: sweep expired entries once it grows past the
	// capacity an hour of distinct keys could plausibly fill.
	if len(n.dedupe) >= 4096 {
		for k, t := range n.dedupe {
			if now.Sub(t) >= dedupeWindow {
				delete(n.dedupe, k)
			}
		}
	}
	n.dedupe[k] = now
	return true
}

// checkLoop runs the periodic pass until stopped.
func (n *Notifier) checkLoop(ctx context.Context) {
	t := time.NewTicker(n.cfg.CheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.runChecks(ctx)
		}
	}
}

// runChecks evaluates every registered check once and feeds what they
// return through checkEvent, in the checks' registration order. One
// check's panic or over-run cannot take out the pass: each runs under
// its own recover — logged, so a broken check is visible — and the
// whole pass is bounded by checkBudget.
func (n *Notifier) runChecks(ctx context.Context) {
	n.mu.Lock()
	checks := append([]Check(nil), n.checks...)
	hp := n.hugepages
	n.mu.Unlock()
	if hp != nil {
		// The hugepage check is appended last, after the registered ones.
		checks = append(checks, func(ctx context.Context, now time.Time) []Event {
			return hugepagesCheck(hp, now)
		})
	}
	if len(checks) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, checkBudget)
	defer cancel()
	for _, c := range checks {
		if ctx.Err() != nil {
			return
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					n.log.Printf("notify: check panicked (skipped this pass): %v", r)
				}
			}()
			for _, ev := range c(ctx, n.now()) {
				n.checkEvent(ev)
			}
		}()
	}
}

// checkEvent applies the open-condition state to one check event
// before enqueueing it. An alert marks its key open (the condition
// holds; the hourly dedupe paces the repeats). A Resolved event is
// delivered only for a key an earlier alert opened — checks emit
// resolved events from their own view of the world and cannot know
// whether a person was ever told, so without this gate every healthy
// pass would resolve every key it watches and the resolutions would
// eat the webhooks' rate limits. The key leaves the open set when its
// resolution is enqueued, whatever the dedupe then does with it: the
// clear was announced, and silence after that is honest.
func (n *Notifier) checkEvent(ev Event) {
	n.mu.Lock()
	if ev.Resolved {
		open, was := n.open[ev.Key]
		if !was {
			n.mu.Unlock()
			return // never alerted: silence, not a resolution
		}
		delete(n.open, ev.Key)
		if ev.Severity == "" {
			ev.Severity = open // carry the level the alert went out at
		}
	} else {
		n.open[ev.Key] = ev.Severity
	}
	n.mu.Unlock()
	n.Enqueue(ev)
}
