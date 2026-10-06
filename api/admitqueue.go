package api

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jrimmer/spoond/v2/substrate"
)

// Queued admission (#129 part 1). A create that cannot be admitted right
// now can wait for room instead of failing: the waiting create holds its
// HTTP request open, the queue lives in the backend process, and on each
// wake-up the queued creates are retried in fair-share order — the owner
// furthest under their guaranteed_mib first, then FIFO. The wait is
// bounded by MAX_ADMIT_WAIT_SECS (0 disables waiting).

// DefaultMaxAdmitWaitSecs is the queued-admission wait cap when
// MAX_ADMIT_WAIT_SECS is unset: a waiting create is admitted or refused
// within fifteen minutes (Honey's flight leases ask for wait: 900).
const DefaultMaxAdmitWaitSecs = 900

// admitQueueTick is how often the queue retries its tickets even when
// nothing signalled: capacity can free in ways that do not call wake
// (a sandbox dying, the node changing), and a bounded retry keeps the
// wait honest.
const admitQueueTick = 5 * time.Second

// errDraining is the refusal a queued create is answered with when the
// admin drain starts (U10): the node is intentionally being emptied, so
// waiting for room is pointless.
var errDraining = errors.New("draining")

// waitRefusal reports whether err is one a create may wait out (#129):
// substrate capacity (hugepages full and preemption could not help), the
// burst reserve, a failed preemption, and the memory-cap quota refusal.
// The lease-count cap, bad requests, auth and unknown images answer at
// once.
func waitRefusal(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, substrate.ErrCapacity) || errors.Is(err, errBurstReserve) || errors.Is(err, errPreemptCannot) {
		return true
	}
	return isMemoryQuotaRefusal(err)
}

// waitRefusalDetail is the refusal the `queued` event names, e.g.
// "memory cap" or "no burst capacity".
func waitRefusalDetail(err error) string {
	switch {
	case errors.Is(err, substrate.ErrCapacity):
		return "capacity"
	case errors.Is(err, errBurstReserve):
		return "no burst capacity"
	case errors.Is(err, errPreemptCannot):
		return "cannot preempt"
	case isMemoryQuotaRefusal(err):
		return "memory cap"
	default:
		return "capacity"
	}
}

// admissionTicket is one waiting create. The lease id is allocated when
// the create is queued (grantLease reuses it), so the `queued` and
// `created` events name the same lease.
type admissionTicket struct {
	ch chan admissionOutcome

	id    string
	owner string

	leaseReq leaseRequest
	// refusal is the error that queued the create, kept for the timeout
	// answer and the `queued` event's detail.
	refusal error

	queuedAt time.Time
	deadline time.Time
	seq      uint64
	done     bool
}

// admissionOutcome is what the waiting create's HTTP handler receives:
// either the granted lease, or the refusal to answer with.
type admissionOutcome struct {
	lease *Lease
	err   error
}

// admissionQueue holds the waiting creates and serialises their
// admissions. It lives on the Service, so it is per-backend and lost on
// restart (a waiting client sees its connection go and retries).
type admissionQueue struct {
	mu      sync.Mutex
	tickets []*admissionTicket
	seq     uint64
	// admitMu serialises admissions from the queue: one queued create is
	// tried at a time so two cannot both pass the same free hugepages.
	admitMu sync.Mutex
	// tick is the periodic retry interval (overridable in tests).
	tick time.Duration
	// capacityGen counts capacity freeing (releases and pauses), so a
	// pass can tell that room appeared while it ran.
	capacityGen atomic.Uint64
}

// maxAdmitWaitSecs is the effective wait cap: the configured value
// (main.go resolves MAX_ADMIT_WAIT_SECS, default 900). 0 disables
// waiting; a negative value reads as 0.
func (s *Service) maxAdmitWaitSecs() int {
	if s.cfg.MaxAdmitWaitSecs < 0 {
		return 0
	}
	return s.cfg.MaxAdmitWaitSecs
}

