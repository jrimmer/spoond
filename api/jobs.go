package api

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
)

// Background exec jobs (2.6, #135): the exec API with "background": true
// starts a command in the caller's lease, records it in lease_jobs, and
// reports its end as an event. This is an extension of exec, not a new
// subsystem: the command runs in the lease's guest, the guest wrapper
// records stdout/stderr/rc under /var/lib/spoond/jobs/<job_id>/, and the
// backend tracks the record. The guest files — never the envd stream —
// are the source of truth, so a backend restart or a broken stream does
// not lose an outcome.
//
// The guest wrapper runs the command in its own session (setsid), so the
// command is detached from the envd Start stream's lifetime: when the
// stream dies (a backend restart) the command keeps running and still
// writes its rc file. While the stream lives, the backend notices the
// exit at once; afterwards a reconcile pass reads the guest files.

const (
	// jobsDir is the guest directory holding one subdirectory per job:
	// stdout, stderr, pid and (once the command ends) rc.
	jobsDir = "/var/lib/spoond/jobs"
	// jobStderrTailBytes is how much stderr the record keeps (last
	// 4 KiB, spec item 4).
	jobStderrTailBytes = 4 << 10
	// jobStdoutReadBytes is how much stdout the record view returns
	// (last 64 KiB, spec item 6).
	jobStdoutReadBytes = 64 << 10
	// jobStderrReadBytes matches jobStdoutReadBytes for the stderr view.
	jobStderrReadBytes = 64 << 10
	// jobOutputDefaultLimit and jobOutputMaxLimit bound the raw output
	// endpoint (?limit=), 1 MiB default and 16 MiB max.
	jobOutputDefaultLimit = 1 << 20
	jobOutputMaxLimit     = 16 << 20
	// jobMaxWaitSecs caps ?wait= on the job read endpoint (spec item 6).
	jobMaxWaitSecs = 900
	// jobReconcileEvery paces the reconcile pass while any job runs.
	jobReconcileEvery = 10 * time.Second
	// jobEventCmdChars cuts the command in a job_started event detail.
	jobEventCmdChars = 120
	// jobEventDetailBytes caps the stderr excerpt in a job_exited event
	// detail (spec item 8: the last 10 lines, at most 1 KiB total).
	jobEventDetailBytes = 1 << 10
	// jobEventStderrLines is how many stderr lines the job_exited detail
	// carries.
	jobEventStderrLines = 10
)

// DefaultMaxRunningJobsPerLease and DefaultJobRetentionSecs are the
// MAX_RUNNING_JOBS_PER_LEASE and JOB_RETENTION_SECS defaults.
const (
	DefaultMaxRunningJobsPerLease = 16
	DefaultJobRetentionSecs       = 604800 // 7 days
)

// errJobCap is returned when a lease is at its running-job cap.
var errJobCap = errors.New("too many running background jobs for this lease")

// jobWrapperScript is the guest-side wrapper. It starts the command as a
// shell invocation in its own session (setsid), records its pid, waits
// for it, then writes the exit code atomically (rc.tmp, then rename) and
// removes the job's staged secrets. It stays attached to the envd stream
// so an in-flight backend notices the exit at once; if the stream dies
// first, the detached command keeps running and still writes its files.
//
// argv layout: /bin/bash -c <jobWrapperScript> bash <job_id> <cmd argv...>
const jobWrapperScript = `job_id="$1"; shift
dir="/var/lib/spoond/jobs/$job_id"
mkdir -p "$dir" || exit 1
setsid "$@" >"$dir/stdout" 2>"$dir/stderr" </dev/null &
child=$!
echo "$child" > "$dir/pid"
wait "$child"
rc=$?
printf '%d\n' "$rc" >"$dir/rc.tmp" && mv "$dir/rc.tmp" "$dir/rc"
if [ -f "$dir/secrets" ]; then
  while IFS= read -r name; do
    [ -n "$name" ] && rm -f "/run/secrets/$name"
  done <"$dir/secrets"
  rm -f "$dir/secrets"
fi
exit "$rc"
`

