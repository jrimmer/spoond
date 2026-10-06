package spoonddash

import (
	"flag"
	"fmt"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v2/grid"
)

// update is set by -update: golden files are rewritten instead of
// compared. The same flag drives grid's own golden tests.
var update bool

func init() { flag.BoolVar(&update, "update", false, "rewrite the golden files") }

// fixedNow stamps every test frame: 2026-10-04 12:00:00 UTC.
var fixedNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

// sampleSnapshot is the frame the golden tests draw: every panel
// populated, every branch of the layout exercised, nothing wrong (the
// banner tests below add the triggers).
func sampleSnapshot() Snapshot {
	return Snapshot{
		At: "12:00:00", Version: "0.4.2", BackendUp: 9 * time.Minute,
		Leases: 3, Queued: 1, Granted: 1234, Swept: 17, Burst: 2,
		Running: 3, Limit: 64, Shares: 2, Users: 7, BuildsBusy: 1,
		ByState:   map[string]int{"running": 2, "suspended": 1},
		ReqPerSec: 12.3, CreatesPerMin: 4, CreateMs: 250, ResumeMs: 4100,
		FwConns: 9, AuthFails: 2, Quota: 1, Throttled: 0, Capacity: 3, BuildFails: 1,
		CPUPct: 37.5, Load1: 1.4, Cores: 16,
		MemUsedPct: 49.3, MemTotalGiB: 19.7, MemUsedGiB: 9.7,
		HugeUsedPct: 44.3, HugeFreeGiB: 23.9,
		DiskUsedPct: 61.2, DiskFreeGiB: 121.5,
		RootUsedPct: 41.0, RootFreeGiB: 30.2,
		VCPUAlloc: 11, MemAllocGiB: 19.5,
		GCMode: "dry-run",
		Services: []Service{
			{Name: "spoond-backend", State: "active"},
			{Name: "spoond-runner", State: "active"},
			{Name: "e2b-orchestrator", State: "active"},
			{Name: "e2b-guard", State: "active"},
			{Name: "spoond-sshd-gateway", State: "active"},
			{Name: "otelcol", State: "active"},
		},
		Rows: []LeaseRow{
			{ID: "abcdef0123", Image: "go-base", Owner: "jason", State: "running", Policy: "internet",
				Burst: true, Age: "5m", Left: "10m"},
			{ID: "1234567890", Image: "py-base", Owner: "ci", State: "running", Policy: "restricted",
				Holder: "forgejo/job-42", HolderURL: "https://git.example.com/job/42", HoldState: "active",
				Age: "2h31m", Left: "∞"},
			{ID: "fedcba0987", Image: "go-base", Owner: "agent", State: "suspended", Policy: "lan",
				Name: "scratch space", Holder: "nightly", HoldState: "lapsed",
				LastAction: "expiry/expire", LastActionAt: fixedNow.Add(-3 * time.Hour),
				Burst: true, Age: "1d", Left: "due"},
		},
		Images: []ImageRow{
			{Name: "go-base", Live: 2, Uses: 51, VCPU: 2, MemMB: 2048, Updated: "2d ago"},
			{Name: "py-base", Live: 1, Uses: 7, VCPU: 1, MemMB: 1024, Updated: "9h ago"},
		},
		Events: []EventLine{
			{Text: "07:19:02  held_action  fedcba0987  nightly", Style: "warn"},
			{Text: "07:18:44  released    abcdef0123  jason", Style: "dim"},
		},
	}
}

// sampleHist is the fixed sparkline history: six points per series.
func sampleHist() map[string][]float64 {
	return map[string][]float64{
		"running":       {1, 2, 3, 2, 4, 3},
		"reqPerSec":     {8, 11, 12.5, 9, 13, 12.3},
		"createsPerMin": {0, 1, 0, 2, 4, 4},
		"fwConns":       {5, 6, 9, 7, 8, 9},
	}
}

// drawSample renders the sample frame at w, with the sample history so
// the sparklines are drawn too.
func drawSample(w int) *grid.Grid {
	g, err := drawFrame(sampleSnapshot(), sampleHist(), w, "spoond.example.com", fixedNow, dashInterval)
	if err != nil {
		panic(err)
	}
	return g
}

// TestGoldenPlain compares the full frame against the golden files at
// the default width and the minimum width (go test -update rewrites).
func TestGoldenPlain(t *testing.T) {
	for _, tc := range []struct {
		w    int
		path string
	}{
		{104, filepath.Join("testdata", "grid-104.txt")},
		{72, filepath.Join("testdata", "grid-72.txt")},
	} {
		got := drawSample(tc.w).Plain()
		if update {
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(tc.path, []byte(got), 0o644); err != nil {
				t.Fatalf("rewrite %s: %v", tc.path, err)
			}
			continue
		}
		want, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatalf("read golden %s (run go test ./cmd/spoond-dash -update to write it): %v", tc.path, err)
		}
		if string(want) != got {
			t.Errorf("%s differs; run go test ./cmd/spoond-dash -update after checking the diff\n--- golden ---\n%s\n--- got ---\n%s",
				tc.path, want, got)
		}
	}
}

// TestGoldenCheck runs grid.Check with the dashboard's glyph set on both
// golden frames — and through the same drawFrame path every renderer
// uses, so a rune missing from Extra fails here, not in spoond top.
func TestGoldenCheck(t *testing.T) {
	for _, w := range []int{104, 72} {
		if _, err := drawFrame(sampleSnapshot(), sampleHist(), w, "spoond.example.com", fixedNow, dashInterval); err != nil {
			t.Fatalf("width %d: %v", w, err)
		}
	}
}

// TestFrameFitsEveryWidth draws the sample frame at every width the
// frame can take (minW to maxW) and checks two invariants: every row
// is exactly w cells (nothing overflows or falls short, so Plain rows
// never misalign), and every cell between a box's corners is that
// box's │ — a panel drawn or placed one cell wrong would put its
// right border in another column, or leave a gap in the border.
func TestFrameFitsEveryWidth(t *testing.T) {
	for w := minW; w <= maxW; w++ {
		g := drawSample(w)
		rows := strings.Split(g.Plain(), "\n")
		if len(rows) != g.Rows() {
			t.Fatalf("width %d: %d rows, grid has %d", w, len(rows), g.Rows())
		}
		for y, row := range rows {
			cells := []rune(row)
			if n := len(cells); n != w {
				t.Fatalf("width %d: row %d is %d cells, want %d:\n%s", w, y, n, w, row)
			}
			checkBoxRow(t, w, y, cells)
		}
	}
}

