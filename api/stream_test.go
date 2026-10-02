package api

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/jrimmer/spoond/identity"
	"github.com/jrimmer/spoond/substrate"
	"github.com/jrimmer/spoond/substrate/fake"
)

// dialStream opens a WebSocket to /stream on ts with the given headers.
func dialStream(t *testing.T, ts *httptest.Server, id string, header http.Header) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/sandboxes/" + id + "/stream"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("dial stream: %v", err)
	}
	t.Cleanup(func() { ws.Close() })
	return ws
}

// bearerHeader builds Authorization (+ optional impersonation) headers.
func bearerHeader(token string, impersonate string) http.Header {
	h := http.Header{"Authorization": {"Bearer " + token}}
	if impersonate != "" {
		h.Set("X-Spoond-User-Id", impersonate)
	}
	return h
}

// startBinary sends a binary-mode first frame and returns the process.
func startBinary(t *testing.T, ws *websocket.Conn) *fakeProcessHandle {
	t.Helper()
	if err := ws.WriteMessage(websocket.TextMessage,
		[]byte(`{"args":["bash"],"binary":true,"cols":120,"rows":40,"pty":true}`)); err != nil {
		t.Fatalf("write first frame: %v", err)
	}
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	mt, payload, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read started frame: %v", err)
	}
	if mt != websocket.TextMessage {
		t.Fatalf("started frame type %d, want text", mt)
	}
	var started struct {
		Stream string `json:"stream"`
		PID    uint32 `json:"pid"`
		Pty    bool   `json:"pty"`
	}
	if err := json.Unmarshal(payload, &started); err != nil || started.Stream != "started" {
		t.Fatalf("started frame %q: %v", payload, err)
	}
	return &fakeProcessHandle{ws: ws, pid: started.PID}
}

// fakeProcessHandle pairs the WebSocket with the fake process it started.
type fakeProcessHandle struct {
	ws  *websocket.Conn
	pid uint32
}

func (h *fakeProcessHandle) proc(t *testing.T, sub *testSub) *fake.FakeProcess {
	t.Helper()
	p := sub.Proc(h.pid)
	if p == nil {
		t.Fatalf("no fake process with pid %d", h.pid)
	}
	return p
}

func (h *fakeProcessHandle) readBinary(t *testing.T) []byte {
	t.Helper()
	h.ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	mt, payload, err := h.ws.ReadMessage()
	if err != nil {
		t.Fatalf("read binary frame: %v", err)
	}
	if mt != websocket.BinaryMessage {
		t.Fatalf("frame type %d, want binary (%q)", mt, payload)
	}
	return payload
}

func (h *fakeProcessHandle) readText(t *testing.T) string {
	t.Helper()
	h.ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	mt, payload, err := h.ws.ReadMessage()
	if err != nil {
		t.Fatalf("read text frame: %v", err)
	}
	if mt != websocket.TextMessage {
		t.Fatalf("frame type %d, want text", mt)
	}
	return string(payload)
}

// TestStreamBinaryFraming pins the binary protocol: output frames are
// binary with the channel in byte 0 (1 stdout, 2 stderr, 3 pty), control
// frames stay text, client binary frames are raw stdin, and {"in"} is
// ignored in binary mode.
func TestStreamBinaryFraming(t *testing.T) {
	ts, _, db, sub := newTestServerWithService(t)
	seedImage(t, db, "py-base", 2048)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)

	ws := dialStream(t, ts, id, bearerHeader("token-a", ""))
	h := startBinary(t, ws)

	if got := sub.LastStart(); got.Cols != 120 || got.Rows != 40 || !got.PTY {
		t.Fatalf("start request = %+v, want cols 120 rows 40 with PTY", got)
	}

	proc := h.proc(t, sub)
	proc.Push(substrate.ProcessEvent{Kind: substrate.EventStdout, Data: []byte("hello")})
	proc.Push(substrate.ProcessEvent{Kind: substrate.EventStderr, Data: []byte("oops")})
	proc.Push(substrate.ProcessEvent{Kind: substrate.EventPTY, Data: []byte("raw")})
	proc.Push(substrate.ProcessEvent{Kind: substrate.EventExit, ExitCode: 7})

	if got := h.readBinary(t); string(got) != "\x01hello" {
		t.Fatalf("stdout frame = %q, want channel-prefixed hello", got)
	}
	if got := h.readBinary(t); string(got) != "\x02oops" {
		t.Fatalf("stderr frame = %q, want channel 2 + oops", got)
	}
	if got := h.readBinary(t); string(got) != "\x03raw" {
		t.Fatalf("pty frame = %q, want channel 3 + raw", got)
	}
	// exit_code stays a text JSON frame in binary mode.
	if got := h.readText(t); got != `{"exit_code":7}` {
		t.Fatalf("exit frame = %q", got)
	}

	// A second session: raw stdin in, {"in"} ignored, controls work.
	ws2 := dialStream(t, ts, id, bearerHeader("token-a", ""))
	h2 := startBinary(t, ws2)
	proc2 := h2.proc(t, sub)
	h2.ws.WriteMessage(websocket.BinaryMessage, []byte("raw-stdin\n"))
	h2.ws.WriteMessage(websocket.TextMessage, []byte(`{"in":"must-be-ignored"}`))
	h2.ws.WriteMessage(websocket.TextMessage, []byte(`{"resize":{"cols":100,"rows":30}}`))
	deadline := time.Now().Add(2 * time.Second)
	for {
		if len(proc2.State().Inputs) >= 1 && len(proc2.State().Resizes) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("binary input/resize not relayed: inputs=%v resizes=%v", proc2.State().Inputs, proc2.State().Resizes)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if string(proc2.State().Inputs[0]) != "raw-stdin\n" {
		t.Fatalf("stdin = %q, want the raw binary payload", proc2.State().Inputs[0])
	}
	for _, in := range proc2.State().Inputs {
		if string(in) == "must-be-ignored" {
			t.Fatal(`{"in"} must be ignored in binary mode`)
		}
	}
	if !reflect.DeepEqual(proc2.State().Resizes, [][2]uint32{{100, 30}}) {
		t.Fatalf("resizes = %v", proc2.State().Resizes)
	}
}

