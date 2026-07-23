package runner

import (
	"context"
	"fmt"
	"log"
	"time"

	runnerv1 "gitea.dev/actions-proto-go/runner/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fetchInterval is the delay between FetchTask calls when no task is returned.
// The Forgejo runner protocol is a long-poll: the server holds the connection
// open until a task is available or the server-side timeout fires (typically
// ~60s). We set our client deadline slightly longer than that.
const (
	fetchTimeout  = 70 * time.Second // server long-poll is ~60s; add margin
	declareDelay  = 5 * time.Second  // initial delay before first FetchTask
)

// Run starts the long-poll loop, blocking until ctx is cancelled.
//
// The loop:
//  1. Load runner credentials from .runner file
//  2. Declare version + labels to Forgejo
//  3. Enter FetchTask long-poll loop
//  4. On receiving a task, dispatch to the task executor (T7/T8)
//  5. Report task state via UpdateTask
func (d *Daemon) Run(ctx context.Context) error {
	creds, err := d.loadOrRegister()
	if err != nil {
		return err
	}

	d.forgejo = NewForgejoClient(d.cfg.ForgejoURL, creds.Token)
	d.creds = creds

	// Declare our version and labels.
	if err := d.declare(ctx); err != nil {
		return fmt.Errorf("declare: %w", err)
	}

	log.Printf("runner %s started, polling %s for tasks", creds.Name, d.cfg.ForgejoURL)

	// Enter the FetchTask loop.
	tasksVersion := int64(0)
	for {
		if ctx.Err() != nil {
			log.Printf("runner shutting down")
			return ctx.Err()
		}

		task, newVersion, err := d.fetchTask(ctx, tasksVersion)
		if err != nil {
			if ctx.Err() != nil {
				log.Printf("runner shutting down")
				return ctx.Err()
			}
			log.Printf("fetch task error: %v (retrying in 5s)", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
			continue
		}

		if newVersion > tasksVersion {
			tasksVersion = newVersion
		}

		if task == nil {
			// No task available; the long-poll timed out. Loop immediately
			// to issue the next FetchTask.
			continue
		}

		// We got a task. Execute it.
		log.Printf("received task %d (workflow payload: %d bytes)", task.Id, len(task.WorkflowPayload))
		if err := d.executeTask(ctx, task); err != nil {
			log.Printf("task %d failed: %v", task.Id, err)
		}
	}
}

// declare announces the runner's version and labels to Forgejo.
func (d *Daemon) declare(ctx context.Context) error {
	if d.forgejo == nil {
		return fmt.Errorf("forgejo client not initialised")
	}
	log.Printf("declaring runner version=%s labels=%v", RunnerVersion, d.cfg.Labels)
	_, err := d.forgejo.Declare(ctx, RunnerVersion, d.cfg.Labels)
	return err
}

// fetchTask wraps the ForgejoClient.FetchTask call with a deadline.
// Returns the task (if any), the new tasks_version, and any error.
func (d *Daemon) fetchTask(ctx context.Context, tasksVersion int64) (*runnerv1.Task, int64, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	resp, err := d.forgejo.FetchTask(fetchCtx, tasksVersion)
	if err != nil {
		// If the context was cancelled (shutdown), don't treat as error.
		if ctx.Err() != nil {
			return nil, tasksVersion, ctx.Err()
		}
		return nil, tasksVersion, err
	}

	if resp.Task != nil {
		return resp.Task, resp.TasksVersion, nil
	}
	return nil, resp.TasksVersion, nil
}

// executeTask handles a single task: report running state, execute the
// workflow, and report the final result. This is a skeleton that T6-T8
// will flesh out with actual workflow parsing and VM-based execution.
func (d *Daemon) executeTask(ctx context.Context, task *runnerv1.Task) error {
	log.Printf("executing task %d", task.Id)

	// Report task as running.
	now := timestamppb.Now()
	runningState := &runnerv1.TaskState{
		Id:        task.Id,
		Result:    runnerv1.Result_RESULT_UNSPECIFIED,
		StartedAt: now,
	}

	if _, err := d.forgejo.UpdateTask(ctx, runningState, nil); err != nil {
		log.Printf("task %d: failed to report running state: %v", task.Id, err)
	}

	// TODO (T6-T8): Parse workflow payload, create VM, execute steps, stream logs.
	// For now, log that we received the task and mark it as skipped since
	// the workflow execution engine is not yet implemented.
	log.Printf("task %d: workflow execution not yet implemented (T6-T8 pending), marking as skipped", task.Id)

	// Send a log line so the Forgejo UI shows something.
	logRows := []*runnerv1.LogRow{
		{
			Time:    timestamppb.Now(),
			Content: "hyper-forgejo-runner received task — workflow execution engine not yet implemented (T6-T8 pending)",
		},
	}
	if _, err := d.forgejo.UpdateLog(ctx, task.Id, 0, logRows, true); err != nil {
		log.Printf("task %d: failed to send log: %v", task.Id, err)
	}

	// Report task as skipped (workflow engine not ready).
	completedState := &runnerv1.TaskState{
		Id:        task.Id,
		Result:    runnerv1.Result_RESULT_SKIPPED,
		StartedAt: now,
		StoppedAt: timestamppb.Now(),
	}

	if _, err := d.forgejo.UpdateTask(ctx, completedState, nil); err != nil {
		return fmt.Errorf("report final state: %w", err)
	}

	log.Printf("task %d: completed (result=skipped)", task.Id)
	return nil
}