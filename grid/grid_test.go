package grid

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestNewSizes(t *testing.T) {
	g := New(4, 3)
	if g.Cols() != 4 || g.Rows() != 3 {
		t.Fatalf("New(4,3) = %dx%d", g.Cols(), g.Rows())
	}
	if len(g.Plain()) != 3*5-1 { // 3 rows of 4 + two newlines
		t.Fatalf("Plain of a fresh grid: %q", g.Plain())
	}
	if s := New(2, 2).Plain(); s != "  \n  " {
		t.Fatalf("fresh grid is not blank: %q", s)
	}
}

func TestPutAndGet(t *testing.T) {
	g := New(4, 3)
	g.Put(1, 1, 'x', "hot")
	c := g.At(1, 1)
	if c.Rune != 'x' || c.Style != "hot" || c.ID != "" {
		t.Fatalf("At(1,1) = %+v", c)
	}
}

func TestPutOutOfBoundsIgnored(t *testing.T) {
	g := New(4, 3)
	g.Put(-1, 0, 'x', "")
	g.Put(0, -1, 'x', "")
	g.Put(4, 0, 'x', "")
	g.Put(0, 3, 'x', "")
	g.Put(-5, -5, 'x', "")
	g.Put(99, 99, 'x', "")
	if s := g.Plain(); s != "    \n    \n    " {
		t.Fatalf("out-of-bounds writes leaked: %q", s)
	}
}

func TestText(t *testing.T) {
	g := New(20, 3)
	if x := g.Text(2, 1, "hello", "s", -1); x != 7 {
		t.Fatalf("Text next x = %d, want 7", x)
	}
	if !strings.Contains(g.Plain(), "  hello") {
		t.Fatalf("Text did not draw: %q", g.Plain())
	}
	if g.At(7, 1).Rune != 0 {
		t.Fatal("Text drew past its string")
	}
}

func TestTextClipsToMax(t *testing.T) {
	g := New(20, 3)
	if x := g.Text(0, 0, "abcdef", "s", 3); x != 3 {
		t.Fatalf("clipped Text next x = %d, want 3", x)
	}
	if !strings.Contains(g.Plain(), "abc ") {
		t.Fatalf("clip wrote past max: %q", g.Plain())
	}
}

func TestTextClipsAtEdge(t *testing.T) {
	g := New(4, 1)
	g.Text(2, 0, "abcdef", "s", -1)
	if g.Plain() != "  ab" {
		t.Fatalf("edge clip wrong: %q", g.Plain())
	}
}

func TestTextNegativeMaxMeansUnlimited(t *testing.T) {
	g := New(40, 1)
	g.Text(0, 0, strings.Repeat("ab", 20), "s", -1)
	if !strings.Contains(g.Plain(), strings.Repeat("ab", 20)) {
		t.Fatal("negative max did not draw everything")
	}
}

func TestSegs(t *testing.T) {
	g := New(20, 1)
	segs := []Seg{{Text: "a", Style: "one"}, {Text: "bcd", Style: "two"}}
	if x := g.Segs(1, 0, segs, -1); x != 5 {
		t.Fatalf("Segs next x = %d, want 5", x)
	}
	if g.At(1, 0).Style != "one" || g.At(2, 0).Style != "two" {
		t.Fatalf("Segs styles wrong: %+v %+v", g.At(1, 0), g.At(2, 0))
	}
}

func TestSegsClipsAcrossStyles(t *testing.T) {
	g := New(20, 1)
	segs := []Seg{{Text: "aaaa", Style: "one"}, {Text: "bbbb", Style: "two"}}
	g.Segs(0, 0, segs, 5)
	if !strings.Contains(g.Plain(), "aaaab") || strings.Contains(g.Plain(), "aaaabb") {
		t.Fatalf("Segs clip wrong: %q", g.Plain())
	}
}

func TestSegsEmpty(t *testing.T) {
	g := New(5, 1)
	if x := g.Segs(2, 0, nil, -1); x != 2 {
		t.Fatalf("Segs(nil) next x = %d", x)
	}
}

func TestCenterAndRight(t *testing.T) {
	g := New(21, 1)
	g.Center(10, 0, []Seg{{Text: "mid", Style: ""}})
	g.Right(20, 0, []Seg{{Text: "end", Style: ""}})
	g.Center(4, 0, []Seg{{Text: "tiny", Style: ""}}) // overwrites, must not crash
	line := strings.Split(g.Plain(), "\n")[0]
	if !strings.Contains(line, "mid") || !strings.HasSuffix(line, "end") {
		t.Fatalf("Center/Right placement wrong: %q", line)
	}
	if strings.Count(line, "end") != 1 || strings.Count(line, "tiny") != 1 {
		t.Fatalf("Center/Right wrote the wrong cells: %q", line)
	}
}

