package api

// Structural-capacity contract (#145 gkx2): every path that meets a
// substrate capacity shortage while granting or snapshotting answers the
// same shape — 503 with Retry-After: 30 and JSON code "capacity_wait" —
// and never a bare 503 without a code. 429 box_full is a different
// refusal and stays as it is on every path.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v3/substrate"
)

// expectCapacityWait asserts the shared structural refusal: 503, a
// Retry-After of 30 seconds and JSON code "capacity_wait".
func expectCapacityWait(t *testing.T, resp *http.Response, body map[string]any) {
	t.Helper()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d (%v), want 503", resp.StatusCode, body)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "30" {
		t.Fatalf("Retry-After = %q, want 30", ra)
	}
	if code, _ := body["code"].(string); code != "capacity_wait" {
		t.Fatalf("code = %v, want capacity_wait: %v", body["code"], body)
	}
}

// TestCapacityWaitEveryPath drives create, clone, fork, restore, restart,
// checkpoint and a files call into a substrate capacity refusal and pins
// the one answer they all owe.
func TestCapacityWaitEveryPath(t *testing.T) {
	ctx := context.Background()
	refuseCreate := func(context.Context, substrate.CreateRequest) (substrate.Sandbox, error) {
		return substrate.Sandbox{}, substrate.ErrCapacity
	}

	t.Run("create", func(t *testing.T) {
		ts, _, _, sub := newTestServerWithService(t)
		sub.createFn = refuseCreate
		t.Cleanup(func() { sub.createFn = nil })
		resp, body := doReq(t, "POST", ts.URL+"/api/leases", "token-a",
			map[string]any{"image": "py-base", "ttl": 300})
		expectCapacityWait(t, resp, body)
	})

	t.Run("clone", func(t *testing.T) {
		ts, svc, _, sub := newTestServerWithService(t)
		l, err := svc.grant(ctx, "consumer-a", "py-base", time.Minute, true, "", nil, "", "", nil)
		if err != nil {
			t.Fatalf("grant: %v", err)
		}
		sub.createFn = refuseCreate
		t.Cleanup(func() { sub.createFn = nil })
		resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+l.ID+"/clone", "token-a", nil)
		expectCapacityWait(t, resp, body)
	})

	t.Run("fork", func(t *testing.T) {
		ts, svc, _, sub := newTestServerWithService(t)
		l, err := svc.grant(ctx, "consumer-a", "py-base", time.Minute, true, "", nil, "", "", nil)
		if err != nil {
			t.Fatalf("grant: %v", err)
		}
		sub.createFn = refuseCreate
		t.Cleanup(func() { sub.createFn = nil })
		resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+l.ID+"/fork", "token-a",
			map[string]any{"count": 1})
		expectCapacityWait(t, resp, body)
	})

	t.Run("restore", func(t *testing.T) {
		ts, svc, _, sub := newTestServerWithService(t)
		l, err := svc.grant(ctx, "consumer-a", "py-base", time.Minute, true, "", nil, "", "", nil)
		if err != nil {
			t.Fatalf("grant: %v", err)
		}
		b, err := svc.checkpointLease(ctx, l)
		if err != nil {
			t.Fatalf("checkpoint: %v", err)
		}
		sub.createFn = refuseCreate
		t.Cleanup(func() { sub.createFn = nil })
		resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+l.ID+"/restore", "token-a",
			map[string]any{"build_id": b.BuildID})
		expectCapacityWait(t, resp, body)
	})

	t.Run("restart", func(t *testing.T) {
		ts, svc, _, sub := newTestServerWithService(t)
		l, err := svc.grant(ctx, "consumer-a", "py-base", time.Minute, true, "", nil, "", "", nil)
		if err != nil {
			t.Fatalf("grant: %v", err)
		}
		sub.createFn = refuseCreate
		t.Cleanup(func() { sub.createFn = nil })
		resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+l.ID+"/restart?mode=cold", "token-a", nil)
		expectCapacityWait(t, resp, body)
	})

	t.Run("checkpoint", func(t *testing.T) {
		ts, svc, _, sub := newTestServerWithService(t)
		l, err := svc.grant(ctx, "consumer-a", "py-base", time.Minute, true, "", nil, "", "", nil)
		if err != nil {
			t.Fatalf("grant: %v", err)
		}
		sub.checkpointFn = func(context.Context, string) (string, substrate.BuildRefs, error) {
			return "", substrate.BuildRefs{}, substrate.ErrCapacity
		}
		t.Cleanup(func() { sub.checkpointFn = nil })
		resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+l.ID+"/checkpoint", "token-a", nil)
		expectCapacityWait(t, resp, body)
	})

	t.Run("files", func(t *testing.T) {
		ts, svc, _, sub := newTestServerWithService(t)
		l, err := svc.grant(ctx, "consumer-a", "py-base", time.Minute, false, "", nil, "", "", nil)
		if err != nil {
			t.Fatalf("grant: %v", err)
		}
		sub.FailCall("Stat", 0, substrate.ErrCapacity)
		resp := filesDo(t, "GET", filesURL(ts, l.ID, "/f.txt", "stat=1"), "token-a", "")
		defer resp.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil && err != io.EOF {
			t.Fatalf("decode: %v", err)
		}
		expectCapacityWait(t, resp, body)
	})
}
