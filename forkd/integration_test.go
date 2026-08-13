//go:build integration

package forkd

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestRealControllerLifecycle exercises Client against a real
// forkd-controller with a real firecracker VM, verifying the wire
// contract (list, spawn, ping, exec, branch, kill) end-to-end including
// the guest-agent boot wait that unit tests and the HTTP mock cannot
// cover.
//
// Requirements (a Linux host with a running forkd-controller):
//
//	FORKD_URL            controller base URL (e.g. http://127.0.0.1:8889)
//	FORKD_SNAPSHOT_TAG   a bootable snapshot tag (e.g. py-base)
//	FORKD_TOKEN          optional bearer token
//
// Run with:
//
//	go test -tags integration -run TestRealControllerLifecycle ./forkd/
func TestRealControllerLifecycle(t *testing.T) {
	url := os.Getenv("FORKD_URL")
	tag := os.Getenv("FORKD_SNAPSHOT_TAG")
	if url == "" || tag == "" {
		t.Skip("FORKD_URL and FORKD_SNAPSHOT_TAG required for real-controller test")
	}
	c := NewClient(url, os.Getenv("FORKD_TOKEN"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	snaps, err := c.ListSnapshots(ctx)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if !snapshotInList(snaps, tag) {
		t.Fatalf("snapshot %q not in controller list: %+v", tag, snaps)
	}

	sbs, err := c.Spawn(ctx, tag, 1, true, 0)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if len(sbs) == 0 {
		t.Fatal("Spawn returned no sandboxes")
	}
	id := sbs[0].ID
	t.Logf("spawned %s (guest %s)", id, sbs[0].GuestAddr)
	t.Cleanup(func() { _ = c.Kill(context.Background(), id) })

	waitForPing(t, c, ctx, id)

	res, err := c.Exec(ctx, id, []string{"echo", "hello-forkd"}, 30)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 || res.Stdout == "" {
		t.Fatalf("unexpected exec result: %+v", res)
	}
	t.Logf("exec stdout=%q exit=%d", res.Stdout, res.ExitCode)

	// Branch (checkpoint): the controller pauses the source VM briefly.
	// The snapshot is left behind — forkd has no snapshot-delete endpoint
	// yet, and retention is out of scope here.
	branchTag := "integration-" + tag + "-" + time.Now().Format("20060102-150405")
	gotTag, err := c.Branch(ctx, id, branchTag)
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}
	if gotTag != branchTag {
		t.Fatalf("Branch returned tag %q, want %q", gotTag, branchTag)
	}
	if ok, err := c.SnapshotExists(ctx, branchTag); err != nil || !ok {
		t.Fatalf("SnapshotExists(%q) = %v, %v; want true", branchTag, ok, err)
	}

	if err := c.Kill(ctx, id); err != nil {
		t.Fatalf("Kill: %v", err)
	}
}

func snapshotInList(snaps []SnapshotInfo, tag string) bool {
	for _, s := range snaps {
		if s.Tag == tag {
			return true
		}
	}
	return false
}

// waitForPing polls the guest agent until it responds or the deadline
// elapses (firecracker VMs need several seconds to boot PID-1 and start
// the agent on :8888).
func waitForPing(t *testing.T, c *Client, ctx context.Context, id string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			t.Fatalf("context done waiting for guest agent: %v", ctx.Err())
		}
		if err := c.Ping(ctx, id); err == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("guest agent did not respond to ping within 60s")
}
