// Package dirwalk walks a substrate filesystem's directory tree
// bounded and gitignore-aware, for the files API's ?list=1.
//
// The walker reads one directory level at a time through a
// substrate.ListDir view, so ignored directories are pruned without
// ever being listed (a node_modules is never walked), and it stops at
// the first entry past one of the limits: an entry budget, a byte
// budget (both measured on the response entries) and a wall clock. A
// stop answers truncated=true with what fit; only a substrate failure
// aborts.
package dirwalk

import (
	"context"
	"errors"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jrimmer/spoond/v4/substrate"
)

// Limits is the walk's bounds. A zero turns a limit off.
type Limits struct {
	// MaxEntries stops at the first entry past this many response
	// entries (0 = unbounded).
	MaxEntries int
	// MaxBytes stops when the next entry would push the response
	// entries' JSON bytes past it.
	MaxBytes int
	// Timeout stops the walk once exceeded (0 = unbounded).
	Timeout time.Duration
	// Hidden lists dot-names; false hides every name starting with a
	// dot (the gitignore reading keeps its own eyes: an ignored tree
	// is still pruned with hidden off).
	Hidden bool
	// Exclude names entries (files and directories) to skip with
	// everything below them, at any depth (the API's comma-separated
	// ?exclude=).
	Exclude []string
	// Gitignore applies .gitignore and .git/info/exclude rules inside
	// a git work tree (the API's default true).
	Gitignore bool
	// MaxIgnoreFile bytes bounds every ignore-file read, so a huge or
	// growing .gitignore cannot pin the walk. It is not a truncation
	// limit: an over-size ignore file is skipped.
	MaxIgnoreFile int64
}

// Result is one walk's entries and how it ended.
type Result struct {
	// Entries, in walk order (depth first, siblings in byte order,
	// paths relative to the listed directory, "/" separators, the
	// listed directory itself never named).
	Entries []substrate.PathEntry
	// Bytes is the cost of Entries' JSON (the entries array
	// elements), the figure max_bytes counts.
	Bytes int
	// Truncated says an entry existed past a limit; false says the
	// whole tree below the directory fit.
	Truncated bool
}

// readFS is the tree view the walk reads, so tests can stub it without
// a substrate.
type readFS interface {
	// List returns dir's direct children, ok=false when the directory
	// is unreadable but present (vanished, permissions: the walk
	// skips its contents). A transport failure is err.
	List(ctx context.Context, dir string) (entries []substrate.DirEntry, ok bool, err error)
	// ReadFile returns a file's bytes; errIgnoreMissing (mapped from
	// substrate.ErrNotFound by the real view) for a missing file.
	ReadFile(ctx context.Context, fpath string, max int64) ([]byte, error)
	// ProbeGit reports whether dir holds a .git and whether it is a
	// directory (a work tree) or a file (the exotic gitfile form).
	// It answers the work-tree question for the listed directory's
	// ANCESTORS, which the walk never Lists.
	ProbeGit(ctx context.Context, dir string) (found bool, isDir bool, err error)
}

// errIgnoreMissing is the readFS's missing-file marker.
var errIgnoreMissing = errors.New("dirwalk: no such ignore file")

// noRulesThere reports whether an ignore-file read above the walk root
// found no rules to load: the file is missing, or it (or its directory)
// is unreadable. Neither fails the listing; the walk goes on without
// that file's rules, as git does with a file it cannot read.
func noRulesThere(err error) bool {
	return errors.Is(err, errIgnoreMissing) || errors.Is(err, substrate.ErrPermission)
}

// errStopped says the walk hit a limit; Walk turns it into a truncated
// success.
var errStopped = errors.New("dirwalk: limit reached")

