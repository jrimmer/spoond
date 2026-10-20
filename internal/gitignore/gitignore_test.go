package gitignore

import (
	"os"
	"strings"
	"testing"
)

// matcherFor compiles raw pattern lines (gitignore(5) syntax) into a
// matcher with every line's domain nil (the walk root's own file).
func matcherFor(lines []string) Matcher {
	ps := make([]Pattern, 0, len(lines))
	for _, l := range lines {
		ps = append(ps, ParsePattern(l, nil))
	}
	return NewMatcher(Compile(ps))
}

func TestMatchBasics(t *testing.T) {
	cases := []struct {
		line  string
		path  string
		isDir bool
		want  bool
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
		{"logs/", "logs", false, false}, // dir-only: a file is not ignored
		{"logs/", "x/logs", true, true},
		{"debug/", "debug", false, false},
		{"**/tmp", "a/tmp", true, true},
		{"**/tmp", "tmp", true, true},
		{"a/**/z", "a/b/c/z", false, true},
		{"a/**/z", "a/z", false, true},
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
		{"important.log", "x/important.log", false, true},
		{"a/b/c", "a/b/c/d", false, false}, // the walk decides a/b/c itself, then prunes it
		{"a", "a/b", true, false},          // an entry's own path only; the walk enters a first
		{"a", "a/b/c", true, false},
		{"/a", "a/b/c", true, false},
		{"b/c", "a/b/c", true, false},
	}
	for _, c := range cases {
		got := matcherFor([]string{c.line}).Match(strings.Split(c.path, "/"), c.isDir)
		if got != c.want {
			t.Errorf("line %q on %q (dir %v) ignored = %v, want %v", c.line, c.path, c.isDir, got, c.want)
		}
	}
}

func TestMatchLastLineWins(t *testing.T) {
	m := matcherFor([]string{"*.log", "!important.log", "# comment", ""})
	if m.Match(strings.Split("x/important.log", "/"), false) {
		t.Fatal("!important.log must re-include x/important.log")
	}
	if !m.Match(strings.Split("x/other.log", "/"), false) {
		t.Fatal("*.log must ignore x/other.log")
	}
	if len(Compile([]Pattern{[]string{"# c"}, []string{""}, []string{"keep"}})) != 1 {
		t.Fatal("comments and blank lines must not compile")
	}
}

