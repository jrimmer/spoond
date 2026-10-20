package gitignore

import "strings"

// Pattern represents a single gitignore pattern line with its rule
// file's domain (the domain's directory segments prefix the line).
type Pattern []string

// pattern is one compiled gitignore line: the raw line text plus the
// flags derived from it.
type pattern struct {
	line

	// pattern holds the raw line text (no leading `!`, no trailing
	// "/" and no anchored-qualifying leading "/"; glob segments stay
	// as written).
	pattern string
}

// Matcher decides whole segment paths against a compiled pattern list.
type Matcher interface {
	// Match reports whether path (slash-split segments) is ignored;
	// isDir says the path's last segment names a directory.
	Match(path []string, isDir bool) bool
	// MatchAny reports whether any line matched at all, ignoring
	// the final word (a matched negation still speaks).
	MatchAny(path []string, isDir bool) bool
}

// ParsePattern attaches the rule file's domain to one pattern line
// (the domain's directory segments prefix the line, so the line only
// ever matches paths below the directory; nil for the walk root's own
// file).
func ParsePattern(p string, domain []string) Pattern {
	return Pattern(append(append([]string{}, domain...), p))
}

// parseCompiled compiles one raw line into the matcher's pattern
// form: the line's meaning (negation, dir-only, anchoring) is read
// from the raw text and carried on the pattern's flags, as
// gitignore(5) specifies.
func parseCompiled(p string, domain []string) pattern {
	pt := &pattern{pattern: p}
	pt.domain = domain
	trimmed := p
	if strings.HasPrefix(trimmed, "!") {
		pt.nc = true
		trimmed = trimmed[1:]
	}
	if strings.HasSuffix(trimmed, "/") {
		pt.isDir = true
		trimmed = trimmed[:len(trimmed)-1]
	}
	pt.ir = strings.HasPrefix(trimmed, "/")
	trimmed = strings.TrimPrefix(trimmed, "/")
	if trimmed == "" {
		return *pt
	}
	// The pattern's segment form: the raw text with `!` and the
	// anchoring "/" off, split on "/"; the matcher matches it
	// segment-wise, `**` segments consumed structurally.
	segs := strings.Split(trimmed, "/")
	pt.ir = pt.ir || len(segs) > 1
	pt.pattern = strings.Join(segs, "/")
	return *pt
}

// Compile turns parsed Pattern lines into matcher patterns.
func Compile(ps []Pattern) []pattern {
	out := make([]pattern, 0, len(ps))
	for _, p := range ps {
		if len(p) == 0 {
			continue
		}
		lineText := p[len(p)-1]
		domain := append([]string{}, p[:len(p)-1]...)
		if lineText == "" || strings.HasPrefix(lineText, "#") {
			continue
		}
		out = append(out, parseCompiled(lineText, domain))
	}
	return out
}