// Walk lists the tree below root (the directory itself never named),
// bounded by lim. Unreadable entries are skipped; other kinds (sockets,
// devices, fifos) are left out; symlinks are named but never followed.
func Walk(ctx context.Context, fs readFS, root string, lim Limits) (Result, error) {
	res := Result{Entries: []substrate.PathEntry{}}
	var deadline time.Time
	if lim.Timeout > 0 {
		// The timeout bounds the whole walk, a single slow substrate
		// call included: the context reaches ListDir and ReadFile and
		// cancels whatever is in flight when it fires.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, lim.Timeout)
		defer cancel()
		deadline = time.Now().Add(lim.Timeout)
	}
	g := &ignoreStack{walkRoot: root}
	if lim.Gitignore {
		// Pending rules load before the first list: every ancestor's
		// .gitignore up to the work-tree root (gitignore applies the
		// ignore files of the path and its parents up to there). The
		// probes also find the work tree itself (a .git directory at
		// or above root): found, the ancestors' lines go live at once.
		// The listed directory's own .gitignore is read from its
		// listing, only when that listing shows one.
		pending, wt, err := loadPendingRoots(ctx, fs, root, ignoreFileMax(lim))
		if err != nil {
			if ctx.Err() != nil {
				res.Truncated = true
				return res, nil
			}
			return res, err
		}
		if wt {
			// A .git directory at or above the listed path: a walk
			// rooted inside a work tree. The ancestors' lines go live,
			// each at its own directory, so a parent's anchored
			// pattern keeps its own scope.
			g.activateAncestors(pending)
			// Decide the listed root's own ancestors first, top down:
			// an ignored ancestor makes the listing empty, exactly as
			// git never descends into it (a deeper "!a/b" cannot
			// re-include below an ignored a).
			if g.rootAncestorsIgnored(root) {
				return res, nil
			}
		} else {
			g.pendingRoots = pending
			g.pendingSet = true
		}
	}
	res, err := walkDir(ctx, fs, root, "", lim, g, res, deadline)
	if err != nil {
		if errors.Is(err, errStopped) || ctx.Err() != nil {
			res.Truncated = true
			return res, nil
		}
		return res, err
	}
	if ctx.Err() != nil {
		res.Truncated = true
	}
	return res, nil
}

