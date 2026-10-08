package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
	svc.store.mu.Lock()
	before := l.LastActive
	svc.store.mu.Unlock()

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
	// An oversize request is refused before touch, so it must not count
	// as activity for the idle sweeper.
	svc.store.mu.Lock()
	after := l.LastActive
	svc.store.mu.Unlock()
	if !after.Equal(before) {
		t.Fatalf("oversize exec touched LastActive: %s -> %s", before, after)
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

// TestExecBodyLimitDefaultAcceptsLargeLegalBody: the documented default
// cap (8 MiB) leaves room for a large command and the full 64 KiB
// secrets budget in one request, so a legitimately large exec is not
// rejected at the default. This pins the cap's headroom: a 120 KiB cmd
// plus 64 KiB of secrets is a few hundred KiB, far under the default.
func TestExecBodyLimitDefaultAcceptsLargeLegalBody(t *testing.T) {
	if maxExecBodyBytes != defaultMaxExecBodyBytes {
		t.Fatalf("precondition: default cap = %d, want %d", maxExecBodyBytes, defaultMaxExecBodyBytes)
	}
	ts, _, _, _ := newTestServerWithService(t)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)

	secrets := map[string]string{}
	remaining := maxSecretsTotalBytes
	for i := 0; remaining > 0; i++ {
		n := remaining
		if n > 8<<10 {
			n = 8 << 10
		}
		secrets[fmt.Sprintf("s%02d", i)] = strings.Repeat("v", n)
		remaining -= n
	}
	large := map[string]any{
		"cmd":     strings.Repeat("a", 120<<10),
		"secrets": secrets,
	}
	raw, err := json.Marshal(large)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if int64(len(raw)) >= maxExecBodyBytes {
		t.Fatalf("test body is %d bytes, at or above the cap %d; it must be legal", len(raw), maxExecBodyBytes)
	}
	req, _ := http.NewRequest("POST", ts.URL+"/api/sandboxes/"+id+"/exec", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer token-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("large legal exec: %v", err)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("large legal exec = %d (%v), want 200", resp.StatusCode, body)
	}
}

// TestInvalidMaxExecBodyBytesWarns: an unusable MAX_EXEC_BODY_BYTES is
// reported once instead of silently falling back to the default cap.
func TestInvalidMaxExecBodyBytesWarns(t *testing.T) {
	var buf bytes.Buffer
	oldWriter, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
	})
	t.Setenv("MAX_EXEC_BODY_BYTES", "not-a-number")
	logInvalidMaxExecBodyBytes()
	if got := buf.String(); !strings.Contains(got, "MAX_EXEC_BODY_BYTES") || !strings.Contains(got, "invalid") {
		t.Fatalf("invalid cap warning = %q, want it to name the variable and the problem", got)
	}

	// A valid value and an unset variable both stay quiet.
	buf.Reset()
	t.Setenv("MAX_EXEC_BODY_BYTES", "1048576")
	logInvalidMaxExecBodyBytes()
	if got := buf.String(); got != "" {
		t.Fatalf("valid cap logged %q, want nothing", got)
	}
	buf.Reset()
	t.Setenv("MAX_EXEC_BODY_BYTES", "")
	logInvalidMaxExecBodyBytes()
	if got := buf.String(); got != "" {
		t.Fatalf("unset cap logged %q, want nothing", got)
	}
}
