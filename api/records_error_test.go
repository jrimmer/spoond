package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jrimmer/spoond/forkd"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// newRecordHTTPServer builds a *Server so tests can call Server methods
// (e.g. writeRecordError) directly.
func newRecordHTTPServer(t *testing.T) *Server {
	t.Helper()
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"token-a": "consumer-a"}, 0, 60*time.Second, 10*time.Minute, "py-base")
	reg := NewImageRegistry(ff, "py-base")
	return NewServer(svc, reg)
}

// TestRecordStartSuspended verifies a suspended (workspace-backed,
// persistent) sandbox rejects record start with 409.
func TestRecordStartSuspended(t *testing.T) {
	ts, _ := newTestServer(t)
	_, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 600, "persistent": true})
	sbID := body["id"].(string)

	if resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/"+sbID+"/suspend", "token-a", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("suspend: %d", resp.StatusCode)
	}
	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes/"+sbID+"/record/start", "token-a", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("record start on suspended sandbox: %d, want 409 (%v)", resp.StatusCode, body)
	}
}

// TestRecordStopSuspended verifies stopping a record whose sandbox was
// suspended mid-run returns 409 (not a double-branch or a 500).
func TestRecordStopSuspended(t *testing.T) {
	ts, _ := newTestServer(t)
	_, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 600, "persistent": true})
	sbID := body["id"].(string)
	_, body = doReq(t, "POST", ts.URL+"/api/sandboxes/"+sbID+"/record/start", "token-a", nil)
	recID := body["id"].(string)

	if resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/"+sbID+"/suspend", "token-a", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("suspend: %d", resp.StatusCode)
	}
	resp, _ := doReq(t, "POST", ts.URL+"/api/records/"+recID+"/stop", "token-a", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("record stop on suspended sandbox: %d, want 409", resp.StatusCode)
	}
}

// TestRecordStopSandboxGone verifies stopping a record whose sandbox was
// deleted returns 404 (the before checkpoint remains replayable).
func TestRecordStopSandboxGone(t *testing.T) {
	ts, _ := newTestServer(t)
	_, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 600})
	sbID := body["id"].(string)
	_, body = doReq(t, "POST", ts.URL+"/api/sandboxes/"+sbID+"/record/start", "token-a", nil)
	recID := body["id"].(string)

	if resp, _ := doReq(t, "DELETE", ts.URL+"/api/sandboxes/"+sbID, "token-a", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete sandbox: %d", resp.StatusCode)
	}
	resp, _ := doReq(t, "POST", ts.URL+"/api/records/"+recID+"/stop", "token-a", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("record stop after sandbox delete: %d, want 404", resp.StatusCode)
	}
}

// TestRecordReplayGCSnapshot verifies replay of a checkpoint whose
// snapshot was GC'd surfaces forkd's 404 as 404 (not a generic 500).
func TestRecordReplayGCSnapshot(t *testing.T) {
	ts, ff := newTestServer(t)
	_, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 600})
	sbID := body["id"].(string)
	_, body = doReq(t, "POST", ts.URL+"/api/sandboxes/"+sbID+"/record/start", "token-a", nil)
	recID := body["id"].(string)
	_, _ = doReq(t, "POST", ts.URL+"/api/records/"+recID+"/stop", "token-a", nil)

	ff.snapshots = nil // simulate the checkpoint being GC'd

	resp, body := doReq(t, "POST", ts.URL+"/api/records/"+recID+"/replay", "token-a", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("replay GC'd snapshot: %d, want 404 (%v)", resp.StatusCode, body)
	}
	if body["error"] != "snapshot no longer exists" {
		t.Fatalf("error body = %v, want 'snapshot no longer exists'", body["error"])
	}
}

// TestRecordQuotaExceeded verifies the per-owner record cap maps to 429.
func TestRecordQuotaExceeded(t *testing.T) {
	ts, svc := newRecordServer(t)
	svc.store.mu.Lock()
	for i := 0; i < maxRecordsPerOwner; i++ {
		id := fmt.Sprintf("rec-%03d", i)
		svc.store.records[id] = &Record{ID: id, Owner: "consumer-a"}
	}
	svc.store.mu.Unlock()

	_, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 600})
	sbID := body["id"].(string)
	resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/"+sbID+"/record/start", "token-a", nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("record start over cap: %d, want 429", resp.StatusCode)
	}
}

// TestWriteRecordErrorMapping unit-tests the sentinel-error -> HTTP status
// mapping (including the forkd.Error 404 downcast through a wrapping
// error) without going through the full HTTP stack.
func TestWriteRecordErrorMapping(t *testing.T) {
	srv := newRecordHTTPServer(t)
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"not found", errRecordNotFound, http.StatusNotFound},
		{"sandbox gone", errRecordSandboxGone, http.StatusNotFound},
		{"suspended", errRecordSuspended, http.StatusConflict},
		{"stopped", errRecordStopped, http.StatusConflict},
		{"already recording", errRecordAlreadyRecording, http.StatusConflict},
		{"label too long", errRecordLabelTooLong, http.StatusBadRequest},
		{"lease quota", errQuotaExceeded, http.StatusTooManyRequests},
		{"record quota", errRecordQuota, http.StatusTooManyRequests},
		{"forkd 404", &forkd.Error{StatusCode: http.StatusNotFound, Message: "gone"}, http.StatusNotFound},
		{"wrapped forkd 404", fmt.Errorf("create workspace: %w", &forkd.Error{StatusCode: http.StatusNotFound, Message: "gone"}), http.StatusNotFound},
		{"forkd 500", &forkd.Error{StatusCode: http.StatusInternalServerError, Message: "boom"}, http.StatusInternalServerError},
		{"generic", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			srv.writeRecordError(w, "test", tc.err)
			if w.Code != tc.want {
				t.Fatalf("writeRecordError(%v) = %d, want %d", tc.err, w.Code, tc.want)
			}
		})
	}
}

// TestRecordStopConcurrent verifies the atomic stop claim: exactly one of
// N concurrent stops branches and closes the record; the rest get 409 and
// the active gauge is not double-decremented.
func TestRecordStopConcurrent(t *testing.T) {
	ts, svc := newRecordServer(t)
	_, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 600})
	sbID := body["id"].(string)
	_, body = doReq(t, "POST", ts.URL+"/api/sandboxes/"+sbID+"/record/start", "token-a", nil)
	recID := body["id"].(string)

	const n = 32
	var ok, conflict int32
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("POST", ts.URL+"/api/records/"+recID+"/stop", nil)
			req.Header.Set("Authorization", "Bearer token-a")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("concurrent stop: %v", err)
				return
			}
			resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusOK:
				atomic.AddInt32(&ok, 1)
			case http.StatusConflict:
				atomic.AddInt32(&conflict, 1)
			}
		}()
	}
	wg.Wait()

	if ok != 1 {
		t.Fatalf("successful stops = %d, want exactly 1", ok)
	}
	if conflict != n-1 {
		t.Fatalf("conflict stops = %d, want %d", conflict, n-1)
	}
	if got := testutil.ToFloat64(svc.metrics.RecordsActive); got != 0 {
		t.Fatalf("RecordsActive = %v, want 0 (no double-decrement)", got)
	}
}
