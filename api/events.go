package api

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
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
	// LeasePinned and LeaseUnpinned mark a pin change (FS5, owner
	// decision 2026-10-09): a pin protects only a running VM — spoond
	// never pauses or releases a running pinned lease before its own
	// expiry, and a paused pinned lease is released 30 d after its pause
	// date like every paused lease. A holder or holder_url label never
	// pins.
	LeasePinned   LeaseEventType = "pinned"
	LeaseUnpinned LeaseEventType = "unpinned"
	// LeasePausedExpiring marks the warning 24 h before a paused lease is
	// released by the one clock (FS5).
	LeasePausedExpiring LeaseEventType = "paused_expiring"
	// LeasePinnedIdle marks a pinned lease crossing the
	// PINNED_IDLE_NOTICE_DAYS threshold (FS5, visibility only): nothing is
	// paused, unpinned or released because of it.
	LeasePinnedIdle LeaseEventType = "pinned_idle"
	// LeaseAdminUnpin marks the admin route that unpins leases by holder
	// prefix for the 2.9→3.0 migration window. It carries lease id "-"
	// (a lease-less placeholder).
	LeaseAdminUnpin LeaseEventType = "admin_unpin"
	// LeaseCheckpointPolicy marks a per-lease checkpoint interval change
	// (2.3, #122): the detail carries the new effective seconds.
	LeaseCheckpointPolicy LeaseEventType = "checkpoint_policy"
	// LeaseIdlePolicy marks a per-lease idle_suspend change (2.5, #129
	// part 2): the detail carries the new effective seconds.
	LeaseIdlePolicy LeaseEventType = "idle_policy"
	// LeaseIdleSuspended marks a lease suspended by the idle sweep (2.5,
	// #129 part 2): the detail carries how long it had been idle. The
	// lease's generation does not change: the pause/resume memory
	// continues, and the next call resumes it.
	LeaseIdleSuspended LeaseEventType = "idle_suspended"
	// LeaseRestored marks a restore in place to a kept checkpoint
	// (2.3, #121): the detail carries the restored-to build id.
	LeaseRestored LeaseEventType = "restored"
	// LeaseCrashTest marks a crash test (owner or admin): a reader
	// of the stream can tell a simulated crash from a real one. It
	// is emitted before the recovery events (recovered/lost) that follow
	// the same call.
	LeaseCrashTest LeaseEventType = "crash_test"
	// LeasePreempted marks a burst lease suspended to free hugepages for
	// a guaranteed admission (#128 part 3). Its generation does not
	// change: the pause/resume memory continues. The detail names the
	// guaranteed lease's owner.
	LeasePreempted LeaseEventType = "preempted"
	// LeasePromoted marks a running burst lease moved to guaranteed
	// because its owner's guarantee has room again (#128): bookkeeping
	// only, the VM is untouched.
	LeasePromoted LeaseEventType = "promoted"
	// LeaseRetry marks a transient crash-recovery failure that keeps the
	// lease recovering for another pass (spoond-dxq): the detail names the
	// attempt and the cause, so an owner sees the retry rather than a
	// silent wait.
	LeaseRetry LeaseEventType = "recovery_retry"
	// LeaseRootfsDead marks a lease whose root disk answered I/O errors
	// (the rootfs liveness probe): the sandbox is treated as crashed and
	// the shared recovery runs. It is not a `lost` event — the recovery
	// may still recover the lease or leave it retrying (spoond-dxq).
	LeaseRootfsDead LeaseEventType = "rootfs_dead"
	// LeaseJobStarted, LeaseJobExited and LeaseJobLost mark a background
	// exec job's life (2.6, #135): started carries the command (cut to
	// 120 chars), exited the exit code and a short stderr tail, lost a
	// job whose guest memory did not continue.
	LeaseJobStarted LeaseEventType = "job_started"
	LeaseJobExited  LeaseEventType = "job_exited"
	LeaseJobLost    LeaseEventType = "job_lost"
	// LeaseStreamGap marks a hole in a stream rather than a lifecycle
	// change. It is synthesized when a consumer's position cannot be
	// honoured (the epoch changed after a restart, the pointed-at event
	// left the ring) or when a subscriber was too slow and events were
	// dropped. It never enters the ring and carries Seq 0.
	LeaseStreamGap LeaseEventType = "gap"
	// LeaseQueued marks a create that was refused but is waiting for
	// admission (#129 part 1). The detail names the refusal it is
	// waiting out (e.g. "memory cap" or "no burst capacity"). The lease
	// id is already allocated, and the created lease keeps it.
	LeaseQueued LeaseEventType = "queued"
	// LeaseTimedOut marks a waiting create that gave up: its wait
	// elapsed (detail "waited Ns"), the client went away ("client
	// gone") or a drain started ("draining").
	LeaseTimedOut LeaseEventType = "timed_out"
	// LeaseGC marks a catalog GC pass that deleted builds or failed a
	// stale building row (2.5, #132 part 2). It is spoond's own
	// maintenance, not a lease's lifecycle: the lease id and owner are
	// empty, so only the all-leases stream (and the events-only token)
	// carries it, never a per-lease one. The detail names the count and
	// the freed bytes for a deletion, or the failed build for the stale
	// sweep.
	LeaseGC LeaseEventType = "gc"
	// LeaseSnapshotSaved marks a lease saved as a named snapshot (2.7,
	// #83): the detail names the name@version, the size and how long the
	// checkpoint took.
	LeaseSnapshotSaved LeaseEventType = "snapshot_saved"
	// LeaseDrainDeferred marks an undrain (or the drain self-heal loop)
	// that could not resume a drained lease yet: an admission refusal, a
	// capacity answer or a bounded context. The lease stays drained and
	// is retried; the detail names the refusal.
	LeaseDrainDeferred LeaseEventType = "drain_deferred"
	// LeaseDrainHealed marks the drain self-heal loop clearing a drain
	// that outlived DRAIN_MAX_SECS while the node was healthy. It is
	// spoond's own maintenance, not a lease's, so it carries no lease id
	// and no owner; the detail names how long the drain lasted.
	LeaseDrainHealed LeaseEventType = "drain_healed"
	// LeaseDrainFailed marks a drain that could not pause a lease: the
	// lease ran on into the orchestrator stop. The detail names the
	// pause error (spoond-52c R2).
	LeaseDrainFailed LeaseEventType = "drain_failed"
	// LeaseBoxFull marks a request that needed room but could not be
	// admitted because every take-back candidate was pinned (FS5).
	// Nothing was paused or released. It carries no lease id; the detail
	// names the request's memory and owner.
	LeaseBoxFull LeaseEventType = "box_full"
	// LeaseDrainGaveUp marks the drain self-heal loop giving up on a
	// drained lease whose resume stayed deferred past DRAIN_RESUME_MAX_AGE.
	// The lease is left suspended (not lost: its snapshot is intact) for
	// the owner or the one paused-release clock to exit (spoond-52c B2).
	LeaseDrainGaveUp LeaseEventType = "drain_gave_up"
	// LeaseUserDeleted marks the cleanup that follows DELETE
	// /api/users/{id} (spoond-q4j): every lease of the removed identity
	// was released (each with its own `released` event and reason
	// `user_deleted`), its named snapshots were dropped, its kept builds
	// unpinned and its running jobs cancelled. The detail summarises the
	// counts; the event carries the removed owner and no lease id.
	LeaseUserDeleted LeaseEventType = "user_deleted"
	// LeaseCriticalRelease marks a paused lease released by disk
	// take-back (FS3a): a request's snapshot write did not fit, and this
	// lease was the biggest disk borrower's oldest unpinned paused one.
	// The release itself emits the ordinary `released` event with reason
	// disk_reclaim; this event announces the take-back first with the
	// owner's ratio and the disk's free percentage in the detail.
	LeaseCriticalRelease LeaseEventType = "critical_release"
	// LeaseDiskCleanup marks one disk take-back pass (FS3a): the detail
	// carries the bytes freed by the garbage pass and by reclaimed
	// paused leases. Spoond's own maintenance, not a lease's: it carries
	// no lease id and no owner.
	LeaseDiskCleanup LeaseEventType = "disk_cleanup"
)

