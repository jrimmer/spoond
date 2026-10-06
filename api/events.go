package api

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// Lease event stream (2.2, #115): every lease lifecycle change emits one
// event on an in-process bus. The bus stamps each event with a
// per-process monotonic sequence and a random per-start epoch, keeps the
// last leaseEventRingSize events in a ring buffer, and fans events out
// to in-process subscribers (the future webhook notifier) and to the
// Server-Sent Events streams in api/events_sse.go. Nothing on this path
// ever blocks a lease lifecycle change: a subscriber that cannot keep up
// has events dropped and is told so with a gap event.

// LeaseEventType names one lease lifecycle change.
type LeaseEventType string

const (
	LeaseCreated       LeaseEventType = "created"
	LeaseReleased      LeaseEventType = "released"
	LeaseSuspended     LeaseEventType = "suspended"
	LeaseResumed       LeaseEventType = "resumed"
	LeaseCheckpointed  LeaseEventType = "checkpointed"
	LeaseRecovered     LeaseEventType = "recovered"
	LeaseLost          LeaseEventType = "lost"
	LeaseRestarted     LeaseEventType = "restarted"
	LeaseHolderSet     LeaseEventType = "holder_set"
	LeaseHolderCleared LeaseEventType = "holder_cleared"
	LeaseHeldAction    LeaseEventType = "held_action"
	// LeaseCheckpointPolicy marks a per-lease checkpoint interval change
	// (2.3, #122): the detail carries the new effective seconds.
	LeaseCheckpointPolicy LeaseEventType = "checkpoint_policy"
	// LeaseRestored marks a restore in place to a kept checkpoint
	// (2.3, #121): the detail carries the restored-to build id.
	LeaseRestored LeaseEventType = "restored"
	// LeasePreempted marks a burst lease suspended to free hugepages for
	// a guaranteed admission (#128 part 3). Its generation does not
	// change: the pause/resume memory continues. The detail names the
	// guaranteed lease's owner.
	LeasePreempted LeaseEventType = "preempted"
	// LeaseStreamGap marks a hole in a stream rather than a lifecycle
	// change. It is synthesized when a consumer's position cannot be
	// honoured (the epoch changed after a restart, the pointed-at event
	// left the ring) or when a subscriber was too slow and events were
	// dropped. It never enters the ring and carries Seq 0.
	LeaseStreamGap LeaseEventType = "gap"
)

// LeaseEvent is one lease lifecycle change.
type LeaseEvent struct {
	Seq     uint64         `json:"seq"`
	Epoch   string         `json:"epoch"`
	At      time.Time      `json:"at"`
	LeaseID string         `json:"lease_id"`
	Owner   string         `json:"owner"`
	Type    LeaseEventType `json:"type"`
	Detail  string         `json:"detail,omitempty"`
}

const (
	// leaseEventRingSize is how many recent events the bus keeps for
	// SSE resume by Last-Event-ID.
	leaseEventRingSize = 10000
	// leaseEventSubBuffer is each subscriber's delivery buffer. A
	// subscriber that lets it fill has events dropped (announced with a
	// gap) instead of stalling the emitters.
	leaseEventSubBuffer = 256
)

// eventBus is the in-process lease event bus: one monotonic sequence, a
// random per-start epoch, the recent-event ring and the fan-out to
// subscribers. Its own mutex is independent of the store lock (events
// are emitted from code holding the store lock, never the other way
// round), so emitting never waits on anything but other emitters.
type eventBus struct {
	mu    sync.Mutex
	epoch string // random per backend start; immutable afterwards
	seq   uint64 // last assigned sequence number
	// ring holds the most recent events, oldest first; start is the
	// oldest entry's index once the ring is full.
	ring  []LeaseEvent
	start int
	subs  map[*eventSub]struct{}
}

func newEventBus() *eventBus {
	return &eventBus{
		epoch: newEventEpoch(),
		ring:  make([]LeaseEvent, 0, leaseEventRingSize),
		subs:  make(map[*eventSub]struct{}),
	}
}

