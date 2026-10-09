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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/jrimmer/spoond/v2/identity"
	"github.com/jrimmer/spoond/v2/store"
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
// and answer the identical refusal when the lease cannot resume.
type workPath struct {
	name string
	do   func(t *testing.T, ts *httptest.Server, srv *Server, id, token string) (*http.Response, string, http.Header)
}

// resumeOnUseWorkPaths is the one table every resume-on-use refusal test
// shares: exec, exec stream, files, the proxy and a job signal. The
// per-path shape is the same whatever refused the resume.
func resumeOnUseWorkPaths() []workPath {
	return []workPath{
		{"exec", func(t *testing.T, ts *httptest.Server, _ *Server, id, token string) (*http.Response, string, http.Header) {
			return rawWork(t, ts, "POST", "/api/leases/"+id+"/exec", token, `{"cmd":"echo hi"}`)
		}},
		{"stream", func(t *testing.T, ts *httptest.Server, _ *Server, id, token string) (*http.Response, string, http.Header) {
			wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/leases/" + id + "/stream"
			_, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer " + token}})
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
		{"files", func(t *testing.T, ts *httptest.Server, _ *Server, id, token string) (*http.Response, string, http.Header) {
			return rawWork(t, ts, "GET", "/api/leases/"+id+"/files/f.txt", token, "")
		}},
		{"proxy", func(t *testing.T, _ *httptest.Server, srv *Server, id, _ string) (*http.Response, string, http.Header) {
			req := httptest.NewRequest("GET", "http://"+id+"-3000.sandbox.example.com/", nil)
			rec := httptest.NewRecorder()
			srv.ProxyHandler().ServeHTTP(rec, req)
			return rec.Result(), rec.Body.String(), rec.Header()
		}},
		{"jobs", func(t *testing.T, ts *httptest.Server, srv *Server, id, token string) (*http.Response, string, http.Header) {
			// A running job record so the signal passes the state check
			// (checked before the resume, review R5) and reaches the
			// resume-on-use. The guest is never reached: the resume is
			// refused before the signal exec.
			if err := srv.svc.db.InsertJob(context.Background(), store.JobRow{
				JobID: "job-any", LeaseID: id, Owner: "consumer-a", Cmd: "sleep 600",
				State: "running", StartedAt: time.Now(),
			}); err != nil {
				t.Fatalf("insert job: %v", err)
			}
			return rawWork(t, ts, "POST", "/api/leases/"+id+"/jobs/job-any/signal", token, `{"signal":"TERM"}`)
		}},
	}
}

// TestResumeOnUseNoRoomEveryPath is the one table test the contract asks
// for: every resume-on-use path answers a no-room refusal identically —
// 503, Retry-After, JSON {"error": ..., "code": "capacity_wait"} — and
// never 409 lease_suspended.
func TestResumeOnUseNoRoomEveryPath(t *testing.T) {
	paths := resumeOnUseWorkPaths()
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
			resp, body, hdr := p.do(t, ts, srv, l.ID, "token-a")
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

// TestResumeOnUseQuotaEveryPath: the owner's own memory quota refusal
// is the one shape every resume-on-use path shares too — 429 with a
// Retry-After header and JSON code quota_exceeded — and never 409
// lease_suspended (review R7). Deleting the code or the header from
// writeResumeRefusal's quota case makes this fail.
func TestResumeOnUseQuotaEveryPath(t *testing.T) {
	for _, p := range resumeOnUseWorkPaths() {
		t.Run(p.name, func(t *testing.T) {
			// Two big leases under a max_mib that fits one: the second
			// fills the cap so the suspended first cannot re-admit.
			srv, h, tok, uid := newMemQuotaServer(t, map[string]int{"big": 4096}, `{"max_mib":4096}`)
			svc := srv.svc
			rec, first := createPersistentAs(t, h, tok, "big")
			if rec.Code != http.StatusCreated {
				t.Fatalf("first create: %d %s", rec.Code, rec.Body.String())
			}
			id := first["id"].(string)
			l := svc.lookup(uid, id)
			if _, err := svc.suspend(context.Background(), ownerIDFor(t, h, tok), id); err != nil {
				t.Fatalf("suspend: %v", err)
			}
			if rec2, _ := createSandboxAs(t, h, tok, "big"); rec2.Code != http.StatusCreated {
				t.Fatalf("second create: %d %s", rec2.Code, rec2.Body.String())
			}

			ts := httptest.NewServer(h)
			t.Cleanup(ts.Close)
			resp, body, hdr := p.do(t, ts, srv, id, tok)
			if resp.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("%s quota refusal = %d, want 429: %s", p.name, resp.StatusCode, body)
			}
			if ra := hdr.Get("Retry-After"); ra != "30" {
				t.Fatalf("%s quota Retry-After = %q, want 30", p.name, ra)
			}
			var doc map[string]any
			if err := json.Unmarshal([]byte(body), &doc); err != nil {
				t.Fatalf("%s body is not JSON: %v (%s)", p.name, err, body)
			}
			if doc["code"] != "quota_exceeded" {
				t.Fatalf("%s code = %v, want quota_exceeded: %s", p.name, doc["code"], body)
			}
			if _, ok := doc["error"]; !ok {
				t.Fatalf("%s body has no error: %s", p.name, body)
			}
			if !l.Suspended {
				t.Fatalf("%s resumed the lease despite the quota", p.name)
			}
		})
	}
}

// TestResumeOnUseBusyEveryPath: while its pause or another caller's
// resume is in flight every work path answers 409 with code lease_busy
// (retryable), never 409 lease_suspended.
func TestResumeOnUseBusyEveryPath(t *testing.T) {
	for _, p := range resumeOnUseWorkPaths() {
		t.Run(p.name, func(t *testing.T) {
			srv, h, tok, uid := newMemQuotaServer(t, map[string]int{"big": 2048}, `{"max_mib":65536}`)
			svc := srv.svc
			rec, first := createPersistentAs(t, h, tok, "big")
			if rec.Code != http.StatusCreated {
				t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
			}
			id := first["id"].(string)
			l := svc.lookup(uid, id)
			if _, err := svc.suspend(context.Background(), ownerIDFor(t, h, tok), id); err != nil {
				t.Fatalf("suspend: %v", err)
			}
			svc.store.mu.Lock()
			l.busy = true
			svc.store.mu.Unlock()
			t.Cleanup(func() {
				svc.store.mu.Lock()
				l.busy = false
				svc.store.mu.Unlock()
			})

			ts := httptest.NewServer(h)
			t.Cleanup(ts.Close)
			resp, body, _ := p.do(t, ts, srv, id, tok)
			if resp.StatusCode != http.StatusConflict {
				t.Fatalf("%s busy = %d, want 409: %s", p.name, resp.StatusCode, body)
			}
			var doc map[string]any
			if err := json.Unmarshal([]byte(body), &doc); err != nil {
				t.Fatalf("%s busy body is not JSON: %v (%s)", p.name, err, body)
			}
			if doc["code"] != "lease_busy" {
				t.Fatalf("%s busy code = %v, want lease_busy: %s", p.name, doc["code"], body)
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

// TestResumeOnUseDrainRefuses: a work call during a planned-restart
// drain must not resume a Drained lease. It answers the shared no-room
// shape (503 capacity_wait) and the lease stays suspended and Drained,
// so the drain's "no running sandboxes" wait is not broken (R1).
func TestResumeOnUseDrainRefuses(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	// Plenty of room: only the drain state may refuse the resume.
	sub.SetNodeInfo(substrate.NodeInfo{Status: "healthy", HugepagesTotal: 1 << 20, HugepageSizeBytes: 2 << 20}, nil)
	ctx := context.Background()
	l, err := svc.grant(ctx, "consumer-a", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	// A drained, suspended lease, as the planned-restart drain leaves it.
	if _, err := svc.pauseLeaseWith(ctx, l, true, suspendPolicy{}); err != nil {
		t.Fatalf("drain pause: %v", err)
	}
	if !l.Drained || !l.Suspended {
		t.Fatal("precondition: lease not suspended and Drained")
	}
	// spoond is draining: a work call must wait, not resume.
	svc.draining.Store(true)
	t.Cleanup(func() { svc.draining.Store(false) })

	resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+l.ID+"/exec", "token-a",
		map[string]any{"cmd": "echo hi"})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("exec during drain = %d, want 503: %v", resp.StatusCode, body)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Fatal("exec during drain has no Retry-After")
	}
	if body["code"] != "capacity_wait" {
		t.Fatalf("exec during drain code = %v, want capacity_wait: %v", body["code"], body)
	}
	if !l.Suspended || !l.Drained {
		t.Fatalf("the drain-refused work call changed the lease: suspended=%v Drained=%v", l.Suspended, l.Drained)
	}
}

// TestResumeOnUseLLM401LeavesSuspended: the LLM gateway authenticates the
// per-user key BEFORE it resumes a suspended lease (review R2). A
// request with no or a wrong key answers 401 and the lease stays
// suspended, so a token holder cannot spend hugepages (or preempt
// another lease) through another owner's lease.
func TestResumeOnUseLLM401LeavesSuspended(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ids, err := identity.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	svc.SetIdentities(ids)
	owner, err := ids.AddUser("owner", identity.KindPerson, []string{"SHA256:fp-owner"}, "owner-tok")
	if err != nil {
		t.Fatal(err)
	}
	if err := ids.SetLLMKey(owner.ID, "slk-owner-secret"); err != nil {
		t.Fatal(err)
	}

	l, err := svc.grant(context.Background(), owner.ID, "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.suspend(context.Background(), owner.ID, l.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	srv := NewServerWithLLM(svc, NewImageRegistry(db), "http://127.0.0.1:1", "host-key", "", nil)
	for _, key := range []string{"", "slk-wrong"} {
		req := httptest.NewRequest("POST", llmGatewayPrefix+l.ID+"/openai/chat/completions", strings.NewReader(`{"model":"m"}`))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		srv.llm.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("llm key %q = %d, want 401: %s", key, rec.Code, rec.Body.String())
		}
		if !l.Suspended {
			t.Fatalf("llm key %q resumed the lease before authenticating", key)
		}
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

// TestResumeOnUseConcurrentExactlyOneResume (review R4): four real
// concurrent work calls on one suspended lease resume it exactly once.
// The one resume's Create runs; the others wait on the busy flag and
// then serve (200) or answer 409 lease_busy, never 500. Deleting
// resumeLease's `l.busy` guard makes more than one Create (or a 500)
// observable here.
func TestResumeOnUseConcurrentExactlyOneResume(t *testing.T) {
	ts, svc, _, sub := newTestServerWithService(t)
	sub.SetNodeInfo(substrate.NodeInfo{Status: "healthy", HugepagesTotal: 1 << 20, HugepageSizeBytes: 2 << 20}, nil)

	ctx := context.Background()
	l, err := svc.grant(ctx, "consumer-a", "py-base", time.Hour, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := svc.suspend(ctx, "consumer-a", l.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if !l.Suspended {
		t.Fatal("precondition: lease not suspended")
	}

	// Hold the first resume's Create briefly so the other three calls
	// reach the lease while its busy flag is set, rather than each
	// finding it already running with nothing to do.
	var creates atomic.Int32
	release := make(chan struct{})
	sub.createFn = func(cctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if req.Resume {
			n := creates.Add(1)
			if n > 1 {
				// A second resume reached createSandbox: the busy guard
				// failed to serialise. Let it complete so the assertion
				// below reports the count rather than a timeout.
				t.Errorf("resume Create #%d ran; want exactly one", n)
			}
			// Wait long enough for the other calls to arrive.
			select {
			case <-release:
			case <-time.After(2 * time.Second):
			}
		}
		return sub.Fake.Create(cctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	const workers = 4
	type result struct {
		status int
		code   any
	}
	results := make(chan result, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("POST", ts.URL+"/api/leases/"+l.ID+"/exec", strings.NewReader(`{"cmd":"echo hi"}`))
			req.Header.Set("Authorization", "Bearer token-a")
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results <- result{status: -1}
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var doc map[string]any
			_ = json.Unmarshal(b, &doc)
			results <- result{status: resp.StatusCode, code: doc["code"]}
		}()
	}
	// Let the first resume reach its Create and the others find the
	// lease busy, then release it.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	close(results)

	oks, busy := 0, 0
	for r := range results {
		switch {
		case r.status == http.StatusOK:
			oks++
		case r.status == http.StatusConflict && r.code == "lease_busy":
			busy++
		default:
			t.Fatalf("concurrent exec = %d code=%v, want 200 or 409 lease_busy", r.status, r.code)
		}
	}
	// The one resume serves at least one call; the others either serve
	// after waiting or answer the retryable 409 lease_busy. None may be
	// a 500, and together they cover every caller.
	if oks < 1 || oks+busy != workers {
		t.Fatalf("concurrent exec: %d served, %d busy (of %d), want >=1 served and no 500", oks, busy, workers)
	}
	if got := creates.Load(); got != 1 {
		t.Fatalf("resume Creates = %d, want exactly 1", got)
	}
	if l.Suspended {
		t.Fatal("the lease is still suspended after the concurrent work calls")
	}
}
