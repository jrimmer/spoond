package runner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestHTTPLeaseClientAgentToken asserts the lease client authenticates
// with the per-agent bearer token it was constructed with (U4: agents as
// users, epic #26 ticket #30). The backend resolves that token to the
// agent's user id, so the client's only job is to send it on every call.
func TestHTTPLeaseClientAgentToken(t *testing.T) {
	const agentToken = "agent-token-abc123"
	var got []string // Authorization headers, in call order

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/sandboxes":
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"sb-1"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec"):
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"stdout":"ok\n","stderr":"","exit":0}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewHTTPLeaseClient(srv.URL, agentToken)
	ctx := context.Background()

	id, err := c.Create(ctx, "dev-base", 600)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := c.Exec(ctx, id, "echo hi", "", nil, 10); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if err := c.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("got %d authenticated calls, want 3", len(got))
	}
	want := "Bearer " + agentToken
	for i, h := range got {
		if h != want {
			t.Errorf("call %d: Authorization = %q, want %q", i, h, want)
		}
	}
}

// TestHTTPLeaseClientCreateLabel (#119): a labelled Create sends no
// holder fields (the lease must not be held: held leases are
// checkpointed periodically) and sets the label as the lease's comment
// right after the create.
func TestHTTPLeaseClientCreateLabel(t *testing.T) {
	var body map[string]any
	var comment map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/sandboxes":
			json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"sb-1"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/leases/sb-1/comment":
			json.NewDecoder(r.Body).Decode(&comment)
			w.Write([]byte(`{"id":"sb-1","ok":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewHTTPLeaseClient(srv.URL, "tok")
	c.WithLabel("forgejo job 42 https://code.example.com/actions/runs/7/jobs/42")
	if _, err := c.Create(context.Background(), "py-base", 600); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, k := range []string{"holder", "holder_url", "hold_ttl"} {
		if _, ok := body[k]; ok {
			t.Errorf("%s sent on create: the job lease must not be held", k)
		}
	}
	if comment["comment"] != "forgejo job 42 https://code.example.com/actions/runs/7/jobs/42" {
		t.Errorf("comment = %q, want the job label", comment["comment"])
	}
}

// TestHTTPLeaseClientCreateLabelFailureIsNotFatal: a comment the backend
// refuses leaves the lease created (the job runs; only the sweep misses
// it if the runner dies).
func TestHTTPLeaseClientCreateLabelFailureIsNotFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/sandboxes" {
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"sb-2"}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewHTTPLeaseClient(srv.URL, "tok")
	c.WithLabel("forgejo job 1 x")
	if id, err := c.Create(context.Background(), "py-base", 600); err != nil || id != "sb-2" {
		t.Fatalf("Create = %q, %v; want sb-2, nil", id, err)
	}
}

// TestHTTPLeaseClientCreateNoLabel: without WithLabel there is no
// comment call (other callers of the client must be unaffected).
func TestHTTPLeaseClientCreateNoLabel(t *testing.T) {
	commented := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/sandboxes" {
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"sb-plain"}`))
			return
		}
		commented = true
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := NewHTTPLeaseClient(srv.URL, "tok")
	if _, err := c.Create(context.Background(), "py-base", 600); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if commented {
		t.Error("comment call made without a label")
	}
}

// fakeSweepBackend is a minimal lease API for SweepOrphans tests.
type fakeSweepBackend struct {
	mu       sync.Mutex
	leases   []leaseRow
	deleted  []string
	failList bool
}

