package store

import (
	"context"
	"testing"
	"time"
)

// Scratch: MAX() semantics — a pair (dropped, newlines) written one
// component at a time.
func TestScratchUpdateJobSkippedMax(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.UpsertLease(ctx, LeaseRow{
		ID: "l-1", Owner: "alice", Image: "py-base",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), LastActive: time.Now(),
		State: "running", Class: "guaranteed",
	}); err != nil {
		t.Fatalf("upsert lease: %v", err)
	}
	if err := db.InsertJob(ctx, JobRow{
		JobID: "j-s", LeaseID: "l-1", Owner: "alice", Cmd: "echo",
		State: "exited", StartedAt: time.Now(), Generation: 1,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Component 1 written first, at its final value; component 2 later.
	if err := db.UpdateJobSkipped(ctx, "j-s", "stdout", 8192, 8192); err != nil {
		t.Fatalf("update bytes: %v", err)
	}
	if err := db.UpdateJobSkipped(ctx, "j-s", "stdout", 4096, 10240); err != nil {
		t.Fatalf("update newlines: %v", err)
	}
	row, err := db.GetJob(ctx, "j-s")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// bytes stays 8192 (MAX), newlines rises to 10240 (MAX).
	if row.StdoutDroppedBytes != 8192 || row.StdoutDroppedNewlines != 10240 {
		t.Fatalf("bytes=%d newlines=%d", row.StdoutDroppedBytes, row.StdoutDroppedNewlines)
	}
}