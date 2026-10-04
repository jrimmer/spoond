package api

// Guest port dial (2.2, #113): a WebSocket that carries raw bytes both
// ways to a TCP port inside a lease, through substrate.DialGuest — the
// host-side path the SSH gateway and the HTTP proxy already use (the
// orchestrator DNATs the sandbox's HostIP to the guest). This is
// host-to-guest, not guest egress, so it works under every network
// policy: egress rules decide what may leave a sandbox, and no packet
// leaves the guest here.

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/websocket"

	"github.com/jrimmer/spoond/v2/substrate"
)

// guestDialIdleTimeout closes a dial that has carried no bytes for this
// long. A quiet TCP stream is indistinguishable from an abandoned one at
// this layer; the cap bounds half-open sockets. A variable (like
// maxExecTimeout) so tests can shrink it.
var guestDialIdleTimeout = 10 * time.Minute

// guestDialLimit caps one owner's concurrent dials. Dials are cheap but
// unbounded sockets are not: this is the same shape as the exec/stream
// busy cap, at its own limit.
const guestDialLimit = 16

// acquireDial reserves a dial slot for owner. Returns false when the
// per-owner cap is already reached (the handler answers 429).
func (s *Server) acquireDial(owner string) bool {
	s.dialMu.Lock()
	defer s.dialMu.Unlock()
	if s.dialCount[owner] >= guestDialLimit {
		return false
	}
	s.dialCount[owner]++
	return true
}

// releaseDial frees a dial slot acquired by acquireDial.
func (s *Server) releaseDial(owner string) {
	s.dialMu.Lock()
	defer s.dialMu.Unlock()
	if s.dialCount[owner] <= 1 {
		delete(s.dialCount, owner)
	} else {
		s.dialCount[owner]--
	}
}

// handleGuestDial upgrades GET /api/leases/{id}/ports/{port}/dial to a
// WebSocket and relays raw bytes between it and a TCP connection to
// {port} inside the lease. Owner or admin; anyone else gets the same
// 404 as the other lease routes (no existence leak). Frames are binary;
// either side closing closes both; guestDialIdleTimeout without bytes
// on either side closes the bridge.
func (s *Server) handleGuestDial(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")

	// Port first: a bad port is a 400 even for a lease that does not
	// exist (there is nothing to leak in a port number).
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		writeError(w, http.StatusBadRequest, "port must be 1-65535")
		return
	}

	// Owner or admin; shared leases are not dialable — dialing reaches
	// arbitrary guest ports, which is a step past what an http share
	// grants (exec/stream/stat).
	lease := s.svc.lookup(owner, id)
	if lease == nil && isAdmin(r) {
		lease = s.svc.lookupAny(id)
	}
	if lease == nil {
		writeError(w, http.StatusNotFound, "lease not found")
		return
	}
	// A suspended lease has no running sandbox to dial into; resume it
	// first.
	if lease.Suspended {
		writeError(w, http.StatusConflict, "lease is suspended; resume it first")
		return
	}
	if lease.State == "lost" {
		writeError(w, http.StatusGone, lostLeaseMessage)
		return
	}

	// Per-owner concurrent cap (429), counted like the exec/stream cap.
	if !s.acquireDial(owner) {
		s.metrics.GuestDialsTotal.WithLabelValues("refused").Inc()
		writeError(w, http.StatusTooManyRequests,
			"too many concurrent dials; try again shortly")
		return
	}
	defer s.releaseDial(owner)

	s.svc.touch(id) // dial attach is activity for the idle sweeper

	conn, err := s.svc.sub.DialGuest(r.Context(), lease.SandboxID, lease.HostIP, port)
	s.metrics.GuestDialsTotal.WithLabelValues(map[bool]string{true: "ok", false: "error"}[err == nil]).Inc()
	if err != nil {
		if errors.Is(err, substrate.ErrNotFound) {
			writeError(w, http.StatusGone, "lease no longer exists")
			return
		}
		s.svc.log.Printf("dial %s: guest %s:%d: %v", lease.ID, lease.HostIP, port, err)
		writeError(w, http.StatusBadGateway, "guest dial failed: "+err.Error())
		return
	}
	defer conn.Close()

	// Upgrade only after the guest dial succeeded, so a failed dial is a
	// plain HTTP error the client can read.
	upgrader := websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true }, // bearer-token auth above
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already wrote the HTTP error
	}
	s.metrics.GuestDialsActive.Inc()
	defer s.metrics.GuestDialsActive.Dec()

	bridgeWSGuest(ws, conn)
}

// bridgeWSGuest pumps raw bytes between an upgraded WebSocket and a TCP
// connection until either side closes or goes quiet for the idle
// timeout; the TCP close then closes the WebSocket and vice versa.
func bridgeWSGuest(ws *websocket.Conn, conn net.Conn) {
	// Server -> client: TCP reads become binary frames. SetReadDeadline
	// is the idle clock: every byte pushes it forward; the loop returns
	// when the guest closes (Read error) or the deadline lapses without
	// bytes. Either way it takes both sockets down: the guest's conn so
	// the socket is not half-held, and the WebSocket so the loop below
	// stops blocking on a client that is waiting for guest bytes.
	go func() {
		defer conn.Close()
		defer ws.Close() // Close may be called concurrently with reads/writes
		buf := make([]byte, 32*1024)
		for {
			if err := conn.SetReadDeadline(time.Now().Add(guestDialIdleTimeout)); err != nil {
				return
			}
			n, err := conn.Read(buf)
			if n > 0 {
				if werr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Client -> server: binary frames are raw bytes to the guest. Any
	// WebSocket error — a close from the client, or the read deadline
	// expiring without a frame — tears the bridge down; the deferred TCP
	// close unblocks the relay goroutine.
	defer conn.Close()
	defer ws.Close()
	for {
		if err := ws.SetReadDeadline(time.Now().Add(guestDialIdleTimeout)); err != nil {
			return
		}
		mt, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.BinaryMessage || len(data) == 0 {
			continue
		}
		if err := conn.SetWriteDeadline(time.Now().Add(guestDialIdleTimeout)); err != nil {
			return
		}
		if _, err := conn.Write(data); err != nil {
			return
		}
	}
}
