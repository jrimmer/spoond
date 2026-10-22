// Ignore matching follows gitignore(5) through this repository's own
// matcher (internal/gitignore), with this package's walk layer on top:
// the ignore stack decides scopes and precedence, the matcher the
// patterns.
package dirwalk

import (
	"path"
	"strings"

	"github.com/jrimmer/spoond/v4/internal/gitignore"
)

// loadIgnoreLines parses one .gitignore file's contents into matcher
// patterns, dropping blank lines and `#` comments (a literal
// `\#`-escaped hash is not a comment), and trimming the trailing
// spaces gitignore(5) ignores unless escaped (the escaped form
// "foo\\ bar" survives with its space).
func loadIgnoreLines(content string) gitignore.Pattern {
	var out gitignore.Pattern
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = trimTrailingSpaces(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// A bare `!` or `/` names nothing; a lone `\\#`-style escape
		// with no body is nothing either.
		if line == "!" || line == "/" || line == "\\" {
			continue
		}
		out = append(out, line)
	}
	return out
}

// trimTrailingSpaces drops the spaces gitignore(5) itself ignores:
// any run of trailing spaces unless each is escaped by a backslash.
// It trims no space that an odd (escaping) backslash precedes.
func trimTrailingSpaces(line string) string {
	end := len(line)
	for end > 0 && line[end-1] == ' ' {
		// Count the backslashes immediately before the space: an odd
		// count means the space is escaped and stays.
		bs := 0
		for bs < end-1 && line[end-2-bs] == '\\' {
			bs++
		}
		if bs%2 == 1 {
			break
		}
		end--
	}
	return line[:end]
}

// ignoreRule is one directory's ignore patterns with the directory's
// path. Patterns are relative to their directory (the dir becomes the
// matcher's domain), so a nested .gitignore's `x` names that
// directory's own x, and git's precedence (a deeper file's decision
// wins) comes from the walk's rule order. Below the walk root the dir
// is the rel form ("" at the root); an ancestor set's dir holds its
// absolute guest path (above the walk root: the walk's rel space
// cannot name it).
type ignoreRule struct {
	dir      string
	patterns gitignore.Pattern
}

// ignoreStack holds every pattern set in effect during the walk, keyed
// by directory, so a directory compiles its .gitignore once and a
// child inherits its ancestors' sets by construction of the walk.
//
// Rules live only inside a git work tree: the root's rules wake when a
// .git directory turns up among the root's children (the root itself
// is the work tree); rules read deeper in the walk push live when
// their directory sat at or below a work tree the walk had reached
// (the root's, or a nested repo's), which push tracks by rel path.
type ignoreStack struct {
	rules []ignoreRule
	// walkRoot is the walk root's absolute guest path: an ancestor
	// rule set's dir rides ABOVE it (the ancestor's absolute form),
	// and the matcher's paths carry the absolute guest path so the
	// domain prefix-strips across the walk root too.
	walkRoot string
	// pendingRoots holds the ignore sets loaded before the first List
	// (the listed directory's and its ancestors', each with its own
	// dir): pendingSet marks them loaded; the walk's .git discovery
	// confirms the work tree (activate) and turns them into rules.
	pendingRoots []pendingRoot
	pendingSet   bool
	// active says the listed root itself is a work tree (.git seen at
	// the root): every pushed rule set is live from then on.
	active bool
	// workTrees holds the directories below the root the walk
	// confirmed as work trees (a .git among their children): its
	// subtree's rules are live.
	workTrees []string
	// pendingSubtrees holds live-domain rule sets pushed before their
	// directory's work tree was confirmed (a nested repo's deeper
	// .gitignore files read on the way down). activateSubtree wakes
	// them with the work tree.
	pendingSubtrees []ignoreRule
	// matchers caches each rule list's compiled matcher (the walk
	// asks every listed child against the deepest scopes).
	matchers map[string]gitignore.Matcher
}

// activate wakes the pending root rules once a work tree is confirmed
// (a .git directory child seen). A .git file (the exotic gitfile form)
// does not make a work tree here: the rules stay off.
func (g *ignoreStack) activate(isDir bool) {
	if g.active || !isDir {
		return
	}
	g.active = true
	for i := len(g.pendingRoots) - 1; i >= 0; i-- {
		// Every pending set sits at or below the confirmed work-tree
		// root (the load stopped there): all go live, deepest-first
		// as loaded, each keeping its dir as its scope.
		g.rules = append(g.rules, ignoreRule{dir: g.pendingRoots[i].dir, patterns: g.pendingRoots[i].lines})
	}
	g.pendingRoots = nil
	g.pendingSet = false
}

// addRoot adds the listed directory's own .gitignore lines: they
// compile with no domain (they anchor at the listed root) and stay
// live for the whole walk.
func (g *ignoreStack) addRoot(lines []string) {
	g.rules = append(g.rules, ignoreRule{dir: "", patterns: lines})
}