func jobResult(exitCode int) string {
	if exitCode == 0 {
		return "ok"
	}
	return "error"
}

// jobInfo is the API view of one lease_jobs row.
type jobInfo struct {
	JobID      string `json:"job_id"`
	LeaseID    string `json:"lease_id"`
	Owner      string `json:"owner"`
	Cmd        string `json:"cmd"`
	Cwd        string `json:"cwd,omitempty"`
	State      string `json:"state"`
	ExitCode   *int   `json:"exit_code"`
	StartedAt  string `json:"started_at"`
	EndedAt    string `json:"ended_at,omitempty"`
	StderrTail string `json:"stderr_tail,omitempty"`
}

// jobInfoOf renders a store row for the API.
func jobInfoOf(r store.JobRow) jobInfo {
	return jobInfo{
		JobID:      r.JobID,
		LeaseID:    r.LeaseID,
		Owner:      r.Owner,
		Cmd:        r.Cmd,
		Cwd:        r.Cwd,
		State:      r.State,
		ExitCode:   r.ExitCode,
		StartedAt:  r.StartedAt.UTC().Format(time.RFC3339Nano),
		EndedAt:    formatRFC3339(r.EndedAt),
		StderrTail: r.StderrTail,
	}
}

// jobPath returns the guest path of one job file.
func jobPath(jobID, name string) string {
	return jobsDir + "/" + jobID + "/" + name
}

// maxRunningJobs returns the configured per-lease running-job cap, or
// the default.
func (s *Service) maxRunningJobs() int {
	if s.cfg.MaxRunningJobsPerLease > 0 {
		return s.cfg.MaxRunningJobsPerLease
	}
	return DefaultMaxRunningJobsPerLease
}

// jobRetention returns the configured exited-job retention, or the
// default.
func (s *Service) jobRetention() time.Duration {
	secs := s.cfg.JobRetentionSecs
	if secs <= 0 {
		secs = DefaultJobRetentionSecs
	}
	return time.Duration(secs) * time.Second
}

// buildJobWrapperArgs builds the argv handed to substrate.Start: the
// wrapper script, the job id, and the command as a shell invocation. cwd
// and env apply the same way exec's buildShellArgs applies them, but the
// env values come from envFile (a shell file the command sources) rather
// than argv, so no value is exposed in the process table. Secret values
// are staged as files under /run/secrets, never passed here.
func buildJobWrapperArgs(jobID, cmd, cwd, envFile string) []string {
	body := jobCommandBody(cmd, cwd, envFile)
	args := []string{"/bin/bash", "-c", jobWrapperScript, "bash", jobID}
	return append(args, "/bin/bash", "-c", body)
}

// jobCommandBody renders the command as a bash -c body: source the env
// file when present, change directory when cwd is set, then run cmd.
func jobCommandBody(cmd, cwd, envFile string) string {
	var parts []string
	if envFile != "" {
		parts = append(parts, ". "+shellQuote(envFile))
	}
	if cwd != "" {
		parts = append(parts, "cd "+shellQuote(cwd))
	}
	parts = append(parts, cmd)
	return strings.Join(parts, " && ")
}

