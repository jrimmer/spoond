package gitignore

// line carries one gitignore pattern line's flags, the matcher's
// compiled form: nc negated (`!`), ir anchored to the rule file's
// directory, isDir trailing-slash directories-only, domain the rule
// file's directory segments (nil at the walk root).
type line struct {
	nc     bool
	ir     bool
	isDir  bool
	domain []string
}

// match reports the pattern's decision on path: 1 ignore, 0 explicit
// re-include (negation), -1 no match.
func (p *pattern) match(path []string, isDir bool) int {
	return matchPattern(p, path, isDir)
}
