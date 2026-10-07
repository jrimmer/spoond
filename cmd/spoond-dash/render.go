// The dashboard on the character grid (#110): one fixed-width grid holds
// the whole frame — header, the Notifications panel for spoond system
// messages when any are active, then a column of panels. The same grid
// feeds Plain (golden tests), ANSI (spoond top) and HTML (the page's
// <pre>), so the terminal and the browser draw one picture from one
// snapshot.
//
// Styles are names, not colours: Plain drops them, ANSI maps them (see
// topStyles in top.go), HTML to g-<name> classes styled once in the
// page's palette.
package spoonddash

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jrimmer/spoond/v2/grid"
)

// Frame width limits: DefaultWidth is what a page shows (the golden
// tests pin its layout), minW the least width every panel stays readable
// at (spoond top on a narrow terminal); wider frames are clipped to
// maxW so no panel ever stretches absurdly.
const (
	DefaultWidth = 104
	minW         = 72
	maxW         = 104
)

// Extra is every non-ASCII rune the dashboard draws beyond grid.Glyphs:
// ✓ an active unit and ✗ one that is not, · separator, ═ the header's
// rule, ┄ the rules inside the capacity and host panels, ■ a lost lease,
// ∞ a persistent lease's remaining time, and the leases panel's marks —
// the run-state glyphs ▶ running, ‖ suspended and ⭘ recovered, and the
// hold marks ◆ held and ◉ lapsed hold. It is passed to grid.Check by
// every renderer, and every rune is asserted to be in the shipped
// JetBrains Mono (TestExtraGlyphsInFont). The Notifications panel's ×
// dismiss control is injected by the page's JS, not drawn on the grid,
// so it is not in this set.
const Extra = "✓✗·═┄■∞◉⭘" + stateGlyphs

// stateGlyphs are the leases panel's run-state and hold glyphs: ▶ ‖ for
// the states with one of their own, ◆ ◉ for the hold marks that lead
// the holder column.
const stateGlyphs = "▶‖◆"

// glyphs is the full set every renderer checks against: the grid
// package's own glyphs plus the dashboard's extras.
func glyphs() string { return grid.Glyphs + Extra }

// stateGlyph names a lease's run state: ▶ running, ‖ suspended,
// ■ lost, ⭘ recovered (which acts like running). A hold is not a state:
// it marks the holder column (◆, or ◉ once lapsed).
func stateGlyph(r LeaseRow) rune {
	switch r.State {
	case "lost":
		return '■'
	case "suspended":
		return '‖'
	case "recovered":
		return '⭘'
	default:
		return '▶'
	}
}

// stateGlyphStyle is the style a lease's glyph and state are drawn in.
func stateGlyphStyle(r LeaseRow) string {
	switch r.State {
	case "lost":
		return "bad"
	case "suspended":
		return "warn"
	default:
		return "ok"
	}
}

// fitItems drops whole items from the right of segs until the row fits
// w, instead of letting the grid clip mid-item. An item ends at one of
// the two separators the rows use: " · " between the legend's entries,
// two spaces between the refusals counters. The legend is centred (a
// narrow frame loses its least important entries, not the row's edges);
// the refusals counters lose their rightmost ones.
func fitItems(segs []grid.Seg, w int) []grid.Seg {
	width := func(ss []grid.Seg) int {
		n := 0
		for _, s := range ss {
			n += len([]rune(s.Text))
		}
		return n
	}
	for width(segs) > w {
		cut := -1
		for i := len(segs) - 1; i >= 0; i-- {
			if segs[i].Text == " · " || segs[i].Text == "  " {
				cut = i
				break
			}
		}
		if cut < 0 {
			break
		}
		segs = segs[:cut]
	}
	return segs
}

// Notice is one spoond system message on the dashboard's notifications
// panel: a stable ID drawn from its trigger, a severity ("warn" or
// "bad") and the message text. The panel is for spoond's own system
// messages only and is dismissable per viewer; it carries no per-lease
// call to action, because the dashboard viewer cannot act on a lease. A
// lost, preempted or lapsed-hold lease is the lease initiator's to deal
// with, and spoond tells that initiator through the API and the lease
// event stream, not through the dashboard.
type Notice struct {
	ID       string
	Severity string // "warn" or "bad"
	Text     string
}

// notices builds the notifications panel's messages from the snapshot,
// in draw order, or nil when there are none (the panel is then never
// drawn). Triggers:
//
//   - a systemd unit not active,
//   - free hugepages or snapshot disk past the danger level,
//   - the snapshot disk's I/O full pressure past DASH_IO_FULL_BAD_PCT,
//   - kept checkpoints past KEPT_DISK_WARN_PCT of the snapshot disk
//     (#126).
//
// Each message's ID comes from its trigger ("unit:<name>", "hugepages",
// "disk", "io-pressure", "kept-disk"), so a viewer's dismissal can
// follow one trigger across refreshes. The leases table still shows a
// lost lease (■ lost), a preempted burst lease and a lapsed hold; those
// are not messages here.
func notices(s Snapshot) []Notice {
	var out []Notice
	for _, svc := range s.Services {
		if svc.State != "active" {
			out = append(out, Notice{ID: "unit:" + svc.Name, Severity: "bad",
				Text: fmt.Sprintf("unit %s is %s", svc.Name, svc.State)})
		}
	}
	if s.HugeFreeGiB > 0 && s.HugeUsedPct >= 92 {
		out = append(out, Notice{ID: "hugepages", Severity: "bad",
			Text: fmt.Sprintf("hugepages only %.1f GiB free - past the danger level", s.HugeFreeGiB)})
	}
	if s.DiskUsedPct >= 90 {
		out = append(out, Notice{ID: "disk", Severity: "bad",
			Text: fmt.Sprintf("snapshot disk %.0f%% used - past the danger level", s.DiskUsedPct)})
	}
	// The disk I/O full pressure (PSI): a sustained stall, not a spike,
	// is a system message. The bad level is configurable (default 15 %)
	// and the text carries the 60 s average it tripped on.
	if s.IOAvail && s.IOFull60 >= ioFullBadPct() {
		out = append(out, Notice{ID: "io-pressure", Severity: "bad",
			Text: fmt.Sprintf("disk i/o stalled: full pressure %.0f%% over 60 s", s.IOFull60)})
	}
	if pct := keptDiskWarnPct(); pct > 0 && s.KeptDiskPct >= pct {
		out = append(out, Notice{ID: "kept-disk", Severity: "warn",
			Text: fmt.Sprintf("kept checkpoints use %.0f%% of the snapshot disk", s.KeptDiskPct)})
	}
	return out
}

// reconcileDismissed is the pure core of the browser's dismissal logic
// (static/js/notifications.js), kept here so the semantics are pinned by
// a test: given the ids the server currently sends active and the ids the
// viewer has dismissed, it returns the dismissals to keep (those still
// active, in the order stored) and the ids that stay visible. A dismissal
// whose id is no longer active is forgotten, so a trigger that clears and
// fires again shows again. Keep this in step with the JS.
func reconcileDismissed(active, dismissed []string) (kept, visible []string) {
	activeSet := make(map[string]bool, len(active))
	for _, id := range active {
		activeSet[id] = true
	}
	keptSet := make(map[string]bool, len(dismissed))
	for _, id := range dismissed {
		if activeSet[id] && !keptSet[id] {
			kept = append(kept, id)
			keptSet[id] = true
		}
	}
	for _, id := range active {
		if !keptSet[id] {
			visible = append(visible, id)
		}
	}
	return kept, visible
}

// DefaultKeptDiskWarnPct is the kept-checkpoint disk share (#126) past
// which the Notifications panel warns, when KEPT_DISK_WARN_PCT is unset.
const DefaultKeptDiskWarnPct = 40.0

// DefaultIOFullWarnPct and DefaultIOFullBadPct are the disk I/O full
// pressure (PSI) levels, in percent of the 60 s average, at which the
// host meter turns warn and bad. The default levels are a first cut and
// will be tuned from #136's measurements.
const (
	DefaultIOFullWarnPct = 5.0
	DefaultIOFullBadPct  = 15.0
)

// keptDiskWarnPct reads the kept-checkpoint disk-share warn level
// (KEPT_DISK_WARN_PCT): a 0 disables the notification; unset or an
// unparsable value means the default.
func keptDiskWarnPct() float64 {
	v := os.Getenv("KEPT_DISK_WARN_PCT")
	if v == "" {
		return DefaultKeptDiskWarnPct
	}
	p, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return DefaultKeptDiskWarnPct
	}
	return p
}

// ioFullWarnPct and ioFullBadPct read the I/O full-pressure warn and
// bad levels (DASH_IO_FULL_WARN_PCT / DASH_IO_FULL_BAD_PCT, in percent
// of the 60 s average). Unset or unparsable values mean the defaults.
func ioFullWarnPct() float64 { return envPct("DASH_IO_FULL_WARN_PCT", DefaultIOFullWarnPct) }

