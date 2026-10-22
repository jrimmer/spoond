package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/jrimmer/spoond/v4/substrate"
	"github.com/jrimmer/spoond/v4/substrate/fake"
)

// Stable error codes on the exec and exec-stream routes (spoond-wir9):
// every HTTP refusal carries a machine-readable "code" beside the
// free-form "error" message, and the stream's error frames name the same
// codes. The code is the contract; the message is diagnostic. The
// statuses were already the contract and do not change.

// execCodeCase is one exec-route case: the request body to post (nil
// posts a valid one), the precondition (a substrate failure, the lease's
// busy flag, the per-owner cap, the body cap), and the status + code to
// assert.
type execCodeCase struct {
	name  string
	body  map[string]any
	want  int
	code  string
	token string // "" = token-a

	// subFail/subErr make the substrate's method fail (FailCall).
	subFail string
	subErr  error
	// busy marks the lease mid-lifecycle-operation (the 409 branch).
	busy bool
	// shrinkBody caps MAX_EXEC_BODY_BYTES below the marshalled body.
	shrinkBody bool
	// execAnswer, when set, is what the fake answers a sync exec with
	// (the timeout case: the substrate's own marker).
	execAnswer *substrate.ExecResult
}

// execExpect asserts status, code and (for the retryable refusals) the
// Retry-After header.
func execExpect(t *testing.T, what string, resp *http.Response, body map[string]any, want int, code string) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("%s = %d, want %d: %v", what, resp.StatusCode, want, body)
	}
	got, _ := body["code"].(string)
	if got != code {
		t.Fatalf("%s code = %q, want %q (body %v)", what, got, code, body)
	}
	if code == "lease_busy" && resp.Header.Get("Retry-After") == "" {
		t.Fatalf("%s lost its Retry-After", what)
	}
}

// TestExecErrorCodes walks the sync exec route's error codes: the route
// side (lease lookup, body cap, validation, the per-owner cap) and the
// substrate side (the sandbox-gone/busy mapping, spawn_error, the
// timeout marker on the success shape).
func TestExecErrorCodes(t *testing.T) {
	cases := []execCodeCase{
		{name: "invalid JSON -> bad_request", body: map[string]any{"malformed": "{"}, want: 400, code: "bad_request"},
		{name: "cmd missing -> bad_request", body: map[string]any{}, want: 400, code: "bad_request"},
		{name: "negative max_runtime_secs -> bad_request", body: map[string]any{"cmd": "echo", "max_runtime_secs": -1}, want: 400, code: "bad_request"},
		{name: "invalid secret name -> bad_request", body: map[string]any{"cmd": "echo x", "secrets": map[string]string{"no space": "v"}}, want: 400, code: "bad_request"},
		{name: "body over the cap -> EFBIG", body: map[string]any{"cmd": "echo"}, want: 413, code: "EFBIG", shrinkBody: true},
		{name: "agent down -> EIO", body: map[string]any{"cmd": "echo"}, want: 500, code: "EIO",
			subFail: "Exec", subErr: errors.New("exec i0: agent unreachable")},
		{name: "sandbox gone -> lease_lost", body: map[string]any{"cmd": "echo"}, want: 410, code: "lease_lost",
			subFail: "Exec", subErr: substrate.ErrNotFound},
		{name: "sandbox missing mid-checkpoint -> lease_busy",
			body: map[string]any{"cmd": "echo"}, want: 409, code: "lease_busy",
			subFail: "Exec", subErr: substrate.ErrNotFound, busy: true},
		{name: "substrate unavailable -> substrate_unavailable",
			body: map[string]any{"cmd": "echo"}, want: 503, code: "substrate_unavailable",
			subFail: "Exec", subErr: substrate.ErrUnavailable},
		{name: "secret stage failed -> internal",
			body: map[string]any{"cmd": "echo", "secrets": map[string]string{"k": "v"}}, want: 500, code: "internal",
			subFail: "WriteFile", subErr: errors.New("disk gone")},
		{name: "unknown lease -> not_found", body: map[string]any{"cmd": "echo"}, want: 404, code: "not_found", token: "token-b"},
		{name: "ended at its time limit -> timeout", body: map[string]any{"cmd": "sleep 5"}, want: 200, code: "timeout",
			execAnswer: &substrate.ExecResult{ExitCode: 124, Stderr: "\n[spoond] exec timed out after 5s\n", TimedOut: true}},
		{name: "a 124 and timeout text the command produced itself -> no code", body: map[string]any{"cmd": "conn-test"}, want: 200, code: "",
			execAnswer: &substrate.ExecResult{ExitCode: 124, Stderr: "connection timed out"}},
		{name: "plain non-zero exit -> no code", body: map[string]any{"cmd": "false"}, want: 200, code: "",
			execAnswer: &substrate.ExecResult{ExitCode: 1, Stderr: "boom"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.shrinkBody {
				withExecBodyLimit(t, 1)
			}
			ts, svc, _db, sub := newTestServerWithService(t)
			_ = _db
			leaseID := filesCreate(t, ts, false)
			if tc.subFail != "" {
				sub.FailCall(tc.subFail, 0, tc.subErr)
			}
			if tc.execAnswer != nil {
				sub.SetExecHandler(func(string, substrate.ExecRequest) substrate.ExecResult {
					return *tc.execAnswer
				})
				t.Cleanup(func() { sub.SetExecHandler(nil) })
			}
			if tc.busy {
				svc.store.mu.Lock()
				svc.store.leases[leaseID].busy = true
				svc.store.mu.Unlock()
			}
			token := tc.token
			if token == "" {
				token = "token-a"
			}
			resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+leaseID+"/exec", token, tc.body)
			execExpect(t, tc.name, resp, body, tc.want, tc.code)
		})
	}
}

