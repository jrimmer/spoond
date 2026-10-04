// The dashboard on the character grid (#110): one fixed-width grid holds
// the whole frame — header, legend, one attention banner when something
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
	"sort"
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
// ✓ an active unit and ✗ one that is not, · separator, ═ and ║ and the
// double corners the panel frames use, ┄ a held-lease action in the
// events panel, ■ the attention marker and the lost state, ∞ a
// persistent lease's remaining time, ◉ a lapsed hold, and the leases
// panel's per-state glyphs (▶ running, ‖ suspended, ◆ held). It is
// passed to grid.Check by every renderer, and every rune is asserted to
// be in the shipped JetBrains Mono (TestExtraGlyphsInFont).
const Extra = "✓✗·═║╔╗╚╝┄■∞◉" + stateGlyphs

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

// bannerRows returns the attention banner's rows — one per trigger, in
// warn style — or nil when nothing needs a person (the banner is then
// never drawn; the frame just starts with the panels). Triggers:
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
			rows = append(rows, fmt.Sprintf("■ unit %s is %s", svc.Name, svc.State))
		}
	}
	if s.ByState["lost"] > 0 {
		rows = append(rows, fmt.Sprintf("■ %d lost lease(s) - a substrate crash dropped them", s.ByState["lost"]))
	}
	if s.HugeFreeGiB > 0 && s.HugeUsedPct >= 92 {
		rows = append(rows, fmt.Sprintf("■ hugepages only %.1f GiB free - past the danger level", s.HugeFreeGiB))
	}
	if s.DiskUsedPct >= 90 {
		rows = append(rows, fmt.Sprintf("■ snapshot disk %.0f%% used - past the danger level", s.DiskUsedPct))
	}
	if !s.CertNotAfter.IsZero() {
		if d := s.CertNotAfter.Sub(now); d < 30*24*time.Hour {
			rows = certBanner(rows, d, s.CertNotAfter)
		}
	}
	for _, r := range s.Rows {
		if !r.LastActionAt.IsZero() && now.Sub(r.LastActionAt) < 24*time.Hour {
			rows = append(rows, fmt.Sprintf("■ held lease %s: %s %s ago", r.ID, r.LastAction, dur(now.Sub(r.LastActionAt))))
		}
	}
	return rows
}

