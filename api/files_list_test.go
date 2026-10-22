package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jrimmer/spoond/v4/substrate"
	"github.com/jrimmer/spoond/v4/substrate/fake"
)

// listFile GETs ?list=1 and decodes the document.
func listFile(t *testing.T, ts *httptest.Server, token, id, p, query string) (int, map[string]any) {
	t.Helper()
	u := filesURL(ts, id, p, "list=1")
	if query != "" {
		u += "&" + query
	}
	resp := filesDo(t, "GET", u, token, "")
	defer resp.Body.Close()
	doc := map[string]any{}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil && err != io.EOF {
		t.Fatalf("decode list: %v", err)
	}
	return resp.StatusCode, doc
}

// listRows renders the document's entries as "kind:path" rows.
func listRows(doc map[string]any) []string {
	entries, _ := doc["entries"].([]any)
	var out []string
	for _, e := range entries {
		m, _ := e.(map[string]any)
		out = append(out, m["kind"].(string)+":"+m["path"].(string))
	}
	return out
}

// seedTree writes a fake tree through the substrate; a value of fileDir
// marks a directory.
const fileDir = "\x00dir"

func seedTree(t *testing.T, svc *Service, sbxID string, tree map[string]string) {
	t.Helper()
	for p, content := range tree {
		if content == fileDir {
			if err := svc.sub.MakeDir(t.Context(), sbxID, p, 0o755); err != nil {
				t.Fatalf("MakeDir %s: %v", p, err)
			}
			continue
		}
		if err := svc.sub.WriteFile(t.Context(), sbxID, p, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", p, err)
		}
	}
}

// listSandbox returns the sandbox id and the fake behind the lease.
func listSandbox(t *testing.T, svc *Service, leaseID string) (string, *fake.Fake) {
	t.Helper()
	svc.store.mu.Lock()
	sb := svc.store.leases[leaseID].SandboxID
	svc.store.mu.Unlock()
	return sb, svc.sub.(*testSub).Fake
}

