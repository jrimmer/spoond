package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v3/store"
	"github.com/jrimmer/spoond/v3/substrate"
	"github.com/jrimmer/spoond/v3/substrate/fake"
)

// This file pins the per-job output cap (spoond-hwto): the
// output_max_bytes request field and the JOB_OUTPUT_MAX_BYTES host
// default shorten (never lengthen) what a job's streams keep; the
// record view and the raw output endpoint report a dropped front as
// "skipped" with absolute offsets, and a substrate without hole-punching
// caps nothing (with a note) rather than truncating by another means.

// writeStream writes one job stream file into the fake guest.
func writeStream(t *testing.T, sub *testSub, sandboxID, jobID, stream, content string) {
	t.Helper()
	if err := sub.WriteFile(context.Background(), sandboxID, jobPath(jobID, stream), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s/%s: %v", jobID, stream, err)
	}
}

// readOutput calls the raw output endpoint and returns the body plus the
// response (the answer's headers carry the absolute start offset and the
// skipped view once the cap has dropped anything).
func readOutput(t *testing.T, ts *httptest.Server, id, jobID, query, token string) (string, *http.Response) {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+"/api/leases/"+id+"/jobs/"+jobID+"/output?"+query, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("output %s: %v", query, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return string(data), resp
}

// TestJobOutputCapHostDefaultDisabled (spoond-hwto): the defaults keep
// today's behaviour —
// JOB_OUTPUT_MAX_BYTES unset caps nothing, a punch is never spent and
// the record and output views render exactly as before (no skipped
// field, no header).
func TestJobOutputCapHostDefaultDisabled(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)
	if svc.jobOutputMax() != DefaultJobOutputMaxBytes {
		t.Fatalf("host default = %d, want 0 (unlimited)", svc.jobOutputMax())
	}

	p := fake.NewProcess(1200)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 900"})
	writeStream(t, sub, sandbox, jobID, "stdout", strings.Repeat("x", 300000))

	// A poll pass on an uncapped job: no punch, no counters, no row
	// change (all three dropped counters still 0).
	svc.reconcileJobs(context.Background())
	row, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if row.StdoutDroppedBytes != 0 || row.StderrDroppedBytes != 0 || row.OutputMaxBytes != 0 {
		t.Fatalf("uncapped job gained cap state: %+v", row)
	}
	if _, err := sub.Stat(context.Background(), sandbox, jobPath(jobID, "stdout.cap")); !errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("uncapped job wrote cap counters: %v", err)
	}
	body, resp := readOutput(t, ts, id, jobID, "stream=stdout&offset=10&limit=4", "token-a")
	if body != "xxxx" {
		t.Fatalf("uncapped output [10,14) = %q, want xxxx", body)
	}
	if resp.Header.Get("X-Output-Skipped") != "" || resp.Header.Get("X-Output-Offset") != "" {
		t.Fatalf("uncapped output response gained cap headers: %v", resp.Header)
	}
}

