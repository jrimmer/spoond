package api

import (
	"context"
	"testing"
	"time"
)

// probeAt runs one rootfs probe pass with the service clock at t.
func probeAt(svc *Service, t time.Time) {
	svc.now = func() time.Time { return t }
	svc.probeRootfsLeases(context.Background())
}

// TestGuestUnreachableRecovered: a guest whose probes only time out
// while another lease answers is recovered from its checkpoint once the
// cutoff has passed, with a guest_unreachable marker first (#159).
func TestGuestUnreachableRecovered(t *testing.T) {
	svc, sub, l := newRootfsProbeService(t, true)
	ctx := context.Background()
	if _, err := svc.checkpointLease(ctx, l); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if _, err := svc.grant(ctx, "consumer-a", "py-base", time.Minute, false, "", nil, "", "", nil); err != nil {
		t.Fatalf("grant second: %v", err)
	}
	oldSandbox := l.SandboxID
	sub.rootfsTimeout[oldSandbox] = true
	esub := svc.Subscribe(EventFilter{})
	t.Cleanup(func() { svc.now = time.Now })

	cutoff := svc.guestUnreachableAfter()
	base := time.Now()
	probeAt(svc, base)
	probeAt(svc, base.Add(cutoff/2))
	if l.State != "running" || l.Generation != 1 {
		t.Fatalf("inside the cutoff lease = %s gen %d, want running gen 1", l.State, l.Generation)
	}
	probeAt(svc, base.Add(cutoff+time.Second))
	esub.Close()
	if l.State != "recovered" || l.Generation != 2 {
		t.Fatalf("past the cutoff lease = %s gen %d, want recovered gen 2", l.State, l.Generation)
	}
	if got := calls(sub.Fake, "Delete "+oldSandbox); got != 1 {
		t.Fatalf("Delete calls for the hung sandbox = %d, want 1", got)
	}
	var detail string
	for _, ev := range collectEvents(esub.C) {
		if ev.LeaseID == l.ID && ev.Type == LeaseGuestUnreachable {
			detail = ev.Detail
		}
	}
	if detail != "guest unreachable for 60 min" {
		t.Fatalf("guest_unreachable marker = %q", detail)
	}
}

// TestGuestUnreachableNeedsAnotherAnswer: when no other lease answers (a
// slow host where every probe times out, or a single running lease),
// silence is no evidence and nothing is recovered.
func TestGuestUnreachableNeedsAnotherAnswer(t *testing.T) {
	svc, sub, l1 := newRootfsProbeService(t, true)
	ctx := context.Background()
	t.Cleanup(func() { svc.now = time.Now })
	sub.rootfsTimeout[l1.SandboxID] = true
	cutoff := svc.guestUnreachableAfter()
	base := time.Now()

	// A single running lease.
	probeAt(svc, base)
	probeAt(svc, base.Add(2*cutoff))
	if l1.State != "running" || l1.Generation != 1 {
		t.Fatalf("lone lease = %s gen %d, want running gen 1", l1.State, l1.Generation)
	}

	// Every lease times out.
	l2, err := svc.grant(ctx, "consumer-a", "py-base", time.Minute, false, "", nil, "", "", nil)
	if err != nil {
		t.Fatalf("grant second: %v", err)
	}
	sub.rootfsUnavailable[l2.SandboxID] = true
	probeAt(svc, base.Add(3*cutoff))
	probeAt(svc, base.Add(5*cutoff))
	if l1.State != "running" || l2.State != "running" {
		t.Fatalf("states = %s, %s with no lease answering, want running", l1.State, l2.State)
	}
	if got := calls(sub.Fake, "Delete "+l1.SandboxID); got != 0 {
		t.Fatalf("Delete calls = %d, want 0", got)
	}
}

// TestGuestUnreachableClockResetsOnAnswer: any answer from the guest (a
// successful exec through the API) stops the clock, so silence on either
// side of it never adds up to the cutoff; the clock starts again at the
// next silent probe. An unavailable answer runs the
// clock like a timeout.
func TestGuestUnreachableClockResetsOnAnswer(t *testing.T) {
	svc, sub, l := newRootfsProbeService(t, false)
	ctx := context.Background()
	if _, err := svc.grant(ctx, "consumer-a", "py-base", time.Minute, false, "", nil, "", "", nil); err != nil {
		t.Fatalf("grant second: %v", err)
	}
	sub.rootfsUnavailable[l.SandboxID] = true
	t.Cleanup(func() { svc.now = time.Now })
	cutoff := svc.guestUnreachableAfter()
	base := time.Now()

	probeAt(svc, base)
	svc.now = func() time.Time { return base.Add(cutoff - 5*time.Minute) }
	svc.recordRootfsAlive(l.ID)
	probeAt(svc, base.Add(cutoff+time.Second))
	if l.State != "running" {
		t.Fatalf("lease = %s after an answer reset the clock, want running", l.State)
	}
	// The clock restarted at the first silent probe after the answer.
	probeAt(svc, base.Add(2*cutoff+2*time.Second))
	if l.State != "lost" || l.live() {
		t.Fatalf("lease = %s after a full cutoff of silence, want lost", l.State)
	}
	if l.LostReason != "guest unreachable for 60 min" {
		t.Fatalf("lost_reason = %q", l.LostReason)
	}
}

// TestGuestUnreachableDisabled: GUEST_UNREACHABLE_SECS=0 leaves a silent
// guest alone however long it is silent.
func TestGuestUnreachableDisabled(t *testing.T) {
	svc, sub, l := newRootfsProbeService(t, false)
	if _, err := svc.grant(context.Background(), "consumer-a", "py-base", time.Minute, false, "", nil, "", "", nil); err != nil {
		t.Fatalf("grant second: %v", err)
	}
	svc.SetGuestUnreachable(0)
	sub.rootfsTimeout[l.SandboxID] = true
	t.Cleanup(func() { svc.now = time.Now })
	base := time.Now()
	probeAt(svc, base)
	probeAt(svc, base.Add(24*time.Hour))
	if l.State != "running" {
		t.Fatalf("lease = %s with the cutoff disabled, want running", l.State)
	}
}
