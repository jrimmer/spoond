// Webhook notification wiring (2.2, #117): the lease event bus's
// person-relevant events are forwarded to the notifier configured by
// the process (SetNotifier), and the GC records its outcome for the
// notify checks. Everything else on the bus is lifecycle churn that
// the SSE event stream serves; only a lost lease and a held-lease
// rule action need a person.
package api

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/jrimmer/spoond/v2/notify"
)

// SetNotifier installs the webhook notifier. Call before Start: the
// notify loop is only started when a notifier is set.
func (s *Service) SetNotifier(n NotifySink) {
	s.notifier = n
}

// NotifySink is what the service feeds events that need a person
// into. notify.Notifier implements it; tests use their own sink.
type NotifySink interface {
	Enqueue(ev notify.Event)
}

// runNotifyLoop forwards lease events to the notifier until ctx ends:
// a lease lost (its sandbox died in a substrate crash, or a recovery
// failed) is critical; a held-lease rule action is warn — critical
// when the action released the lease, taking the holder's work with
// it. Each event's key carries the lease id (and rule), so a
// flapping condition is at most hourly per condition and lease.
func (s *Service) runNotifyLoop(ctx context.Context) {
	if s.notifier == nil {
		return
	}
	sub := s.Subscribe(EventFilter{})
	defer sub.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.C:
			if !ok {
				return
			}
			s.notifyLeaseEvent(ev)
		}
	}
}

// notifyLeaseEvent maps one bus event onto its notifier event, or
// drops it (lifecycle churn, stream gaps).
func (s *Service) notifyLeaseEvent(ev LeaseEvent) {
	switch ev.Type {
	case LeaseLost:
		s.notifier.Enqueue(notify.Event{
			Key:      "lease.lost." + ev.LeaseID,
			Severity: notify.Critical,
			Title:    "Lease " + shortID(ev.LeaseID) + " lost",
			Body: joinBody([]string{
				"lease " + ev.LeaseID + " (owner " + ev.Owner + ")",
				ev.Detail,
			}),
			At: ev.At,
		})
	case LeaseHeldAction:
		rule, action := parseHeldDetail(ev.Detail)
		severity := notify.Warn
		if action == "release" {
			severity = notify.Critical
		}
		s.notifier.Enqueue(notify.Event{
			Key:      "held." + rule + "." + ev.LeaseID,
			Severity: severity,
			Title:    "Held lease " + shortID(ev.LeaseID) + ": " + rule + "/" + action,
			Body: joinBody([]string{
				"lease " + ev.LeaseID + " (owner " + ev.Owner + ")",
				ev.Detail,
			}),
			At: ev.At,
		})
	default:
		// created/released/suspended/… and gap markers: not for a person.
	}
}

// parseHeldDetail splits a held_action event's "rule/action: numbers"
// detail. An unparseable detail still yields a stable key.
func parseHeldDetail(detail string) (rule, action string) {
	head := detail
	if i := strings.IndexByte(head, ':'); i >= 0 {
		head = head[:i]
	}
	rule, action, ok := strings.Cut(head, "/")
	if !ok || rule == "" || action == "" {
		return "held", "action"
	}
	return rule, action
}

// shortID renders a lease id for titles: first segment, shortened.
// Cutting is rune-safe: a split multi-byte rune would garble titles.
func shortID(id string) string {
	const max = 12
	if len(id) <= max {
		return id
	}
	cut := id[:max]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	if len(cut) < len(id) {
		cut += "…"
	}
	return cut
}

func joinBody(lines []string) string {
	out := ""
	for _, l := range lines {
		if l == "" {
			continue
		}
		if out != "" {
			out += "\n"
		}
		out += l
	}
	return out
}

// recordGCOutcome remembers one GC pass's result for the notify
// checks (gc.failed). Never called for a drain-skipped pass: a pass
// that did not run is not a failure.
func (s *Service) recordGCOutcome(err error) {
	if s.gcErr == nil {
		return
	}
	s.gcErr.Set(err)
}

// GCLastError reports the most recent snapshot GC pass's error (nil
// before the first pass and after a good one) — the source of the
// notifier's gc.failed check.
func (s *Service) GCLastError() func() error {
	if s.gcErr == nil {
		return func() error { return nil }
	}
	return s.gcErr.Last
}
