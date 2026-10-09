package api

// Guest port dial tests (#113): the route bridges a WebSocket to a TCP
// port inside a lease through substrate.DialGuest. The fake substrate
// dials a real host:port, so every test points lease.HostIP at an
// in-process TCP echo server.

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/jrimmer/spoond/v2/identity"
)

// newEchoServer starts a TCP echo server (it writes back every byte it
// reads) and registers its shutdown. Returns the host and port to dial.
func newEchoServer(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						if _, werr := c.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}(c)
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port
}

// pointHostIP sets a lease's HostIP to host:port's host — what DialGuest
// dials — bypassing the fake sandbox's empty address.
func pointHostIP(t *testing.T, svc *Service, id, host string, port int) {
	t.Helper()
	svc.store.mu.Lock()
	defer svc.store.mu.Unlock()
	l := svc.store.leases[id]
	if l == nil {
		t.Fatalf("pointHostIP: no lease %s", id)
	}
	l.HostIP = host
}

// dialGuest opens the dial WebSocket and registers cleanup.
func dialGuest(t *testing.T, ts *httptest.Server, id string, port int, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/sandboxes/" + id + "/ports/" + strconv.Itoa(port) + "/dial"
	hdr := http.Header{}
	if token != "" {
		hdr.Set("Authorization", "Bearer "+token)
	}
	ws, resp, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err == nil {
		t.Cleanup(func() { ws.Close() })
	}
	return ws, resp, err
}

// itoa is strconv.Itoa at the local call sites' readability.
var itoa = strconv.Itoa

func TestGuestDialEcho(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)
	host, port := newEchoServer(t)

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)
	pointHostIP(t, svc, id, host, port)

	// The /api/leases alias must work like the /api/sandboxes spelling.
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/leases/" + id + "/ports/" + strconv.Itoa(port) + "/dial"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer token-a"}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { ws.Close() })

	// Client -> guest -> client round-trips.
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("ping-guest")); err != nil {
		t.Fatalf("write: %v", err)
	}
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	mt, payload, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if mt != websocket.BinaryMessage {
		t.Fatalf("frame type %d, want binary", mt)
	}
	if string(payload) != "ping-guest" {
		t.Fatalf("echo = %q, want ping-guest", payload)
	}

	// Larger payloads survive too (multi-read on the guest side).
	big := strings.Repeat("x", 100000)
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte(big)); err != nil {
		t.Fatalf("write big: %v", err)
	}
	got := make([]byte, 0, len(big))
	deadline := time.Now().Add(10 * time.Second)
	for len(got) < len(big) {
		if time.Now().After(deadline) {
			t.Fatalf("big echo incomplete: got %d of %d bytes", len(got), len(big))
		}
		ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, payload, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("read big echo: %v", err)
		}
		got = append(got, payload...)
	}
	if string(got) != big {
		t.Fatalf("big echo mismatch: got %d bytes", len(got))
	}
}

func TestGuestDialClosePropagation(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)
	host, port := newEchoServer(t)

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)
	pointHostIP(t, svc, id, host, port)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	port = ln.Addr().(*net.TCPAddr).Port

	ws, _, err := dialGuest(t, ts, id, port, "token-a")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Client close closes the guest TCP connection.
	if err := ws.Close(); err != nil {
		t.Fatalf("ws close: %v", err)
	}
	select {
	case c := <-accepted:
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Fatal("guest connection still open after the WebSocket closed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("guest connection was never accepted")
	}
}

func TestGuestDialGuestClose(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)
	host, port := newEchoServer(t)

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)
	pointHostIP(t, svc, id, host, port)

	// A server that closes immediately: the WebSocket must close too.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	port = ln.Addr().(*net.TCPAddr).Port

	ws, _, err := dialGuest(t, ts, id, port, "token-a")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Fatal("read after guest close succeeded, want the bridge to close")
	}
}

func TestGuestDialNotFoundForOtherOwner(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)
	host, port := newEchoServer(t)

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)
	pointHostIP(t, svc, id, host, port)

	if _, resp, err := dialGuest(t, ts, id, port, "token-b"); err == nil {
		t.Fatal("another owner's dial succeeded, want 404")
	} else if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-owner dial = %d, want 404", resp.StatusCode)
	}
}
func TestGuestDialSuspendedResumes(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)
	host, port := newEchoServer(t)

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300, "persistent": true})
	id := create["id"].(string)
	pointHostIP(t, svc, id, host, port)

	resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/suspend", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("suspend = %d", resp.StatusCode)
	}
	l := svc.lookupAny(id)

	// The dial resumes the suspended lease on use (#145 D2); it never
	// answers 409 lease_suspended. The resume resets HostIP to the fake
	// sandbox's address, so the dial itself may fail later (that is not
	// a suspend refusal).
	if _, resp, err := dialGuest(t, ts, id, port, "token-a"); err == nil {
		t.Fatal("dial succeeded, want the post-resume dial to fail on the fake address")
	} else if resp.StatusCode == http.StatusConflict {
		t.Fatalf("suspended dial = %d, want no 409: a suspended lease resumes on dial", resp.StatusCode)
	}
	if l.Suspended {
		t.Fatal("the dial did not resume the suspended lease")
	}
}

