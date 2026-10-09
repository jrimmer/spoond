package api

// Resume on the holder's next work call (#145 D2). Every lease
// automatically suspended — pressure, preemption, the idle sweep,
// idle_suspend, a lapsed hold (rule 3) — and one its holder suspended by
// hand resumes when the holder next uses it. A no-room refusal is the one
// 503 capacity_wait shape with Retry-After on every path. GET, status,
// events and SSE never resume. A failed resume is never turned into a
// lost lease.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/jrimmer/spoond/v2/substrate"
)

// rawWork posts a JSON body and returns the response, the raw body text
// and the headers, without decoding (so a non-JSON refusal is visible).
func rawWork(t *testing.T, ts *httptest.Server, method, path, token, jsonBody string) (*http.Response, string, http.Header) {
	t.Helper()
	var rdr io.Reader
	if jsonBody != "" {
		rdr = strings.NewReader(jsonBody)
	}
	req, err := http.NewRequest(method, ts.URL+path, rdr)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b), resp.Header
}

// resumeReason is one suspension a work call must undo by resuming.
type resumeReason struct {
	name    string
	suspend func(t *testing.T, svc *Service, ctx context.Context, l *Lease)
}

// resumeReasonCases covers one automatic or manual suspension per distinct
// reason. Each pause reasons through the shared pause path, matching the
// production caller so the structured facts a resume clears are exercised.
func resumeReasonCases() []resumeReason {
	return []resumeReason{
		{"idle", func(t *testing.T, svc *Service, ctx context.Context, l *Lease) {
			if _, err := svc.pauseLeaseWith(ctx, l, false, suspendPolicy{reason: suspendReasonIdle}); err != nil {
				t.Fatalf("idle suspend: %v", err)
			}
		}},
		{"idle_suspend", func(t *testing.T, svc *Service, ctx context.Context, l *Lease) {
			if _, err := svc.pauseLeaseWith(ctx, l, false, suspendPolicy{reason: suspendReasonIdleSuspend}); err != nil {
				t.Fatalf("idle_suspend suspend: %v", err)
			}
		}},
		{"hold_lapsed", func(t *testing.T, svc *Service, ctx context.Context, l *Lease) {
			if _, err := svc.pauseLeaseWith(ctx, l, false, suspendPolicy{reason: suspendReasonHoldLapsed}); err != nil {
				t.Fatalf("hold_lapsed suspend: %v", err)
			}
		}},
		{"pressure", func(t *testing.T, svc *Service, ctx context.Context, l *Lease) {
			if _, err := svc.pauseLeaseWith(ctx, l, false, suspendPolicy{reason: suspendReasonPressure}); err != nil {
				t.Fatalf("pressure suspend: %v", err)
			}
		}},
		{"preempt", func(t *testing.T, svc *Service, ctx context.Context, l *Lease) {
			svc.store.mu.Lock()
			l.Class = ClassBurst
			svc.store.mu.Unlock()
			if err := svc.preemptLease(ctx, l, "another-owner"); err != nil {
				t.Fatalf("preempt: %v", err)
			}
			if l.PreemptedAt.IsZero() {
				t.Fatal("preempt did not stamp preempted_at")
			}
		}},
		{"hand", func(t *testing.T, svc *Service, ctx context.Context, l *Lease) {
			// suspend() is the owner's hand suspend: the zero policy.
			if _, err := svc.suspend(ctx, "consumer-a", l.ID); err != nil {
				t.Fatalf("hand suspend: %v", err)
			}
		}},
	}
}

// TestResumeOnUsePerSuspendReason: an exec on a lease suspended for any
// reason resumes it and serves the call, never 409 lease_suspended.
func TestResumeOnUsePerSuspendReason(t *testing.T) {
	for _, tc := range resumeReasonCases() {
		t.Run(tc.name, func(t *testing.T) {
			ts, svc, _, _ := newTestServerWithService(t)
			ctx := context.Background()
			// Huge node so a resume always fits.
			svc.sub.(*testSub).SetNodeInfo(substrate.NodeInfo{
				Status:            "healthy",
				HugepagesTotal:    1 << 20,
				HugepageSizeBytes: 2 << 20,
			}, nil)

			l, err := svc.grant(ctx, "consumer-a", "py-base", time.Hour, true, "", nil, "", "", nil)
			if err != nil {
				t.Fatalf("grant: %v", err)
			}
			tc.suspend(t, svc, ctx, l)
			if !l.Suspended {
				t.Fatal("precondition: lease not suspended")
			}

			resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+l.ID+"/exec", "token-a",
				map[string]any{"cmd": "echo hi"})
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("exec on a %s-suspended lease: %d: %v", tc.name, resp.StatusCode, body)
			}
			if l.Suspended || !l.live() {
				t.Fatalf("lease not resumed: state=%s suspended=%v", l.State, l.Suspended)
			}
		})
	}
}