func TestCenterOddWidth(t *testing.T) {
	g := New(11, 1)
	g.Center(5, 0, []Seg{{Text: "abc", Style: ""}})
	if !strings.Contains(g.Plain(), "   abc") {
		t.Fatalf("Center of odd width off: %q", g.Plain())
	}
}

func TestHLineVLine(t *testing.T) {
	g := New(5, 3)
	g.HLine(0, 1, 5, "rule")
	g.VLine(2, 0, 3, "rule")
	want := "  │  \n──│──\n  │  "
	if g.Plain() != want {
		t.Fatalf("lines:\n%s\nwant:\n%s", g.Plain(), want)
	}
}

func TestBox(t *testing.T) {
	g := New(6, 4)
	g.Box(1, 1, 4, 3, "frame", false)
	want := "      \n ┌──┐ \n │  │ \n └──┘ "
	if g.Plain() != want {
		t.Fatalf("box:\n%s\nwant:\n%s", g.Plain(), want)
	}
	if g.At(1, 1).Style != "frame" {
		t.Fatal("box lost its style")
	}
}

func TestBoxDashed(t *testing.T) {
	g := New(6, 4)
	g.Box(1, 1, 4, 3, "frame", true)
	want := "      \n ┌╌╌┐ \n ╎  ╎ \n └╌╌┘ "
	if g.Plain() != want {
		t.Fatalf("dashed box:\n%s\nwant:\n%s", g.Plain(), want)
	}
	g = New(5, 4)
	g.Box(0, 0, 5, 4, "frame", true)
	if g.At(0, 1).Rune != '╎' || g.At(4, 2).Rune != '╎' {
		t.Fatalf("dashed box sides: %q", g.Plain())
	}
}

func TestBoxTooSmall(t *testing.T) {
	g := New(5, 5)
	g.Box(0, 0, 1, 3, "frame", false)
	g.Box(0, 0, 3, 1, "frame", false)
	g.Box(0, 0, 0, 0, "frame", false)
	if strings.ContainsAny(g.Plain(), "┌┐└┘─│╌╎") {
		t.Fatalf("degenerate box drew something:\n%s", g.Plain())
	}
}

func TestBoxClipsAtEdge(t *testing.T) {
	g := New(4, 3)
	g.Box(2, 1, 6, 4, "frame", false) // right and bottom off-grid
	s := g.Plain()
	if !strings.Contains(s, "  ┌─") {
		t.Fatalf("clipped box top wrong:\n%s", s)
	}
}

func TestTitle(t *testing.T) {
	g := New(12, 3)
	g.Box(0, 0, 12, 3, "frame", false)
	g.Title(2, 0, []Seg{{Text: " t ", Style: "title"}})
	line := strings.Split(g.Plain(), "\n")[0]
	if line != "┌─ t ──────┐" {
		t.Fatalf("title row: %q", line)
	}
	if g.At(2, 0).Style != "title" {
		t.Fatal("title lost its style")
	}
}

func TestMark(t *testing.T) {
	g := New(6, 3)
	g.Text(1, 1, "hello", "s", -1)
	g.Mark(0, 0, 6, 2, "pane")
	for x := 0; x < 6; x++ {
		if got := g.At(x, 1).ID; got != "pane" {
			t.Fatalf("Mark missed (%d, 1): id %q", x, got)
		}
	}
	if g.At(0, 2).ID != "" {
		t.Fatal("Mark spilled past its rectangle")
	}
	c := g.At(1, 1)
	if c.Rune != 'h' || c.Style != "s" {
		t.Fatal("Mark clobbered runes or styles")
	}
}

func TestMarkClips(t *testing.T) {
	g := New(4, 4)
	g.Mark(-2, -2, 8, 8, "all")
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			if g.At(x, y).ID != "all" {
				t.Fatalf("Mark clip missed (%d, %d)", x, y)
			}
		}
	}
}

func TestPaintRow(t *testing.T) {
	g := New(4, 2)
	g.Text(0, 1, "ab", "s", -1)
	g.PaintRow(1, "hot")
	for x := 0; x < 4; x++ {
		if got := g.At(x, 1).Style; got != "hot" {
			t.Fatalf("PaintRow cell %d style %q", x, got)
		}
	}
	if g.At(0, 1).Rune != 'a' {
		t.Fatal("PaintRow clobbered runes")
	}
	if g.At(1, 1).ID != "" {
		t.Fatal("PaintRow changed ids")
	}
}