// startJob starts a background exec. It returns the job id, start time
// and the envd Process carrying the stream (the caller watches it), or
// an error the caller maps onto an HTTP status.
func (s *Service) startJob(ctx context.Context, lease *Lease, owner, cmd, cwd string, env map[string]string, secrets map[string]string) (string, time.Time, substrate.Process, error) {
	// The per-lease cap is a check-then-insert against SQLite: hold one
	// lock for both so two concurrent starts for one lease cannot both
	// pass it.
	s.jobStartMu.Lock()
	defer s.jobStartMu.Unlock()
	n, err := s.db.CountRunningJobs(ctx, lease.ID)
	if err != nil {
		return "", time.Time{}, nil, err
	}
	if n >= s.maxRunningJobs() {
		return "", time.Time{}, nil, errJobCap
	}

	jobID := newID()
	startedAt := time.Now().UTC()

	// Stage the job's secrets before the wrapper runs. The guest wrapper
	// removes them at exit; the backend removes them on reconcile if the
	// wrapper never got that far. Only the names (never values) are
	// recorded, in the job directory and in memory.
	secretNames := sortedSecretNames(secrets)
	if len(secrets) > 0 {
		if err := s.stageSecrets(ctx, lease.SandboxID, secrets); err != nil {
			return "", time.Time{}, nil, fmt.Errorf("stage secrets: %w", err)
		}
		// A names-only list the wrapper reads to clean up. Values are
		// never written here.
		if err := s.sub.WriteFile(ctx, lease.SandboxID, jobPath(jobID, "secrets"),
			[]byte(strings.Join(secretNames, "\n")+"\n"), 0o600); err != nil {
			return "", time.Time{}, nil, fmt.Errorf("write job secrets list: %w", err)
		}
	}

	// The command's env (and the pooled-lease id) is written to a file the
	// command shell sources, so no value reaches argv, the recorded
	// command, the logs or an event (spec item 3).
	envFile := ""
	fullEnv := requestEnv(lease, env)
	if len(fullEnv) > 0 {
		var b strings.Builder
		for _, k := range sortedKeys(fullEnv) {
			fmt.Fprintf(&b, "export %s=%s\n", shellQuote(k), shellQuote(fullEnv[k]))
		}
		envFile = jobPath(jobID, "env")
		if err := s.sub.WriteFile(ctx, lease.SandboxID, envFile, []byte(b.String()), 0o600); err != nil {
			if len(secretNames) > 0 {
				s.removeSecrets(lease.SandboxID, secretNames)
			}
			return "", time.Time{}, nil, fmt.Errorf("write job env: %w", err)
		}
	}

	args := buildJobWrapperArgs(jobID, cmd, cwd, envFile)
	proc, err := s.sub.Start(ctx, lease.SandboxID, substrate.StartRequest{Args: args})
	if err != nil {
		if len(secretNames) > 0 {
			s.removeSecrets(lease.SandboxID, secretNames)
		}
		return "", time.Time{}, nil, fmt.Errorf("start: %w", err)
	}

	if err := s.db.InsertJob(ctx, store.JobRow{
		JobID:      jobID,
		LeaseID:    lease.ID,
		Owner:      owner,
		Cmd:        cmd,
		Cwd:        cwd,
		State:      "running",
		StartedAt:  startedAt,
		Generation: lease.Generation,
	}); err != nil {
		// The process is running but unrecorded. Do not leave it: a
		// best-effort TERM and the (empty) record-free result.
		_ = proc.Signal(false)
		_ = proc.Close()
		if len(secretNames) > 0 {
			s.removeSecrets(lease.SandboxID, secretNames)
		}
		return "", time.Time{}, nil, err
	}
	if len(secretNames) > 0 {
		s.recordJobSecrets(jobID, secretNames)
	}
	s.touch(lease.ID)
	if s.metrics != nil {
		s.metrics.JobsRunning.Inc()
	}
	s.incRunningJob(lease.ID)
	s.emitLeaseEvent(lease.ID, owner, LeaseJobStarted, cutDetail(cmd, jobEventCmdChars))
	return jobID, startedAt, proc, nil
}

// watchJob notices the job's exit through the live envd stream. It runs
// in its own goroutine after the 202 answers; the guest rc file is the
// source of truth, so a stream that ends without an exit event (a
// backend restart) leaves the record running for the reconcile pass.
func (s *Service) watchJob(proc substrate.Process, job store.JobRow, lease *Lease) {
	for ev := range proc.Events() {
		switch ev.Kind {
		case substrate.EventExit:
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			s.finishJobFromGuest(ctx, job, lease)
			cancel()
			_ = proc.Close()
			return
		case substrate.EventError:
			// The stream broke: the detached command may still be running,
			// so leave the record for the reconcile pass.
			s.log.Printf("jobs: %s: stream error: %s", job.JobID, ev.Err)
			_ = proc.Close()
			return
		}
	}
}

