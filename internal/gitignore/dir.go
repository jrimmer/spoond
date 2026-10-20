// Package gitignore matches gitignore(5) patterns. It is this
// repository's own matcher: a small glob engine and a precedence
// layer, written for the directory walk and kept dependency-free.
package gitignore

// dir is a matcher over a list of compiled gitignore patterns.
type dir struct {
	patterns []pattern
}

// NewMatcher builds a matcher from compiled patterns.
func NewMatcher(ps []pattern) Matcher {
	return &dir{patterns: ps}
}

// Match reports whether the path is ignored. It decides the path's
// own segments only: the walk decides every directory as it enters it
// and prunes an ignored one, so an ancestor's decision was already
// made and a deeper rule's negation on an ancestor (say "!lib" for
// sub/lib) must not re-include the entry below it. Within the list the
// last matching line wins (git's negation).
func (d *dir) Match(path []string, isDir bool) bool {
	ok, _ := d.decide(path, isDir)
	return ok == 1
}

// MatchAny reports whether any pattern line matched the path at all,
// whichever its final word (true when the last word was a negation,
// which Match answers as not-ignored). The walk's rule search uses it
// to stop at a matched negation.
func (d *dir) MatchAny(path []string, isDir bool) bool {
	_, any := d.decide(path, isDir)
	return any
}

// decide evaluates the pattern list at one path prefix: ok is -1 (no
// match) or the last matching line's decision (1 ignore, 0 negated
// re-include); any says a line matched at all.
func (d *dir) decide(path []string, isDir bool) (ok int, any bool) {
	for i := len(d.patterns) - 1; i >= 0; i-- {
		if r := d.patterns[i].match(path, isDir); r >= 0 {
			return r, true
		}
	}
	return -1, false
}