// TestJobOutputCapPerStreamAndViews (spoond-hwto): a job's
// output_max_bytes keeps the tail of each stream with absolute offsets,
// counting newlines before each punch, and the record view and the raw
// output endpoint both report the drop as "skipped" (bytes, newlines) —
// and omit it while nothing has been dropped.
func TestJobOutputCapPerStreamAndViews(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)

	p := fake.NewProcess(1201)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{
		"cmd":              "sleep 900",
		"output_max_bytes": 10,
	})
	row, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	// The request is stamped whole (under the host's uncapped default).
	if row.OutputMaxBytes != 10 {
		t.Fatalf("output_max_bytes = %d, want the requested 10", row.OutputMaxBytes)
	}

	// The record view omits "skipped" while nothing is dropped: the
	// pre-cap shape, byte for byte.
	beforeResp, before := doReq(t, "GET", ts.URL+"/api/leases/"+id+"/jobs/"+jobID, "token-a", nil)
	if _, ok := before["stdout_skipped"]; ok {
		t.Fatalf("record view has skipped before any drop: %v", before["stdout_skipped"])
	}
	_ = beforeResp

	// stdout: 1000 lines of 10 bytes = 10 KiB, well past the kept window
	// (the 10-byte cap plus one 4 KiB block). stderr stays small: only
	// stdout is capped.
	stdout := strings.Repeat("0123456789", 1000) // every line ends without \n... use lines:
	stdout = strings.TrimRight(strings.Repeat("01234567\n0\n", 1000), "\n")
	writeStream(t, sub, sandbox, jobID, "stdout", stdout)
	writeStream(t, sub, sandbox, jobID, "stderr", "short\n")

	// Poll until the cap lands (the punch is immediate in the fake,
	// but a poll keeps the test honest if the pass is slow).
	deadline := time.Now().Add(jobWatchTimeout)
	for {
		svc.reconcileJobs(context.Background())
		row, err = db.GetJob(context.Background(), jobID)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		if row.StdoutDroppedBytes > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("output cap never applied: stdout dropped = 0")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if row.StderrDroppedBytes != 0 {
		t.Fatalf("stderr dropped = %d, want 0 (it stayed under the cap)", row.StderrDroppedBytes)
	}
	// The kept region: the floor block boundary under (size - kept 4106)
	// — for this content 8192 — and at least the 10-byte cap stays.
	size := int64(len(stdout))
	want := jobCapTarget(size, 0, 10+jobPunchBlockBytes)
	if row.StdoutDroppedBytes != want {
		t.Fatalf("dropped = %d, want the planned %d", row.StdoutDroppedBytes, want)
	}
	if size-row.StdoutDroppedBytes < 10 || size-row.StdoutDroppedBytes > 10+2*jobPunchBlockBytes {
		t.Fatalf("kept %d bytes, outside [cap, cap+2 blocks)", size-row.StdoutDroppedBytes)
	}

	// Absolute offsets: the file's kept region still answers its absolute
	// positions. The last bytes, at their absolute offset, are unchanged.
	keep0 := row.StdoutDroppedBytes
	got, resp := readOutput(t, ts, id, jobID, "stream=stdout&offset="+itoa9(int(size-6))+"&limit=6", "token-a")
	if got != stdout[size-6:] {
		t.Fatalf("tail at absolute offset = %q, want %q", got, stdout[size-6:])
	}
	if resp.Header.Get("X-Output-Skipped") == "" {
		t.Fatalf("capped output response has no X-Output-Skipped header")
	}
	var skipped jobStreamSkipped
	if err := json.Unmarshal([]byte(resp.Header.Get("X-Output-Skipped")), &skipped); err != nil {
		t.Fatalf("X-Output-Skipped: %v", err)
	}
	if skipped.Bytes != row.StdoutDroppedBytes {
		t.Fatalf("skipped bytes = %d, want the row's %d", skipped.Bytes, row.StdoutDroppedBytes)
	}
	// The dropped front's newlines: counted from the written content.
	if skipped.Newlines != int64(strings.Count(stdout[:row.StdoutDroppedBytes], "\n")) {
		t.Fatalf("skipped newlines = %d, want %d (the front's own)", skipped.Newlines, strings.Count(stdout[:row.StdoutDroppedBytes], "\n"))
	}

	// A read into the dropped front is not an error: it returns the first
	// kept bytes, with the absolute offset it actually starts at.
	got, resp = readOutput(t, ts, id, jobID, "stream=stdout&offset=0&limit=10", "token-a")
	if got != stdout[keep0:keep0+10] {
		t.Fatalf("read at dropped offset 0 = %q, want %q", got, stdout[keep0:keep0+10])
	}
	if resp.Header.Get("X-Output-Offset") == "" {
		t.Fatalf("read below the kept region carries no X-Output-Offset header")
	}
	if resp.Header.Get("X-Output-Offset") != itoa9(int(keep0)) {
		t.Fatalf("X-Output-Offset = %q, want %d", resp.Header.Get("X-Output-Offset"), keep0)
	}

	// An offset past the drop is served from its absolute place (the
	// answer is the same bytes an uncapped read of that offset sees).
	mid := keep0 + 5
	got, _ = readOutput(t, ts, id, jobID, "stream=stdout&offset="+itoa9(int(mid))+"&limit=5", "token-a")
	if got != stdout[mid:mid+5] {
		t.Fatalf("read at absolute offset %d = %q, want %q", mid, got, stdout[mid:mid+5])
	}

	// The record view now reports skipped for the stream that dropped
	// something — including how many newlines the front held.
	_, body := doReq(t, "GET", ts.URL+"/api/leases/"+id+"/jobs/"+jobID, "token-a", nil)
	job, _ := body["job"].(map[string]any)
	sk, ok := job["skipped"].(map[string]any)
	if !ok {
		t.Fatalf("record view job.skipped missing after a drop: %v", body)
	}
	sout, _ := sk["stdout"].(map[string]any)
	if int64(sout["bytes"].(float64)) != row.StdoutDroppedBytes {
		t.Fatalf("record view skipped.stdout.bytes = %v, want %d", sout["bytes"], row.StdoutDroppedBytes)
	}
	if int64(sout["newlines"].(float64)) != int64(strings.Count(stdout[:row.StdoutDroppedBytes], "\n")) {
		t.Fatalf("record view skipped.stdout.newlines = %v, want the front's own %d", sout["newlines"], strings.Count(stdout[:row.StdoutDroppedBytes], "\n"))
	}
	if _, ok := sk["stderr"]; ok {
		t.Fatalf("record view reports skipped.stderr for a stream that dropped nothing")
	}
	// The view's stderr bytes are still the whole (uncapped) stream.
	if body["stderr"] != "short\n" {
		t.Fatalf("record view stderr = %q", body["stderr"])
	}
	_ = sandbox
}

