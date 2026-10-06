package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/identity"
	"github.com/jrimmer/spoond/v2/metrics"
	"github.com/jrimmer/spoond/v2/substrate"
)

// newClassServer bootstraps admin + a quota'd user over images of the
// given sizes (name -> memory_mb), like newMemQuotaServer. quota is the
// JSON quota body. The fake's NodeInfo is left at the default (huge).
func newClassServer(t *testing.T, images map[string]int, quota string) (*Server, http.Handler, *testSub, string, string) {
	t.Helper()
	svc, db, sub := newTestService(t)
	for name, mb := range images {
		seedImage(t, db, name, mb)
	}
	ids, _ := identity.NewStore("")
	svc.SetIdentities(ids)
	srv := NewServer(svc, NewImageRegistry(db))
	h := srv.Handler()

	if rec, _ := doUsersReq(t, h, "POST", "/api/users", "legacy-tok", `{"name":"admin","fingerprints":["SHA256:fp-a"],"token":"admin-tok"}`); rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap admin: %d", rec.Code)
	}
	rec, body := doUsersReq(t, h, "POST", "/api/users", "admin-tok", `{"name":"worker","fingerprints":["SHA256:fp-b"],"token":"work-tok"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create user: %d", rec.Code)
	}
	uid := body["user"].(map[string]any)["id"].(string)
	if rec2, _ := doUsersReq(t, h, "POST", "/api/users/"+uid+"/quota", "admin-tok", quota); rec2.Code != http.StatusOK {
		t.Fatalf("set quota: %d %s", rec2.Code, rec2.Body.String())
	}
	// The fake's default NodeInfo is enormous (1<<20 pages of 2 MiB),
	// so the default reserve never bites unless a test sets its own
	// node. Drop the NodeInfo cache the service may already hold.
	sub.SetNodeInfo(substrate.NodeInfo{
		Status:            "healthy",
		HugepagesTotal:    1 << 20,
		HugepageSizeBytes: 2 << 20,
	}, nil)
	svc.nodeInfoMu.Lock()
	svc.nodeInfoAt = time.Time{}
	svc.nodeInfoMu.Unlock()
	return srv, h, sub, "work-tok", uid
}

// createBodyResp POSTs the create body and returns the status, the
// body text (tests that match the error text) and the headers
// (Retry-After).
func createBodyResp(t *testing.T, h http.Handler, token, body string) (int, string, http.Header) {
	t.Helper()
	rec, _ := createSandboxBody(t, h, token, body)
	return rec.Code, rec.Body.String(), rec.Header()
}

// leaseDetail GETs one lease's detail as the caller.
func leaseDetail(t *testing.T, h http.Handler, token, id string) map[string]any {
	t.Helper()
	req := httptestGet(t, h, token, "/api/sandboxes/"+id)
	var m map[string]any
	if err := json.Unmarshal(req.Body.Bytes(), &m); err != nil {
		t.Fatalf("detail %s: %v", id, err)
	}
	return m
}

// TestClassGuaranteedWithinQuota: a user with a guaranteed_mib keeps
// their leases guaranteed while the running charge (this lease
// included) stays within it.
func TestClassGuaranteedWithinQuota(t *testing.T) {
	_, h, _, tok, _ := newClassServer(t, map[string]int{"mid": 1024}, `{"guaranteed_mib":2048,"max_mib":8192}`)

	code, body, _ := createBodyResp(t, h, tok, `{"image":"mid","ttl":60}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	detail := leaseDetail(t, h, tok, firstLeaseID(t, h, tok))
	if got := detail["class"]; got != ClassGuaranteed {
		t.Fatalf("class = %v, want guaranteed", got)
	}
	if got := int(detail["priority"].(float64)); got != 0 {
		t.Fatalf("priority = %d, want the default 0", got)
	}
}

