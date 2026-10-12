// Released leases report what their VM actually held (#130 step 1):
// the released event's detail appends "· peak N MiB"
// (the sampler's measured high-water mark, whole MiB) after the
// release reason, and the release journal line adds peak_mib=N next
// to memory_mib. A lease with no sample gains neither. The peak is
// read before the sampler forgets the lease, so a leak here shows up
// as a missing suffix, never as a wrong one.
package api

import (
	"context"
	"log"
	"strings"
	"testing"
	"time"
)

// TestReleasePeakDetailFmt pins the suffix's own wording: whole MiB
// after the reason, "peak N MiB" alone on an empty reason, and nothing
// at all without a measurement.
func TestReleasePeakDetailFmt(t *testing.T) {
	const MiB = 1024 * 1024
	cases := []struct {
		name   string
		reason string
		has    bool
		peak   float64
		want   string
	}{
		{"ci reason", "ci job 3609 ✓ 11m02s", true, 411.2 * MiB, "ci job 3609 ✓ 11m02s · peak 412 MiB"},
		{"empty reason", "", true, 412 * MiB, "peak 412 MiB"},
		{"exact MiB", "TTL expired", true, 300 * MiB, "TTL expired · peak 300 MiB"},
		{"rounds up", "ttl", true, 300.05 * MiB, "ttl · peak 301 MiB"},
		{"no sample keeps the reason", "deleted through the API", false, 0, "deleted through the API"},
		{"no sample, empty reason stays empty", "", false, 0, ""},
	}
	for _, c := range cases {
		if got := releasePeakDetail(c.reason, c.has, c.peak); got != c.want {
			t.Errorf("%s: releasePeakDetail(%q, %v, %g) = %q, want %q",
				c.name, c.reason, c.has, c.peak, got, c.want)
		}
	}
}

// releasePeakTestService grants a running lease on a sandbox the
// sampler can see and seeds the peak. It is the base the release tests
// build on. grantLease leaves the lease running (the fake substrate's
// create succeeds), so the seeded sample matches the record path's
// live-lease guard.
func releasePeakTestService(t *testing.T, peakMiB float64) (*Service, *Lease, *strings.Builder) {
	t.Helper()
	svc, db, _ := newTestService(t)
	seedImage(t, db, "py-base", 2048)
	l, err := svc.grantLease(context.Background(), leaseRequest{owner: "consumer-a", image: "py-base", ttl: time.Hour})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	svc.store.mu.Lock()
	l.SandboxID, l.HostIP = "sb_peak", "10.0.0.1"
	if !l.live() {
		svc.store.mu.Unlock()
		t.Fatalf("test lease is %q, want running", l.State)
	}
	svc.store.mu.Unlock()

	// One fresh sample: peak == used after a single tick, which mibCeil
	// turns into the whole MiB the test names.
	svc.memSamples.record(
		map[string]*Lease{l.ID: l},
		map[string]float64{"sb_peak": peakMiB * 1024 * 1024},
		map[string]float64{"sb_peak": 8 * 1024 * 1024 * 1024},
	)
	if _, ok := svc.memSamples.peak(l.ID); !ok {
		t.Fatal("seeding the sample failed")
	}

	var buf strings.Builder
	svc.log = log.New(&buf, "", 0)
	return svc, l, &buf
}