// finishJobFromGuest reads a job's rc and stderr tail from the guest
// files and finishes the record. It is a no-op when rc is not there yet.
func (s *Service) finishJobFromGuest(ctx context.Context, job store.JobRow, lease *Lease) {
	code, done, err := s.readJobRC(ctx, lease.SandboxID, job.JobID)
	if err != nil {
		s.log.Printf("jobs: %s: read rc: %v", job.JobID, err)
		return
	}
	if !done {
		return
	}
	stderr, _ := s.readJobStderrTail(ctx, lease.SandboxID, job.JobID, jobStderrTailBytes)
	if err := s.finishJob(ctx, job, code, stderr); err != nil {
		s.log.Printf("jobs: %s: finish: %v", job.JobID, err)
	}
}

// recordJobSecrets remembers the secret names staged for a running job.
func (s *Service) recordJobSecrets(jobID string, names []string) {
	s.secretsMu.Lock()
	defer s.secretsMu.Unlock()
	if s.liveJobSecrets == nil {
		s.liveJobSecrets = map[string][]string{}
	}
	s.liveJobSecrets[jobID] = append([]string(nil), names...)
}

// takeJobSecrets removes and returns the secret names remembered for a
// job.
func (s *Service) takeJobSecrets(jobID string) []string {
	s.secretsMu.Lock()
	defer s.secretsMu.Unlock()
	names := s.liveJobSecrets[jobID]
	delete(s.liveJobSecrets, jobID)
	return names
}

// removeJobSecrets deletes a job's staged secret files and, for names
// that shadow a create-time secret, restores the lease's value (#80).
// Best effort: the guest wrapper normally did this already.
func (s *Service) removeJobSecrets(lease *Lease, jobID string) {
	names := s.takeJobSecrets(jobID)
	if len(names) == 0 {
		return
	}
	s.removeSecrets(lease.SandboxID, names)
	if create := s.createSecretsFor(lease.ID); len(create) > 0 {
		var shadowed []string
		for _, name := range names {
			if _, ok := create[name]; ok {
				shadowed = append(shadowed, name)
			}
		}
		if len(shadowed) > 0 {
			s.restageSecrets(lease.SandboxID, shadowed, create)
		}
	}
}

// finishJob marks a running job exited from the guest's rc file and
// emits the event. It is idempotent: an already-finished record is left
// alone, so the watcher and the reconcile pass cannot both count it.
func (s *Service) finishJob(ctx context.Context, job store.JobRow, exitCode int, stderrTail string) error {
	changed, err := s.db.UpdateJobExit(ctx, job.JobID, exitCode, time.Now().UTC(), stderrTail)
	if err != nil {
		return err
	}
	if !changed {
		return nil // another path already finished it
	}
	s.emitLeaseEvent(job.LeaseID, job.Owner, LeaseJobExited, jobExitDetail(job.Cmd, exitCode, stderrTail))
	s.jobFinishedMetrics(jobResult(exitCode))
	s.decRunningJob(job.LeaseID)
	if l := s.lookupAny(job.LeaseID); l != nil {
		s.removeJobSecrets(l, job.JobID)
	}
	return nil
}

// jobFinishedMetrics bumps the running gauge down and the exited
// counter.
func (s *Service) jobFinishedMetrics(result string) {
	if s.metrics == nil {
		return
	}
	s.metrics.JobsRunning.Dec()
	s.metrics.JobsExitedTotal.WithLabelValues(result).Inc()
}

