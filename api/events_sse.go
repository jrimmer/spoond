package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Server-Sent Events streams of the lease event bus (2.2, #115):
// GET /api/leases/events covers every event the caller may see, GET
// /api/leases/{id}/events one lease. Both ride the standard bearer
// auth; every event is checked again as it is written against the owner
// it was stamped with, so a stream only carries the caller's own leases
// (admins: all).
const (
	// sseHeartbeatEvery paces the comment ping that keeps intermediaries
	// from closing an idle stream.
	sseHeartbeatEvery = 15 * time.Second
	// sseWriteTimeout bounds one write: a client that stops reading is
	// dropped instead of pinning the handler goroutine.
	sseWriteTimeout = 30 * time.Second
)

// leaseEventID renders the SSE id of one event: "<epoch>-<seq>". The
// epoch lets a client detect a backend restart from the id alone.
func leaseEventID(ev *LeaseEvent) string {
	return ev.Epoch + "-" + strconv.FormatUint(ev.Seq, 10)
}

// parseLeaseEventID splits a Last-Event-ID back into epoch and seq.
// ok is false when the id does not have the exact "<epoch>-<seq>"
// shape: exactly one '-', a non-empty epoch and a decimal sequence
// (the ids the bus hands out; anything else is not a position this
// bus produced, so the caller must gap instead of resuming).
func parseLeaseEventID(id string) (epoch string, seq uint64, ok bool) {
	i := strings.IndexByte(id, '-')
	if i <= 0 || i == len(id)-1 || strings.Contains(id[i+1:], "-") {
		return "", 0, false
	}
	if strings.TrimSpace(id[:i]) == "" {
		return "", 0, false // a blank epoch is never this bus's
	}
	seq, err := strconv.ParseUint(id[i+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return id[:i], seq, true
}

// handleLeaseEvents streams the caller's lease events; admins see every
// lease's. Filter: ?lease_id=<id> narrows the stream to one lease (a
// caller who cannot see that lease gets the same 404 as the other
// lease routes). This is the events-only EVENTS_TOKEN's one route: the
// token is admitted at the top of the handler chain, on GET
// /api/leases/events only, and reaches this handler with the marker set
// — it sees every owner's events and no other route, spelling included.
func (s *Server) handleLeaseEvents(w http.ResponseWriter, r *http.Request) {
	if s.isEventsToken(r) {
		leaseID := r.URL.Query().Get("lease_id")
		if leaseID != "" && s.svc.lookupAny(leaseID) == nil {
			writeError(w, http.StatusNotFound, "lease not found")
			return
		}
		s.svc.streamEvents(w, r, EventFilter{LeaseID: leaseID}, true)
		return
	}
	owner := ownerFrom(r.Context())
	admin := isAdmin(r)
	leaseID := r.URL.Query().Get("lease_id")
	if leaseID != "" && s.svc.lookup(owner, leaseID) == nil {
		if !admin || s.svc.lookupAny(leaseID) == nil {
			writeError(w, http.StatusNotFound, "lease not found")
			return
		}
	}
	s.svc.streamEvents(w, r, EventFilter{Owner: owner, LeaseID: leaseID}, admin)
}

// handleLeaseEventsOne streams one lease's events. The owner (or an
// admin); anyone else gets the same 404 as the other lease routes —
// the events-only EVENTS_TOKEN included, whose single route is the
// all-events stream (authMiddleware refuses the token long before the
// mux dispatches here).
func (s *Server) handleLeaseEventsOne(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	owner := ownerFrom(r.Context())
	admin := isAdmin(r)
	if s.svc.lookup(owner, id) == nil && !(admin && s.svc.lookupAny(id) != nil) {
		writeError(w, http.StatusNotFound, "lease not found")
		return
	}
	s.svc.streamEvents(w, r, EventFilter{LeaseID: id}, admin)
}

// streamEvents runs one SSE stream. admin lifts the owner filter (an
// admin sees every lease's events).
func (s *Service) streamEvents(w http.ResponseWriter, r *http.Request, f EventFilter, admin bool) {
	if admin {
		f.Owner = ""
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	hasPos := false
	var posEpoch string
	var posSeq uint64
	gapDetail := ""
	if lid := r.Header.Get("Last-Event-ID"); lid != "" {
		if e, seq, okID := parseLeaseEventID(lid); okID {
			hasPos, posEpoch, posSeq = true, e, seq
		} else {
			// An unparseable id is a position the stream can never
			// honour: announce the same gap as an aged-out position
			// instead of silently starting live.
			gapDetail = fmt.Sprintf("Last-Event-ID %q is not a <epoch>-<seq> id this bus issued", lid)
		}
	}
	// The subscription is taken before the headers go out so events
	// emitted in between are buffered for the stream, not lost.
	sub, replay, gapDetail2, at := s.bus.subscribeWithReplay(f, hasPos, posEpoch, posSeq)
	defer s.bus.unsubscribe(sub)
	if gapDetail == "" {
		gapDetail = gapDetail2
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	deadline := func() { _ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout)) }
	deadline()
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 3000\n\n")
	// Go's server buffers until told otherwise: push the headers and
	// the retry hint out now, or the client waits for the first
	// heartbeat to see its stream open.
	flusher.Flush()

	// withID is false only for a slow subscriber's drop marker: it has
	// no position, so it carries no id line and the client keeps its
	// last real id.
	writeOne := func(ev *LeaseEvent, withID bool) bool {
		deadline()
		if withID {
			if _, err := fmt.Fprintf(w, "id: %s\n", leaseEventID(ev)); err != nil {
				return false
			}
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, marshalLeaseEvent(ev)); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	writeVisible := func(ev *LeaseEvent) bool {
		if !s.eventVisible(r, ev) {
			return true
		}
		return writeOne(ev, true)
	}

	for i := range replay {
		if !writeVisible(&replay[i]) {
			return
		}
	}
	// A position that could not be honoured (unknown epoch after a
	// restart, or one that aged out of the ring) is announced before the
	// live flow, so the consumer knows it missed events and can resync.
	if gapDetail != "" {
		// The gap carries the position the live flow starts after, so a
		// reconnect from it resumes instead of gapping again.
		gap := LeaseEvent{
			Seq:    at,
			Epoch:  s.bus.Epoch(),
			At:     time.Now().UTC(),
			Type:   LeaseStreamGap,
			Detail: gapDetail,
		}
		if !writeOne(&gap, true) {
			return
		}
	}

	heartbeat := time.NewTicker(sseHeartbeatEvery)
	defer heartbeat.Stop()
	clientGone := r.Context().Done()
	for {
		select {
		case <-clientGone:
			return
		case <-heartbeat.C:
			deadline()
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-sub.ch:
			if !ok {
				return
			}
			// Gap events synthesized for this subscriber are always
			// delivered; lifecycle events are access-re-checked at write
			// time (ownership can change mid-stream: releases, share
			// revocations, admin moves).
			if ev.Type != LeaseStreamGap && !s.eventVisible(r, &ev) {
				continue
			}
			if !writeOne(&ev, ev.Type != LeaseStreamGap) {
				return
			}
		}
	}
}

// eventVisible reports whether the caller may see this event: a caller
// is fed only events stamped with their own owner id (the bus stamps
// the lease's true owner at emit time), and an admin is fed everything.
// A caller admitted with the events-only EVENTS_TOKEN has no owner
// identity at all — it is on the route precisely to see every owner's
// events — so it passes too. The stream's subscription filter already
// narrows the flow; this check runs again per event at write time as a
// second gate — a released lease is gone from the store by the time its
// event is written, so the check deliberately reads the event's own
// owner field, which survives the release.
func (s *Service) eventVisible(r *http.Request, ev *LeaseEvent) bool {
	if ev.Type == LeaseStreamGap {
		return true
	}
	if r.Context().Value(ctxEventsToken{}) == true || isAdmin(r) {
		return true
	}
	return ev.Owner != "" && ev.Owner == ownerFrom(r.Context())
}

// marshalLeaseEvent renders one event as the SSE data line, fields in a
// stable order; detail and owner may be empty.
func marshalLeaseEvent(ev *LeaseEvent) string {
	b, _ := json.Marshal(struct {
		Seq     uint64         `json:"seq"`
		Epoch   string         `json:"epoch"`
		At      string         `json:"at"`
		LeaseID string         `json:"lease_id"`
		Owner   string         `json:"owner"`
		Type    LeaseEventType `json:"type"`
		Detail  string         `json:"detail"`
	}{ev.Seq, ev.Epoch, ev.At.Format(time.RFC3339Nano), ev.LeaseID, ev.Owner, ev.Type, ev.Detail})
	return string(b)
}
