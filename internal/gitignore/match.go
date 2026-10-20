package gitignore

import (
	"strings"
)

const (
	separator = "/"
)

// matchPattern is the glob engine: one compiled pattern line against a
// path split into segments. The domain (the rule file's directory)
// must prefix the path, an anchored ("rooted") pattern matches only
// from the path's start, an unanchored one at any depth, `**` spans
// segments and a dir-only pattern never ignores a file.
func matchPattern(p *pattern, path []string, isDir bool) int {
	if len(p.domain) > 0 {
		if len(p.domain) >= len(path) {
			return -1
		}
		for i, d := range p.domain {
			if d != path[i] {
				return -1
			}
		}
		path = path[len(p.domain):]
	}

	pat := strings.Split(p.pattern, separator)
	if len(pat) > 0 && pat[len(pat)-1] == "" {
		pat = pat[:len(pat)-1] // the trailing "/" (dir-only) is no segment
	}
	if len(pat) == 0 {
		return -1
	}

	if !p.ir {
		// Unanchored: the pattern may match at any depth, so try
		// every start offset.
		found := -1
		for off := 0; off < len(path); off++ {
			if segMatch(pat, path[off:], isDir, p.isDir) {
				found = off
				break
			}
		}
		if found < 0 {
			return -1
		}
		path = path[found:]
	}

	if !segMatch(pat, path, isDir, p.isDir) {
		return -1
	}
	if p.nc {
		return 0
	}
	return 1
}

// segMatch matches the pattern's segments against the path's from the
// start, with `**` consuming any number of segments. A dir-only
// pattern must not ignore a file.
func segMatch(pat, path []string, isDir, isDirOnly bool) bool {
	pi, si := 0, 0
	for pi < len(pat) {
		if pat[pi] == "**" {
			// A trailing "**" names what is INSIDE the directory, so it
			// needs at least one segment below it: "a/**" ignores
			// a/b but not a itself (git's rule), which lets "!a/keep"
			// keep an entry. A dir-only pattern then needs the matched
			// last segment to be a directory.
			if pi == len(pat)-1 {
				if si >= len(path) {
					return false
				}
				return !isDirOnly || isDir
			}
			for skip := 0; si+skip <= len(path); skip++ {
				if segMatch(pat[pi+1:], path[si+skip:], isDir, isDirOnly) {
					return true
				}
			}
			return false
		}
		if si >= len(path) {
			return false
		}
		if !segGlob(pat[pi], path[si]) {
			return false
		}
		pi++
		si++
	}
	if si < len(path) {
		return false // pattern exhausted with path left over
	}
	if isDirOnly && !isDir {
		// The pattern reached a file: a dir-only pattern does not
		// ignore it.
		return false
	}
	return true
}

// segGlob matches one path segment against one pattern segment with
// `*`, `?` and `[...]` classes (no nested classes; a `[` without a
// closing `]` stays a literal).
func segGlob(patSegment, name string) bool {
	px, nx := 0, 0
	starX, starN := -1, -1
	for nx < len(name) {
		if px < len(patSegment) {
			switch patSegment[px] {
			case '*':
				starX, starN = px, nx
				px++
				continue
			case '?':
				nx++
				px++
				continue
			case '[':
				if end := strings.IndexByte(patSegment[px:], ']'); end > 1 {
					c := name[nx]
					class := patSegment[px+1 : px+end]
					neg := class[0] == '!' || class[0] == '^'
					if neg {
						class = class[1:]
					}
					matched := classMatch(class, c)
					if neg {
						matched = !matched
					}
					if matched {
						px += end + 1
						nx++
						continue
					}
				} else {
					// No closing ]: a literal [.
					if name[nx] == '[' {
						px++
						nx++
						continue
					}
				}
			}
			if patSegment[px] == '\\' && px+1 < len(patSegment) {
				px++
			}
			if px < len(patSegment) && patSegment[px] == name[nx] {
				px++
				nx++
				continue
			}
		}
		if starX >= 0 {
			starN++
			nx = starN
			px = starX + 1
			continue
		}
		return false
	}
	for px < len(patSegment) && patSegment[px] == '*' {
		px++
	}
	return px == len(patSegment)
}

// classMatch reports whether c is in a character class's body (ranges
// a-z, singles).
func classMatch(class string, c byte) bool {
	for i := 0; i < len(class); i++ {
		if i+2 < len(class) && class[i+1] == '-' {
			if class[i] <= c && c <= class[i+2] {
				return true
			}
			i += 2
			continue
		}
		if class[i] == c {
			return true
		}
	}
	return false
}