// TestClassBurstsPastGuarantee: the lease that passes the owner's
// guaranteed_mib is classified burst; the next one within the
// guarantee again is not (each lease's own charge is what tips the
// sum).
func TestClassBurstsPastGuarantee(t *testing.T) {
	_, h, _, tok, _ := newClassServer(t, map[string]int{"mid": 1024}, `{"guaranteed_mib":2048,"max_mib":8192}`)

	code, body, _ := createBodyResp(t, h, tok, `{"image":"mid","ttl":60}`)
	if code != http.StatusCreated {
		t.Fatalf("first create: %d %s", code, body)
	}
	detail := leaseDetail(t, h, tok, firstLeaseID(t, h, tok))
	if got := detail["class"]; got != ClassGuaranteed {
		t.Fatalf("first lease class = %v, want guaranteed", got)
	}
	// 1024 charged + 1024 new = 2048, exactly the guarantee: still
	// guaranteed.
	code, body, _ = createBodyResp(t, h, tok, `{"image":"mid","ttl":60}`)
	if code != http.StatusCreated {
		t.Fatalf("second create: %d %s", code, body)
	}
	// The third (3072 running) passes the guarantee: burst.
	code, body, _ = createBodyResp(t, h, tok, `{"image":"mid","ttl":60}`)
	if code != http.StatusCreated {
		t.Fatalf("third create: %d %s", code, body)
	}
	detail = leaseDetail(t, h, tok, thirdLeaseID(t, h, tok))
	if got := detail["class"]; got != ClassBurst {
		t.Fatalf("third lease class = %v, want burst", got)
	}
}

// TestClassNoGuaranteeKeepsGuaranteed: a user with no guaranteed_mib
// keeps every lease guaranteed — today's behaviour (#128 part 2 item
// 3).
func TestClassNoGuaranteeKeepsGuaranteed(t *testing.T) {
	_, h, _, tok, _ := newClassServer(t, map[string]int{"mid": 1024}, `{"max_mib":8192}`)

	for i := 0; i < 3; i++ {
		code, body, _ := createBodyResp(t, h, tok, `{"image":"mid","ttl":60}`)
		if code != http.StatusCreated {
			t.Fatalf("create %d: %d %s", i, code, body)
		}
	}
	ids := leaseIDs(t, h, tok)
	for _, id := range ids {
		if got := leaseDetail(t, h, tok, id)["class"]; got != ClassGuaranteed {
			t.Fatalf("lease %s class = %v, want guaranteed (no guarantee set)", id, got)
		}
	}
}

// TestClassExplicitBurstFlag: "burst": true classifies the lease burst
// even within the guarantee, and "priority" is stored and reported.
func TestClassExplicitBurstFlag(t *testing.T) {
	_, h, _, tok, _ := newClassServer(t, map[string]int{"mid": 1024}, `{"guaranteed_mib":8192,"max_mib":16384}`)

	code, body, _ := createBodyResp(t, h, tok, `{"image":"mid","ttl":60,"burst":true,"priority":5}`)
	if code != http.StatusCreated {
		t.Fatalf("burst create: %d %s", code, body)
	}
	detail := leaseDetail(t, h, tok, firstLeaseID(t, h, tok))
	if got := detail["class"]; got != ClassBurst {
		t.Fatalf("class = %v, want burst (explicit burst flag)", got)
	}
	if got := int(detail["priority"].(float64)); got != 5 {
		t.Fatalf("priority = %d, want 5", got)
	}
}