// admissionWait resolves a request's wait field into the effective wait:
// ok is false when the create must not wait (wait <= 0, or waiting is
// disabled). A wait above the cap is clamped to it.
func (s *Service) admissionWait(waitSecs int) (time.Duration, bool) {
	if waitSecs <= 0 {
		return 0, false
	}
	max := s.maxAdmitWaitSecs()
	if max <= 0 {
		return 0, false
	}
	wait := time.Duration(waitSecs) * time.Second
	if cap := time.Duration(max) * time.Second; wait > cap {
		wait = cap
	}
	return wait, true
}

// newAdmissionTicket builds and registers one waiting create, allocating
// its lease id.
func (s *Service) newAdmissionTicket(owner string, req leaseRequest, refusal error, wait time.Duration) *admissionTicket {
	s.admitQ.mu.Lock()
	s.admitQ.seq++
	now := s.now()
	t := &admissionTicket{
		ch:       make(chan admissionOutcome, 1),
		id:       newID(),
		owner:    owner,
		leaseReq: req,
		refusal:  refusal,
		queuedAt: now,
		deadline: now.Add(wait),
		seq:      s.admitQ.seq,
	}
	t.leaseReq.leaseID = t.id
	s.admitQ.tickets = append(s.admitQ.tickets, t)
	depth := len(s.admitQ.tickets)
	s.admitQ.mu.Unlock()

	pos, of := s.ticketPosition(t)
	s.bus.emit(t.id, owner, LeaseQueued, fmt.Sprintf("%s; position %d of %d", waitRefusalDetail(refusal), pos, of))
	if s.metrics != nil {
		s.metrics.LeasesQueued.Set(float64(depth))
	}
	return t
}

// queueDepth is the number of waiting tickets.
func (s *Service) queueDepth() int {
	s.admitQ.mu.Lock()
	defer s.admitQ.mu.Unlock()
	return len(s.admitQ.tickets)
}

// send delivers an outcome to a ticket at most once.
func (t *admissionTicket) send(o admissionOutcome) {
	select {
	case t.ch <- o:
	default:
	}
}

// finishTicket marks a ticket done under the queue lock and removes it,
// so a later wake-up pass never sees it again. It reports whether the
// caller won the race to finish it.
func (s *Service) finishTicket(t *admissionTicket) bool {
	s.admitQ.mu.Lock()
	defer s.admitQ.mu.Unlock()
	if t.done {
		return false
	}
	t.done = true
	kept := s.admitQ.tickets[:0]
	for _, x := range s.admitQ.tickets {
		if x != t {
			kept = append(kept, x)
		}
	}
	s.admitQ.tickets = kept
	if s.metrics != nil {
		s.metrics.LeasesQueued.Set(float64(len(s.admitQ.tickets)))
	}
	return true
}

// waitForAdmission serves the waiting half of a create: it retries the
// queue, then blocks until the ticket is admitted, times out, the client
// goes away or the backend drains. It returns the granted lease (already
// created through the normal admission path), or the refusal the caller
// should answer with, plus how long the create waited.
func (s *Service) waitForAdmission(ctx context.Context, t *admissionTicket) (*Lease, time.Duration, error) {
	timer := time.NewTimer(time.Until(t.deadline))
	defer timer.Stop()
	for {
		s.tryAdmitQueued(ctx)
		select {
		case o := <-t.ch:
			return o.lease, s.now().Sub(t.queuedAt), o.err
		case <-timer.C:
			if !s.finishTicket(t) {
				// Admitted on the wire just before the timer fired.
				o := <-t.ch
				return o.lease, s.now().Sub(t.queuedAt), o.err
			}
			if s.metrics != nil {
				s.metrics.AdmitTimeouts.Inc()
			}
			s.bus.emit(t.id, t.owner, LeaseTimedOut, "waited "+s.now().Sub(t.queuedAt).Round(time.Second).String())
			// The refusal the create would have got had it not waited.
			return nil, s.now().Sub(t.queuedAt), t.refusal
		case <-ctx.Done():
			if !s.finishTicket(t) {
				o := <-t.ch
				return o.lease, s.now().Sub(t.queuedAt), o.err
			}
			if s.metrics != nil {
				s.metrics.AdmitTimeouts.Inc()
			}
			s.bus.emit(t.id, t.owner, LeaseTimedOut, "client gone")
			return nil, s.now().Sub(t.queuedAt), ctx.Err()
		}
	}
}

