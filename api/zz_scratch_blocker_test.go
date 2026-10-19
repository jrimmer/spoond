package api

import (
	"context"
	"strings"
	"testing"

	"github.com/jrimmer/spoond/v3/substrate/fake"
)

// Scratch reproduction of the sidecar-newlines blocker: two cap passes
// on one stream.
func TestScratchSidecarTwoPasses(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)

	p := fake.NewProcess(1901)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{
		"cmd":              "sleep 900",
		"output_max_bytes": 10,
	})

	// Pass 1: 4500 lines "a\n" (9000 bytes) → drop 4096 (2048 newlines).
	writeStream(t, sub, sandbox, jobID, "stdout", strings.Repeat("a\n", 4500))
	svc.reconcileJobs(context.Background())
	row, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	t.Logf("pass1: row dropped=%d newlines=%d", row.StdoutDroppedBytes, row.StdoutDroppedNewlines)

	// Pass 2: the file grows again with newlines in the region the next
	// pass plans to punch.
	writeStream(t, sub, sandbox, jobID, "stdout", strings.Repeat("a\n", 4500)+strings.Repeat("b\n", 5000))
	svc.reconcileJobs(context.Background())
	row2, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job 2: %v", err)
	}
	t.Logf("pass2: row dropped=%d newlines=%d", row2.StdoutDroppedBytes, row2.StdoutDroppedNewlines)

	raw, err := sub.ReadFile(context.Background(), sandbox, jobCapPath(jobID, "stdout"), 1<<20)
	if err != nil {
		t.Fatalf("sidecar: %v", err)
	}
	t.Logf("sidecar=%q", string(raw))
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	last := lines[len(lines)-1]
	d, n, ok := jobCapSidecarOf2(last)
	t.Logf("last sidecar line parsed: dropped=%d newlines=%d ok=%v", d, n, ok)
	if n != row2.StdoutDroppedNewlines {
		t.Errorf("MISMATCH: sidecar newlines %d vs row %d (dropped %d)", n, row2.StdoutDroppedNewlines, row2.StdoutDroppedBytes)
	}
}
// Scratch: the crash-recovery path — the sidecar (with a wrong newlines
// counter) is ahead of the row (the row write failed), and the next pass
// must land the counters.
func TestScratchSidecarRecovery(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)

	p := fake.NewProcess(1902)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{
		"cmd":              "sleep 900",
		"output_max_bytes": 10,
	})

	writeStream(t, sub, sandbox, jobID, "stdout", strings.Repeat("a\n", 4500))
	svc.reconcileJobs(context.Background())
	row, _ := db.GetJob(context.Background(), jobID)
	t.Logf("pass1 row: dropped=%d newlines=%d", row.StdoutDroppedBytes, row.StdoutDroppedNewlines)

	// The record write of pass 2 fails (the sidecar write succeeds
	// first, so the guest is ahead): the pass-2 reconcile's sidecar
	// write is the 3rd WriteFile call overall (pass 1's sidecar was the
	// 1st, the grown stream the 2nd).
	sub.FailCall("WriteFile", 1, context.Canceled)
	writeStream(t, sub, sandbox, jobID, "stdout", strings.Repeat("a\n", 4500)+strings.Repeat("b\n", 5000))
	svc.reconcileJobs(context.Background())

	row2, _ := db.GetJob(context.Background(), jobID)
	t.Logf("after failed pass, row: dropped=%d newlines=%d", row2.StdoutDroppedBytes, row2.StdoutDroppedNewlines)

	for i := 0; i < 5; i++ {
		svc.reconcileJobs(context.Background())
		rowN, _ := db.GetJob(context.Background(), jobID)
		t.Logf("recovery pass %d: row dropped=%d newlines=%d", i, rowN.StdoutDroppedBytes, rowN.StdoutDroppedNewlines)
		if rowN.StdoutDroppedNewlines >= 6144 {
			t.Logf("newlines recovered to %d", rowN.StdoutDroppedNewlines)
			return
		}
	}
	raw, _ := sub.ReadFile(context.Background(), sandbox, jobCapPath(jobID, "stdout"), 1<<20)
	t.Fatalf("row newlines never reached 6144: %v, sidecar=%q", row2, raw)
}

// Scratch: the wrapper-runner recovery — a real-wrapper job with a big
// burst: does the cap with a FAILED row write recover on the next pass,
// and does it keep absolute offsets?
func TestScratchWrapperRecovery(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)
	useJobRunner(t, sub)

	// 26 lines of 24 bytes (624 bytes) — enough for a first pass under
	// the 10-byte cap to punch when combined with a second write? No:
	// drive the failure path directly with pre-written files instead.
	p := fake.NewProcess(1903)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{
		"cmd":              "sleep 900",
		"output_max_bytes": 10,
	})
	name := jobPath(jobID, "stdout")

	// Pass 1: 9000 bytes (4500 lines), punch to 4096 (2048 newlines).
	writeStream2(t, sub, sandbox, jobID, name, strings.Repeat("a\n", 4500))
	svc.reconcileJobs(context.Background())
	row, _ := db.GetJob(context.Background(), jobID)
	t.Logf("pass1 row: dropped=%d newlines=%d", row.StdoutDroppedBytes, row.StdoutDroppedNewlines)

	// Pass 2's record write fails: the row stays behind the guest (the
	// sidecar, written first, lands).
	fdb := &failingJobStore{JobStore: svc.dbJobs, method: "UpdateJobSkipped", nth: 2, err: errStoreBoom}
	svc.dbJobs = fdb
	writeStream2(t, sub, sandbox, jobID, name, strings.Repeat("a\n", 4500)+strings.Repeat("b\n", 5000))
	svc.reconcileJobs(context.Background())
	svc.dbJobs = fdb.JobStore
	row2, _ := db.GetJob(context.Background(), jobID)
	t.Logf("after failed pass, row: dropped=%d newlines=%d", row2.StdoutDroppedBytes, row2.StdoutDroppedNewlines)

	for i := 0; i < 5; i++ {
		svc.reconcileJobs(context.Background())
		rowN, _ := db.GetJob(context.Background(), jobID)
		b, _ := sub.ReadFile(context.Background(), sandbox, name, 1<<20)
		t.Logf("recovery pass %d: row dropped=%d newlines=%d size=%d", i, rowN.StdoutDroppedBytes, rowN.StdoutDroppedNewlines, len(b))
		if rowN.StdoutDroppedNewlines >= 6144 {
			t.Logf("newlines recovered exactly")
			return
		}
	}
	raw, _ := sub.ReadFile(context.Background(), sandbox, jobCapPath(jobID, "stdout"), 1<<20)
	t.Fatalf("row newlines never reached 6144: row=%v sidecar=%q", row2, raw)
}

// writeStream2 writes through the fake by absolute guest path.
func writeStream2(t *testing.T, sub *testSub, sandboxID, jobID, path, content string) {
	t.Helper()
	if err := sub.WriteFile(context.Background(), sandboxID, path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func scratchTail(b []byte, n int) string {
	if len(b) > n {
		return string(b[len(b)-n:])
	}
	return string(b)
}