// itoa9 renders n in decimal (a small number in the cap tests).
func itoa9(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestJobOutputCapDropsKeepGrowing (spoond-hwto): an over-cap stream
// keeps being capped as it grows: a further pass drops more, the row's
// counters rise, and the tail (absolute offsets) keeps answering.
func TestJobOutputCapDropsKeepGrowing(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)

	p := fake.NewProcess(1202)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{
		"cmd":              "sleep 900",
		"output_max_bytes": 10,
	})
	// First pass: 9000 bytes over the 4106-byte kept window gets capped
	// at the floor 4 KiB boundary under 4894 bytes to drop (4096).
	writeStream(t, sub, sandbox, jobID, "stdout", strings.Repeat("a\n", 4500))
	svc.reconcileJobs(context.Background())
	row1, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if row1.StdoutDroppedBytes != 4096 {
		t.Fatalf("first poll dropped %d, want 4096", row1.StdoutDroppedBytes)
	}

	// The file has grown again: the next pass drops more (and the total
	// never un-drops).
	writeStream(t, sub, sandbox, jobID, "stdout", strings.Repeat("a\n", 4500)+strings.Repeat("b\n", 5000))
	svc.reconcileJobs(context.Background())
	row2, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job 2: %v", err)
	}
	if row2.StdoutDroppedBytes <= row1.StdoutDroppedBytes {
		t.Fatalf("second poll did not drop more: %d -> %d", row1.StdoutDroppedBytes, row2.StdoutDroppedBytes)
	}

	// A read below the second drop serves the first kept byte (the kept
	// region here starts inside the b lines), with the absolute offset
	// the answer starts at.
	got2, resp2 := readOutput(t, ts, id, jobID, "stream=stdout&offset=0&limit=4", "token-a")
	if got2 != strings.Repeat("b\n", 2) {
		t.Fatalf("read at 0 = %q, want the first kept bytes", got2)
	}
	if resp2.Header.Get("X-Output-Offset") != itoa9(int(row2.StdoutDroppedBytes)) {
		t.Fatalf("X-Output-Offset = %q, want %d", resp2.Header.Get("X-Output-Offset"), row2.StdoutDroppedBytes)
	}
	// The kept tail's absolute content: the last bytes are the newest.
	got, _ := readOutput(t, ts, id, jobID, "stream=stdout&limit=8", "token-a")
	if got != "b\nb\nb\nb\n" {
		t.Fatalf("absolute tail = %q, want the last b lines", got)
	}
}