// ticketDone reports whether a ticket has already finished.
func (s *Service) ticketDone(t *admissionTicket) bool {
	s.admitQ.mu.Lock()
	defer s.admitQ.mu.Unlock()
	return t.done
}

// tryAdmitQueued makes one pass over the queue in fair-share order,
// admitting every ticket that fits (a smaller one may pass a larger one
// that still does not). Admissions are serialised by admitMu.
func (s *Service) tryAdmitQueued(ctx context.Context) {
	s.admitQ.admitMu.Lock()
	defer s.admitQ.admitMu.Unlock()
	// Re-read the fair-share order before every attempt: a create queued
	// while this pass runs, or an owner whose headroom changed with an
	// admission, must be tried in its place, not after tickets ranked
	// below it that an older snapshot listed first. tried keeps the pass
	// finite (each ticket at most once).
	tried := map[*admissionTicket]bool{}
	gen := s.admitQ.capacityGen.Load()
	refused := false // a ticket was turned away in this pass
	restarts := 0
	for {
		// Room freed during the pass (a release or pause bumped the
		// generation) after a ticket was turned away: start over from
		// the top, so the higher-ranked ticket gets that room before
		// anyone ranked below it. Bounded, so churn cannot livelock.
		if refused && restarts < 3 {
			if g := s.admitQ.capacityGen.Load(); g != gen {
				gen, refused = g, false
				restarts++
				clear(tried)
			}
		}
		var t *admissionTicket
		for _, x := range s.orderedTickets() {
			if !tried[x] && !s.ticketDone(x) {
				t = x
				break
			}
		}
		if t == nil {
			return
		}
		tried[t] = true
		lease, err := s.grantQueued(ctx, t)
		if err != nil {
			if errors.Is(err, errDraining) {
				s.finishTicket(t)
				if s.metrics != nil {
					s.metrics.AdmitTimeouts.Inc()
				}
				s.bus.emit(t.id, t.owner, LeaseTimedOut, "draining")
				t.send(admissionOutcome{err: err})
				s.drainQueue()
				return
			}
			// Still no room (or another refusal): leave the ticket for
			// the next wake-up.
			refused = true
			continue
		}
		if !s.finishTicket(t) {
			// The ticket finished (timeout or client gone) after the lease
			// was created: release it rather than leak a lease nobody owns.
			s.release(context.Background(), lease)
			continue
		}
		if s.metrics != nil {
			s.metrics.AdmitWait.Observe(s.now().Sub(t.queuedAt).Seconds())
		}
		t.send(admissionOutcome{lease: lease})
	}
}

// grantQueued retries one queued create through the normal admission
// path, so classes, the burst reserve, quotas and preemption apply
// unchanged. It re-checks the drain first: a drain starting answers
// queued creates at once.
func (s *Service) grantQueued(ctx context.Context, t *admissionTicket) (*Lease, error) {
	if s.draining.Load() {
		return nil, fmt.Errorf("draining: %w", errDraining)
	}
	return s.grantLease(ctx, t.leaseReq)
}

// orderedTickets snapshots the waiting tickets in fair-share order: the
// owner furthest under its guaranteed_mib first, an owner with no
// headroom (no guaranteed_mib, or already at it) after every owner with
// headroom, then FIFO by queue sequence.
func (s *Service) orderedTickets() []*admissionTicket {
	s.admitQ.mu.Lock()
	list := append([]*admissionTicket(nil), s.admitQ.tickets...)
	s.admitQ.mu.Unlock()

	headroom := map[string]int{}
	has := map[string]bool{}
	for _, t := range list {
		if _, seen := headroom[t.owner]; seen {
			continue
		}
		h, ok := s.ownerHeadroom(t.owner)
		headroom[t.owner] = h
		has[t.owner] = ok && h > 0
	}
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		ah, bh := has[a.owner], has[b.owner]
		if ah != bh {
			return ah // an owner with headroom ahead of the rest
		}
		if ah && headroom[a.owner] != headroom[b.owner] {
			return headroom[a.owner] > headroom[b.owner]
		}
		return a.seq < b.seq
	})
	return list
}