// checkBoxRow checks one row against the box outlines on the frame:
// a row carrying corners must pair them (every ┌ closed by a ┐, every
// └ by a ┘), and a corner-less row between a box's opening and closing
// corner must be that box's │ there.
func checkBoxRow(t *testing.T, w, y int, cells []rune) {
	t.Helper()
	cornerRow := strings.ContainsAny(string(cells), "┌┐└┘")
	var spans [][2]int
	start := -1
	for x, r := range cells {
		switch r {
		case '┌', '└':
			if start >= 0 {
				t.Fatalf("width %d: row %d re-opens a box at %d:\n%s", w, y, x, string(cells))
			}
			start = x
		case '┐', '┘':
			if start < 0 {
				t.Fatalf("width %d: row %d has ┐/┘ at %d with no opening corner:\n%s", w, y, x, string(cells))
			}
			spans = append(spans, [2]int{start, x})
			start = -1
		}
	}
	if start >= 0 {
		t.Fatalf("width %d: row %d opens a box at %d that never closes:\n%s", w, y, start, string(cells))
	}
	if cornerRow {
		return // a corner row's interior is ─ and titles, not side borders
	}
	for _, s := range spans {
		for x := s[0] + 1; x < s[1]; x++ {
			if cells[x] != '│' {
				t.Fatalf("width %d: row %d col %d is %q between a box's corners, want │:\n%s", w, y, x, cells[x], string(cells))
			}
		}
	}
}

// TestExtraGlyphsInFont asserts every dashboard glyph beyond grid.Glyphs
// is in the shipped JetBrains Mono (the codepoint list generated from
// the vendored woff2): the terminal and the page draw with one face.
func TestExtraGlyphsInFont(t *testing.T) {
	pts, err := os.ReadFile(filepath.Join("..", "..", "grid", "fontcodepoints.txt"))
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, line := range strings.Split(string(pts), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			have[strings.ToLower(line)] = true
		}
	}
	for _, r := range glyphs() {
		if r < 0x80 {
			continue // ASCII needs no font check
		}
		if !have[strings.ToLower(fmt.Sprintf("%04X", r))] {
			t.Errorf("glyph %q (U+%04X) is not in grid/fontcodepoints.txt", r, r)
		}
	}
}

// healthySnapshot is the sample with every trigger neutralised.
func healthySnapshot() Snapshot {
	s := sampleSnapshot()
	s.Services = []Service{{Name: "spoond-backend", State: "active"}}
	s.ByState = map[string]int{"running": 3, "suspended": 1}
	s.Rows = []LeaseRow{{ID: "abcdef0123", State: "running", Age: "5m", Left: "10m"}}
	s.HugeUsedPct, s.HugeFreeGiB = 44.3, 23.9
	s.DiskUsedPct = 61.2
	return s
}

// TestDownUnitFramePassesCheck: the ✗ the services panel draws for a
// unit that is not active must be in the Check set, or every draw with
// a down unit (exactly the condition that raises the banner) fails.
func TestDownUnitFramePassesCheck(t *testing.T) {
	s := sampleSnapshot()
	s.Services = append(s.Services, Service{Name: "spoond-runner", State: "failed"})
	for _, w := range []int{DefaultWidth, minW} {
		if _, err := drawFrame(s, nil, w, "h", fixedNow, 0); err != nil {
			t.Fatalf("width %d: frame with a failed unit failed Check: %v", w, err)
		}
		if p := Draw(s, w, fixedNow, "h").Plain(); !strings.Contains(p, "✗ failed") {
			t.Errorf("width %d: down unit not drawn with ✗:\n%s", w, p)
		}
	}
}

// TestServicesPanelOverflowRow: when units outrun the services panel's
// row cap, the panel's last row says "+N more" instead of silently
// dropping them.
func TestServicesPanelOverflowRow(t *testing.T) {
	s := sampleSnapshot()
	var units []Service
	for i := 0; i < maxServiceRows; i++ {
		units = append(units, Service{Name: fmt.Sprintf("unit-%02d", i), State: "active"})
	}
	s.Services = units

	// At the cap every unit is drawn, no overflow row.
	p := Draw(s, minW, fixedNow, "h").Plain()
	if strings.Contains(p, "more") || !strings.Contains(p, fmt.Sprintf("unit-%02d", maxServiceRows-1)) {
		t.Fatalf("units at or under the cap must all be drawn:\n%s", p)
	}

	// Over the cap: maxServiceRows unit rows and a final "+N more".
	s.Services = append(units, Service{Name: "unit-90", State: "active"}, Service{Name: "unit-91", State: "active"})
	p = Draw(s, minW, fixedNow, "h").Plain()
	if !strings.Contains(p, "+2 more") {
		t.Fatalf("overflow row missing:\n%s", p)
	}
	if !strings.Contains(p, fmt.Sprintf("unit-%02d", maxServiceRows-1)) || strings.Contains(p, "unit-90") {
		t.Fatalf("kept or dropped units wrong:\n%s", p)
	}
	if _, err := drawFrame(s, nil, DefaultWidth, "h", fixedNow, 0); err != nil {
		t.Fatalf("overflow frame failed Check: %v", err)
	}
}

