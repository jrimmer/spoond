package fake

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/jrimmer/spoond/v3/substrate"
)

// watchBuffer is how many events one watcher buffers before a slow
// reader starts losing them, like fsnotify's own queue.
const watchBuffer = 128

// fakeWatcher is one Watch registration against a per-sandbox in-memory
// filesystem. Events are queued on events (buffered); the channel closes
// when the watch's context ends, when the sandbox goes (deleted, paused)
// or when the reader falls too far behind.
type fakeWatcher struct {
	mu     sync.Mutex
	events chan substrate.WatchEvent
	ended  bool // its channel is closed; no further sends
}

// Watch registers a change watcher over a sandbox directory. A missing
// path wraps substrate.ErrNotFound; a path that is a file wraps
// substrate.ErrNotDir. Changes land on the channel: WriteFile, MakeDir,
// Rename and Remove (and Delete/Pause of the sandbox) notify every
// matching watcher under the same lock that mutated the filesystem.
func (f *Fake) Watch(ctx context.Context, sandboxID, path string, recursive bool) (<-chan substrate.WatchEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Watch", sandboxID); err != nil {
		return nil, err
	}
	fs := f.fileFS(sandboxID)
	if fs == nil {
		return nil, fmt.Errorf("fake: watch %s %s: %w", sandboxID, path, substrate.ErrNotFound)
	}
	name := cleanPath(path)
	if _, ok := fs.dirs[name]; !ok {
		if _, ok := fs.files[name]; ok {
			return nil, fmt.Errorf("fake: watch %s %s: %w", sandboxID, path, substrate.ErrNotDir)
		}
		return nil, fmt.Errorf("fake: watch %s %s: %w", sandboxID, path, substrate.ErrNotFound)
	}
	w := &fakeWatcher{events: make(chan substrate.WatchEvent, watchBuffer)}
	f.nextWatcherID++
	id := f.nextWatcherID
	f.watchers[id] = watcherEntry{sandboxID: sandboxID, dir: name, recursive: recursive, w: w}
	f.watchersByID[id] = w
	ready := func() {
		select {
		case w.events <- substrate.WatchEvent{Kind: substrate.WatchReady}:
		case <-ctx.Done():
		}
	}
	ready()
	go func() {
		<-ctx.Done()
		f.endWatcher(id)
	}()
	return w.events, nil
}

// endWatcher closes a watcher's channel once. Called by the context
// watcher goroutine when the watch's context ends.
func (f *Fake) endWatcher(id int) {
	f.mu.Lock()
	w := f.watchersByID[id]
	delete(f.watchers, id)
	delete(f.watchersByID, id)
	f.mu.Unlock()
	if w == nil {
		return
	}
	w.mu.Lock()
	done := w.ended
	w.ended = true
	w.mu.Unlock()
	if !done {
		close(w.events)
	}
}

// notifyLocked fans a filesystem change out to the sandbox's matching
// watchers. Call with f.mu held (the mutation holds it).
func (f *Fake) notifyLocked(sandboxID string, changed ...string) {
	for id, entry := range f.watchers {
		if entry.sandboxID != sandboxID {
			continue
		}
		var hit []string
		for _, p := range changed {
			if p != "" && watcherCovers(entry.dir, p, entry.recursive) {
				hit = append(hit, p)
			}
		}
		if len(hit) == 0 {
			continue
		}
		w := entry.w
		// The send is non-blocking, so holding mu across it (and across
		// the close in the terminal paths) keeps a send off a closed
		// channel.
		w.mu.Lock()
		if w.ended {
			w.mu.Unlock()
			continue
		}
		select {
		case w.events <- substrate.WatchEvent{Kind: substrate.WatchChange, Paths: hit}:
			w.mu.Unlock()
		default:
			w.mu.Unlock()
			// A slow reader loses events, like fsnotify's queue.
			f.endSlowWatcher(id)
		}
	}
}

// watcherCovers reports whether a change at p is inside a watcher's
// subtree. A non-recursive watcher sees its direct children only.
func watcherCovers(dir, p string, recursive bool) bool {
	if dir == p {
		// A change to the watched directory itself still reports.
		return true
	}
	if recursive {
		return strings.HasPrefix(p, dir+"/")
	}
	return strings.HasPrefix(p, dir+"/") && !strings.Contains(strings.TrimPrefix(p, dir+"/"), "/")
}

// endSlowWatcher drops a watcher whose reader fell behind: no events are
// lost silently, the channel closes (the reader sees the stream end,
// which the API converts to an error event). id's entry is deleted.
func (f *Fake) endSlowWatcher(id int) {
	// Deleting under f.mu here is safe: notifyLocked calls this with
	// f.mu held, and map deletion during range is allowed in Go.
	w := f.watchersByID[id]
	delete(f.watchers, id)
	delete(f.watchersByID, id)
	if w == nil {
		return
	}
	w.mu.Lock()
	if !w.ended {
		w.ended = true
		close(w.events)
	}
	w.mu.Unlock()
}

// endSandboxWatchers closes every watcher of one sandbox: the files the
// watchers read go with the sandbox (deleted or paused).
func (f *Fake) endSandboxWatchersLocked(sandboxID string) {
	for id, entry := range f.watchers {
		if entry.sandboxID != sandboxID {
			continue
		}
		w := entry.w
		delete(f.watchers, id)
		delete(f.watchersByID, id)
		w.mu.Lock()
		if !w.ended {
			w.ended = true
			close(w.events)
		}
		w.mu.Unlock()
	}
}

// watcherEntry is one live watcher: which sandbox and subtree it covers.
type watcherEntry struct {
	sandboxID string
	dir       string // clean absolute subtree root, "" = the root
	recursive bool
	w         *fakeWatcher
}

// NotifyChange queues a change event to the sandbox's watchers from
// outside the fake (tests that drive the memFS directly). It takes the
// fake's lock like the notifying file operations do.
func (f *Fake) NotifyChange(sandboxID string, paths ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notifyLocked(sandboxID, paths...)
}

// WatcherCount returns how many watchers are live.
func (f *Fake) WatcherCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.watchers)
}
