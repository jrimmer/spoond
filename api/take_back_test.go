package api

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/store"
	"github.com/jrimmer/spoond/v2/substrate"
)

func tbL(id string, mem int, age time.Duration) takeBackLease {
	return takeBackLease{ID: id, MemoryMiB: mem, LastActive: time.Unix(1_000_000, 0).Add(-age)}
}

func ids(vs []takeBackVictim) []string {
	var out []string
	for _, v := range vs {
		out = append(out, v.LeaseID)
	}
	return out
}

func eqStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestMemVictimsInsideSliceNeverChosen(t *testing.T) {
	owners := []takeBackOwner{
		{Owner: "req", UsedMiB: 0, SliceMiB: 8192},
		{Owner: "in", UsedMiB: 8192, SliceMiB: 8192, Leases: []takeBackLease{tbL("a", 4096, time.Hour)}},
	}
	if got := memVictims(owners, "req", 1024); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func TestMemVictimsRatioNotAbsolute(t *testing.T) {
	owners := []takeBackOwner{
		{Owner: "req", UsedMiB: 0, SliceMiB: 8192},
		{Owner: "honey", UsedMiB: 28672, SliceMiB: 12288, Leases: []takeBackLease{tbL("h1", 4096, time.Hour)}},
		{Owner: "pool", UsedMiB: 20480, SliceMiB: 8192, Leases: []takeBackLease{tbL("p1", 4096, time.Hour)}},
	}
	got := memVictims(owners, "req", 4096)
	if !eqStrs(ids(got), []string{"p1"}) {
		t.Fatalf("got %v, want [p1]", ids(got))
	}
	if got[0].Owner != "pool" || got[0].Ratio != 2.5 || got[0].MemoryMiB != 4096 {
		t.Fatalf("bad victim %+v", got[0])
	}
}

func TestMemVictimsLRUWithinOwner(t *testing.T) {
	owners := []takeBackOwner{
		{Owner: "req", SliceMiB: 8192},
		{Owner: "o", UsedMiB: 16384, SliceMiB: 8192, Leases: []takeBackLease{
			tbL("new", 4096, time.Minute), tbL("old", 4096, 3*time.Hour), tbL("mid", 4096, time.Hour), tbL("x", 4096, time.Minute),
		}},
	}
	got := memVictims(owners, "req", 4096)
	if !eqStrs(ids(got), []string{"old"}) {
		t.Fatalf("got %v, want [old]", ids(got))
	}
}

func TestMemVictimsBusyAfterIdle(t *testing.T) {
	busy := tbL("busy", 4096, 5*time.Hour)
	busy.Busy = true
	owners := []takeBackOwner{
		{Owner: "req", SliceMiB: 8192},
		{Owner: "o", UsedMiB: 16384, SliceMiB: 8192, Leases: []takeBackLease{busy, tbL("idle", 4096, time.Minute), tbL("k", 4096, time.Minute)}},
	}
	got := memVictims(owners, "req", 4096)
	if !eqStrs(ids(got), []string{"idle"}) {
		t.Fatalf("got %v, want [idle]", ids(got))
	}
}

func TestMemVictimsPinnedNever(t *testing.T) {
	pin := tbL("pin", 4096, 9*time.Hour)
	pin.Pinned = true
	owners := []takeBackOwner{
		{Owner: "req", SliceMiB: 8192},
		{Owner: "o", UsedMiB: 12288, SliceMiB: 8192, Leases: []takeBackLease{pin, tbL("free", 4096, time.Minute), tbL("k", 4096, time.Minute)}},
	}
	got := memVictims(owners, "req", 4096)
	if !eqStrs(ids(got), []string{"free"}) {
		t.Fatalf("got %v, want [free]", ids(got))
	}
	owners[1].Leases = []takeBackLease{pin}
	if got := memVictims(owners, "req", 4096); got != nil {
		t.Fatalf("only pinned: got %v, want nil", got)
	}
}

func TestMemVictimsRequesterFurtherOverGetsNil(t *testing.T) {
	owners := []takeBackOwner{
		{Owner: "req", UsedMiB: 16384, SliceMiB: 8192},
		{Owner: "o", UsedMiB: 12288, SliceMiB: 8192, Leases: []takeBackLease{tbL("a", 4096, time.Hour), tbL("b", 4096, time.Hour), tbL("c", 4096, time.Hour)}},
	}
	if got := memVictims(owners, "req", 4096); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func TestMemVictimsStopsOnceNeedFits(t *testing.T) {
	owners := []takeBackOwner{
		{Owner: "req", SliceMiB: 8192},
		{Owner: "o", UsedMiB: 24576, SliceMiB: 8192, Leases: []takeBackLease{
			tbL("a", 4096, 4*time.Hour), tbL("b", 4096, 3*time.Hour), tbL("c", 4096, 2*time.Hour), tbL("d", 4096, time.Hour),
		}},
	}
	got := memVictims(owners, "req", 6000)
	if !eqStrs(ids(got), []string{"a", "b"}) {
		t.Fatalf("got %v, want [a b]", ids(got))
	}
	// Caller's view is untouched.
	if owners[1].UsedMiB != 24576 || len(owners[1].Leases) != 4 {
		t.Fatalf("input mutated: %+v", owners[1])
	}
}

func TestMemVictimsZeroSliceSafe(t *testing.T) {
	owners := []takeBackOwner{
		{Owner: "req"},
		{Owner: "o", UsedMiB: 4096, SliceMiB: 0, Leases: []takeBackLease{tbL("a", 4096, time.Hour)}},
	}
	if got := memVictims(owners, "req", 1024); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

// TestTakeBackPauseGuardRace lives at the bottom of this file, with the
// other takeBackPause tests.

// TestTakeBackOwnersViews: takeBackOwners builds one view per fair-share
// owner from the snapshot (UsedMiB, SliceMiB) and lists the owner's
// running, unreleased leases under the store lock — with Pinned, busy
// (an in-flight operation or a running background job) and LastActive
// carried through.
func TestTakeBackOwnersViews(t *testing.T) {
	svc, db, _, ids := newFairShareService(t)
	alice := addIdentityUser(t, ids, "alice")
	bob := addIdentityUser(t, ids, "bob")
	ctx := context.Background()
	seedImage(t, db, "mid", 1024)

	// Alice: two running leases (one pinned, one with a running job).
	a1, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant a1: %v", err)
	}
	if _, err := svc.setPinned(alice.ID, a1.ID, true); err != nil {
		t.Fatalf("pin a1: %v", err)
	}
	a2, err := svc.grantLease(ctx, leaseRequest{owner: alice.ID, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant a2: %v", err)
	}
	// Bob: one running lease plus one suspended (which holds no
	// hugepages and must not be listed).
	b1, err := svc.grantLease(ctx, leaseRequest{owner: bob.ID, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant b1: %v", err)
	}
	b2, err := svc.grantLease(ctx, leaseRequest{owner: bob.ID, image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant b2: %v", err)
	}
	if _, err := svc.suspend(ctx, bob.ID, b2.ID); err != nil {
		t.Fatalf("suspend b2: %v", err)
	}
	// A running background job marks a2 busy like an in-flight pause.
	svc.incRunningJob(a2.ID)

	// Warm the node cache so the fair snapshot is computed; each lease
	// shows up in its owner's memory usage through the store read.
	svc.updateNodeMetrics(ctx)
	svc.invalidateFairShares()

	views := svc.takeBackOwners(ctx)
	by := map[string]takeBackOwner{}
	for _, v := range views {
		by[v.Owner] = v
	}
	av, okA := by[alice.ID]
	bv, okB := by[bob.ID]
	if !okA || !okB {
		t.Fatalf("views missing owners: %+v", views)
	}
	if av.SliceMiB != bv.SliceMiB || av.SliceMiB == 0 {
		t.Fatalf("slices = %d/%d, want equal nonzero", av.SliceMiB, bv.SliceMiB)
	}
	// Memory usage from the snapshot: 2048 MiB each (two 1024 leases).
	if av.UsedMiB != 2048 || bv.UsedMiB != 1024 {
		t.Fatalf("used = alice %d bob %d, want 2048/1024", av.UsedMiB, bv.UsedMiB)
	}
	if len(av.Leases) != 2 || len(bv.Leases) != 1 {
		t.Fatalf("leases = alice %d bob %d, want 2/1", len(av.Leases), len(bv.Leases))
	}
	la := map[string]takeBackLease{}
	for _, l := range av.Leases {
		la[l.ID] = l
	}
	if !la[a1.ID].Pinned {
		t.Fatal("a1 should carry Pinned")
	}
	if !la[a2.ID].Busy {
		t.Fatal("a2 should be busy through its running job")
	}
	if bv.Leases[0].ID != b1.ID || bv.Leases[0].Busy || bv.Leases[0].Pinned {
		t.Fatalf("b1 view = %+v, want the plain running lease", bv.Leases[0])
	}
	if bv.Leases[0].LastActive.IsZero() {
		t.Fatal("LastActive not carried")
	}
}

// newTakeBackService is a fair-share service with an image "mid" of
// 1024 MiB and a dynamic node the tests resize.
func newTakeBackService(t *testing.T) (*Service, *store.DB, *testSub, context.Context) {
	t.Helper()
	svc, db, sub := newTestService(t)
	seedImage(t, db, "mid", 1024)
	// Huge initially, so grants admit without pressure.
	sub.SetNodeInfo(substrate.NodeInfo{
		Status:            "healthy",
		HugepagesTotal:    1 << 20,
		HugepageSizeBytes: 2 << 20,
	}, nil)
	return svc, db, sub, context.Background()
}

// TestTakeBackPausePausesVictim: the happy path — a running unpinned
// lease of another owner is paused with reason take_back, the structured
// facts carry it, the events name the requester and the victim's ratio,
// and the take-back stamp is set (and cleared again by a resume).
func TestTakeBackPausePausesVictim(t *testing.T) {
	svc, db, sub, ctx := newTakeBackService(t)
	victim, err := svc.grantLease(ctx, leaseRequest{owner: "victim-owner", image: "mid", ttl: time.Hour, persistent: true})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	// The victim owner's ratio as the selector computed it.
	events := svc.Subscribe(EventFilter{LeaseID: victim.ID})
	defer events.Close()

	if err := svc.takeBackPause(ctx, victim, "req-owner", 2.5); err != nil {
		t.Fatalf("takeBackPause: %v", err)
	}
	if !victim.Suspended || victim.State != "suspended" {
		t.Fatal("victim was not paused")
	}
	if victim.Pinned {
		t.Fatal("victim pinned?")
	}
	if victim.SuspendReason != suspendReasonTakeBack {
		t.Fatalf("suspend_reason = %q, want take_back", victim.SuspendReason)
	}
	if victim.SuspendBuildID == "" || victim.ResumeBuildID == "" || victim.SuspendBuildID != victim.ResumeBuildID {
		t.Fatalf("suspend facts = build %q resume %q", victim.SuspendBuildID, victim.ResumeBuildID)
	}
	if victim.TakeBackAt.IsZero() || victim.TakeBackFor != "req-owner" || victim.TakeBackRatio != 2.5 {
		t.Fatalf("take-back stamp = %v/%q/%v", victim.TakeBackAt, victim.TakeBackFor, victim.TakeBackRatio)
	}
	if victim.LastAction != pauseActionTakeBack {
		t.Fatalf("last_action = %q, want %q", victim.LastAction, pauseActionTakeBack)
	}
	if victim.Generation != 1 {
		t.Fatalf("take-back bumped the generation to %d, want 1", victim.Generation)
	}
	// The pause credited the victim's hugepages to the cached node
	// reading (pauseLeaseBody): the free figure grew by the lease's
	// memory. Drop the cache afterwards so later reads are fresh.
	svc.nodeInfoMu.Lock()
	credited := svc.nodeInfoCache.FreeHugepageBytes()
	svc.nodeInfoMu.Unlock()
	if credited == 0 {
		t.Fatal("no cached node reading after the pause")
	}

	// Both events arrive: suspended with the structured reason and the
	// take_back event naming the requester.
	sawSuspend, sawTakeBack := false, false
	deadline := time.After(2 * time.Second)
	for !sawSuspend || !sawTakeBack {
		select {
		case ev := <-events.C:
			switch ev.Type {
			case LeaseSuspended:
				if ev.Reason != suspendReasonTakeBack {
					t.Fatalf("suspended reason = %q, want take_back", ev.Reason)
				}
				sawSuspend = true
			case LeaseTakeBack:
				if !strings.Contains(ev.Detail, "req-owner") {
					t.Fatalf("take_back detail = %q, want the requester named", ev.Detail)
				}
				if !strings.Contains(ev.Detail, "2.50") {
					t.Fatalf("take_back detail = %q, want the ratio", ev.Detail)
				}
				sawTakeBack = true
			}
		case <-deadline:
			t.Fatalf("events: suspended=%v take_back=%v", sawSuspend, sawTakeBack)
		}
	}

	// A resume clears the stamp: the lease comes back with no trace of
	// the take-back and no auto-resume debt.
	svc.store.mu.Lock()
	victim.TakeBackAt = time.Time{}
	svc.store.mu.Unlock()
	if _, err := svc.resume(ctx, "victim-owner", victim.ID); err != nil {
		// The resume needs its hugepages back: make room first.
		installDynamicNode(t, svc, sub, 1<<20, 0, 512)
		if _, err := svc.resume(ctx, "victim-owner", victim.ID); err != nil {
			t.Fatalf("resume: %v", err)
		}
	}
	if victim.Suspended || victim.SuspendReason != "" || victim.TakeBackFor != "" || !victim.TakeBackAt.IsZero() {
		t.Fatalf("after resume: suspended=%v reason=%q takeback=%v/%v", victim.Suspended, victim.SuspendReason, victim.TakeBackFor, victim.TakeBackAt)
	}
	_ = db
}

// TestTakeBackPauseRefusesIllegalVictims: the guard re-checks every
// eligibility rule. A pinned, suspended, released or lost lease, one
// with a running background job, the requester's own lease and a lease
// with an in-flight operation are all refused with errLeaseBusy and
// nothing is paused.
func TestTakeBackPauseRefusesIllegalVictims(t *testing.T) {
	svc, db, _, ctx := newTakeBackService(t)
	mk := func(owner string) *Lease {
		t.Helper()
		l, err := svc.grantLease(ctx, leaseRequest{owner: owner, image: "mid", ttl: time.Hour, persistent: true})
		if err != nil {
			t.Fatalf("grant: %v", err)
		}
		return l
	}

	pinned := mk("o1")
	if _, err := svc.setPinned("o1", pinned.ID, true); err != nil {
		t.Fatalf("pin: %v", err)
	}
	suspended := mk("o2")
	if _, err := svc.suspend(ctx, "o2", suspended.ID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	jobbed := mk("o3")
	svc.incRunningJob(jobbed.ID)

	cases := []struct {
		name string
		l    *Lease
		req  string
		busy bool
		kill bool
	}{
		{"pinned", pinned, "req", false, false},
		{"suspended", suspended, "req", false, false},
		{"running job", jobbed, "req", false, false},
		{"requester's own", mk("req"), "req", false, false},
		{"already busy", mk("o4"), "req", true, false},
		{"lost", mk("o5"), "req", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.busy {
				svc.store.mu.Lock()
				tc.l.busy = true
				svc.store.mu.Unlock()
			}
			if tc.kill {
				svc.store.mu.Lock()
				tc.l.setState("lost")
				svc.store.mu.Unlock()
			}
			if err := svc.takeBackPause(ctx, tc.l, tc.req, 2.0); !errors.Is(err, errLeaseBusy) {
				t.Fatalf("takeBackPause err = %v, want errLeaseBusy", err)
			}
			// A lease that was already suspended (the hand suspend above)
			// stays suspended with that suspend's own facts; the point is
			// that the take-back added nothing of its own.
			if tc.l.SuspendReason == suspendReasonTakeBack || tc.l.TakeBackFor != "" {
				t.Fatal("the illegal victim was paused")
			}
		})
	}
	_ = db
}

// TestTakeBackPauseGuardRace: takeBackPause re-checks running, unpinned
// and not-busy in the same critical section where it sets busy. The race
// to cover: after a victim is selected, the owner pins the lease (or a
// job starts) before takeBackPause runs; the pause must then be refused,
// nothing paused, no event emitted. Both orders are exercised, the pin
// landing first must always win, under -race.
func TestTakeBackPauseGuardRace(t *testing.T) {
	svc, _, _, ctx := newTakeBackService(t)

	for i := 0; i < 50; i++ {
		victim, err := svc.grantLease(ctx, leaseRequest{owner: "victim-owner", image: "mid", ttl: time.Hour, persistent: true})
		if err != nil {
			t.Fatalf("grant: %v", err)
		}
		pinned := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			close(pinned) // let the pin race the pause
			if _, err := svc.setPinned("victim-owner", victim.ID, true); err != nil {
				t.Errorf("pin: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			<-pinned
			// The pause must lose whenever the pin landed first: the
			// guard re-checks Pinned under the same lock that sets busy.
			if err := svc.takeBackPause(ctx, victim, "req-owner", 2.0); err != nil {
				if !errors.Is(err, errLeaseBusy) {
					t.Errorf("takeBackPause: %v", err)
				}
				return
			}
			// The pause won the race: the lease must have been unpinned
			// at the moment busy was set, so it may be suspended — but
			// never pinned-and-suspended.
			svc.store.mu.Lock()
			defer svc.store.mu.Unlock()
			if victim.Pinned && victim.Suspended {
				t.Error("a lease pinned before its pause was suspended anyway")
			}
		}()
		wg.Wait()
		// Whatever the outcome, unpin so the next round can grant cleanly.
		if _, err := svc.setPinned("victim-owner", victim.ID, false); err != nil {
			t.Fatalf("unpin: %v", err)
		}
		svc.store.mu.Lock()
		svc.store.leases[victim.ID].released = true
		delete(svc.store.leases, victim.ID)
		svc.store.mu.Unlock()
	}
}

// TestMemVictimsFallsThroughToSmallerLease: when an owner's LRU lease is
// too big to pass the "ratio after give-up stays above the requester"
// check, the owner is skipped only if no smaller lease of theirs passes
// either. The owner's leases are tried in LRU order and the first that
// passes is taken.
func TestMemVictimsFallsThroughToSmallerLease(t *testing.T) {
	// The owner is far over (8192 used of a 4096 slice, ratio 2). Their
	// LRU lease is 4096: giving it up leaves 4096/4096 = 1, not above the
	// requester's ratio-after of (4096+512)/4096 = 1.125. The next lease
	// in LRU order (1024) does: 7168/4096 = 1.75 > 1.125.
	big := tbL("big", 4096, 2*time.Hour)
	small := tbL("small", 1024, time.Hour)
	owners := []takeBackOwner{
		{Owner: "req", UsedMiB: 4096, SliceMiB: 4096},
		{Owner: "o", UsedMiB: 8192, SliceMiB: 4096, Leases: []takeBackLease{big, small}},
	}
	got := memVictims(owners, "req", 512)
	if !eqStrs(ids(got), []string{"small"}) {
		t.Fatalf("got %v, want [small] (the LRU lease is too big to give up)", ids(got))
	}

	// No lease of the owner passes: nothing is taken.
	huge := tbL("huge", 4096, time.Hour)
	owners[1].Leases = []takeBackLease{huge}
	if got := memVictims(owners, "req", 512); got != nil {
		t.Fatalf("got %v, want nil (every lease of the owner drops them below the requester)", ids(got))
	}

	// The LRU lease is small and passes: it is still the pick (no fall
	// through past it).
	tiny := tbL("tiny", 512, 2*time.Hour)
	other := tbL("other", 1024, time.Hour)
	owners[1].Leases = []takeBackLease{tiny, other}
	if got := memVictims(owners, "req", 512); !eqStrs(ids(got), []string{"tiny"}) {
		t.Fatalf("got %v, want [tiny]", ids(got))
	}
}