func TestFilesListOrderAndKinds(t *testing.T) {
	ts, svc, _, id := filesSetup(t)
	sb, _ := listSandbox(t, svc, id)
	seedTree(t, svc, sb, map[string]string{
		"/data/b.txt":    "b",
		"/data/a.txt":    "a",
		"/data/sub":      fileDir,
		"/data/sub/deep": "d",
	})

	st, doc := listFile(t, ts, "token-a", id, "/data", "")
	if st != 200 {
		t.Fatalf("list: %d %v", st, doc)
	}
	want := []string{"f:a.txt", "f:b.txt", "d:sub", "f:sub/deep"}
	if got := listRows(doc); !slices.Equal(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	if doc["count"] != float64(4) || doc["truncated"] != false {
		t.Fatalf("count = %v truncated = %v", doc["count"], doc["truncated"])
	}
}

func TestFilesListHiddenAndExclude(t *testing.T) {
	ts, svc, _, id := filesSetup(t)
	sb, _ := listSandbox(t, svc, id)
	seedTree(t, svc, sb, map[string]string{
		"/p/.dot":      "x",
		"/p/.dotted":   fileDir,
		"/p/.dotted/x": "x",
		"/p/keepme":    "x",
		"/p/skipme":    fileDir,
		"/p/skipme/x":  "x",
		"/p/skipfile":  "x",
	})

	// hidden defaults true: dot-names listed.
	st, doc := listFile(t, ts, "token-a", id, "/p", "")
	if st != 200 {
		t.Fatalf("list: %d %v", st, doc)
	}
	got := listRows(doc)
	want := []string{"f:.dot", "d:.dotted", "f:.dotted/x", "f:keepme", "f:skipfile", "d:skipme", "f:skipme/x"}
	if !slices.Equal(got, want) {
		t.Fatalf("rows = %v, want %v (siblings in byte order)", got, want)
	}

	// hidden=0 hides dot-names (their content, too).
	st, doc = listFile(t, ts, "token-a", id, "/p", "hidden=0")
	if st != 200 {
		t.Fatalf("list hidden=0: %d %v", st, doc)
	}
	got = listRows(doc)
	if !slices.Equal(got, []string{"f:keepme", "f:skipfile", "d:skipme", "f:skipme/x"}) {
		t.Fatalf("rows = %v, want no dot-names with hidden=0", got)
	}

	// exclude skips the named directory with everything below it and a
	// named file, wherever they sit.
	st, doc = listFile(t, ts, "token-a", id, "/p", "hidden=0&exclude=skipme,skipfile")
	if st != 200 {
		t.Fatalf("list exclude: %d %v", st, doc)
	}
	if got := listRows(doc); !slices.Equal(got, []string{"f:keepme"}) {
		t.Fatalf("rows = %v, want only keepme once skipme and skipfile are excluded", got)
	}
}

func TestFilesListDefaultHidden(t *testing.T) {
	ts, svc, _, id := filesSetup(t)
	sb, _ := listSandbox(t, svc, id)
	seedTree(t, svc, sb, map[string]string{
		"/p/.hidden": "x",
		"/p/normal":  "x",
	})
	st, doc := listFile(t, ts, "token-a", id, "/p", "")
	if st != 200 {
		t.Fatalf("list: %d %v", st, doc)
	}
	if got := listRows(doc); !slices.Equal(got, []string{"f:.hidden", "f:normal"}) {
		t.Fatalf("rows = %v, want .hidden listed (hidden defaults true)", got)
	}
}

func TestFilesListGitignore(t *testing.T) {
	ts, svc, _, id := filesSetup(t)
	sb, _ := listSandbox(t, svc, id)
	seedTree(t, svc, sb, map[string]string{
		"/repo/.gitignore":        "ign.txt\n*.log\nnode_modules/\n",
		"/repo/ign.txt":           "x",
		"/repo/app.py":            "x",
		"/repo/err.log":           "x",
		"/repo/node_modules":      fileDir,
		"/repo/node_modules/p/i":  "x",
		"/repo/nested/.gitignore": "!nested.log\n",
		"/repo/nested/nested.log": "x",
		"/repo/nested/o.log":      "x",
	})
	// The .git directory makes the tree a work tree; the fake has no
	// mkdirall for ".git/HEAD" parents beyond .git, which exists.
	seedTree(t, svc, sb, map[string]string{
		"/repo/.git/HEAD": "ref",
		"/repo/.git":      fileDir,
	})

	// Default (gitignore=1): the ignored entries are gone; node_modules
	// is pruned without being walked.
	st, doc := listFile(t, ts, "token-a", id, "/repo", "")
	if st != 200 {
		t.Fatalf("list: %d %v", st, doc)
	}
	rows := listRows(doc)
	for _, row := range rows {
		if strings.Contains(row, "node_modules") || strings.Contains(row, "ign.txt") || strings.HasSuffix(row, ".log") && !strings.HasSuffix(row, "nested/nested.log") {
			t.Fatalf("row %q must be ignored/pruned", row)
		}
	}
	if slices.Equal(rows, []string{"f:.gitignore", "f:app.py", "d:nested", "f:nested/.gitignore", "f:nested/nested.log"}) {
		return
	}
	t.Fatalf("rows = %v, want the gitignore-applied walk", rows)
}

func TestFilesListGitignoreOff(t *testing.T) {
	ts, svc, _, id := filesSetup(t)
	sb, _ := listSandbox(t, svc, id)
	seedTree(t, svc, sb, map[string]string{
		"/repo/.git":       fileDir,
		"/repo/.git/HE":    "x",
		"/repo/.gitign":    "ignored-because-not-a-gitignore\n",
		"/repo/.gitignore": "x.txt\n",
		"/repo/x.txt":      "x",
		"/repo/other":      "x",
	})
	st, doc := listFile(t, ts, "token-a", id, "/repo", "gitignore=0")
	if st != 200 {
		t.Fatalf("list: %d %v", st, doc)
	}
	rows := listRows(doc)
	// gitignore=0 lists what the .gitignore would have ignored (x.txt);
	// .git is skipped regardless.
	for _, row := range rows {
		if row == "d:.git" || strings.HasPrefix(row, "d:.git/") {
			t.Fatalf("gitignore=0 still skips .git: %q", row)
		}
	}
	if !slices.Contains(rows, "f:x.txt") || !slices.Contains(rows, "f:.gitignore") {
		t.Fatalf("rows = %v, want x.txt and .gitignore listed with gitignore=0", rows)
	}
}

func TestFilesListGitignoreOffStillSkipsGit(t *testing.T) {
	ts, svc, _, id := filesSetup(t)
	sb, _ := listSandbox(t, svc, id)
	seedTree(t, svc, sb, map[string]string{
		"/repo/.git":      fileDir,
		"/repo/.git/HEAD": "x",
		"/repo/main.go":   "x",
	})
	st, doc := listFile(t, ts, "token-a", id, "/repo", "gitignore=0")
	if st != 200 {
		t.Fatalf("list: %d %v", st, doc)
	}
	if got := listRows(doc); !slices.Equal(got, []string{"f:main.go"}) {
		t.Fatalf("rows = %v, want main.go only (a .gitignore=0 list still skips .git)", got)
	}
}

func TestFilesListTruncationEntries(t *testing.T) {
	ts, svc, _, id := filesSetup(t)
	sb, _ := listSandbox(t, svc, id)
	tree := map[string]string{}
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		tree["/t/"+n+".txt"] = "x"
	}
	seedTree(t, svc, sb, tree)

	st, doc := listFile(t, ts, "token-a", id, "/t", "max_entries=3")
	if st != 200 {
		t.Fatalf("list: %d %v", st, doc)
	}
	if got := listRows(doc); len(got) != 3 || doc["count"] != float64(3) {
		t.Fatalf("rows = %v count = %v, want 3 entries", got, doc["count"])
	}
	if doc["truncated"] != true {
		t.Fatalf("truncated = %v, want true", doc["truncated"])
	}
	st, doc = listFile(t, ts, "token-a", id, "/t", "max_entries=5")
	if st != 200 || doc["truncated"] != false {
		t.Fatalf("max_entries=5 must fit: %v %v", doc["truncated"], doc)
	}
}