func ioFullBadPct() float64 { return envPct("DASH_IO_FULL_BAD_PCT", DefaultIOFullBadPct) }

// envPct reads a percentage from the environment, falling back to def
// when it is unset or unparsable.
func envPct(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	p, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return p
}

// Draw renders the whole frame at width w. now timestamps the ages;
// host names the node in the header. There is no history, so the
// sparklines draw empty and the throughput title carries no window.
// Goldens that need them call drawFrame directly.
func Draw(s Snapshot, w int, now time.Time, host string) *grid.Grid {
	g, err := drawFrame(s, nil, w, host, now, 0)
	if err != nil {
		// Callers of Draw (tests, one-off renders) pass fixed snapshots;
		// a Check failure is a programming error worth panicking on.
		panic(err)
	}
	return g
}

// histVals is one sparkline series, empty when there is no history.
func (l *layout) histVals(key string) []float64 {
	if l.histFn == nil {
		return nil
	}
	return l.histFn(key)
}

// histMinutes is the history's window in whole minutes: the points
// times the scrape interval, rounded up so the title never undersells
// the window (2 s × 150 points reads "last 5 min"). 0 without a history
// or a known interval (Draw, a frame before the first tick), which
// drops the "· last N min" from the title.
func (l *layout) histMinutes() int {
	if l.histN <= 0 || l.interval <= 0 {
		return 0
	}
	return int(math.Ceil(float64(l.histN) * l.interval.Minutes()))
}

// drawFrame renders a snapshot plus history into the full frame at
// width w: the shared entry point of Draw, the page, the stream and
// spoond top. host names the node in the header; now is the frame's
// timestamp, carried by the snapshot's own ages and clock rather than
// read here; interval is the scrape interval the history points are
// spaced by (the throughput title's window; 0 when there is none). It
// fails only when grid.Check rejects the finished frame (a rune no
// renderer can draw) — the terminal path reports it instead of printing
// a broken frame.
func drawFrame(s Snapshot, hist map[string][]float64, w int, host string, now time.Time, interval time.Duration) (*grid.Grid, error) {
	l := &layout{w: clamp(w, minW, maxW), host: host, s: s,
		notices:  notices(s),
		histFn:   func(k string) []float64 { return hist[k] },
		histN:    len(hist["running"]),
		interval: interval,
	}
	g := l.assemble()
	if err := g.Check(glyphs()); err != nil {
		return nil, err
	}
	return g, nil
}

// dashInterval is the scrape interval the tests space their history
// points by: DASH_INTERVAL's default. The renderers take the real
// interval from the config, so a DASH_INTERVAL of 10 s titles the
// throughput panel with the window it actually shows.
const dashInterval = 2 * time.Second

// layout assembles the grid panel by panel.
type layout struct {
	w       int
	notices []Notice
	host    string
	s       Snapshot
	// histFn serves the sparkline series; Draw leaves it nil (the
	// sparklines then draw empty) and the page/top fill it from the
	// collector's history.
	histFn func(string) []float64
	// interval is the scrape interval the history points are spaced by;
	// 0 when unknown.
	interval time.Duration
	// histN is the history's length in points; 0 (no history yet) leaves
	// the window out of the throughput panel's title.
	histN int
}

// assemble draws the header, the notifications panel and every panel
// into one grid.
// panelsH is the height of the capacity and host panels together: as
// one side-by-side band (the taller panel's height) or as two stacked
// panels.
func (l *layout) panelsH() int {
	if l.wide() {
		return max(l.capacityH(), l.hostH())
	}
	return l.capacityH() + l.hostH()
}

// drawPanels draws the capacity and host panels: side by side at wide
// frames (capacity left, host right, both the taller one's height),
// stacked full width below it (capacity first).
func (l *layout) drawPanels(g *grid.Grid, y int) int {
	if l.wide() {
		h := max(l.capacityH(), l.hostH())
		l.drawCapacity(g, 0, y, panelW, h)
		l.drawHost(g, panelW+panelGap, y, panelW, h)
		return y + h
	}
	y = l.drawCapacity(g, 0, y, l.w, l.capacityH())
	return l.drawHost(g, 0, y, l.w, l.hostH())
}

func (l *layout) assemble() *grid.Grid {
	h := headerRows() +
		l.noticesH() +
		l.panelsH() + l.throughputH() + l.leasesH() +
		l.imagesServicesH() + l.refusalsH() + l.eventsH() +
		1 // the footer line

	g := grid.New(l.w, h)
	l.header(g, 0)
	y := headerRows()

	y = l.drawNotices(g, y)
	y = l.drawPanels(g, y)
	y = l.throughput(g, y)
	y = l.leases(g, y)
	y = l.imagesServices(g, y)
	y = l.refusals(g, y)
	y = l.events(g, y)
	l.footer(g, y)
	return g
}

// defaultProjectURL is the footer's URL when DASH_PROJECT_URL is unset:
// the module's home.
const defaultProjectURL = "github.com/jrimmer/spoond"

// projectHref is the URL the footer's GitHub mark links to:
// DASH_PROJECT_URL, else the module's home, with an https scheme when
// none is given.
func projectHref() string {
	u := os.Getenv("DASH_PROJECT_URL")
	if u == "" {
		u = defaultProjectURL
	}
	if !strings.Contains(u, "://") {
		u = "https://" + u
	}
	return u
}

// releaseDate is the build's release date: the vcs.time build setting of
// the binary's own build (the release commit's date) as YYYY-MM-DD, or
// "" for a dev build with no VCS stamp.
func releaseDate() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return releaseDateFrom(bi.Settings)
}

// releaseDateFrom turns build settings into the footer's date: the
// vcs.time stamp as YYYY-MM-DD, or "" when there is none (a dev build)
// or it does not parse.
func releaseDateFrom(settings []debug.BuildSetting) string {
	for _, s := range settings {
		if s.Key == "vcs.time" {
			if t, err := time.Parse(time.RFC3339, s.Value); err == nil {
				return t.Format("2006-01-02")
			}
		}
	}
	return ""
}

// footerParts is the footer line's parts: the dashboard binary's version
// and its release date (empty for a dev build).
type footerParts struct {
	version, date string
}

// footerPartsFor builds the line's parts from the dashboard build. The
// release date is omitted when the build has no vcs.time.
func footerPartsFor() footerParts {
	return footerParts{version: versionLabel(dashVersion), date: releaseDate()}
}

// footerSegs renders the footer parts as one dim, centred line:
// "Spoond <version> (<date>) · GitHub". The GitHub mark is plain text in
// the terminal grid; the page swaps the span for the mark's SVG
// (applyProjectLink). On a frame too narrow for the whole line the date
// is dropped first; the version and the mark always stay.
func footerSegs(f footerParts, w int) []grid.Seg {
	sep := grid.Seg{Text: " · ", Style: "dim"}
	ver := grid.Seg{Text: "Spoond " + f.version, Style: "dim"}
	logo := grid.Seg{Text: "GitHub", Style: "ghmark"}
	if f.date != "" {
		full := []grid.Seg{ver, {Text: " (" + f.date + ")", Style: "dim"}, sep, logo}
		if segWidth(full) <= w {
			return full
		}
	}
	return []grid.Seg{ver, sep, logo}
}

// footer draws the frame's last row: one dim, centred line naming the
// project — "Spoond <version> (<date>) · GitHub". On a narrow frame the
// date drops first. The GitHub mark span carries the ghmark style: dim
// text in the terminal, swapped for the mark's SVG by the page
// (applyProjectLink).
func (l *layout) footer(g *grid.Grid, y int) {
	g.Center(l.w/2, y, footerSegs(footerPartsFor(), l.w))
}
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// header: the title line at the left margin — SPOOND · host — with a
// blank row under it as the gutter before the panels (headerRows). The
// left inset matches the panels' frames (column 0). The version is not
// here: it lives on the footer with the project, so it never shows
// twice. The holder column's header explains its two marks; every other
// state is spelled out where it is shown. Right-aligned on the same row
// is spoond's own uptime (the backend process, not the host's, which
// would read as spoond's right after a deploy) and the frame's clock:
// "up 35m, 12:41:07". On a frame too narrow for both, the uptime is
// dropped before the time, and the time is dropped rather than overlap
// the title.
func (l *layout) header(g *grid.Grid, y int) {
	title := []grid.Seg{
		{Text: "SPOOND", Style: "head"},
		{Text: " · ", Style: "dim"},
		{Text: l.host, Style: "text"},
	}
	// The right side keeps the clock always and the uptime only when it
	// fits clear of the left-aligned title. titleEnd is one past the
	// title's last cell; the right text must start a column beyond it.
	titleEnd := segWidth(title)
	right := l.s.At
	if l.s.BackendUp > 0 {
		if with := "up " + dur(l.s.BackendUp) + ", " + l.s.At; l.w-segWidth([]grid.Seg{{Text: with}}) > titleEnd {
			right = with
		}
	}
	if l.w-segWidth([]grid.Seg{{Text: right}}) > titleEnd {
		g.Right(l.w-1, y, []grid.Seg{{Text: right, Style: "dim"}})
	}
	g.Segs(0, y, title, -1)
}