// LeaseEvent is one lease lifecycle change. Reason, PolicyStep and
// BuildID carry the structured fields of a suspension (#145 D6): a
// `suspended` event names why it happened, the pressure order's step
// ("" until that order names steps) and the pause build it wrote. They
// are empty on every other event type and on a hand or drain suspend,
// which has no automatic reason.
type LeaseEvent struct {
	Seq     uint64         `json:"seq"`
	Epoch   string         `json:"epoch"`
	At      time.Time      `json:"at"`
	LeaseID string         `json:"lease_id"`
	Owner   string         `json:"owner"`
	Type    LeaseEventType `json:"type"`
	Detail  string         `json:"detail,omitempty"`
	// Reason is idle_suspend|preempt|resume_failed on a suspended
	// event (resume_failed when an undrain left a lease suspended it
	// could not resume); "" for a hand or drain suspend.
	Reason string `json:"reason,omitempty"`
	// PolicyStep is the pressure order's step name that ordered the
	// suspend, or "" when none did.
	PolicyStep string `json:"policy_step,omitempty"`
	// BuildID is the pause build a suspended event wrote.
	BuildID string `json:"build_id,omitempty"`
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
	return b.emitStructured(leaseID, owner, typ, detail, "", "", "")
}

// emitStructured is emit with the structured suspension fields a
// `suspended` event carries (#145 D6).
func (b *eventBus) emitStructured(leaseID, owner string, typ LeaseEventType, detail, reason, policyStep, buildID string) LeaseEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	ev := LeaseEvent{
		Seq:        b.seq,
		Epoch:      b.epoch,
		At:         time.Now().UTC(),
		LeaseID:    leaseID,
		Owner:      owner,
		Type:       typ,
		Detail:     detail,
		Reason:     reason,
		PolicyStep: policyStep,
		BuildID:    buildID,
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
// streams and in-process subscribers are fed from here. A lease-less
// event (box_full, admin_unpin) passes lease id "-", a placeholder so
// the SSE JSON's lease_id is always well-formed and a per-lease filter
// never matches it.
func (s *Service) emitLeaseEvent(leaseID, owner string, typ LeaseEventType, detail string) {
	s.bus.emit(leaseID, owner, typ, detail)
}

// emitSuspendEvent records a `suspended` event with its structured
// fields (#145 D6): reason names why an automatic suspend happened, and
// policyStep the pressure order's step. The event carries build_id too
// and the human detail keeps the existing "paused into build <id>"
// text. For a hand or drain suspend reason is empty: the structured
// fields are all left off, so the event matches the lease, which omits
// its suspension facts for a suspension with no automatic reason.
func (s *Service) emitSuspendEvent(leaseID, owner, buildID, reason, policyStep string) {
	evBuild := ""
	if reason != "" {
		evBuild = buildID
	}
	s.bus.emitStructured(leaseID, owner, LeaseSuspended, "paused into build "+buildID, reason, policyStep, evBuild)
}

// emitGCEvent records one catalog GC maintenance event: a pass that
// deleted builds or failed a stale building row (2.5, #132 part 2). It
// is spoond's own maintenance, not a lease's: the event carries no lease
// id and no owner, so a per-lease subscription never receives it while
// the all-leases stream (and the events-only token) does.
func (s *Service) emitGCEvent(detail string) {
	s.bus.emit("", "", LeaseGC, detail)
}

// Lease-event detail formats (2.5, #132 part 2). The units stay in the
// event text rather than a machine field: the dashboard draws the detail
// verbatim and an operator reads it, so "61 ms" beats "0.061".

// eventDuration renders a duration for an event detail: sub-second in
// whole milliseconds, else a decimal with up to three places for the
// short ones and a whole-second form for the long ones. A zero or
// negative duration (clock skew) reads as 0 ms.
func eventDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Second {
		return strconv.FormatInt(d.Milliseconds(), 10) + " ms"
	}
	if d < time.Minute {
		return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + " s"
	}
	return d.Round(time.Second).String()
}

// shortEventBuildID is a build id as an event detail names it: the
// first 8 characters, with an ellipsis when there was more. It is not
// rune-safe on purpose — build ids are UUID hex.
func shortEventBuildID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8] + "…"
}

// formatEventBytes renders a byte count for a GC detail: GiB with two
// decimals once it is at least one GiB, else MiB with one, else whole
// KiB (a freed build is never 0 here — the GC emits only when it
// deleted something).
func formatEventBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return strconv.FormatFloat(float64(n)/(1<<30), 'f', 2, 64) + " GiB"
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MiB"
	default:
		return strconv.FormatInt(n>>10, 10) + " KiB"
	}
}
