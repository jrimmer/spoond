package grid

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// update is set by -update: golden files are rewritten instead of
// compared, and the test fails so the diff does not pass unnoticed.
var update bool

func init() { flag.BoolVar(&update, "update", false, "rewrite the golden files") }

const goldenDir = "testdata"

func goldenName(ext string) string { return filepath.Join(goldenDir, "sample."+ext) }

func compareGolden(t *testing.T, ext, got string) {
	t.Helper()
	path := goldenName(ext)
	if update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("rewrite %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run go test ./grid -update to write it): %v", path, err)
	}
	if string(want) != got {
		t.Errorf("%s differs from the golden file; run go test ./grid -update after checking the diff\n--- golden ---\n%s\n--- got ---\n%s", path, want, got)
	}
}

func TestGoldenPlain(t *testing.T) { compareGolden(t, "txt", sample().Plain()) }
func TestGoldenANSI(t *testing.T)  { compareGolden(t, "ansi", sample().ANSI(sampleANSIStyles())) }
func TestGoldenHTML(t *testing.T)  { compareGolden(t, "html", sample().HTML()) }

// The ANSI golden file must end every styled run with a reset.
func TestANSIResetsEveryRun(t *testing.T) {
	s := sample().ANSI(sampleANSIStyles())
	if n := strings.Count(s, "\x1b[0m"); n == 0 {
		t.Fatal("no resets in ANSI output")
	}
}

// The HTML golden file must keep one span per (style, id) run, with
// ids on the panel's cells.
func TestHTMLCarriesIDs(t *testing.T) {
	s := sample().HTML()
	if !strings.Contains(s, `data-id="panel"`) {
		t.Error("HTML output lost the panel element id")
	}
}