// newEventEpoch returns a random id distinguishing one backend run from
// the next: a client that sees a different epoch in an event id knows
// the backend restarted and its remembered position is meaningless.
func newEventEpoch() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Epoch returns the bus's per-start epoch. It never changes within a
// process, so a client can tell a restart (and a sequence gap) from a
// resume.
func (b *eventBus) Epoch() string { return b.epoch }

// emit records one event: it takes the next sequence number, enters the
// ring and delivers the event to every matching subscriber. Delivery is
// non-blocking; a full subscriber channel counts as a drop and is
// announced to that subscriber with a gap event.
func (b *eventBus) emit(leaseID, owner string, typ LeaseEventType, detail string) LeaseEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	ev := LeaseEvent{
		Seq:     b.seq,
		Epoch:   b.epoch,
		At:      time.Now().UTC(),
		LeaseID: leaseID,
		Owner:   owner,
		Type:    typ,
		Detail:  detail,
	}
	if len(b.ring) < leaseEventRingSize {
		b.ring = append(b.ring, ev)
	} else {
		b.ring[b.start] = ev
		b.start = (b.start + 1) % leaseEventRingSize
	}
	for sub := range b.subs {
		if !sub.filter.matches(&ev) {
			continue
		}
		sub.deliver(ev, b.epoch)
	}
	return ev
}

// EventFilter selects the events a subscriber receives: zero fields
// match everything; Owner narrows to one owner's leases, LeaseID to one
// lease. Both set, both must match.
type EventFilter struct {
	Owner   string
	LeaseID string
}

func (f EventFilter) matches(ev *LeaseEvent) bool {
	if f.LeaseID != "" && ev.LeaseID != f.LeaseID {
		return false
	}
	if f.Owner != "" && ev.Owner != f.Owner {
		return false
	}
	return true
}

// eventSub is one registered consumer. The channel is the delivery
// path; dropped counts events shed because the channel was full since
// the last gap marker was queued.
type eventSub struct {
	filter  EventFilter
	ch      chan LeaseEvent
	dropped uint64
}

// send queues one event without blocking. A full buffer sheds its
// oldest buffered event (any marker included — a later marker reports
// the running total) and counts it as a drop, then retries.
func (sub *eventSub) send(ev LeaseEvent) {
	for {
		select {
		case sub.ch <- ev:
			return
		default:
		}
		select {
		case <-sub.ch: // discard the oldest buffered event
			sub.dropped++
		default:
			// Drained between the two selects: retry.
		}
	}
}

// deliver queues one event and, when events had to be shed to make
// room (now or earlier), announces the loss with a gap marker right
// behind the freshest events — so a slow subscriber is told even when
// the drops stopped before it started reading again. The marker carries
// no lease identity, so it is sent unfiltered (it reports this
// subscriber's own loss, not a lifecycle change to match against the
// filter).
func (sub *eventSub) deliver(ev LeaseEvent, epoch string) {
	sub.send(ev)
	if sub.dropped > 0 {
		sub.send(LeaseEvent{
			Epoch:  epoch,
			At:     time.Now().UTC(),
			Type:   LeaseStreamGap,
			Detail: fmt.Sprintf("at least %d event(s) dropped; slow subscriber", sub.dropped),
		})
		sub.dropped = 0
	}
}

// registerSub creates and registers a subscriber with f and a fresh
// delivery buffer. Call with b.mu held.
func (b *eventBus) registerSubLocked(f EventFilter) *eventSub {
	sub := &eventSub{filter: f, ch: make(chan LeaseEvent, leaseEventSubBuffer)}
	b.subs[sub] = struct{}{}
	return sub
}

// subscribe registers a subscriber for live events only.
func (b *eventBus) subscribe(f EventFilter) *eventSub {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.registerSubLocked(f)
}

