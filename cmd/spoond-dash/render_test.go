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
		At: "12:00:00", Version: "0.4.2",
		Leases: 5, Queued: 1, Granted: 1234, Swept: 17,
		Running: 3, Limit: 64, Shares: 2, Users: 7, BuildsBusy: 1,
		ByState:   map[string]int{"running": 3, "suspended": 1, "lost": 1},
		ByImage:   map[string]int{"go-base": 2, "py-base": 1},
		ReqPerSec: 12.3, CreatesPerMin: 4, CreateMs: 250, ResumeMs: 4100,
		FwConns: 9, AuthFails: 2, Quota: 1, Throttled: 0, Capacity: 3, BuildFails: 1,
		CPUPct: 37.5, Load1: 1.4, Cores: 16,
		MemUsedPct: 49.3, MemTotalGiB: 19.7, MemUsedGiB: 9.7,
		HugeUsedPct: 44.3, HugeFreeGiB: 23.9,
		DiskUsedPct: 61.2, DiskFreeGiB: 121.5,
		RootUsedPct: 41.0, RootFreeGiB: 30.2,
		VCPUAlloc: 11, MemAllocGiB: 19.5, UptimeH: 720.4,
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
				Age: "5m", Left: "10m"},
			{ID: "1234567890", Image: "py-base", Owner: "ci", State: "running", Policy: "restricted",
				Holder: "forgejo/job-42", HolderURL: "https://git.lacy.casa/job/42", HoldState: "active",
				Age: "2h31m", Left: "∞"},
			{ID: "fedcba0987", Image: "go-base", Owner: "agent", State: "suspended", Policy: "lan",
				Name: "scratch space", Holder: "nightly", HoldState: "lapsed",
				LastAction: "expiry/expire", LastActionAt: fixedNow.Add(-3 * time.Hour),
				Age: "1d", Left: "due"},
		},
		Images: []ImageRow{
			{Name: "go-base", Live: 2, Uses: 51, VCPU: 2, MemMB: 2048, Updated: "2d ago"},
			{Name: "py-base", Live: 1, Uses: 7, VCPU: 1, MemMB: 1024, Updated: "9h ago"},
		},
		Events: []EventLine{
			{Text: "07:19:02 held lease abc: idle/suspend_idle (2h ago)", Style: "warn"},
			{Text: "07:18:44 grant: pooled go-base", Style: "dim"},
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
	g, err := drawFrame(sampleSnapshot(), sampleHist(), w, "vm2.lacy.casa", fixedNow)
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
		if _, err := drawFrame(sampleSnapshot(), sampleHist(), w, "vm2.lacy.casa", fixedNow); err != nil {
			t.Fatalf("width %d: %v", w, err)
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
	s.CertNotAfter = time.Time{}
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
		if _, err := drawFrame(s, nil, w, "h", fixedNow); err != nil {
			t.Fatalf("width %d: frame with a failed unit failed Check: %v", w, err)
		}
		if p := Draw(s, w, fixedNow, "h").Plain(); !strings.Contains(p, "✗ failed") {
			t.Errorf("width %d: down unit not drawn with ✗:\n%s", w, p)
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
// lease, free hugepages and snapshot disk past the danger level, the TLS
// certificate inside 30 days, and a held-lease action within 24 h.
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
		}, "lost lease"},
		{"hugepages danger", func(s *Snapshot) {
			s.HugeUsedPct, s.HugeFreeGiB = 95.0, 1.2
		}, "hugepages"},
		{"snapshot disk danger", func(s *Snapshot) {
			s.DiskUsedPct = 93.0
		}, "snapshot disk"},
		{"cert expiring", func(s *Snapshot) {
			s.CertNotAfter = fixedNow.Add(10 * 24 * time.Hour)
		}, "TLS certificate"},
		{"cert expired", func(s *Snapshot) {
			s.CertNotAfter = fixedNow.Add(-1 * time.Hour)
		}, "expired"},
		{"held action in 24h", func(s *Snapshot) {
			s.Rows = []LeaseRow{{ID: "abc123", LastAction: "idle/suspend_idle", LastActionAt: fixedNow.Add(-2 * time.Hour)}}
		}, "idle/suspend_idle"},
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
		for _, r := range rows {
			if !strings.HasPrefix(r, "■ ") {
				t.Errorf("%s: banner row lacks the ■ marker: %q", tc.name, r)
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

// TestStateGlyphAndStyle: the glyphs the leases panel draws per state,
// and the styles they carry.
func TestStateGlyphAndStyle(t *testing.T) {
	cases := []struct {
		r     LeaseRow
		glyph rune
		style string
	}{
		{LeaseRow{State: "running"}, '▶', "ok"},
		{LeaseRow{State: "recovered"}, '▶', "ok"},
		{LeaseRow{State: "suspended"}, '‖', "warn"},
		{LeaseRow{State: "lost"}, '■', "bad"},
		{LeaseRow{State: "running", HoldState: "active"}, '◆', "state"},
		{LeaseRow{State: "suspended", HoldState: "lapsed"}, '◆', "state"},
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

// TestLeasesShowLapsedHold: a lapsed hold shows ◉lapsed after the holder.
func TestLeasesShowLapsedHold(t *testing.T) {
	p := drawSample(DefaultWidth).Plain()
	if !strings.Contains(p, "nightly ◉lapsed") {
		t.Fatalf("lapsed hold not shown after the holder:\n%s", p)
	}
	if !strings.Contains(p, "forgejo/job-42") {
		t.Fatalf("holder text missing:\n%s", p)
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

// TestPageLinksAreAnchors: holder_url renders as a real <a> with
// rel=noopener, replacing the link span in that row only.
func TestPageLinksAreAnchors(t *testing.T) {
	s := sampleSnapshot()
	g := Draw(s, DefaultWidth, fixedNow, "vm2.lacy.casa")

	links := holderLinks(s, DefaultWidth, fixedNow)
	if len(links) != 1 || links[0].url != "https://git.lacy.casa/job/42" {
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
	if !strings.Contains(html, `href="https://git.lacy.casa/job/42"`) {
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
	// itself stays put).
	changed := d.last
	changed.Rows = append([]LeaseRow(nil), d.last.Rows...)
	changed.Rows[0].State = "suspended"
	w.b.Reset()
	if err := d.writeFrame(w, st, changed, sampleHist()); err != nil {
		t.Fatal(err)
	}
	body := w.b.String()
	if n := strings.Count(body, "event: datastar-patch-elements"); n != 1 {
		t.Fatalf("one changed lease must patch one row, got %d:\n%s", n, body)
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
	// a suspended lease has the same row count, so a is patched per row.
	changed := d.last
	changed.Rows = append([]LeaseRow(nil), d.last.Rows...)
	changed.Rows[0].State = "suspended"
	if err := d.writeFrame(wa, a, changed, sampleHist()); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(wa.b.String(), "data: selector #r"); n != 1 {
		t.Fatalf("viewer a: want one row patch, got %d:\n%s", n, wa.b.String())
	}

	// Viewer b, still on the first frame, must be patched against its
	// own state — the row it never received is sent now.
	wb.b.Reset()
	if err := d.writeFrame(wb, b, changed, sampleHist()); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(wb.b.String(), "data: selector #r"); n != 1 {
		t.Fatalf("viewer b: want one row patch against its own base, got %d:\n%s", n, wb.b.String())
	}
}

// TestRunningByImagePanel: the capacity panel shows a running count per
// image, sorted by name.
func TestRunningByImagePanel(t *testing.T) {
	p := drawSample(DefaultWidth).Plain()
	if !strings.Contains(p, "running by image") {
		t.Fatalf("capacity panel lacks the running-by-image line:\n%s", p)
	}
	if !strings.Contains(p, "go-base 2") || !strings.Contains(p, "py-base 1") {
		t.Fatalf("per-image running counts missing:\n%s", p)
	}
	if strings.Contains(p, "running by image  · ") {
		t.Fatalf("running-by-image line starts with a separator:\n%s", p)
	}
	p72 := drawSample(minW).Plain()
	if !strings.Contains(p72, "running by image") {
		t.Fatalf("narrow frame lacks the running-by-image line:\n%s", p72)
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
		if strings.Contains(r, " lease ") && strings.Contains(r, " holder ") {
			if !strings.Contains(lines[i+1], "no live leases") {
				t.Fatalf("the row under the header is not the empty state:\n%s", p)
			}
			return
		}
	}
	t.Fatalf("leases panel header not found:\n%s", p)
}

// TestEventsPanelMarksHeldActions: automatic held-lease actions are
// drawn with the ┄ the legend promises, journal lines without it.
func TestEventsPanelMarksHeldActions(t *testing.T) {
	s := sampleSnapshot() // Events: one held action (warn), one journal line (dim)
	p := Draw(s, DefaultWidth, fixedNow, "h").Plain()
	lines := strings.Split(p, "\n")
	held, journal := false, false
	for _, r := range lines {
		if strings.Contains(r, "held lease abc") {
			held = strings.Contains(r, "┄")
		}
		if strings.Contains(r, "grant: pooled") {
			journal = !strings.Contains(r, "┄")
		}
	}
	if !held {
		t.Fatalf("held-lease action not drawn with ┄:\n%s", p)
	}
	if !journal {
		t.Fatalf("journal line drawn with ┄:\n%s", p)
	}
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
	g, err := drawFrame(s, nil, 104, "host", fixedNow)
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
	g := Draw(s, DefaultWidth, fixedNow, "vm2.lacy.casa")
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
