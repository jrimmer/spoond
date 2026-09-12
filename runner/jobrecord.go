package runner

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// JobRecord is the durable record of a failed job: which step died, with what
// exit code, and the tail of its output. Forgejo exposes no readable log API,
// so without this a red build reaches a consumer as a bare `failure` with no
// step name, exit code or signal — which is the expensive part of the
// debugging cycle, not the bug itself.
type JobRecord struct {
	JobID    int64        `json:"job_id"`
	Result   string       `json:"result"`
	Error    string       `json:"error,omitempty"`
	Duration string       `json:"duration,omitempty"`
	Steps    []StepRecord `json:"steps"`
}

// StepRecord is one executed step. Index is its 0-based position in the
// workflow, so it lines up with the workflow file the consumer can read.
type StepRecord struct {
	Index  int    `json:"index"`
	Name   string `json:"name"`
	Exit   int    `json:"exit"`
	Result string `json:"result"`
	Logs   int64  `json:"log_rows"`
	Stdout string `json:"stdout_tail,omitempty"`
	Stderr string `json:"stderr_tail,omitempty"`
}

// recordTail caps each captured stream so a record stays readable.
const recordTail = 4096

// maxJobRecords bounds the record directory. Records are written on failure,
// so the cap only bites during a run of failures.
const maxJobRecords = 500

func tailText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// writeJobRecord persists a failed job under dir. Best-effort throughout: a
// record that cannot be written must never change the job's outcome.
func writeJobRecord(dir string, rec *JobRecord) {
	if dir == "" || rec == nil {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("executor: job record dir %s: %v", dir, err)
		return
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return
	}
	path := filepath.Join(dir, fmt.Sprintf("job-%d.json", rec.JobID))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("executor: job record %s: %v", tmp, err)
		return
	}
	_ = os.Rename(tmp, path)
	pruneJobRecords(dir, maxJobRecords)
}

// pruneJobRecords keeps the newest n records.
func pruneJobRecords(dir string, n int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type record struct {
		name string
		mod  time.Time
	}
	var files []record
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "job-") || strings.HasSuffix(name, ".tmp") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, record{name, info.ModTime()})
	}
	if len(files) <= n {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, f := range files[:len(files)-n] {
		_ = os.Remove(filepath.Join(dir, f.name))
	}
}