// TestClassForkAndCloneAdmit: fork children and the clone take their
// own class — a fork past the guarantee bursts its children, a clone
// within it stays guaranteed.
func TestClassForkAndCloneAdmit(t *testing.T) {
	srv, _, _, _, uid := newClassServer(t, map[string]int{"mid": 1024}, `{"guaranteed_mib":2048,"max_mib":16384}`)
	svc := srv.svc
	ctx := context.Background()

	src, err := svc.grantLease(ctx, leaseRequest{owner: uid, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant source: %v", err)
	}
	// The source is 1024 of 2048: a clone (another 1024) fits the
	// guarantee and stays guaranteed.
	clone, _, err := svc.clone(ctx, uid, src.ID)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if clone.Class != ClassGuaranteed {
		t.Fatalf("clone class = %s, want guaranteed", clone.Class)
	}
	// Two fork children (2 × 1024 past the 2048 already running) pass
	// the guarantee: every child of the batch bursts.
	kids, _, err := svc.fork(ctx, uid, src.ID, 2, false, time.Hour, "", "")
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	for _, k := range kids {
		if k.Class != ClassBurst {
			t.Fatalf("fork child class = %s, want burst", k.Class)
		}
	}
}

// TestClassResumeReadmits: a lease that burst because the guarantee
// was full falls back to guaranteed when the charge has room again —
// and re-passes the memory check on the way (resume re-admits).
func TestClassResumeReadmits(t *testing.T) {
	srv, h, _, tok, uid := newClassServer(t, map[string]int{"mid": 1024}, `{"guaranteed_mib":2048,"max_mib":16384}`)
	svc := srv.svc
	owner := ownerIDFor(t, h, tok)
	ctx := context.Background()

	// Two fills spend the whole guarantee (2 × 1024).
	var fillIDs []string
	for i := 0; i < 2; i++ {
		rec, body := createSandboxAs(t, h, tok, "mid")
		if rec.Code != http.StatusCreated {
			t.Fatalf("fill %d: %d %s", i, rec.Code, rec.Body.String())
		}
		fillIDs = append(fillIDs, body["id"].(string))
	}
	// The next lease (persistent) bursts.
	rec, burst := createPersistentAs(t, h, tok, "mid")
	if rec.Code != http.StatusCreated {
		t.Fatalf("burst create: %d %s", rec.Code, rec.Body.String())
	}
	burstID := burst["id"].(string)
	if l := svc.lookup(uid, burstID); l == nil || l.Class != ClassBurst {
		t.Fatalf("lease past the guarantee class = %v, want burst", l)
	}
	// Suspend the burst lease, release the fills, resume: the guarantee
	// has room, so the lease comes back guaranteed.
	l := svc.lookup(uid, burstID)
	if _, err := svc.suspend(ctx, owner, burstID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	for _, fid := range fillIDs {
		svc.release(ctx, svc.lookup(uid, fid))
	}
	if _, err := svc.resumeLease(ctx, l); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if l.Class != ClassGuaranteed {
		t.Fatalf("resumed lease class = %s, want guaranteed (the guarantee has room again)", l.Class)
	}
}

// firstLeaseID returns the store id of the caller's only lease.
func firstLeaseID(t *testing.T, h http.Handler, token string) string {
	t.Helper()
	ids := leaseIDs(t, h, token)
	if len(ids) != 1 {
		t.Fatalf("want 1 lease, got %d", len(ids))
	}
	return ids[0]
}

// thirdLeaseID returns the caller's third lease (list order is map
// order, so the tests use charge arithmetic instead of order — this
// helper pins the one lease whose class differs by checking all
// three).
func thirdLeaseID(t *testing.T, h http.Handler, token string) string {
	t.Helper()
	for _, id := range leaseIDs(t, h, token) {
		if leaseDetail(t, h, token, id)["class"] == ClassBurst {
			return id
		}
	}
	t.Fatal("no burst lease found")
	return ""
}

// leaseIDs lists the caller's lease ids.
func leaseIDs(t *testing.T, h http.Handler, token string) []string {
	t.Helper()
	req := httptestGet(t, h, token, "/api/sandboxes")
	var m struct {
		Sandboxes []map[string]any `json:"sandboxes"`
	}
	if err := json.Unmarshal(req.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range m.Sandboxes {
		out = append(out, s["id"].(string))
	}
	return out
}

// TestBurstReserveRefused: a burst lease is admitted only while the
// node's free hugepages stay above BURST_RESERVE_MIB after it; a
// refusal answers 503 "no burst capacity" with Retry-After: 30, and a
// guaranteed lease is never held to the reserve.
func TestBurstReserveRefused(t *testing.T) {
	srv, h, sub, tok, _ := newClassServer(t, map[string]int{"mid": 1024}, `{"max_mib":8192}`)
	srv.svc.cfg.BurstReserveMiB = 8192
	// 1024 MiB free: plenty for the lease itself, under any reserve.
	sub.SetNodeInfo(substrate.NodeInfo{
		Status:            "healthy",
		HugepagesTotal:    512 + 1, // pages; the fake starts with none "used"
		HugepagesUsed:     0,
		HugepagesReserved: 0,
		HugepageSizeBytes: 2 << 20,
	}, nil)
	svc := srv.svc

	// Explicit burst: refused with 503 no burst capacity + Retry-After.
	code, body, hdr := createBodyResp(t, h, tok, `{"image":"mid","ttl":60,"burst":true}`)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("burst create = %d %s, want 503", code, body)
	}
	if !strings.Contains(body, "no burst capacity") {
		t.Fatalf("503 body should name the burst capacity, got %s", body)
	}
	if ra := hdr.Get("Retry-After"); ra != "30" {
		t.Fatalf("Retry-After = %q, want 30", ra)
	}
	// The same lease without the flag is guaranteed and never touches
	// the reserve.
	code, body, _ = createBodyResp(t, h, tok, `{"image":"mid","ttl":60}`)
	if code != http.StatusCreated {
		t.Fatalf("guaranteed create: %d %s", code, body)
	}
	// Raising the reserve headroom admits the next burst lease.
	sub.SetNodeInfo(substrate.NodeInfo{
		Status:            "healthy",
		HugepagesTotal:    8192 + 512 + 1,
		HugepageSizeBytes: 2 << 20,
	}, nil)
	svc.nodeInfoMu.Lock()
	svc.nodeInfoAt = time.Time{} // drop the cache: the test changed the node
	svc.nodeInfoMu.Unlock()
	code, body, _ = createBodyResp(t, h, tok, `{"image":"mid","ttl":60,"burst":true}`)
	if code != http.StatusCreated {
		t.Fatalf("burst create with headroom: %d %s", code, body)
	}
	if got := leaseDetail(t, h, tok, burstLeaseID(t, h, tok))["class"]; got != ClassBurst {
		t.Fatalf("class = %v, want burst", got)
	}
}

// burstLeaseID returns the caller's only burst lease.
func burstLeaseID(t *testing.T, h http.Handler, token string) string {
	t.Helper()
	for _, id := range leaseIDs(t, h, token) {
		if leaseDetail(t, h, token, id)["class"] == ClassBurst {
			return id
		}
	}
	t.Fatal("no burst lease found")
	return ""
}

// TestBurstReserveSuspendFrees: a suspended burst lease is uncharged,
// so resuming it re-passes the reserve — and a resume into a full
// reserve answers 503 and leaves the lease suspended.
func TestBurstReserveSuspendFrees(t *testing.T) {
	srv, h, sub, tok, uid := newClassServer(t, map[string]int{"mid": 1024}, `{"max_mib":8192}`)
	srv.svc.cfg.BurstReserveMiB = 8192
	svc := srv.svc
	owner := ownerIDFor(t, h, tok)
	ctx := context.Background()

	rec, first := createPersistentAs(t, h, tok, "mid")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	id := first["id"].(string)
	l := svc.lookup(uid, id)
	// Make it burst by hand (the default node has no reserve pressure):
	// the flag is what the request would have carried.
	l.Burst = true
	if _, err := svc.suspend(ctx, owner, id); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	// Shrink the node so the resume's burst admission cannot fit.
	sub.SetNodeInfo(substrate.NodeInfo{
		Status:            "healthy",
		HugepagesTotal:    512 + 1,
		HugepageSizeBytes: 2 << 20,
	}, nil)
	svc.nodeInfoMu.Lock()
	svc.nodeInfoAt = time.Time{}
	svc.nodeInfoMu.Unlock()
	if _, err := svc.resumeLease(ctx, l); err == nil {
		t.Fatal("resume into a full reserve should fail")
	} else if !strings.Contains(err.Error(), "no burst capacity") {
		t.Fatalf("resume error should name the burst capacity, got %v", err)
	}
	if !l.Suspended {
		t.Fatal("refused burst resume must leave the lease suspended")
	}
}

// httptestGet issues a GET with the bearer token and returns the
// recorder.
func httptestGet(t *testing.T, h http.Handler, token, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
	}
	return rec
}

// TestBurstReserveRestartRefused: restart re-admits a suspended lease
// like a resume, so a burst lease restarting into a full reserve
// answers 503 no burst capacity with Retry-After: 30 and stays
// suspended — warm and cold alike.
func TestBurstReserveRestartRefused(t *testing.T) {
	srv, h, sub, tok, uid := newClassServer(t, map[string]int{"mid": 1024}, `{"max_mib":8192}`)
	srv.svc.cfg.BurstReserveMiB = 8192
	svc := srv.svc
	owner := ownerIDFor(t, h, tok)
	ctx := context.Background()

	rec, first := createPersistentAs(t, h, tok, "mid")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	id := first["id"].(string)
	l := svc.lookup(uid, id)
	l.Burst = true // the flag the request would have carried
	if _, err := svc.suspend(ctx, owner, id); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	// Shrink the node so the restart's burst admission cannot fit.
	sub.SetNodeInfo(substrate.NodeInfo{
		Status:            "healthy",
		HugepagesTotal:    512 + 1,
		HugepageSizeBytes: 2 << 20,
	}, nil)
	svc.nodeInfoMu.Lock()
	svc.nodeInfoAt = time.Time{}
	svc.nodeInfoMu.Unlock()

	for _, path := range []string{
		"/api/leases/" + id + "/restart",           // warm
		"/api/leases/" + id + "/restart?mode=cold", // cold
	} {
		resp := postLeaseAction(t, h, tok, path, "")
		if resp.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s = %d %s, want 503", path, resp.Code, resp.Body.String())
		}
		if !strings.Contains(resp.Body.String(), "no burst capacity") {
			t.Fatalf("%s 503 body should name the burst capacity, got %s", path, resp.Body.String())
		}
		if ra := resp.Header().Get("Retry-After"); ra != "30" {
			t.Fatalf("%s Retry-After = %q, want 30", path, ra)
		}
	}
	if !l.Suspended || l.SandboxID == "" {
		t.Fatal("refused restart must leave the suspended lease untouched")
	}
}

