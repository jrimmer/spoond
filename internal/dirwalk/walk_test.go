package dirwalk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v4/substrate"
	"github.com/jrimmer/spoond/v4/substrate/fake"
)

// stubFS is an in-memory readFS for walker tests: dirs/files/symlinks
// by path; the ignore files' contents ride in files.
type stubFS struct {
	dirs       map[string]bool
	files      map[string]string
	symlinks   map[string]string
	unreadable map[string]bool  // List answers !ok, no error
	fail       error            // List answers fail instead of listing
	failDirs   map[string]error // List answers this error for one dir
	readErrs   map[string]error // ReadFile answers this error for one file
	// blockList, when set, parks every List until the context is done:
	// a slow substrate call the walk's timeout must cancel.
	blockList bool
	// watched, when set, collects every dir List is called with.
	watched *[]string
	// reads, when set, collects every file ReadFile is called with.
	reads *[]string
}

func newStub() *stubFS {
	return &stubFS{dirs: map[string]bool{"": true}, files: map[string]string{}, symlinks: map[string]string{}, unreadable: map[string]bool{}, failDirs: map[string]error{}, readErrs: map[string]error{}}
}

func (s *stubFS) addDir(p string) {
	p = strings.Trim(p, "/")
	if p == "" {
		return
	}
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			s.dirs[p[:i]] = true
		}
	}
	s.dirs[p] = true
}

func (s *stubFS) addFile(p, content string) {
	s.addDir(filepath.Dir("/" + p))
	s.files[strings.TrimPrefix(p, "/")] = content
}

func (s *stubFS) List(ctx context.Context, dir string) ([]substrate.DirEntry, bool, error) {
	dir = strings.TrimPrefix(dir, "/")
	if s.watched != nil {
		*s.watched = append(*s.watched, dir)
	}
	if s.blockList {
		<-ctx.Done()
		return nil, false, ctx.Err()
	}
	if s.fail != nil {
		return nil, false, s.fail
	}
	if err := s.failDirs[dir]; err != nil {
		return nil, false, err
	}
	if s.unreadable[dir] {
		return nil, false, nil
	}
	if !s.dirs[dir] {
		return nil, false, os.ErrNotExist
	}
	prefix := dir
	if prefix != "" {
		prefix += "/"
	}
	seen := map[string]string{}
	for p := range s.dirs {
		if p != "" && strings.HasPrefix(p, prefix) && !strings.Contains(strings.TrimPrefix(p, prefix), "/") {
			seen[strings.TrimPrefix(p, prefix)] = substrate.DirDirectory
		}
	}
	for p := range s.files {
		if strings.HasPrefix(p, prefix) && !strings.Contains(strings.TrimPrefix(p, prefix), "/") {
			seen[strings.TrimPrefix(p, prefix)] = substrate.DirFile
		}
	}
	for p := range s.symlinks {
		if strings.HasPrefix(p, prefix) && !strings.Contains(strings.TrimPrefix(p, prefix), "/") {
			seen[strings.TrimPrefix(p, prefix)] = substrate.DirSymlink
		}
	}
	var out []substrate.DirEntry
	for n, k := range seen {
		out = append(out, substrate.DirEntry{Name: n, Kind: k})
	}
	return out, true, nil
}

// ProbeGit reports a .git at dir: the stub's dirs map holds it (a
// directory) or its files map holds it (a .git file).
func (s *stubFS) ProbeGit(_ context.Context, dir string) (found bool, isDir bool, err error) {
	dir = strings.TrimPrefix(dir, "/")
	gp := dir + "/.git"
	if dir == "" {
		gp = ".git"
	}
	if s.dirs[gp] {
		return true, true, nil
	}
	if _, ok := s.files[gp]; ok {
		return true, false, nil
	}
	return false, false, nil
}

func (s *stubFS) ReadFile(_ context.Context, fpath string, max int64) ([]byte, error) {
	fpath = strings.TrimPrefix(fpath, "/")
	if s.reads != nil {
		*s.reads = append(*s.reads, fpath)
	}
	if err := s.readErrs[fpath]; err != nil {
		return nil, err
	}
	// Any file in the way of a read (e.g. /f/.gitignore) is missing.
	if s.files[fpath] == "" && !s.dirs[fpath] && s.symlinks[fpath] == "" && !strings.HasSuffix(fpath, "/.gitignore") {
		return nil, errIgnoreMissing
	}
	data, ok := s.files[fpath]
	if !ok {
		return nil, errIgnoreMissing
	}
	if max > 0 && int64(len(data)) > max {
		return nil, errIgnoreMissing
	}
	return []byte(data), nil
}

// paths renders the walk result as "kind:path" rows.
func paths(res Result) []string {
	var out []string
	for _, e := range res.Entries {
		out = append(out, e.Kind+":"+e.Path)
	}
	return out
}