// subscribeWithReplay registers a subscriber and, in the same critical
// section, snapshots the ring replay for a Last-Event-ID position: no
// event is lost or delivered twice between the snapshot and the start
// of the live flow. gapDetail is "" when the position is continuable
// and otherwise the reason for the gap event the consumer must see
// first (a foreign epoch, or a position that left the ring).
//
// at is the newest sequence assigned when the subscription was taken:
// every later event reaches the subscriber live, so it is the position
// a connect-time gap marker carries.
func (b *eventBus) subscribeWithReplay(f EventFilter, hasPos bool, epoch string, seq uint64) (sub *eventSub, replay []LeaseEvent, gapDetail string, at uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sub = b.registerSubLocked(f)
	if !hasPos {
		return sub, nil, "", b.seq
	}
	replay, gapDetail = b.replayAfterLocked(f, epoch, seq)
	return sub, replay, gapDetail, b.seq
}

// replayAfterLocked resolves a Last-Event-ID position against the ring.
// A matching epoch with the position still buffered resumes: every
// buffered event after it is returned. Anything else (foreign epoch,
// position past the newest assigned sequence, position older than the
// oldest buffered event) announces a gap instead — the caller must
// assume it missed everything in between. Call with b.mu held.
func (b *eventBus) replayAfterLocked(f EventFilter, epoch string, seq uint64) ([]LeaseEvent, string) {
	if epoch != b.epoch {
		return nil, fmt.Sprintf("epoch %q is not the current epoch; the backend restarted", epoch)
	}
	// Above the newest sequence ever assigned there is nothing to resume
	// from, and no way the client saw that id from this bus (a fabricated
	// or foreign id): gap. An empty ring with a fresh bus lands here too.
	if seq > b.seq {
		return nil, fmt.Sprintf("event %d is past this stream's newest (%d)", seq, b.seq)
	}
	n := len(b.ring)
	if n == 0 {
		return nil, ""
	}
	oldest := b.ring[0]
	if n == leaseEventRingSize {
		oldest = b.ring[b.start]
	}
	newest := b.ring[(b.start+n-1)%n]
	// A position above the newest event means the client was somewhere
	// this bus never was (a fabricated id, or a different backend with
	// the same epoch): a silent "no replay" would hide the mistake.
	if seq > newest.Seq {
		return nil, fmt.Sprintf("event %d is past this stream's newest (%d)", seq, newest.Seq)
	}
	if seq == newest.Seq {
		return nil, "" // already current; live events follow
	}
	// Resuming from p replays every event after p, so p is honourable
	// while the event right after it is still buffered.
	if seq+1 < oldest.Seq {
		return nil, fmt.Sprintf("event %d is no longer in the ring (oldest kept: %d)", seq, oldest.Seq)
	}
	var events []LeaseEvent
	for i := 0; i < n; i++ {
		ev := b.ring[(b.start+i)%n]
		if ev.Seq > seq && f.matches(&ev) {
			events = append(events, ev)
		}
	}
	return events, ""
}

// unsubscribe removes a subscriber and closes its channel. Idempotent.
// Events already buffered stay readable.
func (b *eventBus) unsubscribe(sub *eventSub) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subs[sub]; !ok {
		return
	}
	delete(b.subs, sub)
	close(sub.ch)
}

// EventSubscription is one in-process consumer of the lease event
// stream (Service.Subscribe). Events arrive on C; Close stops the flow.
type EventSubscription struct {
	C   <-chan LeaseEvent
	bus *eventBus
	sub *eventSub
}

// Close unsubscribes and closes C. Idempotent; events already buffered
// on C remain readable.
func (s *EventSubscription) Close() { s.bus.unsubscribe(s.sub) }

// Subscribe registers an in-process consumer of the lease event stream
// (the building block for the webhook notifier). Events matching f
// arrive on the returned channel; delivery never blocks the emitters —
// a subscriber that does not keep up has events dropped and is told so
// with a gap event (Seq 0) ahead of the next event that gets through.
func (s *Service) Subscribe(f EventFilter) *EventSubscription {
	sub := s.bus.subscribe(f)
	return &EventSubscription{C: sub.ch, bus: s.bus, sub: sub}
}

// emitLeaseEvent records one lifecycle change on the bus. Safe to call
// with s.store.mu held (the bus never takes the store lock); the SSE
// streams and in-process subscribers are fed from here.
func (s *Service) emitLeaseEvent(leaseID, owner string, typ LeaseEventType, detail string) {
	s.bus.emit(leaseID, owner, typ, detail)
}
