package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// newRecordServer builds a test server and returns the *Service so tests
// can assert on service-owned metrics.
func newRecordServer(t *testing.T) (*httptest.Server, *Service) {
	t.Helper()
	ff := newFakeForkd()
	svc := NewService(ff, map[string]string{"token-a": "consumer-a", "token-b": "consumer-b"}, 0, 60*time.Second, 10*time.Minute)
	reg := NewImageRegistry(ff, "py-base")
	srv := NewServer(svc, reg)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, svc
}

// TestRecordStartStopReplay exercises the full record/replay lifecycle
// and asserts replay spawns from the AFTER checkpoint.
func TestRecordStartStopReplay(t *testing.T) {
	ts, _ := newTestServer(t)

	resp, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 600})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	sbID := body["id"].(string)

	resp, body = doReq(t, "POST", ts.URL+"/api/sandboxes/"+sbID+"/record/start", "token-a", map[string]any{"label": "my-run"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("record start: %d %v", resp.StatusCode, body)
	}
	recID := body["id"].(string)
	beforeTag := body["before_tag"].(string)
	if recID == "" || beforeTag == "" {
		t.Fatalf("record start returned no id/before_tag: %v", body)
	}
	if body["label"] != "my-run" {
		t.Fatalf("label = %v, want my-run", body["label"])
	}

	resp, body = doReq(t, "POST", ts.URL+"/api/records/"+recID+"/stop", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("record stop: %d %v", resp.StatusCode, body)
	}
	afterTag := body["after_tag"].(string)
	if afterTag == "" || afterTag == beforeTag {
		t.Fatalf("after_tag = %q (before %q), want a distinct non-empty tag", afterTag, beforeTag)
	}

	// Replay must spawn from the AFTER checkpoint, not the before.
	resp, body = doReq(t, "POST", ts.URL+"/api/records/"+recID+"/replay", "token-a", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("record replay: %d %v", resp.StatusCode, body)
	}
	if body["id"].(string) == "" {
		t.Fatal("record replay returned no sandbox id")
	}
	if body["image"] != afterTag {
		t.Errorf("replay image = %v, want after_tag %q", body["image"], afterTag)
	}
	if body["source"] != recID || body["branch_tag"] != afterTag {
		t.Errorf("replay source/branch_tag = %v/%v, want %q/%q", body["source"], body["branch_tag"], recID, afterTag)
	}

	resp, body = doReq(t, "GET", ts.URL+"/api/records", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("records list: %d", resp.StatusCode)
	}
	if records := body["records"].([]any); len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}

	resp, _ = doReq(t, "DELETE", ts.URL+"/api/records/"+recID, "token-a", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("record delete: %d", resp.StatusCode)
	}
	if resp, _ = doReq(t, "GET", ts.URL+"/api/records/"+recID, "token-a", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: %d, want 404", resp.StatusCode)
	}
}

// TestRecordReplayBeforeStop verifies the before-checkpoint fallback for
// a run that was never stopped.
func TestRecordReplayBeforeStop(t *testing.T) {
	ts, _ := newTestServer(t)
	_, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 600})
	sbID := body["id"].(string)
	_, body = doReq(t, "POST", ts.URL+"/api/sandboxes/"+sbID+"/record/start", "token-a", nil)
	recID := body["id"].(string)
	beforeTag := body["before_tag"].(string)

	resp, body := doReq(t, "POST", ts.URL+"/api/records/"+recID+"/replay", "token-a", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("replay: %d", resp.StatusCode)
	}
	if body["image"] != beforeTag {
		t.Errorf("replay image = %v, want before_tag %q", body["image"], beforeTag)
	}
}

// TestRecordOwnerScoping verifies records are owner-scoped across read,
// stop, replay, and delete, and that cross-owner lists are empty.
func TestRecordOwnerScoping(t *testing.T) {
	ts, _ := newTestServer(t)
	_, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 600})
	sbID := body["id"].(string)
	_, body = doReq(t, "POST", ts.URL+"/api/sandboxes/"+sbID+"/record/start", "token-a", nil)
	recID := body["id"].(string)

	// token-b cannot get, stop, replay, or delete token-a's record.
	if resp, _ := doReq(t, "GET", ts.URL+"/api/records/"+recID, "token-b", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-owner get: %d, want 404", resp.StatusCode)
	}
	if resp, _ := doReq(t, "POST", ts.URL+"/api/records/"+recID+"/stop", "token-b", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-owner stop: %d, want 404", resp.StatusCode)
	}
	if resp, _ := doReq(t, "POST", ts.URL+"/api/records/"+recID+"/replay", "token-b", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-owner replay: %d, want 404", resp.StatusCode)
	}
	if resp, _ := doReq(t, "DELETE", ts.URL+"/api/records/"+recID, "token-b", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-owner delete: %d, want 404", resp.StatusCode)
	}
	// token-b's list is empty.
	resp, body := doReq(t, "GET", ts.URL+"/api/records", "token-b", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cross-owner list: %d", resp.StatusCode)
	}
	if records := body["records"].([]any); len(records) != 0 {
		t.Fatalf("token-b list = %d records, want 0", len(records))
	}
}