func (f *fakeSweepBackend) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/leases":
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.failList {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"sandboxes": f.leases})
		case r.Method == http.MethodDelete:
			id := strings.TrimPrefix(r.URL.Path, "/api/leases/")
			f.mu.Lock()
			f.deleted = append(f.deleted, id)
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// TestSweepOrphansDeletesOnlyJobLabelled (#119): the start sweep
// deletes every lease whose comment names a Forgejo job and nothing
// else — a lease with another comment, or none, stays.
func TestSweepOrphansDeletesOnlyJobLabelled(t *testing.T) {
	backend := &fakeSweepBackend{leases: []leaseRow{
		{ID: "job-1", Comment: "forgejo job 11"},
		{ID: "job-2", Comment: "forgejo job 12"},
		{ID: "person", Comment: "alice scratch"},
		{ID: "plain"},
	}}
	srv := httptest.NewServer(backend.handler(t))
	defer srv.Close()

	c := NewHTTPLeaseClient(srv.URL, "tok")
	n, err := c.SweepOrphans(context.Background(), nil)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if n != 2 {
		t.Fatalf("swept %d leases, want 2", n)
	}
	if len(backend.deleted) != 2 || backend.deleted[0] != "job-1" || backend.deleted[1] != "job-2" {
		t.Fatalf("deleted %v, want [job-1 job-2]", backend.deleted)
	}
}

// TestSweepOrphansKeepsRunningJobs: a non-nil keep function spares the
// leases of jobs the caller still runs.
func TestSweepOrphansKeepsRunningJobs(t *testing.T) {
	backend := &fakeSweepBackend{leases: []leaseRow{
		{ID: "job-1", Comment: "forgejo job 11"},
		{ID: "job-2", Comment: "forgejo job 12"},
	}}
	srv := httptest.NewServer(backend.handler(t))
	defer srv.Close()

	c := NewHTTPLeaseClient(srv.URL, "tok")
	n, err := c.SweepOrphans(context.Background(), func(id string) bool { return id == "job-1" })
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if n != 1 || len(backend.deleted) != 1 || backend.deleted[0] != "job-2" {
		t.Fatalf("deleted %v (n=%d), want only job-2", backend.deleted, n)
	}
}

// TestSweepOrphansListFailure: a failed list is an error and deletes
// nothing — a transient backend failure never releases the wrong lease.
func TestSweepOrphansListFailure(t *testing.T) {
	backend := &fakeSweepBackend{failList: true, leases: []leaseRow{
		{ID: "job-1", Comment: "forgejo job 11"},
	}}
	srv := httptest.NewServer(backend.handler(t))
	defer srv.Close()

	c := NewHTTPLeaseClient(srv.URL, "tok")
	if _, err := c.SweepOrphans(context.Background(), nil); err == nil {
		t.Fatal("SweepOrphans succeeded on a failed list, want error")
	}
	if len(backend.deleted) != 0 {
		t.Fatalf("deleted %v after a failed list, want none", backend.deleted)
	}
}