// TestReleasedEventCarriesPeak drives a release with a recorded peak:
// the event's detail gains the suffix after the reason and the journal
// line gains peak_mib=N, and the sampler has forgotten the lease when
// the release returns.
func TestReleasedEventCarriesPeak(t *testing.T) {
	// 412.9 MiB: the ceil must show 413, not the peak's floor.
	svc, l, buf := releasePeakTestService(t, 412.9)
	sub := svc.Subscribe(EventFilter{LeaseID: l.ID})
	defer sub.Close()

	svc.releaseBecause(context.Background(), l, "ci job 3609 ✓ 11m02s")

	var released *LeaseEvent
	for released == nil {
		select {
		case ev := <-sub.C:
			if ev.Type == LeaseReleased {
				released = &ev
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no released event arrived")
		}
	}
	want := "ci job 3609 ✓ 11m02s · peak 413 MiB"
	if released.Detail != want {
		t.Errorf("released detail = %q, want %q", released.Detail, want)
	}

	line := strings.TrimSpace(buf.String())
	f := journalFields(t, line)
	if f["peak_mib"] != "413" {
		t.Errorf("journal peak_mib = %q, want 413 (line: %s)", f["peak_mib"], line)
	}
	if f["reason"] != "ci job 3609 ✓ 11m02s" {
		t.Errorf("journal reason = %q, want the verbatim CI reason (line: %s)", f["reason"], line)
	}
	if f["op"] != "release" {
		t.Errorf("journal op = %q, want release", f["op"])
	}
	// The peak's entry left the sampler with the release.
	if _, ok := svc.memSamples.peak(l.ID); ok {
		t.Error("released lease's sampler entry survived the release")
	}
}

// TestReleasedEventWithoutPeak: no sample (no OTEL_PROM_URL, or a lease
// the collector never mentioned) — the released event's detail is the
// bare reason and the journal line carries no peak_mib field at all.
func TestReleasedEventWithoutPeak(t *testing.T) {
	svc, _, _ := newTestService(t)
	seedImage(t, svc.db, "py-base", 2048)
	l, err := svc.grantLease(context.Background(), leaseRequest{owner: "consumer-a", image: "py-base", ttl: time.Hour})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}

	sub := svc.Subscribe(EventFilter{LeaseID: l.ID})
	defer sub.Close()
	var buf strings.Builder
	svc.log = log.New(&buf, "", 0)

	svc.releaseBecause(context.Background(), l, "TTL expired")
	released := waitReleased(t, sub)
	if want := "TTL expired"; released.Detail != want {
		t.Errorf("released detail = %q, want %q", released.Detail, want)
	}
	line := strings.TrimSpace(buf.String())
	f := journalFields(t, line)
	if _, ok := f["peak_mib"]; ok {
		t.Errorf("journal carries peak_mib without a sample (line: %s)", line)
	}
	if f["reason"] != "ttl" {
		t.Errorf("journal reason = %q, want ttl", f["reason"])
	}
}

// waitReleased reads a subscription that was taken before the release
// until the lease's released event arrives (live delivery, no replay).
func waitReleased(t *testing.T, sub *EventSubscription) LeaseEvent {
	t.Helper()
	for {
		select {
		case ev := <-sub.C:
			if ev.Type == LeaseReleased {
				return ev
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no released event arrived")
			return LeaseEvent{}
		}
	}
}

// TestReleasedPeakIsReadBeforeForgetting pins the read-before-forget
// order by the mutation the ticket names: a release that lets the
// sampler's forget outrun its read loses the peak. The hook forgets
// before the release's read could see the entry, so this run emits the
// bare reason and no peak_mib — the failure a forget-first release
// would produce. The committed release reads first and forgets in one
// locked step (peakRelease), and TestReleasedEventCarriesPeak pins its
// suffix-bearing result.
func TestReleasedPeakIsReadBeforeForgetting(t *testing.T) {
	svc, l, buf := releasePeakTestService(t, 412.9)
	sub := svc.Subscribe(EventFilter{LeaseID: l.ID})
	defer sub.Close()

	svc.testBeforeReleaseCommit = func(*Lease) {
		// The mutation: forget the sample before the release's read
		// could see it.
		svc.memSamples.forget(l.ID)
	}
	svc.releaseBecause(context.Background(), l, "ci job 3609 ✓ 11m02s")

	ev := waitReleased(t, sub)
	if want := "ci job 3609 ✓ 11m02s"; ev.Detail != want {
		t.Errorf("the forget-first release must lose the peak (mutation); detail = %q, want %q", ev.Detail, want)
	}
	line := strings.TrimSpace(buf.String())
	f := journalFields(t, line)
	if _, ok := f["peak_mib"]; ok {
		t.Errorf("the forget-first release must write no peak_mib (mutation; line: %s)", line)
	}
}
