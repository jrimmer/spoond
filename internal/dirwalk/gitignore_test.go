package dirwalk

import (
	"strings"
	"testing"

	"github.com/jrimmer/spoond/v3/internal/gitignore"
)

// compileOne compiles one pattern line for the match cases.
func compileOne(t *testing.T, line string) *gitignore.Pattern {
	t.Helper()
	pat := loadIgnoreLines(line + "\n")
	if len(pat) != 1 {
		t.Fatalf("loadIgnoreLines(%q) = %d patterns, want 1", line, len(pat))
	}
	return &pat
}

func TestGitignoreBasics(t *testing.T) {
	cases := []struct {
		line   string
		match  string
		isDir  bool
		ignore bool // want ignored=true
	}{
		{"node_modules", "node_modules", true, true},
		{"node_modules", "node_modules", false, true},
		{"node_modules", "a/node_modules", true, true},
		{"node_modules", "a/b/node_modules", true, true},
		{"node_modules", "node_modulesx", true, false},
		{"/build", "build", true, true},
		{"/build", "sub/build", true, false},
		{"*.log", "a.log", false, true},
		{"*.log", "sub/b.log", false, true},
		{"*.log", "log", false, false},
		{"logs/", "logs", true, true},
		{"logs/", "logs", false, false}, // dir-only: a file of that name is not ignored
		{"logs/", "x/logs", true, true},
		{"debug/", "debug", false, false},
		{"**/tmp", "a/tmp", true, true},
		{"**/tmp", "tmp", true, true},
		{"a/**/z", "a/b/c/z", true, true},
		{"a/**/z", "a/z", true, true},
		{"doc/frotz", "doc/frotz", false, true},
		{"doc/frotz", "a/doc/frotz", false, false}, // embedded slash anchors
		{"?", "a", false, true},
		{"?.go", "x.go", false, true},
		{"[ab].txt", "a.txt", false, true},
		{"[ab].txt", "c.txt", false, false},
		{"[a-c].txt", "b.txt", false, true},
		{"[!a].txt", "b.txt", false, true},
		{"[!a].txt", "a.txt", false, false},
		{"\\#literal", "#literal", false, true},
	}
	for _, c := range cases {
		m := matcherFor(t, []string{c.line})
		got := m.Match(strings.Split(c.match, "/"), c.isDir)
		if got != c.ignore {
			t.Errorf("line %q on %q (dir %v) ignored = %v, want %v", c.line, c.match, c.isDir, got, c.ignore)
		}
	}
}

func TestGitignoreNegationAndComments(t *testing.T) {
	// Last match wins within a file.
	m := matcherFor(t, []string{"*.log", "!important.log", "# comment", "", "trailing/space  "})
	if m.Match(strings.Split("x/important.log", "/"), false) {
		t.Fatal("!important.log must re-include x/important.log")
	}
	if !m.Match(strings.Split("x/other.log", "/"), false) {
		t.Fatal("*.log must ignore x/other.log (the trailing-space line is dead weight)")
	}
	if len(loadIgnoreLines("  \n#. c\nkeep  \n")) != 1 {
		t.Fatal("blank, comment and trailing-space-dropped lines must not compile")
	}
}

// matcherFor compiles raw pattern lines (gitignore(5) syntax) at the
// walk root (no domain) into a matcher.
func matcherFor(t *testing.T, lines []string) gitignore.Matcher {
	t.Helper()
	pats := make([]gitignore.Pattern, 0, len(lines))
	for _, l := range lines {
		pats = append(pats, gitignore.ParsePattern(l, nil))
	}
	return gitignore.NewMatcher(gitignore.Compile(pats))
}

func TestGitignoreFileThenNegateChildRule(t *testing.T) {
	// git's rule: a pattern from a deeper .gitignore may re-include.
	g := &ignoreStack{walkRoot: "/"}
	g.activate(true) // the root's own set rides the confirmed work tree
	g.push("", []string{"*.log"}, false)
	g.push("keep", []string{"!keep.log"}, true) // workTree: hold a .git
	if ign, dec := g.ignored("a.log", false); !ign || !dec {
		t.Fatal("a.log must be ignored")
	}
	if ign, dec := g.ignored("keep/keep.log", false); ign || !dec {
		t.Fatal("keep/keep.log must be re-included by the child rule (a decided negation)")
	}
	if ign, dec := g.ignored("keep/other.log", false); !ign || !dec {
		t.Fatal("keep/other.log stays ignored")
	}
}

func TestGitignoreIgnoredDirectoryPrunes(t *testing.T) {
	g := &ignoreStack{walkRoot: "/"}
	g.activate(true)
	g.push("", []string{"node_modules/"}, false)
	if ign, dec := g.ignored("node_modules", true); !ign || !dec {
		t.Fatal("node_modules dir must be ignored")
	}
	if ign, dec := g.ignored("node_modules", false); ign || dec {
		t.Fatal("a file named node_modules is not ignored by a dir-only pattern")
	}
	if ign, dec := g.ignored("x/node_modules", true); !ign || !dec {
		t.Fatal("node_modules below must be ignored too (unanchored)")
	}
}