// TestResumeOnUseGetNeverResumes: a GET (lease detail, list, stat) never
// resumes a suspended lease. GET is not a work call.
func TestResumeOnUseGetNeverResumes(t *testing.T) {
	ts, svc, _, _ := newTestServerWithService(t)
	ctx := context.Background()
	l, err := svc.grant(ctx, "consumer-a", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.suspend(ctx, "consumer-a", l.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	for _, path := range []string{
		"/api/leases/" + l.ID,
		"/api/sandboxes/" + l.ID,
		"/api/sandboxes",
		"/api/leases/" + l.ID + "/events",
	} {
		resp, _ := doReq(t, "GET", ts.URL+path, "token-a", nil)
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
			t.Fatalf("GET %s = %d, want 200", path, resp.StatusCode)
		}
		if !l.Suspended {
			t.Fatalf("GET %s resumed the lease; GET must never resume", path)
		}
	}
}

// TestResumeOnUseNoRoomCapacityWait: a preempted lease whose resume finds
// no room answers 503 with Retry-After and code capacity_wait, stays
// suspended, and is not marked lost.
func TestResumeOnUseNoRoomCapacityWait(t *testing.T) {
	srv, h, sub, tok, uid := newClassServer(t, map[string]int{"mid": 1024}, `{"max_mib":8192}`)
	svc := srv.svc
	ctx := context.Background()

	rec, first := createPersistentAs(t, h, tok, "mid")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	id := first["id"].(string)
	l := svc.lookup(uid, id)
	l.Burst = true
	if _, err := svc.suspend(ctx, uid, id); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	// Mark it preempted so this is the preemption-reason resume.
	svc.store.mu.Lock()
	l.PreemptedAt = time.Now()
	l.Class = ClassBurst
	svc.store.mu.Unlock()

	// No room: the burst reserve cannot be met.
	svc.cfg.BurstReserveMiB = 8192
	sub.SetNodeInfo(substrate.NodeInfo{Status: "healthy", HugepagesTotal: 512 + 1, HugepageSizeBytes: 2 << 20}, nil)
	svc.nodeInfoMu.Lock()
	svc.nodeInfoAt = time.Time{}
	svc.nodeInfoMu.Unlock()

	rec2 := postLeaseAction(t, h, tok, "/api/leases/"+id+"/exec", `{"cmd":"echo hi"}`)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("exec auto-resume no-room = %d, want 503: %s", rec2.Code, rec2.Body.String())
	}
	if ra := rec2.Header().Get("Retry-After"); ra != "30" {
		t.Fatalf("Retry-After = %q, want 30", ra)
	}
	if !strings.Contains(rec2.Body.String(), `"code":"capacity_wait"`) {
		t.Fatalf("body = %s, want code capacity_wait", rec2.Body.String())
	}
	if !l.Suspended {
		t.Fatal("a refused auto-resume left the lease running")
	}
	if l.State == "lost" || !l.LostAt.IsZero() {
		t.Fatalf("a failed resume marked the lease lost: state=%s lostAt=%v", l.State, l.LostAt)
	}
}

// workPath is one HTTP request that must resume a suspended lease on use
// and answer the identical 503 capacity_wait shape when there is no room.
type workPath struct {
	name string
	do   func(t *testing.T, ts *httptest.Server, srv *Server, id string) (*http.Response, string, http.Header)
}

