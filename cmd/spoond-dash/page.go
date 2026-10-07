// The browser half of the grid dashboard: the page renders the same
// grid as spoond top as HTML inside one <pre>, with WebTUI for the page
// palette and the grid styles as classes (the grid draws its own header). Datastar
// patches changed rows over the existing SSE stream: every grid row is
// one element (id rN), and a frame sends one datastar-patch-elements per
// row that changed — the whole <pre> only when the row count changed.
//
// Holder links become real <a> elements (target=_blank, rel=noopener):
// the grid carries the holder text in the link style, and the row
// renderer swaps that span for an anchor when the lease has a
// holder_url.
package spoonddash

import (
	"fmt"
	"html"
	"strings"

	"github.com/jrimmer/spoond/v2/grid"
)

// gridLine is one grid row as HTML: its row number (the element id is
// r<N>), the panel/notice id its cells carry (""/"notifications"/
// "notice:<id>"), and its rendered spans.
type gridLine struct {
	N    int
	ID   string
	HTML string
}

// linkAt records a holder link on a grid row: the rendered holder text
// and where it points.
type linkAt struct {
	row  int
	text string
	url  string
}

// pageLines renders the wrapped rows for a grid: grid.HTML split into
// rows, each wrapped in a <span class="gr" id="rN"> so Datastar can
// patch it, with holder spans swapped for anchors per links.
func pageLines(g *grid.Grid, links []linkAt) []gridLine {
	rows := strings.Split(g.HTML(), "\n")
	out := make([]gridLine, len(rows))
	for i, r := range rows {
		out[i] = gridLine{N: i, ID: rowID(g, i), HTML: applyLinks(r, i, links)}
	}
	return out
}

// rowID returns the first non-empty grid cell id on row y: the panel or
// notice id the renderer marked the row with. "" for a drawn row with no
// id (the header, the status line).
func rowID(g *grid.Grid, y int) string {
	for x := 0; x < g.Cols(); x++ {
		if id := g.At(x, y).ID; id != "" {
			return id
		}
	}
	return ""
}

// applyLinks swaps the row's link-styled span for a real anchor when the
// row carries a holder link. The span is found by class, not by
// reconstructing it, so clipping cannot lose the link.
func applyLinks(row string, y int, links []linkAt) string {
	var la *linkAt
	for i := range links {
		if links[i].row == y {
			la = &links[i]
			break
		}
	}
	if la == nil {
		return row
	}
	open := strings.Index(row, `<span class="g-link`)
	if open < 0 {
		return row
	}
	close := strings.Index(row[open:], `</span>`)
	if close < 0 {
		return row
	}
	end := open + close // where the span's </span> starts
	close = end + len(`</span>`)
	// The anchor takes the span's text only — from after its opening
	// tag's > to its </span>. Taking the rest of the row duplicated the
	// row's tail (padding and border) and wrapped the row.
	start := open + strings.Index(row[open:], `>`) + 1
	if start <= open || start > end {
		return row
	}
	anchor := `<a class="g-link" href="` + html.EscapeString(la.url) + `" target="_blank" rel="noopener">` + row[start:end] + `</a>`
	return row[:open] + anchor + row[close:]
}

// pageGrid renders the whole grid as the page's row elements (the page
// template puts them inside its <pre>). The rows are blocks, so nothing
// goes between them: a newline text node there drew a blank line under
// every row and broke the box borders.
//
// A row the renderer marked with a notice or panel id also carries it as
// data-notice-id / data-notifications on the wrapper, so the dismissal
// script (static/js/notifications.js) can hide a message row or the whole
// panel across a Datastar patch. A row-level attribute avoids colliding
// with the cell runs' own data-id.
func pageGrid(g *grid.Grid, links []linkAt) string {
	lines := pageLines(g, links)
	parts := make([]string, len(lines))
	for i, l := range lines {
		attr := ""
		switch {
		case strings.HasPrefix(l.ID, "notice:"):
			attr = ` data-notice-id="` + html.EscapeString(strings.TrimPrefix(l.ID, "notice:")) + `"`
		case l.ID == "notifications":
			attr = ` data-notifications`
		}
		parts[i] = fmt.Sprintf(`<span class="gr" id="r%d"%s>%s</span>`, l.N, attr, l.HTML)
	}
	return strings.Join(parts, "")
}