// walkDir lists dpath (rel below the root) and recurses.
func walkDir(ctx context.Context, fs readFS, dpath, rel string, lim Limits, g *ignoreStack, res Result, deadline time.Time) (Result, error) {
	if err := ctx.Err(); err != nil {
		return res, err
	}
	if !deadline.IsZero() && time.Now().After(deadline) {
		res.Truncated = true
		return res, errStopped
	}
	entries, ok, err := fs.List(ctx, dpath)
	if err != nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if rel != "" && (errors.Is(err, substrate.ErrPermission) || errors.Is(err, substrate.ErrNotFound)) {
			// A directory below the root that vanished or refuses a
			// listing is skipped, never fatal: its entry stays named,
			// nothing below it is.
			return res, nil
		}
		return res, err
	}
	if !ok {
		// Unreadable (vanished, permissions): nothing below is named.
		// The parent already listed the directory itself.
		return res, nil
	}
	sortEntries(entries)
	if rel == "" && lim.Gitignore && g.pendingSet {
		for _, e := range entries {
			if e.Name == ".git" {
				// A .git directory at the root makes the tree a work
				// tree: the pending ancestor rules wake.
				g.activate(e.Kind == substrate.DirDirectory)
				break
			}
		}
	}
	// A .git directory among the children marks this directory a work
	// tree (a nested repo below the listed root): its subtree's
	// ignore rules go live, the pending deeper-subtree rules included.
	// A directory already inside a confirmed work tree needs no
	// confirmation of its own.
	if lim.Gitignore && rel != "" && !g.inWorkTree(rel) {
		for _, e := range entries {
			if e.Name == ".git" && e.Kind == substrate.DirDirectory {
				g.activateSubtree(rel)
				break
			}
		}
	}
	// The directory's own .gitignore is read only when its listing
	// shows one, and it governs the entries listed below.
	if lim.Gitignore && hasIgnoreFile(entries) {
		lines, err := loadIgnoreLinesFor(ctx, fs, dpath, ignoreFileMax(lim))
		if err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			if !errors.Is(err, substrate.ErrPermission) && !errors.Is(err, substrate.ErrNotFound) {
				return res, err
			}
			lines = nil // an unreadable ignore file contributes nothing
		}
		if len(lines) > 0 {
			if rel == "" {
				// The listed directory's own file compiles with no
				// domain (it anchors at the listed root) and only
				// lives inside a confirmed work tree.
				if g.active {
					g.addRoot(lines)
				}
			} else {
				g.push(rel, lines, g.inWorkTree(rel))
				defer g.pop(rel)
			}
		}
	}
	for _, e := range entries {
		name := e.Name
		if name == ".git" {
			// Always skipped with everything below it, ignored or not.
			continue
		}
		if !lim.Hidden && strings.HasPrefix(name, ".") {
			continue
		}
		if excluded(name, lim.Exclude) {
			continue
		}
		childRel := name
		if rel != "" {
			childRel = rel + "/" + name
		}
		isDir := e.Kind == substrate.DirDirectory
		ignored, _ := g.ignored(childRel, isDir)
		if lim.Gitignore && ignored {
			// An ignored directory is pruned without being listed (a
			// node_modules is never walked); an ignored file is never
			// named.
			continue
		}
		entry := substrate.PathEntry{Path: childRel, Kind: e.Kind}
		if len(res.Entries)+1 > lim.MaxEntries && lim.MaxEntries > 0 {
			res.Truncated = true
			return res, errStopped
		}
		cost := entryBytes(entry)
		if lim.MaxBytes > 0 && res.Bytes+cost > lim.MaxBytes {
			res.Truncated = true
			return res, errStopped
		}
		res.Entries = append(res.Entries, entry)
		res.Bytes += cost
		if !isDir {
			continue
		}
		child := path.Join(dpath, name)
		res, err = walkDir(ctx, fs, child, childRel, lim, g, res, deadline)
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

// ignoreFileMax is the ignore-file read cap, defaulting at 1 MiB when
// Limits leaves it unset.
func ignoreFileMax(lim Limits) int64 {
	if lim.MaxIgnoreFile > 0 {
		return lim.MaxIgnoreFile
	}
	return 1 << 20
}

// entryBytes is the JSON size of one entry object:
// {"kind":"f","path":"<escaped>"}, plus the separating comma (the
// array's brackets ride with the response envelope, one entry's slack
// each, which the API rounds into max_bytes).
func entryBytes(e substrate.PathEntry) int {
	return len(`{"kind":"`) + len(e.Kind) + len(`","path":"`) + jsonEscapeSize(e.Path) + len(`"}`) + 1
}

// jsonEscapeSize counts the bytes of s as a JSON string body the way
// Go's default encoder writes them (writeJSON's): quotes, backslashes
// and control characters grow, and so do <, > and & (the encoder's
// HTML escaping writes \u003c, \u003e and \u0026).
func jsonEscapeSize(s string) int {
	n := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			// An invalid byte is written as \ufffd, six bytes.
			n += 6
			i++
			continue
		}
		switch {
		case r == '"' || r == '\\' || r == '\b' || r == '\f' || r == '\n' || r == '\r' || r == '\t':
			n += 2
		case r < 0x20 || r == '<' || r == '>' || r == '&' || r == '\u2028' || r == '\u2029':
			n += 6 // \u00XX or \u2028
		default:
			n += size
		}
		i += size
	}
	return n
}

// excluded reports whether name is one of the caller's exclude names.
func excluded(name string, exclude []string) bool {
	for _, x := range exclude {
		if x == name {
			return true
		}
	}
	return false
}

// sortEntries orders siblings in byte order of their names (Go's string
// compare is byte order).
func sortEntries(entries []substrate.DirEntry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
}

// hasIgnoreFile reports whether a directory's listing shows a
// .gitignore. A directory's ignore file is only read when its own
// listing names one.
func hasIgnoreFile(entries []substrate.DirEntry) bool {
	for _, e := range entries {
		if e.Name == ".gitignore" {
			return true
		}
	}
	return false
}

// loadIgnoreLinesFor reads dpath's .gitignore as rule lines. A
// missing file contributes nothing.
func loadIgnoreLinesFor(ctx context.Context, fs readFS, dpath string, max int64) ([]string, error) {
	var out []string
	data, err := fs.ReadFile(ctx, path.Join(dpath, ".gitignore"), max)
	switch {
	case err == nil:
		out = append(out, loadIgnoreLines(string(data))...)
	case !errors.Is(err, errIgnoreMissing):
		return nil, err
	}
	return out, nil
}

// pendingRoot is one rule file's lines with the directory it governs,
// in the walk's rule-dir form (rel below the walk root; "" is the
// listed directory itself).
type pendingRoot struct {
	dir   string
	lines []string
}

