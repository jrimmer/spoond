package grid

import "strings"

// Bar renders a filled fraction as a string of width total cells:
// filled cells of █, the rest ░. It clamps the fraction into
// 0..total, and a non-positive total gives an empty string. The
// caller gives it a style by writing it with Text or Segs.
func Bar(filled, total int) string {
	if total <= 0 {
		return ""
	}
	filled = clamp(filled, 0, total)
	var b strings.Builder
	b.Grow(total)
	for i := 0; i < filled; i++ {
		b.WriteRune('█')
	}
	for i := filled; i < total; i++ {
		b.WriteRune('░')
	}
	return b.String()
}

// Meter draws a one-row meter at (x, y): label, a space, a bar of
// width cells whose style follows pct (ok below warnPct, warn below
// dangerPct, bad at or above), a ╎ tick on the cell at the warning
// level, a space and right, right-aligned so its last cell lands at
// x+width-1. Anything that would fall outside the row is clipped.
//
// The bar gets the cells left over after label, right and the two
// separating spaces; right is usually the number itself ("42%"), and
// the tick marks warnPct so the eye can see how much headroom is left
// before the warn band.
func (g *Grid) Meter(x, y int, label string, pct, warnPct, dangerPct float64, width int, right string) {
	if width <= 0 {
		return
	}
	g.Segs(x, y, []Seg{{Text: label}}, width)

	bx := x + runeLen(label) + 1
	bw := width - runeLen(label) - runeLen(right) - 2
	if bw > 0 {
		filled := clamp(int(pct/100*float64(bw)), 0, bw)
		style := "ok"
		switch {
		case pct >= dangerPct:
			style = "bad"
		case pct >= warnPct:
			style = "warn"
		}
		for i := 0; i < bw; i++ {
			r := '░'
			if i < filled {
				r = '█'
			}
			g.Put(bx+i, y, r, style)
		}
		// Warning tick: ╎ at the warning level, over the bar.
		if warnPct > 0 && warnPct < 100 {
			tx := bx + int(warnPct/100*float64(bw))
			if tx < bx+bw {
				g.Put(tx, y, '╎', style)
			}
		}
	}
	if right != "" {
		g.Right(x+width-1, y, []Seg{{Text: right}})
	}
}

// Sparkline renders values as a string of block rungs ▁▂▃▄▅▆▇█, lo to
// hi scaled to the full range. Values below lo get ▁, above hi get █.
// A flat series (lo == hi) gives every value the middle rung ▅ instead
// of dividing by zero. Empty values gives an empty string.
func Sparkline(values []float64, lo, hi float64) string {
	if len(values) == 0 {
		return ""
	}
	rungs := []rune("▁▂▃▄▅▆▇█")
	var b strings.Builder
	b.Grow(len(values))
	for _, v := range values {
		if lo == hi {
			b.WriteRune(rungs[len(rungs)/2])
			continue
		}
		t := (v - lo) / (hi - lo)
		idx := int(t * float64(len(rungs)))
		idx = clamp(idx, 0, len(rungs)-1)
		b.WriteRune(rungs[idx])
	}
	return b.String()
}

// Spinner cycles through four quadrant glyphs ▖▘▝▗, a cheap animation
// cell for "working" states. Call it with an ever-increasing tick.
func Spinner(tick int) rune {
	spinners := []rune("▖▘▝▗")
	return spinners[((tick%len(spinners))+len(spinners))%len(spinners)]
}

func runeLen(s string) int { return len([]rune(s)) }