// ownerHeadroom reports an owner's headroom under its guaranteed_mib:
// guaranteed − used, and whether the owner has a guarantee at all. Only
// headroom > 0 ranks an owner ahead of the no-guarantee crowd.
func (s *Service) ownerHeadroom(owner string) (int, bool) {
	if s.identities == nil {
		return 0, false
	}
	u := s.identities.UserByID(owner)
	if u == nil || u.GuaranteedMiB <= 0 {
		return 0, false
	}
	return u.GuaranteedMiB - s.usedMiB(owner), true
}

// drainQueue answers every waiting ticket with the drain refusal at
// once, called when the drain starts.
func (s *Service) drainQueue() {
	s.admitQ.mu.Lock()
	pending := s.admitQ.tickets
	s.admitQ.tickets = nil
	s.admitQ.mu.Unlock()
	for _, t := range pending {
		s.admitQ.mu.Lock()
		if t.done {
			s.admitQ.mu.Unlock()
			continue
		}
		t.done = true
		s.admitQ.mu.Unlock()
		if s.metrics != nil {
			s.metrics.AdmitTimeouts.Inc()
		}
		s.bus.emit(t.id, t.owner, LeaseTimedOut, "draining")
		t.send(admissionOutcome{err: fmt.Errorf("draining: %w", errDraining)})
	}
	if s.metrics != nil {
		s.metrics.LeasesQueued.Set(0)
	}
}

// wakeAdmissionQueue nudges a retry pass when tickets are waiting. It is
// non-blocking: the pass runs on its own goroutine (at most one at a
// time) so a release or a quota change never waits on a sandbox create.
func (s *Service) wakeAdmissionQueue() {
	if s.queueDepth() == 0 {
		return
	}
	if !s.wakeScheduled.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.wakeScheduled.Store(false)
		s.tryAdmitQueued(context.Background())
	}()
}

// runAdmitQueueLoop retries the queue on a fixed tick (capacity can free
// without a release/undrain signal) and stops with ctx.
func (s *Service) runAdmitQueueLoop(ctx context.Context) {
	tick := s.admitQ.tick
	if tick <= 0 {
		tick = admitQueueTick
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.queueDepth() == 0 {
				continue
			}
			s.tryAdmitQueued(ctx)
		}
	}
}

// ticketPosition is t's 1-based place in the current fair-share order
// and the queue's length; 0 when t is no longer queued.
func (s *Service) ticketPosition(t *admissionTicket) (int, int) {
	list := s.orderedTickets()
	for i, x := range list {
		if x == t {
			return i + 1, len(list)
		}
	}
	return 0, len(list)
}

// QueuedCreate is one waiting create as GET /api/leases/queue shows it.
type QueuedCreate struct {
	ID       string `json:"id"`
	Owner    string `json:"owner"`
	Image    string `json:"image"`
	Position int    `json:"position"`
	WaitedS  int    `json:"waited_s"`
	Reason   string `json:"reason"`
}

// queuedCreates lists the waiting creates in fair-share order, with
// each one's position in the whole queue; owner "" lists everyone's
// (admins), otherwise only that owner's.
func (s *Service) queuedCreates(owner string) []QueuedCreate {
	now := s.now()
	out := []QueuedCreate{}
	for i, t := range s.orderedTickets() {
		if owner != "" && t.owner != owner {
			continue
		}
		out = append(out, QueuedCreate{
			ID:       t.id,
			Owner:    t.owner,
			Image:    t.leaseReq.image,
			Position: i + 1,
			WaitedS:  int(now.Sub(t.queuedAt) / time.Second),
			Reason:   waitRefusalDetail(t.refusal),
		})
	}
	return out
}