// TestHTTPLeaseClientDeleteReason: DeleteReason sends the reason as a
// JSON body on the DELETE; Delete without a reason sends no body, so the
// backend keeps its default detail.
func TestHTTPLeaseClientDeleteReason(t *testing.T) {
	var gotReason string
	var gotBody bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		var body struct {
			Reason string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			gotBody = true
			gotReason = body.Reason
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewHTTPLeaseClient(srv.URL, "tok")
	if err := c.DeleteReason(context.Background(), "sb-1", "ci job 42 ✗ 4m10s"); err != nil {
		t.Fatalf("DeleteReason: %v", err)
	}
	if !gotBody || gotReason != "ci job 42 ✗ 4m10s" {
		t.Fatalf("reason body = %q (present=%v), want the CI reason", gotReason, gotBody)
	}

	gotBody, gotReason = false, ""
	if err := c.Delete(context.Background(), "sb-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if gotBody {
		t.Fatalf("plain Delete sent a reason body: %q", gotReason)
	}
}

// TestHTTPLeaseClientCreateSendsAdmitWait: every create carries the
// admission wait (RUNNER_ADMIT_WAIT_SECS) so a full node's queue (#129)
// holds it open instead of refusing at once.
func TestHTTPLeaseClientCreateSendsAdmitWait(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/sandboxes" {
			json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"sb-1"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewHTTPLeaseClient(srv.URL, "tok")
	c.SetAdmitWait(900)
	if _, err := c.Create(context.Background(), "py-base", 600); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got, ok := body["wait"].(float64); !ok || int(got) != 900 {
		t.Fatalf("create wait = %v, want 900", body["wait"])
	}
}

// TestHTTPLeaseClientCreateNoWaitWhenDisabled: RUNNER_ADMIT_WAIT_SECS=0
// sends no "wait" field (today's behaviour).
func TestHTTPLeaseClientCreateNoWaitWhenDisabled(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"sb-1"}`))
	}))
	defer srv.Close()

	c := NewHTTPLeaseClient(srv.URL, "tok")
	c.SetAdmitWait(0)
	if _, err := c.Create(context.Background(), "py-base", 600); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, ok := body["wait"]; ok {
		t.Fatalf("create sent a wait with admission waiting disabled: %v", body["wait"])
	}
}

// TestHTTPLeaseClientCreateRetriesCapacity503: a create refused for
// capacity (503) is retried after Retry-After, logged, and eventually
// succeeds — a capacity refusal never fails the job.
func TestHTTPLeaseClientCreateRetriesCapacity503(t *testing.T) {
	var attempts int
	var waits []time.Duration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/sandboxes" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		attempts++
		if attempts < 3 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"capacity: no room"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"sb-cold"}`))
	}))
	defer srv.Close()

	c := NewHTTPLeaseClient(srv.URL, "tok")
	c.sleepFn = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	id, err := c.Create(context.Background(), "py-base", 600)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id != "sb-cold" {
		t.Fatalf("Create id = %q, want sb-cold", id)
	}
	if attempts != 3 {
		t.Fatalf("create attempts = %d, want 3", attempts)
	}
	if len(waits) != 2 || waits[0] != 7*time.Second || waits[1] != 7*time.Second {
		t.Fatalf("retry waits = %v, want two 7s waits", waits)
	}
}

// TestHTTPLeaseClientCreateCapacity503DefaultRetryAfter: a capacity 503
// without a Retry-After uses the default hint.
func TestHTTPLeaseClientCreateCapacity503DefaultRetryAfter(t *testing.T) {
	var waits []time.Duration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(waits) == 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"sb-1"}`))
	}))
	defer srv.Close()

	c := NewHTTPLeaseClient(srv.URL, "tok")
	c.sleepFn = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	if _, err := c.Create(context.Background(), "py-base", 600); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(waits) != 1 || waits[0] != defaultCapacityRetryAfter {
		t.Fatalf("retry wait = %v, want %s", waits, defaultCapacityRetryAfter)
	}
}

// TestHTTPLeaseClientCreateCapacity503BoundedByContext: the job's own
// timeout still bounds the retry loop — a 503 that never clears ends
// when ctx ends, not before and not forever.
func TestHTTPLeaseClientCreateCapacity503BoundedByContext(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := NewHTTPLeaseClient(srv.URL, "tok")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Create(ctx, "py-base", 600)
	if err == nil {
		t.Fatal("Create succeeded on a permanently full node, want a context error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Create error = %v, want context.DeadlineExceeded", err)
	}
	if attempts < 1 {
		t.Fatalf("create attempts = %d, want at least 1", attempts)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Create waited %s past its context, want a bounded wait", elapsed)
	}
}

// TestHTTPLeaseClientCreateNon503NotRetried: a permanent refusal (400,
// 404, 429) is returned at once and never retried.
func TestHTTPLeaseClientCreateNon503NotRetried(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusTooManyRequests} {
		var attempts int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts++
			w.WriteHeader(code)
		}))
		c := NewHTTPLeaseClient(srv.URL, "tok")
		c.sleepFn = func(ctx context.Context, d time.Duration) error {
			t.Fatalf("slept on a %d refusal", code)
			return nil
		}
		if _, err := c.Create(context.Background(), "py-base", 600); err == nil {
			t.Fatalf("Create succeeded on %d, want error", code)
		}
		if attempts != 1 {
			t.Fatalf("create attempts on %d = %d, want 1", code, attempts)
		}
		srv.Close()
	}
}
