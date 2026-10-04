package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Format selects a webhook payload shape. ntfy maps severity to its
// priority and tags headers; slack sends {"text": …} and works for
// Discord incoming webhooks too; json sends the event object itself.
type Format string

const (
	FormatNtfy  Format = "ntfy"
	FormatSlack Format = "slack"
	FormatJSON  Format = "json"
)

// ParseFormat validates a configured format name.
func ParseFormat(s string) (Format, error) {
	switch Format(strings.ToLower(strings.TrimSpace(s))) {
	case FormatNtfy:
		return FormatNtfy, nil
	case FormatSlack:
		return FormatSlack, nil
	case FormatJSON:
		return FormatJSON, nil
	case "":
		return "", fmt.Errorf("format is required (ntfy|slack|json)")
	default:
		return "", fmt.Errorf("unknown format %q (want ntfy|slack|json)", s)
	}
}

// minPriority is ntfy's numeric priority for a severity (5 = urgent
// max, 3 = default, 1 = min).
func minPriority(s Severity) int {
	switch s {
	case Critical:
		return 5
	case Warn:
		return 4
	default:
		return 3
	}
}

// minTags is ntfy's tag list for a severity.
func minTags(s Severity) string {
	switch s {
	case Critical:
		return "rotating_light"
	case Warn:
		return "warning"
	default:
		return "information_source"
	}
}

