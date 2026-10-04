package api

import "sync"

// gcTracker remembers the most recent snapshot GC pass's error for the
// notify checks (gc.failed, 2.2 #117). The backend's GC loop records
// every pass's outcome; a pass that has not run yet is not a failure.
type gcTracker struct {
	mu    sync.Mutex
	last  error
	never bool
}

func newGCTracker() *gcTracker { return &gcTracker{never: true} }

// Set records one pass's outcome.
func (g *gcTracker) Set(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.last, g.never = err, false
}

// Last reports the most recent pass's error: nil when the last pass
// succeeded, and nil before the first pass (an unrun GC is not a
// failure).
func (g *gcTracker) Last() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.never {
		return nil
	}
	return g.last
}