// loadPendingRoots reads every strict ancestor's .gitignore
// (child-most first) up to the work-tree root, plus the work-tree
// root's .git/info/exclude, each as its own pending set (git's
// precedence: a deeper file's decision wins; the walk keeps this
// order, deepest-last). It also reports whether a .git DIRECTORY
// sits at root or at any ancestor: the walk's work tree. That tree
// is only ever a .git at or ABOVE root — root's own List tells
// nothing about ancestors, so the probe walks up. With no work tree
// at or above root, no ancestor file is read at all.
//
// The ancestor sets ride their dir in the walk's rel form (the walk
// root itself is dir ""; an ancestor keeps the path below root, which
// the rule stack prefixes back onto the walk root).
func loadPendingRoots(ctx context.Context, fs readFS, root string, max int64) (pending []pendingRoot, workTree bool, err error) {
	// The .git probe, root then up: the nearest holder is the work
	// tree (a nested repo nearer than a higher one).
	holder := ""
	for a := root; holder == ""; {
		found, isDir, perr := fs.ProbeGit(ctx, a)
		if perr != nil && !errors.Is(perr, substrate.ErrPermission) {
			return nil, false, perr
		}
		// An unreadable directory above the root holds no .git we can
		// see: keep probing upward.
		if found {
			if isDir {
				holder = a
			}
			break // a .git file confirms nothing beyond
		}
		if a == "/" {
			break
		}
		a = path.Dir(a)
	}
	workTree = holder != ""
	if holder == "" {
		// No work tree at or above the listed path: no ancestor's
		// .gitignore may be read (rules only live inside one). The
		// listed directory's own file is read from its listing.
		return nil, false, nil
	}

	// When the listed directory IS the work-tree root, its
	// .git/info/exclude applies too, with the same dir "" (the walk
	// root): the ancestor loop below never reaches root itself.
	if holder != "" && holder == root {
		data, err := fs.ReadFile(ctx, path.Join(root, ".git/info/exclude"), max)
		switch {
		case err == nil:
			if l := loadIgnoreLines(string(data)); len(l) > 0 {
				pending = append(pending, pendingRoot{dir: "", lines: l})
			}
		case !noRulesThere(err):
			return nil, false, err
		}
	}

	// The strict ancestors up to (and including) the work-tree root,
	// child-most first: .gitignore lines, and .git/info/exclude for
	// the work-tree root itself. Above the work-tree root, nothing:
	// git stops there. With no work tree at or above root
	// (holder == ""), no ancestor's lines load: rules only live in a
	// work tree, and the walk's own List (a .git at root) is the
	// only other confirmation.
	a := path.Dir(root)
	for {
		if holder != "" && !withinDir(holder, a) {
			break // past the work-tree root upward: git stops
		}
		if a != root {
			// The listed directory's own .gitignore is not an
			// ancestor's: the walk reads it from the listing.
			data, err := fs.ReadFile(ctx, path.Join(a, ".gitignore"), max)
			switch {
			case err == nil:
				if l := loadIgnoreLines(string(data)); len(l) > 0 {
					pending = append(pending, pendingRoot{dir: ancestorRel(root, a), lines: l})
				}
			case !noRulesThere(err):
				return nil, false, err
			}
		}
		if a == holder {
			// The work-tree root's exclude file; when root itself is
			// that work-tree root, the load above already took it.
			if a != root {
				data, err := fs.ReadFile(ctx, path.Join(a, ".git/info/exclude"), max)
				switch {
				case err == nil:
					if l := loadIgnoreLines(string(data)); len(l) > 0 {
						pending = append(pending, pendingRoot{dir: ancestorRel(root, a), lines: l})
					}
				case !noRulesThere(err):
					return nil, false, err
				}
			}
			break
		}
		if a == "/" {
			break
		}
		a = path.Dir(a)
	}
	return pending, workTree, nil
}

// ancestorRel renders an ancestor's absolute dir as the walk's rel
// form: "" when dir is root itself; else the path below root.
func ancestorRel(root, dir string) string {
	if dir == root {
		return ""
	}
	return strings.TrimPrefix(dir, root+"/")
}