// TestResumeOnUseNoRoomEveryPath is the one table test the contract asks
// for: every resume-on-use path answers a no-room refusal identically —
// 503, Retry-After, JSON {"error": ..., "code": "capacity_wait"} — and
// never 409 lease_suspended.
func TestResumeOnUseNoRoomEveryPath(t *testing.T) {
	paths := []workPath{
		{"exec", func(t *testing.T, ts *httptest.Server, _ *Server, id string) (*http.Response, string, http.Header) {
			return rawWork(t, ts, "POST", "/api/leases/"+id+"/exec", "token-a", `{"cmd":"echo hi"}`)
		}},
		{"stream", func(t *testing.T, ts *httptest.Server, _ *Server, id string) (*http.Response, string, http.Header) {
			wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/leases/" + id + "/stream"
			_, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer token-a"}})
			if err == nil {
				t.Fatal("stream dial succeeded, want a refusal")
			}
			if resp == nil {
				t.Fatal("stream dial returned no HTTP response")
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return resp, string(b), resp.Header
		}},
		{"files", func(t *testing.T, ts *httptest.Server, _ *Server, id string) (*http.Response, string, http.Header) {
			return rawWork(t, ts, "GET", "/api/leases/"+id+"/files/f.txt", "token-a", "")
		}},
		{"proxy", func(t *testing.T, _ *httptest.Server, srv *Server, id string) (*http.Response, string, http.Header) {
			req := httptest.NewRequest("GET", "http://"+id+"-3000.sandbox.example.com/", nil)
			rec := httptest.NewRecorder()
			srv.ProxyHandler().ServeHTTP(rec, req)
			return rec.Result(), rec.Body.String(), rec.Header()
		}},
		{"jobs", func(t *testing.T, ts *httptest.Server, _ *Server, id string) (*http.Response, string, http.Header) {
			return rawWork(t, ts, "POST", "/api/leases/"+id+"/jobs/job-any/signal", "token-a", `{"signal":"TERM"}`)
		}},
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			ts, svc, db, sub := newTestServerWithService(t)
			// A burst lease with no room so the resume is refused on the
			// reserve, the deterministic no-room refusal.
			l, err := svc.grantLease(context.Background(), leaseRequest{owner: "consumer-a", image: "py-base", ttl: time.Hour, persistent: true, burst: true})
			if err != nil {
				t.Fatalf("grant: %v", err)
			}
			if _, err := svc.suspend(context.Background(), "consumer-a", l.ID); err != nil {
				t.Fatalf("suspend: %v", err)
			}
			svc.cfg.BurstReserveMiB = 1 << 20
			sub.SetNodeInfo(substrate.NodeInfo{Status: "healthy", HugepagesTotal: 4096, HugepageSizeBytes: 2 << 20}, nil)
			svc.nodeInfoMu.Lock()
			svc.nodeInfoAt = time.Time{}
			svc.nodeInfoMu.Unlock()

			srv := NewServer(svc, NewImageRegistry(db))
			resp, body, hdr := p.do(t, ts, srv, l.ID)
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("%s no-room = %d, want 503: %s", p.name, resp.StatusCode, body)
			}
			if ra := hdr.Get("Retry-After"); ra != "30" {
				t.Fatalf("%s Retry-After = %q, want 30", p.name, ra)
			}
			var doc map[string]any
			if err := json.Unmarshal([]byte(body), &doc); err != nil {
				t.Fatalf("%s body is not JSON: %v (%s)", p.name, err, body)
			}
			if doc["code"] != "capacity_wait" {
				t.Fatalf("%s code = %v, want capacity_wait: %s", p.name, doc["code"], body)
			}
			if _, ok := doc["error"]; !ok {
				t.Fatalf("%s body has no error: %s", p.name, body)
			}
			if !l.Suspended {
				t.Fatalf("%s resumed the lease despite no room", p.name)
			}
		})
	}
}

// TestResumeOnUseLLMNoRoom: the LLM gateway shares the same no-room shape
// as every other resume-on-use path.
func TestResumeOnUseLLMNoRoom(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	srv := NewServerWithLLM(svc, NewImageRegistry(db), "http://127.0.0.1:1", "host-key", "", nil)
	h := httptest.NewServer(srv.Handler())
	t.Cleanup(h.Close)

	l, err := svc.grantLease(context.Background(), leaseRequest{owner: "consumer-a", image: "py-base", ttl: time.Hour, persistent: true, burst: true})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.suspend(context.Background(), "consumer-a", l.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	svc.cfg.BurstReserveMiB = 1 << 20
	sub.SetNodeInfo(substrate.NodeInfo{Status: "healthy", HugepagesTotal: 4096, HugepageSizeBytes: 2 << 20}, nil)
	svc.nodeInfoMu.Lock()
	svc.nodeInfoAt = time.Time{}
	svc.nodeInfoMu.Unlock()

	req := httptest.NewRequest("POST", llmGatewayPrefix+l.ID+"/openai/chat/completions", strings.NewReader(`{"model":"m"}`))
	rec := httptest.NewRecorder()
	srv.llm.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("llm no-room = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if ra := rec.Header().Get("Retry-After"); ra != "30" {
		t.Fatalf("llm Retry-After = %q, want 30", ra)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(rec.Body.String()), &doc); err != nil {
		t.Fatalf("llm body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if doc["code"] != "capacity_wait" {
		t.Fatalf("llm code = %v, want capacity_wait: %s", doc["code"], rec.Body.String())
	}
	if !l.Suspended {
		t.Fatal("llm resumed the lease despite no room")
	}
}

// TestResumeOnUseBusyCode: while another resume is in flight a second
// work call answers 409 with code lease_busy (retryable), never
// lease_suspended.
func TestResumeOnUseBusyCode(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	sub.SetNodeInfo(substrate.NodeInfo{Status: "healthy", HugepagesTotal: 1 << 20, HugepageSizeBytes: 2 << 20}, nil)
	l, err := svc.grant(context.Background(), "consumer-a", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.pauseLease(context.Background(), l, false); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// Mark the lease busy as an in-flight resume would.
	svc.store.mu.Lock()
	l.busy = true
	svc.store.mu.Unlock()
	t.Cleanup(func() {
		svc.store.mu.Lock()
		l.busy = false
		svc.store.mu.Unlock()
	})

	resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+l.ID+"/exec", "token-a",
		map[string]any{"cmd": "echo hi"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("busy exec = %d, want 409: %v", resp.StatusCode, body)
	}
	if body["code"] != "lease_busy" {
		t.Fatalf("busy code = %v, want lease_busy: %v", body["code"], body)
	}
}