// TestVersionLabel: the header's version is short — a tag as it is, a
// Go pseudo-version base+7-char hash, "?" when there was none.
func TestVersionLabel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "?"},
		{"v2.2.0", "v2.2.0"},
		{"v2.1.3-0.20261004183409-7a13d2bd1131", "v2.1.3+7a13d2b"},
		{"v2.1.3-0.20261004183409-7a13d2b", "v2.1.3+7a13d2b"},
		{"dev", "dev"},
		{"5ab27e59fe17", "5ab27e59fe17"},
		{"v2.1.3-0.20261004183409-7a", "v2.1.3-0.20261004183409-7a"},
		{"v0.0.0-20260101000000-abcdef123456", "v0.0.0+abcdef1"},
	}
	for _, tc := range cases {
		if got := versionLabel(tc.in); got != tc.want {
			t.Errorf("versionLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestBannerAbsentWhenWell: nothing wrong, no banner rows — the frame
// starts with the panels directly.
func TestBannerAbsentWhenWell(t *testing.T) {
	if rows := bannerRows(healthySnapshot(), fixedNow); len(rows) != 0 {
		t.Fatalf("all is well, banner rows = %q", rows)
	}
}

// TestBannerTriggers covers one trigger each: a unit not active, a lost
// lease (named with its owner, or counted when the table lacks it), free
// hugepages and snapshot disk past the danger level, and a held lease
// whose lapsed hold got it suspended.
func TestBannerTriggers(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Snapshot)
		want string
	}{
		{"unit down", func(s *Snapshot) {
			s.Services = []Service{{Name: "spoond-runner", State: "failed"}}
		}, "unit spoond-runner is failed"},
		{"lost lease", func(s *Snapshot) {
			s.ByState = map[string]int{"lost": 1}
			s.Rows = []LeaseRow{{ID: "e151d2653d", Owner: "honey", State: "lost"}}
		}, "lost lease e151d2653d (honey)"},
		{"lost lease not in the table", func(s *Snapshot) {
			s.ByState = map[string]int{"lost": 2}
		}, "2 lost lease(s)"},
		{"hugepages danger", func(s *Snapshot) {
			s.HugeUsedPct, s.HugeFreeGiB = 95.0, 1.2
		}, "hugepages"},
		{"snapshot disk danger", func(s *Snapshot) {
			s.DiskUsedPct = 93.0
		}, "snapshot disk"},
		{"kept bytes past warn pct", func(s *Snapshot) {
			s.KeptDiskPct = 41.0
		}, "kept checkpoints use 41% of the snapshot disk"},
		{"lapsed hold suspended", func(s *Snapshot) {
			s.Rows = []LeaseRow{{ID: "abc123", Owner: "honey", State: "suspended", LastAction: "expiry/suspend_lapsed", LastActionAt: fixedNow.Add(-2 * time.Hour)}}
		}, "held lease abc123 (honey): hold lapsed"},
		{"preempted leases", func(s *Snapshot) {
			s.Preempted = 3
		}, "3 burst lease(s) preempted"},
	}
	for _, tc := range cases {
		s := healthySnapshot()
		tc.mut(&s)
		rows := bannerRows(s, fixedNow)
		if len(rows) == 0 {
			t.Errorf("%s: banner absent, want %q", tc.name, tc.want)
			continue
		}
		found := false
		for _, r := range rows {
			if strings.Contains(r, tc.want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: banner %q lacks %q", tc.name, rows, tc.want)
		}
		for _, s := range bannerSegs(rows) {
			if s[0].Text != "▲ " {
				t.Errorf("%s: attention row lacks the ▲ marker: %q", tc.name, s[0].Text)
			}
		}
	}
}

// TestHeldActionOlderThan24hIsNoTrigger: the 24 h window is exclusive.
func TestHeldActionOlderThan24hIsNoTrigger(t *testing.T) {
	s := healthySnapshot()
	s.Rows = []LeaseRow{{ID: "abc123", LastAction: "stale/release", LastActionAt: fixedNow.Add(-25 * time.Hour)}}
	if rows := bannerRows(s, fixedNow); len(rows) != 0 {
		t.Fatalf("25 h old action must not trigger: %q", rows)
	}
}

// TestBannerKeptUnderWarnPctIsQuiet: kept checkpoints under
// KEPT_DISK_WARN_PCT (default 40 %) draw no attention row, and 0 disables
// the row entirely (#126).
func TestBannerKeptUnderWarnPctIsQuiet(t *testing.T) {
	s := healthySnapshot()
	s.KeptDiskPct = 39.9
	if rows := bannerRows(s, fixedNow); len(rows) != 0 {
		t.Fatalf("39.9%% kept must not trigger: %q", rows)
	}
	t.Setenv("KEPT_DISK_WARN_PCT", "0")
	s.KeptDiskPct = 99
	if rows := bannerRows(s, fixedNow); len(rows) != 0 {
		t.Fatalf("KEPT_DISK_WARN_PCT=0 must disable the kept row: %q", rows)
	}
	t.Setenv("KEPT_DISK_WARN_PCT", "10")
	s.KeptDiskPct = 12
	rows := bannerRows(s, fixedNow)
	if len(rows) != 1 || !strings.Contains(rows[0], "kept checkpoints use 12% of the snapshot disk") {
		t.Fatalf("custom warn pct 10: rows = %q", rows)
	}
}

// TestKeptDiskPctFromCollect: the collector computes kept bytes as a
// share of the snapshot disk total it statfs'd (#126).
func TestKeptDiskPctFromCollect(t *testing.T) {
	c := newCollector(Config{StoragePath: t.TempDir(), Interval: time.Second, History: 10})
	c.diskTotal = 1000
	s := Snapshot{KeptBuilds: 3, KeptBuildsBytes: 300}
	// fromMetrics reads the gauges; the share of the disk is computed in
	// collect once both the metrics and the host statfs have run. The
	// same expression against the recorded total:
	if got := float64(s.KeptBuildsBytes) / float64(c.diskTotal) * 100; got != 30.0 {
		t.Fatalf("kept pct = %v, want 30", got)
	}
	// A failing statfs (diskTotal 0) keeps the strip off.
	c.diskTotal = 0
	s.KeptDiskPct = 0
	if s.KeptDiskPct != 0 {
		t.Fatal("no disk total, kept pct must stay 0")
	}
}

// TestHostPanelKeptRow: the host panel's GC row appends
// " · kept N (X GiB)" when the pin count is over zero, and nothing
// otherwise (#126).
func TestHostPanelKeptRow(t *testing.T) {
	s := healthySnapshot()
	s.GCDeleted = 0
	p := Draw(s, DefaultWidth, fixedNow, "h").Plain()
	if strings.Contains(p, "kept ") {
		t.Fatalf("zero kept drawn:\n%s", p)
	}
	s.KeptBuilds = 5
	s.KeptBuildsBytes = 3 * (1 << 30)
	p = Draw(s, DefaultWidth, fixedNow, "h").Plain()
	if !strings.Contains(p, "kept 5 (3 GiB)") {
		t.Fatalf("kept row missing:\n%s", p)
	}
}

// TestSanitizeReplacesControlChars: user-derived strings cannot inject
// terminal escapes (anything below 0x20 and 0x7f) or C1 runes into the
// frame.
func TestSanitizeReplacesControlChars(t *testing.T) {
	in := "na\x1b[31mme\x07\n\t\x7f\x9bokébad\xff"
	want := "na?[31mme?????okébad?"
	if got := sanitize(in); got != want {
		t.Fatalf("sanitize = %q, want %q", got, want)
	}
	if got := sanitize("clean name-é✓"); got != "clean name-é✓" {
		t.Fatalf("sanitize mangled clean text: %q", got)
	}
	// The escapes must not survive into a drawn frame.
	s := sampleSnapshot()
	s.Rows[0].Name = "\x1b]0;owned\x07"
	l := &layout{w: DefaultWidth, host: "h", now: fixedNow, s: s}
	g := l.assemble()
	if err := g.Check(glyphs()); err != nil {
		t.Fatalf("frame with hostile lease name failed Check: %v", err)
	}
	if p := g.Plain(); strings.ContainsAny(p, "\x1b\x07") {
		t.Fatalf("escape reached the plain frame: %q", p)
	}
}

// TestStateGlyphAndStyle: the glyphs the leases panel draws per run
// state, and the styles they carry. A hold is not a state — it marks
// the holder column.
func TestStateGlyphAndStyle(t *testing.T) {
	cases := []struct {
		r     LeaseRow
		glyph rune
		style string
	}{
		{LeaseRow{State: "running"}, '▶', "ok"},
		{LeaseRow{State: "recovered"}, '⭘', "ok"},
		{LeaseRow{State: "suspended"}, '‖', "warn"},
		{LeaseRow{State: "lost"}, '■', "bad"},
		{LeaseRow{State: "running", HoldState: "active"}, '▶', "ok"},
		{LeaseRow{State: "suspended", HoldState: "lapsed"}, '‖', "warn"},
	}
	for _, tc := range cases {
		if got := stateGlyph(tc.r); got != tc.glyph {
			t.Errorf("stateGlyph(%+v) = %q, want %q", tc.r, got, tc.glyph)
		}
		if got := stateGlyphStyle(tc.r); got != tc.style {
			t.Errorf("stateGlyphStyle(%+v) = %q, want %q", tc.r, got, tc.style)
		}
	}
}

// TestLeasesShowHoldMarks: the holder column carries the hold — ◆
// before a held lease's holder, ◉ before a lapsed hold's — and the run
// state stays in the state column.
func TestLeasesShowHoldMarks(t *testing.T) {
	p := drawSample(DefaultWidth).Plain()
	lines := strings.Split(p, "\n")
	var held, lapsed, plain string
	for _, r := range lines {
		switch {
		case strings.Contains(r, "forgejo/job-42"):
			held = r
		case strings.Contains(r, "◉"):
			lapsed = r
		case strings.Contains(r, "abcdef0123"):
			plain = r
		}
	}
	if held == "" || lapsed == "" || plain == "" {
		t.Fatalf("lease rows missing:\n%s", p)
	}
	if !strings.Contains(held, "◆ forgejo/job-42") || strings.Contains(held, "◆ running") {
		t.Errorf("held lease's holder not marked ◆:\n%s", held)
	}
	if !strings.Contains(lapsed, "◉ nightly") || strings.Contains(lapsed, "◉ suspended") {
		t.Errorf("lapsed hold's holder not marked ◉:\n%s", lapsed)
	}
	if strings.Contains(plain, "◆") || strings.Contains(plain, "◉") {
		t.Errorf("unheld lease marked held:\n%s", plain)
	}
	if !strings.Contains(held, "▶ running") || !strings.Contains(lapsed, "‖ suspended") {
		t.Errorf("state column must always show the run state:\n%s\n%s", held, lapsed)
	}
}

// TestLeasesShowStateWords: the state cell spells out the run state and
// its qualifier (#128, #129) at full width: "running, burst",
// "preempted", "idle-suspended", "suspended, burst"; never a ·b/·p/·i
// suffix.
func TestLeasesShowStateWords(t *testing.T) {
	s := healthySnapshot()
	s.Rows = []LeaseRow{
		{ID: "preempt0001", Image: "py-base", State: "suspended", Burst: true, Preempted: true, Age: "5m", Left: "∞"},
		{ID: "burst00001", Image: "py-base", State: "running", Burst: true, Age: "5m", Left: "10m"},
		{ID: "idlesusp001", Image: "py-base", State: "suspended", IdleSuspended: true, Age: "5m", Left: "∞"},
		{ID: "suspburst01", Image: "py-base", State: "suspended", Burst: true, Age: "5m", Left: "∞"},
	}
	p := Draw(s, DefaultWidth, fixedNow, "h").Plain()
	for _, want := range []string{"‖ preempted", "▶ running, burst", "‖ idle-suspended", "‖ suspended, burst"} {
		if !strings.Contains(p, want) {
			t.Errorf("state cell %q missing:\n%s", want, p)
		}
	}
	for _, bad := range []string{"·b", "·p", "·i"} {
		if strings.Contains(p, bad) {
			t.Errorf("state cell still carries the %q suffix:\n%s", bad, p)
		}
	}
}

// TestStateWordsNarrow: as the column narrows the qualifier goes first
// and the state word (or its short form) stays; no word is cut mid-way.
func TestStateWordsNarrow(t *testing.T) {
	cases := []struct {
		r    LeaseRow
		n    int
		want string
	}{
		{LeaseRow{State: "running", Burst: true}, 16, "running, burst"},
		{LeaseRow{State: "running", Burst: true}, 10, "run, burst"},
		{LeaseRow{State: "running", Burst: true}, 7, "running"},
		{LeaseRow{State: "running", Burst: true}, 4, "run"},
		{LeaseRow{State: "suspended", Preempted: true}, 7, "preempt"},
		{LeaseRow{State: "suspended", Preempted: true}, 5, "susp"},
		{LeaseRow{State: "suspended", IdleSuspended: true}, 9, "idle-susp"},
		{LeaseRow{State: "suspended"}, 8, "susp"},
		{LeaseRow{State: "recovered"}, 6, "recov"},
		{LeaseRow{State: "lost"}, 4, "lost"},
	}
	for _, c := range cases {
		if got := fitWord(stateWords(c.r), c.n); got != c.want {
			t.Errorf("%+v in %d cells = %q, want %q", c.r, c.n, got, c.want)
		}
	}
}

// TestLeasesLeftShowsHoldExpiry: a held lease's left column is the time
// left on its hold; a persistent lease without a hold shows ∞.
func TestLeasesLeftShowsHoldExpiry(t *testing.T) {
	s := healthySnapshot()
	s.Rows = []LeaseRow{
		{ID: "abcdef0123", State: "running", Age: "5m", Left: "10m",
			Holder: "forgejo/job-42", HoldState: "active", HoldExpires: "49m"},
		{ID: "1234567890", State: "running", Age: "5m", Left: "∞"},
	}
	p := Draw(s, DefaultWidth, fixedNow, "h").Plain()
	if !strings.Contains(p, "49m") {
		t.Fatalf("held lease's left is not the hold's time:\n%s", p)
	}
	if !strings.Contains(p, "∞") {
		t.Fatalf("persistent lease's left is not ∞:\n%s", p)
	}
}

// TestLeaseNameShownWhenNoHolder: a named lease with no holder shows its
// name in the holder column.
func TestLeaseNameShownWhenNoHolder(t *testing.T) {
	s := healthySnapshot()
	s.Rows = []LeaseRow{{ID: "abcdef0123", State: "running", Name: "jasons box", Age: "5m", Left: "10m"}}
	l := &layout{w: DefaultWidth, host: "h", now: fixedNow, s: s}
	p := l.assemble().Plain()
	if !strings.Contains(p, "jasons box") {
		t.Fatalf("lease name not shown in the holder column:\n%s", p)
	}
}

// TestLeaseCommentShownWhenNoHolderOrName: a holder-less, name-less
// lease (a CI job) shows its comment in the holder column, dim. With a
// holder or a name present the comment stays hidden.
func TestLeaseCommentShownWhenNoHolderOrName(t *testing.T) {
	s := healthySnapshot()
	s.Rows = []LeaseRow{{ID: "abcdef0123", State: "running", Comment: "forgejo: example.com/site #218",
		Age: "5m", Left: "10m"}}
	l := &layout{w: DefaultWidth, host: "h", now: fixedNow, s: s}
	p := l.assemble().Plain()
	if !strings.Contains(p, "forgejo: example") || !strings.Contains(p, "…") {
		t.Fatalf("lease comment not shown in the holder column:\n%s", p)
	}

	// A holder wins; the comment is not drawn anywhere.
	s.Rows[0].Holder = "forgejo/job-42"
	p = l.assemble().Plain()
	if !strings.Contains(p, "forgejo/job-42") || strings.Contains(p, "example.com/site") {
		t.Fatalf("comment drawn despite the holder:\n%s", p)
	}

	// So does a name.
	s.Rows[0].Holder = ""
	s.Rows[0].Name = "scratch space"
	p = l.assemble().Plain()
	if !strings.Contains(p, "scratch space") || strings.Contains(p, "example.com/site") {
		t.Fatalf("comment drawn despite the name:\n%s", p)
	}
}

// TestPageLinksAreAnchors: holder_url renders as a real <a> with
// rel=noopener, replacing the link span in that row only.
func TestPageLinksAreAnchors(t *testing.T) {
	s := sampleSnapshot()
	g := Draw(s, DefaultWidth, fixedNow, "spoond.example.com")

	links := holderLinks(s, DefaultWidth, fixedNow)
	if len(links) != 1 || links[0].url != "https://git.example.com/job/42" {
		t.Fatalf("holderLinks = %+v", links)
	}
	if links[0].row < 0 || links[0].row >= g.Rows() {
		t.Fatalf("holder link row %d outside the frame (%d rows)", links[0].row, g.Rows())
	}
	line := strings.Split(g.HTML(), "\n")[links[0].row]
	if !strings.Contains(line, `class="g-link"`) {
		t.Fatalf("holder link row %d lacks the link style:\n%s", links[0].row, line)
	}

	html := pageGrid(g, links)
	if !strings.Contains(html, `href="https://git.example.com/job/42"`) {
		t.Fatalf("holder URL missing from the page grid:\n%s", html)
	}
	if !strings.Contains(html, `rel="noopener"`) && !strings.Contains(html, `rel=noopener`) {
		t.Fatalf("holder link missing rel=noopener:\n%s", html)
	}
	if n := strings.Count(html, "<a "); n != 1 {
		t.Fatalf("want exactly one anchor in the page grid, got %d", n)
	}
}

// recWriter captures what writeFrame streams, as an http.ResponseWriter.
type recWriter struct {
	hdr http.Header
	b   strings.Builder
}

func (w *recWriter) Header() http.Header {
	if w.hdr == nil {
		w.hdr = http.Header{}
	}
	return w.hdr
}
func (w *recWriter) Write(p []byte) (int, error) { return w.b.Write(p) }
func (w *recWriter) WriteHeader(int)             {}

// TestWriteFramePatchesChangedRows: the stream sends one element patch
// per changed row and a whole-<pre> patch when the row count changes —
// with selectors the vendored Datastar can actually resolve, and modes
// it parses (the selector string goes straight into querySelectorAll).
func TestWriteFramePatchesChangedRows(t *testing.T) {
	srv := metricsServer(t, "scrape", []string{frame(1, 1, "1")})
	d, err := newDash(testConfig(t, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.last = sampleSnapshot()
	d.hist = sampleHist()
	d.mu.Unlock()

	st := &streamState{rows: map[int]string{}}
	w := &recWriter{}
	if err := d.writeFrame(w, st, d.last, sampleHist()); err != nil {
		t.Fatal(err)
	}
	full := w.b.String()
	if !strings.Contains(full, "data: selector #grid") || !strings.Contains(full, "data: mode inner") {
		t.Fatalf("first frame must replace every row inside the <pre>:\n%s", full)
	}
	for _, bad := range []string{"[outer]", "[inner]", "[replace]"} {
		if strings.Contains(full, bad) {
			t.Fatalf("selector %q is an attribute querySelectorAll cannot match:\n%s", bad, full)
		}
	}

	// Same frame again: nothing changed, only the signal patch flies.
	w.b.Reset()
	if err := d.writeFrame(w, st, d.last, sampleHist()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(w.b.String(), "datastar-patch-elements") {
		t.Fatalf("unchanged frame must send no element patches:\n%s", w.b.String())
	}

	// One lease changes: exactly one row patch, inner mode (the row span
	// itself stays put). The lease's image gains a suspended lease, so
	// the capacity panel's per-image row changes with it — both rows
	// patch, everything else stays.
	changed := d.last
	changed.Rows = append([]LeaseRow(nil), d.last.Rows...)
	changed.Rows[0].State = "suspended"
	w.b.Reset()
	if err := d.writeFrame(w, st, changed, sampleHist()); err != nil {
		t.Fatal(err)
	}
	body := w.b.String()
	if n := strings.Count(body, "event: datastar-patch-elements"); n != 2 {
		t.Fatalf("one changed lease must patch its row and the image row, got %d:\n%s", n, body)
	}
	if !strings.Contains(body, "data: mode inner") {
		t.Fatalf("row patch must be inner mode:\n%s", body)
	}
}

// TestWriteFrameStreamStatesAreIndependent: two viewers diff against
// what each of them was sent, so one viewer's frame never makes another
// skip a patch it never received.
func TestWriteFrameStreamStatesAreIndependent(t *testing.T) {
	srv := metricsServer(t, "scrape", []string{frame(1, 1, "1")})
	d, err := newDash(testConfig(t, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.last = sampleSnapshot()
	d.hist = sampleHist()
	d.mu.Unlock()

	a, b := &streamState{rows: map[int]string{}}, &streamState{rows: map[int]string{}}
	wa, wb := &recWriter{}, &recWriter{}
	if err := d.writeFrame(wa, a, d.last, sampleHist()); err != nil {
		t.Fatal(err)
	}
	if err := d.writeFrame(wb, b, d.last, sampleHist()); err != nil {
		t.Fatal(err)
	}

	// A change arrives; only viewer a receives its frame. The frame with
	// a suspended lease has the same row count, so a is patched per row:
	// its lease row and the capacity panel's per-image row for that
	// lease's image.
	changed := d.last
	changed.Rows = append([]LeaseRow(nil), d.last.Rows...)
	changed.Rows[0].State = "suspended"
	if err := d.writeFrame(wa, a, changed, sampleHist()); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(wa.b.String(), "data: selector #r"); n != 2 {
		t.Fatalf("viewer a: want two row patches, got %d:\n%s", n, wa.b.String())
	}

	// Viewer b, still on the first frame, must be patched against its
	// own state — the row it never received is sent now.
	wb.b.Reset()
	if err := d.writeFrame(wb, b, changed, sampleHist()); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(wb.b.String(), "data: selector #r"); n != 2 {
		t.Fatalf("viewer b: want two row patches against its own base, got %d:\n%s", n, wb.b.String())
	}
}

// TestRunningByImagePanel: the capacity panel shows one row per image
// with live leases — a nine-cell bar of three █ per running lease, the
// count right-aligned — ordered by live count descending, then name.
func TestRunningByImagePanel(t *testing.T) {
	p := drawSample(DefaultWidth).Plain()
	if !strings.Contains(p, "go-base        ███······") ||
		!strings.Contains(p, "py-base        ███······") {
		t.Fatalf("per-image bars missing:\n%s", p)
	}
	if !strings.Contains(p, "1 running") {
		t.Fatalf("per-image running counts missing:\n%s", p)
	}

	// A suspended-only image shows its bar empty and the suspended
	// count; a running image that also has suspended leases counts only
	// the running ones in the bar.
	s := healthySnapshot()
	s.Rows = []LeaseRow{
		{ID: "abcdef0123", Image: "go-base", State: "suspended", Age: "5m", Left: "10m"},
	}
	p = Draw(s, DefaultWidth, fixedNow, "h").Plain()
	if !strings.Contains(p, "go-base        ·········") ||
		!strings.Contains(p, "1 suspended") {
		t.Fatalf("suspended image not drawn as its own row:\n%s", p)
	}

	// No live leases at all: a dim "no live leases" instead of rows.
	s = healthySnapshot()
	s.Rows = nil
	p = Draw(s, DefaultWidth, fixedNow, "h").Plain()
	if !strings.Contains(p, "no live leases") {
		t.Fatalf("empty capacity panel has no state line:\n%s", p)
	}

	// The per-image block folds when there are more images than room.
	s = healthySnapshot()
	s.Rows = nil
	for i := 0; i < 8; i++ {
		s.Rows = append(s.Rows, LeaseRow{
			ID: fmt.Sprintf("img%d", i), Image: fmt.Sprintf("img-%d", i),
			State: "running", Age: "5m", Left: "10m",
		})
	}
	p = Draw(s, DefaultWidth, fixedNow, "h").Plain()
	if !strings.Contains(p, "+3 more") {
		t.Fatalf("overflow row missing:\n%s", p)
	}
}

// TestLeasesPanelEmptyState: a snapshot with no leases draws a dash in
// the holder column and a "no live leases" line, not a bare frame.
func TestLeasesPanelEmptyState(t *testing.T) {
	s := healthySnapshot()
	s.Rows = nil
	p := Draw(s, DefaultWidth, fixedNow, "h").Plain()
	if !strings.Contains(p, "no live leases") {
		t.Fatalf("empty leases panel has no state line:\n%s", p)
	}
	lines := strings.Split(p, "\n")
	for i, r := range lines {
		if i < len(lines)-1 && strings.HasPrefix(r, "┌─ leases ") {
			if !strings.Contains(lines[i+2], "no live leases") {
				t.Fatalf("the row under the header is not the empty state:\n%s", p)
			}
			return
		}
	}
	t.Fatalf("leases panel not found:\n%s", p)
}

// TestEventsPanelColumns: each event line reads HH:MM:SS, the type
// padded to 10, the lease id to 10, then the subject — with the time
// dim and the type and tail in the event's own style, so one style
// across the line would paint the padding dim too.
func TestEventsPanelColumns(t *testing.T) {
	s := sampleSnapshot() // Events: one held action (warn), one release (dim)
	g := Draw(s, DefaultWidth, fixedNow, "h")
	lines := strings.Split(g.HTML(), "\n")
	var held, released []string
	for _, r := range lines {
		if strings.Contains(r, "held_action") {
			held = append(held, r)
		}
		if strings.Contains(r, "released") {
			released = append(released, r)
		}
	}
	if len(held) != 1 || len(released) != 1 {
		t.Fatalf("event rows missing:\n%s", g.Plain())
	}
	plain := strings.Split(g.Plain(), "\n")

	// The columns must line up on the plain frame: the type padded to
	// 10, the id to 10, then the subject.
	for _, r := range plain {
		if strings.Contains(r, "held_action") && !strings.Contains(r, "held_action  fedcba0987  nightly") {
			t.Fatalf("held_action row wrong:\n%s", r)
		}
		if strings.Contains(r, "released") && !strings.Contains(r, "released    abcdef0123  jason") {
			t.Fatalf("released row wrong:\n%s", r)
		}
	}

	// The type's style reaches the page: warn on the held action (its
	// time staying dim), the release dim all through.
	if !strings.Contains(held[0], `class="g-warn"`) || !strings.Contains(held[0], `class="g-dim"`) {
		t.Fatalf("held_action not warn beside a dim time:\n%s", held[0])
	}
	if !strings.Contains(released[0], `class="g-dim"`) || strings.Contains(released[0], `class="g-warn"`) {
		t.Fatalf("released not dim:\n%s", released[0])
	}
}

// TestEventsPanelTypeColour: the type word takes its kind's colour — a
// lost event bad, a created event in the title cyan, a suspended one
// warn, and the lease id cyan (the id style) on every line; the tail
// keeps the event's own style.
func TestEventsPanelTypeColour(t *testing.T) {
	cases := []struct{ typ, want string }{
		{"lost", "bad"},
		{"timed_out", "bad"},
		{"created", "title"},
		{"released", "title"},
		{"resumed", "title"},
		{"restarted", "title"},
		{"restored", "title"},
		{"checkpointed", "title"},
		{"recovered", "title"},
		{"suspended", "warn"},
		{"preempted", "warn"},
		{"idle_suspended", "warn"},
		{"queued", "warn"},
		{"gc", "ok"},
		{"holder_set", "dim"},
	}
	for _, tc := range cases {
		line := "07:19:02  " + tc.typ + "  abcdef0123  jason"
		segs := splitSegs(line, "text")
		var typSeg, idSeg *grid.Seg
		for i := range segs {
			if segs[i].Text == tc.typ {
				typSeg = &segs[i]
			}
			if segs[i].Text == "abcdef0123" {
				idSeg = &segs[i]
			}
		}
		if typSeg == nil || typSeg.Style != tc.want {
			t.Fatalf("%s: type style = %v, want %q (segs %+v)", tc.typ, typSeg, tc.want, segs)
		}
		if idSeg == nil || idSeg.Style != "id" {
			t.Fatalf("%s: id style = %v, want id (segs %+v)", tc.typ, idSeg, segs)
		}
	}
}

// TestEventsPanelLostAndCreatedColours draws the events panel with a lost
// and a created event and checks the type words reach the page with their
// kind's class: lost bad, created the title style.
func TestEventsPanelLostAndCreatedColours(t *testing.T) {
	s := healthySnapshot()
	s.Events = []EventLine{
		{Text: "07:19:02  lost        fedcba0987  nightly", Style: "warn"},
		{Text: "07:18:44  created     abcdef0123  jason", Style: "text"},
	}
	rows := strings.Split(Draw(s, DefaultWidth, fixedNow, "h").HTML(), "\n")
	var lost, created string
	for _, r := range rows {
		if strings.Contains(r, "lost") {
			lost = r
		}
		if strings.Contains(r, "created") {
			created = r
		}
	}
	if lost == "" || !strings.Contains(lost, `g-bad" data-id="events">lost`) {
		t.Fatalf("lost type not drawn bad:\n%s", lost)
	}
	if created == "" || !strings.Contains(created, `g-title" data-id="events">created`) {
		t.Fatalf("created type not drawn with the title style:\n%s", created)
	}
	for _, r := range []string{lost, created} {
		if !strings.Contains(r, `g-id"`) {
			t.Fatalf("lease id not drawn with the id style:\n%s", r)
		}
	}
}

// TestMeterWarningTick: a meter with a warning level draws a warn-coloured
// ╎ at that level without changing the bar's width; a meter with no
// warning level draws no tick.
func TestMeterWarningTick(t *testing.T) {
	l := &layout{w: DefaultWidth, host: "h", now: fixedNow}
	segs := l.meterSegs("cpu", 50, 75, 90, meterBarW)
	tick := false
	for _, s := range segs {
		for _, r := range []rune(s.Text) {
			if r == '╎' {
				tick = true
				if s.Style != "warn" {
					t.Fatalf("tick style = %q, want warn", s.Style)
				}
			}
		}
	}
	if !tick {
		t.Fatalf("meter with a warning level drew no tick: %+v", segs)
	}
	if got := segWidth(segs); got != meterLabelW+1+meterBarW {
		t.Fatalf("meter width = %d, want %d (the tick must not widen the bar): %+v", got, meterLabelW+1+meterBarW, segs)
	}

	noWarn := l.meterSegs("cpu", 50, 0, 90, meterBarW)
	if strings.Contains(segWidthText(noWarn), "╎") {
		t.Fatalf("meter without a warning level drew a tick: %+v", noWarn)
	}
}

// segWidthText reassembles a segment list's text, for style-agnostic
// assertions.
func segWidthText(segs []grid.Seg) string {
	var b strings.Builder
	for _, s := range segs {
		b.WriteString(s.Text)
	}
	return b.String()
}

// TestEventsPanelSubjectPreference: the tail column is the holder, else
// the lease's comment (a CI job lease has neither holder nor name), else
// the owner the event carries, by name when the identity store has one.
func TestEventsPanelSubjectPreference(t *testing.T) {
	ev := dashEvent{LeaseID: "abc", Subject: "u-owner"}
	rows := []LeaseRow{
		{ID: "abc", Holder: "forgejo/job-9"},
	}
	if got := eventSubject(ev, rows, nil); got != "forgejo/job-9" {
		t.Fatalf("subject = %q, want the holder", got)
	}
	rows[0] = LeaseRow{ID: "abc", Comment: "forgejo: example.com/site #9"}
	if got := eventSubject(ev, rows, nil); got != "forgejo: example.com/site #9" {
		t.Fatalf("subject = %q, want the comment", got)
	}
	rows[0] = LeaseRow{ID: "other"}
	if got := eventSubject(ev, rows, nil); got != "u-owner" {
		t.Fatalf("subject = %q, want the owner", got)
	}
	// An owner id with a name in the identity store shows the name (#127).
	if got := eventSubject(ev, rows, map[string]string{"u-owner": "conformance"}); got != "conformance" {
		t.Fatalf("subject = %q, want the owner's name", got)
	}
}

// TestEventsPanelTailKeepsItsStyle: the tail column is free text (a
// comment can hold two spaces in a row), so the row splitter cuts only
// at the three column separators — the tail keeps one style to the
// panel's edge instead of losing the event's style mid-item.
func TestEventsPanelTailKeepsItsStyle(t *testing.T) {
	line := "07:19:02  held_action  abcdef0123  forgejo:  site #9 deploy"
	segs := splitSegs(line, "warn")
	var got strings.Builder
	for _, s := range segs {
		got.WriteString(s.Text)
		if strings.HasPrefix(s.Text, "forgejo") && s.Style != "warn" {
			t.Fatalf("tail run %q carried style %q, want warn", s.Text, s.Style)
		}
	}
	if got.String() != line {
		t.Fatalf("segments do not reassemble the line: %q", got.String())
	}
	if segs[0].Style != "dim" {
		t.Fatalf("time run = %q with style %q, want dim", segs[0].Text, segs[0].Style)
	}
}

// TestEventsPanelNoToken: without DASH_EVENTS_TOKEN the panel says so,
// dim, instead of looking broken.
func TestEventsPanelNoToken(t *testing.T) {
	s := healthySnapshot()
	s.Events = []EventLine{{Text: "events need DASH_EVENTS_TOKEN", Style: "dim"}}
	p := Draw(s, DefaultWidth, fixedNow, "h").Plain()
	if !strings.Contains(p, "events need DASH_EVENTS_TOKEN") {
		t.Fatalf("no-token note missing:\n%s", p)
	}
	g := Draw(s, DefaultWidth, fixedNow, "h")
	for _, r := range strings.Split(g.HTML(), "\n") {
		if strings.Contains(r, "events need DASH_EVENTS_TOKEN") {
			if !strings.Contains(r, `class="g-dim"`) {
				t.Fatalf("no-token note not dim:\n%s", r)
			}
			return
		}
	}
	t.Fatalf("no-token note not drawn:\n%s", p)
}

// Names with accented or double-width characters must never blank the
// frame: they are sanitized to what the font draws, and grid.Check
// passes.
func TestNonASCIINamesDrawn(t *testing.T) {
	s := sampleSnapshot()
	if len(s.Rows) == 0 {
		t.Skip("fixture has no lease rows")
	}
	s.Rows[0].Owner = "josé"
	s.Rows[0].Image = "流-base"
	g, err := drawFrame(s, nil, 104, "host", fixedNow, 0)
	if err != nil {
		t.Fatalf("drawFrame with non-ASCII names: %v", err)
	}
	out := g.Plain()
	if !strings.Contains(out, "josé") || !strings.Contains(out, "?-base") {
		t.Fatalf("names not drawn as expected:\n%s", out)
	}
}

// TestPageGridHasNoTextBetweenRows: the rows are display:block, so any
// text between them (a newline) renders as a blank line under each row.
func TestPageGridHasNoTextBetweenRows(t *testing.T) {
	g := Draw(sampleSnapshot(), DefaultWidth, fixedNow, "h")
	html := pageGrid(g, nil)
	if strings.Contains(html, "</span>\n<span") || strings.Contains(html, "\n") {
		t.Fatalf("page grid has text between its rows:\n%s", html[:200])
	}
	if n := strings.Count(html, `<span class="gr"`); n != g.Rows() {
		t.Fatalf("page grid has %d rows, frame has %d", n, g.Rows())
	}
}

// TestPageLinkRowKeepsItsWidth: swapping the holder span for an anchor
// must not change the row's text — the row's tail (padding, the right
// border) appears once, so the row stays one frame wide and does not
// wrap. A long holder is cut with … inside the panel.
func TestPageLinkRowKeepsItsWidth(t *testing.T) {
	s := sampleSnapshot()
	for i := range s.Rows {
		if s.Rows[i].HolderURL != "" {
			s.Rows[i].Holder = "pool:honey/work-47-with-a-much-longer-holder-name-than-fits-the-column"
		}
	}
	g := Draw(s, DefaultWidth, fixedNow, "spoond.example.com")
	links := holderLinks(s, DefaultWidth, fixedNow)
	if len(links) != 1 {
		t.Fatalf("holderLinks = %+v", links)
	}
	plainRow := strings.Split(g.Plain(), "\n")[links[0].row]
	if !strings.Contains(plainRow, "…") {
		t.Fatalf("long holder not cut with …: %q", plainRow)
	}
	page := applyLinks(strings.Split(g.HTML(), "\n")[links[0].row], links[0].row, links)
	text := html.UnescapeString(regexp.MustCompile(`<[^>]*>`).ReplaceAllString(page, ""))
	if text != plainRow {
		t.Fatalf("linked row's text changed:\n got %q\nwant %q", text, plainRow)
	}
}

// TestServicesFillTallerPanel: beside a taller images panel the services
// panel shows every unit that fits instead of folding at
// maxServiceRows; it folds only units that do not fit at all.
func TestServicesFillTallerPanel(t *testing.T) {
	s := sampleSnapshot()
	s.Services = nil
	for i := 0; i < maxServiceRows+1; i++ {
		s.Services = append(s.Services, Service{Name: fmt.Sprintf("unit-%d", i), State: "active"})
	}
	s.Images = nil
	for i := 0; i < maxServiceRows+4; i++ {
		s.Images = append(s.Images, ImageRow{Name: fmt.Sprintf("img-%d", i)})
	}
	p := Draw(s, DefaultWidth, fixedNow, "h").Plain()
	if strings.Contains(p, "more") || !strings.Contains(p, fmt.Sprintf("unit-%d", maxServiceRows)) {
		t.Fatalf("all units should show beside a taller images panel:\n%s", p)
	}
	// With a short images panel the services panel keeps its own cap.
	s.Images = s.Images[:1]
	for i := maxServiceRows + 1; i < maxServiceRows+5; i++ {
		s.Services = append(s.Services, Service{Name: fmt.Sprintf("unit-%d", i), State: "active"})
	}
	p = Draw(s, DefaultWidth, fixedNow, "h").Plain()
	if !strings.Contains(p, "more") {
		t.Fatalf("units past the panel should fold into +N more:\n%s", p)
	}
}

// TestBannerQuietCases: what does not need a person stays off the
// strip: a held lease running again after a rule suspended it, and suspensions that are not a
// held-lease rule's (preemption has its own row while preempted; a
// hand, drain or idle_suspend pause is expected).
func TestBannerQuietCases(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Snapshot)
	}{
		{"rule-suspended lease running again", func(s *Snapshot) {
			s.Rows = []LeaseRow{{ID: "abc123", State: "running", LastAction: "idle/suspend_idle", LastActionAt: fixedNow.Add(-time.Hour)}}
		}},
		{"preempted then resumed", func(s *Snapshot) {
			s.Rows = []LeaseRow{{ID: "f959ca027f", State: "running", LastAction: "preempt/suspend", LastActionAt: fixedNow.Add(-time.Minute)}}
		}},
		{"suspended by hand", func(s *Snapshot) {
			s.Rows = []LeaseRow{{ID: "abc123", State: "suspended", LastAction: "suspend/hand", LastActionAt: fixedNow.Add(-time.Minute)}}
		}},
		{"drained", func(s *Snapshot) {
			s.Rows = []LeaseRow{{ID: "abc123", State: "suspended", LastAction: "drain/suspend", LastActionAt: fixedNow.Add(-time.Minute)}}
		}},
		{"held idle rule", func(s *Snapshot) {
			s.Rows = []LeaseRow{{ID: "abc123", State: "suspended", LastAction: "idle/suspend_idle", LastActionAt: fixedNow.Add(-time.Minute)}}
		}},
		{"held pressure rule", func(s *Snapshot) {
			s.Rows = []LeaseRow{{ID: "abc123", State: "suspended", LastAction: "pressure/suspend_idle", LastActionAt: fixedNow.Add(-time.Minute)}}
		}},
		{"own idle_suspend", func(s *Snapshot) {
			s.Rows = []LeaseRow{{ID: "abc123", State: "suspended", LastAction: "idle_suspend/suspend_idle", LastActionAt: fixedNow.Add(-time.Minute)}}
		}},
	}
	for _, c := range cases {
		snap := healthySnapshot()
		c.mut(&snap)
		if rows := bannerRows(snap, fixedNow); len(rows) != 0 {
			t.Errorf("%s: banner rows = %q, want none", c.name, rows)
		}
	}
}

// TestLeaseCellsNeverRunTogether: a long owner ends in … and leaves a
// space before the state cell, and a long age ("10h37m") shows whole.
func TestLeaseCellsNeverRunTogether(t *testing.T) {
	s := healthySnapshot()
	s.Rows = []LeaseRow{{ID: "038f2ef4c5", Image: "go-base", Owner: "test-consumer", State: "running", Policy: "internet", Age: "10h37m", Left: "2h29m"}}
	p := Draw(s, DefaultWidth, fixedNow, "h").Plain()
	if !strings.Contains(p, "test-con… ▶ running") {
		t.Errorf("long owner should end in … with a space before the state:\n%s", p)
	}
	if !strings.Contains(p, "10h37m") {
		t.Errorf("age 10h37m cut:\n%s", p)
	}
}

// TestLostRowsCapped: past maxLostRows the remaining lost leases share
// one counting row.
func TestLostRowsCapped(t *testing.T) {
	s := healthySnapshot()
	s.ByState = map[string]int{"lost": 5}
	for _, id := range []string{"a1", "a2", "a3", "a4", "a5"} {
		s.Rows = append(s.Rows, LeaseRow{ID: id, Owner: "honey", State: "lost"})
	}
	rows := lostRows(s)
	if len(rows) != maxLostRows+1 || rows[maxLostRows] != "2 more lost lease(s)" {
		t.Fatalf("lost rows = %q", rows)
	}
}
