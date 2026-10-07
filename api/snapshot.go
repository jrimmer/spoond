package api

import (
	"context"
	"sync/atomic"
	"time"
)

// Snapshot write pacing (spoond-t1s). Every call that makes the
// substrate write a memory snapshot — Pause (suspend, idle sweep,
// held-lease rules, preemption, drain, restart) and Checkpoint (on
// demand, periodic, clone, fork, keep) — goes through one process-wide
// limiter, so the node never sees a stack of snapshot writes at once.
// The 2026-10-06 incident on vm2 was exactly that: a burst of pauses
// saturated the host disk, the orchestrator's NBD server missed the
// guests' rootfs deadline and every guest on the stalled devices lost
// its root disk (permanent EIO).
//
// The width is SNAPSHOT_WRITE_CONCURRENCY (default 1; 0 = unlimited,
// the old behaviour). Waiting is bounded by the caller's context: an
// API caller that has waited longer than snapshotWaitLogThreshold sees
// only added latency, never a different answer.

// DefaultSnapshotWriteConcurrency is the width of the process-wide
// snapshot-write limiter when SNAPSHOT_WRITE_CONCURRENCY is unset: one
// snapshot write at a time.
const DefaultSnapshotWriteConcurrency = 1

// DefaultDrainSnapshotConcurrency is the drain's own limiter width when
// DRAIN_SNAPSHOT_CONCURRENCY is unset: two, so a planned orchestrator
// restart can pause a batch of live leases within the unit's drain
// window while still not stacking the whole node's snapshots at once.
const DefaultDrainSnapshotConcurrency = 2

// snapshotWaitLogThreshold is how long a write must wait behind the
// limiter before it is logged at info: a normally paced write waits
// less than this, and a longer wait is worth a line naming the backlog.
const snapshotWaitLogThreshold = 5 * time.Second

// snapshotLimiter is a counting semaphore over snapshot writes. A nil
// or zero-width slots channel disables it (unlimited), the pre-fix
// behaviour. inFlight counts the writes currently holding a slot, for
// the gauge and for the "behind N other" log line.
type snapshotLimiter struct {
	slots    chan struct{}
	inFlight atomic.Int64
	now      func() time.Time
	logf     func(format string, args ...any)
}

// newSnapshotLimiter builds a limiter of the given width. width 0 means
// unlimited (no slot channel).
func newSnapshotLimiter(width int, now func() time.Time, logf func(format string, args ...any)) *snapshotLimiter {
	l := &snapshotLimiter{now: now, logf: logf}
	if width > 0 {
		l.slots = make(chan struct{}, width)
	}
	return l
}

// acquire takes a slot, waiting until one is free or ctx is done. It
// returns how long the wait was and a release function the caller must
// call exactly once after the substrate write returns. When ctx is done
// first, the context's error is returned and nothing is leaked: the
// send either takes a slot or observes ctx.Done, never both.
//
// A write that waited at least snapshotWaitLogThreshold is logged once,
// naming how long it waited and how many other writes were in flight
// when it started waiting.
func (l *snapshotLimiter) acquire(ctx context.Context) (time.Duration, func(), error) {
	if l == nil {
		return 0, func() {}, nil
	}
	if l.slots == nil {
		// Unlimited: no pacing, but still count the write so the gauge
		// shows the real in-flight snapshot count (the pre-fix mode).
		l.inFlight.Add(1)
		return 0, func() { l.inFlight.Add(-1) }, nil
	}
	start := l.now()
	var waited time.Duration
	select {
	case l.slots <- struct{}{}:
		waited = l.now().Sub(start)
	default:
		// Contended: wait, but only as long as the caller's context.
		behind := l.inFlight.Load()
		select {
		case l.slots <- struct{}{}:
			waited = l.now().Sub(start)
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		}
		if waited >= snapshotWaitLogThreshold {
			l.logf("snapshot write waited %s behind %d other", waited.Round(time.Second), behind)
		}
	}
	l.inFlight.Add(1)
	return waited, func() {
		l.inFlight.Add(-1)
		<-l.slots
	}, nil
}

// busy reports whether every slot is taken right now: the background
// rules use it to stand down for a tick instead of queueing a batch
// behind a running write.
func (l *snapshotLimiter) busy() bool {
	if l == nil || l.slots == nil {
		return false
	}
	return len(l.slots) >= cap(l.slots)
}

// snapshotLimiters are the widths the Service uses: the default limiter
// for every non-drain snapshot write, and the wider drain limiter for
// the admin drain's pauses only.
type snapshotLimiters struct {
	def   *snapshotLimiter
	drain *snapshotLimiter
}

// snapshotAcquire takes a slot for a snapshot write. drain selects the
// drain limiter (only the admin drain sets it); otherwise the default
// limiter paces the write. The returned release must be called exactly
// once, after the substrate write returns.
func (s *Service) snapshotAcquire(ctx context.Context, drain bool) (func(), error) {
	lim := s.snapshotLimiters.def
	if drain && s.snapshotLimiters.drain != nil {
		lim = s.snapshotLimiters.drain
	}
	if lim == nil {
		return func() {}, nil
	}
	waited, release, err := lim.acquire(ctx)
	if err != nil {
		return nil, err
	}
	if s.metrics != nil {
		s.metrics.SnapshotWriteWait.Observe(waited.Seconds())
		s.metrics.SnapshotWritesInFlight.Set(float64(s.snapshotInFlight()))
	}
	return func() {
		release()
		if s.metrics != nil {
			s.metrics.SnapshotWritesInFlight.Set(float64(s.snapshotInFlight()))
		}
	}, nil
}

// snapshotInFlight is the number of snapshot writes in flight across both
// limiters, so the gauge is exact while a drain and ordinary writes overlap.
func (s *Service) snapshotInFlight() int64 {
	var n int64
	for _, l := range []*snapshotLimiter{s.snapshotLimiters.def, s.snapshotLimiters.drain} {
		if l != nil {
			n += l.inFlight.Load()
		}
	}
	return n
}

// snapshotBusy reports whether the default snapshot-write limiter has
// no free slot right now. The background rules read it to skip a lease
// this tick rather than queue one behind a running write.
func (s *Service) snapshotBusy() bool {
	return s.snapshotLimiters.def != nil && s.snapshotLimiters.def.busy()
}