// Webhook is one operator-configured receiver.
type Webhook struct {
	URL         string            `json:"url"`
	Format      Format            `json:"format"`
	MinSeverity Severity          `json:"min_severity,omitempty"`
	Events      []string          `json:"events,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
}

// RedactedURL renders the webhook's URL safe for logs and errors:
// scheme://host only — the path may carry a topic token (ntfy), the
// query an access token, and userinfo a basic-auth credential; all
// three are stripped.
func (w Webhook) RedactedURL() string {
	u, err := url.Parse(w.URL)
	if err != nil {
		return "(unparseable)"
	}
	u.User = nil
	u.Path = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// matches reports whether this webhook wants ev: its severity passes
// the min_severity floor and, when an event key filter is configured,
// the key is listed. Keys may be globs ending in "*" (prefix match),
// so a webhook can take a whole family ("disk.*").
func (w Webhook) matches(ev Event) bool {
	if ev.Severity.rank() < w.MinSeverity.rank() {
		return false
	}
	if len(w.Events) == 0 {
		return true
	}
	for _, pat := range w.Events {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		if strings.HasSuffix(pat, "*") {
			if strings.HasPrefix(ev.Key, strings.TrimSuffix(pat, "*")) {
				return true
			}
			continue
		}
		if pat == ev.Key {
			return true
		}
	}
	return false
}

// webhook is a Notifier's runtime view of one configured receiver:
// its delivery queue and its redacted URL for logs.
type webhook struct {
	Webhook
	index    int
	redacted string
	ch       chan Event
}

// ParseWebhooks decodes NOTIFY_WEBHOOKS: a JSON list of
// {url, format, min_severity, events, headers} objects. An empty or
// unset value means no webhooks (the notifier stays off). Validation
// errors name the offending index, never the URL.
func ParseWebhooks(v string) ([]Webhook, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	var raw []Webhook
	if err := json.Unmarshal([]byte(v), &raw); err != nil {
		return nil, fmt.Errorf("NOTIFY_WEBHOOKS: not a JSON list of webhook objects: %w", err)
	}
	for i, w := range raw {
		if strings.TrimSpace(w.URL) == "" {
			return nil, fmt.Errorf("NOTIFY_WEBHOOKS[%d]: url is required", i)
		}
		u, err := url.Parse(w.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("NOTIFY_WEBHOOKS[%d]: url must be an http(s) URL", i)
		}
		f, err := ParseFormat(string(w.Format))
		if err != nil {
			return nil, fmt.Errorf("NOTIFY_WEBHOOKS[%d]: %v", i, err)
		}
		raw[i].Format = f
		switch raw[i].MinSeverity {
		case "":
			raw[i].MinSeverity = Info
		case Info, Warn, Critical:
		default:
			return nil, fmt.Errorf("NOTIFY_WEBHOOKS[%d]: min_severity must be info|warn|critical", i)
		}
	}
	return raw, nil
}

// rate is one webhook's sliding-window limiter: rateMax deliveries per
// rateWindow.
type rate struct {
	at []time.Time
}

// allow admits one delivery at now, or reports the wait until the
// oldest in-window delivery ages out.
func (r *rate) allow(now time.Time) (ok bool, wait time.Duration) {
	cut := now.Add(-rateWindow)
	kept := r.at[:0]
	for _, t := range r.at {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	r.at = kept
	if len(r.at) >= rateMax {
		return false, rateWindow - now.Sub(r.at[0])
	}
	r.at = append(r.at, now)
	return true, 0
}

// hookQueue is each webhook's delivery buffer.
const hookQueue = 64

// pending is a delivery on a webhook's retry schedule.
type pending struct {
	ev      Event
	next    time.Time // next attempt
	attempt int       // 1-based; the attempt about to run
}

// work is one webhook's delivery worker: it delivers what the
// dispatcher queues immediately (rate permitting) and owns the retry
// schedule for everything it accepts. Fresh events and retries share
// the worker, but not each other's waits: a pending backoff never
// delays a new event, and a busy queue never delays a due retry.
func (n *Notifier) work(ctx context.Context, h *webhook) {
	var (
		lim  rate
		retr []pending
	)
	fresh := func(ev Event) {
		// Reserve the rate slot when possible; a full window sends the
		// message to the retry track with the wait as its backoff — when
		// the window frees, the oldest slot has aged out and the
		// delivery goes out.
		if ok, wait := lim.allow(n.now()); !ok {
			retr = append(retr, pending{ev: ev, attempt: 1, next: n.now().Add(wait)})
			n.metric(h.index, string(ev.Severity), ResultRateLimited)
			return
		}
		n.attempt(ctx, h, pending{ev: ev, attempt: 1}, &retr)
	}
	// wake carries the retry timer's fire-up; capacity 1 — the worker
	// can only be waiting on one backoff at a time, and a fire-up that
	// arrives while it is busy with a fresh event must not block the
	// sleeper goroutine for long.
	wake := make(chan struct{}, 1) // the sleeper's fire-up, one at a time
	var (
		stopTimer func()    // cancels the parked wait
		armedFor  time.Time // what the parked wait is for
	)
	earliest := func() time.Time {
		next := retr[0].next
		for _, p := range retr[1:] {
			if p.next.Before(next) {
				next = p.next
			}
		}
		return next
	}
	// armTimer parks the wait for the earliest pending retry in a
	// helper goroutine (through n.sleep, the injectable clock), which
	// signals wake when it finishes. The worker itself never parks
	// inside a backoff: fresh events on h.ch are handled while the
	// backoff runs, so one failing webhook's schedule can never delay
	// another message's delivery. A wait superseded by an earlier
	// entry is cancelled and replaced.
	armTimer := func(next time.Time) {
		if stopTimer != nil {
			stopTimer()
		}
		d := next.Sub(n.now())
		if d < 0 {
			d = 0
		}
		armedFor = next
		wctx, cancel := context.WithCancel(ctx)
		stopTimer = cancel
		go func() {
			if n.sleep(wctx, d) {
				// The wait ran out (or was already over): poke the worker.
				// A poke already buffered from a superseded wait is
				// harmless — runDue acts only on entries whose time has
				// actually come.
				select {
				case wake <- struct{}{}:
				default:
				}
			}
		}()
	}
	for {
		// Re-arm whenever retries are pending and no wait is parked for
		// the earliest of them: after startup, after a wake, or when a
		// fresh event joined the schedule earlier than the parked wait.
		if len(retr) > 0 {
			if next := earliest(); stopTimer == nil || next.Before(armedFor) {
				armTimer(next)
			}
		}
		select {
		case <-ctx.Done():
			if stopTimer != nil {
				stopTimer()
			}
			return
		case ev, ok := <-h.ch:
			if !ok {
				if stopTimer != nil {
					stopTimer()
				}
				return
			}
			fresh(ev)
		case <-wake:
			// The wait for the earliest entry ran out. Clear the handle
			// so the loop re-arms for whatever is still pending, then
			// make one attempt for every entry whose time has come.
			stopTimer = nil
			n.runDue(ctx, h, &retr, &lim)
		}
	}
}

// runDue makes one attempt for every retry whose next time has passed,
// keeping still-pending entries in order. A rate limiter still full
// sends the message around again with the wait as its next time.
func (n *Notifier) runDue(ctx context.Context, h *webhook, retr *[]pending, lim *rate) {
	now := n.now()
	kept := (*retr)[:0]
	for _, p := range *retr {
		if p.next.After(now) {
			kept = append(kept, p)
			continue
		}
		// A rate slot is needed again; if the window is still full the
		// message waits for the next tick. attempt appends its follow-up
		// retry to kept (never to *retr): the final assignment below
		// would otherwise clobber it with the pre-iteration header.
		if ok, wait := lim.allow(now); !ok {
			p.next = now.Add(wait)
			kept = append(kept, p)
			continue
		}
		n.attempt(ctx, h, p, &kept)
	}
	*retr = kept
}

// attempt makes one delivery try for h. On failure the message moves
// onto h's retry schedule with exponential backoff until the backoff
// would exceed maxBackoff — then it is dropped and counted (and
// mirrored to the state file for the doctor).
func (n *Notifier) attempt(ctx context.Context, h *webhook, p pending, retr *[]pending) {
	if err := n.post(ctx, h, p.ev); err != nil {
		n.metric(h.index, string(p.ev.Severity), ResultRetry)
		// Backoff: 2s, 4s, 8s … capped at 1 h; an attempt whose backoff
		// would exceed the cap is not scheduled: the message is dropped
		// here, counted once.
		backoff := retryBase << (p.attempt - 1)
		if backoff > maxBackoff || backoff <= 0 {
			f := Failure{At: n.now().UTC(), Webhook: h.index, Error: redactErr(err)}
			n.log.Printf("notify: webhook %d (%s): giving up on %q after %d attempt(s): %s",
				h.index, h.redacted, p.ev.Key, p.attempt, f.Error)
			n.state.record(f, n.now())
			n.metric(h.index, string(p.ev.Severity), ResultDropped)
			return
		}
		n.log.Printf("notify: webhook %d (%s): %q attempt %d failed (%s); retry in %s",
			h.index, h.redacted, p.ev.Key, p.attempt, redactErr(err), backoff)
		*retr = append(*retr, pending{ev: p.ev, attempt: p.attempt + 1, next: n.now().Add(backoff)})
		return
	}
	n.log.Printf("notify: webhook %d (%s): delivered %q", h.index, h.redacted, p.ev.Key)
	n.metric(h.index, string(p.ev.Severity), ResultSent)
}

// redactErr keeps URLs (which may carry tokens in path or query) out
// of error strings before they reach logs or the state file. The
// client's *url.Error renders as `Post "<url>": <cause>`: only the cause
// is kept, whole — it names at most the host, which the redacted URL
// shows anyway, and cutting inside it (an x509 error has several
// colons) would leave nothing readable.
func redactErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	msg := err.Error()
	if m := quotedURLPrefix.FindStringIndex(msg); m != nil {
		return msg[m[1]:]
	}
	return msg
}

// quotedURLPrefix matches a url.Error-shaped prefix (`Post "…": `) in an
// error that was flattened to a string on the way.
var quotedURLPrefix = regexp.MustCompile(`^[A-Za-z]+ "[^"]*": `)