// TestStreamKillControl pins {"action":"kill"}: SIGKILL to the process,
// then the exit frame relays.
func TestStreamKillControl(t *testing.T) {
	ts, _, db, sub := newTestServerWithService(t)
	seedImage(t, db, "py-base", 2048)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)

	ws := dialStream(t, ts, id, bearerHeader("token-a", ""))
	if err := ws.WriteMessage(websocket.TextMessage, []byte(`{"args":["bash"],"pty":true}`)); err != nil {
		t.Fatalf("write first frame: %v", err)
	}
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, payload, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read started frame: %v", err)
	}
	var started struct {
		PID uint32 `json:"pid"`
	}
	if err := json.Unmarshal(payload, &started); err != nil {
		t.Fatalf("started frame %q: %v", payload, err)
	}
	proc := sub.Proc(started.PID)
	if proc == nil {
		t.Fatalf("no fake process with pid %d", started.PID)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		proc.Push(substrate.ProcessEvent{Kind: substrate.EventExit, ExitCode: 137})
	}()
	ws.WriteMessage(websocket.TextMessage, []byte(`{"action":"kill"}`))
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, payload, err = ws.ReadMessage()
	if err != nil {
		t.Fatalf("read exit frame: %v", err)
	}
	if string(payload) != `{"exit_code":137}` {
		t.Fatalf("exit frame = %q", payload)
	}
	if !reflect.DeepEqual(proc.State().Signals, []bool{true}) {
		t.Fatalf("signals = %v, want one SIGKILL (true)", proc.State().Signals)
	}
}

// TestStreamSSHShareWithGatewayToken pins the /stream share rule: a
// request carrying the gateway's service token may attach over an ssh
// share; the same share without the gateway token stays closed. (The
// deployment registers the gateway token as a consumer token so it
// authenticates; A1 §5.3 calls it admin-equivalent.)
func TestStreamSSHShareWithGatewayToken(t *testing.T) {
	sub := newTestSub()
	db := newTestDB(t)
	seedImage(t, db, "py-base", 2048)
	svc := NewService(sub, db, map[string]string{
		"gw-tok": "gateway", // the gateway's service token, as deployed
	}, ServiceConfig{DefaultTTL: time.Minute, MaxTTL: 10 * time.Minute,
		HostGuestAddr: "10.1.0.11", HostGuestPort: 8891})
	svc.log = log.New(io.Discard, "", 0)
	svc.SetGatewayToken("gw-tok")
	ids, err := identity.NewStore("")
	if err != nil {
		t.Fatalf("identity store: %v", err)
	}
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(db))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// owner (bootstrapped with the admin-equivalent gateway token, as
	// the deployment's first user is) and friend.
	if rec, body := doUsersReq(t, srv.Handler(), "POST", "/api/users", "gw-tok",
		`{"name":"owner","fingerprints":["SHA256:a"],"token":"owner-tok"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create owner: %d %v", rec.Code, body)
	}
	rec, body := doUsersReq(t, srv.Handler(), "POST", "/api/users", "owner-tok",
		`{"name":"friend","fingerprints":["SHA256:b"],"token":"friend-tok"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create friend: %d %v", rec.Code, body)
	}
	friendID := body["user"].(map[string]any)["id"].(string)

	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "owner-tok", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)

	// An ssh share alone does not open /stream for a plain user token.
	if rec, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/share", "owner-tok",
		map[string]any{"grantee": friendID, "mode": "ssh"}); rec.StatusCode != http.StatusCreated {
		t.Fatalf("share: %d %v", rec.StatusCode, body)
	}
	if resp, _ := doReq(t, "GET", ts.URL+"/api/sandboxes/"+id+"/stream", "friend-tok", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("plain-token stream over ssh share = %d, want 404", resp.StatusCode)
	}

	// The gateway token + impersonation opens it.
	ws := dialStream(t, ts, id, bearerHeader("gw-tok", friendID))
	h := startBinary(t, ws)
	if p := h.proc(t, sub); p == nil {
		t.Fatal("stream over ssh share did not start a process")
	}
}
