package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v4/store"
	"github.com/jrimmer/spoond/v4/substrate"

	"github.com/jrimmer/spoond/v4/substrate/fake"
)

// Stable error codes on the background-jobs routes (spoond-wir9): every
// HTTP refusal carries a machine-readable "code" beside the free-form
// "error" message. The code is the contract; the message is diagnostic.
// The statuses were already the contract and do not change.

// TestJobErrorCodesStart walks the start route's codes: the per-lease
// cap, the save gate, and the substrate-side start failures.
func TestJobErrorCodesStart(t *testing.T) {
	// The per-lease running cap: too_many_jobs.
	t.Run("cap -> too_many_jobs", func(t *testing.T) {
		ts, svc, _, sub := newTestServerWithService(t)
		id, _, _ := createJobLease(t, ts, svc)
		svc.cfg.MaxRunningJobsPerLease = 1

		procs := []*fake.FakeProcess{fake.NewProcess(2001), fake.NewProcess(2002)}
		i := 0
		sub.SetStartHandler(func(sandboxID string, req substrate.StartRequest) (substrate.Process, error) {
			p := procs[i%len(procs)]
			i++
			return p, nil
		})
		t.Cleanup(func() { sub.SetStartHandler(nil) })

		startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})
		resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/exec", "token-a",
			map[string]any{"cmd": "sleep 601", "background": true})
		execExpect(t, "second job over the cap", resp, body, http.StatusTooManyRequests, "too_many_jobs")
	})

	// A named-snapshot save holds the lease's secrets gate: the job does
	// not start, so the answer is lease_busy with its Retry-After.
	t.Run("save in progress -> lease_busy", func(t *testing.T) {
		ts, svc, _, _ := newTestServerWithService(t)
		id, _, _ := createJobLease(t, ts, svc)
		if !svc.secretsGate.beginSave(id) {
			t.Fatal("beginSave refused on a fresh lease")
		}
		defer svc.secretsGate.endSave(id)
		// Secrets bring the request under the gate: a job with none
		// would start without touching it.
		resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/exec", "token-a",
			map[string]any{"cmd": "sleep 600", "background": true, "secrets": map[string]string{"TOK": "v"}})
		execExpect(t, "job under a save", resp, body, http.StatusConflict, "lease_busy")
	})

	// A plain Start failure (the agent refused, the guest rejected the
	// argv): spawn_error.
	t.Run("start refused -> spawn_error", func(t *testing.T) {
		ts, svc, _, sub := newTestServerWithService(t)
		id, _, _ := createJobLease(t, ts, svc)
		sub.SetStartHandler(func(sandboxID string, req substrate.StartRequest) (substrate.Process, error) {
			return nil, errors.New("e2b: start i0: agent unreachable")
		})
		t.Cleanup(func() { sub.SetStartHandler(nil) })
		resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/exec", "token-a",
			map[string]any{"cmd": "sleep 600", "background": true})
		execExpect(t, "refused start", resp, body, http.StatusInternalServerError, "spawn_error")
	})

	// A confirmed not-found from Start: 410 lease_lost (existing
	// behaviour, now with its code).
	t.Run("sandbox gone -> lease_lost", func(t *testing.T) {
		ts, svc, _, sub := newTestServerWithService(t)
		id, _, _ := createJobLease(t, ts, svc)
		sub.SetStartHandler(func(sandboxID string, req substrate.StartRequest) (substrate.Process, error) {
			return nil, substrate.ErrNotFound
		})
		t.Cleanup(func() { sub.SetStartHandler(nil) })
		resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/exec", "token-a",
			map[string]any{"cmd": "sleep 600", "background": true})
		execExpect(t, "gone sandbox", resp, body, http.StatusGone, "lease_lost")
	})
}

