package api

// File watch over Server-Sent Events (GET
// /api/leases/{id}/files/{path...}?watch=1): the guest-side change
// events of one directory as an SSE stream, backed by the substrate's
// Watch (envd's filesystem.WatchDir). Change paths that arrive within
// the coalescing window land as one event. The route rides the files
// model (owner or admin; a share grant does not carry file content)
// with three deliberate differences to the other file routes:
//
//   - A watch holds an exec/stream slot (acquireBusy) for its lifetime,
//     like a /stream attachment: it is a live, connected thing.
//   - A watch is NOT activity: it never calls touch, so an idle lease
//     with an open watch is still idle-suspended on schedule — and the
//     suspension then ends the watch (next point).
//   - When the lease suspends, is lost or is released, the stream
//     carries one {"kind":"error","code":...} event — lease_suspended,
//     lease_lost, lease_gone — and ends. A client re-watches after
//     restoring or resuming the lease.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/jrimmer/spoond/v3/substrate"
)

// watchPace holds the stream's timings, one place so tests can shrink
// them: coalesce (change paths arriving within one window land as a
// single SSE event), heartbeat (the comment ping keeping proxies from
// closing an idle watch), writeTimeout (one write's bound, so a client
// that stops reading is dropped instead of pinning the goroutine) and
// leaseCheck (how often the loop re-reads the lease's state as the
// backstop for an end no event announced) and settle (the window an end
// of the substrate stream waits for the lease's own state to catch up
// with, so a pause that closed the stream first is still named
// lease_suspended and not watch_ended).
type watchPace struct {
	coalesce     time.Duration
	heartbeat    time.Duration
	writeTimeout time.Duration
	leaseCheck   time.Duration
	settle       time.Duration
}

var fileWatchPace = watchPace{
	coalesce:     100 * time.Millisecond,
	heartbeat:    15 * time.Second,
	writeTimeout: 30 * time.Second,
	leaseCheck:   500 * time.Millisecond,
	settle:       2 * time.Second,
}

// fileWatchEvent is one SSE "data:" line's JSON.
type fileWatchEvent struct {
	Kind    string   `json:"kind"`
	Paths   []string `json:"paths,omitempty"`
	Code    string   `json:"code,omitempty"`
	Message string   `json:"message,omitempty"`
}

// watchLeaseState is the lease state a watch maps to its terminal error
// event: running keeps watching.
type watchLeaseState int

const (
	watchStateRunning watchLeaseState = iota
	watchStateSuspended
	watchStateLost
	watchStateGone
)

// watchExitCode names a watch's terminal code from the lease's state.
// A plain stream end under a still-live lease is watch_ended: whatever
// dropped the substrate stream is not the lease's lifecycle.
func watchExitCode(state watchLeaseState) string {
	switch state {
	case watchStateGone:
		return "lease_gone"
	case watchStateLost:
		return "lease_lost"
	case watchStateSuspended:
		return "lease_suspended"
	default:
		return "watch_ended"
	}
}

// watchExitMessage is the terminal error's message beside its code.
func watchExitMessage(state watchLeaseState) string {
	switch state {
	case watchStateGone:
		return "the lease is gone; the watch has ended"
	case watchStateLost:
		return "the lease is lost; the watch has ended"
	case watchStateSuspended:
		return "the lease is suspended; re-watch after it resumes"
	default:
		return "the watch ended"
	}
}

// watchLeaseNow snapshots the lease's lifecycle state under the store
// lock, so the watch loop reads it without racing a pause, a release or
// a suspend. A lease the store no longer lists (it was released, or the
// pointer is a superseded entry) is gone.
func (s *Service) watchLeaseNow(l *Lease) watchLeaseState {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	switch {
	case l.released || s.store.leases[l.ID] != l:
		return watchStateGone
	case l.State == "lost":
		return watchStateLost
	case l.Suspended || !l.live():
		return watchStateSuspended
	default:
		return watchStateRunning
	}
}

// watchEndsOn reports whether a lease lifecycle event can end a watch:
// a pause of any kind (the suspended event, with idle_suspended beside
// it), a loss, a release, and the restored / restarted transitions
// (restore swaps the guest's filesystem; a restart's pause ends the
// watch and the client re-establishes). Anything else — a resumed lease,
// a label change — is not the watch's business.
func watchEndsOn(t LeaseEventType) bool {
	switch t {
	case LeaseSuspended, LeaseIdleSuspended, LeaseLost, LeaseReleased, LeaseRestored, LeaseRestarted:
		return true
	default:
		return false
	}
}

// filesWatchGate is the watch route's gate: like filesGate but without
// the activity touch (a watch is not activity). The returned lease is
// resolved, resumed when suspended and live when not lost.
func (s *Server) filesWatchGate(w http.ResponseWriter, r *http.Request) *Lease {
	lease := s.filesTarget(r)
	if lease == nil {
		writeErrorCode(w, http.StatusNotFound, "not_found", "lease not found")
		return nil
	}
	if !s.ensureRunning(w, r, lease) {
		return nil
	}
	if !s.ensureLive(w, lease) {
		return nil
	}
	return lease
}