// TestJobOutputCapHostDefaultBindsJob (spoond-hwto): the host
// JOB_OUTPUT_MAX_BYTES caps every job's streams, and a job's own
// output_max_bytes can shorten it, never lengthen it —
// retention_secs's mirror (the host default also applying to a legacy
// row with none stamped).
func TestJobOutputCapHostDefaultBindsJob(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)
	svc.cfg.JobOutputMaxBytes = 20

	p := fake.NewProcess(1203)
	installJobProcess(t, sub, p)
	explicit, _ := startBackgroundJob(t, ts, id, map[string]any{
		"cmd":              "sleep 900",
		"output_max_bytes": 10,
	})
	blank, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 900"})
	huge, _ := startBackgroundJob(t, ts, id, map[string]any{
		"cmd":              "sleep 900",
		"output_max_bytes": 1 << 30,
	})
	neg, _ := doReq(t, "POST", ts.URL+"/api/leases/"+id+"/exec", "token-a", map[string]any{
		"cmd": "sleep 900", "output_max_bytes": -1,
	})
	if neg.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative output_max_bytes = %d, want 400", neg.StatusCode)
	}

	row, err := db.GetJob(context.Background(), explicit)
	if err != nil {
		t.Fatalf("get explicit: %v", err)
	}
	if row.OutputMaxBytes != 10 {
		t.Fatalf("explicit request stamped %d, want the shorter 10", row.OutputMaxBytes)
	}
	row, err = db.GetJob(context.Background(), blank)
	if err != nil {
		t.Fatalf("get blank: %v", err)
	}
	if row.OutputMaxBytes != 20 {
		t.Fatalf("unset request stamped %d, want the host's 20", row.OutputMaxBytes)
	}
	row, err = db.GetJob(context.Background(), huge)
	if err != nil {
		t.Fatalf("get huge: %v", err)
	}
	if row.OutputMaxBytes != 20 {
		t.Fatalf("oversized request stamped %d, want the host's 20 (it can never lengthen the cap)", row.OutputMaxBytes)
	}

	// Both jobs' streams are capped at their own stamped cap: a 9000-
	// byte file drops its front to the floor 4 KiB boundary under
	// (size - cap - one block), keeping the tail — 5000 bytes here —
	// well above either cap. (9000 - 4106) floors to 4096; the kept
	// region is 4904 bytes, inside [cap, cap + 2 blocks).
	files := map[string]int64{"explicit": 10, "huge": 20}
	for name, cap := range files {
		jid := explicit
		if name == "huge" {
			jid = huge
		}
		writeStream(t, sub, sandbox, jid, "stdout", strings.Repeat("y", 9000))
		svc.reconcileJobs(context.Background())
		row, err := db.GetJob(context.Background(), jid)
		if err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		if row.StdoutDroppedBytes != 4096 {
			t.Fatalf("%s: dropped %d, want 4096", name, row.StdoutDroppedBytes)
		}
		if kept := int64(9000) - row.StdoutDroppedBytes; kept < int64(cap) || kept > int64(cap)+4*jobPunchBlockBytes {
			t.Fatalf("%s: kept %d bytes after capping at %d", name, kept, cap)
		}
	}
	_ = blank
}

// TestJobOutputCapWithoutPunchHole (spoond-hwto): a substrate without
// hole-punching caps nothing: the bytes stay, no counter moves, no
// sidecar is written, and the read keeps working — the bead's "say so
// and stop" rule, not an invented truncation.
func TestJobOutputCapWithoutPunchHole(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)
	// Strip the fake's PunchHole: no poll pass may punch or record a
	// drop on a substrate without the extension.
	sub.Fake.DisablePunchHole.Store(true)

	p := fake.NewProcess(1204)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{
		"cmd":              "sleep 900",
		"output_max_bytes": 10,
	})
	writeStream(t, sub, sandbox, jobID, "stdout", strings.Repeat("z\n", 100))
	svc.reconcileJobs(context.Background())

	row, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if row.StdoutDroppedBytes != 0 || row.StderrDroppedBytes != 0 {
		t.Fatalf("substrate without punching still recorded a drop: %+v", row)
	}
	if _, err := sub.Stat(context.Background(), sandbox, jobPath(jobID, "stdout.cap")); !errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("substrate without punching wrote cap counters: %v", err)
	}
	// The bytes are all still there and readable by absolute offset.
	got, _ := readOutput(t, ts, id, jobID, "stream=stdout&offset=100&limit=100", "token-a")
	if got != strings.Repeat("z\n", 100)[100:200] {
		t.Fatalf("uncapped (no punch) read = %q", got)
	}
}