func TestPaintRowKeepsIDs(t *testing.T) {
	g := New(3, 2)
	g.Mark(0, 1, 3, 1, "z")
	g.PaintRow(1, "hot")
	if g.At(2, 1).ID != "z" {
		t.Fatal("PaintRow dropped ids")
	}
}

func TestPaintRowOutOfBounds(t *testing.T) {
	g := New(3, 2)
	g.PaintRow(-1, "x")
	g.PaintRow(2, "x")
	if g.Plain() != "   \n   " {
		t.Fatal("PaintRow out of bounds wrote something")
	}
}

// ---------- widgets ----------

func TestBar(t *testing.T) {
	if got := Bar(2, 5); got != "██░░░" {
		t.Fatalf("Bar(2,5) = %q", got)
	}
	if got := Bar(5, 5); got != "█████" {
		t.Fatalf("Bar(5,5) = %q", got)
	}
	if got := Bar(-3, 4); got != "░░░░" {
		t.Fatalf("Bar(-3,4) = %q", got)
	}
	if got := Bar(9, 4); got != "████" {
		t.Fatalf("Bar(9,4) = %q", got)
	}
	if Bar(3, 0) != "" || Bar(3, -2) != "" {
		t.Fatal("non-positive total must give an empty bar")
	}
}

func TestMeterOK(t *testing.T) {
	g := New(20, 1)
	g.Meter(0, 0, "cpu", 30, 60, 85, 20, "30%")
	row := g.Plain()
	if !strings.HasPrefix(row, "cpu ") {
		t.Fatalf("meter label missing: %q", row)
	}
	if !strings.HasSuffix(row, "30%") {
		t.Fatalf("right part not right-aligned: %q", row)
	}
	for x := 4; x < 16; x++ {
		if s := g.At(x, 0).Style; s != "ok" {
			t.Fatalf("ok meter style at %d = %q", x, s)
		}
	}
}

func TestMeterWarn(t *testing.T) {
	g := New(20, 1)
	g.Meter(0, 0, "cpu", 70, 60, 85, 20, "70%")
	if s := g.At(6, 0).Style; s != "warn" {
		t.Fatalf("warn meter style = %q", s)
	}
}

func TestMeterDanger(t *testing.T) {
	g := New(20, 1)
	g.Meter(0, 0, "cpu", 90, 60, 85, 20, "90%")
	for x := 4; x < 16; x++ {
		if s := g.At(x, 0).Style; s != "bad" {
			t.Fatalf("bad meter style at %d = %q", x, s)
		}
	}
}

func TestMeterWarningTick(t *testing.T) {
	g := New(20, 1)
	g.Meter(0, 0, "cpu", 30, 60, 85, 20, "30%")
	tick := -1
	for x := 4; x < 16; x++ {
		if g.At(x, 0).Rune == '╎' {
			tick = x
		}
	}
	if tick < 0 {
		t.Fatal("no ╎ tick in meter")
	}
	if got := float64(tick-4) / 12 * 100; got < 55 || got > 65 {
		t.Fatalf("tick at %d (%.0f%%), want ~60%%", tick, got)
	}
}

func TestMeterClips(t *testing.T) {
	g := New(10, 1)
	g.Meter(-3, 0, "longlabel", 50, 60, 85, 20, "100%") // must not panic
	g.Meter(0, 5, "x", 50, 60, 85, 10, "y")             // off-grid row, ignored
	// "longlabel" starts at -3, so its first three runes are clipped
	// away and the row starts with the tail of the label. The bar and
	// the right-aligned value are cut at the grid edge.
	if g.At(0, 0).Rune != 'g' || !strings.HasSuffix(g.Plain(), "██░") {
		t.Fatalf("unexpected clip result: %q", g.Plain())
	}
}

func TestSparkline(t *testing.T) {
	if got := Sparkline([]float64{0, 5, 10}, 0, 10); got != "▁▅█" {
		t.Fatalf("Sparkline scale wrong: %q", got)
	}
}

func TestSparklineRungs(t *testing.T) {
	got := Sparkline([]float64{0, 1, 2, 3, 4, 5, 6, 7}, 0, 7)
	if got != "▁▂▃▄▅▆▇█" {
		t.Fatalf("Sparkline rungs: %q", got)
	}
}

func TestSparklineClamps(t *testing.T) {
	got := Sparkline([]float64{-5, 100}, 0, 10)
	if got != "▁█" {
		t.Fatalf("Sparkline clamp: %q", got)
	}
}