// rootAncestorsIgnored decides the listed root and its ancestors
// against the live rules, top down, as directories. An ignored one
// makes the listing empty, as git sees it: git never descends into an
// ignored directory, so listing node_modules itself is empty too, and a
// deeper negation ("!a/b" under "a/") cannot re-include anything below.
// The root's own rules (dir "") never decide the root itself.
func (g *ignoreStack) rootAncestorsIgnored(root string) bool {
	var ancestors []string
	for a := root; ; a = path.Dir(a) {
		if a == "." || a == "" || a == "/" {
			break
		}
		ancestors = append(ancestors, a)
	}
	for i := len(ancestors) - 1; i >= 0; i-- {
		if g.absoluteIgnored(ancestors[i], true) {
			return true
		}
	}
	return false
}

// absoluteIgnored decides one absolute path (a directory above or at
// the walk root) against the live rules, deepest rule first, the way
// ignored decides a path below the root.
func (g *ignoreStack) absoluteIgnored(abs string, isDir bool) bool {
	for i := len(g.rules) - 1; i >= 0; i-- {
		rule := g.rules[i]
		if rule.dir == "" || !withinDir(rule.dir, abs) {
			continue
		}
		paths := splitDomain(abs)
		if m := g.matcherFor(rule); m.Match(paths, isDir) {
			return true
		}
		if g.matcherFor(rule).MatchAny(paths, isDir) {
			// A matched negation: re-included, final.
			return false
		}
	}
	return false
}

// activateAncestors wakes the loaded pending sets (the listed
// directory's and its ancestors', each with its own dir) when the
// probe already found the work tree (a .git at or above the listed
// directory): every set sits at or below the work-tree root by
// construction, so all go live, deepest-first as loaded.
func (g *ignoreStack) activateAncestors(pending []pendingRoot) {
	g.active = true
	for i := len(pending) - 1; i >= 0; i-- {
		if len(pending[i].lines) == 0 {
			continue
		}
		g.rules = append(g.rules, ignoreRule{dir: pending[i].dir, patterns: pending[i].lines})
	}
	g.pendingRoots = nil
	g.pendingSet = false
}

// push adds dir's ignore lines to the stack, live when dir sits in a
// work tree: dir holds a .git itself (workTree), or the walk entered
// one above dir. Outside any work tree the set waits on
// pendingSubtrees: its directory's .git may still turn up below it.
// The dir (the walk's rel form) is stored absolute: the matcher paths
// are absolute.
func (g *ignoreStack) push(dir string, lines []string, workTree bool) {
	if workTree {
		g.workTrees = append(g.workTrees, dir)
	}
	if len(lines) == 0 {
		return
	}
	rule := ignoreRule{dir: g.absDir(dir), patterns: lines}
	if workTree || g.workTreeDir(dir) {
		g.rules = append(g.rules, rule)
		return
	}
	g.pendingSubtrees = append(g.pendingSubtrees, rule)
}

// absDir renders a rel dir (below the walk root) as the walk tree's
// absolute-ish path; "" is the walk root itself.
func (g *ignoreStack) absDir(dir string) string {
	if dir == "" {
		return g.walkRoot
	}
	return g.walkRoot + "/" + dir
}

// inWorkTree reports whether rel sits at or below a confirmed work
// tree: the root's confirmation (active — the walk root's own .git or
// a probe-found .git at/above it) covers everything, the nested ones
// their subtree.
func (g *ignoreStack) inWorkTree(rel string) bool {
	if g.active {
		return true
	}
	abs := g.absDir(rel)
	for _, w := range g.workTrees {
		if withinDir(w, abs) {
			return true
		}
	}
	return false
}

// workTreeDir reports whether dir is at or below a confirmed work
// tree.
func (g *ignoreStack) workTreeDir(dir string) bool {
	return g.inWorkTree(dir)
}

// activateSubtree records that rel holds a .git directory (a nested
// repo) and wakes every pending rule set rooted below rel, newest
// (deepest) first: gitignore applies the ignore files of rel's
// subdirectories too, so their rules go live with the work tree.
func (g *ignoreStack) activateSubtree(rel string) {
	abs := g.absDir(rel)
	g.workTrees = append(g.workTrees, abs)
	for i := len(g.pendingSubtrees) - 1; i >= 0; i-- {
		p := g.pendingSubtrees[i]
		if withinDir(abs, p.dir) {
			g.rules = append(g.rules, p)
			g.pendingSubtrees = append(g.pendingSubtrees[:i], g.pendingSubtrees[i+1:]...)
		}
	}
}

// pop unwinds the stack past dir (the walk backtracks): the live
// rules and the pending-subtree ones both go, so a stale set from a
// sibling subtree cannot outlive its directory.
func (g *ignoreStack) pop(dir string) {
	abs := g.absDir(dir)
	for len(g.rules) > 0 && g.rules[len(g.rules)-1].dir == abs {
		g.rules = g.rules[:len(g.rules)-1]
	}
	for i := len(g.pendingSubtrees) - 1; i >= 0; i-- {
		if g.pendingSubtrees[i].dir == abs {
			g.pendingSubtrees = append(g.pendingSubtrees[:i], g.pendingSubtrees[i+1:]...)
		}
	}
}