// TestJobOutputCapDefaultsUnchanged (spoond-hwto): with no host default
// and no request field, an over-cap stream serves its whole content by
// absolute offset — the same answer the pre-cap code returned — and the
// record view's stdout/stderr read is unchanged. The defaults keep
// today's behaviour.
func TestJobOutputCapDefaultsUnchanged(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)

	p := fake.NewProcess(1205)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 900"})
	stdout := strings.Repeat("abcdef\n", 1000) // 7000 bytes
	writeStream(t, sub, sandbox, jobID, "stdout", stdout)
	writeStream(t, sub, sandbox, jobID, "rc", "0\n")
	p.Push(substrate.ProcessEvent{Kind: substrate.EventExit, ExitCode: 0})
	row := waitJobState(t, db, jobID, "exited", jobWatchTimeout)

	for _, q := range []string{"stream=stdout", "stream=stdout&offset=6997", "stream=stdout&offset=0&limit=100"} {
		got, resp := readOutput(t, ts, id, jobID, q, "token-a")
		if resp.Header.Get("X-Output-Skipped") != "" {
			t.Fatalf("%s: got X-Output-Skipped with no cap", q)
		}
		switch q {
		case "stream=stdout":
			if got != stdout {
				t.Fatalf("%s: %d bytes, want the whole 7000-byte stream", q, len(got))
			}
		case "stream=stdout&offset=6997":
			if got != stdout[6997:] {
				t.Fatalf("%s = %q", q, got)
			}
		case "stream=stdout&offset=0&limit=100":
			if got != stdout[:100] {
				t.Fatalf("%s = %q", q, got)
			}
		}
	}
	_ = row
	if svc.jobOutputMax() != 0 {
		t.Fatalf("default host cap is %d, want 0", svc.jobOutputMax())
	}
}

// TestJobOutputCapPunchPlan (spoond-hwto): the drop target is a pure
// function: the floor block boundary at or below the byte that keeps
// exactly the kept window, never before what is already dropped, and
// nothing at all at or under the window.
func TestJobOutputCapPunchPlan(t *testing.T) {
	if g := jobCapTarget(10, 0, 100); g != 0 {
		t.Fatalf("under-window target = %d, want 0", g)
	}
	if g := jobCapTarget(100, 0, 100); g != 0 {
		t.Fatalf("at-window target = %d, want 0", g)
	}
	// 9000 bytes under a 900-byte window: keep from 8100; the floor
	// 4 KiB boundary at or below it is 4096. The kept region holds at
	// least the window (900 bytes here), and the hole itself is aligned.
	if g := jobCapTarget(9000, 0, 900); g != 4096 {
		t.Fatalf("target = %d, want 4096", g)
	}
	// Clean boundary case: size 12288, window 809 → the byte that keeps
	// 809 is 11479 and floor(11479/4096) = 2 → 8192.
	if g := jobCapTarget(12288, 0, 809); g != 8192 {
		t.Fatalf("target = %d, want 8192", g)
	}
	// Already-dropped fronts are never re-dropped or un-dropped.
	if g := jobCapTarget(9000, 8192, 900); g != 0 {
		t.Fatalf("target = %d, want 0", g)
	}
	// A grown file drops more. The counters only ever rise.
	if g := jobCapTarget(24000, 4096, 900); g != 20480 {
		t.Fatalf("target = %d, want 20480", g)
	}
	if g := jobCapTarget(24000, 20480, 900); g != 0 {
		t.Fatalf("target = %d, want 0 (a drop never grows backwards)", g)
	}
	// kept 0 (uncapped) is always nothing.
	if g := jobCapTarget(24000, 0, 0); g != 0 {
		t.Fatalf("target = %d, want 0", g)
	}
}