// versionLabel is a version for the header: "?" when the scrape had
// none. A spoond build version is shortened for the line: a tag stays
// as it is (v2.2.0), a Go pseudo-version (v2.1.3-0.20261004183409-
// 7a13d2bd1131) becomes base+first seven of the hash (v2.1.3+7a13d2b).
// pseudoVersion matches a Go pseudo-version: the base, then the commit
// hash.
var pseudoVersion = regexp.MustCompile(`^(v\d+\.\d+\.\d+)-(?:0\.)?\d{14}-([0-9a-f]{7,40})$`)

func versionLabel(v string) string {
	if v == "" {
		return "?"
	}
	// vX.Y.Z-0.<14-digit time>-<12 hex> after a tag, vX.Y.Z-<time>-<hex>
	// for a module with no tags (v0.0.0-…).
	if m := pseudoVersion.FindStringSubmatch(v); m != nil {
		return m[1] + "+" + m[2][:7]
	}
	return v
}

// panel draws a single-line framed panel of h rows with its title on
// the frame's top border at x+2 (┌─ title ───), at row y, and returns
// the row past its bottom edge. id is the element id Datastar patches
// by.
func (l *layout) panel(g *grid.Grid, x, y, w, h int, title, id string) int {
	g.Box(x, y, w, h, "frame", false)
	if title != "" {
		g.Title(x+2, y, []grid.Seg{{Text: " " + title + " ", Style: "title"}})
	}
	g.Mark(x+1, y+1, w-2, h-2, id)
	return y + h
}

// noticesH is the notifications panel's height: a top border carrying
// the title, one row per message and a bottom border. 0 when there are
// no messages, so the panel is never drawn.
func (l *layout) noticesH() int {
	if len(l.notices) == 0 {
		return 0
	}
	return len(l.notices) + 2
}

// drawNotices draws the full-width Notifications panel at y, in the same
// frame and title style as the other panels, and returns the row past
// its bottom edge. Every row of the panel carries the element id
// "notifications" and each message row its own ID ("notice:<id>"), so
// the page can hide a dismissed message or the whole panel without any
// server state; the terminal draws the panel unchanged.
func (l *layout) drawNotices(g *grid.Grid, y int) int {
	if len(l.notices) == 0 {
		return y
	}
	top := y
	y = l.panel(g, 0, y, l.w, l.noticesH(), "Notifications", "notifications")
	// panel marks only the interior; the page hides the whole panel when
	// every message is dismissed, borders included.
	g.Mark(0, top, l.w, l.noticesH(), "notifications")
	for i, n := range l.notices {
		row := top + 1 + i
		g.Text(2, row, sanitize(n.Text), n.Severity, l.w-4)
		g.Mark(0, row, l.w, 1, "notice:"+sanitize(n.ID))
	}
	return y
}

// wide reports whether the frame carries the capacity and host panels
// side by side; below it they stack full width, capacity first.
func (l *layout) wide() bool { return l.w >= sideBySideW }

// sideBySideW is the least width at which capacity and host sit side
// by side: two 51-cell boxes with a two-cell gap between them.
const sideBySideW = 104

// panelW is the width of one side-by-side panel (capacity, host): a
// 51-cell box, two of which plus the gap fill the 104-cell frame.
const panelW = 51

// panelGap is the space between two side-by-side panels.
const panelGap = 2

// stateCount is one name/count pair of the capacity panel.
type stateCount struct {
	name string
	n    int
}

