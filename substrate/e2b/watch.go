package e2b

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	"github.com/jrimmer/spoond/v4/substrate"
	"github.com/jrimmer/spoond/v4/substrate/e2b/gen/envd/filesystem"
)

// Watch opens envd's filesystem.WatchDir server stream and relays its
// events onto the returned channel, which closes when the context is
// cancelled, the stream ends or a terminal error is relayed.
//
// The path is checked with Stat first: a missing path wraps
// substrate.ErrNotFound and a path that is a file wraps
// substrate.ErrNotDir before any stream opens, the same preconditions the
// fake answers with (WatchDir itself reports them as a stream error after
// the open, which a caller could not tell from a later failure).
func (c *Client) Watch(ctx context.Context, sandboxID, path string, recursive bool) (<-chan substrate.WatchEvent, error) {
	info, err := c.Stat(ctx, sandboxID, path)
	if err != nil {
		return nil, fmt.Errorf("e2b: watch %s %s: %w", sandboxID, path, err)
	}
	if !info.IsDir {
		return nil, fmt.Errorf("e2b: watch %s %s: %w", sandboxID, path, substrate.ErrNotDir)
	}
	stream, err := c.envdFilesystem(sandboxID, "").WatchDir(ctx, connect.NewRequest(&filesystem.WatchDirRequest{
		Path:      path,
		Recursive: recursive,
	}))
	if err != nil {
		if serr := c.sandboxListed(ctx, sandboxID); serr != nil {
			return nil, fmt.Errorf("e2b: watch %s %s: %w", sandboxID, path, fmt.Errorf("%w: %w", serr, err))
		}
		return nil, fmt.Errorf("e2b: watch %s %s: %w", sandboxID, path, mapConnectError(err))
	}
	events := make(chan substrate.WatchEvent, 64)
	go readWatchStream(ctx, sandboxID, path, stream, events)
	return events, nil
}

// readWatchStream pumps one WatchDir stream into the events channel and
// closes it. Keepalives are dropped; the Start event becomes WatchReady
// and every filesystem event one WatchChange carrying the path that
// changed (the envd event's name is watch-root-relative).
func readWatchStream(ctx context.Context, sandboxID, path string, stream *connect.ServerStreamForClient[filesystem.WatchDirResponse], events chan<- substrate.WatchEvent) {
	defer close(events)
	for stream.Receive() {
		switch ev := stream.Msg().GetEvent().(type) {
		case *filesystem.WatchDirResponse_Start:
			if !sendWatchEvent(ctx, events, substrate.WatchEvent{Kind: substrate.WatchReady}) {
				return
			}
		case *filesystem.WatchDirResponse_Filesystem:
			// The name is relative to the watched directory.
			changed := relWatchPath(path, ev.Filesystem.GetName())
			if changed == "" {
				continue
			}
			if !sendWatchEvent(ctx, events, substrate.WatchEvent{Kind: substrate.WatchChange, Paths: []string{changed}}) {
				return
			}
		case *filesystem.WatchDirResponse_Keepalive:
			// ignore
		}
	}
	if err := stream.Err(); err != nil && ctx.Err() == nil {
		sendWatchEvent(ctx, events, substrate.WatchEvent{
			Kind: substrate.WatchError,
			Err:  fmt.Sprintf("e2b: watch %s %s: %s", sandboxID, path, err),
		})
	}
}

// sendWatchEvent delivers one event unless the context is done first.
func sendWatchEvent(ctx context.Context, events chan<- substrate.WatchEvent, ev substrate.WatchEvent) bool {
	select {
	case events <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// relWatchPath joins a watch root with an event's root-relative name.
// "/" (the root itself) yields the root unchanged; an empty name yields
// "" (no path to report).
func relWatchPath(root, name string) string {
	if name == "" {
		return ""
	}
	if root == "/" {
		return "/" + name
	}
	return root + "/" + name
}