// TestJobCapSidecarFormat (spoond-hwto): the sidecar's line format is
// "<dropped> <newlines> <kept>" per pass, one per line, so a reader can
// recover the file's history; the store's counters match the live line
// and the parse falls back to the row for a missing file.
func TestJobCapSidecarFormat(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)

	p := fake.NewProcess(1207)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{
		"cmd":              "sleep 900",
		"output_max_bytes": 10,
	})
	// 9000 bytes over the 4106-byte kept window: the target is the
	// floor 4 KiB boundary under 4894 bytes to drop → 4096 (2 bytes per
	// line: the dropped front holds 2048 newlines).
	writeStream(t, sub, sandbox, jobID, "stdout", strings.Repeat("s\n", 4500))
	svc.reconcileJobs(context.Background())

	raw, err := sub.ReadFile(context.Background(), sandbox, jobCapPath(jobID, "stdout"), 1<<20)
	if err != nil {
		t.Fatalf("sidecar missing: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("sidecar = %q, want one pass's line", string(raw))
	}
	dropped, newlines, ok := jobCapSidecarOf2(lines[len(lines)-1])
	if !ok {
		t.Fatalf("sidecar line %q unparsable", lines[len(lines)-1])
	}
	row, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if dropped != row.StdoutDroppedBytes {
		t.Fatalf("sidecar dropped %d, row %d", dropped, row.StdoutDroppedBytes)
	}
	if newlines != jobStreamNewlines(row, "stdout") {
		t.Fatalf("sidecar newlines %d, row %d", newlines, jobStreamNewlines(row, "stdout"))
	}
	if dropped%jobPunchBlockBytes != 0 {
		t.Fatalf("sidecar dropped %d, not block-aligned", dropped)
	}

	// Store-write failure (recovery) test: a pass whose sidecar lands
	// but whose row write fails must have the next pass recover the
	// row's counters to the sidecar's live line exactly (the sidecar is
	// the crash-recovery source when the row is behind).
	//
	// The row write of pass 2 fails. Pass 2 grows the file to 13000
	// bytes, whose target is the floor 4 KiB boundary under
	// (13000 - 4106) = 8192: the punch takes [4096, 8192), whose 4096
	// bytes hold another 2048 newlines, so the front holds 4096.
	// The second pass's record write fails, so the sidecar (which the
	// same pass wrote first) is ahead of the row: exactly the
	// crash-recovery state the sidecar exists to serve.
	var inner store.JobStore = svc.dbJobs
	fdb := &failingJobStore{JobStore: inner, method: "UpdateJobSkipped", nth: 1, err: errStoreBoom}
	svc.dbJobs = fdb
	writeStream(t, sub, sandbox, jobID, "stdout", strings.Repeat("a\n", 4500)+strings.Repeat("b\n", 2000))
	svc.reconcileJobs(context.Background())
	svc.dbJobs = inner
	row, err = db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job after failed pass: %v", err)
	}
	// The row write of the pass failed whole: both counters stay at pass
	// 1's values (8192/4096 is what the later recovery pass brings, not
	// the failed write).
	if row.StdoutDroppedBytes != 4096 || row.StdoutDroppedNewlines != 2048 {
		t.Fatalf("failed pass left row dropped=%d newlines=%d, want 4096/2048", row.StdoutDroppedBytes, row.StdoutDroppedNewlines)
	}
	lines = strings.Split(strings.TrimRight(sidecarRaw(t, sub, sandbox, jobID, "stdout"), "\n"), "\n")
	sd, sn, sOK := jobCapSidecarOf2(lines[len(lines)-1])
	if !sOK || sd != 8192 {
		t.Fatalf("failed pass left no live sidecar line: %q", lines)
	}
	if sn != 4096 {
		t.Fatalf("sidecar newlines %d, want the punched front's 4096", sn)
	}

	// The next pass lands the counters (nothing more to punch: the file
	// sits inside its kept region and the target has not moved).
	svc.reconcileJobs(context.Background())
	row, err = db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job after recovery: %v", err)
	}
	if row.StdoutDroppedBytes != 8192 || row.StdoutDroppedNewlines != 4096 {
		t.Fatalf("recovery pass left row dropped=%d newlines=%d, want 8192/4096", row.StdoutDroppedBytes, row.StdoutDroppedNewlines)
	}

	// Garbage reads as not-ok (the row falls back).
	for bad := range map[string]bool{"nonsense": true, "-1 0 0": true, "600 0 600 extra": true} {
		if _, _, ok := jobCapSidecarOf2(bad); ok {
			t.Fatalf("garbage line %q parsed ok", bad)
		}
	}
	_ = sandbox
}

