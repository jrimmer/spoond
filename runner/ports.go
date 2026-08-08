// Package runner implements a general-purpose ephemeral job executor.
// The domain core (Executor) depends only on the ports defined here;
// concrete adapters (Forgejo, lease API, exe.dev, ...) live in
// runner/adapters and are wired in by config.
package runner

import (
	"context"
	"time"
)

// Result is the outcome of a job or step.
type Result int

const (
	ResultSuccess Result = iota
	ResultFailure
	ResultCancelled
	ResultSkipped
)

// Job is a unit of work to execute in a sandbox. It is transport-agnostic:
// a Forgejo task, an exe.dev harness request, or a pi/code-harness job all
// map onto it.
type Job struct {
	ID       int64
	Workflow []byte
	Secrets  map[string]string
	Vars     map[string]string
	// Context holds job-scoped values (e.g. github.repository).
	Context map[string]string
}

// StepState is the outcome of one step in a job.
type StepState struct {
	ID        int64
	Result    Result
	LogIndex  int64
	LogLength int64
}

// JobState is the outcome of a whole job.
type JobState struct {
	ID     int64
	Result Result
	Steps  []StepState
}

// LogRow is a single log line streamed back to the job source.
type LogRow struct {
	Time    time.Time
	Content string
}

// ExecResult is the result of a single exec call.
type ExecResult struct {
	Stdout string
	Stderr string
	Exit   int
}

// SandboxProvider grants and releases sandboxes. It is the port the
// executor uses to obtain compute. Adapters: lease HTTP API, direct
// forkd client, exe.dev backend.
type SandboxProvider interface {
	Create(ctx context.Context, image string, ttl int) (string, error)
	Exec(ctx context.Context, id, cmd, cwd string, env map[string]string, timeout int) (*ExecResult, error)
	Delete(ctx context.Context, id string) error
}

// JobSource yields jobs to execute. Adapters: Forgejo runner protocol,
// exe.dev harness, pi/code harness.
type JobSource interface {
	// Fetch returns the next job, or nil if none is available. version
	// is the caller's last-seen version; the adapter returns the latest
	// version so the caller can detect new jobs.
	Fetch(ctx context.Context, version int64) (*Job, int64, error)
}

// JobSink reports job results and streams logs back to the source.
// Adapters: Forgejo runner protocol, exe.dev harness.
type JobSink interface {
	Report(ctx context.Context, state *JobState, outputs map[string]string) error
	Log(ctx context.Context, jobID, index int64, rows []*LogRow, noMore bool) error
}