// TestJobErrorCodesRecord walks the record route's codes: the unknown
// and foreign-job refusals and the jobs-DB failure.
func TestJobErrorCodesRecord(t *testing.T) {
	// An unknown job id: 404 job_not_found.
	t.Run("unknown job -> job_not_found", func(t *testing.T) {
		ts, svc, _, _ := newTestServerWithService(t)
		id, _, _ := createJobLease(t, ts, svc)
		resp, body := doReq(t, "GET", ts.URL+"/api/leases/"+id+"/jobs/nope", "token-a", nil)
		execExpect(t, "unknown job", resp, body, http.StatusNotFound, "job_not_found")
	})

	// Another lease's job: the record lookup is lease-scoped, so the
	// answer is job_not_found on both the record and the output route.
	t.Run("other lease's job -> job_not_found", func(t *testing.T) {
		ts, svc, _, sub := newTestServerWithService(t)
		id, _, _ := createJobLease(t, ts, svc)
		otherID, _, otherSandbox := createJobLease(t, ts, svc)

		p := fake.NewProcess(1009)
		installJobProcess(t, sub, p)
		jobID, _ := startBackgroundJob(t, ts, otherID, map[string]any{"cmd": "sleep 600"})
		writeJobFile(t, sub, otherSandbox, jobID, "pid", "1009\n")

		resp, body := doReq(t, "GET", ts.URL+"/api/leases/"+id+"/jobs/"+jobID, "token-a", nil)
		execExpect(t, "another lease's job", resp, body, http.StatusNotFound, "job_not_found")
		resp, body = doReq(t, "GET", ts.URL+"/api/leases/"+id+"/jobs/"+jobID+"/output", "token-a", nil)
		execExpect(t, "another lease's job output", resp, body, http.StatusNotFound, "job_not_found")
	})

	// A jobs DB failure on the record route: internal (the lease_jobs
	// table is dropped under the service through a second handle on the
	// same store file).
	t.Run("db failure -> internal", func(t *testing.T) {
		svc, sub, path := newStorePathService(t)
		_ = sub
		ts := httptest.NewServer(NewServer(svc, NewImageRegistry(svc.db)).Handler())
		t.Cleanup(ts.Close)
		id, _, _ := createJobLease(t, ts, svc)
		dropJobsTable(t, path)
		resp, body := doReq(t, "GET", ts.URL+"/api/leases/"+id+"/jobs/nope", "token-a", nil)
		execExpect(t, "job lookup failed", resp, body, http.StatusInternalServerError, "internal")
	})
}

// TestJobErrorCodesOutput walks the output route's parameter refusals
// and its substrate failure.
func TestJobErrorCodesOutput(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  int
		code  string
	}{
		{name: "bad stream -> EINVAL", query: "stream=stdin", want: 400, code: "EINVAL"},
		{name: "bad offset -> EINVAL", query: "offset=-2", want: 400, code: "EINVAL"},
		{name: "bad limit -> EINVAL", query: "limit=0", want: 400, code: "EINVAL"},
		{name: "substrate failure -> EIO", query: "", want: 502, code: "EIO"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, svc, db, sub := newTestServerWithService(t)
			id, _, sandbox := createJobLease(t, ts, svc)

			p := fake.NewProcess(1010)
			installJobProcess(t, sub, p)
			jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})
			writeJobFile(t, sub, sandbox, jobID, "stdout", "out\n")
			writeJobFile(t, sub, sandbox, jobID, "stderr", "err\n")
			row := waitJobState(t, db, jobID, "running", jobWatchTimeout)
			if row.State != "running" {
				t.Fatalf("job state %q, want running", row.State)
			}
			if tc.code == "EIO" {
				sub.FailCall("ReadFileRange", 0, errors.New("disk gone"))
			}
			// The output route answers raw bytes on success, so GET it
			// with the token attached and decode only the refusals.
			req, _ := http.NewRequest("GET", ts.URL+"/api/leases/"+id+"/jobs/"+jobID+"/output?"+tc.query, nil)
			req.Header.Set("Authorization", "Bearer token-a")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("output: %v", err)
			}
			defer resp.Body.Close()
			var body map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&body)
			execExpect(t, tc.name, resp, body, tc.want, tc.code)
		})
	}
}