func TestGitignoreInactiveOutsideWorkTree(t *testing.T) {
	g := &ignoreStack{walkRoot: "/"}
	g.activate(true)
	g.push("", []string{"x"}, false)
	if ign, dec := g.ignored("x", false); !ign || !dec {
		t.Fatal("rules apply in an active work tree")
	}
	// Nothing activates: no .git directory seen, rules stay off.
	g2 := &ignoreStack{walkRoot: "/"}
	g2.push("", []string{"x"}, false)
	if ign, dec := g2.ignored("x", false); ign || dec {
		t.Fatal("rules must stay off without a confirmed work tree")
	}
	g2.activate(false) // a .git file: no work tree
	if ign, dec := g2.ignored("x", false); ign || dec {
		t.Fatal("a .git file (not a directory) confirms no work tree")
	}
}

func TestGitignoreRootInfoExclude(t *testing.T) {
	g := &ignoreStack{}
	g.activate(true)
	// .git/info/exclude's patterns act like the root's rule set.
	g.push("", []string{"secret.txt"}, false)
	if ign, dec := g.ignored("secret.txt", false); !ign || !dec {
		t.Fatal(".git/info/exclude pattern must ignore secret.txt")
	}
	if ign, dec := g.ignored("sub/secret.txt", false); !ign || !dec {
		t.Fatal("unanchored pattern matches below too")
	}
}

func TestGitignorePatternDirectoryOnlyFileSubpaths(t *testing.T) {
	g := &ignoreStack{walkRoot: "/"}
	g.activate(true)
	g.push("", []string{"docs/build/"}, false)
	if ign, dec := g.ignored("docs/build", true); !ign || !dec {
		t.Fatal("docs/build must be ignored")
	}
	// The walk prunes at the directory, so deeper paths are never
	// asked; a probe of one still must not crash either way. The
	// dir-only pattern does not speak on the file itself.
	ign, dec := g.ignored("docs/build/out.txt", false)
	if dec && ign {
		t.Fatalf("the dir-only pattern must not name the file itself (ign %v dec %v)", ign, dec)
	}
}

func TestGitignoreStarStarDirOnlyNeverIgnoresFile(t *testing.T) {
	// gitignore(5): a dir-only pattern (a trailing /) never ignores a
	// file of that name, whatever `**` it carries.
	for _, line := range []string{"**/tmp/", "tmp/"} {
		m := matcherFor(t, []string{line})
		if m.Match([]string{"tmp"}, false) {
			t.Errorf("%q must not ignore the file tmp", line)
		}
		if !m.Match([]string{"tmp"}, true) {
			t.Errorf("%q must ignore the directory tmp", line)
		}
		if !m.Match([]string{"a", "tmp"}, true) {
			t.Errorf("%q must ignore the nested directory tmp", line)
		}
		if m.Match([]string{"a", "b", "tmp"}, false) {
			t.Errorf("%q must not ignore the file a/b/tmp", line)
		}
	}
	// An anchored dir-only pattern stays anchored.
	if matcherFor(t, []string{"/tmp/"}).Match([]string{"a", "tmp"}, true) {
		t.Error("/tmp/ must not ignore a/tmp (anchored)")
	}
}

func TestGitignoreStarStarAnchored(t *testing.T) {
	// "**/build": matches build at any depth (gitignore(5) "**/foo"
	// == "foo" + "**/foo").
	m := matcherFor(t, []string{"**/build"})
	if !m.Match([]string{"build"}, false) {
		t.Error("**/build must ignore the file build")
	}
	if !m.Match([]string{"a", "b", "build"}, false) {
		t.Error("**/build must ignore deep build")
	}
	// An embedded ** anchors.
	m = matcherFor(t, []string{"a/**/b"})
	if !m.Match([]string{"a", "b"}, false) {
		t.Error("a/**/b must match a/b (the ** may match nothing)")
	}
	if !m.Match([]string{"a", "x", "y", "b"}, false) {
		t.Error("a/**/b must match a/x/y/b")
	}
	if m.Match([]string{"x", "a", "b"}, false) {
		t.Error("a/**/b must not match x/a/b (anchored by the embedded slash)")
	}
	if matcherFor(t, []string{"a/**/b"}).Match([]string{"a", "b", "c"}, false) {
		t.Error("a/**/b must not ignore a/b/c itself (the walk prunes the matched dir a/b as it enters it)")
	}
	if !matcherFor(t, []string{"a/**/b"}).Match([]string{"a", "b"}, true) {
		t.Error("a/**/b must match the directory a/b (the ** may match nothing)")
	}
}

func TestGitignoreTrailingSpacesAndEscapes(t *testing.T) {
	m := matcherFor(t, []string{"foo\\ bar"})
	if !m.Match([]string{"foo bar"}, false) {
		t.Fatal("escaped space matches a space")
	}
	if m.Match([]string{"foo", "bar"}, false) {
		t.Fatal("escaped space must not match beyond")
	}
	// A trailing space that is not escaped is dropped (trailing/space
	// trimmed by the walker's line loader): "foo " matches foo, and
	// "foo  " (two trailing spaces) too.
	m2 := matcherFor(t, loadIgnoreLines("foo \nfoo  \n"))
	if !m2.Match([]string{"foo"}, false) {
		t.Fatal("unescaped trailing spaces are dropped (matches foo)")
	}
	if m2.Match([]string{"foo "}, false) {
		t.Fatal("the space must be gone, not kept as a pattern char")
	}
	if !matcherFor(t, loadIgnoreLines("foo\\ bar  \n")).Match([]string{"foo bar"}, false) {
		t.Fatal("the escaped space survives; the unescaped tail is dropped")
	}
}