func TestFilesListTruncationBytes(t *testing.T) {
	ts, svc, _, id := filesSetup(t)
	sb, _ := listSandbox(t, svc, id)
	seedTree(t, svc, sb, map[string]string{
		"/t/short": "x",
		"/t/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.txt": "x",
	})
	// The long name sorts first. Its entry costs
	// len(`{"kind":"f","path":""}`)+len(path)+1 = 22+49+1 = 72 bytes;
	// the short one costs 22+5+1 = 28. A budget of 300 holds both
	// entries with room to spare: nothing truncates. (80 below holds
	// the long one and stops at the short.)
	st, doc := listFile(t, ts, "token-a", id, "/t", "max_bytes=300")
	if st != 200 {
		t.Fatalf("list: %d %v", st, doc)
	}
	if doc["truncated"] != false {
		t.Fatalf("300 bytes must fit both entries: %v", doc)
	}
	if rows := listRows(doc); len(rows) != 2 {
		t.Fatalf("rows = %v, want both entries in a 300-byte budget", rows)
	}
	st, doc = listFile(t, ts, "token-a", id, "/t", "max_bytes=80")
	if st != 200 {
		t.Fatalf("list: %d %v", st, doc)
	}
	if doc["truncated"] != true {
		t.Fatalf("80 bytes must truncate after the long name: %v", doc)
	}
	if rows := listRows(doc); len(rows) != 1 {
		t.Fatalf("rows = %v, want one entry in an 80-byte budget", rows)
	}
}

func TestFilesListTruncationTimeout(t *testing.T) {
	ts, svc, _, id := filesSetup(t)
	sb, _ := listSandbox(t, svc, id)
	tree := map[string]string{}
	for i := 0; i < 3000; i++ {
		tree["/deep/d"+string(rune('a'+i%26))+strconv.Itoa(i)] = fileDir
	}
	seedTree(t, svc, sb, tree)

	st, doc := listFile(t, ts, "token-a", id, "/deep", "timeout_ms=1")
	if st != 200 {
		t.Fatalf("list: %d %v", st, doc)
	}
	if doc["truncated"] != true {
		t.Fatalf("a 3000-dir tree cannot fully list in 1 ms: %v", doc["truncated"])
	}
}

