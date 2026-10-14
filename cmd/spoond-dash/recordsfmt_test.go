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

// recSample is a records value with one of everything, for the drawn-
// text tests: every record set, and a known first lease.
func recSample() records {
	at := func(d int, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, pacific) }
	return records{
		Avail:    true,
		BusiestN: 11, BusiestAt: at(12, 1, 28),
		OneImageImg: "honey-go-worker", OneImageN: 8, OneImageAt: at(9, 23, 15),
		LongestDur: 21*time.Hour + 19*time.Minute, LongestImg: "honey-go-worker",
		LongestCreated: at(10, 8, 0), LongestEnd: at(11, 5, 19),
		ShortestDur: 2 * time.Second, ShortestImg: "honey-go-worker",
		ShortestCreated: at(9, 16, 31), ShortestEnd: at(9, 16, 33),
		TopOwner: "honey-svc", TopOwnerN: 468,
		LifetimeN: 589, LifetimeHrs: 618, LifetimeGiBH: 1620,
		BusyDayN: 164, BusyDay: at(9, 0, 0),
		FleetOwner: "honey", FleetN: 6, FleetAt: at(12, 0, 26),
		MostMemMiB: 3990, MostMemImg: "honey-go-worker", MostMemEnd: at(13, 9, 0),
		NoLossDays: 1, LastLoss: at(12, 3, 15),
		LongestNap: 5 * time.Hour, LongestNapImg: "honey-go-worker",
		FastestMs: 41, FastestImg: "honey-go-worker", FastestEnd: at(13, 10, 0),
		FirstCreated: at(9, 0, 0),
	}
}

// TestRecordValuesFull: the drawn values with every record set — the
// twelve labels in the approved render's order and each value's
// "value · image · date" shape.
func TestRecordValuesFull(t *testing.T) {
	rows := recordValues(recSample())
	want := [][4]string{
		{"busiest", "11 at once · 10/12 1:28a", "busiest day", "164 leases · 10/9"},
		{"one image", "honey-go-worker ×8 · 10/9 11:15p", "biggest fleet", "honey · 6 at once · 10/12 12:26a"},
		{"longest run", "21h 19m · honey-go-worker · 10/10", "most memory", "3.9 GiB · honey-go-worker · 10/13"},
		{"shortest run", "2s · honey-go-worker · 10/9 4:31p", "no losses for", "1 day · last 10/12 3:15a"},
		{"top owner", "honey-svc · 468 leases", "longest nap", "5h 0m · honey-go-worker"},
		{"lifetime", "589 leases · 618 h · 1.6k GiB·h", "fastest start", "41 ms · honey-go-worker · 10/13"},
	}
	if len(rows) != len(want) {
		t.Fatalf("recordValues drew %d rows, want %d", len(rows), len(want))
	}
	for i, w := range want {
		for side, wl := range [][2]string{{w[0], w[1]}, {w[2], w[3]}} {
			label, value := rows[i][side].label, rows[i][side].value
			if label != wl[0] || value != wl[1] {
				t.Errorf("row %d col %d = %q · %q, want %q · %q", i, side, label, value, wl[0], wl[1])
			}
		}
	}
}

// TestRecordValuesPlaceholders: the values a fresh history draws — zero
// start_ms and max_nap_secs show the "— (counting from 3.8)" place-
// holder, no lost row shows "no losses yet", and the other records fall
// back to the dash.
func TestRecordValuesPlaceholders(t *testing.T) {
	r := records{Avail: true, FirstCreated: time.Date(2026, 10, 9, 0, 0, 0, 0, pacific)}
	rows := recordValues(r)
	byLabel := map[string]string{}
	for _, pair := range rows {
		byLabel[pair[0].label] = pair[0].value
		byLabel[pair[1].label] = pair[1].value
	}
	for label, want := range map[string]string{
		"longest nap":   countingText,
		"fastest start": countingText,
		"no losses for": recNoLossYet,
		"busiest":       recDash,
		"lifetime":      recDash,
		"most memory":   recDash,
	} {
		if got := byLabel[label]; got != want {
			t.Errorf("%s = %q, want %q", label, got, want)
		}
	}
}

// TestRecFooter: the footer's two clauses — "since <first created
// M/D>", with " · test leases not counted" while the exclude list
// holds names and without it when the list is empty.
func TestRecFooter(t *testing.T) {
	rec := recSample()
	if got, want := recFooter(rec, []string{"conformance"}), "since 10/9 · test leases not counted"; got != want {
		t.Errorf("recFooter = %q, want %q", got, want)
	}
	if got, want := recFooter(rec, nil), "since 10/9"; got != want {
		t.Errorf("recFooter = %q, want %q (no exclusions, no clause)", got, want)
	}
}
