package grid

import "fmt"

// Glyphs lists every non-ASCII rune this package itself can emit: box
// drawing (solid and dashed), the full/hollow block and shade cells of
// Bar, the eight rungs of Sparkline and the four quadrants of
// Spinner. It exists so callers can check a whole user interface
// against a font or terminal: Check rejects anything a grid holds that
// is not printable ASCII and not listed here (or in the caller's extra
// set).
//
// Note the half-filled circles ◐◓◑◒ are deliberately absent: the
// shipped JetBrains Mono does not map them.
var Glyphs = "─│┌┐└┘╌╎█░▁▂▃▄▅▆▇▖▗▘▝"

// glyphSet is Glyphs as a set, built once at init.
var glyphSet = func() map[rune]bool {
	m := make(map[rune]bool, len(Glyphs))
	for _, r := range Glyphs {
		m[r] = true
	}
	return m
}()

// Check reports an error describing the first rune it finds in the
// grid that is neither printable ASCII (0x20–0x7E) nor in Glyphs nor in
// extra, a caller-supplied set such as glyphs a theme adds. A nil
// error means every renderer can assume the grid is drawable.
func (g *Grid) Check(extra string) error {
	set := glyphSet
	if len(extra) > 0 {
		set = make(map[rune]bool, len(glyphSet)+8)
		for r := range glyphSet {
			set[r] = true
		}
		for _, r := range extra {
			set[r] = true
		}
	}
	for y := 0; y < g.h; y++ {
		for x := 0; x < g.w; x++ {
			r := g.cells[y*g.w+x].Rune
			if r == 0 {
				continue
			}
			if r >= 0x20 && r <= 0x7E || set[r] {
				continue
			}
			return fmt.Errorf("grid: rune %q (U+%04X) at (%d, %d) is not printable ASCII, not in Glyphs and not in the extra set", r, r, x, y)
		}
	}
	return nil
}
