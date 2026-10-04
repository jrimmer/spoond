package grid

import (
	"html"
	"strings"
)

// Plain renders the grid as plain text: one line per row, joined by
// newlines, no trailing-space trimming, no escape sequences, styles
// and element ids dropped. Blank cells come out as spaces.
func (g *Grid) Plain() string {
	var b strings.Builder
	b.Grow(g.h * (g.w + 1))
	for y := 0; y < g.h; y++ {
		if y > 0 {
			b.WriteByte('\n')
		}
		for x := 0; x < g.w; x++ {
			r := g.cells[y*g.w+x].Rune
			if r == 0 {
				r = ' '
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ANSI renders the grid as text wrapped in SGR sequences: each
// maximal run of one style is wrapped in styles[style] and followed by
// a reset ("\x1b[0m"). Styles with no entry in the map — including the
// empty style — emit no escapes at all. Element ids are dropped.
func (g *Grid) ANSI(styles map[string]string) string {
	const reset = "\x1b[0m"
	var b strings.Builder
	run := ""
	cur := ""
	flush := func() {
		if run == "" {
			return
		}
		if seq, ok := styles[cur]; ok && seq != "" {
			b.WriteString(seq)
			b.WriteString(run)
			b.WriteString(reset)
		} else {
			b.WriteString(run)
		}
		run = ""
	}
	for y := 0; y < g.h; y++ {
		if y > 0 {
			flush()
			cur = ""
			b.WriteByte('\n')
		}
		for x := 0; x < g.w; x++ {
			c := g.cells[y*g.w+x]
			if c.Style != cur {
				flush()
				cur = c.Style
			}
			r := c.Rune
			if r == 0 {
				r = ' '
			}
			run += string(r)
		}
	}
	flush()
	return b.String()
}

// HTML renders the grid for a browser: each maximal run of one
// (style, id) pair becomes
//
//	<span class="g-<style>" data-id="<id>">…</span>
//
// with the text HTML-escaped. Blank cells render as spaces inside
// their run. Rows are joined by newlines; the caller wraps the result
// in <pre>. Styles with an empty name omit the class attribute; empty
// ids omit data-id. Style and id strings are attribute-escaped.
func (g *Grid) HTML() string {
	var b strings.Builder
	run := ""
	curStyle, curID := "\x00style", "\x00id"
	flush := func() {
		if run == "" {
			return
		}
		b.WriteString(`<span`)
		if curStyle != "" {
			b.WriteString(` class="g-`)
			b.WriteString(html.EscapeString(curStyle))
			b.WriteString(`"`)
		}
		if curID != "" {
			b.WriteString(` data-id="`)
			b.WriteString(html.EscapeString(curID))
			b.WriteString(`"`)
		}
		b.WriteString(`>`)
		b.WriteString(html.EscapeString(run))
		b.WriteString(`</span>`)
		run = ""
	}
	for y := 0; y < g.h; y++ {
		if y > 0 {
			flush()
			curStyle, curID = "\x00style", "\x00id"
			b.WriteByte('\n')
		}
		for x := 0; x < g.w; x++ {
			c := g.cells[y*g.w+x]
			if c.Style != curStyle || c.ID != curID {
				flush()
				curStyle, curID = c.Style, c.ID
			}
			r := c.Rune
			if r == 0 {
				r = ' '
			}
			run += string(r)
		}
	}
	flush()
	return b.String()
}