// markJobLost marks a running job lost (generation bump, cold restart)
// and emits the event. Idempotent: an already-finished record is left
// alone.
func (s *Service) markJobLost(ctx context.Context, job store.JobRow, detail string) {
	changed, err := s.db.MarkJobLost(ctx, job.JobID, time.Now().UTC())
	if err != nil {
		s.storeError("mark_job_lost", job.JobID, err)
		return
	}
	if !changed {
		return
	}
	s.emitLeaseEvent(job.LeaseID, job.Owner, LeaseJobLost, detail)
	if s.metrics != nil {
		s.metrics.JobsRunning.Dec()
		s.metrics.JobsExitedTotal.WithLabelValues("lost").Inc()
	}
	s.decRunningJob(job.LeaseID)
	if l := s.lookupAny(job.LeaseID); l != nil {
		s.removeJobSecrets(l, job.JobID)
	}
}

// markLeaseJobsLost marks every running job of a lease lost: the guest's
// memory did not continue (a generation bump). Called without the store
// lock; safe from any goroutine.
func (s *Service) markLeaseJobsLost(ctx context.Context, leaseID, owner, why string) {
	rows, err := s.db.ListRunningJobs(ctx)
	if err != nil {
		s.storeError("list_running_jobs", leaseID, err)
		return
	}
	for _, job := range rows {
		if job.LeaseID != leaseID {
			continue
		}
		s.markJobLost(ctx, job, why)
	}
}

// readJobRC reads a job's rc file from the guest. ok is false when the
// file does not exist yet (the command is still running).
func (s *Service) readJobRC(ctx context.Context, sandboxID, jobID string) (int, bool, error) {
	data, err := s.sub.ReadFile(ctx, sandboxID, jobPath(jobID, "rc"), 64)
	if err != nil {
		if errors.Is(err, substrate.ErrNotFound) {
			return 0, false, nil
		}
		return 0, false, err
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false, fmt.Errorf("parse rc: %w", err)
	}
	return code, true, nil
}

