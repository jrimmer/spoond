package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/jrimmer/spoond/v3/substrate"
)

// Lease file operations (#114): the API surface of the substrate file
// methods (WriteFile, ReadFile, Stat, MakeDir, Remove). Every route is
// GET/PUT/POST/DELETE /api/{leases,sandboxes}/{id}/files/{path...}; the
// tail is the guest-absolute path. Access follows the other lease
// routes: the owner (or an admin, which 404s the same way and may act
// on any lease) and — deliberately unlike exec/proxy — no http share:
// file content is a wider surface than exec, so a share grant does not
// silently grow into a filesystem read/write.

// maxFileBytes is the request- and response-size cap of the file routes:
// PUT refuses a bigger body with 413 before anything is written, and GET
// refuses to read a bigger file with 413. 256 MiB. A variable only so
// tests can lower it: at the real size, the race detector's shadow
// memory turns the cap test's buffers into several GiB.
var maxFileBytes int64 = 256 << 20

// maxFileTransfers caps file-content transfers (GET content, PUT) in
// flight across the backend, so buffered bodies stay under 1 GiB; past
// it the routes answer 429.
const maxFileTransfers = 4

// acquireXfer reserves a transfer slot, answering 429 when none is free.
func (s *Server) acquireXfer(w http.ResponseWriter) bool {
	select {
	case s.fileXfer <- struct{}{}:
		return true
	default:
		writeErrorCode(w, http.StatusTooManyRequests, "too_busy", "too many file transfers in flight; try again shortly")
		return false
	}
}

func (s *Server) releaseXfer() { <-s.fileXfer }

// filesTarget resolves the caller's access to the lease behind a files
// route: owner or admin, nil (and the caller answers 404) otherwise.
func (s *Server) filesTarget(r *http.Request) *Lease {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	lease := s.svc.lookup(owner, id)
	if lease == nil && isAdmin(r) {
		lease = s.svc.lookupAny(id)
	}
	return lease
}

// filePathValue extracts and validates the {path...} tail: it must be
// present and clean to a guest-absolute path that stays under /.
func filePathValue(r *http.Request) (string, bool) {
	tail := r.PathValue("path")
	if tail == "" {
		return "", false
	}
	cleaned := path.Clean("/" + tail)
	if cleaned == "/" {
		// The tail was empty or dot-dot: it names no file.
		return "", false
	}
	return cleaned, true
}

// filesInvalidPath answers a missing or root-cleaning {path} tail with
// the route's 404 and code EINVAL; the status keeps the old contract.
func filesInvalidPath(w http.ResponseWriter) {
	writeErrorCode(w, http.StatusNotFound, "EINVAL", "file path required")
}

// parseFileMode reads an octal ?mode= parameter ("0644", "644"), falling
// back to def when absent; a malformed value, or one with setuid, setgid
// or sticky bits (the substrate applies permission bits only), is an
// error.
func parseFileMode(raw string, def os.FileMode) (os.FileMode, error) {
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseUint(raw, 8, 32)
	if err != nil || v > 0o777 {
		return 0, errors.New("mode must be octal permission bits, 0000-0777")
	}
	return os.FileMode(v), nil
}

// filesInvalidModeOrOp answers a malformed ?mode or an unknown ?op with
// the routes' 400 and code EINVAL.
func filesInvalidModeOrOp(w http.ResponseWriter, msg string) {
	writeErrorCode(w, http.StatusBadRequest, "EINVAL", msg)
}

