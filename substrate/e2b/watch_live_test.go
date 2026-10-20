//go:build e2blive

// Live watch check against a real envd. Two ways to point it at one:
//
//	SPOOND_LIVE_ENVD_URL=http://127.0.0.1:49983 go test -tags e2blive -count=1 -run TestLiveWatchLocalEnvd ./substrate/e2b/
//
// for an envd listening locally (the lease's own, say), or the E2B_*
// environment plus E2B_LIVE_SANDBOX for an existing sandbox on the host.
package e2b

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v3/substrate"
)

// liveWatchDir names a fresh guest directory per run, so a check never
// confuses an earlier run's directory with its own.
func liveWatchDir(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("/tmp/spoond-live-watch-%d", time.Now().UnixNano())
}

// liveWatch waits for the ready event, writes a file through the
// substrate and waits for its change event.
func liveWatch(t *testing.T, c *Client, sandboxID, dir, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	events, err := c.Watch(ctx, sandboxID, dir, false)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	select {
	case ev, ok := <-events:
		if !ok || ev.Kind != substrate.WatchReady {
			t.Fatalf("first event = %+v (open %v), want ready", ev, ok)
		}
	case <-ctx.Done():
		t.Fatal("no ready event within 30s")
	}
	if err := c.WriteFile(ctx, sandboxID, path, []byte("watch live check\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("the watch ended before the change event")
			}
			switch ev.Kind {
			case substrate.WatchChange:
				for _, p := range ev.Paths {
					if p == path {
						return
					}
				}
				t.Logf("change for another path: %v", ev.Paths)
			case substrate.WatchError:
				t.Fatalf("watch error: %s", ev.Err)
			}
		case <-ctx.Done():
			t.Fatal("no change event within 30s")
		}
	}
}

// TestLiveWatchLocalEnvd watches a directory on a locally listening envd
// and pins that a file write through the substrate produces a change
// event. envd on 127.0.0.1:49984 with no access token:
//
//	envd -isnotfc -no-cgroups -port 49984 &
//	SPOOND_LIVE_ENVD_URL=http://127.0.0.1:49984 go test -tags e2blive -count=1 -run TestLiveWatchLocalEnvd ./substrate/e2b/
func TestLiveWatchLocalEnvd(t *testing.T) {
	url := os.Getenv("SPOOND_LIVE_ENVD_URL")
	if url == "" {
		t.Skip("SPOOND_LIVE_ENVD_URL unset")
	}
	c, err := New(Config{
		GRPCAddr:  "127.0.0.1:1", // never dialed: the watch and files go over envd HTTP
		ProxyURL:  url,
		TokenSeed: []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const sandboxID = "i0123456789abcdefghij"
	dir := liveWatchDir(t)
	if err := c.MakeDir(context.Background(), sandboxID, dir, 0o755); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	liveWatch(t, c, sandboxID, dir, dir+"/f.txt")
}

// TestLiveWatchSandbox watches a directory in an existing E2B sandbox:
// the full client against the orchestrator proxy, a file written through
// its own HTTP upload, and the change event envd's inotify watcher
// reports.
func TestLiveWatchSandbox(t *testing.T) {
	id := os.Getenv("E2B_LIVE_SANDBOX")
	if id == "" {
		t.Skip("E2B_LIVE_SANDBOX unset")
	}
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dir := liveWatchDir(t)
	if err := c.MakeDir(context.Background(), id, dir, 0o755); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	liveWatch(t, c, id, dir, dir+"/f.txt")
}
