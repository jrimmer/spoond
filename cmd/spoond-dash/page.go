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
		out[i] = gridLine{N: i, ID: rowID(g, i), HTML: applyProjectLink(applyLinks(r, i, links))}
	}
	return out
}

// githubMarkSVG is the standard GitHub mark (octocat) as a small inline
// SVG: 16px, currentColor, so it follows the dim footer colour and the
// light/dark theme. It replaces the footer's plain "GitHub" text on the
// page; the terminal keeps the text.
const githubMarkSVG = `<svg class="gh-mark" width="16" height="16" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true"><path fill-rule="evenodd" d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.012 8.012 0 0 0 16 8c0-4.42-3.58-8-8-8z"/></svg>`

// applyProjectLink swaps the footer's GitHub-mark span (the ghmark
// style) for a real anchor carrying the mark's inline SVG. The page
// already draws holder links as anchors, so the project link is one too;
// the target takes the scheme the grid's plain text leaves off. A row
// without the span (every other row) is returned unchanged.
func applyProjectLink(row string) string {
	open := strings.Index(row, `<span class="g-ghmark"`)
	if open < 0 {
		return row
	}
	close := strings.Index(row[open:], `</span>`)
	if close < 0 {
		return row
	}
	end := open + close
	anchor := `<a class="g-ghmark" href="` + html.EscapeString(projectHref()) +
		`" title="spoond on GitHub" aria-label="spoond on GitHub" rel="noopener" target="_blank">` +
		githubMarkSVG + `</a>`
	return row[:open] + anchor + row[end+len(`</span>`):]
}

// rowID returns the first non-empty grid cell id on row y: the panel or
// notice id the renderer marked the row with. "" for a drawn row with no
// id (the header, the footer).
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
