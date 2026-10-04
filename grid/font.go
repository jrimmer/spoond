package grid

import (
	_ "embed"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// fontCodepoints lists every codepoint the shipped JetBrains Mono maps
// (hex, one per line; # comments). A rune outside it would be drawn by a
// fallback font of another width and misalign every column after it.
//
//go:embed fontcodepoints.txt
var fontCodepoints string

var (
	fontOnce sync.Once
	fontSet  map[rune]bool
)

// FontHas reports whether the shipped font draws r. Callers sanitize
// text from users (names, labels) with it before drawing: anything the
// font lacks, including double-width scripts, becomes a placeholder.
func FontHas(r rune) bool {
	fontOnce.Do(func() {
		fontSet = make(map[rune]bool, 1400)
		for _, line := range strings.Split(fontCodepoints, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if v, err := strconv.ParseUint(line, 16, 32); err == nil {
				fontSet[rune(v)] = true
			}
		}
	})
	return fontSet[r]
}

// Drawable reports whether r fills exactly one cell in the shipped
// font: the font has it, it is graphic, and it is not a format
// character (zero-width spaces, bidi controls) or a combining mark,
// which would take no cell and shift every column after it.
func Drawable(r rune) bool {
	if r == utf8.RuneError || !unicode.IsGraphic(r) || unicode.In(r, unicode.Cf, unicode.Mn, unicode.Me) {
		return false
	}
	return FontHas(r)
}

// Sanitize replaces every rune that is not Drawable with '?', so
// user-supplied text can always be drawn, keeps its columns, and never
// carries terminal escapes.
func Sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || Drawable(r) {
			return r
		}
		return '?'
	}, s)
}
