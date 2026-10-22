package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/jrimmer/spoond/v4/substrate"
	"github.com/jrimmer/spoond/v4/substrate/fake"
)

// testClock is a manually advanced service clock.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// checkpointTestServer builds a server whose service clock the test
// drives (the periodic pass's deferral cap counts from it).
func checkpointTestServer(t *testing.T) (*httptest.Server, *Service, *testSub, *testClock) {
	t.Helper()
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	clock := &testClock{now: time.Now()}
	svc.now = clock.Now
	srv := NewServer(svc, NewImageRegistry(db))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, svc, sub, clock
}

// checkpointLeaseAt creates a persistent lease with its own checkpoint
// interval and returns it, aged so it is due on the next pass.
func checkpointLeaseAt(t *testing.T, ts *httptest.Server, svc *Service, interval int64) *Lease {
	t.Helper()
	_, body := doReq(t, "POST", ts.URL+"/api/leases", "token-a", map[string]any{
		"image": "py-base", "ttl": 300, "persistent": true,
		"checkpoint_interval": interval,
	})
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("create lease: no id in %v", body)
	}
	l := svc.lookupAny(id)
	if l == nil {
		t.Fatalf("lease %s not in service", id)
	}
	svc.store.mu.Lock()
	l.LastCheckpointAt = svc.now().Add(-2 * time.Duration(interval) * time.Second)
	svc.saveLeaseLocked(l)
	svc.store.mu.Unlock()
	return l
}

// TestCheckpointDeferredForOpenStream: a due lease with an open stream
// is not checkpointed; the first pass after the stream closes takes it.
func TestCheckpointDeferredForOpenStream(t *testing.T) {
	ts, svc, sub, _ := checkpointTestServer(t)
	ctx := context.Background()
	l := checkpointLeaseAt(t, ts, svc, 60)

	ws := dialStream(t, ts, l.ID, bearerHeader("token-a", ""))
	proc := startStream(t, ws, sub)
	waitOpenWork(t, svc, l.ID, 1)

	svc.checkpointIdleLeases(ctx)
	if got := calls(sub.Fake, "Checkpoint "+l.SandboxID); got != 0 {
		t.Fatalf("due lease with an open stream checkpointed %d times, want 0", got)
	}

	// The stream closes; the next pass checkpoints it.
	proc.Push(substrate.ProcessEvent{Kind: substrate.EventExit, ExitCode: 0})
	ws.Close()
	waitOpenWork(t, svc, l.ID, 0)
	svc.checkpointIdleLeases(ctx)
	if got := calls(sub.Fake, "Checkpoint "+l.SandboxID); got != 1 {
		t.Fatalf("lease checkpointed %d times after its stream closed, want 1 (calls %v)", got, sub.Fake.CallLog())
	}
	svc.store.mu.Lock()
	deferredSince := l.checkpointDeferredSince
	svc.store.mu.Unlock()
	if !deferredSince.IsZero() {
		t.Fatalf("checkpointDeferredSince = %v after the checkpoint, want cleared", deferredSince)
	}
}

// TestCheckpointDeferredCapForces: a stream that stays open past one
// effective interval is checkpointed anyway, once.
func TestCheckpointDeferredCapForces(t *testing.T) {
	ts, svc, sub, clock := checkpointTestServer(t)
	ctx := context.Background()
	var logBuf strings.Builder
	svc.log = log.New(&logBuf, "", 0)
	l := checkpointLeaseAt(t, ts, svc, 60)

	ws := dialStream(t, ts, l.ID, bearerHeader("token-a", ""))
	proc := startStream(t, ws, sub)
	waitOpenWork(t, svc, l.ID, 1)

	svc.checkpointIdleLeases(ctx)
	if got := calls(sub.Fake, "Checkpoint "+l.SandboxID); got != 0 {
		t.Fatalf("first pass checkpointed the lease %d times, want 0", got)
	}
	// Wait less than one interval since the lease first became due: still
	// deferred, and the progress of time alone lets it stay due.
	clock.Advance(30 * time.Second)
	svc.store.mu.Lock()
	l.LastCheckpointAt = svc.now().Add(-2 * time.Minute)
	svc.saveLeaseLocked(l)
	svc.store.mu.Unlock()
	svc.checkpointIdleLeases(ctx)
	if got := calls(sub.Fake, "Checkpoint "+l.SandboxID); got != 0 {
		t.Fatalf("lease checkpointed inside its deferral %d times, want 0", got)
	}
	// Past one full interval since it first became due: the cap forces it.
	clock.Advance(31 * time.Second)
	svc.store.mu.Lock()
	l.LastCheckpointAt = svc.now().Add(-2 * time.Minute)
	svc.saveLeaseLocked(l)
	svc.store.mu.Unlock()
	svc.checkpointIdleLeases(ctx)
	if got := calls(sub.Fake, "Checkpoint "+l.SandboxID); got != 1 {
		t.Fatalf("lease checkpointed %d times after the deferral cap, want 1 (calls %v)", got, sub.Fake.CallLog())
	}
	proc.Push(substrate.ProcessEvent{Kind: substrate.EventExit, ExitCode: 0})
	out := logBuf.String()
	if got := strings.Count(out, "deferred while it has open work"); got != 1 {
		t.Fatalf("deferral logged %d times, want once per deferral: %q", got, out)
	}
	if got := strings.Count(out, "cap forces the checkpoint"); got != 1 {
		t.Fatalf("cap logged %d times, want once per deferral: %q", got, out)
	}
}

