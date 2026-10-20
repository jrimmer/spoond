package fake

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v3/substrate"
)

// watchCtx returns a watch context the test can cancel, with some slack
// past the assertions so a slow machine never kills a pass mid-check.
func watchCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithCancel(t.Context())
}

// waitWatchEvent reads one event with a deadline.
func waitWatchEvent(t *testing.T, events <-chan substrate.WatchEvent) substrate.WatchEvent {
	t.Helper()
	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("watch channel closed before the expected event")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("watch event did not arrive within 5s")
		return substrate.WatchEvent{}
	}
}

// waitWatchClosed waits for the channel to close.
func waitWatchClosed(t *testing.T, events <-chan substrate.WatchEvent) {
	t.Helper()
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("watch channel carried an unexpected event before closing")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch channel did not close within 5s")
	}
}

func TestWatchReadyThenChange(t *testing.T) {
	f, id := newFileSandbox(t)
	ctx, cancel := watchCtx(t)
	defer cancel()
	if err := f.MakeDir(context.Background(), id, "/work", 0o755); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}

	events, err := f.Watch(ctx, id, "/work", true)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if ev := waitWatchEvent(t, events); ev.Kind != substrate.WatchReady {
		t.Fatalf("first event = %+v, want ready", ev)
	}
	if err := f.WriteFile(context.Background(), id, "/work/f.txt", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	ev := waitWatchEvent(t, events)
	if ev.Kind != substrate.WatchChange || len(ev.Paths) != 1 || ev.Paths[0] != "/work/f.txt" {
		t.Fatalf("change event = %+v, want one /work/f.txt", ev)
	}

	// A write outside the subtree is not reported.
	if err := f.WriteFile(context.Background(), id, "/elsewhere/f.txt", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile outside: %v", err)
	}
	select {
	case ev := <-events:
		t.Fatalf("unexpected event %+v for an outside write", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWatchNonRecursiveOnlyTopLevel(t *testing.T) {
	f, id := newFileSandbox(t)
	ctx, cancel := watchCtx(t)
	defer cancel()
	if err := f.MakeDir(context.Background(), id, "/work", 0o755); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}

	events, err := f.Watch(ctx, id, "/work", false)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	waitWatchEvent(t, events) // ready
	if err := f.WriteFile(context.Background(), id, "/work/deep/f.txt", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile deep: %v", err)
	}
	select {
	case ev := <-events:
		t.Fatalf("non-recursive watcher saw a deep change %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
	if err := f.WriteFile(context.Background(), id, "/work/top.txt", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile top: %v", err)
	}
	ev := waitWatchEvent(t, events)
	if ev.Kind != substrate.WatchChange || len(ev.Paths) != 1 || ev.Paths[0] != "/work/top.txt" {
		t.Fatalf("change event = %+v, want one /work/top.txt", ev)
	}
}

func TestWatchErrors(t *testing.T) {
	f, id := newFileSandbox(t)
	if err := f.WriteFile(context.Background(), id, "/f", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// A missing path is ErrNotFound.
	if _, err := f.Watch(context.Background(), id, "/no/such", true); !errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("Watch missing: %v", err)
	}
	// A file is ErrNotDir.
	if _, err := f.Watch(context.Background(), id, "/f", true); !errors.Is(err, substrate.ErrNotDir) {
		t.Fatalf("Watch a file: %v", err)
	}
	// An unknown sandbox is ErrNotFound.
	if _, err := f.Watch(context.Background(), "nosuch", "/f", true); !errors.Is(err, substrate.ErrNotFound) {
		t.Fatalf("Watch unknown sandbox: %v", err)
	}
}

func TestWatchEndsWhenSandboxDeleted(t *testing.T) {
	f, id := newFileSandbox(t)
	ctx, cancel := watchCtx(t)
	defer cancel()
	if err := f.MakeDir(context.Background(), id, "/work", 0o755); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}

	events, err := f.Watch(ctx, id, "/work", false)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	waitWatchEvent(t, events)
	if err := f.Delete(context.Background(), id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	waitWatchClosed(t, events)
}

func TestWatchEndsWhenSandboxPaused(t *testing.T) {
	f, id := newFileSandbox(t)
	if err := f.WriteFile(context.Background(), id, "/work/seed.txt", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	ctx, cancel := watchCtx(t)
	defer cancel()
	if err := f.MakeDir(context.Background(), id, "/work", 0o755); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}

	events, err := f.Watch(ctx, id, "/work", false)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	waitWatchEvent(t, events)
	if _, _, err := f.Pause(context.Background(), id, "tpl"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	waitWatchClosed(t, events)
}

func TestWatchEndsWhenContextCancelled(t *testing.T) {
	f, id := newFileSandbox(t)
	if err := f.MakeDir(context.Background(), id, "/work", 0o755); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	ctx, cancel := watchCtx(t)

	events, err := f.Watch(ctx, id, "/work", false)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	waitWatchEvent(t, events)
	cancel()
	waitWatchClosed(t, events)
	if n := f.WatcherCount(); n != 0 {
		t.Fatalf("WatcherCount after cancel = %d, want 0", n)
	}
}

func TestWatchEventsCoalesceInAPI(t *testing.T) {
	// The fake itself reports every change as one event; the coalescing
	// the SSE stream does is pinned at the API layer. Here: two writes
	// become two events in order, each with its own path.
	f, id := newFileSandbox(t)
	ctx, cancel := watchCtx(t)
	defer cancel()
	if err := f.MakeDir(context.Background(), id, "/work", 0o755); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}

	events, err := f.Watch(ctx, id, "/work", false)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	waitWatchEvent(t, events)
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := f.WriteFile(context.Background(), id, "/work/"+name, []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	if ev := waitWatchEvent(t, events); len(ev.Paths) != 1 || ev.Paths[0] != "/work/a.txt" {
		t.Fatalf("first change = %+v, want /work/a.txt", ev)
	}
	if ev := waitWatchEvent(t, events); len(ev.Paths) != 1 || ev.Paths[0] != "/work/b.txt" {
		t.Fatalf("second change = %+v, want /work/b.txt", ev)
	}
}

func TestWatchRenameAndRemove(t *testing.T) {
	f, id := newFileSandbox(t)
	if err := f.WriteFile(context.Background(), id, "/work/old", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	ctx, cancel := watchCtx(t)
	defer cancel()
	if err := f.MakeDir(context.Background(), id, "/work", 0o755); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}

	events, err := f.Watch(ctx, id, "/work", false)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	waitWatchEvent(t, events)
	if err := f.Rename(context.Background(), id, "/work/old", "/work/new"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	ev := waitWatchEvent(t, events)
	if ev.Kind != substrate.WatchChange || len(ev.Paths) != 2 {
		t.Fatalf("rename event = %+v, want two paths", ev)
	}
	if err := f.Remove(context.Background(), id, "/work/new", false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	ev = waitWatchEvent(t, events)
	if ev.Kind != substrate.WatchChange || len(ev.Paths) != 1 || ev.Paths[0] != "/work/new" {
		t.Fatalf("remove event = %+v, want one /work/new", ev)
	}
}

// TestWatchMakeDirNotifies: a new directory inside the watched subtree
// is a change too (a watcher sees the tree grow).
func TestWatchMakeDirNotifies(t *testing.T) {
	f, id := newFileSandbox(t)
	if err := f.MakeDir(context.Background(), id, "/work", 0o755); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	ctx, cancel := watchCtx(t)
	defer cancel()
	events, err := f.Watch(ctx, id, "/work", true)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	waitWatchEvent(t, events)
	if err := f.MakeDir(context.Background(), id, "/work/sub", 0o755); err != nil {
		t.Fatalf("MakeDir sub: %v", err)
	}
	ev := waitWatchEvent(t, events)
	if ev.Kind != substrate.WatchChange || len(ev.Paths) != 1 || ev.Paths[0] != "/work/sub" {
		t.Fatalf("change event = %+v, want one /work/sub", ev)
	}
}
