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
	"time"
	"unicode/utf8"

	"github.com/jrimmer/spoond/v2/notify"
)

// SetNotifier installs the webhook notifier. Call before Start: the
// notify loop is only started when a notifier is set.
func (s *Service) SetNotifier(n NotifySink) {
	s.notifier = n
}

// Draining reports the admin drain state for the notifier's
// node.draining check (spoond-52c H3/S3): whether a drain is in effect,
// how long it has lasted (from drainStartedAt), and whether the last
// cached NodeInfo says the node is healthy. The check warns only while
// a healthy node's drain is old enough (half DRAIN_MAX_SECS) or the
// node is unhealthy, so a planned restart under a minute stays silent.
func (s *Service) Draining() notify.DrainState {
	st := notify.DrainState{Draining: s.draining.Load()}
	if !st.Draining {
		return st
	}
	if started := s.drainStartedAt.Load(); started != 0 {
		st.For = s.now().Sub(time.Unix(0, started))
	}
	st.NodeHealthy = s.nodeHealthyCached()
	return st
}

// nodeHealthyCached reports whether the last NodeInfo sample said the
// node was healthy, treating 'draining' as reachable (it is doing what it
// was told) and a stale or absent sample as unknown rather than
// unhealthy. Only a sample naming some other status counts as unhealthy,
// so a planned drain's brief alert depends on age, not a cache that has
// not been refreshed yet (spoond-52c S3).
func (s *Service) nodeHealthyCached() bool {
	s.nodeInfoMu.Lock()
	info, at := s.nodeInfoCache, s.nodeInfoAt
	s.nodeInfoMu.Unlock()
	if at.IsZero() || s.now().Sub(at) >= nodeInfoCacheTTL {
		return true
	}
	return info.Status == "healthy" || info.Status == "draining"
}

// DrainWarnAfter is the age past which node.draining warns while the
// node is healthy: half the effective DRAIN_MAX_SECS, so a planned
// restart's brief drain stays silent but a drain approaching its
// self-heal limit is announced (spoond-52c S3).
func (s *Service) DrainWarnAfter() time.Duration {
	max := s.drainMaxSecs()
	if max <= 0 {
		return 0
	}
	return time.Duration(max) * time.Second / 2
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

// KeptDiskProbe adapts the service's kept accounting (#126) into the
// notifier's KeptDisk source: the recorded size_bytes over kept builds
// of live leases, and the snapshot disk's total size from the same
// statfs the held-lease disk rules read. An unreadable disk reports an
// error, and the disk.kept check stays silent for the pass.
func (s *Service) KeptDiskProbe(diskPath string) func() (uint64, uint64, error) {
	return func() (uint64, uint64, error) {
		_, bytes, err := s.keptPins(context.Background())
		if err != nil {
			return 0, 0, err
		}
		total, _, err := s.diskCapacity(diskPath)
		if err != nil {
			return 0, 0, err
		}
		return uint64(bytes), total, nil
	}
}