func TestRecordStopTwiceConflict(t *testing.T) {
	ts, _ := newTestServer(t)
	_, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 600})
	sbID := body["id"].(string)
	_, body = doReq(t, "POST", ts.URL+"/api/sandboxes/"+sbID+"/record/start", "token-a", nil)
	recID := body["id"].(string)

	if resp, _ := doReq(t, "POST", ts.URL+"/api/records/"+recID+"/stop", "token-a", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("first stop: %d", resp.StatusCode)
	}
	if resp, _ := doReq(t, "POST", ts.URL+"/api/records/"+recID+"/stop", "token-a", nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("second stop: %d, want 409", resp.StatusCode)
	}
}

func TestRecordStartMissingSandbox(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/does-not-exist/record/start", "token-a", map[string]any{"label": "x"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("start missing sandbox: %d, want 404", resp.StatusCode)
	}
}

// TestRecordStartTwiceConflict verifies the open-record guard: a sandbox
// can have at most one open record at a time.
func TestRecordStartTwiceConflict(t *testing.T) {
	ts, _ := newTestServer(t)
	_, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 600})
	sbID := body["id"].(string)
	if resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/"+sbID+"/record/start", "token-a", nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first start: %d", resp.StatusCode)
	}
	if resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/"+sbID+"/record/start", "token-a", nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("second start: %d, want 409", resp.StatusCode)
	}
}

// TestRecordLabelTooLong verifies over-long labels are rejected (not
// silently truncated).
func TestRecordLabelTooLong(t *testing.T) {
	ts, _ := newTestServer(t)
	_, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 600})
	sbID := body["id"].(string)
	resp, _ := doReq(t, "POST", ts.URL+"/api/sandboxes/"+sbID+"/record/start", "token-a", map[string]any{"label": strings.Repeat("a", 200)})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("long label: %d, want 400", resp.StatusCode)
	}
}

func TestRecordTag(t *testing.T) {
	tag := recordTag("abcdef0123456789abcdef0123456789", "before")
	if !strings.HasPrefix(tag, "rec-before-abcdef012345-") {
		t.Errorf("recordTag = %q, want rec-before-<12hex>-<unix>", tag)
	}
	// Different record ids yield different tags.
	if tag2 := recordTag("fedcba9876543210fedcba9876543210", "before"); tag2 == tag {
		t.Error("different record ids produced the same tag")
	}
	// before vs after phases differ.
	if a := recordTag("abcdef0123456789abcdef0123456789", "after"); a == tag {
		t.Error("before/after phases produced the same tag")
	}
}

// TestRecordMetrics asserts the record counters/gauges move through the
// lifecycle (issue #55).
func TestRecordMetrics(t *testing.T) {
	ts, svc := newRecordServer(t)
	_, body := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 600})
	sbID := body["id"].(string)
	_, body = doReq(t, "POST", ts.URL+"/api/sandboxes/"+sbID+"/record/start", "token-a", nil)
	recID := body["id"].(string)

	if got := testutil.ToFloat64(svc.metrics.RecordCreated); got != 1 {
		t.Errorf("RecordCreated = %v, want 1", got)
	}
	if got := testutil.ToFloat64(svc.metrics.RecordsActive); got != 1 {
		t.Errorf("RecordsActive = %v, want 1", got)
	}

	doReq(t, "POST", ts.URL+"/api/records/"+recID+"/replay", "token-a", nil)
	if got := testutil.ToFloat64(svc.metrics.RecordReplay); got != 1 {
		t.Errorf("RecordReplay = %v, want 1", got)
	}

	doReq(t, "POST", ts.URL+"/api/records/"+recID+"/stop", "token-a", nil)
	if got := testutil.ToFloat64(svc.metrics.RecordsActive); got != 0 {
		t.Errorf("RecordsActive after stop = %v, want 0", got)
	}
}