// readJobStderrTail reads the last n bytes of a job's stderr. A missing
// file is an empty tail, not an error.
func (s *Service) readJobStderrTail(ctx context.Context, sandboxID, jobID string, n int) (string, error) {
	data, err := s.sub.ReadFile(ctx, sandboxID, jobPath(jobID, "stderr"), int64(n))
	if err != nil {
		if errors.Is(err, substrate.ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}

// jobExitDetail renders the job_exited event detail: "exit <code>"
// plus up to 10 stderr lines, at most 1 KiB total (spec item 8).
func jobExitDetail(_ string, exitCode int, stderrTail string) string {
	detail := fmt.Sprintf("exit %d", exitCode)
	if lines := lastLines(stderrTail, jobEventStderrLines); lines != "" {
		detail += "\n" + lines
	}
	if len(detail) > jobEventDetailBytes {
		detail = detail[:jobEventDetailBytes]
	}
	return detail
}

// lastLines returns the last n non-empty lines of s.
func lastLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// cutDetail shortens s to n bytes, marking an ellipsis when it was
// longer. Used for event details.
func cutDetail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

// sortedKeys returns a map's keys in sorted order.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// runJobReconcileLoop reconciles running jobs every 10 s while any job
// runs (spec item 5). After a backend restart or a broken envd stream,
// the guest files are the source of truth.
func (s *Service) runJobReconcileLoop(ctx context.Context) {
	t := time.NewTicker(jobReconcileEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reconcileJobs(ctx)
		}
	}
}

// reconcileJobs reads each running job's rc through the guest files and
// marks finished ones exited. A job whose lease is gone or whose guest
// files vanished is left running until a generation change or the
// lease's own deletion loses it.
func (s *Service) reconcileJobs(ctx context.Context) {
	rows, err := s.db.ListRunningJobs(ctx)
	if err != nil {
		s.storeError("list_running_jobs", "", err)
		return
	}
	for _, job := range rows {
		l := s.lookupAny(job.LeaseID)
		if l == nil {
			continue // the lease is gone; its records went with it
		}
		if l.Generation > job.Generation || l.State == "lost" {
			s.markJobLost(ctx, job, "lease generation changed; the job did not survive")
			continue
		}
		if l.Suspended {
			// A planned pause continues the memory: leave the job running
			// and reconcile after resume, but keep the lease active so no
			// idle rule suspends it again mid-job.
			s.markActive(job.LeaseID)
			continue
		}
		code, done, err := s.readJobRC(ctx, l.SandboxID, job.JobID)
		if err != nil {
			if errors.Is(err, substrate.ErrNotFound) {
				// The sandbox is gone from the orchestrator: wait for the
				// crash reconcile, which bumps the generation or recovers.
				continue
			}
			s.log.Printf("jobs: reconcile %s: read rc: %v", job.JobID, err)
			continue
		}
		if !done {
			s.markActive(job.LeaseID)
			continue
		}
		stderr, _ := s.readJobStderrTail(ctx, l.SandboxID, job.JobID, jobStderrTailBytes)
		if err := s.finishJob(ctx, job, code, stderr); err != nil {
			s.log.Printf("jobs: reconcile %s: finish: %v", job.JobID, err)
		}
	}
}

// pruneJobs removes exited job records past the retention window. Called
// from the sweeper.
func (s *Service) pruneJobs(ctx context.Context) {
	cutoff := s.now().Add(-s.jobRetention())
	if _, err := s.db.PruneJobs(ctx, cutoff); err != nil {
		s.storeError("prune_jobs", "", err)
	}
}

// readJobOutput reads the last n bytes of a job's stdout or stderr.
func (s *Service) readJobOutput(ctx context.Context, sandboxID, jobID, stream string, n int) (string, error) {
	data, err := s.readJobRange(ctx, sandboxID, jobID, stream, -1, n)
	return string(data), err
}

// jobRangeReader is the optional substrate extension the output
// endpoints use to read a byte range without downloading everything
// before it. The e2b and fake substrates implement it.
type jobRangeReader interface {
	ReadFileRange(ctx context.Context, sandboxID, path string, offset, limit int64) ([]byte, error)
}

// readJobRange reads a byte range of a job's stdout or stderr. offset
// < 0 means the last limit bytes; otherwise it reads limit bytes from
// offset. A missing file is empty output, not an error.
func (s *Service) readJobRange(ctx context.Context, sandboxID, jobID, stream string, offset, limit int) ([]byte, error) {
	path := jobPath(jobID, stream)
	if offset < 0 {
		info, err := s.sub.Stat(ctx, sandboxID, path)
		if err != nil {
			if errors.Is(err, substrate.ErrNotFound) {
				return nil, nil
			}
			return nil, err
		}
		offset = int(info.Size) - limit
		if offset < 0 {
			offset = 0
		}
	}
	if rr, ok := s.sub.(jobRangeReader); ok {
		return rr.ReadFileRange(ctx, sandboxID, path, int64(offset), int64(limit))
	}
	// Fallback: read the prefix up to offset+limit and slice. Fine for
	// substrates without the optional range read.
	data, err := s.sub.ReadFile(ctx, sandboxID, path, int64(offset+limit))
	if err != nil {
		if errors.Is(err, substrate.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if offset >= len(data) {
		return nil, nil
	}
	end := offset + limit
	if end > len(data) {
		end = len(data)
	}
	return data[offset:end], nil
}

// leaseJobsView returns a lease's running job count and its most recent
// exited job, for the lease view's jobs field. Best effort: a store
// error yields zeroes.
func (s *Service) leaseJobsView(ctx context.Context, leaseID string) map[string]any {
	n, err := s.db.CountRunningJobs(ctx, leaseID)
	if err != nil {
		s.log.Printf("lease view: count jobs of %s: %v", leaseID, err)
	}
	view := map[string]any{"running": n, "last_exit": nil}
	if row, ok, err := s.db.LatestJobExitOfLease(ctx, leaseID); err == nil && ok {
		exit := 0
		if row.ExitCode != nil {
			exit = *row.ExitCode
		}
		view["last_exit"] = map[string]any{
			"job_id":    row.JobID,
			"exit_code": exit,
			"ended_at":  formatRFC3339(row.EndedAt),
		}
	}
	return view
}