// mapFileError translates a substrate file error onto the HTTP status
// the route contract names. A substrate that could not confirm the
// sandbox's state (the orchestrator List failed) is 503 with Retry-After
// and code substrate_unavailable, never a not-found: the lease is kept
// and the caller retries.
func (s *Server) mapFileError(w http.ResponseWriter, lease *Lease, op string, err error) {
	sandboxID := lease.SandboxID
	if errors.Is(err, substrate.ErrUnavailable) {
		s.writeSubstrateUnavailableFor(w, lease, "files "+op)
		return
	}
	s.svc.store.mu.Lock()
	busy := lease.busy
	s.svc.store.mu.Unlock()
	switch {
	case errors.Is(err, substrate.ErrNotFound) && busy:
		// Mid-checkpoint the whole sandbox reads as missing: busy, not a
		// missing file (see writeSandboxGone).
		w.Header().Set("Retry-After", "5")
		writeErrorCode(w, http.StatusConflict, "lease_busy", "lease is busy (a checkpoint or another lifecycle operation is in progress); retry shortly")
	case errors.Is(err, substrate.ErrNotFound):
		writeErrorCode(w, http.StatusNotFound, "ENOENT", "file not found")
	case errors.Is(err, substrate.ErrTooLarge):
		writeErrorCode(w, http.StatusRequestEntityTooLarge, "EFBIG", "file exceeds the 256 MiB limit")
	case errors.Is(err, substrate.ErrPermission):
		writeErrorCode(w, http.StatusForbidden, "EACCES", err.Error())
	case errors.Is(err, substrate.ErrIsDir):
		writeErrorCode(w, http.StatusConflict, "EISDIR", err.Error())
	case errors.Is(err, substrate.ErrNotDir),
		errors.Is(err, substrate.ErrNotEmpty),
		errors.Is(err, substrate.ErrExist),
		errors.Is(err, substrate.ErrInvalidOp):
		writeErrorCode(w, http.StatusConflict, fileErrCode(err), err.Error())
	default:
		if errors.Is(err, substrate.ErrCapacity) {
			writeErrorCodeAfter(w, http.StatusServiceUnavailable, capacityRetryAfterSecs, "capacity_wait", "capacity: node is not accepting work")
			return
		}
		s.svc.log.Printf("files: %s %s: %v", op, sandboxID, err)
		writeErrorCode(w, http.StatusBadGateway, "EIO", "file operation failed")
	}
}

// fileErrCode names the code for the substrate's conflict-class file
// errors: ENOTDIR (a file sits in a path's way), ENOTEMPTY, EEXIST (the
// wrong type exists at the path) and EINVAL (an operation the substrate
// cannot perform, e.g. a write over a directory).
func fileErrCode(err error) string {
	switch {
	case errors.Is(err, substrate.ErrNotDir):
		return "ENOTDIR"
	case errors.Is(err, substrate.ErrNotEmpty):
		return "ENOTEMPTY"
	case errors.Is(err, substrate.ErrExist):
		return "EEXIST"
	default:
		return "EINVAL"
	}
}

// filesGate is the common path of every files route: resolve the lease
// (404 for anyone but the owner or an admin), resume one suspended by
// idle_suspend (2.5, #129 part 2) — otherwise a suspended lease has no
// running sandbox to touch, 409 — refuse a lost one (410 lease_lost),
// and count the call as activity for the idle sweep.
func (s *Server) filesGate(w http.ResponseWriter, r *http.Request) *Lease {
	lease := s.filesTarget(r)
	if lease == nil {
		writeErrorCode(w, http.StatusNotFound, "not_found", "lease not found")
		return nil
	}
	s.svc.touch(lease.ID) // files traffic is activity for the idle sweeper
	if !s.ensureRunning(w, r, lease) {
		return nil
	}
	if !s.ensureLive(w, lease) {
		return nil
	}
	return lease
}

// fileRangeReader is the optional substrate extension the file download
// uses to read one byte range without fetching everything before it. The
// e2b and fake substrates implement it.
type fileRangeReader interface {
	ReadFileRange(ctx context.Context, sandboxID, path string, offset, limit int64) ([]byte, error)
}

