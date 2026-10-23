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

	"github.com/jrimmer/spoond/v4/store"
	"github.com/jrimmer/spoond/v4/substrate"
)

// HTTP surface of background exec jobs (2.6, #135):
// POST .../exec with "background": true starts one; GET .../jobs lists a
// lease's jobs; GET .../jobs/{job} returns the record (or long-polls);
// GET .../jobs/{job}/output streams the raw stdout/stderr by range; and
// POST .../jobs/{job}/signal signals the job's process group. All ride
// the same owner/admin/share access as exec.

// handleBackgroundExec answers the exec request with "background": true:
// start the job and 202 as soon as it is running.
func (s *Server) handleBackgroundExec(w http.ResponseWriter, r *http.Request, lease *Lease, owner, cmd, cwd string, env map[string]string, secrets map[string]string, maxRuntimeSecs, retentionSecs, outputMaxBytes int64) {
	jobID, startedAt, proc, err := s.svc.startJob(r.Context(), lease, owner, cmd, cwd, env, secrets, maxRuntimeSecs, retentionSecs, outputMaxBytes)
	if err != nil {
		switch {
		case errors.Is(err, errJobCap):
			writeErrorCode(w, http.StatusTooManyRequests, "too_many_jobs", err.Error())
		case errors.Is(err, errLeaseBusySave):
			// A named-snapshot save holds the lease's secrets gate (2.7,
			// #83 B2): the job did not start, so retrying is safe.
			w.Header().Set("Retry-After", "5")
			writeErrorCode(w, http.StatusConflict, "lease_busy", err.Error())
		case errors.Is(err, substrate.ErrNotFound), errors.Is(err, substrate.ErrUnavailable):
			// A confirmed not-found is 409 busy / 410 lease_lost; an
			// unavailable decision is 503 substrate_unavailable and the
			// lease is kept.
			s.writeSandboxOpErrorFor(w, lease, "exec background", err)
		default:
			// The command could never start (the agent refused, the guest
			// rejected the argv): 500 spawn_error.
			s.svc.log.Printf("exec background: %s: %v", lease.SandboxID, err)
			writeErrorCode(w, http.StatusInternalServerError, "spawn_error", "failed to start background job")
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
		writeErrorCode(w, http.StatusNotFound, "not_found", "lease not found")
		return nil
	}
	// Jobs live in the guest: a lost lease has none to list or reach.
	if !s.ensureLive(w, lease) {
		return nil
	}
	return lease
}

// jobTarget resolves a specific job under a lease. It answers 404
// job_not_found when the job is unknown or belongs to another lease.
func (s *Server) jobTarget(w http.ResponseWriter, r *http.Request, lease *Lease) (store.JobRow, bool) {
	row, err := s.svc.db.GetJob(r.Context(), r.PathValue("job"))
	if errors.Is(err, store.ErrNotFound) {
		writeErrorCode(w, http.StatusNotFound, "job_not_found", "job not found")
		return store.JobRow{}, false
	}
	if err != nil {
		s.svc.log.Printf("jobs: get %s: %v", r.PathValue("job"), err)
		writeErrorCode(w, http.StatusInternalServerError, "internal", "job lookup failed")
		return store.JobRow{}, false
	}
	if row.LeaseID != lease.ID {
		writeErrorCode(w, http.StatusNotFound, "job_not_found", "job not found")
		return store.JobRow{}, false
	}
	return row, true
}

// endedJobs resolves the jobs of a lease that has ended (ended_jobs):
// ok when the caller's lease is gone and was the caller's own, rows its
// archived jobs. Shares end with the lease, so only its owner reads
// them. ok false sends the caller on to the live lookup and its 404.
func (s *Server) endedJobs(r *http.Request) ([]store.JobRow, bool) {
	owner, id := ownerFrom(r.Context()), r.PathValue("id")
	if s.svc.lookupWithShare(owner, id, ShareHTTP) != nil {
		return nil, false
	}
	rows, leaseOwner, err := s.svc.db.ListEndedJobs(r.Context(), id)
	if err != nil {
		s.svc.log.Printf("jobs: list ended %s: %v", id, err)
		return nil, false
	}
	if len(rows) == 0 || leaseOwner != owner {
		return nil, false
	}
	return rows, true
}

// handleJobsList answers GET .../jobs: the lease's jobs, newest first.
// Once the lease has ended its owner still gets the archived records,
// with "lease_ended": true, until the job retention window prunes them.
func (s *Server) handleJobsList(w http.ResponseWriter, r *http.Request) {
	if rows, ok := s.endedJobs(r); ok {
		out := make([]jobInfo, 0, len(rows))
		for _, row := range rows {
			out = append(out, jobInfoOf(row))
		}
		writeJSON(w, http.StatusOK, map[string]any{"jobs": out, "lease_ended": true})
		return
	}
	lease := s.jobsTarget(w, r)
	if lease == nil {
		return
	}
	s.svc.touch(lease.ID)
	rows, err := s.svc.db.ListJobs(r.Context(), lease.ID)
	if err != nil {
		s.svc.log.Printf("jobs: list %s: %v", lease.ID, err)
		writeErrorCode(w, http.StatusInternalServerError, "internal", "job list failed")
		return
	}
	out := make([]jobInfo, 0, len(rows))
	for _, row := range rows {
		out = append(out, jobInfoOf(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

// handleJobGet answers GET .../jobs/{job}: the record plus the last
// 64 KiB of stdout and stderr, naming what the output cap (spoond-hwto)
// has dropped as "skipped" beside it. With ?wait=<seconds> it long-polls
// until the job is no longer running or the wait ends.
func (s *Server) handleJobGet(w http.ResponseWriter, r *http.Request) {
	// An ended lease's job: the archived record, its stderr tail as the
	// stderr (the guest's files went with the sandbox) and no stdout.
	if rows, ok := s.endedJobs(r); ok {
		for _, row := range rows {
			if row.JobID == r.PathValue("job") {
				writeJSON(w, http.StatusOK, map[string]any{
					"job": jobInfoOf(row), "stdout": "", "stderr": row.StderrTail, "lease_ended": true,
				})
				return
			}
		}
		writeErrorCode(w, http.StatusNotFound, "job_not_found", "job not found")
		return
	}
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
	out, outStart, outSkip, err := s.svc.readJobOutput(r.Context(), lease.SandboxID, row.JobID, "stdout", jobStdoutReadBytes, row)
	if err == nil {
		resp["stdout"] = out
		if outSkip.Bytes > 0 {
			resp["stdout_offset"] = outStart
			resp["stdout_skipped"] = outSkip
		}
	} else if errors.Is(err, substrate.ErrUnavailable) {
		s.writeSubstrateUnavailable(w)
		return
	} else if !errors.Is(err, substrate.ErrNotFound) {
		resp["stdout_error"] = "output unavailable"
	}
	errText, errStart, errSkip, errRead := s.svc.readJobOutput(r.Context(), lease.SandboxID, row.JobID, "stderr", jobStderrReadBytes, row)
	if errRead == nil {
		resp["stderr"] = errText
		if errSkip.Bytes > 0 {
			resp["stderr_offset"] = errStart
			resp["stderr_skipped"] = errSkip
		}
	} else if errors.Is(errRead, substrate.ErrUnavailable) {
		s.writeSubstrateUnavailable(w)
		return
	} else if !errors.Is(errRead, substrate.ErrNotFound) {
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

// handleJobDelete answers DELETE .../jobs/{job} (spoond-hwto): the job
// record is deleted and its guest directory removed, so an owner can
// reclaim a chatty job's disk before the retention window. A running job
// answers 409 (signal it first, like the other lifecycle paths); the
// record is resolved before any resume, so deleting a finished job
// cannot wake a suspended lease for nothing. The guest cleanup is
// best-effort: a lease paused mid-delete may put the files back on its
// next resume, and with the record gone nothing prunes them again — they
// then live until the lease itself is released (or the owner deletes
// them the same way once the lease is running again). The 204 answers
// for the record; a cleanup that could not reach the guest is logged,
// not failed, matching the prune path.
func (s *Server) handleJobDelete(w http.ResponseWriter, r *http.Request) {
	lease := s.jobsTarget(w, r)
	if lease == nil {
		return
	}
	s.svc.touch(lease.ID)
	row, ok := s.jobTarget(w, r, lease)
	if !ok {
		return
	}
	if row.State == "running" {
		writeErrorCode(w, http.StatusConflict, "job_running", "job is running; signal it first")
		return
	}
	// Remove the guest directory first, while the record still exists: a
	// client that retries after a failure finds a removable record rather
	// than an orphaned directory nothing points at any more. The lease
	// here is live (jobsTarget checked), so files are reachable unless the
	// sandbox is mid-operation; cleanupJobFiles logs what it could not
	// remove and the row is still deleted below, matching the prune path.
	s.svc.cleanupJobFiles(r.Context(), lease.SandboxID, row.JobID)
	if _, err := s.svc.db.DeleteJob(r.Context(), row.JobID); err != nil {
		s.svc.log.Printf("jobs: delete %s: %v", row.JobID, err)
		writeErrorCode(w, http.StatusInternalServerError, "internal", "job delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
		writeErrorCode(w, http.StatusBadRequest, "EINVAL", "stream must be stdout or stderr")
		return
	}
	offset, err := parseNonNeg(r.URL.Query().Get("offset"))
	if err != nil {
		writeErrorCode(w, http.StatusBadRequest, "EINVAL", "offset must be a non-negative integer")
		return
	}
	limit := jobOutputDefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 {
			writeErrorCode(w, http.StatusBadRequest, "EINVAL", "limit must be a positive integer")
			return
		}
		limit = v
	}
	if limit > jobOutputMaxLimit {
		limit = jobOutputMaxLimit
	}
	data, start, skip, err := s.svc.readJobRange(r.Context(), lease.SandboxID, row.JobID, stream, offset, limit, row)
	if err != nil {
		if errors.Is(err, substrate.ErrUnavailable) {
			s.writeSubstrateUnavailable(w)
			return
		}
		if errors.Is(err, substrate.ErrNotFound) {
			// No output yet (or the sandbox is gone): an empty answer.
			data = nil
		} else {
			// The substrate's read failed for a reason that is neither a
			// confirmed absence nor an unknown state: 502 EIO.
			s.svc.log.Printf("jobs: output %s %s: %v", row.JobID, stream, err)
			writeErrorCode(w, http.StatusBadGateway, "EIO", "could not read job output")
			return
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	// The absolute byte offset the answer starts at: the request's
	// offset when it lies past the cap's dropped front, else the first
	// kept byte (never an error, but a follower must know where it is
	// resuming from). Only sent once the cap has dropped something, so
	// the uncapped shape is byte-identical to before (spoond-hwto).
	if skip.Bytes > 0 {
		w.Header().Set("X-Output-Offset", strconv.FormatInt(start, 10))
		scrubbed, _ := json.Marshal(skip)
		w.Header().Set("X-Output-Skipped", string(scrubbed))
	}
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
// process group. 409 job_not_running when the job is not running. The
// job record is checked BEFORE the lease resumes (review R5): signalling
// a finished job must not spend hugepages resuming a suspended lease for
// nothing.
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
		writeErrorCode(w, http.StatusConflict, "job_not_running", "job is not running")
		return
	}
	// Signalling a running job is work: a suspended lease resumes first,
	// whatever suspended it (#145 D2). A refused resume answers and the
	// caller stops here.
	if !s.ensureRunning(w, r, lease) {
		return
	}
	var req struct {
		Signal string `json:"signal"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Signal == "" {
		req.Signal = "TERM"
	}
	if req.Signal != "TERM" && req.Signal != "KILL" {
		writeErrorCode(w, http.StatusBadRequest, "EINVAL", "signal must be TERM or KILL")
		return
	}
	if err := s.svc.signalJob(r.Context(), lease.ID, lease.SandboxID, row, req.Signal); err != nil {
		if errors.Is(err, substrate.ErrUnavailable) {
			s.writeSubstrateUnavailableFor(w, lease, "job signal")
			return
		}
		if errors.Is(err, substrate.ErrNotFound) {
			// The wrapper never recorded a pid (or the guest lost it): the
			// job's process group is unreachable, like a finished job.
			writeErrorCode(w, http.StatusConflict, "job_not_running", "job is not running")
			return
		}
		// The kill could not be issued, or it ran and failed: 502 EIO.
		s.svc.log.Printf("jobs: signal %s: %v", row.JobID, err)
		writeErrorCode(w, http.StatusBadGateway, "EIO", "could not signal job")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job_id": row.JobID, "signal": req.Signal})
}

// signalJob signals a running job's process group in sandboxID. The
// job's pid was recorded by the wrapper in the job directory; the wrapper
// starts the command via setsid, so that pid is the process-group
// leader. The pid is guest-writable state, so it is validated as a plain
// number and passed as positional arguments to /bin/kill — it can never
// become shell input. A pid file that has not appeared yet (the 202 beat
// the wrapper's write) is waited for briefly, so a signal sent
// immediately after the start still reaches the job; only when it stays
// missing does the caller answer 409. The sandbox id and generation are
// passed in rather than read off a *Lease, so a caller that already
// snapshotted them (reconcile) never races a resume or restore.
func (s *Service) signalJob(ctx context.Context, leaseID, sandboxID string, row store.JobRow, sig string) error {
	pid, err := s.jobPID(ctx, sandboxID, row.JobID)
	if err != nil {
		return err
	}
	// /bin/kill's -SIGNAL -PGID form through a fixed script, with the
	// signal and pid as arguments (never as part of the command text).
	// dash's kill does not accept a `--` separator, so the pid is written
	// straight after the dash; it is already a validated integer.
	res, err := s.sub.Exec(ctx, sandboxID, substrate.ExecRequest{
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
	s.recordRootfsAlive(leaseID)
	return nil
}

// jobPIDWait bounds how long jobPID retries for a pid file the wrapper
// has not written yet (the 202 can beat the wrapper).
var jobPIDWait = 2 * time.Second

// jobPID reads and validates a running job's pid file, retrying briefly
// while the wrapper has not written it yet.
func (s *Service) jobPID(ctx context.Context, sandboxID, jobID string) (int, error) {
	deadline := time.Now().Add(jobPIDWait)
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
			return 0, fmt.Errorf("%w: %w", errJobPIDMissing, substrate.ErrNotFound)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