// post renders ev in the webhook's format and posts it. A 2xx is
// success; anything else is a retryable failure.
func (n *Notifier) post(ctx context.Context, h *webhook, ev Event) error {
	return postEvent(ctx, n.client, h.Webhook, ev)
}

// postEvent delivers one event to a webhook: render, post with the
// configured headers, 2xx is success. A redirect is refused (following
// one would replay configured headers — which may carry secrets — to
// whatever the redirect names); anything else non-2xx is retryable.
// Shared by the notifier and by SendTest (the CLI's one-shot sender).
func postEvent(ctx context.Context, client *http.Client, w Webhook, ev Event) error {
	body, contentType, err := render(w.Format, ev, w.URL)
	if err != nil {
		return err
	}
	target := w.URL
	if w.Format == FormatNtfy {
		target = ntfyPublishURL(w.URL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	for k, v := range w.Headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// SendTest posts one info message to every configured webhook, in
// parallel, and reports per-webhook results (indexed like the config,
// redacted URLs only). It is `spoond notify test`: a configuration
// check that needs no event and no waiting on retries. The context
// bounds each request; timeout leaves the default 10 s.
func SendTest(ctx context.Context, webhooks []Webhook, timeout time.Duration, logger *log.Logger) []TestResult {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	if timeout <= 0 {
		timeout = httpTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := &http.Client{Timeout: timeout, CheckRedirect: refuseRedirect}
	event := Event{
		Key:      "notify.test",
		Severity: Info,
		Title:    "spoond notify test",
		Body:     "If you can read this, this webhook works.",
		At:       time.Now().UTC(),
	}
	out := make([]TestResult, len(webhooks))
	var wg sync.WaitGroup
	for i, w := range webhooks {
		out[i] = TestResult{Webhook: i, Redacted: w.RedactedURL()}
		wg.Add(1)
		go func(i int, w Webhook) {
			defer wg.Done()
			err := postEvent(ctx, client, w, event)
			out[i].Err = redactErrf(err)
			logger.Printf("notify test: webhook %d (%s): %s", i, out[i].Redacted, testOutcome(out[i].Err))
		}(i, w)
	}
	wg.Wait()
	return out
}

// TestResult is one webhook's `spoond notify test` outcome: the
// webhook's index and redacted URL, and nil on success.
type TestResult struct {
	Webhook  int
	Redacted string
	Err      error
}

// testOutcome renders a test result for the log.
func testOutcome(err error) string {
	if err == nil {
		return "delivered"
	}
	return "failed: " + err.Error()
}

// redactErrf redacts a test-delivery error for the CLI output: URL
// fragments (path, query, userinfo) never belong in output.
func redactErrf(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s", redactErr(err))
}

// render builds the request body and content type for one format.
func render(f Format, ev Event, url string) (body []byte, contentType string, err error) {
	switch f {
	case FormatNtfy:
		// ntfy's JSON publishing: the body names the topic and goes to the
		// server's root URL (ntfyPublishURL); posted to the topic URL it
		// would arrive as a message whose text is this JSON. Severity maps
		// to the numeric priority and a tag; the title carries the marker
		// for resolved conditions. The topic is the URL path's last
		// segment; an empty topic is the server's problem, not a render
		// error.
		title := ev.Title
		if ev.Resolved {
			title = "Resolved: " + title
		}
		b := struct {
			Topic    string   `json:"topic"`
			Title    string   `json:"title"`
			Message  string   `json:"message"`
			Priority int      `json:"priority"`
			Tags     []string `json:"tags"`
		}{
			Topic:    ntfyTopic(url),
			Title:    title,
			Message:  ev.Body,
			Priority: minPriority(ev.Severity),
			Tags:     []string{minTags(ev.Severity)},
		}
		if b.Message == "" {
			b.Message = ev.Title
		}
		body, err = json.Marshal(b)
		return body, "application/json", err
	case FormatSlack:
		// Slack and Discord incoming webhooks both take {"text": …}.
		text := ev.Title
		if ev.Resolved {
			text = "Resolved: " + text
		}
		if ev.Body != "" {
			text += "\n" + ev.Body
		}
		body, err = json.Marshal(struct {
			Text string `json:"text"`
		}{text})
		return body, "application/json", err
	case FormatJSON:
		body, err = json.Marshal(ev)
		return body, "application/json", err
	}
	return nil, "", fmt.Errorf("unknown format %q", f)
}

// refuseRedirect is the http.Client CheckRedirect for webhook
// deliveries: a webhook endpoint has no business redirecting, and
// following one would replay the request — with its configured
// headers, which may carry secrets — to whatever the redirect names.
// The error surfaces as an ordinary retryable delivery failure.
func refuseRedirect(*http.Request, []*http.Request) error {
	return fmt.Errorf("webhook redirected: refusing to follow")
}

// ntfyPublishURL is where ntfy takes JSON publishes: the configured
// topic URL without its last path segment (the topic, which travels in
// the body), so a server under a path prefix (https://host/ntfy/topic)
// keeps its prefix. The query stays: ntfy accepts ?auth=… there.
func ntfyPublishURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	path := strings.TrimRight(u.Path, "/")
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		path = path[:i]
	}
	u.Path = path + "/"
	u.RawPath = ""
	return u.String()
}

// ntfyTopic is the URL path's last non-empty segment: ntfy addresses a
// topic as https://host/<topic>. An unparseable URL yields "".
func ntfyTopic(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	path := strings.Trim(u.Path, "/")
	if path == "" {
		return ""
	}
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		path = path[i+1:]
	}
	return path
}
