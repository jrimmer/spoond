package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A failing job must leave a record that names the step that died, with its
// exit code and output tail. Without it a red build reaches the consumer as a
// bare `failure` — Forgejo exposes no readable log API.
func TestExecutorRecordsFailingStep(t *testing.T) {
	payload := `
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - name: setup
        run: echo setup
      - name: build
        run: exit 3
      - name: never runs
        run: echo unreachable
`
	lease := newFakeLease()
	lease.results["echo setup"] = &ExecResult{Stdout: "setup\n", Exit: 0}
	lease.results["exit 3"] = &ExecResult{Stdout: "partial output\n", Stderr: "boom\n", Exit: 3}
	sink := &fakeSink{}
	dir := t.TempDir()
	exec := &Executor{
		Sandbox:      lease,
		Sink:         sink,
		Labels:       map[string]string{"ubuntu-latest": "py-base"},
		DefaultImage: "py-base",
		TTL:          600,
		RecordDir:    dir,
	}
	job := testJob(payload)

	// The journal line is what an operator sees on the host; it must name the
	// failing step rather than only "result=1 steps=2".
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(prev)

	if err := exec.Run(context.Background(), job); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !strings.Contains(logs.String(), "failed_step=1(build) exit=3") {
		t.Errorf("journal does not name the failing step:\n%s", logs.String())
	}

	data, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("job-%d.json", job.ID)))
	if err != nil {
		t.Fatalf("no job record written: %v", err)
	}
	var rec JobRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("record is not valid JSON: %v", err)
	}
	if rec.Result != "failure" {
		t.Errorf("Result = %q, want failure", rec.Result)
	}
	// Two steps ran: the one that failed is last, and the third never started.
	if len(rec.Steps) != 2 {
		t.Fatalf("recorded %d steps, want 2: %+v", len(rec.Steps), rec.Steps)
	}
	last := rec.Steps[len(rec.Steps)-1]
	if last.Name != "build" {
		t.Errorf("failing step name = %q, want build", last.Name)
	}
	if last.Index != 1 {
		t.Errorf("failing step index = %d, want 1 (0-based position in the workflow)", last.Index)
	}
	if last.Exit != 3 {
		t.Errorf("failing step exit = %d, want 3", last.Exit)
	}
	if !strings.Contains(last.Stderr, "boom") {
		t.Errorf("failing step stderr tail = %q, want it to contain boom", last.Stderr)
	}
	if rec.Steps[0].Result != "success" {
		t.Errorf("first step result = %q, want success", rec.Steps[0].Result)
	}
}

// A success writes no record: records exist to explain failures, and writing
// one per job would need a retention policy that a failure-only file does not.
func TestExecutorRecordsNothingOnSuccess(t *testing.T) {
	payload := `
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - name: fine
        run: echo fine
`
	lease := newFakeLease()
	sink := &fakeSink{}
	dir := t.TempDir()
	exec := &Executor{
		Sandbox:      lease,
		Sink:         sink,
		Labels:       map[string]string{"ubuntu-latest": "py-base"},
		DefaultImage: "py-base",
		TTL:          600,
		RecordDir:    dir,
	}
	if err := exec.Run(context.Background(), testJob(payload)); err != nil {
		t.Fatalf("run: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read record dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no records on success, got %v", entries)
	}
}

// Failures that happen before the step loop (no sandbox) are recorded too —
// "cannot create sandbox" is exactly what a consumer cannot see from Forgejo.
func TestExecutorRecordsFailureBeforeSteps(t *testing.T) {
	payload := `
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hello
`
	lease := newFakeLease()
	sink := &fakeSink{}
	dir := t.TempDir()
	exec := &Executor{
		Sandbox:      lease,
		Sink:         sink,
		Labels:       map[string]string{"ubuntu-latest": "no-such-image"},
		DefaultImage: "no-such-image",
		TTL:          600,
		RecordDir:    dir,
	}
	job := testJob(payload)
	if err := exec.Run(context.Background(), job); err == nil {
		t.Fatal("expected an error when the sandbox cannot be created")
	}
	data, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("job-%d.json", job.ID)))
	if err != nil {
		t.Fatalf("no record for a pre-step failure: %v", err)
	}
	var rec JobRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("record is not valid JSON: %v", err)
	}
	if !strings.Contains(rec.Error, "create sandbox") {
		t.Errorf("recorded error = %q, want it to name the failed sandbox create", rec.Error)
	}
	if len(rec.Steps) != 0 {
		t.Errorf("recorded %d steps for a run that never reached the loop", len(rec.Steps))
	}
}

// The record directory is bounded so a run of failures cannot fill the disk.
func TestPruneJobRecordsKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 6; i++ {
		rec := &JobRecord{JobID: int64(i), Result: "failure"}
		writeJobRecord(dir, rec)
		// writeJobRecord uses the file mtime for ordering, and a tight loop
		// can produce identical timestamps — bump them explicitly.
		path := filepath.Join(dir, fmt.Sprintf("job-%d.json", i))
		if err := os.Chtimes(path, recordMtime(i), recordMtime(i)); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}
	pruneJobRecords(dir, 2)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("kept %d records, want 2", len(entries))
	}
	// The two newest survive.
	for _, want := range []string{"job-4.json", "job-5.json"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("%s should have been kept: %v", want, err)
		}
	}
}

// recordMtime gives a deterministic, increasing mtime per index.
func recordMtime(i int) time.Time {
	return time.Unix(int64(1700000000+i), 0)
}
