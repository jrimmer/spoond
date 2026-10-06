package runner

import (
	"context"
	"testing"
	"time"
)

// TestReleaseReasonFormats pins the reason the runner sends on release:
// the job id, an outcome glyph and the duration, with the cancelled form
// naming itself.
func TestReleaseReasonFormats(t *testing.T) {
	job := &Job{ID: 3609}
	cases := []struct {
		result Result
		d      time.Duration
		want   string
	}{
		{ResultSuccess, 11*time.Minute + 2*time.Second, "ci job 3609 ✓ 11m02s"},
		{ResultFailure, 4*time.Minute + 10*time.Second, "ci job 3609 ✗ 4m10s"},
		{ResultSuccess, 42 * time.Second, "ci job 3609 ✓ 42s"},
		{ResultSuccess, time.Hour + 5*time.Minute + 3*time.Second, "ci job 3609 ✓ 1h05m03s"},
		{ResultCancelled, 90 * time.Second, "ci job 3609 cancelled"},
		{ResultSkipped, 3 * time.Second, "ci job 3609 skipped 3s"},
	}
	for _, tc := range cases {
		if got := releaseReason(job, tc.result, tc.d); got != tc.want {
			t.Errorf("releaseReason(%v, %v) = %q, want %q", tc.result, tc.d, got, tc.want)
		}
	}
}

// TestHumanDuration pins the compact duration rendering.
func TestHumanDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{7 * time.Second, "7s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m00s"},
		{11*time.Minute + 2*time.Second, "11m02s"},
		{time.Hour, "1h00m00s"},
		{-time.Second, "0s"},
	}
	for _, tc := range cases {
		if got := humanDuration(tc.d); got != tc.want {
			t.Errorf("humanDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// fakeReasonLease is a SandboxProvider that also implements
// LeaseReleaser, recording the reasons the executor sends.
type fakeReasonLease struct {
	*fakeLease
	reasons []string
}

func (f *fakeReasonLease) DeleteReason(ctx context.Context, id, reason string) error {
	f.reasons = append(f.reasons, reason)
	return nil
}

// TestExecutorReleasesWithReason: the executor releases the job's lease
// through the LeaseReleaser capability with the outcome and duration.
func TestExecutorReleasesWithReason(t *testing.T) {
	payload := `
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`
	fl := &fakeReasonLease{fakeLease: newFakeLease()}
	e := &Executor{Sandbox: fl, Sink: &fakeSink{}, Labels: map[string]string{"ubuntu-latest": "go-base"}}
	if err := e.Run(context.Background(), testJob(payload)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fl.reasons) != 1 {
		t.Fatalf("got %d release reasons, want 1 (%v)", len(fl.reasons), fl.reasons)
	}
	if got := fl.reasons[0]; got != "ci job 1 ✓ 0s" {
		t.Fatalf("release reason = %q, want the job outcome and duration", got)
	}
	// The plain Delete path is not taken when the capability is present.
	if len(fl.deleted) != 0 {
		t.Fatalf("plain Delete also called: %v", fl.deleted)
	}
}
