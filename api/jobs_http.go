package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
)

// HTTP surface of background exec jobs (2.6, #135):
// POST .../exec with "background": true starts one; GET .../jobs lists a
// lease's jobs; GET .../jobs/{job} returns the record (or long-polls);
// GET .../jobs/{job}/output streams the raw stdout/stderr by range; and
// POST .../jobs/{job}/signal signals the job's process group. All ride
// the same owner/admin/share access as exec.

// handleBackgroundExec answers the exec request with "background": true:
// start the job and 202 as soon as it is running.
func (s *Server) handleBackgroundExec(w http.ResponseWriter, r *http.Request, lease *Lease, owner, cmd, cwd string, env map[string]string, secrets map[string]string) {
	jobID, startedAt, proc, err := s.svc.startJob(r.Context(), lease, owner, cmd, cwd, env, secrets)
	if err != nil {
		switch {
		case errors.Is(err, errJobCap):
			writeError(w, http.StatusTooManyRequests, err.Error())
		case errors.Is(err, errLeaseBusySave):
			// A named-snapshot save holds the lease's secrets gate (2.7,
			// #83 B2): the job did not start, so retrying is safe.
			w.Header().Set("Retry-After", "5")
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "lease_busy"})
		case errors.Is(err, substrate.ErrNotFound):
			s.writeSandboxGone(w, lease)
		default:
			s.svc.log.Printf("exec background: %s: %v", lease.SandboxID, err)
			writeError(w, http.StatusInternalServerError, "failed to start background job")
		}
		return
	}
	// Watch the envd stream for the exit. The guest rc file is the source
	// of truth, so a stream that ends early only defers the notice to the
	// reconcile pass. Snapshot the sandbox and generation now: a cold
	// restart later must not redirect this watcher to a new guest.
	sandboxID, generation := lease.SandboxID, lease.Generation
	go s.svc.watchJob(proc, store.JobRow{
		JobID:      jobID,
		LeaseID:    lease.ID,
		Owner:      owner,
		Cmd:        cmd,
		Generation: generation,
	}, sandboxID, generation)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"job_id":     jobID,
		"started_at": startedAt.Format(time.RFC3339Nano),
	})
}

// jobsTarget resolves the caller's access to a lease's jobs: the same
// owner/admin/share gate as exec. nil (the caller answers 404)
// otherwise.
func (s *Server) jobsTarget(w http.ResponseWriter, r *http.Request) *Lease {
	owner := ownerFrom(r.Context())
	lease := s.svc.lookupWithShare(owner, r.PathValue("id"), ShareHTTP)
	if lease == nil {
		writeError(w, http.StatusNotFound, "lease not found")
		return nil
	}
	// Jobs live in the guest: a lost lease has none to list or reach.
	if !s.ensureLive(w, lease) {
		return nil
	}
	return lease
}

// jobTarget resolves a specific job under a lease. It answers 404 when
// the job is unknown or belongs to another lease.
func (s *Server) jobTarget(w http.ResponseWriter, r *http.Request, lease *Lease) (store.JobRow, bool) {
	row, err := s.svc.db.GetJob(r.Context(), r.PathValue("job"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "job not found")
		return store.JobRow{}, false
	}
	if err != nil {
		s.svc.log.Printf("jobs: get %s: %v", r.PathValue("job"), err)
		writeError(w, http.StatusInternalServerError, "job lookup failed")
		return store.JobRow{}, false
	}
	if row.LeaseID != lease.ID {
		writeError(w, http.StatusNotFound, "job not found")
		return store.JobRow{}, false
	}
	return row, true
}

