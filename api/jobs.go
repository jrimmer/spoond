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

	"github.com/jrimmer/spoond/v3/store"
	"github.com/jrimmer/spoond/v3/substrate"
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
	// jobPunchBlockBytes is the guest filesystem's block size (spoond-
	// hwto): the punch target is rounded down to one, so the hole itself
	// is block-aligned and a following extend never turns it back into
	// data. 4 KiB, the common filesystem block size and the least the
	// call reliably honours.
	jobPunchBlockBytes = 4096
)

// DefaultMaxRunningJobsPerLease and DefaultJobRetentionSecs are the
// MAX_RUNNING_JOBS_PER_LEASE and JOB_RETENTION_SECS defaults.
const (
	DefaultMaxRunningJobsPerLease = 16
	DefaultJobRetentionSecs       = 604800 // 7 days
)

// DefaultJobOutputMaxBytes is the JOB_OUTPUT_MAX_BYTES default
// (spoond-hwto): a job's streams are capped at nothing — the whole
// output is kept, exactly as before the cap existed — and only a
// request's own output_max_bytes (or an explicit host config) drops
// anything.
const DefaultJobOutputMaxBytes = 0

// DefaultJobMaxRuntimeSecs is the JOB_MAX_RUNTIME default: a background
// job may run for 24 h before it is killed (spoond-wb5).
const DefaultJobMaxRuntimeSecs = 24 * 60 * 60 // 24 hours

// jobTimedOutReason is stored in lease_jobs.reason and named in the
// job_exited event and the API record when the max runtime, not the
// command, ended a job.
const jobTimedOutReason = store.JobReasonTimedOut

// errJobCap is returned when a lease is at its running-job cap.
var errJobCap = errors.New("too many running background jobs for this lease")

// errLeaseBusySave is returned when a named-snapshot save holds a
// lease's secrets gate, so a background job that wants to stage secrets
// must wait and answer 409 lease_busy (2.7, #83 B2).
var errLeaseBusySave = errors.New("a named snapshot save is in progress; retry")

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
	// Reason is "timed_out" when the max runtime, not the command, ended
	// the job; omitted for a normal exit (spoond-wb5).
	Reason string `json:"reason,omitempty"`
	// MaxRuntimeSecs is the effective cap the job ran under, in seconds,
	// 0 when uncapped.
	MaxRuntimeSecs int64 `json:"max_runtime_secs,omitempty"`
	// RetentionSecs is the effective retention window, in seconds, the
	// job's record and guest files are kept under (spoond-hwto): the
	// host's JOB_RETENTION_SECS shortened by the job's own request.
	RetentionSecs int64 `json:"retention_secs,omitempty"`
	// Skipped names, per stream the output cap has dropped anything
	// from (spoond-hwto), how many bytes — and how many newlines those
	// bytes held — the record view's stdout/stderr read leaves out. A
	// stream the cap has not touched is not named, so the view's shape
	// is unchanged until something is dropped.
	Skipped map[string]jobStreamSkipped `json:"skipped,omitempty"`
}

// jobStreamSkipped is one stream's dropped-front view (spoond-hwto): the
// bytes the output cap dropped from the stream's file (and the newlines
// those bytes held), so an absolute offset the record's caller reads
// can name where the answer starts.
type jobStreamSkipped struct {
	Bytes    int64 `json:"bytes"`
	Newlines int64 `json:"newlines"`
}

// jobInfoOf renders a store row for the API.
func jobInfoOf(r store.JobRow) jobInfo {
	return jobInfo{
		JobID:          r.JobID,
		LeaseID:        r.LeaseID,
		Owner:          r.Owner,
		Cmd:            r.Cmd,
		Cwd:            r.Cwd,
		State:          r.State,
		ExitCode:       r.ExitCode,
		StartedAt:      r.StartedAt.UTC().Format(time.RFC3339Nano),
		EndedAt:        formatRFC3339(r.EndedAt),
		StderrTail:     r.StderrTail,
		Reason:         r.Reason,
		MaxRuntimeSecs: r.MaxRuntimeSecs,
		RetentionSecs:  r.RetentionSecs,
		Skipped:        jobSkippedView(r),
	}
}

// jobSkippedView renders the record's dropped-front counters as the
// API's skipped view (spoond-hwto): nil until any stream has dropped
// anything, so the record view's shape is byte-identical to the
// pre-cap one while nothing is dropped.
func jobSkippedView(r store.JobRow) map[string]jobStreamSkipped {
	var out map[string]jobStreamSkipped
	if r.StdoutDroppedBytes > 0 {
		out = map[string]jobStreamSkipped{"stdout": {r.StdoutDroppedBytes, r.StdoutDroppedNewlines}}
	}
	if r.StderrDroppedBytes > 0 {
		if out == nil {
			out = map[string]jobStreamSkipped{}
		}
		out["stderr"] = jobStreamSkipped{r.StderrDroppedBytes, r.StderrDroppedNewlines}
	}
	return out
}

// jobPath returns the guest path of one job file.
func jobPath(jobID, name string) string {
	return jobsDir + "/" + jobID + "/" + name
}