func TestMatchStarStarDirOnlyNeverIgnoresFile(t *testing.T) {
	// gitignore(5): a dir-only pattern (a trailing /) never ignores a
	// file of that name, whatever `**` it carries.
	for _, line := range []string{"**/tmp/", "tmp/"} {
		m := matcherFor([]string{line})
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
	if matcherFor([]string{"/tmp/"}).Match([]string{"a", "tmp"}, true) {
		t.Error("/tmp/ must not ignore a/tmp (anchored)")
	}
}

func TestMatchStarStarAnchoring(t *testing.T) {
	// "**/build": matches build at any depth (gitignore(5): "**/foo"
	// means "foo" together with "**/foo").
	m := matcherFor([]string{"**/build"})
	if !m.Match([]string{"build"}, false) {
		t.Error("**/build must ignore the file build")
	}
	if !m.Match([]string{"a", "b", "build"}, false) {
		t.Error("**/build must ignore deep build")
	}
	// An embedded ** anchors the pattern.
	m = matcherFor([]string{"a/**/b"})
	if !m.Match([]string{"a", "b"}, false) {
		t.Error("a/**/b must match a/b (the ** may match nothing)")
	}
	if !m.Match([]string{"a", "x", "y", "b"}, false) {
		t.Error("a/**/b must match a/x/y/b")
	}
	if m.Match([]string{"x", "a", "b"}, false) {
		t.Error("a/**/b must not match x/a/b (anchored by the embedded slash)")
	}
}

func TestMatchDomain(t *testing.T) {
	// A pattern from a rule file in repo/src matches only below that
	// directory.
	withDom := Compile([]Pattern{ParsePattern("*.log", []string{"repo", "src"})})
	m := NewMatcher(withDom)
	if !m.Match([]string{"repo", "src", "a.log"}, false) {
		t.Error("a domain pattern must match below its domain")
	}
	if m.Match([]string{"repo", "other", "a.log"}, false) {
		t.Error("a domain pattern must not match outside its domain")
	}
	if m.Match([]string{"repo", "a.log"}, false) {
		t.Error("a domain pattern must not match at the domain itself (strictly below)")
	}
}

func TestMatchNegationReInclude(t *testing.T) {
	m := matcherFor([]string{"!keep.log"})
	if m.Match([]string{"keep", "keep.log"}, false) {
		t.Error("a matched negation must NOT ignore (Match false)")
	}
	m2 := matcherFor([]string{"*.log", "!important.log"})
	if m2.Match([]string{"important.log"}, false) {
		t.Error("negation must re-include (last line wins)")
	}
	if !m.MatchAny([]string{"keep", "keep.log"}, false) {
		t.Error("MatchAny reports the matched negation")
	}
}

// TestMatchEntryPathOnly: a matcher decides one path, never its
// ancestors. The walk decides every directory as it enters it (and
// prunes an ignored one), so a deeper rule's negation on an ancestor
// must not re-include the entry itself: "*.pyc" in the root and "!lib"
// in sub/.gitignore leave sub/lib/x.pyc ignored, as git does.
func TestMatchEntryPathOnly(t *testing.T) {
	root := NewMatcher(Compile([]Pattern{
		ParsePattern("*.pyc", nil),
	}))
	if !root.Match([]string{"sub", "lib", "x.pyc"}, false) {
		t.Fatal("*.pyc must ignore sub/lib/x.pyc (the entry's own path matches)")
	}
	// The deeper rule's negation speaks only for the lib directory.
	sub := NewMatcher(Compile([]Pattern{
		ParsePattern("!lib", []string{"sub"}),
	}))
	if sub.Match([]string{"sub", "lib"}, true) {
		t.Fatal("!lib must not ignore the lib directory")
	}
	if sub.Match([]string{"sub", "lib", "x.pyc"}, false) {
		t.Fatal("!lib must not name sub/lib/x.pyc itself")
	}
}

// TestMatchTrailingGlobstarNeedsSegment: a trailing "/**" matches
// what is inside the directory, at least one segment below it. "a/**"
// alone does not ignore a, so "!a/keep" can still keep a/keep.
func TestMatchTrailingGlobstarNeedsSegment(t *testing.T) {
	m := matcherFor([]string{"a/**", "!a/keep"})
	if m.Match([]string{"a"}, true) {
		t.Fatal("a/** must not ignore a itself")
	}
	if m.Match([]string{"a", "keep"}, true) {
		t.Fatal("!a/keep must re-include a/keep")
	}
	if !m.Match([]string{"a", "other"}, true) {
		t.Fatal("a/** must ignore a/other")
	}
	if !m.Match([]string{"a", "b", "x"}, false) {
		t.Fatal("a/** must ignore a/b/x")
	}
	if matcherFor([]string{"**"}).Match([]string{"x"}, false) != true {
		t.Fatal("a bare ** still matches one segment")
	}
	// A dir-only trailing ** names directories only.
	if matcherFor([]string{"a/**/"}).Match([]string{"a", "file"}, false) {
		t.Fatal("a/**/ must not ignore the file a/file")
	}
	if !matcherFor([]string{"a/**/"}).Match([]string{"a", "dir"}, true) {
		t.Fatal("a/**/ must ignore the directory a/dir")
	}
}

// TestProvenanceComments: the matcher and the walk layer are this
// repository's own code, and their package comments say so. Nothing
// attributes them to an upstream copy, and neither file claims a
// vendored origin.
func TestProvenanceComments(t *testing.T) {
	for _, file := range []string{"dir.go", "match.go", "pattern.go", "../dirwalk/gitignore.go"} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		text := string(data)
		for _, claim := range []string{"go-git", "checked-in copy"} {
			if strings.Contains(text, claim) {
				t.Errorf("%s attributes the code to an upstream copy (%q)", file, claim)
			}
		}
	}
	// The package comment describes what the package is.
	for _, tc := range []struct{ file, want string }{
		{"dir.go", "this"},
		{"../dirwalk/gitignore.go", "gitignore(5)"},
	} {
		data, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		head := string(data)
		if i := strings.Index(head, "package "); i >= 0 {
			head = head[:i]
		}
		if !strings.Contains(head, tc.want) {
			t.Errorf("%s package comment must mention %q, got %q", tc.file, tc.want, head)
		}
	}
}