// TestJobErrorCodesSignal walks the signal route's codes.
func TestJobErrorCodesSignal(t *testing.T) {
	// A finished job: the record check comes before the resume, so a
	// signalled finished job answers 409 job_not_running.
	t.Run("finished job -> job_not_running", func(t *testing.T) {
		ts, svc, db, sub := newTestServerWithService(t)
		id, _, sandbox := createJobLease(t, ts, svc)
		p := fake.NewProcess(1011)
		installJobProcess(t, sub, p)
		jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})
		writeJobFile(t, sub, sandbox, jobID, "rc", "0\n")
		p.Push(substrate.ProcessEvent{Kind: substrate.EventExit, ExitCode: 0})
		waitJobState(t, db, jobID, "exited", jobWatchTimeout)
		resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/jobs/"+jobID+"/signal", "token-a",
			map[string]any{"signal": "TERM"})
		execExpect(t, "finished job", resp, body, http.StatusConflict, "job_not_running")
	})

	// A running job whose pid file never appears: the wrapper never
	// recorded a pid, so the process group is unreachable — the same
	// job_not_running (after jobPID's wait, which the test shortens).
	t.Run("pid never recorded -> job_not_running", func(t *testing.T) {
		old := jobPIDWait
		jobPIDWait = 10 * time.Millisecond
		t.Cleanup(func() { jobPIDWait = old })

		ts, svc, _, sub := newTestServerWithService(t)
		id, _, _ := createJobLease(t, ts, svc)
		installJobProcess(t, sub, fake.NewProcess(1012))
		jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})
		resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/jobs/"+jobID+"/signal", "token-a",
			map[string]any{"signal": "TERM"})
		execExpect(t, "pid never recorded", resp, body, http.StatusConflict, "job_not_running")
	})

	// The kill exec could not be issued: 502 EIO.
	t.Run("kill failed -> EIO", func(t *testing.T) {
		ts, svc, _, sub := newTestServerWithService(t)
		id, _, sandbox := createJobLease(t, ts, svc)
		installJobProcess(t, sub, fake.NewProcess(1013))
		jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})
		writeJobFile(t, sub, sandbox, jobID, "pid", "1013\n")
		sub.FailCall("Exec", 0, errors.New("agent stalled"))
		resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/jobs/"+jobID+"/signal", "token-a",
			map[string]any{"signal": "TERM"})
		execExpect(t, "kill failed", resp, body, http.StatusBadGateway, "EIO")
	})

	// The kill exec could not be decided (the orchestrator stalled):
	// 503 substrate_unavailable.
	t.Run("kill unavailable -> substrate_unavailable", func(t *testing.T) {
		ts, svc, _, sub := newTestServerWithService(t)
		id, _, sandbox := createJobLease(t, ts, svc)
		installJobProcess(t, sub, fake.NewProcess(1014))
		jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})
		writeJobFile(t, sub, sandbox, jobID, "pid", "1014\n")
		sub.FailCall("Exec", 0, substrate.ErrUnavailable)
		resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/jobs/"+jobID+"/signal", "token-a",
			map[string]any{"signal": "TERM"})
		execExpect(t, "kill unavailable", resp, body, http.StatusServiceUnavailable, "substrate_unavailable")
	})

	// An unknown signal: 400 EINVAL. A malformed body: 400 bad_request.
	t.Run("unknown signal -> EINVAL", func(t *testing.T) {
		ts, svc, _, sub := newTestServerWithService(t)
		id, _, sandbox := createJobLease(t, ts, svc)
		installJobProcess(t, sub, fake.NewProcess(1015))
		jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})
		writeJobFile(t, sub, sandbox, jobID, "pid", "1015\n")
		resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/jobs/"+jobID+"/signal", "token-a",
			map[string]any{"signal": "QUIT"})
		execExpect(t, "unknown signal", resp, body, http.StatusBadRequest, "EINVAL")
		resp, body = doReqRaw(t, "POST", ts.URL+"/api/leases/"+id+"/jobs/"+jobID+"/signal", "token-a", "{not json")
		execExpect(t, "malformed body", resp, body, http.StatusBadRequest, "bad_request")
	})
}

// TestJobErrorCodesRoutePrologue pins the shared route prologue: an
// unknown lease is 404 not_found on every jobs route (each with its own
// method; a wrong method is the mux's 405 before any handler runs).
func TestJobErrorCodesRoutePrologue(t *testing.T) {
	_, svc, _, _ := newTestServerWithService(t)
	ts := httptest.NewServer(NewServer(svc, NewImageRegistry(svc.db)).Handler())
	t.Cleanup(ts.Close)
	for _, c := range []struct {
		path, method string
	}{
		{"/api/leases/unknown-lease/jobs", http.MethodGet},
		{"/api/leases/unknown-lease/jobs/j-1", http.MethodGet},
		{"/api/leases/unknown-lease/jobs/j-1/output", http.MethodGet},
		{"/api/leases/unknown-lease/jobs/j-1/signal", http.MethodPost},
	} {
		resp, body := doReq(t, c.method, ts.URL+c.path, "token-b", map[string]any{"signal": "TERM"})
		execExpect(t, c.path, resp, body, http.StatusNotFound, "not_found")
	}
}

// newStorePathService builds a service whose store lives at a known
// path (so a test can reach the same file through a second handle).
func newStorePathService(t *testing.T) (svc *Service, sub *testSub, path string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "spoond.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	sub = newTestSub()
	seedImage(t, db, "py-base", 2048)
	svc = NewService(sub, db, map[string]string{
		"token-a": "consumer-a", "token-b": "consumer-b",
	}, ServiceConfig{DefaultTTL: 60 * time.Second, MaxTTL: 10 * time.Minute})
	svc.log = log.New(io.Discard, "", 0)
	return svc, sub, path
}

// dropJobsTable drops the lease_jobs table through a second handle on
// the store file, so the service's own job lookups start failing (the
// sqlite driver is registered by the store package's import).
func dropJobsTable(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("second store handle: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`DROP TABLE lease_jobs`); err != nil {
		t.Fatalf("drop lease_jobs: %v", err)
	}
}

// doReqRaw posts a body that is already bytes (the malformed-JSON
// cases) and decodes the answer.
func doReqRaw(t *testing.T, method, url, token, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request %s %s: %v", method, url, err)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	return resp, out
}
