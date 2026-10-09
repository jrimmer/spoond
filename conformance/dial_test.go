//go:build conformance

package conformance

// Guest port dial (2.2, #113): a TCP echo listener runs inside a py-base
// lease (a python3 one-liner via exec) and the suite dials it through
// GET /api/sandboxes/{id}/ports/{port}/dial, proving host-to-guest TCP
// works on E2B (substrate.DialGuest, the orchestrator's HostIP DNAT).
// The dial is host-to-guest, not guest egress, so it must work under
// every network policy; the test runs under the default restricted.

import (
	"strconv"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestN7_GuestDialEcho starts a TCP echo server in a lease and round-trips
// bytes through the dial route.
func TestN7_GuestDialEcho(t *testing.T) {
	rec := begin(t)

	l := createLease(t, map[string]any{"image": "py-base", "ttl": 600})
	if l.Address == "" {
		failf(t, "create: empty address in lease %+v", l)
	}

	// The echo server: fork once, echo every byte until the client goes.
	port := 9100 + int(time.Now().UnixNano()%1000) // keep clear of parallel leases
	execOK(t, l.ID, `nohup python3 -c '
import socket
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("0.0.0.0", `+itoa(port)+`))
s.listen(4)
while True:
    c, _ = s.accept()
    def run(c):
        try:
            while True:
                d = c.recv(4096)
                if not d:
                    break
                c.sendall(d)
        except Exception:
            pass
        finally:
            c.close()
    import threading
    threading.Thread(target=run, args=(c,), daemon=True).start()
' >/dev/null 2>&1 &`)

	// Wait until the guest answers (exec into a fresh lease starts fast,
	// but the nohup'd listener needs a moment).
	deadline := time.Now().Add(15 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		if canTCP(t, l.ID, "127.0.0.1", port) {
			ready = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !ready {
		failf(t, "guest echo server on port %d never came up", port)
	}

	conn, status, err := cl.dialGuest(l.ID, port)
	if err != nil {
		failf(t, "dial %s:%d: status %d: %v", l.Address, port, status, err)
	}
	defer conn.Close()
	rec.set("port", port)

	// Round-trip both ways, twice, so each direction gets its own chance
	// to fail.
	for i, payload := range []string{"dial-up", "dial-down"} {
		if err := conn.WriteMessage(websocket.BinaryMessage, []byte(payload)); err != nil {
			failf(t, "write %d: %v", i, err)
		}
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		mt, data, err := conn.ReadMessage()
		if err != nil {
			failf(t, "read %d: %v", i, err)
		}
		if mt != websocket.BinaryMessage {
			failf(t, "read %d: frame type %d, want binary", i, mt)
		}
		if string(data) != payload {
			failf(t, "read %d: echo %q, want %q", i, data, payload)
		}
	}
	rec.set("round_trips", 2)

	// The route closes both sides: after a client close, a fresh dial
	// must still round-trip (the listener keeps serving; the old guest
	// socket is gone).
	conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second))
	conn.Close()

	conn2, status, err := cl.dialGuest(l.ID, port)
	if err != nil {
		failf(t, "redial after close: status %d: %v", status, err)
	}
	defer conn2.Close()
	if err := conn2.WriteMessage(websocket.BinaryMessage, []byte("again")); err != nil {
		failf(t, "redial write: %v", err)
	}
	conn2.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, data, err := conn2.ReadMessage()
	if err != nil {
		failf(t, "redial read: %v", err)
	}
	if string(data) != "again" {
		failf(t, "redial echo %q, want again", data)
	}
}

// TestN7b_GuestDialRefusals checks the route's guards from inside the
// suite: a bad port is 400, an unknown lease is 404, and a suspended
// lease resumes on the dial (2.9, #145 D2) and is then dialed. Nothing
// listens on the dialed port in a fresh py-base lease, so the resumed
// dial fails with 502 — the guest-dial error, not a resume failure (a
// failed resume answers 409/429/503/410, never 502). The lease is
// running after the dial.
func TestN7b_GuestDialRefusals(t *testing.T) {
	begin(t)

	l := createLease(t, map[string]any{"image": "py-base", "ttl": 600, "persistent": true})

	// Bad port.
	if _, status, err := cl.dialGuest(l.ID, 0); err == nil {
		failf(t, "port 0 dial succeeded, want 400")
	} else if status != 400 {
		failf(t, "port 0 dial status %d, want 400", status)
	}

	// Unknown lease.
	if _, status, err := cl.dialGuest("ffffffffffffffffffffffffffffffff", 80); err == nil {
		failf(t, "unknown-lease dial succeeded, want 404")
	} else if status != 404 {
		failf(t, "unknown-lease dial status %d, want 404", status)
	}

	// Suspended lease: the dial resumes it and is then served. Port 80 has
	// no listener in a fresh py-base lease, so the guest dial itself fails
	// with 502 Bad Gateway. The lease must be running after the 502 — that
	// proves the 502 came from the guest dial, not from a resume that was
	// refused (a resume refusal is 409/429/503/410, never 502).
	if st, body, err := cl.suspend(l.ID); err != nil || st != 200 {
		failf(t, "suspend: %d %s (%v)", st, truncate(body), err)
	}
	if _, status, err := cl.dialGuest(l.ID, 80); err == nil {
		failf(t, "suspended-lease dial succeeded, want 502 (nothing listens on 80)")
	} else if status != 502 {
		failf(t, "suspended-lease dial status %d, want 502 (a resumed dial with no listener): %v", status, err)
	}
	if leaseSuspended(t, l.ID) {
		failf(t, "lease %s still suspended after the dial", l.ID)
	}
	// The resumed guest runs: an exec answers (and nothing listens on 80).
	if got := execOK(t, l.ID, "echo dialed"); got != "dialed" {
		failf(t, "exec after the resuming dial: got %q, want dialed", got)
	}
}

// itoa is strconv.Itoa at the call sites inside the shell snippet.
func itoa(n int) string { return strconv.Itoa(n) }
