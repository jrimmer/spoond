// Package grid draws on a character grid: a fixed W×H array of cells,
// each holding a rune, a style name and an element id.
//
// Callers draw with Put, Text and Segs, frame things with Box, and add
// ready-made widgets (Meter, Bar, Sparkline, Spinner), then render the
// whole grid at once as plain text, ANSI-styled text or HTML spans:
//
//	g := grid.New(40, 3)
//	g.Box(0, 0, 40, 3, "frame", false)
//	g.Title(3, 0, []grid.Seg{{Text: " disk ", Style: "title"}})
//	g.Meter(2, 1, "used", 72, 60, 85, 26, "72%")
//	fmt.Print(g.Plain())
//
// Styles are names, not colours: Plain ignores them, ANSI maps them to
// SGR sequences and HTML maps them to g-<name> CSS classes, so one grid
// feeds a terminal and a browser alike. Out-of-bounds writes are
// ignored. Every non-ASCII rune the package itself emits is listed in
// Glyphs, so a finished grid can be checked against a font or terminal
// with Check.
//
// Grid.Box, Grid.Text and the other drawing calls take coordinates and
// widths in cells; x grows right, y grows down, and the origin is the
// top-left cell.
package grid

// Seg is one run of text with a single style name.
type Seg struct {
	Text  string
	Style string
}

// Cell is one position on the grid. Rune 0 means blank.
type Cell struct {
	Rune  rune
	Style string
	ID    string
}

// Grid is a fixed-size character grid.
type Grid struct {
	w, h  int
	cells []Cell
}

// New makes a W×H grid of blank cells.
func New(w, h int) *Grid {
	if w < 0 {
		w = 0
	}
	if h < 0 {
		h = 0
	}
	return &Grid{w: w, h: h, cells: make([]Cell, w*h)}
}

// Cols returns the grid width in cells.
func (g *Grid) Cols() int { return g.w }

// Rows returns the grid height in cells.
func (g *Grid) Rows() int { return g.h }

// At returns the cell at (x, y), or the zero Cell when out of bounds.
func (g *Grid) At(x, y int) Cell {
	if x < 0 || y < 0 || x >= g.w || y >= g.h {
		return Cell{}
	}
	return g.cells[y*g.w+x]
}

// set writes rune and style, keeping any element id: Mark defines
// regions, and drawing inside a marked region does not unmark it.
func (g *Grid) set(x, y int, r rune, style string) {
	if x < 0 || y < 0 || x >= g.w || y >= g.h {
		return
	}
	c := &g.cells[y*g.w+x]
	c.Rune = r
	c.Style = style
}

// Put writes one rune with a style name at (x, y). Out-of-bounds
// writes are ignored.
func (g *Grid) Put(x, y int, r rune, style string) {
	g.set(x, y, r, style)
}

// Text writes s at (x, y) with one style name, clipped to max cells
// (all of s when max is negative), then clipped again at the grid
// edge. It returns the x just past the last cell written, whether or
// not that cell is on the grid, so calls can be chained.
func (g *Grid) Text(x, y int, s, style string, max int) int {
	rs := []rune(s)
	if max >= 0 && len(rs) > max {
		rs = rs[:max]
	}
	for i, r := range rs {
		g.Put(x+i, y, r, style)
	}
	return x + len(rs)
}

// Segs writes a sequence of styled runs starting at (x, y), all
// together clipped to max cells (unlimited when max is negative). It
// returns the next x, like Text.
func (g *Grid) Segs(x, y int, segs []Seg, max int) int {
	for _, s := range segs {
		if max == 0 {
			break
		}
		n := len([]rune(s.Text))
		if max >= 0 && n > max {
			n = max
		}
		g.Text(x, y, s.Text, s.Style, n)
		x += n
		if max > 0 {
			max -= n
		}
	}
	return x
}

// Center writes segs on row y centred on column cx.
func (g *Grid) Center(cx, y int, segs []Seg) {
	g.Segs(cx-segWidth(segs)/2, y, segs, g.w)
}

// Right writes segs on row y right-aligned, so the last rune lands on
// xRight.
func (g *Grid) Right(xRight, y int, segs []Seg) {
	g.Segs(xRight-segWidth(segs)+1, y, segs, g.w)
}

// HLine draws a horizontal ─ line w cells wide from (x, y).
func (g *Grid) HLine(x, y, w int, style string) {
	for i := 0; i < w; i++ {
		g.Put(x+i, y, '─', style)
	}
}

// VLine draws a vertical │ line h cells tall from (x, y).
func (g *Grid) VLine(x, y, h int, style string) {
	for i := 0; i < h; i++ {
		g.Put(x, y+i, '│', style)
	}
}

// Box draws a w×h rectangle with corners ┌┐└┘ and edges ─ and │, or ╌
// and ╎ when dashed. Boxes smaller than 2×2 draw nothing: there is no
// room for corners and edges. Out-of-bounds parts are clipped.
func (g *Grid) Box(x, y, w, h int, style string, dashed bool) {
	if w < 2 || h < 2 {
		return
	}
	hz, vt := '─', '│'
	if dashed {
		hz, vt = '╌', '╎'
	}
	for i := 0; i < w; i++ {
		g.Put(x+i, y, hz, style)
		g.Put(x+i, y+h-1, hz, style)
	}
	for i := 1; i < h-1; i++ {
		g.Put(x, y+i, vt, style)
		g.Put(x+w-1, y+i, vt, style)
	}
	g.Put(x, y, '┌', style)
	g.Put(x+w-1, y, '┐', style)
	g.Put(x, y+h-1, '└', style)
	g.Put(x+w-1, y+h-1, '┘', style)
}

// Title writes segs into a box frame row: the cells the title occupies
// are blanked first, so the frame line is interrupted around the text.
// The caller picks the position, usually a couple of cells right of
// the frame's top-left corner.
func (g *Grid) Title(x, y int, segs []Seg) {
	for i := 0; i < segWidth(segs); i++ {
		g.Put(x+i, y, ' ', "")
	}
	g.Segs(x, y, segs, g.w)
}

// Mark sets the element id on every cell of the rectangle at (x, y)
// sized w×h, leaving runes and styles alone. Renderers attach ids to
// the HTML output (data-id) so clients can patch regions; Plain and
// ANSI ignore them.
func (g *Grid) Mark(x, y, w, h int, id string) {
	for r := y; r < y+h; r++ {
		for c := x; c < x+w; c++ {
			if c < 0 || r < 0 || c >= g.w || r >= g.h {
				continue
			}
			g.cells[r*g.w+c].ID = id
		}
	}
}

// PaintRow sets the style of every cell on row y, keeping runes and
// element ids. Out-of-bounds rows are ignored.
func (g *Grid) PaintRow(y int, style string) {
	if y < 0 || y >= g.h {
		return
	}
	for c := 0; c < g.w; c++ {
		g.cells[y*g.w+c].Style = style
	}
}

func segWidth(segs []Seg) int {
	n := 0
	for _, s := range segs {
		n += len([]rune(s.Text))
	}
	return n
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