func TestGuestDialLimit(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)
	host, port := newEchoServer(t)

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)
	pointHostIP(t, svc, id, host, port)

	// Hold guestDialLimit slots with open dials.
	var open []*websocket.Conn
	for range guestDialLimit {
		ws, _, err := dialGuest(t, ts, id, port, "token-a")
		if err != nil {
			t.Fatalf("dial %d: %v", len(open), err)
		}
		open = append(open, ws)
	}
	if _, resp, err := dialGuest(t, ts, id, port, "token-a"); err == nil {
		t.Fatal("dial over the per-owner cap succeeded, want 429")
	} else if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-cap dial = %d, want 429", resp.StatusCode)
	}
	// The refusal is counted.
	if _, _, refused := guestDialMetrics(t, ts); refused < 1 {
		t.Fatalf("spoond_guest_dials_total{result=\"refused\"} = %v, want >= 1", refused)
	}

	// Another owner is not affected by token-a's cap.
	_, createB := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-b", map[string]any{"image": "py-base", "ttl": 300})
	idB := createB["id"].(string)
	pointHostIP(t, svc, idB, host, port)
	if _, _, err := dialGuest(t, ts, idB, port, "token-b"); err != nil {
		t.Fatalf("other owner over token-a's cap: %v", err)
	}

	// Closing the dials frees the slots.
	for _, ws := range open {
		ws.Close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, _, err := dialGuest(t, ts, id, port, "token-a"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dial slots never freed after the sockets closed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestGuestDialBadPort(t *testing.T) {
	ts, _ := newTestServer(t)
	for _, port := range []string{"0", "-1", "65536", "99999999999999999999", "abc"} {
		wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/sandboxes/whatever-id/ports/" + port + "/dial"
		if _, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer token-a"}}); err == nil {
			t.Fatalf("port %q dial succeeded, want 400", port)
		} else if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("port %q dial = %d, want 400", port, resp.StatusCode)
		}
	}
}

func TestGuestDialDialFailureIsBadGateway(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)
	// Nothing listens on the guest port: the substrate dial fails.
	pointHostIP(t, svc, id, "127.0.0.1", 1)

	if _, resp, err := dialGuest(t, ts, id, 12345, "token-a"); err == nil {
		t.Fatal("dial to a dead guest port succeeded, want 502")
	} else if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("dead-port dial = %d, want 502", resp.StatusCode)
	}
}

func TestGuestDialMetricsAndAlias(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)
	host, port := newEchoServer(t)

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)
	pointHostIP(t, svc, id, host, port)

	ws, _, err := dialGuest(t, ts, id, port, "token-a")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("m")); err != nil {
		t.Fatalf("write: %v", err)
	}
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := ws.ReadMessage(); err != nil {
		t.Fatalf("echo: %v", err)
	}
	ws.Close()

	// The active gauge counts open dials: scrape /metrics and read the
	// gauge and the counter series.
	deadline := time.Now().Add(2 * time.Second)
	for {
		gauge, okResult, refused := guestDialMetrics(t, ts)
		if gauge == 0 && okResult >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial metrics wrong: active=%v ok=%v refused=%v", gauge, okResult, refused)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// guestDialMetrics scrapes /metrics for the dial series. Needs a server
// built with identities or a METRICS_TOKEN in real deployments; the test
// server's token-a is the only auth it has, and /metrics accepts any
// authenticated token when there is no identity store.
func guestDialMetrics(t *testing.T, ts *httptest.Server) (active float64, ok, refused float64) {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer token-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 0, 1<<16)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	for _, line := range strings.Split(string(buf), "\n") {
		switch {
		case strings.HasPrefix(line, "spoond_guest_dials_active "):
			_, v := parseSample(t, line)
			active = v
		case strings.HasPrefix(line, `spoond_guest_dials_total{result="ok"}`):
			_, v := parseSample(t, line)
			ok = v
		case strings.HasPrefix(line, `spoond_guest_dials_total{result="refused"}`):
			_, v := parseSample(t, line)
			refused = v
		}
	}
	return active, ok, refused
}

func parseSample(t *testing.T, line string) (string, float64) {
	t.Helper()
	parts := strings.Fields(line)
	if len(parts) != 2 {
		t.Fatalf("malformed sample: %q", line)
	}
	var v float64
	if _, err := fmt.Sscan(parts[1], &v); err != nil {
		t.Fatalf("sample %q: %v", parts[1], err)
	}
	return parts[0], v
}