// TestCheckpointOnDemandDoesNotWait: only the periodic pass waits for
// open work; a checkpoint asked for through the API runs right away.
func TestCheckpointOnDemandDoesNotWait(t *testing.T) {
	ts, svc, sub, _ := checkpointTestServer(t)
	l := checkpointLeaseAt(t, ts, svc, 60)

	ws := dialStream(t, ts, l.ID, bearerHeader("token-a", ""))
	proc := startStream(t, ws, sub)
	waitOpenWork(t, svc, l.ID, 1)

	resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+l.ID+"/checkpoint", "token-a", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("on-demand checkpoint = %d (%v), want 200", resp.StatusCode, body)
	}
	if got := calls(sub.Fake, "Checkpoint "+l.SandboxID); got != 1 {
		t.Fatalf("on-demand checkpoint called %d times, want 1", got)
	}
	proc.Push(substrate.ProcessEvent{Kind: substrate.EventExit, ExitCode: 0})
}

// TestCheckpointOpenWorkBackgroundJobDoesNotDefer: a background job is
// not open work; a checkpoint pauses it without ending it, so the due
// lease is checkpointed on the first pass.
func TestCheckpointOpenWorkBackgroundJobDoesNotDefer(t *testing.T) {
	ts, svc, sub, _ := checkpointTestServer(t)
	ctx := context.Background()
	id, l, _ := createJobLease(t, ts, svc)
	jobProc := fake.NewProcess(2003)
	installJobProcess(t, sub, jobProc)
	svc.store.mu.Lock()
	l.CheckpointInterval = 60
	l.LastCheckpointAt = svc.now().Add(-2 * time.Minute)
	svc.saveLeaseLocked(l)
	svc.store.mu.Unlock()
	startBackgroundJob(t, ts, id, map[string]any{"cmd": "sleep 600"})
	if !svc.hasRunningJob(l.ID) {
		t.Fatal("background job not recorded as running")
	}
	svc.checkpointIdleLeases(ctx)
	if got := calls(sub.Fake, "Checkpoint "+l.SandboxID); got != 1 {
		t.Fatalf("lease with only a background job checkpointed %d times, want 1 (calls %v)", got, sub.Fake.CallLog())
	}
	jobProc.Push(substrate.ProcessEvent{Kind: substrate.EventExit, ExitCode: 0})
}

// TestCheckpointOpenWorkSyncExecDefers: a synchronous exec in flight
// defers the periodic checkpoint, and its end lets the next pass take
// it.
func TestCheckpointOpenWorkSyncExecDefers(t *testing.T) {
	ts, svc, sub, _ := checkpointTestServer(t)
	ctx := context.Background()
	l := checkpointLeaseAt(t, ts, svc, 60)

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	sub.SetExecHandler(func(string, substrate.ExecRequest) substrate.ExecResult {
		once.Do(func() { close(entered) })
		<-release
		return substrate.ExecResult{Stdout: "ok\n"}
	})
	t.Cleanup(func() { sub.SetExecHandler(nil) })

	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, body := doReq(t, "POST", ts.URL+"/api/leases/"+l.ID+"/exec", "token-a", map[string]any{"cmd": "sleep 1"})
		if resp.StatusCode != http.StatusOK {
			t.Errorf("exec = %d (%v), want 200", resp.StatusCode, body)
		}
	}()
	<-entered
	waitOpenWork(t, svc, l.ID, 1)

	svc.checkpointIdleLeases(ctx)
	if got := calls(sub.Fake, "Checkpoint "+l.SandboxID); got != 0 {
		t.Fatalf("lease with an in-flight sync exec checkpointed %d times, want 0", got)
	}
	close(release)
	<-done
	waitOpenWork(t, svc, l.ID, 0)
	svc.checkpointIdleLeases(ctx)
	if got := calls(sub.Fake, "Checkpoint "+l.SandboxID); got != 1 {
		t.Fatalf("lease checkpointed %d times after its exec ended, want 1 (calls %v)", got, sub.Fake.CallLog())
	}
}

// startStream sends the stream's first frame and returns the fake
// process the fake substrate started for it.
func startStream(t *testing.T, ws *websocket.Conn, sub *testSub) *fake.FakeProcess {
	t.Helper()
	if err := ws.WriteMessage(websocket.TextMessage, []byte(`{"args":["bash"],"pty":false}`)); err != nil {
		t.Fatalf("write first frame: %v", err)
	}
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, payload, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read started frame: %v", err)
	}
	var started struct {
		PID uint32 `json:"pid"`
	}
	if err := json.Unmarshal(payload, &started); err != nil {
		t.Fatalf("started frame %q: %v", payload, err)
	}
	proc := sub.Proc(started.PID)
	if proc == nil {
		t.Fatalf("no fake process with pid %d", started.PID)
	}
	return proc
}

// waitOpenWork polls the lease's open-work counter.
func waitOpenWork(t *testing.T, svc *Service, id string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := svc.openWorkCount(id); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lease %s open work never reached %d (last %d)", id, want, svc.openWorkCount(id))
		}
		time.Sleep(5 * time.Millisecond)
	}
}
