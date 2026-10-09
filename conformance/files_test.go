//go:build conformance

package conformance

import (
	"encoding/json"
	"strings"
	"testing"
)

// Group F2: lease file operations over the API (#114). The routes speak
// for the substrate file methods: PUT writes the request body, GET with
// ?stat=1 returns metadata, plain GET downloads, POST ?op=mkdir creates
// directories, DELETE removes (recursive for directories). The suite
// writes, stats, reads and removes a file in a py-base lease; the guest
// itself (cat) confirms the bytes really landed in the microVM.

// fileStatDoc is the ?stat=1 response shape.
type fileStatDoc struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`
	ModTime string `json:"mod_time"`
	IsDir   bool   `json:"is_dir"`
}

// shQuote wraps s in single quotes, escaping embedded single quotes, so a
// path or marker survives the guest shell.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// TestF2_LeaseFileOperations exercises the file routes end to end: write
// through the API, read back through the API and through the guest, stat
// both a file and a directory, then remove and confirm the 404s.
func TestF2_LeaseFileOperations(t *testing.T) {
	begin(t)

	l := createLease(t, map[string]any{"image": "py-base", "ttl": 600})
	path := "/root/.conformance-files/marker.txt"

	// PUT writes the body; parents are created implicitly.
	body := "conformance " + randMarker()
	st, data, err := cl.filePut(l.ID, path, []byte(body), "mode=0640")
	if err != nil {
		failf(t, "filePut: %v", err)
	}
	if st != 201 {
		failf(t, "filePut: status %d: %s", st, truncate(data))
	}

	// GET downloads the bytes as application/octet-stream.
	st, data, err = cl.fileGet(l.ID, path, "")
	if err != nil {
		failf(t, "fileGet: %v", err)
	}
	if st != 200 {
		failf(t, "fileGet: status %d: %s", st, truncate(data))
	}
	if string(data) != body {
		failf(t, "fileGet: got %q, want %q", data, body)
	}

	// The guest sees the same bytes and the requested mode.
	if got := execOK(t, l.ID, "cat -- "+shQuote(path)); got != body {
		failf(t, "guest cat: got %q, want %q", got, body)
	}
	if mode := execOK(t, l.ID, "stat -c %a -- "+shQuote(path)); mode != "640" {
		failf(t, "guest stat mode: got %q, want 640", mode)
	}

	// ?stat=1 reports the metadata document.
	st, data, err = cl.fileGet(l.ID, path, "stat=1")
	if err != nil {
		failf(t, "stat: %v", err)
	}
	if st != 200 {
		failf(t, "stat: status %d: %s", st, truncate(data))
	}
	var info fileStatDoc
	if err := json.Unmarshal(data, &info); err != nil {
		failf(t, "stat: bad body %q: %v", truncate(data), err)
	}
	if info.Name != "marker.txt" || info.Size != int64(len(body)) || info.IsDir {
		failf(t, "stat doc: %+v", info)
	}
	if info.Mode != "640" {
		failf(t, "stat doc mode = %q, want 640", info.Mode)
	}
	if info.ModTime == "" {
		failf(t, "stat doc has no mod_time")
	}

	// A stat of a directory reports is_dir; mkdir made the parents.
	dir := "/root/.conformance-files/nested/deeper"
	st, data, err = cl.fileMkdir(l.ID, dir, "op=mkdir&mode=0750")
	if err != nil {
		failf(t, "mkdir: %v", err)
	}
	if st != 201 {
		failf(t, "mkdir: status %d: %s", st, truncate(data))
	}
	st, data, err = cl.fileGet(l.ID, dir, "stat=1")
	if err != nil {
		failf(t, "stat dir: %v", err)
	}
	if st != 200 {
		failf(t, "stat dir: status %d: %s", st, truncate(data))
	}
	var dinfo fileStatDoc
	if err := json.Unmarshal(data, &dinfo); err != nil {
		failf(t, "stat dir: bad body: %v", err)
	}
	if !dinfo.IsDir || dinfo.Mode != "750" {
		failf(t, "stat dir doc: %+v", dinfo)
	}

	// A missing path is 404.
	st, _, err = cl.fileGet(l.ID, "/root/.conformance-files/nope", "")
	if err != nil {
		failf(t, "get missing: %v", err)
	}
	if st != 404 {
		failf(t, "get missing: status %d, want 404", st)
	}

	// DELETE removes; a non-recursive remove of a non-empty directory
	// (nested, which holds deeper) is refused, of an empty one (deeper)
	// goes through, and the recursive one takes the rest.
	st, data, err = cl.fileDelete(l.ID, "/root/.conformance-files/nested", "")
	if err != nil {
		failf(t, "delete non-empty dir: %v", err)
	}
	if st != 409 {
		failf(t, "delete non-empty dir: status %d, want 409: %s", st, truncate(data))
	}
	st, data, err = cl.fileDelete(l.ID, dir, "")
	if err != nil {
		failf(t, "delete empty dir: %v", err)
	}
	if st != 204 {
		failf(t, "delete empty dir: status %d, want 204: %s", st, truncate(data))
	}
	st, data, err = cl.fileDelete(l.ID, path, "")
	if err != nil {
		failf(t, "delete file: %v", err)
	}
	if st != 204 {
		failf(t, "delete file: status %d: %s", st, truncate(data))
	}
	st, _, err = cl.fileGet(l.ID, path, "")
	if err != nil {
		failf(t, "get after delete: %v", err)
	}
	if st != 404 {
		failf(t, "get after delete: status %d, want 404", st)
	}
	st, data, err = cl.fileDelete(l.ID, "/root/.conformance-files", "recursive=1")
	if err != nil {
		failf(t, "recursive delete: %v", err)
	}
	if st != 204 {
		failf(t, "recursive delete: status %d: %s", st, truncate(data))
	}
	st, _, err = cl.fileGet(l.ID, "/root/.conformance-files", "stat=1")
	if err != nil {
		failf(t, "stat after recursive delete: %v", err)
	}
	if st != 404 {
		failf(t, "stat after recursive delete: status %d, want 404", st)
	}
}

// TestF2_LeaseFilesSuspendedRefused: a suspended lease resumes on the
// next file work call (2.9, #145 D2) and the call is served — the file
// written before the suspend is read back from the resumed guest. Only
// the paths that cannot resume keep 409 lease_suspended, so the stat
// probe still refuses while the lease is suspended.
func TestF2_LeaseFilesSuspendedRefused(t *testing.T) {
	begin(t)

	l := createLease(t, map[string]any{"image": "py-base", "ttl": 600, "persistent": true})
	path := "/root/.conformance-files-suspend.txt"
	body := "survives " + randMarker()
	st, data, err := cl.filePut(l.ID, path, []byte(body), "")
	if err != nil {
		failf(t, "filePut: %v", err)
	}
	if st != 201 {
		failf(t, "filePut: status %d: %s", st, truncate(data))
	}

	st, data, err = cl.suspend(l.ID)
	if err != nil {
		failf(t, "suspend: %v", err)
	}
	if st != 200 {
		failf(t, "suspend: status %d: %s", st, truncate(data))
	}

	// The stat probe needs a running guest and cannot resume one: it is
	// one of the few paths that still answers 409 lease_suspended.
	if st, data, err = cl.stat(l.ID); err != nil {
		failf(t, "stat on suspended: %v", err)
	} else {
		requireLeaseSuspended(t, "stat on suspended", st, data)
	}

	// Forking checkpoints the source guest, so it cannot resume either:
	// it still answers 409 lease_suspended while the lease is suspended.
	if st, data, err = cl.fork(l.ID, map[string]any{"count": 1}); err != nil {
		failf(t, "fork on suspended: %v", err)
	} else {
		requireLeaseSuspended(t, "fork on suspended", st, data)
	}

	// The file get is work: it resumes the lease and serves the bytes.
	st, data, err = cl.fileGet(l.ID, path, "")
	if err != nil {
		failf(t, "get on suspended: %v", err)
	}
	if st != 200 {
		failf(t, "get on suspended: status %d, want 200 (the call resumes the lease): %s", st, truncate(data))
	}
	if string(data) != body {
		failf(t, "get on suspended: body %q, want %q", data, body)
	}
	if leaseSuspended(t, l.ID) {
		failf(t, "lease %s still suspended after the file get", l.ID)
	}

	// The guest itself sees the file that survived the suspend/resume.
	if got := execOK(t, l.ID, "cat -- "+shQuote(path)); !strings.Contains(got, "survives") {
		failf(t, "guest cat after resume: got %q, want the marker", got)
	}
}