// stateOrder orders ByState: running, recovered, suspended, lost first,
// then any other state alphabetically — stable output for the goldens.
func stateOrder(m map[string]int) []stateCount {
	fixed := []string{"running", "recovered", "suspended", "lost"}
	var out []stateCount
	for _, name := range fixed {
		out = append(out, stateCount{name, m[name]})
	}
	var rest []string
	for name := range m {
		if !contains(fixed, name) {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	for _, name := range rest {
		out = append(out, stateCount{name, m[name]})
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// holderLinks maps each lease row's y to its holder_url: the page
// renderer swaps the row's link-styled span for a real anchor. Only
// leases with a URL appear here. The row math must match assemble:
// header (headerRows) + the notifications panel + the panels above
// leases, then the panel's title row, then one row per lease.
func holderLinks(s Snapshot, w int, now time.Time) []linkAt {
	l := &layout{w: clamp(w, minW, maxW), s: s,
		notices: notices(s)}
	base := headerRows() + l.noticesH() +
		l.panelsH() + l.throughputH()
	leaseY := base + 1 // + the leases panel's title row
	var out []linkAt
	rows := l.s.Rows
	if n := maxLeaseRows(l.w); len(rows) > n {
		rows = rows[:n]
	}
	for i, r := range rows {
		if r.HolderURL != "" {
			out = append(out, linkAt{row: leaseY + 1 + i, text: r.Holder, url: r.HolderURL})
		}
	}
	return out
}

// headerRows is the header's row count: the centred title line, the
// ═ rule under it and the legend.
func headerRows() int { return 2 } // the title and a blank gutter row

// capacity panel: the running meter, the leases line, queued, granted,
// swept, then one row per image with live leases. H is the height the
// panel shares with host when the two sit side by side. A wrapped
// capacity row (the leases line grows with the cluster, the panel does
// not) takes its extra lines into account.
func (l *layout) capacityH() int {
	h := 3 + l.capacityLineCount() // title + lines (+ frame)
	if l.imageRows() > 0 {
		h += l.imageRows() + 1 // a ┄ rule, then the image rows
	}
	return h
}

// capacityLineCount is the capacityRows' drawn line count: wrapped
// rows contribute one line per fold, the rest one each.
func (l *layout) capacityLineCount() int {
	n := 0
	for _, r := range l.capacityRows() {
		n += len(wrapCapacityRow(r, l.wrapWidth()))
	}
	return n
}

// wrapWidth is the cell width a capacity row wraps to: the panel's
// inner width, side by side or full width alike.
func (l *layout) wrapWidth() int {
	if l.wide() {
		return panelW - 4
	}
	return l.w - 4
}

// imageRows is the number of rows the capacity panel's per-image block
// needs: one per image with live leases, bounded by the room the panel
// has, with a final "+N more" row when images were dropped.
func (l *layout) imageRows() int {
	n := len(l.imageCounts())
	if n == 0 {
		return 0
	}
	if room := l.imageRoom(); n > room {
		return room // the last row becomes "+N more"
	}
	return n
}

// maxImageRows is the most per-image rows the capacity panel shows
// before it folds the rest into a "+N more" row.
const maxImageRows = 6

// imageRoom is how many image rows fit: bounded by maxImageRows, less
// the shared row the counts block needs at the minimum height.
func (l *layout) imageRoom() int {
	room := maxImageRows - max(0, len(l.capacityRows())-capacityMinRows+1)
	if room < 1 {
		room = 1
	}
	return room
}

// capacityRow is one plain row of the capacity panel: its segments,
// whether it draws dim (the shares line) and an optional value drawn
// right-aligned (the running meter's count).
type capacityRow struct {
	segs  []grid.Seg
	dim   bool
	right string
	wrap  bool
	// alts are shorter forms of segs, tried in order when segs does not
	// fit; wrapping applies to the last one only if none fits.
	alts [][]grid.Seg
}

// capacityMinRows is the least number of rows the capacity panel's
// fixed block needs, shared meter row included (running, leases,
// queued, shares): the height a side-by-side frame reserves.
const capacityMinRows = 4

// capacityRows builds the capacity panel's fixed rows, top to bottom:
// the running meter, the leases-per-state line, the queued/granted/
// swept line and the shares/users/builds line (dim). The per-image
// rows are drawn separately, after a ┄ rule.
func (l *layout) capacityRows() []capacityRow {
	m := l.meterSegs("running", l.runningPct(), 75, 90, meterBarW)
	rows := []capacityRow{{segs: m, right: fmt.Sprintf("%d / %d", l.s.Running, l.s.Limit)}}

	// Leases: total, then running (with its burst share in brackets,
	// #128), suspended and lost; recovered only when non-zero. A zero
	// count is dim. One line: when the full words do not fit the panel,
	// "susp"/"recov" stand in (alts), and only then does it wrap.
	leaseLine := func(susp, recov string) []grid.Seg {
		count := func(n int, label string) []grid.Seg {
			style := "text"
			if n == 0 {
				style = "dim"
			}
			return []grid.Seg{{Text: " · ", Style: "dim"}, {Text: fmt.Sprintf("%d", n), Style: style}, {Text: " " + label, Style: "dim"}}
		}
		segs := []grid.Seg{{Text: fmt.Sprintf("%d leases", l.s.Leases), Style: "text"}}
		segs = append(segs, count(l.s.ByState["running"], "running")...)
		if n := l.s.Burst; n > 0 {
			segs = append(segs, grid.Seg{Text: fmt.Sprintf(" (%d burst)", n), Style: "dim"})
		}
		segs = append(segs, count(l.s.ByState["suspended"], susp)...)
		segs = append(segs, count(l.s.ByState["lost"], "lost")...)
		if n := l.s.ByState["recovered"]; n > 0 {
			segs = append(segs, count(n, recov)...)
		}
		return segs
	}
	rows = append(rows, capacityRow{segs: leaseLine("suspended", "recovered"), wrap: true,
		alts: [][]grid.Seg{leaseLine("susp", "recov")}})

	queued := fmt.Sprintf("queued %s", fmt.Sprint(l.s.Queued))
	if l.s.Queued > 0 && l.s.QueuedOldest > 0 {
		queued = fmt.Sprintf("%s (oldest %s)", queued, dur(time.Duration(l.s.QueuedOldest*float64(time.Second))))
	}
	rows = append(rows, capacityRow{segs: dimLine(
		queued,
		fmt.Sprintf("granted %s", thousands(l.s.Granted)),
		fmt.Sprintf("swept %s", thousands(l.s.Swept)))})
	rows = append(rows, capacityRow{dim: true, segs: dimLine(
		fmt.Sprintf("shares %d", l.s.Shares),
		fmt.Sprintf("users %d", l.s.Users),
		fmt.Sprintf("builds busy %d", l.s.BuildsBusy))})
	return rows
}

// dimLine joins parts with " · " in the dim style.
func dimLine(parts ...string) []grid.Seg {
	var segs []grid.Seg
	for i, p := range parts {
		if i > 0 {
			segs = append(segs, grid.Seg{Text: " · ", Style: "dim"})
		}
		segs = append(segs, grid.Seg{Text: p, Style: "dim"})
	}
	return segs
}

// thousands formats n with commas every three digits: 1234 reads
// "1,234".
func thousands(n int) string {
	s := strconv.Itoa(n)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	start := len(s) % 3
	if start == 0 {
		start = 3
	}
	out := s[:start]
	for i := start; i < len(s); i += 3 {
		out += "," + s[i:i+3]
	}
	return sign + out
}

// imageCount is one image's live leases for the capacity panel: the
// image's name and its running and suspended counts.
type imageCount struct {
	name               string
	running, suspended int
}

// imageCounts folds the lease rows into per-image live counts: running
// and suspended per image, ordered by live count descending, then name.
// Zero-count states do not create rows.
func (l *layout) imageCounts() []imageCount {
	m := map[string]*imageCount{}
	for _, r := range l.s.Rows {
		if r.State != "running" && r.State != "suspended" {
			continue
		}
		ic := m[r.Image]
		if ic == nil {
			ic = &imageCount{name: r.Image}
			m[r.Image] = ic
		}
		if r.State == "running" {
			ic.running++
		} else {
			ic.suspended++
		}
	}
	out := make([]imageCount, 0, len(m))
	for _, ic := range m {
		out = append(out, *ic)
	}
	sort.Slice(out, func(i, j int) bool {
		li, lj := out[i].running+out[i].suspended, out[j].running+out[j].suspended
		if li != lj {
			return li > lj
		}
		return out[i].name < out[j].name
	})
	return out
}

// drawCapacity draws the capacity panel at (x, y) in w cells and
// returns the row past its bottom edge. The panel keeps its title row,
// its fixed rows and one ┄ rule before the per-image rows (each: name,
// a nine-cell live bar, the count right-aligned).
func (l *layout) drawCapacity(g *grid.Grid, x, y, w, h int) int {
	top := y
	y = l.panel(g, x, y, w, h, "capacity", "capacity")
	inner := w - 4

	row := top + 1
	for _, r := range l.capacityRows() {
		for _, line := range wrapCapacityRow(r, inner) {
			segs := line
			if r.dim {
				segs = make([]grid.Seg, len(line))
				copy(segs, line)
				for i := range segs {
					segs[i].Style = "dim"
				}
			}
			room := inner
			if r.right == "" {
				room = inner + 1 // no right value: the line may use its column
			}
			g.Segs(x+2, row, segs, room)
			if r.right != "" {
				g.Right(x+w-4, row, []grid.Seg{{Text: r.right, Style: "text"}})
			}
			row++
		}
	}
	imgs := l.imageCounts()
	if len(imgs) == 0 {
		g.Text(x+2, row, "no live leases", "dim", inner)
		return y
	}
	l.rule(g, x, w, row)
	row++

	names := make([]imageCount, len(imgs))
	copy(names, imgs)
	truncated := false
	if room := l.imageRoom(); len(names) > room {
		names = names[:room]
		truncated = true
	}
	barX := x + 2 + 15
	for _, im := range names {
		g.Text(x+2, row, ellipsize(sanitize(im.name), 15), "text", 15)
		// Three cells per running lease, at most nine; · fills the rest.
		fill := min(9, im.running*3)
		g.Text(barX, row, strings.Repeat("█", fill)+strings.Repeat("·", 9-fill), "ok", 9)
		right := fmt.Sprintf("%d running", im.running)
		if im.running == 0 {
			right = fmt.Sprintf("%d suspended", im.suspended)
		}
		// The counts sit a column short of the meters' value column, one
		// clear of the frame with the two the meters keep.
		g.Right(x+w-4, row, []grid.Seg{{Text: right, Style: "dim"}})
		row++
	}
	if truncated {
		g.Text(x+2, row, fmt.Sprintf("+%d more", len(imgs)-len(names)), "dim", inner)
	}
	return y
}

// wrapCapacityRow folds one capacity row's segments into lines of at
// most max cells. Only the leases line asks (wrap): the counters grow
// with the cluster while the panel keeps its width, so it continues at
// a "·" separator. Each counter is a "· N label" group — separator,
// value, space-prefixed label — kept whole: a group that does not fit
// moves down together, and the separator it would have left behind is
// dropped with it (a line never starts or ends with " · "). Every
// other row is one line, clipped as before.
func wrapCapacityRow(r capacityRow, max int) [][]grid.Seg {
	// A row with no right-aligned value may use the column the value
	// would have taken, up to the frame.
	room := max
	if r.right == "" {
		room = max + 1
	}
	if segWidth(r.segs) <= room {
		return [][]grid.Seg{r.segs}
	}
	for _, alt := range r.alts {
		if segWidth(alt) <= room {
			return [][]grid.Seg{alt}
		}
	}
	if len(r.alts) > 0 {
		r.segs = r.alts[len(r.alts)-1]
	}
	if !r.wrap {
		return [][]grid.Seg{r.segs}
	}
	var lines [][]grid.Seg
	line := []grid.Seg{}
	n := 0
	for _, s := range r.segs {
		w := len([]rune(s.Text))
		if n+w <= max {
			line = append(line, s)
			n += w
			continue
		}
		if len(line) == 0 {
			// A lone segment wider than the panel: clip it, as Segs
			// would have.
			line = append(line, grid.Seg{Text: ellipsize(s.Text, max), Style: s.Style})
			n = max
			continue
		}
		// The group before this separator stays; the separator drops.
		for line[len(line)-1].Text == " · " {
			line = line[:len(line)-1]
			n -= 3
		}
		lines = append(lines, line)
		line, n = nil, 0
		if s.Text == " · " {
			continue
		}
		// A space-prefixed suffix belongs to the count before it: pull
		// that count down, so the line never starts mid-pair — and drop
		// the separator it leaves behind.
		if strings.HasPrefix(s.Text, " ") && len(lines[len(lines)-1]) > 0 {
			prev := lines[len(lines)-1]
			last := prev[len(prev)-1]
			prev = prev[:len(prev)-1]
			for len(prev) > 0 && prev[len(prev)-1].Text == " · " {
				prev = prev[:len(prev)-1]
			}
			lines[len(lines)-1] = prev
			line = append(line, last)
			n += len([]rune(last.Text))
		}
		line = append(line, s)
		n += w
	}
	if len(line) > 0 {
		lines = append(lines, line)
	}
	return lines
}

// runningPct is the running meter's fill fraction.
func (l *layout) runningPct() float64 {
	if l.s.Limit <= 0 {
		return 0
	}
	return float64(l.s.Running) / float64(l.s.Limit) * 100
}

// meterBarW is the panels' fixed meter bar width: 16 cells, side by
// side and stacked alike, matching the mockup.
const meterBarW = 16

// meterSegs is one meter row's left part as styled segments: a label
// padded to meterLabelW, a space, then a bar of barW cells whose style
// follows pct (ok below warnPct, warn below dangerPct, bad at or
// above), with a ╎ tick at the warning level. The value is not part of
// the row: callers right-align it so it ends one column clear of the
// panel's inner right edge.
func (l *layout) meterSegs(label string, pct, warnPct, dangerPct float64, barW int) []grid.Seg {
	segs := []grid.Seg{{Text: fmt.Sprintf("%-*s", meterLabelW, label), Style: "dim"}, {Text: " ", Style: "dim"}}
	if barW > 0 {
		filled := clamp(int(pct/100*float64(barW)), 0, barW)
		if pct > 0 && filled == 0 {
			filled = 1 // anything above zero shows: 3 of 64 is not an empty bar
		}
		style := "ok"
		switch {
		case pct >= dangerPct:
			style = "bad"
		case pct >= warnPct:
			style = "warn"
		}
		bar := make([]rune, barW)
		for i := range bar {
			bar[i] = '░'
			if i < filled {
				bar[i] = '█'
			}
		}
		// The warning-level tick is a warn-coloured cell inside the bar,
		// not part of it: the bar keeps its width, and a meter without a
		// warning level draws no tick.
		tick := -1
		if warnPct > 0 && warnPct < 100 {
			if tx := int(warnPct / 100 * float64(barW)); tx < barW {
				tick = tx
			}
		}
		if tick < 0 {
			segs = append(segs, grid.Seg{Text: string(bar), Style: style})
		} else {
			if tick > 0 {
				segs = append(segs, grid.Seg{Text: string(bar[:tick]), Style: style})
			}
			segs = append(segs, grid.Seg{Text: "╎", Style: "warn"})
			if tick+1 < barW {
				segs = append(segs, grid.Seg{Text: string(bar[tick+1:]), Style: style})
			}
		}
	}
	return segs
}

// meterLabelW is the label column both panels' meters pad to.
const meterLabelW = 13

// hostRow is one meter row of the host panel.
type hostRow struct {
	label     string
	pct, warn float64
	danger    float64
	right     string
	// text, when non-nil, replaces the meter bar: a plain line whose
	// value carries the style from the same thresholds (the I/O rows,
	// where a fill bar would say nothing useful).
	text []grid.Seg
}

// hostPanelRows builds the host panel's rows: cpu, memory, hugepages,
// snapshot disk, root disk — the meters and levels the old page already
// showed — plus the disk I/O rows and the allocated line.
func hostPanelRows(s Snapshot) []hostRow {
	return append([]hostRow{
		{"cpu", s.CPUPct, 75, 90, fmt.Sprintf("%.0f%%  load %.1f  %d cores", s.CPUPct, s.Load1, s.Cores), nil},
		{"memory", s.MemUsedPct, 80, 92, fmt.Sprintf("%.1f of %.1f GiB", s.MemUsedGiB, s.MemTotalGiB), nil},
		{"hugepages", s.HugeUsedPct, 80, 92, fmt.Sprintf("%.1f GiB free", s.HugeFreeGiB), nil},
		{"snapshot disk", s.DiskUsedPct, 80, 90, fmt.Sprintf("%.1f GiB free", s.DiskFreeGiB), nil},
		{"root disk", s.RootUsedPct, 75, 90, fmt.Sprintf("%.1f GiB free", s.RootFreeGiB), nil},
	}, ioHostRows(s)...)
}

// hostH is the host panel's own height: title, meters, rule, the two
// tail rows and the frame's bottom edge. Side by side, the panel is
// drawn as tall as capacity instead — the taller of the two.
func (l *layout) hostH() int {
	return 1 + len(hostPanelRows(l.s)) + 1 + 2 + 1
}

// hostRows builds the host panel's meter rows: cpu, memory, hugepages,
// snapshot disk, root disk — the meters and levels the old page already
// showed — then the disk I/O rows. right is the value text at the row's
// end.
func (l *layout) hostRows() []hostRow {
	return append([]hostRow{
		{"cpu", l.s.CPUPct, 75, 90, fmt.Sprintf("%.0f%% · load %.1f", l.s.CPUPct, l.s.Load1), nil},
		{"memory", l.s.MemUsedPct, 80, 92, fmt.Sprintf("%.1f of %.1f GiB", l.s.MemUsedGiB, l.s.MemTotalGiB), nil},
		{"hugepages", l.s.HugeUsedPct, 80, 92, fmt.Sprintf("%.1f GiB free", l.s.HugeFreeGiB), nil},
		{"snapshot disk", l.s.DiskUsedPct, 80, 90, fmt.Sprintf("%.1f GiB free", l.s.DiskFreeGiB), nil},
		{"root disk", l.s.RootUsedPct, 75, 90, fmt.Sprintf("%.1f GiB free", l.s.RootFreeGiB), nil},
	}, ioHostRows(l.s)...)
}

// ioHostRows builds the disk I/O rows: the PSI pressure line and the
// device's write throughput and busy share. A kernel without PSI
// (IOAvail false) hides both rather than drawing a calm zero; a device
// the collector could not resolve leaves the second row off.
//
//	I/O pressure  some 0.3% / full 0.0% (60s)
//	nvme0n1       12 MB/s w, 18% busy
//
// The pressure row's style follows the full 60 s average against the
// configurable levels (DASH_IO_FULL_WARN_PCT / DASH_IO_FULL_BAD_PCT);
// the device row's follows its busy share.
func ioHostRows(s Snapshot) []hostRow {
	if !s.IOAvail {
		return nil
	}
	rows := []hostRow{ioPressureRow(s)}
	if s.DiskDevice != "" {
		rows = append(rows, ioDeviceRow(s))
	}
	return rows
}

func ioPressureRow(s Snapshot) hostRow {
	return hostRow{
		text: []grid.Seg{
			{Text: fmt.Sprintf("%-*s", meterLabelW, "I/O pressure"), Style: "dim"},
			{Text: " ", Style: "dim"},
			{Text: fmt.Sprintf("some %.1f%% / full %.1f%% (60s)", s.IOSome60, s.IOFull60),
				Style: meterStyle(s.IOFull60, ioFullWarnPct(), ioFullBadPct())},
		},
	}
}

func ioDeviceRow(s Snapshot) hostRow {
	return hostRow{
		text: []grid.Seg{
			{Text: fmt.Sprintf("%-*s", meterLabelW, s.DiskDevice), Style: "dim"},
			{Text: " ", Style: "dim"},
			{Text: fmt.Sprintf("%.0f MB/s w, %.0f%% busy", s.DiskWriteMB, s.DiskBusyPct),
				Style: meterStyle(s.DiskBusyPct, 80, 90)},
		},
	}
}

// meterStyle is the ok/warn/bad style a value takes at the same
// thresholds the meter bars use.
func meterStyle(pct, warnPct, dangerPct float64) string {
	switch {
	case pct >= dangerPct:
		return "bad"
	case pct >= warnPct:
		return "warn"
	default:
		return "ok"
	}
}

// rule draws a ┄ line across a panel's inner width: from x+2 to
// x+w-3, one column clear of each border.
func (l *layout) rule(g *grid.Grid, x, w, y int) {
	for i := x + 2; i < x+w-2; i++ {
		g.Put(i, y, '┄', "dim")
	}
}

// drawHost draws the host panel at (x, y) in w cells and returns the
// row past its bottom edge: the meters, a ┄ rule, then vcpu and memory
// allocations and the build GC's mode and lifetime count.
func (l *layout) drawHost(g *grid.Grid, x, y, w, h int) int {
	top := y
	y = l.panel(g, x, y, w, h, "host", "host")
	inner := w - 4

	row := top + 1
	for _, r := range l.hostRows() {
		if r.text != nil {
			g.Segs(x+2, row, r.text, inner)
			row++
			continue
		}
		g.Segs(x+2, row, l.meterSegs(r.label, r.pct, r.warn, r.danger, meterBarW), inner)
		g.Right(x+w-4, row, []grid.Seg{{Text: r.right, Style: "text"}})
		row++
	}
	l.rule(g, x, w, row)
	row++

	// Allocations: what the running leases hold against the node's
	// cores and memory (Cores is the total the vcpu fraction is of).
	alloc := dimLine(
		fmt.Sprintf("vcpu alloc %d / %d", l.s.VCPUAlloc, l.s.Cores),
		fmt.Sprintf("mem alloc %.1f GiB", l.s.MemAllocGiB))
	alloc[0].Style = "text"
	alloc[2].Style = "text"
	g.Segs(x+2, row, alloc, inner)
	row++

	// The build GC's mode — bold ok when deletion is on, dim when it
	// only logs candidates — and its lifetime count, left out at zero.
	// The kept checkpoints ride the same row (#126): "kept N (X GiB)"
	// when N > 0.
	segs := []grid.Seg{{Text: "gc ", Style: "dim"}, gcSeg(l.s.GCMode)}
	if l.s.GCDeleted > 0 {
		segs = append(segs,
			grid.Seg{Text: " · ", Style: "dim"},
			grid.Seg{Text: fmt.Sprintf("%s builds deleted", thousands(l.s.GCDeleted)), Style: "text"})
	}
	if l.s.KeptBuilds > 0 {
		segs = append(segs,
			grid.Seg{Text: " · ", Style: "dim"},
			grid.Seg{Text: fmt.Sprintf("kept %s (%s GiB)", thousands(l.s.KeptBuilds), thousands(int(l.s.KeptBuildsBytes/(1<<30)))), Style: "text"})
	}
	g.Segs(x+2, row, segs, inner)
	return y
}

// throughput panel: running leases, requests/s, creates/min, egress
// connections, each a sparkline over the collector's history.
func (l *layout) throughputH() int {
	if l.wide() {
		return 4 // title, the values row, the sparkline row, the bottom edge
	}
	return 6 // the same, 2×2: two value rows and two sparkline rows
}

// throughputSeries is one sparkline: its label, the history key, the
// current value as drawn at the label row's right, and the style its
// sparkline carries (the running-leases series the state colour, the
// rest the spark colour).
func (l *layout) throughputSeries() []struct {
	label string
	key   string
	last  string
	style string
} {
	return []struct {
		label string
		key   string
		last  string
		style string
	}{
		{"running leases", "running", fmt.Sprint(l.s.Running), "state"},
		{"requests / s", "reqPerSec", fmt.Sprintf("%.1f", l.s.ReqPerSec), "spark"},
		{"creates / min", "createsPerMin", fmt.Sprintf("%.0f", l.s.CreatesPerMin), "spark"},
		{"egress conns", "fwConns", fmt.Sprint(l.s.FwConns), "spark"},
	}
}

// sparkW is the sparkline's width in cells, sparkGap the columns
// between two side-by-side sparkline columns.
const (
	sparkW   = 23
	sparkGap = 2
)

// drawThroughput draws the throughput panel at (x, y) in w cells and
// returns the row past its bottom edge. Four columns of label, current
// value and sparkline; at a wide frame all four sit on one row of
// columns, below it 2×2.
func (l *layout) drawThroughput(g *grid.Grid, x, y, w, h int) int {
	top := y
	id := "throughput"
	title := "throughput"
	if n := l.histMinutes(); n > 0 {
		title = fmt.Sprintf("throughput · last %d min", n)
	}
	y = l.panel(g, x, y, w, h, title, id)
	series := l.throughputSeries()
	perRow := 4
	if !l.wide() {
		perRow = 2
	}
	for i, r := range series {
		col, row := i%perRow, i/perRow
		valY := top + 1 + row*2
		spY := valY + 1
		cx := x + 2 + (sparkW+sparkGap)*col
		g.Text(cx, valY, r.label, "dim", 22)
		g.Right(cx+sparkW-1, valY, []grid.Seg{{Text: r.last, Style: "text"}})
		vals := l.histVals(r.key)
		sp := padTo(grid.Sparkline(vals, 0, maxOf(vals)), sparkW)
		g.Segs(cx, spY, []grid.Seg{{Text: sp, Style: r.style}}, sparkW)
	}
	return y
}

func (l *layout) throughput(g *grid.Grid, y int) int {
	return l.drawThroughput(g, 0, y, l.w, l.throughputH())
}

// imagesServices draws the images/services band at (0, y).
func (l *layout) imagesServices(g *grid.Grid, y int) int {
	if l.wide() {
		return l.drawImagesServices(g, y)
	}
	g.Mark(0, y, l.w, l.imagesServicesH(), "images-services")
	return l.drawImagesServices(g, y)
}

// leaseCols picks the leases panel's column layout for width w. At the
// wide frame it is the mockup's: id(12) image(17) owner(10) state(13)
// policy(11) age(6) left(9) holder — the columns start at 2, 14, 31,
// 41, 55, 67, 73, 81 (state keeps a clear cell before policy, so the
// burst marker can fill its column). Below 104 the flexible columns
// give (headers cut, values never reach the next column) and nothing
// overflows.
type leaseCols struct {
	id, img, own, st, pol, age, left, hold int // column start cells
	idW, imgW, ownW, stW, polW, leftW      int
}

func leaseLayout(w int) leaseCols {
	if w >= maxW {
		// The state column fits its longest word in full ("‖ suspended,
		// burst": 18), taking slack from the id (10 shown), the image
		// ("honey-go-worker" still fits) and left; the holder keeps 21.
		return leaseCols{
			id: 2, idW: 11,
			img: 13, imgW: 16,
			own: 29, ownW: 10,
			st: 39, stW: 19,
			pol: 58, polW: 11,
			age:  69,
			left: 76, leftW: 7,
			hold: 83,
		}
	}
	c := leaseCols{id: 2, idW: clamp(w/10, 6, 12)}
	c.img = c.id + c.idW + 1
	c.imgW = clamp(w/7, 8, 17)
	c.own = c.img + c.imgW + 1
	c.ownW = clamp(w/12, 5, 10)
	c.st = c.own + c.ownW + 1
	c.stW = clamp(w/7, 6, 17)
	c.pol = c.st + c.stW + 1
	c.polW = clamp(w/14, 5, 12)
	c.age = c.pol + c.polW + 1
	c.left = c.age + ageW
	c.leftW = clamp(w/12, 4, 9)
	c.hold = c.left + c.leftW + 1
	return c
}

func (l *layout) leasesH() int {
	n := len(l.s.Rows)
	if n > maxLeaseRows(l.w) {
		n = maxLeaseRows(l.w)
	}
	if n == 0 {
		return 4 // title + header + the "no live leases" row (+ frame)
	}
	return 3 + n // title + header + rows (+ frame handled by panel)
}

// maxLeaseRows bounds the leases panel: at least eight rows even at the
// minimum width, never more than sixteen.
func maxLeaseRows(w int) int { return min(16, max(8, (w-8)/6)) }

// ageW is the age column's width: "10h37m" and a separating space.
const ageW = 7

// cell fits s into a table column of width w, leaving the last column
// as space before the next cell: cut to w-1 runes, ending in … when cut.
func cell(s string, w int) string { return ellipsize(s, w-1) }

// ellipsize cuts s to at most n runes, ending in … when it was cut.
func ellipsize(s string, n int) string {
	r := []rune(s)
	if n <= 0 {
		return ""
	}
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// leases panel: id, image, owner, run state with its glyph, policy,
// age, time left (the hold's time when held, ∞ for a persistent lease
// with no hold) and the holder — ◆ before a held lease's holder, ◉
// when the hold has lapsed. On the page the holder is a link.
func (l *layout) leases(g *grid.Grid, y int) int {
	top := y
	y = l.panel(g, 0, y, l.w, l.leasesH(), "leases", "leases")
	c := leaseLayout(l.w)
	// Each header at its column's start, cut to the column's width so a
	// narrow frame cannot smear one header into the next.
	headers := []struct {
		x, w int
		text string
	}{{c.id, c.idW, "id"}, {c.img, c.imgW, "image"}, {c.own, c.ownW, "owner"},
		{c.st, c.stW, "state"}, {c.pol, c.polW, "policy"}, {c.age, 5, "age"},
		{c.left, c.leftW, "left"}, {c.hold, l.w - 2 - c.hold, "holder"}}
	for _, h := range headers {
		text := h.text
		switch text {
		case "policy":
			text = fitWord([]string{"policy", "net"}, h.w-1)
		case "holder":
			// The holder column's marks, spelled out where they are used.
			text = fitWord([]string{"holder (◆ held · ◉ lapsed hold)", "holder ◆ held ◉ lapsed",
				"◆ held · ◉ lapsed hold", "◆ held · ◉ lapsed", "holder"}, h.w)
		}
		g.Text(h.x, top+1, text, "dim", h.w)
	}

	rows := l.s.Rows
	if n := maxLeaseRows(l.w); len(rows) > n {
		rows = rows[:n]
	}
	for i, r := range rows {
		yy := top + 2 + i
		// Every cell keeps one column of space before the next and ends
		// in … when cut, so neighbouring cells never run together.
		// An id is shown as a prefix (the API accepts any unique
		// prefix), so it is cut without an ellipsis.
		g.Text(c.id, yy, sanitize(r.ID), "id", c.idW-1)
		g.Text(c.img, yy, cell(sanitize(r.Image), c.imgW), "text", c.imgW)
		g.Text(c.own, yy, cell(sanitize(r.Owner), c.ownW), "owner", c.ownW)
		// The state cell (#128, #129): the glyph always carries the run
		// state (▶ ‖ ■ ⭘, explained by the legend), and the word spells
		// it out with any qualifier — "running, burst", "preempted",
		// "idle-suspended". When the column is narrow the word steps down
		// through fixed shorter forms (stateWords), so the state is never
		// a cryptic suffix or a word cut mid-way.
		word := fitWord(stateWords(r), c.stW-2)
		segs := []grid.Seg{
			{Text: string(stateGlyph(r)), Style: stateGlyphStyle(r)},
			{Text: " " + word, Style: stateGlyphStyle(r)},
		}
		g.Segs(c.st, yy, segs, c.stW)
		g.Text(c.pol, yy, fitWord(policyWords(r.Policy), c.polW-1), "dim", c.polW)
		g.Text(c.age, yy, cell(r.Age, ageW), "dim", ageW)
		g.Text(c.left, yy, cell(leaseLeft(r), c.leftW), "text", c.leftW)
		l.holder(g, c, yy, r)
	}
	if len(rows) == 0 {
		g.Text(2, top+2, "no live leases", "dim", l.w-4)
	}
	return y
}

// stateWords is a lease row's state word, longest form first, each next
// one shorter: the run state with its qualifier (burst class,
// preemption, idle suspension).
func stateWords(r LeaseRow) []string {
	// The qualifier goes first as the column narrows; the state word
	// (or its short form) stays to the end.
	switch {
	case r.Preempted:
		return []string{"preempted", "preempt", "susp"}
	case r.IdleSuspended:
		return []string{"idle-suspended", "idle-susp", "susp"}
	case r.State == "running" && r.Burst:
		return []string{"running, burst", "run, burst", "running", "run"}
	case r.State == "running":
		return []string{"running", "run"}
	case r.State == "suspended" && r.Burst:
		return []string{"suspended, burst", "susp, burst", "suspended", "susp"}
	case r.State == "suspended":
		return []string{"suspended", "susp"}
	case r.State == "recovered":
		return []string{"recovered", "recov"}
	default:
		return []string{r.State}
	}
}

// policyWords is a network policy, longest form first.
func policyWords(p string) []string {
	switch p {
	case "restricted":
		return []string{"restricted", "rstr"}
	case "internet":
		return []string{"internet", "inet"}
	default:
		return []string{sanitize(p)}
	}
}

// fitWord is the first form that fits n cells, or the shortest form cut
// to n as a last resort.
func fitWord(forms []string, n int) string {
	for _, f := range forms {
		if len([]rune(f)) <= n {
			return f
		}
	}
	return ellipsize(forms[len(forms)-1], n)
}

// leaseLeft is the row's left column: a held lease shows the time left
// on its hold, a persistent lease without a hold ∞, the rest the
// lease's own expiry ("due" when it has passed).
func leaseLeft(r LeaseRow) string {
	if r.HoldState != "" && r.HoldExpires != "" {
		return r.HoldExpires
	}
	return r.Left
}

// holder draws the holder column: ◆ before a held lease's holder, ◉
// when the hold has lapsed; a lease's name when there is no holder; a
// lease's comment — dim, a CI job lease usually — when there is neither;
// a dash when nothing at all. Cut with … so nothing reaches the border.
func (l *layout) holder(g *grid.Grid, c leaseCols, yy int, r LeaseRow) {
	room := l.w - c.hold - 3 // one column clear of the border
	if r.Holder != "" {
		mark, style := "", "link"
		switch r.HoldState {
		case "active":
			mark = "◆ "
		case "lapsed":
			mark = "◉ "
		}
		if mark != "" {
			mw := len([]rune(mark))
			g.Segs(c.hold, yy, []grid.Seg{{Text: mark, Style: "state"}}, room)
			room -= mw
			g.Segs(c.hold+mw, yy, []grid.Seg{{Text: ellipsize(sanitize(r.Holder), room), Style: style}}, room)
			return
		}
		g.Segs(c.hold, yy, []grid.Seg{{Text: ellipsize(sanitize(r.Holder), room), Style: style}}, room)
		return
	}
	if r.Name != "" {
		g.Text(c.hold, yy, ellipsize(sanitize(r.Name), room), "dim", room)
		return
	}
	if r.Comment != "" {
		g.Text(c.hold, yy, ellipsize(sanitize(r.Comment), room), "dim", room)
		return
	}
	g.Text(c.hold, yy, "-", "dim", 1)
}

// images panel: the catalog — name, shape, live leases, lifetime uses,
// baked-at — beside the services panel at a wide frame, stacked below it.
// The empty panel keeps a row for "no images" instead of writing over
// its bottom border.
func (l *layout) imagesH() int {
	if len(l.s.Images) == 0 {
		return 4
	}
	return 3 + len(l.s.Images)
}

// maxServiceRows is the most unit rows the services panel shows before
// it folds the rest into a "+N more" row — the images panel's cap, so
// a host with many units cannot stretch the whole band.
const maxServiceRows = 6

// servicesH is the services panel's own height: title, one row per unit
// up to maxServiceRows ("no units configured" when none), a final
// "+N more" row when units were folded, and the bottom edge. Side by
// side the panel is drawn as tall as images instead — the taller of
// the two.
func (l *layout) servicesH() int {
	if len(l.s.Services) == 0 {
		return 3
	}
	return 3 + min(len(l.s.Services), maxServiceRows) + boolInt(len(l.s.Services) > maxServiceRows)
}

// imagesServicesH is the height of the images/services band: as one
// side-by-side pair, both boxes the taller panel's height; stacked,
// the two panels' heights together.
func (l *layout) imagesServicesH() int {
	if l.wide() {
		return max(l.imagesH(), l.servicesH())
	}
	return l.imagesH() + l.servicesH()
}

// drawImagesServices draws the images and services panels: images left
// (61 cells) and services right (41 cells) at a wide frame, both the
// taller one's height; both full width below it.
func (l *layout) drawImagesServices(g *grid.Grid, y int) int {
	if l.wide() {
		h := max(l.imagesH(), l.servicesH())
		l.drawImages(g, 0, y, imgPanelW, h)
		l.drawServices(g, imgPanelW+panelGap, y, l.w-imgPanelW-panelGap, h)
		return y + h
	}
	y = l.drawImages(g, 0, y, l.w, l.imagesH())
	return l.drawServices(g, 0, y, l.w, l.servicesH())
}

// imgPanelW is the width of the side-by-side images panel; services
// takes the rest of the 104-cell frame after the gap.
const imgPanelW = 61

// imageCol is one column of the images panel: its offset from the
// panel's left edge and its width in cells.
type imageCol struct {
	off, w int
}

// imageCols lays the images panel's columns out: image, vcpu, mem,
// live, uses, baked.
func imageCols(w int) []imageCol {
	cols := []imageCol{{2, 17}, {19, 6}, {25, 9}, {34, 6}, {40, 7}}
	cols = append(cols, imageCol{47, w - 4 - 47 + 1}) // baked, to the inner right edge
	return cols
}

// drawImages draws the images panel at (x, y) in w cells and returns
// the row past its bottom edge: a header row, then one row per image.
func (l *layout) drawImages(g *grid.Grid, x, y, w, h int) int {
	top := y
	y = l.panel(g, x, y, w, h, "images", "images")
	cols := imageCols(w)
	labels := []string{"image", "vcpu", "mem", "live", "uses", "baked"}
	for i, c := range cols {
		g.Text(x+c.off, top+1, labels[i], "dim", c.w)
	}
	for i, im := range l.s.Images {
		yy := top + 2 + i
		vals := []string{
			ellipsize(sanitize(im.Name), cols[0].w),
			fmt.Sprint(im.VCPU),
			memLabel(im.MemMB),
			fmt.Sprint(im.Live),
			fmt.Sprint(im.Uses),
			im.Updated,
		}
		for j, c := range cols {
			g.Text(x+c.off, yy, vals[j], "text", c.w)
		}
	}
	if len(l.s.Images) == 0 {
		g.Text(x+2, top+2, "no images", "dim", w-4)
	}
	return y
}

// memLabel is an image's memory: "2 GiB", "512 MiB".
func memLabel(mb int) string {
	if mb >= 1024 {
		return fmt.Sprintf("%d GiB", mb/1024)
	}
	return fmt.Sprintf("%d MiB", mb)
}

// drawServices draws the services panel at (x, y) in w cells and
// returns the row past its bottom edge: one row per systemd unit,
// the name padded to a fixed column, then the state — ✓ active, ◷ (or
// ○) a state on its way, ✗ a state that needs a person. When units
// outrun the rows the box has, the last row says "+N more".
func (l *layout) drawServices(g *grid.Grid, x, y, w, h int) int {
	top := y
	y = l.panel(g, x, y, w, h, "services", "services")
	if len(l.s.Services) == 0 {
		g.Text(x+2, top+1, "no units configured", "dim", w-4)
		return y
	}
	// Down units lead — they are what needs a person — then the active
	// ones in configured order; only then is the cap applied, so the ✗
	// rows are never the ones folded into "+N more".
	rows := make([]Service, 0, len(l.s.Services))
	for _, svc := range l.s.Services {
		if svc.State != "active" {
			rows = append(rows, svc)
		}
	}
	for _, svc := range l.s.Services {
		if svc.State == "active" {
			rows = append(rows, svc)
		}
	}
	// Fill the rows the box actually has: beside a taller images panel
	// that is more than the panel's own maxServiceRows. Fold into
	// "+N more" only when the units still do not fit.
	room := h - 3 // title, bottom edge, and the row servicesH keeps
	more := 0
	if len(rows) > room {
		rows, more = rows[:room-1], len(rows)-(room-1)
	}
	for i, svc := range rows {
		yy := top + 1 + i
		g.Text(x+2, yy, ellipsize(sanitize(svc.Name), 22), "text", 22)
		g.Segs(x+24, yy, []grid.Seg{serviceState(svc.State)}, w-4-22)
	}
	if more > 0 {
		g.Text(x+2, top+1+len(rows), fmt.Sprintf("+%d more", more), "dim", w-4)
	}
	return y
}

// serviceState is one unit's state as a styled segment: ✓ active (ok);
// ◷ activating, reloading or waiting (dim — on its way, not wrong;
// ○ when the font lacks ◷); ✗ anything else (bad).
func serviceState(state string) grid.Seg {
	switch {
	case state == "active":
		return grid.Seg{Text: "✓ active", Style: "ok"}
	case state == "activating" || state == "reloading" || state == "waiting":
		mark := "◷"
		if !grid.FontHas('◷') {
			mark = "○"
		}
		return grid.Seg{Text: mark + " " + state, Style: "dim"}
	default:
		return grid.Seg{Text: "✗ " + state, Style: "bad"}
	}
}

// refusals panel: the refusal and failure counters on one row, then
// create/resume latencies on the same row when they fit (3 rows: title,
// the row, frame) or on a second row (4 rows). Titled exactly
// "refusals and failures".
func (l *layout) refusalsH() int {
	return boolInt(!l.latenciesShareRow()) + 3
}

// refusalItems is the counters in the order they are drawn.
func (l *layout) refusalItems() []struct {
	label string
	n     int
} {
	return []struct {
		label string
		n     int
	}{
		{"auth", l.s.AuthFails},
		{"quota", l.s.Quota},
		{"throttled", l.s.Throttled},
		{"capacity", l.s.Capacity},
		{"build fails", l.s.BuildFails},
		{"lost leases", l.s.ByState["lost"]},
	}
}

// refusalSegs is the counters joined with two spaces, non-zero values
// in the warn style.
func (l *layout) refusalSegs() []grid.Seg {
	segs := []grid.Seg{}
	for _, p := range l.refusalItems() {
		style := "dim"
		if p.n != 0 {
			style = "warn"
		}
		if len(segs) > 0 {
			segs = append(segs, grid.Seg{Text: "  ", Style: "dim"})
		}
		segs = append(segs,
			grid.Seg{Text: p.label + " ", Style: "dim"},
			grid.Seg{Text: fmt.Sprint(p.n), Style: style})
	}
	return segs
}

// latencySegs is "create N ms · resume N s"; a dash for a latency that
// never happened, and nothing at all when the frame cannot hold the
// counters plus the latencies.
func (l *layout) latencySegs() []grid.Seg {
	return []grid.Seg{
		{Text: "create ", Style: "dim"},
		{Text: msLabel(l.s.CreateMs), Style: "text"},
		{Text: "  resume ", Style: "dim"},
		{Text: msLabel(l.s.ResumeMs), Style: "text"},
	}
}

func (l *layout) refusals(g *grid.Grid, y int) int {
	top := y
	h := l.refusalsH()
	y = l.panel(g, 0, y, l.w, h, "refusals and failures", "refusals")
	counters := fitItems(l.refusalSegs(), l.w-4)
	lat := l.latencySegs()
	g.Segs(2, top+1, counters, l.w-4)
	if l.latenciesShareRow() {
		g.Segs(2+segWidth(counters)+2, top+1, lat, l.w-4-segWidth(counters)-2)
	} else {
		g.Segs(2, top+2, lat, l.w-4)
	}
	return y
}

// latenciesShareRow reports whether the counters and the create/resume
// latencies fit on one row inside the frame (the latencies then sit on
// the same line; otherwise they take a second row).
func (l *layout) latenciesShareRow() bool {
	return segWidth(l.refusalSegs())+2+segWidth(l.latencySegs()) <= l.w-4
}

// msLabel is a latency: "–" when none happened, "250 ms" or "4.1 s".
func msLabel(v float64) string {
	switch {
	case v < 0:
		return "–"
	case v < 1000:
		return fmt.Sprintf("%.0f ms", v)
	default:
		return fmt.Sprintf("%.1f s", v/1000)
	}
}

// events panel: the backend's lease event stream, newest first — one
// line per event: HH:MM:SS, the type padded to 10, the lease id to 10,
// then the holder, else the comment, else the owner. Styled by type
// (lost and held-lease actions warn, releases dim). The panel is
// omitted entirely when the collector has nothing to show.
func (l *layout) eventsH() int {
	if len(l.s.Events) == 0 {
		return 0
	}
	return 2 + len(l.s.Events)
}

func (l *layout) events(g *grid.Grid, y int) int {
	if len(l.s.Events) == 0 {
		return y
	}
	top := y
	y = l.panel(g, 0, y, l.w, l.eventsH(), "events", "events")
	for i, e := range l.s.Events {
		segs := splitSegs(sanitize(e.Text), e.Style)
		l.writeRow(g, 2, top+1+i, segs)
	}
	return y
}

// writeRow writes the styled runs of one row at (x, y), rune by rune:
// one Put per cell. (Segs would run the row's last run over the inner
// padding, restyling the whole tail to that run's style.)
func (l *layout) writeRow(g *grid.Grid, x, y int, segs []grid.Seg) {
	for _, s := range segs {
		for _, r := range []rune(s.Text) {
			if x > l.w-3 {
				return
			}
			g.Put(x, y, r, s.Style)
			x++
		}
	}
}

// eventTypeStyle is the colour the events panel's type word takes: the
// title cyan for the lease lifecycle (created, released, resumed,
// restarted, restored, checkpointed, recovered), warn for a lease put
// aside (suspended, preempted, idle_suspended, queued), bad for one lost
// or timed out, ok for spoond's own catalog gc, dim for anything else.
func eventTypeStyle(t string) string {
	switch t {
	case "created", "released", "resumed", "restarted", "restored", "checkpointed", "recovered":
		return "title"
	case "suspended", "preempted", "idle_suspended", "queued":
		return "warn"
	case "lost", "timed_out":
		return "bad"
	case "gc":
		return "ok"
	default:
		return "dim"
	}
}

// splitSegs breaks one event line into styled runs at its three column
// separators — the run of spaces between time, type, lease id and tail:
// the time stays dim, the type takes its kind's colour, the lease id the
// id style, the tail the event's own style, the separator runs themselves
// dim. The type and id columns are padded, so their padding merges with
// the separator: the whole run of spaces is consumed as one separator,
// and one style across the whole line would paint the padding dim too.
// Only those three separators split the line: the tail is free text (a
// comment can hold two spaces in a row) and is never cut again, so it
// keeps one style to the panel's edge.
func splitSegs(line, style string) []grid.Seg {
	segs := make([]grid.Seg, 0, 7)
	rest := line
	chunk := 0
	for fields := 0; fields < 3; fields++ {
		i := strings.Index(rest, "  ")
		if i < 0 {
			break
		}
		j := i
		for j < len(rest) && rest[j] == ' ' {
			j++
		}
		if i > 0 {
			st := style
			switch chunk {
			case 0:
				st = "dim" // the HH:MM:SS before the first separator
			case 1:
				st = eventTypeStyle(strings.TrimSpace(rest[:i])) // the type word's kind colour
			case 2:
				st = "id" // the lease id, cyan like the leases table
			}
			segs = append(segs, grid.Seg{Text: rest[:i], Style: st})
			chunk++
		}
		segs = append(segs, grid.Seg{Text: rest[i:j], Style: "dim"})
		rest = rest[j:]
	}
	if rest != "" {
		segs = append(segs, grid.Seg{Text: rest, Style: style})
	}
	return segs
}

// sanitize replaces control characters — anything below 0x20 and 0x7f,
// and the C1 range 0x80–0x9f — with '?': lease names, owners, images and
// holder text come from users, and nothing may inject terminal escapes
// into spoond top or odd runes into the page.
func sanitize(s string) string {
	// Control characters and anything the shipped font cannot draw
	// (accented Latin is fine; double-width scripts are not) become '?',
	// so names from users always pass grid.Check and keep columns aligned.
	return grid.Sanitize(s)
}

// gcLabel is the GC's mode for the panels: "-" when the scrape had
// neither the counter nor the environment to read.
func gcLabel(mode string) string {
	if mode == "" {
		return "-"
	}
	return mode
}

// gcSeg is the GC's mode as one segment: bold ok when deletion is on,
// dim when it is a dry run.
func gcSeg(mode string) grid.Seg {
	if mode == "delete" {
		return grid.Seg{Text: "delete on", Style: "ok"}
	}
	return grid.Seg{Text: gcLabel(mode), Style: "dim"}
}

// padTo right-pads s with spaces to n cells (no-op when already longer).
func padTo(s string, n int) string {
	for len([]rune(s)) < n {
		s += " "
	}
	return s
}

// maxOf is the greatest value in vs, or 0.
func maxOf(vs []float64) float64 {
	m := 0.0
	for _, v := range vs {
		if v > m {
			m = v
		}
	}
	return m
}

// segWidth is the total width of segs in cells.
func segWidth(segs []grid.Seg) int {
	n := 0
	for _, s := range segs {
		n += len([]rune(s.Text))
	}
	return n
}

// clamp is the usual three-way clamp.
func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