// TestExecTooBusyCode pins the per-owner 429's code on the exec route
// (too_busy; not in TestExecErrorCodes because the cap is filled on the
// server's limiter, not through a body).
func TestExecTooBusyCode(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	srv := NewServer(svc, NewImageRegistry(db))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	_, body := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{"image": "py-base", "ttl": 60})
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("create lease: no id in %v", body)
	}
	// The owner is the resolved consumer id, not the token.
	for i := 0; i < srv.busyMax; i++ {
		if !srv.acquireBusy("consumer-a") {
			t.Fatalf("could not fill busy slot %d", i)
		}
	}
	resp, rbody := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/exec", "token-a", map[string]any{"cmd": "echo hi"})
	execExpect(t, "exec over the per-owner cap", resp, rbody, http.StatusTooManyRequests, "too_busy")
}

// TestStreamErrorCodes pins the stream route's HTTP refusals (codes on
// the plain HTTP answers before the upgrade) and its WebSocket error
// frames.
func TestStreamErrorCodes(t *testing.T) {
	// The per-owner cap: too_busy, as JSON, before the upgrade.
	t.Run("too busy", func(t *testing.T) {
		svc, db, _ := newTestService(t)
		seedImage(t, db, "py-base", 2048)
		srv := NewServer(svc, NewImageRegistry(db))
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)
		_, body := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{"image": "py-base", "ttl": 60})
		id, _ := body["id"].(string)
		if id == "" {
			t.Fatalf("create lease: no id in %v", body)
		}
		for i := 0; i < srv.busyMax; i++ {
			if !srv.acquireBusy("consumer-a") {
				t.Fatalf("could not fill busy slot %d", i)
			}
		}
		resp, rbody := doReq(t, "GET", ts.URL+"/api/leases/"+id+"/stream", "token-a", nil)
		execExpect(t, "stream over the per-owner cap", resp, rbody, http.StatusTooManyRequests, "too_busy")
	})

	// The WS frame refusals: a non-text first frame, bad JSON and an
	// empty argv answer EINVAL; a start the substrate refuses answers
	// spawn_error.
	t.Run("frame EINVAL", func(t *testing.T) {
		ts, _, _, _ := newTestServerWithService(t)
		id := filesCreate(t, ts, false)
		ws := dialStream(t, ts, id, bearerHeader("token-a", ""))
		defer ws.Close()
		if err := ws.WriteMessage(websocket.BinaryMessage, []byte("garbage")); err != nil {
			t.Fatalf("write binary first frame: %v", err)
		}
		assertStreamCode(t, ws, "EINVAL")
	})

	t.Run("frame bad JSON", func(t *testing.T) {
		ts, _, _, _ := newTestServerWithService(t)
		id := filesCreate(t, ts, false)
		ws := dialStream(t, ts, id, bearerHeader("token-a", ""))
		defer ws.Close()
		if err := ws.WriteMessage(websocket.TextMessage, []byte("{not json")); err != nil {
			t.Fatalf("write bad JSON first frame: %v", err)
		}
		assertStreamCode(t, ws, "EINVAL")
	})

	t.Run("frame no args", func(t *testing.T) {
		ts, _, _, _ := newTestServerWithService(t)
		id := filesCreate(t, ts, false)
		ws := dialStream(t, ts, id, bearerHeader("token-a", ""))
		defer ws.Close()
		if err := ws.WriteMessage(websocket.TextMessage, []byte(`{}`)); err != nil {
			t.Fatalf("write empty first frame: %v", err)
		}
		assertStreamCode(t, ws, "EINVAL")
	})

	t.Run("frame start refused -> spawn_error", func(t *testing.T) {
		ts, _, _, subf := newTestServerWithService(t)
		id := filesCreate(t, ts, false)
		subf.SetStartHandler(func(sandboxID string, req substrate.StartRequest) (substrate.Process, error) {
			return nil, errors.New("e2b: start i0: agent unreachable")
		})
		t.Cleanup(func() { subf.SetStartHandler(nil) })
		ws := dialStream(t, ts, id, bearerHeader("token-a", ""))
		defer ws.Close()
		if err := ws.WriteMessage(websocket.TextMessage, []byte(`{"args":["bash"],"pty":false}`)); err != nil {
			t.Fatalf("write first frame: %v", err)
		}
		assertStreamCode(t, ws, "spawn_error")
	})

	// After the start: the relay's error frame carries the same shape.
	// Every in-flight failure is EIO, even one whose text says "timed
	// out": the stream has no time limit of its own, so a transport
	// error's wording is never read as the command's timeout.
	t.Run("frame in-flight -> EIO", func(t *testing.T) {
		ts, _, _, subf := newTestServerWithService(t)
		id := filesCreate(t, ts, false)
		t.Cleanup(func() { subf.SetStartHandler(nil) })
		proc := fake.NewProcess(2011)
		installJobProcess(t, subf, proc)
		ws := dialStream(t, ts, id, bearerHeader("token-a", ""))
		defer ws.Close()
		if err := ws.WriteMessage(websocket.TextMessage, []byte(`{"args":["bash"],"pty":false}`)); err != nil {
			t.Fatalf("write first frame: %v", err)
		}
		proc.Push(substrate.ProcessEvent{Kind: substrate.EventError, Err: "e2b: exec i0: process stream timed out"})
		assertStreamCode(t, ws, "EIO")
	})

	t.Run("frame in-flight -> EIO", func(t *testing.T) {
		ts, _, _, subf := newTestServerWithService(t)
		id := filesCreate(t, ts, false)
		t.Cleanup(func() { subf.SetStartHandler(nil) })
		proc := fake.NewProcess(2012)
		installJobProcess(t, subf, proc)
		ws := dialStream(t, ts, id, bearerHeader("token-a", ""))
		defer ws.Close()
		if err := ws.WriteMessage(websocket.TextMessage, []byte(`{"args":["bash"],"pty":false}`)); err != nil {
			t.Fatalf("write first frame: %v", err)
		}
		proc.Push(substrate.ProcessEvent{Kind: substrate.EventError, Err: "e2b: exec i0: process stream ended without exit"})
		assertStreamCode(t, ws, "EIO")
	})
}

// assertStreamCode reads the next text frame and asserts its code.
func assertStreamCode(t *testing.T, ws *websocket.Conn, want string) {
	t.Helper()
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	mt, payload, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read error frame: %v", err)
	}
	if mt != websocket.TextMessage {
		t.Fatalf("error frame type %d, want text", mt)
	}
	var frame struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(payload, &frame); err != nil {
		t.Fatalf("bad frame %q: %v", payload, err)
	}
	if frame.Code != want {
		t.Fatalf("frame code = %q, want %q (frame %s)", frame.Code, want, payload)
	}
	if frame.Error == "" {
		t.Fatalf("frame carries no error message: %s", payload)
	}
}