// jobCapPath returns the guest path of one stream's cap counters sidecar
// (spoond-hwto). It lives in the same job directory as the stream, so
// the job's own cleanup (and its retention prune) removes it with the
// stream.
func jobCapPath(jobID, stream string) string {
	return jobPath(jobID, stream+".cap")
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

// effectiveJobRetention returns the retention a new job's record and
// guest files are kept under (spoond-hwto): the host's JOB_RETENTION_SECS,
// shortened by the job's own retention_secs when the request asked for
// less. A request never extends the host window, so an oversized or unset
// request keeps the host value exactly as effectiveJobRuntime does for
// the max runtime.
func (s *Service) effectiveJobRetention(requested int64) int64 {
	host := s.jobRetention()
	hostSecs := int64(host / time.Second)
	req := requested
	if req > hostSecs || req <= 0 {
		return hostSecs
	}
	return req
}

// maxJobCapFile bounds how much of a cap sidecar file (spoond-hwto) any
// read or write takes. A pass that appends its line is bounded (one
// line per pass, one pass per poll); the read's bound keeps a runaway
// file from being pulled whole. At the bound the sidecar is left to the
// next pass and the record's own counters (the row is authoritative
// for reads anyway).
const maxJobCapFile = 1 << 20

// jobOutputMax returns the host's configured per-stream default output
// cap (spoond-hwto). 0 = uncapped; a negative value is treated the same
// way, so a deployment cannot disable the guard by typo.
func (s *Service) jobOutputMax() int64 {
	if s.cfg.JobOutputMaxBytes > 0 {
		return s.cfg.JobOutputMaxBytes
	}
	return DefaultJobOutputMaxBytes
}

// effectiveJobOutputMax returns the per-stream output cap a new job's
// streams run under (spoond-hwto): the host's JOB_OUTPUT_MAX_BYTES
// (0 = unlimited), shortened by the job's own output_max_bytes when
// the request asked for one. A request never lengthens the host cap,
// the way effectiveJobRetention never extends the host window.
func (s *Service) effectiveJobOutputMax(requested int64) int64 {
	host := s.jobOutputMax()
	if host > 0 && (requested <= 0 || requested > host) {
		return host
	}
	return requested
}

// jobPunchHole holds the optional substrate extension the output cap
// runs on (spoond-hwto): punching holes is how the poller drops the
// front of an over-cap stream file while the command keeps appending.
// The e2b substrate implements it with `fallocate --punch-hole` in the
// guest; the fake emulates the same semantics in memory.
type jobPunchHole interface {
	PunchHole(ctx context.Context, sandboxID, path string, offset, length int64) error
}

// jobCapTarget is the drop plan's core (spoond-hwto): the absolute
// offset the poller drops a stream file of `size` bytes to, when its
// record has already dropped `dropped` and its kept window is `kept`
// (the cap plus the block the target rounds into): the floor block
// boundary at or below the byte that keeps exactly `kept` bytes, so the
// kept region always holds at least the cap and the hole itself is
// block-aligned. 0 means nothing to drop: the file is at or under the
// window, or the target has not moved past what is already dropped.
// Pure, so the cap tests pin the block math without a sandbox.
func jobCapTarget(size, dropped, kept int64) int64 {
	if kept <= 0 || size <= kept {
		return 0
	}
	target := (size - kept) / jobPunchBlockBytes * jobPunchBlockBytes
	if target <= dropped {
		return 0
	}
	return target
}

// jobCapCountNewlines counts the newlines of a planned drop's bytes
// (spoond-hwto), so the record can say how many lines a read of the
// stream's front skips over.
func jobCapCountNewlines(data []byte) int64 {
	n := int64(0)
	for _, b := range data {
		if b == '\n' {
			n++
		}
	}
	return n
}

// jobRowDropped is the row's recorded dropped front of one stream.
func jobRowDropped(job store.JobRow, stream string) int64 {
	if stream == "stdout" {
		return job.StdoutDroppedBytes
	}
	return job.StderrDroppedBytes
}

// jobStreamNewlines is the recorded newline count of one stream's
// dropped front (0 for a stream that dropped nothing or a legacy row).
func jobStreamNewlines(job store.JobRow, stream string) int64 {
	if stream == "stdout" {
		return job.StdoutDroppedNewlines
	}
	return job.StderrDroppedNewlines
}

// jobCapLine renders a cap sidecar line, newline included.
func jobCapLine(dropped, newlines, kept int64) string {
	return fmt.Sprintf("%d %d %d\n", dropped, newlines, kept)
}

// jobCapSidecarOf loads the live counters of a stream's cap sidecar.
// ok is false when there is no live line (the file is missing, garbled
// or unwritten), and the caller falls back to the record's own.
func (s *Service) jobCapSidecarOf(ctx context.Context, sandboxID, jobID, stream string) (int64, int64, bool) {
	data, err := s.sub.ReadFile(ctx, sandboxID, jobCapPath(jobID, stream), maxJobCapFile)
	if err != nil || len(data) == 0 {
		return 0, 0, false
	}
	if n := len(data); data[n-1] != '\n' {
		return 0, 0, false
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	last := lines[len(lines)-1]
	dropped, newlines, ok := jobCapSidecarOf2(last)
	if !ok {
		return 0, 0, false
	}
	return dropped, newlines, true
}

// jobCapSidecarOf2 is the pure body of jobCapSidecarOf: one sidecar
// line parsed. ok is false for anything that is not a whole, sane
// "<dropped> <newlines> <kept>" line; the caller then falls back to the
// record's own counters.
func jobCapSidecarOf2(line string) (dropped, newlines int64, ok bool) {
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return 0, 0, false
	}
	a, err1 := strconv.ParseInt(fields[0], 10, 64)
	b, err2 := strconv.ParseInt(fields[1], 10, 64)
	_, err3 := strconv.ParseInt(fields[2], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || a < 0 || b < 0 {
		return 0, 0, false
	}
	// kept is recorded as it was applied (kept = size - dropped at the
	// pass); it is not validated beyond its parse: the record's counters
	// are what reads trust.
	return a, b, true
}

// capJobStream caps one guest stream file (spoond-hwto), the poller's
// working step. dropped/newlines are what the record holds for the
// stream now (the guest sidecar may be ahead); kept is the cap window
// (the cap plus the block its target rounds into). The pass: plan the
// block-aligned kept region from the file's size, count the planned
// region's newlines from a range read of exactly that region, punch the
// region out, then — and only then — make the drop visible by appending
// the sidecar line and raising the record's counters. The punch runs
// first, so a failure leaves no counter describing a drop that did not
// happen; the next pass re-punches a crash-lost write. When the
// substrate cannot punch holes the stream is left whole: truncating it
// by any other means would shift the offsets every absolute-offset
// reader holds, so the cap says so in the log and drops nothing.
func (s *Service) capJobStream(ctx context.Context, sandboxID, jobID, stream string, dropped, newlines, kept int64) {
	path := jobPath(jobID, stream)
	info, err := s.sub.Stat(ctx, sandboxID, path)
	if err != nil {
		return // no stream file (yet); a later pass re-plans
	}
	if info.Size < dropped {
		// The file was rolled back under the record (a snapshot
		// restore): nothing is left to drop and the front is already
		// gone; reads clamp to the record's own counters.
		return
	}
	target := jobCapTarget(info.Size, dropped, kept)
	if target <= dropped {
		return
	}
	ph, ok := s.sub.(jobPunchHole)
	if !ok {
		s.log.Printf("jobs: %s: %s output cap: the guest does not support fallocate --punch-hole; the stream is left untruncated (its bytes are never served twice and nothing is reported dropped)", jobID, stream)
		return
	}
	// Count the newlines the punched bytes hold, from a range read of
	// exactly the region the pass is about to punch: the record gets
	// the front's line count from the same bytes it removes.
	data, rerr := s.readStreamRange(ctx, sandboxID, path, dropped, target-dropped)
	if rerr != nil {
		s.log.Printf("jobs: %s: %s output cap: count %d..%d: %v", jobID, stream, dropped, target, rerr)
		return
	}
	planned := jobCapCountNewlines(data)
	if perr := ph.PunchHole(ctx, sandboxID, path, dropped, target-dropped); perr != nil {
		s.log.Printf("jobs: %s: %s output cap: punch %d..%d: %v", jobID, stream, dropped, target, perr)
		return
	}
	// The sidecar's last live line must be whole before it is appended
	// to; a torn tail line (a crashed old-style write) leaves the
	// sidecar unusable and the record's counters authoritative.
	pre, rerr2 := s.sub.ReadFile(ctx, sandboxID, jobCapPath(jobID, stream), maxJobCapFile)
	if rerr2 != nil && !errors.Is(rerr2, substrate.ErrNotFound) {
		s.log.Printf("jobs: %s: %s output cap: read counters: %v", jobID, stream, rerr2)
		return
	}
	if n := len(pre); n > 0 && pre[n-1] != '\n' {
		s.log.Printf("jobs: %s: %s output cap: counters end mid-line; the pass is skipped", jobID, stream)
		return
	}
	next := string(pre) + jobCapLine(target, newlines+planned, target)
	if werr := s.sub.WriteFile(ctx, sandboxID, jobCapPath(jobID, stream), []byte(next), 0o600); werr != nil {
		// The drop is already punched, so the record is the only view
		// left: a failure here says so and the counters catch up there.
		s.log.Printf("jobs: %s: %s output cap: write counters: %v", jobID, stream, werr)
		return
	}
	if uerr := s.dbJobs.UpdateJobSkipped(ctx, jobID, stream, target, newlines+planned); uerr != nil {
		s.log.Printf("jobs: %s: %s output cap: record skipped: %v", jobID, stream, uerr)
	}
}

// capJobStreams caps both streams of one job (spoond-hwto), the job
// poller's working step.
func (s *Service) capJobStreams(ctx context.Context, job store.JobRow, sandboxID string) {
	kept := job.OutputMaxBytes + jobPunchBlockBytes
	if kept <= jobPunchBlockBytes {
		kept = s.jobOutputMax() + jobPunchBlockBytes
	}
	if kept <= jobPunchBlockBytes {
		return
	}
	for _, stream := range []string{"stdout", "stderr"} {
		dropped, newlines := jobRowDropped(job, stream), jobStreamNewlines(job, stream)
		if d, _, ok := s.jobCapSidecarOf(ctx, sandboxID, job.JobID, stream); ok && d > dropped {
			// The guest file's counters are ahead of the record's (a
			// store write failed after a pass): continue from there, and
			// the record rises as the pass lands.
			dropped = d
		}
		s.capJobStream(ctx, sandboxID, job.JobID, stream, dropped, newlines, kept)
	}
}

// readStreamRange reads a byte range of a guest file through the
// optional range reader, falling back to a prefix read when the
// substrate lacks it (a range of a job stream is small: the read is
// only ever the planned drop's body). A missing file is an empty one.
func (s *Service) readStreamRange(ctx context.Context, sandboxID, path string, offset, limit int64) ([]byte, error) {
	if rr, ok := s.sub.(jobRangeReader); ok {
		return rr.ReadFileRange(ctx, sandboxID, path, offset, limit)
	}
	data, err := s.sub.ReadFile(ctx, sandboxID, path, offset+limit)
	if err != nil {
		if errors.Is(err, substrate.ErrNotFound) || errors.Is(err, substrate.ErrTooLarge) {
			return nil, nil
		}
		return nil, err
	}
	if offset >= int64(len(data)) {
		return nil, nil
	}
	end := offset + limit
	if end > int64(len(data)) {
		end = int64(len(data))
	}
	return data[offset:end], nil
}

// maxJobRuntimeDuration is the largest representable time.Duration; a
// request at or below it cannot overflow the seconds conversion.
const maxJobRuntimeDuration = time.Duration(1<<63 - 1)

// secondsDuration converts a whole number of seconds to a duration,
// clamping at the maximum representable duration. A value that does not
// fit would otherwise overflow the multiplication and wrap negative,
// which reads as "uncapped" and silently defeats the cap (spoond-wb5).
// A non-positive value yields 0.
func secondsDuration(secs int64) time.Duration {
	if secs <= 0 {
		return 0
	}
	if secs > int64(maxJobRuntimeDuration/time.Second) {
		return maxJobRuntimeDuration
	}
	return time.Duration(secs) * time.Second
}

// jobMaxRuntime returns the host's effective JOB_MAX_RUNTIME. 0 uses the
// 24 h default; a negative value disables the cap. The startup parser
// holds the cap as whole seconds, rounding a positive fractional Go
// duration up (so 500ms is 1 s, not a 0 that reads as the default, and
// 1500ms is 2 s, not a truncated 1 s), so the returned duration is always
// whole seconds. An oversized value is clamped instead of being allowed
// to wrap negative.
func (s *Service) jobMaxRuntime() time.Duration {
	secs := s.cfg.JobMaxRuntimeSecs
	if secs == 0 {
		secs = DefaultJobMaxRuntimeSecs
	}
	return secondsDuration(secs)
}

// effectiveJobRuntime returns the max runtime a new job may run: the
// host's JOB_MAX_RUNTIME (0 = uncapped), shortened by the job's own
// max_runtime_secs when it asked for one. A requested value never
// extends the host cap, and a request too large to represent is clamped
// rather than allowed to wrap into "uncapped".
func (s *Service) effectiveJobRuntime(requested int64) time.Duration {
	max := s.jobMaxRuntime()
	if requested <= 0 {
		return max
	}
	req := secondsDuration(requested)
	if max > 0 && max < req {
		return max
	}
	return req
}

// hasSpentRuntime reports whether a running job has been running for at
// least its effective max runtime, given the host's cap. A record with
// max_runtime_secs 0 (a job written before the column existed, or an
// uncapped start) is capped by hostMax, so an upgrade immediately
// applies JOB_MAX_RUNTIME to jobs already running. A hostMax <= 0 means
// the host cap is disabled and the job is uncapped. It is a pure
// function of the row, the host cap and the clock, so the reconcile pass
// can call it without a store read.
func hasSpentRuntime(job store.JobRow, now time.Time, hostMax time.Duration) bool {
	d := hostMax
	if job.MaxRuntimeSecs > 0 {
		d = secondsDuration(job.MaxRuntimeSecs)
	}
	if d <= 0 {
		return false
	}
	return now.Sub(job.StartedAt) >= d
}

// buildJobWrapperArgs builds the argv handed to substrate.Start: the
// wrapper script, the job id, and the command as a shell invocation. cwd
// applies the same way exec applies it. Env values ride
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
func (s *Service) startJob(ctx context.Context, lease *Lease, owner, cmd, cwd string, env map[string]string, secrets map[string]string, requestedMaxRuntimeSecs, requestedRetentionSecs, requestedOutputMaxBytes int64) (string, time.Time, substrate.Process, error) {
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
	maxRuntime := s.effectiveJobRuntime(requestedMaxRuntimeSecs)
	var maxRuntimeSecs int64
	if maxRuntime > 0 {
		// Round up, so a sub-second cap never stores 0 (which reads as
		// uncapped) and never grants a little more than asked. Divide
		// before adding one so a duration near the maximum does not
		// overflow, then clamp back under the largest representable
		// whole-second value so reading it back cannot overflow.
		maxRuntimeSecs = int64(maxRuntime / time.Second)
		if maxRuntime%time.Second != 0 {
			maxRuntimeSecs++
		}
		if lim := int64(maxJobRuntimeDuration / time.Second); maxRuntimeSecs > lim {
			maxRuntimeSecs = lim
		}
	}
	// The record's own retention window (spoond-hwto): the effective
	// host-or-shorter value, so the prune reads the row without knowing
	// the host config. retention_secs of 0 can only mean a legacy row:
	// a job started by this backend always stamps a positive value.
	retentionSecs := s.effectiveJobRetention(requestedRetentionSecs)
	// The streams' own output cap (spoond-hwto): the host default
	// (JOB_OUTPUT_MAX_BYTES, 0 = unlimited) shortened by the request's
	// output_max_bytes.
	outputMax := s.effectiveJobOutputMax(requestedOutputMaxBytes)

	// Stage the job's secrets before the wrapper runs. The guest wrapper
	// removes them at exit; the backend removes them on reconcile if the
	// wrapper never got that far. Only the names (never values) are
	// recorded, in the job directory and in memory.
	secretNames := sortedSecretNames(secrets)
	if len(secrets) > 0 {
		// The per-lease secrets gate (2.7, #83 B2): take it before
		// staging, so a named-snapshot save that holds it makes this job
		// 409 lease_busy instead of letting it write a secret into the
		// checkpoint. Hold the gate across the staging. The names are
		// marked staged before the files are written, so a save that
		// somehow overlapped scrubs them.
		if !s.secretsGate.beginStaging(lease.ID) {
			return "", time.Time{}, nil, errLeaseBusySave
		}
		defer s.secretsGate.endStaging(lease.ID)
		// The lease has had a secret from here on (#141), recorded
		// inside the gate before the files exist.
		s.markHadSecrets(lease)
		s.recordJobSecrets(lease.ID, jobID, secretNames)
		if err := s.stageSecrets(ctx, lease.SandboxID, secrets); err != nil {
			s.forgetJobSecrets(jobID)
			return "", time.Time{}, nil, fmt.Errorf("stage secrets: %w", err)
		}
		// A names-only list the wrapper reads to clean up. Values are
		// never written here.
		if err := s.sub.WriteFile(ctx, lease.SandboxID, jobPath(jobID, "secrets"),
			[]byte(strings.Join(secretNames, "\n")+"\n"), 0o600); err != nil {
			s.removeSecrets(lease.SandboxID, secretNames)
			s.forgetJobSecrets(jobID)
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
			s.forgetJobSecrets(jobID)
		}
		return "", time.Time{}, nil, fmt.Errorf("start: %w", err)
	}

	if err := s.db.InsertJob(ctx, store.JobRow{
		JobID:          jobID,
		LeaseID:        lease.ID,
		Owner:          owner,
		Cmd:            cmd,
		Cwd:            cwd,
		State:          "running",
		StartedAt:      startedAt,
		Generation:     lease.Generation,
		MaxRuntimeSecs: maxRuntimeSecs,
		RetentionSecs:  retentionSecs,
		OutputMaxBytes: outputMax,
	}); err != nil {
		// The process is running but unrecorded. Do not leave it: a
		// best-effort TERM and the (empty) record-free result.
		_ = proc.Signal(false)
		_ = proc.Close()
		if len(secretNames) > 0 {
			s.removeSecrets(lease.SandboxID, secretNames)
			s.forgetJobSecrets(jobID)
		}
		return "", time.Time{}, nil, err
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
	stderr, _ := s.readJobStderrTail(ctx, sandboxID, job.JobID, jobStderrTailBytes, job)
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

// recordJobSecrets remembers the secret names staged for a running job,
// keyed by job id. takeJobSecrets clears them at exit.
func (s *Service) recordJobSecrets(leaseID, jobID string, names []string) {
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

// forgetJobSecrets drops a job's remembered secret names when it never
// started (a staging or Start failure).
func (s *Service) forgetJobSecrets(jobID string) {
	s.takeJobSecrets(jobID)
}

// removeJobSecrets deletes a job's staged secret files and, for names
// that shadow a create-time secret, restores the lease's value (#80).
// Best effort: the guest wrapper normally did this already. It touches
// only a sandbox still on the job's continuity generation: after a cold
// restart the job's secrets are gone with the old guest, and removing
// them from a different sandbox would be wrong.
//
// The removal and the shadowed re-stage run together under the per-lease
// secrets gate (Q1). While a save holds the gate, neither may run: a
// re-stage would land between the save's scrub and its checkpoint, and a
// removal would leave the source without the create-time value the
// save's own re-stage is about to restore. So the names are recorded and
// the save removes them (drainDeferredSecretRemovals) before it
// re-stages every create-time secret. When the gate is free this path
// removes and re-stages as before.
func (s *Service) removeJobSecrets(leaseID, jobID, sandboxID string, generation int64) {
	names := s.takeJobSecrets(jobID)
	if len(names) == 0 {
		return
	}
	if !s.leaseOnGeneration(leaseID, sandboxID, generation) {
		return
	}
	create := s.createSecretsFor(leaseID)
	shadowed := shadowedNames(create, names)
	if !s.secretsGate.beginStaging(leaseID) {
		s.deferSecretRemoval(leaseID, names)
		return
	}
	defer s.secretsGate.endStaging(leaseID)
	s.removeSecrets(sandboxID, names)
	s.restageSecrets(sandboxID, shadowed, create)
}

// shadowedNames returns the names that also exist as create-time
// secrets, i.e. the ones whose removal must be followed by a re-stage.
func shadowedNames(create map[string]string, names []string) []string {
	if len(create) == 0 {
		return nil
	}
	var shadowed []string
	for _, name := range names {
		if _, ok := create[name]; ok {
			shadowed = append(shadowed, name)
		}
	}
	return shadowed
}

// cleanupExecSecrets removes a finished synchronous exec's secret files
// and restores any that shadowed a create-time secret. It is the
// deferred cleanup of exec: it must run while the exec still holds the
// per-lease secrets gate, so a save that is scrubbing cannot see a
// re-staged shadowed file (R2).
func (s *Service) cleanupExecSecrets(l *Lease, execSecrets map[string]string) {
	if len(execSecrets) == 0 {
		return
	}
	s.removeSecrets(l.SandboxID, sortedSecretNames(execSecrets))
	s.restageShadowedSecrets(l.ID, l.SandboxID, sortedSecretNames(execSecrets))
}

// restageShadowedSecrets re-writes the create-time secrets whose names
// a just-removed exec-time or job secret shadowed. Re-staging takes the
// per-lease secrets gate; while a save holds it the re-stage is skipped
// — the save restores every create-time secret after its checkpoint —
// so a shadowed file can never land between the save's scrub and its
// checkpoint (R2). The caller has already removed the exec-time files.
func (s *Service) restageShadowedSecrets(leaseID, sandboxID string, names []string) {
	create := s.createSecretsFor(leaseID)
	shadowed := shadowedNames(create, names)
	if len(shadowed) == 0 {
		return
	}
	if !s.secretsGate.beginStaging(leaseID) {
		return
	}
	defer s.secretsGate.endStaging(leaseID)
	s.restageSecrets(sandboxID, shadowed, create)
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

// leaseBusyOrSuspended reports whether a live lease is suspended or has
// an in-flight lifecycle operation (pause, resume, restart or restore).
// Either way the substrate cannot be reached to signal a job: a pause
// has already taken the lease busy and is moving its memory, and the cap
// defers to the first reconcile that sees the lease running and idle
// (spoond-wb5).
func (s *Service) leaseBusyOrSuspended(leaseID string) bool {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if l := s.store.leases[leaseID]; l != nil && !l.released {
		return l.Suspended || l.busy
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
// alone, so the watcher and the reconcile pass cannot both count it. A
// job the max-runtime cap has flagged is left to that path: the cap owns
// the outcome, so the watcher must not record the kill's signal exit as
// a normal one.
func (s *Service) finishJob(ctx context.Context, job store.JobRow, exitCode int, stderrTail, sandboxID string, generation int64) error {
	if s.jobIsTimingOut(job.JobID) {
		return nil
	}
	changed, err := s.db.UpdateJobExit(ctx, job.JobID, exitCode, time.Now().UTC(), stderrTail)
	if err != nil {
		return err
	}
	s.clearJobTimingOut(job.JobID)
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
// alone. sandboxID/generation name the sandbox the job ran in. A job the
// max-runtime cap was already killing loses its in-memory timed-out
// intent: the lost record, not the cap, owns the outcome now.
func (s *Service) markJobLost(ctx context.Context, job store.JobRow, sandboxID string, generation int64, detail string) {
	changed, err := s.db.MarkJobLost(ctx, job.JobID, time.Now().UTC())
	if err != nil {
		s.storeError("mark_job_lost", job.JobID, err)
		return
	}
	if !changed {
		s.clearJobTimingOut(job.JobID)
		return
	}
	s.clearJobTimingOut(job.JobID)
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

// jobKillPidBackoff bounds how often the max-runtime kill retries a
// running job whose pid file has not appeared. jobPID waits for it on
// each attempt (see signalJob), so without this a job whose file never
// appears would spend that wait on every reconcile pass.
const jobKillPidBackoff = 30 * time.Second

// errJobPIDMissing marks a jobPID that ran out its wait for the pid file.
// It wraps substrate.ErrNotFound (via jobPID) so callers keep answering
// "job is not running".
var errJobPIDMissing = errors.New("no pid file")

// markJobTimingOut flags a job as being killed by the cap, so the live
// watcher's finishJob leaves the record to the cap path.
func (s *Service) markJobTimingOut(jobID string) {
	s.timingOutMu.Lock()
	s.timingOut[jobID] = struct{}{}
	s.timingOutMu.Unlock()
}

// clearJobTimingOut drops the flag once the cap path is done with the job.
func (s *Service) clearJobTimingOut(jobID string) {
	s.timingOutMu.Lock()
	delete(s.timingOut, jobID)
	delete(s.jobKillNotBefore, jobID)
	s.timingOutMu.Unlock()
}

// jobKillDue reports whether the max-runtime kill may retry a job now,
// honouring the backoff a missing pid file set.
func (s *Service) jobKillDue(jobID string) bool {
	s.timingOutMu.Lock()
	defer s.timingOutMu.Unlock()
	at, ok := s.jobKillNotBefore[jobID]
	return !ok || !s.now().Before(at)
}

// deferJobKill pushes the next max-runtime kill of a job out by the
// backoff, so a pid file that never appears does not cost the pid wait
// on every reconcile pass.
func (s *Service) deferJobKill(jobID string) {
	s.timingOutMu.Lock()
	s.jobKillNotBefore[jobID] = s.now().Add(jobKillPidBackoff)
	s.timingOutMu.Unlock()
}

// pruneJobKillState drops the in-memory timing-out intent and pid
// backoff of jobs that are no longer running (a released lease's jobs
// are deleted with it), so an error path cannot leak entries for the
// life of the process. rows is the current set of running jobs.
func (s *Service) pruneJobKillState(rows []store.JobRow) {
	running := make(map[string]struct{}, len(rows))
	for _, job := range rows {
		running[job.JobID] = struct{}{}
	}
	s.timingOutMu.Lock()
	for id := range s.timingOut {
		if _, ok := running[id]; !ok {
			delete(s.timingOut, id)
		}
	}
	for id := range s.jobKillNotBefore {
		if _, ok := running[id]; !ok {
			delete(s.jobKillNotBefore, id)
		}
	}
	s.timingOutMu.Unlock()
}

// jobIsTimingOut reports whether the cap is killing a job right now, so
// the watcher does not record the kill's signal exit as a normal one.
func (s *Service) jobIsTimingOut(jobID string) bool {
	s.timingOutMu.Lock()
	defer s.timingOutMu.Unlock()
	_, ok := s.timingOut[jobID]
	return ok
}

// killJobTimeout stops a job that has spent its effective max runtime.
// It flags the job in memory first, so the live watcher cannot record
// the kill's signal exit as a normal exit in the window before the
// record is closed, then signals the job's process group in the sandbox
// the job ran in. The record is marked exited with reason timed_out once
// the process is gone; when the kill succeeds but the store write fails,
// the in-memory timed-out intent is kept so the next pass records the
// outcome without signalling a process that is already gone. A failed
// kill clears the flag and returns false, leaving the record running so
// the next reconcile tries again; a job that ended on its own in the
// meantime is then closed by the normal rc path. A job whose pid file
// never appears is retried on a backoff, so it does not spend the pid
// wait on every pass. The stderr tail is read before the kill so the
// command's output is kept.
func (s *Service) killJobTimeout(ctx context.Context, job store.JobRow, sandboxID string, generation int64) bool {
	if s.leaseBusyOrSuspended(job.LeaseID) {
		return false
	}
	tail, _ := s.readJobStderrTail(ctx, sandboxID, job.JobID, jobStderrTailBytes, job)
	// A previous pass already killed the process but could not record the
	// outcome: do not signal an already-gone process, just record again.
	if !s.jobIsTimingOut(job.JobID) {
		// Back off a job whose pid file never appeared: jobPID waits for
		// it, so a job long past its cap without one should not spend that
		// wait on every reconcile pass.
		if !s.jobKillDue(job.JobID) {
			return false
		}
		s.markJobTimingOut(job.JobID)
		// Signal the sandbox the job ran in, never a new one after a cold
		// restart: sandboxID/generation were snapshotted by the caller while
		// the lease was on the job's continuity generation.
		if err := s.signalJob(ctx, job.LeaseID, sandboxID, job, "KILL"); err != nil {
			s.clearJobTimingOut(job.JobID)
			if errors.Is(err, errJobPIDMissing) {
				s.deferJobKill(job.JobID)
			}
			s.log.Printf("jobs: %s: max runtime spent; kill: %v", job.JobID, err)
			return false
		}
	}
	// 124 is the conventional timeout exit code, the same exec's own
	// timeout uses; the wrapper may not write rc for a KILL, and the cap,
	// not the command's own status, is what the record reports.
	if err := s.finishJobTimedOut(ctx, job, 124, tail, sandboxID, generation); err != nil {
		// The kill succeeded, so the next pass must record the outcome,
		// not try to signal a gone process. Keep the in-memory intent set
		// and say so; finishJobTimedOut leaves it set on error.
		s.storeError("mark_job_timed_out", job.JobID, err)
	}
	return true
}

// finishJobTimedOut marks a running job exited because its max runtime
// was spent and emits the job_exited event with a detail naming the cap.
// The caller has already killed the process, so this only records the
// outcome, releases the job's bookkeeping and cleans up its secrets.
// Like finishJob it is idempotent: an already-finished record is left
// alone, so the watcher and the reconcile cannot both count it. The
// in-memory timing-out flag is cleared once the store write succeeds (or
// finds the record already finished); on a store error it is left set,
// so a caller that has already killed the process can retry the mark on
// the next pass without a second kill.
func (s *Service) finishJobTimedOut(ctx context.Context, job store.JobRow, exitCode int, stderrTail, sandboxID string, generation int64) error {
	changed, err := s.db.MarkJobTimedOut(ctx, job.JobID, exitCode, time.Now().UTC(), stderrTail)
	if err != nil {
		return err
	}
	s.clearJobTimingOut(job.JobID)
	if !changed {
		return nil // another path already finished it
	}
	s.emitLeaseEvent(job.LeaseID, job.Owner, LeaseJobExited, jobTimedOutDetail(exitCode, stderrTail))
	s.jobFinishedMetrics(jobTimedOutReason)
	s.decRunningJob(job.LeaseID)
	s.removeJobSecrets(job.LeaseID, job.JobID, sandboxID, generation)
	return nil
}

// jobTimedOutDetail renders the job_exited event detail for a job the max
// runtime killed: "timed out" plus the exit code and stderr excerpt, so
// the owner sees the cap, not just a signal exit, ended it.
func jobTimedOutDetail(exitCode int, stderrTail string) string {
	detail := fmt.Sprintf("timed out: exit %d", exitCode)
	if lines := lastLines(toValidUTF8(stderrTail), jobEventStderrLines); lines != "" {
		detail += "\n" + lines
	}
	if len(detail) > jobEventDetailBytes {
		detail = cutToRunes(detail, jobEventDetailBytes)
	}
	return detail
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
func (s *Service) readJobStderrTail(ctx context.Context, sandboxID, jobID string, n int, job store.JobRow) (string, error) {
	data, _, _, err := s.readJobRange(ctx, sandboxID, jobID, "stderr", -1, n, job)
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

// lastLines returns the last n lines of s, keeping empty ones.
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
// lease's own deletion loses it. On a running lease, a job that has
// spent its effective max runtime is killed and marked exited with
// reason timed_out first (spoond-wb5), so a `sleep infinity` cannot pin
// a lease's memory and hugepages forever; a suspended lease's job is
// left for the first reconcile after its resume.
func (s *Service) reconcileJobs(ctx context.Context) {
	rows, err := s.db.ListRunningJobs(ctx)
	if err != nil {
		s.storeError("list_running_jobs", "", err)
		return
	}
	s.pruneJobKillState(rows)
	now := s.now()
	for _, job := range rows {
		sandboxID, generation, ok := s.leaseContinuity(job.LeaseID)
		if !ok {
			continue // the lease is gone; its records went with it
		}
		if generation > job.Generation || s.leaseState(job.LeaseID) == "lost" {
			s.markJobLost(ctx, job, sandboxID, generation, "lease generation changed; the job did not survive")
			continue
		}
		// A planned pause continues the memory: leave the job running and
		// reconcile after resume. A lease busy with an in-flight pause,
		// resume, restart or restore cannot be signalled either, so the cap
		// waits for the first reconcile that sees the lease running and
		// idle. Do NOT call markActive here: a suspended lease is not
		// activity. The max-runtime cap is enforced only once the lease is
		// running again: a suspended lease has already freed its memory,
		// and the substrate cannot kill a process in a paused guest — the
		// first reconcile after resume kills a job whose cap was spent while
		// the lease was paused (spoond-wb5).
		if s.leaseBusyOrSuspended(job.LeaseID) {
			continue
		}
		// The max runtime is wall-clock from the start, so a job that
		// spent its cap while the lease was paused is killed at once on
		// the first reconcile after resume. A failed kill leaves the
		// record running; fall through to the rc check so a job that ended
		// on its own is still noticed, and the next reconcile retries the
		// kill for one that is still alive.
		if hasSpentRuntime(job, now, s.jobMaxRuntime()) {
			if s.killJobTimeout(ctx, job, sandboxID, generation) {
				continue
			}
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
		// The output cap runs only for a job still appending (spoond-hwto):
		// an exited one cannot grow its files, and a finished stderr tail
		// must not be re-punched behind the record's own tail copy.
		if !done {
			s.capJobStreams(ctx, job, sandboxID)
		}
		if !done {
			// A running job in a live lease is activity for the idle
			// rules, but the startup seed already counts it; only record
			// it here so reconcile keeps the lease awake while the command
			// runs (2.6, #135).
			s.markActive(job.LeaseID)
			continue
		}
		stderr, _ := s.readJobStderrTail(ctx, sandboxID, job.JobID, jobStderrTailBytes, job)
		if err := s.finishJob(ctx, job, code, stderr, sandboxID, generation); err != nil {
			s.log.Printf("jobs: reconcile %s: finish: %v", job.JobID, err)
		}
	}
}

// pruneJobs removes exited job records past their retention windows and
// then their guest files, so /var/lib/spoond/jobs does not grow without
// bound. Each row's own window applies (spoond-hwto): a job that asked
// for a shorter retention_secs than the host's JOB_RETENTION_SECS is
// pruned — and its guest directory removed — earlier. Lost records have
// no guest files left to clean, but their rows are swept on the host
// window (spoond-966 L3). Called from the sweeper.
func (s *Service) pruneJobs(ctx context.Context) {
	now := s.now()
	expired, err := s.db.ListExpiredJobs(ctx, now, s.jobRetention())
	if err != nil {
		s.storeError("list_expired_jobs", "", err)
		return
	}
	if _, err := s.db.PruneJobs(ctx, now, s.jobRetention()); err != nil {
		s.storeError("prune_jobs", "", err)
		return
	}
	for _, job := range expired {
		if sandboxID, _, ok := s.leaseContinuity(job.LeaseID); ok {
			s.cleanupJobFiles(ctx, sandboxID, job.JobID)
		}
	}
	if _, err := s.db.PruneLostJobs(ctx, now.Add(-s.jobRetention())); err != nil {
		s.storeError("prune_lost_jobs", "", err)
	}
}

// readJobOutput reads the last n bytes of a job's stdout or stderr for
// the record view. The bytes are made valid UTF-8 before they are
// emitted; the start and the skipped view (spoond-hwto) tell the view
// where in the absolute offsets the answer sits and what the cap has
// dropped in front of it.
func (s *Service) readJobOutput(ctx context.Context, sandboxID, jobID, stream string, n int, job store.JobRow) (string, int64, jobStreamSkipped, error) {
	data, start, skip, err := s.readJobRange(ctx, sandboxID, jobID, stream, -1, n, job)
	return toValidUTF8(string(data)), start, skip, err
}

// jobRangeReader is the optional substrate extension the output
// endpoints use to read a byte range without downloading everything
// before it. The e2b and fake substrates implement it.
type jobRangeReader interface {
	ReadFileRange(ctx context.Context, sandboxID, path string, offset, limit int64) ([]byte, error)
}

// readJobRange reads a byte range of a job's stdout or stderr. offset
// < 0 means the last limit bytes; otherwise it reads limit bytes from
// offset. A missing file is empty output, not an error. When the record
// carries a dropped front — the output cap (spoond-hwto) — bytes below
// `dropped` are gone from the file: a read that would begin there is
// clamped to the first kept byte (the offset the answer starts at, an
// absolute one: after the punch the file's own offsets already sit
// above the dropped front), and any byte the answer skips over is
// summed into `skip` so the caller can say what a follower must not
// assume contiguous.
func (s *Service) readJobRange(ctx context.Context, sandboxID, jobID, stream string, offset, limit int, job store.JobRow) (data []byte, start int64, skip jobStreamSkipped, err error) {
	path := jobPath(jobID, stream)
	dropped := jobRowDropped(job, stream)
	skipBytes := dropped
	if offset < 0 {
		info, serr := s.sub.Stat(ctx, sandboxID, path)
		if serr != nil {
			if errors.Is(serr, substrate.ErrNotFound) {
				return nil, 0, jobStreamSkipped{}, nil
			}
			return nil, 0, jobStreamSkipped{}, serr
		}
		// The tail read resolves at the file's present size (its file
		// offsets are the response's absolute ones).
		offset = int(info.Size) - limit
		if offset < 0 {
			offset = 0
		}
		if int64(offset) < dropped {
			offset = int(dropped)
		}
	} else if int64(offset) < dropped {
		// A read into the dropped front: not an error, it starts at the
		// first kept byte.
		offset = int(dropped)
	}
	// The offset read is file-relative — after the punch the file's own
	// offsets ARE the absolute ones, so the first kept byte's offset is
	// `dropped` itself.
	skip = jobStreamSkipped{}
	if skipBytes > 0 {
		skip = jobStreamSkipped{
			Bytes:    skipBytes,
			Newlines: jobStreamNewlines(job, stream),
		}
	}
	start = int64(offset)
	if rr, ok := s.sub.(jobRangeReader); ok {
		data, err = rr.ReadFileRange(ctx, sandboxID, path, int64(offset), int64(limit))
	} else {
		// Fallback: read the prefix up to offset+limit and slice. Fine
		// for substrates without the optional range read. A file larger
		// than the requested prefix cannot be read whole, so a tail
		// request degrades to empty rather than an error the caller
		// would surface as unavailable.
		var pre []byte
		pre, err = s.sub.ReadFile(ctx, sandboxID, path, int64(offset+limit))
		if errors.Is(err, substrate.ErrTooLarge) {
			// The stream outgrew this substrate's whole-file read cap: a
			// tail of it cannot be served without the range reader, so
			// degrade to empty rather than an error the caller would
			// surface as unavailable, the way the pre-cap code did.
			return nil, start, skip, nil
		}
		if err == nil && offset < len(pre) {
			end := offset + limit
			if end > len(pre) {
				end = len(pre)
			}
			data = pre[offset:end]
		}
	}
	if err != nil {
		if errors.Is(err, substrate.ErrNotFound) {
			return nil, start, skip, nil
		}
		return nil, start, skip, err
	}
	return data, start, skip, nil
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