// handleFileDownload answers GET .../files/{path}: the file's bytes as
// application/octet-stream. ?stat=1 returns the metadata document
// instead. A single standard Range header is served as 206 with
// Content-Range; a range past the end is 416. Multi-range requests (and
// malformed ones) are answered with the whole file, as HTTP allows. 404
// when the path does not exist, 413 beyond 256 MiB.
func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Accept-Ranges", "bytes")
	lease := s.filesGate(w, r)
	if lease == nil {
		return
	}
	guestPath, ok := filePathValue(r)
	if !ok {
		filesInvalidPath(w)
		return
	}
	if r.URL.Query().Get("stat") == "1" {
		info, err := s.svc.sub.Stat(r.Context(), lease.SandboxID, guestPath)
		if err != nil {
			s.mapFileError(w, lease, "stat", err)
			return
		}
		writeJSON(w, http.StatusOK, fileInfoView(info))
		return
	}
	if !s.acquireXfer(w) {
		return
	}
	defer s.releaseXfer()
	rng, ranged := parseByteRange(r.Header.Get("Range"))
	if ranged {
		s.serveFileRange(w, r, lease, guestPath, rng)
		return
	}
	data, err := s.svc.sub.ReadFile(r.Context(), lease.SandboxID, guestPath, maxFileBytes)
	if err != nil {
		s.mapFileError(w, lease, "read", err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// byteRange is one parsed Range header: the forms bytes=a-b, bytes=a-
// and bytes=-n. first is -1 for a suffix range; last is -1 for an open
// one.
type byteRange struct {
	first, last int64
}

// parseByteRange parses a single standard byte Range header. ok is false
// when there is no header, it names another unit, it is malformed, or it
// asks for several ranges: the caller then serves the whole file, which
// HTTP permits.
func parseByteRange(header string) (byteRange, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return byteRange{}, false
	}
	unit, spec, found := strings.Cut(header, "=")
	if !found || !strings.EqualFold(strings.TrimSpace(unit), "bytes") {
		return byteRange{}, false
	}
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.Contains(spec, ",") {
		return byteRange{}, false
	}
	if strings.HasPrefix(spec, "-") {
		n, err := strconv.ParseInt(strings.TrimSpace(spec[1:]), 10, 64)
		if err != nil || n < 0 {
			return byteRange{}, false
		}
		return byteRange{first: -1, last: n}, true
	}
	firstStr, lastStr, found := strings.Cut(spec, "-")
	if !found {
		return byteRange{}, false
	}
	first, err := strconv.ParseInt(strings.TrimSpace(firstStr), 10, 64)
	if err != nil || first < 0 {
		return byteRange{}, false
	}
	last := int64(-1)
	if lastStr = strings.TrimSpace(lastStr); lastStr != "" {
		last, err = strconv.ParseInt(lastStr, 10, 64)
		if err != nil || last < first {
			return byteRange{}, false
		}
	}
	return byteRange{first: first, last: last}, true
}

// resolveRange turns a parsed range into an absolute, inclusive
// [start, end] for a file of size bytes. satisfiable is false for a
// range that begins at or past the end (416).
func (br byteRange) resolve(size int64) (start, end int64, satisfiable bool) {
	if br.first < 0 {
		// Suffix range: the last n bytes. n == 0 names no byte.
		if br.last <= 0 || size == 0 {
			return 0, 0, false
		}
		start = size - br.last
		if start < 0 {
			start = 0
		}
		return start, size - 1, true
	}
	if br.first >= size {
		return 0, 0, false
	}
	end = size - 1
	if br.last >= 0 && br.last < end {
		end = br.last
	}
	return br.first, end, true
}

// serveFileRange reads only the requested byte range and answers 206
// with Content-Range. It stats the file first for its size, which the
// Content-Range and a suffix range both need.
func (s *Server) serveFileRange(w http.ResponseWriter, r *http.Request, lease *Lease, guestPath string, br byteRange) {
	info, err := s.svc.sub.Stat(r.Context(), lease.SandboxID, guestPath)
	if err != nil {
		s.mapFileError(w, lease, "stat", err)
		return
	}
	if info.IsDir {
		writeErrorCode(w, http.StatusConflict, "EISDIR", guestPath+" is a directory")
		return
	}
	start, end, ok := br.resolve(info.Size)
	if !ok {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(info.Size, 10))
		writeErrorCode(w, http.StatusRequestedRangeNotSatisfiable, "EINVAL", "range not satisfiable")
		return
	}
	// One answer carries at most maxFileBytes: a larger range is served
	// short, and Content-Range says so (HTTP lets a client ask again
	// from where the answer stopped).
	if end-start+1 > maxFileBytes {
		end = start + maxFileBytes - 1
	}
	length := end - start + 1
	var data []byte
	if rr, ok := s.svc.sub.(fileRangeReader); ok {
		data, err = rr.ReadFileRange(r.Context(), lease.SandboxID, guestPath, start, length)
	} else {
		// A substrate without the optional range read: fetch the whole
		// file and slice, still capped at maxFileBytes like a plain GET.
		data, err = s.svc.sub.ReadFile(r.Context(), lease.SandboxID, guestPath, maxFileBytes)
		if err == nil {
			if start >= int64(len(data)) {
				data = nil
			} else {
				data = data[start:]
				if int64(len(data)) > length {
					data = data[:length]
				}
			}
		}
	}
	if err != nil {
		s.mapFileError(w, lease, "read", err)
		return
	}
	if len(data) == 0 {
		// The file shrank under the range between the stat and the read.
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(info.Size, 10))
		writeErrorCode(w, http.StatusRequestedRangeNotSatisfiable, "EINVAL", "range not satisfiable")
		return
	}
	// Content-Range names the bytes actually read, which a file that
	// shrank since the stat can make fewer than asked.
	end = start + int64(len(data)) - 1
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, info.Size))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(data)
}

