package spoonddash

import (
	"testing"
	"time"
)

// TestRecTime: the records' time format — M/D h:mma in Pacific
// (10/12 1:28a); noon is 12p, midnight 12a.
func TestRecTime(t *testing.T) {
	cases := []struct {
		utc  string
		want string
	}{
		{"2026-10-12T08:28:00Z", "10/12 1:28a"},  // PDT, −7
		{"2026-10-12T20:15:00Z", "10/12 1:15p"},  // afternoon
		{"2026-10-09T23:31:00Z", "10/9 4:31p"},   // a single-digit day
		{"2026-10-12T07:00:00Z", "10/12 12:00a"}, // midnight
		{"2026-10-12T19:00:00Z", "10/12 12:00p"}, // noon
		{"2026-01-05T18:30:00Z", "1/5 10:30a"},   // PST, −8: 18:30Z is 10:30a
	}
	for _, tc := range cases {
		at, err := time.Parse(time.RFC3339, tc.utc)
		if err != nil {
			t.Fatal(err)
		}
		if got := recTime(at); got != tc.want {
			t.Errorf("recTime(%s) = %q, want %q", tc.utc, got, tc.want)
		}
	}
}

// TestRecDay: the records' date-alone format, M/D in Pacific — a UTC
// evening belongs to the Pacific day it is still or already in.
func TestRecDay(t *testing.T) {
	cases := []struct {
		utc  string
		want string
	}{
		{"2026-10-09T20:00:00Z", "10/9"},
		{"2026-10-12T06:59:00Z", "10/11"}, // 10/11 11:59p PDT
		{"2026-10-12T07:00:00Z", "10/12"}, // 10/12 12:00a PDT
		{"2026-01-01T08:00:00Z", "1/1"},   // 1/1 12:00a PST
	}
	for _, tc := range cases {
		at, err := time.Parse(time.RFC3339, tc.utc)
		if err != nil {
			t.Fatal(err)
		}
		if got := recDay(at); got != tc.want {
			t.Errorf("recDay(%s) = %q, want %q", tc.utc, got, tc.want)
		}
	}
}

// TestRecDur: the records' durations — N ms, Ns, Nm Ns, Nh Nm, Nd Nh.
func TestRecDur(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0 ms"},
		{900 * time.Millisecond, "900 ms"},
		{2 * time.Second, "2s"},
		{59 * time.Second, "59s"},
		{time.Minute, "1m 0s"},
		{90 * time.Second, "1m 30s"},
		{21*time.Hour + 19*time.Minute, "21h 19m"},
		{24 * time.Hour, "1d 0h"},
		{3*24*time.Hour + 5*time.Hour, "3d 5h"},
	}
	for _, tc := range cases {
		if got := recDur(tc.d); got != tc.want {
			t.Errorf("recDur(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// TestRecThousands: k for thousands (1.6k), plain below.
func TestRecThousands(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0"}, {9, "9"}, {589, "589"}, {999, "999"},
		{1000, "1.0k"}, {1640, "1.6k"}, {46800, "46.8k"},
	}
	for _, tc := range cases {
		if got := recThousands(tc.n); got != tc.want {
			t.Errorf("recThousands(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// TestRecHoursAndGiBH: the lifetime row's totals — whole hours below a
// thousand, k above; GiB·h to one decimal, k above a thousand.
func TestRecHoursAndGiBH(t *testing.T) {
	cases := []struct {
		hours float64
		want  string
	}{
		{0, "0 h"},
		{618, "618 h"},
		{999.9, "999 h"},
		{1000, "1.0k h"},
		{1620, "1.6k h"},
	}
	for _, tc := range cases {
		if got := recHours(tc.hours); got != tc.want {
			t.Errorf("recHours(%v) = %q, want %q", tc.hours, got, tc.want)
		}
	}
	for _, tc := range []struct {
		v    float64
		want string
	}{
		{0, "0.0 GiB·h"}, {1.5, "1.5 GiB·h"}, {999.9, "999.9 GiB·h"}, {1600, "1.6k GiB·h"},
	} {
		if got := recGiBH(tc.v); got != tc.want {
			t.Errorf("recGiBH(%v) = %q, want %q", tc.v, got, tc.want)
		}
	}
}

// TestRecExcludeEnv: DASH_RECORDS_EXCLUDE is a comma list; unset keeps
// the default test owners, set but empty counts everyone.
func TestRecExcludeEnv(t *testing.T) {
	if got := recordsExcludeNames(); len(got) != 2 || got[0] != "conformance" || got[1] != "test-consumer" {
		t.Fatalf("default = %q, want conformance,test-consumer", got)
	}
	t.Setenv("DASH_RECORDS_EXCLUDE", "ci-runner, honey-svc ,")
	got := recordsExcludeNames()
	if len(got) != 2 || got[0] != "ci-runner" || got[1] != "honey-svc" {
		t.Fatalf("custom = %q, want ci-runner,honey-svc", got)
	}
	t.Setenv("DASH_RECORDS_EXCLUDE", "")
	if got := recordsExcludeNames(); len(got) != 0 {
		t.Fatalf("empty = %q, want no exclusions", got)
	}
}
