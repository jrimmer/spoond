package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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

// jobWrapperScript is the guest-side wrapper. It starts the command
// with setsid, so the command is its own process group and is detached
// from the envd Start stream's lifetime: a backend restart does not
// kill it. The wrapper itself waits for the command and its EXIT trap
// records the outcome atomically (write rc.tmp, then rename) and
// removes the job's staged secrets, so the files — not the envd stream
// — are the source of truth. A stream that dies only means the backend
// learns the outcome from the files on the next reconcile pass.
//
// SPOOND_JOBS_DIR and SPOOND_SECRETS_DIR default to the production
// paths; the test fake overrides them to a private directory. The pid
// file is written before the command is reaped, so a signal sent right
// after the 202 finds it.
//
// argv layout: /bin/bash -c <jobWrapperScript> spoond-job <job_id> <cmd argv...>
const jobWrapperScript = `job_id="$1"; shift
jobs="${SPOOND_JOBS_DIR:-/var/lib/spoond/jobs}"
secrets="${SPOOND_SECRETS_DIR:-/run/secrets}"
job_dir="$jobs/$job_id"
mkdir -p "$job_dir" || exit 1
setsid "$@" >"$job_dir/stdout" 2>"$job_dir/stderr" </dev/null &
child=$!
# Wait for the child's session (process group child == pid) to exist
# before recording the pid, so a signal sent right after the 202 always
# finds a group to signal.
for _ in 1 2 3 4 5 6 7 8 9 10; do
  kill -0 -"$child" 2>/dev/null && break
  sleep 0.05
done
printf '%d\n' "$child" >"$job_dir/pid"
finalize() {
  rc=$?
  if [ ! -f "$job_dir/rc" ]; then
    printf '%d\n' "$rc" >"$job_dir/rc.tmp" && mv "$job_dir/rc.tmp" "$job_dir/rc"
  fi
  if [ -f "$job_dir/secrets" ]; then
    while IFS= read -r name; do
      [ -n "$name" ] && rm -f "$secrets/$name"
    done <"$job_dir/secrets"
    rm -f "$job_dir/secrets"
  fi
  exit "$rc"
}
trap finalize EXIT
trap '' HUP
trap 'exit 130' INT
trap 'exit 131' QUIT
trap 'exit 143' TERM
wait "$child"
exit $?
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
// applies the same way exec's buildShellArgs applies it. Env values ride
// StartRequest.Env, so nothing is written to the guest's job directory
// and no value reaches argv.
func buildJobWrapperArgs(jobID, cmd, cwd string) []string {
	body := jobCommandBody(cmd, cwd)
	args := []string{"/bin/bash", "-c", jobWrapperScript, "spoond-job", jobID}
	return append(args, "/bin/bash", "-c", body)
}

// jobCommandBody renders the command as a bash -c body: change
// directory when cwd is set, then run cmd.
func jobCommandBody(cmd, cwd string) string {
	if cwd == "" {
		return cmd
	}
	return "cd " + shellQuote(cwd) + " && " + cmd
}

// jobStartLock is one lease's start lock with a waiter count, so the
// per-lease entry can be dropped once no start holds or waits on it.
type jobStartLock struct {
	mu   sync.Mutex
	refs int
}

// acquireJobStart returns the held start lock for a lease. Callers must
// release it with releaseJobStart.
func (s *Service) acquireJobStart(leaseID string) *jobStartLock {
	s.jobStartMu.Lock()
	l := s.jobStarts[leaseID]
	if l == nil {
		l = &jobStartLock{}
		s.jobStarts[leaseID] = l
	}
	l.refs++
	s.jobStartMu.Unlock()
	l.mu.Lock()
	return l
}

// releaseJobStart unlocks a lease's start lock and drops its entry when
// no other start holds or waits on it.
func (s *Service) releaseJobStart(leaseID string, l *jobStartLock) {
	l.mu.Unlock()
	s.jobStartMu.Lock()
	l.refs--
	if l.refs == 0 {
		delete(s.jobStarts, leaseID)
	}
	s.jobStartMu.Unlock()
}

// startJob starts a background exec. It returns the job id, start time
// and the envd Process carrying the stream (the caller watches it), or
// an error the caller maps onto an HTTP status.
func (s *Service) startJob(ctx context.Context, lease *Lease, owner, cmd, cwd string, env map[string]string, secrets map[string]string) (string, time.Time, substrate.Process, error) {
	// The job outlives the HTTP request that asked for it. Detach the
	// whole start from the request's cancellation so (a) the envd Start
	// stream — the stream the watcher holds to notice the exit at once —
	// is not canceled when the 202 response finalises the request or a
	// proxy/client disconnects mid-start, and (b) a disconnect cannot
	// leave a started process unrecorded. context.WithoutCancel keeps the
	// request's values; the caller is already resolved by now.
	ctx = context.WithoutCancel(ctx)
	// The per-lease cap is a check-then-insert against SQLite: hold the
	// lease's start lock for both so two concurrent starts for one lease
	// cannot both pass it, while starts on other leases run concurrently.
	startLock := s.acquireJobStart(lease.ID)
	defer s.releaseJobStart(lease.ID, startLock)
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

	// The command's env (and the pooled-lease id) rides StartRequest.Env,
	// which envd passes to the process: no value reaches argv, the
	// recorded command, the logs, an event, or a guest file (spec item
	// 3). The job directory itself gets only the secrets list, whose
	// names are file names anyway. Copy, so the request's map is never
	// mutated.
	startEnv := map[string]string{"SPOOND_JOBS_DIR": jobsDir}
	for k, v := range requestEnv(lease, env) {
		startEnv[k] = v
	}

	args := buildJobWrapperArgs(jobID, cmd, cwd)
	proc, err := s.sub.Start(ctx, lease.SandboxID, substrate.StartRequest{Args: args, Env: startEnv})
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
// sandboxID and generation are a snapshot taken while the lease was
// running the job: a cold restart must not make this path read the new
// sandbox's files or remove the job's secrets from it.
func (s *Service) watchJob(proc substrate.Process, job store.JobRow, sandboxID string, generation int64) {
	for ev := range proc.Events() {
		switch ev.Kind {
		case substrate.EventExit:
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			s.finishJobFromGuest(ctx, job, sandboxID, generation)
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
	// The channel closed without an exit or error event (for example the
	// backend's envd client saw its context canceled before it could
	// report the error). The detached command may still be running, so
	// leave the record for the reconcile pass — but say so, since this is
	// exactly the stream-broke case the at-once path cannot cover.
	s.log.Printf("jobs: %s: stream ended without an exit", job.JobID)
}

// finishJobFromGuest reads a job's rc and stderr tail from the guest
// files and finishes the record. It is a no-op when rc is not there yet.
func (s *Service) finishJobFromGuest(ctx context.Context, job store.JobRow, sandboxID string, generation int64) {
	code, done, err := s.readJobRC(ctx, sandboxID, job.JobID)
	if err != nil {
		s.log.Printf("jobs: %s: read rc: %v", job.JobID, err)
		return
	}
	if !done {
		return
	}
	stderr, _ := s.readJobStderrTail(ctx, sandboxID, job.JobID, jobStderrTailBytes)
	if err := s.finishJob(ctx, job, code, stderr, sandboxID, generation); err != nil {
		s.log.Printf("jobs: %s: finish: %v", job.JobID, err)
	}
}

// cleanupJobFiles removes one finished job's guest directory. It is
// best-effort and runs only once the job's record has been pruned: until
// then its stdout/stderr are still what the reads serve, so the files
// live exactly as long as the record (JOB_RETENTION_SECS).
func (s *Service) cleanupJobFiles(ctx context.Context, sandboxID, jobID string) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := s.sub.Remove(dctx, sandboxID, jobsDir+"/"+jobID, true); err != nil && !errors.Is(err, substrate.ErrNotFound) {
		s.log.Printf("jobs: %s: cleanup files: %v", jobID, err)
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
// Best effort: the guest wrapper normally did this already. It touches
// only a sandbox still on the job's continuity generation: after a cold
// restart the job's secrets are gone with the old guest, and removing
// them from a different sandbox would be wrong.
func (s *Service) removeJobSecrets(leaseID, jobID, sandboxID string, generation int64) {
	names := s.takeJobSecrets(jobID)
	if len(names) == 0 {
		return
	}
	if !s.leaseOnGeneration(leaseID, sandboxID, generation) {
		return
	}
	s.removeSecrets(sandboxID, names)
	if create := s.createSecretsFor(leaseID); len(create) > 0 {
		var shadowed []string
		for _, name := range names {
			if _, ok := create[name]; ok {
				shadowed = append(shadowed, name)
			}
		}
		if len(shadowed) > 0 {
			s.restageSecrets(sandboxID, shadowed, create)
		}
	}
}

// leaseOnGeneration reports whether a live lease's current sandbox is
// still the one a job ran in (same id, same continuity generation).
func (s *Service) leaseOnGeneration(leaseID, sandboxID string, generation int64) bool {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l := s.store.leases[leaseID]
	return l != nil && !l.released && l.SandboxID == sandboxID && l.Generation == generation
}

// leaseState returns a live lease's lifecycle state ("" when gone).
func (s *Service) leaseState(leaseID string) string {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if l := s.store.leases[leaseID]; l != nil && !l.released {
		return l.State
	}
	return ""
}

// leaseSuspended reports whether a live lease is suspended.
func (s *Service) leaseSuspended(leaseID string) bool {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if l := s.store.leases[leaseID]; l != nil && !l.released {
		return l.Suspended
	}
	return false
}

// leaseContinuity snapshots a lease's sandbox id and generation under
// the store lock, so a job path can read guest files and clean up
// without chasing a lease across a cold restart.
func (s *Service) leaseContinuity(leaseID string) (string, int64, bool) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	l := s.store.leases[leaseID]
	if l == nil || l.released {
		return "", 0, false
	}
	return l.SandboxID, l.Generation, true
}

// finishJob marks a running job exited from the guest's rc file and
// emits the event. It is idempotent: an already-finished record is left
// alone, so the watcher and the reconcile pass cannot both count it.
func (s *Service) finishJob(ctx context.Context, job store.JobRow, exitCode int, stderrTail, sandboxID string, generation int64) error {
	changed, err := s.db.UpdateJobExit(ctx, job.JobID, exitCode, time.Now().UTC(), stderrTail)
	if err != nil {
		return err
	}
	if !changed {
		return nil // another path already finished it
	}
	s.emitLeaseEvent(job.LeaseID, job.Owner, LeaseJobExited, jobExitDetail(exitCode, stderrTail))
	s.jobFinishedMetrics(jobResult(exitCode))
	s.decRunningJob(job.LeaseID)
	s.removeJobSecrets(job.LeaseID, job.JobID, sandboxID, generation)
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
// alone. sandboxID/generation name the sandbox the job ran in.
func (s *Service) markJobLost(ctx context.Context, job store.JobRow, sandboxID string, generation int64, detail string) {
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
	s.removeJobSecrets(job.LeaseID, job.JobID, sandboxID, generation)
}

// markLeaseJobsLost marks every running job of a lease lost: the guest's
// memory did not continue (a generation bump). Called without the store
// lock; safe from any goroutine. It runs after the lease was moved to its
// new sandbox/generation, so removeJobSecrets's continuity guard never
// matches: the lost job's staged files went with the old guest, and the
// new guest never had them. The in-memory names are still dropped, which
// is what markJobLost's cleanup does in that case.
func (s *Service) markLeaseJobsLost(ctx context.Context, leaseID, owner, why string) {
	sandboxID, _, _ := s.leaseContinuity(leaseID)
	rows, err := s.db.ListRunningJobs(ctx)
	if err != nil {
		s.storeError("list_running_jobs", leaseID, err)
		return
	}
	for _, job := range rows {
		if job.LeaseID != leaseID {
			continue
		}
		s.markJobLost(ctx, job, sandboxID, job.Generation, why)
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

// toValidUTF8 replaces invalid UTF-8 from the guest with U+FFFD, so
// non-UTF-8 bytes never reach a record or an event.
func toValidUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "\uFFFD")
}

// readJobStderrTail reads the last n bytes of a job's stderr. A missing
// file is an empty tail, not an error. The read goes through the range
// reader (a true tail), not the prefix ReadFile: an over-limit stderr
// must still yield its last bytes, not a substrate.ErrTooLarge. The
// bytes are made valid UTF-8 (a tail read can start mid-rune and the
// guest may write arbitrary bytes), so the stored tail and the emitted
// detail are always valid text.
func (s *Service) readJobStderrTail(ctx context.Context, sandboxID, jobID string, n int) (string, error) {
	data, err := s.readJobRange(ctx, sandboxID, jobID, "stderr", -1, n)
	return toValidUTF8(string(data)), err
}

// jobExitDetail renders the job_exited event detail: "exit <code>"
// plus up to 10 stderr lines, at most 1 KiB total (spec item 8). The
// cut is rune-safe, so it never splits a multi-byte rune.
func jobExitDetail(exitCode int, stderrTail string) string {
	detail := fmt.Sprintf("exit %d", exitCode)
	if lines := lastLines(toValidUTF8(stderrTail), jobEventStderrLines); lines != "" {
		detail += "\n" + lines
	}
	if len(detail) > jobEventDetailBytes {
		detail = cutToRunes(detail, jobEventDetailBytes)
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

// cutToRunes truncates s to at most n bytes, backing up to a rune
// boundary so a multi-byte rune is never split.
func cutToRunes(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// cutDetail shortens s to n bytes, marking an ellipsis when it was
// longer. The cut lands on a rune boundary, so it never splits a
// multi-byte rune. Used for event details.
func cutDetail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return cutToRunes(s, n)
	}
	return cutToRunes(s, n-3) + "..."
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
		sandboxID, generation, ok := s.leaseContinuity(job.LeaseID)
		if !ok {
			continue // the lease is gone; its records went with it
		}
		if generation > job.Generation || s.leaseState(job.LeaseID) == "lost" {
			s.markJobLost(ctx, job, sandboxID, generation, "lease generation changed; the job did not survive")
			continue
		}
		if s.leaseSuspended(job.LeaseID) {
			// A planned pause continues the memory: leave the job running
			// and reconcile after resume, but keep the lease active so no
			// idle rule suspends it again mid-job.
			s.markActive(job.LeaseID)
			continue
		}
		code, done, err := s.readJobRC(ctx, sandboxID, job.JobID)
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
		stderr, _ := s.readJobStderrTail(ctx, sandboxID, job.JobID, jobStderrTailBytes)
		if err := s.finishJob(ctx, job, code, stderr, sandboxID, generation); err != nil {
			s.log.Printf("jobs: reconcile %s: finish: %v", job.JobID, err)
		}
	}
}

// pruneJobs removes exited job records past the retention window and
// then their guest files, so /var/lib/spoond/jobs does not grow without
// bound. Called from the sweeper.
func (s *Service) pruneJobs(ctx context.Context) {
	cutoff := s.now().Add(-s.jobRetention())
	expired, err := s.db.ListExpiredJobs(ctx, cutoff)
	if err != nil {
		s.storeError("list_expired_jobs", "", err)
		return
	}
	if _, err := s.db.PruneJobs(ctx, cutoff); err != nil {
		s.storeError("prune_jobs", "", err)
		return
	}
	for _, job := range expired {
		if sandboxID, _, ok := s.leaseContinuity(job.LeaseID); ok {
			s.cleanupJobFiles(ctx, sandboxID, job.JobID)
		}
	}
}

// readJobOutput reads the last n bytes of a job's stdout or stderr. The
// bytes are made valid UTF-8 before they are emitted in the record view.
func (s *Service) readJobOutput(ctx context.Context, sandboxID, jobID, stream string, n int) (string, error) {
	data, err := s.readJobRange(ctx, sandboxID, jobID, stream, -1, n)
	return toValidUTF8(string(data)), err
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
	// substrates without the optional range read. A file larger than the
	// requested prefix cannot be read whole, so a tail request degrades to
	// empty rather than an error the caller would surface as unavailable.
	data, err := s.sub.ReadFile(ctx, sandboxID, path, int64(offset+limit))
	if err != nil {
		if errors.Is(err, substrate.ErrNotFound) || errors.Is(err, substrate.ErrTooLarge) {
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

// settleJobsOfReleasedLease ends the bookkeeping of every job still
// running on a lease being released: the running gauge goes down, the
// job's remembered secret names are dropped (the guest and its files are
// gone) and a job_lost event says why. The rows themselves cascade with
// the lease row.
func (s *Service) settleJobsOfReleasedLease(ctx context.Context, l *Lease) {
	jobs, err := s.db.ListJobs(ctx, l.ID)
	if err != nil {
		s.log.Printf("release: list jobs of %s: %v", l.ID, err)
		return
	}
	for _, j := range jobs {
		if j.State != "running" {
			continue
		}
		s.takeJobSecrets(j.JobID)
		if s.metrics != nil {
			s.metrics.JobsRunning.Dec()
			s.metrics.JobsExitedTotal.WithLabelValues("lost").Inc()
		}
		s.emitLeaseEvent(l.ID, l.Owner, LeaseJobLost, "job "+j.JobID+": lease released")
	}
}