// fileInfoView is the ?stat=1 JSON shape.
func fileInfoView(info substrate.FileInfo) map[string]any {
	return map[string]any{
		"name":     info.Name,
		"size":     info.Size,
		"mode":     strconv.FormatUint(uint64(info.Mode.Perm()), 8),
		"mod_time": info.ModTime.UTC().Format(time.RFC3339Nano),
		"is_dir":   info.IsDir,
	}
}

// handleFileUpload answers PUT .../files/{path}: the request body becomes
// the file's content (parents created; replaced when it exists). The mode
// comes from ?mode=0644 (octal, default 0644). A body beyond 256 MiB is
// refused with 413 before anything is written.
func (s *Server) handleFileUpload(w http.ResponseWriter, r *http.Request) {
	lease := s.filesGate(w, r)
	if lease == nil {
		return
	}
	guestPath, ok := filePathValue(r)
	if !ok {
		filesInvalidPath(w)
		return
	}
	mode, err := parseFileMode(r.URL.Query().Get("mode"), 0o644)
	if err != nil {
		filesInvalidModeOrOp(w, err.Error())
		return
	}
	if !s.acquireXfer(w) {
		return
	}
	defer s.releaseXfer()
	data, ok := readBodyCapped(w, r, maxFileBytes)
	if !ok {
		return
	}
	if err := s.svc.sub.WriteFile(r.Context(), lease.SandboxID, guestPath, data, mode); err != nil {
		s.mapFileError(w, lease, "write", err)
		return
	}
	writeJSON(w, http.StatusCreated, fileInfoView(substrate.FileInfo{
		Name: path.Base(guestPath), Size: int64(len(data)), Mode: mode, ModTime: time.Now(),
	}))
}

// readBodyCapped reads the request body, refusing anything beyond max
// bytes with 413 EFBIG before the caller touches the substrate; any other
// read failure is 400 EIO (the body's bytes could not be read, a
// transport failure, not a bad value).
func readBodyCapped(w http.ResponseWriter, r *http.Request, max int64) ([]byte, bool) {
	data, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		writeErrorCode(w, http.StatusBadRequest, "EIO", "reading request body: "+err.Error())
		return nil, false
	}
	if int64(len(data)) > max {
		writeErrorCode(w, http.StatusRequestEntityTooLarge, "EFBIG", "request body exceeds the 256 MiB limit")
		return nil, false
	}
	return data, true
}

// handleFileOp answers POST .../files/{path}?op=mkdir: create the
// directory (and missing parents). The mode comes from ?mode=0755
// (octal, default 0755).
func (s *Server) handleFileOp(w http.ResponseWriter, r *http.Request) {
	lease := s.filesGate(w, r)
	if lease == nil {
		return
	}
	guestPath, ok := filePathValue(r)
	if !ok {
		filesInvalidPath(w)
		return
	}
	if r.URL.Query().Get("op") != "mkdir" {
		filesInvalidModeOrOp(w, "op must be mkdir")
		return
	}
	mode, err := parseFileMode(r.URL.Query().Get("mode"), 0o755)
	if err != nil {
		filesInvalidModeOrOp(w, err.Error())
		return
	}
	if err := s.svc.sub.MakeDir(r.Context(), lease.SandboxID, guestPath, mode); err != nil {
		s.mapFileError(w, lease, "mkdir", err)
		return
	}
	info, err := s.svc.sub.Stat(r.Context(), lease.SandboxID, guestPath)
	if err != nil {
		writeJSON(w, http.StatusCreated, map[string]any{"created": true, "path": guestPath})
		return
	}
	writeJSON(w, http.StatusCreated, fileInfoView(info))
}

// handleFileDelete answers DELETE .../files/{path}: remove the file or —
// with ?recursive=1 — the directory and everything under it.
func (s *Server) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	lease := s.filesGate(w, r)
	if lease == nil {
		return
	}
	guestPath, ok := filePathValue(r)
	if !ok {
		filesInvalidPath(w)
		return
	}
	recursive := r.URL.Query().Get("recursive") == "1"
	if err := s.svc.sub.Remove(r.Context(), lease.SandboxID, guestPath, recursive); err != nil {
		s.mapFileError(w, lease, "remove", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
