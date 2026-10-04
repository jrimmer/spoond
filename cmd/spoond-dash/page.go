// The browser half of the grid dashboard: the page renders the same
// grid as spoond top as HTML inside one <pre>, with WebTUI for the page
// chrome (title, login state) and the grid styles as classes. Datastar
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
// r<N>) and its rendered spans.
type gridLine struct {
	N    int
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
		out[i] = gridLine{N: i, HTML: applyLinks(r, i, links)}
	}
	return out
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
	close += open + len(`</span>`)
	inner := row[open+len(`<span class="g-link`):]
	if i := strings.Index(inner, `>`); i >= 0 {
		inner = inner[i+1:]
	}
	anchor := `<a class="g-link" href="` + html.EscapeString(la.url) + `" target="_blank" rel="noopener">` + inner + `</a>`
	return row[:open] + anchor + row[close:]
}

// pageGrid renders the whole grid as the page's row elements, joined by
// newlines (the page template puts them inside its <pre>).
func pageGrid(g *grid.Grid, links []linkAt) string {
	lines := pageLines(g, links)
	parts := make([]string, len(lines))
	for i, l := range lines {
		parts[i] = fmt.Sprintf(`<span class="gr" id="r%d">%s</span>`, l.N, l.HTML)
	}
	return strings.Join(parts, "\n")
}