func TestGuestDialAdminAllowed(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ids, err := identity.NewStore("")
	if err != nil {
		t.Fatalf("identity store: %v", err)
	}
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(db))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Bootstrap: the first user is admin; the admin then creates the
	// plain user (after bootstrap, creation is admin-only).
	if rec, body := doUsersReq(t, srv.Handler(), "POST", "/api/users", "token-a",
		`{"name":"root","fingerprints":["SHA256:a"],"token":"admin-tok"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create admin: %d %v", rec.Code, body)
	}
	if rec, body := doUsersReq(t, srv.Handler(), "POST", "/api/users", "admin-tok",
		`{"name":"plain","fingerprints":["SHA256:b"],"token":"plain-tok"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create plain user: %d %v", rec.Code, body)
	}

	host, port := newEchoServer(t)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)
	pointHostIP(t, svc, id, host, port)

	// The admin may dial someone else's lease...
	ws, _, err := dialGuest(t, ts, id, port, "admin-tok")
	if err != nil {
		t.Fatalf("admin dial: %v", err)
	}
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("hi")); err != nil {
		t.Fatalf("admin write: %v", err)
	}
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, payload, err := ws.ReadMessage(); err != nil || string(payload) != "hi" {
		t.Fatalf("admin echo = %q (%v), want hi", payload, err)
	}

	// ...while a plain non-owner user gets the same 404 as everywhere.
	if _, resp, err := dialGuest(t, ts, id, port, "plain-tok"); err == nil {
		t.Fatal("plain non-owner dial succeeded, want 404")
	} else if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("plain non-owner dial = %d, want 404", resp.StatusCode)
	}
}

func TestGuestDialMetricsPathNormalized(t *testing.T) {
	// The guest port must not become a metrics label: one series per
	// port would be unbounded cardinality.
	in := "/api/sandboxes/0123456789abcdef0123456789abcdef/ports/8080/dial"
	if got, want := normalizePath(in), "/api/sandboxes/:id/ports/:port/dial"; got != want {
		t.Fatalf("normalizePath(%q) = %q, want %q", in, got, want)
	}
}

func TestGuestDialIdleTimeout(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)

	// A quiet listener: accepts, says nothing, reads nothing.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { // hold it open, silent
				buf := make([]byte, 16)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}(c)
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)
	pointHostIP(t, svc, id, "127.0.0.1", port)

	// Shrink the idle timeout for the test and restore it after (the
	// bridge under test is created by the request, so the variable must
	// already be small when the handler runs).
	old := guestDialIdleTimeout
	guestDialIdleTimeout = 150 * time.Millisecond
	t.Cleanup(func() { guestDialIdleTimeout = old })

	ws, _, err := dialGuest(t, ts, id, port, "token-a")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// The server stops reading after the deadline, so the guest's TCP
	// read loop returning error closes the bridge; from the client side
	// the socket goes away without any frame.
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Fatal("idle dial stayed open past the idle timeout")
	}
	ws.Close()
	// Wait for the server-side bridge to be fully gone before the
	// cleanup restores the timeout variable (the race detector watches
	// the reads inside bridgeWSGuest): the active gauge drops to 0.
	deadline := time.Now().Add(5 * time.Second)
	for {
		active, _, _ := guestDialMetrics(t, ts)
		if active == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle bridge never released its dial slot")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestGuestDialRefusesEnvdAndNoHost(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)

	// No host address: refused, never dialed as ":port" (the backend's
	// own loopback).
	if _, resp, err := dialGuest(t, ts, id, 8080, "token-a"); err == nil || resp == nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("empty HostIP: err=%v resp=%v, want 502", err, resp)
	}

	pointHostIP(t, svc, id, "127.0.0.1", 49983)
	if _, resp, err := dialGuest(t, ts, id, 49983, "token-a"); err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("envd port: err=%v resp=%v, want 403", err, resp)
	}
}

func TestGuestDialOneWayStaysOpen(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)

	// A talker: sends a byte every 30ms, reads nothing.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		for i := 0; i < 20; i++ {
			if _, err := c.Write([]byte{'x'}); err != nil {
				return
			}
			time.Sleep(30 * time.Millisecond)
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)
	pointHostIP(t, svc, id, "127.0.0.1", port)

	old := guestDialIdleTimeout
	guestDialIdleTimeout = 150 * time.Millisecond
	t.Cleanup(func() { guestDialIdleTimeout = old })

	ws, _, err := dialGuest(t, ts, id, port, "token-a")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// 20 bytes over ~600ms, four idle timeouts, with the client never
	// sending: the bridge must stay up while bytes flow one way.
	got := 0
	for got < 20 {
		ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, p, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("one-way stream closed after %d bytes: %v", got, err)
		}
		got += len(p)
	}
	ws.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		active, _, _ := guestDialMetrics(t, ts)
		if active == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bridge never released its dial slot")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