// ignored decides rel (a slash path below the walk root, no trailing
// slash) against the stack, git's precedence: a deeper rule file's
// decision beats a shallower one's, and within a file the last
// matching line wins (negation included). ignored says the path is
// left out; decided says a rule spoke at all — a matched negation
// re-includes and stops the search (a shallower rule file cannot
// re-ignore what a deeper one let back in). isDir says the path names
// a directory. Never decides on the walk root itself (rel == "").
func (g *ignoreStack) ignored(rel string, isDir bool) (ignored, decided bool) {
	if rel == "" {
		return false, false
	}
	abs := g.guestPath(rel)
	for i := len(g.rules) - 1; i >= 0; i-- {
		rule := g.rules[i]
		if !withinDir(rule.dir, abs) {
			continue
		}
		dec := g.ruleDecides(rule, rel, abs, isDir)
		if dec == 0 {
			continue
		}
		if dec > 0 {
			// The rule's last word is a negation: re-included, final.
			return false, true
		}
		return true, true
	}
	return false, false
}

// guestPath renders rel (below the walk root) as the walk tree's
// absolute-ish form the rule dirs carry: walkRoot + "/" + rel, or
// walkRoot itself (never "" — the walk ignores the root, but the
// matcher paths want the full form).
func (g *ignoreStack) guestPath(rel string) string {
	return g.walkRoot + "/" + rel
}

// ruleDecides renders one rule's decision on rel/abspath: +1
// re-include (a matched negation), -1 ignore, 0 no match. Within the
// rule the last matching line wins; Match answers the ignore question,
// and a negation-only match surfaces as Match=false with
// MatchAny=true.
//
// The walk root's own ignore files (dir "") are the work-tree root's
// files when the root is a work tree: their lines are relative to the
// root and compile with no domain, the domain git gives those files.
// Every other set carries its directory as the domain and is matched
// against the walk tree's absolute-ish path (an ancestor's absolute
// form, or a nested directory's).
func (g *ignoreStack) ruleDecides(rule ignoreRule, rel, abspath string, isDir bool) int {
	paths := splitDomain(abspath)
	if rule.dir == "" {
		paths = splitDomain(rel)
	}
	if m := g.matcherFor(rule); m.Match(paths, isDir) {
		return -1
	}
	if m := g.matcherFor(rule); m.MatchAny(paths, isDir) {
		// A line matched whose last word was a negation.
		return 1
	}
	return 0
}

// ruleKey is the matcher cache's key: the rule's directory and its
// pattern lines.
func ruleKey(rule ignoreRule) string {
	return rule.dir + "\x00" + strings.Join(rule.patterns, "\x00")
}

// matcherFor returns the rule's compiled matcher, building it on the
// stack's cache (a rule is compiled once per walk). The rule lines'
// domain is the rule directory's own segment form — except the walk
// root's own set (dir "", the pending load of the listed directory's
// .gitignore and of .git/info/exclude when the walk root is a
// work-tree root): it compiles with no domain, the domain git gives
// the work-tree root's files, and ruleDecides matches it against the
// path relative to the root, so its anchored (`/build`) and
// embedded-slash (`doc/frotz`) lines anchor at the listed root.
func (g *ignoreStack) matcherFor(rule ignoreRule) gitignore.Matcher {
	if m, ok := g.matchers[ruleKey(rule)]; ok {
		return m
	}
	domain := splitDomain(rule.dir)
	if rule.dir == "" {
		domain = nil
	}
	m := gitignore.NewMatcher(gitignore.Compile(
		g.rulePatterns(rule, domain)))
	if g.matchers == nil {
		g.matchers = map[string]gitignore.Matcher{}
	}
	g.matchers[ruleKey(rule)] = m
	return m
}

// rulePatterns renders one rule's lines as Pattern values with the
// rule directory as each line's domain.
func (g *ignoreStack) rulePatterns(rule ignoreRule, domain []string) []gitignore.Pattern {
	out := make([]gitignore.Pattern, 0, len(rule.patterns))
	for _, line := range rule.patterns {
		out = append(out, gitignore.ParsePattern(line, domain))
	}
	return out
}

// withinDir reports whether ruleDir is p or an ancestor of p (both
// the walk tree's absolute-ish form; an empty ruleDir always covers).
func withinDir(ruleDir, p string) bool {
	if ruleDir == "" {
		return true
	}
	return p == ruleDir || strings.HasPrefix(p, ruleDir+"/")
}

// splitDomain splits a slash path into its segments ("" gives none),
// the matcher paths' shape.
func splitDomain(p string) []string {
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}