// handleFileWatch answers GET .../files/{path}?watch=1&recursive=true:
// the watched directory's changes as Server-Sent Events, one data line
// per event: {"kind":"ready"} once the watch is live, then
// {"kind":"change","paths":[...]} events with each coalescing window's
// paths, then one {"kind":"error","code":...,"message":...} when the
// watch ends (and only then). A missing path is 404 and a path that is a
// file is 409, before any stream opens, in the plain JSON error shape
// the other file routes answer with. Opening the watch resumes a
// suspended lease (the watch needs the guest) and holds an exec/stream
// slot for its lifetime, but never counts as activity.
func (s *Server) handleFileWatch(w http.ResponseWriter, r *http.Request) {
	lease := s.filesWatchGate(w, r)
	if lease == nil {
		return
	}
	guestPath, ok := filePathValue(r)
	if !ok {
		writeErrorCode(w, http.StatusNotFound, "EINVAL", "file path required")
		return
	}
	recursive := r.URL.Query().Get("recursive") == "true"
	// The preconditions precede any streaming: the substrate says whether
	// the path exists (404) and is a directory (409) through the same
	// error mapping every file route answers with. (A connect-level
	// watcher conflates a not-a-directory with not-found, so the API
	// prechecks are also the uniform shape across substrates.)
	info, err := s.svc.sub.Stat(r.Context(), lease.SandboxID, guestPath)
	if err != nil {
		s.mapFileError(w, lease, "watch stat", err)
		return
	}
	if !info.IsDir {
		writeErrorCode(w, http.StatusConflict, "ENOTDIR", guestPath+" is not a directory")
		return
	}
	owner := ownerFrom(r.Context())
	if !s.acquireBusy(owner) {
		writeErrorCode(w, http.StatusTooManyRequests, "too_busy", "too many concurrent exec/stream operations; try again shortly")
		return
	}
	defer s.releaseBusy(owner)

	// The watch rides the request's context: a hangup tears the substrate
	// stream down with it.
	events, err := s.svc.sub.Watch(r.Context(), lease.SandboxID, guestPath, recursive)
	if err != nil {
		s.mapFileError(w, lease, "watch", err)
		return
	}

	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		writeErrorCode(w, http.StatusInternalServerError, "internal", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	setDeadline := func() {
		_ = rc.SetWriteDeadline(time.Now().Add(fileWatchPace.writeTimeout))
	}
	setDeadline()
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 3000\n\n")
	// Push the headers (and the reconnect hint) out now, or a client
	// waits for the first event to see the stream open.
	flusher.Flush()

	// The lease subscription is taken before anything is written, so a
	// pause racing the watch's open is still seen. Every lifecycle
	// signal is confirmed against the lease's live state under the store
	// lock: a suspended event that lost a race with a resume leaves a
	// running lease watching, and the loop's own lease check catches a
	// pause whose event never arrived.
	sub := s.svc.Subscribe(EventFilter{LeaseID: lease.ID})
	defer sub.Close()

	leaseState := func() watchLeaseState { return s.svc.watchLeaseNow(lease) }
	writeData := func(ev fileWatchEvent) bool {
		setDeadline()
		line, _ := json.Marshal(ev)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", line); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	endWatch := func(state watchLeaseState) {
		writeData(fileWatchEvent{Kind: "error", Code: watchExitCode(state), Message: watchExitMessage(state)})
	}
	// settleLease waits for the lease's own state to catch up with a
	// pause, a loss or a release in flight: the substrate watch is torn
	// down (and a lifecycle event is emitted) before the lease's
	// Suspended state — or its removal from the store — is visible, so an
	// immediate read would see a still-running lease and mis-report
	// watch_ended for what is really a suspension. It returns the settled
	// state, or watchStateRunning when the lease stayed running through
	// the settle window (a signal that lost a race with a resume).
	settleLease := func() watchLeaseState {
		deadline := time.Now().Add(fileWatchPace.settle)
		for {
			if state := leaseState(); state != watchStateRunning {
				return state
			}
			if time.Now().After(deadline) {
				return watchStateRunning
			}
			sleep := time.NewTimer(20 * time.Millisecond)
			select {
			case <-r.Context().Done():
				sleep.Stop()
				return watchStateRunning
			case <-sleep.C:
			}
		}
	}

	// started gates the protocol order: the substrate's ready lands once
	// and becomes the first event the client sees; anything before it
	// (an early change is unlikely, but possible over a slow envd open)
	// is dropped, because nothing was watching yet.
	started := false
	heartbeat := time.NewTicker(fileWatchPace.heartbeat)
	defer heartbeat.Stop()
	// pendingSignal marks a lifecycle event whose lease state still read
	// running (the pause in flight): the next settle pass re-reads it.
	pendingSignal := false
	for {
		setDeadline()
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-sub.C:
			if !ok {
				return
			}
			if !started || !watchEndsOn(ev.Type) {
				// A lifecycle event before the ready cannot end anything:
				// nothing is watching yet (the gate 409/404s first).
				continue
			}
			// Confirm the signal against the lease's state: a pause whose
			// sub.Pause has not returned yet still reads running — the
			// check is a shortcut for a signal whose state has settled,
			// and one that lost a race with a resume is a running lease
			// that must keep watching.
			if state := leaseState(); state != watchStateRunning {
				endWatch(state)
				return
			}
			pendingSignal = true
		case ev, ok := <-events:
			if !ok {
				// The substrate watch ended: its context was the
				// request's, so a hangup is the clean case; a guest pause
				// or a transport drop can also land here. The lease's
				// state decides the terminal event — after a short settle,
				// because the teardown of the stream can beat the lease's
				// own Suspended/removal by a beat. A lease still running
				// has nothing left to watch, so that is watch_ended.
				endWatch(settleLease())
				return
			}
			switch ev.Kind {
			case substrate.WatchReady:
				if !started {
					started = true
					if !writeData(fileWatchEvent{Kind: "ready"}) {
						return
					}
				}
			case substrate.WatchChange:
				if !started {
					continue
				}
				end := s.flushCoalescedChange(w, flusher, setDeadline, events, ev)
				if end != nil {
					// A write error without a terminal substrate event is a
					// client hangup (or a deadline): the stream just ends. A
					// terminal one (a WatchError inside the window) follows
					// the same settle rule as a close: a pause that landed
					// inside the window must name itself.
					if end.terminal {
						endWatch(settleLease())
					}
					return
				}
				continue
			case substrate.WatchError:
				// The substrate named the failure itself (its message is
				// transport detail): the lease's state decides the code,
				// so a guest pause reads lease_suspended, a release
				// lease_gone and anything else watch_ended.
				if state := leaseState(); state != watchStateRunning {
					endWatch(state)
				} else {
					endWatch(watchStateRunning)
				}
				return
			}
		case <-time.After(fileWatchPace.leaseCheck):
			if pendingSignal || leaseState() != watchStateRunning {
				// A signalled end the lease's state has not shown yet, or
				// one no event announced: settle — wait for the lease's own
				// state to catch up with the pause or release in flight,
				// then name what it became. A clean settle finds the lease
				// running (the signal was, say, a suspended event that
				// lost a race with a resume): nothing ended, keep watching.
				pendingSignal = false
				state := settleLease()
				if state == watchStateRunning {
					continue
				}
				endWatch(state)
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// watchEnd is what a coalesced flush reports back to the loop: nil (no
// error, keep looping), or a terminal condition. terminal marks whether
// the substrate already named its own error (its paths are already
// flushed when present).
type watchEnd struct {
	terminal bool
	err      error
}

// flushCoalescedChange writes one change event, gathering the paths of
// every further change that lands within the coalescing window of the
// first. Paths are deduplicated (a file written twice in a window is one
// path) and sorted, so an event's paths are stable. A substrate error or
// close inside the window is reported back (with the change still
// flushed first, so a terminal event never swallows pending change
// paths); the caller ends the stream.
func (s *Server) flushCoalescedChange(
	w http.ResponseWriter,
	flusher http.Flusher,
	setDeadline func(),
	events <-chan substrate.WatchEvent,
	first substrate.WatchEvent,
) *watchEnd {
	seen := make(map[string]struct{}, len(first.Paths)+1)
	order := make([]string, 0, len(first.Paths)+1)
	add := func(ps []string) {
		for _, p := range ps {
			if p == "" {
				continue
			}
			if _, dup := seen[p]; !dup {
				seen[p] = struct{}{}
				order = append(order, p)
			}
		}
	}
	add(first.Paths)
	timer := time.NewTimer(fileWatchPace.coalesce)
	defer timer.Stop()
	var substrateErr error
gather:
	for {
		select {
		case more, ok := <-events:
			if !ok {
				break gather
			}
			switch more.Kind {
			case substrate.WatchChange:
				add(more.Paths)
			case substrate.WatchError:
				substrateErr = errors.New(more.Err)
				break gather
			}
		case <-timer.C:
			break gather
		}
	}
	sort.Strings(order)
	setDeadline()
	line, _ := json.Marshal(fileWatchEvent{Kind: "change", Paths: order})
	if _, err := fmt.Fprintf(w, "data: %s\n\n", line); err != nil {
		return &watchEnd{err: err}
	}
	flusher.Flush()
	if substrateErr != nil {
		return &watchEnd{terminal: true, err: substrateErr}
	}
	return nil
}
