package notify

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// stateFile mirrors recent delivery failures to disk so spoond doctor
// (a separate process) can report them. Best effort: a broken state
// file never disturbs delivery.
type stateFile struct {
	mu   sync.Mutex
	path string
}

func (sf *stateFile) record(f Failure, now time.Time) {
	if sf == nil || sf.path == "" {
		return
	}
	sf.mu.Lock()
	defer sf.mu.Unlock()
	fails, _ := LoadFailures(sf.path, now)
	fails = append(fails, f)
	fails = pruneFailures(fails, now)
	b, err := json.Marshal(fails)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(sf.path), 0o700); err != nil {
		return
	}
	tmp := sf.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, sf.path)
}

// LoadFailures reads a notifier state file and returns the failures
// of the last 24 hours (spoond doctor's "notify" check). A missing
// file is not an error: no file means the notifier has never dropped a
// delivery, which is the common healthy case.
func LoadFailures(path string, now time.Time) ([]Failure, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var fails []Failure
	if err := json.Unmarshal(b, &fails); err != nil {
		return nil, err
	}
	return pruneFailures(fails, now), nil
}

// pruneFailures keeps only failures within the 24-hour horizon, at
// most maxFailures of them, oldest first.
func pruneFailures(fails []Failure, now time.Time) []Failure {
	out := fails[:0:0]
	for _, f := range fails {
		if f.At.Add(failureHorizon).After(now) {
			out = append(out, f)
		}
	}
	if len(out) > maxFailures {
		out = out[len(out)-maxFailures:]
	}
	return out
}

// TestHook lets tests drive passes by hand and observe the queue.
type TestHook struct {
	n *Notifier
}

// RunChecksNow runs one pass of every registered check into the
// notifier's queue.
func (t *TestHook) RunChecksNow(ctx context.Context) { t.n.runChecks(ctx) }

// QueueLen reports the dispatcher queue depth (test observation).
func (t *TestHook) QueueLen() int { return len(t.n.queue) }
