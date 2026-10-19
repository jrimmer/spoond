// Give-back tests (spoond-aeo6, spoond-90ia): a tick at or above the
// floor resumes one lease spoond's take-back paused, longest-paused
// first, once its memory plus the floor plus the margin is free after
// the running leases fault in what they were granted, no create waits
// for admission, no take-back ran in the last two minutes and no floor
// take-back in the last fifteen. A lease taken back again after a
// give-back is held, the hold doubling per re-take. A lease the caller
// paused is never given back.
//
// The fixtures reuse the pressure tests' dynamic node: free MiB =
// (totalPages - baseUsed - live sandboxes x pagesPerLease) x 2, and a
// "mid" lease is 1024 MiB.

package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v3/substrate"
)

// giveBackFixture grants n "mid" leases to one owner, takes them all
// back by the floor (oldest first, one second apart), and leaves the
// node reading freeMiB free with every lease paused. The clock starts
// sixteen minutes after the last take-back, past the floor quiet period.
func giveBackFixture(t *testing.T, n, freeMiB int) (*Service, *testSub, context.Context, []*Lease, *time.Time) {
	t.Helper()
	svc, sub, ctx := newPressureService(t)
	alice := preemptOwner(t, svc, "alice")
	preemptOwner(t, svc, "bob")
	var ls []*Lease
	for i := 0; i < n; i++ {
		l, err := svc.grantLease(ctx, leaseRequest{owner: alice, image: "mid", ttl: time.Hour, persistent: true})
		if err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
		ls = append(ls, l)
	}
	now := time.Unix(2_000_000, 0)
	svc.now = func() time.Time { return now }
	for i, l := range ls {
		now = time.Unix(2_000_000, 0).Add(time.Duration(i) * time.Second)
		if err := svc.takeBackPause(ctx, l, pressureRequester, 2); err != nil {
			t.Fatalf("take back %d: %v", i, err)
		}
	}
	// 4096 pages total, every lease paused: free = (4096 - base) x 2.
	installDynamicNode(t, svc, sub, 4096, uint64(4096-freeMiB/2), 512)
	svc.cfg.MemoryFloorMiB = 2048
	warmFairShares(svc, ctx)
	now = now.Add(16 * time.Minute)
	return svc, sub, ctx, ls, &now
}

// TestGiveBackResumesTakenBackLease: exactly the lease's memory plus the
// floor plus the default margin free (1024 + 2048 + 1024 = 4096 MiB)
// is enough: one tick resumes it, with the give_back event and counter.
func TestGiveBackResumesTakenBackLease(t *testing.T) {
	svc, _, ctx, ls, _ := giveBackFixture(t, 1, 4096)
	sub := svc.Subscribe(EventFilter{})
	defer sub.Close()

	if !svc.pressureCheck(ctx) {
		t.Fatal("pressureCheck reported nothing done, want a give-back")
	}
	if ls[0].Suspended || ls[0].State != "running" {
		t.Fatalf("lease state %s suspended=%v, want running", ls[0].State, ls[0].Suspended)
	}
	if got := counterValue(t, svc.metrics.MemoryGiveBacksTotal); got != 1 {
		t.Fatalf("give-back counter = %v, want 1", got)
	}
	found := false
	for {
		select {
		case ev := <-sub.C:
			if ev.Type == LeaseResumed && ev.LeaseID == ls[0].ID && len(ev.Detail) >= 9 && ev.Detail[:9] == "give_back" {
				found = true
			}
			continue
		default:
		}
		break
	}
	if !found {
		t.Fatal("no lease.resumed event with a give_back detail")
	}
}

// TestGiveBackNeedsRoom: one MiB short of memory + floor + margin, the
// lease stays paused.
func TestGiveBackNeedsRoom(t *testing.T) {
	svc, _, ctx, ls, _ := giveBackFixture(t, 1, 4096)
	svc.cfg.GiveBackMarginMiB = 1025
	if svc.pressureCheck(ctx) {
		t.Fatal("pressureCheck acted with too little room")
	}
	if !ls[0].Suspended {
		t.Fatal("lease resumed with too little room")
	}
}

