package api

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"time"

	"github.com/jrimmer/spoond/v2/substrate"
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
		writeError(w, http.StatusTooManyRequests, "too many file transfers in flight; try again shortly")
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

// mapFileError translates a substrate file error onto the HTTP status
// the route contract names.
func (s *Server) mapFileError(w http.ResponseWriter, lease *Lease, op string, err error) {
	sandboxID := lease.SandboxID
	s.svc.store.mu.Lock()
	busy := lease.busy
	s.svc.store.mu.Unlock()
	switch {
	case errors.Is(err, substrate.ErrNotFound) && busy:
		// Mid-checkpoint the whole sandbox reads as missing: busy, not a
		// missing file (see writeSandboxGone).
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusConflict, "lease is busy (a checkpoint or another lifecycle operation is in progress); retry shortly")
	case errors.Is(err, substrate.ErrNotFound):
		writeError(w, http.StatusNotFound, "file not found")
	case errors.Is(err, substrate.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "file exceeds the 256 MiB limit")
	case errors.Is(err, substrate.ErrNotDir),
		errors.Is(err, substrate.ErrNotEmpty),
		errors.Is(err, substrate.ErrExist),
		errors.Is(err, substrate.ErrInvalidOp):
		writeError(w, http.StatusConflict, err.Error())
	default:
		if errors.Is(err, substrate.ErrCapacity) {
			writeError(w, http.StatusServiceUnavailable, "node is not accepting work")
			return
		}
		s.svc.log.Printf("files: %s %s: %v", op, sandboxID, err)
		writeError(w, http.StatusBadGateway, "file operation failed")
	}
}

// filesGate is the common path of every files route: resolve the lease
// (404 for anyone but the owner or an admin), resume one suspended by
// idle_suspend (2.5, #129 part 2) — otherwise a suspended lease has no
// running sandbox to touch, 409 — refuse a lost one (410), and count the
// call as activity for the idle sweep.
func (s *Server) filesGate(w http.ResponseWriter, r *http.Request) *Lease {
	lease := s.filesTarget(r)
	if lease == nil {
		writeError(w, http.StatusNotFound, "lease not found")
		return nil
	}
	s.svc.touch(lease.ID) // files traffic is activity for the idle sweeper
	if !s.ensureRunning(w, r, lease) {
		return nil
	}
	if lease.State == "lost" {
		writeError(w, http.StatusGone, lostLeaseMessage)
		return nil
	}
	return lease
}

// handleFileDownload answers GET .../files/{path}: the file's bytes as
// application/octet-stream. ?stat=1 returns the metadata document
// instead. 404 when the path does not exist, 413 beyond 256 MiB.
func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	lease := s.filesGate(w, r)
	if lease == nil {
		return
	}
	guestPath, ok := filePathValue(r)
	if !ok {
		writeError(w, http.StatusNotFound, "file path required")
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
		writeError(w, http.StatusNotFound, "file path required")
		return
	}
	mode, err := parseFileMode(r.URL.Query().Get("mode"), 0o644)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
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
// bytes with 413 before the caller touches the substrate.
func readBodyCapped(w http.ResponseWriter, r *http.Request, max int64) ([]byte, bool) {
	data, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "reading request body: "+err.Error())
		return nil, false
	}
	if int64(len(data)) > max {
		writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds the 256 MiB limit")
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
		writeError(w, http.StatusNotFound, "file path required")
		return
	}
	if r.URL.Query().Get("op") != "mkdir" {
		writeError(w, http.StatusBadRequest, "op must be mkdir")
		return
	}
	mode, err := parseFileMode(r.URL.Query().Get("mode"), 0o755)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
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
		writeError(w, http.StatusNotFound, "file path required")
		return
	}
	recursive := r.URL.Query().Get("recursive") == "1"
	if err := s.svc.sub.Remove(r.Context(), lease.SandboxID, guestPath, recursive); err != nil {
		s.mapFileError(w, lease, "remove", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