func TestSparklineFlat(t *testing.T) {
	got := Sparkline([]float64{3, 3, 3}, 3, 3)
	if got != "▅▅▅" {
		t.Fatalf("flat Sparkline: %q", got)
	}
}

func TestSparklineEmpty(t *testing.T) {
	if Sparkline(nil, 0, 10) != "" {
		t.Fatal("empty sparkline must be empty")
	}
}

func TestSpinner(t *testing.T) {
	cycle := []rune("▖▘▝▗")
	for i, want := range cycle {
		if got := Spinner(i); got != want {
			t.Fatalf("Spinner(%d) = %q, want %q", i, got, want)
		}
	}
	if Spinner(4) != '▖' || Spinner(10) != '▝' {
		t.Fatal("Spinner does not cycle")
	}
	if Spinner(-1) != '▗' {
		t.Fatalf("Spinner(-1) = %q", Spinner(-1))
	}
}

// ---------- glyphs ----------

func TestCheckClean(t *testing.T) {
	g := New(8, 2)
	g.Text(0, 0, "ok 42", "", -1)
	g.Box(0, 1, 8, 1, "", false)    // too small, draws nothing
	g.Box(0, 1, 8, 2, "", true)     // a dashed box of glyphs
	g.Text(0, 1, Bar(2, 8), "", -1) // blocks land over the dashed box
	if err := g.Check(""); err != nil {
		t.Fatalf("Check rejected an all-glyph grid: %v", err)
	}
}

func TestCheckRejectsUnknown(t *testing.T) {
	g := New(4, 1)
	g.Put(2, 0, '☕', "s")
	err := g.Check("")
	if err == nil || !strings.Contains(err.Error(), "U+2615") {
		t.Fatalf("Check(☕) = %v, want a U+2615 failure", err)
	}
}

func TestCheckExtra(t *testing.T) {
	g := New(2, 1)
	g.Put(0, 0, '◐', "s")
	if err := g.Check("◐"); err != nil {
		t.Fatalf("Check with ◐ in extra: %v", err)
	}
	if err := g.Check(""); err == nil {
		t.Fatal("◐ must fail Check without extra")
	}
}

func TestCheckToleratesBlank(t *testing.T) {
	g := New(4, 1)
	if err := g.Check(""); err != nil {
		t.Fatalf("blank grid failed Check: %v", err)
	}
	g.Put(0, 0, '\x7f', "s") // DEL is not printable ASCII
	if err := g.Check(""); err == nil {
		t.Fatal("DEL must fail Check")
	}
}

func TestGlyphsInFont(t *testing.T) {
	seen := map[string]bool{}
	data, err := os.ReadFile("fontcodepoints.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		seen[line] = true
	}
	for _, r := range Glyphs {
		if r <= 0x7F {
			t.Fatalf("Glyphs contains ASCII %q", r)
		}
		if !seen[strings.ToUpper(strconv.FormatUint(uint64(r), 16))] {
			t.Errorf("Glyphs rune %q (U+%04X) is not in fontcodepoints.txt", r, r)
		}
	}
	// 8 box (█ shared by Bar and Sparkline's top rung): 6 corners/lines
	// + 2 dashed, 2 blocks, 7 more rungs, 4 quadrants = 21.
	if n := len([]rune(Glyphs)); n != 21 {
		t.Errorf("Glyphs has %d runes, want 21; update the count if the set changes", n)
	}
}

func TestHalfCirclesNotInGlyphs(t *testing.T) {
	for _, r := range "◐◓◑◒" {
		if strings.ContainsRune(Glyphs, r) {
			t.Errorf("half circle %q must not be in Glyphs: the shipped font lacks it", r)
		}
	}
}

// FontHas knows the shipped font: Latin with accents yes, the
// half-filled circles and double-width scripts no. Sanitize keeps what
// the font draws and replaces the rest and every control character.
func TestFontHasAndSanitize(t *testing.T) {
	for _, r := range "aé─█▖ДΩ" {
		if !FontHas(r) || !Drawable(r) {
			t.Errorf("FontHas/Drawable(%q) = false, want true", r)
		}
	}
	for _, r := range "◐流" {
		if FontHas(r) {
			t.Errorf("FontHas(%q) = true, want false", r)
		}
	}
	for _, r := range "​‮́�" {
		if Drawable(r) {
			t.Errorf("Drawable(%U) = true, want false (zero-width, bidi, combining or replacement)", r)
		}
	}
	if got, want := Sanitize("café \x1b[31m流 a​b é ok\xff"), "café ?[31m? a?b e? ok?"; got != want {
		t.Errorf("Sanitize = %q, want %q", got, want)
	}
}
