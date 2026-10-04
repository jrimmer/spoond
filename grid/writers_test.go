package grid

import (
	"strings"
	"testing"
)

func TestPlainKeepsTrailingSpaces(t *testing.T) {
	g := New(6, 2)
	g.Text(0, 0, "ab", "", -1)
	want := "ab    \n      "
	if got := g.Plain(); got != want {
		t.Fatalf("Plain: %q, want %q", got, want)
	}
}

func TestPlainMatchesGridSize(t *testing.T) {
	g := New(7, 3)
	lines := strings.Split(g.Plain(), "\n")
	if len(lines) != 3 || len(lines[0]) != 7 {
		t.Fatalf("Plain shape: %d lines, first %d cells", len(lines), len(lines[0]))
	}
}

func TestANSIWrapsStyledRunsWithReset(t *testing.T) {
	g := New(6, 1)
	g.Text(0, 0, "ab", "hot", -1)
	g.Text(2, 0, "cd", "cold", -1)
	got := g.ANSI(map[string]string{"hot": "\x1b[31m", "cold": "\x1b[44m"})
	want := "\x1b[31mab\x1b[0m\x1b[44mcd\x1b[0m  "
	if got != want {
		t.Fatalf("ANSI: %q, want %q", got, want)
	}
}

func TestANSIMergesAdjacentRuns(t *testing.T) {
	g := New(4, 1)
	g.Text(0, 0, "abcd", "hot", -1)
	got := g.ANSI(map[string]string{"hot": "\x1b[31m"})
	if got != "\x1b[31mabcd\x1b[0m" {
		t.Fatalf("adjacent same-style cells split: %q", got)
	}
}

func TestANSIUnknownStyleNoEscape(t *testing.T) {
	g := New(3, 1)
	g.Text(0, 0, "abc", "mystery", -1)
	if got := g.ANSI(nil); got != "abc" {
		t.Fatalf("unmapped style emitted escapes: %q", got)
	}
	if got := g.ANSI(map[string]string{"other": "\x1b[1m"}); got != "abc" {
		t.Fatalf("unmapped style emitted escapes: %q", got)
	}
}

func TestANSIEmptySequenceNoEscape(t *testing.T) {
	g := New(3, 1)
	g.Text(0, 0, "abc", "s", -1)
	if got := g.ANSI(map[string]string{"s": ""}); got != "abc" {
		t.Fatalf("empty SGR sequence emitted: %q", got)
	}
}

func TestANSIMultiRow(t *testing.T) {
	g := New(2, 2)
	g.Text(0, 0, "ab", "hot", -1)
	got := g.ANSI(map[string]string{"hot": "\x1b[1m"})
	// the second row holds unstyled blanks, so it emits no escapes
	want := "\x1b[1mab\x1b[0m\n  "
	if got != want {
		t.Fatalf("ANSI rows: %q, want %q", got, want)
	}
}

func TestHTMLRuns(t *testing.T) {
	g := New(5, 1)
	g.Text(0, 0, "ab", "hot", -1)
	g.Text(2, 0, "c", "cold", -1)
	got := g.HTML()
	want := `<span class="g-hot">ab</span><span class="g-cold">c</span><span>  </span>`
	if got != want {
		t.Fatalf("HTML: %q, want %q", got, want)
	}
}

func TestHTMLDataID(t *testing.T) {
	g := New(3, 1)
	g.Text(0, 0, "abc", "s", -1)
	g.Mark(0, 0, 3, 1, "pane")
	want := `<span class="g-s" data-id="pane">abc</span>`
	if got := g.HTML(); got != want {
		t.Fatalf("HTML: %q, want %q", got, want)
	}
}

func TestHTMLEscapesText(t *testing.T) {
	g := New(5, 1)
	g.Text(0, 0, `<&"'>`, "s", -1)
	want := `<span class="g-s">&lt;&amp;&#34;&#39;&gt;</span>`
	if got := g.HTML(); got != want {
		t.Fatalf("HTML escape: %q, want %q", got, want)
	}
}

func TestHTMLEscapesAttributes(t *testing.T) {
	g := New(2, 1)
	g.Put(0, 0, 'x', `a"b`)
	g.Mark(0, 0, 2, 1, `i"d`)
	got := g.HTML()
	if !strings.Contains(got, `class="g-a&#34;b"`) || !strings.Contains(got, `data-id="i&#34;d"`) {
		t.Fatalf("attribute not escaped: %q", got)
	}
}

func TestHTMLRowsJoinedByNewline(t *testing.T) {
	g := New(2, 2)
	got := g.HTML()
	if strings.Count(got, "\n") != 1 {
		t.Fatalf("HTML row separator: %q", got)
	}
}