func TestFilesListMissingAndFilePath(t *testing.T) {
	ts, svc, _, id := filesSetup(t)
	sb, f := listSandbox(t, svc, id)

	// A missing path is 404 with code ENOENT.
	u := filesURL(ts, id, "/no/such/dir", "list=1")
	resp := filesDo(t, "GET", u, "token-a", "")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("list missing: %d %s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), "ENOENT") {
		t.Fatalf("list missing body = %s, want code ENOENT", b)
	}

	// A file is 409 with code ENOTDIR.
	if err := f.WriteFile(t.Context(), sb, "/plain.txt", []byte("x"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	u2 := filesURL(ts, id, "/plain.txt", "list=1")
	resp2 := filesDo(t, "GET", u2, "token-a", "")
	b2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("list on a file: %d %s, want 409", resp2.StatusCode, b2)
	}
	if !strings.Contains(string(b2), "ENOTDIR") {
		t.Fatalf("list on a file body = %s, want code ENOTDIR", b2)
	}
}

// TestFilesListParamErrors: malformed bounds and booleans are 400. A
// zero, negative or over-large bound is not malformed: it clamps to the
// maximum (TestFilesListLimitsClamp).
func TestFilesListParamErrors(t *testing.T) {
	ts, _, _, id := filesSetup(t)
	for _, q := range []string{"hidden=x", "gitignore=x", "max_entries=x", "max_bytes=x", "timeout_ms=x"} {
		st, doc := listFile(t, ts, "token-a", id, "/d", q)
		if st != http.StatusBadRequest {
			t.Fatalf("%s: %d %v, want 400", q, st, doc)
		}
	}
}

// TestFilesListPlusStat: list=1 wins when stat=1 is set too.
func TestFilesListPlusStat(t *testing.T) {
	ts, svc, _, id := filesSetup(t)
	sb, _ := listSandbox(t, svc, id)
	seedTree(t, svc, sb, map[string]string{"/d": fileDir})
	st, doc := listFile(t, ts, "token-a", id, "/d", "stat=1")
	if st != http.StatusOK || doc["entries"] == nil {
		t.Fatalf("list=1 wins with stat=1 also set: %d %v", st, doc)
	}
}

// TestFilesListBusyLease: a busy lease whose sandbox reads as missing
// (a checkpoint holds the lease) is 409 busy, not 404 ENOENT.
func TestFilesListBusyLease(t *testing.T) {
	ts, svc, sub, id := filesSetup(t)
	l := svc.store.leases[id]
	l.busy = true
	sub.FailCall("ListDir", 0, substrate.ErrNotFound)
	u := filesURL(ts, id, "/d", "list=1")
	resp := filesDo(t, "GET", u, "token-a", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("list on a busy lease: %d, want 409 busy", resp.StatusCode)
	}
}

// TestFilesListScopesTreeIgnores: a walk rooted inside a work tree
// applies the work-tree root's .gitignore, and a sibling tree's
// .gitignore (outside the walk's work trees) stays inert.
func TestFilesListScopesTreeIgnores(t *testing.T) {
	ts, svc, _, id := filesSetup(t)
	sb, _ := listSandbox(t, svc, id)
	seedTree(t, svc, sb, map[string]string{
		"/other/.gitignore": "keepme\n",
		"/other/keepme":     "x",
		"/repo/.git":        fileDir,
		"/repo/.git/HEAD":   "ref",
		"/repo/.gitignore":  "*.log\n",
		"/repo/src/a.log":   "x",
		"/repo/src/b.txt":   "x",
	})

	// Inside the work tree: the root's *.log applies below /repo/src.
	st, doc := listFile(t, ts, "token-a", id, "/repo/src", "")
	if st != 200 {
		t.Fatalf("list: %d %v", st, doc)
	}
	if got := listRows(doc); !slices.Equal(got, []string{"f:b.txt"}) {
		t.Fatalf("rows = %v, want b.txt only (the work-tree root's *.log reaches below)", got)
	}

	// A listing of the sibling tree (no .git anywhere near it): its own
	// .gitignore is inert, and /repo's rules cannot reach it either.
	st, doc = listFile(t, ts, "token-a", id, "/other", "")
	if st != 200 {
		t.Fatalf("list other: %d %v", st, doc)
	}
	if got := listRows(doc); !slices.Equal(got, []string{"f:.gitignore", "f:keepme"}) {
		t.Fatalf("rows = %v, want both (no work tree, no ignore)", got)
	}
}

// TestFilesListGitignoreWorkTreeRootAnchored: a listing OF a work-tree
// root applies that root's .gitignore and .git/info/exclude relative to
// the root: anchored (/build, /secret/) and embedded-slash (doc/frotz,
// logs/debug) lines match at the root they name.
func TestFilesListGitignoreWorkTreeRootAnchored(t *testing.T) {
	ts, svc, _, id := filesSetup(t)
	sb, _ := listSandbox(t, svc, id)
	seedTree(t, svc, sb, map[string]string{
		"/repo/.git":              fileDir,
		"/repo/.git/HEAD":         "ref",
		"/repo/.git/info/exclude": "/secret/\nlogs/debug\n",
		"/repo/.gitignore":        "/build\ndoc/frotz\n",
		"/repo/build":             fileDir,
		"/repo/build/x":           "x",
		"/repo/doc/frotz":         "x",
		"/repo/doc/other":         "x",
		"/repo/logs/debug/y":      "x",
		"/repo/logs/keep/z":       "x",
		"/repo/secret/x":          "x",
		"/repo/sub/doc/frotz":     "x",
		"/repo/keep.txt":          "x",
	})

	st, doc := listFile(t, ts, "token-a", id, "/repo", "")
	if st != 200 {
		t.Fatalf("list: %d %v", st, doc)
	}
	want := []string{
		"f:.gitignore", "d:doc", "f:doc/other", "f:keep.txt",
		"d:logs", "d:logs/keep", "f:logs/keep/z",
		"d:sub", "d:sub/doc", "f:sub/doc/frotz",
	}
	if got := listRows(doc); !slices.Equal(got, want) {
		t.Fatalf("rows = %v, want %v (the root's own anchored rules reach below it)", got, want)
	}
}

// TestFilesListSymlinkToDirectory: a symlink to a directory lists as a
// symlink and is never descended; the target's contents appear only
// under the real directory.
func TestFilesListSymlinkToDirectory(t *testing.T) {
	ts, svc, _, id := filesSetup(t)
	sb, _ := listSandbox(t, svc, id)
	seedTree(t, svc, sb, map[string]string{
		"/p/real":   fileDir,
		"/p/real/x": "x",
	})
	if err := svc.sub.(*testSub).WriteSymlink(t.Context(), sb, "/p/real", "/p/link"); err != nil {
		t.Fatalf("WriteSymlink: %v", err)
	}
	st, doc := listFile(t, ts, "token-a", id, "/p", "")
	if st != 200 {
		t.Fatalf("list: %d %v", st, doc)
	}
	if got := listRows(doc); !slices.Equal(got, []string{"l:link", "d:real", "f:real/x"}) {
		t.Fatalf("rows = %v, want the link named, not followed", got)
	}
}

// TestFilesListLimitsClamp: max_entries, max_bytes and timeout_ms
// clamp to the maximum: zero, negative and larger values all mean the
// maximum (50000 entries, 4 MiB, 10 s), never "off" and never more.
func TestFilesListLimitsClamp(t *testing.T) {
	for _, raw := range []string{"0", "-5", "999999999"} {
		st, err := parseListSettings(url.Values{"max_entries": {raw}})
		if err != nil {
			t.Fatalf("max_entries=%s: %v", raw, err)
		}
		if st.limits.MaxEntries != defaultListMaxEntries {
			t.Fatalf("max_entries=%s clamped to %d, want %d", raw, st.limits.MaxEntries, defaultListMaxEntries)
		}
		st, err = parseListSettings(url.Values{"max_bytes": {raw}})
		if err != nil {
			t.Fatalf("max_bytes=%s: %v", raw, err)
		}
		if st.limits.MaxBytes != defaultListMaxBytes {
			t.Fatalf("max_bytes=%s clamped to %d, want %d", raw, st.limits.MaxBytes, defaultListMaxBytes)
		}
		st, err = parseListSettings(url.Values{"timeout_ms": {raw}})
		if err != nil {
			t.Fatalf("timeout_ms=%s: %v", raw, err)
		}
		if st.limits.Timeout != defaultListTimeout {
			t.Fatalf("timeout_ms=%s clamped to %v, want %v", raw, st.limits.Timeout, defaultListTimeout)
		}
	}
	// A value under the maximum is kept as asked.
	st, err := parseListSettings(url.Values{"max_entries": {"7"}, "max_bytes": {"99"}, "timeout_ms": {"250"}})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if st.limits.MaxEntries != 7 || st.limits.MaxBytes != 99 || st.limits.Timeout != 250*time.Millisecond {
		t.Fatalf("bounds = %+v, want the asked 7 / 99 / 250ms", st.limits)
	}
}

// TestFilesListClampedTimeoutStopsSlowWalk: a clamped timeout_ms still
// bounds the walk (a zero or negative timeout_ms is the 10 s maximum,
// not an unbounded walk), and a small timeout_ms truncates.
func TestFilesListClampedTimeoutStopsSlowWalk(t *testing.T) {
	ts, svc, _, id := filesSetup(t)
	sb, _ := listSandbox(t, svc, id)
	tree := map[string]string{}
	for i := 0; i < 3000; i++ {
		tree["/deep/d"+strconv.Itoa(i)] = fileDir
	}
	seedTree(t, svc, sb, tree)
	for _, q := range []string{"timeout_ms=1", "timeout_ms=0", "timeout_ms=-1"} {
		st, doc := listFile(t, ts, "token-a", id, "/deep", q)
		if st != http.StatusOK {
			t.Fatalf("%s: %d %v, want 200", q, st, doc)
		}
	}
}

// TestFilesListCrossFilePrecedence: a deeper .gitignore's negation on
// a directory does not re-include what lies below it ("*.pyc" at the
// root, "!lib" in sub): sub/lib/x.pyc stays ignored, as git does.
func TestFilesListCrossFilePrecedence(t *testing.T) {
	ts, svc, _, id := filesSetup(t)
	sb, _ := listSandbox(t, svc, id)
	seedTree(t, svc, sb, map[string]string{
		"/repo/.git":           fileDir,
		"/repo/.git/HEAD":      "ref",
		"/repo/.gitignore":     "*.pyc\n",
		"/repo/sub/.gitignore": "!lib\n",
		"/repo/sub/lib/x.pyc":  "x",
		"/repo/sub/lib/y.py":   "y",
	})
	st, doc := listFile(t, ts, "token-a", id, "/repo", "")
	if st != 200 {
		t.Fatalf("list: %d %v", st, doc)
	}
	want := []string{"f:.gitignore", "d:sub", "f:sub/.gitignore", "d:sub/lib", "f:sub/lib/y.py"}
	if got := listRows(doc); !slices.Equal(got, want) {
		t.Fatalf("rows = %v, want %v (sub/lib/x.pyc stays ignored)", got, want)
	}
}

// TestFilesListBelowIgnoredAncestor: a listing below an excluded
// directory is empty, as git ignores it: root rules "a/" and "!a/b"
// leave /repo/a/b unlisted.
func TestFilesListBelowIgnoredAncestor(t *testing.T) {
	ts, svc, _, id := filesSetup(t)
	sb, _ := listSandbox(t, svc, id)
	seedTree(t, svc, sb, map[string]string{
		"/repo/.git":       fileDir,
		"/repo/.git/HEAD":  "ref",
		"/repo/.gitignore": "a/\n!a/b\n",
		"/repo/a/b/x":      "x",
	})
	st, doc := listFile(t, ts, "token-a", id, "/repo/a/b", "")
	if st != 200 {
		t.Fatalf("list: %d %v", st, doc)
	}
	if got := listRows(doc); len(got) != 0 || doc["count"] != float64(0) {
		t.Fatalf("rows = %v, want empty (the ancestor a is ignored)", got)
	}
}

// TestFilesListTrailingGlobstar: "a/**" with "!a/keep" does not prune
// a; a/keep is listed, the rest of a is ignored.
func TestFilesListTrailingGlobstar(t *testing.T) {
	ts, svc, _, id := filesSetup(t)
	sb, _ := listSandbox(t, svc, id)
	seedTree(t, svc, sb, map[string]string{
		"/repo/.git":       fileDir,
		"/repo/.git/HEAD":  "ref",
		"/repo/.gitignore": "a/**\n!a/keep\n",
		"/repo/a/keep":     "k",
		"/repo/a/other":    "o",
	})
	st, doc := listFile(t, ts, "token-a", id, "/repo", "")
	if st != 200 {
		t.Fatalf("list: %d %v", st, doc)
	}
	want := []string{"f:.gitignore", "d:a", "f:a/keep"}
	if got := listRows(doc); !slices.Equal(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
}

// slowListSub delays every ListDir, so a test can pin that the walk's
// timeout reaches the substrate call itself.
type slowListSub struct {
	substrate.Substrate
	delay time.Duration
}

func (s slowListSub) ListDir(ctx context.Context, sandboxID, p string) ([]substrate.DirEntry, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.Substrate.ListDir(ctx, sandboxID, p)
}

// TestFilesListTimeoutStopsSlowListDir: one slow ListDir is cut off by
// the walk's timeout: the route answers 200 truncated, not a hang and
// not a 502.
func TestFilesListTimeoutStopsSlowListDir(t *testing.T) {
	ts, svc, _, id := filesSetup(t)
	sb, _ := listSandbox(t, svc, id)
	seedTree(t, svc, sb, map[string]string{"/d": fileDir})
	svc.sub = slowListSub{Substrate: svc.sub, delay: 2 * time.Second}

	start := time.Now()
	st, doc := listFile(t, ts, "token-a", id, "/d", "timeout_ms=50")
	if st != http.StatusOK {
		t.Fatalf("list: %d %v, want 200 truncated", st, doc)
	}
	if doc["truncated"] != true {
		t.Fatalf("truncated = %v, want true (the slow listing was cut off)", doc["truncated"])
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the request took %v: the timeout did not cancel the listing", elapsed)
	}
}
