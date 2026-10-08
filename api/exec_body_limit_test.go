package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// withExecBodyLimit lowers the exec/stream body cap for one test and
// restores it afterwards. The var is process-global, so tests that use
// this must not run in parallel.
func withExecBodyLimit(t *testing.T, n int64) {
	t.Helper()
	old := maxExecBodyBytes
	maxExecBodyBytes = n
	t.Cleanup(func() { maxExecBodyBytes = old })
}

// TestExecBodyLimit413: a POST exec body beyond the cap is refused with
// a JSON 413 before anything is decoded, and a body within it works.
func TestExecBodyLimit413(t *testing.T) {
	withExecBodyLimit(t, 1<<10)
	ts, _, _, _ := newTestServerWithService(t)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)

	// A cmd string pushes the JSON body past the 1 KiB cap.
	oversize, err := json.Marshal(map[string]any{"cmd": strings.Repeat("a", 2<<10)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, _ := http.NewRequest("POST", ts.URL+"/api/sandboxes/"+id+"/exec", strings.NewReader(string(oversize)))
	req.Header.Set("Authorization", "Bearer token-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("oversize exec: %v", err)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize exec = %d (%v), want 413", resp.StatusCode, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "request body exceeds") {
		t.Fatalf("413 error = %q, want it to name the body cap", body["error"])
	}

	// A body within the cap is served as before.
	resp, ok := doReq(t, "POST", ts.URL+"/api/sandboxes/"+id+"/exec", "token-a", map[string]any{"cmd": "echo hi"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("in-cap exec = %d (%v), want 200", resp.StatusCode, ok)
	}
}

// TestExecBodyLimitDoesNotResumeIdleSuspended: an oversize exec body is
// refused before ensureRunning, so it must not wake an idle-suspended
// lease.
func TestExecBodyLimitDoesNotResumeIdleSuspended(t *testing.T) {
	withExecBodyLimit(t, 1<<10)
	ts, svc, _, _ := newTestServerWithService(t)
	ctx := context.Background()

	l, err := svc.grant(ctx, "consumer-a", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.setIdlePolicy(l, 60); err != nil {
		t.Fatalf("setIdlePolicy: %v", err)
	}
	if _, err := svc.pauseLease(ctx, l, false); err != nil {
		t.Fatalf("pause: %v", err)
	}
	svc.store.mu.Lock()
	l.LastAction, l.LastActionAt = idleSuspendRule+"/"+heldActionSuspendIdle, time.Now()
	l.LastActive = time.Now().Add(-2 * time.Minute)
	svc.saveLeaseLocked(l)
	svc.store.mu.Unlock()
	if !l.Suspended {
		t.Fatal("precondition: lease not suspended")
	}

	oversize, err := json.Marshal(map[string]any{"cmd": strings.Repeat("a", 2<<10)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, _ := http.NewRequest("POST", ts.URL+"/api/leases/"+l.ID+"/exec", strings.NewReader(string(oversize)))
	req.Header.Set("Authorization", "Bearer token-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("oversize exec: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize exec = %d, want 413", resp.StatusCode)
	}
	if !l.Suspended {
		t.Fatal("an oversize exec body resumed the idle-suspended lease")
	}

	// A normal exec still auto-resumes and serves.
	resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+l.ID+"/exec", "token-a", map[string]any{"cmd": "echo hi"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("in-cap exec after refusal = %d (%v), want 200", resp.StatusCode, body)
	}
	if l.Suspended {
		t.Fatal("the lease was not resumed by the in-cap exec")
	}
}

// TestStreamFirstFrameLimit: the stream's first message is bounded, so
// an oversize first frame closes the socket with 1009 instead of being
// buffered whole.
func TestStreamFirstFrameLimit(t *testing.T) {
	withExecBodyLimit(t, 1<<10)
	ts, _, _, _ := newTestServerWithService(t)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)

	ws := dialStream(t, ts, id, bearerHeader("token-a", ""))
	frame, err := json.Marshal(map[string]any{"args": []string{"bash", strings.Repeat("a", 2<<10)}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := ws.WriteMessage(websocket.TextMessage, frame); err != nil {
		t.Fatalf("write oversize frame: %v", err)
	}
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, _, err = ws.ReadMessage()
		if err != nil {
			break
		}
	}
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("read after oversize frame: %v, want a close error", err)
	}
	if closeErr.Code != websocket.CloseMessageTooBig {
		t.Fatalf("close code = %d, want %d (message too big)", closeErr.Code, websocket.CloseMessageTooBig)
	}
}