// handleJobsList answers GET .../jobs: the lease's jobs, newest first.
func (s *Server) handleJobsList(w http.ResponseWriter, r *http.Request) {
	lease := s.jobsTarget(w, r)
	if lease == nil {
		return
	}
	s.svc.touch(lease.ID)
	rows, err := s.svc.db.ListJobs(r.Context(), lease.ID)
	if err != nil {
		s.svc.log.Printf("jobs: list %s: %v", lease.ID, err)
		writeError(w, http.StatusInternalServerError, "job list failed")
		return
	}
	out := make([]jobInfo, 0, len(rows))
	for _, row := range rows {
		out = append(out, jobInfoOf(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

// handleJobGet answers GET .../jobs/{job}: the record plus the last
// 64 KiB of stdout and stderr. With ?wait=<seconds> it long-polls until
// the job is no longer running or the wait ends.
func (s *Server) handleJobGet(w http.ResponseWriter, r *http.Request) {
	lease := s.jobsTarget(w, r)
	if lease == nil {
		return
	}
	s.svc.touch(lease.ID)
	row, ok := s.jobTarget(w, r, lease)
	if !ok {
		return
	}
	if wait := jobWaitSeconds(r); wait > 0 {
		row = s.waitJob(r, lease, row, wait)
	}
	resp := map[string]any{"job": jobInfoOf(row)}
	// Output is best effort, but the keys are always present so the shape
	// does not depend on the lease's state (a suspended lease's envd is
	// gone and returns nothing to read).
	resp["stdout"], resp["stderr"] = "", ""
	if out, err := s.svc.readJobOutput(r.Context(), lease.SandboxID, row.JobID, "stdout", jobStdoutReadBytes); err == nil {
		resp["stdout"] = out
	} else if !errors.Is(err, substrate.ErrNotFound) {
		resp["stdout_error"] = "output unavailable"
	}
	if errText, err := s.svc.readJobOutput(r.Context(), lease.SandboxID, row.JobID, "stderr", jobStderrReadBytes); err == nil {
		resp["stderr"] = errText
	} else if !errors.Is(err, substrate.ErrNotFound) {
		resp["stderr_error"] = "output unavailable"
	}
	writeJSON(w, http.StatusOK, resp)
}

// jobWaitSeconds parses ?wait=, clamping to jobMaxWaitSecs.
func jobWaitSeconds(r *http.Request) int {
	raw := r.URL.Query().Get("wait")
	if raw == "" {
		return 0
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return 0
	}
	if v > jobMaxWaitSecs {
		return jobMaxWaitSecs
	}
	return v
}

// waitJob polls a job record until it is no longer running or wait
// seconds pass, then returns the current record. It snapshots the sandbox
// and generation once: the guest files it reads are the job's own, never
// a new sandbox's after a cold restart.
func (s *Server) waitJob(r *http.Request, lease *Lease, row store.JobRow, wait int) store.JobRow {
	sandboxID, generation := lease.SandboxID, lease.Generation
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		if row.State != "running" {
			return row
		}
		if !time.Now().Before(deadline) {
			return row
		}
		select {
		case <-r.Context().Done():
			return row
		case <-t.C:
		}
		// Read the guest files in case the watcher missed the exit (a
		// stream that broke, a backend that restarted): the poll must not
		// depend on an in-process goroutine.
		s.svc.finishJobFromGuest(r.Context(), row, sandboxID, generation)
		next, err := s.svc.db.GetJob(r.Context(), row.JobID)
		if err != nil {
			return row
		}
		row = next
	}
}

// handleJobOutput answers GET .../jobs/{job}/output?stream=&offset=&limit=:
// raw bytes from the chosen stream, so a client can follow output.
func (s *Server) handleJobOutput(w http.ResponseWriter, r *http.Request) {
	lease := s.jobsTarget(w, r)
	if lease == nil {
		return
	}
	s.svc.touch(lease.ID)
	row, ok := s.jobTarget(w, r, lease)
	if !ok {
		return
	}
	stream := r.URL.Query().Get("stream")
	if stream == "" {
		stream = "stdout"
	}
	if stream != "stdout" && stream != "stderr" {
		writeError(w, http.StatusBadRequest, "stream must be stdout or stderr")
		return
	}
	offset, err := parseNonNeg(r.URL.Query().Get("offset"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "offset must be a non-negative integer")
		return
	}
	limit := jobOutputDefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = v
	}
	if limit > jobOutputMaxLimit {
		limit = jobOutputMaxLimit
	}
	data, err := s.svc.readJobRange(r.Context(), lease.SandboxID, row.JobID, stream, offset, limit)
	if err != nil {
		if errors.Is(err, substrate.ErrNotFound) {
			// No output yet (or the sandbox is gone): an empty answer.
			data = nil
		} else {
			s.svc.log.Printf("jobs: output %s %s: %v", row.JobID, stream, err)
			writeError(w, http.StatusBadGateway, "could not read job output")
			return
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// parseNonNeg parses a non-negative integer, defaulting to 0.
func parseNonNeg(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 {
		return 0, errors.New("negative")
	}
	return v, nil
}

// handleJobSignal answers POST .../jobs/{job}/signal: signal the job's
// process group. 409 when the job is not running.
func (s *Server) handleJobSignal(w http.ResponseWriter, r *http.Request) {
	lease := s.jobsTarget(w, r)
	if lease == nil {
		return
	}
	s.svc.touch(lease.ID)
	row, ok := s.jobTarget(w, r, lease)
	if !ok {
		return
	}
	if row.State != "running" {
		writeError(w, http.StatusConflict, "job is not running")
		return
	}
	// A suspended lease has no running sandbox; the job may still be
	// running in its paused guest, but the substrate cannot be reached
	// until the lease resumes. Answer 409 without calling the substrate.
	if s.svc.leaseSuspended(lease.ID) {
		writeLeaseSuspended(w)
		return
	}
	var req struct {
		Signal string `json:"signal"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Signal == "" {
		req.Signal = "TERM"
	}
	if req.Signal != "TERM" && req.Signal != "KILL" {
		writeError(w, http.StatusBadRequest, "signal must be TERM or KILL")
		return
	}
	if err := s.svc.signalJob(r.Context(), lease, row, req.Signal); err != nil {
		if errors.Is(err, substrate.ErrNotFound) {
			writeError(w, http.StatusConflict, "job is not running")
			return
		}
		s.svc.log.Printf("jobs: signal %s: %v", row.JobID, err)
		writeError(w, http.StatusBadGateway, "could not signal job")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job_id": row.JobID, "signal": req.Signal})
}

// signalJob signals a running job's process group. The job's pid was
// recorded by the wrapper in the job directory; the wrapper starts the
// command via setsid, so that pid is the process-group leader. The pid
// is guest-writable state, so it is validated as a plain number and
// passed as positional arguments to /bin/kill — it can never become
// shell input. A pid file that has not appeared yet (the 202 beat the
// wrapper's write) is waited for briefly, so a signal sent immediately
// after the start still reaches the job; only when it stays missing does
// the caller answer 409.
func (s *Service) signalJob(ctx context.Context, lease *Lease, row store.JobRow, sig string) error {
	pid, err := s.jobPID(ctx, lease.SandboxID, row.JobID)
	if err != nil {
		return err
	}
	// /bin/kill's -SIGNAL -PGID form through a fixed script, with the
	// signal and pid as arguments (never as part of the command text).
	// dash's kill does not accept a `--` separator, so the pid is written
	// straight after the dash; it is already a validated integer.
	res, err := s.sub.Exec(ctx, lease.SandboxID, substrate.ExecRequest{
		Args:    []string{"/bin/sh", "-c", `kill -"$1" -"$2"`, "kill", sig, strconv.Itoa(pid)},
		Timeout: 10 * time.Second,
	})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("kill exit %d: %s", res.ExitCode, res.Stderr)
	}
	// A successful signal exec proves the guest is alive (spoond-5ca).
	s.recordRootfsAlive(lease.ID)
	return nil
}

// jobPID reads and validates a running job's pid file, retrying briefly
// while the wrapper has not written it yet.
func (s *Service) jobPID(ctx context.Context, sandboxID, jobID string) (int, error) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := s.sub.ReadFile(ctx, sandboxID, jobPath(jobID, "pid"), 64)
		if err == nil {
			pid, perr := strconv.Atoi(strings.TrimSpace(string(data)))
			if perr != nil || pid <= 1 {
				return 0, fmt.Errorf("no valid pid recorded")
			}
			return pid, nil
		}
		if !errors.Is(err, substrate.ErrNotFound) {
			return 0, err
		}
		if !time.Now().Before(deadline) {
			return 0, err
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
