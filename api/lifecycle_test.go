package api

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/jrimmer/spoond/v2/identity"
	"github.com/jrimmer/spoond/v2/substrate"
	"github.com/jrimmer/spoond/v2/substrate/fake"
)

// newLifecycleService builds a Service over a fake substrate with the
// egress tests' fixed host service address.
func newLifecycleService(t *testing.T) (*Service, *testSub) {
	t.Helper()
	sub := newTestSub()
	db := newTestDB(t)
	svc := NewService(sub, db, map[string]string{"t": "c"}, ServiceConfig{
		DefaultTTL:    time.Minute,
		MaxTTL:        10 * time.Minute,
		HostGuestAddr: "10.0.0.11",
		HostGuestPort: 8891,
		GuestDNSAddr:  "10.0.0.2",
		HostAPIPort:   8890,
	})
	return svc, sub
}

// TestEgressForEachPolicy pins the exact E2B egress policy per spoond
// network policy (U08): host service and DNS allowances, LAN ranges as
// private allowances, and the allowlist split into domains, public
// CIDRs and private allowances.
func TestEgressForEachPolicy(t *testing.T) {
	svc, _ := newLifecycleService(t)
	hostSvc := substrate.PrivateAllowance{CIDR: "10.0.0.11/32", TCPPorts: []uint32{8891}}
	dns := substrate.PrivateAllowance{CIDR: "10.0.0.2/32", TCPPorts: []uint32{53}}

	lanPrivateWant := make([]substrate.PrivateAllowance, 0, len(lanRanges)+2)
	for _, cidr := range lanRanges {
		lanPrivateWant = append(lanPrivateWant, substrate.PrivateAllowance{CIDR: cidr})
	}
	// lan and internet also name the lease API (8890) explicitly: the
	// fork's host-address guard ignores the any-port LAN ranges for the
	// host's own address.
	lanPrivateWant = append(lanPrivateWant, hostSvc, dns,
		substrate.PrivateAllowance{CIDR: "10.0.0.11/32", TCPPorts: []uint32{8890}})

	t.Run("none", func(t *testing.T) {
		got := svc.egressFor(&Lease{NetPolicy: "none"})
		want := substrate.Egress{DeniedCIDRs: []string{"0.0.0.0/0"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("internet", func(t *testing.T) {
		got := svc.egressFor(&Lease{NetPolicy: "internet"})
		want := substrate.Egress{Private: lanPrivateWant}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("lan", func(t *testing.T) {
		got := svc.egressFor(&Lease{NetPolicy: "lan"})
		want := substrate.Egress{
			DeniedCIDRs: []string{"0.0.0.0/0"},
			Private:     lanPrivateWant,
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("restricted", func(t *testing.T) {
		got := svc.egressFor(&Lease{
			NetPolicy: "restricted",
			NetAllow:  []string{"pg.example.com", "1.2.3.4", "10.0.0.0/8", "172.20.1.0/24"},
		})
		want := substrate.Egress{
			DeniedCIDRs:    []string{"0.0.0.0/0"},
			AllowedCIDRs:   []string{"1.2.3.4/32"},
			AllowedDomains: []string{"pg.example.com"},
			Private: []substrate.PrivateAllowance{
				hostSvc, dns,
				{CIDR: "10.0.0.0/8"},
				{CIDR: "172.20.1.0/24"},
			},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	// The default (empty) policy is restricted with no allowlist.
	t.Run("default is restricted", func(t *testing.T) {
		got := svc.egressFor(&Lease{})
		want := substrate.Egress{
			DeniedCIDRs: []string{"0.0.0.0/0"},
			Private:     []substrate.PrivateAllowance{hostSvc, dns},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
}

// TestAdmit: a create is admitted when the node is healthy and the free
// hugepage memory covers the build's, refused with ErrCapacity otherwise
// (handlers map that to 503).
func TestAdmit(t *testing.T) {
	svc, sub := newLifecycleService(t)
	ctx := context.Background()

	// The fake's default: healthy, plenty of free hugepages.
	if err := svc.admit(ctx, 2048); err != nil {
		t.Fatalf("admit with free memory: %v", err)
	}

	// free = (total - used - reserved) * pageBytes = 1 page = 2 MiB: a
	// 2 MiB build fits exactly, a 3 MiB one does not.
	sub.SetNodeInfo(substrate.NodeInfo{
		Status:            "healthy",
		HugepagesTotal:    10,
		HugepagesUsed:     8,
		HugepagesReserved: 1,
		HugepageSizeBytes: 2 << 20,
	}, nil)
	if err := svc.admit(ctx, 2); err != nil {
		t.Fatalf("admit at exactly free memory: %v", err)
	}
	err := svc.admit(ctx, 3)
	if !errors.Is(err, substrate.ErrCapacity) {
		t.Fatalf("admit over free memory = %v, want ErrCapacity", err)
	}

	// A node that is not healthy refuses everything.
	sub.SetNodeInfo(substrate.NodeInfo{Status: "draining"}, nil)
	if err := svc.admit(ctx, 2); !errors.Is(err, substrate.ErrCapacity) {
		t.Fatalf("admit on draining node = %v, want ErrCapacity", err)
	}
}

// TestForkRollback: with count 3 and the 2nd fork create failing, the
// first created sandbox is deleted and the quota reservations are
// released (the caller can lease again up to the cap).
func TestForkRollback(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ids, _ := identity.NewStore("")
	svc.SetIdentities(ids)
	u, err := ids.AddUser("u", identity.KindAgent, []string{"SHA256:fp-u"}, "u-tok")
	if err != nil {
		t.Fatal(err)
	}
	if err := ids.SetQuota(u.ID, 5, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	src, err := svc.grant(ctx, u.ID, "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant source: %v", err)
	}
	createsBefore := calls(sub.Fake, "Create")
	// The source grant consumed Create #1; the fork's 2nd create is the
	// 3rd overall.
	sub.FailCall("Create", int(createsBefore+2), errors.New("node refused"))

	_, _, err = svc.fork(ctx, u.ID, src.ID, 3, false, time.Minute, "", "")
	if err == nil {
		t.Fatal("fork succeeded despite the failed create")
	}
	// One fork create succeeded before the failure; it must be deleted.
	if got := calls(sub.Fake, "Delete"); got != 1 {
		t.Fatalf("expected 1 rollback delete, got %d (calls %v)", got, sub.Fake.CallLog())
	}
	// The source survives.
	if svc.lookup(u.ID, src.ID) == nil {
		t.Fatal("fork rollback deleted the source lease")
	}
	// Quota released: with max_leases=5 and 1 active lease, 4 more fit
	// and the 5th is refused.
	for i := 0; i < 4; i++ {
		if _, err := svc.grant(ctx, u.ID, "py-base", time.Minute, false, "", nil, "", "", nil); err != nil {
			t.Fatalf("grant %d after rollback: %v", i, err)
		}
	}
	if _, err := svc.grant(ctx, u.ID, "py-base", time.Minute, false, "", nil, "", "", nil); !errors.Is(err, errQuotaExceeded) {
		t.Fatalf("grant past cap = %v, want errQuotaExceeded", err)
	}
}

// TestForkRejectsBadCount: count must be 1..20.
func TestForkRejectsBadCount(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	src, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant source: %v", err)
	}
	for _, n := range []int{0, -1, 21} {
		if _, _, err := svc.fork(ctx, "c", src.ID, n, false, time.Minute, "", ""); !errors.Is(err, errBadForkCount) {
			t.Fatalf("fork count %d = %v, want errBadForkCount", n, err)
		}
	}
}

// TestForkRejectsSuspended: a suspended source has no running sandbox to
// checkpoint.
func TestForkRejectsSuspended(t *testing.T) {
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()
	src, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant source: %v", err)
	}
	if _, err := svc.suspend(ctx, "c", src.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if _, _, err := svc.fork(ctx, "c", src.ID, 2, false, time.Minute, "", ""); !errors.Is(err, errSuspended) {
		t.Fatalf("fork of suspended lease = %v, want errSuspended", err)
	}
}

// TestCloneCopiesSourcePolicy: the clone inherits the source's egress
// policy and exposed ports, keeps the source's image name, and the
// source itself is re-pointed at the checkpoint build (item 18).
func TestCloneCopiesSourcePolicy(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	src, err := svc.grant(ctx, "c", "py-base", time.Minute, true, "lan", []string{"10.0.0.0/8"}, "", "", nil, 9042)
	if err != nil {
		t.Fatalf("grant source: %v", err)
	}
	srcBuild := src.BuildID

	cloned, buildID, err := svc.clone(ctx, "c", src.ID)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if cloned.NetPolicy != "lan" || !reflect.DeepEqual(cloned.NetAllow, []string{"10.0.0.0/8"}) {
		t.Fatalf("clone policy = %q %v, want the source's", cloned.NetPolicy, cloned.NetAllow)
	}
	if !reflect.DeepEqual(cloned.ExposePorts, []int{9042}) {
		t.Fatalf("clone expose ports = %v, want [9042]", cloned.ExposePorts)
	}
	if cloned.Image != src.Image {
		t.Fatalf("clone image = %q, want %q", cloned.Image, src.Image)
	}
	if !cloned.Persistent {
		t.Fatal("clone must be persistent")
	}
	if cloned.ExpiresAt.Sub(time.Now()) > svc.cfg.MaxTTL+time.Second {
		t.Fatalf("clone ttl = %v, want ~maxTTL", cloned.ExpiresAt.Sub(time.Now()))
	}
	if buildID == "" {
		t.Fatal("clone returned no checkpoint build id")
	}
	if got := calls(sub.Fake, "Checkpoint "+src.SandboxID); got != 1 {
		t.Fatalf("expected 1 checkpoint of the source, calls: %v", sub.Fake.CallLog())
	}
	// Item 18: the source keeps running, re-pointed at the checkpoint
	// build, with its checkpoint fields stamped.
	if src.BuildID != buildID || src.LastCheckpointBuildID != buildID {
		t.Fatalf("source build = %q last = %q, want the checkpoint build %q", src.BuildID, src.LastCheckpointBuildID, buildID)
	}
	if src.LastCheckpointAt.IsZero() {
		t.Fatal("source LastCheckpointAt not stamped")
	}
	if srcBuild == "" || src.BuildID == srcBuild {
		t.Fatal("source BuildID should move to the new checkpoint build")
	}
	if !src.live() {
		t.Fatal("source must stay running after a checkpoint")
	}
	// The checkpoint build row was recorded.
	if _, err := db.GetBuild(ctx, buildID); err != nil {
		t.Fatalf("checkpoint build row: %v", err)
	}
}

// TestRestartNonPersistent: a non-persistent restart deletes the sandbox
// and creates a fresh one from the image's current build, keeping the
// lease id.
func TestRestartNonPersistent(t *testing.T) {
	svc, db, sub := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	ctx := context.Background()

	l, err := svc.grant(ctx, "c", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	oldSandbox := l.SandboxID

	if _, err := svc.restart(ctx, "c", l.ID, ""); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if l.ID == "" || svc.lookup("c", l.ID) == nil {
		t.Fatal("restart must keep the lease id")
	}
	if l.SandboxID == oldSandbox {
		t.Fatal("restart must create a new sandbox id")
	}
	if got := calls(sub.Fake, "Delete "+oldSandbox); got != 1 {
		t.Fatalf("expected the old sandbox deleted, calls: %v", sub.Fake.CallLog())
	}
	if !l.live() || l.BuildID == "" {
		t.Fatalf("restarted lease not live: %+v", l)
	}
}

// TestStreamRelayFrames pins the server→client frames (exact shape, no
// trailing newline) and the client→server actions (in, resize, eof,
// stop) of the stream relay.
func TestStreamRelayFrames(t *testing.T) {
	ts, _, db, sub := newTestServerWithService(t)
	seedImage(t, db, "py-base", 2048)
	_, create := doReq(t, "POST", ts.URL+"/api/sandboxes", "token-a", map[string]any{"image": "py-base", "ttl": 300})
	id := create["id"].(string)

	dial := func(t *testing.T) *websocket.Conn {
		t.Helper()
		wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/sandboxes/" + id + "/stream"
		h := map[string][]string{"Authorization": {"Bearer token-a"}}
		ws, _, err := websocket.DefaultDialer.Dial(wsURL, h)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { ws.Close() })
		return ws
	}
	readFrame := func(t *testing.T, ws *websocket.Conn) string {
		t.Helper()
		ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		mt, p, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if mt != websocket.TextMessage {
			t.Fatalf("frame type %d, want text", mt)
		}
		return string(p)
	}
	startProc := func(t *testing.T, ws *websocket.Conn) *fake.FakeProcess {
		t.Helper()
		ws.WriteMessage(websocket.TextMessage, []byte(`{"args":["bash"],"cwd":"/root","pty":true}`))
		frame := readFrame(t, ws)
		var started struct {
			Stream string `json:"stream"`
			PID    uint32 `json:"pid"`
			Pty    bool   `json:"pty"`
		}
		if err := json.Unmarshal([]byte(frame), &started); err != nil {
			t.Fatalf("started frame %q: %v", frame, err)
		}
		if frame != `{"stream":"started","pid":`+
			jsonNumber(t, started.PID)+`,"pty":true}` {
			t.Fatalf("started frame = %q, want the exact started shape", frame)
		}
		proc := sub.Proc(started.PID)
		if proc == nil {
			t.Fatalf("no fake process with pid %d", started.PID)
		}
		return proc
	}

	t.Run("server frames", func(t *testing.T) {
		ws := dial(t)
		proc := startProc(t, ws)
		proc.Push(substrate.ProcessEvent{Kind: substrate.EventStdout, Data: []byte("hello")})
		proc.Push(substrate.ProcessEvent{Kind: substrate.EventStderr, Data: []byte("oops")})
		proc.Push(substrate.ProcessEvent{Kind: substrate.EventPTY, Data: []byte("raw")})
		proc.Push(substrate.ProcessEvent{Kind: substrate.EventExit, ExitCode: 3})
		for _, want := range []string{
			`{"out":"hello"}`,
			`{"out":"oops"}`,
			`{"out":"raw"}`,
			`{"exit_code":3}`,
		} {
			if got := readFrame(t, ws); got != want {
				t.Fatalf("frame = %q, want %q", got, want)
			}
		}
	})

	t.Run("client frames", func(t *testing.T) {
		ws := dial(t)
		proc := startProc(t, ws)
		ws.WriteMessage(websocket.TextMessage, []byte(`{"in":"ls\n"}`))
		ws.WriteMessage(websocket.TextMessage, []byte(`{"resize":{"cols":120,"rows":40}}`))
		ws.WriteMessage(websocket.TextMessage, []byte(`{"action":"eof"}`))
		deadline := time.Now().Add(2 * time.Second)
		for {
			if len(proc.State().Inputs) >= 1 && len(proc.State().Resizes) >= 1 && proc.State().StdinClosed {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("client actions not relayed: inputs=%v resizes=%v stdinClosed=%v",
					proc.State().Inputs, proc.State().Resizes, proc.State().StdinClosed)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if string(proc.State().Inputs[0]) != "ls\n" {
			t.Fatalf("input = %q", proc.State().Inputs[0])
		}
		if !reflect.DeepEqual(proc.State().Resizes, [][2]uint32{{120, 40}}) {
			t.Fatalf("resizes = %v", proc.State().Resizes)
		}
	})

	t.Run("stop sends SIGTERM and relays the exit", func(t *testing.T) {
		ws := dial(t)
		proc := startProc(t, ws)
		go func() {
			time.Sleep(100 * time.Millisecond)
			proc.Push(substrate.ProcessEvent{Kind: substrate.EventExit, ExitCode: 0})
		}()
		ws.WriteMessage(websocket.TextMessage, []byte(`{"action":"stop"}`))
		if got := readFrame(t, ws); got != `{"exit_code":0}` {
			t.Fatalf("frame after stop = %q, want the exit frame", got)
		}
		if !reflect.DeepEqual(proc.State().Signals, []bool{false}) {
			t.Fatalf("signals = %v, want one SIGTERM (false)", proc.State().Signals)
		}
	})

	t.Run("websocket close does not kill the process", func(t *testing.T) {
		ws := dial(t)
		proc := startProc(t, ws)
		ws.Close()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && !proc.State().Closed {
			time.Sleep(10 * time.Millisecond)
		}
		if !proc.State().Closed {
			t.Fatal("process stream not closed after the websocket closed")
		}
		// The sandbox itself must stay (no delete, no signal).
		if len(proc.State().Signals) != 0 {
			t.Fatalf("signals = %v, want none", proc.State().Signals)
		}
	})
}

// jsonNumber renders v as it appears in a Go json.Marshal of a uint32.
func jsonNumber(t *testing.T, v uint32) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