// TestGiveBackQuietAfterTakeBack: within fifteen minutes of a floor
// take-back nothing is given back, room or not.
func TestGiveBackQuietAfterTakeBack(t *testing.T) {
	svc, _, ctx, ls, now := giveBackFixture(t, 1, 8192)
	*now = now.Add(-15 * time.Minute) // one minute after the take-back
	svc.pressureCheck(ctx)
	if !ls[0].Suspended {
		t.Fatal("lease given back one minute after a floor take-back")
	}
	*now = now.Add(13 * time.Minute) // fourteen minutes after
	svc.pressureCheck(ctx)
	if !ls[0].Suspended {
		t.Fatal("lease given back inside the floor quiet period")
	}
	*now = now.Add(2 * time.Minute)
	svc.pressureCheck(ctx)
	if ls[0].Suspended {
		t.Fatal("lease not given back after the floor quiet period")
	}
}

// TestGiveBackQuietAfterAdmissionTakeBack: an admission take-back keeps
// give-back quiet for two minutes only.
func TestGiveBackQuietAfterAdmissionTakeBack(t *testing.T) {
	svc, _, ctx, ls, now := giveBackFixture(t, 1, 8192)
	svc.noteTakeBack(false)
	*now = now.Add(time.Minute)
	svc.pressureCheck(ctx)
	if !ls[0].Suspended {
		t.Fatal("lease given back one minute after an admission take-back")
	}
	*now = now.Add(2 * time.Minute)
	svc.pressureCheck(ctx)
	if ls[0].Suspended {
		t.Fatal("lease not given back three minutes after an admission take-back")
	}
}

// TestGiveBackCountsUnfaultedMemory: a running 4096 MiB lease that has
// faulted in only 1024 MiB can still take 3072 more, so give-back
// counts that as spoken for: with 7168 MiB free the 1024 MiB lease fits
// with the default margin (7168 - 3072 = 1024 + 2048 + 1024) and not
// with one MiB more, although free memory alone would pass.
func TestGiveBackCountsUnfaultedMemory(t *testing.T) {
	svc, _, ctx, ls, _ := giveBackFixture(t, 1, 8192)
	carol := preemptOwner(t, svc, "carol")
	if _, err := svc.grantLease(ctx, leaseRequest{owner: carol, image: "big4", ttl: time.Hour, persistent: true}); err != nil {
		t.Fatalf("grant big4: %v", err)
	}
	svc.cfg.GiveBackMarginMiB = 1025
	svc.pressureCheck(ctx)
	if !ls[0].Suspended {
		t.Fatal("lease given back into memory a running lease has not faulted in yet")
	}
	svc.cfg.GiveBackMarginMiB = 0 // the default, 1024
	svc.pressureCheck(ctx)
	if ls[0].Suspended {
		t.Fatal("lease not given back with room after the unfaulted memory")
	}
}

// TestGiveBackHoldsRetakenLease: a lease taken back again after a
// give-back is held 15 minutes, then 30 after the next re-take; one
// that ran an hour after its give-back starts at 15 again.
func TestGiveBackHoldsRetakenLease(t *testing.T) {
	svc, _, ctx, ls, now := giveBackFixture(t, 1, 8192)
	l := ls[0]
	retake := func() {
		t.Helper()
		if err := svc.takeBackPause(ctx, l, "u-carol", 2); err != nil {
			t.Fatalf("re-take: %v", err)
		}
	}
	giveBackAfter := func(wait time.Duration, want bool) {
		t.Helper()
		*now = now.Add(wait)
		svc.pressureCheck(ctx)
		if got := !l.Suspended; got != want {
			t.Fatalf("after %s: given back = %v, want %v", wait, got, want)
		}
	}
	giveBackAfter(0, true)
	retake() // re-take 1: held 15 min
	giveBackAfter(3*time.Minute, false)
	giveBackAfter(13*time.Minute, true)
	retake() // re-take 2: held 30 min
	giveBackAfter(16*time.Minute, false)
	giveBackAfter(15*time.Minute, true)
	*now = now.Add(time.Hour)
	retake() // ran an hour: held 15 min again
	giveBackAfter(16*time.Minute, true)
}