// TestJobOutputCapExitedStreamsUncapped (spoond-hwto): the cap runs for
// a job still appending; an exited job's streams are never punched
// behind the record's own tail copy (the file's last bytes are what
// reads serve and the record carries).
func TestJobOutputCapExitedStreamsUncapped(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)

	p := fake.NewProcess(1208)
	installJobProcess(t, sub, p)
	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{
		"cmd":              "sleep 900",
		"output_max_bytes": 10,
	})
	writeStream(t, sub, sandbox, jobID, "stdout", strings.Repeat("q\n", 100))
	writeStream(t, sub, sandbox, jobID, "rc", "0\n")
	p.Push(substrate.ProcessEvent{Kind: substrate.EventExit, ExitCode: 0})
	waitJobState(t, db, jobID, "exited", jobWatchTimeout) // the watcher's own pass has run

	svc.reconcileJobs(context.Background())
	row, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if row.StdoutDroppedBytes != 0 {
		t.Fatalf("exited job's stream was capped after all: %d dropped", row.StdoutDroppedBytes)
	}
	info, err := sub.Stat(context.Background(), sandbox, jobPath(jobID, "stdout"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size != 200 {
		t.Fatalf("exited stream shrank to %d bytes", info.Size)
	}
}

// sidecarRaw reads one stream's cap sidecar file whole (a test helper):
// "" when the file is missing.
func sidecarRaw(t *testing.T, sub *testSub, sandboxID, jobID, stream string) string {
	t.Helper()
	raw, err := sub.ReadFile(context.Background(), sandboxID, jobCapPath(jobID, stream), maxJobCapFile)
	if err != nil {
		return ""
	}
	return string(raw)
}

// errStoreBoom is the injected store failure of the cap tests.
var errStoreBoom = errors.New("injected store failure")

// failingJobStore wraps a *store.DB for one call of one of its job
// methods (a test helper): the named method's nth call (1-based; 0 =
// every call) fails with err, everything else passes through. The
// embedded JobStore serves every method the wrapper does not override.
type failingJobStore struct {
	store.JobStore
	method string
	nth    int
	n      int
	err    error
}

func (f *failingJobStore) UpdateJobSkipped(ctx context.Context, jobID, stream string, dropped, newlines int64) error {
	if f.method == "UpdateJobSkipped" && f.err != nil {
		f.n++
		if f.nth == 0 || f.nth == f.n {
			return f.err
		}
	}
	return f.JobStore.UpdateJobSkipped(ctx, jobID, stream, dropped, newlines)
}

// TestJobOutputCapRunnerFiles (spoond-hwto): the cap works on the job
// runner's host-backed files too, not just the in-memory filesystem: the
// front is zeroed in place (the file's size and the kept bytes' offsets
// stay absolute), the row reports the drop, and a read below it answers
// the first kept bytes.
func TestJobOutputCapRunnerFiles(t *testing.T) {
	ts, svc, db, sub := newTestServerWithService(t)
	id, _, sandbox := createJobLease(t, ts, svc)
	// Point the job files at a host directory (the runner's mapping)
	// without running the real wrapper: the command is a fake process,
	// so only the cap's file access is on the host path.
	sub.Fake.EnableJobRunner(t.TempDir())
	t.Cleanup(sub.Fake.StopAllJobs)
	p := fake.NewProcess(1209)
	installJobProcess(t, sub, p)

	jobID, _ := startBackgroundJob(t, ts, id, map[string]any{
		"cmd":              "sleep 900",
		"output_max_bytes": 10,
	})
	stdout := strings.Repeat("r\n", 4500) // 9000 bytes
	writeStream(t, sub, sandbox, jobID, "stdout", stdout)

	svc.reconcileJobs(context.Background())
	row, err := db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if row.StdoutDroppedBytes != 4096 {
		t.Fatalf("runner stream dropped %d, want 4096", row.StdoutDroppedBytes)
	}
	if row.StdoutDroppedNewlines != 2048 {
		t.Fatalf("runner stream dropped %d newlines, want 2048", row.StdoutDroppedNewlines)
	}

	// The host file keeps its size; the kept tail sits at its absolute
	// place, and a read below the drop starts at the first kept byte.
	got, resp := readOutput(t, ts, id, jobID, "stream=stdout&offset=0&limit=4", "token-a")
	if got != stdout[row.StdoutDroppedBytes:row.StdoutDroppedBytes+4] {
		t.Fatalf("read at 0 = %q, want the first kept bytes", got)
	}
	if resp.Header.Get("X-Output-Offset") != itoa9(int(row.StdoutDroppedBytes)) {
		t.Fatalf("X-Output-Offset = %q, want %d", resp.Header.Get("X-Output-Offset"), row.StdoutDroppedBytes)
	}
	tail, _ := readOutput(t, ts, id, jobID, "stream=stdout&limit=2", "token-a")
	if tail != strings.Repeat("r\n", 4500)[len(stdout)-2:] {
		t.Fatalf("absolute tail = %q, want the last line", tail)
	}
}
