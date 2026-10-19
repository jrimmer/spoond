package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A store-view wrapper test (scratch start): prove a narrowed view of
// the DB can wrap UpdateJobSkipped without losing the other methods.

// viewJobs is a jobs view over the DB whose UpdateJobSkipped fails once.
type viewJobs struct {
	JobStore
	inner *DB
	n     int
	err   error
}

func (v *viewJobs) UpdateJobSkipped(ctx context.Context, jobID, stream string, dropped, newlines int64) error {
	v.n++
	if v.err != nil && v.n == 2 {
		return v.err
	}
	return v.inner.UpdateJobSkipped(ctx, jobID, stream, dropped, newlines)
}

var errInjected = errors.New("injected store failure")

// TestScratchViewJobs proves the embedded-interface pattern compiles
// and passes through.
func TestScratchViewJobs(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	if err := db.UpsertLease(ctx, LeaseRow{
		ID: "l-1", Owner: "alice", Image: "py-base",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), LastActive: time.Now(),
		State: "running", Class: "guaranteed",
	}); err != nil {
		t.Fatalf("upsert lease: %v", err)
	}
	v := &viewJobs{inner: db}
	if err := v.UpdateJobSkipped(ctx, "j-s", "stdout", 10, 5); err != nil {
		t.Fatalf("pass-through: %v", err)
	}
}