// TestBurstReserveUndrainDefers: an undrain whose burst lease cannot
// fit the reserve leaves it drained (retryable), not lost — the same
// answer an over-quota lease gets — and a later undrain resumes it.
func TestBurstReserveUndrainDefers(t *testing.T) {
	srv, h, sub, tok, uid := newClassServer(t, map[string]int{"mid": 1024}, `{"max_mib":8192}`)
	srv.svc.cfg.BurstReserveMiB = 8192
	svc := srv.svc
	owner := ownerIDFor(t, h, tok)
	ctx := context.Background()

	rec, first := createPersistentAs(t, h, tok, "mid")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	id := first["id"].(string)
	l := svc.lookup(uid, id)
	l.Burst = true
	// Drain pauses the lease and clears Drained on undrain only when
	// the resume succeeds, so set the flag the way drain leaves it.
	if _, err := svc.suspend(ctx, owner, id); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	svc.store.mu.Lock()
	l.Drained = true
	svc.saveLeaseLocked(l)
	svc.store.mu.Unlock()

	// The node cannot fit the burst resume.
	sub.SetNodeInfo(substrate.NodeInfo{
		Status:            "healthy",
		HugepagesTotal:    512 + 1,
		HugepageSizeBytes: 2 << 20,
	}, nil)
	svc.nodeInfoMu.Lock()
	svc.nodeInfoAt = time.Time{}
	svc.nodeInfoMu.Unlock()

	res := svc.undrain(ctx)
	if len(res.Failed) != 1 || res.Resumed != 0 {
		t.Fatalf("undrain = +%v, want the burst lease in failed", res)
	}
	if !l.Drained || l.State != "suspended" {
		t.Fatalf("reserve-refused undrain must keep the lease drained and suspended, got state=%s drained=%v", l.State, l.Drained)
	}
	if l.LostAt != (time.Time{}) {
		t.Fatal("reserve-refused undrain must not stamp the lease lost")
	}

	// Headroom returns; the retry resumes the lease.
	sub.SetNodeInfo(substrate.NodeInfo{
		Status:            "healthy",
		HugepagesTotal:    8192 + 512 + 1,
		HugepageSizeBytes: 2 << 20,
	}, nil)
	svc.nodeInfoMu.Lock()
	svc.nodeInfoAt = time.Time{}
	svc.nodeInfoMu.Unlock()
	res = svc.undrain(ctx)
	if res.Resumed != 1 {
		t.Fatalf("retry undrain resumed = %d, want 1 (failed: %v)", res.Resumed, res.Failed)
	}
	if l.State != "running" || l.Drained {
		t.Fatalf("lease after the retry = %s drained=%v, want running and undrained", l.State, l.Drained)
	}
}