func TestWalkOrderAndShapes(t *testing.T) {
	s := newStub()
	s.addFile("/b.txt", "")
	s.addFile("/a.txt", "")
	s.addDir("/sub")
	s.addFile("/sub/deep.txt", "")
	s.addDir("/sub/lower")
	s.symlinks["sub/link"] = "deep.txt"
	// A socket and a device are left out: the stub's other map stays
	// empty, so nothing extra appears (the stub cannot fake them; the
	// substrate's ListDir already filtered them).

	res, err := Walk(context.Background(), s, "/", Limits{Hidden: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	want := []string{
		"f:a.txt",
		"f:b.txt",
		"d:sub",
		"f:sub/deep.txt",
		"l:sub/link",
		"d:sub/lower",
	}
	got := paths(res)
	if !slices.Equal(got, want) {
		t.Fatalf("walk = %v, want %v (depth first, byte order siblings)", got, want)
	}
	if res.Truncated {
		t.Fatal("a small tree is not truncated")
	}
}

func TestWalkHiddenOff(t *testing.T) {
	s := newStub()
	s.addFile("/.hidden", "")
	s.addFile("/shown.txt", "")
	s.addDir("/.hide")
	s.addFile("/.hide/x", "")

	res, err := Walk(context.Background(), s, "/", Limits{Hidden: false})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := paths(res)
	if !slices.Equal(got, []string{"f:shown.txt"}) {
		t.Fatalf("walk = %v, want only shown.txt", got)
	}
}

func TestWalkExclude(t *testing.T) {
	s := newStub()
	s.addDir("/keep")
	s.addFile("/keep/f.txt", "")
	s.addDir("/skip-me")
	s.addFile("/skip-me/x.txt", "")
	s.addFile("/skip-me", "file too") // name match also skips a file

	res, err := Walk(context.Background(), s, "/", Limits{Hidden: true, Exclude: []string{"skip-me"}})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := paths(res)
	if !slices.Equal(got, []string{"d:keep", "f:keep/f.txt"}) {
		t.Fatalf("walk = %v, want the kept tree only", got)
	}
}

func TestWalkGitignoreNestedAndParent(t *testing.T) {
	s := newStub()
	// The work tree: .git at the root activates the rules.
	s.addDir("/.git")
	s.addFile("/.gitignore", "root-ign.txt\n*.log\n")
	s.addFile("/root-ign.txt", "")
	s.addFile("/keep.txt", "")
	// A nested .gitignore re-includes keep.log in its directory
	// (patterns are relative to the .gitignore's own directory).
	s.addDir("/sub")
	s.addFile("/sub/.gitignore", "!keep.log\n")
	s.addFile("/sub/keep.log", "")
	s.addFile("/sub/root-ign.txt", "")
	s.addFile("/sub/other.log", "")
	// An ignored directory ("logs/") is pruned with everything under it.
	s.addDir("/logs")
	s.addFile("/logs/x.txt", "")
	s.addFile("/.gitignore", "root-ign.txt\n*.log\nlogs/\n")

	res, err := Walk(context.Background(), s, "/", Limits{Hidden: true, Gitignore: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := paths(res)
	// .git is skipped with everything under it; the root's rules ignore
	// root-ign.txt and *.log everywhere; the sub .gitignore re-includes
	// keep.log below /sub; "logs/" ignores the logs directory with its
	// contents without walking it.
	want := []string{"f:.gitignore", "f:keep.txt", "d:sub", "f:sub/.gitignore", "f:sub/keep.log"}
	if !slices.Equal(got, want) {
		t.Fatalf("walk = %v, want %v", got, want)
	}
}

func TestWalkParentGitignoreFromSubdirectory(t *testing.T) {
	s := newStub()
	s.addDir("/.git")
	s.addFile("/.gitignore", "*.log\n!/keep/\n")
	s.addDir("/keep")
	s.addFile("/keep/ok.log", "") // parent's negation keeps it
	s.addFile("/drop.log", "")
	s.addFile("/plain.txt", "")

	res, err := Walk(context.Background(), s, "/keep", Limits{Hidden: true, Gitignore: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := paths(res)
	if !slices.Equal(got, []string{"f:ok.log"}) {
		t.Fatalf("walk = %v, want the parent rules applied below /keep (negation keeps ok.log)", got)
	}
}

func TestWalkSubdirectoryWorkTree(t *testing.T) {
	s := newStub()
	s.addDir("/home/user/repo/.git")
	s.addFile("/home/user/repo/.gitignore", "build\n")
	s.addFile("/home/user/.gitignore", "x\n")
	s.addFile("/home/user/x", "")
	s.addFile("/home/user/repo/build", "")
	s.addFile("/home/user/repo/app", "")

	res, err := Walk(context.Background(), s, "/home/user", Limits{Hidden: true, Gitignore: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	rows := paths(res)
	// /home/user's own .gitignore is inert (no .git at or above
	// /home/user within the walk); the repo's rules apply below it.
	if !slices.Contains(rows, "f:x") {
		t.Fatalf("x must survive the inert /home/user/.gitignore: %v", rows)
	}
	for _, row := range rows {
		if strings.Contains(row, "repo/build") {
			t.Fatalf("repo/build must be ignored by the repo's rules: %v", rows)
		}
	}
	if !slices.Contains(rows, "f:.gitignore") || !slices.Contains(rows, "f:repo/app") {
		t.Fatalf("rows = %v, want the repo's own files and the inert root ignore file", rows)
	}
}

func TestWalkIgnoresNeedWorkTree(t *testing.T) {
	s := newStub()
	s.addFile("/.gitignore", "x.txt\n") // but no .git anywhere
	s.addFile("/x.txt", "")

	res, err := Walk(context.Background(), s, "/", Limits{Hidden: true, Gitignore: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := paths(res)
	if len(got) != 2 || got[0] != "f:.gitignore" || got[1] != "f:x.txt" {
		t.Fatalf("walk = %v, want both (no work tree, no ignore)", got)
	}
}

func TestWalkPrunesWithoutWalking(t *testing.T) {
	s := newStub()
	s.addDir("/.git")
	s.addFile("/.gitignore", "node_modules/\n")
	s.addDir("/node_modules")
	s.addFile("/node_modules/left/package.json", "")
	s.addFile("/node_modules/left/inner.txt", "")
	s.addFile("/app.js", "")
	// Pin that node_modules is never listed: the stub records the dirs
	// its List was asked for.
	var listed []string
	s.watched = &listed

	res, err := Walk(context.Background(), s, "/", Limits{Hidden: true, Gitignore: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := paths(res)
	if !slices.Equal(got, []string{"f:.gitignore", "f:app.js"}) {
		t.Fatalf("walk = %v, want .gitignore and app.js (node_modules pruned)", got)
	}
	for _, dir := range listed {
		if strings.Contains(dir, "node_modules") {
			t.Fatalf("node_modules was listed (%q): must be pruned, not walked", dir)
		}
	}
}

func TestWalkGitAlwaysSkipped(t *testing.T) {
	s := newStub()
	s.addDir("/.git")
	s.addFile("/.git/config", "")
	s.addFile("/.git/info/exclude", "excl.txt\n")
	s.addFile("/excl.txt", "")
	s.addFile("/out.txt", "")

	res, err := Walk(context.Background(), s, "/", Limits{Hidden: true, Gitignore: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := paths(res)
	if slices.Contains(got, "d:.git") || slices.Contains(got, "d:.git/config") {
		t.Fatalf(".git must never be listed: %v", got)
	}
	// And the exclude file's rule applies (the root is a work tree).
	if !slices.Equal(got, []string{"f:out.txt"}) {
		t.Fatalf("walk = %v, want out.txt only (.git/info/exclude active)", got)
	}
}

func TestWalkTruncationEntries(t *testing.T) {
	s := newStub()
	for _, n := range []string{"a", "b", "c", "d"} {
		s.addFile("/"+n+".txt", "")
	}
	res, err := Walk(context.Background(), s, "/", Limits{Hidden: true, MaxEntries: 2})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := paths(res)
	if len(got) != 2 || res.Truncated != true {
		t.Fatalf("got %d entries truncated=%v at max_entries=2 (got %v)", len(got), res.Truncated, got)
	}
	res, err = Walk(context.Background(), s, "/", Limits{Hidden: true, MaxEntries: 4})
	if err != nil || res.Truncated {
		t.Fatalf("max_entries=4 fits: %v truncated=%v err=%v", paths(res), res.Truncated, err)
	}
}

func TestWalkTruncationBytes(t *testing.T) {
	s := newStub()
	s.addFile("/short", "")
	s.addFile("/a-very-long-file-name-with-many-letters.txt", "")

	long := substrate.PathEntry{Path: "a-very-long-file-name-with-many-letters.txt", Kind: "f"}
	short := substrate.PathEntry{Path: "short", Kind: "f"}
	// max_bytes=1 cannot hold even one entry.
	res, err := Walk(context.Background(), s, "/", Limits{Hidden: true, MaxBytes: 1})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Entries) != 0 || !res.Truncated {
		t.Fatalf("max_bytes=1 cannot hold even one entry (got %d, truncated %v)", len(res.Entries), res.Truncated)
	}
	// Byte order lists the long name first; a budget of exactly its
	// entry's cost holds it and stops before the short one.
	res, err = Walk(context.Background(), s, "/", Limits{Hidden: true, MaxBytes: entryBytes(long)})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Entries) != 1 || !res.Truncated {
		t.Fatalf("long fits alone: %d entries, truncated %v", len(res.Entries), res.Truncated)
	}
	// Both fit: the byte budget covers both entries (each entry's cost
	// includes its separating comma, counted generously).
	res, err = Walk(context.Background(), s, "/", Limits{Hidden: true, MaxBytes: entryBytes(long) + entryBytes(short)})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(res.Entries) != 2 || res.Truncated {
		t.Fatalf("both fit: %d entries, truncated %v", len(res.Entries), res.Truncated)
	}
}

func TestWalkTruncationTimeout(t *testing.T) {
	s := newStub()
	for i := 0; i < 1000; i++ {
		s.addDir("/d" + strings.Repeat("a", 40) + strconv.Itoa(i))
	}
	res, err := Walk(context.Background(), s, "/", Limits{Hidden: true, Timeout: time.Nanosecond})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if !res.Truncated || len(res.Entries) == 1000 {
		t.Fatalf("timeout must stop a deep tree: %d entries, truncated %v", len(res.Entries), res.Truncated)
	}
}

func TestWalkUnreadableSkipped(t *testing.T) {
	s := newStub()
	s.addDir("/present-but-locked")
	s.addFile("/present-but-locked/x", "")
	s.unreadable["present-but-locked"] = true
	s.addFile("/ok.txt", "")

	res, err := Walk(context.Background(), s, "/", Limits{Hidden: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := paths(res)
	if !slices.Equal(got, []string{"f:ok.txt", "d:present-but-locked"}) {
		t.Fatalf("walk = %v, want the locked dir named but not read", got)
	}
}

func TestWalkListFails(t *testing.T) {
	s := newStub()
	s.fail = errors.New("transport down")
	if _, err := Walk(context.Background(), s, "/", Limits{Hidden: true}); err == nil {
		t.Fatal("a transport failure must fail the walk")
	}
}

// fakeFS adapts the substrate fake to the walk's readFS, the way the
// API does.
type fakeFS struct {
	f  *fake.Fake
	id string
}

func (a fakeFS) List(ctx context.Context, dir string) ([]substrate.DirEntry, bool, error) {
	items, err := a.f.ListDir(ctx, a.id, dir)
	if err != nil {
		return nil, false, err
	}
	return items, true, nil
}

func (a fakeFS) ReadFile(ctx context.Context, fpath string, max int64) ([]byte, error) {
	data, err := a.f.ReadFile(ctx, a.id, fpath, max)
	if err != nil {
		if errors.Is(err, substrate.ErrNotFound) {
			return nil, errIgnoreMissing
		}
		return nil, err
	}
	return data, nil
}

// ProbeGit stats dir/.git (one call, no List of the ancestors).
func (a fakeFS) ProbeGit(ctx context.Context, dir string) (found bool, isDir bool, err error) {
	info, err := a.f.Stat(ctx, a.id, dir+"/.git")
	if err != nil {
		if errors.Is(err, substrate.ErrNotFound) {
			return false, false, nil
		}
		return false, false, err
	}
	return true, info.IsDir, nil
}

func TestWalkOnFakeSubstrate(t *testing.T) {
	f := fake.New()
	id := "i0123456789abcdefghij"
	ctx := context.Background()
	if _, err := f.Create(ctx, substrate.CreateRequest{SandboxID: id, TemplateID: "template0000000000000"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, p := range []struct {
		path    string
		content string
		dir     bool
	}{
		{"/repo/.gitignore", "ign.txt\n*.log\nnode_modules/\n", false},
		{"/repo/build.log", "", false},
		{"/repo/app.py", "app", false},
		{"/repo/node_modules/pkg/index.js", "x", false},
	} {
		if p.dir {
			if err := f.MakeDir(ctx, id, p.path, 0o755); err != nil {
				t.Fatalf("MakeDir %s: %v", p.path, err)
			}
			continue
		}
		if err := f.WriteFile(ctx, id, p.path, []byte(p.content), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", p.path, err)
		}
	}
	if err := f.MakeDir(ctx, id, "/repo/.git", 0o755); err != nil {
		t.Fatalf("MakeDir .git: %v", err)
	}
	if err := f.WriteFile(ctx, id, "/repo/.git/HEAD", []byte("ref"), 0o644); err != nil {
		t.Fatalf("WriteFile HEAD: %v", err)
	}

	res, err := Walk(ctx, fakeFS{f: f, id: id}, "/repo", Limits{Hidden: true, Gitignore: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := paths(res)
	if !slices.Equal(got, []string{"f:.gitignore", "f:app.py"}) {
		t.Fatalf("walk = %v, want .gitignore and app.py (ign.txt and node_modules pruned)", got)
	}
}

// TestWalkNestedGitignoreNoRootFiles: a repo whose root has no
// .gitignore and no .git/info/exclude still prunes what a nested
// .gitignore ignores: the walk-tree detection cannot ride on ignore
// files existing (the pendingRoot gate it once was).
func TestWalkNestedGitignoreNoRootFiles(t *testing.T) {
	s := newStub()
	s.addDir("/.git")
	s.addDir("/sub")
	s.addFile("/sub/.gitignore", "x.txt\n")
	s.addFile("/sub/x.txt", "")
	s.addFile("/sub/other.txt", "")

	res, err := Walk(context.Background(), s, "/", Limits{Hidden: true, Gitignore: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := paths(res)
	want := []string{"d:sub", "f:sub/.gitignore", "f:sub/other.txt"}
	if !slices.Equal(got, want) {
		t.Fatalf("walk = %v, want %v (the nested .gitignore ignores sub/x.txt)", got, want)
	}
}

// TestWalkAncestorRulesBelowRoot: a listing from inside a work tree
// (/repo/src, the work tree at /repo) applies the work-tree root's
// .gitignore: the parents up to the work-tree root ignore too.
func TestWalkAncestorRulesBelowRoot(t *testing.T) {
	s := newStub()
	s.addDir("/repo/.git")
	s.addFile("/repo/.git/HEAD", "ref")
	s.addFile("/repo/.gitignore", "*.log\n")
	s.addDir("/repo/src")
	s.addFile("/repo/src/a.log", "")
	s.addFile("/repo/src/b.txt", "")

	res, err := Walk(context.Background(), s, "/repo/src", Limits{Hidden: true, Gitignore: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := paths(res)
	want := []string{"f:b.txt"}
	if !slices.Equal(got, want) {
		t.Fatalf("walk = %v, want %v (the work-tree root's *.log reaches below /repo/src)", got, want)
	}
}

// TestWalkDeepUnderWorkTree: a walk three levels under the work-tree
// root: the root's .gitignore and .git/info/exclude apply to the
// listed tree, the intermediate .gitignore speaks first at its own
// level.
func TestWalkDeepUnderWorkTree(t *testing.T) {
	s := newStub()
	s.addDir("/wrk/.git")
	s.addFile("/wrk/.git/info/exclude", "secret.txt\n")
	s.addFile("/wrk/.gitignore", "*.log\n")
	s.addDir("/wrk/src")
	s.addDir("/wrk/src/deep")
	s.addFile("/wrk/src/.gitignore", "muted.txt\n")
	s.addFile("/wrk/src/deep/x.log", "x")      // the work-tree root's *.log
	s.addFile("/wrk/src/deep/muted.txt", "x")  // /wrk/src's .gitignore
	s.addFile("/wrk/src/deep/secret.txt", "x") // .git/info/exclude
	s.addFile("/wrk/src/deep/keep.txt", "x")

	res, err := Walk(context.Background(), s, "/wrk/src/deep", Limits{Hidden: true, Gitignore: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := paths(res)
	want := []string{"f:keep.txt"}
	if !slices.Equal(got, want) {
		t.Fatalf("walk = %v, want %v (all three ignore files reach below)", got, want)
	}
}

// TestWalkRootWorkTreeAnchoredOwnRules: a listing OF the work-tree
// root applies its own .gitignore's anchored (`/build`) and
// embedded-slash (`doc/frotz`) lines: the root's rules compile with
// the empty domain the work-tree root uses in git, so they anchor at
// the root they name.
func TestWalkRootWorkTreeAnchoredOwnRules(t *testing.T) {
	s := newStub()
	s.addDir("/.git")
	s.addFile("/.gitignore", "/build\ndoc/frotz\n")
	s.addDir("/build")
	s.addFile("/build/x", "")
	s.addDir("/sub/doc")
	s.addFile("/sub/doc/frotz", "") // anchored: outside /build, below /sub
	s.addDir("/doc")
	s.addFile("/doc/frotz", "")
	s.addFile("/doc/other", "")
	s.addFile("/keep.txt", "")

	res, err := Walk(context.Background(), s, "/", Limits{Hidden: true, Gitignore: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := paths(res)
	want := []string{"f:.gitignore", "d:doc", "f:doc/other", "f:keep.txt", "d:sub", "d:sub/doc", "f:sub/doc/frotz"}
	if !slices.Equal(got, want) {
		t.Fatalf("walk = %v, want %v (the root's /build and doc/frotz reach below it)", got, want)
	}
}

// TestWalkGitignoreWorkTreeRootOwnFilesAnchored: a listing OF the work-tree
// root applies the root's own .gitignore and .git/info/exclude relative
// to that root: their anchored (`/build`, `/secret/`) and
// embedded-slash (`doc/frotz`, `logs/debug`) lines match at the root,
// not below a nested directory of the same name.
func TestWalkGitignoreWorkTreeRootOwnFilesAnchored(t *testing.T) {
	s := newStub()
	s.addDir("/repo/.git")
	s.addFile("/repo/.gitignore", "/build\ndoc/frotz\n")
	s.addFile("/repo/.git/info/exclude", "/secret/\nlogs/debug\n")
	s.addDir("/repo/build")
	s.addFile("/repo/build/x", "")
	s.addDir("/repo/doc")
	s.addFile("/repo/doc/frotz", "")
	s.addFile("/repo/doc/other", "")
	s.addDir("/repo/sub/doc")
	s.addFile("/repo/sub/doc/frotz", "") // anchored: outside the root's doc
	s.addDir("/repo/secret")
	s.addFile("/repo/secret/x", "")
	s.addDir("/repo/logs")
	s.addDir("/repo/logs/debug")
	s.addFile("/repo/logs/debug/y", "")
	s.addDir("/repo/logs/keep")
	s.addFile("/repo/logs/keep/z", "")
	s.addFile("/repo/keep.txt", "")

	res, err := Walk(context.Background(), s, "/repo", Limits{Hidden: true, Gitignore: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := paths(res)
	want := []string{
		"f:.gitignore",
		"d:doc", "f:doc/other",
		"f:keep.txt",
		"d:logs", "d:logs/keep", "f:logs/keep/z",
		"d:sub", "d:sub/doc", "f:sub/doc/frotz",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("walk = %v, want %v (the root's own anchored rules reach below it)", got, want)
	}
}

// TestWalkListSymlinkToDirectoryNotFollowed: the fake's symlink form
// (a link to a directory) lists as a symlink and is never descended:
// the target's contents appear only under the real directory.
func TestWalkListSymlinkToDirectoryNotFollowed(t *testing.T) {
	f := fake.New()
	id := "i0123456789abcdefghij"
	ctx := context.Background()
	if _, err := f.Create(ctx, substrate.CreateRequest{SandboxID: id, TemplateID: "template0000000000000"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := f.MakeDir(ctx, id, "/repo/real", 0o755); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	if err := f.WriteFile(ctx, id, "/repo/real/x", []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := f.WriteSymlink(ctx, id, "/repo/real", "/repo/link"); err != nil {
		t.Fatalf("WriteSymlink: %v", err)
	}

	res, err := Walk(ctx, fakeFS{f: f, id: id}, "/repo", Limits{Hidden: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := paths(res)
	want := []string{"l:link", "d:real", "f:real/x"}
	if !slices.Equal(got, want) {
		t.Fatalf("walk = %v, want %v (a symlink to a directory is named, not followed)", got, want)
	}
}

// TestWalkAboveRootIgnoreStaysOut: a sibling tree's .gitignore (outside
// the walk root's ancestry and its work trees) never applies: rules
// only live inside a work tree, and /data's own .git marks that one.
func TestWalkAboveRootIgnoreStaysOut(t *testing.T) {
	s := newStub()
	s.addDir("/other")
	s.addFile("/other/.gitignore", "keep.txt\n")
	s.addDir("/data/.git")
	s.addFile("/data/.gitignore", "go.txt\n")
	s.addFile("/data/go.txt", "x")
	s.addFile("/data/keep.txt", "x")

	res, err := Walk(context.Background(), s, "/data", Limits{Hidden: true, Gitignore: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := paths(res)
	want := []string{"f:.gitignore", "f:keep.txt"}
	if !slices.Equal(got, want) {
		t.Fatalf("walk = %v, want %v (the other tree's rules stay out)", got, want)
	}
}

// TestWalkCrossFileNegationKeepsEntryIgnored: a deeper .gitignore's
// negation on an ANCESTOR directory does not re-include what lies
// below it: "*.pyc" at the root and "!lib" in sub/.gitignore leave
// sub/lib/x.pyc ignored, as git does. The entry's own path decides.
func TestWalkCrossFileNegationKeepsEntryIgnored(t *testing.T) {
	s := newStub()
	s.addDir("/.git")
	s.addFile("/.gitignore", "*.pyc\n")
	s.addDir("/sub")
	s.addFile("/sub/.gitignore", "!lib\n")
	s.addFile("/sub/lib/x.pyc", "")
	s.addFile("/sub/lib/y.py", "")
	s.addFile("/sub/keep.py", "")

	res, err := Walk(context.Background(), s, "/", Limits{Hidden: true, Gitignore: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := paths(res)
	want := []string{"f:.gitignore", "d:sub", "f:sub/.gitignore", "f:sub/keep.py", "d:sub/lib", "f:sub/lib/y.py"}
	if !slices.Equal(got, want) {
		t.Fatalf("walk = %v, want %v (sub/lib/x.pyc stays ignored)", got, want)
	}
}

// TestWalkTrailingGlobstarKeepsNegatedChild: "a/**" with "!a/keep"
// does not prune a: a/keep is listed, the rest of a is ignored.
func TestWalkTrailingGlobstarKeepsNegatedChild(t *testing.T) {
	s := newStub()
	s.addDir("/.git")
	s.addFile("/.gitignore", "a/**\n!a/keep\n")
	s.addDir("/a")
	s.addFile("/a/keep", "x")
	s.addFile("/a/other", "x")
	s.addFile("/a/deep/x", "x")

	res, err := Walk(context.Background(), s, "/", Limits{Hidden: true, Gitignore: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := paths(res)
	want := []string{"f:.gitignore", "d:a", "f:a/keep"}
	if !slices.Equal(got, want) {
		t.Fatalf("walk = %v, want %v (a is entered, a/keep re-included)", got, want)
	}
}

// TestWalkRootAncestorIgnored: a listing below an ignored directory is
// empty: the root's own ancestors are decided first, as git does. The
// root rules "a/" and "!a/b" ignore the ancestor a, so "!a/b" cannot
// re-include a/b (its parent is ignored).
func TestWalkRootAncestorIgnored(t *testing.T) {
	s := newStub()
	s.addDir("/repo/.git")
	s.addFile("/repo/.gitignore", "a/\n!a/b\n")
	s.addDir("/repo/a/b")
	s.addFile("/repo/a/b/x", "")
	s.addFile("/repo/a/b/y", "")

	res, err := Walk(context.Background(), s, "/repo/a/b", Limits{Hidden: true, Gitignore: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if got := paths(res); len(got) != 0 {
		t.Fatalf("walk = %v, want empty (the ancestor a is ignored)", got)
	}
	if res.Truncated {
		t.Fatal("an ignored root is not truncated")
	}
}

// TestWalkRootAncestorKeptWhenNotIgnored: a listing below an ancestor
// no rule names walks normally.
func TestWalkRootAncestorKeptWhenNotIgnored(t *testing.T) {
	s := newStub()
	s.addDir("/repo/.git")
	s.addFile("/repo/.gitignore", "a/*.log\n")
	s.addDir("/repo/a/b")
	s.addFile("/repo/a/b/x", "")

	res, err := Walk(context.Background(), s, "/repo/a/b", Limits{Hidden: true, Gitignore: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if got := paths(res); !slices.Equal(got, []string{"f:x"}) {
		t.Fatalf("walk = %v, want x (no rule names the ancestor)", got)
	}
}

// TestWalkNoAncestorReadsOutsideWorkTree: outside a git work tree no
// ancestor .gitignore is read: rules only live inside one.
func TestWalkNoAncestorReadsOutsideWorkTree(t *testing.T) {
	s := newStub()
	s.addDir("/parent/child")
	s.addFile("/parent/child/y", "")
	s.addFile("/parent/.gitignore", "y\n")
	s.addFile("/.gitignore", "x\n")
	var reads []string
	s.reads = &reads

	res, err := Walk(context.Background(), s, "/parent/child", Limits{Hidden: true, Gitignore: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if got := paths(res); !slices.Equal(got, []string{"f:y"}) {
		t.Fatalf("walk = %v, want y (no work tree, no ignore)", got)
	}
	for _, r := range reads {
		if strings.HasSuffix(r, "/.gitignore") && r != "parent/child/.gitignore" {
			t.Fatalf("read %q outside the walk root with no work tree", r)
		}
	}
}

// TestWalkIgnoreFileReadOnlyWhenListed: a directory's .gitignore is
// read only when its listing shows one.
func TestWalkIgnoreFileReadOnlyWhenListed(t *testing.T) {
	s := newStub()
	s.addDir("/.git")
	s.addFile("/a.txt", "")
	var reads []string
	s.reads = &reads

	if _, err := Walk(context.Background(), s, "/", Limits{Hidden: true, Gitignore: true}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	for _, r := range reads {
		if strings.HasSuffix(r, ".gitignore") {
			t.Fatalf("read %q even though no listing showed a .gitignore", r)
		}
	}
}

// TestWalkUnreadableBelowRootSkipped: a directory below the root that
// vanished or is unreadable (ErrNotFound, ErrPermission from List) is
// skipped, never fatal; its entry stays named.
func TestWalkUnreadableBelowRootSkipped(t *testing.T) {
	s := newStub()
	s.addDir("/gone")
	s.addFile("/gone/x", "")
	s.failDirs["gone"] = fmt.Errorf("list: %w", substrate.ErrNotFound)
	s.addDir("/locked")
	s.addFile("/locked/x", "")
	s.failDirs["locked"] = fmt.Errorf("list: %w", substrate.ErrPermission)
	s.addFile("/ok.txt", "")

	res, err := Walk(context.Background(), s, "/", Limits{Hidden: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := paths(res)
	if !slices.Equal(got, []string{"d:gone", "d:locked", "f:ok.txt"}) {
		t.Fatalf("walk = %v, want the unreadable dirs named, their contents skipped", got)
	}
}

// TestWalkUnreadableIgnoreFileSkipped: an unreadable or vanished
// .gitignore below the root is skipped, never fatal.
func TestWalkUnreadableIgnoreFileSkipped(t *testing.T) {
	s := newStub()
	s.addDir("/repo/.git")
	s.addFile("/repo/.gitignore", "*.log\n")
	s.addDir("/repo/sub")
	s.addFile("/repo/sub/.gitignore", "x.txt\n")
	s.readErrs["repo/sub/.gitignore"] = fmt.Errorf("read: %w", substrate.ErrPermission)
	s.addFile("/repo/sub/a.log", "")
	s.addFile("/repo/sub/x.txt", "")
	s.addFile("/repo/sub/keep.txt", "")

	res, err := Walk(context.Background(), s, "/repo", Limits{Hidden: true, Gitignore: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := paths(res)
	want := []string{"f:.gitignore", "d:sub", "f:sub/.gitignore", "f:sub/keep.txt", "f:sub/x.txt"}
	if !slices.Equal(got, want) {
		t.Fatalf("walk = %v, want %v (the unreadable .gitignore contributes nothing)", got, want)
	}
}

// TestWalkTimeoutCancelsSlowList: the walk runs under the Limits
// timeout's context, so a single slow List call (a hung substrate) is
// cancelled and the walk answers truncated, not an error.
func TestWalkTimeoutCancelsSlowList(t *testing.T) {
	s := newStub()
	s.blockList = true
	res, err := Walk(context.Background(), s, "/", Limits{Hidden: true, Timeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("Walk: %v, want a truncated success", err)
	}
	if !res.Truncated || len(res.Entries) != 0 {
		t.Fatalf("truncated = %v entries = %d, want a truncated empty walk", res.Truncated, len(res.Entries))
	}
}

// TestJsonEscapeSize: the entry budget counts what encoding/json
// writes. Invalid UTF-8 and U+2028/U+2029 are six-byte escapes.
func TestJsonEscapeSize(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"abc", 3},
		{"é", 2},
		{"\u2028", 6},
		{"\u2029", 6},
		{"\xff", 6},
		{"\xff\xfe", 12},
		{"\xed\xa0\x80", 18}, // three invalid bytes, three escapes
		{"\"", 2},
		{"\n", 2},
		{"<", 6},
	}
	for _, c := range cases {
		if got := jsonEscapeSize(c.in); got != c.want {
			t.Errorf("jsonEscapeSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestWalkSubstrateSkipsUnreadableBelowRoot: through the real
// dirwalk.Substrate adapter, a directory below the root whose ListDir
// fails with ErrPermission is skipped, never fatal.
func TestWalkSubstrateSkipsUnreadableBelowRoot(t *testing.T) {
	f := fake.New()
	id := "i0123456789abcdefghij"
	ctx := context.Background()
	if _, err := f.Create(ctx, substrate.CreateRequest{SandboxID: id, TemplateID: "template0000000000000"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, p := range []string{"/repo/a/x", "/repo/b/y"} {
		if err := f.WriteFile(ctx, id, p, []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", p, err)
		}
	}
	// The first ListDir is the root's; the second (a, in byte order)
	// answers ErrPermission, the third (b) succeeds.
	f.FailCall("ListDir", 2, fmt.Errorf("locked: %w", substrate.ErrPermission))

	res, err := Walk(ctx, Substrate{Sub: f, ID: id}, "/repo", Limits{Hidden: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := paths(res)
	if !slices.Equal(got, []string{"d:a", "d:b", "f:b/y"}) {
		t.Fatalf("walk = %v, want a named but not read, b walked", got)
	}
}

// TestWalkSubstrateSkipsVanishedBelowRoot: a directory below the root
// that vanished (ErrNotFound) is skipped the same way.
func TestWalkSubstrateSkipsVanishedBelowRoot(t *testing.T) {
	f := fake.New()
	id := "i0123456789abcdefghij"
	ctx := context.Background()
	if _, err := f.Create(ctx, substrate.CreateRequest{SandboxID: id, TemplateID: "template0000000000000"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, p := range []string{"/repo/a/x", "/repo/keep/y"} {
		if err := f.WriteFile(ctx, id, p, []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", p, err)
		}
	}
	f.FailCall("ListDir", 2, fmt.Errorf("gone: %w", substrate.ErrNotFound))

	res, err := Walk(ctx, Substrate{Sub: f, ID: id}, "/repo", Limits{Hidden: true})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := paths(res)
	if !slices.Equal(got, []string{"d:a", "d:keep", "f:keep/y"}) {
		t.Fatalf("walk = %v, want a named but not read, keep walked", got)
	}
}

// TestSubstrateReadFileSkipsMissingAndUnreadable: the adapter turns a
// missing ignore file (and one a file sits in the way of, and an
// over-size one) into errIgnoreMissing; a permission refusal surfaces
// as the substrate error the walk skips.
func TestSubstrateReadFileSkipsMissingAndUnreadable(t *testing.T) {
	f := fake.New()
	id := "i0123456789abcdefghij"
	ctx := context.Background()
	if _, err := f.Create(ctx, substrate.CreateRequest{SandboxID: id, TemplateID: "template0000000000000"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := f.WriteFile(ctx, id, "/f.txt", []byte("xxxx"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sub := Substrate{Sub: f, ID: id}
	if _, err := sub.ReadFile(ctx, "/nope/.gitignore", 1<<10); !errors.Is(err, errIgnoreMissing) {
		t.Fatalf("missing ignore file: %v, want errIgnoreMissing", err)
	}
	if _, err := sub.ReadFile(ctx, "/f.txt/.gitignore", 1<<10); !errors.Is(err, errIgnoreMissing) {
		t.Fatalf("ignore file under a file: %v, want errIgnoreMissing", err)
	}
	if _, err := sub.ReadFile(ctx, "/f.txt", 2); !errors.Is(err, errIgnoreMissing) {
		t.Fatalf("over-size ignore file: %v, want errIgnoreMissing", err)
	}
	f.FailCall("ReadFile", 0, fmt.Errorf("locked: %w", substrate.ErrPermission))
	if _, err := sub.ReadFile(ctx, "/x/.gitignore", 1<<10); !errors.Is(err, substrate.ErrPermission) {
		t.Fatalf("unreadable ignore file: %v, want ErrPermission", err)
	}
}

// TestWalkIgnoredRootIsEmpty: listing an ignored directory itself is
// empty, as git sees it (it never descends into node_modules), the same
// as listing below it. gitignore=false still lists it.
func TestWalkIgnoredRootIsEmpty(t *testing.T) {
	s := newStub()
	s.addDir("/repo/.git")
	s.addFile("/repo/.gitignore", "node_modules/\n")
	s.addFile("/repo/node_modules/pkg/index.js", "")

	for _, root := range []string{"/repo/node_modules", "/repo/node_modules/pkg"} {
		res, err := Walk(context.Background(), s, root, Limits{Hidden: true, Gitignore: true})
		if err != nil {
			t.Fatalf("Walk %s: %v", root, err)
		}
		if got := paths(res); len(got) != 0 {
			t.Fatalf("walk %s = %v, want empty (ignored)", root, got)
		}
	}
	res, err := Walk(context.Background(), s, "/repo/node_modules", Limits{Hidden: true, Gitignore: false})
	if err != nil {
		t.Fatalf("Walk gitignore=false: %v", err)
	}
	if got := paths(res); !slices.Equal(got, []string{"d:pkg", "f:pkg/index.js"}) {
		t.Fatalf("walk gitignore=false = %v, want the whole tree", got)
	}
}

// TestWalkUnreadableAboveRoot: an unreadable ancestor .gitignore is "no
// rules there", not a failed listing.
func TestWalkUnreadableAboveRoot(t *testing.T) {
	s := newStub()
	s.addDir("/repo/.git")
	s.addFile("/repo/.gitignore", "*.log\n")
	s.addFile("/repo/sub/a.txt", "")
	s.readErrs["repo/.gitignore"] = substrate.ErrPermission

	res, err := Walk(context.Background(), s, "/repo/sub", Limits{Hidden: true, Gitignore: true})
	if err != nil {
		t.Fatalf("Walk: %v (an unreadable ancestor .gitignore must not fail the listing)", err)
	}
	if got := paths(res); !slices.Equal(got, []string{"f:a.txt"}) {
		t.Fatalf("walk = %v, want [f:a.txt]", got)
	}
}