// TestGiveBackWaitsForQueuedCreates: a create waiting for admission
// comes first.
func TestGiveBackWaitsForQueuedCreates(t *testing.T) {
	svc, _, ctx, ls, _ := giveBackFixture(t, 1, 8192)
	svc.admitQ.mu.Lock()
	svc.admitQ.tickets = append(svc.admitQ.tickets, &admissionTicket{})
	svc.admitQ.mu.Unlock()
	svc.pressureCheck(ctx)
	if !ls[0].Suspended {
		t.Fatal("lease given back while a create waited")
	}
	svc.admitQ.mu.Lock()
	svc.admitQ.tickets = nil
	svc.admitQ.mu.Unlock()
	svc.pressureCheck(ctx)
	if ls[0].Suspended {
		t.Fatal("lease not given back once the queue emptied")
	}
}

// TestGiveBackNeverResumesCallerPauses: a hand suspend and an idle
// suspend are the caller's; give-back leaves them alone.
func TestGiveBackNeverResumesCallerPauses(t *testing.T) {
	svc, _, ctx, ls, _ := giveBackFixture(t, 2, 8192)
	svc.store.mu.Lock()
	ls[0].SuspendReason = "" // what a hand suspend leaves
	ls[1].SuspendReason = "idle_suspend"
	svc.store.mu.Unlock()
	svc.pressureCheck(ctx)
	svc.pressureCheck(ctx)
	if !ls[0].Suspended || !ls[1].Suspended {
		t.Fatalf("caller pauses given back: %v %v", ls[0].Suspended, ls[1].Suspended)
	}
}

// TestGiveBackFIFOOnePerTick: the longest-paused lease comes back first,
// one per tick.
func TestGiveBackFIFOOnePerTick(t *testing.T) {
	svc, _, ctx, ls, _ := giveBackFixture(t, 2, 8192)
	svc.pressureCheck(ctx)
	if ls[0].Suspended || !ls[1].Suspended {
		t.Fatalf("after one tick: first suspended=%v second suspended=%v, want only the first back", ls[0].Suspended, ls[1].Suspended)
	}
	svc.pressureCheck(ctx)
	if ls[1].Suspended {
		t.Fatal("second lease not given back on the next tick")
	}
}

// TestGiveBackFailedResumeBacksOff: a resume that fails leaves the lease
// paused and passes it over for ten minutes.
func TestGiveBackFailedResumeBacksOff(t *testing.T) {
	svc, sub, ctx, ls, now := giveBackFixture(t, 1, 8192)
	fail := true
	sub.createFn = func(ctx context.Context, req substrate.CreateRequest) (substrate.Sandbox, error) {
		if fail {
			return substrate.Sandbox{}, errors.New("restore failed")
		}
		return sub.Fake.Create(ctx, req)
	}
	t.Cleanup(func() { sub.createFn = nil })

	svc.pressureCheck(ctx)
	if !ls[0].Suspended || lostErr(ls[0]) != nil {
		t.Fatalf("after a failed give-back: suspended=%v lost=%v, want suspended and not lost", ls[0].Suspended, lostErr(ls[0]))
	}
	fail = false
	*now = now.Add(5 * time.Minute)
	svc.pressureCheck(ctx)
	if !ls[0].Suspended {
		t.Fatal("given back inside the 10 min back-off")
	}
	*now = now.Add(6 * time.Minute)
	svc.pressureCheck(ctx)
	if ls[0].Suspended {
		t.Fatal("not given back after the back-off")
	}
}

// TestGiveBackOff: GIVE_BACK=off resumes nothing.
func TestGiveBackOff(t *testing.T) {
	svc, _, ctx, ls, _ := giveBackFixture(t, 1, 8192)
	svc.cfg.GiveBackOff = true
	svc.pressureCheck(ctx)
	if !ls[0].Suspended {
		t.Fatal("lease given back with GIVE_BACK=off")
	}
}

// TestGiveBackNeverTakesBack: a give-back admission never pauses another
// lease for its room, even when take-back could make some.
func TestGiveBackNeverTakesBack(t *testing.T) {
	svc, _, ctx, _, _ := giveBackFixture(t, 1, 8192)
	if err := svc.takeBackForAdmission(withoutTakeBack(ctx), "bob", 1<<20); err != nil {
		t.Fatalf("takeBackForAdmission: %v", err)
	}
	if got := counterValue(t, svc.metrics.MemoryPressureTakebacksTotal); got != 0 {
		t.Fatalf("take-backs = %v, want 0", got)
	}
}
