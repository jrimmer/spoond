//go:build conformance

package conformance

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Group B: the bounded directory listing (?list=1, spoond-bvmc). The
// live behaviour a fake tree cannot prove: a repo whose node_modules
// holds a wide tree lists in one answer, the ignored directory pruned
// without ever being walked (its depth would blow the answer's bounds
// otherwise).

// fileListDoc is the ?list=1 response shape.
type fileListDoc struct {
	Entries   []fileListEntry `json:"entries"`
	Count     int             `json:"count"`
	Truncated bool            `json:"truncated"`
}

type fileListEntry struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

// listRows renders the entries as "kind:path" rows.
func (d fileListDoc) listRows() []string {
	out := make([]string, 0, len(d.Entries))
	for _, e := range d.Entries {
		out = append(out, e.Kind+":"+e.Path)
	}
	return out
}

// TestB1_ListRepoWithoutWalkingNodeModules: a repo with a .gitignore
// naming node_modules/ lists only its own files, however deep the
// ignored tree sits; ?list=1 refuses a file path with 409 ENOTDIR and a
// missing one with 404 ENOENT.
func TestB1_ListRepoWithoutWalkingNodeModules(t *testing.T) {
	begin(t)

	l := createLease(t, map[string]any{"image": "py-base", "ttl": 600})
	repo := "/root/conf-list-repo"

	// Build the repo in the guest: a .git dir, a .gitignore, tracked
	// files, and a node_modules that is wide on purpose (a walk of it
	// would list thousands of entries and blow any small bound).
	script := fmt.Sprintf(`
set -e
mkdir -p -- %[1]s/.git/objects
: > %[1]s/.git/HEAD
printf 'node_modules/\n*.log\n' > %[1]s/.gitignore
printf 'app\n' > %[1]s/app.py
printf 'x\n' > %[1]s/keep.log.disabled
rm -f %[1]s/keep.log.disabled
mkdir -p -- %[1]s/node_modules/%[2]s
for i in $(seq 1 40); do
  mkdir -p -- "%[1]s/node_modules/%[2]s/left-$i"
  printf 'x' > "%[1]s/node_modules/%[2]s/left-$i/index.js"
  mkdir -p -- "%[1]s/node_modules/%[2]s/right-$i"
  printf 'x' > "%[1]s/node_modules/%[2]s/right-$i/index.js"
done
`, shQuote(repo), "pkg")
	if got := execOK(t, l.ID, script); got != "" {
		failf(t, "repo build: %q", got)
	}

	st, data, err := cl.fileGet(l.ID, repo, "list=1")
	if err != nil {
		failf(t, "fileGet list: %v", err)
	}
	if st != 200 {
		failf(t, "list: status %d: %s", st, truncate(data))
	}
	var doc fileListDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		failf(t, "list: bad body %q: %v", truncate(data), err)
	}
	rows := doc.listRows()
	for _, row := range rows {
		if strings.Contains(row, "node_modules") {
			failf(t, "an ignored directory was listed or walked: %q (rows %v)", row, rows)
		}
		if strings.HasSuffix(row, ".log") {
			failf(t, "a *.log entry was listed: %q", row)
		}
	}
	// The listed tree: the .gitignore itself, the tracked file, and
	// nothing else (.git is always skipped).
	if len(rows) == 0 {
		failf(t, "list gave no entries")
	}
	for _, want := range []string{"f:.gitignore", "f:app.py"} {
		found := false
		for _, row := range rows {
			if strings.HasSuffix(row, want) {
				found = true
			}
		}
		if !found {
			failf(t, "rows %v miss %q", rows, want)
		}
	}
	if doc.Truncated {
		failf(t, "a small repo lists untruncated")
	}
	if doc.Count != len(doc.Entries) {
		failf(t, "count %d does not match %d entries", doc.Count, len(doc.Entries))
	}

	// A listing of a file answers 409 with code ENOTDIR.
	st, data, err = cl.fileGet(l.ID, repo+"/app.py", "list=1")
	if err != nil {
		failf(t, "list on a file: %v", err)
	}
	if st != 409 || !strings.Contains(string(data), "ENOTDIR") {
		failf(t, "list on a file: %d %s, want 409 ENOTDIR", st, truncate(data))
	}

	// A listing of a missing path answers 404 with code ENOENT.
	st, data, err = cl.fileGet(l.ID, repo+"/no/such", "list=1")
	if err != nil {
		failf(t, "list missing: %v", err)
	}
	if st != 404 || !strings.Contains(string(data), "ENOENT") {
		failf(t, "list missing: %d %s, want 404 ENOENT", st, truncate(data))
	}

	// Bounds work: one entry only, truncated.
	st, data, err = cl.fileGet(l.ID, repo, "list=1&max_entries=1")
	if err != nil {
		failf(t, "list max_entries: %v", err)
	}
	if st != 200 {
		failf(t, "list max_entries: %d %s", st, truncate(data))
	}
	var bounded fileListDoc
	if err := json.Unmarshal(data, &bounded); err != nil {
		failf(t, "list max_entries: bad body: %v", err)
	}
	if len(bounded.Entries) != 1 || !bounded.Truncated {
		failf(t, "list max_entries=1: %d entries truncated=%v, want 1 and true", len(bounded.Entries), bounded.Truncated)
	}

	// gitignore=0 lists the ignored entries (except .git, always
	// skipped; the listing is truncated by the module tree's width).
	st, data, err = cl.fileGet(l.ID, repo, "list=1&gitignore=0&max_entries=5")
	if err != nil {
		failf(t, "list gitignore=0: %v", err)
	}
	if st != 200 {
		failf(t, "list gitignore=0: %d %s", st, truncate(data))
	}
	var unign fileListDoc
	if err := json.Unmarshal(data, &unign); err != nil {
		failf(t, "list gitignore=0: bad body: %v", err)
	}
	if !unign.Truncated {
		failf(t, "listing node_modules with five entries must truncate")
	}
	if !unign.containsRow("d:node_modules") {
		failf(t, "gitignore=0 rows %v miss node_modules", unign.listRows())
	}
}

// containsRow reports whether a row is present.
func (d fileListDoc) containsRow(row string) bool {
	for _, r := range d.listRows() {
		if r == row {
			return true
		}
	}
	return false
}
