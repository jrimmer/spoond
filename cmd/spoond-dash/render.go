// The dashboard on the character grid (#110): one fixed-width grid holds
// the whole frame — header, legend, the attention strip when something
// needs a person, then a column of panels. The same grid feeds Plain
// (golden tests), ANSI (spoond top) and HTML (the page's <pre>), so the
// terminal and the browser draw one picture from one snapshot.
//
// Styles are names, not colours: Plain drops them, ANSI maps them (see
// topStyles in top.go), HTML to g-<name> classes styled once in the
// page's palette.
package spoonddash

import (
	"fmt"
	"math"
	"regexp"
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
// rule, ▲ the attention strip's marker, ┄ a held-lease action in the
// events panel, ▲ the attention strip, ■ the lost state, ∞ a
// persistent lease's remaining time, ◉ a lapsed hold, and the leases
// panel's per-state glyphs (▶ running, ‖ suspended, ◆ held). It is
// passed to grid.Check by every renderer, and every rune is asserted to
// be in the shipped JetBrains Mono (TestExtraGlyphsInFont).
const Extra = "✓✗·═▲┄■∞◉" + stateGlyphs

// stateGlyphs are the leases panel's per-state glyphs.
const stateGlyphs = "▶‖◆"

// glyphs is the full set every renderer checks against: the grid
// package's own glyphs plus the dashboard's extras.
func glyphs() string { return grid.Glyphs + Extra }

// stateGlyph names a lease state: ▶ running (and recovered, which acts
// like running), ‖ suspended, ◆ held (a hold in any state), ■ lost.
func stateGlyph(r LeaseRow) rune {
	if r.State == "lost" {
		return '■'
	}
	if r.HoldState != "" {
		return '◆'
	}
	if r.State == "suspended" {
		return '‖'
	}
	return '▶'
}

// stateGlyphStyle is the style a lease's glyph and state are drawn in.
func stateGlyphStyle(r LeaseRow) string {
	if r.State == "lost" {
		return "bad"
	}
	if r.HoldState != "" {
		return "state"
	}
	if r.State == "suspended" {
		return "warn"
	}
	return "ok"
}

// legendRow is the legend line under the header: what each glyph means,
// so the leases and units panels need no legends of their own.
func legendRow() []grid.Seg {
	seg := func(g, name string) []grid.Seg {
		return []grid.Seg{{Text: g, Style: "state"}, {Text: " " + name, Style: "dim"}}
	}
	segs := []grid.Seg{}
	for _, p := range []struct{ g, name string }{
		{"▶", "running"}, {"‖", "suspended"}, {"◆", "held"}, {"■", "lost"},
	} {
		segs = append(segs, seg(p.g, p.name)...)
		segs = append(segs, grid.Seg{Text: " · ", Style: "dim"})
	}
	segs = append(segs,
		grid.Seg{Text: "✓ active unit", Style: "dim"},
		grid.Seg{Text: " · ", Style: "dim"},
		grid.Seg{Text: "◉ lapsed hold", Style: "state"},
		grid.Seg{Text: " · ", Style: "dim"},
		grid.Seg{Text: "┄ held-lease action", Style: "dim"})
	return segs
}

// fitSegs drops whole legend items from the right (an item ends at a
// " · " separator) until the row fits w, so a centred legend on a narrow
// frame loses its least important entries instead of being cut at both
// edges.
func fitSegs(segs []grid.Seg, w int) []grid.Seg {
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
			if segs[i].Text == " · " {
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

// bannerRows returns the attention strip's rows — one per trigger, in
// the banner style — or nil when nothing needs a person (the strip is
// then never drawn; the frame just starts with the panels). The rows
// carry the message only; the ▲ marker is prepended at draw time.
// Triggers:
//
//   - a systemd unit not active,
//   - a lost lease,
//   - free hugepages or snapshot disk past the danger level,
//   - the served TLS certificate (TLS_CERT) within 30 days of expiring,
//   - an automatic held-lease action in the last 24 h (from last_action).
func bannerRows(s Snapshot, now time.Time) []string {
	var rows []string
	for _, svc := range s.Services {
		if svc.State != "active" {
			rows = append(rows, fmt.Sprintf("unit %s is %s", svc.Name, svc.State))
		}
	}
	if s.ByState["lost"] > 0 {
		rows = append(rows, fmt.Sprintf("%d lost lease(s) - a substrate crash dropped them", s.ByState["lost"]))
	}
	if s.HugeFreeGiB > 0 && s.HugeUsedPct >= 92 {
		rows = append(rows, fmt.Sprintf("hugepages only %.1f GiB free - past the danger level", s.HugeFreeGiB))
	}
	if s.DiskUsedPct >= 90 {
		rows = append(rows, fmt.Sprintf("snapshot disk %.0f%% used - past the danger level", s.DiskUsedPct))
	}
	if !s.CertNotAfter.IsZero() {
		if d := s.CertNotAfter.Sub(now); d < 30*24*time.Hour {
			rows = certBanner(rows, d, s.CertNotAfter)
		}
	}
	for _, r := range s.Rows {
		if !r.LastActionAt.IsZero() && now.Sub(r.LastActionAt) < 24*time.Hour {
			rows = append(rows, fmt.Sprintf("held lease %s: %s %s ago", r.ID, r.LastAction, dur(now.Sub(r.LastActionAt))))
		}
	}
	return rows
}

// certBanner appends the TLS certificate's row: at 30 days it needs a
// person before basic auth starts failing; inside 7 days it is urgent.
func certBanner(rows []string, d time.Duration, notAfter time.Time) []string {
	when := notAfter.Format("2006-01-02")
	if d < 0 {
		return append(rows, "the TLS certificate (TLS_CERT) expired "+when)
	}
	if d < 7*24*time.Hour {
		return append(rows, fmt.Sprintf("the TLS certificate expires in %s (%s) - renew it", dur(d), when))
	}
	return append(rows, fmt.Sprintf("the TLS certificate expires in %s (%s)", dur(d), when))
}

// Draw renders the whole frame at width w. now timestamps the banner and
// the ages; host names the node in the header. The sparklines draw
// empty (no history): goldens that need them call drawFrame directly.
func Draw(s Snapshot, w int, now time.Time, host string) *grid.Grid {
	g, err := drawFrame(s, nil, w, host, now)
	if err != nil {
		// Callers of Draw (tests, one-off renders) pass fixed snapshots;
		// a Check failure is a programming error worth panicking on.
		panic(err)
	}
	return g
}

// bannerSegs converts bannerRows' strings to the styled segments the
// frame draws: one banner-styled row per trigger, each led by ▲.
// Shared by Draw, the page/top renderers and the tests, so the strip is
// styled one way.
func bannerSegs(rows []string) [][]grid.Seg {
	if len(rows) == 0 {
		return nil
	}
	out := make([][]grid.Seg, len(rows))
	for i, r := range rows {
		out[i] = []grid.Seg{{Text: "▲ ", Style: "banner"}, {Text: r, Style: "banner"}}
	}
	return out
}

// drawFrame renders a snapshot plus history into the full frame at
// width w: the shared entry point of Draw, the page, the stream and
// spoond top. host names the node in the header; now timestamps the
// banner and the ages. It fails only when grid.Check rejects the
// finished frame (a rune no renderer can draw) — the terminal path
// reports it instead of printing a broken frame.
func drawFrame(s Snapshot, hist map[string][]float64, w int, host string, now time.Time) (*grid.Grid, error) {
	l := &layout{w: clamp(w, minW, maxW), host: host, now: now, s: s,
		banner: bannerSegs(bannerRows(s, now)),
		histFn: func(k string) []float64 { return hist[k] }}
	g := l.assemble()
	if err := g.Check(glyphs()); err != nil {
		return nil, err
	}
	return g, nil
}

// layout assembles the grid panel by panel.
type layout struct {
	w      int
	banner [][]grid.Seg
	host   string
	now    time.Time
	s      Snapshot
	// histFn serves the sparkline series; Draw leaves it nil (the
	// sparklines then draw empty) and the page/top fill it from the
	// collector's history.
	histFn func(string) []float64
}

// assemble draws the header, the banner and every panel into one grid.
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
		len(l.banner) + boolInt(len(l.banner) > 0) + // banner rows + a blank row under them
		l.panelsH() + l.throughputH() + l.leasesH() +
		l.imagesH() + l.servicesH() + l.refusalsH() + l.eventsH() +
		1 // the status line

	g := grid.New(l.w, h)
	l.header(g, 0)
	y := headerRows()

	// One attention strip row per trigger, painted full width in the
	// banner style, then a blank row — only when something needs a
	// person.
	for _, segs := range l.banner {
		g.Segs(0, y, segs, l.w)
		g.Mark(0, y, l.w, 1, "banner")
		g.PaintRow(y, "banner")
		y++
	}
	if len(l.banner) > 0 {
		y++
	}

	y = l.drawPanels(g, y)
	y = l.throughput(g, y)
	y = l.leases(g, y)
	y = l.images(g, y)
	y = l.servicesPanel(g, y)
	y = l.refusals(g, y)
	y = l.events(g, y)
	l.statusLine(g, y, l.s.At)
	return g
}

// statusItem is one status-line entry: label, the value shown in
// brackets and whether it is healthy (ok style) or not (warn/bad).
type statusItem struct {
	label string
	value string
	style string
}

// statusItems builds the status line's entries left to right: leases,
// hugepages, snapshot disk, the certificate's remaining days and the
// units. The styles reuse the thresholds the meters and the attention
// strip already use. The certificate is left out when there is none.
func statusItems(s Snapshot, now time.Time) []statusItem {
	items := []statusItem{
		{"leases", fmt.Sprintf("%d/%d", s.Running, s.Limit), "ok"},
	}
	if s.Limit > 0 {
		if pct := float64(s.Running) / float64(s.Limit) * 100; pct >= 90 {
			items[0].style = "bad"
		} else if pct >= 75 {
			items[0].style = "warn"
		}
	}
	switch {
	case s.HugeUsedPct >= 92:
		items = append(items, statusItem{"hugepages", fmt.Sprintf("%.1f GiB free", s.HugeFreeGiB), "bad"})
	case s.HugeUsedPct >= 80:
		items = append(items, statusItem{"hugepages", fmt.Sprintf("%.1f GiB free", s.HugeFreeGiB), "warn"})
	default:
		items = append(items, statusItem{"hugepages", fmt.Sprintf("%.1f GiB free", s.HugeFreeGiB), "ok"})
	}
	switch {
	case s.DiskUsedPct >= 90:
		items = append(items, statusItem{"disk", fmt.Sprintf("%.1f GiB free", s.DiskFreeGiB), "bad"})
	case s.DiskUsedPct >= 80:
		items = append(items, statusItem{"disk", fmt.Sprintf("%.1f GiB free", s.DiskFreeGiB), "warn"})
	default:
		items = append(items, statusItem{"disk", fmt.Sprintf("%.1f GiB free", s.DiskFreeGiB), "ok"})
	}
	if !s.CertNotAfter.IsZero() {
		items = append(items, statusItem{"cert", certDays(s.CertNotAfter.Sub(now)), certStyle(s.CertNotAfter.Sub(now))})
	}
	units, down := 0, 0
	for _, svc := range s.Services {
		units++
		if svc.State != "active" {
			down++
		}
	}
	if units > 0 {
		st := "ok"
		if down > 0 {
			st = "bad"
		}
		items = append(items, statusItem{"units", fmt.Sprintf("%d/%d", units-down, units), st})
	}
	return items
}

// certDays is the certificate's remaining time in days: negative when
// it has already expired.
func certDays(d time.Duration) string {
	return fmt.Sprintf("%dd", int(math.Floor(d.Hours()/24)))
}

// certStyle is the certificate's health: bad once expired, warn inside
// the attention strip's 30-day window, ok before it.
func certStyle(d time.Duration) string {
	switch {
	case d < 0:
		return "bad"
	case d < 30*24*time.Hour:
		return "warn"
	default:
		return "ok"
	}
}

// statusLine draws the frame's last row, outside any box: label
// [value] entries left to right, the clock right-aligned on the same
// row. At narrow widths entries are dropped from the right (cert, then
// units) until the line fits.
func (l *layout) statusLine(g *grid.Grid, y int, at string) {
	items := statusItems(l.s, l.now)
	// Drop from the right until what remains fits, the clock always
	// kept.
	for len(items) > 0 && statusW(items)+clockW(at, l.w) > l.w {
		items = items[:len(items)-1]
	}
	segs := []grid.Seg{}
	for _, it := range items {
		segs = append(segs,
			grid.Seg{Text: it.label + " ", Style: "dim"},
			grid.Seg{Text: "[", Style: "dim"},
			grid.Seg{Text: it.value, Style: it.style},
			grid.Seg{Text: "]  ", Style: "dim"})
	}
	g.Segs(0, y, segs, l.w)
	g.Right(l.w-1, y, []grid.Seg{{Text: at, Style: "dim"}})
}

// statusW is the width the items draw at: label, brackets and two
// trailing spaces each (the last pair included, so the math ignores
// where the line ends).
func statusW(items []statusItem) int {
	n := 0
	for _, it := range items {
		n += len(it.label) + len(it.value) + 5
	}
	return n
}

// clockW is the clock's footprint: its cells plus the gap that keeps
// it clear of the items (at least two columns, on the narrowest frame
// just its own width).
func clockW(at string, w int) int {
	gap := 2
	if w <= minW {
		gap = 1
	}
	return len(at) + gap
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// header: the title line centred — SPOOND · host · version · e2b orch
// · uptime — an ═ rule across the full width, and the legend row, also
// centred. The frame time is gone: the status line's clock replaced it.
func (l *layout) header(g *grid.Grid, y int) {
	segs := []grid.Seg{
		{Text: "SPOOND", Style: "head"},
		{Text: " · ", Style: "dim"},
		{Text: l.host, Style: "text"},
		{Text: " · ", Style: "dim"},
		{Text: versionLabel(dashVersion), Style: "text"},
		{Text: " · ", Style: "dim"},
		{Text: "e2b " + versionLabel(l.s.Version), Style: "text"},
		{Text: " · ", Style: "dim"},
		{Text: "up " + fmt.Sprintf("%.0fh", l.s.UptimeH), Style: "text"},
	}
	g.Center(l.w/2, y, segs)
	for x := 0; x < l.w; x++ {
		g.Put(x, y+1, '═', "frame")
	}
	g.Center(l.w/2, y+2, fitSegs(legendRow(), l.w))
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
// header (headerRows) + banner rows (+ a blank under them) + the panels above
// leases, then the panel's title row, then one row per lease.
func holderLinks(s Snapshot, w int, now time.Time) []linkAt {
	l := &layout{w: clamp(w, minW, maxW), s: s, now: now,
		banner: bannerSegs(bannerRows(s, now))}
	base := headerRows() + len(l.banner) + boolInt(len(l.banner) > 0) +
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
func headerRows() int { return 3 }

// capacity panel: the running meter, the leases line, queued, granted,
// swept, then one row per image with live leases. H is the height the
// panel shares with host when the two sit side by side.
func (l *layout) capacityH() int {
	h := 3 + len(l.capacityRows()) // title + rows (+ frame)
	if l.imageRows() > 0 {
		h += l.imageRows() + 1 // a ┄ rule, then the image rows
	}
	return h
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

	// Leases: total, then running, suspended and lost counts. A zero
	// count is dim; recovered appears only when non-zero.
	segs := []grid.Seg{{Text: fmt.Sprintf("%d", l.s.Leases), Style: "text"}}
	for _, st := range []struct {
		name string
		n    int
	}{
		{"running", l.s.ByState["running"]},
		{"suspended", l.s.ByState["suspended"]},
		{"lost", l.s.ByState["lost"]},
	} {
		segs = append(segs, grid.Seg{Text: " · ", Style: "dim"})
		style := "text"
		if st.n == 0 {
			style = "dim"
		}
		segs = append(segs,
			grid.Seg{Text: fmt.Sprintf("%d", st.n), Style: style},
			grid.Seg{Text: " " + st.name, Style: "dim"})
	}
	if n := l.s.ByState["recovered"]; n > 0 {
		segs = append(segs,
			grid.Seg{Text: " · ", Style: "dim"},
			grid.Seg{Text: fmt.Sprintf("%d", n), Style: "text"},
			grid.Seg{Text: " recovered", Style: "dim"})
	}
	rows = append(rows, capacityRow{segs: segs})

	rows = append(rows, capacityRow{segs: dimLine(
		fmt.Sprintf("queued %s", fmt.Sprint(l.s.Queued)),
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
		segs := r.segs
		if r.dim {
			segs = make([]grid.Seg, len(r.segs))
			copy(segs, r.segs)
			for i := range segs {
				segs[i].Style = "dim"
			}
		}
		g.Segs(x+2, row, segs, inner)
		if r.right != "" {
			g.Right(x+w-4, row, []grid.Seg{{Text: r.right, Style: "text"}})
		}
		row++
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
		if warnPct > 0 && warnPct < 100 {
			tx := int(warnPct / 100 * float64(barW))
			if tx < barW {
				bar[tx] = '╎'
			}
		}
		segs = append(segs, grid.Seg{Text: string(bar), Style: style})
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
}

// hostPanelRows builds the host panel's rows: cpu, memory, hugepages,
// snapshot disk, root disk — the meters and levels the old page already
// showed — plus the allocated line.
func hostPanelRows(s Snapshot) []hostRow {
	return []hostRow{
		{"cpu", s.CPUPct, 75, 90, fmt.Sprintf("%.0f%%  load %.1f  %d cores", s.CPUPct, s.Load1, s.Cores)},
		{"memory", s.MemUsedPct, 80, 92, fmt.Sprintf("%.1f of %.1f GiB", s.MemUsedGiB, s.MemTotalGiB)},
		{"hugepages", s.HugeUsedPct, 80, 92, fmt.Sprintf("%.1f GiB free", s.HugeFreeGiB)},
		{"snapshot disk", s.DiskUsedPct, 80, 90, fmt.Sprintf("%.1f GiB free", s.DiskFreeGiB)},
		{"root disk", s.RootUsedPct, 75, 90, fmt.Sprintf("%.1f GiB free", s.RootFreeGiB)},
	}
}

// hostH is the host panel's own height: title, meters, rule, the two
// tail rows and the frame's bottom edge. Side by side, the panel is
// drawn as tall as capacity instead — the taller of the two.
func (l *layout) hostH() int {
	return 1 + len(hostPanelRows(l.s)) + 1 + 2 + 1
}

// hostRows builds the host panel's meter rows: cpu, memory, hugepages,
// snapshot disk, root disk — the meters and levels the old page already
// showed. right is the value text at the row's end.
func (l *layout) hostRows() []hostRow {
	return []hostRow{
		{"cpu", l.s.CPUPct, 75, 90, fmt.Sprintf("%.0f%% · load %.1f", l.s.CPUPct, l.s.Load1)},
		{"memory", l.s.MemUsedPct, 80, 92, fmt.Sprintf("%.1f of %.1f GiB", l.s.MemUsedGiB, l.s.MemTotalGiB)},
		{"hugepages", l.s.HugeUsedPct, 80, 92, fmt.Sprintf("%.1f GiB free", l.s.HugeFreeGiB)},
		{"snapshot disk", l.s.DiskUsedPct, 80, 90, fmt.Sprintf("%.1f GiB free", l.s.DiskFreeGiB)},
		{"root disk", l.s.RootUsedPct, 75, 90, fmt.Sprintf("%.1f GiB free", l.s.RootFreeGiB)},
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
		g.Segs(x+2, row, l.meterSegs(r.label, r.pct, r.warn, r.danger, meterBarW), inner)
		g.Right(x+w-4, row, []grid.Seg{{Text: r.right, Style: "text"}})
		row++
	}
	l.rule(g, x, w, row)
	row++

	alloc := dimLine(
		fmt.Sprintf("vcpu alloc %d / %d", l.s.VCPUAlloc, l.s.Cores),
		fmt.Sprintf("mem alloc %.1f GiB", l.s.MemAllocGiB))
	alloc[0].Style = "text"
	alloc[2].Style = "text"
	g.Segs(x+2, row, alloc, inner)
	row++

	segs := []grid.Seg{gcSeg(l.s.GCMode)}
	if l.s.GCDeleted > 0 {
		segs = append(segs,
			grid.Seg{Text: " · ", Style: "dim"},
			grid.Seg{Text: fmt.Sprintf("%s builds deleted", thousands(l.s.GCDeleted)), Style: "text"})
	}
	g.Segs(x+2, row, segs, inner)
	return y
}

// throughput panel: running leases, requests/s, creates/min, egress
// connections as sparklines over the collector's history.
func (l *layout) throughputH() int { return 6 } // title + four sparkline rows + frame

func (l *layout) throughput(g *grid.Grid, y int) int {
	rows := []struct {
		label string
		key   string
		vals  []float64
		last  string
	}{
		{"running", "running", nil, fmt.Sprint(l.s.Running)},
		{"req/s", "reqPerSec", nil, fmt.Sprintf("%.1f", l.s.ReqPerSec)},
		{"creates/min", "createsPerMin", nil, fmt.Sprintf("%.0f", l.s.CreatesPerMin)},
		{"egress conns", "fwConns", nil, fmt.Sprint(l.s.FwConns)},
	}
	h := l.throughputH()
	top := y
	y = l.panel(g, 0, y, l.w, h, "throughput (5 min)", "throughput")
	spW := 24
	for i, r := range rows {
		yy := top + 1 + i
		g.Text(2, yy, r.label, "dim", 12)
		vals := r.vals
		if l.histFn != nil {
			vals = l.histFn(r.key)
		}
		sp := padTo(grid.Sparkline(vals, 0, maxOf(vals)), spW)
		g.Segs(15, yy, []grid.Seg{{Text: sp, Style: "spark"}}, spW)
		g.Right(l.w-3, yy, []grid.Seg{{Text: r.last, Style: "text"}})
	}
	return y
}

// leaseCols picks the leases panel's column layout for width w.
type leaseCols struct {
	id, img, own, st, pol, age, left, hold int // column start cells
	idW, imgW, ownW, stW, polW             int
}

func leaseLayout(w int) leaseCols {
	c := leaseCols{id: 2, idW: 10}
	c.img = c.id + c.idW + 1
	c.imgW = clamp(w/9, 9, 16)
	c.own = c.img + c.imgW + 1
	c.ownW = clamp(w/11, 6, 12)
	c.st = c.own + c.ownW + 1
	c.stW = 11
	c.pol = c.st + c.stW + 1
	c.polW = clamp(w/16, 6, 10)
	c.age = c.pol + c.polW + 1
	c.left = c.age + 6
	c.hold = c.left + 6
	return c
}

// meterBarW is the panels' fixed meter bar width: 16 cells in a
// side-by-side panel (label 13 + one gap + the bar + one gap + a value
// of at most 12 cells fills 47 of the 49 inner columns); full width
// stacks keep it, matching the mockup.
func (l *layout) meterBarW() int {
	barW := l.w - 4 - meterLabelW - 14 - maxMeterValueW
	if barW < 8 {
		barW = 8
	}
	if barW > 16 {
		barW = 16
	}
	return barW
}

// maxMeterValueW is the widest value a meter shows in a side-by-side
// panel: "121.5 GiB free".
const maxMeterValueW = 15

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

// leases panel: id, image, owner, state with a glyph, policy, age, left
// and the holder (the holder text; a lapsed hold shows ◉ after it).
func (l *layout) leases(g *grid.Grid, y int) int {
	top := y
	y = l.panel(g, 0, y, l.w, l.leasesH(), "leases", "leases")
	c := leaseLayout(l.w)
	// Each header at its column's start, so it lines up with the rows.
	for _, h := range []struct {
		x    int
		text string
	}{{c.id, "lease"}, {c.img, "image"}, {c.own, "owner"}, {c.st, "state"}, {c.pol, "net"}, {c.age, "age"}, {c.left, "left"}, {c.hold, "holder"}} {
		g.Text(h.x, top+1, h.text, "dim", l.w-2-h.x)
	}

	rows := l.s.Rows
	if n := maxLeaseRows(l.w); len(rows) > n {
		rows = rows[:n]
	}
	for i, r := range rows {
		yy := top + 2 + i
		g.Text(c.id, yy, sanitize(r.ID), "text", c.idW)
		g.Text(c.img, yy, sanitize(r.Image), "text", c.imgW)
		g.Text(c.own, yy, sanitize(r.Owner), "text", c.ownW)
		segs := []grid.Seg{
			{Text: string(stateGlyph(r)), Style: stateGlyphStyle(r)},
			{Text: " " + r.State, Style: stateGlyphStyle(r)},
		}
		g.Segs(c.st, yy, segs, c.stW)
		g.Text(c.pol, yy, r.Policy, "dim", c.polW)
		g.Text(c.age, yy, r.Age, "dim", 5)
		g.Text(c.left, yy, r.Left, "text", 5)
		if r.Holder != "" {
			room := l.w - c.hold - 3 // one column clear of the border
			lapsed := ""
			if r.HoldState == "lapsed" {
				// The holder keeps at least a few columns: a narrow frame
				// gets the bare glyph instead of the word.
				lapsed = " ◉lapsed"
				if room-len([]rune(lapsed)) < 6 {
					lapsed = " ◉"
				}
				room -= len([]rune(lapsed))
			}
			seg := []grid.Seg{{Text: ellipsize(sanitize(r.Holder), room), Style: "link"}}
			if lapsed != "" {
				seg = append(seg, grid.Seg{Text: lapsed, Style: "warn"})
			}
			g.Segs(c.hold, yy, seg, l.w-c.hold-2)
		} else if r.Name != "" {
			// no holder: show the lease's name instead
			g.Text(c.hold, yy, sanitize(r.Name), "dim", l.w-c.hold-2)
		} else {
			g.Text(c.hold, yy, "-", "dim", 1)
		}
	}
	if len(rows) == 0 {
		g.Text(2, top+2, "no live leases", "dim", l.w-4)
	}
	return y
}

// images panel: the catalog — name, live leases, lifetime uses, shape,
// baked-at.
func (l *layout) imagesH() int { return 3 + len(l.s.Images) }

func (l *layout) images(g *grid.Grid, y int) int {
	top := y
	y = l.panel(g, 0, y, l.w, l.imagesH(), "images", "images")
	nameW := clamp(l.w-40, 12, 44)
	cx := 2 + nameW + 1 // the numbers' column, after the padded name
	g.Text(2, top+1, "image", "dim", nameW)
	g.Text(cx, top+1, "live uses  shape        baked", "dim", l.w-2-cx)
	for i, im := range l.s.Images {
		yy := top + 2 + i
		g.Text(2, yy, sanitize(im.Name), "text", nameW)
		g.Text(cx, yy, fmt.Sprintf("%4d %4d  %-11s  %s", im.Live, im.Uses, fmt.Sprintf("%dv/%dM", im.VCPU, im.MemMB), im.Updated), "dim", l.w-2-cx)
	}
	return y
}

// services panel: one row per systemd unit, ✓ when active, otherwise
// the state in the bad style. Wide frames use two columns.
func (l *layout) servicesH() int {
	n := len(l.s.Services)
	if n == 0 {
		return 3
	}
	if l.w >= 84 {
		return 2 + (n+1)/2
	}
	return 2 + n
}

func (l *layout) servicesPanel(g *grid.Grid, y int) int {
	top := y
	y = l.panel(g, 0, y, l.w, l.servicesH(), "units", "services")
	if len(l.s.Services) == 0 {
		g.Text(2, top+1, "no units configured", "dim", l.w-4)
		return y
	}
	twoCol := l.w >= 84
	half := (len(l.s.Services) + 1) / 2
	colW := (l.w - 8) / 2
	if !twoCol {
		colW = l.w - 4
	}
	for i, svc := range l.s.Services {
		col, row := 0, i
		if twoCol && i >= half {
			col, row = 1, i-half
		}
		segs := []grid.Seg{{Text: "✓ ", Style: "ok"}, {Text: sanitize(svc.Name), Style: "text"}}
		if svc.State != "active" {
			segs = []grid.Seg{{Text: "✗ ", Style: "bad"}, {Text: svc.State + " ", Style: "bad"}, {Text: sanitize(svc.Name), Style: "text"}}
		}
		g.Segs(2+col*(colW+4), top+1+row, segs, colW)
	}
	return y
}

// refusals panel: refusal and failure counters, non-zero ones
// highlighted in the bad style; create/resume latencies on the right.
func (l *layout) refusalsH() int { return 4 } // title + counters + latencies + frame

func (l *layout) refusals(g *grid.Grid, y int) int {
	top := y
	y = l.panel(g, 0, y, l.w, l.refusalsH(), "refusals & failures", "refusals")
	pairs := []struct {
		label string
		n     int
	}{
		{"auth", l.s.AuthFails},
		{"throttled", l.s.Throttled},
		{"quota", l.s.Quota},
		{"capacity", l.s.Capacity},
		{"builds", l.s.BuildFails},
	}
	cx := 2
	for _, p := range pairs {
		style := "dim"
		if p.n != 0 {
			style = "bad"
		}
		segs := []grid.Seg{{Text: fmt.Sprintf("%s %d", p.label, p.n), Style: style}}
		g.Segs(cx, top+1, segs, l.w-2-cx)
		cx += segWidth(segs) + 4
	}
	g.Text(2, top+2, "create "+msLabel(l.s.CreateMs)+"   resume "+msLabel(l.s.ResumeMs), "dim", l.w-4)
	return y
}

// msLabel is a latency: "-" when none happened, "250ms" or "1.2s".
func msLabel(v float64) string {
	switch {
	case v < 0:
		return "-"
	case v < 1000:
		return fmt.Sprintf("%.0fms", v)
	default:
		return fmt.Sprintf("%.1fs", v/1000)
	}
}

// events panel: the backend's last activity — automatic held-lease
// actions (marker ┄) and the backend's last journal lines, both already
// collected. The panel is omitted entirely when the collector has
// nothing to show.
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
		style, text := e.Style, sanitize(e.Text)
		if e.Style == "warn" {
			// An automatic held-lease action (the legend's ┄): the
			// marker leads, so the panel's held-lease lines read as one
			// kind at a glance in the terminal too.
			style = "state"
			text = "┄ " + text
		}
		g.Text(2, top+1+i, text, style, l.w-4)
	}
	return y
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
		return grid.Seg{Text: gcLabel(mode), Style: "ok"}
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