// certBanner appends the TLS certificate's row: at 30 days it needs a
// person before basic auth starts failing; inside 7 days it is urgent.
func certBanner(rows []string, d time.Duration, notAfter time.Time) []string {
	when := notAfter.Format("2006-01-02")
	if d < 0 {
		return append(rows, "■ the TLS certificate (TLS_CERT) expired "+when)
	}
	if d < 7*24*time.Hour {
		return append(rows, fmt.Sprintf("■ the TLS certificate expires in %s (%s) - renew it", dur(d), when))
	}
	return append(rows, fmt.Sprintf("■ the TLS certificate expires in %s (%s)", dur(d), when))
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
// frame draws: one warn-styled row per trigger. Shared by Draw, the
// page/top renderers and the tests, so the banner is styled one way.
func bannerSegs(rows []string) [][]grid.Seg {
	if len(rows) == 0 {
		return nil
	}
	out := make([][]grid.Seg, len(rows))
	for i, r := range rows {
		out[i] = []grid.Seg{{Text: r, Style: "warn"}}
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
func (l *layout) assemble() *grid.Grid {
	h := 2 + // header + legend
		len(l.banner) + boolInt(len(l.banner) > 0) + // banner rows + a blank row under them
		l.capacityH() + l.hostH() + l.throughputH() + l.leasesH() +
		l.imagesH() + l.servicesH() + l.refusalsH() + l.eventsH()

	g := grid.New(l.w, h)
	l.header(g, 0)
	y := 2

	// One attention banner row per trigger, then a blank row — only
	// when something needs a person.
	for _, segs := range l.banner {
		g.Segs(0, y, segs, l.w)
		g.Mark(0, y, l.w, 1, "banner")
		y++
	}
	if len(l.banner) > 0 {
		y++
	}

	y = l.capacity(g, y)
	y = l.hostPanel(g, y)
	y = l.throughput(g, y)
	y = l.leases(g, y)
	y = l.images(g, y)
	y = l.servicesPanel(g, y)
	y = l.refusals(g, y)
	l.events(g, y)
	return g
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// header: SPOOND, host, spoond version, orchestrator version, uptime,
// with the frame time on the right; the legend row under it.
func (l *layout) header(g *grid.Grid, y int) {
	segs := []grid.Seg{
		{Text: "SPOOND", Style: "head"},
		{Text: " " + l.host, Style: "text"},
		{Text: "  spoond " + dashVersion, Style: "text"},
		{Text: "  orch " + versionLabel(l.s.Version), Style: "text"},
		{Text: "  up " + fmt.Sprintf("%.0fh", l.s.UptimeH), Style: "text"},
	}
	g.Segs(0, y, segs, l.w)
	g.Right(l.w-1, y, []grid.Seg{{Text: l.s.At, Style: "dim"}})
	g.Segs(0, y+1, legendRow(), l.w)
}

// versionLabel is an orchestrator version or "?" when the scrape had none.
func versionLabel(v string) string {
	if v == "" {
		return "?"
	}
	return v
}

// panel draws a double-line framed panel of h rows with its title on
// the frame, at row y, and returns the row past its bottom edge. id is
// the element id Datastar patches by.
func (l *layout) panel(g *grid.Grid, y, h int, title, id string) int {
	drawPanelFrame(g, y, l.w, h)
	if title != "" {
		g.Title(3, y, []grid.Seg{{Text: " " + title + " ", Style: "title"}})
	}
	g.Mark(1, y+1, l.w-2, h-2, id)
	return y + h
}

// drawPanelFrame draws a ═-framed rectangle w×h at (0, y) in the
// "frame" style: double-line edges, so panels read as distinct from the
// plain header. Drawn with Put (the package's Box draws single lines
// only).
func drawPanelFrame(g *grid.Grid, y, w, h int) {
	for i := 0; i < w; i++ {
		g.Put(i, y, '═', "frame")
		g.Put(i, y+h-1, '═', "frame")
	}
	for i := 1; i < h-1; i++ {
		g.Put(0, y+i, '║', "frame")
		g.Put(w-1, y+i, '║', "frame")
	}
	g.Put(0, y, '╔', "frame")
	g.Put(w-1, y, '╗', "frame")
	g.Put(0, y+h-1, '╚', "frame")
	g.Put(w-1, y+h-1, '╝', "frame")
}

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
// header (2) + banner rows (+ a blank under them) + the panels above
// leases, then the panel's title row, then one row per lease.
func holderLinks(s Snapshot, w int, now time.Time) []linkAt {
	l := &layout{w: clamp(w, minW, maxW), s: s, now: now,
		banner: bannerSegs(bannerRows(s, now))}
	base := headerRows() + len(l.banner) + boolInt(len(l.banner) > 0) +
		l.capacityH() + l.hostH() + l.throughputH()
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

// headerRows is the header's row count (title + legend).
func headerRows() int { return 2 }

// capacity panel: running / limit meter, leases by state, queued,
// granted, swept, running by image.
func (l *layout) capacityH() int {
	half := (l.w-4)/2 - 2
	rows := max(kvRowCount(half, l.capLeft()), kvRowCount(half, l.capRight()))
	h := 4 + rows // title + meter + states + counts (+ frame)
	if len(l.s.ByImage) > 0 {
		h++ // the running-by-image line
	}
	return h
}

func (l *layout) capLeft() [][2]string {
	return [][2]string{
		{"queued", fmt.Sprint(l.s.Queued)},
		{"granted", fmt.Sprint(l.s.Granted)},
		{"swept", fmt.Sprint(l.s.Swept)},
	}
}

// capRight is the right column of counts. The GC mode is not here: the
// task puts it in the host panel, where it already sits.
func (l *layout) capRight() [][2]string {
	return [][2]string{
		{"shares", fmt.Sprint(l.s.Shares)},
		{"users", fmt.Sprint(l.s.Users)},
		{"builds", fmt.Sprint(l.s.BuildsBusy)},
	}
}

func (l *layout) capacity(g *grid.Grid, y int) int {
	states := stateOrder(l.s.ByState)
	top := y
	y = l.panel(g, y, l.capacityH(), "capacity", "capacity")

	pct, right := 0.0, fmt.Sprintf("%d of %d", l.s.Running, l.s.Limit)
	if l.s.Limit > 0 {
		pct = float64(l.s.Running) / float64(l.s.Limit) * 100
	} else {
		right = fmt.Sprintf("%d", l.s.Running)
	}
	g.Meter(2, top+1, "running", pct, 75, 90, l.w-4, right)

	// states line: every state, count and glyph style for lost.
	line := g.Text(2, top+2, "", "", 0)
	for _, st := range states {
		style := "text"
		if st.name == "lost" && st.n > 0 {
			style = "bad"
		}
		line = g.Segs(line, top+2, []grid.Seg{
			{Text: fmt.Sprintf("%d", st.n), Style: style},
			{Text: " " + st.name, Style: "dim"},
			{Text: "  ", Style: ""},
		}, l.w-4)
	}
	// two columns of counts: queued/granted/swept and shares/users/builds.
	used := l.kv(g, 2, top+3, (l.w-4)/2-2, l.capLeft())
	used = max(used, l.kv(g, 2+(l.w-4)/2, top+3, (l.w-4)/2-2, l.capRight()))

	// running by image: which images the running leases serve (sorted by
	// name, so the line is stable frame to frame).
	if len(l.s.ByImage) > 0 {
		names := make([]string, 0, len(l.s.ByImage))
		for name := range l.s.ByImage {
			names = append(names, name)
		}
		sort.Strings(names)
		ly := top + 3 + used
		cx := g.Text(2, ly, "running by image ", "dim", -1)
		for i, name := range names {
			segs := []grid.Seg{
				{Text: sanitize(name), Style: "text"},
				{Text: fmt.Sprintf(" %d", l.s.ByImage[name]), Style: "text"},
			}
			if i < len(names)-1 {
				segs = append(segs, grid.Seg{Text: " · ", Style: "dim"})
			}
			cx = g.Segs(cx, ly, segs, l.w-2-cx)
		}
	}
	return y
}

// kv draws "label value" pairs left to right, wrapping to the next row
// when a pair would not fit; it returns the number of rows it used.
func (l *layout) kv(g *grid.Grid, x, y, w int, pairs [][2]string) int {
	cx, row := x, 0
	for _, p := range pairs {
		segs := []grid.Seg{
			{Text: p[0] + " ", Style: "dim"},
			{Text: p[1], Style: "text"},
		}
		n := segWidth(segs)
		if cx > x && cx+n > x+w {
			cx = x
			row++
		}
		g.Segs(cx, y+row, segs, x+w-cx)
		cx += n + 3
	}
	return row + 1
}

// kvRowCount is the number of rows kv needs for pairs in w cells.
func kvRowCount(w int, pairs [][2]string) int {
	cx, row := 0, 0
	for _, p := range pairs {
		n := len(p[0]) + 1 + len(p[1])
		if cx > 0 && cx+n > w {
			cx = 0
			row++
		}
		cx += n + 3
	}
	return row + 1
}

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

func (l *layout) hostH() int {
	kv := kvRowCount(l.w-4, [][2]string{
		{"vcpu", fmt.Sprint(l.s.VCPUAlloc)},
		{"mem allocated", fmt.Sprintf("%.1f GiB", l.s.MemAllocGiB)},
		{"gc", gcLabel(l.s.GCMode)},
	})
	return 3 + len(hostPanelRows(l.s)) + kv - 1
}

func (l *layout) hostPanel(g *grid.Grid, y int) int {
	rows := hostPanelRows(l.s)
	h := l.hostH() // title row + meters + allocated row + frame
	top := y
	y = l.panel(g, y, h, "host", "host")
	for i, r := range rows {
		g.Meter(2, top+1+i, r.label, r.pct, r.warn, r.danger, l.w-4, r.right)
	}
	l.kv(g, 2, top+1+len(rows), l.w-4, [][2]string{
		{"vcpu", fmt.Sprint(l.s.VCPUAlloc)},
		{"mem allocated", fmt.Sprintf("%.1f GiB", l.s.MemAllocGiB)},
		{"gc", gcLabel(l.s.GCMode)},
	})
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
	y = l.panel(g, y, h, "throughput (5 min)", "throughput")
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

// leases panel: id, image, owner, state with a glyph, policy, age, left
// and the holder (the holder text; a lapsed hold shows ◉ after it).
func (l *layout) leases(g *grid.Grid, y int) int {
	top := y
	y = l.panel(g, y, l.leasesH(), "leases", "leases")
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
			hold := sanitize(r.Holder)
			seg := []grid.Seg{{Text: hold, Style: "link"}}
			if r.HoldState == "lapsed" {
				seg = append(seg, grid.Seg{Text: " ◉lapsed", Style: "warn"})
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
	y = l.panel(g, y, l.imagesH(), "images", "images")
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
	y = l.panel(g, y, l.servicesH(), "units", "services")
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
	y = l.panel(g, y, l.refusalsH(), "refusals & failures", "refusals")
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
	y = l.panel(g, y, l.eventsH(), "events", "events")
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