// TestNodeMetricsFillsBurstCache: the node-gauge refresh and the burst
// reserve read the same NodeInfo cache (#128 part 2). A gauge pass under
// a roomy node caches it, so a burst admission right after the node
// shrinks is still admitted from the cached (roomy) value instead of
// fetching the shrunken one and refusing on the reserve.
func TestNodeMetricsFillsBurstCache(t *testing.T) {
	srv, h, sub, tok, _ := newClassServer(t, map[string]int{"mid": 1024}, `{"max_mib":8192}`)
	svc := srv.svc
	svc.SetMetrics(metrics.NewBackendMetrics())
	svc.cfg.BurstReserveMiB = 8192
	// The gauge loop sees a node with plenty of headroom and caches it.
	sub.SetNodeInfo(substrate.NodeInfo{
		Status:            "healthy",
		HugepagesTotal:    8192 + 512 + 1,
		HugepageSizeBytes: 2 << 20,
	}, nil)
	svc.updateNodeMetrics(context.Background())
	// The node shrinks before the burst admission, but the cached value
	// (fresh, under nodeInfoCacheTTL) still admits it.
	sub.SetNodeInfo(substrate.NodeInfo{
		Status:            "healthy",
		HugepagesTotal:    512 + 1,
		HugepageSizeBytes: 2 << 20,
	}, nil)
	code, body, _ := createBodyResp(t, h, tok, `{"image":"mid","ttl":60,"burst":true}`)
	if code != http.StatusCreated {
		t.Fatalf("burst create on the gauge-cached node = %d %s, want 201", code, body)
	}
}
